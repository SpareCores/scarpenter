/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	vpc "github.com/alibabacloud-go/vpc-20160428/v7/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	api "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/alicloud"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
)

// driversMu guards drivers, which maps the drivers the conformance fixture built to
// their environments, so that the hooks (which only get the driver) can reach the
// fake account.
var (
	driversMu sync.Mutex
	drivers   = map[vendors.Driver]*denv{}
)

func envOf(t *testing.T, d vendors.Driver) *denv {
	t.Helper()
	driversMu.Lock()
	defer driversMu.Unlock()
	e, ok := drivers[d]
	if !ok {
		t.Fatal("an unknown driver")
	}
	return e
}

func register(e *denv) {
	driversMu.Lock()
	drivers[e.d] = e
	driversMu.Unlock()
}

// conformanceEnv builds a driver over the complete type list of the committed
// snapshot, with quotas high enough to launch, and warms the instance type table
// the way the quota tracker does at startup.
func conformanceEnv(t *testing.T) *denv {
	t.Helper()
	e := newDenv(t, denvOptions{types: snapshotTypes(t), cluster: "conformance"})
	e.warm(testRegion)
	register(e)
	return e
}

// droppedFamily says whether the plan's table drops the type: bare metal, the
// Super Computing Cluster, the test families and the classic-network-only legacy
// families c1 and s1. Written from the plan, not from the driver's rule.
func droppedFamily(typ string) bool {
	parts := strings.Split(typ, ".")
	if len(parts) < 3 {
		return true
	}
	fam := parts[1]
	for _, p := range []string{"ebm", "scc", "poc-test", "e-test", "vfx", "hbm"} {
		if strings.HasPrefix(fam, p) {
			return true
		}
	}
	return fam == "c1" || fam == "s1"
}

var injected = map[vendors.Class]struct {
	code   string
	status int
}{
	vendors.InsufficientCapacity: {"OperationDenied.NoStock", 400},
	vendors.Unsupported:          {"InvalidInstanceType.ZoneNotSupported", 400},
	vendors.QuotaExceeded:        {"QuotaExceed.ElasticQuota", 400},
	vendors.ZoneUnusable:         {"QuotaExceeded.PrivateIpAddress", 400},
	vendors.Retryable:            {"InternalError", 500},
	vendors.Fatal:                {"Forbidden.RAM", 403},
}

var seedCIDR, seedID atomic.Int32

// conformanceFixture wires the suite to the driver for one resource kind: the
// selector, image and resolution-order subtests run once, in the subnets run.
func conformanceFixture(t *testing.T, kind vendors.ResourceKind, params func(*testing.T, vendors.Driver, string) json.RawMessage) conformance.Fixture {
	fx := conformanceFixture0(t, kind, params)
	switch kind {
	case KindSubnets:
	case KindDeploymentSets:
		// A deployment set takes no tags: nothing to seed by tag.
		fx.Seed, fx.SeedImage, fx.PreCreate = nil, nil, nil
		fx.OrderClass, fx.SetDefault, fx.FailCreate = nil, nil, nil
	default:
		fx.Seed, fx.SeedImage = nil, nil // the selector cases cover every kind once, in the subnets run
		fx.OrderClass, fx.SetDefault, fx.FailCreate = nil, nil, nil
	}
	return fx
}

func conformanceFixture0(t *testing.T, kind vendors.ResourceKind, params func(*testing.T, vendors.Driver, string) json.RawMessage) conformance.Fixture {
	rows := alicloudServers(t)
	all, unknown := make([]string, 0, len(rows)), []string{}
	for _, r := range rows {
		all = append(all, r.APIReference)
		if droppedFamily(r.APIReference) {
			unknown = append(unknown, r.APIReference)
		}
	}
	return conformance.Fixture{
		New:    func(t *testing.T) vendors.Driver { return conformanceEnv(t).d },
		Region: testRegion, Zone: testZoneA, InstanceType: testType, Arch: archARM64, MaxUserData: MaxUserData,
		InstanceTypes: all, UnknownQuotaTypes: unknown,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, req vendors.CreateRequest) {
			t.Helper()
			inj := injected[class]
			times := 1
			if class == vendors.Retryable {
				times = maxAttempts // the driver retries a transient error before it gives up
			}
			envOf(t, d).acct.Inject("RunInstances", fake.APIError(inj.code, inj.status, "injected"), times)
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			envOf(t, d).acct.Stop(ref.Region(), ref.ID())
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, _ vendors.CreateRequest) {
			t.Helper()
			envOf(t, d).acct.SetQuota(testRegion, 0, 0)
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":    json.RawMessage(`{"data":[{"name":"pgdata","category":"cloud_essd","performanceLevel":"PL1","size":"100Gi","mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"auto":  json.RawMessage(`{"root":{"category":"cloud_auto","size":"100Gi","provisionedIops":3000}}`),
				"entry": json.RawMessage(`{"root":{"category":"cloud_essd_entry","size":"40Gi"}}`),
				"enc":   json.RawMessage(`{"root":{"encrypted":true}}`),
				"local": json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/mnt/local"}}`),
			},
			vendors.ProfilePlacement: {
				"colocated": json.RawMessage(`{"deploymentSet":{"strategy":"LowLatency"}}`),
				"spread":    json.RawMessage(`{"deploymentSet":{"strategy":"Availability"}}`),
				"named":     json.RawMessage(`{"deploymentSet":{"use":{"name":"my-set"}}}`),
			},
			vendors.ProfileNetwork: {
				"slow":   json.RawMessage(`{"internetMaxBandwidthOut":10}`),
				"billed": json.RawMessage(`{"internetChargeType":"PayByBandwidth"}`),
			},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: Vendor, APIReference: "ecs.g8y.large", Family: "ecs.g8y", VCPUs: 2, Arch: "arm64"},
			{Vendor: Vendor, APIReference: "ecs.g7.large", Family: "ecs.g7", VCPUs: 2},
			{Vendor: Vendor, APIReference: "ecs.i4.xlarge", Family: "ecs.i4", VCPUs: 4, LocalStorageGB: 1788, LocalStorageCount: 1},
			{Vendor: Vendor, APIReference: "ecs.e-c1m2.large", Family: "ecs.e-c1m2", VCPUs: 2},
		},
		ResourceKind: kind, ResourceParams: params,
		Seed: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
			t.Helper()
			return seed(t, envOf(t, d), k, region, spec)
		},
		SeedImage: seedImage,
		SeedKinds: []vendors.ResourceKind{KindSubnets, KindSecurityGroups},
		SeedAttrs: []string{"created", "description", "cidr"},
		SeedCIDRs: [2]string{"172.20.201.0/24", "172.20.202.0/24"}, SeedWithin: "172.16.0.0/12",
		PreCreate: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
			t.Helper()
			return preCreate(t, envOf(t, d), k, region, tags)
		},
		OrderClass: func(t *testing.T, _ vendors.Driver, res map[vendors.ResourceKind]*common.Resource, ipv6 bool) *v1alpha1.SCNodeClass {
			t.Helper()
			nc := classWith(&api.Resources{Subnets: res[KindSubnets], SecurityGroups: res[KindSecurityGroups]})
			nc.Spec.Networking.PublicIPv6 = &ipv6
			return nc
		},
		SetDefault: func(t *testing.T, d vendors.Driver, kind vendors.ResourceKind, region string, state conformance.DefaultState) {
			t.Helper()
			// The default VPC of the fake account has no IPv6 range, like a real one:
			// only its absence is arranged.
			if state == conformance.DefaultAbsent && kind == KindSubnets {
				envOf(t, d).acct.RemoveDefaultNetwork(region)
			}
		},
		FailCreate: func(t *testing.T, d vendors.Driver, kind vendors.ResourceKind) {
			t.Helper()
			if kind == KindSubnets {
				// The VPC exists when the creation of its first vSwitch fails (a
				// refusal, not a transient error that the driver would retry).
				envOf(t, d).acct.Inject("CreateVSwitch", fake.APIError("Forbidden.RAM", 403, "injected"), 1)
			}
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			e := envOf(t, d).restart()
			register(e)
			return e.d
		},
	}
}

func classWith(res *api.Resources) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{Alicloud: &api.Settings{Resources: res}}},
	}
}

// seedImage makes an image the way a user would: in the owner's scope (a selector
// without match.owner finds it), with the release in the tags flatcar-channel and
// flatcar-version when it says one.
func seedImage(t *testing.T, d vendors.Driver, region string, spec conformance.ImageSpec) string {
	t.Helper()
	tags := map[string]string{}
	maps.Copy(tags, spec.Tags)
	if spec.Version != "" {
		tags[tagFlatcarChannel], tags[tagFlatcarVersion] = spec.Channel, spec.Version
	}
	arch := "x86_64"
	if spec.Arch == archARM64 {
		arch = "arm64"
	}
	id := fmt.Sprintf("m-conf%08d", seedID.Add(1))
	envOf(t, d).acct.AddImage(region, fake.ImageSpec{ID: id, Name: "conformance-" + id, Arch: arch, Tags: tags, Created: spec.Created})
	return id
}

// seed makes a resource the way a user would, with the name, tags and the
// attributes of the spec that ECS has (description of a group, range of a
// vSwitch, creation time).
func seed(t *testing.T, e *denv, kind vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
	t.Helper()
	ctx := context.Background()
	var id string
	switch kind {
	case KindSubnets:
		cidr := spec.CIDR
		if cidr == "" {
			cidr = fmt.Sprintf("172.20.%d.0/24", 10+seedCIDR.Add(1))
		}
		in := &vpc.CreateVSwitchRequest{
			RegionId: &region, VpcId: new(e.vpc[region]), ZoneId: new(testZoneA), CidrBlock: &cidr, VSwitchName: &spec.Name,
		}
		for k, v := range spec.Tags {
			in.Tag = append(in.Tag, &vpc.CreateVSwitchRequestTag{Key: new(k), Value: new(v)})
		}
		out, err := e.acct.VPC(region).CreateVSwitch(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		id = *out.VSwitchId
	default:
		desc := spec.Description
		if desc == "" {
			desc = "user group"
		}
		in := &ecs.CreateSecurityGroupRequest{
			RegionId: &region, VpcId: new(e.vpc[region]), SecurityGroupName: &spec.Name, Description: &desc,
		}
		for k, v := range spec.Tags {
			in.Tag = append(in.Tag, &ecs.CreateSecurityGroupRequestTag{Key: new(k), Value: new(v)})
		}
		out, err := e.acct.ECS(region).CreateSecurityGroup(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		id = *out.SecurityGroupId
	}
	if !spec.Created.IsZero() {
		e.acct.SetCreated(region, id, spec.Created)
	}
	return vendors.ResourceRef{Kind: kind, Region: region, ID: id, Name: spec.Name}
}

// preCreate makes a resource the way a user would: with the user's tags, not
// SCarpenter's marker.
func preCreate(t *testing.T, e *denv, kind vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
	t.Helper()
	return seed(t, e, kind, region, conformance.SeedSpec{Name: "user-" + string(kind), Tags: tags,
		CIDR: "172.20.200.0/24"})
}

func sgParams(t *testing.T, d vendors.Driver, region string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(SecurityGroupParams{VPC: envOf(t, d).vpc[region], SelfReference: true})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestConformance runs the shared driver suite over the complete alicloud type list
// of the committed snapshot, once per resource kind the driver creates: the suite's
// managed-resource subtests take one kind.
func TestConformance(t *testing.T) {
	for _, tc := range []struct {
		kind   vendors.ResourceKind
		params func(*testing.T, vendors.Driver, string) json.RawMessage
	}{
		{kind: KindDeploymentSets},
		{kind: KindSecurityGroups, params: sgParams},
		{kind: KindRunSecurityGroups, params: sgParams},
		{kind: KindSubnets},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			conformance.Run(t, conformanceFixture(t, tc.kind, tc.params))
		})
	}
}
