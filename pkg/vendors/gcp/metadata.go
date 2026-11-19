/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: the metadata server over IPv4 and IPv6 (IPv6-only
// instances). The same addresses serve DNS and NTP over UDP and DNS over TCP port 53, which stay open.
// Flatcar's GCE agent (oem-gce.service) and its daemons run as one unit.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
			netip.MustParseAddr("fd20:ce::254"),
		},
		Units: []string{"oem-gce.service"},
	}
}

var _ vendors.InstanceMetadata = (*Driver)(nil)
