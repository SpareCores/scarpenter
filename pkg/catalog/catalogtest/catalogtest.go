/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package catalogtest has the helpers tests of the catalog and of its users
// share: a Keeper source that replays the committed snapshot
// (test/data/sc-snapshot) and driver facts derived from the snapshot's zone
// table. Keeper calls never leave the process.
package catalogtest

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SpareCores/sc-keeper-go/keepertest"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/catalog"
)

// SnapshotDir is the committed snapshot.
func SnapshotDir() string {
	return filepath.Join(testutil.RepoRoot(), "test", "data", "sc-snapshot")
}

// Server starts the replay server of the snapshot; it stops with the test.
func Server(t testing.TB) *keepertest.Server {
	t.Helper()
	return keepertest.NewFromDir(t, SnapshotDir())
}

// Source is a Keeper source reading the replay server. Prices are read one
// region per request, which is how the snapshot is recorded, so any subset of
// regions replays.
func Source(t testing.TB) *catalog.KeeperSource {
	t.Helper()
	return SourceAt(Server(t).URL)
}

// SourceAt is Source for a given server URL.
func SourceAt(url string) *catalog.KeeperSource {
	return catalog.NewKeeperSource(catalog.KeeperOptions{
		URL: url, UserAgent: "scarpenter-test", PricesRegionsPerRequest: 1,
	})
}

// VendorRow is a row of the vendor table with the billing fields, which are
// nullable in Keeper: a nil pointer is a null.
type VendorRow struct {
	VendorID                string  `json:"vendor_id"`
	Name                    string  `json:"name"`
	Status                  string  `json:"status"`
	BillingIncrementSeconds *int    `json:"billing_increment_seconds"`
	MinimumBillingSeconds   *int    `json:"minimum_billing_seconds"`
	StoppedServerCharged    *bool   `json:"stopped_server_charged"`
	BillingComment          *string `json:"billing_comment"`
}

// ServerWithVendors is Server with the vendor table replaced by rows, for tests
// that need a billing rule the committed snapshot does not have (the committed
// table has a null row for azure). Passing no rows serves an empty table.
func ServerWithVendors(t testing.TB, rows ...VendorRow) *keepertest.Server {
	t.Helper()
	h, err := keepertest.NewHandler(SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil {
		rows = []VendorRow{}
	}
	h.Mount("/table/vendor", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	}))
	return keepertest.NewServer(t, h)
}

type tableRow struct {
	VendorID     string `json:"vendor_id"`
	RegionID     string `json:"region_id"`
	ZoneID       string `json:"zone_id"`
	APIReference string `json:"api_reference"`
	Status       string `json:"status"`
}

func readTable(t testing.TB, name string) []tableRow {
	t.Helper()
	f, err := os.Open(filepath.Join(SnapshotDir(), "tables", name+".json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var rows []tableRow
	if err := json.NewDecoder(zr).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// Facts returns driver facts for the vendors, with the zones of the snapshot's
// zone table as the driver's account would see them: Keeper's zone ID as the
// ID and the vendor's zone name as the name (AWS account names would differ,
// see ADR-009). A vendor not in the list has no zones. Active regions and
// zones only.
func Facts(t testing.TB, vendorsWithZones ...string) catalog.StaticFacts {
	t.Helper()
	regionName := map[[2]string]string{}
	for _, r := range readTable(t, "region") {
		if r.Status == "active" {
			regionName[[2]string{r.VendorID, r.RegionID}] = r.APIReference
		}
	}
	facts := catalog.StaticFacts{ZonesByVendor: map[string]map[string][]catalog.Zone{}}
	for _, v := range vendorsWithZones {
		facts.ZonesByVendor[v] = map[string][]catalog.Zone{}
	}
	for _, z := range readTable(t, "zone") {
		zones, ok := facts.ZonesByVendor[z.VendorID]
		name, active := regionName[[2]string{z.VendorID, z.RegionID}]
		if !ok || !active || z.Status != "active" {
			continue
		}
		zones[name] = append(zones[name], catalog.Zone{ID: z.ZoneID, Name: z.APIReference})
	}
	return facts
}
