/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clocktesting "k8s.io/utils/clock/testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

// flakyKeeper replays the snapshot and fails every request while down is set.
func flakyKeeper(t *testing.T) (url string, down *atomic.Bool) {
	t.Helper()
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	down = &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "keeper is down", http.StatusInternalServerError)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, down
}

func quickKeeper(url string) *catalog.KeeperSource {
	return catalog.NewKeeperSource(catalog.KeeperOptions{
		URL: url, PricesRegionsPerRequest: 1,
		ClientOptions: []keeper.Option{keeper.WithRetries(0, time.Millisecond, time.Millisecond)},
	})
}

func TestStaleWhileError(t *testing.T) {
	url, down := flakyKeeper(t)
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	reg := prometheus.NewRegistry()
	c := catalog.New(catalog.Options{
		Source: quickKeeper(url), Facts: catalogtest.Facts(t, "aws"), Clock: clk,
		RefreshInterval: time.Hour, MaxAge: 24 * time.Hour, Registerer: reg,
	})
	cl := catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})

	// Cold start with Keeper down: not ready, nothing offered.
	down.Store(true)
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err == nil {
		t.Fatal("the refresh succeeded against a failing Keeper")
	}
	st := c.Status("c")
	if st.Ready || st.Reason != "NoSnapshot" || !strings.Contains(st.Message, "Keeper") {
		t.Errorf("cold start: %+v", st)
	}
	if its := all(t, c, catalog.Filter{}); len(its) != 0 {
		t.Errorf("a catalog without a snapshot offered %d types", len(its))
	}

	// Keeper recovers.
	down.Store(false)
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := c.Status("c"); !st.Ready || st.LastError != nil {
		t.Fatalf("after the first refresh: %+v", st)
	}
	want := len(all(t, c, catalog.Filter{}))
	if want < 1000 {
		t.Fatalf("only %d types", want)
	}

	// Keeper fails again: the last good snapshot stays, the class stays ready
	// until the snapshot is older than the maximum age.
	down.Store(true)
	clk.Step(2 * time.Hour)
	if err := c.Refresh(t.Context()); err == nil {
		t.Fatal("the refresh succeeded against a failing Keeper")
	}
	st = c.Status("c")
	if !st.Ready || st.LastError == nil || st.Age != 2*time.Hour {
		t.Errorf("failing, still young: %+v", st)
	}
	if got := len(all(t, c, catalog.Filter{})); got != want {
		t.Errorf("types changed from %d to %d on a failed refresh", want, got)
	}
	clk.Step(23 * time.Hour)
	st = c.Status("c")
	if st.Ready || st.Reason != "Stale" || !strings.Contains(st.Message, "older than the maximum age") {
		t.Errorf("failing, too old: %+v", st)
	}
	// Stale data is still served; the NodeClass is what stops provisioning.
	if got := len(all(t, c, catalog.Filter{})); got != want {
		t.Errorf("stale snapshot offers %d types, want %d", got, want)
	}

	if got := counterValue(t, reg, "scarpenter_catalog_refresh_errors_total"); got != 2 {
		t.Errorf("refresh errors = %v, want 2", got)
	}

	// Recovery.
	down.Store(false)
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := c.Status("c"); !st.Ready || st.LastError != nil || st.Age != 0 {
		t.Errorf("after recovery: %+v", st)
	}
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("metric %s is not registered", name)
	return 0
}

func TestMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	c := catalog.New(catalog.Options{Source: catalogtest.Source(t), Clock: clk, Registerer: reg})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}, "hcloud": {"fsn1"}})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	clk.Step(90 * time.Second)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != 1 {
				continue
			}
			if got[mf.GetName()] == nil {
				got[mf.GetName()] = map[string]float64{}
			}
			got[mf.GetName()][m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	for _, v := range []string{"aws", "hcloud"} {
		if got["scarpenter_catalog_age_seconds"][v] != 90 {
			t.Errorf("age of %s = %v, want 90", v, got["scarpenter_catalog_age_seconds"][v])
		}
		if got["scarpenter_catalog_instance_types"][v] == 0 || got["scarpenter_catalog_offerings"][v] == 0 {
			t.Errorf("%s: no types or offerings in the metrics: %v", v, got)
		}
	}
	if got["scarpenter_catalog_instance_types"]["hcloud"] != 15 {
		t.Errorf("hcloud types = %v, want 15", got["scarpenter_catalog_instance_types"]["hcloud"])
	}
}

func TestScopeChangeTriggersARefresh(t *testing.T) {
	src := &fakeSource{data: synthetic()}
	c := catalog.New(catalog.Options{Source: src, Facts: syntheticFacts()})
	cl := catalog.NewClass("c", catalog.Scope{"v": {"region-1"}})
	c.SetClasses([]catalog.Class{cl})
	if st := c.Status("c"); st.Ready || st.Reason != "NoSnapshot" {
		t.Errorf("before the first refresh: %+v", st)
	}
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := c.Status("c"); !st.Ready {
		t.Fatalf("after the refresh: %+v", st)
	}
	req := src.reqs[0]
	if len(req.Scope["v"]) != 1 {
		t.Errorf("scope = %v", req.Scope)
	}

	// A second class with another region widens the union; until the refresh
	// it is not covered.
	cl2 := catalog.NewClass("d", catalog.Scope{"v": {"region-2"}, "w": nil})
	c.SetClasses([]catalog.Class{cl, cl2})
	if st := c.Status("d"); st.Ready || st.Reason != "ScopePending" {
		t.Errorf("new class before the refresh: %+v", st)
	}
	if st := c.Status("c"); !st.Ready {
		t.Errorf("the old class must stay ready: %+v", st)
	}
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := c.Status("d"); !st.Ready {
		t.Errorf("new class after the refresh: %+v", st)
	}
	last := src.reqs[len(src.reqs)-1]
	if got := strings.Join(last.Scope["v"], ","); got != "region-1,region-2" {
		t.Errorf("union scope of v = %q", got)
	}
	if _, ok := last.Scope["w"]; !ok {
		t.Errorf("union scope lacks w: %v", last.Scope)
	}

	// One class that lists no regions makes the union "all".
	cl3 := catalog.NewClass("e", catalog.Scope{"v": nil})
	if got := catalog.UnionScope([]catalog.Class{cl, cl3}); len(got["v"]) != 0 {
		t.Errorf("union with an all-regions class = %v", got)
	}
	if st := c.Status("nope"); st.Ready || st.Reason != "UnknownClass" {
		t.Errorf("unknown class: %+v", st)
	}
}

func TestStartRefreshesOnStartTriggerAndInterval(t *testing.T) {
	src := &fakeSource{data: synthetic()}
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	c := catalog.New(catalog.Options{Source: src, Facts: syntheticFacts(), Clock: clk, RefreshInterval: time.Hour})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil})})
	go func() { _ = c.Start(t.Context()) }()

	waitUntil(t, "the first refresh", func() bool { return src.loadCount() == 1 && clk.HasWaiters() })

	// A new scope refreshes at once, without the interval passing.
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil, "w": nil})})
	waitUntil(t, "the refresh for the new scope", func() bool { return src.loadCount() == 2 && clk.HasWaiters() })

	// The interval (with at most 10 % jitter) triggers the next one.
	clk.Step(67 * time.Minute)
	waitUntil(t, "the interval refresh", func() bool { return src.loadCount() == 3 && clk.HasWaiters() })

	// After a failure the retry comes within a minute.
	src.setErr(errors.New("boom"))
	clk.Step(67 * time.Minute)
	waitUntil(t, "the failing refresh", func() bool { return src.loadCount() == 4 && clk.HasWaiters() })
	src.setErr(nil)
	clk.Step(61 * time.Second)
	waitUntil(t, "the retry", func() bool { return src.loadCount() == 5 && clk.HasWaiters() })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNoClassesNoSnapshot(t *testing.T) {
	src := &fakeSource{data: synthetic()}
	c := catalog.New(catalog.Options{Source: src})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if src.loadCount() != 0 {
		t.Error("the source was read without any NodeClass")
	}
	if its, _ := c.InstanceTypes(t.Context(), catalog.Query{Class: "c"}); len(its) != 0 {
		t.Error("types without a class")
	}
}
