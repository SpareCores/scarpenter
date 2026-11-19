/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/fake"
)

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

// TestLeanOverhead checks the Lean footprint in the catalog: its values are the
// ones of the preset, systemReserved and the eviction threshold count in the
// allocatable, and what the NodeClass sets overrides the preset.
func TestLeanOverhead(t *testing.T) {
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "lean"},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{Fake: &fake.Settings{}}},
	}
	nc.Spec.Kubelet.Footprint = v1alpha1.FootprintLean
	class, err := ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	if class.MaxPods != 10 {
		t.Errorf("lean maxPods = %d, want 10", class.MaxPods)
	}
	rl := capacity(1, 1024, &class, 0, false)
	oh := overhead(1, &class, rl)
	for name, want := range map[string]struct {
		got  resource.Quantity
		want string
	}{
		"kube-reserved memory":   {*oh.KubeReserved.Memory(), "192Mi"},
		"system-reserved memory": {*oh.SystemReserved.Memory(), "128Mi"},
		"eviction memory":        {*oh.EvictionThreshold.Memory(), "50Mi"},
	} {
		if want.got.String() != want.want {
			t.Errorf("%s = %s, want %s", name, want.got.String(), want.want)
		}
	}
	// 1 GiB: 947Mi - 192Mi - 128Mi - 50Mi.
	allocatable := rl[corev1.ResourceMemory]
	allocatable.Sub(*oh.KubeReserved.Memory())
	allocatable.Sub(*oh.SystemReserved.Memory())
	allocatable.Sub(*oh.EvictionThreshold.Memory())
	if got := allocatable.Value() >> 20; got != 577 {
		t.Errorf("lean allocatable memory on 1 GiB = %dMi, want 577Mi", got)
	}

	// What the NodeClass sets wins, key by key.
	nc.Spec.Kubelet.KubeReserved = map[string]string{"memory": "300Mi"}
	maxPods := int32(20)
	nc.Spec.Kubelet.MaxPods = &maxPods
	nc.Spec.Kubelet.EvictionHard = map[string]string{"memory.available": "80Mi"}
	if class, err = ClassFromNodeClass(nc); err != nil {
		t.Fatal(err)
	}
	oh = overhead(1, &class, rl)
	if class.MaxPods != 20 || oh.KubeReserved.Memory().String() != "300Mi" ||
		oh.EvictionThreshold.Memory().String() != "80Mi" || oh.SystemReserved.Memory().String() != "128Mi" {
		t.Errorf("overrides: %d pods, %v, %v, %v", class.MaxPods, oh.KubeReserved, oh.EvictionThreshold, oh.SystemReserved)
	}
}
