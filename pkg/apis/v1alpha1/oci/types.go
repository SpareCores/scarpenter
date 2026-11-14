/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the OCI block of an SCNodeClass (spec.vendors.oci): layer 2 of the
// node settings, overriding the driver's built-in defaults for every OCI node
// (00a section 3). The compartment is not here: it is the controller's
// (--oci-compartment), one for every NodeClass.
type Settings struct {
	// regions is the allow-list of OCI regions (eu-frankfurt-1,
	// us-gov-ashburn-1); empty means every region the tenancy has subscribed.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Pattern=`^[a-z]{2}(-[a-z]+)+-[0-9]+$`
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// storage overrides the built-in storage defaults (a 64 GiB boot volume of
	// the balanced performance level). Data volumes and local storage are not
	// supported in v1.
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for OCI. publicIPv4
	// defaults to true; inbound rules become rules of the network security
	// group.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// metadata is extra instance metadata. user_data and ssh_authorized_keys are
	// reserved: the first is the Ignition and the nodes carry no SSH keys.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="!('user_data' in self) && !('ssh_authorized_keys' in self)",message="the metadata keys user_data and ssh_authorized_keys are reserved"
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`

	// resources says how the OCI resources a node depends on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the OCI resource kinds of 00a section 9. Each kind has at most
// one of use, create and forbid. ids are keyed by region (OCIDs are regional: no
// "*") and hold OCIDs of the kind.
type Resources struct {
	// subnets nodes are launched into (the network, the VCN, is implied by the
	// subnet). A regional subnet serves every availability domain. Without use,
	// SCarpenter creates a VCN with an internet gateway, a route table, a minimal
	// security list and one regional public subnet per region (create).
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.ids) || self.use.ids.all(r, r != '*' && self.use.ids[r].all(id, id.matches('^ocid1[.]subnet[.][a-z0-9]+[.][a-z0-9-]*([.][a-z0-9]+)+$')))",message="use.ids are subnet OCIDs per region name, never *"
	// +optional
	Subnets *SubnetResource `json:"subnets,omitempty"`

	// networkSecurityGroups are attached to the nodes' VNICs (at most five per
	// VNIC), in the VCN of the subnet. Without use, SCarpenter creates one group
	// per NodeClass and region with the rules of networking.inbound.
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.ids) || self.use.ids.all(r, r != '*' && self.use.ids[r].all(id, id.matches('^ocid1[.]networksecuritygroup[.][a-z0-9]+[.][a-z0-9-]*([.][a-z0-9]+)+$')))",message="use.ids are network security group OCIDs per region name, never *"
	// +optional
	NetworkSecurityGroups *common.Resource `json:"networkSecurityGroups,omitempty"`

	// images brings the Flatcar images with the one selector of every resource
	// kind (plan 14 A1). Optional: without it the driver finds the images of the
	// pinned release in the controller's compartment by the name
	// flatcar-<channel>-<version>-<arch>. Images are regional: IDs go under the
	// region name, never "*". Whatever the lookup, the image's tags or operating
	// system version must say the pinned channel and version, or the images are
	// not ready. Images take use only.
	// +kubebuilder:validation:XValidation:rule="has(self.use) && !has(self.create) && !(has(self.forbid) && self.forbid)",message="images accepts use only: it has no create and no forbid"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.select) || self.use.select != 'all'",message="images cannot use select all: a launch needs one image"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.ids) || self.use.ids.all(r, r != '*' && self.use.ids[r].all(id, id.matches('^ocid1[.]image[.][a-z0-9]+[.][a-z0-9-]*([.][a-z0-9]+)+$')))",message="use.ids are image OCIDs per region name, never *"
	// +optional
	Images *common.Resource `json:"images,omitempty"`
}

// SubnetResource is the resource of the subnets kind: use, create and forbid as
// on every kind, with a create that can also say the managed VCN's range.
//
// +kubebuilder:validation:XValidation:rule="(has(self.use) ? 1 : 0) + (has(self.create) ? 1 : 0) + ((has(self.forbid) && self.forbid) ? 1 : 0) <= 1",message="at most one of use, create and forbid may be set"
type SubnetResource struct {
	// use brings existing subnets: validated per region, never modified and
	// never deleted.
	// +optional
	Use *common.ResourceUse `json:"use,omitempty"`

	// create lets SCarpenter create and own the VCN and its subnet.
	// +optional
	Create *SubnetCreate `json:"create,omitempty"`

	// forbid opts the kind out of auto-creation.
	// +optional
	Forbid bool `json:"forbid,omitempty"`
}

// SubnetCreate configures the managed network: its lifecycle and timing as on
// every kind, and the range of the VCN.
type SubnetCreate struct {
	common.ResourceCreate `json:",inline"`

	// cidr is the IPv4 range of the VCN SCarpenter creates (one regional subnet,
	// the first /20 of it). Default 10.240.0.0/16. Set it when a network your own
	// network is peered with, or reaches by VPN, uses that range.
	// +kubebuilder:validation:MaxLength=18
	// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self).ip().family() == 4 && cidr(self).masked() == cidr(self) && cidr(self).prefixLength() <= 20 && (cidr('10.0.0.0/8').containsCIDR(self) || cidr('172.16.0.0/12').containsCIDR(self) || cidr('192.168.0.0/16').containsCIDR(self))",message="cidr must be an IPv4 CIDR inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16, with a prefix length of at most 20"
	// +optional
	CIDR string `json:"cidr,omitempty"`
}

// Common is the subnets resource as the generic resource engine sees it.
func (r *SubnetResource) Common() *common.Resource {
	if r == nil {
		return nil
	}
	out := &common.Resource{Use: r.Use, Forbid: r.Forbid}
	if r.Create != nil {
		c := r.Create.ResourceCreate
		out.Create = &c
	}
	return out
}

// Performance is the performance level of the boot volume: Keeper's storage IDs
// of OCI; the level sets the volume performance units (VPUs) per GB. The lower-cost
// level (0 VPUs) is for block volumes only: OCI does not take it for a boot volume.
// +kubebuilder:validation:Enum=balanced;higher-performance;ultra-high-performance
type Performance string

// Performance levels of a boot volume (10, 20 and 30 to 120 VPUs per GB).
const (
	PerformanceBalanced  Performance = "balanced"
	PerformanceHigher    Performance = "higher-performance"
	PerformanceUltraHigh Performance = "ultra-high-performance"
)

// Storage is the OCI storage block: the boot volume. It is the vendor block's
// storage (layer 2). Data volumes and local storage are not supported in v1.
type Storage struct {
	// root configures the boot volume.
	// +optional
	Root *RootVolume `json:"root,omitempty"`
}

// RootVolume configures the boot volume.
// +kubebuilder:validation:XValidation:rule="!has(self.vpusPerGb) || (has(self.performance) && self.performance == 'ultra-high-performance')",message="vpusPerGb is only valid with performance ultra-high-performance"
type RootVolume struct {
	// sizeGb of the boot volume in GB (OCI's GB). Built-in default 64; the
	// Flatcar image needs about 47.
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=32768
	// +optional
	SizeGb *int64 `json:"sizeGb,omitempty"`

	// performance level of the volume. Built-in default balanced (10 VPUs per
	// GB).
	// +optional
	Performance Performance `json:"performance,omitempty"`

	// vpusPerGb of an ultra-high-performance volume: 30 to 120 in steps of 10,
	// default 30.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=120
	// +kubebuilder:validation:MultipleOf=10
	// +optional
	VpusPerGb *int64 `json:"vpusPerGb,omitempty"`

	// kmsKeyId is the OCID of a Vault key that encrypts the volume; default
	// the Oracle-managed key.
	// +kubebuilder:validation:Pattern=`^(ocid1[.]key[.][a-z0-9]+[.][a-z0-9-]*([.][a-z0-9]+)+)?$`
	// +optional
	KmsKeyID string `json:"kmsKeyId,omitempty"`
}

// GetRegions returns the region allow-list (empty: every subscribed region).
func (s *Settings) GetRegions() []string {
	if s == nil {
		return nil
	}
	return s.Regions
}
