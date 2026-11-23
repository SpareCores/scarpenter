/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package facts answers the catalog's questions about the drivers (the
// catalog.Facts interface): which zones each region has, the root disk and the
// profiles of the defaults, the price of the defaults and which instance types
// the account can launch where. The answers come from the driver registry and
// are loaded in the background, so building the catalog never calls a vendor.
package facts

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
	"github.com/SpareCores/scarpenter/pkg/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Defaults of the reload loop.
const (
	// DefaultInterval is how often regions and zones are read again.
	DefaultInterval = time.Hour
	// DefaultVendorTimeout bounds the load of one vendor.
	DefaultVendorTimeout = 3 * time.Minute
	// DefaultRegionTimeout bounds each driver call of one region (the driver's
	// own call timeout, 45 s with retries, would stall a vendor for minutes
	// on a region that never answers).
	DefaultRegionTimeout = 30 * time.Second
	// maxParallelRegions bounds the regions of a vendor read at once.
	maxParallelRegions = 8
	// retryInterval is how often Start looks for failed regions that are due,
	// and the first backoff of a failed region; the backoff doubles up to
	// maxBackoff.
	retryInterval = 10 * time.Second
	maxBackoff    = 15 * time.Minute
)

// Launchable is the optional view of which types the account can launch where;
// the quota tracker implements it.
type Launchable interface {
	Launchable(vendor, apiReference, zoneID string) (launchable, known bool)
	LaunchableEpoch() uint64
}

// Options configures Facts.
type Options struct {
	Registry *vendors.Registry
	// Launchable may be nil.
	Launchable Launchable
	Interval   time.Duration
	// VendorTimeout bounds the load of one vendor; DefaultVendorTimeout if zero.
	VendorTimeout time.Duration
	// RegionTimeout bounds the listing of the regions and of the zones of one
	// region; DefaultRegionTimeout if zero.
	RegionTimeout time.Duration
	Clock         clock.Clock
}

type vendorFacts struct {
	// zoneRegion maps a zone name to its region.
	zoneRegion map[string]string
	regions    []string
	zones      map[string][]catalog.Zone
	hasZones   bool
	// skipped maps the regions the credentials cannot read to the reason;
	// skipCode to the class and code of the error, which, unlike the reason
	// (it carries the request ID), stays the same from one load to the next.
	skipped  map[string]string
	skipCode map[string]string
	// failed are the regions that could not be read in the last load. They
	// are in regions (with their last-known zones, or none), and retried when
	// their backoff expires.
	failed map[string]failure
}

// failure is the retry state of a region (or of a vendor with no facts).
type failure struct {
	attempts int
	// next is when the retry is due.
	next time.Time
	// stale is set when the region has last-known zones.
	stale bool
}

// backoffDelay is the wait after the given number of failed attempts: 10 s,
// doubling up to 15 min.
func backoffDelay(attempts int) time.Duration {
	d := retryInterval
	for i := 1; i < attempts && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func newFailure(prev failure, stale bool, now time.Time) failure {
	n := prev.attempts + 1
	return failure{attempts: n, next: now.Add(backoffDelay(n)), stale: stale}
}

// failure returns the retry state of the region, if it failed.
func (vf *vendorFacts) failure(region string) failure {
	if vf == nil {
		return failure{}
	}
	return vf.failed[region]
}

// settled reports whether the region needs no read at the time: it was read,
// is skipped because the credentials cannot read it, or failed and its retry
// is not due.
func (vf *vendorFacts) settled(region string, now time.Time) bool {
	if vf == nil {
		return false
	}
	if _, skipped := vf.skipped[region]; skipped {
		return true
	}
	if _, read := vf.zones[region]; !read {
		return false
	}
	fl, failed := vf.failed[region]
	return !failed || now.Before(fl.next)
}

// due reports whether a failed region is due for a retry.
func (vf *vendorFacts) due(now time.Time) bool {
	for _, fl := range vf.failed {
		if !now.Before(fl.next) {
			return true
		}
	}
	return false
}

// Facts implements catalog.Facts and catalog.LaunchableFacts over the
// registry. It is safe for concurrent use.
type Facts struct {
	opts Options

	mu   sync.RWMutex
	data map[string]*vendorFacts
	// skippedSeen remembers the skipped regions already warned about
	// (vendor/region -> class and code of the error).
	skippedSeen map[string]string
	// vendorFailed is the retry state of the vendors that have no facts.
	vendorFailed map[string]failure
	epoch        atomic.Uint64

	defaults  sync.Map // class hash/vendor -> resolved
	defaultsN atomic.Int64
}

var (
	_ catalog.Facts             = (*Facts)(nil)
	_ catalog.LaunchableFacts   = (*Facts)(nil)
	_ catalog.RegionPricedFacts = (*Facts)(nil)
	_ catalog.VirtualGPUFacts   = (*Facts)(nil)
	_ catalog.PriceSink         = (*Facts)(nil)
)

// New returns facts without data: Load (or Start) reads regions and zones.
func New(opts Options) *Facts {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.VendorTimeout <= 0 {
		opts.VendorTimeout = DefaultVendorTimeout
	}
	if opts.RegionTimeout <= 0 {
		opts.RegionTimeout = DefaultRegionTimeout
	}
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	return &Facts{opts: opts, data: map[string]*vendorFacts{}, vendorFailed: map[string]failure{}}
}

// Load reads the regions and the zones of every region of every enabled
// vendor. The vendors load concurrently, each bounded by Options.VendorTimeout,
// and each is published as soon as it is read, so a vendor that hangs or fails
// holds back no other. One region cannot block the others either:
//
//   - a region whose zones the credentials cannot read (the driver's
//     RegionUnavailable: denied by policy, not opted in) is skipped, with one
//     warning when it starts being skipped (later loads log it at V(1));
//   - a region that fails otherwise (throttling, network, 5xx) keeps its
//     last-known zones, or is left out when it has none, and Load returns the
//     error so that Start retries soon, but the other regions' facts are used;
//   - only a vendor with no readable region at all, or one that did not finish
//     in time, fails as a whole: it keeps its previous facts (Loaded stays
//     false until it has some), the error is logged and returned.
func (f *Facts) Load(ctx context.Context) error { return f.load(ctx, false) }

// load is Load; with retryOnly it reads again only the failed regions whose
// backoff expired and the vendors without facts whose backoff expired.
func (f *Facts) load(ctx context.Context, retryOnly bool) error {
	logger := log.FromContext(ctx).WithName("driver-facts")
	names := f.opts.Registry.Vendors()
	logger.V(1).Info("loading the driver facts", "vendors", names, "retry", retryOnly)
	now := f.opts.Clock.Now()
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, vendor := range names {
		wg.Go(func() { errs[i] = f.loadOne(ctx, logger, vendor, retryOnly, now) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// pending reports whether a vendor has no facts or has a failed region.
func (f *Facts) pending() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, vendor := range f.opts.Registry.Vendors() {
		if vf := f.data[vendor]; vf == nil || len(vf.failed) > 0 {
			return true
		}
	}
	return false
}

// loadOne reads one vendor within the timeout and publishes its facts.
func (f *Facts) loadOne(ctx context.Context, logger logr.Logger, vendor string, retryOnly bool, now time.Time) error {
	drv, _ := f.opts.Registry.Driver(vendor)
	f.mu.RLock()
	prev := f.data[vendor]
	vfail := f.vendorFailed[vendor]
	f.mu.RUnlock()
	if retryOnly && (prev == nil && now.Before(vfail.next) || prev != nil && !prev.due(now)) {
		return nil
	}
	vctx, cancel := context.WithTimeout(ctx, f.opts.VendorTimeout)
	defer cancel()
	vf, softErrs, err := f.loadVendor(vctx, drv, prev, retryOnly, now)
	if err != nil {
		err = fmt.Errorf("vendor %s: %w", vendor, err)
		if ctx.Err() == nil {
			f.mu.Lock()
			f.vendorFailed[vendor] = newFailure(vfail, false, now)
			f.mu.Unlock()
			logger.Error(err, "loading the driver facts of a vendor failed, it is retried with backoff", "vendor", vendor)
		}
		return err
	}
	if !maps.EqualFunc(prev.failedSet(), vf.failed, func(failure, failure) bool { return true }) && len(vf.failed) > 0 {
		logger.Error(errors.Join(softErrs...), "loading the zones of some regions failed, they are retried with backoff",
			"vendor", vendor, "failedRegions", slices.Sorted(maps.Keys(vf.failed)))
	}
	f.mu.Lock()
	old, had := f.data[vendor]
	changed := !had || !sameFacts(old, vf)
	f.data[vendor] = vf
	delete(f.vendorFailed, vendor)
	f.logSkipped(logger, vendor, vf)
	f.mu.Unlock()
	switch {
	case !had:
		f.epoch.Add(1)
		logger.Info("loaded the driver facts of a vendor", "vendor", vendor, "regions", len(vf.regions))
	case changed:
		f.epoch.Add(1)
		logger.V(1).Info("the driver facts of a vendor changed", "vendor", vendor, "regions", len(vf.regions))
	}
	var soft []error
	for _, e := range softErrs {
		soft = append(soft, fmt.Errorf("vendor %s: %w", vendor, e))
	}
	return errors.Join(soft...)
}

// failedSet returns the failed regions of the vendor (nil-safe).
func (vf *vendorFacts) failedSet() map[string]failure {
	if vf == nil {
		return nil
	}
	return vf.failed
}

// logSkipped warns once for each region that starts being skipped, and says
// when one is read again. Called with f.mu held.
func (f *Facts) logSkipped(logger logr.Logger, vendor string, vf *vendorFacts) {
	if f.skippedSeen == nil {
		f.skippedSeen = map[string]string{}
	}
	skipped := vf.skipped
	for region, why := range skipped {
		key := vendor + "/" + region
		if code := vf.skipCode[region]; f.skippedSeen[key] == code {
			logger.V(1).Info("still skipping a region the credentials cannot read",
				"vendor", vendor, "region", region, "reason", why)
			continue
		}
		f.skippedSeen[key] = vf.skipCode[region]
		logger.Info("skipping a region the credentials cannot read: its zones and offerings are not loaded",
			"vendor", vendor, "region", region, "reason", why)
	}
	for key := range f.skippedSeen {
		if v, r, _ := strings.Cut(key, "/"); v == vendor {
			if _, still := skipped[r]; !still {
				delete(f.skippedSeen, key)
				logger.Info("a skipped region can be read again", "vendor", vendor, "region", r)
			}
		}
	}
}

// loadVendor reads the zones of the regions of the vendor, up to
// maxParallelRegions at once, each within RegionTimeout. errs are the regions
// that failed for another reason than RegionUnavailable (also in vf.failed);
// err is set when no region could be read at all. With retryOnly the regions
// that prev has settled are kept without a call.
func (f *Facts) loadVendor(
	ctx context.Context, drv vendors.Driver, prev *vendorFacts, retryOnly bool, now time.Time,
) (vf *vendorFacts, errs []error, err error) {
	rctx, cancel := context.WithTimeout(ctx, f.opts.RegionTimeout)
	regions, err := drv.Regions(rctx)
	cancel()
	if err != nil {
		return nil, nil, fmt.Errorf("listing regions: %w", err)
	}
	type result struct {
		zones []vendors.Zone
		err   error
		kept  bool
	}
	results := make([]result, len(regions))
	sem := make(chan struct{}, maxParallelRegions)
	var wg sync.WaitGroup
	for i, r := range regions {
		if retryOnly && prev.settled(r.Name, now) {
			results[i].kept = true
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			zctx, cancel := context.WithTimeout(ctx, f.opts.RegionTimeout)
			defer cancel()
			results[i].zones, results[i].err = drv.Zones(zctx, r.Name)
		})
	}
	wg.Wait()

	vf = &vendorFacts{
		zones: map[string][]catalog.Zone{}, zoneRegion: map[string]string{},
		skipped: map[string]string{}, skipCode: map[string]string{}, failed: map[string]failure{},
	}
	var skipped []error
	readable := 0 // the regions with zones, read now or earlier
	for i, r := range regions {
		var cz []catalog.Zone
		read := true // false for a region that failed and has no last-known zones
		switch res := results[i]; {
		case res.kept:
			if why, ok := prev.skipped[r.Name]; ok {
				vf.skipped[r.Name] = why
				vf.skipCode[r.Name] = prev.skipCode[r.Name]
				continue
			}
			cz = prev.zones[r.Name]
			if fl, failed := prev.failed[r.Name]; failed {
				vf.failed[r.Name] = fl
				read = fl.stale
			}
		case res.err == nil:
			cz = make([]catalog.Zone, 0, len(res.zones))
			for _, z := range res.zones {
				cz = append(cz, catalog.Zone{ID: z.ID, Name: z.Name})
			}
			slices.SortFunc(cz, func(a, b catalog.Zone) int { return compare(a.Name, b.Name) })
		case vendors.IsClass(res.err, vendors.RegionUnavailable):
			vf.skipped[r.Name] = res.err.Error()
			vf.skipCode[r.Name] = string(vendors.ClassOf(res.err)) + "/" + vendors.CodeOf(res.err)
			skipped = append(skipped, fmt.Errorf("region %s: %w", r.Name, res.err))
			continue
		default:
			errs = append(errs, fmt.Errorf("listing the zones of %s: %w", r.Name, res.err))
			old, stale := prev.zonesOf(r.Name)
			vf.failed[r.Name] = newFailure(prev.failure(r.Name), stale, now)
			cz = old
			read = stale
		}
		if read {
			readable++
		}
		vf.regions = append(vf.regions, r.Name)
		vf.zones[r.Name] = cz
		for _, z := range cz {
			vf.zoneRegion[z.Name] = r.Name
		}
		vf.hasZones = vf.hasZones || len(cz) > 0
	}
	slices.Sort(vf.regions)
	if len(regions) > 0 && readable == 0 {
		return nil, nil, fmt.Errorf("no region could be read: %w", errors.Join(append(errs, skipped...)...))
	}
	return vf, errs, nil
}

// zonesOf returns the zones last loaded of the region.
func (vf *vendorFacts) zonesOf(region string) ([]catalog.Zone, bool) {
	if vf == nil {
		return nil, false
	}
	if fl, failed := vf.failed[region]; failed && !fl.stale {
		return nil, false
	}
	z, ok := vf.zones[region]
	return z, ok
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func sameFacts(a, b *vendorFacts) bool {
	if a.hasZones != b.hasZones || !slices.Equal(a.regions, b.regions) || len(a.zones) != len(b.zones) {
		return false
	}
	for r, za := range a.zones {
		if !slices.Equal(za, b.zones[r]) {
			return false
		}
	}
	return true
}

// Loaded reports whether the vendor has been read at least once. It is per
// vendor so that one whose facts keep failing does not hold back the others.
func (f *Facts) Loaded(vendor string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.data[vendor] != nil
}

// Start loads the facts until the context ends. Everything is read every
// interval; in between, every 10 s, the failed regions and the vendors without
// facts whose backoff expired are read again. It implements controller-runtime's
// Runnable.
func (f *Facts) Start(ctx context.Context) error {
	var lastFull time.Time
	for {
		now := f.opts.Clock.Now()
		full := lastFull.IsZero() || now.Sub(lastFull) >= f.opts.Interval
		if full {
			lastFull = now
		}
		_ = f.load(ctx, !full) // failures are logged per vendor
		wait := f.opts.Interval - f.opts.Clock.Since(lastFull)
		if f.pending() {
			wait = min(wait, retryInterval)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-f.opts.Clock.After(max(wait, 0)):
		}
	}
}

// NeedLeaderElection says that the facts load on the leader only.
func (f *Facts) NeedLeaderElection() bool { return true }

// Regions returns the regions of the vendor the credentials can use; nil until
// they are loaded.
func (f *Facts) Regions(vendor string) []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if vf := f.data[vendor]; vf != nil {
		return slices.Clone(vf.regions)
	}
	return nil
}

// SkippedRegions returns the regions of the vendor that were skipped because
// the credentials cannot read them, with the reason.
func (f *Facts) SkippedRegions(vendor string) map[string]string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if vf := f.data[vendor]; vf != nil {
		return maps.Clone(vf.skipped)
	}
	return nil
}

// UnavailableRegions returns the sorted regions of the vendor that have no
// zones for lack of a read: the skipped ones (the credentials cannot read
// them) and the failed ones without last-known zones (retried with backoff).
func (f *Facts) UnavailableRegions(vendor string) []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	vf := f.data[vendor]
	if vf == nil {
		return nil
	}
	out := slices.Collect(maps.Keys(vf.skipped))
	for r, fl := range vf.failed {
		if !fl.stale {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}

// Zones implements catalog.Facts.
func (f *Facts) Zones(vendor string) (map[string][]catalog.Zone, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	vf := f.data[vendor]
	if vf == nil {
		// Nothing loaded yet: no zones, so no offerings, rather than offerings
		// the driver may not be able to launch.
		return map[string][]catalog.Zone{}, true
	}
	return vf.zones, vf.hasZones
}

func (f *Facts) nodeOptions(vendor string) vendors.NodeOptions {
	if drv, ok := f.opts.Registry.Driver(vendor); ok {
		if no, ok := drv.(vendors.NodeOptions); ok {
			return no
		}
	}
	return nil
}

// defaultsFor returns the resolved settings of the class's default route (the
// driver's defaults and the NodeClass's vendor block), cached per class
// content: the catalog asks for every offering of every type.
func (f *Facts) defaultsFor(
	vendor string, class *catalog.Class, no vendors.NodeOptions,
) (vendors.VendorSettings, bool) {
	key := class.Hash + "/" + vendor
	if v, ok := f.defaults.Load(key); ok {
		d := v.(resolved)
		return d.settings, d.ok
	}
	s, err := options.Resolve(no, class.Inputs[vendor], options.Selection{})
	if f.defaultsN.Add(1) > maxCachedDefaults {
		f.defaults.Clear()
		f.defaultsN.Store(0)
	}
	f.defaults.Store(key, resolved{settings: s, ok: err == nil})
	return s, err == nil
}

type resolved struct {
	settings vendors.VendorSettings
	ok       bool
}

// maxCachedDefaults bounds the cache of resolved defaults; NodeClass edits
// leave entries of old content behind.
const maxCachedDefaults = 256

// RootDiskBytes implements catalog.Facts: the root disk of the type under the
// class's resolved default settings. The driver answers for a type that
// defines its own root disk (RootDiskForType); else the settings' root disk;
// for 0, or a vendor without NodeOptions, the catalog's storage size of the
// type (Keeper's storage_size, taken as GB; 0 when the type has none).
func (f *Facts) RootDiskBytes(vendor string, class *catalog.Class, srv *model.Server) (int64, bool) {
	catalogBytes := func() (int64, bool) {
		b := int64(srv.LocalStorageGB * 1e9)
		return b, b > 0
	}
	no := f.nodeOptions(vendor)
	if no == nil {
		return catalogBytes()
	}
	s, ok := f.defaultsFor(vendor, class, no)
	if !ok {
		s = no.Defaults()
		s.RootDiskBytes = no.RootDiskBytes(s)
	}
	if b, ok := no.RootDiskForType(s, catalogType(vendor, srv)); ok && b > 0 {
		return b, true
	}
	if s.RootDiskBytes > 0 {
		return s.RootDiskBytes, true
	}
	return catalogBytes()
}

func catalogType(vendor string, srv *model.Server) vendors.CatalogType {
	return vendors.CatalogType{
		Vendor: vendor, APIReference: srv.APIReference, Family: srv.Family, Arch: srv.Arch,
		VCPUs: srv.VCPUs, MemoryMiB: srv.MemoryMiB,
		LocalStorageGB: srv.LocalStorageGB, LocalStorageCount: srv.LocalStorageCount,
		CompatibleStorage: srv.CompatibleStorage,
	}
}

// regionOf returns the region of a zone name, "" when unknown.
func (f *Facts) regionOf(vendor, zone string) string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	vf := f.data[vendor]
	if vf == nil {
		return ""
	}
	if region, ok := vf.zoneRegion[zone]; ok {
		return region
	}
	return zone // a vendor without zones is offered with zone = region
}

// Supported implements catalog.Facts: the driver says which of the class's
// profiles the type can host in the zone. With pricing.allowIncomplete false a
// profile whose price is incomplete is not supported either, so that the
// scheduler rejects it instead of Create (00a section 10).
func (f *Facts) Supported(vendor string, class *catalog.Class, srv *model.Server, zone string,
) map[labels.ProfileKind][]string {
	no := f.nodeOptions(vendor)
	in := class.Inputs[vendor]
	if no == nil || len(in.Profiles) == 0 {
		return nil
	}
	ct := catalogType(vendor, srv)
	var got map[string][]string
	if na, ok := no.(vendors.NetworkAwareOptions); ok {
		got = na.SupportedFor(ct, zone, in.Networking, vendors.Profiles(in.Profiles))
	} else {
		got = no.Supported(ct, zone, vendors.Profiles(in.Profiles))
	}
	if len(got) == 0 {
		return nil
	}
	region := f.regionOf(vendor, zone)
	out := map[labels.ProfileKind][]string{}
	for kind, names := range got {
		for _, name := range names {
			if _, defined := in.Profiles[kind][name]; !defined {
				continue
			}
			if kind == vendors.ProfileStorage && !f.storageHostable(no, class, in, vendor, name, ct) {
				continue
			}
			if !class.AllowIncompletePrice && !f.profileComplete(no, in, kind, name, ct, region, zone) {
				continue
			}
			out[labels.ProfileKind(kind)] = append(out[labels.ProfileKind(kind)], name)
		}
	}
	// A driver builds its answer from maps: the order is not its to define.
	for _, names := range out {
		slices.Sort(names)
	}
	return out
}

func selectionOf(kind, name string) options.Selection {
	sel := options.Selection{}
	switch kind {
	case vendors.ProfileStorage:
		sel.Storage = name
	case vendors.ProfileNetwork:
		sel.Network = name
	default:
		sel.Placement = name
	}
	return sel
}

// storageHostable says whether the type can host the settings of a storage
// profile (resolved once per class, vendor and profile, like the defaults).
func (f *Facts) storageHostable(
	no vendors.NodeOptions, class *catalog.Class, in options.Input, vendor, name string, ct vendors.CatalogType,
) bool {
	sc, ok := no.(vendors.SettingsChecker)
	if !ok {
		return true
	}
	key := class.Hash + "/" + vendor + "/" + vendors.ProfileStorage + "/" + name
	var r resolved
	if v, ok := f.defaults.Load(key); ok {
		r = v.(resolved)
	} else {
		s, err := options.Resolve(no, in, selectionOf(vendors.ProfileStorage, name))
		r = resolved{settings: s, ok: err == nil}
		if f.defaultsN.Add(1) > maxCachedDefaults {
			f.defaults.Clear()
			f.defaultsN.Store(0)
		}
		f.defaults.Store(key, r)
	}
	return r.ok && sc.SupportsSettings(r.settings, ct)
}

// DefaultSupported implements catalog.DefaultRouteFacts: the type can host the
// class's default settings.
func (f *Facts) DefaultSupported(vendor string, class *catalog.Class, srv *model.Server) bool {
	no := f.nodeOptions(vendor)
	if no == nil {
		return true
	}
	sc, ok := no.(vendors.SettingsChecker)
	if !ok {
		return true
	}
	s, ok := f.defaultsFor(vendor, class, no)
	return !ok || sc.SupportsSettings(s, catalogType(vendor, srv))
}

func (f *Facts) profileComplete(no vendors.NodeOptions, in options.Input, kind, name string, ct vendors.CatalogType,
	region, zone string) bool {
	s, err := options.Resolve(no, in, selectionOf(kind, name))
	if err != nil {
		return false
	}
	_, complete := no.PriceDelta(s, vendors.Offering{
		Region: region, Zone: zone, CapacityType: vendors.CapacityOnDemand, APIReference: ct.APIReference, Type: ct,
	})
	return complete
}

// PriceDelta implements catalog.Facts: the price of the class's default
// settings.
func (f *Facts) PriceDelta(vendor string, class *catalog.Class, srv *model.Server, zone, capacityType string,
) (float64, bool) {
	no := f.nodeOptions(vendor)
	if no == nil {
		return 0, true
	}
	s, ok := f.defaultsFor(vendor, class, no)
	if !ok {
		return 0, true // the NodeClass controller reports the invalid block
	}
	ct := catalogType(vendor, srv)
	return no.PriceDelta(s, vendors.Offering{
		Region: f.regionOf(vendor, zone), Zone: zone, CapacityType: capacityType, APIReference: srv.APIReference, Type: ct,
	})
}

// RegionPriced implements catalog.RegionPricedFacts: the driver says its prices
// are per region (vendors.RegionPriced).
func (f *Facts) RegionPriced(vendor string) bool {
	if drv, ok := f.opts.Registry.Driver(vendor); ok {
		if rp, ok := drv.(vendors.RegionPriced); ok {
			return rp.PricesPerRegion()
		}
	}
	return false
}

// VirtualGPU implements catalog.VirtualGPUFacts: the driver says which types
// carry a virtual GPU (vendors.GPUVirtualizer).
func (f *Facts) VirtualGPU(vendor, apiReference string) bool {
	if drv, ok := f.opts.Registry.Driver(vendor); ok {
		if v, ok := drv.(vendors.GPUVirtualizer); ok {
			return v.Virtual(apiReference)
		}
	}
	return false
}

// Launchable implements catalog.LaunchableFacts.
func (f *Facts) Launchable(vendor, apiReference, zoneID string) (launchable, known bool) {
	if f.opts.Launchable == nil {
		return false, false
	}
	return f.opts.Launchable.Launchable(vendor, apiReference, zoneID)
}

// Epoch implements catalog.Facts: it changes when the zones or the launchable
// sets change.
func (f *Facts) Epoch() uint64 {
	e := f.epoch.Load()
	for _, vendor := range f.opts.Registry.Vendors() {
		if drv, ok := f.opts.Registry.Driver(vendor); ok {
			if r, ok := drv.(vendors.EpochReporter); ok {
				e += r.NodeOptionsEpoch()
			}
		}
	}
	if f.opts.Launchable != nil {
		// The two counters only grow, so their sum changes whenever one does.
		e += f.opts.Launchable.LaunchableEpoch()
	}
	return e
}

// SetKeeperPrices implements catalog.PriceSink: it hands each vendor's storage
// and IPv4 price rows to its driver, when the driver is a
// vendors.KeeperPriceConsumer. A consumer of a vendor without rows gets none:
// its prices become unknown, as for any region the data lacks.
func (f *Facts) SetKeeperPrices(version string, storage []model.StoragePrice, ipv4 []model.IPv4Price) {
	byVendor := map[string]*vendors.KeeperPrices{}
	of := func(vendor string) *vendors.KeeperPrices {
		p, ok := byVendor[vendor]
		if !ok {
			p = &vendors.KeeperPrices{Hash: version}
			byVendor[vendor] = p
		}
		return p
	}
	for _, r := range storage {
		p := of(r.Vendor)
		p.Storage = append(p.Storage, vendors.StoragePrice{
			Region: r.RegionID, StorageID: r.StorageID, Name: r.StorageName, Unit: r.Unit, USD: r.USD,
		})
	}
	for _, r := range ipv4 {
		p := of(r.Vendor)
		p.IPv4 = append(p.IPv4, vendors.IPv4Price{Region: r.RegionID, Unit: r.Unit, USD: r.USD})
	}
	for _, vendor := range f.opts.Registry.Vendors() {
		drv, _ := f.opts.Registry.Driver(vendor)
		consumer, ok := drv.(vendors.KeeperPriceConsumer)
		if !ok {
			continue
		}
		if p := byVendor[vendor]; p != nil {
			consumer.SetKeeperPrices(*p)
		} else {
			consumer.SetKeeperPrices(vendors.KeeperPrices{Hash: version})
		}
	}
}

// LogFacts logs what was loaded, for the start-up log.
func (f *Facts) LogFacts(logger logr.Logger) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for vendor, vf := range f.data {
		logger.Info("driver facts", "vendor", vendor, "regions", len(vf.regions), "zones", len(vf.zones),
			"skippedRegions", slices.Sorted(maps.Keys(vf.skipped)))
	}
}
