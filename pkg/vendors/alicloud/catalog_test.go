/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestFamilyOf(t *testing.T) {
	cases := map[string]string{
		"ecs.g8y.large":              "ecs.g8y",
		"ecs.e-c1m1.large":           "ecs.e-c1m1",
		"ecs.u0.c1m1.large":          "ecs.u0",
		"ecs.ebmgn9ge-1.5t.64xlarge": "ecs.ebmgn9ge-1",
		"ecs.c7a-nps1.2xlarge":       "ecs.c7a-nps1",
	}
	for name, want := range cases {
		if got, ok := FamilyOf(name); !ok || got != want {
			t.Errorf("FamilyOf(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, bad := range []string{"", "g8y.large", "ecs.g8y", "ecs..large", "aws.m7i.large", "ecs.g8y."} {
		if _, ok := FamilyOf(bad); ok {
			t.Errorf("FamilyOf(%q) must fail", bad)
		}
		if Launchable(bad) {
			t.Errorf("%q must not be launchable", bad)
		}
	}
}

func TestDropReason(t *testing.T) {
	drop := []string{
		"ecs.ebmc7.32xlarge", "ecs.ebmgn9ge-1.5t.64xlarge", "ecs.scch5.16xlarge", "ecs.sccg7.24xlarge",
		"ecs.poc-test.large", "ecs.e-test-c1m1.large", "ecs.vfx-c1m1.large", "ecs.hbm-bf62p2l2d9-d.large",
		"ecs.c1.large", "ecs.s1.large",
	}
	for _, n := range drop {
		if Launchable(n) || DropReason(n) == "" {
			t.Errorf("%s must be dropped", n)
		}
	}
	// Exact matches must not take the neighbours: c10a, s1x-like names, sc3 (not scc).
	keep := []string{
		"ecs.c10a.large", "ecs.c6a.large", "ecs.g8y.large", "ecs.e-c1m1.large", "ecs.sc3.large", "ecs.u0.c1m1.large",
		"ecs.i4.xlarge", "ecs.gn7i-c16g1.4xlarge", "ecs.t6-c1m1.large", "ecs.s6-c1m1.large", "ecs.s2.large",
	}
	for _, n := range keep {
		if !Launchable(n) {
			t.Errorf("%s must be launchable, dropped for %q", n, DropReason(n))
		}
	}
}

func TestHasLocalDisks(t *testing.T) {
	yes := []string{"ecs.i1.xlarge", "ecs.i3g.2xlarge", "ecs.i4.large", "ecs.d1.2xlarge", "ecs.d2s.5xlarge", "ecs.d3c.16xlarge", "ecs.i5e-grds-2t.large"}
	no := []string{"ecs.g8y.large", "ecs.ic5.large", "ecs.c10a.large", "ecs.e-c1m1.large", "ecs.hfc6.large", "ecs.i.large"}
	for _, n := range yes {
		if !HasLocalDisks(n) {
			t.Errorf("%s has local disks", n)
		}
	}
	for _, n := range no {
		if HasLocalDisks(n) {
			t.Errorf("%s has no local disks", n)
		}
	}
	if HasLocalDisks("nonsense") {
		t.Error("a bad name has none")
	}
}

type serverRow struct {
	VendorID     string `json:"vendor_id"`
	APIReference string `json:"api_reference"`
}

func alicloudTypes(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(testutil.RepoRoot(), "test", "data", "sc-snapshot", "tables", "server.json.gz"))
	if err != nil {
		t.Skipf("the Keeper snapshot is not available: %v", err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var rows []serverRow
	if err := json.NewDecoder(zr).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if r.VendorID == "alicloud" {
			out = append(out, r.APIReference)
		}
	}
	return out
}

// TestEverySnapshotFamilyIsClassified pins the verdict of every family of the
// committed Keeper snapshot in testdata/families.golden: launchable, or dropped
// with the reason. A family the snapshot gains that is not in the file fails the
// test until someone classifies it (rerun with -update and review the diff), as
// plan 21 section 3 asks.
func TestEverySnapshotFamilyIsClassified(t *testing.T) {
	types := alicloudTypes(t)
	if len(types) < 1000 {
		t.Fatalf("the snapshot has %d alicloud types, expected about 2000", len(types))
	}
	verdicts := map[string]string{}
	for _, n := range types {
		fam, ok := FamilyOf(n)
		if !ok {
			t.Errorf("%q is not an ecs.<family>.<size> name", n)
			continue
		}
		v := "launchable"
		if r := DropReason(n); r != "" {
			v = "drop: " + r
		}
		if old, seen := verdicts[fam]; seen && old != v {
			t.Errorf("family %s has types with two verdicts: %q and %q", fam, old, v)
		}
		verdicts[fam] = v
	}
	fams := make([]string, 0, len(verdicts))
	for f := range verdicts {
		fams = append(fams, f)
	}
	slices.Sort(fams)
	var b strings.Builder
	for _, f := range fams {
		fmt.Fprintf(&b, "%s\t%s\n", f, verdicts[f])
	}
	path := filepath.Join("testdata", "families.golden")
	if *update {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	golden := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fam, v, _ := strings.Cut(sc.Text(), "\t")
		golden[fam] = v
	}
	var unclassified, changed, gone []string
	for _, fam := range fams {
		if g, ok := golden[fam]; !ok {
			unclassified = append(unclassified, fam)
		} else if g != verdicts[fam] {
			changed = append(changed, fam)
		}
	}
	for fam := range golden {
		if _, ok := verdicts[fam]; !ok {
			gone = append(gone, fam)
		}
	}
	slices.Sort(gone)
	if len(unclassified)+len(changed)+len(gone) > 0 {
		t.Fatalf("families not in testdata/families.golden: %v; verdict changed: %v; no longer in the snapshot: %v "+
			"(classify them in catalog.go, then go test -run Classified -update)", unclassified, changed, gone)
	}
}
