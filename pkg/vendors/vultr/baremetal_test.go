/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coreos/butane/config"
	butane "github.com/coreos/butane/config/common"
	"github.com/go-logr/logr"
	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

const bmPlan = "vbm-6c-32gb"

// bmEnv is an ops env with a bare-metal plan in stock in ewr.
func bmEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.acct.AddPlan(bmPlan, 6, 32768, 240)
	e.acct.SetAvailable("ewr", plan, "vc2-2c-4gb", bmPlan)
	e.ops.RegistrationTimeout = time.Hour
	return e
}

func (e *env) bmRequest(claim, uid string) vendors.CreateRequest {
	r := e.request(claim, uid)
	r.InstanceType = bmPlan
	r.Prepared = &vendors.Prepared{BootFragments: []vendors.BootFragment{{Name: vultr.HostFirewallFragmentName}}}
	return r
}

func TestBareMetalLaunchUsesTheBareMetalAPIAndStock(t *testing.T) {
	e := bmEnv(t)
	inst, err := e.ops.Launch(context.Background(), e.bmRequest("claim-1", "uid-1"), e.launchOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(inst.Ref), "ewr/bm:") || inst.Type != bmPlan || inst.State != vendors.StatePending {
		t.Fatalf("got %+v", inst)
	}
	if e.acct.Calls("CreateInstance") != 0 || len(e.acct.BareMetals()) != 1 || len(e.acct.Instances()) != 0 {
		t.Fatal("the server is not a bare-metal one")
	}
	body := e.acct.Inputs("CreateBareMetal")[0].(govultr.BareMetalCreate)
	if body.SnapshotID != e.snap || body.Hostname != "claim-1" || body.UserData == "" {
		t.Errorf("create body: %+v", body)
	}
	// Out of stock is capacity, found before the create.
	e.acct.SetAvailable("ewr", plan)
	e.clk.Step(time.Hour)
	_, err = e.ops.Launch(context.Background(), e.bmRequest("claim-2", "uid-2"), e.launchOpts())
	if !vendors.IsClass(err, vendors.InsufficientCapacity) || e.acct.Calls("CreateBareMetal") != 1 {
		t.Fatalf("out of stock: %v", err)
	}
}

func TestBareMetalLaunchNeedsTheHostFirewall(t *testing.T) {
	e := bmEnv(t)
	req := e.bmRequest("claim-1", "uid-1")
	for name, p := range map[string]*vendors.Prepared{"none": nil, "other fragment": {BootFragments: []vendors.BootFragment{{Name: "x"}}}} {
		req.Prepared = p
		_, err := e.ops.Launch(context.Background(), req, e.launchOpts())
		if !vendors.IsClass(err, vendors.Unsupported) || e.acct.Calls("CreateBareMetal") != 0 {
			t.Errorf("%s: %v, %d creates", name, err, e.acct.Calls("CreateBareMetal"))
		}
	}
}

func TestBareMetalLaunchIgnoresVPCs(t *testing.T) {
	e := bmEnv(t)
	lo := e.launchOpts()
	lo.VPCIDs = []string{"0c000000-0000-4000-8000-000000000003"}
	if _, err := e.ops.Launch(context.Background(), e.bmRequest("claim-1", "uid-1"), lo); err != nil {
		t.Fatal(err)
	}
}

func TestBareMetalReplayFindsTheServer(t *testing.T) {
	e := bmEnv(t)
	first, err := e.ops.Launch(context.Background(), e.bmRequest("claim-1", "uid-1"), e.launchOpts())
	if err != nil {
		t.Fatal(err)
	}
	// Another process has no cache: the list lookup finds it.
	other := vultr.NewOps(e.acct, cluster)
	other.Clock = e.clk
	other.RegistrationTimeout = time.Hour
	again, err := other.Launch(context.Background(), e.bmRequest("claim-1", "uid-1"), e.launchOpts())
	if err != nil || again.Ref != first.Ref || e.acct.Calls("CreateBareMetal") != 1 {
		t.Fatalf("replay: %+v %v, %d creates", again, err, e.acct.Calls("CreateBareMetal"))
	}
}

func TestRefsPickTheEndpoint(t *testing.T) {
	e := bmEnv(t)
	ctx := context.Background()
	bm, err := e.ops.Launch(ctx, e.bmRequest("claim-bm", "uid-bm"), e.launchOpts())
	if err != nil {
		t.Fatal(err)
	}
	in := e.launch(t, "claim-in", "uid-in")
	if strings.Contains(string(in.Ref), "bm:") {
		t.Fatalf("an instance ref changed: %s", in.Ref)
	}
	if got, err := e.ops.GetInstance(ctx, bm.Ref); err != nil || got.Ref != bm.Ref {
		t.Fatalf("get bare metal: %+v %v", got, err)
	}
	if got, err := e.ops.GetInstance(ctx, in.Ref); err != nil || got.Ref != in.Ref {
		t.Fatalf("get instance: %+v %v", got, err)
	}
	if e.acct.Calls("GetBareMetal") != 1 || e.acct.Calls("GetInstance") != 1 {
		t.Errorf("calls: bare metal %d, instance %d, want one each", e.acct.Calls("GetBareMetal"), e.acct.Calls("GetInstance"))
	}
	// A bare-metal ID is not looked up as an instance, and a bad one is NotFound.
	if _, err := e.ops.GetInstance(ctx, "ewr/bm:nope"); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("bad id: %v", err)
	}
	// A server is gone for the instance endpoint and vice versa.
	if _, err := e.ops.GetInstance(ctx, vendors.NewInstanceRef("ewr", strings.TrimPrefix(bm.Ref.ID(), "bm:"))); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("a server ID without the bm: marker was found as an instance: %v", err)
	}
}

func TestListMergesInstancesAndBareMetal(t *testing.T) {
	e := bmEnv(t)
	ctx := context.Background()
	in := e.launch(t, "claim-in", "uid-in")
	e.clk.Step(time.Minute)
	bm, _ := e.ops.Launch(ctx, e.bmRequest("claim-bm", "uid-bm"), e.launchOpts())
	e.acct.AddForeignInstance("ewr", plan, "team=infra")

	got, err := e.ops.ListInstances(ctx, []string{"ewr"}, map[vendors.TagKey]string{vendors.TagCluster: cluster})
	if err != nil || len(got) != 2 || got[0].Ref != in.Ref || got[1].Ref != bm.Ref {
		t.Fatalf("list: %+v %v", got, err)
	}
	found, err := e.ops.FindInstance(ctx, "ewr", map[vendors.TagKey]string{vendors.TagNodeClaim: "claim-bm"})
	if err != nil || found.Ref != bm.Ref {
		t.Fatalf("find: %+v %v", found, err)
	}
	e.acct.Advance()
	if st := mustGet(t, e, bm.Ref).State; st != vendors.StateRunning {
		t.Errorf("an active, running server is %s", st)
	}
}

func mustGet(t *testing.T, e *env, ref vendors.InstanceRef) *vendors.Instance {
	t.Helper()
	inst, err := e.ops.GetInstance(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestBareMetalDeleteHaltsFirst(t *testing.T) {
	e := bmEnv(t)
	ctx := context.Background()
	bm, _ := e.ops.Launch(ctx, e.bmRequest("claim-bm", "uid-bm"), e.launchOpts())
	e.acct.Advance()

	if err := e.ops.DeleteInstance(ctx, bm.Ref); err != nil {
		t.Fatal(err)
	}
	if len(e.acct.BareMetals()) != 0 || e.acct.Calls("HaltBareMetal") != 1 || len(e.acct.DeletedWhileRunning) != 0 {
		t.Fatalf("halts %d, deleted while running %v", e.acct.Calls("HaltBareMetal"), e.acct.DeletedWhileRunning)
	}
	// Gone is success, and another cluster's server is refused.
	if err := e.ops.DeleteInstance(ctx, bm.Ref); err != nil {
		t.Errorf("a gone server: %v", err)
	}
	foreign, _ := e.acct.CreateBareMetal(ctx, &govultr.BareMetalCreate{Region: "ewr", Plan: bmPlan, Tags: tags("other")})
	e.acct.Advance()
	err := e.ops.DeleteInstance(ctx, vendors.NewInstanceRef("ewr", "bm:"+foreign.ID))
	if !vendors.IsClass(err, vendors.Fatal) || len(e.acct.BareMetals()) != 1 {
		t.Errorf("a foreign server: %v", err)
	}
}

func tags(cluster string) []string {
	return []string{"scarpenter.sparecores.com/cluster=" + cluster}
}

func TestBareMetalDeleteIsSentWhateverTheHaltAnswered(t *testing.T) {
	e := bmEnv(t)
	ctx := context.Background()
	bm, _ := e.ops.Launch(ctx, e.bmRequest("claim-bm", "uid-bm"), e.launchOpts())
	// The halt of a server that is still installing is refused as locked (409):
	// the delete is sent anyway.
	if err := e.ops.DeleteInstance(ctx, bm.Ref); err != nil || len(e.acct.BareMetals()) != 0 || e.acct.Calls("DeleteBareMetal") != 1 {
		t.Fatalf("halt while installing: %v, %d deletes", err, e.acct.Calls("DeleteBareMetal"))
	}
	// Both fail: the halt's error is the one to retry on; a locked delete alone is retryable too.
	bm, _ = e.ops.Launch(ctx, e.bmRequest("claim-2", "uid-2"), e.launchOpts())
	e.acct.Advance()
	e.acct.FailNext("DeleteBareMetal", 2, &fake.Error{Status: http.StatusConflict, Message: "Server is currently locked"})
	e.acct.FailNext("HaltBareMetal", 1, &fake.Error{Status: http.StatusBadGateway, Message: "down"})
	if err := e.ops.DeleteInstance(ctx, bm.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("both failed: %v", err)
	}
	if err := e.ops.DeleteInstance(ctx, bm.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("locked delete: %v", err)
	}
	// A retryable halt failure still sends the delete.
	e.acct.FailNext("HaltBareMetal", 1, &fake.Error{Status: http.StatusServiceUnavailable, Message: "down"})
	if err := e.ops.DeleteInstance(ctx, bm.Ref); err != nil || len(e.acct.BareMetals()) != 0 {
		t.Fatalf("halt failed, delete sent: %v, %d servers", err, len(e.acct.BareMetals()))
	}
}

func TestBareMetalIsListedOnlyWhileEnabled(t *testing.T) {
	e := bmEnv(t)
	ctx := context.Background()
	if _, err := e.ops.Launch(ctx, e.bmRequest("claim-1", "uid-1"), e.launchOpts()); err != nil {
		t.Fatal(err)
	}
	before := e.acct.Calls("ListBareMetals")
	e.ops.RegistrationTimeout = 0
	e.acct.FailNext("ListBareMetals", -1, &fake.Error{Status: 500, Message: "down"})
	list, err := e.ops.ListInstances(ctx, nil, nil)
	if err != nil || len(list) != 0 || e.acct.Calls("ListBareMetals") != before {
		t.Fatalf("disabled: %v %v, %d bare-metal lists", list, err, e.acct.Calls("ListBareMetals")-before)
	}
	e.ops.RegistrationTimeout = time.Hour
	if _, err := e.ops.ListInstances(ctx, nil, nil); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("enabled, endpoint down: %v", err)
	}
}

// ---- the guard ----

func bmDriver(t *testing.T, e *drvEnv, window time.Duration) *vultr.Driver {
	t.Helper()
	d := vultr.New(vultr.WithAPI(e.acct))
	cfg := vendors.Config{ClusterName: testCluster, Clock: e.clk, Log: logr.Discard(), RegistrationTimeout: window}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBareMetalNeedsALongRegistrationWindow(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	typ := vendors.CatalogType{Vendor: "vultr", APIReference: "vbm-4c-32gb", Arch: "amd64"}
	for _, c := range []struct {
		window time.Duration
		ok     bool
	}{{0, false}, {15 * time.Minute, false}, {25*time.Minute - time.Second, false}, {25 * time.Minute, true}, {time.Hour, true}} {
		d := bmDriver(t, e, c.window)
		if ok, why := d.LaunchableType(typ); ok != c.ok || (!ok && !strings.Contains(why, "registration timeout")) {
			t.Errorf("window %s: LaunchableType = %v %q", c.window, ok, why)
		}
		types, err := d.LaunchableTypes(ctx, "ewr")
		if err != nil || (types["vbm-4c-32gb"] != nil) != c.ok {
			t.Errorf("window %s: offered %v, %v", c.window, types, err)
		}
		req := vendors.CreateRequest{
			Region: "ewr", Zone: "ewr", InstanceType: "vbm-4c-32gb", Image: vendors.Image{Ref: e.snap},
			UserData: []byte("{}"), Node: vendors.NodeInfo{Name: "n"}, IdempotencyKey: "u",
			Tags:     map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClaim: "n"},
			Prepared: &vendors.Prepared{BootFragments: []vendors.BootFragment{{Name: vultr.HostFirewallFragmentName}}},
		}
		_, err = d.CreateInstance(ctx, req)
		if c.ok != (err == nil) || (!c.ok && !vendors.IsClass(err, vendors.Unsupported)) {
			t.Errorf("window %s: create: %v", c.window, err)
		}
		_, err = d.Prepare(ctx, vendors.PrepareRequest{InstanceType: "vbm-4c-32gb", Resources: provider{}})
		if !c.ok && !vendors.IsClass(err, vendors.Unsupported) {
			t.Errorf("window %s: prepare: %v", c.window, err)
		}
		e.acct.ClearErrors()
		for _, b := range e.acct.BareMetals() {
			_ = e.acct.DeleteBareMetal(ctx, b.ID)
		}
		e.clk.Step(time.Hour)
	}
	// Plans that are not bare metal never care.
	if ok, _ := bmDriver(t, e, 0).LaunchableType(vendors.CatalogType{Vendor: "vultr", APIReference: "vc2-2c-4gb", Arch: "amd64"}); !ok {
		t.Error("a vc2 plan is refused for the window")
	}
}

// ---- the host firewall ----

func rule(ipType, proto, port, subnet string, size int) govultr.FirewallRule {
	return govultr.FirewallRule{Action: "accept", IPType: ipType, Protocol: proto, Port: port, Subnet: subnet, SubnetSize: size}
}

func TestHostFirewallFragmentTranslatesTheGroupsRules(t *testing.T) {
	frag, err := vultr.HostFirewallFragment([]govultr.FirewallRule{
		rule("v4", "tcp", "22", "203.0.113.7", 32),
		rule("v4", "tcp", "8000:9000", "10.0.0.5", 8),
		rule("v4", "udp", "1:65535", "0.0.0.0", 0),
		rule("v4", "icmp", "", "198.51.100.0", 24),
		rule("v6", "tcp", "443", "2001:db8::", 32),
		rule("v6", "icmp", "", "::", 0),
		rule("v4", "tcp", "", "192.0.2.0", 24),
		rule("v4", "gre", "", "192.0.2.1", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip saddr 203.0.113.7/32 tcp dport 22 accept",
		"ip saddr 10.0.0.0/8 tcp dport 8000-9000 accept", // masked
		"meta nfproto ipv4 udp dport 1-65535 accept",
		"ip saddr 198.51.100.0/24 meta l4proto icmp accept",
		"ip6 saddr 2001:db8::/32 tcp dport 443 accept",
		"meta nfproto ipv6 meta l4proto ipv6-icmp accept",
		"ip saddr 192.0.2.0/24 meta l4proto tcp accept",
		"ip saddr 192.0.2.1/32 meta l4proto gre accept",
		"policy drop",
		// Before the NAT of kube-proxy, so that NodePorts and hostPorts are covered.
		"chain prerouting {",
		"type filter hook prerouting priority mangle; policy drop;",
		`iifname != { @PUBLIC_IFS@ } accept`,
	} {
		if !strings.Contains(frag.Butane, want) {
			t.Errorf("the ruleset lacks %q", want)
		}
	}
	if strings.Contains(frag.Butane, "hook input") || strings.Contains(frag.Butane, "hook forward") {
		t.Error("the table hooks input or forward: forwarded NodePort traffic would be open")
	}
	testutil.Golden(t, "host_firewall_fragment.golden.yaml", []byte(frag.Butane))
	if frag.Name == "" {
		t.Error("the fragment has no name")
	}
	// It is a valid flatcar Butane config, in strict mode.
	_, rep, err := config.TranslateBytes([]byte(frag.Butane), butane.TranslateBytesOptions{})
	if err != nil || len(rep.Entries) > 0 {
		t.Fatalf("butane: %v %s", err, rep.String())
	}
}

func TestHostFirewallWithoutRulesDropsEverythingPublic(t *testing.T) {
	frag, err := vultr.HostFirewallFragment(nil)
	if err != nil || !strings.Contains(frag.Butane, "policy drop") || strings.Contains(frag.Butane, "saddr") {
		t.Fatalf("%v\n%s", err, frag.Butane)
	}
}

func TestHostFirewallRefusesWhatItCannotTranslate(t *testing.T) {
	bad := map[string]govultr.FirewallRule{
		"source":   {Action: "accept", IPType: "v4", Protocol: "tcp", Port: "22", Source: "cloudflare"},
		"action":   {Action: "drop", IPType: "v4", Protocol: "tcp", Port: "22", Subnet: "10.0.0.0", SubnetSize: 8},
		"protocol": rule("v4", "sctp", "22", "10.0.0.0", 8),
		"ip type":  rule("v5", "tcp", "22", "10.0.0.0", 8),
		"subnet":   rule("v4", "tcp", "22", "not-an-ip", 8),
		"family":   rule("v6", "tcp", "22", "10.0.0.0", 8),
		"port":     rule("v4", "tcp", "22; flush ruleset", "10.0.0.0", 8),
		"range":    rule("v4", "tcp", "9000:8000", "10.0.0.0", 8),
		"zero":     rule("v4", "tcp", "0", "10.0.0.0", 8),
	}
	for name, r := range bad {
		if _, err := vultr.HostFirewallFragment([]govultr.FirewallRule{r}); err == nil {
			t.Errorf("%s: translated", name)
		}
	}
}

// The rules a NodeClass with the pod and VPC CIDRs writes, read back the way the
// API answers (the CIDR also in source), translate into the golden ruleset.
func TestHostFirewallOfTheRulesScarpenterWrites(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	d := bmDriver(t, e, time.Hour)
	g, err := e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "g"})
	if err != nil {
		t.Fatal(err)
	}
	ssh, https := int32(22), int32(443)
	reqs, err := vultr.FirewallRules(&common.Inbound{Rules: []common.InboundRule{
		{Protocol: "any", CIDRs: []string{"10.32.0.0/14", "10.0.0.0/20", "fd00:32::/48"}},
		{Protocol: "tcp", FromPort: &ssh, CIDRs: []string{"203.0.113.0/24"}},
		{Protocol: "tcp", FromPort: &https, CIDRs: []string{"0.0.0.0/0", "::/0"}},
		{Protocol: "icmp", CIDRs: []string{"0.0.0.0/0"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range reqs {
		if _, err := e.acct.CreateFirewallRule(ctx, g.ID, &reqs[i]); err != nil {
			t.Fatal(err)
		}
	}
	rules, _, err := e.acct.ListFirewallRules(ctx, g.ID, &govultr.ListOptions{PerPage: 100})
	if err != nil || len(rules) != len(reqs) || rules[0].Source != "10.32.0.0/14" {
		t.Fatalf("the fake does not answer the real shape: %+v %v", rules, err)
	}
	res := provider{vultr.KindFirewallGroups: {{Kind: vultr.KindFirewallGroups, ID: g.ID}}}
	req := prepareReq(res, common.Networking{})
	req.InstanceType = "vbm-4c-32gb"
	prep, err := d.Prepare(ctx, req)
	if err != nil || len(prep.BootFragments) != 1 {
		t.Fatalf("%+v %v", prep, err)
	}
	testutil.Golden(t, "host_firewall_scarpenter_rules.golden.yaml", []byte(prep.BootFragments[0].Butane))
}

// A source that is not a CIDR, or that disagrees with the subnet, is refused; a
// CIDR in source alone is read.
func TestHostFirewallSources(t *testing.T) {
	r := rule("v4", "tcp", "22", "10.32.0.0", 14)
	r.Source = "10.32.0.0/14"
	if frag, err := vultr.HostFirewallFragment([]govultr.FirewallRule{r}); err != nil ||
		!strings.Contains(frag.Butane, "ip saddr 10.32.0.0/14 tcp dport 22 accept") {
		t.Errorf("source and subnet: %v", err)
	}
	r = govultr.FirewallRule{Action: "accept", IPType: "v4", Protocol: "tcp", Port: "22", Source: "10.32.0.0/14"}
	if frag, err := vultr.HostFirewallFragment([]govultr.FirewallRule{r}); err != nil ||
		!strings.Contains(frag.Butane, "ip saddr 10.32.0.0/14 tcp dport 22 accept") {
		t.Errorf("source alone: %v", err)
	}
	// Host bits in the source mask to the subnet.
	r = rule("v4", "tcp", "22", "10.32.0.0", 14)
	r.Source = "10.33.1.1/14"
	if frag, err := vultr.HostFirewallFragment([]govultr.FirewallRule{r}); err != nil ||
		!strings.Contains(frag.Butane, "ip saddr 10.32.0.0/14 tcp dport 22 accept") {
		t.Errorf("source with host bits: %v", err)
	}
	v6 := govultr.FirewallRule{Action: "accept", IPType: "v6", Protocol: "tcp", Port: "22", Source: "10.32.0.0/14"}
	for name, src := range map[string]string{
		"cloudflare": "cloudflare", "other subnet": "192.0.2.0/24", "load balancer": "6b1d7f0e-5a0c-4c7e-9a52-2f0c1f5b8a11",
	} {
		r := rule("v4", "tcp", "22", "10.32.0.0", 14)
		r.Source = src
		if _, err := vultr.HostFirewallFragment([]govultr.FirewallRule{r}); err == nil {
			t.Errorf("%s: translated", name)
		}
	}
	if _, err := vultr.HostFirewallFragment([]govultr.FirewallRule{v6}); err == nil {
		t.Error("a v4 source of a v6 rule: translated")
	}
}

func TestPrepareAddsTheHostFirewallOfABareMetalLaunch(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	d := bmDriver(t, e, time.Hour)
	g, err := e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.acct.CreateFirewallRule(ctx, g.ID, &govultr.FirewallRuleReq{
		IPType: "v4", Protocol: "tcp", Port: "22", Subnet: "203.0.113.0", SubnetSize: 24,
	}); err != nil {
		t.Fatal(err)
	}
	res := provider{vultr.KindFirewallGroups: {{Kind: vultr.KindFirewallGroups, ID: g.ID}}}
	// The default isolation needs a VPC for instances; a bare-metal server has none.
	req := prepareReq(res, common.Networking{})
	req.InstanceType = "vbm-4c-32gb"
	prep, err := d.Prepare(ctx, req)
	if err != nil || len(prep.BootFragments) != 1 || !strings.Contains(prep.BootFragments[0].Butane, "ip saddr 203.0.113.0/24 tcp dport 22 accept") {
		t.Fatalf("%+v %v", prep, err)
	}
	if len(prep.Resources) != 1 {
		t.Errorf("resources %+v, want the firewall group only", prep.Resources)
	}
	// An instance plan gets none, and needs its VPC.
	req.InstanceType = testType
	req.Settings.Networking.Isolation = common.IsolationNone
	if prep, err = d.Prepare(ctx, req); err != nil || len(prep.BootFragments) != 0 {
		t.Fatalf("an instance: %+v %v", prep, err)
	}

	// Rules that cannot be read, or translated, fail the launch.
	req.InstanceType = "vbm-4c-32gb"
	e.acct.FailNext("ListFirewallRules", 1, &fake.Error{Status: 500, Message: "down"})
	if _, err := d.Prepare(ctx, req); err == nil || !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("unreadable rules: %v", err)
	}
	e.acct.AddRules(g.ID, govultr.FirewallRule{IPType: "v4", Protocol: "tcp", Port: "22", Source: "cloudflare"})
	if _, err := d.Prepare(ctx, req); err == nil || !vendors.IsClass(err, vendors.InvalidOffering) || errors.Is(err, context.Canceled) {
		t.Errorf("untranslatable rules: %v", err)
	}
}
