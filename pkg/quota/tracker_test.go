/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package quota_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/quota"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// stub is a driver with quotas and launchable types.
type stub struct {
	mu        sync.Mutex
	regional  map[string][]vendors.Bucket // region -> buckets
	global    []vendors.Bucket
	counted   []vendors.InstanceRef
	consume   map[string][]vendors.Consumption // "type/capacity" -> consumption
	launch    map[string]map[string][]string
	err       error
	calls     map[string]int
	limitsSeq []bool
}

func newStub() *stub {
	return &stub{
		regional: map[string][]vendors.Bucket{}, consume: map[string][]vendors.Consumption{},
		launch: map[string]map[string][]string{}, calls: map[string]int{},
	}
}

func (s *stub) Vendor() string                                    { return "stub" }
func (s *stub) Init(context.Context, vendors.Config) error        { return nil }
func (s *stub) Regions(context.Context) ([]vendors.Region, error) { return nil, nil }
func (s *stub) Zones(context.Context, string) ([]vendors.Zone, error) {
	return nil, nil
}
func (s *stub) ResolveImage(context.Context, vendors.ImageRequest) (*vendors.Image, error) {
	return nil, nil
}
func (s *stub) CreateInstance(context.Context, vendors.CreateRequest) (*vendors.Instance, error) {
	return nil, nil
}
func (s *stub) GetInstance(context.Context, vendors.InstanceRef) (*vendors.Instance, error) {
	return nil, nil
}
func (s *stub) FindInstance(context.Context, string, map[vendors.TagKey]string) (*vendors.Instance, error) {
	return nil, nil
}
func (s *stub) ListInstances(context.Context, []string, map[vendors.TagKey]string) ([]*vendors.Instance, error) {
	return nil, nil
}
func (s *stub) DeleteInstance(context.Context, vendors.InstanceRef) error { return nil }
func (s *stub) TagKey(k vendors.TagKey) string                            { return vendors.DefaultTagKey(k) }

func (s *stub) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[region]++
	s.limitsSeq = append(s.limitsSeq, vendors.QuotaRefreshFrom(ctx).Limits)
	if s.err != nil {
		return nil, s.err
	}
	return &vendors.QuotaSnapshot{Buckets: append([]vendors.Bucket(nil), s.regional[region]...),
		Counted: append([]vendors.InstanceRef(nil), s.counted...)}, nil
}

func (s *stub) GlobalQuotas(context.Context) (*vendors.QuotaSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[""]++
	if s.err != nil {
		return nil, s.err
	}
	return &vendors.QuotaSnapshot{Buckets: append([]vendors.Bucket(nil), s.global...)}, nil
}

func (s *stub) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.consume[instanceType+"/"+capacityType]
	return c, ok
}

func (s *stub) LaunchableTypes(_ context.Context, region string) (map[string][]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls["launch/"+region]++
	if s.err != nil {
		return nil, s.err
	}
	return s.launch[region], nil
}

func (s *stub) set(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

func (s *stub) callCount(region string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[region]
}

type env struct {
	t     *testing.T
	clk   *clocktesting.FakeClock
	drv   *stub
	un    *unavailable.Cache
	tr    *quota.Tracker
	scope map[string][]string
}

func setup(t *testing.T, mod func(*quota.Options)) *env {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	drv := newStub()
	drv.regional["r1"] = []vendors.Bucket{{ID: "std", Scope: vendors.QuotaScopeRegion, Unit: "vCPU", Limit: 16, Used: 4}}
	drv.regional["r2"] = []vendors.Bucket{{ID: "std", Scope: vendors.QuotaScopeRegion, Unit: "vCPU", Limit: 100, Used: 0}}
	drv.consume["m.large/on-demand"] = []vendors.Consumption{{Bucket: "std", Amount: 4}}
	drv.consume["m.xlarge/on-demand"] = []vendors.Consumption{{Bucket: "std", Amount: 8}, {Bucket: "all", Amount: 8}}
	un := unavailable.New(clk, unavailable.TTLs{})
	e := &env{t: t, clk: clk, drv: drv, un: un, scope: map[string][]string{"stub": {"r1", "r2"}}}
	o := quota.Options{
		Registry: vendors.NewRegistryFrom(drv), Clock: clk, Unavailable: un,
		Scope:           func() map[string][]string { return e.scope },
		RefreshInterval: 2 * time.Minute, LimitsInterval: 15 * time.Minute, MaxStaleness: 15 * time.Minute,
		Debounce: 10 * time.Second, ReservationTTL: 10 * time.Minute, Jitter: 0.0001,
	}
	if mod != nil {
		mod(&o)
	}
	e.tr = quota.New(o)
	return e
}

func (e *env) refresh() {
	e.t.Helper()
	if err := e.tr.Refresh(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func TestHeadroomAndAvailability(t *testing.T) {
	e := setup(t, nil)
	e.refresh()
	for _, tc := range []struct {
		name, region, typ, ct string
		blocked, known        bool
		head                  int64
	}{
		{"fits", "r1", "m.large", "on-demand", false, true, 12},
		{"region with room", "r2", "m.large", "on-demand", false, true, 100},
		{"unknown type", "r1", "x.huge", "on-demand", false, false, 0},
		{"known type, other capacity type", "r1", "m.large", "spot", false, false, 0},
	} {
		if got := e.tr.Unavailable("stub", tc.typ, tc.region, "z", tc.ct); got != tc.blocked {
			t.Errorf("%s: unavailable = %v", tc.name, got)
		}
		if h, k := e.tr.Headroom("stub", tc.region, tc.typ, tc.ct); k != tc.known || (k && h != tc.head) {
			t.Errorf("%s: headroom = %d known %v", tc.name, h, k)
		}
	}
	// Exactly enough room is allowed, one vCPU less is not.
	e.drv.set(func() { e.drv.regional["r1"][0].Used = 12 })
	e.refresh()
	if e.tr.Unavailable("stub", "m.large", "r1", "z", "on-demand") {
		t.Error("4 free vCPUs should fit a 4 vCPU instance")
	}
	e.drv.set(func() { e.drv.regional["r1"][0].Used = 13 })
	e.refresh()
	blocked, b := e.tr.Blocked("stub", "r1", "m.large", "on-demand")
	if !blocked || b.ID != "std" || b.Limit != 16 || b.Used != 13 {
		t.Errorf("blocked = %v %+v", blocked, b)
	}
}

func TestBucketMissingFromSnapshotIsUnknown(t *testing.T) {
	e := setup(t, nil)
	e.drv.set(func() { e.drv.consume["g.large/on-demand"] = []vendors.Consumption{{Bucket: "gpu", Amount: 4}} })
	e.refresh()
	if e.tr.Unavailable("stub", "g.large", "r1", "z", "on-demand") {
		t.Error("a bucket the driver did not report must not block")
	}
	if _, known := e.tr.Headroom("stub", "r1", "g.large", "on-demand"); known {
		t.Error("expected unknown")
	}
	if !e.tr.HasQuotaInfo("stub", "g.large", "on-demand") || e.tr.HasQuotaInfo("stub", "nope", "on-demand") {
		t.Error("HasQuotaInfo is wrong")
	}
	if e.tr.HasQuotaInfo("other", "m.large", "on-demand") {
		t.Error("a vendor without a reporter has no quota info")
	}
}

func TestGlobalBucketSpansRegions(t *testing.T) {
	e := setup(t, nil)
	e.drv.set(func() {
		e.drv.global = []vendors.Bucket{{ID: "all", Scope: vendors.QuotaScopeGlobal, Unit: "vCPU", Limit: 10, Used: 0}}
	})
	e.refresh()
	r1, d := e.tr.Reserve("stub", "r1", "m.xlarge", "on-demand", "a")
	if r1 == nil || !d.Allowed || !d.Known {
		t.Fatalf("first reserve: %+v", d)
	}
	// 8 of 10 global vCPUs are reserved in r1; r2 has plenty regionally but not globally.
	r2, d := e.tr.Reserve("stub", "r2", "m.xlarge", "on-demand", "b")
	if r2 != nil || d.Allowed || d.Bucket != "all" {
		t.Fatalf("second reserve should be blocked by the global bucket: %+v", d)
	}
	e.tr.Release(r1)
	if r, d := e.tr.Reserve("stub", "r2", "m.xlarge", "on-demand", "b"); r == nil || !d.Allowed {
		t.Fatalf("after release: %+v", d)
	}
	var global int
	for _, b := range e.tr.Buckets() {
		if b.ID == "all" {
			global++
			if b.Region != "" || b.Scope != vendors.QuotaScopeGlobal || b.Reserved != 8 {
				t.Errorf("global bucket view: %+v", b)
			}
		}
	}
	if global != 1 {
		t.Errorf("want one global bucket view, got %d", global)
	}
}

func TestReservationLifecycle(t *testing.T) {
	e := setup(t, nil)
	e.refresh()
	seq := e.un.Sequence()

	r, d := e.tr.Reserve("stub", "r1", "m.large", "on-demand", "claim-a")
	if r == nil || !d.Allowed || d.Need != 4 || d.Bucket != "std" {
		t.Fatalf("reserve: %+v", d)
	}
	if e.un.Sequence() == seq {
		t.Error("a reservation must bump the sequence")
	}
	if h, _ := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); h != 8 {
		t.Errorf("headroom after reserving = %d, want 8", h)
	}
	// Unknown quota: allowed, nothing reserved.
	if r, d := e.tr.Reserve("stub", "r1", "x.huge", "on-demand", "x"); r != nil || !d.Allowed || d.Known {
		t.Errorf("unknown reserve: %+v", d)
	}

	// Release is idempotent and nil-safe.
	seq = e.un.Sequence()
	e.tr.Release(r)
	e.tr.Release(r)
	e.tr.Release(nil)
	e.tr.Commit(nil, "x")
	if h, _ := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); h != 12 {
		t.Errorf("headroom after release = %d, want 12", h)
	}
	if e.un.Sequence() == seq {
		t.Error("a release must bump the sequence")
	}

	// A committed reservation is released when a refresh counts the instance.
	r, _ = e.tr.Reserve("stub", "r1", "m.large", "on-demand", "claim-b")
	e.tr.Commit(r, "r1/i-1")
	e.refresh()
	if h, _ := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); h != 8 {
		t.Errorf("not yet counted: headroom = %d, want 8", h)
	}
	e.drv.set(func() {
		e.drv.regional["r1"][0].Used = 8
		e.drv.counted = []vendors.InstanceRef{"r1/i-1"}
	})
	e.refresh()
	if h, _ := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); h != 8 {
		t.Errorf("counted: headroom = %d, want 8 (used 8, no reservation)", h)
	}
	for _, b := range e.tr.Buckets() {
		if b.Region == "r1" && b.Reserved != 0 {
			t.Errorf("reservation not released: %+v", b)
		}
	}

	// A terminated instance releases its reservation without being counted.
	e.drv.set(func() { e.drv.counted = nil })
	r, _ = e.tr.Reserve("stub", "r1", "m.large", "on-demand", "claim-d")
	e.tr.Commit(r, "r1/i-gone")
	e.tr.ReleaseInstance("stub", "r1/i-gone")
	e.tr.ReleaseInstance("stub", "r1/i-gone")
	if h, _ := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); h != 8 {
		t.Errorf("after ReleaseInstance headroom = %d, want 8", h)
	}

	// An uncommitted reservation expires after the TTL.
	e.drv.set(func() { e.drv.counted = nil })
	r, _ = e.tr.Reserve("stub", "r1", "m.large", "on-demand", "claim-c")
	_ = r
	e.clk.Step(9 * time.Minute)
	e.tr.Tick(context.Background())
	if h, _ := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); h != 4 {
		t.Errorf("before expiry headroom = %d, want 4", h)
	}
	e.clk.Step(2 * time.Minute)
	e.tr.Tick(context.Background())
	if h, _ := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); h != 8 {
		t.Errorf("after expiry headroom = %d, want 8", h)
	}
}

func TestConcurrentReservesCompeteForTheLastHeadroom(t *testing.T) {
	e := setup(t, nil)
	e.drv.set(func() { e.drv.regional["r1"][0].Used = 0; e.drv.regional["r1"][0].Limit = 40 })
	e.refresh()
	var won atomic.Int64
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			if r, d := e.tr.Reserve("stub", "r1", "m.large", "on-demand", string(rune('a'+i%26))); r != nil && d.Allowed {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	if won.Load() != 10 {
		t.Fatalf("%d reservations won, want exactly 10 (40 vCPUs / 4)", won.Load())
	}
	if h, _ := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); h != 0 {
		t.Errorf("headroom = %d, want 0", h)
	}
}

func TestStalenessMakesTheRegionUnknown(t *testing.T) {
	e := setup(t, nil)
	e.drv.set(func() { e.drv.regional["r1"][0].Used = 16 })
	e.refresh()
	if !e.tr.Unavailable("stub", "m.large", "r1", "z", "on-demand") {
		t.Fatal("expected blocked")
	}
	e.drv.set(func() { e.drv.err = errors.New("api down") })
	seq := e.un.Sequence()
	e.clk.Step(14 * time.Minute)
	e.tr.Tick(context.Background())
	if !e.tr.Unavailable("stub", "m.large", "r1", "z", "on-demand") {
		t.Error("still within the staleness window")
	}
	e.clk.Step(2 * time.Minute)
	e.tr.Tick(context.Background())
	if e.tr.Unavailable("stub", "m.large", "r1", "z", "on-demand") {
		t.Error("a stale region must count as unknown, not blocked")
	}
	if _, known := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); known {
		t.Error("stale: want unknown")
	}
	if e.un.Sequence() == seq {
		t.Error("the staleness flip must bump the sequence")
	}
	// The API comes back.
	e.drv.set(func() { e.drv.err = nil })
	e.refresh()
	if !e.tr.Unavailable("stub", "m.large", "r1", "z", "on-demand") {
		t.Error("fresh data blocks again")
	}
}

func TestRefreshErrorsKeepTheLastValues(t *testing.T) {
	reg := prometheus.NewRegistry()
	e := setup(t, func(o *quota.Options) { o.Registerer = reg })
	e.refresh()
	e.drv.set(func() { e.drv.err = errors.New("boom") })
	if err := e.tr.Refresh(context.Background()); err == nil {
		t.Fatal("want the refresh error")
	}
	if h, known := e.tr.Headroom("stub", "r1", "m.large", "on-demand"); !known || h != 12 {
		t.Errorf("last values must stay: %d %v", h, known)
	}
	if n := errorCount(t, reg, "r1"); n < 1 {
		t.Errorf("refresh errors = %v", n)
	}
}

func errorCount(t *testing.T, reg *prometheus.Registry, region string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "scarpenter_quota_refresh_errors_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "region" && l.GetValue() == region {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("no error counter for %s", region)
	return 0
}

func TestRefreshCadenceAndLimitsFlag(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	e.tr.Tick(ctx) // first tick: everything is due
	if e.drv.callCount("r1") != 1 || e.drv.callCount("") != 1 {
		t.Fatalf("calls after the first tick: r1=%d global=%d", e.drv.callCount("r1"), e.drv.callCount(""))
	}
	e.clk.Step(time.Minute)
	e.tr.Tick(ctx)
	if e.drv.callCount("r1") != 1 {
		t.Error("refreshed before the interval")
	}
	e.clk.Step(90 * time.Second) // 2.5 minutes
	e.tr.Tick(ctx)
	if e.drv.callCount("r1") != 2 {
		t.Errorf("usage refresh missing: %d", e.drv.callCount("r1"))
	}
	e.drv.mu.Lock()
	first := e.drv.limitsSeq[0]
	second := e.drv.limitsSeq[len(e.drv.limitsSeq)-1]
	e.drv.mu.Unlock()
	if !first {
		t.Error("the first refresh must read the limits")
	}
	_ = second
	// Limits are due again after 15 minutes; usage-only refreshes in between.
	var sawUsageOnly, sawLimits bool
	for range 8 {
		e.clk.Step(2*time.Minute + 30*time.Second)
		e.tr.Tick(ctx)
	}
	e.drv.mu.Lock()
	for _, l := range e.drv.limitsSeq[2:] {
		sawUsageOnly = sawUsageOnly || !l
		sawLimits = sawLimits || l
	}
	e.drv.mu.Unlock()
	if !sawUsageOnly || !sawLimits {
		t.Errorf("want both usage-only and limit refreshes, got usage-only=%v limits=%v", sawUsageOnly, sawLimits)
	}
}

func TestScopeChangesDropAndAddRegions(t *testing.T) {
	e := setup(t, nil)
	e.refresh()
	if len(e.tr.Buckets()) == 0 {
		t.Fatal("no buckets")
	}
	e.scope = map[string][]string{"stub": {"r2"}}
	e.tr.Tick(context.Background())
	for _, b := range e.tr.Buckets() {
		if b.Region == "r1" {
			t.Errorf("r1 left the scope but its bucket stays: %+v", b)
		}
	}
	if e.tr.Unavailable("stub", "m.large", "r1", "z", "on-demand") {
		t.Error("a region out of scope has unknown quota")
	}
}

func TestDebouncedTriggers(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	e.tr.Tick(ctx)
	base := e.drv.callCount("r1")

	// Within the debounce window the refresh waits for its end.
	e.clk.Step(3 * time.Second)
	e.tr.NotifyQuotaError("stub", "r1")
	e.tr.NotifyChanged("stub", "r1")
	e.tr.Tick(ctx)
	if e.drv.callCount("r1") != base {
		t.Fatal("refreshed inside the debounce window")
	}
	e.clk.Step(8 * time.Second)
	e.tr.Tick(ctx)
	if e.drv.callCount("r1") != base+1 {
		t.Fatalf("want one coalesced refresh, got %d", e.drv.callCount("r1")-base)
	}
	if e.drv.callCount("r2") != base {
		t.Error("another region was refreshed")
	}

	// Outside the window the refresh is immediate.
	e.clk.Step(30 * time.Second)
	e.tr.NotifyChanged("stub", "r1")
	e.tr.Tick(ctx)
	if e.drv.callCount("r1") != base+2 {
		t.Errorf("want an immediate refresh, got %d", e.drv.callCount("r1")-base)
	}
	// Vendors without a reporter are ignored.
	e.tr.NotifyChanged("nobody", "r1")
}

func TestLaunchableTypes(t *testing.T) {
	e := setup(t, nil)
	e.drv.set(func() {
		e.drv.launch["r1"] = map[string][]string{"m.large": {"z1", "z2"}, "m.xlarge": {"z1"}}
		e.drv.launch["r2"] = map[string][]string{"m.large": {"z3"}}
	})
	if _, known := e.tr.Launchable("stub", "m.large", "z1"); known {
		t.Error("nothing is known before the first refresh")
	}
	epoch := e.tr.LaunchableEpoch()
	e.refresh()
	if e.tr.LaunchableEpoch() == epoch {
		t.Error("the epoch must change with new data")
	}
	for _, tc := range []struct {
		typ, zone      string
		launchable, ok bool
	}{
		{"m.large", "z1", true, true},
		{"m.xlarge", "z1", true, true},
		{"m.xlarge", "z2", false, true},
		{"m.large", "z3", true, true},
		{"m.huge", "z1", false, true},
		{"m.large", "z9", false, false}, // a zone nobody reported: unknown
	} {
		l, k := e.tr.Launchable("stub", tc.typ, tc.zone)
		if l != tc.launchable || k != tc.ok {
			t.Errorf("%s in %s: launchable=%v known=%v", tc.typ, tc.zone, l, k)
		}
	}
	epoch = e.tr.LaunchableEpoch()
	e.refresh()
	if e.tr.LaunchableEpoch() != epoch {
		t.Error("an unchanged answer must not change the epoch")
	}
	e.drv.set(func() { e.drv.launch["r1"] = map[string][]string{"m.large": {"z1"}} })
	e.refresh()
	if e.tr.LaunchableEpoch() == epoch {
		t.Error("a changed answer must change the epoch")
	}
	if l, _ := e.tr.Launchable("stub", "m.large", "z2"); l {
		t.Error("z2 is no longer launchable")
	}
}

func TestRefreshBumpsOnlyOnChange(t *testing.T) {
	e := setup(t, nil)
	e.refresh()
	seq := e.un.Sequence()
	e.refresh()
	if e.un.Sequence() != seq {
		t.Error("an identical refresh must not bump the sequence")
	}
	e.drv.set(func() { e.drv.regional["r1"][0].Used = 5 })
	e.refresh()
	if e.un.Sequence() == seq {
		t.Error("changed usage must bump the sequence")
	}
}

func TestMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	e := setup(t, func(o *quota.Options) { o.Registerer = reg })
	e.refresh()
	if r, _ := e.tr.Reserve("stub", "r1", "m.large", "on-demand", "a"); r == nil {
		t.Fatal("reserve")
	}
	e.clk.Step(30 * time.Second)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, mf := range mfs {
		got[mf.GetName()] = true
		if mf.GetName() == "scarpenter_quota_reserved" {
			for _, m := range mf.GetMetric() {
				var region string
				for _, l := range m.GetLabel() {
					if l.GetName() == "region" {
						region = l.GetValue()
					}
				}
				if region == "r1" && m.GetGauge().GetValue() != 4 {
					t.Errorf("reserved = %v", m.GetGauge().GetValue())
				}
			}
		}
		if mf.GetName() == "scarpenter_quota_refresh_age_seconds" {
			for _, m := range mf.GetMetric() {
				if m.GetGauge().GetValue() != 30 {
					t.Errorf("age = %v", m.GetGauge().GetValue())
				}
			}
		}
	}
	for _, name := range []string{"scarpenter_quota_limit", "scarpenter_quota_used", "scarpenter_quota_reserved",
		"scarpenter_quota_refresh_age_seconds"} {
		if !got[name] {
			t.Errorf("metric %s missing", name)
		}
	}
	// A second tracker replaces the collectors of the first.
	setup(t, func(o *quota.Options) { o.Registerer = reg })
}
