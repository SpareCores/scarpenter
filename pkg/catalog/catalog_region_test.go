package catalog_test

import (
	"testing"

	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// A retired region may share an alias with a current one: the active region
// answers, whatever the order of the data.
func TestRegionNamePrefersAnActiveRegion(t *testing.T) {
	for _, retiredFirst := range []bool{true, false} {
		c := newSynthetic(t, func(d *model.Data) {
			d.Regions[0].Aliases = []string{"Frankfurt"}
			retired := model.Region{Vendor: "v", ID: "r0", Name: "region-old", Aliases: []string{"Frankfurt"}}
			if retiredFirst {
				d.Regions = append([]model.Region{retired}, d.Regions...)
			} else {
				d.Regions = append(d.Regions, retired)
			}
		}, nil)
		for range 20 { // map order differs between iterations
			if name, found, loaded := c.RegionName("v", "Frankfurt"); name != "region-1" || !found || !loaded {
				t.Fatalf("RegionName = %q, %v, %v, want the active region-1", name, found, loaded)
			}
		}
		if name, found, _ := c.RegionName("v", "region-old"); name != "region-old" || !found {
			t.Errorf("a retired region is found by its own name: %q, %v", name, found)
		}
		if _, found, loaded := c.RegionName("v", "nowhere"); found || !loaded {
			t.Errorf("found = %v, loaded = %v for an unknown name", found, loaded)
		}
	}
}
