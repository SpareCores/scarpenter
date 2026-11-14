/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure_test

import (
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/billing"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
)

// Keeper's recorded Azure row has a 300 second minimum, which Azure's pricing
// page does not document: the driver's rule must replace it, or retention would
// hold nodes for five minutes.
func TestRecordedKeeperMinimumDoesNotLeakIntoRetention(t *testing.T) {
	var row *catalogtest.VendorRow
	rows := catalogtest.Rows[catalogtest.VendorRow](t, "vendor")
	for i := range rows {
		if rows[i].VendorID == "azure" {
			row = &rows[i]
		}
	}
	if row == nil || row.MinimumBillingSeconds == nil || *row.MinimumBillingSeconds == 0 {
		t.Fatalf("the recording has no azure minimum to override: %+v", row)
	}
	facts := catalog.BillingFacts{HasMinimum: true, Minimum: time.Duration(*row.MinimumBillingSeconds) * time.Second}
	if row.BillingIncrementSeconds != nil {
		facts.HasIncrement, facts.Increment = true, time.Duration(*row.BillingIncrementSeconds)*time.Second
	}
	rule, ok := azure.New().BillingRule("", "")
	if !ok {
		t.Fatal("no billing rule")
	}
	resolved := billing.Resolve(facts, &rule)
	if resolved.Minimum != 0 {
		t.Errorf("Minimum = %v, want none", resolved.Minimum)
	}
	st := resolved.Assess(true, billing.Settings{Enabled: true, MinPeriod: 10 * time.Minute, Margin: 3 * time.Minute})
	if st.Active {
		t.Errorf("retention applies to Azure: %+v", st)
	}
}
