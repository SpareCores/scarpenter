/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package labels

import (
	"math"
	"slices"

	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// Metric is the evaluation side of a NodeClass metric (00a section 7.2).
type Metric struct {
	Name string
	// Scale multiplies the score before it is rounded to the label's integer.
	Scale float64
	// PerVCPU divides the score by the vCPU count first.
	PerVCPU bool
	// LowerIsBetter flips the percentile and the per-USD value.
	LowerIsBetter bool
}

// MetricValues are the values of one metric over a set of servers.
type MetricValues struct {
	Metric Metric
	// Score is the (normalised) score before scaling.
	Score map[model.Key]float64
	// Percentile is 0 to 100, 100 best (also for lower-is-better benchmarks).
	Percentile map[model.Key]int
}

// ComputeMetric normalises the raw scores of the servers and ranks them.
// Servers are the types of all the NodeClass's vendors: the percentile is
// across them. raw is keyed by server; servers without a score are absent from
// the result.
func ComputeMetric(m Metric, servers []*model.Server, raw map[model.Key]float64) *MetricValues {
	if m.Scale <= 0 {
		m.Scale = 1
	}
	v := &MetricValues{Metric: m, Score: map[model.Key]float64{}, Percentile: map[model.Key]int{}}
	type scored struct {
		key   model.Key
		score float64
	}
	var all []scored
	for _, s := range servers {
		r, ok := raw[s.Key()]
		if !ok || math.IsNaN(r) || math.IsInf(r, 0) {
			continue
		}
		if m.PerVCPU {
			if s.VCPUs <= 0 {
				continue
			}
			r /= float64(s.VCPUs)
		}
		v.Score[s.Key()] = r
		all = append(all, scored{s.Key(), r})
	}
	// Worst first: a type's percentile is the share of the other types it beats
	// (ties share the lower rank).
	slices.SortFunc(all, func(a, b scored) int {
		switch {
		case a.score == b.score:
			return 0
		case (a.score < b.score) != m.LowerIsBetter:
			return -1
		}
		return 1
	})
	n := len(all)
	for i := 0; i < n; {
		j := i
		for j < n && all[j].score == all[i].score {
			j++
		}
		pct := 100
		if n > 1 {
			pct = int(math.Round(100 * float64(i) / float64(n-1)))
		}
		for _, s := range all[i:j] {
			v.Percentile[s.key] = pct
		}
		i = j
	}
	return v
}

// Label returns the integer value of the score label.
func (v *MetricValues) Label(key model.Key) (int64, bool) {
	s, ok := v.Score[key]
	if !ok {
		return 0, false
	}
	return int64(math.Round(s * v.Metric.Scale)), true
}

// TypeRequirements returns metric-<m> and metric-<m>-pct for a server: the
// score and the percentile, DoesNotExist for an unbenchmarked type.
func (v *MetricValues) TypeRequirements(key model.Key) []*scheduling.Requirement {
	score, pct := v1alpha1.MetricLabel(v.Metric.Name), v1alpha1.MetricPercentileLabel(v.Metric.Name)
	label, ok := v.Label(key)
	if !ok {
		return []*scheduling.Requirement{Absent(score), Absent(pct)}
	}
	return []*scheduling.Requirement{
		Int(score, float64(label)),
		Int(pct, float64(v.Percentile[key])),
	}
}

// PerUSDRequirement returns metric-<m>-per-usd for an offering: the scaled
// score per USD/hour. It is DoesNotExist for an unbenchmarked type, and for a
// lower-is-better metric, where a score per dollar means nothing.
func (v *MetricValues) PerUSDRequirement(key model.Key, usdPerHour float64) *scheduling.Requirement {
	name := v1alpha1.MetricPerUSDLabel(v.Metric.Name)
	label, ok := v.Label(key)
	if !ok || usdPerHour <= 0 || v.Metric.LowerIsBetter {
		return Absent(name)
	}
	return Int(name, float64(label)/usdPerHour)
}

// MetricKeys returns the type-level and offering-level label keys of a metric.
func MetricKeys(name string) (typeKeys, offeringKeys []string) {
	return []string{v1alpha1.MetricLabel(name), v1alpha1.MetricPercentileLabel(name)},
		[]string{v1alpha1.MetricPerUSDLabel(name)}
}

// AbsentMetric returns the requirements of a metric for a type that the
// metric could not be computed for at all (the benchmark query failed).
func AbsentMetric(name string) []*scheduling.Requirement {
	return []*scheduling.Requirement{
		Absent(v1alpha1.MetricLabel(name)), Absent(v1alpha1.MetricPercentileLabel(name)),
	}
}
