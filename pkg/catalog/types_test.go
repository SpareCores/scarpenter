/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// TestGoldenAWSTypes pins the whole mapping for a fixed set of AWS types in
// one region: capacity, overhead, every requirement of the type and of its
// offerings.
func TestGoldenAWSTypes(t *testing.T) {
	facts := catalogtestFacts(t)
	facts.RootDisk = map[string]int64{"aws": 128 << 30}
	cl := catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})
	c := catalog.New(catalog.Options{Source: sourceFor(t), Facts: facts})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	its := all(t, c, catalog.Filter{})
	var b strings.Builder
	for _, name := range []string{"aws.t3a.small", "aws.m7i.large", "aws.c7g.xlarge", "aws.p4d.24xlarge"} {
		it := byName(its, name)
		if it == nil {
			t.Fatalf("%s is not in the catalog", name)
		}
		b.WriteString("=== " + name + "\n")
		b.WriteString(dump(it))
	}
	testutil.Golden(t, "aws-us-east-1.golden", []byte(b.String()))
}

// TestEveryKeyIsDefinedOnEveryType is the rule of 00a section 2: an undefined
// key would match every type, so every selectable key of a type is defined
// (DoesNotExist where unknown), on the whole AWS catalog.
func TestEveryKeyIsDefinedOnEveryType(t *testing.T) {
	c, cl := refreshed(t, catalog.Scope{"aws": nil}, "aws")
	its := all(t, c, catalog.Filter{})
	if len(its) < 1000 {
		t.Fatalf("only %d AWS types", len(its))
	}
	want := labels.AttributeKeys(cl.CPUFlags)
	want = append(want, corev1.LabelInstanceTypeStable, corev1.LabelArchStable, corev1.LabelOSStable,
		corev1.LabelTopologyRegion, corev1.LabelTopologyZone, v1alpha1.LabelZoneID, karpv1.CapacityTypeLabelKey,
		v1alpha1.LabelSelection, v1alpha1.LabelStorageProfile, v1alpha1.LabelNetworkProfile, v1alpha1.LabelPlacementProfile)
	offeringKeys := append([]string{
		karpv1.CapacityTypeLabelKey, corev1.LabelTopologyZone, corev1.LabelTopologyRegion, v1alpha1.LabelZoneID,
	}, labels.LocationKeys...)
	for _, it := range its {
		for _, k := range want {
			if !it.Requirements.Has(k) {
				t.Fatalf("%s lacks the key %s", it.Name, k)
			}
		}
		if it.Requirements.Has(v1alpha1.LabelPlacementGroup) {
			t.Fatalf("%s defines the placement group, which is a free-form run key", it.Name)
		}
		for _, o := range it.Offerings {
			for _, k := range offeringKeys {
				if !o.Requirements.Has(k) {
					t.Fatalf("an offering of %s lacks the key %s", it.Name, k)
				}
			}
		}
	}
}

func TestCapacityAndOverhead(t *testing.T) {
	facts := catalogtestFacts(t)
	facts.RootDisk = map[string]int64{"aws": 128 << 30}
	cl := catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})
	c := catalog.New(catalog.Options{Source: sourceFor(t), Facts: facts})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	m7i := byName(all(t, c, catalog.Filter{}), "aws.m7i.large")
	// 8192 MiB less 7.5 %.
	if got := m7i.Capacity.Memory().String(); got != "7577Mi" {
		t.Errorf("memory = %s, want 7577Mi", got)
	}
	if got := m7i.Capacity.Cpu().Value(); got != 2 {
		t.Errorf("cpu = %d", got)
	}
	if got := m7i.Capacity.Pods().Value(); got != 110 {
		t.Errorf("pods = %d", got)
	}
	if got := m7i.Capacity.StorageEphemeral().String(); got != "128Gi" {
		t.Errorf("ephemeral-storage = %s", got)
	}
	// 2 vCPUs: 6 % of the first core and 1 % of the second; 11 MiB per pod
	// plus 255 MiB.
	if got := m7i.Overhead.KubeReserved.Cpu().MilliValue(); got != 70 {
		t.Errorf("kube-reserved cpu = %dm, want 70m", got)
	}
	if got := m7i.Overhead.KubeReserved.Memory().String(); got != "1465Mi" {
		t.Errorf("kube-reserved memory = %s, want 1465Mi", got)
	}
	if got := m7i.Overhead.EvictionThreshold.Memory().String(); got != "100Mi" {
		t.Errorf("eviction threshold = %s, want 100Mi", got)
	}
	// Without a driver fact the ephemeral storage is omitted.
	c2 := catalog.New(catalog.Options{Source: sourceFor(t), Facts: catalogtestFacts(t)})
	c2.SetClasses([]catalog.Class{cl})
	if err := c2.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, has := byName(all(t, c2, catalog.Filter{}), "aws.m7i.large").Capacity[corev1.ResourceEphemeralStorage]; has {
		t.Error("ephemeral-storage is set without a root disk fact")
	}
}

func TestKubeReservedFormula(t *testing.T) {
	for _, tc := range []struct {
		cpus      int
		pods      int64
		cpuMilli  int64
		memoryMiB int64
	}{
		{1, 110, 60, 1465},
		{2, 110, 70, 1465},
		{4, 110, 80, 1465},
		{8, 110, 90, 1465},
		{96, 110, 310, 1465},
		{2, 29, 70, 574},
	} {
		got := catalog.KubeReserved(tc.cpus, tc.pods)
		if got.Cpu().MilliValue() != tc.cpuMilli || got.Memory().Value() != tc.memoryMiB<<20 {
			t.Errorf("KubeReserved(%d, %d) = cpu %dm memory %s, want %dm %dMi", tc.cpus, tc.pods, got.Cpu().MilliValue(), got.Memory(), tc.cpuMilli, tc.memoryMiB)
		}
	}
}

func TestSyntheticMapping(t *testing.T) {
	c := newSynthetic(t, nil, nil)
	its := all(t, c, catalog.Filter{})
	if len(its) != 2 {
		t.Fatalf("got %d types, want 2", len(its))
	}
	small, big := byName(its, "v.small"), byName(its, "v.big")
	if small == nil || big == nil {
		t.Fatalf("types: %v", its)
	}
	if got := len(small.Offerings); got != 3 {
		t.Errorf("small has %d offerings, want 3", got)
	}
	// Hyperthreading: two vCPUs on one core.
	if !small.Requirements.Get(v1alpha1.LabelHyperthreading).Has("true") {
		t.Error("hyperthreading is not true for 2 vCPUs on 1 core")
	}
	if !big.Requirements.Get(v1alpha1.LabelHyperthreading).Has("true") && big.Requirements.Get(v1alpha1.LabelHyperthreading).Operator() != corev1.NodeSelectorOpDoesNotExist {
		t.Error("hyperthreading of a type with unknown cores must be DoesNotExist")
	}
	if !big.Requirements.Get(corev1.LabelArchStable).Has("arm64") {
		t.Error("arch of big is not arm64")
	}
	// Location comes from the region.
	for _, o := range small.Offerings {
		if !o.Requirements.Get(v1alpha1.LabelCountry).Has("de") || !o.Requirements.Get(v1alpha1.LabelContinent).Has("europe") ||
			!o.Requirements.Get(v1alpha1.LabelCity).Has("frankfurt") {
			t.Errorf("location of %v: %v", o.Requirements, reqLines(o.Requirements))
		}
		if o.Requirements.Get(v1alpha1.LabelGreenEnergy).Operator() != corev1.NodeSelectorOpDoesNotExist {
			t.Error("unknown green energy must be DoesNotExist")
		}
	}
}

func TestZonesFromDriverFacts(t *testing.T) {
	// The account calls the zones differently, and lacks z2 (ADR-009).
	facts := catalog.StaticFacts{ZonesByVendor: map[string]map[string][]catalog.Zone{
		"v": {"region-1": {{ID: "z1", Name: "my-zone-a"}}},
	}}
	c := newSynthetic(t, nil, facts)
	small := byName(all(t, c, catalog.Filter{}), "v.small")
	if got := len(small.Offerings); got != 2 {
		t.Fatalf("offerings = %d, want 2 (z2 dropped)", got)
	}
	for _, o := range small.Offerings {
		if !o.Requirements.Get(corev1.LabelTopologyZone).Has("my-zone-a") || !o.Requirements.Get(v1alpha1.LabelZoneID).Has("z1") {
			t.Errorf("offering zone: %v", reqLines(o.Requirements))
		}
	}
	if got := small.Requirements.Get(corev1.LabelTopologyZone).Values(); len(got) != 1 || got[0] != "my-zone-a" {
		t.Errorf("type zones = %v", got)
	}
}

func TestRegionLevelPricesAreReplicatedIntoEveryZone(t *testing.T) {
	c := newSynthetic(t, func(d *model.Data) {
		d.Prices = []model.Price{{Vendor: "v", RegionID: "r1", ServerID: "1", CapacityType: model.CapacityOnDemand, USDPerHour: 0.2}}
	}, nil)
	small := byName(all(t, c, catalog.Filter{}), "v.small")
	if small == nil || len(small.Offerings) != 2 {
		t.Fatalf("want one offering per zone, got %v", small)
	}
	for _, o := range small.Offerings {
		if o.Price != 0.2 {
			t.Errorf("price = %v", o.Price)
		}
	}
}

func TestVendorWithoutZonesUsesTheRegion(t *testing.T) {
	c := newSynthetic(t, func(d *model.Data) {
		d.Zones = nil
		d.Prices = []model.Price{
			{Vendor: "v", RegionID: "r1", ServerID: "1", CapacityType: model.CapacityOnDemand, USDPerHour: 0.2},
			{Vendor: "v", RegionID: "r1", ZoneID: "r1", ServerID: "2", CapacityType: model.CapacityOnDemand, USDPerHour: 0.9},
		}
	}, catalog.StaticFacts{})
	its := all(t, c, catalog.Filter{})
	for _, name := range []string{"v.small", "v.big"} {
		it := byName(its, name)
		if it == nil || len(it.Offerings) != 1 {
			t.Fatalf("%s: %v", name, it)
		}
		if !it.Offerings[0].Requirements.Get(corev1.LabelTopologyZone).Has("region-1") {
			t.Errorf("%s zone = %v", name, reqLines(it.Offerings[0].Requirements))
		}
	}
}

func TestDataFiltering(t *testing.T) {
	cases := map[string]struct {
		mutate func(*model.Data)
		facts  catalog.Facts
		want   []string
	}{
		"a server without a price is dropped": {
			mutate: func(d *model.Data) { d.Prices = d.Prices[:3] }, want: []string{"v.small"},
		},
		"prices of an inactive region are dropped": {
			mutate: func(d *model.Data) { d.Regions[0].Active = false }, want: nil,
		},
		"two servers with one API reference keep the lower ID": {
			mutate: func(d *model.Data) {
				d.Servers = append(d.Servers, model.Server{Vendor: "v", ID: "9", APIReference: "small", VCPUs: 64, Arch: "amd64", MemoryMiB: 1})
				d.Prices = append(d.Prices, model.Price{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: "9", CapacityType: model.CapacityOnDemand, USDPerHour: 1})
			},
			want: []string{"v.big", "v.small"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newSynthetic(t, tc.mutate, tc.facts)
			var got []string
			for _, it := range all(t, c, catalog.Filter{}) {
				got = append(got, it.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("types = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInactiveZoneDropsOnlyThatZone(t *testing.T) {
	c := newSynthetic(t, func(d *model.Data) { d.Zones[0].Active = false }, nil)
	small := byName(all(t, c, catalog.Filter{}), "v.small")
	if small == nil || len(small.Offerings) != 1 {
		t.Fatalf("small = %v", small)
	}
	if !small.Offerings[0].Requirements.Get(v1alpha1.LabelZoneID).Has("z2") {
		t.Errorf("remaining offering: %v", reqLines(small.Offerings[0].Requirements))
	}
}

// incompleteFacts reports an incomplete price delta for one zone.
type incompleteFacts struct{ catalog.StaticFacts }

func (incompleteFacts) PriceDelta(_ string, _ *catalog.Class, _, zone, _ string) (float64, bool) {
	if zone == "zone-2" {
		return 0.01, false
	}
	return 0.005, true
}

func TestPriceDeltaAndIncompletePrices(t *testing.T) {
	facts := incompleteFacts{syntheticFacts()}
	c := newSynthetic(t, nil, facts)
	small := byName(all(t, c, catalog.Filter{}), "v.small")
	prices := map[string]float64{}
	for _, o := range small.Offerings {
		prices[o.Zone()+"/"+o.CapacityType()] = o.Price
	}
	if got := prices["zone-1/on-demand"]; got < 0.1049 || got > 0.1051 {
		t.Errorf("complete delta: price %v, want 0.105", got)
	}
	if got := prices["zone-2/on-demand"]; got < 0.1199 || got > 0.1201 {
		t.Errorf("incomplete delta is still offered by default: price %v, want 0.12", got)
	}

	// pricing.allowIncomplete: false excludes offerings with an incomplete price.
	src := &fakeSource{data: synthetic()}
	c2 := catalog.New(catalog.Options{Source: src, Facts: facts})
	cl := catalog.NewClass("c", catalog.Scope{"v": nil})
	cl.AllowIncompletePrice = false
	cl.Finalize()
	c2.SetClasses([]catalog.Class{cl})
	if err := c2.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	small = byName(all(t, c2, catalog.Filter{}), "v.small")
	for _, o := range small.Offerings {
		if o.Zone() == "zone-2" {
			t.Errorf("incomplete offering was not excluded: %v", reqLines(o.Requirements))
		}
	}
}

func TestLookupResolvesNamesThatDoNotContainTheReference(t *testing.T) {
	long := strings.Repeat("x", 80)
	c := newSynthetic(t, func(d *model.Data) {
		d.Servers[0].APIReference = long
	}, nil)
	its := all(t, c, catalog.Filter{})
	var name string
	for _, it := range its {
		if len(it.Name) == 63 {
			name = it.Name
		}
	}
	if name == "" {
		t.Fatalf("no truncated name in %d types", len(its))
	}
	ref, ok := c.Lookup(name)
	if !ok || ref.Vendor != "v" || ref.APIReference != long {
		t.Errorf("Lookup(%q) = %v, %v", name, ref, ok)
	}
	// The raw reference does not fit a label: the key is not defined.
	if byName(its, name).Requirements.Get(v1alpha1.LabelAPIReference).Operator() != corev1.NodeSelectorOpDoesNotExist {
		t.Error("api-reference must be DoesNotExist when it does not fit a label")
	}
	if _, ok := c.Lookup("v.nope"); ok {
		t.Error("Lookup found an unknown type")
	}
}
