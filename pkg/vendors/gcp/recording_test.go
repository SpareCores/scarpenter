/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"testing"

	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

// Keeper names GCP's regions by number and its disk types by number; the
// driver looks them up by the region name and the disk type name. The catalog
// hands over the names: this runs the recorded Keeper data through it.
func TestRecordedKeeperPricesAreFoundByName(t *testing.T) {
	data, err := catalogtest.Source(t).Load(t.Context(), catalog.Request{Scope: catalog.Scope{"gcp": {"europe-west3"}}})
	if err != nil {
		t.Fatal(err)
	}
	var prices vendors.KeeperPrices
	for _, r := range data.StoragePrices {
		prices.Storage = append(prices.Storage, vendors.StoragePrice{
			Region: r.RegionID, StorageID: r.StorageID, Name: r.StorageName, Unit: r.Unit, USD: r.USD})
	}
	for _, r := range data.IPv4Prices {
		prices.IPv4 = append(prices.IPv4, vendors.IPv4Price{Region: r.RegionID, Unit: r.Unit, USD: r.USD})
	}
	if len(prices.Storage) == 0 || len(prices.IPv4) == 0 {
		t.Fatalf("the recording has %d storage and %d IPv4 rows of gcp", len(prices.Storage), len(prices.IPv4))
	}
	d := gcp.New(gcp.WithAPI(fake.NewSeeded("p")), gcp.WithProject("p"))
	if err := d.Init(t.Context(), vendors.Config{Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	d.SetKeeperPrices(prices)
	off := vendors.Offering{Region: "europe-west3", APIReference: "e2-small"}
	if price, complete := d.PriceDelta(d.Defaults(), off); !complete || price <= 0 {
		t.Errorf("PriceDelta in europe-west3 = %v, complete %v; want the default disk and address priced", price, complete)
	}
}
