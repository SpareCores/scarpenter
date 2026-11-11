/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/selector"
)

func catalogType(name string) vendors.CatalogType {
	return vendors.CatalogType{Vendor: Vendor, APIReference: name, VCPUs: 2}
}

func TestDefaultsAndRootDisk(t *testing.T) {
	d := New()
	def := d.Defaults()
	require.EqualValues(t, 64<<30, def.RootDiskBytes)
	st, err := DecodeSettings(def)
	require.NoError(t, err)
	require.Equal(t, RootDisk{StorageAccountType: "Premium_LRS", DiskSizeGB: 64, Caching: "ReadWrite"}, st.Root())
	require.EqualValues(t, 100<<30, d.RootDiskBytes(vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"diskSizeGB":100}}}`)}))
	require.EqualValues(t, 64<<30, d.RootDiskBytes(vendors.VendorSettings{Raw: json.RawMessage(`{`)}), "unreadable settings: the default")
	_, ok := d.RootDiskForType(def, catalogType("Standard_D2s_v5"))
	require.False(t, ok)
}

func TestSupported(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	profiles := vendors.Profiles{
		vendors.ProfileStorage: {
			"premium-db":  json.RawMessage(`{"data":[{"name":"a","diskSizeGB":10,"mountPath":"/a"}]}`),
			"ssd":         json.RawMessage(`{"root":{"storageAccountType":"StandardSSD_LRS"},"data":[{"name":"a","storageAccountType":"StandardSSD_LRS","diskSizeGB":10,"mountPath":"/a"}]}`),
			"ephemeral":   json.RawMessage(`{"root":{"ephemeral":{"placement":"CacheDisk"}}}`),
			"local":       json.RawMessage(`{"localStorage":{"policy":"mount"}}`),
			"ignore":      json.RawMessage(`{"localStorage":{"policy":"ignore"}}`),
			"many":        json.RawMessage(`{"data":[` + manyDisks(9) + `]}`),
			"broken-json": json.RawMessage(`{`),
		},
		vendors.ProfileNetwork: {
			"fast": json.RawMessage(`{"acceleratedNetworking":{"enabled":"true"}}`), "off": json.RawMessage(`{"acceleratedNetworking":{"enabled":"false"}}`),
			"auto": json.RawMessage(`{}`), "weird": json.RawMessage(`{"acceleratedNetworking":{"enabled":"maybe"}}`), "broken": json.RawMessage(`[`),
		},
		vendors.ProfilePlacement: {"colo": json.RawMessage(`{"proximityPlacementGroup":{"strategy":"cluster"}}`), "broken": json.RawMessage(`[`)},
	}

	// Until the facts are known a profile that depends on them is not offered.
	got := d.Supported(catalogType("Standard_D2s_v5"), "1", profiles)
	require.Equal(t, []string{"ignore"}, got[vendors.ProfileStorage])
	require.Equal(t, []string{"auto", "off"}, got[vendors.ProfileNetwork])
	require.Equal(t, []string{"colo"}, got[vendors.ProfilePlacement])

	_, err := d.LaunchableTypes(ctx, testRegion)
	require.NoError(t, err)
	got = d.Supported(catalogType("Standard_D2s_v5"), "1", profiles)
	require.Equal(t, []string{"ignore", "premium-db", "ssd"}, got[vendors.ProfileStorage], "no ephemeral disk, a few disks, no mount")
	require.Equal(t, []string{"auto", "fast", "off"}, got[vendors.ProfileNetwork])
	got = d.Supported(catalogType("Standard_D2ps_v6"), "1", profiles)
	require.Contains(t, got[vendors.ProfileStorage], "ephemeral")
	got = d.Supported(catalogType("Standard_B2ats_v2"), "1", profiles)
	require.NotContains(t, got[vendors.ProfileNetwork], "fast", "no accelerated networking on the size")
	got = d.Supported(catalogType("Standard_A2_v2"), "1", profiles)
	require.NotContains(t, got[vendors.ProfileStorage], "premium-db", "no premium storage on the size")
	require.Contains(t, got[vendors.ProfileStorage], "ssd")
	require.Empty(t, d.Supported(catalogType("Standard_D2s_v5"), "1", vendors.Profiles{}))
}

func manyDisks(n int) string {
	var s string
	for i := range n {
		if i > 0 {
			s += ","
		}
		s += `{"name":"d` + string(rune('a'+i)) + `","diskSizeGB":10,"mountPath":"/d` + string(rune('a'+i)) + `"}`
	}
	return s
}

func TestPriceDelta(t *testing.T) {
	prices := StaticPrices{
		Storage: map[string]float64{"westeurope/premium_lrs": 0.146, "westeurope/standardssd_lrs": 0.075},
		IPv4:    map[string]float64{"westeurope": 0.005},
	}
	d := New(WithPrices(prices))
	o := vendors.Offering{Region: testRegion, APIReference: "Standard_D2s_v5"}
	delta := func(raw string) (float64, bool) {
		return d.PriceDelta(vendors.VendorSettings{Raw: json.RawMessage(raw)}, o)
	}

	// A 64 GiB Premium disk is billed as its 64 GiB tier; the address is added.
	p, complete := delta(`{}`)
	require.True(t, complete)
	require.InDelta(t, 64*0.146/730+0.005, p, 1e-9)
	// 65 GiB is the next tier.
	p, _ = delta(`{"storage":{"root":{"diskSizeGB":65}}}`)
	require.InDelta(t, 128*0.146/730+0.005, p, 1e-9)
	// Data disks add; the default SKU of a data disk is Premium_LRS.
	p, complete = delta(`{"storage":{"data":[{"name":"a","diskSizeGB":100}]}}`)
	require.True(t, complete)
	require.InDelta(t, (64+128)*0.146/730+0.005, p, 1e-9)
	// The transactions of Standard SKUs are not priced: incomplete.
	_, complete = delta(`{"storage":{"root":{"storageAccountType":"StandardSSD_LRS"}}}`)
	require.False(t, complete)
	// A disk the source does not price: incomplete.
	_, complete = delta(`{"storage":{"root":{"storageAccountType":"Premium_ZRS"}}}`)
	require.False(t, complete)
	// An ephemeral OS disk costs no disk.
	p, complete = delta(`{"storage":{"root":{"ephemeral":{"placement":"CacheDisk"}}}}`)
	require.True(t, complete)
	require.InDelta(t, 0.005, p, 1e-9)
	// Without a public address there is no address price.
	p, _ = d.PriceDelta(vendors.VendorSettings{Raw: json.RawMessage(`{}`), Networking: common.Networking{PublicIPv4: new(false)}}, o)
	require.InDelta(t, 64*0.146/730, p, 1e-9)
	// A region without an address price, and unreadable settings, are incomplete.
	_, complete = d.PriceDelta(vendors.VendorSettings{}, vendors.Offering{Region: "northeurope"})
	require.False(t, complete)
	_, complete = delta(`{`)
	require.False(t, complete)
	_, complete = New().PriceDelta(vendors.VendorSettings{}, o)
	require.False(t, complete, "no price source")
	require.EqualValues(t, 16384, tierOf(9000))
	require.EqualValues(t, 40000, tierOf(40000))
}

func TestPrepareDefaults(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	p := &provider{d: d, region: testRegion}
	req := vendors.PrepareRequest{
		Region: testRegion, Zone: "1", InstanceType: "Standard_D2s_v5", Resources: p,
		Tags:     map[vendors.TagKey]string{vendors.TagNodeClass: "class"},
		Settings: vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"data":[{"name":"pgdata","diskSizeGB":50,"mountPath":"/data"}]}}`)},
	}
	prep, err := d.Prepare(ctx, req)
	require.NoError(t, err)
	require.Equal(t, []vendors.ResourceKind{KindResourceGroups, KindSubnets, KindSecurityGroups}, p.kinds())
	byKind := map[vendors.ResourceKind]vendors.ResourceRef{}
	for _, r := range prep.Resources {
		byKind[r.Kind] = r
	}
	require.True(t, byKind[KindSecurityGroups].Managed)
	require.NotContains(t, byKind, KindAppSecGroups, "isolation none: no allow between the nodes")
	require.Equal(t, []vendors.DeviceHint{{Name: "pgdata", Filesystem: "ext4", MountPath: "/data", ByID: "/dev/disk/azure/scsi1/lun0", SizeBytes: 50 << 30}}, prep.Devices)

	// Prepare is idempotent: the second run finds what the first made.
	again, err := d.Prepare(ctx, req)
	require.NoError(t, err)
	require.Equal(t, prep.Resources, again.Resources)

	// The network of the launch is read back by kind.
	n := NetworkOf(prep)
	require.Equal(t, byKind[KindResourceGroups].ID, n.ResourceGroup)
	require.Equal(t, byKind[KindSubnets].ID, n.SubnetID)
	require.Equal(t, byKind[KindSecurityGroups].ID, n.NSGID)
}

func TestPrepareIsolation(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	prepare := func(iso common.Isolation, runKey string, use map[vendors.ResourceKind]vendors.UseRef) (*vendors.Prepared, *provider, error) {
		p := &provider{d: d, region: testRegion, use: use}
		prep, err := d.Prepare(ctx, vendors.PrepareRequest{
			Region: testRegion, Zone: "1", Resources: p, RunKey: runKey,
			Tags: map[vendors.TagKey]string{vendors.TagNodeClass: "class"},
			Settings: vendors.VendorSettings{Networking: common.Networking{Isolation: iso,
				Inbound: &common.Inbound{Rules: []common.InboundRule{{Protocol: "udp", FromPort: new(int32(41641)), CIDRs: []string{"0.0.0.0/0"}}}}}},
		})
		return prep, p, err
	}
	kinds := func(prep *vendors.Prepared) map[vendors.ResourceKind]vendors.ResourceRef {
		out := map[vendors.ResourceKind]vendors.ResourceRef{}
		for _, r := range prep.Resources {
			out[r.Kind] = r
		}
		return out
	}

	// nodeclass: the class's group allows its nodes, which join the application group.
	prep, _, err := prepare(common.IsolationNodeClass, "", nil)
	require.NoError(t, err)
	k := kinds(prep)
	require.Contains(t, k, KindAppSecGroups)
	require.Equal(t, ASGIDOf(k[KindSecurityGroups].ID), k[KindAppSecGroups].ID)

	// run without a run key is one implicit run: the same as nodeclass.
	prep, _, err = prepare(common.IsolationRun, "", nil)
	require.NoError(t, err)
	require.Contains(t, kinds(prep), KindAppSecGroups)
	require.NotContains(t, kinds(prep), KindRunSecurityGroups)

	// run with a run key: the run's own group and application group.
	prep, p, err := prepare(common.IsolationRun, "run-a", nil)
	require.NoError(t, err)
	k = kinds(prep)
	require.Contains(t, k, KindRunSecurityGroups)
	require.NotContains(t, k, KindSecurityGroups, "a NIC takes one group")
	require.Equal(t, ASGIDOf(k[KindRunSecurityGroups].ID), k[KindAppSecGroups].ID)
	require.Contains(t, p.kinds(), KindSecurityGroups, "the class's group exists too, unused by this run")
	other, _, err := prepare(common.IsolationRun, "run-b", nil)
	require.NoError(t, err)
	require.NotEqual(t, k[KindRunSecurityGroups].ID, kinds(other)[KindRunSecurityGroups].ID, "one group per run")

	// A group the NodeClass brings decides alone, whatever the isolation.
	e.userNetwork(t, "user-rg", "user-vnet", nil, "10.0.0.0/24")
	user, err := e.sub.NSGs().CreateOrUpdate(ctx, "user-rg", "user-nsg", armnetwork.SecurityGroup{Location: new(testRegion)})
	require.NoError(t, err)
	use := map[vendors.ResourceKind]vendors.UseRef{KindSecurityGroups: {Name: "user-nsg"}}
	prep, _, err = prepare(common.IsolationRun, "run-a", use)
	require.NoError(t, err)
	k = kinds(prep)
	require.Equal(t, *user.ID, k[KindSecurityGroups].ID)
	require.NotContains(t, k, KindRunSecurityGroups)
	require.NotContains(t, k, KindAppSecGroups)
	require.True(t, d.warned["class/"+testRegion], "said once that isolation has no effect")
	_, _, err = prepare(common.IsolationNone, "", use)
	require.NoError(t, err)
}

func TestPrepareBroughtNetworkAndPlacement(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	ctx := context.Background()
	v := e.userNetwork(t, "user-rg", "user-vnet", map[string]string{"env": "x"}, "10.0.0.0/30")
	use := map[vendors.ResourceKind]vendors.UseRef{
		KindResourceGroups: {IDs: []string{"user-rg"}},
		KindSubnets:        {Tags: map[string]string{"env": "x"}, Select: common.SelectAll},
	}
	prepare := func(placement string, runKey string) (*vendors.Prepared, error) {
		return d.Prepare(ctx, vendors.PrepareRequest{
			Region: testRegion, Zone: "1", RunKey: runKey,
			Resources: &provider{d: d, region: testRegion, use: use},
			Tags:      map[vendors.TagKey]string{vendors.TagNodeClass: "class"},
			Settings: vendors.VendorSettings{Placement: json.RawMessage(placement),
				Profiles: map[string]string{vendors.ProfilePlacement: "colo"}},
		})
	}
	// Two subnets: the one with the most free addresses (a /30 has none).
	prep, err := prepare("", "")
	require.NoError(t, err)
	n := NetworkOf(prep)
	require.Equal(t, "user-rg", n.ResourceGroup)
	require.Equal(t, *v.Properties.Subnets[1].ID, n.SubnetID)

	// A placement group is created per run key, or brought by name.
	_, err = prepare(`{"proximityPlacementGroup":{"strategy":"cluster"}}`, "")
	require.ErrorContains(t, err, "needs a run key")
	prep, err = prepare(`{"proximityPlacementGroup":{"strategy":"cluster"}}`, "run-a")
	require.NoError(t, err)
	ppg := NetworkOf(prep).PPGID
	require.NotEmpty(t, ppg)
	prep, err = prepare(`{"proximityPlacementGroup":{"strategy":"cluster"}}`, "run-a")
	require.NoError(t, err)
	require.Equal(t, ppg, NetworkOf(prep).PPGID, "the same run, the same group")
	_, err = prepare(`{"proximityPlacementGroup":{"use":{"name":"missing"}}}`, "")
	var nm *selector.NoMatchError
	require.ErrorAs(t, err, &nm, "the engine turns the verdict into ResourceMissing")
	_, err = prepare(`{`, "")
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))

	// A subnet without free addresses ends the zone.
	e.sub.AddResourceGroup("full-rg")
	use[KindSubnets] = vendors.UseRef{Name: "user-vnet/a", Select: common.SelectAll}
	use[KindResourceGroups] = vendors.UseRef{IDs: []string{"full-rg"}}
	prep, err = prepare("", "")
	require.NoError(t, err, "a single subnet is taken as it is: the platform reports a full one")
	use[KindSubnets] = vendors.UseRef{Name: "user-vnet/*", Select: common.SelectAll}
	_, err = d.chooseSubnet(ctx, []vendors.ResourceRef{{ID: *v.Properties.Subnets[0].ID}, {ID: *v.Properties.Subnets[0].ID}})
	require.Equal(t, vendors.CodeResourceMissingInZone, vendors.CodeOf(err))
	_, err = onlyRef(nil, "x")
	require.Equal(t, vendors.CodeResourceMissing, vendors.CodeOf(err))
	_, err = onlyRef([]vendors.ResourceRef{{}, {}}, "x")
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
	require.Equal(t, 0, freeAddresses(armnetwork.Subnet{Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new("garbage")}}))
	require.Equal(t, 251, freeAddresses(armnetwork.Subnet{Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefixes: []*string{new("10.0.0.0/24")}}}))
}

func TestPrepareErrorsPassThrough(t *testing.T) {
	e := newEnv(t)
	d := e.driver(t)
	failing := &failingProvider{kind: KindSubnets, err: vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone, context.Canceled)}
	_, err := d.Prepare(context.Background(), vendors.PrepareRequest{Region: testRegion, Zone: "1", Resources: failing})
	require.Equal(t, vendors.CodeResourceMissingInZone, vendors.CodeOf(err), "the attempt loop reads the code")
	for _, kind := range []vendors.ResourceKind{KindResourceGroups, KindSecurityGroups} {
		_, err := d.Prepare(context.Background(), vendors.PrepareRequest{Region: testRegion, Zone: "1",
			Resources: &failingProvider{kind: kind, err: vendors.Errorf(vendors.Fatal, "boom")}})
		require.Error(t, err, kind)
	}
	_, err = d.Prepare(context.Background(), vendors.PrepareRequest{Region: testRegion, Settings: vendors.VendorSettings{Raw: json.RawMessage(`{`)}})
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
}

// failingProvider answers every kind with a managed resource but one, which fails.
type failingProvider struct {
	kind vendors.ResourceKind
	err  error
}

func (p *failingProvider) Require(_ context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	if req.Kind == p.kind {
		return nil, p.err
	}
	return []vendors.ResourceRef{{Kind: req.Kind, ID: "/subscriptions/s/resourceGroups/g/providers/Microsoft.Network/networkSecurityGroups/n", Managed: true}}, nil
}

func vendors_settings(raw, network []byte) vendors.VendorSettings {
	return vendors.VendorSettings{Raw: raw, Network: network}
}
