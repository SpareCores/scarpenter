/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors_test

import (
	"testing"
	"time"

	testingclock "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestTerminatingTracker(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Unix(1000, 0))
	tr := vendors.NewTerminatingTracker(clk, 0)
	a, b := vendors.NewInstanceRef("r1", "i-a"), vendors.NewInstanceRef("r2", "i-b")

	if tr.Expired("v", a) || tr.Expired("v", b) {
		t.Fatal("the first sighting is inside the grace")
	}
	clk.Step(vendors.DefaultTerminatingGrace - time.Second)
	if tr.Expired("v", a) {
		t.Error("expired one second early")
	}
	clk.Step(time.Second)
	if !tr.Expired("v", a) {
		t.Error("not expired after the grace")
	}
	// A complete listing of r1 without a drops its clock; r2 is untouched.
	tr.Retain("v", "r1", map[vendors.InstanceRef]bool{})
	if tr.Expired("v", a) {
		t.Error("a forgotten instance restarts the clock")
	}
	if !tr.Expired("v", b) {
		t.Error("another region's entry was dropped")
	}
	tr.Retain("v", "r2", map[vendors.InstanceRef]bool{b: true})
	if !tr.Expired("v", b) {
		t.Error("a retained entry lost its clock")
	}
	tr.Forget("v", b)
	if tr.Expired("v", b) {
		t.Error("Forget keeps the clock")
	}
	// The vendor is part of the key.
	clk.Step(time.Hour)
	if !tr.Expired("v", a) || tr.Expired("other", a) {
		t.Error("vendors share a clock")
	}
}
