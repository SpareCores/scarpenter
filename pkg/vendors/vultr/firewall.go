/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

// MaxFirewallRules is the number of rules a Vultr firewall group takes.
const MaxFirewallRules = 50

// allPorts is what a tcp or udp rule without ports covers: Vultr has no "all
// protocols" rule.
const allPorts = "1:65535"

// FirewallRules turns the generic inbound rules into Vultr's: a rule becomes
// one rule per CIDR, and "any" becomes tcp, udp (all ports) and icmp. Identical
// rules are kept once. Everything not allowed is denied by the group's default.
// More than MaxFirewallRules is an error (the NodeClassValidator rejects it
// earlier).
func FirewallRules(in *common.Inbound) ([]govultr.FirewallRuleReq, error) {
	if in == nil {
		return nil, nil
	}
	var out []govultr.FirewallRuleReq
	add := func(r govultr.FirewallRuleReq) {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	for i, rule := range in.Rules {
		ports := ""
		if rule.FromPort != nil {
			ports = strconv.Itoa(int(*rule.FromPort))
			if rule.ToPort != nil && *rule.ToPort != *rule.FromPort {
				ports += ":" + strconv.Itoa(int(*rule.ToPort))
			}
		}
		var protocols []string
		switch rule.Protocol {
		case common.Protocol("tcp"), common.Protocol("udp"), common.Protocol("icmp"):
			protocols = []string{string(rule.Protocol)}
		case common.Protocol("any"):
			protocols = []string{"tcp", "udp", "icmp"}
		default:
			return nil, fmt.Errorf("inbound rule %d: unknown protocol %q", i, rule.Protocol)
		}
		for _, c := range rule.CIDRs {
			p, err := netip.ParsePrefix(c)
			if err != nil {
				return nil, fmt.Errorf("inbound rule %d: %w", i, err)
			}
			p = p.Masked()
			ipType := "v4"
			if p.Addr().Is6() {
				ipType = "v6"
			}
			for _, proto := range protocols {
				r := govultr.FirewallRuleReq{IPType: ipType, Protocol: proto, Subnet: p.Addr().String(), SubnetSize: p.Bits()}
				if proto != "icmp" {
					r.Port = ports
					if r.Port == "" {
						r.Port = allPorts
					}
				}
				add(r)
			}
		}
	}
	if len(out) > MaxFirewallRules {
		return nil, fmt.Errorf("the inbound rules need %d firewall rules, a Vultr group takes %d", len(out), MaxFirewallRules)
	}
	return out, nil
}

// FirewallGroupInUse is the InUse of a firewall group: instances use it.
func FirewallGroupInUse(g govultr.FirewallGroup) bool { return g.InstanceCount > 0 }
