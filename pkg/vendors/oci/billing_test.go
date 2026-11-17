/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/billing"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
)

func TestBillingRule(t *testing.T) {
	var d vendors.BillingDescriber = &driver.Driver{}
	for _, tc := range []struct {
		family  string
		charged *bool // nil: no override
	}{
		{"Standard", new(false)},
		{"standard", new(false)},
		{"Dense I/O", new(true)},
		{"Dense-I-O", new(true)},
		{"HPC", new(true)},
		{"Optimized", nil}, // VM.Optimized3 pauses, BM.Optimized3 continues
		{"GPU", nil},
		{"", nil},
		{"Other", nil},
	} {
		rule, ok := d.BillingRule("eu-frankfurt-1", tc.family)
		if !ok || rule.Verified || rule.Source == "" || rule.Increment == nil || *rule.Increment != time.Second ||
			rule.Minimum == nil || *rule.Minimum != time.Minute || rule.DeleteLatency != 0 {
			t.Errorf("%q: %+v %v", tc.family, rule, ok)
		}
		if (rule.StoppedCharged == nil) != (tc.charged == nil) ||
			rule.StoppedCharged != nil && *rule.StoppedCharged != *tc.charged {
			t.Errorf("%q: StoppedCharged = %v, want %v", tc.family, rule.StoppedCharged, tc.charged)
		}
	}
}

// Over Keeper's vendor row of oci (one hour minimum, stopped charged) the rule
// gives the VM minimum, and the Standard family does not charge a stopped server.
func TestBillingRuleOverKeepersRow(t *testing.T) {
	stopped := true
	keeperRow := catalog.BillingFacts{
		Increment: time.Second, HasIncrement: true, Minimum: time.Hour, HasMinimum: true, StoppedCharged: &stopped,
	}
	d := &driver.Driver{}
	rule, _ := d.BillingRule("eu-frankfurt-1", "Standard")
	got := billing.Resolve(keeperRow, &rule)
	if got.Minimum != time.Minute || got.Increment != time.Second || got.StoppedCharged || got.Verified {
		t.Errorf("resolved rule = %+v", got)
	}
	rule, _ = d.BillingRule("eu-frankfurt-1", "Dense I/O")
	if got = billing.Resolve(keeperRow, &rule); !got.StoppedCharged {
		t.Errorf("Dense I/O resolved to %+v", got)
	}
	rule, _ = d.BillingRule("eu-frankfurt-1", "Optimized")
	if got = billing.Resolve(keeperRow, &rule); !got.StoppedCharged {
		t.Errorf("Optimized has Keeper's row: %+v", got)
	}
}
