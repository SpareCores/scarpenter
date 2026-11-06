/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog_test

import (
	"runtime"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

var threeRegions = []string{"us-east-1", "eu-central-1", "eu-west-1"}

func scopes() map[string]catalog.Scope {
	return map[string]catalog.Scope{
		"aws-all-regions":   {"aws": nil},
		"aws-three-regions": {"aws": threeRegions},
	}
}

func countOfferings(c *catalog.Catalog, t testing.TB) (types, offerings int) {
	its := all(t, c, catalog.Filter{})
	for _, it := range its {
		offerings += len(it.Offerings)
	}
	return len(its), offerings
}

// BenchmarkBuildCatalog measures turning the loaded Keeper data of AWS into
// instance types and offerings (a new snapshot is built on every iteration:
// refresh indexing plus the mapping), for all regions and for three. Run with
//
//	go test ./pkg/catalog -run '^$' -bench BuildCatalog -benchmem
func BenchmarkBuildCatalog(b *testing.B) {
	for name, scope := range scopes() {
		b.Run(name, func(b *testing.B) {
			facts := catalogtest.Facts(b, "aws")
			data, err := catalogtest.Source(b).Load(b.Context(), catalog.Request{Scope: scope})
			if err != nil {
				b.Fatal(err)
			}
			cl := catalog.NewClass("c", scope)
			var types, offerings int
			b.ResetTimer()
			for b.Loop() {
				c := catalog.New(catalog.Options{Source: &fakeSource{data: data}, Facts: facts})
				c.SetClasses([]catalog.Class{cl})
				if err := c.Refresh(b.Context()); err != nil {
					b.Fatal(err)
				}
				types, offerings = countOfferings(c, b)
			}
			b.ReportMetric(float64(types), "types")
			b.ReportMetric(float64(offerings), "offerings")
		})
	}
}

// TestCatalogSize logs the size of the AWS catalog for all regions and for
// three: types, offerings and the memory the built catalog retains (the
// numbers of the plan 03 report). It asserts only that the scale is what the
// snapshot has.
func TestCatalogSize(t *testing.T) {
	for name, scope := range scopes() {
		t.Run(name, func(t *testing.T) {
			facts := catalogtest.Facts(t, "aws")
			data, err := catalogtest.Source(t).Load(t.Context(), catalog.Request{Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			c := catalog.New(catalog.Options{Source: &fakeSource{data: data}, Facts: facts})
			c.SetClasses([]catalog.Class{catalog.NewClass("c", scope)})
			if err := c.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			types, offerings := countOfferings(c, t)
			runtime.GC()
			runtime.ReadMemStats(&after)
			t.Logf("%s: %d types, %d offerings, %d MiB retained by the snapshot and the built catalog",
				name, types, offerings, (after.HeapAlloc-before.HeapAlloc)>>20)
			if types < 500 || offerings < 1000 {
				t.Errorf("implausibly small: %d types, %d offerings", types, offerings)
			}
			runtime.KeepAlive(c)
			runtime.KeepAlive(data)
		})
	}
}
