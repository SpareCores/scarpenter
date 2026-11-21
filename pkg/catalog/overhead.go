/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"fmt"
	"maps"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

// ResourceNVIDIAGPU is the extended resource of NVIDIA GPUs, which the NVIDIA
// device plugin registers.
const ResourceNVIDIAGPU corev1.ResourceName = "nvidia.com/gpu"

// DefaultMaxPods is the pods capacity when a NodeClass does not set
// kubelet.maxPods.
const DefaultMaxPods = 110

// DefaultVMMemoryOverheadPercent is the share of the catalog memory a VM does
// not offer to the guest (the default of the AWS provider's vmMemoryOverhead).
const DefaultVMMemoryOverheadPercent = 7.5

// minVMMemoryOverheadMiB is the least the overhead takes from a VM:
// a 0.5 GiB VM reports 448 MiB, 12.5% less, where 7.5% would say 473.
const minVMMemoryOverheadMiB = 64

// defaultEvictionMemory is the kubelet's default hard eviction threshold for
// memory.available.
const defaultEvictionMemory = "100Mi"

// minKubeReservedMemoryMiB is the least the cap of the default kube-reserved
// memory goes down to: what the Lean footprint reserves for the kubelet and the
// system together on a 1-2 GiB node (192Mi + 128Mi). A Standard node, whose
// daemons have no memory limits, never reserves less than that, so Lean leaves
// Pods less memory than Standard nowhere above 1 GiB.
const minKubeReservedMemoryMiB = v1alpha1.LeanKubeReservedMiB + v1alpha1.LeanSystemReservedMiB

// KubeReserved returns the resources reserved for Kubernetes daemons: the
// formula of the AWS provider (pkg/providers/instancetype/types.go of
// aws/karpenter-provider-aws at commit 30c236cafacb3a23fb3a50370d09d71750b56b29,
// Apache-2.0), with the memory capped as AKS does: memory is 11 MiB per pod
// plus 255 MiB, at most a quarter of the node's memory (memoryMiB, after the VM
// overhead) but not less than 320 MiB for that cap (minKubeReservedMemoryMiB).
// The cap only matters below about 6 GiB, where the AWS formula would leave
// 1 GiB nodes with no allocatable memory; CPU is 6% of the first
// core, 1% of the second, 0.5% of the third and fourth and 0.25% of every core
// above; ephemeral storage is 1 GiB (the kubelet's own default).
func KubeReserved(cpus int, pods int64, memoryMiB int64) corev1.ResourceList {
	millicores := float64(cpus) * 1000
	var cpuOverhead float64
	for _, r := range []struct {
		start, end float64
		share      float64
	}{
		{0, 1000, 0.06},
		{1000, 2000, 0.01},
		{2000, 4000, 0.005},
		{4000, math.MaxInt32, 0.0025},
	} {
		if millicores < r.start {
			continue
		}
		span := r.end - r.start
		if millicores < r.end {
			span = millicores - r.start
		}
		cpuOverhead += span * r.share
	}
	memory := min(11*pods+255, max(memoryMiB/4, minKubeReservedMemoryMiB))
	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewMilliQuantity(int64(cpuOverhead), resource.DecimalSI),
		corev1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dMi", memory)),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
}

// capacity returns the resources of a server for a NodeClass: cpu, memory
// reduced by the VM overhead (by at least 64 MiB: below
// 0.85 GiB the percentage alone overestimates what the guest reports),
// pods and the root disk of the vendor's default
// settings as ephemeral storage (omitted when the driver does not say), and
// nvidia.com/gpu for a type with a driver selection (the device plugin the
// chart ships makes it appear on the node).
func capacity(vcpus int, memoryMiB float64, class *Class, rootDiskBytes int64, rootDiskKnown bool,
	gpu *nvidia.Selection) corev1.ResourceList {
	overhead := class.VMMemoryOverheadPercent
	memory := math.Floor(memoryMiB * (1 - overhead/100))
	memory = min(memory, memoryMiB-minVMMemoryOverheadMiB)
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(vcpus), resource.DecimalSI),
		corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", int64(memory))),
		corev1.ResourcePods:   *resource.NewQuantity(class.MaxPods, resource.DecimalSI),
	}
	if rootDiskKnown && rootDiskBytes > 0 {
		rl[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(rootDiskBytes, resource.BinarySI)
	}
	if gpu != nil {
		rl[ResourceNVIDIAGPU] = *resource.NewQuantity(gpu.Count, resource.DecimalSI)
	}
	return rl
}

// overhead returns the node overhead for a NodeClass: the kube-reserved
// formula, where a key the NodeClass sets replaces the computed value of that
// resource; the NodeClass's system-reserved; and the hard eviction thresholds
// (memory.available defaults to 100Mi) for the resources they cover. With the
// Lean footprint the memory of the three defaults to v1alpha1.LeanMemory of the
// node's memory, which the kubelet configuration reads too
// (KubeletSpec.EffectiveFor).
func overhead(vcpus int, class *Class, capacity corev1.ResourceList) *cloudprovider.InstanceTypeOverhead {
	memory := capacity[corev1.ResourceMemory]
	nodeMiB := memory.Value() >> 20
	kube := KubeReserved(vcpus, class.MaxPods, nodeMiB)
	system := corev1.ResourceList{}
	evictionMemory := defaultEvictionMemory
	if class.Lean {
		kubeMiB, systemMiB, evictionMiB := v1alpha1.LeanMemory(nodeMiB)
		kube[corev1.ResourceMemory] = resource.MustParse(fmt.Sprintf("%dMi", kubeMiB))
		system[corev1.ResourceMemory] = resource.MustParse(fmt.Sprintf("%dMi", systemMiB))
		evictionMemory = fmt.Sprintf("%dMi", evictionMiB)
	}
	maps.Copy(kube, class.KubeReserved)
	maps.Copy(system, class.SystemReserved)
	return &cloudprovider.InstanceTypeOverhead{
		KubeReserved:      kube,
		SystemReserved:    system,
		EvictionThreshold: evictionThreshold(class.EvictionHard, evictionMemory, capacity),
	}
}

// evictionSignals maps the eviction signals that reserve a resource to it; the
// inode and pid signals reserve nothing the scheduler counts.
var evictionSignals = map[string]corev1.ResourceName{
	"memory.available":   corev1.ResourceMemory,
	"nodefs.available":   corev1.ResourceEphemeralStorage,
	"imagefs.available":  "",
	"nodefs.inodesFree":  "",
	"imagefs.inodesFree": "",
	"pid.available":      "",
}

func evictionThreshold(
	thresholds map[string]string, defaultMemory string, capacity corev1.ResourceList,
) corev1.ResourceList {
	out := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(defaultMemory)}
	for signal, value := range thresholds {
		name := evictionSignals[signal]
		if name == "" {
			continue
		}
		q, percent, isPercent, err := parseThreshold(value)
		if err != nil {
			// ClassFromNodeClass rejects what does not parse.
			continue
		}
		if isPercent {
			have := capacity[name]
			q = *resource.NewQuantity(int64(float64(have.Value())*percent/100), resource.BinarySI)
		}
		out[name] = q
	}
	return out
}
