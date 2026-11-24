/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"cmp"
	"slices"
)

// MaxSampleTypes is the number of type names a ReasonCount carries.
const MaxSampleTypes = 5

// Summary counts what a class offers of one vendor (plan 28): the status block,
// the metrics and the ConfigMap header come from it.
type Summary struct {
	// Types are the offered types, Offerings their offerings (available or
	// not) and AvailableOfferings those that are available.
	Types, Offerings, AvailableOfferings int
	// Dropped counts the types that are not offered, per reason (NotLaunchable
	// too, when it is type-level). Unavailable
	// counts the offerings of the offered types that are not available, per
	// reason; the removed ones (ZoneNotAvailable, IncompletePrice,
	// NotLaunchable) are counted here too. Both are sorted by code.
	Dropped, Unavailable []ReasonCount
}

// ReasonCount is the number of types or offerings with a reason, and the first
// MaxSampleTypes instance type names (in name order) they belong to.
type ReasonCount struct {
	Code  Code
	Count int
	Types []string
}

// Summaries returns the summary of every vendor of the report, by vendor.
func (r Report) Summaries() map[string]Summary {
	add := func(c map[Code]*ReasonCount, code Code, name string) {
		rc := c[code]
		if rc == nil {
			rc = &ReasonCount{Code: code}
			c[code] = rc
		}
		rc.Count++
		if len(rc.Types) < MaxSampleTypes && !slices.Contains(rc.Types, name) {
			rc.Types = append(rc.Types, name)
		}
	}
	dropped, unavailable := map[string]map[Code]*ReasonCount{}, map[string]map[Code]*ReasonCount{}
	out := map[string]Summary{}
	for _, t := range r.Types {
		s := out[t.Vendor]
		if dropped[t.Vendor] == nil {
			dropped[t.Vendor], unavailable[t.Vendor] = map[Code]*ReasonCount{}, map[Code]*ReasonCount{}
		}
		if t.Dropped.Code != "" {
			add(dropped[t.Vendor], t.Dropped.Code, t.InstanceType)
			out[t.Vendor] = s
			continue
		}
		s.Types++
		for _, o := range t.Offerings {
			s.Offerings++
			if o.Available {
				s.AvailableOfferings++
			} else {
				add(unavailable[t.Vendor], o.Reason.Code, t.InstanceType)
			}
		}
		out[t.Vendor] = s
	}
	for vendor, s := range out {
		s.Dropped, s.Unavailable = sortedCounts(dropped[vendor]), sortedCounts(unavailable[vendor])
		out[vendor] = s
	}
	return out
}

func sortedCounts(m map[Code]*ReasonCount) []ReasonCount {
	out := make([]ReasonCount, 0, len(m))
	for _, rc := range m {
		out = append(out, *rc)
	}
	slices.SortFunc(out, func(a, b ReasonCount) int { return cmp.Compare(a.Code, b.Code) })
	return out
}
