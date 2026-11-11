/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package resourcestest is an in-memory driver with the ManagedResources
// capability for the tests of the resource engine and of what is built on it.
// It models the account's pre-existing resources (use, vendor defaults), the
// managed ones the engine creates, who uses them, and failing deletions.
package resourcestest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

// The kinds of the double, named like the AWS block's resource keys.
const (
	Subnets         vendors.ResourceKind = "subnets"
	SecurityGroups  vendors.ResourceKind = "securityGroups"
	PlacementGroups vendors.ResourceKind = "placementGroups"
)

// Existing is a resource the account had before SCarpenter.
type Existing struct {
	Kind    vendors.ResourceKind
	Region  string
	ID      string
	Name    string
	Tags    map[string]string
	Default bool
	// Network is the network the resource belongs to (a security group's VPC).
	Network string
	// NoIPv6 marks a default that cannot honour a request for IPv6.
	NoIPv6 bool
}

type record struct {
	ref      vendors.ResourceRef
	lastUsed time.Time
}

// Driver is the double. Exported fields may be set before use; the methods are
// safe for concurrent use.
type Driver struct {
	VendorID string
	Clock    clock.Clock
	// Existing are the account's own resources.
	Existing []Existing
	// EnsureHook, when set, runs inside Ensure before the lookup (to hold
	// concurrent callers).
	EnsureHook func()

	mu        sync.Mutex
	kindCalls map[vendors.ResourceKind]int
	managed   map[string]*record
	users     map[string]int
	seq       int
	// DeleteErr makes the deletion of a resource ID fail while set.
	DeleteErr map[string]error
	// Calls records "ensure:created:<id>", "ensure:found:<id>" and "delete:<id>".
	Calls []string
	// GlobalSubnets makes the subnets kind global at the vendor: Resolve of a
	// selector finds the resources of any region (they all list the same).
	GlobalSubnets bool
	// ResolveCalls counts the Resolve calls of selectors; DefaultCalls those of
	// Default.
	ResolveCalls atomic.Int32
	DefaultCalls atomic.Int32
	// Fail makes an operation fail with the error: "resolve/<kind>" or
	// "default/<kind>" (a vendor error, which stops the resolution order).
	Fail map[string]error
	// EnsureErr makes the Ensure of a kind fail.
	EnsureErr map[vendors.ResourceKind]error
	// OptionalSubnets declares the subnets kind optional (KindSpec.Optional).
	OptionalSubnets bool
}

// New returns a driver serving the vendor ID.
func New(vendor string, clk clock.Clock) *Driver {
	return &Driver{
		VendorID: vendor, Clock: clk, kindCalls: map[vendors.ResourceKind]int{}, managed: map[string]*record{},
		users: map[string]int{}, DeleteErr: map[string]error{},
		Fail: map[string]error{}, EnsureErr: map[vendors.ResourceKind]error{},
	}
}

// SetUsers sets how many instances use the resource.
func (d *Driver) SetUsers(id string, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.users[id] = n
	if r := d.managed[id]; r != nil {
		r.lastUsed = d.Clock.Now()
	}
}

// Seed adds a managed resource as if an earlier process had created it.
func (d *Driver) Seed(
	kind vendors.ResourceKind, region, name string, tags map[vendors.TagKey]string, at time.Time,
) vendors.ResourceRef {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	ref := vendors.ResourceRef{
		Kind: kind, Region: region, ID: fmt.Sprintf("res-%03d", d.seq), Name: name, Managed: true,
		Tags: maps.Clone(tags), CreatedAt: at,
	}
	d.managed[ref.ID] = &record{ref: ref, lastUsed: at}
	return ref
}

// ResolveCallsOf counts the Resolve calls for the kind.
func (d *Driver) ResolveCallsOf(kind vendors.ResourceKind) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.kindCalls[kind]
}

// Managed returns the IDs of the managed resources, sorted.
func (d *Driver) Managed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Sorted(maps.Keys(d.managed))
}

// CallLog returns a copy of the recorded calls.
func (d *Driver) CallLog() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.Calls)
}

func (d *Driver) Vendor() string { return d.VendorID }

func (d *Driver) Init(context.Context, vendors.Config) error { return nil }

func (d *Driver) Regions(context.Context) ([]vendors.Region, error) { return nil, nil }

func (d *Driver) Zones(context.Context, string) ([]vendors.Zone, error) { return nil, nil }

func (d *Driver) ResolveImage(context.Context, vendors.ImageRequest) (*vendors.Image, error) {
	return &vendors.Image{}, nil
}

func (d *Driver) CreateInstance(context.Context, vendors.CreateRequest) (*vendors.Instance, error) {
	return nil, vendors.Errorf(vendors.Fatal, "not used")
}

func (d *Driver) GetInstance(context.Context, vendors.InstanceRef) (*vendors.Instance, error) {
	return nil, vendors.Errorf(vendors.NotFound, "not used")
}

func (d *Driver) FindInstance(context.Context, string, map[vendors.TagKey]string) (*vendors.Instance, error) {
	return nil, vendors.Errorf(vendors.NotFound, "not used")
}

func (d *Driver) ListInstances(context.Context, []string, map[vendors.TagKey]string) ([]*vendors.Instance, error) {
	return nil, nil
}

func (d *Driver) DeleteInstance(context.Context, vendors.InstanceRef) error { return nil }

func (d *Driver) TagKey(k vendors.TagKey) string { return vendors.DefaultTagKey(k) }

// Kinds implements vendors.ManagedResources.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: PlacementGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: SecurityGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleRetain, DeleteOrder: 25,
			InNetwork: Subnets},
		{Kind: Subnets, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30,
			Optional: d.OptionalSubnets},
	}
}

// Resolve implements vendors.ManagedResources.
func (d *Driver) Resolve(
	_ context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: the default is Default's")
	}
	d.ResolveCalls.Add(1)
	d.mu.Lock()
	d.kindCalls[kind]++
	d.mu.Unlock()
	if err := d.fail("resolve", kind); err != nil {
		return nil, err
	}
	var cands []Existing
	for _, e := range d.Existing {
		if e.Kind == kind && (e.Region == region || (d.GlobalSubnets && kind == Subnets)) {
			cands = append(cands, e)
		}
	}
	res, err := selector.Apply(ref, d.Selectors()[kind], cands, func(e Existing) selector.Fields {
		attrs := map[string][]string{}
		if e.Network != "" {
			attrs[vendors.MatchNetwork] = []string{e.Network}
		}
		return selector.Fields{ID: e.ID, Name: e.Name, Tags: e.Tags, Attrs: attrs}
	})
	if err != nil {
		return nil, err
	}
	return refsOf(kind, region, res.Items), nil
}

func refsOf(kind vendors.ResourceKind, region string, items []Existing) []vendors.ResourceRef {
	out := make([]vendors.ResourceRef, 0, len(items))
	for _, e := range items {
		out = append(out, vendors.ResourceRef{Kind: kind, Region: region, ID: e.ID, Name: e.Name, Network: e.Network})
	}
	return out
}

// fail returns the error set for the operation on the kind, if any.
func (d *Driver) fail(op string, kind vendors.ResourceKind) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Fail[op+"/"+string(kind)]
}

// Default implements vendors.ResourceDefaults.
func (d *Driver) Default(_ context.Context, kind vendors.ResourceKind, region string) ([]vendors.ResourceRef, error) {
	d.DefaultCalls.Add(1)
	if err := d.fail("default", kind); err != nil {
		return nil, err
	}
	var cands []Existing
	for _, e := range d.Existing {
		if e.Kind == kind && e.Region == region && e.Default {
			cands = append(cands, e)
		}
	}
	return refsOf(kind, region, cands), nil
}

// DefaultSuits implements vendors.DefaultChecker: a default marked NoIPv6
// cannot honour publicIPv6.
func (d *Driver) DefaultSuits(
	_ context.Context, kind vendors.ResourceKind, region string, defaults []vendors.ResourceRef, s vendors.VendorSettings,
) (bool, string, error) {
	if s.Networking.PublicIPv6 == nil || !*s.Networking.PublicIPv6 {
		return true, "", nil
	}
	for _, e := range d.Existing {
		if e.Kind == kind && e.Region == region && e.Default && e.NoIPv6 &&
			slices.ContainsFunc(defaults, func(r vendors.ResourceRef) bool { return r.ID == e.ID }) {
			return false, e.ID + " has no IPv6 range", nil
		}
	}
	return true, "", nil
}

// Selectors implements vendors.ManagedResources: names and tags, evaluated by
// the client, on the kinds a NodeClass can bring.
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	caps := vendors.SelectorCaps{
		Tags: vendors.TagsMap, Name: &vendors.FieldCap{Field: "name", Side: vendors.SideClient},
		DefaultSelect: common.SelectAll,
	}
	inNetwork := caps
	inNetwork.DefaultSelect = common.SelectExactlyOne // a group is one: an ambiguous opt-in fails
	inNetwork.Match = map[string]vendors.FieldCap{vendors.MatchNetwork: {Field: "network", Side: vendors.SideClient}}
	global := caps
	global.Global = d.GlobalSubnets
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		PlacementGroups: caps, SecurityGroups: inNetwork, Subnets: global,
	}
}

// Ensure implements vendors.ManagedResources.
func (d *Driver) Ensure(_ context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if d.EnsureHook != nil {
		d.EnsureHook()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.EnsureErr[kind]; err != nil {
		return vendors.ResourceRef{}, false, err
	}
	for _, r := range d.managed {
		if r.ref.Kind == kind && r.ref.Region == region && r.ref.Name == spec.Name {
			d.Calls = append(d.Calls, "ensure:found:"+r.ref.ID)
			return r.ref, false, nil
		}
	}
	d.seq++
	now := d.Clock.Now()
	tags := maps.Clone(spec.Tags)
	ref := vendors.ResourceRef{
		Kind: kind, Region: region, ID: fmt.Sprintf("res-%03d", d.seq), Name: spec.Name, Managed: true, Tags: tags,
		CreatedAt: now,
	}
	d.managed[ref.ID] = &record{ref: ref, lastUsed: now}
	d.Calls = append(d.Calls, "ensure:created:"+ref.ID)
	return ref, true, nil
}

// InUse implements vendors.ManagedResources.
func (d *Driver) InUse(_ context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.managed[ref.ID]
	if r == nil {
		return false, time.Time{}, vendors.Errorf(vendors.NotFound, "no resource %s", ref.ID)
	}
	if d.users[ref.ID] > 0 {
		return true, d.Clock.Now(), nil
	}
	return false, r.lastUsed, nil
}

// Delete implements vendors.ManagedResources.
func (d *Driver) Delete(_ context.Context, ref vendors.ResourceRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.DeleteErr[ref.ID]; err != nil {
		return err
	}
	if _, ok := d.managed[ref.ID]; !ok {
		return nil
	}
	delete(d.managed, ref.ID)
	d.Calls = append(d.Calls, "delete:"+ref.ID)
	return nil
}

// List implements vendors.ManagedResources.
func (d *Driver) List(
	_ context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []vendors.ResourceRef
	for _, r := range d.managed {
		if len(regions) > 0 && !slices.Contains(regions, r.ref.Region) {
			continue
		}
		ok := true
		for k, v := range tags {
			ok = ok && r.ref.Tags[k] == v
		}
		if ok {
			out = append(out, r.ref)
		}
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

var (
	_ vendors.ManagedResources = (*Driver)(nil)
	_ vendors.ResourceDefaults = (*Driver)(nil)
	_ vendors.DefaultChecker   = (*Driver)(nil)
)
