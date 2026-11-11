/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
	uc "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

const conformanceCluster = "conformance"

// drivers maps the drivers the conformance fixture built to their environments, so
// that the hooks (which only get the driver) can reach the fake account.
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

func register(e *env) {
	driversMu.Lock()
	drivers[e.d] = e
	driversMu.Unlock()
}

func warm(t *testing.T, e *env) *env {
	t.Helper()
	if _, err := e.d.LaunchableTypes(context.Background(), testZone); err != nil {
		t.Fatal(err)
	}
	register(e)
	return e
}

var injected = map[vendors.Class]struct {
	code   string
	status int
}{
	vendors.InsufficientCapacity: {"SERVER_RESOURCES_UNAVAILABLE", 503},
	vendors.Unsupported:          {"PLAN_CORE_NUMBER_ILLEGAL", 400},
	vendors.QuotaExceeded:        {"SERVER_CORE_LIMIT_REACHED", 403},
	vendors.ZoneUnusable:         {"ZONE_INVALID", 400},
	vendors.Retryable:            {"SERVER_STATE_ILLEGAL", 409},
	vendors.Fatal:                {"AUTHENTICATION_FAILED", 401},
}

func conformanceTypes(t *testing.T) (all, unknown []string) {
	t.Helper()
	for _, r := range upcloudServers(t) {
		all = append(all, r.APIReference)
		if uc.FamilyOf(r.APIReference).Supported() {
			unknown = append(unknown, r.APIReference+"/"+vendors.CapacitySpot)
		} else {
			unknown = append(unknown, r.APIReference)
		}
	}
	return all, unknown
}

func conformanceFixture(t *testing.T, kind vendors.ResourceKind, params json.RawMessage) conformance.Fixture {
	all, unknown := conformanceTypes(t)
	fx := conformance.Fixture{
		New: func(t *testing.T) vendors.Driver {
			opts := envOptions{cluster: conformanceCluster, templateID: true, shortKeys: true}
			opts.fake.Plans = snapshotPlans(t)
			return warm(t, newEnv(t, opts)).d
		},
		Region: testZone, Zone: testZone, InstanceType: testType, MaxUserData: uc.MaxUserData,
		StoresIdempotencyKey: true, AccountWideQuota: true,
		InstanceTypes: all, UnknownQuotaTypes: unknown,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, _ vendors.CreateRequest) {
			t.Helper()
			in := injected[class]
			envOf(t, d).f.InjectCode("CreateServer", in.code, in.status, 1)
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			envOf(t, d).f.ForceState(ref.ID(), upcloud.ServerStateStopped)
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, _ vendors.CreateRequest) {
			t.Helper()
			envOf(t, d).f.SetLimits(upcloud.ResourceLimits{Cores: 1})
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":  json.RawMessage(`{"data":[{"name":"pgdata","tier":"maxiops","size":"100Gi","mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"std": json.RawMessage(`{"root":{"tier":"standard"}}`),
				"big": json.RawMessage(`{"root":{"size":"100Gi"}}`),
			},
			vendors.ProfilePlacement: {
				"colocated": json.RawMessage(`{"serverGroup":{"antiAffinity":"strict"}}`),
				"spread":    json.RawMessage(`{"serverGroup":{"antiAffinity":"yes"}}`),
			},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: "upcloud", APIReference: "PREMIUM-2xCPU-4GB", Family: "premium", VCPUs: 2, LocalStorageGB: 50},
			{Vendor: "upcloud", APIReference: "STARTER-1xCPU-1GB", Family: "starter", VCPUs: 1, LocalStorageGB: 10},
			{Vendor: "upcloud", APIReference: "CLOUDNATIVE-2xCPU-4GB", Family: "cloudnative", VCPUs: 2},
		},
		ResourceKind: kind,
		ResourceParams: func(*testing.T, vendors.Driver, string) json.RawMessage {
			return params
		},
		Seed: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
			t.Helper()
			return seed(t, envOf(t, d), k, region, spec)
		},
		SeedImage: seedImage,
		SeedKinds: []vendors.ResourceKind{uc.KindNetworks, uc.KindServerGroups, uc.KindFirewalls},
		SeedAttrs: []string{"cidr", "specialNames", "created"},
		SeedCIDRs: [2]string{"172.20.1.0/24", "172.20.2.0/24"}, SeedWithin: "172.20.0.0/16",
		PreCreate: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
			t.Helper()
			return seed(t, envOf(t, d), k, region, conformance.SeedSpec{Name: "user-" + string(k), Tags: tags})
		},
		// The resolution order (ADR-022) without a vendor default: UpCloud has none for
		// any kind, so the cases that arrange a default are skipped and the others
		// (nothing found creates, forbid, interruption, kinds in a network) run.
		OrderClass: func(t *testing.T, _ vendors.Driver, res map[vendors.ResourceKind]*common.Resource, ipv6 bool) *v1alpha1.SCNodeClass {
			t.Helper()
			nc := class(testVersion, nil, nil)
			r := nc.Spec.Vendors.UpCloud.Resources
			r.Networks, r.Firewalls, r.ServerGroups = res[uc.KindNetworks], res[uc.KindFirewalls], res[uc.KindServerGroups]
			nc.Spec.Networking.PublicIPv6 = &ipv6
			return nc
		},
		SetDefault: func(t *testing.T, _ vendors.Driver, _ vendors.ResourceKind, _ string, state conformance.DefaultState) {
			t.Helper()
			if state != conformance.DefaultAbsent {
				t.Skip("UpCloud has no vendor default for any kind")
			}
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			old := envOf(t, d)
			e := attach(t, old.f, old.opts)
			register(e)
			return e.d
		},
	}
	if kind != uc.KindNetworks {
		fx.Seed, fx.SeedImage, fx.SeedKinds, fx.OrderClass, fx.SetDefault = nil, nil, nil, nil, nil
	}
	return fx
}

var seedCIDR, seedTemplate atomic.Int32

// seedImage makes a template the way a user would: private to the account, with
// the release in the labels flatcar-channel and flatcar-version when it says one.
func seedImage(t *testing.T, d vendors.Driver, region string, spec conformance.ImageSpec) string {
	t.Helper()
	labels := map[string]string{}
	maps.Copy(labels, spec.Tags)
	if spec.Version != "" {
		labels[uc.LabelFlatcarChannel], labels[uc.LabelFlatcarVersion] = spec.Channel, spec.Version
	}
	labels[uc.LabelFlatcarArch] = spec.Arch
	e := envOf(t, d)
	id := e.f.AddTemplate(region, fmt.Sprintf("conformance-%d", seedTemplate.Add(1)), upcloud.StorageTierMaxIOPS, 10, labels)
	if !spec.Created.IsZero() {
		e.f.SetStorageCreated(id, spec.Created)
	}
	return id
}

// seed makes a resource the way a user would, with the name, tags and range of the
// spec.
func seed(t *testing.T, e *env, kind vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
	t.Helper()
	ctx := context.Background()
	labels := make([]upcloud.Label, 0, len(spec.Tags))
	for _, k := range sortedLabelKeys(spec.Tags) {
		labels = append(labels, upcloud.Label{Key: k, Value: spec.Tags[k]})
	}
	switch kind {
	case uc.KindNetworks:
		cidr := spec.CIDR
		if cidr == "" {
			cidr = fmt.Sprintf("172.21.%d.0/24", seedCIDR.Add(1))
		}
		n, err := e.f.CreateNetwork(ctx, &request.CreateNetworkRequest{
			Name: spec.Name, Zone: region, Labels: labels,
			IPNetworks: upcloud.IPNetworkSlice{{Address: cidr, Family: upcloud.IPAddressFamilyIPv4}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: n.UUID, Name: spec.Name}
	case uc.KindFirewalls:
		r, err := e.f.CreateFirewallRuleset(ctx, api.CreateRulesetRequest{Name: spec.Name, Stateful: true, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		return vendors.ResourceRef{Kind: kind, Region: region, ID: r.UUID, Name: spec.Name}
	}
	ls := upcloud.LabelSlice(labels)
	g, err := e.f.CreateServerGroup(ctx, &request.CreateServerGroupRequest{
		Title: spec.Name, AntiAffinityPolicy: upcloud.ServerGroupAntiAffinityPolicyStrict, Labels: &ls,
	})
	if err != nil {
		t.Fatal(err)
	}
	return vendors.ResourceRef{Kind: kind, Region: region, ID: g.UUID, Name: spec.Name}
}

func sortedLabelKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	for i := range ks {
		for j := i + 1; j < len(ks); j++ {
			if strings.Compare(ks[j], ks[i]) < 0 {
				ks[i], ks[j] = ks[j], ks[i]
			}
		}
	}
	return ks
}

// TestConformance runs the shared driver suite over the complete UpCloud plan
// list of the committed snapshot, once per resource kind the driver creates: the
// suite's managed-resource subtests take one kind. The ruleset kind is covered by
// its own tests (the suite's in-use check needs a server with a private
// interface), its selector cases run in the networks run.
func TestConformance(t *testing.T) {
	for _, tc := range []struct {
		kind   vendors.ResourceKind
		params json.RawMessage
	}{
		{kind: uc.KindNetworks},
		{kind: uc.KindServerGroups, params: json.RawMessage(`{"antiAffinity":"strict"}`)},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			conformance.Run(t, conformanceFixture(t, tc.kind, tc.params))
		})
	}
}
