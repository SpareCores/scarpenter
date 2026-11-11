/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/resources"
)

// The Alibaba Cloud examples of docs/examples name only what the driver's
// declaration supports for each kind, and validate.
func TestThePublishedExamplesUseWhatTheDriverSupports(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "docs", "examples", "alicloud-*.yaml"))
	if err != nil || len(files) < 2 {
		t.Fatalf("expected the examples, got %v (err %v)", files, err)
	}
	d := New()
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var nc v1alpha1.SCNodeClass
			if err := yaml.UnmarshalStrict(raw, &nc); err != nil {
				t.Fatal(err)
			}
			if err := resources.ValidateUses(&nc, Vendor, d); err != nil {
				t.Error(err)
			}
			if err := resources.CheckUsesSelect(&nc); err != nil {
				t.Error(err)
			}
			if err := d.ValidateNodeClass(bg(), &nc); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestSelectorReferenceMatchesTheDeclarations keeps docs/reference/resource-selectors.md in
// step with what the driver declares: every supported attribute and filter name of a
// kind appears in the alicloud section, and the attributes the section's tables list for
// the kind are declared.
func TestSelectorReferenceMatchesTheDeclarations(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "reference", "resource-selectors.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "\n### alicloud\n")
	if start < 0 {
		t.Fatal("the alicloud section of resource-selectors.md was not found")
	}
	section := doc[start:]
	headings := map[string]string{
		"images": "**images**", "subnets": "**subnets**", "securityGroups": "**securityGroups**",
		"deploymentSets": "**deploymentSets**",
	}
	for kind, caps := range (&Driver{}).Selectors() {
		i := strings.Index(section, headings[string(kind)])
		if i < 0 {
			t.Errorf("%s: no %s heading in the alicloud section", kind, headings[string(kind)])
			continue
		}
		part := section[i+2:]
		// The part about a kind runs to the next kind's heading.
		for _, h := range headings {
			if j := strings.Index(part, h); j > 0 && h != headings[string(kind)] {
				part = part[:j]
			}
		}
		for attr := range caps.Match {
			if !strings.Contains(part, "`match."+attr+"`") && !strings.Contains(part, "`match.createdAfter`, `createdBefore`") &&
				!strings.Contains(part, "`match.cidr`, `within`") {
				t.Errorf("%s: match.%s is declared but not in the section", kind, attr)
			}
		}
		for _, f := range caps.Filters {
			if !strings.Contains(part, f.Name) {
				t.Errorf("%s: the filter %s is declared but not documented", kind, f.Name)
			}
		}
		for _, m := range regexp.MustCompile("`match\\.([A-Za-z]+)`").FindAllStringSubmatch(part, -1) {
			if _, ok := caps.Match[m[1]]; !ok {
				t.Errorf("%s: the section lists match.%s, which the driver does not declare", kind, m[1])
			}
		}
	}
}
