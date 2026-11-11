/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// HomeRegionEnv overrides the region of the global DescribeRegions call (the
// default is DefaultHomeRegion). The driver reads its own variable instead of a
// controller flag: nothing outside this package branches on the vendor.
const HomeRegionEnv = "SCARPENTER_ALICLOUD_HOME_REGION"

// Cache lifetimes.
const (
	regionsTTL = time.Hour
	zonesTTL   = time.Hour
)

// vanishWindow is how long after a launch an instance that is gone counts as
// released by ECS for lack of stock (plan 21 section 3, Async): the NodeClaim
// registration timeout of core.
const vanishWindow = 15 * time.Minute

func init() {
	vendors.Register(Vendor, func() vendors.Driver { return New() })
}

// Option configures a Driver (used by the tests and by wiring).
type Option func(*Driver)

// WithClientFactory replaces the SDK clients, for the tests.
func WithClientFactory(f ClientFactory) Option { return func(d *Driver) { d.factory = f } }

// WithHomeRegion sets the region of the global DescribeRegions call.
func WithHomeRegion(r string) Option { return func(d *Driver) { d.home = r } }

// WithSleep replaces the wait between the retries of a call, for the tests.
func WithSleep(f func(ctx context.Context, d time.Duration) error) Option {
	return func(d *Driver) { d.sleep = f }
}

// WithPrices sets the source of the disk prices; by default the driver reads
// them from Keeper.
func WithPrices(p PriceSource) Option { return func(d *Driver) { d.prices = p } }

// Driver implements vendors.Driver and the capabilities NodeClassValidator,
// ImageReporter, NodeOptions, EpochReporter, ManagedResources, ResourceDefaults,
// ZoneChecker, ArchNamer, QuotaReporter, OfferingLister, BillingDescriber and
// NodeHardener.
type Driver struct {
	cfg     vendors.Config
	clock   clock.Clock
	log     logr.Logger
	factory ClientFactory
	home    string
	sleep   func(ctx context.Context, d time.Duration) error
	prices  PriceSource
	keeper  *keeperPrices

	cloud  *Cloud
	quotas *Quotas

	// epoch changes when the answers of Supported and PriceDelta may have.
	epoch uint64

	mu       sync.Mutex
	regions  cached[[]vendors.Region]
	zones    map[string]*cached[[]vendors.Zone]
	images   map[imageKey]cachedImage
	nvmeOf   map[string]bool
	vsw      map[string]cachedVSwitch
	sgVPC    map[string]cachedSG
	launched map[vendors.InstanceRef]time.Time
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
	d := &Driver{
		zones: map[string]*cached[[]vendors.Zone]{}, images: map[imageKey]cachedImage{}, nvmeOf: map[string]bool{},
		vsw: map[string]cachedVSwitch{}, sgVPC: map[string]cachedSG{}, launched: map[vendors.InstanceRef]time.Time{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Vendor implements vendors.Driver.
func (d *Driver) Vendor() string { return Vendor }

// Init implements vendors.Driver. It builds the client layer from the default
// credential chain of credentials-go but makes no API call: the credentials
// resolve with the first call, so an unreachable Alibaba Cloud does not keep the
// controller from starting.
func (d *Driver) Init(_ context.Context, cfg vendors.Config) error {
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	if d.home == "" {
		d.home = DefaultHomeRegion
		if v := os.Getenv(HomeRegionEnv); v != "" {
			d.home = v
		}
	}
	if d.factory == nil {
		d.factory = NewClientFactory(ClientOptions{UserAgent: "scarpenter/" + version.Version})
	}
	d.cloud = NewCloud(cfg.ClusterName, d.factory, d.clock)
	if d.sleep != nil {
		d.cloud.sleep = d.sleep
	}
	d.quotas = NewQuotas(d.cloud)
	if d.prices == nil && cfg.KeeperURL != "" {
		d.keeper = newKeeperPrices(cfg.KeeperURL, func() { d.bumpEpoch() })
		d.prices = d.keeper
	}
	return nil
}

func (d *Driver) bumpEpoch() {
	d.mu.Lock()
	d.epoch++
	d.mu.Unlock()
}

// NodeOptionsEpoch implements vendors.EpochReporter: Supported and PriceDelta
// depend on the instance type table and the prices, which load at runtime.
func (d *Driver) NodeOptionsEpoch() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.epoch
}

// TagKey implements vendors.Driver: scarpenter.sparecores.com/<key> (ECS accepts
// slash keys).
func (d *Driver) TagKey(k vendors.TagKey) string { return TagKey(k) }

// Regions implements vendors.Driver: the regions of the account, read from the
// home region and cached for an hour. A region the account cannot use is
// reported by Zones (RegionUnavailable), not here.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	d.mu.Lock()
	if v, ok := d.regions.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return slices.Clone(v), nil
	}
	d.mu.Unlock()
	regions, err := d.cloud.Regions(ctx, d.home, nil)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.regions.set(regions, d.clock.Now().Add(regionsTTL))
	d.mu.Unlock()
	d.kickPrices()
	return slices.Clone(regions), nil
}

// Zones implements vendors.Driver: the zones of the region, cached for an hour;
// the zone ID and the zone name are the same text (ap-southeast-1a).
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	d.mu.Lock()
	if c := d.zones[region]; c != nil {
		if v, ok := c.get(d.clock.Now()); ok {
			d.mu.Unlock()
			return slices.Clone(v), nil
		}
	}
	d.mu.Unlock()
	zones, err := d.cloud.Zones(ctx, region)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	c := d.zones[region]
	if c == nil {
		c = &cached[[]vendors.Zone]{}
		d.zones[region] = c
	}
	c.set(zones, d.clock.Now().Add(zonesTTL))
	d.mu.Unlock()
	return slices.Clone(zones), nil
}

// kickPrices starts the background load of the disk prices.
func (d *Driver) kickPrices() {
	if d.keeper != nil {
		d.keeper.kick(d.clock.Now(), func(err error) { d.log.Error(err, "loading the disk prices from Keeper") })
	}
}

// LoadPrices reads the disk prices from Keeper now and waits for the result; the
// driver otherwise loads them in the background from the first read of the
// regions. A no-op without a Keeper URL.
func (d *Driver) LoadPrices(ctx context.Context) error {
	if d.keeper == nil {
		return nil
	}
	return d.keeper.load(ctx)
}

// Cloud returns the client layer, for the sweeper CLI and the tests.
func (d *Driver) Cloud() *Cloud { return d.cloud }

// ---- instances ----------------------------------------------------------------

// CreateInstance implements vendors.Driver: the resources Prepare resolved (the
// vSwitch, the groups, the deployment set), then Cloud.CreateInstance, which
// builds the one RunInstances call and resolves replays through the NodeClaim
// tag. A launch the request cannot satisfy for the image (an NVMe type with an
// image that lacks NVMe support) is refused before the call.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	lr, err := d.launchResources(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := d.checkImageFits(req); err != nil {
		return nil, err
	}
	inst, err := d.cloud.CreateInstance(ctx, req, lr)
	if err != nil {
		if vendors.CodeOf(err) == "QuotaExceeded.PrivateIpAddress" {
			d.forgetVSwitch(lr.VSwitchID)
		}
		return nil, err
	}
	d.mu.Lock()
	d.launched[inst.Ref] = d.clock.Now()
	d.pruneLaunched()
	d.mu.Unlock()
	return inst, nil
}

// pruneLaunched drops the launches older than the vanish window (mu held).
func (d *Driver) pruneLaunched() {
	now := d.clock.Now()
	for ref, at := range d.launched {
		if now.Sub(at) > vanishWindow {
			delete(d.launched, ref)
		}
	}
}

// GetInstance implements vendors.Driver. An instance that this process launched
// and that is gone within the vanish window was released by ECS right after it
// accepted the call (SystemFailure.Delete, a stock shortage): the first look says
// InsufficientCapacity (code InstanceVanished), the next NotFound.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	if !d.knownRegion(ctx, ref.Region()) {
		return nil, notFound(ref.String())
	}
	inst, err := d.cloud.GetInstance(ctx, ref)
	if err == nil || !vendors.IsClass(err, vendors.NotFound) {
		return inst, err
	}
	d.mu.Lock()
	at, ok := d.launched[ref]
	delete(d.launched, ref)
	d.mu.Unlock()
	if ok {
		if cerr := d.cloud.CheckLaunched(ctx, ref, at, vanishWindow); cerr != nil {
			return nil, cerr
		}
	}
	return nil, err
}

// FindInstance implements vendors.Driver.
func (d *Driver) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	if !d.knownRegion(ctx, region) {
		return nil, notFound("in the region " + region)
	}
	return d.cloud.FindInstance(ctx, region, tags)
}

// ListInstances implements vendors.Driver.
func (d *Driver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	var known []string
	for _, r := range regions {
		if d.knownRegion(ctx, r) {
			known = append(known, r)
		}
	}
	return d.cloud.ListInstances(ctx, known, tags)
}

// DeleteInstance implements vendors.Driver.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	if !d.knownRegion(ctx, ref.Region()) {
		return nil // a region the account cannot use holds none of its instances
	}
	d.mu.Lock()
	delete(d.launched, ref)
	d.mu.Unlock()
	return d.cloud.DeleteInstance(ctx, ref)
}

// knownRegion says whether the account can use the region (DescribeRegions lists
// it); a failure to list is taken as known, so that a transient error surfaces
// from the call itself. A reference to another region is a stale or malformed
// one: its instance does not exist.
func (d *Driver) knownRegion(ctx context.Context, region string) bool {
	if region == "" {
		return false
	}
	regions, err := d.Regions(ctx)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == region })
}

// ---- quotas and offerings ------------------------------------------------------

// Consumption implements vendors.QuotaReporter.
func (d *Driver) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	return d.quotas.Consumption(instanceType, capacityType)
}

// Quotas implements vendors.QuotaReporter.
func (d *Driver) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	return d.quotas.Quotas(ctx, region)
}

// GlobalQuotas implements vendors.QuotaReporter.
func (d *Driver) GlobalQuotas(ctx context.Context) (*vendors.QuotaSnapshot, error) {
	return d.quotas.GlobalQuotas(ctx)
}

// LaunchableTypes implements vendors.OfferingLister: the types DescribeAvailableResource
// reports with stock in a zone, for either capacity type (a type that has stock
// as spot only is still offered; the launch error settles the rest), minus the
// families the driver never launches. Reading the offerings also reads the
// vCPUs of the offered types, which the quota consumption needs.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, capacity := range []string{vendors.CapacityOnDemand, vendors.CapacitySpot} {
		types, err := d.cloud.LaunchableTypes(ctx, region, capacity)
		if err != nil {
			if capacity == vendors.CapacitySpot && vendors.IsClass(err, vendors.Unsupported) {
				continue // the account cannot buy spot here: on-demand stock is the answer
			}
			return nil, err
		}
		for typ, zones := range types {
			for _, z := range zones {
				if !slices.Contains(out[typ], z) {
					out[typ] = append(out[typ], z)
				}
			}
		}
	}
	names := make([]string, 0, len(out))
	for typ, zones := range out {
		slices.Sort(zones)
		names = append(names, typ)
	}
	slices.Sort(names)
	before := d.cloud.Types.Len()
	if err := d.cloud.LoadTypes(ctx, region, names); err != nil {
		return nil, fmt.Errorf("reading the instance types of %s: %w", region, err)
	}
	if d.cloud.Types.Len() != before {
		d.bumpEpoch()
	}
	return out, nil
}

// ---- billing --------------------------------------------------------------------

// BillingRule implements vendors.BillingDescriber. ECS pay-as-you-go instances
// are billed per second and a short-lived instance pays a minimum of 10, 5 or 2
// minutes by its vCPU count (the pay-as-you-go page of the vendor, read
// 2026-11-10). The rule is not Verified, so billing retention stays off for
// Alibaba Cloud and the NodeClass status says why: no live check has confirmed
// the minimum, which cycle it applies to (the instance's life or the last
// hourly settlement) or the billing start, and a rule per region and family
// cannot express a minimum that depends on the vCPU count of the type (plan 21,
// section 3 and the live check described there).
func (d *Driver) BillingRule(string, string) (vendors.BillingRule, bool) {
	inc := time.Second
	return vendors.BillingRule{
		Increment: &inc,
		Source: "per second with a minimum of 10, 5 or 2 minutes by vCPU count (pay-as-you-go page, 2026-11-10); " +
			"unverified: plan 21 step 0 settles the cycle and the vCPU thresholds",
	}, true
}

// ---- small helpers -----------------------------------------------------------------

func compare(a, b string) int { return compareStr(a, b) }

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

var (
	_ vendors.Driver             = (*Driver)(nil)
	_ vendors.NodeClassValidator = (*Driver)(nil)
	_ vendors.ImageReporter      = (*Driver)(nil)
	_ vendors.NodeOptions        = (*Driver)(nil)
	_ vendors.EpochReporter      = (*Driver)(nil)
	_ vendors.ManagedResources   = (*Driver)(nil)
	_ vendors.ResourceDefaults   = (*Driver)(nil)
	_ vendors.DefaultChecker     = (*Driver)(nil)
	_ vendors.ZoneChecker        = (*Driver)(nil)
	_ vendors.ArchNamer          = (*Driver)(nil)
	_ vendors.QuotaReporter      = (*Driver)(nil)
	_ vendors.OfferingLister     = (*Driver)(nil)
	_ vendors.BillingDescriber   = (*Driver)(nil)
	_ vendors.NodeHardener       = (*Driver)(nil)
)
