/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"errors"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

func quotaCloud(t *testing.T) (*fake.Cloud, osapi.Clients, string) {
	t.Helper()
	c := fake.New()
	c.AddRegion("GRA11", fake.RegionConfig{
		Flavors: []osapi.Flavor{{Name: "b3-8", VCPUs: 2, RAMMiB: 8192, DiskGB: 50}, {Name: "t1-le-45", VCPUs: 8, RAMMiB: 46080, DiskGB: 300}},
		Limits:  osapi.Limits{MaxInstances: 10, MaxCores: 16, MaxRAMMiB: 65536},
	})
	cl, _ := c.Clients(context.Background(), "GRA11")
	return c, cl, c.AddImage("GRA11", osapi.Image{Name: "f"})
}

func TestQuotas(t *testing.T) {
	ctx := context.Background()
	c, cl, img := quotaCloud(t)
	fl, _ := cl.Compute.ListFlavors(ctx)
	if _, err := cl.Compute.CreateServer(ctx, osapi.CreateServerRequest{Name: "mine", FlavorID: fl[0].ID, ImageID: img}); err != nil {
		t.Fatal(err)
	}
	c.AddForeignServer("GRA11", "foreign", "b3-8")

	q := NewQuotaReader(c, clocktesting.NewFakeClock(time.Unix(0, 0)))
	snap, err := q.Quotas(ctx, "GRA11")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int64{BucketInstances: {10, 2}, BucketCores: {16, 4}, BucketRAM: {65536, 16384}}
	if len(snap.Buckets) != 3 {
		t.Fatalf("buckets %+v", snap.Buckets)
	}
	for _, b := range snap.Buckets {
		w, ok := want[b.ID]
		if !ok || b.Limit != w[0] || b.Used != w[1] || b.Scope != vendors.QuotaScopeRegion {
			t.Errorf("bucket %+v, want %v (the usage is the whole project: the foreign server counts)", b, w)
		}
	}
	if len(snap.Counted) != 2 {
		t.Errorf("counted %v", snap.Counted)
	}
	if g, err := q.GlobalQuotas(ctx); err != nil || len(g.Buckets) != 0 {
		t.Errorf("OVH limits are per region: %v %v", g, err)
	}

	got, ok := q.Consumption("b3-8", vendors.CapacityOnDemand)
	if !ok || len(got) != 3 || got[0] != (vendors.Consumption{Bucket: BucketInstances, Amount: 1}) ||
		got[1] != (vendors.Consumption{Bucket: BucketCores, Amount: 2}) || got[2] != (vendors.Consumption{Bucket: BucketRAM, Amount: 8192}) {
		t.Errorf("consumption %v %v", got, ok)
	}
	for _, tc := range []struct{ typ, cap, why string }{
		{"b3-8", vendors.CapacitySpot, "spot does not exist"},
		{"t1-le-45", vendors.CapacityOnDemand, "GPU flavors are not launched"},
		{"b3-999", vendors.CapacityOnDemand, "a flavor the region does not have is unknown"},
	} {
		if _, ok := q.Consumption(tc.typ, tc.cap); ok {
			t.Errorf("%s: want unknown", tc.why)
		}
	}
	// Counted drops what the tracker no longer needs: a deleted server.
	ids := c.ServerIDs("GRA11")
	_ = cl.Compute.DeleteServer(ctx, ids[0])
	snap, _ = q.Quotas(ctx, "GRA11")
	if len(snap.Counted) != 1 || snap.Buckets[0].Used != 1 {
		t.Errorf("after delete: %+v", snap)
	}
}

func TestSnapshotSkipsDeleted(t *testing.T) {
	s := Snapshot("GRA11", osapi.Limits{MaxInstances: 5}, []osapi.Server{
		{ID: "a", Status: "ACTIVE"}, {ID: "b", Status: "DELETED"}, {ID: "c", Status: "SHUTOFF"}, {ID: "d", Status: "SOFT_DELETED"},
	})
	if len(s.Counted) != 2 || s.Counted[0] != "GRA11/a" || s.Counted[1] != "GRA11/c" {
		t.Errorf("counted %v: stopped servers still use the quota", s.Counted)
	}
}

func TestQuotaErrorsAreClassified(t *testing.T) {
	ctx := context.Background()
	c, _, _ := quotaCloud(t)
	c.AddRegion("SBG5", fake.RegionConfig{NoEndpoint: true})
	q := NewQuotaReader(c, nil)
	if _, err := q.Quotas(ctx, "SBG5"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("no endpoint: %v", err)
	}
	c.InjectError("GRA11", "GetLimits", &osapi.APIError{Service: "nova", Status: 503, Message: "down"}, 1)
	if _, err := q.Quotas(ctx, "GRA11"); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("503: %v", err)
	}
	c.InjectError("GRA11", "ListFlavors", errors.New("boom"), 1)
	if _, err := q.Quotas(ctx, "GRA11"); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("unknown: %v", err)
	}
}

func TestFlavorCacheTTL(t *testing.T) {
	ctx := context.Background()
	c, _, _ := quotaCloud(t)
	clk := clocktesting.NewFakeClock(time.Unix(0, 0))
	q := NewQuotaReader(c, clk)
	if err := q.LoadFlavors(ctx, "GRA11"); err != nil {
		t.Fatal(err)
	}
	// Within the TTL the flavors are not read again: an injected failure is not hit.
	c.InjectError("GRA11", "ListFlavors", errors.New("boom"), 0)
	if err := q.LoadFlavors(ctx, "GRA11"); err != nil {
		t.Fatalf("cached: %v", err)
	}
	clk.Step(2 * time.Hour)
	if err := q.LoadFlavors(ctx, "GRA11"); err == nil {
		t.Fatal("after the TTL the flavors are read again")
	}
}
