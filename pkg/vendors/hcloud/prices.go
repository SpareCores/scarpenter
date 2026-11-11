/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultIPv4PricePerHour is the price of a Primary IPv4 address in USD per
// hour: 0.50 EUR per month over 730 hours at about the current rate (plan 15).
// Keeper has no IPv4 price; override it with SCARPENTER_HCLOUD_IPV4_PRICE.
const DefaultIPv4PricePerHour = 0.0007

// IPv4PriceEnv overrides DefaultIPv4PricePerHour.
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
	// IPv4Price is the price of a Primary IPv4 address per hour in USD.
	IPv4Price() (usdPerHour float64, ok bool)
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
func (p StaticPrices) IPv4Price() (float64, bool) { return p.IPv4, p.IPv4 >= 0 }

// keeperPrices reads the volume prices from Keeper's storage price endpoint in
// the background. Until the first successful load the prices are unknown.
type keeperPrices struct {
	url    string
	ipv4   float64
	client *http.Client
	onLoad func()

	mu      sync.RWMutex
	volume  map[string]float64
	loaded  bool
	loading bool
	last    time.Time
}

// priceRefresh is how long loaded prices are trusted before a reload.
const priceRefresh = 6 * time.Hour

func newKeeperPrices(url string, ipv4 float64, onLoad func()) *keeperPrices {
	return &keeperPrices{url: strings.TrimRight(url, "/"), ipv4: ipv4, client: &http.Client{Timeout: 2 * time.Minute},
		onLoad: onLoad}
}

// VolumePrice implements PriceSource.
func (k *keeperPrices) VolumePrice(location string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.volume[location]
	return v, ok
}

// IPv4Price implements PriceSource: the driver's constant (Keeper has none).
func (k *keeperPrices) IPv4Price() (float64, bool) { return k.ipv4, true }

// loadedOnce reports whether the volume prices were loaded.
func (k *keeperPrices) loadedOnce() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.loaded
}

// kick starts a load in the background when none ran lately.
func (k *keeperPrices) kick(now time.Time, errs func(error)) {
	k.mu.Lock()
	interval := time.Minute
	if k.loaded {
		interval = priceRefresh
	}
	if k.loading || (!k.last.IsZero() && now.Sub(k.last) < interval) {
		k.mu.Unlock()
		return
	}
	k.loading, k.last = true, now
	k.mu.Unlock()
	go func() {
		err := k.load(context.Background())
		k.mu.Lock()
		k.loading = false
		k.mu.Unlock()
		if err != nil {
			errs(err)
		}
	}()
}

// storagePriceRow is the part of a row of Keeper's /storage_prices the driver reads.
type storagePriceRow struct {
	VendorID  string  `json:"vendor_id"`
	RegionID  string  `json:"region_id"`
	StorageID string  `json:"storage_id"`
	Unit      string  `json:"unit"`
	Price     float64 `json:"price"`
	Currency  string  `json:"currency"`
	Status    string  `json:"status"`
}

// storagePricesPath is Keeper's anonymous endpoint of the volume prices of
// Hetzner Cloud: one `block` row per location, unit GB/month, USD.
const storagePricesPath = "/storage_prices?vendor=hcloud&currency=USD&limit=-1"

// load reads the volume prices. Rows that are not USD per GB and month, or have
// no positive price, are ignored.
func (k *keeperPrices) load(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url+storagePricesPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "scarpenter")
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("reading Keeper's storage prices: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("reading Keeper's storage prices: HTTP %d", resp.StatusCode)
	}
	var rows []storagePriceRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return fmt.Errorf("decoding Keeper's storage prices: %w", err)
	}
	volume := map[string]float64{}
	for _, r := range rows {
		if r.VendorID != Name || r.Currency != "USD" || r.Unit != "GB/month" || r.Price <= 0 ||
			r.Status != "active" || !strings.EqualFold(r.StorageID, volumeStorageID) {
			continue
		}
		volume[r.RegionID] = r.Price
	}
	k.mu.Lock()
	k.volume, k.loaded = volume, true
	k.mu.Unlock()
	if k.onLoad != nil {
		k.onLoad()
	}
	return nil
}
