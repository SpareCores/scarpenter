/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
)

// Family is a plan family of UpCloud's price list.
type Family string

// The plan families. A name that matches none is FamilyUnknown, which the
// driver does not launch until it is classified here (a test fails for a new
// family in the committed Keeper snapshot).
const (
	FamilyGeneral     Family = "general"     // 2xCPU-4GB (retired in Keeper's data, still launchable where offered)
	FamilyStarter     Family = "starter"     // STARTER-1xCPU-1GB
	FamilyPremium     Family = "premium"     // PREMIUM-2xCPU-4GB
	FamilyDeveloper   Family = "developer"   // DEV-1xCPU-1GB-10GB
	FamilyHiCPU       Family = "hicpu"       // HICPU-8xCPU-16GB
	FamilyHiMem       Family = "himem"       // HIMEM-2xCPU-16GB
	FamilyCloudNative Family = "cloudnative" // CLOUDNATIVE-2xCPU-4GB: no included storage or IPv4
	FamilyGPU         Family = "gpu"         // GPU-..., GPU-SPOT-...: not in v1 (ADR-011)
	FamilyUnknown     Family = ""
)

var generalRE = regexp.MustCompile(`^\d+xCPU-\d+GB$`)

// FamilyOf classifies a plan by its name.
func FamilyOf(name string) Family {
	switch {
	case generalRE.MatchString(name):
		return FamilyGeneral
	case strings.HasPrefix(name, "STARTER-"):
		return FamilyStarter
	case strings.HasPrefix(name, "PREMIUM-"):
		return FamilyPremium
	case strings.HasPrefix(name, "DEV-"):
		return FamilyDeveloper
	case strings.HasPrefix(name, "HICPU-"):
		return FamilyHiCPU
	case strings.HasPrefix(name, "HIMEM-"):
		return FamilyHiMem
	case strings.HasPrefix(name, "CLOUDNATIVE-"):
		return FamilyCloudNative
	case strings.HasPrefix(name, "GPU-"):
		return FamilyGPU
	}
	return FamilyUnknown
}

// Supported reports whether v1 launches the family: GPU plans have no drivers
// in v1 and their spot variants (GPU-SPOT-) stay out; an unknown family waits
// for a decision.
func (f Family) Supported() bool {
	return f != FamilyGPU && f != FamilyUnknown
}

// Plan is a plan as the driver uses it.
type Plan struct {
	Name     string
	Family   Family
	Cores    int
	MemoryMB int
	// StorageGB and Tier are the included storage; 0 and "" for plans without.
	StorageGB int
	Tier      string
	GPUs      int
}

// PlanOf converts the API's plan record.
func PlanOf(p upcloud.Plan) Plan {
	return Plan{
		Name: p.Name, Family: FamilyOf(p.Name), Cores: p.CoreNumber, MemoryMB: p.MemoryAmount,
		StorageGB: p.StorageSize, Tier: p.StorageTier, GPUs: p.GPUAmount,
	}
}

// Plans indexes plans by name.
type Plans map[string]Plan

// PlansOf indexes the API's plan list.
func PlansOf(ps *upcloud.Plans) Plans {
	out := Plans{}
	for _, p := range ps.Plans {
		out[p.Name] = PlanOf(p)
	}
	return out
}

// Supported reports whether the driver launches the plan.
func (p Plan) Supported() bool { return p.Family.Supported() }

// HasIPv4 reports whether the plan includes a public IPv4 address (Cloud Native
// plans do not: the address is billed separately).
func (p Plan) HasIPv4() bool { return p.Family != FamilyCloudNative }

// MinRootGB is the least root size the driver defaults to: the plan's included
// storage, at least 25 GB (open question 4).
const MinRootGB = 25

// MaxStorageGB is the largest storage; the smallest is 10 GB.
const (
	MinStorageGB = 10
	MaxStorageGB = 1024
)

// MaxStorageDevices caps the storage devices of one server at the lower of the
// two numbers the API gives (16 in the table, "more than four" in an error text)
// until spike S3 says otherwise.
const MaxStorageDevices = 4

// DefaultRootTier is the tier of the root disk without a setting: the plan's
// included tier (Standard for the Starter and Developer plans, MaxIOPS for the
// others) and MaxIOPS for plans without included storage. The one type-dependent
// default of the driver (documented, like GCP's Hyperdisk series).
func (p Plan) DefaultRootTier() string {
	if p.Tier != "" && p.StorageGB > 0 {
		return p.Tier
	}
	return upcloud.StorageTierMaxIOPS
}

// DefaultRootGB is the plan's included storage, at least MinRootGB.
func (p Plan) DefaultRootGB() int { return max(p.StorageGB, MinRootGB) }

// RootDiskBytes is the root disk size the scheduler sees as ephemeral storage
// when the NodeClass sets none (RootDiskForType of plan 14, A2): the default
// size, with GB read as GiB (UpCloud's storage unit; unverified, S4).
// RootDiskForType answers with it, and with the NodeClass's size when
// storage.root sets one (Driver.RootDiskForType).
func (p Plan) RootDiskBytes() int64 { return int64(p.DefaultRootGB()) << 30 }

// ValidatePlan checks that a name is a plan the driver launches and returns it.
func (ps Plans) ValidatePlan(name string) (Plan, error) {
	p, ok := ps[name]
	if !ok {
		return Plan{}, fmt.Errorf("plan %q is not in the account's plan list", name)
	}
	if !p.Supported() {
		return Plan{}, fmt.Errorf("plan %q (%s family) is not supported", name, p.Family)
	}
	return p, nil
}
