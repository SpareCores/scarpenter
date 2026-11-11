/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	gcpapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// localSSDPattern matches the bundled local SSDs in /dev/disk/by-id: the udev
// rules of google-guest-configs name SCSI local disks google-local-ssd-<n> and
// NVMe ones google-local-nvme-ssd-<n> (UNVERIFIED, spike).
const localSSDPattern = "google-local-*"

// hyperdiskIncludedIOPS and hyperdiskIncludedThroughput: a Hyperdisk Balanced
// volume includes a baseline of IOPS and throughput; only what is provisioned
// above it costs, which Keeper's GB-month price does not cover.
const (
	hyperdiskBalancedBaselineIOPS       = 3000
	hyperdiskBalancedBaselineThroughput = 140
)

// Supported implements vendors.NodeOptions: the profiles of the NodeClass that the
// machine type can honour (ADR-017: nothing is degraded silently). The series
// facts are the tables of series.go.
//   - storage: a pinned root disk type must be one the series boots from, data
//     disks one it attaches; local storage (mount or raid0) needs a type that
//     bundles local SSDs;
//   - network: Tier 1 needs a series and size that offer it;
//   - placement: compact placement needs a series that supports it; spread and
//     a policy by name (use) are accepted for every type.
func (d *Driver) Supported(it vendors.CatalogType, _ string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	for kind, byName := range p {
		for name, block := range byName {
			ok := false
			switch kind {
			case vendors.ProfileStorage:
				ok = supportsStorage(block, it)
			case vendors.ProfileNetwork:
				ok = supportsNetwork(block, it)
			case vendors.ProfilePlacement:
				ok = supportsPlacement(block, it)
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

func supportsStorage(block json.RawMessage, it vendors.CatalogType) bool {
	st, err := decodeStorage(block)
	if err != nil {
		return false
	}
	if st.Root != nil && st.Root.DiskType != "" && !CanBootFrom(it.APIReference, string(st.Root.DiskType)) {
		return false
	}
	for _, dd := range st.Data {
		if !CanAttach(it.APIReference, string(dd.DiskType)) {
			return false
		}
	}
	if ls := st.LocalStorage; ls != nil && ls.Policy != common.LocalStorageIgnore && it.LocalStorageCount == 0 {
		return false
	}
	return true
}

func supportsNetwork(block json.RawMessage, it vendors.CatalogType) bool {
	n, err := decodeNetwork(block)
	if err != nil {
		return false
	}
	if boolOr(n.Tier1, false) && (!inSeries(tier1Series, it.APIReference) || it.VCPUs < tier1MinVCPUs) {
		return false
	}
	return true
}

func supportsPlacement(block json.RawMessage, it vendors.CatalogType) bool {
	p, err := decodePlacement(block)
	if err != nil {
		return false
	}
	if rp := p.ResourcePolicy; rp != nil && rp.GroupPlacement != nil {
		return inSeries(compactSeries, it.APIReference)
	}
	return true
}

// PriceDelta implements vendors.NodeOptions: the disks at Keeper's GB-month price
// of the region and disk type divided by 730, plus the external IPv4 address.
// The price is incomplete when a component is not priced: provisioned Hyperdisk
// IOPS or throughput above the baseline, Tier 1 networking, and any price the
// source lacks.
func (d *Driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	st, err := decodeSettings(s)
	if err != nil {
		return 0, false
	}
	complete := true
	var total float64
	disk := func(dk Disk) {
		price, ok := d.storagePrice(o.Region, dk.Type)
		if !ok {
			complete = false
		} else {
			total += float64(dk.SizeGb) * price / hoursPerMonth
		}
		if dk.ProvisionedIops > hyperdiskBalancedBaselineIOPS ||
			dk.ProvisionedThroughput > hyperdiskBalancedBaselineThroughput {
			complete = false
		}
	}
	disk(rootDisk(st, o.APIReference))
	for _, dd := range dataDisks(st) {
		disk(dataDisk(dd))
	}
	if boolOr(s.Networking.PublicIPv4, true) {
		if p, ok := d.ipv4Price(o.Region); ok {
			total += p
		} else {
			complete = false
		}
	}
	if n, err := decodeNetwork(s.Network); err == nil && boolOr(n.Tier1, false) {
		complete = false
	}
	return total, complete
}

func (d *Driver) storagePrice(region, diskType string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.StoragePrice(region, diskType)
}

func (d *Driver) ipv4Price(region string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.IPv4Price(region)
}

// FirewallParams are the create parameters of the firewall kinds: the network
// the rules belong to, whether the nodes of the tag may reach each other, and
// the inbound rules of the NodeClass.
type FirewallParams struct {
	Network       string               `json:"network"`
	SelfReference bool                 `json:"selfReference,omitempty"`
	Inbound       []common.InboundRule `json:"inbound,omitempty"`
}

// PolicyParams are the create parameters of a placement resource policy.
type PolicyParams struct {
	// Collocated is a compact placement policy; AvailabilityDomainCount a spread
	// policy (exactly one is set).
	Collocated              bool  `json:"collocated,omitempty"`
	AvailabilityDomainCount int32 `json:"availabilityDomainCount,omitempty"`
}

// Prepare implements vendors.NodeOptions: it makes the resources of the launch
// ready through the resource provider (the engine decides whether a resource is
// brought, the project's default or managed): the subnet, the firewall tags, the
// placement policy of the profile and, for the disks, the device hints for the
// first-boot unit. An error of the provider is returned as it is: a
// ResourceMissingInZone must reach the attempt loop with its code.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	prep := &vendors.Prepared{}

	// The pool of a create override is part of the identity of the managed subnet;
	// without one the subnet is the cluster's, shared by every NodeClass.
	var subnetParams json.RawMessage
	if pool := poolOf(st); pool != "" {
		if subnetParams, err = json.Marshal(SubnetParams{Pool: pool}); err != nil {
			return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
	}
	subnetRefs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindSubnets, Params: subnetParams, Settings: &req.Settings})
	if err != nil {
		return nil, err
	}
	wantIPv6 := boolOr(req.Settings.Networking.PublicIPv6, false)
	sn, err := d.chooseSubnet(ctx, req.Region, subnetRefs, wantIPv6)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, sn)

	fws, err := d.requireFirewalls(ctx, req, sn)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, fws...)

	if rp := pl.ResourcePolicy; rp != nil {
		ref, err := d.requirePolicy(ctx, req, rp)
		if err != nil {
			return nil, err
		}
		prep.Resources = append(prep.Resources, ref)
	}

	for _, dd := range dataDisks(st) {
		fs := string(dd.Filesystem)
		if fs == "" {
			fs = string(common.FilesystemExt4)
		}
		prep.Devices = append(prep.Devices, vendors.DeviceHint{
			Name: dd.Name, Filesystem: fs, MountPath: dd.MountPath, ByID: DataDevicePath(dd.Name),
		})
	}
	if policy, mount, ok := localStorage(st); ok && policy != "" && policy != string(common.LocalStorageIgnore) {
		prep.Local = &vendors.LocalHint{
			Policy: policy, MountPath: mount, DevicePattern: localSSDPattern, Count: max(req.Type.LocalStorageCount, 1),
		}
	}
	return prep, nil
}

// requireFirewalls returns the network tags of the launch: those the NodeClass
// brings (use), or the tag of the NodeClass in the network of the subnet, and,
// for isolation run with a run key, the tag of the run.
func (d *Driver) requireFirewalls(ctx context.Context, req vendors.PrepareRequest, sn vendors.ResourceRef,
) ([]vendors.ResourceRef, error) {
	net := req.Settings.Networking
	class := req.Tags[vendors.TagNodeClass]
	params := FirewallParams{
		Network: sn.Network,
		// Isolation run without a run key has no run to keep apart: the nodes of
		// the NodeClass are one implicit run and reach each other.
		SelfReference: net.Isolation == common.IsolationNodeClass ||
			(net.Isolation == common.IsolationRun && req.RunKey == ""),
	}
	if net.Inbound != nil {
		params.Inbound = net.Inbound.Rules
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
	}
	refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindFirewalls, Key: class + "/" + sn.Network, Params: raw, Network: sn.Network, Settings: &req.Settings,
	})
	if err != nil {
		return nil, err
	}
	out := slices.Clone(refs)
	if net.Isolation == common.IsolationRun && req.RunKey != "" {
		raw, err := json.Marshal(FirewallParams{Network: sn.Network, SelfReference: true})
		if err != nil {
			return nil, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
		run, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindRunFirewalls, Key: class + "/" + sn.Network + "/" + req.RunKey, Params: raw,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, run...)
	}
	return out, nil
}

// requirePolicy returns the resource policy of the placement profile: a policy by
// name (use) or one created per profile and run key (a profile that creates
// policies needs a run key).
func (d *Driver) requirePolicy(ctx context.Context, req vendors.PrepareRequest, rp *gcpapi.ResourcePolicy,
) (vendors.ResourceRef, error) {
	profile := req.Settings.Profiles[vendors.ProfilePlacement]
	rr := vendors.ResourceRequest{Kind: KindResourcePolicies}
	if rp.Use != nil {
		rr.Use = &vendors.UseRef{Name: rp.Use.Name}
	} else {
		if req.RunKey == "" {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
				"the placement profile %q creates resource policies and needs a run key (the placement-group label)", profile))
		}
		rr.Key = profile + "/" + req.RunKey
		params := PolicyParams{Collocated: rp.GroupPlacement != nil}
		if rp.Spread != nil {
			params.AvailabilityDomainCount = rp.Spread.AvailabilityDomainCount
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, err)
		}
		rr.Params = raw
	}
	refs, err := req.Resources.Require(ctx, rr)
	if err != nil {
		return vendors.ResourceRef{}, err
	}
	if len(refs) == 0 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("no resource policy for the profile %q", profile))
	}
	if len(refs) > 1 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, codeInvalidSettings, fmt.Errorf(
			"the name of the resource policy matches %d policies; it must match one", len(refs)))
	}
	return refs[0], nil
}
