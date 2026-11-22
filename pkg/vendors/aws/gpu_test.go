/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

func TestVirtualGPUFamilies(t *testing.T) {
	var d vendors.GPUVirtualizer = &aws.Driver{}
	for ref, want := range map[string]bool{
		"g6f.large": true, "g6f.4xlarge": true, "gr6f.4xlarge": true, "g7f.xlarge": true,
		"g6.xlarge": false, "gr6.4xlarge": false, "g6e.xlarge": false, "g5g.xlarge": false,
		"g4dn.xlarge": false, "p5.4xlarge": false, "p3dn.24xlarge": false, "t3a.small": false, "": false,
	} {
		if got := d.Virtual(ref); got != want {
			t.Errorf("Virtual(%q) = %v, want %v", ref, got, want)
		}
	}
}
