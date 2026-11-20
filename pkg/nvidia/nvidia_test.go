/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nvidia_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

const (
	stable = "4757.2.3"
	alpha  = "4865.0.0"
)

var (
	sha256Pattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	runtimeVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

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
		{"nvswitch", gpu("A100", 8), stable, nvidia.Supported},
		{"nvswitch 16", gpu("A100", 16), stable, nvidia.Supported},
		{"h200", gpu("H200", 8), stable, nvidia.Supported},
		{"nvlink5 needs nvlsm", gpu("B200", 8), alpha, nvidia.ReasonNVSwitch},
		{"b300 needs nvlsm", gpu("B300", 8), alpha, nvidia.ReasonNVSwitch},
		{"blackwell slice", gpu("B200", 4), alpha, nvidia.Supported},
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
			if b.Version == "" {
				t.Errorf("%s: branch %d has no version", r.Version, b.Number)
			}
			for flavour, build := range map[string]nvidia.Build{"open": b.Open, "proprietary": b.Proprietary} {
				// A half-pinned build would be a build that looks verified and is not.
				if (build.RawSHA256 == "") != (build.PayloadSHA256 == "") {
					t.Errorf("%s: branch %d %s: only one of the two pins is set", r.Version, b.Number, flavour)
				}
				for _, sum := range []string{build.RawSHA256, build.PayloadSHA256} {
					if sum != "" && !sha256Pattern.MatchString(sum) {
						t.Errorf("%s: branch %d %s: %q is not a sha256", r.Version, b.Number, flavour, sum)
					}
				}
			}
			if b.Open.RawSHA256 == "" && b.Proprietary.RawSHA256 == "" {
				t.Errorf("%s: branch %d has no verified build", r.Version, b.Number)
			}
		}
		if !runtimeVersionPattern.MatchString(r.RuntimeVersion) || !sha256Pattern.MatchString(r.RuntimeSHA256) {
			t.Errorf("%s: the nvidia-runtime pin is %q %q", r.Version, r.RuntimeVersion, r.RuntimeSHA256)
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
		if want[b.Number] != b.Version || b.Open.RawSHA256 == "" || b.Proprietary.RawSHA256 == "" {
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

// The selection carries what the node needs: the sysext name and the pins of
// its image, and the runtime sysext.
func TestSelectionCarriesTheSysextAndItsPins(t *testing.T) {
	tests := []struct {
		model, sysext, raw string
	}{
		{"T4", "nvidia-drivers-570-open", "bd02a246daced8f446fedd5d2d31e587adcfddca70b580ad379344f1e16fadc9"},
		{"V100", "nvidia-drivers-570", "e190e7ec96167d2ffa6039e45a67675d7db3674c2160c33f68ead09d39e47b7a"},
	}
	for _, tc := range tests {
		sel, reason := nvidia.Select(gpu(tc.model, 1), stable)
		if reason != nvidia.Supported {
			t.Fatalf("%s: dropped: %s", tc.model, reason)
		}
		if sel.Sysext != tc.sysext || sel.RawSHA256 != tc.raw || !sha256Pattern.MatchString(sel.PayloadSHA256) {
			t.Errorf("%s: %+v", tc.model, sel)
		}
		if sel.RuntimeVersion != "v1.17.9" || sel.RuntimeSHA256 != "7863300dc49b353b75ff0bbc835b8651d791832316890e040aa4002a36bdffc3" {
			t.Errorf("%s: runtime %s %s", tc.model, sel.RuntimeVersion, sel.RuntimeSHA256)
		}
	}
}

func TestFromLabelsRebuildsTheSelection(t *testing.T) {
	want, _ := nvidia.Select(gpu("T4", 2), stable)
	got, err := nvidia.FromLabels(stable, want.Branch, want.Version, want.Flavour, "T4", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for name, args := range map[string][]string{
		"unknown release": {"4000.0.0", "570", "570.211.01", "open", "T4"},
		"wrong version":   {stable, "570", "570.1.1", "open", "T4"},
		"no 580":          {stable, "580", "580.178.04", "open", "T4"},
		"wrong flavour":   {stable, "570", "570.211.01", "gold", "T4"},
		"unknown model":   {stable, "570", "570.211.01", "open", "X999"},
	} {
		if _, err := nvidia.FromLabels(args[0], args[1], args[2], nvidia.Flavour(args[3]), args[4], 1); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A label value spells RTX 5000 as RTX-5000.
	if _, err := nvidia.FromLabels(stable, "570", "570.211.01", nvidia.Open, "RTX-5000", 1); err != nil {
		t.Errorf("RTX-5000: %v", err)
	}
}

// The renderer's NVSwitch guard needs the flag and the pin from the labels.
func TestFromLabelsCarriesTheNVSwitchFlagAndPin(t *testing.T) {
	want, reason := nvidia.Select(gpu("H100", 8), stable)
	if reason != nvidia.Supported || !want.NVSwitch || want.FabricManagerSHA256 == "" {
		t.Fatalf("Select: %+v, %q", want, reason)
	}
	got, err := nvidia.FromLabels(stable, want.Branch, want.Version, want.Flavour, "H100", 8)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	slice, err := nvidia.FromLabels(stable, want.Branch, want.Version, want.Flavour, "H100", 4)
	if err != nil || slice.NVSwitch || slice.FabricManagerSHA256 != "" {
		t.Errorf("4 GPUs: %+v, %v", slice, err)
	}
	// NVLink5 systems need more than Fabric Manager.
	if _, err := nvidia.FromLabels(alpha, "580", "580.178.04", nvidia.Open, "B200", 8); err == nil {
		t.Error("an 8-GPU B200 was rebuilt")
	}
}

// A release entry must pin Fabric Manager for the driver of every NVSwitch
// model it offers: an 8-GPU type of a model that needs nothing more than Fabric
// Manager is supported on every release that has a driver for it.
func TestEveryReleaseHasFabricManagerPinsForItsNVSwitchModels(t *testing.T) {
	for _, r := range nvidia.Releases() {
		for _, model := range []string{"A100", "H100", "H200"} {
			sel, reason := nvidia.Select(gpu(model, 8), r.Version)
			if reason != nvidia.Supported || !sel.NVSwitch || !sha256Pattern.MatchString(sel.FabricManagerSHA256) {
				t.Errorf("%s on %s: %+v, %q", model, r.Version, sel, reason)
			}
		}
		for version, sum := range r.FabricManager {
			if !sha256Pattern.MatchString(sum) || !slices.ContainsFunc(r.Branches, func(b nvidia.Branch) bool { return b.Version == version }) {
				t.Errorf("%s: Fabric Manager pin %q %q matches no driver version of the release", r.Version, version, sum)
			}
		}
	}
	if got := nvidia.FabricManagerFile("570.211.01"); got != "fabricmanager-linux-x86_64-570.211.01-archive.tar.xz" {
		t.Errorf("file = %s", got)
	}
	if got := nvidia.FabricManagerURL("570.211.01"); !strings.HasSuffix(got, "/redist/fabricmanager/linux-x86_64/fabricmanager-linux-x86_64-570.211.01-archive.tar.xz") {
		t.Errorf("URL = %s", got)
	}
}

func TestSelectionsAreDistinctPerSysext(t *testing.T) {
	sels := nvidia.Selections(stable)
	names := map[string]bool{}
	for _, s := range sels {
		names[s.Sysext] = true
	}
	if len(sels) != 2 || !names["nvidia-drivers-570-open"] || !names["nvidia-drivers-570"] {
		t.Errorf("selections = %+v", sels)
	}
	if nvidia.Selections("4000.0.0") != nil {
		t.Error("an unknown release has selections")
	}
}
