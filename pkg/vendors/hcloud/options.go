/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	hcapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MaxVolumes is the number of volumes one server can have attached.
const MaxVolumes = 16

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskBytes implements vendors.NodeOptions: the root disk belongs to the
// server type and cannot be configured, so there is no per-NodeClass answer.
func (d *Driver) RootDiskBytes(vendors.VendorSettings) int64 { return 0 }

// RootDiskForType implements vendors.NodeOptions: the root disk of a type in
// bytes (the API's disk is GB, decimal), from the cached server type table; the
// catalog's storage size answers before the table is loaded.
func (d *Driver) RootDiskForType(_ vendors.VendorSettings, it vendors.CatalogType) (int64, bool) {
	if d.p == nil {
		return 0, false
	}
	return d.p.cachedRootDisk(it.APIReference)
}

// Supported implements vendors.NodeOptions: a storage profile is valid where
// its volumes fit the limits and the location sells volumes; a placement
// profile everywhere. There are no network profiles.
func (d *Driver) Supported(_ vendors.CatalogType, zone string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	for kind, byName := range p {
		for name, block := range byName {
			ok := false
			switch kind {
			case vendors.ProfileStorage:
				ok = d.supportsStorage(block, zone)
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

func (d *Driver) supportsStorage(block json.RawMessage, zone string) bool {
	st, err := decodeStorage(block)
	if err != nil || len(st.Data) > MaxVolumes {
		return false
	}
	for _, dv := range st.Data {
		if n := sizeGB(dv.Size); n < MinVolumeGB || n > MaxVolumeGB {
			return false
		}
	}
	if len(st.Data) > 0 && d.prices != nil && pricesLoaded(d.prices) {
		_, ok := d.prices.VolumePrice(zone)
		return ok
	}
	return true
}

// pricesLoaded says whether the source's volume prices are known yet: a source
// that is still loading says nothing about a location.
func pricesLoaded(p PriceSource) bool {
	if l, ok := p.(interface{ loadedOnce() bool }); ok {
		return l.loadedOnce()
	}
	return true
}

// PriceDelta implements vendors.NodeOptions: the volumes (GB-month over 730
// hours, Keeper's block price of the location) and the Primary IPv4 (a
// constant). Placement groups, firewalls and networks are free; traffic over the
// included amount is not priced.
func (d *Driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	st, err := decodeSettings(s)
	if err != nil {
		return 0, false
	}
	complete := true
	var total float64
	location := o.Zone
	if location == "" {
		location = o.Region
	}
	if vols := dataVolumes(st); len(vols) > 0 {
		price, ok := 0.0, false
		if d.prices != nil {
			price, ok = d.prices.VolumePrice(location)
		}
		if !ok {
			complete = false
		} else {
			for _, dv := range vols {
				total += float64(sizeGB(dv.Size)) * price / hoursPerMonth
			}
		}
	}
	if boolOr(s.Networking.PublicIPv4, true) {
		price, ok := 0.0, false
		if d.prices != nil {
			price, ok = d.prices.IPv4Price()
		}
		if !ok {
			complete = false
		} else {
			total += price
		}
	}
	return total, complete
}

// bringsNetwork says whether the NodeClass has a networks `use`. The resolved
// settings do not carry the resource configuration (the engine reads it), and an
// optional kind that nothing configures must not be required (the engine would
// try to create it), so the NodeClass is read here, from the cache. Without a
// client (tests of the lower layers) no network is brought.
func (d *Driver) bringsNetwork(ctx context.Context, nodeClass string) bool {
	var r client.Reader = d.cfg.Reader
	if d.cfg.Client != nil {
		r = d.cfg.Client
	}
	if r == nil || nodeClass == "" {
		return false
	}
	nc := &v1alpha1.SCNodeClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeClass}, nc); err != nil {
		d.log.Error(err, "reading the NodeClass to see whether it brings a network", "nodeclass", nodeClass)
		return false
	}
	cfgs, err := options.ResourceConfig(nc, Name)
	if err != nil {
		return false
	}
	return cfgs[string(KindNetworks)].Use != nil
}

// FirewallParams are the create parameters of a managed firewall: the inbound
// rules of networking.inbound (outbound is open).
type FirewallParams struct {
	Rules []common.InboundRule `json:"rules,omitempty"`
}

// PlacementGroupParams are the create parameters of a managed placement group.
type PlacementGroupParams struct {
	Type string `json:"type"`
}

// VolumeParams are the create parameters of a managed volume.
type VolumeParams struct {
	SizeGB int `json:"sizeGB"`
}

// Prepare implements vendors.NodeOptions: it requires the resources of the
// launch through the engine (network, firewalls, placement group) and creates
// the data volumes, one per storage.data entry, in the launch's location. It is
// idempotent: the names are deterministic.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	st, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	prep := &vendors.Prepared{}

	// A network is optional and never created: only a `use` brings one.
	if d.bringsNetwork(ctx, req.Tags[vendors.TagNodeClass]) {
		refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{Kind: KindNetworks, Settings: &req.Settings})
		if err != nil {
			return nil, err
		}
		if len(refs) != 1 {
			return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
				"the networks selector matches %d networks in %s; a server joins one", len(refs), req.Zone))
		}
		prep.Resources = append(prep.Resources, refs[0])
	}

	// The firewall: brought by `use`, discovered, or the managed deny-inbound one.
	params := FirewallParams{}
	if in := req.Settings.Networking.Inbound; in != nil {
		params.Rules = in.Rules
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	fws, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindFirewalls, Key: req.Tags[vendors.TagNodeClass], Params: raw, Settings: &req.Settings,
	})
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, fws...)

	if g := pl.PlacementGroup; g != nil {
		ref, err := d.requirePlacementGroup(ctx, req, g)
		if err != nil {
			return nil, err
		}
		prep.Resources = append(prep.Resources, ref)
	}

	claim := req.Tags[vendors.TagNodeClaim]
	for _, dv := range dataVolumes(st) {
		if claim == "" {
			return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
				"a launch with data volumes needs the NodeClaim tag to name them"))
		}
		raw, err := json.Marshal(VolumeParams{SizeGB: sizeGB(dv.Size)})
		if err != nil {
			return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
		}
		refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindVolumes, Zone: req.Zone, Key: claim + "/" + dv.Name, Params: raw, Settings: &req.Settings,
		})
		if err != nil {
			return nil, err
		}
		if len(refs) != 1 {
			return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
				"preparing the volume %s returned %d resources", dv.Name, len(refs)))
		}
		id, err := parseID(refs[0].ID)
		if err != nil {
			return nil, err
		}
		fs := string(dv.Filesystem)
		if fs == "" {
			fs = string(common.FilesystemExt4)
		}
		prep.Resources = append(prep.Resources, refs[0])
		prep.Devices = append(prep.Devices, vendors.DeviceHint{
			Name: dv.Name, Filesystem: fs, MountPath: dv.MountPath, ByID: VolumeByIDPath(id),
			SizeBytes: int64(sizeGB(dv.Size)) * gb,
		})
	}
	return prep, nil
}

func (d *Driver) requirePlacementGroup(
	ctx context.Context, req vendors.PrepareRequest, g *hcapi.PlacementGroup,
) (vendors.ResourceRef, error) {
	profile := req.Settings.Profiles[vendors.ProfilePlacement]
	rr := vendors.ResourceRequest{Kind: KindPlacementGroups}
	if g.Use != nil {
		rr.Use = &vendors.UseRef{Name: g.Use.Name}
	} else {
		if req.RunKey == "" {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
				"the placement profile %q creates placement groups and needs a run key (the placement-group label)", profile))
		}
		rr.Key = profile + "/" + req.RunKey
		params, err := json.Marshal(PlacementGroupParams{Type: string(g.Type)})
		if err != nil {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
		}
		rr.Params = params
	}
	refs, err := req.Resources.Require(ctx, rr)
	if err != nil {
		return vendors.ResourceRef{}, err
	}
	if len(refs) == 0 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing,
			fmt.Errorf("no placement group for the profile %q", profile))
	}
	if len(refs) > 1 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
			"the name of the placement group matches %d groups; it must match one", len(refs)))
	}
	return refs[0], nil
}

func parseID(s string) (int64, error) {
	id, err := ServerID(s)
	if err != nil {
		return 0, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf("%q is not a Hetzner ID", s))
	}
	return id, nil
}
