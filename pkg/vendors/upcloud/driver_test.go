/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	uc "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

func TestSnapshotPlansAreClassified(t *testing.T) {
	var gpu, supported int
	for _, r := range upcloudServers(t) {
		fam := uc.FamilyOf(r.APIReference)
		if fam == uc.FamilyUnknown {
			t.Errorf("the plan %s of the Keeper snapshot has no family: classify it in plans.go", r.APIReference)
		}
		if fam == uc.FamilyGPU {
			gpu++
			if fam.Supported() {
				t.Errorf("%s is launchable", r.APIReference)
			}
		} else if fam.Supported() {
			supported++
		}
	}
	if gpu == 0 || supported == 0 {
		t.Errorf("the snapshot has %d GPU and %d supported plans", gpu, supported)
	}
}

func TestInitNeedsATokenAndAValidRange(t *testing.T) {
	t.Setenv("UPCLOUD_TOKEN", "")
	if err := uc.New().Init(context.Background(), vendors.Config{}); err == nil {
		t.Error("Init without a token succeeded")
	}
	t.Setenv("UPCLOUD_TOKEN", "token")
	d := uc.New()
	if err := d.Init(context.Background(), vendors.Config{}); err != nil {
		t.Errorf("Init with a token: %v (it makes no call)", err)
	}
	t.Setenv(uc.NetworkRangeEnv, "10.0.0.0/24")
	if err := uc.New().Init(context.Background(), vendors.Config{}); err == nil || !strings.Contains(err.Error(), "/16") {
		t.Errorf("Init with a /24 range: %v", err)
	}
	t.Setenv(uc.NetworkRangeEnv, "")
	if err := uc.New(uc.WithNetworkRange("not a cidr")).Init(context.Background(), vendors.Config{}); err == nil {
		t.Error("Init with a bad range succeeded")
	}
	if d.Vendor() != "upcloud" || d.TagKey(vendors.TagCluster) != "scarpenter-cluster" {
		t.Errorf("identity: %s %s", d.Vendor(), d.TagKey(vendors.TagCluster))
	}
	if got := vendors.Registered(); !contains(got, "upcloud") {
		t.Errorf("the driver is not registered: %v", got)
	}
}

func contains(l []string, s string) bool { return slices.Contains(l, s) }

func TestRegionsAreZonesAndCached(t *testing.T) {
	e := newEnv(t, envOptions{})
	rs, err := e.d.Regions(e.ctx())
	if err != nil || len(rs) != 3 || rs[0].Name != "de-fra1" {
		t.Fatalf("Regions = %v, %v", rs, err)
	}
	if _, err := e.d.Regions(e.ctx()); err != nil || e.f.Calls("GetZones") != 1 {
		t.Errorf("GetZones was called %d times, want a cached answer", e.f.Calls("GetZones"))
	}
	zs, err := e.d.Zones(e.ctx(), "fi-hel1")
	if err != nil || len(zs) != 1 || zs[0].ID != "fi-hel1" || zs[0].Name != "fi-hel1" {
		t.Errorf("Zones = %v, %v", zs, err)
	}
	if _, err := e.d.Zones(e.ctx(), "xx-nowh1"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("Zones of an unknown zone: %v", err)
	}
	e2 := newEnv(t, envOptions{})
	e2.f.InjectCode("GetZones", "AUTHENTICATION_FAILED", 401, 1)
	if _, err := e2.d.Regions(e2.ctx()); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("Regions with bad credentials: %v", err)
	}
}

func settings(t *testing.T, d *uc.Driver, block string, net common.Networking, profiles vendors.Profiles, sel options.Selection,
) vendors.VendorSettings {
	t.Helper()
	if block == "" {
		block = "{}"
	}
	s, err := options.Resolve(d, options.Input{Block: json.RawMessage(block), Networking: net, Profiles: profiles}, sel)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func defaultNet() common.Networking { return options.ResolveNetworking(common.Networking{}, nil) }

func TestCreateInstanceWritesTheRequestTheDriverPromises(t *testing.T) {
	e := newEnv(t, envOptions{})
	rule := common.InboundRule{Protocol: "tcp", FromPort: new(int32(8080)), CIDRs: []string{"192.0.2.0/24"}}
	net := defaultNet()
	net.Inbound = &common.Inbound{Rules: []common.InboundRule{rule}}
	net.PublicIPv6 = new(true)
	req := e.request("n1", testType)
	req.Settings = settings(t, e.d,
		`{"storage":{"data":[{"name":"scratch","tier":"maxiops","size":"20Gi","mountPath":"/mnt/s"}]}}`, net, nil,
		options.Selection{})
	req.ExtraTags = map[string]string{"team": "infra"}
	inst, err := e.d.CreateInstance(e.ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Type != testType || inst.Zone != testZone || inst.State == vendors.StateTerminating || inst.BillingStart.IsZero() {
		t.Errorf("the instance %+v", inst)
	}
	_, uuid, _ := uc.ParseRef(inst.Ref)
	srv, ok := e.f.Server(uuid)
	if !ok {
		t.Fatal("no server")
	}
	if srv.Hostname != "n1" || srv.Title != "n1" || srv.Plan != testType || srv.Metadata != upcloud.True {
		t.Errorf("server %+v", srv)
	}
	labels := map[string]string{}
	for _, l := range srv.Labels {
		labels[l.Key] = l.Value
	}
	for k, want := range map[string]string{
		"scarpenter-cluster": testCluster, "scarpenter-nodeclaim": "n1", "scarpenter-managed-by": "scarpenter",
		"scarpenter-idempotency-key": "uid-n1", "team": "infra",
	} {
		if labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, labels[k], want)
		}
	}
	if labels[uc.LabelCreated] == "" {
		t.Error("no creation label")
	}
	checkStorages(t, e, srv)
	want, err := uc.PublicFirewallRules(uc.FirewallOptions{
		Inbound: []uc.Inbound{{CIDR: "192.0.2.0/24", Protocol: "tcp", PortStart: 8080, PortEnd: 8080}}, Tailscale: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := e.f.ServerFirewall(uuid)
	if len(got) != len(want) {
		t.Fatalf("the firewall has %d rules, want %d", len(got), len(want))
	}
	var ipv6 bool
	for _, ifc := range srv.Networking.Interfaces {
		for _, a := range ifc.IPAddresses {
			ipv6 = ipv6 || a.Family == upcloud.IPAddressFamilyIPv6
		}
	}
	if !ipv6 {
		t.Error("publicIPv6 did not add an IPv6 address")
	}
}

func TestCreateInstanceRefusesWhatCannotLaunch(t *testing.T) {
	e := newEnv(t, envOptions{})
	for name, tc := range map[string]struct {
		plan  string
		class vendors.Class
	}{
		"not in the account's list": {"NOPE-1xCPU-1GB", vendors.Unsupported},
		"a GPU plan":                {"GPU-8xCPU-64GB-1xL40S", vendors.Unsupported},
	} {
		req := e.request("c-"+strings.ToLower(tc.plan[:3]), tc.plan)
		if _, err := e.d.CreateInstance(e.ctx(), req); vendors.ClassOf(err) != tc.class {
			t.Errorf("%s: %v, want %s", name, err, tc.class)
		}
	}
	big := e.request("c-big", testType)
	big.UserData = []byte(strings.Repeat("x", uc.MaxUserData+1))
	if _, err := e.d.CreateInstance(e.ctx(), big); vendors.CodeOf(err) != "UserDataTooLarge" {
		t.Errorf("oversized user-data: %v", err)
	}
	gone := e.request("c-gone", testType)
	gone.Image.Ref = "00000000-0000-4000-8000-000000000000"
	if _, err := e.d.CreateInstance(e.ctx(), gone); vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Errorf("a template that does not exist: %v", err)
	}
	// publicIPv4 false without a private network has no route.
	net := defaultNet()
	net.PublicIPv4 = new(false)
	noroute := e.request("c-noroute", testType)
	noroute.Settings = settings(t, e.d, "", net, nil, options.Selection{})
	if _, err := e.d.CreateInstance(e.ctx(), noroute); vendors.CodeOf(err) != "NoRoute" {
		t.Errorf("publicIPv4 false without a network: %v", err)
	}
	if n := len(e.f.Servers()); n != 0 {
		t.Errorf("%d servers after refused launches", n)
	}
}

func TestAReplayRepairsTheMissingFirewall(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := e.request("n1", testType)
	first, err := e.d.CreateInstance(e.ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	_, uuid, _ := uc.ParseRef(first.Ref)
	if err := uc.WriteFirewall(e.ctx(), e.f, uuid, nil); err != nil {
		t.Fatal(err)
	}
	if len(e.f.ServerFirewall(uuid)) != 0 {
		t.Fatal("the firewall was not cleared")
	}
	again, err := e.d.CreateInstance(e.ctx(), req)
	if err != nil || again.Ref != first.Ref {
		t.Fatalf("the replay: %v, %v", again, err)
	}
	if len(e.f.ServerFirewall(uuid)) == 0 {
		t.Error("the replay left the server without its firewall")
	}
	if n := len(e.f.Servers()); n != 1 {
		t.Errorf("%d servers after a replay", n)
	}
}

func TestAServerWhoseFirewallFailsIsDeletedAndTheKeyStaysUnused(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := e.request("n1", testType)
	e.f.InjectCode("CreateFirewallRules", "ACTION_FORBIDDEN", 403, 1)
	if _, err := e.d.CreateInstance(e.ctx(), req); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("a failed firewall write: %v", err)
	}
	if n := len(e.f.Servers()); n != 0 {
		t.Errorf("%d servers left behind", n)
	}
	if _, err := e.d.CreateInstance(e.ctx(), req); err != nil {
		t.Errorf("the next attempt with the same key: %v", err)
	}
}

func TestALostCreateResponseIsFoundByItsLabel(t *testing.T) {
	e := newEnv(t, envOptions{})
	req := e.request("n1", testType)
	// The call times out although the server was made: the fake creates it, then the
	// lookup after the error finds it by the idempotency label.
	first, err := e.d.CreateInstance(e.ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	e.f.Inject("CreateServer", context.DeadlineExceeded, 1)
	other := req
	other.Tags = maps.Clone(req.Tags)
	again, err := e.d.CreateInstance(e.ctx(), other)
	if err != nil || again.Ref != first.Ref {
		t.Errorf("the replay found %v, %v; want %s", again, err, first.Ref)
	}
	// A timeout with nothing created is Retryable.
	fresh := e.request("n2", testType)
	e.f.Inject("CreateServer", errors.New("connection reset"), 1)
	if _, err := e.d.CreateInstance(e.ctx(), fresh); err == nil {
		t.Error("a transport error with nothing created succeeded")
	}
}

func TestDeleteInstanceRefusesAForeignServer(t *testing.T) {
	e := newEnv(t, envOptions{})
	foreign := e.f.AddServer(fake.ServerSpec{Zone: testZone, Plan: testType, Title: "theirs", Labels: map[string]string{
		"scarpenter-cluster": "another-cluster",
	}})
	if err := e.d.DeleteInstance(e.ctx(), uc.NewRef(testZone, foreign)); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("deleting another cluster's server: %v", err)
	}
	plain := e.f.AddServer(fake.ServerSpec{Zone: testZone, Plan: testType, Title: "plain"})
	if err := e.d.DeleteInstance(e.ctx(), uc.NewRef(testZone, plain)); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("deleting an unlabelled server: %v", err)
	}
	if _, ok := e.f.Server(foreign); !ok {
		t.Error("the foreign server was deleted")
	}
	// A wrong zone is not that server.
	mine := e.request("mine", testType)
	inst, err := e.d.CreateInstance(e.ctx(), mine)
	if err != nil {
		t.Fatal(err)
	}
	_, uuid, _ := uc.ParseRef(inst.Ref)
	if err := e.d.DeleteInstance(e.ctx(), uc.NewRef("de-fra1", uuid)); err != nil {
		t.Errorf("a reference in another zone: %v", err)
	}
	if _, ok := e.f.Server(uuid); !ok {
		t.Error("a reference in another zone deleted the server")
	}
	if err := e.d.DeleteInstance(e.ctx(), inst.Ref); err != nil {
		t.Fatal(err)
	}
}

func TestListAndFindInstances(t *testing.T) {
	e := newEnv(t, envOptions{})
	a, err := e.d.CreateInstance(e.ctx(), e.request("a", testType))
	if err != nil {
		t.Fatal(err)
	}
	e.f.AddServer(fake.ServerSpec{Zone: testZone, Plan: testType, Title: "other-owner"})
	all, err := e.d.ListInstances(e.ctx(), nil, nil)
	if err != nil || len(all) != 1 || all[0].Ref != a.Ref {
		t.Errorf("ListInstances = %v, %v; want only SCarpenter's server", all, err)
	}
	none, err := e.d.ListInstances(e.ctx(), []string{"de-fra1"}, nil)
	if err != nil || len(none) != 0 {
		t.Errorf("ListInstances in another zone = %v, %v", none, err)
	}
	if _, err := e.d.FindInstance(e.ctx(), testZone, nil); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("FindInstance without tags: %v", err)
	}
	if _, err := e.d.FindInstance(e.ctx(), "de-fra1", map[vendors.TagKey]string{vendors.TagNodeClaim: "a"}); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("FindInstance in another zone: %v", err)
	}
	got, err := e.d.FindInstance(e.ctx(), testZone, map[vendors.TagKey]string{vendors.TagNodeClaim: "a"})
	if err != nil || got.Ref != a.Ref {
		t.Errorf("FindInstance = %v, %v", got, err)
	}
	if _, err := e.d.GetInstance(e.ctx(), uc.NewRef(testZone, "00000000-0000-4000-8000-0000000000ff")); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("GetInstance of a deleted server: %v", err)
	}
	if _, err := e.d.GetInstance(e.ctx(), uc.NewRef("de-fra1", mustUUID(a.Ref))); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("GetInstance in the wrong zone: %v", err)
	}
}

func mustUUID(ref vendors.InstanceRef) string { return ref.ID() }

func TestInstanceStatesAndBillingStart(t *testing.T) {
	e := newEnv(t, envOptions{})
	a, err := e.d.CreateInstance(e.ctx(), e.request("a", testType))
	if err != nil {
		t.Fatal(err)
	}
	if a.BillingStart.IsZero() || !a.BillingStart.Equal(a.CreatedAt) {
		t.Errorf("billing start %v, created %v", a.BillingStart, a.CreatedAt)
	}
	e.f.ForceState(a.Ref.ID(), upcloud.ServerStateError)
	got, err := e.d.GetInstance(e.ctx(), a.Ref)
	if err != nil || got.State != vendors.StateTerminating {
		t.Errorf("a server in error: %v, %v", got, err)
	}
}

func TestBillingRuleIsUnverifiedAndHardeningIsOffered(t *testing.T) {
	e := newEnv(t, envOptions{})
	rule, ok := e.d.BillingRule("", "")
	if !ok || rule.Verified || rule.Increment == nil || rule.Increment.Hours() != 1 || rule.DeleteLatency != uc.StopTimeout {
		t.Errorf("BillingRule = %+v, %v", rule, ok)
	}
	if got := e.d.Hardenings(); len(got) != 1 || got[0] != vendors.HardeningMetadataFromPods {
		t.Errorf("Hardenings = %v", got)
	}
	frags, err := vendors.BootFragmentsFor(e.d, []string{vendors.HardeningMetadataFromPods, "unknown"}, vendors.BootFragmentRequest{})
	if err != nil || len(frags) != 1 || !strings.Contains(frags[0].Butane, uc.MetadataAddress) {
		t.Errorf("BootFragmentsFor = %v, %v", frags, err)
	}
	if frags, err := vendors.BootFragmentsFor(e.d, nil, vendors.BootFragmentRequest{}); err != nil || len(frags) != 0 {
		t.Errorf("nothing is always on: %v, %v", frags, err)
	}
	if name, ok := e.d.VendorArch("amd64"); !ok || name != "amd64" {
		t.Errorf("VendorArch(amd64) = %q, %v", name, ok)
	}
	if _, ok := e.d.VendorArch("arm64"); ok {
		t.Error("UpCloud has no arm64")
	}
}

func TestGlobalQuotasCacheTheLimitsAndConsumptionNeedsThePlanTable(t *testing.T) {
	e := newEnv(t, envOptions{})
	if _, known := e.d.Consumption(testType, vendors.CapacityOnDemand); known {
		t.Error("consumption known before the plan table was read")
	}
	if _, err := e.d.LaunchableTypes(e.ctx(), testZone); err != nil {
		t.Fatal(err)
	}
	cons, known := e.d.Consumption("CLOUDNATIVE-2xCPU-4GB", vendors.CapacityOnDemand)
	if !known {
		t.Fatal("Cloud Native consumption unknown")
	}
	for _, c := range cons {
		if c.Bucket == uc.BucketPublicIPv4 {
			t.Error("a Cloud Native plan has no included IPv4 address")
		}
	}
	if _, known := e.d.Consumption(testType, vendors.CapacitySpot); known {
		t.Error("spot is unknown")
	}
	if _, known := e.d.Consumption("GPU-8xCPU-64GB-1xL40S", vendors.CapacityOnDemand); known {
		t.Error("a GPU plan is unknown")
	}
	lazy := vendors.WithQuotaRefresh(e.ctx(), vendors.QuotaRefresh{Limits: false})
	g1, err := e.d.GlobalQuotas(lazy)
	if err != nil || len(g1.Buckets) == 0 {
		t.Fatalf("GlobalQuotas = %v, %v", g1, err)
	}
	if _, err := e.d.GlobalQuotas(lazy); err != nil || e.f.Calls("GetAccount") != 1 {
		t.Errorf("GetAccount called %d times, want the limits cached", e.f.Calls("GetAccount"))
	}
	if _, err := e.d.GlobalQuotas(vendors.WithQuotaRefresh(e.ctx(), vendors.QuotaRefresh{Limits: true})); err != nil ||
		e.f.Calls("GetAccount") != 2 {
		t.Errorf("a refresh did not read the limits again (%d calls)", e.f.Calls("GetAccount"))
	}
	if snap, err := e.d.Quotas(e.ctx(), testZone); err != nil || len(snap.Buckets) != 0 {
		t.Errorf("Quotas(zone) = %v, %v: every limit is account-wide", snap, err)
	}
	if _, err := e.d.LaunchableTypes(e.ctx(), "xx-nowh1"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("LaunchableTypes of an unknown zone: %v", err)
	}
	types, err := e.d.LaunchableTypes(e.ctx(), testZone)
	if err != nil || len(types["GPU-8xCPU-64GB-1xL40S"]) != 0 || len(types["PREMIUM-1xCPU-1GB"]) != 1 {
		t.Errorf("LaunchableTypes: %v, %v", types, err)
	}
}

// checkStorages checks the devices of a launch (the plan's 50 GB MaxIOPS root and the data disk) and the cluster label of
// every storage the call made.
func checkStorages(t *testing.T, e *env, srv *upcloud.ServerDetails) {
	t.Helper()
	devs := srv.StorageDevices
	if len(devs) != 2 || devs[0].Tier != upcloud.StorageTierMaxIOPS || devs[0].Size != 50 || devs[1].Size != 20 {
		t.Errorf("devices %+v", devs)
	}
	for _, st := range e.f.Storages() {
		if st.Type == upcloud.StorageTypeTemplate {
			continue
		}
		var cluster string
		for _, l := range st.Labels {
			if l.Key == "scarpenter-cluster" {
				cluster = l.Value
			}
		}
		if cluster != testCluster {
			t.Errorf("the storage %s is not labelled with the cluster: %+v", st.Title, st.Labels)
		}
	}
}
