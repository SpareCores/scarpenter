/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Provisioning states of a VM.
const (
	provCreating  = "Creating"
	provDeleting  = "Deleting"
	provFailed    = "Failed"
	provSucceeded = "Succeeded"
)

// Power states of the instance view, the part after "PowerState/".
const (
	PowerStarting     = "starting"
	PowerRunning      = "running"
	PowerStopping     = "stopping"
	PowerStopped      = "stopped"
	PowerDeallocating = "deallocating"
	PowerDeallocated  = "deallocated"
)

// powerStateOf reads the power state from an instance view's status codes
// ("PowerState/running"); "" when the view has none.
func powerStateOf(iv *armcompute.VirtualMachineInstanceView) string {
	if iv == nil {
		return ""
	}
	for _, s := range iv.Statuses {
		if s == nil || s.Code == nil {
			continue
		}
		if state, ok := strings.CutPrefix(*s.Code, "PowerState/"); ok {
			return state
		}
	}
	return ""
}

// StateOf maps the provisioning state and the power state to the contract's
// instance state (plan 17 section 3, plan 14 A4): creating and starting are
// pending; stopping, stopped, deallocating and deallocated are terminating (a
// VM that powered itself off is collected); Deleting is terminating; a Failed
// VM that still exists is running (it costs money: the NodeClaim deletes it).
// A VM whose power state is not known (no instance view) is pending until it
// is.
func StateOf(vm armcompute.VirtualMachine) string {
	var prov string
	var iv *armcompute.VirtualMachineInstanceView
	if vm.Properties != nil {
		prov = deref(vm.Properties.ProvisioningState)
		iv = vm.Properties.InstanceView
	}
	switch prov {
	case provDeleting:
		return vendors.StateTerminating
	case provCreating:
		return vendors.StatePending
	case provFailed:
		return vendors.StateRunning
	}
	switch powerStateOf(iv) {
	case PowerRunning:
		return vendors.StateRunning
	case PowerStopping, PowerStopped, PowerDeallocating, PowerDeallocated:
		return vendors.StateTerminating
	}
	return vendors.StatePending
}
