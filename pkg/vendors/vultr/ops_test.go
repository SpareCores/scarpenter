/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/vultr/govultr/v3"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

var _ vultr.API = (*fake.Account)(nil)

const (
	cluster = "my-cluster"
	plan    = "vc2-1c-1gb"
	snapDsc = "flatcar-stable-4081.2.0-amd64"
)

type env struct {
	acct *fake.Account
	ops  *vultr.Ops
	clk  *clocktesting.FakeClock
	snap string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	a := fake.New()
	for _, p := range []struct {
		id   string
		disk int
	}{{plan, 25}, {"vc2-2c-4gb", 80}, {"vx1-g-2c-8g", 1}, {"vx1-g-4c-16g", 100}, {"vcg-a16-6c-64g-16vram", 350}, {"vc2-1c-0.5gb-free", 10}, {"vhf-1c-1gb", 32}} {
		a.AddPlan(p.id, 1, 1024, p.disk)
	}
	a.AddRegion("ewr", plan, "vc2-2c-4gb", "vx1-g-2c-8g", "vx1-g-4c-16g", "vcg-a16-6c-64g-16vram", "vc2-1c-0.5gb-free", "vhf-1c-1gb")
	a.AddRegion("fra", plan)
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	a.SetClock(clk.Now)
	ops := vultr.NewOps(a, cluster)
	ops.Clock = clk
	return &env{acct: a, ops: ops, clk: clk, snap: a.AddSnapshot(snapDsc, fake.SnapshotComplete)}
}

func (e *env) request(claim, uid string) vendors.CreateRequest {
	return vendors.CreateRequest{
		Region: "ewr", InstanceType: plan, CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`), IdempotencyKey: uid, Node: vendors.NodeInfo{Name: claim},
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: cluster, vendors.TagNodeClaim: claim, vendors.TagNodePool: "pool",
			vendors.TagNodeClass: "default", vendors.TagManagedBy: vendors.ManagedByValue,
		},
	}
}

func (e *env) launchOpts() vultr.LaunchOptions { return vultr.LaunchOptions{SnapshotID: e.snap} }

func (e *env) launch(t *testing.T, claim, uid string) *vendors.Instance {
	t.Helper()
	inst, err := e.ops.Launch(context.Background(), e.request(claim, uid), e.launchOpts())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	return inst
}

func TestLaunchCreatesAPendingInstance(t *testing.T) {
	e := newEnv(t)
	inst := e.launch(t, "claim-1", "uid-1")
	if inst.State != vendors.StatePending || inst.Region != "ewr" || inst.Zone != "ewr" || inst.Type != plan || len(inst.Addresses) != 0 {
		t.Fatalf("got %+v", inst)
	}
	if inst.Tags[vendors.TagNodeClaim] != "claim-1" || inst.Tags[vendors.TagIdempotencyKey] != "uid-1" {
		t.Fatalf("tags %v", inst.Tags)
	}
	e.acct.Advance()
	got, err := e.ops.GetInstance(context.Background(), inst.Ref)
	if err != nil || got.State != vendors.StateRunning || len(got.Addresses) != 1 {
		t.Fatalf("after advance: %+v %v", got, err)
	}
}

func TestLaunchReplayReturnsTheOriginal(t *testing.T) {
	e := newEnv(t)
	first := e.launch(t, "claim-1", "uid-1")
	// Another process has no cache: the list lookup finds it. The replay asks
	// for another region and plan and still gets the original.
	other := vultr.NewOps(e.acct, cluster)
	req := e.request("claim-1", "uid-1")
	req.Region, req.InstanceType = "fra", "vc2-2c-4gb"
	got, err := other.Launch(context.Background(), req, e.launchOpts())
	if err != nil || got.Ref != first.Ref {
		t.Fatalf("replay: %+v %v", got, err)
	}
	if n := len(e.acct.Instances()); n != 1 {
		t.Fatalf("%d instances", n)
	}
	// The same process answers from its cache without a list call.
	lists := e.acct.Calls("ListInstances")
	if got, err := e.ops.Launch(context.Background(), e.request("claim-1", "uid-1"), e.launchOpts()); err != nil || got.Ref != first.Ref {
		t.Fatalf("cached replay: %+v %v", got, err)
	}
	if e.acct.Calls("ListInstances") != lists {
		t.Error("the cached replay listed instances")
	}
}

// The cache is confirmed before it answers: an instance that is gone (deleted by
// the garbage collector, an operator) or stopped is not handed back.
func TestLaunchReplayDoesNotReturnAGoneInstance(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	first := e.launch(t, "claim-1", "uid-1")
	if err := e.acct.DeleteInstance(ctx, first.Ref.ID()); err != nil {
		t.Fatal(err)
	}
	second, err := e.ops.Launch(ctx, e.request("claim-1", "uid-1"), e.launchOpts())
	if err != nil || second.Ref == first.Ref || len(e.acct.Instances()) != 1 {
		t.Fatalf("replay after a delete: %+v, %v, %d instances", second, err, len(e.acct.Instances()))
	}
	// A transient failure of the confirmation is the caller's to retry.
	e.acct.FailNext("GetInstance", 1, &fake.Error{Status: 503, Message: "down"})
	if _, err := e.ops.Launch(ctx, e.request("claim-1", "uid-1"), e.launchOpts()); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("got %v", err)
	}
	// A stopped one is returned as it is, terminating: the core treats it as
	// gone, and no second instance is created for the key.
	e.acct.SetInstanceStatus(second.Ref.ID(), "active", "stopped", "ok")
	if third, err := e.ops.Launch(ctx, e.request("claim-1", "uid-1"), e.launchOpts()); err != nil || third.Ref != second.Ref ||
		third.State != vendors.StateTerminating || len(e.acct.Instances()) != 1 {
		t.Fatalf("replay of a stopped instance: %+v, %v", third, err)
	}
}

func TestLaunchFailedCreateLeavesTheKeyUnused(t *testing.T) {
	e := newEnv(t)
	e.acct.FailNext("CreateInstance", 1, &fake.Error{Status: 500, Message: "boom"})
	if _, err := e.ops.Launch(context.Background(), e.request("claim-1", "uid-1"), e.launchOpts()); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("got %v", err)
	}
	if n := len(e.acct.Instances()); n != 0 {
		t.Fatalf("%d instances after a failed create", n)
	}
	e.launch(t, "claim-1", "uid-1")
	if n := len(e.acct.Instances()); n != 1 {
		t.Fatalf("%d instances", n)
	}
}

// A create the server accepted but whose response was lost: the retry in the
// same process uses the cache only when the first call returned, so here it
// is the list that finds the instance, once it shows (eventual consistency).
func TestLaunchLostResponse(t *testing.T) {
	e := newEnv(t)
	e.acct.TimeoutNextCreates(1)
	_, err := e.ops.Launch(context.Background(), e.request("claim-1", "uid-1"), e.launchOpts())
	if !vendors.IsClass(err, vendors.Retryable) || vendors.CodeOf(err) != "CallTimeout" {
		t.Fatalf("got %v", err)
	}
	got, err := e.ops.Launch(context.Background(), e.request("claim-1", "uid-1"), e.launchOpts())
	if err != nil || len(e.acct.Instances()) != 1 {
		t.Fatalf("retry: %+v %v, %d instances", got, err, len(e.acct.Instances()))
	}
}

// With a lagging list the retry would create a second instance: this is the
// documented window, closed only by the orphan sweep (and by the cache when the
// first response arrived).
func TestLaunchLostResponseWithListLagIsTheDocumentedWindow(t *testing.T) {
	e := newEnv(t)
	e.acct.ListLag = true
	e.acct.TimeoutNextCreates(1)
	if _, err := e.ops.Launch(context.Background(), e.request("claim-1", "uid-1"), e.launchOpts()); err == nil {
		t.Fatal("no error")
	}
	e.launch(t, "claim-1", "uid-1")
	if n := len(e.acct.Instances()); n != 2 {
		t.Fatalf("%d instances: the window moved", n)
	}
	// With the response received, the cache closes the window.
	e.launch(t, "claim-2", "uid-2")
	e.launch(t, "claim-2", "uid-2")
	if n := len(e.acct.Instances()); n != 3 {
		t.Fatalf("%d instances", n)
	}
}

func TestLaunchCacheExpires(t *testing.T) {
	e := newEnv(t)
	e.acct.ListLag = true
	first := e.launch(t, "claim-1", "uid-1")
	e.clk.Step(vultr.RecentCreateTTL + time.Second)
	e.acct.Settle()
	got := e.launch(t, "claim-1", "uid-1")
	if got.Ref != first.Ref || len(e.acct.Instances()) != 1 {
		t.Fatalf("got %v, %d instances", got.Ref, len(e.acct.Instances()))
	}
}

func TestLaunchStockCheckComesBeforeTheCall(t *testing.T) {
	e := newEnv(t)
	e.acct.SetAvailable("ewr", "vc2-2c-4gb")
	_, err := e.ops.Launch(context.Background(), e.request("claim-1", "uid-1"), e.launchOpts())
	if !vendors.IsClass(err, vendors.InsufficientCapacity) || vendors.IsAccountWide(err) {
		t.Fatalf("got %v", err)
	}
	if e.acct.Calls("CreateInstance") != 0 {
		t.Fatal("create was called for a plan out of stock")
	}
	// The stock is cached for the TTL and read again after it.
	e.acct.SetAvailable("ewr", plan)
	if _, err := e.ops.Launch(context.Background(), e.request("claim-1", "uid-1"), e.launchOpts()); !vendors.IsClass(err, vendors.InsufficientCapacity) {
		t.Fatalf("cached stock: %v", err)
	}
	e.clk.Step(vultr.DefaultAvailabilityTTL + time.Second)
	e.launch(t, "claim-1", "uid-1")
}

func TestLaunchUnknownRegion(t *testing.T) {
	e := newEnv(t)
	req := e.request("claim-1", "uid-1")
	req.Region = "mars"
	if _, err := e.ops.Launch(context.Background(), req, e.launchOpts()); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestLaunchAccountLimitIsAccountWideQuota(t *testing.T) {
	e := newEnv(t)
	e.acct.InstanceLimit = 1
	e.launch(t, "claim-1", "uid-1")
	_, err := e.ops.Launch(context.Background(), e.request("claim-2", "uid-2"), e.launchOpts())
	if !vendors.IsClass(err, vendors.QuotaExceeded) || !vendors.IsAccountWide(err) || vendors.CodeOf(err) != "AccountLimit" {
		t.Fatalf("got %v", err)
	}
}

func TestLaunchErrorClasses(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*env, *vendors.CreateRequest, *vultr.LaunchOptions)
		class vendors.Class
	}{
		{"unknown plan", func(e *env, r *vendors.CreateRequest, _ *vultr.LaunchOptions) {
			e.acct.SetAvailable("ewr", "nope-1c")
			r.InstanceType = "nope-1c"
		}, vendors.Unsupported},
		{"bad snapshot", func(_ *env, _ *vendors.CreateRequest, o *vultr.LaunchOptions) { o.SnapshotID = "missing" }, vendors.Fatal},
		{"pending snapshot", func(e *env, _ *vendors.CreateRequest, o *vultr.LaunchOptions) {
			o.SnapshotID = e.acct.AddSnapshot("other", fake.SnapshotPendingState)
		}, vendors.Fatal},
		{"unknown firewall group", func(_ *env, _ *vendors.CreateRequest, o *vultr.LaunchOptions) { o.FirewallGroupID = "missing" }, vendors.Fatal},
		{"unknown VPC", func(_ *env, _ *vendors.CreateRequest, o *vultr.LaunchOptions) { o.VPCIDs = []string{"missing"} }, vendors.Fatal},
		{"rate limited", func(e *env, _ *vendors.CreateRequest, _ *vultr.LaunchOptions) {
			e.acct.FailNext("CreateInstance", 1, &fake.Error{Status: 429, Message: fake.MsgRateLimited})
		}, vendors.Retryable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			req, lo := e.request("claim-1", "uid-1"), e.launchOpts()
			c.edit(e, &req, &lo)
			_, err := e.ops.Launch(context.Background(), req, lo)
			if vendors.ClassOf(err) != c.class {
				t.Fatalf("got %v", err)
			}
			if n := len(e.acct.Instances()); n != 0 {
				t.Fatalf("%d instances", n)
			}
		})
	}
}

func TestLaunchAttachesFirewallGroupAndVPC(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	g, err := e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "g"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.acct.CreateVPC(ctx, &govultr.VPCReq{Region: "ewr", Description: "v"})
	if err != nil {
		t.Fatal(err)
	}
	lo := e.launchOpts()
	lo.FirewallGroupID, lo.VPCIDs = g.ID, []string{v.ID}
	inst, err := e.ops.Launch(ctx, e.request("claim-1", "uid-1"), lo)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.acct.GetFirewallGroup(ctx, g.ID); !vultr.FirewallGroupInUse(*got) {
		t.Error("the group is not in use")
	}
	if att, _, _ := e.acct.ListVPCAttachments(ctx, v.ID, nil); len(att) != 1 {
		t.Errorf("attachments %v", att)
	}
	if err := e.ops.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.acct.GetFirewallGroup(ctx, g.ID); vultr.FirewallGroupInUse(*got) {
		t.Error("the group is still in use")
	}
}

func TestGetInstance(t *testing.T) {
	e := newEnv(t)
	refs := []vendors.InstanceRef{"ewr/1a000000-0000-4000-8000-0000000000ff", "ewr/missing", "ewr", "ewr/", ""}
	for _, ref := range refs {
		if _, err := e.ops.GetInstance(context.Background(), ref); !vendors.IsClass(err, vendors.NotFound) {
			t.Errorf("%q: %v", ref, err)
		}
	}
	// Only the well-formed reference reaches the API.
	if n := e.acct.Calls("GetInstance"); n != 1 {
		t.Errorf("%d get calls", n)
	}
}

func TestStoppedInstanceIsTerminating(t *testing.T) {
	e := newEnv(t)
	inst := e.launch(t, "claim-1", "uid-1")
	e.acct.Advance()
	e.acct.SetInstanceStatus(inst.Ref.ID(), "active", "stopped", "ok")
	got, err := e.ops.GetInstance(context.Background(), inst.Ref)
	if err != nil || got.State != vendors.StateTerminating || !got.Gone() {
		t.Fatalf("got %+v %v", got, err)
	}
	list, err := e.ops.ListInstances(context.Background(), nil, map[vendors.TagKey]string{vendors.TagCluster: cluster})
	if err != nil || len(list) != 1 || list[0].State != vendors.StateTerminating {
		t.Fatalf("a terminating instance must be listed: %+v %v", list, err)
	}
}

func TestDeleteInstance(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	inst := e.launch(t, "claim-1", "uid-1")
	if err := e.ops.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if n := len(e.acct.Instances()); n != 0 {
		t.Fatalf("%d instances", n)
	}
	// Idempotent: gone, or a reference that does not parse, is success.
	for _, ref := range []vendors.InstanceRef{inst.Ref, "ewr/never-was", "garbage", ""} {
		if err := e.ops.DeleteInstance(ctx, ref); err != nil {
			t.Errorf("delete %q: %v", ref, err)
		}
	}
	if e.acct.Calls("DeleteInstance") != 1 {
		t.Errorf("%d delete calls for one instance", e.acct.Calls("DeleteInstance"))
	}
}

func TestDeleteRefusesInstancesOfOthers(t *testing.T) {
	e := newEnv(t)
	foreign := e.acct.AddForeignInstance("ewr", plan)
	other := e.acct.AddForeignInstance("ewr", plan, "scarpenter.sparecores.com/cluster=another-cluster")
	for _, id := range []string{foreign, other} {
		err := e.ops.DeleteInstance(context.Background(), vendors.NewInstanceRef("ewr", id))
		if !vendors.IsClass(err, vendors.Fatal) || vendors.CodeOf(err) != "NotOurInstance" {
			t.Errorf("%s: %v", id, err)
		}
	}
	if n := len(e.acct.Instances()); n != 2 || e.acct.Calls("DeleteInstance") != 0 {
		t.Fatalf("%d instances, %d deletes", n, e.acct.Calls("DeleteInstance"))
	}
}

func TestDeleteWhileInstallingIsRetryable(t *testing.T) {
	e := newEnv(t)
	e.acct.RefuseDeleteWhilePending = true
	inst := e.launch(t, "claim-1", "uid-1")
	if err := e.ops.DeleteInstance(context.Background(), inst.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("got %v", err)
	}
	e.acct.Advance()
	if err := e.ops.DeleteInstance(context.Background(), inst.Ref); err != nil {
		t.Fatal(err)
	}
}

func TestListAndFindIgnoreOtherOwners(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.acct.AddForeignInstance("ewr", plan)
	e.acct.AddForeignInstance("ewr", plan, "scarpenter.sparecores.com/cluster=another-cluster")
	a := e.launch(t, "claim-1", "uid-1")
	req := e.request("claim-2", "uid-2")
	req.Region = "fra"
	b, err := e.ops.Launch(ctx, req, e.launchOpts())
	if err != nil {
		t.Fatal(err)
	}
	mine := map[vendors.TagKey]string{vendors.TagCluster: cluster}
	list, err := e.ops.ListInstances(ctx, nil, mine)
	if err != nil || len(list) != 2 {
		t.Fatalf("all regions: %v %v", list, err)
	}
	// One region is one filtered call; two regions are filtered client-side.
	if list, _ = e.ops.ListInstances(ctx, []string{"fra"}, mine); len(list) != 1 || list[0].Ref != b.Ref {
		t.Fatalf("fra: %v", list)
	}
	if list, _ = e.ops.ListInstances(ctx, []string{"ewr", "fra"}, mine); len(list) != 2 {
		t.Fatalf("both: %v", list)
	}
	if list, _ = e.ops.ListInstances(ctx, []string{"sgp"}, mine); len(list) != 0 {
		t.Fatalf("none: %v", list)
	}
	// Tags beyond the server-side one are compared client-side.
	both := map[vendors.TagKey]string{vendors.TagCluster: cluster, vendors.TagNodeClaim: "claim-1"}
	if list, _ = e.ops.ListInstances(ctx, nil, both); len(list) != 1 || list[0].Ref != a.Ref {
		t.Fatalf("two tags: %v", list)
	}
	found, err := e.ops.FindInstance(ctx, "", map[vendors.TagKey]string{vendors.TagNodeClaim: "claim-2"})
	if err != nil || found.Ref != b.Ref {
		t.Fatalf("find: %v %v", found, err)
	}
	if _, err := e.ops.FindInstance(ctx, "ewr", map[vendors.TagKey]string{vendors.TagNodeClaim: "claim-2"}); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("find in the wrong region: %v", err)
	}
	// One list call per sweep, not one per region.
	before := e.acct.Calls("ListInstances")
	if _, err := e.ops.ListInstances(ctx, []string{"ewr", "fra", "sgp"}, mine); err != nil {
		t.Fatal(err)
	}
	if got := e.acct.Calls("ListInstances") - before; got != 1 {
		t.Errorf("%d list calls", got)
	}
}

func TestListPaginates(t *testing.T) {
	e := newEnv(t)
	// 1200 instances span three pages of 500.
	for i := range 1200 {
		e.acct.AddForeignInstance("ewr", plan, fmt.Sprintf("scarpenter.sparecores.com/cluster=%s", cluster), fmt.Sprintf("n=%d", i))
	}
	list, err := e.ops.ListInstances(context.Background(), nil, map[vendors.TagKey]string{vendors.TagCluster: cluster})
	if err != nil || len(list) != 1200 {
		t.Fatalf("%d instances, %v", len(list), err)
	}
	if got := e.acct.Calls("ListInstances"); got != 3 {
		t.Errorf("%d pages", got)
	}
}

func TestListErrorsAreClassified(t *testing.T) {
	e := newEnv(t)
	e.acct.FailNext("ListInstances", 1, &fake.Error{Status: 429, Message: fake.MsgRateLimited})
	if _, err := e.ops.ListInstances(context.Background(), nil, nil); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("got %v", err)
	}
}

func TestSnapshotDescription(t *testing.T) {
	if got := vultr.SnapshotDescription("stable", "4081.2.0", "amd64"); got != snapDsc {
		t.Fatalf("got %q", got)
	}
}

func TestLaunchableTypes(t *testing.T) {
	e := newEnv(t)
	got, err := e.ops.LaunchableTypes(context.Background(), "ewr")
	// vx1-g-2c-8g is diskless, vcg is GPU and the free plan is dropped.
	want := []string{"vc2-1c-1gb", "vc2-2c-4gb", "vhf-1c-1gb", "vx1-g-4c-16g"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := e.ops.LaunchableTypes(context.Background(), "mars"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Fatalf("unknown region: %v", err)
	}
	// The plan list is cached; the stock is cached for the TTL.
	if _, err := e.ops.LaunchableTypes(context.Background(), "fra"); err != nil {
		t.Fatal(err)
	}
	if e.acct.Calls("ListPlans") != 1 || e.acct.Calls("RegionAvailability") != 3 {
		t.Errorf("plans %d, availability %d", e.acct.Calls("ListPlans"), e.acct.Calls("RegionAvailability"))
	}
	e.acct.SetAvailable("fra", plan, "vc2-2c-4gb")
	if got, _ := e.ops.LaunchableTypes(context.Background(), "fra"); !slices.Equal(got, []string{plan}) {
		t.Fatalf("stock changed inside the TTL: %v", got)
	}
	e.clk.Step(vultr.DefaultAvailabilityTTL + time.Second)
	if got, _ := e.ops.LaunchableTypes(context.Background(), "fra"); !slices.Equal(got, []string{plan, "vc2-2c-4gb"}) {
		t.Fatalf("after the TTL: %v", got)
	}
}

func TestRegions(t *testing.T) {
	e := newEnv(t)
	got, err := e.ops.Regions(context.Background(), nil)
	if err != nil || len(got) != 2 || got[0].Name != "ewr" || got[1].Name != "fra" {
		t.Fatalf("got %v %v", got, err)
	}
	e.acct.FailNext("ListRegions", 1, errors.New("gave up after 4 attempts, last error: x"))
	if _, err := e.ops.Regions(context.Background(), nil); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("got %v", err)
	}
}
