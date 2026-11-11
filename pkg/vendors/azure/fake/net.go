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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
)

// NICs is the network interface API.
type NICs struct{ s *Subscription }

// PIPs is the public IP address API.
type PIPs struct{ s *Subscription }

func copyNIC(n armnetwork.Interface) armnetwork.Interface {
	out := n
	if n.Properties != nil {
		p := *n.Properties
		p.IPConfigurations = nil
		for _, c := range n.Properties.IPConfigurations {
			cc := *c
			if c.Properties != nil {
				pp := *c.Properties
				cc.Properties = &pp
			}
			p.IPConfigurations = append(p.IPConfigurations, &cc)
		}
		out.Properties = &p
	}
	return out
}

func copyPIP(n armnetwork.PublicIPAddress) armnetwork.PublicIPAddress {
	out := n
	if n.Properties != nil {
		p := *n.Properties
		out.Properties = &p
	}
	return out
}

// Get reads a NIC.
func (a *NICs) Get(ctx context.Context, rg, name string) (armnetwork.Interface, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "nic.get"); err != nil {
		return armnetwork.Interface{}, err
	}
	n, ok := s.nics[key(rg, name)]
	if !ok {
		return armnetwork.Interface{}, notFound("ResourceNotFound", fmt.Sprintf("network interface %s/%s", rg, name))
	}
	out := copyNIC(*n)
	// Like ARM, the NIC names the VM that uses it.
	for _, v := range s.vms {
		for _, ref := range v.vm.Properties.NetworkProfile.NetworkInterfaces {
			if strings.EqualFold(val(ref.ID), val(n.ID)) && out.Properties != nil {
				out.Properties.VirtualMachine = &armnetwork.SubResource{ID: v.vm.ID}
			}
		}
	}
	return out, nil
}

// CreateOrUpdate creates a NIC (idempotent by name); the subnet must be given,
// a referenced public IP must exist, and the NIC gets a private address.
func (a *NICs) CreateOrUpdate(ctx context.Context, rg, name string, nic armnetwork.Interface) (armnetwork.Interface, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "nic.put"); err != nil {
		return armnetwork.Interface{}, err
	}
	if err := s.groupExists(rg); err != nil {
		return armnetwork.Interface{}, err
	}
	if nic.Properties == nil || len(nic.Properties.IPConfigurations) == 0 {
		return armnetwork.Interface{}, Error(http.StatusBadRequest, "InvalidRequestFormat", "The NIC needs an IP configuration.")
	}
	k := key(rg, name)
	stored := copyNIC(nic)
	if old, ok := s.nics[k]; ok && old.Properties != nil && len(old.Properties.IPConfigurations) > 0 {
		// An update keeps the private address.
		stored.Properties.IPConfigurations[0].Properties.PrivateIPAddress = old.Properties.IPConfigurations[0].Properties.PrivateIPAddress
	}
	for _, c := range stored.Properties.IPConfigurations {
		if c.Properties == nil || c.Properties.Subnet == nil || val(c.Properties.Subnet.ID) == "" {
			return armnetwork.Interface{}, Error(http.StatusBadRequest, "InvalidRequestFormat", "The IP configuration needs a subnet.")
		}
		if p := c.Properties.PublicIPAddress; p != nil {
			id, err := arm.ParseResourceID(val(p.ID))
			if err != nil {
				return armnetwork.Interface{}, Error(http.StatusBadRequest, "InvalidResourceReference", "Invalid public IP reference.")
			}
			if _, ok := s.pips[key(id.ResourceGroupName, id.Name)]; !ok {
				return armnetwork.Interface{}, notFound("ResourceNotFound", "public IP address "+id.Name)
			}
		}
		if val(c.Properties.PrivateIPAddress) == "" {
			c.Properties.PrivateIPAddress = new(fmt.Sprintf("10.240.%d.%d", s.nextIP/250, s.nextIP%250+4))
			s.nextIP++
		}
	}
	stored.ID = new(s.id(rg, "Microsoft.Network", "networkInterfaces", name))
	stored.Name = new(name)
	stored.Type = new("Microsoft.Network/networkInterfaces")
	stored.Properties.ProvisioningState = new(armnetwork.ProvisioningStateSucceeded)
	s.nics[k] = &stored
	return copyNIC(stored), nil
}

// Delete removes a NIC; one attached to a VM cannot be deleted.
func (a *NICs) Delete(ctx context.Context, rg, name string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "nic.delete"); err != nil {
		return err
	}
	n, ok := s.nics[key(rg, name)]
	if !ok {
		return nil
	}
	for _, v := range s.vms {
		for _, ref := range v.vm.Properties.NetworkProfile.NetworkInterfaces {
			if strings.EqualFold(val(ref.ID), val(n.ID)) {
				return Error(http.StatusBadRequest, "NicInUse", fmt.Sprintf("Network interface %s is used by VM %s.", name, val(v.vm.Name)))
			}
		}
	}
	delete(s.nics, key(rg, name))
	return nil
}

// Get reads a public IP.
func (a *PIPs) Get(ctx context.Context, rg, name string) (armnetwork.PublicIPAddress, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "pip.get"); err != nil {
		return armnetwork.PublicIPAddress{}, err
	}
	p, ok := s.pips[key(rg, name)]
	if !ok {
		return armnetwork.PublicIPAddress{}, notFound("ResourceNotFound", fmt.Sprintf("public IP address %s/%s", rg, name))
	}
	out := copyPIP(*p)
	// Like ARM, the address names the IP configuration that uses it.
	for _, n := range s.nics {
		for _, c := range n.Properties.IPConfigurations {
			if c.Properties.PublicIPAddress != nil && strings.EqualFold(val(c.Properties.PublicIPAddress.ID), val(p.ID)) {
				out.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: new(val(n.ID) + "/ipConfigurations/" + val(c.Name))}
			}
		}
	}
	return out, nil
}

// CreateOrUpdate creates a public IP (idempotent by name). A Standard static
// address is known right after the PUT. The limit of the location's
// StandardPublicIPAddresses usage applies.
func (a *PIPs) CreateOrUpdate(ctx context.Context, rg, name string, pip armnetwork.PublicIPAddress) (armnetwork.PublicIPAddress, error) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "pip.put"); err != nil {
		return armnetwork.PublicIPAddress{}, err
	}
	if err := s.groupExists(rg); err != nil {
		return armnetwork.PublicIPAddress{}, err
	}
	k := key(rg, name)
	stored := copyPIP(pip)
	if stored.Properties == nil {
		stored.Properties = &armnetwork.PublicIPAddressPropertiesFormat{}
	}
	if old, ok := s.pips[k]; ok {
		// The zones of a public IP cannot change (the exact ARM code is
		// unverified).
		if !sameZones(old.Zones, pip.Zones) {
			return armnetwork.PublicIPAddress{}, Error(http.StatusBadRequest, "PropertyChangeNotAllowed",
				fmt.Sprintf("Changing property 'zones' of public IP address %s is not allowed.", name))
		}
		stored.Properties.IPAddress = old.Properties.IPAddress
	} else {
		if l, ok := s.locations[strings.ToLower(val(pip.Location))]; ok {
			if limit, ok := l.limits["StandardPublicIPAddresses"]; ok && s.pipCount(l.name)+l.foreign["StandardPublicIPAddresses"] >= limit {
				return armnetwork.PublicIPAddress{}, Error(http.StatusBadRequest, "PublicIPCountLimitReached",
					fmt.Sprintf("Cannot create more than %d public IP addresses for this subscription in this region.", limit))
			}
		}
		stored.Properties.IPAddress = new(fmt.Sprintf("20.%d.%d.%d", s.nextPub/65025%250, s.nextPub/250%250, s.nextPub%250+1))
		s.nextPub++
	}
	stored.ID = new(s.id(rg, "Microsoft.Network", "publicIPAddresses", name))
	stored.Name = new(name)
	stored.Type = new("Microsoft.Network/publicIPAddresses")
	stored.Properties.ProvisioningState = new(armnetwork.ProvisioningStateSucceeded)
	s.pips[k] = &stored
	return copyPIP(stored), nil
}

func sameZones(a, b []*string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if val(a[i]) != val(b[i]) {
			return false
		}
	}
	return true
}

func (s *Subscription) pipCount(loc string) int64 {
	var n int64
	for _, p := range s.pips {
		if strings.EqualFold(val(p.Location), loc) {
			n++
		}
	}
	return n
}

// Delete removes a public IP; one attached to a NIC cannot be deleted.
func (a *PIPs) Delete(ctx context.Context, rg, name string) error {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.begin(ctx, "pip.delete"); err != nil {
		return err
	}
	p, ok := s.pips[key(rg, name)]
	if !ok {
		return nil
	}
	for _, n := range s.nics {
		for _, c := range n.Properties.IPConfigurations {
			if c.Properties.PublicIPAddress != nil && strings.EqualFold(val(c.Properties.PublicIPAddress.ID), val(p.ID)) {
				return Error(http.StatusBadRequest, "PublicIPAddressCannotBeDeleted",
					fmt.Sprintf("Public IP address %s is in use by network interface %s.", name, val(n.Name)))
			}
		}
	}
	delete(s.pips, key(rg, name))
	return nil
}
