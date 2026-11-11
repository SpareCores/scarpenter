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

	api "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/alicloud"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

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

// The settings types are the API's (pkg/apis/v1alpha1/alicloud); the merged
// block and the profiles reach the driver as JSON in vendors.VendorSettings.
type (
	// Settings is the merged `alicloud` block.
	Settings = api.Settings
	// Storage is the storage block.
	Storage = api.Storage
	// Disk is the system disk.
	Disk = api.Disk
	// DataDisk is an extra disk.
	DataDisk = api.DataDisk
)

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
	return overlay(d, string(r.Category), string(r.PerformanceLevel), r.Size, r.Encrypted, r.KMSKeyID,
		r.ProvisionedIops, r.BurstingEnabled)
}

func overlay(
	d resolvedDisk, category, level string, size *resource.Quantity, encrypted *bool, kms string,
	iops *int64, bursting *bool,
) resolvedDisk {
	if category != "" {
		d.Category = category
		if level == "" && category != CategoryESSD {
			d.Level = ""
		}
	}
	if level != "" {
		d.Level = level
	}
	if g := gibOf(size); g > 0 {
		d.GiB = g
	}
	if encrypted != nil {
		d.Encrypted = *encrypted
	}
	d.KMSKeyID = kms
	d.Iops, d.Bursting = iops, bursting
	return d
}

// dataDisks returns the extra disks in the order of the profile.
func dataDisks(st Settings) []resolvedDisk {
	if st.Storage == nil {
		return nil
	}
	out := make([]resolvedDisk, 0, len(st.Storage.Data))
	for _, dd := range st.Storage.Data {
		size := dd.Size
		d := overlay(resolvedDisk{Category: CategoryESSD, Level: DefaultRootLevel}, string(dd.Category),
			string(dd.PerformanceLevel), &size, dd.Encrypted, dd.KMSKeyID, dd.ProvisionedIops, dd.BurstingEnabled)
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
		charge = string(st.InternetChargeType)
	}
	if networking != nil && !*networking {
		mbit = 0
	}
	return mbit, charge
}

func httpTokens(st Settings) string {
	if st.MetadataOptions != nil && st.MetadataOptions.HTTPTokens != "" {
		return string(st.MetadataOptions.HTTPTokens)
	}
	return DefaultHTTPTokens
}
