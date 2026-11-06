/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package common

// Filesystem of an extra volume.
// +kubebuilder:validation:Enum=ext4;xfs;btrfs
type Filesystem string

// Filesystems.
const (
	FilesystemExt4  Filesystem = "ext4"
	FilesystemXFS   Filesystem = "xfs"
	FilesystemBtrfs Filesystem = "btrfs"
)

// GuestVolume is the part of an extra volume that describes the guest, not
// the vendor: the same on every vendor (00a section 6.1). The vendor's own
// fields (type, size, IOPS, ...) sit next to it in the vendor package.
type GuestVolume struct {
	// name identifies the volume in the profile and in the device hints
	// rendered into Ignition.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	// +required
	Name string `json:"name"`

	// filesystem the first-boot unit creates on the volume.
	// +kubebuilder:default=ext4
	// +optional
	Filesystem Filesystem `json:"filesystem,omitempty"`

	// mountPath is the absolute path the volume is mounted at.
	// +kubebuilder:validation:Pattern=`^/[^\x00]*$`
	// +kubebuilder:validation:MaxLength=255
	// +required
	MountPath string `json:"mountPath"`
}

// LocalStoragePolicy is what to do with the local disks of instance types
// that have them.
// +kubebuilder:validation:Enum=ignore;mount;raid0
type LocalStoragePolicy string

// Local storage policies.
const (
	LocalStorageIgnore LocalStoragePolicy = "ignore"
	LocalStorageMount  LocalStoragePolicy = "mount"
	LocalStorageRaid0  LocalStoragePolicy = "raid0"
)

// LocalStorage configures instance-local disks; it is valid only on types that
// have them.
type LocalStorage struct {
	// policy: ignore leaves the disks alone, mount mounts them, raid0 stripes
	// them into one volume and mounts it.
	// +required
	Policy LocalStoragePolicy `json:"policy"`

	// mountPath is the absolute path the local storage is mounted at.
	// +kubebuilder:validation:Pattern=`^/[^\x00]*$`
	// +kubebuilder:validation:MaxLength=255
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}
