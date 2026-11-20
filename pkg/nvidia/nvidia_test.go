/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nvidia_test

import (
	"slices"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

const stable = "4757.2.3"

func gpu(model string, count float64) nvidia.GPU {
	return nvidia.GPU{Manufacturer: "NVIDIA", Model: model, Count: count, AMD64: true}
}

func TestSelectByArchitectureOnTheStableRelease(t *testing.T) {
	tests := []struct {
		model   string
		branch  string
		version string
		flavour nvidia.Flavour
		arch    nvidia.Architecture
	}{
		// Maxwell, Pascal and Volta: proprietary, on the highest branch.
		{"M60", "570", "570.211.01", nvidia.Proprietary, nvidia.Maxwell},
		{"P100", "570", "570.211.01", nvidia.Proprietary, nvidia.Pascal},
		{"V100", "570", "570.211.01", nvidia.Proprietary, nvidia.Volta},
		{"V100S", "570", "570.211.01", nvidia.Proprietary, nvidia.Volta},
		// Turing to Hopper: open modules.
		{"T4", "570", "570.211.01", nvidia.Open, nvidia.Turing},
		{"RTX 5000", "570", "570.211.01", nvidia.Open, nvidia.Turing},
		{"A10G", "570", "570.211.01", nvidia.Open, nvidia.Ampere},
		{"L40S", "570", "570.211.01", nvidia.Open, nvidia.Ada},
		{"H100", "570", "570.211.01", nvidia.Open, nvidia.Hopper},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			sel, reason := nvidia.Select(gpu(tc.model, 1), stable)
			if reason != nvidia.Supported {
				t.Fatalf("dropped: %s", reason)
			}
			if sel.Branch != tc.branch || sel.Version != tc.version || sel.Flavour != tc.flavour ||
				sel.Architecture != tc.arch || sel.Count != 1 || sel.NVSwitch {
				t.Errorf("selection = %+v", sel)
			}
		})
	}
}

func TestBlackwellIsUnsupportedWithout580(t *testing.T) {
	for _, m := range []string{"RTX PRO 6000", "RTX PRO 4500", "B200", "B300", "GB200", "GB300"} {
		if _, reason := nvidia.Select(gpu(m, 1), stable); reason != nvidia.ReasonNoDriverBranch {
			t.Errorf("%s on %s: reason = %q, want GPUNoDriverBranch", m, stable, reason)
		}
	}
}

func TestAlphaOffers580ToEveryArchitecture(t *testing.T) {
	for _, tc := range []struct {
		model   string
		flavour nvidia.Flavour
	}{
		{"RTX PRO 6000", nvidia.Open}, // Blackwell needs the open modules
		{"H100", nvidia.Open},
		{"V100", nvidia.Proprietary}, // 580 is the last branch for Volta
		{"M60", nvidia.Proprietary},
	} {
		sel, reason := nvidia.Select(gpu(tc.model, 1), "4865.0.0")
		if reason != nvidia.Supported || sel.Branch != "580" || sel.Version != "580.178.04" || sel.Flavour != tc.flavour {
			t.Errorf("%s: %+v, %q", tc.model, sel, reason)
		}
	}
}

func TestKeplerNeedsTheBranch470WhichNoReleaseBuilds(t *testing.T) {
	for _, r := range nvidia.Releases() {
		if _, reason := nvidia.Select(gpu("K80", 1), r.Version); reason != nvidia.ReasonNoDriverBranch {
			t.Errorf("K80 on %s: reason = %q", r.Version, reason)
		}
	}
}

func TestDropReasons(t *testing.T) {
	arm := gpu("T4G", 1)
	arm.AMD64 = false
	virtual := gpu("A10", 1)
	virtual.Virtual = true
	tests := []struct {
		name    string
		gpu     nvidia.GPU
		version string
		want    nvidia.Reason
	}{
		{"amd", nvidia.GPU{Manufacturer: "AMD", Model: "MI300X", Count: 8, AMD64: true}, stable, nvidia.ReasonManufacturer},
		{"no manufacturer", nvidia.GPU{Model: "T4", Count: 1, AMD64: true}, stable, nvidia.ReasonManufacturer},
		{"no model", nvidia.GPU{Manufacturer: "NVIDIA", Count: 1, AMD64: true}, stable, nvidia.ReasonModelUnknown},
		{"new model", gpu("X999", 1), stable, nvidia.ReasonModelUnknown},
		{"fraction", gpu("L4", 0.125), stable, nvidia.ReasonFractional},
		{"no gpu", gpu("L4", 0), stable, nvidia.ReasonFractional},
		{"arm64", arm, stable, nvidia.ReasonArch},
		{"unknown release", gpu("T4", 1), "1.0.0", nvidia.ReasonNoDriverBranch},
		{"virtual", virtual, stable, nvidia.ReasonVirtual},
		{"nvswitch", gpu("A100", 8), stable, nvidia.ReasonNVSwitch},
		{"nvswitch 16", gpu("A100", 16), stable, nvidia.ReasonNVSwitch},
		{"4 of an nvswitch model", gpu("A100", 4), stable, nvidia.Supported},
		{"8 volta is not nvswitch", gpu("V100", 8), stable, nvidia.Supported},
		{"fraction before arch", func() nvidia.GPU { g := gpu("L4", 0.5); g.AMD64 = false; return g }(), stable, nvidia.ReasonFractional},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, reason := nvidia.Select(tc.gpu, tc.version); reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

func TestMultiGPUCount(t *testing.T) {
	sel, reason := nvidia.Select(gpu("A100", 4), stable)
	if reason != nvidia.Supported || sel.Count != 4 || sel.NVSwitch {
		t.Errorf("%+v, %q", sel, reason)
	}
}

func TestEveryReasonIsListed(t *testing.T) {
	if len(nvidia.Reasons) != 7 {
		t.Errorf("Reasons = %v", nvidia.Reasons)
	}
}

func TestReleaseEntries(t *testing.T) {
	rels := nvidia.Releases()
	if rels[0].Version != stable {
		t.Errorf("the pinned stable release is not first: %s", rels[0].Version)
	}
	seen := map[string]bool{}
	for _, r := range rels {
		if seen[r.Version] {
			t.Errorf("release %s twice", r.Version)
		}
		seen[r.Version] = true
		if !slices.IsSortedFunc(r.Branches, func(a, b nvidia.Branch) int { return b.Number - a.Number }) {
			t.Errorf("%s: branches are not highest first", r.Version)
		}
		for _, b := range r.Branches {
			if b.Version == "" || (!b.Open && !b.Proprietary) {
				t.Errorf("%s: branch %d: %+v", r.Version, b.Number, b)
			}
		}
		got, ok := nvidia.LookupRelease(r.Version)
		if !ok || got.Version != r.Version {
			t.Errorf("LookupRelease(%s) = %+v, %v", r.Version, got, ok)
		}
	}
	if _, ok := nvidia.LookupRelease("0.0.0"); ok {
		t.Error("an unknown release was found")
	}
}

func TestStableBuilds535550And570(t *testing.T) {
	rel, _ := nvidia.LookupRelease(stable)
	want := map[int]string{570: "570.211.01", 550: "550.163.01", 535: "535.309.01"}
	if len(rel.Branches) != len(want) {
		t.Fatalf("branches = %+v", rel.Branches)
	}
	for _, b := range rel.Branches {
		if want[b.Number] != b.Version || !b.Open || !b.Proprietary {
			t.Errorf("branch %+v", b)
		}
	}
}

func TestModels(t *testing.T) {
	names := nvidia.Models()
	if !slices.IsSorted(names) {
		t.Error("Models is not sorted")
	}
	for _, m := range names {
		if _, ok := nvidia.ModelArchitecture(m); !ok {
			t.Errorf("%s has no architecture", m)
		}
	}
	if _, ok := nvidia.ModelArchitecture("X999"); ok {
		t.Error("an unknown model was classified")
	}
	if a, _ := nvidia.ModelArchitecture("L40S"); a != nvidia.Ada {
		t.Errorf("L40S = %s", a)
	}
}
