/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"maps"
	"strings"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// IPv4PriceEnv overrides Keeper's price of a Primary IPv4 address (USD per
// hour).
const IPv4PriceEnv = "SCARPENTER_HCLOUD_IPV4_PRICE"

// hoursPerMonth converts a GB-month price to an hourly one (00a section 10).
const hoursPerMonth = 730

// volumeStorageID is the storage row of a Hetzner volume in Keeper.
const volumeStorageID = "block"

// PriceSource answers the prices of what an offering costs on top of the
// server: volumes and the Primary IPv4 address. A price the source does not
// know makes the offering's price incomplete instead of guessing.
type PriceSource interface {
	// VolumePrice is the price of a GB of volume per month in USD at the location.
	VolumePrice(location string) (usdPerGBMonth float64, ok bool)
	// IPv4Price is the price of a Primary IPv4 address per hour in USD at the
	// location.
	IPv4Price(location string) (usdPerHour float64, ok bool)
}

// StaticPrices is a PriceSource with fixed answers.
type StaticPrices struct {
	// Volume is keyed by location.
	Volume map[string]float64
	// IPv4 is the price per hour (negative: unknown).
	IPv4 float64
}

// VolumePrice implements PriceSource.
func (p StaticPrices) VolumePrice(location string) (float64, bool) {
	v, ok := p.Volume[location]
	return v, ok
}

// IPv4Price implements PriceSource.
func (p StaticPrices) IPv4Price(string) (float64, bool) { return p.IPv4, p.IPv4 >= 0 }

// keeperPrices holds the prices of the Keeper data version the catalog loaded
// last (SetKeeperPrices). Until the first call, and for anything the data
// lacks, the prices are unknown.
type keeperPrices struct {
	// ipv4Override is the operator's price of a Primary IPv4 address (USD per
	// hour); it wins over Keeper's rows.
	ipv4Override *float64

	mu     sync.RWMutex
	volume map[string]float64
	ipv4   map[string]float64
	loaded bool
	hash   string
}

func newKeeperPrices(ipv4Override *float64) *keeperPrices {
	return &keeperPrices{ipv4Override: ipv4Override}
}

// VolumePrice implements PriceSource.
func (k *keeperPrices) VolumePrice(location string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.volume[location]
	return v, ok
}

// IPv4Price implements PriceSource: the operator's override when set, else
// Keeper's row of the location.
func (k *keeperPrices) IPv4Price(location string) (float64, bool) {
	if k.ipv4Override != nil {
		return *k.ipv4Override, true
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.ipv4[location]
	return v, ok
}

// loadedOnce reports whether Keeper's prices were set.
func (k *keeperPrices) loadedOnce() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.loaded
}

// set replaces the prices with the rows of a Keeper data version and reports
// whether they differ from the ones held (the first call always does).
func (k *keeperPrices) set(p vendors.KeeperPrices) bool {
	volume := map[string]float64{}
	for _, r := range vendors.GBMonth(p.Storage) {
		if strings.EqualFold(r.StorageID, volumeStorageID) {
			volume[r.Region] = r.USD
		}
	}
	ipv4 := vendors.HourlyIPv4(p.IPv4, hoursPerMonth)
	k.mu.Lock()
	defer k.mu.Unlock()
	changed := !k.loaded || !maps.Equal(k.volume, volume) || !maps.Equal(k.ipv4, ipv4)
	k.volume, k.ipv4, k.loaded, k.hash = volume, ipv4, true, p.Hash
	return changed
}
