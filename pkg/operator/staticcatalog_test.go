/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator

import (
	"strings"
	"testing"

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
