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
