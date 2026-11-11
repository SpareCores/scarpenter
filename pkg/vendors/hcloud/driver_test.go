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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	hcapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
	hcfake "github.com/SpareCores/scarpenter/pkg/vendors/hcloud/fake"
)

func TestInit(t *testing.T) {
	ctx := context.Background()
	t.Setenv(hcloud.TokenEnv, "")
	d := hcloud.New(hcloud.WithRegisterer(nopRegisterer{}))
	if err := d.Init(ctx, vendors.Config{ClusterName: "c", Log: logr.Discard()}); err == nil ||
		!strings.Contains(err.Error(), "HCLOUD_TOKEN") {
		t.Errorf("without a token: %v", err)
	}
	if _, err := d.Regions(ctx); vendors.ClassOf(err) != vendors.Fatal {
		t.Errorf("a driver that was not initialised: %v", err)
	}
	if err := hcloud.New(hcloud.WithHCloudServerLimit(-1), hcloud.WithRegisterer(nopRegisterer{})).Init(
		ctx, vendors.Config{Log: logr.Discard()}); err == nil {
		t.Error("a negative server limit")
	}

	// With a token, the client talks to the configured endpoint: nothing is called by Init.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer tok" || !strings.HasPrefix(r.UserAgent(), "scarpenter/") {
			t.Errorf("the request: %v %q", r.Header, r.UserAgent())
		}
		w.Header().Set("RateLimit-Limit", "3600")
		w.Header().Set("RateLimit-Remaining", "100")
		_ = json.NewEncoder(w).Encode(map[string]any{"locations": []any{
			map[string]any{"id": 1, "name": "fsn1", "network_zone": "eu-central"}}, "meta": map[string]any{
			"pagination": map[string]any{"page": 1, "last_page": 1, "total_entries": 1}}})
	}))
	defer srv.Close()
	t.Setenv(hcloud.TokenEnv, "tok")
	t.Setenv(hcloud.EndpointEnv, srv.URL)
	reg := prometheus.NewRegistry()
	d = hcloud.New(hcloud.WithRegisterer(reg))
	if err := d.Init(ctx, vendors.Config{ClusterName: "c", Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("Init made %d API calls", calls)
	}
	regions, err := d.Regions(ctx)
	if err != nil || len(regions) != 1 || regions[0].Name != "fsn1" {
		t.Errorf("Regions: %v %v", regions, err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "scarpenter_hcloud_api_") {
			continue
		}
		seen++
		v := mf.GetMetric()[0].GetGauge().GetValue()
		if (mf.GetName() == "scarpenter_hcloud_api_remaining" && v != 100) || (mf.GetName() == "scarpenter_hcloud_api_limit" && v != 3600) {
			t.Errorf("%s = %v", mf.GetName(), v)
		}
	}
	if seen != 2 {
		t.Errorf("the rate limit gauges: %d", seen)
	}
	// A second driver in the same registry replaces the gauges.
	if err := hcloud.New(hcloud.WithRegisterer(reg)).Init(ctx, vendors.Config{ClusterName: "c", Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	if d.Vendor() != "hcloud" || d.TagKey(vendors.TagCluster) != "scarpenter.sparecores.com/cluster" {
		t.Error("identity")
	}
}

func TestKeeperPricesAndPriceDelta(t *testing.T) {
	rows := `[{"vendor_id":"hcloud","region_id":"fsn1","storage_id":"block","unit":"GB/month","price":0.0584,"currency":"USD","status":"active"},
	{"vendor_id":"hcloud","region_id":"nbg1","storage_id":"block","unit":"TB/month","price":9,"currency":"USD","status":"active"},
	{"vendor_id":"aws","region_id":"hel1","storage_id":"block","unit":"GB/month","price":1,"currency":"USD","status":"active"}]`
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		_, _ = w.Write([]byte(rows))
	}))
	defer srv.Close()
	t.Setenv(hcloud.IPv4PriceEnv, "0.001")
	f := hcfake.New()
	d := hcloud.New(hcloud.WithAPI(f), hcloud.WithRegisterer(nopRegisterer{}))
	if err := d.Init(context.Background(), vendors.Config{ClusterName: "c", Log: logr.Discard(), KeeperURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	vol := json.RawMessage(`{"data":[{"name":"a","size":"100G","mountPath":"/a"}]}`)
	s := vendors.VendorSettings{Raw: json.RawMessage(`{"storage":` + string(vol) + `}`)}
	off := vendors.Offering{Region: "fsn1", Zone: "fsn1"}
	if _, complete := d.PriceDelta(s, off); complete {
		t.Error("the volume price is unknown before Keeper answered")
	}
	// Before the load, a storage profile is accepted anywhere (nothing is known).
	if got := d.Supported(vendors.CatalogType{}, "sin", vendors.Profiles{"storage": {"a": vol}}); len(got["storage"]) != 1 {
		t.Errorf("Supported before the load: %v", got)
	}
	before := d.NodeOptionsEpoch()
	if err := d.LoadPrices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "/storage_prices?vendor=hcloud") || d.NodeOptionsEpoch() == before {
		t.Errorf("the request %q, epoch %d -> %d", path, before, d.NodeOptionsEpoch())
	}
	delta, complete := d.PriceDelta(s, off)
	want := 100*0.0584/730 + 0.001
	if !complete || delta < want-1e-9 || delta > want+1e-9 {
		t.Errorf("PriceDelta = %v, %v; want %v", delta, complete, want)
	}
	// IPv6-only has no IPv4 charge; no volumes, no price needed.
	s6 := vendors.VendorSettings{Networking: common.Networking{PublicIPv4: new(false)}}
	if delta, complete := d.PriceDelta(s6, vendors.Offering{Region: "sin"}); delta != 0 || !complete {
		t.Errorf("IPv6-only without volumes: %v %v", delta, complete)
	}
	// nbg1 has no usable row: its volume price is unknown and the profile unsupported there.
	if _, complete := d.PriceDelta(s, vendors.Offering{Zone: "nbg1"}); complete {
		t.Error("no price row for nbg1")
	}
	if got := d.Supported(vendors.CatalogType{}, "nbg1", vendors.Profiles{"storage": {"a": vol}}); len(got["storage"]) != 0 {
		t.Errorf("a location without a volume price: %v", got)
	}
	if got := d.Supported(vendors.CatalogType{}, "fsn1", vendors.Profiles{"storage": {"a": vol}}); len(got["storage"]) != 1 {
		t.Errorf("a location with one: %v", got)
	}
	t.Setenv(hcloud.IPv4PriceEnv, "free")
	if err := hcloud.New(hcloud.WithAPI(f), hcloud.WithRegisterer(nopRegisterer{})).Init(context.Background(),
		vendors.Config{Log: logr.Discard(), KeeperURL: srv.URL}); err == nil {
		t.Error("an invalid IPv4 price")
	}
	rows = "not json"
	if err := d.LoadPrices(context.Background()); err == nil {
		t.Error("an invalid answer")
	}
}

func TestSupportedAndRootDisk(t *testing.T) {
	e := newCEnv()
	d := e.driver(t)
	s := d.Defaults()
	if s.RootDiskBytes != 0 || string(s.Raw) != "{}" {
		t.Errorf("defaults: %+v", s)
	}
	ct := vendors.CatalogType{Vendor: "hcloud", APIReference: "cx23"}
	if b, ok := d.RootDiskForType(s, ct); ok || b != 0 {
		t.Errorf("before the table is loaded the catalog answers: %d %v", b, ok)
	}
	if _, err := d.LaunchableTypes(context.Background(), "fsn1"); err != nil {
		t.Fatal(err)
	}
	if b, ok := d.RootDiskForType(s, ct); !ok || b != 40_000_000_000 {
		t.Errorf("the root disk of cx23 is the type's 40 GB: %d %v", b, ok)
	}
	if _, ok := d.RootDiskForType(s, vendors.CatalogType{APIReference: "nope"}); ok {
		t.Error("an unknown type")
	}
	tooMany := `{"data":[` + strings.TrimSuffix(strings.Repeat(`{"name":"v","size":"10G","mountPath":"/v"},`, 17), ",") + `]}`
	got := d.Supported(ct, "fsn1", vendors.Profiles{
		"storage": {
			"ok": json.RawMessage(`{"data":[{"name":"a","size":"10G","mountPath":"/a"}]}`), "tiny": json.RawMessage(`{"data":[{"name":"a","size":"9G","mountPath":"/a"}]}`),
			"huge": json.RawMessage(`{"data":[{"name":"a","size":"11Ti","mountPath":"/a"}]}`), "many": json.RawMessage(tooMany),
			"bad": json.RawMessage(`{"data":"x"}`),
		},
		"placement": {"spread": json.RawMessage(`{"placementGroup":{"type":"spread"}}`), "bad": json.RawMessage(`[]`)},
		"network":   {"nic": json.RawMessage(`{}`)},
	})
	if fmt.Sprint(got) != "map[placement:[spread] storage:[ok]]" {
		t.Errorf("Supported = %v", got)
	}
	if _, complete := d.PriceDelta(vendors.VendorSettings{Raw: json.RawMessage(`[`)}, vendors.Offering{}); complete {
		t.Error("undecodable settings")
	}
}

func TestBillingRuleIsUnverified(t *testing.T) {
	r, ok := hcloud.New().BillingRule("fsn1", "cx")
	if !ok || r.Verified || r.Increment == nil || *r.Increment != time.Hour || r.StoppedCharged == nil || !*r.StoppedCharged ||
		r.Minimum != nil || r.Source == "" {
		t.Errorf("the rule: %+v", r)
	}
}

func TestQuotaReporterWithAndWithoutLimit(t *testing.T) {
	e := newCEnv()
	ctx := context.Background()
	d := e.driver(t, hcloud.WithHCloudServerLimit(5))
	e.f.AddForeignServer("nbg1", "cx23", nil)
	snap, err := d.GlobalQuotas(ctx)
	if err != nil || len(snap.Buckets) != 1 || snap.Buckets[0].Limit != 5 || snap.Buckets[0].Used != 1 ||
		snap.Buckets[0].Scope != vendors.QuotaScopeGlobal || len(snap.Counted) != 1 {
		t.Errorf("account-wide counting includes servers of other owners: %+v %v", snap, err)
	}
	if r, err := d.Quotas(ctx, "fsn1"); err != nil || len(r.Buckets) != 0 {
		t.Errorf("no regional bucket: %+v %v", r, err)
	}
	if c, ok := d.Consumption("cx23", vendors.CapacityOnDemand); !ok || c[0].Amount != 1 {
		t.Errorf("%v %v", c, ok)
	}
	if _, ok := d.Consumption("cx23", vendors.CapacitySpot); ok {
		t.Error("Hetzner has no spot")
	}
	unknown := e.driver(t, hcloud.WithHCloudServerLimit(0))
	if _, ok := unknown.Consumption("cx23", vendors.CapacityOnDemand); ok {
		t.Error("without a limit the headroom is unknown")
	}
}

func TestLaunchReplayInAnotherLocationAndSSHKey(t *testing.T) {
	e := newCEnv()
	ctx := context.Background()
	d := e.driver(t)
	key := e.f.AddSSHKey("ops")
	req := vendors.CreateRequest{
		Region: "fsn1", Zone: "fsn1", InstanceType: "cx23", CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image: vendors.Image{Ref: fmt.Sprint(e.x86.ID)}, UserData: []byte("{}"), IdempotencyKey: "uid-1",
		Settings: vendors.VendorSettings{Raw: json.RawMessage(`{"sshKey":"ops"}`)},
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: conformanceCluster, vendors.TagNodeClaim: "claim-1", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		Node: vendors.NodeInfo{Name: "claim-1"},
	}
	first, err := d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := e.f.GetServer(ctx, mustID(t, first.Ref.ID()))
	if first.BillingStart.IsZero() || !first.BillingStart.Equal(srv.Created) || len(srv.PublicNet.Firewalls) != 0 {
		t.Errorf("the billing start is the server's creation: %+v", first)
	}
	replay := req
	replay.Zone, replay.Region = "nbg1", "nbg1"
	got, err := d.CreateInstance(ctx, replay)
	if err != nil || got.Ref != first.Ref {
		t.Errorf("a replay in another location returns the first server: %v %v", got, err)
	}
	// An SSH key that does not exist is refused; one that does is resolved once.
	bad := req
	bad.Settings.Raw = json.RawMessage(`{"sshKey":"nobody"}`)
	bad.Node.Name, bad.IdempotencyKey = "claim-2", "uid-2"
	if _, err := d.CreateInstance(ctx, bad); vendors.ClassOf(err) != vendors.Unsupported {
		t.Errorf("a missing SSH key: %v", err)
	}
	_ = key
	// Malformed settings and references.
	for name, mutate := range map[string]func(*vendors.CreateRequest){
		"settings": func(r *vendors.CreateRequest) { r.Settings.Raw = json.RawMessage(`[`) },
		"image":    func(r *vendors.CreateRequest) { r.Image.Ref = "ubuntu-24.04" },
		"resources": func(r *vendors.CreateRequest) {
			r.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{{Kind: "firewalls", ID: "x"}}}
		},
	} {
		r := req
		r.Node.Name, r.IdempotencyKey = "bad-"+name, "uid-bad-"+name
		mutate(&r)
		if _, err := d.CreateInstance(ctx, r); vendors.ClassOf(err) != vendors.Fatal {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The node name falls back to the NodeClaim tag.
	named := req
	named.Node.Name, named.IdempotencyKey = "", "uid-3"
	named.Tags = map[vendors.TagKey]string{vendors.TagCluster: conformanceCluster, vendors.TagNodeClaim: "claim-3",
		vendors.TagManagedBy: vendors.ManagedByValue}
	named.Settings.Raw = nil
	if got, err := d.CreateInstance(ctx, named); err != nil {
		t.Error(err)
	} else if srv, _ := e.f.GetServer(ctx, mustID(t, got.Ref.ID())); srv.Name != "claim-3" {
		t.Errorf("name %q", srv.Name)
	}
}

func TestRateLimitWarning(t *testing.T) {
	e := newCEnv()
	d := e.driver(t)
	for range 3000 { // the bucket is below 20 %
		_, _ = e.f.GetServer(context.Background(), 1)
	}
	if !e.f.RateLimit().LowRemaining() {
		t.Fatal("the fake's bucket is not low")
	}
	for range 3 {
		if _, err := d.ListInstances(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNodeClassValidation(t *testing.T) {
	d := hcloud.New()
	nc := func(mutate func(*v1alpha1.SCNodeClass, *hcapi.Settings)) *v1alpha1.SCNodeClass {
		b := &hcapi.Settings{}
		c := &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}, Spec: v1alpha1.SCNodeClassSpec{
			Vendors: v1alpha1.VendorBlocks{HCloud: b}}}
		if mutate != nil {
			mutate(c, b)
		}
		return c
	}
	size := func(q string) hcapi.DataVolume {
		return hcapi.DataVolume{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, Size: mustQuantity(q)}
	}
	netUse := &common.Resource{Use: &common.ResourceUse{Name: "n"}}
	tests := map[string]struct {
		nc   *v1alpha1.SCNodeClass
		want string
	}{
		"valid":        {nc(nil), ""},
		"not hcloud":   {&v1alpha1.SCNodeClass{Spec: v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{AWS: nil}}}, ""},
		"bad location": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) { b.Regions = []string{"eu-central-1"} }), "not a Hetzner location"},
		"ssh key":      {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) { b.SSHKey = " key" }), "sshKey"},
		"no public address": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Networking = &common.NetworkingOverride{PublicIPv4: new(false)}
		}), "needs a public IPv4 or IPv6"},
		"ipv6 only": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Networking = &common.NetworkingOverride{PublicIPv4: new(false), PublicIPv6: new(true)}
		}), ""},
		"bad cidr": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Networking = &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"nope"}}}}}
		}), "not a CIDR"},
		"bad protocol": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Networking = &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "gre", CIDRs: []string{"10.0.0.0/8"}}}}}
		}), "not tcp, udp, icmp or any"},
		"volume too small": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Storage = &hcapi.Storage{Data: []hcapi.DataVolume{size("5G")}}
		}), "10 to 10240 GB"},
		"volumes clash": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Storage = &hcapi.Storage{Data: []hcapi.DataVolume{size("10G"), size("10G")}}
		}), "used twice"},
		"relative mount": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			v := size("10G")
			v.MountPath = "d"
			b.Storage = &hcapi.Storage{Data: []hcapi.DataVolume{v}}
		}), "not absolute"},
		"networks create": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Resources = &hcapi.Resources{Networks: &common.Resource{Create: &common.ResourceCreate{}}}
		}), "networks.create is not supported"},
		"network without isolation": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Resources = &hcapi.Resources{Networks: netUse}
		}), "networking.isolation"},
		"network with run isolation": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Networking = &common.NetworkingOverride{Isolation: common.IsolationRun}
			b.Resources = &hcapi.Resources{Networks: netUse}
		}), "run is not possible"},
		"network with nodeclass isolation": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Networking = &common.NetworkingOverride{Isolation: common.IsolationNodeClass}
			b.Resources = &hcapi.Resources{Networks: netUse}
		}), ""},
		"unsupported selector field": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Resources = &hcapi.Resources{Firewalls: &common.Resource{Use: &common.ResourceUse{
				Match: &common.ResourceMatch{Architecture: []string{"amd64"}}}}}
		}), "match.architecture is not supported"},
		"ids": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Resources = &hcapi.Resources{Images: &common.Resource{Use: &common.ResourceUse{
				IDs: map[string]common.IDList{"eu-central-1": {"ami-1"}, "*": {"abc", "123", "{region}"}}}}}
		}), "is not a location"},
		"labels": {nc(func(_ *v1alpha1.SCNodeClass, b *hcapi.Settings) {
			b.Resources = &hcapi.Resources{Firewalls: &common.Resource{Use: &common.ResourceUse{
				Tags: map[string]string{"bad key!": "x", "ok": "has space", "any": "*", "templated": "{region}"}}}}
		}), "not a valid label key"},
		"tag reserved": {nc(func(c *v1alpha1.SCNodeClass, _ *hcapi.Settings) {
			c.Spec.Tags = map[string]string{"scarpenter.sparecores.com/cluster": "x"}
		}), "reserved for SCarpenter"},
		"tag invalid": {nc(func(c *v1alpha1.SCNodeClass, _ *hcapi.Settings) {
			c.Spec.Tags = map[string]string{"bad key": "x"}
		}), "tags:"},
		"storage profile": {nc(func(c *v1alpha1.SCNodeClass, _ *hcapi.Settings) {
			c.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{"p": {HCloud: &hcapi.Storage{Data: []hcapi.DataVolume{size("1G")}}}}
		}), "profiles.storage.p.hcloud"},
		"placement profile": {nc(func(c *v1alpha1.SCNodeClass, _ *hcapi.Settings) {
			c.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{"p": {HCloud: &hcapi.Placement{PlacementGroup: &hcapi.PlacementGroup{}}}}
		}), "exactly one of type and use"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := d.ValidateNodeClass(context.Background(), tc.nc)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("%v, want %q", err, tc.want)
			}
		})
	}
}

func TestVendorArch(t *testing.T) {
	d := hcloud.New()
	for arch, want := range map[string]string{"amd64": "x86", "arm64": "arm"} {
		if got, ok := d.VendorArch(arch); !ok || got != want {
			t.Errorf("%s: %q %v", arch, got, ok)
		}
	}
	if _, ok := d.VendorArch("riscv"); ok {
		t.Error("riscv")
	}
}

var _ = hc.ArchitectureX86
