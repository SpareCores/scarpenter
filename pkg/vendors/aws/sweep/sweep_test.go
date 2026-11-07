/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package sweep_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	clocktesting "k8s.io/utils/clock/testing"

	awsdriver "github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/sweep"
)

var ctx = context.Background()

const (
	ciOld    = "scarpenter-ci-100"
	ciYoung  = "scarpenter-ci-200"
	ciNoTag  = "scarpenter-ci-300"
	prod     = "prod-cluster"
	noVPC    = "eu-north-1"
	startUTC = 1_800_000_000
)

type env struct {
	acct *fake.Account
	clk  *clocktesting.FakeClock
	t    *testing.T
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Unix(startUTC, 0))
	acct := fake.New(fake.WithClock(clk), fake.WithRegions(
		fake.DefaultRegion("us-east-2", "use2"),
		fake.DefaultRegion("eu-central-1", "euc1"),
		func() fake.RegionSpec { r := fake.DefaultRegion(noVPC, "eun1"); r.NoDefaultVPC = true; return r }(),
	))
	acct.SetInstanceTypes([]fake.InstanceType{{Name: "t3a.small", VCPUs: 2}})
	for _, r := range []string{"us-east-2", "eu-central-1", noVPC} {
		acct.SetQuota(r, "L-1216C47A", 1000)
	}
	return &env{acct: acct, clk: clk, t: t}
}

func (e *env) factory() awsdriver.ClientFactory {
	return func(_ context.Context, region string) (awsdriver.Clients, error) {
		return awsdriver.Clients{EC2: e.acct.EC2(region), Quotas: e.acct.Quotas(region)}, nil
	}
}

func (e *env) sweep(o sweep.Options) *sweep.Report {
	e.t.Helper()
	o.Now = e.clk.Now
	rep, err := sweep.Run(ctx, e.factory(), "us-east-2", o)
	if err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func tags(kv ...string) []types.Tag {
	var out []types.Tag
	for i := 0; i < len(kv); i += 2 {
		out = append(out, types.Tag{Key: &kv[i], Value: &kv[i+1]})
	}
	return out
}

func spec(rt types.ResourceType, kv ...string) []types.TagSpecification {
	return []types.TagSpecification{{ResourceType: rt, Tags: tags(kv...)}}
}

// launch starts an instance of the cluster in us-east-2 and returns its ID.
func (e *env) launch(cluster string) string {
	e.t.Helper()
	out, err := e.acct.EC2("us-east-2").RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: new("ami-1"), InstanceType: "t3a.small", MinCount: new(int32(1)), MaxCount: new(int32(1)),
		Placement:         &types.Placement{AvailabilityZone: new("us-east-2a")},
		TagSpecifications: spec(types.ResourceTypeInstance, sweep.ClusterTag, cluster, "Name", "n"),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return *out.Instances[0].InstanceId
}

// groupIDs lists every security group of the region (the fake pages by three).
func (e *env) groupIDs(region string) []string {
	e.t.Helper()
	var ids []string
	var token *string
	for {
		out, err := e.acct.EC2(region).DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{NextToken: token})
		if err != nil {
			e.t.Fatal(err)
		}
		for _, g := range out.SecurityGroups {
			ids = append(ids, *g.GroupId)
		}
		if token = out.NextToken; token == nil {
			return ids
		}
	}
}

func (e *env) states() map[string]types.InstanceStateName { return e.acct.InstanceStates("us-east-2") }

func created(sec int64) string { return strconv.FormatInt(sec, 10) }

func outcome(rep *sweep.Report, kind, id string) sweep.Outcome {
	for _, i := range rep.Items {
		if i.Kind == kind && i.ID == id {
			return i.Outcome
		}
	}
	return ""
}

func TestInstancesAreSelectedByClusterAndAge(t *testing.T) {
	e := newEnv(t)
	old := e.launch(ciOld)
	foreign := e.launch(prod)
	bare := func() string { // no cluster tag at all
		out, err := e.acct.EC2("us-east-2").RunInstances(ctx, &ec2.RunInstancesInput{
			ImageId: new("ami-1"), InstanceType: "t3a.small", MinCount: new(int32(1)), MaxCount: new(int32(1)),
			Placement: &types.Placement{AvailabilityZone: new("us-east-2a")}})
		if err != nil {
			t.Fatal(err)
		}
		return *out.Instances[0].InstanceId
	}()
	e.clk.Step(3 * time.Hour)
	young := e.launch(ciYoung)
	e.clk.Step(time.Hour)
	// A cluster of another prefix with a similar name must not match.
	lookalike := e.launch("scarpenter-ci")

	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour})
	if got := outcome(rep, "instance", old); got != sweep.Deleted {
		t.Errorf("the old instance: %q", got)
	}
	if got := outcome(rep, "instance", young); got != sweep.Skipped {
		t.Errorf("the young instance (an hour old): %q", got)
	}
	st := e.states()
	for id, want := range map[string]types.InstanceStateName{
		old: types.InstanceStateNameTerminated, young: types.InstanceStateNameRunning,
		foreign: types.InstanceStateNameRunning, bare: types.InstanceStateNameRunning, lookalike: types.InstanceStateNameRunning,
	} {
		if st[id] != want {
			t.Errorf("instance %s is %s, want %s", id, st[id], want)
		}
	}
	for _, id := range []string{foreign, bare, lookalike} {
		if outcome(rep, "instance", id) != "" {
			t.Errorf("the instance %s is none of the sweep's business but was reported", id)
		}
	}
}

func TestExactClusterIgnoresAgeAndSiblings(t *testing.T) {
	e := newEnv(t)
	mine := e.launch(ciYoung)
	sibling := e.launch(ciOld)
	rep := e.sweep(sweep.Options{Cluster: ciYoung})
	if outcome(rep, "instance", mine) != sweep.Deleted {
		t.Errorf("the cluster's own instance was not deleted: %+v", rep.Items)
	}
	if st := e.states(); st[sibling] != types.InstanceStateNameRunning {
		t.Errorf("a sibling cluster's instance is %s", st[sibling])
	}
}

func TestDryRunDeletesNothing(t *testing.T) {
	e := newEnv(t)
	id := e.launch(ciOld)
	e.clk.Step(3 * time.Hour)
	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour, DryRun: true})
	if rep.Count(sweep.WouldDelete) != 1 || rep.Count(sweep.Deleted) != 0 {
		t.Errorf("dry run: %+v", rep.Items)
	}
	if e.states()[id] != types.InstanceStateNameRunning {
		t.Errorf("a dry run terminated the instance")
	}
	if e.acct.Calls("TerminateInstances") != 0 {
		t.Errorf("a dry run called TerminateInstances")
	}
}

func TestGroupsAreSweptByTheirCreationTag(t *testing.T) {
	e := newEnv(t)
	c := e.acct.EC2("us-east-2")
	vpcs, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		t.Fatal(err)
	}
	vpc := *vpcs.Vpcs[0].VpcId
	oldAt := created(startUTC)
	youngAt := created(startUTC + 3*3600)
	mk := func(name string, kv ...string) string {
		out, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: &name, Description: &name, VpcId: &vpc,
			TagSpecifications: spec(types.ResourceTypeSecurityGroup, kv...)})
		if err != nil {
			t.Fatal(err)
		}
		return *out.GroupId
	}
	oldSG := mk("old", sweep.ClusterTag, ciOld, sweep.CreatedAtTag, oldAt)
	youngSG := mk("young", sweep.ClusterTag, ciOld, sweep.CreatedAtTag, youngAt)
	noAge := mk("noage", sweep.ClusterTag, ciNoTag)
	foreign := mk("foreign", sweep.ClusterTag, prod, sweep.CreatedAtTag, oldAt)
	if _, err := c.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("pg-old"), Strategy: types.PlacementStrategyCluster,
		TagSpecifications: spec(types.ResourceTypePlacementGroup, sweep.ClusterTag, ciOld, sweep.CreatedAtTag, oldAt)}); err != nil {
		t.Fatal(err)
	}
	e.clk.Step(4 * time.Hour)

	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour})
	if got := outcome(rep, "security-group", oldSG); got != sweep.Deleted {
		t.Errorf("old group: %q", got)
	}
	// Four hours passed since the start; the young group was created three hours
	// after it, so it is one hour old and stays.
	if got := outcome(rep, "security-group", youngSG); got != sweep.Skipped {
		t.Errorf("young group: %q", got)
	}
	if got := outcome(rep, "placement-group", "pg-old"); got != sweep.Deleted {
		t.Errorf("old placement group: %q", got)
	}
	if got := outcome(rep, "security-group", noAge); got != sweep.Skipped {
		t.Errorf("a group without a creation tag: %q (its age is unknown, it must be left alone)", got)
	}
	if got := outcome(rep, "security-group", foreign); got != "" {
		t.Errorf("another cluster's group was reported: %q", got)
	}
	ids := e.groupIDs("us-east-2")
	for _, id := range []string{youngSG, noAge, foreign} {
		if !slices.Contains(ids, id) {
			t.Errorf("the group %s was deleted", id)
		}
	}
	if slices.Contains(ids, oldSG) {
		t.Errorf("the old group is still there")
	}

	// Without an age limit (the e2e teardown of one cluster) everything of the
	// cluster goes, also what has no known age.
	rep = e.sweep(sweep.Options{Cluster: ciNoTag})
	if got := outcome(rep, "security-group", noAge); got != sweep.Deleted {
		t.Errorf("exact cluster, no age limit: %q", got)
	}
}

func TestInstancesGoBeforeTheGroupTheyUse(t *testing.T) {
	e := newEnv(t)
	c := e.acct.EC2("us-east-2")
	vpcs, _ := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	vpc := *vpcs.Vpcs[0].VpcId
	sg, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: new("g"), Description: new("g"), VpcId: &vpc,
		TagSpecifications: spec(types.ResourceTypeSecurityGroup, sweep.ClusterTag, ciOld, sweep.CreatedAtTag, created(startUTC))})
	if err != nil {
		t.Fatal(err)
	}
	subnets := e.acct.DefaultSubnets("us-east-2")
	if _, err := c.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: new("ami-1"), InstanceType: "t3a.small", MinCount: new(int32(1)), MaxCount: new(int32(1)),
		Placement:         &types.Placement{AvailabilityZone: new("us-east-2a")},
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{SubnetId: new(subnets["us-east-2a"]), Groups: []string{*sg.GroupId}}},
		TagSpecifications: spec(types.ResourceTypeInstance, sweep.ClusterTag, ciOld),
	}); err != nil {
		t.Fatal(err)
	}
	e.clk.Step(3 * time.Hour)
	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour})
	if got := outcome(rep, "security-group", *sg.GroupId); got != sweep.Deleted {
		t.Errorf("the group of a terminated instance: %q, items %+v", got, rep.Items)
	}
}

// managedNetwork creates the layout of SCarpenter's managed network (overview
// section 11.2 and docs/reference/aws-driver.md): a VPC 10.240.0.0/16 named
// scarpenter-<cluster>-subnets-<region>, a subnet per zone, an internet gateway
// and a route table associated with the subnets, all tagged.
func (e *env) managedNetwork(region, cluster string, at int64) (vpc string) {
	e.t.Helper()
	c := e.acct.EC2(region)
	tg := func(rt types.ResourceType, name string) []types.TagSpecification {
		return spec(rt, sweep.ClusterTag, cluster, "scarpenter.sparecores.com/managed-by", "scarpenter",
			"scarpenter.sparecores.com/resource-kind", "subnets", sweep.CreatedAtTag, created(at), "Name", name)
	}
	base := fmt.Sprintf("scarpenter-%s-subnets-%s", cluster, region)
	v, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.240.0.0/16"), TagSpecifications: tg(types.ResourceTypeVpc, base)})
	if err != nil {
		e.t.Fatal(err)
	}
	vpc = *v.Vpc.VpcId
	g, err := c.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{TagSpecifications: tg(types.ResourceTypeInternetGateway, base+"-igw")})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{InternetGatewayId: g.InternetGateway.InternetGatewayId, VpcId: &vpc}); err != nil {
		e.t.Fatal(err)
	}
	rt, err := c.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: &vpc, TagSpecifications: tg(types.ResourceTypeRouteTable, base+"-rt")})
	if err != nil {
		e.t.Fatal(err)
	}
	for i, z := range []string{"a", "b", "c"} {
		sn, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: &vpc, AvailabilityZone: new(region + z),
			CidrBlock: new(fmt.Sprintf("10.240.%d.0/20", i*16)), TagSpecifications: tg(types.ResourceTypeSubnet, base+"-"+region+z)})
		if err != nil {
			e.t.Fatal(err)
		}
		if _, err := c.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{RouteTableId: rt.RouteTable.RouteTableId, SubnetId: sn.Subnet.SubnetId}); err != nil {
			e.t.Fatal(err)
		}
	}
	return vpc
}

func TestManagedNetworkIsDeletedInOrderInARegionWithoutADefaultVPC(t *testing.T) {
	e := newEnv(t)
	vpc := e.managedNetwork(noVPC, ciOld, startUTC)
	// A network of a young cluster and an untagged VPC stay.
	young := e.managedNetwork(noVPC, ciYoung, startUTC+3*3600)
	other, err := e.acct.EC2(noVPC).CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.99.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	// An internet gateway nobody attached, tagged by the cluster.
	orphan, err := e.acct.EC2(noVPC).CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: spec(types.ResourceTypeInternetGateway, sweep.ClusterTag, ciOld, sweep.CreatedAtTag, created(startUTC))})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Step(3*time.Hour + time.Minute)

	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{noVPC}})
	if got := outcome(rep, "vpc", vpc); got != sweep.Deleted {
		t.Fatalf("the managed VPC: %q, items %+v", got, rep.Items)
	}
	if got := outcome(rep, "internet-gateway", *orphan.InternetGateway.InternetGatewayId); got != sweep.Deleted {
		t.Errorf("the unattached gateway: %q", got)
	}
	if rep.Count(sweep.Failed) != 0 {
		t.Errorf("failures: %+v", rep.Items)
	}
	left, err := e.acct.EC2(noVPC).DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(left.Vpcs))
	for _, v := range left.Vpcs {
		ids = append(ids, *v.VpcId)
	}
	slices.Sort(ids)
	want := []string{young, *other.Vpc.VpcId}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("VPCs left: %v, want %v", ids, want)
	}
	// The young network is intact: its subnets and gateway still exist.
	subnets, _ := e.acct.EC2(noVPC).DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	if len(subnets.Subnets) != 3 {
		t.Errorf("subnets left: %d, want the 3 of the young network", len(subnets.Subnets))
	}
}

func TestDefaultNetworkIsNeverTouched(t *testing.T) {
	e := newEnv(t)
	c := e.acct.EC2("us-east-2")
	before, _ := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	sgs := e.groupIDs("us-east-2")
	subnets, _ := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	e.clk.Step(10 * time.Hour)
	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour})
	if len(rep.Items) != 0 {
		t.Errorf("an account of only defaults produced items: %+v", rep.Items)
	}
	after, _ := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	sgsAfter := e.groupIDs("us-east-2")
	subnetsAfter, _ := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	if len(after.Vpcs) != len(before.Vpcs) || len(sgsAfter) != len(sgs) ||
		len(subnetsAfter.Subnets) != len(subnets.Subnets) {
		t.Errorf("the default network changed")
	}
}

func TestEveryEnabledRegionIsSwept(t *testing.T) {
	e := newEnv(t)
	// An instance in another region (eu-central-1), not the home region.
	out, err := e.acct.EC2("eu-central-1").RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: new("ami-1"), InstanceType: "t3a.small", MinCount: new(int32(1)), MaxCount: new(int32(1)),
		Placement:         &types.Placement{AvailabilityZone: new("eu-central-1a")},
		TagSpecifications: spec(types.ResourceTypeInstance, sweep.ClusterTag, ciOld)})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Step(3 * time.Hour)
	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour})
	if got := outcome(rep, "instance", *out.Instances[0].InstanceId); got != sweep.Deleted {
		t.Errorf("an instance of another region: %q", got)
	}
}

func TestAFailingRegionDoesNotStopTheOthers(t *testing.T) {
	e := newEnv(t)
	id := e.launch(ciOld)
	e.clk.Step(3 * time.Hour)
	// A region that cannot be reached at all.
	rep, err := sweep.Run(ctx, func(_ context.Context, region string) (awsdriver.Clients, error) {
		if region == "eu-central-1" {
			return awsdriver.Clients{}, fmt.Errorf("no credentials for %s", region)
		}
		return awsdriver.Clients{EC2: e.acct.EC2(region), Quotas: e.acct.Quotas(region)}, nil
	}, "us-east-2", sweep.Options{MaxAge: 2 * time.Hour, Now: e.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if outcome(rep, "instance", id) != sweep.Deleted {
		t.Errorf("us-east-2 was not swept: %+v", rep.Items)
	}
	if outcome(rep, "region", "eu-central-1") != sweep.Failed {
		t.Errorf("the unreachable region is not a failure: %+v", rep.Items)
	}
}

func TestSelectorNeedsNoDefaultButNeverMatchesTheBarePrefix(t *testing.T) {
	e := newEnv(t)
	e.launch("scarpenter-ci-") // the prefix alone is not a cluster name
	e.clk.Step(3 * time.Hour)
	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour})
	if rep.Count(sweep.Deleted) != 0 {
		t.Errorf("the bare prefix matched: %+v", rep.Items)
	}
}

// noAccess is a region the role may not look at: every instance listing is refused.
type noAccess struct{ awsdriver.EC2API }

func (noAccess) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "not authorized in this region"}
}

func TestARegionTheRoleMayNotUseIsSkippedNotFailed(t *testing.T) {
	e := newEnv(t)
	id := e.launch(ciOld)
	e.clk.Step(3 * time.Hour)
	rep, err := sweep.Run(ctx, func(_ context.Context, region string) (awsdriver.Clients, error) {
		if region == "eu-central-1" {
			return awsdriver.Clients{EC2: noAccess{e.acct.EC2(region)}}, nil
		}
		return awsdriver.Clients{EC2: e.acct.EC2(region), Quotas: e.acct.Quotas(region)}, nil
	}, "us-east-2", sweep.Options{MaxAge: 2 * time.Hour, Now: e.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if outcome(rep, "region", "eu-central-1") != sweep.Skipped || rep.Count(sweep.Failed) != 0 {
		t.Errorf("a region without access: %+v", rep.Items)
	}
	if outcome(rep, "instance", id) != sweep.Deleted {
		t.Errorf("the other regions were not swept: %+v", rep.Items)
	}
}

// The account's defaults are never deleted, whatever their tags say: each guard has its
// own assertion, so that removing one fails here and not only in IAM, which is the second
// line (a delete needs the cluster tag in the policy too, and a tag on a default resource is
// all it takes to pass that).
func TestDefaultsAreNeverDeletedEvenWhenTagged(t *testing.T) {
	e := newEnv(t)
	e.acct.TagDefaultResources("us-east-2", map[string]string{sweep.ClusterTag: ciOld, sweep.CreatedAtTag: "1"})
	c := e.acct.EC2("us-east-2")
	vpcs, _ := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	var defaultVPC string
	for _, v := range vpcs.Vpcs {
		if v.IsDefault != nil && *v.IsDefault {
			defaultVPC = *v.VpcId
		}
	}
	if defaultVPC == "" {
		t.Fatal("no default VPC in the fake")
	}
	e.clk.Step(10 * time.Hour)
	rep := e.sweep(sweep.Options{MaxAge: 2 * time.Hour, Regions: []string{"us-east-2"}})

	if got := outcome(rep, "vpc", defaultVPC); got != sweep.Skipped {
		t.Errorf("the default VPC: %q, want skipped", got)
	}
	var sgSkipped, rtSkipped int
	for _, i := range rep.Items {
		switch {
		case i.Kind == "security-group" && i.Outcome == sweep.Skipped:
			sgSkipped++
		case i.Kind == "route-table" && i.Outcome == sweep.Skipped:
			rtSkipped++
		case i.Outcome == sweep.Deleted || i.Outcome == sweep.Failed:
			t.Errorf("something was deleted or attempted: %+v", i)
		}
	}
	if sgSkipped == 0 {
		t.Errorf("the default security group was not reported as skipped: %+v", rep.Items)
	}
	if rtSkipped == 0 {
		t.Errorf("the main route table was not reported as skipped: %+v", rep.Items)
	}
	for _, op := range []string{"DeleteVpc", "DeleteSecurityGroup", "DeleteRouteTable"} {
		if n := e.acct.Calls(op); n != 0 {
			t.Errorf("%s was called %d times on defaults", op, n)
		}
	}
}
