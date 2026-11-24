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
	"slices"
	"strings"
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
// (overview section 4); repair runs only when core's NodeRepair feature gate is on.
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
	// TerminatingGrace is how long an instance may be seen terminating before
	// Delete deletes it again (a stopped instance is terminating for ever);
	// defaults to vendors.DefaultTerminatingGrace.
	TerminatingGrace time.Duration
	InstanceCacheTTL time.Duration
	ImageCacheTTL    time.Duration
	Clock            clock.Clock
	// Registerer receives the metrics; nil registers none.
	Registerer prometheus.Registerer
	// OnQuotaRefusal is told when a launch attempt is refused for quota (the
	// quota tracker has no headroom, or the vendor says so): the billing
	// retention controller releases held empty nodes of the scope on it. Nil
	// tells nobody.
	OnQuotaRefusal func(QuotaRefusal)
	// OnInstanceGone is told, from Delete, of a NodeClaim whose instance is gone
	// and which SCarpenter did not delete (a silent reclaim, a crash, a delete
	// outside SCarpenter): the interruption controller records the reason. goneReason
	// is the driver's Instance.GoneReason ("" when it cannot tell). Nil tells nobody.
	OnInstanceGone func(ctx context.Context, nodeClaim *karpv1.NodeClaim, vendor, goneReason string)
}

// QuotaRefusal is a launch of a NodeClass that was refused for a vendor quota
// in a region and capacity type.
type QuotaRefusal struct {
	NodeClass    string
	Vendor       string
	Region       string
	CapacityType string
	// CPU is the vCPUs of the type the launch wanted, the amount that would
	// have to be freed.
	CPU int64
}

// CloudProvider is SCarpenter's implementation of core's interface.
type CloudProvider struct {
	opts    Options
	metrics *metrics
	// started is when New ran: the start of FactsStartupGrace.
	started time.Time

	listMu sync.Mutex
	cache  instanceCache

	// terminating keeps the first sighting of each terminating instance by
	// Delete, keyed by the NodeClaim's provider ID.
	terminating *vendors.TerminatingTracker

	// deleteIssued are the provider IDs whose delete the driver accepted
	// (DeleteInstance returned nil), with the time: Delete leaves such an
	// instance, terminating, to the vendor for the terminating grace.
	// deleteAttempted are the provider IDs Delete has called DeleteInstance for,
	// whatever the answer, with the time: an instance found gone or stopping
	// afterwards (core calls Delete several times, and a driver may stop an
	// instance in steps) is the expected end, not a loss (see reportGone, the only
	// reader). Entries are dropped after an hour. In memory only; the durable
	// signal is core's InstanceTerminating condition, this covers the moment
	// before core has stored it.
	issuedMu        sync.Mutex
	deleteIssued    map[string]time.Time
	deleteAttempted map[string]time.Time

	imageMu sync.Mutex
	images  map[imageKey]cachedImage
	// imageGen counts ForgetImage calls: a resolution that started before one
	// must not be cached after it, it may carry the forgotten image.
	imageGen uint64

	// notReadyLogged is, per NodeClass, the vendors GetInstanceTypes last logged
	// as not ready.
	notReadyMu     sync.Mutex
	notReadyLogged map[string]string
	// factsWarned is, per NodeClass, the vendors warnMissingFacts last named
	// (under notReadyMu).
	factsWarned map[string]string
	// gpuUnguarded are the NodePools withoutUnguardedGPUs has logged (under notReadyMu).
	gpuUnguarded map[string]bool
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
	return &CloudProvider{
		opts: opts, metrics: newMetrics(opts.Registerer), started: opts.Clock.Now(), images: map[imageKey]cachedImage{},
		terminating:  vendors.NewTerminatingTracker(opts.Clock, opts.TerminatingGrace),
		deleteIssued: map[string]time.Time{}, deleteAttempted: map[string]time.Time{},
	}
}

// Name returns scarpenter.
func (c *CloudProvider) Name() string { return Name }

// GetSupportedNodeClasses returns SCNodeClass.
func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.SCNodeClass{}}
}

// DisruptionReasons has nothing beyond core's.
func (c *CloudProvider) DisruptionReasons() []karpv1.DisruptionReason { return nil }

// RepairPolicies replaces nodes that are Ready=False or Ready=Unknown for ten
// minutes. Core's matcher wants exactly one policy without a ReasonRegex (the
// fallback, which must be ReplaceNode) and an explicit action on the others.
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		{
			ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse,
			TolerationDuration: repairToleration, Action: cloudprovider.ReplaceNode,
		},
		{
			ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionUnknown, ReasonRegex: ".*",
			TolerationDuration: repairToleration, Action: cloudprovider.ReplaceNode,
		},
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
	// While the catalog and the driver facts load after a start the list is
	// incomplete. Core reads an incomplete list as "instance type not found" and
	// marks every NodeClaim Drifted, so report an error instead.
	if !c.opts.Catalog.Loaded(nc.Name) {
		return nil, fmt.Errorf("the catalog of the NodeClass %s is not loaded yet", nc.Name)
	}
	ready := readyVendors(nc)
	if missing := c.missingFacts(nc, ready); len(missing) > 0 {
		if c.opts.Clock.Since(c.started) < FactsStartupGrace {
			return nil, fmt.Errorf("the driver facts of %s are not loaded yet", strings.Join(missing, ", "))
		}
		// Facts that keep failing must not stop the other vendors: offer what there is.
		c.warnMissingFacts(ctx, nc.Name, missing)
	}
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodePool.Spec.Template.Spec.Requirements...)
	its, err := c.opts.Catalog.InstanceTypes(ctx, catalog.Query{Class: nc.Name, Filter: catalog.NewFilter(reqs)})
	if err != nil {
		return nil, err
	}
	c.opts.Catalog.LogNodePool(ctx, nc.Name, nodePool.Name, reqs)
	c.logNotReady(ctx, nc, ready)
	if len(ready) == len(nc.Spec.Vendors.Names()) {
		return c.withoutUnguardedGPUs(ctx, nodePool, its), nil
	}
	out := make([]*cloudprovider.InstanceType, 0, len(its))
	for _, it := range its {
		if ref, ok := c.opts.Catalog.Lookup(it.Name); ok && ready[ref.Vendor] {
			out = append(out, it)
		}
	}
	return c.withoutUnguardedGPUs(ctx, nodePool, out), nil
}

// withoutUnguardedGPUs leaves out the NVIDIA types when the NodePool does not list the
// gpu-pending taint in its startupTaints (ADR-031): without it core treats the taint of a
// registered GPU node as permanent and may launch more nodes than the Pods need. It
// fails closed, and logs once per NodePool until the taint is listed.
func (c *CloudProvider) withoutUnguardedGPUs(
	ctx context.Context, nodePool *karpv1.NodePool, its []*cloudprovider.InstanceType,
) []*cloudprovider.InstanceType {
	pending := corev1.Taint{Key: v1alpha1.TaintGPUPending, Effect: corev1.TaintEffectNoSchedule}
	guarded := slices.ContainsFunc(nodePool.Spec.Template.Spec.StartupTaints,
		func(t corev1.Taint) bool { return t.MatchTaint(&pending) })
	c.notReadyMu.Lock()
	defer c.notReadyMu.Unlock()
	if guarded {
		delete(c.gpuUnguarded, nodePool.Name)
		return its
	}
	out := make([]*cloudprovider.InstanceType, 0, len(its))
	for _, it := range its {
		if it.Requirements.Get(v1alpha1.LabelNVIDIADriverBranch).Operator() != corev1.NodeSelectorOpIn {
			out = append(out, it)
		}
	}
	if len(out) < len(its) && !c.gpuUnguarded[nodePool.Name] {
		if c.gpuUnguarded == nil {
			c.gpuUnguarded = map[string]bool{}
		}
		c.gpuUnguarded[nodePool.Name] = true
		log.FromContext(ctx).Info("the NVIDIA GPU types are not offered: the NodePool does not list the taint "+
			v1alpha1.TaintGPUPending+":NoSchedule in spec.template.spec.startupTaints (ADR-031)",
			"nodepool", nodePool.Name)
	}
	return out
}

// FactsStartupGrace is how long after the start GetInstanceTypes reports an
// error while the facts of a ready vendor are not loaded. Afterwards it offers
// the types it has, so that a vendor whose driver keeps failing does not stop
// the others. It is the vendor load timeout (facts.DefaultVendorTimeout) and a margin.
const FactsStartupGrace = 4 * time.Minute

// missingFacts returns the ready vendors of the NodeClass whose driver facts
// are not loaded.
func (c *CloudProvider) missingFacts(nc *v1alpha1.SCNodeClass, ready map[string]bool) []string {
	var missing []string
	for _, v := range nc.Spec.Vendors.Names() {
		if ready[v] && !c.opts.Catalog.FactsLoaded(v) {
			missing = append(missing, v)
		}
	}
	return missing
}

// warnMissingFacts logs once per change of the set that the NodeClass's list
// goes without the types of vendors whose facts are still not loaded.
func (c *CloudProvider) warnMissingFacts(ctx context.Context, class string, missing []string) {
	line := strings.Join(missing, ", ")
	c.notReadyMu.Lock()
	defer c.notReadyMu.Unlock()
	if c.factsWarned[class] == line {
		return
	}
	if c.factsWarned == nil {
		c.factsWarned = map[string]string{}
	}
	c.factsWarned[class] = line
	log.FromContext(ctx).Info("the driver facts are still not loaded, offering the types without them",
		"nodeclass", class, "vendors", missing)
}

// logNotReady logs, at V(1), the vendors whose types GetInstanceTypes leaves out
// (reason VendorNotReady, detail the VendorReady condition's reason), once per
// change of that set.
func (c *CloudProvider) logNotReady(ctx context.Context, nc *v1alpha1.SCNodeClass, ready map[string]bool) {
	var notReady []string
	for _, v := range nc.Spec.Vendors.Names() {
		if !ready[v] {
			notReady = append(notReady, v+" ("+vendorNotReadyReason(nc, v)+")")
		}
	}
	line := strings.Join(notReady, ", ")
	c.notReadyMu.Lock()
	defer c.notReadyMu.Unlock()
	if c.notReadyLogged[nc.Name] == line {
		return
	}
	if c.notReadyLogged == nil {
		c.notReadyLogged = map[string]string{}
	}
	c.notReadyLogged[nc.Name] = line
	if line != "" {
		log.FromContext(ctx).V(1).Info("not offering the types of vendors that are not ready",
			"nodeclass", nc.Name, "reason", catalog.VendorNotReady, "vendors", notReady)
	}
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
// says so. A terminating instance is left to the vendor for the terminating
// grace and deleted again after it.
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
			c.reportGone(ctx, nodeClaim, vendor, "")
			c.forget(ctx, vendor, region, providerID, name)
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return fmt.Errorf("looking up the instance of %s: %w", providerID, err)
	}
	if inst.State == vendors.StateTerminated {
		c.reportGone(ctx, nodeClaim, vendor, inst.GoneReason)
		c.forget(ctx, vendor, region, providerID, name)
		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance of %s is terminated", providerID))
	}
	if inst.State == vendors.StateTerminating {
		c.reportGone(ctx, nodeClaim, vendor, inst.GoneReason)
		// After a delete the driver accepted, the vendor is probably deleting it
		// and core asks again. One that stays terminating (a stopped instance
		// never goes away by itself) is deleted again after the grace;
		// DeleteInstance is idempotent. One SCarpenter has not deleted yet (a
		// stopped one, or one a driver halted in its steps) is deleted at once.
		if c.deleteWasIssued(providerID) && !c.terminating.Expired(vendor, vendors.InstanceRef(providerID)) {
			return nil
		}
		c.terminating.Forget(vendor, vendors.InstanceRef(providerID)) // the next delete comes after another grace
	}
	c.note(c.deleteAttempted, providerID)
	if err := drv.DeleteInstance(ctx, inst.Ref); err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			c.forget(ctx, vendor, region, providerID, name)
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return fmt.Errorf("deleting the instance of %s: %w", providerID, err)
	}
	c.noteDeleteIssued(providerID)
	if c.opts.Quota != nil {
		c.opts.Quota.ReleaseInstance(vendor, inst.Ref)
		c.opts.Quota.NotifyChanged(vendor, region)
	}
	c.dropInstance(providerID)
	return nil
}

func (c *CloudProvider) noteDeleteIssued(providerID string) { c.note(c.deleteIssued, providerID) }

func (c *CloudProvider) deleteWasIssued(providerID string) bool {
	c.issuedMu.Lock()
	defer c.issuedMu.Unlock()
	_, ok := c.deleteIssued[providerID]
	return ok
}

// note records the provider ID in one of the two delete sets and drops the
// entries older than an hour.
func (c *CloudProvider) note(set map[string]time.Time, providerID string) {
	now := c.opts.Clock.Now()
	c.issuedMu.Lock()
	defer c.issuedMu.Unlock()
	for id, at := range set {
		if now.Sub(at) > time.Hour {
			delete(set, id)
		}
	}
	set[providerID] = now
}

// reportGone tells OnInstanceGone that the NodeClaim's instance is gone
// (terminating or terminated), unless SCarpenter deleted it or core is
// disrupting the NodeClaim on purpose (consolidation, drift, expiry: core sets
// DisruptionReason first). What is left is an instance the vendor reclaimed or
// that was deleted outside SCarpenter (plan 26 section 5).
//
// Only the first Delete of a NodeClaim reports: core sets the InstanceTerminating
// condition after any Delete that did not fail, so a NodeClaim that has it was
// seen by Delete before, which deleted the instance or found it gone already.
// The condition is stored on the NodeClaim, so a restart of SCarpenter between
// two Deletes does not turn its own delete into a reported loss.
func (c *CloudProvider) reportGone(ctx context.Context, nodeClaim *karpv1.NodeClaim, vendor, goneReason string) {
	if c.opts.OnInstanceGone == nil {
		return
	}
	c.issuedMu.Lock()
	_, issued := c.deleteAttempted[nodeClaim.Status.ProviderID]
	c.issuedMu.Unlock()
	conds := nodeClaim.StatusConditions()
	if issued || conds.Get(karpv1.ConditionTypeDisruptionReason) != nil ||
		conds.Get(karpv1.ConditionTypeInstanceTerminating).IsTrue() {
		return
	}
	c.opts.OnInstanceGone(ctx, nodeClaim, vendor, goneReason)
}

// forget cleans up after an instance that is gone: the cache entry, the
// bootstrap token and the vendor's quota marks of the region, since the quota the
// instance held is free now.
func (c *CloudProvider) forget(ctx context.Context, vendor, region, providerID, nodeClaim string) {
	c.opts.Unavailable.ClearQuotaExceeded(vendor, region)
	c.dropInstance(providerID)
	c.terminating.Forget(vendor, vendors.InstanceRef(providerID))
	if err := c.opts.Tokens.Delete(ctx, nodeClaim); err != nil {
		// A leftover token expires and is collected; it must not block the delete.
		log.FromContext(ctx).Error(err, "deleting the bootstrap token", "nodeclaim", nodeClaim)
	}
	if c.opts.Quota != nil {
		c.opts.Quota.NotifyChanged(vendor, region)
	}
}

var errNoCandidates = errors.New("no offering satisfies the NodeClaim's requirements and has capacity")
