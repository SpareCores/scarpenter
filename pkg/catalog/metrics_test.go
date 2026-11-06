/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	selapi "github.com/SpareCores/sc-keeper-go/selection/api"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

func benchmark(id, config string) *selapi.BenchmarkRef {
	ref := &selapi.BenchmarkRef{ID: id}
	if config != "" {
		ref.Config = &apiextensionsv1.JSON{Raw: []byte(config)}
	}
	return ref
}

// metricClass is a class over the test vendors with Geekbench single and multi
// core (per vCPU for multi), stress-ng, a lower-is-better latency and one
// selection profile.
func metricClass() catalog.Class {
	cl := catalog.NewClass("c", catalog.Scope{"aws": nil, "hcloud": {"fsn1"}, "gcp": {"europe-west3"}})
	two, scale := int32(2), int32(100)
	three, six := int32(3), int32(6)
	cl.Metrics = map[string]v1alpha1.Metric{
		"single-core": {Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)},
		"multi-core":  {Benchmark: benchmark("geekbench:score", `{"cores":"multi"}`), Normalize: selapi.RankingNormalizePerVcpu, Scale: &scale},
		"stress":      {Benchmark: benchmark("stress_ng:bestn", "")},
		"latency":     {Benchmark: benchmark("membench:latency", `{"scope":"RAM"}`)},
		"broken":      {Benchmark: benchmark("geekbench:score", `{"cores":"quantum"}`)},
	}
	cl.Selections = map[string]v1alpha1.Selection{
		"fast-single-core": {
			Selector: &selapi.Selector{VCPUs: &selapi.IntRange{Min: &two}},
			Ranking: &selapi.Ranking{
				Score:     selapi.RankingScore{Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)},
				Objective: selapi.ObjectiveScore,
				Band:      &selapi.RankingBand{WithinPercent: 12},
				Families:  &selapi.RankingFamilies{Min: &three, Max: &six},
			},
		},
	}
	cl.Finalize()
	return cl
}

func metricCatalog(t *testing.T) []*cloudprovider.InstanceType {
	t.Helper()
	cl := metricClass()
	c := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws", "gcp")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	return all(t, c, catalog.Filter{})
}

func intValue(t *testing.T, it *cloudprovider.InstanceType, key string) (int, bool) {
	t.Helper()
	r := it.Requirements.Get(key)
	if r.Operator() == corev1.NodeSelectorOpDoesNotExist {
		return 0, false
	}
	n, err := strconv.Atoi(r.Any())
	if err != nil {
		t.Fatalf("%s %s = %q is not an integer", it.Name, key, r.Any())
	}
	return n, true
}

func TestMetricLabels(t *testing.T) {
	its := metricCatalog(t)

	pct := map[string]int{}
	var best, worstLatency string
	bestScore, bestLatencyPct := -1, -1
	benchmarked, unbenchmarked := 0, 0
	for _, it := range its {
		score, has := intValue(t, it, v1alpha1.MetricLabel("single-core"))
		p, hasPct := intValue(t, it, v1alpha1.MetricPercentileLabel("single-core"))
		if has != hasPct {
			t.Fatalf("%s: score and percentile must exist together", it.Name)
		}
		if !has {
			unbenchmarked++
			// An unbenchmarked type has no per-USD label either.
			for _, o := range it.Offerings {
				if o.Requirements.Get(v1alpha1.MetricPerUSDLabel("single-core")).Operator() != corev1.NodeSelectorOpDoesNotExist {
					t.Fatalf("%s: an unbenchmarked type has a per-USD label", it.Name)
				}
			}
			continue
		}
		benchmarked++
		if p < 0 || p > 100 {
			t.Fatalf("%s: percentile %d", it.Name, p)
		}
		pct[it.Name] = p
		if score > bestScore {
			best, bestScore = it.Name, score
		}
		if lp, ok := intValue(t, it, v1alpha1.MetricPercentileLabel("latency")); ok && lp > bestLatencyPct {
			bestLatencyPct, worstLatency = lp, it.Name
		}
	}
	if benchmarked < 500 || unbenchmarked == 0 {
		t.Errorf("benchmarked %d, unbenchmarked %d types", benchmarked, unbenchmarked)
	}
	if pct[best] != 100 {
		t.Errorf("the best single-core type %s has percentile %d, want 100", best, pct[best])
	}
	// Lower is better: the best latency has percentile 100 as well.
	if bestLatencyPct != 100 {
		t.Errorf("best latency percentile = %d (%s), want 100", bestLatencyPct, worstLatency)
	}

	// The percentile is across all vendors of the class: hcloud and gcp types
	// are ranked against AWS's.
	var seenVendors = map[string]bool{}
	for _, it := range its {
		if _, ok := pct[it.Name]; ok {
			seenVendors[it.Requirements.Get(v1alpha1.LabelVendor).Any()] = true
		}
	}
	for _, v := range []string{"aws", "gcp", "hcloud"} {
		if !seenVendors[v] {
			t.Errorf("no benchmarked %s types", v)
		}
	}
}

func TestMetricScaleAndPerVCPU(t *testing.T) {
	its := metricCatalog(t)
	m7i := byName(its, "aws.m7i.large")
	single, ok1 := intValue(t, m7i, v1alpha1.MetricLabel("single-core"))
	multi, ok2 := intValue(t, m7i, v1alpha1.MetricLabel("multi-core"))
	if !ok1 || !ok2 {
		t.Fatalf("m7i.large has no Geekbench scores")
	}
	// Multi core is per vCPU (2) and scaled by 100: roughly the multi score /
	// 2 * 100, so far above the single-core score of the same order.
	if multi < single {
		t.Errorf("per-vCPU multi-core score x100 = %d, single = %d", multi, single)
	}
	// The per-USD value of an offering is the scaled score over its price.
	var seen bool
	for _, o := range m7i.Offerings {
		perUSD, err := strconv.Atoi(o.Requirements.Get(v1alpha1.MetricPerUSDLabel("single-core")).Any())
		if err != nil {
			t.Fatalf("per-USD label: %v", err)
		}
		want := float64(single) / o.Price
		if float64(perUSD) < want-1 || float64(perUSD) > want+1 {
			t.Errorf("per-USD = %d, want %.1f (score %d / price %v)", perUSD, want, single, o.Price)
		}
		seen = true
	}
	if !seen {
		t.Fatal("no offerings")
	}
	// Lower-is-better benchmarks have no score per dollar.
	for _, o := range m7i.Offerings {
		if o.Requirements.Get(v1alpha1.MetricPerUSDLabel("latency")).Operator() != corev1.NodeSelectorOpDoesNotExist {
			t.Fatal("a lower-is-better metric has a per-USD label")
		}
	}
}

func TestInvalidMetricIsDoesNotExist(t *testing.T) {
	its := metricCatalog(t)
	for _, it := range its {
		for _, k := range []string{
			v1alpha1.MetricLabel("broken"), v1alpha1.MetricPercentileLabel("broken"),
		} {
			if it.Requirements.Get(k).Operator() != corev1.NodeSelectorOpDoesNotExist {
				t.Fatalf("%s defines %s although the benchmark config is unknown", it.Name, k)
			}
		}
		for _, o := range it.Offerings {
			if o.Requirements.Get(v1alpha1.MetricPerUSDLabel("broken")).Operator() != corev1.NodeSelectorOpDoesNotExist {
				t.Fatalf("an offering of %s defines the per-USD label of a broken metric", it.Name)
			}
		}
	}
}

func TestSelectionMembership(t *testing.T) {
	its := metricCatalog(t)
	members, families := 0, map[string]bool{}
	for _, it := range its {
		r := it.Requirements.Get(v1alpha1.LabelSelection)
		if r.Operator() == corev1.NodeSelectorOpDoesNotExist {
			continue
		}
		if !r.Has("fast-single-core") || r.Len() != 1 {
			t.Fatalf("%s: selection = %v", it.Name, r.Values())
		}
		members++
		families[it.Requirements.Get(v1alpha1.LabelVendor).Any()+"/"+it.Requirements.Get(v1alpha1.LabelFamily).Any()] = true
		if vcpus, _ := intValue(t, it, v1alpha1.LabelVCPUs); vcpus < 2 {
			t.Errorf("%s has %d vCPUs, the selector asks for at least 2", it.Name, vcpus)
		}
	}
	if members == 0 || members == len(its) {
		t.Fatalf("%d of %d types are members", members, len(its))
	}
	// The ranking keeps three to six families per vendor.
	if len(families) < 3 || len(families) > 6*3 {
		t.Errorf("members come from %d families: %v", len(families), families)
	}
	t.Logf("%d member types of %d families", members, len(families))
}

// An invalid selection is reported and its members are not labelled.
func TestInvalidSelectionIsReported(t *testing.T) {
	cl := metricClass()
	cl.Selections["bad"] = v1alpha1.Selection{Ranking: &selapi.Ranking{
		Score: selapi.RankingScore{Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)}, // no band and no families.max
	}}
	cl.Finalize()
	c := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws", "gcp")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	metrics, selections := c.Invalid(&cl)
	if _, ok := metrics["broken"]; !ok || len(metrics) != 1 {
		t.Errorf("invalid metrics = %v, want only broken", metrics)
	}
	if reason, ok := selections["bad"]; !ok || len(selections) != 1 {
		t.Errorf("invalid selections = %v, want only bad", selections)
	} else if reason == "" {
		t.Error("no reason")
	}
	for _, it := range all(t, c, catalog.Filter{}) {
		if it.Requirements.Get(v1alpha1.LabelSelection).Has("bad") {
			t.Fatalf("%s belongs to an invalid selection", it.Name)
		}
	}
}

func TestPinnedSelectionPricesByTheObjective(t *testing.T) {
	cl := metricClass()
	two := int32(2)
	cl.Selections["cheap-fast"] = v1alpha1.Selection{
		Selector: &selapi.Selector{VCPUs: &selapi.IntRange{Min: &two}},
		Ranking: &selapi.Ranking{
			Score:     selapi.RankingScore{Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)},
			Objective: selapi.ObjectiveScorePerPrice,
			Band:      &selapi.RankingBand{WithinPercent: 20},
		},
	}
	cl.Selections["cheapest"] = v1alpha1.Selection{
		Ranking: &selapi.Ranking{
			Score:     selapi.RankingScore{Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)},
			Objective: selapi.ObjectivePrice,
			Band:      &selapi.RankingBand{WithinPercent: 50},
		},
	}
	cl.Finalize()
	c := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws", "gcp")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	filter := func(sel string) catalog.Filter {
		return catalog.NewFilter(scheduling.NewRequirements(
			scheduling.NewRequirement(v1alpha1.LabelSelection, corev1.NodeSelectorOpIn, sel),
			scheduling.NewRequirement(v1alpha1.LabelVendor, corev1.NodeSelectorOpIn, "aws"),
		))
	}
	member := func(profile string) *cloudprovider.InstanceType {
		for _, it := range all(t, c, filter(profile)) {
			if it.Requirements.Get(v1alpha1.LabelSelection).Has(profile) {
				if _, ok := intValue(t, it, v1alpha1.MetricLabel("single-core")); ok {
					return it
				}
			}
		}
		t.Fatalf("profile %s has no member", profile)
		return nil
	}
	plain := func(name string) *cloudprovider.InstanceType { return byName(all(t, c, catalog.Filter{}), name) }
	realPrice := func(name string, o *cloudprovider.Offering) float64 {
		for _, p := range plain(name).Offerings {
			if p.Zone() == o.Zone() && p.CapacityType() == o.CapacityType() {
				return p.Price
			}
		}
		t.Fatalf("no real offering for %s", name)
		return 0
	}

	// Objective Score: price 1/score, so the best score is the cheapest.
	fast := member("fast-single-core")
	score, _ := intValue(t, plain(fast.Name), v1alpha1.MetricLabel("single-core"))
	for _, o := range fast.Offerings {
		if !o.Available || o.Price*float64(score) < 0.99 || o.Price*float64(score) > 1.01 {
			t.Fatalf("Score objective: price %v for score %d (available %v)", o.Price, score, o.Available)
		}
	}
	// ScorePerPrice: price divided by score.
	cheapFast := member("cheap-fast")
	score, _ = intValue(t, plain(cheapFast.Name), v1alpha1.MetricLabel("single-core"))
	for _, o := range cheapFast.Offerings {
		if want := realPrice(cheapFast.Name, o) / float64(score); o.Price < want*0.99 || o.Price > want*1.01 {
			t.Fatalf("ScorePerPrice objective: price %v, want %v", o.Price, want)
		}
	}
	// The Price objective keeps the real price.
	cheapest := member("cheapest")
	for _, o := range cheapest.Offerings {
		if real := realPrice(cheapest.Name, o); o.Price != real {
			t.Fatalf("Price objective changed the price: %v != %v", o.Price, real)
		}
	}
	// A filter that does not pin a profile keeps the real prices.
	if o := plain(fast.Name).Offerings[0]; o.Price != realPrice(fast.Name, o) {
		t.Error("an unpinned query is repriced")
	}
}

// A metric whose keys core does not know would make core reject every offering
// that defines its per-USD key, so it is not stamped until a restart has
// registered it, and the class reports RestartRequired.
func TestMetricAddedAfterStartIsNotStampedUntilRestart(t *testing.T) {
	cl := catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})
	cl.Metrics = map[string]v1alpha1.Metric{"late-metric": {Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)}}
	cl.Finalize()
	c := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		v1alpha1.MetricLabel("late-metric"), v1alpha1.MetricPerUSDLabel("late-metric"), v1alpha1.MetricPercentileLabel("late-metric"),
	}
	got := c.RestartRequired(&cl)
	if len(got) != 3 {
		t.Fatalf("RestartRequired = %v, want %v", got, want)
	}
	for _, it := range all(t, c, catalog.Filter{}) {
		if it.Requirements.Has(v1alpha1.MetricLabel("late-metric")) {
			t.Fatalf("%s defines a label core does not know", it.Name)
		}
	}
	if keys := labels.RegisterKeys(labels.MetricLabelKeys([]string{"late-metric"})...); len(keys) != 3 {
		t.Errorf("registered %v", keys)
	}
	if got := c.RestartRequired(&cl); len(got) != 0 {
		t.Errorf("after the registration: %v", got)
	}
}

// A selector-only profile has no ranking, hence no objective: pinning it keeps
// the real price and every offering available.
func TestPinnedSelectorOnlyProfileKeepsRealPrices(t *testing.T) {
	cl := catalog.NewClass("c", catalog.Scope{"v": nil})
	two := int32(2)
	cl.Selections = map[string]v1alpha1.Selection{"big": {Selector: &selapi.Selector{VCPUs: &selapi.IntRange{Min: &two}}}}
	cl.Finalize()
	// The synthetic source cannot evaluate selections; serve the members itself.
	data := synthetic()
	spec := selapi.Spec{Selector: cl.Selections["big"].Selector}
	id := catalog.SelectionQueryID("v", nil, &spec)
	data.Selected = map[string]map[string]model.Member{id: {"small": {}}}
	c := catalog.New(catalog.Options{Source: &fakeSource{data: data}, Facts: syntheticFacts()})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := catalog.NewFilter(scheduling.NewRequirements(scheduling.NewRequirement(v1alpha1.LabelSelection, corev1.NodeSelectorOpIn, "big")))
	it := byName(all(t, c, f), "v.small")
	if it == nil || !it.Requirements.Get(v1alpha1.LabelSelection).Has("big") {
		t.Fatalf("small is not a member: %v", it)
	}
	for _, o := range it.Offerings {
		if !o.Available || (o.Price != 0.10 && o.Price != 0.11 && o.Price != 0.03) {
			t.Errorf("offering %v price %v available %v", o.Zone(), o.Price, o.Available)
		}
	}
}

// A profile that is invalid for one vendor keeps its members at the others, and
// the reason names the vendor.
func TestSelectionInvalidForOneVendorKeepsTheOthers(t *testing.T) {
	cl := catalog.NewClass("c", catalog.Scope{"v": nil, "w": nil})
	cl.Selections = map[string]v1alpha1.Selection{"p": {Selector: &selapi.Selector{}}}
	cl.Finalize()
	data := synthetic()
	spec := selapi.Spec{Selector: cl.Selections["p"].Selector}
	data.Selected = map[string]map[string]model.Member{catalog.SelectionQueryID("v", nil, &spec): {"small": {}}}
	data.Invalid = map[string]string{catalog.SelectionQueryID("w", nil, &spec): "boom"}
	c := catalog.New(catalog.Options{Source: &fakeSource{data: data}, Facts: syntheticFacts()})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	it := byName(all(t, c, catalog.Filter{}), "v.small")
	if it == nil || !it.Requirements.Get(v1alpha1.LabelSelection).Has("p") {
		t.Error("the valid vendor lost its members")
	}
	_, sels := c.Invalid(&cl)
	if sels["p"] != "vendor w: boom" {
		t.Errorf("invalid selections = %v", sels)
	}
}
