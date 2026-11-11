/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

// AWS bills per second with a 60 second minimum: the rule is verified and under
// the hold floor, so billing retention reports MinimumTooShort and never holds.
func TestBillingRule(t *testing.T) {
	var d vendors.BillingDescriber = &aws.Driver{}
	rule, ok := d.BillingRule("us-east-1", "m7i")
	if !ok || !rule.Verified || rule.Source == "" || rule.Increment == nil || *rule.Increment != time.Second ||
		rule.Minimum == nil || *rule.Minimum != time.Minute || rule.DeleteLatency != 0 {
		t.Errorf("%+v %v", rule, ok)
	}
}
