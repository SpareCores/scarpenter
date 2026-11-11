/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

var ctx = context.Background()

func code(err error) string {
	if p, ok := errors.AsType[*upcloud.Problem](err); ok {
		return p.ErrorCode()
	}
	return ""
}

// createReq builds a minimal create call cloning the template.
func createReq(f *Fake, plan, template string) *request.CreateServerRequest {
	labels := upcloud.LabelSlice{{Key: "k1", Value: "v1"}}
	return &request.CreateServerRequest{
		Zone: "fi-hel1", Plan: plan, Hostname: "h", Title: "h", Metadata: upcloud.True, Firewall: "on", Labels: &labels,
		UserData: "{}",
		Networking: &request.CreateServerNetworking{Interfaces: request.CreateServerInterfaceSlice{
			{Type: "public", IPAddresses: request.CreateServerIPAddressSlice{{Family: "IPv4"}}},
			{Type: "utility", IPAddresses: request.CreateServerIPAddressSlice{{Family: "IPv4"}}},
		}},
		StorageDevices: request.CreateServerStorageDeviceSlice{{
			Action: "clone", Storage: template, Title: "root", Size: 25, Tier: "maxiops",
		}},
	}
}

func setup(t *testing.T, o Options) (*Fake, string) {
	t.Helper()
	f := New(o)
	return f, f.AddTemplate("fi-hel1", "tpl", "maxiops", 10, map[string]string{"scarpenter-image": "flatcar"})
}

func TestServerLifecycle(t *testing.T) {
	f, tpl := setup(t, Options{})
	d, err := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl))
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "maintenance" {
		t.Fatalf("a new server is %s, want maintenance while its storage is cloned", d.State)
	}
	// Delete and stop of a server in maintenance are refused.
	if err := f.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: d.UUID}); code(err) != "SERVER_STATE_ILLEGAL" {
		t.Errorf("delete in maintenance: %v", err)
	}
	if _, err := f.StopServer(ctx, &request.StopServerRequest{UUID: d.UUID}); code(err) != "SERVER_STATE_ILLEGAL" {
		t.Errorf("stop in maintenance: %v", err)
	}
	if got, err := f.WaitForServerState(ctx, &request.WaitForServerStateRequest{UUID: d.UUID, DesiredState: "started"}); err != nil || got.State != "started" {
		t.Fatalf("wait: %v %v", got, err)
	}
	// A started server cannot be deleted; a stop passes through maintenance.
	if err := f.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: d.UUID}); code(err) != "SERVER_STATE_ILLEGAL" {
		t.Errorf("delete started: %v", err)
	}
	if s, err := f.StopServer(ctx, &request.StopServerRequest{UUID: d.UUID, StopType: "hard"}); err != nil || s.State != "maintenance" {
		t.Fatalf("stop: %v %v", s, err)
	}
	if err := f.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: d.UUID}); code(err) != "SERVER_STATE_ILLEGAL" {
		t.Errorf("delete while stopping: %v", err)
	}
	if _, err := f.WaitForServerState(ctx, &request.WaitForServerStateRequest{UUID: d.UUID, DesiredState: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.StopServer(ctx, &request.StopServerRequest{UUID: d.UUID}); code(err) != "SERVER_STATE_ILLEGAL" {
		t.Errorf("stop of a stopped server: %v", err)
	}
	if err := f.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: d.UUID, Backups: "delete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: d.UUID}); code(err) != "SERVER_NOT_FOUND" {
		t.Errorf("get after delete: %v", err)
	}
	if err := f.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: d.UUID}); code(err) != "SERVER_NOT_FOUND" {
		t.Errorf("second delete: %v", err)
	}
	if got := len(f.Storages()); got != 1 {
		t.Errorf("storages left = %d, want the template only", got)
	}
}

func TestNoIdempotencyToken(t *testing.T) {
	f, tpl := setup(t, Options{})
	for range 2 {
		if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.Servers()) != 2 {
		t.Fatal("a replayed create must make a second server: UpCloud has no token")
	}
}

func TestListHasNoLabelsDetailsDo(t *testing.T) {
	f, tpl := setup(t, Options{AutoSettle: true})
	d, _ := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl))
	other, _ := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl))
	if _, err := f.ModifyServer(ctx, &request.ModifyServerRequest{
		UUID: other.UUID, Labels: &upcloud.LabelSlice{{Key: "k1", Value: "other"}},
	}); err != nil {
		t.Fatal(err)
	}
	list, err := f.GetServersWithFilters(ctx, &request.GetServersWithFiltersRequest{Filters: []request.QueryFilter{
		request.FilterLabel{Label: upcloud.Label{Key: "k1", Value: "v1"}},
	}})
	if err != nil || len(list.Servers) != 1 || list.Servers[0].UUID != d.UUID {
		t.Fatalf("filtered list = %+v %v", list, err)
	}
	if list.Servers[0].Tags != nil {
		t.Error("list items carry no labels or tags")
	}
	keyOnly, _ := f.GetServersWithFilters(ctx, &request.GetServersWithFiltersRequest{Filters: []request.QueryFilter{
		request.FilterLabelKey{Key: "k1"},
	}})
	if len(keyOnly.Servers) != 2 {
		t.Errorf("key filter matched %d", len(keyOnly.Servers))
	}
	both, _ := f.GetServersWithFilters(ctx, &request.GetServersWithFiltersRequest{Filters: []request.QueryFilter{
		request.FilterLabel{Label: upcloud.Label{Key: "k1", Value: "v1"}}, request.FilterLabel{Label: upcloud.Label{Key: "k2", Value: "x"}},
	}})
	if len(both.Servers) != 0 {
		t.Errorf("filters are ANDed in the fake, matched %d", len(both.Servers))
	}
	det, _ := f.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: d.UUID})
	if len(det.Labels) != 1 || det.Labels[0].Value != "v1" {
		t.Errorf("details labels = %v", det.Labels)
	}
	// The returned structs are copies.
	det.Labels[0].Value = "mutated"
	if again, _ := f.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: d.UUID}); again.Labels[0].Value != "v1" {
		t.Error("the fake leaks its state")
	}
}

func TestTemplateRules(t *testing.T) {
	f, tpl := setup(t, Options{})
	other := f.AddTemplate("de-fra1", "tpl-de", "maxiops", 10, nil)
	std := f.AddTemplate("fi-hel1", "tpl-std", "standard", 10, nil)
	for name, tc := range map[string]struct {
		mod  func(*request.CreateServerRequest)
		want string
	}{
		"template in another zone": {func(r *request.CreateServerRequest) { r.StorageDevices[0].Storage = other }, "ZONE_MISMATCH"},
		"unknown template":         {func(r *request.CreateServerRequest) { r.StorageDevices[0].Storage = "nope" }, "STORAGE_NOT_FOUND"},
		"smaller than the image":   {func(r *request.CreateServerRequest) { r.StorageDevices[0].Size = 5 }, "SIZE_INVALID"},
		"tier of another template": {func(r *request.CreateServerRequest) { r.StorageDevices[0].Storage = std }, "STORAGE_TIER_ILLEGAL"},
		"unknown zone":             {func(r *request.CreateServerRequest) { r.Zone = "xx-nowhere1" }, "ZONE_INVALID"},
		"unknown plan":             {func(r *request.CreateServerRequest) { r.Plan = "9xCPU-1GB" }, "PLAN_INVALID"},
		"user data too large":      {func(r *request.CreateServerRequest) { r.UserData = strings.Repeat("x", 16385) }, "USER_DATA_INVALID"},
		"no interfaces":            {func(r *request.CreateServerRequest) { r.Networking.Interfaces = nil }, "INTERFACE_LIMIT_REACHED"},
		"unknown network": {func(r *request.CreateServerRequest) {
			r.Networking.Interfaces = append(r.Networking.Interfaces, request.CreateServerInterface{Type: "private", Network: "x"})
		}, "NETWORK_NOT_FOUND"},
	} {
		t.Run(name, func(t *testing.T) {
			r := createReq(f, "1xCPU-2GB", tpl)
			tc.mod(r)
			if _, err := f.CreateServer(ctx, r); code(err) != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
	// A template that is still being created cannot be cloned; the tier rule is configurable (S1).
	f.SetStorageState(tpl, "maintenance")
	if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl)); code(err) != "STORAGE_STATE_ILLEGAL" {
		t.Errorf("offline template: %v", err)
	}
	loose := New(Options{LooseTemplateTier: true})
	lt := loose.AddTemplate("fi-hel1", "t", "maxiops", 10, nil)
	r := createReq(loose, "1xCPU-2GB", lt)
	r.StorageDevices[0].Tier = "standard"
	if _, err := loose.CreateServer(ctx, r); err != nil {
		t.Errorf("loose tier: %v", err)
	}
}

func TestPlanTierRule(t *testing.T) {
	plans := DefaultPlans()
	plans[0].AllowedTiers = []string{"standard"}
	f := New(Options{Plans: plans, LooseTemplateTier: true})
	tpl := f.AddTemplate("fi-hel1", "t", "maxiops", 10, nil)
	r := createReq(f, "DEV-1xCPU-1GB-10GB", tpl)
	r.StorageDevices[0].Tier = "maxiops"
	if _, err := f.CreateServer(ctx, r); code(err) != "STORAGE_TIER_ILLEGAL" {
		t.Fatalf("a tier the plan does not allow: %v", err)
	}
}

func TestStorageAndLabelLimits(t *testing.T) {
	f, tpl := setup(t, Options{MaxStorageDevices: 2})
	r := createReq(f, "1xCPU-2GB", tpl)
	for range 2 {
		r.StorageDevices = append(r.StorageDevices, request.CreateServerStorageDevice{Action: "create", Title: "d", Size: 10, Tier: "maxiops"})
	}
	if _, err := f.CreateServer(ctx, r); code(err) != "STORAGE_DEVICE_LIMIT_REACHED" {
		t.Errorf("devices: %v", err)
	}
	for name, ls := range map[string]upcloud.LabelSlice{
		"key too long": {{Key: strings.Repeat("k", 33), Value: "v"}},
		"duplicate":    {{Key: "ab", Value: "1"}, {Key: "ab", Value: "2"}},
		"value long":   {{Key: "ab", Value: strings.Repeat("v", 256)}},
	} {
		r := createReq(f, "1xCPU-2GB", tpl)
		r.Labels = &ls
		if _, err := f.CreateServer(ctx, r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := make(upcloud.LabelSlice, 256)
	for i := range many {
		many[i] = upcloud.Label{Key: "k" + strings.Repeat("a", i/26) + string(rune('a'+i%26)), Value: "v"}
	}
	r = createReq(f, "1xCPU-2GB", tpl)
	r.Labels = &many
	if _, err := f.CreateServer(ctx, r); code(err) != "LABEL_LIMIT_REACHED" {
		t.Errorf("256 labels: %v", err)
	}
}

func TestAccountLimitsCountOtherOwnersServers(t *testing.T) {
	f, tpl := setup(t, Options{Limits: upcloud.ResourceLimits{Cores: 3}})
	f.AddServer(ServerSpec{Zone: "de-fra1", Plan: "2xCPU-4GB", Title: "theirs"})
	if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl)); err != nil {
		t.Fatalf("3 cores of 3: %v", err)
	}
	if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl)); code(err) != "SERVER_CORE_LIMIT_REACHED" {
		t.Fatalf("4 cores of 3: %v", err)
	}
	// Limits are account-wide: another zone is no different.
	r := createReq(f, "1xCPU-2GB", f.AddTemplate("de-fra1", "t", "maxiops", 10, nil))
	r.Zone = "de-fra1"
	if _, err := f.CreateServer(ctx, r); code(err) != "SERVER_CORE_LIMIT_REACHED" {
		t.Fatalf("other zone: %v", err)
	}
	f.SetLimits(upcloud.ResourceLimits{})
	if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl)); err != nil {
		t.Fatalf("no limits: %v", err)
	}
}

func TestCreatingLimit(t *testing.T) {
	f, tpl := setup(t, Options{CreatingLimit: 2})
	for range 2 {
		if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-1GB", tpl)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-1GB", tpl)); code(err) != "SERVER_CREATING_LIMIT_REACHED" {
		t.Fatalf("third concurrent create: %v", err)
	}
	f.Settle()
	if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-1GB", tpl)); err != nil {
		t.Fatalf("after settling: %v", err)
	}
}

func TestStrictAntiAffinity(t *testing.T) {
	f, tpl := setup(t, Options{HostsPerZone: 2, AutoSettle: true})
	g, err := f.CreateServerGroup(ctx, &request.CreateServerGroupRequest{
		Title: "g", AntiAffinityPolicy: upcloud.ServerGroupAntiAffinityPolicyStrict,
		Labels: &upcloud.LabelSlice{{Key: "scarpenter-cluster", Value: "c1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	members := make([]string, 0, 2)
	for range 2 {
		r := createReq(f, "1xCPU-1GB", tpl)
		r.ServerGroup = g.UUID
		d, err := f.CreateServer(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, d.UUID)
	}
	r := createReq(f, "1xCPU-1GB", tpl)
	r.ServerGroup = g.UUID
	if _, err := f.CreateServer(ctx, r); code(err) != "SERVER_RESOURCES_UNAVAILABLE_FOR_STRICT_AA" {
		t.Fatalf("third member: %v", err)
	}
	got, _ := f.GetServerGroup(ctx, &request.GetServerGroupRequest{UUID: g.UUID})
	if len(got.Members) != 2 || len(got.AntiAffinityStatus) != 2 || got.AntiAffinityStatus[0].Status != "met" {
		t.Errorf("group = %+v", got)
	}
	// Deleting a member frees a slot, the group stays after its servers.
	f.ForceState(members[0], "stopped")
	if err := f.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: members[0]}); err != nil {
		t.Fatal(err)
	}
	r = createReq(f, "1xCPU-1GB", tpl)
	r.ServerGroup = g.UUID
	if _, err := f.CreateServer(ctx, r); err != nil {
		t.Fatalf("after a member left: %v", err)
	}
	list, _ := f.GetServerGroups(ctx, &request.GetServerGroupsRequest{Filters: []request.QueryFilter{
		request.FilterLabel{Label: upcloud.Label{Key: "scarpenter-cluster", Value: "c1"}},
	}})
	if len(list) != 1 {
		t.Errorf("groups by label = %d", len(list))
	}
	if err := f.DeleteServerGroup(ctx, &request.DeleteServerGroupRequest{UUID: g.UUID}); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteServerGroup(ctx, &request.DeleteServerGroupRequest{UUID: g.UUID}); code(err) != "GROUP_NOT_FOUND" {
		t.Errorf("second delete: %v", err)
	}
}

func TestNetworks(t *testing.T) {
	f := New(Options{MaxNetworks: 2})
	mk := func(zone, cidr string, labels ...upcloud.Label) (*upcloud.Network, error) {
		return f.CreateNetwork(ctx, &request.CreateNetworkRequest{
			Name: "n", Zone: zone, Labels: labels, IPNetworks: upcloud.IPNetworkSlice{{Address: cidr, DHCP: upcloud.True}},
		})
	}
	n1, err := mk("fi-hel1", "10.1.0.0/24", upcloud.Label{Key: "scarpenter-use", Value: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mk("de-fra1", "10.1.0.128/25"); code(err) != "NETWORK_ADDRESS_OVERLAP" {
		t.Errorf("overlap: %v", err)
	}
	if _, err := mk("de-fra1", "not-a-cidr"); code(err) != "IP_NETWORK_ADDRESS_INVALID" {
		t.Errorf("bad cidr: %v", err)
	}
	if _, err := mk("xx", "10.2.0.0/24"); code(err) != "ZONE_INVALID" {
		t.Errorf("zone: %v", err)
	}
	if _, err := mk("de-fra1", "10.2.0.0/24"); err != nil {
		t.Fatal(err)
	}
	if _, err := mk("de-fra1", "10.3.0.0/24"); code(err) != "NETWORK_LIMIT_REACHED" {
		t.Errorf("limit: %v", err)
	}
	by, _ := f.GetNetworks(ctx, request.FilterLabel{Label: upcloud.Label{Key: "scarpenter-use", Value: "true"}})
	if len(by.Networks) != 1 || by.Networks[0].UUID != n1.UUID {
		t.Errorf("by label: %+v", by.Networks)
	}
	if err := f.DeleteNetwork(ctx, &request.DeleteNetworkRequest{UUID: n1.UUID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.GetNetworkDetails(ctx, &request.GetNetworkDetailsRequest{UUID: n1.UUID}); code(err) != "NETWORK_NOT_FOUND" {
		t.Errorf("get deleted: %v", err)
	}
	if _, err := f.CreateServer(ctx, func() *request.CreateServerRequest {
		tpl := f.AddTemplate("fi-hel1", "t", "maxiops", 10, nil)
		n, _ := mk("de-fra1", "10.4.0.0/24")
		_ = n
		r := createReq(f, "1xCPU-1GB", tpl)
		r.Networking.Interfaces = append(r.Networking.Interfaces, request.CreateServerInterface{Type: "private", Network: n1.UUID})
		return r
	}()); code(err) != "NETWORK_NOT_FOUND" {
		t.Errorf("deleted network on a server: %v", err)
	}
}

func TestPrivateNetworkZoneMismatch(t *testing.T) {
	f, tpl := setup(t, Options{})
	n, _ := f.CreateNetwork(ctx, &request.CreateNetworkRequest{Zone: "de-fra1", IPNetworks: upcloud.IPNetworkSlice{{Address: "10.5.0.0/24"}}})
	r := createReq(f, "1xCPU-1GB", tpl)
	r.Networking.Interfaces = append(r.Networking.Interfaces, request.CreateServerInterface{Type: "private", Network: n.UUID})
	if _, err := f.CreateServer(ctx, r); code(err) != "ZONE_MISMATCH" {
		t.Fatalf("network of another zone: %v", err)
	}
}

func TestFirewallRulesReplaceAndLimit(t *testing.T) {
	f, tpl := setup(t, Options{AutoSettle: true})
	d, _ := f.CreateServer(ctx, createReq(f, "1xCPU-1GB", tpl))
	set := func(n int) error {
		rules := make([]upcloud.FirewallRule, n)
		for i := range rules {
			rules[i] = upcloud.FirewallRule{Action: "accept", Direction: "in"}
		}
		return f.CreateFirewallRules(ctx, &request.CreateFirewallRulesRequest{ServerUUID: d.UUID, FirewallRules: rules})
	}
	if err := set(5); err != nil {
		t.Fatal(err)
	}
	if err := set(3); err != nil {
		t.Fatal(err)
	}
	got, _ := f.GetFirewallRules(ctx, &request.GetFirewallRulesRequest{ServerUUID: d.UUID})
	if len(got.FirewallRules) != 3 || got.FirewallRules[2].Position != 3 {
		t.Errorf("rules = %+v", got.FirewallRules)
	}
	if err := set(MaxFirewallRules + 1); code(err) != "FIREWALL_RULE_LIMIT_REACHED" {
		t.Errorf("over the limit: %v", err)
	}
	if err := set(MaxFirewallRules); err != nil {
		t.Errorf("at the limit: %v", err)
	}
}

func TestRulesetAttachNeedsPrivateInterface(t *testing.T) {
	f, tpl := setup(t, Options{AutoSettle: true})
	d, _ := f.CreateServer(ctx, createReq(f, "1xCPU-1GB", tpl))
	rs, err := f.CreateFirewallRuleset(ctx, apiRuleset())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.AttachPrivateFirewallRuleset(ctx, d.UUID, rs.UUID); code(err) != "INTERFACE_NOT_FOUND" {
		t.Errorf("a server with no private interface: %v", err)
	}
	if err := f.AttachPrivateFirewallRuleset(ctx, d.UUID, "nope"); code(err) != "FIREWALL_RULESET_NOT_FOUND" {
		t.Errorf("unknown ruleset: %v", err)
	}
	list, _ := f.GetFirewallRulesets(ctx)
	if len(list) != 1 || len(list[0].Rules) != 0 || len(list[0].Labels) != 1 {
		t.Errorf("list = %+v: no rules in a list, labels yes", list)
	}
	if one, _ := f.GetFirewallRuleset(ctx, rs.UUID); len(one.Rules) != 1 {
		t.Errorf("get = %+v", one)
	}
}

func TestInjectionContextAndCounts(t *testing.T) {
	f := New(Options{})
	f.InjectCode("GetZones", "SERVICE_ERROR", 500, 2)
	for range 2 {
		if _, err := f.GetZones(ctx); code(err) != "SERVICE_ERROR" {
			t.Fatalf("injected: %v", err)
		}
	}
	if z, err := f.GetZones(ctx); err != nil || len(z.Zones) != 3 {
		t.Fatalf("after the injection: %v %v", z, err)
	}
	if f.Calls("GetZones") != 3 {
		t.Errorf("calls = %d", f.Calls("GetZones"))
	}
	f.Inject("GetPlans", errors.New("boom"), 0)
	for range 3 {
		if _, err := f.GetPlans(ctx); err == nil {
			t.Fatal("a permanent injection stopped")
		}
	}
	f.Clear("GetPlans")
	if _, err := f.GetPlans(ctx); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.GetAccount(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
}

func TestIPAddresses(t *testing.T) {
	f, tpl := setup(t, Options{AutoSettle: true})
	r := createReq(f, "1xCPU-1GB", tpl)
	r.Networking.Interfaces[0].IPAddresses = append(r.Networking.Interfaces[0].IPAddresses, request.CreateServerIPAddress{Family: "IPv6"})
	d, err := f.CreateServer(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.IPAddresses) != 3 {
		t.Fatalf("server addresses = %+v", d.IPAddresses)
	}
	all, _ := f.GetIPAddresses(ctx)
	var pub4, pub6, util int
	for _, ip := range all.IPAddresses {
		switch {
		case ip.Access == "public" && ip.Family == "IPv4":
			pub4++
		case ip.Access == "public" && ip.Family == "IPv6":
			pub6++
		case ip.Access == "utility":
			util++
		}
		if ip.ServerUUID != d.UUID {
			t.Errorf("address of %s", ip.ServerUUID)
		}
	}
	if pub4 != 1 || pub6 != 1 || util != 1 {
		t.Errorf("v4 %d v6 %d utility %d", pub4, pub6, util)
	}
}

func TestStorageModifyAndListing(t *testing.T) {
	f, tpl := setup(t, Options{AutoSettle: true})
	d, _ := f.CreateServer(ctx, createReq(f, "1xCPU-1GB", tpl))
	root := d.StorageDevices[0].UUID
	if _, err := f.ModifyStorage(ctx, &request.ModifyStorageRequest{UUID: root, Size: 10}); code(err) != "SIZE_INVALID" {
		t.Errorf("shrink: %v", err)
	}
	if _, err := f.ModifyStorage(ctx, &request.ModifyStorageRequest{UUID: root, Labels: &[]upcloud.Label{{Key: "scarpenter-cluster", Value: "c1"}}}); err != nil {
		t.Fatal(err)
	}
	priv, _ := f.GetStorages(ctx, &request.GetStoragesRequest{Access: "private"})
	tmpl, _ := f.GetStorages(ctx, &request.GetStoragesRequest{Access: "private", Type: "template"})
	disks, _ := f.GetStorages(ctx, &request.GetStoragesRequest{Access: "private", Type: "normal"})
	byLabel, _ := f.GetStorages(ctx, &request.GetStoragesRequest{Access: "private", Filters: []request.QueryFilter{
		request.FilterLabel{Label: upcloud.Label{Key: "scarpenter-cluster", Value: "c1"}},
	}})
	if len(priv.Storages) != 2 || len(tmpl.Storages) != 1 || len(disks.Storages) != 1 || len(byLabel.Storages) != 1 {
		t.Errorf("private %d templates %d disks %d by label %d", len(priv.Storages), len(tmpl.Storages), len(disks.Storages), len(byLabel.Storages))
	}
	if tmpl.Storages[0].Labels[0].Key != "scarpenter-image" {
		t.Errorf("template labels = %v", tmpl.Storages[0].Labels)
	}
	st, _ := f.GetStorageDetails(ctx, &request.GetStorageDetailsRequest{UUID: root})
	if st.Origin != tpl || st.Size != 25 || st.Tier != "maxiops" || len(st.ServerUUIDs) != 1 || st.PartOfPlan != "yes" {
		t.Errorf("root = %+v", st)
	}
	if _, err := f.GetStorageDetails(ctx, &request.GetStorageDetailsRequest{UUID: "nope"}); code(err) != "STORAGE_NOT_FOUND" {
		t.Errorf("unknown storage: %v", err)
	}
}

func TestOutOfStockAndForceState(t *testing.T) {
	f, tpl := setup(t, Options{})
	f.SetOutOfStock("fi-hel1", "1xCPU-2GB", true)
	if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-2GB", tpl)); code(err) != "SERVER_RESOURCES_UNAVAILABLE" {
		t.Fatalf("out of stock: %v", err)
	}
	if _, err := f.CreateServer(ctx, createReq(f, "1xCPU-1GB", tpl)); err != nil {
		t.Fatalf("another plan: %v", err)
	}
	f.SetOutOfStock("fi-hel1", "1xCPU-2GB", false)
	id := f.AddServer(ServerSpec{Zone: "fi-hel1", Plan: "1xCPU-1GB", Title: "x"})
	f.ForceState(id, "stopped")
	if s, _ := f.Server(id); s.State != "stopped" {
		t.Errorf("state = %s", s.State)
	}
}

func apiRuleset() api.CreateRulesetRequest {
	return api.CreateRulesetRequest{
		Name: "rs", Stateful: true, Labels: []upcloud.Label{{Key: "scarpenter-use", Value: "true"}},
		Rules: []api.RulesetRule{{Action: "accept", Direction: "in", Protocol: "tcp", PortStart: 22, PortEnd: 22}},
	}
}
