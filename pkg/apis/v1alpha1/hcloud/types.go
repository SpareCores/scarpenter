/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the Hetzner Cloud block of an SCNodeClass (spec.vendors.hcloud):
// layer 2 of the node settings, overriding the driver's built-in defaults for
// every Hetzner Cloud node (00a section 3).
type Settings struct {
	// regions is the allow-list of Hetzner locations (fsn1, nbg1, hel1, ash,
	// hil, sin); empty means every location of the project.
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// networking overrides the generic spec.networking for Hetzner. A server
	// needs a public IPv4 or IPv6 address (publicIPv4 defaults to true,
	// publicIPv6 to false); inbound rules become the rules of the firewall
	// SCarpenter creates.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// sshKey is the name or ID of an SSH key of the project that is injected
	// into the nodes. Optional, default none: nodes are configured by
	// Ignition and need no key.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	SSHKey string `json:"sshKey,omitempty"`

	// storage adds data volumes. The root disk belongs to the server type
	// and cannot be configured.
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// resources says how the Hetzner resources a node depends on are
	// provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the Hetzner resource kinds of 00a section 9. Each kind has at
// most one of use, create and forbid.
type Resources struct {
	// images brings the Flatcar snapshots with the one selector of every
	// resource kind (plan 14 A1). Optional: without it the driver finds the
	// snapshots of the pinned release by their labels (os=flatcar,
	// flatcar-channel and flatcar-version). Snapshots are global in the
	// project, so IDs go under the key "*". Images take use only.
	// +kubebuilder:validation:XValidation:rule="has(self.use) && !has(self.create) && !(has(self.forbid) && self.forbid)",message="images accepts use only: it has no create and no forbid"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.select) || self.use.select != 'all'",message="images cannot use select all: a launch needs one image"
	// +optional
	Images *common.Resource `json:"images,omitempty"`

	// networks brings a private network per location (by ID, name, labels or
	// range). SCarpenter does not create networks in v1: use only.
	// +kubebuilder:validation:XValidation:rule="!has(self.create)",message="networks.create is not supported in v1: bring the network with use"
	// +optional
	Networks *common.Resource `json:"networks,omitempty"`

	// firewalls applied to the nodes. Without use, SCarpenter creates one
	// deny-inbound firewall per location (plus the networking.inbound rules)
	// and deletes it when it has been idle for its TTL.
	// +optional
	Firewalls *common.Resource `json:"firewalls,omitempty"`

	// placementGroups nodes are launched into (see the placement profiles).
	// +optional
	PlacementGroups *common.Resource `json:"placementGroups,omitempty"`
}

// Storage is the Hetzner storage block: data volumes. It is the vendor block's
// storage (layer 2) and the hcloud block of a storage profile (layer 3). There
// is no root and no local storage: the root disk is the server type's, and
// Hetzner servers have no local disks.
type Storage struct {
	// data lists data volumes: each is created in the location before the
	// server and attached to it. Volumes outlive servers, so SCarpenter
	// deletes them once no server uses them.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +optional
	Data []DataVolume `json:"data,omitempty"`
}

// DataVolume is a data volume: its size plus the guest side (name, filesystem,
// mount path). The first-boot unit formats and mounts it by its stable
// /dev/disk/by-id link.
type DataVolume struct {
	common.GuestVolume `json:",inline"`

	// size of the volume, e.g. 100Gi. Hetzner counts GB: the size is rounded
	// up to whole GB and is 10 GB to 10 TB.
	// +required
	Size resource.Quantity `json:"size"`
}

// Placement is the Hetzner block of a placement profile: a placement group.
type Placement struct {
	// placementGroup is the placement construct nodes join; a group SCarpenter
	// creates needs a run key (label placement-group). A spread group holds
	// at most 10 servers.
	// +optional
	PlacementGroup *PlacementGroup `json:"placementGroup,omitempty"`
}

// PlacementGroupType is the Hetzner placement group type.
// +kubebuilder:validation:Enum=spread
type PlacementGroupType string

// PlacementGroupSpread spreads the servers of the group over hosts.
const PlacementGroupSpread PlacementGroupType = "spread"

// PlacementGroup creates a group per run key (type) or references an existing
// one (use). Exactly one of type and use is set.
// +kubebuilder:validation:XValidation:rule="has(self.type) != has(self.use)",message="set exactly one of type and use"
type PlacementGroup struct {
	// type of the placement groups SCarpenter creates.
	// +optional
	Type PlacementGroupType `json:"type,omitempty"`

	// use references an existing placement group, which is never modified or
	// deleted; the run key then only separates NodeClaims.
	// +optional
	Use *PlacementGroupRef `json:"use,omitempty"`
}

// PlacementGroupRef names an existing placement group.
type PlacementGroupRef struct {
	// name of the placement group.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +required
	Name string `json:"name"`
}

// GetRegions returns the location allow-list (empty: every location).
func (s *Settings) GetRegions() []string {
	if s == nil {
		return nil
	}
	return s.Regions
}
