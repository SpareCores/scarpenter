/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"

// Settings is the Vultr block of an SCNodeClass (spec.vendors.vultr): layer 2 of
// the node settings (00a section 3). One API key is one account, so the block
// names no scope; the key comes from the controller's Secret.
type Settings struct {
	// regions is the allow-list of Vultr region IDs (ewr, fra, ...); empty
	// means every region the API lists. Vultr has no zones: the zone of a node
	// is its region.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// networking overrides the generic spec.networking for Vultr.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// resources says how the Vultr resources a node depends on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the Vultr resource kinds (plan 20, section 3.2). Each kind has
// at most one of use, create and forbid. Vultr snapshots, firewall groups and
// VPCs carry a description and no tags, so `use.tags` is rejected on every kind.
type Resources struct {
	// images brings the Flatcar snapshot with the one selector of every
	// resource kind: snapshot IDs (global in the account: the key "*" is every
	// region), or a pattern for the description. Optional: without it the
	// driver finds the snapshot described flatcar-<channel>-<version>-<arch>.
	// Snapshots cannot be shared between accounts, so SCarpenter never creates
	// them: images take use only, and the snapshot must say which Flatcar
	// release it is (by its description) or the region's images are not ready.
	// +kubebuilder:validation:XValidation:rule="has(self.use) && !has(self.create) && !(has(self.forbid) && self.forbid)",message="images accepts use only: it has no create and no forbid"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.select) || self.use.select != 'all'",message="images cannot use select all: a launch needs one image"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.tags)",message="tags are not supported: Vultr snapshots have a description and no tags"
	// +optional
	Images *common.Resource `json:"images,omitempty"`

	// firewallGroups attached to nodes. Inbound is open without a group, so
	// without one SCarpenter creates a group per NodeClass and region with the
	// rules of spec.networking.inbound (none: deny all). A group is global in
	// the account; the same ID serves every region.
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.tags)",message="tags are not supported: Vultr firewall groups have a description and no tags"
	// +optional
	FirewallGroups *common.Resource `json:"firewallGroups,omitempty"`

	// networks (VPCs) nodes are attached to, per region. Without one nodes use
	// the public network only. isolation: nodeclass needs a VPC.
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.tags)",message="tags are not supported: Vultr VPCs have a description and no tags"
	// +optional
	Networks *common.Resource `json:"networks,omitempty"`
}

// Storage is the Vultr block of a storage profile. It is empty in v1: the plan's
// disk is the root disk and extra block storage volumes are deferred, so a
// storage profile with a vultr block makes every Vultr offering unavailable
// with a reason.
type Storage struct{}

// Network is the Vultr block of a network profile. Empty in v1: Vultr's create
// call has no NIC features.
type Network struct{}

// Placement is the Vultr block of a placement profile. Empty in v1: Vultr has
// no placement groups or tenancy choice.
type Placement struct{}

// GetRegions returns the region allow-list (empty: every region).
func (s *Settings) GetRegions() []string {
	if s == nil {
		return nil
	}
	return s.Regions
}
