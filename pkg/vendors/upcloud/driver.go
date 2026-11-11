/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// MaxUserData is the budget the bootstrap renderer holds the user-data to: 16
// KiB, as on AWS. UpCloud's own limit is unverified (spike S3); the default
// templates are about 5 KiB.
const MaxUserData = 16 * 1024

// NetworkRangeEnv overrides the /16 inside which SCarpenter's managed networks
// take their /24 (default DefaultNetworkRange). Networks of an account must not
// overlap, so the range must stay clear of the networks the operator brings.
const NetworkRangeEnv = "SCARPENTER_UPCLOUD_NETWORK_RANGE"

// DefaultNetworkRange is the default /16 of the managed networks (the managed
// AWS VPC is 10.240.0.0/16).
const DefaultNetworkRange = "10.241.0.0/16"

// Cache lifetimes.
const (
	regionsTTL = time.Hour
	plansTTL   = time.Hour
)

func init() {
	vendors.Register(Vendor, func() vendors.Driver { return New() })
}

// Option configures a Driver (used by the tests and by wiring).
type Option func(*Driver)

// WithAPI replaces the API client, for the tests (the fake) and for tools that
// bring their own (the canary's read-only guard).
func WithAPI(a api.API) Option { return func(d *Driver) { d.api = a } }

// WithPrices sets the source of the storage and IPv4 prices; by default the
// driver reads them from Keeper.
func WithPrices(p PriceSource) Option { return func(d *Driver) { d.prices = p } }

// WithNetworkRange sets the /16 of the managed networks, as NetworkRangeEnv does.
func WithNetworkRange(cidr string) Option { return func(d *Driver) { d.networkRange = cidr } }

// Driver implements vendors.Driver and the capabilities NodeClassValidator,
// ImageReporter, NodeOptions, EpochReporter, ManagedResources, QuotaReporter,
// OfferingLister, BillingDescriber, ArchNamer and NodeHardener.
type Driver struct {
	cfg          vendors.Config
	clock        clock.Clock
	log          logr.Logger
	api          api.API
	prices       PriceSource
	keeper       *keeperPrices
	networkRange string

	// epoch changes when the answers of Supported and PriceDelta may have.
	epoch atomic.Uint64

	mu      sync.Mutex
	regions cached[[]vendors.Region]
	plans   cached[Plans]
	// templates are the Flatcar templates the last resolution of each zone
	// matched (all tiers), for the tier choice of CreateInstance and Supported.
	templates map[string][]Template
	// limits are the account limits of the quota reader, reused for 15 minutes.
	limits   cached[limitsSnapshot]
	userSeen map[string]bool
}

// cached is a value with an expiry.
type cached[T any] struct {
	v     T
	until time.Time
	ok    bool
}

func (c *cached[T]) get(now time.Time) (T, bool) {
	if c.ok && now.Before(c.until) {
		return c.v, true
	}
	var zero T
	return zero, false
}

func (c *cached[T]) set(v T, until time.Time) { c.v, c.until, c.ok = v, until, true }

// New returns an uninitialised driver.
func New(opts ...Option) *Driver {
	d := &Driver{templates: map[string][]Template{}, userSeen: map[string]bool{}}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Vendor implements vendors.Driver.
func (d *Driver) Vendor() string { return Vendor }

// Init implements vendors.Driver. It builds the client from UPCLOUD_TOKEN but
// makes no API call, so an unreachable UpCloud does not keep the controller from
// starting; one client serves every zone.
func (d *Driver) Init(_ context.Context, cfg vendors.Config) error {
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	if d.api == nil {
		c, err := api.NewFromEnv("scarpenter/" + version.Version)
		if err != nil {
			return fmt.Errorf("creating the UpCloud client: %w", err)
		}
		d.api = c
	}
	if d.networkRange == "" {
		d.networkRange = os.Getenv(NetworkRangeEnv)
	}
	if d.networkRange == "" {
		d.networkRange = DefaultNetworkRange
	}
	if err := checkRange(d.networkRange); err != nil {
		return fmt.Errorf("%s: %w", NetworkRangeEnv, err)
	}
	if d.prices == nil && cfg.KeeperURL != "" {
		d.keeper = newKeeperPrices(cfg.KeeperURL, func() { d.epoch.Add(1) })
		d.prices = d.keeper
	}
	return nil
}

// NodeOptionsEpoch implements vendors.EpochReporter: Supported and PriceDelta
// depend on the plan table, the templates found and the prices, which load at
// runtime.
func (d *Driver) NodeOptionsEpoch() uint64 { return d.epoch.Load() }

// TagKey implements vendors.Driver: scarpenter-<key> (label keys are 2 to 32
// characters, and scarpenter.sparecores.com/idempotency-key has 33).
func (d *Driver) TagKey(k vendors.TagKey) string { return TagKey(k) }

// Regions implements vendors.Driver: the zones of the account, which are the
// regions (GET /zone lists the public ones and the account's private ones), cached
// for an hour.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	d.mu.Lock()
	if v, ok := d.regions.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return slices.Clone(v), nil
	}
	d.mu.Unlock()
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	zs, err := d.api.GetZones(cctx)
	if err != nil {
		return nil, Classify(err)
	}
	regions := make([]vendors.Region, 0, len(zs.Zones))
	for _, z := range zs.Zones {
		if z.ID != "" {
			regions = append(regions, vendors.Region{Name: z.ID})
		}
	}
	slices.SortFunc(regions, func(a, b vendors.Region) int { return strings.Compare(a.Name, b.Name) })
	d.mu.Lock()
	d.regions.set(regions, d.clock.Now().Add(regionsTTL))
	d.mu.Unlock()
	d.kickPrices()
	return slices.Clone(regions), nil
}

// Zones implements vendors.Driver: one zone whose ID and name are the region
// (Keeper's dummy zone). A region the account cannot use is RegionUnavailable.
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	if !d.knownRegion(ctx, region) {
		return nil, vendors.Errorf(vendors.RegionUnavailable, "the account cannot use the zone %q", region)
	}
	return []vendors.Zone{Zone(region)}, nil
}

// knownRegion says whether the account can use the region; a failure to list is
// taken as known, so that a transient error surfaces from the call itself.
func (d *Driver) knownRegion(ctx context.Context, region string) bool {
	if strings.TrimSpace(region) == "" {
		return false
	}
	regions, err := d.Regions(ctx)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == region })
}

// planTable returns the account's plans (GET /plan), cached for an hour. A
// failure with a stale table serves the table.
func (d *Driver) planTable(ctx context.Context) (Plans, error) {
	d.mu.Lock()
	if v, ok := d.plans.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return v, nil
	}
	stale := d.plans
	d.mu.Unlock()
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	ps, err := d.api.GetPlans(cctx)
	if err != nil {
		if stale.ok {
			return stale.v, nil
		}
		return nil, Classify(err)
	}
	plans := PlansOf(ps)
	d.mu.Lock()
	first := !d.plans.ok
	d.plans.set(plans, d.clock.Now().Add(plansTTL))
	d.mu.Unlock()
	if first {
		d.epoch.Add(1)
	}
	return plans, nil
}

// knownPlans returns the plan table without a call: nil until it was read.
func (d *Driver) knownPlans() Plans {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.plans.ok {
		return d.plans.v
	}
	return nil
}

// kickPrices starts the background load of the prices from Keeper.
func (d *Driver) kickPrices() {
	if d.keeper != nil {
		d.keeper.kick(d.clock.Now(), func(err error) { d.log.Error(err, "loading the prices from Keeper") })
	}
}

// LoadPrices reads the prices from Keeper now and waits for the result; the
// driver otherwise loads them in the background from the first read of the
// regions. A no-op without a Keeper URL.
func (d *Driver) LoadPrices(ctx context.Context) error {
	if d.keeper == nil {
		return nil
	}
	return d.keeper.load(ctx)
}

// warnOnce logs a message once per key.
func (d *Driver) warnOnce(key, msg string, kv ...any) {
	d.mu.Lock()
	seen := d.userSeen[key]
	d.userSeen[key] = true
	d.mu.Unlock()
	if !seen {
		d.log.Info(msg, kv...)
	}
}

var (
	_ vendors.Driver             = (*Driver)(nil)
	_ vendors.NodeClassValidator = (*Driver)(nil)
	_ vendors.ImageReporter      = (*Driver)(nil)
	_ vendors.NodeOptions        = (*Driver)(nil)
	_ vendors.EpochReporter      = (*Driver)(nil)
	_ vendors.ManagedResources   = (*Driver)(nil)
	_ vendors.QuotaReporter      = (*Driver)(nil)
	_ vendors.OfferingLister     = (*Driver)(nil)
	_ vendors.BillingDescriber   = (*Driver)(nil)
	_ vendors.ArchNamer          = (*Driver)(nil)
	_ vendors.NodeHardener       = (*Driver)(nil)
)
