/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/vendortest"
)

type snapRegion struct {
	VendorID     string `json:"vendor_id"`
	RegionID     string `json:"region_id"`
	APIReference string `json:"api_reference"`
	Status       string `json:"status"`
}

type snapZone struct {
	VendorID string `json:"vendor_id"`
	RegionID string `json:"region_id"`
	ZoneID   string `json:"zone_id"`
	Status   string `json:"status"`
}

func ovhRegions(t testing.TB) []snapRegion {
	out := vendortest.SnapshotRows[snapRegion](t, "region", "ovh")
	if len(out) == 0 {
		t.Fatal("the snapshot has no OVH regions")
	}
	return out
}

// Plan 14 A8: every Keeper OVH region of the committed snapshot maps to an API
// name or is listed as unmapped, and the table has no row the snapshot lacks (a
// stale row would hide a rename). Keeper's snapshot has the active regions
// only, so the rows noted as inactive are the ones it lacks.
func TestEveryKeeperRegionMapsOrIsUnmapped(t *testing.T) {
	m, err := NewRegionMap(nil)
	if err != nil {
		t.Fatal(err)
	}
	inSnapshot := map[string]bool{}
	for _, r := range ovhRegions(t) {
		inSnapshot[r.APIReference] = true
		_, mapped := m.API(r.APIReference)
		listed := slices.Contains(m.Unmapped(), strings.ToUpper(r.APIReference))
		if mapped == listed {
			t.Errorf("Keeper region %s (%s): mapped=%v listed as unmapped=%v, want exactly one", r.APIReference, r.Status, mapped, listed)
		}
		// An active region must not be silently dropped for being inactive.
		if r.Status == "inactive" && mapped {
			t.Errorf("inactive Keeper region %s is mapped", r.APIReference)
		}
	}
	for _, row := range regionAliases {
		// Keeper's snapshot has the active regions only: a row noted as inactive
		// is not in it, and one that is in it is no longer inactive.
		if row.Note == noteInactive && inSnapshot[row.Keeper] {
			t.Errorf("table row %s is noted inactive, but the snapshot has it", row.Keeper)
		}
		if row.Note != noteInactive && !inSnapshot[row.Keeper] {
			t.Errorf("table row %s is not a Keeper region of the snapshot (stale)", row.Keeper)
		}
	}
	// The regions the plan says Keeper prices (b3-8 in 16 regions): all map.
	for _, name := range []string{"GRA7", "GRA9", "GRA11", "SBG5", "SBG7", "DE1", "UK1", "WAW1", "BHS5", "SGP1", "SYD1", "RBX-A", "AP-SOUTH-MUM-1", "AP-SOUTHEAST-SYD-2", "EU-WEST-PAR", "EU-SOUTH-MIL"} {
		if _, ok := m.API(name); !ok {
			t.Errorf("priced region %s is unmapped", name)
		}
	}
}

func TestRegionMapLookups(t *testing.T) {
	m, _ := NewRegionMap(nil)
	if a, ok := m.API("gra11"); !ok || a != "GRA11" {
		t.Errorf("API(gra11) = %q %v: lookups ignore case", a, ok)
	}
	if k, ok := m.Keeper("eu-west-par"); !ok || k != "EU-WEST-PAR" {
		t.Errorf("Keeper = %q %v", k, ok)
	}
	if _, ok := m.API("GRA"); ok {
		t.Error("an unmapped trigram has no API name")
	}
	if !m.Known("GRA") || m.Known("MARS1") {
		t.Error("Known: listed regions are known, others not")
	}
	joined, unknown := m.Join([]string{"GRA11", "EU-WEST-PAR", "MARS1", "SBG5"})
	names := make([]string, 0, len(joined))
	for _, r := range joined {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "GRA11,EU-WEST-PAR,SBG5" || strings.Join(unknown, ",") != "MARS1" {
		t.Errorf("Join: %v, unknown %v", names, unknown)
	}
}

func TestRegionMapOverrides(t *testing.T) {
	m, err := NewRegionMap(map[string]string{"gra11": "eu-west-gra", "GRA": "GRA1", "SBG5": ""})
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := m.API("GRA11"); a != "EU-WEST-GRA" {
		t.Errorf("override: %s", a)
	}
	if k, ok := m.Keeper("EU-WEST-GRA"); !ok || k != "GRA11" {
		t.Errorf("reverse of an override: %s %v", k, ok)
	}
	if _, ok := m.Keeper("GRA11"); ok {
		t.Error("the replaced API name no longer maps back")
	}
	if a, ok := m.API("GRA"); !ok || a != "GRA1" {
		t.Errorf("an unmapped row can be mapped: %s %v", a, ok)
	}
	if _, ok := m.API("SBG5"); ok || !slices.Contains(m.Unmapped(), "SBG5") {
		t.Error("an empty value unmaps")
	}
	if _, err := NewRegionMap(map[string]string{"GRA9": "GRA11"}); err == nil {
		t.Error("two Keeper regions on one API name must fail")
	}
}

// Keeper's zones of the snapshot are what ZoneIDs derives, for every active
// region, whatever Nova names its zones: "nova" in a 1-AZ region, the zone IDs
// themselves in a 3-AZ one (the real names are spike S4).
func TestZoneIDsMatchTheSnapshot(t *testing.T) {
	all := vendortest.SnapshotRows[snapZone](t, "zone", "ovh")
	want := map[string][]string{}
	for _, z := range all {
		if z.VendorID == "ovh" && z.Status == "active" {
			want[z.RegionID] = append(want[z.RegionID], z.ZoneID)
		}
	}
	m, _ := NewRegionMap(nil)
	for _, r := range ovhRegions(t) {
		if _, ok := m.API(r.APIReference); !ok || r.Status != "active" {
			continue
		}
		var azs []string
		switch len(want[r.APIReference]) {
		case 1:
			azs = []string{"nova"}
		default:
			azs = slices.Clone(want[r.APIReference])
		}
		var got []string
		for _, z := range ZoneIDs(r.APIReference, azs) {
			got = append(got, z.ID)
		}
		slices.Sort(got)
		w := slices.Clone(want[r.APIReference])
		slices.Sort(w)
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s: zones %v, Keeper has %v", r.APIReference, got, w)
		}
	}
}

func TestZoneIDsNames(t *testing.T) {
	z := ZoneIDs("EU-WEST-PAR", []string{"nova-c", "nova-a", "nova-b"})
	if z[0].ID != "eu-west-par-a" || z[0].Name != "nova-a" || z[2].ID != "eu-west-par-c" {
		t.Errorf("letters from the suffix: %+v", z)
	}
	z = ZoneIDs("EU-SOUTH-MIL", []string{"zone-x", "zone-y", "zone-z"})
	if z[0].ID != "eu-south-mil-a" || z[1].ID != "eu-south-mil-b" || z[2].ID != "eu-south-mil-c" {
		t.Errorf("sorted order when the names have no letter: %+v", z)
	}
	if z = ZoneIDs("GRA11", []string{"nova"}); len(z) != 1 || z[0].ID != "gra11-a" || z[0].Name != "nova" {
		t.Errorf("1-AZ: %+v", z)
	}
	if z = ZoneIDs("GRA11", nil); len(z) != 0 {
		t.Errorf("no zones: %+v", z)
	}
}
