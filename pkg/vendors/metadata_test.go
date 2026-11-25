/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

type endpointsDriver struct {
	vendors.Driver
	ep vendors.MetadataEndpoints
}

func (d endpointsDriver) Vendor() string                               { return "x" }
func (d endpointsDriver) MetadataEndpoints() vendors.MetadataEndpoints { return d.ep }

type plainDriver struct{ vendors.Driver }

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

func TestRestrictMetadata(t *testing.T) {
	// A driver without the capability, or without addresses, gets nothing.
	if _, ok, err := vendors.RestrictMetadata(plainDriver{}); ok || err != nil {
		t.Errorf("a plain driver: %v, %v", ok, err)
	}
	if _, ok, err := vendors.RestrictMetadata(endpointsDriver{}); ok || err != nil {
		t.Errorf("no addresses: %v, %v", ok, err)
	}

	f, ok, err := vendors.RestrictMetadata(endpointsDriver{ep: vendors.MetadataEndpoints{
		Addresses: addrs("169.254.169.254", "::ffff:168.63.129.16", "fd00:ec2::254"),
		Units:     []string{"agent.service", "tmpl@.service"},
	}})
	if err != nil || !ok || f.Name != "metadata-access" {
		t.Fatalf("RestrictMetadata = %+v, %v, %v", f, ok, err)
	}
	for _, want := range []string{
		// IPv4 and IPv6 in their own files; a v4-mapped address is a v4 address.
		"/etc/scarpenter/metadata-iptables.rules", "-d 169.254.169.254/32 -p tcp -m tcp ! --dport 53 --syn -j DROP",
		"-d 168.63.129.16/32 -p tcp -m tcp ! --dport 53 --syn -m cgroup --path scarpenter.slice/scarpenter-metadata.slice -j RETURN",
		"/etc/scarpenter/metadata-ip6tables.rules", "-d fd00:ec2::254/128 -p tcp -m tcp ! --dport 53 --syn -j DROP",
		// the forwarded Pod traffic is dropped for every address
		"-A SCARPENTER-IMDS-FORWARD -d 169.254.169.254/32", "-A SCARPENTER-IMDS-FORWARD -d fd00:ec2::254/128",
		// Afterburn's units and the driver's are in the slice
		"- name: coreos-metadata.service", "- name: coreos-metadata-sshkeys@.service", "- name: agent.service", "- name: tmpl@.service",
		// and start after the rules, which a recreated slice needs loaded again
		"Wants=scarpenter-metadata-firewall.service\n            After=scarpenter-metadata-firewall.service\n",
	} {
		if !strings.Contains(f.Butane, want) {
			t.Errorf("the fragment lacks %q:\n%s", want, f.Butane)
		}
	}
	if strings.Contains(f.Butane, "::ffff") {
		t.Error("a v4-mapped address was written as IPv6")
	}

	// An IPv4-only vendor gets no IPv6 rules file.
	f, _, _ = vendors.RestrictMetadata(endpointsDriver{ep: vendors.MetadataEndpoints{Addresses: addrs("100.100.100.200")}})
	if strings.Contains(f.Butane, "- path: /etc/scarpenter/metadata-ip6tables.rules") {
		t.Errorf("an IPv6 rules file for an IPv4-only vendor:\n%s", f.Butane)
	}

	for name, ep := range map[string]vendors.MetadataEndpoints{
		"a unit with a space":       {Addresses: addrs("1.2.3.4"), Units: []string{"a b.service"}},
		"a unit that is no service": {Addresses: addrs("1.2.3.4"), Units: []string{"a.timer"}},
		"a unit with a newline":     {Addresses: addrs("1.2.3.4"), Units: []string{"a.service\nExecStart=x"}},
		"an invalid address":        {Addresses: []netip.Addr{{}}},
	} {
		if _, _, err := vendors.RestrictMetadata(endpointsDriver{ep: ep}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestHostOnlyMetadata(t *testing.T) {
	if _, ok, err := vendors.HostOnlyMetadata(endpointsDriver{}); ok || err != nil {
		t.Errorf("no addresses: %v, %v", ok, err)
	}
	f, ok, err := vendors.HostOnlyMetadata(endpointsDriver{ep: vendors.MetadataEndpoints{
		Addresses: addrs("169.254.169.254", "fd00:ec2::254"), Units: []string{"agent.service"},
	}})
	if err != nil || !ok || f.Name != "metadata-access" {
		t.Fatalf("HostOnlyMetadata = %+v, %v, %v", f, ok, err)
	}
	for _, want := range []string{
		"-A SCARPENTER-IMDS-FORWARD -d 169.254.169.254/32 -p tcp -m tcp ! --dport 53 --syn -j DROP",
		"-A SCARPENTER-IMDS-FORWARD -d fd00:ec2::254/128 -p tcp -m tcp ! --dport 53 --syn -j DROP",
		":SCARPENTER-IMDS-OUTPUT - [0:0]", // the script hooks the chain, which stays empty
		"scarpenter-metadata.slice", "scarpenter-metadata-firewall.service", "- name: agent.service", "Requires=scarpenter-metadata-firewall.service",
	} {
		if !strings.Contains(f.Butane, want) {
			t.Errorf("the fragment lacks %q:\n%s", want, f.Butane)
		}
	}
	for _, bad := range []string{"-A SCARPENTER-IMDS-OUTPUT", "-m cgroup"} {
		if strings.Contains(f.Butane, bad) {
			t.Errorf("the HostOnly fragment has %q:\n%s", bad, f.Butane)
		}
	}
}
