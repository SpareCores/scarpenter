/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package options

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var kinds = []string{vendors.ProfileStorage, vendors.ProfileNetwork, vendors.ProfilePlacement}

// Selection holds the profile of each kind a node uses; "" and "default" mean
// the vendor defaults.
type Selection struct {
	Storage, Network, Placement string
}

// Name returns the selected name of the kind, "default" when none.
func (s Selection) Name(kind string) string {
	var n string
	switch kind {
	case vendors.ProfileStorage:
		n = s.Storage
	case vendors.ProfileNetwork:
		n = s.Network
	case vendors.ProfilePlacement:
		n = s.Placement
	}
	if n == "" {
		return vendors.DefaultProfile
	}
	return n
}

// Applied returns the applied profile names by kind ("default" where none),
// which is what the profile labels of the node say.
func (s Selection) Applied() map[string]string {
	out := make(map[string]string, len(kinds))
	for _, k := range kinds {
		out[k] = s.Name(k)
	}
	return out
}

// IsDefault reports whether no profile is selected.
func (s Selection) IsDefault() bool {
	return s.Name(vendors.ProfileStorage) == vendors.DefaultProfile &&
		s.Name(vendors.ProfileNetwork) == vendors.DefaultProfile &&
		s.Name(vendors.ProfilePlacement) == vendors.DefaultProfile
}

// ErrProfileNotDefined is returned for a selected profile that has no block
// for the vendor: it does not exist there (ADR-017).
var ErrProfileNotDefined = errors.New("the profile is not defined for the vendor")

// Resolve computes the settings of a node (00a section 3): the driver's
// built-in defaults, overridden by the NodeClass's vendor block, with the
// selected storage profile overriding the storage block and the network and
// placement profiles passed as they are. The result is deterministic.
func Resolve(no vendors.NodeOptions, in Input, sel Selection) (vendors.VendorSettings, error) {
	def := no.Defaults()
	base, err := decode(def.Raw)
	if err != nil {
		return vendors.VendorSettings{}, fmt.Errorf("the driver's defaults: %w", err)
	}
	block, err := decode(in.Block)
	if err != nil {
		return vendors.VendorSettings{}, err
	}
	merged := merge(base, block)

	s := vendors.VendorSettings{Networking: in.Networking, Profiles: sel.Applied()}
	for _, kind := range kinds {
		name := sel.Name(kind)
		if name == vendors.DefaultProfile {
			continue
		}
		pb, ok := in.Profiles[kind][name]
		if !ok {
			return vendors.VendorSettings{}, fmt.Errorf("%s profile %q: %w", kind, name, ErrProfileNotDefined)
		}
		switch kind {
		case vendors.ProfileStorage:
			patch, err := decode(pb)
			if err != nil {
				return vendors.VendorSettings{}, fmt.Errorf("storage profile %q: %w", name, err)
			}
			cur, _ := merged["storage"].(map[string]any)
			merged["storage"] = merge(cur, patch)
		case vendors.ProfileNetwork:
			s.Network = slices.Clone(pb)
		case vendors.ProfilePlacement:
			s.Placement = slices.Clone(pb)
		}
	}
	if s.Raw, err = json.Marshal(merged); err != nil {
		return vendors.VendorSettings{}, fmt.Errorf("encoding the settings: %w", err)
	}
	s.RootDiskBytes = no.RootDiskBytes(s)
	return s, nil
}

// Validate resolves the defaults and every profile of the input, to find a
// block that cannot be merged before a launch does.
func Validate(no vendors.NodeOptions, in Input) error {
	var errs []error
	if _, err := Resolve(no, in, Selection{}); err != nil {
		errs = append(errs, err)
	}
	for _, kind := range kinds {
		for name := range in.Profiles[kind] {
			sel := Selection{}
			switch kind {
			case vendors.ProfileStorage:
				sel.Storage = name
			case vendors.ProfileNetwork:
				sel.Network = name
			default:
				sel.Placement = name
			}
			if _, err := Resolve(no, in, sel); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Select chooses the profile of each kind for an offering from the NodeClaim's
// requirements (00a section 5.2). A kind the NodeClaim does not constrain gets
// the default; otherwise the values allowed by the claim, the instance type and
// the offering are candidates, the default winning when it is among them and
// the first by name otherwise. ok is false when a constrained kind has no
// allowed value, which the scheduler's feasibility check should have made
// impossible.
func Select(claim, typ, off scheduling.Requirements) (sel Selection, ok bool) {
	ok = true
	pick := func(kind labels.ProfileKind) string {
		key := labels.ProfileLabel(kind)
		if !claim.Has(key) {
			return vendors.DefaultProfile
		}
		allowed := sets.New[string]()
		have := typ.Get(key)
		if off.Has(key) {
			have = have.Intersection(off.Get(key))
		}
		for _, v := range have.Values() {
			if claim.Get(key).Has(v) {
				allowed.Insert(v)
			}
		}
		switch {
		case allowed.Len() == 0:
			ok = false
			return ""
		case allowed.Has(vendors.DefaultProfile):
			return vendors.DefaultProfile
		}
		return sets.List(allowed)[0]
	}
	sel.Storage = pick(labels.ProfileStorage)
	sel.Network = pick(labels.ProfileNetwork)
	sel.Placement = pick(labels.ProfilePlacement)
	return sel, ok
}

// RunKey returns the placement run key of the NodeClaim's requirements: the
// single value of the placement-group requirement, "" without one. A claim
// that allows several keys is ambiguous and an error.
func RunKey(claim scheduling.Requirements) (string, error) {
	if !claim.Has(v1alpha1.LabelPlacementGroup) {
		return "", nil
	}
	r := claim.Get(v1alpha1.LabelPlacementGroup)
	if r.Operator() != corev1.NodeSelectorOpIn || len(r.Values()) != 1 {
		return "", fmt.Errorf("the placement run key must be exactly one value, the NodeClaim allows %v", r.Values())
	}
	key := r.Values()[0]
	if err := ValidateValue(v1alpha1.LabelPlacementGroup, key); err != nil {
		return "", err
	}
	return key, nil
}
