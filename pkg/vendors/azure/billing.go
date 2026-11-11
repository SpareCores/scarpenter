/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// BillingRule implements vendors.BillingDescriber. Azure bills a virtual machine
// per full minute (seconds are dropped) and documents no minimum, so there is no
// period or minimum worth holding a node for: billing retention (ADR-024, plan
// 23) never applies and the NodeClass status says PerSecondBilling. The rule is
// reported verified for that reason only (the same as AWS's): it comes from the
// pricing documentation, not from a live account, and it holds nothing either
// way. Keeper's own 1 second and 300 second row is not supported by the page
// (a data issue of plan 13), so the driver overrides it. BillingStart stays
// unset on instances: retention has nothing to count from.
func (d *Driver) BillingRule(string, string) (vendors.BillingRule, bool) {
	inc := time.Minute
	return vendors.BillingRule{
		Increment: &inc, Verified: true,
		Source: "Azure virtual machine pricing: per full minute, no documented minimum",
	}, true
}
