/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var _ vendors.GPUVirtualizer = (*Driver)(nil)

// Virtual implements vendors.GPUVirtualizer: the vGPU families (vgn*, sgn*) and
// any family with a vWS licence (*-vws*) run on a GRID driver, which SCarpenter
// does not boot (plan 12, D10). Rows without a model need no rule here: the
// core drops them as unknown.
func (d *Driver) Virtual(apiReference string) bool {
	family, ok := FamilyOf(apiReference)
	if !ok {
		return false
	}
	name := strings.TrimPrefix(family, "ecs.")
	return strings.HasPrefix(name, "vgn") || strings.HasPrefix(name, "sgn") || strings.Contains(name, "-vws")
}
