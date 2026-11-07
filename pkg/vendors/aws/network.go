/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// managedVPCCIDR is the address range of a managed VPC; each zone gets a /20.
const managedVPCCIDR = "10.240.0.0/16"

// subnetTTL is how long the driver trusts what it read about a subnet (overview
// section 11.2).
const subnetTTL = 5 * time.Minute

// subnetInfo is what the launch needs to know about a subnet.
type subnetInfo struct {
	id, vpc, zone string
	free          int32
}

type cachedSubnet struct {
	info  subnetInfo
	until time.Time
}

func (d *Driver) rememberSubnet(s ec2types.Subnet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subnets[deref(s.SubnetId)] = cachedSubnet{
		info: subnetInfo{id: deref(s.SubnetId), vpc: deref(s.VpcId), zone: deref(s.AvailabilityZone),
			free: deref(s.AvailableIpAddressCount)},
		until: d.clock.Now().Add(subnetTTL),
	}
}

// forgetSubnet drops what is known about a subnet (it ran out of addresses).
func (d *Driver) forgetSubnet(id string) {
	d.mu.Lock()
	delete(d.subnets, id)
	d.mu.Unlock()
}

// subnetsByID returns the subnets, reading the ones not cached.
func (d *Driver) subnetsByID(ctx context.Context, cl Clients, ids []string) (map[string]subnetInfo, error) {
	out := map[string]subnetInfo{}
	var missing []string
	d.mu.Lock()
	for _, id := range ids {
		if c, ok := d.subnets[id]; ok && d.clock.Now().Before(c.until) {
			out[id] = c.info
		} else {
			missing = append(missing, id)
		}
	}
	d.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}
	subs, err := d.describeSubnetPages(ctx, cl, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("subnet-id"), Values: missing}},
	})
	if err != nil {
		return nil, err
	}
	for _, s := range subs {
		d.rememberSubnet(s)
		out[deref(s.SubnetId)] = subnetInfo{id: deref(s.SubnetId), vpc: deref(s.VpcId),
			zone: deref(s.AvailabilityZone), free: deref(s.AvailableIpAddressCount)}
	}
	return out, nil
}

// vpcSubnets returns the subnets of a VPC in the zone ("" for every zone).
func (d *Driver) vpcSubnets(ctx context.Context, cl Clients, vpcID, zone string) ([]subnetInfo, error) {
	filters := []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}}
	if zone != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("availability-zone"), Values: []string{zone}})
	}
	subs, err := d.describeSubnetPages(ctx, cl, &ec2.DescribeSubnetsInput{Filters: filters})
	if err != nil {
		return nil, err
	}
	out := make([]subnetInfo, 0, len(subs))
	for _, s := range subs {
		d.rememberSubnet(s)
		out = append(out, subnetInfo{id: deref(s.SubnetId), vpc: deref(s.VpcId), zone: deref(s.AvailabilityZone),
			free: deref(s.AvailableIpAddressCount)})
	}
	return out, nil
}

// chooseSubnet picks, among the resolved subnets (or VPCs of a managed network),
// the one in the zone with the most free addresses. A zone without one is
// unusable (overview section 11.2).
func (d *Driver) chooseSubnet(
	ctx context.Context, region, zone string, refs []vendors.ResourceRef) (subnetInfo, error,
) {
	cl, err := d.clients(ctx, region)
	if err != nil {
		return subnetInfo{}, err
	}
	var ids []string
	var cands []subnetInfo
	for _, r := range refs {
		if r.Kind != KindSubnets {
			continue
		}
		if strings.HasPrefix(r.ID, "vpc-") {
			subs, err := d.vpcSubnets(ctx, cl, r.ID, zone)
			if err != nil {
				return subnetInfo{}, err
			}
			cands = append(cands, subs...)
			continue
		}
		ids = append(ids, r.ID)
	}
	if len(ids) > 0 {
		known, err := d.subnetsByID(ctx, cl, ids)
		if err != nil {
			return subnetInfo{}, err
		}
		for _, id := range ids {
			if s, ok := known[id]; ok && s.zone == zone {
				cands = append(cands, s)
			}
		}
	}
	cands = slices.DeleteFunc(cands, func(s subnetInfo) bool { return s.zone != zone })
	if len(cands) == 0 {
		return subnetInfo{}, vendors.NewError(vendors.ZoneUnusable, codeNoSubnet,
			fmt.Errorf("no subnet in %s (%s)", zone, region))
	}
	slices.SortStableFunc(cands, func(a, b subnetInfo) int {
		if a.free != b.free {
			return int(b.free - a.free)
		}
		return strings.Compare(a.id, b.id)
	})
	return cands[0], nil
}

// launchSubnet returns the subnet of a launch: the one Prepare chose, or, for a
// launch without prepared resources, the default VPC's default subnet of the
// zone. A VPC reference (a managed network) is narrowed to its subnet in the
// zone.
func (d *Driver) launchSubnet(ctx context.Context, region, zone, hint string) (string, error) {
	if strings.HasPrefix(hint, "subnet-") {
		return hint, nil
	}
	var refs []vendors.ResourceRef
	if hint != "" {
		refs = []vendors.ResourceRef{{Kind: KindSubnets, Region: region, ID: hint}}
	} else {
		var err error
		if refs, err = d.Resolve(ctx, KindSubnets, region, vendors.UseRef{}); err != nil {
			return "", err
		}
	}
	s, err := d.chooseSubnet(ctx, region, zone, refs)
	if err != nil {
		return "", err
	}
	return s.id, nil
}

// ---- managed network ----

// networkName returns the Name tag of a part of the network.
func networkName(base, part string) string { return base + "-" + part }

// ensureNetwork finds or creates the managed VPC of the name with its subnets (one
// per zone), internet gateway and route table. Every part carries the ownership
// marker from the call that creates it, and the whole is completed on every call,
// so a creation interrupted half way is finished by the next one.
func (d *Driver) ensureNetwork(ctx context.Context, cl Clients, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	vpcs, err := cl.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: append(d.tagFilters(spec.Tags),
		ec2types.Filter{Name: aws.String("tag:Name"), Values: []string{spec.Name}})})
	if err != nil {
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	var vpc ec2types.Vpc
	created := false
	if len(vpcs.Vpcs) > 0 {
		vpc = vpcs.Vpcs[0]
	} else {
		out, err := cl.EC2.CreateVpc(ctx, &ec2.CreateVpcInput{
			CidrBlock: aws.String(managedVPCCIDR),
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeVpc,
				Tags: d.resourceTags(spec)}},
		})
		if err != nil {
			return vendors.ResourceRef{}, false, classify(err, errContext{})
		}
		vpc, created = *out.Vpc, true
	}
	if err := d.completeNetwork(ctx, cl, region, vpc, spec); err != nil {
		return vendors.ResourceRef{}, false, fmt.Errorf("completing the network %s: %w", spec.Name, err)
	}
	ref := vendors.ResourceRef{
		Kind: KindSubnets, Region: region, ID: deref(vpc.VpcId), Name: spec.Name, Managed: true,
		Tags: d.markerTags(vpc.Tags),
	}
	if created {
		ref.Tags = spec.Tags
	}
	return ref, created, nil
}

// ensureZoneSubnets returns the subnets of the VPC, one per usable zone, creating
// the missing ones.
func (d *Driver) ensureZoneSubnets(
	ctx context.Context, cl Clients, region, vpcID string, tagsFor func(string) []ec2types.Tag,
) ([]string, error) {
	zones, err := d.Zones(ctx, region)
	if err != nil {
		return nil, err
	}
	haveRaw, err := d.describeSubnetPages(ctx, cl, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		return nil, err
	}
	usedCIDRs := map[string]bool{}
	have := make([]subnetInfo, 0, len(haveRaw))
	for _, sn := range haveRaw {
		usedCIDRs[deref(sn.CidrBlock)] = true
		have = append(have, subnetInfo{id: deref(sn.SubnetId), zone: deref(sn.AvailabilityZone)})
	}
	var subnetIDs []string
	for _, z := range zones {
		idx := slices.IndexFunc(have, func(s subnetInfo) bool { return s.zone == z.Name })
		if idx >= 0 {
			subnetIDs = append(subnetIDs, have[idx].id)
			continue
		}
		// The first free /20: a zone that appears between others (the zones of an
		// account change) must not collide with a subnet that exists.
		cidr := ""
		for n := range 16 {
			if c := fmt.Sprintf("10.240.%d.0/20", n*16); !usedCIDRs[c] {
				cidr = c
				break
			}
		}
		if cidr == "" {
			return nil, vendors.Errorf(vendors.Fatal, "the managed network %s has no free /20 left for %s", vpcID, z.Name)
		}
		usedCIDRs[cidr] = true
		out, err := cl.EC2.CreateSubnet(ctx, &ec2.CreateSubnetInput{
			VpcId: aws.String(vpcID), AvailabilityZone: aws.String(z.Name), CidrBlock: aws.String(cidr),
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeSubnet, Tags: tagsFor(z.Name)}},
		})
		if err != nil {
			return nil, classify(err, errContext{})
		}
		d.rememberSubnet(*out.Subnet)
		subnetIDs = append(subnetIDs, deref(out.Subnet.SubnetId))
	}
	return subnetIDs, nil
}

// completeNetwork creates what the managed VPC lacks: a subnet per usable zone, an
// attached internet gateway, a route table with the default route to it,
// associated with every subnet.
func (d *Driver) completeNetwork(
	ctx context.Context, cl Clients, region string, vpc ec2types.Vpc, spec vendors.CreateSpec,
) error {
	vpcID := deref(vpc.VpcId)
	tagsFor := func(part string) []ec2types.Tag {
		s := spec
		s.Name = networkName(spec.Name, part)
		return d.resourceTags(s)
	}
	subnetIDs, err := d.ensureZoneSubnets(ctx, cl, region, vpcID, tagsFor)
	if err != nil {
		return err
	}
	gw, attached, err := d.findGateway(ctx, cl, vpcID, networkName(spec.Name, "igw"), spec)
	if err != nil {
		return err
	}
	if gw == "" {
		out, err := cl.EC2.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInternetGateway,
				Tags: tagsFor("igw")}},
		})
		if err != nil {
			return classify(err, errContext{})
		}
		gw = deref(out.InternetGateway.InternetGatewayId)
	}
	if !attached {
		if _, err := cl.EC2.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
			InternetGatewayId: aws.String(gw), VpcId: aws.String(vpcID),
		}); err != nil && !isCode(err, "Resource.AlreadyAssociated") {
			return classify(err, errContext{})
		}
	}

	rts, err := cl.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: []ec2types.Filter{
		{Name: aws.String("vpc-id"), Values: []string{vpcID}},
	}})
	if err != nil {
		return classify(err, errContext{})
	}
	var rt *ec2types.RouteTable
	associated := map[string]bool{}
	for i := range rts.RouteTables {
		t := rts.RouteTables[i]
		if len(t.Associations) > 0 && deref(t.Associations[0].Main) {
			continue
		}
		if owned(d.markerTags(t.Tags), d.cluster(spec)) {
			rt = &t
			for _, a := range t.Associations {
				associated[deref(a.SubnetId)] = true
			}
		}
	}
	if rt == nil {
		// No ClientToken: the engine serialises the creation, and a table that a timed-out
		// call created is found (by marker) and used or deleted with the network. A token
		// would be replayed by EC2 for a table that was deleted since.
		out, err := cl.EC2.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
			VpcId: aws.String(vpcID),
			TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeRouteTable,
				Tags: tagsFor("rt")}},
		})
		if err != nil {
			return classify(err, errContext{})
		}
		rt = out.RouteTable
	}
	// The default route is checked on every call: a creation that stopped after the
	// table would leave subnets without egress, and a route to a gateway that is not
	// this network's (a stale one) is a blackhole that is replaced.
	if err := d.ensureDefaultRoute(ctx, cl, rt, gw); err != nil {
		return err
	}
	for _, id := range subnetIDs {
		if associated[id] {
			continue
		}
		if _, err := cl.EC2.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
			RouteTableId: rt.RouteTableId, SubnetId: aws.String(id),
		}); err != nil {
			return classify(err, errContext{})
		}
	}
	return nil
}

func (d *Driver) cluster(spec vendors.CreateSpec) string { return spec.Tags[vendors.TagCluster] }

// findGateway returns the internet gateway of the managed network: the one
// attached to the VPC, else one of ours with the network's name that is not
// attached anywhere (an earlier attach that failed; without this each retry would
// create another gateway and leak toward the per-region quota). "" when none.
func (d *Driver) findGateway(
	ctx context.Context, cl Clients, vpcID, name string, spec vendors.CreateSpec,
) (id string, attached bool, err error) {
	out, err := cl.EC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: append(d.tagFilters(spec.Tags), ec2types.Filter{Name: aws.String("tag:Name"), Values: []string{name}}),
	})
	if err != nil {
		return "", false, classify(err, errContext{})
	}
	for _, g := range out.InternetGateways {
		for _, a := range g.Attachments {
			if deref(a.VpcId) == vpcID {
				return deref(g.InternetGatewayId), true, nil
			}
		}
	}
	for _, g := range out.InternetGateways {
		if len(g.Attachments) == 0 {
			return deref(g.InternetGatewayId), false, nil
		}
	}
	return "", false, nil
}

func (d *Driver) listNetworks(
	ctx context.Context, cl Clients, region string, filters []ec2types.Filter) ([]vendors.ResourceRef, error,
) {
	var out []vendors.ResourceRef
	in := &ec2.DescribeVpcsInput{Filters: filters}
	for {
		res, err := cl.EC2.DescribeVpcs(ctx, in)
		if err != nil {
			return nil, classify(err, errContext{})
		}
		for _, v := range res.Vpcs {
			tags := d.markerTags(v.Tags)
			if d.kindOfTags(tags, KindSubnets) != KindSubnets {
				continue
			}
			out = append(out, vendors.ResourceRef{
				Kind: KindSubnets, Region: region, ID: deref(v.VpcId), Name: nameOf(v.Tags),
				Managed: tags[vendors.TagManagedBy] == vendors.ManagedByValue, Tags: tags,
			})
		}
		if res.NextToken == nil {
			return out, nil
		}
		in.NextToken = res.NextToken
	}
}

// deleteNetwork deletes a managed VPC and its parts in dependency order:
// subnets, the route table (after its associations), the internet gateway
// (detached first), the VPC. Only parts that carry the ownership marker go; a
// part someone else added to the VPC makes the VPC's deletion a dependency
// violation, which is Retryable. Every step is idempotent, so a deletion
// interrupted half way continues on the next call.
func (d *Driver) deleteNetwork(ctx context.Context, cl Clients, ref vendors.ResourceRef) error {
	vpcs, err := cl.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{ref.ID}}},
	})
	if err != nil {
		return classify(err, errContext{})
	}
	if len(vpcs.Vpcs) == 0 {
		return nil
	}
	if !d.markedFor(vpcs.Vpcs[0].Tags, ref) {
		return vendors.Errorf(vendors.Fatal, "the VPC %s was not created by SCarpenter and is never deleted", ref.ID)
	}
	cluster := ref.Tags[vendors.TagCluster]
	subs, err := d.describeSubnetPages(ctx, cl, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{ref.ID}}},
	})
	if err != nil {
		return err
	}
	rts, err := cl.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{ref.ID}}},
	})
	if err != nil {
		return classify(err, errContext{})
	}
	// Associations of our route table go first: a subnet with one cannot be
	// deleted in the real service until it is removed (and the table cannot be).
	for _, t := range rts.RouteTables {
		if !owned(d.markerTags(t.Tags), cluster) {
			continue
		}
		for _, a := range t.Associations {
			if a.RouteTableAssociationId == nil || deref(a.Main) {
				continue
			}
			if _, err := cl.EC2.DisassociateRouteTable(ctx,
				&ec2.DisassociateRouteTableInput{AssociationId: a.RouteTableAssociationId}); err != nil &&
				!isCode(err, "InvalidAssociationID.NotFound") {
				return classify(err, errContext{})
			}
		}
	}
	for _, s := range subs {
		// A subnet someone else added to the VPC is left alone: the VPC's
		// deletion then fails with a dependency violation, which is retried.
		if !owned(d.markerTags(s.Tags), cluster) {
			continue
		}
		if _, err := cl.EC2.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: s.SubnetId}); err != nil &&
			!isCode(err, "InvalidSubnetID.NotFound") {
			return classify(err, errContext{})
		}
		d.forgetSubnet(deref(s.SubnetId))
	}
	for _, t := range rts.RouteTables {
		if !owned(d.markerTags(t.Tags), cluster) {
			continue
		}
		if _, err := cl.EC2.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: t.RouteTableId}); err != nil &&
			!isCode(err, "InvalidRouteTableID.NotFound") {
			return classify(err, errContext{})
		}
	}
	if err := d.deleteGateways(ctx, cl, ref.ID, cluster); err != nil {
		return err
	}
	if err := d.deleteStrayGateways(ctx, cl, ref); err != nil {
		return err
	}
	if _, err := cl.EC2.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(ref.ID)}); err != nil && !isCode(err,
		"InvalidVpcID.NotFound") {
		return classify(err, errContext{})
	}
	return nil
}

// deleteGateways detaches and deletes the VPC's internet gateways that carry the
// marker.
func (d *Driver) deleteGateways(ctx context.Context, cl Clients, vpcID, cluster string) error {
	gws, err := cl.EC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: []ec2types.Filter{{Name: aws.String("attachment.vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		return classify(err, errContext{})
	}
	for _, g := range gws.InternetGateways {
		if !owned(d.markerTags(g.Tags), cluster) {
			continue
		}
		if _, err := cl.EC2.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
			InternetGatewayId: g.InternetGatewayId, VpcId: aws.String(vpcID),
		}); err != nil && !isCode(err, "Gateway.NotAttached") {
			return classify(err, errContext{})
		}
		if err := d.deleteGateway(ctx, cl, g.InternetGatewayId); err != nil {
			return err
		}
	}
	return nil
}

// deleteGateway deletes one internet gateway; one that is gone is success.
func (d *Driver) deleteGateway(ctx context.Context, cl Clients, id *string) error {
	if _, err := cl.EC2.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{InternetGatewayId: id}); err != nil &&
		!isCode(err, "InvalidInternetGatewayID.NotFound") {
		return classify(err, errContext{})
	}
	return nil
}

// deleteStrayGateways deletes the unattached gateways of the network (a failed attach
// leaves one): found by marker and the network's gateway name.
func (d *Driver) deleteStrayGateways(ctx context.Context, cl Clients, ref vendors.ResourceRef) error {
	out, err := cl.EC2.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: append(d.tagFilters(ref.Tags), ec2types.Filter{Name: aws.String("tag:Name"),
			Values: []string{networkName(ref.Name, "igw")}}),
	})
	if err != nil {
		return classify(err, errContext{})
	}
	for _, g := range out.InternetGateways {
		if len(g.Attachments) == 0 && owned(d.markerTags(g.Tags), ref.Tags[vendors.TagCluster]) {
			if err := d.deleteGateway(ctx, cl, g.InternetGatewayId); err != nil {
				return err
			}
		}
	}
	return nil
}

// markedFor says whether the tags carry the ownership marker of the cluster the
// reference belongs to.
func (d *Driver) markedFor(tags []ec2types.Tag, ref vendors.ResourceRef) bool {
	cluster := ref.Tags[vendors.TagCluster]
	if d.cfg.ClusterName != "" && cluster != d.cfg.ClusterName {
		return false
	}
	return owned(d.markerTags(tags), cluster)
}

// ensureDefaultRoute makes the route table send 0.0.0.0/0 to the gateway.
func (d *Driver) ensureDefaultRoute(ctx context.Context, cl Clients, rt *ec2types.RouteTable, gw string) error {
	const dest = "0.0.0.0/0"
	existing := slices.IndexFunc(rt.Routes, func(r ec2types.Route) bool { return deref(r.DestinationCidrBlock) == dest })
	switch {
	case existing >= 0 && deref(rt.Routes[existing].GatewayId) == gw:
		return nil
	case existing >= 0:
		if _, err := cl.EC2.ReplaceRoute(ctx, &ec2.ReplaceRouteInput{
			RouteTableId: rt.RouteTableId, DestinationCidrBlock: aws.String(dest), GatewayId: aws.String(gw),
		}); err != nil {
			return classify(err, errContext{})
		}
	default:
		if _, err := cl.EC2.CreateRoute(ctx, &ec2.CreateRouteInput{
			RouteTableId: rt.RouteTableId, DestinationCidrBlock: aws.String(dest), GatewayId: aws.String(gw),
		}); err != nil && !isCode(err, "RouteAlreadyExists") {
			return classify(err, errContext{})
		}
	}
	return nil
}
