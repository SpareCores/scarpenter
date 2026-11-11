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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

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
	return conformance.Fixture{
		New: func(t *testing.T) vendors.Driver {
			a, _ := newAccount(clk)
			a.AddSnapshotAs(suiteImage, "flatcar-"+testRelease+"-amd64 suite", fake.SnapshotComplete, time.Time{})
			return conformanceDriver(t, a, clk)
		},
		Region: testRegion, Zone: testRegion, InstanceType: testType, MaxUserData: vultr.MaxUserDataBytes,
		NoArm64: true, StoresIdempotencyKey: true, InstanceTypes: all,
		Now: time.Now,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, _ vendors.CreateRequest) {
			t.Helper()
			accountOf(t, d).FailNext("CreateInstance", 1, injected[class])
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			accountOf(t, d).SetInstanceStatus(ref.ID(), "active", "stopped", "ok")
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
