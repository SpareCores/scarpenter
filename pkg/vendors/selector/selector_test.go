/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selector_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

func TestExpand(t *testing.T) {
	x := selector.Context{
		Region: "eu-west-1", Zone: "eu-west-1a", Arch: "arm64", Channel: "stable", Version: "4081.3.1", NodeClass: "default",
		VendorArch: func(a string) (string, bool) { return map[string]string{"amd64": "x86_64", "arm64": "arm64"}[a], true },
	}
	for in, want := range map[string]string{
		"flatcar-{channel}-{version}-{arch}": "flatcar-stable-4081.3.1-arm64",
		"{region}/{zone}/{nodeclass}":        "eu-west-1/eu-west-1a/default",
		"arch={vendorArch}":                  "arch=arm64",
		"a{{b}}c":                            "a{b}c",
		"{{{region}}}":                       "{eu-west-1}",
		"plain":                              "plain",
		"":                                   "",
	} {
		if got, err := selector.Expand(in, x); err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	x.Arch = "amd64"
	if got, _ := selector.Expand("{vendorArch}", x); got != "x86_64" {
		t.Errorf("{vendorArch} for amd64 = %q", got)
	}
	for _, in := range []string{"{nope}", "{region", "region}", "{}", "{a b}", "{zone}{x}"} {
		if got, err := selector.Expand(in, x); err == nil {
			t.Errorf("Expand(%q) = %q, want an error", in, got)
		}
	}
	// A placeholder without a value here is an error, not an empty string.
	if _, err := selector.Expand("{zone}", selector.Context{Region: "r"}); err == nil || !strings.Contains(err.Error(), "{zone} has no value") {
		t.Errorf("{zone} without a zone: %v", err)
	}
	if _, err := selector.Expand("{vendorArch}", selector.Context{Arch: "amd64"}); err == nil {
		t.Error("{vendorArch} on a vendor without a spelling must be an error")
	}
}

func TestPlaceholdered(t *testing.T) {
	names, err := selector.Placeholdered("{region}-{{x}}-{zone}")
	if err != nil || !slices.Equal(names, []string{"region", "zone"}) {
		t.Errorf("%v, %v", names, err)
	}
	if _, err := selector.Placeholdered("{what}"); err == nil || !strings.Contains(err.Error(), "known:") {
		t.Errorf("an unknown name must list the known ones: %v", err)
	}
}

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"nodes-*", "nodes-a", true},
		{"nodes-*", "node-a", false},
		{"*", "", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{`a\*b`, "a*b", true},
		{`a\*b`, "aXb", false},
		{`a\?`, "a?", true},
		{`a\\b`, `a\b`, true},
		{"héllo*", "héllo wörld", true},
		{"A", "a", false},
		{"*.example.com", "x.example.com", true},
	} {
		if got := selector.Glob(tc.pattern, tc.s); got != tc.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
	if selector.HasWildcard(`a\*b`) || !selector.HasWildcard("a*b") || !selector.HasWildcard(`a\\*`) {
		t.Error("HasWildcard")
	}
	if got := selector.LongestLiteral(`ab*cdef?g\*h`); got != "g*h" && got != "cdef" {
		t.Errorf("LongestLiteral = %q", got)
	}
}

func TestNaturalCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"4081.3.1", "4081.10.0", -1},
		{"4081.10.0", "4081.3.1", 1},
		{"a", "a", 0},
		{"n-9", "n-10", -1},
		{"007", "7", 0},
		{"a1", "a01", 0},
		{"abc", "abd", -1},
		{"abc", "ab", 1},
		{"", "a", -1},
		{"v2", "v10b", -1},
	} {
		if got := selector.NaturalCompare(tc.a, tc.b); got != tc.want {
			t.Errorf("NaturalCompare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// item is what the tests select from.
type item struct {
	id, name, desc, zone, net, state, arch, owner string
	tags                                          map[string]string
	strs                                          []string
	created                                       time.Time
	cidrs                                         []string
	deprecated                                    bool
}

func fields(i item) selector.Fields {
	attrs := map[string][]string{vendors.MatchState: {i.state}}
	if i.arch != "" {
		attrs[vendors.MatchArchitecture] = []string{i.arch}
	}
	if i.owner != "" {
		attrs[vendors.MatchOwner] = []string{i.owner}
	}
	if i.zone != "" {
		attrs[vendors.MatchZone] = []string{i.zone}
	}
	if i.net != "" {
		attrs[vendors.MatchNetwork] = []string{i.net}
	}
	if i.desc != "" {
		attrs[vendors.MatchDescription] = []string{i.desc}
	}
	if len(i.cidrs) > 0 {
		attrs[vendors.MatchCIDR] = i.cidrs
	}
	if i.deprecated {
		attrs[vendors.MatchIncludeDeprecated] = []string{"true"}
	}
	return selector.Fields{ID: i.id, Name: i.name, Tags: i.tags, TagStrings: i.strs, Created: i.created, Attrs: attrs}
}

func ids(items []item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.id
	}
	return out
}

var (
	day  = func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	caps = vendors.SelectorCaps{
		Zonal: true, HasCreated: true, Tags: vendors.TagsMap, Name: &vendors.FieldCap{Side: vendors.SideServer},
		Match: map[string]vendors.FieldCap{
			vendors.MatchState: {}, vendors.MatchIncludeDeprecated: {}, vendors.MatchArchitecture: {}, vendors.MatchOwner: {},
			vendors.MatchZone: {}, vendors.MatchNetwork: {}, vendors.MatchCIDR: {}, vendors.MatchWithin: {},
			vendors.MatchCreatedAfter: {}, vendors.MatchCreatedBefore: {}, vendors.MatchDescription: {},
		},
		DefaultSelect: common.SelectAll,
	}
	stock = []item{
		{id: "a", name: "flatcar-4081.3.1", state: "available", tags: map[string]string{"env": "prod", "team": "x"}, created: day(1),
			arch: "amd64", zone: "z1", net: "n1", cidrs: []string{"10.8.4.0/24"}, desc: "first"},
		{id: "b", name: "flatcar-4081.10.0", state: "available", tags: map[string]string{"env": "prod"}, created: day(3),
			arch: "arm64", zone: "z2", net: "n1", cidrs: []string{"10.8.5.0/24", "fd00::/64"}, desc: "second"},
		{id: "c", name: "other", state: "pending", tags: map[string]string{"env": "dev"}, created: day(2),
			arch: "amd64", zone: "z1", net: "n2", cidrs: []string{"192.168.0.0/16"}},
		{id: "d", name: "flatcar-old", state: "available", tags: map[string]string{"env": "prod"}, created: day(3), deprecated: true,
			arch: "amd64", zone: "z1", net: "n1"},
	}
)

func apply(t *testing.T, ref vendors.UseRef) ([]string, error) {
	t.Helper()
	res, err := selector.Apply(ref, caps, stock, fields)
	return ids(res.Items), err
}

func TestApplyAndsEveryField(t *testing.T) {
	for name, tc := range map[string]struct {
		ref  vendors.UseRef
		want []string
	}{
		"name glob":               {vendors.UseRef{Name: "flatcar-*"}, []string{"a", "b"}},
		"name exact":              {vendors.UseRef{Name: "other", Select: common.SelectAll, Match: common.ResourceMatch{State: []string{"pending"}}}, []string{"c"}},
		"state default":           {vendors.UseRef{Name: "other"}, nil},
		"tags value":              {vendors.UseRef{Tags: map[string]string{"env": "prod"}}, []string{"a", "b"}},
		"tags star":               {vendors.UseRef{Tags: map[string]string{"team": "*"}}, []string{"a"}},
		"tags two":                {vendors.UseRef{Tags: map[string]string{"env": "prod", "team": "x"}}, []string{"a"}},
		"tags missing key":        {vendors.UseRef{Tags: map[string]string{"nope": "*"}}, nil},
		"deprecated excluded":     {vendors.UseRef{Name: "flatcar-old"}, nil},
		"deprecated included":     {vendors.UseRef{Name: "flatcar-old", Match: common.ResourceMatch{IncludeDeprecated: new(true)}}, []string{"d"}},
		"architecture":            {vendors.UseRef{Name: "flatcar-*", Match: common.ResourceMatch{Architecture: []string{"arm64"}}}, []string{"b"}},
		"architecture or":         {vendors.UseRef{Name: "flatcar-*", Match: common.ResourceMatch{Architecture: []string{"arm64", "amd64"}}}, []string{"a", "b"}},
		"zone":                    {vendors.UseRef{Match: common.ResourceMatch{Zone: []string{"z2"}}}, []string{"b"}},
		"network":                 {vendors.UseRef{Match: common.ResourceMatch{Network: []string{"n1"}}}, []string{"a", "b"}},
		"description glob":        {vendors.UseRef{Match: common.ResourceMatch{Description: "sec*"}}, []string{"b"}},
		"created after":           {vendors.UseRef{Match: common.ResourceMatch{CreatedAfter: &metav1.Time{Time: day(2)}}}, []string{"b"}},
		"created before":          {vendors.UseRef{Match: common.ResourceMatch{CreatedBefore: &metav1.Time{Time: day(3)}}}, []string{"a"}},
		"cidr equals":             {vendors.UseRef{Match: common.ResourceMatch{CIDR: "10.8.5.0/24"}}, []string{"b"}},
		"cidr normalised":         {vendors.UseRef{Match: common.ResourceMatch{CIDR: "10.8.5.7/24"}}, []string{"b"}},
		"cidr any of several":     {vendors.UseRef{Match: common.ResourceMatch{CIDR: "fd00::/64"}}, []string{"b"}},
		"cidr is not within":      {vendors.UseRef{Match: common.ResourceMatch{CIDR: "10.8.0.0/16"}}, nil},
		"within":                  {vendors.UseRef{Match: common.ResourceMatch{Within: "10.8.0.0/16"}}, []string{"a", "b"}},
		"within itself":           {vendors.UseRef{Match: common.ResourceMatch{Within: "10.8.4.0/24"}}, []string{"a"}},
		"within a smaller range":  {vendors.UseRef{Match: common.ResourceMatch{Within: "10.8.4.0/25"}}, nil},
		"ids":                     {vendors.UseRef{IDs: []string{"b", "c"}}, []string{"b"}},
		"ids and tags":            {vendors.UseRef{IDs: []string{"a", "b"}, Tags: map[string]string{"team": "x"}}, []string{"a"}},
		"everything":              {vendors.UseRef{IDs: []string{"a", "b"}, Name: "flatcar-*", Tags: map[string]string{"env": "prod"}, Match: common.ResourceMatch{Zone: []string{"z2"}, Architecture: []string{"arm64"}, Within: "10.0.0.0/8"}}, []string{"b"}},
		"owner is not re-applied": {vendors.UseRef{Match: common.ResourceMatch{Owner: []string{"someone"}}, Name: "other"}, nil},
	} {
		got, err := apply(t, tc.ref)
		if _, none := errors.AsType[*selector.NoMatchError](err); tc.want == nil {
			if !none {
				t.Errorf("%s: %v, %v; want nothing", name, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

func TestApplyReAppliesWhatTheVendorOnlyNarrowed(t *testing.T) {
	// The vendor returned a superset (everything): the answer is the same.
	ref := vendors.UseRef{Name: "flatcar-*", Tags: map[string]string{"env": "prod"}, Match: common.ResourceMatch{Zone: []string{"z1"}}}
	res, err := selector.Apply(ref, caps, stock, fields)
	if err != nil || !slices.Equal(ids(res.Items), []string{"a"}) {
		t.Fatalf("%v, %v", ids(res.Items), err)
	}
	only, _ := selector.Apply(ref, caps, []item{stock[0]}, fields)
	if !slices.Equal(ids(only.Items), ids(res.Items)) {
		t.Errorf("the vendor's own narrowing gives %v, the matcher %v", ids(only.Items), ids(res.Items))
	}
}

func TestStagesNameWhatExcludedTheRest(t *testing.T) {
	_, err := apply(t, vendors.UseRef{Tags: map[string]string{"env": "prod"}, Match: common.ResourceMatch{Zone: []string{"z9"}}})
	nm, ok := errors.AsType[*selector.NoMatchError](err)
	if !ok {
		t.Fatalf("%v", err)
	}
	want := "nothing matches: 4 listed, 3 match tags map[env:prod], 3 match match.state [available], 3 match match.includeDeprecated [false]"
	_ = want
	msg := nm.Error()
	for _, frag := range []string{"4 listed", "match tags", "0 match match.zone [z9]"} {
		if !strings.Contains(msg, frag) {
			t.Errorf("%q does not contain %q", msg, frag)
		}
	}
	if strings.Contains(msg, "match.cidr") || strings.HasSuffix(msg, "0 match match.zone [z9]") == false {
		t.Errorf("stages after the one that emptied the list are noise: %q", msg)
	}
}

func TestIDsMissingAndMismatched(t *testing.T) {
	res, err := selector.Apply(vendors.UseRef{IDs: []string{"a", "ghost", "c"}, Tags: map[string]string{"env": "prod"}}, caps, stock, fields)
	if err != nil || !slices.Equal(ids(res.Items), []string{"a"}) {
		t.Fatalf("a missing ID must not fail the others: %v, %v", ids(res.Items), err)
	}
	if !slices.Equal(res.Missing, []string{"ghost"}) || !slices.Equal(res.Mismatched, []string{"c"}) {
		t.Errorf("missing %v, mismatched %v", res.Missing, res.Mismatched)
	}
	_, err = selector.Apply(vendors.UseRef{IDs: []string{"ghost", "c"}, Tags: map[string]string{"env": "prod"}}, caps, stock, fields)
	nm, _ := errors.AsType[*selector.NoMatchError](err)
	if nm == nil || !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "do not match: c") {
		t.Errorf("both kinds of ID must be named: %v", err)
	}
}

func TestSelect(t *testing.T) {
	flatcar := vendors.UseRef{Name: "flatcar-*"}
	for name, tc := range map[string]struct {
		ref  vendors.UseRef
		want []string
		kind string // "", "none", "ambiguous"
	}{
		"all":                       {vendors.UseRef{Name: "flatcar-*", Select: common.SelectAll}, []string{"a", "b"}, ""},
		"exactlyOne with one":       {vendors.UseRef{Name: "other", Select: common.SelectExactlyOne, Match: common.ResourceMatch{State: []string{"pending"}}}, []string{"c"}, ""},
		"exactlyOne with two":       {vendors.UseRef{Name: "flatcar-*", Select: common.SelectExactlyOne}, nil, "ambiguous"},
		"exactlyOne with none":      {vendors.UseRef{Name: "zzz", Select: common.SelectExactlyOne}, nil, "none"},
		"all with none":             {vendors.UseRef{Name: "zzz", Select: common.SelectAll}, nil, "none"},
		"newest by created":         {vendors.UseRef{Name: "flatcar-*", Select: common.SelectNewest}, []string{"b"}, ""},
		"oldest by created":         {vendors.UseRef{Name: "flatcar-*", Select: common.SelectOldest, OrderBy: "created"}, []string{"a"}, ""},
		"newest by name, natural":   {vendors.UseRef{Name: "flatcar-*", Select: common.SelectNewest, OrderBy: "name"}, []string{"b"}, ""},
		"oldest by name, natural":   {vendors.UseRef{Name: "flatcar-*", Select: common.SelectOldest, OrderBy: "name"}, []string{"a"}, ""},
		"newest by a label":         {vendors.UseRef{Tags: map[string]string{"env": "prod"}, Select: common.SelectNewest, OrderBy: "label:team"}, []string{"a"}, ""},
		"a tie":                     {vendors.UseRef{Tags: map[string]string{"env": "prod"}, Match: common.ResourceMatch{IncludeDeprecated: new(true)}, Select: common.SelectNewest}, nil, "ambiguous"},
		"a label nobody has":        {vendors.UseRef{Tags: map[string]string{"env": "prod"}, Select: common.SelectNewest, OrderBy: "label:nope"}, nil, "none"},
		"the default select is all": {flatcar, []string{"a", "b"}, ""},
	} {
		got, err := apply(t, tc.ref)
		_, none := errors.AsType[*selector.NoMatchError](err)
		amb, many := errors.AsType[*selector.AmbiguousError](err)
		switch tc.kind {
		case "none":
			if !none {
				t.Errorf("%s: %v, %v; want NoMatch", name, got, err)
			}
		case "ambiguous":
			if !many || amb.Count < 2 || len(amb.Candidates) == 0 {
				t.Errorf("%s: %v, %v; want Ambiguous with candidates", name, got, err)
			}
		default:
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("%s: %v, %v; want %v", name, got, err, tc.want)
			}
		}
	}
	// Without a creation time, newest needs an orderBy.
	noCreated := caps
	noCreated.HasCreated = false
	if _, err := selector.Apply(vendors.UseRef{Name: "flatcar-*", Select: common.SelectNewest}, noCreated, stock, fields); err == nil ||
		!strings.Contains(err.Error(), "orderBy") {
		t.Errorf("newest without a creation time or orderBy: %v", err)
	}
	// The default select comes from the declaration, exactlyOne without one.
	exact := caps
	exact.DefaultSelect = ""
	if _, err := selector.Apply(vendors.UseRef{Name: "flatcar-*"}, exact, stock, fields); !errors.As(err, new(*selector.AmbiguousError)) {
		t.Errorf("no default select must mean exactlyOne: %v", err)
	}
}

func TestTagsInThePlainStringForm(t *testing.T) {
	strCaps := vendors.SelectorCaps{Tags: vendors.TagsStrings, DefaultSelect: common.SelectAll}
	items := []item{
		{id: "1", strs: []string{"env=prod", "scarpenter-use=true"}},
		{id: "2", strs: []string{"env=dev", "env2=prod"}},
		{id: "3", strs: []string{"plain"}},
	}
	for name, tc := range map[string]struct {
		tags map[string]string
		want []string
	}{
		"pair":          {map[string]string{"env": "prod"}, []string{"1"}},
		"star":          {map[string]string{"env": "*"}, []string{"1", "2"}},
		"star is a key": {map[string]string{"plain": "*"}, nil},
		"two":           {map[string]string{"env": "dev", "env2": "prod"}, []string{"2"}},
		"reserved":      {map[string]string{"scarpenter-use": "true"}, []string{"1"}},
	} {
		res, err := selector.Apply(vendors.UseRef{Tags: tc.tags}, strCaps, items, func(i item) selector.Fields {
			return selector.Fields{ID: i.id, TagStrings: i.strs}
		})
		got := ids(res.Items)
		if tc.want == nil {
			if !errors.As(err, new(*selector.NoMatchError)) {
				t.Errorf("%s: %v, %v", name, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

func TestBuild(t *testing.T) {
	x := selector.Context{Region: "r1", Zone: "r1a", NodeClass: "nc", Channel: "stable", Version: "1.2.3",
		VendorArch: func(string) (string, bool) { return "x86", true }}
	use := &common.ResourceUse{
		IDs:        map[string]common.IDList{"*": {"g1", "g2"}, "r1": {"g2", "l1"}, "r2": {"other"}},
		Name:       "{nodeclass}-{zone}",
		Tags:       map[string]string{"{channel}": "{version}", "k": "*"},
		Match:      &common.ResourceMatch{Zone: []string{"{zone}"}, Description: "{region}*", CIDR: "10.0.0.0/24"},
		Filters:    []common.ResourceFilter{{Name: "arch", Values: []string{"{region}"}}},
		Expression: "name = {{x}}",
	}
	ref, err := selector.Build(use, x, vendors.SelectorCaps{DefaultSelect: common.SelectAll})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ref.IDs, []string{"g1", "g2", "l1"}) {
		t.Errorf("the IDs of r1 are those under * and r1, without duplicates: %v", ref.IDs)
	}
	if ref.Name != "nc-r1a" || ref.Tags["stable"] != "1.2.3" || ref.Tags["k"] != "*" || ref.Match.Zone[0] != "r1a" ||
		ref.Match.Description != "r1*" || ref.Filters[0].Values[0] != "r1" || ref.Expression != "name = {x}" {
		t.Errorf("not expanded: %+v", ref)
	}
	if ref.Select != common.SelectAll {
		t.Errorf("select = %q, want the kind's default", ref.Select)
	}
	if use.IDs["r1"][0] != "g2" || use.Name != "{nodeclass}-{zone}" {
		t.Error("Build must not modify the NodeClass's block")
	}
	use.Select = common.SelectNewest
	if ref, _ := selector.Build(use, x, vendors.SelectorCaps{DefaultSelect: common.SelectAll}); ref.Select != common.SelectNewest {
		t.Errorf("an explicit select wins: %q", ref.Select)
	}

	// An ids map without an entry for the region is an error, never "default".
	if _, err := selector.Build(&common.ResourceUse{IDs: map[string]common.IDList{"r2": {"x"}}}, x, caps); err == nil ||
		!strings.Contains(err.Error(), "no IDs are configured for the region r1") {
		t.Errorf("no IDs for the region: %v", err)
	}
	if ref, err := selector.Build(&common.ResourceUse{IDs: map[string]common.IDList{"*": {"x"}}}, x, caps); err != nil || !slices.Equal(ref.IDs, []string{"x"}) {
		t.Errorf("a global ID: %v, %v", ref.IDs, err)
	}
	for name, u := range map[string]*common.ResourceUse{
		"nil":                           nil,
		"empty":                         {},
		"only a select":                 {Select: common.SelectAll},
		"empty tags":                    {Tags: map[string]string{}},
		"a bad placeholder":             {Name: "{nope}"},
		"a placeholder lacking a value": {Name: "{arch}"},
		"a bad cidr":                    {Match: &common.ResourceMatch{CIDR: "nope"}},
	} {
		if ref, err := selector.Build(u, x, caps); err == nil {
			t.Errorf("%s: %+v, want an error", name, ref)
		}
	}
	if !selector.UsesZone(use) || selector.UsesZone(&common.ResourceUse{Name: "{region}"}) || selector.UsesZone(&common.ResourceUse{Name: "{{zone}}"}) {
		t.Error("UsesZone")
	}
	if selector.Selects(&common.ResourceUse{}) || !selector.Selects(&common.ResourceUse{Name: "x"}) || selector.Selects(nil) {
		t.Error("Selects")
	}
	if got := selector.IDsFor(map[string]common.IDList{"*": {"a"}, "r": {"a", "b"}}, "r"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("IDsFor = %v", got)
	}
}

func TestUseRefIsDefault(t *testing.T) {
	if !(vendors.UseRef{}).IsDefault() || !(vendors.UseRef{Select: common.SelectAll, OrderBy: "name"}).IsDefault() {
		t.Error("nothing that selects is the vendor's default")
	}
	for name, u := range map[string]vendors.UseRef{
		"ids": {IDs: []string{"a"}}, "name": {Name: "a"}, "tags": {Tags: map[string]string{"a": "b"}},
		"match": {Match: common.ResourceMatch{Zone: []string{"z"}}}, "filters": {Filters: []common.ResourceFilter{{Name: "a"}}},
		"expression": {Expression: "x"},
	} {
		if u.IsDefault() {
			t.Errorf("%s: is not the default", name)
		}
	}
}

func TestValidate(t *testing.T) {
	c := vendors.SelectorCaps{
		Tags: vendors.TagsMap, Name: &vendors.FieldCap{}, Expression: "gcp-filter", HasCreated: true,
		Match:   map[string]vendors.FieldCap{vendors.MatchState: {}, vendors.MatchCreatedAfter: {}},
		Filters: []vendors.FilterCap{{Name: "status"}, {Name: "labels.*"}},
	}
	for name, tc := range map[string]struct {
		use  common.ResourceUse
		want string
	}{
		"ok":                {common.ResourceUse{Name: "a", Tags: map[string]string{"a": "b"}, Expression: "x", Select: common.SelectNewest}, ""},
		"filter by prefix":  {common.ResourceUse{Filters: []common.ResourceFilter{{Name: "labels.env", Values: []string{"a"}}, {Name: "status", Values: []string{"a"}}}}, ""},
		"a bare prefix":     {common.ResourceUse{Filters: []common.ResourceFilter{{Name: "labels.", Values: []string{"a"}}}}, `filters[0]: "labels." is not a images filter`},
		"unknown filter":    {common.ResourceUse{Filters: []common.ResourceFilter{{Name: "statuss", Values: []string{"a"}}}}, `filters[0]: "statuss" is not a images filter of v (labels.*, status)`},
		"unknown match":     {common.ResourceUse{Match: &common.ResourceMatch{Zone: []string{"z"}}}, "match.zone is not supported: images of v is not zonal"},
		"unsupported match": {common.ResourceUse{Match: &common.ResourceMatch{Owner: []string{"z"}}}, "match.owner is not supported for images of v (supported: createdAfter, state)"},
		"no creation time":  {common.ResourceUse{Name: "a", OrderBy: "created", Select: common.SelectNewest}, ""},
		"bad cidr":          {common.ResourceUse{Match: &common.ResourceMatch{State: []string{"available"}, CIDR: "x"}}, "is not a CIDR"},
		"label order":       {common.ResourceUse{Name: "a", Select: common.SelectOldest, OrderBy: "label:version"}, ""},
	} {
		got := errors.Join(selector.Validate("v", selector.ImagesKind, "p", &tc.use, c)...)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: %v", name, got)
		case tc.want != "" && (got == nil || !strings.Contains(got.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, got, tc.want)
		}
	}
	none := vendors.SelectorCaps{}
	for name, tc := range map[string]struct {
		use  common.ResourceUse
		want string
	}{
		"no name":       {common.ResourceUse{Name: "a"}, "name is not supported for images of v (it has no name field)"},
		"no tags":       {common.ResourceUse{Tags: map[string]string{"a": "b"}}, "tags are not supported"},
		"no expression": {common.ResourceUse{Expression: "x"}, "expression is not supported"},
		"no created":    {common.ResourceUse{Match: &common.ResourceMatch{CreatedAfter: &metav1.Time{}}}, "has no creation time"},
		"no filters":    {common.ResourceUse{Filters: []common.ResourceFilter{{Name: "a", Values: []string{"b"}}}}, "accepts no filters"},
		"no order":      {common.ResourceUse{Name: "x", OrderBy: "created", Select: common.SelectNewest}, "orderBy created is not supported"},
		"no label":      {common.ResourceUse{Name: "x", OrderBy: "label:a", Select: common.SelectNewest}, "has no tags or labels"},
		"empty use":     {common.ResourceUse{}, "selects nothing"},
		"empty match":   {common.ResourceUse{Match: &common.ResourceMatch{}}, "selects nothing"},
	} {
		got := errors.Join(selector.Validate("v", selector.ImagesKind, "p", &tc.use, none)...)
		if got == nil || !strings.Contains(got.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, got, tc.want)
		}
	}
	// {arch} is for images only.
	if err := errors.Join(selector.Validate("v", "subnets", "p", &common.ResourceUse{Name: "{arch}"}, c)...); err == nil {
		t.Error("{arch} outside images")
	}
	if err := errors.Join(selector.Validate("v", selector.ImagesKind, "p", &common.ResourceUse{Name: "{arch}-{vendorArch}-{channel}"}, c)...); err != nil {
		t.Errorf("{arch} in images: %v", err)
	}
}
