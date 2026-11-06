/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
)

func availability(t *testing.T, c *catalog.Catalog, typeName string) map[string]bool {
	t.Helper()
	it := byName(all(t, c, catalog.Filter{}), typeName)
	if it == nil {
		t.Fatalf("no type %s", typeName)
	}
	out := map[string]bool{}
	for _, o := range it.Offerings {
		out[o.Zone()+"/"+o.CapacityType()] = o.Available
	}
	return out
}

func TestUnavailableOfferingsAreMarkedAndRecover(t *testing.T) {
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	un := unavailable.New(clk, unavailable.TTLs{})
	src := &fakeSource{data: synthetic()}
	c := catalog.New(catalog.Options{Source: src, Facts: syntheticFacts(), Clock: clk, Unavailable: un})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for k, avail := range availability(t, c, "v.small") {
		if !avail {
			t.Fatalf("%s is unavailable at the start", k)
		}
	}

	un.MarkInsufficientCapacity("v", "v.small", "zone-1", "on-demand")
	got := availability(t, c, "v.small")
	if got["zone-1/on-demand"] || !got["zone-2/on-demand"] || !got["zone-1/spot"] {
		t.Errorf("after an insufficient capacity error: %v", got)
	}
	// Other types are untouched.
	for k, avail := range availability(t, c, "v.big") {
		if !avail {
			t.Errorf("v.big %s is unavailable", k)
		}
	}

	un.MarkQuotaExceeded("v", "region-1", "spot")
	got = availability(t, c, "v.small")
	if got["zone-1/spot"] {
		t.Errorf("spot in the region after a quota error: %v", got)
	}

	// The cache expires entries by itself and the next query sees it.
	clk.Step(4 * time.Minute)
	got = availability(t, c, "v.small")
	if !got["zone-1/on-demand"] {
		t.Errorf("insufficient capacity entry did not expire: %v", got)
	}
	if got["zone-1/spot"] {
		t.Errorf("the quota entry (30 minutes) expired early: %v", got)
	}
	clk.Step(30 * time.Minute)
	for k, avail := range availability(t, c, "v.small") {
		if !avail {
			t.Errorf("%s is still unavailable", k)
		}
	}
}

func TestResultsAreCachedByTheUnavailableSequence(t *testing.T) {
	un := unavailable.New(nil, unavailable.TTLs{})
	c := catalog.New(catalog.Options{Source: &fakeSource{data: synthetic()}, Facts: syntheticFacts(), Unavailable: un})
	c.SetClasses([]catalog.Class{catalog.NewClass("c", catalog.Scope{"v": nil})})
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	a, b := all(t, c, catalog.Filter{}), all(t, c, catalog.Filter{})
	if len(a) == 0 || a[0] != b[0] {
		t.Error("an identical query did not hit the cache")
	}
	un.MarkZoneUnusable("v", "zone-1")
	d := all(t, c, catalog.Filter{})
	if d[0] == a[0] {
		t.Error("the cache ignored the change of the unavailable cache")
	}
	// The cached type of the old generation was not modified.
	for _, o := range a[0].Offerings {
		if !o.Available {
			t.Error("a type handed out earlier was modified")
		}
	}
	// A new snapshot is a new generation.
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e := all(t, c, catalog.Filter{}); e[0] == d[0] {
		t.Error("the cache survived a refresh")
	}
}
