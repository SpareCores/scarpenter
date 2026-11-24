/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// A bare-metal server cannot join a Vultr firewall group, so its node filters
// the public interface itself: HostFirewallFragment turns the rules of the
// launch's managed firewall group into an nftables ruleset that a unit loads
// before the kubelet starts. The group stays the one source of the inbound
// policy of instances and bare metal alike.

// HostFirewallFragmentName names the fragment in the node's user-data.
const HostFirewallFragmentName = "host-firewall"

// publicIfPlaceholder stands for the public interfaces in the ruleset, a
// comma-separated list of quoted names. They are only known on the node (the
// ones of the IPv4 and IPv6 default routes), where the script replaces it.
const publicIfPlaceholder = "@PUBLIC_IFS@"

// familyIPv6 is the IPv6 family of a rule.
const familyIPv6 = "ipv6"

var portRange = regexp.MustCompile(`^([0-9]{1,5})(?::([0-9]{1,5}))?$`)

// hostFirewallRule translates one rule of a firewall group into an nftables
// rule that accepts what the rule allows. A rule it cannot express is an error:
// a node never gets a looser or different policy than the group's.
func hostFirewallRule(r govultr.FirewallRule) (string, error) {
	if r.Action != "" && r.Action != "accept" {
		return "", fmt.Errorf("rule %d has the action %q, only accept rules are translated", r.ID, r.Action)
	}
	family := ""
	switch r.IPType {
	case "v4":
		family = "ipv4"
	case "v6":
		family = familyIPv6
	default:
		return "", fmt.Errorf("rule %d has the IP type %q", r.ID, r.IPType)
	}
	prefix, err := rulePrefix(r)
	if err != nil {
		return "", err
	}
	if prefix.Addr().Is6() != (family == familyIPv6) {
		return "", fmt.Errorf("rule %d has the source %s, not a %s prefix", r.ID, prefix, r.IPType)
	}
	prefix = prefix.Masked()
	// A rule for every address of a family matches the family only.
	match := "meta nfproto " + family
	if prefix.Bits() > 0 {
		match = map[string]string{"ipv4": "ip", familyIPv6: "ip6"}[family] + " saddr " + prefix.String()
	}
	switch r.Protocol {
	case "tcp", "udp":
		if r.Port == "" {
			return fmt.Sprintf("%s meta l4proto %s accept", match, r.Protocol), nil
		}
		ports, err := nftPorts(r.Port)
		if err != nil {
			return "", fmt.Errorf("rule %d: %w", r.ID, err)
		}
		return fmt.Sprintf("%s %s dport %s accept", match, r.Protocol, ports), nil
	case protoICMP:
		proto := protoICMP
		if family == familyIPv6 {
			proto = "ipv6-icmp"
		}
		return fmt.Sprintf("%s meta l4proto %s accept", match, proto), nil
	case "gre", "esp", "ah":
		return fmt.Sprintf("%s meta l4proto %s accept", match, r.Protocol), nil
	}
	return "", fmt.Errorf("rule %d has the protocol %q", r.ID, r.Protocol)
}

// rulePrefix is the source network of a rule. Vultr answers the CIDR the rule
// was created with in the subnet and subnet_size fields and in source, which
// the API also uses for a named source ("cloudflare"): a source that is not a
// CIDR is an error, and a source that disagrees with the subnet is too.
func rulePrefix(r govultr.FirewallRule) (netip.Prefix, error) {
	var fromSubnet netip.Prefix
	if r.Subnet != "" || r.Source == "" {
		p, err := netip.ParsePrefix(r.Subnet + "/" + strconv.Itoa(r.SubnetSize))
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("rule %d has the subnet %s/%d, not a prefix", r.ID, r.Subnet, r.SubnetSize)
		}
		fromSubnet = p.Masked()
	}
	if r.Source == "" {
		return fromSubnet, nil
	}
	p, err := netip.ParsePrefix(r.Source)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("rule %d uses the source %q, only CIDR sources are translated", r.ID, r.Source)
	}
	p = p.Masked()
	if fromSubnet.IsValid() && fromSubnet != p {
		return netip.Prefix{}, fmt.Errorf("rule %d has the source %s and the subnet %s", r.ID, p, fromSubnet)
	}
	return p, nil
}

// nftPorts turns Vultr's "22" or "8000:9000" into nftables' "22" or "8000-9000".
func nftPorts(port string) (string, error) {
	m := portRange.FindStringSubmatch(port)
	if m == nil {
		return "", fmt.Errorf("the port %q is not a port or a range", port)
	}
	from, _ := strconv.Atoi(m[1])
	to := from
	if m[2] != "" {
		to, _ = strconv.Atoi(m[2])
	}
	if from < 1 || to > 65535 || from > to {
		return "", fmt.Errorf("the port %q is out of range", port)
	}
	if from == to {
		return strconv.Itoa(from), nil
	}
	return fmt.Sprintf("%d-%d", from, to), nil
}

// HostFirewallFragment is the boot fragment that filters a node's public
// interface like the firewall group with these rules would: inbound is dropped
// unless it is loopback, traffic of a connection the node opened, ICMPv6
// neighbour discovery or what a rule accepts; other interfaces (VPC, Pods) are
// not filtered. The unit loads after the network is up, when the public
// interfaces are known, and the kubelet requires it: a node whose rules did not
// load never joins.
func HostFirewallFragment(rules []govultr.FirewallRule) (vendors.BootFragment, error) {
	var b strings.Builder
	b.WriteString("variant: flatcar\nversion: 1.1.0\nstorage:\n  files:\n")
	b.WriteString("    - path: /etc/scarpenter/host-firewall.nft\n      mode: 0644\n      contents:\n        inline: |\n")
	b.WriteString("          add table inet scarpenter_host\n")
	b.WriteString("          delete table inet scarpenter_host\n")
	b.WriteString("          table inet scarpenter_host {\n")
	b.WriteString("            chain prerouting {\n")
	// prerouting, after conntrack (-200) and before the NAT of kube-proxy: the rules
	// see the original destination port of NodePorts and hostPorts, whose packets
	// are forwarded, not delivered to the node, so an input hook would miss them.
	b.WriteString("              type filter hook prerouting priority mangle; policy drop;\n")
	b.WriteString("              iifname \"lo\" accept\n")
	fmt.Fprintf(&b, "              iifname != { %s } accept\n", publicIfPlaceholder)
	b.WriteString("              ct state established,related accept\n")
	b.WriteString("              ct state invalid drop\n")
	b.WriteString("              icmpv6 type { nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n")
	for _, r := range rules {
		line, err := hostFirewallRule(r)
		if err != nil {
			return vendors.BootFragment{}, fmt.Errorf("translating the firewall group's rules: %w", err)
		}
		fmt.Fprintf(&b, "              %s\n", line)
	}
	b.WriteString("            }\n          }\n")
	b.WriteString(hostFirewallUnits)
	return vendors.BootFragment{Name: HostFirewallFragmentName, Butane: b.String()}, nil
}

// hostFirewallUnits are the script that loads the ruleset for the interface of
// the default route, the unit that runs it and the kubelet's dependency on it.
const hostFirewallUnits = `    - path: /opt/scarpenter/bin/scarpenter-host-firewall
      mode: 0755
      contents:
        inline: |
          #!/bin/sh
          set -eu
          devs=$({ ip -4 route show default; ip -6 route show default; } |
            awk '{ for (i = 1; i < NF; i++) if ($i == "dev") print "\"" $(i + 1) "\"" }' | sort -u | paste -sd, -)
          if [ -z "$devs" ]; then
            echo "no default route: the public interface is unknown" >&2
            exit 1
          fi
          sed "s/` + publicIfPlaceholder + `/$devs/" /etc/scarpenter/host-firewall.nft | nft -f -
systemd:
  units:
    - name: scarpenter-host-firewall.service
      enabled: true
      contents: |
        [Unit]
        Description=Filter the public interface like the Vultr firewall group
        After=network-online.target
        Wants=network-online.target
        Before=kubelet.service
        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/opt/scarpenter/bin/scarpenter-host-firewall
        [Install]
        WantedBy=multi-user.target
    - name: kubelet.service
      dropins:
        - name: 16-scarpenter-host-firewall.conf
          contents: |
            [Unit]
            Requires=scarpenter-host-firewall.service
            After=scarpenter-host-firewall.service
`
