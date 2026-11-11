/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"net/netip"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

func TestPublicFirewallRulesGolden(t *testing.T) {
	rules, err := PublicFirewallRules(FirewallOptions{
		Tailscale: true,
		Inbound: []Inbound{
			{CIDR: "198.51.100.0/24", Protocol: "tcp", PortStart: 22, PortEnd: 22},
			{Protocol: "tcp", PortStart: 443, PortEnd: 443},
			{CIDR: "2001:db8:1::/48", Protocol: "udp", PortStart: 53, PortEnd: 53},
			{CIDR: "203.0.113.7/32", Protocol: "icmp"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "firewall_rules.golden.json", rules)
}

func TestPublicFirewallRulesShape(t *testing.T) {
	rules, err := PublicFirewallRules(FirewallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Stateless, first match wins: the drop-in default is the last inbound rule,
	// then outbound accept; positions run from 1.
	n := len(rules)
	if last := rules[n-1]; last.Direction != "out" || last.Action != "accept" {
		t.Errorf("last rule = %+v, want accept out", last)
	}
	if def := rules[n-2]; def.Direction != "in" || def.Action != "drop" || def.Protocol != "" {
		t.Errorf("default rule = %+v, want drop in", def)
	}
	for i, r := range rules {
		if r.Position != i+1 {
			t.Errorf("rule %d has position %d", i, r.Position)
		}
		if r.Direction == "in" && r.Action == "accept" && r.Comment == "tailscale" {
			t.Errorf("tailscale rule without the option")
		}
	}
	var eph int
	for _, r := range rules {
		if r.DestinationPortStart == "32768" && r.DestinationPortEnd == "60999" {
			eph++
		}
	}
	if eph != 4 {
		t.Errorf("ephemeral rules = %d, want tcp and udp for both families", eph)
	}
}

func TestPublicFirewallRulesRangeEnds(t *testing.T) {
	rules, err := PublicFirewallRules(FirewallOptions{Inbound: []Inbound{
		{CIDR: "198.51.100.77/24", Protocol: "tcp", PortStart: 22, PortEnd: 22},
		{CIDR: "2001:db8:1::/48", Protocol: "tcp", PortStart: 22, PortEnd: 22},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var v4, v6 *upcloud.FirewallRule
	for i, r := range rules {
		switch r.SourceAddressStart {
		case "198.51.100.0":
			v4 = &rules[i]
		case "2001:db8:1::":
			v6 = &rules[i]
		}
	}
	if v4 == nil || v4.SourceAddressEnd != "198.51.100.255" || v4.Family != "IPv4" {
		t.Errorf("v4 rule = %+v", v4)
	}
	if v6 == nil || v6.SourceAddressEnd != "2001:db8:1:ffff:ffff:ffff:ffff:ffff" || v6.Family != "IPv6" {
		t.Errorf("v6 rule = %+v", v6)
	}
}

func TestLastAddr(t *testing.T) {
	for cidr, want := range map[string]string{
		"10.0.0.0/8": "10.255.255.255", "10.1.2.3/32": "10.1.2.3", "0.0.0.0/0": "255.255.255.255",
		"192.168.1.0/25": "192.168.1.127", "fe80::/10": "febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
	} {
		if got := lastAddr(netip.MustParsePrefix(cidr)).String(); got != want {
			t.Errorf("%s: %s, want %s", cidr, got, want)
		}
	}
}

func TestPublicFirewallRulesInvalid(t *testing.T) {
	for name, in := range map[string]Inbound{
		"bad cidr":     {CIDR: "nope", Protocol: "tcp", PortStart: 1, PortEnd: 1},
		"bad protocol": {Protocol: "gre"},
		"zero port":    {Protocol: "tcp"},
		"reversed":     {Protocol: "udp", PortStart: 90, PortEnd: 80},
		"too high":     {Protocol: "udp", PortStart: 1, PortEnd: 70000},
	} {
		if _, err := PublicFirewallRules(FirewallOptions{Inbound: []Inbound{in}}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := make([]Inbound, 600)
	for i := range many {
		many[i] = Inbound{Protocol: "tcp", PortStart: 1000 + i, PortEnd: 1000 + i}
	}
	if _, err := PublicFirewallRules(FirewallOptions{Inbound: many}); err == nil {
		t.Error("more than 1000 rules accepted")
	}
}

func TestWriteFirewallOnFake(t *testing.T) {
	e := newEnv(t, fake.Options{})
	d := e.launch("nc-fw", "1xCPU-2GB")
	rules, err := PublicFirewallRules(FirewallOptions{Tailscale: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFirewall(e.ctx, e.f, d.UUID, rules); err != nil {
		t.Fatal(err)
	}
	if got := e.f.ServerFirewall(d.UUID); len(got) != len(rules) || got[0].Position != 1 {
		t.Fatalf("server has %d rules", len(got))
	}
	// A second write replaces: the call is idempotent.
	if err := WriteFirewall(e.ctx, e.f, d.UUID, rules[:3]); err != nil {
		t.Fatal(err)
	}
	if got := e.f.ServerFirewall(d.UUID); len(got) != 3 {
		t.Fatalf("after replace: %d rules", len(got))
	}
	if err := WriteFirewall(e.ctx, e.f, "00000000-0000-4000-8000-000000000000", rules); !isGone(err) {
		t.Errorf("unknown server: %v", err)
	}
}
