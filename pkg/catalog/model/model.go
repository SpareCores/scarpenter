/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package model is the vendor-neutral data model between a catalog source
// (Keeper, a static file) and the code that builds Karpenter instance types
// from it (pkg/catalog, pkg/catalog/labels). It has no dependency on Keeper's
// wire types.
package model

import "time"

// Capacity types of an offering (Karpenter's karpenter.sh/capacity-type).
const (
	CapacityOnDemand = "on-demand"
	CapacitySpot     = "spot"
)

// Architectures, as Kubernetes names them.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// Server is one instance type of a vendor, with the hardware facts Keeper has.
// Nullable facts are pointers; units are Spare Cores' own.
type Server struct {
	Vendor string
	// ID is the vendor-scoped server id of the data source; prices refer to it.
	ID string
	// APIReference is the vendor's name of the instance type.
	APIReference string
	Family       string

	VCPUs         int
	CPUCores      *int
	CPUAllocation string
	// Arch is amd64 or arm64.
	Arch            string
	CPUManufacturer string
	CPUFamily       string
	// CPUSpeedGHz is the clock speed.
	CPUSpeedGHz *float64
	CPUFlags    []string
	// CPUL2PerCoreKiB and CPUL3KiB are cache sizes in KiB; L3 is the total.
	CPUL2PerCoreKiB *float64
	CPUL3KiB        *float64

	MemoryMiB float64

	AcceleratorCount        float64
	AcceleratorType         string
	AcceleratorManufacturer string
	AcceleratorModel        string
	AcceleratorMemoryMiB    *float64

	NetworkBaselineGbps *float64
	NetworkPeakGbps     *float64

	// LocalStorageGB is the bundled local disk in GB (0: none); LocalStorageCount
	// the number of disks when the source lists them (0: unknown or none).
	LocalStorageGB    float64
	LocalStorageCount int
}

// Key identifies a server across the catalog.
type Key struct {
	Vendor string
	ID     string
}

// Key returns the server's key.
func (s *Server) Key() Key { return Key{Vendor: s.Vendor, ID: s.ID} }

// Region is a vendor region with the location facts of Keeper's region table.
type Region struct {
	Vendor string
	// ID is the source's region id (what queries and prices use).
	ID string
	// Name is the vendor's name of the region (topology.kubernetes.io/region).
	Name    string
	Aliases []string
	Active  bool

	// CountryID is the ISO country code, upper case as in Keeper.
	CountryID string
	City      string
	State     string
	// GreenEnergy is nil when unknown.
	GreenEnergy *bool
}

// Zone is a vendor zone of the source's zone table.
type Zone struct {
	Vendor   string
	RegionID string
	// ID is the source's zone id (the portable one on AWS, ADR-009).
	ID     string
	Name   string
	Active bool
}

// Price is the hourly USD price of one server in one zone with one capacity
// type. ZoneID is empty for a region-level price.
type Price struct {
	Vendor       string
	RegionID     string
	ZoneID       string
	ServerID     string
	CapacityType string
	USDPerHour   float64
}

// Vendor is a vendor row of the source's vendor table, with the billing facts.
// Nullable facts are pointers: a null is unknown, not zero.
type Vendor struct {
	ID     string
	Active bool

	// BillingIncrementSeconds is the granularity compute is billed in.
	BillingIncrementSeconds *int
	// MinimumBillingSeconds is the minimum billed duration of a run.
	MinimumBillingSeconds *int
	// StoppedServerCharged says whether a stopped server is still charged.
	StoppedServerCharged *bool
	// BillingComment is free text on billing exceptions.
	BillingComment string
}

// Data is one consistent state of the source: what a refresh reads.
type Data struct {
	// Version identifies the data (Keeper's database hash); a refresh that
	// reads the same version produces the same catalog.
	Version   string
	FetchedAt time.Time

	Servers []Server
	Regions []Region
	Zones   []Zone
	Prices  []Price
	// Vendors is the vendor table; empty when the source has none or could not
	// read it (billing retention then reports NoKeeperData, nothing else
	// depends on it).
	Vendors []Vendor

	// Scores holds the benchmark scores the request asked for, by the request's
	// metric query ID, then by APIReference of that query's vendor. Types
	// without a score are absent.
	Scores map[string]map[string]float64
	// Selected holds the members of the selection profiles the request asked
	// for, by the request's selection query ID, then by APIReference.
	Selected map[string]map[string]Member
	// Invalid maps the ID of a metric or selection query the source could not
	// answer for a reason of the query itself (an unknown benchmark config, an
	// invalid selector) to the reason. Those labels are DoesNotExist; the rest
	// of the refresh is not affected.
	Invalid map[string]string
}

// Member is a type that belongs to a selection profile.
type Member struct {
	// Score is the (normalised) ranking score, nil without a ranking or a score.
	Score *float64
}
