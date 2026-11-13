/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"maps"
	"strings"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// hoursPerMonth converts a GB-month price to an hourly one (00a section 10).
const hoursPerMonth = 730

// PriceSource answers the prices of what an offering costs on top of the VM: the
// managed disks and the public IPv4 address (00a section 10). A price the source
// does not know makes the offering's price incomplete instead of guessing.
type PriceSource interface {
	// StoragePrice is the price of a GiB of the disk SKU (Premium_LRS ...) per
	// month in USD.
	StoragePrice(region, storageAccountType string) (usdPerGiBMonth float64, ok bool)
	// IPv4Price is the price of a public IPv4 address per hour in USD.
	IPv4Price(region string) (usdPerHour float64, ok bool)
}

// StaticPrices is a PriceSource with fixed answers: Storage is keyed by
// "<region>/<lower-case storageAccountType>", IPv4 by region.
type StaticPrices struct {
	Storage map[string]float64
	IPv4    map[string]float64
}

// StoragePrice implements PriceSource.
func (p StaticPrices) StoragePrice(region, t string) (float64, bool) {
	v, ok := p.Storage[region+"/"+strings.ToLower(t)]
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
	// ipv4Override is the operator's price of a public IPv4 address (USD per
	// hour); it wins over Keeper's rows.
	ipv4Override *float64

	mu      sync.RWMutex
	storage map[string]float64
	ipv4    map[string]float64
	loaded  bool
	hash    string
}

func newKeeperPrices(ipv4Override *float64) *keeperPrices {
	return &keeperPrices{ipv4Override: ipv4Override}
}

// StoragePrice implements PriceSource.
func (k *keeperPrices) StoragePrice(region, t string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.storage[region+"/"+strings.ToLower(t)]
	return v, ok
}

// IPv4Price implements PriceSource: the operator's override when set, else
// Keeper's row of the region.
func (k *keeperPrices) IPv4Price(region string) (float64, bool) {
	if k.ipv4Override != nil {
		return *k.ipv4Override, true
	}
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
