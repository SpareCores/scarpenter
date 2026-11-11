/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

func TestSelectorsOfTheSubnetsAreSentToEC2AndFinishedByTheMatcher(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	ec := e.acct.EC2(testRegion)
	vpc := defaultVPC(t, e, testRegion)
	mk := func(name, cidr, zone string, tags map[string]string) string {
		tl := make([]ec2types.Tag, 0, 1+len(tags))
		tl = append(tl, ec2types.Tag{Key: new("Name"), Value: &name})
		for k, v := range tags {
			tl = append(tl, ec2types.Tag{Key: new(k), Value: new(v)})
		}
		out, err := ec.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: &vpc, AvailabilityZone: &zone, CidrBlock: &cidr,
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSubnet, Tags: tl}}})
		if err != nil {
			t.Fatal(err)
		}
		return *out.Subnet.SubnetId
	}
	a := mk("n-9", "10.8.4.0/24", "us-east-1a", map[string]string{"team": "x", "env": "prod"})
	b := mk("n-10", "10.8.5.0/24", "us-east-1b", map[string]string{"team": "x"})
	c := mk("n-2", "10.9.0.0/24", "us-east-1a", map[string]string{"env": "dev"})
	zoneID := func(name string) string {
		zs, err := e.d.Zones(ctx, testRegion)
		if err != nil {
			t.Fatal(err)
		}
		for _, z := range zs {
			if z.Name == name {
				return z.ID
			}
		}
		t.Fatalf("no zone %s", name)
		return ""
	}
	subnets := func(ref vendors.UseRef) ([]string, error) {
		refs, err := e.d.Resolve(ctx, aws.KindSubnets, testRegion, ref)
		ids := make([]string, 0, len(refs))
		for _, r := range refs {
			ids = append(ids, r.ID)
		}
		slices.Sort(ids)
		return ids, err
	}
	sorted := func(ids ...string) []string { slices.Sort(ids); return ids }
	for name, tc := range map[string]struct {
		ref  vendors.UseRef
		want []string
	}{
		"cidr equals":             {vendors.UseRef{Match: common.ResourceMatch{CIDR: "10.8.4.0/24"}, Select: common.SelectAll}, sorted(a)},
		"within contains":         {vendors.UseRef{Match: common.ResourceMatch{Within: "10.8.0.0/16"}, Select: common.SelectAll}, sorted(a, b)},
		"within does not equal":   {vendors.UseRef{Match: common.ResourceMatch{Within: "10.8.4.0/25"}, Select: common.SelectAll}, nil},
		"zone ID":                 {vendors.UseRef{Name: "n-*", Match: common.ResourceMatch{Zone: []string{zoneID("us-east-1a")}}, Select: common.SelectAll}, sorted(a, c)},
		"zone ID and tags":        {vendors.UseRef{Tags: map[string]string{"env": "*"}, Match: common.ResourceMatch{Zone: []string{zoneID("us-east-1a")}}, Select: common.SelectAll}, sorted(a, c)},
		"network":                 {vendors.UseRef{Match: common.ResourceMatch{Network: []string{vpc}}, Name: "n-*", Select: common.SelectAll}, sorted(a, b, c)},
		"wrong network":           {vendors.UseRef{Match: common.ResourceMatch{Network: []string{"vpc-0123456789abcdef0"}}, Select: common.SelectAll}, nil},
		"native filter":           {vendors.UseRef{Filters: []common.ResourceFilter{{Name: "cidr-block", Values: []string{"10.9.0.0/24", "10.8.5.0/24"}}}, Select: common.SelectAll}, sorted(b, c)},
		"native filter and name":  {vendors.UseRef{Name: "n-2", Filters: []common.ResourceFilter{{Name: "cidr-block", Values: []string{"10.8.5.0/24"}}}, Select: common.SelectAll}, nil},
		"ids are ANDed":           {vendors.UseRef{IDs: []string{a, b}, Tags: map[string]string{"env": "prod"}, Select: common.SelectAll}, sorted(a)},
		"newest by name, natural": {vendors.UseRef{Name: "n-*", Select: common.SelectNewest, OrderBy: "name"}, sorted(b)},
		"oldest by name, natural": {vendors.UseRef{Name: "n-*", Select: common.SelectOldest, OrderBy: "name"}, sorted(c)},
		"a tie is ambiguous":      {vendors.UseRef{Tags: map[string]string{"team": "x"}, Select: common.SelectOldest, OrderBy: "label:team"}, nil},
		"state default available": {vendors.UseRef{Name: "n-9", Select: common.SelectAll}, sorted(a)},
		"failed is not available": {vendors.UseRef{Name: "n-9", Match: common.ResourceMatch{State: []string{"failed"}}, Select: common.SelectAll}, nil},
	} {
		got, err := subnets(tc.ref)
		_, none := errors.AsType[*selector.NoMatchError](err)
		_, many := errors.AsType[*selector.AmbiguousError](err)
		switch {
		case tc.want == nil && !none && !many:
			t.Errorf("%s: %v, %v; want nothing or a tie", name, got, err)
		case tc.want != nil && (err != nil || !slices.Equal(got, tc.want)):
			t.Errorf("%s: %v, %v; want %v", name, got, err, tc.want)
		}
	}
	// exactlyOne with two matches names the stages.
	_, err := subnets(vendors.UseRef{Name: "n-*", Select: common.SelectExactlyOne})
	if amb, ok := errors.AsType[*selector.AmbiguousError](err); !ok || amb.Count != 3 || !strings.Contains(err.Error(), "3 match") {
		t.Errorf("exactlyOne over three matches: %v", err)
	}
	// A missing ID does not fail the others, and is named when nothing matches.
	got, err := subnets(vendors.UseRef{IDs: []string{a, "subnet-00000000000000000"}, Select: common.SelectAll})
	if err != nil || !slices.Equal(got, sorted(a)) {
		t.Errorf("a missing ID next to a real one: %v, %v", got, err)
	}
	_, err = subnets(vendors.UseRef{IDs: []string{"subnet-00000000000000000"}})
	if nm, ok := errors.AsType[*selector.NoMatchError](err); !ok || !slices.Equal(nm.Missing, []string{"subnet-00000000000000000"}) {
		t.Errorf("a missing ID alone: %v", err)
	}
	// The request to EC2 carries every field the vendor can evaluate.
	in := e.acct.Inputs("DescribeSubnets")
	last := in[len(in)-1].(*ec2.DescribeSubnetsInput)
	names := map[string]bool{}
	for _, f := range last.Filters {
		names[*f.Name] = true
	}
	if !names["subnet-id"] {
		t.Errorf("the last call (an ID selector) sent the filters %v; want subnet-id", names)
	}
}

func TestEC2FiltersCarryTheWholeSelector(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	vpc := defaultVPC(t, e, testRegion)
	_, _ = e.d.Resolve(ctx, aws.KindSubnets, testRegion, vendors.UseRef{
		IDs: []string{"subnet-0123456789abcdef0"}, Name: "nodes-*",
		Tags: map[string]string{"env": "prod", "team": "*"},
		Match: common.ResourceMatch{Zone: []string{"use1-az1"}, Network: []string{vpc}, CIDR: "10.0.0.0/24",
			State: []string{"available", "pending"}},
		Filters: []common.ResourceFilter{{Name: "map-public-ip-on-launch", Values: []string{"true"}}},
		Select:  common.SelectAll,
	})
	in := e.acct.Inputs("DescribeSubnets")
	last := in[len(in)-1].(*ec2.DescribeSubnetsInput)
	got := map[string][]string{}
	for _, f := range last.Filters {
		got[*f.Name] = f.Values
	}
	want := map[string][]string{
		"subnet-id": {"subnet-0123456789abcdef0"}, "tag:Name": {"nodes-*"}, "tag:env": {"prod"}, "tag-key": {"team"},
		"availability-zone-id": {"use1-az1"}, "vpc-id": {vpc}, "cidr-block": {"10.0.0.0/24"},
		"state": {"available", "pending"}, "map-public-ip-on-launch": {"true"},
	}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			t.Errorf("filter %s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("filters %v, want exactly %v", got, want)
	}
	if len(last.SubnetIds) != 0 {
		t.Errorf("SubnetIds %v: one missing ID would fail the whole call, the ID filter is used", last.SubnetIds)
	}
}

func TestValidateRejectsWhatTheDeclarationDoesNotSupport(t *testing.T) {
	e := newEnv(t, envOptions{})
	_ = e
	caps := (&aws.Driver{}).Selectors()
	for name, tc := range map[string]struct {
		kind vendors.ResourceKind
		use  common.ResourceUse
		want string
	}{
		"unknown filter":         {aws.KindSubnets, common.ResourceUse{Filters: []common.ResourceFilter{{Name: "cidr-blok", Values: []string{"x"}}}}, `filters[0]: "cidr-blok" is not a subnets filter of aws (`},
		"a tag filter":           {aws.KindSubnets, common.ResourceUse{Filters: []common.ResourceFilter{{Name: "tag:env", Values: []string{"x"}}}}, ""},
		"owner on subnets":       {aws.KindSubnets, common.ResourceUse{Match: &common.ResourceMatch{Owner: []string{"self"}}}, "match.owner is not supported for subnets of aws (supported: cidr, network, state, within, zone)"},
		"description on subnets": {aws.KindSubnets, common.ResourceUse{Match: &common.ResourceMatch{CreatedAfter: nil, Description: "x"}}, "match.description is not supported"},
		"orderBy created":        {aws.KindSubnets, common.ResourceUse{Name: "a", Select: common.SelectNewest, OrderBy: "created"}, "has no creation time"},
		"newest without orderBy": {aws.KindSecurityGroups, common.ResourceUse{Name: "a", Select: common.SelectNewest}, "needs orderBy"},
		"expression":             {aws.KindSubnets, common.ResourceUse{Expression: "x"}, "expression is not supported"},
		"zone on groups":         {aws.KindSecurityGroups, common.ResourceUse{Name: "a-{zone}"}, "{zone} is only valid on zonal kinds"},
		"zone on subnets":        {aws.KindSubnets, common.ResourceUse{Name: "a-{zone}"}, ""},
		"unknown placeholder":    {aws.KindSubnets, common.ResourceUse{Name: "a-{zoen}"}, "unknown placeholder {zoen}"},
		"arch outside images":    {aws.KindSubnets, common.ResourceUse{Name: "a-{arch}"}, "{arch} is only valid in images"},
		"literal braces":         {aws.KindSubnets, common.ResourceUse{Name: "a-{{x}}"}, ""},
		"empty use":              {aws.KindSubnets, common.ResourceUse{}, "selects nothing"},
		"placement description":  {aws.KindPlacementGroups, common.ResourceUse{Match: &common.ResourceMatch{Description: "x"}}, "match.description is not supported"},
	} {
		errs := selector.Validate("aws", tc.kind, "p", &tc.use, caps[tc.kind])
		got := errors.Join(errs...)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: %v", name, got)
		case tc.want != "" && (got == nil || !strings.Contains(got.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, got, tc.want)
		}
	}
}

// TestFilterNamesAreInTheSDKDocumentation keeps the declared filter names to what
// the pinned SDK documents for each Describe call.
func TestFilterNamesAreInTheSDKDocumentation(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/aws/aws-sdk-go-v2/service/ec2").Output()
	if err != nil {
		t.Skipf("the SDK source is not available: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	files := map[vendors.ResourceKind]string{
		aws.KindSubnets: "api_op_DescribeSubnets.go", aws.KindSecurityGroups: "api_op_DescribeSecurityGroups.go",
		aws.KindPlacementGroups: "api_op_DescribePlacementGroups.go",
	}
	for kind, caps := range (&aws.Driver{}).Selectors() {
		raw, err := os.ReadFile(filepath.Join(dir, files[kind]))
		if err != nil {
			t.Fatal(err)
		}
		doc := regexp.MustCompile(`(?m)^\s*//\s*`).ReplaceAllString(string(raw), "")
		doc = strings.Join(strings.Fields(doc), " ")
		for _, f := range caps.Filters {
			name := strings.TrimSuffix(f.Name, "*")
			name = strings.TrimSuffix(name, ":")
			if !regexp.MustCompile(`- ` + regexp.QuoteMeta(name) + ` ?:? -`).MatchString(doc) {
				t.Errorf("%s: the filter %q is not in the SDK's documentation of the call", kind, f.Name)
			}
		}
	}
}

// TestSelectorReferenceMatchesTheDeclarations keeps docs/reference/resource-selectors.md
// in step with what the driver declares: every supported attribute and filter
// name of a kind appears in the aws section, and the attributes the section's
// tables list for the kind are declared.
func TestSelectorReferenceMatchesTheDeclarations(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "reference", "resource-selectors.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "\n### aws\n")
	end := strings.Index(doc, "\n### gcp\n")
	if start < 0 || end < start {
		t.Fatal("the aws section of resource-selectors.md was not found")
	}
	section := doc[start:end]
	for kind, caps := range (&aws.Driver{}).Selectors() {
		// The part of the section about the kind: from its heading to the next.
		i := strings.Index(section, "**"+string(kind)+"**")
		if i < 0 {
			t.Errorf("%s: no **%s** heading in the aws section", kind, kind)
			continue
		}
		part := section[i+2:]
		for _, stop := range []string{"\n**", "\n\nOther kinds"} {
			if j := strings.Index(part, stop); j >= 0 {
				part = part[:j]
			}
		}
		// The tables write a family of filters once: ip-permission.{cidr,protocol}.
		for _, m := range regexp.MustCompile(`([\w.-]+)\.\{([^}]+)\}`).FindAllStringSubmatch(part, -1) {
			for suffix := range strings.SplitSeq(m[2], ",") {
				part += "\n" + m[1] + "." + strings.TrimSpace(suffix)
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
			// egress.ip-permission.x is written as the prefix egress. on ip-permission.x.
			egress := strings.HasPrefix(name, "egress.") && strings.Contains(part, "prefix `egress.`") &&
				strings.Contains(part, strings.TrimPrefix(name, "egress."))
			if !strings.Contains(part, name) && !egress {
				t.Errorf("%s: the filter %s is declared but not documented", kind, f.Name)
			}
		}
		// The other way: a match attribute in the table is declared.
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
