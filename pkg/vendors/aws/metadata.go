/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: the instance metadata service (IMDS) over IPv4 and
// IPv6 (the IPv6 endpoint of Nitro instances). The driver's hop limit of 1 keeps Pods in the Pod network off
// it already; the restriction adds the Pods in the host network. The DNS (169.254.169.253) and NTP
// (169.254.169.123) addresses are other addresses and stay open. Flatcar's AMI runs the SSM agent, which
// reads the instance identity and the instance role's credentials from it.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
			netip.MustParseAddr("fd00:ec2::254"),
		},
		Units: []string{"amazon-ssm-agent.service"},
	}
}

var _ vendors.InstanceMetadata = (*Driver)(nil)
