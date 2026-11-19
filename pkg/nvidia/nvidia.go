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
	"B200": {arch: Blackwell, nvswitchGPUs: 8}, "B300": {arch: Blackwell, nvswitchGPUs: 8},
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

// Branch is a driver branch a Flatcar release builds.
type Branch struct {
	// Number is the branch (570).
	Number int
	// Version is the exact driver version of the release (570.211.01); the
	// open and the proprietary build have the same one.
	Version string
	// Open and Proprietary say which sysexts the release builds
	// (nvidia-drivers-<branch>-open and nvidia-drivers-<branch>).
	Open, Proprietary bool
}

// Release is what a Flatcar release builds.
type Release struct {
	// Version is the Flatcar version (4757.2.3).
	Version string
	// Branches are the driver branches, highest first.
	Branches []Branch
	// FabricManager lists the driver versions with a pinned Fabric Manager
	// archive (plan 12, D9); until a version is listed, NVSwitch systems are
	// not offered on the release.
	FabricManager []string
}

// releases are the Flatcar releases SCarpenter knows driver sysexts of, the
// pinned stable release first. Verified against the update server's listing
// of each release; a release is added with its first NodeClass.
var releases = []Release{
	{
		Version: "4757.2.3", // stable: no 580 yet
		Branches: []Branch{
			{Number: 570, Version: "570.211.01", Open: true, Proprietary: true},
			{Number: 550, Version: "550.163.01", Open: true, Proprietary: true},
			{Number: 535, Version: "535.309.01", Open: true, Proprietary: true},
		},
	},
	{
		// Alpha, which adds 580 (and still builds the older branches, which
		// no supported architecture would pick over 580 and which are left
		// out until a NodeClass needs them).
		Version: "4865.0.0",
		Branches: []Branch{
			{Number: 580, Version: "580.178.04", Open: true, Proprietary: true},
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

func (r Release) hasFabricManager(driverVersion string) bool {
	return slices.Contains(r.FabricManager, driverVersion)
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
	sel.NVSwitch = m.nvswitchGPUs > 0 && sel.Count >= int64(m.nvswitchGPUs)
	if sel.NVSwitch && !rel.hasFabricManager(sel.Version) {
		return Selection{}, ReasonNVSwitch
	}
	if g.Virtual {
		return Selection{}, ReasonVirtual
	}
	return sel, Supported
}

// pick returns the highest branch of the release within the architecture's range.
func (r Release) pick(arch Architecture) (Selection, bool) {
	rule := architectures[arch]
	for _, b := range r.Branches {
		if b.Number < rule.minBranch || (rule.maxBranch > 0 && b.Number > rule.maxBranch) {
			continue
		}
		var flavour Flavour
		switch {
		case rule.openAllowed && b.Open:
			flavour = Open
		case rule.openRequired || !b.Proprietary:
			continue
		default:
			flavour = Proprietary
		}
		return Selection{Branch: strconv.Itoa(b.Number), Version: b.Version, Flavour: flavour}, true
	}
	return Selection{}, false
}

// SupportTable renders the supported models of a Flatcar release as a
// Markdown table (the one in the reference docs): per model its architecture,
// the driver the release gives it and the GPU count from which the type is an
// NVSwitch system. It is empty for an unknown release.
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
			switchFrom = fmt.Sprintf("%d GPUs", m.nvswitchGPUs)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", name, m.arch, driver, switchFrom)
	}
	return b.String()
}
