/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// DefaultVCNCIDR is the range of the VCN SCarpenter creates, unless the NodeClass's
// resources.subnets.create.cidr says another. The one regional subnet is the first /20 of it.
const DefaultVCNCIDR = "10.240.0.0/16"

// SubnetParams are the create parameters of the managed network (Params of the ResourceRequest,
// which the resource engine hashes into the VCN's name: another range is another network).
type SubnetParams struct {
	CIDR string `json:"cidr,omitempty"`
}

// GroupParams are the create parameters of a network security group (hashed into its name: a
// changed rule set is a new group, the old one is collected by its lifecycle).
type GroupParams struct {
	// VCN is the VCN the group lives in: the subnet's.
	VCN string `json:"vcnId"`
	// SelfReference allows all traffic between the members of the group: the rule of isolation
	// nodeclass (the NodeClass's group) and of isolation run (the run's group).
	SelfReference bool `json:"selfReference,omitempty"`
	// Inbound are the rules of spec.networking.inbound.
	Inbound []common.InboundRule `json:"inbound,omitempty"`
}

// subnetCIDR is the first /20 of the VCN's range.
func subnetCIDR(vcn string) (string, error) {
	p, err := netip.ParsePrefix(vcn)
	if err != nil || !p.Addr().Is4() || p.Bits() > 20 {
		return "", fmt.Errorf("%q is not an IPv4 range of at most /20", vcn)
	}
	sub, err := p.Addr().Prefix(20)
	if err != nil {
		return "", err
	}
	return sub.String(), nil
}

// networkParts are the names of the parts of a managed network; the VCN carries the engine's name.
type networkParts struct{ vcn, igw, route, list, subnet string }

func partsOf(name string) networkParts {
	return networkParts{vcn: name, igw: name + "-igw", route: name + "-rt", list: name + "-sl", subnet: name}
}

// notAvailable is the Retryable error of a part that is not AVAILABLE yet.
func notAvailable(what, name, state string) error {
	return vendors.NewError(vendors.Retryable, "ResourcePending",
		fmt.Errorf("the %s %s is %s: try again", what, name, state))
}

// ensureNetwork finds or creates the managed network of the name: the VCN, an internet gateway, a
// route table (0.0.0.0/0 to the gateway), a minimal security list (egress all; ingress only ICMP
// type 3 code 4, the "fragmentation needed" message path MTU discovery needs, because the VNIC MTU
// is 9000 and the internet path 1500; the subnet must not inherit the VCN's default list, which also
// allows SSH from anywhere: the groups carry the policy) and one regional public subnet of the first
// /20. A part left by an interrupted creation is adopted and finished by the next call (all parts
// carry the marker); a part still PROVISIONING makes the call Retryable.
func (d *Driver) ensureNetwork(ctx context.Context, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	var p SubnetParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeSettings, err)
		}
	}
	cidr := cmp.Or(p.CIDR, DefaultVCNCIDR)
	subCIDR, err := subnetCIDR(cidr)
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeSettings, err)
	}
	tags, err := resourceTags(spec)
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeSettings, err)
	}
	n := partsOf(spec.Name)
	vcn, created, err := d.ensureVCN(ctx, region, n.vcn, cidr, tags)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	steps := []func() (bool, error){
		func() (bool, error) { return d.ensureGateway(ctx, region, vcn.Id, n.igw, tags) },
		func() (bool, error) { return d.ensureRouteTable(ctx, region, vcn.Id, n.route, tags) },
		func() (bool, error) { return d.ensureSecurityList(ctx, region, vcn.Id, n.list, tags) },
		func() (bool, error) { return d.ensureSubnet(ctx, region, vcn.Id, n, subCIDR, tags) },
	}
	for _, step := range steps {
		c, err := step()
		if err != nil {
			return vendors.ResourceRef{}, false, err
		}
		created = created || c
	}
	return d.vcnRef(region, *vcn), created, nil
}

func (d *Driver) ensureVCN(ctx context.Context, region, name, cidr string, tags map[string]string,
) (*core.Vcn, bool, error) {
	vcns, err := pages(func(page *string) ([]core.Vcn, *string, error) {
		res, err := d.api.Network.ListVcns(ctx, region, core.ListVcnsRequest{
			CompartmentId: &d.compartment, DisplayName: &name, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return nil, false, err
	}
	var vcn *core.Vcn
	for i := range vcns {
		if s := vcns[i].LifecycleState; s != core.VcnLifecycleStateTerminated && s != core.VcnLifecycleStateTerminating {
			vcn = &vcns[i]
			break
		}
	}
	created := false
	if vcn == nil {
		res, err := d.api.Network.CreateVcn(ctx, region, core.CreateVcnRequest{CreateVcnDetails: core.CreateVcnDetails{
			CompartmentId: &d.compartment, DisplayName: &name, CidrBlocks: []string{cidr}, FreeformTags: tags}})
		if err != nil {
			return nil, false, classify(err, kindRead)
		}
		vcn, created = &res.Vcn, true
	}
	if !d.owned(vcn.FreeformTags) {
		return nil, false, vendors.Errorf(vendors.Fatal,
			"a VCN named %s exists in %s without the ownership marker of this cluster", name, region)
	}
	if vcn.LifecycleState != core.VcnLifecycleStateAvailable {
		return nil, false, notAvailable("VCN", name, string(vcn.LifecycleState))
	}
	return vcn, created, nil
}

func (d *Driver) ensureGateway(ctx context.Context, region string, vcn *string, name string, tags map[string]string,
) (bool, error) {
	igws, err := pages(func(page *string) ([]core.InternetGateway, *string, error) {
		res, err := d.api.Network.ListInternetGateways(ctx, region, core.ListInternetGatewaysRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return false, err
	}
	for _, g := range igws {
		if d.owned(g.FreeformTags) {
			return false, availableOr(g.LifecycleState == core.InternetGatewayLifecycleStateAvailable,
				"internet gateway", name, string(g.LifecycleState))
		}
	}
	res, err := d.api.Network.CreateInternetGateway(ctx, region, core.CreateInternetGatewayRequest{
		CreateInternetGatewayDetails: core.CreateInternetGatewayDetails{CompartmentId: &d.compartment, VcnId: vcn,
			DisplayName: &name, IsEnabled: new(true), FreeformTags: tags}})
	if err != nil {
		return false, classify(err, kindRead)
	}
	return true, availableOr(res.LifecycleState == core.InternetGatewayLifecycleStateAvailable,
		"internet gateway", name, string(res.LifecycleState))
}

// gatewayOf is the managed internet gateway of the VCN, the target of the default route.
func (d *Driver) gatewayOf(ctx context.Context, region string, vcn *string) (*string, error) {
	igws, err := pages(func(page *string) ([]core.InternetGateway, *string, error) {
		res, err := d.api.Network.ListInternetGateways(ctx, region, core.ListInternetGatewaysRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return nil, err
	}
	for _, g := range igws {
		if d.owned(g.FreeformTags) {
			return g.Id, nil
		}
	}
	return nil, vendors.Errorf(vendors.Retryable, "the internet gateway of the VCN %s is not listed yet", deref(vcn))
}

func (d *Driver) ensureRouteTable(ctx context.Context, region string, vcn *string, name string, tags map[string]string,
) (bool, error) {
	routes, err := pages(func(page *string) ([]core.RouteTable, *string, error) {
		res, err := d.api.Network.ListRouteTables(ctx, region, core.ListRouteTablesRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return false, err
	}
	for _, r := range routes {
		if d.owned(r.FreeformTags) {
			return false, availableOr(r.LifecycleState == core.RouteTableLifecycleStateAvailable,
				"route table", name, string(r.LifecycleState))
		}
	}
	igw, err := d.gatewayOf(ctx, region, vcn)
	if err != nil {
		return false, err
	}
	res, err := d.api.Network.CreateRouteTable(ctx, region, core.CreateRouteTableRequest{
		CreateRouteTableDetails: core.CreateRouteTableDetails{CompartmentId: &d.compartment, VcnId: vcn,
			DisplayName: &name, FreeformTags: tags, RouteRules: []core.RouteRule{{
				NetworkEntityId: igw, Destination: new("0.0.0.0/0"), DestinationType: core.RouteRuleDestinationTypeCidrBlock}}}})
	if err != nil {
		return false, classify(err, kindRead)
	}
	return true, availableOr(res.LifecycleState == core.RouteTableLifecycleStateAvailable,
		"route table", name, string(res.LifecycleState))
}

func (d *Driver) ensureSecurityList(ctx context.Context, region string, vcn *string, name string,
	tags map[string]string,
) (bool, error) {
	list, err := d.ownedSecurityList(ctx, region, vcn)
	if err != nil {
		return false, err
	}
	if list != nil {
		return false, availableOr(list.LifecycleState == core.SecurityListLifecycleStateAvailable,
			"security list", name, string(list.LifecycleState))
	}
	res, err := d.api.Network.CreateSecurityList(ctx, region, core.CreateSecurityListRequest{
		CreateSecurityListDetails: core.CreateSecurityListDetails{CompartmentId: &d.compartment, VcnId: vcn,
			DisplayName: &name, FreeformTags: tags,
			EgressSecurityRules: []core.EgressSecurityRule{{Protocol: new("all"), Destination: new("0.0.0.0/0"),
				DestinationType: core.EgressSecurityRuleDestinationTypeCidrBlock, IsStateless: new(false)}},
			IngressSecurityRules: []core.IngressSecurityRule{{Protocol: new("1"), Source: new("0.0.0.0/0"),
				SourceType: core.IngressSecurityRuleSourceTypeCidrBlock, IsStateless: new(false),
				IcmpOptions: &core.IcmpOptions{Type: new(3), Code: new(4)}}}}})
	if err != nil {
		return false, classify(err, kindRead)
	}
	return true, availableOr(res.LifecycleState == core.SecurityListLifecycleStateAvailable,
		"security list", name, string(res.LifecycleState))
}

func (d *Driver) ownedSecurityList(ctx context.Context, region string, vcn *string) (*core.SecurityList, error) {
	lists, err := pages(func(page *string) ([]core.SecurityList, *string, error) {
		res, err := d.api.Network.ListSecurityLists(ctx, region, core.ListSecurityListsRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return nil, err
	}
	for i := range lists {
		if d.owned(lists[i].FreeformTags) {
			return &lists[i], nil
		}
	}
	return nil, nil
}

func (d *Driver) ensureSubnet(ctx context.Context, region string, vcn *string, n networkParts, cidr string,
	tags map[string]string,
) (bool, error) {
	subs, err := d.ownedSubnets(ctx, region, vcn)
	if err != nil {
		return false, err
	}
	for _, s := range subs {
		if s.LifecycleState != core.SubnetLifecycleStateTerminating {
			d.rememberSubnet(region, s)
			return false, availableOr(s.LifecycleState == core.SubnetLifecycleStateAvailable,
				"subnet", n.subnet, string(s.LifecycleState))
		}
	}
	route, list, err := d.subnetParts(ctx, region, vcn)
	if err != nil {
		return false, err
	}
	res, err := d.api.Network.CreateSubnet(ctx, region, core.CreateSubnetRequest{
		CreateSubnetDetails: core.CreateSubnetDetails{
			CompartmentId: &d.compartment, VcnId: vcn, DisplayName: &n.subnet, CidrBlock: &cidr,
			ProhibitPublicIpOnVnic: new(false), RouteTableId: route, SecurityListIds: []string{deref(list)},
			FreeformTags: tags}})
	if err != nil {
		return false, classify(err, kindRead)
	}
	d.rememberSubnet(region, res.Subnet)
	return true, availableOr(res.LifecycleState == core.SubnetLifecycleStateAvailable,
		"subnet", n.subnet, string(res.LifecycleState))
}

// subnetParts are the managed route table and security list the subnet is made with.
func (d *Driver) subnetParts(ctx context.Context, region string, vcn *string) (route, list *string, err error) {
	routes, err := pages(func(page *string) ([]core.RouteTable, *string, error) {
		res, err := d.api.Network.ListRouteTables(ctx, region, core.ListRouteTablesRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range routes {
		if d.owned(r.FreeformTags) {
			route = r.Id
		}
	}
	sl, err := d.ownedSecurityList(ctx, region, vcn)
	if err != nil {
		return nil, nil, err
	}
	if route == nil || sl == nil {
		return nil, nil, vendors.Errorf(vendors.Retryable,
			"the route table and security list of the VCN %s are not listed yet", deref(vcn))
	}
	return route, sl.Id, nil
}

// availableOr is the Retryable error of a part that is not AVAILABLE yet, or nil.
func availableOr(available bool, what, name, state string) error {
	if available {
		return nil
	}
	return notAvailable(what, name, state)
}

// vcnRef is the managed resource of the subnets kind: the VCN, which stands for its subnet, gateway,
// route table and security list (as the VPC does on AWS). Prepare reads the subnets in it. The
// reference stays the same when an interrupted creation is completed.
func (d *Driver) vcnRef(region string, v core.Vcn) vendors.ResourceRef {
	return vendors.ResourceRef{Kind: KindSubnets, Region: region, ID: deref(v.Id), Name: deref(v.DisplayName),
		Managed: true, Network: deref(v.Id), Tags: TagsOf(v.FreeformTags), CreatedAt: timeOf(v.TimeCreated)}
}

// listNetworks lists the managed networks of the region carrying all the tags: the VCNs with the
// marker.
func (d *Driver) listNetworks(ctx context.Context, region string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	vcns, err := pages(func(page *string) ([]core.Vcn, *string, error) {
		res, err := d.api.Network.ListVcns(ctx, region, core.ListVcnsRequest{CompartmentId: &d.compartment, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return nil, err
	}
	var out []vendors.ResourceRef
	for _, v := range vcns {
		gone := v.LifecycleState == core.VcnLifecycleStateTerminated
		if gone || !d.owned(v.FreeformTags) || !tagsMatch(v.FreeformTags, tags) {
			continue
		}
		out = append(out, d.vcnRef(region, v))
	}
	return out, nil
}

func (d *Driver) ownedSubnets(ctx context.Context, region string, vcn *string) ([]core.Subnet, error) {
	subs, err := d.allSubnets(ctx, region, vcn)
	if err != nil {
		return nil, err
	}
	out := subs[:0]
	for _, s := range subs {
		if d.owned(s.FreeformTags) && s.LifecycleState != core.SubnetLifecycleStateTerminated {
			out = append(out, s)
		}
	}
	return out, nil
}

func (d *Driver) allSubnets(ctx context.Context, region string, vcn *string) ([]core.Subnet, error) {
	return pages(func(page *string) ([]core.Subnet, *string, error) {
		res, err := d.api.Network.ListSubnets(ctx, region, core.ListSubnetsRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
}

// vcnOf is the VCN of a reference of the subnets kind: the ref's ID is the VCN of a managed network,
// or a subnet (the VCN is Network, or read from the subnet).
func (d *Driver) vcnOf(ctx context.Context, ref vendors.ResourceRef) (string, error) {
	if strings.HasPrefix(ref.ID, "ocid1.vcn.") {
		return ref.ID, nil
	}
	if ref.Network != "" {
		return ref.Network, nil
	}
	res, err := d.api.Network.GetSubnet(ctx, ref.Region, core.GetSubnetRequest{SubnetId: &ref.ID})
	if err != nil {
		return "", d.gone(ref, err)
	}
	return deref(res.VcnId), nil
}

// subnetIDs are the subnets a reference of the subnets kind stands for: the subnet itself, or every
// subnet of the VCN.
func (d *Driver) subnetIDs(ctx context.Context, ref vendors.ResourceRef) ([]string, error) {
	if !strings.HasPrefix(ref.ID, "ocid1.vcn.") {
		if _, err := d.subnet(ctx, ref.Region, ref.ID); err != nil {
			if vendors.IsClass(err, vendors.Unsupported) {
				return nil, vendors.NewError(vendors.NotFound, vendors.CodeOf(err), err)
			}
			return nil, err
		}
		if ref.Network == "" {
			return []string{ref.ID}, nil
		}
	}
	vcn, err := d.vcnOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	subs, err := d.allSubnets(ctx, ref.Region, &vcn)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, deref(s.Id))
	}
	return out, nil
}

// deleteNetwork deletes the managed network of a reference, in the dependency order: the subnet,
// the route table, the internet gateway, the security list, the VCN. Only a VCN with the
// ownership marker is touched, only its parts with the marker are deleted, and the VCN goes only
// when no subnet is left in it (one a user added is not ours).
func (d *Driver) deleteNetwork(ctx context.Context, ref vendors.ResourceRef) error {
	vcnID, err := d.vcnOf(ctx, ref)
	if err != nil {
		if vendors.IsClass(err, vendors.NotFound) {
			return nil
		}
		return err
	}
	vcns, err := pages(func(page *string) ([]core.Vcn, *string, error) {
		res, err := d.api.Network.ListVcns(ctx, ref.Region, core.ListVcnsRequest{CompartmentId: &d.compartment, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return err
	}
	i := slices.IndexFunc(vcns, func(v core.Vcn) bool { return deref(v.Id) == vcnID })
	if i < 0 || vcns[i].LifecycleState == core.VcnLifecycleStateTerminated {
		return nil // already gone
	}
	if !d.owned(vcns[i].FreeformTags) {
		return vendors.Errorf(vendors.Fatal, "%s was not created by SCarpenter and is never deleted", ref.ID)
	}
	remaining, err := d.deleteSubnets(ctx, ref.Region, &vcnID)
	if err != nil || remaining > 0 {
		return err
	}
	if err := d.deleteParts(ctx, ref.Region, &vcnID); err != nil {
		return err
	}
	_, err = d.api.Network.DeleteVcn(ctx, ref.Region, core.DeleteVcnRequest{VcnId: &vcnID})
	return deleted(err)
}

// deleted is the outcome of a delete call: a resource that is gone is success, a conflict (it is
// still used) is Retryable.
func deleted(err error) error {
	if err != nil && !isNotFound(err) {
		return classify(err, kindDelete)
	}
	return nil
}

// deleteSubnets deletes the subnets of ours in the VCN and returns how many subnets are left:
// those of a user, and those OCI is still terminating.
func (d *Driver) deleteSubnets(ctx context.Context, region string, vcn *string) (int, error) {
	subs, err := d.allSubnets(ctx, region, vcn)
	if err != nil {
		return 0, err
	}
	remaining := 0
	for _, s := range subs {
		switch {
		case s.LifecycleState == core.SubnetLifecycleStateTerminated:
		case !d.owned(s.FreeformTags) || s.LifecycleState == core.SubnetLifecycleStateTerminating:
			remaining++
		default:
			_, err := d.api.Network.DeleteSubnet(ctx, region, core.DeleteSubnetRequest{SubnetId: s.Id})
			if err := deleted(err); err != nil {
				return 0, err
			}
		}
	}
	return remaining, nil
}

// deleteParts deletes the route table, the internet gateway and the security list of ours in the VCN.
func (d *Driver) deleteParts(ctx context.Context, region string, vcn *string) error {
	net := d.api.Network
	routes, err := pages(func(page *string) ([]core.RouteTable, *string, error) {
		res, err := net.ListRouteTables(ctx, region, core.ListRouteTablesRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return err
	}
	for _, r := range routes {
		if d.owned(r.FreeformTags) {
			_, err := net.DeleteRouteTable(ctx, region, core.DeleteRouteTableRequest{RtId: r.Id})
			if err := deleted(err); err != nil {
				return err
			}
		}
	}
	igws, err := pages(func(page *string) ([]core.InternetGateway, *string, error) {
		res, err := net.ListInternetGateways(ctx, region, core.ListInternetGatewaysRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return err
	}
	for _, g := range igws {
		if d.owned(g.FreeformTags) {
			_, err := net.DeleteInternetGateway(ctx, region, core.DeleteInternetGatewayRequest{IgId: g.Id})
			if err := deleted(err); err != nil {
				return err
			}
		}
	}
	lists, err := pages(func(page *string) ([]core.SecurityList, *string, error) {
		res, err := net.ListSecurityLists(ctx, region, core.ListSecurityListsRequest{
			CompartmentId: &d.compartment, VcnId: vcn, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return err
	}
	for _, l := range lists {
		if d.owned(l.FreeformTags) {
			_, err := net.DeleteSecurityList(ctx, region, core.DeleteSecurityListRequest{SecurityListId: l.Id})
			if err := deleted(err); err != nil {
				return err
			}
		}
	}
	return nil
}

// protocolNumber maps the protocol of an inbound rule to the IANA number OCI takes.
func protocolNumber(p common.Protocol) string {
	switch p {
	case protoTCP:
		return "6"
	case protoUDP:
		return "17"
	case protoICMP:
		return "1"
	}
	return "all"
}

// The protocols of an inbound rule.
const (
	protoTCP  common.Protocol = "tcp"
	protoUDP  common.Protocol = "udp"
	protoICMP common.Protocol = "icmp"
)

// groupRules are the rules of a managed group: egress to anywhere, ICMP type 3 code 4 from anywhere
// (path MTU discovery), the group itself when it is self-referencing and the NodeClass's inbound rules.
func groupRules(groupID string, p GroupParams) []core.AddSecurityRuleDetails {
	rules := []core.AddSecurityRuleDetails{
		{Direction: core.AddSecurityRuleDetailsDirectionEgress, Protocol: new("all"), Destination: new("0.0.0.0/0"),
			DestinationType: core.AddSecurityRuleDetailsDestinationTypeCidrBlock, IsStateless: new(false),
			Description: new("egress")},
		{Direction: core.AddSecurityRuleDetailsDirectionIngress, Protocol: new("1"), Source: new("0.0.0.0/0"),
			SourceType: core.AddSecurityRuleDetailsSourceTypeCidrBlock, IsStateless: new(false),
			IcmpOptions: &core.IcmpOptions{Type: new(3), Code: new(4)}, Description: new("path MTU discovery")},
	}
	if p.SelfReference {
		rules = append(rules, core.AddSecurityRuleDetails{Direction: core.AddSecurityRuleDetailsDirectionIngress,
			Protocol: new("all"), Source: &groupID, SourceType: core.AddSecurityRuleDetailsSourceTypeNetworkSecurityGroup,
			IsStateless: new(false), Description: new("the group itself")})
	}
	for _, r := range p.Inbound {
		for _, c := range r.CIDRs {
			rule := core.AddSecurityRuleDetails{Direction: core.AddSecurityRuleDetailsDirectionIngress,
				Protocol: new(protocolNumber(r.Protocol)), Source: new(c), IsStateless: new(false),
				SourceType: core.AddSecurityRuleDetailsSourceTypeCidrBlock, Description: new("networking.inbound")}
			if r.Protocol == protoTCP || r.Protocol == protoUDP {
				lo, hi := 1, 65535
				if r.FromPort != nil {
					lo, hi = int(*r.FromPort), int(*r.FromPort)
					if r.ToPort != nil {
						hi = int(*r.ToPort)
					}
				}
				pr := &core.PortRange{Min: &lo, Max: &hi}
				if r.Protocol == protoTCP {
					rule.TcpOptions = &core.TcpOptions{DestinationPortRange: pr}
				} else {
					rule.UdpOptions = &core.UdpOptions{DestinationPortRange: pr}
				}
			}
			rules = append(rules, rule)
		}
	}
	return rules
}

// maxRulesPerCall is the number of rules AddNetworkSecurityGroupSecurityRules takes.
const maxRulesPerCall = 25

// ensureGroup finds the group of the name in the VCN or creates it with the ownership marker and the
// rules of its parameters; a group that exists without rules (the creation was interrupted between
// the group and its rules) gets them from the next call.
func (d *Driver) ensureGroup(ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	var p GroupParams
	if err := json.Unmarshal(spec.Params, &p); err != nil || p.VCN == "" {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeSettings,
			fmt.Errorf("a network security group needs the parameter vcnId: %v", err))
	}
	tags, err := resourceTags(spec)
	if err != nil {
		return vendors.ResourceRef{}, false, vendors.NewError(vendors.Fatal, codeSettings, err)
	}
	net := d.api.Network
	found, err := d.listGroups(ctx, region, listFilters{vcn: p.VCN, name: spec.Name})
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	var g *core.NetworkSecurityGroup
	for i := range found {
		if found[i].LifecycleState != core.NetworkSecurityGroupLifecycleStateTerminated &&
			found[i].LifecycleState != core.NetworkSecurityGroupLifecycleStateTerminating {
			g = &found[i]
		}
	}
	created := false
	if g == nil {
		res, err := net.CreateNetworkSecurityGroup(ctx, region, core.CreateNetworkSecurityGroupRequest{
			CreateNetworkSecurityGroupDetails: core.CreateNetworkSecurityGroupDetails{CompartmentId: &d.compartment,
				VcnId: &p.VCN, DisplayName: &spec.Name, FreeformTags: tags}})
		if err != nil {
			return vendors.ResourceRef{}, false, classify(err, kindRead)
		}
		g, created = &res.NetworkSecurityGroup, true
	}
	if !d.owned(g.FreeformTags) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal,
			"a network security group named %s exists in %s without the ownership marker of this cluster", spec.Name, p.VCN)
	}
	if g.LifecycleState != core.NetworkSecurityGroupLifecycleStateAvailable {
		return vendors.ResourceRef{}, false, notAvailable("network security group", spec.Name, string(g.LifecycleState))
	}
	existing, err := pages(func(page *string) ([]core.SecurityRule, *string, error) {
		res, err := net.ListNetworkSecurityGroupSecurityRules(ctx, region, core.ListNetworkSecurityGroupSecurityRulesRequest{
			NetworkSecurityGroupId: g.Id, Page: page})
		return res.Items, res.OpcNextPage, classify(err, kindRead)
	})
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	if len(existing) == 0 {
		rules := groupRules(deref(g.Id), p)
		for len(rules) > 0 {
			n := min(len(rules), maxRulesPerCall)
			details := core.AddNetworkSecurityGroupSecurityRulesDetails{SecurityRules: rules[:n]}
			if _, err := net.AddNetworkSecurityGroupSecurityRules(ctx, region,
				core.AddNetworkSecurityGroupSecurityRulesRequest{
					NetworkSecurityGroupId: g.Id, AddNetworkSecurityGroupSecurityRulesDetails: details}); err != nil {
				return vendors.ResourceRef{}, false, classify(err, kindRead)
			}
			rules = rules[n:]
		}
	}
	ref := d.groupRef(kind, region, *g)
	ref.Managed = true
	return ref, created, nil
}

// deleteGroup deletes a group that carries the ownership marker; a 409 (VNICs still carry it) is
// Retryable.
func (d *Driver) deleteGroup(ctx context.Context, ref vendors.ResourceRef) error {
	res, err := d.api.Network.GetNetworkSecurityGroup(ctx, ref.Region, core.GetNetworkSecurityGroupRequest{
		NetworkSecurityGroupId: &ref.ID})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return classify(err, kindRead)
	}
	if !d.owned(res.FreeformTags) {
		return vendors.Errorf(vendors.Fatal, "%s was not created by SCarpenter and is never deleted", ref.ID)
	}
	if _, err := d.api.Network.DeleteNetworkSecurityGroup(ctx, ref.Region, core.DeleteNetworkSecurityGroupRequest{
		NetworkSecurityGroupId: &ref.ID}); err != nil && !isNotFound(err) {
		return classify(err, kindDelete)
	}
	return nil
}
