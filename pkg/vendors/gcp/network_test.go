/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

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

// driverOver is a driver of the test cluster over another ComputeAPI (a wrapper
// of the fake that plays a second controller).
func driverOver(t *testing.T, api gcp.ComputeAPI) *gcp.Driver {
	t.Helper()
	d := gcp.New(gcp.WithAPI(api), gcp.WithProject(testProject), gcp.WithConfirmedSeries(confirmed),
		gcp.WithCallTimeouts(5*time.Second, 5*time.Second), gcp.WithLaunchWait(10*time.Second))
	clock := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	if err := d.Init(t.Context(), vendors.Config{ClusterName: testCluster, Clock: clock, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	return d
}

// otherController deletes the managed network with its last subnetwork right
// before the first subnetwork insert it forwards: another controller's garbage
// collection between this one's network check and its insert.
type otherController struct {
	*fake.Compute
	once sync.Once
	t    *testing.T
}

func (o *otherController) InsertSubnetwork(ctx context.Context, r *computepb.InsertSubnetworkRequest,
) (*computepb.Operation, error) {
	o.once.Do(func() {
		if _, err := o.DeleteSubnetwork(ctx, &computepb.DeleteSubnetworkRequest{Project: testProject,
			Region: fake.SeedRegion, Subnetwork: subA}); err != nil {
			o.t.Error(err)
		}
		if _, err := o.DeleteNetwork(ctx, &computepb.DeleteNetworkRequest{Project: testProject, Network: netName}); err != nil {
			o.t.Error(err)
		}
	})
	return o.Compute.InsertSubnetwork(ctx, r)
}

// The network goes (another controller deleted its last subnetwork) between the
// check of the network and the insert of a subnetwork: the network is made again.
func TestManagedNetworkGoneBeforeTheInsert(t *testing.T) {
	e := newEnv(t, envOptions{})
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(subA, "")); err != nil {
		t.Fatal(err)
	}
	d := driverOver(t, &otherController{Compute: e.c, t: t})
	b, created, err := d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedOtherReg, spec(subB, ""))
	if err != nil || !created || b.Network != netName {
		t.Fatalf("Ensure = %+v, %v, %v", b, created, err)
	}
	if got := rangeOf(t, e, fake.SeedOtherReg, subB); got != "10.240.0.0/20" {
		t.Errorf("range %s in the new network", got)
	}
}

// A region that deletes the network's last subnetwork and a region that adds one,
// at the same time in one controller: the network is never deleted under the
// insert.
func TestManagedNetworkDeleteAndEnsureConcurrent(t *testing.T) {
	e := newEnv(t, envOptions{})
	for i := range 20 {
		a, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(subA, ""))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var delErr, ensErr error
		wg.Go(func() { delErr = e.d.Delete(t.Context(), a) })
		wg.Go(func() { _, _, ensErr = e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedOtherReg, spec(subB, "")) })
		wg.Wait()
		if delErr != nil || ensErr != nil {
			t.Fatalf("round %d: delete %v, ensure %v", i, delErr, ensErr)
		}
		if _, err := e.c.GetSubnetwork(t.Context(), &computepb.GetSubnetworkRequest{Project: testProject,
			Region: fake.SeedOtherReg, Subnetwork: subB}); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		b, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedOtherReg, spec(subB, ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.d.Delete(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	}
}

// Two controllers allocate the same range: Compute refuses the second in the
// insert's operation, and the allocation is retried.
func TestManagedSubnetRangeConflictInTheOperation(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.c.InjectOpError("InsertSubnetwork", "INVALID_USAGE",
		"Invalid IPCidrRange: 10.240.0.0/20 conflicts with existing subnetwork 'other' in region 'us-central1'.", 400)
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(subA, "")); err != nil {
		t.Fatalf("the conflict of the operation was not retried: %v", err)
	}
}

// The network outlives its last subnetwork while rules of another NodeClass are
// in it: the subnetwork's deletion is done, the network lists as an orphan, and
// goes once the rules are gone.
func TestManagedNetworkKeptByRules(t *testing.T) {
	e := newEnv(t, envOptions{})
	a, _, err := e.d.Ensure(t.Context(), gcp.KindSubnets, fake.SeedRegion, spec(subA, ""))
	if err != nil {
		t.Fatal(err)
	}
	rule := "scarpenter-conformance-firewalls-other"
	if _, err := e.c.InsertFirewall(t.Context(), &computepb.InsertFirewallRequest{Project: testProject,
		FirewallResource: &computepb.Firewall{Name: &rule, Network: new("projects/" + testProject + "/global/networks/" + netName),
			TargetTags: []string{rule}, Allowed: []*computepb.Allowed{{IPProtocol: new("all")}}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Delete(t.Context(), a); err != nil {
		t.Fatalf("Delete with the network in use = %v", err)
	}
	listed, err := e.d.List(t.Context(), nil, nil)
	if err != nil || len(listed) != 1 || listed[0].Name != netName {
		t.Fatalf("List = %+v, %v: want the orphan network", listed, err)
	}
	if _, err := e.c.DeleteFirewall(t.Context(), &computepb.DeleteFirewallRequest{Project: testProject, Firewall: rule}); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Delete(t.Context(), listed[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.GetNetwork(t.Context(), &computepb.GetNetworkRequest{Project: testProject, Network: netName}); !gcp.IsNotFound(err) {
		t.Errorf("the orphan network was not deleted: %v", err)
	}
}
