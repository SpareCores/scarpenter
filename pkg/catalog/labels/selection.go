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

// Selection returns the selection label of a type: In the profiles it belongs
// to, DoesNotExist for none (00a section 7.3).
func Selection(profiles []string) *scheduling.Requirement {
	if len(profiles) == 0 {
		return Absent(v1alpha1.LabelSelection)
	}
	p := slices.Clone(profiles)
	slices.Sort(p)
	return In(v1alpha1.LabelSelection, slices.Compact(p)...)
}
