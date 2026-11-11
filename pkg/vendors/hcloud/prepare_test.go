/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	hcapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	hcfake "github.com/SpareCores/scarpenter/pkg/vendors/hcloud/fake"
)

// engEnv is the driver behind the real resource engine, which decides where each
// resource comes from (use, discovery, managed).
type engEnv struct {
	*cenv
	d      *wrapped
	engine *resources.Engine
	clk    *clocktesting.FakeClock
	kube   client.Client
}

func newEngEnv(t *testing.T) *engEnv {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	f := hcfake.New(hcfake.WithClock(clk))
	x86, arm := f.AddFlatcarImages("stable", "4757.2.3")
	c := &cenv{f: f, x86: x86, arm: arm}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	d := hcloud.New(hcloud.WithAPI(f), hcloud.WithHCloudServerLimit(100), hcloud.WithRegisterer(nopRegisterer{}),
		hcloud.WithPrices(hcloud.StaticPrices{Volume: map[string]float64{"fsn1": 0.0484}, IPv4: 0.0007}))
	if err := d.Init(context.Background(), vendors.Config{
		ClusterName: conformanceCluster, Clock: clk, Log: logr.Discard(), Client: kube,
	}); err != nil {
		t.Fatal(err)
	}
	w := &wrapped{Driver: d, env: c}
	eng := resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(w), Reader: kube, Clock: clk, ClusterName: conformanceCluster,
		Regions: func(string) []string { return []string{"fsn1"} },
		Zones:   func(_, region string) []string { return []string{region} },
	})
	return &engEnv{cenv: c, d: w, engine: eng, clk: clk, kube: kube}
}

func (e *engEnv) nodeClass(t *testing.T, mutate func(*v1alpha1.SCNodeClass, *hcapi.Settings)) *v1alpha1.SCNodeClass {
	t.Helper()
	b := &hcapi.Settings{}
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{HCloud: b}},
	}
	if mutate != nil {
		mutate(nc, b)
	}
	if err := e.kube.Create(context.Background(), nc); err != nil {
		t.Fatal(err)
	}
	return nc
}

// settings resolves the NodeClass's settings with the profiles selected.
func settingsOf(t *testing.T, d vendors.NodeOptions, nc *v1alpha1.SCNodeClass, sel options.Selection) vendors.VendorSettings {
	t.Helper()
	in, ok, err := options.InputFromNodeClass(nc, "hcloud")
	if err != nil || !ok {
		t.Fatalf("the input: %v %v", ok, err)
	}
	s, err := options.Resolve(d, in, sel)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *engEnv) prepare(t *testing.T, nc *v1alpha1.SCNodeClass, s vendors.VendorSettings, region, runKey string,
) (*vendors.Prepared, error) {
	t.Helper()
	return e.d.Prepare(context.Background(), vendors.PrepareRequest{
		Region: region, Zone: region, InstanceType: "cx23", CapacityType: vendors.CapacityOnDemand,
		Type: vendors.CatalogType{Vendor: "hcloud", APIReference: "cx23"}, Settings: s, RunKey: runKey,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: conformanceCluster, vendors.TagNodeClaim: "claim-1", vendors.TagNodeClass: nc.Name,
			vendors.TagManagedBy: vendors.ManagedByValue,
		},
		Resources: e.engine.Provider(nc, "hcloud", region, region),
	})
}

func kindIDs(p *vendors.Prepared, kind vendors.ResourceKind) []string {
	var out []string
	for _, r := range p.Resources {
		if r.Kind == kind {
			out = append(out, r.ID)
		}
	}
	return out
}

func (e *engEnv) launch(t *testing.T, p *vendors.Prepared, name string, s vendors.VendorSettings) *vendors.Instance {
	t.Helper()
	inst, err := e.d.CreateInstance(context.Background(), vendors.CreateRequest{
		Region: "fsn1", Zone: "fsn1", InstanceType: "cx23", CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image: vendors.Image{Ref: fmt.Sprint(e.x86.ID)}, UserData: []byte("{}"), Settings: s, Prepared: p,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: conformanceCluster, vendors.TagNodeClaim: name, vendors.TagManagedBy: vendors.ManagedByValue,
			vendors.TagNodeClass: "class",
		},
		IdempotencyKey: "uid-" + name, Node: vendors.NodeInfo{Name: name},
	})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestPrepareCreatesTheManagedFirewall(t *testing.T) {
	e := newEngEnv(t)
	nc := e.nodeClass(t, nil)
	s := settingsOf(t, e.d, nc, options.Selection{})
	p, err := e.prepare(t, nc, s, "fsn1", "")
	if err != nil {
		t.Fatal(err)
	}
	fws := kindIDs(p, hcloud.KindFirewalls)
	if len(fws) != 1 || len(kindIDs(p, hcloud.KindNetworks)) != 0 || len(p.Devices) != 0 {
		t.Fatalf("a default launch needs one firewall and nothing else: %+v", p)
	}
	again, err := e.prepare(t, nc, s, "fsn1", "")
	if err != nil || kindIDs(again, hcloud.KindFirewalls)[0] != fws[0] {
		t.Fatalf("Prepare is idempotent: %v, %v", again, err)
	}
	list, err := e.d.List(context.Background(), []string{"fsn1"}, nil)
	if err != nil || len(list) != 1 || !list[0].Managed || list[0].Kind != hcloud.KindFirewalls ||
		list[0].Tags[vendors.TagResource] != "firewalls" || !strings.HasPrefix(list[0].Name, "scarpenter-conformance-firewalls-fsn1") {
		t.Fatalf("the managed firewall: %+v, %v", list, err)
	}
	fw, _ := e.f.GetFirewall(context.Background(), mustID(t, fws[0]))
	if len(fw.Rules) != 0 || fw.Labels[hcloud.LabelLocation] != "fsn1" {
		t.Errorf("a default firewall has no rules (inbound is denied) and records its location: %+v", fw)
	}
}

func mustID(t *testing.T, s string) int64 {
	t.Helper()
	id, err := hcloud.ServerID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestInboundRulesBecomeFirewallRules(t *testing.T) {
	e := newEngEnv(t)
	nc := e.nodeClass(t, func(nc *v1alpha1.SCNodeClass, b *hcapi.Settings) {
		b.Networking = &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{
			{Protocol: "tcp", FromPort: new(int32(443)), ToPort: new(int32(443)), CIDRs: []string{"0.0.0.0/0"}},
			{Protocol: "any", CIDRs: []string{"10.0.0.0/8"}},
		}}}
	})
	p, err := e.prepare(t, nc, settingsOf(t, e.d, nc, options.Selection{}), "fsn1", "")
	if err != nil {
		t.Fatal(err)
	}
	fw, _ := e.f.GetFirewall(context.Background(), mustID(t, kindIDs(p, hcloud.KindFirewalls)[0]))
	// tcp 443, and "any" as tcp, udp and icmp.
	if len(fw.Rules) != 4 || *fw.Rules[0].Port != "443" || fw.Rules[3].Port != nil {
		t.Errorf("the rules: %+v", fw.Rules)
	}
	other := e.nodeClass(t, func(nc *v1alpha1.SCNodeClass, _ *hcapi.Settings) { nc.Name = "other" })
	q, err := e.prepare(t, other, settingsOf(t, e.d, other, options.Selection{}), "fsn1", "")
	if err != nil || kindIDs(q, hcloud.KindFirewalls)[0] == kindIDs(p, hcloud.KindFirewalls)[0] {
		t.Errorf("another rule set is another firewall: %v %v", q, err)
	}
}

func TestBroughtFirewallAndNetwork(t *testing.T) {
	e := newEngEnv(t)
	fw, _ := e.f.CreateFirewall(context.Background(), hc.FirewallCreateOpts{
		Name: "mine", Labels: map[string]string{"team": "a"}})
	nw := e.f.AddNetwork("net-fsn1", "10.8.0.0/21", map[string]string{"team": "a"}, hc.NetworkZoneEUCentral)
	nc := e.nodeClass(t, func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
		b.Resources = &hcapi.Resources{
			Firewalls: &common.Resource{Use: &common.ResourceUse{Tags: map[string]string{"team": "a"}}},
			Networks:  &common.Resource{Use: &common.ResourceUse{Name: "net-{zone}"}},
		}
	})
	s := settingsOf(t, e.d, nc, options.Selection{})
	p, err := e.prepare(t, nc, s, "fsn1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := kindIDs(p, hcloud.KindFirewalls); len(got) != 1 || got[0] != fmt.Sprint(fw.ID) {
		t.Errorf("the brought firewall: %v", got)
	}
	if got := kindIDs(p, hcloud.KindNetworks); len(got) != 1 || got[0] != fmt.Sprint(nw.ID) {
		t.Errorf("the brought network: %v", got)
	}
	if list, _ := e.d.List(context.Background(), nil, nil); len(list) != 0 {
		t.Errorf("nothing is created when everything is brought: %v", list)
	}
	inst := e.launch(t, p, "n1", s)
	srv, _ := e.f.GetServer(context.Background(), mustID(t, inst.Ref.ID()))
	if len(srv.PrivateNet) != 1 || srv.PrivateNet[0].Network.ID != nw.ID || len(srv.PublicNet.Firewalls) != 1 {
		t.Errorf("the server joins the brought network and firewall: %+v", srv)
	}

	// A per-location name that matches nothing in ash.
	_, err = e.prepare(t, nc, s, "ash", "")
	if vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Errorf("no network named for ash: %v, want ResourceMissingInZone", err)
	}

	// The network by ID has no subnet in the network zone of ash: the stage says so.
	byID := e.nodeClass(t, func(nc *v1alpha1.SCNodeClass, b *hcapi.Settings) {
		nc.Name = "by-id"
		b.Resources = &hcapi.Resources{Networks: &common.Resource{Use: &common.ResourceUse{
			IDs: map[string]common.IDList{"*": {fmt.Sprint(nw.ID)}}}}}
	})
	_, err = e.prepare(t, byID, settingsOf(t, e.d, byID, options.Selection{}), "ash", "")
	if vendors.CodeOf(err) != vendors.CodeResourceMissing || !strings.Contains(err.Error(), "subnet") {
		t.Errorf("a network without a subnet in the location: %v, want ResourceMissing naming the subnet stage", err)
	}
}

func TestDataVolumesAreCreatedBeforeTheServer(t *testing.T) {
	e := newEngEnv(t)
	nc := e.nodeClass(t, func(nc *v1alpha1.SCNodeClass, _ *hcapi.Settings) {
		nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{"db": {HCloud: &hcapi.Storage{Data: []hcapi.DataVolume{
			{GuestVolume: common.GuestVolume{Name: "pgdata", Filesystem: "xfs", MountPath: "/var/lib/pgdata"}, Size: resource.MustParse("100Gi")},
			{GuestVolume: common.GuestVolume{Name: "wal", MountPath: "/wal"}, Size: resource.MustParse("20G")},
		}}}}
	})
	s := settingsOf(t, e.d, nc, options.Selection{Storage: "db"})
	p, err := e.prepare(t, nc, s, "fsn1", "")
	if err != nil {
		t.Fatal(err)
	}
	vols := kindIDs(p, hcloud.KindVolumes)
	if len(vols) != 2 || len(p.Devices) != 2 {
		t.Fatalf("two volumes and two device hints: %+v", p)
	}
	for i, d := range p.Devices {
		if d.ByID != hcloud.VolumeByIDPath(mustID(t, vols[i])) || d.SerialPattern != "" {
			t.Errorf("the device is known by its by-id link: %+v", d)
		}
	}
	if p.Devices[0].SizeBytes != 108_000_000_000 {
		t.Errorf("100Gi is 108 GB: %d", p.Devices[0].SizeBytes)
	}
	if p.Devices[1].Filesystem != "ext4" || p.Devices[1].SizeBytes != 20_000_000_000 {
		t.Errorf("the default filesystem is ext4 and 20G is 20 GB: %+v", p.Devices[1])
	}
	v, _ := e.f.GetVolume(context.Background(), mustID(t, vols[0]))
	if v.Size != 108 || v.Location.Name != "fsn1" || v.Labels[hcloud.TagKey(vendors.TagResource)] != "volumes" {
		t.Errorf("the volume of 100Gi is 108 GB (rounded up), in the location, with the marker: %+v", v)
	}
	if again, err := e.prepare(t, nc, s, "fsn1", ""); err != nil || !slices.Equal(kindIDs(again, hcloud.KindVolumes), vols) {
		t.Errorf("Prepare is idempotent: %v %v", again, err)
	}

	// The volumes are attached at create, and outlive the server until the engine
	// collects them: perUse, after the grace.
	inst := e.launch(t, p, "claim-1", s)
	srv, _ := e.f.GetServer(context.Background(), mustID(t, inst.Ref.ID()))
	if len(srv.Volumes) != 2 {
		t.Fatalf("attached volumes: %+v", srv.Volumes)
	}
	if err := e.engine.Sweep(context.Background(), sets.New[string]()); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.d.List(context.Background(), nil, nil); len(list) != 3 {
		t.Errorf("a firewall and two volumes in use are kept: %v", list)
	}
	if err := e.d.DeleteInstance(context.Background(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	e.f.Tick()
	// First sighting without use starts the clock; after the grace the volumes go.
	for range 3 {
		e.clk.Step(3 * time.Minute)
		if err := e.engine.Sweep(context.Background(), sets.New[string]()); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := e.d.List(context.Background(), nil, nil)
	if len(list) != 1 || list[0].Kind != hcloud.KindFirewalls {
		t.Errorf("only the firewall remains (idleTTL 24h): %v", list)
	}
	e.clk.Step(25 * time.Hour)
	for range 2 {
		if err := e.engine.Sweep(context.Background(), sets.New[string]()); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := e.d.List(context.Background(), nil, nil); len(list) != 0 {
		t.Errorf("the idle firewall is deleted: %v", list)
	}
}

func TestPlacementGroupPerRunKey(t *testing.T) {
	e := newEngEnv(t)
	nc := e.nodeClass(t, func(nc *v1alpha1.SCNodeClass, _ *hcapi.Settings) {
		nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{
			"spread": {HCloud: &hcapi.Placement{PlacementGroup: &hcapi.PlacementGroup{Type: hcapi.PlacementGroupSpread}}},
			"shared": {HCloud: &hcapi.Placement{PlacementGroup: &hcapi.PlacementGroup{Use: &hcapi.PlacementGroupRef{Name: "mine"}}}},
		}
	})
	s := settingsOf(t, e.d, nc, options.Selection{Placement: "spread"})
	if _, err := e.prepare(t, nc, s, "fsn1", ""); vendors.CodeOf(err) != hcloud.CodeInvalidSettings {
		t.Errorf("a created group needs a run key: %v", err)
	}
	a, err := e.prepare(t, nc, s, "fsn1", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.prepare(t, nc, s, "fsn1", "run-b")
	if err != nil || kindIDs(a, hcloud.KindPlacementGroups)[0] == kindIDs(b, hcloud.KindPlacementGroups)[0] {
		t.Fatalf("a group per run key: %v %v", b, err)
	}
	inst := e.launch(t, a, "claim-a", s)
	srv, _ := e.f.GetServer(context.Background(), mustID(t, inst.Ref.ID()))
	if srv.PlacementGroup == nil || fmt.Sprint(srv.PlacementGroup.ID) != kindIDs(a, hcloud.KindPlacementGroups)[0] {
		t.Errorf("the server is in the group: %+v", srv.PlacementGroup)
	}

	// A brought group by name.
	g, _ := e.f.CreatePlacementGroup(context.Background(), hc.PlacementGroupCreateOpts{
		Name: "mine", Type: hc.PlacementGroupTypeSpread})
	shared := settingsOf(t, e.d, nc, options.Selection{Placement: "shared"})
	p, err := e.prepare(t, nc, shared, "fsn1", "")
	if err != nil || kindIDs(p, hcloud.KindPlacementGroups)[0] != fmt.Sprint(g.ID) {
		t.Errorf("the brought group: %v %v", p, err)
	}
	if _, err := e.prepare(t, nc, settingsOf(t, e.d, nc, options.Selection{Placement: "shared"}), "fsn1", ""); err != nil {
		t.Errorf("a run key is not needed for a brought group: %v", err)
	}
}

func TestResolutionOrderOfTheFirewall(t *testing.T) {
	e := newEngEnv(t)
	optIn := map[string]string{e.d.TagKey(vendors.TagUse): vendors.OptInValue}
	tagged, err := e.f.CreateFirewall(context.Background(), hc.FirewallCreateOpts{Name: "tagged", Labels: optIn})
	if err != nil {
		t.Fatal(err)
	}
	nc := e.nodeClass(t, nil)
	s := settingsOf(t, e.d, nc, options.Selection{})
	p, err := e.prepare(t, nc, s, "fsn1", "")
	if err != nil || !slices.Equal(kindIDs(p, hcloud.KindFirewalls), []string{fmt.Sprint(tagged.ID)}) {
		t.Fatalf("a tagged firewall is used as found: %v %v", p, err)
	}
	if list, _ := e.d.List(context.Background(), nil, nil); len(list) != 0 {
		t.Errorf("nothing was created: %v", list)
	}

	// An explicit use that matches nothing fails and does not create.
	missing := e.nodeClass(t, func(nc *v1alpha1.SCNodeClass, b *hcapi.Settings) {
		nc.Name = "missing"
		b.Resources = &hcapi.Resources{Firewalls: &common.Resource{Use: &common.ResourceUse{Name: "nothing"}}}
	})
	_, err = e.prepare(t, missing, settingsOf(t, e.d, missing, options.Selection{}), "fsn1", "")
	if vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Errorf("an explicit use that matches nothing: %v", err)
	}
	// Forbid with nothing tagged: no creation.
	e2 := newEngEnv(t)
	forbid := e2.nodeClass(t, func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
		b.Resources = &hcapi.Resources{Firewalls: &common.Resource{Forbid: true}}
	})
	_, err = e2.prepare(t, forbid, settingsOf(t, e2.d, forbid, options.Selection{}), "fsn1", "")
	if vendors.CodeOf(err) != vendors.CodeResourceForbidden {
		t.Errorf("forbid: %v", err)
	}
	if list, _ := e2.d.List(context.Background(), nil, nil); len(list) != 0 {
		t.Errorf("a forbidden creation creates nothing: %v", list)
	}
}

func TestStatusCheckOfTheResources(t *testing.T) {
	e := newEngEnv(t)
	e.f.AddNetwork("net", "10.9.0.0/21", map[string]string{"k": "v"}, hc.NetworkZoneEUCentral)
	nc := e.nodeClass(t, func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
		b.Resources = &hcapi.Resources{Networks: &common.Resource{Use: &common.ResourceUse{Tags: map[string]string{"k": "v"}}}}
	})
	rep := e.engine.Check(context.Background(), nc, "hcloud", []string{"fsn1", "ash"})
	bad, why := rep.NotReady()
	if len(bad) != 1 || bad[0] != "ash" || !strings.Contains(why["ash"], "networks") {
		t.Errorf("the network has a subnet in fsn1's zone only: %v %v", bad, why)
	}
}

func mustQuantity(s string) resource.Quantity { return resource.MustParse(s) }

func hcfakeErr() error { return hcfake.APIError(hc.ErrorCodeServerError, "down") }
