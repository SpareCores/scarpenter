/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the OVH block of an SCNodeClass (spec.vendors.ovh): layer 2 of the
// node settings, overriding the driver's built-in defaults for every OVH node
// (00a section 3).
// +kubebuilder:validation:XValidation:rule="!has(self.networking) || !has(self.networking.publicIPv4) || self.networking.publicIPv4",message="publicIPv4 must stay true on OVH: a private-only node needs a billed gateway and is not supported"
type Settings struct {
	// regions is the allow-list of regions, by the names of the Keystone
	// catalog of the project (upper case, such as GRA11 or EU-WEST-PAR); empty
	// means every region the credentials' catalog has and Keeper prices.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// imageOwner is the project ID that owns the Flatcar images; empty means
	// the credentials' own project.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F-]*$`
	// +optional
	ImageOwner string `json:"imageOwner,omitempty"`

	// storage overrides the built-in storage defaults (the flavor's local disk
	// as the root; no extra volumes).
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for OVH. publicIPv4 must
	// stay true.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// configDrive is Nova's config_drive: the user data is also attached as a
	// config drive. Built-in default true.
	// +optional
	ConfigDrive *bool `json:"configDrive,omitempty"`

	// resources says how the OpenStack resources a node depends on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the OVH resource kinds of 00a section 9. Each kind has at most
// one of use, create and forbid.
type Resources struct {
	// images brings the Flatcar boot images, one per region and architecture
	// (SCarpenter never creates images): explicit Glance image IDs per region, a
	// name pattern, tags or native filters. Optional: without it the driver finds
	// the image named flatcar-<channel>-<version>-<arch> in the project. Whatever
	// the lookup, the image's name or tags must say the pinned channel and
	// version, or the region's images are not ready. Images take use only.
	// +kubebuilder:validation:XValidation:rule="has(self.use) && !has(self.create) && !(has(self.forbid) && self.forbid)",message="images accepts use only: it has no create and no forbid"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.select) || self.use.select != 'all'",message="images cannot use select all: a launch needs one image"
	// +optional
	Images *common.Resource `json:"images,omitempty"`

	// securityGroups attached to nodes (Neutron groups, by UUID per region). The
	// project's default group is used when the kind is not configured and the
	// request needs nothing it cannot give.
	// +optional
	SecurityGroups *common.Resource `json:"securityGroups,omitempty"`

	// serverGroups nodes are launched into (see the placement profiles).
	// +optional
	ServerGroups *common.Resource `json:"serverGroups,omitempty"`
}

// VolumeType is an OVH Cinder volume type.
// +kubebuilder:validation:Enum=classic;high-speed;high-speed-gen2
type VolumeType string

// OVH volume types.
const (
	VolumeTypeClassic       VolumeType = "classic"
	VolumeTypeHighSpeed     VolumeType = "high-speed"
	VolumeTypeHighSpeedGen2 VolumeType = "high-speed-gen2"
)

// Storage is the OVH storage block: the root disk and the extra Cinder volumes.
// It is the vendor block's storage (layer 2) and the OVH block of a storage
// profile (layer 3).
type Storage struct {
	// root selects the root disk: absent, the flavor's local NVMe disk.
	// +optional
	Root *Root `json:"root,omitempty"`

	// data lists extra Cinder volumes created with the instance (at most 8).
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	// +optional
	Data []DataVolume `json:"data,omitempty"`

	// localStorage is not supported on OVH: a profile that asks for it is
	// unsupported for every type, and the vendor block is invalid with it.
	// +optional
	LocalStorage *common.LocalStorage `json:"localStorage,omitempty"`
}

// Root selects the root disk.
type Root struct {
	// bootFromVolume boots from a Cinder volume created from the image; the
	// flavor's disk is then unused. The only way to choose the root size and type.
	// +optional
	BootFromVolume *VolumeSpec `json:"bootFromVolume,omitempty"`
}

// VolumeSpec is a Cinder volume type and size.
type VolumeSpec struct {
	// volumeType is the Cinder volume type.
	// +required
	VolumeType VolumeType `json:"volumeType"`

	// size of the volume, 10Gi to 12000Gi.
	// +required
	Size resource.Quantity `json:"size"`
}

// DataVolume is an extra Cinder volume: its type and size plus the guest side
// (name, filesystem, mount path).
type DataVolume struct {
	common.GuestVolume `json:",inline"`

	// volumeType is the Cinder volume type.
	// +required
	VolumeType VolumeType `json:"volumeType"`

	// size of the volume, 10Gi to 12000Gi.
	// +required
	Size resource.Quantity `json:"size"`
}

// Placement is the OVH block of a placement profile.
type Placement struct {
	// serverGroup is the Nova server group nodes join; one is created per run
	// key (label placement-group).
	// +optional
	ServerGroup *ServerGroup `json:"serverGroup,omitempty"`
}

// ServerGroupPolicy is the Nova server group policy.
// +kubebuilder:validation:Enum=affinity;anti-affinity
type ServerGroupPolicy string

// Server group policies.
const (
	PolicyAffinity     ServerGroupPolicy = "affinity"
	PolicyAntiAffinity ServerGroupPolicy = "anti-affinity"
)

// ServerGroup creates a group per run key with the policy, or references an
// existing one (use). Exactly one of policy and use is set.
// +kubebuilder:validation:XValidation:rule="has(self.policy) != has(self.use)",message="set exactly one of policy and use"
type ServerGroup struct {
	// policy of the server groups SCarpenter creates.
	// +optional
	Policy ServerGroupPolicy `json:"policy,omitempty"`

	// use references an existing server group, which is never modified or
	// deleted.
	// +optional
	Use *ServerGroupRef `json:"use,omitempty"`
}

// ServerGroupRef names an existing server group.
type ServerGroupRef struct {
	// name of the server group.
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

// GetImageOwner returns the project that owns the Flatcar images ("" means the
// credentials' own project).
func (s *Settings) GetImageOwner() string {
	if s == nil {
		return ""
	}
	return s.ImageOwner
}
