/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"maps"
	"slices"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

// The seeded tenancy: OCIDs of the shape OCI uses with made-up unique parts.
const (
	SeedTenancy     = "ocid1.tenancy.oc1..aaaaexampletenancy"
	SeedCompartment = "ocid1.compartment.oc1..aaaaexamplecompartment"
	SeedSubnet      = "ocid1.subnet.oc1.eu-frankfurt-1.aaaaexamplesubnet"
	SeedImage       = "ocid1.image.oc1.eu-frankfurt-1.aaaaexampleimage"
	// SeedRegion has three availability domains, SeedRegionB too (with another
	// naming), SeedRegionOne one, SeedRegionUnsubscribed is listed but not READY.
	SeedRegion             = "eu-frankfurt-1"
	SeedRegionB            = "us-ashburn-1"
	SeedRegionOne          = "uk-london-1"
	SeedRegionUnsubscribed = "ap-sydney-1"
	// The tenancy prefix of the AD names: every tenancy has its own, and its AD
	// numbering is its own too.
	SeedPrefix = "aBcD"
)

// SeedZoneA, SeedZoneB are the zone names (the AD names without the prefix) of
// SeedRegion.
const (
	SeedZoneA = "EU-FRANKFURT-1-AD-1"
	SeedZoneB = "EU-FRANKFURT-1-AD-2"
	SeedZoneC = "EU-FRANKFURT-1-AD-3"
)

// ADFull is the full name of an AD of the seeded tenancy for a zone name.
func ADFull(zone string) string { return SeedPrefix + ":" + zone }

// NewSeeded returns a fake of the seeded tenancy: three subscribed regions (three,
// three and one availability domains, the second one named like Ashburn's) and a
// fourth that is not READY. It has no shapes: add them with AddShape.
func NewSeeded() *Cloud {
	c := New(SeedTenancy, SeedCompartment)
	c.AddRegion(SeedRegion, identity.RegionSubscriptionStatusReady,
		ADFull(SeedZoneA), ADFull(SeedZoneB), ADFull(SeedZoneC))
	c.AddRegion(SeedRegionB, identity.RegionSubscriptionStatusReady,
		"zXyW:US-ASHBURN-AD-1", "zXyW:US-ASHBURN-AD-2", "zXyW:US-ASHBURN-AD-3")
	c.AddRegion(SeedRegionOne, identity.RegionSubscriptionStatusReady, "qRsT:UK-LONDON-1-AD-1")
	c.AddRegion(SeedRegionUnsubscribed, identity.RegionSubscriptionStatusInProgress, "mNoP:AP-SYDNEY-1-AD-1")
	return c
}

// FlexShape returns a flexible shape: OCPUs 1 to maxOCPUs, memory min to max GB
// per OCPU and in all, the per-NUMA limits (0: none) and the limit names it
// consumes.
func FlexShape(name string, maxOCPUs, maxGB, minPerOCPU, maxPerOCPU, numaOCPUs, numaGB float32, quotaNames ...string,
) core.Shape {
	return core.Shape{
		Shape: &name, IsFlexible: new(true), QuotaNames: quotaNames,
		OcpuOptions: &core.ShapeOcpuOptions{Min: common.Float32(1), Max: &maxOCPUs, MaxPerNumaNode: nonZero(numaOCPUs)},
		MemoryOptions: &core.ShapeMemoryOptions{
			MinInGBs: common.Float32(1), MaxInGBs: &maxGB, MinPerOcpuInGBs: &minPerOCPU, MaxPerOcpuInGBs: &maxPerOCPU,
			DefaultPerOcpuInGBs: &minPerOCPU, MaxPerNumaNodeInGBs: nonZero(numaGB),
		},
	}
}

// FixedShape returns a shape with a fixed size.
func FixedShape(name string, ocpus, memoryGB float32, quotaNames ...string) core.Shape {
	return core.Shape{
		Shape: &name, IsFlexible: new(false), Ocpus: &ocpus, MemoryInGBs: &memoryGB, QuotaNames: quotaNames,
	}
}

func nonZero(v float32) *float32 {
	if v == 0 {
		return nil
	}
	return &v
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

func sortedRegions(m map[string]*region) []*region {
	out := make([]*region, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, m[k])
	}
	return out
}
