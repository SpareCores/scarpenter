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

// Select says how many of the matches of a ResourceUse are used.
//   - exactlyOne: one match; zero or several make the region unusable;
//   - all: every match (zero is an error);
//   - newest and oldest: the first or last by orderBy; a tie is an error.
//
// +kubebuilder:validation:Enum=exactlyOne;all;newest;oldest
type Select string

// Selection modes.
const (
	SelectExactlyOne Select = "exactlyOne"
	SelectAll        Select = "all"
	SelectNewest     Select = "newest"
	SelectOldest     Select = "oldest"
)

// ResourceUse is the one selector for the existing resources of a kind, the same
// for every vendor (ADR-023, docs/reference/resource-selectors.md). Every field
// is optional, at least one is required, all given fields are ANDed and the
// values of a list are ORed. A field the vendor's driver does not support for
// the kind is rejected by the driver's validation, never ignored.
//
// +kubebuilder:validation:XValidation:rule="has(self.ids) || has(self.name) || has(self.tags) || has(self.match) || has(self.filters) || has(self.expression)",message="set at least one of ids, name, tags, match, filters and expression: an empty use would select every resource"
// +kubebuilder:validation:XValidation:rule="!has(self.orderBy) || (has(self.select) && (self.select == 'newest' || self.select == 'oldest'))",message="orderBy is only valid with select newest or oldest"
type ResourceUse struct {
	// ids lists vendor IDs per region (the vendor's region name). The key "*"
	// applies to every region in scope (a resource that is global at the vendor,
	// the same ID in every region); a region key adds to it. With other fields
	// the IDs are ANDed with them.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.size() > 0 && k.size() <= 64)",message="ids keys are a region name or *"
	// +optional
	IDs map[string]IDList `json:"ids,omitempty"`

	// name is a glob on the kind's name field: * is any run of characters, ?
	// one character, \ escapes them; exact without wildcards.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Name string `json:"name,omitempty"`

	// tags are tags (labels, where the vendor has no tags) that must all be on
	// the resource with the given value; the value "*" means the key exists.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.size() > 0 && k.size() <= 255 && self[k].size() <= 255)",message="tag keys are 1 to 255 characters and values at most 255"
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// match holds the portable attributes, with one spelling on every vendor.
	// +optional
	Match *ResourceMatch `json:"match,omitempty"`

	// filters are the vendor's own filters for the kind, passed through: name is
	// the vendor's filter or parameter name, values are ORed.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Filters []ResourceFilter `json:"filters,omitempty"`

	// expression is a query in the vendor's own language, where it has one.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	Expression string `json:"expression,omitempty"`

	// select says how many of the matches are used. Without it the kind's
	// default applies (the driver's capability declaration).
	// +optional
	Select Select `json:"select,omitempty"`

	// orderBy is what newest and oldest order by: created, name or
	// label:<key>.
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="self == 'created' || self == 'name' || (self.startsWith('label:') && self.size() > 6)",message="orderBy is created, name or label:<key>"
	// +optional
	OrderBy string `json:"orderBy,omitempty"`
}

// IDList is the IDs of a resource kind in one region.
// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=64
// +kubebuilder:validation:items:MinLength=1
// +kubebuilder:validation:items:MaxLength=255
type IDList []string

// ResourceFilter is one of the vendor's own filters.
type ResourceFilter struct {
	// name is the vendor's own filter or parameter name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +required
	Name string `json:"name"`

	// values are ORed.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=255
	// +listType=atomic
	// +required
	Values []string `json:"values"`
}

// ResourceMatch is the fixed set of portable attributes (resource-selectors.md).
// A driver declares which it supports per kind; lists are ORed, attributes are
// ANDed.
//
// +kubebuilder:validation:XValidation:rule="has(self.architecture) || has(self.owner) || has(self.state) || has(self.includeDeprecated) || has(self.createdAfter) || has(self.createdBefore) || has(self.description) || has(self.zone) || has(self.network) || has(self.cidr) || has(self.within)",message="match sets no attribute"
type ResourceMatch struct {
	// architecture is the CPU architecture of an image.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:items:Enum=amd64;arm64
	// +listType=set
	// +optional
	Architecture []string `json:"architecture,omitempty"`

	// owner is whose resources to look at: self, an account or project ID, or
	// the vendor's public alias.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=255
	// +listType=set
	// +optional
	Owner []string `json:"owner,omitempty"`

	// state is the vendor's state, normalised. Default [available] for every
	// kind that has a state.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +kubebuilder:validation:items:Enum=available;pending;failed
	// +listType=set
	// +optional
	State []string `json:"state,omitempty"`

	// includeDeprecated includes images the vendor marked deprecated.
	// +optional
	IncludeDeprecated *bool `json:"includeDeprecated,omitempty"`

	// createdAfter and createdBefore bound the creation time (RFC 3339), on
	// kinds that have one.
	// +optional
	CreatedAfter *metav1.Time `json:"createdAfter,omitempty"`
	// +optional
	CreatedBefore *metav1.Time `json:"createdBefore,omitempty"`

	// description is a glob on the description field.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Description string `json:"description,omitempty"`

	// zone lists the vendor's zone IDs of a zonal resource.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=255
	// +listType=set
	// +optional
	Zone []string `json:"zone,omitempty"`

	// network lists the network IDs a subnet or security group belongs to.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=255
	// +listType=set
	// +optional
	Network []string `json:"network,omitempty"`

	// cidr is a range the resource's range must equal.
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="cidr must be a CIDR"
	// +optional
	CIDR string `json:"cidr,omitempty"`

	// within is a range the resource's range must lie inside.
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="within must be a CIDR"
	// +optional
	Within string `json:"within,omitempty"`
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
