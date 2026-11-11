/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources_test

import (
	"errors"
	"slices"
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

// optIn is the opt-in tag of the discovery step, as the double spells it.
var optIn = map[string]string{vendors.DefaultTagKey(vendors.TagUse): vendors.OptInValue}

func ex(kind vendors.ResourceKind, region, id string, mut func(*resourcestest.Existing)) resourcestest.Existing {
	e := resourcestest.Existing{Kind: kind, Region: region, ID: id}
	if mut != nil {
		mut(&e)
	}
	return e
}

func tagged(e *resourcestest.Existing)    { e.Tags = optIn }
func isDefault(e *resourcestest.Existing) { e.Default = true }

// source returns the step the check reported for the resource.
func source(t *testing.T, rep resources.Report, kind vendors.ResourceKind, region, id string) v1alpha1.ResourceSource {
	t.Helper()
	for _, r := range rep.Resources {
		if r.Kind == string(kind) && r.Region == region && r.ID == id {
			return r.Source
		}
	}
	t.Fatalf("no %s %s in %s in the report: %+v", kind, id, region, rep.Resources)
	return ""
}

func TestExplicitUseBeatsTaggedBeatsDefaultBeatsCreate(t *testing.T) {
	use := &common.ResourceUse{Name: "mine"}
	for _, tc := range []struct {
		name     string
		existing []resourcestest.Existing
		cfg      *common.Resource
		want     string
		managed  bool
		source   v1alpha1.ResourceSource
	}{
		{"use over everything", []resourcestest.Existing{
			{Kind: resourcestest.Subnets, Region: region1, ID: "named", Name: "mine"},
			ex(resourcestest.Subnets, region1, "tagged", tagged), ex(resourcestest.Subnets, region1, "dflt", isDefault)},
			&common.Resource{Use: use}, "named", false, v1alpha1.ResourceSourceUse},
		{"tagged over the default", []resourcestest.Existing{
			ex(resourcestest.Subnets, region1, "tagged", tagged), ex(resourcestest.Subnets, region1, "dflt", isDefault)},
			nil, "tagged", false, v1alpha1.ResourceSourceDiscovered},
		{"the default over creating", []resourcestest.Existing{ex(resourcestest.Subnets, region1, "dflt", isDefault)},
			nil, "dflt", false, v1alpha1.ResourceSourceDefault},
		{"an untagged resource is not discovered", []resourcestest.Existing{
			{Kind: resourcestest.Subnets, Region: region1, ID: "other", Tags: map[string]string{"x": "y"}}},
			&common.Resource{}, "", true, v1alpha1.ResourceSourceCreated},
		{"nothing at all creates", nil, nil, "", true, v1alpha1.ResourceSourceCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var res *aws.Resources
			if tc.cfg != nil {
				res = &aws.Resources{Subnets: tc.cfg}
			}
			nc := class("c", res)
			e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
			e.drv.Existing = tc.existing
			refs, err := e.require(nc, region1, subnets())
			if err != nil || len(refs) != 1 || refs[0].Managed != tc.managed || (tc.want != "" && refs[0].ID != tc.want) {
				t.Fatalf("require = %+v, %v; want %q managed=%v", refs, err, tc.want, tc.managed)
			}
			// The status check reports the same resource with the step that produced it. A
			// created one appears once the launch created it (managed resources are listed).
			e.clock.Step(2 * time.Minute)
			rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
			if bad, why := rep.NotReady(); len(bad) != 0 {
				t.Fatalf("not ready: %v", why)
			}
			if got := source(t, rep, resourcestest.Subnets, region1, refs[0].ID); got != tc.source {
				t.Errorf("source %q, want %q", got, tc.source)
			}
		})
	}
}

func TestAnExplicitUseThatMatchesNothingFailsAndDoesNotFallThrough(t *testing.T) {
	nc := class("c", subnetsUse(&common.ResourceUse{Name: "absent"}))
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.Existing = []resourcestest.Existing{
		ex(resourcestest.Subnets, region1, "tagged", tagged), ex(resourcestest.Subnets, region1, "dflt", isDefault)}
	_, err := e.require(nc, region1, subnets())
	if vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Fatalf("%v, want ResourceMissing", err)
	}
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	if bad, why := rep.NotReady(); len(bad) != 1 || !strings.Contains(why[region1], "subnets") {
		t.Errorf("not ready %v %v; want the region failed on subnets", bad, why)
	}
	if len(e.drv.CallLog()) != 0 || len(rep.Resources) != 0 {
		t.Errorf("a failed explicit use created or reported something: %v %+v", e.drv.CallLog(), rep.Resources)
	}
}

func TestACreateSkipsDiscoveryAndTheDefault(t *testing.T) {
	nc := class("c", &aws.Resources{Subnets: &common.Resource{Create: &common.ResourceCreate{}}})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.Existing = []resourcestest.Existing{
		ex(resourcestest.Subnets, region1, "tagged", tagged), ex(resourcestest.Subnets, region1, "dflt", isDefault)}
	refs, err := e.require(nc, region1, subnets())
	if err != nil || len(refs) != 1 || !refs[0].Managed {
		t.Fatalf("require = %+v, %v; want a created resource", refs, err)
	}
	if n := e.drv.ResolveCallsOf(resourcestest.Subnets) + int(e.drv.DefaultCalls.Load()); n != 0 {
		t.Errorf("%d discovery or default calls for an explicit create", n)
	}
}

func TestADefaultThatCannotHonourTheRequestIsSkipped(t *testing.T) {
	nc := class("c", nil)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.Existing = []resourcestest.Existing{{Kind: resourcestest.Subnets, Region: region1, ID: "dflt", Default: true, NoIPv6: true}}
	v6 := &vendors.VendorSettings{Networking: common.Networking{PublicIPv6: new(true)}}
	req := subnets()
	req.Settings = v6
	refs, err := e.require(nc, region1, req)
	if err != nil || len(refs) != 1 || !refs[0].Managed {
		t.Fatalf("with IPv6: %+v, %v; want the default skipped and a created resource", refs, err)
	}
	// Without the setting the default serves.
	refs, err = e.require(nc, region1, subnets())
	if err != nil || refs[0].ID != "dflt" {
		t.Errorf("without IPv6: %+v, %v; want the default", refs, err)
	}

	// With creation forbidden the skipped default ends in the failure, which says why.
	forbid := class("f", &aws.Resources{Subnets: &common.Resource{Forbid: true}})
	_, err = e.require(forbid, region1, req)
	if vendors.CodeOf(err) != vendors.CodeResourceForbidden || !strings.Contains(err.Error(), "cannot honour the request") ||
		!strings.Contains(err.Error(), "dflt has no IPv6 range") {
		t.Errorf("forbid and an unsuitable default: %v", err)
	}
}

func TestTheStatusCheckSkipsADefaultThatCannotHonourTheNodeClass(t *testing.T) {
	// The check asks the default for the NodeClass's own settings, when the
	// driver has node options; the double has none, so it asks nothing and the
	// default is reported. (The AWS driver's check is covered by its tests.)
	nc := class("c", nil)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.Existing = []resourcestest.Existing{{Kind: resourcestest.Subnets, Region: region1, ID: "dflt", Default: true}}
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	if got := source(t, rep, resourcestest.Subnets, region1, "dflt"); got != v1alpha1.ResourceSourceDefault {
		t.Errorf("source %q", got)
	}
}

func TestAnAmbiguousOptInFailsInsteadOfFallingThrough(t *testing.T) {
	nc := class("c", nil)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.Existing = []resourcestest.Existing{
		ex(resourcestest.SecurityGroups, region1, "sg-1", tagged), ex(resourcestest.SecurityGroups, region1, "sg-2", tagged),
		ex(resourcestest.SecurityGroups, region1, "sg-default", isDefault)}
	_, err := e.require(nc, region1, vendors.ResourceRequest{Kind: resourcestest.SecurityGroups})
	if vendors.CodeOf(err) != vendors.CodeResourceMissing || !strings.Contains(err.Error(), "opt-in tag") ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("%v, want ResourceMissing naming the ambiguous opt-in tag", err)
	}
	if len(e.drv.CallLog()) != 0 {
		t.Errorf("an ambiguous opt-in fell through to the next step: %v", e.drv.CallLog())
	}
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	if bad, why := rep.NotReady(); len(bad) != 1 || !strings.Contains(why[region1], "securityGroups") ||
		!strings.Contains(why[region1], "ambiguous") {
		t.Errorf("check: %v %v", bad, why)
	}
}

func TestAVendorErrorStopsTheOrder(t *testing.T) {
	boom := vendors.Errorf(vendors.Retryable, "the API is down")
	for _, op := range []string{"resolve/subnets", "default/subnets"} {
		t.Run(op, func(t *testing.T) {
			nc := class("c", nil)
			e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
			e.drv.Fail[op] = boom
			_, err := e.require(nc, region1, subnets())
			if !vendors.IsClass(err, vendors.Retryable) {
				t.Fatalf("%v, want the vendor's error", err)
			}
			if len(e.drv.CallLog()) != 0 {
				t.Errorf("a vendor error fell through to creation: %v", e.drv.CallLog())
			}
			rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
			if len(rep.Errors) == 0 {
				t.Error("the check did not report the vendor's error")
			}
			if st, known := rep.Regions[region1]; known {
				t.Errorf("a transient failure must keep the last verdict, got %+v", st)
			}
		})
	}
}

func TestForbidFailsWithTheKindAndWhatWasTried(t *testing.T) {
	nc := class("c", &aws.Resources{Subnets: &common.Resource{Forbid: true}})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	_, why := rep.NotReady()
	for _, want := range []string{"subnets", "opt-in tag " + vendors.DefaultTagKey(vendors.TagUse) + "=true", "nothing is tagged",
		"the vendor's default: none", "creating it is forbidden"} {
		if !strings.Contains(why[region1], want) {
			t.Errorf("the failure %q lacks %q", why[region1], want)
		}
	}
	_, err := e.require(nc, region1, subnets())
	if vendors.CodeOf(err) != vendors.CodeResourceForbidden || !strings.Contains(err.Error(), "us-east-1") ||
		!strings.Contains(err.Error(), "subnets") || !strings.Contains(err.Error(), "nothing is tagged") {
		t.Errorf("the launch error %v", err)
	}
	if len(e.drv.CallLog()) != 0 {
		t.Errorf("forbid created: %v", e.drv.CallLog())
	}
	// A tagged resource satisfies a forbid.
	e.drv.Existing = []resourcestest.Existing{ex(resourcestest.Subnets, region1, "tagged", tagged)}
	e.clock.Step(2 * time.Minute)
	rep = e.engine.Check(t.Context(), nc, "aws", []string{region1})
	if bad, _ := rep.NotReady(); len(bad) != 0 || source(t, rep, resourcestest.Subnets, region1, "tagged") != v1alpha1.ResourceSourceDiscovered {
		t.Errorf("forbid with a tagged resource: %v %+v", bad, rep.Resources)
	}
}

func TestACreationThatFailsLeavesNothingAndIsNotAFallThrough(t *testing.T) {
	boom := vendors.Errorf(vendors.Fatal, "access denied")
	nc := class("c", nil)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.EnsureErr[resourcestest.Subnets] = boom
	_, err := e.require(nc, region1, subnets())
	if !errors.Is(err, boom) && !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("%v, want the vendor's error", err)
	}
	if got := e.drv.Managed(); len(got) != 0 {
		t.Errorf("a failed creation left %v", got)
	}
	// The next attempt, once the vendor allows it, creates.
	delete(e.drv.EnsureErr, resourcestest.Subnets)
	refs, err := e.require(nc, region1, subnets())
	if err != nil || len(refs) != 1 || !refs[0].Managed {
		t.Errorf("the retry: %+v, %v", refs, err)
	}
}

func TestKindsThatLiveInANetworkAreResolvedWithinIt(t *testing.T) {
	// The subnets are found by the opt-in tag in vpc-1; the groups tagged in both
	// networks resolve to the one in vpc-1, whichever step found the network.
	for _, subnet := range []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-1", Network: "vpc-1", Tags: optIn},
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-d", Network: "vpc-1", Default: true},
	} {
		t.Run(subnet.ID, func(t *testing.T) {
			nc := class("c", nil)
			e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
			e.drv.Existing = []resourcestest.Existing{subnet,
				{Kind: resourcestest.SecurityGroups, Region: region1, ID: "sg-2", Network: "vpc-2", Tags: optIn},
				{Kind: resourcestest.SecurityGroups, Region: region1, ID: "sg-1", Network: "vpc-1", Tags: optIn}}
			rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
			if bad, why := rep.NotReady(); len(bad) != 0 {
				t.Fatalf("not ready: %v", why)
			}
			got := ""
			for _, r := range rep.Resources {
				if r.Kind == string(resourcestest.SecurityGroups) {
					got += r.ID + " "
				}
			}
			if strings.TrimSpace(got) != "sg-1" {
				t.Errorf("the groups of the check: %q, want only sg-1 of the network of the subnets", got)
			}
			// At a launch the driver names the network of the subnet it chose.
			refs, err := e.require(nc, region1, vendors.ResourceRequest{Kind: resourcestest.SecurityGroups, Network: "vpc-2"})
			if err != nil || len(refs) != 1 || refs[0].ID != "sg-2" {
				t.Errorf("a launch into vpc-2: %+v, %v", refs, err)
			}
		})
	}
}

func TestAGroupOutsideTheNetworkOfTheLaunchSkipsOnlyItsZone(t *testing.T) {
	nc := class("c", &aws.Resources{SecurityGroups: &common.Resource{Use: &common.ResourceUse{Name: "sg"}}})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.SecurityGroups, Region: region1, ID: "sg-1", Name: "sg", Network: "vpc-1"}}
	refs, err := e.require(nc, region1, vendors.ResourceRequest{Kind: resourcestest.SecurityGroups, Network: "vpc-1"})
	if err != nil || len(refs) != 1 {
		t.Fatalf("inside the network: %+v, %v", refs, err)
	}
	e.clock.Step(2 * time.Minute)
	_, err = e.require(nc, region1, vendors.ResourceRequest{Kind: resourcestest.SecurityGroups, Network: "vpc-2", Zone: "us-east-1a"})
	if vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Fatalf("outside the network: %v, want ResourceMissingInZone", err)
	}
	if st, ok := e.engine.RegionState("c", "aws", region1); ok && !st.Ready && len(st.Zones) == 0 {
		t.Errorf("the region is gated by a mismatch of one launch: %+v", st)
	}
}

func TestDiscoveryOfAGlobalKindListsOnce(t *testing.T) {
	nc := class("c", nil)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.GlobalSubnets = true
	e.drv.Existing = []resourcestest.Existing{ex(resourcestest.Subnets, region1, "fw", tagged)}
	e.engine.Check(t.Context(), nc, "aws", []string{region1, region2})
	if n := e.drv.ResolveCallsOf(resourcestest.Subnets); n != 1 {
		t.Errorf("%d discovery listings for two regions of a global kind, want 1", n)
	}
}

func TestSourcesOfManagedAndSharedResources(t *testing.T) {
	nc := class("c", &aws.Resources{Subnets: &common.Resource{Create: &common.ResourceCreate{
		Timing: &common.Timing{Mode: common.TimingPreCreate, Regions: []string{region1}}}}})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	sources := map[string][]v1alpha1.ResourceSource{}
	for _, r := range rep.Resources {
		sources[r.Kind] = append(sources[r.Kind], r.Source)
	}
	if !slices.Equal(sources["subnets"], []v1alpha1.ResourceSource{v1alpha1.ResourceSourceCreated}) {
		t.Errorf("sources %v, want the preCreated subnet as created", sources)
	}
}
