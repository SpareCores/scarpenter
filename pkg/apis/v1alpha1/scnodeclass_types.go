/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types of an SCNodeClass. Ready is the root condition; it is true
// when the shared parts are ready. A vendor that is not ready only makes its
// own offerings unavailable (overview section 6.2).
const (
	ConditionTypeCatalogReady   = "CatalogReady"
	ConditionTypeArtifactsReady = "ArtifactsReady"
	// ConditionTypeValidationSucceeded: the semantic checks of the NodeClass
	// pass (kubelet settings, Kubernetes version skew, references, a trial
	// rendering of the user-data within its budget).
	ConditionTypeValidationSucceeded = "ValidationSucceeded"
	// ConditionTypeRestartRequired is informational and not a dependent of
	// Ready: a metric was added after start-up and its labels need a restart.
	ConditionTypeRestartRequired = "RestartRequired"
)

// Condition types of a vendor in status.vendors.<vendor>.conditions.
// QuotaAvailable=False and ConfigurationWarning=True are informational.
const (
	VendorConditionReady          = "VendorReady"
	VendorConditionImagesReady    = "ImagesReady"
	VendorConditionResourcesReady = "ResourcesReady"
	VendorConditionQuotaAvailable = "QuotaAvailable"
	// VendorConditionConfigurationWarning is present, True, while the driver
	// has something to say about a setting that has no effect for the vendor.
	VendorConditionConfigurationWarning = "ConfigurationWarning"
)

// SCNodeClass is the node definition SCarpenter launches nodes from: shared
// settings plus a block per vendor.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=karpenter,shortName=scnc
// +kubebuilder:printcolumn:name="Vendors",type=string,JSONPath=`.status.vendorNames`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SCNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec SCNodeClassSpec `json:"spec"`
	// +optional
	Status SCNodeClassStatus `json:"status,omitempty"`
}

// SCNodeClassList is a list of SCNodeClass.
// +kubebuilder:object:root=true
type SCNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SCNodeClass `json:"items"`
}

// StatusConditions returns the condition set of the SCNodeClass (operatorpkg).
func (in *SCNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions(
		ConditionTypeCatalogReady,
		ConditionTypeArtifactsReady,
		ConditionTypeValidationSucceeded,
	).For(in, opts...)
}

// GetConditions returns the conditions of the SCNodeClass.
func (in *SCNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

// SetConditions replaces the conditions of the SCNodeClass.
func (in *SCNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}

// SCNodeClassStatus is the observed state of an SCNodeClass.
type SCNodeClassStatus struct {
	// conditions of the SCNodeClass: Ready, CatalogReady, ArtifactsReady,
	// ValidationSucceeded, and RestartRequired when set.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`

	// vendorNames lists the vendors that have a block in the spec, for the
	// printer column.
	// +listType=set
	// +optional
	VendorNames []string `json:"vendorNames,omitempty"`

	// resolved holds the artifacts resolved for the whole SCNodeClass.
	// +optional
	Resolved *ResolvedArtifacts `json:"resolved,omitempty"`

	// vendors is the observed state per vendor.
	// +optional
	Vendors map[string]VendorStatus `json:"vendors,omitempty"`

	// billing says per vendor whether billing retention is active and why not.
	// +listType=map
	// +listMapKey=vendor
	// +optional
	Billing []BillingStatus `json:"billing,omitempty"`
}

// ResolvedArtifacts are the sysext images resolved from the pinned versions,
// per architecture (amd64, arm64) (overview section 8.4).
type ResolvedArtifacts struct {
	// kubernetes is the Kubernetes sysext per architecture.
	// +optional
	Kubernetes map[string]Artifact `json:"kubernetes,omitempty"`

	// tailscale is the Tailscale sysext per architecture.
	// +optional
	Tailscale map[string]Artifact `json:"tailscale,omitempty"`
}

// Artifact is a downloadable image pinned by its checksum.
type Artifact struct {
	// url the image is downloaded from.
	// +required
	URL string `json:"url"`

	// sha256 of the image, lower-case hex.
	// +required
	SHA256 string `json:"sha256"`
}

// VendorStatus is the observed state of one vendor of the SCNodeClass.
type VendorStatus struct {
	// conditions of the vendor: VendorReady, ImagesReady, ResourcesReady,
	// QuotaAvailable, and ConfigurationWarning when set.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`

	// hash is the drift hash of the shared fields that change what a node is
	// plus this vendor's block (overview section 6.3).
	// +optional
	Hash string `json:"hash,omitempty"`

	// images are the resolved boot images per region and architecture.
	// +optional
	Images []ImageStatus `json:"images,omitempty"`

	// resources are the vendor resources referenced or managed, per region, up to
	// 128 entries. Past that, resources is cut at 128 and resourceCounts says how
	// many resources there are per region and kind.
	// +kubebuilder:validation:MaxItems=128
	// +optional
	Resources []ResourceStatus `json:"resources,omitempty"`

	// resourceCounts is the number of resources per region and kind, only when
	// there are more than resources can list.
	// +kubebuilder:validation:MaxItems=512
	// +optional
	ResourceCounts []ResourceCount `json:"resourceCounts,omitempty"`

	// quota is the quota view per region and capacity type.
	// +optional
	Quota []QuotaStatus `json:"quota,omitempty"`
}

// ImageStatus is the boot image of a region and architecture.
type ImageStatus struct {
	// +required
	Region string `json:"region"`
	// arch is amd64 or arm64.
	// +required
	Arch string `json:"arch"`
	// ref is the vendor's image reference (for example an AMI ID).
	// +required
	Ref string `json:"ref"`
	// ready says whether the image can be launched in the region.
	// +required
	Ready bool `json:"ready"`
	// message explains an image that is not ready.
	// +optional
	Message string `json:"message,omitempty"`
	// reason says why an image is not ready when the cause is the image itself:
	// ImageVersionMismatch (it is another Flatcar release than the pinned one),
	// ImageVersionUnknown (it does not say which) or a driver's own reason (for
	// example MarketplaceTermsNotAccepted). It becomes the reason of the
	// ImagesReady condition.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ResourceSource says which step of the resolution order (ADR-022) produced a
// resource.
// +kubebuilder:validation:Enum=use;discovered;default;created
type ResourceSource string

// The steps that produce a resource.
const (
	ResourceSourceUse        ResourceSource = "use"
	ResourceSourceDiscovered ResourceSource = "discovered"
	ResourceSourceDefault    ResourceSource = "default"
	ResourceSourceCreated    ResourceSource = "created"
)

// ResourceStatus is a vendor resource referenced (use) or managed (create).
type ResourceStatus struct {
	// kind of the resource, for example subnets.
	// +required
	Kind string `json:"kind"`
	// +required
	Region string `json:"region"`
	// id of the resource at the vendor.
	// +required
	ID string `json:"id"`
	// name of the resource, where the vendor has one.
	// +optional
	Name string `json:"name,omitempty"`
	// managed is true for resources SCarpenter created.
	// +optional
	Managed bool `json:"managed,omitempty"`
	// source is the step of the resolution order that produced the resource:
	// use (the NodeClass's explicit use), discovered (tagged with the opt-in tag
	// scarpenter.sparecores.com/use), default (the vendor's own default) or
	// created (managed by SCarpenter).
	// +optional
	Source ResourceSource `json:"source,omitempty"`
	// lastUsed is when an instance last used a managed resource.
	// +optional
	LastUsed *metav1.Time `json:"lastUsed,omitempty"`
	// message explains a resource that is not ready or cannot be deleted.
	// +optional
	Message string `json:"message,omitempty"`
}

// ResourceCount is the number of resources of a kind in a region.
type ResourceCount struct {
	// +required
	Kind string `json:"kind"`
	// +required
	Region string `json:"region"`
	// count of the resources of the kind in the region.
	// +required
	Count int32 `json:"count"`
}

// QuotaStatus is the quota view of one bucket.
type QuotaStatus struct {
	// +required
	Region string `json:"region"`
	// capacityType is on-demand or spot.
	// +required
	CapacityType string `json:"capacityType"`
	// bucket is the vendor's quota identifier.
	// +required
	Bucket string `json:"bucket"`
	// used is the current consumption in the bucket's unit.
	// +required
	Used int64 `json:"used"`
	// limit is the bucket's limit in the same unit.
	// +required
	Limit int64 `json:"limit"`
	// reason is set when the bucket has no headroom, for example QuotaExhausted.
	// +optional
	Reason string `json:"reason,omitempty"`
}
