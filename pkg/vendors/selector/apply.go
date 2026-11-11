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
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Fields are what the matcher reads of one item the vendor returned. A driver
// fills what it knows: a field it leaves out is not re-applied (the vendor
// evaluated it, or the kind has no such field), so a "server" field never
// needs data the response lacks.
type Fields struct {
	ID   string
	Name string
	// Tags are the tags or labels of a kind with key and value; TagStrings the
	// plain-string tags of OpenStack, where the pair k: v is the string "k=v".
	Tags       map[string]string
	TagStrings []string
	// Created is the creation time; the zero time is unknown, and an item with
	// an unknown time never passes createdAfter or createdBefore.
	Created time.Time
	// Attrs holds the values of the portable attributes the item has, by
	// attribute name (vendors.MatchArchitecture, ...): the item's own
	// architecture, owner, normalised state (available, pending, failed),
	// description, zone ID, network IDs and CIDRs. Attrs[MatchIncludeDeprecated]
	// = ["true"] marks a deprecated item. An attribute that is absent is
	// unknown and is not re-applied.
	Attrs map[string][]string
}

// Stage is the number of candidates after one stage of the selection: listed
// (what the vendor returned), then each field of the selector in order.
type Stage struct {
	// Name is "listed", "ids", "name", "tags" or "match.<attribute>"; Detail the
	// values the stage asked for.
	Name   string
	Detail string
	Count  int
}

func (s Stage) String() string {
	if s.Name == "listed" {
		return fmt.Sprintf("%d listed", s.Count)
	}
	d := ""
	if s.Detail != "" {
		d = " " + s.Detail
	}
	return fmt.Sprintf("%d match %s%s", s.Count, s.Name, d)
}

func stagesString(st []Stage) string {
	parts := make([]string, len(st))
	for i, s := range st {
		parts[i] = s.String()
	}
	return strings.Join(parts, ", ")
}

// NoMatchError says that nothing matched the selector. The region is unusable.
type NoMatchError struct {
	Stages []Stage
	// Missing are the requested IDs the vendor did not return; Mismatched the
	// IDs it returned that failed another field.
	Missing, Mismatched []string
}

func (e *NoMatchError) Error() string {
	msg := "nothing matches: " + stagesString(e.Stages)
	if len(e.Missing) > 0 {
		msg += "; IDs not in the vendor's answer: " + strings.Join(e.Missing, ", ")
	}
	if len(e.Mismatched) > 0 {
		msg += "; IDs that do not match: " + strings.Join(e.Mismatched, ", ")
	}
	return msg
}

// AmbiguousError says that the selector selected more than its select allows:
// several matches under exactlyOne, or a tie for newest or oldest.
type AmbiguousError struct {
	Select common.Select
	Stages []Stage
	// Count is the number of matches (of the tie, for newest and oldest) and
	// Candidates the first of them, by ID and name.
	Count      int
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	what := "select exactlyOne needs one"
	if e.Select == common.SelectNewest || e.Select == common.SelectOldest {
		what = fmt.Sprintf("%d tie for %s", e.Count, e.Select)
	}
	return fmt.Sprintf("%d match (%s), %s: %s", e.Count, stagesString(e.Stages), what, strings.Join(e.Candidates, ", "))
}

// maxCandidates are the candidates an AmbiguousError names.
const maxCandidates = 5

// Result is the outcome of Apply.
type Result[T any] struct {
	// Items are the selected items, best first for newest and oldest.
	Items  []T
	Stages []Stage
	// Missing and Mismatched as in NoMatchError, also when other IDs matched:
	// a missing ID does not fail the others.
	Missing, Mismatched []string
}

// Apply runs the selector over what the vendor listed: the fields are ANDed,
// every field the items give data for is re-applied (the vendor may have
// narrowed to a superset), then Select chooses. It returns *NoMatchError for
// zero matches and *AmbiguousError for too many; the other errors are
// malformed selectors that validation should have caught. ref must not be the
// default selector.
func Apply[T any](
	ref vendors.UseRef, caps vendors.SelectorCaps, listed []T, fields func(T) Fields,
) (Result[T], error) {
	type cand struct {
		item T
		f    Fields
	}
	cands := make([]cand, len(listed))
	for i, it := range listed {
		cands[i] = cand{it, fields(it)}
	}
	var res Result[T]
	res.Stages = []Stage{{Name: "listed", Count: len(cands)}}
	stage := func(name, detail string, keep func(Fields) bool) {
		if len(cands) == 0 {
			return // the stage that emptied the list is the one to report
		}
		cands = slices.DeleteFunc(cands, func(c cand) bool { return !keep(c.f) })
		res.Stages = append(res.Stages, Stage{Name: name, Detail: detail, Count: len(cands)})
	}

	if len(ref.IDs) > 0 {
		for _, id := range ref.IDs {
			if !slices.ContainsFunc(cands, func(c cand) bool { return c.f.ID == id }) {
				res.Missing = append(res.Missing, id)
			}
		}
		stage("ids", "", func(f Fields) bool { return slices.Contains(ref.IDs, f.ID) })
	}
	idMatched := func() {
		// IDs that passed the ids stage and fell at a later one.
		if len(ref.IDs) == 0 {
			return
		}
		for _, id := range ref.IDs {
			if !slices.Contains(res.Missing, id) && !slices.ContainsFunc(cands, func(c cand) bool { return c.f.ID == id }) {
				res.Mismatched = append(res.Mismatched, id)
			}
		}
	}
	if ref.Name != "" {
		stage("name", fmt.Sprintf("%q", ref.Name), func(f Fields) bool { return Glob(ref.Name, f.Name) })
	}
	if len(ref.Tags) > 0 {
		stage("tags", fmt.Sprint(ref.Tags), func(f Fields) bool { return tagsMatch(ref.Tags, caps.Tags, f) })
	}
	for _, attr := range vendors.MatchAttributes {
		if detail, keep, ok := matchStage(attr, ref.Match, caps); ok {
			stage("match."+attr, detail, keep)
		}
	}
	idMatched()
	if res.Stages[len(res.Stages)-1].Count == 0 {
		return res, &NoMatchError{Stages: res.Stages, Missing: res.Missing, Mismatched: res.Mismatched}
	}
	items, err := choose(ref, caps, cands, func(c cand) (T, Fields) { return c.item, c.f }, &res)
	if err != nil {
		return res, err
	}
	res.Items = items
	return res, nil
}

// choose applies Select to the candidates.
func choose[C, T any](
	ref vendors.UseRef, caps vendors.SelectorCaps, cands []C, split func(C) (T, Fields), res *Result[T],
) ([]T, error) {
	sel := ref.Select
	if sel == "" {
		sel = caps.DefaultSelect
	}
	if sel == "" {
		sel = common.SelectExactlyOne
	}
	items := make([]T, len(cands))
	fs := make([]Fields, len(cands))
	for i, c := range cands {
		items[i], fs[i] = split(c)
	}
	describe := func(idx []int) []string {
		out := make([]string, 0, min(len(idx), maxCandidates))
		for _, i := range idx[:min(len(idx), maxCandidates)] {
			n := fs[i].ID
			if fs[i].Name != "" && fs[i].Name != n {
				n += " (" + fs[i].Name + ")"
			}
			out = append(out, n)
		}
		return out
	}
	all := make([]int, len(items))
	for i := range all {
		all[i] = i
	}
	switch sel {
	case common.SelectAll:
		return items, nil
	case common.SelectExactlyOne:
		if len(items) > 1 {
			return nil, &AmbiguousError{Select: sel, Stages: res.Stages, Count: len(items), Candidates: describe(all)}
		}
		return items, nil
	case common.SelectNewest, common.SelectOldest:
		order := ref.OrderBy
		if order == "" {
			if !caps.HasCreated {
				return nil, fmt.Errorf("select %s needs orderBy: the kind has no creation time", sel)
			}
			order = orderCreated
		}
		key := func(i int) (k string, t time.Time, ok bool) {
			switch {
			case order == orderCreated:
				return "", fs[i].Created, !fs[i].Created.IsZero()
			case order == "name":
				return fs[i].Name, time.Time{}, true
			case strings.HasPrefix(order, "label:"):
				v, ok := fs[i].Tags[strings.TrimPrefix(order, "label:")]
				return v, time.Time{}, ok
			}
			return "", time.Time{}, false
		}
		cmp := func(a, b int) int {
			ka, ta, _ := key(a)
			kb, tb, _ := key(b)
			if order == orderCreated {
				return ta.Compare(tb)
			}
			return NaturalCompare(ka, kb)
		}
		var idx []int
		for i := range items {
			if _, _, ok := key(i); ok {
				idx = append(idx, i)
			}
		}
		if len(idx) == 0 {
			return nil, &NoMatchError{Stages: append(slices.Clone(res.Stages), Stage{Name: "orderBy", Detail: order}),
				Missing: res.Missing, Mismatched: res.Mismatched}
		}
		slices.SortStableFunc(idx, func(a, b int) int {
			if sel == common.SelectNewest {
				return cmp(b, a)
			}
			return cmp(a, b)
		})
		tied := 1
		for tied < len(idx) && cmp(idx[0], idx[tied]) == 0 {
			tied++
		}
		if tied > 1 {
			return nil, &AmbiguousError{Select: sel, Stages: res.Stages, Count: tied, Candidates: describe(idx[:tied])}
		}
		out := make([]T, len(idx))
		for i, j := range idx {
			out[i] = items[j]
		}
		return out[:1], nil
	}
	return nil, fmt.Errorf("unknown select %q", sel)
}

// tagsMatch checks every pair of the selector against the item's tags.
func tagsMatch(want map[string]string, form vendors.TagForm, f Fields) bool {
	for k, v := range want {
		if form == vendors.TagsStrings {
			if !slices.ContainsFunc(f.TagStrings, func(s string) bool {
				if v == "*" {
					return strings.HasPrefix(s, k+"=")
				}
				return s == k+"="+v
			}) {
				return false
			}
			continue
		}
		have, ok := f.Tags[k]
		if !ok || (v != "*" && have != v) {
			return false
		}
	}
	return true
}

// matchStage builds the stage of one `match` attribute; ok is false when the
// selector does not set it.
func matchStage(attr string, m common.ResourceMatch, caps vendors.SelectorCaps) (
	detail string, keep func(Fields) bool, ok bool,
) {
	in := func(want []string) func(Fields) bool {
		return func(f Fields) bool {
			have, known := f.Attrs[attr]
			return !known || slices.ContainsFunc(have, func(h string) bool { return slices.Contains(want, h) })
		}
	}
	if list := listAttr(attr, m); len(list) > 0 {
		return fmt.Sprint(list), in(list), true
	}
	_, hasCap := caps.Match[attr]
	switch attr {
	case vendors.MatchState:
		// A kind with a state never selects a resource that is still being
		// created unless the selector says so.
		if hasCap {
			return "[available]", in([]string{"available"}), true
		}
	case vendors.MatchIncludeDeprecated:
		if hasCap && (m.IncludeDeprecated == nil || !*m.IncludeDeprecated) {
			return "[false]", func(f Fields) bool { return !slices.Contains(f.Attrs[attr], "true") }, true
		}
	case vendors.MatchCreatedAfter:
		if m.CreatedAfter != nil {
			t := m.CreatedAfter.Time
			return t.Format(time.RFC3339), func(f Fields) bool { return !f.Created.IsZero() && f.Created.After(t) }, true
		}
	case vendors.MatchCreatedBefore:
		if m.CreatedBefore != nil {
			t := m.CreatedBefore.Time
			return t.Format(time.RFC3339), func(f Fields) bool { return !f.Created.IsZero() && f.Created.Before(t) }, true
		}
	case vendors.MatchDescription:
		if m.Description != "" {
			return fmt.Sprintf("%q", m.Description), func(f Fields) bool {
				have, known := f.Attrs[attr]
				return !known || (len(have) > 0 && Glob(m.Description, have[0]))
			}, true
		}
	case vendors.MatchCIDR, vendors.MatchWithin:
		return rangeStage(attr, m)
	}
	return "", nil, false
}

// listAttr is the list of values of an attribute that is a list.
func listAttr(attr string, m common.ResourceMatch) []string {
	switch attr {
	case vendors.MatchArchitecture:
		return m.Architecture
	case vendors.MatchOwner:
		return m.Owner
	case vendors.MatchState:
		return m.State
	case vendors.MatchZone:
		return m.Zone
	case vendors.MatchNetwork:
		return m.Network
	}
	return nil
}

// rangeStage is the stage of cidr (the range equals) and within (it lies inside).
func rangeStage(attr string, m common.ResourceMatch) (string, func(Fields) bool, bool) {
	text := m.CIDR
	if attr == vendors.MatchWithin {
		text = m.Within
	}
	if text == "" {
		return "", nil, false
	}
	p, err := netip.ParsePrefix(text)
	if err != nil {
		return text, func(Fields) bool { return false }, true
	}
	p = p.Masked()
	if attr == vendors.MatchCIDR {
		return text, cidrCheck(func(q netip.Prefix) bool { return q == p }), true
	}
	return text, cidrCheck(func(q netip.Prefix) bool { return q.Bits() >= p.Bits() && p.Contains(q.Addr()) }), true
}

// cidrCheck keeps an item if any of its ranges (the attribute "cidr") passes;
// an item without ranges data is not re-applied.
func cidrCheck(ok func(netip.Prefix) bool) func(Fields) bool {
	return func(f Fields) bool {
		have, known := f.Attrs[vendors.MatchCIDR]
		if !known {
			return true
		}
		return slices.ContainsFunc(have, func(s string) bool {
			p, err := netip.ParsePrefix(s)
			return err == nil && ok(p.Masked())
		})
	}
}
