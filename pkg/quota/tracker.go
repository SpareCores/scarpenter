/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package quota is the quota tracker of overview section 5.9 (ADR-013). It
// reads the buckets (limit and usage of the whole account) of drivers with the
// QuotaReporter capability and the launchable types of drivers with the
// OfferingLister capability, keeps an in-flight ledger of launches that usage
// does not show yet, and answers whether an offering has headroom. Where a
// vendor or a type has no quota knowledge, the answer is "unknown" and nothing is
// blocked: the reactive path of the unavailable-offerings cache applies.
//
// The tracker has no Kubernetes client and no catalog; it is clock-injectable
// and its work is driven by Tick, which Start calls once a second.
package quota

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Defaults of the tracker (overview sections 5.9 and 12).
const (
	DefaultRefreshInterval    = 2 * time.Minute
	DefaultLimitsInterval     = 15 * time.Minute
	DefaultMaxStaleness       = 15 * time.Minute
	DefaultLaunchableInterval = time.Hour
	DefaultDebounce           = 10 * time.Second
	DefaultReservationTTL     = 10 * time.Minute
	DefaultJitter             = 0.1
	// tickInterval is how often Start looks for due work.
	tickInterval = time.Second
	labelRegion  = "region"
	labelVendor  = "vendor"
	// maxParallel bounds the concurrent driver calls of one tick.
	maxParallel = 8
)

// Options configures a Tracker.
type Options struct {
	Registry *vendors.Registry
	Clock    clock.Clock
	// Unavailable's sequence is bumped whenever the view of the tracker changes,
	// so that the catalog's cached results are dropped.
	Unavailable *unavailable.Cache
	// Scope returns the concrete regions in scope per vendor.
	Scope func() map[string][]string
	// RefreshInterval is the period of usage refreshes, LimitsInterval of
	// limit refreshes, MaxStaleness the age after which a region without a
	// successful refresh counts as unknown.
	RefreshInterval time.Duration
	LimitsInterval  time.Duration
	MaxStaleness    time.Duration
	// LaunchableInterval is the period of the launchable-types refresh.
	LaunchableInterval time.Duration
	// Debounce is the minimum distance of triggered refreshes of a region.
	Debounce time.Duration
	// ReservationTTL is how long an in-flight reservation lives at most.
	ReservationTTL time.Duration
	// Jitter is the share of an interval a refresh is moved by.
	Jitter     float64
	Registerer prometheus.Registerer
}

type regionKey struct{ vendor, region string }

// state is the last quota view of a region (or of the account, region "").
type state struct {
	snap        *vendors.QuotaSnapshot
	okAt        time.Time // last successful refresh
	attemptAt   time.Time // last refresh attempt
	limitsAt    time.Time // last successful refresh with limits
	nextDue     time.Time
	triggerAt   time.Time // a debounced refresh is due at this time
	stale       bool
	lastErr     error
	refreshing  bool
	launchAt    time.Time
	launchNext  time.Time
	launchTypes map[string][]string
}

type bucketKey struct {
	vendor, region, bucket string // region "" for a global bucket
}

type item struct {
	key    bucketKey
	amount int64
}

// Reservation is an in-flight claim on the buckets an instance will consume.
type Reservation struct {
	id        uint64
	vendor    string
	region    string
	owner     string
	items     []item
	createdAt time.Time
	ref       vendors.InstanceRef
}

// Decision is the answer of Reserve.
type Decision struct {
	// Known is false when the quota of the type is unknown: nothing is blocked
	// and nothing reserved.
	Known   bool
	Allowed bool
	// Bucket is the limiting bucket (the one that blocks, or has the least
	// headroom).
	Bucket   string
	Headroom int64
	Need     int64
}

// BucketView is a bucket with the ledger's reservations.
type BucketView struct {
	Vendor string
	// Region is empty for a global bucket.
	Region   string
	ID       string
	Scope    vendors.QuotaScope
	Unit     string
	Limit    int64
	Used     int64
	Reserved int64
}

// Headroom is limit - used - reserved.
func (b BucketView) Headroom() int64 { return b.Limit - b.Used - b.Reserved }

// Tracker implements the quota tracker. It is safe for concurrent use.
type Tracker struct {
	opts Options
	wake chan struct{}

	mu        sync.RWMutex
	states    map[regionKey]*state
	reporters map[string]vendors.QuotaReporter
	listers   map[string]vendors.OfferingLister
	reserved  map[bucketKey]int64
	ledger    map[uint64]*Reservation
	nextID    uint64
	// launchable merges the regions' data per vendor: type -> zone IDs, and the
	// zone IDs any region reported.
	launchable    map[string]map[string]map[string]bool
	launchZones   map[string]map[string]bool
	launchEpoch   uint64
	refreshErrors *prometheus.CounterVec
}

// New returns a tracker; Start (or Tick) drives it.
func New(o Options) *Tracker {
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	if o.Scope == nil {
		o.Scope = func() map[string][]string { return nil }
	}
	set := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	set(&o.RefreshInterval, DefaultRefreshInterval)
	set(&o.LimitsInterval, DefaultLimitsInterval)
	set(&o.MaxStaleness, DefaultMaxStaleness)
	set(&o.LaunchableInterval, DefaultLaunchableInterval)
	set(&o.Debounce, DefaultDebounce)
	set(&o.ReservationTTL, DefaultReservationTTL)
	if o.Jitter <= 0 {
		o.Jitter = DefaultJitter
	}
	t := &Tracker{
		opts: o, wake: make(chan struct{}, 1),
		states: map[regionKey]*state{}, reporters: map[string]vendors.QuotaReporter{},
		listers: map[string]vendors.OfferingLister{}, reserved: map[bucketKey]int64{},
		ledger: map[uint64]*Reservation{}, launchable: map[string]map[string]map[string]bool{},
		launchZones: map[string]map[string]bool{},
		refreshErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "scarpenter_quota_refresh_errors_total",
			Help: "Failed quota or launchable-type refreshes per vendor and region.",
		}, []string{labelVendor, labelRegion}),
	}
	if o.Registry != nil {
		for _, v := range o.Registry.Vendors() {
			d, _ := o.Registry.Driver(v)
			if r, ok := d.(vendors.QuotaReporter); ok {
				t.reporters[v] = r
			}
			if l, ok := d.(vendors.OfferingLister); ok {
				t.listers[v] = l
			}
		}
	}
	if o.Registerer != nil {
		register(o.Registerer, t.refreshErrors)
		register(o.Registerer, &collector{t: t})
	}
	return t
}

func register(reg prometheus.Registerer, c prometheus.Collector) {
	err := reg.Register(c)
	if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
		reg.Unregister(already.ExistingCollector)
		err = reg.Register(c)
	}
	if err != nil {
		panic(fmt.Sprintf("registering the quota metrics: %v", err))
	}
}

// NeedLeaderElection says that the tracker runs on the leader only.
func (t *Tracker) NeedLeaderElection() bool { return true }

// Start drives the tracker until the context ends.
func (t *Tracker) Start(ctx context.Context) error {
	for {
		t.Tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.wake:
		case <-t.opts.Clock.After(tickInterval):
		}
	}
}

func (t *Tracker) bump() {
	if t.opts.Unavailable != nil {
		t.opts.Unavailable.Bump()
	}
}

func (t *Tracker) jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (1 + t.opts.Jitter*(2*rand.Float64()-1))) //nolint:gosec // jitter
}

// Tick does the work that is due now: refreshes, expiry of reservations and the
// staleness check.
func (t *Tracker) Tick(ctx context.Context) { _ = t.run(ctx, false) }

// Refresh refreshes everything in scope now, limits included, and returns the
// joined errors of the refreshes that failed.
func (t *Tracker) Refresh(ctx context.Context) error { return t.run(ctx, true) }

type job struct {
	key    regionKey
	global bool
	limits bool
	usage  bool
	launch bool
}

func (t *Tracker) run(ctx context.Context, force bool) error {
	now := t.opts.Clock.Now()
	scope := t.opts.Scope()
	var jobs []job

	t.mu.Lock()
	changed := t.expireLocked(now)
	want := map[regionKey]bool{}
	for vendor, regions := range scope {
		_, rep := t.reporters[vendor]
		_, lis := t.listers[vendor]
		if !rep && !lis {
			continue
		}
		keys := []regionKey{{vendor, ""}}
		for _, r := range regions {
			keys = append(keys, regionKey{vendor, r})
		}
		for _, k := range keys {
			want[k] = true
			st := t.states[k]
			if st == nil {
				st = &state{}
				t.states[k] = st
			}
			if st.refreshing {
				continue
			}
			j := job{key: k, global: k.region == ""}
			if rep {
				j.usage = force || st.nextDue.IsZero() || !now.Before(st.nextDue) ||
					(!st.triggerAt.IsZero() && !now.Before(st.triggerAt))
				j.limits = force || st.limitsAt.IsZero() || now.Sub(st.limitsAt) >= t.opts.LimitsInterval
			}
			if lis && !j.global {
				j.launch = force || st.launchNext.IsZero() || !now.Before(st.launchNext)
			}
			if j.usage || j.launch {
				st.refreshing = true
				jobs = append(jobs, j)
			}
		}
	}
	for k := range t.states {
		if !want[k] {
			delete(t.states, k)
			changed = true
		}
	}
	for _, st := range t.states {
		stale := st.snap != nil && now.Sub(st.okAt) > t.opts.MaxStaleness
		if stale != st.stale {
			st.stale = stale
			changed = true
		}
	}
	t.mu.Unlock()
	if changed {
		t.bump()
	}

	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, maxParallel)
		emu  sync.Mutex
		errs []error
	)
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := t.refresh(ctx, j); err != nil {
				emu.Lock()
				errs = append(errs, err)
				emu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (t *Tracker) refresh(ctx context.Context, j job) error {
	var errs []error
	if j.usage {
		if err := t.refreshUsage(ctx, j); err != nil {
			errs = append(errs, err)
		}
	}
	if j.launch {
		if err := t.refreshLaunchable(ctx, j.key); err != nil {
			errs = append(errs, err)
		}
	}
	t.mu.Lock()
	if st := t.states[j.key]; st != nil {
		st.refreshing = false
	}
	t.mu.Unlock()
	return errors.Join(errs...)
}

func (t *Tracker) refreshUsage(ctx context.Context, j job) error {
	rep := t.reporters[j.key.vendor]
	cctx := vendors.WithQuotaRefresh(ctx, vendors.QuotaRefresh{Limits: j.limits})
	var snap *vendors.QuotaSnapshot
	var err error
	if j.global {
		snap, err = rep.GlobalQuotas(cctx)
	} else {
		snap, err = rep.Quotas(cctx, j.key.region)
	}
	now := t.opts.Clock.Now()

	t.mu.Lock()
	st := t.states[j.key]
	if st == nil {
		t.mu.Unlock()
		return nil
	}
	st.attemptAt, st.triggerAt = now, time.Time{}
	st.nextDue = now.Add(t.jittered(t.opts.RefreshInterval))
	if err != nil {
		st.lastErr = err
		t.mu.Unlock()
		t.refreshErrors.WithLabelValues(j.key.vendor, j.key.region).Inc()
		return fmt.Errorf("refreshing the %s quota of %s: %w", j.key.vendor, regionName(j.key.region), err)
	}
	if snap == nil {
		snap = &vendors.QuotaSnapshot{}
	}
	changed := st.snap == nil || !slices.Equal(st.snap.Buckets, snap.Buckets) || st.stale
	st.snap, st.okAt, st.lastErr, st.stale = snap, now, nil, false
	if j.limits {
		st.limitsAt = now
	}
	if t.releaseCountedLocked(j.key.vendor, snap.Counted) {
		changed = true
	}
	t.mu.Unlock()
	if changed {
		t.bump()
	}
	return nil
}

func regionName(r string) string {
	if r == "" {
		return "the account"
	}
	return r
}

func (t *Tracker) refreshLaunchable(ctx context.Context, k regionKey) error {
	types, err := t.listers[k.vendor].LaunchableTypes(ctx, k.region)
	now := t.opts.Clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.states[k]
	if st == nil {
		return nil
	}
	st.launchNext = now.Add(t.jittered(t.opts.LaunchableInterval))
	if err != nil {
		// Retry with the usage cadence, not after the launchable interval.
		st.launchNext = now.Add(t.jittered(t.opts.RefreshInterval))
		t.refreshErrors.WithLabelValues(k.vendor, k.region).Inc()
		return fmt.Errorf("listing the launchable %s types of %s: %w", k.vendor, k.region, err)
	}
	st.launchAt = now
	if mapsEqual(st.launchTypes, types) {
		return nil
	}
	st.launchTypes = types
	t.rebuildLaunchableLocked(k.vendor)
	t.launchEpoch++
	return nil
}

func mapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !slices.Equal(v, w) {
			return false
		}
	}
	return true
}

func (t *Tracker) rebuildLaunchableLocked(vendor string) {
	merged := map[string]map[string]bool{}
	zones := map[string]bool{}
	for k, st := range t.states {
		if k.vendor != vendor || k.region == "" {
			continue
		}
		for typ, zs := range st.launchTypes {
			if merged[typ] == nil {
				merged[typ] = map[string]bool{}
			}
			for _, z := range zs {
				merged[typ][z] = true
				zones[z] = true
			}
		}
	}
	t.launchable[vendor], t.launchZones[vendor] = merged, zones
}

// Launchable reports whether the driver says the type can be launched in the
// zone (Keeper's zone ID). known is false when the vendor has no OfferingLister
// or no region reported the zone yet: a zone in which nothing is launchable at
// all is indistinguishable from an unknown one and stays offered.
func (t *Tracker) Launchable(vendor, apiReference, zoneID string) (launchable, known bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.launchZones[vendor][zoneID] {
		return false, false
	}
	return t.launchable[vendor][apiReference][zoneID], true
}

// LaunchableEpoch changes whenever the launchable types change.
func (t *Tracker) LaunchableEpoch() uint64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.launchEpoch
}

// freshLocked returns the usable snapshot of a state.
func (t *Tracker) freshLocked(k regionKey, now time.Time) *vendors.QuotaSnapshot {
	st := t.states[k]
	if st == nil || st.snap == nil || now.Sub(st.okAt) > t.opts.MaxStaleness {
		return nil
	}
	return st.snap
}

type needed struct {
	key    bucketKey
	amount int64
	b      vendors.Bucket
}

// needsLocked resolves the consumption of an instance to the known buckets of
// the region's and the account's snapshots; buckets without a fresh snapshot
// entry are left out (unknown).
func (t *Tracker) needsLocked(vendor, region, apiRef, capacityType string, now time.Time) []needed {
	rep := t.reporters[vendor]
	if rep == nil {
		return nil
	}
	cons, ok := rep.Consumption(apiRef, capacityType)
	if !ok {
		return nil
	}
	rs := t.freshLocked(regionKey{vendor, region}, now)
	gs := t.freshLocked(regionKey{vendor, ""}, now)
	var out []needed
	for _, c := range cons {
		if rs != nil {
			if i := slices.IndexFunc(rs.Buckets, func(b vendors.Bucket) bool { return b.ID == c.Bucket }); i >= 0 {
				out = append(out, needed{bucketKey{vendor, region, c.Bucket}, c.Amount, rs.Buckets[i]})
				continue
			}
		}
		if gs != nil {
			if i := slices.IndexFunc(gs.Buckets, func(b vendors.Bucket) bool { return b.ID == c.Bucket }); i >= 0 {
				out = append(out, needed{bucketKey{vendor, "", c.Bucket}, c.Amount, gs.Buckets[i]})
			}
		}
	}
	return out
}

// evalLocked computes the decision for one instance.
func (t *Tracker) evalLocked(vendor, region, apiRef, capacityType string, now time.Time) (Decision, []needed) {
	ns := t.needsLocked(vendor, region, apiRef, capacityType, now)
	if len(ns) == 0 {
		return Decision{Allowed: true}, nil
	}
	d := Decision{Known: true, Allowed: true}
	first := true
	for _, n := range ns {
		head := n.b.Limit - n.b.Used - t.reserved[n.key]
		short := head < n.amount
		// The limiting bucket is the first that blocks, else the one with the
		// least headroom.
		better := first || (short && d.Allowed) || (short == !d.Allowed && head < d.Headroom)
		if better {
			d.Bucket, d.Headroom, d.Need = n.b.ID, head, n.amount
		}
		if short {
			d.Allowed = false
		}
		first = false
	}
	return d, ns
}

// Unavailable implements catalog.Gate: the quota is known and has no headroom
// for one more instance.
func (t *Tracker) Unavailable(vendor, apiReference, region, _, capacityType string) bool {
	now := t.opts.Clock.Now()
	t.mu.RLock()
	defer t.mu.RUnlock()
	d, _ := t.evalLocked(vendor, region, apiReference, capacityType, now)
	return d.Known && !d.Allowed
}

// Headroom returns the smallest headroom over the buckets the instance
// consumes, ledger included; known is false for unknown quota.
func (t *Tracker) Headroom(vendor, region, apiReference, capacityType string) (headroom int64, known bool) {
	now := t.opts.Clock.Now()
	t.mu.RLock()
	defer t.mu.RUnlock()
	d, _ := t.evalLocked(vendor, region, apiReference, capacityType, now)
	return d.Headroom, d.Known
}

// HasQuotaInfo reports whether the driver reports quotas and maps the type.
func (t *Tracker) HasQuotaInfo(vendor, apiReference, capacityType string) bool {
	rep := t.reporters[vendor]
	if rep == nil {
		return false
	}
	_, ok := rep.Consumption(apiReference, capacityType)
	return ok
}

// Blocked is Unavailable with the limiting bucket.
func (t *Tracker) Blocked(vendor, region, apiReference, capacityType string) (blocked bool, limiting BucketView) {
	now := t.opts.Clock.Now()
	t.mu.RLock()
	defer t.mu.RUnlock()
	d, ns := t.evalLocked(vendor, region, apiReference, capacityType, now)
	if !d.Known || d.Allowed {
		return false, BucketView{}
	}
	for _, n := range ns {
		if n.b.ID == d.Bucket {
			return true, t.viewLocked(n.key, n.b)
		}
	}
	return true, BucketView{}
}

func (t *Tracker) viewLocked(k bucketKey, b vendors.Bucket) BucketView {
	scope := b.Scope
	if scope == "" {
		scope = vendors.QuotaScopeRegion
		if k.region == "" {
			scope = vendors.QuotaScopeGlobal
		}
	}
	return BucketView{
		Vendor: k.vendor, Region: k.region, ID: b.ID, Scope: scope, Unit: b.Unit,
		Limit: b.Limit, Used: b.Used, Reserved: t.reserved[k],
	}
}

// Buckets returns every bucket of the fresh snapshots with its reservations,
// sorted by vendor, region and ID.
func (t *Tracker) Buckets() []BucketView {
	now := t.opts.Clock.Now()
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []BucketView
	for k := range t.states {
		snap := t.freshLocked(k, now)
		if snap == nil {
			continue
		}
		for _, b := range snap.Buckets {
			out = append(out, t.viewLocked(bucketKey{k.vendor, k.region, b.ID}, b))
		}
	}
	slices.SortFunc(out, func(a, b BucketView) int {
		if c := cmpStr(a.Vendor, b.Vendor); c != 0 {
			return c
		}
		if c := cmpStr(a.Region, b.Region); c != 0 {
			return c
		}
		return cmpStr(a.ID, b.ID)
	})
	return out
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Reserve atomically checks the headroom for one instance and reserves its
// consumption. For unknown quota it reserves nothing and allows the launch.
func (t *Tracker) Reserve(vendor, region, apiReference, capacityType, owner string) (*Reservation, Decision) {
	now := t.opts.Clock.Now()
	t.mu.Lock()
	changed := t.expireLocked(now)
	d, ns := t.evalLocked(vendor, region, apiReference, capacityType, now)
	var r *Reservation
	if d.Known && d.Allowed {
		t.nextID++
		r = &Reservation{id: t.nextID, vendor: vendor, region: region, owner: owner, createdAt: now}
		for _, n := range ns {
			r.items = append(r.items, item{n.key, n.amount})
			t.reserved[n.key] += n.amount
		}
		t.ledger[r.id] = r
		changed = true
	}
	t.mu.Unlock()
	if changed {
		t.bump()
	}
	return r, d
}

// Commit ties the reservation to the instance the launch created, so that the
// refresh that counts the instance releases it. Nil-safe.
func (t *Tracker) Commit(r *Reservation, ref vendors.InstanceRef) {
	if r == nil {
		return
	}
	t.mu.Lock()
	r.ref = ref
	t.mu.Unlock()
}

// Reconcile ties the reservation of an attempted launch to the instance the
// vendor returned, when that is not the offering that was attempted (a replayed
// idempotency key returns the first instance, whatever the replay asked for):
// the attempted offering's reservation is released and the instance's own
// consumption is reserved against the instance, unless a reservation of the
// instance exists already. The headroom is not checked: the instance exists.
// Nil-safe on res.
func (t *Tracker) Reconcile(
	res *Reservation, vendor, region, apiReference, capacityType, owner string, ref vendors.InstanceRef,
) {
	now := t.opts.Clock.Now()
	t.mu.Lock()
	changed := false
	if res != nil {
		changed = t.dropLocked(res)
	}
	has := false
	for _, r := range t.ledger {
		if r.vendor == vendor && r.ref == ref {
			has = true
			break
		}
	}
	if !has {
		if ns := t.needsLocked(vendor, region, apiReference, capacityType, now); len(ns) > 0 {
			t.nextID++
			r := &Reservation{id: t.nextID, vendor: vendor, region: region, owner: owner, createdAt: now, ref: ref}
			for _, n := range ns {
				r.items = append(r.items, item{n.key, n.amount})
				t.reserved[n.key] += n.amount
			}
			t.ledger[r.id] = r
			changed = true
		}
	}
	t.mu.Unlock()
	if changed {
		t.bump()
	}
}

// Release drops a reservation (the launch failed). Nil-safe and idempotent.
func (t *Tracker) Release(r *Reservation) {
	if r == nil {
		return
	}
	t.mu.Lock()
	ok := t.dropLocked(r)
	t.mu.Unlock()
	if ok {
		t.bump()
	}
}

func (t *Tracker) dropLocked(r *Reservation) bool {
	if _, ok := t.ledger[r.id]; !ok {
		return false
	}
	delete(t.ledger, r.id)
	for _, it := range r.items {
		if t.reserved[it.key] -= it.amount; t.reserved[it.key] <= 0 {
			delete(t.reserved, it.key)
		}
	}
	return true
}

func (t *Tracker) expireLocked(now time.Time) bool {
	changed := false
	for _, r := range t.ledger {
		if !now.Before(r.createdAt.Add(t.opts.ReservationTTL)) {
			changed = t.dropLocked(r) || changed
		}
	}
	return changed
}

func (t *Tracker) releaseCountedLocked(vendor string, counted []vendors.InstanceRef) bool {
	if len(counted) == 0 {
		return false
	}
	changed := false
	for _, r := range t.ledger {
		if r.vendor == vendor && r.ref != "" && slices.Contains(counted, r.ref) {
			changed = t.dropLocked(r) || changed
		}
	}
	return changed
}

// ReleaseInstance drops the reservations tied to the instance: it is being
// terminated, so a refresh that has not counted it yet never will, and the
// reservation would otherwise hold headroom until it expires.
func (t *Tracker) ReleaseInstance(vendor string, ref vendors.InstanceRef) {
	t.mu.Lock()
	changed := false
	for _, r := range t.ledger {
		if r.vendor == vendor && r.ref == ref && ref != "" {
			changed = t.dropLocked(r) || changed
		}
	}
	t.mu.Unlock()
	if changed {
		t.bump()
	}
}

// NotifyQuotaError asks for a refresh of the region after a QuotaExceeded error.
func (t *Tracker) NotifyQuotaError(vendor, region string) { t.notify(vendor, region) }

// NotifyChanged asks for a refresh of the region after SCarpenter launched or
// terminated an instance there.
func (t *Tracker) NotifyChanged(vendor, region string) { t.notify(vendor, region) }

func (t *Tracker) notify(vendor, region string) {
	now := t.opts.Clock.Now()
	t.mu.Lock()
	if _, ok := t.reporters[vendor]; !ok {
		t.mu.Unlock()
		return
	}
	for _, k := range []regionKey{{vendor, region}, {vendor, ""}} {
		st := t.states[k]
		if st == nil {
			st = &state{}
			t.states[k] = st
		}
		at := now
		if !st.attemptAt.IsZero() && now.Sub(st.attemptAt) < t.opts.Debounce {
			at = st.attemptAt.Add(t.opts.Debounce)
		}
		if st.triggerAt.IsZero() || at.Before(st.triggerAt) {
			st.triggerAt = at
		}
	}
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// collector exports the buckets and the refresh ages.
type collector struct{ t *Tracker }

var (
	bucketLabels = []string{labelVendor, labelRegion, "bucket"}
	limitDesc    = prometheus.NewDesc("scarpenter_quota_limit", "Quota limit of a bucket.", bucketLabels, nil)
	usedDesc     = prometheus.NewDesc("scarpenter_quota_used",
		"Quota usage of a bucket (the whole account).", bucketLabels, nil)
	reservedDesc = prometheus.NewDesc("scarpenter_quota_reserved",
		"In-flight reservations of launches that the usage does not show yet.", bucketLabels, nil)
	ageDesc = prometheus.NewDesc("scarpenter_quota_refresh_age_seconds",
		"Seconds since the last successful quota refresh of a vendor and region.", []string{labelVendor, labelRegion}, nil)
)

// Describe implements prometheus.Collector.
func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- limitDesc
	ch <- usedDesc
	ch <- reservedDesc
	ch <- ageDesc
}

// Collect implements prometheus.Collector.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	for _, b := range c.t.Buckets() {
		l := []string{b.Vendor, b.Region, b.ID}
		ch <- prometheus.MustNewConstMetric(limitDesc, prometheus.GaugeValue, float64(b.Limit), l...)
		ch <- prometheus.MustNewConstMetric(usedDesc, prometheus.GaugeValue, float64(b.Used), l...)
		ch <- prometheus.MustNewConstMetric(reservedDesc, prometheus.GaugeValue, float64(b.Reserved), l...)
	}
	now := c.t.opts.Clock.Now()
	c.t.mu.RLock()
	defer c.t.mu.RUnlock()
	for k, st := range c.t.states {
		if st.snap != nil {
			ch <- prometheus.MustNewConstMetric(ageDesc, prometheus.GaugeValue, now.Sub(st.okAt).Seconds(), k.vendor, k.region)
		}
	}
}
