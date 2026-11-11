/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package billing holds the pure arithmetic of billing-period-aware node
// retention (ADR-024, plan 23): the rule a vendor's billing resolves to from
// Keeper's vendor row and the driver's override (Resolve), whether the rule
// makes a node worth holding (Rule.Assess) and the period boundaries and
// release deadlines of a node (Window). It has no Kubernetes client and no
// clock of its own, and it never branches on a vendor name.
package billing

import (
	"fmt"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Constants of the retention arithmetic.
const (
	// HoldFloor is the shortest window worth a hold: a minimum that leaves
	// less than this between the start and the deadline is not held (with the
	// default 3 minute margin a 5 minute minimum holds 2 minutes, a 10 minute
	// one 7, and 120 seconds never).
	HoldFloor = 2 * time.Minute
	// MinMargin is the lowest margin the API accepts (spec.billing.margin).
	MinMargin = 90 * time.Second
	// LatencyAllowance is added to a driver's DeleteLatency to get the least
	// margin a vendor needs: drain, the termination finalizer and skew.
	LatencyAllowance = 90 * time.Second
	// SkewAllowance is how far in the future a billing start may lie before it
	// is not believed (clock skew between the controller and the vendor).
	SkewAllowance = 2 * time.Minute
)

// Reasons of the per-vendor status (status.billing[].reason).
const (
	ReasonActive           = "Active"
	ReasonDisabled         = "Disabled"
	ReasonPerSecondBilling = "PerSecondBilling"
	ReasonMinimumTooShort  = "MinimumTooShort"
	ReasonUnverified       = "Unverified"
	ReasonUnsupportedShape = "UnsupportedShape"
	ReasonNoKeeperData     = "NoKeeperData"
	ReasonNoBillingStart   = "NoBillingStart"
	// ReasonPeriodTooShort: a held increment shorter than twice the effective
	// margin leaves no window between two deadlines.
	ReasonPeriodTooShort = "PeriodTooShort"
	// ReasonInvalidSettings: spec.billing.minPeriod below twice the margin.
	ReasonInvalidSettings = "InvalidSettings"
)

// Shape is how a rule's boundaries are laid out.
type Shape string

// The shapes Resolve supports, and none.
const (
	// ShapeNone: nothing is known or nothing is worth holding.
	ShapeNone Shape = ""
	// ShapeIncrements: the boundaries are start + n*Increment (hourly vendors).
	ShapeIncrements Shape = "increments"
	// ShapeMinimumOnly: one boundary, start + Minimum, then per second.
	ShapeMinimumOnly Shape = "minimum only"
)

// Rule is a vendor's billing rule for a region and family: the charge of a run
// of t seconds is Minimum up to Minimum, then t rounded up to whole increments.
// Zero Increment means unknown, zero Minimum none.
type Rule struct {
	Increment      time.Duration
	Minimum        time.Duration
	StoppedCharged bool
	DeleteLatency  time.Duration
	// Source says where the rule comes from, for the status.
	Source string
	// Verified is true only when the driver says a live check confirmed the
	// rule (and Keeper or the driver gives an increment).
	Verified bool
}

// Resolve combines Keeper's vendor row with the driver's override, field by
// field: a field the override sets wins, the rest is Keeper's. A driver without
// an override (nil) is unverified. Whether the rule's shape is supported
// depends on spec.billing.minPeriod, so Assess decides it: increments held
// (Increment >= minPeriod) with Minimum equal to Increment (or no minimum), and
// minimum-only (Increment < minPeriod, one boundary at start + Minimum); a
// minimum different from a held increment is UnsupportedShape, never guessed.
func Resolve(keeper catalog.BillingFacts, override *vendors.BillingRule) Rule {
	var r Rule
	source := ""
	if keeper.HasIncrement || keeper.HasMinimum || keeper.StoppedCharged != nil {
		source = "Keeper vendor table"
	}
	if keeper.HasIncrement {
		r.Increment = keeper.Increment
	}
	if keeper.HasMinimum {
		r.Minimum = keeper.Minimum
	}
	if keeper.StoppedCharged != nil {
		r.StoppedCharged = *keeper.StoppedCharged
	}
	if override != nil {
		if override.Increment != nil {
			r.Increment = *override.Increment
		}
		if override.Minimum != nil {
			r.Minimum = *override.Minimum
		}
		if override.StoppedCharged != nil {
			r.StoppedCharged = *override.StoppedCharged
		}
		r.DeleteLatency = override.DeleteLatency
		src := override.Source
		if src == "" {
			src = "driver rule"
		}
		if source != "" {
			src += "; " + source
		}
		source = src
		r.Verified = override.Verified
	}
	r.Source = source
	if r.Increment <= 0 {
		r.Verified = false
	}
	return r
}

// Settings are the parts of spec.billing the arithmetic needs.
type Settings struct {
	// Enabled is retainUntilPeriodEnd.
	Enabled   bool
	MinPeriod time.Duration
	Margin    time.Duration
}

// EffectiveMargin is the margin the deadlines use: the larger of the NodeClass's
// and the vendor's delete latency plus its allowance.
func EffectiveMargin(margin time.Duration, r Rule) time.Duration {
	return max(margin, r.DeleteLatency+LatencyAllowance)
}

// Status is what a rule comes to for a NodeClass (status.billing[]).
type Status struct {
	Active bool
	Reason string
	// Message explains the reason; for Active it is the shape.
	Message string
	Shape   Shape
	// Period is the held increment, or the minimum for the minimum-only shape.
	Period time.Duration
	// Margin is the effective margin.
	Margin time.Duration
}

// held reports whether the rule's increments are held: the increment is at
// least minPeriod.
func (r Rule) held(minPeriod time.Duration) bool {
	return r.Increment > 0 && r.Increment >= minPeriod
}

// unsupported reports a shape that is not supported under minPeriod (ADR-024,
// plan 23 decision 12): increments held with a minimum different from the
// increment (90m over 60m, 30m over 60m). With the increment under minPeriod
// the same rule is minimum-only, which is supported (10m over 5m holds until
// start + 10m less the margin).
func (r Rule) unsupported(minPeriod time.Duration) bool {
	return r.held(minPeriod) && r.Minimum > 0 && r.Minimum != r.Increment
}

// Assess says whether a vendor's nodes can be held under the settings, and if
// not why. keeperRow is whether Keeper has a vendor row at all. The order of
// the reasons is the order an operator should fix them in.
func (r Rule) Assess(keeperRow bool, s Settings) Status {
	margin := EffectiveMargin(s.Margin, r)
	no := func(reason, format string, args ...any) Status {
		return Status{Reason: reason, Message: fmt.Sprintf(format, args...), Margin: margin}
	}
	switch {
	case !s.Enabled:
		return no(ReasonDisabled, "spec.billing.retainUntilPeriodEnd is false")
	case !keeperRow || r.Increment <= 0:
		return no(ReasonNoKeeperData, "Keeper's vendor table has no billing increment for the vendor")
	case r.unsupported(s.MinPeriod):
		return no(ReasonUnsupportedShape,
			"a minimum of %s differs from the %s increment held at minPeriod %s; only equal or minimum-only rules are supported",
			r.Minimum, r.Increment, s.MinPeriod)
	case !r.Verified:
		return no(ReasonUnverified, "the vendor's billing rule has not been verified on a live account")
	}
	if r.held(s.MinPeriod) {
		switch {
		case s.MinPeriod < 2*s.Margin:
			return no(ReasonInvalidSettings, "minPeriod %s is below twice the margin %s", s.MinPeriod, s.Margin)
		case r.Increment < 2*margin:
			return no(ReasonPeriodTooShort, "the %s period is shorter than twice the margin %s", r.Increment, margin)
		}
		return Status{Active: true, Reason: ReasonActive, Message: string(ShapeIncrements), Shape: ShapeIncrements,
			Period: r.Increment, Margin: margin}
	}
	switch {
	case r.Minimum <= 0:
		return no(ReasonPerSecondBilling, "billed per %s with no minimum", r.Increment)
	case r.Minimum-margin < HoldFloor:
		return no(ReasonMinimumTooShort,
			"a minimum of %s less the margin %s leaves under %s to hold", r.Minimum, margin, HoldFloor)
	}
	return Status{Active: true, Reason: ReasonActive, Message: string(ShapeMinimumOnly), Shape: ShapeMinimumOnly,
		Period: r.Minimum, Margin: margin}
}

// Window is the arithmetic of one node. The anchor is always start, the
// vendor's billing start (there is no clock-hour anchor, ADR-024).
//
// With increments held (Increment >= minPeriod, and Minimum equal to it or
// absent) the boundary is the next start + n*Increment strictly after now.
// Otherwise the only boundary is start + Minimum and last is true: there is no
// next period, and the boundary stays that one after it passed.
// deadline is boundary less the effective margin (the larger of margin and the
// rule's DeleteLatency plus its allowance) and inMargin is now >= deadline.
//
// A zero start, a rule without a boundary or an unsupported shape gives zero
// times and false: the caller must not hold the node.
func Window(start time.Time, r Rule, minPeriod, margin time.Duration, now time.Time) (
	boundary, deadline time.Time, inMargin, last bool) {
	if start.IsZero() || r.unsupported(minPeriod) {
		return time.Time{}, time.Time{}, false, false
	}
	switch {
	case r.held(minPeriod):
		n := int64(1)
		if elapsed := now.Sub(start); elapsed > 0 {
			n = int64(elapsed/r.Increment) + 1
		}
		boundary = start.Add(time.Duration(n) * r.Increment)
	case r.Minimum > 0:
		boundary, last = start.Add(r.Minimum), true
	default:
		return time.Time{}, time.Time{}, false, false
	}
	deadline = boundary.Add(-EffectiveMargin(margin, r))
	return boundary, deadline, !now.Before(deadline), last
}

// Describe renders a rule for logs and the status ("1h0m0s increments").
func (r Rule) Describe() string {
	parts := []string{"increment " + r.Increment.String()}
	if r.Minimum > 0 {
		parts = append(parts, "minimum "+r.Minimum.String())
	}
	return strings.Join(parts, ", ")
}
