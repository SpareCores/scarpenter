/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package fake

import (
	"context"
	"slices"
	"strconv"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// filtered lists the objects of a map that pass the name filter and the label
// selector, sorted by ID. op names the API method for the counters.
func filtered[T any](f *Fake, op string, m map[int64]*T, name string, selector string,
	nameOf func(*T) string, labelsOf func(*T) map[string]string, idOf func(*T) int64, cp func(*T) *T,
) ([]*T, error) {
	sel, err := ParseSelector(selector)
	if err != nil {
		f.calls[op]++
		return nil, APIError(hc.ErrorCodeInvalidInput, err.Error(), "label_selector")
	}
	var out []*T
	for _, o := range m {
		if name != "" && nameOf(o) != name {
			continue
		}
		if !sel.Matches(labelsOf(o)) {
			continue
		}
		out = append(out, cp(o))
	}
	slices.SortFunc(out, func(a, b *T) int { return int(idOf(a) - idOf(b)) })
	if err := f.call(op, pagesFor(len(out))); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- networks (read-only: use only) ----

func copyNetwork(n *hc.Network) *hc.Network {
	c := *n
	c.Labels = cloneMap(n.Labels)
	c.Subnets = slices.Clone(n.Subnets)
	return &c
}

// GetNetwork implements hcloud.API.
func (f *Fake) GetNetwork(_ context.Context, id int64) (*hc.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetNetwork", 1); err != nil {
		return nil, err
	}
	if n := f.networks[id]; n != nil {
		return copyNetwork(n), nil
	}
	return nil, nil
}

// Networks implements hcloud.API.
func (f *Fake) Networks(_ context.Context, o hc.NetworkListOpts) ([]*hc.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filtered(f, "Networks", f.networks, o.Name, o.LabelSelector,
		func(n *hc.Network) string { return n.Name }, func(n *hc.Network) map[string]string { return n.Labels },
		func(n *hc.Network) int64 { return n.ID }, copyNetwork)
}

// ---- firewalls ----

func copyFirewall(x *hc.Firewall) *hc.Firewall {
	c := *x
	c.Labels = cloneMap(x.Labels)
	c.Rules = slices.Clone(x.Rules)
	c.AppliedTo = slices.Clone(x.AppliedTo)
	return &c
}

// GetFirewall implements hcloud.API.
func (f *Fake) GetFirewall(_ context.Context, id int64) (*hc.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetFirewall", 1); err != nil {
		return nil, err
	}
	if x := f.firewalls[id]; x != nil {
		return copyFirewall(x), nil
	}
	return nil, nil
}

// Firewalls implements hcloud.API.
func (f *Fake) Firewalls(_ context.Context, o hc.FirewallListOpts) ([]*hc.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filtered(f, "Firewalls", f.firewalls, o.Name, o.LabelSelector,
		func(x *hc.Firewall) string { return x.Name }, func(x *hc.Firewall) map[string]string { return x.Labels },
		func(x *hc.Firewall) int64 { return x.ID }, copyFirewall)
}

// CreateFirewall implements hcloud.API: names are unique, the count is limited,
// tcp and udp rules need a port (and only they may have one), inbound rules
// need source IPs.
func (f *Fake) CreateFirewall(_ context.Context, o hc.FirewallCreateOpts) (*hc.Firewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateFirewall", 1); err != nil {
		return nil, err
	}
	if len(o.Rules) > 500 {
		return nil, APIError(hc.ErrorCodeInvalidInput, "too many rules", "rules")
	}
	for _, r := range o.Rules {
		tcpUDP := r.Protocol == hc.FirewallRuleProtocolTCP || r.Protocol == hc.FirewallRuleProtocolUDP
		switch {
		case tcpUDP && (r.Port == nil || *r.Port == ""):
			return nil, APIError(hc.ErrorCodeInvalidInput, "port is required for tcp and udp", "rules")
		case !tcpUDP && r.Port != nil:
			return nil, APIError(hc.ErrorCodeInvalidInput, "port is only allowed for tcp and udp", "rules")
		case r.Direction == hc.FirewallRuleDirectionIn && len(r.SourceIPs) == 0:
			return nil, APIError(hc.ErrorCodeInvalidInput, "source_ips is required for inbound rules", "rules")
		}
	}
	for _, x := range f.firewalls {
		if x.Name == o.Name {
			return nil, APIError(hc.ErrorCodeUniquenessError, "firewall name is already used", "name")
		}
	}
	if f.limits.Firewalls > 0 && len(f.firewalls) >= f.limits.Firewalls {
		return nil, APIError(hc.ErrorCodeResourceLimitExceeded, "firewall limit reached")
	}
	x := &hc.Firewall{
		ID: f.id(), Name: o.Name, Labels: cloneMap(o.Labels), Rules: slices.Clone(o.Rules), Created: f.clock.Now(),
	}
	f.firewalls[x.ID] = x
	return copyFirewall(x), nil
}

// DeleteFirewall implements hcloud.API: resource_in_use while applied to a server.
func (f *Fake) DeleteFirewall(_ context.Context, x *hc.Firewall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteFirewall", 1); err != nil {
		return err
	}
	cur := f.firewalls[x.ID]
	if cur == nil {
		return APIError(hc.ErrorCodeNotFound, "firewall not found")
	}
	if len(cur.AppliedTo) > 0 {
		return APIError(hc.ErrorCodeResourceInUse, "firewall is applied to a resource")
	}
	delete(f.firewalls, x.ID)
	return nil
}

// ---- placement groups ----

func copyGroup(g *hc.PlacementGroup) *hc.PlacementGroup {
	c := *g
	c.Labels = cloneMap(g.Labels)
	c.Servers = slices.Clone(g.Servers)
	return &c
}

// GetPlacementGroup implements hcloud.API.
func (f *Fake) GetPlacementGroup(_ context.Context, id int64) (*hc.PlacementGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetPlacementGroup", 1); err != nil {
		return nil, err
	}
	if g := f.groups[id]; g != nil {
		return copyGroup(g), nil
	}
	return nil, nil
}

// PlacementGroups implements hcloud.API.
func (f *Fake) PlacementGroups(_ context.Context, o hc.PlacementGroupListOpts) ([]*hc.PlacementGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filtered(f, "PlacementGroups", f.groups, o.Name, o.LabelSelector,
		func(g *hc.PlacementGroup) string { return g.Name }, func(g *hc.PlacementGroup) map[string]string { return g.Labels },
		func(g *hc.PlacementGroup) int64 { return g.ID }, copyGroup)
}

// CreatePlacementGroup implements hcloud.API.
func (f *Fake) CreatePlacementGroup(_ context.Context, o hc.PlacementGroupCreateOpts) (*hc.PlacementGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreatePlacementGroup", 1); err != nil {
		return nil, err
	}
	if o.Type != hc.PlacementGroupTypeSpread {
		return nil, APIError(hc.ErrorCodeInvalidInput, "type must be spread", "type")
	}
	for _, g := range f.groups {
		if g.Name == o.Name {
			return nil, APIError(hc.ErrorCodeUniquenessError, "placement group name is already used", "name")
		}
	}
	if f.limits.PlacementGroups > 0 && len(f.groups) >= f.limits.PlacementGroups {
		return nil, APIError(hc.ErrorCodeResourceLimitExceeded, "placement group limit reached")
	}
	g := &hc.PlacementGroup{ID: f.id(), Name: o.Name, Labels: cloneMap(o.Labels), Type: o.Type, Created: f.clock.Now()}
	f.groups[g.ID] = g
	return copyGroup(g), nil
}

// DeletePlacementGroup implements hcloud.API: resource_in_use with members.
func (f *Fake) DeletePlacementGroup(_ context.Context, g *hc.PlacementGroup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeletePlacementGroup", 1); err != nil {
		return err
	}
	cur := f.groups[g.ID]
	if cur == nil {
		return APIError(hc.ErrorCodeNotFound, "placement group not found")
	}
	if len(cur.Servers) > 0 {
		return APIError(hc.ErrorCodeResourceInUse, "placement group has servers")
	}
	delete(f.groups, g.ID)
	return nil
}

// ---- volumes ----

func copyVolume(v *hc.Volume) *hc.Volume {
	c := *v
	c.Labels = cloneMap(v.Labels)
	return &c
}

// CreateVolume implements hcloud.API: location-bound, names unique, 10 to
// 10240 GB, never attached by the create here (the server create attaches).
func (f *Fake) CreateVolume(_ context.Context, o hc.VolumeCreateOpts) (*hc.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateVolume", 1); err != nil {
		return nil, err
	}
	if o.Size < 10 || o.Size > 10240 {
		return nil, APIError(hc.ErrorCodeInvalidInput, "size must be 10 to 10240", "size")
	}
	if o.Location == nil || f.findLocation(o.Location.Name) == nil {
		return nil, APIError(hc.ErrorCodeInvalidInput, "location not found", "location")
	}
	for _, v := range f.volumes {
		if v.Name == o.Name {
			return nil, APIError(hc.ErrorCodeUniquenessError, "volume name is already used", "name")
		}
	}
	if f.limits.Volumes > 0 && len(f.volumes) >= f.limits.Volumes {
		return nil, APIError(hc.ErrorCodeResourceLimitExceeded, "volume limit reached")
	}
	v := &hc.Volume{
		ID: f.id(), Name: o.Name, Size: o.Size, Status: hc.VolumeStatusAvailable, Labels: cloneMap(o.Labels),
		Location: f.findLocation(o.Location.Name), Created: f.clock.Now(), Format: o.Format,
	}
	v.LinuxDevice = "/dev/disk/by-id/scsi-0HC_Volume_" + itoa(v.ID)
	f.volumes[v.ID] = v
	return copyVolume(v), nil
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// GetVolume implements hcloud.API.
func (f *Fake) GetVolume(_ context.Context, id int64) (*hc.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetVolume", 1); err != nil {
		return nil, err
	}
	if v := f.volumes[id]; v != nil {
		return copyVolume(v), nil
	}
	return nil, nil
}

// Volumes implements hcloud.API.
func (f *Fake) Volumes(_ context.Context, o hc.VolumeListOpts) ([]*hc.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filtered(f, "Volumes", f.volumes, o.Name, o.LabelSelector,
		func(v *hc.Volume) string { return v.Name }, func(v *hc.Volume) map[string]string { return v.Labels },
		func(v *hc.Volume) int64 { return v.ID }, copyVolume)
}

// DeleteVolume implements hcloud.API: resource_in_use while attached.
func (f *Fake) DeleteVolume(_ context.Context, v *hc.Volume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteVolume", 1); err != nil {
		return err
	}
	cur := f.volumes[v.ID]
	if cur == nil {
		return APIError(hc.ErrorCodeNotFound, "volume not found")
	}
	if cur.Server != nil {
		return APIError(hc.ErrorCodeResourceInUse, "volume is attached to a server")
	}
	delete(f.volumes, v.ID)
	return nil
}

// ---- Primary IPs ----

// GetPrimaryIP implements hcloud.API.
func (f *Fake) GetPrimaryIP(_ context.Context, id int64) (*hc.PrimaryIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetPrimaryIP", 1); err != nil {
		return nil, err
	}
	if ip := f.ips[id]; ip != nil {
		c := *ip
		return &c, nil
	}
	return nil, nil
}

// DeletePrimaryIP implements hcloud.API: an assigned IP cannot be deleted.
func (f *Fake) DeletePrimaryIP(_ context.Context, ip *hc.PrimaryIP) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeletePrimaryIP", 1); err != nil {
		return err
	}
	cur := f.ips[ip.ID]
	if cur == nil {
		return APIError(hc.ErrorCodeNotFound, "primary ip not found")
	}
	if cur.AssigneeID != 0 {
		return APIError(hc.ErrorCodePrimaryIPAssigned, "primary ip is assigned")
	}
	delete(f.ips, ip.ID)
	return nil
}
