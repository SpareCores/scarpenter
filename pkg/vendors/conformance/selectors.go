/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package conformance

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// SeedSpec describes a resource a user brought, for the selector subtests. Name
// and Tags are always honoured; the other fields only when the fixture's
// SeedAttrs lists them.
type SeedSpec struct {
	Name string
	Tags map[string]string
	// Created is the creation time ("created").
	Created time.Time
	// Description ("description").
	Description string
	// Zone is the account's zone name ("zone").
	Zone string
	// CIDR is the range ("cidr").
	CIDR string
}

// SeedFunc makes a resource the way a user would. The returned ref has the ID
// (and the name, where the vendor has one).
type SeedFunc func(
	t *testing.T, d vendors.Driver, kind vendors.ResourceKind, region string, spec SeedSpec,
) vendors.ResourceRef

func (fx Fixture) seeds(attr string) bool { return slices.Contains(fx.SeedAttrs, attr) }

// selectors runs the cases of the `use` selector (docs/reference/resource-selectors.md)
// against every kind that accepts it: the same assertions whoever evaluates a
// field, the vendor, a superset or only the client.
func selectors(t *testing.T, e *env) {
	mr, ok := e.d.(vendors.ManagedResources)
	if !ok {
		t.Skip("the driver has no managed resources")
	}
	caps := mr.Selectors()
	kinds := e.fx.SeedKinds
	if len(kinds) == 0 && e.fx.ResourceKind != "" {
		kinds = []vendors.ResourceKind{e.fx.ResourceKind}
	}
	for kind := range caps {
		checkDeclaration(t, e.d.Vendor(), kind, caps[kind])
	}
	if e.fx.Seed == nil {
		t.Skip("the fixture has no Seed")
	}
	for _, kind := range kinds {
		c, ok := caps[kind]
		if !ok {
			t.Errorf("the fixture's kind %s has no selector declaration", kind)
			continue
		}
		t.Run(string(kind), func(t *testing.T) { selectorCases(t, e, mr, kind, c) })
	}
}

// checkDeclaration checks that a declaration is well formed and that validation
// rejects what it does not support, with the supported names.
func checkDeclaration(t *testing.T, vendor string, kind vendors.ResourceKind, c vendors.SelectorCaps) {
	t.Helper()
	checkWellFormed(t, kind, c)
	path := "vendors." + vendor + ".resources." + string(kind) + ".use"
	reject := func(name string, u common.ResourceUse, want string) {
		t.Helper()
		err := errors.Join(selector.Validate(vendor, kind, path, &u, c)...)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), string(kind)) {
			t.Errorf("%s/%s: %v, want an error with %q and the kind", kind, name, err, want)
		}
	}
	reject("empty use", common.ResourceUse{}, "selects nothing")
	reject("unknown filter", common.ResourceUse{Filters: []common.ResourceFilter{
		{Name: "no-such-filter-zz", Values: []string{"x"}}}}, "no-such-filter-zz")
	reject("unknown placeholder", common.ResourceUse{Name: "x-{nope}"}, "unknown placeholder {nope}")
	if !c.HasCreated {
		reject("created order", common.ResourceUse{Name: "x", Select: common.SelectNewest, OrderBy: orderCreated},
			"has no creation time")
		reject("created match", common.ResourceUse{Match: &common.ResourceMatch{CreatedAfter: &metaTime}},
			"has no creation time")
	}
	if !c.Zonal {
		reject("zone placeholder", common.ResourceUse{Name: "x-{zone}"}, "{zone} is only valid on zonal kinds")
	}
	if c.Name == nil {
		reject("name", common.ResourceUse{Name: "x"}, "name is not supported")
	}
	if c.Tags == vendors.TagsNone {
		reject("tags", common.ResourceUse{Tags: map[string]string{"a": "b"}}, "tags are not supported")
	}
	if c.Expression == "" {
		reject("expression", common.ResourceUse{Expression: "x"}, "expression is not supported")
	}
	for _, attr := range vendors.MatchAttributes {
		if _, ok := c.Match[attr]; !ok {
			reject("unsupported match."+attr, common.ResourceUse{Match: matchOf(attr)}, "match."+attr+" is not supported")
		}
	}
	for _, f := range c.Filters {
		name := f.Name
		if before, ok := strings.CutSuffix(name, "*"); ok {
			name = before + "x"
		}
		u := common.ResourceUse{Filters: []common.ResourceFilter{{Name: name, Values: []string{"v"}}}}
		if err := errors.Join(selector.Validate(vendor, kind, path, &u, c)...); err != nil &&
			strings.Contains(err.Error(), "is not a") {
			t.Errorf("%s: the declared filter %q is rejected: %v", kind, f.Name, err)
		}
	}
}

// checkWellFormed checks the sides, the attributes and the default select.
func checkWellFormed(t *testing.T, kind vendors.ResourceKind, c vendors.SelectorCaps) {
	t.Helper()
	okSide := func(s vendors.Side) bool {
		return s == vendors.SideServer || s == vendors.SideServerClient || s == vendors.SideClient
	}
	if c.Name != nil && !okSide(c.Name.Side) {
		t.Errorf("%s: the name has the side %q", kind, c.Name.Side)
	}
	for attr, f := range c.Match {
		switch {
		case !slices.Contains(vendors.MatchAttributes, attr):
			t.Errorf("%s: %q is not a match attribute", kind, attr)
		case !okSide(f.Side):
			t.Errorf("%s: match.%s has the side %q", kind, attr, f.Side)
		case attr == vendors.MatchZone && !c.Zonal:
			t.Errorf("%s: match.zone on a kind that is not zonal", kind)
		case (attr == vendors.MatchCreatedAfter || attr == vendors.MatchCreatedBefore) && !c.HasCreated:
			t.Errorf("%s: match.%s on a kind without a creation time", kind, attr)
		}
	}
	seen := map[string]bool{}
	for _, f := range c.Filters {
		if f.Name == "" || seen[f.Name] || !okSide(f.Side) {
			t.Errorf("%s: filter %+v is empty, listed twice or has no side", kind, f)
		}
		seen[f.Name] = true
	}
	switch c.DefaultSelect {
	case common.SelectExactlyOne, common.SelectAll, common.SelectNewest, common.SelectOldest:
	default:
		t.Errorf("%s: the default select is %q", kind, c.DefaultSelect)
	}
	if kind == selector.ImagesKind && c.DefaultSelect != common.SelectExactlyOne {
		t.Errorf("%s: a launch needs one image, the default select is exactlyOne", kind)
	}
}

var metaTime = metav1.Time{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

// matchOf is a match that sets only the attribute.
func matchOf(attr string) *common.ResourceMatch {
	m := &common.ResourceMatch{}
	switch attr {
	case vendors.MatchArchitecture:
		m.Architecture = []string{archAMD64}
	case vendors.MatchOwner:
		m.Owner = []string{"self"}
	case vendors.MatchState:
		m.State = []string{"available"}
	case vendors.MatchIncludeDeprecated:
		m.IncludeDeprecated = new(true)
	case vendors.MatchCreatedAfter:
		m.CreatedAfter = &metaTime
	case vendors.MatchCreatedBefore:
		m.CreatedBefore = &metaTime
	case vendors.MatchDescription:
		m.Description = "x"
	case vendors.MatchZone:
		m.Zone = []string{"z"}
	case vendors.MatchNetwork:
		m.Network = []string{"n"}
	case vendors.MatchCIDR:
		m.CIDR = "10.0.0.0/24"
	case vendors.MatchWithin:
		m.Within = "10.0.0.0/16"
	}
	return m
}

func idsOf(refs []vendors.ResourceRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.ID
	}
	slices.Sort(out)
	return out
}

// sel is what the selector cases of one kind work with.
type sel struct {
	t    *testing.T
	e    *env
	mr   vendors.ManagedResources
	kind vendors.ResourceKind
	c    vendors.SelectorCaps
	tag  string
	// a, b and d are the resources seeded first: a and b have the pair k=1 (a also
	// env=prod), d has k=2.
	a, b, d vendors.ResourceRef
}

// tags are the tags of a seeded resource: the case's own tag and the pairs.
func (s *sel) tags(pairs ...string) map[string]string {
	m := map[string]string{"scsel": s.tag}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

func (s *sel) seed(spec SeedSpec) vendors.ResourceRef {
	s.t.Helper()
	return s.e.fx.Seed(s.t, s.e.d, s.kind, s.e.region, spec)
}

// use builds the selector of the region the way the core does, and resolves it.
func (s *sel) resolve(u common.ResourceUse, zone string) ([]vendors.ResourceRef, error) {
	s.t.Helper()
	x := selector.Context{Region: s.e.region, Zone: zone, NodeClass: nodeClassName, Channel: "stable"}
	ref, err := selector.Build(&u, x, s.c)
	if err != nil {
		s.t.Fatalf("building %+v: %v", u, err)
	}
	return s.mr.Resolve(s.t.Context(), s.kind, s.e.region, ref)
}

func (s *sel) expect(name string, u common.ResourceUse, want ...vendors.ResourceRef) {
	s.t.Helper()
	got, err := s.resolve(u, s.e.zone)
	if err != nil || !slices.Equal(idsOf(got), idsOf(want)) {
		s.t.Errorf("%s: %v, %v; want %v", name, idsOf(got), err, idsOf(want))
	}
}

// none expects the verdict that nothing matches, and returns it.
func (s *sel) noMatch(name string, u common.ResourceUse) *selector.NoMatchError {
	s.t.Helper()
	got, err := s.resolve(u, s.e.zone)
	nm, ok := errors.AsType[*selector.NoMatchError](err)
	if !ok || len(got) != 0 {
		s.t.Errorf("%s: %v, %v; want NoMatch", name, idsOf(got), err)
	}
	return nm
}

// ambiguous expects the verdict that too much matches, and returns it.
func (s *sel) tooMany(name string, u common.ResourceUse) error {
	s.t.Helper()
	_, err := s.resolve(u, s.e.zone)
	if _, ok := errors.AsType[*selector.AmbiguousError](err); !ok {
		s.t.Errorf("%s: %v, want Ambiguous", name, err)
	}
	return err
}

func (s *sel) none(name string, u common.ResourceUse) { _ = s.noMatch(name, u) }

func (s *sel) ambiguous(name string, u common.ResourceUse) { _ = s.tooMany(name, u) }

func selectorCases(
	t *testing.T, e *env, mr vendors.ManagedResources, kind vendors.ResourceKind, c vendors.SelectorCaps,
) {
	if c.Tags == vendors.TagsNone {
		t.Skip("the kind has no tags to seed")
	}
	s := &sel{t: t, e: e, mr: mr, kind: kind, c: c, tag: fmt.Sprintf("scsel%d", counter.Add(1))}
	s.andedFields()
	s.ids()
	s.oneOrMany()
	s.ordering()
	s.created()
	s.zone()
	s.globs()
	s.descriptions()
	s.ranges()
	if !(vendors.UseRef{Select: common.SelectAll}).IsDefault() {
		t.Error("IsDefault: only nothing that selects is the vendor's default")
	}
}

var (
	all     = common.SelectAll
	useTags = func(m map[string]string) common.ResourceUse { return common.ResourceUse{Tags: m, Select: all} }
)

// andedFields: fields are ANDed, list values ORed.
func (s *sel) andedFields() {
	s.a = s.seed(SeedSpec{Name: "sel-" + s.tag + "-a", Tags: s.tags("k", "1", "env", "prod")})
	s.b = s.seed(SeedSpec{Name: "sel-" + s.tag + "-b", Tags: s.tags("k", "1")})
	s.d = s.seed(SeedSpec{Name: "other-" + s.tag, Tags: s.tags("k", "2")})
	a, b, d := s.a, s.b, s.d
	s.expect("tags", useTags(s.tags()), a, b, d)
	s.expect("tags and a value", useTags(s.tags("k", "1")), a, b)
	s.expect("tags and a key that exists", useTags(s.tags("env", "*")), a)
	s.expect("tags with two pairs", useTags(s.tags("k", "1", "env", "prod")), a)
	nm := s.noMatch("a key nobody has", useTags(s.tags("nobody", "*")))
	if nm != nil && !strings.Contains(nm.Error(), "listed") {
		s.t.Errorf("the stages are named: %v", nm)
	}
	if s.c.Name != nil {
		pre := "sel-" + s.tag + "-"
		s.expect("name glob", common.ResourceUse{Name: pre + "*", Select: all}, a, b)
		s.expect("name exact", common.ResourceUse{Name: pre + "a", Select: all}, a)
		s.expect("name and tags", common.ResourceUse{Name: pre + "*", Tags: s.tags("env", "prod"), Select: all}, a)
		s.none("name and tags that exclude it",
			common.ResourceUse{Name: "other-" + s.tag, Tags: s.tags("k", "1"), Select: all})
		s.expect("name, a question mark", common.ResourceUse{Name: pre + "?", Select: all}, a, b)
	}
}

// ids: ANDed with the rest; a missing ID does not fail the others and is named; an
// ID that exists but does not match is named; the key * is every region.
func (s *sel) ids() {
	a, b, d, region := s.a, s.b, s.d, s.e.region
	missing := "scsel-missing-" + s.tag
	ids := func(l ...string) map[string]common.IDList { return map[string]common.IDList{region: l} }
	s.expect("ids", common.ResourceUse{IDs: ids(a.ID, b.ID), Select: all}, a, b)
	s.expect("ids and tags", common.ResourceUse{IDs: ids(a.ID, b.ID, d.ID), Tags: s.tags("k", "1"), Select: all}, a, b)
	s.expect("a missing ID does not fail the others", common.ResourceUse{IDs: ids(a.ID, missing), Select: all}, a)
	if nm := s.noMatch("only a missing ID", common.ResourceUse{IDs: ids(missing)}); nm != nil &&
		!slices.Contains(nm.Missing, missing) {
		s.t.Errorf("the missing ID is named: %v", nm)
	}
	// A vendor that filters on the server cannot tell a mismatch from a missing
	// ID, so the ID is named in either list.
	nm := s.noMatch("an ID that does not match", common.ResourceUse{IDs: ids(d.ID), Tags: s.tags("k", "1")})
	if nm != nil &&
		!strings.Contains(nm.Error(), d.ID) {
		s.t.Errorf("the ID that does not match is named: %v", nm)
	}
	global := map[string]common.IDList{selector.RegionAll: {a.ID}, region: {b.ID}}
	s.expect("a global ID", common.ResourceUse{IDs: global, Select: all}, a, b)
	s.expect("a global ID alone", common.ResourceUse{IDs: map[string]common.IDList{selector.RegionAll: {a.ID}}}, a)
}

// oneOrMany: zero matches and too many are stated, never a guess.
func (s *sel) oneOrMany() {
	s.none("zero matches", common.ResourceUse{Tags: s.tags("k", "nobody")})
	err := s.tooMany("two matches under exactlyOne",
		common.ResourceUse{Tags: s.tags("k", "1"), Select: common.SelectExactlyOne})
	if err != nil && !strings.Contains(err.Error(), "2 match") {
		s.t.Errorf("the Ambiguous error carries the count: %v", err)
	}
	s.expect("exactlyOne with one", common.ResourceUse{Tags: s.tags("env", "prod"), Select: common.SelectExactlyOne}, s.a)
}

// ordering: newest and oldest by name in natural order, by label, and a tie.
func (s *sel) ordering() {
	if s.c.Name == nil {
		return
	}
	pre := "nat-" + s.tag + "-"
	n9 := s.seed(SeedSpec{Name: pre + "9", Tags: s.tags("v", "4081.3.1")})
	n10 := s.seed(SeedSpec{Name: pre + "10", Tags: s.tags("v", "4081.10.0")})
	byName := func(sel common.Select) common.ResourceUse {
		return common.ResourceUse{Name: pre + "*", Select: sel, OrderBy: "name"}
	}
	byLabel := func(sel common.Select) common.ResourceUse {
		return common.ResourceUse{Tags: s.tags("v", "*"), Select: sel, OrderBy: "label:v"}
	}
	s.expect("newest by name is natural", byName(common.SelectNewest), n10)
	s.expect("oldest by name is natural", byName(common.SelectOldest), n9)
	s.expect("newest by a label", byLabel(common.SelectNewest), n10)
	s.expect("oldest by a label", byLabel(common.SelectOldest), n9)
	s.ambiguous("a tie for newest",
		common.ResourceUse{Tags: s.tags("k", "1"), Select: common.SelectNewest, OrderBy: "label:k"})
}

// created: the creation time orders, bounds and ties.
func (s *sel) created() {
	if !s.c.HasCreated || !s.e.fx.seeds("created") {
		return
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-48 * time.Hour)
	seed := func(n int, at time.Time) vendors.ResourceRef {
		return s.seed(SeedSpec{Name: fmt.Sprintf("age-%s-%d", s.tag, n), Tags: s.tags("age", "y"), Created: at})
	}
	old, mid, young := seed(1, base), seed(2, base.Add(time.Hour)), seed(3, base.Add(2*time.Hour))
	age := func(sel common.Select, m *common.ResourceMatch) common.ResourceUse {
		return common.ResourceUse{Tags: s.tags("age", "y"), Select: sel, Match: m}
	}
	s.expect("newest by created", age(common.SelectNewest, nil), young)
	oldest := common.ResourceUse{Tags: s.tags("age", "y"), Select: common.SelectOldest, OrderBy: "created"}
	s.expect("oldest by created", oldest, old)
	after, before := metav1.Time{Time: base.Add(30 * time.Minute)}, metav1.Time{Time: base.Add(90 * time.Minute)}
	s.expect("createdAfter", age(all, &common.ResourceMatch{CreatedAfter: &after}), mid, young)
	s.expect("createdBefore", age(all, &common.ResourceMatch{CreatedBefore: &before}), old, mid)
	s.expect("created window", age(all, &common.ResourceMatch{CreatedAfter: &after, CreatedBefore: &before}), mid)
	seed(4, base.Add(2*time.Hour))
	s.ambiguous("two created at the same time under newest", age(common.SelectNewest, nil))
}

// zone: {zone} is expanded per zone.
func (s *sel) zone() {
	if !s.c.Zonal || s.c.Name == nil || s.e.zone == "" {
		return
	}
	z := s.seed(SeedSpec{Name: "zn-" + s.tag + "-" + s.e.zone, Tags: s.tags("zn", "y")})
	u := common.ResourceUse{Name: "zn-" + s.tag + "-{zone}", Select: all}
	got, err := s.resolve(u, s.e.zone)
	if err != nil || !slices.Equal(idsOf(got), idsOf([]vendors.ResourceRef{z})) {
		s.t.Errorf("{zone}: %v, %v; want %s", idsOf(got), err, z.ID)
	}
	if _, err := s.resolve(u, "elsewhere-zone"); !errors.As(err, new(*selector.NoMatchError)) {
		s.t.Errorf("{zone} in a zone without a match: %v, want NoMatch", err)
	}
}

// globs: * and ? are wildcards, a backslash makes them literal.
func (s *sel) globs() {
	if s.c.Name == nil || !s.e.fx.seeds("specialNames") {
		return
	}
	star := s.seed(SeedSpec{Name: "gl-" + s.tag + "-a*b", Tags: s.tags("gl", "y")})
	s.seed(SeedSpec{Name: "gl-" + s.tag + "-aXb", Tags: s.tags("gl", "y")})
	s.expect("an escaped star is literal", common.ResourceUse{Name: `gl-` + s.tag + `-a\*b`, Select: all}, star)
	if got, _ := s.resolve(common.ResourceUse{Name: "gl-" + s.tag + "-a*b", Select: all}, s.e.zone); len(got) != 2 {
		s.t.Errorf("an unescaped star is a wildcard: %v", idsOf(got))
	}
}

func (s *sel) descriptions() {
	if _, ok := s.c.Match[vendors.MatchDescription]; !ok || !s.e.fx.seeds("description") {
		return
	}
	x := s.seed(SeedSpec{Name: "ds-" + s.tag + "-x", Tags: s.tags("ds", "y"), Description: "alpha " + s.tag})
	s.seed(SeedSpec{Name: "ds-" + s.tag + "-y", Tags: s.tags("ds", "y"), Description: "beta " + s.tag})
	s.expect("description", common.ResourceUse{Tags: s.tags("ds", "y"), Select: all,
		Match: &common.ResourceMatch{Description: "alpha*"}}, x)
}

// ranges: the range equals a CIDR, or lies inside one, for kinds that have one.
func (s *sel) ranges() {
	if _, ok := s.c.Match[vendors.MatchCIDR]; !ok || !s.e.fx.seeds("cidr") {
		return
	}
	fx := s.e.fx
	r1 := s.seed(SeedSpec{Name: "ip-" + s.tag + "-1", Tags: s.tags("ip", "y"), CIDR: fx.SeedCIDRs[0]})
	r2 := s.seed(SeedSpec{Name: "ip-" + s.tag + "-2", Tags: s.tags("ip", "y"), CIDR: fx.SeedCIDRs[1]})
	ip := func(m common.ResourceMatch) common.ResourceUse {
		return common.ResourceUse{Tags: s.tags("ip", "y"), Select: all, Match: &m}
	}
	s.expect("cidr equals the first", ip(common.ResourceMatch{CIDR: fx.SeedCIDRs[0]}), r1)
	s.expect("cidr equals the second", ip(common.ResourceMatch{CIDR: fx.SeedCIDRs[1]}), r2)
	if _, ok := s.c.Match[vendors.MatchWithin]; ok {
		s.expect("within the parent", ip(common.ResourceMatch{Within: fx.SeedWithin}), r1, r2)
		s.expect("within itself", ip(common.ResourceMatch{Within: fx.SeedCIDRs[0]}), r1)
		s.none("within a smaller range", ip(common.ResourceMatch{Within: "10.255.255.0/30"}))
	}
}
