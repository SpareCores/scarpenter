/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// Settings is the Alibaba Cloud block of an SCNodeClass (spec.vendors.alicloud):
// layer 2 of the node settings, overriding the driver's built-in defaults for
// every Alibaba Cloud node (00a section 3).
type Settings struct {
	// regions is the allow-list of Alibaba Cloud region IDs (ap-southeast-1,
	// eu-central-1); empty means every region the account can use.
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// imageOwner is the account UID that owns the Flatcar images. Empty means
	// the account of the controller's credentials (images created there with
	// ImportImage or CopyImage, the supported path of v1); set it for images
	// another account shares or publishes.
	// +kubebuilder:validation:Pattern=`^([0-9]{1,20})?$`
	// +optional
	ImageOwner string `json:"imageOwner,omitempty"`

	// storage overrides the built-in storage defaults (a cloud_essd PL0 system
	// disk of 64 GiB, not encrypted; no data disks).
	// +optional
	Storage *Storage `json:"storage,omitempty"`

	// networking overrides the generic spec.networking for Alibaba Cloud.
	// +optional
	Networking *common.NetworkingOverride `json:"networking,omitempty"`

	// internetMaxBandwidthOut is the peak public outbound bandwidth in Mbit/s;
	// 0 means no public IPv4 address (the same as networking.publicIPv4: false).
	// Built-in default 100.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	InternetMaxBandwidthOut *int32 `json:"internetMaxBandwidthOut,omitempty"`

	// internetChargeType is how the public bandwidth is billed. Built-in
	// default PayByTraffic. PayByBandwidth makes the offering's price incomplete.
	// +optional
	InternetChargeType InternetChargeType `json:"internetChargeType,omitempty"`

	// metadataOptions configures the instance metadata service. Built-in
	// default: httpTokens optional, because Ignition's aliyun provider reads
	// the user data without a token (see the reference for the consequence and
	// the hardening that closes it).
	// +optional
	MetadataOptions *MetadataOptions `json:"metadataOptions,omitempty"`

	// resourceGroupId is the enterprise resource group of the resources created
	// for the nodes. Empty means the account's default group.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ResourceGroupID string `json:"resourceGroupId,omitempty"`

	// resources says how the Alibaba Cloud resources a node depends on are
	// provided.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the Alibaba Cloud resource kinds of 00a section 9. Each kind has
// at most one of use, create and forbid.
type Resources struct {
	// subnets are the vSwitches nodes are launched into (with their VPC).
	// +optional
	Subnets *common.Resource `json:"subnets,omitempty"`

	// securityGroups attached to nodes (normal groups in the vSwitches' VPC).
	// +optional
	SecurityGroups *common.Resource `json:"securityGroups,omitempty"`

	// deploymentSets nodes are launched into (see the placement profiles).
	// +optional
	DeploymentSets *common.Resource `json:"deploymentSets,omitempty"`

	// images brings the Flatcar boot images with the one selector of every
	// resource kind: explicit image IDs per region, a name pattern, tags or
	// native filters. Optional: without it the driver finds the pinned release
	// by the name convention Flatcar-<channel>-<version>-<arch> among the
	// images of imageOwner. Whatever the lookup, the image's name or tags must
	// say the pinned channel and version, or the region's images are not ready.
	// Images take use only.
	// +kubebuilder:validation:XValidation:rule="has(self.use) && !has(self.create) && !(has(self.forbid) && self.forbid)",message="images accepts use only: it has no create and no forbid"
	// +kubebuilder:validation:XValidation:rule="!has(self.use) || !has(self.use.select) || self.use.select != 'all'",message="images cannot use select all: a launch needs one image"
	// +optional
	Images *common.Resource `json:"images,omitempty"`
}

// InternetChargeType is the billing method of the public bandwidth.
// +kubebuilder:validation:Enum=PayByTraffic;PayByBandwidth
type InternetChargeType string

// Internet charge types.
const (
	PayByTraffic   InternetChargeType = "PayByTraffic"
	PayByBandwidth InternetChargeType = "PayByBandwidth"
)

// DiskCategory is an ECS disk category.
// +kubebuilder:validation:Enum=cloud_essd;cloud_auto;cloud_essd_entry;cloud_efficiency;cloud_ssd
type DiskCategory string

// ECS disk categories.
const (
	DiskCategoryESSD       DiskCategory = "cloud_essd"
	DiskCategoryAuto       DiskCategory = "cloud_auto"
	DiskCategoryESSDEntry  DiskCategory = "cloud_essd_entry"
	DiskCategoryEfficiency DiskCategory = "cloud_efficiency"
	DiskCategorySSD        DiskCategory = "cloud_ssd"
)

// PerformanceLevel is the performance level of an ESSD.
// +kubebuilder:validation:Enum=PL0;PL1;PL2;PL3
type PerformanceLevel string

// Storage is the Alibaba Cloud storage block: the system disk and the extra data
// disks created with the instance. It is the vendor block's storage (layer 2)
// and the alicloud block of a storage profile (layer 3).
// +kubebuilder:validation:XValidation:rule="!has(self.localStorage) || self.localStorage.policy == 'ignore'",message="localStorage policy must be ignore: local disks are not supported on Alibaba Cloud yet"
type Storage struct {
	// root configures the system disk.
	// +optional
	Root *Disk `json:"root,omitempty"`

	// data lists extra disks created with the instance (all deleted with it).
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +optional
	Data []DataDisk `json:"data,omitempty"`

	// localStorage is accepted so that a profile written for a vendor with
	// local disks is not pruned silently, but local disks are not supported on
	// Alibaba Cloud yet (the i and d families are deferred): only the policy
	// ignore is accepted, any other is refused when the object is applied.
	// +optional
	LocalStorage *common.LocalStorage `json:"localStorage,omitempty"`
}

// Disk configures the system disk.
// +kubebuilder:validation:XValidation:rule="!has(self.performanceLevel) || !has(self.category) || self.category == 'cloud_essd'",message="performanceLevel is only valid for cloud_essd"
// +kubebuilder:validation:XValidation:rule="(!has(self.provisionedIops) && !has(self.burstingEnabled)) || !has(self.category) || self.category == 'cloud_auto'",message="provisionedIops and burstingEnabled are only valid for cloud_auto"
// +kubebuilder:validation:XValidation:rule="!has(self.kmsKeyID) || !has(self.encrypted) || self.encrypted",message="kmsKeyID needs encrypted"
type Disk struct {
	// category is the disk category. Built-in default cloud_essd.
	// +optional
	Category DiskCategory `json:"category,omitempty"`

	// performanceLevel of a cloud_essd disk. Built-in default PL0.
	// +optional
	PerformanceLevel PerformanceLevel `json:"performanceLevel,omitempty"`

	// size of the disk, e.g. 64Gi. Built-in default 64Gi for the system disk.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// provisionedIops of a cloud_auto disk.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50000
	// +optional
	ProvisionedIops *int64 `json:"provisionedIops,omitempty"`

	// burstingEnabled turns performance bursting on for a cloud_auto disk.
	// +optional
	BurstingEnabled *bool `json:"burstingEnabled,omitempty"`

	// encrypted encrypts the disk. Built-in default false: ECS refuses encrypted
	// system disks in some zones (Singapore zone A, Hong Kong zone D), and true
	// takes those zones out of the offerings.
	// +optional
	Encrypted *bool `json:"encrypted,omitempty"`

	// kmsKeyID is the KMS key used for encryption; the account's default
	// service key when empty.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	KMSKeyID string `json:"kmsKeyID,omitempty"`
}

// DataDisk is an extra disk: ECS settings plus the guest side (name,
// filesystem, mount path).
// +kubebuilder:validation:XValidation:rule="!has(self.performanceLevel) || !has(self.category) || self.category == 'cloud_essd'",message="performanceLevel is only valid for cloud_essd"
// +kubebuilder:validation:XValidation:rule="(!has(self.provisionedIops) && !has(self.burstingEnabled)) || (has(self.category) && self.category == 'cloud_auto')",message="provisionedIops and burstingEnabled are only valid for cloud_auto"
// +kubebuilder:validation:XValidation:rule="!has(self.kmsKeyID) || !has(self.encrypted) || self.encrypted",message="kmsKeyID needs encrypted"
type DataDisk struct {
	common.GuestVolume `json:",inline"`

	// category is the disk category. Built-in default cloud_essd.
	// +optional
	Category DiskCategory `json:"category,omitempty"`

	// performanceLevel of a cloud_essd disk. Built-in default PL0.
	// +optional
	PerformanceLevel PerformanceLevel `json:"performanceLevel,omitempty"`

	// size of the disk, e.g. 100Gi.
	// +required
	Size resource.Quantity `json:"size"`

	// provisionedIops of a cloud_auto disk.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50000
	// +optional
	ProvisionedIops *int64 `json:"provisionedIops,omitempty"`

	// burstingEnabled turns performance bursting on for a cloud_auto disk.
	// +optional
	BurstingEnabled *bool `json:"burstingEnabled,omitempty"`

	// encrypted encrypts the disk. Built-in default false.
	// +optional
	Encrypted *bool `json:"encrypted,omitempty"`

	// kmsKeyID is the KMS key used for encryption.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	KMSKeyID string `json:"kmsKeyID,omitempty"`
}

// HTTPTokens is the IMDS token requirement. Only optional is accepted: see
// MetadataOptions.
// +kubebuilder:validation:Enum=optional
type HTTPTokens string

// MetadataOptions configures the ECS instance metadata service.
type MetadataOptions struct {
	// httpTokens is the token requirement of the metadata service. Only
	// optional is accepted: required (the hardened mode, IMDSv2 only) makes ECS
	// answer 403 to Ignition's aliyun provider, which reads the user data
	// without a token, so the nodes would not boot. Built-in default optional.
	// +optional
	HTTPTokens HTTPTokens `json:"httpTokens,omitempty"`
}

// Network is the Alibaba Cloud block of a network profile.
type Network struct {
	// internetMaxBandwidthOut is the peak public outbound bandwidth in Mbit/s;
	// 0 means no public IPv4 address.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	InternetMaxBandwidthOut *int32 `json:"internetMaxBandwidthOut,omitempty"`

	// internetChargeType is how the public bandwidth is billed.
	// +optional
	InternetChargeType InternetChargeType `json:"internetChargeType,omitempty"`
}

// DeploymentStrategy is the placement strategy of a deployment set.
// +kubebuilder:validation:Enum=LowLatency;Availability
type DeploymentStrategy string

// Deployment strategies: LowLatency puts the instances close together (the
// cluster placement of other clouds), Availability spreads them over hardware.
const (
	StrategyLowLatency   DeploymentStrategy = "LowLatency"
	StrategyAvailability DeploymentStrategy = "Availability"
)

// Placement is the Alibaba Cloud block of a placement profile.
type Placement struct {
	// deploymentSet is the placement construct nodes join; it needs a run key
	// (label placement-group) to be created per run.
	// +optional
	DeploymentSet *DeploymentSet `json:"deploymentSet,omitempty"`
}

// DeploymentSet creates a deployment set per run key (strategy) or references an
// existing one (use). Exactly one of strategy and use is set.
// +kubebuilder:validation:XValidation:rule="has(self.strategy) != has(self.use)",message="set exactly one of strategy and use"
type DeploymentSet struct {
	// strategy of the deployment sets SCarpenter creates.
	// +optional
	Strategy DeploymentStrategy `json:"strategy,omitempty"`

	// use references an existing deployment set, which is never modified or
	// deleted; the run key then only separates NodeClaims.
	// +optional
	Use *DeploymentSetRef `json:"use,omitempty"`
}

// DeploymentSetRef names an existing deployment set.
type DeploymentSetRef struct {
	// name of the deployment set.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
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

// GetImageOwner returns the account that owns the Flatcar images ("" means the
// account of the credentials).
func (s *Settings) GetImageOwner() string {
	if s == nil {
		return ""
	}
	return s.ImageOwner
}
