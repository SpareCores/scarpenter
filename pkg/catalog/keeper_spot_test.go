/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"testing"

	"github.com/SpareCores/scarpenter/pkg/catalog"
)

// spotOfferings counts the spot and on-demand offerings of a vendor's types.
func spotOfferings(t *testing.T, vendor string, regions ...string) (spot, onDemand int) {
	t.Helper()
	c, _ := refreshed(t, catalog.Scope{vendor: regions}, vendor)
	for _, it := range all(t, c, catalog.Filter{}) {
		for _, o := range it.Offerings {
			if o.Requirements.Get("karpenter.sh/capacity-type").Has("spot") {
				spot++
			} else {
				onDemand++
			}
		}
	}
	return spot, onDemand
}

// Spot offerings exist exactly where Keeper has spot price rows: nothing in the
// catalog lists the vendors that have spot (plan 26, section 7).
func TestSpotOfferingsComeFromKeepersSpotRows(t *testing.T) {
	for _, tc := range []struct {
		vendor  string
		region  string
		hasSpot bool
	}{
		{"aws", "us-east-1", true},
		{"gcp", "europe-west3", true},
		{"azure", "", true},
		{"alicloud", "", true},
		{"hcloud", "fsn1", false},
		{"ovh", "", false},
		{"vultr", "", false},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			var regions []string
			if tc.region != "" {
				regions = []string{tc.region}
			}
			spot, onDemand := spotOfferings(t, tc.vendor, regions...)
			if onDemand == 0 {
				t.Fatalf("%s has no on-demand offerings: the scope matches nothing", tc.vendor)
			}
			if got := spot > 0; got != tc.hasSpot {
				t.Errorf("%s has %d spot offerings, want some: %v", tc.vendor, spot, tc.hasSpot)
			}
		})
	}
}

// A spot offering carries Keeper's spot price, not the on-demand one.
func TestSpotOfferingsArePricedFromSpotRows(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"aws": {"us-east-1"}}, "aws")
	it := byName(all(t, c, catalog.Filter{}), "aws.m5.large")
	if it == nil {
		t.Fatal("no aws.m5.large")
	}
	cheapest := map[string]float64{}
	for _, o := range it.Offerings {
		capacity := o.Requirements.Get("karpenter.sh/capacity-type").Any()
		if p, ok := cheapest[capacity]; !ok || o.Price < p {
			cheapest[capacity] = o.Price
		}
	}
	spot, onDemand := cheapest["spot"], cheapest["on-demand"]
	if spot <= 0 || onDemand <= 0 || spot >= onDemand {
		t.Errorf("cheapest spot %v, cheapest on-demand %v: want 0 < spot < on-demand", spot, onDemand)
	}
}
