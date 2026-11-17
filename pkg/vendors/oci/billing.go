/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var _ vendors.BillingDescriber = (*Driver)(nil)

// BillingRule implements vendors.BillingDescriber (ADR-024).
//
// Compute instances are billed per second with a 60 second minimum for virtual
// machines (GPU VMs included) and a one hour minimum for bare metal, as Oracle's
// release note "Per-second billing for Compute instances" says
// (https://docs.oracle.com/iaas/releasenotes/changes/20facdce-34bf-4648-a0a7-de5db73657ce/index.htm).
// Keeper's vendor row has one value for the vendor, the bare metal hour. The
// family Keeper gives a server (Standard, Optimized, Dense I/O) does not tell
// a VM from bare metal, so the rule is the one of the VM shapes, which is what
// the driver launches in practice: the minimum is under the hold floor of
// billing retention, so nothing is held. A bare metal node is therefore not
// held for its hour (it is deleted when idle, as without retention).
//
// A stopped instance is charged by family, from Oracle's page on billing for
// stopped instances
// (https://docs.oracle.com/iaas/Content/Compute/Tasks/resource-billing-stopped-instances.htm):
// billing pauses for Standard shapes, VM.Optimized3 and VM.GPU.A10, and
// continues for Dense I/O, HPC, BM.Optimized3 and the other GPU shapes. The page
// does not name the Ampere (A1, A2, A4), burstable and standard bare metal
// shapes; they are Standard in Keeper's data, and are treated as Standard (pause)
// by inference from "Standard shapes". Optimized and GPU cover both a shape
// that pauses and one that continues, and an unknown family is not described:
// Keeper's row stands for them. The driver never stops an instance (it
// terminates), so this only informs the status.
//
// The rule comes from documentation, not from a live check: it is not
// verified, and billing retention stays off for OCI.
func (d *Driver) BillingRule(_, family string) (vendors.BillingRule, bool) {
	inc, minimum := time.Second, time.Minute
	rule := vendors.BillingRule{
		Increment: &inc, Minimum: &minimum, Verified: false,
		Source: "Oracle docs: per second, 60 second minimum for VMs (one hour for bare metal, not held)",
	}
	switch normalizeFamily(family) {
	case "standard":
		charged := false
		rule.StoppedCharged = &charged
	case "denseio", "hpc":
		charged := true
		rule.StoppedCharged = &charged
	}
	return rule, true
}

// normalizeFamily lower-cases a family and drops everything but letters and
// digits: Keeper's "Dense I/O" and its label value "Dense-I-O" both give "denseio".
func normalizeFamily(family string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(family) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
