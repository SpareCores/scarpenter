/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

func groupTags(kind vendors.ResourceKind) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{
		vendors.TagCluster: conformanceCluster, vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: string(kind),
	}
}

func i32(v int32) *int32 { return new(v) }

func ingressOf(t *testing.T, w *world, region, id string) []osapi.SecurityRule {
	t.Helper()
	cl, _ := w.cloud.Clients(ctx, region)
	gs, err := cl.Network.ListSecurityGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []osapi.SecurityRule
	for _, g := range gs {
		if g.ID == id {
			for _, r := range g.Rules {
				if r.Direction == "ingress" {
					out = append(out, r)
				}
			}
		}
	}
	return out
}

func TestSecurityGroupRulesFollowTheParameters(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	spec := vendors.CreateSpec{Name: "scarpenter-conformance-securitygroups-gra11", Tags: groupTags(KindSecurityGroups)}
	params := func(p SecurityGroupParams) json.RawMessage {
		raw, _ := json.Marshal(p)
		return raw
	}
	spec.Params = params(SecurityGroupParams{SelfReference: true, Inbound: []common.InboundRule{
		{Protocol: "tcp", FromPort: i32(10250), CIDRs: []string{"10.0.0.0/8", "2001:db8::/32"}},
		{Protocol: "udp", FromPort: i32(41641), ToPort: i32(41642), CIDRs: []string{"0.0.0.0/0"}},
		{Protocol: "icmp", CIDRs: []string{"0.0.0.0/0", "::/0"}},
		{Protocol: "tcp", CIDRs: []string{"192.0.2.0/24"}},
	}})
	ref, created, err := d.Ensure(ctx, KindSecurityGroups, testRegion, spec)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	rules := ingressOf(t, w, testRegion, ref.ID)
	// Two self-reference rules and six inbound ones.
	if len(rules) != 2+6 {
		t.Fatalf("%d ingress rules: %+v", len(rules), rules)
	}
	var v6icmp, tcpAny bool
	for _, r := range rules {
		v6icmp = v6icmp || (r.Protocol == "ipv6-icmp" && r.EtherType == "IPv6")
		tcpAny = tcpAny || (r.Protocol == "tcp" && r.PortMin == 0 && r.RemoteCIDR == "192.0.2.0/24")
	}
	if !v6icmp || !tcpAny {
		t.Errorf("ICMP over IPv6 is ipv6-icmp and a rule without ports covers every port: %+v", rules)
	}

	// The same parameters change nothing; fewer rules remove the rest.
	again, created, err := d.Ensure(ctx, KindSecurityGroups, testRegion, spec)
	if err != nil || created || again.ID != ref.ID || len(ingressOf(t, w, testRegion, ref.ID)) != 8 {
		t.Fatalf("idempotent: %+v %v %v", again, created, err)
	}
	spec.Params = params(SecurityGroupParams{Inbound: []common.InboundRule{{Protocol: "tcp", FromPort: i32(22), CIDRs: []string{"10.1.2.3/8"}}}})
	if _, _, err := d.Ensure(ctx, KindSecurityGroups, testRegion, spec); err != nil {
		t.Fatal(err)
	}
	rules = ingressOf(t, w, testRegion, ref.ID)
	if len(rules) != 1 || rules[0].PortMin != 22 {
		t.Errorf("the rules follow the parameters: %+v", rules)
	}
	spec.Params = json.RawMessage(`{`)
	if _, _, err := d.Ensure(ctx, KindSecurityGroups, testRegion, spec); vendors.CodeOf(err) != CodeInvalidSettings {
		t.Errorf("bad parameters: %v", err)
	}
}

func TestSecurityGroupOwnership(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	cl, _ := w.cloud.Clients(ctx, testRegion)
	user, err := cl.Network.CreateSecurityGroup(ctx, "scarpenter-conformance-mine", "a user group")
	if err != nil {
		t.Fatal(err)
	}
	spec := vendors.CreateSpec{Name: "scarpenter-conformance-mine", Tags: groupTags(KindSecurityGroups)}
	if _, _, err := d.Ensure(ctx, KindSecurityGroups, testRegion, spec); vendors.CodeOf(err) != CodeNoMarker {
		t.Errorf("a group of that name without our marker is not adopted: %v", err)
	}
	if err := d.Delete(ctx, vendors.ResourceRef{Kind: KindSecurityGroups, Region: testRegion, ID: user.ID}); vendors.CodeOf(err) != CodeNotOurs {
		t.Errorf("a user's group is never deleted: %v", err)
	}
	// Another cluster's group is not ours either.
	other := groupTags(KindSecurityGroups)
	other[vendors.TagCluster] = "another"
	spec2 := vendors.CreateSpec{Name: "scarpenter-another-sg", Tags: other}
	ref, _, err := d.Ensure(ctx, KindSecurityGroups, testRegion, spec2)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, ref); vendors.CodeOf(err) != CodeNotOurs {
		t.Errorf("another cluster's group: %v", err)
	}
	long := groupTags(KindSecurityGroups)
	long[vendors.TagNodeClass] = string(make([]byte, 300))
	if _, _, err := d.Ensure(ctx, KindSecurityGroups, testRegion, vendors.CreateSpec{Name: "x", Tags: long}); vendors.CodeOf(err) != CodeMarkerTooLong {
		t.Errorf("an oversized marker: %v", err)
	}
	if _, _, err := d.Ensure(ctx, KindSecurityGroups, testRegion, vendors.CreateSpec{Tags: long}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a resource without a name: %v", err)
	}
	if _, _, err := d.Ensure(ctx, "subnets", testRegion, spec); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("an unknown kind: %v", err)
	}
	if err := d.Delete(ctx, vendors.ResourceRef{Kind: "subnets", Region: testRegion}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("deleting an unknown kind: %v", err)
	}
	if _, _, err := d.InUse(ctx, vendors.ResourceRef{Kind: KindSecurityGroups, Region: testRegion, ID: "missing"}); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("InUse of a group that does not exist: %v", err)
	}
	if _, err := d.Resolve(ctx, "subnets", testRegion, vendors.UseRef{Name: "x"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("resolving an unknown kind: %v", err)
	}
	if err := d.Delete(ctx, vendors.ResourceRef{Kind: KindSecurityGroups, Region: "NO-SUCH-1", ID: "x"}); err != nil {
		t.Errorf("deleting in a region outside the catalog: %v", err)
	}
}

func TestServerGroups(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	name := "scarpenter-conformance-servergroups-par-0123456789"
	tags := groupTags(KindServerGroups)
	ref, created, err := d.Ensure(ctx, KindServerGroups, multiRegion, vendors.CreateSpec{
		Name: name, Tags: tags, Params: json.RawMessage(`{"policy":"anti-affinity"}`)})
	if err != nil || !created || !ref.Managed {
		t.Fatalf("%+v %v %v", ref, created, err)
	}
	if again, created, err := d.Ensure(ctx, KindServerGroups, multiRegion, vendors.CreateSpec{Name: name, Tags: tags}); err != nil || created || again.ID != ref.ID {
		t.Errorf("idempotent: %+v %v %v", again, created, err)
	}
	// The name is the only ownership marker of a server group.
	if _, _, err := d.Ensure(ctx, KindServerGroups, multiRegion, vendors.CreateSpec{Name: "mine", Tags: tags}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a name without the cluster prefix: %v", err)
	}
	if _, _, err := d.Ensure(ctx, KindServerGroups, multiRegion, vendors.CreateSpec{
		Name: name + "x", Tags: tags, Params: json.RawMessage(`{"policy":"soft-affinity"}`)}); vendors.CodeOf(err) != CodeGroupPolicy {
		t.Errorf("a policy OVH does not document: %v", err)
	}
	if _, _, err := d.Ensure(ctx, KindServerGroups, multiRegion, vendors.CreateSpec{Name: name + "x", Tags: tags, Params: json.RawMessage(`[`)}); vendors.CodeOf(err) != CodeInvalidSettings {
		t.Errorf("bad parameters: %v", err)
	}
	cl, _ := w.cloud.Clients(ctx, multiRegion)
	foreign, _ := cl.Compute.CreateServerGroup(ctx, "a-user-group", "affinity")

	listed, err := d.List(ctx, []string{multiRegion}, map[vendors.TagKey]string{vendors.TagCluster: conformanceCluster})
	if err != nil || len(listed) != 1 || listed[0].ID != ref.ID || listed[0].Kind != KindServerGroups {
		t.Fatalf("only the cluster's group is listed: %+v %v", listed, err)
	}
	if other, _ := d.List(ctx, nil, map[vendors.TagKey]string{vendors.TagNodeClass: "class"}); len(other) != 0 {
		t.Errorf("a server group has no NodeClass to match: %+v", other)
	}
	got, err := d.Resolve(ctx, KindServerGroups, multiRegion, vendors.UseRef{Name: "a-user-*"})
	if err != nil || len(got) != 1 || got[0].ID != foreign.ID || got[0].Managed {
		t.Errorf("resolve by name: %+v %v", got, err)
	}
	if err := d.Delete(ctx, vendors.ResourceRef{Kind: KindServerGroups, Region: multiRegion, ID: foreign.ID}); vendors.CodeOf(err) != CodeNotOurs {
		t.Errorf("a user's server group is never deleted: %v", err)
	}
	if in, _, err := d.InUse(ctx, ref); err != nil || in {
		t.Errorf("an empty group: %v %v", in, err)
	}
	if _, _, err := d.InUse(ctx, vendors.ResourceRef{Kind: KindServerGroups, Region: multiRegion, ID: "missing"}); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("InUse of a group that does not exist: %v", err)
	}
	if err := d.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestSelectorsOfSecurityGroups(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	cl, _ := w.cloud.Clients(ctx, testRegion)
	a, _ := cl.Network.CreateSecurityGroup(ctx, "nodes-a", "alpha group")
	b, _ := cl.Network.CreateSecurityGroup(ctx, "nodes-b", "beta group")
	use := func(u common.ResourceUse) []string {
		x := selector.Context{Region: testRegion, NodeClass: "c", Channel: "stable"}
		ref, err := selector.Build(&u, x, d.Selectors()[KindSecurityGroups])
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.Resolve(ctx, KindSecurityGroups, testRegion, ref)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(got))
		for _, r := range got {
			ids = append(ids, r.ID)
		}
		slices.Sort(ids)
		return ids
	}
	want := []string{a.ID, b.ID}
	slices.Sort(want)
	if got := use(common.ResourceUse{Name: "nodes-?", Select: common.SelectAll}); !slices.Equal(got, want) {
		t.Errorf("glob: %v", got)
	}
	if got := use(common.ResourceUse{Name: "nodes-*", Match: &common.ResourceMatch{Description: "alpha*"}}); !slices.Equal(got, []string{a.ID}) {
		t.Errorf("description: %v", got)
	}
	if got := use(common.ResourceUse{IDs: map[string]common.IDList{testRegion: {b.ID}}}); !slices.Equal(got, []string{b.ID}) {
		t.Errorf("ids: %v", got)
	}
	// Neutron's name is not unique: two groups of one name are ambiguous under exactlyOne.
	_, _ = cl.Network.CreateSecurityGroup(ctx, "nodes-a", "again")
	ref := vendors.UseRef{Name: "nodes-a", Select: common.SelectExactlyOne}
	if _, err := d.Resolve(ctx, KindSecurityGroups, testRegion, ref); err == nil {
		t.Error("two groups of one name are ambiguous")
	}
	if _, err := d.Resolve(ctx, KindSecurityGroups, testRegion, vendors.UseRef{}); err == nil {
		t.Error("an empty selector selects nothing")
	}
}
