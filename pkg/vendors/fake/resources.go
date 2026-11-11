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
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
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
	// KindImages is the boot image of a release: not managed (no create, never
	// deleted), only brought with `use` and resolved against the `images` list of
	// the steering ConfigMap.
	KindImages vendors.ResourceKind = "images"
)

// The names of the fields the fake declares.
const (
	fieldState = "state"
	fieldName  = "name"
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
	// Default marks the vendor's own default resource of the kind; NoIPv6 says
	// that such a default cannot honour publicIPv6 (the default network has no
	// IPv6 range), so a NodeClass that asks for it skips the default.
	Default bool `json:"default,omitempty"`
	NoIPv6  bool `json:"noIPv6,omitempty"`
	// The attributes the portable match reads; unset ones are unknown, and
	// State defaults to available.
	Created     *time.Time `json:"created,omitempty"`
	Description string     `json:"description,omitempty"`
	Zone        string     `json:"zone,omitempty"`
	Network     string     `json:"network,omitempty"`
	CIDR        string     `json:"cidr,omitempty"`
	State       string     `json:"state,omitempty"`
	// The attributes of an image (the steering key `images`): the CPU
	// architecture (amd64 or arm64), the owner, whether it is deprecated, and
	// the Flatcar release it says it is (empty: it does not say), which the
	// version check reads, next to the labels flatcar-channel and flatcar-version
	// of Tags.
	Arch       string `json:"arch,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Deprecated bool   `json:"deprecated,omitempty"`
	Channel    string `json:"channel,omitempty"`
	Version    string `json:"version,omitempty"`
}

// The architectures of images, as the platform names them.
const (
	archAMD64 = "amd64"
	archARM64 = "arm64"
)

// SelectorMode says who evaluates the selector in the fake: the "vendor" (the
// fake narrows exactly), a superset (the fake narrows loosely and the shared
// matcher finishes) or only the client (the fake returns everything). The
// conformance cases run the same assertions in every mode.
type SelectorMode int

// Selector modes.
const (
	SelectorServer SelectorMode = iota
	SelectorSuperset
	SelectorClient
)

// SetSelectorMode changes who evaluates the selector (tests).
func (d *Driver) SetSelectorMode(m SelectorMode) { d.selMode.Store(int32(m)) }

func (d *Driver) side() vendors.Side {
	switch SelectorMode(d.selMode.Load()) {
	case SelectorSuperset:
		return vendors.SideServerClient
	case SelectorClient:
		return vendors.SideClient
	}
	return vendors.SideServer
}

// Selectors implements vendors.ManagedResources: the kinds a NodeClass can
// bring. The fake declares everything the matcher can do, with the side of the
// current mode, and the filters state, zone and tag:<key> (always evaluated by
// the fake, like a native filter).
func (d *Driver) Selectors() map[vendors.ResourceKind]vendors.SelectorCaps {
	side := d.side()
	match := func(attrs ...string) map[string]vendors.FieldCap {
		m := map[string]vendors.FieldCap{}
		for _, a := range attrs {
			m[a] = vendors.FieldCap{Field: a, Side: side}
		}
		return m
	}
	filters := []vendors.FilterCap{
		{Name: fieldState, Side: vendors.SideServer}, {Name: "zone", Side: vendors.SideServer},
		{Name: "tag:*", Side: vendors.SideServer},
	}
	return map[vendors.ResourceKind]vendors.SelectorCaps{
		KindNetwork: {
			Zonal: true, HasCreated: true, Tags: vendors.TagsMap, Name: &vendors.FieldCap{Field: fieldName, Side: side},
			Match: match(vendors.MatchZone, vendors.MatchNetwork, vendors.MatchCIDR, vendors.MatchWithin,
				vendors.MatchState, vendors.MatchDescription, vendors.MatchCreatedAfter, vendors.MatchCreatedBefore),
			Filters: filters, DefaultSelect: common.SelectAll,
		},
		KindGroup: {
			HasCreated: true, Tags: vendors.TagsMap, Name: &vendors.FieldCap{Field: fieldName, Side: side},
			Match:   match(vendors.MatchState, vendors.MatchCreatedAfter, vendors.MatchCreatedBefore),
			Filters: filters, DefaultSelect: common.SelectExactlyOne,
		},
		KindImages: {
			HasCreated: true, Tags: vendors.TagsMap, Name: &vendors.FieldCap{Field: fieldName, Side: side},
			Match: match(vendors.MatchArchitecture, vendors.MatchOwner, vendors.MatchState, vendors.MatchIncludeDeprecated,
				vendors.MatchCreatedAfter, vendors.MatchCreatedBefore, vendors.MatchDescription),
			Filters: []vendors.FilterCap{
				{Name: fieldState, Side: vendors.SideServer}, {Name: "tag:*", Side: vendors.SideServer},
			},
			DefaultSelect: common.SelectExactlyOne,
		},
	}
}

// VendorArch implements vendors.ArchNamer (the fake serves the AWS spelling).
func (d *Driver) VendorArch(arch string) (string, bool) {
	switch arch {
	case archAMD64:
		return "x86_64", true
	case archARM64:
		return archARM64, true
	}
	return "", false
}

func (e existingResource) fields() selector.Fields {
	f := selector.Fields{ID: e.ID, Name: e.Name, Tags: e.Tags, Attrs: map[string][]string{}}
	if e.Created != nil {
		f.Created = *e.Created
	}
	state := e.State
	if state == "" {
		state = "available"
	}
	f.Attrs[vendors.MatchState] = []string{state}
	if e.Description != "" {
		f.Attrs[vendors.MatchDescription] = []string{e.Description}
	}
	if e.Zone != "" {
		f.Attrs[vendors.MatchZone] = []string{e.Zone}
	}
	if e.Network != "" {
		f.Attrs[vendors.MatchNetwork] = []string{e.Network}
	}
	if e.CIDR != "" {
		f.Attrs[vendors.MatchCIDR] = []string{e.CIDR}
	}
	if e.Arch != "" {
		f.Attrs[vendors.MatchArchitecture] = []string{e.Arch}
	}
	if e.Owner != "" {
		f.Attrs[vendors.MatchOwner] = []string{e.Owner}
	}
	f.Attrs[vendors.MatchIncludeDeprecated] = []string{strconv.FormatBool(e.Deprecated)}
	return f
}

// nativeFilters applies the fake's own filters, which the fake always
// evaluates: state, zone and tag:<key>.
func nativeFilters(fs []common.ResourceFilter, f selector.Fields) bool {
	for _, fl := range fs {
		var have []string
		switch name := fl.Name; {
		case name == "state" || name == "zone":
			have = f.Attrs[name]
		case strings.HasPrefix(name, "tag:"):
			if v, ok := f.Tags[strings.TrimPrefix(name, "tag:")]; ok {
				have = []string{v}
			}
		}
		if !slices.ContainsFunc(have, func(h string) bool { return slices.Contains(fl.Values, h) }) {
			return false
		}
	}
	return true
}

// narrow is what the fake, as the vendor, does before answering: exactly what
// it is asked (server), a superset of it (a substring of the name, tag keys
// only) or nothing (client).
func narrow(mode SelectorMode, ref vendors.UseRef, caps vendors.SelectorCaps, cands []existingResource,
) []existingResource {
	switch mode {
	case SelectorClient:
		return cands
	case SelectorSuperset:
		loose := vendors.UseRef{Tags: map[string]string{}, Select: common.SelectAll}
		for k := range ref.Tags {
			loose.Tags[k] = "*"
		}
		if lit := selector.LongestLiteral(ref.Name); ref.Name != "" && lit != "" {
			cands = slices.DeleteFunc(slices.Clone(cands), func(e existingResource) bool {
				return !strings.Contains(e.Name, lit)
			})
		}
		if len(loose.Tags) == 0 {
			return cands
		}
		res, _ := selector.Apply(loose, caps, cands, existingResource.fields)
		return res.Items
	}
	exact := ref
	exact.Filters = nil
	exact.Select, exact.OrderBy = common.SelectAll, "" // a vendor lists, it does not choose
	res, _ := selector.Apply(exact, caps, cands, existingResource.fields)
	return res.Items
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
// resources (steering), never the managed ones, selected as a vendor would:
// narrowed by the fake (per the selector mode) and finished by the shared
// matcher. An empty selector selects nothing (the defaults are Default's).
func (d *Driver) Resolve(ctx context.Context, kind vendors.ResourceKind, region string, ref vendors.UseRef,
) ([]vendors.ResourceRef, error) {
	if ref.IsDefault() {
		return nil, vendors.Errorf(vendors.Fatal, "an empty selector selects nothing: the vendor's default is Default's")
	}
	d.refreshConfig(ctx)
	var cands []existingResource
	source := d.current().existing
	if kind == KindImages {
		source = d.current().images
	}
	for _, e := range source {
		global := kind == KindImages && e.Region == selector.RegionAll
		if e.Kind == string(kind) && (e.Region == region || global) {
			cands = append(cands, e)
		}
	}
	caps := d.Selectors()[kind]
	cands = slices.DeleteFunc(cands, func(e existingResource) bool { return !nativeFilters(ref.Filters, e.fields()) })
	cands = narrow(SelectorMode(d.selMode.Load()), ref, caps, cands)
	res, err := selector.Apply(ref, caps, cands, existingResource.fields)
	if err != nil {
		return nil, err
	}
	return existingRefs(kind, region, res.Items), nil
}

func existingRefs(kind vendors.ResourceKind, region string, items []existingResource) []vendors.ResourceRef {
	out := make([]vendors.ResourceRef, 0, len(items))
	for _, e := range items {
		out = append(out, vendors.ResourceRef{Kind: kind, Region: region, ID: e.ID, Name: e.Name, Network: e.Network})
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Default implements vendors.ResourceDefaults: the account's resources the
// steering marks as the vendor's default of the kind.
func (d *Driver) Default(ctx context.Context, kind vendors.ResourceKind, region string) ([]vendors.ResourceRef, error) {
	d.refreshConfig(ctx)
	var cands []existingResource
	for _, e := range d.current().existing {
		if e.Kind == string(kind) && e.Region == region && e.Default {
			cands = append(cands, e)
		}
	}
	return existingRefs(kind, region, cands), nil
}

// DefaultSuits implements vendors.DefaultChecker: a default marked noIPv6
// cannot honour publicIPv6.
func (d *Driver) DefaultSuits(
	ctx context.Context, kind vendors.ResourceKind, region string, defaults []vendors.ResourceRef,
	s vendors.VendorSettings,
) (bool, string, error) {
	if s.Networking.PublicIPv6 == nil || !*s.Networking.PublicIPv6 {
		return true, "", nil
	}
	d.refreshConfig(ctx)
	for _, e := range d.current().existing {
		if e.Kind == string(kind) && e.Region == region && e.Default && e.NoIPv6 &&
			slices.ContainsFunc(defaults, func(r vendors.ResourceRef) bool { return r.ID == e.ID }) {
			return false, fmt.Sprintf("%s has no IPv6 range and publicIPv6 is on", e.ID), nil
		}
	}
	return true, "", nil
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

// usersLocked counts the instances that use the resource: every one that is not
// terminated, a powered-off (terminating) one included, which still holds it.
func (d *Driver) usersLocked(id string) int {
	n := 0
	for _, i := range d.instances {
		if i.State != vendors.StateTerminated && slices.Contains(i.resources, id) {
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

var (
	_ vendors.ManagedResources = (*Driver)(nil)
	_ vendors.ResourceDefaults = (*Driver)(nil)
	_ vendors.DefaultChecker   = (*Driver)(nil)
)
