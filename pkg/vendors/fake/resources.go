//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// keyResources is the key of the resource lists in the ConfigMaps.
const keyResources = "resources"

// The resource kinds of the fake. They are named like the AWS block's resource
// keys (the fake serves that vendor ID), so a NodeClass configures them in
// spec.vendors.aws.resources: subnets stands for the regional network (shared
// infrastructure, idleTTL), placementGroups for the per-run group (perUse), and
// volumes for the per-node separate volumes (perUse; always created, not
// configurable, like every storage profile's volumes).
const (
	KindNetwork vendors.ResourceKind = "subnets"
	KindGroup   vendors.ResourceKind = "placementGroups"
	KindVolume  vendors.ResourceKind = "volumes"
)

// Kinds implements vendors.ManagedResources. Deletion order: volumes, then
// groups, then the network.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindVolume, Scope: vendors.ScopeNode, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 10},
		{Kind: KindGroup, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: KindNetwork, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30},
	}
}

// resourceRecord is a resource of the fake account. Managed ones are persisted
// in the resources ConfigMap, so that they survive a restart of the controller.
type resourceRecord struct {
	Kind      string            `json:"kind"`
	Region    string            `json:"region"`
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Managed   bool              `json:"managed"`
	Tags      map[string]string `json:"tags,omitempty"`
	Params    string            `json:"params,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	LastUsed  time.Time         `json:"lastUsed"`
}

// existingResource is a resource the account had before SCarpenter (the
// steering ConfigMap's existingResources): what `use` references and the
// vendor's default resources resolve to.
type existingResource struct {
	Kind   string            `json:"kind"`
	Region string            `json:"region"`
	ID     string            `json:"id"`
	Name   string            `json:"name,omitempty"`
	Tags   map[string]string `json:"tags,omitempty"`
	// Default marks the vendor's own default resource of the kind.
	Default bool `json:"default,omitempty"`
}

func (r *resourceRecord) ref() vendors.ResourceRef {
	tags := map[vendors.TagKey]string{}
	for k, v := range r.Tags {
		tags[vendors.TagKey(k)] = v
	}
	return vendors.ResourceRef{
		Kind: vendors.ResourceKind(r.Kind), Region: r.Region, ID: r.ID, Name: r.Name, Managed: r.Managed, Tags: tags,
		CreatedAt: r.CreatedAt,
	}
}

// loadResources reads the persisted resources once.
func (d *Driver) loadResources(ctx context.Context) error {
	if d.resLoaded {
		return nil
	}
	if d.cfg.Client == nil {
		d.resLoaded = true
		return nil
	}
	reader := d.cfg.Reader
	if reader == nil {
		reader = d.cfg.Client
	}
	cm := &corev1.ConfigMap{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: d.opts.ConfigNamespace, Name: d.opts.ResourcesName}, cm)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return vendors.NewError(vendors.Retryable, "ResourcesUnavailable", fmt.Errorf("reading the fake resources: %w", err))
	default:
		var list []*resourceRecord
		if raw := cm.Data[keyResources]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &list); err != nil {
				return vendors.NewError(vendors.Fatal, "ResourcesCorrupt", fmt.Errorf("decoding the fake resources: %w", err))
			}
		}
		for _, r := range list {
			d.resources[r.ID] = r
		}
	}
	d.resLoaded = true
	return nil
}

// saveResources writes the managed resources (called with resMu held).
func (d *Driver) saveResources(ctx context.Context) error {
	if d.cfg.Client == nil {
		return nil
	}
	var list []*resourceRecord
	for _, r := range d.resources {
		if r.Managed {
			list = append(list, r)
		}
	}
	slices.SortFunc(list, func(a, b *resourceRecord) int { return strings.Compare(a.ID, b.ID) })
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	key := types.NamespacedName{Namespace: d.opts.ConfigNamespace, Name: d.opts.ResourcesName}
	reader := d.cfg.Reader
	if reader == nil {
		reader = d.cfg.Client
	}
	var last error
	for range 5 {
		cm := &corev1.ConfigMap{}
		err := reader.Get(ctx, key, cm)
		switch {
		case apierrors.IsNotFound(err):
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
				Data:       map[string]string{keyResources: string(raw)},
			}
			err = d.cfg.Client.Create(ctx, cm)
		case err == nil:
			cm.Data = map[string]string{keyResources: string(raw)}
			err = d.cfg.Client.Update(ctx, cm)
		}
		if err == nil {
			return nil
		}
		last = err
	}
	return vendors.NewError(vendors.Retryable, "ResourcesUnavailable", fmt.Errorf("writing the fake resources: %w", last))
}

func recordID(kind vendors.ResourceKind, name string, at time.Time) string {
	sum := sha256.Sum256([]byte(name + at.UTC().Format(time.RFC3339Nano)))
	return "fake-" + strings.ToLower(string(kind)) + "-" + hex.EncodeToString(sum[:])[:8]
}

func tagStrings(in map[vendors.TagKey]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

// Resolve implements vendors.ManagedResources: the account's existing
// resources (steering), never the managed ones.
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	d.refreshConfig(ctx)
	var out []vendors.ResourceRef
	for _, e := range d.current().existing {
		if e.Kind != string(kind) || e.Region != region {
			continue
		}
		switch {
		case ref.IsDefault():
			if !e.Default {
				continue
			}
		case len(ref.IDs) > 0:
			if !slices.Contains(ref.IDs, e.ID) {
				continue
			}
		case ref.NamePattern != "":
			if ok, _ := path.Match(ref.NamePattern, e.Name); !ok {
				continue
			}
		default:
			matches := true
			for k, v := range ref.Tags {
				if e.Tags[k] != v {
					matches = false
				}
			}
			if !matches {
				continue
			}
		}
		out = append(out, vendors.ResourceRef{Kind: kind, Region: region, ID: e.ID, Name: e.Name})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// Ensure implements vendors.ManagedResources: find by name and ownership
// marker, else create; the resource is persisted.
func (d *Driver) Ensure(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if !slices.ContainsFunc(d.Kinds(), func(k vendors.KindSpec) bool { return k.Kind == kind }) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "unknown resource kind %q", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	d.resMu.Lock()
	defer d.resMu.Unlock()
	if err := d.loadResources(ctx); err != nil {
		return vendors.ResourceRef{}, false, err
	}
	for _, r := range d.resources {
		if r.Managed && r.Kind == string(kind) && r.Region == region && r.Name == spec.Name {
			if r.Tags[string(vendors.TagCluster)] != spec.Tags[vendors.TagCluster] ||
				r.Tags[string(vendors.TagManagedBy)] != spec.Tags[vendors.TagManagedBy] {
				return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
					"a resource named %s exists without the ownership marker", spec.Name)
			}
			d.recordResource("ensure", r, "found")
			return r.ref(), false, nil
		}
	}
	now := d.clock.Now()
	r := &resourceRecord{
		Kind: string(kind), Region: region, ID: recordID(kind, spec.Name, now), Name: spec.Name, Managed: true,
		Tags: tagStrings(spec.Tags), Params: string(spec.Params), CreatedAt: now, LastUsed: now,
	}
	maps.Copy(r.Tags, spec.ExtraTags)
	d.resources[r.ID] = r
	if err := d.saveResources(ctx); err != nil {
		delete(d.resources, r.ID)
		return vendors.ResourceRef{}, false, err
	}
	d.recordResource("ensure", r, "created")
	return r.ref(), true, nil
}

// usersLocked counts the live instances that use the resource and returns the
// latest time one of them ended.
func (d *Driver) usersLocked(id string) int {
	n := 0
	for _, i := range d.instances {
		if !i.Gone() && slices.Contains(i.resources, id) {
			n++
		}
	}
	return n
}

// InUse implements vendors.ManagedResources.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	d.resMu.Lock()
	defer d.resMu.Unlock()
	if err := d.loadResources(ctx); err != nil {
		return false, time.Time{}, err
	}
	r, ok := d.resources[ref.ID]
	if !ok {
		return false, time.Time{}, vendors.Errorf(vendors.NotFound, "resource %s does not exist", ref.ID)
	}
	d.mu.Lock()
	users := d.usersLocked(ref.ID)
	d.mu.Unlock()
	if users > 0 {
		return true, d.clock.Now(), nil
	}
	return false, r.LastUsed, nil
}

// Delete implements vendors.ManagedResources: a resource that an instance still
// uses is refused, as a vendor does (Retryable); one SCarpenter did not create
// is refused (Fatal); a missing one is not an error.
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	d.resMu.Lock()
	defer d.resMu.Unlock()
	if err := d.loadResources(ctx); err != nil {
		return err
	}
	r, ok := d.resources[ref.ID]
	if !ok {
		for _, e := range d.current().existing {
			if e.ID == ref.ID {
				return vendors.Errorf(vendors.Fatal, "resource %s was not created by SCarpenter and is never deleted", ref.ID)
			}
		}
		return nil
	}
	if !r.Managed {
		return vendors.Errorf(vendors.Fatal, "resource %s was not created by SCarpenter and is never deleted", ref.ID)
	}
	d.mu.Lock()
	users := d.usersLocked(ref.ID)
	d.mu.Unlock()
	if users > 0 {
		d.recordResource("delete", r, "in-use")
		return vendors.NewError(vendors.Retryable, "ResourceInUse",
			fmt.Errorf("the %s %s is used by %d instances", r.Kind, r.ID, users))
	}
	delete(d.resources, r.ID)
	if err := d.saveResources(ctx); err != nil {
		d.resources[r.ID] = r
		return err
	}
	d.recordResource("delete", r, "ok")
	return nil
}

// List implements vendors.ManagedResources.
func (d *Driver) List(ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	d.resMu.Lock()
	defer d.resMu.Unlock()
	if err := d.loadResources(ctx); err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, r := range d.resources {
		if !r.Managed || (len(regions) > 0 && !slices.Contains(regions, r.Region)) {
			continue
		}
		ok := true
		for k, v := range tags {
			if r.Tags[string(k)] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, r.ref())
		}
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// touchResources records that the instance used (or stopped using) the
// resources now: the last use the garbage collector counts from.
func (d *Driver) touchResources(ids []string) {
	if len(ids) == 0 {
		return
	}
	d.resMu.Lock()
	defer d.resMu.Unlock()
	now := d.clock.Now()
	changed := false
	for _, id := range ids {
		if r, ok := d.resources[id]; ok {
			r.LastUsed, changed = now, changed || r.Managed
		}
	}
	if changed {
		// Best effort: a restart then still knows when the resource was used.
		if err := d.saveResources(context.Background()); err != nil {
			d.cfg.Log.Error(err, "persisting the last use of the fake resources")
		}
	}
}

// resourceSnapshot returns the records for the state ConfigMap.
func (d *Driver) resourceSnapshot() []resourceRecord {
	d.resMu.Lock()
	defer d.resMu.Unlock()
	out := make([]resourceRecord, 0, len(d.resources))
	for _, r := range d.resources {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b resourceRecord) int { return strings.Compare(a.ID, b.ID) })
	return out
}

var _ vendors.ManagedResources = (*Driver)(nil)
