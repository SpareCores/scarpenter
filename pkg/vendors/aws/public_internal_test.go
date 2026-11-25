/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"testing"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestPublicIPv6IsStable(t *testing.T) {
	v6 := func(a string, primary bool) ec2types.InstanceIpv6Address {
		return ec2types.InstanceIpv6Address{Ipv6Address: new(a), IsPrimaryIpv6: new(primary)}
	}
	eni := func(index int32, addrs ...ec2types.InstanceIpv6Address) ec2types.InstanceNetworkInterface {
		return ec2types.InstanceNetworkInterface{
			Attachment: &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: new(index)}, Ipv6Addresses: addrs,
		}
	}
	for name, nis := range map[string][]ec2types.InstanceNetworkInterface{
		"primary of the first interface": {eni(1, v6("2001:db8::1", true)), eni(0, v6("2001:db8::a", false), v6("2001:db8::b", true))},
		"the same, other order":          {eni(0, v6("2001:db8::b", true), v6("2001:db8::a", false)), eni(1, v6("2001:db8::1", true))},
	} {
		inst := toInstance("us-east-1", ec2types.Instance{NetworkInterfaces: nis})
		if inst.PublicIPv6 != "2001:db8::b" {
			t.Errorf("%s: public IPv6 %q, want the primary address of device index 0", name, inst.PublicIPv6)
		}
	}
	// No attachment and no primary flag: the first address.
	inst := toInstance("us-east-1", ec2types.Instance{NetworkInterfaces: []ec2types.InstanceNetworkInterface{
		{Ipv6Addresses: []ec2types.InstanceIpv6Address{{Ipv6Address: new("2001:db8::7")}}},
	}})
	if inst.PublicIPv6 != "2001:db8::7" {
		t.Errorf("public IPv6 %q, want 2001:db8::7", inst.PublicIPv6)
	}
}
