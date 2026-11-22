/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure_test

import (
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
)

func TestVirtualGPUSeries(t *testing.T) {
	var d vendors.GPUVirtualizer = &azure.Driver{}
	for ref, want := range map[string]bool{
		"Standard_NV6s_v2": true, "Standard_NV12s_v3": true, "Standard_NV36ads_A10_v5": true,
		"Standard_NV72ads_A10_v5": true, "Standard_NV36adms_A10_v5": true, "Standard_NV4as_v4": true,
		"Standard_NC4as_T4_v3": false, "Standard_NC24ads_A100_v4": false, "Standard_NC40ads_H100_v5": false,
		"Standard_NC16ads_A10_v4": false, "Standard_ND96isr_H100_v5": false, "Standard_ND40rs_v2": false,
		"Standard_D2as_v5": false, "": false,
	} {
		if got := d.Virtual(ref); got != want {
			t.Errorf("Virtual(%q) = %v, want %v", ref, got, want)
		}
	}
}
