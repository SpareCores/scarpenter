/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var _ vendors.BillingDescriber = (*Driver)(nil)

// BillingRule implements vendors.BillingDescriber. UpCloud bills cloud servers per
// hour, a month capped at 672 hours (the monthly price divided by 672); Keeper's
// vendor row says the same (3600 s increment, 3600 s minimum, stopped servers
// charged). Nothing here is verified against a live account: whether the hour
// counts from the creation or from the first start, and whether the hour is the
// clock hour, are unknown, and a server in maintenance while its storage is cloned
// may or may not be billed. The rule is therefore reported unverified, which keeps
// billing retention off for UpCloud and says so in the NodeClass status, until a
// live check (plan 23: the billed hours of a server deleted at 59 and 61 minutes)
// has been recorded. DeleteLatency is the longest of the hourly vendors: the
// server is stopped first (waiting for stopped, bounded by StopTimeout), then
// deleted with its storages; the margin must cover it.
func (d *Driver) BillingRule(string, string) (vendors.BillingRule, bool) {
	inc, minimum, charged := time.Hour, time.Hour, true
	return vendors.BillingRule{
		Increment: &inc, Minimum: &minimum, StoppedCharged: &charged, DeleteLatency: StopTimeout,
		Verified: false,
		Source:   "UpCloud billing (hourly, a month capped at 672 hours); the start of the period is not verified live",
	}, true
}
