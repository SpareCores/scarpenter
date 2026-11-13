/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// Zone is a zone of a region as the driver's account sees it (overview section
// 5.8, ADR-009): the Keeper zone ID is the portable identity, the name is what
// topology.kubernetes.io/zone carries.
type Zone struct {
	// ID is Keeper's zone ID (use1-az1).
	ID string
	// Name is the account's zone name (us-east-1a).
	Name string
}

// Facts are the driver facts the catalog needs. Drivers do not exist in plan 03;
// plan 04 implements it over the driver registry and tests use StaticFacts.
// Implementations must be safe for concurrent use.
type Facts interface {
	// Zones returns the usable zones per region (the vendor's region name) and
	// whether the vendor has a zone concept. A vendor without zones (false) is
	// offered with zone = region. For a vendor with zones, Keeper zone IDs that
	// are not in the map are dropped, and a region-level price is replicated into
	// every zone of the region.
	Zones(vendor string) (zones map[string][]Zone, hasZones bool)

	// RootDiskBytes is the size of the root disk of the instance type under
	// the vendor's resolved default settings for the class, the ephemeral
	// storage the scheduler assumes: the driver's answer for the type where
	// the type defines its root disk, else the settings', else the catalog's
	// storage size of the type. ok is false when none says (the resource is
	// then omitted).
	RootDiskBytes(vendor string, class *Class, srv *model.Server) (bytes int64, ok bool)

	// Supported is the driver's profile hook (00a section 5.3): the profiles of
	// the class (class.Profiles, with their blocks in class.Inputs) that are
	// valid for the instance type in the zone, by kind. "default" is always
	// valid and may be left out.
	Supported(vendor string, class *Class, srv *model.Server, zone string) map[labels.ProfileKind][]string

	// PriceDelta is the price of the class's resolved default settings added
	// to an offering (00a section 10), and whether it is complete.
	PriceDelta(vendor string, class *Class, srv *model.Server, zone, capacityType string) (
		usdPerHour float64, complete bool)

	// Epoch changes whenever any answer above may have changed; it is part of
	// the catalog's cache keys.
	Epoch() uint64
}

// LaunchableFacts is an optional extension of Facts: the driver's
// OfferingLister view (overview section 5.9). An offering whose type is not
// launchable in the zone is dropped from the catalog instead of being found out
// by failed launches. Types or zones the driver says nothing about are kept.
type LaunchableFacts interface {
	// Launchable reports whether the type can be launched in the zone, given by
	// Keeper's zone ID; known is false when the driver does not say.
	Launchable(vendor, apiReference, zoneID string) (launchable, known bool)
}

// PriceSink is an optional extension of Facts: the drivers that price the
// disks and the public IPv4 address of an offering from the source's data.
// The catalog calls SetKeeperPrices with the rows of the data it has loaded,
// after a successful refresh and before it publishes that data, so the
// offerings of a data version always use that version's prices. The rows
// cover every vendor of the data; the sink forwards each vendor's to its
// driver.
type PriceSink interface {
	SetKeeperPrices(version string, storage []model.StoragePrice, ipv4 []model.IPv4Price)
}

// StaticFacts is a Facts with fixed answers, for tests and for drivers that
// have nothing to say yet. Supported reports only the default profile.
type StaticFacts struct {
	// ZonesByVendor maps vendor to region to zones; a vendor that is absent has
	// no zones.
	ZonesByVendor map[string]map[string][]Zone
	// RootDisk is the root disk size per vendor in bytes; absent: unknown.
	RootDisk map[string]int64
}

var _ Facts = StaticFacts{}

// Zones implements Facts.
func (f StaticFacts) Zones(vendor string) (map[string][]Zone, bool) {
	z, ok := f.ZonesByVendor[vendor]
	return z, ok
}

// RootDiskBytes implements Facts.
func (f StaticFacts) RootDiskBytes(vendor string, _ *Class, _ *model.Server) (int64, bool) {
	b, ok := f.RootDisk[vendor]
	return b, ok
}

// Supported implements Facts: nothing beyond the default profile.
func (StaticFacts) Supported(string, *Class, *model.Server, string) map[labels.ProfileKind][]string {
	return nil
}

// PriceDelta implements Facts: no delta, complete.
func (StaticFacts) PriceDelta(string, *Class, *model.Server, string, string) (float64, bool) {
	return 0, true
}

// Epoch implements Facts.
func (StaticFacts) Epoch() uint64 { return 0 }
