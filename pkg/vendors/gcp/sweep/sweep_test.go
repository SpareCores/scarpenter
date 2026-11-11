/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/sweep"
)

const proj = "my-project"

var (
	ctx = context.Background()
	now = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	// A resource created three hours ago is old enough (limit two hours), one
	// created thirty minutes ago is not.
	oldAt   = now.Add(-3 * time.Hour)
	youngAt = now.Add(-30 * time.Minute)
)

// world builds on the seeded fake: helpers that create resources as of a time.
type world struct {
	t *testing.T
	c *fake.Compute
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return &world{t: t, c: fake.NewSeeded(proj)}
}

func (w *world) at(ts time.Time) { w.c.SetClock(func() time.Time { return ts }) }

func (w *world) must(op *computepb.Operation, err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	_ = op
}

func (w *world) network(name, desc string) {
	w.t.Helper()
	w.must(w.c.InsertNetwork(ctx, &computepb.InsertNetworkRequest{Project: proj, NetworkResource: &computepb.Network{
		Name: new(name), Description: new(desc), AutoCreateSubnetworks: new(false)}}))
}

func (w *world) subnet(region, name, network, cidr, desc string) {
	w.t.Helper()
	w.must(w.c.InsertSubnetwork(ctx, &computepb.InsertSubnetworkRequest{Project: proj, Region: region,
		SubnetworkResource: &computepb.Subnetwork{Name: new(name), Description: new(desc), IpCidrRange: new(cidr),
			Network: new("projects/" + proj + "/global/networks/" + network)}}))
}

func (w *world) firewall(name, network, desc string) {
	w.t.Helper()
	w.must(w.c.InsertFirewall(ctx, &computepb.InsertFirewallRequest{Project: proj, FirewallResource: &computepb.Firewall{
		Name: new(name), Description: new(desc), Network: new("projects/" + proj + "/global/networks/" + network)}}))
}

func (w *world) policy(region, name, desc string) {
	w.t.Helper()
	w.must(w.c.InsertResourcePolicy(ctx, &computepb.InsertResourcePolicyRequest{Project: proj, Region: region,
		ResourcePolicyResource: &computepb.ResourcePolicy{Name: new(name), Description: new(desc)}}))
}

// instance creates a running instance in the subnet (a URL of projects/...), with the
// labels and, optionally, a resource policy.
func (w *world) instance(zone, name, subnetURL string, labels map[string]string, policy string) {
	w.t.Helper()
	in := &computepb.Instance{
		Name: new(name), MachineType: new("zones/" + zone + "/machineTypes/e2-small"), Labels: labels,
		Disks: []*computepb.AttachedDisk{{Boot: new(true), AutoDelete: new(true),
			InitializeParams: &computepb.AttachedDiskInitializeParams{
				SourceImage: new("projects/" + fake.SeedImageProject + "/global/images/" + fake.SeedImage)}}},
		NetworkInterfaces: []*computepb.NetworkInterface{{Subnetwork: new(subnetURL)}},
		Scheduling:        &computepb.Scheduling{ProvisioningModel: new("STANDARD"), OnHostMaintenance: new("MIGRATE")},
	}
	if policy != "" {
		in.ResourcePolicies = []string{"projects/" + proj + "/regions/" + gcp.RegionOfZone(zone) + "/resourcePolicies/" + policy}
	}
	w.must(w.c.InsertInstance(ctx, &computepb.InsertInstanceRequest{Project: proj, Zone: zone, InstanceResource: in}))
}

func subnetURL(region, name string) string {
	return "projects/" + proj + "/regions/" + region + "/subnetworks/" + name
}

func (w *world) instances() []string {
	all := w.c.Instances()
	out := make([]string, 0, len(all))
	for _, in := range all {
		out = append(out, in.GetName())
	}
	return out
}

func (w *world) has(kind, region, name string) bool {
	w.t.Helper()
	switch kind {
	case "network":
		_, err := w.c.GetNetwork(ctx, &computepb.GetNetworkRequest{Project: proj, Network: name})
		return err == nil
	case "subnet":
		_, err := w.c.GetSubnetwork(ctx, &computepb.GetSubnetworkRequest{Project: proj, Region: region, Subnetwork: name})
		return err == nil
	case "policy":
		_, err := w.c.GetResourcePolicy(ctx, &computepb.GetResourcePolicyRequest{Project: proj, Region: region, ResourcePolicy: name})
		return err == nil
	case "firewall":
		l, err := w.c.ListFirewalls(ctx, &computepb.ListFirewallsRequest{Project: proj})
		if err != nil {
			w.t.Fatal(err)
		}
		return slices.ContainsFunc(l.GetItems(), func(f *computepb.Firewall) bool { return f.GetName() == name })
	}
	w.t.Fatalf("unknown kind %s", kind)
	return false
}

func opts(o sweep.Options) sweep.Options {
	o.Now = func() time.Time { return now }
	o.PollInterval = time.Millisecond
	if o.WaitDeleted == 0 {
		o.WaitDeleted = 2 * time.Second
	}
	return o
}

func run(t *testing.T, w *world, o sweep.Options) *sweep.Report {
	t.Helper()
	// As in the command: every call under a deadline.
	api := gcp.WithTimeouts(w.c, 5*time.Second, 5*time.Second)
	rep, err := sweep.Run(ctx, api, proj, opts(o))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// itemOf finds the item of a kind and ID.
func itemOf(rep *sweep.Report, kind, id string) *sweep.Item {
	for i := range rep.Items {
		if rep.Items[i].Kind == kind && rep.Items[i].ID == id {
			return &rep.Items[i]
		}
	}
	return nil
}

func wantOutcome(t *testing.T, rep *sweep.Report, kind, id string, want sweep.Outcome) {
	t.Helper()
	it := itemOf(rep, kind, id)
	if it == nil {
		t.Fatalf("no item for %s %s in %+v", kind, id, rep.Items)
	}
	if it.Outcome != want {
		t.Fatalf("%s %s: outcome %s (%s), want %s", kind, id, it.Outcome, it.Reason, want)
	}
}

const (
	ciCluster = "scarpenter-ci-100"
	prefix    = "scarpenter-scarpenter-ci-100-"
	netName   = prefix + "subnets"
	fwName    = prefix + "e2e-amd64-europe-west3"
	snName    = prefix + "subnets-europe-west3"
	polName   = prefix + "placement-europe-west3-abc"
)

func marker(cluster, kind string) string { return gcp.MarkerDescription(cluster, "e2e-amd64", kind) }

// managed builds the managed network of a run as of the clock: a network, a subnet, a
// firewall rule, a resource policy and an instance that uses the subnet and the policy.
func managed(w *world, at time.Time) {
	w.at(at)
	w.network(netName, marker(ciCluster, "subnets"))
	w.subnet(fake.SeedRegion, snName, netName, "10.240.16.0/20", marker(ciCluster, "subnets"))
	w.firewall(fwName, netName, marker(ciCluster, "firewalls"))
	w.policy(fake.SeedRegion, polName, marker(ciCluster, "resourcePolicies"))
	w.instance(fake.SeedZoneA, "node-1", subnetURL(fake.SeedRegion, snName),
		map[string]string{sweep.ClusterLabel: ciCluster}, polName)
}

func TestSweepsAManagedRunInDependencyOrder(t *testing.T) {
	w := newWorld(t)
	managed(w, oldAt)
	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	// Everything the run made is gone, and it took the dependency order for the fake to
	// accept it: the policy and the subnet are refused while the instance exists, the
	// network while the subnet or the rule exists.
	if rep.Count(sweep.Failed) != 0 {
		t.Fatalf("failures: %+v", rep.Items)
	}
	if got := w.instances(); len(got) != 0 {
		t.Fatalf("instances left: %v", got)
	}
	for _, k := range [][3]string{{"network", "", netName}, {"subnet", fake.SeedRegion, snName},
		{"policy", fake.SeedRegion, polName}, {"firewall", "", fwName}} {
		if w.has(k[0], k[1], k[2]) {
			t.Errorf("%s %s is still there", k[0], k[2])
		}
	}
	wantOutcome(t, rep, "instance", fake.SeedZoneA+"/node-1", sweep.Deleted)
	wantOutcome(t, rep, "resource-policy", polName, sweep.Deleted)
	wantOutcome(t, rep, "firewall", fwName, sweep.Deleted)
	wantOutcome(t, rep, "subnet", snName, sweep.Deleted)
	wantOutcome(t, rep, "network", netName, sweep.Deleted)
	// The order of the report is the order of the deletions.
	var kinds []string
	for _, it := range rep.Items {
		if it.Outcome == sweep.Deleted {
			kinds = append(kinds, it.Kind)
		}
	}
	if want := []string{"instance", "resource-policy", "firewall", "subnet", "network"}; !slices.Equal(kinds, want) {
		t.Fatalf("order %v, want %v", kinds, want)
	}
	// The seeded world's own network is not the sweep's business.
	if !w.has("network", "", "vpc") || !w.has("subnet", fake.SeedRegion, fake.SeedSubnet) {
		t.Fatal("the sweep deleted what is not marked")
	}
}

func TestYoungResourcesAreLeftAlone(t *testing.T) {
	w := newWorld(t)
	managed(w, youngAt)
	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	if rep.Count(sweep.Deleted) != 0 || rep.Count(sweep.Failed) != 0 {
		t.Fatalf("a young run was swept: %+v", rep.Items)
	}
	if len(w.instances()) != 1 || !w.has("network", "", netName) || !w.has("firewall", "", fwName) ||
		!w.has("subnet", fake.SeedRegion, snName) || !w.has("policy", fake.SeedRegion, polName) {
		t.Fatal("something of a young run is gone")
	}
	wantOutcome(t, rep, "instance", fake.SeedZoneA+"/node-1", sweep.Skipped)
	wantOutcome(t, rep, "network", netName, sweep.Skipped)
	if !strings.Contains(itemOf(rep, "instance", fake.SeedZoneA+"/node-1").Reason, "only") {
		t.Fatalf("the reason does not say why: %+v", itemOf(rep, "instance", fake.SeedZoneA+"/node-1"))
	}
}

func TestNoAgeLimitSweepsAYoungRunOfThatCluster(t *testing.T) {
	// What the teardown does for its own cluster: -max-age 0.
	w := newWorld(t)
	managed(w, youngAt)
	rep := run(t, w, sweep.Options{Cluster: ciCluster, Regions: []string{fake.SeedRegion}})
	if rep.Count(sweep.Failed) != 0 || len(w.instances()) != 0 || w.has("network", "", netName) {
		t.Fatalf("the cluster was not swept: %+v", rep.Items)
	}
}

func TestOnlyTheSelectedClustersAndBothMarkers(t *testing.T) {
	w := newWorld(t)
	w.at(oldAt)
	other := "scarpenter-prod-1"
	// The lane's cluster by prefix; a cluster that does not start with it; the right
	// description under the wrong name; the right name with a foreign description; a
	// description of a different format; no description at all.
	w.network(netName, marker(ciCluster, "subnets"))
	w.network("scarpenter-"+other+"-subnets", marker(other, "subnets"))
	w.network("my-own-network", marker(ciCluster, "subnets"))
	w.network(prefix+"foreign", "created by hand")
	w.network(prefix+"nodescription", "")
	// A bare prefix is not a cluster: "scarpenter-ci-" alone names none.
	w.network("scarpenter-scarpenter-ci--x", marker("scarpenter-ci-", "subnets"))
	w.instance(fake.SeedZoneA, "keep-1", subnetURL(fake.SeedRegion, fake.SeedSubnet), map[string]string{sweep.ClusterLabel: other}, "")
	w.instance(fake.SeedZoneA, "keep-2", subnetURL(fake.SeedRegion, fake.SeedSubnet), nil, "")
	w.instance(fake.SeedZoneA, "keep-3", subnetURL(fake.SeedRegion, fake.SeedSubnet), map[string]string{"scarpenter-cluster": "not-" + ciCluster}, "")
	w.instance(fake.SeedZoneA, "drop", subnetURL(fake.SeedRegion, fake.SeedSubnet), map[string]string{sweep.ClusterLabel: ciCluster}, "")

	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	if got := w.instances(); !slices.Equal(got, []string{"keep-1", "keep-2", "keep-3"}) {
		t.Fatalf("instances left %v", got)
	}
	for name, want := range map[string]bool{
		netName: false, "scarpenter-" + other + "-subnets": true, "my-own-network": true,
		prefix + "foreign": true, prefix + "nodescription": true, "scarpenter-scarpenter-ci--x": true,
	} {
		if got := w.has("network", "", name); got != want {
			t.Errorf("network %s exists = %t, want %t", name, got, want)
		}
	}
	// The name that does not carry the cluster prefix is reported: it is a resource of a
	// selected cluster that the sweep refuses to touch.
	it := itemOf(rep, "network", "my-own-network")
	if it == nil || it.Outcome != sweep.Skipped || !strings.Contains(it.Reason, "does not start with") {
		t.Fatalf("the misnamed network: %+v", it)
	}
	if itemOf(rep, "network", "scarpenter-"+other+"-subnets") != nil {
		t.Fatal("another cluster's resource is in the report")
	}
}

func TestRegionsNarrowTheSweep(t *testing.T) {
	w := newWorld(t)
	w.at(oldAt)
	w.network(netName, marker(ciCluster, "subnets"))
	w.subnet(fake.SeedRegion, snName, netName, "10.240.16.0/20", marker(ciCluster, "subnets"))
	w.subnet(fake.SeedOtherReg, prefix+"subnets-us-central1", netName, "10.240.32.0/20", marker(ciCluster, "subnets"))
	w.policy(fake.SeedOtherReg, prefix+"placement-us-central1", marker(ciCluster, "resourcePolicies"))
	w.instance(fake.SeedZoneA, "eu", subnetURL(fake.SeedRegion, snName), map[string]string{sweep.ClusterLabel: ciCluster}, "")
	w.instance(fake.SeedOtherZ, "us", subnetURL(fake.SeedOtherReg, prefix+"subnets-us-central1"),
		map[string]string{sweep.ClusterLabel: ciCluster}, "")

	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	if got := w.instances(); !slices.Equal(got, []string{"us"}) {
		t.Fatalf("instances left %v", got)
	}
	if !w.has("subnet", fake.SeedOtherReg, prefix+"subnets-us-central1") || !w.has("policy", fake.SeedOtherReg, prefix+"placement-us-central1") {
		t.Fatal("the other region was swept")
	}
	if w.has("subnet", fake.SeedRegion, snName) {
		t.Fatal("the swept region's subnet is still there")
	}
	// A network is global: it stays while a subnet of another region is in it.
	if !w.has("network", "", netName) {
		t.Fatal("a network with a subnet left was deleted")
	}
	it := itemOf(rep, "network", netName)
	if it == nil || it.Outcome != sweep.Skipped || !strings.Contains(it.Reason, "us-central1/"+prefix+"subnets-us-central1") {
		t.Fatalf("the network's item: %+v", it)
	}

	// Every region next: now it all goes.
	run(t, w, sweep.Options{MaxAge: 2 * time.Hour})
	if len(w.instances()) != 0 || w.has("network", "", netName) {
		t.Fatal("the all-regions sweep left something")
	}
}

func TestDryRunDeletesNothing(t *testing.T) {
	w := newWorld(t)
	managed(w, oldAt)
	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, DryRun: true})
	if rep.Count(sweep.Deleted) != 0 || rep.Count(sweep.WouldDelete) != 5 {
		t.Fatalf("dry run: %+v", rep.Items)
	}
	for _, k := range []string{"DeleteInstance", "DeleteResourcePolicy", "DeleteFirewall", "DeleteSubnetwork", "DeleteNetwork"} {
		if n := w.c.Calls(k); n != 0 {
			t.Errorf("a dry run called %s %d times", k, n)
		}
	}
	if len(w.instances()) != 1 || !w.has("network", "", netName) {
		t.Fatal("a dry run deleted something")
	}
	// The network would go too: its only subnet would.
	wantOutcome(t, rep, "network", netName, sweep.WouldDelete)
}

func TestTheLanesOwnResourcesGoByTheirMarker(t *testing.T) {
	w := newWorld(t)
	w.at(oldAt)
	byo := sweep.ByoNamePrefix + "100"
	w.network(byo, sweep.LaneMarker(ciCluster))
	w.subnet(fake.SeedRegion, byo+"-europe-west3", byo, "10.241.0.0/24", sweep.LaneMarker(ciCluster))
	w.firewall(byo+"-nodes", byo, sweep.LaneMarker(ciCluster))
	// Not the lane's: the same prefix with another run's cluster, and no marker.
	w.network(sweep.ByoNamePrefix+"200", sweep.LaneMarker("scarpenter-other-200"))
	w.network(sweep.ByoNamePrefix+"300", "")

	rep := run(t, w, sweep.Options{Cluster: ciCluster, Regions: []string{fake.SeedRegion}})
	if rep.Count(sweep.Failed) != 0 {
		t.Fatalf("failures: %+v", rep.Items)
	}
	if w.has("network", "", byo) || w.has("subnet", fake.SeedRegion, byo+"-europe-west3") || w.has("firewall", "", byo+"-nodes") {
		t.Fatal("the lane's byo resources are still there")
	}
	if !w.has("network", "", sweep.ByoNamePrefix+"200") || !w.has("network", "", sweep.ByoNamePrefix+"300") {
		t.Fatal("a byo network of another run (or none) was deleted")
	}
	// What SCarpenter's own garbage collector reads is not a lane marker, and the reverse:
	// the controller must never take the byo resources for its own.
	if _, _, _, ok := gcp.ParseMarker(sweep.LaneMarker(ciCluster)); ok {
		t.Fatal("the lane marker is a SCarpenter marker: the controller would delete the byo resources")
	}
	if strings.HasPrefix(byo, gcp.ResourceNamePrefix(ciCluster)) {
		t.Fatal("the byo name carries the controller's name prefix")
	}
}

func TestADeletionThatFindsTheResourceInUseIsRetried(t *testing.T) {
	w := newWorld(t)
	managed(w, oldAt)
	w.c.Inject("DeleteSubnetwork", fake.Error(400, "resourceInUseByAnotherResource", "in use by an instance still shutting down"), 2)
	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	if rep.Count(sweep.Failed) != 0 || w.has("subnet", fake.SeedRegion, snName) || w.has("network", "", netName) {
		t.Fatalf("the retry did not get through: %+v", rep.Items)
	}
	if n := w.c.Calls("DeleteSubnetwork"); n != 3 {
		t.Fatalf("DeleteSubnetwork called %d times, want 3", n)
	}
}

func TestAResourceThatStaysInUseFails(t *testing.T) {
	w := newWorld(t)
	managed(w, oldAt)
	w.c.Inject("DeleteSubnetwork", fake.Error(400, "resourceInUseByAnotherResource", "in use"), 1000)
	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}, WaitDeleted: 20 * time.Millisecond})
	wantOutcome(t, rep, "subnet", snName, sweep.Failed)
	if rep.Count(sweep.Failed) != 1 {
		t.Fatalf("failures: %+v", rep.Items)
	}
	// Its network is not deleted behind its back.
	if !w.has("network", "", netName) {
		t.Fatal("the network was deleted with its subnet left")
	}
	wantOutcome(t, rep, "network", netName, sweep.Skipped)
}

func TestFailedOperationsAreReported(t *testing.T) {
	w := newWorld(t)
	managed(w, oldAt)
	w.c.InjectOpError("DeleteInstance", "INTERNAL_ERROR", "boom", 500)
	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}, WaitDeleted: 30 * time.Millisecond})
	wantOutcome(t, rep, "instance", fake.SeedZoneA+"/node-1", sweep.Failed)
	if !strings.Contains(itemOf(rep, "instance", fake.SeedZoneA+"/node-1").Reason, "INTERNAL_ERROR") {
		t.Fatalf("the reason hides the error: %+v", itemOf(rep, "instance", fake.SeedZoneA+"/node-1"))
	}
	// The instance is still there, so the policy and the subnet are refused and reported.
	if rep.Count(sweep.Failed) < 2 {
		t.Fatalf("the dependents were not reported: %+v", rep.Items)
	}
}

func TestAnInstanceThatIsGoneAlreadyCountsAsDeleted(t *testing.T) {
	w := newWorld(t)
	managed(w, oldAt)
	w.c.Inject("DeleteInstance", fake.Error(404, "notFound", "gone"), 1)
	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	wantOutcome(t, rep, "instance", fake.SeedZoneA+"/node-1", sweep.Deleted)
}

func TestStoppedAndTerminatedInstancesAreSwept(t *testing.T) {
	w := newWorld(t)
	w.at(oldAt)
	for _, name := range []string{"stopped", "suspended"} {
		w.instance(fake.SeedZoneA, name, subnetURL(fake.SeedRegion, fake.SeedSubnet), map[string]string{sweep.ClusterLabel: ciCluster}, "")
	}
	w.c.SetStatus(fake.SeedZoneA, "stopped", "TERMINATED")
	w.c.SetStatus(fake.SeedZoneA, "suspended", "SUSPENDED")
	run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	if len(w.instances()) != 0 {
		t.Fatalf("instances left: %v", w.instances())
	}
}

func TestAZoneThatDoesNotAnswerIsNotAnError(t *testing.T) {
	w := newWorld(t)
	w.at(oldAt)
	w.instance(fake.SeedZoneA, "a", subnetURL(fake.SeedRegion, fake.SeedSubnet), map[string]string{sweep.ClusterLabel: ciCluster}, "")
	w.instance(fake.SeedZoneB, "b", subnetURL(fake.SeedRegion, fake.SeedSubnet), map[string]string{sweep.ClusterLabel: ciCluster}, "")
	w.c.SetZoneUnreachable(fake.SeedZoneB, true)
	rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	if rep.Count(sweep.Failed) != 0 || !slices.Equal(w.instances(), []string{"b"}) {
		t.Fatalf("partial success: %+v, instances %v", rep.Items, w.instances())
	}
}

func TestARegionWithoutAccess(t *testing.T) {
	denied := fake.Error(403, "forbidden", "Required 'compute.resourcePolicies.list' permission")
	t.Run("all regions: skipped", func(t *testing.T) {
		w := newWorld(t)
		managed(w, oldAt)
		w.c.Inject("ListResourcePolicies", denied, 1)
		rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour})
		if rep.Count(sweep.Failed) != 0 || rep.Count(sweep.Skipped) != 1 {
			t.Fatalf("items: %+v", rep.Items)
		}
	})
	t.Run("a requested region: a failure", func(t *testing.T) {
		w := newWorld(t)
		managed(w, oldAt)
		w.c.Inject("ListResourcePolicies", denied, 1)
		rep := run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
		if rep.Count(sweep.Failed) == 0 {
			t.Fatalf("a refused region passed: %+v", rep.Items)
		}
	})
}

func TestTheRegionListFailingEndsTheSweep(t *testing.T) {
	w := newWorld(t)
	w.c.Inject("ListRegions", fake.Error(403, "forbidden", "no"), 1)
	if _, err := sweep.Run(ctx, w.c, proj, opts(sweep.Options{})); err == nil {
		t.Fatal("no error without the region list")
	}
}

func TestEverythingPagesAndRunsUnderDeadlines(t *testing.T) {
	w := newWorld(t)
	w.at(oldAt)
	for i := range 12 {
		name := "n" + string(rune('a'+i))
		w.instance(fake.SeedZoneA, name, subnetURL(fake.SeedRegion, fake.SeedSubnet), map[string]string{sweep.ClusterLabel: ciCluster}, "")
	}
	// The fake's page size is 500: a call with a deadline context that is already over
	// fails at the deadline, never hangs.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	rep, err := sweep.Run(dead, gcp.WithTimeouts(w.c, time.Second, time.Second), proj, opts(sweep.Options{Regions: []string{fake.SeedRegion}}))
	if err == nil && len(w.instances()) != 12 {
		t.Fatalf("a cancelled sweep deleted instances: %+v", rep.Items)
	}
	rep = run(t, w, sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{fake.SeedRegion}})
	if rep.Count(sweep.Deleted) != 12 || len(w.instances()) != 0 {
		t.Fatalf("items: %d deleted, left %v", rep.Count(sweep.Deleted), w.instances())
	}
}

func TestParseRegions(t *testing.T) {
	for in, want := range map[string][]string{
		"":                           nil,
		"all":                        nil,
		"ALL":                        nil,
		"europe-west3":               {"europe-west3"},
		" europe-west3 , us-east1,,": {"europe-west3", "us-east1"},
		"us-east1,us-east1":          {"us-east1"},
	} {
		got, err := sweep.ParseRegions(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("ParseRegions(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := sweep.ParseRegions("all,us-east1"); err == nil {
		t.Error("all with a name was accepted")
	}
}

func TestLaneMarker(t *testing.T) {
	c, ok := sweep.ParseLaneMarker(sweep.LaneMarker(ciCluster))
	if !ok || c != ciCluster {
		t.Fatalf("round trip: %q %t", c, ok)
	}
	for _, bad := range []string{"", "scarpenter: cluster=x", sweep.LaneMarkerPrefix, sweep.LaneMarkerPrefix + "cluster="} {
		if _, ok := sweep.ParseLaneMarker(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
