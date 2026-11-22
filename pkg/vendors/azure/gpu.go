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

// Virtual implements vendors.GPUVirtualizer: the GPU of these sizes cannot run
// on the datacenter driver SCarpenter boots (plan 12, D10), so the catalog
// drops them as GPUVirtual:
//   - the NV series (NV v2 and v3 with the M60, NVadsA10 v5 with the A10, and the
//     later NV sizes) are visualization sizes whose GPUs are vGPUs on NVIDIA's
//     GRID driver;
//   - the NCC series (NCC40ads_H100_v5) are confidential VM sizes with a GPU:
//     they boot Azure Container Linux, which has no NVIDIA driver. The driver
//     also refuses them at launch (ConfidentialGPUNotSupported), as a backstop.
//
// The NC and ND series pass the whole GPU through and are not affected.
func (d *Driver) Virtual(apiReference string) bool {
	return strings.HasPrefix(apiReference, "Standard_NV") || strings.HasPrefix(apiReference, "Standard_NCC")
}
