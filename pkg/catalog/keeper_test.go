/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/SpareCores/sc-keeper-go/keepertest"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

func serverID(t *testing.T, vendor, apiReference string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(catalogtest.SnapshotDir(), "tables", "server.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		VendorID     string `json:"vendor_id"`
		ServerID     string `json:"server_id"`
		APIReference string `json:"api_reference"`
	}
	if err := json.NewDecoder(zr).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.VendorID == vendor && r.APIReference == apiReference {
			return r.ServerID
		}
	}
	t.Fatalf("no server %s %s", vendor, apiReference)
	return ""
}

// With an API key the flat /table/server_prices is read instead of the large
// anonymous /server_prices (overview section 5.1).
func TestAPIKeyReadsTheFlatPriceTable(t *testing.T) {
	m7i := serverID(t, "aws", "m7i.large")
	var tableRequests, anonymousRequests atomic.Int32
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	h.Mount("/table/server_prices", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tableRequests.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if got := r.URL.Query()["vendor_regions"]; !slices.Equal(got, []string{"aws~us-east-1"}) {
			http.Error(w, "unexpected regions", http.StatusBadRequest)
			return
		}
		rows := []map[string]any{
			{"vendor_id": "aws", "region_id": "us-east-1", "zone_id": "use1-az1", "server_id": m7i, "operating_system": "Linux", "allocation": "ondemand", "unit": "hour", "price": 0.1, "currency": "USD", "status": "active"},
			{"vendor_id": "aws", "region_id": "us-east-1", "zone_id": "use1-az1", "server_id": m7i, "operating_system": "Linux", "allocation": "spot", "unit": "hour", "price": 0.03, "currency": "USD", "status": "active"},
			{"vendor_id": "aws", "region_id": "us-east-1", "zone_id": "use1-az1", "server_id": m7i, "operating_system": "Windows", "allocation": "ondemand", "unit": "hour", "price": 0.2, "currency": "USD", "status": "active"},
			{"vendor_id": "aws", "region_id": "us-east-1", "zone_id": "use1-az1", "server_id": m7i, "operating_system": "Linux", "allocation": "reserved", "unit": "hour", "price": 0.05, "currency": "USD", "status": "active"},
			{"vendor_id": "aws", "region_id": "us-east-1", "zone_id": "use1-az2", "server_id": m7i, "operating_system": "Linux", "allocation": "ondemand", "unit": "hour", "price": 0.1, "currency": "USD", "status": "inactive"},
			{"vendor_id": "aws", "region_id": "us-east-1", "zone_id": "use1-az2", "server_id": m7i, "operating_system": "Linux", "allocation": "ondemand", "unit": "month", "price": 70, "currency": "USD", "status": "active"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	}))
	h.Mount("/server_prices", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		anonymousRequests.Add(1)
		http.Error(w, "must not be used with an API key", http.StatusTeapot)
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := catalog.New(catalog.Options{
		Source: catalog.NewKeeperSource(catalog.KeeperOptions{URL: srv.URL, APIKey: "secret-key"}),
		Facts:  catalogtest.Facts(t, "aws"),
	})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	its := all(t, c, catalog.Filter{})
	if len(its) != 1 || its[0].Name != "aws.m7i.large" {
		t.Fatalf("types = %v", its)
	}
	// Only the active, hourly, Linux on-demand and spot rows count.
	if len(its[0].Offerings) != 2 {
		t.Errorf("offerings = %d, want 2", len(its[0].Offerings))
	}
	if tableRequests.Load() != 1 || anonymousRequests.Load() != 0 {
		t.Errorf("requests: %d table, %d anonymous", tableRequests.Load(), anonymousRequests.Load())
	}
}

func TestKeeperSourceFilters(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"hcloud": {"fsn1"}})
	its := all(t, c, catalog.Filter{})
	if len(its) != 15 {
		t.Fatalf("hcloud has %d types in fsn1, want 15 (the active ones with a price)", len(its))
	}
	for _, it := range its {
		if len(it.Offerings) != 1 {
			t.Errorf("%s has %d offerings: hcloud has no zones and no spot", it.Name, len(it.Offerings))
			continue
		}
		o := it.Offerings[0]
		if !o.Requirements.Get(corev1.LabelTopologyZone).Has("fsn1") || !o.Requirements.Get(corev1.LabelTopologyRegion).Has("fsn1") ||
			!o.Requirements.Get("karpenter.sh/capacity-type").Has("on-demand") {
			t.Errorf("%s: %v", it.Name, reqLines(o.Requirements))
		}
		if !o.Requirements.Get(v1alpha1.LabelCountry).Has("de") {
			t.Errorf("%s: country %v", it.Name, reqLines(o.Requirements))
		}
	}
}

func TestGCPUsesKeepersNumericIDsAndTheVendorsNames(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"gcp": {"europe-west3"}}, "gcp")
	its := all(t, c, catalog.Filter{})
	if len(its) < 100 {
		t.Fatalf("gcp has %d types", len(its))
	}
	it := byName(its, "gcp.n2-standard-4")
	if it == nil {
		t.Fatal("no n2-standard-4")
	}
	// The region label is the vendor's name (Keeper's id is 1300); zones are
	// the vendor's names with Keeper's numeric IDs.
	if r := it.Requirements.Get(corev1.LabelTopologyRegion); r.Len() != 1 || !r.Has("europe-west3") {
		t.Errorf("regions = %v", r.Values())
	}
	zones, ids := it.Requirements.Get(corev1.LabelTopologyZone), it.Requirements.Get(v1alpha1.LabelZoneID)
	if !zones.Has("europe-west3-a") || !zones.Has("europe-west3-b") || !zones.Has("europe-west3-c") {
		t.Errorf("zones = %v", zones.Values())
	}
	for _, id := range ids.Values() {
		if _, err := json.Number(id).Int64(); err != nil {
			t.Errorf("zone id %q is not numeric", id)
		}
	}
	if !it.Requirements.Get(v1alpha1.LabelCountry).Has("de") {
		t.Errorf("country = %v", it.Requirements.Get(v1alpha1.LabelCountry).Values())
	}
	// Both capacity types exist at GCP.
	if r := it.Requirements.Get("karpenter.sh/capacity-type"); !r.Has("spot") || !r.Has("on-demand") {
		t.Errorf("capacity types = %v", r.Values())
	}
}

func TestRegionAllowListAcceptsAliasesAndIDs(t *testing.T) {
	for _, region := range []string{"europe-west3", "1300"} {
		c, _ := refreshed(t, catalog.Scope{"gcp": {region}}, "gcp")
		if its := all(t, c, catalog.Filter{}); len(its) < 100 {
			t.Errorf("region %q: %d types", region, len(its))
		}
	}
	// A region Keeper does not know is ignored; nothing is read for it.
	c, _ := refreshed(t, catalog.Scope{"hcloud": {"mars-1"}})
	if its := all(t, c, catalog.Filter{}); len(its) != 0 {
		t.Errorf("unknown region: %d types", len(its))
	}
}

func TestUnusableServersAreDropped(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"aws": {"us-east-1"}}, "aws")
	its := all(t, c, catalog.Filter{})
	// Inactive, retired and planned-for-retirement servers are not offered, and
	// neither are architectures Kubernetes does not know.
	for _, it := range its {
		if a := it.Requirements.Get(corev1.LabelArchStable); !a.Has("amd64") && !a.Has("arm64") {
			t.Errorf("%s: arch %v", it.Name, a.Values())
		}
		for _, o := range it.Offerings {
			if o.Price <= 0 {
				t.Errorf("%s has an offering priced %v", it.Name, o.Price)
			}
		}
	}
	for _, name := range []string{"aws.t3a.small", "aws.m7i.large", "aws.c7g.xlarge", "aws.p4d.24xlarge"} {
		if byName(its, name) == nil {
			t.Errorf("%s is missing", name)
		}
	}
}

// Retiring types stay in the catalog (overview 5.1); inactive and retired ones
// are dropped.
func TestPlannedForRetirementServersAreKept(t *testing.T) {
	f, err := os.Open(filepath.Join(catalogtest.SnapshotDir(), "tables", "server.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		VendorID, APIReference, Status string
	}
	var raw []map[string]any
	if err := json.NewDecoder(zr).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	for _, r := range raw {
		if r["vendor_id"] == "gcp" {
			rows = append(rows, struct{ VendorID, APIReference, Status string }{"gcp", r["api_reference"].(string), r["status"].(string)})
		}
	}
	data, err := catalogtest.Source(t).Load(t.Context(), catalog.Request{Scope: catalog.Scope{"gcp": {"europe-west3"}}})
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, s := range data.Servers {
		have[s.APIReference] = true
	}
	planned := 0
	for _, r := range rows {
		switch r.Status {
		case "planned-for-retirement":
			planned++
			if !have[r.APIReference] {
				t.Errorf("%s (planned for retirement) was dropped", r.APIReference)
			}
		case "inactive", "retired":
			if have[r.APIReference] {
				t.Errorf("%s (%s) was kept", r.APIReference, r.Status)
			}
		}
	}
	if planned == 0 {
		t.Fatal("the snapshot has no planned-for-retirement gcp server")
	}
}

func TestEditingAClassPrunesItsOldBuild(t *testing.T) {
	c, cl := refreshed(t, catalog.Scope{"aws": {"us-east-1"}}, "aws")
	if len(all(t, c, catalog.Filter{})) == 0 {
		t.Fatal("no types")
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range 3 {
		edited := cl
		edited.MaxPods = int64(30 + i)
		edited.Finalize()
		c.SetClasses([]catalog.Class{edited})
		if len(all(t, c, catalog.Filter{})) == 0 {
			t.Fatal("no types")
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	// Three edits would add three builds (~30 MiB each for one region); pruned,
	// only one stays.
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 25<<20 {
		t.Errorf("the heap grew by %d MiB over three edits", grown>>20)
	}
}
