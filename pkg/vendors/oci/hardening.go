/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"fmt"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataAddress is the address of the instance metadata service. It answers any process that
// sends the header Authorization: Bearer Oracle, has no hop limit, and holds the node's user_data
// (a one-hour bootstrap token, the Tailscale key): Pods can read it (plan 25, section 8).
const MetadataAddress = "169.254.169.254"

// Hardenings implements vendors.NodeHardener: the one opt-in hardening, dropping the traffic Pods
// send to the metadata service (off by default; plan 14, A7).
func (d *Driver) Hardenings() []string { return []string{vendors.HardeningMetadataFromPods} }

// BootFragments implements vendors.NodeHardener: no fragment is always on; the requested hardening
// is the shared fragment for the metadata address.
func (d *Driver) BootFragments(req vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	var out []vendors.BootFragment
	for _, h := range req.Hardening {
		if h != vendors.HardeningMetadataFromPods {
			return nil, fmt.Errorf("the OCI driver does not offer the hardening %q", h)
		}
		f, err := vendors.BlockMetadataFromPods(MetadataAddress)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

var _ vendors.NodeHardener = (*Driver)(nil)
