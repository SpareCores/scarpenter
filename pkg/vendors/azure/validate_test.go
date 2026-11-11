/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	azapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/azure"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

const (
	subnetARM = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/subnets/n"
	nsgARM    = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkSecurityGroups/n"
	ppgARM    = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/proximityPlacementGroups/p"
	validKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"
)

func TestValidateNodeClass(t *testing.T) {
	d := New(WithSubscription("00000000-0000-0000-0000-000000000001"))
	block := func(s azapi.Settings) *v1alpha1.SCNodeClass {
		nc := &v1alpha1.SCNodeClass{}
		nc.Spec.Vendors.Azure = &s
		return nc
	}
	res := func(r azapi.Resources) *v1alpha1.SCNodeClass { return block(azapi.Settings{Resources: &r}) }
	use := func(u common.ResourceUse) *common.Resource { return &common.Resource{Use: &u} }
	ids := func(region string, id ...string) common.ResourceUse {
		return common.ResourceUse{IDs: map[string]common.IDList{region: id}}
	}
	cases := []struct {
		name string
		nc   *v1alpha1.SCNodeClass
		want string // a substring of the error; "" means valid
	}{
		{"empty block", block(azapi.Settings{}), ""},
		{"a full valid block", block(azapi.Settings{
			Regions: []string{"westeurope", "northeurope"}, SubscriptionID: "00000000-0000-0000-0000-000000000001", ImageOwner: "kinvolk",
			AdminSSHPublicKey: validKey, EncryptionAtHost: new(true),
			Storage: &azapi.Storage{Root: &azapi.RootDisk{StorageAccountType: "StandardSSD_LRS", DiskSizeGB: 128, Caching: "ReadOnly",
				DiskEncryptionSetID: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/diskEncryptionSets/des"},
				Data:         []azapi.DataDisk{{GuestVolume: common.GuestVolume{Name: "d", MountPath: "/d"}, DiskSizeGB: 100}},
				LocalStorage: &azapi.LocalStorage{Policy: "ignore"}},
		}), ""},
		{"a region name", block(azapi.Settings{Regions: []string{"westeurope", "West Europe"}}), `"West Europe" is not an Azure location name`},
		{"another subscription", block(azapi.Settings{SubscriptionID: "11111111-1111-1111-1111-111111111111"}), "another subscription"},
		{"a bad key", block(azapi.Settings{AdminSSHPublicKey: "not a key"}), "authorized_keys"},
		{"a root disk type", block(azapi.Settings{Storage: &azapi.Storage{Root: &azapi.RootDisk{StorageAccountType: "Ultra"}}}), "is not one of"},
		{"a root size", block(azapi.Settings{Storage: &azapi.Storage{Root: &azapi.RootDisk{DiskSizeGB: 5000}}}), "30 to 4095"},
		{"an encryption set", block(azapi.Settings{Storage: &azapi.Storage{Root: &azapi.RootDisk{DiskEncryptionSetID: "des"}}}), "disk encryption set"},
		{"ephemeral with a type", block(azapi.Settings{Storage: &azapi.Storage{Root: &azapi.RootDisk{
			StorageAccountType: "Premium_LRS", Ephemeral: &azapi.Ephemeral{Placement: "CacheDisk"}}}}), "no storageAccountType"},
		{"ephemeral with caching", block(azapi.Settings{Storage: &azapi.Storage{Root: &azapi.RootDisk{
			Caching: "ReadWrite", Ephemeral: &azapi.Ephemeral{Placement: "CacheDisk"}}}}), "read-only cached"},
		{"a data disk", block(azapi.Settings{Storage: &azapi.Storage{Data: []azapi.DataDisk{
			{GuestVolume: common.GuestVolume{Name: "d"}, DiskSizeGB: 0, StorageAccountType: "x"},
			{GuestVolume: common.GuestVolume{Name: "d"}, DiskSizeGB: 40000}}}}), "used twice"},
		{"a data disk size", block(azapi.Settings{Storage: &azapi.Storage{Data: []azapi.DataDisk{{GuestVolume: common.GuestVolume{Name: "d"}, DiskSizeGB: 40000}}}}), "1 to 32767"},
		{"a data disk type", block(azapi.Settings{Storage: &azapi.Storage{Data: []azapi.DataDisk{{GuestVolume: common.GuestVolume{Name: "d"}, DiskSizeGB: 4, StorageAccountType: "x"}}}}), "is not one of"},
		{"mounting local disks", block(azapi.Settings{Storage: &azapi.Storage{LocalStorage: &azapi.LocalStorage{Policy: "mount"}}}), "only the policy ignore"},
		{"an inbound CIDR", block(azapi.Settings{Networking: &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"nope"}}}}}}), "is not a CIDR"},
		{"an IPv6 inbound CIDR", block(azapi.Settings{Networking: &common.NetworkingOverride{Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"::/0"}}}}}}), "IPv4 only"},
		{"IPv6", block(azapi.Settings{Networking: &common.NetworkingOverride{PublicIPv6: new(true)}}), "IPv4 only in v1"},
		{"no public address", block(azapi.Settings{Networking: &common.NetworkingOverride{PublicIPv4: new(false)}}), "no egress"},
		{"no public address on brought subnets", block(azapi.Settings{Networking: &common.NetworkingOverride{PublicIPv4: new(false)},
			Resources: &azapi.Resources{Subnets: use(ids("westeurope", subnetARM))}}), ""},

		{"resource group name and ARM ID", res(azapi.Resources{ResourceGroups: use(ids("westeurope", "my-rg", "/subscriptions/s/resourceGroups/g"))}), ""},
		{"a resource group ID", res(azapi.Resources{ResourceGroups: use(ids("westeurope", "bad name!"))}), "not a resource group name"},
		{"a resource group ARM ID", res(azapi.Resources{ResourceGroups: use(ids("westeurope", "/subscriptions/s/nope"))}), "ARM ID of a resource group"},
		{"subnet ids", res(azapi.Resources{Subnets: use(ids("westeurope", subnetARM, nsgARM))}), "not the ARM ID of a subnet"},
		{"security group ids", res(azapi.Resources{SecurityGroups: use(ids("westeurope", subnetARM))}), "network security group"},
		{"placement group ids", res(azapi.Resources{ProximityPlacementGroups: use(ids("westeurope", ppgARM, "x"))}), "proximity placement group"},
		{"ids for a bad region", res(azapi.Resources{SecurityGroups: use(ids("Nowhere", nsgARM))}), `"Nowhere" is not a region`},
		{"a placeholder ID is checked later", res(azapi.Resources{SecurityGroups: use(ids("westeurope", "{nodeclass}"))}), ""},
		{"the zone placeholder", res(azapi.Resources{Subnets: use(common.ResourceUse{Name: "v/{zone}"})}), "{zone} is only valid on zonal kinds"},
		{"an expression", res(azapi.Resources{Subnets: use(common.ResourceUse{Expression: "resources"})}), "expression is not supported"},
		{"created ordering", res(azapi.Resources{ResourceGroups: use(common.ResourceUse{Name: "x", Select: common.SelectNewest, OrderBy: "created"})}), "no creation time"},
		{"a tag of the selector", res(azapi.Resources{SecurityGroups: use(common.ResourceUse{Tags: map[string]string{"a/b": "c"}})}), "tag name"},
		{"a wildcard tag", res(azapi.Resources{SecurityGroups: use(common.ResourceUse{Tags: map[string]string{"scarpenter-use": "*", "x-{region}": "y"}})}), ""},
		{"images by URN", res(azapi.Resources{Images: use(ids("westeurope", "kinvolk:offer:sku:4547.1.0"))}), ""},
		{"images by a gallery ID", res(azapi.Resources{Images: use(ids("westeurope", "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/galleries/g/images/i/versions/1.0.0"))}), "gallery images are not supported"},
		{"images by a bad URN", res(azapi.Resources{Images: use(ids("westeurope", "kinvolk:offer"))}), "not publisher:offer:sku:version"},
		{"images by a pattern", res(azapi.Resources{Images: use(common.ResourceUse{Name: "kinvolk:offer:stable-gen2:4547.*"})}), ""},
		{"images by a bad pattern", res(azapi.Resources{Images: use(common.ResourceUse{Name: "kinvolk:*:stable-gen2:4547.*"})}), "the first three parts are exact"},
		{"images by tags", res(azapi.Resources{Images: use(common.ResourceUse{Tags: map[string]string{"a": "b"}})}), "tags are not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := d.ValidateNodeClass(context.Background(), tc.nc)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateProfilesAndTags(t *testing.T) {
	d := New()
	nc := &v1alpha1.SCNodeClass{}
	nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{
		"bad": {Azure: &azapi.Storage{Root: &azapi.RootDisk{StorageAccountType: "x"}}},
		"ok":  {Azure: &azapi.Storage{Data: []azapi.DataDisk{{GuestVolume: common.GuestVolume{Name: "a", MountPath: "/a"}, DiskSizeGB: 1}}}},
	}
	nc.Spec.Profiles.Network = map[string]v1alpha1.NetworkProfile{
		"fast":  {Azure: &azapi.Network{AcceleratedNetworking: &azapi.AcceleratedNetworking{Enabled: "true"}}},
		"weird": {Azure: &azapi.Network{AcceleratedNetworking: &azapi.AcceleratedNetworking{Enabled: "maybe"}}},
	}
	nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{
		"both":    {Azure: &azapi.Placement{ProximityPlacementGroup: &azapi.ProximityPlacementGroup{Strategy: "cluster", Use: &azapi.ProximityPlacementGroupRef{Name: "x"}}}},
		"neither": {Azure: &azapi.Placement{ProximityPlacementGroup: &azapi.ProximityPlacementGroup{}}},
		"fine":    {Azure: &azapi.Placement{ProximityPlacementGroup: &azapi.ProximityPlacementGroup{Strategy: "cluster"}}},
	}
	nc.Spec.Tags = map[string]string{"team": "x", "bad/key": "y", "scarpenter-cluster": "z", "Name": "n", "long": strings.Repeat("x", 300)}
	err := d.ValidateNodeClass(context.Background(), nc)
	require.Error(t, err)
	for _, want := range []string{
		"profiles.storage.bad.azure.root.storageAccountType", "profiles.network.weird.azure", "profiles.placement.both.azure",
		"profiles.placement.neither.azure", "tags: tag name \"bad/key\"", "reserved for SCarpenter", "exceeds 256",
	} {
		require.Contains(t, err.Error(), want)
	}
	require.NotContains(t, err.Error(), "profiles.storage.ok")
	require.NotContains(t, err.Error(), "profiles.placement.fine")
	require.NotContains(t, err.Error(), "profiles.network.fast")

	many := &v1alpha1.SCNodeClass{}
	many.Spec.Tags = map[string]string{}
	for i := range 45 {
		many.Spec.Tags["k"+strings.Repeat("a", i)] = "v"
	}
	require.ErrorContains(t, d.ValidateNodeClass(context.Background(), many), "at most 40")
}

// TestAPITypesMatchTheSettings round-trips a block with every field of the API
// types through the driver's own decoding: the two carry the same JSON names, so
// a field added to one and not the other fails here.
func TestAPITypesMatchTheSettings(t *testing.T) {
	api := azapi.Settings{
		SubscriptionID: "sub", ImageOwner: "owner", AdminSSHPublicKey: validKey, EncryptionAtHost: new(true),
		Storage: &azapi.Storage{
			Root: &azapi.RootDisk{StorageAccountType: "Premium_ZRS", DiskSizeGB: 100, Caching: "ReadOnly", DiskEncryptionSetID: "des",
				Ephemeral: &azapi.Ephemeral{Placement: "NvmeDisk"}},
			Data: []azapi.DataDisk{{GuestVolume: common.GuestVolume{Name: "d", Filesystem: "xfs", MountPath: "/d"},
				StorageAccountType: "StandardSSD_LRS", DiskSizeGB: 20, Caching: "ReadWrite"}},
			LocalStorage: &azapi.LocalStorage{Policy: "ignore"},
		},
	}
	raw, err := json.Marshal(api)
	require.NoError(t, err)
	st, err := DecodeSettings(vendors_settings(raw, nil))
	require.NoError(t, err)
	require.Equal(t, "sub", st.SubscriptionID)
	require.Equal(t, "owner", st.ImageOwner)
	require.Equal(t, validKey, st.AdminSSHPublicKey)
	require.True(t, *st.EncryptionAtHost)
	r := st.Root()
	require.Equal(t, RootDisk{StorageAccountType: "Premium_ZRS", DiskSizeGB: 100, Caching: "ReadOnly", DiskEncryptionSetID: "des",
		Ephemeral: &Ephemeral{Placement: "NvmeDisk"}}, r)
	require.Equal(t, []DataDisk{{Name: "d", Filesystem: "xfs", MountPath: "/d", StorageAccountType: "StandardSSD_LRS", DiskSizeGB: 20, Caching: "ReadWrite"}}, st.DataDisks())
	require.Equal(t, "ignore", st.Storage.LocalStorage.Policy)

	// The profile blocks too.
	net, err := json.Marshal(azapi.Network{AcceleratedNetworking: &azapi.AcceleratedNetworking{Enabled: "true"}})
	require.NoError(t, err)
	st, err = DecodeSettings(vendors_settings(nil, net))
	require.NoError(t, err)
	require.Equal(t, "true", st.AcceleratedNetwork)
	pl, err := json.Marshal(azapi.Placement{ProximityPlacementGroup: &azapi.ProximityPlacementGroup{Use: &azapi.ProximityPlacementGroupRef{Name: "p"}}})
	require.NoError(t, err)
	got, err := decodePlacement(pl)
	require.NoError(t, err)
	require.Equal(t, "p", got.ProximityPlacementGroup.Use.Name)
	require.Equal(t, "owner", (&api).GetImageOwner())
	require.Empty(t, (*azapi.Settings)(nil).GetImageOwner())
	require.Empty(t, (*azapi.Settings)(nil).GetRegions())
}
