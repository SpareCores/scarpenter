/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

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

// keeperPrices reads the volume prices from Keeper's /storage_prices, in the
// background, and refreshes them every few hours.
type keeperPrices struct {
	url    string
	client *http.Client
	onLoad func()

	mu      sync.RWMutex
	storage map[string]float64
	loaded  bool
	loading bool
	last    time.Time
}

const priceRefresh = 6 * time.Hour

func newKeeperPrices(url string, onLoad func()) *keeperPrices {
	return &keeperPrices{url: strings.TrimRight(url, "/"), client: &http.Client{Timeout: 2 * time.Minute}, onLoad: onLoad}
}

// StoragePrice implements PriceSource.
func (k *keeperPrices) StoragePrice(region, volumeType string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.storage[strings.ToUpper(region)+"/"+strings.ToLower(volumeType)]
	return v, ok
}

// kick starts a load unless one is running or one ran recently.
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

type storagePriceRow struct {
	VendorID  string  `json:"vendor_id"`
	RegionID  string  `json:"region_id"`
	StorageID string  `json:"storage_id"`
	Unit      string  `json:"unit"`
	Price     float64 `json:"price"`
	Currency  string  `json:"currency"`
	Status    string  `json:"status"`
}

const storagePricesPath = "/storage_prices?vendor=ovh&currency=USD&limit=-1"

// load reads the prices now.
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
	storage := map[string]float64{}
	for _, r := range rows {
		if r.VendorID != Vendor || r.Currency != "USD" || r.Unit != "GB/month" || r.Price <= 0 || r.Status != "active" {
			continue
		}
		storage[strings.ToUpper(r.RegionID)+"/"+strings.ToLower(r.StorageID)] = r.Price
	}
	k.mu.Lock()
	k.storage, k.loaded = storage, true
	k.mu.Unlock()
	if k.onLoad != nil {
		k.onLoad()
	}
	return nil
}
