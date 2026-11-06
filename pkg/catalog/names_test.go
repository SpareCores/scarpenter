/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/catalog"
)

var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)

func TestInstanceTypeName(t *testing.T) {
	long := strings.Repeat("a", 80)
	cases := []struct {
		vendor, ref string
		want        string // empty: only the properties are checked
	}{
		{"aws", "m7i.large", "aws.m7i.large"},
		{"gcp", "n2-standard-4", "gcp.n2-standard-4"},
		{"azure", "Standard_D2s_v3", "azure.Standard_D2s_v3"},
		{"hcloud", "cx22", "hcloud.cx22"},
		{"aws", strings.Repeat("x", 57), "aws." + strings.Repeat("x", 57)}, // 61 characters
		{"aws", strings.Repeat("x", 59), "aws." + strings.Repeat("x", 59)}, // exactly 63
		{"v", "with space", ""},
		{"v", "slash/name", ""},
		{"v", "trailing-", ""},
		{"v", "ünïcode", ""},
		{"v", long, ""},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		got := catalog.InstanceTypeName(tc.vendor, tc.ref)
		if tc.want != "" && got != tc.want {
			t.Errorf("InstanceTypeName(%q, %q) = %q, want %q", tc.vendor, tc.ref, got, tc.want)
		}
		if !labelValue.MatchString(got) {
			t.Errorf("InstanceTypeName(%q, %q) = %q is not a label value", tc.vendor, tc.ref, got)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%q and %q share the name %q", other, tc.ref, got)
		}
		seen[got] = tc.ref
	}
}

func TestInstanceTypeNameHashing(t *testing.T) {
	long := strings.Repeat("a", 80)
	got := catalog.InstanceTypeName("v", long)
	// 56 characters of the sanitised name, a dash, six hex characters.
	if len(got) != 63 || !strings.HasPrefix(got, "v."+strings.Repeat("a", 54)) || !regexp.MustCompile(`-[0-9a-f]{6}$`).MatchString(got) {
		t.Errorf("long name = %q", got)
	}
	if got != catalog.InstanceTypeName("v", long) {
		t.Error("names are not deterministic")
	}
	// Names that differ only in the replaced character do not collide.
	if catalog.InstanceTypeName("v", "a b") == catalog.InstanceTypeName("v", "a/b") {
		t.Error("a b and a/b collide")
	}
	if got := catalog.InstanceTypeName("v", "a b"); !strings.HasPrefix(got, "v.a-b-") {
		t.Errorf("sanitised name = %q", got)
	}
	// Two long names with the same 56-character prefix do not collide either.
	if catalog.InstanceTypeName("v", long+"1") == catalog.InstanceTypeName("v", long+"2") {
		t.Error("long names with a common prefix collide")
	}
}
