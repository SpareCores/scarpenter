/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/quota"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// skip reasons of a candidate, as metric label values.
const (
	skipNoQuota     = "no-quota"
	skipUnavailable = "unavailable"
	skipImage       = "image"
	skipRegionFull  = "quota-exceeded"
)

// launchState is what one Create keeps between its attempts.
type launchState struct {
	nc        *v1alpha1.SCNodeClass
	nodeClaim *karpv1.NodeClaim
	token     string
	// userData is rendered once per vendor, region and architecture.
	userData map[string][]byte
	// blocked holds the (vendor, region, capacity type) that returned a quota
	// error during this Create: the others of them are skipped.
	blocked map[string]bool
	// attempts counts vendor calls; skipped counts candidates passed over
	// because of quota; imageErr is the last image failure.
	attempts int
	skipped  int
	imageErr error
	lastErr  error
	// keepToken is set when the failure may hide a launched instance.
	keepToken bool
}

// Create launches the NodeClaim (overview section 4.1): the candidate offerings
// in price order, the first with quota headroom is reserved and launched, errors
// of class InsufficientCapacity and QuotaExceeded fall through to the next one,
// up to --max-launch-attempts vendor calls. When nothing is left it returns
// InsufficientCapacityError, and core deletes the NodeClaim.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	started := c.opts.Clock.Now()
	st := &launchState{nodeClaim: nodeClaim, userData: map[string][]byte{}, blocked: map[string]bool{}}
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

	cands, err := c.candidates(ctx, nc, nodeClaim)
	if err != nil {
		return nil, cloudprovider.NewCreateError(err, "InstanceTypeResolutionFailed", "error resolving instance types")
	}
	if len(cands) == 0 {
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
			c.commit(res, inst.Ref)
			return c.created(nc, nodeClaim, cands, cand, inst), nil
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
			c.opts.Unavailable.MarkUnsupported(cand.vendor, cand.typeName, cand.zone)
		case vendors.ZoneUnusable:
			c.opts.Unavailable.MarkZoneUnusable(cand.vendor, cand.zone)
		case vendors.QuotaExceeded:
			c.onQuotaExceeded(st, cand)
		case vendors.Retryable:
			st.keepToken = true
			return nil, fmt.Errorf("launching %s: %w", cand.typeName, err)
		default: // Fatal, NotFound
			c.publish(nc, EventLaunchFailed, fmt.Sprintf("launching %s in %s failed: %v", cand.typeName, cand.zone, err),
				cand.vendor, vendors.CodeOf(err))
			return nil, cloudprovider.NewCreateError(fmt.Errorf("launching %s: %w", cand.typeName, err),
				"LaunchFailed", truncate(err.Error(), 200))
		}
	}

	switch {
	case st.attempts == 0 && st.skipped == 0 && st.imageErr != nil:
		c.publish(nc, EventImageFailed, st.imageErr.Error(), "image")
		return nil, cloudprovider.NewCreateError(st.imageErr, "ImageResolutionFailed", truncate(st.imageErr.Error(), 200))
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
	if c.opts.Unavailable.IsUnavailable(unavailable.Offering{
		Vendor: cand.vendor, InstanceType: cand.typeName, Region: cand.region, Zone: cand.zone,
		CapacityType: cand.capacityType,
	}) {
		return skipUnavailable
	}
	return ""
}

func regionKey(c *candidate) string { return c.vendor + "/" + c.region + "/" + c.capacityType }

// onQuotaExceeded records a quota error (overview sections 4.1 and 5.6): a
// vendor with a fresh quota view for the type refreshes its view of the region
// and the tracker decides from then on; a vendor without one (no quota API, or a
// stale or missing view) has the region and capacity type skipped for the TTL. Either way the other candidates of the
// region and capacity type are not tried by this Create.
func (c *CloudProvider) onQuotaExceeded(st *launchState, cand *candidate) {
	st.blocked[regionKey(cand)] = true
	if c.opts.Quota != nil && c.opts.Quota.HasQuotaInfo(cand.vendor, cand.apiReference, cand.capacityType) {
		// A refresh is worth trying either way; a fresh view decides from then
		// on. A stale or missing view (unknown) falls to the reactive path.
		c.opts.Quota.NotifyQuotaError(cand.vendor, cand.region)
		if _, known := c.opts.Quota.Headroom(cand.vendor, cand.region, cand.apiReference, cand.capacityType); known {
			return
		}
	}
	c.opts.Unavailable.MarkQuotaExceeded(cand.vendor, cand.region, cand.capacityType)
}

// launch makes one vendor call for the candidate.
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
	providerID := scope.ProviderID(cand.vendor, cand.region, nodeClaim.Name)
	udKey := cand.vendor + "/" + cand.region + "/" + arch
	userData, ok := st.userData[udKey]
	if !ok {
		userData, err = c.opts.UserData.Render(ctx, RenderRequest{
			NodeClass: nc, NodeClaim: nodeClaim, Vendor: cand.vendor, Region: cand.region, Arch: arch,
			Token: st.token, NodeName: nodeClaim.Name, ProviderID: providerID,
		})
		if err != nil {
			return nil, vendors.NewError(vendors.Fatal, "UserDataRender", fmt.Errorf("rendering the user-data: %w", err))
		}
		st.userData[udKey] = userData
	}
	var settings vendors.VendorSettings
	if no, ok := drv.(vendors.NodeOptions); ok {
		settings = no.Defaults()
	}
	req := vendors.CreateRequest{
		Region: cand.region, Zone: cand.zone, InstanceType: cand.apiReference, CapacityType: cand.capacityType,
		Arch: arch, Image: *img, UserData: userData, Settings: settings,
		Tags:           c.launchTags(nc, nodeClaim),
		ExtraTags:      nc.Spec.Tags,
		IdempotencyKey: string(nodeClaim.UID),
		Node: vendors.NodeInfo{
			Name: nodeClaim.Name, ProviderID: providerID, Labels: nodeLabels(nodeClaim.Name, cand.it, cand.off),
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
	nc *v1alpha1.SCNodeClass, nodeClaim *karpv1.NodeClaim, cands []candidate, attempted *candidate, inst *vendors.Instance,
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
			log.FromContext(context.Background()).Info("the instance of a replayed idempotency key matches no candidate; "+
				"describing the NodeClaim from the catalog", "instance", inst.Ref, "type", inst.Type, "zone", inst.Zone)
			return c.fromInstance(nc, attempted.vendor, inst)
		}
	}
	c.putInstance(cand.vendor, inst)
	if c.opts.Quota != nil {
		c.opts.Quota.NotifyChanged(cand.vendor, cand.region)
	}
	out := &karpv1.NodeClaim{}
	out.Labels = nodeLabels(nodeClaim.Name, cand.it, cand.off)
	out.Annotations = map[string]string{
		v1alpha1.AnnotationInstanceRef: string(inst.Ref),
		v1alpha1.AnnotationPrice:       formatPrice(cand.price),
	}
	if hash := nc.Status.Vendors[cand.vendor].Hash; hash != "" {
		out.Annotations[v1alpha1.AnnotationNodeClassHash] = hash
		out.Annotations[v1alpha1.AnnotationNodeClassHashVersion] = nodeclass.HashVersion
	}
	out.Status.ProviderID = scope.ProviderID(cand.vendor, inst.Region, nodeClaim.Name)
	out.Status.Capacity = cand.it.Capacity
	out.Status.Allocatable = cand.it.Allocatable()
	return out
}

// fromInstance describes a NodeClaim from an instance alone (the catalog type
// and offering of the instance), for an instance no candidate explains.
func (c *CloudProvider) fromInstance(
	nc *v1alpha1.SCNodeClass, vendor string, inst *vendors.Instance,
) *karpv1.NodeClaim {
	c.putInstance(vendor, inst)
	out := c.instanceToNodeClaim(vendor, inst)
	out.Annotations[v1alpha1.AnnotationInstanceRef] = string(inst.Ref)
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

type imageKey struct{ vendor, region, arch, channel, version string }

type cachedImage struct {
	image vendors.Image
	until time.Time
}

// image resolves the official image of the NodeClass's Flatcar release for the
// region and architecture, cached for an hour (overview section 9).
func (c *CloudProvider) image(
	ctx context.Context, drv vendors.Driver, nc *v1alpha1.SCNodeClass, vendor, region, arch string,
) (*vendors.Image, error) {
	key := imageKey{vendor, region, arch, string(nc.Spec.Flatcar.Channel), nc.Spec.Flatcar.Version}
	now := c.opts.Clock.Now()
	c.imageMu.Lock()
	if ci, ok := c.images[key]; ok && now.Before(ci.until) {
		c.imageMu.Unlock()
		img := ci.image
		return &img, nil
	}
	c.imageMu.Unlock()
	img, err := drv.ResolveImage(ctx, vendors.ImageRequest{
		Region: region, Arch: arch, Channel: key.channel, Version: key.version,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving the image of %s in %s (%s): %w", vendor, region, arch, err)
	}
	c.imageMu.Lock()
	c.images[key] = cachedImage{image: *img, until: now.Add(c.opts.ImageCacheTTL)}
	c.imageMu.Unlock()
	return img, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// commit and release are nil-safe: without a tracker there is no reservation.
func (c *CloudProvider) commit(res *quota.Reservation, ref vendors.InstanceRef) {
	if c.opts.Quota != nil {
		c.opts.Quota.Commit(res, ref)
	}
}

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
