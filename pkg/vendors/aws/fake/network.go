/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ---- VPCs ----

func (v *vpc) attrs(name string) ([]string, bool) {
	switch name {
	case "vpc-id":
		return []string{v.id}, true
	case "isDefault", "is-default":
		return []string{fmt.Sprint(v.isDefault)}, true
	case "cidr":
		return []string{v.cidr}, true
	}
	return nil, false
}

func (v *vpc) toSDK() types.Vpc {
	out := types.Vpc{VpcId: new(v.id), CidrBlock: new(v.cidr), IsDefault: new(v.isDefault), Tags: tagList(v.tags),
		State: types.VpcStateAvailable}
	if v.ipv6 != "" {
		out.Ipv6CidrBlockAssociationSet = []types.VpcIpv6CidrBlockAssociation{{
			Ipv6CidrBlock: new(v.ipv6), AssociationId: new("vpc-cidr-assoc-" + v.id),
			Ipv6CidrBlockState: &types.VpcCidrBlockState{State: types.VpcCidrBlockStateCodeAssociated},
		}}
		if v.ipv6Pending > 0 {
			out.Ipv6CidrBlockAssociationSet[0].Ipv6CidrBlockState.State = types.VpcCidrBlockStateCodeAssociating
		}
	}
	return out
}

// AssociateVpcCidrBlock implements EC2API: an Amazon-provided IPv6 block, once.
func (e *EC2) AssociateVpcCidrBlock(ctx context.Context, in *ec2.AssociateVpcCidrBlockInput, _ ...func(*ec2.Options),
) (*ec2.AssociateVpcCidrBlockOutput, error) {
	r, err := e.begin(ctx, "AssociateVpcCidrBlock", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	v := r.vpcs[deref(in.VpcId)]
	if v == nil {
		return nil, apiError("InvalidVpcID.NotFound", "the vpc does not exist")
	}
	if in.AmazonProvidedIpv6CidrBlock == nil || !*in.AmazonProvidedIpv6CidrBlock {
		return nil, apiError("MissingParameter", "only the Amazon-provided IPv6 block is supported by the fake")
	}
	if v.ipv6 != "" {
		return nil, apiError("CidrConflict", "the vpc already has an IPv6 CIDR block")
	}
	v.ipv6, v.ipv6Pending = e.a.newIPv6Block(), e.a.ipv6Delay
	return &ec2.AssociateVpcCidrBlockOutput{VpcId: in.VpcId}, nil
}

// AssociateSubnetCidrBlock implements EC2API: a /64 inside the VPC's /56.
func (e *EC2) AssociateSubnetCidrBlock(ctx context.Context, in *ec2.AssociateSubnetCidrBlockInput, _ ...func(*ec2.Options),
) (*ec2.AssociateSubnetCidrBlockOutput, error) {
	r, err := e.begin(ctx, "AssociateSubnetCidrBlock", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	sn := r.subnets[deref(in.SubnetId)]
	if sn == nil {
		return nil, apiError("InvalidSubnetID.NotFound", "the subnet does not exist")
	}
	if err := r.checkSubnetIPv6(sn, deref(in.Ipv6CidrBlock)); err != nil {
		return nil, err
	}
	sn.ipv6 = *in.Ipv6CidrBlock
	return &ec2.AssociateSubnetCidrBlockOutput{SubnetId: in.SubnetId}, nil
}

// checkSubnetIPv6 checks that the /64 is inside the VPC's IPv6 block and not used.
func (r *region) checkSubnetIPv6(sn *subnet, cidr string) error {
	v := r.vpcs[sn.vpc]
	p, err := netip.ParsePrefix(cidr)
	if err != nil || p.Bits() != 64 {
		return apiError("InvalidParameterValue", "the IPv6 CIDR of a subnet is a /64: "+cidr)
	}
	if v.ipv6 == "" || !netip.MustParsePrefix(v.ipv6).Contains(p.Addr()) {
		return apiError("InvalidParameterValue", "the CIDR "+cidr+" is not inside an IPv6 block of the VPC")
	}
	for _, o := range r.subnets {
		if o.vpc == sn.vpc && o.ipv6 == cidr {
			return apiError("InvalidSubnet.Conflict", "the IPv6 CIDR "+cidr+" conflicts with another subnet")
		}
	}
	return nil
}

// DescribeVpcs implements EC2API.
func (e *EC2) DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options),
) (*ec2.DescribeVpcsOutput, error) {
	r, err := e.begin(ctx, "DescribeVpcs", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.Vpc
	for _, id := range sortedKeys(r.vpcs) {
		v := r.vpcs[id]
		if len(in.VpcIds) > 0 && !slices.Contains(in.VpcIds, id) {
			continue
		}
		ok, err := matches(in.Filters, v.tags, v.attrs)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, v.toSDK())
			if v.ipv6Pending > 0 {
				v.ipv6Pending--
			}
		}
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.pageSize)
	return &ec2.DescribeVpcsOutput{Vpcs: items, NextToken: next}, nil
}

// CreateVpc implements EC2API.
func (e *EC2) CreateVpc(ctx context.Context, in *ec2.CreateVpcInput, _ ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
	r, err := e.begin(ctx, "CreateVpc", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	tags := tagsFromSpecs(in.TagSpecifications, types.ResourceTypeVpc)
	if deref(in.CidrBlock) == "" {
		return nil, apiError("MissingParameter", "CidrBlock is required")
	}
	v := e.a.newVPC(r, *in.CidrBlock, tags)
	if in.AmazonProvidedIpv6CidrBlock != nil && *in.AmazonProvidedIpv6CidrBlock {
		v.ipv6, v.ipv6Pending = e.a.newIPv6Block(), e.a.ipv6Delay
	}
	sdk := v.toSDK()
	return &ec2.CreateVpcOutput{Vpc: &sdk}, nil
}

// DeleteVpc implements EC2API: a VPC with subnets, an attached internet gateway,
// a route table of its own or security groups other than the default is a
// dependency violation.
func (e *EC2) DeleteVpc(ctx context.Context, in *ec2.DeleteVpcInput, _ ...func(*ec2.Options)) (*ec2.DeleteVpcOutput, error) {
	r, err := e.begin(ctx, "DeleteVpc", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	id := deref(in.VpcId)
	if _, ok := r.vpcs[id]; !ok {
		return nil, apiError("InvalidVpcID.NotFound", fmt.Sprintf("The vpc ID '%s' does not exist", id))
	}
	dep := apiError("DependencyViolation", fmt.Sprintf("The vpc '%s' has dependencies and cannot be deleted.", id))
	for _, s := range r.subnets {
		if s.vpc == id {
			return nil, dep
		}
	}
	for g, v := range r.attached {
		if v == id {
			_ = g
			return nil, dep
		}
	}
	for _, t := range r.rts {
		if t.vpc == id && !t.main {
			return nil, dep
		}
	}
	for _, s := range r.sgs {
		if s.vpc == id && s.name != "default" {
			return nil, dep
		}
	}
	for gid, t := range r.rts {
		if t.vpc == id {
			delete(r.rts, gid)
		}
	}
	for sid, s := range r.sgs {
		if s.vpc == id {
			delete(r.sgs, sid)
		}
	}
	delete(r.vpcs, id)
	return &ec2.DeleteVpcOutput{}, nil
}

// ---- subnets ----

func (s *subnet) attrs(name string) ([]string, bool) {
	switch name {
	case "subnet-id":
		return []string{s.id}, true
	case "vpc-id":
		return []string{s.vpc}, true
	case "availability-zone":
		return []string{s.az}, true
	case "default-for-az", "defaultForAz":
		return []string{fmt.Sprint(s.defaultForAZ)}, true
	case "cidr-block", "cidrBlock":
		return []string{s.cidr}, true
	}
	return nil, false
}

func (s *subnet) toSDK(r *region) types.Subnet {
	zoneID := ""
	if z, ok := r.zoneByName(s.az); ok {
		zoneID = z.ID
	}
	out := types.Subnet{
		SubnetId: new(s.id), VpcId: new(s.vpc), AvailabilityZone: new(s.az), AvailabilityZoneId: new(zoneID),
		CidrBlock: new(s.cidr), AvailableIpAddressCount: new(s.free), DefaultForAz: new(s.defaultForAZ),
		MapPublicIpOnLaunch: new(s.mapPublic), Tags: tagList(s.tags), State: types.SubnetStateAvailable,
	}
	if s.ipv6 != "" {
		out.Ipv6CidrBlockAssociationSet = []types.SubnetIpv6CidrBlockAssociation{{
			Ipv6CidrBlock: new(s.ipv6), AssociationId: new("subnet-cidr-assoc-" + s.id),
			Ipv6CidrBlockState: &types.SubnetCidrBlockState{State: types.SubnetCidrBlockStateCodeAssociated},
		}}
		if s.ipv6Associating {
			out.Ipv6CidrBlockAssociationSet[0].Ipv6CidrBlockState.State = types.SubnetCidrBlockStateCodeAssociating
		}
	}
	return out
}

// DescribeSubnets implements EC2API.
func (e *EC2) DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options),
) (*ec2.DescribeSubnetsOutput, error) {
	r, err := e.begin(ctx, "DescribeSubnets", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.Subnet
	for _, id := range sortedKeys(r.subnets) {
		s := r.subnets[id]
		if len(in.SubnetIds) > 0 && !slices.Contains(in.SubnetIds, id) {
			continue
		}
		ok, err := matches(in.Filters, s.tags, s.attrs)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s.toSDK(r))
		}
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.pageSize)
	return &ec2.DescribeSubnetsOutput{Subnets: items, NextToken: next}, nil
}

// CreateSubnet implements EC2API.
func (e *EC2) CreateSubnet(ctx context.Context, in *ec2.CreateSubnetInput, _ ...func(*ec2.Options),
) (*ec2.CreateSubnetOutput, error) {
	r, err := e.begin(ctx, "CreateSubnet", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	v := r.vpcs[deref(in.VpcId)]
	if v == nil {
		return nil, apiError("InvalidVpcID.NotFound", "the vpc does not exist")
	}
	if _, ok := r.zoneByName(deref(in.AvailabilityZone)); !ok {
		return nil, apiError("InvalidParameterValue", "Value ("+deref(in.AvailabilityZone)+") for parameter availabilityZone is invalid.")
	}
	for _, s := range r.subnets {
		if s.vpc == v.id && s.cidr == deref(in.CidrBlock) {
			return nil, apiError("InvalidSubnet.Conflict", "The CIDR '"+s.cidr+"' conflicts with another subnet")
		}
	}
	s0 := &subnet{vpc: v.id}
	if c := deref(in.Ipv6CidrBlock); c != "" {
		if err := r.checkSubnetIPv6(s0, c); err != nil {
			return nil, err
		}
	}
	s := e.a.newSubnet(r, v, *in.AvailabilityZone, deref(in.CidrBlock), tagsFromSpecs(in.TagSpecifications, types.ResourceTypeSubnet))
	s.ipv6 = deref(in.Ipv6CidrBlock)
	sub := s.toSDK(r)
	return &ec2.CreateSubnetOutput{Subnet: &sub}, nil
}

// DeleteSubnet implements EC2API: a subnet with instances is a dependency
// violation, and a route table association goes with it.
func (e *EC2) DeleteSubnet(ctx context.Context, in *ec2.DeleteSubnetInput, _ ...func(*ec2.Options),
) (*ec2.DeleteSubnetOutput, error) {
	r, err := e.begin(ctx, "DeleteSubnet", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	id := deref(in.SubnetId)
	if _, ok := r.subnets[id]; !ok {
		return nil, apiError("InvalidSubnetID.NotFound", fmt.Sprintf("The subnet ID '%s' does not exist", id))
	}
	for _, i := range r.insts {
		if i.subnet == id && i.live() {
			return nil, apiError("DependencyViolation", fmt.Sprintf("The subnet '%s' has dependencies and cannot be deleted.", id))
		}
	}
	for _, t := range r.rts {
		for assoc, sn := range t.subnets {
			if sn == id {
				delete(t.subnets, assoc)
			}
		}
	}
	delete(r.subnets, id)
	return &ec2.DeleteSubnetOutput{}, nil
}

// ---- internet gateways ----

// CreateInternetGateway implements EC2API.
func (e *EC2) CreateInternetGateway(ctx context.Context, in *ec2.CreateInternetGatewayInput, _ ...func(*ec2.Options),
) (*ec2.CreateInternetGatewayOutput, error) {
	r, err := e.begin(ctx, "CreateInternetGateway", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	g := &igw{id: e.a.id("igw"), tags: tagsFromSpecs(in.TagSpecifications, types.ResourceTypeInternetGateway)}
	r.igws[g.id] = g
	return &ec2.CreateInternetGatewayOutput{InternetGateway: &types.InternetGateway{
		InternetGatewayId: new(g.id), Tags: tagList(g.tags)}}, nil
}

// AttachInternetGateway implements EC2API.
func (e *EC2) AttachInternetGateway(ctx context.Context, in *ec2.AttachInternetGatewayInput, _ ...func(*ec2.Options),
) (*ec2.AttachInternetGatewayOutput, error) {
	r, err := e.begin(ctx, "AttachInternetGateway", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	id := deref(in.InternetGatewayId)
	if _, ok := r.igws[id]; !ok {
		return nil, apiError("InvalidInternetGatewayID.NotFound", "the internet gateway does not exist")
	}
	if _, ok := r.vpcs[deref(in.VpcId)]; !ok {
		return nil, apiError("InvalidVpcID.NotFound", "the vpc does not exist")
	}
	if _, ok := r.attached[id]; ok {
		return nil, apiError("Resource.AlreadyAssociated", "the internet gateway is already attached")
	}
	r.attached[id] = *in.VpcId
	return &ec2.AttachInternetGatewayOutput{}, nil
}

// DetachInternetGateway implements EC2API: a VPC with a public address in use
// keeps its gateway.
func (e *EC2) DetachInternetGateway(ctx context.Context, in *ec2.DetachInternetGatewayInput, _ ...func(*ec2.Options),
) (*ec2.DetachInternetGatewayOutput, error) {
	r, err := e.begin(ctx, "DetachInternetGateway", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	id := deref(in.InternetGatewayId)
	if r.attached[id] != deref(in.VpcId) {
		return nil, apiError("Gateway.NotAttached", "the internet gateway is not attached to the vpc")
	}
	for _, i := range r.insts {
		if i.vpc == *in.VpcId && i.live() && i.publicIP != "" {
			return nil, apiError("DependencyViolation", "the network has mapped public addresses and cannot be detached")
		}
	}
	delete(r.attached, id)
	return &ec2.DetachInternetGatewayOutput{}, nil
}

// DeleteInternetGateway implements EC2API: an attached gateway is a dependency
// violation.
func (e *EC2) DeleteInternetGateway(ctx context.Context, in *ec2.DeleteInternetGatewayInput, _ ...func(*ec2.Options),
) (*ec2.DeleteInternetGatewayOutput, error) {
	r, err := e.begin(ctx, "DeleteInternetGateway", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	id := deref(in.InternetGatewayId)
	if _, ok := r.igws[id]; !ok {
		return nil, apiError("InvalidInternetGatewayID.NotFound", fmt.Sprintf("The internetGateway ID '%s' does not exist", id))
	}
	if _, ok := r.attached[id]; ok {
		return nil, apiError("DependencyViolation", fmt.Sprintf("The internetGateway '%s' has dependencies and cannot be deleted.", id))
	}
	delete(r.igws, id)
	return &ec2.DeleteInternetGatewayOutput{}, nil
}

// DescribeInternetGateways implements EC2API.
func (e *EC2) DescribeInternetGateways(ctx context.Context, in *ec2.DescribeInternetGatewaysInput, _ ...func(*ec2.Options),
) (*ec2.DescribeInternetGatewaysOutput, error) {
	r, err := e.begin(ctx, "DescribeInternetGateways", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.InternetGateway
	for _, id := range sortedKeys(r.igws) {
		g := r.igws[id]
		attrs := func(name string) ([]string, bool) {
			switch name {
			case "internet-gateway-id":
				return []string{id}, true
			case "attachment.vpc-id":
				if v, ok := r.attached[id]; ok {
					return []string{v}, true
				}
				return nil, true
			}
			return nil, false
		}
		ok, err := matches(in.Filters, g.tags, attrs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		ig := types.InternetGateway{InternetGatewayId: new(id), Tags: tagList(g.tags)}
		if v, ok := r.attached[id]; ok {
			ig.Attachments = []types.InternetGatewayAttachment{{VpcId: new(v), State: types.AttachmentStatusAttached}}
		}
		out = append(out, ig)
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.pageSize)
	return &ec2.DescribeInternetGatewaysOutput{InternetGateways: items, NextToken: next}, nil
}

// ---- route tables ----

// CreateRouteTable implements EC2API.
func (e *EC2) CreateRouteTable(ctx context.Context, in *ec2.CreateRouteTableInput, _ ...func(*ec2.Options),
) (*ec2.CreateRouteTableOutput, error) {
	r, err := e.begin(ctx, "CreateRouteTable", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	if _, ok := r.vpcs[deref(in.VpcId)]; !ok {
		return nil, apiError("InvalidVpcID.NotFound", "the vpc does not exist")
	}
	// A ClientToken is remembered with its parameters, as EC2 does (the fake keeps
	// it for ever): the same token with another VPC is a mismatch.
	if tok := deref(in.ClientToken); tok != "" {
		if prev, ok := r.rtTokens[tok]; ok {
			if prev.vpc != *in.VpcId {
				return nil, apiError("IdempotentParameterMismatch", "Client token "+tok+" was used with different parameters")
			}
			// EC2 replays the answer of the first call, also for a table that was deleted
			// since.
			return &ec2.CreateRouteTableOutput{RouteTable: &types.RouteTable{
				RouteTableId: new(prev.id), VpcId: new(prev.vpc), Tags: tagList(prev.tags)}}, nil
		}
	}
	t := &routeTable{id: e.a.id("rtb"), vpc: *in.VpcId, subnets: map[string]string{},
		tags: tagsFromSpecs(in.TagSpecifications, types.ResourceTypeRouteTable)}
	r.rts[t.id] = t
	if tok := deref(in.ClientToken); tok != "" {
		r.rtTokens[tok] = t
	}
	return &ec2.CreateRouteTableOutput{RouteTable: &types.RouteTable{
		RouteTableId: new(t.id), VpcId: new(t.vpc), Tags: tagList(t.tags)}}, nil
}

// CreateRoute implements EC2API.
func (e *EC2) CreateRoute(ctx context.Context, in *ec2.CreateRouteInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error) {
	r, err := e.begin(ctx, "CreateRoute", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	t := r.rts[deref(in.RouteTableId)]
	if t == nil {
		return nil, apiError("InvalidRouteTableID.NotFound", "the route table does not exist")
	}
	if g := deref(in.GatewayId); g != "" {
		if r.attached[g] != t.vpc {
			return nil, apiError("InvalidParameterValue", "the gateway is not attached to the route table's vpc")
		}
	}
	dest := deref(in.DestinationCidrBlock)
	if dest == "" {
		dest = deref(in.DestinationIpv6CidrBlock)
	}
	route := dest + "->" + deref(in.GatewayId)
	if slices.Contains(t.routes, route) {
		return nil, apiError("RouteAlreadyExists", "the route "+route+" already exists")
	}
	t.routes = append(t.routes, route)
	return &ec2.CreateRouteOutput{Return: new(true)}, nil
}

// ReplaceRoute implements EC2API: the destination must have a route.
func (e *EC2) ReplaceRoute(ctx context.Context, in *ec2.ReplaceRouteInput, _ ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error) {
	r, err := e.begin(ctx, "ReplaceRoute", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	t := r.rts[deref(in.RouteTableId)]
	if t == nil {
		return nil, apiError("InvalidRouteTableID.NotFound", "the route table does not exist")
	}
	for i, route := range t.routes {
		if dest, _, _ := strings.Cut(route, "->"); dest == cmp.Or(deref(in.DestinationCidrBlock), deref(in.DestinationIpv6CidrBlock)) {
			t.routes[i] = dest + "->" + deref(in.GatewayId)
			return &ec2.ReplaceRouteOutput{}, nil
		}
	}
	return nil, apiError("InvalidRoute.NotFound", "no route with destination-cidr-block "+deref(in.DestinationCidrBlock))
}

// AssociateRouteTable implements EC2API.
func (e *EC2) AssociateRouteTable(ctx context.Context, in *ec2.AssociateRouteTableInput, _ ...func(*ec2.Options),
) (*ec2.AssociateRouteTableOutput, error) {
	r, err := e.begin(ctx, "AssociateRouteTable", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	t := r.rts[deref(in.RouteTableId)]
	if t == nil {
		return nil, apiError("InvalidRouteTableID.NotFound", "the route table does not exist")
	}
	if _, ok := r.subnets[deref(in.SubnetId)]; !ok {
		return nil, apiError("InvalidSubnetID.NotFound", "the subnet does not exist")
	}
	assoc := e.a.id("rtbassoc")
	t.subnets[assoc] = *in.SubnetId
	return &ec2.AssociateRouteTableOutput{AssociationId: new(assoc)}, nil
}

// DisassociateRouteTable implements EC2API.
func (e *EC2) DisassociateRouteTable(ctx context.Context, in *ec2.DisassociateRouteTableInput, _ ...func(*ec2.Options),
) (*ec2.DisassociateRouteTableOutput, error) {
	r, err := e.begin(ctx, "DisassociateRouteTable", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	for _, t := range r.rts {
		if _, ok := t.subnets[deref(in.AssociationId)]; ok {
			delete(t.subnets, *in.AssociationId)
			return &ec2.DisassociateRouteTableOutput{}, nil
		}
	}
	return nil, apiError("InvalidAssociationID.NotFound", "the association does not exist")
}

// DeleteRouteTable implements EC2API: a route table with associations is a
// dependency violation.
func (e *EC2) DeleteRouteTable(ctx context.Context, in *ec2.DeleteRouteTableInput, _ ...func(*ec2.Options),
) (*ec2.DeleteRouteTableOutput, error) {
	r, err := e.begin(ctx, "DeleteRouteTable", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	id := deref(in.RouteTableId)
	t := r.rts[id]
	if t == nil {
		return nil, apiError("InvalidRouteTableID.NotFound", fmt.Sprintf("The routeTable ID '%s' does not exist", id))
	}
	if len(t.subnets) > 0 || t.main {
		return nil, apiError("DependencyViolation", fmt.Sprintf("The routeTable '%s' has dependencies and cannot be deleted.", id))
	}
	delete(r.rts, id)
	return &ec2.DeleteRouteTableOutput{}, nil
}

// DescribeRouteTables implements EC2API.
func (e *EC2) DescribeRouteTables(ctx context.Context, in *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options),
) (*ec2.DescribeRouteTablesOutput, error) {
	r, err := e.begin(ctx, "DescribeRouteTables", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.RouteTable
	for _, id := range sortedKeys(r.rts) {
		t := r.rts[id]
		attrs := func(name string) ([]string, bool) {
			switch name {
			case "route-table-id":
				return []string{id}, true
			case "vpc-id":
				return []string{t.vpc}, true
			}
			return nil, false
		}
		ok, err := matches(in.Filters, t.tags, attrs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		rt := types.RouteTable{RouteTableId: new(id), VpcId: new(t.vpc), Tags: tagList(t.tags)}
		for _, route := range t.routes {
			dest, gw, _ := strings.Cut(route, "->")
			route := types.Route{GatewayId: new(gw)}
			if strings.Contains(dest, ":") {
				route.DestinationIpv6CidrBlock = new(dest)
			} else {
				route.DestinationCidrBlock = new(dest)
			}
			rt.Routes = append(rt.Routes, route)
		}
		if t.main {
			rt.Associations = append(rt.Associations, types.RouteTableAssociation{Main: new(true), RouteTableId: new(id)})
		}
		for _, assoc := range sortedKeys(t.subnets) {
			rt.Associations = append(rt.Associations, types.RouteTableAssociation{
				RouteTableAssociationId: new(assoc), SubnetId: new(t.subnets[assoc]), RouteTableId: new(id)})
		}
		out = append(out, rt)
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.pageSize)
	return &ec2.DescribeRouteTablesOutput{RouteTables: items, NextToken: next}, nil
}

// ---- security groups ----

func (g *sg) attrs(name string) ([]string, bool) {
	switch name {
	case "group-id":
		return []string{g.id}, true
	case "group-name":
		return []string{g.name}, true
	case "vpc-id":
		return []string{g.vpc}, true
	}
	return nil, false
}

func (g *sg) toSDK() types.SecurityGroup {
	out := types.SecurityGroup{GroupId: new(g.id), GroupName: new(g.name), VpcId: new(g.vpc), Description: new(g.desc),
		Tags: tagList(g.tags)}
	for _, r := range g.ingress {
		p := types.IpPermission{IpProtocol: new(r.protocol)}
		if r.protocol != "-1" && r.protocol != "icmp" {
			p.FromPort, p.ToPort = new(r.from), new(r.to)
		}
		switch {
		case r.cidr != "":
			p.IpRanges = []types.IpRange{{CidrIp: new(r.cidr)}}
		case r.cidr6 != "":
			p.Ipv6Ranges = []types.Ipv6Range{{CidrIpv6: new(r.cidr6)}}
		case r.grp != "":
			p.UserIdGroupPairs = []types.UserIdGroupPair{{GroupId: new(r.grp)}}
		}
		out.IpPermissions = append(out.IpPermissions, p)
	}
	return out
}

// DescribeSecurityGroups implements EC2API.
func (e *EC2) DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options),
) (*ec2.DescribeSecurityGroupsOutput, error) {
	r, err := e.begin(ctx, "DescribeSecurityGroups", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.SecurityGroup
	for _, id := range sortedKeys(r.sgs) {
		g := r.sgs[id]
		if len(in.GroupIds) > 0 && !slices.Contains(in.GroupIds, id) {
			continue
		}
		ok, err := matches(in.Filters, g.tags, g.attrs)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, g.toSDK())
		}
	}
	items, next := page(out, in.NextToken, in.MaxResults, e.a.pageSize)
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: items, NextToken: next}, nil
}

// CreateSecurityGroup implements EC2API.
func (e *EC2) CreateSecurityGroup(ctx context.Context, in *ec2.CreateSecurityGroupInput, _ ...func(*ec2.Options),
) (*ec2.CreateSecurityGroupOutput, error) {
	r, err := e.begin(ctx, "CreateSecurityGroup", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	if _, ok := r.vpcs[deref(in.VpcId)]; !ok {
		return nil, apiError("InvalidVpcID.NotFound", "the vpc does not exist")
	}
	for _, g := range r.sgs {
		if g.vpc == *in.VpcId && g.name == deref(in.GroupName) {
			return nil, apiError("InvalidGroup.Duplicate", fmt.Sprintf("The security group '%s' already exists for VPC '%s'", g.name, g.vpc))
		}
	}
	g := &sg{id: e.a.id("sg"), name: deref(in.GroupName), vpc: *in.VpcId, desc: deref(in.Description),
		tags: tagsFromSpecs(in.TagSpecifications, types.ResourceTypeSecurityGroup)}
	r.sgs[g.id] = g
	return &ec2.CreateSecurityGroupOutput{GroupId: new(g.id), Tags: tagList(g.tags)}, nil
}

func rulesOf(perms []types.IpPermission) []sgRule {
	var out []sgRule
	for _, p := range perms {
		base := sgRule{protocol: deref(p.IpProtocol), from: deref(p.FromPort), to: deref(p.ToPort)}
		for _, c := range p.IpRanges {
			x := base
			x.cidr = deref(c.CidrIp)
			out = append(out, x)
		}
		for _, c := range p.Ipv6Ranges {
			x := base
			x.cidr6 = deref(c.CidrIpv6)
			out = append(out, x)
		}
		for _, c := range p.UserIdGroupPairs {
			x := base
			x.grp = deref(c.GroupId)
			out = append(out, x)
		}
	}
	return out
}

// AuthorizeSecurityGroupIngress implements EC2API.
func (e *EC2) AuthorizeSecurityGroupIngress(ctx context.Context, in *ec2.AuthorizeSecurityGroupIngressInput, _ ...func(*ec2.Options),
) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	r, err := e.begin(ctx, "AuthorizeSecurityGroupIngress", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	g := r.sgs[deref(in.GroupId)]
	if g == nil {
		return nil, apiError("InvalidGroup.NotFound", "the security group does not exist")
	}
	for _, rule := range rulesOf(in.IpPermissions) {
		if slices.Contains(g.ingress, rule) {
			return nil, apiError("InvalidPermission.Duplicate", "the specified rule already exists")
		}
		g.ingress = append(g.ingress, rule)
	}
	return &ec2.AuthorizeSecurityGroupIngressOutput{Return: new(true)}, nil
}

// RevokeSecurityGroupIngress implements EC2API.
func (e *EC2) RevokeSecurityGroupIngress(ctx context.Context, in *ec2.RevokeSecurityGroupIngressInput, _ ...func(*ec2.Options),
) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	r, err := e.begin(ctx, "RevokeSecurityGroupIngress", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	g := r.sgs[deref(in.GroupId)]
	if g == nil {
		return nil, apiError("InvalidGroup.NotFound", "the security group does not exist")
	}
	for _, rule := range rulesOf(in.IpPermissions) {
		g.ingress = slices.DeleteFunc(g.ingress, func(x sgRule) bool { return x == rule })
	}
	return &ec2.RevokeSecurityGroupIngressOutput{Return: new(true)}, nil
}

// DeleteSecurityGroup implements EC2API: a group that an instance uses, or that
// another group's rule names, is a dependency violation.
func (e *EC2) DeleteSecurityGroup(ctx context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options),
) (*ec2.DeleteSecurityGroupOutput, error) {
	r, err := e.begin(ctx, "DeleteSecurityGroup", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	id := deref(in.GroupId)
	if _, ok := r.sgs[id]; !ok {
		return nil, apiError("InvalidGroup.NotFound", fmt.Sprintf("The security group '%s' does not exist", id))
	}
	for _, i := range r.insts {
		if i.live() && slices.Contains(i.groups, id) {
			return nil, apiError("DependencyViolation", fmt.Sprintf("resource %s has a dependent object", id))
		}
	}
	for gid, g := range r.sgs {
		if gid != id && slices.ContainsFunc(g.ingress, func(x sgRule) bool { return x.grp == id }) {
			return nil, apiError("DependencyViolation", fmt.Sprintf("resource %s has a dependent object", id))
		}
	}
	delete(r.sgs, id)
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

// SecurityGroupRules returns the ingress rules of a group as "proto:from-to:source".
func (a *Account) SecurityGroupRules(regionName, id string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.regions[regionName].sgs[id]
	if g == nil {
		return nil
	}
	var out []string
	for _, r := range g.ingress {
		src := r.cidr + r.cidr6 + r.grp
		out = append(out, fmt.Sprintf("%s:%d-%d:%s", r.protocol, r.from, r.to, src))
	}
	slices.Sort(out)
	return out
}

// ---- placement groups ----

func (g *placementGroup) attrs(name string) ([]string, bool) {
	switch name {
	case "group-name":
		return []string{g.name}, true
	case "strategy":
		return []string{g.strategy}, true
	}
	return nil, false
}

// DescribePlacementGroups implements EC2API.
func (e *EC2) DescribePlacementGroups(ctx context.Context, in *ec2.DescribePlacementGroupsInput, _ ...func(*ec2.Options),
) (*ec2.DescribePlacementGroupsOutput, error) {
	r, err := e.begin(ctx, "DescribePlacementGroups", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	var out []types.PlacementGroup
	for _, id := range sortedKeys(r.pgs) {
		g := r.pgs[id]
		if len(in.GroupIds) > 0 && !slices.Contains(in.GroupIds, id) {
			continue
		}
		if len(in.GroupNames) > 0 && !slices.Contains(in.GroupNames, g.name) {
			continue
		}
		ok, err := matches(in.Filters, g.tags, g.attrs)
		if err != nil {
			return nil, err
		}
		if ok {
			pg := types.PlacementGroup{GroupId: new(id), GroupName: new(g.name), Strategy: types.PlacementStrategy(g.strategy),
				State: types.PlacementGroupStateAvailable, Tags: tagList(g.tags)}
			if g.strategy == "partition" {
				pg.PartitionCount = new(g.partitions)
			}
			out = append(out, pg)
		}
	}
	return &ec2.DescribePlacementGroupsOutput{PlacementGroups: out}, nil
}

// CreatePlacementGroup implements EC2API.
func (e *EC2) CreatePlacementGroup(ctx context.Context, in *ec2.CreatePlacementGroupInput, _ ...func(*ec2.Options),
) (*ec2.CreatePlacementGroupOutput, error) {
	r, err := e.begin(ctx, "CreatePlacementGroup", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	name := deref(in.GroupName)
	if r.pgByName(name) != nil {
		return nil, apiError("InvalidPlacementGroup.Duplicate", fmt.Sprintf("The placement group '%s' already exists.", name))
	}
	switch in.Strategy {
	case types.PlacementStrategyCluster, types.PlacementStrategySpread, types.PlacementStrategyPartition:
	default:
		return nil, apiError("InvalidParameterValue", "invalid placement strategy "+string(in.Strategy))
	}
	if in.PartitionCount != nil && in.Strategy != types.PlacementStrategyPartition {
		return nil, apiError("InvalidParameterCombination", "PartitionCount is only valid for the partition strategy")
	}
	g := &placementGroup{id: strings.Replace(e.a.id("pg"), "pg-", "pg-", 1), name: name, strategy: string(in.Strategy),
		partitions: deref(in.PartitionCount), tags: tagsFromSpecs(in.TagSpecifications, types.ResourceTypePlacementGroup)}
	r.pgs[g.id] = g
	return &ec2.CreatePlacementGroupOutput{PlacementGroup: &types.PlacementGroup{
		GroupId: new(g.id), GroupName: new(name), Strategy: in.Strategy, Tags: tagList(g.tags)}}, nil
}

// DeletePlacementGroup implements EC2API: a group with instances is in use.
func (e *EC2) DeletePlacementGroup(ctx context.Context, in *ec2.DeletePlacementGroupInput, _ ...func(*ec2.Options),
) (*ec2.DeletePlacementGroupOutput, error) {
	r, err := e.begin(ctx, "DeletePlacementGroup", in)
	if err != nil {
		return nil, err
	}
	defer e.end()
	name := deref(in.GroupName)
	g := r.pgByName(name)
	if g == nil {
		return nil, apiError("InvalidPlacementGroup.Unknown", fmt.Sprintf("The placement group '%s' is unknown.", name))
	}
	for _, i := range r.insts {
		if i.pg == name && i.live() {
			return nil, apiError("InvalidPlacementGroup.InUse", fmt.Sprintf("The placement group '%s' is in use and may not be deleted.", name))
		}
	}
	delete(r.pgs, g.id)
	return &ec2.DeletePlacementGroupOutput{}, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// AddRoute adds a route to a route table, for tests that need a stale one.
func (a *Account) AddRoute(regionName, tableID, destination, gateway string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.regions[regionName].rts[tableID]
	t.routes = append(t.routes, destination+"->"+gateway)
}

// SetSubnetIPv6Associating gives a subnet the /64 with the index and shows it as
// still being associated.
func (a *Account) SetSubnetIPv6Associating(regionName, subnetID string, index int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.regions[regionName]
	sn := r.subnets[subnetID]
	sn.ipv6, sn.ipv6Associating = subnetIPv6(r.vpcs[sn.vpc].ipv6, index), true
}
