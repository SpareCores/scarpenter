/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/quota"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// skip reasons of a candidate, as metric label values.
const (
	skipNoQuota     = "no-quota"
	skipUnavailable = "unavailable"
	skipImage       = "image"
	skipResources   = "resources"
)

// launchState is what one Create keeps between its attempts.
type launchState struct {
	nc        *v1alpha1.SCNodeClass
	nodeClaim *karpv1.NodeClaim
	token     string
	// settings resolves the vendor settings of the offerings; runKey is the
	// placement run key of the NodeClaim (overview section 4.1).
	settings *settingsResolver
	runKey   string
	// blocked holds the (vendor, region) whose vendor resources are missing, and
	// the zone of one: the other candidates of them are skipped.
	blocked map[string]bool
	// attempts counts vendor calls; skipped counts candidates passed over
	// because of quota; imageErr is the last image failure.
	attempts int
	skipped  int
	imageErr error
	lastErr  error
	// keepToken is set when the failure may hide a launched instance.
	keepToken bool
	// prepared are the resources the last attempt launched with.
	prepared *vendors.Prepared
}

// Create launches the NodeClaim (overview section 4.1): the candidate offerings
// in price order, the first with quota headroom is reserved and launched, errors
// of class InsufficientCapacity and QuotaExceeded fall through to the next one,
// up to --max-launch-attempts vendor calls. When nothing is left it returns
// InsufficientCapacityError, and core deletes the NodeClaim.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	started := c.opts.Clock.Now()
	st := &launchState{nodeClaim: nodeClaim, blocked: map[string]bool{}}
	created, err := c.create(ctx, st)
	result := resultSuccess
	if err != nil {
		result = resultFailed
	}
	c.metrics.attempts.WithLabelValues(result).Observe(float64(st.attempts))
	c.metrics.duration.WithLabelValues(result).Observe(c.opts.Clock.Since(started).Seconds())
	if err != nil && st.token != "" && !st.keepToken {
		// The token of a NodeClaim that did not launch is useless. It is kept
		// when the error is Retryable: the instance may exist (a replayed
		// idempotency key returns it) and booted with this token, so the retry
		// must find it.
		if derr := c.opts.Tokens.Delete(ctx, nodeClaim.Name); derr != nil {
			log.FromContext(ctx).Error(derr, "deleting the bootstrap token of a failed launch")
		}
	}
	return created, err
}

func (c *CloudProvider) create(ctx context.Context, st *launchState) (*karpv1.NodeClaim, error) {
	logger := log.FromContext(ctx)
	nodeClaim := st.nodeClaim
	nc, err := c.resolveNodeClass(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// No NodeClass, no capacity possibilities (the AWS provider does the
			// same).
			return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("resolving the NodeClass: %w", err))
		}
		return nil, fmt.Errorf("resolving the NodeClass: %w", err)
	}
	st.nc = nc
	if err := checkNodeClassReady(nc); err != nil {
		return nil, err
	}
	// The key manager that mints a node's key is not wired in yet.
	if nc.Spec.Network.Tailscale.PerNode() {
		return nil, cloudprovider.NewNodeClassNotReadyError(
			errors.New("keySource PerNode is not implemented in the launch path yet"))
	}

	st.settings = newSettingsResolver(nc, c.opts.Registry)
	claimReqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	if st.runKey, err = options.RunKey(claimReqs); err != nil {
		c.publish(nc, EventLaunchFailed, err.Error(), "run-key")
		return nil, cloudprovider.NewCreateError(err, "InvalidRunKey", truncate(err.Error()))
	}
	cands, err := c.candidates(ctx, nc, nodeClaim, st.settings)
	if err != nil {
		return nil, cloudprovider.NewCreateError(err, "InstanceTypeResolutionFailed", "error resolving instance types")
	}
	if len(cands) == 0 {
		// A catalog that is not ready (a controller that has just started, a
		// snapshot that is too old) knows no types: that is not a lack of
		// capacity, and InsufficientCapacity would make core delete the NodeClaim,
		// also one whose instance a replayed idempotency key would find. The
		// launch is retried instead.
		if cs := c.opts.Catalog.Status(nc.Name); !cs.Ready {
			err := fmt.Errorf("the catalog is not ready (%s): %s", cs.Reason, cs.Message)
			return nil, cloudprovider.NewCreateError(err, "CatalogNotReady", truncate(err.Error()))
		}
		if vendor, reason := c.notReadyVendor(nc, nodeClaim); vendor != "" {
			return nil, cloudprovider.NewNodeClassNotReadyError(fmt.Errorf("vendor %s is not ready: %s", vendor, reason))
		}
		return nil, cloudprovider.NewInsufficientCapacityError(errNoCandidates)
	}

	// The token is minted before the first vendor call (overview section 4.1
	// step 3); its kubeconfig goes into every rendering.
	if st.token, err = c.opts.Tokens.Mint(ctx, nodeClaim); err != nil {
		return nil, fmt.Errorf("minting the bootstrap token: %w", err)
	}

	for i := range cands {
		cand := &cands[i]
		if st.attempts >= c.opts.MaxLaunchAttempts {
			break
		}
		if reason := c.skipReason(st, cand); reason != "" {
			c.metrics.skipped.WithLabelValues(cand.vendor, cand.region, cand.capacityType, reason).Inc()
			continue
		}
		res, ok := c.reserveQuota(ctx, st, cand)
		if !ok {
			continue
		}
		inst, err := c.launch(ctx, st, cand)
		if err == nil {
			c.reconcileQuota(res, cand, inst, nodeClaim.Name)
			return c.created(ctx, nc, nodeClaim, cands, cand, inst, st.runKey, st.prepared), nil
		}
		if _, isImage := errors.AsType[*imageError](err); isImage {
			c.release(res)
			st.imageErr = err
			c.metrics.skipped.WithLabelValues(cand.vendor, cand.region, cand.capacityType, skipImage).Inc()
			continue
		}
		c.release(res)
		st.attempts++
		st.lastErr = err
		class := vendors.ClassOf(err)
		c.metrics.launches.WithLabelValues(cand.vendor, cand.region, cand.capacityType, string(class)).Inc()
		logger.Info("launch attempt failed", "type", cand.typeName, "zone", cand.zone, "region", cand.region,
			"capacity-type", cand.capacityType, "class", class, "error", err.Error())
		switch class {
		case vendors.InsufficientCapacity:
			c.opts.Unavailable.MarkInsufficientCapacity(cand.vendor, cand.typeName, cand.zone, cand.capacityType)
		case vendors.Unsupported:
			c.onUnsupported(logger, st, cand, err)
		case vendors.InvalidOffering:
			c.opts.Unavailable.MarkInvalid(cand.vendor, cand.typeName, cand.zone, cand.capacityType)
			c.publish(nc, EventOfferingRejected, launchMessage(err),
				cand.vendor, cand.typeName, cand.zone, cand.capacityType)
		case vendors.ZoneUnusable:
			c.opts.Unavailable.MarkZoneUnusable(cand.vendor, cand.zone)
		case vendors.QuotaExceeded:
			c.reportQuotaRefusal(st.nc, cand)
			c.onQuotaExceeded(cand, vendors.IsAccountWide(err))
		case vendors.DailyLimit:
			c.onDailyLimit(cand, err)
		case vendors.Retryable:
			st.keepToken = true
			return nil, cloudprovider.NewCreateError(fmt.Errorf("launching %s: %w", cand.typeName, err),
				"LaunchFailed", truncate(launchMessage(err)))
		default: // Fatal, NotFound
			c.publish(nc, EventLaunchFailed, launchMessage(err), cand.vendor, vendors.CodeOf(err))
			return nil, cloudprovider.NewCreateError(fmt.Errorf("launching %s: %w", cand.typeName, err),
				"LaunchFailed", truncate(launchMessage(err)))
		}
	}

	switch {
	case st.attempts == 0 && st.skipped == 0 && st.imageErr != nil:
		c.publish(nc, EventImageFailed, st.imageErr.Error(), "image")
		return nil, cloudprovider.NewCreateError(st.imageErr, "ImageResolutionFailed", truncate(st.imageErr.Error()))
	case st.lastErr != nil:
		return nil, cloudprovider.NewInsufficientCapacityError(launchFailure{st.lastErr})
	}
	return nil, cloudprovider.NewInsufficientCapacityError(
		fmt.Errorf("no candidate offering had quota headroom (%d skipped)", st.skipped))
}

// reserveQuota reserves the quota of a launch of the candidate. It returns
// false, after counting the candidate as skipped, when the quota has no headroom.
func (c *CloudProvider) reserveQuota(ctx context.Context, st *launchState, cand *candidate) (*quota.Reservation, bool) {
	if c.opts.Quota == nil {
		return nil, true
	}
	res, d := c.opts.Quota.Reserve(cand.vendor, cand.region, cand.apiReference, cand.capacityType, st.nodeClaim.Name)
	if !d.Allowed {
		c.reportQuotaRefusal(st.nc, cand)
		st.skipped++
		c.metrics.skipped.WithLabelValues(cand.vendor, cand.region, cand.capacityType, skipNoQuota).Inc()
		log.FromContext(ctx).V(1).Info("skipping an offering without quota headroom", "type", cand.typeName,
			"region", cand.region, "capacity-type", cand.capacityType, "bucket", d.Bucket,
			"headroom", d.Headroom, "needed", d.Need)
		return nil, false
	}
	return res, true
}

// onUnsupported records an Unsupported error. A vendor resource of the region
// that is missing has the engine make the region's offerings unavailable, and
// nothing of this type or zone is wrong: the other candidates of the region are
// skipped, or only those of the zone when only the zone lacks it. A spot request
// to a vendor without spot marks the spot offering only. Otherwise the type in
// the zone is marked.
func (c *CloudProvider) onUnsupported(logger logr.Logger, st *launchState, cand *candidate, err error) {
	switch vendors.CodeOf(err) {
	case vendors.CodeResourceMissing, vendors.CodeResourceForbidden:
		st.blocked[resourceKey(cand)] = true
	case vendors.CodeResourceMissingInZone:
		st.blocked[resourceKey(cand)+"/"+cand.zone] = true
	case vendors.CodeNoSpot:
		// The vendor has no spot: only the spot offering is wrong, on-demand of
		// the same type and zone is fine.
		c.opts.Unavailable.MarkInvalid(cand.vendor, cand.typeName, cand.zone, cand.capacityType)
	default:
		c.markUnsupported(logger, cand, err)
	}
}

// reportQuotaRefusal tells the OnQuotaRefusal hook that the candidate's launch
// was refused for quota.
func (c *CloudProvider) reportQuotaRefusal(nc *v1alpha1.SCNodeClass, cand *candidate) {
	if c.opts.OnQuotaRefusal == nil {
		return
	}
	cpu := cand.it.Capacity[corev1.ResourceCPU]
	c.opts.OnQuotaRefusal(QuotaRefusal{
		NodeClass: nc.Name, Vendor: cand.vendor, Region: cand.region, CapacityType: cand.capacityType, CPU: cpu.Value(),
	})
}

// skipReason says why a candidate is passed over without a vendor call: it
// became unavailable since the types were computed (another Create's failure),
// or a quota error in its region and capacity type came earlier in this Create.
func (c *CloudProvider) skipReason(st *launchState, cand *candidate) string {
	if st.blocked[resourceKey(cand)] || st.blocked[resourceKey(cand)+"/"+cand.zone] {
		return skipResources
	}
	if c.opts.Unavailable.IsUnavailable(unavailable.Offering{
		Vendor: cand.vendor, APIReference: cand.apiReference, InstanceType: cand.typeName, Region: cand.region,
		Zone:         cand.zone,
		CapacityType: cand.capacityType,
	}) {
		return skipUnavailable
	}
	return ""
}

// resourceKey identifies the vendor resources of a region, which all offerings
// of the region share.
func resourceKey(c *candidate) string { return "resources/" + c.vendor + "/" + c.region }

// onQuotaExceeded records a quota error (overview sections 4.1 and 5.6): the
// quota tracker refreshes its view of the region, and the bucket the vendor
// counts the launch against is marked unavailable for the TTL, whatever the
// tracker showed before (a view with headroom is stale). The bucket is the one
// the tracker finds limiting, else every bucket the type consumes; a vendor
// without a bucket mapping has the region and capacity type marked. A quota
// error of a vendor whose limits are account-wide (vendors.Error.AccountWide)
// has the capacity type marked in every region of the vendor instead. A deleted
// NodeClaim of the vendor and region clears the marks (CloudProvider.Delete).
func (c *CloudProvider) onQuotaExceeded(cand *candidate, accountWide bool) {
	if c.opts.Quota != nil {
		c.opts.Quota.NotifyQuotaError(cand.vendor, cand.region)
	}
	if accountWide {
		c.opts.Unavailable.MarkQuotaExceededAccountWide(cand.vendor, cand.capacityType)
		return
	}
	buckets := c.quotaBuckets(cand)
	if len(buckets) == 0 {
		c.opts.Unavailable.MarkQuotaExceeded(cand.vendor, cand.region, cand.capacityType)
		return
	}
	for _, b := range buckets {
		c.opts.Unavailable.MarkQuotaBucket(cand.vendor, cand.region, b)
	}
}

// quotaBuckets returns the quota buckets a quota error of the candidate may
// have counted against; none when the vendor has no bucket mapping.
func (c *CloudProvider) quotaBuckets(cand *candidate) []string {
	if c.opts.Quota == nil {
		return nil
	}
	if b, known := c.opts.Quota.LimitingBucket(cand.vendor, cand.region, cand.apiReference, cand.capacityType); known {
		return []string{b}
	}
	return c.opts.Quota.BucketsOf(cand.vendor, cand.apiReference, cand.capacityType)
}

// onDailyLimit marks the vendor's region unavailable until the limit resets:
// the vendor's reset time when the driver knows it, else the next 00:00 UTC.
// Deleting NodeClaims does not clear it.
func (c *CloudProvider) onDailyLimit(cand *candidate, err error) {
	resetAt := time.Time{}
	if e, ok := errors.AsType[*vendors.Error](err); ok {
		resetAt = e.ResetAt
	}
	if resetAt.IsZero() {
		resetAt = nextMidnightUTC(c.opts.Clock.Now())
	}
	c.opts.Unavailable.MarkDailyLimit(cand.vendor, cand.region, resetAt)
}

// nextMidnightUTC is when a daily limit that the vendor gives no reset time for
// is assumed to reset. Within the quota TTL after midnight the limit may be
// the previous day's count that has not been reset yet, so the mark then lasts
// the TTL, and the offerings are tried again soon.
func nextMidnightUTC(now time.Time) time.Time {
	now = now.UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if now.Sub(midnight) < unavailable.DefaultQuotaExceededTTL {
		return now.Add(unavailable.DefaultQuotaExceededTTL)
	}
	return midnight.AddDate(0, 0, 1)
}

// launchFailure is a launch error whose text is launchMessage.
type launchFailure struct{ err error }

func (f launchFailure) Error() string { return launchMessage(f.err) }
func (f launchFailure) Unwrap() error { return f.err }

// launchMessage is the text of a launch failure on the NodeClaim's launch
// condition and in events: "<Class> (<Code>): <vendor message>". k8s-inspector
// reads the class from it.
//
// Only the documented classes appear: an error without a class, and a class that
// is not a launch outcome (NotFound, RegionUnavailable), is Fatal.
func launchMessage(err error) string {
	e, ok := errors.AsType[*vendors.Error](err)
	if !ok {
		e = vendors.NewError(vendors.Fatal, "", err)
	}
	switch e.Class {
	case vendors.InsufficientCapacity, vendors.ZoneUnusable, vendors.QuotaExceeded, vendors.DailyLimit,
		vendors.Retryable, vendors.Unsupported, vendors.InvalidOffering, vendors.Fatal:
		return e.Error()
	}
	return vendors.NewError(vendors.Fatal, e.Code, e.Err).Error()
}

// launch prepares the resources of the candidate and makes the vendor call.
func (c *CloudProvider) launch(ctx context.Context, st *launchState, cand *candidate) (*vendors.Instance, error) {
	nc, nodeClaim := st.nc, st.nodeClaim
	drv, ok := c.opts.Registry.Driver(cand.vendor)
	if !ok {
		return nil, vendors.Errorf(vendors.Fatal, "vendor %q is not enabled", cand.vendor)
	}
	arch := cand.it.Requirements.Get("kubernetes.io/arch").Any()
	img, err := c.image(ctx, drv, nc, cand.vendor, cand.region, arch)
	if err != nil {
		return nil, &imageError{err: err}
	}

	// Resources the instance needs first: vendor resources with create,
	// placement groups, separate volumes (overview section 4.1 step 3).
	var prepared *vendors.Prepared
	if no, ok := drv.(vendors.NodeOptions); ok {
		prepared, err = no.Prepare(ctx, vendors.PrepareRequest{
			Region: cand.region, Zone: cand.zone, InstanceType: cand.apiReference, CapacityType: cand.capacityType,
			Type:     catalogTypeOf(cand),
			Settings: cand.settings, Tags: c.launchTags(nc, nodeClaim), RunKey: st.runKey,
			Resources: c.resourceProvider(nc, cand),
		})
		if err != nil {
			return nil, fmt.Errorf("preparing the launch: %w", err)
		}
	}
	st.prepared = prepared
	providerID := scope.ProviderID(cand.vendor, cand.region, nodeClaim.Name)
	rr := RenderRequest{
		NodeClass: nc, NodeClaim: nodeClaim, Vendor: cand.vendor, Region: cand.region, Zone: cand.zone,
		InstanceType: cand.apiReference, Arch: arch, Token: st.token, NodeName: nodeClaim.Name, ProviderID: providerID,
		KubeReserved: kubeReserved(cand.it), NodeMemoryMiB: cand.it.Capacity.Memory().Value() >> 20,
	}
	if prepared != nil {
		rr.Volumes, rr.Local = volumeHints(prepared)
		rr.NodeIPCIDR = prepared.NodeIPCIDR
	}
	if rr.NVIDIA, err = nvidiaSelection(nc, cand.it); err != nil {
		return nil, vendors.NewError(vendors.Fatal, "UserDataRender", err)
	}
	spot := cand.capacityType == vendors.CapacitySpot
	rr.Spot = spot
	frags, err := vendors.BootFragmentsOf(drv, nc.Spec.RestrictsInstanceMetadata(), spot)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "UserDataRender", err)
	}
	for _, f := range frags {
		rr.VendorFragments = append(rr.VendorFragments, bootstrap.Fragment{Name: f.Name, Butane: f.Butane})
	}
	userData, err := c.opts.UserData.Render(ctx, rr)
	if tooLarge, ok := errors.AsType[*bootstrap.UserDataTooLargeError](err); ok {
		// This offering's combination (a spot GPU type with the agent, say) does not
		// fit the vendor's limit: reject the offering, not the NodeClass or the other
		// offerings of the type.
		return nil, vendors.NewError(vendors.InvalidOffering, "UserDataTooLarge", tooLarge)
	}
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "UserDataRender", fmt.Errorf("rendering the user-data: %w", err))
	}
	req := vendors.CreateRequest{
		Region: cand.region, Zone: cand.zone, InstanceType: cand.apiReference, CapacityType: cand.capacityType,
		Arch: arch, Image: *img, UserData: userData, Settings: cand.settings, Prepared: prepared,
		Tags:           c.launchTags(nc, nodeClaim),
		ExtraTags:      nc.Spec.Tags,
		IdempotencyKey: string(nodeClaim.UID),
		Node: vendors.NodeInfo{
			Name: nodeClaim.Name, ProviderID: providerID,
			Labels:   nodeLabels(nodeClaim.Name, cand.it, cand.off, cand.sel.Applied(), st.runKey),
			Capacity: cand.it.Capacity, Allocatable: cand.it.Allocatable(),
		},
	}
	inst, err := drv.CreateInstance(ctx, req)
	if err != nil {
		return nil, err
	}
	c.metrics.launches.WithLabelValues(cand.vendor, cand.region, cand.capacityType, resultSuccess).Inc()
	return inst, nil
}

// nvidiaSelection is the NVIDIA driver of the candidate's type, read from the
// driver labels and the nvidia.com/gpu capacity the catalog stamped on it (it
// does so only for supported types, and only once nodes boot with the driver:
// nvidia.NodeBootstrap); nil for every other type.
func nvidiaSelection(nc *v1alpha1.SCNodeClass, it *cloudprovider.InstanceType) (*nvidia.Selection, error) {
	label := func(key string) string {
		r := it.Requirements.Get(key)
		if r.Operator() == corev1.NodeSelectorOpIn && len(r.Values()) == 1 {
			return r.Values()[0]
		}
		return ""
	}
	branch := label(v1alpha1.LabelNVIDIADriverBranch)
	if branch == "" {
		return nil, nil
	}
	count := it.Capacity[catalog.ResourceNVIDIAGPU]
	sel, err := nvidia.FromLabels(nc.Spec.Flatcar.Version, branch, label(v1alpha1.LabelNVIDIADriverVersion),
		nvidia.Flavour(label(v1alpha1.LabelNVIDIADriverFlavor)), label(v1alpha1.LabelAcceleratorModel), count.Value())
	if err != nil {
		return nil, err
	}
	return &sel, nil
}

// catalogTypeOf describes the candidate's instance type for the driver from the
// labels the catalog stamped on it.
func catalogTypeOf(cand *candidate) vendors.CatalogType {
	one := func(key string) string {
		r := cand.it.Requirements.Get(key)
		if r.Operator() == corev1.NodeSelectorOpIn && len(r.Values()) == 1 {
			return r.Values()[0]
		}
		return ""
	}
	num := func(key string) float64 {
		f, _ := strconv.ParseFloat(one(key), 64)
		return f
	}
	return vendors.CatalogType{
		Vendor: cand.vendor, APIReference: cand.apiReference, Family: one(v1alpha1.LabelFamily),
		Arch: one(corev1.LabelArchStable), VCPUs: int(num(v1alpha1.LabelVCPUs)), MemoryMiB: num(v1alpha1.LabelMemoryMiB),
		LocalStorageGB: num(v1alpha1.LabelLocalStorageGB), LocalStorageCount: int(num(v1alpha1.LabelLocalStorageCount)),
	}
}

// resourceProvider returns the vendor resources of the launch, as the
// NodeClass configures them; without the engine a driver that needs a resource
// fails clearly.
func (c *CloudProvider) resourceProvider(nc *v1alpha1.SCNodeClass, cand *candidate) vendors.ResourceProvider {
	if c.opts.Resources == nil {
		return noResources{}
	}
	return c.opts.Resources.Provider(nc, cand.vendor, cand.region, cand.zone)
}

type noResources struct{}

func (noResources) Require(_ context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	return nil, vendors.Errorf(vendors.Fatal, "no resource engine is configured, cannot provide %s", req.Kind)
}

// kubeReserved is the kubelet's kubeReserved of the instance type: the
// reservation the scheduler assumed for it, so that the node's allocatable is
// what the scheduler counted on.
func kubeReserved(it *cloudprovider.InstanceType) map[string]string {
	if it.Overhead == nil {
		return nil
	}
	out := map[string]string{}
	for name, q := range it.Overhead.KubeReserved {
		out[string(name)] = q.String()
	}
	return out
}

// volumeHints converts the driver's device hints for the user-data.
func volumeHints(p *vendors.Prepared) ([]bootstrap.VolumeHint, *bootstrap.LocalHint) {
	vols := make([]bootstrap.VolumeHint, 0, len(p.Devices))
	for _, d := range p.Devices {
		vols = append(vols, bootstrap.VolumeHint{
			Name: d.Name, Filesystem: d.Filesystem, MountPath: d.MountPath, ByID: d.ByID,
			SerialPattern: d.SerialPattern, SizeBytes: d.SizeBytes,
		})
	}
	var local *bootstrap.LocalHint
	if p.Local != nil {
		local = &bootstrap.LocalHint{
			Policy: p.Local.Policy, MountPath: p.Local.MountPath, DevicePattern: p.Local.DevicePattern, Count: p.Local.Count,
		}
	}
	return vols, local
}

// reconcileQuota commits the reservation of the attempted offering to the
// instance the vendor returned. A replayed idempotency key returns the first
// instance, which may be another offering than the one attempted now: the
// reservation then follows the instance (the attempted one is released and the
// instance's own consumption reserved against it).
func (c *CloudProvider) reconcileQuota(res *quota.Reservation, cand *candidate, inst *vendors.Instance, owner string) {
	if c.opts.Quota == nil {
		return
	}
	if inst.Type == cand.apiReference && inst.Region == cand.region && inst.CapacityType == cand.capacityType {
		c.opts.Quota.Commit(res, inst.Ref)
		return
	}
	c.opts.Quota.Reconcile(res, cand.vendor, inst.Region, inst.Type, inst.CapacityType, owner, inst.Ref)
}

func (c *CloudProvider) launchTags(nc *v1alpha1.SCNodeClass, nodeClaim *karpv1.NodeClaim) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{
		vendors.TagCluster:   c.opts.ClusterName,
		vendors.TagNodeClaim: nodeClaim.Name,
		vendors.TagNodePool:  nodeClaim.Labels[karpv1.NodePoolLabelKey],
		vendors.TagNodeClass: nc.Name,
		vendors.TagManagedBy: vendors.ManagedByValue,
	}
}

// created builds the NodeClaim Create returns: provider ID, capacity and
// allocatable of the chosen type, the labels and the annotations (overview
// section 4.1 step 6).
func (c *CloudProvider) created(
	ctx context.Context, nc *v1alpha1.SCNodeClass, nodeClaim *karpv1.NodeClaim, cands []candidate, attempted *candidate,
	inst *vendors.Instance, runKey string, prepared *vendors.Prepared,
) *karpv1.NodeClaim {
	// The instance is the truth: a replayed idempotency key returns the first
	// instance, which may be another offering than the one attempted now.
	cand := attempted
	if inst.Type != attempted.apiReference || inst.Zone != attempted.zone || inst.CapacityType != attempted.capacityType ||
		inst.Region != attempted.region {
		cand = nil
		for i := range cands {
			k := &cands[i]
			if k.vendor == attempted.vendor && k.apiReference == inst.Type && k.zone == inst.Zone &&
				k.capacityType == inst.CapacityType && k.region == inst.Region {
				cand = k
				break
			}
		}
		if cand == nil {
			log.FromContext(ctx).Info("the instance of a replayed idempotency key matches no candidate; "+
				"describing the NodeClaim from the catalog", "instance", inst.Ref, "type", inst.Type, "zone", inst.Zone)
			return c.fromInstance(nc, attempted.vendor, inst)
		}
	}
	c.putInstance(cand.vendor, inst)
	if c.opts.Quota != nil {
		c.opts.Quota.NotifyChanged(cand.vendor, cand.region)
	}
	out := &karpv1.NodeClaim{}
	out.Labels = nodeLabels(nodeClaim.Name, cand.it, cand.off, cand.sel.Applied(), runKey)
	out.Annotations = map[string]string{
		v1alpha1.AnnotationInstanceRef: string(inst.Ref),
		v1alpha1.AnnotationPrice:       formatPrice(cand.usd),
	}
	setBillingStart(out.Annotations, inst)
	if !cand.complete {
		out.Annotations[v1alpha1.AnnotationPriceIncomplete] = "true"
	}
	if hash := nc.Status.Vendors[cand.vendor].Hash; hash != "" {
		out.Annotations[v1alpha1.AnnotationNodeClassHash] = hash
		out.Annotations[v1alpha1.AnnotationNodeClassHashVersion] = nodeclass.HashVersion
	}
	// The resources of the launch, for the instance the attempt created (a
	// replayed key may return an older instance whose resources are not known).
	if prepared != nil && len(prepared.Resources) > 0 && inst.Type == attempted.apiReference &&
		inst.Zone == attempted.zone && inst.Region == attempted.region {
		byKind := map[string][]string{}
		for _, r := range prepared.Resources {
			byKind[string(r.Kind)] = append(byKind[string(r.Kind)], r.ID)
		}
		raw, _ := json.Marshal(byKind)
		out.Annotations[v1alpha1.AnnotationResources] = string(raw)
	}
	if in, ok, err := options.InputFromNodeClass(nc, cand.vendor); err == nil && ok {
		if hashes := in.AppliedHashes(cand.sel); len(hashes) > 0 {
			raw, _ := json.Marshal(hashes)
			out.Annotations[v1alpha1.AnnotationProfileHashes] = string(raw)
		}
	}
	out.Status.ProviderID = scope.ProviderID(cand.vendor, inst.Region, nodeClaim.Name)
	out.Status.Capacity = cand.it.Capacity
	out.Status.Allocatable = cand.it.Allocatable()
	return out
}

// fromInstance describes a NodeClaim from an instance alone (the catalog type
// and offering of the instance), for an instance no candidate explains. The
// price is the catalog's for the NodeClass defaults: what profiles the
// instance was launched with is not known here.
func (c *CloudProvider) fromInstance(
	nc *v1alpha1.SCNodeClass, vendor string, inst *vendors.Instance,
) *karpv1.NodeClaim {
	c.putInstance(vendor, inst)
	out := c.instanceToNodeClaim(vendor, inst)
	out.Annotations[v1alpha1.AnnotationInstanceRef] = string(inst.Ref)
	if p, ok := c.opts.Catalog.Price(nc.Name, catalog.InstanceTypeName(vendor, inst.Type), inst.Region, inst.Zone,
		inst.CapacityType); ok {
		out.Annotations[v1alpha1.AnnotationPrice] = formatPrice(p.USD)
		if !p.Complete {
			out.Annotations[v1alpha1.AnnotationPriceIncomplete] = "true"
		}
	}
	if hash := nc.Status.Vendors[vendor].Hash; hash != "" {
		out.Annotations[v1alpha1.AnnotationNodeClassHash] = hash
		out.Annotations[v1alpha1.AnnotationNodeClassHashVersion] = nodeclass.HashVersion
	}
	return out
}

// imageError marks a failure to resolve the boot image: the candidate is
// skipped without counting an attempt.
type imageError struct{ err error }

func (e *imageError) Error() string { return e.err.Error() }
func (e *imageError) Unwrap() error { return e.err }

type imageKey struct{ vendor, region, arch, channel, version, owner, use string }

type cachedImage struct {
	image vendors.Image
	until time.Time
}

// image resolves the official image of the NodeClass's Flatcar release for the
// region and architecture, cached for an hour (overview section 9).
func (c *CloudProvider) image(
	ctx context.Context, drv vendors.Driver, nc *v1alpha1.SCNodeClass, vendor, region, arch string,
) (*vendors.Image, error) {
	use, _, err := selector.ImageUse(nc, drv, region, arch)
	if err != nil {
		return nil, fmt.Errorf("resolving the image of %s in %s (%s): %w", vendor, region, arch, err)
	}
	enc, _ := json.Marshal(use)
	channel := string(nc.Spec.Flatcar.Channel)
	if channel == "" {
		channel = selector.DefaultChannel // the CRD defaults it; an object built in code may not
	}
	key := imageKey{
		vendor, region, arch, channel, nc.Spec.Flatcar.Version, nc.Spec.Vendors.ImageOwner(vendor), string(enc),
	}
	now := c.opts.Clock.Now()
	c.imageMu.Lock()
	if ci, ok := c.images[key]; ok && now.Before(ci.until) {
		c.imageMu.Unlock()
		img := ci.image
		return &img, nil
	}
	c.imageMu.Unlock()
	img, err := drv.ResolveImage(ctx, vendors.ImageRequest{
		Region: region, Arch: arch, Channel: key.channel, Version: key.version, Owner: key.owner, Use: use,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving the image of %s in %s (%s): %w", vendor, region, arch, err)
	}
	c.imageMu.Lock()
	c.images[key] = cachedImage{image: *img, until: now.Add(c.opts.ImageCacheTTL)}
	c.imageMu.Unlock()
	return img, nil
}

// maxConditionMessage is the longest message a CreateError carries into the
// NodeClaim's Launched condition.
const maxConditionMessage = 200

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxConditionMessage {
		return s
	}
	return s[:maxConditionMessage]
}

// release is nil-safe: without a tracker there is no reservation.
func (c *CloudProvider) release(res *quota.Reservation) {
	if c.opts.Quota != nil {
		c.opts.Quota.Release(res)
	}
}

// notReadyVendor returns a vendor of the NodeClass that the NodeClaim allows and
// that is not ready, with the reason, when no allowed vendor is ready ("" when
// one is): with no candidates that is NodeClassNotReady (overview section 4.1
// step 1). A vendor that is not ready must not turn the failure of another,
// ready vendor of a multi-vendor NodeClass into NodeClassNotReady: that vendor
// simply has no capacity, which is InsufficientCapacity.
func (c *CloudProvider) notReadyVendor(nc *v1alpha1.SCNodeClass, nodeClaim *karpv1.NodeClaim) (string, string) {
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	var notReady, reason string
	for _, vendor := range nc.Spec.Vendors.Names() {
		if reqs.Has(v1alpha1.LabelVendor) && !reqs.Get(v1alpha1.LabelVendor).Has(vendor) {
			continue
		}
		ok, why := vendorReady(nc, vendor)
		if ok {
			return "", ""
		}
		if notReady == "" {
			notReady, reason = vendor, why
		}
	}
	return notReady, reason
}

// markUnsupported marks the offering's type and zone unsupported: for minutes
// when the driver's own pre-launch check found it, for a day when the vendor said
// so. Create skips an offering with a live mark, so the line is once per mark.
func (c *CloudProvider) markUnsupported(logger logr.Logger, cand *candidate, err error) {
	preLaunch := vendors.IsPreLaunch(err)
	if preLaunch {
		c.opts.Unavailable.MarkUnsupportedPreLaunch(cand.vendor, cand.typeName, cand.zone)
	} else {
		c.opts.Unavailable.MarkUnsupported(cand.vendor, cand.typeName, cand.zone)
	}
	logger.Info("marked the offering unsupported", "type", cand.typeName, "zone", cand.zone,
		"code", vendors.CodeOf(err), "preLaunch", preLaunch)
}
