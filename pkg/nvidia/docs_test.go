/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nvidia_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

// TestDocsTablesAreGenerated keeps the supported-model tables of the
// reference between the markers <!-- nvidia-table:<release> --> and
// <!-- /nvidia-table --> equal to nvidia.SupportTable (UPDATE_GOLDEN=1
// rewrites them).
func TestDocsTablesAreGenerated(t *testing.T) {
	path := filepath.Join(testutil.RepoRoot(), "docs", "reference", "labels-and-offerings.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, rel := range nvidia.Releases() {
		open := "<!-- nvidia-table:" + rel.Version + " -->\n"
		start := strings.Index(doc, open)
		if start < 0 {
			t.Fatalf("%s has no marker for the table of release %s", path, rel.Version)
		}
		start += len(open)
		end := strings.Index(doc[start:], "<!-- /nvidia-table -->")
		if end < 0 {
			t.Fatalf("%s: the table of release %s is not closed", path, rel.Version)
		}
		end += start
		want := nvidia.SupportTable(rel.Version)
		if doc[start:end] == want {
			continue
		}
		if os.Getenv("UPDATE_GOLDEN") != "1" {
			t.Fatalf("the table of release %s is stale; run with UPDATE_GOLDEN=1. Want:\n%s", rel.Version, want)
		}
		doc = doc[:start] + want + doc[end:]
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil { //nolint:gosec // a docs file
			t.Fatal(err)
		}
	}
}
