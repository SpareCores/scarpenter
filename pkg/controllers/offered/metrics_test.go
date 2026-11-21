/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package offered

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/SpareCores/scarpenter/pkg/catalog"
)

// The default set stays under 30 series per class and vendor, however many types
// there are; the per-offering gauge exists only with the flag.
func TestMetricSeries(t *testing.T) {
	r := fixture(286)
	// Every offering-level code on top of the gated ones.
	codes := []catalog.Code{catalog.ZoneNotAvailable, catalog.IncompletePrice, catalog.NotLaunchable, catalog.UnavailableCache,
		catalog.ImageGap, catalog.RegionNotReady, catalog.ZoneMissingResource}
	for i, code := range codes {
		r.Types[10+i].Offerings[0] = catalog.OfferingReport{Region: "r", CapacityType: "spot", Zone: "z", Reason: catalog.Reason{Code: code}}
	}
	for _, detail := range []bool{false, true} {
		m := newMetrics(detail)
		m.record("bench", r)
		reg := prometheus.NewPedanticRegistry()
		reg.MustRegister(m)
		byState, err := testutil.GatherAndCount(reg, "scarpenter_catalog_offerings_by_state")
		if err != nil {
			t.Fatal(err)
		}
		perOffering, err := testutil.GatherAndCount(reg, "scarpenter_instance_type_offering_available")
		if err != nil {
			t.Fatal(err)
		}
		if byState != 1+1+len(codes) || byState > 9 {
			t.Errorf("detail=%v: %d by-state series, want 9", detail, byState)
		}
		if (perOffering > 0) != detail {
			t.Errorf("detail=%v: %d per-offering series", detail, perOffering)
		}
		m.forget("bench")
		if n, _ := testutil.GatherAndCount(reg); n != 0 {
			t.Errorf("%d series after the class was forgotten", n)
		}
	}
}
