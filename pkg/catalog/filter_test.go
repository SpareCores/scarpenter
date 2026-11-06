/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
)

// coreFilter is how core narrows instance types for a set of requirements: the
// type's requirements must be compatible, and a compatible, available offering
// must exist (pkg/controllers/provisioning/scheduling/nodeclaim.go). It returns,
// per type, the compatible available offerings.
func coreFilter(its []*cloudprovider.InstanceType, reqs scheduling.Requirements) map[string][]string {
	out := map[string][]string{}
	for _, it := range its {
		if it.Requirements.Intersects(reqs) != nil {
			continue
		}
		offs := it.Offerings.Available().Compatible(reqs)
		if len(offs) == 0 {
			continue
		}
		var ids []string
		for _, o := range offs {
			ids = append(ids, fmt.Sprintf("%s/%s/%.6f", o.Zone(), o.CapacityType(), o.Price))
		}
		slices.Sort(ids)
		out[it.Name] = ids
	}
	return out
}

func req(key string, op corev1.NodeSelectorOperator, values ...string) *scheduling.Requirement {
	return scheduling.NewRequirement(key, op, values...)
}

// TestPreFilterEqualsCoresFiltering proves the pre-filter safe (overview
// section 4): for every set of NodePool requirements, what core keeps of the
// pre-filtered list equals what it keeps of the unfiltered one, offering by
// offering.
func TestPreFilterEqualsCoresFiltering(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"aws": {"us-east-1", "eu-central-1"}}, "aws")
	unfiltered := all(t, c, catalog.Filter{})
	total := len(unfiltered)
	zone := unfiltered[0].Offerings[0].Zone()

	In, NotIn := corev1.NodeSelectorOpIn, corev1.NodeSelectorOpNotIn
	cases := []struct {
		name string
		reqs []*scheduling.Requirement
		// smaller says the filter must drop something.
		smaller bool
	}{
		{"no requirements", nil, false},
		{"arch arm64", []*scheduling.Requirement{req(corev1.LabelArchStable, In, "arm64")}, true},
		{"arch not arm64", []*scheduling.Requirement{req(corev1.LabelArchStable, NotIn, "arm64")}, true},
		{"spot only", []*scheduling.Requirement{req(karpv1.CapacityTypeLabelKey, In, "spot")}, false},
		{"on-demand or spot", []*scheduling.Requirement{req(karpv1.CapacityTypeLabelKey, In, "spot", "on-demand")}, false},
		{"region eu-central-1", []*scheduling.Requirement{req(corev1.LabelTopologyRegion, In, "eu-central-1")}, false},
		{"not region us-east-1", []*scheduling.Requirement{req(corev1.LabelTopologyRegion, NotIn, "us-east-1")}, false},
		{"one zone", []*scheduling.Requirement{req(corev1.LabelTopologyZone, In, zone)}, false},
		{"unknown zone", []*scheduling.Requirement{req(corev1.LabelTopologyZone, In, "mars-1a")}, true},
		{"unknown region", []*scheduling.Requirement{req(corev1.LabelTopologyRegion, In, "mars-1")}, true},
		{"three instance types", []*scheduling.Requirement{req(corev1.LabelInstanceTypeStable, In, "aws.m7i.large", "aws.c7g.xlarge", "aws.t3a.small", "aws.nope")}, true},
		{"not one instance type", []*scheduling.Requirement{req(corev1.LabelInstanceTypeStable, NotIn, "aws.m7i.large")}, true},
		{"vendor aws", []*scheduling.Requirement{req(v1alpha1.LabelVendor, In, "aws")}, false},
		{"vendor gcp", []*scheduling.Requirement{req(v1alpha1.LabelVendor, In, "gcp")}, true},
		{"vendor not aws", []*scheduling.Requirement{req(v1alpha1.LabelVendor, NotIn, "aws")}, true},
		{"exists", []*scheduling.Requirement{req(corev1.LabelArchStable, corev1.NodeSelectorOpExists)}, false},
		{"does not exist", []*scheduling.Requirement{req(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpDoesNotExist)}, true},
		{"combination", []*scheduling.Requirement{
			req(corev1.LabelArchStable, In, "amd64"), req(karpv1.CapacityTypeLabelKey, In, "on-demand"),
			req(corev1.LabelTopologyRegion, In, "us-east-1"), req(corev1.LabelInstanceTypeStable, NotIn, "aws.m7i.large"),
		}, true},
		{"requirements the pre-filter does not read", []*scheduling.Requirement{
			req(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "7"), req(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpLt, "33"),
			req(v1alpha1.LabelFamily, In, "m7i", "c7g"), req(v1alpha1.LabelStorageProfile, In, v1alpha1.DefaultProfile),
		}, false},
		{"pre-filtered and other requirements", []*scheduling.Requirement{
			req(corev1.LabelArchStable, In, "arm64"), req(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "3"),
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs := scheduling.NewRequirements(tc.reqs...)
			filtered := all(t, c, catalog.NewFilter(reqs))
			if tc.smaller && len(filtered) >= total {
				t.Errorf("the pre-filter kept all %d types", total)
			}
			want, got := coreFilter(unfiltered, reqs), coreFilter(filtered, reqs)
			if len(want) != len(got) {
				t.Fatalf("core keeps %d types of the unfiltered list and %d of the pre-filtered one", len(want), len(got))
			}
			for name, offs := range want {
				if !slices.Equal(offs, got[name]) {
					t.Errorf("%s: offerings differ:\n unfiltered: %v\n filtered:   %v", name, offs, got[name])
				}
			}
			// The pre-filter never adds anything.
			for _, it := range filtered {
				if byName(unfiltered, it.Name) == nil {
					t.Errorf("%s is not in the unfiltered list", it.Name)
				}
			}
		})
	}
}

func TestPreFilterRemovesOfferingsOfExcludedValues(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"aws": {"us-east-1", "eu-central-1"}}, "aws")
	f := catalog.NewFilter(scheduling.NewRequirements(
		req(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "eu-central-1"),
		req(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "spot"),
	))
	its := all(t, c, f)
	if len(its) < 500 {
		t.Fatalf("only %d types", len(its))
	}
	for _, it := range its {
		for _, o := range it.Offerings {
			if !o.Requirements.Get(corev1.LabelTopologyRegion).Has("eu-central-1") || !o.Requirements.Get(karpv1.CapacityTypeLabelKey).Has("spot") {
				t.Fatalf("%s kept an offering outside the filter: %v", it.Name, reqLines(o.Requirements))
			}
		}
	}
}

func TestFilterVendorsLimitTheScope(t *testing.T) {
	c := newSynthetic(t, nil, nil)
	if its := all(t, c, catalog.NewFilter(scheduling.NewRequirements(req(v1alpha1.LabelVendor, corev1.NodeSelectorOpIn, "aws")))); len(its) != 0 {
		t.Errorf("a NodePool that allows another vendor got %d types", len(its))
	}
	if its := all(t, c, catalog.NewFilter(scheduling.NewRequirements(req(v1alpha1.LabelVendor, corev1.NodeSelectorOpIn, "v", "aws")))); len(its) != 2 {
		t.Errorf("got %d types", len(its))
	}
}
