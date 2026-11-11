/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Instance statuses of the Compute API.
const (
	StatusProvisioning = "PROVISIONING"
	StatusStaging      = "STAGING"
	StatusRunning      = "RUNNING"
	StatusStopping     = "STOPPING"
	StatusStopped      = "STOPPED"
	StatusTerminated   = "TERMINATED"
	StatusSuspending   = "SUSPENDING"
	StatusSuspended    = "SUSPENDED"
	StatusRepairing    = "REPAIRING"
)

// StateOf maps an instance status to the contract's states (plan 16, section 3;
// plan 14, A4). STOPPING is also what a deleting instance shows. STOPPED,
// TERMINATED (GCP's other name for a stopped VM) and SUSPENDED are terminating:
// the instance is not coming back and its disks still cost, so the garbage
// collector deletes it. An unknown status is running, the conservative answer
// (it is not deleted on a guess).
func StateOf(status string) string {
	switch status {
	case StatusProvisioning, StatusStaging:
		return vendors.StatePending
	case StatusStopping, StatusStopped, StatusTerminated, StatusSuspending, StatusSuspended:
		return vendors.StateTerminating
	}
	return vendors.StateRunning
}

// CapacityTypeOf reads the capacity type of an instance from its scheduling.
func CapacityTypeOf(in *computepb.Instance) string {
	if s := in.GetScheduling(); s != nil && (s.GetProvisioningModel() == "SPOT" || s.GetPreemptible()) {
		return vendors.CapacitySpot
	}
	return vendors.CapacityOnDemand
}

// AddressesOf lists the addresses of an instance: the private and public IPv4
// and the IPv6 addresses of every network interface, in a stable order. The
// serving certificate approver checks the node's IP SANs against them.
func AddressesOf(in *computepb.Instance) []string {
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, nic := range in.GetNetworkInterfaces() {
		add(nic.GetNetworkIP())
		for _, ac := range nic.GetAccessConfigs() {
			add(ac.GetNatIP())
		}
		add(nic.GetIpv6Address())
		for _, ac := range nic.GetIpv6AccessConfigs() {
			add(ac.GetExternalIpv6())
		}
	}
	return out
}

// ToInstance converts a Compute instance of the region to the contract's
// Instance. The zone is the last segment of the instance's zone URL.
func ToInstance(region string, in *computepb.Instance) *vendors.Instance {
	zone := in.GetZone()
	if i := strings.LastIndexByte(zone, '/'); i >= 0 {
		zone = zone[i+1:]
	}
	inst := &vendors.Instance{
		Ref: NewRef(region, zone, in.GetName()), Region: region, Zone: zone,
		Type:         lastSegment(in.GetMachineType()),
		CapacityType: CapacityTypeOf(in),
		State:        StateOf(in.GetStatus()),
		Tags:         TagsOf(in.GetLabels()),
		Addresses:    AddressesOf(in),
	}
	if t, err := time.Parse(time.RFC3339, in.GetCreationTimestamp()); err == nil {
		inst.CreatedAt = t
		// Per second billing with a one minute minimum: retention never applies
		// (BillingRule), the stamp is the creation time for the record.
		inst.BillingStart = t
	}
	return inst
}

func lastSegment(s string) string { return s[strings.LastIndexByte(s, '/')+1:] }
