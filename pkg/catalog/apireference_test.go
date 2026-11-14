/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"compress/gzip"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// A Keeper-shaped set of OCI: the sized types share the api_reference of their
// shape, the fixed shape has its own, and another vendor has one name per
// server.
func duplicateData() *model.Data {
	srv := func(vendor, id, ref string, vcpus int) model.Server {
		return model.Server{Vendor: vendor, ID: id, APIReference: ref, VCPUs: vcpus, Arch: "amd64", MemoryMiB: float64(vcpus) * 4096}
	}
	return &model.Data{
		Servers: []model.Server{
			srv("oci", "VM.Standard.E5.Flex.4-16", "VM.Standard.E5.Flex", 8),
			srv("oci", "BM.Standard3.64", "BM.Standard3.64", 128),
			srv("oci", "VM.Standard.E5.Flex.2-8", "VM.Standard.E5.Flex", 4),
			srv("oci", "VM.Standard.A1.Flex.2-8", "VM.Standard.A1.Flex", 2),
			srv("oci", "VM.Standard.A1.Flex.4-8", "VM.Standard.A1.Flex", 4),
			srv("aws", "1", "m7i.large", 2),
			srv("aws", "2", "m7i.xlarge", 4),
			// The same reference at another vendor is not a duplicate.
			srv("hetzner", "9", "m7i.large", 2),
		},
	}
}

func apiReferences(data *model.Data) map[string]string {
	out := map[string]string{}
	for _, s := range data.Servers {
		out[s.Vendor+"/"+s.ID] = s.APIReference
	}
	return out
}

func TestRuleRNamesDuplicatesByServerID(t *testing.T) {
	data := duplicateData()
	if n := catalog.UniqueAPIReferences(data); n != 4 {
		t.Errorf("renamed %d servers, want the 4 sized ones", n)
	}
	want := map[string]string{
		"oci/VM.Standard.E5.Flex.4-16": "VM.Standard.E5.Flex.4-16",
		"oci/VM.Standard.E5.Flex.2-8":  "VM.Standard.E5.Flex.2-8",
		"oci/VM.Standard.A1.Flex.2-8":  "VM.Standard.A1.Flex.2-8",
		"oci/VM.Standard.A1.Flex.4-8":  "VM.Standard.A1.Flex.4-8",
		"oci/BM.Standard3.64":          "BM.Standard3.64",
		"aws/1":                        "m7i.large",
		"aws/2":                        "m7i.xlarge",
		"hetzner/9":                    "m7i.large",
	}
	got := apiReferences(data)
	if !maps.Equal(got, want) {
		t.Errorf("api references = %v, want %v", got, want)
	}
	// It is idempotent.
	if n := catalog.UniqueAPIReferences(data); n != 0 {
		t.Errorf("a second run renamed %d servers", n)
	}
}

// The scores of a vendor are read by server ID and keyed by APIReference: after
// the rule each size has its own score.
func TestRuleRScoresMapToSizedNames(t *testing.T) {
	data := duplicateData()
	catalog.UniqueAPIReferences(data)
	byID := map[string]float64{
		"VM.Standard.E5.Flex.4-16": 20, "VM.Standard.E5.Flex.2-8": 10, "BM.Standard3.64": 99,
	}
	got := catalog.ByAPIReference(data, "oci", byID)
	want := map[string]float64{
		"VM.Standard.E5.Flex.4-16": 20, "VM.Standard.E5.Flex.2-8": 10, "BM.Standard3.64": 99,
	}
	if !maps.Equal(got, want) {
		t.Errorf("scores = %v, want %v", got, want)
	}
}

// The local selection names its candidates by APIReference, so the profile's
// members are the sized names.
func TestRuleRSelectionCandidatesAreSized(t *testing.T) {
	data := duplicateData()
	catalog.UniqueAPIReferences(data)
	for _, s := range data.Servers {
		data.Prices = append(data.Prices, model.Price{
			Vendor: s.Vendor, RegionID: "r1", ZoneID: "z1", ServerID: s.ID, CapacityType: model.CapacityOnDemand, USDPerHour: 0.1,
		})
	}
	cands := catalog.SelectionCandidates(data, "oci", nil, nil, keeper.AllocationOnDemand)
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		names = append(names, c.Name())
	}
	slices.Sort(names)
	want := []string{
		"BM.Standard3.64", "VM.Standard.A1.Flex.2-8", "VM.Standard.A1.Flex.4-8",
		"VM.Standard.E5.Flex.2-8", "VM.Standard.E5.Flex.4-16",
	}
	if !slices.Equal(names, want) {
		t.Errorf("candidates = %v, want %v", names, want)
	}
}

// Through the catalog every size is an instance type of its own, not one per
// shape.
func TestRuleRInstanceTypesArePerSize(t *testing.T) {
	c := newSynthetic(t, func(d *model.Data) {
		d.Servers = []model.Server{
			{Vendor: "v", ID: "S.2-8", APIReference: "S", VCPUs: 4, Arch: "amd64", MemoryMiB: 8192},
			{Vendor: "v", ID: "S.4-16", APIReference: "S", VCPUs: 8, Arch: "amd64", MemoryMiB: 16384},
		}
		d.Prices = []model.Price{
			{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: "S.2-8", CapacityType: model.CapacityOnDemand, USDPerHour: 0.1},
			{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: "S.4-16", CapacityType: model.CapacityOnDemand, USDPerHour: 0.2},
		}
		catalog.UniqueAPIReferences(d)
	}, nil)
	its := all(t, c, catalog.Filter{})
	names := make([]string, 0, len(its))
	for _, it := range its {
		names = append(names, it.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"v.S.2-8", "v.S.4-16"}) {
		t.Errorf("instance types = %v, want one per size", names)
	}
}

// Without the rule (a source that forgot it) the snapshot keeps the lower ID
// and drops the other, as before; the drop is logged.
func TestDuplicateReferenceInASnapshotKeepsTheLowerID(t *testing.T) {
	c := newSynthetic(t, func(d *model.Data) {
		d.Servers = []model.Server{
			{Vendor: "v", ID: "S.4-16", APIReference: "S", VCPUs: 8, Arch: "amd64", MemoryMiB: 16384},
			{Vendor: "v", ID: "S.2-8", APIReference: "S", VCPUs: 4, Arch: "amd64", MemoryMiB: 8192},
		}
		d.Prices = []model.Price{
			{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: "S.2-8", CapacityType: model.CapacityOnDemand, USDPerHour: 0.1},
			{Vendor: "v", RegionID: "r1", ZoneID: "z1", ServerID: "S.4-16", CapacityType: model.CapacityOnDemand, USDPerHour: 0.2},
		}
	}, nil)
	its := all(t, c, catalog.Filter{})
	if len(its) != 1 || its[0].Name != "v.S" {
		t.Fatalf("%d instance types, want the one named v.S", len(its))
	}
}

// The committed Keeper snapshot has no duplicates: the rule changes the data of
// no vendor in it.
func TestRuleRChangesNothingInTheCommittedSnapshot(t *testing.T) {
	f, err := os.Open(filepath.Join(catalogtest.SnapshotDir(), "tables", "server.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var rows []keeper.TableServer
	if err := json.NewDecoder(zr).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	data := &model.Data{}
	vendors := map[string]bool{}
	for i := range rows {
		if srv, ok := catalog.ServerFromTable(&rows[i]); ok {
			data.Servers = append(data.Servers, srv)
			vendors[srv.Vendor] = true
		}
	}
	if len(vendors) < 5 || len(data.Servers) < 1000 {
		t.Fatalf("only %d servers of %d vendors read: the test checks nothing", len(data.Servers), len(vendors))
	}
	before := apiReferences(data)
	if n := catalog.UniqueAPIReferences(data); n != 0 {
		t.Errorf("rule R renamed %d servers of the committed snapshot", n)
	}
	if !maps.Equal(before, apiReferences(data)) {
		t.Error("api references changed")
	}
}
