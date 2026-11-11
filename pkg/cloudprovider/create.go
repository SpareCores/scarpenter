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
	skipRegionFull  = "quota-exceeded"
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
	// blocked holds the (vendor, region, capacity type) that returned a quota
	// error during this Create, and the (vendor, region) whose vendor resources
	// are missing: the others of them are skipped.
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
		var res *quota.Reservation
		if c.opts.Quota != nil {
			var d quota.Decision
			res, d = c.opts.Quota.Reserve(cand.vendor, cand.region, cand.apiReference, cand.capacityType, nodeClaim.Name)
			if !d.Allowed {
				st.skipped++
				c.metrics.skipped.WithLabelValues(cand.vendor, cand.region, cand.capacityType, skipNoQuota).Inc()
				logger.V(1).Info("skipping an offering without quota headroom", "type", cand.typeName,
					"region", cand.region, "capacity-type", cand.capacityType, "bucket", d.Bucket,
					"headroom", d.Headroom, "needed", d.Need)
				continue
			}
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
			if code := vendors.CodeOf(err); code == vendors.CodeResourceMissing || code == vendors.CodeResourceForbidden {
				// A vendor resource of the region is missing: the engine made the
				// region's offerings unavailable, and nothing of this type or zone is
				// wrong. The other candidates of the region are not tried.
				st.blocked[resourceKey(cand)] = true
				continue
			} else if code == vendors.CodeResourceMissingInZone {
				// Only the zone lacks it: the other zones of the region are tried.
				st.blocked[resourceKey(cand)+"/"+cand.zone] = true
				continue
			}
			c.opts.Unavailable.MarkUnsupported(cand.vendor, cand.typeName, cand.zone)
		case vendors.ZoneUnusable:
			c.opts.Unavailable.MarkZoneUnusable(cand.vendor, cand.zone)
		case vendors.QuotaExceeded:
			c.onQuotaExceeded(st, cand, vendors.IsAccountWide(err))
		case vendors.Retryable:
			st.keepToken = true
			return nil, fmt.Errorf("launching %s: %w", cand.typeName, err)
		default: // Fatal, NotFound
			c.publish(nc, EventLaunchFailed, fmt.Sprintf("launching %s in %s failed: %v", cand.typeName, cand.zone, err),
				cand.vendor, vendors.CodeOf(err))
			return nil, cloudprovider.NewCreateError(fmt.Errorf("launching %s: %w", cand.typeName, err),
				"LaunchFailed", truncate(err.Error()))
		}
	}

	switch {
	case st.attempts == 0 && st.skipped == 0 && st.imageErr != nil:
		c.publish(nc, EventImageFailed, st.imageErr.Error(), "image")
		return nil, cloudprovider.NewCreateError(st.imageErr, "ImageResolutionFailed", truncate(st.imageErr.Error()))
	case st.lastErr != nil:
		return nil, cloudprovider.NewInsufficientCapacityError(
			fmt.Errorf("every launch attempt failed (%d attempts), the last: %w", st.attempts, st.lastErr))
	}
	return nil, cloudprovider.NewInsufficientCapacityError(
		fmt.Errorf("no candidate offering had quota headroom (%d skipped)", st.skipped))
}

// skipReason says why a candidate is passed over without a vendor call: it
// became unavailable since the types were computed (another Create's failure),
// or a quota error in its region and capacity type came earlier in this Create.
func (c *CloudProvider) skipReason(st *launchState, cand *candidate) string {
	if st.blocked[regionKey(cand)] {
		return skipRegionFull
	}
	if st.blocked[resourceKey(cand)] || st.blocked[resourceKey(cand)+"/"+cand.zone] {
		return skipResources
	}
	if c.opts.Unavailable.IsUnavailable(unavailable.Offering{
		Vendor: cand.vendor, InstanceType: cand.typeName, Region: cand.region, Zone: cand.zone,
		CapacityType: cand.capacityType,
	}) {
		return skipUnavailable
	}
	return ""
}

func regionKey(c *candidate) string { return c.vendor + "/" + c.region + "/" + c.capacityType }

// resourceKey identifies the vendor resources of a region, which all offerings
// of the region share.
func resourceKey(c *candidate) string { return "resources/" + c.vendor + "/" + c.region }

// onQuotaExceeded records a quota error (overview sections 4.1 and 5.6): a
// vendor with a fresh quota view for the type refreshes its view of the region
// and the tracker decides from then on; a vendor without one (no quota API, or a
// stale or missing view) has the region and capacity type skipped for the TTL. Either way the other candidates of the
// region and capacity type are not tried by this Create. A quota error of a vendor
// whose limits are account-wide (vendors.Error.AccountWide) has the capacity type
// skipped in every region of the vendor instead of the one region.
func (c *CloudProvider) onQuotaExceeded(st *launchState, cand *candidate, accountWide bool) {
	st.blocked[regionKey(cand)] = true
	if c.opts.Quota != nil && c.opts.Quota.HasQuotaInfo(cand.vendor, cand.apiReference, cand.capacityType) {
		// A refresh is worth trying either way; a fresh view decides from then
		// on. A stale or missing view (unknown) falls to the reactive path.
		c.opts.Quota.NotifyQuotaError(cand.vendor, cand.region)
		if _, known := c.opts.Quota.Headroom(cand.vendor, cand.region, cand.apiReference, cand.capacityType); known {
			return
		}
	}
	if accountWide {
		c.opts.Unavailable.MarkQuotaExceededAccountWide(cand.vendor, cand.capacityType)
		return
	}
	c.opts.Unavailable.MarkQuotaExceeded(cand.vendor, cand.region, cand.capacityType)
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
		KubeReserved: kubeReserved(cand.it),
	}
	if prepared != nil {
		rr.Volumes, rr.Local = volumeHints(prepared)
	}
	frags, err := vendors.BootFragmentsFor(drv, nc.Spec.HardeningNames(), vendors.BootFragmentRequest{
		Region: cand.region, Zone: cand.zone, InstanceType: cand.apiReference, Arch: arch,
	})
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "UserDataRender", err)
	}
	for _, f := range frags {
		rr.VendorFragments = append(rr.VendorFragments, bootstrap.Fragment{Name: f.Name, Butane: f.Butane})
	}
	userData, err := c.opts.UserData.Render(ctx, rr)
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
