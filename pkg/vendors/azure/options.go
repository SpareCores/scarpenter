/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const giB = int64(1) << 30

// defaultSettings are the built-in defaults (00a section 5.3, layer 1): a
// Premium_LRS root disk of 64 GiB with read and write caching. Platform-managed
// encryption is always on in Azure; accelerated networking is auto (the network
// profile); the public IPv4 address is the generic default.
const defaultSettings = `{"storage":{"root":{"storageAccountType":"Premium_LRS","diskSizeGB":64,"caching":"ReadWrite"}}}`

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskBytes implements vendors.NodeOptions: the size of the OS disk, which
// the scheduler assumes as ephemeral storage.
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	st, err := DecodeSettings(s)
	if err != nil {
		return int64(DefaultRootDiskGB) * giB
	}
	return st.RootDiskBytes()
}

// RootDiskForType implements vendors.NodeOptions: the OS disk is a managed disk
// of the settings, not of the size.
func (d *Driver) RootDiskForType(vendors.VendorSettings, vendors.CatalogType) (int64, bool) {
	return 0, false
}

// Supported implements vendors.NodeOptions: the profiles of the NodeClass that
// the size can honour (ADR-017: nothing is degraded silently).
//   - storage: Premium disks need the PremiumIO capability, an ephemeral OS disk
//     EphemeralOSDiskSupported, and the number of data disks is bounded by
//     MaxDataDiskCount; the local disks are not used in v1;
//   - network: accelerated networking true needs the capability (auto adapts);
//   - placement: a proximity placement group of a strategy, or one by name, is
//     accepted (which sizes a group can take cannot be known in advance: an
//     OverconstrainedAllocationRequest makes the offering unavailable for the
//     unavailable TTL).
//
// The facts come from the SKU table, read at runtime; until they are known a
// profile that depends on them is not offered, and the epoch of the node options
// changes when they arrive. The default profile is always valid: a size without
// premium storage fails the default root disk at launch (Unsupported) and the
// NodeClass sets StandardSSD_LRS to include it.
func (d *Driver) Supported(it vendors.CatalogType, _ string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	sku, known := d.skuFacts(it.APIReference)
	for kind, byName := range p {
		for name, block := range byName {
			ok := false
			switch kind {
			case vendors.ProfileStorage:
				ok = supportsStorage(block, sku, known)
			case vendors.ProfileNetwork:
				ok = supportsNetwork(block, sku, known)
			case vendors.ProfilePlacement:
				_, err := decodePlacement(block)
				ok = err == nil
			}
			if ok {
				out[kind] = append(out[kind], name)
			}
		}
	}
	for _, names := range out {
		slices.Sort(names)
	}
	return out
}

func supportsStorage(block json.RawMessage, sku SKU, known bool) bool {
	st, err := decodeStorage(block)
	if err != nil {
		return false
	}
	if st.LocalStorage != nil && st.LocalStorage.Policy != "" && st.LocalStorage.Policy != "ignore" {
		return false
	}
	if !known {
		return len(storageTypes(st)) == 0 && (st.Root == nil || st.Root.Ephemeral == nil) && len(st.Data) == 0
	}
	if slices.ContainsFunc(storageTypes(st), IsPremium) && !sku.PremiumIO {
		return false
	}
	if st.Root != nil && st.Root.Ephemeral != nil && !sku.EphemeralOSDisk {
		return false
	}
	return sku.MaxDataDisks == 0 || len(st.Data) <= sku.MaxDataDisks
}

// storageTypes lists the disk SKUs a storage block names (the root's, then the
// data disks'; a data disk without one is the default Premium_LRS).
func storageTypes(st StorageSettings) []string {
	var out []string
	if st.Root != nil && st.Root.StorageAccountType != "" {
		out = append(out, st.Root.StorageAccountType)
	}
	for _, d := range st.Data {
		t := d.StorageAccountType
		if t == "" {
			t = DefaultStorageAccountType
		}
		out = append(out, t)
	}
	return out
}

func supportsNetwork(block json.RawMessage, sku SKU, known bool) bool {
	mode, err := decodeNetworkProfile(block)
	if err != nil {
		return false
	}
	if mode == settingTrue {
		return known && sku.AcceleratedNetwork
	}
	return mode == "auto" || mode == "false"
}

// diskTiers are the sizes managed disks are billed at (a 64 GiB disk is a P6,
// an E6 or an S6): the price of a disk rounds its size up to the next tier. The
// tiers are those of the Azure pricing page, not yet checked against a bill.
var diskTiers = []int32{4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32767}

func tierOf(gb int32) int32 {
	for _, t := range diskTiers {
		if gb <= t {
			return t
		}
	}
	return gb
}

// PriceDelta implements vendors.NodeOptions: the managed disks at Keeper's
// GB-month price per region and SKU divided by 730 (the size rounded up to its
// billing tier), plus the public IPv4 address where it is billed. The price is
// incomplete when a component is not priced: a disk price the source lacks, the
// transaction costs of the Standard SKUs (HDD and SSD), and an ephemeral OS disk
// (which costs nothing here but whose cost is part of the size).
func (d *Driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	st, err := DecodeSettings(s)
	if err != nil {
		return 0, false
	}
	complete := true
	var total float64
	disk := func(typ string, gb int32) {
		price, ok := d.storagePrice(o.Region, typ)
		if !ok {
			complete = false
		} else {
			total += float64(tierOf(gb)) * price / hoursPerMonth
		}
		if !IsPremium(typ) {
			complete = false // transaction costs are not in the price
		}
	}
	if root := st.Root(); root.Ephemeral == nil {
		disk(root.StorageAccountType, root.DiskSizeGB)
	}
	for _, dd := range st.DataDisks() {
		disk(dd.StorageAccountType, dd.DiskSizeGB)
	}
	if boolOr(s.Networking.PublicIPv4, true) {
		if p, ok := d.ipv4Price(o.Region); ok {
			total += p
		} else {
			complete = false
		}
	}
	return total, complete
}

func (d *Driver) storagePrice(region, typ string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.StoragePrice(region, typ)
}

func (d *Driver) ipv4Price(region string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.IPv4Price(region)
}

// onlyRef returns the single resource of a Require, or the error of none or
// several (a choice the NodeClass must make: a NIC takes one group).
func onlyRef(refs []vendors.ResourceRef, what string) (vendors.ResourceRef, error) {
	switch len(refs) {
	case 1:
		return refs[0], nil
	case 0:
		return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("no %s", what))
	}
	return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
		"the selector of the %s matches %d resources; it must match one", what, len(refs)))
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // the parameter types are plain structs
	}
	return raw
}

// Prepare implements vendors.NodeOptions: it makes the resources of the launch
// ready through the resource provider (the engine decides whether a resource is
// brought, discovered or managed; Azure has no default): the resource group, the
// subnet, the network security group (and, for isolation run, that of the run),
// the proximity placement group of the profile and, for the data disks, the
// device hints for the first-boot unit. The public IP and the NIC are made by the
// launch itself, per NodeClaim, under names that carry the hash of their
// parameters. An error of the provider is returned as it is: a
// ResourceMissingInZone must reach the attempt loop with its code.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	st, err := DecodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	prep := &vendors.Prepared{}
	rgRefs, err := req.Resources.Require(ctx, vendors.ResourceRequest{Kind: KindResourceGroups, Settings: &req.Settings})
	if err != nil {
		return nil, err
	}
	rg, err := onlyRef(rgRefs, "resource group")
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, rg)

	subRefs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindSubnets, Params: mustJSON(ResourceParams{ResourceGroup: rg.ID}), Settings: &req.Settings,
	})
	if err != nil {
		return nil, err
	}
	sn, err := d.chooseSubnet(ctx, subRefs)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, sn)

	groups, err := d.requireGroups(ctx, req, rg.ID)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, groups...)

	if g := pl.ProximityPlacementGroup; g != nil {
		ref, err := d.requirePlacementGroup(ctx, req, rg.ID, g.Strategy, g.Use != nil, func() string { return g.Use.Name })
		if err != nil {
			return nil, err
		}
		prep.Resources = append(prep.Resources, ref)
	}
	prep.Devices = deviceHints(st)
	return prep, nil
}

// requireGroups returns the network security group of the launch: the one the
// NodeClass brings (use: it decides alone, a NIC takes one), else the group of
// the NodeClass, or, for isolation run with a run key, the group of the run (its
// own, with its own application security group: the allow of the nodes of one run
// names that group). The NIC joins the application security group of a managed
// group, which is what the allow rule matches.
func (d *Driver) requireGroups(ctx context.Context, req vendors.PrepareRequest, rg string) ([]vendors.ResourceRef, error) {
	net := req.Settings.Networking
	class := req.Tags[vendors.TagNodeClass]
	var inbound []common.InboundRule
	if net.Inbound != nil {
		inbound = net.Inbound.Rules
	}
	// Isolation run without a run key has no run to keep apart: the nodes of the
	// NodeClass are one implicit run and reach each other (a VPC-routed CNI of the
	// direct template needs it).
	self := net.Isolation == common.IsolationNodeClass || (net.Isolation == common.IsolationRun && req.RunKey == "")
	refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindSecurityGroups, Key: class, Settings: &req.Settings,
		Params: mustJSON(ResourceParams{ResourceGroup: rg, SelfReference: self, Inbound: inbound}),
	})
	if err != nil {
		return nil, err
	}
	nsg, err := onlyRef(refs, "network security group")
	if err != nil {
		return nil, err
	}
	if !nsg.Managed {
		d.warnBrought(class, req.Region, net.Isolation)
		return []vendors.ResourceRef{nsg}, nil
	}
	if net.Isolation == common.IsolationRun && req.RunKey != "" {
		run, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindRunSecurityGroups, Key: class + "/" + req.RunKey,
			Params: mustJSON(ResourceParams{ResourceGroup: rg, SelfReference: true, Inbound: inbound}),
		})
		if err != nil {
			return nil, err
		}
		if nsg, err = onlyRef(run, "run network security group"); err != nil {
			return nil, err
		}
	}
	out := []vendors.ResourceRef{nsg}
	if self || (net.Isolation == common.IsolationRun && req.RunKey != "") {
		out = append(out, vendors.ResourceRef{
			Kind: KindAppSecGroups, Region: req.Region, ID: ASGIDOf(nsg.ID), Name: ASGName(nsg.Name), Managed: true,
		})
	}
	return out, nil
}

// warnBrought says once per NodeClass and region that a brought network security
// group decides alone: a NIC takes one group, so the run's application security
// group grants nothing unless the brought group has a rule for it, and
// networking.isolation has no effect (open question 11 of plan 17).
func (d *Driver) warnBrought(class, region string, isolation common.Isolation) {
	if isolation == common.IsolationNone {
		return
	}
	key := class + "/" + region
	d.mu.Lock()
	seen := d.warned[key]
	d.warned[key] = true
	d.mu.Unlock()
	if !seen {
		d.log.Info("networking.isolation has no effect on a brought network security group: it decides alone",
			"nodeClass", class, "region", region, "isolation", isolation)
	}
}

// requirePlacementGroup returns the proximity placement group of the profile: a
// group by name (use) or one created per profile and run key (a profile that
// creates groups needs a run key).
func (d *Driver) requirePlacementGroup(
	ctx context.Context, req vendors.PrepareRequest, rg, strategy string, byName bool, name func() string,
) (vendors.ResourceRef, error) {
	profile := req.Settings.Profiles[vendors.ProfilePlacement]
	rr := vendors.ResourceRequest{Kind: KindPlacement}
	if byName {
		rr.Use = &vendors.UseRef{Name: name()}
	} else {
		if req.RunKey == "" {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
				"the placement profile %q creates proximity placement groups and needs a run key (the placement-group label)",
				profile))
		}
		rr.Key = profile + "/" + req.RunKey
		rr.Params = mustJSON(ResourceParams{ResourceGroup: rg, Strategy: strategy})
	}
	refs, err := req.Resources.Require(ctx, rr)
	if err != nil {
		return vendors.ResourceRef{}, err
	}
	return onlyRef(refs, "proximity placement group for the profile "+profile)
}

// chooseSubnet picks the subnet of the launch among the resolved ones: the one
// with the most free addresses (a /20 holds 4091 nodes; a brought virtual network
// may have several subnets, which a subnet is full in is read from its IP
// configurations).
func (d *Driver) chooseSubnet(ctx context.Context, refs []vendors.ResourceRef) (vendors.ResourceRef, error) {
	if len(refs) <= 1 {
		return onlyRef(refs, "subnet")
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	vnets, err := d.clients.VNets.ListAll(cctx)
	if err != nil {
		return vendors.ResourceRef{}, classify(err, errContext{})
	}
	free := map[string]int{}
	for _, v := range vnets {
		if v.Properties == nil {
			continue
		}
		for _, sn := range v.Properties.Subnets {
			if sn != nil && sn.Properties != nil {
				free[strings.ToLower(deref(sn.ID))] = freeAddresses(*sn)
			}
		}
	}
	best := refs[0]
	for _, r := range refs[1:] {
		if free[strings.ToLower(r.ID)] > free[strings.ToLower(best.ID)] {
			best = r
		}
	}
	if f, ok := free[strings.ToLower(best.ID)]; ok && f <= 0 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissingInZone,
			fmt.Errorf("the subnets of the NodeClass have no free address"))
	}
	return best, nil
}

// freeAddresses is the number of addresses of a subnet that no NIC has: its
// range less the five Azure reserves and the IP configurations in it.
func freeAddresses(sn armnetwork.Subnet) int {
	p := sn.Properties
	prefix := deref(p.AddressPrefix)
	if prefix == "" && len(p.AddressPrefixes) > 0 {
		prefix = deref(p.AddressPrefixes[0])
	}
	pf, err := netip.ParsePrefix(prefix)
	if err != nil || !pf.Addr().Is4() {
		return 0
	}
	return 1<<(32-pf.Bits()) - 5 - len(p.IPConfigurations)
}
