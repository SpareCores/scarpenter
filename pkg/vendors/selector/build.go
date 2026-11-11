/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selector

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// RegionAll is the key of `ids` for a resource that is global at the vendor:
// its IDs apply to every region in scope.
const RegionAll = "*"

// Selects reports whether the use block gives anything that selects (a present
// but empty block selects nothing and is invalid, never "the default").
func Selects(u *common.ResourceUse) bool {
	if u == nil {
		return false
	}
	hasMatch := u.Match != nil && len(setAttributes(u.Match)) > 0
	return len(u.IDs) > 0 || u.Name != "" || len(u.Tags) > 0 || hasMatch || len(u.Filters) > 0 || u.Expression != ""
}

// UsesZone reports whether any string of the block has the {zone} placeholder:
// the selector is then expanded per zone.
func UsesZone(u *common.ResourceUse) bool {
	found := false
	strs(u, func(s string) {
		if names, err := Placeholdered(s); err == nil && slices.Contains(names, phZone) {
			found = true
		}
	})
	return found
}

// strs calls f for every string value of the block that has placeholders.
func strs(u *common.ResourceUse, f func(string)) {
	if u == nil {
		return
	}
	for _, ids := range u.IDs {
		for _, id := range ids {
			f(id)
		}
	}
	f(u.Name)
	for k, v := range u.Tags {
		f(k)
		f(v)
	}
	if m := u.Match; m != nil {
		for _, l := range [][]string{m.Owner, m.Zone, m.Network} {
			for _, s := range l {
				f(s)
			}
		}
		f(m.Description)
		f(m.CIDR)
		f(m.Within)
	}
	for _, fl := range u.Filters {
		for _, v := range fl.Values {
			f(v)
		}
	}
	f(u.Expression)
}

// IDsFor returns the IDs of the region: those under the key "*" and under the
// region's own key, without duplicates, in order.
func IDsFor(ids map[string]common.IDList, region string) []string {
	var out []string
	for _, k := range []string{RegionAll, region} {
		for _, id := range ids[k] {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// Build turns the NodeClass's `use` into the selector of one region (and zone):
// the IDs of the region gathered, every string expanded, Select defaulted from
// the kind's capabilities. A block that selects nothing, and ids that name no
// ID for the region, are errors: an empty reference would mean the vendor's
// default.
func Build(u *common.ResourceUse, x Context, caps vendors.SelectorCaps) (vendors.UseRef, error) {
	if !Selects(u) {
		return vendors.UseRef{}, errors.New("the use block selects nothing: set at least one of ids, name, tags, " +
			"match, filters and expression")
	}
	var ref vendors.UseRef
	var errs []error
	ex := func(s string) string {
		if s == "" {
			return ""
		}
		v, err := Expand(s, x)
		if err != nil {
			errs = append(errs, err)
		}
		return v
	}
	exAll := func(l []string) []string {
		out := make([]string, 0, len(l))
		for _, s := range l {
			out = append(out, ex(s))
		}
		return out
	}
	if u.IDs != nil {
		ids := IDsFor(u.IDs, x.Region)
		if len(ids) == 0 {
			return vendors.UseRef{}, fmt.Errorf("no IDs are configured for the region %s (and none under %q)",
				x.Region, RegionAll)
		}
		ref.IDs = exAll(ids)
	}
	ref.Name = ex(u.Name)
	if len(u.Tags) > 0 {
		ref.Tags = map[string]string{}
		for k, v := range u.Tags {
			ref.Tags[ex(k)] = ex(v)
		}
	}
	if m := u.Match; m != nil {
		ref.Match = *m.DeepCopy()
		ref.Match.Owner = exAll(m.Owner)
		ref.Match.Zone = exAll(m.Zone)
		ref.Match.Network = exAll(m.Network)
		ref.Match.Description = ex(m.Description)
		ref.Match.CIDR = ex(m.CIDR)
		ref.Match.Within = ex(m.Within)
		for _, c := range []struct{ name, v string }{{"match.cidr", ref.Match.CIDR}, {"match.within", ref.Match.Within}} {
			if c.v == "" {
				continue
			}
			if _, err := netip.ParsePrefix(c.v); err != nil {
				errs = append(errs, fmt.Errorf("%s: %q is not a CIDR", c.name, c.v))
			}
		}
	}
	for _, f := range u.Filters {
		ref.Filters = append(ref.Filters, common.ResourceFilter{Name: f.Name, Values: exAll(f.Values)})
	}
	ref.Expression = ex(u.Expression)
	ref.Select = u.Select
	if ref.Select == "" {
		ref.Select = caps.DefaultSelect
	}
	ref.OrderBy = u.OrderBy
	if err := errors.Join(errs...); err != nil {
		return vendors.UseRef{}, err
	}
	return ref, nil
}
