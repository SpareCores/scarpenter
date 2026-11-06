/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
)

// podReqs builds the scheduling requirements of a pod whose node affinity has
// one expression per entry, as core does for pending pods.
func podReqs(exprs ...corev1.NodeSelectorRequirement) scheduling.Requirements {
	return scheduling.NewPodRequirements(&corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: exprs}},
		}},
	}}})
}

func expr(key string, op corev1.NodeSelectorOperator, values ...string) corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{Key: key, Operator: op, Values: values}
}

// typeCompatible is core's check of an instance type against the requirements
// of a pod or NodeClaim: Intersects, which ignores keys that only one side
// defines (compatible() in pkg/controllers/provisioning/scheduling/nodeclaim.go
// of core v1.15.1). Offerings are checked separately, with
// IsCompatible(..., AllowUndefinedWellKnownLabels), which also rejects an
// offering that defines a key core does not know.
func typeCompatible(it *cloudprovider.InstanceType, reqs scheduling.Requirements) bool {
	return it.Requirements.Intersects(reqs) == nil
}

// TestQ11GtLtAndDoesNotExist verifies overview 15 Q11 with core's own
// scheduling requirements: a pod's Gt and Lt match the numeric requirements of
// instance types and of offerings, and DoesNotExist on a type excludes it for
// pods that require the key.
func TestQ11GtLtAndDoesNotExist(t *testing.T) {
	its := metricCatalog(t)

	m7i := byName(its, "aws.m7i.large")    // 2 vCPUs, benchmarked
	p4d := byName(its, "aws.p4d.24xlarge") // 96 vCPUs, 8 GPUs
	single, _ := intValue(t, m7i, v1alpha1.MetricLabel("single-core"))
	if m7i == nil || p4d == nil || single == 0 {
		t.Fatal("test types are missing")
	}
	var unbenchmarked *cloudprovider.InstanceType
	for _, it := range its {
		if _, ok := intValue(t, it, v1alpha1.MetricLabel("single-core")); !ok && it.Requirements.Get(v1alpha1.LabelVendor).Has("aws") {
			unbenchmarked = it
			break
		}
	}
	if unbenchmarked == nil {
		t.Fatal("every type is benchmarked")
	}

	numeric := []struct {
		name string
		e    corev1.NodeSelectorRequirement
		it   *cloudprovider.InstanceType
		want bool
	}{
		{"vcpus Gt 1 matches 2", expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "1"), m7i, true},
		{"vcpus Gt 2 does not match 2", expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "2"), m7i, false},
		{"vcpus Lt 3 matches 2", expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpLt, "3"), m7i, true},
		{"vcpus Lt 2 does not match 2", expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpLt, "2"), m7i, false},
		{"vcpus Gt 7 does not match 2", expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "7"), m7i, false},
		{"vcpus Gt 7 matches 96", expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "7"), p4d, true},
		{"memory-mib Gt 8191 matches 8192", expr(v1alpha1.LabelMemoryMiB, corev1.NodeSelectorOpGt, "8191"), m7i, true},
		{"cpu-clock-mhz Gt 3199 matches 3200", expr(v1alpha1.LabelCPUClockMHz, corev1.NodeSelectorOpGt, "3199"), m7i, true},
		{"metric score Gt below matches", expr(v1alpha1.MetricLabel("single-core"), corev1.NodeSelectorOpGt, itoa(single-1)), m7i, true},
		{"metric score Gt itself does not match", expr(v1alpha1.MetricLabel("single-core"), corev1.NodeSelectorOpGt, itoa(single)), m7i, false},
		{"metric percentile Lt 101 matches", expr(v1alpha1.MetricPercentileLabel("single-core"), corev1.NodeSelectorOpLt, "101"), m7i, true},
		// 2 to 16 vCPUs, the example of 00a section 7.1.
		{"2-16 vcpus excludes 96", expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpLt, "17"), p4d, false},
		// accelerator-count is DoesNotExist on types without accelerators.
		{"accelerator-count Gt 0 excludes a type without GPUs", expr(v1alpha1.LabelAcceleratorCount, corev1.NodeSelectorOpGt, "0"), m7i, false},
		{"accelerator-count Gt 0 matches 8 GPUs", expr(v1alpha1.LabelAcceleratorCount, corev1.NodeSelectorOpGt, "0"), p4d, true},
		{"accelerator-count Exists excludes a type without GPUs", expr(v1alpha1.LabelAcceleratorCount, corev1.NodeSelectorOpExists), m7i, false},
		{"accelerator-model In L4 excludes a type without GPUs", expr(v1alpha1.LabelAcceleratorModel, corev1.NodeSelectorOpIn, "L4"), m7i, false},
		// DoesNotExist semantics for unknown values: any threshold excludes
		// unbenchmarked types.
		{"metric Gt 0 excludes an unbenchmarked type", expr(v1alpha1.MetricLabel("single-core"), corev1.NodeSelectorOpGt, "0"), unbenchmarked, false},
		{"metric percentile Gt 89 excludes an unbenchmarked type", expr(v1alpha1.MetricPercentileLabel("single-core"), corev1.NodeSelectorOpGt, "89"), unbenchmarked, false},
		{"metric Exists excludes an unbenchmarked type", expr(v1alpha1.MetricLabel("single-core"), corev1.NodeSelectorOpExists), unbenchmarked, false},
		// ... and a pod that wants the key to be absent matches it.
		{"metric DoesNotExist matches an unbenchmarked type", expr(v1alpha1.MetricLabel("single-core"), corev1.NodeSelectorOpDoesNotExist), unbenchmarked, true},
		{"metric DoesNotExist excludes a benchmarked type", expr(v1alpha1.MetricLabel("single-core"), corev1.NodeSelectorOpDoesNotExist), m7i, false},
		// selection is DoesNotExist for non-members.
		{"selection In a profile excludes a non-member", expr(v1alpha1.LabelSelection, corev1.NodeSelectorOpIn, "fast-single-core"), unbenchmarked, false},
		// The default profile is on every type.
		{"storage-profile In default matches", expr(v1alpha1.LabelStorageProfile, corev1.NodeSelectorOpIn, "default"), m7i, true},
		{"storage-profile In db-io2 excludes (no such profile)", expr(v1alpha1.LabelStorageProfile, corev1.NodeSelectorOpIn, "db-io2"), m7i, false},
		// A boolean flag.
		{"cpu-flag-avx512f In true matches m7i", expr(v1alpha1.CPUFlagLabel("avx512f"), corev1.NodeSelectorOpIn, "true"), m7i, true},
	}
	for _, tc := range numeric {
		t.Run("type: "+tc.name, func(t *testing.T) {
			if got := typeCompatible(tc.it, podReqs(tc.e)); got != tc.want {
				t.Errorf("compatible = %v, want %v (requirements: %v)", got, tc.want, reqLines(tc.it.Requirements))
			}
		})
	}

	// The same on offerings: per-offering requirements are checked by core
	// through Offerings.HasCompatible.
	t.Run("offering: per-USD Gt and Lt", func(t *testing.T) {
		o := m7i.Offerings[0]
		perUSD, ok := intValue(t, &cloudprovider.InstanceType{Requirements: o.Requirements}, v1alpha1.MetricPerUSDLabel("single-core"))
		if !ok || perUSD == 0 {
			t.Fatalf("no per-USD label on %v", reqLines(o.Requirements))
		}
		for _, tc := range []struct {
			e    corev1.NodeSelectorRequirement
			want bool
		}{
			{expr(v1alpha1.MetricPerUSDLabel("single-core"), corev1.NodeSelectorOpGt, itoa(perUSD-1)), true},
			{expr(v1alpha1.MetricPerUSDLabel("single-core"), corev1.NodeSelectorOpGt, itoa(perUSD)), false},
			{expr(v1alpha1.MetricPerUSDLabel("single-core"), corev1.NodeSelectorOpLt, itoa(perUSD+1)), true},
			{expr(v1alpha1.MetricPerUSDLabel("single-core"), corev1.NodeSelectorOpLt, itoa(perUSD)), false},
		} {
			if got := (cloudprovider.Offerings{o}).HasCompatible(podReqs(tc.e)); got != tc.want {
				t.Errorf("%s %s %v: compatible = %v, want %v", tc.e.Key, tc.e.Operator, tc.e.Values, got, tc.want)
			}
		}
	})
	t.Run("offering: DoesNotExist excludes unbenchmarked types", func(t *testing.T) {
		// A pod that needs a score per dollar finds no compatible offering on a
		// type without a score.
		e := expr(v1alpha1.MetricPerUSDLabel("single-core"), corev1.NodeSelectorOpGt, "0")
		if unbenchmarked.Offerings.HasCompatible(podReqs(e)) {
			t.Error("an offering of an unbenchmarked type is compatible with a per-USD threshold")
		}
		// Through core's filter on whole types, with offerings considered.
		filtered := cloudprovider.InstanceTypes(its).Compatible(podReqs(e))
		if len(filtered) == 0 || len(filtered) == len(its) {
			t.Errorf("%d of %d types are compatible with a per-USD threshold", len(filtered), len(its))
		}
		if byName(filtered, unbenchmarked.Name) != nil {
			t.Error("an unbenchmarked type passes core's filter")
		}
	})
	t.Run("offering: location labels", func(t *testing.T) {
		syn := newSynthetic(t, nil, nil)
		offs := byName(all(t, syn, catalog.Filter{}), "v.small").Offerings
		for _, tc := range []struct {
			e    corev1.NodeSelectorRequirement
			want bool
		}{
			{expr(v1alpha1.LabelCountry, corev1.NodeSelectorOpIn, "de"), true},
			{expr(v1alpha1.LabelCountry, corev1.NodeSelectorOpIn, "us"), false},
			{expr(v1alpha1.LabelContinent, corev1.NodeSelectorOpIn, "europe"), true},
			{expr(v1alpha1.LabelCity, corev1.NodeSelectorOpIn, "frankfurt"), true},
			// Unknown state and green energy are DoesNotExist: a pod that
			// requires a value finds nothing.
			{expr(v1alpha1.LabelState, corev1.NodeSelectorOpIn, "hesse"), false},
			{expr(v1alpha1.LabelGreenEnergy, corev1.NodeSelectorOpIn, "true"), false},
			{expr(v1alpha1.LabelGreenEnergy, corev1.NodeSelectorOpDoesNotExist), true},
		} {
			if got := offs.HasCompatible(podReqs(tc.e)); got != tc.want {
				t.Errorf("%s %s %v: compatible = %v, want %v", tc.e.Key, tc.e.Operator, tc.e.Values, got, tc.want)
			}
		}
	})
}

// TestUndefinedKeysMatchEverything documents why every key is defined: an
// instance type that does not define a well-known key matches any value of it.
func TestUndefinedKeysMatchEverything(t *testing.T) {
	undefined := &cloudprovider.InstanceType{Name: "x", Requirements: scheduling.NewRequirements()}
	for _, e := range []corev1.NodeSelectorRequirement{
		expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "1000"),
		expr(v1alpha1.LabelAcceleratorModel, corev1.NodeSelectorOpIn, "H100"),
	} {
		if !typeCompatible(undefined, podReqs(e)) {
			t.Errorf("an undefined %s unexpectedly excludes the type", e.Key)
		}
	}
	// With the key defined as DoesNotExist the same pods find nothing.
	defined := &cloudprovider.InstanceType{Name: "y", Requirements: scheduling.NewRequirements(
		labels.Absent(v1alpha1.LabelVCPUs), labels.Absent(v1alpha1.LabelAcceleratorModel))}
	for _, e := range []corev1.NodeSelectorRequirement{
		expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "1000"),
		expr(v1alpha1.LabelAcceleratorModel, corev1.NodeSelectorOpIn, "H100"),
	} {
		if typeCompatible(defined, podReqs(e)) {
			t.Errorf("a DoesNotExist %s does not exclude the type", e.Key)
		}
	}
}

// TestUnregisteredKeysFollowCoresRealChecks shows why only registered keys are
// stamped: core's IsCompatible on offerings rejects an unknown key, and its
// Intersects on types ignores it.
func TestUnregisteredKeysFollowCoresRealChecks(t *testing.T) {
	const key = "scarpenter.sparecores.com/never-registered"
	pod := podReqs(expr(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "0"))
	it := &cloudprovider.InstanceType{Name: "x", Requirements: scheduling.NewRequirements(labels.In(key, "5"))}
	if !typeCompatible(it, pod) {
		t.Error("an unregistered type-level key must be ignored by Intersects")
	}
	off := cloudprovider.Offerings{{Requirements: scheduling.NewRequirements(labels.In(key, "5")), Available: true}}
	if off.HasCompatible(pod) {
		t.Error("an offering with an unregistered key must be rejected by IsCompatible")
	}
}
