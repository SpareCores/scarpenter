/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: the instance metadata service, which answers any
// process that sends the header Authorization: Bearer Oracle and holds the node's user_data (a one-hour
// bootstrap token, the Tailscale key). The address also serves the VCN resolver and NTP (port 53 and UDP,
// left open). Flatcar's OCI image has no agent besides Afterburn's units.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
			netip.MustParseAddr("fd00:c1::a9fe:a9fe"),
		},
	}
}

var _ vendors.InstanceMetadata = (*Driver)(nil)
