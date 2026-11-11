/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the Azure block of an SCNodeClass (spec.vendors.azure): layer 2 of
// the node settings, overriding the driver's built-in defaults for every Azure
// node (00a section 3). Azure's unit is the region (the location name, for
// example westeurope); logical zones 1, 2 and 3 keep their names.
type Settings struct {
	// regions is the allow-list of Azure regions (location names); empty means
	// every region the subscription can use.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// subscriptionID is a guard: it must equal the subscription the controller
	// is configured for (AZURE_SUBSCRIPTION_ID), so that a NodeClass cannot
	// target the wrong controller. Empty means no check.
	// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})?$`
	// +optional
	SubscriptionID string `json:"subscriptionID,omitempty"`

	// imageOwner is the marketplace publisher of the Flatcar images. Empty
	// means the official publisher (kinvolk).
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9][A-Za-z0-9._-]{0,127})?$`
	// +optional
	ImageOwner string `json:"imageOwner,omitempty"`

	// adminSSHPublicKey is the public key (an authorized_keys line) the VMs are
	// created with: Azure's API wants a credential for a Linux VM. Empty means a
	// key generated at start whose private half is discarded; access to nodes is
	// Ignition's business.
	// +kubebuilder:validation:MaxLength=8192
	// +optional
	AdminSSHPublicKey string `json:"adminSSHPublicKey,omitempty"`

	// encryptionAtHost encrypts the VM's disk caches and temporary disk at the
	// host. It needs the subscription feature EncryptionAtHost.
	// +optional
	EncryptionAtHost *bool `json:"encryptionAtHost,omitempty"`

	// storage overrides the built-in storage defaults (Premium_LRS, 64 GiB
	// root disk; no extra disks).
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for Azure.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// resources says how the Azure resources a node depends on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the Azure resource kinds of 00a section 9. Each kind has at most
// one of use, create and forbid.
type Resources struct {
	// resourceGroups hold the VMs, their NICs and public IPs (and the managed
	// network). Its ID is the group's name.
	// +optional
	ResourceGroups *common.Resource `json:"resourceGroups,omitempty"`

	// subnets nodes are launched into. A subnet is selected through its virtual
	// network (tags are the VNet's); Azure subnets are regional, so {zone} is not
	// valid here.
	// +optional
	Subnets *common.Resource `json:"subnets,omitempty"`

	// securityGroups (network security groups) attached to the node NICs. A NIC
	// takes one group, so a group brought with use decides alone and
	// networking.isolation has no effect on it.
	// +optional
	SecurityGroups *common.Resource `json:"securityGroups,omitempty"`

	// proximityPlacementGroups nodes are launched into (see the placement
	// profiles).
	// +optional
	ProximityPlacementGroups *common.Resource `json:"proximityPlacementGroups,omitempty"`

	// images brings the Flatcar boot images with the one selector of every
	// resource kind (plan 14 A1): marketplace image URNs per region
	// (publisher:offer:sku:version) or a name pattern with a version glob.
	// Optional: without it the driver finds the pinned release in the official
	// offers. Whatever the lookup, the image's version must be the pinned one,
	// or the region's images are not ready. Images take use only.
	// +kubebuilder:validation:XValidation:rule="has(self.use) && !has(self.create) && !(has(self.forbid) && self.forbid)",message="images accepts use only: it has no create and no forbid"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.select) || self.use.select != 'all'",message="images cannot use select all: a launch needs one image"
	// +optional
	Images *common.Resource `json:"images,omitempty"`
}

// StorageAccountType is the SKU of a managed disk.
// +kubebuilder:validation:Enum=Standard_LRS;StandardSSD_LRS;StandardSSD_ZRS;Premium_LRS;Premium_ZRS
type StorageAccountType string

// Managed disk SKUs that v1 creates with the VM.
const (
	StandardLRS    StorageAccountType = "Standard_LRS"
	StandardSSDLRS StorageAccountType = "StandardSSD_LRS"
	StandardSSDZRS StorageAccountType = "StandardSSD_ZRS"
	PremiumLRS     StorageAccountType = "Premium_LRS"
	PremiumZRS     StorageAccountType = "Premium_ZRS"
)

// Caching is the host caching of a disk.
// +kubebuilder:validation:Enum=None;ReadOnly;ReadWrite
type Caching string

// Storage is the Azure storage block: the root disk, the extra managed disks and
// the local disks. It is the vendor block's storage (layer 2) and the Azure
// block of a storage profile (layer 3).
type Storage struct {
	// root configures the OS disk.
	// +optional
	Root *RootDisk `json:"root,omitempty"`

	// data lists extra managed disks created with the VM, at the LUNs of their
	// order.
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Data []DataDisk `json:"data,omitempty"`

	// localStorage configures the VM's temporary and local NVMe disks. Only
	// ignore is supported in v1.
	// +optional
	LocalStorage *LocalStorage `json:"localStorage,omitempty"`
}

// LocalStorage is the local disk policy; v1 supports ignore only (mount and raid0
// wait for the device-path spike of plan 17).
type LocalStorage struct {
	// policy: ignore leaves the local disks alone.
	// +kubebuilder:validation:Enum=ignore
	// +required
	Policy common.LocalStoragePolicy `json:"policy"`
}

// RootDisk configures the OS disk.
// +kubebuilder:validation:XValidation:rule="!has(self.ephemeral) || !has(self.storageAccountType)",message="an ephemeral OS disk has no storageAccountType"
// +kubebuilder:validation:XValidation:rule="!has(self.ephemeral) || !has(self.caching) || self.caching == 'ReadOnly'",message="an ephemeral OS disk is read-only cached"
type RootDisk struct {
	// storageAccountType is the disk SKU. Built-in default Premium_LRS (sizes
	// without the PremiumIO capability then get no offering: set
	// StandardSSD_LRS to include them).
	// +optional
	StorageAccountType StorageAccountType `json:"storageAccountType,omitempty"`

	// diskSizeGB of the OS disk. Built-in default 64.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=4095
	// +optional
	DiskSizeGB int32 `json:"diskSizeGB,omitempty"`

	// caching of the OS disk. Built-in default ReadWrite.
	// +optional
	Caching Caching `json:"caching,omitempty"`

	// diskEncryptionSetID is the disk encryption set (customer-managed key) of
	// the disk; platform-managed keys (always on) when empty.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	DiskEncryptionSetID string `json:"diskEncryptionSetID,omitempty"`

	// ephemeral puts the OS disk on local storage instead of a managed disk.
	// +optional
	Ephemeral *Ephemeral `json:"ephemeral,omitempty"`
}

// EphemeralPlacement is where an ephemeral OS disk lives.
// +kubebuilder:validation:Enum=CacheDisk;ResourceDisk;NvmeDisk
type EphemeralPlacement string

// Ephemeral is an ephemeral OS disk.
type Ephemeral struct {
	// placement of the ephemeral disk: the cache, the temporary (resource) disk
	// or the local NVMe disk. The size must support it and the image must fit.
	// +required
	Placement EphemeralPlacement `json:"placement"`
}

// DataDisk is an extra managed disk: Azure settings plus the guest side (name,
// filesystem, mount path). Its device is /dev/disk/azure/scsi1/lun<N>, N the
// index in the list.
type DataDisk struct {
	common.GuestVolume `json:",inline"`

	// storageAccountType is the disk SKU. Built-in default Premium_LRS.
	// +optional
	StorageAccountType StorageAccountType `json:"storageAccountType,omitempty"`

	// diskSizeGB of the disk.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32767
	// +required
	DiskSizeGB int32 `json:"diskSizeGB"`

	// caching of the disk. Built-in default None.
	// +optional
	Caching Caching `json:"caching,omitempty"`
}

// AcceleratedNetworkingMode is the setting of accelerated networking.
// +kubebuilder:validation:Enum=auto;"true";"false"
type AcceleratedNetworkingMode string

// Network is the Azure block of a network profile.
type Network struct {
	// acceleratedNetworking configures SR-IOV on the NIC. auto (the built-in
	// default) turns it on for sizes that support it and off elsewhere; true makes
	// the other sizes infeasible.
	// +optional
	AcceleratedNetworking *AcceleratedNetworking `json:"acceleratedNetworking,omitempty"`
}

// AcceleratedNetworking configures accelerated networking.
type AcceleratedNetworking struct {
	// enabled is auto, "true" or "false" (quote the booleans).
	// +required
	Enabled AcceleratedNetworkingMode `json:"enabled"`
}

// Placement is the Azure block of a placement profile.
type Placement struct {
	// proximityPlacementGroup is the placement construct nodes join; a profile
	// that creates groups needs a run key (label placement-group) to create one
	// per run.
	// +optional
	ProximityPlacementGroup *ProximityPlacementGroup `json:"proximityPlacementGroup,omitempty"`
}

// PlacementStrategy is the strategy of a proximity placement group.
// +kubebuilder:validation:Enum=cluster
type PlacementStrategy string

// ProximityPlacementGroup creates a group per run key (strategy) or references an
// existing one (use). Exactly one of strategy and use is set.
// +kubebuilder:validation:XValidation:rule="has(self.strategy) != has(self.use)",message="set exactly one of strategy and use"
type ProximityPlacementGroup struct {
	// strategy of the groups SCarpenter creates: cluster keeps the VMs close.
	// +optional
	Strategy PlacementStrategy `json:"strategy,omitempty"`

	// use references an existing proximity placement group, which is never
	// modified or deleted; the run key then only separates NodeClaims.
	// +optional
	Use *ProximityPlacementGroupRef `json:"use,omitempty"`
}

// ProximityPlacementGroupRef names an existing proximity placement group.
type ProximityPlacementGroupRef struct {
	// name of the proximity placement group.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +required
	Name string `json:"name"`
}

// GetRegions returns the region allow-list (empty: every region).
func (s *Settings) GetRegions() []string {
	if s == nil {
		return nil
	}
	return s.Regions
}

// GetImageOwner returns the marketplace publisher of the Flatcar images ("" means
// the official publisher).
func (s *Settings) GetImageOwner() string {
	if s == nil {
		return ""
	}
	return s.ImageOwner
}
