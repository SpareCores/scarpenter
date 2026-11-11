/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"net/netip"
	"sync"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

const (
	netName = "scarpenter-conformance-subnets"
	subA    = "scarpenter-conformance-subnets-europe-west3"
	subB    = "scarpenter-conformance-subnets-us-central1"
)

func rangeOf(t *testing.T, e *env, region, name string) string {
	t.Helper()
	s, err := e.c.GetSubnetwork(t.Context(), &computepb.GetSubnetworkRequest{Project: testProject, Region: region, Subnetwork: name})
	if err != nil {
		t.Fatal(err)
	}
	return s.GetIpCidrRange()
}

// One global network, one subnetwork per region with distinct /20s of the pool,
// the same after a restart, and deleted with the last subnetwork.
func TestManagedNetworkTwoRegionsAndRestart(t *testing.T) {
	e := newEnv(t, envOptions{})
	a, created, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(subA, ""))
	if err != nil || !created {
		t.Fatalf("region A: %v, %v", created, err)
	}
	b, created, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedOtherReg, spec(subB, ""))
	if err != nil || !created {
		t.Fatalf("region B: %v, %v", created, err)
	}
	if a.Network != netName || b.Network != netName {
		t.Errorf("networks %q and %q, want the one global %q", a.Network, b.Network, netName)
	}
	nets, err := e.c.ListNetworks(t.Context(), &computepb.ListNetworksRequest{Project: testProject})
	if err != nil || len(nets.GetItems()) != 3 { // the seeded "vpc", "default" and ours
		t.Errorf("%d networks, %v: want the seeded two and one managed", len(nets.GetItems()), err)
	}
	if got := rangeOf(t, e, fake.SeedRegion, subA); got != "10.240.0.0/20" {
		t.Errorf("region A range %s", got)
	}
	if got := rangeOf(t, e, fake.SeedOtherReg, subB); got != "10.240.16.0/20" {
		t.Errorf("region B range %s", got)
	}

	// A restart: the same subnetworks are adopted, nothing is allocated again.
	e2 := restart(t, e.c, envOptions{})
	for region, name := range map[string]string{fake.SeedRegion: subA, fake.SeedOtherReg: subB} {
		again, created, err := e2.d.Ensure(t.Context(), gcp.KindSubnets, region, spec(name, ""))
		if err != nil || created || again.ID == "" {
			t.Errorf("after a restart %s: %v, %v", region, created, err)
		}
	}
	if got := rangeOf(t, e, fake.SeedRegion, subA); got != "10.240.0.0/20" {
		t.Errorf("range changed after a restart: %s", got)
	}
	// A restart that lost a subnetwork (deleted by hand) takes the first free /20: the hole.
	if err := e2.d.Delete(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e2.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(subA, "")); err != nil {
		t.Fatal(err)
	}
	if got := rangeOf(t, e, fake.SeedRegion, subA); got != "10.240.0.0/20" {
		t.Errorf("the hole was not reused: %s", got)
	}

	// The network lives while one subnetwork does, and goes with the last.
	if err := e2.d.Delete(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.GetNetwork(t.Context(), &computepb.GetNetworkRequest{Project: testProject, Network: netName}); err != nil {
		t.Errorf("the network went with the first subnetwork: %v", err)
	}
	listed, err := e2.d.List(t.Context(), nil, nil)
	if err != nil || len(listed) != 1 || listed[0].Name != subA {
		t.Errorf("List = %+v, %v", listed, err)
	}
	last := listed[0]
	if err := e2.d.Delete(t.Context(), last); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.GetNetwork(t.Context(), &computepb.GetNetworkRequest{Project: testProject, Network: netName}); !gcp.IsNotFound(err) {
		t.Errorf("the network outlived its last subnetwork: %v", err)
	}
}

// The allocation steps over subnetworks that exist: a foreign range in the
// managed network, secondary ranges, and a pool of the NodeClass.
func TestManagedSubnetAllocation(t *testing.T) {
	e := newEnv(t, envOptions{})
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(subA, "")); err != nil {
		t.Fatal(err)
	}
	// Someone puts a /24 at the next /20 and a /16 inside the one after.
	for i, c := range []string{"10.240.16.0/24", "10.240.32.0/28"} {
		name := "foreign" + string(rune('a'+i))
		op, err := e.c.InsertSubnetwork(t.Context(), &computepb.InsertSubnetworkRequest{Project: testProject, Region: fake.SeedOtherReg,
			SubnetworkResource: &computepb.Subnetwork{Name: &name, IpCidrRange: &c,
				Network: new("projects/" + testProject + "/global/networks/" + netName)}})
		if err != nil {
			t.Fatal(err)
		}
		_ = op
	}
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedOtherReg, spec(subB, "")); err != nil {
		t.Fatal(err)
	}
	if got := rangeOf(t, e, fake.SeedOtherReg, subB); got != "10.240.48.0/20" {
		t.Errorf("range %s, want the first /20 that overlaps nothing", got)
	}

	// A pool of the NodeClass: another subnetwork, taken from it.
	c := e.c
	e2 := restart(t, c, envOptions{})
	pooled := "scarpenter-conformance-subnets-europe-west3-pool"
	if _, _, err := e2.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(pooled, `{"pool":"172.20.0.0/16"}`)); err != nil {
		t.Fatal(err)
	}
	if got := rangeOf(t, e, fake.SeedRegion, pooled); got != "172.20.0.0/20" {
		t.Errorf("pooled range %s", got)
	}
	// The pool is checked again by the driver: the CRD already did.
	for _, p := range []string{`{"pool":"8.8.0.0/16"}`, `{"pool":"10.0.0.0/24"}`, `{"pool":"x"}`} {
		if _, _, err := e2.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec("scarpenter-conformance-subnets-bad", p)); vendors.ClassOf(err) != vendors.Fatal {
			t.Errorf("pool %s: %v, want Fatal", p, err)
		}
	}
	// A pool with no free /20 left.
	for _, region := range []string{fake.SeedOtherReg} {
		if _, _, err := e2.d.Ensure(t.Context(), gcp.KindSubnets, region, spec("scarpenter-conformance-subnets-full", `{"pool":"172.20.0.0/20"}`)); vendors.ClassOf(err) != vendors.Fatal {
			t.Errorf("an exhausted pool: %v, want Fatal", err)
		}
	}
}

// Regions ensured at the same time (the engine serialises one region, not all)
// never collide.
func TestManagedSubnetAllocationConcurrent(t *testing.T) {
	e := newEnv(t, envOptions{})
	var wg sync.WaitGroup
	regions := []string{fake.SeedRegion, fake.SeedOtherReg}
	errs := make([]error, len(regions))
	for i, r := range regions {
		wg.Go(func() {
			_, _, errs[i] = e.d.Ensure(t.Context(), gcp.KindSubnets, r, spec("scarpenter-conformance-subnets-"+r, ""))
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	a := netip.MustParsePrefix(rangeOf(t, e, fake.SeedRegion, subA))
	b := netip.MustParsePrefix(rangeOf(t, e, fake.SeedOtherReg, subB))
	if a.Overlaps(b) {
		t.Errorf("%s and %s overlap", a, b)
	}
}

// Another process takes the range between the listing and the insert: the real
// error is retried with a fresh listing.
func TestManagedSubnetRangeRace(t *testing.T) {
	e := newEnv(t, envOptions{})
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(subA, "")); err != nil {
		t.Fatal(err)
	}
	e.c.Inject("InsertSubnetwork", fake.Error(400, "invalid",
		"Invalid IPCidrRange: 10.240.16.0/20 conflicts with existing subnetwork 'other' in region 'us-central1'."), 1)
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedOtherReg, spec(subB, "")); err != nil {
		t.Fatalf("the conflict was not retried: %v", err)
	}
	e.c.Inject("InsertSubnetwork", fake.Error(400, "invalid",
		"Invalid IPCidrRange: x conflicts with existing subnetwork 'other' in region 'r'."), 10)
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedOtherReg, spec("scarpenter-conformance-subnets-again", "")); err == nil {
		t.Error("an insert that always conflicts succeeded")
	}
}

func TestParsePool(t *testing.T) {
	for p, ok := range map[string]bool{
		"10.240.0.0/12": true, "10.0.0.0/8": true, "172.16.0.0/12": true, "172.31.240.0/20": true, "192.168.0.0/16": true,
		"192.168.0.0/20": true, "10.0.0.0/7": false, "172.0.0.0/8": false, "100.64.0.0/10": false, "10.0.0.0/21": false,
		"10.0.0.1/8": false, "::/0": false, "": false, "fd00::/8": false,
	} {
		if _, err := gcp.ParsePool(p); (err == nil) != ok {
			t.Errorf("ParsePool(%q) = %v, want ok=%v", p, err, ok)
		}
	}
}
