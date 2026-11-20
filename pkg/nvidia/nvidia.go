/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package nvidia is the support table for NVIDIA GPU nodes: which GPU models
// SCarpenter can run, on which driver branch and flavour of the Flatcar
// release a NodeClass pins. It is pure data and functions, with no Kubernetes
// client and no vendor knowledge (plan 12, decision D1).
//
// The driver is Flatcar's prebuilt sysext of the release, so what a release
// offers is fixed by the release: a table entry per Flatcar version lists the
// branches it builds and their driver versions.
package nvidia

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Manufacturer is Keeper's accelerator manufacturer of NVIDIA GPUs.
const Manufacturer = "NVIDIA"

// NodeBootstrap says that nodes of a supported type boot with the driver and
// that the chart runs the device plugin (plan 12, steps (b) and (c)). Until
// then the catalog advertises no nvidia.com/gpu and no driver labels: Karpenter
// would launch a node for a Pod that requests GPUs, and the node would never
// initialize because nothing registers the resource. The step that ships the
// driver and the device plugin sets it.
const NodeBootstrap = false

// Architecture is a GPU architecture.
type Architecture string

// The architectures of the models SCarpenter classifies.
const (
	Kepler    Architecture = "Kepler"
	Maxwell   Architecture = "Maxwell"
	Pascal    Architecture = "Pascal"
	Volta     Architecture = "Volta"
	Turing    Architecture = "Turing"
	Ampere    Architecture = "Ampere"
	Ada       Architecture = "Ada"
	Hopper    Architecture = "Hopper"
	Blackwell Architecture = "Blackwell"
)

// archRule says which driver branches and flavours an architecture can use.
type archRule struct {
	// openAllowed: the open kernel modules support the architecture (Turing
	// and later); openRequired: only they do (Blackwell).
	openAllowed, openRequired bool
	// minBranch and maxBranch bound the driver branch; zero is unbounded.
	minBranch, maxBranch int
}

var architectures = map[Architecture]archRule{
	Kepler:    {maxBranch: 470},
	Maxwell:   {maxBranch: 580},
	Pascal:    {maxBranch: 580},
	Volta:     {maxBranch: 580},
	Turing:    {openAllowed: true},
	Ampere:    {openAllowed: true},
	Ada:       {openAllowed: true},
	Hopper:    {openAllowed: true},
	Blackwell: {openAllowed: true, openRequired: true, minBranch: 580},
}

// model is what the table knows of a Keeper accelerator model.
type model struct {
	arch Architecture
	// nvswitchGPUs is the GPU count from which the type is an NVSwitch system
	// (HGX baseboards), which needs Fabric Manager; zero: never.
	nvswitchGPUs int
	// needsNVLSM: the NVSwitch system is NVLink5 (HGX B200 and B300), where
	// Fabric Manager alone is not enough: NVIDIA's start script also runs the
	// NVLink subnet manager (nvlsm), which needs ib_umad and ibstat, and
	// Flatcar has neither. Such a model stays dropped (ReasonNVSwitch).
	needsNVLSM bool
}

// models maps Keeper's accelerator_model to what the table knows of it. A new
// model is one line here; a test over the committed Keeper snapshots fails on
// an NVIDIA model that is missing.
var models = map[string]model{
	"K80": {arch: Kepler},
	"M60": {arch: Maxwell},
	"P4":  {arch: Pascal}, "P40": {arch: Pascal}, "P100": {arch: Pascal},
	"V100": {arch: Volta}, "V100S": {arch: Volta},
	"T4": {arch: Turing}, "T4G": {arch: Turing}, "RTX 5000": {arch: Turing},
	"A10": {arch: Ampere}, "A10G": {arch: Ampere}, "A16": {arch: Ampere}, "A30": {arch: Ampere}, "A40": {arch: Ampere},
	"A100": {arch: Ampere, nvswitchGPUs: 8},
	"L4":   {arch: Ada}, "L20": {arch: Ada}, "L40": {arch: Ada}, "L40S": {arch: Ada},
	"H100": {arch: Hopper, nvswitchGPUs: 8}, "H200": {arch: Hopper, nvswitchGPUs: 8},
	"B200":  {arch: Blackwell, nvswitchGPUs: 8, needsNVLSM: true},
	"B300":  {arch: Blackwell, nvswitchGPUs: 8, needsNVLSM: true},
	"GB200": {arch: Blackwell}, "GB300": {arch: Blackwell},
	"RTX PRO 4500": {arch: Blackwell}, "RTX PRO 6000": {arch: Blackwell},
}

// Models returns the classified model names, sorted.
func Models() []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ModelArchitecture returns the architecture of a model; ok is false for a
// model the table does not classify.
func ModelArchitecture(name string) (Architecture, bool) {
	m, ok := models[name]
	return m.arch, ok
}

// Flavour is the kind of kernel modules of a driver sysext.
type Flavour string

// The flavours; the values are the nvidia-driver-flavor label's.
const (
	Open        Flavour = "open"
	Proprietary Flavour = "proprietary"
)

// Build pins one driver sysext of a release (nvidia-drivers-<branch>[-open]).
// A build without a RawSHA256 is not usable: the type stays unsupported until
// someone verifies the pins.
type Build struct {
	// PayloadSHA256 is the sha256 of Flatcar's signed payload, the .gz the
	// release server publishes (its .gz.sha256 file); a mirror's script checks
	// the download against it.
	PayloadSHA256 string
	// RawSHA256 is the sha256 of the extracted sysext image, the .raw: what
	// Ignition verifies on the file it writes (the hash is over the
	// decompressed bytes, so a mirror may serve it compressed).
	RawSHA256 string
}

func (b Build) usable() bool { return b.RawSHA256 != "" }

// Branch is a driver branch a Flatcar release builds.
type Branch struct {
	// Number is the branch (570).
	Number int
	// Version is the exact driver version of the release (570.211.01); the
	// open and the proprietary build have the same one.
	Version string
	// Open and Proprietary are the sysexts the release builds
	// (nvidia-drivers-<branch>-open and nvidia-drivers-<branch>); a flavour the
	// release does not build, or whose pins are not verified, is the zero Build.
	Open, Proprietary Build
}

// Release is what a Flatcar release builds.
type Release struct {
	// Version is the Flatcar version (4757.2.3).
	Version string
	// Branches are the driver branches, highest first.
	Branches []Branch
	// RuntimeVersion and RuntimeSHA256 pin the nvidia-runtime sysext of the
	// sysext-bakery (nvidia-runtime-<RuntimeVersion>-x86-64.raw) the release
	// is used with; without them no GPU type is offered on the release.
	RuntimeVersion, RuntimeSHA256 string
	// FabricManager maps a driver version to the sha256 of NVIDIA's Fabric
	// Manager archive of that version (plan 12, D9); until a version is
	// listed, NVSwitch systems are not offered on the release.
	FabricManager map[string]string
}

// runtime is the nvidia-runtime sysext of every release below: 1.17.9 is the
// version Flatcar's own documentation uses (1.16 and older do not work with the
// prebuilt driver sysexts). The sha256 is the line of the release's SHA256SUMS
// and of the file itself.
const (
	runtimeVersion = "v1.17.9"
	runtimeSHA256  = "7863300dc49b353b75ff0bbc835b8651d791832316890e040aa4002a36bdffc3"
)

// releases are the Flatcar releases SCarpenter knows driver sysexts of, the
// pinned stable release first. A release is added with its first NodeClass. The
// versions come from the release's ebuilds and the update server's listing; the
// pins were taken from the update server (the .gz.sha256 files) and by
// extracting the verified payloads with Flatcar's download_sysext (hack/nvidia-mirror.sh
// does both and prints these entries). The Fabric Manager pins are the sha256 of
// NVIDIA's archives, equal to the sha256 of the driver release's redistrib_<version>.json
// manifest and to the hash of the downloaded file (2026-11-20).
var releases = []Release{
	{
		Version: "4757.2.3", // stable: no 580 yet
		Branches: []Branch{
			{
				Number: 570, Version: "570.211.01",
				Open: Build{
					PayloadSHA256: "a0dc6aec56b5e13734c206eff8dbc74b7fde2d44e3db11dac51ff5374f9b198a",
					RawSHA256:     "bd02a246daced8f446fedd5d2d31e587adcfddca70b580ad379344f1e16fadc9",
				},
				Proprietary: Build{
					PayloadSHA256: "331250c60796353f0e732a17ea2c7edf890514bab67566198f382142756242d2",
					RawSHA256:     "e190e7ec96167d2ffa6039e45a67675d7db3674c2160c33f68ead09d39e47b7a",
				},
			},
			{
				Number: 550, Version: "550.163.01",
				Open: Build{
					PayloadSHA256: "a4ebb3eb43cd2317bfa949f123493f9d04bb568d555717169159a701cc01913b",
					RawSHA256:     "522c18a095be12b2bda8357d3e713cc819c6c644763280b5b9f416f4ac8697df",
				},
				Proprietary: Build{
					PayloadSHA256: "3c8f495cab4f43cecae765e7a388669938f43aef52cd1b03c2b0a40a00e49af6",
					RawSHA256:     "9343da55f603b7becea296c8b6ac1e890f69a81ab049944362966e9574f06245",
				},
			},
			{
				Number: 535, Version: "535.309.01",
				Open: Build{
					PayloadSHA256: "733bc0ff4d385b0edc17e05f77a3f592c27804ca0e7d46703f234da156ada6a2",
					RawSHA256:     "47dabfe4edadc8164a8d2e3e01094260a4c5fb50deb4f6b22fd4ffb33824cc71",
				},
				Proprietary: Build{
					PayloadSHA256: "8e95b626900af8d8aa20ecdf41f6bf5163370a9ec445e59faa21989b87945fb1",
					RawSHA256:     "645e718efd0335fa43d05bfe689263223f842b78bc682b3d8d7a335ca6e1e460",
				},
			},
		},
		RuntimeVersion: runtimeVersion, RuntimeSHA256: runtimeSHA256,
		FabricManager: map[string]string{
			"570.211.01": "ca18525b22546e26e26687637b87e492c6dab6477df681abbb6be942f9e5e944",
			"550.163.01": "6517bab21f7f9e8c366bae219d45c0be01c59cbcf9b307689deab3a074f92b44",
			"535.309.01": "8d1e281e11fcf3469a7c4fa5aac431deb5a070f684082bb367d056e5cd3cd1e5",
		},
	},
	{
		// Alpha, which adds 580 (and still builds the older branches, which
		// no supported architecture would pick over 580 and which are left
		// out until a NodeClass needs them).
		Version: "4865.0.0",
		Branches: []Branch{
			{
				Number: 580, Version: "580.178.04",
				Open: Build{
					PayloadSHA256: "12c07de63768578cb685dd1f4e0bc89108779f73ebc06206cd931483b5d6edde",
					RawSHA256:     "88d5c259b7bb006d382d40bfdf24ecc1c0d3293dbf1cd29bcc028c766b634cd9",
				},
				Proprietary: Build{
					PayloadSHA256: "5405709d588271c154c4b89561b76757c878116e8befef45ef2e72c6452c9ed4",
					RawSHA256:     "ce2db01735ead7e0af6d453f354156e9553b86e2d8dd6bdb641bff31cf1a5b72",
				},
			},
		},
		RuntimeVersion: runtimeVersion, RuntimeSHA256: runtimeSHA256,
		FabricManager: map[string]string{
			"580.178.04": "c599d3418672fee3eb4cb0fa98009f946196b5a601e25bb3874005c53b2d348c",
		},
	},
}

// Releases returns the known releases, the pinned stable one first.
func Releases() []Release { return slices.Clone(releases) }

// LookupRelease returns the release of a Flatcar version.
func LookupRelease(flatcarVersion string) (Release, bool) {
	for _, r := range releases {
		if r.Version == flatcarVersion {
			return r, true
		}
	}
	return Release{}, false
}

// FabricManagerFile is the file name of NVIDIA's Fabric Manager archive of a
// driver version.
func FabricManagerFile(driverVersion string) string {
	return "fabricmanager-linux-x86_64-" + driverVersion + "-archive.tar.xz"
}

// FabricManagerURL is where NVIDIA publishes the Fabric Manager archive of a
// driver version (the driver redistributables tree).
func FabricManagerURL(driverVersion string) string {
	return "https://developer.download.nvidia.com/compute/nvidia-driver/redist/fabricmanager/linux-x86_64/" +
		FabricManagerFile(driverVersion)
}

// FabricManagerPin returns the sha256 of the Fabric Manager archive of a driver
// version on a Flatcar release; empty without a pin.
func FabricManagerPin(flatcarVersion, driverVersion string) string {
	rel, _ := LookupRelease(flatcarVersion)
	return rel.FabricManager[driverVersion]
}

// withFabricManager marks the selection as an NVSwitch system when the model
// and the GPU count say so and adds the release's Fabric Manager pin; it
// returns false when the type then cannot be offered (no pin, or a model that
// needs more than Fabric Manager).
func (r Release) withFabricManager(sel *Selection, m model) bool {
	sel.NVSwitch = m.nvswitchGPUs > 0 && sel.Count >= int64(m.nvswitchGPUs)
	if !sel.NVSwitch {
		return true
	}
	sel.FabricManagerSHA256 = r.FabricManager[sel.Version]
	return sel.FabricManagerSHA256 != "" && !m.needsNVLSM
}

// Reason is why a GPU type is not offered; it is the reason label of the
// scarpenter_catalog_dropped_types gauge.
type Reason string

// The reasons, in the order they are checked. Supported is the empty reason.
const (
	Supported Reason = ""
	// ReasonManufacturer: the GPU is not NVIDIA's (or Keeper does not say).
	ReasonManufacturer Reason = "GPUManufacturer"
	// ReasonModelUnknown: the model is not in the table.
	ReasonModelUnknown Reason = "GPUModelUnknown"
	// ReasonFractional: a fraction of a GPU (vGPU, MIG-like slices).
	ReasonFractional Reason = "GPUFractional"
	// ReasonArch: not amd64.
	ReasonArch Reason = "GPUArch"
	// ReasonNoDriverBranch: the NodeClass's Flatcar release builds no branch
	// for the model's architecture.
	ReasonNoDriverBranch Reason = "GPUNoDriverBranch"
	// ReasonNVSwitch: an NVSwitch system without a Fabric Manager pin.
	ReasonNVSwitch Reason = "GPUNVSwitch"
	// ReasonVirtual: the vendor says the GPU is virtual and needs a GRID driver.
	ReasonVirtual Reason = "GPUVirtual"
)

// Reasons lists every reason in check order.
var Reasons = []Reason{
	ReasonManufacturer, ReasonModelUnknown, ReasonFractional, ReasonArch,
	ReasonNoDriverBranch, ReasonNVSwitch, ReasonVirtual,
}

// GPU is the accelerator of an instance type, as far as the table needs it.
type GPU struct {
	// Manufacturer and Model are Keeper's accelerator manufacturer and model.
	Manufacturer, Model string
	// Count is Keeper's accelerator count; fractional GPUs have a fraction.
	Count float64
	// AMD64 says the CPU architecture is amd64.
	AMD64 bool
	// Virtual is the vendor's answer (vendors.GPUVirtualizer).
	Virtual bool
}

// Selection is the driver chosen for a GPU type on a Flatcar release; its
// strings are the values of the three driver labels.
type Selection struct {
	// Branch is the driver branch ("570").
	Branch string
	// Version is the exact driver version ("570.211.01").
	Version string
	// Flavour is the kernel module flavour.
	Flavour Flavour
	// Count is the whole number of GPUs: the nvidia.com/gpu capacity.
	Count int64
	// Architecture is the model's GPU architecture.
	Architecture Architecture
	// NVSwitch says the type is an NVSwitch system (it needs Fabric Manager).
	NVSwitch bool
	// FabricManagerSHA256 is the sha256 of the Fabric Manager archive of
	// Version (FabricManagerFile); set for an NVSwitch system.
	FabricManagerSHA256 string
	// Sysext is the name of the driver sysext ("nvidia-drivers-570-open"), the
	// line of enabled-sysext.conf; its pins are the payload's sha256 (the .gz
	// of the release server) and the extracted image's.
	Sysext                   string
	PayloadSHA256, RawSHA256 string
	// RuntimeVersion and RuntimeSHA256 pin the nvidia-runtime sysext of the
	// release (the version as the sysext-bakery spells it, "v1.17.9").
	RuntimeVersion, RuntimeSHA256 string
}

// Select chooses the driver for a GPU type on a Flatcar release: the highest
// branch the release builds within the architecture's range, the open modules
// where the architecture allows them. The reason is empty for a supported type
// and says why the type is dropped otherwise.
func Select(g GPU, flatcarVersion string) (Selection, Reason) {
	if g.Manufacturer != Manufacturer {
		return Selection{}, ReasonManufacturer
	}
	m, ok := models[g.Model]
	if !ok {
		return Selection{}, ReasonModelUnknown
	}
	if g.Count < 1 || g.Count != math.Trunc(g.Count) {
		return Selection{}, ReasonFractional
	}
	if !g.AMD64 {
		return Selection{}, ReasonArch
	}
	rel, ok := LookupRelease(flatcarVersion)
	if !ok {
		return Selection{}, ReasonNoDriverBranch
	}
	sel, ok := rel.pick(m.arch)
	if !ok {
		return Selection{}, ReasonNoDriverBranch
	}
	sel.Count = int64(g.Count)
	sel.Architecture = m.arch
	if !rel.withFabricManager(&sel, m) {
		return Selection{}, ReasonNVSwitch
	}
	if g.Virtual {
		return Selection{}, ReasonVirtual
	}
	return sel, Supported
}

// pick returns the highest branch of the release within the architecture's
// range, with the sysext it needs. Only a build with verified pins counts, and
// only on a release with a pinned runtime sysext.
func (r Release) pick(arch Architecture) (Selection, bool) {
	rule := architectures[arch]
	if r.RuntimeVersion == "" || r.RuntimeSHA256 == "" {
		return Selection{}, false
	}
	for _, b := range r.Branches {
		if b.Number < rule.minBranch || (rule.maxBranch > 0 && b.Number > rule.maxBranch) {
			continue
		}
		flavour, build, name := Proprietary, b.Proprietary, "nvidia-drivers-"+strconv.Itoa(b.Number)
		switch {
		case rule.openAllowed && b.Open.usable():
			flavour, build, name = Open, b.Open, name+"-open"
		case rule.openRequired || !build.usable():
			continue
		}
		return Selection{
			Branch: strconv.Itoa(b.Number), Version: b.Version, Flavour: flavour,
			Sysext: name, PayloadSHA256: build.PayloadSHA256, RawSHA256: build.RawSHA256,
			RuntimeVersion: r.RuntimeVersion, RuntimeSHA256: r.RuntimeSHA256,
		}, true
	}
	return Selection{}, false
}

// FromLabels rebuilds the selection of a type from its driver labels (branch,
// exact version and flavour), its accelerator model label and its GPU count, with
// the pins of the release: what the launch of a node needs after the catalog
// stamped the labels. The model decides whether the type is an NVSwitch
// system. It fails when the release has no such verified build, or no Fabric
// Manager pin for an NVSwitch system.
func FromLabels(flatcarVersion, branch, version string, flavour Flavour, modelLabel string,
	count int64) (Selection, error) {
	// The label value is the model with the characters a label cannot hold
	// replaced by "-" (RTX-5000); no model name has a hyphen of its own.
	model := strings.ReplaceAll(modelLabel, "-", " ")
	m, ok := models[model]
	if !ok {
		return Selection{}, fmt.Errorf("NVIDIA model %q is not in the table", modelLabel)
	}
	rel, _ := LookupRelease(flatcarVersion)
	for _, sel := range Selections(flatcarVersion) {
		if sel.Branch == branch && sel.Version == version && sel.Flavour == flavour {
			sel.Count, sel.Architecture = count, m.arch
			if !rel.withFabricManager(&sel, m) {
				return Selection{}, fmt.Errorf(
					"release %s cannot run the NVSwitch system %s with driver %s: no Fabric Manager pin, or the model needs more",
					flatcarVersion, model, version)
			}
			return sel, nil
		}
	}
	return Selection{}, fmt.Errorf("release %s has no verified %s NVIDIA driver %s (branch %s)",
		flatcarVersion, flavour, version, branch)
}

// Selections returns the distinct driver selections a release makes over every
// model, for sizing trial renderings: one entry per branch and flavour in use.
// None is an NVSwitch system (see FabricManagerPin).
func Selections(flatcarVersion string) []Selection {
	rel, ok := LookupRelease(flatcarVersion)
	if !ok {
		return nil
	}
	var out []Selection
	for _, name := range Models() {
		sel, ok := rel.pick(models[name].arch)
		if ok && !slices.ContainsFunc(out, func(o Selection) bool { return o.Sysext == sel.Sysext }) {
			sel.Count, sel.Architecture = 1, models[name].arch
			out = append(out, sel)
		}
	}
	return out
}

// SupportTable renders the supported models of a Flatcar release as a
// Markdown table (the one in the reference docs): per model its architecture,
// the driver the release gives it and the GPU count from which the type is an
// NVSwitch system, and whether such a type is offered. It is empty for an unknown release.
func SupportTable(flatcarVersion string) string {
	rel, ok := LookupRelease(flatcarVersion)
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString("| Model | Architecture | Driver | NVSwitch from |\n|---|---|---|---|\n")
	for _, name := range Models() {
		m := models[name]
		driver := "none"
		if sel, ok := rel.pick(m.arch); ok {
			driver = fmt.Sprintf("%s (%s, %s)", sel.Branch, sel.Version, sel.Flavour)
		}
		switchFrom := "-"
		if m.nvswitchGPUs > 0 {
			switchFrom = fmt.Sprintf("%d GPUs, with Fabric Manager", m.nvswitchGPUs)
			if m.needsNVLSM {
				switchFrom = fmt.Sprintf("%d GPUs, not offered (needs nvlsm)", m.nvswitchGPUs)
			}
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", name, m.arch, driver, switchFrom)
	}
	return b.String()
}
