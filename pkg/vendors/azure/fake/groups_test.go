/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/stretchr/testify/require"
)

func vnetBody(prefix string) armnetwork.VirtualNetwork {
	return armnetwork.VirtualNetwork{
		Location: new("westeurope"), Tags: map[string]*string{"t": new("1")},
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{new("10.0.0.0/8")}},
			Subnets:      []*armnetwork.Subnet{{Name: new("n"), Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new(prefix)}}},
		},
	}
}

func TestGroupsAndTheirContents(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	g, err := s.Groups().CreateOrUpdate(ctx, "mine", armresources.ResourceGroup{Location: new("westeurope"), Tags: map[string]*string{"a": new("b")}})
	require.NoError(t, err)
	require.Equal(t, "mine", *g.Name)
	got, err := s.Groups().Get(ctx, "MINE")
	require.NoError(t, err)
	require.Equal(t, "b", *got.Tags["a"])
	_, err = s.Groups().Get(ctx, "nope")
	code, status := codeOf(t, err)
	require.Equal(t, "ResourceGroupNotFound", code)
	require.Equal(t, http.StatusNotFound, status)
	all, err := s.Groups().List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2, "rg and mine")

	// Things in a group: listed with their tags; deleting the group deletes them.
	_, err = s.VNets().CreateOrUpdate(ctx, "mine", "v", vnetBody("10.0.0.0/24"))
	require.NoError(t, err)
	_, err = s.NSGs().CreateOrUpdate(ctx, "mine", "n", armnetwork.SecurityGroup{Location: new("westeurope")})
	require.NoError(t, err)
	_, err = s.ASGs().CreateOrUpdate(ctx, "mine", "a", armnetwork.ApplicationSecurityGroup{Location: new("westeurope")})
	require.NoError(t, err)
	_, err = s.PPGs().CreateOrUpdate(ctx, "mine", "p", armcompute.ProximityPlacementGroup{Location: new("westeurope")})
	require.NoError(t, err)
	res, err := s.Groups().Resources(ctx, "mine")
	require.NoError(t, err)
	require.Len(t, res, 4)
	_, err = s.Groups().Resources(ctx, "nope")
	require.Error(t, err)
	require.NoError(t, s.Groups().Delete(ctx, "mine"))
	require.NoError(t, s.Groups().Delete(ctx, "mine"), "a group that is gone is success")
	require.Empty(t, s.VNets().mustList(t))
	_, err = s.VNets().Get(ctx, "mine", "v")
	require.Error(t, err)
}

func (a *VNets) mustList(t *testing.T) []armnetwork.VirtualNetwork {
	t.Helper()
	l, err := a.ListAll(context.Background())
	require.NoError(t, err)
	return l
}

func TestNetworkResourceRules(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	_, err := s.VNets().CreateOrUpdate(ctx, "nope", "v", vnetBody("10.0.0.0/24"))
	require.Error(t, err, "the group must exist")
	bad := vnetBody("10.0.0.0/24")
	bad.Location = new("mars")
	_, err = s.VNets().CreateOrUpdate(ctx, rg, "v", bad)
	code, _ := codeOf(t, err)
	require.Equal(t, "InvalidResourceLocation", code)
	_, err = s.VNets().CreateOrUpdate(ctx, rg, "v", armnetwork.VirtualNetwork{Location: new("westeurope")})
	require.Error(t, err, "an address space is needed")
	noPrefix := vnetBody("")
	_, err = s.VNets().CreateOrUpdate(ctx, rg, "v", noPrefix)
	require.Error(t, err, "a subnet needs a prefix")

	v, err := s.VNets().CreateOrUpdate(ctx, rg, "vnet", vnetBody("10.0.0.0/24"))
	require.NoError(t, err)
	subnetID := *v.Properties.Subnets[0].ID
	require.Contains(t, subnetID, "/virtualNetworks/vnet/subnets/n")

	// A NIC in the subnet keeps the network from being deleted, and shows in the
	// subnet's IP configurations.
	n, err := s.NICs().CreateOrUpdate(ctx, rg, "nic", armnetwork.Interface{
		Location: new("westeurope"),
		Properties: &armnetwork.InterfacePropertiesFormat{IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
			{Name: new("ip"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{Subnet: &armnetwork.Subnet{ID: &subnetID}}}}},
	})
	require.NoError(t, err)
	got, err := s.VNets().Get(ctx, rg, "vnet")
	require.NoError(t, err)
	require.Len(t, got.Properties.Subnets[0].Properties.IPConfigurations, 1)
	err = s.VNets().Delete(ctx, rg, "vnet")
	code, _ = codeOf(t, err)
	require.Equal(t, "InUseSubnetCannotBeDeleted", code)

	// A network security group and an application security group a NIC uses.
	_, err = s.NSGs().CreateOrUpdate(ctx, rg, "nsg", armnetwork.SecurityGroup{Location: new("westeurope"), Properties: &armnetwork.SecurityGroupPropertiesFormat{
		SecurityRules: []*armnetwork.SecurityRule{
			{Name: new("a"), Properties: &armnetwork.SecurityRulePropertiesFormat{Direction: new(armnetwork.SecurityRuleDirectionInbound), Priority: new(int32(100))}},
			{Name: new("b"), Properties: &armnetwork.SecurityRulePropertiesFormat{Direction: new(armnetwork.SecurityRuleDirectionInbound), Priority: new(int32(100))}},
		}}})
	code, _ = codeOf(t, err)
	require.Equal(t, "SecurityRuleParametersPriorityNotUnique", code)
	_, err = s.NSGs().CreateOrUpdate(ctx, rg, "nsg", armnetwork.SecurityGroup{Location: new("westeurope"), Properties: &armnetwork.SecurityGroupPropertiesFormat{
		SecurityRules: []*armnetwork.SecurityRule{nil}}})
	require.Error(t, err)
	nsg, err := s.NSGs().CreateOrUpdate(ctx, rg, "nsg", armnetwork.SecurityGroup{Location: new("westeurope")})
	require.NoError(t, err)
	asg, err := s.ASGs().CreateOrUpdate(ctx, rg, "asg", armnetwork.ApplicationSecurityGroup{Location: new("westeurope")})
	require.NoError(t, err)
	n.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: nsg.ID}
	n.Properties.IPConfigurations[0].Properties.ApplicationSecurityGroups = []*armnetwork.ApplicationSecurityGroup{{ID: asg.ID}}
	_, err = s.NICs().CreateOrUpdate(ctx, rg, "nic", n)
	require.NoError(t, err)
	gn, err := s.NSGs().Get(ctx, rg, "nsg")
	require.NoError(t, err)
	require.Len(t, gn.Properties.NetworkInterfaces, 1)
	code, _ = codeOf(t, s.NSGs().Delete(ctx, rg, "nsg"))
	require.Equal(t, "InUseNetworkSecurityGroupCannotBeDeleted", code)
	code, _ = codeOf(t, s.ASGs().Delete(ctx, rg, "asg"))
	require.Equal(t, "ApplicationSecurityGroupInUse", code)
	nics, err := s.NICs().ListInGroup(ctx, rg)
	require.NoError(t, err)
	require.Len(t, nics, 1)
	require.NoError(t, s.NICs().Delete(ctx, rg, "nic"))
	for _, err := range []error{s.NSGs().Delete(ctx, rg, "nsg"), s.ASGs().Delete(ctx, rg, "asg"), s.VNets().Delete(ctx, rg, "vnet"),
		s.NSGs().Delete(ctx, rg, "nsg"), s.ASGs().Delete(ctx, rg, "asg"), s.VNets().Delete(ctx, rg, "vnet")} {
		require.NoError(t, err)
	}
	for _, l := range [][]error{
		{get(s.NSGs().Get(ctx, rg, "nsg"))}, {get(s.ASGs().Get(ctx, rg, "asg"))}, {get(s.PPGs().Get(ctx, rg, "ppg"))},
	} {
		require.Error(t, l[0])
	}
	_, err = s.NSGs().ListAll(ctx)
	require.NoError(t, err)
	_, err = s.ASGs().ListAll(ctx)
	require.NoError(t, err)
	_, err = s.PPGs().ListAll(ctx)
	require.NoError(t, err)
}

func get[T any](_ T, err error) error { return err }

func TestProximityPlacementGroupRules(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	p, err := s.PPGs().CreateOrUpdate(ctx, rg, "ppg", armcompute.ProximityPlacementGroup{Location: new("westeurope")})
	require.NoError(t, err)
	vm := vmBody(nic(t, s, "n1"), "Standard_D2s_v5", "1")
	vm.Properties.ProximityPlacementGroup = &armcompute.SubResource{ID: p.ID}
	_, err = s.VMs().CreateOrUpdate(ctx, rg, "vm", vm)
	require.NoError(t, err)
	got, err := s.PPGs().Get(ctx, rg, "ppg")
	require.NoError(t, err)
	require.Len(t, got.Properties.VirtualMachines, 1)
	code, _ := codeOf(t, s.PPGs().Delete(ctx, rg, "ppg"))
	require.Equal(t, "ProximityPlacementGroupInUse", code)
	require.NoError(t, s.VMs().Delete(ctx, rg, "vm", true))
	require.NoError(t, s.PPGs().Delete(ctx, rg, "ppg"))
	_, err = s.PPGs().CreateOrUpdate(ctx, rg, "x", armcompute.ProximityPlacementGroup{Location: new("mars")})
	require.Error(t, err)
	_, err = s.ASGs().CreateOrUpdate(ctx, rg, "x", armnetwork.ApplicationSecurityGroup{Location: new("mars")})
	require.Error(t, err)
	_, err = s.NSGs().CreateOrUpdate(ctx, rg, "x", armnetwork.SecurityGroup{Location: new("mars")})
	require.Error(t, err)
}

func TestImageVersionsAndMisc(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	s.AddImage("p", "o", "sku", "2.0.0", nil)
	s.SetImageArch("p", "o", "sku", "2.0.0", "Arm64")
	s.RemoveImageFrom("westeurope", "p", "o", "sku", "1.0.0")
	v, err := s.Images().ListVersions(ctx, "westeurope", "p", "o", "sku")
	require.NoError(t, err)
	require.Equal(t, []string{"2.0.0"}, v)
	_, err = s.Images().ListVersions(ctx, "westeurope", "p", "o", "other")
	require.Error(t, err)
	img, err := s.Images().Get(ctx, "westeurope", "p", "o", "sku", "2.0.0")
	require.NoError(t, err)
	require.Equal(t, "Arm64", string(*img.Properties.Architecture))
	s.AddLogicalLocation("stg")
	locs, err := s.Locations().List(ctx)
	require.NoError(t, err)
	require.Len(t, locs, 2)
	require.Len(t, locs[1].AvailabilityZoneMappings, 3, "a zonal location lists its logical zones")
}
