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
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

func explainWith(t *testing.T, c *catalog.Catalog, q catalog.Query) map[string]catalog.TypeReport {
	t.Helper()
	q.Class = "c"
	rep, err := c.Explain(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]catalog.TypeReport{}
	for _, tr := range rep.Types {
		out[tr.InstanceType] = tr
	}
	return out
}

func TestExplainNamesTheKeyThatTheNodePoolPreFilterRemoves(t *testing.T) {
	c := newSynthetic(t, nil, nil)
	in := func(key string, vals ...string) scheduling.Requirements {
		return scheduling.NewRequirements(scheduling.NewRequirement(key, corev1.NodeSelectorOpIn, vals...))
	}
	for name, tc := range map[string]struct {
		reqs     scheduling.Requirements
		typeName string
		key      string
	}{
		"architecture":  {in(corev1.LabelArchStable, "arm64"), "v.small", corev1.LabelArchStable},
		"instance type": {in(corev1.LabelInstanceTypeStable, "v.big"), "v.small", corev1.LabelInstanceTypeStable},
		"region":        {in(corev1.LabelTopologyRegion, "elsewhere"), "v.small", corev1.LabelTopologyRegion},
		"zone":          {in(corev1.LabelTopologyZone, "zone-9"), "v.big", corev1.LabelTopologyZone},
		"capacity type": {in(karpv1.CapacityTypeLabelKey, "spot"), "v.big", karpv1.CapacityTypeLabelKey},
	} {
		t.Run(name, func(t *testing.T) {
			tr := explainWith(t, c, catalog.Query{Requirements: tc.reqs})[tc.typeName]
			if want := (catalog.Reason{Code: catalog.NodePoolPreFilter, Detail: tc.key}); tr.Dropped != want {
				t.Errorf("%s: dropped %+v, want %+v", tc.typeName, tr.Dropped, want)
			}
		})
	}
	// An offering the filter removes from a type that stays.
	tr := explainWith(t, c, catalog.Query{Requirements: in(karpv1.CapacityTypeLabelKey, "on-demand")})["v.small"]
	if tr.Dropped.Code != "" {
		t.Fatalf("v.small dropped: %+v", tr.Dropped)
	}
	removed := 0
	for _, o := range tr.Offerings {
		if o.Removed {
			removed++
			if o.CapacityType != "spot" || o.Reason != (catalog.Reason{Code: catalog.NodePoolPreFilter, Detail: karpv1.CapacityTypeLabelKey}) {
				t.Errorf("removed offering %+v", o)
			}
		}
	}
	if removed != 1 {
		t.Errorf("%d offerings removed by the filter, want the one spot offering", removed)
	}
}

// The first failing key is the one core's Intersects fails on, for every type
// of the AWS snapshot.
func TestExplainRequirementMismatchAgreesWithCore(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"aws": {"us-east-1"}}, "aws")
	its := all(t, c, catalog.Filter{})
	in := func(key string, op corev1.NodeSelectorOperator, vals ...string) *scheduling.Requirement {
		return scheduling.NewRequirement(key, op, vals...)
	}
	for name, reqs := range map[string]scheduling.Requirements{
		"zone ID":    scheduling.NewRequirements(in(v1alpha1.LabelZoneID, corev1.NodeSelectorOpIn, "nowhere")),
		"zone IDs":   scheduling.NewRequirements(in(v1alpha1.LabelZoneID, corev1.NodeSelectorOpIn, "use1-az1", "use1-az2")),
		"no zone ID": scheduling.NewRequirements(in(v1alpha1.LabelZoneID, corev1.NodeSelectorOpDoesNotExist)),
		"not in":     scheduling.NewRequirements(in(v1alpha1.LabelZoneID, corev1.NodeSelectorOpNotIn, "use1-az1")),
		"two keys": scheduling.NewRequirements(in(v1alpha1.LabelZoneID, corev1.NodeSelectorOpIn, "nowhere"),
			in(v1alpha1.LabelVendor, corev1.NodeSelectorOpIn, "aws")),
	} {
		t.Run(name, func(t *testing.T) {
			got := explainWith(t, c, catalog.Query{Requirements: reqs})
			mismatched := 0
			for _, it := range its {
				tr := got[it.Name]
				// Types the pre-filter removes are not asked about.
				if tr.Dropped.Code == catalog.NodePoolPreFilter {
					continue
				}
				coreFails := it.Requirements.Intersects(reqs) != nil
				if coreFails != (tr.Dropped.Code == catalog.NodePoolRequirement) {
					t.Fatalf("%s: core fails %v, reason %+v", it.Name, coreFails, tr.Dropped)
				}
				if coreFails {
					mismatched++
					if _, ok := reqs[tr.Dropped.Detail]; !ok {
						t.Fatalf("%s: the key %q is not one of the NodePool's", it.Name, tr.Dropped.Detail)
					}
				}
			}
			t.Logf("%d of %d types mismatch", mismatched, len(its))
		})
	}
}

func TestExplainReportsTheTypesThatDoNotFitTheRequests(t *testing.T) {
	c := newSynthetic(t, nil, nil)
	cpu := func(n string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(n)}
	}
	// v.small has 2 vCPUs, v.big 16.
	got := explainWith(t, c, catalog.Query{Requests: cpu("3")})
	if got["v.small"].FitsRequests || !got["v.big"].FitsRequests {
		t.Errorf("fits with 3 CPUs: small %v, big %v", got["v.small"].FitsRequests, got["v.big"].FitsRequests)
	}
	got = explainWith(t, c, catalog.Query{})
	if !got["v.small"].FitsRequests {
		t.Error("a query without requests says a type does not fit")
	}
}

// A pinned selection profile that does not rank a type makes its offerings
// Unrankable, and Explain agrees with the types a NodePool that pins it sees.
func TestExplainShowsUnrankableTypesUnderAPinnedProfile(t *testing.T) {
	cl := metricClass()
	c := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws", "gcp")})
	c.SetClasses([]catalog.Class{cl})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	reqs := scheduling.NewRequirements(
		scheduling.NewRequirement(v1alpha1.LabelSelection, corev1.NodeSelectorOpIn, "fast-single-core"),
		scheduling.NewRequirement(v1alpha1.LabelVendor, corev1.NodeSelectorOpIn, "aws"))
	// With the NodePool's requirements the types outside the profile fail on the
	// selection label first (NodePoolRequirement); with only the filter, which the
	// catalog applies, what is left is the offerings the profile cannot price.
	if got := explainWith(t, c, catalog.Query{Requirements: reqs}); len(got) == 0 {
		t.Fatal("empty report")
	} else {
		keyed := 0
		for _, tr := range got {
			if tr.Dropped == (catalog.Reason{Code: catalog.NodePoolRequirement, Detail: v1alpha1.LabelSelection}) {
				keyed++
			}
		}
		if keyed == 0 {
			t.Error("no type fails on the selection label")
		}
	}
	rep := explainWith(t, c, catalog.Query{Filter: catalog.NewFilter(reqs)})
	unrankable := 0
	for _, tr := range rep {
		for _, o := range tr.Offerings {
			if o.Reason.Code == catalog.Unrankable {
				unrankable++
				if o.Available || o.Reason.Detail != "fast-single-core" {
					t.Fatalf("%s: %+v", tr.InstanceType, o)
				}
			}
		}
	}
	if unrankable == 0 {
		t.Fatal("no unrankable offering")
	}
	// What InstanceTypes hands out for the same NodePool is down exactly there.
	for _, it := range all(t, c, catalog.NewFilter(reqs)) {
		for _, o := range it.Offerings {
			want := true
			for _, ro := range rep[it.Name].Offerings {
				if !ro.Removed && ro.Zone == o.Zone() && ro.CapacityType == o.CapacityType() {
					want = ro.Available
				}
			}
			if o.Available != want {
				t.Fatalf("%s %s/%s: available %v, report %v", it.Name, o.Zone(), o.CapacityType(), o.Available, want)
			}
		}
	}
}

func TestNodePoolReasonsAreLoggedOncePerGenerationAndNodePool(t *testing.T) {
	l := newLogCatalog(t, synthetic())
	pool := func(zoneID string) scheduling.Requirements {
		return scheduling.NewRequirements(scheduling.NewRequirement(v1alpha1.LabelZoneID, corev1.NodeSelectorOpIn, zoneID))
	}
	l.LogNodePool(l.ctx, "c", "bench", pool("z9"))
	lines := l.take()
	if len(lines) != 1 || !strings.Contains(lines[0], `"nodepool"="bench"`) || !strings.Contains(lines[0], `"reason"="NodePoolRequirement"`) ||
		!strings.Contains(lines[0], "v.small") || !strings.Contains(lines[0], "v.big") {
		t.Fatalf("first report: %q", lines)
	}
	l.LogNodePool(l.ctx, "c", "bench", pool("z9"))
	if lines := l.take(); len(lines) != 0 {
		t.Fatalf("again, same generation: %q", lines)
	}
	// Another NodePool is a report of its own, and a matching one has nothing to say.
	l.LogNodePool(l.ctx, "c", "other", pool("z1"))
	if lines := l.take(); len(lines) != 0 {
		t.Fatalf("a NodePool every type matches: %q", lines)
	}
	l.un.MarkInsufficientCapacity("v", "v.small", "zone-1", "on-demand")
	l.LogNodePool(l.ctx, "c", "bench", pool("z9"))
	if lines := l.take(); len(lines) != 0 {
		t.Fatalf("the class-level reason of a new generation is not repeated per NodePool: %q", lines)
	}
}
