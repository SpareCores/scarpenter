/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"testing"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud/sweep"
)

const region = "ap-southeast-1"

type world struct {
	acct *fake.Cloud
	clk  *clocktesting.FakeClock
	f    alicloud.ClientFactory
}

func newWorld(t *testing.T) *world {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	a := fake.New(clk)
	a.AddRegion(region, region+"a")
	a.AddRegion("eu-central-1", "eu-central-1a")
	a.AddType(fake.TypeSpec{Name: "ecs.g7.large", VCPUs: 2, MemoryGiB: 8})
	a.OfferEverywhere(region)
	a.AddImage(region, fake.ImageSpec{ID: "m-1", Name: "img"})
	return &world{acct: a, clk: clk, f: func(_ context.Context, r string) (alicloud.Clients, error) {
		return alicloud.Clients{ECS: a.ECS(r), VPC: a.VPC(r)}, nil
	}}
}

func (w *world) opts(o sweep.Options) sweep.Options {
	o.Now = w.clk.Now
	o.PollInterval = time.Millisecond
	o.Regions = []string{region}
	return o
}

// stack creates what a lane run leaves: a VPC with a vSwitch, a group, a deployment set and
// an instance of the cluster, at the clock's now.
func (w *world) stack(t *testing.T, cluster string) (inst string) {
	t.Helper()
	ctx := context.Background()
	tag := func(k, v string) *vpc.CreateVpcRequestTag { return &vpc.CreateVpcRequestTag{Key: &k, Value: &v} }
	v, err := w.acct.VPC(region).CreateVpc(ctx, &vpc.CreateVpcRequest{CidrBlock: new("10.240.0.0/16"), VpcName: new("n"),
		Tag: []*vpc.CreateVpcRequestTag{tag(sweep.ClusterTag, cluster)}})
	if err != nil {
		t.Fatal(err)
	}
	sw, err := w.acct.VPC(region).CreateVSwitch(ctx, &vpc.CreateVSwitchRequest{VpcId: v.VpcId, ZoneId: new(region + "a"),
		CidrBlock: new("10.240.0.0/20"), Tag: []*vpc.CreateVSwitchRequestTag{{Key: new(sweep.ClusterTag), Value: &cluster}}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := w.acct.ECS(region).CreateSecurityGroup(ctx, &ecs.CreateSecurityGroupRequest{VpcId: v.VpcId, SecurityGroupName: new("g"),
		Tag: []*ecs.CreateSecurityGroupRequestTag{{Key: new(sweep.ClusterTag), Value: &cluster}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.acct.ECS(region).CreateDeploymentSet(ctx, &ecs.CreateDeploymentSetRequest{DeploymentSetName: new("ds-" + cluster),
		Description: new("scarpenter.sparecores.com: cluster=" + cluster + " managed-by=scarpenter")}); err != nil {
		t.Fatal(err)
	}
	out, err := w.acct.ECS(region).RunInstances(ctx, &ecs.RunInstancesRequest{
		Amount: new(int32(1)), MinAmount: new(int32(1)), InstanceChargeType: new("PostPaid"), ImageId: new("m-1"),
		InstanceType: new("ecs.g7.large"), VSwitchId: sw.VSwitchId, SecurityGroupIds: []*string{g.SecurityGroupId},
		Tag: []*ecs.RunInstancesRequestTag{{Key: new(sweep.ClusterTag), Value: &cluster}}, ZoneId: new(region + "a"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return *out.InstanceIdSets.InstanceIdSet[0]
}

func TestSweepDeletesAClusterInDependencyOrder(t *testing.T) {
	w := newWorld(t)
	w.stack(t, "scarpenter-ci-1")
	w.stack(t, "other-cluster")
	rep, err := sweep.Run(context.Background(), w.f, region, w.opts(sweep.Options{Cluster: "scarpenter-ci-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(sweep.Failed) != 0 || rep.Count(sweep.Deleted) != 5 {
		t.Fatalf("%+v", rep.Items)
	}
	if left := w.acct.InstanceIDs(region); len(left) != 1 {
		t.Errorf("instances left: %v (the other cluster's must stay)", left)
	}
	// The other cluster is untouched and not reported.
	for _, it := range rep.Items {
		if it.Cluster != "scarpenter-ci-1" {
			t.Errorf("%+v", it)
		}
	}
}

func TestSweepByPrefixAndAge(t *testing.T) {
	w := newWorld(t)
	w.stack(t, "scarpenter-ci-old")
	w.clk.Step(3 * time.Hour)
	w.stack(t, "scarpenter-ci-new")
	rep, err := sweep.Run(context.Background(), w.f, region, w.opts(sweep.Options{MaxAge: 2 * time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(sweep.Deleted) != 5 || rep.Count(sweep.Skipped) == 0 {
		t.Fatalf("%+v", rep.Items)
	}
	// A dry run deletes nothing.
	rep, _ = sweep.Run(context.Background(), w.f, region, w.opts(sweep.Options{MaxAge: 0, DryRun: true}))
	if rep.Count(sweep.WouldDelete) != 5 || rep.Count(sweep.Deleted) != 0 || len(w.acct.InstanceIDs(region)) != 1 {
		t.Fatalf("%+v", rep.Items)
	}
}

func TestSweepNeverTouchesDefaultsOrUnmarked(t *testing.T) {
	w := newWorld(t)
	vpcID, _ := w.acct.AddDefaultNetwork(region)
	// A default VPC carrying the cluster tag is still never deleted.
	w.acct.AddForeignInstance(region, region+"a", "ecs.g7.large", false)
	rep, err := sweep.Run(context.Background(), w.f, region, w.opts(sweep.Options{}))
	if err != nil || rep.Count(sweep.Deleted) != 0 || w.acct.DefaultVPC(region) != vpcID || len(w.acct.InstanceIDs(region)) != 1 {
		t.Fatalf("%+v %v", rep.Items, err)
	}
}

func TestSweepAllRegionsAndErrors(t *testing.T) {
	w := newWorld(t)
	w.stack(t, "scarpenter-ci-1")
	o := w.opts(sweep.Options{})
	o.Regions = nil
	rep, err := sweep.Run(context.Background(), w.f, region, o)
	if err != nil || rep.Count(sweep.Deleted) != 5 {
		t.Fatalf("%+v %v", rep.Items, err)
	}
	// A region the role cannot use is skipped.
	w.acct.Deny("eu-central-1")
	rep, err = sweep.Run(context.Background(), w.f, region, o)
	if err != nil || rep.Count(sweep.Skipped) != 1 || rep.Count(sweep.Failed) != 0 {
		t.Fatalf("%+v %v", rep.Items, err)
	}
	// A failing region list makes the sweep meaningless.
	w.acct.Inject("DescribeRegions", fake.APIError("Forbidden.RAM", 403, "x"), 0)
	if _, err := sweep.Run(context.Background(), w.f, region, o); err == nil {
		t.Error("no region list")
	}
	w.acct.Clear("")
	failing := func(context.Context, string) (alicloud.Clients, error) {
		return alicloud.Clients{}, context.DeadlineExceeded
	}
	if _, err := sweep.Run(context.Background(), failing, region, o); err == nil {
		t.Error("no clients")
	}
	rep, _ = sweep.Run(context.Background(), failing, region, w.opts(sweep.Options{}))
	if rep.Count(sweep.Failed) != 1 {
		t.Errorf("%+v", rep.Items)
	}
}

func TestSweepReportsWhatItCannotDelete(t *testing.T) {
	w := newWorld(t)
	w.stack(t, "scarpenter-ci-1")
	w.acct.Inject("DeleteSecurityGroup", fake.APIError("DependencyViolation.Instance", 400, "x"), 0)
	w.acct.Inject("DeleteVSwitch", fake.APIError("DependencyViolation.Instance", 400, "x"), 0)
	rep, _ := sweep.Run(context.Background(), w.f, region, w.opts(sweep.Options{}))
	if rep.Count(sweep.Failed) < 2 {
		t.Errorf("%+v", rep.Items)
	}
}

func TestParseRegions(t *testing.T) {
	if got, err := sweep.ParseRegions(" a, b,a,"); err != nil || len(got) != 2 {
		t.Error(got, err)
	}
	if got, err := sweep.ParseRegions("all"); err != nil || got != nil {
		t.Error(got, err)
	}
	if _, err := sweep.ParseRegions("all,a"); err == nil {
		t.Error("all with names")
	}
}

var _ = vendors.TagCluster
