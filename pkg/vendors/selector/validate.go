/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selector

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// orderCreated is the orderBy that is the creation time.
const orderCreated = "created"

// ImagesKind is the kind in which {arch} and {vendorArch} are known: a launch
// asks for an image of an architecture.
const ImagesKind vendors.ResourceKind = "images"

// FilterAllowed reports whether name is accepted under `filters`: an exact
// name, or a pattern that ends in * with a non-empty rest.
func FilterAllowed(caps vendors.SelectorCaps, name string) (vendors.FilterCap, bool) {
	for _, f := range caps.Filters {
		prefix, pattern := strings.CutSuffix(f.Name, "*")
		// A pattern needs something for its *: "tag:" alone is no filter.
		if f.Name == name || (pattern && len(name) > len(prefix) && strings.HasPrefix(name, prefix)) {
			return f, true
		}
	}
	return vendors.FilterCap{}, false
}

// nameList lists up to max names for an error message.
func nameList(names []string, max int) string {
	slices.Sort(names)
	if len(names) > max {
		return strings.Join(names[:max], ", ") + ", ..."
	}
	return strings.Join(names, ", ")
}

// Validate checks a `use` block against the driver's declaration for the kind
// and returns every problem, naming the vendor, kind, field and the supported
// names. A block that selects nothing (empty, or emptied by pruning an old
// field) is an error. path is the field path, for example
// vendors.aws.resources.subnets.use.
func Validate(
	vendor string, kind vendors.ResourceKind, path string, u *common.ResourceUse, caps vendors.SelectorCaps,
) []error {
	if u == nil {
		return nil
	}
	v := &validator{vendor: vendor, kind: kind, path: path, caps: caps}
	if !Selects(u) {
		v.bad("selects nothing: set at least one of ids, name, tags, match, filters and expression " +
			"(an old field the API server pruned leaves an empty use, which is never \"the default\")")
		return v.errs
	}
	v.fields(u)
	if u.Match != nil {
		v.match(u.Match)
	}
	v.filters(u.Filters)
	v.order(u)
	strs(u, v.placeholders)
	return v.errs
}

type validator struct {
	vendor string
	kind   vendors.ResourceKind
	path   string
	caps   vendors.SelectorCaps
	errs   []error
}

func (v *validator) bad(format string, a ...any) {
	v.errs = append(v.errs, fmt.Errorf("%s: %s", v.path, fmt.Sprintf(format, a...)))
}

// fields checks the fields that are supported or not as a whole.
func (v *validator) fields(u *common.ResourceUse) {
	if u.Name != "" && v.caps.Name == nil {
		v.bad("name is not supported for %s of %s (it has no name field): use ids, tags or the vendor's filters",
			v.kind, v.vendor)
	}
	if len(u.Tags) > 0 && v.caps.Tags == vendors.TagsNone {
		v.bad("tags are not supported for %s of %s (it has no tags or labels)", v.kind, v.vendor)
	}
	if u.Expression != "" && v.caps.Expression == "" {
		v.bad("expression is not supported for %s of %s (the vendor has no query language for it)", v.kind, v.vendor)
	}
}

// match checks the attributes of match against the declaration.
func (v *validator) match(m *common.ResourceMatch) {
	for _, a := range setAttributes(m) {
		if _, ok := v.caps.Match[a]; ok {
			continue
		}
		switch {
		case a == vendors.MatchZone && !v.caps.Zonal:
			v.bad("match.zone is not supported: %s of %s is not zonal", v.kind, v.vendor)
		case (a == vendors.MatchCreatedAfter || a == vendors.MatchCreatedBefore) && !v.caps.HasCreated:
			v.bad("match.%s is not supported: %s of %s has no creation time", a, v.kind, v.vendor)
		default:
			names := make([]string, 0, len(v.caps.Match))
			for k := range v.caps.Match {
				names = append(names, k)
			}
			v.bad("match.%s is not supported for %s of %s (supported: %s)", a, v.kind, v.vendor, nameList(names, 20))
		}
	}
	for name, text := range map[string]string{"cidr": m.CIDR, "within": m.Within} {
		if text == "" || strings.Contains(text, "{") {
			continue
		}
		if _, err := netip.ParsePrefix(text); err != nil {
			v.bad("match.%s: %q is not a CIDR", name, text)
		}
	}
}

// filters checks the names of the vendor's own filters.
func (v *validator) filters(fs []common.ResourceFilter) {
	for i, f := range fs {
		if _, ok := FilterAllowed(v.caps, f.Name); ok {
			continue
		}
		names := make([]string, 0, len(v.caps.Filters))
		for _, c := range v.caps.Filters {
			names = append(names, c.Name)
		}
		if len(names) == 0 {
			v.bad("filters[%d]: %q is not a filter: %s of %s accepts no filters", i, f.Name, v.kind, v.vendor)
		} else {
			v.bad("filters[%d]: %q is not a %s filter of %s (%s)", i, f.Name, v.kind, v.vendor, nameList(names, 12))
		}
	}
}

// order checks select and orderBy against the kind's creation time and labels.
func (v *validator) order(u *common.ResourceUse) {
	if u.OrderBy == orderCreated && !v.caps.HasCreated {
		v.bad("orderBy created is not supported: %s of %s has no creation time (use name or label:<key>)", v.kind, v.vendor)
	}
	if (u.Select == common.SelectNewest || u.Select == common.SelectOldest) && u.OrderBy == "" && !v.caps.HasCreated {
		v.bad("select %s needs orderBy: %s of %s has no creation time (use name or label:<key>)", u.Select, v.kind, v.vendor)
	}
	if strings.HasPrefix(u.OrderBy, "label:") && v.caps.Tags == vendors.TagsNone {
		v.bad("orderBy %s is not supported: %s of %s has no tags or labels", u.OrderBy, v.kind, v.vendor)
	}
}

// placeholders checks the placeholders of one string.
func (v *validator) placeholders(s string) {
	if s == "" {
		return
	}
	names, err := Placeholdered(s)
	if err != nil {
		v.bad("%v", err)
		return
	}
	for _, n := range names {
		switch {
		case n == phZone && !v.caps.Zonal:
			v.bad("%q: {zone} is only valid on zonal kinds, and %s of %s is not", s, v.kind, v.vendor)
		case (n == phArch || n == phVendorArch) && v.kind != ImagesKind:
			v.bad("%q: {%s} is only valid in images", s, n)
		}
	}
}

// setAttributes lists the attributes the match sets.
func setAttributes(m *common.ResourceMatch) []string {
	var out []string
	add := func(ok bool, a string) {
		if ok {
			out = append(out, a)
		}
	}
	add(len(m.Architecture) > 0, vendors.MatchArchitecture)
	add(len(m.Owner) > 0, vendors.MatchOwner)
	add(len(m.State) > 0, vendors.MatchState)
	add(m.IncludeDeprecated != nil, vendors.MatchIncludeDeprecated)
	add(m.CreatedAfter != nil, vendors.MatchCreatedAfter)
	add(m.CreatedBefore != nil, vendors.MatchCreatedBefore)
	add(m.Description != "", vendors.MatchDescription)
	add(len(m.Zone) > 0, vendors.MatchZone)
	add(len(m.Network) > 0, vendors.MatchNetwork)
	add(m.CIDR != "", vendors.MatchCIDR)
	add(m.Within != "", vendors.MatchWithin)
	return out
}
