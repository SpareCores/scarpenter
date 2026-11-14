/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// hoursPerMonth converts the GB-month prices of Keeper's storage rows.
const hoursPerMonth = 730

// defaultSettings are the built-in defaults of layer 1 (plan 25, section 8): a 64 GB boot volume of
// the balanced level.
const defaultSettings = `{"storage":{"root":{"sizeGb":64,"performance":"balanced"}}}`

// keeperPrices holds the storage prices (USD per GB-month by region and level) of the Keeper data
// version the catalog loaded last. Until the first call, and for anything the data lacks, they are
// unknown. IPv4 is free on OCI: Keeper has no row for it.
type keeperPrices struct {
	mu      sync.RWMutex
	storage map[string]float64
}

func (k *keeperPrices) price(region, level string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.storage[region+"/"+level]
	return v, ok
}

// SetKeeperPrices implements vendors.KeeperPriceConsumer.
func (d *Driver) SetKeeperPrices(p vendors.KeeperPrices) {
	storage := map[string]float64{}
	for _, r := range vendors.GBMonth(p.Storage) {
		storage[r.Region+"/"+strings.ToLower(r.StorageID)] = r.USD
	}
	d.prices.mu.Lock()
	changed := d.prices.storage == nil || !maps.Equal(d.prices.storage, storage)
	d.prices.storage = storage
	d.prices.mu.Unlock()
	if changed {
		d.epoch.Add(1)
	}
}

// PricesPerRegion implements vendors.RegionPriced: availability domains are named and numbered per
// tenancy, so Keeper's per-AD prices are read as the region's (plan 25, section 6).
func (d *Driver) PricesPerRegion() bool { return true }

// NodeOptionsEpoch implements vendors.EpochReporter: PriceDelta depends on prices that load at
// runtime.
func (d *Driver) NodeOptionsEpoch() uint64 { return d.epoch.Load() }

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings), RootDiskBytes: DefaultRootGB * giB}
	return s
}

// RootDiskBytes implements vendors.NodeOptions: the root volume of the settings (the image needs
// about 47 GiB; the volume is the NodeClass's, not the type's).
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	st, err := decodeSettings(s)
	if err != nil {
		return 0
	}
	return st.Storage.Root.SizeGb * giB
}

// RootDiskForType implements vendors.NodeOptions: the root volume is not the type's.
func (d *Driver) RootDiskForType(vendors.VendorSettings, vendors.CatalogType) (int64, bool) {
	return 0, false
}

// Supported implements vendors.NodeOptions: OCI has no storage, network or placement profile blocks
// in v1, so no profile is supported anywhere.
func (d *Driver) Supported(vendors.CatalogType, string, vendors.Profiles) map[string][]string {
	return nil
}

// PriceDelta implements vendors.NodeOptions: the root volume's GB times the GB-month price of the
// region and performance level over 730 hours (the level's price includes its VPUs: balanced
// 0.0425 = 0.0255 + 10 x 0.0017). It is incomplete when the price is unknown, for a volume with more
// VPUs than the level's price covers, and for the boot volume's free allowance, which is not
// modelled. IPv4 is free.
func (d *Driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	st, err := decodeSettings(s)
	if err != nil {
		return 0, false
	}
	r := st.Storage.Root
	p, ok := d.prices.price(o.Region, r.Performance)
	if !ok {
		return 0, false
	}
	complete := r.Performance != PerformanceUltraHigh || r.VpusPerGb == 0 || r.VpusPerGb == defaultUltraVPUs
	return float64(r.SizeGb) * p / hoursPerMonth, complete
}

// Prepare implements vendors.NodeOptions: it makes the resources of the launch ready through the
// resource provider (the engine decides whether a resource is brought or managed): the subnet that
// serves the zone and the network security groups in its VCN, the NodeClass's and, for isolation
// run, the run's. An error of the provider is returned as it is: a ResourceMissingInZone must reach
// the attempt loop with its code, so that it skips only the zone.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	if _, err := decodeSettings(req.Settings); err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeSettings, err)
	}
	var params json.RawMessage
	if cidr := createCIDR(req.Settings); cidr != "" {
		var err error
		if params, err = json.Marshal(SubnetParams{CIDR: cidr}); err != nil {
			return nil, vendors.NewError(vendors.Fatal, codeSettings, err)
		}
	}
	subnetRefs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindSubnets, Params: params, Settings: &req.Settings})
	if err != nil {
		return nil, err
	}
	net := req.Settings.Networking
	sn, err := d.chooseSubnet(ctx, req.Region, req.Zone, subnetRefs,
		boolOr(net.PublicIPv4, true), boolOr(net.PublicIPv6, false))
	if err != nil {
		return nil, err
	}
	prep := &vendors.Prepared{Resources: []vendors.ResourceRef{
		{Kind: KindSubnets, Region: req.Region, ID: sn.ID, Network: sn.VCN}}}
	groups, err := d.requireGroups(ctx, req, sn)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, groups...)
	if len(groups) > maxNSGs {
		return nil, vendors.NewError(vendors.Fatal, codeSettings, fmt.Errorf(
			"%d network security groups for one VNIC; OCI allows %d", len(groups), maxNSGs))
	}
	return prep, nil
}

// createCIDR is the range of the create block of the subnets kind in the NodeClass's resources, or "".
func createCIDR(s vendors.VendorSettings) string {
	if len(s.Resources) == 0 {
		return ""
	}
	var r struct {
		Subnets *struct {
			Create *struct {
				CIDR string `json:"cidr"`
			} `json:"create"`
		} `json:"subnets"`
	}
	if json.Unmarshal(s.Resources, &r) != nil || r.Subnets == nil || r.Subnets.Create == nil {
		return ""
	}
	return r.Subnets.Create.CIDR
}

// requireGroups returns the network security groups of the launch: those the NodeClass brings, or
// its own group in the VCN of the subnet, and, for isolation run with a run key, the group of the
// run. A brought group must be in the subnet's VCN; where it is not the zone is skipped.
func (d *Driver) requireGroups(ctx context.Context, req vendors.PrepareRequest, sn subnetInfo,
) ([]vendors.ResourceRef, error) {
	net := req.Settings.Networking
	class := req.Tags[vendors.TagNodeClass]
	params := GroupParams{
		VCN: sn.VCN,
		// Isolation run without a run key has no run to keep apart: the nodes of the NodeClass are
		// one implicit run and reach each other.
		SelfReference: net.Isolation == common.IsolationNodeClass ||
			(net.Isolation == common.IsolationRun && req.RunKey == ""),
	}
	if net.Inbound != nil {
		params.Inbound = net.Inbound.Rules
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeSettings, err)
	}
	refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindNetworkSecurityGroups, Key: class + "/" + sn.VCN, Params: raw, Network: sn.VCN, Settings: &req.Settings})
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if !r.Managed && r.Network != "" && r.Network != sn.VCN {
			return nil, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone, fmt.Errorf(
				"the network security group %s is in %s, not in %s, the VCN of the subnet %s in %s",
				r.ID, r.Network, sn.VCN, sn.ID, req.Zone))
		}
	}
	out := make([]vendors.ResourceRef, 0, len(refs)+1)
	for _, r := range refs {
		r.Kind, r.Region = KindNetworkSecurityGroups, req.Region
		out = append(out, r)
	}
	if net.Isolation == common.IsolationRun && req.RunKey != "" {
		raw, err := json.Marshal(GroupParams{VCN: sn.VCN, SelfReference: true})
		if err != nil {
			return nil, vendors.NewError(vendors.Fatal, codeSettings, err)
		}
		run, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindRunNetworkSecurityGroup, Key: class + "/" + sn.VCN + "/" + req.RunKey, Params: raw, Network: sn.VCN})
		if err != nil {
			return nil, err
		}
		for _, r := range run {
			r.Kind, r.Region = KindRunNetworkSecurityGroup, req.Region
			out = append(out, r)
		}
	}
	return out, nil
}

var (
	_ vendors.NodeOptions         = (*Driver)(nil)
	_ vendors.EpochReporter       = (*Driver)(nil)
	_ vendors.KeeperPriceConsumer = (*Driver)(nil)
	_ vendors.RegionPriced        = (*Driver)(nil)
)
