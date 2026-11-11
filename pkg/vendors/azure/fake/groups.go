/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

//nolint:lll,goconst,dupl // the fake repeats the SDK's long signatures and ARM texts
package fake

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

// Groups is the resource group API.
type Groups struct{ s *Subscription }

// VNets is the virtual network API.
type VNets struct{ s *Subscription }

// NSGs is the network security group API.
type NSGs struct{ s *Subscription }

// ASGs is the application security group API.
type ASGs struct{ s *Subscription }

// PPGs is the proximity placement group API.
type PPGs struct{ s *Subscription }

// Groups, VNets, NSGs, ASGs and PPGs return the APIs.
func (s *Subscription) Groups() *Groups { return &Groups{s} }
func (s *Subscription) VNets() *VNets   { return &VNets{s} }
func (s *Subscription) NSGs() *NSGs     { return &NSGs{s} }
func (s *Subscription) ASGs() *ASGs     { return &ASGs{s} }
func (s *Subscription) PPGs() *PPGs     { return &PPGs{s} }

func copyTags(in map[string]*string) map[string]*string {
	if in == nil {
		return nil
	}
	out := make(map[string]*string, len(in))
	for k, v := range in {
		out[k] = new(val(v))
	}
	return out
}

func copyRG(g armresources.ResourceGroup) armresources.ResourceGroup {
	g.Tags = copyTags(g.Tags)
	if g.Properties != nil {
		p := *g.Properties
		g.Properties = &p
	}
	return g
}

// Get reads a group.
func (a *Groups) Get(ctx context.Context, name string) (armresources.ResourceGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "group.get"); err != nil {
		return armresources.ResourceGroup{}, err
	}
	g, ok := s.rgs[strings.ToLower(name)]
	if !ok {
		return armresources.ResourceGroup{}, s.groupExists(name)
	}
	return copyRG(*g), nil
}

// CreateOrUpdate creates a group, or updates its tags.
func (a *Groups) CreateOrUpdate(ctx context.Context, name string, rg armresources.ResourceGroup) (armresources.ResourceGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "group.put"); err != nil {
		return armresources.ResourceGroup{}, err
	}
	g := s.putGroup(name, val(rg.Location), copyTags(rg.Tags))
	return copyRG(*g), nil
}

// Delete removes the group and everything in it, like ARM.
func (a *Groups) Delete(ctx context.Context, name string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "group.delete"); err != nil {
		return err
	}
	k := strings.ToLower(name)
	if !s.groups[k] {
		return nil
	}
	prefix := k + "/"
	for _, kk := range mapKeys(s.vms) {
		if strings.HasPrefix(kk, prefix) {
			s.removeVM(kk)
		}
	}
	dropPrefix(s.nics, prefix)
	dropPrefix(s.pips, prefix)
	dropPrefix(s.vnets, prefix)
	dropPrefix(s.nsgs, prefix)
	dropPrefix(s.asgs, prefix)
	dropPrefix(s.ppgs, prefix)
	dropPrefix(s.disks, prefix)
	delete(s.groups, k)
	delete(s.rgs, k)
	return nil
}

func dropPrefix[V any](m map[string]V, prefix string) {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			delete(m, k)
		}
	}
}

// List lists every group, with its tags (the list API returns them, unlike the
// tag-filtered resource listing).
func (a *Groups) List(ctx context.Context) ([]armresources.ResourceGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "group.list"); err != nil {
		return nil, err
	}
	var out []armresources.ResourceGroup
	for _, k := range sortedKeys(s.rgs) {
		out = append(out, copyRG(*s.rgs[k]))
	}
	return out, nil
}

// Resources lists what a group holds, with tags (the disks have none: inline
// disks are not tagged).
func (a *Groups) Resources(ctx context.Context, name string) ([]*armresources.GenericResourceExpanded, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "group.resources"); err != nil {
		return nil, err
	}
	if err := s.groupExists(name); err != nil {
		return nil, err
	}
	prefix := strings.ToLower(name) + "/"
	var out []*armresources.GenericResourceExpanded
	add := func(id, n, typ, loc *string, tags map[string]*string) {
		out = append(out, &armresources.GenericResourceExpanded{ID: id, Name: n, Type: typ, Location: loc, Tags: copyTags(tags)})
	}
	for _, k := range sortedKeys(s.vms) {
		if v := s.vms[k]; strings.HasPrefix(k, prefix) {
			add(v.vm.ID, v.vm.Name, new("Microsoft.Compute/virtualMachines"), v.vm.Location, v.vm.Tags)
		}
	}
	for _, k := range sortedKeys(s.nics) {
		if n := s.nics[k]; strings.HasPrefix(k, prefix) {
			add(n.ID, n.Name, new("Microsoft.Network/networkInterfaces"), n.Location, n.Tags)
		}
	}
	for _, k := range sortedKeys(s.pips) {
		if p := s.pips[k]; strings.HasPrefix(k, prefix) {
			add(p.ID, p.Name, new("Microsoft.Network/publicIPAddresses"), p.Location, p.Tags)
		}
	}
	for _, k := range sortedKeys(s.vnets) {
		if v := s.vnets[k]; strings.HasPrefix(k, prefix) {
			add(v.ID, v.Name, new("Microsoft.Network/virtualNetworks"), v.Location, v.Tags)
		}
	}
	for _, k := range sortedKeys(s.nsgs) {
		if v := s.nsgs[k]; strings.HasPrefix(k, prefix) {
			add(v.ID, v.Name, new("Microsoft.Network/networkSecurityGroups"), v.Location, v.Tags)
		}
	}
	for _, k := range sortedKeys(s.asgs) {
		if v := s.asgs[k]; strings.HasPrefix(k, prefix) {
			add(v.ID, v.Name, new("Microsoft.Network/applicationSecurityGroups"), v.Location, v.Tags)
		}
	}
	for _, k := range sortedKeys(s.ppgs) {
		if v := s.ppgs[k]; strings.HasPrefix(k, prefix) {
			add(v.ID, v.Name, new("Microsoft.Compute/proximityPlacementGroups"), v.Location, v.Tags)
		}
	}
	for _, k := range sortedKeys(s.disks) {
		if n, ok := strings.CutPrefix(k, prefix); ok {
			add(new(s.id(name, "Microsoft.Compute", "disks", n)), new(n), new("Microsoft.Compute/disks"), nil, nil)
		}
	}
	return out, nil
}

func (s *Subscription) checkLocation(loc, typ string) error {
	if _, ok := s.locations[strings.ToLower(loc)]; !ok {
		return Error(http.StatusBadRequest, "InvalidResourceLocation",
			fmt.Sprintf("The provided location '%s' is not available for resource type '%s'.", loc, typ))
	}
	return nil
}

func sameID(a, b string) bool { return strings.EqualFold(a, b) }

// nicsUsing returns the NICs whose IP configurations satisfy f. The caller holds the lock.
func (s *Subscription) nicsUsing(f func(c *armnetwork.InterfaceIPConfiguration, n *armnetwork.Interface) bool) []*armnetwork.Interface {
	var out []*armnetwork.Interface
	for _, k := range sortedKeys(s.nics) {
		n := s.nics[k]
		if n.Properties == nil {
			continue
		}
		for _, c := range n.Properties.IPConfigurations {
			if c != nil && c.Properties != nil && f(c, n) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// --- virtual networks ---

func copyVNet(v armnetwork.VirtualNetwork) armnetwork.VirtualNetwork {
	v.Tags = copyTags(v.Tags)
	if v.Properties != nil {
		p := *v.Properties
		p.Subnets = nil
		for _, sn := range v.Properties.Subnets {
			c := *sn
			if sn.Properties != nil {
				pp := *sn.Properties
				c.Properties = &pp
			}
			p.Subnets = append(p.Subnets, &c)
		}
		v.Properties = &p
	}
	return v
}

// vnetView fills the read-only ipConfigurations of the subnets.
func (s *Subscription) vnetView(v *armnetwork.VirtualNetwork) armnetwork.VirtualNetwork {
	out := copyVNet(*v)
	if out.Properties == nil {
		return out
	}
	for _, sn := range out.Properties.Subnets {
		sn.Properties.IPConfigurations = nil
		for _, n := range s.nicsUsing(func(c *armnetwork.InterfaceIPConfiguration, _ *armnetwork.Interface) bool {
			return c.Properties.Subnet != nil && sameID(val(c.Properties.Subnet.ID), val(sn.ID))
		}) {
			sn.Properties.IPConfigurations = append(sn.Properties.IPConfigurations,
				&armnetwork.IPConfiguration{ID: new(val(n.ID) + "/ipConfigurations/ipconfig1")})
		}
	}
	return out
}

// Get reads a virtual network.
func (a *VNets) Get(ctx context.Context, rg, name string) (armnetwork.VirtualNetwork, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "vnet.get"); err != nil {
		return armnetwork.VirtualNetwork{}, err
	}
	v, ok := s.vnets[key(rg, name)]
	if !ok {
		return armnetwork.VirtualNetwork{}, notFound("ResourceNotFound", "virtual network "+rg+"/"+name)
	}
	return s.vnetView(v), nil
}

// CreateOrUpdate creates a virtual network with its subnets.
func (a *VNets) CreateOrUpdate(ctx context.Context, rg, name string, vnet armnetwork.VirtualNetwork) (armnetwork.VirtualNetwork, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "vnet.put"); err != nil {
		return armnetwork.VirtualNetwork{}, err
	}
	if err := s.groupExists(rg); err != nil {
		return armnetwork.VirtualNetwork{}, err
	}
	if err := s.checkLocation(val(vnet.Location), "Microsoft.Network/virtualNetworks"); err != nil {
		return armnetwork.VirtualNetwork{}, err
	}
	if vnet.Properties == nil || vnet.Properties.AddressSpace == nil || len(vnet.Properties.AddressSpace.AddressPrefixes) == 0 {
		return armnetwork.VirtualNetwork{}, Error(http.StatusBadRequest, "InvalidRequestFormat", "The virtual network needs an address space.")
	}
	stored := copyVNet(vnet)
	stored.ID = new(s.id(rg, "Microsoft.Network", "virtualNetworks", name))
	stored.Name = new(name)
	stored.Type = new("Microsoft.Network/virtualNetworks")
	stored.Properties.ProvisioningState = new(armnetwork.ProvisioningStateSucceeded)
	for _, sn := range stored.Properties.Subnets {
		sn.ID = new(val(stored.ID) + "/subnets/" + val(sn.Name))
		sn.Type = new("Microsoft.Network/virtualNetworks/subnets")
		if sn.Properties == nil || (val(sn.Properties.AddressPrefix) == "" && len(sn.Properties.AddressPrefixes) == 0) {
			return armnetwork.VirtualNetwork{}, Error(http.StatusBadRequest, "InvalidRequestFormat", "The subnet needs an address prefix.")
		}
		sn.Properties.ProvisioningState = new(armnetwork.ProvisioningStateSucceeded)
	}
	s.vnets[key(rg, name)] = &stored
	return s.vnetView(&stored), nil
}

// Delete removes a virtual network; one with a NIC in a subnet cannot be deleted.
func (a *VNets) Delete(ctx context.Context, rg, name string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "vnet.delete"); err != nil {
		return err
	}
	v, ok := s.vnets[key(rg, name)]
	if !ok {
		return nil
	}
	for _, sn := range v.Properties.Subnets {
		if used := s.nicsUsing(func(c *armnetwork.InterfaceIPConfiguration, _ *armnetwork.Interface) bool {
			return c.Properties.Subnet != nil && sameID(val(c.Properties.Subnet.ID), val(sn.ID))
		}); len(used) > 0 {
			return Error(http.StatusBadRequest, "InUseSubnetCannotBeDeleted",
				fmt.Sprintf("Subnet %s is in use by %s and cannot be deleted.", val(sn.Name), val(used[0].ID)))
		}
	}
	delete(s.vnets, key(rg, name))
	return nil
}

// ListAll lists the virtual networks of the subscription, with tags.
func (a *VNets) ListAll(ctx context.Context) ([]armnetwork.VirtualNetwork, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "vnet.list"); err != nil {
		return nil, err
	}
	var out []armnetwork.VirtualNetwork
	for _, k := range sortedKeys(s.vnets) {
		out = append(out, s.vnetView(s.vnets[k]))
	}
	return out, nil
}

// --- network security groups ---

func copyNSG(n armnetwork.SecurityGroup) armnetwork.SecurityGroup {
	n.Tags = copyTags(n.Tags)
	if n.Properties != nil {
		p := *n.Properties
		p.SecurityRules = append([]*armnetwork.SecurityRule(nil), n.Properties.SecurityRules...)
		n.Properties = &p
	}
	return n
}

func (s *Subscription) nsgView(n *armnetwork.SecurityGroup) armnetwork.SecurityGroup {
	out := copyNSG(*n)
	if out.Properties == nil {
		out.Properties = &armnetwork.SecurityGroupPropertiesFormat{}
	}
	out.Properties.NetworkInterfaces = nil
	for _, nic := range s.nicsUsingNSG(val(n.ID)) {
		out.Properties.NetworkInterfaces = append(out.Properties.NetworkInterfaces, &armnetwork.Interface{ID: nic.ID})
	}
	return out
}

func (s *Subscription) nicsUsingNSG(id string) []*armnetwork.Interface {
	var out []*armnetwork.Interface
	for _, k := range sortedKeys(s.nics) {
		n := s.nics[k]
		if n.Properties != nil && n.Properties.NetworkSecurityGroup != nil && sameID(val(n.Properties.NetworkSecurityGroup.ID), id) {
			out = append(out, n)
		}
	}
	return out
}

// Get reads a network security group.
func (a *NSGs) Get(ctx context.Context, rg, name string) (armnetwork.SecurityGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "nsg.get"); err != nil {
		return armnetwork.SecurityGroup{}, err
	}
	n, ok := s.nsgs[key(rg, name)]
	if !ok {
		return armnetwork.SecurityGroup{}, notFound("ResourceNotFound", "network security group "+rg+"/"+name)
	}
	return s.nsgView(n), nil
}

// CreateOrUpdate creates a network security group; the rules replace the old ones.
func (a *NSGs) CreateOrUpdate(ctx context.Context, rg, name string, nsg armnetwork.SecurityGroup) (armnetwork.SecurityGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "nsg.put"); err != nil {
		return armnetwork.SecurityGroup{}, err
	}
	if err := s.groupExists(rg); err != nil {
		return armnetwork.SecurityGroup{}, err
	}
	if err := s.checkLocation(val(nsg.Location), "Microsoft.Network/networkSecurityGroups"); err != nil {
		return armnetwork.SecurityGroup{}, err
	}
	stored := copyNSG(nsg)
	if stored.Properties == nil {
		stored.Properties = &armnetwork.SecurityGroupPropertiesFormat{}
	}
	prios := map[string]bool{}
	for _, r := range stored.Properties.SecurityRules {
		if r == nil || r.Properties == nil {
			return armnetwork.SecurityGroup{}, Error(http.StatusBadRequest, "InvalidRequestFormat", "A security rule needs properties.")
		}
		k := fmt.Sprintf("%s/%d", val(r.Properties.Direction), val(r.Properties.Priority))
		if prios[k] {
			return armnetwork.SecurityGroup{}, Error(http.StatusBadRequest, "SecurityRuleParametersPriorityNotUnique", "Two security rules have the same priority and direction.")
		}
		prios[k] = true
		r.Properties.ProvisioningState = new(armnetwork.ProvisioningStateSucceeded)
	}
	stored.ID = new(s.id(rg, "Microsoft.Network", "networkSecurityGroups", name))
	stored.Name = new(name)
	stored.Type = new("Microsoft.Network/networkSecurityGroups")
	stored.Properties.ProvisioningState = new(armnetwork.ProvisioningStateSucceeded)
	s.nsgs[key(rg, name)] = &stored
	return s.nsgView(&stored), nil
}

// Delete removes a network security group; one a NIC uses cannot be deleted.
func (a *NSGs) Delete(ctx context.Context, rg, name string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "nsg.delete"); err != nil {
		return err
	}
	n, ok := s.nsgs[key(rg, name)]
	if !ok {
		return nil
	}
	if used := s.nicsUsingNSG(val(n.ID)); len(used) > 0 {
		return Error(http.StatusBadRequest, "InUseNetworkSecurityGroupCannotBeDeleted",
			fmt.Sprintf("Network security group %s is in use by %s.", name, val(used[0].ID)))
	}
	delete(s.nsgs, key(rg, name))
	return nil
}

// ListAll lists the network security groups of the subscription.
func (a *NSGs) ListAll(ctx context.Context) ([]armnetwork.SecurityGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "nsg.list"); err != nil {
		return nil, err
	}
	var out []armnetwork.SecurityGroup
	for _, k := range sortedKeys(s.nsgs) {
		out = append(out, s.nsgView(s.nsgs[k]))
	}
	return out, nil
}

// --- application security groups ---

func copyASG(a armnetwork.ApplicationSecurityGroup) armnetwork.ApplicationSecurityGroup {
	a.Tags = copyTags(a.Tags)
	if a.Properties != nil {
		p := *a.Properties
		a.Properties = &p
	}
	return a
}

func (s *Subscription) nicsUsingASG(id string) []*armnetwork.Interface {
	return s.nicsUsing(func(c *armnetwork.InterfaceIPConfiguration, _ *armnetwork.Interface) bool {
		for _, g := range c.Properties.ApplicationSecurityGroups {
			if g != nil && sameID(val(g.ID), id) {
				return true
			}
		}
		return false
	})
}

// Get reads an application security group.
func (a *ASGs) Get(ctx context.Context, rg, name string) (armnetwork.ApplicationSecurityGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "asg.get"); err != nil {
		return armnetwork.ApplicationSecurityGroup{}, err
	}
	g, ok := s.asgs[key(rg, name)]
	if !ok {
		return armnetwork.ApplicationSecurityGroup{}, notFound("ResourceNotFound", "application security group "+rg+"/"+name)
	}
	return copyASG(*g), nil
}

// CreateOrUpdate creates an application security group.
func (a *ASGs) CreateOrUpdate(ctx context.Context, rg, name string, asg armnetwork.ApplicationSecurityGroup) (armnetwork.ApplicationSecurityGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "asg.put"); err != nil {
		return armnetwork.ApplicationSecurityGroup{}, err
	}
	if err := s.groupExists(rg); err != nil {
		return armnetwork.ApplicationSecurityGroup{}, err
	}
	if err := s.checkLocation(val(asg.Location), "Microsoft.Network/applicationSecurityGroups"); err != nil {
		return armnetwork.ApplicationSecurityGroup{}, err
	}
	stored := copyASG(asg)
	stored.ID = new(s.id(rg, "Microsoft.Network", "applicationSecurityGroups", name))
	stored.Name = new(name)
	stored.Type = new("Microsoft.Network/applicationSecurityGroups")
	stored.Properties = &armnetwork.ApplicationSecurityGroupPropertiesFormat{ProvisioningState: new(armnetwork.ProvisioningStateSucceeded)}
	s.asgs[key(rg, name)] = &stored
	return copyASG(stored), nil
}

// Delete removes an application security group; one a NIC joins cannot be deleted.
func (a *ASGs) Delete(ctx context.Context, rg, name string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "asg.delete"); err != nil {
		return err
	}
	g, ok := s.asgs[key(rg, name)]
	if !ok {
		return nil
	}
	if used := s.nicsUsingASG(val(g.ID)); len(used) > 0 {
		return Error(http.StatusBadRequest, "ApplicationSecurityGroupInUse",
			fmt.Sprintf("Application security group %s is in use by %s.", name, val(used[0].ID)))
	}
	delete(s.asgs, key(rg, name))
	return nil
}

// ListAll lists the application security groups of the subscription.
func (a *ASGs) ListAll(ctx context.Context) ([]armnetwork.ApplicationSecurityGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "asg.list"); err != nil {
		return nil, err
	}
	var out []armnetwork.ApplicationSecurityGroup
	for _, k := range sortedKeys(s.asgs) {
		out = append(out, copyASG(*s.asgs[k]))
	}
	return out, nil
}

// --- proximity placement groups ---

func copyPPG(p armcompute.ProximityPlacementGroup) armcompute.ProximityPlacementGroup {
	p.Tags = copyTags(p.Tags)
	if p.Properties != nil {
		pp := *p.Properties
		p.Properties = &pp
	}
	return p
}

func (s *Subscription) ppgView(p *armcompute.ProximityPlacementGroup) armcompute.ProximityPlacementGroup {
	out := copyPPG(*p)
	if out.Properties == nil {
		out.Properties = &armcompute.ProximityPlacementGroupProperties{}
	}
	out.Properties.VirtualMachines = nil
	for _, k := range sortedKeys(s.vms) {
		v := s.vms[k].vm
		if v.Properties.ProximityPlacementGroup != nil && sameID(val(v.Properties.ProximityPlacementGroup.ID), val(p.ID)) {
			out.Properties.VirtualMachines = append(out.Properties.VirtualMachines, &armcompute.SubResourceWithColocationStatus{ID: v.ID})
		}
	}
	return out
}

// Get reads a proximity placement group.
func (a *PPGs) Get(ctx context.Context, rg, name string) (armcompute.ProximityPlacementGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "ppg.get"); err != nil {
		return armcompute.ProximityPlacementGroup{}, err
	}
	p, ok := s.ppgs[key(rg, name)]
	if !ok {
		return armcompute.ProximityPlacementGroup{}, notFound("ResourceNotFound", "proximity placement group "+rg+"/"+name)
	}
	return s.ppgView(p), nil
}

// CreateOrUpdate creates a proximity placement group.
func (a *PPGs) CreateOrUpdate(ctx context.Context, rg, name string, ppg armcompute.ProximityPlacementGroup) (armcompute.ProximityPlacementGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "ppg.put"); err != nil {
		return armcompute.ProximityPlacementGroup{}, err
	}
	if err := s.groupExists(rg); err != nil {
		return armcompute.ProximityPlacementGroup{}, err
	}
	if err := s.checkLocation(val(ppg.Location), "Microsoft.Compute/proximityPlacementGroups"); err != nil {
		return armcompute.ProximityPlacementGroup{}, err
	}
	stored := copyPPG(ppg)
	stored.ID = new(s.id(rg, "Microsoft.Compute", "proximityPlacementGroups", name))
	stored.Name = new(name)
	stored.Type = new("Microsoft.Compute/proximityPlacementGroups")
	if stored.Properties == nil {
		stored.Properties = &armcompute.ProximityPlacementGroupProperties{}
	}
	s.ppgs[key(rg, name)] = &stored
	return s.ppgView(&stored), nil
}

// Delete removes a proximity placement group; one with VMs cannot be deleted.
func (a *PPGs) Delete(ctx context.Context, rg, name string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "ppg.delete"); err != nil {
		return err
	}
	p, ok := s.ppgs[key(rg, name)]
	if !ok {
		return nil
	}
	if v := s.ppgView(p); len(v.Properties.VirtualMachines) > 0 {
		return Error(http.StatusConflict, "ProximityPlacementGroupInUse",
			fmt.Sprintf("Proximity placement group %s has virtual machines and cannot be deleted.", name))
	}
	delete(s.ppgs, key(rg, name))
	return nil
}

// ListAll lists the proximity placement groups of the subscription.
func (a *PPGs) ListAll(ctx context.Context) ([]armcompute.ProximityPlacementGroup, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "ppg.list"); err != nil {
		return nil, err
	}
	var out []armcompute.ProximityPlacementGroup
	for _, k := range sortedKeys(s.ppgs) {
		out = append(out, s.ppgView(s.ppgs[k]))
	}
	return out, nil
}

// ListInGroup lists the NICs of a resource group (the NIC API's ListInGroup).
func (a *NICs) ListInGroup(ctx context.Context, rg string) ([]armnetwork.Interface, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "nic.list"); err != nil {
		return nil, err
	}
	var out []armnetwork.Interface
	for _, k := range sortedKeys(s.nics) {
		if strings.HasPrefix(k, strings.ToLower(rg)+"/") {
			out = append(out, copyNIC(*s.nics[k]))
		}
	}
	return out, nil
}

// Names lists the application security groups that exist (rg/name, lower case).
func (a *ASGs) Names() []string {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	return sortedKeys(a.s.asgs)
}
