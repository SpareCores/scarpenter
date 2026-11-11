/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	uc "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

var marker = map[vendors.TagKey]string{
	vendors.TagCluster: testCluster, vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
}

func spec(name string, kind vendors.ResourceKind, params any) vendors.CreateSpec {
	tags := map[vendors.TagKey]string{}
	maps.Copy(tags, marker)
	tags[vendors.TagResource] = string(kind)
	raw, _ := json.Marshal(params)
	if params == nil {
		raw = nil
	}
	return vendors.CreateSpec{Name: name, Tags: tags, Params: raw, ExtraTags: map[string]string{"team": "infra"}}
}

func TestManagedNetworksAvoidTheNetworksOfTheAccount(t *testing.T) {
	e := newEnv(t, envOptions{})
	first, created, err := e.d.Ensure(e.ctx(), uc.KindNetworks, testZone, spec("scarpenter-my-cluster-networks-fi-hel1", uc.KindNetworks, nil))
	if err != nil || !created || !first.Managed || first.Network != first.ID {
		t.Fatalf("Ensure = %+v, %v, %v", first, created, err)
	}
	n, _ := e.f.GetNetworkDetails(e.ctx(), &request.GetNetworkDetailsRequest{UUID: first.ID})
	cidr := n.IPNetworks[0].Address
	if n.IPNetworks[0].DHCP != upcloud.True || n.IPNetworks[0].DHCPDefaultRoute != upcloud.False || n.Router != "" {
		t.Errorf("network %+v", n.IPNetworks)
	}
	labels := map[string]string{}
	for _, l := range n.Labels {
		labels[l.Key] = l.Value
	}
	if labels["team"] != "infra" || labels["scarpenter-cluster"] != testCluster || labels[uc.LabelRegion] != testZone ||
		labels["scarpenter-resource-kind"] != "networks" {
		t.Errorf("labels %v", labels)
	}
	// Another zone's network of the same name gets another range (the account's networks must not overlap).
	other, _, err := e.d.Ensure(e.ctx(), uc.KindNetworks, "de-fra1", spec("scarpenter-my-cluster-networks-fi-hel1", uc.KindNetworks, nil))
	if err != nil {
		t.Fatal(err)
	}
	on, _ := e.f.GetNetworkDetails(e.ctx(), &request.GetNetworkDetailsRequest{UUID: other.ID})
	if on.IPNetworks[0].Address == cidr {
		t.Errorf("two networks of the account share %s", cidr)
	}
	// A network range with every /24 taken is refused.
	small := newEnv(t, envOptions{driver: []uc.Option{uc.WithNetworkRange("10.1.0.0/16")}, fake: fake.Options{MaxNetworks: 1000}})
	for i := range 256 {
		_, err := small.f.CreateNetwork(small.ctx(), &request.CreateNetworkRequest{
			Name: "x", Zone: testZone,
			IPNetworks: upcloud.IPNetworkSlice{{Address: "10.1." + itoa(i) + ".0/24", Family: upcloud.IPAddressFamilyIPv4}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := small.d.Ensure(small.ctx(), uc.KindNetworks, testZone, spec("scarpenter-x", uc.KindNetworks, nil)); vendors.CodeOf(err) != "NetworkRange" {
		t.Errorf("Ensure with every /24 of the range taken: %v", err)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestEnsureRefusesWhatItCannotDo(t *testing.T) {
	e := newEnv(t, envOptions{})
	if _, _, err := e.d.Ensure(e.ctx(), "subnets", testZone, spec("x", "subnets", nil)); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("an unknown kind: %v", err)
	}
	if _, _, err := e.d.Ensure(e.ctx(), uc.KindNetworks, testZone, vendors.CreateSpec{}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a resource without a name: %v", err)
	}
	if _, err := e.d.Resolve(e.ctx(), uc.KindImages, testZone, vendors.UseRef{Name: "x"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("Resolve of images: %v", err)
	}
	if _, err := e.d.Resolve(e.ctx(), uc.KindNetworks, testZone, vendors.UseRef{}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("Resolve of an empty selector: %v", err)
	}
	if _, _, err := e.d.InUse(e.ctx(), vendors.ResourceRef{Kind: "subnets"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("InUse of an unknown kind: %v", err)
	}
	if err := e.d.Delete(e.ctx(), vendors.ResourceRef{Kind: "subnets"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("Delete of an unknown kind: %v", err)
	}
	bad := spec("scarpenter-fw", uc.KindFirewalls, nil)
	bad.Params = json.RawMessage(`{`)
	if _, _, err := e.d.Ensure(e.ctx(), uc.KindFirewalls, testZone, bad); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("bad ruleset parameters: %v", err)
	}
	bad.Params = json.RawMessage(`{"cidr":"nope"}`)
	if _, _, err := e.d.Ensure(e.ctx(), uc.KindFirewalls, testZone, bad); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a bad network range: %v", err)
	}
	bad.Params = json.RawMessage(`{`)
	if _, _, err := e.d.Ensure(e.ctx(), uc.KindServerGroups, testZone, bad); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("bad group parameters: %v", err)
	}
	e.f.InjectCode("GetNetworks", "AUTHENTICATION_FAILED", 401, 1)
	if _, _, err := e.d.Ensure(e.ctx(), uc.KindNetworks, testZone, spec("n", uc.KindNetworks, nil)); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a failed list: %v", err)
	}
}

func TestManagedRulesetsAreAdoptedAndAttributedToTheirZone(t *testing.T) {
	e := newEnv(t, envOptions{})
	name := "scarpenter-my-cluster-firewalls-de-fra1-abc"
	// A ruleset whose list answer carries no labels: adopted by its deterministic name, and
	// attributed to a zone by the name.
	r, err := e.f.CreateFirewallRuleset(e.ctx(), api.CreateRulesetRequest{Name: name, Stateful: true})
	if err != nil {
		t.Fatal(err)
	}
	ref, created, err := e.d.Ensure(e.ctx(), uc.KindFirewalls, "de-fra1", spec(name, uc.KindFirewalls, nil))
	if err != nil || created || ref.ID != r.UUID {
		t.Fatalf("Ensure = %+v, %v, %v", ref, created, err)
	}
	// A new one carries its labels and the zone.
	fresh, created, err := e.d.Ensure(e.ctx(), uc.KindFirewalls, testZone, spec("scarpenter-my-cluster-firewalls-fi-hel1", uc.KindFirewalls,
		uc.FirewallParams{}))
	if err != nil || !created {
		t.Fatalf("Ensure = %+v, %v, %v", fresh, created, err)
	}
	list, err := e.d.List(e.ctx(), []string{testZone}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(list) != 1 || list[0].ID != fresh.ID || list[0].Region != testZone {
		t.Errorf("List in %s = %+v, %v", testZone, list, err)
	}
	// The one without labels does not carry the marker the tags ask for, so it is not listed.
	all, err := e.d.List(e.ctx(), nil, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(all) != 1 {
		t.Errorf("List = %+v, %v", all, err)
	}
	// Only what carries the marker is deleted.
	if err := e.d.Delete(e.ctx(), vendors.ResourceRef{Kind: uc.KindFirewalls, ID: fresh.ID}); err != nil {
		t.Errorf("Delete of a managed ruleset: %v", err)
	}
	user, err := e.f.CreateFirewallRuleset(e.ctx(), api.CreateRulesetRequest{Name: "theirs", Labels: []upcloud.Label{{Key: "mine", Value: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.d.Delete(e.ctx(), vendors.ResourceRef{Kind: uc.KindFirewalls, ID: user.UUID}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("Delete of another's ruleset: %v", err)
	}
	if !strings.HasPrefix(name, "scarpenter-") {
		t.Fatal("test name")
	}
}

func TestResolveNetworksByFiltersAndTags(t *testing.T) {
	e := newEnv(t, envOptions{})
	mk := func(zone, name string, labels ...upcloud.Label) string {
		n, err := e.f.CreateNetwork(e.ctx(), &request.CreateNetworkRequest{
			Name: name, Zone: zone, Labels: labels,
			IPNetworks: upcloud.IPNetworkSlice{{Address: "172.31." + itoa(len(name)*7+len(zone)) + ".0/24", Family: upcloud.IPAddressFamilyIPv4}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return n.UUID
	}
	a := mk(testZone, "alpha", upcloud.Label{Key: "tier", Value: "db"})
	b := mk("de-fra1", "bravo-x", upcloud.Label{Key: "tier", Value: "db"})
	got, err := e.d.Resolve(e.ctx(), uc.KindNetworks, testZone, vendors.UseRef{Tags: map[string]string{"tier": "*"}})
	if err != nil || len(got) != 1 || got[0].ID != a {
		t.Errorf("a key that exists, in the zone: %v, %v", got, err)
	}
	got, err = e.d.Resolve(e.ctx(), uc.KindNetworks, "de-fra1", vendors.UseRef{IDs: []string{a, b}})
	if err != nil || len(got) != 1 || got[0].ID != b {
		t.Errorf("IDs of two zones, in one: %v, %v", got, err)
	}
	if _, err := e.d.Resolve(e.ctx(), uc.KindNetworks, testZone, vendors.UseRef{Name: "alpha", Filters: nil, Tags: map[string]string{"tier": "web"}}); err == nil {
		t.Error("a tag that does not match found a network")
	}
	// The utility and public networks are not private.
	if _, err := e.d.Resolve(e.ctx(), uc.KindNetworks, testZone, vendors.UseRef{Name: "alpha",
		Filters: nil}); err != nil {
		t.Errorf("by name: %v", err)
	}
}
