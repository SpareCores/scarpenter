/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package labels

import (
	"slices"

	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// attributeKeys are the fixed attribute label keys (00a section 7.1), in a
// stable order. cpu-flag-<flag> keys follow from the NodeClass.
var attributeKeys = []string{
	v1alpha1.LabelVendor, v1alpha1.LabelAPIReference, v1alpha1.LabelFamily,
	v1alpha1.LabelVCPUs, v1alpha1.LabelCPUCores, v1alpha1.LabelMemoryMiB, v1alpha1.LabelMemoryPerVCPUMiB,
	v1alpha1.LabelCPUManufacturer, v1alpha1.LabelCPUFamily, v1alpha1.LabelCPUAllocation,
	v1alpha1.LabelCPUClockMHz, v1alpha1.LabelCPUL2PerCoreKiB, v1alpha1.LabelCPUL3MiB, v1alpha1.LabelHyperthreading,
	v1alpha1.LabelAcceleratorCount, v1alpha1.LabelAcceleratorMemoryMiB, v1alpha1.LabelAcceleratorManufacturer,
	v1alpha1.LabelAcceleratorModel, v1alpha1.LabelAcceleratorType,
	v1alpha1.LabelNetworkBaselineMbps, v1alpha1.LabelNetworkPeakMbps,
	v1alpha1.LabelLocalStorageGB, v1alpha1.LabelLocalStorageCount,
}

// AttributeKeys returns every attribute label key of an instance type for the
// given CPU flags.
func AttributeKeys(cpuFlags []string) []string {
	keys := slices.Clone(attributeKeys)
	for _, f := range cpuFlags {
		keys = append(keys, v1alpha1.CPUFlagLabel(f))
	}
	return keys
}

// Attributes returns the attribute label requirements of a server: one
// requirement for every key of AttributeKeys, DoesNotExist where the fact is
// unknown. An API reference that is not a valid label value is not a label (the
// key is DoesNotExist); the catalog's lookup still resolves such a type.
func Attributes(s *model.Server, cpuFlags []string) []*scheduling.Requirement {
	out := make([]*scheduling.Requirement, 0, len(attributeKeys)+len(cpuFlags))
	add := func(r *scheduling.Requirement) { out = append(out, r) }

	add(Enum(v1alpha1.LabelVendor, s.Vendor))
	if v, ok := Value(s.APIReference); ok && v == s.APIReference {
		add(In(v1alpha1.LabelAPIReference, v))
	} else {
		add(Absent(v1alpha1.LabelAPIReference))
	}
	add(Enum(v1alpha1.LabelFamily, s.Family))

	add(Int(v1alpha1.LabelVCPUs, float64(s.VCPUs)))
	if s.CPUCores != nil && *s.CPUCores > 0 {
		add(Int(v1alpha1.LabelCPUCores, float64(*s.CPUCores)))
	} else {
		add(Absent(v1alpha1.LabelCPUCores))
	}
	add(Int(v1alpha1.LabelMemoryMiB, s.MemoryMiB))
	if s.VCPUs > 0 {
		add(Int(v1alpha1.LabelMemoryPerVCPUMiB, s.MemoryMiB/float64(s.VCPUs)))
	} else {
		add(Absent(v1alpha1.LabelMemoryPerVCPUMiB))
	}

	add(Enum(v1alpha1.LabelCPUManufacturer, s.CPUManufacturer))
	add(Enum(v1alpha1.LabelCPUFamily, s.CPUFamily))
	add(Enum(v1alpha1.LabelCPUAllocation, s.CPUAllocation))
	add(positive(v1alpha1.LabelCPUClockMHz, s.CPUSpeedGHz, 1000))
	add(positive(v1alpha1.LabelCPUL2PerCoreKiB, s.CPUL2PerCoreKiB, 1))
	add(positive(v1alpha1.LabelCPUL3MiB, s.CPUL3KiB, 1.0/1024))
	// More vCPUs than cores means simultaneous multithreading.
	if s.CPUCores != nil && *s.CPUCores > 0 {
		add(Bool(v1alpha1.LabelHyperthreading, s.VCPUs > *s.CPUCores))
	} else {
		add(Absent(v1alpha1.LabelHyperthreading))
	}

	if s.AcceleratorCount > 0 {
		add(Int(v1alpha1.LabelAcceleratorCount, s.AcceleratorCount))
		add(positive(v1alpha1.LabelAcceleratorMemoryMiB, s.AcceleratorMemoryMiB, 1))
		add(Enum(v1alpha1.LabelAcceleratorManufacturer, s.AcceleratorManufacturer))
		add(Enum(v1alpha1.LabelAcceleratorModel, s.AcceleratorModel))
		add(Enum(v1alpha1.LabelAcceleratorType, s.AcceleratorType))
	} else {
		for _, k := range []string{
			v1alpha1.LabelAcceleratorCount, v1alpha1.LabelAcceleratorMemoryMiB, v1alpha1.LabelAcceleratorManufacturer,
			v1alpha1.LabelAcceleratorModel, v1alpha1.LabelAcceleratorType,
		} {
			add(Absent(k))
		}
	}

	add(positive(v1alpha1.LabelNetworkBaselineMbps, s.NetworkBaselineGbps, 1000))
	add(positive(v1alpha1.LabelNetworkPeakMbps, s.NetworkPeakGbps, 1000))
	// No bundled disk is a fact (0), not an unknown.
	add(Int(v1alpha1.LabelLocalStorageGB, s.LocalStorageGB))
	switch {
	case s.LocalStorageGB <= 0:
		add(Int(v1alpha1.LabelLocalStorageCount, 0))
	case s.LocalStorageCount > 0:
		add(Int(v1alpha1.LabelLocalStorageCount, float64(s.LocalStorageCount)))
	default:
		add(Absent(v1alpha1.LabelLocalStorageCount))
	}

	// An empty flag list means the type was not inspected: unknown, not "no
	// flags" (the semantics of the selection engine).
	for _, f := range cpuFlags {
		key := v1alpha1.CPUFlagLabel(f)
		if len(s.CPUFlags) == 0 {
			add(Absent(key))
			continue
		}
		add(Bool(key, slices.Contains(s.CPUFlags, f)))
	}
	return out
}

// positive is Int of v*scale for a known, positive value, DoesNotExist else.
func positive(key string, v *float64, scale float64) *scheduling.Requirement {
	if v == nil || *v <= 0 {
		return Absent(key)
	}
	r := Int(key, *v*scale)
	// A tiny positive value must not round to the label "0".
	if r.Has("0") {
		return Int(key, 1)
	}
	return r
}
