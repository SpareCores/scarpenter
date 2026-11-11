/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

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

// keeperPrices reads the disk prices from Keeper's storage price endpoint in the
// background. Until the first successful load (and for anything the tables lack)
// the prices are unknown.
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

// priceRefresh is how long loaded prices are trusted before a reload.
const priceRefresh = 6 * time.Hour

func newKeeperPrices(url string, onLoad func()) *keeperPrices {
	return &keeperPrices{url: strings.TrimRight(url, "/"), client: &http.Client{Timeout: 2 * time.Minute}, onLoad: onLoad}
}

// StoragePrice implements PriceSource.
func (k *keeperPrices) StoragePrice(region, category, level string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return lookupPrice(k.storage, region, category, level)
}

// kick starts a load in the background when none ran lately: loaded prices are
// trusted for priceRefresh, a failed load is tried again after a minute.
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

// storagePriceRow is the part of a row of Keeper's /storage_prices that the
// driver reads.
type storagePriceRow struct {
	VendorID  string  `json:"vendor_id"`
	RegionID  string  `json:"region_id"`
	StorageID string  `json:"storage_id"`
	Unit      string  `json:"unit"`
	Price     float64 `json:"price"`
	Currency  string  `json:"currency"`
	Status    string  `json:"status"`
}

// storagePricesPath is Keeper's anonymous endpoint of the storage prices of
// Alibaba Cloud: one row per region and disk type, unit "GB/month", USD, region_id
// equal to the region ID. The storage IDs are Keeper's own and were not read from
// the live API (no cloud or Keeper call was made to write this): a row whose ID is
// the category (cloud_essd) or the category and level (cloud_essd_pl1) is used,
// anything else leaves the price unknown.
const storagePricesPath = "/storage_prices?vendor=alicloud&currency=USD&limit=-1"

// load reads the storage prices of Alibaba Cloud. Rows that are not USD per GB and
// month, or have no positive price, are ignored.
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
		id := strings.ToLower(r.StorageID)
		// cloud_essd_pl1 is the category and level of the price table.
		if cat, lvl, ok := strings.Cut(id, "_pl"); ok {
			storage[r.RegionID+"/"+cat+"/pl"+lvl] = r.Price
			continue
		}
		storage[r.RegionID+"/"+id] = r.Price
	}
	k.mu.Lock()
	k.storage, k.loaded = storage, true
	k.mu.Unlock()
	if k.onLoad != nil {
		k.onLoad()
	}
	return nil
}
