/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import "slices"

// Series names that several tables list.
const (
	seriesA4X = "a4x"
	seriesH4D = "h4d"
)

// The machine series facts the driver acts on. They come from Google's machine
// family documentation as the plan recorded them and are UNVERIFIED until the
// spike or the lane's runs confirm them; every table is kept honest by a test
// that runs it over the series of the committed Keeper snapshot, and a wrong
// entry shows as an API error of the launch (Unsupported), never as a silent
// substitute.

// nvmeOnlySeries attach persistent disks only over NVMe.
var nvmeOnlySeries = []string{"c3", "c3d", "c4", "c4a", "c4d", "n4", "n4a", "n4d", "a3", "a4", seriesA4X, "h3", "z3",
	"m3", "m4", "x4", "c4n", "m4n", seriesH4D, "z4d", "z4m", "x5"}

// noMigrateSeries cannot live migrate: their on-demand instances use
// onHostMaintenance TERMINATE (GPU series and the others that run on hardware
// that cannot migrate).
var noMigrateSeries = []string{"a2", "a3", "a4", seriesA4X, "g2", "g4", "h3", "c4a", "n4a", "t2a"}

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

// MaintenanceFor is onHostMaintenance of an on-demand instance: MIGRATE, or
// TERMINATE for the series that cannot migrate and for compact placement.
func MaintenanceFor(machineType string, compact bool) string {
	if compact || inSeries(noMigrateSeries, machineType) {
		return "TERMINATE"
	}
	return "MIGRATE"
}
