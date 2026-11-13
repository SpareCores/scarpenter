/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/SpareCores/sc-keeper-go/keepertest"
	selapi "github.com/SpareCores/sc-keeper-go/selection/api"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

// requestLog serves the snapshot and records what a refresh asks for. Spot
// score queries (not recorded in the snapshot) are answered with the on-demand
// recording, so the test can see them.
type requestLog struct {
	mu       sync.Mutex
	servers  []url.Values
	prices   int
	h        http.Handler
	spotOnly bool
}

func (l *requestLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	switch r.URL.Path {
	case "/servers":
		l.servers = append(l.servers, r.URL.Query())
	case "/server_prices", "/table/server_prices":
		l.prices++
	}
	l.mu.Unlock()
	if l.spotOnly && r.URL.Path == "/servers" && r.URL.Query().Get("best_price_allocation") == "SPOT_ONLY" {
		q := r.URL.Query()
		q.Set("best_price_allocation", "ONDEMAND_ONLY")
		r.URL.RawQuery = q.Encode()
	}
	l.h.ServeHTTP(w, r)
}

func newRequestLog(t *testing.T) (*requestLog, string) {
	t.Helper()
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &requestLog{h: h, spotOnly: true}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	return l, srv.URL
}

func loadSelection(t *testing.T, base string, regions []string, spec selapi.Spec) (map[string]struct{}, string) {
	t.Helper()
	id := catalog.SelectionQueryID("aws", regions, &spec)
	data, err := catalogtest.SourceAt(base).Load(t.Context(), catalog.Request{
		Scope:      catalog.Scope{"aws": regions},
		Selections: []catalog.SelectionQuery{{ID: id, Vendor: "aws", Regions: regions, Spec: spec}},
	})
	if err != nil {
		t.Fatal(err)
	}
	members := map[string]struct{}{}
	for n := range data.Selected[id] {
		members[n] = struct{}{}
	}
	return members, data.Invalid[id]
}

// A selection profile reads no /servers rows for specifications or prices: the
// only /servers requests are benchmark score queries of the whole vendor (no
// regions, no push-down filters), and prices come from the price endpoint of
// the catalog only.
func TestSelectionAsksOnlyForScores(t *testing.T) {
	l, base := newRequestLog(t)
	two, three, six := int32(2), int32(3), int32(6)
	max := 0.5
	spec := selapi.Spec{
		Selector: &selapi.Selector{
			VCPUs: &selapi.IntRange{Min: &two}, Price: &selapi.PriceSelector{MaxHourly: "0.5"},
			Benchmarks: []selapi.BenchmarkFilter{{BenchmarkRef: *benchmark("geekbench:score", `{"cores":"single"}`), Min: &max}},
		},
		Ranking: &selapi.Ranking{
			Score:     selapi.RankingScore{Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)},
			Objective: selapi.ObjectiveScore, Band: &selapi.RankingBand{WithinPercent: 12},
			Families: &selapi.RankingFamilies{Min: &three, Max: &six},
		},
	}
	members, invalid := loadSelection(t, base, []string{"us-east-1"}, spec)
	if invalid != "" || len(members) == 0 {
		t.Fatalf("members %d, invalid %q", len(members), invalid)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.servers) != 1 {
		t.Fatalf("%d /servers requests, want one (the score query of the one benchmark): %v", len(l.servers), l.servers)
	}
	q := l.servers[0]
	if len(q["vendor_regions"]) != 0 || q.Get("benchmark_id") != "geekbench:score" || q.Get("best_price_allocation") != "ONDEMAND_ONLY" {
		t.Errorf("score query = %v", q)
	}
	for _, name := range []string{"vcpus_min", "vcpus_max", "price_max", "benchmark_score_min", "memory_min"} {
		if q.Has(name) {
			t.Errorf("push-down filter %s sent", name)
		}
	}
	if l.prices == 0 {
		t.Error("the prices of the catalog were not read")
	}
}

// A spot-basis profile reads the scores of spot-priced types: a type with a
// spot price and no on-demand price still gets its score.
func TestSpotSelectionReadsSpotScores(t *testing.T) {
	l, base := newRequestLog(t)
	three := int32(3)
	spec := selapi.Spec{
		Selector: &selapi.Selector{Price: &selapi.PriceSelector{Basis: selapi.PriceBasisSpot}},
		Ranking: &selapi.Ranking{
			Score:     selapi.RankingScore{Benchmark: benchmark("geekbench:score", `{"cores":"single"}`)},
			Objective: selapi.ObjectiveScorePerPrice, Band: &selapi.RankingBand{WithinPercent: 20},
			Families: &selapi.RankingFamilies{Min: &three},
		},
	}
	members, invalid := loadSelection(t, base, []string{"us-east-1"}, spec)
	if invalid != "" || len(members) == 0 {
		t.Fatalf("members %d, invalid %q", len(members), invalid)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var spot []url.Values
	for _, q := range l.servers {
		if q["best_price_allocation"][0] == "SPOT_ONLY" {
			spot = append(spot, q)
		}
	}
	if len(spot) != 1 || len(spot[0]["vendor_regions"]) != 0 {
		t.Errorf("spot score queries = %v", spot)
	}
	if slices.ContainsFunc(l.servers, func(q url.Values) bool { return q["best_price_allocation"][0] != "SPOT_ONLY" }) {
		t.Errorf("a spot profile read on-demand scores: %v", l.servers)
	}
}
