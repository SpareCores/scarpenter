/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"context"
	"fmt"
	"maps"
	"regexp"
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
// stock, without the vendors.Driver surface, which wraps them (driver.go).
type Ops struct {
	API     API
	Cluster string
	Tags    TagStyle
	Clock   clock.Clock

	AvailabilityTTL time.Duration
	PlansTTL        time.Duration
	// RegistrationTimeout is the operator's registration window; bare-metal plans
	// are listed only when it is long enough (checkBareMetalWindow).
	RegistrationTimeout time.Duration

	mu    sync.Mutex
	avail map[string]availability
	plans map[string]planInfo

	plansT time.Time
	recent map[string]recentCreate
}

// planInfo is what the plan list says of a plan that the driver decides by.
type planInfo struct {
	disk int    // GB
	typ  string // Vultr's plan type: vc2, vdm, ...
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

// ListInstances returns the instances and bare-metal servers in the regions
// (empty: all) that carry all the tags, oldest first. A suspended or stopped
// one is returned as terminating.
func (o *Ops) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	opts := o.instanceListOptions(regions, tags)
	raw, err := listAll(ctx, opOther, opts, o.API.ListInstances)
	if err != nil {
		return nil, err
	}
	// The bare-metal endpoint is read only while bare metal is enabled (the
	// registration window is open), so that it cannot break the instance paths
	// of a deployment that never uses it. Once enabled, its errors are returned:
	// a server missing from the list would let a replayed launch create a second
	// one.
	var servers []govultr.BareMetalServer
	if bareMetalWindowOK(o.RegistrationTimeout) {
		if servers, err = listAll(ctx, opOther, opts, o.API.ListBareMetals); err != nil {
			return nil, err
		}
	}
	all := make([]*vendors.Instance, 0, len(raw)+len(servers))
	for _, r := range raw {
		all = append(all, ToInstance(r, o.Tags))
	}
	for _, s := range servers {
		all = append(all, ToBareMetalInstance(s, o.Tags))
	}
	var out []*vendors.Instance
	for _, inst := range all {
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

// instanceID is the shape of a Vultr instance ID, a UUID.
var instanceID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isInstanceID reports whether id can be a Vultr instance ID. Anything else is
// answered locally (plan 14, A9): what the API answers for a malformed ID (404
// or a 400 that would classify Fatal) is unverified.
func isInstanceID(id string) bool { return instanceID.MatchString(id) }

// GetInstance reads an instance or bare-metal server by reference. A deleted
// one disappears (there is no terminated state), so this never returns a
// terminated one; a reference without a UUID is NotFound without a call.
func (o *Ops) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	id, bareMetal := splitRef(ref)
	if !isInstanceID(id) {
		return nil, vendors.NewError(vendors.NotFound, codeNotFound,
			fmt.Errorf("instance reference %q has no instance UUID", ref))
	}
	if bareMetal {
		s, err := o.API.GetBareMetal(ctx, id)
		if err != nil {
			return nil, classify(err, opGet)
		}
		return ToBareMetalInstance(*s, o.Tags), nil
	}
	in, err := o.API.GetInstance(ctx, id)
	if err != nil {
		return nil, classify(err, opGet)
	}
	return ToInstance(*in, o.Tags), nil
}

// DeleteInstance deletes an instance or bare-metal server that carries this
// cluster's tag. A gone one (404, or an unparseable reference) is success; one of
// another cluster is refused; a refusal because the instance is still installing
// is Retryable (unverified, spike S4). A bare-metal server is halted first, see
// deleteBareMetal.
func (o *Ops) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	id, bareMetal := splitRef(ref)
	if !isInstanceID(id) {
		return nil
	}
	inst, err := o.GetInstance(ctx, ref)
	if err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			return nil
		}
		return err
	}
	if got := inst.Tags[vendors.TagCluster]; got != o.Cluster || o.Cluster == "" {
		return vendors.NewError(vendors.Fatal, codeNotOurs,
			fmt.Errorf("instance %s does not carry the tag of cluster %q: not deleted", id, o.Cluster))
	}
	if bareMetal {
		return o.deleteBareMetal(ctx, id)
	}
	if err := o.API.DeleteInstance(ctx, id); err != nil {
		if e := classify(err, opDelete); !vendors.IsClass(e, vendors.NotFound) {
			return e
		}
	}
	return nil
}

// deleteBareMetal halts the server and then deletes it: a deleted bare-metal
// server keeps running for minutes (observed: about 4.5), and its kubelet
// registers the Node again. The delete is the cost-safety action, so it is sent
// whatever the halt answered, unless the halt says the server is gone (404). A
// failed halt is returned only when the delete fails too, as the error to retry
// on.
func (o *Ops) deleteBareMetal(ctx context.Context, id string) error {
	var haltErr error
	if err := o.API.HaltBareMetal(ctx, id); err != nil {
		haltErr = classify(err, opDelete)
		if vendors.IsClass(haltErr, vendors.NotFound) {
			return nil
		}
	}
	if err := o.API.DeleteBareMetal(ctx, id); err != nil {
		e := classify(err, opDelete)
		if vendors.IsClass(e, vendors.NotFound) {
			return nil
		}
		if haltErr != nil {
			return haltErr
		}
		return e
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
			// The replay is rare, so the cache is confirmed: an instance deleted
			// since (the garbage collector, an operator) must not be handed back.
			if cur, err := o.GetInstance(ctx, inst.Ref); err == nil && cur.State != vendors.StateTerminating {
				return cur, nil
			} else if err != nil && !vendors.IsClass(err, vendors.NotFound) {
				return nil, err
			}
			o.forgetCreate(key)
		}
		found, err := o.FindInstance(ctx, "", map[vendors.TagKey]string{vendors.TagIdempotencyKey: key})
		switch {
		case err == nil:
			return found, nil
		case !vendors.IsClass(err, vendors.NotFound):
			return nil, err
		}
	}
	var inst *vendors.Instance
	var err error
	if isBareMetalPlan(req.InstanceType) {
		inst, err = o.createBareMetal(ctx, req, lo)
	} else {
		inst, err = o.createInstance(ctx, req, lo)
	}
	if err != nil {
		return nil, err
	}
	if key != "" {
		o.mu.Lock()
		o.recent[key] = recentCreate{inst: inst, at: o.Clock.Now()}
		o.mu.Unlock()
	}
	return inst, nil
}

// checkStock fails when the plan is not in stock in the region. The check comes
// before the create call: it is capacity, which the message of a failed create
// can only guess at.
func (o *Ops) checkStock(ctx context.Context, region, plan, planType string) error {
	have, err := o.availablePlans(ctx, region, planType)
	if err != nil {
		return err
	}
	if _, ok := have[plan]; !ok {
		return planNotAvailable(plan, region)
	}
	return nil
}

func (o *Ops) createInstance(
	ctx context.Context, req vendors.CreateRequest, lo LaunchOptions,
) (*vendors.Instance, error) {
	body, err := BuildCreateRequest(req, lo)
	if err != nil {
		return nil, err
	}
	if err := o.checkStock(ctx, req.Region, req.InstanceType, planTypeAll); err != nil {
		return nil, err
	}
	in, err := o.API.CreateInstance(ctx, body)
	if err != nil {
		return nil, classify(err, opCreateInstance)
	}
	return ToInstance(*in, o.Tags), nil
}

// createBareMetal creates the server. It has no firewall group and no VPC, so
// the node must carry the host firewall (Prepare's boot fragment) in its user
// data: a launch without it is refused, so that a server is never created open.
func (o *Ops) createBareMetal(
	ctx context.Context, req vendors.CreateRequest, lo LaunchOptions,
) (*vendors.Instance, error) {
	if !hasHostFirewall(req.Prepared) {
		return nil, vendors.NewError(vendors.Unsupported, codeIncompatible, fmt.Errorf(
			"bare metal plan %s needs the %s boot fragment from Prepare: a server cannot join a firewall group",
			req.InstanceType, HostFirewallFragmentName))
	}
	body, err := BuildBareMetalCreateRequest(req, lo)
	if err != nil {
		return nil, err
	}
	if err := o.checkStock(ctx, req.Region, req.InstanceType, planTypeBareMetal); err != nil {
		return nil, err
	}
	s, err := o.API.CreateBareMetal(ctx, body)
	if err != nil {
		return nil, classify(err, opCreateInstance)
	}
	return ToBareMetalInstance(*s, o.Tags), nil
}

// hasHostFirewall says whether the launch carries the host firewall fragment.
func hasHostFirewall(p *vendors.Prepared) bool {
	return p != nil && slices.ContainsFunc(p.BootFragments, func(f vendors.BootFragment) bool {
		return f.Name == HostFirewallFragmentName
	})
}

func (o *Ops) forgetCreate(key string) {
	o.mu.Lock()
	delete(o.recent, key)
	o.mu.Unlock()
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

// availablePlans is the set of plans of a type (planTypeAll, planTypeBareMetal)
// in stock in the region, cached for the availability TTL.
func (o *Ops) availablePlans(ctx context.Context, region, planType string) (map[string]struct{}, error) {
	key := region + "/" + planType
	o.mu.Lock()
	a, ok := o.avail[key]
	o.mu.Unlock()
	if ok && o.Clock.Since(a.at) < o.ttl(o.AvailabilityTTL, DefaultAvailabilityTTL) {
		return a.plans, nil
	}
	av, err := o.API.RegionAvailability(ctx, region, planType)
	if err != nil {
		return nil, classify(err, opRegion)
	}
	a = availability{plans: map[string]struct{}{}, at: o.Clock.Now()}
	for _, p := range av.AvailablePlans {
		a.plans[p] = struct{}{}
	}
	o.mu.Lock()
	o.avail[key] = a
	o.mu.Unlock()
	return a.plans, nil
}

func (o *Ops) listPlans(ctx context.Context, opts *govultr.ListOptions) ([]govultr.Plan, *govultr.Meta, error) {
	return o.API.ListPlans(ctx, planTypeAll, opts)
}

// planInfos is the plan list's disk size and type per plan, cached.
func (o *Ops) planInfos(ctx context.Context) (map[string]planInfo, error) {
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
	p = make(map[string]planInfo, len(plans))
	for _, pl := range plans {
		p[pl.ID] = planInfo{disk: pl.Disk, typ: pl.Type}
	}
	o.mu.Lock()
	o.plans, o.plansT = p, o.Clock.Now()
	o.mu.Unlock()
	return p, nil
}

// LaunchableTypes is the OfferingLister of a region: the plans in stock
// (GET /v2/regions/{id}/availability, of type all and, when the registration
// window allows bare metal, of type vbm) that the driver launches (ClassifyPlan),
// sorted.
func (o *Ops) LaunchableTypes(ctx context.Context, region string) ([]string, error) {
	have, err := o.availablePlans(ctx, region, planTypeAll)
	if err != nil {
		return nil, err
	}
	if bareMetalWindowOK(o.RegistrationTimeout) {
		bm, err := o.availablePlans(ctx, region, planTypeBareMetal)
		if err != nil {
			return nil, err
		}
		have = maps.Clone(have)
		maps.Copy(have, bm)
	}
	infos, err := o.planInfos(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for id := range have {
		if ok, _ := ClassifyPlan(id, infos[id].disk, infos[id].typ); ok &&
			checkBareMetalWindow(id, o.RegistrationTimeout) == nil {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

// CheckPlanType refuses a GPU plan whose type the driver does not launch
// (ClassifyPlanType) as Unsupported. The plan list is read only for GPU plans.
func (o *Ops) CheckPlanType(ctx context.Context, id string) error {
	if !isGPUPlan(id) {
		return nil
	}
	infos, err := o.planInfos(ctx)
	if err != nil {
		return err
	}
	if ok, why := ClassifyPlanType(id, infos[id].typ); !ok {
		return vendors.NewError(vendors.Unsupported, codeIncompatible,
			fmt.Errorf("the driver cannot launch plan %s: %s", id, why))
	}
	return nil
}

// listOne is the list options of a call that only proves the key and the ACL.
func listOne() *govultr.ListOptions { return &govultr.ListOptions{PerPage: 1} }
