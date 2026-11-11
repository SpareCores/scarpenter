/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// InstanceState maps a Nova server status to the contract's instance states.
//
//	pending      BUILD, REBUILD, RESIZE, VERIFY_RESIZE, REVERT_RESIZE, MIGRATING,
//	             and any status the driver does not know (not deleted on a guess)
//	running      ACTIVE, and the transient REBOOT, HARD_REBOOT, PASSWORD
//	terminating  DELETING, SHUTOFF, STOPPED, PAUSED, SUSPENDED, SHELVED,
//	             SHELVED_OFFLOADED, RESCUE, ERROR
//	terminated   DELETED, SOFT_DELETED
//
// OpenStack has no shutdown behaviour "terminate": a guest poweroff (the
// poweroffAfter fail-safe, a crashed node) leaves a SHUTOFF server that OVH keeps
// billing, so it is terminating (the stopped-node rule of plan 14, A4): it stays
// in ListInstances and the garbage collector deletes it after the grace period.
func InstanceState(status string) string {
	switch strings.ToUpper(status) {
	case "ACTIVE", "REBOOT", "HARD_REBOOT", "PASSWORD":
		return vendors.StateRunning
	case "DELETING", "SHUTOFF", "STOPPED", "PAUSED", "SUSPENDED", "SHELVED", "SHELVED_OFFLOADED", "RESCUE", "ERROR":
		return vendors.StateTerminating
	case "DELETED", "SOFT_DELETED":
		return vendors.StateTerminated
	}
	// BUILD, REBUILD, RESIZE, VERIFY_RESIZE, REVERT_RESIZE, MIGRATING, UNKNOWN and
	// whatever a newer Nova adds.
	return vendors.StatePending
}
