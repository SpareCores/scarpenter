/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"maps"
	"strings"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// PriceSource answers the prices of what an offering costs on top of the
// instance: its disks (00a section 10). The prices come from Keeper's storage
// prices; a price the source does not know makes the offering's price
// incomplete instead of guessing. The public IPv4 address is free and traffic is
// not priced for any vendor, so there is no IP price.
type PriceSource interface {
	// StoragePrice is the price of a GiB of the disk category (and, for
	// cloud_essd, the performance level) per month in USD.
	StoragePrice(region, category, level string) (usdPerGiBMonth float64, ok bool)
}

// StaticPrices is a PriceSource with fixed answers, keyed by
// "<region>/<category>" or "<region>/<category>/<level>" (the level, when the
// disk has one, is tried first).
type StaticPrices map[string]float64

// StoragePrice implements PriceSource.
func (p StaticPrices) StoragePrice(region, category, level string) (float64, bool) {
	return lookupPrice(p, region, category, level)
}

// lookupPrice finds a price by region and category, with the performance level
// first: Keeper's row of the category is the price of the level it names, which
// Keeper does not say (unverified, plan 21 section 4), so a row without a level
// serves only the disks that have none and PL1, the API's default level.
func lookupPrice(m map[string]float64, region, category, level string) (float64, bool) {
	if level != "" {
		if v, ok := m[region+"/"+category+"/"+strings.ToLower(level)]; ok {
			return v, true
		}
		if !strings.EqualFold(level, "PL1") {
			return 0, false
		}
	}
	v, ok := m[region+"/"+category]
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
func (k *keeperPrices) StoragePrice(region, category, level string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return lookupPrice(k.storage, region, category, level)
}

// set replaces the prices with the rows of a Keeper data version and reports
// whether they differ from the ones held (the first call always does).
func (k *keeperPrices) set(p vendors.KeeperPrices) bool {
	storage := map[string]float64{}
	for _, r := range vendors.GBMonth(p.Storage) {
		id := strings.ToLower(r.StorageID)
		if cat, lvl, ok := strings.Cut(id, "_pl"); ok {
			storage[r.Region+"/"+cat+"/pl"+lvl] = r.USD
			continue
		}
		storage[r.Region+"/"+id] = r.USD
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	changed := !k.loaded || !maps.Equal(k.storage, storage)
	k.storage, k.loaded, k.hash = storage, true, p.Hash
	return changed
}
