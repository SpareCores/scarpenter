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
)

// DefaultMaxPods is the pods capacity when a NodeClass does not set
// kubelet.maxPods.
const DefaultMaxPods = 110

// DefaultVMMemoryOverheadPercent is the share of the catalog memory a VM does
// not offer to the guest (the default of the AWS provider's vmMemoryOverhead).
const DefaultVMMemoryOverheadPercent = 7.5

// defaultEvictionMemory is the kubelet's default hard eviction threshold for
// memory.available.
const defaultEvictionMemory = "100Mi"

// KubeReserved returns the resources reserved for Kubernetes daemons: the
// formula of the AWS provider (pkg/providers/instancetype/types.go of
// aws/karpenter-provider-aws at commit 30c236cafacb3a23fb3a50370d09d71750b56b29,
// Apache-2.0): memory is 11 MiB per pod plus 255 MiB; CPU is 6% of the first
// core, 1% of the second, 0.5% of the third and fourth and 0.25% of every core
// above; ephemeral storage is 1 GiB (the kubelet's own default).
func KubeReserved(cpus int, pods int64) corev1.ResourceList {
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
	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewMilliQuantity(int64(cpuOverhead), resource.DecimalSI),
		corev1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dMi", 11*pods+255)),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
}

// capacity returns the resources of a server for a NodeClass: cpu, memory
// reduced by the VM overhead, pods and the root disk of the vendor's default
// settings as ephemeral storage (omitted when the driver does not say). No GPU
// resources: without a device plugin they would never appear on the node.
func capacity(vcpus int, memoryMiB float64, class *Class, rootDiskBytes int64, rootDiskKnown bool) corev1.ResourceList {
	overhead := class.VMMemoryOverheadPercent
	memory := math.Floor(memoryMiB * (1 - overhead/100))
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(vcpus), resource.DecimalSI),
		corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", int64(memory))),
		corev1.ResourcePods:   *resource.NewQuantity(class.MaxPods, resource.DecimalSI),
	}
	if rootDiskKnown && rootDiskBytes > 0 {
		rl[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(rootDiskBytes, resource.BinarySI)
	}
	return rl
}

// overhead returns the node overhead for a NodeClass: the kube-reserved
// formula, where a key the NodeClass sets replaces the computed value of that
// resource; the NodeClass's system-reserved; and the hard eviction thresholds
// (memory.available defaults to 100Mi) for the resources they cover.
func overhead(vcpus int, class *Class, capacity corev1.ResourceList) *cloudprovider.InstanceTypeOverhead {
	kube := KubeReserved(vcpus, class.MaxPods)
	maps.Copy(kube, class.KubeReserved)
	return &cloudprovider.InstanceTypeOverhead{
		KubeReserved:      kube,
		SystemReserved:    class.SystemReserved,
		EvictionThreshold: evictionThreshold(class.EvictionHard, capacity),
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

func evictionThreshold(thresholds map[string]string, capacity corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(defaultEvictionMemory)}
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
