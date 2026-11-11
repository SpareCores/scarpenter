/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

func bucketOf(s *vendors.QuotaSnapshot, id string) (vendors.Bucket, bool) {
	for _, b := range s.Buckets {
		if b.ID == id {
			return b, true
		}
	}
	return vendors.Bucket{}, false
}

func TestQuotasFollowInstances(t *testing.T) {
	e := newEnv(t, envOptions{})
	if _, err := e.d.LaunchableTypes(t.Context(), testRegion); err != nil {
		t.Fatal(err)
	}
	// Usage by other workloads of the project counts too.
	e.c.AddForeignUsage(testRegion, "CPUS", 4)
	before, err := e.d.Quotas(t.Context(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := bucketOf(before, "CPUS"); !ok || b.Limit != 24 || b.Used != 4 || b.Scope != vendors.QuotaScopeRegion || b.Unit != "vCPU" {
		t.Fatalf("CPUS before = %+v, %v", b, ok)
	}
	inst, err := e.d.CreateInstance(t.Context(), request(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := e.d.Quotas(t.Context(), testRegion)
	if b, _ := bucketOf(after, "CPUS"); b.Used != 6 {
		t.Errorf("CPUS after = %+v", b)
	}
	if len(after.Counted) != 1 || after.Counted[0] != inst.Ref {
		t.Errorf("counted = %v", after.Counted)
	}
	// A stopped instance holds no CPU quota and is not counted.
	_, zone, name, _ := gcp.ParseRef(inst.Ref)
	e.c.SetStatus(zone, name, gcp.StatusStopped)
	stopped, _ := e.d.Quotas(t.Context(), testRegion)
	if len(stopped.Counted) != 0 {
		t.Errorf("a stopped instance is counted: %v", stopped.Counted)
	}
	cons, ok := e.d.Consumption("n2-standard-8", vendors.CapacityOnDemand)
	if !ok || len(cons) != 2 || cons[0].Amount != 8 {
		t.Errorf("n2 consumption = %v, %v", cons, ok)
	}
	// Spot and the series the table does not confirm are unknown.
	if _, ok := e.d.Consumption("n2-standard-8", vendors.CapacitySpot); ok {
		t.Error("spot is known")
	}
	if _, ok := e.d.Consumption("c3-standard-4", vendors.CapacityOnDemand); ok {
		t.Error("an unconfirmed series is known")
	}
	if _, ok := e.d.Consumption("no-such-type", vendors.CapacityOnDemand); ok {
		t.Error("an unknown type is known")
	}
	g, err := e.d.GlobalQuotas(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := bucketOf(g, "CPUS_ALL_REGIONS"); !ok || b.Scope != vendors.QuotaScopeGlobal || b.Limit != 32 {
		t.Errorf("CPUS_ALL_REGIONS = %+v, %v", b, ok)
	}
	if _, err := e.d.Quotas(t.Context(), "mars-north1"); err == nil {
		t.Error("the quota of an unknown region")
	}
}

// TestNoConfirmedSeries: the production table is empty, so every type is unknown
// and takes the reactive path until a live run confirms a series.
func TestNoConfirmedSeries(t *testing.T) {
	if len(gcp.ConfirmedSeries) != 0 {
		t.Fatalf("the confirmed series are %v: a series is added only with the lane's quota scenario as its proof", gcp.ConfirmedSeries)
	}
	d := gcp.New()
	if _, ok := d.Consumption("e2-small", vendors.CapacityOnDemand); ok {
		t.Error("consumption is known without confirmed series")
	}
}

func TestQuotaExceededIsReactive(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.c.SetLimit(testRegion, "CPUS", 0)
	_, err := e.d.CreateInstance(t.Context(), request(`{}`))
	if !vendors.IsClass(err, vendors.QuotaExceeded) || vendors.IsAccountWide(err) {
		t.Errorf("a region quota: %v, want QuotaExceeded, not account-wide", err)
	}
	e = newEnv(t, envOptions{})
	e.c.SetLimit("", "CPUS_ALL_REGIONS", 0)
	_, err = e.d.CreateInstance(t.Context(), request(`{}`))
	if !vendors.IsClass(err, vendors.QuotaExceeded) || !vendors.IsAccountWide(err) {
		t.Errorf("the account quota: %v, want an account-wide QuotaExceeded", err)
	}
}

func TestLaunchableTypes(t *testing.T) {
	e := newEnv(t, envOptions{bare: true})
	e.c.AddMachineType(fake.SeedZoneA, "a2-highgpu-1g", 12, 1, "X86_64")
	e.c.AddMachineType(fake.SeedZoneA, "n2-custom-4-8192", 4, 8192, "X86_64")
	e.c.AddMachineType(fake.SeedZoneA, "c3-standard-192-metal", 192, 1, "X86_64")
	e.c.RemoveMachineType(fake.SeedZoneB, "e2-small")
	types, err := e.d.LaunchableTypes(t.Context(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	for _, dropped := range []string{"a2-highgpu-1g", "n2-custom-4-8192", "c3-standard-192-metal"} {
		if _, ok := types[dropped]; ok {
			t.Errorf("%s is offered", dropped)
		}
	}
	// A machine type a zone does not list is not offered there.
	if zs := types["e2-small"]; len(zs) != 1 || zs[0] != "2000" {
		t.Errorf("e2-small zones = %v", zs)
	}
	if zs := types["n2-standard-2"]; len(zs) != 2 {
		t.Errorf("n2-standard-2 zones = %v", zs)
	}
	if e.d.NodeOptionsEpoch() == 0 {
		t.Error("the epoch did not change with the machine types")
	}
}
