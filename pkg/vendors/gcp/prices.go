/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

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

// DefaultIPv4PricePerHour is what Google charges for an external IPv4 address
// of an instance (USD per hour). UNVERIFIED: the pricing page was not read; it
// is not a Keeper price (Keeper has none for GCP). Override it with
// SCARPENTER_GCP_IPV4_PRICE.
const DefaultIPv4PricePerHour = 0.005

// hoursPerMonth converts a GB-month price to an hourly one (00a section 10).
const hoursPerMonth = 730

// PriceSource answers the prices of what an offering costs on top of the
// instance: disks and the external IPv4 address. The disk prices come from
// Keeper's storage prices; a price the source does not know makes the
// offering's price incomplete instead of guessing.
type PriceSource interface {
	// StoragePrice is the price of a GiB of the disk type per month in USD.
	StoragePrice(region, diskType string) (usdPerGiBMonth float64, ok bool)
	// IPv4Price is the price of an external IPv4 address per hour in USD.
	IPv4Price(region string) (usdPerHour float64, ok bool)
}

// StaticPrices is a PriceSource with fixed answers.
type StaticPrices struct {
	// Storage is keyed by "<region>/<disk type>", IPv4 by region.
	Storage map[string]float64
	IPv4    map[string]float64
}

// StoragePrice implements PriceSource.
func (p StaticPrices) StoragePrice(region, diskType string) (float64, bool) {
	v, ok := p.Storage[region+"/"+diskType]
	return v, ok
}

// IPv4Price implements PriceSource.
func (p StaticPrices) IPv4Price(region string) (float64, bool) {
	v, ok := p.IPv4[region]
	return v, ok
}

// keeperPrices reads the disk prices from Keeper's storage price endpoint in
// the background. Until the first successful load (and for anything the tables
// lack) the prices are unknown.
type keeperPrices struct {
	url    string
	ipv4   float64
	client *http.Client
	onLoad func()

	mu      sync.RWMutex
	storage map[string]float64
	loaded  bool
	loading bool
	last    time.Time
}

const priceRefresh = 6 * time.Hour

func newKeeperPrices(url string, ipv4 float64, onLoad func()) *keeperPrices {
	return &keeperPrices{url: strings.TrimRight(url, "/"), ipv4: ipv4, client: &http.Client{Timeout: 2 * time.Minute},
		onLoad: onLoad}
}

// StoragePrice implements PriceSource.
func (k *keeperPrices) StoragePrice(region, diskType string) (float64, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	v, ok := k.storage[region+"/"+diskType]
	return v, ok
}

// IPv4Price implements PriceSource: the driver's constant.
func (k *keeperPrices) IPv4Price(string) (float64, bool) { return k.ipv4, true }

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

type storagePriceRow struct {
	VendorID  string  `json:"vendor_id"`
	RegionID  string  `json:"region_id"`
	StorageID string  `json:"storage_id"`
	Unit      string  `json:"unit"`
	Price     float64 `json:"price"`
	Currency  string  `json:"currency"`
	Status    string  `json:"status"`
}

// storagePricesPath is Keeper's anonymous endpoint of the storage prices of GCP:
// one row per region and disk type (pd-balanced, hyperdisk-balanced, ...), unit
// GB/month, USD (plan 16, section 4).
const storagePricesPath = "/storage_prices?vendor=gcp&currency=USD&limit=-1"

// load reads the storage prices of GCP. Rows that are not USD per GB and month,
// or have no positive price, are ignored and leave that disk type's price
// unknown (the offering's price incomplete).
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
		if r.VendorID != Name || r.Currency != "USD" || r.Unit != "GB/month" || r.Price <= 0 || r.Status != "active" {
			continue
		}
		storage[r.RegionID+"/"+strings.ToLower(r.StorageID)] = r.Price
	}
	k.mu.Lock()
	k.storage, k.loaded = storage, true
	k.mu.Unlock()
	if k.onLoad != nil {
		k.onLoad()
	}
	return nil
}
