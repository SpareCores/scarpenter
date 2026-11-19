/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: the OpenStack metadata service, where the node's user
// data (a bootstrap token, the Tailscale key) is readable from Pods: OpenStack has nothing like AWS's hop limit.
// Ignition and Afterburn also use its IPv6 link-local address.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
			netip.MustParseAddr("fe80::a9fe:a9fe"),
		},
	}
}
