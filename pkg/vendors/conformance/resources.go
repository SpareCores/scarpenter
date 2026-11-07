/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// directProvider is a ResourceProvider without the engine's policies: it
// resolves references and ensures resources by a deterministic name, which is
// what the engine does for a launch.
type directProvider struct {
	mr     vendors.ManagedResources
	region string
	tags   map[vendors.TagKey]string
}

func (p directProvider) Require(ctx context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	if req.Use != nil {
		return p.mr.Resolve(ctx, req.Kind, p.region, *req.Use)
	}
	sum := sha256.Sum256([]byte(req.Zone + "/" + req.Key))
	name := fmt.Sprintf("scarpenter-conformance-%s-%s-%s",
		req.Kind, p.region, hex.EncodeToString(sum[:])[:8])
	tags := maps.Clone(p.tags)
	tags[vendors.TagResource] = string(req.Kind)
	ref, _, err := p.mr.Ensure(ctx, req.Kind, p.region,
		vendors.CreateSpec{Name: name, Zone: req.Zone, Tags: tags, Params: req.Params})
	if err != nil {
		return nil, err
	}
	return []vendors.ResourceRef{ref}, nil
}

var resourceTags = map[vendors.TagKey]string{
	vendors.TagCluster: clusterName, vendors.TagNodeClass: nodeClassName, vendors.TagManagedBy: vendors.ManagedByValue,
}

func nodeOptions(t *testing.T, e *env) {
	no, ok := e.d.(vendors.NodeOptions)
	if !ok {
		t.Skip("the driver has no node options")
	}
	defaults := no.Defaults()
	if len(defaults.Raw) > 0 {
		var m map[string]any
		if err := json.Unmarshal(defaults.Raw, &m); err != nil {
			t.Errorf("the defaults are not a JSON object: %v", err)
		}
	}
	if got := no.RootDiskBytes(defaults); got != defaults.RootDiskBytes {
		t.Errorf("RootDiskBytes(Defaults()) = %d, Defaults().RootDiskBytes = %d", got, defaults.RootDiskBytes)
	}
	// An empty profile set supports nothing but the default.
	for _, it := range e.fx.ProfileTypes {
		if got := no.Supported(it, e.zone, vendors.Profiles{}); len(got) != 0 {
			t.Errorf("%s: Supported without profiles = %v", it.APIReference, got)
		}
	}

	in := options.Input{Block: json.RawMessage(`{}`), Profiles: e.fx.Profiles}
	if err := options.Validate(no, in); err != nil {
		t.Fatalf("the fixture's profiles do not resolve against the defaults: %v", err)
	}
	for _, it := range e.fx.ProfileTypes {
		first := no.Supported(it, e.zone, e.fx.Profiles)
		again := no.Supported(it, e.zone, e.fx.Profiles)
		if fmt.Sprint(first) != fmt.Sprint(again) {
			t.Errorf("%s: Supported is not deterministic: %v and %v", it.APIReference, first, again)
		}
		for kind, names := range first {
			if kind != vendors.ProfileStorage && kind != vendors.ProfileNetwork && kind != vendors.ProfilePlacement {
				t.Errorf("%s: unknown profile kind %q", it.APIReference, kind)
			}
			for _, name := range names {
				if _, defined := e.fx.Profiles[kind][name]; !defined && name != vendors.DefaultProfile {
					t.Errorf("%s: Supported returned the undefined profile %s/%s", it.APIReference, kind, name)
				}
			}
		}
		// Every defined profile is supported or explicitly absent; a supported
		// one resolves, has a finite non-negative price and prepares idempotently.
		for kind, byName := range e.fx.Profiles {
			for name := range byName {
				if !slices.Contains(first[kind], name) {
					continue
				}
				sel := options.Selection{}
				switch kind {
				case vendors.ProfileStorage:
					sel.Storage = name
				case vendors.ProfileNetwork:
					sel.Network = name
				default:
					sel.Placement = name
				}
				settings, err := options.Resolve(no, in, sel)
				if err != nil {
					t.Errorf("%s: the supported profile %s/%s does not resolve: %v", it.APIReference, kind, name, err)
					continue
				}
				delta, _ := no.PriceDelta(settings, vendors.Offering{
					Region: e.region, Zone: e.zone, CapacityType: vendors.CapacityOnDemand, APIReference: it.APIReference, Type: it,
				})
				if delta < 0 || delta != delta {
					t.Errorf("%s: the price delta of %s/%s is %v", it.APIReference, kind, name, delta)
				}
				e.prepareTwice(t, no, it, settings)
			}
		}
	}
}

// prepareTwice runs Prepare twice with one request: the second run must find
// what the first made.
func (e *env) prepareTwice(t *testing.T, no vendors.NodeOptions, it vendors.CatalogType, s vendors.VendorSettings) {
	t.Helper()
	mr, ok := e.d.(vendors.ManagedResources)
	if !ok {
		return
	}
	req := vendors.PrepareRequest{
		Region: e.region, Zone: e.zone, InstanceType: it.APIReference, CapacityType: vendors.CapacityOnDemand, Type: it,
		Settings: s,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: clusterName, vendors.TagNodeClaim: "prepare-claim", vendors.TagNodeClass: nodeClassName,
			vendors.TagManagedBy: vendors.ManagedByValue,
		},
		RunKey:    "conformance-run",
		Resources: directProvider{mr: mr, region: e.region, tags: resourceTags},
	}
	first, err := no.Prepare(t.Context(), req)
	if err != nil {
		t.Errorf("Prepare(%s, %v): %v", it.APIReference, s.Profiles, err)
		return
	}
	second, err := no.Prepare(t.Context(), req)
	if err != nil {
		t.Errorf("the second Prepare: %v", err)
		return
	}
	ids := func(p *vendors.Prepared) []string {
		out := make([]string, 0, len(p.Resources))
		for _, r := range p.Resources {
			out = append(out, string(r.Kind)+"/"+r.ID)
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(ids(first), ids(second)) {
		t.Errorf("Prepare is not idempotent: %v then %v", ids(first), ids(second))
	}
	if fmt.Sprint(first.Devices) != fmt.Sprint(second.Devices) || fmt.Sprint(first.Local) != fmt.Sprint(second.Local) {
		t.Errorf("Prepare returned other device hints the second time: %v and %v", first.Devices, second.Devices)
	}
	for _, d := range first.Devices {
		if (d.ByID == "") == (d.SerialPattern == "") || d.MountPath == "" || d.Name == "" {
			t.Errorf("an invalid device hint %+v: exactly one of by-id and serial pattern, and a name and mount path", d)
		}
	}
}

func managedResources(t *testing.T, e *env) {
	mr, ok := e.d.(vendors.ManagedResources)
	if !ok {
		t.Skip("the driver has no managed resources")
	}
	kind := e.fx.ResourceKind
	if kind == "" {
		t.Skip("the fixture names no resource kind")
	}
	checkKinds(t, mr, kind)
	spec := vendors.CreateSpec{
		Name: "scarpenter-conformance-" + string(kind) + "-" + e.region,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: clusterName, vendors.TagNodeClass: nodeClassName, vendors.TagManagedBy: vendors.ManagedByValue,
			vendors.TagResource: string(kind),
		},
	}
	first := checkEnsureAndList(t, mr, e, kind, spec)
	if e.fx.PreCreate != nil {
		checkUseResources(t, mr, e, kind)
	}
	if _, err := mr.Resolve(t.Context(), kind, e.region, vendors.UseRef{}); err != nil {
		t.Errorf("Resolve of the vendor default: %v", err)
	}
	if e.fx.Restart != nil {
		checkAdoption(t, e, kind, spec, first)
	}
	checkInUseAndDelete(t, mr, e, first)
}

// checkKinds checks that the kinds are well formed and include the fixture's.
func checkKinds(t *testing.T, mr vendors.ManagedResources, kind vendors.ResourceKind) {
	t.Helper()
	kinds := mr.Kinds()
	if len(kinds) == 0 {
		t.Fatal("no resource kinds")
	}
	seen := map[vendors.ResourceKind]bool{}
	known := false
	for _, k := range kinds {
		switch {
		case k.Kind == "" || seen[k.Kind]:
			t.Errorf("kind %q is empty or listed twice", k.Kind)
		case k.Scope != vendors.ScopeRegion && k.Scope != vendors.ScopeRun && k.Scope != vendors.ScopeNode:
			t.Errorf("kind %s has the scope %q", k.Kind, k.Scope)
		case k.DefaultPolicy != "perUse" && k.DefaultPolicy != "idleTTL" && k.DefaultPolicy != "retain":
			t.Errorf("kind %s has the default policy %q", k.Kind, k.DefaultPolicy)
		}
		seen[k.Kind] = true
		known = known || k.Kind == kind
	}
	if !known {
		t.Fatalf("the fixture's kind %s is not a kind of the driver", kind)
	}
}

// checkEnsureAndList checks that Ensure is idempotent, that tags round-trip and
// that List filters by region and marker. It returns the created resource.
func checkEnsureAndList(
	t *testing.T, mr vendors.ManagedResources, e *env, kind vendors.ResourceKind, spec vendors.CreateSpec,
) vendors.ResourceRef {
	t.Helper()
	ctx := t.Context()
	first, created, err := mr.Ensure(ctx, kind, e.region, spec)
	if err != nil || !created || !first.Managed || first.ID == "" || first.Name != spec.Name {
		t.Fatalf("Ensure = %+v, %v, %v; want a created managed resource", first, created, err)
	}
	second, created, err := mr.Ensure(ctx, kind, e.region, spec)
	if err != nil || created || second.ID != first.ID {
		t.Errorf("a second Ensure = %+v, created=%v, %v; want the same resource, not created", second, created, err)
	}
	listed, err := mr.List(ctx, []string{e.region}, clusterTag)
	if err != nil || len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatalf("List by the marker = %+v, %v", listed, err)
	}
	for k, v := range spec.Tags {
		if listed[0].Tags[k] != v {
			t.Errorf("tag %s round-trips as %q, want %q", k, listed[0].Tags[k], v)
		}
	}
	if other, err := mr.List(ctx, []string{e.region + "-nowhere"}, clusterTag); err != nil || len(other) != 0 {
		t.Errorf("List in another region = %+v, %v", other, err)
	}
	foreign := map[vendors.TagKey]string{vendors.TagCluster: "another-cluster"}
	if other, err := mr.List(ctx, nil, foreign); err != nil || len(other) != 0 {
		t.Errorf("List of another cluster = %+v, %v", other, err)
	}
	return first
}

const useTagKey = "scarpenter-conformance-use"

// checkUseResources checks that resources the user brought are found, are not
// ours, and are never modified or deleted.
func checkUseResources(t *testing.T, mr vendors.ManagedResources, e *env, kind vendors.ResourceKind) {
	t.Helper()
	ctx := t.Context()
	user := e.fx.PreCreate(t, e.d, kind, e.region, map[string]string{useTagKey: "true"})
	for name, ref := range map[string]vendors.UseRef{
		"id": {IDs: []string{user.ID}}, "tags": {Tags: map[string]string{useTagKey: "true"}},
	} {
		got, err := mr.Resolve(ctx, kind, e.region, ref)
		if err != nil || len(got) != 1 || got[0].ID != user.ID || got[0].Managed {
			t.Errorf("Resolve by %s = %+v, %v; want the user's unmanaged resource", name, got, err)
		}
	}
	nothing := vendors.UseRef{Tags: map[string]string{useTagKey: "absent"}}
	if got, err := mr.Resolve(ctx, kind, e.region, nothing); err != nil || len(got) != 0 {
		t.Errorf("Resolve of nothing = %+v, %v; want an empty result, not an error", got, err)
	}
	if err := mr.Delete(ctx, vendors.ResourceRef{Kind: kind, Region: e.region, ID: user.ID, Name: user.Name}); err == nil {
		t.Error("Delete of a resource SCarpenter did not create succeeded")
	}
	if got, _ := mr.Resolve(ctx, kind, e.region, vendors.UseRef{IDs: []string{user.ID}}); len(got) != 1 {
		t.Error("a Delete of a use resource removed it")
	}
	if listed, _ := mr.List(ctx, []string{e.region}, clusterTag); len(listed) != 1 {
		t.Errorf("a use resource shows up as managed: %+v", listed)
	}
}

// checkAdoption checks that managed resources are found again after a restart.
func checkAdoption(
	t *testing.T, e *env, kind vendors.ResourceKind, spec vendors.CreateSpec, first vendors.ResourceRef,
) {
	t.Helper()
	d2 := e.fx.Restart(t, e.d)
	mr2, ok := d2.(vendors.ManagedResources)
	if !ok {
		t.Fatal("the restarted driver has no managed resources")
	}
	listed, err := mr2.List(t.Context(), []string{e.region}, clusterTag)
	if err != nil || len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatalf("after a restart List = %+v, %v; want the resource %s", listed, err, first.ID)
	}
	again, created, err := mr2.Ensure(t.Context(), kind, e.region, spec)
	if err != nil || created || again.ID != first.ID {
		t.Errorf("after a restart Ensure = %+v, created=%v, %v; want adoption of %s", again, created, err, first.ID)
	}
}

// checkInUseAndDelete checks that an instance that uses the resource keeps it:
// it is in use and its deletion is refused (the vendor refuses too); after the
// instance is gone it can be deleted, twice.
func checkInUseAndDelete(t *testing.T, mr vendors.ManagedResources, e *env, first vendors.ResourceRef) {
	t.Helper()
	ctx := t.Context()
	if inUse, _, err := mr.InUse(ctx, first); err != nil || inUse {
		t.Fatalf("InUse of a fresh resource = %v, %v", inUse, err)
	}
	req := e.request(t)
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{first}}
	inst := e.create(t, req)
	if inUse, _, err := mr.InUse(ctx, first); err != nil || !inUse {
		t.Errorf("InUse with a running instance = %v, %v", inUse, err)
	}
	if err := mr.Delete(ctx, first); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("Delete of a resource in use = %v, want Retryable", err)
	}
	if err := e.d.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	inUse, last, err := mr.InUse(ctx, first)
	if err != nil || inUse {
		t.Errorf("InUse after the instance was deleted = %v, %v, %v; want unused (last use may be unknown)", inUse, last, err)
	}
	for i := range 2 {
		if err := mr.Delete(ctx, first); err != nil {
			t.Fatalf("Delete #%d: %v", i+1, err)
		}
	}
	if listed, _ := mr.List(ctx, []string{e.region}, clusterTag); len(listed) != 0 {
		t.Errorf("a deleted resource is still listed: %+v", listed)
	}
}
