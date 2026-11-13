/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection"
	selapi "github.com/SpareCores/sc-keeper-go/selection/api"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// The equivalence of the local selection and Keeper's /servers on the recorded
// data of test/data/sc-snapshot (plan 13, part A). The snapshot holds the
// /servers responses of one region per vendor (and of three AWS regions) for
// three benchmarks, on-demand basis; the local builder reads the same
// snapshot's tables, /server_prices and the vendor-wide scores. The documented
// differences are in the section "Part A equivalence" of
// docs/plans/v1/wip/13-keeper-catalog-snapshot.md and in the comments below.

type equivCase struct {
	name   string
	vendor string
	names  []string // regions as the NodeClass names them
	ids    []string // Keeper's region ids, as /servers takes them
}

var equivCases = []equivCase{
	{name: "aws one region", vendor: "aws", names: []string{"us-east-1"}, ids: []string{"us-east-1"}},
	{name: "aws three regions", vendor: "aws", names: []string{"eu-central-1", "eu-west-1", "us-east-1"},
		ids: []string{"eu-central-1", "eu-west-1", "us-east-1"}},
	{name: "gcp", vendor: "gcp", names: []string{"europe-west3"}, ids: []string{"1300"}},
	{name: "hcloud", vendor: "hcloud", names: []string{"fsn1"}, ids: []string{"fsn1"}},
}

// difference explains why a type of the /servers path may be missing from the
// local candidates (and so from the members); "" means unexplained. The two
// known reasons are properties of the catalog's data, not of the local builder:
//   - architecture: i386 and the *_mac types have no Kubernetes architecture;
//     the catalog does not hold them, /servers lists them, and Evaluate
//     excludes them as provider-ineligible either way;
//   - retiring: the anonymous /server_prices lists the prices of active servers
//     only (only_active defaults to true there), so a planned-for-retirement
//     type has no price in the catalog, while /servers prices it from
//     ServerPrice directly. It has no offering in the catalog either, so it is
//     not launchable and its labels do not change. Part B of plan 13 reads
//     the snapshot's server_price table, which has these prices.
func difference(t *testing.T, f *keeper.Fetcher, data *model.Data, vendor, name string) string {
	t.Helper()
	servers, err := f.TableServers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range servers {
		s := &servers[i]
		if s.VendorID != vendor || s.APIReference != name {
			continue
		}
		if s.CPUArchitecture != "x86_64" && s.CPUArchitecture != "arm64" {
			return "architecture"
		}
		if s.Status == "planned-for-retirement" {
			for i := range data.Prices {
				if data.Prices[i].Vendor == vendor && data.Prices[i].ServerID == s.ServerID {
					return ""
				}
			}
			return "retiring"
		}
	}
	return ""
}

func benchKey(t *testing.T, cat *keeper.BenchmarkCatalog, id, config string) keeper.BenchmarkKey {
	t.Helper()
	var raw []byte
	if config != "" {
		raw = []byte(config)
	}
	k, err := cat.Resolve(id, raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The recorded benchmarks (on-demand, per region).
var equivBenchmarks = []struct{ id, config string }{
	{"geekbench:score", `{"cores":"single"}`},
	{"stress_ng:bestn", ""},
	{"membench:latency", `{"scope":"RAM"}`},
}

// Candidate by candidate: the types of the /servers query of one region and
// benchmark, their prices and their scores equal what the local builder
// derives from the tables, prices and vendor-wide scores of the same snapshot.
func TestLocalCandidatesEqualTheServersRows(t *testing.T) {
	srv := catalogtest.Server(t)
	f := keeper.NewFetcher(keeper.New(srv.URL))
	cat, err := f.BenchmarkCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range equivCases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := catalogtest.SourceAt(srv.URL).Load(t.Context(), catalog.Request{Scope: catalog.Scope{tc.vendor: tc.names}})
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range equivBenchmarks {
				key := benchKey(t, cat, b.id, b.config)
				if tc.name == "aws three regions" && b.id != "geekbench:score" {
					continue // only that benchmark is recorded for three regions
				}
				rows, err := f.Servers(t.Context(), keeper.Query{Vendor: tc.vendor, Regions: tc.ids, Allocation: keeper.AllocationOnDemand, Benchmark: &key})
				if err != nil {
					t.Fatal(err)
				}
				scores, err := catalog.BenchmarkScores(t.Context(), f, tc.vendor, key, keeper.AllocationOnDemand)
				if err != nil {
					t.Fatal(err)
				}
				cands := catalog.SelectionCandidates(data, tc.vendor, tc.ids,
					map[keeper.BenchmarkKey]map[string]float64{key: scores}, keeper.AllocationOnDemand)
				compareCandidates(t, tc.name+"/"+b.id, rows, cands, key, func(n string) string {
					return difference(t, f, data, tc.vendor, n)
				})
			}
		})
	}
}

func compareCandidates(t *testing.T, name string, rows []keeper.Server, cands []*selection.Candidate, key keeper.BenchmarkKey,
	explain func(string) string) {
	t.Helper()
	want := map[string]*keeper.Server{}
	for i := range rows {
		// /servers with a best_price_allocation hides types without a price of
		// it, with or without regions, so every row has one.
		want[rows[i].APIReference] = &rows[i]
	}
	got := map[string]*selection.Candidate{}
	for _, c := range cands {
		got[c.Name()] = c
	}
	var onlyKeeper, onlyLocal, price, score []string
	for _, n := range slices.Sorted(maps.Keys(want)) {
		c, ok := got[n]
		if !ok {
			if reason := explain(n); reason != "" {
				t.Logf("%s: %s is only in /servers (%s)", name, n, reason)
				continue
			}
			onlyKeeper = append(onlyKeeper, n)
			continue
		}
		w := want[n]
		if w.Price == nil || math.Abs(*w.Price-c.Prices[keeper.AllocationOnDemand]) > 1e-9 {
			price = append(price, fmt.Sprintf("%s: /servers %v, local %v", n, deref(w.Price), c.Prices[keeper.AllocationOnDemand]))
		}
		gs, ok := c.Scores[key]
		if (w.SelectedBenchmarkScore == nil) != !ok || (ok && math.Abs(*w.SelectedBenchmarkScore-gs) > 1e-9) {
			score = append(score, fmt.Sprintf("%s: /servers %v, local %v", n, deref(w.SelectedBenchmarkScore), gs))
		}
	}
	for n := range got {
		if want[n] == nil {
			onlyLocal = append(onlyLocal, n)
		}
	}
	slices.Sort(onlyLocal)
	if len(onlyKeeper)+len(onlyLocal)+len(price)+len(score) > 0 {
		t.Errorf("%s: /servers has %d types, local %d\nonly /servers: %v\nonly local: %v\nprices: %v\nscores: %v",
			name, len(want), len(got), onlyKeeper, onlyLocal, price, score)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// equivSpecs are selection profiles over one benchmark, the shape every test
// and the L3 NodeClasses use: the old path needs the recorded base query,
// which only a single-benchmark profile has (one merged query).
func equivSpecs() map[string]selapi.Spec {
	geek := benchmark("geekbench:score", `{"cores":"single"}`)
	bestn := benchmark("stress_ng:bestn", "")
	latency := benchmark("membench:latency", `{"scope":"RAM"}`)
	two, three, six := int32(2), int32(3), int32(6)
	mn, mx := 1500.0, 900.0
	rank := func(b *selapi.BenchmarkRef, o selapi.Objective, band float64) *selapi.Ranking {
		return &selapi.Ranking{Score: selapi.RankingScore{Benchmark: b}, Objective: o, Band: &selapi.RankingBand{WithinPercent: band}}
	}
	fast := rank(geek, selapi.ObjectiveScore, 12)
	fast.Families = &selapi.RankingFamilies{Min: &three, Max: &six}
	return map[string]selapi.Spec{
		"fast-single-core": {Selector: &selapi.Selector{VCPUs: &selapi.IntRange{Min: &two}}, Ranking: fast},
		"cheap-fast":       {Selector: &selapi.Selector{VCPUs: &selapi.IntRange{Min: &two}}, Ranking: rank(geek, selapi.ObjectiveScorePerPrice, 20)},
		"cheapest":         {Ranking: rank(geek, selapi.ObjectivePrice, 50)},
		"stress":           {Ranking: rank(bestn, selapi.ObjectiveScore, 10)},
		"lowest-latency":   {Ranking: rank(latency, selapi.ObjectiveScore, 15)},
		"geekbench-min":    {Selector: &selapi.Selector{Benchmarks: []selapi.BenchmarkFilter{{BenchmarkRef: *geek, Min: &mn}}}},
		"geekbench-min-include": {Selector: &selapi.Selector{Unbenchmarked: selapi.UnbenchmarkedInclude,
			Benchmarks: []selapi.BenchmarkFilter{{BenchmarkRef: *geek, Min: &mn}}}},
		"geekbench-min-family": {Selector: &selapi.Selector{Unbenchmarked: selapi.UnbenchmarkedIncludeIfFamilyQualifies,
			Benchmarks: []selapi.BenchmarkFilter{{BenchmarkRef: *geek, Min: &mn}}}},
		"latency-max": {Selector: &selapi.Selector{Benchmarks: []selapi.BenchmarkFilter{{BenchmarkRef: *latency, Max: &mx}}}},
		"per-vcpu": {Selector: &selapi.Selector{Benchmarks: []selapi.BenchmarkFilter{
			{BenchmarkRef: *geek, Min: &mx, Normalize: selapi.NormalizePerVcpu}}}},
		"price-capped": {Selector: &selapi.Selector{Price: &selapi.PriceSelector{MaxHourly: "0.2"},
			Benchmarks: []selapi.BenchmarkFilter{{BenchmarkRef: *geek, Min: &mx}}}},
		"arm-only": {Selector: &selapi.Selector{Architectures: []selapi.Architecture{selapi.ArchitectureARM64}},
			Ranking: rank(geek, selapi.ObjectiveScore, 30)},
	}
}

// Profile by profile: the members of the local selection equal those of the
// /servers path (PlanQueries, BuildCandidates, Evaluate), and so do their
// ranking scores.
func TestLocalSelectionEqualsTheServersPath(t *testing.T) {
	srv := catalogtest.Server(t)
	f := keeper.NewFetcher(keeper.New(srv.URL))
	cat, err := f.BenchmarkCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	opts := selection.Options{Catalog: cat, DefaultBasis: selapi.PriceBasisOnDemand}
	specs := equivSpecs()
	// The report: selections whose members and ranking scores are identical,
	// and those with members only /servers selects, by reason.
	var identical, differing int
	reasons := map[string]int{}
	t.Cleanup(func() {
		t.Logf("equivalence report: %d selections identical, %d differ (members only in /servers by reason: %v)",
			identical, differing, reasons)
	})
	for _, tc := range equivCases {
		for _, name := range slices.Sorted(maps.Keys(specs)) {
			spec := specs[name]
			if tc.name == "aws three regions" && name != "fast-single-core" && name != "cheap-fast" && name != "cheapest" &&
				name != "geekbench-min" {
				continue // recorded for geekbench single only
			}
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				plan, err := selection.PlanQueries(&spec, tc.vendor, tc.ids, opts)
				if err != nil {
					t.Fatal(err)
				}
				results, err := f.ServersAll(t.Context(), plan)
				if err != nil {
					t.Fatal(err)
				}
				want, err := selection.Evaluate(&spec, selection.BuildCandidates(results), opts)
				if err != nil {
					t.Fatal(err)
				}

				id := catalog.SelectionQueryID(tc.vendor, tc.names, &spec)
				data, err := catalogtest.SourceAt(srv.URL).Load(t.Context(), catalog.Request{
					Scope:      catalog.Scope{tc.vendor: tc.names},
					Selections: []catalog.SelectionQuery{{ID: id, Vendor: tc.vendor, Regions: tc.names, Spec: spec}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if reason, bad := data.Invalid[id]; bad {
					t.Fatalf("invalid: %s", reason)
				}
				got := data.Selected[id]
				if len(want.InstanceTypes) == 0 {
					t.Fatalf("the /servers path selects nothing: the case proves nothing")
				}
				explained := compareMembers(t, want, got, func(n string) string { return difference(t, f, data, tc.vendor, n) })
				if len(explained) == 0 {
					identical++
				} else {
					differing++
				}
				for reason, n := range explained {
					reasons[reason] += n
				}
			})
		}
	}
}

// compareMembers fails on an unexplained difference and returns the explained
// ones by reason.
func compareMembers(t *testing.T, want selection.Result, got map[string]model.Member, explain func(string) string) map[string]int {
	t.Helper()
	explained := map[string]int{}
	var missing, extra, scores []string
	wantSet := map[string]*float64{}
	for _, it := range want.InstanceTypes {
		wantSet[it.Name] = it.Score
		m, ok := got[it.Name]
		if !ok {
			if reason := explain(it.Name); reason != "" {
				t.Logf("%s is only selected by /servers (%s)", it.Name, reason)
				explained[reason]++
				continue
			}
			missing = append(missing, it.Name)
			continue
		}
		if (it.Score == nil) != (m.Score == nil) || (it.Score != nil && math.Abs(*it.Score-*m.Score) > 1e-9) {
			scores = append(scores, it.Name)
		}
	}
	for n := range got {
		if _, ok := wantSet[n]; !ok {
			extra = append(extra, n)
		}
	}
	slices.Sort(extra)
	if len(missing)+len(extra)+len(scores) > 0 {
		t.Errorf("/servers selects %d types, local %d\nonly /servers: %v\nonly local: %v\nscore differs: %v",
			len(want.InstanceTypes), len(got), missing, extra, scores)
	}
	return explained
}
