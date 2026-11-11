/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestRootDisk(t *testing.T) {
	d := New()
	def := d.Defaults()
	if def.RootDiskBytes != 0 || d.RootDiskBytes(def) != 0 {
		t.Errorf("the default root is the flavor's disk: %d", def.RootDiskBytes)
	}
	b3 := vendors.CatalogType{APIReference: "b3-8", LocalStorageGB: 50, LocalStorageCount: 1}
	if b, ok := d.RootDiskForType(def, b3); ok || b != 0 {
		t.Errorf("a flavor's own disk is the catalog's: %d %v", b, ok)
	}
	// The i1 types add their NVMe disks into Keeper's storage_size: the root is the 50 GB system disk.
	i1 := vendors.CatalogType{APIReference: "i1-45", LocalStorageGB: 1950, LocalStorageCount: 3}
	if b, ok := d.RootDiskForType(def, i1); !ok || b != 50e9 {
		t.Errorf("i1: %d %v", b, ok)
	}
	boot := vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"bootFromVolume":{"volumeType":"high-speed","size":"100Gi"}}}}`)}
	boot.RootDiskBytes = d.RootDiskBytes(boot)
	for _, it := range []vendors.CatalogType{b3, i1} {
		if b, ok := d.RootDiskForType(boot, it); !ok || b != 100<<30 {
			t.Errorf("%s boots from the volume: %d %v", it.APIReference, b, ok)
		}
	}
	if d.RootDiskBytes(vendors.VendorSettings{Raw: json.RawMessage(`{`)}) != 0 {
		t.Error("settings that do not decode have no root size")
	}
}

func TestSupportedProfiles(t *testing.T) {
	d := New()
	p := vendors.Profiles{
		vendors.ProfileStorage: {
			"ok":     json.RawMessage(`{"data":[{"name":"a","volumeType":"classic","size":"10Gi","mountPath":"/a"}]}`),
			"local":  json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/l"}}`),
			"tiny":   json.RawMessage(`{"root":{"bootFromVolume":{"volumeType":"classic","size":"5Gi"}}}`),
			"broken": json.RawMessage(`{"data":"x"}`),
		},
		vendors.ProfilePlacement: {
			"anti":   json.RawMessage(`{"serverGroup":{"policy":"anti-affinity"}}`),
			"both":   json.RawMessage(`{"serverGroup":{"policy":"affinity","use":{"name":"g"}}}`),
			"soft":   json.RawMessage(`{"serverGroup":{"policy":"soft-affinity"}}`),
			"broken": json.RawMessage(`[`),
		},
		vendors.ProfileNetwork: {"fast": json.RawMessage(`{}`)},
	}
	got := d.Supported(vendors.CatalogType{APIReference: "b3-8"}, "nova", p)
	if !slices.Equal(got[vendors.ProfileStorage], []string{"ok"}) || !slices.Equal(got[vendors.ProfilePlacement], []string{"anti"}) ||
		len(got[vendors.ProfileNetwork]) != 0 {
		t.Errorf("only valid profiles are supported, localStorage and network profiles never: %v", got)
	}
}

func TestPriceDelta(t *testing.T) {
	d := New(WithPrices(StaticPrices{"GRA11/high-speed": 0.073, "GRA11/classic": 0.0365}))
	off := vendors.Offering{Region: "GRA11", CapacityType: vendors.CapacityOnDemand, APIReference: "b3-8"}
	delta := func(raw string) (float64, bool) {
		return d.PriceDelta(vendors.VendorSettings{Raw: json.RawMessage(raw)}, off)
	}
	if p, ok := delta(`{}`); p != 0 || !ok {
		t.Errorf("the flavor's own disk is in its price: %v %v", p, ok)
	}
	p, ok := delta(`{"storage":{"root":{"bootFromVolume":{"volumeType":"high-speed","size":"100Gi"}},"data":[{"name":"a","volumeType":"classic","size":"730Gi","mountPath":"/a"}]}}`)
	if want := 100*0.073/730 + 730*0.0365/730; !ok || fmt.Sprintf("%.6f", p) != fmt.Sprintf("%.6f", want) {
		t.Errorf("%v %v, want %v", p, ok, want)
	}
	if _, ok := delta(`{"storage":{"data":[{"name":"a","volumeType":"high-speed-gen2","size":"10Gi","mountPath":"/a"}]}}`); ok {
		t.Error("a volume type without a price is incomplete")
	}
	if _, ok := delta(`{`); ok {
		t.Error("settings that do not decode are incomplete")
	}
	if _, ok := New().PriceDelta(vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"bootFromVolume":{"volumeType":"classic","size":"10Gi"}}}}`)}, off); ok {
		t.Error("without a price source nothing is complete")
	}
}

// directResources is the engine's job in miniature: Require resolves a use or
// ensures a deterministic resource.
type directResources struct {
	d    *Driver
	last []vendors.ResourceRequest
}

func (r *directResources) Require(ctx context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	r.last = append(r.last, req)
	if req.Use != nil {
		return r.d.Resolve(ctx, req.Kind, testRegion, *req.Use)
	}
	tags := groupTags(req.Kind)
	name := fmt.Sprintf("scarpenter-%s-%s-%d", conformanceCluster, req.Kind, len(r.last))
	if req.Kind == KindSecurityGroups {
		name = "scarpenter-conformance-securitygroups"
	}
	ref, _, err := r.d.Ensure(ctx, req.Kind, testRegion, vendors.CreateSpec{Name: name, Tags: tags, Params: req.Params})
	return []vendors.ResourceRef{ref}, err
}

func TestPrepare(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	res := &directResources{d: d}
	settings := func(net common.Networking, placement string) vendors.VendorSettings {
		s := vendors.VendorSettings{Networking: net, Profiles: map[string]string{vendors.ProfilePlacement: "spread"}}
		s.Raw = json.RawMessage(`{"configDrive":true,"storage":{"data":[{"name":"pg","volumeType":"high-speed","size":"100Gi","mountPath":"/pg","filesystem":"xfs"},{"name":"tmp","volumeType":"classic","size":"10Gi","mountPath":"/tmp2"}]}}`)
		if placement != "" {
			s.Placement = json.RawMessage(placement)
		}
		return s
	}
	prep := func(s vendors.VendorSettings, runKey string) (*vendors.Prepared, error) {
		return d.Prepare(ctx, vendors.PrepareRequest{
			Region: testRegion, Zone: testZone, InstanceType: testType, Settings: s, RunKey: runKey, Resources: res,
			Tags: map[vendors.TagKey]string{vendors.TagNodeClass: "class"},
		})
	}
	// Isolation run with a run key: the NodeClass group and a group of the run.
	net := common.Networking{Isolation: common.IsolationRun, Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", FromPort: i32(22), CIDRs: []string{"10.0.0.0/8"}}}}}
	p, err := prep(settings(net, `{"serverGroup":{"policy":"anti-affinity"}}`), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(p.Resources))
	for _, r := range p.Resources {
		kinds = append(kinds, string(r.Kind))
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"runSecurityGroups", "securityGroups", "serverGroups"}) {
		t.Errorf("resources %v", kinds)
	}
	if len(p.Devices) != 2 || p.Devices[0].Name != "pg" || p.Devices[0].Filesystem != "xfs" || p.Devices[0].SerialPattern != "*" ||
		p.Devices[0].SizeBytes != 100<<30 || p.Devices[1].Filesystem != "ext4" {
		t.Errorf("devices %+v", p.Devices)
	}
	var sgParams SecurityGroupParams
	_ = json.Unmarshal(res.last[0].Params, &sgParams)
	if sgParams.SelfReference || len(sgParams.Inbound) != 1 {
		t.Errorf("isolation run with a run key: no self-reference in the shared group, the rules in it: %+v", sgParams)
	}

	// Isolation nodeclass: self-reference in the shared group, no run group.
	res.last = nil
	p, err = prep(settings(common.Networking{Isolation: common.IsolationNodeClass}, ""), "run-1")
	if err != nil || len(p.Resources) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	_ = json.Unmarshal(res.last[0].Params, &sgParams)
	if !sgParams.SelfReference {
		t.Errorf("%+v", sgParams)
	}
	// A server group that is created needs a run key; one that is used does not.
	if _, err := prep(settings(common.Networking{}, `{"serverGroup":{"policy":"affinity"}}`), ""); vendors.CodeOf(err) != CodeInvalidSettings {
		t.Errorf("no run key: %v", err)
	}
	cl, _ := w.cloud.Clients(ctx, testRegion)
	g, _ := cl.Compute.CreateServerGroup(ctx, "mine", "affinity")
	p, err = prep(settings(common.Networking{}, `{"serverGroup":{"use":{"name":"mine"}}}`), "")
	if err != nil || !slices.ContainsFunc(p.Resources, func(r vendors.ResourceRef) bool { return r.ID == g.ID }) {
		t.Errorf("a server group to use: %+v %v", p, err)
	}
	if _, err := prep(settings(common.Networking{}, `{"serverGroup":{"use":{"name":"absent"}}}`), ""); err == nil {
		t.Error("a server group to use that does not exist")
	}
	_, _ = cl.Compute.CreateServerGroup(ctx, "twin", "affinity")
	_, _ = cl.Compute.CreateServerGroup(ctx, "twin", "affinity")
	if _, err := prep(settings(common.Networking{}, `{"serverGroup":{"use":{"name":"twin"}}}`), ""); err == nil {
		t.Error("two groups of one name")
	}
	if _, err := prep(settings(common.Networking{}, `[`), "r"); vendors.CodeOf(err) != CodeInvalidSettings {
		t.Errorf("a broken placement profile: %v", err)
	}
	bad := settings(common.Networking{}, "")
	bad.Raw = json.RawMessage(`{"storage":{"localStorage":{"policy":"mount"}}}`)
	if _, err := prep(bad, "r"); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("localStorage: %v", err)
	}
}

func TestProfilesResolveAgainstTheDefaults(t *testing.T) {
	d := New()
	in := options.Input{Block: json.RawMessage(`{"storage":{"root":{"bootFromVolume":{"volumeType":"classic","size":"50Gi"}}}}`), Profiles: vendors.Profiles{
		vendors.ProfileStorage: {"db": json.RawMessage(`{"data":[{"name":"a","volumeType":"classic","size":"10Gi","mountPath":"/a"}]}`)},
	}}
	if err := options.Validate(d, in); err != nil {
		t.Fatal(err)
	}
	s, err := options.Resolve(d, in, options.Selection{Storage: "db"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := ParseSettings(s.Raw)
	if err != nil || len(st.Storage.Data) != 1 || st.Storage.Root.BootFromVolume == nil || s.RootDiskBytes != 50<<30 {
		t.Errorf("the profile adds to the block's storage: %+v %v %d", st, err, s.RootDiskBytes)
	}
	if !strings.Contains(string(s.Raw), `"configDrive":true`) {
		t.Errorf("the defaults are merged: %s", s.Raw)
	}
}
