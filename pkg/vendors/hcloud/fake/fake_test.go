/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package fake_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud/fake"
)

var ctx = context.Background()

func code(err error) hc.ErrorCode {
	if e, ok := err.(hc.Error); ok {
		return e.Code
	}
	return ""
}

func fields(err error) []string {
	e, _ := err.(hc.Error)
	d, _ := e.Details.(hc.ErrorDetailsInvalidInput)
	out := make([]string, 0, len(d.Fields))
	for _, f := range d.Fields {
		out = append(out, f.Name)
	}
	return out
}

func opts(f *fake.Fake, name string, mod ...func(*hc.ServerCreateOpts)) hc.ServerCreateOpts {
	x86, _ := f.AddFlatcarImages("stable", "1."+name)
	o := hc.ServerCreateOpts{
		Name: name, ServerType: &hc.ServerType{Name: "cx23"}, Location: &hc.Location{Name: "fsn1"}, Image: &hc.Image{ID: x86.ID},
		Labels: map[string]string{"a": "b"}, PublicNet: &hc.ServerCreatePublicNet{EnableIPv4: true},
	}
	for _, m := range mod {
		m(&o)
	}
	return o
}

func TestSelectorParse(t *testing.T) {
	labels := map[string]string{"a": "1", "b": "2", "env": "prod"}
	for sel, want := range map[string]bool{
		"": true, "a=1": true, "a==1": true, "a=2": false, "a!=2": true, "a!=1": false, "missing!=x": true,
		"a": true, "missing": false, "!missing": true, "!a": false,
		"env in (prod,dev)": true, "env in (dev,test)": false, "env notin (dev)": true, "env notin (prod)": false,
		"missing notin (x)": true, "a=1,b=2": true, "a=1,b=3": false, "env in (prod,dev),a=1": true,
	} {
		s, err := fake.ParseSelector(sel)
		if err != nil {
			t.Errorf("%q: %v", sel, err)
			continue
		}
		if got := s.Matches(labels); got != want {
			t.Errorf("%q: %v, want %v", sel, got, want)
		}
	}
	for _, bad := range []string{"a in prod", "=x", "a in (b"} {
		if _, err := fake.ParseSelector(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestServerLifecycle(t *testing.T) {
	f := fake.New()
	res, err := f.CreateServer(ctx, opts(f, "n1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Server.Status != hc.ServerStatusInitializing || res.Action.Status != hc.ActionStatusRunning {
		t.Errorf("%s %s", res.Server.Status, res.Action.Status)
	}
	if res.Server.PublicNet.IPv4.IP == nil || res.Server.PublicNet.IPv4.ID == 0 || !res.Server.PublicNet.IPv6.IsUnspecified() {
		t.Errorf("public net %+v", res.Server.PublicNet)
	}
	if err := f.WaitForAction(ctx, res.Action); err != nil {
		t.Fatal(err)
	}
	s, _ := f.GetServer(ctx, res.Server.ID)
	if s.Status != hc.ServerStatusRunning || s.PrimaryDiskSize != 40 || s.Location.Name != "fsn1" {
		t.Errorf("%+v", s)
	}
	f.PowerOff(s.ID)
	if s, _ = f.GetServer(ctx, s.ID); s.Status != hc.ServerStatusOff {
		t.Error(s.Status)
	}
	act, err := f.DeleteServer(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ = f.GetServer(ctx, s.ID); s == nil || s.Status != hc.ServerStatusDeleting {
		t.Error("a deleting server is still there until the action completes")
	}
	if err := f.WaitForAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	if s, _ = f.GetServer(ctx, res.Server.ID); s != nil {
		t.Error("deleted")
	}
	if _, err := f.DeleteServer(ctx, res.Server); code(err) != hc.ErrorCodeNotFound {
		t.Errorf("delete of a gone server: %v", err)
	}
	if len(f.PrimaryIPs()) != 0 {
		t.Error("auto_delete Primary IPs go with the server")
	}
}

func TestTickStatusMachine(t *testing.T) {
	f := fake.New()
	res, _ := f.CreateServer(ctx, opts(f, "n1"))
	step := func(want hc.ServerStatus) {
		t.Helper()
		f.Tick()
		if s, _ := f.GetServer(ctx, res.Server.ID); s.Status != want {
			t.Errorf("status %s, want %s", s.Status, want)
		}
	}
	step(hc.ServerStatusStarting)
	step(hc.ServerStatusRunning)
	f.SetStatus(res.Server.ID, hc.ServerStatusStopping)
	step(hc.ServerStatusOff)
	_, _ = f.DeleteServer(ctx, res.Server)
	f.Tick()
	if f.ServerCount() != 0 {
		t.Error("Tick finishes a delete")
	}
}

func TestCreateValidation(t *testing.T) {
	f := fake.New()
	_, err := f.CreateServer(ctx, opts(f, "dup"))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mod    func(*hc.ServerCreateOpts)
		code   hc.ErrorCode
		fields []string
	}{
		"duplicate name":   {func(o *hc.ServerCreateOpts) { o.Name = "dup" }, hc.ErrorCodeUniquenessError, nil},
		"bad name":         {func(o *hc.ServerCreateOpts) { o.Name = "Bad_Name" }, hc.ErrorCodeInvalidInput, []string{"name"}},
		"user data":        {func(o *hc.ServerCreateOpts) { o.UserData = strings.Repeat("x", fake.MaxUserData+1) }, hc.ErrorCodeInvalidInput, []string{"user_data"}},
		"unknown type":     {func(o *hc.ServerCreateOpts) { o.ServerType = &hc.ServerType{Name: "zz9"} }, hc.ErrorCodeInvalidInput, []string{"server_type"}},
		"unknown location": {func(o *hc.ServerCreateOpts) { o.Location = &hc.Location{Name: "nbg1-dc3"} }, hc.ErrorCodeInvalidInput, []string{"location"}},
		"arm type in ash": {func(o *hc.ServerCreateOpts) {
			o.ServerType = &hc.ServerType{Name: "cax11"}
			o.Location = &hc.Location{Name: "ash"}
		}, hc.ErrorCodeInvalidInput, []string{"server_type"}},
		"unknown image":    {func(o *hc.ServerCreateOpts) { o.Image = &hc.Image{ID: 1} }, hc.ErrorCodeInvalidInput, []string{"image"}},
		"wrong arch image": {func(o *hc.ServerCreateOpts) { o.ServerType = &hc.ServerType{Name: "cax11"} }, hc.ErrorCodeInvalidInput, []string{"image"}},
		"unknown network":  {func(o *hc.ServerCreateOpts) { o.Networks = []*hc.Network{{ID: 5}} }, hc.ErrorCodeInvalidInput, []string{"networks"}},
		"unknown firewall": {func(o *hc.ServerCreateOpts) { o.Firewalls = []*hc.ServerCreateFirewall{{Firewall: hc.Firewall{ID: 5}}} }, hc.ErrorCodeInvalidInput, []string{"firewalls"}},
		"unknown group":    {func(o *hc.ServerCreateOpts) { o.PlacementGroup = &hc.PlacementGroup{ID: 5} }, hc.ErrorCodeInvalidInput, []string{"placement_group"}},
		"unknown volume":   {func(o *hc.ServerCreateOpts) { o.Volumes = []*hc.Volume{{ID: 5}} }, hc.ErrorCodeInvalidInput, []string{"volumes"}},
		"unknown ssh key":  {func(o *hc.ServerCreateOpts) { o.SSHKeys = []*hc.SSHKey{{ID: 5}} }, hc.ErrorCodeInvalidInput, []string{"ssh_keys"}},
	} {
		_, err := f.CreateServer(ctx, opts(f, "x"+strings.ReplaceAll(name, " ", ""), tc.mod))
		if code(err) != tc.code {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if tc.fields != nil && fmt.Sprint(fields(err)) != fmt.Sprint(tc.fields) {
			t.Errorf("%s: fields %v", name, fields(err))
		}
	}
	if f.ServerCount() != 1 {
		t.Errorf("failed creates leave nothing: %d servers", f.ServerCount())
	}
}

func TestStockAndDeprecation(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	f := fake.New(fake.WithClock(clocktesting.NewFakeClock(now)))
	f.SetAvailable("cx23", "fsn1", false)
	if _, err := f.CreateServer(ctx, opts(f, "a")); code(err) != hc.ErrorCodeResourceUnavailable {
		t.Errorf("out of stock: %v", err)
	}
	types, _ := f.ServerTypes(ctx)
	for _, l := range types[0].Locations {
		if l.Location.Name == "fsn1" && l.Available {
			t.Error("GET /server_types must show available false")
		}
	}
	f.SetDeprecation("cpx22", "fsn1", now.Add(-48*time.Hour), now.Add(-time.Hour))
	if _, err := f.CreateServer(ctx, opts(f, "b", func(o *hc.ServerCreateOpts) { o.ServerType = &hc.ServerType{Name: "cpx22"} })); code(err) != hc.ErrorCodeInvalidServerType {
		t.Errorf("past unavailable_after: %v", err)
	}
}

func TestLimitsCountForeignServers(t *testing.T) {
	f := fake.New(fake.WithLimits(fake.Limits{Servers: 2}))
	f.AddForeignServer("fsn1", "cx23", nil)
	if _, err := f.CreateServer(ctx, opts(f, "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateServer(ctx, opts(f, "b")); code(err) != hc.ErrorCodeResourceLimitExceeded {
		t.Errorf("limit counts the project's servers, foreign included: %v", err)
	}
	g := fake.New(fake.WithLimits(fake.Limits{PrimaryIPs: 1}))
	if _, err := g.CreateServer(ctx, opts(g, "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateServer(ctx, opts(g, "b")); code(err) != hc.ErrorCodeResourceLimitExceeded {
		t.Errorf("primary ip limit: %v", err)
	}
}

func TestListFiltersAndPages(t *testing.T) {
	f := fake.New()
	for i := range 120 {
		lbl := "x"
		if i%3 == 0 {
			lbl = "y"
		}
		if _, err := f.CreateServer(ctx, opts(f, fmt.Sprintf("n%03d", i), func(o *hc.ServerCreateOpts) {
			o.Labels = map[string]string{"k": lbl}
			o.PublicNet = &hc.ServerCreatePublicNet{EnableIPv4: false, EnableIPv6: false}
		})); err != nil {
			t.Fatal(err)
		}
	}
	before := f.Requests()
	all, err := f.Servers(ctx, hc.ServerListOpts{})
	if err != nil || len(all) != 120 {
		t.Fatalf("%d %v", len(all), err)
	}
	if got := f.Requests() - before; got != 3 {
		t.Errorf("120 servers are 3 pages of 50, cost %d requests", got)
	}
	ys, _ := f.Servers(ctx, hc.ServerListOpts{ListOpts: hc.ListOpts{LabelSelector: "k=y"}})
	if len(ys) != 40 {
		t.Errorf("%d", len(ys))
	}
	one, _ := f.Servers(ctx, hc.ServerListOpts{Name: "n007"})
	if len(one) != 1 {
		t.Errorf("by name: %d", len(one))
	}
	if _, err := f.Servers(ctx, hc.ServerListOpts{ListOpts: hc.ListOpts{LabelSelector: "k in x"}}); code(err) != hc.ErrorCodeInvalidInput {
		t.Errorf("bad selector: %v", err)
	}
	empty := fake.New()
	b := empty.Requests()
	_, _ = empty.Servers(ctx, hc.ServerListOpts{})
	if empty.Requests()-b != 1 {
		t.Error("an empty list still costs one request")
	}
}

func TestPrimaryIPs(t *testing.T) {
	f := fake.New(fake.WithPrimaryIPAutoDelete(false))
	res, _ := f.CreateServer(ctx, opts(f, "n", func(o *hc.ServerCreateOpts) {
		o.PublicNet = &hc.ServerCreatePublicNet{EnableIPv4: true, EnableIPv6: true}
	}))
	if res.Server.PublicNet.IPv6.Network == nil {
		t.Error("ipv6 /64")
	}
	ips := f.PrimaryIPs()
	if len(ips) != 2 || ips[0].AssigneeID != res.Server.ID || ips[0].AutoDelete {
		t.Fatalf("%+v", ips)
	}
	if err := f.DeletePrimaryIP(ctx, ips[0]); code(err) != hc.ErrorCodePrimaryIPAssigned {
		t.Errorf("assigned: %v", err)
	}
	act, _ := f.DeleteServer(ctx, res.Server)
	_ = f.WaitForAction(ctx, act)
	ips = f.PrimaryIPs()
	if len(ips) != 2 || ips[0].AssigneeID != 0 {
		t.Fatalf("without auto_delete the IPs stay, unassigned: %+v", ips)
	}
	if err := f.DeletePrimaryIP(ctx, ips[0]); err != nil {
		t.Error(err)
	}
	if ip, _ := f.GetPrimaryIP(ctx, ips[0].ID); ip != nil {
		t.Error("deleted")
	}
	if err := f.DeletePrimaryIP(ctx, ips[0]); code(err) != hc.ErrorCodeNotFound {
		t.Error(err)
	}
}

func TestNetworksFirewallsGroupsVolumes(t *testing.T) {
	f := fake.New()
	nw := f.AddNetwork("net", "10.0.0.0/16", map[string]string{"use": "true"}, hc.NetworkZoneEUCentral)
	fw, err := f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "fw", Labels: map[string]string{"l": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "fw"}); code(err) != hc.ErrorCodeUniquenessError {
		t.Error(err)
	}
	pg, _ := f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "pg", Type: hc.PlacementGroupTypeSpread})
	if _, err := f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "pg2", Type: "other"}); code(err) != hc.ErrorCodeInvalidInput {
		t.Error(err)
	}
	vol, err := f.CreateVolume(ctx, hc.VolumeCreateOpts{Name: "v", Size: 10, Location: &hc.Location{Name: "fsn1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateVolume(ctx, hc.VolumeCreateOpts{Name: "small", Size: 9, Location: &hc.Location{Name: "fsn1"}}); code(err) != hc.ErrorCodeInvalidInput {
		t.Error(err)
	}
	res, err := f.CreateServer(ctx, opts(f, "n", func(o *hc.ServerCreateOpts) {
		o.Networks = []*hc.Network{{ID: nw.ID}}
		o.Firewalls = []*hc.ServerCreateFirewall{{Firewall: hc.Firewall{ID: fw.ID}}}
		o.PlacementGroup = &hc.PlacementGroup{ID: pg.ID}
		o.Volumes = []*hc.Volume{{ID: vol.ID}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Server.PrivateNet) != 1 || res.Server.PrivateNet[0].IP == nil || res.Server.PlacementGroup.ID != pg.ID || len(res.Server.Volumes) != 1 {
		t.Errorf("%+v", res.Server)
	}
	if got, _ := f.GetFirewall(ctx, fw.ID); len(got.AppliedTo) != 1 {
		t.Error("applied_to")
	}
	if got, _ := f.GetPlacementGroup(ctx, pg.ID); len(got.Servers) != 1 {
		t.Error("group members")
	}
	if got, _ := f.GetVolume(ctx, vol.ID); got.Server == nil || got.Server.ID != res.Server.ID {
		t.Error("volume attached")
	}
	for name, err := range map[string]error{
		"firewall": f.DeleteFirewall(ctx, fw), "group": f.DeletePlacementGroup(ctx, pg), "volume": f.DeleteVolume(ctx, vol),
	} {
		if code(err) != hc.ErrorCodeResourceInUse {
			t.Errorf("%s in use: %v", name, err)
		}
	}
	if _, err := f.CreateServer(ctx, opts(f, "n2", func(o *hc.ServerCreateOpts) { o.Volumes = []*hc.Volume{{ID: vol.ID}} })); code(err) != hc.ErrorCodeInvalidInput {
		t.Errorf("an attached volume cannot be attached again: %v", err)
	}
	act, _ := f.DeleteServer(ctx, res.Server)
	_ = f.WaitForAction(ctx, act)
	// Volumes outlive servers; firewall and group are free again.
	if got, _ := f.GetVolume(ctx, vol.ID); got == nil || got.Server != nil {
		t.Error("the volume outlives the server, detached")
	}
	for name, err := range map[string]error{
		"firewall": f.DeleteFirewall(ctx, fw), "group": f.DeletePlacementGroup(ctx, pg), "volume": f.DeleteVolume(ctx, vol),
	} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, err := range []error{f.DeleteFirewall(ctx, fw), f.DeletePlacementGroup(ctx, pg), f.DeleteVolume(ctx, vol)} {
		if code(err) != hc.ErrorCodeNotFound {
			t.Error(err)
		}
	}
}

func TestVolumeLocationBound(t *testing.T) {
	f := fake.New()
	vol, _ := f.CreateVolume(ctx, hc.VolumeCreateOpts{Name: "v", Size: 10, Location: &hc.Location{Name: "nbg1"}})
	if _, err := f.CreateServer(ctx, opts(f, "n", func(o *hc.ServerCreateOpts) { o.Volumes = []*hc.Volume{{ID: vol.ID}} })); code(err) != hc.ErrorCodeInvalidInput {
		t.Errorf("volume of another location: %v", err)
	}
}

func TestNetworkZones(t *testing.T) {
	f := fake.New()
	nw := f.AddNetwork("eu", "10.0.0.0/16", nil, hc.NetworkZoneEUCentral)
	_, err := f.CreateServer(ctx, opts(f, "n", func(o *hc.ServerCreateOpts) {
		o.Networks = []*hc.Network{{ID: nw.ID}}
		o.Location = &hc.Location{Name: "ash"}
	}))
	if code(err) != hc.ErrorCodeNoSubnetAvailable {
		t.Errorf("no subnet in the zone of ash: %v", err)
	}
}

func TestPlacementGroupLimit(t *testing.T) {
	f := fake.New()
	pg, _ := f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "pg", Type: hc.PlacementGroupTypeSpread})
	for i := range fake.MaxSpread {
		if _, err := f.CreateServer(ctx, opts(f, fmt.Sprintf("n%d", i), func(o *hc.ServerCreateOpts) { o.PlacementGroup = &hc.PlacementGroup{ID: pg.ID} })); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.CreateServer(ctx, opts(f, "n11", func(o *hc.ServerCreateOpts) { o.PlacementGroup = &hc.PlacementGroup{ID: pg.ID} })); code(err) != hc.ErrorCodePlacementError {
		t.Errorf("eleventh member: %v", err)
	}
}

func TestImagesFilter(t *testing.T) {
	f := fake.New()
	x1, a1 := f.AddFlatcarImages("stable", "4757.2.3")
	_, _ = f.AddFlatcarImages("stable", "4700.1.0")
	f.AddImage(hc.ArchitectureX86, map[string]string{"os": "debian"})
	got, err := f.Images(ctx, hc.ImageListOpts{
		ListOpts: hc.ListOpts{LabelSelector: "os=flatcar,flatcar-channel=stable,flatcar-version=4757.2.3"},
		Type:     []hc.ImageType{hc.ImageTypeSnapshot}, Architecture: []hc.Architecture{hc.ArchitectureX86},
	})
	if err != nil || len(got) != 1 || got[0].ID != x1.ID {
		t.Errorf("%v %v", got, err)
	}
	got, _ = f.Images(ctx, hc.ImageListOpts{ListOpts: hc.ListOpts{LabelSelector: "os=flatcar,flatcar-version=4757.2.3"}, Architecture: []hc.Architecture{hc.ArchitectureARM}})
	if len(got) != 1 || got[0].ID != a1.ID {
		t.Error("arm")
	}
	if got, _ = f.Images(ctx, hc.ImageListOpts{ListOpts: hc.ListOpts{LabelSelector: "os"}}); len(got) != 5 {
		t.Errorf("presence: %d", len(got))
	}
}

func TestInjectionAndRateLimit(t *testing.T) {
	f := fake.New()
	f.Inject("Locations", fake.APIError(hc.ErrorCodeServerError, "boom"), 2)
	for range 2 {
		if _, err := f.Locations(ctx); code(err) != hc.ErrorCodeServerError {
			t.Fatal(err)
		}
	}
	if _, err := f.Locations(ctx); err != nil {
		t.Errorf("injection exhausted: %v", err)
	}
	if f.Calls("Locations") != 3 {
		t.Error("calls")
	}
	f.Inject("ServerTypes", fake.APIError(hc.ErrorCodeMaintenance, "m"), -1)
	for range 3 {
		if _, err := f.ServerTypes(ctx); code(err) != hc.ErrorCodeMaintenance {
			t.Fatal(err)
		}
	}
	f.ClearInjected()
	if _, err := f.ServerTypes(ctx); err != nil {
		t.Error(err)
	}
	rl := f.RateLimit()
	if rl.Limit != 3600 || rl.Remaining >= 3600 {
		t.Errorf("%+v", rl)
	}
	f.SetRateLimited(true)
	if _, err := f.Locations(ctx); code(err) != hc.ErrorCodeRateLimitExceeded {
		t.Errorf("429 mode: %v", err)
	}
	f.SetRateLimited(false)
	f.ResetRateLimit()
	if f.RateLimit().Remaining != 3600 {
		t.Error("refill")
	}
	for range 3600 {
		_, _ = f.GetServer(ctx, 1)
	}
	if _, err := f.GetServer(ctx, 1); code(err) != hc.ErrorCodeRateLimitExceeded {
		t.Errorf("budget exhausted: %v", err)
	}
}

func TestFailedCreateAction(t *testing.T) {
	f := fake.New()
	f.FailCreateAction("placement_error", "could not place")
	res, _ := f.CreateServer(ctx, opts(f, "n"))
	err := f.WaitForAction(ctx, res.Action)
	ae, ok := err.(hc.ActionError)
	if !ok || ae.Code != "placement_error" {
		t.Errorf("%v", err)
	}
	f.FailCreateAction("", "")
	res2, _ := f.CreateServer(ctx, opts(f, "n2"))
	if err := f.WaitForAction(ctx, res2.Action); err != nil {
		t.Error(err)
	}
}

func TestProtectedDelete(t *testing.T) {
	f := fake.New()
	res, _ := f.CreateServer(ctx, opts(f, "n"))
	f.ProtectServer(res.Server.ID, true)
	if _, err := f.DeleteServer(ctx, res.Server); code(err) != hc.ErrorCodeProtected {
		t.Error(err)
	}
	f.ProtectServer(res.Server.ID, false)
	if _, err := f.DeleteServer(ctx, res.Server); err != nil {
		t.Error(err)
	}
}

func TestSSHKeyLookup(t *testing.T) {
	f := fake.New()
	k := f.AddSSHKey("my-key")
	for _, q := range []string{"my-key", fmt.Sprint(k.ID)} {
		if got, _ := f.SSHKey(ctx, q); got == nil || got.ID != k.ID {
			t.Errorf("%s", q)
		}
	}
	if got, _ := f.SSHKey(ctx, "nope"); got != nil {
		t.Error("unknown key is nil")
	}
}

var _ clock.Clock = clocktesting.NewFakeClock(time.Now())

func TestFirewallRuleValidation(t *testing.T) {
	f := fake.New()
	src := []net.IPNet{{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}
	port := "22"
	for name, r := range map[string]hc.FirewallRule{
		"tcp without port":  {Direction: hc.FirewallRuleDirectionIn, Protocol: hc.FirewallRuleProtocolTCP, SourceIPs: src},
		"icmp with port":    {Direction: hc.FirewallRuleDirectionIn, Protocol: hc.FirewallRuleProtocolICMP, SourceIPs: src, Port: &port},
		"in without source": {Direction: hc.FirewallRuleDirectionIn, Protocol: hc.FirewallRuleProtocolTCP, Port: &port},
	} {
		_, err := f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: name, Rules: []hc.FirewallRule{r}})
		if code(err) != hc.ErrorCodeInvalidInput {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := hc.FirewallRule{Direction: hc.FirewallRuleDirectionIn, Protocol: hc.FirewallRuleProtocolTCP, SourceIPs: src, Port: &port}
	if _, err := f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "ok", Rules: []hc.FirewallRule{ok}}); err != nil {
		t.Error(err)
	}
}
