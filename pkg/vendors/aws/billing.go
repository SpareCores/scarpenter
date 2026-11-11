/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var _ vendors.BillingDescriber = (*Driver)(nil)

// BillingRule implements vendors.BillingDescriber. EC2 Linux instances are
// billed per second with a 60 second minimum, as the on-demand pricing page
// says (https://aws.amazon.com/ec2/pricing/on-demand/, "Per second billing"),
// the same as Keeper's vendor row. The minimum is under the hold floor of
// billing retention, so the rule never holds a node; it is reported verified
// so that the NodeClass status says MinimumTooShort instead of Unverified.
func (d *Driver) BillingRule(string, string) (vendors.BillingRule, bool) {
	inc, minimum := time.Second, time.Minute
	return vendors.BillingRule{
		Increment: &inc, Minimum: &minimum, Verified: true,
		Source: "EC2 on-demand pricing: per second, 60 second minimum",
	}, true
}
