/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The managed network of a NodeClass without `use` (Azure has no default
// resource): a virtual network and one subnet, IPv4 only.
const (
	managedVNetPrefix  = "10.240.0.0/16"
	managedSubnetName  = "nodes"
	managedSubnetRange = "10.240.0.0/20"
	// asgSuffix is the name of the application security group of a managed
	// network security group, after the group's own name.
	asgSuffix = "-nodes"
	// Priorities of the rules of a managed network security group: the allow
	// between the nodes of the group, then the inbound rules of the NodeClass, then
	// the deny that closes the virtual network's built-in allow (priority 65000).
	priorityNodes   = 50
	priorityInbound = 100
	priorityDeny    = 4000
	// codeInUse is the code of the Retryable error of the deletion of a resource
	// something still uses.
	codeInUse = "ResourceInUse"
)

// ResourceParams are the create parameters of a managed resource (Params of the
// ResourceRequest, which the resource engine hashes into the resource's name: a
// changed rule set is a new group, the old one is collected by its lifecycle).
type ResourceParams struct {
	// ResourceGroup holds the resource (not for a resource group itself).
	ResourceGroup string `json:"resourceGroup,omitempty"`
	// SelfReference allows the nodes of the group to reach each other: the rule of
	// isolation nodeclass (the NodeClass's group) and of isolation run (the run's
	// group).
	SelfReference bool `json:"selfReference,omitempty"`
	// Inbound are the rules of spec.networking.inbound.
	Inbound []common.InboundRule `json:"inbound,omitempty"`
	// Strategy is the placement strategy of a proximity placement group.
	Strategy string `json:"strategy,omitempty"`
}

// Kinds implements vendors.ManagedResources. The deletion order is placement
// groups and run groups first (nothing depends on them but instances), then the
// NodeClass groups, then the network, then the resource groups, which hold them.
func (d *Driver) Kinds() []vendors.KindSpec {
	return []vendors.KindSpec{
		{Kind: KindPlacement, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 10},
		{Kind: KindRunSecurityGroups, Scope: vendors.ScopeRun, DefaultPolicy: common.LifecyclePerUse, DeleteOrder: 20},
		{Kind: KindSecurityGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 30},
		{Kind: KindSubnets, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleIdleTTL, DeleteOrder: 40},
		{Kind: KindResourceGroups, Scope: vendors.ScopeRegion, DefaultPolicy: common.LifecycleRetain, DeleteOrder: 50},
	}
}

func isKind(kind vendors.ResourceKind) bool {
	switch kind {
	case KindResourceGroups, KindSubnets, KindSecurityGroups, KindRunSecurityGroups, KindPlacement:
		return true
	}
	return false
}

// azureTags returns the tags of a managed resource: the NodeClass's own tags,
// then the ownership marker and the name, which win.
func azureTags(spec vendors.CreateSpec) map[string]*string {
	m := map[string]string{}
	maps.Copy(m, spec.ExtraTags)
	m[TagNameName] = spec.Name
	for k, v := range spec.Tags {
		m[TagKey(k)] = v
	}
	return tagPtrs(m)
}

// ownedBy says whether the tags carry the ownership marker of the cluster:
// cluster and managed-by (the NodeClass and kind tags are descriptive).
func ownedBy(tags map[string]*string, cluster string) bool {
	t := LogicalTags(tags)
	return t[vendors.TagManagedBy] == vendors.ManagedByValue && t[vendors.TagCluster] == cluster && cluster != ""
}

// markerTags are the logical tags a managed resource carries.
func markerTags(tags map[string]*string) map[vendors.TagKey]string {
	have := LogicalTags(tags)
	out := map[vendors.TagKey]string{}
	for _, k := range vendors.ResourceTagKeys {
		if v, ok := have[k]; ok {
			out[k] = v
		}
	}
	return out
}

// tagsMatch says whether the tags carry all the wanted logical tags.
func tagsMatch(tags map[string]*string, want map[vendors.TagKey]string) bool {
	have := LogicalTags(tags)
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (d *Driver) params(spec vendors.CreateSpec, needGroup bool) (ResourceParams, error) {
	var p ResourceParams
	if len(spec.Params) > 0 {
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return p, invalidf("the create parameters of %s are not valid: %v", spec.Name, err)
		}
	}
	if needGroup && p.ResourceGroup == "" {
		return p, invalidf("creating %s needs the parameter resourceGroup", spec.Name)
	}
	return p, nil
}

// Ensure implements vendors.ManagedResources: find by name and ownership marker,
// else create with the marker in the creating call. created is false when an
// existing resource was adopted; a resource of the name that does not carry the
// marker is never adopted (NameConflict).
func (d *Driver) Ensure(
	ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	if !isKind(kind) {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "the Azure driver has no resource kind %q", kind)
	}
	if spec.Name == "" {
		return vendors.ResourceRef{}, false, vendors.Errorf(vendors.Fatal, "a managed resource needs a name")
	}
	switch kind {
	case KindResourceGroups:
		return d.ensureGroup(ctx, region, spec)
	case KindSubnets:
		return d.ensureNetwork(ctx, region, spec)
	case KindSecurityGroups, KindRunSecurityGroups:
		return d.ensureSecurityGroup(ctx, kind, region, spec)
	}
	return d.ensurePlacementGroup(ctx, region, spec)
}

func conflict(kind vendors.ResourceKind, name string) error {
	return vendors.NewError(vendors.Fatal, codeNameConflict,
		fmt.Errorf("a %s named %s exists without SCarpenter's ownership marker: refusing to adopt it", kind, name))
}

func (d *Driver) ensureGroup(ctx context.Context, region string, spec vendors.CreateSpec) (vendors.ResourceRef, bool, error) {
	ref := func(g armresources.ResourceGroup) vendors.ResourceRef {
		return vendors.ResourceRef{Kind: KindResourceGroups, Region: region, ID: spec.Name, Name: spec.Name, Managed: true,
			Tags: markerTags(g.Tags)}
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	g, err := d.clients.Groups.Get(cctx, spec.Name)
	switch {
	case err == nil:
		if !ownedBy(g.Tags, d.cfg.ClusterName) {
			return vendors.ResourceRef{}, false, conflict(KindResourceGroups, spec.Name)
		}
		return ref(g), false, nil
	case !isNotFound(err):
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	g, err = d.clients.Groups.CreateOrUpdate(cctx, spec.Name, armresources.ResourceGroup{
		Location: new(region), Tags: azureTags(spec),
	})
	if err != nil {
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	return ref(g), true, nil
}

func (d *Driver) ensureNetwork(ctx context.Context, region string, spec vendors.CreateSpec) (vendors.ResourceRef, bool, error) {
	p, err := d.params(spec, true)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	ref := func(v armnetwork.VirtualNetwork) vendors.ResourceRef {
		return vendors.ResourceRef{Kind: KindSubnets, Region: region, ID: deref(v.ID) + "/subnets/" + managedSubnetName,
			Name: spec.Name, Managed: true, Tags: markerTags(v.Tags), Network: deref(v.ID)}
	}
	cctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	v, err := d.clients.VNets.Get(cctx, p.ResourceGroup, spec.Name)
	switch {
	case err == nil:
		if !ownedBy(v.Tags, d.cfg.ClusterName) {
			return vendors.ResourceRef{}, false, conflict(KindSubnets, spec.Name)
		}
		return ref(v), false, nil
	case !isNotFound(err):
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	v, err = d.clients.VNets.CreateOrUpdate(cctx, p.ResourceGroup, spec.Name, armnetwork.VirtualNetwork{
		Location: new(region), Tags: azureTags(spec),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{new(managedVNetPrefix)}},
			Subnets: []*armnetwork.Subnet{{
				Name:       new(managedSubnetName),
				Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new(managedSubnetRange)},
			}},
		},
	})
	if err != nil {
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	return ref(v), true, nil
}

// securityRules builds the rules of a managed network security group: the allow
// between the nodes of its application security group (SelfReference), the
// inbound rules of the NodeClass at 100 and up, and the deny of the virtual
// network's traffic, which closes the built-in AllowVnetInBound.
func securityRules(p ResourceParams, asgID string) []*armnetwork.SecurityRule {
	var rules []*armnetwork.SecurityRule
	if p.SelfReference && asgID != "" {
		rules = append(rules, &armnetwork.SecurityRule{
			Name: new("allow-nodes"),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Access: new(armnetwork.SecurityRuleAccessAllow), Direction: new(armnetwork.SecurityRuleDirectionInbound),
				Priority: new(int32(priorityNodes)), Protocol: new(armnetwork.SecurityRuleProtocolAsterisk),
				SourcePortRange: new("*"), DestinationPortRange: new("*"),
				SourceApplicationSecurityGroups:      []*armnetwork.ApplicationSecurityGroup{{ID: new(asgID)}},
				DestinationApplicationSecurityGroups: []*armnetwork.ApplicationSecurityGroup{{ID: new(asgID)}},
			},
		})
	}
	for i, r := range p.Inbound {
		ports := "*"
		if r.FromPort != nil {
			to := *r.FromPort
			if r.ToPort != nil {
				to = *r.ToPort
			}
			ports = fmt.Sprint(*r.FromPort)
			if to != *r.FromPort {
				ports = fmt.Sprintf("%d-%d", *r.FromPort, to)
			}
		}
		proto := armnetwork.SecurityRuleProtocolAsterisk
		switch r.Protocol {
		case common.Protocol("tcp"):
			proto = armnetwork.SecurityRuleProtocolTCP
		case common.Protocol("udp"):
			proto = armnetwork.SecurityRuleProtocolUDP
		case common.Protocol("icmp"):
			proto, ports = armnetwork.SecurityRuleProtocolIcmp, "*"
		}
		prefixes := make([]*string, 0, len(r.CIDRs))
		for _, c := range r.CIDRs {
			prefixes = append(prefixes, new(c))
		}
		rules = append(rules, &armnetwork.SecurityRule{
			Name: new(fmt.Sprintf("inbound-%d", i+1)),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Access: new(armnetwork.SecurityRuleAccessAllow), Direction: new(armnetwork.SecurityRuleDirectionInbound),
				Priority: new(int32(priorityInbound + i)), Protocol: new(proto),
				SourcePortRange: new("*"), DestinationPortRange: new(ports), DestinationAddressPrefix: new("*"),
				SourceAddressPrefixes: prefixes,
			},
		})
	}
	return append(rules, &armnetwork.SecurityRule{
		Name: new("deny-vnet-inbound"),
		Properties: &armnetwork.SecurityRulePropertiesFormat{
			Access: new(armnetwork.SecurityRuleAccessDeny), Direction: new(armnetwork.SecurityRuleDirectionInbound),
			Priority: new(int32(priorityDeny)), Protocol: new(armnetwork.SecurityRuleProtocolAsterisk),
			SourcePortRange: new("*"), DestinationPortRange: new("*"),
			SourceAddressPrefix: new("VirtualNetwork"), DestinationAddressPrefix: new("*"),
		},
	})
}

// ASGName is the name of the application security group of a managed network
// security group.
func ASGName(nsgName string) string { return nsgName + asgSuffix }

// ASGIDOf is the ARM ID of the application security group of a managed network
// security group, derived from the group's own ID (the same resource group, the
// name plus asgSuffix).
func ASGIDOf(nsgID string) string {
	r, err := arm.ParseResourceID(nsgID)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s",
		r.SubscriptionID, r.ResourceGroupName, ASGName(r.Name))
}

func (d *Driver) ensureSecurityGroup(
	ctx context.Context, kind vendors.ResourceKind, region string, spec vendors.CreateSpec,
) (vendors.ResourceRef, bool, error) {
	p, err := d.params(spec, true)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	ref := func(n armnetwork.SecurityGroup) vendors.ResourceRef {
		return vendors.ResourceRef{Kind: kind, Region: region, ID: deref(n.ID), Name: spec.Name, Managed: true,
			Tags: markerTags(n.Tags)}
	}
	cctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	n, err := d.clients.NSGs.Get(cctx, p.ResourceGroup, spec.Name)
	found := err == nil
	switch {
	case found:
		if !ownedBy(n.Tags, d.cfg.ClusterName) {
			return vendors.ResourceRef{}, false, conflict(kind, spec.Name)
		}
	case !isNotFound(err):
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	// The application security group first: the rule of the group names it, and a
	// creation interrupted after it is finished by the next call.
	asgID := ""
	if p.SelfReference {
		g, err := d.ensureASG(cctx, region, p.ResourceGroup, spec)
		if err != nil {
			return vendors.ResourceRef{}, false, err
		}
		asgID = deref(g.ID)
	}
	if found {
		return ref(n), false, nil
	}
	n, err = d.clients.NSGs.CreateOrUpdate(cctx, p.ResourceGroup, spec.Name, armnetwork.SecurityGroup{
		Location: new(region), Tags: azureTags(spec),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{SecurityRules: securityRules(p, asgID)},
	})
	if err != nil {
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	return ref(n), true, nil
}

func (d *Driver) ensureASG(
	ctx context.Context, region, rg string, spec vendors.CreateSpec,
) (armnetwork.ApplicationSecurityGroup, error) {
	name := ASGName(spec.Name)
	g, err := d.clients.ASGs.Get(ctx, rg, name)
	if err == nil {
		if !ownedBy(g.Tags, d.cfg.ClusterName) {
			return g, conflict(KindAppSecGroups, name)
		}
		return g, nil
	}
	if !isNotFound(err) {
		return g, classify(err, errContext{})
	}
	asgSpec := spec
	asgSpec.Name = name
	g, err = d.clients.ASGs.CreateOrUpdate(ctx, rg, name, armnetwork.ApplicationSecurityGroup{
		Location: new(region), Tags: azureTags(asgSpec),
	})
	return g, classify(err, errContext{})
}

func (d *Driver) ensurePlacementGroup(ctx context.Context, region string, spec vendors.CreateSpec) (vendors.ResourceRef, bool, error) {
	p, err := d.params(spec, true)
	if err != nil {
		return vendors.ResourceRef{}, false, err
	}
	if p.Strategy != "" && p.Strategy != "cluster" {
		return vendors.ResourceRef{}, false, invalidf("the proximity placement group strategy %q is not cluster", p.Strategy)
	}
	ref := func(g armcompute.ProximityPlacementGroup) vendors.ResourceRef {
		return vendors.ResourceRef{Kind: KindPlacement, Region: region, ID: deref(g.ID), Name: spec.Name, Managed: true,
			Tags: markerTags(g.Tags)}
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	g, err := d.clients.PPGs.Get(cctx, p.ResourceGroup, spec.Name)
	switch {
	case err == nil:
		if !ownedBy(g.Tags, d.cfg.ClusterName) {
			return vendors.ResourceRef{}, false, conflict(KindPlacement, spec.Name)
		}
		return ref(g), false, nil
	case !isNotFound(err):
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	g, err = d.clients.PPGs.CreateOrUpdate(cctx, p.ResourceGroup, spec.Name, armcompute.ProximityPlacementGroup{
		Location: new(region), Tags: azureTags(spec),
		Properties: &armcompute.ProximityPlacementGroupProperties{
			ProximityPlacementGroupType: new(armcompute.ProximityPlacementGroupTypeStandard),
		},
	})
	if err != nil {
		return vendors.ResourceRef{}, false, classify(err, errContext{})
	}
	return ref(g), true, nil
}

// List implements vendors.ManagedResources: the managed resources carrying all
// the tags, top-level resources only (the virtual network stands for its subnet,
// the network security group for its application security group). Regions the
// subscription does not have hold none.
func (d *Driver) List(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]vendors.ResourceRef, error) {
	inRegion := func(loc string) bool {
		return len(regions) == 0 || slices.ContainsFunc(regions, func(r string) bool { return sameLocation(loc, r) })
	}
	region := func(loc string) string {
		for _, r := range regions {
			if sameLocation(loc, r) {
				return r
			}
		}
		return strings.ToLower(strings.ReplaceAll(loc, " ", ""))
	}
	cctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	var out []vendors.ResourceRef
	add := func(kind vendors.ResourceKind, loc, id, name string, t map[string]*string, network string) {
		if !inRegion(loc) || !tagsMatch(t, tags) || !ownedBy(t, d.cfg.ClusterName) {
			return
		}
		m := markerTags(t)
		if k := vendors.ResourceKind(m[vendors.TagResource]); isKind(k) && (kind == KindSecurityGroups || k == kind) {
			kind = k
		}
		out = append(out, vendors.ResourceRef{Kind: kind, Region: region(loc), ID: id, Name: name, Managed: true, Tags: m,
			Network: network})
	}
	groups, err := d.clients.Groups.List(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	for _, g := range groups {
		add(KindResourceGroups, deref(g.Location), deref(g.Name), deref(g.Name), g.Tags, "")
	}
	vnets, err := d.clients.VNets.ListAll(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	for _, v := range vnets {
		add(KindSubnets, deref(v.Location), deref(v.ID)+"/subnets/"+managedSubnetName, deref(v.Name), v.Tags, deref(v.ID))
	}
	nsgs, err := d.clients.NSGs.ListAll(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	for _, n := range nsgs {
		add(KindSecurityGroups, deref(n.Location), deref(n.ID), deref(n.Name), n.Tags, "")
	}
	ppgs, err := d.clients.PPGs.ListAll(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	for _, g := range ppgs {
		add(KindPlacement, deref(g.Location), deref(g.ID), deref(g.Name), g.Tags, "")
	}
	slices.SortFunc(out, func(a, b vendors.ResourceRef) int {
		return strings.Compare(string(a.Kind)+a.ID, string(b.Kind)+b.ID)
	})
	return out, nil
}

// InUse implements vendors.ManagedResources: whether a live instance uses the
// resource. A subnet is used by a NIC (its IP configurations), a network
// security group by a NIC or a subnet, a proximity placement group by a VM, a
// resource group by anything that is not a leftover of SCarpenter's own (a VM, or
// a resource without the marker). When it is not in use the last use is unknown,
// so the zero time is returned and the garbage collector counts the idle time
// from its first sighting of the resource.
func (d *Driver) InUse(ctx context.Context, ref vendors.ResourceRef) (bool, time.Time, error) {
	used, err := d.usedBy(ctx, ref)
	if err != nil {
		return false, time.Time{}, err
	}
	if used {
		return true, d.clock.Now(), nil
	}
	return false, time.Time{}, nil
}

// usedBy reads the resource and says whether something uses it; NotFound when it
// does not exist.
func (d *Driver) usedBy(ctx context.Context, ref vendors.ResourceRef) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	notFound := func(err error) error {
		if isNotFound(err) {
			return vendors.NewError(vendors.NotFound, "", fmt.Errorf("the %s %s does not exist", ref.Kind, ref.ID))
		}
		return classify(err, errContext{})
	}
	switch ref.Kind {
	case KindResourceGroups:
		res, err := d.clients.Groups.Resources(cctx, ref.ID)
		if err != nil {
			return false, notFound(err)
		}
		for _, r := range res {
			if r == nil {
				continue
			}
			if strings.EqualFold(deref(r.Type), vmType) || !ownedBy(r.Tags, d.cfg.ClusterName) {
				return true, nil
			}
		}
		return false, nil
	case KindSubnets:
		rg, name, ok := vnetOf(ref.ID)
		if !ok {
			return false, vendors.Errorf(vendors.NotFound, "%q is not the ID of a subnet", ref.ID)
		}
		v, err := d.clients.VNets.Get(cctx, rg, name)
		if err != nil {
			return false, notFound(err)
		}
		if v.Properties != nil {
			for _, sn := range v.Properties.Subnets {
				if sn != nil && sn.Properties != nil && len(sn.Properties.IPConfigurations) > 0 {
					return true, nil
				}
			}
		}
		return false, nil
	case KindSecurityGroups, KindRunSecurityGroups:
		r, err := arm.ParseResourceID(ref.ID)
		if err != nil {
			return false, vendors.Errorf(vendors.NotFound, "%q is not the ID of a network security group", ref.ID)
		}
		n, err := d.clients.NSGs.Get(cctx, r.ResourceGroupName, r.Name)
		if err != nil {
			return false, notFound(err)
		}
		return n.Properties != nil && (len(n.Properties.NetworkInterfaces) > 0 || len(n.Properties.Subnets) > 0), nil
	case KindPlacement:
		r, err := arm.ParseResourceID(ref.ID)
		if err != nil {
			return false, vendors.Errorf(vendors.NotFound, "%q is not the ID of a proximity placement group", ref.ID)
		}
		g, err := d.clients.PPGs.Get(cctx, r.ResourceGroupName, r.Name)
		if err != nil {
			return false, notFound(err)
		}
		return g.Properties != nil && len(g.Properties.VirtualMachines) > 0, nil
	}
	return false, vendors.Errorf(vendors.Fatal, "the Azure driver has no resource kind %q", ref.Kind)
}

// vnetOf returns the resource group and name of the virtual network of a subnet
// ID, or of a virtual network ID.
func vnetOf(id string) (rg, vnet string, ok bool) {
	r, err := arm.ParseResourceID(id)
	if err != nil {
		return "", "", false
	}
	switch strings.ToLower(r.ResourceType.String()) {
	case "microsoft.network/virtualnetworks":
		return r.ResourceGroupName, r.Name, true
	case "microsoft.network/virtualnetworks/subnets":
		if r.Parent == nil {
			return "", "", false
		}
		return r.ResourceGroupName, r.Parent.Name, true
	}
	return "", "", false
}

// Delete implements vendors.ManagedResources: only resources that carry the
// cluster's ownership marker are deleted (never one the user brought); a
// resource that is already gone is success; one that something still uses is
// Retryable. A managed subnet is deleted with its virtual network, a network
// security group with its application security group (after it), a resource
// group only when nothing but SCarpenter's own leftovers is in it (deleting a
// group deletes everything in it).
func (d *Driver) Delete(ctx context.Context, ref vendors.ResourceRef) error {
	if !isKind(ref.Kind) {
		return vendors.Errorf(vendors.Fatal, "the Azure driver has no resource kind %q", ref.Kind)
	}
	tags, found, err := d.tagsOf(ctx, ref)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if !ownedBy(tags, d.cfg.ClusterName) {
		return vendors.NewError(vendors.Fatal, codeNotOwned, fmt.Errorf(
			"%s %s was not created by this cluster's SCarpenter and is never deleted", ref.Kind, ref.ID))
	}
	used, err := d.usedBy(ctx, ref)
	switch {
	case vendors.IsClass(err, vendors.NotFound):
		return nil
	case err != nil:
		return err
	case used:
		return vendors.NewError(vendors.Retryable, codeInUse, fmt.Errorf("the %s %s is still in use", ref.Kind, ref.ID))
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	var derr error
	switch ref.Kind {
	case KindResourceGroups:
		derr = d.clients.Groups.Delete(cctx, ref.ID)
	case KindSubnets:
		rg, name, _ := vnetOf(ref.ID)
		derr = d.clients.VNets.Delete(cctx, rg, name)
	case KindSecurityGroups, KindRunSecurityGroups:
		r, _ := arm.ParseResourceID(ref.ID)
		if derr = d.clients.NSGs.Delete(cctx, r.ResourceGroupName, r.Name); derr == nil || isNotFound(derr) {
			derr = d.deleteASG(cctx, r.ResourceGroupName, ASGName(r.Name))
		}
	case KindPlacement:
		r, _ := arm.ParseResourceID(ref.ID)
		derr = d.clients.PPGs.Delete(cctx, r.ResourceGroupName, r.Name)
	}
	if derr != nil && !isNotFound(derr) {
		return classify(derr, errContext{})
	}
	return nil
}

// deleteASG deletes the application security group of a network security group
// when it exists and carries the marker.
func (d *Driver) deleteASG(ctx context.Context, rg, name string) error {
	g, err := d.clients.ASGs.Get(ctx, rg, name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedBy(g.Tags, d.cfg.ClusterName) {
		return nil
	}
	return d.clients.ASGs.Delete(ctx, rg, name)
}

// tagsOf reads the tags of the resource of a reference; found is false when it
// does not exist (a reference that is not an ID of the kind included).
func (d *Driver) tagsOf(ctx context.Context, ref vendors.ResourceRef) (tags map[string]*string, found bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	switch ref.Kind {
	case KindResourceGroups:
		g, err := d.clients.Groups.Get(cctx, ref.ID)
		return g.Tags, err == nil, ignoreNotFound(err)
	case KindSubnets:
		rg, name, ok := vnetOf(ref.ID)
		if !ok {
			return nil, false, vendors.Errorf(vendors.Fatal, "%q is not the ID of a subnet that SCarpenter created", ref.ID)
		}
		v, err := d.clients.VNets.Get(cctx, rg, name)
		return v.Tags, err == nil, ignoreNotFound(err)
	case KindSecurityGroups, KindRunSecurityGroups:
		r, perr := arm.ParseResourceID(ref.ID)
		if perr != nil {
			return nil, false, vendors.Errorf(vendors.Fatal, "%q is not the ID of a network security group", ref.ID)
		}
		n, err := d.clients.NSGs.Get(cctx, r.ResourceGroupName, r.Name)
		return n.Tags, err == nil, ignoreNotFound(err)
	}
	r, perr := arm.ParseResourceID(ref.ID)
	if perr != nil {
		return nil, false, vendors.Errorf(vendors.Fatal, "%q is not the ID of a proximity placement group", ref.ID)
	}
	g, err := d.clients.PPGs.Get(cctx, r.ResourceGroupName, r.Name)
	return g.Tags, err == nil, ignoreNotFound(err)
}

// ignoreNotFound drops a not found error (the caller reads found) and classifies
// the others.
func ignoreNotFound(err error) error {
	if err == nil || isNotFound(err) {
		return nil
	}
	return classify(err, errContext{})
}
