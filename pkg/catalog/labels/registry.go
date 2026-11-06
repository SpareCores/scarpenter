/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package labels

import (
	"slices"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// Registration with Karpenter core. Core reads karpv1.WellKnownLabels when its
// controllers start (node metrics take their label names from it) and the set
// is an unlocked global map, so keys are never registered at runtime: the
// operator registers every key it knows before WithCoreControllers (the fixed
// keys in the API package's init, the rest through RegisterKeys), and a
// NodeClass that later defines a metric not registered at start gets a
// RestartRequired condition naming MissingKeys.

// FlagKeys returns the cpu-flag label keys of every curated flag: all of them
// are registered, whichever the NodeClasses enable.
func FlagKeys() []string {
	out := make([]string, len(v1alpha1.CPUFlagLabelValues))
	for i, f := range v1alpha1.CPUFlagLabelValues {
		out[i] = v1alpha1.CPUFlagLabel(f)
	}
	return out
}

// MetricLabelKeys returns every label key of the metrics: score, percentile and
// per-USD.
func MetricLabelKeys(names []string) []string {
	out := make([]string, 0, 3*len(names))
	for _, n := range names {
		out = append(out, v1alpha1.MetricLabel(n), v1alpha1.MetricPercentileLabel(n), v1alpha1.MetricPerUSDLabel(n))
	}
	return out
}

// RegisterKeys adds the keys to core's well-known labels, and returns the keys
// that were not registered before. It must run before core's controllers are
// built and never afterwards: the set is not synchronised.
func RegisterKeys(keys ...string) (added []string) {
	for _, k := range keys {
		if !karpv1.WellKnownLabels.Has(k) {
			added = append(added, k)
		}
	}
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Insert(keys...)
	return added
}

// MissingKeys returns the metric label keys of the names that are not
// registered with core, sorted. Reading the set is safe at any time as long as
// nobody registers concurrently, which only happens before start-up.
func MissingKeys(metricNames []string) []string {
	var missing []string
	for _, k := range MetricLabelKeys(metricNames) {
		if !karpv1.WellKnownLabels.Has(k) {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	return missing
}

// RegisteredFlags returns the flags whose cpu-flag label key is registered
// with core, in the order given.
func RegisteredFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if karpv1.WellKnownLabels.Has(v1alpha1.CPUFlagLabel(f)) {
			out = append(out, f)
		}
	}
	return out
}

// RegisteredMetrics returns the metrics whose three label keys are all
// registered with core, in the order given.
func RegisteredMetrics(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if len(MissingKeys([]string{n})) == 0 {
			out = append(out, n)
		}
	}
	return out
}
