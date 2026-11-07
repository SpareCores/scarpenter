/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package cloudprovider implements Karpenter core's CloudProvider interface
// for SCarpenter, vendor-neutral on top of the driver contract (overview
// section 4): the instance types come from the catalog, nodes are launched
// through the driver of the offering's vendor, quota-aware (section 5.9) and
// with a fallback over the next offerings on capacity errors (section 4.1).
package cloudprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/quota"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Name is the name of the cloud provider.
const Name = "scarpenter"

// NodeClassDrift is the drift reason of a NodeClaim whose NodeClass changed in
// a way that changes the node (overview section 6.3).
const NodeClassDrift cloudprovider.DriftReason = "NodeClassDrift"

// Defaults.
const (
	// DefaultInstanceCacheTTL is how long the List cache serves (overview
	// section 4).
	DefaultInstanceCacheTTL = 30 * time.Second
	// DefaultImageCacheTTL is how long a resolved image is reused (overview
	// section 9).
	DefaultImageCacheTTL = time.Hour
	// DefaultMaxLaunchAttempts is --max-launch-attempts.
	DefaultMaxLaunchAttempts = 3
)

// repairToleration is how long a node may be NotReady before core repairs it
// (overview section 4); users who do not want repair disable core's NodeRepair
// feature gate.
const repairToleration = 10 * time.Minute

// Options are the dependencies of the CloudProvider.
type Options struct {
	Client      client.Client
	Catalog     *catalog.Catalog
	Registry    *vendors.Registry
	Unavailable *unavailable.Cache
	// Quota may be nil: no quota knowledge, every vendor reactive.
	Quota *quota.Tracker
	// Scope resolves the regions to list instances in.
	Scope *scope.Resolver
	// Recorder publishes Events; nil publishes none.
	Recorder events.Recorder
	// Resources provides the vendor resources of a launch to the drivers'
	// Prepare; nil means a driver that needs one fails the launch.
	Resources *resources.Engine
	// Tokens and UserData default to StubBootstrap.
	Tokens   TokenMinter
	UserData UserDataRenderer
	// InstanceTypeStore applies NodeOverlays to the candidates when the feature
	// gate is on; nil applies none.
	InstanceTypeStore *nodeoverlay.InstanceTypeStore
	ClusterName       string
	MaxLaunchAttempts int
	InstanceCacheTTL  time.Duration
	ImageCacheTTL     time.Duration
	Clock             clock.Clock
	// Registerer receives the metrics; nil registers none.
	Registerer prometheus.Registerer
}

// CloudProvider is SCarpenter's implementation of core's interface.
type CloudProvider struct {
	opts    Options
	metrics *metrics

	listMu sync.Mutex
	cache  instanceCache

	imageMu sync.Mutex
	images  map[imageKey]cachedImage
}

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

// New returns the CloudProvider.
func New(opts Options) *CloudProvider {
	if opts.Tokens == nil {
		opts.Tokens = StubBootstrap{}
	}
	if opts.UserData == nil {
		opts.UserData = StubBootstrap{}
	}
	if opts.MaxLaunchAttempts < 1 {
		opts.MaxLaunchAttempts = DefaultMaxLaunchAttempts
	}
	if opts.InstanceCacheTTL <= 0 {
		opts.InstanceCacheTTL = DefaultInstanceCacheTTL
	}
	if opts.ImageCacheTTL <= 0 {
		opts.ImageCacheTTL = DefaultImageCacheTTL
	}
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	return &CloudProvider{opts: opts, metrics: newMetrics(opts.Registerer), images: map[imageKey]cachedImage{}}
}

// Name returns scarpenter.
func (c *CloudProvider) Name() string { return Name }

// GetSupportedNodeClasses returns SCNodeClass.
func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.SCNodeClass{}}
}

// DisruptionReasons has nothing beyond core's.
func (c *CloudProvider) DisruptionReasons() []karpv1.DisruptionReason { return nil }

// RepairPolicies repairs nodes that are Ready=False or Ready=Unknown for ten
// minutes.
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: repairToleration},
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionUnknown, TolerationDuration: repairToleration},
	}
}

// Reboot is not supported.
func (c *CloudProvider) Reboot(context.Context, *karpv1.NodeClaim, string) error {
	return cloudprovider.NewNodeRebootNotImplementedError()
}

// GetInstanceTypes returns the instance types of the NodePool's NodeClass for
// the vendors the NodePool allows, pre-filtered by its requirements; offerings
// are marked unavailable by the unavailable-offerings cache and the quota
// tracker (overview section 4). A vendor that is not ready in the NodeClass
// yields no instance types.
func (c *CloudProvider) GetInstanceTypes(
	ctx context.Context, nodePool *karpv1.NodePool,
) ([]*cloudprovider.InstanceType, error) {
	nc, err := c.resolveNodeClass(ctx, nodePool.Spec.Template.Spec.NodeClassRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Without the NodeClass there is nothing to offer.
			return nil, nil
		}
		return nil, fmt.Errorf("resolving the NodeClass: %w", err)
	}
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodePool.Spec.Template.Spec.Requirements...)
	its, err := c.opts.Catalog.InstanceTypes(ctx, catalog.Query{Class: nc.Name, Filter: catalog.NewFilter(reqs)})
	if err != nil {
		return nil, err
	}
	ready := readyVendors(nc)
	if len(ready) == len(nc.Spec.Vendors.Names()) {
		return its, nil
	}
	out := make([]*cloudprovider.InstanceType, 0, len(its))
	for _, it := range its {
		if ref, ok := c.opts.Catalog.Lookup(it.Name); ok && ready[ref.Vendor] {
			out = append(out, it)
		}
	}
	return out, nil
}

// IsDrifted reports NodeClassDrift when the hash the NodeClaim was launched with
// differs from the NodeClass's current hash for the NodeClaim's vendor (overview
// section 6.3). Requirement drift is core's.
func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	launched := nodeClaim.Annotations[v1alpha1.AnnotationNodeClassHash]
	if launched == "" || nodeClaim.Spec.NodeClassRef == nil {
		return "", nil
	}
	vendor, _, _, err := scope.ParseProviderID(nodeClaim.Status.ProviderID)
	if err != nil {
		return "", nil //nolint:nilerr // not launched yet: nothing to compare
	}
	nc, err := c.resolveNodeClass(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("resolving the NodeClass: %w", err)
	}
	current := nc.Status.Vendors[vendor].Hash
	if current == "" {
		return "", nil
	}
	if current != launched {
		return NodeClassDrift, nil
	}
	return c.profileDrift(nodeClaim, nc, vendor), nil
}

// profileDrift compares the hashes of the profiles the node was launched with
// (recorded at launch) with the profiles' current blocks: changing one profile
// drifts only the nodes that use it, and a profile that was removed or lost its
// block for the vendor drifts them too (overview section 6.3).
func (c *CloudProvider) profileDrift(
	nodeClaim *karpv1.NodeClaim, nc *v1alpha1.SCNodeClass, vendor string,
) cloudprovider.DriftReason {
	raw := nodeClaim.Annotations[v1alpha1.AnnotationProfileHashes]
	if raw == "" {
		return ""
	}
	var launched map[string]string
	if err := json.Unmarshal([]byte(raw), &launched); err != nil {
		return ""
	}
	in, ok, err := options.InputFromNodeClass(nc, vendor)
	if err != nil || !ok {
		return ""
	}
	for key, hash := range launched {
		if current, ok := in.CurrentHash(key); !ok || current != hash {
			return NodeClassDrift
		}
	}
	return ""
}

// Get returns the NodeClaim of the instance behind the provider ID: from the
// List cache, else from the driver (by the NodeClaim tag). Terminated or
// terminating instances are NotFound.
func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	vendor, region, name, err := scope.ParseProviderID(providerID)
	if err != nil {
		return nil, cloudprovider.NewNodeClaimNotFoundError(err)
	}
	all, err := c.instances(ctx, false)
	if err != nil {
		return nil, err
	}
	if li, ok := all[providerID]; ok {
		return c.instanceToNodeClaim(li.vendor, li.instance), nil
	}
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("vendor %q is not enabled", vendor))
	}
	inst, err := drv.FindInstance(ctx, region, c.tags(name))
	if err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			return nil, cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return nil, fmt.Errorf("finding the instance of %s: %w", providerID, err)
	}
	if inst.Gone() {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance of %s is %s", providerID, inst.State))
	}
	c.putInstance(vendor, inst)
	return c.instanceToNodeClaim(vendor, inst), nil
}

// List returns the NodeClaims of every instance of the cluster, cached for 30 s.
func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	all, err := c.instances(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]*karpv1.NodeClaim, 0, len(all))
	for _, li := range all {
		out = append(out, c.instanceToNodeClaim(li.vendor, li.instance))
	}
	return out, nil
}

// tags are the tags that identify a NodeClaim's instance.
func (c *CloudProvider) tags(nodeClaim string) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{vendors.TagCluster: c.opts.ClusterName, vendors.TagNodeClaim: nodeClaim}
}

// Delete terminates the instance of the NodeClaim (overview section 4): by the
// instance-ref annotation, else by tag lookup. An instance that is gone is
// NodeClaimNotFound, which core treats as done; core calls Delete again until it
// says so.
func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	providerID := nodeClaim.Status.ProviderID
	vendor, region, name, err := scope.ParseProviderID(providerID)
	if err != nil {
		return cloudprovider.NewNodeClaimNotFoundError(err)
	}
	drv, ok := c.opts.Registry.Driver(vendor)
	if !ok {
		// No driver can reach the instance (nor can the garbage collector):
		// failing forever would only keep the finalizer.
		log.FromContext(ctx).Error(nil, "the vendor of the NodeClaim is not enabled, treating its instance as gone",
			"provider-id", providerID, "vendor", vendor)
		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("vendor %q is not enabled", vendor))
	}
	var inst *vendors.Instance
	if ref := nodeClaim.Annotations[v1alpha1.AnnotationInstanceRef]; ref != "" {
		inst, err = drv.GetInstance(ctx, vendors.InstanceRef(ref))
	} else {
		inst, err = drv.FindInstance(ctx, region, c.tags(name))
	}
	if err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			c.forget(ctx, vendor, region, providerID, name)
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return fmt.Errorf("looking up the instance of %s: %w", providerID, err)
	}
	if inst.Gone() {
		if inst.State == vendors.StateTerminated {
			c.forget(ctx, vendor, region, providerID, name)
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance of %s is terminated", providerID))
		}
		return nil // terminating: core asks again
	}
	if err := drv.DeleteInstance(ctx, inst.Ref); err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			c.forget(ctx, vendor, region, providerID, name)
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return fmt.Errorf("deleting the instance of %s: %w", providerID, err)
	}
	if c.opts.Quota != nil {
		c.opts.Quota.ReleaseInstance(vendor, inst.Ref)
		c.opts.Quota.NotifyChanged(vendor, region)
	}
	c.dropInstance(providerID)
	return nil
}

// forget cleans up after an instance that is gone: the cache entry and the
// bootstrap token.
func (c *CloudProvider) forget(ctx context.Context, vendor, region, providerID, nodeClaim string) {
	c.dropInstance(providerID)
	if err := c.opts.Tokens.Delete(ctx, nodeClaim); err != nil {
		// A leftover token expires and is collected; it must not block the delete.
		log.FromContext(ctx).Error(err, "deleting the bootstrap token", "nodeclaim", nodeClaim)
	}
	if c.opts.Quota != nil {
		c.opts.Quota.NotifyChanged(vendor, region)
	}
}

var errNoCandidates = errors.New("no offering satisfies the NodeClaim's requirements and has capacity")
