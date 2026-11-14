/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// catalogDriver is a driver with its own catalog; the registry only needs its
// vendor ID.
type catalogDriver struct {
	vendors.Driver
	vendor string
	yaml   string
}

func (d catalogDriver) Vendor() string      { return d.vendor }
func (d catalogDriver) CatalogYAML() []byte { return []byte(d.yaml) }

type plainDriver struct {
	vendors.Driver
	vendor string
}

func (d plainDriver) Vendor() string { return d.vendor }

const oneType = `
vendors:
  - name: dev
    regions: [{name: here, zones: [here-a]}]
    types:
      - {name: tiny, vcpus: 1, memoryMiB: 512, prices: [{region: here, onDemand: 0.01}]}
`

func TestStaticCatalogsCollectsOnlyTheDriversThatBringOne(t *testing.T) {
	reg := vendors.NewRegistryFrom(catalogDriver{vendor: "dev", yaml: oneType}, plainDriver{vendor: "aws"})
	got, err := staticCatalogs(reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["dev"] == nil || got["aws"] != nil {
		t.Errorf("static catalogs of %v, want only dev", got)
	}
}

func TestStaticCatalogsRejectsAnInvalidCatalog(t *testing.T) {
	reg := vendors.NewRegistryFrom(catalogDriver{vendor: "dev", yaml: "vendors: [{name: dev, types: [{name: broken}]}]"})
	if _, err := staticCatalogs(reg); err == nil || !strings.Contains(err.Error(), "dev") {
		t.Errorf("error = %v, want the driver to be named", err)
	}
}

func TestOwnCatalogsAddsTheEmbeddedVendorsOfTheOption(t *testing.T) {
	o := &Operator{Options: &options.Options{EmbeddedCatalogVendors: []string{"oci"}, Vendors: []string{"oci", "dev"}}}
	o.staticCatalogs = map[string]catalog.Source{"dev": nil}
	own, embedded, err := o.ownCatalogs(logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 2 || own["oci"] == nil || !slices.Equal(embedded, []string{"oci"}) {
		t.Errorf("own catalogs %v, embedded %v", own, embedded)
	}
	if len(o.staticCatalogs) != 1 {
		t.Errorf("the static catalogs were changed: %v", o.staticCatalogs)
	}

	// Without the vendor's driver the vendor is still served from the embedded
	// catalog, but not reported as such (no log line, no gauge).
	o = &Operator{Options: &options.Options{EmbeddedCatalogVendors: []string{"oci"}, Vendors: []string{"aws"}}}
	if own, embedded, err = o.ownCatalogs(logr.Discard()); err != nil || own["oci"] == nil || len(embedded) != 0 {
		t.Errorf("own catalogs %v, embedded %v (%v)", own, embedded, err)
	}

	// Removing the vendor from the option is the swap to Keeper: no own catalog.
	o = &Operator{Options: &options.Options{Vendors: []string{"oci"}}}
	if own, embedded, err = o.ownCatalogs(logr.Discard()); err != nil || len(own) != 0 || len(embedded) != 0 {
		t.Errorf("own catalogs %v, embedded %v (%v)", own, embedded, err)
	}
}

func TestOwnCatalogsRejectsUnknownAndClashingVendors(t *testing.T) {
	o := &Operator{Options: &options.Options{EmbeddedCatalogVendors: []string{"nosuch"}}}
	if _, _, err := o.ownCatalogs(logr.Discard()); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("unknown vendor: error = %v", err)
	}
	o = &Operator{Options: &options.Options{EmbeddedCatalogVendors: []string{"oci"}}}
	o.staticCatalogs = map[string]catalog.Source{"oci": nil}
	if _, _, err := o.ownCatalogs(logr.Discard()); err == nil || !strings.Contains(err.Error(), "its own catalog") {
		t.Errorf("clash: error = %v", err)
	}
}
