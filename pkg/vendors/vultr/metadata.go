/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: Vultr's metadata service, where Ignition reads the
// user data (/user-data/user-data) and which has nothing like AWS's hop limit.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
		},
		// The VPC unit reads the interfaces from the metadata service.
		Units: []string{"scarpenter-vpc-interface.service"},
	}
}
