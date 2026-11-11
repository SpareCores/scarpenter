/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The settings types below mirror the `alicloud` block of spec.vendors and of
// the storage and network profiles (plan 21 section 2). They are local until
// the block's API types land in pkg/apis/v1alpha1/alicloud (plan 14).
//
// TODO(plan 14): replace these with the API types and decode the placement
// profile ({deploymentSet: {strategy | use}}) into the launch resources.

// Disk categories of ECS.
const (
	CategoryESSD       = "cloud_essd"
	CategoryAuto       = "cloud_auto"
	CategoryESSDEntry  = "cloud_essd_entry"
	CategoryEfficiency = "cloud_efficiency"
	CategorySSD        = "cloud_ssd"
)

// Built-in defaults (layer 1, plan 21 section 4).
const (
	DefaultRootCategory  = CategoryESSD
	DefaultRootLevel     = "PL0"
	DefaultRootGiB       = 64
	DefaultBandwidthOut  = 100
	DefaultChargeType    = "PayByTraffic"
	DefaultHTTPTokens    = "optional"
	minSystemDiskGiB     = 20
	maxSystemDiskGiB     = 2048
	giB                  = int64(1) << 30
	maxTags              = 20
	maxInternetBandwidth = 100
)

// Disk is a root or data disk.
type Disk struct {
	Category         string             `json:"category,omitempty"`
	PerformanceLevel string             `json:"performanceLevel,omitempty"`
	Size             *resource.Quantity `json:"size,omitempty"`
	Encrypted        *bool              `json:"encrypted,omitempty"`
	KMSKeyID         string             `json:"kmsKeyID,omitempty"`
	ProvisionedIops  *int64             `json:"provisionedIops,omitempty"`
	BurstingEnabled  *bool              `json:"burstingEnabled,omitempty"`
}

// DataDisk is an extra disk: a Disk with the name, file system and mount
// point of the first-boot unit.
type DataDisk struct {
	Disk       `json:",inline"`
	Name       string `json:"name,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
	MountPath  string `json:"mountPath,omitempty"`
}

// Storage is the storage block.
type Storage struct {
	Root *Disk      `json:"root,omitempty"`
	Data []DataDisk `json:"data,omitempty"`
}

// MetadataOptions are the instance metadata options.
type MetadataOptions struct {
	HTTPTokens string `json:"httpTokens,omitempty"`
}

// Settings is the merged `alicloud` block.
type Settings struct {
	Storage                 *Storage         `json:"storage,omitempty"`
	InternetMaxBandwidthOut *int32           `json:"internetMaxBandwidthOut,omitempty"`
	InternetChargeType      string           `json:"internetChargeType,omitempty"`
	MetadataOptions         *MetadataOptions `json:"metadataOptions,omitempty"`
	ResourceGroupID         string           `json:"resourceGroupId,omitempty"`
	ImageOwner              string           `json:"imageOwner,omitempty"`
}

// decodeSettings decodes the merged vendor block (defaults, NodeClass block and
// the selected storage profile) and lays the selected network profile over it.
func decodeSettings(s vendors.VendorSettings) (Settings, error) {
	var out Settings
	if len(s.Raw) > 0 {
		if err := json.Unmarshal(s.Raw, &out); err != nil {
			return out, fmt.Errorf("decoding the alicloud settings: %w", err)
		}
	}
	if len(s.Network) > 0 {
		if err := json.Unmarshal(s.Network, &out); err != nil {
			return out, fmt.Errorf("decoding the network profile: %w", err)
		}
	}
	return out, nil
}

// resolvedDisk is a disk with the defaults applied.
type resolvedDisk struct {
	Category, Level string
	GiB             int64
	Encrypted       bool
	KMSKeyID        string
	Iops            *int64
	Bursting        *bool
	Name            string
}

func gibOf(q *resource.Quantity) int64 {
	if q == nil {
		return 0
	}
	return (q.Value() + giB - 1) / giB
}

// rootDisk returns the root disk with the defaults: cloud_essd PL0, 64 GiB, not
// encrypted (the RunInstances page refuses encrypted system disks in some
// zones, so the default is off; plan 21 section 4).
func rootDisk(st Settings) resolvedDisk {
	d := resolvedDisk{Category: DefaultRootCategory, Level: DefaultRootLevel, GiB: DefaultRootGiB}
	if st.Storage == nil || st.Storage.Root == nil {
		return d
	}
	return overlayDisk(d, *st.Storage.Root)
}

func overlayDisk(d resolvedDisk, r Disk) resolvedDisk {
	if r.Category != "" {
		d.Category = r.Category
		if r.PerformanceLevel == "" && r.Category != CategoryESSD {
			d.Level = ""
		}
	}
	if r.PerformanceLevel != "" {
		d.Level = r.PerformanceLevel
	}
	if g := gibOf(r.Size); g > 0 {
		d.GiB = g
	}
	if r.Encrypted != nil {
		d.Encrypted = *r.Encrypted
	}
	d.KMSKeyID = r.KMSKeyID
	d.Iops, d.Bursting = r.ProvisionedIops, r.BurstingEnabled
	return d
}

// dataDisks returns the extra disks in the order of the profile.
func dataDisks(st Settings) []resolvedDisk {
	if st.Storage == nil {
		return nil
	}
	out := make([]resolvedDisk, 0, len(st.Storage.Data))
	for _, dd := range st.Storage.Data {
		d := overlayDisk(resolvedDisk{Category: CategoryESSD, Level: DefaultRootLevel}, dd.Disk)
		d.Name = dd.Name
		out = append(out, d)
	}
	return out
}

// RootDiskBytes is the root disk of the resolved settings, what the scheduler
// assumes as ephemeral storage.
func RootDiskBytes(s vendors.VendorSettings) int64 {
	st, err := decodeSettings(s)
	if err != nil {
		return DefaultRootGiB * giB
	}
	return rootDisk(st).GiB * giB
}

// bandwidth returns the resolved public bandwidth and charge type: 0 without a
// public IPv4 (networking.publicIPv4: false and internetMaxBandwidthOut: 0 mean
// the same).
func bandwidth(st Settings, networking *bool) (mbit int32, charge string) {
	mbit, charge = DefaultBandwidthOut, DefaultChargeType
	if st.InternetMaxBandwidthOut != nil {
		mbit = *st.InternetMaxBandwidthOut
	}
	if st.InternetChargeType != "" {
		charge = st.InternetChargeType
	}
	if networking != nil && !*networking {
		mbit = 0
	}
	return mbit, charge
}

func httpTokens(st Settings) string {
	if st.MetadataOptions != nil && st.MetadataOptions.HTTPTokens != "" {
		return st.MetadataOptions.HTTPTokens
	}
	return DefaultHTTPTokens
}
