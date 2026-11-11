/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import "slices"

// The machine series facts the driver acts on. They come from Google's machine
// family documentation as the plan recorded them and are UNVERIFIED until the
// spike or the lane's runs confirm them; every table is kept honest by a test
// that runs it over the series of the committed Keeper snapshot, and a wrong
// entry shows as an API error of the launch (Unsupported), never as a silent
// substitute.

// nvmeOnlySeries attach persistent disks only over NVMe.
var nvmeOnlySeries = []string{"c3", "c3d", "c4", "c4a", "c4d", "n4", "n4a", "n4d", "a3", "a4", "a4x", "h3", "z3",
	"m3", "m4", "x4", "c4n", "m4n", "h4d", "z4d", "z4m", "x5"}

// noHyperdiskSeries cannot attach Hyperdisk.
var noHyperdiskSeries = []string{"e2", "n1", "f1", "g1", "t2a", "t2d"}

// noMigrateSeries cannot live migrate: their on-demand instances use
// onHostMaintenance TERMINATE (GPU series and the others that run on hardware
// that cannot migrate).
var noMigrateSeries = []string{"a2", "a3", "a4", "a4x", "g2", "g4", "h3", "c4a", "n4a", "t2a"}

// compactSeries support compact placement (a resource policy with
// collocation COLLOCATED); such instances also use onHostMaintenance TERMINATE.
var compactSeries = []string{"a2", "a3", "a4", "c2", "c2d", "c3", "c3d", "c4", "c4a", "c4d", "g2", "g4", "h3", "n2",
	"n2d", "n4", "z3"}

// tier1Series support Tier 1 networking (TIER_1 egress bandwidth), from 30
// vCPUs.
var tier1Series = []string{"c2", "c2d", "c3", "c3d", "c4", "c4a", "c4d", "m3", "m4", "n2", "n2d", "n4", "x4"}

// tier1MinVCPUs is the smallest machine Tier 1 networking is offered on.
const tier1MinVCPUs = 30

func inSeries(list []string, machineType string) bool {
	return slices.Contains(list, Family(machineType))
}

// DiskInterface is the attachment interface the series needs for data disks:
// NVME where only NVMe is offered, "" (the API's choice, SCSI) elsewhere.
func DiskInterface(machineType string) string {
	if inSeries(nvmeOnlySeries, machineType) {
		return "NVME"
	}
	return ""
}

// CanBootFrom says whether the series boots from a disk type: the Hyperdisk-only
// series boot only from Hyperdisk, the others boot from every type but cannot
// all attach Hyperdisk.
func CanBootFrom(machineType, diskType string) bool {
	hyper := len(diskType) > 10 && diskType[:10] == "hyperdisk-"
	switch {
	case inSeries(hyperdiskOnlySeries, machineType):
		return hyper
	case hyper:
		return !inSeries(noHyperdiskSeries, machineType)
	}
	return true
}

// CanAttach says whether the series attaches the disk type as a data disk.
func CanAttach(machineType, diskType string) bool {
	if len(diskType) > 10 && diskType[:10] == "hyperdisk-" {
		return !inSeries(noHyperdiskSeries, machineType)
	}
	// The Hyperdisk-only series attach persistent disks of the PD types too.
	return true
}

// MaintenanceFor is onHostMaintenance of an on-demand instance: MIGRATE, or
// TERMINATE for the series that cannot migrate and for compact placement.
func MaintenanceFor(machineType string, compact bool) string {
	if compact || inSeries(noMigrateSeries, machineType) {
		return "TERMINATE"
	}
	return "MIGRATE"
}
