/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package facts_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
)

// regionDriver has several regions whose zone listing can be made to fail.
type regionDriver struct {
	driver
	mu   sync.Mutex
	errs map[string]error
}

func (d *regionDriver) set(region string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.errs == nil {
		d.errs = map[string]error{}
	}
	if err == nil {
		delete(d.errs, region)
	} else {
		d.errs[region] = err
	}
}

func (d *regionDriver) Regions(context.Context) ([]vendors.Region, error) {
	return []vendors.Region{{Name: "af-south-1"}, {Name: "eu-west-1"}, {Name: "us-east-2"}}, nil
}

func (d *regionDriver) Zones(_ context.Context, region string) ([]vendors.Zone, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[region]; err != nil {
		return nil, err
	}
	return []vendors.Zone{{ID: region + "-az1", Name: region + "a"}}, nil
}

var (
	errDenied    = vendors.NewError(vendors.RegionUnavailable, "UnauthorizedOperation", errors.New("explicit deny"))
	errOptIn     = vendors.NewError(vendors.RegionUnavailable, "OptInRequired", errors.New("not opted in"))
	errThrottled = vendors.NewError(vendors.Retryable, "RequestLimitExceeded", errors.New("slow down"))
	errBroken    = vendors.NewError(vendors.Fatal, "InvalidParameterValue", errors.New("bad"))
)

// capture returns a context whose logger records the lines (verbosity 0 only).
func capture(t *testing.T) (context.Context, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	l := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{})
	return log.IntoContext(t.Context(), l), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

func TestADeniedRegionIsSkippedAndTheOthersLoad(t *testing.T) {
	d := &regionDriver{}
	d.set("af-south-1", errDenied)
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	ctx, lines := capture(t)
	if err := f.Load(ctx); err != nil {
		t.Fatalf("a denied region failed the load: %v", err)
	}
	if !f.Loaded("aws") {
		t.Error("not loaded")
	}
	if got := f.Regions("aws"); !slices.Equal(got, []string{"eu-west-1", "us-east-2"}) {
		t.Errorf("regions %v", got)
	}
	zones, ok := f.Zones("aws")
	if !ok || len(zones["us-east-2"]) != 1 || len(zones["af-south-1"]) != 0 {
		t.Errorf("zones %v", zones)
	}
	if sk := f.SkippedRegions("aws"); len(sk) != 1 || !strings.Contains(sk["af-south-1"], "UnauthorizedOperation") {
		t.Errorf("skipped %v", sk)
	}
	if got := regionLines(lines()); len(got) != 1 {
		t.Errorf("want one warning naming the region, got %q", got)
	}
	// Later loads do not warn again (V(1) only), also when the message differs
	// (a real API error carries a new request ID every time).
	for i := range 3 {
		d.set("af-south-1", vendors.NewError(vendors.RegionUnavailable, "UnauthorizedOperation",
			fmt.Errorf("explicit deny, RequestID: %d", i)))
		if err := f.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := regionLines(lines()); len(got) != 1 {
		t.Errorf("the warning repeated: %q", got)
	}
	// The region becomes readable: it is loaded and the recovery is logged.
	d.set("af-south-1", nil)
	if err := f.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Regions("aws"); len(got) != 3 || len(f.SkippedRegions("aws")) != 0 {
		t.Errorf("regions %v, skipped %v", got, f.SkippedRegions("aws"))
	}
	if got := regionLines(lines()); len(got) != 2 || !strings.Contains(got[1], "again") {
		t.Errorf("recovery not logged: %q", got)
	}
}

func TestAnOptInRegionIsSkipped(t *testing.T) {
	d := &regionDriver{}
	d.set("af-south-1", errOptIn)
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	if err := f.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.Regions("aws"); len(got) != 2 || slices.Contains(got, "af-south-1") {
		t.Errorf("regions %v", got)
	}
}

func TestLoadFailsWhenNoRegionCanBeRead(t *testing.T) {
	d := &regionDriver{}
	for _, r := range []string{"af-south-1", "eu-west-1", "us-east-2"} {
		d.set(r, errDenied)
	}
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	err := f.Load(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no region could be read") {
		t.Fatalf("err %v", err)
	}
	if f.Loaded("aws") {
		t.Error("loaded")
	}
}

func TestATransientRegionErrorKeepsTheLastKnownZones(t *testing.T) {
	d := &regionDriver{}
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	if err := f.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.set("eu-west-1", errThrottled)
	err := f.Load(t.Context())
	if err == nil || !strings.Contains(err.Error(), "eu-west-1") {
		t.Fatalf("a transient error must still be reported so that the load is retried: %v", err)
	}
	zones, _ := f.Zones("aws")
	if len(zones["eu-west-1"]) != 1 || len(f.Regions("aws")) != 3 {
		t.Errorf("the last-known zones were dropped: %v", zones)
	}
	if !f.Loaded("aws") {
		t.Error("a transient region error must not unload the facts")
	}
}

func TestATransientErrorWithoutLastKnownZonesLeavesTheRegionWithoutZones(t *testing.T) {
	d := &regionDriver{}
	d.set("eu-west-1", errBroken)
	f := facts.New(facts.Options{Registry: vendors.NewRegistryFrom(d)})
	if err := f.Load(t.Context()); err == nil {
		t.Fatal("the failure was not reported")
	}
	// The region stays known (a NodeClass naming it stays Ready) but has no
	// zones, so no offerings.
	if got := f.Regions("aws"); !slices.Equal(got, []string{"af-south-1", "eu-west-1", "us-east-2"}) {
		t.Errorf("regions %v", got)
	}
	zones, _ := f.Zones("aws")
	if len(zones["eu-west-1"]) != 0 || len(zones["us-east-2"]) != 1 {
		t.Errorf("zones %v: the other regions must load", zones)
	}
}

// stuckDriver is a vendor whose Regions call blocks until the context ends
// while stuck is set.
type stuckDriver struct {
	driver
	name  string
	stuck atomic.Bool
	calls atomic.Int32
}

func (d *stuckDriver) Vendor() string { return d.name }

func (d *stuckDriver) Regions(ctx context.Context) ([]vendors.Region, error) {
	d.calls.Add(1)
	if d.stuck.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return d.driver.Regions(ctx)
}

func TestAStuckVendorTimesOutAndIsRetriedWithoutHidingTheOthers(t *testing.T) {
	good1, good2 := &stuckDriver{name: "aws"}, &stuckDriver{name: "gcp"}
	stuck := &stuckDriver{name: "hcloud"}
	stuck.stuck.Store(true)
	clk := clocktesting.NewFakeClock(time.Now())
	f := facts.New(facts.Options{
		Registry:      vendors.NewRegistryFrom(good1, good2, stuck),
		VendorTimeout: 200 * time.Millisecond,
		Clock:         clk,
	})
	ctx, lines := capture(t)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- f.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// The healthy vendors are loaded before the stuck one gives up.
	waitFor(t, func() bool { return f.Loaded("aws") && f.Loaded("gcp") })
	if f.Loaded("hcloud") {
		t.Error("the stuck vendor is loaded")
	}

	// The stuck vendor times out with an error naming it, and is retried.
	waitFor(t, func() bool { return slices.ContainsFunc(lines(), isStuckError) })
	if f.Loaded("hcloud") {
		t.Error("the stuck vendor is loaded after the timeout")
	}
	if got := stuck.calls.Load(); got != 1 {
		t.Fatalf("stuck vendor called %d times before the retry", got)
	}
	// Wait for Start to sleep, then let the retry interval pass.
	waitFor(t, clk.HasWaiters)
	stuck.stuck.Store(false)
	clk.Step(time.Minute)
	waitFor(t, func() bool { return f.Loaded("hcloud") })
	if got := stuck.calls.Load(); got < 2 {
		t.Errorf("the stuck vendor was not retried: %d calls", got)
	}
	if !slices.ContainsFunc(lines(), func(l string) bool {
		return strings.Contains(l, "loaded the driver facts of a vendor") && strings.Contains(l, `"vendor"="hcloud"`)
	}) {
		t.Errorf("no info line for the loaded vendor: %q", lines())
	}
}

func isStuckError(l string) bool {
	return strings.Contains(l, `"vendor"="hcloud"`) && true &&
		strings.Contains(l, "loading the driver facts of a vendor failed")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// regionLines keeps the log lines about a skipped region.
func regionLines(all []string) []string {
	return slices.DeleteFunc(all, func(l string) bool { return !strings.Contains(l, "af-south-1") })
}

// hangingRegions is a vendor whose Zones call blocks until the context ends
// for the regions in hung.
type hangingRegions struct {
	regionDriver
	mu2   sync.Mutex
	hung  map[string]bool
	calls map[string]int
}

func (d *hangingRegions) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	d.mu2.Lock()
	d.calls[region]++
	hung := d.hung[region]
	d.mu2.Unlock()
	if hung {
		<-ctx.Done()
		return nil, vendors.NewError(vendors.Retryable, "CallTimeout", ctx.Err())
	}
	return d.regionDriver.Zones(ctx, region)
}

func (d *hangingRegions) unhang(region string) {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	delete(d.hung, region)
}

func (d *hangingRegions) callsOf(region string) int {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	return d.calls[region]
}

func TestHangingRegionsDoNotFailTheVendorAndOnlyTheyAreRetried(t *testing.T) {
	d := &hangingRegions{hung: map[string]bool{"eu-west-1": true}, calls: map[string]int{}}
	other := &stuckDriver{name: "gcp"}
	clk := clocktesting.NewFakeClock(time.Now())
	f := facts.New(facts.Options{
		Registry:      vendors.NewRegistryFrom(d, other),
		RegionTimeout: 100 * time.Millisecond,
		Clock:         clk,
	})
	ctx, lines := capture(t)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- f.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// The vendor is published with the regions that loaded, the hung one
	// is logged once, and no other vendor waits.
	waitFor(t, func() bool { return f.Loaded("aws") && f.Loaded("gcp") })
	waitFor(t, func() bool {
		return slices.ContainsFunc(lines(), func(l string) bool {
			return strings.Contains(l, "some regions failed") && strings.Contains(l, `"vendor"="aws"`) &&
				strings.Contains(l, "eu-west-1") && !strings.Contains(l, "us-east-2")
		})
	})
	if got := f.Regions("aws"); len(got) != 3 {
		t.Errorf("regions %v: the failed region must stay known", got)
	}
	if zones, _ := f.Zones("aws"); len(zones["eu-west-1"]) != 0 || len(zones["af-south-1"]) != 1 {
		t.Errorf("zones %v", zones)
	}

	// The retry reads the failed region only.
	waitFor(t, clk.HasWaiters)
	d.unhang("eu-west-1")
	clk.Step(retryIntervalForTest)
	waitFor(t, func() bool { z, _ := f.Zones("aws"); return len(z["eu-west-1"]) == 1 })
	if got := d.callsOf("eu-west-1"); got != 2 {
		t.Errorf("eu-west-1 read %d times, want 2", got)
	}
	if got := d.callsOf("us-east-2"); got != 1 {
		t.Errorf("us-east-2 read %d times, want 1 (not retried)", got)
	}
}

// retryIntervalForTest is long enough to pass the retry wait of Start.
const retryIntervalForTest = time.Minute

// withHung builds a started Facts over a vendor whose eu-west-1 hangs and a
// healthy second vendor, on a fake clock.
func withHung(t *testing.T, interval, vendorTimeout time.Duration) (*facts.Facts, *hangingRegions, *clocktesting.FakeClock, func() []string) {
	t.Helper()
	d := &hangingRegions{hung: map[string]bool{"eu-west-1": true}, calls: map[string]int{}}
	clk := clocktesting.NewFakeClock(time.Now())
	f := facts.New(facts.Options{
		Registry:      vendors.NewRegistryFrom(d, &stuckDriver{name: "gcp"}),
		RegionTimeout: 100 * time.Millisecond,
		VendorTimeout: vendorTimeout,
		Interval:      interval,
		Clock:         clk,
	})
	ctx, lines := capture(t)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- f.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { return f.Loaded("aws") && f.Loaded("gcp") && d.callsOf("eu-west-1") == 1 })
	waitFor(t, clk.HasWaiters)
	return f, d, clk, lines
}

func TestAFailedRegionIsRetriedWithABackoff(t *testing.T) {
	_, d, clk, lines := withHung(t, time.Hour, time.Minute)
	// step moves the clock and waits for Start to finish what it woke for.
	step := func(by time.Duration, wantCalls int) {
		t.Helper()
		clk.Step(by)
		waitFor(t, clk.HasWaiters)
		if got := d.callsOf("eu-west-1"); got != wantCalls {
			t.Fatalf("after +%s: %d reads of the failed region, want %d", by, got, wantCalls)
		}
	}
	// t=0 failed (retry due at 10 s), t=10 failed (due at 30), t=30 failed (due at 70).
	step(5*time.Second, 1)
	step(5*time.Second, 2)
	step(10*time.Second, 2)
	step(10*time.Second, 3)
	step(30*time.Second, 3)
	step(10*time.Second, 4)
	// The failed set did not change: one Error line only.
	n := 0
	for _, l := range lines() {
		if strings.Contains(l, "some regions failed") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d error lines for an unchanged failed set, want 1", n)
	}
}

func TestTheFullRefreshRunsWhileARegionKeepsFailing(t *testing.T) {
	f, d, clk, _ := withHung(t, time.Hour, time.Minute)
	if got := d.callsOf("us-east-2"); got != 1 {
		t.Fatalf("healthy region read %d times at the start", got)
	}
	for range 5 { // retries alone never read the healthy regions
		clk.Step(15 * time.Minute)
		waitFor(t, clk.HasWaiters)
	}
	if got := d.callsOf("us-east-2"); got < 2 {
		t.Errorf("the healthy region was not read again after an hour: %d reads", got)
	}
	if !f.Loaded("aws") {
		t.Error("not loaded")
	}
}

func TestAVendorTimeoutKeepsTheRegionsThatLoaded(t *testing.T) {
	d := &hangingRegions{hung: map[string]bool{"eu-west-1": true}, calls: map[string]int{}}
	f := facts.New(facts.Options{
		Registry:      vendors.NewRegistryFrom(d),
		RegionTimeout: time.Minute,
		VendorTimeout: 100 * time.Millisecond,
	})
	err := f.Load(t.Context())
	if err == nil {
		t.Fatal("the timeout was not reported")
	}
	if !f.Loaded("aws") {
		t.Fatal("the regions that loaded were dropped")
	}
	if zones, _ := f.Zones("aws"); len(zones["us-east-2"]) != 1 || len(zones["eu-west-1"]) != 0 {
		t.Errorf("zones %v", zones)
	}
}
