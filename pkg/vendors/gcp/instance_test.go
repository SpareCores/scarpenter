/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

var reqCounter atomic.Int32

// request is a launch of e2-small in zone a with the seeded subnetwork.
func request(settings string) vendors.CreateRequest {
	n := reqCounter.Add(1)
	name := fmt.Sprintf("claim-%d", n)
	s := vendors.VendorSettings{Raw: json.RawMessage(settings)}
	return vendors.CreateRequest{
		Region: fake.SeedRegion, Zone: fake.SeedZoneA, InstanceType: "e2-small", CapacityType: vendors.CapacityOnDemand,
		Arch: "amd64", Image: vendors.Image{Ref: "image"}, UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Settings: s,
		Prepared: &vendors.Prepared{Resources: []vendors.ResourceRef{
			{Kind: gcp.KindSubnets, Region: fake.SeedRegion, ID: gcp.SubnetworkURL(testProject, fake.SeedRegion, fake.SeedSubnet)},
			{Kind: gcp.KindFirewalls, ID: "nodes"}, {Kind: gcp.KindRunFirewalls, ID: "scarpenter-run-1"},
		}},
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: testCluster, vendors.TagNodeClaim: name, vendors.TagNodePool: "pool",
			vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		ExtraTags:      map[string]string{"team": "data"},
		IdempotencyKey: fmt.Sprintf("0f8fad5b-d9cb-469f-a165-%012d", n),
		Node:           vendors.NodeInfo{Name: name},
	}
}

func TestCreateWritesTheInstance(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(`{"metadata":{"owner":"me"},"networking":{}}`)
	req.Settings.Networking.PublicIPv4 = new(false)
	inst, err := e.d.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ref != gcp.NewRef(fake.SeedRegion, fake.SeedZoneA, req.Node.Name) || inst.Zone != fake.SeedZoneA ||
		inst.State != vendors.StateRunning && inst.State != vendors.StatePending || inst.BillingStart.IsZero() ||
		len(inst.Addresses) == 0 {
		t.Fatalf("instance = %+v", inst)
	}
	var got *computepb.Instance
	for _, i := range e.c.Instances() {
		if i.GetName() == req.Node.Name {
			got = i
		}
	}
	if got == nil {
		t.Fatal("the fake has no instance")
	}
	if got.GetLabels()["scarpenter-cluster"] != testCluster || got.GetLabels()["team"] != "data" ||
		got.GetLabels()["scarpenter-idempotency-key"] != req.IdempotencyKey {
		t.Errorf("labels = %v", got.GetLabels())
	}
	if tags := got.GetTags().GetItems(); len(tags) != 2 || tags[0] != "nodes" || tags[1] != "scarpenter-run-1" {
		t.Errorf("network tags = %v", tags)
	}
	if got.GetScheduling().GetProvisioningModel() != "STANDARD" || got.GetScheduling().GetOnHostMaintenance() != "MIGRATE" {
		t.Errorf("scheduling = %v", got.GetScheduling())
	}
	if len(got.GetNetworkInterfaces()[0].GetAccessConfigs()) != 0 {
		t.Errorf("a public IPv4 was asked off: %v", got.GetNetworkInterfaces()[0].GetAccessConfigs())
	}
	boot := got.GetDisks()[0]
	if !boot.GetAutoDelete() || boot.GetInitializeParams().GetDiskSizeGb() != 64 ||
		boot.GetInitializeParams().GetSourceImage() != "projects/"+testProject+"/global/images/image" {
		t.Errorf("boot disk = %v", boot)
	}
	meta := map[string]string{}
	for _, m := range got.GetMetadata().GetItems() {
		meta[m.GetKey()] = m.GetValue()
	}
	if meta["owner"] != "me" || meta["block-project-ssh-keys"] != "TRUE" || meta["enable-oslogin"] != "FALSE" ||
		meta["user-data"] != string(req.UserData) {
		t.Errorf("metadata = %v", meta)
	}
}

func TestCreateSpotDataDisksAndPlacement(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.c.SetLimit(testRegion, "PREEMPTIBLE_CPUS", 8)
	// A managed policy through the provider of the test.
	pol, _, err := e.d.Ensure(t.Context(), gcp.KindResourcePolicies, testRegion, vendors.CreateSpec{
		Name:   "scarpenter-conformance-resourcePolicies-europe-west3",
		Tags:   map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClass: "class"},
		Params: json.RawMessage(`{"collocated":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	req := request(`{"storage":{"data":[{"name":"pgdata","diskType":"pd-ssd","sizeGb":50,"mountPath":"/d"}]}}`)
	req.InstanceType, req.CapacityType = "n2-standard-2", vendors.CapacitySpot
	req.Settings.Placement = json.RawMessage(`{"resourcePolicy":{"groupPlacement":{"collocation":"COLLOCATED"}}}`)
	req.Prepared.Resources = append(req.Prepared.Resources, pol)
	if _, err := e.d.CreateInstance(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	var got *computepb.Instance
	for _, i := range e.c.Instances() {
		got = i
	}
	s := got.GetScheduling()
	if s.GetProvisioningModel() != "SPOT" || s.GetInstanceTerminationAction() != "DELETE" || s.GetOnHostMaintenance() != "TERMINATE" {
		t.Errorf("scheduling = %v", s)
	}
	if len(got.GetDisks()) != 2 || got.GetDisks()[1].GetDeviceName() != "pgdata" || !got.GetDisks()[1].GetAutoDelete() {
		t.Errorf("disks = %v", got.GetDisks())
	}
	if len(got.GetResourcePolicies()) != 1 {
		t.Errorf("policies = %v", got.GetResourcePolicies())
	}
}

func TestCreateRefusals(t *testing.T) {
	e := newEnv(t, envOptions{})
	cases := map[string]struct {
		mutate func(*vendors.CreateRequest)
		class  vendors.Class
	}{
		"a bad settings block":    {func(r *vendors.CreateRequest) { r.Settings.Raw = json.RawMessage(`{`) }, vendors.Fatal},
		"another project":         {func(r *vendors.CreateRequest) { r.Settings.Raw = json.RawMessage(`{"project":"other-project"}`) }, vendors.Fatal},
		"a root disk too small":   {func(r *vendors.CreateRequest) { r.Image.RootDiskBytes = 100 << 30 }, vendors.Fatal},
		"a bad placement":         {func(r *vendors.CreateRequest) { r.Settings.Placement = json.RawMessage(`{`) }, vendors.Fatal},
		"a bad network profile":   {func(r *vendors.CreateRequest) { r.Settings.Network = json.RawMessage(`{`) }, vendors.Fatal},
		"a reserved metadata key": {func(r *vendors.CreateRequest) { r.Settings.Raw = json.RawMessage(`{"metadata":{"ssh-keys":"x"}}`) }, vendors.Fatal},
	}
	for name, tc := range cases {
		req := request(`{}`)
		tc.mutate(&req)
		if _, err := e.d.CreateInstance(t.Context(), req); vendors.ClassOf(err) != tc.class {
			t.Errorf("%s: %v, want %s", name, err, tc.class)
		}
	}
}

// TestCreateWithoutASubnetUsesTheDefault: a launch whose caller prepared no
// subnetwork falls back to the project's default network, and has nothing to
// fall back to without one.
func TestCreateWithoutASubnetUsesTheDefault(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(`{}`)
	req.Prepared = &vendors.Prepared{}
	if _, err := e.d.CreateInstance(t.Context(), req); err != nil {
		t.Fatalf("with a default network: %v", err)
	}
	bare := newEnv(t, envOptions{bare: true})
	req = request(`{}`)
	req.Prepared = &vendors.Prepared{}
	if _, err := bare.d.CreateInstance(t.Context(), req); vendors.ClassOf(err) != vendors.Unsupported {
		t.Errorf("without a default network: %v, want Unsupported", err)
	}
}

// TestRequestIDCarriesTheAttempt: a failed operation is not replayed. The fake
// replays the operation of a request ID, failed or not (the real behaviour is
// unverified), so a retry of the same offering that reused the ID would get the
// failure again; the driver counts the attempt up after a failure.
func TestRequestIDCarriesTheAttempt(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(`{}`)
	e.c.Stockout(fake.SeedZoneA, "e2-small", false, true)
	if _, err := e.d.CreateInstance(t.Context(), req); !vendors.IsClass(err, vendors.InsufficientCapacity) {
		t.Fatalf("the stockout: %v", err)
	}
	e.c.Stockout(fake.SeedZoneA, "e2-small", false, false)
	if _, err := e.d.CreateInstance(t.Context(), req); err != nil {
		t.Fatalf("the retry of the same offering replayed the failure: %v", err)
	}
	if n := e.c.Calls("InsertInstance"); n != 2 {
		t.Errorf("%d inserts, want 2", n)
	}
}

// TestALostResponseKeepsTheRequestID: an insert whose operation is still running
// when the driver gives up is retried with the same request ID, so the API
// answers with the original operation and no second instance is made.
func TestALostResponseKeepsTheRequestID(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.c.SetOpPolls(1 << 30)
	e.c.Inject("WaitZoneOperation", fake.Error(504, "backendError", "deadline"), 1)
	req := request(`{}`)
	if _, err := e.d.CreateInstance(t.Context(), req); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("the lost wait: %v", err)
	}
	// The retry finds the instance by its idempotency label (it exists already).
	inst, err := e.d.CreateInstance(t.Context(), req)
	if err != nil || inst.Ref != gcp.NewRef(fake.SeedRegion, fake.SeedZoneA, req.Node.Name) {
		t.Fatalf("the retry: %v, %v", inst, err)
	}
	if n := len(e.c.Instances()); n != 1 {
		t.Errorf("%d instances, want 1", n)
	}
}

// laggingList hides the instances from the next aggregated lists (the lookup of
// a key right after an insert may not see it yet) and records the request IDs
// of the inserts.
type laggingList struct {
	*fake.Compute
	hide       int
	requestIDs []string
}

func (l *laggingList) AggregatedListInstances(
	ctx context.Context, req *computepb.AggregatedListInstancesRequest,
) (*computepb.InstanceAggregatedList, error) {
	if l.hide > 0 {
		l.hide--
		return &computepb.InstanceAggregatedList{}, nil
	}
	return l.Compute.AggregatedListInstances(ctx, req)
}

func (l *laggingList) InsertInstance(
	ctx context.Context, req *computepb.InsertInstanceRequest,
) (*computepb.Operation, error) {
	l.requestIDs = append(l.requestIDs, req.GetRequestId())
	return l.Compute.InsertInstance(ctx, req)
}

// TestAFailedWaitKeepsTheRequestID: a wait that fails in transport (a 5xx, not
// the deadline) says nothing about the insert, so the retry of the same offering
// reuses the request ID and gets the original operation back.
func TestAFailedWaitKeepsTheRequestID(t *testing.T) {
	e := newEnv(t, envOptions{})
	api := &laggingList{Compute: e.c, hide: 2}
	d := gcp.New(gcp.WithAPI(api), gcp.WithProject(testProject), gcp.WithConfirmedSeries(confirmed),
		gcp.WithCallTimeouts(5*time.Second, 5*time.Second), gcp.WithLaunchWait(10*time.Second))
	if err := d.Init(t.Context(), vendors.Config{ClusterName: testCluster, Clock: e.clock, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	e.c.Inject("WaitZoneOperation", fake.Error(503, "backendError", "unavailable"), 1)
	req := request(`{}`)
	if _, err := d.CreateInstance(t.Context(), req); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("the failed wait: %v", err)
	}
	if _, err := d.CreateInstance(t.Context(), req); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if len(api.requestIDs) != 2 || api.requestIDs[0] == "" || api.requestIDs[0] != api.requestIDs[1] {
		t.Errorf("the request IDs %q, want the same one twice", api.requestIDs)
	}
	if n := len(e.c.Instances()); n != 1 {
		t.Errorf("%d instances, want 1", n)
	}
}

func TestNameTakenByAnother(t *testing.T) {
	e := newEnv(t, envOptions{})
	first := request(`{}`)
	if _, err := e.d.CreateInstance(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	// Another claim with the same instance name in the same zone: 409, not ours.
	other := request(`{}`)
	other.Node.Name = first.Node.Name
	if _, err := e.d.CreateInstance(t.Context(), other); vendors.ClassOf(err) != vendors.Fatal ||
		vendors.CodeOf(err) != gcp.CodeAlreadyExists {
		t.Errorf("a taken name: %v, want Fatal ALREADY_EXISTS", err)
	}
}

func TestOrganizationPolicyViolation(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.c.SetOrgPolicyViolation(true)
	_, err := e.d.CreateInstance(t.Context(), request(`{}`))
	if vendors.ClassOf(err) != vendors.Fatal || vendors.CodeOf(err) != gcp.CodeOrgPolicy {
		t.Errorf("an organization policy: %v, want Fatal %s", err, gcp.CodeOrgPolicy)
	}
}

func TestDeleteRefusesAForeignInstance(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := request(`{}`)
	req.Tags[vendors.TagCluster] = "another-cluster"
	inst, err := e.d.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.DeleteInstance(t.Context(), inst.Ref); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a foreign instance: %v, want Fatal", err)
	}
	if len(e.c.Instances()) != 1 {
		t.Error("the foreign instance was deleted")
	}
}

func TestListFindAndStates(t *testing.T) {
	e := newEnv(t, envOptions{})
	a, b := request(`{}`), request(`{}`)
	b.Zone = fake.SeedZoneB
	ia, err := e.d.CreateInstance(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := e.d.CreateInstance(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.d.ListInstances(t.Context(), []string{fake.SeedRegion}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if other, _ := e.d.ListInstances(t.Context(), []string{fake.SeedOtherReg}, nil); len(other) != 0 {
		t.Errorf("another region lists %v", other)
	}
	for status, want := range map[string]string{
		gcp.StatusProvisioning: vendors.StatePending, gcp.StatusStaging: vendors.StatePending,
		gcp.StatusRunning: vendors.StateRunning, gcp.StatusRepairing: vendors.StateRunning,
		gcp.StatusStopping: vendors.StateTerminating, gcp.StatusStopped: vendors.StateTerminating,
		gcp.StatusTerminated: vendors.StateTerminating, gcp.StatusSuspended: vendors.StateTerminating,
		"SOMETHING_NEW": vendors.StateRunning,
	} {
		_, zone, name, _ := gcp.ParseRef(ia.Ref)
		e.c.SetStatus(zone, name, status)
		if got, err := e.d.GetInstance(t.Context(), ia.Ref); err != nil || got.State != want {
			t.Errorf("status %s: %v, %v; want %s", status, got, err, want)
		}
	}
	found, err := e.d.FindInstance(t.Context(), fake.SeedRegion, map[vendors.TagKey]string{vendors.TagNodeClaim: b.Node.Name})
	if err != nil || found.Ref != ib.Ref {
		t.Errorf("Find = %v, %v", found, err)
	}
	if _, err := e.d.FindInstance(t.Context(), fake.SeedOtherReg, map[vendors.TagKey]string{vendors.TagNodeClaim: b.Node.Name}); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("Find in another region: %v", err)
	}
	if _, err := e.d.GetInstance(t.Context(), gcp.NewRef(fake.SeedRegion, fake.SeedZoneA, "nope")); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("Get of a missing instance: %v", err)
	}
	// The unreachable zone of a partial answer does not fail the list.
	e.c.SetZoneUnreachable(fake.SeedZoneB, true)
	if list, err := e.d.ListInstances(t.Context(), []string{fake.SeedRegion}, nil); err != nil || len(list) != 1 {
		t.Errorf("a list with an unreachable zone = %v, %v", list, err)
	}
}

func TestDeleteWaitsAndIsIdempotent(t *testing.T) {
	e := newEnv(t, envOptions{})
	inst, err := e.d.CreateInstance(t.Context(), request(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.c.Instances()) != 0 {
		t.Error("the instance is not gone")
	}
	// An operation that fails is an error of the call.
	again, err := e.d.CreateInstance(t.Context(), request(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e.c.InjectOpError("DeleteInstance", "INTERNAL_ERROR", "boom", 500)
	if err := e.d.DeleteInstance(t.Context(), again.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("a failed delete operation: %v, want Retryable", err)
	}
}
