/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
)

func TestVPCInterfaceFragmentConfiguresTheNICByMACWithoutARoute(t *testing.T) {
	frag := vultr.VPCInterfaceFragment()
	if frag.Name != vultr.VPCInterfaceFragmentName {
		t.Errorf("name %q", frag.Name)
	}
	for _, want := range []string{
		"http://169.254.169.254/v1.json",
		`."network-type" == "private"`,
		"[.mac, .ipv4.address, .ipv4.netmask]",
		"MACAddress=%s",
		"Address=%s/%s",
		"/etc/systemd/network/10-scarpenter-vpc-",
		"Before=kubelet.service",
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
