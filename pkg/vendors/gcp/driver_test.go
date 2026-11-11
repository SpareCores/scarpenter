/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

func TestRegionsAndZones(t *testing.T) {
	e := newEnv(t, envOptions{bare: true})
	regions, err := e.d.Regions(t.Context())
	if err != nil || len(regions) != 2 || regions[0].Name != fake.SeedRegion || regions[1].Name != fake.SeedOtherReg {
		t.Fatalf("Regions = %v, %v", regions, err)
	}
	zones, err := e.d.Zones(t.Context(), fake.SeedRegion)
	if err != nil || len(zones) != 2 || zones[0].Name != fake.SeedZoneA || zones[0].ID != "2000" || zones[1].ID != "2001" {
		t.Fatalf("Zones = %v, %v", zones, err)
	}
	// A zone that is down or deprecated is not offered, a region that is unknown has none.
	e.c.SetZoneStatus(fake.SeedZoneB, "DOWN")
	e2 := restart(t, e.c, envOptions{})
	if zs, _ := e2.d.Zones(t.Context(), fake.SeedRegion); len(zs) != 1 {
		t.Errorf("a down zone is offered: %v", zs)
	}
	if zs, err := e2.d.Zones(t.Context(), "mars-north1"); err != nil || len(zs) != 0 {
		t.Errorf("Zones of an unknown region = %v, %v", zs, err)
	}
	// The answers are cached for an hour: a second call makes no request.
	before := e.c.Calls("ListRegions")
	if _, err := e.d.Regions(t.Context()); err != nil || e.c.Calls("ListRegions") != before {
		t.Errorf("the regions are read again within the hour")
	}
	e.clock.Step(2 * time.Hour)
	if _, err := e.d.Regions(t.Context()); err != nil || e.c.Calls("ListRegions") != before+1 {
		t.Errorf("the regions are not read again after the TTL")
	}
}

func TestRegionsPermissionDenied(t *testing.T) {
	e := newEnv(t, envOptions{bare: true})
	e.c.Inject("ListRegions", fake.Error(403, "forbidden", "denied"), 1)
	e2 := restart(t, e.c, envOptions{})
	if _, err := e2.d.Regions(t.Context()); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("a denied region list: %v, want RegionUnavailable", err)
	}
}

func TestIdentityAndBilling(t *testing.T) {
	d := gcp.New()
	if d.Vendor() != "gcp" || d.TagKey(vendors.TagCluster) != "scarpenter-cluster" {
		t.Errorf("identity: %s %s", d.Vendor(), d.TagKey(vendors.TagCluster))
	}
	rule, ok := d.BillingRule("europe-west3", "e2")
	if !ok || rule.Verified || rule.Minimum == nil || *rule.Minimum != time.Minute || rule.Source == "" {
		t.Errorf("billing rule = %+v, %v: must be reported, unverified (retention stays off)", rule, ok)
	}
	if a, ok := d.VendorArch("amd64"); !ok || a != "X86_64" {
		t.Errorf("VendorArch(amd64) = %q, %v", a, ok)
	}
	if a, ok := d.VendorArch("arm64"); !ok || a != "ARM64" {
		t.Errorf("VendorArch(arm64) = %q, %v", a, ok)
	}
	if _, ok := d.VendorArch("riscv"); ok {
		t.Error("VendorArch(riscv) is known")
	}
}

func TestInitNeedsAProject(t *testing.T) {
	e := fake.NewSeeded("p")
	d := gcp.New(gcp.WithAPI(e))
	// Without a project in the option or the context, and an API given (no credentials to
	// read), Init says what to set.
	err := d.Init(t.Context(), vendors.Config{Log: logr.Discard()})
	if err == nil || !strings.Contains(err.Error(), "--gcp-project") {
		t.Errorf("Init without a project: %v", err)
	}
	d = gcp.New(gcp.WithAPI(e), gcp.WithProject("p"))
	if err := d.Init(t.Context(), vendors.Config{Log: logr.Discard()}); err != nil || d.Project() != "p" {
		t.Errorf("Init with a project: %v, %q", err, d.Project())
	}
}

func TestInitReadsTheIPv4Price(t *testing.T) {
	t.Setenv(gcp.IPv4PriceEnv, "abc")
	d := gcp.New(gcp.WithAPI(fake.NewSeeded("p")), gcp.WithProject("p"))
	if err := d.Init(t.Context(), vendors.Config{Log: logr.Discard(), KeeperURL: "http://keeper.invalid"}); err == nil {
		t.Error("a bad price override is accepted")
	}
	t.Setenv(gcp.IPv4PriceEnv, "0.01")
	d = gcp.New(gcp.WithAPI(fake.NewSeeded("p")), gcp.WithProject("p"))
	if err := d.Init(t.Context(), vendors.Config{Log: logr.Discard(), KeeperURL: "http://keeper.invalid"}); err != nil {
		t.Fatal(err)
	}
}

// TestKeeperPrices reads the disk prices from a fake Keeper and prices a node.
func TestKeeperPrices(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.URL.RequestURI(), "/storage_prices?vendor=gcp") {
			t.Errorf("unexpected request %s", r.URL.RequestURI())
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"vendor_id": "gcp", "region_id": fake.SeedRegion, "storage_id": "pd-balanced", "unit": "GB/month",
				"price": 0.146, "currency": "USD", "status": "active"},
			{"vendor_id": "gcp", "region_id": fake.SeedRegion, "storage_id": "hyperdisk-balanced", "unit": "GB/month",
				"price": 0.1, "currency": "EUR", "status": "active"},
			{"vendor_id": "aws", "region_id": fake.SeedRegion, "storage_id": "pd-ssd", "unit": "GB/month",
				"price": 1, "currency": "USD", "status": "active"},
		})
	}))
	defer srv.Close()
	d := gcp.New(gcp.WithAPI(fake.NewSeeded("p")), gcp.WithProject("p"))
	if err := d.Init(t.Context(), vendors.Config{Log: logr.Discard(), KeeperURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if err := d.LoadPrices(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := d.Defaults()
	price, complete := d.PriceDelta(s, vendors.Offering{Region: fake.SeedRegion, APIReference: "e2-small"})
	want := 64*0.146/730 + gcp.DefaultIPv4PricePerHour
	if !complete || price < want-1e-9 || price > want+1e-9 {
		t.Errorf("PriceDelta = %v, %v; want %v, complete", price, complete, want)
	}
	// A disk type Keeper has no usable price for makes the price incomplete.
	hs := vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"diskType":"hyperdisk-balanced","sizeGb":10}}}`)}
	if _, complete := d.PriceDelta(hs, vendors.Offering{Region: fake.SeedRegion}); complete {
		t.Error("a price without a row is complete")
	}
	if d.NodeOptionsEpoch() == 0 {
		t.Error("the epoch did not change when the prices loaded")
	}
	// Regions kicks the background load, and a bad answer is an error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	d2 := gcp.New(gcp.WithAPI(fake.NewSeeded("p")), gcp.WithProject("p"))
	if err := d2.Init(t.Context(), vendors.Config{Log: logr.Discard(), KeeperURL: bad.URL}); err != nil {
		t.Fatal(err)
	}
	if err := d2.LoadPrices(t.Context()); err == nil {
		t.Error("an HTTP 500 from Keeper is not an error")
	}
	if err := gcp.New().LoadPrices(t.Context()); err != nil {
		t.Errorf("LoadPrices without Keeper: %v", err)
	}
	if _, err := d2.Regions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := (gcp.StaticPrices{Storage: map[string]float64{"r/pd-ssd": 1}, IPv4: map[string]float64{"r": 2}}).StoragePrice("r", "pd-ssd"); !ok {
		t.Error("StaticPrices")
	}
	if v, ok := (gcp.StaticPrices{IPv4: map[string]float64{"r": 2}}).IPv4Price("r"); !ok || v != 2 {
		t.Error("StaticPrices IPv4")
	}
}

// hangAPI is a ComputeAPI whose calls block until their context ends: a hung
// connection. Every call the driver makes must keep a deadline.
type hangAPI struct {
	gcp.ComputeAPI
	calls atomic.Int32
}

func (h *hangAPI) AggregatedListInstances(ctx context.Context, _ *computepb.AggregatedListInstancesRequest,
) (*computepb.InstanceAggregatedList, error) {
	h.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (h *hangAPI) ListRegions(ctx context.Context, _ *computepb.ListRegionsRequest) (*computepb.RegionList, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestEveryCallKeepsItsDeadline(t *testing.T) {
	h := &hangAPI{ComputeAPI: fake.NewSeeded("p")}
	d := gcp.New(gcp.WithAPI(h), gcp.WithProject("p"), gcp.WithCallTimeouts(50*time.Millisecond, 50*time.Millisecond))
	if err := d.Init(t.Context(), vendors.Config{ClusterName: "c", Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := vendors.CreateRequest{Region: fake.SeedRegion, Zone: fake.SeedZoneA, InstanceType: "e2-small",
			CapacityType: vendors.CapacityOnDemand, IdempotencyKey: "uid-1", Node: vendors.NodeInfo{Name: "n1"}}
		if _, err := d.CreateInstance(t.Context(), req); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("CreateInstance on a hung API: %v, want Retryable", err)
		}
		if _, err := d.Regions(t.Context()); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("Regions on a hung API: %v, want Retryable", err)
		}
		if _, err := d.ListInstances(t.Context(), []string{fake.SeedRegion}, nil); err == nil {
			t.Error("ListInstances on a hung API succeeded")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a call hung: it has no deadline")
	}
}
