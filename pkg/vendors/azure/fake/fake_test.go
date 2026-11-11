/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/require"
)

func codeOf(t *testing.T, err error) (string, int) {
	t.Helper()
	var re *azcore.ResponseError
	require.True(t, errors.As(err, &re), "%v", err)
	return re.ErrorCode, re.StatusCode
}

const (
	rg     = "rg"
	subnet = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/subnets/n"
)

func newSub(t *testing.T) *Subscription {
	t.Helper()
	s := New("s")
	s.AddLocation("westeurope", true)
	require.NoError(t, s.AddSKU("westeurope", SKU{Name: "Standard_D2s_v5", Family: "fam", VCPUs: 2}))
	require.NoError(t, s.AddSKU("westeurope", SKU{Name: "Standard_B2s", Family: "b", VCPUs: 2, NoSpot: true}))
	s.AddResourceGroup(rg)
	s.AddImage("p", "o", "sku", "1.0.0", nil)
	return s
}

func nic(t *testing.T, s *Subscription, name string) string {
	t.Helper()
	n, err := s.NICs().CreateOrUpdate(context.Background(), rg, name, armnetwork.Interface{
		Location: new("westeurope"),
		Properties: &armnetwork.InterfacePropertiesFormat{IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
			{Name: new("ip"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{Subnet: &armnetwork.Subnet{ID: new(subnet)}}}}},
	})
	require.NoError(t, err)
	return *n.ID
}

func vmBody(nicID, size, zone string) armcompute.VirtualMachine {
	vm := armcompute.VirtualMachine{
		Location: new("westeurope"), Tags: map[string]*string{"k": new("v")},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: new(armcompute.VirtualMachineSizeTypes(size))},
			OSProfile:       &armcompute.OSProfile{CustomData: new(base64.StdEncoding.EncodeToString([]byte("x")))},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{Publisher: new("p"), Offer: new("o"), SKU: new("sku"), Version: new("1.0.0")},
				OSDisk:         &armcompute.OSDisk{Name: new("os"), CreateOption: new(armcompute.DiskCreateOptionTypesFromImage)},
			},
			NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{ID: &nicID,
				Properties: &armcompute.NetworkInterfaceReferenceProperties{DeleteOption: new(armcompute.DeleteOptionsDelete)}}}},
		},
	}
	if zone != "" {
		vm.Zones = []*string{&zone}
	}
	return vm
}

func TestVMLifecycleAndIdempotency(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	id := nic(t, s, "n1")
	vm, err := s.VMs().CreateOrUpdate(ctx, rg, "vm", vmBody(id, "Standard_D2s_v5", "1"))
	require.NoError(t, err)
	require.Equal(t, "Succeeded", *vm.Properties.ProvisioningState)
	require.Nil(t, vm.Properties.InstanceView, "the instance view only comes with $expand")

	got, err := s.VMs().Get(ctx, rg, "vm", true)
	require.NoError(t, err)
	require.Len(t, got.Properties.InstanceView.Statuses, 2)
	require.Equal(t, "PowerState/running", *got.Properties.InstanceView.Statuses[1].Code)

	// PUT of the same name is idempotent; another zone is refused.
	again, err := s.VMs().CreateOrUpdate(ctx, rg, "vm", vmBody(id, "Standard_D2s_v5", "1"))
	require.NoError(t, err)
	require.Equal(t, *vm.ID, *again.ID)
	_, err = s.VMs().CreateOrUpdate(ctx, rg, "vm", vmBody(id, "Standard_D2s_v5", "2"))
	code, status := codeOf(t, err)
	require.Equal(t, "PropertyChangeNotAllowed", code)
	require.Equal(t, http.StatusBadRequest, status)
	require.Len(t, s.VMNames(), 1)

	// The NIC cannot be deleted while a VM uses it; deleting a gone VM succeeds.
	code, _ = codeOf(t, s.NICs().Delete(ctx, rg, "n1"))
	require.Equal(t, "NicInUse", code)
	require.NoError(t, s.VMs().Delete(ctx, rg, "vm", true))
	require.NoError(t, s.VMs().Delete(ctx, rg, "vm", true))
	require.Empty(t, s.NICNames(), "the delete option cascades to the NIC")
	_, err = s.VMs().Get(ctx, rg, "vm", false)
	code, status = codeOf(t, err)
	require.Equal(t, "ResourceNotFound", code)
	require.Equal(t, http.StatusNotFound, status)
}

func TestVMValidation(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	id := nic(t, s, "n1")
	put := func(vm armcompute.VirtualMachine) string {
		_, err := s.VMs().CreateOrUpdate(ctx, rg, "vm", vm)
		if err == nil {
			return ""
		}
		code, _ := codeOf(t, err)
		return code
	}
	require.Equal(t, "InvalidParameter", put(vmBody(id, "Standard_Nope", "1")), "unknown size")
	require.Equal(t, "SkuNotAvailable", put(vmBody(id, "Standard_D2s_v5", "4")), "zone the size does not have")
	spot := vmBody(id, "Standard_B2s", "1")
	spot.Properties.Priority = new(armcompute.VirtualMachinePriorityTypesSpot)
	require.Equal(t, "AzureSpotIsNotSupportedForThisVMSize", put(spot))
	big := vmBody(id, "Standard_D2s_v5", "1")
	big.Properties.OSProfile.CustomData = new(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 65536))))
	require.Equal(t, "InvalidParameter", put(big), "custom data over 65535 bytes")
	noImage := vmBody(id, "Standard_D2s_v5", "1")
	noImage.Properties.StorageProfile.ImageReference.Version = new("9.9.9")
	require.Equal(t, "PlatformImageNotFound", put(noImage))
	require.Equal(t, "ResourceNotFound", put(vmBody("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/none", "Standard_D2s_v5", "1")), "missing NIC")
	loc := vmBody(id, "Standard_D2s_v5", "1")
	loc.Location = new("mars")
	require.Equal(t, "InvalidResourceLocation", put(loc))
	_, err := s.VMs().CreateOrUpdate(ctx, "other-rg", "vm", vmBody(id, "Standard_D2s_v5", "1"))
	code, _ := codeOf(t, err)
	require.Equal(t, "ResourceGroupNotFound", code)
	require.Equal(t, "", put(vmBody(id, "Standard_D2s_v5", "1")))
}

func TestQuotaUsageAndFailureInjection(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	s.SetLimit("westeurope", "cores", 3)
	id := nic(t, s, "n1")
	_, err := s.VMs().CreateOrUpdate(ctx, rg, "a", vmBody(id, "Standard_D2s_v5", "1"))
	require.NoError(t, err)
	usages, err := s.Usages().ComputeUsages(ctx, "westeurope")
	require.NoError(t, err)
	byName := map[string]*armcompute.Usage{}
	for _, u := range usages {
		byName[*u.Name.Value] = u
	}
	require.EqualValues(t, 2, *byName["cores"].CurrentValue)
	require.EqualValues(t, 3, *byName["cores"].Limit)
	require.EqualValues(t, 2, *byName["fam"].CurrentValue)
	_, err = s.VMs().CreateOrUpdate(ctx, rg, "b", vmBody(id, "Standard_D2s_v5", "1"))
	code, _ := codeOf(t, err)
	require.Equal(t, "OperationNotAllowed", code)
	require.Contains(t, err.Error(), "exceeding approved Total Regional Cores quota")

	// Injected failures are consumed in order and counted.
	s.FailNext("vm.get", Error(429, "TooManyRequests", "x"), 2)
	for range 2 {
		_, err = s.VMs().Get(ctx, rg, "a", false)
		code, _ = codeOf(t, err)
		require.Equal(t, "TooManyRequests", code)
	}
	_, err = s.VMs().Get(ctx, rg, "a", false)
	require.NoError(t, err)
	require.Equal(t, 3, s.Calls("vm.get"))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.VMs().Get(canceled, rg, "a", false)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPublicIPLimitAndInUse(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	s.SetLimit("westeurope", "StandardPublicIPAddresses", 1)
	pip, err := s.PIPs().CreateOrUpdate(ctx, rg, "p1", armnetwork.PublicIPAddress{Location: new("westeurope")})
	require.NoError(t, err)
	require.NotEmpty(t, *pip.Properties.IPAddress)
	_, err = s.PIPs().CreateOrUpdate(ctx, rg, "p2", armnetwork.PublicIPAddress{Location: new("westeurope")})
	code, _ := codeOf(t, err)
	require.Equal(t, "PublicIPCountLimitReached", code)
	// An update keeps the address and is not counted twice.
	again, err := s.PIPs().CreateOrUpdate(ctx, rg, "p1", armnetwork.PublicIPAddress{Location: new("westeurope")})
	require.NoError(t, err)
	require.Equal(t, *pip.Properties.IPAddress, *again.Properties.IPAddress)
	// The zones of an address cannot change.
	_, err = s.PIPs().CreateOrUpdate(ctx, rg, "p1", armnetwork.PublicIPAddress{Location: new("westeurope"), Zones: []*string{new("2")}})
	code, _ = codeOf(t, err)
	require.Equal(t, "PropertyChangeNotAllowed", code)

	_, err = s.NICs().CreateOrUpdate(ctx, rg, "n", armnetwork.Interface{Location: new("westeurope"), Properties: &armnetwork.InterfacePropertiesFormat{
		IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{Name: new("ip"), Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
			Subnet: &armnetwork.Subnet{ID: new(subnet)}, PublicIPAddress: &armnetwork.PublicIPAddress{ID: pip.ID}}}}}})
	require.NoError(t, err)
	code, _ = codeOf(t, s.PIPs().Delete(ctx, rg, "p1"))
	require.Equal(t, "PublicIPAddressCannotBeDeleted", code)
	require.NoError(t, s.NICs().Delete(ctx, rg, "n"))
	require.NoError(t, s.PIPs().Delete(ctx, rg, "p1"))
	require.NoError(t, s.PIPs().Delete(ctx, rg, "p1"))
}

func TestResourcesTagListingReturnsNoTags(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	id := nic(t, s, "n1")
	_, err := s.VMs().CreateOrUpdate(ctx, rg, "vm", vmBody(id, "Standard_D2s_v5", "1"))
	require.NoError(t, err)
	got, err := s.Resources().ListByTag(ctx, "K", "v")
	require.NoError(t, err)
	require.Len(t, got, 1, "the tag name matches case-insensitively")
	require.Equal(t, "Microsoft.Compute/virtualMachines", *got[0].Type)
	require.Nil(t, got[0].Tags, "a tag filter returns the resources without their tags")
	got, err = s.Resources().ListByTag(ctx, "k", "other")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestImagesAgreementsLocationsAndSKUs(t *testing.T) {
	ctx := context.Background()
	s := newSub(t)
	s.AddImage("p", "o", "other", "2.0.0", &armcompute.PurchasePlan{Name: new("other"), Product: new("o"), Publisher: new("p")})
	img, err := s.Images().Get(ctx, "westeurope", "P", "O", "SKU", "1.0.0")
	require.NoError(t, err)
	require.Nil(t, img.Properties.Plan)
	img, err = s.Images().Get(ctx, "westeurope", "p", "o", "other", "2.0.0")
	require.NoError(t, err)
	require.Equal(t, "other", *img.Properties.Plan.Name)
	_, err = s.Images().Get(ctx, "westeurope", "p", "o", "sku", "9")
	code, status := codeOf(t, err)
	require.Equal(t, "PlatformImageNotFound", code)
	require.Equal(t, http.StatusNotFound, status)
	skus, err := s.Images().ListSKUs(ctx, "westeurope", "p", "o")
	require.NoError(t, err)
	require.Equal(t, []string{"other", "sku"}, skus)
	_, err = s.Images().ListSKUs(ctx, "westeurope", "p", "none")
	require.Error(t, err)

	ok, err := s.Agreements().Accepted(ctx, "p", "o", "other")
	require.NoError(t, err)
	require.False(t, ok)
	s.AcceptTerms("p", "o", "other", true)
	ok, _ = s.Agreements().Accepted(ctx, "p", "o", "other")
	require.True(t, ok)

	locs, err := s.Locations().List(ctx)
	require.NoError(t, err)
	require.Len(t, locs, 1)
	rs, err := s.SKUs().List(ctx, "westeurope")
	require.NoError(t, err)
	require.Len(t, rs, 2)
	_, err = s.SKUs().List(ctx, "mars")
	code, _ = codeOf(t, err)
	require.Equal(t, "InvalidResourceLocation", code)
}
