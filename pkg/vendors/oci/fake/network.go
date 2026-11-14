/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll,dupl // the fake repeats the SDK method signatures, and its list calls share a shape that differs in the fields they filter on
package fake

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
)

// The network objects of the fake: VCNs with their gateways, route tables and
// security lists, subnets and network security groups, created by the driver
// (a managed network) or seeded by a test (what a user brought). Every object has
// a region; a delete of an object something still uses is the 409 OCI answers.

type netStore struct {
	vcns      map[string]*core.Vcn
	subnets   map[string]*core.Subnet
	igws      map[string]*core.InternetGateway
	routes    map[string]*core.RouteTable
	lists     map[string]*core.SecurityList
	nsgs      map[string]*core.NetworkSecurityGroup
	nsgRules  map[string][]core.AddSecurityRuleDetails
	objRegion map[string]string
}

func newNetStore() netStore {
	return netStore{
		vcns: map[string]*core.Vcn{}, subnets: map[string]*core.Subnet{}, igws: map[string]*core.InternetGateway{},
		routes: map[string]*core.RouteTable{}, lists: map[string]*core.SecurityList{},
		nsgs: map[string]*core.NetworkSecurityGroup{}, nsgRules: map[string][]core.AddSecurityRuleDetails{},
		objRegion: map[string]string{},
	}
}

func (c *Cloud) stamp() *common.SDKTime { return &common.SDKTime{Time: c.now()} }

// checkNet is the common entry of the network calls.
func (c *Cloud) checkNet(ctx context.Context, op, regionName string) error {
	if err := c.enter(ctx, op); err != nil {
		return err
	}
	_, err := c.usable(regionName)
	return err
}

func (c *Cloud) inCompartment(id *string) error {
	if deref(id) != c.compartment {
		return notFound("compartment")
	}
	return nil
}

func (c *Cloud) vcnIn(regionName string, id *string) (*core.Vcn, error) {
	v, ok := c.net.vcns[deref(id)]
	if !ok || c.net.objRegion[deref(id)] != regionName {
		return nil, notFound("vcn " + deref(id))
	}
	return v, nil
}

func conflict(format string, a ...any) error {
	return Error(http.StatusConflict, "Conflict", fmt.Sprintf(format, a...))
}

// subnetUsers are the VNIC attachments in use (not detached) of the subnet.
func (c *Cloud) subnetUsers(subnetID string) int {
	n := 0
	for _, atts := range c.attach {
		for _, a := range atts {
			if deref(a.SubnetId) == subnetID && a.LifecycleState != core.VnicAttachmentLifecycleStateDetached {
				n++
			}
		}
	}
	return n
}

// nsgUsers are the live VNICs that carry the group.
func (c *Cloud) nsgUsers(nsgID string) []string {
	var out []string
	for id, v := range c.vnics {
		if !slices.Contains(v.NsgIds, nsgID) {
			continue
		}
		live := false
		for _, atts := range c.attach {
			for _, a := range atts {
				live = live || (deref(a.VnicId) == id && a.LifecycleState != core.VnicAttachmentLifecycleStateDetached)
			}
		}
		if live {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// SeedVcn adds a VCN a user brought and returns its OCID.
func (c *Cloud) SeedVcn(regionName, name, cidr string, tags map[string]string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID("vcn", regionName)
	c.net.vcns[id] = &core.Vcn{Id: &id, CompartmentId: &c.compartment, CidrBlock: &cidr, DisplayName: &name,
		FreeformTags: tags, LifecycleState: core.VcnLifecycleStateAvailable, TimeCreated: c.stamp()}
	c.net.objRegion[id] = regionName
	return id
}

// SeedSubnet adds a subnet a user brought: ad is "" for a regional subnet; ipv6
// gives it an IPv6 prefix; prohibitPublic sets prohibitPublicIpOnVnic.
func (c *Cloud) SeedSubnet(regionName, vcnID, name, cidr, ad string, tags map[string]string, ipv6, prohibitPublic bool,
) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addSubnet(regionName, vcnID, name, cidr, ad, tags, ipv6, prohibitPublic)
}

func (c *Cloud) addSubnet(regionName, vcnID, name, cidr, ad string, tags map[string]string, ipv6, prohibitPublic bool,
) string {
	id := c.nextID("subnet", regionName)
	s := &core.Subnet{Id: &id, CompartmentId: &c.compartment, VcnId: &vcnID, CidrBlock: &cidr, DisplayName: &name,
		FreeformTags: tags, LifecycleState: core.SubnetLifecycleStateAvailable, TimeCreated: c.stamp(),
		ProhibitPublicIpOnVnic: &prohibitPublic}
	if ad != "" {
		s.AvailabilityDomain = &ad
	}
	if ipv6 {
		s.Ipv6CidrBlock = new("2001:db8::/64")
	}
	c.net.subnets[id] = s
	c.net.objRegion[id] = regionName
	return id
}

// SeedNSG adds a network security group a user brought.
func (c *Cloud) SeedNSG(regionName, vcnID, name string, tags map[string]string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID("networksecuritygroup", regionName)
	c.net.nsgs[id] = &core.NetworkSecurityGroup{Id: &id, CompartmentId: &c.compartment, VcnId: &vcnID, DisplayName: &name,
		FreeformTags: tags, LifecycleState: core.NetworkSecurityGroupLifecycleStateAvailable, TimeCreated: c.stamp()}
	c.net.objRegion[id] = regionName
	return id
}

// NetworkCounts are the numbers of VCNs, subnets, internet gateways, route
// tables, security lists and network security groups of the fake.
func (c *Cloud) NetworkCounts() (vcns, subnets, gateways, routeTables, securityLists, groups int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.net.vcns), len(c.net.subnets), len(c.net.igws), len(c.net.routes), len(c.net.lists), len(c.net.nsgs)
}

// NSGRules returns the rules added to a group.
func (c *Cloud) NSGRules(id string) []core.AddSecurityRuleDetails {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.net.nsgRules[id])
}

// Subnet returns a subnet of the fake.
func (c *Cloud) Subnet(id string) (core.Subnet, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.net.subnets[id]
	if !ok {
		return core.Subnet{}, false
	}
	return *s, true
}

// VcnByName returns the VCN of a name (a test's lookup of a managed network).
func (c *Cloud) VcnByName(name string) (core.Vcn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range c.net.vcns {
		if deref(v.DisplayName) == name {
			return *v, true
		}
	}
	return core.Vcn{}, false
}

func (c *Cloud) CreateVcn(ctx context.Context, regionName string, req core.CreateVcnRequest) (core.CreateVcnResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "CreateVcn", regionName); err != nil {
		return core.CreateVcnResponse{}, err
	}
	d := req.CreateVcnDetails
	if err := c.inCompartment(d.CompartmentId); err != nil {
		return core.CreateVcnResponse{}, err
	}
	cidr := deref(d.CidrBlock)
	if len(d.CidrBlocks) > 0 {
		cidr = d.CidrBlocks[0]
	}
	if _, err := netip.ParsePrefix(cidr); err != nil {
		return core.CreateVcnResponse{}, invalid("cidrBlock %q is not a CIDR", cidr)
	}
	id := c.nextID("vcn", regionName)
	v := &core.Vcn{Id: &id, CompartmentId: &c.compartment, CidrBlock: &cidr, CidrBlocks: []string{cidr},
		DisplayName: d.DisplayName, FreeformTags: d.FreeformTags, LifecycleState: core.VcnLifecycleStateAvailable,
		TimeCreated: c.stamp()}
	c.net.vcns[id] = v
	c.net.objRegion[id] = regionName
	return core.CreateVcnResponse{Vcn: *v}, nil
}

func (c *Cloud) ListVcns(ctx context.Context, regionName string, req core.ListVcnsRequest) (core.ListVcnsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListVcns", regionName); err != nil {
		return core.ListVcnsResponse{}, err
	}
	if err := c.inCompartment(req.CompartmentId); err != nil {
		return core.ListVcnsResponse{}, err
	}
	var all []core.Vcn
	for id, v := range c.net.vcns {
		if c.net.objRegion[id] == regionName && (req.DisplayName == nil || deref(v.DisplayName) == *req.DisplayName) {
			all = append(all, *v)
		}
	}
	slices.SortFunc(all, func(a, b core.Vcn) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListVcnsResponse{Items: page, OpcNextPage: next}, nil
}

func (c *Cloud) DeleteVcn(ctx context.Context, regionName string, req core.DeleteVcnRequest) (core.DeleteVcnResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "DeleteVcn", regionName); err != nil {
		return core.DeleteVcnResponse{}, err
	}
	if _, err := c.vcnIn(regionName, req.VcnId); err != nil {
		return core.DeleteVcnResponse{}, err
	}
	id := deref(req.VcnId)
	for _, s := range c.net.subnets {
		if deref(s.VcnId) == id {
			return core.DeleteVcnResponse{}, conflict("The vcn %s still has subnets", id)
		}
	}
	for _, g := range c.net.igws {
		if deref(g.VcnId) == id {
			return core.DeleteVcnResponse{}, conflict("The vcn %s still has an internet gateway", id)
		}
	}
	for _, g := range c.net.nsgs {
		if deref(g.VcnId) == id {
			return core.DeleteVcnResponse{}, conflict("The vcn %s still has network security groups", id)
		}
	}
	for _, l := range c.net.lists {
		if deref(l.VcnId) == id && !strings.HasPrefix(deref(l.DisplayName), "Default ") {
			return core.DeleteVcnResponse{}, conflict("The vcn %s still has a security list", id)
		}
	}
	for _, r := range c.net.routes {
		if deref(r.VcnId) == id {
			return core.DeleteVcnResponse{}, conflict("The vcn %s still has a route table", id)
		}
	}
	delete(c.net.vcns, id)
	return core.DeleteVcnResponse{}, nil
}

func (c *Cloud) CreateInternetGateway(ctx context.Context, regionName string, req core.CreateInternetGatewayRequest,
) (core.CreateInternetGatewayResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "CreateInternetGateway", regionName); err != nil {
		return core.CreateInternetGatewayResponse{}, err
	}
	d := req.CreateInternetGatewayDetails
	if _, err := c.vcnIn(regionName, d.VcnId); err != nil {
		return core.CreateInternetGatewayResponse{}, err
	}
	id := c.nextID("internetgateway", regionName)
	g := &core.InternetGateway{Id: &id, CompartmentId: &c.compartment, VcnId: d.VcnId, DisplayName: d.DisplayName,
		FreeformTags: d.FreeformTags, IsEnabled: d.IsEnabled, LifecycleState: core.InternetGatewayLifecycleStateAvailable,
		TimeCreated: c.stamp()}
	c.net.igws[id] = g
	c.net.objRegion[id] = regionName
	return core.CreateInternetGatewayResponse{InternetGateway: *g}, nil
}

func (c *Cloud) ListInternetGateways(ctx context.Context, regionName string, req core.ListInternetGatewaysRequest,
) (core.ListInternetGatewaysResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListInternetGateways", regionName); err != nil {
		return core.ListInternetGatewaysResponse{}, err
	}
	var all []core.InternetGateway
	for id, g := range c.net.igws {
		if c.net.objRegion[id] == regionName && (req.VcnId == nil || deref(g.VcnId) == *req.VcnId) {
			all = append(all, *g)
		}
	}
	slices.SortFunc(all, func(a, b core.InternetGateway) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListInternetGatewaysResponse{Items: page, OpcNextPage: next}, nil
}

func (c *Cloud) DeleteInternetGateway(ctx context.Context, regionName string, req core.DeleteInternetGatewayRequest,
) (core.DeleteInternetGatewayResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "DeleteInternetGateway", regionName); err != nil {
		return core.DeleteInternetGatewayResponse{}, err
	}
	id := deref(req.IgId)
	if _, ok := c.net.igws[id]; !ok || c.net.objRegion[id] != regionName {
		return core.DeleteInternetGatewayResponse{}, notFound("internet gateway " + id)
	}
	for _, r := range c.net.routes {
		for _, rule := range r.RouteRules {
			if deref(rule.NetworkEntityId) == id {
				return core.DeleteInternetGatewayResponse{}, conflict("The internet gateway %s is a target of the route table %s", id, deref(r.Id))
			}
		}
	}
	delete(c.net.igws, id)
	return core.DeleteInternetGatewayResponse{}, nil
}

func (c *Cloud) CreateRouteTable(ctx context.Context, regionName string, req core.CreateRouteTableRequest,
) (core.CreateRouteTableResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "CreateRouteTable", regionName); err != nil {
		return core.CreateRouteTableResponse{}, err
	}
	d := req.CreateRouteTableDetails
	if _, err := c.vcnIn(regionName, d.VcnId); err != nil {
		return core.CreateRouteTableResponse{}, err
	}
	for _, rule := range d.RouteRules {
		if _, ok := c.net.igws[deref(rule.NetworkEntityId)]; !ok {
			return core.CreateRouteTableResponse{}, invalid("The route target %s does not exist", deref(rule.NetworkEntityId))
		}
	}
	id := c.nextID("routetable", regionName)
	r := &core.RouteTable{Id: &id, CompartmentId: &c.compartment, VcnId: d.VcnId, DisplayName: d.DisplayName,
		FreeformTags: d.FreeformTags, RouteRules: d.RouteRules, LifecycleState: core.RouteTableLifecycleStateAvailable,
		TimeCreated: c.stamp()}
	c.net.routes[id] = r
	c.net.objRegion[id] = regionName
	return core.CreateRouteTableResponse{RouteTable: *r}, nil
}

func (c *Cloud) ListRouteTables(ctx context.Context, regionName string, req core.ListRouteTablesRequest,
) (core.ListRouteTablesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListRouteTables", regionName); err != nil {
		return core.ListRouteTablesResponse{}, err
	}
	var all []core.RouteTable
	for id, r := range c.net.routes {
		if c.net.objRegion[id] == regionName && (req.VcnId == nil || deref(r.VcnId) == *req.VcnId) {
			all = append(all, *r)
		}
	}
	slices.SortFunc(all, func(a, b core.RouteTable) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListRouteTablesResponse{Items: page, OpcNextPage: next}, nil
}

func (c *Cloud) DeleteRouteTable(ctx context.Context, regionName string, req core.DeleteRouteTableRequest,
) (core.DeleteRouteTableResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "DeleteRouteTable", regionName); err != nil {
		return core.DeleteRouteTableResponse{}, err
	}
	id := deref(req.RtId)
	if _, ok := c.net.routes[id]; !ok || c.net.objRegion[id] != regionName {
		return core.DeleteRouteTableResponse{}, notFound("route table " + id)
	}
	for _, s := range c.net.subnets {
		if deref(s.RouteTableId) == id {
			return core.DeleteRouteTableResponse{}, conflict("The route table %s is used by the subnet %s", id, deref(s.Id))
		}
	}
	delete(c.net.routes, id)
	return core.DeleteRouteTableResponse{}, nil
}

func (c *Cloud) CreateSecurityList(ctx context.Context, regionName string, req core.CreateSecurityListRequest,
) (core.CreateSecurityListResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "CreateSecurityList", regionName); err != nil {
		return core.CreateSecurityListResponse{}, err
	}
	d := req.CreateSecurityListDetails
	if _, err := c.vcnIn(regionName, d.VcnId); err != nil {
		return core.CreateSecurityListResponse{}, err
	}
	id := c.nextID("securitylist", regionName)
	l := &core.SecurityList{Id: &id, CompartmentId: &c.compartment, VcnId: d.VcnId, DisplayName: d.DisplayName,
		FreeformTags: d.FreeformTags, EgressSecurityRules: d.EgressSecurityRules, IngressSecurityRules: d.IngressSecurityRules,
		LifecycleState: core.SecurityListLifecycleStateAvailable, TimeCreated: c.stamp()}
	c.net.lists[id] = l
	c.net.objRegion[id] = regionName
	return core.CreateSecurityListResponse{SecurityList: *l}, nil
}

func (c *Cloud) ListSecurityLists(ctx context.Context, regionName string, req core.ListSecurityListsRequest,
) (core.ListSecurityListsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListSecurityLists", regionName); err != nil {
		return core.ListSecurityListsResponse{}, err
	}
	var all []core.SecurityList
	for id, l := range c.net.lists {
		if c.net.objRegion[id] == regionName && (req.VcnId == nil || deref(l.VcnId) == *req.VcnId) {
			all = append(all, *l)
		}
	}
	slices.SortFunc(all, func(a, b core.SecurityList) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListSecurityListsResponse{Items: page, OpcNextPage: next}, nil
}

func (c *Cloud) DeleteSecurityList(ctx context.Context, regionName string, req core.DeleteSecurityListRequest,
) (core.DeleteSecurityListResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "DeleteSecurityList", regionName); err != nil {
		return core.DeleteSecurityListResponse{}, err
	}
	id := deref(req.SecurityListId)
	if _, ok := c.net.lists[id]; !ok || c.net.objRegion[id] != regionName {
		return core.DeleteSecurityListResponse{}, notFound("security list " + id)
	}
	for _, s := range c.net.subnets {
		if slices.Contains(s.SecurityListIds, id) {
			return core.DeleteSecurityListResponse{}, conflict("The security list %s is used by the subnet %s", id, deref(s.Id))
		}
	}
	delete(c.net.lists, id)
	return core.DeleteSecurityListResponse{}, nil
}

func (c *Cloud) CreateSubnet(ctx context.Context, regionName string, req core.CreateSubnetRequest,
) (core.CreateSubnetResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "CreateSubnet", regionName); err != nil {
		return core.CreateSubnetResponse{}, err
	}
	d := req.CreateSubnetDetails
	v, err := c.vcnIn(regionName, d.VcnId)
	if err != nil {
		return core.CreateSubnetResponse{}, err
	}
	sub, err := netip.ParsePrefix(deref(d.CidrBlock))
	if err != nil {
		return core.CreateSubnetResponse{}, invalid("cidrBlock %q is not a CIDR", deref(d.CidrBlock))
	}
	if vp, _ := netip.ParsePrefix(deref(v.CidrBlock)); !vp.Contains(sub.Addr()) || vp.Bits() > sub.Bits() {
		return core.CreateSubnetResponse{}, invalid("The subnet range %s is not inside the vcn range %s", sub, vp)
	}
	for _, s := range c.net.subnets {
		if deref(s.VcnId) == deref(d.VcnId) {
			if o, _ := netip.ParsePrefix(deref(s.CidrBlock)); o.Overlaps(sub) {
				return core.CreateSubnetResponse{}, conflict("The subnet range %s overlaps the subnet %s", sub, deref(s.Id))
			}
		}
	}
	id := c.addSubnet(regionName, deref(d.VcnId), deref(d.DisplayName), deref(d.CidrBlock), deref(d.AvailabilityDomain),
		d.FreeformTags, false, deref(d.ProhibitPublicIpOnVnic))
	s := c.net.subnets[id]
	s.RouteTableId, s.SecurityListIds = d.RouteTableId, slices.Clone(d.SecurityListIds)
	return core.CreateSubnetResponse{Subnet: *s}, nil
}

func (c *Cloud) GetSubnet(ctx context.Context, regionName string, req core.GetSubnetRequest) (core.GetSubnetResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "GetSubnet", regionName); err != nil {
		return core.GetSubnetResponse{}, err
	}
	id := deref(req.SubnetId)
	s, ok := c.net.subnets[id]
	if !ok || c.net.objRegion[id] != regionName {
		return core.GetSubnetResponse{}, notFound("subnet " + id)
	}
	return core.GetSubnetResponse{Subnet: *s}, nil
}

func (c *Cloud) ListSubnets(ctx context.Context, regionName string, req core.ListSubnetsRequest) (core.ListSubnetsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListSubnets", regionName); err != nil {
		return core.ListSubnetsResponse{}, err
	}
	if err := c.inCompartment(req.CompartmentId); err != nil {
		return core.ListSubnetsResponse{}, err
	}
	var all []core.Subnet
	for id, s := range c.net.subnets {
		switch {
		case c.net.objRegion[id] != regionName:
		case req.VcnId != nil && deref(s.VcnId) != *req.VcnId:
		case req.DisplayName != nil && deref(s.DisplayName) != *req.DisplayName:
		case req.LifecycleState != "" && s.LifecycleState != req.LifecycleState:
		default:
			all = append(all, *s)
		}
	}
	slices.SortFunc(all, func(a, b core.Subnet) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListSubnetsResponse{Items: page, OpcNextPage: next}, nil
}

func (c *Cloud) DeleteSubnet(ctx context.Context, regionName string, req core.DeleteSubnetRequest) (core.DeleteSubnetResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "DeleteSubnet", regionName); err != nil {
		return core.DeleteSubnetResponse{}, err
	}
	id := deref(req.SubnetId)
	if _, ok := c.net.subnets[id]; !ok || c.net.objRegion[id] != regionName {
		return core.DeleteSubnetResponse{}, notFound("subnet " + id)
	}
	if c.subnetUsers(id) > 0 {
		return core.DeleteSubnetResponse{}, conflict("The subnet %s still has VNICs attached", id)
	}
	delete(c.net.subnets, id)
	return core.DeleteSubnetResponse{}, nil
}

func (c *Cloud) CreateNetworkSecurityGroup(ctx context.Context, regionName string, req core.CreateNetworkSecurityGroupRequest,
) (core.CreateNetworkSecurityGroupResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "CreateNetworkSecurityGroup", regionName); err != nil {
		return core.CreateNetworkSecurityGroupResponse{}, err
	}
	d := req.CreateNetworkSecurityGroupDetails
	if _, err := c.vcnIn(regionName, d.VcnId); err != nil {
		return core.CreateNetworkSecurityGroupResponse{}, err
	}
	id := c.nextID("networksecuritygroup", regionName)
	g := &core.NetworkSecurityGroup{Id: &id, CompartmentId: &c.compartment, VcnId: d.VcnId, DisplayName: d.DisplayName,
		FreeformTags: d.FreeformTags, LifecycleState: core.NetworkSecurityGroupLifecycleStateAvailable, TimeCreated: c.stamp()}
	c.net.nsgs[id] = g
	c.net.objRegion[id] = regionName
	return core.CreateNetworkSecurityGroupResponse{NetworkSecurityGroup: *g}, nil
}

func (c *Cloud) GetNetworkSecurityGroup(ctx context.Context, regionName string, req core.GetNetworkSecurityGroupRequest,
) (core.GetNetworkSecurityGroupResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "GetNetworkSecurityGroup", regionName); err != nil {
		return core.GetNetworkSecurityGroupResponse{}, err
	}
	id := deref(req.NetworkSecurityGroupId)
	g, ok := c.net.nsgs[id]
	if !ok || c.net.objRegion[id] != regionName {
		return core.GetNetworkSecurityGroupResponse{}, notFound("network security group " + id)
	}
	return core.GetNetworkSecurityGroupResponse{NetworkSecurityGroup: *g}, nil
}

func (c *Cloud) ListNetworkSecurityGroups(ctx context.Context, regionName string, req core.ListNetworkSecurityGroupsRequest,
) (core.ListNetworkSecurityGroupsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListNetworkSecurityGroups", regionName); err != nil {
		return core.ListNetworkSecurityGroupsResponse{}, err
	}
	if err := c.inCompartment(req.CompartmentId); err != nil {
		return core.ListNetworkSecurityGroupsResponse{}, err
	}
	var all []core.NetworkSecurityGroup
	for id, g := range c.net.nsgs {
		switch {
		case c.net.objRegion[id] != regionName:
		case req.VcnId != nil && deref(g.VcnId) != *req.VcnId:
		case req.DisplayName != nil && deref(g.DisplayName) != *req.DisplayName:
		case req.LifecycleState != "" && g.LifecycleState != req.LifecycleState:
		default:
			all = append(all, *g)
		}
	}
	slices.SortFunc(all, func(a, b core.NetworkSecurityGroup) int { return strings.Compare(deref(a.Id), deref(b.Id)) })
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListNetworkSecurityGroupsResponse{Items: page, OpcNextPage: next}, nil
}

func (c *Cloud) DeleteNetworkSecurityGroup(ctx context.Context, regionName string, req core.DeleteNetworkSecurityGroupRequest,
) (core.DeleteNetworkSecurityGroupResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "DeleteNetworkSecurityGroup", regionName); err != nil {
		return core.DeleteNetworkSecurityGroupResponse{}, err
	}
	id := deref(req.NetworkSecurityGroupId)
	if _, ok := c.net.nsgs[id]; !ok || c.net.objRegion[id] != regionName {
		return core.DeleteNetworkSecurityGroupResponse{}, notFound("network security group " + id)
	}
	if len(c.nsgUsers(id)) > 0 {
		return core.DeleteNetworkSecurityGroupResponse{}, conflict("The network security group %s is used by VNICs", id)
	}
	delete(c.net.nsgs, id)
	delete(c.net.nsgRules, id)
	return core.DeleteNetworkSecurityGroupResponse{}, nil
}

func (c *Cloud) AddNetworkSecurityGroupSecurityRules(ctx context.Context, regionName string,
	req core.AddNetworkSecurityGroupSecurityRulesRequest,
) (core.AddNetworkSecurityGroupSecurityRulesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "AddNetworkSecurityGroupSecurityRules", regionName); err != nil {
		return core.AddNetworkSecurityGroupSecurityRulesResponse{}, err
	}
	id := deref(req.NetworkSecurityGroupId)
	if _, ok := c.net.nsgs[id]; !ok || c.net.objRegion[id] != regionName {
		return core.AddNetworkSecurityGroupSecurityRulesResponse{}, notFound("network security group " + id)
	}
	if len(req.SecurityRules) > 25 {
		return core.AddNetworkSecurityGroupSecurityRulesResponse{}, invalid("At most 25 rules per call")
	}
	c.net.nsgRules[id] = append(c.net.nsgRules[id], req.SecurityRules...)
	return core.AddNetworkSecurityGroupSecurityRulesResponse{}, nil
}

func (c *Cloud) ListNetworkSecurityGroupVnics(ctx context.Context, regionName string,
	req core.ListNetworkSecurityGroupVnicsRequest,
) (core.ListNetworkSecurityGroupVnicsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListNetworkSecurityGroupVnics", regionName); err != nil {
		return core.ListNetworkSecurityGroupVnicsResponse{}, err
	}
	id := deref(req.NetworkSecurityGroupId)
	if _, ok := c.net.nsgs[id]; !ok || c.net.objRegion[id] != regionName {
		return core.ListNetworkSecurityGroupVnicsResponse{}, notFound("network security group " + id)
	}
	var all []core.NetworkSecurityGroupVnic
	for _, v := range c.nsgUsers(id) {
		all = append(all, core.NetworkSecurityGroupVnic{VnicId: new(v)})
	}
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListNetworkSecurityGroupVnicsResponse{Items: page, OpcNextPage: next}, nil
}

func (c *Cloud) ListNetworkSecurityGroupSecurityRules(ctx context.Context, regionName string,
	req core.ListNetworkSecurityGroupSecurityRulesRequest,
) (core.ListNetworkSecurityGroupSecurityRulesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkNet(ctx, "ListNetworkSecurityGroupSecurityRules", regionName); err != nil {
		return core.ListNetworkSecurityGroupSecurityRulesResponse{}, err
	}
	id := deref(req.NetworkSecurityGroupId)
	if _, ok := c.net.nsgs[id]; !ok || c.net.objRegion[id] != regionName {
		return core.ListNetworkSecurityGroupSecurityRulesResponse{}, notFound("network security group " + id)
	}
	var all []core.SecurityRule
	for i, r := range c.net.nsgRules[id] {
		all = append(all, core.SecurityRule{Direction: core.SecurityRuleDirectionEnum(r.Direction), Protocol: r.Protocol,
			Id: new(fmt.Sprintf("%s-rule-%d", id, i)), Source: r.Source, Destination: r.Destination})
	}
	page, next := paged(all, c.pageSize, req.Page)
	return core.ListNetworkSecurityGroupSecurityRulesResponse{Items: page, OpcNextPage: next}, nil
}

// SetCreated sets the creation time of a subnet or a network security group.
func (c *Cloud) SetCreated(id string, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &common.SDKTime{Time: t}
	if s, ok := c.net.subnets[id]; ok {
		s.TimeCreated = st
	}
	if g, ok := c.net.nsgs[id]; ok {
		g.TimeCreated = st
	}
}
