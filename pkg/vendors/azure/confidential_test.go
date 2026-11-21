/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

const (
	snpSize = "Standard_DC2as_v5"
	tdxSize = "Standard_DC2es_v6"
)

// confidentialEnv adds an SNP, a TDX and an SGX size to every location and the
// image versions to westeurope only.
func confidentialEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	for _, loc := range []string{"westeurope", "northeurope", "westcentralus"} {
		for _, k := range []fake.SKU{
			{Name: snpSize, Family: "standardDCASv5Family", VCPUs: 2, NoAcceleratedNet: true, Confidential: "SNP"},
			{Name: tdxSize, Family: "standardDCEV6Family", VCPUs: 2, Confidential: "TDX"},
			{Name: "Standard_DC2s_v3", Family: "standardDCSv3Family", VCPUs: 2},
		} {
			require.NoError(t, e.sub.AddSKU(loc, k))
		}
	}
	day := 24 * time.Hour
	now := time.Now()
	e.sub.AddCommunityVersion("westeurope", ConfidentialGallery, ConfidentialImageDefinition, "202610.30.1", now.Add(-20*day), false)
	e.sub.AddCommunityVersion("westeurope", ConfidentialGallery, ConfidentialImageDefinition, "202611.13.0", now.Add(-5*day), false)
	e.sub.AddCommunityVersion("westeurope", ConfidentialGallery, ConfidentialImageDefinition, "202611.20.0", now.Add(-1*day), true)
	return e
}

func TestConfidentialDetection(t *testing.T) {
	skus := skuTable(t, confidentialEnv(t), "westeurope")
	require.Equal(t, ConfidentialSNP, skus[snpSize].Confidential)
	require.Equal(t, ConfidentialTDX, skus[tdxSize].Confidential)
	require.Empty(t, skus["Standard_DC2s_v3"].Confidential, "SGX sizes report no type: plain Flatcar")
	require.Empty(t, skus["Standard_D2s_v5"].Confidential)
}

func TestConfidentialVMBody(t *testing.T) {
	e := confidentialEnv(t)
	skus := skuTable(t, e, testRegion)
	id := "/CommunityGalleries/g/Images/i/Versions/1.0.0"
	for _, size := range []string{snpSize, tdxSize} {
		req, img := request{nodeClaim: "nc-cvm", size: size}.build(t)
		sku := skus[size]
		l, err := NewLaunchSpec(req, testCluster, img, nil, id, &sku, "key")
		require.NoError(t, err)
		p := l.VM("/nic").Properties
		require.Equal(t, armcompute.ImageReference{CommunityGalleryImageID: new(id)}, *p.StorageProfile.ImageReference, size)
		require.Equal(t, armcompute.SecurityTypesConfidentialVM, *p.SecurityProfile.SecurityType)
		require.True(t, *p.SecurityProfile.UefiSettings.SecureBootEnabled)
		require.True(t, *p.SecurityProfile.UefiSettings.VTpmEnabled)
		require.Equal(t, armcompute.SecurityEncryptionTypesVMGuestStateOnly,
			*p.StorageProfile.OSDisk.ManagedDisk.SecurityProfile.SecurityEncryptionType)
		require.False(t, *l.NIC("").Properties.EnableAcceleratedNetworking, "%s: accelerated networking is off", size)
		require.NotEmpty(t, *p.OSProfile.CustomData, "Ignition is the custom data")
		require.Nil(t, l.VM("/nic").Plan)
	}
}

func TestConfidentialSettingsAreUnsupported(t *testing.T) {
	e := confidentialEnv(t)
	sku := skuTable(t, e, testRegion)[snpSize]
	for name, set := range map[string]struct{ raw, network string }{
		"ephemeral OS disk":             {raw: `{"storage":{"root":{"ephemeral":{"placement":"CacheDisk"}}}}`},
		"encryption at host":            {raw: `{"encryptionAtHost":true}`},
		"forced accelerated networking": {network: `{"acceleratedNetworking":{"enabled":"true"}}`},
	} {
		req, img := request{nodeClaim: "nc-x", size: snpSize, raw: set.raw}.build(t)
		req.Settings.Network = []byte(set.network)
		_, err := NewLaunchSpec(req, testCluster, img, nil, "id", &sku, "key")
		require.True(t, vendors.IsClass(err, vendors.Unsupported), "%s: %v", name, err)
	}
}

func TestConfidentialImageIsTheNewestIncludedVersion(t *testing.T) {
	e := confidentialEnv(t)
	d := e.driver(t)
	id, err := d.confidentialImage(context.Background(), "westeurope")
	require.NoError(t, err)
	require.Equal(t, "/CommunityGalleries/"+ConfidentialGallery+"/Images/"+ConfidentialImageDefinition+"/Versions/202611.13.0", id,
		"the newest version that is not excluded from latest")
	require.Equal(t, 1, e.sub.Calls("image.community"))
	_, err = d.confidentialImage(context.Background(), "westeurope")
	require.NoError(t, err)
	require.Equal(t, 1, e.sub.Calls("image.community"), "cached")
}

func TestConfidentialSizeBootsTheCommunityImage(t *testing.T) {
	e := confidentialEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	for _, size := range []string{snpSize, tdxSize} {
		req := e.launchReq(t, "cvm-"+size[len(size)-6:])
		req.InstanceType = size
		inst, err := d.CreateInstance(ctx, req)
		require.NoError(t, err, size)
		require.Equal(t, vendors.StateRunning, inst.State)
	}
	// A plain size of the same request still boots the Flatcar image.
	_, err := d.CreateInstance(ctx, e.launchReq(t, "plain"))
	require.NoError(t, err)
}

func TestConfidentialSizeIsUnsupportedWhereTheImageIsMissing(t *testing.T) {
	e := confidentialEnv(t)
	d := e.driver(t)
	req := e.launchReq(t, "cvm-north")
	req.Region = "northeurope"
	req.InstanceType = snpSize
	_, err := d.CreateInstance(context.Background(), req)
	require.True(t, vendors.IsClass(err, vendors.Unsupported), "%v", err)
	require.Equal(t, codeConfidentialImageNotFound, vendors.CodeOf(err))
	require.Contains(t, err.Error(), "northeurope")

	// The miss is cached for a short time, then looked up again.
	_, err = d.CreateInstance(context.Background(), req)
	require.True(t, vendors.IsClass(err, vendors.Unsupported))
	require.Equal(t, 1, e.sub.Calls("image.community"))
}
