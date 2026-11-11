/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"encoding/json"
	"fmt"

	gcpapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The built-in defaults (00a section 5.3, layer 1): a 64 GiB root disk; its type
// is pd-balanced, or hyperdisk-balanced for the series that boot only from
// Hyperdisk (DefaultRootDiskType, decided per machine type at launch, which is
// why the default block names no type: a pinned pd-balanced and the default
// must stay distinguishable).
const (
	defaultRootGiB = 64
	giB            = int64(1) << 30
)

const defaultSettings = `{"storage":{"root":{"sizeGb":64}}}`

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// decodeSettings decodes the merged vendor block (defaults, NodeClass block and
// the selected storage profile).
func decodeSettings(s vendors.VendorSettings) (gcpapi.Settings, error) {
	var out gcpapi.Settings
	if len(s.Raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(s.Raw, &out); err != nil {
		return out, fmt.Errorf("decoding the GCP settings: %w", err)
	}
	return out, nil
}

func decodePlacement(raw json.RawMessage) (gcpapi.Placement, error) {
	var p gcpapi.Placement
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("decoding the placement profile: %w", err)
		}
	}
	return p, nil
}

func decodeNetwork(raw json.RawMessage) (gcpapi.Network, error) {
	var n gcpapi.Network
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &n); err != nil {
			return n, fmt.Errorf("decoding the network profile: %w", err)
		}
	}
	return n, nil
}

func decodeStorage(raw json.RawMessage) (gcpapi.Storage, error) {
	var s gcpapi.Storage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, fmt.Errorf("decoding the storage profile: %w", err)
		}
	}
	return s, nil
}

// rootDisk returns the resolved boot disk of the settings for a machine type.
func rootDisk(st gcpapi.Settings, machineType string) Disk {
	d := Disk{Type: DefaultRootDiskType(machineType), SizeGb: defaultRootGiB}
	if st.Storage == nil || st.Storage.Root == nil {
		return d
	}
	r := st.Storage.Root
	if r.DiskType != "" {
		d.Type = string(r.DiskType)
	}
	if r.SizeGb != nil && *r.SizeGb > 0 {
		d.SizeGb = *r.SizeGb
	}
	d.ProvisionedIops, d.ProvisionedThroughput = deref(r.ProvisionedIops), deref(r.ProvisionedThroughput)
	d.KMSKeyName = r.KMSKeyName
	return d
}

// dataDisk converts an extra disk of the settings.
func dataDisk(dd gcpapi.DataDisk) Disk {
	return Disk{
		Name: dd.Name, Type: string(dd.DiskType), SizeGb: dd.SizeGb,
		ProvisionedIops: deref(dd.ProvisionedIops), ProvisionedThroughput: deref(dd.ProvisionedThroughput),
		KMSKeyName: dd.KMSKeyName,
	}
}

func dataDisks(st gcpapi.Settings) []gcpapi.DataDisk {
	if st.Storage == nil {
		return nil
	}
	return st.Storage.Data
}

// localStorage returns the local storage policy block, false without one.
func localStorage(st gcpapi.Settings) (policy, mountPath string, ok bool) {
	if st.Storage == nil || st.Storage.LocalStorage == nil {
		return "", "", false
	}
	ls := st.Storage.LocalStorage
	return string(ls.Policy), ls.MountPath, true
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// Defaults implements vendors.NodeOptions.
func (d *Driver) Defaults() vendors.VendorSettings {
	s := vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)}
	s.RootDiskBytes = d.RootDiskBytes(s)
	return s
}

// RootDiskForType implements vendors.NodeOptions: the boot disk is a disk of the
// settings, not of the type.
func (d *Driver) RootDiskForType(vendors.VendorSettings, vendors.CatalogType) (int64, bool) {
	return 0, false
}

// RootDiskBytes implements vendors.NodeOptions: the size of the boot disk, which
// the scheduler assumes as ephemeral storage.
func (d *Driver) RootDiskBytes(s vendors.VendorSettings) int64 {
	st, err := decodeSettings(s)
	if err != nil {
		return defaultRootGiB * giB
	}
	return rootDisk(st, "").SizeGb * giB
}
