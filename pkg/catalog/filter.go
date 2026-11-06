/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/sc-keeper-go/selection/api"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// Filter is the pre-filter of GetInstanceTypes (overview section 4): the
// NodePool's requirements on vendor, region, zone, architecture, capacity type
// and instance type. Dropping what they exclude is safe, since core would drop
// it anyway; it keeps the lists small at scale (overview section 5.7).
//
// A requirement matches by Requirement.Has, so In, NotIn, Exists and DoesNotExist
// all work. The zero Filter keeps everything.
type Filter struct {
	vendor, region, zone, arch, capacityType, instanceType *scheduling.Requirement
	// selection is the single selection profile the requirements pin, or "".
	selection string
}

// filterKeys are the requirement keys the pre-filter reads.
var filterKeys = []string{
	v1alpha1.LabelVendor, corev1.LabelTopologyRegion, corev1.LabelTopologyZone, corev1.LabelArchStable,
	karpv1.CapacityTypeLabelKey, corev1.LabelInstanceTypeStable,
}

// NewFilter builds the pre-filter from a NodePool's requirements (for example
// scheduling.NewNodeSelectorRequirementsWithMinValues of the template's).
func NewFilter(reqs scheduling.Requirements) Filter {
	get := func(key string) *scheduling.Requirement {
		if reqs.Has(key) {
			return reqs.Get(key)
		}
		return nil
	}
	f := Filter{
		vendor: get(v1alpha1.LabelVendor), region: get(corev1.LabelTopologyRegion),
		zone: get(corev1.LabelTopologyZone), arch: get(corev1.LabelArchStable),
		capacityType: get(karpv1.CapacityTypeLabelKey), instanceType: get(corev1.LabelInstanceTypeStable),
	}
	// A NodePool that pins exactly one selection profile is priced by its
	// objective (00a section 7.3).
	if sel := get(v1alpha1.LabelSelection); sel != nil && sel.Operator() == corev1.NodeSelectorOpIn && sel.Len() == 1 {
		f.selection = sel.Values()[0]
	}
	return f
}

func (f Filter) allowsVendor(v string) bool {
	return f.vendor == nil || f.vendor.Has(v)
}

func (f Filter) allowsType(name, arch string) bool {
	return (f.instanceType == nil || f.instanceType.Has(name)) && (f.arch == nil || f.arch.Has(arch))
}

func (f Filter) allowsOffering(region, zone, capacityType string) bool {
	return (f.region == nil || f.region.Has(region)) &&
		(f.zone == nil || f.zone.Has(zone)) &&
		(f.capacityType == nil || f.capacityType.Has(capacityType))
}

// key is a canonical string of the filter, for cache keys.
func (f Filter) key() string {
	var b strings.Builder
	for i, r := range []*scheduling.Requirement{f.vendor, f.region, f.zone, f.arch, f.capacityType, f.instanceType} {
		b.WriteString(filterKeys[i])
		b.WriteByte('=')
		if r != nil {
			b.WriteString(string(r.Operator()))
			vals := r.Values()
			slices.Sort(vals)
			b.WriteString(strings.Join(vals, ","))
		}
		b.WriteByte(';')
	}
	b.WriteString("selection=" + f.selection)
	return b.String()
}

// pricedBy is how a pinned selection profile reprices offerings.
type pricedBy struct {
	name string
	sel  *selectionView
}

// effectivePrice prices an offering by the profile's objective so that the
// cheapest offering is the best by the objective (00a section 7.3):
// price / score for a higher-is-better ScorePerPrice, price * score for a
// lower-is-better one, 1 / score and score for the Score objective. ok is false
// when the type has no score.
func (p pricedBy) effectivePrice(key model.Key, usd float64) (float64, bool) {
	score, ok := p.sel.scores[key]
	if !ok || score <= 0 {
		return 0, false
	}
	switch p.sel.objective {
	case api.ObjectiveScorePerPrice:
		if p.sel.lowerIsBetter {
			return usd * score, true
		}
		return usd / score, true
	default: // Score
		if p.sel.lowerIsBetter {
			return score, true
		}
		return 1 / score, true
	}
}
