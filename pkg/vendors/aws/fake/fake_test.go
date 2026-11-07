/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/smithy-go"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

// These tests pin the behaviour of the fake the driver tests rely on: where it
// differs from EC2, the driver's tests prove nothing.

var ctx = context.Background()

func code(err error) string {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		return apiErr.ErrorCode()
	}
	return ""
}

func newAccount(t *testing.T) (*fake.Account, *fake.EC2, *clocktesting.FakeClock) {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	a := fake.New(fake.WithClock(clk))
	a.SetInstanceTypes([]fake.InstanceType{{Name: "m7i.large", VCPUs: 2}, {Name: "t3.micro", VCPUs: 2, Burstable: true}, {Name: "p4d.24xlarge", VCPUs: 96}})
	a.SetQuota("us-east-1", "L-1216C47A", 4)
	a.SetQuota("us-east-1", "L-34B43A08", 2)
	return a, a.EC2("us-east-1"), clk
}

func run(t *testing.T, c *fake.EC2, mutate func(*ec2.RunInstancesInput)) (*ec2.RunInstancesOutput, error) {
	t.Helper()
	in := &ec2.RunInstancesInput{ImageId: new("ami-1"), InstanceType: "m7i.large", MinCount: new(int32(1)), MaxCount: new(int32(1)),
		Placement: &types.Placement{AvailabilityZone: new("us-east-1a")}}
	if mutate != nil {
		mutate(in)
	}
	return c.RunInstances(ctx, in)
}

func TestClientTokenIdempotency(t *testing.T) {
	_, c, _ := newAccount(t)
	token := func(in *ec2.RunInstancesInput) { in.ClientToken = new("tok") }
	first, err := run(t, c, token)
	if err != nil {
		t.Fatal(err)
	}
	again, err := run(t, c, token)
	if err != nil || *again.Instances[0].InstanceId != *first.Instances[0].InstanceId {
		t.Fatalf("an identical replay = %v, %v", again, err)
	}
	_, err = run(t, c, func(in *ec2.RunInstancesInput) { token(in); in.Placement.AvailabilityZone = new("us-east-1b") })
	if code(err) != "IdempotentParameterMismatch" {
		t.Errorf("a replay with another zone: %v", err)
	}
	// A failed call does not consume the token.
	a2, c2, _ := newAccount(t)
	a2.Fail(fake.Failure{Op: "RunInstances", Code: "InsufficientInstanceCapacity"})
	if _, err := run(t, c2, token); code(err) != "InsufficientInstanceCapacity" {
		t.Fatalf("got %v", err)
	}
	if _, err := run(t, c2, func(in *ec2.RunInstancesInput) { token(in); in.Placement.AvailabilityZone = new("us-east-1b") }); err != nil {
		t.Errorf("a token after a failed call: %v", err)
	}
}

func TestRunInstancesChecks(t *testing.T) {
	_, c, _ := newAccount(t)
	for name, tc := range map[string]struct {
		mutate func(*ec2.RunInstancesInput)
		want   string
	}{
		"no image":       {func(in *ec2.RunInstancesInput) { in.ImageId = nil }, "MissingParameter"},
		"unknown type":   {func(in *ec2.RunInstancesInput) { in.InstanceType = "zz9.large" }, "InvalidInstanceType"},
		"two instances":  {func(in *ec2.RunInstancesInput) { in.MaxCount = new(int32(2)) }, "InvalidParameterValue"},
		"unknown subnet": {func(in *ec2.RunInstancesInput) { in.SubnetId = new("subnet-nope") }, "InvalidSubnetID.NotFound"},
		"bad user data":  {func(in *ec2.RunInstancesInput) { in.UserData = new("!!!") }, "InvalidUserData.Malformed"},
		"too much user data": {func(in *ec2.RunInstancesInput) {
			in.UserData = new(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 16385))))
		}, "InvalidParameterValue"},
		"unknown group": {func(in *ec2.RunInstancesInput) {
			in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{Groups: []string{"sg-nope"}}}
		}, "InvalidGroup.NotFound"},
		"unknown placement":    {func(in *ec2.RunInstancesInput) { in.Placement.GroupName = new("nope") }, "InvalidPlacementGroup.Unknown"},
		"subnet in other zone": {func(in *ec2.RunInstancesInput) { in.Placement.AvailabilityZone = new("us-east-1z") }, "InvalidParameterValue"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := run(t, c, tc.mutate); code(err) != tc.want {
				t.Errorf("got %v, want %s", err, tc.want)
			}
		})
	}
	// The maximum user-data is accepted.
	if _, err := run(t, c, func(in *ec2.RunInstancesInput) {
		in.UserData = new(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 16384))))
	}); err != nil {
		t.Errorf("16384 bytes: %v", err)
	}
}

func TestQuotaIsEnforcedPerClassAndCapacityType(t *testing.T) {
	a, c, _ := newAccount(t)
	if _, err := run(t, c, nil); err != nil { // 2 of 4
		t.Fatal(err)
	}
	if _, err := run(t, c, nil); err != nil { // 4 of 4
		t.Fatal(err)
	}
	if _, err := run(t, c, nil); code(err) != "VcpuLimitExceeded" {
		t.Errorf("the fifth vCPU: %v", err)
	}
	spot := func(in *ec2.RunInstancesInput) {
		in.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{MarketType: types.MarketTypeSpot}
	}
	if _, err := run(t, c, spot); err != nil {
		t.Errorf("spot has its own quota: %v", err)
	}
	if _, err := run(t, c, spot); code(err) != "MaxSpotInstanceCountExceeded" {
		t.Errorf("a second spot instance: %v", err)
	}
	if _, err := run(t, c, func(in *ec2.RunInstancesInput) { in.InstanceType = "p4d.24xlarge" }); code(err) != "VcpuLimitExceeded" {
		t.Errorf("a P instance with a zero quota: %v", err)
	}
	_ = a
}

func TestInstanceLifecycleAndSpotRequests(t *testing.T) {
	a, c, clk := newAccount(t)
	out, err := run(t, c, func(in *ec2.RunInstancesInput) {
		in.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{MarketType: types.MarketTypeSpot}
		in.TagSpecifications = []types.TagSpecification{{ResourceType: types.ResourceTypeInstance, Tags: []types.Tag{{Key: new("k"), Value: new("v")}}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	id := *out.Instances[0].InstanceId
	if out.Instances[0].InstanceLifecycle != types.InstanceLifecycleTypeSpot {
		t.Error("a spot instance has no lifecycle")
	}
	reqs, _ := c.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{})
	if len(reqs.SpotInstanceRequests) != 1 || *reqs.SpotInstanceRequests[0].InstanceId != id || reqs.SpotInstanceRequests[0].State != types.SpotInstanceStateActive {
		t.Fatalf("spot requests %+v", reqs.SpotInstanceRequests)
	}
	term, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if err != nil || term.TerminatingInstances[0].CurrentState.Name != types.InstanceStateNameTerminated {
		t.Fatalf("terminate: %v, %v", term, err)
	}
	// The request outlives its instance by minutes (verified live), then closes.
	if s := a.SpotRequests("us-east-1"); s[*reqs.SpotInstanceRequests[0].SpotInstanceRequestId] != "active" {
		t.Errorf("the request is %v right after the termination", s)
	}
	clk.Step(2 * time.Minute)
	for _, s := range a.SpotRequests("us-east-1") {
		if s != "active" {
			t.Error("closed too early")
		}
	}
	clk.Step(2 * time.Minute)
	for _, s := range a.SpotRequests("us-east-1") {
		if s != "closed" {
			t.Errorf("not closed after four minutes: %s", s)
		}
	}
	// Terminating twice is fine; an unknown or malformed ID is not.
	if _, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Error(err)
	}
	if _, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{"i-nope"}}); code(err) != "InvalidInstanceID.NotFound" {
		t.Error(err)
	}
	if _, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{"x"}}); code(err) != "InvalidInstanceID.Malformed" {
		t.Error(err)
	}
	// A terminated instance stays describable, its quota use is gone.
	d, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil || d.Reservations[0].Instances[0].State.Name != types.InstanceStateNameTerminated {
		t.Errorf("describe: %v, %v", d, err)
	}
	if _, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{"i-nope"}}); code(err) != "InvalidInstanceID.NotFound" {
		t.Error(err)
	}
	if _, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{"bad"}}); code(err) != "InvalidInstanceID.Malformed" {
		t.Error(err)
	}
}

func TestFiltersAndPagination(t *testing.T) {
	a, c, _ := newAccount(t)
	a.SetQuota("us-east-1", "L-1216C47A", 1000)
	for _, n := range []string{"alpha", "beta", "alpha2", "gamma", "delta"} {
		if _, err := run(t, c, func(in *ec2.RunInstancesInput) {
			in.TagSpecifications = []types.TagSpecification{{ResourceType: types.ResourceTypeInstance, Tags: []types.Tag{{Key: new("Name"), Value: new(n)}}}}
		}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(filters ...types.Filter) (n, calls int) {
		var token *string
		for {
			out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: filters, NextToken: token})
			if err != nil {
				t.Fatal(err)
			}
			calls++
			for _, r := range out.Reservations {
				n += len(r.Instances)
			}
			if token = out.NextToken; token == nil {
				return n, calls
			}
		}
	}
	if n, calls := count(); n != 5 || calls != 2 {
		t.Errorf("all instances: %d in %d pages, want 5 in 2 (pages of three)", n, calls)
	}
	if n, _ := count(types.Filter{Name: new("tag:Name"), Values: []string{"alpha*"}}); n != 2 {
		t.Errorf("a wildcard tag filter matched %d, want 2", n)
	}
	if n, _ := count(types.Filter{Name: new("tag:Name"), Values: []string{"beta", "gamma"}}); n != 2 {
		t.Errorf("alternative values matched %d, want 2", n)
	}
	if n, _ := count(types.Filter{Name: new("tag:Name"), Values: []string{"alpha"}}, types.Filter{Name: new("instance-state-name"), Values: []string{"stopped"}}); n != 0 {
		t.Errorf("filters are ANDed: %d", n)
	}
	if _, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []types.Filter{{Name: new("no-such-filter"), Values: []string{"x"}}}}); code(err) != "InvalidParameterValue" {
		t.Errorf("an unknown filter: %v", err)
	}
}

func TestNetworkDependencies(t *testing.T) {
	a, c, _ := newAccount(t)
	vpc, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.1.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	vpcID := vpc.Vpc.VpcId
	sn, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: vpcID, AvailabilityZone: new("us-east-1a"), CidrBlock: new("10.1.0.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: vpcID, AvailabilityZone: new("us-east-1a"), CidrBlock: new("10.1.0.0/24")}); code(err) != "InvalidSubnet.Conflict" {
		t.Errorf("an overlapping subnet: %v", err)
	}
	if _, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: vpcID, AvailabilityZone: new("us-east-1q"), CidrBlock: new("10.1.1.0/24")}); code(err) != "InvalidParameterValue" {
		t.Errorf("a subnet in an unknown zone: %v", err)
	}
	igw, _ := c.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{})
	gwID := igw.InternetGateway.InternetGatewayId
	if _, err := c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{InternetGatewayId: gwID, VpcId: vpcID}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{InternetGatewayId: gwID, VpcId: vpcID}); code(err) != "Resource.AlreadyAssociated" {
		t.Errorf("attaching twice: %v", err)
	}
	rt, _ := c.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: vpcID})
	rtID := rt.RouteTable.RouteTableId
	if _, err := c.CreateRoute(ctx, &ec2.CreateRouteInput{RouteTableId: rtID, DestinationCidrBlock: new("0.0.0.0/0"), GatewayId: gwID}); err != nil {
		t.Fatal(err)
	}
	assoc, err := c.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{RouteTableId: rtID, SubnetId: sn.Subnet.SubnetId})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: new("g"), Description: new("d"), VpcId: vpcID})
	if _, err := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: new("g"), Description: new("d"), VpcId: vpcID}); code(err) != "InvalidGroup.Duplicate" {
		t.Errorf("a duplicate group name: %v", err)
	}
	// An instance in the subnet.
	if _, err := run(t, c, func(in *ec2.RunInstancesInput) {
		in.Placement.AvailabilityZone = new("us-east-1a")
		in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{
			SubnetId: sn.Subnet.SubnetId, Groups: []string{*g.GroupId}, AssociatePublicIpAddress: new(true)}}
	}); err != nil {
		t.Fatal(err)
	}
	// Dependency errors, each blocking its own deletion.
	for name, tc := range map[string]func() error{
		"vpc with subnets": func() error { _, err := c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: vpcID}); return err },
		"subnet with an instance": func() error {
			_, err := c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: sn.Subnet.SubnetId})
			return err
		},
		"group in use": func() error {
			_, err := c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: g.GroupId})
			return err
		},
		"route table with an association": func() error {
			_, err := c.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: rtID})
			return err
		},
		"attached gateway": func() error {
			_, err := c.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: gwID})
			return err
		},
		"detach with public ip": func() error {
			_, err := c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{InternetGatewayId: gwID, VpcId: vpcID})
			return err
		},
	} {
		if err := tc(); code(err) != "DependencyViolation" {
			t.Errorf("%s: %v, want DependencyViolation", name, err)
		}
	}
	ids := a.InstanceStates("us-east-1")
	for id := range ids {
		if _, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		name string
		do   func() error
	}{
		{"detach", func() error {
			_, err := c.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{InternetGatewayId: gwID, VpcId: vpcID})
			return err
		}},
		{"delete gateway", func() error {
			_, err := c.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: gwID})
			return err
		}},
		{"disassociate", func() error {
			_, err := c.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{AssociationId: assoc.AssociationId})
			return err
		}},
		{"delete route table", func() error {
			_, err := c.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: rtID})
			return err
		}},
		{"delete subnet", func() error {
			_, err := c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: sn.Subnet.SubnetId})
			return err
		}},
		{"vpc still has a group", func() error {
			_, err := c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: vpcID})
			if code(err) != "DependencyViolation" {
				return errors.New("a VPC with a security group of its own was deleted")
			}
			return nil
		}},
		{"delete group", func() error {
			_, err := c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: g.GroupId})
			return err
		}},
		{"delete vpc", func() error { _, err := c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: vpcID}); return err }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	for name, fn := range map[string]func() error{
		"vpc": func() error { _, err := c.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: vpcID}); return err },
		"subnet": func() error {
			_, err := c.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: sn.Subnet.SubnetId})
			return err
		},
		"group": func() error {
			_, err := c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: g.GroupId})
			return err
		},
	} {
		if err := fn(); !strings.HasSuffix(code(err), "NotFound") {
			t.Errorf("deleting a deleted %s: %v", name, err)
		}
	}
}

func TestSecurityGroupRulesAndReferences(t *testing.T) {
	a, c, _ := newAccount(t)
	vpc, _ := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.1.0.0/16")})
	g1, _ := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: new("g1"), Description: new("d"), VpcId: vpc.Vpc.VpcId})
	g2, _ := c.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: new("g2"), Description: new("d"), VpcId: vpc.Vpc.VpcId})
	perm := types.IpPermission{IpProtocol: new("tcp"), FromPort: new(int32(22)), ToPort: new(int32(22)), IpRanges: []types.IpRange{{CidrIp: new("10.0.0.0/8")}}}
	if _, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: g1.GroupId, IpPermissions: []types.IpPermission{perm}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: g1.GroupId, IpPermissions: []types.IpPermission{perm}}); code(err) != "InvalidPermission.Duplicate" {
		t.Errorf("a duplicate rule: %v", err)
	}
	if got := a.SecurityGroupRules("us-east-1", *g1.GroupId); len(got) != 1 || got[0] != "tcp:22-22:10.0.0.0/8" {
		t.Errorf("rules %v", got)
	}
	// A rule of g2 names g1: g1 cannot be deleted.
	if _, err := c.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: g2.GroupId,
		IpPermissions: []types.IpPermission{{IpProtocol: new("-1"), UserIdGroupPairs: []types.UserIdGroupPair{{GroupId: g1.GroupId}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: g1.GroupId}); code(err) != "DependencyViolation" {
		t.Errorf("a group another group's rule names: %v", err)
	}
	if _, err := c.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: g1.GroupId, IpPermissions: []types.IpPermission{perm}}); err != nil {
		t.Fatal(err)
	}
	if got := a.SecurityGroupRules("us-east-1", *g1.GroupId); len(got) != 0 {
		t.Errorf("a revoked rule is still there: %v", got)
	}
	// A group and a subnet of different VPCs do not mix.
	if _, err := run(t, c, func(in *ec2.RunInstancesInput) {
		in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{Groups: []string{*g1.GroupId}}}
	}); code(err) != "InvalidParameter" {
		t.Errorf("a group of another VPC: %v", err)
	}
}

func TestPlacementGroups(t *testing.T) {
	a, c, _ := newAccount(t)
	a.SetQuota("us-east-1", "L-1216C47A", 1000)
	if _, err := c.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("g"), Strategy: types.PlacementStrategyCluster}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("g"), Strategy: types.PlacementStrategyCluster}); code(err) != "InvalidPlacementGroup.Duplicate" {
		t.Errorf("a duplicate group: %v", err)
	}
	if _, err := c.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("x"), Strategy: "bogus"}); code(err) != "InvalidParameterValue" {
		t.Errorf("a bogus strategy: %v", err)
	}
	if _, err := c.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("y"), Strategy: types.PlacementStrategySpread, PartitionCount: new(int32(3))}); code(err) != "InvalidParameterCombination" {
		t.Errorf("partitions on a spread group: %v", err)
	}
	inGroup := func(typ string) func(*ec2.RunInstancesInput) {
		return func(in *ec2.RunInstancesInput) {
			in.InstanceType = types.InstanceType(typ)
			in.Placement.GroupName = new("g")
		}
	}
	if _, err := run(t, c, inGroup("t3.micro")); code(err) != "InvalidParameterCombination" {
		t.Errorf("a burstable type in a cluster group: %v", err)
	}
	out, err := run(t, c, inGroup("m7i.large"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{GroupName: new("g")}); code(err) != "InvalidPlacementGroup.InUse" {
		t.Errorf("a group in use: %v", err)
	}
	if _, err := c.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{*out.Instances[0].InstanceId}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{GroupName: new("g")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{GroupName: new("g")}); code(err) != "InvalidPlacementGroup.Unknown" {
		t.Errorf("a deleted group: %v", err)
	}
	// A spread group takes seven instances per zone.
	if _, err := c.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("s"), Strategy: types.PlacementStrategySpread}); err != nil {
		t.Fatal(err)
	}
	spread := func(in *ec2.RunInstancesInput) { in.Placement.GroupName = new("s") }
	for range 7 {
		if _, err := run(t, c, spread); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(t, c, spread); code(err) != "InsufficientInstanceCapacity" {
		t.Errorf("the eighth: %v", err)
	}
	if _, err := run(t, c, func(in *ec2.RunInstancesInput) { spread(in); in.Placement.AvailabilityZone = new("us-east-1b") }); err != nil {
		t.Errorf("another zone: %v", err)
	}
	if out, err := c.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{GroupNames: []string{"s"}}); err != nil || len(out.PlacementGroups) != 1 {
		t.Errorf("describe by name: %v, %v", out, err)
	}
}

func TestSubnetAddressesAndPublicIPs(t *testing.T) {
	a, c, _ := newAccount(t)
	sn := a.DefaultSubnets("us-east-1")["us-east-1a"]
	a.SetFreeAddresses("us-east-1", sn, 1)
	out, err := run(t, c, nil)
	if err != nil || out.Instances[0].PublicIpAddress == nil {
		t.Fatalf("a launch into a default subnet: %v, %v; default subnets map public addresses", out, err)
	}
	if _, err := run(t, c, nil); code(err) != "InsufficientFreeAddressesInSubnet" {
		t.Errorf("an exhausted subnet: %v", err)
	}
	a.SetFreeAddresses("us-east-1", sn, 5)
	out, err = run(t, c, func(in *ec2.RunInstancesInput) {
		in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{SubnetId: &sn, AssociatePublicIpAddress: new(false)}}
	})
	if err != nil || out.Instances[0].PublicIpAddress != nil {
		t.Errorf("a launch without a public address: %v, %v", out, err)
	}
}

func TestRegionsZonesAndOfferings(t *testing.T) {
	a := fake.New(fake.WithRegions(
		fake.RegionSpec{Name: "us-east-1", AZs: []fake.AZ{{Name: "us-east-1a", ID: "use1-az4"}, {Name: "us-east-1b", ID: "use1-az6"}, {Name: "lz", ID: "use1-lz1", Type: "local-zone"}}},
		fake.RegionSpec{Name: "af-south-1", Disabled: true, NoDefaultVPC: true},
	))
	a.SetInstanceTypes([]fake.InstanceType{{Name: "m7i.large", VCPUs: 2}, {Name: "c7g.large", VCPUs: 2, Arch: "arm64"}})
	a.SetOffering("us-east-1", "c7g.large", "use1-az6")
	c := a.EC2("us-east-1")
	regions, _ := c.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if len(regions.Regions) != 1 {
		t.Errorf("enabled regions %v", regions.Regions)
	}
	all, _ := c.DescribeRegions(ctx, &ec2.DescribeRegionsInput{AllRegions: new(true)})
	if len(all.Regions) != 2 {
		t.Errorf("all regions %v", all.Regions)
	}
	if _, err := a.EC2("af-south-1").DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}); code(err) != "AuthFailure" {
		t.Errorf("a disabled region: %v", err)
	}
	if _, err := a.EC2("mars").DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}); code(err) != "InvalidRegion" {
		t.Errorf("an unknown region: %v", err)
	}
	zones, _ := c.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []types.Filter{{Name: new("zone-type"), Values: []string{"availability-zone"}}}})
	if len(zones.AvailabilityZones) != 2 {
		t.Errorf("zones %v", zones.AvailabilityZones)
	}
	for _, lt := range []struct {
		typ  types.LocationType
		want map[string][]string
	}{
		{types.LocationTypeAvailabilityZoneId, map[string][]string{"m7i.large": {"use1-az4", "use1-az6"}, "c7g.large": {"use1-az6"}}},
		{types.LocationTypeAvailabilityZone, map[string][]string{"m7i.large": {"us-east-1a", "us-east-1b"}, "c7g.large": {"us-east-1b"}}},
		{types.LocationTypeRegion, map[string][]string{"m7i.large": {"us-east-1"}, "c7g.large": {"us-east-1"}}},
	} {
		out, err := c.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{LocationType: lt.typ})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]string{}
		for _, o := range out.InstanceTypeOfferings {
			got[string(o.InstanceType)] = append(got[string(o.InstanceType)], *o.Location)
		}
		for typ, want := range lt.want {
			if strings.Join(got[typ], ",") != strings.Join(want, ",") {
				t.Errorf("%s offerings of %s: %v, want %v", lt.typ, typ, got[typ], want)
			}
		}
	}
	// Default VPC: only where the region has one.
	if len(a.DefaultSubnets("us-east-1")) != 2 {
		t.Errorf("default subnets %v: one per availability zone, none in a local zone", a.DefaultSubnets("us-east-1"))
	}
	if len(a.Regions()) != 2 {
		t.Errorf("regions %v", a.Regions())
	}
}

func TestInstanceTypesAndImages(t *testing.T) {
	a, c, _ := newAccount(t)
	a.AddImage("us-east-1", fake.Image{ID: "ami-a", Name: "Flatcar-stable-1-hvm", Owner: "o1"})
	a.AddImage("us-east-1", fake.Image{ID: "ami-b", Name: "Flatcar-stable-1-arm64-hvm", Owner: "o1", Arch: "arm64"})
	a.AddImage("us-east-1", fake.Image{ID: "ami-c", Name: "Flatcar-stable-1-hvm", Owner: "o2"})
	got, _ := c.DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{"o1"}, Filters: []types.Filter{
		{Name: new("name"), Values: []string{"Flatcar-stable-1-*hvm"}}, {Name: new("architecture"), Values: []string{"arm64"}}}})
	if len(got.Images) != 1 || *got.Images[0].ImageId != "ami-b" {
		t.Errorf("images %v", got.Images)
	}
	if _, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{"ami-nope"}}); code(err) != "InvalidAMIID.NotFound" {
		t.Errorf("an unknown AMI: %v", err)
	}
	var seen int
	var token *string
	for {
		out, err := c.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{NextToken: token, MaxResults: new(int32(2))})
		if err != nil {
			t.Fatal(err)
		}
		seen += len(out.InstanceTypes)
		if token = out.NextToken; token == nil {
			break
		}
	}
	if seen != 3 {
		t.Errorf("%d types", seen)
	}
	if _, err := c.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{"zz9.large"}}); code(err) != "InvalidInstanceType" {
		t.Errorf("an unknown type: %v", err)
	}
	info, _ := c.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: []types.InstanceType{"t3.micro"}})
	for _, s := range info.InstanceTypes[0].PlacementGroupInfo.SupportedStrategies {
		if s == types.PlacementGroupStrategyCluster {
			t.Error("a burstable type supports cluster groups")
		}
	}
}

func TestCapacityReservationsAndForeignInstances(t *testing.T) {
	a, c, _ := newAccount(t)
	a.AddCapacityReservation("us-east-1", fake.CapacityReservation{ID: "cr-1", Type: "m7i.large", AZ: "us-east-1a", Total: 3, Free: 2})
	a.AddCapacityReservation("us-east-1", fake.CapacityReservation{ID: "cr-2", Type: "m7i.large", AZ: "us-east-1a", Total: 1, Free: 1, State: "cancelled"})
	out, _ := c.DescribeCapacityReservations(ctx, &ec2.DescribeCapacityReservationsInput{Filters: []types.Filter{{Name: new("state"), Values: []string{"active"}}}})
	if len(out.CapacityReservations) != 1 || *out.CapacityReservations[0].AvailableInstanceCount != 2 {
		t.Errorf("reservations %v", out.CapacityReservations)
	}
	id := a.AddForeignInstance("us-east-1", "m7i.large", true, types.InstanceStateNameRunning)
	a.AddOpenSpotRequest("us-east-1", "m7i.large")
	d, _ := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if d.Reservations[0].Instances[0].InstanceLifecycle != types.InstanceLifecycleTypeSpot {
		t.Error("a foreign spot instance is not spot")
	}
	reqs, _ := c.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{Filters: []types.Filter{{Name: new("state"), Values: []string{"open"}}}})
	if len(reqs.SpotInstanceRequests) != 1 || reqs.SpotInstanceRequests[0].InstanceId != nil {
		t.Errorf("open requests %v", reqs.SpotInstanceRequests)
	}
	// Foreign instances use the quota too.
	a.SetQuota("us-east-1", "L-34B43A08", 3)
	spot := func(in *ec2.RunInstancesInput) {
		in.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{MarketType: types.MarketTypeSpot}
	}
	if _, err := run(t, c, spot); code(err) != "MaxSpotInstanceCountExceeded" {
		t.Errorf("a spot launch beside a foreign instance: %v", err)
	}
	a.SetInstanceState("us-east-1", id, types.InstanceStateNameStopped)
	if _, err := run(t, c, spot); err != nil {
		t.Errorf("a stopped instance counts against the quota: %v", err)
	}
	if got := a.UserData("us-east-1", id); got != "" {
		t.Errorf("user data %q", got)
	}
	if len(a.Volumes("us-east-1")) != 1 {
		t.Errorf("volumes %v", a.Volumes("us-east-1"))
	}
}

func TestErrorInjection(t *testing.T) {
	a, c, _ := newAccount(t)
	calls := 0
	a.Fail(fake.Failure{Op: "DescribeVpcs", Code: "Throttling", Count: 2, Match: func(in any) bool { calls++; return true }})
	for i := range 3 {
		_, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
		if (i < 2) != (code(err) == "Throttling") {
			t.Errorf("call %d: %v", i, err)
		}
	}
	a.Fail(fake.Failure{Op: "DescribeVpcs", Code: "Forever", Count: -1})
	for range 3 {
		if _, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}); code(err) != "Forever" {
			t.Errorf("an endless failure: %v", err)
		}
	}
	a.Fail(fake.Failure{Op: "GetServiceQuota", Block: true})
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := a.Quotas("us-east-1").GetServiceQuota(cctx, &servicequotas.GetServiceQuotaInput{ServiceCode: new("ec2"), QuotaCode: new("L-1216C47A")}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a blocked call: %v", err)
	}
	if a.Calls("DescribeVpcs") != 6 || len(a.Inputs("DescribeVpcs")) != 6 || calls == 0 {
		t.Errorf("calls %d inputs %d", a.Calls("DescribeVpcs"), len(a.Inputs("DescribeVpcs")))
	}
}

func TestServiceQuotas(t *testing.T) {
	a, _, _ := newAccount(t)
	q := a.Quotas("us-east-1")
	out, err := q.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: new("ec2"), QuotaCode: new("L-1216C47A")})
	if err != nil || *out.Quota.Value != 4 || out.Quota.GlobalQuota {
		t.Fatalf("quota %v, %v", out, err)
	}
	if _, err := q.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: new("ec2"), QuotaCode: new("L-nope")}); code(err) != "NoSuchResourceException" {
		t.Error(err)
	}
	if _, err := q.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: new("s3"), QuotaCode: new("L-1216C47A")}); code(err) != "NoSuchResourceException" {
		t.Error(err)
	}
	a.RemoveQuota("us-east-1", "L-1216C47A")
	if _, err := q.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: new("ec2"), QuotaCode: new("L-1216C47A")}); code(err) != "NoSuchResourceException" {
		t.Error(err)
	}
	n := 0
	var token *string
	for {
		l, err := q.ListServiceQuotas(ctx, &servicequotas.ListServiceQuotasInput{ServiceCode: new("ec2"), NextToken: token})
		if err != nil {
			t.Fatal(err)
		}
		n += len(l.Quotas)
		if token = l.NextToken; token == nil {
			break
		}
	}
	if n != 17 {
		t.Errorf("%d quotas listed, want the 17 that remain", n)
	}
	// The defaults of a new account: five Standard vCPUs and nothing else.
	b := fake.New()
	if got, _ := b.Quotas("us-east-1").GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: new("ec2"), QuotaCode: new("L-1216C47A")}); *got.Quota.Value != 5 {
		t.Errorf("standard default %v", *got.Quota.Value)
	}
	if got, _ := b.Quotas("us-east-1").GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: new("ec2"), QuotaCode: new("L-DB2E81BA")}); *got.Quota.Value != 0 {
		t.Errorf("G default %v", *got.Quota.Value)
	}
}

func TestDescribeCallsOfTheNetworkObjects(t *testing.T) {
	a, c, _ := newAccount(t)
	defaults := a.DefaultSubnets("us-east-1")
	subs, err := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []types.Filter{{Name: new("default-for-az"), Values: []string{"true"}}}})
	if err != nil || len(subs.Subnets) != 3 {
		t.Fatalf("default subnets %v, %v", subs, err)
	}
	for _, s := range subs.Subnets {
		if defaults[*s.AvailabilityZone] != *s.SubnetId || *s.AvailableIpAddressCount <= 0 || *s.AvailabilityZoneId == "" {
			t.Errorf("subnet %+v", s)
		}
	}
	vpcs, _ := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []types.Filter{{Name: new("isDefault"), Values: []string{"true"}}}})
	if len(vpcs.Vpcs) != 1 {
		t.Fatalf("default VPCs %v", vpcs.Vpcs)
	}
	igws, _ := c.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{Filters: []types.Filter{{Name: new("attachment.vpc-id"), Values: []string{*vpcs.Vpcs[0].VpcId}}}})
	if len(igws.InternetGateways) != 1 || *igws.InternetGateways[0].Attachments[0].VpcId != *vpcs.Vpcs[0].VpcId {
		t.Errorf("the default VPC's gateway %v", igws.InternetGateways)
	}
	rts, _ := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: []types.Filter{{Name: new("vpc-id"), Values: []string{*vpcs.Vpcs[0].VpcId}}}})
	if len(rts.RouteTables) != 1 || !*rts.RouteTables[0].Associations[0].Main {
		t.Errorf("the default VPC's route tables %v", rts.RouteTables)
	}
	sgs, _ := c.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []types.Filter{
		{Name: new("vpc-id"), Values: []string{*vpcs.Vpcs[0].VpcId}}, {Name: new("group-name"), Values: []string{"default"}}}})
	if len(sgs.SecurityGroups) != 1 || len(sgs.SecurityGroups[0].IpPermissions) != 1 {
		t.Errorf("the default group %v: it allows its own members", sgs.SecurityGroups)
	}
	if a.Clock() == nil {
		t.Error("no clock")
	}
}

func TestIPv6AndRoutes(t *testing.T) {
	a := fake.New(fake.WithSlowIPv6(2))
	c := a.EC2("us-east-1")
	vpc, err := c.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: new("10.1.0.0/16"), AmazonProvidedIpv6CidrBlock: new(true)})
	if err != nil {
		t.Fatal(err)
	}
	id := *vpc.Vpc.VpcId
	state := func() types.VpcCidrBlockStateCode {
		out, _ := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{id}})
		return out.Vpcs[0].Ipv6CidrBlockAssociationSet[0].Ipv6CidrBlockState.State
	}
	seen := make([]types.VpcCidrBlockStateCode, 0, 3)
	for range 3 {
		seen = append(seen, state())
	}
	want := []types.VpcCidrBlockStateCode{types.VpcCidrBlockStateCodeAssociating,
		types.VpcCidrBlockStateCodeAssociating, types.VpcCidrBlockStateCodeAssociated}
	if !slices.Equal(seen, want) {
		t.Errorf("states %v, want associating twice then associated", seen)
	}
	out, _ := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{id}})
	block := *out.Vpcs[0].Ipv6CidrBlockAssociationSet[0].Ipv6CidrBlock
	if _, err := c.AssociateVpcCidrBlock(ctx, &ec2.AssociateVpcCidrBlockInput{VpcId: &id, AmazonProvidedIpv6CidrBlock: new(true)}); code(err) != "CidrConflict" {
		t.Errorf("a second block: %v", err)
	}
	if _, err := c.AssociateVpcCidrBlock(ctx, &ec2.AssociateVpcCidrBlockInput{VpcId: &id}); code(err) != "MissingParameter" {
		t.Errorf("no block asked for: %v", err)
	}
	if _, err := c.AssociateVpcCidrBlock(ctx, &ec2.AssociateVpcCidrBlockInput{VpcId: new("vpc-nope"), AmazonProvidedIpv6CidrBlock: new(true)}); code(err) != "InvalidVpcID.NotFound" {
		t.Errorf("an unknown VPC: %v", err)
	}
	// The /64 of a subnet must be inside the /56 and unique.
	p := netip.MustParsePrefix(block)
	raw := p.Addr().As16()
	raw[7] = 3
	cidr64 := netip.PrefixFrom(netip.AddrFrom16(raw), 64).String()
	sn, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: &id, AvailabilityZone: new("us-east-1a"), CidrBlock: new("10.1.0.0/24"), Ipv6CidrBlock: &cidr64})
	if err != nil {
		t.Fatal(err)
	}
	if len(sn.Subnet.Ipv6CidrBlockAssociationSet) != 1 {
		t.Errorf("subnet %+v", sn.Subnet)
	}
	other, err := c.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: &id, AvailabilityZone: new("us-east-1b"), CidrBlock: new("10.1.1.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ cidr, want string }{
		"a duplicate":  {cidr64, "InvalidSubnet.Conflict"},
		"outside":      {"2001:db8::/64", "InvalidParameterValue"},
		"not a /64":    {block, "InvalidParameterValue"},
		"not a prefix": {"nonsense", "InvalidParameterValue"},
	} {
		if _, err := c.AssociateSubnetCidrBlock(ctx, &ec2.AssociateSubnetCidrBlockInput{SubnetId: other.Subnet.SubnetId, Ipv6CidrBlock: new(tc.cidr)}); code(err) != tc.want {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
	if _, err := c.AssociateSubnetCidrBlock(ctx, &ec2.AssociateSubnetCidrBlockInput{SubnetId: new("subnet-nope"), Ipv6CidrBlock: &cidr64}); code(err) != "InvalidSubnetID.NotFound" {
		t.Errorf("an unknown subnet: %v", err)
	}
	a.SetSubnetIPv6Associating("us-east-1", *other.Subnet.SubnetId, 9)
	subs, _ := c.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{*other.Subnet.SubnetId}})
	if subs.Subnets[0].Ipv6CidrBlockAssociationSet[0].Ipv6CidrBlockState.State != types.SubnetCidrBlockStateCodeAssociating {
		t.Error("the subnet's block is not associating")
	}

	// Routes of both families, replace and not found.
	igw, _ := c.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{})
	gw := igw.InternetGateway.InternetGatewayId
	if _, err := c.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{InternetGatewayId: gw, VpcId: &id}); err != nil {
		t.Fatal(err)
	}
	rt, _ := c.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: &id})
	tbl := rt.RouteTable.RouteTableId
	if _, err := c.ReplaceRoute(ctx, &ec2.ReplaceRouteInput{RouteTableId: tbl, DestinationIpv6CidrBlock: new("::/0"), GatewayId: gw}); code(err) != "InvalidRoute.NotFound" {
		t.Errorf("replacing a route that does not exist: %v", err)
	}
	if _, err := c.CreateRoute(ctx, &ec2.CreateRouteInput{RouteTableId: tbl, DestinationIpv6CidrBlock: new("::/0"), GatewayId: gw}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRoute(ctx, &ec2.CreateRouteInput{RouteTableId: tbl, DestinationIpv6CidrBlock: new("::/0"), GatewayId: gw}); code(err) != "RouteAlreadyExists" {
		t.Errorf("a duplicate route: %v", err)
	}
	a.AddRoute("us-east-1", *tbl, "0.0.0.0/0", "igw-stale")
	if _, err := c.ReplaceRoute(ctx, &ec2.ReplaceRouteInput{RouteTableId: tbl, DestinationCidrBlock: new("0.0.0.0/0"), GatewayId: gw}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReplaceRoute(ctx, &ec2.ReplaceRouteInput{RouteTableId: new("rtb-nope"), DestinationCidrBlock: new("0.0.0.0/0"), GatewayId: gw}); code(err) != "InvalidRouteTableID.NotFound" {
		t.Errorf("an unknown table: %v", err)
	}
	tables, _ := c.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{})
	var v6, v4 bool
	for _, tb := range tables.RouteTables {
		for _, r := range tb.Routes {
			v6 = v6 || (r.DestinationIpv6CidrBlock != nil && *r.GatewayId == *gw)
			v4 = v4 || (r.DestinationCidrBlock != nil && *r.GatewayId == *gw)
		}
	}
	if !v6 || !v4 {
		t.Errorf("routes: ipv6 %v ipv4 %v", v6, v4)
	}
}

func TestInstancesGetIPv6OnlyFromASubnetWithIt(t *testing.T) {
	r := fake.DefaultRegion("us-east-1", "use1")
	r.DefaultVPCIPv6 = true
	a := fake.New(fake.WithRegions(r))
	a.SetInstanceTypes([]fake.InstanceType{{Name: "m7i.large", VCPUs: 2}})
	a.SetQuota("us-east-1", "L-1216C47A", 100)
	c := a.EC2("us-east-1")
	out, err := run(t, c, func(in *ec2.RunInstancesInput) {
		in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{Ipv6AddressCount: new(int32(1))}}
	})
	if err != nil || len(out.Instances[0].NetworkInterfaces[0].Ipv6Addresses) != 1 {
		t.Fatalf("%v, %v", out, err)
	}
	b := fake.New()
	b.SetInstanceTypes([]fake.InstanceType{{Name: "m7i.large", VCPUs: 2}})
	b.SetQuota("us-east-1", "L-1216C47A", 100)
	if _, err := run(t, b.EC2("us-east-1"), func(in *ec2.RunInstancesInput) {
		in.NetworkInterfaces = []types.InstanceNetworkInterfaceSpecification{{Ipv6AddressCount: new(int32(1))}}
	}); code(err) != "InvalidParameterValue" {
		t.Errorf("a subnet without IPv6: %v", err)
	}
}
