/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"maps"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestLabelValue(t *testing.T) {
	long := strings.Repeat("a", 80)
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"scarpenter", "scarpenter"},
		{"a.b_c-d", "a.b_c-d"},
		{strings.Repeat("x", 63), strings.Repeat("x", 63)},
	} {
		if got := LabelValue(tc.in); got != tc.want {
			t.Errorf("LabelValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{long, "has space", "-leading", "trailing-", "uniécode", strings.Repeat("y", 64)} {
		got := LabelValue(in)
		if len(got) > 63 || !labelValueRE.MatchString(got) || got == "" {
			t.Errorf("LabelValue(%q) = %q is not a valid value", in, got)
		}
		if got == in {
			t.Errorf("LabelValue(%q) should have changed it", in)
		}
		if LabelValue(in) != got {
			t.Errorf("LabelValue(%q) is not deterministic", in)
		}
	}
	if got := LabelValue(long); got != strings.Repeat("a", 50)+"-"+shortHash(long) {
		t.Errorf("long value: %q", got)
	}
}

// TestLabelValueCollisions: names with the same 50-character prefix map apart,
// and a changed character mapping to the same sanitized text does too.
func TestLabelValueCollisions(t *testing.T) {
	prefix := strings.Repeat("p", 60)
	seen := map[string]string{}
	for i := range 2000 {
		in := prefix + strings.Repeat("0", i%7) + string(rune('a'+i%26)) + strings.Repeat("z", i/26)
		got := LabelValue(in)
		if prev, dup := seen[got]; dup && prev != in {
			t.Fatalf("%q and %q both map to %q", prev, in, got)
		}
		seen[got] = in
	}
	if LabelValue("a b") == LabelValue("a-b") {
		t.Error("a sanitized value must not collide with its valid look-alike")
	}
}

func TestValidLabelKey(t *testing.T) {
	for k, want := range map[string]bool{
		"scarpenter.sparecores.com/cluster": true,
		"os":                                true,
		"a/b":                               true,
		"":                                  false,
		"/b":                                false,
		"a/":                                false,
		"a/b/c":                             false,
		"UPPER.example.com/x":               false,
		"-bad":                              false,
		"has space":                         false,
		strings.Repeat("n", 64):             false,
		"p/" + strings.Repeat("n", 63):      true,
	} {
		if got := ValidLabelKey(k); got != want {
			t.Errorf("ValidLabelKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestLabelsAndSelector(t *testing.T) {
	tags := map[vendors.TagKey]string{vendors.TagCluster: "c1", vendors.TagNodeClaim: strings.Repeat("n", 70)}
	got, err := Labels(tags, map[string]string{"team": "a", "scarpenter.sparecores.com/cluster": "evil"})
	if err != nil {
		t.Fatal(err)
	}
	if got["scarpenter.sparecores.com/cluster"] != "c1" || got["team"] != "a" {
		t.Errorf("labels %v (a logical tag must win over an extra)", got)
	}
	if v := got["scarpenter.sparecores.com/nodeclaim"]; len(v) > 63 {
		t.Errorf("nodeclaim label %q too long", v)
	}
	if _, err := Labels(nil, map[string]string{"bad key": "x"}); err == nil {
		t.Error("an invalid extra key is an error")
	}
	sel := Selector(tags)
	want := "scarpenter.sparecores.com/cluster=c1,scarpenter.sparecores.com/nodeclaim=" + LabelValue(strings.Repeat("n", 70))
	if sel != want {
		t.Errorf("selector %q, want %q", sel, want)
	}
	if Selector(nil) != "" {
		t.Error("empty selector")
	}
	if KeyPresent(vendors.TagCluster) != "scarpenter.sparecores.com/cluster" {
		t.Error("KeyPresent")
	}
	if !HasTags(got, tags) || HasTags(got, map[vendors.TagKey]string{vendors.TagCluster: "c2"}) || HasTags(got, map[vendors.TagKey]string{vendors.TagNodePool: "x"}) {
		t.Error("HasTags")
	}
}

func TestTagsOfRoundTrip(t *testing.T) {
	in := map[vendors.TagKey]string{
		vendors.TagCluster: "c", vendors.TagNodeClaim: "nc", vendors.TagNodePool: "np", vendors.TagNodeClass: "cls",
		vendors.TagManagedBy: "scarpenter", vendors.TagIdempotencyKey: "0b8f2a3e-1111-2222-3333-444455556666",
	}
	labels, err := Labels(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := TagsOf(labels); !maps.Equal(got, in) {
		t.Errorf("got %v", got)
	}
}

func TestServerName(t *testing.T) {
	for _, in := range []string{"node-1", "a", strings.Repeat("a", 63)} {
		if got := ServerName(in); got != in {
			t.Errorf("ServerName(%q) = %q", in, got)
		}
	}
	for _, in := range []string{strings.Repeat("a", 64), "has.dot", "Upper", "under_score", "-x"} {
		got := ServerName(in)
		if len(got) > MaxServerName || !hostLabelRE.MatchString(got) || got == in {
			t.Errorf("ServerName(%q) = %q", in, got)
		}
	}
	a, b := ServerName(strings.Repeat("a", 70)+"1"), ServerName(strings.Repeat("a", 70)+"2")
	if a == b {
		t.Error("two long names with the same prefix collide")
	}
	if got := ResourceName(strings.Repeat("v", 70), MaxVolumeName); len(got) > MaxVolumeName {
		t.Errorf("volume name %q", got)
	}
	if got := ResourceName("scarpenter-c-volumes-fsn1", MaxVolumeName); got != "scarpenter-c-volumes-fsn1" {
		t.Errorf("short resource name changed: %q", got)
	}
}
