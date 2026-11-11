/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

var marker = map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClass: "class"}

func spec(name string, params string) vendors.CreateSpec {
	s := vendors.CreateSpec{Name: name, Tags: marker}
	if params != "" {
		s.Params = json.RawMessage(params)
	}
	return s
}

func addRule(t *testing.T, e *env, name, network string, targetTags []string, disabled bool, desc string) {
	t.Helper()
	op, err := e.c.InsertFirewall(t.Context(), &computepb.InsertFirewallRequest{Project: testProject,
		FirewallResource: &computepb.Firewall{Name: &name, Network: &network, TargetTags: targetTags, Disabled: &disabled,
			Direction: new("INGRESS"), Description: &desc, Allowed: []*computepb.Allowed{{IPProtocol: new("all")}}}})
	if err != nil || op == nil {
		t.Fatal(err)
	}
}

func TestResolveSubnets(t *testing.T) {
	e := newEnv(t, envOptions{})
	id := gcp.SubnetworkURL(testProject, testRegion, fake.SeedSubnet)
	for name, ref := range map[string]vendors.UseRef{
		"a full ID":     {IDs: []string{id}},
		"a short ID":    {IDs: []string{fake.SeedSubnet}},
		"a name":        {Name: "no*"},
		"an exact name": {Name: fake.SeedSubnet},
		"a network":     {Match: common.ResourceMatch{Network: []string{"vpc"}}},
		"a cidr":        {Match: common.ResourceMatch{CIDR: fake.SeedSubnetCIDR}},
		"within":        {Match: common.ResourceMatch{Within: "10.240.0.0/12"}},
	} {
		got, err := e.d.Resolve(t.Context(), gcp.KindSubnets, testRegion, ref)
		if err != nil || len(got) != 1 || got[0].ID != id || got[0].Network != "vpc" || got[0].Managed {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	_, err := e.d.Resolve(t.Context(), gcp.KindSubnets, testRegion, vendors.UseRef{IDs: []string{"nope"}})
	var nm *selector.NoMatchError
	if !errors.As(err, &nm) || len(nm.Missing) != 1 {
		t.Errorf("a missing subnetwork: %v", err)
	}
	if _, err := e.d.Resolve(t.Context(), gcp.KindSubnets, testRegion, vendors.UseRef{}); err == nil {
		t.Error("an empty selector resolved")
	}
	if _, err := e.d.Resolve(t.Context(), "volumes", testRegion, vendors.UseRef{Name: "x"}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an unknown kind: %v", err)
	}
	// The filters go to the API as equalities.
	if _, err := e.d.Resolve(t.Context(), gcp.KindSubnets, testRegion, vendors.UseRef{
		Name: "nodes", Filters: []common.ResourceFilter{{Name: "name", Values: []string{"nodes"}}}}); err != nil {
		t.Errorf("a native filter: %v", err)
	}
}

func TestResolveFirewalls(t *testing.T) {
	e := newEnv(t, envOptions{})
	net := "projects/" + testProject + "/global/networks/vpc"
	addRule(t, e, "allow-nodes", net, []string{"nodes", "web"}, false, "")
	addRule(t, e, "allow-off", net, []string{"off"}, true, "")
	addRule(t, e, "allow-all", net, nil, false, "")
	for name, ref := range map[string]vendors.UseRef{
		"an ID": {IDs: []string{"nodes"}}, "a name": {Name: "nod*"}, "a network": {IDs: []string{"nodes"},
			Match: common.ResourceMatch{Network: []string{"vpc"}}},
	} {
		got, err := e.d.Resolve(t.Context(), gcp.KindFirewalls, testRegion, ref)
		if err != nil || len(got) != 1 || got[0].ID != "nodes" || got[0].Network != "vpc" {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	// A disabled rule does not make the tag available.
	if _, err := e.d.Resolve(t.Context(), gcp.KindFirewalls, testRegion, vendors.UseRef{IDs: []string{"off"}}); err == nil {
		t.Error("a tag of a disabled rule resolved")
	}
	all, err := e.d.Resolve(t.Context(), gcp.KindFirewalls, testRegion, vendors.UseRef{Name: "*", Select: common.SelectAll})
	if err != nil || len(all) != 2 {
		t.Errorf("every available tag = %+v, %v", all, err)
	}
}

func TestResolveResourcePolicies(t *testing.T) {
	e := newEnv(t, envOptions{})
	ref, _, err := e.d.Ensure(t.Context(), gcp.KindResourcePolicies, testRegion, spec("scarpenter-conformance-resourcePolicies-europe-west3", `{"availabilityDomainCount":2}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, use := range map[string]vendors.UseRef{
		"an ID": {IDs: []string{ref.ID}}, "a short ID": {IDs: []string{ref.Name}}, "a name": {Name: "scarpenter-*"},
	} {
		got, err := e.d.Resolve(t.Context(), gcp.KindResourcePolicies, testRegion, use)
		if err != nil || len(got) != 1 || got[0].ID != ref.ID || got[0].Managed {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	if _, err := e.d.Resolve(t.Context(), gcp.KindResourcePolicies, testRegion, vendors.UseRef{IDs: []string{"nope"}}); err == nil {
		t.Error("a missing policy resolved")
	}
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindResourcePolicies, testRegion, spec("p-nothing", `{}`)); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a policy without a shape: %v", err)
	}
}

func TestDefaultNetwork(t *testing.T) {
	e := newEnv(t, envOptions{})
	defs, err := e.d.Default(t.Context(), gcp.KindSubnets, testRegion)
	if err != nil || len(defs) != 1 || defs[0].Name != "default" || defs[0].Network != "default" {
		t.Fatalf("Default = %+v, %v", defs, err)
	}
	if none, err := e.d.Default(t.Context(), gcp.KindFirewalls, testRegion); err != nil || len(none) != 0 {
		t.Errorf("firewalls have a default: %v, %v", none, err)
	}
	bare := newEnv(t, envOptions{bare: true})
	if none, err := bare.d.Default(t.Context(), gcp.KindSubnets, testRegion); err != nil || len(none) != 0 {
		t.Errorf("a project without a default network: %v, %v", none, err)
	}
	// The default network's open rules: it cannot honour isolation, so it is skipped.
	ok, why, err := e.d.DefaultSuits(t.Context(), gcp.KindSubnets, testRegion, defs, e.d.Defaults())
	if err != nil || !ok {
		t.Errorf("the default without rules: %v, %q, %v", ok, why, err)
	}
	addRule(t, e, "default-allow-internal", "projects/"+testProject+"/global/networks/default", nil, false, "")
	ok, why, err = e.d.DefaultSuits(t.Context(), gcp.KindSubnets, testRegion, defs, e.d.Defaults())
	if err != nil || ok || !strings.Contains(why, "default-allow-internal") {
		t.Errorf("the default with an open rule: %v, %q, %v", ok, why, err)
	}
	v6 := e.d.Defaults()
	v6.Networking.PublicIPv6 = new(true)
	if ok, why, _ := e.d.DefaultSuits(t.Context(), gcp.KindSubnets, testRegion, defs, v6); ok || !strings.Contains(why, "IPv6") {
		t.Errorf("the default with IPv6: %v, %q", ok, why)
	}
	if ok, _, _ := e.d.DefaultSuits(t.Context(), gcp.KindFirewalls, testRegion, nil, v6); !ok {
		t.Error("only subnets are checked")
	}
}

func TestManagedNetworkLifecycle(t *testing.T) {
	e := newEnv(t, envOptions{})
	name := "scarpenter-conformance-subnets-europe-west3"
	ref, created, err := e.d.Ensure(t.Context(), gcp.KindSubnets, testRegion, spec(name, ""))
	if err != nil || !created || ref.Network != name || !ref.Managed {
		t.Fatalf("Ensure = %+v, %v, %v", ref, created, err)
	}
	// The network of an interrupted creation (no subnetwork yet) is adopted and finished.
	e2 := newEnv(t, envOptions{})
	_, _ = e2.c.InsertNetwork(t.Context(), &computepb.InsertNetworkRequest{Project: testProject, NetworkResource: &computepb.Network{
		Name: &name, Description: new(gcp.MarkerDescription(testCluster, "class", "subnets") + " region=" + testRegion)}})
	listed, err := e2.d.List(t.Context(), []string{testRegion}, nil)
	if err != nil || len(listed) != 1 || listed[0].Kind != gcp.KindSubnets || !strings.Contains(listed[0].ID, "/global/networks/") {
		t.Fatalf("an orphan network is listed as %+v, %v", listed, err)
	}
	if _, created, err := e2.d.Ensure(t.Context(), gcp.KindSubnets, testRegion, spec(name, "")); err != nil || !created {
		t.Errorf("finishing the creation: %v, %v", created, err)
	}
	// The orphan network is deleted on its own.
	e3 := newEnv(t, envOptions{})
	_, _ = e3.c.InsertNetwork(t.Context(), &computepb.InsertNetworkRequest{Project: testProject, NetworkResource: &computepb.Network{
		Name: &name, Description: new(gcp.MarkerDescription(testCluster, "class", "subnets") + " region=" + testRegion)}})
	orphan, _ := e3.d.List(t.Context(), nil, nil)
	if err := e3.d.Delete(t.Context(), orphan[0]); err != nil {
		t.Fatal(err)
	}
	if l, _ := e3.d.List(t.Context(), nil, nil); len(l) != 0 {
		t.Errorf("the orphan network is still listed: %v", l)
	}
	// What is not ours is never touched: a network with the name but no marker.
	e4 := newEnv(t, envOptions{})
	_, _ = e4.c.InsertNetwork(t.Context(), &computepb.InsertNetworkRequest{Project: testProject, NetworkResource: &computepb.Network{
		Name: &name, Description: new("mine")}})
	if _, _, err := e4.d.Ensure(t.Context(), gcp.KindSubnets, testRegion, spec(name, "")); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a foreign network of the name: %v, want Fatal", err)
	}
	if err := e4.d.Delete(t.Context(), vendors.ResourceRef{Kind: gcp.KindSubnets, Region: testRegion,
		ID: "projects/" + testProject + "/global/networks/" + name, Name: name}); err != nil {
		t.Errorf("a foreign network is skipped: %v", err)
	}
	if _, err := e4.c.GetNetwork(t.Context(), &computepb.GetNetworkRequest{Project: testProject, Network: name}); err != nil {
		t.Errorf("a foreign network was deleted: %v", err)
	}
	// The bring-your-own subnetwork is never deleted.
	if err := e.d.Delete(t.Context(), vendors.ResourceRef{Kind: gcp.KindSubnets, Region: testRegion, Name: fake.SeedSubnet,
		ID: gcp.SubnetworkURL(testProject, testRegion, fake.SeedSubnet)}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a user's subnetwork: %v, want Fatal", err)
	}
	if err := e.d.Delete(t.Context(), vendors.ResourceRef{Kind: "volumes"}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an unknown kind: %v", err)
	}
}

func TestManagedFirewallRules(t *testing.T) {
	e := newEnv(t, envOptions{})
	name := "scarpenter-conformance-firewalls-europe-west3"
	params := `{"network":"vpc","selfReference":true,"inbound":[` +
		`{"protocol":"tcp","fromPort":22,"toPort":23,"cidrs":["10.0.0.0/8"]},` +
		`{"protocol":"udp","fromPort":41641,"cidrs":["0.0.0.0/0"]},` +
		`{"protocol":"icmp","cidrs":["0.0.0.0/0"]},{"protocol":"any","cidrs":["192.168.0.0/16"]}]}`
	ref, created, err := e.d.Ensure(t.Context(), gcp.KindFirewalls, testRegion, spec(name, params))
	if err != nil || !created || ref.ID != name {
		t.Fatalf("Ensure = %+v, %v, %v", ref, created, err)
	}
	rules := e.c.Firewalls()
	if len(rules) != 6 {
		t.Fatalf("%d rules, want 6 (self, deny and four inbound)", len(rules))
	}
	ports := map[string]string{}
	for _, r := range rules {
		if r.GetTargetTags()[0] != name || r.GetDirection() != "INGRESS" {
			t.Errorf("rule %s: %v", r.GetName(), r)
		}
		for _, a := range r.GetAllowed() {
			ports[r.GetName()] = a.GetIPProtocol() + ":" + strings.Join(a.GetPorts(), ",")
		}
		if len(r.GetDenied()) == 1 && r.GetPriority() != 65534 {
			t.Errorf("the deny rule has the priority %d, it must lose to every allow", r.GetPriority())
		}
	}
	for suffix, want := range map[string]string{"in0": "tcp:22-23", "in1": "udp:41641", "in2": "icmp:", "in3": "all:", "self": "all:"} {
		if ports[name+"-"+suffix] != want {
			t.Errorf("rule %s allows %q, want %q", suffix, ports[name+"-"+suffix], want)
		}
	}
	// A rule that went missing is made again; the others stay.
	_, _ = e.c.DeleteFirewall(t.Context(), &computepb.DeleteFirewallRequest{Project: testProject, Firewall: name + "-in2"})
	if _, created, err := e.d.Ensure(t.Context(), gcp.KindFirewalls, testRegion, spec(name, params)); err != nil || !created {
		t.Errorf("repairing the rules: %v, %v", created, err)
	}
	if len(e.c.Firewalls()) != 6 {
		t.Errorf("%d rules after the repair", len(e.c.Firewalls()))
	}
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindFirewalls, testRegion, spec("x", `{}`)); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a firewall without a network: %v", err)
	}
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindFirewalls, testRegion, spec("x", `{`)); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("bad parameters: %v", err)
	}
	if _, _, err := e.d.Ensure(t.Context(), "volumes", testRegion, spec("x", "")); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an unknown kind: %v", err)
	}
	if _, _, err := e.d.Ensure(t.Context(), gcp.KindFirewalls, testRegion, spec("", "")); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a nameless resource: %v", err)
	}
	// A foreign rule of the name is not adopted.
	e2 := newEnv(t, envOptions{})
	addRule(t, e2, name+"-self", "projects/"+testProject+"/global/networks/vpc", []string{"other"}, false, "mine")
	if _, _, err := e2.d.Ensure(t.Context(), gcp.KindFirewalls, testRegion, spec(name, `{"network":"vpc","selfReference":true}`)); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a foreign rule of the name: %v, want Fatal", err)
	}
	// Delete removes every rule of the tag, and nothing else.
	addRule(t, e, "keep-me", "projects/"+testProject+"/global/networks/vpc", []string{name}, false, "mine")
	if err := e.d.Delete(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if left := e.c.Firewalls(); len(left) != 1 || left[0].GetName() != "keep-me" {
		t.Errorf("rules left = %v", left)
	}
	if err := e.d.Delete(t.Context(), ref); err != nil {
		t.Errorf("a second delete: %v", err)
	}
}

func TestListFiltersByMarkerAndRegion(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, k := range []vendors.ResourceKind{gcp.KindSubnets, gcp.KindFirewalls, gcp.KindRunFirewalls, gcp.KindResourcePolicies} {
		params := `{"network":"vpc","collocated":true}`
		if _, _, err := e.d.Ensure(t.Context(), k, testRegion, vendors.CreateSpec{
			Name: "scarpenter-conformance-" + string(k) + "-europe-west3", Params: json.RawMessage(params),
			Tags: map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClass: "class"}}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := e.d.List(t.Context(), nil, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(all) != 4 {
		t.Fatalf("List = %v, %v", all, err)
	}
	kinds := map[vendors.ResourceKind]bool{}
	for _, r := range all {
		kinds[r.Kind] = true
		if r.Tags[vendors.TagNodeClass] != "class" || r.Tags[vendors.TagResource] != string(r.Kind) || r.CreatedAt.IsZero() {
			t.Errorf("%s: %+v", r.Kind, r)
		}
	}
	if len(kinds) != 4 {
		t.Errorf("kinds = %v", kinds)
	}
	for tag, want := range map[vendors.TagKey]string{vendors.TagNodeClass: "other", vendors.TagManagedBy: "somebody", vendors.TagResource: "volumes"} {
		if l, _ := e.d.List(t.Context(), nil, map[vendors.TagKey]string{tag: want}); len(l) != 0 {
			t.Errorf("tag %s=%s lists %v", tag, want, l)
		}
	}
	if l, _ := e.d.List(t.Context(), []string{fake.SeedOtherReg}, nil); len(l) != 0 {
		t.Errorf("another region lists %v", l)
	}
	// A driver without a cluster name owns nothing.
	noCluster := restart(t, e.c, envOptions{cluster: "x"})
	if l, _ := noCluster.d.List(t.Context(), nil, nil); len(l) != 0 {
		t.Errorf("another cluster lists %v", l)
	}
}

func TestInUseKinds(t *testing.T) {
	e := newEnv(t, envOptions{})
	pol, _, err := e.d.Ensure(t.Context(), gcp.KindResourcePolicies, testRegion, spec("scarpenter-conformance-pol", `{"availabilityDomainCount":2}`))
	if err != nil {
		t.Fatal(err)
	}
	fw, _, _ := e.d.Ensure(t.Context(), gcp.KindFirewalls, testRegion, spec("scarpenter-conformance-fw", `{"network":"vpc"}`))
	subnet, _, _ := e.d.Ensure(t.Context(), gcp.KindSubnets, testRegion, spec("scarpenter-conformance-net", ""))
	for _, r := range []vendors.ResourceRef{pol, fw, subnet} {
		if in, _, err := e.d.InUse(t.Context(), r); err != nil || in {
			t.Errorf("%s fresh: %v, %v", r.Kind, in, err)
		}
	}
	req := request(`{}`)
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{subnet, fw, pol}}
	if _, err := e.d.CreateInstance(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	for _, r := range []vendors.ResourceRef{pol, fw, subnet} {
		if in, last, err := e.d.InUse(t.Context(), r); err != nil || !in || last.IsZero() {
			t.Errorf("%s used: %v, %v, %v", r.Kind, in, last, err)
		}
		if err := e.d.Delete(t.Context(), r); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("deleting the used %s: %v, want Retryable", r.Kind, err)
		}
	}
	if _, _, err := e.d.InUse(t.Context(), vendors.ResourceRef{Kind: gcp.KindSubnets, Region: testRegion, Name: "gone",
		ID: gcp.SubnetworkURL(testProject, testRegion, "gone")}); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("InUse of a missing resource: %v", err)
	}
	if _, _, err := e.d.InUse(t.Context(), vendors.ResourceRef{Kind: "volumes"}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("InUse of an unknown kind: %v", err)
	}
}

func TestKindsAndSelectorDeclarations(t *testing.T) {
	d := gcp.New()
	kinds := d.Kinds()
	order := map[vendors.ResourceKind]int{}
	for _, k := range kinds {
		order[k.Kind] = k.DeleteOrder
	}
	// Policies and run rules go before the NodeClass rules, which go before the network.
	if order[gcp.KindResourcePolicies] >= order[gcp.KindFirewalls] || order[gcp.KindRunFirewalls] >= order[gcp.KindFirewalls] ||
		order[gcp.KindFirewalls] >= order[gcp.KindSubnets] {
		t.Errorf("delete order = %v", order)
	}
	caps := d.Selectors()
	for _, k := range []vendors.ResourceKind{gcp.KindSubnets, gcp.KindFirewalls, gcp.KindResourcePolicies, gcp.KindImages} {
		if _, ok := caps[k]; !ok {
			t.Errorf("no selector declaration for %s", k)
		}
	}
	if caps[gcp.KindImages].Tags != vendors.TagsLabels || !caps[gcp.KindImages].Global {
		t.Errorf("images: %+v", caps[gcp.KindImages])
	}
	// Networks, subnetworks, rules and policies have no labels: tags are rejected.
	for _, k := range []vendors.ResourceKind{gcp.KindSubnets, gcp.KindFirewalls, gcp.KindResourcePolicies} {
		if caps[k].Tags != vendors.TagsNone {
			t.Errorf("%s accepts tags", k)
		}
	}
	if !slices.ContainsFunc(caps[gcp.KindImages].Filters, func(f vendors.FilterCap) bool { return f.Name == "family" }) {
		t.Error("images has no family filter")
	}
}
