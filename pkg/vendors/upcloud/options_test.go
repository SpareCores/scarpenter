/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	ucapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/upcloud"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	uc "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
)

func TestRootDiskIsThePlansUnlessTheNodeClassSetsIt(t *testing.T) {
	e := newEnv(t, envOptions{})
	def := e.d.Defaults()
	if def.RootDiskBytes != 25<<30 || e.d.RootDiskBytes(def) != 25<<30 {
		t.Errorf("defaults %d", def.RootDiskBytes)
	}
	for name, tc := range map[string]struct {
		localGB float64
		want    int64
	}{
		"a plan with 50 GB":  {50, 50},
		"a small plan":       {10, 25},
		"a plan without one": {0, 25},
	} {
		got, ok := e.d.RootDiskForType(def, vendors.CatalogType{LocalStorageGB: tc.localGB})
		if !ok || got != tc.want<<30 {
			t.Errorf("%s: %d, %v; want %d GiB", name, got, ok, tc.want)
		}
	}
	s := settings(t, e.d, `{"storage":{"root":{"size":"64Gi"}}}`, defaultNet(), nil, options.Selection{})
	if got, ok := e.d.RootDiskForType(s, vendors.CatalogType{LocalStorageGB: 10}); !ok || got != 64<<30 || s.RootDiskBytes != 64<<30 {
		t.Errorf("the NodeClass's size: %d %d, %v", got, s.RootDiskBytes, ok)
	}
	bad := vendors.VendorSettings{Raw: json.RawMessage(`{`)}
	if e.d.RootDiskBytes(bad) != 25<<30 {
		t.Error("undecodable settings do not give the least size")
	}
}

func offering(plan string, gb float64) vendors.Offering {
	return vendors.Offering{
		Region: testZone, Zone: testZone, CapacityType: vendors.CapacityOnDemand, APIReference: plan,
		Type: vendors.CatalogType{APIReference: plan, LocalStorageGB: gb},
	}
}

func TestPriceDeltaChargesWhatThePlanDoesNotInclude(t *testing.T) {
	prices := uc.StaticPrices{
		Storage: map[string]float64{testZone + "/maxiops": 67.2, testZone + "/standard": 33.6, testZone + "/hdd": 6.72},
		IPv4:    map[string]float64{testZone: 0.004},
	}
	e := newEnv(t, envOptions{prices: prices})
	if _, err := e.d.LaunchableTypes(e.ctx(), testZone); err != nil {
		t.Fatal(err)
	}
	delta := func(block, plan string, gb float64, net common.Networking) (float64, bool) {
		return e.d.PriceDelta(settings(t, e.d, block, net, nil, options.Selection{}), offering(plan, gb))
	}
	// The included disk is free.
	if p, ok := delta("", testType, 50, defaultNet()); p != 0 || !ok {
		t.Errorf("the included disk: %v, %v", p, ok)
	}
	// A bigger disk is billed in full, per GB-hour (the month is 672 hours).
	if p, ok := delta(`{"storage":{"root":{"size":"100Gi"}}}`, testType, 50, defaultNet()); !ok || math.Abs(p-100*67.2/672) > 1e-9 {
		t.Errorf("a 100 GB root: %v, %v", p, ok)
	}
	// Another tier than the plan's is billed in full.
	if p, ok := delta(`{"storage":{"root":{"tier":"standard"}}}`, testType, 50, defaultNet()); !ok || math.Abs(p-50*33.6/672) > 1e-9 {
		t.Errorf("a standard root on a maxiops plan: %v, %v", p, ok)
	}
	// A Cloud Native plan has no disk and no IPv4 address included.
	want := 25*67.2/672 + 0.004
	if p, ok := delta("", "CLOUDNATIVE-2xCPU-4GB", 0, defaultNet()); !ok || math.Abs(p-want) > 1e-9 {
		t.Errorf("a Cloud Native plan: %v, %v; want %v", p, ok, want)
	}
	net := defaultNet()
	net.PublicIPv4 = new(false)
	if p, _ := delta("", "CLOUDNATIVE-2xCPU-4GB", 0, net); math.Abs(p-25*67.2/672) > 1e-9 {
		t.Errorf("without IPv4: %v", p)
	}
	// Data disks are priced by tier.
	block := `{"storage":{"data":[{"name":"a","tier":"hdd","size":"100Gi","mountPath":"/a"}]}}`
	if p, ok := delta(block, testType, 50, defaultNet()); !ok || math.Abs(p-100*6.72/672) > 1e-9 {
		t.Errorf("a data disk: %v, %v", p, ok)
	}
	// Unknown prices make the price incomplete.
	bare := newEnv(t, envOptions{})
	if _, err := bare.d.LaunchableTypes(bare.ctx(), testZone); err != nil {
		t.Fatal(err)
	}
	if _, ok := bare.d.PriceDelta(settings(t, bare.d, block, defaultNet(), nil, options.Selection{}), offering(testType, 50)); ok {
		t.Error("a price without a price source is complete")
	}
	if _, ok := bare.d.PriceDelta(settings(t, bare.d, "", defaultNet(), nil, options.Selection{}), offering("CLOUDNATIVE-2xCPU-4GB", 0)); ok {
		t.Error("a Cloud Native price without a price source is complete")
	}
	if _, ok := e.d.PriceDelta(vendors.VendorSettings{Raw: json.RawMessage(`{`)}, offering(testType, 50)); ok {
		t.Error("undecodable settings are priced")
	}
	// Before the plan table is read an explicit tier cannot be compared with the plan's.
	cold := newEnv(t, envOptions{prices: prices})
	if _, ok := cold.d.PriceDelta(settings(t, cold.d, `{"storage":{"root":{"tier":"standard"}}}`, defaultNet(), nil, options.Selection{}),
		offering(testType, 50)); ok {
		t.Error("an explicit tier against an unknown plan tier is complete")
	}
}

func TestKeeperPricesLoadInTheBackgroundAndBumpTheEpoch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/storage_prices", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vendor") != "upcloud" || r.URL.Query().Get("currency") != "USD" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[
		 {"vendor_id":"upcloud","region_id":"fi-hel1","storage_id":"MaxIOPS","unit":"GB/month","price":0.22,"currency":"USD","status":"active"},
		 {"vendor_id":"upcloud","region_id":"fi-hel1","storage_id":"hdd","unit":"GB/month","price":0.05,"currency":"EUR","status":"active"},
		 {"vendor_id":"upcloud","region_id":"fi-hel1","storage_id":"standard","unit":"GB/month","price":0,"currency":"USD","status":"active"},
		 {"vendor_id":"aws","region_id":"fi-hel1","storage_id":"x","unit":"GB/month","price":1,"currency":"USD","status":"active"}]`))
	})
	mux.HandleFunc("/ipv4_prices", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"vendor_id":"upcloud","region_id":"fi-hel1","unit":"month","price":6.72,"currency":"USD","status":"active"},
		 {"vendor_id":"upcloud","region_id":"de-fra1","unit":"hour","price":0.01,"currency":"USD","status":"active"},
		 {"vendor_id":"upcloud","region_id":"nl-ams1","unit":"year","price":1,"currency":"USD","status":"active"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e := newEnv(t, envOptions{})
	d := uc.New(uc.WithAPI(e.f))
	if err := d.Init(e.ctx(), vendors.Config{ClusterName: testCluster, KeeperURL: srv.URL, Clock: e.clock}); err != nil {
		t.Fatal(err)
	}
	before := d.NodeOptionsEpoch()
	if err := d.LoadPrices(e.ctx()); err != nil {
		t.Fatal(err)
	}
	if d.NodeOptionsEpoch() == before {
		t.Error("loading the prices did not change the epoch")
	}
	p, ok := d.PriceDelta(vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"size":"100Gi"}}}`)},
		offering("CLOUDNATIVE-2xCPU-4GB", 0))
	if !ok || math.Abs(p-(100*0.22/672+6.72/672)) > 1e-9 {
		t.Errorf("PriceDelta = %v, %v", p, ok)
	}
	// Regions drives the background load, which the same driver repeats later.
	if _, err := d.Regions(e.ctx()); err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewServer(http.NotFoundHandler())
	defer bad.Close()
	d2 := uc.New(uc.WithAPI(e.f))
	if err := d2.Init(e.ctx(), vendors.Config{ClusterName: testCluster, KeeperURL: bad.URL, Clock: e.clock}); err != nil {
		t.Fatal(err)
	}
	if err := d2.LoadPrices(e.ctx()); err == nil {
		t.Error("a Keeper that answers 404 loaded")
	}
	if err := e.d.LoadPrices(e.ctx()); err != nil {
		t.Errorf("without a Keeper URL LoadPrices is a no-op: %v", err)
	}
}

func TestValidateNodeClass(t *testing.T) {
	e := newEnv(t, envOptions{})
	ok := class(testVersion, []string{"fi-hel1", "de-fra1"}, nil)
	ok.Spec.Vendors.UpCloud.Storage = &ucapi.Storage{Root: &ucapi.RootVolume{Tier: "maxiops"}}
	if err := e.d.ValidateNodeClass(e.ctx(), ok); err != nil {
		t.Errorf("a valid NodeClass: %v", err)
	}
	size := func(s string) *ucapi.Storage {
		q := parseQuantity(s)
		return &ucapi.Storage{Root: &ucapi.RootVolume{Size: &q}}
	}
	mk := func(f func(nc *v1alpha1.SCNodeClass)) *v1alpha1.SCNodeClass {
		nc := class(testVersion, nil, nil)
		f(nc)
		return nc
	}
	for name, tc := range map[string]struct {
		nc   *v1alpha1.SCNodeClass
		want string
	}{
		"lts channel": {mk(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Flatcar.Channel = "lts" }), "lts is not supported on UpCloud"},
		"bad zone":    {class(testVersion, []string{"eu-central-1"}, nil), "not an UpCloud zone"},
		"root too small": {mk(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.UpCloud.Storage = size("5Gi") }),
			"10 to 1024 GB"},
		"root too big": {mk(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.UpCloud.Storage = size("2000Gi") }), "10 to 1024 GB"},
		"hdd root": {mk(func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.UpCloud.Storage = &ucapi.Storage{Root: &ucapi.RootVolume{Tier: "hdd"}}
		}), "not a tier a root disk can have"},
		"too many disks": {mk(func(nc *v1alpha1.SCNodeClass) {
			st := &ucapi.Storage{}
			for _, n := range []string{"a", "b", "c", "d"} {
				st.Data = append(st.Data, ucapi.DataVolume{GuestVolume: common.GuestVolume{Name: n, MountPath: "/" + n}, Tier: "hdd",
					Size: parseQuantity("10Gi")})
			}
			nc.Spec.Vendors.UpCloud.Storage = st
		}), "storage devices"},
		"duplicate disk": {mk(func(nc *v1alpha1.SCNodeClass) {
			v := ucapi.DataVolume{GuestVolume: common.GuestVolume{Name: "a", MountPath: "/a"}, Tier: "hdd", Size: parseQuantity("10Gi")}
			nc.Spec.Vendors.UpCloud.Storage = &ucapi.Storage{Data: []ucapi.DataVolume{v, v}}
		}), "used twice"},
		"bad CIDR": {mk(func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.UpCloud.Networking = &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{
				{Protocol: "tcp", CIDRs: []string{"nope"}}}}}
		}), "is not a CIDR"},
		"no egress": {mk(func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.UpCloud.Networking = &common.NetworkingOverride{PublicIPv4: new(false)}
		}), "publicIPv4: false is refused"},
		"bad zone key": {mk(func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.UpCloud.Resources = &ucapi.Resources{Networks: &common.Resource{Use: &common.ResourceUse{
				IDs: map[string]common.IDList{"eu-central-1": {"x"}}}}}
		}), "is not a zone"},
		"bad id": {mk(func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.UpCloud.Resources = &ucapi.Resources{Networks: &common.Resource{Use: &common.ResourceUse{
				IDs: map[string]common.IDList{"fi-hel1": {"not-a-uuid"}}}}}
		}), "is not an UpCloud UUID"},
		"bad tag": {mk(func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.UpCloud.Resources = &ucapi.Resources{Firewalls: &common.Resource{Use: &common.ResourceUse{
				Tags: map[string]string{"x": "1"}}}}
		}), "must be 2 to 32 characters"},
		"unsupported match": {mk(func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.UpCloud.Resources = &ucapi.Resources{ServerGroups: &common.Resource{Use: &common.ResourceUse{
				Match: &common.ResourceMatch{CIDR: "10.0.0.0/24"}}}}
		}), "match.cidr is not supported"},
		"bad profile": {mk(func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{"p": {UpCloud: size("5Gi")}}
			nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{"q": {UpCloud: &ucapi.Placement{}}}
		}), "serverGroup is required"},
		"reserved tag": {mk(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = map[string]string{"scarpenter-x": "1"} }), "reserved prefix"},
	} {
		err := e.d.ValidateNodeClass(e.ctx(), tc.nc)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", name, err, tc.want)
		}
	}
	// A NodeClass without the upcloud block is not this driver's business.
	if err := e.d.ValidateNodeClass(e.ctx(), &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "x"}}); err != nil {
		t.Errorf("a NodeClass without an upcloud block: %v", err)
	}
	// publicIPv4 false is fine with a brought network.
	byo := mk(func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.UpCloud.Networking = &common.NetworkingOverride{PublicIPv4: new(false)}
		nc.Spec.Vendors.UpCloud.Resources = &ucapi.Resources{Networks: &common.Resource{Use: &common.ResourceUse{
			Tags: map[string]string{uc.LabelUse: "true"}}}}
	})
	if err := e.d.ValidateNodeClass(e.ctx(), byo); err != nil {
		t.Errorf("publicIPv4 false with a network: %v", err)
	}
}

func parseQuantity(s string) resource.Quantity { return resource.MustParse(s) }

// Under isolation run (the API default) the server-group profiles that need a run
// key are not offered; a group by name is; nodeclass and none offer both.
func TestSupportedForIsolation(t *testing.T) {
	e := newEnv(t, envOptions{})
	p := vendors.Profiles{vendors.ProfilePlacement: {
		"spread": []byte(`{"serverGroup":{"antiAffinity":"strict"}}`),
		"byname": []byte(`{"serverGroup":{"use":{"name":"existing"}}}`),
	}}
	it := vendors.CatalogType{APIReference: testType}
	for iso, want := range map[common.Isolation]string{
		common.IsolationRun:       "[byname]",
		common.IsolationNodeClass: "[byname spread]",
		common.IsolationNone:      "[byname spread]",
	} {
		got := e.d.SupportedFor(it, testZone, common.Networking{Isolation: iso}, p)
		if fmt.Sprint(got[vendors.ProfilePlacement]) != want {
			t.Errorf("isolation %s: placement = %v, want %s", iso, got[vendors.ProfilePlacement], want)
		}
	}
}

func TestValidateNodeClassRunNeedsNoRunKeyProfile(t *testing.T) {
	e := newEnv(t, envOptions{})
	mk := func(iso common.Isolation, group *ucapi.ServerGroup) *v1alpha1.SCNodeClass {
		nc := class(testVersion, nil, nil)
		nc.Spec.Networking.Isolation = iso
		nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{"spread": {UpCloud: &ucapi.Placement{ServerGroup: group}}}
		return nc
	}
	create := &ucapi.ServerGroup{AntiAffinity: ucapi.AntiAffinityStrict}
	for _, iso := range []common.Isolation{"", common.IsolationRun} {
		err := e.d.ValidateNodeClass(e.ctx(), mk(iso, create))
		if err == nil || !strings.Contains(err.Error(), "set spec.networking.isolation to nodeclass or none") {
			t.Errorf("isolation %q: %v, want the message to set nodeclass or none", iso, err)
		}
	}
	for _, iso := range []common.Isolation{common.IsolationNodeClass, common.IsolationNone} {
		if err := e.d.ValidateNodeClass(e.ctx(), mk(iso, create)); err != nil {
			t.Errorf("isolation %s: %v", iso, err)
		}
	}
	use := &ucapi.ServerGroup{Use: &ucapi.ServerGroupRef{Name: "existing"}}
	if err := e.d.ValidateNodeClass(e.ctx(), mk("", use)); err != nil {
		t.Errorf("a group by name under run: %v", err)
	}
	// The vendor block's override wins over spec.networking.
	nc := mk(common.IsolationRun, create)
	nc.Spec.Vendors.UpCloud.Networking = &common.NetworkingOverride{Isolation: common.IsolationNone}
	if err := e.d.ValidateNodeClass(e.ctx(), nc); err != nil {
		t.Errorf("override isolation none: %v", err)
	}
}
