/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"maps"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

const gibBytes = 1 << 30

// ResourcesWithRoot returns the capacity and allocatable resources of an
// instance type of the class with a root disk of rootBytes as its ephemeral
// storage, derived like the catalog derives them: the overhead of the class
// (reserved and eviction resources, the latter possibly a share of the disk) is
// taken off the capacity. ok is false for a type the class does not have.
func (c *Catalog) ResourcesWithRoot(className, name string, rootBytes int64) (capacity, allocatable corev1.ResourceList, ok bool) {
	it, class, ok := c.typeAndClass(className, name)
	if !ok {
		return nil, nil, false
	}
	capacity, allocatable = withRoot(it, class, rootBytes)
	return capacity, allocatable, true
}

// RootDiskFor returns the smallest root disk, in whole GiB, with which the
// type's allocatable ephemeral storage covers requests, at most the root disk
// the catalog advertises for the type. ok is false when requests ask for no
// ephemeral storage or the class does not have the type.
func (c *Catalog) RootDiskFor(className, name string, requests corev1.ResourceList) (bytes int64, ok bool) {
	want := requests[corev1.ResourceEphemeralStorage]
	if want.IsZero() {
		return 0, false
	}
	it, class, ok := c.typeAndClass(className, name)
	if !ok {
		return 0, false
	}
	advertised := it.Capacity[corev1.ResourceEphemeralStorage]
	maxGiB := (advertised.Value() + gibBytes - 1) / gibBytes
	// Allocatable grows with the disk, so the smallest size that covers the
	// requests is found by bisection.
	n := sort.Search(int(maxGiB), func(i int) bool {
		_, alloc := withRoot(it, class, int64(i+1)*gibBytes)
		got := alloc[corev1.ResourceEphemeralStorage]
		return got.Cmp(want) >= 0
	})
	return int64(min(n+1, int(maxGiB))) * gibBytes, true
}

func (c *Catalog) typeAndClass(className, name string) (*cloudprovider.InstanceType, *Class, bool) {
	it, ok := c.Type(className, name)
	if !ok {
		return nil, nil, false
	}
	c.mu.RLock()
	class := c.classes[className]
	c.mu.RUnlock()
	return it, class, class != nil
}

func withRoot(it *cloudprovider.InstanceType, class *Class, rootBytes int64) (capacity, allocatable corev1.ResourceList) {
	capacity = maps.Clone(it.Capacity)
	capacity[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(rootBytes, resource.BinarySI)
	cpu := capacity[corev1.ResourceCPU]
	oh := overhead(int(cpu.Value()), class, capacity)
	return capacity, resources.Subtract(capacity, oh.Total())
}
