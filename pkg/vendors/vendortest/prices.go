/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package vendortest has helpers the driver tests share.
package vendortest

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SpareCores/sc-keeper-go/columnar"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// StoragePrices reads a JSON array of Keeper storage price rows (extra fields
// are ignored) and returns the rows of the vendor that the catalog hands to a
// driver: status active, currency USD, a price above zero. The unit is not
// filtered: the driver ignores what it cannot price.
func StoragePrices(t testing.TB, path, vendor string) []vendors.StoragePrice {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a test fixture
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		VendorID  string  `json:"vendor_id"`
		RegionID  string  `json:"region_id"`
		StorageID string  `json:"storage_id"`
		Unit      string  `json:"unit"`
		Price     float64 `json:"price"`
		Currency  string  `json:"currency"`
		Status    string  `json:"status"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	var out []vendors.StoragePrice
	for _, r := range rows {
		if r.VendorID == vendor && r.Status == "active" && r.Currency == "USD" && r.Price > 0 {
			out = append(out, vendors.StoragePrice{Region: r.RegionID, StorageID: r.StorageID, Unit: r.Unit, USD: r.Price})
		}
	}
	return out
}

// SnapshotRows decodes one per-vendor file of the committed Keeper snapshot
// (test/data/sc-snapshot/snapshot/<table>/<vendor>.json.gz, columnar JSON) into
// rows of T, matched to the columns by their json tags.
func SnapshotRows[T any](t testing.TB, table, vendor string) []T {
	t.Helper()
	path := filepath.Join(testutil.RepoRoot(), "test", "data", "sc-snapshot", "snapshot", table, vendor+".json.gz")
	f, err := os.Open(path) //nolint:gosec // a test fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := columnar.DecodeRows[T](zr, -1)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
