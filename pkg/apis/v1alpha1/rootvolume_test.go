/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

func TestRootVolumeMinBytes(t *testing.T) {
	const gib = int64(1) << 30
	q := func(s string) *resource.Quantity { return new(resource.MustParse(s)) }
	tests := []struct {
		name      string
		spec      *v1alpha1.RootVolumeSpec
		memoryMiB float64
		wantGiB   int64
	}{
		{"unset", nil, 1 << 20, 0},
		{"no multiplier", &v1alpha1.RootVolumeSpec{}, 1 << 20, 0},
		{"1 TiB of memory, multiplier 1", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("1")}, 1 << 20, 1024},
		{"16 GiB of memory, multiplier 1", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("1")}, 16 << 10, 16},
		{"multiplier 1.5", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("1500m")}, 16 << 10, 24},
		{"rounds up", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("0.3")}, 1000, 1},
		{"exact product is not rounded up", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("0.3")}, 10 << 10, 3},
		{"above the cap gets the cap", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("1"), MemoryMaxSize: q("16Ti")}, 32 << 20, 16384},
		{"below the cap is untouched", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("1"), MemoryMaxSize: q("16Ti")}, 1 << 20, 1024},
		{"cap in whole GiB, rounded down", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("1"), MemoryMaxSize: q("1500Mi")}, 1 << 20, 1},
		{"unknown memory", &v1alpha1.RootVolumeSpec{MemoryMultiplier: q("1")}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.MinBytes(tt.memoryMiB); got != tt.wantGiB*gib {
				t.Errorf("MinBytes = %d GiB, want %d", got/gib, tt.wantGiB)
			}
		})
	}
}
