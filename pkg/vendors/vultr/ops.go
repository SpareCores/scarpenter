/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vultr/govultr/v3"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Defaults of Ops.
const (
	// DefaultAvailabilityTTL is how long the stock of a region is cached; Vultr's
	// stock moves faster than AWS's, so it is short (plan 20, section 5).
	DefaultAvailabilityTTL = 5 * time.Minute
	// DefaultPlansTTL is how long the plan list is cached.
	DefaultPlansTTL = time.Hour
	// RecentCreateTTL is how long a created instance is remembered by its
	// idempotency key, closing the window in which the list does not show it yet.
	RecentCreateTTL = 10 * time.Minute

	perPage  = 500
	maxPages = 200
)

// Ops are the operations of the driver on top of API: everything the plan
// specifies about launching, finding, listing and deleting instances and about
// snapshots and stock, without the vendors.Driver surface.
// TODO(plan 14): the Driver wraps Ops (Init builds the client from
// VULTR_API_KEY, the CreateRequest carries Settings and Prepared).
type Ops struct {
	API     API
	Cluster string
	Tags    TagStyle
	Clock   clock.Clock

	AvailabilityTTL time.Duration
	PlansTTL        time.Duration

	mu     sync.Mutex
	avail  map[string]availability
	plans  map[string]int // plan ID to disk GB
	plansT time.Time
	recent map[string]recentCreate
}

type availability struct {
	plans map[string]struct{}
	at    time.Time
}

type recentCreate struct {
	inst *vendors.Instance
	at   time.Time
}

// NewOps returns Ops for a cluster.
func NewOps(api API, cluster string) *Ops {
	return &Ops{
		API: api, Cluster: cluster, Clock: clock.RealClock{},
		avail: map[string]availability{}, recent: map[string]recentCreate{},
	}
}

func (o *Ops) ttl(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// listAll pages through a list call until the cursor ends.
func listAll[T any](ctx context.Context, op operation, base govultr.ListOptions,
	fetch func(context.Context, *govultr.ListOptions) ([]T, *govultr.Meta, error)) ([]T, error) {
	var all []T
	opts := base
	opts.PerPage = perPage
	for range maxPages {
		items, meta, err := fetch(ctx, &opts)
		if err != nil {
			return nil, classify(err, op)
		}
		all = append(all, items...)
		if meta == nil || meta.Links == nil || meta.Links.Next == "" || meta.Links.Next == opts.Cursor {
			return all, nil
		}
		opts.Cursor = meta.Links.Next
	}
	return nil, vendors.Errorf(vendors.Fatal, "the list does not end after %d pages", maxPages)
}

// Regions lists the regions of the API, restricted to allowed (empty: all).
func (o *Ops) Regions(ctx context.Context, allowed []string) ([]vendors.Region, error) {
	listed, err := listAll(ctx, opOther, govultr.ListOptions{}, o.API.ListRegions)
	if err != nil {
		return nil, err
	}
	return RegionsOf(listed, allowed), nil
}

// instanceListOptions is the one filtered, paginated call of ListInstances: one
// server-side tag (the filter takes one value) and the region when exactly one
// is asked for; the rest is client-side.
func (o *Ops) instanceListOptions(regions []string, tags map[vendors.TagKey]string) govultr.ListOptions {
	var opts govultr.ListOptions
	for _, k := range vendors.TagKeys {
		if v, ok := tags[k]; ok {
			opts.Tag = o.Tags.Tag(k, v)
			break
		}
	}
	if len(regions) == 1 {
		opts.Region = regions[0]
	}
	return opts
}

// ListInstances returns the instances in the regions (empty: all) that carry
// all the tags, oldest first. A suspended instance is returned as terminating.
func (o *Ops) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	raw, err := listAll(ctx, opOther, o.instanceListOptions(regions, tags), o.API.ListInstances)
	if err != nil {
		return nil, err
	}
	var out []*vendors.Instance
	for _, r := range raw {
		inst := ToInstance(r, o.Tags)
		if len(regions) > 0 && !slices.Contains(regions, inst.Region) {
			continue
		}
		if hasTags(inst, tags) {
			out = append(out, inst)
		}
	}
	slices.SortStableFunc(out, func(a, b *vendors.Instance) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.Ref), string(b.Ref))
	})
	return out, nil
}

// FindInstance is ListInstances narrowed to one hit, the oldest; NotFound when
// there is none.
func (o *Ops) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	var regions []string
	if region != "" {
		regions = []string{region}
	}
	list, err := o.ListInstances(ctx, regions, tags)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no instance with these tags")
	}
	return list[0], nil
}

// GetInstance reads an instance by reference. A deleted instance disappears
// (there is no terminated state), so this never returns a terminated one; a
// reference that does not parse is NotFound.
func (o *Ops) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	if ref.ID() == "" {
		return nil, vendors.NewError(vendors.NotFound, codeNotFound, fmt.Errorf("instance reference %q has no ID", ref))
	}
	in, err := o.API.GetInstance(ctx, ref.ID())
	if err != nil {
		return nil, classify(err, opGet)
	}
	return ToInstance(*in, o.Tags), nil
}

// DeleteInstance deletes an instance that carries this cluster's tag. A gone
// instance (404, or an unparseable reference) is success; one of another
// cluster is refused; a refusal because the instance is still installing is
// Retryable (unverified, spike S4).
func (o *Ops) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	if ref.ID() == "" {
		return nil
	}
	in, err := o.API.GetInstance(ctx, ref.ID())
	if err != nil {
		e := classify(err, opGet)
		if vendors.IsClass(e, vendors.NotFound) {
			return nil
		}
		return e
	}
	if got := ToInstance(*in, o.Tags).Tags[vendors.TagCluster]; got != o.Cluster || o.Cluster == "" {
		return vendors.NewError(vendors.Fatal, codeNotOurs,
			fmt.Errorf("instance %s does not carry the tag of cluster %q: not deleted", ref.ID(), o.Cluster))
	}
	if err := o.API.DeleteInstance(ctx, ref.ID()); err != nil {
		if e := classify(err, opDelete); !vendors.IsClass(e, vendors.NotFound) {
			return e
		}
	}
	return nil
}

// Launch creates an instance idempotently. Vultr has no create token, so the
// NodeClaim UID is a tag and the create is preceded by a lookup: a hit is
// returned whatever region, plan or capacity type the replay asks for (the
// driver contract). A create whose call failed after the server accepted it is
// found by the same lookup on the retry, once the list shows it; the cache of
// the last RecentCreateTTL of creates covers the gap inside one process, and the
// garbage collector's orphan sweep is the backstop.
func (o *Ops) Launch(ctx context.Context, req vendors.CreateRequest, lo LaunchOptions) (*vendors.Instance, error) {
	lo.Tags = o.Tags
	key := req.IdempotencyKey
	if key != "" {
		if inst := o.recentCreate(key); inst != nil {
			return inst, nil
		}
		found, err := o.FindInstance(ctx, "", map[vendors.TagKey]string{vendors.TagIdempotencyKey: key})
		switch {
		case err == nil:
			return found, nil
		case !vendors.IsClass(err, vendors.NotFound):
			return nil, err
		}
	}
	body, err := BuildCreateRequest(req, lo)
	if err != nil {
		return nil, err
	}
	// The stock check comes before the call: it is capacity, which the message
	// of a failed create can only guess at.
	have, err := o.availablePlans(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	if _, ok := have[req.InstanceType]; !ok {
		return nil, planNotAvailable(req.InstanceType, req.Region)
	}
	in, err := o.API.CreateInstance(ctx, body)
	if err != nil {
		return nil, classify(err, opCreateInstance)
	}
	inst := ToInstance(*in, o.Tags)
	if key != "" {
		o.mu.Lock()
		o.recent[key] = recentCreate{inst: inst, at: o.Clock.Now()}
		o.mu.Unlock()
	}
	return inst, nil
}

func (o *Ops) recentCreate(key string) *vendors.Instance {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.Clock.Now()
	for k, r := range o.recent {
		if now.Sub(r.at) > RecentCreateTTL {
			delete(o.recent, k)
		}
	}
	if r, ok := o.recent[key]; ok {
		return r.inst
	}
	return nil
}

// availablePlans is the set of plans in stock in the region, cached for the
// availability TTL.
func (o *Ops) availablePlans(ctx context.Context, region string) (map[string]struct{}, error) {
	o.mu.Lock()
	a, ok := o.avail[region]
	o.mu.Unlock()
	if ok && o.Clock.Since(a.at) < o.ttl(o.AvailabilityTTL, DefaultAvailabilityTTL) {
		return a.plans, nil
	}
	av, err := o.API.RegionAvailability(ctx, region, planTypeAll)
	if err != nil {
		return nil, classify(err, opRegion)
	}
	a = availability{plans: map[string]struct{}{}, at: o.Clock.Now()}
	for _, p := range av.AvailablePlans {
		a.plans[p] = struct{}{}
	}
	o.mu.Lock()
	o.avail[region] = a
	o.mu.Unlock()
	return a.plans, nil
}

func (o *Ops) listPlans(ctx context.Context, opts *govultr.ListOptions) ([]govultr.Plan, *govultr.Meta, error) {
	return o.API.ListPlans(ctx, planTypeAll, opts)
}

// planDisks is the plan list's disk size per plan, cached.
func (o *Ops) planDisks(ctx context.Context) (map[string]int, error) {
	o.mu.Lock()
	p, at := o.plans, o.plansT
	o.mu.Unlock()
	if p != nil && o.Clock.Since(at) < o.ttl(o.PlansTTL, DefaultPlansTTL) {
		return p, nil
	}
	plans, err := listAll(ctx, opOther, govultr.ListOptions{}, o.listPlans)
	if err != nil {
		return nil, err
	}
	p = make(map[string]int, len(plans))
	for _, pl := range plans {
		p[pl.ID] = pl.Disk
	}
	o.mu.Lock()
	o.plans, o.plansT = p, o.Clock.Now()
	o.mu.Unlock()
	return p, nil
}

// LaunchableTypes is the OfferingLister of a region: the plans in stock
// (GET /v2/regions/{id}/availability) that the driver launches (ClassifyPlan),
// sorted.
func (o *Ops) LaunchableTypes(ctx context.Context, region string) ([]string, error) {
	have, err := o.availablePlans(ctx, region)
	if err != nil {
		return nil, err
	}
	disks, err := o.planDisks(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for id := range have {
		if ok, _ := ClassifyPlan(id, disks[id]); ok {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

// SnapshotDescription is the description the images pipeline gives the Flatcar
// snapshot of a release: flatcar-<channel>-<version>-<arch>.
func SnapshotDescription(channel, version, arch string) string {
	return fmt.Sprintf("flatcar-%s-%s-%s", channel, version, arch)
}

// ResolveSnapshot finds the Flatcar snapshot of a release by its description:
// exactly one snapshot with that exact description (the API filter may match a
// substring, so the compare is client-side) and the status complete. Vultr has
// no Arm instances, so arm64 is Unsupported.
// TODO(plan 14): images.use.ids skips the lookup, images.use.name replaces the
// pattern, and the Image carries RootDiskBytes where the snapshot tells.
func (o *Ops) ResolveSnapshot(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if req.Arch != "amd64" {
		return nil, noInstancesOf(req.Arch)
	}
	desc := SnapshotDescription(req.Channel, req.Version, req.Arch)
	all, err := listAll(ctx, opOther, govultr.ListOptions{Description: desc}, o.API.ListSnapshots)
	if err != nil {
		return nil, err
	}
	var exact, complete []govultr.Snapshot
	for _, s := range all {
		if s.Description != desc {
			continue
		}
		exact = append(exact, s)
		if s.Status == "complete" {
			complete = append(complete, s)
		}
	}
	switch {
	case len(complete) == 1:
		return &vendors.Image{Ref: complete[0].ID}, nil
	case len(complete) > 1:
		return nil, vendors.NewError(vendors.Fatal, codeImageAmbiguous,
			fmt.Errorf("%d complete snapshots are described %q; exactly one is required", len(complete), desc))
	case len(exact) > 0:
		return nil, vendors.NewError(vendors.Fatal, codeImageNotFound,
			fmt.Errorf("the snapshot described %q is not complete yet (status %q)", desc, exact[0].Status))
	}
	return nil, vendors.NewError(vendors.Fatal, codeImageNotFound, fmt.Errorf("no snapshot is described %q", desc))
}
