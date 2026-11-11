/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import "time"

// BillingFacts are the billing facts Keeper's vendor table has for a vendor
// (ADR-024). A fact Keeper has as null is unknown (Has* false), never zero.
type BillingFacts struct {
	// Increment is the granularity compute is billed in; Minimum the minimum
	// billed duration of a run.
	Increment, Minimum       time.Duration
	HasIncrement, HasMinimum bool
	// StoppedCharged is nil when Keeper does not say.
	StoppedCharged *bool
	// Comment is Keeper's free text on billing exceptions.
	Comment string
}

// BillingFacts returns the vendor's billing facts of the current snapshot. ok
// is false when there is no snapshot or it has no row for the vendor (the
// vendor table is read with the rest of the refresh; a table that could not be
// read leaves it empty).
func (c *Catalog) BillingFacts(vendor string) (BillingFacts, bool) {
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()
	if snap == nil {
		return BillingFacts{}, false
	}
	row, ok := snap.vendorRows[vendor]
	if !ok {
		return BillingFacts{}, false
	}
	var f BillingFacts
	if row.BillingIncrementSeconds != nil {
		f.Increment, f.HasIncrement = time.Duration(*row.BillingIncrementSeconds)*time.Second, true
	}
	if row.MinimumBillingSeconds != nil {
		f.Minimum, f.HasMinimum = time.Duration(*row.MinimumBillingSeconds)*time.Second, true
	}
	f.StoppedCharged = row.StoppedServerCharged
	f.Comment = row.BillingComment
	return f, true
}
