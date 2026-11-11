/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vultr/govultr/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

func managedSpec(name string, kind vendors.ResourceKind, params json.RawMessage) vendors.CreateSpec {
	return vendors.CreateSpec{
		Name: name, Params: params,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: testCluster, vendors.TagNodeClass: "default", vendors.TagManagedBy: vendors.ManagedByValue,
			vendors.TagResource: string(kind),
		},
	}
}

func inboundParams(t *testing.T, rules ...common.InboundRule) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(vultr.FirewallGroupParams{Inbound: &common.Inbound{Rules: rules}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func port(p int32) *int32 { return new(p) }

func TestKindsAndSelectorsAgree(t *testing.T) {
	d := vultr.New()
	caps := d.Selectors()
	for _, k := range d.Kinds() {
		if _, ok := caps[k.Kind]; !ok {
			t.Errorf("the kind %s has no selector declaration", k.Kind)
		}
	}
	if _, ok := caps[selector.ImagesKind]; !ok {
		t.Error("no images declaration")
	}
	for kind, c := range caps {
		if c.Tags != vendors.TagsNone {
			t.Errorf("%s: Vultr snapshots, groups and VPCs have no tags", kind)
		}
	}
	if !caps[vultr.KindImages].Global || !caps[vultr.KindFirewallGroups].Global || caps[vultr.KindNetworks].Global {
		t.Error("snapshots and firewall groups are global in the account, VPCs are regional")
	}
}

func TestEnsureFirewallGroupCreatesItsRulesAndIsIdempotent(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	params := inboundParams(t, common.InboundRule{Protocol: "tcp", FromPort: port(22), CIDRs: []string{"192.0.2.0/24"}})
	spec := managedSpec("scarpenter-my-cluster-firewall-groups-ewr-abc", vultr.KindFirewallGroups, params)

	ref, created, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec)
	if err != nil || !created || !ref.Managed || ref.Region != "ewr" || ref.Name != spec.Name || ref.Tags[vendors.TagNodeClass] != "default" {
		t.Fatalf("ensure: %+v, %v, %v", ref, created, err)
	}
	g := e.acct.Groups()[0]
	if g.RuleCount != 1 || !strings.HasPrefix(g.Description, spec.Name+" | cluster=my-cluster") || !strings.HasSuffix(g.Description, "region=ewr") {
		t.Fatalf("group: %+v", g)
	}
	again, created, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec)
	if err != nil || created || again.ID != ref.ID || e.acct.Calls("CreateFirewallGroup") != 1 || e.acct.Calls("CreateFirewallRule") != 1 {
		t.Fatalf("second ensure: %+v, %v, %v", again, created, err)
	}

	// A group whose creation stopped halfway gets its rules; a changed rule set
	// is synced (the old rule goes, the new one comes).
	params2 := inboundParams(t, common.InboundRule{Protocol: "any", CIDRs: []string{"10.0.0.0/8", "2001:db8::/32"}})
	spec2 := spec
	spec2.Params = params2
	if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec2); err != nil {
		t.Fatal(err)
	}
	if got := e.acct.Groups()[0].RuleCount; got != 6 { // tcp, udp and icmp for each of the two CIDRs
		t.Fatalf("%d rules", got)
	}
	spec2.Params = nil // no inbound rules: deny all
	if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec2); err != nil || e.acct.Groups()[0].RuleCount != 0 {
		t.Fatalf("deny all: %v, %d rules", err, e.acct.Groups()[0].RuleCount)
	}
}

func TestEnsureRefusesWhatItDoesNotOwn(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	// The name is ours, the cluster is someone else's.
	other := managedSpec("scarpenter-other-firewall-groups-ewr", vultr.KindFirewallGroups, nil)
	other.Tags[vendors.TagCluster] = "other"
	if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", other); err != nil {
		t.Fatal(err)
	}
	mine := managedSpec("scarpenter-other-firewall-groups-ewr", vultr.KindFirewallGroups, nil)
	if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", mine); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("adopted another cluster's group: %v", err)
	}
	vpcOther := managedSpec("scarpenter-other-networks-ewr", vultr.KindNetworks, nil)
	vpcOther.Tags[vendors.TagCluster] = "other"
	if _, _, err := e.d.Ensure(ctx, vultr.KindNetworks, "ewr", vpcOther); err != nil {
		t.Fatal(err)
	}
	vpcMine := managedSpec("scarpenter-other-networks-ewr", vultr.KindNetworks, nil)
	if _, _, err := e.d.Ensure(ctx, vultr.KindNetworks, "ewr", vpcMine); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("adopted another cluster's VPC: %v", err)
	}

	for name, spec := range map[string]vendors.CreateSpec{
		"no name":    {},
		"a bad name": managedSpec("two words", vultr.KindFirewallGroups, nil),
		"bad params": managedSpec("x", vultr.KindFirewallGroups, json.RawMessage(`{`)),
		"bad rules":  managedSpec("x", vultr.KindFirewallGroups, inboundParams(t, common.InboundRule{Protocol: "tcp", CIDRs: []string{"nope"}})),
		"no cluster": {Name: "x"},
	} {
		if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec); !vendors.IsClass(err, vendors.Fatal) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := e.d.Ensure(ctx, "elasticIPs", "ewr", managedSpec("x", "elasticIPs", nil)); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("an unknown kind: %v", err)
	}
}

func TestEnsureErrorsAreClassified(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	spec := managedSpec("scarpenter-my-cluster-firewall-groups-ewr", vultr.KindFirewallGroups,
		inboundParams(t, common.InboundRule{Protocol: "tcp", FromPort: port(22), CIDRs: []string{"192.0.2.0/24"}}))
	e.acct.FailNext("CreateFirewallGroup", 1, &fake.Error{Status: 400, Message: "Firewall group limit reached."})
	if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec); !vendors.IsClass(err, vendors.QuotaExceeded) || !vendors.IsAccountWide(err) {
		t.Fatalf("group limit: %v", err)
	}
	e.acct.FailNext("CreateFirewallRule", 1, &fake.Error{Status: 503, Message: "down"})
	if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("a rule that fails: %v", err)
	}
	// The group exists without its rule; the next call completes it.
	ref, created, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec)
	if err != nil || created || e.acct.Groups()[0].RuleCount != 1 || ref.ID == "" {
		t.Fatalf("repair: %+v, %v, %v", ref, created, err)
	}
	e.acct.FailNext("CreateVPC", 1, &fake.Error{Status: 400, Message: "VPC limit reached."})
	if _, _, err := e.d.Ensure(ctx, vultr.KindNetworks, "ewr", managedSpec("scarpenter-my-cluster-networks-ewr", vultr.KindNetworks, nil)); !vendors.IsClass(err, vendors.QuotaExceeded) {
		t.Fatalf("VPC limit: %v", err)
	}
	e.acct.FailNext("ListFirewallGroups", 1, &fake.Error{Status: 503, Message: "down"})
	if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", spec); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("list: %v", err)
	}
	e.acct.FailNext("ListVPCs", 1, &fake.Error{Status: 503, Message: "down"})
	if _, _, err := e.d.Ensure(ctx, vultr.KindNetworks, "ewr", managedSpec("x", vultr.KindNetworks, nil)); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("list VPCs: %v", err)
	}
}

func TestListFiltersByRegionAndMarker(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	for _, r := range []string{"ewr", "fra"} {
		if _, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, r, managedSpec("scarpenter-my-cluster-firewall-groups-"+r, vultr.KindFirewallGroups, nil)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.d.Ensure(ctx, vultr.KindNetworks, r, managedSpec("scarpenter-my-cluster-networks-"+r, vultr.KindNetworks, nil)); err != nil {
			t.Fatal(err)
		}
	}
	// Things that are not ours: a user's group, a group of another cluster, and a
	// group with an older marker without a region.
	_, _ = e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "my own group"})
	_, _ = e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{
		Description: "scarpenter-other-firewall-groups-ewr | cluster=other nodeclass=default managed-by=scarpenter resource-kind=firewallGroups region=ewr"})
	_, _ = e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{
		Description: "scarpenter-my-cluster-firewall-groups-old | cluster=my-cluster nodeclass=default managed-by=scarpenter resource-kind=firewallGroups"})

	cluster := map[vendors.TagKey]string{vendors.TagCluster: testCluster}
	all, err := e.d.List(ctx, nil, cluster) // the older marker has no region: listed, but in no region
	if err != nil || len(all) != 5 {
		t.Fatalf("all: %+v, %v", all, err)
	}
	ewr, err := e.d.List(ctx, []string{"ewr"}, cluster)
	if err != nil || len(ewr) != 2 || ewr[0].Region != "ewr" || ewr[1].Region != "ewr" {
		t.Fatalf("ewr: %+v, %v", ewr, err)
	}
	if got, err := e.d.List(ctx, nil, map[vendors.TagKey]string{vendors.TagResource: "networks"}); err != nil || len(got) != 2 {
		t.Fatalf("networks: %+v, %v", got, err)
	}
	e.acct.FailNext("ListVPCs", 1, &fake.Error{Status: 503, Message: "down"})
	if _, err := e.d.List(ctx, nil, cluster); err == nil {
		t.Fatal("a failing VPC list was swallowed")
	}
	e.acct.FailNext("ListFirewallGroups", 1, &fake.Error{Status: 503, Message: "down"})
	if _, err := e.d.List(ctx, nil, cluster); err == nil {
		t.Fatal("a failing group list was swallowed")
	}
}

func TestDeleteTouchesOnlyWhatItOwns(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	mine, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", managedSpec("scarpenter-my-cluster-firewall-groups-ewr", vultr.KindFirewallGroups, nil))
	if err != nil {
		t.Fatal(err)
	}
	user, err := e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "the operator's group"})
	if err != nil {
		t.Fatal(err)
	}
	userVPC, _ := e.acct.CreateVPC(ctx, &govultr.VPCReq{Region: "ewr", Description: "the operator's VPC"})
	if err := e.d.Delete(ctx, vendors.ResourceRef{Kind: vultr.KindFirewallGroups, ID: user.ID}); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("deleted a user's group: %v", err)
	}
	if err := e.d.Delete(ctx, vendors.ResourceRef{Kind: vultr.KindNetworks, ID: userVPC.ID}); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("deleted a user's VPC: %v", err)
	}
	// A managed group of the right marker but another kind is not a VPC.
	if err := e.d.Delete(ctx, vendors.ResourceRef{Kind: vultr.KindNetworks, ID: mine.ID}); !vendors.IsClass(err, vendors.NotFound) && err != nil {
		t.Logf("a group asked for as a VPC: %v", err)
	}
	if err := e.d.Delete(ctx, vendors.ResourceRef{Kind: "elasticIPs", ID: "x"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("unknown kind: %v", err)
	}
	for i := range 2 {
		if err := e.d.Delete(ctx, mine); err != nil {
			t.Fatalf("delete #%d: %v", i+1, err)
		}
	}
	if used, _, err := e.d.InUse(ctx, mine); !vendors.IsClass(err, vendors.NotFound) || used {
		t.Fatalf("InUse of a deleted group: %v, %v", used, err)
	}
	if _, _, err := e.d.InUse(ctx, vendors.ResourceRef{Kind: vultr.KindNetworks, ID: "00000000-0000-4000-8000-000000000000"}); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("InUse of a missing VPC: %v", err)
	}
	if _, _, err := e.d.InUse(ctx, vendors.ResourceRef{Kind: "elasticIPs"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("InUse of an unknown kind: %v", err)
	}
	e.acct.FailNext("GetFirewallGroup", 1, &fake.Error{Status: 503, Message: "down"})
	if err := e.d.Delete(ctx, mine); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("a failing get: %v", err)
	}
}

func TestResolveFirewallGroups(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e.acct.SetClock(func() time.Time { return t0 })
	a, _ := e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "prod-nodes-a"})
	e.acct.SetClock(func() time.Time { return t0.Add(time.Hour) })
	b, _ := e.acct.CreateFirewallGroup(ctx, &govultr.FirewallGroupReq{Description: "prod-nodes-b"})
	_, _ = e.acct.CreateFirewallRule(ctx, b.ID, &govultr.FirewallRuleReq{IPType: "v4", Protocol: "tcp", Subnet: "0.0.0.0", SubnetSize: 0, Port: "22"})
	res := func(u vendors.UseRef) ([]vendors.ResourceRef, error) {
		return e.d.Resolve(ctx, vultr.KindFirewallGroups, "ewr", u)
	}

	if got, err := res(vendors.UseRef{IDs: []string{a.ID}, Select: common.SelectExactlyOne}); err != nil || len(got) != 1 || got[0].ID != a.ID ||
		got[0].Managed || got[0].Region != "ewr" || got[0].Name != "prod-nodes-a" {
		t.Fatalf("by ID: %+v, %v", got, err)
	}
	if got, err := res(vendors.UseRef{Name: "prod-nodes-*", Select: common.SelectAll}); err != nil || len(got) != 2 {
		t.Fatalf("by name: %+v, %v", got, err)
	}
	if got, err := res(vendors.UseRef{Name: "prod-nodes-*", Select: common.SelectNewest}); err != nil || len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("newest: %+v, %v", got, err)
	}
	after := metav1.Time{Time: t0.Add(30 * time.Minute)}
	if got, err := res(vendors.UseRef{Name: "prod-*", Match: common.ResourceMatch{CreatedAfter: &after}}); err != nil || len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("createdAfter: %+v, %v", got, err)
	}
	if got, err := res(vendors.UseRef{Name: "prod-*", Filters: []common.ResourceFilter{{Name: "rule_count", Values: []string{"1", "2"}}}}); err != nil || len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("filter: %+v, %v", got, err)
	}
	if _, err := res(vendors.UseRef{Name: "prod-*"}); err == nil {
		t.Fatal("two groups under exactlyOne")
	} else if _, ok := errors.AsType[*selector.AmbiguousError](err); !ok {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := res(vendors.UseRef{Name: "nothing"}); err == nil {
		t.Fatal("no match")
	} else if _, ok := errors.AsType[*selector.NoMatchError](err); !ok {
		t.Fatalf("no match: %v", err)
	}
	if _, err := res(vendors.UseRef{}); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("an empty selector: %v", err)
	}
	if _, err := e.d.Resolve(ctx, "elasticIPs", "ewr", vendors.UseRef{Name: "x"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("an unknown kind: %v", err)
	}
	e.acct.FailNext("ListFirewallGroups", 1, &fake.Error{Status: 503, Message: "down"})
	if _, err := res(vendors.UseRef{Name: "x"}); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("a failing list: %v", err)
	}
}

func TestResolveVPCsAreMatchedToTheRegion(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	ewr, _ := e.acct.CreateVPC(ctx, &govultr.VPCReq{Region: "ewr", Description: "private-ewr", V4Subnet: "10.10.0.0", V4SubnetMask: 24})
	_, _ = e.acct.CreateVPC(ctx, &govultr.VPCReq{Region: "fra", Description: "private-fra", V4Subnet: "10.20.0.0", V4SubnetMask: 24})
	res := func(region string, u vendors.UseRef) ([]vendors.ResourceRef, error) {
		return e.d.Resolve(ctx, vultr.KindNetworks, region, u)
	}
	if got, err := res("ewr", vendors.UseRef{Name: "private-*"}); err != nil || len(got) != 1 || got[0].ID != ewr.ID || got[0].Network != ewr.ID || got[0].Region != "ewr" {
		t.Fatalf("by name: %+v, %v", got, err)
	}
	if _, err := res("fra", vendors.UseRef{IDs: []string{ewr.ID}}); err == nil {
		t.Fatal("a VPC of another region matched")
	}
	cidr := common.ResourceMatch{CIDR: "10.10.0.0/24"}
	if got, err := res("ewr", vendors.UseRef{Match: cidr}); err != nil || len(got) != 1 {
		t.Fatalf("cidr: %+v, %v", got, err)
	}
	within := common.ResourceMatch{Within: "10.0.0.0/8"}
	if got, err := res("fra", vendors.UseRef{Match: within}); err != nil || len(got) != 1 || got[0].Name != "private-fra" {
		t.Fatalf("within: %+v, %v", got, err)
	}
	if _, err := res("ewr", vendors.UseRef{Match: common.ResourceMatch{CIDR: "10.99.0.0/24"}}); err == nil {
		t.Fatal("another range matched")
	}
	e.acct.FailNext("ListVPCs", 1, &fake.Error{Status: 503, Message: "down"})
	if _, err := res("ewr", vendors.UseRef{Name: "x"}); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("a failing list: %v", err)
	}
}

func TestVPCInUseFollowsItsAttachments(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	v, _, err := e.d.Ensure(ctx, vultr.KindNetworks, "ewr", managedSpec("scarpenter-my-cluster-networks-ewr", vultr.KindNetworks, nil))
	if err != nil {
		t.Fatal(err)
	}
	if used, _, err := e.d.InUse(ctx, v); err != nil || used {
		t.Fatalf("a fresh VPC: %v, %v", used, err)
	}
	in, err := e.acct.CreateInstance(ctx, &govultr.InstanceCreateReq{Region: "ewr", Plan: testType, AttachVPC: []string{v.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if used, last, err := e.d.InUse(ctx, v); err != nil || !used || last.IsZero() {
		t.Fatalf("attached: %v, %v, %v", used, last, err)
	}
	if err := e.d.Delete(ctx, v); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("delete while attached: %v", err)
	}
	if err := e.acct.DeleteInstance(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Delete(ctx, v); err != nil {
		t.Fatal(err)
	}
	e.acct.FailNext("ListVPCAttachments", 1, &fake.Error{Status: 503, Message: "down"})
	v2, _, _ := e.d.Ensure(ctx, vultr.KindNetworks, "ewr", managedSpec("scarpenter-my-cluster-networks-ewr-2", vultr.KindNetworks, nil))
	if _, _, err := e.d.InUse(ctx, v2); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("a failing attachments list: %v", err)
	}
}
