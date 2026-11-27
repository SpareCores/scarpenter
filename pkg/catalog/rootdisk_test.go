/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/catalog"
)

const gib = int64(1) << 30

// rootDiskCatalog is the synthetic catalog advertising 1000 GiB of root disk
// for every type, as a NodeClass with rootVolume.maxSize does, under a class
// with the given hard eviction thresholds.
func rootDiskCatalog(t *testing.T, evictionHard map[string]string) *catalog.Catalog {
	t.Helper()
	facts := syntheticFacts()
	facts.RootDisk = map[string]int64{"v": 1000 * gib}
	class := catalog.NewClass("c", catalog.Scope{"v": nil})
	class.EvictionHard = evictionHard
	c := catalog.New(catalog.Options{Source: &fakeSource{data: synthetic()}, Facts: facts})
	c.SetClasses([]catalog.Class{class})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func ephemeral(gibs int64) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceEphemeralStorage: *resource.NewQuantity(gibs*gib, resource.BinarySI)}
}

// TestRootDiskFor: the smallest whole-GiB root disk whose allocatable ephemeral
// storage covers the request: the 1 GiB the kubelet reserves comes on top, a
// share-of-disk eviction threshold scales it, and the advertised size is the cap.
func TestRootDiskFor(t *testing.T) {
	tests := []struct {
		name     string
		eviction map[string]string
		request  corev1.ResourceList
		wantGiB  int64
		wantOK   bool
	}{
		{"request plus the reserved 1 GiB", nil, ephemeral(600), 601, true},
		{"a small request", nil, ephemeral(1), 2, true},
		{"a request above the advertised size is capped", nil, ephemeral(2000), 1000, true},
		{"no ephemeral-storage request", nil, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, 0, false},
		{"eviction share of the disk", map[string]string{"nodefs.available": "10%"}, ephemeral(89), 100, true},
		{"eviction quantity", map[string]string{"nodefs.available": "5Gi"}, ephemeral(94), 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := rootDiskCatalog(t, tt.eviction)
			got, ok := c.RootDiskFor("c", "v.small", tt.request)
			if ok != tt.wantOK || got != tt.wantGiB*gib {
				t.Errorf("RootDiskFor = %d GiB, %v; want %d GiB, %v", got/gib, ok, tt.wantGiB, tt.wantOK)
			}
			if !ok {
				return
			}
			// The size found covers the request, one GiB less does not (unless capped).
			_, alloc, _ := c.ResourcesWithRoot("c", "v.small", got)
			have := alloc[corev1.ResourceEphemeralStorage]
			want := tt.request[corev1.ResourceEphemeralStorage]
			if got < 1000*gib && have.Cmp(want) < 0 {
				t.Errorf("allocatable %s does not cover %s", have.String(), want.String())
			}
			_, less, _ := c.ResourcesWithRoot("c", "v.small", got-gib)
			haveLess := less[corev1.ResourceEphemeralStorage]
			if got < 1000*gib && haveLess.Cmp(want) >= 0 {
				t.Errorf("a disk one GiB smaller already covers the request")
			}
		})
	}
}

func TestResourcesWithRoot(t *testing.T) {
	c := rootDiskCatalog(t, nil)
	it, _ := c.Type("c", "v.small")
	capacity, allocatable, ok := c.ResourcesWithRoot("c", "v.small", 200*gib)
	if !ok {
		t.Fatal("no resources")
	}
	if got := capacity[corev1.ResourceEphemeralStorage]; got.Value() != 200*gib {
		t.Errorf("capacity ephemeral-storage = %s, want 200Gi", got.String())
	}
	if got := allocatable[corev1.ResourceEphemeralStorage]; got.Value() != 199*gib {
		t.Errorf("allocatable ephemeral-storage = %s, want 199Gi", got.String())
	}
	mem, itMem, allocMem, itAllocMem := capacity[corev1.ResourceMemory], it.Capacity[corev1.ResourceMemory],
		allocatable[corev1.ResourceMemory], it.Allocatable()[corev1.ResourceMemory]
	if mem.Cmp(itMem) != 0 || allocMem.Cmp(itAllocMem) != 0 {
		t.Error("the other resources differ from the type's")
	}
	if got := it.Capacity[corev1.ResourceEphemeralStorage]; got.Value() != 1000*gib {
		t.Errorf("the catalog's type changed: %s", got.String())
	}
	if _, _, ok := c.ResourcesWithRoot("c", "v.nosuch", gib); ok {
		t.Error("resources for an unknown type")
	}
}
