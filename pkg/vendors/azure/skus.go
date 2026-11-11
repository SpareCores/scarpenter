/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"slices"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
)

// Capability names of a compute SKU (plan 17 section 4; the names are
// unverified against a live subscription, the fake and the canary pin them).
const (
	capVCPUs        = "vCPUs"
	capGenerations  = "HyperVGenerations"
	capPremiumIO    = "PremiumIO"
	capLowPriority  = "LowPriorityCapable"
	capAccelNet     = "AcceleratedNetworkingEnabled"
	capEphemeralOS  = "EphemeralOSDiskSupported"
	capMaxDataDisks = "MaxDataDiskCount"
	capDiskCtrl     = "DiskControllerTypes"
	capArch         = "CpuArchitectureType"
	capCachedDisk   = "CachedDiskBytes"
	capResourceDisk = "MaxResourceVolumeMB"
)

// SKU is one virtual machine size of a location, as the SKU table says.
type SKU struct {
	Name string
	// Family is the quota family (standardDSv5Family): the SKU's, never
	// derived from the name.
	Family string
	VCPUs  int
	// Arch is amd64 or arm64.
	Arch string
	// Gen2 is whether the size supports Hyper-V generation 2 (the only
	// generation of v1).
	Gen2 bool
	// SCSI is whether the size offers the SCSI disk controller (v1 does not
	// offer NVMe-only sizes).
	SCSI               bool
	PremiumIO          bool
	LowPriorityCapable bool
	AcceleratedNetwork bool
	EphemeralOSDisk    bool
	MaxDataDisks       int
	CachedDiskBytes    int64
	ResourceDiskMB     int64
	Zones              []string // zones the size exists in; empty for a region without zones
	RestrictedLocation bool
	RestrictedZones    []string
}

// capabilities indexes the capability list of a SKU.
func capabilities(cs []*armcompute.ResourceSKUCapabilities) map[string]string {
	out := make(map[string]string, len(cs))
	for _, c := range cs {
		if c != nil && c.Name != nil && c.Value != nil {
			out[*c.Name] = *c.Value
		}
	}
	return out
}

func capBool(c map[string]string, name string) bool {
	return strings.EqualFold(c[name], "true")
}

func capInt(c map[string]string, name string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(c[name]), 10, 64)
	return n
}

// ParseSKU reads a virtual machine SKU of the location. ok is false for
// anything that is not a virtual machine size of the location.
func ParseSKU(r *armcompute.ResourceSKU, location string) (SKU, bool) {
	if r == nil || r.Name == nil || !strings.EqualFold(deref(r.ResourceType), "virtualMachines") {
		return SKU{}, false
	}
	inLocation := false
	for _, l := range r.Locations {
		if l != nil && strings.EqualFold(*l, location) {
			inLocation = true
		}
	}
	if !inLocation {
		return SKU{}, false
	}
	c := capabilities(r.Capabilities)
	s := SKU{
		Name:               *r.Name,
		Family:             deref(r.Family),
		VCPUs:              int(capInt(c, capVCPUs)),
		Arch:               archAMD64,
		Gen2:               strings.Contains(strings.ToUpper(c[capGenerations]), "V2"),
		SCSI:               true,
		PremiumIO:          capBool(c, capPremiumIO),
		LowPriorityCapable: capBool(c, capLowPriority),
		AcceleratedNetwork: capBool(c, capAccelNet),
		EphemeralOSDisk:    capBool(c, capEphemeralOS),
		MaxDataDisks:       int(capInt(c, capMaxDataDisks)),
		CachedDiskBytes:    capInt(c, capCachedDisk),
		ResourceDiskMB:     capInt(c, capResourceDisk),
	}
	if strings.EqualFold(c[capArch], "Arm64") {
		s.Arch = archARM64
	}
	if ctrl, ok := c[capDiskCtrl]; ok {
		s.SCSI = false
		for t := range strings.SplitSeq(ctrl, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "SCSI") {
				s.SCSI = true
			}
		}
	}
	for _, li := range r.LocationInfo {
		if li == nil || !strings.EqualFold(deref(li.Location), location) {
			continue
		}
		for _, z := range li.Zones {
			if z != nil && !slices.Contains(s.Zones, *z) {
				s.Zones = append(s.Zones, *z)
			}
		}
	}
	slices.Sort(s.Zones)
	for _, rs := range r.Restrictions {
		if rs == nil || rs.Type == nil {
			continue
		}
		// Both reason codes (NotAvailableForSubscription, QuotaId) block the
		// launch.
		switch *rs.Type {
		case armcompute.ResourceSKURestrictionsTypeLocation:
			s.RestrictedLocation = true
		case armcompute.ResourceSKURestrictionsTypeZone:
			for _, z := range rs.Values {
				if z != nil {
					s.RestrictedZones = append(s.RestrictedZones, *z)
				}
			}
			// Zone restrictions also list the zones in RestrictionInfo.
			if rs.RestrictionInfo != nil {
				for _, z := range rs.RestrictionInfo.Zones {
					if z != nil {
						s.RestrictedZones = append(s.RestrictedZones, *z)
					}
				}
			}
		}
	}
	return s, true
}

// Launchable reports whether the size can be launched by v1 at all: Gen2, a
// SCSI controller and not restricted in the location.
func (s SKU) Launchable() bool {
	return s.Gen2 && s.SCSI && !s.RestrictedLocation
}

// LaunchableZones returns the zones the size can be launched in: its zones
// without a restriction, or "0" for a region without zones. Empty when the size
// is not launchable.
func (s SKU) LaunchableZones() []string {
	if !s.Launchable() {
		return nil
	}
	if len(s.Zones) == 0 {
		return []string{ZoneNone}
	}
	var out []string
	for _, z := range s.Zones {
		if !slices.Contains(s.RestrictedZones, z) {
			out = append(out, z)
		}
	}
	return out
}

// ParseSKUs parses a SKU table, dropping what is not a virtual machine size of
// the location, by name.
func ParseSKUs(rs []*armcompute.ResourceSKU, location string) map[string]SKU {
	out := map[string]SKU{}
	for _, r := range rs {
		if s, ok := ParseSKU(r, location); ok {
			out[s.Name] = s
		}
	}
	return out
}

// LaunchableTypes maps the size names to the zones they can be launched in, for
// OfferingLister (sizes without a launchable zone are left out).
func LaunchableTypes(skus map[string]SKU) map[string][]string {
	out := map[string][]string{}
	for name, s := range skus {
		if zones := s.LaunchableZones(); len(zones) > 0 {
			out[name] = zones
		}
	}
	return out
}

// RegionUsable reports whether the SKU table shows any launchable size: a
// region whose sizes are all restricted is RegionUnavailable.
func RegionUsable(skus map[string]SKU) bool {
	for _, s := range skus {
		if len(s.LaunchableZones()) > 0 {
			return true
		}
	}
	return false
}
