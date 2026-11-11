/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import "github.com/SpareCores/scarpenter/pkg/vendors"

// MetadataAddress is the address of UpCloud's metadata service. It carries the
// user-data (the node's Ignition configuration, with the join token) and has
// nothing like AWS's hop limit, so Pods can reach it unless the hardening
// metadata-from-pods drops their traffic.
const MetadataAddress = "169.254.169.254"

// Hardenings implements vendors.NodeHardener.
func (d *Driver) Hardenings() []string { return []string{vendors.HardeningMetadataFromPods} }

// BootFragments implements vendors.NodeHardener: nothing is always on; the
// hardening metadata-from-pods drops Pod traffic forwarded to the metadata address.
func (d *Driver) BootFragments(req vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	var out []vendors.BootFragment
	for _, h := range req.Hardening {
		if h != vendors.HardeningMetadataFromPods {
			continue
		}
		f, err := vendors.BlockMetadataFromPods(MetadataAddress)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
