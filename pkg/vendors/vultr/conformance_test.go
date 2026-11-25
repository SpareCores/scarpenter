/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vultr/govultr/v3"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	apicommon "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	vultrapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

const conformanceCluster = "conformance"

// The conformance suite's requests carry a fixed image reference.
const suiteImage = "image"

// accounts maps the drivers the fixture built to their fake accounts, so that the
// hooks (which only get the driver) can reach the account.
var (
	accountsMu sync.Mutex
	accounts   = map[vendors.Driver]*fake.Account{}
)

func accountOf(t *testing.T, d vendors.Driver) *fake.Account {
	t.Helper()
	accountsMu.Lock()
	defer accountsMu.Unlock()
	a, ok := accounts[d]
	if !ok {
		t.Fatal("an unknown driver")
	}
	return a
}

func conformanceDriver(t *testing.T, a *fake.Account, clk *clocktesting.FakeClock) vendors.Driver {
	t.Helper()
	d := driverOver(t, a, clk, conformanceCluster)
	accountsMu.Lock()
	accounts[d] = a
	accountsMu.Unlock()
	return d
}

var injected = map[vendors.Class]*fake.Error{
	vendors.InsufficientCapacity: {Status: 400, Message: fake.MsgPlanUnavailable},
	vendors.Unsupported:          {Status: 400, Message: fake.MsgInvalidPlan},
	vendors.QuotaExceeded:        {Status: 400, Message: fake.MsgInstanceLimit},
	vendors.ZoneUnusable:         {Status: 400, Message: fake.MsgInvalidRegion},
	vendors.Retryable:            {Status: 503, Message: "Service unavailable."},
	vendors.Fatal:                {Status: 400, Message: fake.MsgInvalidUserData},
}

var seedImages atomic.Int32

func conformanceFixture(t *testing.T, kind vendors.ResourceKind) conformance.Fixture {
	rows := vultrServers(t)
	all := make([]string, 0, len(rows))
	for _, r := range rows {
		all = append(all, r.APIReference)
	}
	clk := clocktesting.NewFakeClock(time.Now())
	fx := conformance.Fixture{
		New: func(t *testing.T) vendors.Driver {
			a, _ := newAccount(clk)
			a.AddSnapshotAs(suiteImage, "flatcar-"+testRelease+"-amd64 suite", fake.SnapshotComplete, time.Time{})
			return conformanceDriver(t, a, clk)
		},
		Region: testRegion, Zone: testRegion, InstanceType: testType, MaxUserData: vultr.MaxUserDataBytes,
		NoArm64: true, StoresIdempotencyKey: true, NoSpot: true, InstanceTypes: all,
		Now: time.Now,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, _ vendors.CreateRequest) {
			t.Helper()
			accountOf(t, d).FailNext("CreateInstance", 1, injected[class])
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			accountOf(t, d).SetInstanceStatus(ref.ID(), "active", "stopped", "ok")
		},
		AttachForeign: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) func() bool {
			t.Helper()
			acct := accountOf(t, d)
			block := acct.AttachBlock()
			return func() bool { return acct.BlockExists(block) }
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage:   {"data": json.RawMessage(`{}`)},
			vendors.ProfileNetwork:   {"fast": json.RawMessage(`{}`)},
			vendors.ProfilePlacement: {"close": json.RawMessage(`{}`)},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: "vultr", APIReference: testType, Family: "vc2", VCPUs: 1, LocalStorageGB: 25},
			{Vendor: "vultr", APIReference: "vhf-1c-1gb", Family: "vhf", VCPUs: 1, LocalStorageGB: 32},
			{Vendor: "vultr", APIReference: "vx1-g-2c-8g", Family: "vx1", VCPUs: 2},
		},
		ResourceKind: kind,
		// A group with a rule: the creation is the group and then its rules.
		ResourceParams: func(*testing.T, vendors.Driver, string) json.RawMessage {
			if kind != vultr.KindFirewallGroups {
				return nil
			}
			return json.RawMessage(`{"inbound":{"rules":[{"protocol":"tcp","fromPort":22,"cidrs":["10.0.0.0/8"]}]}}`)
		},
		// Vultr's groups and networks have a description and no tags: the cases seed
		// them by it (the name). There is no vendor default of any kind: no SetDefault.
		Seed: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
			t.Helper()
			return seed(t, accountOf(t, d), clk, k, region, spec)
		},
		SeedAttrs: []string{"created", "cidr"},
		SeedCIDRs: [2]string{"172.20.1.0/24", "172.20.2.0/24"}, SeedWithin: "172.16.0.0/12",
		OrderClass: func(t *testing.T, _ vendors.Driver, res map[vendors.ResourceKind]*apicommon.Resource, ipv6 bool) *v1alpha1.SCNodeClass {
			t.Helper()
			nc := nodeClass(v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{Vultr: &vultrapi.Settings{
				Resources: &vultrapi.Resources{FirewallGroups: res[vultr.KindFirewallGroups], Networks: res[vultr.KindNetworks]},
			}}})
			nc.Spec.Networking.PublicIPv6 = &ipv6
			return nc
		},
		FailCreate: func(t *testing.T, d vendors.Driver, kind vendors.ResourceKind) {
			t.Helper()
			// The group exists when its first rule fails (a network is one call).
			accountOf(t, d).FailNext("CreateFirewallRule", 1, &fake.Error{Status: 400, Message: "Invalid rule."})
		},
		SeedImage: func(t *testing.T, d vendors.Driver, _ string, spec conformance.ImageSpec) string {
			t.Helper()
			// A snapshot has no tags: the release is in its description, and the
			// suffix keeps the descriptions of the cases apart (the default lookup
			// is the exact description, which none of them has).
			desc := fmt.Sprintf("conformance-image-%d", seedImages.Add(1))
			if spec.Version != "" {
				desc = fmt.Sprintf("flatcar-%s-%s-amd64 case-%d", spec.Channel, spec.Version, seedImages.Add(1))
			}
			return accountOf(t, d).AddSnapshotAs("", desc, fake.SnapshotComplete, spec.Created)
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			return conformanceDriver(t, accountOf(t, d), clk)
		},
	}
	if kind != vultr.KindFirewallGroups {
		fx.FailCreate = nil
	}
	return fx
}

// seed makes a resource the way a user would: a firewall group or a network with
// the name as its description, created at the spec's time.
func seed(
	t *testing.T, a *fake.Account, clk *clocktesting.FakeClock, kind vendors.ResourceKind, region string,
	spec conformance.SeedSpec,
) vendors.ResourceRef {
	t.Helper()
	ctx := context.Background()
	if !spec.Created.IsZero() {
		a.SetClock(func() time.Time { return spec.Created })
		defer a.SetClock(clk.Now)
	}
	ref := vendors.ResourceRef{Kind: kind, Region: region, Name: spec.Name}
	switch kind {
	case vultr.KindFirewallGroups:
		g, err := a.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: spec.Name})
		if err != nil {
			t.Fatal(err)
		}
		ref.ID = g.ID
	case vultr.KindNetworks:
		req := &govultr.VPCReq{Region: region, Description: spec.Name}
		if spec.CIDR != "" {
			p := netip.MustParsePrefix(spec.CIDR)
			req.V4Subnet, req.V4SubnetMask = p.Addr().String(), p.Bits()
		}
		v, err := a.CreateVPC(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		ref.ID, ref.Network = v.ID, v.ID
	default:
		t.Fatalf("seed: no kind %s", kind)
	}
	return ref
}

// TestConformance runs the shared driver suite over the complete Vultr type list
// of the committed snapshot, once per resource kind the driver creates.
func TestConformance(t *testing.T) {
	for _, kind := range []vendors.ResourceKind{vultr.KindFirewallGroups, vultr.KindNetworks} {
		t.Run(string(kind), func(t *testing.T) {
			conformance.Run(t, conformanceFixture(t, kind))
		})
	}
}

var _ = context.Background
