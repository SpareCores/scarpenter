/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"encoding/json"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"

	keeper "github.com/SpareCores/sc-keeper-go"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

func TestKeeperSourceFilters(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"hcloud": {"fsn1"}})
	its := all(t, c, catalog.Filter{})
	if len(its) != 13 {
		t.Fatalf("hcloud has %d types in fsn1, want 13 (the active ones with a price)", len(its))
	}
	for _, it := range its {
		if len(it.Offerings) != 1 {
			t.Errorf("%s has %d offerings: hcloud has no zones and no spot", it.Name, len(it.Offerings))
			continue
		}
		o := it.Offerings[0]
		if !o.Requirements.Get(corev1.LabelTopologyZone).Has("fsn1") || !o.Requirements.Get(corev1.LabelTopologyRegion).Has("fsn1") ||
			!o.Requirements.Get("karpenter.sh/capacity-type").Has("on-demand") {
			t.Errorf("%s: %v", it.Name, reqLines(o.Requirements))
		}
		if !o.Requirements.Get(v1alpha1.LabelCountry).Has("de") {
			t.Errorf("%s: country %v", it.Name, reqLines(o.Requirements))
		}
	}
}

func TestGCPUsesKeepersNumericIDsAndTheVendorsNames(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"gcp": {"europe-west3"}}, "gcp")
	its := all(t, c, catalog.Filter{})
	if len(its) < 100 {
		t.Fatalf("gcp has %d types", len(its))
	}
	it := byName(its, "gcp.n2-standard-4")
	if it == nil {
		t.Fatal("no n2-standard-4")
	}
	// The region label is the vendor's name (Keeper's id is 1300); zones are
	// the vendor's names with Keeper's numeric IDs.
	if r := it.Requirements.Get(corev1.LabelTopologyRegion); r.Len() != 1 || !r.Has("europe-west3") {
		t.Errorf("regions = %v", r.Values())
	}
	zones, ids := it.Requirements.Get(corev1.LabelTopologyZone), it.Requirements.Get(v1alpha1.LabelZoneID)
	if !zones.Has("europe-west3-a") || !zones.Has("europe-west3-b") || !zones.Has("europe-west3-c") {
		t.Errorf("zones = %v", zones.Values())
	}
	for _, id := range ids.Values() {
		if _, err := json.Number(id).Int64(); err != nil {
			t.Errorf("zone id %q is not numeric", id)
		}
	}
	if !it.Requirements.Get(v1alpha1.LabelCountry).Has("de") {
		t.Errorf("country = %v", it.Requirements.Get(v1alpha1.LabelCountry).Values())
	}
	// Both capacity types exist at GCP.
	if r := it.Requirements.Get("karpenter.sh/capacity-type"); !r.Has("spot") || !r.Has("on-demand") {
		t.Errorf("capacity types = %v", r.Values())
	}
}

func TestRegionAllowListAcceptsAliasesAndIDs(t *testing.T) {
	for _, region := range []string{"europe-west3", "1300"} {
		c, _ := refreshed(t, catalog.Scope{"gcp": {region}}, "gcp")
		if its := all(t, c, catalog.Filter{}); len(its) < 100 {
			t.Errorf("region %q: %d types", region, len(its))
		}
	}
	// A region Keeper does not know is ignored; nothing is read for it.
	c, _ := refreshed(t, catalog.Scope{"hcloud": {"mars-1"}})
	if its := all(t, c, catalog.Filter{}); len(its) != 0 {
		t.Errorf("unknown region: %d types", len(its))
	}
}

func TestUnusableServersAreDropped(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"aws": {"us-east-1"}}, "aws")
	its := all(t, c, catalog.Filter{})
	// Inactive, retired and planned-for-retirement servers are not offered, and
	// neither are architectures Kubernetes does not know.
	for _, it := range its {
		if a := it.Requirements.Get(corev1.LabelArchStable); !a.Has("amd64") && !a.Has("arm64") {
			t.Errorf("%s: arch %v", it.Name, a.Values())
		}
		for _, o := range it.Offerings {
			if o.Price <= 0 {
				t.Errorf("%s has an offering priced %v", it.Name, o.Price)
			}
		}
	}
	for _, name := range []string{"aws.t3a.small", "aws.m7i.large", "aws.c7g.xlarge", "aws.p4d.24xlarge"} {
		if byName(its, name) == nil {
			t.Errorf("%s is missing", name)
		}
	}
}

// Retiring types stay in the catalog (overview 5.1). Keeper's snapshot has no
// inactive or retired servers at all.
func TestPlannedForRetirementServersAreKept(t *testing.T) {
	rows := catalogtest.Rows[keeper.TableServer](t, "server/gcp")
	data, err := catalogtest.Source(t).Load(t.Context(), catalog.Request{Scope: catalog.Scope{"gcp": {"europe-west3"}}})
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, s := range data.Servers {
		have[s.APIReference] = true
	}
	planned := 0
	for i := range rows {
		r := &rows[i]
		switch r.Status {
		case "planned-for-retirement":
			planned++
			if !have[r.APIReference] {
				t.Errorf("%s (planned for retirement) was dropped", r.APIReference)
			}
		case "active":
		default:
			t.Errorf("the snapshot has a %s server %s", r.Status, r.APIReference)
		}
	}
	if planned == 0 {
		t.Fatal("the snapshot has no planned-for-retirement gcp server")
	}
}

func TestEditingAClassPrunesItsOldBuild(t *testing.T) {
	c, cl := refreshed(t, catalog.Scope{"aws": {"us-east-1"}}, "aws")
	if len(all(t, c, catalog.Filter{})) == 0 {
		t.Fatal("no types")
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range 3 {
		edited := cl
		edited.MaxPods = int64(30 + i)
		edited.Finalize()
		c.SetClasses([]catalog.Class{edited})
		if len(all(t, c, catalog.Filter{})) == 0 {
			t.Fatal("no types")
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	// Three edits would add three builds (~30 MiB each for one region); pruned,
	// only one stays.
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 25<<20 {
		t.Errorf("the heap grew by %d MiB over three edits", grown>>20)
	}
}
