/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import "github.com/SpareCores/scarpenter/pkg/vendors"

// PrivateRoutesFragmentName names the fragment in the node's user-data.
const PrivateRoutesFragmentName = "private-routes"

// UpCloud delivers the route to the managed databases attached to an SDN network
// over DHCP on that network. A server that runs before the database is attached
// gets the route only at its next lease renewal, and the documentation gives
// neither the lease time nor the prefix of the route, so the node renews the
// leases of its network interfaces on a timer instead of writing a route itself.

// privateRoutesButane holds the timer and the service it starts. networkctl
// renew sends a DHCP request, and systemd-networkd applies the routes of the
// answer. Only the interfaces that have a device behind them are renewed (not
// tailscale0, the CNI bridges or veth pairs).
const privateRoutesButane = `variant: flatcar
version: 1.1.0
systemd:
  units:
    - name: scarpenter-private-routes.service
      contents: |
        [Unit]
        Description=Renew the DHCP leases to pick up the routes of the SDN network
        After=systemd-networkd.service
        [Service]
        Type=oneshot
        ExecStart=/bin/sh -c 'for d in /sys/class/net/*/device; do networkctl renew "$(basename "${d%/device}")"; done'
    - name: scarpenter-private-routes.timer
      enabled: true
      contents: |
        [Unit]
        Description=Renew the DHCP leases to pick up the routes of the SDN network
        [Timer]
        OnBootSec=2min
        OnUnitActiveSec=5min
        [Install]
        WantedBy=timers.target
`

// PrivateRoutesFragment is the boot fragment of a launch that attaches an SDN network.
func PrivateRoutesFragment() vendors.BootFragment {
	return vendors.BootFragment{Name: PrivateRoutesFragmentName, Butane: privateRoutesButane}
}
