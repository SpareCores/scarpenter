/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
)

type server struct {
	d upcloud.ServerDetails
	// target is the state the server settles to; empty when it is settled.
	target string
	fw     []upcloud.FirewallRule
	// creating marks a server whose create has not settled (CreatingLimit).
	creating bool
}

func (s *server) copy() *upcloud.ServerDetails {
	d := s.d
	d.Labels = slices.Clone(d.Labels)
	d.StorageDevices = slices.Clone(d.StorageDevices)
	d.IPAddresses = slices.Clone(d.IPAddresses)
	d.Networking.Interfaces = slices.Clone(d.Networking.Interfaces)
	d.Tags = slices.Clone(d.Tags)
	return &d
}

func (s *server) summary() upcloud.Server {
	// The list answers carry no labels and no storage: the plan's idempotency
	// lookup has to read the details of every match.
	sv := s.d.Server
	sv.Tags = nil
	return sv
}

func (f *Fake) planOf(r *request.CreateServerRequest) (PlanSpec, error) {
	if r.Plan == "" || r.Plan == "custom" {
		return PlanSpec{Plan: upcloud.Plan{Name: "custom", CoreNumber: r.CoreNumber, MemoryAmount: r.MemoryAmount}}, nil
	}
	p, ok := f.plans[r.Plan]
	if !ok {
		return PlanSpec{}, invalid("PLAN_INVALID")
	}
	return p, nil
}

func (f *Fake) hasZone(z string) bool {
	return slices.ContainsFunc(f.zones, func(zz upcloud.Zone) bool { return zz.ID == z })
}

// usage is the account's consumption: every server (other owners' included) and
// every private storage.
func (f *Fake) usage() (cores, memMB, ipv4 int, gb map[string]int) {
	gb = map[string]int{}
	for _, s := range f.servers {
		cores += s.d.CoreNumber
		memMB += s.d.MemoryAmount
		for _, ip := range s.d.IPAddresses {
			if ip.Access == upcloud.IPAddressAccessPublic && ip.Family == upcloud.IPAddressFamilyIPv4 {
				ipv4++
			}
		}
	}
	for _, st := range f.storages {
		if st.d.Access == upcloud.StorageAccessPrivate {
			gb[st.d.Tier] += st.d.Size
		}
	}
	return cores, memMB, ipv4, gb
}

func (f *Fake) checkLabels(ls []upcloud.Label) error {
	if len(ls) > 255 {
		return invalid("LABEL_LIMIT_REACHED")
	}
	seen := map[string]bool{}
	for _, l := range ls {
		if len(l.Key) < 2 || len(l.Key) > 32 || strings.HasPrefix(l.Key, "_") {
			return invalid("INVALID_LABEL_KEY")
		}
		if len(l.Value) > 255 {
			return invalid("INVALID_LABEL_VALUE")
		}
		if seen[l.Key] {
			return invalid("DUPLICATE_LABEL_KEYS")
		}
		seen[l.Key] = true
	}
	return nil
}

// CreateServer creates a server in the maintenance state: its storage is cloned
// and it starts later (Settle, or WaitForServerState). There is no idempotency
// token: a replayed call creates a second server.
// storageLabels go on every storage the call creates or clones (the API takes
// labels per storage device; api.Client puts them in the body).
func (f *Fake) CreateServer(
	ctx context.Context, r *request.CreateServerRequest, storageLabels ...upcloud.Label,
) (*upcloud.ServerDetails, error) {
	done, err := f.begin(ctx, "CreateServer")
	if err != nil {
		return nil, err
	}
	defer done()
	if !f.hasZone(r.Zone) {
		return nil, invalid("ZONE_INVALID")
	}
	plan, err := f.planOf(r)
	if err != nil {
		return nil, err
	}
	if len(r.UserData) > f.opts.MaxUserData {
		return nil, invalid("USER_DATA_INVALID")
	}
	var labels []upcloud.Label
	if r.Labels != nil {
		labels = *r.Labels
	}
	if err := f.checkLabels(labels); err != nil {
		return nil, err
	}
	if err := f.checkLabels(storageLabels); err != nil {
		return nil, err
	}
	if len(r.StorageDevices) > f.opts.MaxStorageDevices {
		return nil, invalid("STORAGE_DEVICE_LIMIT_REACHED")
	}
	devs, err := f.planDevices(r, plan)
	if err != nil {
		return nil, err
	}
	ifaces, nIPv4, err := f.planInterfaces(r)
	if err != nil {
		return nil, err
	}
	if err := f.checkCreateLimits(plan, devs, nIPv4); err != nil {
		return nil, err
	}
	if f.outOfStock[r.Zone+"/"+plan.Name] {
		return nil, Problem("SERVER_RESOURCES_UNAVAILABLE", 503)
	}
	if r.ServerGroup != "" {
		if err := f.checkGroup(r.ServerGroup); err != nil {
			return nil, err
		}
	}
	return f.commitServer(r, plan, labels, storageLabels, devs, ifaces), nil
}

type deviceSpec struct {
	req      request.CreateServerStorageDevice
	template *storage
}

// planDevices validates the storage devices of a create call.
func (f *Fake) planDevices(r *request.CreateServerRequest, plan PlanSpec) ([]deviceSpec, error) {
	var out []deviceSpec
	for _, d := range r.StorageDevices {
		spec := deviceSpec{req: d}
		tier := d.Tier
		switch d.Action {
		case request.CreateServerStorageDeviceActionClone:
			t, ok := f.storages[d.Storage]
			if !ok {
				return nil, notFound("STORAGE_NOT_FOUND")
			}
			if t.d.Zone != r.Zone {
				return nil, invalid("ZONE_MISMATCH")
			}
			if t.d.State != upcloud.StorageStateOnline {
				return nil, conflict("STORAGE_STATE_ILLEGAL")
			}
			if d.Size != 0 && d.Size < t.d.Size {
				return nil, invalid("SIZE_INVALID")
			}
			if tier == "" {
				tier = t.d.Tier
			}
			if tier != t.d.Tier && !f.opts.LooseTemplateTier {
				return nil, invalid("STORAGE_TIER_ILLEGAL")
			}
			spec.template = t
		case request.CreateServerStorageDeviceActionCreate:
			if d.Size < 10 || d.Size > 1024 {
				return nil, invalid("SIZE_INVALID")
			}
		default:
			return nil, invalid("STORAGE_ACTION_INVALID")
		}
		if tier != "" && len(plan.AllowedTiers) > 0 && !slices.Contains(plan.AllowedTiers, tier) {
			return nil, invalid("STORAGE_TIER_ILLEGAL")
		}
		out = append(out, spec)
	}
	return out, nil
}

type ifaceSpec struct {
	typ     string
	network string
	ips     []string // families
}

// planInterfaces validates the interfaces and counts the public IPv4 addresses
// they take.
func (f *Fake) planInterfaces(r *request.CreateServerRequest) ([]ifaceSpec, int, error) {
	if r.Networking == nil {
		return nil, 0, invalid("NETWORKING_INVALID")
	}
	var out []ifaceSpec
	ipv4 := 0
	if len(r.Networking.Interfaces) < 1 || len(r.Networking.Interfaces) > 10 {
		return nil, 0, invalid("INTERFACE_LIMIT_REACHED")
	}
	for _, i := range r.Networking.Interfaces {
		spec := ifaceSpec{typ: i.Type, network: i.Network}
		for _, a := range i.IPAddresses {
			spec.ips = append(spec.ips, a.Family)
			if i.Type == upcloud.NetworkTypePublic && a.Family == upcloud.IPAddressFamilyIPv4 {
				ipv4++
			}
		}
		switch i.Type {
		case upcloud.NetworkTypePublic:
			if len(spec.ips) > 0 && f.noIPs[r.Zone] {
				return nil, 0, Problem("IP_ADDRESS_RESOURCES_UNAVAILABLE", 503)
			}
		case upcloud.NetworkTypePrivate:
			n, ok := f.networks[i.Network]
			if !ok {
				return nil, 0, notFound("NETWORK_NOT_FOUND")
			}
			if n.n.Zone != r.Zone {
				return nil, 0, invalid("ZONE_MISMATCH")
			}
		case upcloud.NetworkTypeUtility:
		default:
			return nil, 0, invalid("INTERFACE_TYPE_INVALID")
		}
		out = append(out, spec)
	}
	return out, ipv4, nil
}

// checkCreateLimits applies the account limits (0 is no limit) and the limit
// on servers being created at once.
func (f *Fake) checkCreateLimits(plan PlanSpec, devs []deviceSpec, ipv4 int) error {
	creating := 0
	for _, s := range f.servers {
		if s.creating {
			creating++
		}
	}
	if creating >= f.opts.CreatingLimit {
		return Problem("SERVER_CREATING_LIMIT_REACHED", http.StatusTooManyRequests)
	}
	cores, mem, ips, gb := f.usage()
	l := f.limits
	if l.Cores > 0 && cores+plan.CoreNumber > l.Cores {
		return Problem("SERVER_CORE_LIMIT_REACHED", http.StatusForbidden)
	}
	if l.Memory > 0 && mem+plan.MemoryAmount > l.Memory {
		return Problem("SERVER_MEMORY_LIMIT_REACHED", http.StatusForbidden)
	}
	if l.PublicIPv4 > 0 && ips+ipv4 > l.PublicIPv4 {
		return Problem("SERVER_IP_LIMIT_REACHED", http.StatusForbidden)
	}
	add := map[string]int{}
	for _, d := range devs {
		size, tier := d.req.Size, d.req.Tier
		if d.template != nil {
			tier = cmpOr(tier, d.template.d.Tier)
			size = max(size, d.template.d.Size)
		}
		add[tier] += size
	}
	for tier, n := range add {
		limit, code := 0, ""
		switch tier {
		case upcloud.StorageTierMaxIOPS:
			limit, code = l.StorageMaxIOPS, "MAXIOPS_STORAGE_LIMIT_REACHED"
		case upcloud.StorageTierStandard:
			limit, code = l.StorageSSD, "STANDARD_STORAGE_LIMIT_REACHED"
		case upcloud.StorageTierHDD:
			limit, code = l.StorageHDD, "HDD_STORAGE_LIMIT_REACHED"
		}
		if limit > 0 && gb[tier]+n > limit {
			return Problem(code, http.StatusForbidden)
		}
	}
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (f *Fake) checkGroup(id string) error {
	g, ok := f.groups[id]
	if !ok {
		return notFound("GROUP_NOT_FOUND")
	}
	if g.g.AntiAffinityPolicy == upcloud.ServerGroupAntiAffinityPolicyStrict && len(g.g.Members) >= f.opts.HostsPerZone {
		return Problem("SERVER_RESOURCES_UNAVAILABLE_FOR_STRICT_AA", http.StatusServiceUnavailable)
	}
	return nil
}

func (f *Fake) commitServer(
	r *request.CreateServerRequest, plan PlanSpec, labels, storageLabels []upcloud.Label, devs []deviceSpec,
	ifaces []ifaceSpec,
) *upcloud.ServerDetails {
	id := f.uuid('s')
	s := &server{creating: true}
	s.d.UUID, s.d.Zone = id, r.Zone
	s.d.Hostname, s.d.Title = r.Hostname, r.Title
	s.d.Plan = plan.Name
	s.d.CoreNumber, s.d.MemoryAmount = plan.CoreNumber, plan.MemoryAmount
	s.d.State = upcloud.ServerStateMaintenance
	s.d.Metadata = r.Metadata
	s.d.Firewall = r.Firewall
	s.d.FirewallPrivate = "off"
	s.d.Labels = slices.Clone(labels)
	s.d.ServerGroup = r.ServerGroup
	s.d.Timezone, s.d.SimpleBackup = r.TimeZone, r.SimpleBackup
	s.d.NICModel, s.d.VideoModel = r.NICModel, r.VideoModel
	s.d.RemoteAccessEnabled = r.RemoteAccessEnabled
	s.d.BootOrder = r.BootOrder
	for i, d := range devs {
		st := f.newStorage(d, r.Zone, plan, i == 0, id)
		st.d.Labels = slices.Clone(storageLabels)
		s.d.StorageDevices = append(s.d.StorageDevices, upcloud.ServerStorageDevice{
			Address: fmt.Sprintf("virtio:%d", i), UUID: st.d.UUID, Size: st.d.Size, Tier: st.d.Tier, Title: st.d.Title,
			Type: upcloud.StorageTypeDisk, Encrypted: st.d.Encrypted, PartOfPlan: st.d.PartOfPlan,
			BootDisk: map[bool]int{true: 1}[i == 0],
		})
	}
	f.attachInterfaces(s, ifaces)
	if r.ServerGroup != "" {
		g := f.groups[r.ServerGroup]
		g.g.Members = append(g.g.Members, id)
	}
	f.servers[id] = s
	s.target = upcloud.ServerStateStarted
	if f.opts.AutoSettle {
		f.settle(s)
	}
	return s.copy()
}

func (f *Fake) newStorage(d deviceSpec, zone string, plan PlanSpec, root bool, serverID string) *storage {
	st := &storage{}
	st.d.UUID = f.uuid('d')
	st.d.Zone, st.d.Access, st.d.Type = zone, upcloud.StorageAccessPrivate, upcloud.StorageTypeDisk
	// In maintenance while the server is created (the clone runs); settle
	// brings it online. The API fills "created" for backups only.
	st.d.State = upcloud.StorageStateMaintenance
	st.d.Title = d.req.Title
	st.d.Encrypted = d.req.Encrypted
	st.d.Size, st.d.Tier = d.req.Size, d.req.Tier
	if d.template != nil {
		st.d.Origin = d.template.d.UUID
		st.d.Size = max(d.req.Size, d.template.d.Size)
		st.d.Tier = cmpOr(d.req.Tier, d.template.d.Tier)
	}
	if root && plan.StorageSize > 0 && st.d.Size <= plan.StorageSize && st.d.Tier == plan.StorageTier {
		st.d.PartOfPlan = "yes"
	}
	st.d.ServerUUIDs = upcloud.ServerUUIDSlice{serverID}
	f.storages[st.d.UUID] = st
	return st
}

func (f *Fake) nextHost() int { f.seq++; return f.seq%250 + 2 }

func (f *Fake) attachInterfaces(s *server, ifaces []ifaceSpec) {
	for i, spec := range ifaces {
		mac := fmt.Sprintf("de:ad:be:ef:00:%02x", i)
		itf := upcloud.ServerInterface{Index: i + 1, Type: spec.typ, Network: spec.network, MAC: mac}
		if spec.typ == upcloud.NetworkTypeUtility {
			itf.Network = "utility-" + s.d.Zone
		}
		if spec.typ == upcloud.NetworkTypePublic {
			itf.Network = "public-" + s.d.Zone
		}
		for _, fam := range spec.ips {
			ip := upcloud.IPAddress{
				Access: spec.typ, Family: fam, ServerUUID: s.d.UUID, MAC: itf.MAC, Zone: s.d.Zone,
				PartOfPlan: upcloud.Empty, ReleasePolicy: upcloud.IPAddressReleasePolicyRelease,
			}
			ip.Address = f.address(spec, fam)
			itf.IPAddresses = append(itf.IPAddresses, ip)
			s.d.IPAddresses = append(s.d.IPAddresses, ip)
		}
		s.d.Networking.Interfaces = append(s.d.Networking.Interfaces, itf)
	}
}

func (f *Fake) address(spec ifaceSpec, family string) string {
	h := f.nextHost()
	switch {
	case spec.typ == upcloud.NetworkTypePrivate:
		if n, ok := f.networks[spec.network]; ok && len(n.n.IPNetworks) > 0 {
			if p, err := netip.ParsePrefix(n.n.IPNetworks[0].Address); err == nil {
				a := p.Masked().Addr().As4()
				a[3] = byte(h)
				return netip.AddrFrom4(a).String()
			}
		}
		return fmt.Sprintf("172.16.0.%d", h)
	case spec.typ == upcloud.NetworkTypeUtility:
		return fmt.Sprintf("10.1.0.%d", h)
	case family == upcloud.IPAddressFamilyIPv6:
		return fmt.Sprintf("2001:db8::%x", h)
	}
	return fmt.Sprintf("203.0.113.%d", h)
}

func (f *Fake) settle(s *server) {
	if s.target != "" {
		s.d.State, s.target = s.target, ""
	}
	s.creating = false
	for _, dev := range s.d.StorageDevices {
		if st, ok := f.storages[dev.UUID]; ok && st.d.State == upcloud.StorageStateMaintenance {
			st.d.State = upcloud.StorageStateOnline
		}
	}
}

// Settle completes every transition in progress (the clone of a new server, a
// stop).
func (f *Fake) Settle() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.servers {
		f.settle(s)
	}
}

// GetServersWithFilters lists the servers. Label filters are supported (request.FilterLabel,
// request.FilterLabelKey); several filters must all match (unverified for the real API).
// The answer holds summaries without labels.
func (f *Fake) GetServersWithFilters(
	ctx context.Context, r *request.GetServersWithFiltersRequest,
) (*upcloud.Servers, error) {
	done, err := f.begin(ctx, "GetServersWithFilters")
	if err != nil {
		return nil, err
	}
	defer done()
	out := &upcloud.Servers{}
	for _, id := range f.sortedServerIDs() {
		s := f.servers[id]
		ok, err := matchLabels(s.d.Labels, r.Filters)
		if err != nil {
			return nil, err
		}
		if ok {
			out.Servers = append(out.Servers, s.summary())
		}
	}
	return out, nil
}

func (f *Fake) sortedServerIDs() []string {
	ids := make([]string, 0, len(f.servers))
	for id := range f.servers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// matchLabels applies label query filters (all must match).
func matchLabels(labels []upcloud.Label, fs []request.QueryFilter) (bool, error) {
	for _, q := range fs {
		switch v := q.(type) {
		case request.FilterLabel:
			if !slices.ContainsFunc(labels, func(l upcloud.Label) bool { return l.Key == v.Key && l.Value == v.Value }) {
				return false, nil
			}
		case request.FilterLabelKey:
			if !slices.ContainsFunc(labels, func(l upcloud.Label) bool { return l.Key == v.Key }) {
				return false, nil
			}
		default:
			return false, invalid("FILTER_INVALID")
		}
	}
	return true, nil
}

// GetServerDetails returns the details; SERVER_NOT_FOUND for a server that is gone.
func (f *Fake) GetServerDetails(
	ctx context.Context, r *request.GetServerDetailsRequest,
) (*upcloud.ServerDetails, error) {
	done, err := f.begin(ctx, "GetServerDetails")
	if err != nil {
		return nil, err
	}
	defer done()
	s, ok := f.servers[r.UUID]
	if !ok {
		return nil, notFound("SERVER_NOT_FOUND")
	}
	return s.copy(), nil
}

// StopServer starts a stop; the server is in maintenance until it settles. A
// server that is not started refuses (SERVER_STATE_ILLEGAL).
func (f *Fake) StopServer(ctx context.Context, r *request.StopServerRequest) (*upcloud.ServerDetails, error) {
	done, err := f.begin(ctx, "StopServer")
	if err != nil {
		return nil, err
	}
	defer done()
	s, ok := f.servers[r.UUID]
	if !ok {
		return nil, notFound("SERVER_NOT_FOUND")
	}
	if s.d.State != upcloud.ServerStateStarted {
		return nil, conflict("SERVER_STATE_ILLEGAL")
	}
	s.d.State, s.target = upcloud.ServerStateMaintenance, upcloud.ServerStateStopped
	if f.opts.AutoSettle {
		f.settle(s)
	}
	return s.copy(), nil
}

// ModifyServer changes labels, title, hostname and the firewall switches.
func (f *Fake) ModifyServer(ctx context.Context, r *request.ModifyServerRequest) (*upcloud.ServerDetails, error) {
	done, err := f.begin(ctx, "ModifyServer")
	if err != nil {
		return nil, err
	}
	defer done()
	s, ok := f.servers[r.UUID]
	if !ok {
		return nil, notFound("SERVER_NOT_FOUND")
	}
	if r.Labels != nil {
		if err := f.checkLabels(*r.Labels); err != nil {
			return nil, err
		}
		s.d.Labels = slices.Clone(*r.Labels)
	}
	if r.Title != "" {
		s.d.Title = r.Title
	}
	if r.Hostname != "" {
		s.d.Hostname = r.Hostname
	}
	if r.Firewall != "" {
		s.d.Firewall = r.Firewall
	}
	if r.FirewallPrivate != "" {
		s.d.FirewallPrivate = r.FirewallPrivate
	}
	return s.copy(), nil
}

// DeleteServerAndStorages deletes a stopped server and all its storages. Any
// other state, a stop in progress included, is SERVER_STATE_ILLEGAL.
func (f *Fake) DeleteServerAndStorages(ctx context.Context, r *request.DeleteServerAndStoragesRequest) error {
	done, err := f.begin(ctx, "DeleteServerAndStorages")
	if err != nil {
		return err
	}
	defer done()
	s, ok := f.servers[r.UUID]
	if !ok {
		return notFound("SERVER_NOT_FOUND")
	}
	if s.d.State != upcloud.ServerStateStopped {
		return conflict("SERVER_STATE_ILLEGAL")
	}
	for _, d := range s.d.StorageDevices {
		delete(f.storages, d.UUID)
	}
	if g, ok := f.groups[s.d.ServerGroup]; ok {
		g.g.Members = slices.DeleteFunc(g.g.Members, func(m string) bool { return m == r.UUID })
	}
	for _, rs := range f.rulesets {
		rs.servers = slices.DeleteFunc(rs.servers, func(m string) bool { return m == r.UUID })
	}
	delete(f.servers, r.UUID)
	return nil
}

// WaitForServerState settles the server's transition and returns its details when
// it is in the desired state (or out of the undesired one); otherwise an error
// (the real call would block until its context ends).
func (f *Fake) WaitForServerState(
	ctx context.Context, r *request.WaitForServerStateRequest,
) (*upcloud.ServerDetails, error) {
	done, err := f.begin(ctx, "WaitForServerState")
	if err != nil {
		return nil, err
	}
	defer done()
	s, ok := f.servers[r.UUID]
	if !ok {
		return nil, notFound("SERVER_NOT_FOUND")
	}
	f.settle(s)
	if (r.DesiredState != "" && s.d.State == r.DesiredState) ||
		(r.UndesiredState != "" && s.d.State != r.UndesiredState) {
		return s.copy(), nil
	}
	return nil, fmt.Errorf("server %s is %s, not %s", r.UUID, s.d.State, r.DesiredState)
}
