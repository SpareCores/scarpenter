/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package catalogtest has the helpers tests of the catalog and of its users
// share: a Keeper source that replays the committed recording of Keeper's
// snapshot (test/data/sc-snapshot) and driver facts derived from its zone
// tables. Keeper calls never leave the process.
package catalogtest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SpareCores/sc-keeper-go/columnar"
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

// Source is a Keeper source reading the replay server.
func Source(t testing.TB) *catalog.KeeperSource {
	t.Helper()
	return SourceAt(Server(t).URL)
}

// SourceAt is Source for a given server URL.
func SourceAt(url string) *catalog.KeeperSource {
	return catalog.NewKeeperSource(catalog.KeeperOptions{URL: url, UserAgent: "scarpenter-test"})
}

// Rows decodes one file of the committed snapshot (path as in Keeper's
// manifest: "vendor", "server/aws"; the recorder stores a colon as an
// underscore) into rows of T, matched to the columns by
// their json tags.
func Rows[T any](t testing.TB, path string) []T {
	t.Helper()
	return decodeRows[T](t, filepath.Join(SnapshotDir(), "snapshot"), path)
}

func decodeRows[T any](t testing.TB, dir, path string) []T {
	t.Helper()
	body := readFile(t, dir, path)
	rows, err := columnar.DecodeRows[T](bytes.NewReader(body), -1)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// readFile is the uncompressed JSON of one gzip file below dir.
func readFile(t testing.TB, dir, path string) []byte {
	t.Helper()
	file := filepath.FromSlash(strings.ReplaceAll(path, ":", "_")) + ".json.gz"
	f, err := os.Open(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// OCIDir is the committed Keeper snapshot of the vendor oci (plan 25): the
// eight files of the vendor's tables as Keeper's snapshot routes serve them
// (gzip of columnar JSON, "<path>.json.gz"), read from a copy of the crawler's
// database of 2026-11-17, when Keeper started to serve the vendor. The
// recording of SnapshotDir does not hold it; this directory goes away when that
// recording is refreshed with oci in its query list.
func OCIDir() string {
	return filepath.Join(testutil.RepoRoot(), "test", "data", "sc-snapshot-oci")
}

// OCIPaths are the files of OCIDir, as paths of Keeper's snapshot.
var OCIPaths = []string{
	"vendor", "region/oci", "zone/oci", "server/oci", "server_price/oci",
	"storage/oci", "storage_price/oci", "ipv4_price/oci",
}

// OCIRows decodes one file of OCIDir (path as in OCIPaths) into rows of T,
// matched to the columns by their json tags.
func OCIRows[T any](t testing.TB, path string) []T {
	t.Helper()
	return decodeRows[T](t, OCIDir(), path)
}

// ServerWithOCI serves the committed snapshot with the files of OCIDir added,
// in a second data version that /healthcheck pins. The vendor table is the one
// of OCIDir: it has the row of oci only.
func ServerWithOCI(t testing.TB) *keepertest.Server {
	t.Helper()
	h, err := keepertest.NewHandler(SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	overrides := map[string][]byte{}
	for _, path := range OCIPaths {
		overrides[path] = readFile(t, OCIDir(), path)
	}
	if err := h.AddHash(NewHash, overrides); err != nil {
		t.Fatal(err)
	}
	if err := h.SetCurrent(NewHash); err != nil {
		t.Fatal(err)
	}
	return keepertest.NewServer(t, h)
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

// NewHash is the hash of the second data version the helpers add.
const NewHash = "1111111111111111111111111111111111111111"

// Columns encodes rows of structs with json tags as the columnar JSON of a
// snapshot file (an override of keepertest's AddHash).
func Columns[T any](t testing.TB, rows []T) []byte {
	t.Helper()
	cols := map[string][]json.RawMessage{}
	var order []string
	for i, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			for k := range obj {
				order = append(order, k)
			}
		}
		for _, k := range order {
			cols[k] = append(cols[k], obj[k])
		}
	}
	if len(rows) == 0 {
		var zero T
		raw, _ := json.Marshal(zero)
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		for k := range obj {
			cols[k] = []json.RawMessage{}
		}
	}
	out, err := json.Marshal(cols)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ServerWithVendors serves the snapshot with the vendor table replaced by
// rows, for tests that need a billing rule the committed snapshot does not
// have. The rows are in a second data version that /healthcheck pins. Passing
// no rows serves an empty table.
func ServerWithVendors(t testing.TB, rows ...VendorRow) *keepertest.Server {
	t.Helper()
	h, err := keepertest.NewHandler(SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil {
		rows = []VendorRow{}
	}
	if err := h.AddHash(NewHash, map[string][]byte{"vendor": Columns(t, rows)}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetCurrent(NewHash); err != nil {
		t.Fatal(err)
	}
	return keepertest.NewServer(t, h)
}

type tableRow struct {
	VendorID     string `json:"vendor_id"`
	RegionID     string `json:"region_id"`
	ZoneID       string `json:"zone_id"`
	APIReference string `json:"api_reference"`
	Status       string `json:"status"`
}

// Facts returns driver facts for the vendors, with the zones of the snapshot's
// zone tables as the driver's account would see them: Keeper's zone ID as the
// ID and the vendor's zone name as the name (AWS account names would differ,
// see ADR-009). A vendor not in the list has no zones. Active regions and
// zones only.
func Facts(t testing.TB, vendorsWithZones ...string) catalog.StaticFacts {
	t.Helper()
	facts := catalog.StaticFacts{ZonesByVendor: map[string]map[string][]catalog.Zone{}}
	for _, v := range vendorsWithZones {
		facts.ZonesByVendor[v] = map[string][]catalog.Zone{}
		regionName := map[string]string{}
		for _, r := range Rows[tableRow](t, "region/"+v) {
			if r.Status == "active" {
				regionName[r.RegionID] = r.APIReference
			}
		}
		for _, z := range Rows[tableRow](t, "zone/"+v) {
			if name, ok := regionName[z.RegionID]; ok && z.Status == "active" {
				zones := facts.ZonesByVendor[v]
				zones[name] = append(zones[name], catalog.Zone{ID: z.ZoneID, Name: z.APIReference})
			}
		}
	}
	return facts
}
