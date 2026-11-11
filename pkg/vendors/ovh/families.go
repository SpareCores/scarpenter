/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"slices"
	"strings"
)

// Family returns the flavor family of an API reference, the part before the
// first dash: b3 for b3-8, c3 for c3-16-flex, t1 for t1-le-45.
func Family(apiReference string) string {
	f, _, _ := strings.Cut(apiReference, "-")
	return f
}

// Launched families: the types the driver launches (plan 18, "Catalog"): general
// purpose b2/b3, CPU c2/c3, memory r2/r3, Discovery d2 (shared CPU) and the
// storage-optimised i1, with the -flex variants (their API reference is the
// flavor name, so they are separate types).
var launchedFamilies = []string{"b2", "b3", "c2", "c3", "r2", "r3", "d2", "i1"}

// Excluded families and why. The conformance suite's UnknownQuotaTypes holds
// exactly these; a test asserts every Keeper OVH family of the snapshot is in
// one list or the other.
var (
	// GPU families: any server with accelerators; the driver has no NVIDIA
	// sysext path, and their quota is separate.
	gpuFamilies = []string{"t1", "t2", "a10", "a100", "h100", "h200", "l4", "l40s", "rtx5000", "g1", "g2", "g3"}
	// Metal (Ironic bare metal); booting our image there is unverified.
	metalFamilies = []string{"bm"}
	// Keeper's inactive legacy families.
	legacyFamilies = []string{"eg", "hg", "s1", "sp", "vps"}
)

// Why a family is not launched.
const (
	ReasonGPU    = "gpu"
	ReasonMetal  = "metal"
	ReasonLegacy = "legacy"
)

// Launchable reports whether the driver launches the type, and, when it does
// not, why ("" for a family nobody classified yet, which a test makes fail).
func Launchable(apiReference string) (ok bool, reason string) {
	f := Family(apiReference)
	switch {
	case slices.Contains(launchedFamilies, f):
		return true, ""
	case slices.Contains(gpuFamilies, f):
		return false, ReasonGPU
	case slices.Contains(metalFamilies, f):
		return false, ReasonMetal
	case slices.Contains(legacyFamilies, f):
		return false, ReasonLegacy
	}
	return false, ""
}

// ExcludedFamilies returns every family the driver knows and does not launch
// (the conformance UnknownQuotaTypes families), sorted.
func ExcludedFamilies() []string {
	out := slices.Concat(gpuFamilies, metalFamilies, legacyFamilies)
	slices.Sort(out)
	return out
}

// LaunchedFamilies returns the launched families.
func LaunchedFamilies() []string { return slices.Clone(launchedFamilies) }

// RootDiskGB is the size of the local root disk of a launched flavor as Keeper
// describes it: storage_size for b, c, r and d2, but for i1 storage_size includes
// the local NVMe disks (1950 to 7650 GB), so the root is the 50 GB entry of the
// storages list (the smallest). Keeper's storages are in GB.
func RootDiskGB(apiReference string, storageSize float64, storages []float64) float64 {
	if Family(apiReference) == "i1" && len(storages) > 0 {
		return slices.Min(storages)
	}
	return storageSize
}
