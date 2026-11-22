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
// flavor name, so they are separate types), and the GPU families (plan 12, step i).
var launchedFamilies = slices.Concat([]string{"b2", "b3", "c2", "c3", "r2", "r3", "d2", "i1"}, gpuFamilies)

// gpuFamilies are the NVIDIA GPU flavors (T1 and T2 are V100, V100S and the
// like, RTX 5000, A10, A100, H100, H200, L4, L40S). The core's GPU rules (pkg/nvidia)
// decide which of their types the catalog keeps: Volta types get the proprietary
// driver, the 8-GPU h200 gets Fabric Manager. They are launched like any other
// flavor; the GPU count has no Nova quota (see the quota reader).
var gpuFamilies = []string{"t1", "t2", "a10", "a100", "h100", "h200", "l4", "l40s", "rtx5000"}

// Excluded families and why. The conformance suite's UnknownQuotaTypes holds
// exactly these; a test asserts every Keeper OVH family of the snapshot is in
// one list or the other.
var (
	// Retired GPU families (the first generations): excluded unless Keeper's
	// data shows them active again.
	retiredGPUFamilies = []string{"g1", "g2", "g3"}
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
	case slices.Contains(retiredGPUFamilies, f):
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
	out := slices.Concat(retiredGPUFamilies, metalFamilies, legacyFamilies)
	slices.Sort(out)
	return out
}

// LaunchedFamilies returns the launched families.
func LaunchedFamilies() []string { return slices.Clone(launchedFamilies) }
