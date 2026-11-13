/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"maps"
	"strings"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// hoursPerMonth turns a GB-month price into an hourly one (plan 18: /730).
const hoursPerMonth = 730

// PriceSource gives the price of a Cinder volume type in a region.
type PriceSource interface {
	// StoragePrice is USD per GB-month; ok is false for a type or region the
	// source does not know.
	StoragePrice(region, volumeType string) (usdPerGBMonth float64, ok bool)
}

// StaticPrices is a PriceSource of fixed prices, keyed "<REGION>/<volume type>"
// (region upper case), for the tests.
type StaticPrices map[string]float64

// StoragePrice implements PriceSource.
func (p StaticPrices) StoragePrice(region, volumeType string) (float64, bool) {
	v, ok := p[strings.ToUpper(region)+"/"+strings.ToLower(volumeType)]
	return v, ok
}

// keeperPrices holds the prices of the Keeper data version the catalog loaded
// last (SetKeeperPrices). Until the first call, and for anything the data
// lacks, the prices are unknown.
type keeperPrices struct {
	mu      sync.RWMutex
	storage map[string]float64
	loaded  bool
	hash    string
}

func newKeeperPrices() *keeperPrices { return &keeperPrices{} }

// StoragePrice implements PriceSource.
func (k *keeperPrices) StoragePrice(region, volumeType string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.storage[strings.ToUpper(region)+"/"+strings.ToLower(volumeType)]
	return v, ok
}

// set replaces the prices with the rows of a Keeper data version and reports
// whether they differ from the ones held (the first call always does).
func (k *keeperPrices) set(p vendors.KeeperPrices) bool {
	storage := map[string]float64{}
	for _, r := range vendors.GBMonth(p.Storage) {
		storage[strings.ToUpper(r.Region)+"/"+strings.ToLower(r.StorageID)] = r.USD
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	changed := !k.loaded || !maps.Equal(k.storage, storage)
	k.storage, k.loaded, k.hash = storage, true, p.Hash
	return changed
}
