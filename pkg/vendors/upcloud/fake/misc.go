/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"net/http"
	"net/netip"
	"slices"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

type storage struct{ d upcloud.StorageDetails }

type network struct{ n upcloud.Network }

type group struct{ g upcloud.ServerGroup }

type ruleset struct {
	r       api.Ruleset
	servers []string
}

func (s *storage) copy() *upcloud.StorageDetails {
	d := s.d
	d.Labels = slices.Clone(d.Labels)
	d.ServerUUIDs = slices.Clone(d.ServerUUIDs)
	return &d
}

// GetStorages lists the storages of the account: Access "private" (every
// storage of the account, the disks of servers included) and Type ("template",
// "normal") narrow the list; label filters work as for servers. The answer
// carries the labels (the plan's template lookup relies on it).
func (f *Fake) GetStorages(ctx context.Context, r *request.GetStoragesRequest) (*upcloud.Storages, error) {
	done, err := f.begin(ctx, "GetStorages")
	if err != nil {
		return nil, err
	}
	defer done()
	out := &upcloud.Storages{}
	for _, id := range sortedKeys(f.storages) {
		st := f.storages[id]
		if r.Access != "" && st.d.Access != r.Access {
			continue
		}
		if r.Type != "" && !storageTypeMatches(st.d.Type, r.Type) {
			continue
		}
		ok, err := matchLabels(st.d.Labels, r.Filters)
		if err != nil {
			return nil, err
		}
		if ok {
			out.Storages = append(out.Storages, st.copy().Storage)
		}
	}
	return out, nil
}

func storageTypeMatches(have, want string) bool {
	if want == upcloud.StorageTypeNormal {
		return have == upcloud.StorageTypeDisk || have == upcloud.StorageTypeNormal
	}
	return have == want
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// GetStorageDetails returns one storage.
func (f *Fake) GetStorageDetails(
	ctx context.Context, r *request.GetStorageDetailsRequest,
) (*upcloud.StorageDetails, error) {
	done, err := f.begin(ctx, "GetStorageDetails")
	if err != nil {
		return nil, err
	}
	defer done()
	st, ok := f.storages[r.UUID]
	if !ok {
		return nil, notFound("STORAGE_NOT_FOUND")
	}
	return st.copy(), nil
}

// ModifyStorage changes the title, the labels and (growing only) the size.
func (f *Fake) ModifyStorage(ctx context.Context, r *request.ModifyStorageRequest) (*upcloud.StorageDetails, error) {
	done, err := f.begin(ctx, "ModifyStorage")
	if err != nil {
		return nil, err
	}
	defer done()
	st, ok := f.storages[r.UUID]
	if !ok {
		return nil, notFound("STORAGE_NOT_FOUND")
	}
	// "The storage state must be online" (API reference, Modify storage).
	if st.d.State != upcloud.StorageStateOnline {
		return nil, conflict("STORAGE_STATE_ILLEGAL")
	}
	if r.Labels != nil {
		if err := f.checkLabels(*r.Labels); err != nil {
			return nil, err
		}
		st.d.Labels = slices.Clone(*r.Labels)
	}
	if r.Title != "" {
		st.d.Title = r.Title
	}
	if r.Size != 0 {
		if r.Size < st.d.Size {
			return nil, invalid("SIZE_INVALID")
		}
		st.d.Size = r.Size
	}
	return st.copy(), nil
}

// AddTemplate adds a private template (a custom image) to a zone and returns its UUID.
func (f *Fake) AddTemplate(zone, title, tier string, sizeGB int, labels map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := &storage{}
	st.d.UUID = f.uuid('t')
	st.d.Zone, st.d.Title, st.d.Tier, st.d.Size = zone, title, tier, sizeGB
	st.d.Access, st.d.Type = upcloud.StorageAccessPrivate, upcloud.StorageTypeTemplate
	st.d.State = upcloud.StorageStateOnline
	st.d.TemplateType = upcloud.StorageTemplateTypeNative
	for _, k := range sortedKeys(labels) {
		st.d.Labels = append(st.d.Labels, upcloud.Label{Key: k, Value: labels[k]})
	}
	f.storages[st.d.UUID] = st
	return st.d.UUID
}

// SetStorageState forces a storage state (a template that is still being created).
func (f *Fake) SetStorageState(uuid, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.storages[uuid]; ok {
		st.d.State = state
	}
}

// ServerSpec describes a server to add directly, as another workload of the account.
type ServerSpec struct {
	Zone, Plan, Title string
	State             string // default started
	Labels            map[string]string
	PublicIPv4        bool
}

// AddServer adds a settled server that nothing here created (other owners'
// servers count against the account's limits and appear in the lists).
func (f *Fake) AddServer(sp ServerSpec) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	plan := f.plans[sp.Plan]
	s := &server{}
	s.d.UUID, s.d.Zone, s.d.Title, s.d.Hostname = f.uuid('o'), sp.Zone, sp.Title, sp.Title
	s.d.Plan, s.d.CoreNumber, s.d.MemoryAmount = plan.Name, plan.CoreNumber, plan.MemoryAmount
	s.d.State = cmpOr(sp.State, upcloud.ServerStateStarted)
	s.d.FirewallPrivate = "off"
	for _, k := range sortedKeys(sp.Labels) {
		s.d.Labels = append(s.d.Labels, upcloud.Label{Key: k, Value: sp.Labels[k]})
	}
	if sp.PublicIPv4 {
		f.attachInterfaces(s, []ifaceSpec{{typ: upcloud.NetworkTypePublic, ips: []string{upcloud.IPAddressFamilyIPv4}}})
	}
	if plan.StorageSize > 0 {
		st := f.newStorage(deviceSpec{req: request.CreateServerStorageDevice{
			Title: sp.Title, Size: plan.StorageSize, Tier: plan.StorageTier,
		}}, sp.Zone, plan, true, s.d.UUID)
		s.d.StorageDevices = []upcloud.ServerStorageDevice{{
			UUID: st.d.UUID, Size: st.d.Size, Tier: st.d.Tier, Title: st.d.Title, PartOfPlan: st.d.PartOfPlan,
		}}
	}
	f.servers[s.d.UUID] = s
	return s.d.UUID
}

// ForceState sets a server's state (a guest poweroff is "stopped", a failed one "error").
func (f *Fake) ForceState(uuid, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[uuid]; ok {
		s.d.State, s.target, s.creating = state, "", false
	}
}

// Server returns a copy of a server, false when it does not exist.
func (f *Fake) Server(uuid string) (*upcloud.ServerDetails, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[uuid]
	if !ok {
		return nil, false
	}
	return s.copy(), true
}

// Servers returns copies of every server, ordered by UUID.
func (f *Fake) Servers() []upcloud.ServerDetails {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := f.sortedServerIDs()
	out := make([]upcloud.ServerDetails, 0, len(ids))
	for _, id := range ids {
		out = append(out, *f.servers[id].copy())
	}
	return out
}

// Storages returns copies of every storage, ordered by UUID.
func (f *Fake) Storages() []upcloud.StorageDetails {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := sortedKeys(f.storages)
	out := make([]upcloud.StorageDetails, 0, len(ids))
	for _, id := range ids {
		out = append(out, *f.storages[id].copy())
	}
	return out
}

// ServerFirewall returns the rules written to a server.
func (f *Fake) ServerFirewall(uuid string) []upcloud.FirewallRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[uuid]; ok {
		return slices.Clone(s.fw)
	}
	return nil
}

// GetNetworks lists networks, narrowed by label filters. Utility and public
// networks are not listed (they are not the account's).
func (f *Fake) GetNetworks(ctx context.Context, fs ...request.QueryFilter) (*upcloud.Networks, error) {
	done, err := f.begin(ctx, "GetNetworks")
	if err != nil {
		return nil, err
	}
	defer done()
	out := &upcloud.Networks{}
	for _, id := range sortedKeys(f.networks) {
		n := f.networks[id]
		ok, err := matchLabels(n.n.Labels, fs)
		if err != nil {
			return nil, err
		}
		if ok {
			out.Networks = append(out.Networks, f.networkCopy(n))
		}
	}
	return out, nil
}

func (f *Fake) networkCopy(n *network) upcloud.Network {
	c := n.n
	c.Labels = slices.Clone(c.Labels)
	c.IPNetworks = slices.Clone(c.IPNetworks)
	c.Servers = nil
	for _, id := range sortedKeys(f.servers) {
		for _, i := range f.servers[id].d.Networking.Interfaces {
			if i.Network == c.UUID {
				c.Servers = append(c.Servers, upcloud.NetworkServer{ServerUUID: id, ServerTitle: f.servers[id].d.Title})
			}
		}
	}
	return c
}

// GetNetworkDetails returns one network.
func (f *Fake) GetNetworkDetails(ctx context.Context, r *request.GetNetworkDetailsRequest) (*upcloud.Network, error) {
	done, err := f.begin(ctx, "GetNetworkDetails")
	if err != nil {
		return nil, err
	}
	defer done()
	n, ok := f.networks[r.UUID]
	if !ok {
		return nil, notFound("NETWORK_NOT_FOUND")
	}
	c := f.networkCopy(n)
	return &c, nil
}

// CreateNetwork creates a private SDN network. Networks of an account must not
// overlap (NETWORK_ADDRESS_OVERLAP is this fake's name for the error: the real
// code is unverified), and the account has a network limit.
func (f *Fake) CreateNetwork(ctx context.Context, r *request.CreateNetworkRequest) (*upcloud.Network, error) {
	done, err := f.begin(ctx, "CreateNetwork")
	if err != nil {
		return nil, err
	}
	defer done()
	if !f.hasZone(r.Zone) {
		return nil, invalid("ZONE_INVALID")
	}
	if len(f.networks) >= f.opts.MaxNetworks || (f.limits.Networks > 0 && len(f.networks) >= f.limits.Networks) {
		return nil, Problem("NETWORK_LIMIT_REACHED", http.StatusForbidden)
	}
	if err := f.checkLabels(r.Labels); err != nil {
		return nil, err
	}
	for _, in := range r.IPNetworks {
		p, err := netip.ParsePrefix(in.Address)
		if err != nil {
			return nil, invalid("IP_NETWORK_ADDRESS_INVALID")
		}
		for _, n := range f.networks {
			for _, e := range n.n.IPNetworks {
				if q, err := netip.ParsePrefix(e.Address); err == nil && q.Overlaps(p) {
					return nil, conflict("NETWORK_ADDRESS_OVERLAP")
				}
			}
		}
	}
	n := &network{}
	n.n = upcloud.Network{
		UUID: f.uuid('n'), Name: r.Name, Zone: r.Zone, Type: upcloud.NetworkTypePrivate,
		IPNetworks: slices.Clone(r.IPNetworks), Labels: slices.Clone(r.Labels),
	}
	f.networks[n.n.UUID] = n
	c := f.networkCopy(n)
	return &c, nil
}

// DeleteNetwork deletes a network no server has an interface on
// (NETWORK_NOT_EMPTY, 409, as the API reference documents).
func (f *Fake) DeleteNetwork(ctx context.Context, r *request.DeleteNetworkRequest) error {
	done, err := f.begin(ctx, "DeleteNetwork")
	if err != nil {
		return err
	}
	defer done()
	n, ok := f.networks[r.UUID]
	if !ok {
		return notFound("NETWORK_NOT_FOUND")
	}
	if len(f.networkCopy(n).Servers) > 0 {
		return conflict("NETWORK_NOT_EMPTY")
	}
	delete(f.networks, r.UUID)
	return nil
}

// GetFirewallRules returns the rules of a server's Public & Utility firewall.
func (f *Fake) GetFirewallRules(
	ctx context.Context, r *request.GetFirewallRulesRequest,
) (*upcloud.FirewallRules, error) {
	done, err := f.begin(ctx, "GetFirewallRules")
	if err != nil {
		return nil, err
	}
	defer done()
	s, ok := f.servers[r.ServerUUID]
	if !ok {
		return nil, notFound("SERVER_NOT_FOUND")
	}
	return &upcloud.FirewallRules{FirewallRules: slices.Clone(s.fw)}, nil
}

// MaxFirewallRules is the limit of rules per server.
const MaxFirewallRules = 1000

// CreateFirewallRules replaces the whole rule list of the server (the API's
// "create rules" call does), numbering the positions from 1.
func (f *Fake) CreateFirewallRules(ctx context.Context, r *request.CreateFirewallRulesRequest) error {
	done, err := f.begin(ctx, "CreateFirewallRules")
	if err != nil {
		return err
	}
	defer done()
	s, ok := f.servers[r.ServerUUID]
	if !ok {
		return notFound("SERVER_NOT_FOUND")
	}
	if len(r.FirewallRules) > MaxFirewallRules {
		return invalid("FIREWALL_RULE_LIMIT_REACHED")
	}
	rules := slices.Clone([]upcloud.FirewallRule(r.FirewallRules))
	for i := range rules {
		rules[i].Position = i + 1
	}
	s.fw = rules
	return nil
}

func (g *group) copy() upcloud.ServerGroup {
	c := g.g
	c.Labels = slices.Clone(c.Labels)
	c.Members = slices.Clone(c.Members)
	c.AntiAffinityStatus = nil
	for i, m := range c.Members {
		st := upcloud.ServerAntiAffinityStatusMet
		if g.g.AntiAffinityPolicy == upcloud.ServerGroupAntiAffinityPolicyOff && i > 0 {
			st = upcloud.ServerAntiAffinityStatusUnmet
		}
		c.AntiAffinityStatus = append(c.AntiAffinityStatus,
			upcloud.ServerGroupMemberAntiAffinityStatus{ServerUUID: m, Status: st})
	}
	return c
}

// GetServerGroups lists the groups, narrowed by label filters.
func (f *Fake) GetServerGroups(
	ctx context.Context, r *request.GetServerGroupsRequest,
) (upcloud.ServerGroups, error) {
	done, err := f.begin(ctx, "GetServerGroups")
	if err != nil {
		return nil, err
	}
	defer done()
	var out upcloud.ServerGroups
	for _, id := range sortedKeys(f.groups) {
		g := f.groups[id]
		ok, err := matchLabels(g.g.Labels, r.Filters)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, g.copy())
		}
	}
	return out, nil
}

// GetServerGroup returns one group.
func (f *Fake) GetServerGroup(ctx context.Context, r *request.GetServerGroupRequest) (*upcloud.ServerGroup, error) {
	done, err := f.begin(ctx, "GetServerGroup")
	if err != nil {
		return nil, err
	}
	defer done()
	g, ok := f.groups[r.UUID]
	if !ok {
		return nil, notFound("GROUP_NOT_FOUND")
	}
	c := g.copy()
	return &c, nil
}

// CreateServerGroup creates a group (members may be added at server creation).
func (f *Fake) CreateServerGroup(
	ctx context.Context, r *request.CreateServerGroupRequest,
) (*upcloud.ServerGroup, error) {
	done, err := f.begin(ctx, "CreateServerGroup")
	if err != nil {
		return nil, err
	}
	defer done()
	var labels []upcloud.Label
	if r.Labels != nil {
		labels = *r.Labels
	}
	if err := f.checkLabels(labels); err != nil {
		return nil, err
	}
	g := &group{g: upcloud.ServerGroup{
		UUID: f.uuid('g'), Title: r.Title, AntiAffinityPolicy: cmpPolicy(r.AntiAffinityPolicy),
		Labels: slices.Clone(labels), Members: slices.Clone(r.Members),
	}}
	f.groups[g.g.UUID] = g
	c := g.copy()
	return &c, nil
}

func cmpPolicy(p upcloud.ServerGroupAntiAffinityPolicy) upcloud.ServerGroupAntiAffinityPolicy {
	if p == "" {
		return upcloud.ServerGroupAntiAffinityPolicyOff
	}
	return p
}

// DeleteServerGroup deletes a group; its members stay.
func (f *Fake) DeleteServerGroup(ctx context.Context, r *request.DeleteServerGroupRequest) error {
	done, err := f.begin(ctx, "DeleteServerGroup")
	if err != nil {
		return err
	}
	defer done()
	if _, ok := f.groups[r.UUID]; !ok {
		return notFound("GROUP_NOT_FOUND")
	}
	delete(f.groups, r.UUID)
	return nil
}

// GetIPAddresses lists the addresses of every server (public, utility and private).
func (f *Fake) GetIPAddresses(ctx context.Context) (*upcloud.IPAddresses, error) {
	done, err := f.begin(ctx, "GetIPAddresses")
	if err != nil {
		return nil, err
	}
	defer done()
	out := &upcloud.IPAddresses{}
	for _, id := range f.sortedServerIDs() {
		out.IPAddresses = append(out.IPAddresses, f.servers[id].d.IPAddresses...)
	}
	return out, nil
}

// GetFirewallRulesets lists the rulesets (without rules, with labels).
func (f *Fake) GetFirewallRulesets(ctx context.Context) ([]api.Ruleset, error) {
	done, err := f.begin(ctx, "GetFirewallRulesets")
	if err != nil {
		return nil, err
	}
	defer done()
	var out []api.Ruleset
	for _, id := range sortedKeys(f.rulesets) {
		r := f.rulesets[id].r
		r.Rules = nil
		out = append(out, r)
	}
	return out, nil
}

// GetFirewallRuleset returns one ruleset with its rules.
func (f *Fake) GetFirewallRuleset(ctx context.Context, uuid string) (*api.Ruleset, error) {
	done, err := f.begin(ctx, "GetFirewallRuleset")
	if err != nil {
		return nil, err
	}
	defer done()
	rs, ok := f.rulesets[uuid]
	if !ok {
		return nil, notFound("FIREWALL_RULESET_NOT_FOUND")
	}
	r := rs.r
	return &r, nil
}

// CreateFirewallRuleset creates a ruleset.
func (f *Fake) CreateFirewallRuleset(ctx context.Context, r api.CreateRulesetRequest) (*api.Ruleset, error) {
	done, err := f.begin(ctx, "CreateFirewallRuleset")
	if err != nil {
		return nil, err
	}
	defer done()
	if err := f.checkLabels(r.Labels); err != nil {
		return nil, err
	}
	rs := &ruleset{r: api.Ruleset{
		UUID: f.uuid('r'), Name: r.Name, Description: r.Description, Stateful: r.Stateful, Enabled: true, Version: 1,
		Labels: slices.Clone(r.Labels), Rules: slices.Clone(r.Rules),
	}}
	f.rulesets[rs.r.UUID] = rs
	out := rs.r
	return &out, nil
}

// DeleteFirewallRuleset deletes a ruleset no server uses (FIREWALL_RULESET_IN_USE
// is this fake's name for the dependency error: the real code is unverified).
func (f *Fake) DeleteFirewallRuleset(ctx context.Context, uuid string) error {
	done, err := f.begin(ctx, "DeleteFirewallRuleset")
	if err != nil {
		return err
	}
	defer done()
	rs, ok := f.rulesets[uuid]
	if !ok {
		return notFound("FIREWALL_RULESET_NOT_FOUND")
	}
	if len(rs.servers) > 0 {
		return conflict("FIREWALL_RULESET_IN_USE")
	}
	delete(f.rulesets, uuid)
	return nil
}

// AttachPrivateFirewallRuleset attaches a ruleset to the server's private
// interfaces; the server needs one (a private interface), as the ruleset only
// filters those.
func (f *Fake) AttachPrivateFirewallRuleset(ctx context.Context, serverUUID, rulesetUUID string) error {
	done, err := f.begin(ctx, "AttachPrivateFirewallRuleset")
	if err != nil {
		return err
	}
	defer done()
	s, ok := f.servers[serverUUID]
	if !ok {
		return notFound("SERVER_NOT_FOUND")
	}
	rs, ok := f.rulesets[rulesetUUID]
	if !ok {
		return notFound("FIREWALL_RULESET_NOT_FOUND")
	}
	if !slices.ContainsFunc(s.d.Networking.Interfaces, func(i upcloud.ServerInterface) bool {
		return i.Type == upcloud.NetworkTypePrivate
	}) {
		return invalid("INTERFACE_NOT_FOUND")
	}
	if !slices.Contains(rs.servers, serverUUID) {
		rs.servers = append(rs.servers, serverUUID)
	}
	return nil
}

// DetachPrivateFirewallRuleset detaches it again.
func (f *Fake) DetachPrivateFirewallRuleset(ctx context.Context, serverUUID, rulesetUUID string) error {
	done, err := f.begin(ctx, "DetachPrivateFirewallRuleset")
	if err != nil {
		return err
	}
	defer done()
	if _, ok := f.servers[serverUUID]; !ok {
		return notFound("SERVER_NOT_FOUND")
	}
	rs, ok := f.rulesets[rulesetUUID]
	if !ok {
		return notFound("FIREWALL_RULESET_NOT_FOUND")
	}
	rs.servers = slices.DeleteFunc(rs.servers, func(m string) bool { return m == serverUUID })
	return nil
}

// RulesetServers returns the servers a ruleset is attached to.
func (f *Fake) RulesetServers(uuid string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rs, ok := f.rulesets[uuid]; ok {
		return slices.Clone(rs.servers)
	}
	return nil
}
