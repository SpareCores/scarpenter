/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"fmt"
	"net/netip"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// PrivateRoutesFragmentName names the fragment in the node's user-data.
const PrivateRoutesFragmentName = "private-routes"

// UpCloud delivers the route to the managed databases attached to an SDN network
// over DHCP on that network. A server that runs before the database is attached
// gets the route only at its next lease renewal, and the documentation gives
// neither the lease time nor the prefix of the route, so the node renews the
// lease of its SDN interface on a timer instead of writing a route itself.

// privateRoutesButane holds the script, the service and the timer; %s is the
// network's range. networkctl renew sends a DHCP request and systemd-networkd
// applies the routes of the answer. The SDN interface is the one that has an
// address in the range. The script always exits 0: a failed renewal is retried
// by the next timer run, and the unit never shows as failed.
const privateRoutesButane = `variant: flatcar
version: 1.1.0
storage:
  files:
    - path: /opt/scarpenter/bin/scarpenter-private-routes
      mode: 0755
      contents:
        inline: |
          #!/bin/sh
          for dev in $(ip -o -4 addr show to %s | awk '{print $2}'); do
            networkctl renew "$dev" || true
          done
systemd:
  units:
    - name: scarpenter-private-routes.service
      contents: |
        [Unit]
        Description=Renew the DHCP lease of the SDN interface to pick up the routes of the network
        After=systemd-networkd.service
        [Service]
        Type=oneshot
        ExecStart=/opt/scarpenter/bin/scarpenter-private-routes
    - name: scarpenter-private-routes.timer
      enabled: true
      contents: |
        [Unit]
        Description=Renew the DHCP lease of the SDN interface to pick up the routes of the network
        [Timer]
        OnBootSec=2min
        OnUnitActiveSec=5min
        [Install]
        WantedBy=timers.target
`

// PrivateRoutesFragment is the boot fragment of a launch that attaches an SDN
// network with DHCP; networkRange is the network's IPv4 range.
func PrivateRoutesFragment(networkRange string) (vendors.BootFragment, error) {
	p, err := netip.ParsePrefix(networkRange)
	if err != nil {
		return vendors.BootFragment{}, fmt.Errorf("SDN network range: %w", err)
	}
	return vendors.BootFragment{Name: PrivateRoutesFragmentName, Butane: fmt.Sprintf(privateRoutesButane, p)}, nil
}
