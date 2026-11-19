/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: the instance metadata service and the WireServer
// (168.63.129.16, TCP 80 and 32526), which the Azure agent (waagent.service) talks to. The WireServer also
// serves DNS (port 53, left open) and DHCP (UDP). Node user-data comes from customData on the OVF device,
// not from here.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
			netip.MustParseAddr("168.63.129.16"),
		},
		Units: []string{"waagent.service"},
	}
}

var _ vendors.InstanceMetadata = (*Driver)(nil)
