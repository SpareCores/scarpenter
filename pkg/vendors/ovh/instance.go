/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// Codes of the instance calls.
const (
	CodeStaleServer = "StaleServerRemoved"
	CodeNoExtNet    = "NoExtNet"
)

// launchResources are the Neutron groups and the Nova server group a node
// uses, from Prepared.
type launchResources struct {
	groupNames  []string
	groupIDs    []string
	serverGroup string
}

func resourcesOf(p *vendors.Prepared) launchResources {
	var lr launchResources
	if p == nil {
		return lr
	}
	for _, r := range p.Resources {
		switch r.Kind {
		case KindSecurityGroups, KindRunSecurityGroups:
			if !slices.Contains(lr.groupIDs, r.ID) {
				lr.groupIDs = append(lr.groupIDs, r.ID)
				lr.groupNames = append(lr.groupNames, r.Name)
			}
		case KindServerGroups:
			lr.serverGroup = r.ID
		}
	}
	return lr
}

// claimName is the Nova server name of the launch: the NodeClaim.
func claimName(r vendors.CreateRequest) string {
	if r.Node.Name != "" {
		return r.Node.Name
	}
	return r.Tags[vendors.TagNodeClaim]
}

func nameFilter(name string) string { return "^" + regexp.QuoteMeta(name) + "$" }

// clusterFilter is the server filter that lists a cluster's servers (Nova
// microversion 2.26); a Nova that ignores it returns everything, and the callers
// filter on the metadata too.
func clusterFilter(tags map[vendors.TagKey]string) []string {
	if c := tags[vendors.TagCluster]; c != "" {
		return []string{ClusterTag(c)}
	}
	return nil
}

// CreateInstance implements vendors.Driver: Nova has no client token, so the
// idempotency key lives in the server's metadata and the call first looks for a
// server that carries it (whatever the flavor or zone of the replay), deletes
// what an earlier attempt left of the same name, and only then creates; it then
// waits for the scheduler (CreateAndWait) so that a capacity failure falls
// through to the next candidate.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if len(req.UserData) > MaxUserData {
		return nil, vendors.NewError(vendors.Fatal, CodeUserDataTooLarge, fmt.Errorf(
			"the user data is %d bytes; the budget is %d (a larger cluster CA or Ignition addition does not fit)",
			len(req.UserData), MaxUserData))
	}
	st, err := ParseVendorSettings(req.Settings)
	if err != nil {
		return nil, err
	}
	cl, err := d.clients(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	name := claimName(req)
	if name == "" {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf("the launch has no NodeClaim name"))
	}
	if inst, err := d.replay(ctx, cl, req, name); inst != nil || err != nil {
		return inst, err
	}
	flavorID, err := d.flavorID(ctx, cl, req.Region, req.InstanceType)
	if err != nil {
		return nil, err
	}
	ext, err := d.extNetwork(ctx, cl, req.Region)
	if err != nil {
		return nil, err
	}
	zones, err := d.Zones(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	lr := resourcesOf(req.Prepared)
	groups, err := d.groupNames(ctx, cl, lr)
	if err != nil {
		return nil, err
	}
	osReq, err := BuildCreateRequest(LaunchPlan{
		Request: req, Settings: st, FlavorID: flavorID, ExternalNetworkID: ext, SecurityGroups: groups,
		ServerGroupID: lr.serverGroup, MultiAZ: len(zones) > 1,
	})
	if err != nil {
		return nil, err
	}
	s, err := CreateAndWait(ctx, cl.Compute, osReq, d.wait)
	if err != nil {
		return nil, err
	}
	return d.instance(ctx, req.Region, *s), nil
}

// replay finds the server an earlier attempt of the same launch made (the same
// idempotency key) and cleans up what it left: a server of the key in ERROR, and
// a server of the same name that belongs to an earlier NodeClaim (another key,
// the cluster's marker), after which the call is Retryable. nil, nil means
// create.
func (d *Driver) replay(
	ctx context.Context, cl osapi.Clients, req vendors.CreateRequest, name string,
) (*vendors.Instance, error) {
	servers, err := cl.Compute.ListServers(ctx, osapi.ServerFilter{
		Name: nameFilter(name), Tags: clusterFilter(req.Tags),
	})
	if err != nil {
		return nil, Classify(err)
	}
	cluster := req.Tags[vendors.TagCluster]
	servers = slices.DeleteFunc(servers, func(s osapi.Server) bool { return s.Name != name })
	match, stale := FindByKey(servers, req.IdempotencyKey)
	if match != nil {
		return d.instance(ctx, req.Region, *match), nil
	}
	for _, s := range stale {
		if err := d.remove(ctx, cl, s.ID); err != nil {
			return nil, err
		}
	}
	if req.IdempotencyKey == "" || cluster == "" {
		return nil, nil
	}
	leftover := false
	for _, s := range servers {
		if s.Metadata[TagKey(vendors.TagCluster)] != cluster ||
			s.Metadata[TagKey(vendors.TagIdempotencyKey)] == req.IdempotencyKey ||
			ServerState(s) == vendors.StateTerminated || strings.EqualFold(s.TaskState, osapi.TaskDeleting) ||
			slices.ContainsFunc(stale, func(x osapi.Server) bool { return x.ID == s.ID }) {
			continue
		}
		if err := d.remove(ctx, cl, s.ID); err != nil {
			return nil, err
		}
		leftover = true
	}
	if leftover {
		return nil, vendors.NewError(vendors.Retryable, CodeStaleServer, fmt.Errorf(
			"a server named %s of an earlier NodeClaim was left behind and is being deleted; the launch is retried", name))
	}
	return nil, nil
}

// remove deletes a server; one that is gone already is fine.
func (d *Driver) remove(ctx context.Context, cl osapi.Clients, id string) error {
	if err := cl.Compute.DeleteServer(ctx, id); err != nil && !IsNotFound(err) {
		return Classify(err)
	}
	return nil
}

// instance maps a server to the contract's Instance. The billing start is
// Nova's created, which is at BUILD, earlier than the meter's start: the safe
// side (plan 18, "Billing period").
func (d *Driver) instance(ctx context.Context, region string, s osapi.Server) *vendors.Instance {
	in := InstanceFrom(region, d.zoneOf(ctx, region, s), s)
	in.BillingStart = s.Created
	return in
}

// flavorMap returns the region's flavors by name, cached for an hour.
func (d *Driver) flavorMap(
	ctx context.Context, cl osapi.Clients, region string, refresh bool,
) (map[string]osapi.Flavor, error) {
	if !refresh {
		d.mu.Lock()
		v, ok := d.flavors[region].get(d.clock.Now())
		d.mu.Unlock()
		if ok {
			return v, nil
		}
	}
	fl, err := cl.Compute.ListFlavors(ctx)
	if err != nil {
		return nil, Classify(err)
	}
	m := make(map[string]osapi.Flavor, len(fl))
	for _, f := range fl {
		m[f.Name] = f
	}
	d.mu.Lock()
	c := d.flavors[region]
	if c == nil {
		c = &cached[map[string]osapi.Flavor]{}
		d.flavors[region] = c
	}
	c.set(m, d.clock.Now().Add(flavorTTL))
	d.mu.Unlock()
	return m, nil
}

// flavorID is the region's ID of a flavor name (IDs differ per region; the name
// is Keeper's api_reference). A name the region lacks is Unsupported there.
func (d *Driver) flavorID(ctx context.Context, cl osapi.Clients, region, name string) (string, error) {
	m, err := d.flavorMap(ctx, cl, region, false)
	if err != nil {
		return "", err
	}
	f, ok := m[name]
	if !ok {
		if m, err = d.flavorMap(ctx, cl, region, true); err != nil {
			return "", err
		}
		if f, ok = m[name]; !ok {
			return "", vendors.NewError(vendors.Unsupported, CodeFlavorUnusable,
				fmt.Errorf("the flavor %q does not exist in %s", name, region))
		}
	}
	return f.ID, nil
}

// extNetwork is the region's Ext-Net network, cached: the external network
// named Ext-Net, which gives a public IPv4 and IPv6 without floating IPs.
func (d *Driver) extNetwork(ctx context.Context, cl osapi.Clients, region string) (string, error) {
	d.mu.Lock()
	v, ok := d.extNet[region].get(d.clock.Now())
	d.mu.Unlock()
	if ok {
		return v, nil
	}
	nets, err := cl.Network.ListNetworks(ctx)
	if err != nil {
		return "", Classify(err)
	}
	id := ""
	for _, n := range nets {
		if n.Name == osapi.ExternalNetwork && (n.External || id == "") {
			id = n.ID
		}
	}
	if id == "" {
		return "", vendors.NewError(vendors.Unsupported, CodeNoExtNet,
			fmt.Errorf("the region %s has no %s network: a node cannot get a public address there",
				region, osapi.ExternalNetwork))
	}
	d.mu.Lock()
	c := d.extNet[region]
	if c == nil {
		c = &cached[string]{}
		d.extNet[region] = c
	}
	c.set(id, d.clock.Now().Add(extNetTTL))
	d.mu.Unlock()
	return id, nil
}

// groupNames are the names of the groups of a launch (Nova attaches groups by
// name); a group handed over by ID alone is looked up.
func (d *Driver) groupNames(ctx context.Context, cl osapi.Clients, lr launchResources) ([]string, error) {
	out := make([]string, 0, len(lr.groupIDs))
	var all []osapi.SecurityGroup
	for i, id := range lr.groupIDs {
		if lr.groupNames[i] != "" {
			out = append(out, lr.groupNames[i])
			continue
		}
		if all == nil {
			var err error
			if all, err = cl.Network.ListSecurityGroups(ctx); err != nil {
				return nil, Classify(err)
			}
		}
		i := slices.IndexFunc(all, func(g osapi.SecurityGroup) bool { return g.ID == id })
		if i < 0 {
			return nil, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
				fmt.Errorf("the security group %s does not exist", id))
		}
		out = append(out, all[i].Name)
	}
	return out, nil
}

// GetInstance implements vendors.Driver. A reference the driver cannot parse, or
// one into a region the catalog lacks, is an instance that does not exist.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	region, id, err := ParseInstanceRef(ref)
	if err != nil {
		return nil, err
	}
	cl, err := d.clients(ctx, region)
	if err != nil {
		if regionGone(err) {
			return nil, vendors.NewError(vendors.NotFound, CodeNotFound, err)
		}
		return nil, err
	}
	s, err := cl.Compute.GetServer(ctx, id)
	if err != nil {
		return nil, Classify(err)
	}
	if ServerState(*s) == vendors.StateTerminated {
		return nil, vendors.NewError(vendors.NotFound, CodeNotFound, fmt.Errorf("the server %s is deleted", id))
	}
	return d.instance(ctx, region, *s), nil
}

// matches says the server's metadata carries all the tags.
func matches(s osapi.Server, tags map[vendors.TagKey]string) bool {
	for k, v := range tags {
		if s.Metadata[TagKey(k)] != v {
			return false
		}
	}
	return true
}

// stuckDeletingAfter is how long a server may stay in task state deleting
// before the listings show it again (as terminating), so that the garbage
// collector's second delete reaches a server Nova's delete left stuck (the
// driver contract: ListInstances returns the instances the vendor is deleting).
// A delete that completes normally is gone well inside it.
const stuckDeletingAfter = 5 * time.Minute

// deletingForgetAfter drops the first sightings of servers no longer seen.
const deletingForgetAfter = 24 * time.Hour

// stuckDeleting records the first sighting of a server in task state deleting
// and reports whether it has been deleting for longer than stuckDeletingAfter.
func (d *Driver) stuckDeleting(region, id string, now time.Time) bool {
	key := region + "/" + id
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, at := range d.deletingSince {
		if now.Sub(at) > deletingForgetAfter {
			delete(d.deletingSince, k)
		}
	}
	at, seen := d.deletingSince[key]
	if !seen {
		d.deletingSince[key] = now
		return false
	}
	return now.Sub(at) >= stuckDeletingAfter
}

// list reads the region's servers that carry the tags, oldest first, without the
// ones that are gone (DELETED) and without those being deleted (task state
// deleting) for less than stuckDeletingAfter: nobody needs to delete them again.
// A server stuck deleting for longer comes back as terminating, for the garbage
// collector to delete again. A server that is off by itself (SHUTOFF) stays: OVH
// keeps billing it.
func (d *Driver) list(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) ([]osapi.Server, error) {
	cl, err := d.clients(ctx, region)
	if err != nil {
		return nil, err
	}
	servers, err := cl.Compute.ListServers(ctx, osapi.ServerFilter{Tags: clusterFilter(tags)})
	if err != nil {
		return nil, Classify(err)
	}
	now := d.clock.Now()
	servers = slices.DeleteFunc(servers, func(s osapi.Server) bool {
		if ServerState(s) == vendors.StateTerminated || !matches(s, tags) {
			return true
		}
		return strings.EqualFold(s.TaskState, osapi.TaskDeleting) && !d.stuckDeleting(region, s.ID, now)
	})
	slices.SortStableFunc(servers, func(a, b osapi.Server) int { return a.Created.Compare(b.Created) })
	return servers, nil
}

// FindInstance implements vendors.Driver: a live server of the tags; a running or
// pending one wins over one that is going away.
func (d *Driver) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	servers, err := d.list(ctx, region, tags)
	if err != nil {
		if regionGone(err) {
			return nil, vendors.NewError(vendors.NotFound, CodeNotFound, err)
		}
		return nil, err
	}
	if len(servers) == 0 {
		return nil, vendors.NewError(vendors.NotFound, CodeNotFound,
			fmt.Errorf("no server has the tags %v in %s", tags, region))
	}
	best := servers[0]
	for _, s := range servers {
		if ServerState(best) == vendors.StateTerminating && ServerState(s) != vendors.StateTerminating {
			best = s
		}
	}
	return d.instance(ctx, region, best), nil
}

// ListInstances implements vendors.Driver. A terminating server (shut off by
// itself, which OVH keeps billing, or stuck deleting) stays in the list so that
// the garbage collector deletes it; DELETED ones do not.
func (d *Driver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	known, err := d.Regions(ctx)
	if err != nil {
		return nil, err
	}
	var out []*vendors.Instance
	for _, r := range regions {
		if !slices.ContainsFunc(known, func(k vendors.Region) bool { return k.Name == r }) {
			continue
		}
		servers, err := d.list(ctx, r, tags)
		if err != nil {
			return nil, err
		}
		for _, s := range servers {
			out = append(out, d.instance(ctx, r, s))
		}
	}
	return out, nil
}

// DeleteInstance implements vendors.Driver: 404 is success, and so is a
// reference that cannot exist; 409 (a server in a transient task state) is
// Retryable. Volumes made by the create call go with the server.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	region, id, err := ParseInstanceRef(ref)
	if err != nil {
		return nil //nolint:nilerr // a reference that cannot exist is an instance that is gone
	}
	cl, err := d.clients(ctx, region)
	if err != nil {
		if regionGone(err) {
			return nil
		}
		return err
	}
	return d.remove(ctx, cl, id)
}
