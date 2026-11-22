/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"regexp"
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var _ vendors.GPUVirtualizer = (*Driver)(nil)

// fractionalFamily matches the families that sell a slice of a GPU (g6f, gr6f,
// and a later g7f): the generation number, then "f".
var fractionalFamily = regexp.MustCompile(`^gr?[0-9]+f$`)

// Virtual implements vendors.GPUVirtualizer: the fractional families run on a
// GRID (vGPU) driver, which SCarpenter does not boot (plan 12, D10).
func (d *Driver) Virtual(apiReference string) bool {
	family, _, _ := strings.Cut(apiReference, ".")
	return fractionalFamily.MatchString(family)
}
