/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package hcloud

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/SpareCores/scarpenter/internal/version"
	operatoroptions "github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Environment variables of the driver: the project's API token and, for tests,
// the API endpoint.
const (
	TokenEnv    = "HCLOUD_TOKEN"
	EndpointEnv = "HCLOUD_ENDPOINT"
)

func init() {
	vendors.Register(Name, func() vendors.Driver { return New() })
}

// Option configures New.
type Option func(*Driver)

// WithAPI replaces the API (tests: the in-memory fake); without it Init builds
// the client from HCLOUD_TOKEN.
func WithAPI(a API) Option { return func(d *Driver) { d.api = a } }

// WithPrices replaces the price source (tests); without it Init reads Keeper's
// volume prices when a Keeper URL is configured.
func WithPrices(p PriceSource) Option { return func(d *Driver) { d.prices = p } }

// WithHCloudServerLimit sets the project's server limit (0: unknown), instead of
// --hcloud-server-limit.
func WithHCloudServerLimit(n int) Option {
	return func(d *Driver) { d.serverLimit, d.limitSet = n, true }
}

// WithRegisterer receives the rate-limit gauges (default: the controller's
// metrics registry).
func WithRegisterer(r prometheus.Registerer) Option { return func(d *Driver) { d.registerer = r } }

// Driver is the Hetzner Cloud driver: one client, one project, the project's
// locations as regions and zones (plan 15).
type Driver struct {
	cfg   vendors.Config
	clock clock.Clock
	log   logr.Logger

	api         API
	p           *Project
	prices      PriceSource
	keeper      *keeperPrices
	serverLimit int
	limitSet    bool
	registerer  prometheus.Registerer

	// epoch counts price loads (the server type table's changes are the
	// Project's).
	epoch atomic.Uint64

	mu       sync.Mutex
	images   map[imageKey]cachedImage
	sshKeys  map[string]int64
	warnedAt time.Time
}

// New returns an uninitialised driver.
func New(opts ...Option) *Driver {
	d := &Driver{images: map[imageKey]cachedImage{}, sshKeys: map[string]int64{}}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Vendor implements vendors.Driver.
func (d *Driver) Vendor() string { return Name }

// TagKey implements vendors.Driver: scarpenter.sparecores.com/<key>, a valid
// Hetzner label key.
func (d *Driver) TagKey(k vendors.TagKey) string { return TagKey(k) }

// Init implements vendors.Driver. It makes no API call.
func (d *Driver) Init(ctx context.Context, cfg vendors.Config) error {
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	if !d.limitSet {
		if o := operatoroptions.FromContext(ctx); o != nil {
			d.serverLimit = o.HCloudServerLimit
		}
	}
	if d.serverLimit < 0 {
		return fmt.Errorf("the Hetzner server limit must not be negative, got %d", d.serverLimit)
	}
	if d.registerer == nil {
		d.registerer = metrics.Registry
	}
	if d.api == nil {
		token := os.Getenv(TokenEnv)
		if token == "" {
			return fmt.Errorf("%s is not set: the hcloud driver needs the API token of the Hetzner Cloud project", TokenEnv)
		}
		d.api = NewClient(ClientConfig{
			Token: token, Endpoint: os.Getenv(EndpointEnv), Version: version.Version, Registry: d.registerer,
		})
	}
	d.p = NewProject(d.api, cfg.ClusterName, WithClock(d.clock), WithServerLimit(d.serverLimit))
	if d.prices == nil && cfg.KeeperURL != "" {
		ipv4 := DefaultIPv4PricePerHour
		if v := os.Getenv(IPv4PriceEnv); v != "" {
			p, err := strconv.ParseFloat(v, 64)
			if err != nil || p < 0 || math.IsNaN(p) || math.IsInf(p, 0) {
				return fmt.Errorf("%s=%q is not a price in USD per hour (a finite number, 0 for free)", IPv4PriceEnv, v)
			}
			ipv4 = p
		}
		d.log.Info("the price of a Primary IPv4 address is a constant, not from Keeper (it has none)",
			"usdPerHour", ipv4, "override", IPv4PriceEnv)
		d.keeper = newKeeperPrices(cfg.KeeperURL, ipv4, func() { d.epoch.Add(1) })
		d.prices = d.keeper
	}
	d.registerRateLimitGauges()
	return nil
}

// registerRateLimitGauges exposes the project's rate limit from the
// RateLimit-* headers of the last response (plan 15, "Rate limit").
func (d *Driver) registerRateLimitGauges() {
	gauge := func(name, help string, read func(RateLimit) float64) {
		g := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 {
			rl := d.api.RateLimit()
			if !rl.Known() {
				return math.NaN()
			}
			return read(rl)
		})
		if err := d.registerer.Register(g); err != nil {
			if are, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
				d.registerer.Unregister(are.ExistingCollector)
				_ = d.registerer.Register(g)
			}
		}
	}
	gauge("scarpenter_hcloud_api_remaining", "Requests left in the Hetzner Cloud project's rate limit bucket (RateLimit-Remaining).",
		func(r RateLimit) float64 { return float64(r.Remaining) })
	gauge("scarpenter_hcloud_api_limit", "The Hetzner Cloud project's rate limit per hour (RateLimit-Limit).",
		func(r RateLimit) float64 { return float64(r.Limit) })
}

// warnRateLimit logs a warning, at most every five minutes, when less than 20 %
// of the project's rate limit is left.
func (d *Driver) warnRateLimit() {
	rl := d.api.RateLimit()
	if !rl.LowRemaining() {
		return
	}
	d.mu.Lock()
	now := d.clock.Now()
	skip := !d.warnedAt.IsZero() && now.Sub(d.warnedAt) < 5*time.Minute
	if !skip {
		d.warnedAt = now
	}
	d.mu.Unlock()
	if !skip {
		d.log.Info("the Hetzner Cloud rate limit is running low", "remaining", rl.Remaining, "limit", rl.Limit)
	}
}

func (d *Driver) ready() error {
	if d.p == nil {
		return vendors.Errorf(vendors.Fatal, "the Hetzner Cloud driver is not initialised")
	}
	return nil
}

// Regions implements vendors.Driver: the project's locations.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	d.kickPrices()
	return d.p.Regions(ctx)
}

// Zones implements vendors.Driver: a location is its own single zone.
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.p.Zones(ctx, region)
}

func (d *Driver) kickPrices() {
	if d.keeper != nil {
		d.keeper.kick(d.clock.Now(), func(err error) { d.log.Error(err, "loading the volume prices from Keeper") })
	}
}

// LoadPrices loads the volume prices now (tests and start-up).
func (d *Driver) LoadPrices(ctx context.Context) error {
	if d.keeper == nil {
		return nil
	}
	return d.keeper.load(ctx)
}

// NodeOptionsEpoch implements vendors.EpochReporter: the epoch changes when the
// server type table or the volume prices load with other content.
func (d *Driver) NodeOptionsEpoch() uint64 {
	n := d.epoch.Load()
	if d.p != nil {
		n += d.p.Epoch()
	}
	return n
}

// LaunchableTypes implements vendors.OfferingLister.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.p.LaunchableTypes(ctx, region)
}

// Quotas implements vendors.QuotaReporter: Hetzner's limits are per project and
// not readable, so there are no regional buckets.
func (d *Driver) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.p.Quotas(ctx, region)
}

// GlobalQuotas implements vendors.QuotaReporter: the one global bucket of
// --hcloud-server-limit, none while it is 0 (unknown).
func (d *Driver) GlobalQuotas(ctx context.Context) (*vendors.QuotaSnapshot, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.p.GlobalQuotas(ctx)
}

// Consumption implements vendors.QuotaReporter.
func (d *Driver) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	if d.p == nil {
		return nil, false
	}
	return d.p.Consumption(instanceType, capacityType)
}

// BillingRule implements vendors.BillingDescriber (ADR-024): hourly, rounded up
// per started hour, and a powered-off server still bills until it is deleted.
// Verified stays false until the live check of plan 15 (periods count from the
// server's creation, not from the clock hour) has passed: retention is off for
// Hetzner until then and the NodeClass status says why.
func (d *Driver) BillingRule(string, string) (vendors.BillingRule, bool) {
	inc, charged := time.Hour, true
	return vendors.BillingRule{
		Increment: &inc, StoppedCharged: &charged, Verified: billingVerified,
		Source: "Hetzner Cloud billing FAQ: hourly, per started hour; a stopped server is still charged " +
			"(from-start periods and the billing start are not yet checked live)",
	}, true
}

// billingVerified is flipped by the live check of plan 15: periods count from
// the creation of the server and the billing start is its `created`.
const billingVerified = false

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
)
