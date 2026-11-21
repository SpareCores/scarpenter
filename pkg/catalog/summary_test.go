/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/catalog"
)

func TestSummariesCountAndSample(t *testing.T) {
	r := catalog.Report{}
	for i := range 8 {
		r.Types = append(r.Types, catalog.TypeReport{Vendor: "v", InstanceType: fmt.Sprintf("v.t%d", i), Offerings: []catalog.OfferingReport{
			{Zone: "a", Available: true},
			{Zone: "b", Reason: catalog.Reason{Code: catalog.QuotaGate, Detail: "bucket x"}},
			{Zone: "c", Removed: true, Reason: catalog.Reason{Code: catalog.ZoneNotAvailable, Detail: "c"}},
		}})
	}
	r.Types = append(r.Types,
		catalog.TypeReport{Vendor: "v", InstanceType: "v.free", Dropped: catalog.Reason{Code: catalog.NoPrice}},
		catalog.TypeReport{Vendor: "w", InstanceType: "w.one", Offerings: []catalog.OfferingReport{{Available: true}}})

	got := r.Summaries()
	v := got["v"]
	if v.Types != 8 || v.Offerings != 24 || v.AvailableOfferings != 8 {
		t.Errorf("counts: %+v", v)
	}
	want := []catalog.ReasonCount{
		{Code: catalog.QuotaGate, Count: 8, Types: []string{"v.t0", "v.t1", "v.t2", "v.t3", "v.t4"}},
		{Code: catalog.ZoneNotAvailable, Count: 8, Types: []string{"v.t0", "v.t1", "v.t2", "v.t3", "v.t4"}},
	}
	if !reflect.DeepEqual(v.Unavailable, want) {
		t.Errorf("unavailable: %+v", v.Unavailable)
	}
	if !reflect.DeepEqual(v.Dropped, []catalog.ReasonCount{{Code: catalog.NoPrice, Count: 1, Types: []string{"v.free"}}}) {
		t.Errorf("dropped: %+v", v.Dropped)
	}
	if w := got["w"]; w.Types != 1 || w.AvailableOfferings != 1 || len(w.Dropped) != 0 || len(w.Unavailable) != 0 {
		t.Errorf("w: %+v", w)
	}
}
