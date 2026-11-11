/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package billing_test

import (
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/billing"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const (
	minute = time.Minute
	hour   = time.Hour
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func facts(inc, minimum time.Duration) catalog.BillingFacts {
	f := catalog.BillingFacts{}
	if inc > 0 {
		f.Increment, f.HasIncrement = inc, true
	}
	if minimum > 0 {
		f.Minimum, f.HasMinimum = minimum, true
	}
	return f
}

func override(inc, minimum time.Duration, verified bool) *vendors.BillingRule {
	r := &vendors.BillingRule{Verified: verified, Source: "test"}
	if inc > 0 {
		r.Increment = &inc
	}
	if minimum > 0 {
		r.Minimum = &minimum
	}
	return r
}

func TestResolve(t *testing.T) {
	stopped := true
	for _, tc := range []struct {
		name     string
		keeper   catalog.BillingFacts
		override *vendors.BillingRule
		inc, min time.Duration
		verified bool
	}{
		{"hourly, verified", facts(hour, hour), override(0, 0, true), hour, hour, true},
		{"hourly without a minimum", facts(hour, 0), override(0, 0, true), hour, 0, true},
		{"hourly, no override: unverified", facts(hour, hour), nil, hour, hour, false},
		{"hourly, override unverified", facts(hour, hour), override(0, 0, false), hour, hour, false},
		{"minimum only: 10m over per second", facts(time.Second, 10*minute), override(0, 0, true), time.Second, 10 * minute, true},
		{"90m minimum over a 60m increment: Assess decides the shape", facts(hour, 90*minute), override(0, 0, true), hour, 90 * minute, true},
		{"30m minimum over a 60m increment: Assess decides the shape", facts(hour, 30*minute), override(0, 0, true), hour, 30 * minute, true},
		{"override wins field by field", facts(time.Second, minute), override(0, 5*minute, true), time.Second, 5 * minute, true},
		{"override gives the increment Keeper lacks", facts(0, 0), override(hour, hour, true), hour, hour, true},
		{"override wins over the minimum", facts(hour, hour), override(0, 90*minute, true), hour, 90 * minute, true},
		{"null Keeper fields, no override", facts(0, 0), nil, 0, 0, false},
		{"null increment never verifies", facts(0, 10*minute), override(0, 0, true), 0, 10 * minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := billing.Resolve(tc.keeper, tc.override)
			if r.Increment != tc.inc || r.Minimum != tc.min || r.Verified != tc.verified {
				t.Errorf("got %+v, want increment %s minimum %s verified %v", r, tc.inc, tc.min, tc.verified)
			}
		})
	}

	t.Run("stopped and latency come from the override, source names both", func(t *testing.T) {
		keeper := facts(hour, hour)
		keeper.StoppedCharged = &stopped
		ov := override(0, 0, true)
		ov.DeleteLatency = 2 * minute
		r := billing.Resolve(keeper, ov)
		if !r.StoppedCharged || r.DeleteLatency != 2*minute || r.Source != "test; Keeper vendor table" {
			t.Errorf("%+v", r)
		}
	})
}

func TestWindow(t *testing.T) {
	hourly := billing.Resolve(facts(hour, hour), override(0, 0, true))
	min10 := billing.Resolve(facts(time.Second, 10*minute), override(0, 0, true))
	min5 := billing.Resolve(facts(time.Second, 5*minute), override(0, 0, true))
	slowDelete := hourly
	slowDelete.DeleteLatency = 3 * minute // effective margin 4m30s
	const minPeriod, margin = 10 * minute, 3 * minute

	for _, tc := range []struct {
		name     string
		rule     billing.Rule
		start    time.Time
		now      time.Time
		boundary time.Duration // from t0, zero start only
		deadline time.Duration
		inMargin bool
		last     bool
		zero     bool
	}{
		{name: "just started", rule: hourly, start: t0, now: t0, boundary: hour, deadline: hour - 3*minute},
		{name: "mid first period", rule: hourly, start: t0, now: t0.Add(20 * minute), boundary: hour, deadline: 57 * minute},
		{name: "one second before the deadline", rule: hourly, start: t0, now: t0.Add(57*minute - time.Second),
			boundary: hour, deadline: 57 * minute},
		{name: "exactly on the deadline", rule: hourly, start: t0, now: t0.Add(57 * minute), boundary: hour,
			deadline: 57 * minute, inMargin: true},
		{name: "inside the margin", rule: hourly, start: t0, now: t0.Add(59 * minute), boundary: hour,
			deadline: 57 * minute, inMargin: true},
		{name: "exactly on a boundary starts the next period", rule: hourly, start: t0, now: t0.Add(hour),
			boundary: 2 * hour, deadline: 2*hour - 3*minute},
		{name: "many periods in", rule: hourly, start: t0, now: t0.Add(37*hour + 58*minute),
			boundary: 38 * hour, deadline: 38*hour - 3*minute, inMargin: true},
		{name: "a start in the future counts the first period", rule: hourly, start: t0.Add(time.Minute), now: t0,
			boundary: hour + minute, deadline: hour + minute - 3*minute},
		{name: "the vendor's delete latency widens the margin", rule: slowDelete, start: t0, now: t0.Add(56 * minute),
			boundary: hour, deadline: hour - 4*minute - 30*time.Second, inMargin: true},
		{name: "minimum only: 600 s holds 7 minutes", rule: min10, start: t0, now: t0.Add(6 * minute),
			boundary: 10 * minute, deadline: 7 * minute, last: true},
		{name: "minimum only: at the deadline", rule: min10, start: t0, now: t0.Add(7 * minute),
			boundary: 10 * minute, deadline: 7 * minute, inMargin: true, last: true},
		{name: "minimum only: past the boundary stays the one boundary", rule: min10, start: t0, now: t0.Add(30 * minute),
			boundary: 10 * minute, deadline: 7 * minute, inMargin: true, last: true},
		{name: "minimum only: 300 s holds 2 minutes", rule: min5, start: t0, now: t0.Add(minute),
			boundary: 5 * minute, deadline: 2 * minute, last: true},
		{name: "zero start", rule: hourly, now: t0, zero: true},
		{name: "a rule with neither increments to hold nor a minimum", rule: billing.Resolve(facts(time.Second, 0), nil),
			start: t0, now: t0, zero: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boundary, deadline, inMargin, last := billing.Window(tc.start, tc.rule, minPeriod, margin, tc.now)
			if tc.zero {
				if !boundary.IsZero() || !deadline.IsZero() || inMargin || last {
					t.Errorf("want zero values, got %s %s %v %v", boundary, deadline, inMargin, last)
				}
				return
			}
			if want := t0.Add(tc.boundary); !boundary.Equal(want) {
				t.Errorf("boundary %s, want %s", boundary, want)
			}
			if want := t0.Add(tc.deadline); !deadline.Equal(want) {
				t.Errorf("deadline %s, want %s", deadline, want)
			}
			if inMargin != tc.inMargin || last != tc.last {
				t.Errorf("inMargin %v last %v, want %v %v", inMargin, last, tc.inMargin, tc.last)
			}
		})
	}
}

// A period that is not a whole number of hours counts from the start all the
// same: nothing in the arithmetic assumes the hour.
func TestWindowCountsAnyPeriod(t *testing.T) {
	five := billing.Resolve(facts(15*minute, 15*minute), override(0, 0, true))
	boundary, deadline, in, last := billing.Window(t0, five, 10*minute, 3*minute, t0.Add(31*minute))
	if !boundary.Equal(t0.Add(45*minute)) || !deadline.Equal(t0.Add(42*minute)) || in || last {
		t.Errorf("%s %s %v %v", boundary, deadline, in, last)
	}
}

func TestAssess(t *testing.T) {
	on := func(minPeriod, margin time.Duration) billing.Settings {
		return billing.Settings{Enabled: true, MinPeriod: minPeriod, Margin: margin}
	}
	def := on(10*minute, 3*minute)
	hourly := billing.Resolve(facts(hour, hour), override(0, 0, true))
	for _, tc := range []struct {
		name      string
		rule      billing.Rule
		keeperRow bool
		settings  billing.Settings
		active    bool
		reason    string
		message   string
		period    time.Duration
	}{
		{"hourly", hourly, true, def, true, billing.ReasonActive, "increments", hour},
		{"disabled", hourly, true, billing.Settings{MinPeriod: 10 * minute, Margin: 3 * minute}, false, billing.ReasonDisabled, "", 0},
		{"no Keeper row", hourly, false, def, false, billing.ReasonNoKeeperData, "", 0},
		{"null increment", billing.Resolve(facts(0, 10*minute), override(0, 0, true)), true, def, false, billing.ReasonNoKeeperData, "", 0},
		{"unverified", billing.Resolve(facts(hour, hour), nil), true, def, false, billing.ReasonUnverified, "", 0},
		{"90m over 60m", billing.Resolve(facts(hour, 90*minute), override(0, 0, true)), true, def, false, billing.ReasonUnsupportedShape, "", 0},
		{"30m over 60m", billing.Resolve(facts(hour, 30*minute), override(0, 0, true)), true, def, false, billing.ReasonUnsupportedShape, "", 0},
		{"per second with no minimum", billing.Resolve(facts(time.Second, 0), override(0, 0, true)), true, def, false, billing.ReasonPerSecondBilling, "", 0},
		{"per second with a 60 s minimum (AWS)", billing.Resolve(facts(time.Second, minute), override(0, 0, true)), true, def, false, billing.ReasonMinimumTooShort, "", 0},
		{"120 s minimum never holds", billing.Resolve(facts(time.Second, 2*minute), override(0, 0, true)), true, def, false, billing.ReasonMinimumTooShort, "", 0},
		{"300 s minimum holds 2 minutes", billing.Resolve(facts(time.Second, 5*minute), override(0, 0, true)), true, def, true, billing.ReasonActive, "minimum only", 5 * minute},
		{"600 s minimum holds 7 minutes", billing.Resolve(facts(time.Second, 10*minute), override(0, 0, true)), true, def, true, billing.ReasonActive, "minimum only", 10 * minute},
		// minimum - margin = 2m exactly is the hold floor and holds.
		{"the hold floor exactly", billing.Resolve(facts(time.Second, 5*minute), override(0, 0, true)), true, on(10*minute, 3*minute), true, billing.ReasonActive, "minimum only", 5 * minute},
		{"one second under the floor", billing.Resolve(facts(time.Second, 5*minute-time.Second), override(0, 0, true)), true, def, false, billing.ReasonMinimumTooShort, "", 0},
		{"a bigger margin lifts the floor", billing.Resolve(facts(time.Second, 5*minute), override(0, 0, true)), true, on(10*minute, 4*minute), false, billing.ReasonMinimumTooShort, "", 0},
		// 10m over 5m: the 5m increment is under minPeriod, so the rule is
		// minimum-only (decision 12), not unsupported; with minPeriod 5m the
		// increment is held and the shape is unsupported.
		{"10m over 5m is minimum-only", billing.Resolve(facts(5*minute, 10*minute), override(0, 0, true)), true, def, true, billing.ReasonActive, "minimum only", 10 * minute},
		{"10m over 5m held at minPeriod 5m", billing.Resolve(facts(5*minute, 10*minute), override(0, 0, true)), true, on(5*minute, 2*minute), false, billing.ReasonUnsupportedShape, "", 0},
		{"90m over 60m with minPeriod 2h is minimum-only", billing.Resolve(facts(hour, 90*minute), override(0, 0, true)), true, on(2*hour, 3*minute), true, billing.ReasonActive, "minimum only", 90 * minute},
		{"increment under minPeriod is not held", billing.Resolve(facts(5*minute, 5*minute), override(0, 0, true)), true, def, true, billing.ReasonActive, "minimum only", 5 * minute},
		{"minPeriod under twice the margin", hourly, true, on(5*minute, 3*minute), false, billing.ReasonInvalidSettings, "", 0},
		{"period under twice the margin", billing.Resolve(facts(5*minute, 5*minute), override(0, 0, true)), true, on(5*minute, 3*minute), false, billing.ReasonInvalidSettings, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.rule.Assess(tc.keeperRow, tc.settings)
			if st.Active != tc.active || st.Reason != tc.reason || st.Period != tc.period ||
				(tc.message != "" && st.Message != tc.message) || (!tc.active && st.Message == "") {
				t.Errorf("got %+v", st)
			}
		})
	}

	t.Run("a held period under twice the delete-latency margin", func(t *testing.T) {
		r := billing.Resolve(facts(10*minute, 10*minute), override(0, 0, true))
		r.DeleteLatency = 4 * minute // effective margin 5m30s: 10m < 11m
		if st := r.Assess(true, def); st.Active || st.Reason != billing.ReasonPeriodTooShort {
			t.Errorf("%+v", st)
		}
	})
}

// A 10-minute minimum over 5-minute increments is minimum-only at the default
// minPeriod (one boundary at start + 10m), and gives no window when minPeriod
// holds the increment (the unsupported shape).
func TestWindowMinimumOverAShorterIncrement(t *testing.T) {
	r := billing.Resolve(facts(5*minute, 10*minute), override(0, 0, true))
	boundary, deadline, in, last := billing.Window(t0, r, 10*minute, 3*minute, t0.Add(6*minute))
	if !boundary.Equal(t0.Add(10*minute)) || !deadline.Equal(t0.Add(7*minute)) || in || !last {
		t.Errorf("minimum-only: %s %s %v %v", boundary, deadline, in, last)
	}
	if boundary, _, _, _ := billing.Window(t0, r, 5*minute, 2*minute, t0.Add(time.Minute)); !boundary.IsZero() {
		t.Errorf("unsupported shape gives a boundary %s", boundary)
	}
}
