/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
)

// The VPC unit reads the metadata service, which the restricted access drops for
// every process outside the metadata slice.
func TestRestrictedMetadataPutsTheVPCUnitInTheSlice(t *testing.T) {
	f, ok, err := vendors.RestrictMetadata(vultr.New())
	if err != nil || !ok {
		t.Fatalf("RestrictMetadata = %v, %v", ok, err)
	}
	if !strings.Contains(f.Butane, "- name: scarpenter-vpc-interface.service") {
		t.Errorf("the VPC unit is not in the metadata slice:\n%s", f.Butane)
	}
}

func TestVPCInterfaceFragmentConfiguresTheNICByMACWithoutARoute(t *testing.T) {
	frag := vultr.VPCInterfaceFragment()
	if frag.Name != vultr.VPCInterfaceFragmentName {
		t.Errorf("name %q", frag.Name)
	}
	for _, want := range []string{
		"http://169.254.169.254/v1.json",
		`."network-type" == "private"`,
		"[.mac, .ipv4.address, .ipv4.netmask, (.mtu // 1450)]",
		"MTUBytes=%s",
		"variant: flatcar",
		"MACAddress=%s",
		"Address=%s/%s",
		"/etc/systemd/network/10-scarpenter-vpc-",
		"Before=network-online.target kubelet.service",
		"Wants=scarpenter-vpc-interface.service",
		"scarpenter-vpc-interface.service\n",
	} {
		if !strings.Contains(frag.Butane, want) {
			t.Errorf("fragment lacks %q", want)
		}
	}
	// The kubelet only wants the unit, and the VPC never gets a gateway or a route.
	for _, bad := range []string{"Requires=", "Gateway", "Route", "DHCP"} {
		if strings.Contains(frag.Butane, bad) {
			t.Errorf("fragment has %q", bad)
		}
	}
}
