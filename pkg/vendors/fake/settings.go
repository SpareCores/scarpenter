//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The fake serves the vendor ID of the catalog it impersonates (aws by
// default), so its NodeClasses use that vendor's typed blocks and its
// profiles carry that vendor's blocks. It reads the settings through its own
// tolerant structs, which accept the AWS shape (volumeSize, placementGroup)
// and the shape of the fake API package (size, group).

// storageBlock is the storage settings: root and extra volumes, local storage.
type storageBlock struct {
	Root *struct {
		Size       *resource.Quantity `json:"size"`
		VolumeSize *resource.Quantity `json:"volumeSize"`
		IOPS       *int32             `json:"iops"`
		Throughput *int32             `json:"throughput"`
	} `json:"root"`
	Data []dataDisk `json:"data"`
	// LocalStorage mounts the instance-local disks of types that have them.
	LocalStorage *struct {
		Policy    string `json:"policy"`
		MountPath string `json:"mountPath"`
	} `json:"localStorage"`
}

type dataDisk struct {
	Name       string             `json:"name"`
	Filesystem string             `json:"filesystem"`
	MountPath  string             `json:"mountPath"`
	Size       *resource.Quantity `json:"size"`
	VolumeSize *resource.Quantity `json:"volumeSize"`
	IOPS       *int32             `json:"iops"`
	Throughput *int32             `json:"throughput"`
}

func (d dataDisk) bytes() int64 {
	switch {
	case d.VolumeSize != nil:
		return d.VolumeSize.Value()
	case d.Size != nil:
		return d.Size.Value()
	}
	return 0
}

// placementBlock is the placement profile.
type placementBlock struct {
	PlacementGroup *struct {
		Strategy string `json:"strategy"`
		Use      *struct {
			Name string `json:"name"`
		} `json:"use"`
	} `json:"placementGroup"`
	Group *struct {
		Strategy string `json:"strategy"`
	} `json:"group"`
	Tenancy string `json:"tenancy"`
}

// group returns the strategy and the referenced group name of a placement
// profile with a group; ok is false without one.
func (p placementBlock) group() (strategy, use string, ok bool) {
	switch {
	case p.PlacementGroup != nil:
		if p.PlacementGroup.Use != nil {
			return "", p.PlacementGroup.Use.Name, true
		}
		return p.PlacementGroup.Strategy, "", true
	case p.Group != nil:
		return p.Group.Strategy, "", true
	}
	return "", "", false
}

type settingsBlock struct {
	Storage storageBlock `json:"storage"`
}

func decodeSettings(s vendors.VendorSettings) (settingsBlock, error) {
	var b settingsBlock
	if len(s.Raw) > 0 {
		if err := json.Unmarshal(s.Raw, &b); err != nil {
			return b, fmt.Errorf("decoding the settings: %w", err)
		}
	}
	return b, nil
}

func decodePlacement(raw json.RawMessage) (placementBlock, error) {
	var p placementBlock
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("decoding the placement profile: %w", err)
		}
	}
	return p, nil
}

// storageOf decodes a storage profile's block.
func storageOf(raw json.RawMessage) (storageBlock, bool) {
	var s storageBlock
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, false
	}
	return s, true
}

// defaultRoot is the fake's root disk.
const defaultSettings = `{"storage":{"root":{"volumeSize":"64Gi"}}}`

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskBytes implements vendors.NodeOptions.
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	b, err := decodeSettings(s)
	if err != nil || b.Storage.Root == nil {
		return rootDiskBytes.Value()
	}
	switch {
	case b.Storage.Root.VolumeSize != nil:
		return b.Storage.Root.VolumeSize.Value()
	case b.Storage.Root.Size != nil:
		return b.Storage.Root.Size.Value()
	}
	return rootDiskBytes.Value()
}

// RootDiskForType implements vendors.NodeOptions: the steering's rootDisks
// rules make a type define its own root disk.
func (d *Driver) RootDiskForType(_ vendors.VendorSettings, it vendors.CatalogType) (int64, bool) {
	return d.current().rootDisk(it)
}

// Supported implements vendors.NodeOptions: every defined profile is valid
// except where it cannot be honoured (ADR-017): local storage needs a type with
// local disks, and the ConfigMap's unsupported rules (steering, for the
// scenarios that need a type that cannot host a profile).
func (d *Driver) Supported(it vendors.CatalogType, zone string, p vendors.Profiles) map[string][]string {
	conf := d.current()
	out := map[string][]string{}
	for kind, byName := range p {
		for name, block := range byName {
			if kind == vendors.ProfileStorage {
				if st, ok := storageOf(block); ok && st.LocalStorage != nil && st.LocalStorage.Policy != "ignore" &&
					it.LocalStorageGB <= 0 {
					continue
				}
			}
			if conf.unsupported(kind, name, it, zone) {
				continue
			}
			out[kind] = append(out[kind], name)
		}
	}
	for _, names := range out {
		slices.Sort(names)
	}
	return out
}

// NodeOptionsEpoch implements vendors.EpochReporter: it changes when the
// steering ConfigMap's unsupported and rootDisks rules do.
func (d *Driver) NodeOptionsEpoch() uint64 {
	d.refreshConfig(context.Background())
	return d.unsupportedEpoch.Load()
}

// storagePricePerGiBMonth is the fake's price of a disk, per GiB and month.
const (
	storagePricePerGiBMonth = 0.10
	hoursPerMonth           = 730
)

// PriceDelta implements vendors.NodeOptions: extra volumes, and the root disk
// beyond the default, cost storagePricePerGiBMonth; provisioned IOPS and
// throughput and dedicated tenancy are not priced, which makes the price
// incomplete (00a section 10).
func (d *Driver) PriceDelta(s vendors.VendorSettings, _ vendors.Offering) (float64, bool) {
	b, err := decodeSettings(s)
	if err != nil {
		return 0, true
	}
	complete := true
	var extra int64
	if b.Storage.Root != nil {
		if b.Storage.Root.IOPS != nil || b.Storage.Root.Throughput != nil {
			complete = false
		}
		if root := d.RootDiskBytes(s); root > rootDiskBytes.Value() {
			extra += root - rootDiskBytes.Value()
		}
	}
	for _, v := range b.Storage.Data {
		extra += v.bytes()
		if v.IOPS != nil || v.Throughput != nil {
			complete = false
		}
	}
	if pl, err := decodePlacement(s.Placement); err == nil && strings.EqualFold(pl.Tenancy, "dedicated") {
		complete = false
	}
	gib := float64(extra) / float64(1<<30)
	return gib * storagePricePerGiBMonth / hoursPerMonth, complete
}

// Prepare implements vendors.NodeOptions: the regional network the node
// attaches to, the placement group of the run (a profile with a group needs a
// run key), the separate volumes with their device hints, and the local
// storage hint.
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	b, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "InvalidSettings", err)
	}
	pl, err := decodePlacement(req.Settings.Placement)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "InvalidSettings", err)
	}
	prep := &vendors.Prepared{}
	claim := req.Tags[vendors.TagNodeClaim]

	require := func(r vendors.ResourceRequest) error {
		refs, err := req.Resources.Require(ctx, r)
		if err != nil {
			return err
		}
		prep.Resources = append(prep.Resources, refs...)
		return nil
	}
	if err := require(vendors.ResourceRequest{Kind: KindNetwork}); err != nil {
		return nil, err
	}
	if strategy, use, ok := pl.group(); ok {
		if req.RunKey == "" {
			return nil, vendors.Errorf(vendors.Fatal, "the placement profile %q needs a run key (the placement-group label)",
				req.Settings.Profiles[vendors.ProfilePlacement])
		}
		r := vendors.ResourceRequest{Kind: KindGroup}
		if use != "" {
			r.Use = &vendors.UseRef{Name: use}
		} else {
			r.Key = req.Settings.Profiles[vendors.ProfilePlacement] + "/" + req.RunKey
			r.Params, _ = json.Marshal(map[string]string{"strategy": strategy})
		}
		if err := require(r); err != nil {
			return nil, err
		}
	}
	for _, v := range b.Storage.Data {
		params, _ := json.Marshal(map[string]int64{"sizeBytes": v.bytes()})
		before := len(prep.Resources)
		if err := require(vendors.ResourceRequest{
			Kind: KindVolume, Key: claim + "/" + req.Zone + "/" + v.Name, Zone: req.Zone, Params: params,
		}); err != nil {
			return nil, err
		}
		fs := v.Filesystem
		if fs == "" {
			fs = "ext4"
		}
		vol := prep.Resources[before]
		prep.Devices = append(prep.Devices, vendors.DeviceHint{
			Name: v.Name, Filesystem: fs, MountPath: v.MountPath, ByID: "/dev/disk/by-id/fake-" + vol.ID,
		})
	}
	if ls := b.Storage.LocalStorage; ls != nil && ls.Policy != "" && ls.Policy != "ignore" {
		count := max(req.Type.LocalStorageCount, 1)
		prep.Local = &vendors.LocalHint{
			Policy: ls.Policy, MountPath: ls.MountPath, DevicePattern: "fake-local-*", Count: count,
		}
	}
	return prep, nil
}
