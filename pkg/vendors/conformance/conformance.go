/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package conformance is the test suite every driver runs against its
// in-process fake (and, opt-in, against the real vendor): overview section 7.4.
// Adding a vendor means implementing vendors.Driver and passing Run.
//
//	func TestConformance(t *testing.T) {
//	    conformance.Run(t, conformance.Fixture{New: newDriver, InstanceType: "m7i.large", ...})
//	}
package conformance

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// PreCreateFunc makes a resource the way a user would: not managed, with the
// tags.
type PreCreateFunc func(
	t *testing.T, d vendors.Driver, kind vendors.ResourceKind, region string, tags map[string]string,
) vendors.ResourceRef

// Fixture connects the suite to a driver.
type Fixture struct {
	// New returns a fresh, initialised driver. The suite calls it once per
	// subtest, so subtests never share instances.
	New func(t *testing.T) vendors.Driver

	// Region and Zone (the account's name) are where the suite launches; empty
	// means the first region and its first zone.
	Region string
	Zone   string
	// InstanceType (API reference) is a type launchable in Region and Zone,
	// with CapacityType (default on-demand) and Arch (default amd64).
	InstanceType string
	CapacityType string
	Arch         string

	// Zoneless says the vendor has no zones: Zones may be empty.
	Zoneless bool
	// MaxUserData is the largest user-data the vendor accepts; the suite checks
	// that one byte more is rejected as Fatal. 0 skips the check.
	MaxUserData int

	// Inject makes the next CreateInstance that matches req (same region, zone,
	// type and capacity type) fail with an error the vendor would classify as
	// class. Without it the classification subtests are skipped.
	Inject func(t *testing.T, d vendors.Driver, class vendors.Class, req vendors.CreateRequest)

	// The quota subtests run for drivers that implement vendors.QuotaReporter.
	//
	// InstanceTypes are the API references of the vendor's catalog (or a
	// sample): each must map to buckets or be listed in UnknownQuotaTypes.
	InstanceTypes     []string
	UnknownQuotaTypes []string
	// ExhaustQuota makes the buckets that req consumes unable to take another
	// instance (the vendor's next launch of req fails with its quota error).
	ExhaustQuota func(t *testing.T, d vendors.Driver, req vendors.CreateRequest)

	// The node options subtests run for drivers that implement
	// vendors.NodeOptions.
	//
	// Profiles are sample profile blocks (kind, name, the vendor's JSON) and
	// ProfileTypes the instance types to check them against: for every type
	// each profile is either supported, and then resolves, prices and prepares
	// idempotently, or explicitly left out of Supported.
	Profiles     vendors.Profiles
	ProfileTypes []vendors.CatalogType

	// The managed resources subtests run for drivers that implement
	// vendors.ManagedResources.
	//
	// ResourceKind is a regional kind that SCarpenter may create. PreCreate
	// makes a resource of the kind the way a user would (not managed, with the
	// tags), Restart returns a new driver over the same account, as after a
	// restart of the controller. Subtests without their hook are skipped.
	ResourceKind vendors.ResourceKind
	PreCreate    PreCreateFunc
	Restart      func(t *testing.T, d vendors.Driver) vendors.Driver
}

const (
	clusterName   = "conformance"
	nodeClassName = "class"
	otherSuffix   = "-other"
)

var (
	counter    atomic.Int64
	clusterTag = map[vendors.TagKey]string{vendors.TagCluster: clusterName}
)

func claimTag(name string) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{vendors.TagNodeClaim: name}
}

// env is what one subtest works with.
type env struct {
	fx     Fixture
	d      vendors.Driver
	region string
	zone   string
}

func (e *env) request(t *testing.T) vendors.CreateRequest {
	t.Helper()
	n := counter.Add(1)
	name := fmt.Sprintf("conf-%d", n)
	capType := e.fx.CapacityType
	if capType == "" {
		capType = vendors.CapacityOnDemand
	}
	arch := e.fx.Arch
	if arch == "" {
		arch = "amd64"
	}
	return vendors.CreateRequest{
		Region: e.region, Zone: e.zone, InstanceType: e.fx.InstanceType, CapacityType: capType, Arch: arch,
		Image:    vendors.Image{Ref: "image"},
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: clusterName, vendors.TagNodeClaim: name, vendors.TagNodePool: "pool",
			vendors.TagNodeClass: nodeClassName, vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: fmt.Sprintf("uid-%d", n),
		Node: vendors.NodeInfo{
			Name: name, ProviderID: fmt.Sprintf("scarpenter://%s/%s/%s", e.d.Vendor(), e.region, name),
		},
	}
}

func (e *env) create(t *testing.T, req vendors.CreateRequest) *vendors.Instance {
	t.Helper()
	inst, err := e.d.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatalf("CreateInstance(%s): %v", req, err)
	}
	return inst
}

// Run executes the suite.
func Run(t *testing.T, fx Fixture) {
	t.Helper()
	if fx.New == nil || fx.InstanceType == "" {
		t.Fatal("conformance: the fixture needs New and InstanceType")
	}
	sub := func(name string, fn func(t *testing.T, e *env)) {
		t.Run(name, func(t *testing.T) {
			d := fx.New(t)
			e := &env{fx: fx, d: d, region: fx.Region, zone: fx.Zone}
			if e.region == "" {
				regions, err := d.Regions(t.Context())
				if err != nil || len(regions) == 0 {
					t.Fatalf("Regions: %v, %v", regions, err)
				}
				e.region = regions[0].Name
			}
			if e.zone == "" {
				zones, err := d.Zones(t.Context(), e.region)
				if err != nil {
					t.Fatalf("Zones(%s): %v", e.region, err)
				}
				if len(zones) > 0 {
					e.zone = zones[0].Name
				}
			}
			fn(t, e)
		})
	}

	sub("identity", func(t *testing.T, e *env) {
		if e.d.Vendor() == "" {
			t.Error("Vendor is empty")
		}
		for _, k := range vendors.TagKeys {
			if e.d.TagKey(k) == "" {
				t.Errorf("TagKey(%s) is empty", k)
			}
		}
	})
	sub("zone mapping", zoneMapping)
	sub("idempotent create", idempotentCreate)
	sub("idempotency key semantics", idempotencyKeySemantics)
	sub("not found", notFound)
	sub("list by tags", listByTags)
	sub("delete is idempotent", deleteIdempotent)
	sub("tag round trip", tagRoundTrip)
	sub("user-data limit", userDataLimit)
	sub("error classification", errorClassification)
	sub("quota reporter", quotaReporter)
	sub("offering lister", offeringLister)
	sub("node options", nodeOptions)
	sub("managed resources", managedResources)
}

func zoneMapping(t *testing.T, e *env) {
	regions, err := e.d.Regions(t.Context())
	if err != nil || len(regions) == 0 {
		t.Fatalf("Regions: %v, %v", regions, err)
	}
	for _, r := range regions {
		if r.Name == "" {
			t.Error("a region has no name")
		}
		zones, err := e.d.Zones(t.Context(), r.Name)
		if err != nil {
			t.Errorf("Zones(%s): %v", r.Name, err)
			continue
		}
		if len(zones) == 0 && !e.fx.Zoneless {
			t.Errorf("region %s has no zones and the vendor is not zoneless", r.Name)
		}
		seen := map[string]bool{}
		for _, z := range zones {
			if z.ID == "" || z.Name == "" {
				t.Errorf("zone %+v of %s lacks an ID or a name", z, r.Name)
			}
			if seen[z.ID] {
				t.Errorf("zone ID %s of %s appears twice", z.ID, r.Name)
			}
			seen[z.ID] = true
		}
	}
}

func idempotentCreate(t *testing.T, e *env) {
	req := e.request(t)
	first := e.create(t, req)
	second := e.create(t, req)
	if first.Ref != second.Ref {
		t.Errorf("the same idempotency key launched two instances: %s and %s", first.Ref, second.Ref)
	}
	other := req
	other.IdempotencyKey += otherSuffix
	other.Tags = cloneTags(req.Tags)
	other.Tags[vendors.TagNodeClaim] += otherSuffix
	other.Node.Name += otherSuffix
	if third := e.create(t, other); third.Ref == first.Ref {
		t.Error("a different idempotency key returned the first instance")
	}
	if first.Region != e.region || first.Type != req.InstanceType || first.CapacityType != req.CapacityType {
		t.Errorf("the instance does not match the request: %+v", first)
	}
	if first.State != vendors.StateRunning && first.State != vendors.StatePending {
		t.Errorf("a new instance is %q", first.State)
	}
	if first.CreatedAt.IsZero() {
		t.Error("the instance has no creation time")
	}
}

// otherZone returns a zone of the request's region other than the request's,
// or "" when there is none.
func (e *env) otherZone(t *testing.T, req vendors.CreateRequest) string {
	t.Helper()
	zones, err := e.d.Zones(t.Context(), req.Region)
	if err != nil {
		t.Fatalf("Zones(%s): %v", req.Region, err)
	}
	for _, z := range zones {
		if z.Name != req.Zone {
			return z.Name
		}
	}
	return ""
}

// idempotencyKeySemantics pins the two rules the CloudProvider's attempt loop
// relies on: replaying a key returns the original instance whatever else the
// request says, and a failed create leaves the key unused.
func idempotencyKeySemantics(t *testing.T, e *env) {
	req := e.request(t)
	other := e.otherZone(t, req)
	if other == "" {
		t.Skip("the region has one zone")
	}
	first := e.create(t, req)
	replay := req
	replay.Zone = other
	got, err := e.d.CreateInstance(t.Context(), replay)
	if err != nil || got.Ref != first.Ref || got.Zone != first.Zone {
		t.Errorf("a replay with another zone returned %+v, %v; want the original instance %s in %s",
			got, err, first.Ref, first.Zone)
	}

	if e.fx.Inject == nil {
		return
	}
	failing := e.request(t)
	e.fx.Inject(t, e.d, vendors.InsufficientCapacity, failing)
	if _, err := e.d.CreateInstance(t.Context(), failing); err == nil {
		t.Fatal("the injected error did not fail the create")
	}
	retry := failing
	retry.Zone = e.otherZone(t, failing)
	inst, err := e.d.CreateInstance(t.Context(), retry)
	if err != nil {
		t.Fatalf("a create with the key of a failed one (another zone) failed: %v", err)
	}
	if inst.Zone != retry.Zone {
		t.Errorf("the key of a failed create was consumed: launched in %q, want %q", inst.Zone, retry.Zone)
	}
}

func notFound(t *testing.T, e *env) {
	ref := vendors.NewInstanceRef(e.region, "i-does-not-exist")
	if _, err := e.d.GetInstance(t.Context(), ref); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("GetInstance of an unknown instance: %v, want NotFound", err)
	}
	tags := map[vendors.TagKey]string{vendors.TagNodeClaim: "nobody"}
	if _, err := e.d.FindInstance(t.Context(), e.region, tags); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("FindInstance without a match: %v, want NotFound", err)
	}
	req := e.request(t)
	inst := e.create(t, req)
	got, err := e.d.GetInstance(t.Context(), inst.Ref)
	if err != nil || got.Ref != inst.Ref {
		t.Fatalf("GetInstance: %v, %v", got, err)
	}
	found, err := e.d.FindInstance(t.Context(), e.region, claimTag(req.Tags[vendors.TagNodeClaim]))
	if err != nil || found.Ref != inst.Ref {
		t.Fatalf("FindInstance: %v, %v", found, err)
	}
}

func listByTags(t *testing.T, e *env) {
	a, b := e.request(t), e.request(t)
	b.Tags[vendors.TagCluster] = "another-cluster"
	ia, ib := e.create(t, a), e.create(t, b)
	list, err := e.d.ListInstances(t.Context(), []string{e.region}, clusterTag)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRef(list, ia.Ref) || hasRef(list, ib.Ref) {
		t.Errorf("listing by the cluster tag returned %v, want only %s", refs(list), ia.Ref)
	}
	other, err := e.d.ListInstances(t.Context(), []string{e.region + "-nowhere"}, nil)
	if err != nil || len(other) != 0 {
		t.Errorf("listing another region returned %v, %v", refs(other), err)
	}
	if err := e.d.DeleteInstance(t.Context(), ia.Ref); err != nil {
		t.Fatal(err)
	}
	list, err = e.d.ListInstances(t.Context(), []string{e.region}, clusterTag)
	if err != nil || hasRef(list, ia.Ref) {
		t.Errorf("a deleted instance is still listed: %v, %v", refs(list), err)
	}
	_, err = e.d.FindInstance(t.Context(), e.region, claimTag(a.Tags[vendors.TagNodeClaim]))
	if !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("FindInstance of a deleted instance: %v, want NotFound", err)
	}
}

func deleteIdempotent(t *testing.T, e *env) {
	inst := e.create(t, e.request(t))
	for i := range 2 {
		if err := e.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
			t.Fatalf("DeleteInstance #%d: %v", i+1, err)
		}
	}
	got, err := e.d.GetInstance(t.Context(), inst.Ref)
	if err == nil && !got.Gone() {
		t.Errorf("a deleted instance is %q", got.State)
	}
	if err != nil && !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("GetInstance of a deleted instance: %v", err)
	}
	unknown := vendors.NewInstanceRef(e.region, "i-never-existed")
	if err := e.d.DeleteInstance(t.Context(), unknown); err != nil {
		t.Errorf("deleting an unknown instance: %v", err)
	}
}

func tagRoundTrip(t *testing.T, e *env) {
	req := e.request(t)
	inst := e.create(t, req)
	got, err := e.d.GetInstance(t.Context(), inst.Ref)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range req.Tags {
		if got.Tags[k] != v {
			t.Errorf("tag %s: got %q, want %q", k, got.Tags[k], v)
		}
	}
	names := map[string]vendors.TagKey{}
	for _, k := range vendors.TagKeys {
		n := e.d.TagKey(k)
		if prev, dup := names[n]; dup {
			t.Errorf("tag keys %s and %s map to %q", prev, k, n)
		}
		names[n] = k
	}
}

func userDataLimit(t *testing.T, e *env) {
	if e.fx.MaxUserData == 0 {
		t.Skip("the fixture sets no user-data limit")
	}
	req := e.request(t)
	req.UserData = []byte(strings.Repeat("x", e.fx.MaxUserData))
	e.create(t, req)
	req = e.request(t)
	req.UserData = []byte(strings.Repeat("x", e.fx.MaxUserData+1))
	if _, err := e.d.CreateInstance(t.Context(), req); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("oversized user-data: %v, want Fatal", err)
	}
}

func errorClassification(t *testing.T, e *env) {
	if e.fx.Inject == nil {
		t.Skip("the fixture has no Inject hook")
	}
	for _, class := range []vendors.Class{
		vendors.InsufficientCapacity, vendors.Unsupported, vendors.QuotaExceeded,
		vendors.ZoneUnusable, vendors.Retryable, vendors.Fatal,
	} {
		req := e.request(t)
		e.fx.Inject(t, e.d, class, req)
		_, err := e.d.CreateInstance(t.Context(), req)
		if got := vendors.ClassOf(err); got != class {
			t.Errorf("an injected %s error was classified %q (%v)", class, got, err)
		}
		// The injection is spent after one call: the next launch works.
		e.create(t, req)
	}
}

func quotaReporter(t *testing.T, e *env) {
	qr, ok := e.d.(vendors.QuotaReporter)
	if !ok {
		t.Skip("the driver has no quota reporter")
	}
	unknown := map[string]bool{}
	for _, u := range e.fx.UnknownQuotaTypes {
		unknown[u] = true
	}
	for _, typ := range e.fx.InstanceTypes {
		for _, capType := range []string{vendors.CapacityOnDemand, vendors.CapacitySpot} {
			cons, known := qr.Consumption(typ, capType)
			switch {
			case known && len(cons) == 0:
				t.Errorf("%s/%s: known consumption without buckets", typ, capType)
			case known && unknown[typ]:
				t.Errorf("%s/%s is listed as unknown but maps to buckets", typ, capType)
			case !known && !unknown[typ]:
				t.Errorf("%s/%s maps to no bucket and is not on the unknown list", typ, capType)
			}
			for _, c := range cons {
				if c.Amount <= 0 || c.Bucket == "" {
					t.Errorf("%s/%s: invalid consumption %+v", typ, capType, c)
				}
			}
		}
	}

	req := e.request(t)
	cons, known := qr.Consumption(req.InstanceType, req.CapacityType)
	if !known {
		t.Skip("the fixture's launch type has unknown quota")
	}
	before := usage(t, qr, e.region, cons)
	inst := e.create(t, req)
	snap, err := qr.Quotas(t.Context(), e.region)
	if err != nil {
		t.Fatal(err)
	}
	after := usage(t, qr, e.region, cons)
	for _, c := range cons {
		if after[c.Bucket]-before[c.Bucket] != c.Amount {
			t.Errorf("bucket %s: used went from %d to %d, want +%d", c.Bucket, before[c.Bucket], after[c.Bucket], c.Amount)
		}
	}
	if !containsRef(snap.Counted, inst.Ref) {
		// A global bucket is counted in GlobalQuotas.
		g, err := qr.GlobalQuotas(t.Context())
		if err != nil || !containsRef(g.Counted, inst.Ref) {
			t.Errorf("the launched instance %s is not in Counted", inst.Ref)
		}
	}

	exhaust := e.request(t)
	if e.fx.ExhaustQuota == nil {
		return
	}
	e.fx.ExhaustQuota(t, e.d, exhaust)
	_, err = e.d.CreateInstance(t.Context(), exhaust)
	if !vendors.IsClass(err, vendors.QuotaExceeded) {
		t.Errorf("a launch without quota: %v, want QuotaExceeded (not InsufficientCapacity)", err)
	}
}

// usage returns the used amount of the buckets, from the region's or the
// global snapshot.
func usage(t *testing.T, qr vendors.QuotaReporter, region string, cons []vendors.Consumption) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	r, err := qr.Quotas(t.Context(), region)
	if err != nil {
		t.Fatal(err)
	}
	g, err := qr.GlobalQuotas(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snaps := []*vendors.QuotaSnapshot{r, g}
	for _, c := range cons {
		found := false
		for _, s := range snaps {
			for _, b := range s.Buckets {
				if b.ID == c.Bucket {
					out[c.Bucket], found = b.Used, true
				}
			}
		}
		if !found {
			t.Errorf("bucket %s is consumed but not reported", c.Bucket)
		}
	}
	return out
}

func offeringLister(t *testing.T, e *env) {
	ol, ok := e.d.(vendors.OfferingLister)
	if !ok {
		t.Skip("the driver has no offering lister")
	}
	types, err := ol.LaunchableTypes(t.Context(), e.region)
	if err != nil {
		t.Fatal(err)
	}
	if len(types) == 0 {
		t.Fatal("no launchable types")
	}
	zones, err := e.d.Zones(t.Context(), e.region)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, z := range zones {
		ids[z.ID] = true
	}
	for typ, zs := range types {
		for _, z := range zs {
			if !ids[z] {
				t.Errorf("%s is launchable in %s, which is not a zone ID of %s", typ, z, e.region)
			}
		}
	}
	if zs := types[e.fx.InstanceType]; len(zs) == 0 {
		t.Errorf("the fixture's type %s is not launchable anywhere in %s", e.fx.InstanceType, e.region)
	}
}

func cloneTags(in map[vendors.TagKey]string) map[vendors.TagKey]string {
	return maps.Clone(in)
}

func hasRef(list []*vendors.Instance, ref vendors.InstanceRef) bool {
	for _, i := range list {
		if i.Ref == ref {
			return true
		}
	}
	return false
}

func containsRef(list []vendors.InstanceRef, ref vendors.InstanceRef) bool {
	return slices.Contains(list, ref)
}

func refs(list []*vendors.Instance) []vendors.InstanceRef {
	out := make([]vendors.InstanceRef, 0, len(list))
	for _, i := range list {
		out = append(out, i.Ref)
	}
	return out
}
