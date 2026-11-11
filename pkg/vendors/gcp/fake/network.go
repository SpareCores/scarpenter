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
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// AddDefaultNetwork adds the auto-mode network "default" with one subnetwork
// named "default" in every region added so far (10.128.0.0/20, stepping by
// 16 in the second octet block per region). A fake without it has no default
// network, like a project under constraints/compute.skipDefaultNetworkCreation.
func (c *Compute) AddDefaultNetwork() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.networks["default"] = &computepb.Network{
		Name: new("default"), Id: new(c.seq), AutoCreateSubnetworks: new(true),
		SelfLink: new(c.url("global/networks/default")), Description: new("Default network for the project"),
	}
	for i, r := range sortedKeys(c.regions) {
		cidr := fmt.Sprintf("10.%d.0.0/20", 128+i)
		c.seq++
		c.subnets[r+"/default"] = &computepb.Subnetwork{
			Name: new("default"), Id: new(c.seq), Region: new(c.url("regions/" + r)),
			Network: new(c.url("global/networks/default")), IpCidrRange: new(cidr),
			StackType: new("IPV4_ONLY"), State: new("READY"),
			SelfLink: new(c.url("regions/" + r + "/subnetworks/default")),
		}
	}
}

// ----- networks -----

func (c *Compute) subnetsOf(network string) []string {
	var out []string
	for _, k := range sortedKeys(c.subnets) {
		if last(c.subnets[k].GetNetwork()) == network {
			out = append(out, c.subnets[k].GetSelfLink())
		}
	}
	return out
}

func (c *Compute) GetNetwork(_ context.Context, r *computepb.GetNetworkRequest) (*computepb.Network, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetNetwork"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	n := c.networks[r.GetNetwork()]
	if n == nil {
		return nil, notFound("projects/" + r.GetProject() + "/global/networks/" + r.GetNetwork())
	}
	out := proto.Clone(n).(*computepb.Network)
	out.Subnetworks = c.subnetsOf(n.GetName())
	return out, nil
}

func (c *Compute) ListNetworks(_ context.Context, r *computepb.ListNetworksRequest) (*computepb.NetworkList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListNetworks"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	var all []*computepb.Network
	for _, k := range sortedKeys(c.networks) {
		if ok, err := matchFilter(r.Filter, nameValues(k)); err != nil {
			return nil, err
		} else if ok {
			n := proto.Clone(c.networks[k]).(*computepb.Network)
			n.Subnetworks = c.subnetsOf(k)
			all = append(all, n)
		}
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.NetworkList{Items: all[lo:hi], NextPageToken: next}, nil
}

func (c *Compute) InsertNetwork(_ context.Context, r *computepb.InsertNetworkRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("InsertNetwork"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	if op := c.replay("InsertNetwork", "global", r.RequestId); op != nil {
		return op, nil
	}
	in := proto.Clone(r.GetNetworkResource()).(*computepb.Network)
	name := in.GetName()
	if name == "" {
		return nil, invalid("Invalid value for field 'resource.name': ''")
	}
	if c.networks[name] != nil {
		return nil, Error(http.StatusConflict, "alreadyExists",
			fmt.Sprintf("The resource 'projects/%s/global/networks/%s' already exists", r.GetProject(), name))
	}
	c.seq++
	in.Id = new(c.seq)
	in.SelfLink = new(c.url("global/networks/" + name))
	in.CreationTimestamp = new(c.timestamp())
	c.networks[name] = in
	op := c.newOp("insert", "global", "global/networks/"+name, c.takeOpFailure("InsertNetwork"), nil)
	c.remember("InsertNetwork", "global", r.RequestId, op)
	return proto.Clone(op).(*computepb.Operation), nil
}

func (c *Compute) DeleteNetwork(_ context.Context, r *computepb.DeleteNetworkRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("DeleteNetwork"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	name := r.GetNetwork()
	if c.networks[name] == nil {
		return nil, notFound("projects/" + r.GetProject() + "/global/networks/" + name)
	}
	if subs := c.subnetsOf(name); len(subs) > 0 {
		return nil, inUse("The network resource 'projects/%s/global/networks/%s' is already being used by '%s'",
			r.GetProject(), name, subs[0])
	}
	for _, k := range sortedKeys(c.firewalls) {
		if last(c.firewalls[k].GetNetwork()) == name {
			return nil, inUse("The network resource 'projects/%s/global/networks/%s' is already being used by "+
				"'projects/%s/global/firewalls/%s'", r.GetProject(), name, r.GetProject(), k)
		}
	}
	delete(c.networks, name)
	return c.simpleOp("global", "global/networks/"+name, "DeleteNetwork"), nil
}

// simpleOp registers a delete operation for a call whose effect is already applied.
func (c *Compute) simpleOp(scope, target, method string) *computepb.Operation {
	return proto.Clone(c.newOp("delete", scope, target, c.takeOpFailure(method), nil)).(*computepb.Operation)
}

// ----- subnetworks -----

func (c *Compute) GetSubnetwork(_ context.Context, r *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetSubnetwork"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	s := c.subnets[r.GetRegion()+"/"+r.GetSubnetwork()]
	if s == nil {
		return nil, notFound(fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", r.GetProject(), r.GetRegion(),
			r.GetSubnetwork()))
	}
	return proto.Clone(s).(*computepb.Subnetwork), nil
}

func (c *Compute) ListSubnetworks(
	_ context.Context, r *computepb.ListSubnetworksRequest,
) (*computepb.SubnetworkList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListSubnetworks"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	if c.regions[r.GetRegion()] == nil {
		return nil, notFound("projects/" + r.GetProject() + "/regions/" + r.GetRegion())
	}
	var all []*computepb.Subnetwork
	for _, k := range sortedKeys(c.subnets) {
		region, name, _ := strings.Cut(k, "/")
		if region != r.GetRegion() {
			continue
		}
		s := c.subnets[k]
		ok, err := matchFilter(r.Filter, func(f string) ([]string, bool) {
			switch f {
			case fieldName:
				return []string{name}, true
			case "network":
				return []string{s.GetNetwork()}, true
			}
			return nil, false
		})
		if err != nil {
			return nil, err
		}
		if ok {
			all = append(all, proto.Clone(s).(*computepb.Subnetwork))
		}
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.SubnetworkList{Items: all[lo:hi], NextPageToken: next}, nil
}

func (c *Compute) InsertSubnetwork(
	_ context.Context, r *computepb.InsertSubnetworkRequest,
) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("InsertSubnetwork"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	scope := "regions/" + r.GetRegion()
	if op := c.replay("InsertSubnetwork", scope, r.RequestId); op != nil {
		return op, nil
	}
	if c.regions[r.GetRegion()] == nil {
		return nil, notFound("projects/" + r.GetProject() + "/regions/" + r.GetRegion())
	}
	in := proto.Clone(r.GetSubnetworkResource()).(*computepb.Subnetwork)
	name := in.GetName()
	if name == "" {
		return nil, invalid("Invalid value for field 'resource.name': ''")
	}
	key := r.GetRegion() + "/" + name
	if c.subnets[key] != nil {
		return nil, Error(http.StatusConflict, "alreadyExists",
			fmt.Sprintf("The resource 'projects/%s/regions/%s/subnetworks/%s' already exists", r.GetProject(),
				r.GetRegion(), name))
	}
	network := last(in.GetNetwork())
	if c.networks[network] == nil {
		return nil, notFound(in.GetNetwork())
	}
	p, err := netip.ParsePrefix(in.GetIpCidrRange())
	if err != nil {
		return nil, invalid("Invalid IPCidrRange: %s", in.GetIpCidrRange())
	}
	for _, k := range sortedKeys(c.subnets) {
		o := c.subnets[k]
		if last(o.GetNetwork()) != network {
			continue
		}
		// Primary and secondary ranges share one space inside a network.
		ranges := []string{o.GetIpCidrRange()}
		for _, sec := range o.GetSecondaryIpRanges() {
			ranges = append(ranges, sec.GetIpCidrRange())
		}
		for _, rg := range ranges {
			if op, err := netip.ParsePrefix(rg); err == nil && op.Overlaps(p) {
				return nil, invalid("Invalid IPCidrRange: %s conflicts with existing subnetwork '%s' in region '%s'.", p,
					o.GetName(), last(o.GetRegion()))
			}
		}
	}
	c.seq++
	in.Id = new(c.seq)
	in.Region = new(c.url(scope))
	in.Network = new(c.url("global/networks/" + network))
	in.SelfLink = new(c.url(scope + "/subnetworks/" + name))
	in.State = new("READY")
	in.CreationTimestamp = new(c.timestamp())
	if in.StackType == nil {
		in.StackType = new("IPV4_ONLY")
	}
	c.subnets[key] = in
	op := c.newOp("insert", scope, scope+"/subnetworks/"+name, c.takeOpFailure("InsertSubnetwork"), nil)
	c.remember("InsertSubnetwork", scope, r.RequestId, op)
	return proto.Clone(op).(*computepb.Operation), nil
}

func (c *Compute) DeleteSubnetwork(
	_ context.Context, r *computepb.DeleteSubnetworkRequest,
) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("DeleteSubnetwork"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	key := r.GetRegion() + "/" + r.GetSubnetwork()
	if c.subnets[key] == nil {
		return nil, notFound(fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", r.GetProject(), r.GetRegion(),
			r.GetSubnetwork()))
	}
	for _, k := range sortedKeys(c.instances) {
		in := c.instances[k]
		for _, nic := range in.GetNetworkInterfaces() {
			if regionOf(last(in.GetZone())) == r.GetRegion() && last(nic.GetSubnetwork()) == r.GetSubnetwork() {
				return nil, inUse("The subnetwork resource 'projects/%s/regions/%s/subnetworks/%s' is already being used by "+
					"'projects/%s/zones/%s/instances/%s'",
					r.GetProject(), r.GetRegion(), r.GetSubnetwork(), r.GetProject(), last(in.GetZone()), in.GetName())
			}
		}
	}
	delete(c.subnets, key)
	delete(c.usedIPs, key)
	return c.simpleOp("regions/"+r.GetRegion(), "regions/"+r.GetRegion()+"/subnetworks/"+r.GetSubnetwork(),
		"DeleteSubnetwork"), nil
}

// ----- firewalls -----

func (c *Compute) ListFirewalls(_ context.Context, r *computepb.ListFirewallsRequest) (*computepb.FirewallList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListFirewalls"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	var all []*computepb.Firewall
	for _, k := range sortedKeys(c.firewalls) {
		if ok, err := matchFilter(r.Filter, nameValues(k)); err != nil {
			return nil, err
		} else if ok {
			all = append(all, proto.Clone(c.firewalls[k]).(*computepb.Firewall))
		}
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.FirewallList{Items: all[lo:hi], NextPageToken: next}, nil
}

func (c *Compute) InsertFirewall(_ context.Context, r *computepb.InsertFirewallRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("InsertFirewall"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	if op := c.replay("InsertFirewall", "global", r.RequestId); op != nil {
		return op, nil
	}
	in := proto.Clone(r.GetFirewallResource()).(*computepb.Firewall)
	name := in.GetName()
	if name == "" {
		return nil, invalid("Invalid value for field 'resource.name': ''")
	}
	if c.firewalls[name] != nil {
		return nil, Error(http.StatusConflict, "alreadyExists",
			fmt.Sprintf("The resource 'projects/%s/global/firewalls/%s' already exists", r.GetProject(), name))
	}
	if c.networks[last(in.GetNetwork())] == nil {
		return nil, notFound(in.GetNetwork())
	}
	c.seq++
	in.Id = new(c.seq)
	in.SelfLink = new(c.url("global/firewalls/" + name))
	in.Network = new(c.url("global/networks/" + last(in.GetNetwork())))
	in.CreationTimestamp = new(c.timestamp())
	c.firewalls[name] = in
	op := c.newOp("insert", "global", "global/firewalls/"+name, c.takeOpFailure("InsertFirewall"), nil)
	c.remember("InsertFirewall", "global", r.RequestId, op)
	return proto.Clone(op).(*computepb.Operation), nil
}

func (c *Compute) DeleteFirewall(_ context.Context, r *computepb.DeleteFirewallRequest) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("DeleteFirewall"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	if c.firewalls[r.GetFirewall()] == nil {
		return nil, notFound("projects/" + r.GetProject() + "/global/firewalls/" + r.GetFirewall())
	}
	delete(c.firewalls, r.GetFirewall())
	return c.simpleOp("global", "global/firewalls/"+r.GetFirewall(), "DeleteFirewall"), nil
}

// ----- resource policies -----

func (c *Compute) GetResourcePolicy(
	_ context.Context, r *computepb.GetResourcePolicyRequest,
) (*computepb.ResourcePolicy, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("GetResourcePolicy"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	p := c.policies[r.GetRegion()+"/"+r.GetResourcePolicy()]
	if p == nil {
		return nil, notFound(fmt.Sprintf("projects/%s/regions/%s/resourcePolicies/%s", r.GetProject(), r.GetRegion(),
			r.GetResourcePolicy()))
	}
	return proto.Clone(p).(*computepb.ResourcePolicy), nil
}

func (c *Compute) ListResourcePolicies(
	_ context.Context, r *computepb.ListResourcePoliciesRequest,
) (*computepb.ResourcePolicyList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("ListResourcePolicies"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	if c.regions[r.GetRegion()] == nil {
		return nil, notFound("projects/" + r.GetProject() + "/regions/" + r.GetRegion())
	}
	var all []*computepb.ResourcePolicy
	for _, k := range sortedKeys(c.policies) {
		region, name, _ := strings.Cut(k, "/")
		if region != r.GetRegion() {
			continue
		}
		if ok, err := matchFilter(r.Filter, nameValues(name)); err != nil {
			return nil, err
		} else if ok {
			all = append(all, proto.Clone(c.policies[k]).(*computepb.ResourcePolicy))
		}
	}
	lo, hi, next, err := pageBounds(len(all), r.MaxResults, r.PageToken)
	if err != nil {
		return nil, err
	}
	return &computepb.ResourcePolicyList{Items: all[lo:hi], NextPageToken: next}, nil
}

func (c *Compute) InsertResourcePolicy(
	_ context.Context, r *computepb.InsertResourcePolicyRequest,
) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("InsertResourcePolicy"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	scope := "regions/" + r.GetRegion()
	if op := c.replay("InsertResourcePolicy", scope, r.RequestId); op != nil {
		return op, nil
	}
	if c.regions[r.GetRegion()] == nil {
		return nil, notFound("projects/" + r.GetProject() + "/regions/" + r.GetRegion())
	}
	in := proto.Clone(r.GetResourcePolicyResource()).(*computepb.ResourcePolicy)
	name := in.GetName()
	if name == "" {
		return nil, invalid("Invalid value for field 'resource.name': ''")
	}
	key := r.GetRegion() + "/" + name
	if c.policies[key] != nil {
		return nil, Error(http.StatusConflict, "alreadyExists",
			fmt.Sprintf("The resource 'projects/%s/regions/%s/resourcePolicies/%s' already exists", r.GetProject(),
				r.GetRegion(), name))
	}
	c.seq++
	in.Id = new(c.seq)
	in.Region = new(c.url(scope))
	in.SelfLink = new(c.url(scope + "/resourcePolicies/" + name))
	in.Status = new("READY")
	in.CreationTimestamp = new(c.timestamp())
	c.policies[key] = in
	op := c.newOp("insert", scope, scope+"/resourcePolicies/"+name, c.takeOpFailure("InsertResourcePolicy"), nil)
	c.remember("InsertResourcePolicy", scope, r.RequestId, op)
	return proto.Clone(op).(*computepb.Operation), nil
}

func (c *Compute) DeleteResourcePolicy(
	_ context.Context, r *computepb.DeleteResourcePolicyRequest,
) (*computepb.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.begin("DeleteResourcePolicy"); err != nil {
		return nil, err
	}
	if err := c.checkProject(r.GetProject()); err != nil {
		return nil, err
	}
	key := r.GetRegion() + "/" + r.GetResourcePolicy()
	if c.policies[key] == nil {
		return nil, notFound(fmt.Sprintf("projects/%s/regions/%s/resourcePolicies/%s", r.GetProject(), r.GetRegion(),
			r.GetResourcePolicy()))
	}
	for _, k := range sortedKeys(c.instances) {
		in := c.instances[k]
		if regionOf(last(in.GetZone())) != r.GetRegion() {
			continue
		}
		for _, p := range in.GetResourcePolicies() {
			if last(p) == r.GetResourcePolicy() {
				return nil, inUse("The resource_policy resource 'projects/%s/regions/%s/resourcePolicies/%s' "+
					"is already being used by 'projects/%s/zones/%s/instances/%s'",
					r.GetProject(), r.GetRegion(), r.GetResourcePolicy(), r.GetProject(), last(in.GetZone()), in.GetName())
			}
		}
	}
	delete(c.policies, key)
	return c.simpleOp("regions/"+r.GetRegion(), "regions/"+r.GetRegion()+"/resourcePolicies/"+r.GetResourcePolicy(),
		"DeleteResourcePolicy"), nil
}

// seedNetwork adds the custom network "vpc" and the subnet "nodes".
func (c *Compute) seedNetwork() {
	c.seq++
	c.networks["vpc"] = &computepb.Network{
		Name: new("vpc"), Id: new(c.seq), AutoCreateSubnetworks: new(false),
		SelfLink: new(c.url("global/networks/vpc")),
	}
	c.seq++
	c.subnets[SeedRegion+"/"+SeedSubnet] = &computepb.Subnetwork{
		Name: new(SeedSubnet), Id: new(c.seq), Region: new(c.url("regions/" + SeedRegion)),
		Network: new(c.url("global/networks/vpc")), IpCidrRange: new(SeedSubnetCIDR),
		StackType: new("IPV4_IPV6"), State: new("READY"),
		SelfLink: new(c.url("regions/" + SeedRegion + "/subnetworks/" + SeedSubnet)),
	}
}

// Firewalls returns a snapshot of every firewall rule, sorted by name.
func (c *Compute) Firewalls() []*computepb.Firewall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*computepb.Firewall, 0, len(c.firewalls))
	for _, k := range sortedKeys(c.firewalls) {
		out = append(out, proto.Clone(c.firewalls[k]).(*computepb.Firewall))
	}
	return out
}
