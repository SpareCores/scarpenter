/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/
package hcloud

import (
	"strings"
	"testing"

	hc "github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func baseSpec() LaunchSpec {
	return LaunchSpec{
		Name: "nc-1", ServerType: "cpx22", Location: "fsn1", ImageID: 99, UserData: []byte(`{"ignition":{}}`),
		Tags:           map[vendors.TagKey]string{vendors.TagCluster: "c", vendors.TagNodeClaim: "nc-1", vendors.TagManagedBy: "scarpenter"},
		ExtraTags:      map[string]string{"team": "infra"},
		IdempotencyKey: "uid-1", PublicIPv4: true,
	}
}

func TestBuildCreateOptsMinimal(t *testing.T) {
	o, err := BuildCreateOpts(baseSpec())
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "nc-1" || o.ServerType.Name != "cpx22" || o.Location.Name != "fsn1" || o.Image.ID != 99 || o.UserData != `{"ignition":{}}` {
		t.Errorf("%+v", o)
	}
	if o.StartAfterCreate == nil || !*o.StartAfterCreate {
		t.Error("start_after_create")
	}
	if o.PublicNet == nil || !o.PublicNet.EnableIPv4 || o.PublicNet.EnableIPv6 {
		t.Errorf("public_net %+v", o.PublicNet)
	}
	if len(o.Networks)+len(o.Firewalls)+len(o.Volumes)+len(o.SSHKeys) != 0 || o.PlacementGroup != nil || o.Automount != nil {
		t.Errorf("optional fields set: %+v", o)
	}
	want := map[string]string{
		"scarpenter.sparecores.com/cluster": "c", "scarpenter.sparecores.com/nodeclaim": "nc-1",
		"scarpenter.sparecores.com/managed-by": "scarpenter", "scarpenter.sparecores.com/idempotency-key": "uid-1",
		"scarpenter.sparecores.com/capacity-type": "on-demand", "team": "infra",
	}
	if len(o.Labels) != len(want) {
		t.Errorf("labels %v", o.Labels)
	}
	for k, v := range want {
		if o.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, o.Labels[k], v)
		}
	}
}

func TestBuildCreateOptsFull(t *testing.T) {
	s := baseSpec()
	s.PublicIPv6, s.NetworkID, s.FirewallID, s.PlacementGroupID, s.VolumeIDs, s.SSHKeyID = true, 11, 12, 13, []int64{21, 22}, 5
	o, err := BuildCreateOpts(s)
	if err != nil {
		t.Fatal(err)
	}
	if !o.PublicNet.EnableIPv6 || o.Networks[0].ID != 11 || o.Firewalls[0].Firewall.ID != 12 || o.PlacementGroup.ID != 13 ||
		len(o.Volumes) != 2 || o.Volumes[1].ID != 22 || o.SSHKeys[0].ID != 5 {
		t.Errorf("%+v", o)
	}
	if o.Automount == nil || *o.Automount {
		t.Error("automount must be false with volumes")
	}
}

func TestBuildCreateOptsNames(t *testing.T) {
	s := baseSpec()
	s.Name = strings.Repeat("n", 80)
	o, err := BuildCreateOpts(s)
	if err != nil || len(o.Name) > MaxServerName || o.Name == s.Name {
		t.Errorf("%q %v", o.Name, err)
	}
}

func TestBuildCreateOptsErrors(t *testing.T) {
	for name, mod := range map[string]func(*LaunchSpec){
		"user data too large": func(s *LaunchSpec) { s.UserData = make([]byte, MaxUserData+1) },
		"no public address":   func(s *LaunchSpec) { s.PublicIPv4 = false },
		"no image":            func(s *LaunchSpec) { s.ImageID = 0 },
		"no type":             func(s *LaunchSpec) { s.ServerType = "" },
		"bad extra key":       func(s *LaunchSpec) { s.ExtraTags = map[string]string{"bad key": "x"} },
	} {
		s := baseSpec()
		mod(&s)
		if _, err := BuildCreateOpts(s); vendors.ClassOf(err) != vendors.Fatal {
			t.Errorf("%s: %v", name, err)
		}
	}
	s := baseSpec()
	s.UserData = make([]byte, MaxUserData+1)
	if _, err := BuildCreateOpts(s); vendors.CodeOf(err) != CodeUserDataTooLarge {
		t.Errorf("code %q", vendors.CodeOf(err))
	}
	s = baseSpec()
	s.UserData = make([]byte, MaxUserData)
	if _, err := BuildCreateOpts(s); err != nil {
		t.Errorf("exactly the limit is fine: %v", err)
	}
	// IPv6-only is accepted (the warning belongs to the validator).
	s = baseSpec()
	s.PublicIPv4, s.PublicIPv6 = false, true
	if _, err := BuildCreateOpts(s); err != nil {
		t.Error(err)
	}
}

func TestVolumeOpts(t *testing.T) {
	o, err := BuildVolumeOpts(VolumeSpec{Name: "scarpenter-c-volumes-nc-1-pgdata", Location: "fsn1", SizeGB: 107,
		Tags: map[vendors.TagKey]string{vendors.TagCluster: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if o.Size != 107 || o.Location.Name != "fsn1" || o.Format != nil || o.Server != nil || o.Automount == nil || *o.Automount ||
		o.Labels["scarpenter.sparecores.com/cluster"] != "c" {
		t.Errorf("%+v", o)
	}
	for _, gb := range []int{0, 9, 10241} {
		if _, err := BuildVolumeOpts(VolumeSpec{Name: "v", Location: "fsn1", SizeGB: gb}); err == nil {
			t.Errorf("%d GB", gb)
		}
	}
	for _, gb := range []int{10, 10240} {
		if _, err := BuildVolumeOpts(VolumeSpec{Name: "v", Location: "fsn1", SizeGB: gb}); err != nil {
			t.Errorf("%d GB: %v", gb, err)
		}
	}
	long := strings.Repeat("v", 90)
	if o, _ := BuildVolumeOpts(VolumeSpec{Name: long, Location: "fsn1", SizeGB: 10}); len(o.Name) > MaxVolumeName {
		t.Errorf("name %q", o.Name)
	}
}

func TestVolumeSizeGB(t *testing.T) {
	for in, want := range map[int64]int{100 << 30: 108, 10_000_000_000: 10, 10_000_000_001: 11, 1: 1} {
		if got := VolumeSizeGB(in); got != want {
			t.Errorf("%d: %d, want %d", in, got, want)
		}
	}
}

func TestFirewallRules(t *testing.T) {
	rules, err := FirewallRules([]common.InboundRule{
		{Protocol: "tcp", FromPort: new(int32(22)), CIDRs: []string{"10.0.0.0/8", "2001:db8::/32"}},
		{Protocol: "tcp", FromPort: new(int32(8000)), ToPort: new(int32(8010)), CIDRs: []string{"0.0.0.0/0"}},
		{Protocol: "icmp", CIDRs: []string{"0.0.0.0/0"}},
		{Protocol: "any", FromPort: nil, CIDRs: []string{"192.0.2.0/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 6 {
		t.Fatalf("%d rules", len(rules))
	}
	if *rules[0].Port != "22" || len(rules[0].SourceIPs) != 2 || rules[0].Direction != hc.FirewallRuleDirectionIn || rules[0].Protocol != hc.FirewallRuleProtocolTCP {
		t.Errorf("%+v", rules[0])
	}
	if *rules[1].Port != "8000-8010" {
		t.Errorf("range: %v", *rules[1].Port)
	}
	if rules[2].Port != nil || rules[2].Protocol != hc.FirewallRuleProtocolICMP {
		t.Errorf("icmp %+v", rules[2])
	}
	if rules[3].Protocol != hc.FirewallRuleProtocolTCP || rules[4].Protocol != hc.FirewallRuleProtocolUDP || rules[5].Protocol != hc.FirewallRuleProtocolICMP {
		t.Error("any must become tcp, udp, icmp")
	}
	if rules[3].Port == nil || *rules[3].Port != "1-65535" || rules[4].Port == nil || *rules[4].Port != "1-65535" || rules[5].Port != nil {
		t.Errorf("tcp and udp without ports need the full range: %v %v %v", rules[3].Port, rules[4].Port, rules[5].Port)
	}
	if got, err := FirewallRules(nil); err != nil || got != nil {
		t.Errorf("no rules: %v %v", got, err)
	}
	anyPorts, _ := FirewallRules([]common.InboundRule{{Protocol: "any", FromPort: new(int32(53)), CIDRs: []string{"0.0.0.0/0"}}})
	if anyPorts[0].Port == nil || anyPorts[1].Port == nil || anyPorts[2].Port != nil {
		t.Error("ports apply to tcp and udp only")
	}
	if _, err := FirewallRules([]common.InboundRule{{Protocol: "tcp", CIDRs: []string{"not-a-cidr"}}}); err == nil {
		t.Error("bad cidr")
	}
	many := make([]common.InboundRule, 170)
	for i := range many {
		many[i] = common.InboundRule{Protocol: "any", CIDRs: []string{"10.0.0.0/8"}}
	}
	if _, err := FirewallRules(many); err == nil {
		t.Error("510 rules exceed the limit")
	}
}
