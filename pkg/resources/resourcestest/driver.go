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
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
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

	mu      sync.Mutex
	managed map[string]*record
	users   map[string]int
	seq     int
	// DeleteErr makes the deletion of a resource ID fail while set.
	DeleteErr map[string]error
	// Calls records "ensure:created:<id>", "ensure:found:<id>" and "delete:<id>".
	Calls []string
}

// New returns a driver serving the vendor ID.
func New(vendor string, clk clock.Clock) *Driver {
	return &Driver{
		VendorID: vendor, Clock: clk, managed: map[string]*record{}, users: map[string]int{}, DeleteErr: map[string]error{},
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
		{Kind: SecurityGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleRetain, DeleteOrder: 25},
		{Kind: Subnets, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30},
	}
}

// Resolve implements vendors.ManagedResources.
func (d *Driver) Resolve(
	_ context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	var out []vendors.ResourceRef
	for _, e := range d.Existing {
		if e.Kind != kind || e.Region != region {
			continue
		}
		ok := false
		switch {
		case ref.IsDefault():
			ok = e.Default
		case len(ref.IDs) > 0:
			ok = slices.Contains(ref.IDs, e.ID)
		case ref.NamePattern != "":
			ok, _ = path.Match(ref.NamePattern, e.Name)
		default:
			ok = true
			for k, v := range ref.Tags {
				ok = ok && e.Tags[k] == v
			}
		}
		if ok {
			out = append(out, vendors.ResourceRef{Kind: kind, Region: region, ID: e.ID, Name: e.Name})
		}
	}
	return out, nil
}

// Ensure implements vendors.ManagedResources.
func (d *Driver) Ensure(_ context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if d.EnsureHook != nil {
		d.EnsureHook()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
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

var _ vendors.ManagedResources = (*Driver)(nil)
