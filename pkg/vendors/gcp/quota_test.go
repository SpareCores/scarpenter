/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"reflect"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

func TestQuotaReader(t *testing.T) {
	ctx := t.Context()
	c := fake.NewSeeded("proj")
	c.AddForeignUsage(fake.SeedRegion, "CPUS", 5)
	c.AddForeignUsage("", "CPUS_ALL_REGIONS", 7)
	spec := baseSpec()
	spec.MachineType = "n2-standard-2"
	req, _ := BuildInsert(spec)
	if _, err := c.InsertInstance(ctx, req); err != nil {
		t.Fatal(err)
	}

	r := &QuotaReader{API: c, Project: "proj"}
	snap, err := r.Quotas(ctx, fake.SeedRegion)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]vendors.Bucket{}
	for _, b := range snap.Buckets {
		got[b.ID] = b
	}
	if b := got["CPUS"]; b.Limit != 24 || b.Used != 7 || b.Scope != vendors.QuotaScopeRegion || b.Unit != "vCPU" {
		t.Fatalf("CPUS bucket = %+v", b)
	}
	if b := got["N2_CPUS"]; b.Limit != 8 || b.Used != 2 {
		t.Fatalf("N2_CPUS bucket = %+v", b)
	}
	if b := got["PREEMPTIBLE_CPUS"]; b.Limit != 0 || b.Used != 0 {
		t.Fatalf("PREEMPTIBLE_CPUS bucket = %+v", b)
	}
	if b := got["INSTANCES"]; b.Unit != "instances" || b.Limit != 240 {
		t.Fatalf("INSTANCES bucket = %+v", b)
	}
	if _, in := got["CPUS_ALL_REGIONS"]; in {
		t.Fatal("the region's snapshot must not hold the project-wide bucket")
	}
	// Sorted by ID.
	for i := 1; i < len(snap.Buckets); i++ {
		if snap.Buckets[i-1].ID > snap.Buckets[i].ID {
			t.Fatalf("buckets not sorted: %v", snap.Buckets)
		}
	}

	gs, err := r.GlobalQuotas(ctx)
	if err != nil || len(gs.Buckets) != 1 {
		t.Fatalf("global = %v, %v", gs, err)
	}
	if b := gs.Buckets[0]; b.ID != "CPUS_ALL_REGIONS" || b.Scope != vendors.QuotaScopeGlobal || b.Limit != 32 || b.Used != 9 {
		t.Fatalf("global bucket = %+v", b)
	}

	// A project without the all-regions quota has no global bucket.
	empty := fake.New("proj")
	empty.AddRegion("r1", nil)
	gs, err = (&QuotaReader{API: empty, Project: "proj"}).GlobalQuotas(ctx)
	if err != nil || len(gs.Buckets) != 0 {
		t.Fatalf("global without the quota = %v, %v", gs, err)
	}
}

func TestQuotaReaderErrors(t *testing.T) {
	ctx := t.Context()
	c := fake.NewSeeded("proj")
	r := &QuotaReader{API: c, Project: "proj"}
	if _, err := r.Quotas(ctx, "nowhere1"); vendors.ClassOf(err) != vendors.NotFound {
		t.Fatalf("missing region = %v", err)
	}
	c.Inject("GetRegion", fake.Error(403, "forbidden", "Required 'compute.regions.get' permission"), 1)
	if _, err := r.Quotas(ctx, fake.SeedRegion); vendors.ClassOf(err) != vendors.RegionUnavailable {
		t.Fatalf("denied region = %v", err)
	}
	c.Inject("GetRegion", fake.Error(503, "backendError", "x"), 1)
	if _, err := r.Quotas(ctx, fake.SeedRegion); vendors.ClassOf(err) != vendors.Retryable {
		t.Fatalf("backend error = %v", err)
	}
	c.Inject("GetProject", fake.Error(503, "backendError", "x"), 1)
	if _, err := r.GlobalQuotas(ctx); vendors.ClassOf(err) != vendors.Retryable {
		t.Fatalf("project backend error = %v", err)
	}
}

func TestConsumption(t *testing.T) {
	r := &QuotaReader{}
	// Nothing is confirmed by default: every type is unknown (reactive).
	if _, ok := r.Consumption("n2-standard-8", 8, vendors.CapacityOnDemand); ok {
		t.Fatal("an unconfirmed series must be unknown")
	}
	r.Confirmed = map[string][]string{"n2": {"CPUS", "N2_CPUS"}, "e2": {"CPUS"}, "empty": {}}
	got, ok := r.Consumption("n2-standard-8", 8, vendors.CapacityOnDemand)
	want := []vendors.Consumption{{Bucket: "CPUS", Amount: 8}, {Bucket: "N2_CPUS", Amount: 8}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("n2 = %v, %v", got, ok)
	}
	if got, ok := r.Consumption("e2-small", 2, vendors.CapacityOnDemand); !ok || len(got) != 1 || got[0].Bucket != "CPUS" {
		t.Fatalf("e2 = %v, %v", got, ok)
	}
	for name, f := range map[string]func() bool{
		"spot":        func() bool { _, ok := r.Consumption("n2-standard-8", 8, vendors.CapacitySpot); return ok },
		"zero vcpus":  func() bool { _, ok := r.Consumption("n2-standard-8", 0, vendors.CapacityOnDemand); return ok },
		"other":       func() bool { _, ok := r.Consumption("c3-standard-4", 4, vendors.CapacityOnDemand); return ok },
		"empty table": func() bool { _, ok := r.Consumption("empty-x", 4, vendors.CapacityOnDemand); return ok },
	} {
		if f() {
			t.Errorf("%s must be unknown", name)
		}
	}
}

func TestSeriesMetrics(t *testing.T) {
	for series, want := range map[string][]string{
		"e2": {"CPUS"}, "n1": {"CPUS"}, "n2": {"CPUS", "N2_CPUS"}, "c3": {"CPUS", "C3_CPUS"}, "t2a": {"CPUS", "T2A_CPUS"},
		"c4": nil, "c4a": nil, "n4": nil, "": nil,
	} {
		if got := SeriesMetrics(series); !reflect.DeepEqual(got, want) {
			t.Errorf("SeriesMetrics(%q) = %v, want %v", series, got, want)
		}
	}
}

func TestIsCPUMetric(t *testing.T) {
	for m, want := range map[string]bool{
		"CPUS": true, "N2_CPUS": true, "PREEMPTIBLE_CPUS": true, "CPUS_ALL_REGIONS": true, "CPUS_PER_VM_FAMILY": true,
		"DISKS_TOTAL_GB": false, "IN_USE_ADDRESSES": false, "INSTANCES": false, "GPUS_ALL_REGIONS": false,
	} {
		if got := IsCPUMetric(m); got != want {
			t.Errorf("IsCPUMetric(%s) = %v", m, got)
		}
	}
	// The unit of every kind of metric.
	if unitOf("CPUS") != "vCPU" || unitOf("INSTANCES") != "instances" || unitOf("DISKS_TOTAL_GB") != "count" {
		t.Fatal("unitOf")
	}
	_ = computepb.Quota{}
}
