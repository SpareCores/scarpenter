/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import "testing"

// TestKubeReservedMemoryCap pins the default kube-reserved memory,
// min(255Mi + 11Mi * maxPods, 25% of the node's memory), and the resulting
// allocatable at the sizes of the instance types, with 110 pods (the default)
// and 10 pods, for a catalog memory of N GiB less the default VM overhead.
func TestKubeReservedMemoryCap(t *testing.T) {
	for _, tc := range []struct {
		gib         float64
		capacityMiB int64 // after the 7.5% VM overhead
		pods        int64
		reservedMiB int64
	}{
		{1, 947, 110, 236},     // 25% of 947Mi, not 1465Mi
		{2, 1894, 110, 473},    // 25% of 1894Mi
		{4, 3788, 110, 947},    // 25% of 3788Mi
		{8, 7577, 110, 1465},   // the formula: below 25% (1894Mi)
		{16, 15155, 110, 1465}, // the formula
		{1, 947, 10, 236},      // 255Mi + 110Mi = 365Mi, over 25%
		{4, 3788, 10, 365},     // the formula
	} {
		class := NewClass("c", Scope{"fake": nil})
		class.MaxPods = tc.pods
		rl := capacity(2, tc.gib*1024, &class, 0, false)
		if got := rl.Memory().Value() >> 20; got != tc.capacityMiB {
			t.Fatalf("%v GiB: capacity = %dMi, want %dMi", tc.gib, got, tc.capacityMiB)
		}
		oh := overhead(2, &class, rl)
		if got := oh.KubeReserved.Memory().Value() >> 20; got != tc.reservedMiB {
			t.Errorf("%v GiB, %d pods: kube-reserved memory = %dMi, want %dMi", tc.gib, tc.pods, got, tc.reservedMiB)
		}
	}
}
