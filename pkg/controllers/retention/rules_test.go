/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention_test

import (
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/billing"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/controllers/retention"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// describer is a driver that only describes its billing.
type describer struct {
	vendors.Driver
	vendor string
	rule   vendors.BillingRule
}

func (d describer) Vendor() string { return d.vendor }
func (d describer) BillingRule(region, family string) (vendors.BillingRule, bool) {
	return d.rule, region != "no-rule"
}

// plain is a driver without the capability.
type plain struct {
	vendors.Driver
	vendor string
}

func (p plain) Vendor() string { return p.vendor }

// The rule of a vendor is Keeper's row with the driver's override on top; a
// driver without the capability is unverified, and a vendor without a row has no
// Keeper data.
func TestCatalogRules(t *testing.T) {
	url := catalogtest.ServerWithVendors(t,
		catalogtest.VendorRow{VendorID: "hcloud", Status: "active", BillingIncrementSeconds: new(3600), MinimumBillingSeconds: new(3600)},
		catalogtest.VendorRow{VendorID: "aws", Status: "active", BillingIncrementSeconds: new(1), MinimumBillingSeconds: new(60)},
	).URL
	cat := catalog.New(catalog.Options{Source: catalogtest.SourceAt(url), Facts: catalogtest.Facts(t, "aws")})
	cat.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})})
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	ten := 10 * time.Minute
	reg := vendors.NewRegistryFrom(
		describer{vendor: "aws", rule: vendors.BillingRule{Minimum: &ten, Verified: true, Source: "docs", DeleteLatency: 2 * time.Minute}},
		plain{vendor: "hcloud"},
	)
	rules := retention.CatalogRules{Catalog: cat, Registry: reg}

	r, row := rules.Rule("aws", "us-east-1", "m7i")
	if !row || r.Increment != time.Second || r.Minimum != ten || !r.Verified || r.DeleteLatency != 2*time.Minute {
		t.Errorf("aws: %+v row=%v: the driver's minimum wins, Keeper's increment stays", r, row)
	}
	if st := r.Assess(row, billing.Settings{Enabled: true, MinPeriod: 10 * time.Minute, Margin: 3 * time.Minute}); !st.Active ||
		st.Margin != 210*time.Second {
		t.Errorf("assess: %+v (the margin is widened by the delete latency: 2m + 90s)", st)
	}
	r, row = rules.Rule("aws", "no-rule", "m7i")
	if !row || r.Verified {
		t.Errorf("a driver with nothing to add leaves Keeper's row, unverified: %+v", r)
	}
	r, row = rules.Rule("hcloud", "fsn1", "cx")
	if !row || r.Increment != hour || r.Verified {
		t.Errorf("hcloud (no capability): %+v row=%v", r, row)
	}
	if _, row := rules.Rule("ovh", "gra", "b2"); row {
		t.Error("a vendor without a row has no Keeper data")
	}
}
