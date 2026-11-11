/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"fmt"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataAddress is the address of the GCE metadata server. It is reachable from
// any Pod and has no hop limit (plan 16, section 3; AWS's hop limit of 1 has no
// equivalent).
const MetadataAddress = "169.254.169.254"

var _ vendors.NodeHardener = (*Driver)(nil)

// Hardenings implements vendors.NodeHardener: the one opt-in hardening, dropping
// the traffic Pods send to the metadata server (off by default; plan 14, A7).
func (d *Driver) Hardenings() []string { return []string{vendors.HardeningMetadataFromPods} }

// BootFragments implements vendors.NodeHardener: no fragment is always on (the
// arm64 image of plan 22 ships the udev rule of /dev/disk/by-id/google-<name>
// itself); the requested hardening is the shared fragment for the GCE metadata
// address.
func (d *Driver) BootFragments(req vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	var out []vendors.BootFragment
	for _, h := range req.Hardening {
		if h != vendors.HardeningMetadataFromPods {
			return nil, fmt.Errorf("the GCP driver does not offer the hardening %q", h)
		}
		f, err := vendors.BlockMetadataFromPods(MetadataAddress)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
