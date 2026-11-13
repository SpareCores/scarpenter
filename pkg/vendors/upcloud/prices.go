/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"maps"
	"strings"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// PriceSource answers the prices of what an offering costs on top of the plan:
// disks that the plan does not include and the public IPv4 address of Cloud
// Native plans. The prices come from Keeper, in USD (the catalog asks Keeper for
// USD too, so no currency is converted here); a price the source does not know
// makes the offering's price incomplete instead of guessing.
type PriceSource interface {
	// StoragePrice is the price of a GB of the tier (maxiops, standard, hdd) per
	// month in USD in the zone.
	StoragePrice(region, tier string) (usdPerGBMonth float64, ok bool)
	// IPv4Price is the price of a public IPv4 address per hour in USD.
	IPv4Price(region string) (usdPerHour float64, ok bool)
}

// StaticPrices is a PriceSource with fixed answers.
type StaticPrices struct {
	// Storage is keyed by "<region>/<tier>", IPv4 by region.
	Storage map[string]float64
	IPv4    map[string]float64
}

// StoragePrice implements PriceSource.
func (p StaticPrices) StoragePrice(region, tier string) (float64, bool) {
	v, ok := p.Storage[region+"/"+tier]
	return v, ok
}

// IPv4Price implements PriceSource.
func (p StaticPrices) IPv4Price(region string) (float64, bool) {
	v, ok := p.IPv4[region]
	return v, ok
}

// keeperPrices holds the prices of the Keeper data version the catalog loaded
// last (SetKeeperPrices). Until the first call, and for anything the data
// lacks, the prices are unknown.
type keeperPrices struct {
	mu      sync.RWMutex
	storage map[string]float64
	ipv4    map[string]float64
	loaded  bool
	hash    string
}

func newKeeperPrices() *keeperPrices { return &keeperPrices{} }

// StoragePrice implements PriceSource.
func (k *keeperPrices) StoragePrice(region, tier string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.storage[region+"/"+tier]
	return v, ok
}

// IPv4Price implements PriceSource.
func (k *keeperPrices) IPv4Price(region string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.ipv4[region]
	return v, ok
}

// set replaces the prices with the rows of a Keeper data version and reports
// whether they differ from the ones held (the first call always does).
func (k *keeperPrices) set(p vendors.KeeperPrices) bool {
	storage := map[string]float64{}
	for _, r := range vendors.GBMonth(p.Storage) {
		storage[r.Region+"/"+strings.ToLower(r.StorageID)] = r.USD
	}
	ipv4 := vendors.HourlyIPv4(p.IPv4, hoursPerMonth)
	k.mu.Lock()
	defer k.mu.Unlock()
	changed := !k.loaded || !maps.Equal(k.storage, storage) || !maps.Equal(k.ipv4, ipv4)
	k.storage, k.ipv4, k.loaded, k.hash = storage, ipv4, true, p.Hash
	return changed
}
