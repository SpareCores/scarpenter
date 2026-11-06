/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package fake holds the blocks of the fake vendor, the development driver
// that launches KWOK nodes for any catalog instance type. Its blocks mirror the
// shape of a real vendor's (regions, storage, resources, profiles) so the
// shared machinery (settings resolution, resources, profiles) can be tested
// without a cloud.
//
// +kubebuilder:object:generate=true
package fake

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the fake block of an SCNodeClass (spec.vendors.fake).
type Settings struct {
	// regions is the allow-list of regions; empty means every region of the
	// catalog.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// storage overrides the fake driver's defaults.
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for the fake vendor.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// resources says how the fake resources a node depends on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the fake resource kinds.
type Resources struct {
	// networks the fake nodes attach to.
	// +optional
	Networks *common.Resource `json:"networks,omitempty"`
}

// Storage is the fake storage block (vendor block and storage profile).
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

// RootDisk configures the fake root disk.
type RootDisk struct {
	// size of the root disk.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
}

// DataDisk is a fake extra disk.
type DataDisk struct {
	common.GuestVolume `json:",inline"`

	// size of the disk.
	// +required
	Size resource.Quantity `json:"size"`
}

// Network is the fake block of a network profile.
type Network struct {
	// accelerated marks the fake nodes as having accelerated networking.
	// +optional
	Accelerated *bool `json:"accelerated,omitempty"`
}

// GroupStrategy is the strategy of a fake placement group.
// +kubebuilder:validation:Enum=cluster;spread
type GroupStrategy string

// Placement is the fake block of a placement profile.
type Placement struct {
	// group is the fake placement construct nodes join; it needs a run key.
	// +optional
	Group *Group `json:"group,omitempty"`
}

// Group configures a fake placement group.
type Group struct {
	// strategy of the group.
	// +required
	Strategy GroupStrategy `json:"strategy"`
}
