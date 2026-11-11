/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// BillingRule implements vendors.BillingDescriber (ADR-024, plan 23). OVH's
// billing page says EU, CA and APAC regions bill per second with a one-minute
// minimum (https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/analyze-billing),
// which is under the hold floor of billing retention, so nothing is held there.
// For the US regions the terms say hourly, rounded up (their timed example, 9:50
// to 10:10 is one hour), and the US support guide says per second: conflicting. A
// stopped or paused instance is still billed.
//
// Every rule is reported unverified: no live check has confirmed it, so billing
// retention stays off for OVH until it has (plan 18, "Verify live": create and
// delete at 1, 59 and 61 minutes per region and read the usage per instance).
// Keeper's own row says 3600/3600 for the vendor.
func (d *Driver) BillingRule(region, _ string) (vendors.BillingRule, bool) {
	charged := true
	inc, minimum := time.Second, time.Minute
	source := "OVH billing page: per second with a one-minute minimum in EU, CA and APAC (not confirmed live)"
	if strings.HasPrefix(strings.ToUpper(region), "US-") {
		inc, minimum = time.Hour, time.Hour
		source = "OVH terms: hourly, rounded up, in the US; the US support guide says per second " +
			"(conflicting, not confirmed live)"
	}
	return vendors.BillingRule{
		Increment: &inc, Minimum: &minimum, StoppedCharged: &charged, Verified: false, Source: source,
	}, true
}
