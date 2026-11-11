/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors_test

import (
	"slices"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

type hardener struct {
	vendors.Driver
	offered []string
}

func (h *hardener) Vendor() string       { return "h" }
func (h *hardener) Hardenings() []string { return h.offered }
func (h *hardener) BootFragments(req vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	out := make([]vendors.BootFragment, 0, 1+len(req.Hardening))
	out = append(out, vendors.BootFragment{Name: "always", Butane: "a"})
	for _, name := range req.Hardening {
		out = append(out, vendors.BootFragment{Name: name, Butane: name})
	}
	return out, nil
}

type plain struct{ vendors.Driver }

func TestBootFragmentsOfADriver(t *testing.T) {
	h := &hardener{offered: []string{"metadata-from-pods"}}
	// Only the hardenings the driver offers reach it; the always-on fragments come
	// whatever the NodeClass enabled.
	frags, err := vendors.BootFragmentsFor(h, []string{"metadata-from-pods", "other"}, vendors.BootFragmentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(frags))
	for _, f := range frags {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"always", "metadata-from-pods"}) {
		t.Errorf("fragments = %v", names)
	}
	offered, unoffered := vendors.OfferedHardenings(h, []string{"metadata-from-pods", "other"})
	if !slices.Equal(offered, []string{"metadata-from-pods"}) || !slices.Equal(unoffered, []string{"other"}) {
		t.Errorf("offered %v, unoffered %v", offered, unoffered)
	}
	// A driver that is no NodeHardener contributes nothing and offers nothing.
	if frags, err := vendors.BootFragmentsFor(plain{}, []string{"metadata-from-pods"}, vendors.BootFragmentRequest{}); err != nil || frags != nil {
		t.Errorf("a plain driver: %v, %v", frags, err)
	}
	if o, u := vendors.OfferedHardenings(plain{}, []string{"metadata-from-pods"}); o != nil || len(u) != 1 {
		t.Errorf("a plain driver offers %v, not %v", o, u)
	}
}
