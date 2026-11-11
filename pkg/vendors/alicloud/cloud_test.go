/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"errors"
	"testing"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud/fake"
)

const (
	testCluster = "my-cluster"
	testRegion  = "ap-southeast-1"
	testZoneA   = "ap-southeast-1a"
	testZoneB   = "ap-southeast-1b"
	testType    = "ecs.g8y.large"
)

// The fake implements the interfaces of the driver: checked here, because the
// fake does not import this package.
var (
	_ ECSAPI = (*fake.ECS)(nil)
	_ VPCAPI = (*fake.VPC)(nil)
)

type env struct {
	t     *testing.T
	f     *fake.Cloud
	clk   *clocktesting.FakeClock
	cloud *Cloud
	q     *Quotas

	vpcID     string
	vswitches map[string]string
	group     string
	image     string
	sleeps    []time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	f := fake.New(clk)
	f.AddRegion(testRegion, testZoneA, testZoneB)
	f.AddRegion("eu-central-1", "eu-central-1a")
	f.AddType(fake.TypeSpec{Name: testType, VCPUs: 2, MemoryGiB: 8, Arch: "arm64", NVMe: true, Disks: 17})
	f.AddType(fake.TypeSpec{Name: "ecs.g7.xlarge", VCPUs: 4, MemoryGiB: 16, Disks: 17})
	f.AddType(fake.TypeSpec{Name: "ecs.ebmg7.32xlarge", VCPUs: 128, MemoryGiB: 512})
	f.AddType(fake.TypeSpec{Name: "ecs.i4.xlarge", VCPUs: 4, MemoryGiB: 32, LocalDisk: 1})
	f.OfferEverywhere(testRegion)
	e := &env{t: t, f: f, clk: clk}
	e.vpcID, e.vswitches = f.AddDefaultNetwork(testRegion)
	e.image = f.AddImage(testRegion, fake.ImageSpec{Name: "Flatcar-stable-4757.2.3-arm64", Arch: "arm64"})
	e.cloud = NewCloud(testCluster, func(_ context.Context, region string) (Clients, error) {
		return Clients{ECS: f.ECS(region), VPC: f.VPC(region)}, nil
	}, clk)
	e.cloud.sleep = func(_ context.Context, d time.Duration) error { e.sleeps = append(e.sleeps, d); return nil }
	e.q = NewQuotas(e.cloud)
	// The group of the nodes, in the default VPC.
	g, err := e.f.ECS(testRegion).CreateSecurityGroup(context.Background(), sgReq(e.vpcID))
	if err != nil {
		t.Fatal(err)
	}
	e.group = *g.SecurityGroupId
	return e
}

func (e *env) request(claim, uid string) (vendors.CreateRequest, LaunchResources) {
	req := baseRequest()
	req.Region, req.Zone = testRegion, testZoneA
	req.Image = vendors.Image{Ref: e.image, RootDiskBytes: 9 << 30}
	req.Tags = map[vendors.TagKey]string{
		vendors.TagCluster: testCluster, vendors.TagNodeClaim: claim, vendors.TagNodePool: "default",
		vendors.TagNodeClass: "default", vendors.TagManagedBy: vendors.ManagedByValue,
	}
	req.IdempotencyKey = uid
	return req, LaunchResources{VSwitchID: e.vswitches[testZoneA], SecurityGroupIDs: []string{e.group}}
}

func (e *env) create(claim, uid string) (*vendors.Instance, error) {
	req, lr := e.request(claim, uid)
	return e.cloud.CreateInstance(context.Background(), req, lr)
}

func TestCreateGetFindListDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	inst, err := e.create("default-abc12", "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Region != testRegion || inst.Zone != testZoneA || inst.Type != testType || inst.CapacityType != vendors.CapacityOnDemand {
		t.Fatalf("instance = %+v", inst)
	}
	if inst.State != vendors.StateRunning || inst.Tags[vendors.TagNodeClaim] != "default-abc12" || inst.Tags[vendors.TagCluster] != testCluster {
		t.Fatalf("state/tags = %s %v", inst.State, inst.Tags)
	}
	if len(inst.Addresses) != 2 || inst.CreatedAt.IsZero() {
		t.Fatalf("addresses = %v, created %v", inst.Addresses, inst.CreatedAt)
	}
	got, err := e.cloud.GetInstance(ctx, inst.Ref)
	if err != nil || got.Ref != inst.Ref {
		t.Fatalf("get: %v %v", got, err)
	}
	found, err := e.cloud.FindInstance(ctx, testRegion, map[vendors.TagKey]string{vendors.TagNodeClaim: "default-abc12"})
	if err != nil || found.Ref != inst.Ref {
		t.Fatalf("find: %v %v", found, err)
	}
	if _, err := e.cloud.FindInstance(ctx, testRegion, map[vendors.TagKey]string{vendors.TagNodeClaim: "other"}); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("find of nothing: %v", err)
	}
	// Another cluster's instance is listed only without the cluster tag filter.
	list, err := e.cloud.ListInstances(ctx, []string{testRegion, "eu-central-1"}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if err := e.cloud.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cloud.GetInstance(ctx, inst.Ref); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := e.cloud.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatalf("a second delete must succeed: %v", err)
	}
	for _, bad := range []vendors.InstanceRef{"", "garbage", "ap-southeast-1/", "/i-1"} {
		if _, err := e.cloud.GetInstance(ctx, bad); !vendors.IsClass(err, vendors.NotFound) {
			t.Errorf("get(%q): %v", bad, err)
		}
		if err := e.cloud.DeleteInstance(ctx, bad); err != nil {
			t.Errorf("delete(%q): %v", bad, err)
		}
	}
}

func TestListPagesThroughEveryInstance(t *testing.T) {
	e := newEnv(t)
	const n = 230 // more than two pages of 100
	e.f.SetQuota(testRegion, 1_000_000, 1_000_000)
	for i := range n {
		if _, err := e.create("claim-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "uid-"+string(rune('a'+i%26))+string(rune('a'+i/26))); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if i == 150 {
			break
		}
	}
	list, err := e.cloud.ListInstances(context.Background(), []string{testRegion}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 151 {
		t.Fatalf("listed %d, want 151", len(list))
	}
	if got := e.f.Calls("DescribeInstances"); got < 5 { // the creates look up too; the list needs two pages
		t.Fatalf("DescribeInstances calls = %d", got)
	}
}

func TestStoppedInstanceIsTerminatingAndStillListed(t *testing.T) {
	e := newEnv(t)
	inst, _ := e.create("default-abc12", "uid-1")
	e.f.Stop(testRegion, inst.Ref.ID())
	got, err := e.cloud.GetInstance(context.Background(), inst.Ref)
	if err != nil || got.State != vendors.StateTerminating || !got.Gone() {
		t.Fatalf("stopped: %+v %v", got, err)
	}
	list, _ := e.cloud.ListInstances(context.Background(), []string{testRegion}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if len(list) != 1 || list[0].State != vendors.StateTerminating {
		t.Fatalf("list = %+v", list)
	}
	// The garbage collector deletes it.
	if err := e.cloud.DeleteInstance(context.Background(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	if ids := e.f.InstanceIDs(testRegion); len(ids) != 0 {
		t.Fatalf("instances left: %v", ids)
	}
}

func TestPendingThenRunning(t *testing.T) {
	e := newEnv(t)
	e.f.PendingFor = time.Minute
	inst, err := e.create("default-abc12", "uid-1")
	if err != nil || inst.State != vendors.StatePending {
		t.Fatalf("new instance: %+v %v", inst, err)
	}
	e.clk.Step(2 * time.Minute)
	got, _ := e.cloud.GetInstance(context.Background(), inst.Ref)
	if got.State != vendors.StateRunning {
		t.Fatalf("state = %s", got.State)
	}
	// Deleting a pending instance is refused by ECS and retried by the caller.
	inst2, _ := e.create("default-xyz", "uid-2")
	err = e.cloud.DeleteInstance(context.Background(), inst2.Ref)
	if !vendors.IsClass(err, vendors.Retryable) || !isCode(err, "IncorrectInstanceStatus") {
		t.Fatalf("delete of a pending instance: %v", err)
	}
	e.clk.Step(2 * time.Minute)
	if err := e.cloud.DeleteInstance(context.Background(), inst2.Ref); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRefusesAnotherClusterAndUntagged(t *testing.T) {
	e := newEnv(t)
	foreign := e.f.AddForeignInstance(testRegion, testZoneA, testType, false)
	err := e.cloud.DeleteInstance(context.Background(), vendors.NewInstanceRef(testRegion, foreign))
	if vendors.ClassOf(err) != vendors.Fatal || vendors.CodeOf(err) != "NotOwned" {
		t.Fatalf("delete of a foreign instance: %v", err)
	}
	if len(e.f.InstanceIDs(testRegion)) != 1 {
		t.Fatal("the foreign instance must still exist")
	}
	other := NewCloud("other-cluster", e.cloud.factory, e.clk)
	mine, _ := e.create("default-abc12", "uid-1")
	if err := other.DeleteInstance(context.Background(), mine.Ref); vendors.CodeOf(err) != "NotOwned" {
		t.Fatalf("another cluster must not delete it: %v", err)
	}
}

func TestIdempotencyReplayReturnsTheOriginal(t *testing.T) {
	e := newEnv(t)
	first, err := e.create("default-abc12", "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	// The same key and offering, and then another zone and type: the original.
	req, lr := e.request("default-abc12", "uid-1")
	req.Zone, req.InstanceType, req.CapacityType = testZoneB, "ecs.g7.xlarge", vendors.CapacitySpot
	lr.VSwitchID = e.vswitches[testZoneB]
	again, err := e.cloud.CreateInstance(context.Background(), req, lr)
	if err != nil || again.Ref != first.Ref || again.Zone != testZoneA {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if ids := e.f.InstanceIDs(testRegion); len(ids) != 1 {
		t.Fatalf("instances = %v", ids)
	}
}

// hidingECS answers the first n DescribeInstances calls (the lookups of the
// create path) with an empty list, as ECS does while an instance is not visible
// yet.
type hidingECS struct {
	ECSAPI
	hide int
}

func (h *hidingECS) DescribeInstances(ctx context.Context, in *ecs.DescribeInstancesRequest) (*ecs.DescribeInstancesResponseBody, error) {
	if h.hide > 0 {
		h.hide--
		return &ecs.DescribeInstancesResponseBody{Instances: &ecs.DescribeInstancesResponseBodyInstances{}}, nil
	}
	return h.ECSAPI.DescribeInstances(ctx, in)
}

func (e *env) cloudWith(ecsAPI func(ECSAPI) ECSAPI) *Cloud {
	c := NewCloud(testCluster, func(_ context.Context, region string) (Clients, error) {
		return Clients{ECS: ecsAPI(e.f.ECS(region)), VPC: e.f.VPC(region)}, nil
	}, e.clk)
	c.sleep = e.cloud.sleep
	return c
}

// ECS answers a reused ClientToken with other parameters with
// IdempotentParameterMismatch. When the lookup by tag finds the instance of the
// NodeClaim, that is the answer; this test makes the lookup miss once (as a
// not-yet-visible instance would) so that the mismatch of the replay, here with
// a re-rendered user data, resolves through the second lookup.
func TestIdempotentParameterMismatchResolvesToTheOriginal(t *testing.T) {
	e := newEnv(t)
	first, err := e.create("default-abc12", "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	c := e.cloudWith(func(a ECSAPI) ECSAPI { return &hidingECS{ECSAPI: a, hide: 1} })
	req, lr := e.request("default-abc12", "uid-1")
	req.UserData = []byte(`{"ignition":{"version":"3.4.0"},"changed":true}`)
	got, err := c.CreateInstance(context.Background(), req, lr)
	if err != nil || got.Ref != first.Ref {
		t.Fatalf("got %+v, %v; want the original %s", got, err, first.Ref)
	}
	if ids := e.f.InstanceIDs(testRegion); len(ids) != 1 {
		t.Fatalf("a duplicate was created: %v", ids)
	}
}

// When the mismatch finds no instance (the token was consumed by a failed call:
// unverified whether ECS does that), the launch is repeated once with a salted
// token, so the failed create leaves the key unused.
func TestSpentTokenIsRetriedWithASaltedToken(t *testing.T) {
	e := newEnv(t)
	e.f.ConsumeTokenOnFailure = true
	// The first launch fails for stock and spends its token.
	e.f.Offer(testRegion, testZoneA, fake.OnDemand, fake.StatusSoldOut, testType)
	if _, err := e.create("default-abc12", "uid-1"); !vendors.IsClass(err, vendors.InsufficientCapacity) {
		t.Fatalf("first create: %v", err)
	}
	// Stock is back, the replay changes the user data: the same token is a mismatch.
	e.f.Offer(testRegion, testZoneA, fake.OnDemand, fake.StatusAvailable, testType)
	req, lr := e.request("default-abc12", "uid-1")
	req.UserData = []byte(`{"ignition":{"version":"3.4.0"},"changed":true}`)
	got, err := e.cloud.CreateInstance(context.Background(), req, lr)
	if err != nil || got == nil {
		t.Fatalf("the salted retry must launch: %v", err)
	}
	if ids := e.f.InstanceIDs(testRegion); len(ids) != 1 {
		t.Fatalf("instances = %v", ids)
	}
}

// A failed create leaves the key unused: the next attempt with another
// offering launches (the contract's rule), also with the default fake.
func TestFailedCreateLeavesTheKeyUnused(t *testing.T) {
	e := newEnv(t)
	e.f.Offer(testRegion, testZoneA, fake.OnDemand, fake.StatusSoldOut, testType)
	if _, err := e.create("default-abc12", "uid-1"); !vendors.IsClass(err, vendors.InsufficientCapacity) {
		t.Fatalf("sold out: %v", err)
	}
	req, lr := e.request("default-abc12", "uid-1")
	req.Zone, lr.VSwitchID = testZoneB, e.vswitches[testZoneB]
	got, err := e.cloud.CreateInstance(context.Background(), req, lr)
	if err != nil || got.Zone != testZoneB {
		t.Fatalf("next offering: %+v %v", got, err)
	}
}

func TestLaunchErrorClasses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*env, *vendors.CreateRequest)
		class vendors.Class
		code  string
	}{
		{"sold out", func(e *env, _ *vendors.CreateRequest) {
			e.f.Offer(testRegion, testZoneA, fake.OnDemand, fake.StatusSoldOut, testType)
		}, vendors.InsufficientCapacity, "OperationDenied.NoStock"},
		{"not sold in the zone", func(e *env, r *vendors.CreateRequest) {
			e.f.AddType(fake.TypeSpec{Name: "ecs.odd.large", VCPUs: 2})
			r.InstanceType = "ecs.odd.large"
		}, vendors.Unsupported, "InvalidInstanceType.ZoneNotSupported"},
		{"unknown type", func(_ *env, r *vendors.CreateRequest) { r.InstanceType = "ecs.nope.large" },
			vendors.Unsupported, "InvalidInstanceType.ValueNotSupported"},
		{"vCPU quota", func(e *env, _ *vendors.CreateRequest) { e.f.SetQuota(testRegion, 1, 1) },
			vendors.QuotaExceeded, "QuotaExceed.ElasticQuota"},
		{"vSwitch full", func(e *env, _ *vendors.CreateRequest) { e.f.Fill(testRegion, e.vswitches[testZoneA], 4096) },
			vendors.ZoneUnusable, "QuotaExceeded.PrivateIpAddress"},
		{"group full", func(e *env, _ *vendors.CreateRequest) {
			e.f.SetGroupLimit(testRegion, e.group, 1)
			if _, err := e.create("first", "uid-first"); err != nil {
				t.Fatal(err)
			}
		}, vendors.ZoneUnusable, "SecurityGroupInstanceLimitExceed"},
		{"spot not supported", func(e *env, r *vendors.CreateRequest) {
			e.f.DisableSpot(testRegion)
			r.CapacityType = vendors.CapacitySpot
		}, vendors.Unsupported, "InvalidAccount.NotSupportSpot"},
		{"image gone", func(e *env, r *vendors.CreateRequest) { r.Image.Ref = "m-gone" }, vendors.Fatal, "InvalidImageId.NotFound"},
		{"region denied", func(e *env, _ *vendors.CreateRequest) { e.f.Deny(testRegion) }, vendors.Fatal, "RegionUnauthorized"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			req, lr := e.request("default-abc12", "uid-1")
			c.setup(e, &req)
			_, err := e.cloud.CreateInstance(context.Background(), req, lr)
			if vendors.ClassOf(err) != c.class || vendors.CodeOf(err) != c.code {
				t.Fatalf("got %s (%s): %v; want %s (%s)", vendors.ClassOf(err), vendors.CodeOf(err), err, c.class, c.code)
			}
		})
	}
}

func TestCreateRetriesThrottlingWithTheSameToken(t *testing.T) {
	e := newEnv(t)
	e.f.Inject("RunInstances", fake.APIError("Throttling.User", 400, "slow down"), 2)
	got, err := e.create("default-abc12", "uid-1")
	if err != nil || got == nil {
		t.Fatalf("create: %v", err)
	}
	if n := len(e.sleeps); n != 2 || e.sleeps[1] != 2*e.sleeps[0] {
		t.Fatalf("backoff = %v", e.sleeps)
	}
	if len(e.f.InstanceIDs(testRegion)) != 1 {
		t.Fatal("exactly one instance")
	}
}

func TestRetryIsBounded(t *testing.T) {
	e := newEnv(t)
	e.f.Inject("RunInstances", fake.APIError("ServiceUnavailable", 503, "down"), 0)
	_, err := e.create("default-abc12", "uid-1")
	if !vendors.IsClass(err, vendors.Retryable) || !isCode(err, "ServiceUnavailable") {
		t.Fatalf("err = %v", err)
	}
	if got := e.f.Calls("RunInstances"); got != maxAttempts {
		t.Fatalf("attempts = %d, want %d", got, maxAttempts)
	}
	// A code that is not transient is not retried.
	e.f.Clear("")
	e.f.Inject("RunInstances", fake.APIError("InvalidImageId.NotFound", 404, "no"), 0)
	before := e.f.Calls("RunInstances")
	if _, err := e.create("default-abc12", "uid-1"); vendors.ClassOf(err) != vendors.Fatal {
		t.Fatalf("err = %v", err)
	}
	if e.f.Calls("RunInstances")-before != 1 {
		t.Fatal("a fatal error must not be retried")
	}
}

func TestCallStopsOnCancellation(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.cloud.GetInstance(ctx, vendors.NewInstanceRef(testRegion, "i-1"))
	if !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("err = %v", err)
	}
	e.f.Inject("DescribeInstances", fake.APIError("Throttling", 400, "x"), 0)
	e.cloud.sleep = func(ctx context.Context, _ time.Duration) error { return errors.New("never reached") }
	ctx2, cancel2 := context.WithCancel(context.Background())
	e.cloud.sleep = func(ctx context.Context, _ time.Duration) error { cancel2(); return ctx.Err() }
	_, err = e.cloud.GetInstance(ctx2, vendors.NewInstanceRef(testRegion, "i-1"))
	if !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("err = %v", err)
	}
}

func TestVanishedInstanceIsInsufficientCapacity(t *testing.T) {
	e := newEnv(t)
	e.f.PendingFor = time.Minute
	e.f.VanishAfter(testRegion, testZoneA, fake.OnDemand, testType, 30*time.Second)
	inst, err := e.create("default-abc12", "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.cloud.CheckLaunched(ctx, inst.Ref, inst.CreatedAt, 5*time.Minute); err != nil {
		t.Fatalf("still there: %v", err)
	}
	e.clk.Step(time.Minute)
	err = e.cloud.CheckLaunched(ctx, inst.Ref, inst.CreatedAt, 5*time.Minute)
	if !vendors.IsClass(err, vendors.InsufficientCapacity) || vendors.CodeOf(err) != codeInstanceVanished {
		t.Fatalf("vanished within the launch timeout: %v", err)
	}
	e.clk.Step(time.Hour)
	if err := e.cloud.CheckLaunched(ctx, inst.Ref, inst.CreatedAt, 5*time.Minute); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("gone long after: %v", err)
	}
	if _, err := e.cloud.GetInstance(ctx, inst.Ref); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("get: %v", err)
	}
}

func sgReq(vpcID string) *ecs.CreateSecurityGroupRequest {
	return &ecs.CreateSecurityGroupRequest{VpcId: &vpcID, SecurityGroupName: new("nodes")}
}
