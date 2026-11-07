//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	qemuapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// diskPattern and localPattern are what the guest's by-id links and the serials
// of the driver's disks look like: data disks carry a serial "scv<12 hex>" and
// local disks "scl<n>" (see dataSerial and localSerial).
const (
	dataSerialPattern  = "scv*"
	localDevicePattern = "virtio-scl*"
)

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskBytes implements vendors.NodeOptions.
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	set, err := decodeSettings(s)
	if err != nil {
		return defaultRootBytes()
	}
	return set.rootSize()
}

// Supported implements vendors.NodeOptions (ADR-017): a storage profile with
// local storage needs a type with local disks, a profile that pins CPUs a host
// with enough of them; profiles that do not decode are left out.
func (d *Driver) Supported(it vendors.CatalogType, _ string, p vendors.Profiles) map[string][]string {
	out := map[string][]string{}
	for kind, byName := range p {
		for name, block := range byName {
			if d.profileSupported(kind, block, it) {
				out[kind] = append(out[kind], name)
			}
		}
	}
	for _, names := range out {
		slices.Sort(names)
	}
	return out
}

func (d *Driver) profileSupported(kind string, block json.RawMessage, it vendors.CatalogType) bool {
	switch kind {
	case vendors.ProfileStorage:
		var s qemuapi.Storage
		if json.Unmarshal(block, &s) != nil {
			return false
		}
		if ls := s.LocalStorage; ls != nil && ls.Policy != "ignore" && (it.LocalStorageGB <= 0 || it.LocalStorageCount <= 0) {
			return false
		}
		return true
	case vendors.ProfileNetwork:
		var n qemuapi.Network
		return json.Unmarshal(block, &n) == nil
	case vendors.ProfilePlacement:
		var pl qemuapi.Placement
		if json.Unmarshal(block, &pl) != nil {
			return false
		}
		return pl.PinCPUs == nil || !*pl.PinCPUs || it.VCPUs <= d.host.CPUs()
	}
	return false
}

// PriceDelta implements vendors.NodeOptions: the settings cost nothing.
func (d *Driver) PriceDelta(vendors.VendorSettings, vendors.Offering) (float64, bool) { return 0, true }

// roundUp rounds a disk size up to the 512-byte sector the disk images use, so
// that the size the hint expects is the size the guest sees.
func roundUp(n int64) int64 { return (n + 511) / 512 * 512 }

// Prepare implements vendors.NodeOptions: the network the VM attaches to, and
// the device hints of its extra disks. Nothing else needs to exist before the
// instance (the disk images are made by CreateInstance, from the same plan).
func (d *Driver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	set, err := decodeSettings(req.Settings)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "InvalidSettings", err)
	}
	refs, err := req.Resources.Require(ctx, vendors.ResourceRequest{Kind: KindNetwork})
	if err != nil {
		return nil, err
	}
	prep := &vendors.Prepared{Resources: refs}

	// A data disk whose size no other data disk has is found by its by-id link;
	// disks of one size by the serial pattern and the size (the guest cannot tell
	// them apart before it chooses), which exercises both ways of the first-boot
	// unit.
	plan := diskPlan(set, 0, 0)
	sizes := map[int64]int{}
	for _, dk := range plan {
		sizes[dk.Size]++
	}
	for _, dk := range plan {
		fs := "ext4"
		var mount string
		for _, dd := range set.Storage.Data {
			if dd.Name == dk.Name {
				mount = dd.MountPath
				if dd.Filesystem != "" {
					fs = string(dd.Filesystem)
				}
			}
		}
		hint := vendors.DeviceHint{Name: dk.Name, Filesystem: fs, MountPath: mount}
		if sizes[dk.Size] == 1 {
			hint.ByID = "/dev/disk/by-id/virtio-" + dk.Serial
		} else {
			hint.SerialPattern, hint.SizeBytes = dataSerialPattern, dk.Size
		}
		prep.Devices = append(prep.Devices, hint)
	}
	if ls := set.Storage.LocalStorage; ls != nil && ls.Policy != "ignore" && ls.Policy != "" {
		count := req.Type.LocalStorageCount
		if count == 0 {
			count = d.types[req.InstanceType].LocalStorageCount
		}
		prep.Local = &vendors.LocalHint{
			Policy: string(ls.Policy), MountPath: ls.MountPath, DevicePattern: localDevicePattern, Count: count,
		}
	}
	if prep.Local != nil && prep.Local.MountPath == "" {
		return nil, vendors.NewError(vendors.Fatal, "InvalidSettings",
			fmt.Errorf("localStorage.mountPath is required for the policy %s", prep.Local.Policy))
	}
	return prep, nil
}
