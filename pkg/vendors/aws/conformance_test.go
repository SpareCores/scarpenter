/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
)

const conformanceCluster = "conformance"

// drivers maps the drivers the conformance fixture built to their environments,
// so that the hooks (which only get the driver) can reach the fake account.
var (
	driversMu sync.Mutex
	drivers   = map[vendors.Driver]*env{}
)

func envOf(t *testing.T, d vendors.Driver) *env {
	t.Helper()
	driversMu.Lock()
	defer driversMu.Unlock()
	e, ok := drivers[d]
	if !ok {
		t.Fatal("an unknown driver")
	}
	return e
}

// conformanceEnv builds a driver over the complete AWS type list of the committed
// snapshot, with quotas high enough to launch, and warms the instance type table
// the way the quota tracker does at startup.
func conformanceEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, envOptions{types: snapshotTypes(t), cluster: conformanceCluster})
	for _, r := range []string{"us-east-1", "eu-central-1"} {
		for _, class := range []string{"L-1216C47A", "L-34B43A08"} {
			e.acct.SetQuota(r, class, 100000)
		}
	}
	if _, err := e.d.LaunchableTypes(context.Background(), testRegion); err != nil {
		t.Fatal(err)
	}
	register(e)
	return e
}

func register(e *env) {
	driversMu.Lock()
	drivers[e.d] = e
	driversMu.Unlock()
}

// noSpotFamily says whether the plan's table has no Spot quota for the family:
// HPC and High Memory (u) types are On-Demand only. Written from the table, not
// from the driver's rule.
func noSpotFamily(typ string) bool {
	return strings.HasPrefix(typ, "hpc") || strings.HasPrefix(typ, "u-") || strings.HasPrefix(typ, "u7i")
}

func conformanceTypes(t *testing.T) (all, unknown []string) {
	t.Helper()
	for _, r := range awsServers(t) {
		all = append(all, r.APIReference)
		switch {
		case strings.HasPrefix(r.APIReference, "mac"):
			unknown = append(unknown, r.APIReference)
		case noSpotFamily(r.APIReference):
			unknown = append(unknown, r.APIReference+"/"+vendors.CapacitySpot)
		}
	}
	return all, unknown
}

var injected = map[vendors.Class]string{
	vendors.InsufficientCapacity: "InsufficientInstanceCapacity",
	vendors.Unsupported:          "Unsupported",
	vendors.QuotaExceeded:        "VcpuLimitExceeded",
	vendors.ZoneUnusable:         "InsufficientFreeAddressesInSubnet",
	vendors.Retryable:            "InternalError",
	vendors.Fatal:                "UnauthorizedOperation",
}

func conformanceFixture(t *testing.T, kind vendors.ResourceKind, params func(*testing.T, vendors.Driver, string) json.RawMessage) conformance.Fixture {
	all, unknown := conformanceTypes(t)
	return conformance.Fixture{
		New:    func(t *testing.T) vendors.Driver { return conformanceEnv(t).d },
		Region: testRegion, Zone: testZone, InstanceType: testType, MaxUserData: 16 * 1024,
		InstanceTypes: all, UnknownQuotaTypes: unknown,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, req vendors.CreateRequest) {
			t.Helper()
			envOf(t, d).acct.Fail(fake.Failure{
				Op: "RunInstances", Code: injected[class],
				Match: func(in any) bool {
					ri := in.(*ec2.RunInstancesInput)
					return string(ri.InstanceType) == req.InstanceType && deref(ri.Placement.AvailabilityZone) == req.Zone &&
						(ri.InstanceMarketOptions != nil) == (req.CapacityType == vendors.CapacitySpot)
				},
			})
			// The classification matches codes; the injected error message names the
			// type and zone like the service does, for InvalidParameterCombination.
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			envOf(t, d).acct.SetInstanceState(ref.Region(), ref.ID(), ec2types.InstanceStateNameStopped)
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, _ vendors.CreateRequest) {
			t.Helper()
			e := envOf(t, d)
			for _, code := range []string{"L-1216C47A", "L-34B43A08"} {
				e.acct.SetQuota(testRegion, code, 0)
			}
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":    json.RawMessage(`{"data":[{"name":"pgdata","volumeType":"gp3","volumeSize":"100Gi","iops":4000,"mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"local": json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/mnt/local"}}`),
				"big":   json.RawMessage(`{"root":{"volumeType":"io2","volumeSize":"200Gi","iops":70000}}`),
			},
			vendors.ProfilePlacement: {
				"colocated": json.RawMessage(`{"placementGroup":{"strategy":"cluster"}}`),
				"spread":    json.RawMessage(`{"placementGroup":{"strategy":"spread"}}`),
				"dedicated": json.RawMessage(`{"tenancy":"dedicated"}`),
			},
			vendors.ProfileNetwork: {"fast": json.RawMessage(`{"enaExpress":{"enabled":true}}`)},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: "aws", APIReference: "m7i.large", Family: "m7i", VCPUs: 2},
			{Vendor: "aws", APIReference: "c7gn.large", Family: "c7gn", VCPUs: 2, Arch: "arm64"},
			{Vendor: "aws", APIReference: "m6id.large", Family: "m6id", VCPUs: 2, LocalStorageGB: 118, LocalStorageCount: 1},
			{Vendor: "aws", APIReference: "t3.micro", Family: "t3", VCPUs: 2},
		},
		ResourceKind: kind, ResourceParams: params,
		PreCreate: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
			t.Helper()
			return preCreate(t, envOf(t, d), k, region, tags)
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			e := envOf(t, d).restart(t)
			register(e)
			return e.d
		},
	}
}

// preCreate makes a resource the way a user would: with the user's tags, not
// SCarpenter's marker.
func preCreate(t *testing.T, e *env, kind vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
	t.Helper()
	ctx := context.Background()
	cl := e.acct.EC2(region)
	tagList := make([]ec2types.Tag, 0, len(tags))
	for k, v := range tags {
		tagList = append(tagList, ec2types.Tag{Key: &k, Value: &v})
	}
	name := "user-" + string(kind)
	switch kind {
	case aws.KindSubnets:
		vpcs, err := cl.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []ec2types.Filter{{Name: new("isDefault"), Values: []string{"true"}}}})
		if err != nil || len(vpcs.Vpcs) == 0 {
			t.Fatalf("the default VPC: %v", err)
		}
		out, err := cl.CreateSubnet(ctx, &ec2.CreateSubnetInput{
			VpcId: vpcs.Vpcs[0].VpcId, AvailabilityZone: new(testZone), CidrBlock: new("172.31.200.0/24"),
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSubnet, Tags: tagList}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: *out.Subnet.SubnetId}
	case aws.KindPlacementGroups:
		_, err := cl.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
			GroupName: &name, Strategy: ec2types.PlacementStrategyCluster,
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypePlacementGroup, Tags: tagList}},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := cl.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{})
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range out.PlacementGroups {
			if *g.GroupName == name {
				return vendors.ResourceRef{Kind: kind, Region: region, ID: *g.GroupId, Name: name}
			}
		}
	default:
		vpc := defaultVPC(t, e, region)
		out, err := cl.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName: &name, Description: new("user group"), VpcId: &vpc,
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSecurityGroup, Tags: tagList}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: *out.GroupId, Name: name}
	}
	t.Fatal("preCreate: not created")
	return vendors.ResourceRef{}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func defaultVPC(t *testing.T, e *env, region string) string {
	t.Helper()
	out, err := e.acct.EC2(region).DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{{Name: new("isDefault"), Values: []string{"true"}}},
	})
	if err != nil || len(out.Vpcs) == 0 {
		t.Fatalf("the default VPC: %v", err)
	}
	return *out.Vpcs[0].VpcId
}

func sgParams(t *testing.T, d vendors.Driver, region string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(aws.SecurityGroupParams{VPC: defaultVPC(t, envOf(t, d), region), SelfReference: true})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestConformance runs the shared driver suite over the complete AWS type list
// of the committed snapshot (plan 04 review N7), once per resource kind the
// driver creates: the suite's managed-resource subtests take one kind.
func TestConformance(t *testing.T) {
	for _, tc := range []struct {
		kind   vendors.ResourceKind
		params func(*testing.T, vendors.Driver, string) json.RawMessage
	}{
		{kind: aws.KindPlacementGroups},
		{kind: aws.KindSecurityGroups, params: sgParams},
		{kind: aws.KindRunSecurityGroups, params: sgParams},
		{kind: aws.KindSubnets},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			conformance.Run(t, conformanceFixture(t, tc.kind, tc.params))
		})
	}
}
