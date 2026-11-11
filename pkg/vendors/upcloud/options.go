/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	ucapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/upcloud"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Codes of the errors raised by the node options.
const (
	codeInvalidSettings = "InvalidSettings"
	codeIsolationRun    = "IsolationRunUnsupported"
)

// serialPattern is the device hint of a data disk: the virtio serial of an UpCloud
// disk is the first 20 hex digits of the storage UUID, unknown before the create,
// so the first-boot unit matches any serial and the size (the root disk has
// partitions and is skipped; equal-size disks rely on the unit's lock). The
// by-id name is unverified (spike S2).
const serialPattern = "*"

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskBytes implements vendors.NodeOptions: the size of the root disk of the
// settings, which the scheduler assumes as ephemeral storage; the least size
// (25 GB) when the settings name none (the plan's included storage is the
// default of a launch, RootDiskForType gives it per type).
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	st, err := decodeSettings(s)
	if err != nil {
		return int64(MinRootGB) * giB
	}
	if gb := rootOf(st).SizeGB; gb > 0 {
		return int64(gb) * giB
	}
	return int64(MinRootGB) * giB
}

// RootDiskForType implements vendors.NodeOptions: the root disk of a plan is the
// plan's included storage (at least 25 GB), or the NodeClass's size when
// storage.root sets one. Keeper's storage_size of the type is the included
// storage; a plan without one (Cloud Native) gets the least size.
func (d *Driver) RootDiskForType(s vendors.VendorSettings, it vendors.CatalogType) (int64, bool) {
	st, err := decodeSettings(s)
	if err == nil {
		if gb := rootOf(st).SizeGB; gb > 0 {
			return int64(gb) * giB, true
		}
	}
	return int64(max(int(it.LocalStorageGB), MinRootGB)) * giB, true
}

// Supported implements vendors.NodeOptions: the profiles of the NodeClass that
// the instance type can honour (ADR-017: nothing is degraded silently).
//   - storage: the root tier needs a Flatcar template of that tier in the zone
//     (known from the last image resolution; until it is known the tier is
//     offered and a launch that finds none is Unsupported), sizes within the
//     storage limits, and the storage devices of the server within the limit;
//   - placement: a server group is supported on every plan;
//   - network: UpCloud has no network profile blocks.
func (d *Driver) Supported(_ vendors.CatalogType, zone string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	tiers := d.zoneTiers(zone)
	for kind, byName := range p {
		for name, block := range byName {
			ok := false
			switch kind {
			case vendors.ProfileStorage:
				ok = supportsStorage(block, tiers)
			case vendors.ProfilePlacement:
				pl, err := decodePlacement(block)
				ok = err == nil && pl.ServerGroup != nil
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

func supportsStorage(block json.RawMessage, tiers []string) bool {
	st, err := decodeStorage(block)
	if err != nil {
		return false
	}
	if st.Root != nil {
		r := rootOf(ucapi.Settings{Storage: &st})
		if r.Tier != "" && len(tiers) > 0 && !slices.Contains(tiers, r.Tier) {
			return false
		}
		if r.SizeGB != 0 && (r.SizeGB < MinStorageGB || r.SizeGB > MaxStorageGB) {
			return false
		}
	}
	if 1+len(st.Data) > MaxStorageDevices {
		return false
	}
	for _, v := range dataOf(ucapi.Settings{Storage: &st}) {
		if v.SizeGB < MinStorageGB || v.SizeGB > MaxStorageGB {
			return false
		}
	}
	return true
}

// zoneTiers are the tiers of the templates the last image resolution found in
// the zone; nil when none ran yet.
func (d *Driver) zoneTiers(zone string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, t := range d.templates[zone] {
		if !slices.Contains(out, t.Tier) {
			out = append(out, t.Tier)
		}
	}
	return out
}

// hoursPerMonth converts a monthly price to an hourly one: UpCloud bills hourly
// and caps a month at 672 hours (the monthly price divided by 672, a node that
// lives a month pays the monthly price).
const hoursPerMonth = 672

// PriceDelta implements vendors.NodeOptions: the root disk where it is not
// included in the plan (a size above the plan's included storage, a tier other
// than its tier, or a plan with none) at Keeper's GB-month price of the tier,
// the data disks, and the public IPv4 address where it is billed apart (Cloud
// Native plans). The price is incomplete when a component is not priced.
func (d *Driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	st, err := decodeSettings(s)
	if err != nil {
		return 0, false
	}
	complete := true
	var total float64
	add := func(tier string, gb int) {
		price, ok := d.storagePrice(o.Region, tier)
		if !ok {
			complete = false
			return
		}
		total += float64(gb) * price / hoursPerMonth
	}
	root := rootOf(st)
	includedGB, includedTier := int(o.Type.LocalStorageGB), ""
	if p, ok := d.knownPlans()[o.APIReference]; ok {
		includedGB, includedTier = p.StorageGB, p.Tier
	}
	rootGB := root.SizeGB
	if rootGB == 0 {
		rootGB = max(includedGB, MinRootGB)
	}
	switch {
	case includedGB > 0 && rootGB <= includedGB && (root.Tier == "" || root.Tier == includedTier):
		// The included disk, free.
	case includedGB > 0 && root.Tier != "" && includedTier == "":
		// The tier of the included disk is not known yet: the price may be free or full.
		complete = false
	default:
		tier := root.Tier
		if tier == "" {
			tier = includedTier
		}
		if tier == "" {
			tier = string(ucapi.TierMaxIOPS)
		}
		add(tier, rootGB)
	}
	for _, v := range dataOf(st) {
		add(v.Tier, v.SizeGB)
	}
	if boolOr(s.Networking.PublicIPv4, true) && FamilyOf(o.APIReference) == FamilyCloudNative {
		if p, ok := d.ipv4Price(o.Region); ok {
			total += p
		} else {
			complete = false
		}
	}
	return total, complete
}

func (d *Driver) storagePrice(region, tier string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.StoragePrice(region, tier)
}

func (d *Driver) ipv4Price(region string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.IPv4Price(region)
}

// resourceCfg is how the NodeClass provides the kinds the launch may need.
type resourceCfg struct {
	networks, firewalls *common.Resource
}

// classResources reads the resources configuration of the NodeClass named by the
// tags: the settings of a launch do not carry it, and whether a node gets a
// private interface depends on it (there is no vendor default network). Without
// a Kubernetes reader or the NodeClass the configuration is empty.
func (d *Driver) classResources(ctx context.Context, class string) (resourceCfg, error) {
	if d.cfg.Reader == nil || class == "" {
		return resourceCfg{}, nil
	}
	var nc v1alpha1.SCNodeClass
	if err := d.cfg.Reader.Get(ctx, client.ObjectKey{Name: class}, &nc); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return resourceCfg{}, nil
		}
		return resourceCfg{}, vendors.NewError(vendors.Retryable, "ReadNodeClass", fmt.Errorf(
			"reading the NodeClass %s: %w", class, err))
	}
	cfgs, err := options.ResourceConfig(&nc, Vendor)
	if err != nil {
		return resourceCfg{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	var out resourceCfg
	if c, ok := cfgs[string(KindNetworks)]; ok {
		out.networks = &c
	}
	if c, ok := cfgs[string(KindFirewalls)]; ok {
		out.firewalls = &c
	}
	return out, nil
}

func (r resourceCfg) wantsNetwork() bool {
	return r.networks != nil && !r.networks.Forbid
}

func (r resourceCfg) wantsFirewall() bool {
	return r.firewalls != nil && !r.firewalls.Forbid
}

// FirewallParams are the create parameters of a managed ruleset: the inbound
// rules of the NodeClass and, with isolation nodeclass, the network's range.
type FirewallParams struct {
	Inbound []common.InboundRule `json:"inbound,omitempty"`
	// CIDR is the private network's range: the nodes of the NodeClass reach each
	// other inside it.
	CIDR string `json:"cidr,omitempty"`
}

// ServerGroupParams are the create parameters of a managed server group.
type ServerGroupParams struct {
	AntiAffinity string `json:"antiAffinity,omitempty"`
}

// Prepare implements vendors.NodeOptions: it makes the resources of the launch
// ready through the resource provider (the engine decides whether a resource is
// brought, discovered or managed): the SDN network and the ruleset where the
// NodeClass configures them or isolates by NodeClass, the server group of the
// placement profile, and the device hints of the data disks for the first-boot
// unit. An error of the provider is returned as it is: a ResourceMissingInZone
// must reach the attempt loop with its code.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	net := req.Settings.Networking
	if net.Isolation == common.IsolationRun && req.RunKey != "" {
		return nil, vendors.NewError(vendors.Unsupported, codeIsolationRun, fmt.Errorf(
			"isolation run needs one SDN network per run key, which UpCloud nodes do not get in this version "+
				"(account network limit, create latency); use isolation nodeclass or none"))
	}
	cfg, err := d.classResources(ctx, req.Tags[vendors.TagNodeClass])
	if err != nil {
		return nil, err
	}
	prep := &vendors.Prepared{}

	var network *vendors.ResourceRef
	if cfg.wantsNetwork() || net.Isolation == common.IsolationNodeClass {
		refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindNetworks, Zone: req.Zone, Settings: &req.Settings,
		})
		if err != nil {
			return nil, err
		}
		if len(refs) == 0 {
			return nil, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
				fmt.Errorf("no SDN network for %s", req.Zone))
		}
		network = &refs[0]
		prep.Resources = append(prep.Resources, refs[0])
	}
	if network != nil && (cfg.wantsFirewall() || net.Isolation == common.IsolationNodeClass) {
		params := FirewallParams{}
		if net.Inbound != nil {
			params.Inbound = net.Inbound.Rules
		}
		if net.Isolation == common.IsolationNodeClass {
			cidr, err := d.networkCIDR(ctx, network.ID)
			if err != nil {
				return nil, err
			}
			params.CIDR = cidr
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
		refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindFirewalls, Zone: req.Zone, Key: req.Tags[vendors.TagNodeClass], Params: raw, Settings: &req.Settings,
		})
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			prep.Resources = append(prep.Resources, refs[0])
		}
	}

	if g := pl.ServerGroup; g != nil {
		ref, err := d.requireServerGroup(ctx, req, g)
		if err != nil {
			return nil, err
		}
		prep.Resources = append(prep.Resources, ref)
	}

	for _, v := range dataOf(st) {
		prep.Devices = append(prep.Devices, vendors.DeviceHint{
			Name: v.Name, Filesystem: filesystemOf(st, v.Name), MountPath: mountPathOf(st, v.Name),
			SerialPattern: serialPattern, SizeBytes: int64(v.SizeGB) * giB,
		})
	}
	return prep, nil
}

func mountPathOf(st ucapi.Settings, name string) string {
	if st.Storage != nil {
		for _, d := range st.Storage.Data {
			if d.Name == name {
				return d.MountPath
			}
		}
	}
	return ""
}

// requireServerGroup returns the server group of the profile: a group by title
// (use) or one created per profile and run key (a profile that creates groups
// needs a run key).
func (d *Driver) requireServerGroup(ctx context.Context, req vendors.PrepareRequest, g *ucapi.ServerGroup,
) (vendors.ResourceRef, error) {
	profile := req.Settings.Profiles[vendors.ProfilePlacement]
	rr := vendors.ResourceRequest{Kind: KindServerGroups, Zone: req.Zone}
	if g.Use != nil {
		rr.Use = &vendors.UseRef{Name: g.Use.Name}
	} else {
		if req.RunKey == "" {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
				"the placement profile %q creates server groups and needs a run key (the placement-group label)", profile))
		}
		rr.Key = profile + "/" + req.RunKey
		policy := string(g.AntiAffinity)
		if policy == "" {
			policy = string(ucapi.AntiAffinityStrict)
		}
		params, err := json.Marshal(ServerGroupParams{AntiAffinity: policy})
		if err != nil {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
		rr.Params = params
	}
	refs, err := req.Resources.Require(ctx, rr)
	if err != nil {
		return vendors.ResourceRef{}, err
	}
	switch {
	case len(refs) == 0:
		return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("no server group for the profile %q", profile))
	case len(refs) > 1:
		return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
			"the name of the server group matches %d groups; it must match one", len(refs)))
	}
	return refs[0], nil
}

// networkCIDR is the first IPv4 range of a network.
func (d *Driver) networkCIDR(ctx context.Context, uuid string) (string, error) {
	n, err := GetNetwork(ctx, d.api, uuid)
	if err != nil {
		return "", err
	}
	for _, in := range n.IPNetworks {
		if in.Family == "" || in.Family == "IPv4" {
			return in.Address, nil
		}
	}
	return "", vendors.Errorf(vendors.Fatal, "the network %s has no IPv4 range", uuid)
}
