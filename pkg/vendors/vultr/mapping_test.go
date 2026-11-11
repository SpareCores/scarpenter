/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestProviderID(t *testing.T) {
	id := ProviderID("ewr", "claim-1")
	if id != "scarpenter://vultr/ewr/claim-1" {
		t.Fatalf("got %q", id)
	}
	region, claim, err := ParseProviderID(id)
	if err != nil || region != "ewr" || claim != "claim-1" {
		t.Fatalf("got %q %q %v", region, claim, err)
	}
	for _, bad := range []string{"", "scarpenter://aws/ewr/claim-1", "scarpenter://vultr/ewr", "scarpenter://vultr//claim", "vultr://ewr/claim", "scarpenter://vultr/a/b/c"} {
		if _, _, err := ParseProviderID(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestZoneIsTheRegion(t *testing.T) {
	if z := Zone("ewr"); z.ID != "ewr" || z.Name != "ewr" {
		t.Fatalf("got %+v", z)
	}
}

func TestRegionsOf(t *testing.T) {
	listed := []govultr.Region{{ID: "fra"}, {ID: "ewr"}, {ID: "sao"}}
	names := func(rs []vendors.Region) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return out
	}
	if got := names(RegionsOf(listed, nil)); !slices.Equal(got, []string{"ewr", "fra", "sao"}) {
		t.Fatalf("all: %v", got)
	}
	if got := names(RegionsOf(listed, []string{"sao", "nope"})); !slices.Equal(got, []string{"sao"}) {
		t.Fatalf("allowed: %v", got)
	}
}

func TestPlanOfAndRootDisk(t *testing.T) {
	ct := vendors.CatalogType{Vendor: "vultr", APIReference: "vc2-2c-4gb", Arch: "amd64", LocalStorageGB: 80}
	if p, err := PlanOf(ct); err != nil || p != "vc2-2c-4gb" {
		t.Fatalf("got %q %v", p, err)
	}
	if got := RootDiskForType(ct); got != 80_000_000_000 {
		t.Fatalf("root disk %d", got)
	}
	ct.Arch = "arm64"
	if _, err := PlanOf(ct); !vendors.IsClass(err, vendors.Unsupported) {
		t.Fatalf("arm64: %v", err)
	}
	if _, err := PlanOf(vendors.CatalogType{Arch: "amd64"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("no reference: %v", err)
	}
}

func TestTagStyles(t *testing.T) {
	logical := map[vendors.TagKey]string{vendors.TagCluster: "my-cluster", vendors.TagNodeClaim: "claim-1"}
	extra := map[string]string{"team": "data", "scarpenter.sparecores.com/cluster": "spoofed"}
	got := TagStyleDomain.Tags(logical, extra)
	want := []string{
		"scarpenter.sparecores.com/cluster=my-cluster", "scarpenter.sparecores.com/nodeclaim=claim-1", "team=data",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("domain style: %v", got)
	}
	if back := TagStyleDomain.ParseTags(got); !maps.Equal(back, logical) {
		t.Fatalf("round trip: %v", back)
	}
	dash := TagStyleDash.Tags(logical, nil)
	if !slices.Equal(dash, []string{"scarpenter-cluster=my-cluster", "scarpenter-nodeclaim=claim-1"}) {
		t.Fatalf("dash style: %v", dash)
	}
	if back := TagStyleDash.ParseTags(dash); !maps.Equal(back, logical) {
		t.Fatalf("dash round trip: %v", back)
	}
	// The other style's tags and foreign tags are not ours; values may hold "=".
	if back := TagStyleDash.ParseTags(got); len(back) != 0 {
		t.Fatalf("read the other style: %v", back)
	}
	if back := TagStyleDomain.ParseTags([]string{"noequals", "scarpenter.sparecores.com/cluster=a=b"}); back[vendors.TagCluster] != "a=b" {
		t.Fatalf("value with =: %v", back)
	}
}

func TestDescriptionMarker(t *testing.T) {
	m := Marker{Cluster: "my-cluster", NodeClass: "default", ResourceKind: "firewallGroups"}
	d, err := FormatDescription("scarpenter-my-cluster-firewall-groups-ewr", m)
	if err != nil {
		t.Fatal(err)
	}
	const want = "scarpenter-my-cluster-firewall-groups-ewr | cluster=my-cluster nodeclass=default managed-by=scarpenter resource-kind=firewallGroups"
	if d != want {
		t.Fatalf("got %q", d)
	}
	name, got, ok := ParseDescription(d)
	if !ok || name != "scarpenter-my-cluster-firewall-groups-ewr" || got != m {
		t.Fatalf("parse: %q %+v %v", name, got, ok)
	}
	for _, bad := range []string{
		"", "plain", "name | cluster=a", "name | cluster=a nodeclass=b managed-by=someone resource-kind=k",
		"name | cluster=a nodeclass=b managed-by=scarpenter", "name | cluster= nodeclass=b managed-by=scarpenter resource-kind=k",
		"name | cluster=a nodeclass=b managed-by=scarpenter resource-kind=k extra=1", "| cluster=a nodeclass=b managed-by=scarpenter resource-kind=k",
		"name | cluster=a nodeclass=b managed-by=scarpenter resource-kind=k cluster=a",
	} {
		if _, _, ok := ParseDescription(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	for _, m := range []Marker{{"a b", "n", "k"}, {"a", "", "k"}, {"a", "n|", "k"}} {
		if _, err := FormatDescription("x", m); err == nil {
			t.Errorf("%+v formatted", m)
		}
	}
	if _, err := FormatDescription("two words", Marker{"a", "n", "k"}); err == nil {
		t.Error("a name with a space formatted")
	}
}

func TestClassifyPlan(t *testing.T) {
	cases := []struct {
		id   string
		disk int
		ok   bool
		why  string
	}{
		{"vc2-2c-4gb", 80, true, ""},
		{"vhf-1c-1gb", 32, true, ""},
		{"vhp-12c-24gb-intel", 480, true, ""},
		{"voc-c-2c-4gb-50s-amd", 50, true, ""},
		{"vx1-g-4c-16g", 100, true, ""},
		{"vx1-g-4c-16g", 1, false, "diskless"},
		{"vx1-g-4c-16g", 0, false, "diskless"},
		{"vc2-1c-0.5gb-free", 10, false, "free"},
		{"vcg-a100-12c-120g-80vram", 0, false, "Cloud GPU"},
		{"vdm-8c-64g", 0, false, "bare metal"},
		{"vbm-4c-32gb", 240, false, "bare metal"},
		{"zzz-1c-1gb", 10, false, "unclassified"},
		{"", 10, false, "unclassified"},
	}
	for _, c := range cases {
		ok, why := ClassifyPlan(c.id, c.disk)
		if ok != c.ok || (!ok && !strings.Contains(why, c.why)) || (ok && why != "") {
			t.Errorf("ClassifyPlan(%q, %d) = %v %q, want %v %q", c.id, c.disk, ok, why, c.ok, c.why)
		}
	}
}

// Every family of the representative plan list must be classified on purpose.
func TestEveryFamilyOfThePlanListIsClassified(t *testing.T) {
	for _, id := range representativePlans {
		if _, why := ClassifyPlan(id, 100); strings.Contains(why, "unclassified") {
			t.Errorf("%s: %s", id, why)
		}
	}
}

// representativePlans are plan IDs of each family of the public plan list
// (GET /v2/plans needs no key), not a snapshot of it: Keeper's committed
// snapshot has no Vultr rows yet.
var representativePlans = []string{
	"vc2-1c-0.5gb-free", "vc2-1c-1gb", "vc2-2c-4gb", "vhf-1c-1gb", "vhp-1c-1gb-amd", "vhp-12c-24gb-intel",
	"voc-c-2c-4gb-50s-amd", "voc-g-2c-8gb-50s-amd", "vx1-g-2c-8g", "vcg-a16-6c-64g-16vram", "vdm-a100-8c-64g", "vbm-4c-32gb",
}
