/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

// Code names the gate that removed an instance type or an offering, or made an
// offering unavailable (plan 28, section 2). Codes are API: they are
// documented in docs/reference/labels-and-offerings.md, only added, never
// renamed.
type Code string

// The reason codes. The GPU* codes are the nvidia.Reasons.
const (
	// NotInCatalog: Keeper's status or the architecture keeps a server out of
	// the data; it is never listed per type.
	NotInCatalog Code = "NotInCatalog"
	// NoPrice: no active USD price above zero in scope.
	NoPrice Code = "NoPrice"
	// RegionNotAllowed: every price row is in a region the class does not allow.
	RegionNotAllowed Code = "RegionNotAllowed"
	// ZoneNotAvailable: the price row's zone is not one of the driver's account,
	// or Keeper marked it inactive, or the driver lists no zones of the region.
	ZoneNotAvailable Code = "ZoneNotAvailable"
	// IncompletePrice: the price delta is incomplete and the class does not
	// allow incomplete prices.
	IncompletePrice Code = "IncompletePrice"
	// NotLaunchable: the driver says the type cannot launch in the zone.
	NotLaunchable Code = "NotLaunchable"
	// NoOffering: every offering of the type was removed; the detail is the
	// dominant reason.
	NoOffering Code = "NoOffering"
	// UnavailableCache: a live entry of the unavailable-offerings cache.
	UnavailableCache Code = "UnavailableCache"
	// QuotaGate: the quota is known and has no headroom.
	QuotaGate Code = "QuotaGate"
	// ImageGap: the class has no ready boot image in the region.
	ImageGap Code = "ImageGap"
	// RegionNotReady: the class's vendor resources are not ready in the region.
	RegionNotReady Code = "RegionNotReady"
	// ZoneMissingResource: a required resource is missing in the zone.
	ZoneMissingResource Code = "ZoneMissingResource"
	// Unrankable: the NodePool pins a selection profile and the type has no
	// score.
	Unrankable Code = "Unrankable"
	// VendorNotReady: GetInstanceTypes leaves out the types of a vendor that is
	// not ready.
	VendorNotReady Code = "VendorNotReady"
	// DriverNotEnabled: no driver for the vendor (Create only).
	DriverNotEnabled Code = "DriverNotEnabled"
	// NodePoolRequirement: the type's requirements do not intersect the
	// NodePool's (Explain with a NodePool only).
	NodePoolRequirement Code = "NodePoolRequirement"
	// NodePoolPreFilter: the NodePool's requirements on architecture, instance
	// type, region, zone or capacity type exclude it.
	NodePoolPreFilter Code = "NodePoolPreFilter"
	// ProfileMismatch, SettingsFailed and IncompleteSettingsPrice happen at
	// Create only.
	ProfileMismatch         Code = "ProfileMismatch"
	SettingsFailed          Code = "SettingsFailed"
	IncompleteSettingsPrice Code = "IncompleteSettingsPrice"
	// CatalogNotReady: no snapshot, unknown class, or the scope not refreshed.
	CatalogNotReady Code = "CatalogNotReady"
)

// Reason is a code with the data the gate used. A detail is a quota bucket, a
// zone, a region or a cache class, never a driver message or an account
// identifier.
type Reason struct {
	Code   Code
	Detail string
}

// typeDrop is a type the build left out, with the reason.
type typeDrop struct {
	vendor, apiReference string
	reason               Reason
}

// offeringDrop is an offering of a type the build left out.
type offeringDrop struct {
	region, zone, capacityType string
	reason                     Reason
}
