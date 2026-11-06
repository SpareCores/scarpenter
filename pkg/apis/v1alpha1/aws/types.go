/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the AWS block of an SCNodeClass (spec.vendors.aws): layer 2 of
// the node settings, overriding the driver's built-in defaults for every AWS
// node (00a section 3).
type Settings struct {
	// regions is the allow-list of AWS regions; empty means every active
	// region the driver can use.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// storage overrides the built-in storage defaults (gp3, 64 GiB, encrypted
	// root volume; no extra volumes).
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for AWS.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// metadataOptions configures the instance metadata service. Built-in
	// default: IMDSv2 only, hop limit 1.
	// +optional
	MetadataOptions *MetadataOptions `json:"metadataOptions,omitempty"`

	// instanceProfile is the name of an IAM instance profile attached to the
	// nodes. Empty by default: nodes hold no cloud credentials.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	InstanceProfile string `json:"instanceProfile,omitempty"`

	// imageOwner is the AWS account that owns the Flatcar images. Empty means
	// the official Flatcar account.
	// +kubebuilder:validation:Pattern=`^([0-9]{12})?$`
	// +optional
	ImageOwner string `json:"imageOwner,omitempty"`

	// resources says how the AWS resources a node depends on are provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the AWS resource kinds of 00a section 9. Each kind has at most
// one of use, create and forbid.
type Resources struct {
	// subnets nodes are launched into.
	// +optional
	Subnets *common.Resource `json:"subnets,omitempty"`

	// securityGroups attached to nodes.
	// +optional
	SecurityGroups *common.Resource `json:"securityGroups,omitempty"`

	// placementGroups nodes are launched into (see the placement profiles).
	// +optional
	PlacementGroups *common.Resource `json:"placementGroups,omitempty"`
}

// VolumeType is an EBS volume type.
// +kubebuilder:validation:Enum=gp2;gp3;io1;io2;st1;sc1;standard
type VolumeType string

// EBS volume types.
const (
	VolumeTypeGP2      VolumeType = "gp2"
	VolumeTypeGP3      VolumeType = "gp3"
	VolumeTypeIO1      VolumeType = "io1"
	VolumeTypeIO2      VolumeType = "io2"
	VolumeTypeST1      VolumeType = "st1"
	VolumeTypeSC1      VolumeType = "sc1"
	VolumeTypeStandard VolumeType = "standard"
)

// Storage is the AWS storage block: the settings of the root volume, the extra
// EBS volumes and the instance-local disks. It is the vendor block's storage
// (layer 2) and the AWS block of a storage profile (layer 3).
type Storage struct {
	// root configures the root EBS volume.
	// +optional
	Root *RootVolume `json:"root,omitempty"`

	// data lists extra EBS volumes created with the instance.
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	// +optional
	Data []DataVolume `json:"data,omitempty"`

	// localStorage configures the instance store disks of types that have
	// them.
	// +optional
	LocalStorage *common.LocalStorage `json:"localStorage,omitempty"`
}

// RootVolume configures the root EBS volume.
// +kubebuilder:validation:XValidation:rule="!has(self.iops) || !has(self.volumeType) || self.volumeType in ['gp3', 'io1', 'io2']",message="iops is only valid for gp3, io1 and io2"
// +kubebuilder:validation:XValidation:rule="!has(self.throughput) || !has(self.volumeType) || self.volumeType == 'gp3'",message="throughput is only valid for gp3"
// +kubebuilder:validation:XValidation:rule="!has(self.volumeType) || !(self.volumeType in ['io1', 'io2']) || has(self.iops)",message="io1 and io2 need iops"
// +kubebuilder:validation:XValidation:rule="!has(self.kmsKeyID) || !has(self.encrypted) || self.encrypted",message="kmsKeyID needs encrypted"
type RootVolume struct {
	// volumeType is the EBS volume type. Built-in default gp3.
	// +optional
	VolumeType VolumeType `json:"volumeType,omitempty"`

	// volumeSize of the root volume, e.g. 64Gi. Built-in default 64Gi.
	// +optional
	VolumeSize *resource.Quantity `json:"volumeSize,omitempty"`

	// iops provisioned for gp3, io1 and io2 volumes.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=256000
	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// throughput in MiB/s provisioned for gp3 volumes.
	// +kubebuilder:validation:Minimum=125
	// +kubebuilder:validation:Maximum=2000
	// +optional
	Throughput *int32 `json:"throughput,omitempty"`

	// encrypted encrypts the volume. Built-in default true.
	// +optional
	Encrypted *bool `json:"encrypted,omitempty"`

	// kmsKeyID is the KMS key (ID, alias or ARN) used for encryption; the
	// account's default EBS key when empty.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	KMSKeyID string `json:"kmsKeyID,omitempty"`
}

// DataVolume is an extra EBS volume: EBS settings plus the guest side
// (name, filesystem, mount path).
// +kubebuilder:validation:XValidation:rule="!has(self.iops) || self.volumeType in ['gp3', 'io1', 'io2']",message="iops is only valid for gp3, io1 and io2"
// +kubebuilder:validation:XValidation:rule="!has(self.throughput) || self.volumeType == 'gp3'",message="throughput is only valid for gp3"
// +kubebuilder:validation:XValidation:rule="!(self.volumeType in ['io1', 'io2']) || has(self.iops)",message="io1 and io2 need iops"
// +kubebuilder:validation:XValidation:rule="!has(self.kmsKeyID) || !has(self.encrypted) || self.encrypted",message="kmsKeyID needs encrypted"
type DataVolume struct {
	common.GuestVolume `json:",inline"`

	// volumeType is the EBS volume type.
	// +required
	VolumeType VolumeType `json:"volumeType"`

	// volumeSize of the volume, e.g. 500Gi.
	// +required
	VolumeSize resource.Quantity `json:"volumeSize"`

	// iops provisioned for gp3, io1 and io2 volumes.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=256000
	// +optional
	IOPS *int32 `json:"iops,omitempty"`

	// throughput in MiB/s provisioned for gp3 volumes.
	// +kubebuilder:validation:Minimum=125
	// +kubebuilder:validation:Maximum=2000
	// +optional
	Throughput *int32 `json:"throughput,omitempty"`

	// encrypted encrypts the volume. Built-in default true.
	// +optional
	Encrypted *bool `json:"encrypted,omitempty"`

	// kmsKeyID is the KMS key (ID, alias or ARN) used for encryption.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	KMSKeyID string `json:"kmsKeyID,omitempty"`
}

// HTTPTokens is the IMDS token requirement.
// +kubebuilder:validation:Enum=required;optional
type HTTPTokens string

// MetadataOptions configures the EC2 instance metadata service.
type MetadataOptions struct {
	// httpTokens: required allows IMDSv2 only. Built-in default required.
	// +optional
	HTTPTokens HTTPTokens `json:"httpTokens,omitempty"`

	// httpPutResponseHopLimit is the IMDS hop limit; 1 keeps the service out
	// of reach of containers. Built-in default 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	// +optional
	HTTPPutResponseHopLimit *int32 `json:"httpPutResponseHopLimit,omitempty"`
}

// Tenancy of an instance.
// +kubebuilder:validation:Enum=default;dedicated
type Tenancy string

// Placement is the AWS block of a placement profile: a placement group and the
// tenancy.
type Placement struct {
	// placementGroup is the placement construct nodes join; it needs a run key
	// (label placement-group) to be created per run.
	// +optional
	PlacementGroup *PlacementGroup `json:"placementGroup,omitempty"`

	// tenancy of the instances: dedicated runs them on single-tenant
	// hardware.
	// +optional
	Tenancy Tenancy `json:"tenancy,omitempty"`
}

// PlacementGroupStrategy is the EC2 placement strategy.
// +kubebuilder:validation:Enum=cluster;partition;spread
type PlacementGroupStrategy string

// PlacementGroup creates a group per run key (strategy) or references an
// existing one (use). Exactly one of strategy and use is set.
// +kubebuilder:validation:XValidation:rule="has(self.strategy) != has(self.use)",message="set exactly one of strategy and use"
// +kubebuilder:validation:XValidation:rule="!has(self.partitionCount) || (has(self.strategy) && self.strategy == 'partition')",message="partitionCount is only valid with strategy partition"
type PlacementGroup struct {
	// strategy of the placement groups SCarpenter creates.
	// +optional
	Strategy PlacementGroupStrategy `json:"strategy,omitempty"`

	// partitionCount is the number of partitions (strategy partition).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=7
	// +optional
	PartitionCount *int32 `json:"partitionCount,omitempty"`

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

// Network is the AWS block of a network profile.
type Network struct {
	// enaExpress configures ENA Express on supported instance types.
	// +optional
	ENAExpress *ENAExpress `json:"enaExpress,omitempty"`
}

// ENAExpress configures ENA Express.
type ENAExpress struct {
	// enabled turns ENA Express on.
	// +required
	Enabled bool `json:"enabled"`

	// udp enables ENA Express for UDP traffic as well.
	// +optional
	UDP *bool `json:"udp,omitempty"`
}
