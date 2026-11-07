/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package options is the vendor-neutral machinery behind node settings
// (00a sections 3, 5 and 6): the registry of the keys workloads select by,
// the resolution of a vendor's settings from the driver's defaults, the
// NodeClass's vendor block and the selected profiles, the choice of profiles
// from a NodeClaim's requirements, and the per-profile drift hashes. It
// names no vendor: the blocks are opaque JSON that the drivers decode.
package options

import (
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
)

// Kind is the kind of value of an option key (00a section 5.1).
type Kind string

// The kinds of option keys.
const (
	// KindEnum: the values are known to the NodeClass (profile names) and
	// defined on every instance type.
	KindEnum Kind = "Enum"
	// KindInt and KindBool are the numeric and boolean attribute keys; they
	// are registered by the catalog (internal to it), listed here for the
	// documentation of the model.
	KindInt  Kind = "Int"
	KindBool Kind = "Bool"
	// KindName is a free slug: the placement run key, the only key that is
	// not defined on instance types.
	KindName Kind = "Name"
)

// Spec describes one workload-selectable key.
type Spec struct {
	// Key is the label key.
	Key  string
	Kind Kind
	// Profile is the profile kind an Enum key selects.
	Profile labels.ProfileKind
	// Doc says what the key selects, for the reference documentation.
	Doc string
}

// specs are the keys of this package: the profile names and the run key. They
// are registered as well-known labels of the restricted domain by the API
// package's init (and so known to Karpenter core), a test keeps the two lists
// in step.
var specs = []Spec{
	{Key: v1alpha1.LabelStorageProfile, Kind: KindEnum, Profile: labels.ProfileStorage,
		Doc: "the storage profile of the node: root and extra volumes, local storage"},
	{Key: v1alpha1.LabelNetworkProfile, Kind: KindEnum, Profile: labels.ProfileNetwork,
		Doc: "the network profile of the node: NIC features"},
	{Key: v1alpha1.LabelPlacementProfile, Kind: KindEnum, Profile: labels.ProfilePlacement,
		Doc: "the placement profile of the node: placement construct and tenancy"},
	{Key: v1alpha1.LabelPlacementGroup, Kind: KindName,
		Doc: "the placement run key: nodes with the same key and placement profile share one native group"},
}

// Specs returns the registered keys.
func Specs() []Spec { return slices.Clone(specs) }

// Lookup returns the spec of a key.
func Lookup(key string) (Spec, bool) {
	for _, s := range specs {
		if s.Key == key {
			return s, true
		}
	}
	return Spec{}, false
}

// Registered reports whether the key is registered with Karpenter core as a
// well-known label (so that NodePools and pods may use it).
func Registered(key string) bool { return karpv1.WellKnownLabels.Has(key) }

// ValidateValue checks a value of a key: a Kubernetes label value, which is
// also what a profile name or run key has to be to be selectable.
func ValidateValue(key, value string) error {
	s, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("%s is not an option key", key)
	}
	if value == "" {
		return fmt.Errorf("%s: the value is empty", key)
	}
	if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
		return fmt.Errorf("%s: %q is not a valid %s value: %s", key, value, s.Kind, errs[0])
	}
	return nil
}
