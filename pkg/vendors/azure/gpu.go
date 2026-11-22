/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"strings"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var _ vendors.GPUVirtualizer = (*Driver)(nil)

// Virtual implements vendors.GPUVirtualizer: the NV series (NV v2 and v3 with
// the M60, NVadsA10 v5 with the A10, and the later NV sizes) are the
// visualization sizes, whose GPUs are vGPUs served by NVIDIA's GRID driver, not
// the datacenter driver SCarpenter boots (plan 12, D10). The NC and ND series
// pass the whole GPU through and are not virtual.
func (d *Driver) Virtual(apiReference string) bool {
	return strings.HasPrefix(apiReference, "Standard_NV")
}
