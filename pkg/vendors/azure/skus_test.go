/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

func skuTable(t *testing.T, e *env, region string) map[string]SKU {
	t.Helper()
	rs, err := e.clients.SKUs.List(context.Background(), region)
	require.NoError(t, err)
	return ParseSKUs(rs, region)
}

func TestParseSKUs(t *testing.T) {
	e := newEnv(t)
	skus := skuTable(t, e, "westeurope")
	d2 := skus["Standard_D2s_v5"]
	require.Equal(t, SKU{
		Name: "Standard_D2s_v5", Family: "standardDSv5Family", VCPUs: 2, Arch: "amd64", Gen2: true, SCSI: true,
		PremiumIO: true, LowPriorityCapable: true, AcceleratedNetwork: true, MaxDataDisks: 8, Zones: []string{"1", "2", "3"},
	}, d2)
	require.Equal(t, "arm64", skus["Standard_D2ps_v6"].Arch)
	require.True(t, skus["Standard_D2ps_v6"].EphemeralOSDisk)
	require.False(t, skus["Standard_B2ats_v2"].LowPriorityCapable, "B-series have no Spot")
	require.False(t, skus["Standard_A2_v2"].PremiumIO)
	require.False(t, skus["Standard_D2as_v6"].SCSI, "NVMe-only")
	// Not a virtual machine size, or another location: dropped.
	require.Empty(t, ParseSKUs(nil, "westeurope"))
	_, ok := ParseSKU(nil, "westeurope")
	require.False(t, ok)
}

func TestLaunchableTypes(t *testing.T) {
	e := newEnv(t)
	got := LaunchableTypes(skuTable(t, e, "westeurope"))
	require.Equal(t, []string{"1", "2", "3"}, got["Standard_D2s_v5"])
	require.Equal(t, []string{"1", "2"}, got["Standard_D8s_v5"], "zone 3 is restricted for the subscription")
	require.NotContains(t, got, "Standard_A2_v2", "Gen1-only sizes are dropped")
	require.NotContains(t, got, "Standard_D2as_v6", "NVMe-only sizes are dropped")
	require.NotContains(t, got, "Standard_NC6", "restricted in the location")
	require.True(t, RegionUsable(skuTable(t, e, "westeurope")))
	require.False(t, RegionUsable(nil))

	// A region without zones has zone 0.
	nz := LaunchableTypes(skuTable(t, e, "westcentralus"))
	require.Equal(t, []string{ZoneNone}, nz["Standard_D2s_v5"])
}

func TestRegionUnusableWhenEverySizeRestricted(t *testing.T) {
	e := newEnv(t)
	e.sub.AddLocation("brazilus", true)
	require.NoError(t, e.sub.AddSKU("brazilus", fakeSKU("Standard_D2s_v5", true)))
	require.False(t, RegionUsable(skuTable(t, e, "brazilus")))
}

func fakeSKU(name string, restricted bool) fake.SKU {
	return fake.SKU{Name: name, Family: "standardDSv5Family", VCPUs: 2, RestrictedLocation: restricted}
}
