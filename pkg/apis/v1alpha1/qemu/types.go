/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package qemu holds the blocks of the qemu vendor, the development driver
// that boots real Flatcar VMs on the developer's machine (the L4 test lane).
//
// +kubebuilder:object:generate=true
package qemu

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Accelerator is the QEMU accelerator.
// +kubebuilder:validation:Enum=auto;kvm;tcg
type Accelerator string

// Settings is the qemu block of an SCNodeClass (spec.vendors.qemu).
type Settings struct {
	// regions is the allow-list of qemu hosts ("regions"); empty means the
	// local host.
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// accelerator used for the VMs: auto picks kvm when /dev/kvm is usable and
	// falls back to tcg (software emulation, slow).
	// +kubebuilder:default=auto
	// +optional
	Accelerator Accelerator `json:"accelerator,omitempty"`

	// storage overrides the qemu driver's defaults.
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for qemu.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// resources says how the host resources the VMs depend on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the qemu resource kinds.
type Resources struct {
	// networks (bridges with DHCP) the VMs attach to.
	// +optional
	Networks *common.Resource `json:"networks,omitempty"`
}

// DiskFormat is the format of a VM disk image.
// +kubebuilder:validation:Enum=qcow2;raw
type DiskFormat string

// Storage is the qemu storage block (vendor block and storage profile).
type Storage struct {
	// root configures the root disk.
	// +optional
	Root *RootDisk `json:"root,omitempty"`

	// data lists extra disks.
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	// +optional
	Data []DataDisk `json:"data,omitempty"`
}

// RootDisk configures the root disk image.
type RootDisk struct {
	// size the root image is grown to.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// format of the overlay image.
	// +optional
	Format DiskFormat `json:"format,omitempty"`
}

// DataDisk is an extra disk image.
type DataDisk struct {
	common.GuestVolume `json:",inline"`

	// size of the disk image.
	// +required
	Size resource.Quantity `json:"size"`

	// format of the disk image.
	// +optional
	Format DiskFormat `json:"format,omitempty"`
}

// NICModel is the emulated network card.
// +kubebuilder:validation:Enum=virtio-net-pci;e1000
type NICModel string

// Network is the qemu block of a network profile.
type Network struct {
	// model of the emulated network card.
	// +optional
	Model NICModel `json:"model,omitempty"`
}

// Placement is the qemu block of a placement profile.
type Placement struct {
	// pinCPUs pins the VM's vCPUs to host cores.
	// +optional
	PinCPUs *bool `json:"pinCPUs,omitempty"`
}

// GetRegions returns the region allow-list (empty: every region).
func (s *Settings) GetRegions() []string {
	if s == nil {
		return nil
	}
	return s.Regions
}
