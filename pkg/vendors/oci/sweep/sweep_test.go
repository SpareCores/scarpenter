/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/sweep"
)

const (
	cluster = "scarpenter-ci-100"
	other   = "scarpenter-ci-200"
	region  = fake.SeedRegion
)

var (
	ctx = context.Background()
	now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	// Three hours ago is old enough (limit two hours), thirty minutes is not.
	oldAt   = now.Add(-3 * time.Hour)
	youngAt = now.Add(-30 * time.Minute)
)

// world builds on the seeded fake: helpers that create resources as of a time.
type world struct {
	t *testing.T
	c *fake.Cloud
}

func newWorld(t *testing.T) *world {
	t.Helper()
	c := fake.NewSeeded()
	c.AddShape(region, fake.FlexShape("VM.Standard.E5.Flex", 64, 1024, 1, 64, 0, 0, "standard-e5-core-count"))
	return &world{t: t, c: c}
}

func (w *world) at(ts time.Time) { w.c.SetClock(func() time.Time { return ts }) }

func (w *world) check(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

// managed tags: what the driver writes on a resource it manages.
func managed(c string) map[string]string {
	return map[string]string{
		sweep.ClusterTag: c, sweep.ManagedByTag: "scarpenter",
		"scarpenter-nodeclass": "e2e-create", "scarpenter-resource-kind": "subnets",
	}
}

func byoTags(c string) map[string]string { return map[string]string{sweep.LaneTag: c} }

// network creates a VCN with the parts of a managed network (gateway, route table, security list, regional
// subnet) and a group, all named name and tagged tags; it returns the IDs by part.
func (w *world) network(name string, tags map[string]string, at time.Time) map[string]string {
	w.t.Helper()
	w.at(at)
	ids := map[string]string{}
	vcn, err := w.c.CreateVcn(ctx, region, core.CreateVcnRequest{CreateVcnDetails: core.CreateVcnDetails{
		CompartmentId: new(fake.SeedCompartment), CidrBlock: new("10.240.0.0/16"), DisplayName: new(name), FreeformTags: tags}})
	w.check(err)
	ids["vcn"] = *vcn.Id
	igw, err := w.c.CreateInternetGateway(ctx, region, core.CreateInternetGatewayRequest{
		CreateInternetGatewayDetails: core.CreateInternetGatewayDetails{CompartmentId: new(fake.SeedCompartment),
			VcnId: vcn.Id, IsEnabled: new(true), DisplayName: new(name + "-igw"), FreeformTags: tags}})
	w.check(err)
	ids["igw"] = *igw.Id
	rt, err := w.c.CreateRouteTable(ctx, region, core.CreateRouteTableRequest{CreateRouteTableDetails: core.CreateRouteTableDetails{
		CompartmentId: new(fake.SeedCompartment), VcnId: vcn.Id, DisplayName: new(name + "-rt"), FreeformTags: tags,
		RouteRules: []core.RouteRule{{NetworkEntityId: igw.Id, Destination: new("0.0.0.0/0")}}}})
	w.check(err)
	ids["rt"] = *rt.Id
	sl, err := w.c.CreateSecurityList(ctx, region, core.CreateSecurityListRequest{CreateSecurityListDetails: core.CreateSecurityListDetails{
		CompartmentId: new(fake.SeedCompartment), VcnId: vcn.Id, DisplayName: new(name + "-sl"), FreeformTags: tags}})
	w.check(err)
	ids["sl"] = *sl.Id
	sn, err := w.c.CreateSubnet(ctx, region, core.CreateSubnetRequest{CreateSubnetDetails: core.CreateSubnetDetails{
		CompartmentId: new(fake.SeedCompartment), VcnId: vcn.Id, CidrBlock: new("10.240.0.0/20"), DisplayName: new(name),
		FreeformTags: tags, RouteTableId: rt.Id, SecurityListIds: []string{*sl.Id}}})
	w.check(err)
	ids["subnet"] = *sn.Id
	g, err := w.c.CreateNetworkSecurityGroup(ctx, region, core.CreateNetworkSecurityGroupRequest{
		CreateNetworkSecurityGroupDetails: core.CreateNetworkSecurityGroupDetails{CompartmentId: new(fake.SeedCompartment),
			VcnId: vcn.Id, DisplayName: new(name + "-nodes"), FreeformTags: tags}})
	w.check(err)
	ids["nsg"] = *g.Id
	return ids
}

// instance launches an instance (a running one, in the seeded subnet) of a cluster at a time.
func (w *world) instance(c string, at time.Time, tags map[string]string) string {
	w.t.Helper()
	w.at(at)
	all := map[string]string{}
	if c != "" {
		all[sweep.ClusterTag] = c
	}
	maps.Copy(all, tags)
	res, err := w.c.LaunchInstance(ctx, region, core.LaunchInstanceRequest{LaunchInstanceDetails: core.LaunchInstanceDetails{
		CompartmentId: new(fake.SeedCompartment), AvailabilityDomain: new(fake.ADFull(fake.SeedZoneA)),
		Shape: new("VM.Standard.E5.Flex"), ShapeConfig: &core.LaunchInstanceShapeConfigDetails{Ocpus: new(float32(1)),
			MemoryInGBs: new(float32(4))},
		DisplayName: new("node"), FreeformTags: all,
		SourceDetails:     core.InstanceSourceViaImageDetails{ImageId: new(fake.SeedImage), BootVolumeSizeInGBs: new(int64(64))},
		CreateVnicDetails: &core.CreateVnicDetails{SubnetId: new(fake.SeedSubnet)},
	}})
	w.check(err)
	return *res.Id
}

func (w *world) sweep(o sweep.Options) *sweep.Report {
	w.t.Helper()
	o.Compartment, o.Tenancy = fake.SeedCompartment, fake.SeedTenancy
	o.Now = func() time.Time { return now }
	o.PollInterval = time.Millisecond
	o.WaitDeleted = 200 * time.Millisecond
	o.Logf = w.t.Logf
	return w.run(o, nil)
}

func (w *world) run(o sweep.Options, vol sweep.VolumesAPI) *sweep.Report {
	w.t.Helper()
	rep, err := sweep.Run(ctx, oci.Clients{Compute: w.c, Network: w.c, Identity: w.c, Limits: w.c}, vol, o)
	if err != nil {
		w.t.Fatal(err)
	}
	return rep
}

// counts are the VCNs, subnets, gateways, route tables, security lists and groups left. The seeded
// tenancy has one VCN, one subnet and two groups of its own.
type counts struct{ vcns, subnets, gateways, routes, lists, groups int }

func (w *world) counts() counts {
	v, s, g, r, l, n := w.c.NetworkCounts()
	return counts{v, s, g, r, l, n}
}

var seeded = counts{vcns: 1, subnets: 1, groups: 2}

func (w *world) instanceState(id string) core.InstanceLifecycleStateEnum {
	in, ok := w.c.Instance(id)
	if !ok {
		w.t.Fatalf("no instance %s", id)
	}
	return in.LifecycleState
}

func outcomes(rep *sweep.Report, kind string) []sweep.Outcome {
	var out []sweep.Outcome
	for _, i := range rep.Items {
		if i.Kind == kind {
			out = append(out, i.Outcome)
		}
	}
	return out
}

// fakeVolumes is a Block Volume service with a list of boot volumes.
type fakeVolumes struct {
	mu      sync.Mutex
	volumes []core.BootVolume
	deleted []string
	// attached are the IDs OCI refuses to delete (409).
	attached map[string]bool
}

func (f *fakeVolumes) ListBootVolumes(_ context.Context, _ string, req core.ListBootVolumesRequest,
) (core.ListBootVolumesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var items []core.BootVolume
	for _, v := range f.volumes {
		if *v.CompartmentId == *req.CompartmentId && *v.AvailabilityDomain == *req.AvailabilityDomain {
			items = append(items, v)
		}
	}
	return core.ListBootVolumesResponse{Items: items}, nil
}

func (f *fakeVolumes) DeleteBootVolume(_ context.Context, _ string, req core.DeleteBootVolumeRequest,
) (core.DeleteBootVolumeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attached[*req.BootVolumeId] {
		return core.DeleteBootVolumeResponse{}, fake.Error(http.StatusConflict, "Conflict", "the volume is attached")
	}
	f.deleted = append(f.deleted, *req.BootVolumeId)
	return core.DeleteBootVolumeResponse{}, nil
}

func (f *fakeVolumes) add(id string, state core.BootVolumeLifecycleStateEnum, at time.Time, tags map[string]string) {
	f.volumes = append(f.volumes, core.BootVolume{Id: new(id), CompartmentId: new(fake.SeedCompartment),
		AvailabilityDomain: new(fake.ADFull(fake.SeedZoneA)), LifecycleState: state, TimeCreated: &common.SDKTime{Time: at},
		FreeformTags: tags})
}

func TestSweepsAClusterInDependencyOrder(t *testing.T) {
	w := newWorld(t)
	w.network("scarpenter-"+cluster+"-subnets", managed(cluster), oldAt)
	id := w.instance(cluster, oldAt, nil)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if got := w.counts(); got != seeded {
		t.Errorf("left %+v, want only the seeded %+v", got, seeded)
	}
	if st := w.instanceState(id); st != core.InstanceLifecycleStateTerminated {
		t.Errorf("the instance is %s", st)
	}
	kinds := make([]string, 0, len(rep.Items))
	for _, i := range rep.Items {
		if i.Outcome != sweep.Deleted {
			t.Errorf("not deleted: %+v", i)
		}
		kinds = append(kinds, i.Kind)
	}
	want := []string{"instance", "network-security-group", "subnet", "route-table", "internet-gateway", "security-list", "vcn"}
	if !slices.Equal(kinds, want) {
		t.Errorf("order %v, want %v", kinds, want)
	}
	if bv := w.c.BootVolumes(); len(bv) != 0 {
		t.Errorf("boot volumes left: %v (the terminate deletes them)", bv)
	}
	if w.c.Calls("TerminateInstance") != 1 {
		t.Errorf("TerminateInstance was called %d times", w.c.Calls("TerminateInstance"))
	}
}

func TestAnotherClustersResourcesAreNotTouched(t *testing.T) {
	w := newWorld(t)
	w.network("scarpenter-"+other+"-subnets", managed(other), oldAt)
	id := w.instance(other, oldAt, nil)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if len(rep.Items) != 0 {
		t.Errorf("items for another cluster: %+v", rep.Items)
	}
	if got := w.counts(); got != (counts{vcns: 2, subnets: 2, gateways: 1, routes: 1, lists: 1, groups: 3}) {
		t.Errorf("counts changed: %+v", got)
	}
	if st := w.instanceState(id); st != core.InstanceLifecycleStateRunning {
		t.Errorf("the instance is %s", st)
	}
}

func TestThePrefixSelectsEveryClusterOfTheLane(t *testing.T) {
	w := newWorld(t)
	w.network("scarpenter-"+cluster+"-subnets", managed(cluster), oldAt)
	w.network("scarpenter-"+other+"-subnets", managed(other), oldAt)
	w.instance("unrelated", oldAt, nil)
	w.instance(sweep.DefaultClusterPrefix, oldAt, nil) // the bare prefix is no cluster
	rep := w.sweep(sweep.Options{Regions: []string{region}})
	if got := w.counts(); got != seeded {
		t.Errorf("left %+v", got)
	}
	if n := rep.Count(sweep.Deleted); n != 6*2 {
		t.Errorf("deleted %d, want 12: %+v", n, rep.Items)
	}
	if rep.Count(sweep.Failed) != 0 {
		t.Errorf("failures: %+v", rep.Items)
	}
}

func TestOnlyOldResourcesAreSwept(t *testing.T) {
	w := newWorld(t)
	w.network("scarpenter-"+cluster+"-subnets", managed(cluster), youngAt)
	id := w.instance(cluster, youngAt, nil)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}, MaxAge: 2 * time.Hour})
	if rep.Count(sweep.Deleted) != 0 || rep.Count(sweep.Skipped) == 0 {
		t.Errorf("young resources: %+v", rep.Items)
	}
	if got := w.counts(); got.vcns != 2 || w.instanceState(id) != core.InstanceLifecycleStateRunning {
		t.Errorf("young resources were touched: %+v", got)
	}
	for _, i := range rep.Items {
		if !strings.Contains(i.Reason, "only ") {
			t.Errorf("the reason of %+v does not say why", i)
		}
	}
	// Without a limit the same resources go.
	w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if got := w.counts(); got != seeded {
		t.Errorf("left %+v", got)
	}
}

func TestAnInstanceWithoutACreationTimeUsesTheTag(t *testing.T) {
	w := newWorld(t)
	w.instance(cluster, youngAt, map[string]string{sweep.CreatedAtTag: "1"})
	// TimeCreated is set by the fake; the tag is the fallback only, so a young instance is skipped.
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}, MaxAge: 2 * time.Hour})
	if got := outcomes(rep, "instance"); !slices.Equal(got, []sweep.Outcome{sweep.Skipped}) {
		t.Errorf("outcomes %v", got)
	}
}

func TestAResourceNeedsItsMarkerAndItsName(t *testing.T) {
	w := newWorld(t)
	// The cluster tag alone, without managed-by: not SCarpenter's.
	w.network("scarpenter-"+cluster+"-nomark", map[string]string{sweep.ClusterTag: cluster}, oldAt)
	// The marker with another name: not the cluster's resource.
	w.network("somebody-elses-vcn", managed(cluster), oldAt)
	// A name that fits without a tag: untouched and not even reported.
	w.network("scarpenter-"+cluster+"-untagged", nil, oldAt)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if rep.Count(sweep.Deleted) != 0 {
		t.Errorf("deleted something it had no right to: %+v", rep.Items)
	}
	if got := w.counts(); got != (counts{vcns: 4, subnets: 4, gateways: 3, routes: 3, lists: 3, groups: 5}) {
		t.Errorf("counts changed: %+v", got)
	}
	reasons := map[string]bool{}
	for _, i := range rep.Items {
		if i.Outcome != sweep.Skipped {
			t.Errorf("%+v", i)
		}
		reasons[i.Reason] = true
	}
	if len(reasons) != 2 {
		t.Errorf("want one reason for the missing marker and one for the name: %v", reasons)
	}
	if rep.Count(sweep.Failed) != 0 {
		t.Errorf("failures: %+v", rep.Items)
	}
}

func TestTheLanesBroughtResourcesGoByTheirOwnTag(t *testing.T) {
	w := newWorld(t)
	byo := sweep.ByoNamePrefix + "100"
	w.network(byo, byoTags(cluster), oldAt)
	// The byo name without the lane's tag, and with another run's, stay.
	w.network(sweep.ByoNamePrefix+"101", nil, oldAt)
	w.network(sweep.ByoNamePrefix+"102", byoTags(other), oldAt)
	// The driver's marker on a byo name is not the lane's: nothing.
	w.network(sweep.ByoNamePrefix+"103", managed(cluster), oldAt)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if n := rep.Count(sweep.Deleted); n != 6 {
		t.Errorf("deleted %d, want the six parts of the one byo network: %+v", n, rep.Items)
	}
	if got := w.counts(); got != (counts{vcns: 4, subnets: 4, gateways: 3, routes: 3, lists: 3, groups: 5}) {
		t.Errorf("counts %+v", got)
	}
}

func TestAForeignSubnetKeepsTheVCNAlive(t *testing.T) {
	w := newWorld(t)
	ids := w.network("scarpenter-"+cluster+"-subnets", managed(cluster), oldAt)
	foreign := w.c.SeedSubnet(region, ids["vcn"], "users-subnet", "10.240.16.0/20", "", nil, false, false)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if _, ok := w.c.Subnet(foreign); !ok {
		t.Fatal("a subnet that is not SCarpenter's was deleted")
	}
	if got := outcomes(rep, "vcn"); !slices.Equal(got, []sweep.Outcome{sweep.Skipped}) {
		t.Errorf("the VCN: %v", got)
	}
	if got := w.counts(); got.vcns != 2 || got.subnets != 2 {
		t.Errorf("counts %+v", got)
	}
}

func TestADryRunDeletesNothing(t *testing.T) {
	w := newWorld(t)
	w.network("scarpenter-"+cluster+"-subnets", managed(cluster), oldAt)
	id := w.instance(cluster, oldAt, nil)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}, DryRun: true})
	if rep.Count(sweep.WouldDelete) != 7 || rep.Count(sweep.Deleted) != 0 {
		t.Errorf("%+v", rep.Items)
	}
	if got := w.counts(); got.vcns != 2 || w.instanceState(id) != core.InstanceLifecycleStateRunning {
		t.Errorf("a dry run changed something: %+v", got)
	}
	for _, op := range []string{"TerminateInstance", "DeleteVcn", "DeleteSubnet", "DeleteNetworkSecurityGroup"} {
		if w.c.Calls(op) != 0 {
			t.Errorf("%s was called in a dry run", op)
		}
	}
}

func TestAResourceStillInUseIsRetried(t *testing.T) {
	w := newWorld(t)
	w.network("scarpenter-"+cluster+"-subnets", managed(cluster), oldAt)
	w.c.Inject("DeleteSubnet", fake.Error(http.StatusConflict, "Conflict", "the subnet is in use"), 2)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if got := w.counts(); got != seeded {
		t.Errorf("left %+v: %+v", got, rep.Items)
	}
	if w.c.Calls("DeleteSubnet") != 3 {
		t.Errorf("DeleteSubnet was called %d times", w.c.Calls("DeleteSubnet"))
	}
}

func TestAResourceThatStaysInUseFailsAfterTheWait(t *testing.T) {
	w := newWorld(t)
	w.network("scarpenter-"+cluster+"-subnets", managed(cluster), oldAt)
	w.c.Inject("DeleteNetworkSecurityGroup", fake.Error(http.StatusConflict, "Conflict", "in use"), 1000)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if got := outcomes(rep, "network-security-group"); !slices.Equal(got, []sweep.Outcome{sweep.Failed}) {
		t.Errorf("group: %v", got)
	}
	// The later stages still ran, but the VCN has its group left: its own delete fails.
	if got := outcomes(rep, "subnet"); !slices.Equal(got, []sweep.Outcome{sweep.Deleted}) {
		t.Errorf("subnet: %v", got)
	}
	if got := outcomes(rep, "vcn"); !slices.Equal(got, []sweep.Outcome{sweep.Failed}) {
		t.Errorf("vcn: %v", got)
	}
}

func TestAnInstanceThatIsAlreadyTerminatingIsOnlyWaitedFor(t *testing.T) {
	w := newWorld(t)
	id := w.instance(cluster, oldAt, nil)
	w.c.SetState(id, core.InstanceLifecycleStateTerminating)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if w.c.Calls("TerminateInstance") != 0 {
		t.Error("an instance that is terminating was terminated again")
	}
	// Still terminating after the wait: the failure says so, and the later stages go on.
	if got := outcomes(rep, "instance"); !slices.Equal(got, []sweep.Outcome{sweep.Failed}) || !strings.Contains(rep.Items[0].Reason, "TERMINATING") {
		t.Errorf("%+v", rep.Items)
	}
}

func TestATerminatedInstanceIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	id := w.instance(cluster, oldAt, nil)
	w.c.SetState(id, core.InstanceLifecycleStateTerminated)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if len(rep.Items) != 0 || w.c.Calls("TerminateInstance") != 0 {
		t.Errorf("%+v", rep.Items)
	}
}

func TestAnInstanceThatFailsToTerminateIsReported(t *testing.T) {
	w := newWorld(t)
	w.instance(cluster, oldAt, nil)
	w.c.Inject("TerminateInstance", fake.Error(http.StatusInternalServerError, "InternalError", "boom"), 1)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if got := outcomes(rep, "instance"); !slices.Equal(got, []sweep.Outcome{sweep.Failed}) {
		t.Errorf("%+v", rep.Items)
	}
}

func TestEverySubscribedRegionIsSweptWithoutRegions(t *testing.T) {
	w := newWorld(t)
	w.instance(cluster, oldAt, nil)
	rep := w.sweep(sweep.Options{Cluster: cluster})
	if rep.Count(sweep.Deleted) != 1 || rep.Count(sweep.Failed) != 0 {
		t.Errorf("%+v", rep.Items)
	}
	// Three subscribed regions are listed per stage; the unsubscribed one is not.
	if n := w.c.Calls("ListInstances"); n != 3 {
		t.Errorf("ListInstances was called %d times, want one per subscribed region", n)
	}
}

func TestADeniedRegionIsSkippedOnlyWhenTheRegionsWereNotAsked(t *testing.T) {
	denied := fake.Error(http.StatusForbidden, "NotAuthorized", "no access")
	w := newWorld(t)
	w.c.Inject("ListInstances", denied, 1)
	rep := w.sweep(sweep.Options{Cluster: cluster})
	if n := rep.Count(sweep.Skipped); n != 1 || rep.Count(sweep.Failed) != 0 {
		t.Errorf("all regions: %+v", rep.Items)
	}
	w = newWorld(t)
	w.c.Inject("ListInstances", denied, 1)
	rep = w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if n := rep.Count(sweep.Failed); n != 1 || rep.Count(sweep.Skipped) != 0 {
		t.Errorf("asked region: %+v", rep.Items)
	}
}

func TestAFailedListIsAFailureOfTheRegionAndTheOtherStagesRun(t *testing.T) {
	w := newWorld(t)
	w.network("scarpenter-"+cluster+"-subnets", managed(cluster), oldAt)
	w.c.Inject("ListSubnets", fake.Error(http.StatusInternalServerError, "InternalError", "boom"), 1000)
	rep := w.sweep(sweep.Options{Cluster: cluster, Regions: []string{region}})
	if rep.Count(sweep.Failed) < 1 {
		t.Errorf("no failure: %+v", rep.Items)
	}
	if got := outcomes(rep, "network-security-group"); !slices.Equal(got, []sweep.Outcome{sweep.Deleted}) {
		t.Errorf("an earlier stage did not run: %+v", rep.Items)
	}
	// The subnets could not be listed, so none was deleted and the route table is still in use by
	// one: the later stage ran, and OCI's answer is the failure.
	if got := outcomes(rep, "route-table"); !slices.Equal(got, []sweep.Outcome{sweep.Failed}) {
		t.Errorf("a later stage did not run: %+v", rep.Items)
	}
}

func TestBootVolumesAreOnlySweptWhenAsked(t *testing.T) {
	vol := &fakeVolumes{attached: map[string]bool{"bv-attached": true}}
	vol.add("bv-old", core.BootVolumeLifecycleStateAvailable, oldAt, nil)
	vol.add("bv-young", core.BootVolumeLifecycleStateAvailable, youngAt, nil)
	vol.add("bv-terminating", core.BootVolumeLifecycleStateTerminating, oldAt, nil)
	vol.add("bv-attached", core.BootVolumeLifecycleStateAvailable, oldAt, nil)
	vol.add("bv-other", core.BootVolumeLifecycleStateAvailable, oldAt, map[string]string{sweep.ClusterTag: other})
	vol.add("bv-tagged", core.BootVolumeLifecycleStateAvailable, oldAt, map[string]string{sweep.ClusterTag: cluster})
	w := newWorld(t)
	opts := sweep.Options{Compartment: fake.SeedCompartment, Tenancy: fake.SeedTenancy, Cluster: cluster,
		Regions: []string{region}, MaxAge: 2 * time.Hour, Now: func() time.Time { return now }}
	rep := w.run(opts, vol)
	if len(rep.Items) != 0 || len(vol.deleted) != 0 {
		t.Fatalf("without BootVolumes: %+v %v", rep.Items, vol.deleted)
	}
	opts.BootVolumes = true
	rep = w.run(opts, vol)
	slices.Sort(vol.deleted)
	if want := []string{"bv-old", "bv-tagged"}; !slices.Equal(vol.deleted, want) {
		t.Errorf("deleted %v, want %v: %+v", vol.deleted, want, rep.Items)
	}
	if got := rep.Count(sweep.Skipped); got != 2 { // the young one and the attached one
		t.Errorf("skipped %d: %+v", got, rep.Items)
	}
	// A dry run deletes nothing.
	vol.deleted = nil
	opts.DryRun = true
	rep = w.run(opts, vol)
	if len(vol.deleted) != 0 || rep.Count(sweep.WouldDelete) != 3 { // an attached one is not known to be
		t.Errorf("dry run: %v %+v", vol.deleted, rep.Items)
	}
}

func TestBootVolumesNeedTheirClient(t *testing.T) {
	w := newWorld(t)
	_, err := sweep.Run(ctx, oci.Clients{Compute: w.c, Network: w.c, Identity: w.c, Limits: w.c}, nil,
		sweep.Options{Compartment: fake.SeedCompartment, Tenancy: fake.SeedTenancy, BootVolumes: true})
	if err == nil {
		t.Error("no error without the volumes client")
	}
	if _, err = sweep.Run(ctx, oci.Clients{}, nil, sweep.Options{}); err == nil {
		t.Error("no error without the compartment and tenancy")
	}
}

func TestTheRegionListIsAFailureOfTheWholeSweep(t *testing.T) {
	w := newWorld(t)
	w.c.Inject("ListRegionSubscriptions", fake.Error(http.StatusInternalServerError, "InternalError", "boom"), 1)
	_, err := sweep.Run(ctx, oci.Clients{Compute: w.c, Network: w.c, Identity: w.c, Limits: w.c}, nil,
		sweep.Options{Compartment: fake.SeedCompartment, Tenancy: fake.SeedTenancy})
	if err == nil || !strings.Contains(err.Error(), "region subscriptions") {
		t.Errorf("err = %v", err)
	}
}

func TestParseRegions(t *testing.T) {
	for in, want := range map[string][]string{
		"": nil, "all": nil, "ALL": nil, " eu-frankfurt-1 , us-ashburn-1,eu-frankfurt-1,": {"eu-frankfurt-1", "us-ashburn-1"},
	} {
		got, err := sweep.ParseRegions(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("ParseRegions(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := sweep.ParseRegions("all,eu-frankfurt-1"); err == nil {
		t.Error("all with a region name was accepted")
	}
}

func TestNamesAgreeWithTheDriver(t *testing.T) {
	if sweep.ClusterTag != oci.TagKey("cluster") || sweep.ManagedByTag != oci.TagKey("managed-by") {
		t.Errorf("the tags differ from the driver's: %s %s", sweep.ClusterTag, sweep.ManagedByTag)
	}
	if got := sweep.ResourceNamePrefix("c1"); got != "scarpenter-c1-" {
		t.Errorf("prefix %q", got)
	}
}
