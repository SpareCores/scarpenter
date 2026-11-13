/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package labels_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

func values(r *scheduling.Requirement) string {
	if r.Operator() == corev1.NodeSelectorOpDoesNotExist {
		return "-"
	}
	v := r.Values()
	slices.Sort(v)
	return strings.Join(v, ",")
}

func attrs(s *model.Server, flags ...string) map[string]string {
	out := map[string]string{}
	for _, r := range labels.Attributes(s, flags) {
		out[r.Key] = values(r)
	}
	return out
}

func TestAttributesOfAFullyKnownServer(t *testing.T) {
	s := &model.Server{
		Vendor: "aws", APIReference: "m7i.large", Family: "m7i", VCPUs: 4, CPUCores: new(2), CPUAllocation: "Dedicated",
		Arch: "amd64", CPUManufacturer: "Intel", CPUFamily: "Xeon", CPUSpeedGHz: new(3.2), CPUFlags: []string{"avx2", "sha_ni"},
		CPUL2PerCoreKiB: new(2048.0), CPUL3KiB: new(107520.0), MemoryMiB: 16384,
		AcceleratorCount: 2, AcceleratorType: "GPU", AcceleratorManufacturer: "NVIDIA", AcceleratorModel: "L4", AcceleratorMemoryMiB: new(49152.0),
		NetworkBaselineGbps: new(0.781), NetworkPeakGbps: new(12.5), LocalStorageGB: 474, LocalStorageCount: 1,
	}
	got := attrs(s, "avx2", "avx512f")
	want := map[string]string{
		v1alpha1.LabelVendor: "aws", v1alpha1.LabelAPIReference: "m7i.large", v1alpha1.LabelFamily: "m7i",
		v1alpha1.LabelVCPUs: "4", v1alpha1.LabelCPUCores: "2", v1alpha1.LabelMemoryMiB: "16384", v1alpha1.LabelMemoryPerVCPUMiB: "4096",
		v1alpha1.LabelCPUManufacturer: "Intel", v1alpha1.LabelCPUFamily: "Xeon", v1alpha1.LabelCPUAllocation: "Dedicated",
		v1alpha1.LabelCPUClockMHz: "3200", v1alpha1.LabelCPUL2PerCoreKiB: "2048", v1alpha1.LabelCPUL3MiB: "105",
		v1alpha1.LabelHyperthreading:   "true",
		v1alpha1.LabelAcceleratorCount: "2", v1alpha1.LabelAcceleratorMemoryMiB: "49152", v1alpha1.LabelAcceleratorManufacturer: "NVIDIA",
		v1alpha1.LabelAcceleratorModel: "L4", v1alpha1.LabelAcceleratorType: "GPU",
		v1alpha1.LabelNetworkBaselineMbps: "781", v1alpha1.LabelNetworkPeakMbps: "12500",
		v1alpha1.LabelLocalStorageGB: "474", v1alpha1.LabelLocalStorageCount: "1",
		v1alpha1.CPUFlagLabel("avx2"): "true", v1alpha1.CPUFlagLabel("avx512f"): "false",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d keys, want %d: %v", len(got), len(want), got)
	}
	keys := labels.AttributeKeys([]string{"avx2", "avx512f"})
	if len(keys) != len(got) {
		t.Errorf("AttributeKeys has %d keys, Attributes %d", len(keys), len(got))
	}
}

func TestAttributesOfASparseServerAreDoesNotExist(t *testing.T) {
	got := attrs(&model.Server{Vendor: "v", APIReference: "x", VCPUs: 2, MemoryMiB: 2048}, "avx2")
	for _, k := range []string{
		v1alpha1.LabelFamily, v1alpha1.LabelCPUCores, v1alpha1.LabelCPUManufacturer, v1alpha1.LabelCPUFamily, v1alpha1.LabelCPUAllocation,
		v1alpha1.LabelCPUClockMHz, v1alpha1.LabelCPUL2PerCoreKiB, v1alpha1.LabelCPUL3MiB, v1alpha1.LabelHyperthreading,
		v1alpha1.LabelAcceleratorCount, v1alpha1.LabelAcceleratorMemoryMiB, v1alpha1.LabelAcceleratorManufacturer,
		v1alpha1.LabelAcceleratorModel, v1alpha1.LabelAcceleratorType, v1alpha1.LabelNetworkBaselineMbps,
		v1alpha1.LabelNetworkPeakMbps,
		// Not inspected: unknown, not false.
		v1alpha1.CPUFlagLabel("avx2"),
	} {
		if got[k] != "-" {
			t.Errorf("%s = %q, want DoesNotExist", k, got[k])
		}
	}
	// No bundled disk is a fact, not an unknown.
	if got[v1alpha1.LabelLocalStorageGB] != "0" || got[v1alpha1.LabelLocalStorageCount] != "0" || got[v1alpha1.LabelMemoryPerVCPUMiB] != "1024" {
		t.Errorf("local storage %q, memory per vCPU %q", got[v1alpha1.LabelLocalStorageGB], got[v1alpha1.LabelMemoryPerVCPUMiB])
	}
	// A local disk of unknown count: the size is known, the count is not.
	if got := attrs(&model.Server{Vendor: "v", APIReference: "x", VCPUs: 1, LocalStorageGB: 100}); got[v1alpha1.LabelLocalStorageGB] != "100" || got[v1alpha1.LabelLocalStorageCount] != "-" {
		t.Errorf("local disk of unknown count: %q %q", got[v1alpha1.LabelLocalStorageGB], got[v1alpha1.LabelLocalStorageCount])
	}
	// A tiny positive value never rounds to 0.
	got = attrs(&model.Server{Vendor: "v", APIReference: "x", VCPUs: 1, NetworkBaselineGbps: new(0.0001)})
	if got[v1alpha1.LabelNetworkBaselineMbps] != "1" {
		t.Errorf("network baseline = %q", got[v1alpha1.LabelNetworkBaselineMbps])
	}
}

func TestValueAndSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Xeon": "Xeon", "EPYC 9R14": "EPYC-9R14", "  Intel(R) ": "Intel-R", "a/b": "a-b", "": "", "---": "", "ünï": "n",
	} {
		got, ok := labels.Value(in)
		if got != want || ok != (want != "") {
			t.Errorf("Value(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if got, _ := labels.Value(strings.Repeat("a", 100)); len(got) != 63 {
		t.Errorf("long value has %d characters", len(got))
	}
	for in, want := range map[string]string{
		"Frankfurt": "frankfurt", "New York": "new-york", "Ashburn, VA": "ashburn-va", "São Paulo": "s-o-paulo", "": "", "--": "",
	} {
		got, ok := labels.Slug(in)
		if got != want || ok != (want != "") {
			t.Errorf("Slug(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestComputeMetricPercentiles(t *testing.T) {
	servers := make([]*model.Server, 0, 6)
	raw := map[model.Key]float64{}
	scores := []float64{100, 200, 200, 400, 800}
	for i, sc := range scores {
		s := &model.Server{Vendor: "v", ID: string(rune('a' + i)), VCPUs: 2}
		servers = append(servers, s)
		raw[s.Key()] = sc
	}
	servers = append(servers, &model.Server{Vendor: "v", ID: "unbenchmarked", VCPUs: 2})

	v := labels.ComputeMetric(labels.Metric{Name: "m", Scale: 10}, servers, raw)
	wantPct := map[string]int{"a": 0, "b": 25, "c": 25, "d": 75, "e": 100}
	for id, want := range wantPct {
		if got := v.Percentile[model.Key{Vendor: "v", ID: id}]; got != want {
			t.Errorf("percentile of %s = %d, want %d", id, got, want)
		}
	}
	if _, ok := v.Label(model.Key{Vendor: "v", ID: "unbenchmarked"}); ok {
		t.Error("an unbenchmarked server has a score")
	}
	if got, _ := v.Label(model.Key{Vendor: "v", ID: "e"}); got != 8000 {
		t.Errorf("scaled score = %d, want 8000", got)
	}
	reqs := v.TypeRequirements(model.Key{Vendor: "v", ID: "unbenchmarked"})
	if values(reqs[0]) != "-" || values(reqs[1]) != "-" {
		t.Errorf("unbenchmarked requirements: %v %v", reqs[0], reqs[1])
	}

	// Lower is better: the lowest value is the best, percentile 100.
	lo := labels.ComputeMetric(labels.Metric{Name: "lat", Scale: 1, LowerIsBetter: true}, servers, raw)
	if lo.Percentile[model.Key{Vendor: "v", ID: "a"}] != 100 || lo.Percentile[model.Key{Vendor: "v", ID: "e"}] != 0 {
		t.Errorf("lower-is-better percentiles: %v", lo.Percentile)
	}
	if r := lo.PerUSDRequirement(model.Key{Vendor: "v", ID: "a"}, 0.5); values(r) != "-" {
		t.Errorf("per-USD of a lower-is-better metric = %s", values(r))
	}
	if r := v.PerUSDRequirement(model.Key{Vendor: "v", ID: "e"}, 0.5); values(r) != "16000" {
		t.Errorf("per-USD = %s, want 16000", values(r))
	}
	if r := v.PerUSDRequirement(model.Key{Vendor: "v", ID: "e"}, 0); values(r) != "-" {
		t.Errorf("per-USD at price 0 = %s", values(r))
	}

	// Per vCPU: a type with more vCPUs does not win by size.
	big := &model.Server{Vendor: "v", ID: "big", VCPUs: 16}
	small := &model.Server{Vendor: "v", ID: "small", VCPUs: 2}
	pv := labels.ComputeMetric(labels.Metric{Name: "m", Scale: 1, PerVCPU: true}, []*model.Server{big, small},
		map[model.Key]float64{big.Key(): 1600, small.Key(): 400})
	if pv.Percentile[small.Key()] != 100 || pv.Percentile[big.Key()] != 0 {
		t.Errorf("per-vCPU percentiles: %v", pv.Percentile)
	}
	// A single benchmarked type is the best.
	one := labels.ComputeMetric(labels.Metric{Name: "m"}, []*model.Server{small}, map[model.Key]float64{small.Key(): 5})
	if one.Percentile[small.Key()] != 100 {
		t.Errorf("single type percentile = %d", one.Percentile[small.Key()])
	}
}

func TestSelectionAndProfiles(t *testing.T) {
	if got := values(labels.Selection(nil)); got != "-" {
		t.Errorf("no membership = %s", got)
	}
	if got := values(labels.Selection([]string{"b", "a", "b"})); got != "a,b" {
		t.Errorf("membership = %s", got)
	}
	reqs0 := labels.ProfileRequirements(nil)
	got := make([]string, 0, len(reqs0))
	for _, r := range reqs0 {
		got = append(got, r.Key+"="+values(r))
	}
	want := []string{
		v1alpha1.LabelStorageProfile + "=default", v1alpha1.LabelNetworkProfile + "=default", v1alpha1.LabelPlacementProfile + "=default",
	}
	if !slices.Equal(got, want) {
		t.Errorf("default profiles = %v", got)
	}
	reqs := labels.ProfileRequirements(map[labels.ProfileKind][]string{labels.ProfileStorage: {"db", "db"}})
	if values(reqs[0]) != "db,default" {
		t.Errorf("storage = %s", values(reqs[0]))
	}
}

func TestLocation(t *testing.T) {
	loc := func(r *model.Region) map[string]string {
		out := map[string]string{}
		for _, q := range labels.Location(r) {
			out[q.Key] = values(q)
		}
		return out
	}
	got := loc(&model.Region{CountryID: "DE", City: "Frankfurt am Main", State: "Hesse", GreenEnergy: new(true)})
	want := map[string]string{
		v1alpha1.LabelCountry: "de", v1alpha1.LabelContinent: "europe", v1alpha1.LabelCity: "frankfurt-am-main",
		v1alpha1.LabelState: "hesse", v1alpha1.LabelGreenEnergy: "true",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	got = loc(&model.Region{CountryID: "ZZ", GreenEnergy: new(false)})
	if got[v1alpha1.LabelCountry] != "zz" || got[v1alpha1.LabelContinent] != "-" || got[v1alpha1.LabelCity] != "-" ||
		got[v1alpha1.LabelState] != "-" || got[v1alpha1.LabelGreenEnergy] != "false" {
		t.Errorf("unknown location = %v", got)
	}
	if got := loc(&model.Region{}); got[v1alpha1.LabelCountry] != "-" || got[v1alpha1.LabelGreenEnergy] != "-" {
		t.Errorf("empty region = %v", got)
	}
	if len(labels.LocationKeys) != 5 {
		t.Errorf("location keys = %v", labels.LocationKeys)
	}
}

func TestContinents(t *testing.T) {
	for country, want := range map[string]string{
		"de": "europe", "US": "north-america", "br": "south-america", "jp": "asia", "ae": "asia", "za": "africa",
		"au": "oceania", "mx": "north-america", "tr": "asia", "ru": "europe", "gb": "europe", "aq": "antarctica",
	} {
		if got, ok := labels.ContinentOf(country); !ok || got != want {
			t.Errorf("ContinentOf(%s) = %q, %v; want %q", country, got, ok, want)
		}
	}
	if _, ok := labels.ContinentOf("zz"); ok {
		t.Error("zz has a continent")
	}
	// Every country of an active region in the Keeper snapshot has a continent
	// (all vendors).
	type region struct {
		VendorID  string `json:"vendor_id"`
		CountryID string `json:"country_id"`
		Status    string `json:"status"`
	}
	regions := make([]region, 0, 300)
	for _, v := range []string{"aws", "gcp", "azure", "alicloud", "hcloud", "ovh", "upcloud", "vultr"} {
		regions = append(regions, catalogtest.Rows[region](t, "region/"+v)...)
	}
	checked := 0
	for _, r := range regions {
		if r.Status != "active" {
			continue
		}
		checked++
		if _, ok := labels.ContinentOf(r.CountryID); !ok {
			t.Errorf("%s region in country %q has no continent", r.VendorID, r.CountryID)
		}
	}
	if checked < 100 {
		t.Errorf("only %d active regions checked", checked)
	}
}

func TestRegistration(t *testing.T) {
	flagKeys := labels.FlagKeys()
	if len(flagKeys) != len(v1alpha1.CPUFlagLabelValues) {
		t.Fatalf("%d flag keys for %d flags", len(flagKeys), len(v1alpha1.CPUFlagLabelValues))
	}
	metrics := []string{"reg-test-a", "reg-test-b"}
	keys := labels.MetricLabelKeys(metrics)
	if len(keys) != 6 {
		t.Fatalf("keys = %v", keys)
	}
	if missing := labels.MissingKeys(metrics); len(missing) != 6 {
		t.Errorf("missing before registration = %v", missing)
	}
	if got := labels.RegisteredMetrics(metrics); len(got) != 0 {
		t.Errorf("registered metrics before registration = %v", got)
	}
	if added := labels.RegisterKeys(append(keys[:3:3], flagKeys...)...); len(added) < 3 {
		t.Errorf("added = %v", added)
	}
	if got := labels.RegisteredMetrics(metrics); !slices.Equal(got, []string{"reg-test-a"}) {
		t.Errorf("registered metrics = %v", got)
	}
	if missing := labels.MissingKeys(metrics); len(missing) != 3 {
		t.Errorf("missing after the first registration = %v", missing)
	}
	// Registering again adds nothing.
	if added := labels.RegisterKeys(keys[:3]...); len(added) != 0 {
		t.Errorf("second registration added %v", added)
	}
	if got := labels.RegisteredFlags([]string{"avx2", "nope"}); !slices.Equal(got, []string{"avx2"}) {
		t.Errorf("registered flags = %v", got)
	}
}

// TestCuratedFlagEnumMatchesTheCRD keeps the Go list of registered cpu-flag keys
// in step with the closed enum of spec.catalog.cpuFlagLabels.
func TestCuratedFlagEnumMatchesTheCRD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "config", "crd", "bases", "scarpenter.sparecores.com_scnodeclasses.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties map[string]struct {
							Properties map[string]struct {
								Properties map[string]struct {
									Items struct {
										Enum []string `json:"enum"`
									} `json:"items"`
								} `json:"properties"`
							} `json:"properties"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	enum := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["catalog"].Properties["cpuFlagLabels"].Items.Enum
	if len(enum) == 0 {
		t.Fatal("no cpuFlagLabels enum found in the CRD")
	}
	a, b := slices.Clone(enum), slices.Clone(v1alpha1.CPUFlagLabelValues)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		t.Errorf("CRD enum %v != Go list %v", a, b)
	}
	for _, f := range v1alpha1.DefaultCPUFlagLabels {
		if !slices.Contains(b, f) {
			t.Errorf("default flag %s is not in the curated set", f)
		}
	}
}
