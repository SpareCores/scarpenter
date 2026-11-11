/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/resources/resourcestest"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func subnetsUse(u *common.ResourceUse) *aws.Resources {
	return &aws.Resources{Subnets: &common.Resource{Use: u}}
}

func TestGlobalIDsApplyToEveryRegion(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-g", Name: "global"},
		{Kind: resourcestest.Subnets, Region: region2, ID: "subnet-g", Name: "global"},
		{Kind: resourcestest.Subnets, Region: region2, ID: "subnet-b", Name: "local"},
	}
	nc := class("c", subnetsUse(&common.ResourceUse{IDs: map[string]common.IDList{"*": {"subnet-g"}, region2: {"subnet-b"}}}))
	for region, want := range map[string]int{region1: 1, region2: 2} {
		refs, err := e.require(nc, region, subnets())
		if err != nil || len(refs) != want {
			t.Errorf("%s: %+v, %v; want %d", region, refs, err, want)
		}
	}
}

func TestUseFieldsAreAndedAndTheNameIsReported(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-a", Name: "sc-a", Tags: map[string]string{"env": "prod"}},
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-b", Name: "sc-b", Tags: map[string]string{"env": "dev"}},
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-c", Name: "other", Tags: map[string]string{"env": "prod"}},
	}
	nc := class("c", subnetsUse(&common.ResourceUse{Name: "sc-*", Tags: map[string]string{"env": "prod"}}))
	refs, err := e.require(nc, region1, subnets())
	if err != nil || len(refs) != 1 || refs[0].ID != "subnet-a" {
		t.Fatalf("name and tags: %+v, %v", refs, err)
	}
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	if len(rep.Resources) != 1 || rep.Resources[0].Name != "sc-a" || rep.Resources[0].ID != "subnet-a" {
		t.Errorf("the status entry carries the name: %+v", rep.Resources)
	}
}

func TestASelectorThatSelectsNothingOrTooMuchMakesTheRegionUnusableWithTheCounts(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-a", Name: "sc-a", Tags: map[string]string{"env": "prod"}},
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-b", Name: "sc-b", Tags: map[string]string{"env": "prod"}},
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-c", Name: "other"},
	}
	for name, tc := range map[string]struct {
		use  common.ResourceUse
		want []string
	}{
		"zero matches":       {common.ResourceUse{Tags: map[string]string{"env": "dev"}}, []string{"3 listed", "0 match tags"}},
		"two for exactlyOne": {common.ResourceUse{Tags: map[string]string{"env": "prod"}, Select: common.SelectExactlyOne}, []string{"2 match", "3 listed", "2 match tags", "exactlyOne"}},
		"a tie":              {common.ResourceUse{Name: "sc-*", Select: common.SelectNewest, OrderBy: "label:env"}, []string{"2 tie for newest"}},
		"a missing ID":       {common.ResourceUse{IDs: map[string]common.IDList{region1: {"subnet-zz"}}}, []string{"subnet-zz"}},
	} {
		t.Run(name, func(t *testing.T) {
			nc := class(strings.ReplaceAll(name, " ", "-"), subnetsUse(&tc.use))
			rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
			st := rep.Regions[region1]
			if st.Ready {
				t.Fatalf("the region is ready: %+v", st)
			}
			for _, w := range tc.want {
				if !strings.Contains(st.Reason, w) {
					t.Errorf("reason %q does not contain %q", st.Reason, w)
				}
			}
			_, err := e.require(nc, region1, subnets())
			if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != vendors.CodeResourceMissing {
				t.Errorf("a launch: %v, want Unsupported/ResourceMissing", err)
			}
			if len(rep.Errors) != 0 {
				t.Errorf("a selection is a verdict, not a failure: %v", rep.Errors)
			}
		})
	}
	if calls := e.drv.CallLog(); len(calls) != 0 {
		t.Errorf("nothing is created when a use selects nothing: %v", calls)
	}
}

func TestAPresentButEmptyUseIsInvalidNeverTheDefault(t *testing.T) {
	// A stored object whose old selector was pruned has a use without a field. It
	// must not fall back to the vendor's default or to creating the resource.
	e := newEnv(t, envOpts{})
	e.drv.Existing = []resourcestest.Existing{{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-default", Default: true}}
	nc := class("c", subnetsUse(&common.ResourceUse{}))
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	if st := rep.Regions[region1]; st.Ready || !strings.Contains(st.Reason, "selects nothing") {
		t.Errorf("an empty use: %+v", st)
	}
	if len(rep.Resources) != 0 {
		t.Errorf("the default was used: %+v", rep.Resources)
	}
	if refs, err := e.require(nc, region1, subnets()); err == nil {
		t.Errorf("a launch with an empty use got %+v", refs)
	}
	if calls := e.drv.CallLog(); len(calls) != 0 {
		t.Errorf("the resource was created: %v", calls)
	}
}

func TestTheStatusListsAtMost128ResourcesAndCountsThePast(t *testing.T) {
	e := newEnv(t, envOpts{})
	for i := range 150 {
		e.drv.Existing = append(e.drv.Existing, resourcestest.Existing{
			Kind: resourcestest.Subnets, Region: region1, ID: fmt.Sprintf("subnet-%03d", i), Name: fmt.Sprintf("n%03d", i),
			Tags: map[string]string{"use": "true"}})
	}
	for i := range 3 {
		e.drv.Existing = append(e.drv.Existing, resourcestest.Existing{
			Kind: resourcestest.Subnets, Region: region2, ID: fmt.Sprintf("subnet-b%d", i), Tags: map[string]string{"use": "true"}})
	}
	nc := class("c", subnetsUse(&common.ResourceUse{Tags: map[string]string{"use": "true"}, Select: common.SelectAll}))
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1, region2})
	if len(rep.Resources) != resources.MaxStatusEntries {
		t.Errorf("%d entries, want %d", len(rep.Resources), resources.MaxStatusEntries)
	}
	want := []v1alpha1.ResourceCount{{Kind: "subnets", Region: region2, Count: 3}, {Kind: "subnets", Region: region1, Count: 150}}
	if len(rep.Counts) != 2 || rep.Counts[0] != want[0] || rep.Counts[1] != want[1] {
		t.Errorf("counts %+v, want %+v (per region and kind, of all resources)", rep.Counts, want)
	}
	// Under the cap there are no counts.
	e.clock.Step(time.Minute)
	small := class("small", subnetsUse(&common.ResourceUse{IDs: map[string]common.IDList{region1: {"subnet-001"}}}))
	if rep := e.engine.Check(t.Context(), small, "aws", []string{region1}); len(rep.Counts) != 0 || len(rep.Resources) != 1 {
		t.Errorf("under the cap: %+v, counts %+v", rep.Resources, rep.Counts)
	}
}

func TestZonePlaceholderInATagSelectsPerZone(t *testing.T) {
	e := newEnv(t, envOpts{zones: map[string][]string{region1: {"us-east-1a", "us-east-1b"}}})
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-a", Tags: map[string]string{"az": "us-east-1a"}},
	}
	nc := class("c", subnetsUse(&common.ResourceUse{Tags: map[string]string{"az": "{zone}"}}))
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	st := rep.Regions[region1]
	if !st.Ready || !strings.Contains(st.Zones["us-east-1b"], "subnets in us-east-1b") || st.Zones["us-east-1a"] != "" {
		t.Errorf("{zone} in a tag value must be expanded per zone, with only the zone without a match unavailable: %+v", st)
	}
}

func TestCheckUsesSelectRejectsAnEmptyUseOfAnyVendor(t *testing.T) {
	// The API server rejects an empty use on write, but an object stored before
	// a field was renamed is pruned on read and CEL does not run again: the
	// controller must not read it as "nothing given".
	nc := class("c", &aws.Resources{
		Subnets:         &common.Resource{Use: &common.ResourceUse{}},
		SecurityGroups:  &common.Resource{Use: &common.ResourceUse{Match: &common.ResourceMatch{}}},
		PlacementGroups: &common.Resource{Use: &common.ResourceUse{Name: "ok"}},
	})
	err := resources.CheckUsesSelect(nc)
	if err == nil || !strings.Contains(err.Error(), "vendors.aws.resources.subnets.use: selects nothing") ||
		!strings.Contains(err.Error(), "vendors.aws.resources.securityGroups.use: selects nothing") ||
		strings.Contains(err.Error(), "placementGroups") {
		t.Errorf("%v", err)
	}
	ok := class("c", &aws.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{Tags: map[string]string{"a": "b"}}}})
	if err := resources.CheckUsesSelect(ok); err != nil {
		t.Errorf("a use with a field: %v", err)
	}
	if err := resources.CheckUsesSelect(class("c", nil)); err != nil {
		t.Errorf("no resources: %v", err)
	}
}

func TestValidateUsesChecksTheDriversDeclaration(t *testing.T) {
	e := newEnv(t, envOpts{})
	nc := class("c", &aws.Resources{
		Subnets:         &common.Resource{Use: &common.ResourceUse{Filters: []common.ResourceFilter{{Name: "nope", Values: []string{"x"}}}}},
		SecurityGroups:  &common.Resource{Use: &common.ResourceUse{Name: "a-{zone}"}},
		PlacementGroups: &common.Resource{Use: &common.ResourceUse{Name: "ok", Tags: map[string]string{"a": "b"}}},
	})
	err := resources.ValidateUses(nc, "aws", e.drv)
	if err == nil || !strings.Contains(err.Error(), "filters[0]") || !strings.Contains(err.Error(), "securityGroups.use") ||
		!strings.Contains(err.Error(), "{zone} is only valid on zonal kinds") || strings.Contains(err.Error(), "placementGroups") {
		t.Errorf("%v", err)
	}
}

func TestAKindThatIsGlobalAtTheVendorIsListedOncePerCheck(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.drv.GlobalSubnets = true
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "fw-1", Name: "global-fw", Tags: map[string]string{"use": "true"}},
	}
	nc := class("c", subnetsUse(&common.ResourceUse{Tags: map[string]string{"use": "true"}}))
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1, region2})
	if bad, why := rep.NotReady(); len(bad) != 0 {
		t.Fatalf("not ready: %v %v", bad, why)
	}
	if n := e.drv.ResolveCalls.Load(); n != 1 {
		t.Errorf("%d listings for two regions, want 1 (the kind is global)", n)
	}
	// A kind that is regional is listed per region.
	e2 := newEnv(t, envOpts{})
	e2.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "a", Tags: map[string]string{"use": "true"}},
		{Kind: resourcestest.Subnets, Region: region2, ID: "b", Tags: map[string]string{"use": "true"}},
	}
	e2.engine.Check(t.Context(), nc, "aws", []string{region1, region2})
	if n := e2.drv.ResolveCalls.Load(); n != 2 {
		t.Errorf("%d listings for two regions, want 2", n)
	}
}
