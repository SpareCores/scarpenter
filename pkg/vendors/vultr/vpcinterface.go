/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import "github.com/SpareCores/scarpenter/pkg/vendors"

// A Vultr VPC has no DHCP and Vultr's cloud-init, which sets the address of the
// VPC interface on its own images, does not run on Flatcar. The node therefore
// reads the address from the metadata service and gives it to systemd-networkd.

// VPCInterfaceFragmentName names the fragment in the node's user-data.
const VPCInterfaceFragmentName = "vpc-interface"

// VPCInterfaceFragment is the boot fragment of a launch that attaches a VPC: a
// oneshot unit that configures the private interface before the kubelet starts.
// The kubelet only wants it, so a failed unit never keeps the node from joining.
func VPCInterfaceFragment() vendors.BootFragment {
	return vendors.BootFragment{Name: VPCInterfaceFragmentName, Butane: vpcInterfaceButane}
}

// vpcInterfaceButane holds the script and its unit. The script reads interfaces[] of the
// metadata (mac, network-type "private", ipv4.address, ipv4.netmask and mtu), writes one
// .network file per private interface that matches the NIC by MAC and carries the MTU and
// the static address only (no gateway, no route: the default route stays on the public
// interface), and reconfigures the link.
const vpcInterfaceButane = `variant: flatcar
version: 1.1.0
storage:
  files:
    - path: /opt/scarpenter/bin/scarpenter-vpc-interface
      mode: 0755
      contents:
        inline: |
          #!/bin/sh
          set -eu
          tries=20
          rows=
          while [ "$tries" -gt 0 ]; do
            rows=$(curl -fsS --max-time 3 http://169.254.169.254/v1.json 2>/dev/null |
              jq -r '.interfaces[]? |
                select(."network-type" == "private" and (.unconfigured | not) and ((.ipv4.address // "") != "")) |
                [.mac, .ipv4.address, .ipv4.netmask, (.mtu // 1450)] | join("|")' 2>/dev/null) || rows=
            [ -n "$rows" ] && break
            tries=$((tries - 1))
            sleep 2
          done
          if [ -z "$rows" ]; then
            echo "no private interface in the metadata service" >&2
            exit 1
          fi
          devs=
          while IFS='|' read -r mac addr mask mtu; do
            prefix=0
            for octet in $(echo "$mask" | tr . ' '); do
              case $octet in
                255) prefix=$((prefix + 8)) ;;
                254) prefix=$((prefix + 7)) ;;
                252) prefix=$((prefix + 6)) ;;
                248) prefix=$((prefix + 5)) ;;
                240) prefix=$((prefix + 4)) ;;
                224) prefix=$((prefix + 3)) ;;
                192) prefix=$((prefix + 2)) ;;
                128) prefix=$((prefix + 1)) ;;
              esac
            done
            file="/etc/systemd/network/10-scarpenter-vpc-$(echo "$mac" | tr ':' '-').network"
          printf '[Match]\nMACAddress=%s\n\n[Link]\nMTUBytes=%s\n\n[Network]\nAddress=%s/%s\n' \
            "$mac" "$mtu" "$addr" "$prefix" > "$file.new"
            if cmp -s "$file.new" "$file"; then
              rm "$file.new"
              echo "VPC interface $mac already has $addr/$prefix, mtu $mtu"
            else
              mv "$file.new" "$file"
              echo "VPC interface $mac configured with $addr/$prefix, mtu $mtu"
            fi
            dev=$(grep -lix "$mac" /sys/class/net/*/address | head -n 1 | cut -d/ -f5)
            [ -n "$dev" ] && devs="$devs $dev"
          done <<EOT
          $rows
          EOT
          networkctl reload
          if [ -n "$devs" ]; then
            # shellcheck disable=SC2086 # one interface name per word
            networkctl reconfigure $devs
          fi
systemd:
  units:
    - name: scarpenter-vpc-interface.service
      enabled: true
      contents: |
        [Unit]
        Description=Configure the VPC interface from the Vultr metadata service
        After=systemd-networkd.service
        Wants=systemd-networkd.service
        Before=network-online.target kubelet.service
        [Service]
        Type=oneshot
        RemainAfterExit=yes
        TimeoutStartSec=180
        ExecStart=/opt/scarpenter/bin/scarpenter-vpc-interface
        [Install]
        WantedBy=multi-user.target
    - name: kubelet.service
      dropins:
        - name: 17-scarpenter-vpc-interface.conf
          contents: |
            [Unit]
            Wants=scarpenter-vpc-interface.service
            After=scarpenter-vpc-interface.service
`
