/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// Inbound is one allowed inbound flow of the NodeClass (spec.networking.inbound),
// one per rule and CIDR.
type Inbound struct {
	// CIDR is the source; an empty one means anywhere (both families).
	CIDR string
	// Protocol is tcp, udp or icmp; empty means any.
	Protocol string
	// PortStart and PortEnd are the destination ports (0 for icmp).
	PortStart, PortEnd int
}

// TailscalePort is the UDP port of Tailscale's direct paths.
const TailscalePort = 41641

// EphemeralPorts is Flatcar's default ephemeral range: the replies to the
// node's own connections come back there (the firewall is stateless).
const (
	EphemeralStart = 32768
	EphemeralEnd   = 60999
)

// FirewallOptions select the built-in rules.
type FirewallOptions struct {
	Inbound []Inbound
	// Tailscale opens UDP 41641 for direct paths (the tailscale template only).
	Tailscale bool
}

type ruleBuilder struct{ rules []upcloud.FirewallRule }

func (b *ruleBuilder) add(r upcloud.FirewallRule) { b.rules = append(b.rules, r) }

// accept adds an accept-in rule for both address families (or the family of the CIDR).
func (b *ruleBuilder) accept(comment, proto string, ports [2]int, cidr string, families ...string) {
	for _, fam := range families {
		r := upcloud.FirewallRule{
			Action: upcloud.FirewallRuleActionAccept, Direction: upcloud.FirewallRuleDirectionIn, Family: fam,
			Protocol: proto, Comment: comment,
		}
		if ports[0] != 0 {
			r.DestinationPortStart, r.DestinationPortEnd = strconv.Itoa(ports[0]), strconv.Itoa(ports[1])
		}
		if cidr != "" {
			p := netip.MustParsePrefix(cidr).Masked()
			r.SourceAddressStart, r.SourceAddressEnd = p.Addr().String(), lastAddr(p).String()
		}
		b.add(r)
	}
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr().AsSlice()
	for i := range a {
		bits := min(max(p.Bits()-8*i, 0), 8)
		a[i] |= byte(0xff >> bits)
	}
	out, _ := netip.AddrFromSlice(a)
	return out
}

var bothFamilies = []string{upcloud.IPAddressFamilyIPv4, upcloud.IPAddressFamilyIPv6}

// PublicFirewallRules builds the Public & Utility firewall of every node
// (plan 19, "Node settings"). The firewall is stateless and the first match
// wins, so replies are allowed explicitly: ICMP (ICMPv6 needs it for neighbour
// discovery) and DHCP replies; the NodeClass's inbound rules; Tailscale's UDP
// port when asked; the ephemeral range for TCP and UDP; then drop in. Outbound
// is accepted. The list is an iteration target of the L5 lane, not final.
func PublicFirewallRules(o FirewallOptions) ([]upcloud.FirewallRule, error) {
	var b ruleBuilder
	// ICMP and, for the IPv6 family, ICMPv6 (the API's protocol is icmp for both).
	b.accept("icmp", upcloud.FirewallRuleProtocolICMP, [2]int{}, "", bothFamilies...)
	b.add(upcloud.FirewallRule{
		Action: upcloud.FirewallRuleActionAccept, Direction: upcloud.FirewallRuleDirectionIn,
		Family: upcloud.IPAddressFamilyIPv4, Protocol: upcloud.FirewallRuleProtocolUDP, Comment: "dhcp reply",
		SourcePortStart: "67", SourcePortEnd: "67", DestinationPortStart: "68", DestinationPortEnd: "68",
	})
	b.add(upcloud.FirewallRule{
		Action: upcloud.FirewallRuleActionAccept, Direction: upcloud.FirewallRuleDirectionIn,
		Family: upcloud.IPAddressFamilyIPv6, Protocol: upcloud.FirewallRuleProtocolUDP, Comment: "dhcpv6 reply",
		SourcePortStart: "547", SourcePortEnd: "547", DestinationPortStart: "546", DestinationPortEnd: "546",
	})
	for _, in := range o.Inbound {
		fams := bothFamilies
		if in.CIDR != "" {
			p, err := netip.ParsePrefix(in.CIDR)
			if err != nil {
				return nil, fmt.Errorf("inbound CIDR %q: %w", in.CIDR, err)
			}
			fams = []string{upcloud.IPAddressFamilyIPv4}
			if p.Addr().Is6() {
				fams = []string{upcloud.IPAddressFamilyIPv6}
			}
		}
		switch in.Protocol {
		case upcloud.FirewallRuleProtocolTCP, upcloud.FirewallRuleProtocolUDP:
			if in.PortStart < 1 || in.PortEnd < in.PortStart || in.PortEnd > 65535 {
				return nil, fmt.Errorf("inbound ports %d-%d are not a range", in.PortStart, in.PortEnd)
			}
		case upcloud.FirewallRuleProtocolICMP, "":
			// icmp and any have no ports.
		default:
			return nil, fmt.Errorf("inbound protocol %q is not tcp, udp, icmp or any", in.Protocol)
		}
		b.accept("inbound", in.Protocol, [2]int{in.PortStart, in.PortEnd}, in.CIDR, fams...)
	}
	if o.Tailscale {
		b.accept("tailscale", upcloud.FirewallRuleProtocolUDP, [2]int{TailscalePort, TailscalePort}, "", bothFamilies...)
	}
	for _, proto := range []string{upcloud.FirewallRuleProtocolTCP, upcloud.FirewallRuleProtocolUDP} {
		b.accept("ephemeral", proto, [2]int{EphemeralStart, EphemeralEnd}, "", bothFamilies...)
	}
	b.add(upcloud.FirewallRule{
		Action: upcloud.FirewallRuleActionDrop, Direction: upcloud.FirewallRuleDirectionIn, Comment: "default",
	})
	b.add(upcloud.FirewallRule{
		Action: upcloud.FirewallRuleActionAccept, Direction: upcloud.FirewallRuleDirectionOut, Comment: "outbound",
	})
	for i := range b.rules {
		b.rules[i].Position = i + 1
	}
	if len(b.rules) > 1000 {
		return nil, fmt.Errorf("%d firewall rules exceed the limit of 1000", len(b.rules))
	}
	return b.rules, nil
}

// WriteFirewall replaces the server's Public & Utility firewall rules. Rollout
// takes 1 to 2 minutes (spike S3 measures it); the account's default rules
// apply until it does.
func WriteFirewall(ctx context.Context, a api.API, serverUUID string, rules []upcloud.FirewallRule) error {
	ctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	return Classify(a.CreateFirewallRules(ctx, &request.CreateFirewallRulesRequest{
		ServerUUID: serverUUID, FirewallRules: request.FirewallRuleSlice(rules),
	}))
}
