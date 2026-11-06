/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	selapi "github.com/SpareCores/sc-keeper-go/selection/api"

	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
)

// Defaults of the refresh loop (overview section 5.2).
const (
	DefaultRefreshInterval = time.Hour
	DefaultMaxAge          = 24 * time.Hour
	// DefaultJitter is the share of the interval a refresh is moved by, up or down.
	DefaultJitter = 0.1
	// retryInterval is the longest wait after a failed refresh.
	retryInterval = time.Minute
	// maxCachedResults bounds the cache of query results of one generation.
	maxCachedResults = 64
)

// Options configures a Catalog.
type Options struct {
	Source Source
	// Facts are the driver facts; nil means StaticFacts{}: no zones, no root
	// disk, no profiles.
	Facts Facts
	// Unavailable is the cache of failed offerings; nil means an empty one.
	Unavailable *unavailable.Cache
	// RefreshInterval and MaxAge default to one hour and 24 hours.
	RefreshInterval time.Duration
	MaxAge          time.Duration
	// Jitter is the share of the refresh interval the next refresh moves by
	// (default 0.1).
	Jitter float64
	// Clock defaults to the real one.
	Clock clock.Clock
	// Registerer receives the catalog metrics; nil registers none.
	Registerer prometheus.Registerer
}

// Catalog holds the last good snapshot of the Keeper data for the union of the
// scopes of all SCNodeClasses and turns it into instance types (overview
// section 5). It is safe for concurrent use.
type Catalog struct {
	opts Options

	refreshMu sync.Mutex // serialises Refresh

	mu          sync.RWMutex
	classes     map[string]*Class
	snap        *snapshot
	lastErr     error
	lastErrAt   time.Time
	nextSnapID  uint64
	trigger     chan struct{}
	results     resultCache
	refreshErrs prometheus.Counter
}

// New returns a catalog without data: Refresh (or Start) reads it.
func New(opts Options) *Catalog {
	if opts.Facts == nil {
		opts.Facts = StaticFacts{}
	}
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.Unavailable == nil {
		opts.Unavailable = unavailable.New(opts.Clock, unavailable.TTLs{})
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.Jitter <= 0 {
		opts.Jitter = DefaultJitter
	}
	c := &Catalog{
		opts: opts, classes: map[string]*Class{}, trigger: make(chan struct{}, 1),
		refreshErrs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "scarpenter_catalog_refresh_errors_total",
			Help: "Failed catalog refreshes (the last good snapshot stays in use).",
		}),
	}
	if opts.Registerer != nil {
		for _, col := range []prometheus.Collector{c.refreshErrs, &collector{c: c}} {
			register(opts.Registerer, col)
		}
	}
	return c
}

// register adds the collector; a collector of an earlier catalog (a test that
// builds several, a restart of the component) is replaced.
func register(reg prometheus.Registerer, col prometheus.Collector) {
	err := reg.Register(col)
	if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
		reg.Unregister(already.ExistingCollector)
		err = reg.Register(col)
	}
	if err != nil {
		// A programming error (conflicting descriptors), caught by every test.
		panic(fmt.Sprintf("registering the catalog metrics: %v", err))
	}
}

// SetClasses replaces the NodeClasses the catalog serves. When the scope (the
// vendors and regions) or a metric or selection query changes, a refresh is
// triggered; Start performs it immediately.
func (c *Catalog) SetClasses(classes []Class) {
	m := make(map[string]*Class, len(classes))
	for i := range classes {
		cl := classes[i]
		m[cl.Name] = &cl
	}
	c.mu.Lock()
	c.classes = m
	snap := c.snap
	c.mu.Unlock()
	if snap != nil {
		snap.prune(classes)
	}
	_, key := buildRequest(classes)
	if snap == nil || snap.requestKey != key {
		select {
		case c.trigger <- struct{}{}:
		default:
		}
	}
}

// buildRequest derives what a refresh reads from the classes, and a key that
// identifies it.
func buildRequest(classes []Class) (Request, string) {
	req := Request{Scope: UnionScope(classes)}
	seenMetric, seenSel := map[string]bool{}, map[string]bool{}
	for i := range classes {
		cl := &classes[i]
		for _, vendor := range cl.VendorNames() {
			for _, name := range cl.MetricNames() {
				m := cl.Metrics[name]
				q := metricQuery(vendor, &metricDef{benchmark: m.Benchmark, profile: m.WorkloadProfile})
				if !seenMetric[q.ID] {
					seenMetric[q.ID] = true
					req.Metrics = append(req.Metrics, q)
				}
			}
			for _, name := range slices.Sorted(maps.Keys(cl.Selections)) {
				sel := cl.Selections[name]
				spec := selapi.Spec{Selector: sel.Selector, Ranking: sel.Ranking}
				id := SelectionQueryID(vendor, cl.Vendors[vendor], &spec)
				if !seenSel[id] {
					seenSel[id] = true
					req.Selections = append(req.Selections, SelectionQuery{
						ID: id, Vendor: vendor, Regions: slices.Clone(cl.Vendors[vendor]), Spec: spec,
					})
				}
			}
		}
	}
	slices.SortFunc(req.Metrics, func(a, b MetricQuery) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(req.Selections, func(a, b SelectionQuery) int { return strings.Compare(a.ID, b.ID) })

	ids := make([]string, 0, len(req.Metrics)+len(req.Selections))
	for _, q := range req.Metrics {
		ids = append(ids, q.ID)
	}
	for _, q := range req.Selections {
		ids = append(ids, q.ID)
	}
	raw, err := json.Marshal(struct {
		Scope Scope
		IDs   []string
	}{req.Scope, ids})
	if err != nil {
		panic(fmt.Sprintf("hashing the catalog request: %v", err))
	}
	sum := sha256.Sum256(raw)
	return req, hex.EncodeToString(sum[:])[:16]
}

// Refresh reads the source once for the current classes. On failure the last
// good snapshot stays in use and the error is returned (and counted).
func (c *Catalog) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	classes := make([]Class, 0, len(c.classes))
	for _, cl := range c.classes {
		classes = append(classes, *cl)
	}
	c.mu.RUnlock()
	slices.SortFunc(classes, func(a, b Class) int { return strings.Compare(a.Name, b.Name) })

	req, key := buildRequest(classes)
	if len(req.Scope) == 0 {
		// Nothing to read: no NodeClass yet. Drop any old snapshot, so that the
		// age and the counts do not describe data nobody asked for.
		c.mu.Lock()
		c.snap = nil
		c.mu.Unlock()
		return nil
	}
	data, err := c.opts.Source.Load(ctx, req)
	if err != nil {
		c.refreshErrs.Inc()
		c.mu.Lock()
		c.lastErr, c.lastErrAt = err, c.opts.Clock.Now()
		c.mu.Unlock()
		return fmt.Errorf("refreshing the catalog: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextSnapID++
	c.snap = newSnapshot(c.nextSnapID, data, req.Scope, key, c.opts.Clock.Now())
	c.lastErr = nil
	return nil
}

// Start refreshes the catalog until the context ends: at the start, every
// refresh interval (moved by the jitter), after a failure sooner (at most a
// minute), and whenever SetClasses changes the scope. It implements
// controller-runtime's Runnable.
func (c *Catalog) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("catalog")
	// A scope change before the start is covered by the first refresh.
	select {
	case <-c.trigger:
	default:
	}
	for {
		start := time.Now()
		if err := c.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Error(err, "catalog refresh failed, keeping the last good snapshot")
		} else {
			logger.V(1).Info("catalog refreshed", "duration", time.Since(start).String())
		}
		wait := c.nextWait()
		select {
		case <-ctx.Done():
			return nil
		case <-c.trigger:
		case <-c.opts.Clock.After(wait):
		}
	}
}

// nextWait is the interval until the next refresh.
func (c *Catalog) nextWait() time.Duration {
	d := float64(c.opts.RefreshInterval)
	d *= 1 + c.opts.Jitter*(2*rand.Float64()-1) //nolint:gosec // jitter, not security
	wait := time.Duration(d)
	c.mu.RLock()
	failed := c.lastErr != nil
	c.mu.RUnlock()
	if failed {
		wait = min(wait, retryInterval)
	}
	return wait
}

// NeedLeaderElection says that the catalog runs on the leader only.
func (c *Catalog) NeedLeaderElection() bool { return true }

// Status is the state of the catalog for one NodeClass.
type Status struct {
	// Ready is true when a snapshot covers the class's scope and is not older
	// than the maximum age (overview section 5.2).
	Ready bool
	// Reason is a CamelCase reason when not ready: NoSnapshot, ScopePending,
	// Stale or UnknownClass.
	Reason  string
	Message string
	// Age is the age of the snapshot (zero without one).
	Age time.Duration
	// LastError is the last refresh error, nil after a success.
	LastError error
}

// Status returns the catalog state for the NodeClass. A failed refresh alone
// does not make a class not ready: the last good snapshot is used until it is
// older than the maximum age.
func (c *Catalog) Status(className string) Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	class, ok := c.classes[className]
	st := Status{LastError: c.lastErr}
	if !ok {
		st.Reason, st.Message = "UnknownClass", "the catalog does not know the SCNodeClass"
		return st
	}
	if c.snap == nil {
		st.Reason, st.Message = "NoSnapshot", "the catalog has not been read from Keeper yet"
		if c.lastErr != nil {
			st.Message += ": " + c.lastErr.Error()
		}
		return st
	}
	st.Age = c.opts.Clock.Since(c.snap.loadedAt)
	for vendor, regions := range class.Vendors {
		if !scopeCovers(c.snap.scope, vendor, regions) {
			st.Reason = "ScopePending"
			st.Message = "the catalog has not been refreshed for the vendors and regions of the SCNodeClass yet"
			return st
		}
	}
	if st.Age > c.opts.MaxAge {
		st.Reason = "Stale"
		st.Message = fmt.Sprintf("the catalog is %s old, older than the maximum age %s",
			st.Age.Round(time.Second), c.opts.MaxAge)
		if c.lastErr != nil {
			st.Message += ": " + c.lastErr.Error()
		}
		return st
	}
	st.Ready = true
	return st
}

func scopeCovers(scope Scope, vendor string, regions []string) bool {
	have, ok := scope[vendor]
	if !ok {
		return false
	}
	if len(have) == 0 {
		return true
	}
	if len(regions) == 0 {
		return false
	}
	return !slices.ContainsFunc(regions, func(r string) bool { return !slices.Contains(have, r) })
}

// RestartRequired returns the label keys of the class's metrics that were not
// registered with Karpenter core at start-up; non-empty means the class needs a
// controller restart before those labels are selectable (00a section 7.2).
func (c *Catalog) RestartRequired(class *Class) []string {
	return labels.MissingKeys(class.MetricNames())
}

// Invalid returns the metrics and selection profiles of the class that the
// data source could not evaluate (an unknown benchmark config, an invalid
// selector), with the reason. Their labels are DoesNotExist; the NodeClass
// controller reports them.
func (c *Catalog) Invalid(class *Class) (metrics, selections map[string]string) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()
	if snap == nil {
		return nil, nil
	}
	v := snap.view(class)
	return maps.Clone(v.invalidMetrics), maps.Clone(v.invalidSelections)
}

// Version returns the data version of the current snapshot ("" without one).
func (c *Catalog) Version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.snap == nil {
		return ""
	}
	return c.snap.data.Version
}

// Ref names the server behind an instance type.
type Ref struct {
	Vendor       string
	APIReference string
}

// Lookup resolves an instance type name of the current snapshot to its vendor
// and raw API reference (also when the reference does not fit a label).
func (c *Catalog) Lookup(instanceType string) (Ref, bool) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()
	if snap == nil {
		return Ref{}, false
	}
	vendor, rest, ok := strings.Cut(instanceType, ".")
	if !ok {
		return Ref{}, false
	}
	// Names that were truncated or sanitised do not contain the raw reference:
	// search the vendor's servers.
	vd := snap.vendors[vendor]
	if vd == nil {
		return Ref{}, false
	}
	if srv, ok := vd.byAPIRef[rest]; ok && InstanceTypeName(vendor, srv.APIReference) == instanceType {
		return Ref{Vendor: vendor, APIReference: srv.APIReference}, true
	}
	for _, srv := range vd.servers {
		if InstanceTypeName(vendor, srv.APIReference) == instanceType {
			return Ref{Vendor: vendor, APIReference: srv.APIReference}, true
		}
	}
	return Ref{}, false
}

// Query asks for the instance types of one NodeClass.
type Query struct {
	// Class is the SCNodeClass name.
	Class string
	// Filter is the pre-filter from the NodePool's requirements. The scope of a
	// query is the vendors the filter allows that the class configures, in the
	// class's regions.
	Filter Filter
}

// InstanceTypes returns the instance types for the NodeClass, pre-filtered
// (overview section 4). Without a snapshot, or for an unknown class, it returns
// nothing: pods stay pending with a clear reason instead of nodes being launched
// blind. The result is cached by snapshot, class, scope, filter and the
// sequence of the unavailable-offerings cache; callers must not modify it.
func (c *Catalog) InstanceTypes(_ context.Context, q Query) ([]*cloudprovider.InstanceType, error) {
	c.mu.RLock()
	snap := c.snap
	class := c.classes[q.Class]
	c.mu.RUnlock()
	if snap == nil || class == nil {
		return nil, nil
	}
	var vendors []string
	for _, v := range class.VendorNames() {
		if q.Filter.allowsVendor(v) {
			vendors = append(vendors, v)
		}
	}
	var pin *pricedBy
	if name := q.Filter.selection; name != "" {
		if sel, ok := snap.view(class).selection[name]; ok && sel.objective != selapi.ObjectivePrice {
			pin = &pricedBy{name: name, sel: sel}
		}
	}

	gen := strconv.FormatUint(snap.id, 10) + "/" + strconv.FormatUint(c.opts.Facts.Epoch(), 10) + "/" +
		strconv.FormatUint(c.opts.Unavailable.Sequence(), 10)
	key := class.Hash + "|" + strings.Join(vendors, ",") + "|" + q.Filter.key()
	if its, ok := c.cached(gen, key); ok {
		return its, nil
	}

	var out []*cloudprovider.InstanceType
	for _, vendor := range vendors {
		vb := c.vendorBuild(snap, class, vendor)
		for _, bt := range vb.types {
			if it := c.apply(bt, vendor, q.Filter, pin); it != nil {
				out = append(out, it)
			}
		}
	}
	c.store(gen, key, out)
	return out, nil
}

// apply pre-filters a type's offerings, marks the unavailable ones and reprices
// by a pinned selection objective. It returns the shared type when nothing
// changes, a copy otherwise, and nil when the filter leaves nothing.
func (c *Catalog) apply(bt *builtType, vendor string, f Filter, pin *pricedBy) *cloudprovider.InstanceType {
	if !f.allowsType(bt.it.Name, bt.server.Arch) {
		return nil
	}
	keep := make([]int, 0, len(bt.offerings))
	changed := false
	var avail []bool
	var prices []float64
	for i, o := range bt.offerings {
		if !f.allowsOffering(o.region, o.zone, o.capacityType) {
			changed = true
			continue
		}
		keep = append(keep, i)
		down := c.opts.Unavailable.IsUnavailable(unavailable.Offering{
			Vendor: vendor, InstanceType: bt.it.Name, Region: o.region, Zone: o.zone, CapacityType: o.capacityType,
		})
		price := o.usd
		if pin != nil {
			p, ok := pin.effectivePrice(bt.server.Key(), o.usd)
			if !ok {
				// Not rankable by the objective: it cannot be priced by it.
				down = true
			} else {
				price = p
			}
		}
		avail = append(avail, !down)
		prices = append(prices, price)
		changed = changed || down || price != o.usd
	}
	if len(keep) == 0 {
		return nil
	}
	if !changed {
		return bt.it
	}
	it := &cloudprovider.InstanceType{
		Name: bt.it.Name, Requirements: bt.it.Requirements, Capacity: bt.it.Capacity, Overhead: bt.it.Overhead,
		Offerings: make(cloudprovider.Offerings, len(keep)),
	}
	for j, i := range keep {
		src := bt.it.Offerings[i]
		it.Offerings[j] = &cloudprovider.Offering{Requirements: src.Requirements, Price: prices[j], Available: avail[j]}
	}
	return it
}

// resultCache keeps the results of one generation (snapshot, facts epoch,
// unavailable sequence); a new generation drops them all.
type resultCache struct {
	mu  sync.Mutex
	gen string
	m   map[string][]*cloudprovider.InstanceType
}

func (c *Catalog) cached(gen, key string) ([]*cloudprovider.InstanceType, bool) {
	rc := &c.results
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.gen != gen {
		return nil, false
	}
	its, ok := rc.m[key]
	return its, ok
}

func (c *Catalog) store(gen, key string, its []*cloudprovider.InstanceType) {
	rc := &c.results
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.gen != gen || rc.m == nil {
		rc.gen, rc.m = gen, map[string][]*cloudprovider.InstanceType{}
	}
	if len(rc.m) >= maxCachedResults {
		rc.m = map[string][]*cloudprovider.InstanceType{}
	}
	rc.m[key] = its
}

// Counts returns the number of instance types and offerings per vendor of the
// snapshot, before the driver's zone mapping (the scheduling-level numbers come
// from InstanceTypes).
func (c *Catalog) counts() map[string][2]int {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()
	if snap == nil {
		return nil
	}
	out := make(map[string][2]int, len(snap.vendors))
	for vendor, vd := range snap.vendors {
		out[vendor] = [2]int{vd.nTypes, vd.nPrices}
	}
	return out
}

// collector exposes the age and size of the snapshot.
type collector struct{ c *Catalog }

var (
	vendorLabel = []string{"vendor"}
	ageDesc     = prometheus.NewDesc("scarpenter_catalog_age_seconds",
		"Seconds since the catalog snapshot was read from Keeper.", vendorLabel, nil)
	typesDesc = prometheus.NewDesc("scarpenter_catalog_instance_types",
		"Instance types of the catalog snapshot with at least one price in scope.", vendorLabel, nil)
	offeringsDesc = prometheus.NewDesc("scarpenter_catalog_offerings",
		"Priced offerings of the catalog snapshot (price rows in scope, before the driver's zone mapping).",
		vendorLabel, nil)
)

// Describe implements prometheus.Collector.
func (k *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- ageDesc
	ch <- typesDesc
	ch <- offeringsDesc
}

// Collect implements prometheus.Collector.
func (k *collector) Collect(ch chan<- prometheus.Metric) {
	k.c.mu.RLock()
	snap := k.c.snap
	k.c.mu.RUnlock()
	if snap == nil {
		return
	}
	age := k.c.opts.Clock.Since(snap.loadedAt).Seconds()
	for vendor, n := range k.c.counts() {
		ch <- prometheus.MustNewConstMetric(ageDesc, prometheus.GaugeValue, age, vendor)
		ch <- prometheus.MustNewConstMetric(typesDesc, prometheus.GaugeValue, float64(n[0]), vendor)
		ch <- prometheus.MustNewConstMetric(offeringsDesc, prometheus.GaugeValue, float64(n[1]), vendor)
	}
}
