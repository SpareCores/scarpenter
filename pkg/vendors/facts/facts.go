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
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Defaults of the reload loop.
const (
	// DefaultInterval is how often regions and zones are read again.
	DefaultInterval = time.Hour
	// retryInterval is the wait after a failed load.
	retryInterval = 10 * time.Second
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
	Clock      clock.Clock
}

type vendorFacts struct {
	regions  []string
	zones    map[string][]catalog.Zone
	hasZones bool
}

// Facts implements catalog.Facts and catalog.LaunchableFacts over the
// registry. It is safe for concurrent use.
type Facts struct {
	opts Options

	mu     sync.RWMutex
	data   map[string]*vendorFacts
	loaded bool
	epoch  atomic.Uint64
}

var (
	_ catalog.Facts           = (*Facts)(nil)
	_ catalog.LaunchableFacts = (*Facts)(nil)
)

// New returns facts without data: Load (or Start) reads regions and zones.
func New(opts Options) *Facts {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	return &Facts{opts: opts, data: map[string]*vendorFacts{}}
}

// Load reads the regions and the zones of every region of every enabled
// vendor. A vendor that fails keeps its previous facts; the errors are joined.
func (f *Facts) Load(ctx context.Context) error {
	var errs []error
	next := map[string]*vendorFacts{}
	for _, vendor := range f.opts.Registry.Vendors() {
		drv, _ := f.opts.Registry.Driver(vendor)
		vf, err := loadVendor(ctx, drv)
		if err != nil {
			errs = append(errs, fmt.Errorf("vendor %s: %w", vendor, err))
			continue
		}
		next[vendor] = vf
	}
	f.mu.Lock()
	changed := !f.loaded
	for vendor, vf := range next {
		if old, ok := f.data[vendor]; !ok || !sameFacts(old, vf) {
			changed = true
		}
		f.data[vendor] = vf
	}
	f.loaded = len(errs) == 0
	f.mu.Unlock()
	if changed {
		f.epoch.Add(1)
	}
	return errors.Join(errs...)
}

func loadVendor(ctx context.Context, drv vendors.Driver) (*vendorFacts, error) {
	regions, err := drv.Regions(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing regions: %w", err)
	}
	vf := &vendorFacts{zones: map[string][]catalog.Zone{}}
	for _, r := range regions {
		vf.regions = append(vf.regions, r.Name)
		zones, err := drv.Zones(ctx, r.Name)
		if err != nil {
			return nil, fmt.Errorf("listing the zones of %s: %w", r.Name, err)
		}
		cz := make([]catalog.Zone, 0, len(zones))
		for _, z := range zones {
			cz = append(cz, catalog.Zone{ID: z.ID, Name: z.Name})
		}
		slices.SortFunc(cz, func(a, b catalog.Zone) int { return compare(a.Name, b.Name) })
		vf.zones[r.Name] = cz
		vf.hasZones = vf.hasZones || len(cz) > 0
	}
	slices.Sort(vf.regions)
	return vf, nil
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

// Loaded reports whether every vendor has been read at least once.
func (f *Facts) Loaded() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.loaded
}

// Start loads the facts until the context ends: retrying every ten seconds
// until the first success, then every interval. It implements
// controller-runtime's Runnable.
func (f *Facts) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("driver-facts")
	for {
		wait := f.opts.Interval
		if err := f.Load(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Error(err, "loading the regions and zones of the drivers failed")
			wait = retryInterval
		} else {
			logger.V(1).Info("loaded the regions and zones of the drivers")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-f.opts.Clock.After(wait):
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

// RootDiskBytes implements catalog.Facts.
func (f *Facts) RootDiskBytes(vendor string, _ *catalog.Class) (int64, bool) {
	no := f.nodeOptions(vendor)
	if no == nil {
		return 0, false
	}
	b := no.Defaults().RootDiskBytes
	return b, b > 0
}

// Supported implements catalog.Facts (labels.Supporter).
func (f *Facts) Supported(vendor, apiReference, zone string, defined labels.Profiles) map[labels.ProfileKind][]string {
	no := f.nodeOptions(vendor)
	if no == nil {
		return nil
	}
	profiles := vendors.Profiles{}
	for k, names := range defined {
		profiles[string(k)] = names
	}
	got := no.Supported(vendors.CatalogType{Vendor: vendor, APIReference: apiReference}, zone, profiles)
	if len(got) == 0 {
		return nil
	}
	out := map[labels.ProfileKind][]string{}
	for k, names := range got {
		out[labels.ProfileKind(k)] = names
	}
	return out
}

// PriceDelta implements catalog.Facts.
func (f *Facts) PriceDelta(vendor string, _ *catalog.Class, apiReference, zone, capacityType string) (float64, bool) {
	no := f.nodeOptions(vendor)
	if no == nil {
		return 0, true
	}
	off := vendors.Offering{Zone: zone, CapacityType: capacityType, APIReference: apiReference}
	return no.PriceDelta(no.Defaults(), off)
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
	if f.opts.Launchable != nil {
		// The two counters only grow, so their sum changes whenever one does.
		e += f.opts.Launchable.LaunchableEpoch()
	}
	return e
}

// LogFacts logs what was loaded, for the start-up log.
func (f *Facts) LogFacts(logger logr.Logger) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for vendor, vf := range f.data {
		logger.Info("driver facts", "vendor", vendor, "regions", len(vf.regions), "zones", len(vf.zones))
	}
}
