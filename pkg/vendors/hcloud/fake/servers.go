/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package fake

import (
	"context"
	"net"
	"regexp"
	"slices"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// MaxUserData is the API's user-data limit.
const MaxUserData = 32 * 1024

// MaxNetworkAttachments is the limit of resources attached to one network.
const MaxNetworkAttachments = 100

// MaxSpread is the size of a spread placement group.
const MaxSpread = 10

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func copyServer(s *hc.Server) *hc.Server {
	c := *s
	c.Labels = cloneMap(s.Labels)
	c.Volumes = slices.Clone(s.Volumes)
	c.PrivateNet = slices.Clone(s.PrivateNet)
	if s.ServerType != nil {
		t := *s.ServerType
		c.ServerType = &t
	}
	if s.Location != nil {
		l := *s.Location
		c.Location = &l
	}
	return &c
}

func (f *Fake) serverByName(name string) *hc.Server {
	for _, s := range f.servers {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func (f *Fake) newAction(command string, apply func()) *hc.Action {
	a := &hc.Action{ID: f.id(), Status: hc.ActionStatusRunning, Command: command, Started: f.clock.Now()}
	f.actions[a.ID] = &action{a: a, apply: apply}
	c := *a
	return &c
}

// createPlan is a validated create: the objects the request names.
type createPlan struct {
	st             *hc.ServerType
	loc            *hc.Location
	im             *hc.Image
	nw             *hc.Network
	fws            []*hc.Firewall
	pg             *hc.PlacementGroup
	vols           []*hc.Volume
	wantV4, wantV6 bool
}

func invalid(msg, field string) error { return APIError(hc.ErrorCodeInvalidInput, msg, field) }

// validateCreate checks a create request the way the API does and resolves the
// objects it names (the caller holds the lock).
func (f *Fake) validateCreate(o hc.ServerCreateOpts) (*createPlan, error) {
	if o.Name == "" || len(o.Name) > 63 || !hostnameRE.MatchString(o.Name) {
		return nil, invalid("invalid name", "name")
	}
	if f.serverByName(o.Name) != nil {
		return nil, APIError(hc.ErrorCodeUniquenessError, "server name is already used", "name")
	}
	if len(o.UserData) > MaxUserData {
		return nil, invalid("user_data is too long", "user_data")
	}
	if o.ServerType == nil || o.Location == nil || o.Image == nil {
		return nil, invalid("server_type, location and image are required", "server_type")
	}
	pl := &createPlan{st: f.findType(o.ServerType.Name), loc: f.findLocation(o.Location.Name), im: f.images[o.Image.ID]}
	switch {
	case pl.st == nil:
		return nil, invalid("server type not found", "server_type")
	case pl.loc == nil:
		return nil, invalid("location not found", "location")
	}
	if err := f.checkStock(pl); err != nil {
		return nil, err
	}
	if pl.im == nil {
		return nil, invalid("image not found", "image")
	}
	if pl.im.Architecture != pl.st.Architecture {
		return nil, invalid("image architecture does not match the server type", "image")
	}
	if err := f.checkLimits(o, pl); err != nil {
		return nil, err
	}
	return pl, f.resolveAttachments(o, pl)
}

// checkStock applies the type's entry for the location: not supported there,
// past its deprecation, or temporarily out of stock.
func (f *Fake) checkStock(pl *createPlan) error {
	var entry *hc.ServerTypeLocation
	for i := range pl.st.Locations {
		if pl.st.Locations[i].Location.Name == pl.loc.Name {
			entry = &pl.st.Locations[i]
		}
	}
	if entry == nil {
		return invalid("server type is not supported in the location", "server_type")
	}
	if d := entry.Deprecation; d != nil && !d.UnavailableAfter.IsZero() && !f.clock.Now().Before(d.UnavailableAfter) {
		return APIError(hc.ErrorCodeInvalidServerType, "server type is deprecated")
	}
	if !entry.Available {
		return APIError(hc.ErrorCodeResourceUnavailable, "the server type is currently unavailable in the location")
	}
	return nil
}

func (f *Fake) checkLimits(o hc.ServerCreateOpts, pl *createPlan) error {
	if f.limits.Servers > 0 && len(f.servers) >= f.limits.Servers {
		return APIError(hc.ErrorCodeResourceLimitExceeded, "server limit reached")
	}
	pl.wantV4 = o.PublicNet == nil || o.PublicNet.EnableIPv4
	pl.wantV6 = o.PublicNet == nil || o.PublicNet.EnableIPv6
	if need := b2i(pl.wantV4) + b2i(pl.wantV6); f.limits.PrimaryIPs > 0 && len(f.ips)+need > f.limits.PrimaryIPs {
		return APIError(hc.ErrorCodeResourceLimitExceeded, "primary_ip limit reached")
	}
	return nil
}

// resolveAttachments finds the network, firewalls, placement group, volumes and
// SSH keys of the request and applies their constraints.
func (f *Fake) resolveAttachments(o hc.ServerCreateOpts, pl *createPlan) error {
	if len(o.Networks) > 0 {
		pl.nw = f.networks[o.Networks[0].ID]
		if pl.nw == nil {
			return invalid("network not found", "networks")
		}
		if !slices.ContainsFunc(pl.nw.Subnets, func(s hc.NetworkSubnet) bool {
			return s.Type == hc.NetworkSubnetTypeCloud && s.NetworkZone == pl.loc.NetworkZone
		}) {
			return APIError(hc.ErrorCodeNoSubnetAvailable, "no subnet in the network zone of the location")
		}
		if f.attachments(pl.nw.ID) >= MaxNetworkAttachments {
			return APIError(hc.ErrorCodeIPNotAvailable, "the network is full")
		}
	}
	for _, w := range o.Firewalls {
		fw := f.firewalls[w.Firewall.ID]
		if fw == nil {
			return invalid("firewall not found", "firewalls")
		}
		pl.fws = append(pl.fws, fw)
	}
	if o.PlacementGroup != nil {
		pl.pg = f.groups[o.PlacementGroup.ID]
		if pl.pg == nil {
			return invalid("placement group not found", "placement_group")
		}
		if len(pl.pg.Servers) >= MaxSpread {
			return APIError(hc.ErrorCodePlacementError, "the placement group is full")
		}
	}
	for _, v := range o.Volumes {
		vol := f.volumes[v.ID]
		if vol == nil {
			return invalid("volume not found", "volumes")
		}
		if vol.Location == nil || vol.Location.Name != pl.loc.Name || vol.Server != nil {
			return invalid("volume is in another location or already attached", "volumes")
		}
		pl.vols = append(pl.vols, vol)
	}
	for _, k := range o.SSHKeys {
		if f.sshKeys[k.ID] == nil {
			return invalid("ssh key not found", "ssh_keys")
		}
	}
	return nil
}

// CreateServer implements hcloud.API with the validations the plan relies on:
// unique name (uniqueness_error), type and location (invalid_input fields),
// stock (resource_unavailable), architecture of the image (invalid_input
// image), project limits (resource_limit_exceeded), the network's subnet
// (no_subnet_available, ip_not_available), spread groups (placement_error),
// volumes (location-bound, attached at create).
func (f *Fake) CreateServer(_ context.Context, o hc.ServerCreateOpts) (hc.ServerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res hc.ServerCreateResult
	if err := f.call("CreateServer", 1); err != nil {
		return res, err
	}
	pl, err := f.validateCreate(o)
	if err != nil {
		return res, err
	}
	s := f.build(o, pl)
	f.servers[s.ID] = s
	start := o.StartAfterCreate == nil || *o.StartAfterCreate
	res.Server = copyServer(s)
	res.Action = f.newAction("create_server", func() {
		if start {
			s.Status = hc.ServerStatusRunning
		} else {
			s.Status = hc.ServerStatusOff
		}
	})
	if f.failCreate != nil {
		f.actions[res.Action.ID].failed = f.failCreate
	}
	return res, nil
}

// build creates the server and wires it to the validated objects.
func (f *Fake) build(o hc.ServerCreateOpts, pl *createPlan) *hc.Server {
	s := &hc.Server{
		ID: f.id(), Name: o.Name, Status: hc.ServerStatusInitializing, Created: f.clock.Now(),
		ServerType: copyType(pl.st), Location: pl.loc, Labels: cloneMap(o.Labels), PrimaryDiskSize: pl.st.Disk,
		Image: pl.im,
	}
	if pl.wantV4 {
		ip := f.newPrimaryIP(hc.PrimaryIPTypeIPv4, s, pl.loc)
		s.PublicNet.IPv4 = hc.ServerPublicNetIPv4{ID: ip.ID, IP: ip.IP}
	}
	if pl.wantV6 {
		ip := f.newPrimaryIP(hc.PrimaryIPTypeIPv6, s, pl.loc)
		s.PublicNet.IPv6 = hc.ServerPublicNetIPv6{ID: ip.ID, IP: ip.IP, Network: ip.Network}
	}
	if pl.nw != nil {
		n := f.attachments(pl.nw.ID) + 2
		base := pl.nw.IPRange.IP.To4()
		ip := net.IPv4(base[0], base[1], base[2]+byte(n/250), base[3]+byte(n%250))
		s.PrivateNet = []hc.ServerPrivateNet{{Network: &hc.Network{ID: pl.nw.ID}, IP: ip}}
	}
	for _, fw := range pl.fws {
		fw.AppliedTo = append(fw.AppliedTo, hc.FirewallResource{
			Type: hc.FirewallResourceTypeServer, Server: &hc.FirewallResourceServer{ID: s.ID},
		})
		s.PublicNet.Firewalls = append(s.PublicNet.Firewalls, &hc.ServerFirewallStatus{
			Firewall: hc.Firewall{ID: fw.ID}, Status: hc.FirewallStatusApplied,
		})
	}
	if pl.pg != nil {
		pl.pg.Servers = append(pl.pg.Servers, s.ID)
		s.PlacementGroup = &hc.PlacementGroup{ID: pl.pg.ID, Name: pl.pg.Name, Type: pl.pg.Type}
	}
	for _, v := range pl.vols {
		v.Server = &hc.Server{ID: s.ID}
		s.Volumes = append(s.Volumes, &hc.Volume{ID: v.ID})
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (f *Fake) attachments(netID int64) int {
	n := 0
	for _, s := range f.servers {
		for _, pn := range s.PrivateNet {
			if pn.Network != nil && pn.Network.ID == netID {
				n++
			}
		}
	}
	return n
}

func (f *Fake) newPrimaryIP(t hc.PrimaryIPType, s *hc.Server, loc *hc.Location) *hc.PrimaryIP {
	ip := &hc.PrimaryIP{
		ID: f.id(), Type: t, AssigneeID: s.ID, AssigneeType: "server", AutoDelete: f.ipAutoDelete,
		Location: loc, Created: f.clock.Now(), Name: "primary_ip-" + s.Name, Labels: map[string]string{},
	}
	if t == hc.PrimaryIPTypeIPv4 {
		ip.IP = ipv4For(ip.ID)
	} else {
		n := ipv6NetFor(ip.ID)
		ip.IP, ip.Network = n.IP, n
	}
	f.ips[ip.ID] = ip
	return ip
}

// ProtectServer sets delete protection (a delete then fails with protected).
func (f *Fake) ProtectServer(id int64, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.servers[id]; s != nil {
		s.Protection.Delete = on
	}
}

// GetServer implements hcloud.API; nil for an unknown server.
func (f *Fake) GetServer(_ context.Context, id int64) (*hc.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetServer", 1); err != nil {
		return nil, err
	}
	if s := f.servers[id]; s != nil {
		return copyServer(s), nil
	}
	return nil, nil
}

// Servers implements hcloud.API: the label selector, the name and the status
// filters, paged at 50.
func (f *Fake) Servers(_ context.Context, o hc.ServerListOpts) ([]*hc.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sel, err := ParseSelector(o.LabelSelector)
	if err != nil {
		f.calls["Servers"]++
		return nil, APIError(hc.ErrorCodeInvalidInput, err.Error(), "label_selector")
	}
	var out []*hc.Server
	for _, s := range f.servers {
		if o.Name != "" && s.Name != o.Name {
			continue
		}
		if len(o.Status) > 0 && !slices.Contains(o.Status, s.Status) {
			continue
		}
		if !sel.Matches(s.Labels) {
			continue
		}
		out = append(out, copyServer(s))
	}
	slices.SortFunc(out, func(a, b *hc.Server) int { return int(a.ID - b.ID) })
	if err := f.call("Servers", pagesFor(len(out))); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteServer implements hcloud.API: the server goes to deleting; the action
// completes (and the server disappears) when waited for or at the next Tick.
func (f *Fake) DeleteServer(_ context.Context, s *hc.Server) (*hc.Action, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteServer", 1); err != nil {
		return nil, err
	}
	cur := f.servers[s.ID]
	if cur == nil {
		return nil, APIError(hc.ErrorCodeNotFound, "server not found")
	}
	if cur.Protection.Delete {
		return nil, APIError(hc.ErrorCodeProtected, "server deletion is protected")
	}
	cur.Status = hc.ServerStatusDeleting
	return f.newAction("delete_server", func() { f.removeServer(cur) }), nil
}

// removeServer releases everything the server held: volumes stay (detached),
// firewalls and groups forget it, Primary IPs are unassigned and, with
// auto_delete, deleted.
func (f *Fake) removeServer(s *hc.Server) {
	delete(f.servers, s.ID)
	for _, v := range f.volumes {
		if v.Server != nil && v.Server.ID == s.ID {
			v.Server = nil
		}
	}
	for _, fw := range f.firewalls {
		fw.AppliedTo = slices.DeleteFunc(fw.AppliedTo, func(r hc.FirewallResource) bool {
			return r.Server != nil && r.Server.ID == s.ID
		})
	}
	for _, g := range f.groups {
		g.Servers = slices.DeleteFunc(g.Servers, func(id int64) bool { return id == s.ID })
	}
	for _, ip := range f.ips {
		if ip.AssigneeID == s.ID {
			ip.AssigneeID, ip.AssigneeType = 0, ""
			if ip.AutoDelete {
				delete(f.ips, ip.ID)
			}
		}
	}
}

// WaitForAction implements hcloud.API: it completes the action (applying its
// effect) or returns its error.
func (f *Fake) WaitForAction(_ context.Context, a *hc.Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("WaitForAction", 1); err != nil {
		return err
	}
	act := f.actions[a.ID]
	if act == nil {
		return nil
	}
	if act.a.Status == hc.ActionStatusSuccess {
		return nil
	}
	if act.failed != nil {
		act.a.Status = hc.ActionStatusError
		return hc.ActionError{Code: act.failed.ErrorCode, Message: act.failed.ErrorMessage}
	}
	if act.apply != nil {
		act.apply()
	}
	act.a.Status, act.a.Progress, act.a.Finished = hc.ActionStatusSuccess, 100, f.clock.Now()
	return nil
}
