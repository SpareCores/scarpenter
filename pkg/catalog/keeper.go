/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection"
	selapi "github.com/SpareCores/sc-keeper-go/selection/api"

	"github.com/SpareCores/scarpenter/pkg/catalog/model"
)

// Keeper's status and currency values.
const (
	statusActive   = "active"
	statusRetiring = "planned-for-retirement"
	currencyUSD    = "USD"
)

// KeeperOptions configures a KeeperSource.
type KeeperOptions struct {
	// URL is the base URL of the Keeper API (default: the public one).
	URL string
	// APIKey is an optional Keeper API key: it raises the rate limits, and
	// prices are then read from /table/server_prices (flat rows) instead of
	// the large anonymous /server_prices.
	APIKey string
	// UserAgent names the caller to Keeper's operators.
	UserAgent string
	// HTTPClient replaces the default.
	HTTPClient *http.Client
	// PricesRegionsPerRequest is the number of regions of one price request
	// (default: the client's, 20). Tests read one region per request, so that
	// every subset of regions has a recorded response.
	PricesRegionsPerRequest int
	// ClientOptions are passed to the Keeper client last (retries, sleep).
	ClientOptions []keeper.Option
}

// KeeperSource reads the catalog from the Keeper API through sc-keeper-go
// (overview section 5.1): a memoising fetcher per refresh, so all requests of
// one refresh carry the same data version.
type KeeperSource struct {
	opts   KeeperOptions
	client *keeper.Client
}

var _ Source = (*KeeperSource)(nil)

// NewKeeperSource returns a source reading from Keeper.
func NewKeeperSource(opts KeeperOptions) *KeeperSource {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	copts := []keeper.Option{keeper.WithHTTPClient(opts.HTTPClient)}
	if opts.UserAgent != "" {
		copts = append(copts, keeper.WithUserAgent(opts.UserAgent))
	}
	if opts.APIKey != "" {
		copts = append(copts, keeper.WithAPIKey(opts.APIKey), keeper.WithRegionsPerRequest(0))
	}
	if opts.PricesRegionsPerRequest > 0 {
		copts = append(copts, keeper.WithServerPricesRegionsPerRequest(opts.PricesRegionsPerRequest))
	}
	copts = append(copts, opts.ClientOptions...)
	return &KeeperSource{opts: opts, client: keeper.New(opts.URL, copts...)}
}

// Load reads the server, region and zone tables, the prices of the scope and
// the benchmark and selection queries. Any failure to read fails the load; a
// query that is invalid in itself is reported in Data.Invalid.
func (k *KeeperSource) Load(ctx context.Context, req Request) (*model.Data, error) {
	f := keeper.NewFetcher(k.client)
	health, err := f.Health(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading Keeper health: %w", err)
	}
	data := &model.Data{
		Version:   health.DatabaseHash,
		FetchedAt: time.Now(),
		Scores:    map[string]map[string]float64{},
		Selected:  map[string]map[string]model.Member{},
		Invalid:   map[string]string{},
	}

	if err := k.loadTables(ctx, f, req.Scope, data); err != nil {
		return nil, err
	}
	k.loadVendors(ctx, f, data)
	regionIDs := regionResolver(data.Regions)
	if err := k.loadPrices(ctx, f, req.Scope, regionIDs, data); err != nil {
		return nil, err
	}
	if err := k.loadMetrics(ctx, f, req.Metrics, data); err != nil {
		return nil, err
	}
	if err := k.loadSelections(ctx, f, req.Selections, regionIDs, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (k *KeeperSource) loadTables(ctx context.Context, f *keeper.Fetcher, scope Scope, data *model.Data) error {
	servers, err := f.TableServers(ctx)
	if err != nil {
		return fmt.Errorf("reading /table/server: %w", err)
	}
	for i := range servers {
		s := &servers[i]
		// Retiring types can still be launched: dropping them would make the
		// running nodes drift with instanceTypeNotFound (overview 5.1).
		if _, in := scope[s.VendorID]; !in || (s.Status != statusActive && s.Status != statusRetiring) {
			continue
		}
		if srv, ok := serverFromTable(s); ok {
			data.Servers = append(data.Servers, srv)
		}
	}
	regions, err := f.Regions(ctx)
	if err != nil {
		return fmt.Errorf("reading /table/region: %w", err)
	}
	for _, r := range regions {
		if _, in := scope[r.VendorID]; !in {
			continue
		}
		reg := model.Region{
			Vendor: r.VendorID, ID: r.RegionID, Name: r.APIReference, Aliases: r.Aliases,
			Active: r.Status == statusActive, CountryID: r.CountryID,
		}
		if r.City != nil {
			reg.City = *r.City
		}
		if r.State != nil {
			reg.State = *r.State
		}
		reg.GreenEnergy = r.GreenEnergy
		data.Regions = append(data.Regions, reg)
	}
	zones, err := f.Zones(ctx)
	if err != nil {
		return fmt.Errorf("reading /table/zone: %w", err)
	}
	for _, z := range zones {
		if _, in := scope[z.VendorID]; !in {
			continue
		}
		data.Zones = append(data.Zones, model.Zone{
			Vendor: z.VendorID, RegionID: z.RegionID, ID: z.ZoneID, Name: z.APIReference, Active: z.Status == statusActive,
		})
	}
	return nil
}

// loadVendors reads the vendor table, which holds the billing facts. It is one
// small call, and nothing but billing retention needs it: a failure is logged
// and leaves the table empty instead of failing the refresh.
func (k *KeeperSource) loadVendors(ctx context.Context, f *keeper.Fetcher, data *model.Data) {
	rows, err := f.Vendors(ctx)
	if err != nil {
		log.FromContext(ctx).Info("reading /table/vendor failed, billing retention has no Keeper data", "error", err.Error())
		return
	}
	for i := range rows {
		v := &rows[i]
		mv := model.Vendor{
			ID: v.VendorID, Active: v.Status == statusActive,
			BillingIncrementSeconds: v.BillingIncrementSeconds, MinimumBillingSeconds: v.MinimumBillingSeconds,
			StoppedServerCharged: v.StoppedServerCharged,
		}
		if v.BillingComment != nil {
			mv.BillingComment = *v.BillingComment
		}
		data.Vendors = append(data.Vendors, mv)
	}
}

func serverFromTable(s *keeper.TableServer) (model.Server, bool) {
	var arch string
	switch s.CPUArchitecture {
	case "x86_64":
		arch = model.ArchAMD64
	case "arm64":
		arch = model.ArchARM64
	default:
		// i386 and the *_mac architectures have no Kubernetes equivalent.
		return model.Server{}, false
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	srv := model.Server{
		Vendor: s.VendorID, ID: s.ServerID, APIReference: s.APIReference, Family: deref(s.Family),
		VCPUs: s.VCPUs, CPUCores: s.CPUCores, CPUAllocation: s.CPUAllocation, Arch: arch,
		CPUManufacturer: deref(s.CPUManufacturer), CPUFamily: deref(s.CPUFamily),
		CPUSpeedGHz: s.CPUSpeed, CPUFlags: s.CPUFlags,
		CPUL2PerCoreKiB: s.CPUL2Cache, CPUL3KiB: s.CPUL3CacheTotal,
		CPUL1dPerCoreKiB: s.CPUL1dCache, CPUL1iPerCoreKiB: s.CPUL1iCache,
		CPUL3CacheKiB: s.CPUL3Cache, CPUL3CacheTotalKiB: s.CPUL3CacheTotal,
		MemoryMiB:        s.MemoryAmount,
		AcceleratorCount: s.AcceleratorCount, AcceleratorType: strings.ToUpper(deref(s.AcceleratorType)),
		AcceleratorManufacturer: deref(s.AcceleratorManufacturer), AcceleratorModel: deref(s.AcceleratorModel),
		AcceleratorMemoryMiB: s.AcceleratorMemoryTotal, AcceleratorFamily: deref(s.AcceleratorFamily),
		AcceleratorMemoryMinMiB: s.AcceleratorMemoryMin,
		NetworkBaselineGbps:     s.NetworkSpeedBaseline, NetworkPeakGbps: s.NetworkSpeedMax,
		LocalStorageGB: s.StorageSize, LocalStorageCount: len(s.Storages),
	}
	if srv.CPUL3KiB == nil {
		srv.CPUL3KiB = s.CPUL3Cache
	}
	return srv, true
}

// regionResolver maps a vendor's region allow-list entries (the vendor's region
// name, Keeper's region id or an alias) to the ids of active Keeper regions
// (Keeper keeps retired regions, whose aliases may name a current one).
func regionResolver(regions []model.Region) func(vendor string, names []string) []string {
	return func(vendor string, names []string) []string {
		var ids []string
		for _, name := range names {
			for i := range regions {
				r := &regions[i]
				if r.Vendor == vendor && r.Active && matchesRegion(r, name) && !slices.Contains(ids, r.ID) {
					ids = append(ids, r.ID)
				}
			}
		}
		return ids
	}
}

func matchesRegion(r *model.Region, name string) bool {
	return r.ID == name || r.Name == name || slices.Contains(r.Aliases, name)
}

func (k *KeeperSource) loadPrices(ctx context.Context, f *keeper.Fetcher, scope Scope,
	resolve func(string, []string) []string, data *model.Data) error {
	logger := log.FromContext(ctx)
	for _, vendor := range slices.Sorted(maps.Keys(scope)) {
		var ids []string
		if names := scope[vendor]; len(names) > 0 {
			ids = resolve(vendor, names)
			for _, n := range names {
				if len(resolve(vendor, []string{n})) == 0 {
					logger.Info("region of the NodeClass scope is unknown to Keeper, ignored", "vendor", vendor, "region", n)
				}
			}
			if len(ids) == 0 {
				continue
			}
		} else {
			for i := range data.Regions {
				if r := &data.Regions[i]; r.Vendor == vendor && r.Active {
					ids = append(ids, r.ID)
				}
			}
		}
		slices.Sort(ids)
		if k.opts.APIKey != "" {
			rows, err := f.TableServerPrices(ctx, keeper.PriceQuery{Vendor: vendor, Regions: ids, Currency: currencyUSD})
			if err != nil {
				return fmt.Errorf("reading /table/server_prices of %s: %w", vendor, err)
			}
			for _, r := range rows {
				data.Prices = appendPrice(data.Prices, r.VendorID, r.RegionID, r.ZoneID, r.ServerID, r.OperatingSystem,
					r.Allocation, r.Unit, r.Currency, r.Status, r.Price)
			}
			continue
		}
		rows, err := f.ServerPrices(ctx, keeper.ServerPricesQuery{Vendor: vendor, Regions: ids, Currency: currencyUSD})
		if err != nil {
			return fmt.Errorf("reading /server_prices of %s: %w", vendor, err)
		}
		for _, r := range rows {
			data.Prices = appendPrice(data.Prices, r.VendorID, r.RegionID, r.ZoneID, r.ServerID, r.OperatingSystem,
				r.Allocation, r.Unit, r.Currency, r.Status, r.Price)
		}
	}
	return nil
}

// appendPrice keeps the prices selection counts (selection.AcceptPrice: active,
// hourly, USD, Linux or blank, above zero, on-demand or spot), so offerings
// and selection profiles see the same prices.
func appendPrice(prices []model.Price, vendor, region, zone, server, os string, alloc keeper.PriceAllocation,
	unit, currency, status string, price float64) []model.Price {
	a, ok := selection.AcceptPrice(status, unit, currency, os, alloc, price)
	if !ok {
		return prices
	}
	capacity := model.CapacityOnDemand
	if a == keeper.AllocationSpot {
		capacity = model.CapacitySpot
	}
	return append(prices, model.Price{
		Vendor: vendor, RegionID: region, ZoneID: zone, ServerID: server, CapacityType: capacity, USDPerHour: price,
	})
}

func (k *KeeperSource) loadMetrics(ctx context.Context, f *keeper.Fetcher, queries []MetricQuery,
	data *model.Data) error {
	if len(queries) == 0 {
		return nil
	}
	catalog, err := f.BenchmarkCatalog(ctx)
	if err != nil {
		return fmt.Errorf("reading /benchmark_configs: %w", err)
	}
	for _, q := range queries {
		key, err := resolveBenchmark(catalog, q)
		if err != nil {
			data.Invalid[q.ID] = err.Error()
			continue
		}
		byID, err := benchmarkScores(ctx, f, q.Vendor, key, keeper.AllocationOnDemand)
		if err != nil {
			return err
		}
		data.Scores[q.ID] = byAPIReference(data, q.Vendor, byID)
	}
	return nil
}

// benchmarkScores reads the scores of one benchmark of a vendor's types, by
// server id. Keeper lists a type only when it has a price of the allocation
// (in any region), so the allocation decides which types can have a score: the
// metrics read on-demand scores (a spot-only type gets none), a selection
// profile the scores of its price basis, as its /servers queries did. The
// query has no regions, so metrics and profiles of an allocation share one
// request per refresh.
func benchmarkScores(ctx context.Context, f *keeper.Fetcher, vendor string, key keeper.BenchmarkKey,
	alloc keeper.Allocation) (map[string]float64, error) {
	rows, err := f.Servers(ctx, keeper.Query{Vendor: vendor, Allocation: alloc, Benchmark: &key})
	if err != nil {
		return nil, fmt.Errorf("reading benchmark %s of %s: %w", key, vendor, err)
	}
	scores := make(map[string]float64, len(rows))
	for i := range rows {
		if rows[i].SelectedBenchmarkScore != nil {
			scores[rows[i].ServerID] = *rows[i].SelectedBenchmarkScore
		}
	}
	return scores, nil
}

// byAPIReference maps scores by server id to the api_reference of the vendor's
// server rows (Data.Scores' key). The two differ for GCP and Hetzner.
func byAPIReference(data *model.Data, vendor string, byID map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(byID))
	for i := range data.Servers {
		if s := &data.Servers[i]; s.Vendor == vendor {
			if score, ok := byID[s.ID]; ok {
				out[s.APIReference] = score
			}
		}
	}
	return out
}

func resolveBenchmark(catalog *keeper.BenchmarkCatalog, q MetricQuery) (keeper.BenchmarkKey, error) {
	if q.Benchmark != nil {
		var config []byte
		if q.Benchmark.Config != nil {
			config = q.Benchmark.Config.Raw
		}
		return catalog.Resolve(q.Benchmark.ID, config)
	}
	return catalog.Resolve(keeper.WorkloadProfileID(string(q.WorkloadProfile)), nil)
}

// loadSelections evaluates the selection profiles on the data of the refresh:
// the servers and prices already read, and the scores of the benchmarks the
// profile uses (the same source as the metrics'). It sends no /servers query
// for specifications or prices.
func (k *KeeperSource) loadSelections(ctx context.Context, f *keeper.Fetcher, queries []SelectionQuery,
	resolve func(string, []string) []string, data *model.Data) error {
	if len(queries) == 0 {
		return nil
	}
	catalog, err := f.BenchmarkCatalog(ctx)
	if err != nil {
		return fmt.Errorf("reading /benchmark_configs: %w", err)
	}
	opts := selection.Options{Catalog: catalog, DefaultBasis: selapi.PriceBasisOnDemand}
	rows := map[string][]keeper.Server{}
	for _, q := range queries {
		var ids []string
		if len(q.Regions) > 0 {
			if ids = resolve(q.Vendor, q.Regions); len(ids) == 0 {
				data.Invalid[q.ID] = "none of the regions is known to Keeper"
				continue
			}
		}
		spec := q.Spec
		keys, err := selection.BenchmarkKeys(&spec, opts)
		if err != nil {
			data.Invalid[q.ID] = err.Error()
			continue
		}
		basis := selection.AllocationOf(selection.ResolveBasis(&spec, opts.DefaultBasis))
		scores := make(map[keeper.BenchmarkKey]map[string]float64, len(keys))
		for _, key := range keys {
			if scores[key], err = benchmarkScores(ctx, f, q.Vendor, key, basis); err != nil {
				return fmt.Errorf("reading the scores of selection %s: %w", q.Vendor, err)
			}
		}
		cands := selectionCandidates(vendorRows(rows, data, q.Vendor), data, q.Vendor, ids, scores, basis)
		res, err := selection.Evaluate(&spec, cands, opts)
		if err != nil {
			data.Invalid[q.ID] = err.Error()
			continue
		}
		members := make(map[string]model.Member, len(res.InstanceTypes))
		for _, it := range res.InstanceTypes {
			members[it.Name] = model.Member{Score: it.Score}
		}
		data.Selected[q.ID] = members
	}
	return nil
}

// selectionCandidates are the candidates of a vendor in the regions (empty:
// all) that price on the basis, from the data of the refresh.
func selectionCandidates(rows []keeper.Server, data *model.Data, vendor string, regionIDs []string,
	scores map[keeper.BenchmarkKey]map[string]float64, basis keeper.Allocation) []*selection.Candidate {
	return selection.BuildCatalogCandidates(selection.CatalogInput{
		Servers: rows, Prices: vendorPrices(data, vendor), Regions: regionIDs, Scores: scores,
	}, basis)
}

// vendorRows returns the vendor's servers as selection's specification rows,
// built once per refresh.
func vendorRows(cache map[string][]keeper.Server, data *model.Data, vendor string) []keeper.Server {
	if rows, ok := cache[vendor]; ok {
		return rows
	}
	var rows []keeper.Server
	for i := range data.Servers {
		if s := &data.Servers[i]; s.Vendor == vendor {
			rows = append(rows, specRow(s))
		}
	}
	cache[vendor] = rows
	return rows
}

// specRow is the specification of a server as selection reads it. The catalog
// holds orderable servers only (no Status), and nullable text columns are nil
// when blank, as in Keeper.
func specRow(s *model.Server) keeper.Server {
	text := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	arch := "arm64"
	if s.Arch == model.ArchAMD64 {
		arch = "x86_64"
	}
	row := keeper.Server{
		ServerID: s.ID, VendorID: s.Vendor, APIReference: s.APIReference, Family: s.Family,
		VCPUs: s.VCPUs, CPUCores: s.CPUCores, CPUAllocation: s.CPUAllocation, CPUArchitecture: arch,
		CPUManufacturer: text(s.CPUManufacturer), CPUFamily: text(s.CPUFamily), CPUSpeed: s.CPUSpeedGHz, CPUFlags: s.CPUFlags,
		CPUL1dCache: s.CPUL1dPerCoreKiB, CPUL1iCache: s.CPUL1iPerCoreKiB, CPUL2Cache: s.CPUL2PerCoreKiB,
		CPUL3Cache: s.CPUL3CacheKiB, CPUL3CacheTotal: s.CPUL3CacheTotalKiB,
		MemoryAmount:     s.MemoryMiB,
		AcceleratorCount: s.AcceleratorCount, AcceleratorType: text(s.AcceleratorType),
		AcceleratorFamily:       text(s.AcceleratorFamily),
		AcceleratorManufacturer: text(s.AcceleratorManufacturer), AcceleratorModel: text(s.AcceleratorModel),
		AcceleratorMemoryMin: s.AcceleratorMemoryMinMiB, AcceleratorMemoryTotal: s.AcceleratorMemoryMiB,
		NetworkSpeedBaseline: s.NetworkBaselineGbps, NetworkSpeedMax: s.NetworkPeakGbps,
		StorageSize: s.LocalStorageGB,
	}
	// Selection counts the disks only.
	if s.LocalStorageCount > 0 {
		row.Storages = make([]keeper.Storage, s.LocalStorageCount)
	}
	return row
}

// vendorPrices yields the vendor's prices of the data. They were filtered by
// selection.AcceptPrice when they were read, so the neutral values stand for
// the columns the catalog does not keep.
func vendorPrices(data *model.Data, vendor string) iter.Seq[selection.CatalogPrice] {
	return func(yield func(selection.CatalogPrice) bool) {
		for i := range data.Prices {
			p := &data.Prices[i]
			if p.Vendor != vendor {
				continue
			}
			alloc := keeper.PriceAllocationOnDemand
			if p.CapacityType == model.CapacitySpot {
				alloc = keeper.PriceAllocationSpot
			}
			if !yield(selection.CatalogPrice{
				ServerID: p.ServerID, RegionID: p.RegionID, ZoneID: p.ZoneID, Allocation: alloc,
				Unit: "hour", Currency: currencyUSD, Status: statusActive, Price: p.USDPerHour,
			}) {
				return
			}
		}
	}
}
