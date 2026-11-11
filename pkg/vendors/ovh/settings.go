/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Settings are the resolved vendor settings of a node as the launch request
// builder reads them (the merged `vendors.ovh` block, plan 18 "The spec.vendors.ovh
// block" and "Node options"). They are decoded from VendorSettings.Raw.
//
// TODO(plan 14): replace by the NodeClass API types once the `ovh` block exists in
// pkg/apis; the JSON shape is the plan's.
type Settings struct {
	// ConfigDrive is Nova's config_drive; default true (spike S2 may change it).
	ConfigDrive *bool           `json:"configDrive,omitempty"`
	Storage     StorageSettings `json:"storage"`
	Networking  NetworkSettings `json:"networking"`
}

// StorageSettings are the storage block: the flavor's local disk is the root
// unless bootFromVolume says otherwise.
type StorageSettings struct {
	Root RootSettings `json:"root"`
	// Data are extra Cinder volumes created by the server create call (at most 8).
	Data []DataVolume `json:"data,omitempty"`
	// LocalStorage is not supported on OVH in v1: a profile that asks for it is
	// invalid for every type (ADR-017).
	LocalStorage json.RawMessage `json:"localStorage,omitempty"`
}

// RootSettings select the root disk.
type RootSettings struct {
	// BootFromVolume boots from a Cinder volume created from the image; absent,
	// the root is the flavor's local disk.
	BootFromVolume *VolumeSpec `json:"bootFromVolume,omitempty"`
}

// VolumeSpec is a volume type and size.
type VolumeSpec struct {
	VolumeType string            `json:"volumeType"`
	Size       resource.Quantity `json:"size"`
}

// DataVolume is an extra volume; Filesystem and MountPath go to the first-boot
// unit through the device hints (plan 18 "Device hints").
type DataVolume struct {
	Name       string            `json:"name"`
	VolumeType string            `json:"volumeType"`
	Size       resource.Quantity `json:"size"`
	Filesystem string            `json:"filesystem,omitempty"`
	MountPath  string            `json:"mountPath,omitempty"`
}

// NetworkSettings are the generic network overrides.
type NetworkSettings struct {
	// PublicIPv4 must stay true: a private-only node needs a billed gateway and
	// is unsupported in v1 (ADR-017).
	PublicIPv4 *bool `json:"publicIPv4,omitempty"`
	// PublicIPv6 is informational: Ext-Net ports carry both families (S5).
	PublicIPv6 *bool `json:"publicIPv6,omitempty"`
}

// Limits of the Cinder volumes OVH sells, in GiB, and of the data volumes per node.
const (
	MinVolumeGiB  = 10
	MaxVolumeGiB  = 12000
	MaxDataVolume = 8
)

// KnownVolumeTypes are the OVH volume types of the plan; the volume types of a
// region (Cinder) are the truth at launch, this list validates early.
var KnownVolumeTypes = []string{"classic", "high-speed", "high-speed-gen2"}

// ParseSettings decodes and validates the merged vendor block; empty or null raw
// is the defaults.
func ParseSettings(raw json.RawMessage) (Settings, error) {
	var s Settings
	if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return Settings{}, invalidSettings("%v", err)
		}
	}
	if s.ConfigDrive == nil {
		t := true
		s.ConfigDrive = &t
	}
	return s, s.Validate()
}

func invalidSettings(format string, args ...any) error {
	return vendors.NewError(vendors.Fatal, CodeInvalidSettings, fmt.Errorf("ovh settings: "+format, args...))
}

func gib(q resource.Quantity) int64 {
	b := q.Value()
	return (b + 1<<30 - 1) >> 30
}

func checkVolume(what, volumeType string, q resource.Quantity) error {
	if !slices.Contains(KnownVolumeTypes, volumeType) {
		return invalidSettings("%s: volume type %q is not one of %v", what, volumeType, KnownVolumeTypes)
	}
	if g := gib(q); g < MinVolumeGiB || g > MaxVolumeGiB {
		return invalidSettings("%s: size %s is outside %d to %d GiB", what, q.String(), MinVolumeGiB, MaxVolumeGiB)
	}
	return nil
}

// Validate checks what no region can fix (ADR-017: nothing is degraded silently).
func (s Settings) Validate() error {
	if s.Networking.PublicIPv4 != nil && !*s.Networking.PublicIPv4 {
		return invalidSettings("publicIPv4 must stay true: a private-only node needs a billed gateway and is not supported")
	}
	if ls := bytes.TrimSpace(s.Storage.LocalStorage); len(ls) > 0 && !bytes.Equal(ls, []byte("null")) {
		return invalidSettings("localStorage is not supported: the local disks of OVH flavors are the root disk")
	}
	if b := s.Storage.Root.BootFromVolume; b != nil {
		if err := checkVolume("storage.root.bootFromVolume", b.VolumeType, b.Size); err != nil {
			return err
		}
	}
	if len(s.Storage.Data) > MaxDataVolume {
		return invalidSettings("%d data volumes, at most %d", len(s.Storage.Data), MaxDataVolume)
	}
	names := map[string]bool{}
	for i, d := range s.Storage.Data {
		if d.Name == "" || names[d.Name] {
			return invalidSettings("storage.data[%d]: the name %q is empty or repeated", i, d.Name)
		}
		names[d.Name] = true
		if err := checkVolume(fmt.Sprintf("storage.data[%s]", d.Name), d.VolumeType, d.Size); err != nil {
			return err
		}
	}
	return nil
}

// RootDiskBytes is the root disk size the settings fix: the bootFromVolume size,
// or 0 for the flavor's local disk (its size depends on the type, not on the
// settings; the catalog supplies it from Keeper, see RootDiskGB).
func (s Settings) RootDiskBytes() int64 {
	if b := s.Storage.Root.BootFromVolume; b != nil {
		return gib(b.Size) << 30
	}
	return 0
}
