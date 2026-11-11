/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
)

// TestSelectorReferenceMatchesTheDeclarations keeps docs/reference/resource-selectors.md
// in step with what the driver declares: every supported attribute and filter of a
// kind appears in the ovh section, and the attributes the section's table lists for
// the kind are declared.
func TestSelectorReferenceMatchesTheDeclarations(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "reference", "resource-selectors.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "\n### ovh\n")
	end := strings.Index(doc, "\n### upcloud\n")
	if start < 0 || end < start {
		t.Fatal("the ovh section of resource-selectors.md was not found")
	}
	section := doc[start:end]
	for kind, caps := range (&Driver{}).Selectors() {
		i := strings.Index(section, "**"+string(kind)+"**")
		if i < 0 {
			t.Errorf("%s: no **%s** heading in the ovh section", kind, kind)
			continue
		}
		part := section[i+2:]
		for _, stop := range []string{"\n**", "\n\nOther kinds"} {
			if j := strings.Index(part, stop); j >= 0 {
				part = part[:j]
			}
		}
		for attr := range caps.Match {
			if !strings.Contains(part, "`match."+attr+"`") {
				t.Errorf("%s: match.%s is declared but not in the table", kind, attr)
			}
		}
		for _, f := range caps.Filters {
			name := f.Name
			if before, ok := strings.CutSuffix(name, "*"); ok {
				name = before + "<k>"
			}
			if !strings.Contains(part, name) {
				t.Errorf("%s: the filter %s is declared but not documented", kind, f.Name)
			}
		}
		for _, m := range regexp.MustCompile("`match\\.([A-Za-z]+)`").FindAllStringSubmatch(part, -1) {
			if _, ok := caps.Match[m[1]]; !ok {
				t.Errorf("%s: the table lists match.%s, which the driver does not declare", kind, m[1])
			}
		}
		if caps.Name != nil && !strings.Contains(part, "| `name` |") {
			t.Errorf("%s: name is declared but not in the table", kind)
		}
	}
}
