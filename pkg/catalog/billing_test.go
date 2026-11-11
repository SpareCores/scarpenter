/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SpareCores/sc-keeper-go/keepertest"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

func intp(v int) *int { return &v }

func TestBillingFactsFromTheVendorTable(t *testing.T) {
	c := catalog.New(catalog.Options{Source: catalogtest.SourceAt(catalogtest.ServerWithVendors(t,
		catalogtest.VendorRow{VendorID: "aws", Status: "active", BillingIncrementSeconds: intp(1), MinimumBillingSeconds: intp(60)},
		catalogtest.VendorRow{VendorID: "alicloud", Status: "active", MinimumBillingSeconds: intp(600)},
		catalogtest.VendorRow{VendorID: "azure", Status: "active"},
	).URL), Facts: catalogtest.Facts(t, "aws")})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	f, ok := c.BillingFacts("aws")
	if !ok || !f.HasIncrement || f.Increment != time.Second || !f.HasMinimum || f.Minimum != time.Minute {
		t.Errorf("aws: %+v %v", f, ok)
	}
	f, ok = c.BillingFacts("alicloud")
	if !ok || f.HasIncrement || !f.HasMinimum || f.Minimum != 10*time.Minute {
		t.Errorf("alicloud (null increment): %+v %v", f, ok)
	}
	f, ok = c.BillingFacts("azure")
	if !ok || f.HasIncrement || f.HasMinimum || f.StoppedCharged != nil {
		t.Errorf("azure (all null): %+v %v", f, ok)
	}
	if _, ok := c.BillingFacts("hcloud"); ok {
		t.Error("a vendor without a row has no facts")
	}
}

// The committed snapshot carries the vendor table, with a null row.
func TestSnapshotHasVendorTable(t *testing.T) {
	c, _ := refreshed(t, catalog.Scope{"aws": {"us-east-1"}}, "aws")
	if f, ok := c.BillingFacts("aws"); !ok || f.Increment != time.Second || f.Minimum != time.Minute {
		t.Errorf("aws: %+v %v", f, ok)
	}
	if f, ok := c.BillingFacts("azure"); !ok || f.HasIncrement || f.HasMinimum {
		t.Errorf("azure: %+v %v", f, ok)
	}
}

// A vendor table that cannot be read never fails the refresh: the facts are
// missing, nothing else is.
func TestMissingVendorTableDoesNotFailTheRefresh(t *testing.T) {
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/table/vendor" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := catalog.New(catalog.Options{Source: quickKeeper(srv.URL), Facts: catalogtest.Facts(t, "aws")})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"aws": {"us-east-1"}})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatalf("the refresh must succeed: %v", err)
	}
	if _, ok := c.BillingFacts("aws"); ok {
		t.Error("no vendor table, no facts")
	}
	if !c.Status("c").Ready {
		t.Error("the class is ready without the vendor table")
	}
}
