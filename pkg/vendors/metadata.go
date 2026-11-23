/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// BootFragment is a Butane config (variant flatcar, version 1.1.0) that the core
// builds for a node. It is rendered like the user's own Butane (the template
// variables of the node bootstrap reference are available) and merged after the
// base config and before the network template and the user's Butane, so those can
// still override it.
type BootFragment struct {
	// Name identifies the fragment in errors and in the user-data's parts.
	Name   string
	Butane string
}

// MetadataEndpoints says where a vendor's instance metadata services are and which
// node components, besides Afterburn's, talk to them.
type MetadataEndpoints struct {
	// Addresses are the IPv4 and IPv6 addresses of the metadata services as the
	// node reaches them, including a second service such as Azure's WireServer.
	// Only TCP connections the node opens are restricted, and not to port 53: some
	// of these addresses also serve DNS (GCP, OCI, Azure's WireServer) and a
	// vendor's NTP, which must keep working, and the replies to connections that
	// come from them (Azure's load balancer probes come from the WireServer's
	// address) must pass. Addresses of other services (AWS's DNS and NTP at
	// 169.254.169.253 and 169.254.169.123) are not listed.
	Addresses []netip.Addr
	// Units are the systemd units of the vendor's Flatcar image (its OEM agents)
	// that must keep reaching the addresses. The units of Afterburn
	// (coreos-metadata.service and coreos-metadata-sshkeys@.service) are always
	// allowed and not listed.
	Units []string
}

// InstanceMetadata is the optional capability of a driver whose vendor serves
// instance metadata (and the node's user-data) to the node over the network.
// The core then closes the addresses to everything but the allowed units on nodes
// of a NodeClass whose spec.instanceMetadata.access is Restricted (the default):
// see RestrictMetadata. A driver that does not implement it leaves its nodes alone.
type InstanceMetadata interface {
	MetadataEndpoints() MetadataEndpoints
}

// The slice that holds the allowed units, and its cgroup path: the dash in the
// name makes scarpenter.slice its parent, and the firewall's cgroup match needs
// the path below the cgroup root.
const (
	metadataSlice       = "scarpenter-metadata.slice"
	metadataSliceCgroup = "scarpenter.slice/" + metadataSlice
	metadataFirewall    = "scarpenter-metadata-firewall.service"
)

// afterburnUnits are the units that read the instance metadata on every Flatcar
// node (the OEM images add nothing else on the vendors that have no agent).
var afterburnUnits = []string{"coreos-metadata.service", "coreos-metadata-sshkeys@.service"}

var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.@-]+\.service$`)

// RestrictMetadata returns the fragment that restricts the driver's metadata
// addresses to the allowed units (ADR-025), and false when the driver declares
// none. The mechanism, which the node reference documents:
//
//   - the allowed units run in scarpenter-metadata.slice (a Slice= drop-in each);
//   - a oneshot unit ordered before network-pre.target loads iptables rules into
//     the security table: in OUTPUT, a TCP connection to the addresses (but to
//     port 53) is dropped unless the socket's cgroup is below that slice; in
//     FORWARD it is dropped for Pods in the Pod network. IPv4 and IPv6 alike;
//   - the allowed units want the unit and start after it, so that rules for a
//     slice that was stopped and started again (a new cgroup) are loaded again
//     before they run;
//   - kubelet requires the unit, so a node whose rules failed to load never joins.
func RestrictMetadata(d Driver) (BootFragment, bool, error) {
	return restrictWith(d, agentParts{})
}

// restrictWith is RestrictMetadata with more files and units in the same
// fragment (the interruption agent): one part of the user-data instead of two.
func restrictWith(d Driver, extra agentParts) (BootFragment, bool, error) {
	im, ok := d.(InstanceMetadata)
	if !ok {
		return BootFragment{}, false, nil
	}
	ep := im.MetadataEndpoints()
	if len(ep.Addresses) == 0 {
		return BootFragment{}, false, nil
	}
	var v4, v6 []string
	for _, a := range ep.Addresses {
		a = a.Unmap()
		switch {
		case !a.IsValid():
			return BootFragment{}, false, fmt.Errorf("%s: an invalid metadata address", d.Vendor())
		case a.Is4():
			v4 = append(v4, a.String())
		default:
			v6 = append(v6, a.String())
		}
	}
	units := append([]string{}, afterburnUnits...)
	for _, u := range ep.Units {
		if !unitName.MatchString(u) {
			return BootFragment{}, false, fmt.Errorf("%s: %q is not a systemd service unit name", d.Vendor(), u)
		}
		units = append(units, u)
	}

	var b strings.Builder
	b.WriteString("variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n")
	writeRules(&b, "/etc/scarpenter/metadata-iptables.rules", v4, "/32")
	writeRules(&b, "/etc/scarpenter/metadata-ip6tables.rules", v6, "/128")
	b.WriteString(firewallScript)
	b.WriteString(extra.files)
	b.WriteString("systemd:\n  units:\n")
	b.WriteString(sliceAndFirewallUnits)
	b.WriteString(extra.units)
	for _, u := range units {
		fmt.Fprintf(&b, "    - name: %s\n      dropins:\n        - name: 10-scarpenter-metadata.conf\n"+
			"          contents: |\n            [Unit]\n            Wants=%s\n            After=%s\n"+
			"            [Service]\n            Slice=%s\n", u, metadataFirewall, metadataFirewall, metadataSlice)
	}
	return BootFragment{Name: "metadata-access", Butane: b.String()}, true, nil
}

// writeRules writes one iptables-restore file for the addresses of a family, if
// there are any. The chains are filled in one transaction, and only then hooked by
// the script: the cgroup match is refused in a chain that no built-in chain calls
// from OUTPUT yet.
func writeRules(b *strings.Builder, path string, addrs []string, prefix string) {
	if len(addrs) == 0 {
		return
	}
	fmt.Fprintf(b, "    - path: %s\n      mode: 0644\n      contents:\n        inline: |\n", path)
	b.WriteString("          *security\n")
	b.WriteString("          :SCARPENTER-IMDS-OUTPUT - [0:0]\n")
	b.WriteString("          :SCARPENTER-IMDS-FORWARD - [0:0]\n")
	for _, a := range addrs {
		match := "-d " + a + prefix + " -p tcp -m tcp ! --dport 53 --syn"
		fmt.Fprintf(b, "          -A SCARPENTER-IMDS-OUTPUT %s -m cgroup --path %s -j RETURN\n", match, metadataSliceCgroup)
		fmt.Fprintf(b, "          -A SCARPENTER-IMDS-OUTPUT %s -j DROP\n", match)
		fmt.Fprintf(b, "          -A SCARPENTER-IMDS-FORWARD %s -j DROP\n", match)
	}
	b.WriteString("          COMMIT\n")
}

// firewallScript loads the rules files that exist. It is safe to run again: the
// chains are replaced in one transaction and the jumps are added once.
const firewallScript = `    - path: /opt/scarpenter/bin/scarpenter-metadata-firewall
      mode: 0755
      contents:
        inline: |
          #!/bin/sh
          set -eu
          load() {
            cmd=$1 rules=$2
            [ -f "$rules" ] || return 0
            "$cmd-restore" -w --noflush < "$rules"
            for hook in OUTPUT FORWARD; do
              "$cmd" -w -t security -C "$hook" -j "SCARPENTER-IMDS-$hook" 2>/dev/null ||
                "$cmd" -w -t security -I "$hook" 1 -j "SCARPENTER-IMDS-$hook"
            done
          }
          load iptables /etc/scarpenter/metadata-iptables.rules
          load ip6tables /etc/scarpenter/metadata-ip6tables.rules
`

// sliceAndFirewallUnits are the slice, the unit that loads the rules and the
// kubelet's dependency on it. The rules are never removed: stopping the unit or
// the slice leaves them in place, and a slice that is recreated has a new cgroup
// that the loaded rules no longer match, which blocks the allowed units too
// (fail closed) until the unit runs again. Stopping the slice stops the unit
// (Requires=), and the next allowed unit that starts starts it again (Wants= in
// its drop-in); the kubelet is stopped with it and needs a start.
const sliceAndFirewallUnits = `    - name: scarpenter-metadata.slice
      contents: |
        [Unit]
        Description=Node components allowed to reach the instance metadata service
    - name: scarpenter-metadata-firewall.service
      enabled: true
      contents: |
        [Unit]
        Description=Restrict the instance metadata service to the scarpenter-metadata slice
        DefaultDependencies=no
        Requires=scarpenter-metadata.slice
        After=scarpenter-metadata.slice
        Before=network-pre.target
        Wants=network-pre.target
        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/opt/scarpenter/bin/scarpenter-metadata-firewall
        [Install]
        WantedBy=basic.target
    - name: kubelet.service
      dropins:
        - name: 15-scarpenter-metadata.conf
          contents: |
            [Unit]
            Requires=scarpenter-metadata-firewall.service
            After=scarpenter-metadata-firewall.service
`
