/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

func spec(name string, kind vendors.ResourceKind, params any) vendors.CreateSpec {
	var raw json.RawMessage
	if params != nil {
		raw, _ = json.Marshal(params)
	}
	return vendors.CreateSpec{
		Name: name, Params: raw, ExtraTags: map[string]string{"team": "data"},
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: testCluster, vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
			vendors.TagResource: string(kind),
		},
	}
}

func TestKinds(t *testing.T) {
	e := newEnv(t, envOptions{})
	byKind := map[vendors.ResourceKind]vendors.KindSpec{}
	for _, k := range e.d.Kinds() {
		byKind[k.Kind] = k
	}
	// Plan 06's defaults: subnets (the regional network) idleTTL, placement groups
	// perUse per run; the NodeClass's groups idleTTL, the run's groups perUse.
	for kind, want := range map[vendors.ResourceKind]struct {
		scope  vendors.ResourceScope
		policy string
	}{
		aws.KindSubnets: {vendors.ScopeRegion, "idleTTL"}, aws.KindSecurityGroups: {vendors.ScopeRegion, "idleTTL"},
		aws.KindPlacementGroups: {vendors.ScopeRun, "perUse"}, aws.KindRunSecurityGroups: {vendors.ScopeRun, "perUse"},
	} {
		k, ok := byKind[kind]
		if !ok || k.Scope != want.scope || string(k.DefaultPolicy) != want.policy {
			t.Errorf("kind %s = %+v, want scope %s policy %s", kind, k, want.scope, want.policy)
		}
	}
	// Deletion order: groups before the network that holds them.
	if byKind[aws.KindPlacementGroups].DeleteOrder >= byKind[aws.KindSecurityGroups].DeleteOrder ||
		byKind[aws.KindSecurityGroups].DeleteOrder >= byKind[aws.KindSubnets].DeleteOrder ||
		byKind[aws.KindRunSecurityGroups].DeleteOrder >= byKind[aws.KindSubnets].DeleteOrder {
		t.Errorf("delete orders %+v", byKind)
	}
}

func TestResolveDefaultsAreTheDefaultVPCsSubnets(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	got, err := e.d.Resolve(ctx, aws.KindSubnets, testRegion, vendors.UseRef{})
	if err != nil {
		t.Fatal(err)
	}
	want := e.acct.DefaultSubnets(testRegion)
	if len(got) != len(want) {
		t.Fatalf("resolved %d subnets, want the default subnet of each of the %d zones", len(got), len(want))
	}
	for _, r := range got {
		if r.Managed || !slices.Contains(mapValues(want), r.ID) {
			t.Errorf("subnet %+v is not an unmanaged default subnet", r)
		}
	}
	// Nothing else has a default: the groups are SCarpenter's own.
	for _, kind := range []vendors.ResourceKind{aws.KindSecurityGroups, aws.KindPlacementGroups, aws.KindRunSecurityGroups} {
		if got, err := e.d.Resolve(ctx, kind, testRegion, vendors.UseRef{}); err != nil || len(got) != 0 {
			t.Errorf("%s default = %v, %v", kind, got, err)
		}
	}
	// A region without a default VPC resolves to nothing, not an error.
	regions := []fake.RegionSpec{fake.DefaultRegion(testRegion, "use1")}
	regions[0].NoDefaultVPC = true
	e2 := newEnv(t, envOptions{regions: regions})
	if got, err := e2.d.Resolve(ctx, aws.KindSubnets, testRegion, vendors.UseRef{}); err != nil || len(got) != 0 {
		t.Errorf("no default VPC: %v, %v", got, err)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestResolveByIDsNamePatternAndTags(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	ec := e.acct.EC2(testRegion)
	vpc := defaultVPC(t, e, testRegion)
	mk := func(name, cidr, zone string, tags ...ec2types.Tag) ec2types.Subnet {
		out, err := ec.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: &vpc, AvailabilityZone: &zone, CidrBlock: &cidr,
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSubnet,
				Tags: append([]ec2types.Tag{{Key: new("Name"), Value: &name}}, tags...)}}})
		if err != nil {
			t.Fatal(err)
		}
		return *out.Subnet
	}
	a := mk("nodes-a", "172.31.211.0/24", "us-east-1a", ec2types.Tag{Key: new("use"), Value: new("scarpenter")})
	b := mk("nodes-b", "172.31.212.0/24", "us-east-1b", ec2types.Tag{Key: new("use"), Value: new("scarpenter")})
	mk("other", "172.31.213.0/24", "us-east-1c")
	idsOf := func(refs []vendors.ResourceRef) []string {
		out := make([]string, 0, len(refs))
		for _, r := range refs {
			out = append(out, r.ID)
		}
		return out
	}
	for name, tc := range map[string]struct {
		ref  vendors.UseRef
		want []string
	}{
		"ids":                     {vendors.UseRef{IDs: []string{*a.SubnetId, *b.SubnetId}}, []string{*a.SubnetId, *b.SubnetId}},
		"an unknown id is absent": {vendors.UseRef{IDs: []string{*a.SubnetId, "subnet-00000000000000000"}}, []string{*a.SubnetId}},
		"name pattern":            {vendors.UseRef{NamePattern: "nodes-*"}, []string{*a.SubnetId, *b.SubnetId}},
		"exact name":              {vendors.UseRef{NamePattern: "nodes-a"}, []string{*a.SubnetId}},
		"tags":                    {vendors.UseRef{Tags: map[string]string{"use": "scarpenter"}}, []string{*a.SubnetId, *b.SubnetId}},
		"tags that match nothing": {vendors.UseRef{Tags: map[string]string{"use": "nobody"}}, nil},
		"two tags":                {vendors.UseRef{Tags: map[string]string{"use": "scarpenter", "Name": "nodes-b"}}, []string{*b.SubnetId}},
	} {
		got, err := e.d.Resolve(ctx, aws.KindSubnets, testRegion, tc.ref)
		slices.Sort(tc.want)
		if err != nil || !slices.Equal(idsOf(got), tc.want) {
			t.Errorf("%s: %v, %v; want %v", name, idsOf(got), err, tc.want)
		}
		for _, r := range got {
			if r.Managed || r.Kind != aws.KindSubnets || r.Region != testRegion {
				t.Errorf("%s: %+v", name, r)
			}
		}
	}
	// Security and placement groups resolve the same way.
	g, err := ec.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: new("my-sg"), Description: new("x"), VpcId: &vpc,
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSecurityGroup, Tags: []ec2types.Tag{{Key: new("use"), Value: new("yes")}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []vendors.UseRef{{IDs: []string{*g.GroupId}}, {NamePattern: "my-*"}, {Tags: map[string]string{"use": "yes"}}} {
		got, err := e.d.Resolve(ctx, aws.KindSecurityGroups, testRegion, ref)
		if err != nil || len(got) != 1 || got[0].ID != *g.GroupId || got[0].Name != "my-sg" {
			t.Errorf("security group by %+v: %v, %v", ref, got, err)
		}
	}
	if _, err := ec.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("my-pg"), Strategy: ec2types.PlacementStrategySpread,
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypePlacementGroup, Tags: []ec2types.Tag{{Key: new("use"), Value: new("yes")}}}}}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []vendors.UseRef{{NamePattern: "my-pg"}, {Tags: map[string]string{"use": "yes"}}} {
		got, err := e.d.Resolve(ctx, aws.KindPlacementGroups, testRegion, ref)
		if err != nil || len(got) != 1 || got[0].Name != "my-pg" {
			t.Errorf("placement group by %+v: %v, %v", ref, got, err)
		}
	}
	pgs, _ := e.d.Resolve(ctx, aws.KindPlacementGroups, testRegion, vendors.UseRef{NamePattern: "my-pg"})
	byID, err := e.d.Resolve(ctx, aws.KindPlacementGroups, testRegion, vendors.UseRef{IDs: []string{pgs[0].ID, "pg-00000000000000000"}})
	if err != nil || len(byID) != 1 {
		t.Errorf("placement group by ID: %v, %v", byID, err)
	}
	if _, err := e.d.Resolve(ctx, "bogus", testRegion, vendors.UseRef{}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an unknown kind: %v", err)
	}
}

func TestEnsureAdoptsAfterARestartAndNeverDuplicates(t *testing.T) {
	for _, kind := range []vendors.ResourceKind{aws.KindPlacementGroups, aws.KindSecurityGroups, aws.KindRunSecurityGroups, aws.KindSubnets} {
		t.Run(string(kind), func(t *testing.T) {
			e := newEnv(t, envOptions{})
			ctx := context.Background()
			var params any
			if kind == aws.KindSecurityGroups || kind == aws.KindRunSecurityGroups {
				params = aws.SecurityGroupParams{VPC: defaultVPC(t, e, testRegion), SelfReference: true}
			}
			sp := spec("scarpenter-my-cluster-"+string(kind)+"-us-east-1", kind, params)
			first, created, err := e.d.Ensure(ctx, kind, testRegion, sp)
			if err != nil || !created || !first.Managed || first.Name != sp.Name || first.Tags[vendors.TagResource] != string(kind) {
				t.Fatalf("Ensure = %+v, %v, %v", first, created, err)
			}
			again, created, err := e.d.Ensure(ctx, kind, testRegion, sp)
			if err != nil || created || again.ID != first.ID {
				t.Errorf("second Ensure = %+v, %v, %v", again, created, err)
			}
			e2 := e.restart(t)
			adopted, created, err := e2.d.Ensure(ctx, kind, testRegion, sp)
			if err != nil || created || adopted.ID != first.ID {
				t.Errorf("after a restart Ensure = %+v, %v, %v; want adoption", adopted, created, err)
			}
			listed, err := e2.d.List(ctx, nil, map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagManagedBy: vendors.ManagedByValue})
			if err != nil || len(listed) != 1 || listed[0].ID != first.ID || listed[0].Kind != kind || !listed[0].Managed ||
				listed[0].Tags[vendors.TagNodeClass] != "class" || listed[0].Name != sp.Name {
				t.Errorf("List = %+v, %v", listed, err)
			}
			if other, _ := e2.d.List(ctx, nil, map[vendors.TagKey]string{vendors.TagCluster: "another"}); len(other) != 0 {
				t.Errorf("another cluster's listing: %+v", other)
			}
		})
	}
}

func TestEnsureRefusesANameThatExistsWithoutTheMarker(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	vpc := defaultVPC(t, e, testRegion)
	if _, err := e.acct.EC2(testRegion).CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: new("taken"), Description: new("x"), VpcId: &vpc}); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.d.Ensure(ctx, aws.KindSecurityGroups, testRegion, spec("taken", aws.KindSecurityGroups, aws.SecurityGroupParams{VPC: vpc}))
	if vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("adopting a group someone else made: %v", err)
	}
	if _, err := e.acct.EC2(testRegion).CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("taken-pg"), Strategy: ec2types.PlacementStrategyCluster}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.d.Ensure(ctx, aws.KindPlacementGroups, testRegion, spec("taken-pg", aws.KindPlacementGroups, nil)); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("adopting a placement group someone else made: %v", err)
	}
	if _, _, err := e.d.Ensure(ctx, aws.KindSecurityGroups, testRegion, spec("x", aws.KindSecurityGroups, nil)); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a security group without a VPC: %v", err)
	}
	if _, _, err := e.d.Ensure(ctx, aws.KindPlacementGroups, testRegion, vendors.CreateSpec{}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a resource without a name: %v", err)
	}
}

// hideFirstLookup answers the first lookup of a group by name with nothing: the
// creator that races this one has not finished yet.
type hideFirstLookup struct {
	aws.EC2API
	hidden bool
}

func (h *hideFirstLookup) DescribePlacementGroups(ctx context.Context, in *ec2.DescribePlacementGroupsInput, opts ...func(*ec2.Options),
) (*ec2.DescribePlacementGroupsOutput, error) {
	if !h.hidden {
		h.hidden = true
		return &ec2.DescribePlacementGroupsOutput{}, nil
	}
	return h.EC2API.DescribePlacementGroups(ctx, in, opts...)
}

func TestEnsureAdoptsAGroupCreatedByARacer(t *testing.T) {
	// Another creator of the same name wins the race: EC2's Duplicate is adoption,
	// not an error.
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	sp := spec("scarpenter-my-cluster-placement-groups-us-east-1", aws.KindPlacementGroups, nil)
	winner, _, err := e.d.Ensure(ctx, aws.KindPlacementGroups, testRegion, sp)
	if err != nil {
		t.Fatal(err)
	}
	racer := aws.New(aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
		return aws.Clients{EC2: &hideFirstLookup{EC2API: e.acct.EC2(region)}, Quotas: e.acct.Quotas(region)}, nil
	}))
	if err := racer.Init(ctx, vendors.Config{ClusterName: testCluster, Clock: e.clock}); err != nil {
		t.Fatal(err)
	}
	ref, created, err := racer.Ensure(ctx, aws.KindPlacementGroups, testRegion, sp)
	if err != nil || created || ref.ID != winner.ID {
		t.Errorf("Ensure of the loser = %+v, %v, %v; want the winner's group, not created", ref, created, err)
	}
}

func TestSecurityGroupRulesAreBroughtInLine(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	vpc := defaultVPC(t, e, testRegion)
	sp := spec("scarpenter-my-cluster-security-groups-us-east-1", aws.KindSecurityGroups, nil)
	mk := func(p aws.SecurityGroupParams) vendors.ResourceRef {
		raw, _ := json.Marshal(p)
		sp.Params = raw
		ref, _, err := e.d.Ensure(ctx, aws.KindSecurityGroups, testRegion, sp)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	ref := mk(aws.SecurityGroupParams{VPC: vpc, SelfReference: true})
	if got := e.acct.SecurityGroupRules(testRegion, ref.ID); !slices.Equal(got, []string{"-1:0-0:" + ref.ID}) {
		t.Fatalf("rules %v", got)
	}
	// A creation interrupted after the group existed (or a rule someone removed or
	// added by hand) is finished by the next Ensure.
	_, err := e.acct.EC2(testRegion).AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: &ref.ID,
		IpPermissions: []ec2types.IpPermission{{IpProtocol: new("tcp"), FromPort: new(int32(9999)), ToPort: new(int32(9999)), IpRanges: []ec2types.IpRange{{CidrIp: new("0.0.0.0/0")}}}}})
	if err != nil {
		t.Fatal(err)
	}
	mk(aws.SecurityGroupParams{VPC: vpc, SelfReference: true})
	if got := e.acct.SecurityGroupRules(testRegion, ref.ID); !slices.Equal(got, []string{"-1:0-0:" + ref.ID}) {
		t.Errorf("a rule added by hand stayed: %v", got)
	}
}

func TestManagedNetworkIsCompletedAndDeletedInDependencyOrder(t *testing.T) {
	regions := []fake.RegionSpec{fake.DefaultRegion(testRegion, "use1")}
	regions[0].NoDefaultVPC = true
	e := newEnv(t, envOptions{regions: regions})
	ctx := context.Background()
	sp := spec("scarpenter-my-cluster-subnets-us-east-1", aws.KindSubnets, nil)
	// A creation that stops after the subnets (the gateway call fails) ...
	e.acct.Fail(fake.Failure{Op: "CreateInternetGateway", Code: "InternalError"})
	if _, _, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, sp); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("got %v", err)
	}
	// ... is finished by the next call, with no second VPC and no second subnet.
	ref, created, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, sp)
	if err != nil || created {
		t.Fatalf("Ensure after a partial creation = %+v, %v, %v", ref, created, err)
	}
	if e.acct.Calls("CreateVpc") != 1 || e.acct.Calls("CreateSubnet") != 3 || e.acct.Calls("AttachInternetGateway") != 1 {
		t.Errorf("creations: vpc %d, subnets %d, attach %d", e.acct.Calls("CreateVpc"), e.acct.Calls("CreateSubnet"), e.acct.Calls("AttachInternetGateway"))
	}
	// The route table routes to the gateway and every subnet uses it.
	if e.acct.Calls("CreateRoute") != 1 || e.acct.Calls("AssociateRouteTable") != 3 {
		t.Errorf("route %d, associations %d", e.acct.Calls("CreateRoute"), e.acct.Calls("AssociateRouteTable"))
	}
	if !isIDPrefix(ref.ID, "vpc-") {
		t.Errorf("the managed resource is %q, want the VPC", ref.ID)
	}

	// A launch in the network keeps it.
	// (a VPC reference in the prepared resources stands for its subnet in the zone)
	req := request(testRegion, "us-east-1a", testType, vendors.CapacityOnDemand)
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{ref}}
	inst, err := e.d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	inUse, _, err := e.d.InUse(ctx, ref)
	if err != nil || !inUse {
		t.Errorf("InUse = %v, %v", inUse, err)
	}
	// A security group of ours in the VPC blocks the network's deletion (the
	// garbage collector deletes groups first, by their kind's order).
	sgRef, _, err := e.d.Ensure(ctx, aws.KindSecurityGroups, testRegion,
		spec("scarpenter-my-cluster-security-groups-us-east-1", aws.KindSecurityGroups, aws.SecurityGroupParams{VPC: ref.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.Delete(ctx, ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("deleting a network with an instance: %v, want Retryable", err)
	}
	if err := e.d.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Delete(ctx, ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("deleting a network that still holds a security group: %v, want Retryable", err)
	}
	if err := e.d.Delete(ctx, sgRef); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := e.d.Delete(ctx, ref); err != nil {
			t.Fatalf("Delete #%d: %v", i+1, err)
		}
	}
	listed, _ := e.d.List(ctx, nil, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if len(listed) != 0 {
		t.Errorf("after the deletion: %+v", listed)
	}
	vpcs, _ := e.acct.EC2(testRegion).DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if len(vpcs.Vpcs) != 0 {
		t.Errorf("VPCs left: %d", len(vpcs.Vpcs))
	}
	igws, _ := e.acct.EC2(testRegion).DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{})
	rts, _ := e.acct.EC2(testRegion).DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{})
	if len(igws.InternetGateways) != 0 || len(rts.RouteTables) != 0 {
		t.Errorf("left over: %d gateways, %d route tables", len(igws.InternetGateways), len(rts.RouteTables))
	}
}

func isIDPrefix(id, prefix string) bool { return len(id) > len(prefix) && id[:len(prefix)] == prefix }

func TestDeleteNeverTouchesWhatSCarpenterDidNotCreate(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	vpc := defaultVPC(t, e, testRegion)
	ec := e.acct.EC2(testRegion)
	g, err := ec.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{GroupName: new("users"), Description: new("x"), VpcId: &vpc})
	if err != nil {
		t.Fatal(err)
	}
	// Even when the reference claims the marker: the resource itself must have it.
	claims := map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagManagedBy: vendors.ManagedByValue}
	for _, ref := range []vendors.ResourceRef{
		{Kind: aws.KindSecurityGroups, Region: testRegion, ID: *g.GroupId, Tags: claims},
		{Kind: aws.KindSecurityGroups, Region: testRegion, ID: *g.GroupId},
		{Kind: aws.KindSubnets, Region: testRegion, ID: vpc, Tags: claims}, // the default VPC
		{Kind: aws.KindSubnets, Region: testRegion, ID: e.acct.DefaultSubnets(testRegion)[testZone], Tags: claims},
	} {
		if err := e.d.Delete(ctx, ref); err == nil {
			t.Errorf("Delete(%+v) succeeded", ref)
		}
	}
	if _, err := ec.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{GroupName: new("users-pg"), Strategy: ec2types.PlacementStrategyCluster}); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Delete(ctx, vendors.ResourceRef{Kind: aws.KindPlacementGroups, Region: testRegion, Name: "users-pg", Tags: claims}); err == nil {
		t.Error("a placement group SCarpenter did not create was deleted")
	}
	// A resource of another cluster with the right tags is not ours either.
	other := spec("scarpenter-other-placement-groups-us-east-1", aws.KindPlacementGroups, nil)
	other.Tags[vendors.TagCluster] = "other"
	ref, _, err := e.d.Ensure(ctx, aws.KindPlacementGroups, testRegion, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.Delete(ctx, ref); err == nil {
		t.Error("another cluster's placement group was deleted")
	}
	// Gone is success.
	gone := vendors.ResourceRef{Kind: aws.KindSecurityGroups, Region: testRegion, ID: "sg-00000000000000000", Tags: claims}
	if err := e.d.Delete(ctx, gone); err != nil {
		t.Errorf("a resource that is gone: %v", err)
	}
}

func TestInUseAndDeletePerKind(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	vpc := defaultVPC(t, e, testRegion)
	sg, _, err := e.d.Ensure(ctx, aws.KindSecurityGroups, testRegion,
		spec("scarpenter-my-cluster-security-groups-us-east-1", aws.KindSecurityGroups, aws.SecurityGroupParams{VPC: vpc}))
	if err != nil {
		t.Fatal(err)
	}
	pg, _, err := e.d.Ensure(ctx, aws.KindPlacementGroups, testRegion, spec("scarpenter-my-cluster-placement-groups-us-east-1-x", aws.KindPlacementGroups, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []vendors.ResourceRef{sg, pg} {
		inUse, last, err := e.d.InUse(ctx, r)
		if err != nil || inUse || !last.IsZero() {
			t.Errorf("%s fresh: InUse = %v, %v, %v; the last use of an unused resource is unknown (zero)", r.Kind, inUse, last, err)
		}
	}
	req := request(testRegion, testZone, testType, vendors.CapacityOnDemand)
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{
		{Kind: aws.KindSubnets, ID: e.acct.DefaultSubnets(testRegion)[testZone]}, sg, pg,
	}}
	inst, err := e.d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []vendors.ResourceRef{sg, pg} {
		inUse, last, err := e.d.InUse(ctx, r)
		if err != nil || !inUse || !last.Equal(e.clock.Now()) {
			t.Errorf("%s in use: %v, %v, %v", r.Kind, inUse, last, err)
		}
		if err := e.d.Delete(ctx, r); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("%s: deleting a resource in use: %v, want Retryable", r.Kind, err)
		}
	}
	if err := e.d.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	e.clock.Step(time.Minute)
	for _, r := range []vendors.ResourceRef{sg, pg} {
		if inUse, _, err := e.d.InUse(ctx, r); err != nil || inUse {
			t.Errorf("%s after the instance ended: %v, %v", r.Kind, inUse, err)
		}
		if err := e.d.Delete(ctx, r); err != nil {
			t.Errorf("%s: %v", r.Kind, err)
		}
		if _, _, err := e.d.InUse(ctx, r); !vendors.IsClass(err, vendors.NotFound) {
			t.Errorf("%s: InUse of a deleted resource: %v", r.Kind, err)
		}
	}
	if _, _, err := e.d.InUse(ctx, vendors.ResourceRef{Kind: "bogus"}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an unknown kind: %v", err)
	}
}

func TestListInAnUnknownRegionIsEmpty(t *testing.T) {
	e := newEnv(t, envOptions{})
	got, err := e.d.List(context.Background(), []string{"mars-north-1"}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(got) != 0 {
		t.Errorf("%v, %v", got, err)
	}
}

func TestManagedNetworkDeletionLeavesAForeignSubnetAlone(t *testing.T) {
	regions := []fake.RegionSpec{fake.DefaultRegion(testRegion, "use1")}
	regions[0].NoDefaultVPC = true
	e := newEnv(t, envOptions{regions: regions})
	ctx := context.Background()
	ref, _, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, spec("scarpenter-my-cluster-subnets-us-east-1", aws.KindSubnets, nil))
	if err != nil {
		t.Fatal(err)
	}
	mine, err := e.acct.EC2(testRegion).CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: &ref.ID, AvailabilityZone: new("us-east-1a"), CidrBlock: new("10.240.200.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	err = e.d.Delete(ctx, ref)
	if !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("deleting a network with a subnet someone added: %v, want Retryable (dependency violation)", err)
	}
	subs, _ := e.acct.EC2(testRegion).DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	if len(subs.Subnets) != 1 || *subs.Subnets[0].SubnetId != *mine.Subnet.SubnetId {
		t.Errorf("subnets left: %v; only the foreign one may remain", subs.Subnets)
	}
}

func TestManagedNetworkCanBeRecreatedAndRepairsAMissingRoute(t *testing.T) {
	regions := []fake.RegionSpec{fake.DefaultRegion(testRegion, "use1")}
	regions[0].NoDefaultVPC = true
	e := newEnv(t, envOptions{regions: regions})
	ctx := context.Background()
	sp := spec("scarpenter-my-cluster-subnets-us-east-1", aws.KindSubnets, nil)

	// A CreateRoute that fails after the table exists: the next call adds the route.
	e.acct.Fail(fake.Failure{Op: "CreateRoute", Code: "InternalError"})
	if _, _, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, sp); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("got %v", err)
	}
	first, _, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, sp)
	if err != nil {
		t.Fatal(err)
	}
	rts, _ := e.acct.EC2(testRegion).DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{})
	routes := 0
	for _, rt := range rts.RouteTables {
		routes += len(rt.Routes)
	}
	if routes != 1 || e.acct.Calls("CreateRouteTable") != 1 {
		t.Errorf("%d routes, %d tables created; want the missing route added to the existing table", routes, e.acct.Calls("CreateRouteTable"))
	}

	// The idle GC deletes the network; the next launch creates it again. The token of
	// the route table must not be the old VPC's (EC2 would refuse it for as long as it
	// remembers it).
	if err := e.d.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, created, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, sp)
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("recreating the network: %+v, %v, %v", second, created, err)
	}
}

func TestInUseFollowsShortPages(t *testing.T) {
	// A filtered DescribeInstances may return an empty page with a NextToken; the
	// first page alone would say "not in use" and start the idle clock.
	acct := fake.New(fake.WithShortFilteredPages())
	acct.SetInstanceTypes(smallTypes)
	acct.SetQuota(testRegion, "L-1216C47A", 100)
	e := newEnvOver(t, acct, nil2clock(), envOptions{})
	ctx := context.Background()
	pg, _, err := e.d.Ensure(ctx, aws.KindPlacementGroups, testRegion, spec("scarpenter-my-cluster-placement-groups-us-east-1", aws.KindPlacementGroups, nil))
	if err != nil {
		t.Fatal(err)
	}
	req := request(testRegion, testZone, testType, vendors.CapacityOnDemand)
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{pg}}
	if _, err := e.d.CreateInstance(ctx, req); err != nil {
		t.Fatal(err)
	}
	if inUse, _, err := e.d.InUse(ctx, pg); err != nil || !inUse {
		t.Errorf("InUse = %v, %v; want in use", inUse, err)
	}
}

func TestManagedNetworkReusesAGatewayThatWasNotAttachedAndPicksAFreeCIDR(t *testing.T) {
	regions := []fake.RegionSpec{fake.DefaultRegion(testRegion, "use1")}
	regions[0].NoDefaultVPC = true
	e := newEnv(t, envOptions{regions: regions})
	ctx := context.Background()
	sp := spec("scarpenter-my-cluster-subnets-us-east-1", aws.KindSubnets, nil)
	// The attach fails once: the gateway exists, unattached. The retry must attach
	// that one, not create another.
	e.acct.Fail(fake.Failure{Op: "AttachInternetGateway", Code: "InternalError"})
	if _, _, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, sp); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("got %v", err)
	}
	ref, _, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, sp)
	if err != nil {
		t.Fatal(err)
	}
	if e.acct.Calls("CreateInternetGateway") != 1 {
		t.Errorf("%d gateways created, want 1", e.acct.Calls("CreateInternetGateway"))
	}
	igws, _ := e.acct.EC2(testRegion).DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{})
	if len(igws.InternetGateways) != 1 || len(igws.InternetGateways[0].Attachments) != 1 {
		t.Errorf("gateways %v, want one, attached", igws.InternetGateways)
	}
	// Zone a loses its subnet and a subnet that is not ours sits on the CIDR that zone
	// a would get by its position (10.240.0.0/20) in zone c: the new subnet of zone a
	// takes the first free /20 instead of colliding.
	ec := e.acct.EC2(testRegion)
	subs, _ := ec.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{})
	for _, sn := range subs.Subnets {
		if z := *sn.AvailabilityZone; z == "us-east-1a" || z == "us-east-1c" {
			if _, err := ec.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: sn.SubnetId}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := ec.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: &ref.ID, AvailabilityZone: new("us-east-1c"), CidrBlock: new("10.240.0.0/20")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.d.Ensure(ctx, aws.KindSubnets, testRegion, sp); err != nil {
		t.Fatalf("completing a network whose zone gets another CIDR: %v", err)
	}
}
