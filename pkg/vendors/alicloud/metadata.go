/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: the ECS instance metadata endpoint, where Ignition
// reads the user data without a token (HttpTokens stays optional, so it cannot be closed at the API). The rest of
// 100.100.0.0/16 holds other platform services and is not touched.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("100.100.100.200"),
		},
	}
}

// dnsFragment makes the node resolve through Alibaba's public resolvers. The
// DHCP resolvers (100.100.2.136 and 100.100.2.138) lie inside 100.64.0.0/10,
// Tailscale's CGNAT range: after the node joins the tailnet, tailscaled's
// handling of that range breaks traffic with Alibaba's 100.100.x service
// addresses, and the node can no longer resolve the API server's name. The
// metadata service (100.100.100.200) and any 100.100.x NTP server are in the
// range too, so this fixes DNS only. A drop-in of the image's default network
// unit sets the public resolvers and ignores the DHCP ones.
const dnsFragment = `variant: flatcar
version: 1.1.0
storage:
  files:
    - path: /etc/systemd/network/zz-default.network.d/10-scarpenter-dns.conf
      mode: 0644
      contents:
        inline: |
          [Network]
          DNS=223.5.5.5
          DNS=223.6.6.6
          [DHCPv4]
          UseDNS=false
`

// BootFragments implements vendors.VendorBootFragments.
func (d *Driver) BootFragments() []vendors.BootFragment {
	return []vendors.BootFragment{{Name: "dns", Butane: dnsFragment}}
}
