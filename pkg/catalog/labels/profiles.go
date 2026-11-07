/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package labels

import (
	"slices"

	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// ProfileKind is a kind of node profile (00a section 6).
type ProfileKind string

// The profile kinds, one label each.
const (
	ProfileStorage   ProfileKind = "storage"
	ProfileNetwork   ProfileKind = "network"
	ProfilePlacement ProfileKind = "placement"
)

// ProfileKinds lists the kinds in a fixed order.
var ProfileKinds = []ProfileKind{ProfileStorage, ProfileNetwork, ProfilePlacement}

// ProfileLabel returns the label key of a profile kind.
func ProfileLabel(k ProfileKind) string {
	switch k {
	case ProfileStorage:
		return v1alpha1.LabelStorageProfile
	case ProfileNetwork:
		return v1alpha1.LabelNetworkProfile
	default:
		return v1alpha1.LabelPlacementProfile
	}
}

// Profiles are the profile names a NodeClass defines, by kind.
type Profiles map[ProfileKind][]string

// ProfileRequirements returns the three profile labels: In [default,
// <supported...>] for each kind.
func ProfileRequirements(supported map[ProfileKind][]string) []*scheduling.Requirement {
	out := make([]*scheduling.Requirement, 0, len(ProfileKinds))
	for _, k := range ProfileKinds {
		names := append([]string{v1alpha1.DefaultProfile}, supported[k]...)
		slices.Sort(names)
		out = append(out, In(ProfileLabel(k), slices.Compact(names)...))
	}
	return out
}
