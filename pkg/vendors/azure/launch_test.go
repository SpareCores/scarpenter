/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestVMBody(t *testing.T) {
	e := newEnv(t)
	l := e.spec(t, request{nodeClaim: "nc-1", extraTags: map[string]string{"team": "x"}, raw: `{
		"storage":{"root":{"storageAccountType":"StandardSSD_LRS","diskSizeGB":128,"caching":"ReadOnly","diskEncryptionSetID":"/des/1"},
		"data":[{"name":"data","storageAccountType":"Premium_LRS","diskSizeGB":256,"filesystem":"xfs","mountPath":"/data"}]},
		"encryptionAtHost":true}`})
	name := VMName(testCluster, "nc-1")
	vm := l.VM("/nic/id")

	require.Equal(t, "westeurope", *vm.Location)
	require.Equal(t, []string{"1"}, []string{*vm.Zones[0]})
	require.Nil(t, vm.Plan, "no plan without a purchase plan")
	require.Equal(t, map[string]*string{
		"scarpenter-cluster": new(testCluster), "scarpenter-nodeclaim": new("nc-1"), "scarpenter-nodepool": new("pool"),
		"scarpenter-nodeclass": new("class"), "scarpenter-managed-by": new("scarpenter"), "Name": new("nc-1"), "team": new("x"),
	}, vm.Tags)
	p := vm.Properties
	require.Equal(t, "Standard_D2s_v5", string(*p.HardwareProfile.VMSize))

	os := p.OSProfile
	require.Equal(t, name, *os.ComputerName)
	require.Equal(t, "core", *os.AdminUsername)
	require.False(t, *os.AllowExtensionOperations)
	raw, err := base64.StdEncoding.DecodeString(*os.CustomData)
	require.NoError(t, err)
	require.JSONEq(t, `{"ignition":{"version":"3.4.0"}}`, string(raw))
	require.True(t, *os.LinuxConfiguration.DisablePasswordAuthentication)
	require.Len(t, os.LinuxConfiguration.SSH.PublicKeys, 1)
	require.Equal(t, "/home/core/.ssh/authorized_keys", *os.LinuxConfiguration.SSH.PublicKeys[0].Path)
	require.Equal(t, "ssh-ed25519 AAAA test", *os.LinuxConfiguration.SSH.PublicKeys[0].KeyData)

	ref := p.StorageProfile.ImageReference
	require.Equal(t, []string{"kinvolk", OfferAMD64, "stable-gen2", "4547.1.0"}, []string{*ref.Publisher, *ref.Offer, *ref.SKU, *ref.Version})
	od := p.StorageProfile.OSDisk
	require.Equal(t, name+"-os", *od.Name)
	require.Equal(t, armcompute.DiskCreateOptionTypesFromImage, *od.CreateOption)
	require.EqualValues(t, 128, *od.DiskSizeGB)
	require.Equal(t, armcompute.CachingTypesReadOnly, *od.Caching)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *od.DeleteOption)
	require.Equal(t, armcompute.StorageAccountTypesStandardSSDLRS, *od.ManagedDisk.StorageAccountType)
	require.Equal(t, "/des/1", *od.ManagedDisk.DiskEncryptionSet.ID)
	require.Nil(t, od.DiffDiskSettings)

	require.Len(t, p.StorageProfile.DataDisks, 1)
	dd := p.StorageProfile.DataDisks[0]
	require.EqualValues(t, 0, *dd.Lun)
	require.Equal(t, name+"-data", *dd.Name)
	require.Equal(t, armcompute.DiskCreateOptionTypesEmpty, *dd.CreateOption)
	require.EqualValues(t, 256, *dd.DiskSizeGB)
	require.Equal(t, armcompute.CachingTypesNone, *dd.Caching)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *dd.DeleteOption)
	require.Equal(t, armcompute.StorageAccountTypesPremiumLRS, *dd.ManagedDisk.StorageAccountType)

	nics := p.NetworkProfile.NetworkInterfaces
	require.Len(t, nics, 1)
	require.Equal(t, "/nic/id", *nics[0].ID)
	require.True(t, *nics[0].Properties.Primary)
	require.Equal(t, armcompute.DeleteOptionsDelete, *nics[0].Properties.DeleteOption)

	require.True(t, *p.DiagnosticsProfile.BootDiagnostics.Enabled)
	require.True(t, *p.SecurityProfile.EncryptionAtHost)
	require.Nil(t, p.Priority)
	require.Nil(t, p.EvictionPolicy)
	require.Nil(t, p.BillingProfile)
	require.Nil(t, p.ProximityPlacementGroup)
	require.Nil(t, p.AdditionalCapabilities)
}

func TestVMBodySpotPlanPlacementEphemeral(t *testing.T) {
	e := newEnv(t)
	req, img := request{nodeClaim: "nc-2", size: "Standard_D2ps_v6", capacity: vendors.CapacitySpot, zone: "0",
		raw: `{"storage":{"root":{"storageAccountType":"Premium_LRS","ephemeral":{"placement":"CacheDisk"}}}}`}.build(t)
	req.Prepared.Resources = append(req.Prepared.Resources,
		vendors.ResourceRef{Kind: KindPlacement, ID: "/ppg/1"}, vendors.ResourceRef{Kind: KindAppSecGroups, ID: "/asg/1"})
	skus := skuTable(t, e, testRegion)
	sku := skus["Standard_D2ps_v6"]
	l, err := NewLaunchSpec(req, testCluster, img, &ImagePlan{Name: "stable", Product: "offer", Publisher: "kinvolk"}, &sku, "key")
	require.NoError(t, err)
	vm := l.VM("/nic")
	require.Nil(t, vm.Zones, "zone 0 is a region without zones: no zones field")
	require.Equal(t, armcompute.VirtualMachinePriorityTypesSpot, *vm.Properties.Priority)
	require.Equal(t, armcompute.VirtualMachineEvictionPolicyTypesDelete, *vm.Properties.EvictionPolicy)
	require.InDelta(t, -1, *vm.Properties.BillingProfile.MaxPrice, 0)
	require.Equal(t, "/ppg/1", *vm.Properties.ProximityPlacementGroup.ID)
	require.Equal(t, armcompute.Plan{Name: new("stable"), Product: new("offer"), Publisher: new("kinvolk")}, *vm.Plan)
	od := vm.Properties.StorageProfile.OSDisk
	require.Equal(t, armcompute.DiffDiskOptionsLocal, *od.DiffDiskSettings.Option)
	require.Equal(t, armcompute.DiffDiskPlacementCacheDisk, *od.DiffDiskSettings.Placement)
	require.Equal(t, armcompute.CachingTypesReadOnly, *od.Caching, "ephemeral disks are read-only cached")
	require.Nil(t, od.ManagedDisk.StorageAccountType)
}

func TestNICAndPIPBodies(t *testing.T) {
	e := newEnv(t)
	req, img := request{nodeClaim: "nc-3"}.build(t)
	req.Prepared.Resources = append(req.Prepared.Resources, vendors.ResourceRef{Kind: KindAppSecGroups, ID: "/asg/1"})
	skus := skuTable(t, e, testRegion)
	sku := skus["Standard_D2s_v5"]
	l, err := NewLaunchSpec(req, testCluster, img, nil, &sku, "key")
	require.NoError(t, err)

	pip := l.PIP()
	require.Equal(t, "Standard", string(*pip.SKU.Name))
	require.Equal(t, "Static", string(*pip.Properties.PublicIPAllocationMethod))
	require.Equal(t, "IPv4", string(*pip.Properties.PublicIPAddressVersion))
	require.Equal(t, "Delete", string(*pip.Properties.DeleteOption))
	require.Equal(t, []string{"1"}, []string{*pip.Zones[0]})
	require.Equal(t, l.VMName()+"-pip", l.PIPName())

	nic := l.NIC("/pip/1")
	require.True(t, *nic.Properties.EnableAcceleratedNetworking, "auto is on where the size supports it")
	require.Equal(t, testNSG, *nic.Properties.NetworkSecurityGroup.ID)
	ipc := nic.Properties.IPConfigurations[0].Properties
	require.Equal(t, testSubnet, *ipc.Subnet.ID)
	require.Equal(t, "/pip/1", *ipc.PublicIPAddress.ID)
	require.Equal(t, "/asg/1", *ipc.ApplicationSecurityGroups[0].ID)
	require.Nil(t, l.NIC("").Properties.IPConfigurations[0].Properties.PublicIPAddress)

	// The accelerated networking flag is part of the NIC's name.
	off := l
	off.Settings.AcceleratedNetwork = "false"
	require.NotEqual(t, l.NICName(), off.NICName())
	require.False(t, *off.NIC("").Properties.EnableAcceleratedNetworking)
	// auto without the capability is off.
	b := skus["Standard_B2ats_v2"]
	l.SKU = &b
	require.False(t, l.AcceleratedNetworking())
}

func TestLaunchSpecValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name  string
		r     request
		class vendors.Class
		code  string
	}{
		{"premium on a size without it", request{nodeClaim: "a", size: "Standard_A2_v2"}, vendors.Unsupported, codeInvalidSettings},
		{"spot on a B-series", request{nodeClaim: "a", size: "Standard_B2ats_v2", capacity: vendors.CapacitySpot,
			raw: `{"storage":{"root":{"storageAccountType":"StandardSSD_LRS"}}}`}, vendors.Unsupported, "AzureSpotIsNotSupportedForThisVMSize"},
		{"ephemeral without the capability", request{nodeClaim: "a", raw: `{"storage":{"root":{"ephemeral":{"placement":"CacheDisk"}}}}`}, vendors.Unsupported, codeInvalidSettings},
		{"accelerated networking forced", request{nodeClaim: "a", size: "Standard_B2ats_v2",
			raw: `{"storage":{"root":{"storageAccountType":"StandardSSD_LRS"}}}`}, "", ""},
		{"unknown storage type", request{nodeClaim: "a", raw: `{"storage":{"root":{"storageAccountType":"Magic_LRS"}}}`}, vendors.Fatal, codeInvalidSettings},
		{"user-data too large", request{nodeClaim: "a", userData: []byte(strings.Repeat("x", 16385))}, vendors.Fatal, codeUserDataTooLarge},
		{"bad extra tag", request{nodeClaim: "a", extraTags: map[string]string{"a/b": "c"}}, vendors.Fatal, codeInvalidSettings},
		{"no name", request{}, vendors.Fatal, codeInvalidSettings},
		{"bad json", request{nodeClaim: "a", raw: `{`}, vendors.Fatal, codeInvalidSettings},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, img := c.r.build(t)
			skus := skuTable(t, e, testRegion)
			var sku *SKU
			if s, ok := skus[req.InstanceType]; ok {
				sku = &s
			}
			_, err := NewLaunchSpec(req, testCluster, img, nil, sku, "key")
			if c.class == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, c.class, vendors.ClassOf(err), "%v", err)
			require.Equal(t, c.code, vendors.CodeOf(err))
		})
	}
	// Accelerated networking forced on a size without it.
	req, img := request{nodeClaim: "a", size: "Standard_B2ats_v2", raw: `{"storage":{"root":{"storageAccountType":"StandardSSD_LRS"}}}`}.build(t)
	req.Settings.Network = []byte(`{"acceleratedNetworking":{"enabled":"true"}}`)
	sku := skuTable(t, e, testRegion)["Standard_B2ats_v2"]
	_, err := NewLaunchSpec(req, testCluster, img, nil, &sku, "key")
	require.Equal(t, vendors.Unsupported, vendors.ClassOf(err))
}

func TestMissingNetworkAndDataDiskLimit(t *testing.T) {
	e := newEnv(t)
	req, img := request{nodeClaim: "a"}.build(t)
	req.Prepared = nil
	_, err := NewLaunchSpec(req, testCluster, img, nil, nil, "key")
	require.Error(t, err)

	disks := strings.Repeat(`{"name":"d","diskSizeGB":10},`, 9)
	req, img = request{nodeClaim: "a", raw: `{"storage":{"data":[` + strings.TrimSuffix(disks, ",") + `]}}`}.build(t)
	sku := skuTable(t, e, testRegion)["Standard_D2s_v5"]
	_, err = NewLaunchSpec(req, testCluster, img, nil, &sku, "key")
	require.Equal(t, vendors.Unsupported, vendors.ClassOf(err), "more than MaxDataDiskCount")
}

func TestDeviceHintsAndRootDisk(t *testing.T) {
	e := newEnv(t)
	l := e.spec(t, request{nodeClaim: "a", raw: `{"storage":{"data":[
		{"name":"a","diskSizeGB":10,"filesystem":"xfs","mountPath":"/a"},{"name":"b","diskSizeGB":20}]}}`})
	hints := l.DeviceHints()
	require.Len(t, hints, 2)
	require.Equal(t, "/dev/disk/azure/scsi1/lun0", hints[0].ByID)
	require.Equal(t, "/dev/disk/azure/scsi1/lun1", hints[1].ByID)
	require.EqualValues(t, 10<<30, hints[0].SizeBytes)
	require.Equal(t, "/a", hints[0].MountPath)
	require.EqualValues(t, 64<<30, l.Settings.RootDiskBytes(), "built-in default 64 GiB")
}

func TestDefaultsAndNetworkProfile(t *testing.T) {
	st, err := DecodeSettings(vendors.VendorSettings{})
	require.NoError(t, err)
	r := st.Root()
	require.Equal(t, RootDisk{StorageAccountType: "Premium_LRS", DiskSizeGB: 64, Caching: "ReadWrite"}, r)
	require.Equal(t, "auto", st.AcceleratedNetwork)

	st, err = DecodeSettings(vendors.VendorSettings{Network: []byte(`{"acceleratedNetworking":{"enabled":"false"}}`)})
	require.NoError(t, err)
	require.Equal(t, "false", st.AcceleratedNetwork)
	_, err = DecodeSettings(vendors.VendorSettings{Network: []byte(`{"acceleratedNetworking":{"enabled":"maybe"}}`)})
	require.Error(t, err)
}

func TestNetworkOf(t *testing.T) {
	require.Equal(t, Network{}, NetworkOf(nil))
	n := NetworkOf(&vendors.Prepared{Resources: []vendors.ResourceRef{
		{Kind: KindResourceGroups, ID: "rg"}, {Kind: KindSubnets, ID: "sn"}, {Kind: KindSecurityGroups, ID: "nsg"},
		{Kind: KindAppSecGroups, ID: "a1"}, {Kind: KindAppSecGroups, ID: "a2"}, {Kind: KindPlacement, ID: "ppg"},
		{Kind: "other", ID: "x"},
	}})
	require.Equal(t, Network{ResourceGroup: "rg", SubnetID: "sn", NSGID: "nsg", ASGIDs: []string{"a1", "a2"}, PPGID: "ppg"}, n)
}

func TestGenerateDiscardedKey(t *testing.T) {
	k1, err := GenerateDiscardedKey()
	require.NoError(t, err)
	k2, err := GenerateDiscardedKey()
	require.NoError(t, err)
	require.NotEqual(t, k1, k2)
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k1))
	require.NoError(t, err)
	require.Equal(t, "ssh-ed25519", pub.Type())
	require.NotContains(t, k1, "\n")
}

func TestAdminKeyOverridesGenerated(t *testing.T) {
	e := newEnv(t)
	l := e.spec(t, request{nodeClaim: "a", raw: `{"adminSSHPublicKey":"ssh-ed25519 OPERATOR"}`})
	require.Equal(t, "ssh-ed25519 OPERATOR", l.SSHPublicKey)
}
