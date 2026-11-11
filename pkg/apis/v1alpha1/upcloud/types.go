/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the UpCloud block of an SCNodeClass (spec.vendors.upcloud): layer
// 2 of the node settings, overriding the driver's built-in defaults for every
// UpCloud node (00a section 3).
type Settings struct {
	// regions is the allow-list of UpCloud zones (fi-hel1, de-fra1, ...; UpCloud's
	// regions are its zones); empty means every zone the credentials can use.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// storage overrides the built-in storage defaults (the root disk in the
	// tier of the image template, the plan's included size, at least 25 GB,
	// encrypted; no extra disks).
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for UpCloud.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// resources says how the UpCloud resources a node depends on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the UpCloud resource kinds. Each kind has at most one of use,
// create and forbid. Without any of them a node has no private interface (there
// is no default network) unless its isolation needs one (nodeclass, or run
// without a run key: a managed network), and no server group.
type Resources struct {
	// images brings the Flatcar boot templates: private custom templates the
	// operator imported into the account, one per zone and storage tier
	// (rung 3 of the image policy). Optional: without it the driver finds the
	// pinned release by the label convention (scarpenter-image=flatcar,
	// flatcar-channel, flatcar-version, flatcar-arch=amd64, flatcar-tier).
	// Whatever the lookup, the template's labels must say the pinned channel
	// and version, or the zone's images are not ready. Images take use only.
	// +kubebuilder:validation:XValidation:rule="has(self.use) && !has(self.create) && !(has(self.forbid) && self.forbid)",message="images accepts use only: it has no create and no forbid"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.select) || self.use.select != 'all'",message="images cannot use select all: a launch needs one image"
	// +optional
	Images *common.Resource `json:"images,omitempty"`

	// networks are the SDN private networks nodes get a private interface on.
	// Without use or create the node has the public and utility interfaces
	// only, unless networking.isolation is nodeclass, or run (the default)
	// without a run key: those get a managed network.
	// +optional
	Networks *common.Resource `json:"networks,omitempty"`

	// firewalls are SDN firewall rulesets attached to the private interface. They
	// never cover the public interface: UpCloud's Public and Utility firewall is
	// per server and written by the driver for every node.
	// +optional
	Firewalls *common.Resource `json:"firewalls,omitempty"`

	// serverGroups are the anti-affinity groups of the placement profiles.
	// +optional
	ServerGroups *common.Resource `json:"serverGroups,omitempty"`
}

// StorageTier is the tier of a storage device.
// +kubebuilder:validation:Enum=maxiops;standard
type StorageTier string

// DataTier is the tier of an extra storage device.
// +kubebuilder:validation:Enum=maxiops;standard;hdd
type DataTier string

// Storage tiers.
const (
	TierMaxIOPS  StorageTier = "maxiops"
	TierStandard StorageTier = "standard"
	TierHDD      DataTier    = "hdd"
)

// Storage is the UpCloud storage block: the root disk and the extra disks. It
// is the vendor block's storage (layer 2) and the UpCloud block of a storage
// profile (layer 3). UpCloud has no local disks: every plan disk is network
// storage, so there is no localStorage here.
type Storage struct {
	// root configures the root disk, a clone of the Flatcar template.
	// +optional
	Root *RootVolume `json:"root,omitempty"`

	// data lists extra disks created with the server and deleted with it.
	// +kubebuilder:validation:MaxItems=3
	// +listType=map
	// +listMapKey=name
	// +optional
	Data []DataVolume `json:"data,omitempty"`
}

// RootVolume configures the root disk.
type RootVolume struct {
	// tier of the root disk. A clone keeps the tier of its template, so the
	// tier must be one an image template of the zone was made with. Built-in
	// default: the plan's included tier where a template of it exists, else the
	// tier of the template.
	// +optional
	Tier StorageTier `json:"tier,omitempty"`

	// size of the root disk, for example 25Gi (UpCloud's GB are taken as GiB);
	// 10 to 1024. Built-in default: the plan's included storage, at least 25.
	// A disk larger than the plan's included storage is billed in full.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// encrypted encrypts the disk at rest (free). Built-in default true.
	// +optional
	Encrypted *bool `json:"encrypted,omitempty"`
}

// DataVolume is an extra disk: UpCloud settings plus the guest side (name,
// filesystem, mount path).
type DataVolume struct {
	common.GuestVolume `json:",inline"`

	// tier of the disk.
	// +required
	Tier DataTier `json:"tier"`

	// size of the disk, for example 100Gi; 10 to 1024.
	// +required
	Size resource.Quantity `json:"size"`

	// encrypted encrypts the disk at rest. Built-in default true.
	// +optional
	Encrypted *bool `json:"encrypted,omitempty"`
}

// AntiAffinity is the anti-affinity policy of a server group.
//   - strict: the servers of the group must run on different hosts; a launch
//     that cannot fails with no capacity;
//   - yes: the servers are spread over hosts where possible.
//
// +kubebuilder:validation:Enum=strict;yes
type AntiAffinity string

// Anti-affinity policies.
const (
	AntiAffinityStrict AntiAffinity = "strict"
	AntiAffinityYes    AntiAffinity = "yes"
)

// Placement is the UpCloud block of a placement profile: a server group.
// UpCloud has no cluster or co-location construct and no dedicated tenancy, so
// profiles that ask for them have no UpCloud block.
type Placement struct {
	// serverGroup is the anti-affinity group nodes join; it needs a run key
	// (label placement-group) to be created per run.
	// +required
	ServerGroup *ServerGroup `json:"serverGroup"`
}

// ServerGroup creates a group per run key or references an existing one (name).
// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.antiAffinity)",message="antiAffinity is only valid for the groups SCarpenter creates, not with use"
type ServerGroup struct {
	// antiAffinity policy of the groups SCarpenter creates. Default strict.
	// +optional
	AntiAffinity AntiAffinity `json:"antiAffinity,omitempty"`

	// use references an existing server group by title, which is never modified
	// or deleted; the run key then only separates NodeClaims.
	// +optional
	Use *ServerGroupRef `json:"use,omitempty"`
}

// ServerGroupRef names an existing server group.
type ServerGroupRef struct {
	// name is the title of the server group.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +required
	Name string `json:"name"`
}

// GetRegions returns the zone allow-list (empty: every zone).
func (s *Settings) GetRegions() []string {
	if s == nil {
		return nil
	}
	return s.Regions
}
