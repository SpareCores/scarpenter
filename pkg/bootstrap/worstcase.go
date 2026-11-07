/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap

import (
	"encoding/json"
	"fmt"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// storageShape is the part of a vendor's storage block (the block's `storage`
// member and every storage profile) that decides what the volumes template
// renders: the guest side of the volumes, which every vendor spells the same
// (common.GuestVolume, common.LocalStorage).
type storageShape struct {
	Data []struct {
		Name       string `json:"name"`
		Filesystem string `json:"filesystem"`
		MountPath  string `json:"mountPath"`
	} `json:"data"`
	LocalStorage *struct {
		Policy    string `json:"policy"`
		MountPath string `json:"mountPath"`
	} `json:"localStorage"`
}

// worstCase is the largest first-boot volume setup a vendor's block and storage
// profiles allow.
type worstCase struct {
	vendor  string
	volumes []VolumeHint
	local   *LocalHint
}

// Stand-ins for what only the launch knows: the longest plausible serial pattern
// and device pattern of a vendor, a size, and the most local disks an instance
// type has (the AWS d3 and d2 families have up to 24 local disks).
const (
	worstSerialPattern = "vendor-disk-serial-*"
	worstDevicePattern = "nvme-Amazon_EC2_NVMe_Instance_Storage_*"
	worstLocalDisks    = 24
	worstSizeBytes     = int64(16) << 40
)

// worstCaseVolumes returns, per vendor of the NodeClass, every storage setup a
// launch can have: the vendor block's own (storage.data, storage.localStorage)
// and each storage profile on top of it (a profile's data list replaces the
// block's, its localStorage replaces the block's), with stand-ins for the device
// hints that are the longest plausible and local disks counted as the most a type
// has. Which setup is the largest depends on the names and paths, so all are
// rendered (plan 06 and 07 reviews).
func worstCaseVolumes(nc *v1alpha1.SCNodeClass) ([]worstCase, error) {
	var out []worstCase
	for _, vendor := range nc.Spec.Vendors.Names() {
		block, _ := nc.Spec.Vendors.Get(vendor)
		base, err := shapeOf(block, true)
		if err != nil {
			return nil, fmt.Errorf("the %s block: %w", vendor, err)
		}
		candidates := []storageShape{base}
		for name, p := range nc.Spec.Profiles.Storage {
			pb, ok := p.For(vendor)
			if !ok {
				continue
			}
			ps, err := shapeOf(pb, false)
			if err != nil {
				return nil, fmt.Errorf("the storage profile %s of %s: %w", name, vendor, err)
			}
			merged := base
			if ps.Data != nil {
				merged.Data = ps.Data
			}
			if ps.LocalStorage != nil {
				merged.LocalStorage = ps.LocalStorage
			}
			candidates = append(candidates, merged)
		}
		for _, c := range candidates {
			wc := worstCase{vendor: vendor}
			for _, d := range c.Data {
				fs := d.Filesystem
				if fs == "" {
					fs = "ext4"
				}
				wc.volumes = append(wc.volumes, VolumeHint{
					Name: d.Name, Filesystem: fs, MountPath: d.MountPath, SerialPattern: worstSerialPattern, SizeBytes: worstSizeBytes,
				})
			}
			if ls := c.LocalStorage; ls != nil && (ls.Policy == localPolicyMount || ls.Policy == localPolicyRaid0) {
				mount := ls.MountPath
				if mount == "" {
					mount = "/mnt/local"
				}
				wc.local = &LocalHint{
					Policy: ls.Policy, MountPath: mount, DevicePattern: worstDevicePattern, Count: worstLocalDisks,
				}
			}
			if wc.volumes == nil && wc.local == nil {
				continue
			}
			out = append(out, wc)
		}
	}
	return out, nil
}

// shapeOf decodes a vendor block (storage is its `storage` member) or a storage
// profile (the storage block itself).
func shapeOf(v any, inBlock bool) (storageShape, error) {
	var s storageShape
	if v == nil {
		return s, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return s, err
	}
	if inBlock {
		var b struct {
			Storage storageShape `json:"storage"`
		}
		err = json.Unmarshal(raw, &b)
		return b.Storage, err
	}
	err = json.Unmarshal(raw, &s)
	return s, err
}
