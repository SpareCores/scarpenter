/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// MetadataEndpoints implements vendors.InstanceMetadata: UpCloud's metadata service, where Ignition reads the
// user data (the driver enables it at creation) and which has nothing like AWS's hop limit.
func (d *Driver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{
		Addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
		},
	}
}

// tailnetFragment keeps the node's access to UpCloud's private database range
// after it joins the tailnet. A managed database attached to the node's SDN
// network resolves to an address in 100.64.0.0/10, Tailscale's CGNAT range:
// tailscaled's ts-input chain drops packets from that range that arrive outside
// tailscale0, and the database's replies arrive on the SDN interface.
// --netfilter-mode=nodivert makes tailscaled create its chains without jumping
// to them from INPUT and FORWARD, so the drop never applies. The drop-in hands
// the flag to the tailscale template's scarpenter-tailscale-up.service, so
// BootFragments adds it only for that template.
const tailnetFragment = `variant: flatcar
version: 1.1.0
systemd:
  units:
    - name: scarpenter-tailscale-up.service
      dropins:
        - name: 20-upcloud-netfilter.conf
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
