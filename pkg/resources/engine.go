/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package resources is the vendor-neutral engine behind vendor resources
// (00a section 9, ADR-018): for every resource kind a driver declares, a
// NodeClass says use (brought by the user: IDs per region, a name pattern or a
// tag selector; validated, never modified or deleted), create (managed by
// SCarpenter with a lifecycle) or forbid; with neither, the vendor's own
// default resource is used where it exists and the resource is created
// otherwise. The engine resolves and ensures resources for Create (through the
// ResourceProvider a driver's Prepare receives), checks them per region for
// the NodeClass status and the catalog, creates the preCreate ones, and
// deletes managed resources by their lifecycle (garbage collection) in the
// driver's dependency order.
package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Defaults of the options.
const (
	// DefaultTTL is the idle time of an idleTTL resource without a ttl in the
	// NodeClass (--resource-default-ttl).
	DefaultTTL = 24 * time.Hour
	// DefaultPerUseGrace is how long an unused perUse resource is kept, for
	// launches in flight (overview section 10).
	DefaultPerUseGrace = 2 * time.Minute

	// ensureTTL and useTTL are how long a found resource is trusted without
	// asking the vendor again.
	ensureTTL = time.Minute
	useTTL    = 30 * time.Second
	// maxBackoff caps the retry delay of a failing deletion.
	maxBackoff = 10 * time.Minute
)

// Event reasons.
const (
	EventCreated       = "ResourceCreated"
	EventAdopted       = "ResourceAdopted"
	EventDeleted       = "ResourceDeleted"
	EventDeleteFailed  = "ResourceDeleteFailed"
	EventCreateFailed  = "ResourceCreateFailed"
	eventDedupeTimeout = 10 * time.Minute
)

// Options configures the Engine.
type Options struct {
	Registry *vendors.Registry
	// Reader reads the SCNodeClasses (the cached client).
	Reader      client.Reader
	Clock       clock.Clock
	ClusterName string
	// DefaultTTL and PerUseGrace default to DefaultTTL and DefaultPerUseGrace.
	DefaultTTL  time.Duration
	PerUseGrace time.Duration
	// DryRun logs and counts the deletions without making them.
	DryRun   bool
	Recorder events.Recorder
	// DeploymentRef names the object that receives the Events when the
	// resource's NodeClass is gone; nil or a nil result means log only.
	DeploymentRef func() *corev1.ObjectReference
	// Regions returns the regions in use for a vendor (garbage collection lists
	// them every sweep).
	Regions func(vendor string) []string
	// AllRegions returns every region the driver can use: the garbage collector
	// also lists those, every FullSweepEvery-th sweep and after a NodeClass
	// disappeared, so that managed resources of regions that no NodeClass or
	// NodeClaim uses any more are still found by their ownership marker. nil
	// means the regions in use only.
	AllRegions func(vendor string) []string
	// FullSweepEvery is how many sweeps pass between two sweeps of every
	// region (default 10).
	FullSweepEvery int
	// Zones returns the zone names of a region, to expand {zone} patterns of
	// `use` references per zone; nil means they cannot be expanded.
	Zones func(vendor, region string) []string
	// ResolveTTL is how long an answer for a `use` reference is trusted
	// (default 30s; the status check interval is a sensible bound).
	ResolveTTL time.Duration
	// Unavailable is bumped when the readiness of a region changes, so that the
	// catalog drops its cached results; may be nil.
	Unavailable *unavailable.Cache
	// Registerer receives the metrics; nil registers none.
	Registerer prometheus.Registerer
}

// RegionState is whether the resources a NodeClass needs in a region are ready.
type RegionState struct {
	Ready  bool
	Reason string
	// Zones are the zones of a ready region that lack a resource (a {zone}
	// pattern that matches nothing there), with the reason: only their offerings
	// are unavailable.
	Zones map[string]string
}

type regionKey struct{ class, vendor, region string }

type cachedRefs struct {
	refs  []vendors.ResourceRef
	until time.Time
}

type observation struct {
	inUse bool
	last  time.Time
}

type failure struct {
	attempts int
	next     time.Time
	message  string
}

// Engine implements the resource framework. It is safe for concurrent use.
type Engine struct {
	opts    Options
	metrics *metrics

	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	ensured  map[string]cachedRefs // by vendor/kind/region/name
	resolved map[string]cachedRefs // by vendor/kind/region/reference
	touched  map[string]time.Time  // by vendor/id: last launch that required it
	seen     map[string]bool       // by vendor/id: created or adopted by this process
	observed map[string]observation
	failures map[string]*failure
	regions  map[regionKey]RegionState
	// firstSeen is when this process first saw a managed resource: a restart
	// restarts the idle clock instead of trusting a driver that cannot say
	// when the resource was last used.
	firstSeen map[string]time.Time
	// sweeps counts sweeps; knownClasses are the NodeClasses the last sweep saw;
	// managedKeys are the gauge series set per vendor.
	sweeps       int
	knownClasses sets.Set[string]
	managedKeys  map[string]sets.Set[string]
	// watch are the regions outside the regions in use where a full sweep found
	// managed resources: they are swept every time until they are empty.
	watch map[string]sets.Set[string]
}

// New returns an Engine.
func New(opts Options) *Engine {
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.DefaultTTL <= 0 {
		opts.DefaultTTL = DefaultTTL
	}
	if opts.PerUseGrace <= 0 {
		opts.PerUseGrace = DefaultPerUseGrace
	}
	if opts.ResolveTTL <= 0 {
		opts.ResolveTTL = useTTL
	}
	if opts.FullSweepEvery <= 0 {
		opts.FullSweepEvery = 10
	}
	return &Engine{
		opts: opts, metrics: newMetrics(opts.Registerer),
		locks: map[string]*sync.Mutex{}, ensured: map[string]cachedRefs{}, resolved: map[string]cachedRefs{},
		touched: map[string]time.Time{}, seen: map[string]bool{}, observed: map[string]observation{},
		failures: map[string]*failure{}, regions: map[regionKey]RegionState{},
		firstSeen: map[string]time.Time{}, managedKeys: map[string]sets.Set[string]{},
		watch: map[string]sets.Set[string]{},
	}
}

// managed returns the driver's ManagedResources capability and its kinds.
func (e *Engine) managed(vendor string) (vendors.ManagedResources, map[vendors.ResourceKind]vendors.KindSpec, bool) {
	drv, ok := e.opts.Registry.Driver(vendor)
	if !ok {
		return nil, nil, false
	}
	mr, ok := drv.(vendors.ManagedResources)
	if !ok {
		return nil, nil, false
	}
	kinds := map[vendors.ResourceKind]vendors.KindSpec{}
	for _, k := range mr.Kinds() {
		kinds[k.Kind] = k
	}
	return mr, kinds, true
}

func (e *Engine) lock(vendor string, kind vendors.ResourceKind, region string) *sync.Mutex {
	k := vendor + "/" + string(kind) + "/" + region
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.locks[k]
	if m == nil {
		m = &sync.Mutex{}
		e.locks[k] = m
	}
	return m
}

// Provider returns the ResourceProvider of a launch into the region of the
// vendor with the NodeClass's configuration.
// zone is the zone of the launch attempt, used for {zone} patterns when a
// request names none.
func (e *Engine) Provider(nc *v1alpha1.SCNodeClass, vendor, region, zone string) vendors.ResourceProvider {
	return &provider{e: e, nc: nc, vendor: vendor, region: region, zone: zone}
}

type provider struct {
	e      *Engine
	nc     *v1alpha1.SCNodeClass
	vendor string
	region string
	zone   string
}

// Require implements vendors.ResourceProvider.
func (p *provider) Require(ctx context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	return p.e.require(ctx, p.nc, p.vendor, p.region, p.zone, req)
}

func missing(format string, args ...any) error {
	return vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing, fmt.Errorf(format, args...))
}

func forbidden(format string, args ...any) error {
	return vendors.NewError(vendors.Unsupported, vendors.CodeResourceForbidden, fmt.Errorf(format, args...))
}

// useRefFor builds the UseRef of a `use` block for the region and zone. An
// explicit ID list without an entry for the region is an error here: an empty
// UseRef would mean the vendor's default.
func useRefFor(u *common.ResourceUse, region, zone string) (vendors.UseRef, error) {
	switch {
	case u.IDs != nil:
		ids := u.IDs[region]
		if len(ids) == 0 {
			return vendors.UseRef{}, fmt.Errorf("no IDs are configured for the region %s", region)
		}
		return vendors.UseRef{IDs: ids}, nil
	case u.Name != "":
		return vendors.UseRef{NamePattern: strings.NewReplacer("{region}", region, "{zone}", zone).Replace(u.Name)}, nil
	case u.Selector != nil:
		return vendors.UseRef{Tags: u.Selector.Tags}, nil
	}
	return vendors.UseRef{}, errors.New("the use block names no IDs, name or selector")
}

func describe(ref vendors.UseRef) string {
	switch {
	case len(ref.IDs) > 0:
		return "IDs " + strings.Join(ref.IDs, ", ")
	case ref.NamePattern != "":
		return "the name " + ref.NamePattern
	case len(ref.Tags) > 0:
		return fmt.Sprintf("the tags %v", ref.Tags)
	}
	return "the vendor's default"
}

// resolve asks the driver for existing resources, trusting a recent answer.
func (e *Engine) resolve(
	ctx context.Context, mr vendors.ManagedResources, vendor string, kind vendors.ResourceKind, region string,
	ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	key := fmt.Sprintf("%s/%s/%s/%v", vendor, kind, region, ref)
	now := e.opts.Clock.Now()
	e.mu.Lock()
	if c, ok := e.resolved[key]; ok && now.Before(c.until) {
		e.mu.Unlock()
		return c.refs, nil
	}
	e.mu.Unlock()
	refs, err := mr.Resolve(ctx, kind, region, ref)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.resolved[key] = cachedRefs{refs: refs, until: now.Add(e.opts.ResolveTTL)}
	e.mu.Unlock()
	return refs, nil
}

func (e *Engine) require(
	ctx context.Context, nc *v1alpha1.SCNodeClass, vendor, region, launchZone string, req vendors.ResourceRequest,
) ([]vendors.ResourceRef, error) {
	mr, kinds, ok := e.managed(vendor)
	if !ok {
		return nil, vendors.Errorf(vendors.Fatal, "the driver of %s has no managed resources", vendor)
	}
	spec, ok := kinds[req.Kind]
	if !ok {
		return nil, vendors.Errorf(vendors.Fatal, "the driver of %s has no resource kind %q", vendor, req.Kind)
	}
	cfgs, err := options.ResourceConfig(nc, vendor)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "ResourceConfig", err)
	}
	var cfg common.Resource
	if c, ok := cfgs[string(req.Kind)]; ok {
		cfg = c
	}

	// A missing resource makes the region's offerings unavailable until it
	// shows up (the status controller re-evaluates).
	fail := func(err error) error {
		e.setRegion(nc.Name, vendor, region, RegionState{Reason: fmt.Sprintf("%s: %v", req.Kind, err)})
		return err
	}
	// gate says whether a missing reference makes the whole region unavailable:
	// not a reference of a profile (it concerns that placement only), and not a
	// per-zone pattern (the other zones of the region are fine).
	useRef := func(ref vendors.UseRef, gate bool, zone string) ([]vendors.ResourceRef, error) {
		refs, err := e.resolve(ctx, mr, vendor, req.Kind, region, ref)
		if err != nil {
			return nil, err
		}
		if len(refs) == 0 {
			err := missing("no %s in %s matches %s", req.Kind, region, describe(ref))
			if gate {
				return nil, fail(err)
			}
			if zone != "" {
				// Only this zone lacks the resource.
				e.setZoneMissing(nc.Name, vendor, region, zone, err.Error())
				return nil, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone, err)
			}
			return nil, err
		}
		return refs, nil
	}

	switch {
	case req.Use != nil:
		return useRef(*req.Use, false, "")
	case cfg.Use != nil:
		// {zone} is the zone of the launch; the name of a managed resource uses
		// only the zone the driver asks for.
		zone := req.Zone
		if zone == "" {
			zone = launchZone
		}
		ref, err := useRefFor(cfg.Use, region, zone)
		if err != nil {
			return nil, fail(missing("%s in %s: %v", req.Kind, region, err))
		}
		perZone := strings.Contains(cfg.Use.Name, "{zone}")
		if perZone {
			return useRef(ref, false, zone)
		}
		return useRef(ref, true, "")
	case cfg.Forbid:
		refs, err := e.resolve(ctx, mr, vendor, req.Kind, region, vendors.UseRef{})
		if err != nil {
			return nil, err
		}
		if len(refs) == 0 {
			return nil, fail(forbidden("creating %s is forbidden and %s has no default in %s", req.Kind, vendor, region))
		}
		return refs, nil
	case cfg.Create == nil && spec.Scope == vendors.ScopeRegion:
		// Neither use nor create: the vendor's own default resource, where it
		// exists, is used as found (00a section 9.2).
		refs, err := e.resolve(ctx, mr, vendor, req.Kind, region, vendors.UseRef{})
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			return refs, nil
		}
	}
	ref, err := e.ensure(ctx, nc, mr, vendor, region, spec, req)
	if err != nil {
		return nil, err
	}
	return []vendors.ResourceRef{ref}, nil
}

// ensure finds or creates the managed resource, serialised per (vendor, kind,
// region): the launches that need it wait for the first one's result.
func (e *Engine) ensure(
	ctx context.Context, nc *v1alpha1.SCNodeClass, mr vendors.ManagedResources, vendor, region string,
	spec vendors.KindSpec, req vendors.ResourceRequest,
) (vendors.ResourceRef, error) {
	// The create parameters are part of the identity: a changed strategy must
	// not adopt the group of the old one.
	key := req.Key
	if len(req.Params) > 0 {
		sum := sha256.Sum256(req.Params)
		key += "|" + hex.EncodeToString(sum[:])[:8]
	}
	name := ResourceName(e.opts.ClusterName, spec.Kind, region, req.Zone, key)
	mu := e.lock(vendor, spec.Kind, region)
	mu.Lock()
	defer mu.Unlock()

	ck := vendor + "/" + string(spec.Kind) + "/" + region + "/" + name
	now := e.opts.Clock.Now()
	e.mu.Lock()
	if c, ok := e.ensured[ck]; ok && now.Before(c.until) && len(c.refs) == 1 {
		ref := c.refs[0]
		e.touched[vendor+"/"+ref.ID] = now
		e.mu.Unlock()
		return ref, nil
	}
	e.mu.Unlock()

	tags := map[vendors.TagKey]string{
		vendors.TagCluster: e.opts.ClusterName, vendors.TagNodeClass: nc.Name,
		vendors.TagManagedBy: vendors.ManagedByValue, vendors.TagResource: string(spec.Kind),
	}
	ref, created, err := mr.Ensure(ctx, spec.Kind, region, vendors.CreateSpec{
		Name: name, Zone: req.Zone, Tags: tags, ExtraTags: nc.Spec.Tags, Params: req.Params,
	})
	if err != nil {
		e.metrics.operation(vendor, spec.Kind, region, opCreate, resultError)
		e.event(ctx, nc.Name, corev1.EventTypeWarning, EventCreateFailed,
			fmt.Sprintf("creating the %s %s in %s failed: %v", spec.Kind, name, region, err), name)
		return vendors.ResourceRef{}, err
	}
	ref.Managed = true
	e.mu.Lock()
	first := !e.seen[vendor+"/"+ref.ID]
	e.seen[vendor+"/"+ref.ID] = true
	e.ensured[ck] = cachedRefs{refs: []vendors.ResourceRef{ref}, until: now.Add(ensureTTL)}
	e.touched[vendor+"/"+ref.ID] = now
	e.mu.Unlock()
	switch {
	case created:
		e.metrics.operation(vendor, spec.Kind, region, opCreate, resultOK)
		e.event(ctx, nc.Name, corev1.EventTypeNormal, EventCreated,
			fmt.Sprintf("created the %s %s (%s) in %s", spec.Kind, name, ref.ID, region), name)
		log.FromContext(ctx).Info("created a managed resource", "vendor", vendor, "kind", spec.Kind, "region", region,
			"name", name, "id", ref.ID)
	case first:
		e.metrics.operation(vendor, spec.Kind, region, opAdopt, resultOK)
		e.event(ctx, nc.Name, corev1.EventTypeNormal, EventAdopted,
			fmt.Sprintf("adopted the existing %s %s (%s) in %s", spec.Kind, name, ref.ID, region), name)
		log.FromContext(ctx).Info("adopted a managed resource", "vendor", vendor, "kind", spec.Kind, "region", region,
			"name", name, "id", ref.ID)
	}
	return ref, nil
}

var (
	camel     = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	unsafeRun = regexp.MustCompile(`[^a-z0-9-]+`)
)

func slug(s string) string {
	return strings.Trim(unsafeRun.ReplaceAllString(strings.ToLower(camel.ReplaceAllString(s, "$1-$2")), "-"), "-")
}

// ResourceName is the deterministic name of a managed resource,
// scarpenter-<cluster>-<kind>-<region>, with a hash of the zone and key
// appended for resources that are not shared per region (00a section 9.2).
func ResourceName(cluster string, kind vendors.ResourceKind, region, zone, key string) string {
	name := NamePrefix(cluster) + slug(string(kind)) + "-" + slug(region)
	suffix := ""
	if zone != "" || key != "" {
		sum := sha256.Sum256([]byte(zone + "\x00" + key))
		suffix = "-" + hex.EncodeToString(sum[:])[:10]
	}
	if len(name)+len(suffix) > maxNameLength {
		// The strictest vendors (GCP) allow 63 characters: shorten the middle and
		// keep it unique with a hash of what was cut.
		sum := sha256.Sum256([]byte(name))
		keep := maxNameLength - len(suffix) - 9
		name = strings.TrimRight(name[:keep], "-") + "-" + hex.EncodeToString(sum[:])[:8]
	}
	return name + suffix
}

// maxNameLength is the longest name of a managed resource.
const maxNameLength = 63

// NamePrefix is what the name of every managed resource of the cluster starts
// with.
func NamePrefix(cluster string) string { return "scarpenter-" + clusterSlug(cluster) + "-" }

// maxClusterSlug keeps names within the 63 characters of the strictest vendors
// (GCP): a longer cluster name is truncated and a hash of the whole keeps it
// unique.
const maxClusterSlug = 20

func clusterSlug(cluster string) string {
	s := slug(cluster)
	if len(s) <= maxClusterSlug {
		return s
	}
	sum := sha256.Sum256([]byte(cluster))
	return strings.TrimRight(s[:maxClusterSlug-7], "-") + "-" + hex.EncodeToString(sum[:])[:6]
}

func (e *Engine) setRegion(class, vendor, region string, st RegionState) {
	k := regionKey{class, vendor, region}
	e.mu.Lock()
	old, had := e.regions[k]
	e.regions[k] = st
	e.mu.Unlock()
	if (!had || old.Ready != st.Ready || !maps.Equal(old.Zones, st.Zones)) && e.opts.Unavailable != nil {
		e.opts.Unavailable.Bump()
	}
}

// setZoneMissing marks one zone of a region unavailable for the class.
func (e *Engine) setZoneMissing(class, vendor, region, zone, reason string) {
	k := regionKey{class, vendor, region}
	e.mu.Lock()
	st := e.regions[k]
	if st.Zones[zone] == reason {
		e.mu.Unlock()
		return
	}
	zones := map[string]string{zone: reason}
	maps.Copy(zones, st.Zones)
	zones[zone] = reason
	if _, known := e.regions[k]; !known {
		st.Ready = true
	}
	st.Zones = zones
	e.regions[k] = st
	e.mu.Unlock()
	if e.opts.Unavailable != nil {
		e.opts.Unavailable.Bump()
	}
}

// SetRegionState records whether the resources the class needs in the region
// are ready (the status controller's verdict).
func (e *Engine) SetRegionState(class, vendor, region string, st RegionState) {
	e.setRegion(class, vendor, region, st)
}

// RegionState returns the recorded state; ok is false before the first check.
func (e *Engine) RegionState(class, vendor, region string) (RegionState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.regions[regionKey{class, vendor, region}]
	return st, ok
}

// UnavailableFor implements catalog.ClassGate: the offerings of a region whose
// required resources are missing or forbidden are unavailable.
func (e *Engine) UnavailableFor(class, vendor, _, region, zone, _ string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.regions[regionKey{class, vendor, region}]
	if !ok {
		return false
	}
	_, zoneMissing := st.Zones[zone]
	return !st.Ready || zoneMissing
}

// ForgetClass drops what the engine knows about a deleted NodeClass.
func (e *Engine) ForgetClass(class string) {
	e.mu.Lock()
	changed := false
	for k := range e.regions {
		if k.class == class {
			delete(e.regions, k)
			changed = true
		}
	}
	e.mu.Unlock()
	if changed && e.opts.Unavailable != nil {
		e.opts.Unavailable.Bump()
	}
}

func (e *Engine) event(ctx context.Context, class, typ, reason, msg string, dedupe ...string) {
	if e.opts.Recorder == nil {
		return
	}
	evt := events.Event{
		Type: typ, Reason: reason, Message: msg,
		DedupeValues: append([]string{reason, class}, dedupe...), DedupeTimeout: eventDedupeTimeout,
	}
	if class != "" && e.opts.Reader != nil {
		nc := &v1alpha1.SCNodeClass{}
		if err := e.opts.Reader.Get(ctx, types.NamespacedName{Name: class}, nc); err == nil {
			evt.InvolvedObject = nc
			e.opts.Recorder.Publish(evt)
			return
		}
	}
	if e.opts.DeploymentRef != nil {
		if ref := e.opts.DeploymentRef(); ref != nil {
			evt.InvolvedObject = ref
			e.opts.Recorder.Publish(evt)
		}
	}
}
