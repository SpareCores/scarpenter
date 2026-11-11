/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	ovhapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/ovh"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// defaultSettings are the built-in settings (layer 1): the flavor's local disk as
// the root, no extra volumes, public IPv4, a config drive next to the user data
// (spike S2 may change it).
const defaultSettings = `{"configDrive":true}`

// i1RootGB is the root disk of the storage-optimised i1 flavors: Keeper's
// storage_size of an i1 type adds up its local NVMe disks (1950 to 7650 GB), but
// the root is the 50 GB system disk (plan 18, "Node options").
const i1RootGB = 50

// dataDeviceSerial is the serial pattern of a data volume's device until spike S8
// says which bus Flatcar sees on OVH: virtio disks carry the first 20 characters of
// the volume UUID as the serial, the volume IDs exist only after the create call,
// so the hint is every disk of the volume's size (the unit locks equal sizes).
const dataDeviceSerial = "*"

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskBytes implements vendors.NodeOptions: the bootFromVolume size, or 0 for
// the flavor's local disk (its size depends on the type, RootDiskForType).
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	st, err := ParseSettings(s.Raw)
	if err != nil {
		return 0
	}
	return st.RootDiskBytes()
}

// RootDiskForType implements vendors.NodeOptions: the root disk of the type when
// the settings do not boot from a volume: the 50 GB system disk of the i1 flavors;
// for the others the catalog's own storage size (Keeper's storage_size, the
// flavor's disk) is the root, so ok is false.
func (d *Driver) RootDiskForType(s vendors.VendorSettings, it vendors.CatalogType) (int64, bool) {
	if b := d.RootDiskBytes(s); b > 0 {
		return b, true
	}
	if Family(it.APIReference) == "i1" {
		return int64(i1RootGB) * 1e9, true
	}
	return 0, false
}

func decodeStorage(raw json.RawMessage) (ovhapi.Storage, error) {
	var s ovhapi.Storage
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("decoding the storage profile: %w", err)
		}
	}
	return s, nil
}

func decodePlacement(raw json.RawMessage) (ovhapi.Placement, error) {
	var p ovhapi.Placement
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("decoding the placement profile: %w", err)
		}
	}
	return p, nil
}

// Supported implements vendors.NodeOptions: a profile is supported when nothing
// in it is unsupported on OVH for every type (localStorage, an invalid volume) .
func (d *Driver) Supported(_ vendors.CatalogType, _ string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	for kind, byName := range p {
		for name, block := range byName {
			ok := false
			switch kind {
			case vendors.ProfileStorage:
				ok = supportsStorage(block)
			case vendors.ProfilePlacement:
				ok = supportsPlacement(block)
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

func supportsStorage(block json.RawMessage) bool {
	st, err := decodeStorage(block)
	if err != nil || st.LocalStorage != nil {
		return false // localStorage is unsupported on OVH (ADR-017)
	}
	return len(validateStorage("", &st)) == 0
}

func supportsPlacement(block json.RawMessage) bool {
	p, err := decodePlacement(block)
	if err != nil {
		return false
	}
	return len(validatePlacement("", &p)) == 0
}

// PriceDelta implements vendors.NodeOptions: the Cinder volumes on top of the
// flavor price (the flavor's local disk is in the flavor's price), from Keeper's
// GB-month prices per region divided by 730. Incomplete where a price is
// missing; there is nothing else to price (the public address is free).
func (d *Driver) PriceDelta(s vendors.VendorSettings, o vendors.Offering) (float64, bool) {
	st, err := ParseSettings(s.Raw)
	if err != nil {
		return 0, false
	}
	complete := true
	var total float64
	vol := func(volumeType string, gib int64) {
		price, ok := d.storagePrice(o.Region, volumeType)
		if !ok {
			complete = false
			return
		}
		total += float64(gib) * price / hoursPerMonth
	}
	if b := st.Storage.Root.BootFromVolume; b != nil {
		vol(b.VolumeType, gib(b.Size))
	}
	for _, dv := range st.Storage.Data {
		vol(dv.VolumeType, gib(dv.Size))
	}
	return total, complete
}

func (d *Driver) storagePrice(region, volumeType string) (float64, bool) {
	if d.prices == nil {
		return 0, false
	}
	return d.prices.StoragePrice(region, volumeType)
}

// Prepare implements vendors.NodeOptions: the security groups of the node (the
// NodeClass's own, or a managed group per NodeClass and region and, for
// isolation run, one per run), the server group of a placement profile, and the
// hints of the data volumes.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	st, err := ParseVendorSettings(req.Settings)
	if err != nil {
		return nil, err
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	prep := &vendors.Prepared{}
	groups, err := d.requireGroups(ctx, req)
	if err != nil {
		return nil, err
	}
	prep.Resources = append(prep.Resources, groups...)
	if g := pl.ServerGroup; g != nil {
		ref, err := d.requireServerGroup(ctx, req, g)
		if err != nil {
			return nil, err
		}
		prep.Resources = append(prep.Resources, ref)
	}
	for _, dv := range st.Storage.Data {
		fs := dv.Filesystem
		if fs == "" {
			fs = "ext4"
		}
		prep.Devices = append(prep.Devices, vendors.DeviceHint{
			Name: dv.Name, Filesystem: fs, MountPath: dv.MountPath,
			SerialPattern: dataDeviceSerial, SizeBytes: gib(dv.Size) << 30,
		})
	}
	return prep, nil
}

func (d *Driver) requireGroups(ctx context.Context, req vendors.PrepareRequest) ([]vendors.ResourceRef, error) {
	net := req.Settings.Networking
	class := req.Tags[vendors.TagNodeClass]
	params := SecurityGroupParams{
		SelfReference: net.Isolation == "nodeclass" || (net.Isolation == "run" && req.RunKey == ""),
	}
	if net.Inbound != nil {
		params.Inbound = net.Inbound.Rules
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
	}
	refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{
		Kind: KindSecurityGroups, Key: class, Params: raw, Settings: &req.Settings,
	})
	if err != nil {
		return nil, err
	}
	out := slices.Clone(refs)
	if net.Isolation == "run" && req.RunKey != "" {
		raw, err := json.Marshal(SecurityGroupParams{SelfReference: true})
		if err != nil {
			return nil, vendors.NewError(vendors.Fatal, CodeInvalidSettings, err)
		}
		run, err := req.Resources.Require(ctx, vendors.ResourceRequest{
			Kind: KindRunSecurityGroups, Key: class + "/" + req.RunKey, Params: raw,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, run...)
	}
	return out, nil
}

func (d *Driver) requireServerGroup(
	ctx context.Context, req vendors.PrepareRequest, g *ovhapi.ServerGroup,
) (vendors.ResourceRef, error) {
	profile := req.Settings.Profiles[vendors.ProfilePlacement]
	rr := vendors.ResourceRequest{Kind: KindServerGroups}
	if g.Use != nil {
		rr.Use = &vendors.UseRef{Name: g.Use.Name}
	} else {
		if req.RunKey == "" {
			return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
				"the placement profile %q creates server groups and needs a run key (the placement-group label)", profile))
		}
		rr.Key = profile + "/" + req.RunKey
		params, err := json.Marshal(ServerGroupParams{Policy: string(g.Policy)})
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
			fmt.Errorf("no server group for the profile %q", profile))
	}
	if len(refs) > 1 {
		return vendors.ResourceRef{}, vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf(
			"the name of the server group matches %d groups; it must match one", len(refs)))
	}
	return refs[0], nil
}
