/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: Hetzner's metadata service, where Ignition reads the
// user data (the API has no switch for it), at its IPv4 address and at the link-local IPv6 address that
// Afterburn and cloud-init can use.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
			netip.MustParseAddr("fe80::a9fe:a9fe"),
		},
	}
}

var _ vendors.InstanceMetadata = (*Driver)(nil)
