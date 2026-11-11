/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Cache lifetimes (plan 15: rate budget, "Regions and zones", "Availability").
const (
	locationsTTL   = time.Hour
	serverTypesTTL = 3 * time.Minute
	// serversTTL is the sharing window of the project-wide server list: every
	// caller within it gets the same list (never a per-node GET in a loop).
	serversTTL = 5 * time.Second
	// MaxSpreadMembers is the size of a spread placement group.
	MaxSpreadMembers = 10
)

// ServerLimitBucket is the ID of the one synthetic global quota bucket.
const ServerLimitBucket = "servers"

// Project is the driver's view of one Hetzner project: the API behind the
// caches and the flows that are more than one call (create with idempotency,
// delete with the Primary IP cleanup, the shared server list, the optional
// server limit). The vendors.Driver implementation embeds it
// (TODO(plan 14): Init, ResolveImage through the images kind, NodeOptions,
// ManagedResources).
type Project struct {
	api         API
	clock       clock.Clock
	cluster     string
	serverLimit int

	mu        sync.Mutex
	locs      cached[[]*hc.Location]
	types     cached[[]*hc.ServerType]
	servers   cached[[]*hc.Server]
	typesHash string
	epoch     atomic.Uint64
}

type cached[T any] struct {
	v     T
	until time.Time
	ok    bool
}

// ProjectOption configures a Project.
type ProjectOption func(*Project)

// WithClock sets the clock of the caches (tests).
func WithClock(c clock.Clock) ProjectOption { return func(p *Project) { p.clock = c } }

// WithServerLimit sets the project's server limit (--hcloud-server-limit), which
// enables the synthetic quota bucket; 0 (the default) means unknown.
func WithServerLimit(n int) ProjectOption { return func(p *Project) { p.serverLimit = n } }

// NewProject wraps the API for the cluster (its name is the ownership check of
// DeleteInstance).
func NewProject(api API, cluster string, opts ...ProjectOption) *Project {
	p := &Project{api: api, clock: clock.RealClock{}, cluster: cluster}
	for _, o := range opts {
		o(p)
	}
	return p
}

// API returns the wrapped API (for the resource engine and the tests).
func (p *Project) API() API { return p.api }

// RateLimit returns the last rate-limit headers seen.
func (p *Project) RateLimit() RateLimit { return p.api.RateLimit() }

func (p *Project) locations(ctx context.Context) ([]*hc.Location, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.locs.ok && p.clock.Now().Before(p.locs.until) {
		return p.locs.v, nil
	}
	l, err := p.api.Locations(ctx)
	if err != nil {
		return nil, classify(err)
	}
	p.locs = cached[[]*hc.Location]{v: l, until: p.clock.Now().Add(locationsTTL), ok: true}
	return l, nil
}

// Regions returns the project's locations (cached 1 h). The token covers every
// location, so there is no RegionUnavailable.
func (p *Project) Regions(ctx context.Context) ([]vendors.Region, error) {
	l, err := p.locations(ctx)
	if err != nil {
		return nil, err
	}
	return RegionNames(l), nil
}

// Zones returns the single zone of a location: the location itself.
func (p *Project) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	l, err := p.locations(ctx)
	if err != nil {
		return nil, err
	}
	z, ok := LocationZones(l, region)
	if !ok {
		return nil, vendors.NewError(vendors.Unsupported, CodeUnsupportedLoc,
			fmt.Errorf("%q is not a Hetzner location of this project", region))
	}
	return z, nil
}

// NetworkZone returns the network zone of a location.
func (p *Project) NetworkZone(ctx context.Context, location string) (hc.NetworkZone, error) {
	l, err := p.locations(ctx)
	if err != nil {
		return "", err
	}
	return NetworkZoneOf(l, location), nil
}

// ServerTypes returns the type table (cached 3 min: the same call serves the
// type table and the availability).
func (p *Project) ServerTypes(ctx context.Context) ([]*hc.ServerType, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.types.ok && p.clock.Now().Before(p.types.until) {
		return p.types.v, nil
	}
	t, err := p.api.ServerTypes(ctx)
	if err != nil {
		return nil, classify(err)
	}
	p.types = cached[[]*hc.ServerType]{v: t, until: p.clock.Now().Add(serverTypesTTL), ok: true}
	if h := typesHash(t); h != p.typesHash {
		p.typesHash = h
		p.epoch.Add(1)
	}
	return t, nil
}

func typesHash(t []*hc.ServerType) string {
	parts := make([]string, 0, len(t))
	for _, st := range t {
		parts = append(parts, fmt.Sprintf("%s:%d:%d:%v:%s", st.Name, st.Cores, st.Disk, st.Memory, st.Architecture))
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// Epoch changes when the server type table changed (the EpochReporter input:
// the root disk per type is part of what the catalog derives).
func (p *Project) Epoch() uint64 { return p.epoch.Load() }

// LaunchableTypes implements vendors.OfferingLister: the types whose entry for
// the location is available and not past its deprecation, keyed by zone ID.
func (p *Project) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	t, err := p.ServerTypes(ctx)
	if err != nil {
		return nil, err
	}
	return LaunchableTypes(t, region, p.clock.Now()), nil
}

// RootDiskForType is the root disk of a type in bytes (the API's disk is GB,
// decimal); ok is false for a type the table does not have.
func (p *Project) RootDiskForType(ctx context.Context, apiReference string) (int64, bool, error) {
	t, err := p.ServerTypes(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, st := range t {
		if st.Name == apiReference {
			return int64(st.Disk) * 1_000_000_000, st.Disk > 0, nil
		}
	}
	return 0, false, nil
}

// FindImage resolves the Flatcar snapshot of a pinned release by the label
// convention (os=flatcar, flatcar-channel, flatcar-version) and the
// architecture: exactly one. None is NotFound (code ImageNotFound), several are
// Fatal (ImageAmbiguous). Snapshots are global in the project, so there is no
// location. TODO(plan 14): replaced by the images kind's selector.
func (p *Project) FindImage(ctx context.Context, arch, channel, version string) (*hc.Image, error) {
	ha, ok := ImageArch(arch)
	if !ok {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf("unknown architecture %q", arch))
	}
	sel := fmt.Sprintf("os=flatcar,flatcar-channel=%s,flatcar-version=%s", channel, version)
	imgs, err := p.api.Images(ctx, hc.ImageListOpts{
		ListOpts:     hc.ListOpts{LabelSelector: sel},
		Type:         []hc.ImageType{hc.ImageTypeSnapshot},
		Status:       []hc.ImageStatus{hc.ImageStatusAvailable},
		Architecture: []hc.Architecture{ha},
		Sort:         []string{"created:desc"},
	})
	if err != nil {
		return nil, classify(err)
	}
	switch len(imgs) {
	case 0:
		return nil, vendors.NewError(vendors.NotFound, CodeImageNotFound, fmt.Errorf(
			"no available Flatcar snapshot (%s) for %s %s; create it with hcloud-upload-image and the labels of the convention",
			arch, channel, version))
	case 1:
		return imgs[0], nil
	}
	return nil, vendors.NewError(vendors.Fatal, CodeImageAmbiguous, fmt.Errorf(
		"%d Flatcar snapshots (%s) carry the labels for %s %s; exactly one is required", len(imgs), arch, channel, version))
}

// ResolveSSHKey finds the key of the NodeClass's sshKey (name or ID).
func (p *Project) ResolveSSHKey(ctx context.Context, idOrName string) (int64, error) {
	k, err := p.api.SSHKey(ctx, idOrName)
	if err != nil {
		return 0, classify(err)
	}
	if k == nil {
		return 0, vendors.NewError(vendors.Unsupported, CodeInvalidSettings,
			fmt.Errorf("the SSH key %q does not exist in the project", idOrName))
	}
	return k.ID, nil
}

// serverList is the shared, project-wide server list: one paginated call every
// 5 seconds at most, whatever the number of callers. It holds every server of
// the project (also those of other owners: the server limit counts them).
func (p *Project) serverList(ctx context.Context) ([]*hc.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.servers.ok && p.clock.Now().Before(p.servers.until) {
		return p.servers.v, nil
	}
	s, err := p.api.Servers(ctx, hc.ServerListOpts{})
	if err != nil {
		return nil, classify(err)
	}
	p.servers = cached[[]*hc.Server]{v: s, until: p.clock.Now().Add(serversTTL), ok: true}
	return s, nil
}

func (p *Project) forgetServers() {
	p.mu.Lock()
	p.servers = cached[[]*hc.Server]{}
	p.mu.Unlock()
}

// Create launches one server (plan 15, "CreateInstance"): one POST /servers, then
// the wait for the create action. A failed or timed-out action deletes the
// half-created server and returns the action's error, classified, so that the
// attempt loop can fall back to another offering.
//
// Idempotency: the server name is unique per project. A replay fails with
// uniqueness_error; the server is read by name and returned when its
// idempotency-key label is the key (whatever zone, type or capacity type the
// replay asked for). A server of that name with another key is a leftover of an
// earlier NodeClaim of the same name: Retryable, code NameInUse.
func (p *Project) Create(ctx context.Context, spec LaunchSpec) (*vendors.Instance, error) {
	opts, err := BuildCreateOpts(spec)
	if err != nil {
		return nil, err
	}
	if spec.PlacementGroupID != 0 {
		if err := p.checkPlacementRoom(ctx, spec.PlacementGroupID); err != nil {
			return nil, err
		}
	}
	res, err := p.api.CreateServer(ctx, opts)
	if err != nil {
		if isCode(err, hc.ErrorCodeUniquenessError) {
			if inst, ferr := p.replay(ctx, opts.Name, spec.IdempotencyKey); inst != nil || ferr != nil {
				return inst, ferr
			}
		}
		return nil, classify(err)
	}
	p.forgetServers()
	if res.Server == nil {
		return nil, vendors.Errorf(vendors.Fatal, "POST /servers returned no server")
	}
	if res.Action != nil {
		if err := p.api.WaitForAction(ctx, res.Action); err != nil {
			p.discard(ctx, res.Server)
			return nil, classify(err)
		}
	}
	return ToInstance(res.Server, spec.Location), nil
}

// replay resolves a uniqueness_error of a create: the server by name, when it
// carries the key. nil, nil means the name is free after all (the caller returns
// the original error).
func (p *Project) replay(ctx context.Context, name, key string) (*vendors.Instance, error) {
	list, err := p.api.Servers(ctx, hc.ServerListOpts{Name: name})
	if err != nil {
		return nil, classify(err)
	}
	for _, s := range list {
		if s.Name != name {
			continue
		}
		if key != "" && s.Labels[TagKey(vendors.TagIdempotencyKey)] == LabelValue(key) {
			return ToInstance(s, ""), nil
		}
		return nil, vendors.NewError(vendors.Retryable, CodeNameInUse, fmt.Errorf(
			"a server named %q of another NodeClaim still exists; the garbage collector deletes it", name))
	}
	return nil, nil
}

// discard deletes a half-created server, under its own context (the create's
// may be the thing that expired).
func (p *Project) discard(ctx context.Context, s *hc.Server) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultCallTimeout)
	defer cancel()
	_, _ = p.api.DeleteServer(dctx, s)
	p.forgetServers()
}

// checkPlacementRoom counts the group's members from the shared server list
// and refuses a create into a full spread group (10 servers): the group is not
// reused, the next run key member waits.
func (p *Project) checkPlacementRoom(ctx context.Context, groupID int64) error {
	list, err := p.serverList(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, s := range list {
		if s.PlacementGroup != nil && s.PlacementGroup.ID == groupID {
			n++
		}
	}
	if n >= MaxSpreadMembers {
		return vendors.NewError(vendors.InsufficientCapacity, CodePlacementFull, fmt.Errorf(
			"placement group %d has %d servers, the spread limit", groupID, n))
	}
	return nil
}

// Get returns the instance of a reference; NotFound for a server the API does
// not know and for a reference that does not parse. Hetzner has no terminated
// instances: a deleted server is gone.
func (p *Project) Get(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	loc, id, err := ParseRef(ref)
	if err != nil {
		return nil, vendors.NewError(vendors.NotFound, "", err)
	}
	s, err := p.api.GetServer(ctx, id)
	if err != nil {
		return nil, classify(err)
	}
	if s == nil {
		return nil, vendors.Errorf(vendors.NotFound, "server %d does not exist", id)
	}
	return ToInstance(s, loc), nil
}

// List returns the servers in the locations (empty: all) that carry all the
// tags, from the shared list; stopped servers are included (terminating), the
// garbage collector deletes them.
func (p *Project) List(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	list, err := p.serverList(ctx)
	if err != nil {
		return nil, err
	}
	var out []*vendors.Instance
	for _, s := range list {
		inst := ToInstance(s, "")
		if len(regions) > 0 && !slices.Contains(regions, inst.Region) {
			continue
		}
		if !HasTags(s.Labels, tags) {
			continue
		}
		out = append(out, inst)
	}
	return out, nil
}

// Find returns the first server in the location with all the tags; NotFound
// when there is none.
func (p *Project) Find(ctx context.Context, region string, tags map[vendors.TagKey]string) (*vendors.Instance, error) {
	got, err := p.List(ctx, []string{region}, tags)
	if err != nil {
		return nil, err
	}
	if len(got) == 0 {
		return nil, vendors.Errorf(vendors.NotFound, "no server in %s with the tags %s", region, Selector(tags))
	}
	return got[0], nil
}

// codeNotOwned is raised when a delete would hit a server that is not this
// cluster's.
const codeNotOwned = "NotOwned"

// Delete deletes the server of a reference (plan 15, "DeleteInstance"): not found
// is success, a reference that does not parse is a server that is gone, a server
// of another cluster or without the marker is refused (a broad token could
// delete anything), locked is Retryable and protected Fatal. It waits for the
// delete action, then removes the server's Primary IPs that stayed unassigned
// without auto_delete (they bill monthly); whether Hetzner auto-deletes them is
// unverified (L5 asserts it).
func (p *Project) Delete(ctx context.Context, ref vendors.InstanceRef) error {
	_, id, err := ParseRef(ref)
	if err != nil {
		return nil
	}
	s, err := p.api.GetServer(ctx, id)
	if err != nil {
		if isCode(err, hc.ErrorCodeNotFound) {
			return nil
		}
		return classify(err)
	}
	if s == nil {
		return nil
	}
	if s.Labels[TagKey(vendors.TagManagedBy)] != vendors.ManagedByValue ||
		s.Labels[TagKey(vendors.TagCluster)] != LabelValue(p.cluster) {
		return vendors.NewError(vendors.Fatal, codeNotOwned, fmt.Errorf(
			"server %d (%s) does not carry this cluster's marker (managed-by=%s, cluster=%s); refusing to delete it",
			s.ID, s.Name, vendors.ManagedByValue, p.cluster))
	}
	ips := primaryIPIDs(s)
	act, err := p.api.DeleteServer(ctx, s)
	p.forgetServers()
	if err != nil {
		if isCode(err, hc.ErrorCodeNotFound) {
			return nil
		}
		return classify(err)
	}
	if act != nil {
		if err := p.api.WaitForAction(ctx, act); err != nil && !isCode(err, hc.ErrorCodeNotFound) {
			return classify(err)
		}
	}
	return p.CleanupPrimaryIPs(ctx, ips)
}

func primaryIPIDs(s *hc.Server) []int64 {
	var ids []int64
	if s.PublicNet.IPv4.ID != 0 {
		ids = append(ids, s.PublicNet.IPv4.ID)
	}
	if s.PublicNet.IPv6.ID != 0 {
		ids = append(ids, s.PublicNet.IPv6.ID)
	}
	return ids
}

// CleanupPrimaryIPs deletes the Primary IPs that are unassigned and not
// auto-deleting. One that is gone, or still assigned, is left alone.
func (p *Project) CleanupPrimaryIPs(ctx context.Context, ids []int64) error {
	var errs []error
	for _, id := range ids {
		ip, err := p.api.GetPrimaryIP(ctx, id)
		if err != nil {
			if !isCode(err, hc.ErrorCodeNotFound) {
				errs = append(errs, classify(err))
			}
			continue
		}
		if ip == nil || ip.AssigneeID != 0 || ip.AutoDelete {
			continue
		}
		err = p.api.DeletePrimaryIP(ctx, ip)
		if err != nil && !isCode(err, hc.ErrorCodeNotFound, hc.ErrorCodeResourceInUse) {
			errs = append(errs, classify(err))
		}
	}
	return errors.Join(errs...)
}

// Quotas implements vendors.QuotaReporter: Hetzner has no regional quota.
func (p *Project) Quotas(context.Context, string) (*vendors.QuotaSnapshot, error) {
	return &vendors.QuotaSnapshot{}, nil
}

// GlobalQuotas implements vendors.QuotaReporter: with a configured server
// limit one global bucket servers (unit instances) whose usage is every server
// of the project, those of other owners included. Without a limit the project's
// limits are unknown (they are not readable through the API) and the path is
// reactive: QuotaExceeded on resource_limit_exceeded.
func (p *Project) GlobalQuotas(ctx context.Context) (*vendors.QuotaSnapshot, error) {
	if p.serverLimit <= 0 {
		return &vendors.QuotaSnapshot{}, nil
	}
	list, err := p.serverList(ctx)
	if err != nil {
		return nil, err
	}
	snap := &vendors.QuotaSnapshot{Buckets: []vendors.Bucket{{
		ID: ServerLimitBucket, Scope: vendors.QuotaScopeGlobal, Unit: "instances",
		Limit: int64(p.serverLimit), Used: int64(len(list)),
	}}}
	for _, s := range list {
		loc := ""
		if s.Location != nil {
			loc = s.Location.Name
		}
		snap.Counted = append(snap.Counted, NewRef(loc, s.ID))
	}
	return snap, nil
}

// Consumption implements vendors.QuotaReporter: every type consumes one server
// of the bucket; false (unknown, reactive) without a configured limit.
func (p *Project) Consumption(string, string) ([]vendors.Consumption, bool) {
	if p.serverLimit <= 0 {
		return nil, false
	}
	return []vendors.Consumption{{Bucket: ServerLimitBucket, Amount: 1}}, true
}

var (
	_ vendors.QuotaReporter  = (*Project)(nil)
	_ vendors.OfferingLister = (*Project)(nil)
	_ vendors.EpochReporter  = (*Project)(nil)
)

// NodeOptionsEpoch implements vendors.EpochReporter.
func (p *Project) NodeOptionsEpoch() uint64 { return p.Epoch() }

// ServerID parses a decimal ID, for the callers that read IDs out of resource
// references.
func ServerID(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
