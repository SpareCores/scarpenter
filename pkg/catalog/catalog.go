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
	// Gate marks more offerings unavailable (the quota tracker); nil means none.
	// Its answer may change at any time: the owner calls Unavailable.Bump when
	// it does, which drops the cached results.
	Gate Gate
	// ClassGate is a Gate whose answer depends on the NodeClass, for example
	// the vendor resources a NodeClass brings or forbids (plan 06); nil means
	// none. Like Gate, its owner calls Unavailable.Bump when an answer changes.
	ClassGate ClassGate
	// Clock defaults to the real one.
	Clock clock.Clock
	// EmbeddedVendors are the vendors served from embedded data (EmbeddedSource):
	// they are exported as the gauge scarpenter_catalog_embedded_vendor.
	EmbeddedVendors []string
	// Registerer receives the catalog metrics; nil registers none.
	Registerer prometheus.Registerer
}

// Gate says whether an offering cannot be launched for a reason the catalog
// does not know, for example because the account has no quota left for it
// (overview section 5.9). It is asked for every offering of a query, so it must
// be cheap.
type Gate interface {
	Unavailable(vendor, apiReference, region, zone, capacityType string) bool
}

// ClassGate is a Gate for answers that differ between NodeClasses.
type ClassGate interface {
	UnavailableFor(class, vendor, apiReference, region, zone, capacityType string) bool
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
	auth        authState
}

// authState is what the refreshes say about Keeper's API key: unknown until a
// refresh read a snapshot or was refused.
type authState struct {
	known         bool
	authorized    bool
	keyConfigured bool
}

// keeperKeyDocs is where a Keeper API key is requested.
const keeperKeyDocs = "https://docs.sparecores.com/api/#authentication"

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

	return req, requestKey(req)
}

// requestKey identifies what a request reads: its scope and its queries (the
// IDs say what they ask for), in the order given.
func requestKey(req Request) string {
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
	return hex.EncodeToString(sum[:])[:16]
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
		ae, unauthorized := errors.AsType[*KeeperAuthError](err)
		c.mu.Lock()
		c.lastErr, c.lastErrAt = err, c.opts.Clock.Now()
		if unauthorized {
			c.auth = authState{known: true, keyConfigured: ae.KeyConfigured}
		}
		c.mu.Unlock()
		if unauthorized {
			log.FromContext(ctx).WithName("catalog").Error(err, "Keeper needs an API key",
				"keeperURL", ae.URL, "keyConfigured", ae.KeyConfigured,
				"docs", `docs section "Keeper API key" (how-to-guides/install.md); request a key at `+keeperKeyDocs)
		}
		return fmt.Errorf("refreshing the catalog: %w", err)
	}
	now := c.opts.Clock.Now()
	c.mu.Lock()
	if c.snap != nil && c.snap.data == data && c.snap.requestKey == key {
		// The source still has the data of the current snapshot: it is as fresh
		// as Keeper's, and nothing is rebuilt.
		c.snap.loadedAt = now
		c.lastErr = nil
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	if sink, ok := c.opts.Facts.(PriceSink); ok && data.AuxPricesRead {
		// Before the data is published, so that its offerings never meet the
		// prices of another version.
		sink.SetKeeperPrices(data.Version, data.StoragePrices, data.IPv4Prices)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextSnapID++
	c.snap = newSnapshot(c.nextSnapID, data, req.Scope, key, now)
	c.lastErr = nil
	if data.SnapshotRead {
		c.auth = authState{known: true, authorized: true, keyConfigured: c.auth.keyConfigured}
	}
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
			if _, unauthorized := errors.AsType[*KeeperAuthError](err); !unauthorized {
				// An unauthorized refresh logged its own, single line.
				logger.Error(err, "catalog refresh failed, keeping the last good snapshot")
			}
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
	// Reason is a CamelCase reason when not ready: NoSnapshot, KeeperAPIKeyRequired,
	// ScopePending, Stale or UnknownClass. Message names the data version when
	// ready.
	Reason  string
	Message string
	// Age is the age of the snapshot (zero without one).
	Age time.Duration
	// LastError is the last refresh error, nil after a success.
	LastError error
	// KeeperAuth says whether Keeper accepted the catalog's requests; it is
	// informational and not part of Ready.
	KeeperAuth KeeperAuth
}

// KeeperAuth is the state of the catalog's access to Keeper (the condition
// KeeperAuthorized). Known is false until a refresh read a snapshot or was
// refused with a 401.
type KeeperAuth struct {
	Known      bool
	Authorized bool
	// Reason and Message say why not, when Known and not Authorized.
	Reason  string
	Message string
}

// Reasons of an unauthorized KeeperAuth, and of CatalogReady when no catalog
// exists because of it.
const (
	ReasonAPIKeyMissing  = "APIKeyMissing"
	ReasonAPIKeyRejected = "APIKeyRejected"
	ReasonKeyRequired    = "KeeperAPIKeyRequired"
)

func (a authState) status() KeeperAuth {
	switch {
	case !a.known:
		return KeeperAuth{}
	case a.authorized:
		return KeeperAuth{Known: true, Authorized: true}
	case a.keyConfigured:
		return KeeperAuth{Known: true, Reason: ReasonAPIKeyRejected, Message: "Keeper rejected the API key: replace the " +
			"key in the Secret of keeper.apiKey.existingSecret (flag --keeper-api-key-file); see the documentation " +
			"section \"Keeper API key\" (request a key at " + keeperKeyDocs + ")"}
	}
	return KeeperAuth{Known: true, Reason: ReasonAPIKeyMissing, Message: "Keeper needs an API key, none is configured: " +
		"set keeper.apiKey.existingSecret (flag --keeper-api-key-file); see the documentation section \"Keeper API key\" " +
		"(request a key at " + keeperKeyDocs + ")"}
}

// Status returns the catalog state for the NodeClass. A failed refresh alone
// does not make a class not ready: the last good snapshot is used until it is
// older than the maximum age.
func (c *Catalog) Status(className string) Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	class, ok := c.classes[className]
	st := Status{LastError: c.lastErr, KeeperAuth: c.auth.status()}
	if !ok {
		st.Reason, st.Message = "UnknownClass", "the catalog does not know the SCNodeClass"
		return st
	}
	if c.snap == nil {
		st.Reason, st.Message = "NoSnapshot", "the catalog has not been read from Keeper yet"
		if st.KeeperAuth.Known && !st.KeeperAuth.Authorized {
			st.Reason, st.Message = ReasonKeyRequired, st.KeeperAuth.Message
		} else if c.lastErr != nil {
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
	if d := c.snap.data; d.SnapshotRead && d.Version != "" {
		st.Message = "Keeper data version " + d.Version
		if !d.PublishedAt.IsZero() {
			st.Message += ", published " + d.PublishedAt.UTC().Format(time.RFC3339)
		}
	}
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

// Type returns one instance type of the class by catalog name, without the
// pre-filter, the availability marks or the result cache: it is for resolving
// the capacity, labels and prices of an instance that already exists. The
// result is shared; callers must not modify it.
func (c *Catalog) Type(className, name string) (*cloudprovider.InstanceType, bool) {
	c.mu.RLock()
	snap := c.snap
	class := c.classes[className]
	c.mu.RUnlock()
	if snap == nil || class == nil {
		return nil, false
	}
	vendor, _, ok := strings.Cut(name, ".")
	if !ok || !classHasVendor(class, vendor) {
		return nil, false
	}
	if bt := c.vendorBuild(snap, class, vendor).byName[name]; bt != nil {
		return bt.it, true
	}
	return nil, false
}

// OfferingPrice is the price of an offering as the catalog computed it.
type OfferingPrice struct {
	// Base is the instance's price from Keeper, Delta the price of the
	// NodeClass's default settings on top of it (00a section 10): disks and
	// public IPv4. USD is their sum, the price of the offering without a
	// selected profile.
	Base, Delta, USD float64
	// Complete is false when the price misses components the data does not
	// have.
	Complete bool
}

// Price returns the real USD price of one offering of a type of the class
// (independent of a pinned selection objective), and whether the offering
// exists. It serves what Create records on the NodeClaim.
func (c *Catalog) Price(className, name, region, zone, capacityType string) (OfferingPrice, bool) {
	c.mu.RLock()
	snap := c.snap
	class := c.classes[className]
	c.mu.RUnlock()
	if snap == nil || class == nil {
		return OfferingPrice{}, false
	}
	vendor, _, ok := strings.Cut(name, ".")
	if !ok || !classHasVendor(class, vendor) {
		return OfferingPrice{}, false
	}
	vb := c.vendorBuild(snap, class, vendor)
	bt := vb.byName[name]
	if bt == nil {
		return OfferingPrice{}, false
	}
	for _, o := range bt.offerings {
		if o.region == region && o.zone == zone && o.capacityType == capacityType {
			return OfferingPrice{Base: o.base, Delta: o.delta, USD: o.usd, Complete: o.complete}, true
		}
	}
	return OfferingPrice{}, false
}

func classHasVendor(class *Class, vendor string) bool {
	_, ok := class.Vendors[vendor]
	return ok
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
	if c.opts.ClassGate != nil {
		// Availability then depends on the class's name, which the hash leaves out.
		key = q.Class + "|" + key
	}
	if its, ok := c.cached(gen, key); ok {
		return its, nil
	}

	var out []*cloudprovider.InstanceType
	for _, vendor := range vendors {
		vb := c.vendorBuild(snap, class, vendor)
		for _, bt := range vb.types {
			if it := c.apply(bt, vendor, q.Class, q.Filter, pin); it != nil {
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
func (c *Catalog) apply(bt *builtType, vendor, class string, f Filter, pin *pricedBy) *cloudprovider.InstanceType {
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
		}) || (c.opts.Gate != nil &&
			c.opts.Gate.Unavailable(vendor, bt.server.APIReference, o.region, o.zone, o.capacityType)) ||
			(c.opts.ClassGate != nil &&
				c.opts.ClassGate.UnavailableFor(class, vendor, bt.server.APIReference, o.region, o.zone, o.capacityType))
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
	if rc.gen != gen && rc.gen != "" && genOlder(gen, rc.gen) {
		// A slow query of an older generation must not reset a newer one.
		return
	}
	if rc.gen != gen || rc.m == nil {
		rc.gen, rc.m = gen, map[string][]*cloudprovider.InstanceType{}
	}
	if len(rc.m) >= maxCachedResults {
		rc.m = map[string][]*cloudprovider.InstanceType{}
	}
	rc.m[key] = its
}

// genOlder reports whether generation a is older than b. Generations are
// "<snapshot>/<epoch>/<sequence>" of monotonically growing numbers.
func genOlder(a, b string) bool {
	pa, pb := strings.Split(a, "/"), strings.Split(b, "/")
	if len(pa) != len(pb) {
		return false
	}
	for i := range pa {
		x, errX := strconv.ParseUint(pa[i], 10, 64)
		y, errY := strconv.ParseUint(pb[i], 10, 64)
		if errX != nil || errY != nil || x == y {
			continue
		}
		return x < y
	}
	return false
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
	publishedDesc = prometheus.NewDesc("scarpenter_catalog_data_published_timestamp_seconds",
		"Unix time Keeper published the data version of the catalog snapshot (SC-Data-Published-At).", nil, nil)
	embeddedDesc = prometheus.NewDesc("scarpenter_catalog_embedded_vendor",
		"1 for each vendor served from the catalog embedded in the binary instead of Keeper (static prices and sizes).",
		vendorLabel, nil)
	unauthorizedDesc = prometheus.NewDesc("scarpenter_catalog_keeper_unauthorized",
		"1 when Keeper answered the last refresh with 401 (no key, or a rejected one), 0 after a successful read.",
		nil, nil)
)

// Describe implements prometheus.Collector.
func (k *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- ageDesc
	ch <- typesDesc
	ch <- offeringsDesc
	ch <- publishedDesc
	ch <- unauthorizedDesc
	ch <- embeddedDesc
}

// Collect implements prometheus.Collector.
func (k *collector) Collect(ch chan<- prometheus.Metric) {
	for _, vendor := range k.c.opts.EmbeddedVendors {
		ch <- prometheus.MustNewConstMetric(embeddedDesc, prometheus.GaugeValue, 1, vendor)
	}
	k.c.mu.RLock()
	snap, auth := k.c.snap, k.c.auth
	var age float64
	var published time.Time
	if snap != nil {
		age = k.c.opts.Clock.Since(snap.loadedAt).Seconds()
		published = snap.data.PublishedAt
	}
	k.c.mu.RUnlock()
	if auth.known {
		v := 0.0
		if !auth.authorized {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(unauthorizedDesc, prometheus.GaugeValue, v)
	}
	if snap == nil {
		return
	}
	if !published.IsZero() {
		ch <- prometheus.MustNewConstMetric(publishedDesc, prometheus.GaugeValue, float64(published.Unix()))
	}
	for vendor, n := range k.c.counts() {
		ch <- prometheus.MustNewConstMetric(ageDesc, prometheus.GaugeValue, age, vendor)
		ch <- prometheus.MustNewConstMetric(typesDesc, prometheus.GaugeValue, float64(n[0]), vendor)
		ch <- prometheus.MustNewConstMetric(offeringsDesc, prometheus.GaugeValue, float64(n[1]), vendor)
	}
}
