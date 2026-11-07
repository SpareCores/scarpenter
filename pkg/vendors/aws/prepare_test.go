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
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	awsfake "github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

// engineEnv is the driver behind the real resource engine, which decides where
// each resource comes from (use, the account's default, managed).
type engineEnv struct {
	*env
	engine *resources.Engine
}

func newEngineEnv(t *testing.T, e *env) *engineEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	eng := resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(e.d), Reader: kube, Clock: e.clock, ClusterName: testCluster,
		Regions: func(string) []string { return []string{testRegion} },
		Zones: func(_, region string) []string {
			zs, _ := e.d.Zones(context.Background(), region)
			names := make([]string, 0, len(zs))
			for _, z := range zs {
				names = append(names, z.Name)
			}
			return names
		},
	})
	return &engineEnv{env: e, engine: eng}
}

func classWith(res *awsapi.Resources) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{AWS: &awsapi.Settings{Resources: res}}},
	}
}

func (e *engineEnv) prepare(t *testing.T, nc *v1alpha1.SCNodeClass, region, zone, typ string, s vendors.VendorSettings, runKey string,
) (*vendors.Prepared, error) {
	t.Helper()
	return e.d.Prepare(context.Background(), vendors.PrepareRequest{
		Region: region, Zone: zone, InstanceType: typ, CapacityType: vendors.CapacityOnDemand,
		Type:     vendors.CatalogType{Vendor: "aws", APIReference: typ},
		Settings: s, RunKey: runKey,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: testCluster, vendors.TagNodeClaim: "claim", vendors.TagNodeClass: nc.Name, vendors.TagManagedBy: vendors.ManagedByValue,
		},
		Resources: e.engine.Provider(nc, "aws", region, zone),
	})
}

func defaultNetworking() common.Networking {
	return options.ResolveNetworking(common.Networking{}, nil)
}

func ids(p *vendors.Prepared, kind vendors.ResourceKind) []string {
	var out []string
	for _, r := range p.Resources {
		if r.Kind == kind {
			out = append(out, r.ID)
		}
	}
	return out
}

func warmTypes(t *testing.T, e *env) {
	t.Helper()
	if _, err := e.d.LaunchableTypes(context.Background(), testRegion); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareUsesTheDefaultVPCAsFoundAndCreatesTheGroup(t *testing.T) {
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	nc := classWith(nil)
	s := resolveSettings(t, e.d, "", defaultNetworking(), nil, options.Selection{})
	p, err := e.prepare(t, nc, testRegion, "us-east-1b", testType, s, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p, aws.KindSubnets); len(got) != 1 || got[0] != subnetOf(e.env, testRegion, "us-east-1b") {
		t.Errorf("subnets %v, want the default subnet of the zone", got)
	}
	sgs := ids(p, aws.KindSecurityGroups)
	if len(sgs) != 1 {
		t.Fatalf("security groups %v, want the NodeClass's group", sgs)
	}
	// The default VPC is used as found: nothing was created in it but the group.
	for _, op := range []string{"CreateVpc", "CreateSubnet", "CreateInternetGateway", "CreateRouteTable"} {
		if n := e.acct.Calls(op); n != 0 {
			t.Errorf("%s was called %d times for a region with a default VPC", op, n)
		}
	}
	// Ownership marker in the creating call, deterministic name with the prefix.
	in := e.acct.Inputs("CreateSecurityGroup")[0].(*ec2.CreateSecurityGroupInput)
	if !strings.HasPrefix(deref(in.GroupName), "scarpenter-my-cluster-security-groups-us-east-1-") || len(deref(in.GroupName)) > 63 {
		t.Errorf("group name %q", deref(in.GroupName))
	}
	tags := map[string]string{}
	for _, tg := range in.TagSpecifications[0].Tags {
		tags[deref(tg.Key)] = deref(tg.Value)
	}
	for k, v := range map[string]string{
		"scarpenter.sparecores.com/cluster": testCluster, "scarpenter.sparecores.com/managed-by": "scarpenter",
		"scarpenter.sparecores.com/nodeclass": "class", "scarpenter.sparecores.com/resource-kind": "securityGroups", "Name": deref(in.GroupName),
	} {
		if tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, tags[k], v)
		}
	}
	// No inbound rules; isolation run without a run key is one implicit run: the
	// group's members reach each other.
	if rules := e.acct.SecurityGroupRules(testRegion, sgs[0]); len(rules) != 1 || rules[0] != "-1:0-0:"+sgs[0] {
		t.Errorf("rules %v, want only the self reference", rules)
	}
	// Idempotent: the second Prepare finds the group.
	p2, err := e.prepare(t, nc, testRegion, "us-east-1b", testType, s, "")
	if err != nil || !slices.Equal(ids(p2, aws.KindSecurityGroups), sgs) || e.acct.Calls("CreateSecurityGroup") != 1 {
		t.Errorf("second Prepare = %v, %v (creates: %d)", ids(p2, aws.KindSecurityGroups), err, e.acct.Calls("CreateSecurityGroup"))
	}
}

func TestPrepareSecurityGroupRulesFollowNetworking(t *testing.T) {
	nc := classWith(nil)
	inbound := &common.Inbound{Rules: []common.InboundRule{
		{Protocol: "tcp", FromPort: new(int32(22)), CIDRs: []string{"203.0.113.0/24", "2001:db8::/32"}},
		{Protocol: "udp", FromPort: new(int32(51820)), ToPort: new(int32(51830)), CIDRs: []string{"0.0.0.0/0"}},
		{Protocol: "icmp", CIDRs: []string{"10.0.0.0/8", "2001:db8:1::/48"}},
		{Protocol: "any", CIDRs: []string{"192.0.2.1/32"}},
		{Protocol: "tcp", CIDRs: []string{"198.51.100.0/24"}},
	}}
	for _, tc := range []struct {
		name      string
		isolation common.Isolation
		runKey    string
		wantSelf  bool
		wantRun   bool
	}{
		{"none", common.IsolationNone, "run-a", false, false},
		{"nodeclass", common.IsolationNodeClass, "run-a", true, false},
		{"run", common.IsolationRun, "run-a", false, true},
		{"run without a run key", common.IsolationRun, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngineEnv(t, newEnv(t, envOptions{}))
			net := common.Networking{Isolation: tc.isolation, Inbound: inbound}
			s := resolveSettings(t, e.d, "", net, nil, options.Selection{})
			p, err := e.prepare(t, nc, testRegion, testZone, testType, s, tc.runKey)
			if err != nil {
				t.Fatal(err)
			}
			sgs, run := ids(p, aws.KindSecurityGroups), ids(p, aws.KindRunSecurityGroups)
			if len(sgs) != 1 || (len(run) == 1) != tc.wantRun {
				t.Fatalf("groups %v and run groups %v", sgs, run)
			}
			rules := e.acct.SecurityGroupRules(testRegion, sgs[0])
			want := []string{
				"-1:0-0:192.0.2.1/32", "58:-1--1:2001:db8:1::/48", "icmp:-1--1:10.0.0.0/8", "tcp:0-65535:198.51.100.0/24", "tcp:22-22:2001:db8::/32",
				"tcp:22-22:203.0.113.0/24", "udp:51820-51830:0.0.0.0/0",
			}
			if tc.wantSelf {
				want = append(want, "-1:0-0:"+sgs[0])
			}
			slices.Sort(want)
			if !slices.Equal(rules, want) {
				t.Errorf("rules %v, want %v", rules, want)
			}
			if tc.wantRun {
				if r := e.acct.SecurityGroupRules(testRegion, run[0]); len(r) != 1 || r[0] != "-1:0-0:"+run[0] {
					t.Errorf("the run group's rules %v, want only the self reference", r)
				}
			}
		})
	}
}

func TestPrepareARunGroupIsPerRunKey(t *testing.T) {
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	nc := classWith(nil)
	s := resolveSettings(t, e.d, "", defaultNetworking(), nil, options.Selection{})
	a, _ := e.prepare(t, nc, testRegion, testZone, testType, s, "run-a")
	b, _ := e.prepare(t, nc, testRegion, testZone, testType, s, "run-b")
	a2, _ := e.prepare(t, nc, testRegion, testZone, testType, s, "run-a")
	if ids(a, aws.KindRunSecurityGroups)[0] == ids(b, aws.KindRunSecurityGroups)[0] || ids(a, aws.KindRunSecurityGroups)[0] != ids(a2, aws.KindRunSecurityGroups)[0] {
		t.Errorf("run groups %v %v %v", ids(a, aws.KindRunSecurityGroups), ids(b, aws.KindRunSecurityGroups), ids(a2, aws.KindRunSecurityGroups))
	}
	if ids(a, aws.KindSecurityGroups)[0] != ids(b, aws.KindSecurityGroups)[0] {
		t.Error("the NodeClass's group is not shared by the runs")
	}
}

func TestPrepareUseSubnetsByIDChoosesTheZonesSubnetWithTheMostFreeAddresses(t *testing.T) {
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	vpc := defaultVPC(t, e.env, testRegion)
	ec := e.acct.EC2(testRegion)
	mk := func(zone, cidr string) string {
		out, err := ec.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{VpcId: &vpc, AvailabilityZone: &zone, CidrBlock: &cidr})
		if err != nil {
			t.Fatal(err)
		}
		return *out.Subnet.SubnetId
	}
	a1, a2, b1 := mk("us-east-1a", "172.31.201.0/24"), mk("us-east-1a", "172.31.202.0/24"), mk("us-east-1b", "172.31.203.0/24")
	e.acct.SetFreeAddresses(testRegion, a1, 10)
	e.acct.SetFreeAddresses(testRegion, a2, 200)
	nc := classWith(&awsapi.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{IDs: map[string][]string{testRegion: {a1, a2, b1}}}}})
	s := resolveSettings(t, e.d, "", defaultNetworking(), nil, options.Selection{})
	p, err := e.prepare(t, nc, testRegion, "us-east-1a", testType, s, "")
	if err != nil || ids(p, aws.KindSubnets)[0] != a2 {
		t.Errorf("zone a: %v, %v; want the subnet with the most free addresses %s", ids(p, aws.KindSubnets), err, a2)
	}
	p, err = e.prepare(t, nc, testRegion, "us-east-1b", testType, s, "")
	if err != nil || ids(p, aws.KindSubnets)[0] != b1 {
		t.Errorf("zone b: %v, %v", ids(p, aws.KindSubnets), err)
	}
	// A zone without one of the subnets is unusable on its own.
	if _, err := e.prepare(t, nc, testRegion, "us-east-1c", testType, s, ""); !vendors.IsClass(err, vendors.ZoneUnusable) || vendors.CodeOf(err) != "NoSubnetInZone" {
		t.Errorf("zone c: %v, want ZoneUnusable NoSubnetInZone", err)
	}
}

func TestPrepareZoneNamePatternGatesOnlyTheZoneWithoutAMatch(t *testing.T) {
	// A `use` name pattern with {zone}: a zone without a match is gated on its
	// own (plan 06), and the code reaches the attempt loop unchanged so that
	// Create skips just that zone and not the region.
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	vpc := defaultVPC(t, e.env, testRegion)
	ec := e.acct.EC2(testRegion)
	for i, zone := range []string{"us-east-1a", "us-east-1b"} {
		cidr := "172.31.20" + string(rune('1'+i)) + ".0/24"
		_, err := ec.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{VpcId: &vpc, AvailabilityZone: &zone, CidrBlock: &cidr,
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSubnet,
				Tags: []ec2types.Tag{{Key: new("Name"), Value: new("nodes-" + zone)}}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	nc := classWith(&awsapi.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{Name: "nodes-{zone}"}}})
	s := resolveSettings(t, e.d, "", defaultNetworking(), nil, options.Selection{})
	for _, zone := range []string{"us-east-1a", "us-east-1b"} {
		if _, err := e.prepare(t, nc, testRegion, zone, testType, s, ""); err != nil {
			t.Errorf("zone %s: %v", zone, err)
		}
	}
	_, err := e.prepare(t, nc, testRegion, "us-east-1c", testType, s, "")
	if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Errorf("zone c: %v, want Unsupported %s", err, vendors.CodeResourceMissingInZone)
	}
	// The zone stays gated on the engine's side, the region is not.
	if st, ok := e.engine.RegionState("class", "aws", testRegion); !ok || !st.Ready || st.Zones["us-east-1c"] == "" {
		t.Errorf("region state %+v, %v; want the region ready and zone c gated", st, ok)
	}
}

func TestPrepareReturnsTheProvidersCodesUnchanged(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, code := range []string{vendors.CodeResourceMissingInZone, vendors.CodeResourceMissing, vendors.CodeResourceForbidden} {
		p := failingProvider{err: vendors.NewError(vendors.Unsupported, code, context.DeadlineExceeded)}
		s := resolveSettings(t, e.d, "", defaultNetworking(), nil, options.Selection{})
		_, err := e.d.Prepare(context.Background(), vendors.PrepareRequest{
			Region: testRegion, Zone: testZone, InstanceType: testType, Settings: s, Resources: p,
		})
		if vendors.CodeOf(err) != code || !vendors.IsClass(err, vendors.Unsupported) {
			t.Errorf("a provider error %s came out as %v", code, err)
		}
	}
}

type failingProvider struct{ err error }

func (f failingProvider) Require(context.Context, vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	return nil, f.err
}

func TestPrepareSecurityGroupOfAnotherVPCGatesTheZone(t *testing.T) {
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	other, err := e.acct.EC2(testRegion).CreateVpc(context.Background(), &ec2.CreateVpcInput{CidrBlock: new("10.9.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.acct.EC2(testRegion).CreateSecurityGroup(context.Background(), &ec2.CreateSecurityGroupInput{
		GroupName: new("elsewhere"), Description: new("x"), VpcId: other.Vpc.VpcId})
	if err != nil {
		t.Fatal(err)
	}
	nc := classWith(&awsapi.Resources{SecurityGroups: &common.Resource{Use: &common.ResourceUse{IDs: map[string][]string{testRegion: {*g.GroupId}}}}})
	s := resolveSettings(t, e.d, "", defaultNetworking(), nil, options.Selection{})
	_, err = e.prepare(t, nc, testRegion, testZone, testType, s, "")
	if vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Errorf("a group in another VPC: %v, want %s", err, vendors.CodeResourceMissingInZone)
	}
}

func TestPrepareCreatesAManagedNetworkWhereThereIsNoDefaultVPC(t *testing.T) {
	regions := []awsfake.RegionSpec{awsfake.DefaultRegion(testRegion, "use1")}
	regions[0].NoDefaultVPC = true
	e := newEngineEnv(t, newEnv(t, envOptions{regions: regions}))
	nc := classWith(nil)
	s := resolveSettings(t, e.d, "", defaultNetworking(), nil, options.Selection{})
	p, err := e.prepare(t, nc, testRegion, "us-east-1b", testType, s, "")
	if err != nil {
		t.Fatal(err)
	}
	if e.acct.Calls("CreateVpc") != 1 || e.acct.Calls("CreateSubnet") != 3 || e.acct.Calls("CreateInternetGateway") != 1 ||
		e.acct.Calls("AttachInternetGateway") != 1 || e.acct.Calls("CreateRouteTable") != 1 || e.acct.Calls("CreateRoute") != 1 ||
		e.acct.Calls("AssociateRouteTable") != 3 {
		t.Errorf("creations: vpc %d, subnets %d, igw %d, attach %d, rt %d, route %d, assoc %d", e.acct.Calls("CreateVpc"),
			e.acct.Calls("CreateSubnet"), e.acct.Calls("CreateInternetGateway"), e.acct.Calls("AttachInternetGateway"),
			e.acct.Calls("CreateRouteTable"), e.acct.Calls("CreateRoute"), e.acct.Calls("AssociateRouteTable"))
	}
	// Every part carries the ownership marker from its creating call.
	for _, in := range e.acct.Inputs("CreateSubnet") {
		if len(in.(*ec2.CreateSubnetInput).TagSpecifications) == 0 {
			t.Error("a subnet is created without tags")
		}
	}
	// The launch got the subnet of its zone, in the managed VPC, and a group in it.
	sn := ids(p, aws.KindSubnets)
	if len(sn) != 1 {
		t.Fatalf("subnets %v", sn)
	}
	got, _ := e.acct.EC2(testRegion).DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: new("subnet-id"), Values: sn}}})
	if deref(got.Subnets[0].AvailabilityZone) != "us-east-1b" {
		t.Errorf("subnet in %s", deref(got.Subnets[0].AvailabilityZone))
	}
	// A second Prepare, and one of another zone, create nothing more.
	if _, err := e.prepare(t, nc, testRegion, "us-east-1c", testType, s, ""); err != nil {
		t.Fatal(err)
	}
	if e.acct.Calls("CreateVpc") != 1 {
		t.Errorf("%d VPCs, want one per region", e.acct.Calls("CreateVpc"))
	}
}

func TestForbidWithoutDefaultsIsForbidden(t *testing.T) {
	regions := []awsfake.RegionSpec{awsfake.DefaultRegion(testRegion, "use1")}
	regions[0].NoDefaultVPC = true
	e := newEngineEnv(t, newEnv(t, envOptions{regions: regions}))
	nc := classWith(&awsapi.Resources{Subnets: &common.Resource{Forbid: true}})
	s := resolveSettings(t, e.d, "", defaultNetworking(), nil, options.Selection{})
	_, err := e.prepare(t, nc, testRegion, testZone, testType, s, "")
	if vendors.CodeOf(err) != vendors.CodeResourceForbidden {
		t.Errorf("got %v, want %s", err, vendors.CodeResourceForbidden)
	}
	if e.acct.Calls("CreateVpc") != 0 {
		t.Error("a forbidden VPC was created")
	}
}

func TestPreparePlacementGroups(t *testing.T) {
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	warmTypes(t, e.env)
	nc := classWith(nil)
	profiles := vendors.Profiles{vendors.ProfilePlacement: {
		"cluster":   json.RawMessage(`{"placementGroup":{"strategy":"cluster"}}`),
		"spread":    json.RawMessage(`{"placementGroup":{"strategy":"spread"}}`),
		"partition": json.RawMessage(`{"placementGroup":{"strategy":"partition","partitionCount":4}}`),
		"existing":  json.RawMessage(`{"placementGroup":{"use":{"name":"my-group"}}}`),
	}}
	for name, strategy := range map[string]string{"cluster": "cluster", "spread": "spread", "partition": "partition"} {
		s := resolveSettings(t, e.d, "", defaultNetworking(), profiles, options.Selection{Placement: name})
		if _, err := e.prepare(t, nc, testRegion, testZone, testType, s, ""); vendors.CodeOf(err) != "InvalidSettings" {
			t.Errorf("%s without a run key: %v, want an error asking for the run key", name, err)
		}
		p, err := e.prepare(t, nc, testRegion, testZone, testType, s, "run-1")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		pgs := ids(p, aws.KindPlacementGroups)
		if len(pgs) != 1 {
			t.Fatalf("%s: placement groups %v", name, pgs)
		}
		out, _ := e.acct.EC2(testRegion).DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{})
		found := false
		for _, g := range out.PlacementGroups {
			if deref(g.GroupId) == pgs[0] {
				found = true
				if string(g.Strategy) != strategy || (strategy == "partition" && deref(g.PartitionCount) != 4) {
					t.Errorf("%s: group %+v", name, g)
				}
				if !strings.HasPrefix(deref(g.GroupName), "scarpenter-my-cluster-placement-groups-us-east-1-") {
					t.Errorf("%s: name %q", name, deref(g.GroupName))
				}
			}
		}
		if !found {
			t.Errorf("%s: the group is not in the account", name)
		}
		// The same run key, the same group; another run key, another.
		p2, _ := e.prepare(t, nc, testRegion, testZone, testType, s, "run-1")
		p3, _ := e.prepare(t, nc, testRegion, testZone, testType, s, "run-2")
		if ids(p2, aws.KindPlacementGroups)[0] != pgs[0] || ids(p3, aws.KindPlacementGroups)[0] == pgs[0] {
			t.Errorf("%s: groups %v %v %v", name, pgs, ids(p2, aws.KindPlacementGroups), ids(p3, aws.KindPlacementGroups))
		}
	}
	// A group by name (use) is referenced, never created.
	_, err := e.acct.EC2(testRegion).CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: new("my-group"), Strategy: ec2types.PlacementStrategyCluster})
	if err != nil {
		t.Fatal(err)
	}
	before := e.acct.Calls("CreatePlacementGroup")
	s := resolveSettings(t, e.d, "", defaultNetworking(), profiles, options.Selection{Placement: "existing"})
	p, err := e.prepare(t, nc, testRegion, testZone, testType, s, "")
	if err != nil || e.acct.Calls("CreatePlacementGroup") != before || len(ids(p, aws.KindPlacementGroups)) != 1 {
		t.Errorf("use by name: %v, %v (creates %d)", ids(p, aws.KindPlacementGroups), err, e.acct.Calls("CreatePlacementGroup")-before)
	}
	for _, r := range p.Resources {
		if r.Kind == aws.KindPlacementGroups && r.Name != "my-group" {
			t.Errorf("the referenced group is %q", r.Name)
		}
	}
	s = resolveSettings(t, e.d, "", defaultNetworking(), vendors.Profiles{vendors.ProfilePlacement: {"missing": json.RawMessage(`{"placementGroup":{"use":{"name":"absent"}}}`)}},
		options.Selection{Placement: "missing"})
	if _, err := e.prepare(t, nc, testRegion, testZone, testType, s, ""); vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Errorf("a group that does not exist: %v", err)
	}
}

func TestSpreadPlacementGroupSizeLimit(t *testing.T) {
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	warmTypes(t, e.env)
	nc := classWith(nil)
	profiles := vendors.Profiles{vendors.ProfilePlacement: {"spread": json.RawMessage(`{"placementGroup":{"strategy":"spread"}}`)}}
	s := resolveSettings(t, e.d, "", defaultNetworking(), profiles, options.Selection{Placement: "spread"})
	ctx := context.Background()
	var pgName string
	for i := range 7 {
		p, err := e.prepare(t, nc, testRegion, testZone, testType, s, "run")
		if err != nil {
			t.Fatalf("member %d: %v", i+1, err)
		}
		for _, r := range p.Resources {
			if r.Kind == aws.KindPlacementGroups {
				pgName = r.Name
			}
		}
		req := request(testRegion, testZone, testType, vendors.CapacityOnDemand)
		req.Settings, req.Prepared = s, p
		if _, err := e.d.CreateInstance(ctx, req); err != nil {
			t.Fatalf("member %d: %v", i+1, err)
		}
	}
	calls := e.acct.Calls("RunInstances")
	_, err := e.prepare(t, nc, testRegion, testZone, testType, s, "run")
	if !vendors.IsClass(err, vendors.InsufficientCapacity) || vendors.CodeOf(err) != "PlacementGroupFull" {
		t.Errorf("the eighth member in the zone: %v, want InsufficientCapacity PlacementGroupFull", err)
	}
	if e.acct.Calls("RunInstances") != calls {
		t.Error("the full group reached EC2")
	}
	// Another zone is a separate set of seven.
	if _, err := e.prepare(t, nc, testRegion, "us-east-1b", testType, s, "run"); err != nil {
		t.Errorf("another zone of the group: %v", err)
	}
	_ = pgName
}

func TestPrepareDeviceHints(t *testing.T) {
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	warmTypes(t, e.env)
	nc := classWith(nil)
	profiles := vendors.Profiles{vendors.ProfileStorage: {
		"db":    json.RawMessage(`{"data":[{"name":"pgdata","volumeType":"gp3","volumeSize":"100Gi","mountPath":"/var/lib/pgdata","filesystem":"xfs"},{"name":"scratch","volumeType":"gp3","volumeSize":"50Gi","mountPath":"/scratch"}]}`),
		"local": json.RawMessage(`{"localStorage":{"policy":"raid0","mountPath":"/mnt/local"}}`),
		"both":  json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/mnt/l"},"data":[{"name":"d","volumeType":"gp2","volumeSize":"10Gi","mountPath":"/d"}]}`),
		"skip":  json.RawMessage(`{"localStorage":{"policy":"ignore"}}`),
	}}
	s := resolveSettings(t, e.d, "", defaultNetworking(), profiles, options.Selection{Storage: "db"})
	p, err := e.prepare(t, nc, testRegion, testZone, testType, s, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []vendors.DeviceHint{
		{Name: "pgdata", Filesystem: "xfs", MountPath: "/var/lib/pgdata", SerialPattern: "vol*", SizeBytes: 100 << 30},
		{Name: "scratch", Filesystem: "ext4", MountPath: "/scratch", SerialPattern: "vol*", SizeBytes: 50 << 30},
	}
	if !slices.Equal(p.Devices, want) || p.Local != nil {
		t.Errorf("devices %+v local %+v, want %+v", p.Devices, p.Local, want)
	}
	s = resolveSettings(t, e.d, "", defaultNetworking(), profiles, options.Selection{Storage: "local"})
	p, err = e.prepare(t, nc, testRegion, testZone, "m6id.large", s, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Local == nil || *p.Local != (vendors.LocalHint{Policy: "raid0", MountPath: "/mnt/local", DevicePattern: "nvme-Amazon_EC2_NVMe_Instance_Storage_*", Count: 1}) || len(p.Devices) != 0 {
		t.Errorf("local hint %+v devices %+v", p.Local, p.Devices)
	}
	s = resolveSettings(t, e.d, "", defaultNetworking(), profiles, options.Selection{Storage: "skip"})
	if p, _ = e.prepare(t, nc, testRegion, testZone, "m6id.large", s, ""); p.Local != nil {
		t.Errorf("policy ignore needs no hint: %+v", p.Local)
	}
}

func TestAChangedStrategyIsAnotherGroup(t *testing.T) {
	// The create parameters are part of a group's identity: a profile whose
	// strategy changed must not adopt the group of the old one; the old group is
	// collected by its lifecycle (perUse).
	e := newEngineEnv(t, newEnv(t, envOptions{}))
	warmTypes(t, e.env)
	nc := classWith(nil)
	group := func(strategy string) string {
		profiles := vendors.Profiles{vendors.ProfilePlacement: {"pg": json.RawMessage(`{"placementGroup":{"strategy":"` + strategy + `"}}`)}}
		s := resolveSettings(t, e.d, "", defaultNetworking(), profiles, options.Selection{Placement: "pg"})
		p, err := e.prepare(t, nc, testRegion, testZone, testType, s, "run")
		if err != nil {
			t.Fatal(err)
		}
		return ids(p, aws.KindPlacementGroups)[0]
	}
	a, b, again := group("cluster"), group("spread"), group("cluster")
	if a == b || a != again {
		t.Errorf("groups %s (cluster), %s (spread), %s (cluster again)", a, b, again)
	}
}
