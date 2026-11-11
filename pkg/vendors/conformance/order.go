/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package conformance

import (
	"slices"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// DefaultState is what the account has as the vendor's default of the order kind.
type DefaultState int

// The states of the vendor's default.
const (
	// DefaultAbsent: the region has no default resource of the kind.
	DefaultAbsent DefaultState = iota
	// DefaultPresent: the default exists and honours every request.
	DefaultPresent
	// DefaultWithoutIPv6: the default exists but cannot honour publicIPv6 (a
	// default network without an IPv6 range). A vendor whose default never has
	// IPv6 treats it as DefaultPresent.
	DefaultWithoutIPv6
)

// OrderClassFunc builds the SCNodeClass of the vendor with the resource
// configuration per kind (nil entries and a nil map: no configuration), with
// networking.publicIPv6 as given.
type OrderClassFunc func(
	t *testing.T, d vendors.Driver, res map[vendors.ResourceKind]*common.Resource, ipv6 bool,
) *v1alpha1.SCNodeClass

// SetDefaultFunc arranges the vendor's default of the kind in the region.
type SetDefaultFunc func(t *testing.T, d vendors.Driver, kind vendors.ResourceKind, region string, state DefaultState)

// order is the environment of the resolution-order cases: a fresh driver behind
// the core's engine, which is what a launch and the status check use.
type order struct {
	t      *testing.T
	e      *env
	kind   vendors.ResourceKind
	mr     vendors.ManagedResources
	engine *resources.Engine
	optIn  map[string]string
}

func newOrder(t *testing.T, e *env) *order {
	t.Helper()
	mr, ok := e.d.(vendors.ManagedResources)
	if !ok || e.fx.OrderClass == nil || e.fx.Seed == nil || e.fx.SetDefault == nil || e.fx.ResourceKind == "" {
		t.Skip("the fixture has no OrderClass, SetDefault and Seed, or the driver no managed resources")
	}
	o := &order{t: t, e: e, kind: e.fx.ResourceKind, mr: mr, optIn: map[string]string{e.d.TagKey(vendors.TagUse): vendors.OptInValue}}
	o.engine = resources.New(resources.Options{Registry: vendors.NewRegistryFrom(e.d), ClusterName: clusterName})
	return o
}

func (o *order) class(cfg *common.Resource, ipv6 bool) *v1alpha1.SCNodeClass {
	o.t.Helper()
	var res map[vendors.ResourceKind]*common.Resource
	if cfg != nil {
		res = map[vendors.ResourceKind]*common.Resource{o.kind: cfg}
	}
	return o.e.fx.OrderClass(o.t, o.e.d, res, ipv6)
}

func (o *order) require(nc *v1alpha1.SCNodeClass, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	o.t.Helper()
	req.Kind = o.kind
	return o.engine.Provider(nc, o.e.d.Vendor(), o.e.region, o.e.zone).Require(o.t.Context(), req)
}

func (o *order) check(nc *v1alpha1.SCNodeClass) resources.Report {
	o.t.Helper()
	return o.engine.Check(o.t.Context(), nc, o.e.d.Vendor(), []string{o.e.region})
}

func (o *order) seedTagged(name string) vendors.ResourceRef {
	o.t.Helper()
	return o.e.fx.Seed(o.t, o.e.d, o.kind, o.e.region, SeedSpec{Name: name, Tags: o.optIn})
}

func (o *order) setDefault(state DefaultState) {
	o.t.Helper()
	o.e.fx.SetDefault(o.t, o.e.d, o.kind, o.e.region, state)
}

// managedCount is how many managed resources SCarpenter owns in the account.
func (o *order) managedCount() int {
	o.t.Helper()
	listed, err := o.mr.List(o.t.Context(), []string{o.e.region}, clusterTag)
	if err != nil {
		o.t.Fatal(err)
	}
	return len(listed)
}

func (o *order) fresh(t *testing.T) *order {
	t.Helper()
	d := o.e.fx.New(t)
	e := &env{fx: o.e.fx, d: d, region: o.e.region, zone: o.e.zone}
	return newOrder(t, e)
}

// sourceOf returns the step the check reported for the resource.
func sourceOf(rep resources.Report, kind vendors.ResourceKind, id string) v1alpha1.ResourceSource {
	for _, r := range rep.Resources {
		if r.Kind == string(kind) && r.ID == id {
			return r.Source
		}
	}
	return ""
}

// resolutionOrder runs the cases of ADR-022 against the driver behind the core's
// engine: explicit use, then the opt-in tag, then the vendor's default (unless it
// cannot honour the request), then creation, then a failure that names the kind,
// the region and what was tried.
func resolutionOrder(t *testing.T, e *env) {
	base := newOrder(t, e)
	cases := []struct {
		name string
		run  func(t *testing.T, o *order)
	}{
		{"explicit beats tagged beats default", explicitBeatsTaggedBeatsDefault},
		{"tagged beats default beats create", taggedBeatsDefaultBeatsCreate},
		{"nothing found creates", nothingFoundCreates},
		{"an explicit use that matches nothing fails and does not fall through", explicitMatchesNothing},
		{"an explicit create skips discovery and the default", explicitCreateSkips},
		{"a default that cannot honour the request is skipped", unsuitableDefaultIsSkipped},
		{"forbid ends at the failure with the kind, region and steps", forbidFailure},
		{"a creation interrupted midway is completed by the next pass", creationInterrupted},
		{"kinds in a network resolve within it", withinTheNetwork},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, base.fresh(t)) })
	}
}

func explicitBeatsTaggedBeatsDefault(t *testing.T, o *order) {
	o.setDefault(DefaultPresent)
	tagged := o.seedTagged("order-tagged")
	named := o.e.fx.Seed(t, o.e.d, o.kind, o.e.region, SeedSpec{Name: "order-explicit", Tags: map[string]string{"x": "y"}})
	nc := o.class(&common.Resource{Use: &common.ResourceUse{Name: "order-explicit"}}, false)
	refs, err := o.require(nc, vendors.ResourceRequest{})
	if err != nil || !slices.Equal(idsOf(refs), idsOf([]vendors.ResourceRef{named})) {
		t.Fatalf("explicit use = %v, %v; want only %s (not the tagged %s, not the default)", idsOf(refs), err, named.ID, tagged.ID)
	}
	rep := o.check(nc)
	if got := sourceOf(rep, o.kind, named.ID); got != v1alpha1.ResourceSourceUse {
		t.Errorf("the check says %q for the explicit use", got)
	}
}

func taggedBeatsDefaultBeatsCreate(t *testing.T, o *order) {
	o.setDefault(DefaultPresent)
	nc := o.class(nil, false)

	// The default, with nothing tagged: used as found, never created or modified.
	refs, err := o.require(nc, vendors.ResourceRequest{})
	if err != nil || len(refs) == 0 || slices.ContainsFunc(refs, func(r vendors.ResourceRef) bool { return r.Managed }) {
		t.Fatalf("the vendor's default = %v, %v; want unmanaged default resources", refs, err)
	}
	defaults := idsOf(refs)
	if rep := o.check(nc); sourceOf(rep, o.kind, refs[0].ID) != v1alpha1.ResourceSourceDefault {
		t.Errorf("the check says %q for the default", sourceOf(rep, o.kind, refs[0].ID))
	}
	if n := o.managedCount(); n != 0 {
		t.Errorf("%d managed resources although the default served", n)
	}

	// A tagged resource now wins over the default: the operator's explicit opt-in. One
	// without the tag is not discovered. (The engine trusts an answer for a while; a
	// new engine asks again.)
	o.e.fx.Seed(t, o.e.d, o.kind, o.e.region, SeedSpec{Name: "order-untagged", Tags: map[string]string{"x": "y"}})
	tagged := o.seedTagged("order-tagged")
	o.engine = resources.New(resources.Options{Registry: vendors.NewRegistryFrom(o.e.d), ClusterName: clusterName})
	refs, err = o.require(nc, vendors.ResourceRequest{})
	if err != nil || !slices.Equal(idsOf(refs), []string{tagged.ID}) {
		t.Fatalf("with a tagged resource = %v, %v; want %s and not the default %v", idsOf(refs), err, tagged.ID, defaults)
	}
	if rep := o.check(nc); sourceOf(rep, o.kind, tagged.ID) != v1alpha1.ResourceSourceDiscovered {
		t.Errorf("the check says %q for the tagged resource", sourceOf(rep, o.kind, tagged.ID))
	}
}

func nothingFoundCreates(t *testing.T, o *order) {
	o.setDefault(DefaultAbsent)
	nc := o.class(nil, false)
	refs, err := o.require(nc, vendors.ResourceRequest{Params: o.createParams()})
	if err != nil || len(refs) != 1 || !refs[0].Managed {
		t.Fatalf("with nothing found = %v, %v; want one managed resource", refs, err)
	}
	if n := o.managedCount(); n != 1 {
		t.Errorf("%d managed resources, want the created one", n)
	}
	if rep := o.check(nc); sourceOf(rep, o.kind, refs[0].ID) != v1alpha1.ResourceSourceCreated {
		t.Errorf("the check says %q for the created resource", sourceOf(rep, o.kind, refs[0].ID))
	}
}

func (o *order) createParams() []byte {
	if o.e.fx.ResourceParams == nil {
		return nil
	}
	return o.e.fx.ResourceParams(o.t, o.e.d, o.e.region)
}

func explicitMatchesNothing(t *testing.T, o *order) {
	o.setDefault(DefaultPresent)
	o.seedTagged("order-tagged")
	nc := o.class(&common.Resource{Use: &common.ResourceUse{Name: "order-absent"}}, false)
	refs, err := o.require(nc, vendors.ResourceRequest{})
	if vendors.CodeOf(err) != vendors.CodeResourceMissing || len(refs) != 0 {
		t.Fatalf("an explicit use that matches nothing = %v, %v; want ResourceMissing", refs, err)
	}
	if n := o.managedCount(); n != 0 {
		t.Errorf("%d managed resources: the order fell through to creation", n)
	}
	rep := o.check(nc)
	if bad, why := rep.NotReady(); len(bad) != 1 || !strings.Contains(why[o.e.region], string(o.kind)) {
		t.Errorf("the check: not ready %v %v; want the region failed on %s", bad, why, o.kind)
	}
	if len(rep.Resources) != 0 {
		t.Errorf("the check reports %+v although the explicit use matched nothing", rep.Resources)
	}
}

func explicitCreateSkips(t *testing.T, o *order) {
	o.setDefault(DefaultPresent)
	tagged := o.seedTagged("order-tagged")
	nc := o.class(&common.Resource{Create: &common.ResourceCreate{}}, false)
	refs, err := o.require(nc, vendors.ResourceRequest{Params: o.createParams()})
	if err != nil || len(refs) != 1 || !refs[0].Managed || refs[0].ID == tagged.ID {
		t.Fatalf("an explicit create = %v, %v; want a created resource, not the tagged or the default", refs, err)
	}
}

func unsuitableDefaultIsSkipped(t *testing.T, o *order) {
	o.setDefault(DefaultWithoutIPv6)
	ipv6 := &vendors.VendorSettings{Networking: common.Networking{PublicIPv6: new(true)}}
	nc := o.class(nil, true)
	refs, err := o.require(nc, vendors.ResourceRequest{Params: o.createParams(), Settings: ipv6})
	if err != nil || len(refs) != 1 || !refs[0].Managed {
		t.Fatalf("publicIPv6 on a default without IPv6 = %v, %v; want the default skipped and a created resource", refs, err)
	}
	// The status check asks the same of the NodeClass's own settings.
	rep := o.check(nc)
	for _, r := range rep.Resources {
		if r.Source == v1alpha1.ResourceSourceDefault {
			t.Errorf("the check reports the default %s although it cannot honour publicIPv6", r.ID)
		}
	}
	// With creation forbidden the skipped default ends in the failure, which says so.
	forbid := o.class(&common.Resource{Forbid: true}, true)
	_, err = o.require(forbid, vendors.ResourceRequest{Settings: ipv6})
	if vendors.CodeOf(err) != vendors.CodeResourceForbidden || !strings.Contains(err.Error(), "cannot honour the request") {
		t.Errorf("forbid with an unsuitable default: %v; want ResourceForbidden naming why the default was skipped", err)
	}
}

func forbidFailure(t *testing.T, o *order) {
	o.setDefault(DefaultAbsent)
	nc := o.class(&common.Resource{Forbid: true}, false)
	_, err := o.require(nc, vendors.ResourceRequest{})
	if vendors.CodeOf(err) != vendors.CodeResourceForbidden {
		t.Fatalf("forbid with nothing found: %v, want ResourceForbidden", err)
	}
	for _, want := range []string{string(o.kind), o.e.region, "nothing is tagged", "forbidden"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q does not name %q", err, want)
		}
	}
	if n := o.managedCount(); n != 0 {
		t.Errorf("%d managed resources after a forbidden creation", n)
	}
	rep := o.check(nc)
	if bad, why := rep.NotReady(); len(bad) != 1 || !strings.Contains(why[o.e.region], string(o.kind)) ||
		!strings.Contains(why[o.e.region], "forbidden") {
		t.Errorf("the check: %v %v", bad, why)
	}
}

func creationInterrupted(t *testing.T, o *order) {
	if o.e.fx.FailCreate == nil {
		t.Skip("the fixture cannot interrupt a creation")
	}
	o.setDefault(DefaultAbsent)
	nc := o.class(nil, false)
	o.e.fx.FailCreate(t, o.e.d, o.kind)
	if _, err := o.require(nc, vendors.ResourceRequest{Params: o.createParams()}); err == nil {
		t.Fatal("the interrupted creation succeeded")
	}
	// What the interrupted pass left is ours and listed (nothing is orphaned) ...
	left, err := o.mr.List(t.Context(), []string{o.e.region}, clusterTag)
	if err != nil || len(left) > 1 {
		t.Fatalf("an interrupted creation left %+v, %v; want at most one resource of ours", left, err)
	}
	// ... and the next pass adopts and completes it: one resource, no duplicate.
	refs, err := o.require(nc, vendors.ResourceRequest{Params: o.createParams()})
	if err != nil || len(refs) != 1 || !refs[0].Managed {
		t.Fatalf("the next pass = %v, %v", refs, err)
	}
	if len(left) == 1 && left[0].ID != refs[0].ID {
		t.Errorf("the next pass created %s instead of completing %s", refs[0].ID, left[0].ID)
	}
	if n := o.managedCount(); n != 1 {
		t.Errorf("%d managed resources after the second pass, want 1", n)
	}
}

// withinTheNetwork checks the kinds that live in a network: the tagged resource is
// found in its own network and in no other.
func withinTheNetwork(t *testing.T, o *order) {
	var found bool
	for _, k := range o.mr.Kinds() {
		if k.InNetwork == "" || k.Scope != vendors.ScopeRegion || !slices.Contains(seedKinds(o.e.fx), k.Kind) {
			continue
		}
		found = true
		o2 := *o
		o2.kind = k.Kind
		tagged := o.e.fx.Seed(t, o.e.d, k.Kind, o.e.region, SeedSpec{Name: "order-in-network", Tags: o.optIn})
		got, err := o.mr.Resolve(t.Context(), k.Kind, o.e.region, vendors.UseRef{IDs: []string{tagged.ID}})
		if err != nil || len(got) != 1 || got[0].Network == "" {
			t.Fatalf("%s: the driver does not report the network of %s: %v, %v", k.Kind, tagged.ID, got, err)
		}
		// Discovery and an explicit use alike stay within the network the launch is in.
		for name, cfg := range map[string]*common.Resource{
			"discovery": nil, "use": {Use: &common.ResourceUse{Tags: o.optIn}},
		} {
			var res map[vendors.ResourceKind]*common.Resource
			if cfg != nil {
				res = map[vendors.ResourceKind]*common.Resource{k.Kind: cfg}
			}
			nc := o.e.fx.OrderClass(t, o.e.d, res, false)
			refs, err := o2.require(nc, vendors.ResourceRequest{Network: got[0].Network})
			if err != nil || !slices.Equal(idsOf(refs), []string{tagged.ID}) {
				t.Errorf("%s of %s in its network = %v, %v; want %s", name, k.Kind, idsOf(refs), err, tagged.ID)
			}
			refs, err = o2.require(nc, vendors.ResourceRequest{Network: "no-such-network", Zone: o.e.zone})
			if slices.Contains(idsOf(refs), tagged.ID) {
				t.Errorf("%s of %s in another network returned %s of the first", name, k.Kind, tagged.ID)
			}
			if name == "use" && vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
				t.Errorf("an explicit use of %s in another network: %v; want ResourceMissingInZone", k.Kind, err)
			}
		}
	}
	if !found {
		t.Skip("the driver has no kind that lives in a network")
	}
}

func seedKinds(fx Fixture) []vendors.ResourceKind {
	if len(fx.SeedKinds) > 0 {
		return fx.SeedKinds
	}
	return []vendors.ResourceKind{fx.ResourceKind}
}
