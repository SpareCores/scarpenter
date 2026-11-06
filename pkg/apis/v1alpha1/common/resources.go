/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package common

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Resource describes how one kind of vendor resource (a subnet, a security
// group, a placement group, ...) is provided: brought by the user (use),
// created and owned by SCarpenter (create), or not created at all (forbid).
// With none of the three set, the driver uses the vendor's own default
// resource where it exists and creates the resource otherwise, with the kind's
// default lifecycle (00a section 9.2).
//
// +kubebuilder:validation:XValidation:rule="(has(self.use) ? 1 : 0) + (has(self.create) ? 1 : 0) + ((has(self.forbid) && self.forbid) ? 1 : 0) <= 1",message="at most one of use, create and forbid may be set"
type Resource struct {
	// use brings an existing resource: it is validated per region, never
	// modified and never deleted.
	// +optional
	Use *ResourceUse `json:"use,omitempty"`

	// create lets SCarpenter create and own the resource, with a lifecycle.
	// +optional
	Create *ResourceCreate `json:"create,omitempty"`

	// forbid opts the kind out of auto-creation: offerings that need it are
	// unavailable unless the vendor's default resource exists.
	// +optional
	Forbid bool `json:"forbid,omitempty"`
}

// ResourceUse selects existing resources. Exactly one of ids, name and
// selector is set.
//
// +kubebuilder:validation:XValidation:rule="(has(self.ids) ? 1 : 0) + (has(self.name) ? 1 : 0) + (has(self.selector) ? 1 : 0) == 1",message="set exactly one of ids, name and selector"
type ResourceUse struct {
	// ids lists the vendor IDs explicitly, per region.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	// +optional
	IDs map[string][]string `json:"ids,omitempty"`

	// name is a name pattern; {region} and {zone} are replaced by the region
	// and zone in scope.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Name string `json:"name,omitempty"`

	// selector matches resources by tags (labels, where the vendor has no
	// tags), evaluated in every region in scope.
	// +optional
	Selector *TagSelector `json:"selector,omitempty"`
}

// TagSelector matches resources whose tags contain all the given pairs.
type TagSelector struct {
	// tags must all be present with the given values on the resource.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=32
	// +required
	Tags map[string]string `json:"tags"`
}

// ResourceCreate configures a managed resource.
type ResourceCreate struct {
	// lifecycle decides when the resource is deleted. Without it the kind's
	// default applies (00a section 9.3).
	// +optional
	Lifecycle *Lifecycle `json:"lifecycle,omitempty"`

	// timing decides when the resource is created. Default: on demand.
	// +optional
	Timing *Timing `json:"timing,omitempty"`
}

// LifecyclePolicy is when a managed resource is deleted.
//   - perUse: when no instance uses it any more;
//   - idleTTL: when no instance has used it for ttl;
//   - retain: when its SCNodeClass is deleted.
//
// +kubebuilder:validation:Enum=perUse;idleTTL;retain
type LifecyclePolicy string

// Lifecycle policies.
const (
	LifecyclePerUse  LifecyclePolicy = "perUse"
	LifecycleIdleTTL LifecyclePolicy = "idleTTL"
	LifecycleRetain  LifecyclePolicy = "retain"
)

// Lifecycle of a managed resource.
// +kubebuilder:validation:XValidation:rule="self.policy == 'idleTTL' || !has(self.ttl)",message="ttl is only valid with policy idleTTL"
type Lifecycle struct {
	// policy is perUse, idleTTL or retain.
	// +required
	Policy LifecyclePolicy `json:"policy"`

	// ttl is how long an unused resource is kept (policy idleTTL). Default
	// 24h, or the controller's --resource-default-ttl.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// TimingMode is when a managed resource is created.
// +kubebuilder:validation:Enum=onDemand;preCreate
type TimingMode string

// Timing modes.
const (
	TimingOnDemand  TimingMode = "onDemand"
	TimingPreCreate TimingMode = "preCreate"
)

// Timing of the creation of a managed resource.
// +kubebuilder:validation:XValidation:rule="self.mode == 'preCreate' ? has(self.regions) : !has(self.regions)",message="regions is required with mode preCreate and invalid otherwise"
type Timing struct {
	// mode: onDemand creates the resource when the first launch needs it;
	// preCreate creates it in the listed regions right away and keeps it.
	// +kubebuilder:default=onDemand
	// +optional
	Mode TimingMode `json:"mode,omitempty"`

	// regions are the regions to create the resource in (mode preCreate).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`
}
