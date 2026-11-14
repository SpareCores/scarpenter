/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const subnetTTL = 5 * time.Minute

// subnetInfo is what a launch into a subnet depends on.
type subnetInfo struct {
	ID, VCN string
	// AD is the name of the availability domain of an AD-specific subnet, without the
	// tenancy prefix; "" for a regional subnet, which serves every availability domain.
	AD string
	// ProhibitPublic is prohibitPublicIpOnVnic: the VNIC cannot have a public address.
	ProhibitPublic bool
	// IPv6 says the subnet has an IPv6 prefix.
	IPv6 bool
}

func subnetInfoOf(s core.Subnet) subnetInfo {
	return subnetInfo{
		ID: deref(s.Id), VCN: deref(s.VcnId), AD: ADName(deref(s.AvailabilityDomain)),
		ProhibitPublic: deref(s.ProhibitPublicIpOnVnic), IPv6: deref(s.Ipv6CidrBlock) != "" || len(s.Ipv6CidrBlocks) > 0,
	}
}

func (d *Driver) rememberSubnet(region string, s core.Subnet) {
	d.mu.Lock()
	d.subnets[region+"/"+deref(s.Id)] = set(subnetInfoOf(s), d.clock.Now().Add(subnetTTL))
	d.mu.Unlock()
}

// subnet reads a subnet, from what Resolve saw a moment ago or a get. One that does not exist
// is a missing resource of the launch.
func (d *Driver) subnet(ctx context.Context, region, id string) (subnetInfo, error) {
	d.mu.Lock()
	c := d.subnets[region+"/"+id]
	d.mu.Unlock()
	if v, ok := c.get(d.clock.Now()); ok {
		return v, nil
	}
	res, err := d.api.Network.GetSubnet(ctx, region, core.GetSubnetRequest{SubnetId: &id})
	if err != nil {
		if isNotFound(err) {
			return subnetInfo{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
				fmt.Errorf("the subnet %s does not exist in %s", id, region))
		}
		return subnetInfo{}, classify(err, kindRead)
	}
	d.rememberSubnet(region, res.Subnet)
	return subnetInfoOf(res.Subnet), nil
}

// serves reports whether the subnet can host a node of the zone (a regional subnet serves every
// availability domain).
func (s subnetInfo) serves(zone string) bool { return s.AD == "" || s.AD == zone }

// honours reports whether a launch with the networking options can use the subnet: a public
// address needs a subnet that allows it, an IPv6 address one with a prefix.
func (s subnetInfo) honours(publicIPv4, publicIPv6 bool) bool {
	return (!publicIPv4 || !s.ProhibitPublic) && (!publicIPv6 || s.IPv6)
}

// subnetsOf reads the subnets the resolved references name.
func (d *Driver) subnetsOf(ctx context.Context, region string, refs []vendors.ResourceRef) ([]subnetInfo, error) {
	out := make([]subnetInfo, 0, len(refs))
	for _, r := range refs {
		if r.Kind != KindSubnets {
			continue
		}
		if strings.HasPrefix(r.ID, "ocid1.vcn.") {
			// A managed network: the subnets SCarpenter made in the VCN.
			subs, err := d.ownedSubnets(ctx, region, &r.ID)
			if err != nil {
				return nil, err
			}
			for _, s := range subs {
				d.rememberSubnet(region, s)
				out = append(out, subnetInfoOf(s))
			}
			continue
		}
		si, err := d.subnet(ctx, region, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	slices.SortFunc(out, func(a, b subnetInfo) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// chooseSubnet picks the subnet of a launch in the zone: among those that serve it and honour the
// options, a regional one before an AD-specific one. A zone no subnet can serve is skipped for this
// create (ResourceMissingInZone); the resource check reports it (ZoneGaps).
func (d *Driver) chooseSubnet(ctx context.Context, region, zone string, refs []vendors.ResourceRef,
	publicIPv4, publicIPv6 bool,
) (subnetInfo, error) {
	subs, err := d.subnetsOf(ctx, region, refs)
	if err != nil {
		return subnetInfo{}, err
	}
	if gap := subnetGap(subs, zone, publicIPv4, publicIPv6); gap != "" {
		return subnetInfo{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone, fmt.Errorf("%s", gap))
	}
	var fit []subnetInfo
	for _, s := range subs {
		if s.serves(zone) && s.honours(publicIPv4, publicIPv6) {
			fit = append(fit, s)
		}
	}
	slices.SortStableFunc(fit, func(a, b subnetInfo) int { return strings.Compare(a.AD, b.AD) }) // "" first
	return fit[0], nil
}

// subnetGap says why no subnet of the list can host a node of the zone, or "" when one can.
func subnetGap(subs []subnetInfo, zone string, publicIPv4, publicIPv6 bool) string {
	var serving []subnetInfo
	for _, s := range subs {
		if s.serves(zone) {
			serving = append(serving, s)
		}
	}
	switch {
	case len(subs) == 0:
		return "no subnet was resolved for the launch"
	case len(serving) == 0:
		return fmt.Sprintf("no subnet serves the availability domain %s (the subnets are AD-specific; add a regional "+
			"subnet or one in %s)", zone, zone)
	case slices.ContainsFunc(serving, func(s subnetInfo) bool { return s.honours(publicIPv4, publicIPv6) }):
		return ""
	case publicIPv4 && !slices.ContainsFunc(serving, func(s subnetInfo) bool { return !s.ProhibitPublic }):
		return fmt.Sprintf("the subnets in %s prohibit public IP addresses and networking.publicIPv4 is true: set "+
			"publicIPv4 to false and give the subnet a NAT gateway", zone)
	}
	return fmt.Sprintf("publicIPv6 needs a subnet with an IPv6 prefix; the subnets in %s have none", zone)
}

// ZoneGaps implements vendors.ZoneChecker: an availability domain no brought subnet can serve (the
// subnets are AD-specific for the other ADs, prohibit the public address the NodeClass asks for, or
// lack the IPv6 prefix it asks for) cannot host a node. A managed network is regional and public,
// so a region with nothing found has no gaps.
func (d *Driver) ZoneGaps(
	ctx context.Context, region string, s vendors.VendorSettings, found map[vendors.ResourceKind][]vendors.ResourceRef,
) (map[string]string, error) {
	if len(found[KindSubnets]) == 0 {
		return nil, nil
	}
	subs, err := d.subnetsOf(ctx, region, found[KindSubnets])
	if err != nil {
		return nil, err
	}
	zones, err := d.Zones(ctx, region)
	if err != nil {
		return nil, err
	}
	gaps := map[string]string{}
	for _, z := range zones {
		g := subnetGap(subs, z.Name, boolOr(s.Networking.PublicIPv4, true), boolOr(s.Networking.PublicIPv6, false))
		if g != "" {
			gaps[z.Name] = g
		}
	}
	return gaps, nil
}

var _ vendors.ZoneChecker = (*Driver)(nil)
