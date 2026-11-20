/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
)

// The instance types of the fixtures must not change when the reasons are
// computed: this digest was taken before the catalog kept them.
func TestInstanceTypesOfTheFixturesAreUnchanged(t *testing.T) {
	facts := catalogtestFacts(t)
	facts.RootDisk = map[string]int64{"aws": 128 << 30}
	cl := catalog.NewClass("c", catalog.Scope{"aws": nil, "hcloud": nil})
	cl.FlatcarVersion = catalogtest.FlatcarVersion
	cl.Finalize()
	c := catalog.New(catalog.Options{Source: sourceFor(t), Facts: facts})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	its := all(t, c, catalog.Filter{})
	for _, it := range its {
		h.Write([]byte(dump(it)))
	}
	testutil.Golden(t, "instance-types-digest.golden", fmt.Appendf(nil, "%d types, sha256 %x\n", len(its), h.Sum(nil)))
}

// reasons maps "type" and "type/zone/capacity" to the reason code of a report.
func reasons(t testing.TB, c *catalog.Catalog) map[string]catalog.Reason {
	t.Helper()
	rep, err := c.Explain(t.Context(), catalog.Query{Class: "c"})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]catalog.Reason{}
	for _, tr := range rep.Types {
		if tr.Dropped.Code != "" {
			out[tr.InstanceType] = tr.Dropped
		}
		for _, o := range tr.Offerings {
			if o.Reason.Code != "" {
				out[tr.InstanceType+"/"+o.Zone+"/"+o.CapacityType] = o.Reason
			}
		}
	}
	return out
}

func wantReasons(t *testing.T, got, want map[string]catalog.Reason) {
	t.Helper()
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("%s: reason %+v (present %v), want %+v", k, g, ok, w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected reason %+v", k, g)
		}
	}
}

// launchFacts says which (type, zone ID) cannot launch.
type launchFacts struct {
	catalog.StaticFacts
	no map[string]bool
}

func (f launchFacts) Launchable(_, apiReference, zoneID string) (bool, bool) {
	if f.no[apiReference+"/"+zoneID] {
		return false, true
	}
	return true, true
}

func TestBuildReasons(t *testing.T) {
	priced := func(id, ref, zone string) (model.Server, model.Price) {
		return model.Server{Vendor: "v", ID: id, APIReference: ref, VCPUs: 2, Arch: "amd64", MemoryMiB: 4096},
			model.Price{Vendor: "v", RegionID: "r1", ZoneID: zone, ServerID: id, CapacityType: model.CapacityOnDemand, USDPerHour: 0.2}
	}
	add := func(d *model.Data, ref, zone string) {
		s, p := priced(ref, ref, zone)
		d.Servers = append(d.Servers, s)
		d.Prices = append(d.Prices, p)
	}
	cases := map[string]struct {
		mutate func(*model.Data)
		facts  catalog.Facts
		scope  catalog.Scope
		noInc  bool
		want   map[string]catalog.Reason
	}{
		"NoPrice": {
			mutate: func(d *model.Data) {
				d.Servers = append(d.Servers, model.Server{Vendor: "v", ID: "9", APIReference: "unpriced", VCPUs: 1, Arch: "amd64"})
			},
			want: map[string]catalog.Reason{"v.unpriced": {Code: catalog.NoPrice}},
		},
		"GPU": {
			mutate: func(d *model.Data) {
				add(d, "gpu", "z1")
				d.Servers[len(d.Servers)-1].AcceleratorType = model.AcceleratorGPU
				d.Servers[len(d.Servers)-1].AcceleratorCount = 1
				d.Servers[len(d.Servers)-1].AcceleratorManufacturer = "AMD"
			},
			want: map[string]catalog.Reason{"v.gpu": {Code: "GPUManufacturer"}},
		},
		"RegionNotAllowed": {
			mutate: func(d *model.Data) {
				d.Regions = append(d.Regions, model.Region{Vendor: "v", ID: "r2", Name: "region-2", Active: true})
				add(d, "elsewhere", "")
				d.Prices[len(d.Prices)-1].RegionID = "r2"
			},
			scope: catalog.Scope{"v": {"region-1"}},
			want:  map[string]catalog.Reason{"v.elsewhere": {Code: catalog.RegionNotAllowed, Detail: "r2"}},
		},
		"ZoneNotAvailable: a zone the account lacks, every row": {
			mutate: func(d *model.Data) { add(d, "ghost", "z9") },
			want: map[string]catalog.Reason{
				"v.ghost": {Code: catalog.NoOffering, Detail: string(catalog.ZoneNotAvailable)},
			},
		},
		"ZoneNotAvailable: one zone of a type": {
			mutate: func(d *model.Data) {
				add(d, "partial", "z1")
				d.Prices = append(d.Prices, model.Price{Vendor: "v", RegionID: "r1", ZoneID: "z9", ServerID: "partial",
					CapacityType: model.CapacityOnDemand, USDPerHour: 0.2})
			},
			want: map[string]catalog.Reason{"v.partial/z9/on-demand": {Code: catalog.ZoneNotAvailable, Detail: "z9"}},
		},
		"ZoneNotAvailable: Keeper marked the zone inactive": {
			mutate: func(d *model.Data) { d.Zones[0].Active = false },
			want: map[string]catalog.Reason{
				"v.big":                {Code: catalog.NoOffering, Detail: string(catalog.ZoneNotAvailable)},
				"v.small/z1/on-demand": {Code: catalog.ZoneNotAvailable, Detail: "z1"},
				"v.small/z1/spot":      {Code: catalog.ZoneNotAvailable, Detail: "z1"},
			},
		},
		"ZoneNotAvailable: the driver lists no zones": {
			facts: catalog.StaticFacts{ZonesByVendor: map[string]map[string][]catalog.Zone{"v": {}}},
			want: map[string]catalog.Reason{
				"v.small": {Code: catalog.NoOffering, Detail: string(catalog.ZoneNotAvailable)},
				"v.big":   {Code: catalog.NoOffering, Detail: string(catalog.ZoneNotAvailable)},
			},
		},
		"IncompletePrice": {
			facts: incompleteFacts{syntheticFacts()},
			noInc: true,
			want: map[string]catalog.Reason{
				"v.small/z2/on-demand": {Code: catalog.IncompletePrice, Detail: "z2 on-demand"},
			},
		},
		"NotLaunchable and NoOffering": {
			facts: launchFacts{syntheticFacts(), map[string]bool{"small/z2": true, "big/z1": true}},
			want: map[string]catalog.Reason{
				"v.small/z2/on-demand": {Code: catalog.NotLaunchable, Detail: "z2"},
				"v.big":                {Code: catalog.NoOffering, Detail: string(catalog.NotLaunchable)},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data := synthetic()
			if tc.mutate != nil {
				tc.mutate(data)
			}
			facts := tc.facts
			if facts == nil {
				facts = syntheticFacts()
			}
			scope := tc.scope
			if scope == nil {
				scope = catalog.Scope{"v": nil}
			}
			cl := catalog.NewClass("c", scope)
			cl.AllowIncompletePrice = !tc.noInc
			cl.Finalize()
			c := catalog.New(catalog.Options{Source: &fakeSource{data: data}, Facts: facts})
			c.SetClasses([]catalog.Class{cl})
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := reasons(t, c)
			wantReasons(t, got, tc.want)
		})
	}
}

// codeGate answers with a fixed reason for the offerings of a zone.
type codeGate struct {
	zone   string
	reason catalog.Reason
}

func (g codeGate) Unavailable(_, _, _, zone, _ string) (catalog.Reason, bool) {
	return g.reason, zone == g.zone
}

func (g codeGate) UnavailableFor(_, _, _, _, zone, _ string) (catalog.Reason, bool) {
	return g.reason, zone == g.zone
}

func TestAvailabilityReasons(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	cases := map[string]struct {
		opts func(*unavailable.Cache) catalog.Options
		want catalog.Reason
	}{
		"UnavailableCache": {
			opts: func(un *unavailable.Cache) catalog.Options {
				un.MarkInsufficientCapacity("v", "v.small", "zone-1", "on-demand")
				return catalog.Options{}
			},
			want: catalog.Reason{Code: catalog.UnavailableCache, Detail: "InsufficientCapacity until 2026-11-20T12:03:00Z"},
		},
		"QuotaGate": {
			opts: func(*unavailable.Cache) catalog.Options {
				return catalog.Options{Gate: codeGate{"zone-1", catalog.Reason{Code: catalog.QuotaGate, Detail: "cores, headroom 0, need 2"}}}
			},
			want: catalog.Reason{Code: catalog.QuotaGate, Detail: "cores, headroom 0, need 2"},
		},
		"ImageGap": {
			opts: func(*unavailable.Cache) catalog.Options {
				return catalog.Options{ClassGate: codeGate{"zone-1", catalog.Reason{Code: catalog.ImageGap, Detail: "region-1"}}}
			},
			want: catalog.Reason{Code: catalog.ImageGap, Detail: "region-1"},
		},
		"RegionNotReady": {
			opts: func(*unavailable.Cache) catalog.Options {
				return catalog.Options{ClassGate: codeGate{"zone-1", catalog.Reason{Code: catalog.RegionNotReady, Detail: "region-1"}}}
			},
			want: catalog.Reason{Code: catalog.RegionNotReady, Detail: "region-1"},
		},
		"ZoneMissingResource": {
			opts: func(*unavailable.Cache) catalog.Options {
				return catalog.Options{ClassGate: codeGate{"zone-1", catalog.Reason{Code: catalog.ZoneMissingResource, Detail: "zone-1"}}}
			},
			want: catalog.Reason{Code: catalog.ZoneMissingResource, Detail: "zone-1"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			un := unavailable.New(clk, unavailable.TTLs{})
			opts := tc.opts(un)
			opts.Source, opts.Facts, opts.Clock, opts.Unavailable = &fakeSource{data: synthetic()}, syntheticFacts(), clk, un
			c := catalog.New(opts)
			c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil})})
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := reasons(t, c)
			if tc.want.Code == catalog.UnavailableCache {
				// Only the marked offering.
				wantReasons(t, got, map[string]catalog.Reason{"v.small/zone-1/on-demand": tc.want})
				return
			}
			for _, k := range []string{"v.small/zone-1/on-demand", "v.small/zone-1/spot", "v.big/zone-1/on-demand"} {
				if got[k] != tc.want {
					t.Errorf("%s: %+v, want %+v", k, got[k], tc.want)
				}
			}
			// zone-2 is not gated.
			if r, ok := got["v.small/zone-2/on-demand"]; ok {
				t.Errorf("zone-2: %+v", r)
			}
		})
	}
}

// Explain and InstanceTypes agree on every offering, whatever the gates say.
func TestExplainAgreesWithInstanceTypes(t *testing.T) {
	for seed := range uint64(8) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 7))
			clk := clocktesting.NewFakeClock(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
			un := unavailable.New(clk, unavailable.TTLs{})
			gate := &randomGate{rng: rng, down: map[string]catalog.Reason{}}
			facts := catalogtestFacts(t)
			cl := catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1", "eu-central-1"}})
			cl.FlatcarVersion = catalogtest.FlatcarVersion
			cl.Finalize()
			c := catalog.New(catalog.Options{Source: sourceFor(t), Facts: facts, Clock: clk, Unavailable: un, Gate: gate, ClassGate: gate})
			c.SetClasses([]catalog.Class{cl})
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			its := all(t, c, catalog.Filter{})
			for range 40 {
				it := its[rng.IntN(len(its))]
				o := it.Offerings[rng.IntN(len(it.Offerings))]
				if rng.IntN(2) == 0 {
					un.MarkInterrupted("aws", it.Name, o.Zone(), o.CapacityType())
				}
			}
			gate.arm()
			its = all(t, c, catalog.Filter{})
			rep, err := c.Explain(t.Context(), catalog.Query{Class: "c"})
			if err != nil {
				t.Fatal(err)
			}
			fromReport := map[string]bool{}
			for _, tr := range rep.Types {
				for _, o := range tr.Offerings {
					if !o.Removed {
						if o.Available != (o.Reason.Code == "") {
							t.Fatalf("%s: available %v with reason %+v", tr.InstanceType, o.Available, o.Reason)
						}
						fromReport[tr.InstanceType+"/"+o.Zone+"/"+o.CapacityType] = o.Available
					}
				}
			}
			n, down := 0, 0
			for _, it := range its {
				for _, o := range it.Offerings {
					key := it.Name + "/" + o.Zone() + "/" + o.CapacityType()
					want, ok := fromReport[key]
					if !ok || want != o.Available {
						t.Fatalf("%s: InstanceTypes available=%v, report %v (present %v)", key, o.Available, want, ok)
					}
					n++
					if !o.Available {
						down++
					}
				}
			}
			if n != len(fromReport) {
				t.Errorf("%d offerings in the report, %d in InstanceTypes", len(fromReport), n)
			}
			if down == 0 || down == n {
				t.Errorf("%d of %d offerings unavailable: the fixture does not exercise the gates", down, n)
			}
		})
	}
}

// randomGate marks a random share of the offerings it is asked about.
type randomGate struct {
	mu    sync.Mutex
	rng   *rand.Rand
	armed bool
	down  map[string]catalog.Reason
}

func (g *randomGate) arm() { g.armed = true }

func (g *randomGate) decide(key string, code catalog.Code) (catalog.Reason, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.armed {
		return catalog.Reason{}, false
	}
	key = string(code) + key
	r, seen := g.down[key]
	if !seen {
		// The answer is remembered either way: a gate answers the same twice.
		if g.rng.IntN(5) == 0 {
			r = catalog.Reason{Code: code, Detail: "x"}
		}
		g.down[key] = r
	}
	return r, r.Code != ""
}

func (g *randomGate) Unavailable(vendor, ref, region, zone, ct string) (catalog.Reason, bool) {
	return g.decide(strings.Join([]string{vendor, ref, region, zone, ct}, "/"), catalog.QuotaGate)
}

func (g *randomGate) UnavailableFor(class, vendor, ref, region, zone, ct string) (catalog.Reason, bool) {
	return g.decide(strings.Join([]string{class, vendor, ref, region, zone, ct}, "/"), catalog.ImageGap)
}

// logCatalog is a catalog over the synthetic data whose logger records the
// lines at V(1).
type logCatalog struct {
	*catalog.Catalog
	un    *unavailable.Cache
	clk   *clocktesting.FakeClock
	ctx   context.Context
	mu    sync.Mutex
	lines []string
}

func newLogCatalog(t *testing.T, data *model.Data) *logCatalog {
	t.Helper()
	return newLogCatalogFacts(t, data, syntheticFacts())
}

func newLogCatalogFacts(t *testing.T, data *model.Data, facts catalog.Facts) *logCatalog {
	t.Helper()
	l := &logCatalog{clk: clocktesting.NewFakeClock(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))}
	l.un = unavailable.New(l.clk, unavailable.TTLs{})
	logger := funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, args)
	}, funcr.Options{Verbosity: 1})
	l.ctx = logr.NewContext(t.Context(), logger)
	l.Catalog = catalog.New(catalog.Options{Source: &fakeSource{data: data}, Facts: facts,
		Clock: clock.Clock(l.clk), Unavailable: l.un})
	l.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil})})
	if err := l.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *logCatalog) query(t *testing.T, f catalog.Filter) []*cloudprovider.InstanceType {
	t.Helper()
	its, err := l.InstanceTypes(l.ctx, catalog.Query{Class: "c", Filter: f})
	if err != nil {
		t.Fatal(err)
	}
	return its
}

// take returns and clears the lines logged so far.
func (l *logCatalog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	l.lines = nil
	return out
}

func wantLine(t *testing.T, lines []string, parts ...string) {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1: %q", len(lines), lines)
	}
	for _, p := range parts {
		if !strings.Contains(lines[0], p) {
			t.Errorf("line %q lacks %q", lines[0], p)
		}
	}
}

func TestOfferedTypesAreLoggedOnTransitionsOnly(t *testing.T) {
	data := synthetic()
	data.Servers = append(data.Servers, model.Server{Vendor: "v", ID: "9", APIReference: "unpriced", VCPUs: 1, Arch: "amd64"})
	l := newLogCatalog(t, data)

	// The first report names what is not offered, one line per reason.
	l.query(t, catalog.Filter{})
	wantLine(t, l.take(), `"reason"="NoPrice"`, `v.unpriced`, `"count"=1`)

	// Queries of the same generation, with any filter, log nothing.
	l.query(t, catalog.Filter{})
	l.query(t, catalog.NewFilter(scheduling.NewRequirements(req(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"))))
	if lines := l.take(); len(lines) != 0 {
		t.Fatalf("lines for an unchanged generation: %q", lines)
	}

	l.un.MarkInsufficientCapacity("v", "v.small", "zone-1", "on-demand")
	l.query(t, catalog.Filter{})
	l.query(t, catalog.Filter{})
	wantLine(t, l.take(), `"type"="v.small"`, `"zone"="zone-1"`, `"capacityType"="on-demand"`,
		`"reason"="UnavailableCache"`, `InsufficientCapacity until`)

	l.clk.Step(4 * time.Minute)
	l.query(t, catalog.Filter{})
	wantLine(t, l.take(), `"type"="v.small"`, `"reason"="Available"`, `"was"="UnavailableCache"`)
}

func TestABurstOfChangesIsLoggedPerReason(t *testing.T) {
	data := synthetic()
	for i := range 120 {
		id := fmt.Sprintf("t%d", i)
		data.Servers = append(data.Servers, model.Server{Vendor: "v", ID: id, APIReference: id, VCPUs: 2, Arch: "amd64", MemoryMiB: 1024})
		data.Prices = append(data.Prices, model.Price{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: id,
			CapacityType: model.CapacityOnDemand, USDPerHour: 1})
	}
	l := newLogCatalog(t, data)
	l.query(t, catalog.Filter{})
	if lines := l.take(); len(lines) != 0 {
		t.Fatalf("lines for a catalog with nothing to report: %q", lines)
	}
	for i := range 120 {
		l.un.MarkInsufficientCapacity("v", fmt.Sprintf("v.t%d", i), "zone-1", "on-demand")
	}
	l.query(t, catalog.Filter{})
	lines := l.take()
	// 120 types in chunks of 50.
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3 (one reason, 120 types in chunks of 50): %q", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, `"reason"="UnavailableCache"`) || !strings.Contains(line, `"count"=120`) {
			t.Errorf("line %q", line)
		}
	}
	if !slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, "v.t119") }) {
		t.Error("a type is not named")
	}
}

// A deleted class is forgotten: when it comes back its first report is logged
// again in full.
func TestLogStateIsDroppedWithTheClass(t *testing.T) {
	data := synthetic()
	data.Servers = append(data.Servers, model.Server{Vendor: "v", ID: "9", APIReference: "unpriced", VCPUs: 1, Arch: "amd64"})
	l := newLogCatalog(t, data)
	l.query(t, catalog.Filter{})
	if got := l.take(); len(got) != 1 {
		t.Fatalf("first report: %q", got)
	}
	l.SetClasses(nil)
	l.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil})})
	l.query(t, catalog.Filter{})
	wantLine(t, l.take(), `v.unpriced`)
}

// loadingFacts are facts that are not loaded until load is called: no zones
// before, as with the driver facts right after a start.
type loadingFacts struct {
	catalog.StaticFacts
	loaded atomic.Bool
	epoch  atomic.Uint64
}

func (f *loadingFacts) Zones(vendor string) (map[string][]catalog.Zone, bool) {
	if !f.loaded.Load() {
		return map[string][]catalog.Zone{}, true
	}
	return f.StaticFacts.Zones(vendor)
}

func (f *loadingFacts) Loaded() bool  { return f.loaded.Load() }
func (f *loadingFacts) Epoch() uint64 { return f.epoch.Load() }
func (f *loadingFacts) load() {
	f.loaded.Store(true)
	f.epoch.Add(1)
}

// Right after a start the facts are not loaded and every type would be reported
// as ZoneNotAvailable: nothing is logged until they are, and then the first
// report has nothing to say for a healthy catalog.
func TestNothingIsLoggedBeforeTheFactsLoad(t *testing.T) {
	facts := &loadingFacts{StaticFacts: syntheticFacts()}
	l := newLogCatalogFacts(t, synthetic(), facts)
	l.query(t, catalog.Filter{})
	if lines := l.take(); len(lines) != 0 {
		t.Fatalf("lines before the facts are loaded: %q", lines)
	}
	facts.load()
	l.query(t, catalog.Filter{})
	if lines := l.take(); len(lines) != 0 {
		t.Fatalf("lines for a healthy catalog after the load: %q", lines)
	}
}
