/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

// GPUVirtualizer is the optional capability of a driver whose vendor sells
// virtual GPUs: slices of a GPU that need the vendor's GRID (vGPU) driver, not
// the datacenter driver SCarpenter boots (plan 12, D10). The catalog drops the
// types it reports (reason GPUVirtual).
type GPUVirtualizer interface {
	// Virtual reports whether the GPU of the instance type (the vendor's name,
	// as in the API reference label) is virtual.
	Virtual(apiReference string) bool
}
