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

// tailnetFragment keeps the node's access to Alibaba's 100.100.0.0/16 services
// (the metadata service at 100.100.100.200 and the DHCP resolvers 100.100.2.136
// and 100.100.2.138) after it joins the tailnet. They lie inside 100.64.0.0/10,
// Tailscale's CGNAT range: tailscaled's ts-input chain drops packets from that
// range that arrive outside tailscale0, and their replies arrive on eth0 (the
// routes are fine). --netfilter-mode=nodivert makes tailscaled create its
// chains without jumping to them from INPUT and FORWARD, so the drop never
// applies. The drop-in hands the flag to the tailscale template's
// scarpenter-tailscale-up.service, so BootFragments adds it only for that template.
const tailnetFragment = `variant: flatcar
version: 1.1.0
systemd:
  units:
    - name: scarpenter-tailscale-up.service
      dropins:
        - name: 20-alicloud-netfilter.conf
          contents: |
            [Service]
            Environment=SCARPENTER_TAILSCALE_UP_ARGS=--netfilter-mode=nodivert
`

// BootFragments implements vendors.VendorBootFragments.
func (d *Driver) BootFragments(tailnet bool) []vendors.BootFragment {
	if !tailnet {
		return nil
	}
	return []vendors.BootFragment{{Name: "tailnet", Butane: tailnetFragment}}
}
