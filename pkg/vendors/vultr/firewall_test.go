/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vultr/govultr/v3"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
)

func inbound(rules ...common.InboundRule) *common.Inbound { return &common.Inbound{Rules: rules} }

func TestFirewallRules(t *testing.T) {
	from, to := int32(8000), int32(8100)
	one := int32(22)
	cases := []struct {
		name string
		in   *common.Inbound
		want []govultr.FirewallRuleReq
	}{
		{"no rules", nil, nil},
		{"deny all", inbound(), nil},
		{"tcp port", inbound(common.InboundRule{Protocol: "tcp", FromPort: &one, CIDRs: []string{"192.0.2.0/24"}}),
			[]govultr.FirewallRuleReq{{IPType: "v4", Protocol: "tcp", Subnet: "192.0.2.0", SubnetSize: 24, Port: "22"}}},
		{"range, host bits masked", inbound(common.InboundRule{Protocol: "udp", FromPort: &from, ToPort: &to, CIDRs: []string{"192.0.2.9/24"}}),
			[]govultr.FirewallRuleReq{{IPType: "v4", Protocol: "udp", Subnet: "192.0.2.0", SubnetSize: 24, Port: "8000:8100"}}},
		{"tcp without ports covers all ports", inbound(common.InboundRule{Protocol: "tcp", CIDRs: []string{"0.0.0.0/0"}}),
			[]govultr.FirewallRuleReq{{IPType: "v4", Protocol: "tcp", Subnet: "0.0.0.0", SubnetSize: 0, Port: "1:65535"}}},
		{"icmp has no port, v6", inbound(common.InboundRule{Protocol: "icmp", CIDRs: []string{"2001:db8::/32"}}),
			[]govultr.FirewallRuleReq{{IPType: "v6", Protocol: "icmp", Subnet: "2001:db8::", SubnetSize: 32}}},
		{"any is tcp, udp and icmp per CIDR", inbound(common.InboundRule{Protocol: "any", CIDRs: []string{"10.0.0.0/8", "2001:db8::/32"}}),
			[]govultr.FirewallRuleReq{
				{IPType: "v4", Protocol: "tcp", Subnet: "10.0.0.0", SubnetSize: 8, Port: "1:65535"},
				{IPType: "v4", Protocol: "udp", Subnet: "10.0.0.0", SubnetSize: 8, Port: "1:65535"},
				{IPType: "v4", Protocol: "icmp", Subnet: "10.0.0.0", SubnetSize: 8},
				{IPType: "v6", Protocol: "tcp", Subnet: "2001:db8::", SubnetSize: 32, Port: "1:65535"},
				{IPType: "v6", Protocol: "udp", Subnet: "2001:db8::", SubnetSize: 32, Port: "1:65535"},
				{IPType: "v6", Protocol: "icmp", Subnet: "2001:db8::", SubnetSize: 32},
			}},
		{"duplicates are kept once", inbound(
			common.InboundRule{Protocol: "tcp", FromPort: &one, CIDRs: []string{"192.0.2.0/24"}},
			common.InboundRule{Protocol: "tcp", FromPort: &one, ToPort: &one, CIDRs: []string{"192.0.2.0/24"}}),
			[]govultr.FirewallRuleReq{{IPType: "v4", Protocol: "tcp", Subnet: "192.0.2.0", SubnetSize: 24, Port: "22"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FirewallRules(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %+v", got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("rule %d: %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestFirewallRulesErrors(t *testing.T) {
	if _, err := FirewallRules(inbound(common.InboundRule{Protocol: "gre", CIDRs: []string{"10.0.0.0/8"}})); err == nil {
		t.Error("unknown protocol accepted")
	}
	if _, err := FirewallRules(inbound(common.InboundRule{Protocol: "tcp", CIDRs: []string{"not-a-cidr"}})); err == nil {
		t.Error("bad CIDR accepted")
	}
	// 17 "any" CIDRs make 51 rules; 16 make 48.
	cidrs := make([]string, 0, 17)
	for i := range 17 {
		cidrs = append(cidrs, fmt.Sprintf("10.%d.0.0/16", i))
	}
	_, err := FirewallRules(inbound(common.InboundRule{Protocol: "any", CIDRs: cidrs}))
	if err == nil || !strings.Contains(err.Error(), "51") {
		t.Errorf("over the limit: %v", err)
	}
	if got, err := FirewallRules(inbound(common.InboundRule{Protocol: "any", CIDRs: cidrs[:16]})); err != nil || len(got) != 48 {
		t.Errorf("under the limit: %d %v", len(got), err)
	}
}

func TestFirewallGroupInUse(t *testing.T) {
	if FirewallGroupInUse(govultr.FirewallGroup{}) || !FirewallGroupInUse(govultr.FirewallGroup{InstanceCount: 1}) {
		t.Fatal("InUse is instance_count > 0")
	}
}
