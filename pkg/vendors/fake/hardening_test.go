//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake_test

import (
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func TestFakeOffersTheMetadataHardening(t *testing.T) {
	d := newHarness(t, map[string]string{"quota": baseQuota}).d
	if got := d.Hardenings(); len(got) != 1 || got[0] != vendors.HardeningMetadataFromPods {
		t.Fatalf("Hardenings = %v", got)
	}
	none, _ := d.BootFragments(vendors.BootFragmentRequest{})
	one, err := d.BootFragments(vendors.BootFragmentRequest{Hardening: []string{vendors.HardeningMetadataFromPods}})
	if err != nil || len(none) != 0 || len(one) != 1 || !strings.Contains(one[0].Butane, "169.254.169.254") {
		t.Errorf("fragments: %v / %v, %v", none, one, err)
	}
}
