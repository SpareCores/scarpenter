/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"fmt"
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/fake"
)

// TestKubeReservedMemoryCap pins the default kube-reserved memory,
// min(255Mi + 11Mi * maxPods, max(25% of the node's memory, 320Mi)), at the
// sizes of the instance types, with 110 pods (the default) and 10 pods, for a
// catalog memory of N GiB less the default VM overhead.
func TestKubeReservedMemoryCap(t *testing.T) {
	for _, tc := range []struct {
		gib         float64
		capacityMiB int64 // after the 7.5% VM overhead
		pods        int64
		reservedMiB int64
	}{
		{1, 947, 110, 320},     // 25% of 947Mi is 236Mi, below the Lean floor
		{2, 1894, 110, 473},    // 25% of 1894Mi
		{4, 3788, 110, 947},    // 25% of 3788Mi
		{8, 7577, 110, 1465},   // the formula: below 25% (1894Mi)
		{16, 15155, 110, 1465}, // the formula
		{1, 947, 10, 320},      // 255Mi + 110Mi = 365Mi, over the floor
		{1, 947, 5, 310},       // 255Mi + 55Mi, below the floor: the formula
		{4, 3788, 10, 365},     // the formula
	} {
		class := NewClass("c", Scope{"fake": nil})
		class.MaxPods = tc.pods
		rl := capacity(2, tc.gib*1024, &class, 0, false, nil)
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
	rl := capacity(1, 1024, &class, 0, false, nil)
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
	if got := leanAllocatableMiB(oh, 947); got != 577 {
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

func leanAllocatableMiB(oh *cloudprovider.InstanceTypeOverhead, nodeMiB int64) int64 {
	return nodeMiB - oh.KubeReserved.Memory().Value()>>20 - oh.SystemReserved.Memory().Value()>>20 -
		oh.EvictionThreshold.Memory().Value()>>20
}

// TestLeanOverheadBySize pins the Lean memory values at every size and the
// allocatable they leave, against the fixed 192Mi, 128Mi and 50Mi that were
// measured on nodes of 448, 951, 1957 and 3905 MiB. The kubelet
// configuration test in pkg/bootstrap has the same rows.
func TestLeanOverheadBySize(t *testing.T) {
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "lean"},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{Fake: &fake.Settings{}}},
	}
	nc.Spec.Kubelet.Footprint = v1alpha1.FootprintLean
	class, err := ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		nodeMiB, kube, system, eviction, allocMiB int64
		// before is the allocatable of the fixed 192Mi, 128Mi and 50Mi.
		before int64
	}{
		{448, 144, 96, 16, 192, 78},
		{951, 192, 128, 50, 581, 581},
		{1957, 192, 128, 50, 1587, 1587},
		{3905, 192, 244, 50, 3419, 3535},
		{65536, 192, 384, 50, 64910, 65166},
	} {
		t.Run(fmt.Sprintf("%dMi", tc.nodeMiB), func(t *testing.T) {
			rl := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", tc.nodeMiB))}
			oh := overhead(2, &class, rl)
			got := [3]int64{
				oh.KubeReserved.Memory().Value() >> 20, oh.SystemReserved.Memory().Value() >> 20,
				oh.EvictionThreshold.Memory().Value() >> 20,
			}
			if got != [3]int64{tc.kube, tc.system, tc.eviction} {
				t.Errorf("kube, system, eviction = %v, want %d, %d, %d", got, tc.kube, tc.system, tc.eviction)
			}
			alloc := leanAllocatableMiB(oh, tc.nodeMiB)
			if alloc != tc.allocMiB {
				t.Errorf("allocatable %dMi, want %dMi", alloc, tc.allocMiB)
			}
			switch {
			case tc.nodeMiB == 448 && (alloc < 150 || alloc > 200):
				t.Errorf("a 0.5 GiB node leaves %dMi, want 150-200Mi", alloc)
			case tc.nodeMiB >= 951 && tc.nodeMiB <= 1957 &&
				math.Abs(float64(alloc-tc.before)) > 0.1*float64(tc.before):
				t.Errorf("allocatable %dMi is not within 10%% of %dMi", alloc, tc.before)
			}
		})
	}
}

// TestLeanNeverLeavesLessThanStandard pins the rule the floor of the default
// reservation keeps: at every size, a Lean node leaves Pods at least as much
// memory as a Standard one with the default maxPods.
func TestLeanNeverLeavesLessThanStandard(t *testing.T) {
	allocatable := func(footprint v1alpha1.Footprint, gib float64) int64 {
		nc := &v1alpha1.SCNodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "c"},
			Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{Fake: &fake.Settings{}}},
		}
		nc.Spec.Kubelet.Footprint = footprint
		class, err := ClassFromNodeClass(nc)
		if err != nil {
			t.Fatal(err)
		}
		rl := capacity(2, gib*1024, &class, 0, false, nil)
		oh := overhead(2, &class, rl)
		m := rl[corev1.ResourceMemory]
		m.Sub(*oh.KubeReserved.Memory())
		m.Sub(*oh.SystemReserved.Memory())
		m.Sub(*oh.EvictionThreshold.Memory())
		return m.Value() >> 20
	}
	for _, gib := range []float64{0.5, 1, 1.5, 2, 4, 8, 16, 64} {
		lean, standard := allocatable(v1alpha1.FootprintLean, gib), allocatable("", gib)
		if lean < standard {
			t.Errorf("%v GiB: Lean allocatable %dMi < Standard %dMi", gib, lean, standard)
		}
	}
}
