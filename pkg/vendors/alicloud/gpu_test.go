/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestVirtualGPUFamilies(t *testing.T) {
	var d vendors.GPUVirtualizer = &Driver{}
	for ref, want := range map[string]bool{
		"ecs.vgn6i-m4-vws.xlarge": true, "ecs.vgn6i-m16-vws.5xlarge": true,
		"ecs.sgn7i-vws-m2.xlarge": true, "ecs.sgn8ia-m2.xlarge": true,
		"ecs.gn7i-c8g1-vws.xlarge": true,
		"ecs.gn6i-c4g1.xlarge":     false, "ecs.gn7i-2x.8xlarge": false, "ecs.gn8is.2xlarge": false,
		"ecs.gn6v-c8g1.2xlarge": false, "ecs.g8y.large": false, "ecs.u1-c1m2.large": false, "": false, "gn6i": false,
	} {
		if got := d.Virtual(ref); got != want {
			t.Errorf("Virtual(%q) = %v, want %v", ref, got, want)
		}
	}
}
