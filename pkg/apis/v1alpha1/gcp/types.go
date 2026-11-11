/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the GCP block of an SCNodeClass (spec.vendors.gcp): layer 2 of the
// node settings, overriding the driver's built-in defaults for every GCP node
// (00a section 3).
type Settings struct {
	// regions is the allow-list of GCP regions; empty means every region the
	// driver can read.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// project is the project of the nodes. The controller targets one project
	// (--gcp-project); when set here it must equal it, so that a mismatch is a
	// visible condition and not a launch error.
	// +kubebuilder:validation:Pattern=`^([a-z][-a-z0-9.:]{4,61}[a-z0-9])?$`
	// +optional
	Project string `json:"project,omitempty"`

	// imageProject is the project that owns the Flatcar images. Empty means the
	// public Flatcar project (amd64) and the Spare Cores project (arm64).
	// +kubebuilder:validation:Pattern=`^([a-z][-a-z0-9.:]{4,61}[a-z0-9])?$`
	// +optional
	ImageProject string `json:"imageProject,omitempty"`

	// storage overrides the built-in storage defaults (pd-balanced, 64 GiB root
	// disk; Hyperdisk for the series that boot only from it; no extra disks).
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for GCP.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// serviceAccount is the e-mail of a service account attached to the nodes
	// (needs iam.serviceAccounts.actAs). Empty by default: nodes hold no cloud
	// credentials.
	// +kubebuilder:validation:MaxLength=254
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// metadata is extra instance metadata. user-data, ssh-keys, enable-oslogin
	// and block-project-ssh-keys are reserved.
	// +kubebuilder:validation:MaxProperties=32
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`

	// resources says how the GCP resources a node depends on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the GCP resource kinds of 00a section 9. Each kind has at most
// one of use, create and forbid.
type Resources struct {
	// subnets nodes are launched into (the network is implied by the subnet).
	// +optional
	Subnets *SubnetResource `json:"subnets,omitempty"`

	// firewalls are network tags: the tag of the rules that allow the nodes'
	// traffic. ids are tag names.
	// +optional
	Firewalls *common.Resource `json:"firewalls,omitempty"`

	// resourcePolicies are the placement policies nodes are launched into (see
	// the placement profiles).
	// +optional
	ResourcePolicies *common.Resource `json:"resourcePolicies,omitempty"`

	// images brings the Flatcar boot images with the one selector of every
	// resource kind (plan 14 A1): image IDs under "*" (images are global), a
	// name, a family or native filters. Optional: without it the driver finds the
	// pinned release by name in the image project. Whatever the lookup, the
	// image's name, family or labels must say the pinned channel and version, or
	// the images are not ready. Images take use only.
	// +kubebuilder:validation:XValidation:rule="has(self.use) && !has(self.create) && !(has(self.forbid) && self.forbid)",message="images accepts use only: it has no create and no forbid"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.select) || self.use.select != 'all'",message="images cannot use select all: a launch needs one image"
	// +optional
	Images *common.Resource `json:"images,omitempty"`
}

// SubnetResource is the resource of the subnets kind: use, create and forbid
// as on every kind, with a create that can also say which private range the
// managed subnetworks are taken from.
//
// +kubebuilder:validation:XValidation:rule="(has(self.use) ? 1 : 0) + (has(self.create) ? 1 : 0) + ((has(self.forbid) && self.forbid) ? 1 : 0) <= 1",message="at most one of use, create and forbid may be set"
type SubnetResource struct {
	// use brings existing subnetworks: validated per region, never modified
	// and never deleted.
	// +optional
	Use *common.ResourceUse `json:"use,omitempty"`

	// create lets SCarpenter create and own the network and its subnetworks.
	// +optional
	Create *SubnetCreate `json:"create,omitempty"`

	// forbid opts the kind out of auto-creation.
	// +optional
	Forbid bool `json:"forbid,omitempty"`
}

// SubnetCreate configures the managed network: its lifecycle and timing as on
// every kind, and the pool the subnetworks are taken from.
type SubnetCreate struct {
	common.ResourceCreate `json:",inline"`

	// pool is the private range the managed subnetworks (one /20 per region) are
	// taken from: each is the first /20 of the pool that overlaps no subnetwork of
	// the managed network. Default 10.240.0.0/12. Set it when a network your own
	// network is peered with, or reaches by VPN, uses that range.
	// +kubebuilder:validation:MaxLength=18
	// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self).ip().family() == 4 && cidr(self).masked() == cidr(self) && cidr(self).prefixLength() <= 20 && (cidr('10.0.0.0/8').containsCIDR(self) || cidr('172.16.0.0/12').containsCIDR(self) || cidr('192.168.0.0/16').containsCIDR(self))",message="pool must be an IPv4 CIDR inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16, with a prefix length of at most 20"
	// +optional
	Pool string `json:"pool,omitempty"`
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

// DiskType is a Compute Engine disk type.
// +kubebuilder:validation:Enum=pd-standard;pd-balanced;pd-ssd;hyperdisk-balanced;hyperdisk-extreme;hyperdisk-throughput;hyperdisk-ml
type DiskType string

// Disk types.
const (
	DiskTypePDStandard         DiskType = "pd-standard"
	DiskTypePDBalanced         DiskType = "pd-balanced"
	DiskTypePDSSD              DiskType = "pd-ssd"
	DiskTypeHyperdiskBalanced  DiskType = "hyperdisk-balanced"
	DiskTypeHyperdiskExtreme   DiskType = "hyperdisk-extreme"
	DiskTypeHyperdiskThroughpt DiskType = "hyperdisk-throughput"
	DiskTypeHyperdiskML        DiskType = "hyperdisk-ml"
)

// Storage is the GCP storage block: the root disk, the extra disks and the
// instance-local SSDs. It is the vendor block's storage (layer 2) and the GCP
// block of a storage profile (layer 3).
type Storage struct {
	// root configures the boot disk.
	// +optional
	Root *RootDisk `json:"root,omitempty"`

	// data lists extra disks created with the instance.
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	// +optional
	Data []DataDisk `json:"data,omitempty"`

	// localStorage configures the local SSDs of types that include them.
	// +optional
	LocalStorage *common.LocalStorage `json:"localStorage,omitempty"`
}

// RootDisk configures the boot disk.
// +kubebuilder:validation:XValidation:rule="!has(self.provisionedIops) || !has(self.diskType) || self.diskType.startsWith('hyperdisk-')",message="provisionedIops is only valid for Hyperdisk"
// +kubebuilder:validation:XValidation:rule="!has(self.provisionedThroughput) || !has(self.diskType) || self.diskType.startsWith('hyperdisk-')",message="provisionedThroughput is only valid for Hyperdisk"
type RootDisk struct {
	// diskType of the boot disk. Built-in default pd-balanced, hyperdisk-balanced
	// for the series that boot only from Hyperdisk.
	// +optional
	DiskType DiskType `json:"diskType,omitempty"`

	// sizeGb of the boot disk in GiB (GCP's "GB"). Built-in default 64.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=65536
	// +optional
	SizeGb *int64 `json:"sizeGb,omitempty"`

	// provisionedIops for Hyperdisk.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=350000
	// +optional
	ProvisionedIops *int64 `json:"provisionedIops,omitempty"`

	// provisionedThroughput in MiB/s for Hyperdisk.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +optional
	ProvisionedThroughput *int64 `json:"provisionedThroughput,omitempty"`

	// kmsKeyName is the customer-managed encryption key
	// (projects/.../cryptoKeys/...); Google's own key when empty.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	KMSKeyName string `json:"kmsKeyName,omitempty"`
}

// DataDisk is an extra disk: Compute Engine settings plus the guest side (name,
// filesystem, mount path). The name is the disk's device name, so the guest
// path /dev/disk/by-id/google-<name> is known before the launch.
// +kubebuilder:validation:XValidation:rule="!has(self.provisionedIops) || self.diskType.startsWith('hyperdisk-')",message="provisionedIops is only valid for Hyperdisk"
// +kubebuilder:validation:XValidation:rule="!has(self.provisionedThroughput) || self.diskType.startsWith('hyperdisk-')",message="provisionedThroughput is only valid for Hyperdisk"
type DataDisk struct {
	common.GuestVolume `json:",inline"`

	// diskType of the disk.
	// +required
	DiskType DiskType `json:"diskType"`

	// sizeGb of the disk in GiB.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65536
	// +required
	SizeGb int64 `json:"sizeGb"`

	// provisionedIops for Hyperdisk.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=350000
	// +optional
	ProvisionedIops *int64 `json:"provisionedIops,omitempty"`

	// provisionedThroughput in MiB/s for Hyperdisk.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +optional
	ProvisionedThroughput *int64 `json:"provisionedThroughput,omitempty"`

	// kmsKeyName is the customer-managed encryption key.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	KMSKeyName string `json:"kmsKeyName,omitempty"`
}

// Placement is the GCP block of a placement profile: a resource policy.
type Placement struct {
	// resourcePolicy is the placement policy nodes join. A policy SCarpenter
	// creates needs a run key (label placement-group): one policy per run.
	// +optional
	ResourcePolicy *ResourcePolicy `json:"resourcePolicy,omitempty"`
}

// ResourcePolicy creates a policy per run key (groupPlacement or spread) or
// references an existing one (use). Exactly one of the three is set.
// +kubebuilder:validation:XValidation:rule="(has(self.groupPlacement) ? 1 : 0) + (has(self.spread) ? 1 : 0) + (has(self.use) ? 1 : 0) == 1",message="set exactly one of groupPlacement, spread and use"
type ResourcePolicy struct {
	// groupPlacement places the instances close together (compact placement).
	// +optional
	GroupPlacement *GroupPlacement `json:"groupPlacement,omitempty"`

	// spread spreads the instances over availability domains.
	// +optional
	Spread *Spread `json:"spread,omitempty"`

	// use references an existing policy, which is never modified or deleted.
	// +optional
	Use *ResourcePolicyRef `json:"use,omitempty"`
}

// Collocation of a group placement policy.
// +kubebuilder:validation:Enum=COLLOCATED
type Collocation string

// GroupPlacement is a compact placement policy.
type GroupPlacement struct {
	// collocation of the instances.
	// +kubebuilder:default=COLLOCATED
	// +optional
	Collocation Collocation `json:"collocation,omitempty"`
}

// Spread is a spread placement policy.
type Spread struct {
	// availabilityDomainCount is the number of availability domains.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	// +required
	AvailabilityDomainCount int32 `json:"availabilityDomainCount"`
}

// ResourcePolicyRef names an existing resource policy of the region.
type ResourcePolicyRef struct {
	// name of the resource policy.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`
}

// NicType of the network interface.
// +kubebuilder:validation:Enum=GVNIC;VIRTIO_NET
type NicType string

// Network is the GCP block of a network profile.
type Network struct {
	// nicType selects the virtual NIC; unset leaves the series' default.
	// +optional
	NicType NicType `json:"nicType,omitempty"`

	// tier1 turns on Tier 1 networking (higher egress bandwidth) on the series
	// that support it.
	// +optional
	Tier1 *bool `json:"tier1,omitempty"`
}

// GetRegions returns the region allow-list (empty: every region).
func (s *Settings) GetRegions() []string {
	if s == nil {
		return nil
	}
	return s.Regions
}

// GetImageOwner returns the project that owns the Flatcar images ("" means the
// defaults: the public Flatcar project, and the Spare Cores project for arm64).
func (s *Settings) GetImageOwner() string {
	if s == nil {
		return ""
	}
	return s.ImageProject
}
