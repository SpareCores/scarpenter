/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
)

// provider is a ResourceProvider without the engine's policies: it resolves the
// uses of the test and ensures the rest by a deterministic name, as the engine does.
type provider struct {
	d      *gcp.Driver
	region string
	use    map[vendors.ResourceKind]vendors.UseRef
	calls  []vendors.ResourceRequest
}

func (p *provider) Require(ctx context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	p.calls = append(p.calls, req)
	if u, ok := p.use[req.Kind]; ok {
		return p.d.Resolve(ctx, req.Kind, p.region, u)
	}
	if req.Use != nil {
		return p.d.Resolve(ctx, req.Kind, p.region, *req.Use)
	}
	if req.Kind == gcp.KindSubnets {
		// The managed network is one resource per region.
		ref, _, err := p.d.Ensure(ctx, req.Kind, p.region, vendors.CreateSpec{
			Name: "scarpenter-conformance-subnets-" + p.region, Tags: marker})
		return []vendors.ResourceRef{ref}, err
	}
	sum := sha256.Sum256([]byte(req.Key))
	name := fmt.Sprintf("scarpenter-conformance-%s-%s-%s", req.Kind, p.region, hex.EncodeToString(sum[:])[:6])
	ref, _, err := p.d.Ensure(ctx, req.Kind, p.region, vendors.CreateSpec{Name: name, Tags: marker, Params: req.Params})
	return []vendors.ResourceRef{ref}, err
}

func prepareReq(e *env, p *provider, settings string) vendors.PrepareRequest {
	s := e.d.Defaults()
	if settings != "" {
		s.Raw = json.RawMessage(settings)
	}
	return vendors.PrepareRequest{Region: testRegion, Zone: testZone, InstanceType: "n2-standard-8", CapacityType: "on-demand",
		Settings: s, Tags: map[vendors.TagKey]string{vendors.TagNodeClass: "class"}, Resources: p}
}

func TestPrepareManagedResources(t *testing.T) {
	e := newEnv(t, envOptions{})
	p := &provider{d: e.d, region: testRegion}
	req := prepareReq(e, p, "")
	req.Settings.Networking = common.Networking{Isolation: common.IsolationRun, Inbound: &common.Inbound{Rules: []common.InboundRule{
		{Protocol: "tcp", FromPort: new(int32(443)), CIDRs: []string{"0.0.0.0/0"}}}}}
	req.RunKey = "run-1"
	prep, err := e.d.Prepare(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[vendors.ResourceKind]int{}
	for _, r := range prep.Resources {
		kinds[r.Kind]++
	}
	if kinds[gcp.KindSubnets] != 1 || kinds[gcp.KindFirewalls] != 1 || kinds[gcp.KindRunFirewalls] != 1 {
		t.Errorf("prepared %v", kinds)
	}
	// Idempotent: a second Prepare finds the same resources and creates nothing new.
	n := len(e.c.Firewalls())
	again, err := e.d.Prepare(t.Context(), req)
	if err != nil || len(e.c.Firewalls()) != n || again.Resources[0].ID != prep.Resources[0].ID {
		t.Errorf("a second Prepare: %v, %d rules (was %d)", err, len(e.c.Firewalls()), n)
	}
	// The firewall asked for the inbound rules and no self rule for isolation run
	// with a run key (the run has its own).
	var params gcp.FirewallParams
	for _, c := range p.calls {
		if c.Kind == gcp.KindFirewalls {
			_ = json.Unmarshal(c.Params, &params)
			if c.Network == "" || c.Key == "" {
				t.Errorf("the firewall request has no network or key: %+v", c)
			}
		}
	}
	if params.SelfReference || len(params.Inbound) != 1 {
		t.Errorf("firewall params = %+v", params)
	}
}

func TestPrepareDevicesAndLocalDisks(t *testing.T) {
	e := newEnv(t, envOptions{})
	p := &provider{d: e.d, region: testRegion}
	req := prepareReq(e, p, `{"storage":{"data":[{"name":"pgdata","diskType":"pd-ssd","sizeGb":50,"mountPath":"/d"}],`+
		`"localStorage":{"policy":"raid0","mountPath":"/mnt/l"}}}`)
	req.Type = vendors.CatalogType{LocalStorageCount: 2}
	prep, err := e.d.Prepare(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(prep.Devices) != 1 || prep.Devices[0].ByID != "/dev/disk/by-id/google-pgdata" || prep.Devices[0].Filesystem != "ext4" ||
		prep.Devices[0].MountPath != "/d" {
		t.Errorf("devices = %+v", prep.Devices)
	}
	if prep.Local == nil || prep.Local.Policy != "raid0" || prep.Local.Count != 2 || prep.Local.DevicePattern == "" {
		t.Errorf("local = %+v", prep.Local)
	}
}

func TestPreparePlacementPolicies(t *testing.T) {
	e := newEnv(t, envOptions{})
	p := &provider{d: e.d, region: testRegion}
	// A policy of a profile needs a run key.
	req := prepareReq(e, p, "")
	req.Settings.Placement = json.RawMessage(`{"resourcePolicy":{"groupPlacement":{"collocation":"COLLOCATED"}}}`)
	req.Settings.Profiles = map[string]string{vendors.ProfilePlacement: "colocated"}
	if _, err := e.d.Prepare(t.Context(), req); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a policy without a run key: %v", err)
	}
	req.RunKey = "run-1"
	prep, err := e.d.Prepare(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if last := prep.Resources[len(prep.Resources)-1]; last.Kind != gcp.KindResourcePolicies || !last.Managed {
		t.Errorf("resources = %+v", prep.Resources)
	}
	// A policy by name is looked up, never created.
	created, _, _ := e.d.Ensure(t.Context(), gcp.KindResourcePolicies, testRegion, spec("scarpenter-conformance-theirs", `{"availabilityDomainCount":2}`))
	req.Settings.Placement = json.RawMessage(`{"resourcePolicy":{"use":{"name":"` + created.Name + `"}}}`)
	prep, err = e.d.Prepare(t.Context(), req)
	if err != nil || prep.Resources[len(prep.Resources)-1].ID != created.ID {
		t.Errorf("a policy by name: %+v, %v", prep, err)
	}
	req.Settings.Placement = json.RawMessage(`{"resourcePolicy":{"use":{"name":"missing"}}}`)
	if _, err := e.d.Prepare(t.Context(), req); err == nil {
		t.Error("a missing policy prepared")
	}
	req.Settings.Placement = json.RawMessage(`{`)
	if _, err := e.d.Prepare(t.Context(), req); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a bad placement block: %v", err)
	}
	req.Settings.Placement = nil
	req.Settings.Raw = json.RawMessage(`{`)
	if _, err := e.d.Prepare(t.Context(), req); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("bad settings: %v", err)
	}
}

func TestPrepareChoosesAnIPv6Subnet(t *testing.T) {
	e := newEnv(t, envOptions{})
	// The seeded user subnetwork is dual stack but its IPv6 range is internal: not
	// usable for a public IPv6 address.
	p := &provider{d: e.d, region: testRegion, use: map[vendors.ResourceKind]vendors.UseRef{
		gcp.KindSubnets: {Name: "nodes"}}}
	req := prepareReq(e, p, "")
	req.Settings.Networking.PublicIPv6 = new(true)
	if _, err := e.d.Prepare(t.Context(), req); vendors.ClassOf(err) != vendors.Unsupported ||
		vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Errorf("an IPv4-only subnetwork with publicIPv6: %v", err)
	}
	// The managed subnetwork has an external IPv6 range.
	p = &provider{d: e.d, region: testRegion}
	if prep, err := e.d.Prepare(t.Context(), withProvider(req, p)); err != nil || prep.Resources[0].Kind != gcp.KindSubnets {
		t.Errorf("the managed network with publicIPv6: %v", err)
	}
}

func TestPrepareWithoutSubnets(t *testing.T) {
	e := newEnv(t, envOptions{})
	p := &emptyProvider{}
	req := prepareReq(e, nil, "")
	req.Resources = p
	if _, err := e.d.Prepare(t.Context(), req); !vendors.IsClass(err, vendors.Unsupported) {
		t.Errorf("no subnets: %v", err)
	}
}

func withProvider(r vendors.PrepareRequest, p vendors.ResourceProvider) vendors.PrepareRequest {
	r.Resources = p
	return r
}

type emptyProvider struct{}

func (emptyProvider) Require(context.Context, vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	return nil, nil
}

func TestSupportedProfiles(t *testing.T) {
	d := gcp.New()
	profiles := vendors.Profiles{
		vendors.ProfileStorage: {
			"hd":     json.RawMessage(`{"data":[{"name":"d","diskType":"hyperdisk-balanced","sizeGb":10,"mountPath":"/d"}]}`),
			"pd":     json.RawMessage(`{"root":{"diskType":"pd-balanced"}}`),
			"hdroot": json.RawMessage(`{"root":{"diskType":"hyperdisk-balanced"}}`),
			"local":  json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/l"}}`),
			"ignore": json.RawMessage(`{"localStorage":{"policy":"ignore"}}`),
			"broken": json.RawMessage(`{`),
		},
		vendors.ProfileNetwork: {
			"tier1": json.RawMessage(`{"tier1":true}`), "gvnic": json.RawMessage(`{"nicType":"GVNIC"}`),
			"broken": json.RawMessage(`{`),
		},
		vendors.ProfilePlacement: {
			"compact": json.RawMessage(`{"resourcePolicy":{"groupPlacement":{"collocation":"COLLOCATED"}}}`),
			"spread":  json.RawMessage(`{"resourcePolicy":{"spread":{"availabilityDomainCount":2}}}`),
			"broken":  json.RawMessage(`{`),
		},
	}
	type tc struct {
		it   vendors.CatalogType
		want map[string][]string
	}
	for _, c := range []tc{
		{vendors.CatalogType{APIReference: "e2-small", VCPUs: 2}, map[string][]string{
			vendors.ProfileStorage: {"ignore", "pd"}, vendors.ProfileNetwork: {"gvnic"}, vendors.ProfilePlacement: {"spread"}}},
		{vendors.CatalogType{APIReference: "n2-standard-32", VCPUs: 32}, map[string][]string{
			vendors.ProfileStorage: {"hd", "hdroot", "ignore", "pd"}, vendors.ProfileNetwork: {"gvnic", "tier1"},
			vendors.ProfilePlacement: {"compact", "spread"}}},
		{vendors.CatalogType{APIReference: "c4a-standard-4", VCPUs: 4}, map[string][]string{
			vendors.ProfileStorage: {"hd", "hdroot", "ignore"}, vendors.ProfileNetwork: {"gvnic"},
			vendors.ProfilePlacement: {"compact", "spread"}}},
		{vendors.CatalogType{APIReference: "c3-standard-8-lssd", VCPUs: 8, LocalStorageCount: 2}, map[string][]string{
			vendors.ProfileStorage: {"hd", "hdroot", "ignore", "local", "pd"}, vendors.ProfileNetwork: {"gvnic"},
			vendors.ProfilePlacement: {"compact", "spread"}}},
	} {
		got := d.Supported(c.it, testZone, profiles)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: %v, want %v", c.it.APIReference, got, c.want)
		}
	}
}

func TestPriceDeltaIncompleteness(t *testing.T) {
	prices := gcp.StaticPrices{Storage: map[string]float64{testRegion + "/pd-balanced": 0.1, testRegion + "/hyperdisk-balanced": 0.2},
		IPv4: map[string]float64{testRegion: 0.01}}
	e := newEnv(t, envOptions{prices: prices})
	off := vendors.Offering{Region: testRegion, APIReference: "n2-standard-8"}
	s := e.d.Defaults()
	if p, ok := e.d.PriceDelta(s, off); !ok || p < 64*0.1/730 {
		t.Errorf("defaults: %v, %v", p, ok)
	}
	s.Networking.PublicIPv4 = new(false)
	pNoIP, _ := e.d.PriceDelta(s, off)
	if d := pNoIP - 64*0.1/730; d < -1e-12 || d > 1e-12 {
		t.Errorf("without the address: %v", pNoIP)
	}
	for name, raw := range map[string]string{
		"more IOPS than the baseline": `{"storage":{"root":{"diskType":"hyperdisk-balanced","sizeGb":10,"provisionedIops":5000}}}`,
		"more throughput":             `{"storage":{"root":{"diskType":"hyperdisk-balanced","sizeGb":10,"provisionedThroughput":500}}}`,
		"an unpriced disk type":       `{"storage":{"data":[{"name":"d","diskType":"pd-ssd","sizeGb":10,"mountPath":"/d"}]}}`,
	} {
		v := vendors.VendorSettings{Raw: json.RawMessage(raw)}
		if _, ok := e.d.PriceDelta(v, off); ok {
			t.Errorf("%s: complete", name)
		}
	}
	v := e.d.Defaults()
	v.Network = json.RawMessage(`{"tier1":true}`)
	if _, ok := e.d.PriceDelta(v, off); ok {
		t.Error("tier 1 is priced")
	}
	if _, ok := e.d.PriceDelta(vendors.VendorSettings{Raw: json.RawMessage(`{`)}, off); ok {
		t.Error("bad settings are priced")
	}
	// Without a price source every price is incomplete.
	if _, ok := gcp.New().PriceDelta(s, off); ok {
		t.Error("no source is complete")
	}
}

func TestRootDiskDefaults(t *testing.T) {
	d := gcp.New()
	s := d.Defaults()
	if d.RootDiskBytes(s) != 64<<30 || s.RootDiskBytes != 64<<30 {
		t.Errorf("default root disk = %d", d.RootDiskBytes(s))
	}
	s.Raw = json.RawMessage(`{"storage":{"root":{"sizeGb":100}}}`)
	if d.RootDiskBytes(s) != 100<<30 {
		t.Errorf("root disk = %d", d.RootDiskBytes(s))
	}
	s.Raw = json.RawMessage(`{`)
	if d.RootDiskBytes(s) != 64<<30 {
		t.Errorf("a broken block falls back to the default: %d", d.RootDiskBytes(s))
	}
	if _, ok := d.RootDiskForType(s, vendors.CatalogType{}); ok {
		t.Error("the type defines the root disk")
	}
}

func TestSeriesTables(t *testing.T) {
	for _, tc := range []struct {
		typ, disk    string
		boot, attach bool
	}{
		{"e2-small", "pd-balanced", true, true}, {"e2-small", "hyperdisk-balanced", false, false},
		{"c4-standard-4", "pd-balanced", false, true}, {"c4-standard-4", "hyperdisk-balanced", true, true},
		{"n2-standard-2", "hyperdisk-balanced", true, true},
	} {
		if gcp.CanBootFrom(tc.typ, tc.disk) != tc.boot || gcp.CanAttach(tc.typ, tc.disk) != tc.attach {
			t.Errorf("%s/%s: boot %v attach %v", tc.typ, tc.disk, gcp.CanBootFrom(tc.typ, tc.disk), gcp.CanAttach(tc.typ, tc.disk))
		}
	}
	if gcp.DiskInterface("c3-standard-4") != "NVME" || gcp.DiskInterface("e2-small") != "" {
		t.Error("disk interfaces")
	}
	if gcp.MaintenanceFor("e2-small", false) != "MIGRATE" || gcp.MaintenanceFor("e2-small", true) != "TERMINATE" ||
		gcp.MaintenanceFor("c4a-standard-4", false) != "TERMINATE" {
		t.Error("maintenance")
	}
	// Every series of the snapshot that the driver offers is classified for the root
	// disk: a Hyperdisk-only series boots from Hyperdisk, the others from a PD type.
	for _, r := range gcpServers(t) {
		def := gcp.DefaultRootDiskType(r.APIReference)
		if !gcp.CanBootFrom(r.APIReference, def) {
			t.Errorf("%s cannot boot from its own default %s", r.APIReference, def)
		}
	}
}
