/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import "testing"

// The bucket of a limit: Used is the sum over the ADs, the headroom the largest
// available (never negative), so Limit - Used is the best AD's room.
func TestBucketOf(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reads       []availability
		used, limit int64
	}{
		{"one region read", []availability{{used: 3, available: 7}}, 3, 10},
		{"three ADs", []availability{{used: 2, available: 8}, {used: 0, available: 20}, {used: 5, available: 0}}, 7, 27},
		{"over the limit everywhere", []availability{{used: 4, available: -1}, {used: 1, available: -2}}, 5, 5},
	} {
		b := bucketOf("standard-e5-core-count", tc.reads)
		if b.Used != tc.used || b.Limit != tc.limit || b.Unit != UnitOCPU {
			t.Errorf("%s: %+v, want used %d, limit %d", tc.name, b, tc.used, tc.limit)
		}
	}
}

// The unit of a limit name, whatever scope infix OCI puts before "-count": the names
// are the real ones of the tenancy (live service limits, 2026-11-15; the quota-exceeded
// error of run 43646354816) and of the shape table.
func TestLimitForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string
		ok   bool
	}{
		{"standard-e5-core-count", UnitOCPU, true},
		{"standard-e5-memory-count", UnitGB, true},
		{"standard-e3-core-ad-count", UnitOCPU, true},
		{"standard-e3-memory-count", UnitGB, true},
		{"standard-a1-core-regional-count", UnitOCPU, true},
		{"standard-a1-memory-regional-count", UnitGB, true},
		{"standard2-4-core-count", UnitOCPU, true},
		{"bm-standard3-64-count", UnitInstances, true},
		{"bm-gpu-a10-4-count", UnitInstances, true},
		{"standard-core-ad", "", false},
		{"standard-e5-core", "", false},
		{"", "", false},
	} {
		unit, ok := limitForm(tc.name)
		if unit != tc.unit || ok != tc.ok {
			t.Errorf("limitForm(%q) = %q, %v; want %q, %v", tc.name, unit, ok, tc.unit, tc.ok)
		}
		if got := IsCoreLimit(tc.name); got != (tc.unit == UnitOCPU) {
			t.Errorf("IsCoreLimit(%q) = %v", tc.name, got)
		}
	}
}

// The family of a limit name: the name without "-count", the scope infix and the form,
// so the OCPU and GB limits of a shape family share it.
func TestLimitFamily(t *testing.T) {
	for name, want := range map[string]string{
		"standard-e5-core-ad-count":         "standard-e5",
		"standard-e5-memory-count":          "standard-e5",
		"standard-e3-core-ad-count":         "standard-e3",
		"standard-a1-core-regional-count":   "standard-a1",
		"standard-a1-memory-regional-count": "standard-a1",
		"standard2-4-core-count":            "standard2-4",
		"bm-standard3-64-count":             "bm-standard3-64",
	} {
		if _, got, ok := limitParts(name); !ok || got != want {
			t.Errorf("limitParts(%q) family = %q, %v; want %q", name, got, ok, want)
		}
		if got := bucketOf(name, nil).Family; got != want {
			t.Errorf("bucketOf(%q).Family = %q, want %q", name, got, want)
		}
	}
}
