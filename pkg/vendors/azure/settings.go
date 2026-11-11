/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The settings are the Azure vendor block of plan 17 section 7. They are
// decoded here from the merged JSON the core hands over; the API types of the
// NodeClass block (pkg/apis/v1alpha1/azure) come with the end-to-end step
// (TODO(plan 14)), and these types then move there.

// Built-in defaults (layer 1).
const (
	DefaultStorageAccountType = "Premium_LRS"
	DefaultRootDiskGB         = 64
	DefaultCaching            = "ReadWrite"
	maxUserData               = 16384
)

// Storage account types of managed disks that v1 creates inline.
var storageAccountTypes = []string{"Standard_LRS", "StandardSSD_LRS", "StandardSSD_ZRS", "Premium_LRS", "Premium_ZRS"}

// IsPremium reports whether the storage account type needs the PremiumIO
// capability.
func IsPremium(t string) bool { return t == "Premium_LRS" || t == "Premium_ZRS" }

// Settings is the Azure vendor block.
type Settings struct {
	Storage            *StorageSettings `json:"storage,omitempty"`
	AdminSSHPublicKey  string           `json:"adminSSHPublicKey,omitempty"`
	EncryptionAtHost   *bool            `json:"encryptionAtHost,omitempty"`
	ImageOwner         string           `json:"imageOwner,omitempty"`
	SubscriptionID     string           `json:"subscriptionID,omitempty"`
	AcceleratedNetwork string           `json:"-"` // set from the network profile by DecodeSettings
}

// StorageSettings is storage.
type StorageSettings struct {
	Root *RootDisk  `json:"root,omitempty"`
	Data []DataDisk `json:"data,omitempty"`
}

// RootDisk is storage.root.
type RootDisk struct {
	StorageAccountType  string     `json:"storageAccountType,omitempty"`
	DiskSizeGB          int32      `json:"diskSizeGB,omitempty"`
	Caching             string     `json:"caching,omitempty"`
	DiskEncryptionSetID string     `json:"diskEncryptionSetID,omitempty"`
	Ephemeral           *Ephemeral `json:"ephemeral,omitempty"`
}

// Ephemeral is storage.root.ephemeral: CacheDisk, ResourceDisk or NvmeDisk.
type Ephemeral struct {
	Placement string `json:"placement"`
}

// DataDisk is one entry of storage.data[].
type DataDisk struct {
	Name               string `json:"name"`
	StorageAccountType string `json:"storageAccountType,omitempty"`
	DiskSizeGB         int32  `json:"diskSizeGB"`
	Caching            string `json:"caching,omitempty"`
	Filesystem         string `json:"filesystem,omitempty"`
	MountPath          string `json:"mountPath,omitempty"`
}

// networkProfile is the Azure network profile block.
type networkProfile struct {
	AcceleratedNetworking *struct {
		Enabled string `json:"enabled"`
	} `json:"acceleratedNetworking,omitempty"`
}

// DecodeSettings decodes the merged vendor block and the selected network
// profile of the resolved settings. Accelerated networking is "auto" (the
// default), settingTrue or "false".
func DecodeSettings(s vendors.VendorSettings) (Settings, error) {
	var out Settings
	if len(s.Raw) > 0 {
		if err := json.Unmarshal(s.Raw, &out); err != nil {
			return out, fmt.Errorf("decoding the Azure settings: %w", err)
		}
	}
	out.AcceleratedNetwork = "auto"
	if len(s.Network) > 0 {
		var np networkProfile
		if err := json.Unmarshal(s.Network, &np); err != nil {
			return out, fmt.Errorf("decoding the Azure network profile: %w", err)
		}
		if np.AcceleratedNetworking != nil && np.AcceleratedNetworking.Enabled != "" {
			out.AcceleratedNetwork = np.AcceleratedNetworking.Enabled
		}
	}
	if !slices.Contains([]string{"auto", settingTrue, "false"}, out.AcceleratedNetwork) {
		return out, fmt.Errorf("acceleratedNetworking.enabled %q is not auto, true or false", out.AcceleratedNetwork)
	}
	return out, nil
}

// Root returns the resolved root disk with the defaults applied.
func (st Settings) Root() RootDisk {
	r := RootDisk{StorageAccountType: DefaultStorageAccountType, DiskSizeGB: DefaultRootDiskGB, Caching: DefaultCaching}
	if st.Storage == nil || st.Storage.Root == nil {
		return r
	}
	in := *st.Storage.Root
	if in.StorageAccountType != "" {
		r.StorageAccountType = in.StorageAccountType
	}
	if in.DiskSizeGB > 0 {
		r.DiskSizeGB = in.DiskSizeGB
	}
	if in.Caching != "" {
		r.Caching = in.Caching
	}
	r.DiskEncryptionSetID = in.DiskEncryptionSetID
	r.Ephemeral = in.Ephemeral
	return r
}

// DataDisks returns the extra disks in profile order (the LUN is the index).
func (st Settings) DataDisks() []DataDisk {
	if st.Storage == nil {
		return nil
	}
	out := slices.Clone(st.Storage.Data)
	for i := range out {
		if out[i].StorageAccountType == "" {
			out[i].StorageAccountType = DefaultStorageAccountType
		}
		if out[i].Caching == "" {
			out[i].Caching = "None"
		}
	}
	return out
}

// RootDiskBytes is the root disk the scheduler assumes as ephemeral storage.
func (st Settings) RootDiskBytes() int64 { return int64(st.Root().DiskSizeGB) << 30 }

// validStorageType reports whether t is a type v1 creates inline.
func validStorageType(t string) bool { return slices.Contains(storageAccountTypes, t) }
