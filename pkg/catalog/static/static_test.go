/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package static_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/static"
)

const doc = `
vendors:
  - name: fake
    regions:
      - {name: region-a, country: DE, city: Frankfurt, greenEnergy: true, zones: [region-a1, region-a2]}
      - {name: region-b, country: US, zones: [region-b1]}
    types:
      - name: small-2
        family: small
        arch: amd64
        vcpus: 2
        memoryMiB: 4096
        prices:
          - {region: region-a, onDemand: 0.05, spot: 0.02}
          - {region: region-b, onDemand: 0.06}
      - name: arm-4
        family: arm
        arch: arm64
        vcpus: 4
        memoryMiB: 8192
        gpus: 1
        gpuModel: T4
        gpuManufacturer: NVIDIA
        prices:
          - {region: region-a, onDemand: 0.20}
`

func TestStaticCatalogAsASource(t *testing.T) {
	src, err := static.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	facts := catalog.StaticFacts{ZonesByVendor: map[string]map[string][]catalog.Zone{
		"fake": {
			"region-a": {{ID: "region-a1", Name: "region-a1"}, {ID: "region-a2", Name: "region-a2"}},
			"region-b": {{ID: "region-b1", Name: "region-b1"}},
		},
	}}
	cl := catalog.NewClass("c", catalog.Scope{"fake": nil})
	c := catalog.New(catalog.Options{Source: src, Facts: facts})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	its, err := c.InstanceTypes(t.Context(), catalog.Query{Class: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 2 {
		t.Fatalf("got %d types", len(its))
	}
	byName := map[string]int{}
	for _, it := range its {
		byName[it.Name] = len(it.Offerings)
	}
	// small-2: region-a (2 zones x 2 capacity types) and region-b (1 zone, on demand).
	if byName["fake.small-2"] != 5 || byName["fake.arm-4"] != 2 {
		t.Errorf("offerings per type = %v", byName)
	}
	for _, it := range its {
		if it.Name == "fake.arm-4" {
			if !it.Requirements.Get(corev1.LabelArchStable).Has("arm64") || !it.Requirements.Get(v1alpha1.LabelAcceleratorModel).Has("T4") {
				t.Errorf("arm-4: %v", it.Requirements)
			}
		}
	}

	// The region allow-list narrows what is read.
	cl2 := catalog.NewClass("d", catalog.Scope{"fake": {"region-b"}})
	c.SetClasses([]catalog.Class{cl2})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	its, _ = c.InstanceTypes(t.Context(), catalog.Query{Class: "d"})
	if len(its) != 1 || its[0].Name != "fake.small-2" || len(its[0].Offerings) != 1 {
		t.Errorf("region-b: %v", its)
	}
}

func TestStaticMetricsAndSelectionsAreInvalid(t *testing.T) {
	src, err := static.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	data, err := src.Load(t.Context(), catalog.Request{
		Scope:      catalog.Scope{"fake": nil},
		Metrics:    []catalog.MetricQuery{{ID: "m", Vendor: "fake"}},
		Selections: []catalog.SelectionQuery{{ID: "s", Vendor: "fake"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if data.Invalid["m"] == "" || data.Invalid["s"] == "" || data.Version == "" {
		t.Errorf("data: %+v", data)
	}
}

func TestStaticRejectsBadFiles(t *testing.T) {
	for name, bad := range map[string]string{
		"unknown field":  "vendors: [{name: v, bogus: 1}]",
		"no vcpus":       "vendors: [{name: v, regions: [{name: r}], types: [{name: t, memoryMiB: 1, prices: []}]}]",
		"bad arch":       "vendors: [{name: v, regions: [{name: r}], types: [{name: t, vcpus: 1, memoryMiB: 1, arch: mips}]}]",
		"unknown region": "vendors: [{name: v, regions: [{name: r}], types: [{name: t, vcpus: 1, memoryMiB: 1, prices: [{region: x, onDemand: 1}]}]}]",
	} {
		if _, err := static.Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "static catalog") {
			t.Errorf("%s: error %v", name, err)
		}
	}
	if _, err := static.Load("/does/not/exist.yaml"); err == nil {
		t.Error("a missing file was accepted")
	}
}
