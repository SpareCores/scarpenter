/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

func marker(kind string) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{
		vendors.TagCluster: conformanceCluster, vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: kind,
	}
}

func TestResolveOfEveryKind(t *testing.T) {
	e := newCEnv()
	d := e.driver(t)
	ctx := context.Background()
	n1 := e.f.AddNetwork("prod-a", "10.1.0.0/16", map[string]string{"env": "prod"}, hc.NetworkZoneEUCentral)
	n2 := e.f.AddNetwork("prod-b", "10.2.0.0/16", map[string]string{"env": "prod"}, hc.NetworkZoneUSEast)
	e.f.AddNetwork("dev", "192.168.0.0/24", map[string]string{"env": "dev"}, hc.NetworkZoneEUCentral)
	ids := func(refs []vendors.ResourceRef) []string {
		out := make([]string, 0, len(refs))
		for _, r := range refs {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	resolve := func(kind vendors.ResourceKind, region string, u vendors.UseRef) ([]vendors.ResourceRef, error) {
		return d.Resolve(ctx, kind, region, u)
	}
	// Networks: only those with a subnet in the zone of the location; by label expression,
	// range and zone.
	got, err := resolve(hcloud.KindNetworks, "fsn1", vendors.UseRef{Expression: "env!=dev", Select: common.SelectAll})
	if err != nil || !slices.Equal(ids(got), []string{fmt.Sprint(n1.ID)}) {
		t.Errorf("expression: %v %v", ids(got), err)
	}
	got, err = resolve(hcloud.KindNetworks, "ash", vendors.UseRef{Tags: map[string]string{"env": "prod"}, Select: common.SelectAll})
	if err != nil || !slices.Equal(ids(got), []string{fmt.Sprint(n2.ID)}) {
		t.Errorf("ash: %v %v", ids(got), err)
	}
	got, err = resolve(hcloud.KindNetworks, "fsn1", vendors.UseRef{
		Match: common.ResourceMatch{Within: "10.0.0.0/8", Zone: []string{"eu-central"}}, Select: common.SelectAll})
	if err != nil || !slices.Equal(ids(got), []string{fmt.Sprint(n1.ID)}) {
		t.Errorf("within and zone: %v %v", ids(got), err)
	}
	if _, err := resolve(hcloud.KindNetworks, "fsn1", vendors.UseRef{Tags: map[string]string{"env": "prod"}}); err != nil {
		t.Errorf("exactlyOne with one in the zone: %v", err)
	}
	if _, err := resolve(hcloud.KindNetworks, "fsn1", vendors.UseRef{Name: "prod-b"}); err == nil {
		t.Error("a network without a subnet in the zone")
	}
	if _, err := resolve(hcloud.KindNetworks, "nowhere", vendors.UseRef{Name: "prod-a"}); err == nil {
		t.Error("an unknown location")
	}

	// Images: the snapshots of the project.
	got, err = resolve(hcloud.KindImages, "fsn1", vendors.UseRef{
		Tags: map[string]string{"os": "flatcar"}, Match: common.ResourceMatch{Architecture: []string{"amd64"}}})
	if err != nil || !slices.Equal(ids(got), []string{fmt.Sprint(e.x86.ID)}) {
		t.Errorf("images: %v %v", ids(got), err)
	}
	if _, err := resolve("subnets", "fsn1", vendors.UseRef{Name: "x"}); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an unknown kind: %v", err)
	}
	if _, err := resolve(hcloud.KindFirewalls, "fsn1", vendors.UseRef{}); err == nil {
		t.Error("an empty selector")
	}
}

// mrEnv is a driver over a fake project with one managed resource of each kind.
type mrEnv struct {
	e           *cenv
	d           *wrapped
	fw, pg, vol vendors.ResourceRef
}

func mrSpec(kind, name string) vendors.CreateSpec {
	return vendors.CreateSpec{Name: name, Tags: marker(kind), ExtraTags: map[string]string{"team": "infra"}}
}

func newMREnv(t *testing.T) *mrEnv {
	t.Helper()
	e := newCEnv()
	m := &mrEnv{e: e, d: e.driver(t)}
	ctx := context.Background()
	var created bool
	var err error
	m.fw, created, err = m.d.Ensure(ctx, hcloud.KindFirewalls, "fsn1",
		mrSpec("firewalls", "scarpenter-conformance-firewalls-fsn1"))
	if err != nil || !created || !m.fw.Managed {
		t.Fatalf("%+v %v %v", m.fw, created, err)
	}
	if m.pg, _, err = m.d.Ensure(ctx, hcloud.KindPlacementGroups, "fsn1",
		mrSpec("placementGroups", "scarpenter-conformance-pg")); err != nil {
		t.Fatal(err)
	}
	vs := mrSpec("volumes", "scarpenter-conformance-vol")
	vs.Params = json.RawMessage(`{"sizeGB":25}`)
	if m.vol, _, err = m.d.Ensure(ctx, hcloud.KindVolumes, "nbg1", vs); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestEnsureLabelsAndSizes(t *testing.T) {
	m := newMREnv(t)
	ctx := context.Background()
	got, _ := m.e.f.GetFirewall(ctx, mustID(t, m.fw.ID))
	if got.Labels["team"] != "infra" || got.Labels[hcloud.LabelLocation] != "fsn1" {
		t.Errorf("the labels: %v", got.Labels)
	}
	v, _ := m.e.f.GetVolume(ctx, mustID(t, m.vol.ID))
	if v.Size != 25 || v.Location.Name != "nbg1" {
		t.Errorf("the volume: %+v", v)
	}
}

func TestEnsureRefusals(t *testing.T) {
	m := newMREnv(t)
	d, e := m.d, m.e
	ctx := context.Background()
	if _, _, err := d.Ensure(ctx, hcloud.KindNetworks, "fsn1", mrSpec("networks", "n")); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("networks are not created: %v", err)
	}
	if _, _, err := d.Ensure(ctx, hcloud.KindFirewalls, "fsn1", vendors.CreateSpec{}); err == nil {
		t.Error("a nameless resource")
	}
	bad := mrSpec("firewalls", "x")
	bad.Params = json.RawMessage(`{`)
	for _, k := range []vendors.ResourceKind{hcloud.KindFirewalls, hcloud.KindPlacementGroups, hcloud.KindVolumes} {
		if _, _, err := d.Ensure(ctx, k, "fsn1", bad); vendors.ClassOf(err) != vendors.Fatal {
			t.Errorf("%s with malformed parameters: %v", k, err)
		}
	}
	badTags := mrSpec("firewalls", "y")
	badTags.ExtraTags = map[string]string{"bad key!": "v"}
	if _, _, err := d.Ensure(ctx, hcloud.KindFirewalls, "fsn1", badTags); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("an invalid label: %v", err)
	}

	// A foreign object of the same name is never adopted.
	_, _ = e.f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "taken"})
	_, _ = e.f.CreatePlacementGroup(ctx, hc.PlacementGroupCreateOpts{Name: "taken", Type: hc.PlacementGroupTypeSpread})
	_, _ = e.f.CreateVolume(ctx, hc.VolumeCreateOpts{Name: "taken", Size: 10, Location: &hc.Location{Name: "fsn1"}})
	for _, k := range []vendors.ResourceKind{hcloud.KindFirewalls, hcloud.KindPlacementGroups, hcloud.KindVolumes} {
		if _, _, err := d.Ensure(ctx, k, "fsn1", mrSpec(string(k), "taken")); vendors.CodeOf(err) != "NotOwned" {
			t.Errorf("%s of a foreign name: %v", k, err)
		}
	}

	// One of ours with the name exists by the time of the call: adopted, not created.
	raced := mrSpec("firewalls", "raced")
	_, _ = e.f.CreateFirewall(ctx, hc.FirewallCreateOpts{Name: "raced", Labels: mustLabels(raced)})
	if r, created, err := d.Ensure(ctx, hcloud.KindFirewalls, "fsn1", raced); err != nil || created || r.Name != "raced" {
		t.Errorf("adopting an owned one: %+v %v %v", r, created, err)
	}
}

func TestInUseAndDelete(t *testing.T) {
	m := newMREnv(t)
	d, e := m.d, m.e
	ctx := context.Background()
	all := []vendors.ResourceRef{m.fw, m.pg, m.vol}
	for _, r := range all {
		if used, _, err := d.InUse(ctx, r); err != nil || used {
			t.Errorf("%s fresh: %v %v", r.Kind, used, err)
		}
	}
	inst, err := d.CreateInstance(ctx, vendors.CreateRequest{
		Region: "nbg1", Zone: "nbg1", InstanceType: "cx23", CapacityType: vendors.CapacityOnDemand,
		Image: vendors.Image{Ref: fmt.Sprint(e.x86.ID)}, UserData: []byte("{}"), IdempotencyKey: "u",
		Tags: map[vendors.TagKey]string{vendors.TagCluster: conformanceCluster, vendors.TagManagedBy: vendors.ManagedByValue},
		Node: vendors.NodeInfo{Name: "n1"}, Prepared: &vendors.Prepared{Resources: all},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if used, last, err := d.InUse(ctx, r); err != nil || !used || last.IsZero() {
			t.Errorf("%s in use: %v %v", r.Kind, used, err)
		}
		if err := d.Delete(ctx, r); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("deleting %s in use: %v", r.Kind, err)
		}
	}
	if err := d.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	e.f.Tick()
	for _, r := range all {
		if err := d.Delete(ctx, r); err != nil {
			t.Errorf("delete %s: %v", r.Kind, err)
		}
		if _, _, err := d.InUse(ctx, r); !vendors.IsClass(err, vendors.NotFound) {
			t.Errorf("InUse of a deleted %s: %v", r.Kind, err)
		}
	}
	if used, _, err := d.InUse(ctx, vendors.ResourceRef{Kind: hcloud.KindNetworks, ID: "1"}); err != nil || !used {
		t.Errorf("a brought network is never ours to delete: %v %v", used, err)
	}
	for _, r := range []vendors.ResourceRef{
		{Kind: "images", ID: "1"}, {Kind: hcloud.KindFirewalls, ID: "x"},
	} {
		if _, _, err := d.InUse(ctx, r); err == nil {
			t.Errorf("InUse of %v", r)
		}
	}
	for _, r := range []vendors.ResourceRef{
		{Kind: hcloud.KindNetworks, ID: "1"}, {Kind: "images", ID: "1"}, {Kind: hcloud.KindFirewalls, ID: "x"},
	} {
		if err := d.Delete(ctx, r); err == nil {
			t.Errorf("Delete of %v", r)
		}
	}
}

func TestListByMarkerAndRegion(t *testing.T) {
	m := newMREnv(t)
	ctx := context.Background()
	_, _, _ = m.d.Ensure(ctx, hcloud.KindFirewalls, "nbg1", mrSpec("firewalls", "scarpenter-conformance-firewalls-nbg1"))
	cluster := map[vendors.TagKey]string{vendors.TagCluster: conformanceCluster}
	all, err := m.d.List(ctx, nil, cluster)
	if err != nil || len(all) != 4 {
		t.Fatalf("%v %v", all, err)
	}
	nbg, _ := m.d.List(ctx, []string{"nbg1"}, cluster)
	if len(nbg) != 2 {
		t.Errorf("the volume and the firewall of nbg1: %+v", nbg)
	}
	for _, r := range nbg {
		if r.Region != "nbg1" {
			t.Errorf("%+v", r)
		}
	}
}

func mustLabels(s vendors.CreateSpec) map[string]string {
	l, err := hcloud.Labels(s.Tags, s.ExtraTags)
	if err != nil {
		panic(err)
	}
	l[hcloud.LabelLocation] = "fsn1"
	return l
}

var _ = metav1.Now
