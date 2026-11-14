/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// StateOf maps an instance's lifecycle state to the contract's states (plan 25,
// section 5; plan 14, A4). A stopped Standard instance stops compute billing but
// its boot volume still costs, and it is not coming back, so STOPPING, STOPPED
// and TERMINATING are terminating and the garbage collector deletes it. OCI
// keeps a TERMINATED instance listed for about twelve hours: it is terminated,
// and ListInstances of the driver drops it. An unknown state is running, the
// conservative answer (it is not deleted on a guess).
func StateOf(s core.InstanceLifecycleStateEnum) string {
	switch s {
	case core.InstanceLifecycleStateProvisioning, core.InstanceLifecycleStateStarting:
		return vendors.StatePending
	case core.InstanceLifecycleStateStopping, core.InstanceLifecycleStateStopped,
		core.InstanceLifecycleStateTerminating:
		return vendors.StateTerminating
	case core.InstanceLifecycleStateTerminated:
		return vendors.StateTerminated
	}
	return vendors.StateRunning
}

// CapacityTypeOf reads the capacity type of an instance: a preemptible
// configuration is spot.
func CapacityTypeOf(in core.Instance) string {
	if in.PreemptibleInstanceConfig != nil {
		return vendors.CapacitySpot
	}
	return vendors.CapacityOnDemand
}

// TypeOf rebuilds the type name of an instance from its shape and shape
// configuration; it equals the CreateRequest's InstanceType byte for byte for an
// instance the driver launched.
func TypeOf(in core.Instance) string {
	shape := deref(in.Shape)
	cfg := in.ShapeConfig
	if cfg == nil || cfg.Ocpus == nil || cfg.MemoryInGBs == nil {
		return shape
	}
	return typeOfInstance(shape, float64(*cfg.Ocpus), float64(*cfg.MemoryInGBs), true)
}

// ToInstance converts an instance of the region to the contract's Instance. The
// addresses are not part of it (they come from the VNIC attachments).
func ToInstance(region string, in core.Instance) *vendors.Instance {
	out := &vendors.Instance{
		Ref: NewRef(region, deref(in.Id)), Region: region, Zone: ADName(deref(in.AvailabilityDomain)),
		Type: TypeOf(in), CapacityType: CapacityTypeOf(in), State: StateOf(in.LifecycleState),
		Tags: TagsOf(in.FreeformTags),
	}
	if in.TimeCreated != nil {
		// Per second billing with a one minute minimum: retention never applies
		// (plan 25, billing period); the stamp is the creation time for the record.
		out.CreatedAt = in.TimeCreated.Time
		out.BillingStart = in.TimeCreated.Time
	}
	return out
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
