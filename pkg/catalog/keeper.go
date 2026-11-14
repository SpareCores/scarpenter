/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalog

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
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

// The per-vendor tables a vendor needs to have data (storage_price and
// ipv4_price only serve the drivers, and may be missing).
var requiredTables = []string{"region", "zone", "server", "server_price"}

// KeeperOptions configures a KeeperSource.
type KeeperOptions struct {
	// URL is the base URL of the Keeper API.
	URL string
	// APIKeyFile is the optional file with the Keeper API key. It is read at
	// every refresh, so a rotated key takes effect without a restart; an empty
	// file is an error. A Keeper that needs no key (a self-hosted one) is read
	// without it.
	APIKeyFile string
	// UserAgent names the caller to Keeper's operators.
	UserAgent string
	// HTTPClient replaces the default.
	HTTPClient *http.Client
	// CacheDir keeps the snapshot files of the data version on disk (empty: no
	// copy, every new version is fetched in full).
	CacheDir string
	// ClientOptions are passed to the Keeper client last (retries, sleep).
	ClientOptions []keeper.Option
}

// KeeperAuthError is the failure of a refresh that Keeper answered with 401.
// It is never retried: Keeper charges a rejected request 11 credits. The
// catalog reports it as the condition KeeperAuthorized=False.
type KeeperAuthError struct {
	// URL is the Keeper base URL.
	URL string
	// KeyConfigured says whether a key was sent: false means none is
	// configured, true that Keeper rejected the one sent.
	KeyConfigured bool
	Err           error
}

func (e *KeeperAuthError) Error() string {
	if e.KeyConfigured {
		return "Keeper rejected the API key: " + e.Err.Error()
	}
	return "Keeper needs an API key: " + e.Err.Error()
}

// Unwrap returns Keeper's error (a *keeper.UnauthorizedError).
func (e *KeeperAuthError) Unwrap() error { return e.Err }

// KeeperSource reads the catalog from Keeper's snapshot of one pinned data
// version (plan 13): /healthcheck pins the hash, the files of the hash are
// read from the disk copy or fetched, and only /benchmark_configs is read
// live besides.
type KeeperSource struct {
	opts  KeeperOptions
	cache diskCache

	mu   sync.Mutex
	last *loaded
}

// loaded is the last successful load, for the early exit of an unchanged
// version.
type loaded struct {
	hash       string
	requestKey string
	data       *model.Data
}

var _ Source = (*KeeperSource)(nil)

// NewKeeperSource returns a source reading from Keeper.
func NewKeeperSource(opts KeeperOptions) *KeeperSource {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	return &KeeperSource{opts: opts, cache: diskCache{dir: opts.CacheDir}}
}

// client builds the Keeper client of a refresh, with the key as the file has
// it now.
func (k *KeeperSource) client() (*keeper.Client, bool, error) {
	copts := []keeper.Option{keeper.WithHTTPClient(k.opts.HTTPClient)}
	if k.opts.UserAgent != "" {
		copts = append(copts, keeper.WithUserAgent(k.opts.UserAgent))
	}
	key := ""
	if k.opts.APIKeyFile != "" {
		raw, err := os.ReadFile(k.opts.APIKeyFile)
		if err != nil {
			return nil, false, fmt.Errorf("reading the Keeper API key file: %w", err)
		}
		if key = strings.TrimSpace(string(raw)); key == "" {
			return nil, false, fmt.Errorf("the Keeper API key file %s is empty", k.opts.APIKeyFile)
		}
		copts = append(copts, keeper.WithAPIKey(key))
	}
	copts = append(copts, k.opts.ClientOptions...)
	return keeper.New(k.opts.URL, copts...), key != "", nil
}

// Load reads one consistent data version for the request: the version
// /healthcheck pins. When Keeper still serves the version of the last load
// for the same request, it returns that load's data (the same pointer) after
// the one /healthcheck request. A query that is invalid in itself is reported
// in Data.Invalid; any failure to read fails the load.
func (k *KeeperSource) Load(ctx context.Context, req Request) (*model.Data, error) {
	client, keyConfigured, err := k.client()
	if err != nil {
		return nil, err
	}
	data, err := k.load(ctx, client, req)
	if ua, ok := errors.AsType[*keeper.UnauthorizedError](err); ok {
		return nil, &KeeperAuthError{URL: k.opts.URL, KeyConfigured: keyConfigured, Err: ua}
	}
	return data, err
}

func (k *KeeperSource) load(ctx context.Context, client *keeper.Client, req Request) (*model.Data, error) {
	health, err := client.Health(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading Keeper health: %w", err)
	}
	hash, published := health.DatabaseHash, health.DatabasePublishedAt
	if hash == "" {
		return nil, errors.New("Keeper serves no data version") //nolint:staticcheck // Keeper is a name
	}
	if !validHash(hash) {
		return nil, fmt.Errorf("the data version %q of Keeper's health check is not a hash", hash)
	}
	reqKey := requestKey(req)
	k.mu.Lock()
	last := k.last
	k.mu.Unlock()
	if last != nil && last.hash == hash && last.requestKey == reqKey {
		return last.data, nil
	}
	for attempt := 0; ; attempt++ {
		r := &refresh{
			src: k, client: client, hash: hash, published: published, logger: log.FromContext(ctx),
			scoreRows: map[string][]keeper.BenchmarkScore{},
		}
		data, err := r.run(ctx, req)
		if err == nil {
			k.mu.Lock()
			k.last = &loaded{hash: hash, requestKey: reqKey, data: data}
			k.mu.Unlock()
			return data, nil
		}
		// The replica no longer serves the hash: pin to the one it serves now,
		// once. A second failure waits for the next refresh.
		pe, ok := errors.AsType[*keeper.ProblemError](err)
		if !ok || !keeper.IsSnapshotNotAvailable(err) || attempt > 0 || pe.Current == "" || pe.Current == hash {
			return nil, err
		}
		if !validHash(pe.Current) {
			return nil, fmt.Errorf("the current data version %q Keeper names is not a hash: %w", pe.Current, err)
		}
		r.logger.Info("Keeper no longer serves the data version, pinning to its current one",
			"requested", hash, "current", pe.Current)
		hash, published = pe.Current, time.Time{}
	}
}

// hashPattern is what Keeper's snapshot routes accept as a hash. It also keeps
// a hash from ever being more than a file name.
var hashPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func validHash(h string) bool { return hashPattern.MatchString(h) }

// refresh is one pass over the files of one pinned hash.
type refresh struct {
	src    *KeeperSource
	client *keeper.Client
	hash   string
	// published is when the hash was published, from /healthcheck or the
	// manifest response; zero when unknown.
	published time.Time
	logger    logr.Logger

	manifest   keeper.Manifest
	manifestGz *keeper.SnapshotBody
	// fromDisk says that the manifest came from the disk copy.
	fromDisk bool

	cacheErrLogged bool
	prevHash       string
	prevManifest   *keeper.Manifest
	prevTried      bool

	benchmarks *keeper.BenchmarkCatalog
	scoreRows  map[string][]keeper.BenchmarkScore
}

// run reads everything the request needs and builds the data.
func (r *refresh) run(ctx context.Context, req Request) (*model.Data, error) {
	if err := r.loadManifest(ctx); err != nil {
		return nil, err
	}
	data := &model.Data{
		Version:      r.hash,
		FetchedAt:    time.Now(),
		PublishedAt:  r.published,
		SnapshotRead: true,
		Scores:       map[string]map[string]float64{},
		Selected:     map[string]map[string]model.Member{},
		Invalid:      map[string]string{},
	}
	if err := r.loadVendorTable(ctx, data); err != nil {
		return nil, err
	}
	missing := map[string]bool{}
	for _, vendor := range slices.Sorted(maps.Keys(req.Scope)) {
		if err := r.loadVendor(ctx, vendor, req.Scope[vendor], data); err != nil {
			if nd, ok := errors.AsType[*noDataError](err); ok {
				r.logger.Info("Keeper has no data for the vendor, it is skipped", "vendor", vendor, "reason", nd.Error())
				missing[vendor] = true
				continue
			}
			return nil, err
		}
	}
	local := newLocalData(data)
	if err := r.loadMetrics(ctx, req.Metrics, data, missing); err != nil {
		return nil, err
	}
	if err := r.loadSelections(ctx, req.Selections, data, local, missing); err != nil {
		return nil, err
	}
	// The manifest goes to disk last: a directory with one holds a complete
	// load of the hash.
	if r.src.cache.enabled() && !r.fromDisk {
		r.cacheError(r.writeManifest())
	}
	r.cacheError(r.src.cache.prune(r.hash))
	return data, nil
}

// cacheError logs a failure of the disk copy once per refresh (a directory that
// cannot be written fails every file); the refresh goes on without it.
func (r *refresh) cacheError(err error) {
	if err != nil && !r.cacheErrLogged {
		r.cacheErrLogged = true
		r.logger.Error(err, "the Keeper disk copy is not usable, reading without it")
	}
}

// writeManifest keeps the manifest and when its hash was published.
func (r *refresh) writeManifest() error {
	if !r.published.IsZero() {
		if err := r.src.cache.writeMeta(r.hash, publishedFile, []byte(r.published.UTC().Format(time.RFC3339))); err != nil {
			return err
		}
	}
	return r.src.cache.write(r.hash, "", r.manifestGz.Gzip)
}

// noDataError says that the manifest lists none of a vendor's files.
type noDataError struct{ vendor, table string }

func (e *noDataError) Error() string { return fmt.Sprintf("the manifest has no %s file", e.table) }

// loadManifest reads the manifest of the hash from the disk copy, else from
// Keeper.
func (r *refresh) loadManifest(ctx context.Context) error {
	if gz, ok := r.src.cache.read(r.hash, ""); ok {
		body := &keeper.SnapshotBody{Gzip: gz}
		if m, err := keeper.DecodeManifest(body); err == nil && len(m.Files) > 0 {
			r.manifest, r.manifestGz, r.fromDisk = m, body, true
			if r.published.IsZero() {
				if raw, ok := r.src.cache.readMeta(r.hash, publishedFile); ok {
					if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw))); err == nil {
						r.published = t.UTC()
					}
				}
			}
			return nil
		}
		r.src.cache.remove(r.hash, "")
	}
	body, err := r.client.SnapshotManifestBody(ctx, r.hash)
	if err != nil {
		return fmt.Errorf("reading the Keeper manifest of %s: %w", r.hash, err)
	}
	m, err := keeper.DecodeManifest(body)
	if err != nil {
		return fmt.Errorf("reading the Keeper manifest of %s: %w", r.hash, err)
	}
	if body.Version.Hash != "" && body.Version.Hash != r.hash {
		return fmt.Errorf("the Keeper snapshot is of data version %s, not the pinned %s", body.Version.Hash, r.hash)
	}
	r.manifest, r.manifestGz = m, body
	if !body.Version.PublishedAt.IsZero() {
		r.published = body.Version.PublishedAt
	}
	return nil
}

// previousManifest is the manifest of the previous hash's directory, whose
// files that did not change are copied (loaded once, nil without one).
func (r *refresh) previousManifest() *keeper.Manifest {
	if r.prevTried {
		return r.prevManifest
	}
	r.prevTried = true
	r.prevHash = r.src.cache.previous(r.hash)
	if r.prevHash == "" {
		return nil
	}
	gz, ok := r.src.cache.read(r.prevHash, "")
	if !ok {
		return nil
	}
	if m, err := keeper.DecodeManifest(&keeper.SnapshotBody{Gzip: gz}); err == nil {
		r.prevManifest = &m
	}
	return r.prevManifest
}

// file returns the file of the manifest's path: from the disk copy when its
// bytes match the manifest, copied from the previous hash's directory when
// the file did not change, else fetched. A file the manifest does not list is
// fetched and neither verified nor kept (a score file of a benchmark Keeper
// has no scores of is an empty file, not a 404).
func (r *refresh) file(ctx context.Context, path string) (*keeper.SnapshotBody, int, error) {
	mf, listed := r.manifest.Lookup(path)
	rows := -1
	if listed {
		rows = mf.Rows
		if gz, ok := r.src.cache.read(r.hash, path); ok {
			body := &keeper.SnapshotBody{Gzip: gz}
			if body.Verify(mf.SHA256) == nil {
				return body, rows, nil
			}
			r.logger.Info("a file of the Keeper disk copy does not match the manifest, fetching it again", "path", path)
			r.src.cache.remove(r.hash, path)
		}
		if body := r.copyFromPrevious(path, mf); body != nil {
			return body, rows, nil
		}
	}
	body, err := r.client.SnapshotFile(ctx, r.hash, path)
	if err != nil {
		return nil, 0, fmt.Errorf("reading the Keeper snapshot file %s: %w", path, err)
	}
	if body.Version.Hash != "" && body.Version.Hash != r.hash {
		return nil, 0, fmt.Errorf("the Keeper file %s is of data version %s, not the pinned %s",
			path, body.Version.Hash, r.hash)
	}
	if !listed {
		return body, rows, nil
	}
	if err := body.Verify(mf.SHA256); err != nil {
		return nil, 0, fmt.Errorf("the Keeper snapshot file %s: %w", path, err)
	}
	r.cacheError(r.src.cache.write(r.hash, path, body.Gzip))
	return body, rows, nil
}

// copyFromPrevious takes the file from the previous hash's directory when the
// previous manifest has the same SHA-256 for it, and keeps it under this
// hash. It returns nil when it cannot.
func (r *refresh) copyFromPrevious(path string, mf keeper.ManifestFile) *keeper.SnapshotBody {
	prev := r.previousManifest()
	if prev == nil {
		return nil
	}
	pf, ok := prev.Lookup(path)
	if !ok || !strings.EqualFold(pf.SHA256, mf.SHA256) {
		return nil
	}
	gz, ok := r.src.cache.read(r.prevHash, path)
	if !ok {
		return nil
	}
	body := &keeper.SnapshotBody{Gzip: gz}
	if body.Verify(mf.SHA256) != nil {
		return nil
	}
	r.cacheError(r.src.cache.write(r.hash, path, gz))
	return body
}

// decodeFile reads the rows of one snapshot file.
func decodeFile[T any](ctx context.Context, r *refresh, path string) ([]T, error) {
	body, rows, err := r.file(ctx, path)
	if err != nil {
		return nil, err
	}
	out, err := keeper.DecodeSnapshotRows[T](body, rows)
	if err != nil {
		return nil, fmt.Errorf("decoding the Keeper snapshot file %s: %w", path, err)
	}
	return out, nil
}

// loadVendorTable reads the global vendor table, which holds the billing
// facts. A Keeper without the file leaves the table empty (retention then has
// no Keeper data); every other failure fails the load.
func (r *refresh) loadVendorTable(ctx context.Context, data *model.Data) error {
	path := keeper.TablePath("vendor")
	if _, listed := r.manifest.Lookup(path); !listed {
		r.logger.Info("the Keeper snapshot has no vendor table, billing retention has no Keeper data")
		return nil
	}
	rows, err := decodeFile[keeper.Vendor](ctx, r, path)
	if keeper.IsNotFound(err) {
		r.logger.Info("the Keeper snapshot has no vendor table, billing retention has no Keeper data", "error", err.Error())
		return nil
	}
	if err != nil {
		return err
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
	return nil
}

// priceRow is the part of a server_price row the catalog reads (the file has
// more columns; the decoder skips them).
type priceRow struct {
	RegionID        string                 `json:"region_id"`
	ZoneID          string                 `json:"zone_id"`
	ServerID        string                 `json:"server_id"`
	OperatingSystem string                 `json:"operating_system"`
	Allocation      keeper.PriceAllocation `json:"allocation"`
	Unit            string                 `json:"unit"`
	Currency        string                 `json:"currency"`
	Status          string                 `json:"status"`
	Price           float64                `json:"price"`
}

// loadVendor reads a vendor's tables into the data: its servers, regions and
// zones, the server prices of the regions in scope, and its storage and IPv4
// prices. A vendor the manifest has none of the required files of is a
// *noDataError.
func (r *refresh) loadVendor(ctx context.Context, vendor string, allowed []string, data *model.Data) error {
	if !safeName(vendor) {
		return &noDataError{vendor: vendor, table: "valid vendor id"}
	}
	for _, t := range requiredTables {
		if _, ok := r.manifest.Lookup(keeper.VendorTablePath(t, vendor)); !ok {
			return &noDataError{vendor: vendor, table: keeper.VendorTablePath(t, vendor)}
		}
	}
	servers, err := decodeFile[keeper.TableServer](ctx, r, keeper.VendorTablePath("server", vendor))
	if err != nil {
		return err
	}
	for i := range servers {
		s := &servers[i]
		// Retiring types can still be launched: dropping them would make the
		// running nodes drift with instanceTypeNotFound (overview 5.1).
		if s.Status != statusActive && s.Status != statusRetiring {
			continue
		}
		if srv, ok := serverFromTable(s); ok {
			data.Servers = append(data.Servers, srv)
		}
	}
	regions, err := decodeFile[keeper.Region](ctx, r, keeper.VendorTablePath("region", vendor))
	if err != nil {
		return err
	}
	first := len(data.Regions)
	for _, rg := range regions {
		reg := model.Region{
			Vendor: vendor, ID: rg.RegionID, Name: rg.APIReference, Aliases: rg.Aliases,
			Active: rg.Status == statusActive, CountryID: rg.CountryID,
		}
		if rg.City != nil {
			reg.City = *rg.City
		}
		if rg.State != nil {
			reg.State = *rg.State
		}
		reg.GreenEnergy = rg.GreenEnergy
		data.Regions = append(data.Regions, reg)
	}
	zones, err := decodeFile[keeper.Zone](ctx, r, keeper.VendorTablePath("zone", vendor))
	if err != nil {
		return err
	}
	for _, z := range zones {
		data.Zones = append(data.Zones, model.Zone{
			Vendor: vendor, RegionID: z.RegionID, ID: z.ZoneID, Name: z.APIReference, Active: z.Status == statusActive,
		})
	}
	ids := r.regionsInScope(vendor, allowed, data.Regions[first:])
	if ids != nil {
		if err := r.loadPrices(ctx, vendor, ids, data); err != nil {
			return err
		}
	}
	return r.loadAuxPrices(ctx, vendor, data)
}

// regionsInScope returns the ids of the regions whose prices are read: the
// active ones the allow-list names, or all active ones without a list. It
// returns nil when the list names none that Keeper knows.
func (r *refresh) regionsInScope(vendor string, allowed []string, regions []model.Region) map[string]bool {
	ids := map[string]bool{}
	if len(allowed) > 0 {
		resolve := regionResolver(regions)
		for _, id := range resolve(vendor, allowed) {
			ids[id] = true
		}
		for _, n := range allowed {
			if len(resolve(vendor, []string{n})) == 0 {
				r.logger.Info("region of the NodeClass scope is unknown to Keeper, ignored", "vendor", vendor, "region", n)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		return ids
	}
	for i := range regions {
		if regions[i].Active {
			ids[regions[i].ID] = true
		}
	}
	return ids
}

func (r *refresh) loadPrices(ctx context.Context, vendor string, regions map[string]bool, data *model.Data) error {
	rows, err := decodeFile[priceRow](ctx, r, keeper.VendorTablePath("server_price", vendor))
	if err != nil {
		return err
	}
	for i := range rows {
		p := &rows[i]
		if !regions[p.RegionID] {
			continue
		}
		data.Prices = appendPrice(data.Prices, vendor, p.RegionID, p.ZoneID, p.ServerID, p.OperatingSystem,
			p.Allocation, p.Unit, p.Currency, p.Status, p.Price)
	}
	return nil
}

// storageRow is the part of a storage row the catalog reads: the name of the
// storage type, which a vendor's own API uses where its ids are numbers (GCP).
type storageRow struct {
	StorageID string `json:"storage_id"`
	Name      string `json:"name"`
}

// loadAuxPrices reads the vendor's storage and IPv4 prices, for its driver:
// active USD rows with a price above zero, in every region. Regions are named
// as the vendor does (the region table's api_reference), storage types carry
// the source's name besides the id.
func (r *refresh) loadAuxPrices(ctx context.Context, vendor string, data *model.Data) error {
	regionName := map[string]string{}
	for i := range data.Regions {
		if rg := &data.Regions[i]; rg.Vendor == vendor && rg.Name != "" {
			regionName[rg.ID] = rg.Name
		}
	}
	named := func(id string) string {
		if n, ok := regionName[id]; ok {
			return n
		}
		return id
	}
	if path := keeper.VendorTablePath("storage_price", vendor); r.listed(path) {
		storageName := map[string]string{}
		if spath := keeper.VendorTablePath("storage", vendor); r.listed(spath) {
			rows, err := decodeFile[storageRow](ctx, r, spath)
			if err != nil {
				return err
			}
			for _, s := range rows {
				storageName[s.StorageID] = s.Name
			}
		}
		rows, err := decodeFile[keeper.StoragePrice](ctx, r, path)
		if err != nil {
			return err
		}
		for _, p := range rows {
			if p.Status == statusActive && p.Currency == currencyUSD && p.Price > 0 {
				data.StoragePrices = append(data.StoragePrices, model.StoragePrice{
					Vendor: vendor, RegionID: named(p.RegionID), StorageID: p.StorageID,
					StorageName: storageName[p.StorageID], Unit: p.Unit, USD: p.Price,
				})
			}
		}
	}
	if path := keeper.VendorTablePath("ipv4_price", vendor); r.listed(path) {
		rows, err := decodeFile[keeper.IPv4Price](ctx, r, path)
		if err != nil {
			return err
		}
		for _, p := range rows {
			if p.Status == statusActive && p.Currency == currencyUSD && p.Price > 0 {
				data.IPv4Prices = append(data.IPv4Prices, model.IPv4Price{
					Vendor: vendor, RegionID: named(p.RegionID), Unit: p.Unit, USD: p.Price,
				})
			}
		}
	}
	return nil
}

func (r *refresh) listed(path string) bool {
	_, ok := r.manifest.Lookup(path)
	return ok
}

// benchmarkCatalog reads /benchmark_configs once per refresh.
func (r *refresh) benchmarkCatalog(ctx context.Context) (*keeper.BenchmarkCatalog, error) {
	if r.benchmarks != nil {
		return r.benchmarks, nil
	}
	configs, err := r.client.BenchmarkConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading /benchmark_configs: %w", err)
	}
	cat, err := keeper.NewBenchmarkCatalog(configs)
	if err != nil {
		return nil, fmt.Errorf("reading /benchmark_configs: %w", err)
	}
	r.benchmarks = cat
	return cat, nil
}

// scoreRowsOf reads the score file of a benchmark at a vendor (every config
// in one file), once per refresh.
func (r *refresh) scoreRowsOf(ctx context.Context, vendor, benchmarkID string) ([]keeper.BenchmarkScore, error) {
	path := keeper.ScorePath(vendor, benchmarkID)
	if rows, ok := r.scoreRows[path]; ok {
		return rows, nil
	}
	rows, err := decodeFile[keeper.BenchmarkScore](ctx, r, path)
	if err != nil {
		return nil, err
	}
	r.scoreRows[path] = rows
	return rows, nil
}

// scores is the score per server id of one benchmark config at a vendor, by
// the rules of /servers (selection.ScoresOf; the config text is compared byte
// for byte).
func (r *refresh) scores(ctx context.Context, vendor string, key keeper.BenchmarkKey) (map[string]float64, error) {
	if !safeName(vendor) || !safeName(key.ID) {
		return nil, nil
	}
	rows, err := r.scoreRowsOf(ctx, vendor, key.ID)
	if err != nil {
		return nil, err
	}
	return selection.ScoresOf(rows, key.ID, key.Config), nil
}

func (r *refresh) loadMetrics(ctx context.Context, queries []MetricQuery, data *model.Data,
	missing map[string]bool) error {
	if len(queries) == 0 {
		return nil
	}
	catalog, err := r.benchmarkCatalog(ctx)
	if err != nil {
		return err
	}
	for _, q := range queries {
		if missing[q.Vendor] {
			data.Invalid[q.ID] = "Keeper has no data for vendor " + q.Vendor
			continue
		}
		key, err := resolveBenchmark(catalog, q)
		if err != nil {
			data.Invalid[q.ID] = err.Error()
			continue
		}
		byID, err := r.scores(ctx, q.Vendor, key)
		if err != nil {
			return err
		}
		if len(byID) == 0 {
			data.Invalid[q.ID] = fmt.Sprintf("Keeper has no scores of benchmark %s for %s in data version %s",
				key, q.Vendor, r.hash)
			continue
		}
		data.Scores[q.ID] = byAPIReference(data, q.Vendor, byID)
	}
	return nil
}

// loadSelections evaluates the selection profiles on the data of the refresh:
// the servers and prices already read, and the scores of the benchmarks the
// profile uses (the same source as the metrics').
func (r *refresh) loadSelections(ctx context.Context, queries []SelectionQuery, data *model.Data, local *localData,
	missing map[string]bool) error {
	if len(queries) == 0 {
		return nil
	}
	catalog, err := r.benchmarkCatalog(ctx)
	if err != nil {
		return err
	}
	opts := selection.Options{Catalog: catalog, DefaultBasis: selapi.PriceBasisOnDemand}
	for _, q := range queries {
		if missing[q.Vendor] {
			data.Invalid[q.ID] = "Keeper has no data for vendor " + q.Vendor
			continue
		}
		var ids []string
		if len(q.Regions) > 0 {
			if ids = regionResolver(data.Regions)(q.Vendor, q.Regions); len(ids) == 0 {
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
			if scores[key], err = r.scores(ctx, q.Vendor, key); err != nil {
				return fmt.Errorf("reading the scores of selection %s: %w", q.Vendor, err)
			}
		}
		cands := local.candidates(q.Vendor, ids, scores, basis)
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

// localData is what a refresh derives once from its data for the local
// selection: the vendors' specification rows and prices. Selection profiles
// of one refresh share it.
type localData struct {
	data   *model.Data
	rows   map[string][]keeper.Server
	prices map[string][]selection.CatalogPrice
}

func newLocalData(data *model.Data) *localData { return &localData{data: data} }

// candidates are the candidates of a vendor in the regions (empty: all) that
// price on the basis, from the data of the refresh.
func (l *localData) candidates(vendor string, regionIDs []string,
	scores map[keeper.BenchmarkKey]map[string]float64, basis keeper.Allocation) []*selection.Candidate {
	if l.rows == nil {
		l.split()
	}
	return selection.BuildCatalogCandidates(selection.CatalogInput{
		Servers: l.rows[vendor], Prices: slices.Values(l.prices[vendor]), Regions: regionIDs, Scores: scores,
	}, basis)
}

// split groups the servers (as selection's specification rows) and the prices
// of the data by vendor. The prices were filtered by selection.AcceptPrice when
// they were read, so neutral values stand for the columns the catalog does not
// keep.
func (l *localData) split() {
	l.rows = map[string][]keeper.Server{}
	l.prices = map[string][]selection.CatalogPrice{}
	for i := range l.data.Servers {
		s := &l.data.Servers[i]
		l.rows[s.Vendor] = append(l.rows[s.Vendor], specRow(s))
	}
	for i := range l.data.Prices {
		p := &l.data.Prices[i]
		alloc := keeper.PriceAllocationOnDemand
		if p.CapacityType == model.CapacitySpot {
			alloc = keeper.PriceAllocationSpot
		}
		l.prices[p.Vendor] = append(l.prices[p.Vendor], selection.CatalogPrice{
			ServerID: p.ServerID, RegionID: p.RegionID, ZoneID: p.ZoneID, Allocation: alloc,
			Unit: "hour", Currency: currencyUSD, Status: statusActive, Price: p.USDPerHour,
		})
	}
}

// familyOf is the instance family of a server: Keeper's series (the hardware
// line: t3a, n2, cpx), else its family. Since sc-crawler 0.10.4 the family
// column is the broad class ("General Purpose") where a series exists; vendors
// without series (UpCloud, Vultr bare metal) keep the family as it is.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func familyOf(s *keeper.TableServer) string {
	if s.Series != nil && *s.Series != "" {
		return *s.Series
	}
	return deref(s.Family)
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
	srv := model.Server{
		Vendor: s.VendorID, ID: s.ServerID, APIReference: s.APIReference, Family: familyOf(s),
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

// appendPrice keeps the prices selection counts (selection.AcceptPrice: active,
// hourly, USD, Linux or blank, above zero, on-demand or spot), so offerings
// and selection profiles see the same prices.
func appendPrice(prices []model.Price, vendor, region, zone, server, opSys string, alloc keeper.PriceAllocation,
	unit, currency, status string, price float64) []model.Price {
	a, ok := selection.AcceptPrice(status, unit, currency, opSys, alloc, price)
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
