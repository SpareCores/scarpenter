/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var _ vendors.BillingDescriber = (*Driver)(nil)

// BillingRule implements vendors.BillingDescriber. Compute Engine bills per
// second with a one minute minimum (Keeper's row says 1/60). The compute pricing
// page could not be read when the plan was written, so the rule is reported
// unverified: billing retention stays off for GCP and the NodeClass status says
// Unverified (ADR-017, ADR-024). Were it verified, the minimum is far below the
// hold floor of a retention period and would never hold a node anyway.
func (d *Driver) BillingRule(string, string) (vendors.BillingRule, bool) {
	inc, minimum := time.Second, time.Minute
	return vendors.BillingRule{
		Increment: &inc, Minimum: &minimum, Verified: false,
		Source: "Keeper's vendor row (per second, one minute minimum); Google's pricing page not read",
	}, true
}
