/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

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

// keeperPrices reads the storage and IPv4 prices from Keeper in the background.
// Until the first successful load (and for anything the tables lack) the prices
// are unknown.
type keeperPrices struct {
	url    string
	client *http.Client
	onLoad func()

	mu      sync.RWMutex
	storage map[string]float64
	ipv4    map[string]float64
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

// priceRow is the part of a row of Keeper's /storage_prices and /ipv4_prices that
// the driver reads (the rows also embed the region, vendor and storage objects).
type priceRow struct {
	VendorID  string  `json:"vendor_id"`
	RegionID  string  `json:"region_id"`
	StorageID string  `json:"storage_id"`
	Unit      string  `json:"unit"`
	Price     float64 `json:"price"`
	Currency  string  `json:"currency"`
	Status    string  `json:"status"`
}

// Keeper's anonymous price endpoints for UpCloud, USD (converted by Keeper), all
// rows. The paths and the row shape are those of the AWS storage prices; they
// are unverified for UpCloud: a row the driver cannot read leaves that price
// unknown and the offering's price incomplete.
const (
	storagePricesPath = "/storage_prices?vendor=upcloud&currency=USD&limit=-1"
	ipv4PricesPath    = "/ipv4_prices?vendor=upcloud&currency=USD&limit=-1"
)

func (k *keeperPrices) rows(ctx context.Context, path string) ([]priceRow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "scarpenter")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading Keeper's prices (%s): %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("reading Keeper's prices (%s): HTTP %d", path, resp.StatusCode)
	}
	var rows []priceRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decoding Keeper's prices (%s): %w", path, err)
	}
	return rows, nil
}

// load reads the prices. A storage price is USD per GB and month (the unit
// "GB/month"); an IPv4 price per month (divided by 672) or per hour. Rows of
// another vendor or currency, inactive ones and those without a positive price
// are ignored.
func (k *keeperPrices) load(ctx context.Context) error {
	srows, err := k.rows(ctx, storagePricesPath)
	if err != nil {
		return err
	}
	storage := map[string]float64{}
	for _, r := range srows {
		if r.VendorID != Vendor || r.Currency != "USD" || r.Price <= 0 || r.Status != "active" || r.Unit != "GB/month" {
			continue
		}
		storage[r.RegionID+"/"+strings.ToLower(r.StorageID)] = r.Price
	}
	ipv4 := map[string]float64{}
	if irows, err := k.rows(ctx, ipv4PricesPath); err == nil {
		for _, r := range irows {
			if r.VendorID != Vendor || r.Currency != "USD" || r.Price <= 0 || r.Status != "active" {
				continue
			}
			switch r.Unit {
			case "month":
				ipv4[r.RegionID] = r.Price / hoursPerMonth
			case "hour":
				ipv4[r.RegionID] = r.Price
			}
		}
	}
	k.mu.Lock()
	k.storage, k.ipv4, k.loaded = storage, ipv4, true
	k.mu.Unlock()
	if k.onLoad != nil {
		k.onLoad()
	}
	return nil
}
