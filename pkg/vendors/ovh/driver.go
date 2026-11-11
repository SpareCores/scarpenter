/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package ovh is the OVHcloud Public Cloud driver (plan 18): a driver for the
// OpenStack APIs of one Public Cloud project (Keystone's catalog, Nova, Neutron,
// Glance, Cinder, through gophercloud), with OVH's region and flavor rules.
// Instances are Nova servers on the public Ext-Net network, found again by the
// idempotency key in their metadata; the Flatcar image is one the operator
// uploaded to Glance per region (rung 3 of the image ladder, ADR-021); the
// managed resources are Neutron security groups and Nova server groups. Every
// call goes through the narrow osapi interfaces, which the in-memory cloud of
// pkg/vendors/ovh/fake implements for the tests.
package ovh

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
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi/sdk"
)

// Name is the registration name and the Keeper vendor ID of the driver.
const Name = Vendor

// Environment variables of the driver itself. The credentials are the standard
// OpenStack ones (OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, OS_PROJECT_ID,
// OS_USER_DOMAIN_NAME): the `openstackUser` mode of plan 18.
const (
	// ScheduleWaitEnv is how long a create waits for the scheduler (a Go
	// duration; default 45s).
	ScheduleWaitEnv = "SCARPENTER_OVH_SCHEDULE_WAIT"
	// RegionAliasesEnv is the region alias table's runtime rows: comma-separated
	// KEEPER=API pairs ("GRA11=GRA11-NEW"), an empty API name leaves the Keeper
	// region unmapped.
	RegionAliasesEnv = "SCARPENTER_OVH_REGION_ALIASES"
)

// Cache lifetimes (plan 18, "Driver").
const (
	regionsTTL = time.Hour
	zonesTTL   = time.Hour
	extNetTTL  = time.Hour
	imageTTL   = time.Hour
	// imageMissTTL is how long "no such image" is remembered, so that a region
	// without the image is not asked every reconcile.
	imageMissTTL = 5 * time.Minute
)

func init() {
	vendors.Register(Name, func() vendors.Driver { return New() })
}

// Option configures a Driver (used by the tests and by wiring).
type Option func(*Driver)

// WithProvider replaces the OpenStack provider (the tests give the in-memory
// cloud); by default Init uses the standard OS_* environment.
func WithProvider(p osapi.Provider) Option { return func(d *Driver) { d.provider = p } }

// WithRegionAliases adds rows to the region alias table (Keeper name to API
// name; "" leaves the Keeper region unmapped).
func WithRegionAliases(m map[string]string) Option { return func(d *Driver) { d.aliases = m } }

// WithScheduleWait sets the wait for the scheduler of a create.
func WithScheduleWait(w WaitOptions) Option { return func(d *Driver) { d.wait = w } }

// WithCallTimeout sets the deadline of one OpenStack call.
func WithCallTimeout(t time.Duration) Option { return func(d *Driver) { d.callTimeout = t } }

// WithPrices sets the source of the volume prices; by default the driver reads
// them from Keeper.
func WithPrices(p PriceSource) Option { return func(d *Driver) { d.prices = p } }

// Driver implements vendors.Driver and the capabilities NodeClassValidator,
// ImageReporter, NodeOptions, EpochReporter, ManagedResources, QuotaReporter,
// OfferingLister, BillingDescriber, ArchNamer and NodeHardener.
type Driver struct {
	cfg         vendors.Config
	clock       clock.Clock
	log         logr.Logger
	provider    osapi.Provider
	aliases     map[string]string
	regionMap   *RegionMap
	wait        WaitOptions
	callTimeout time.Duration
	prices      PriceSource
	keeper      *keeperPrices
	quota       *QuotaReader

	// epoch changes when the answers of Supported and PriceDelta may have.
	epoch atomic.Uint64

	mu         sync.Mutex
	cl         map[string]osapi.Clients
	regions    cached[[]vendors.Region]
	zones      map[string]*cached[[]vendors.Zone]
	flavors    map[string]*cached[map[string]osapi.Flavor]
	extNet     map[string]*cached[string]
	images     map[imageKey]cachedImage
	unknownLog map[string]bool
	// deletingSince is the first sighting of a server in task state deleting
	// (region/ID), for stuckDeleting.
	deletingSince map[string]time.Time
}

// cached is a value with an expiry.
type cached[T any] struct {
	v     T
	until time.Time
	ok    bool
}

func (c *cached[T]) get(now time.Time) (T, bool) {
	if c != nil && c.ok && now.Before(c.until) {
		return c.v, true
	}
	var zero T
	return zero, false
}

func (c *cached[T]) set(v T, until time.Time) { c.v, c.until, c.ok = v, until, true }

// New returns an uninitialised driver.
func New(opts ...Option) *Driver {
	d := &Driver{
		callTimeout: DefaultCallTimeout, cl: map[string]osapi.Clients{}, zones: map[string]*cached[[]vendors.Zone]{},
		flavors: map[string]*cached[map[string]osapi.Flavor]{}, extNet: map[string]*cached[string]{},
		images: map[imageKey]cachedImage{}, unknownLog: map[string]bool{}, deletingSince: map[string]time.Time{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Vendor implements vendors.Driver.
func (d *Driver) Vendor() string { return Name }

// Init implements vendors.Driver. It checks that the credentials are in the
// environment but makes no API call: the authentication happens with the first
// call, so an unreachable OVH does not keep the controller from starting.
func (d *Driver) Init(_ context.Context, cfg vendors.Config) error {
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	if d.aliases == nil {
		a, err := aliasesFromEnv(os.Getenv(RegionAliasesEnv))
		if err != nil {
			return err
		}
		d.aliases = a
	}
	m, err := NewRegionMap(d.aliases)
	if err != nil {
		return fmt.Errorf("the OVH region aliases: %w", err)
	}
	d.regionMap = m
	if d.wait == (WaitOptions{}) {
		if v := os.Getenv(ScheduleWaitEnv); v != "" {
			w, err := time.ParseDuration(v)
			if err != nil || w <= 0 {
				return fmt.Errorf("%s=%q is not a positive duration", ScheduleWaitEnv, v)
			}
			d.wait.Timeout = w
		}
	}
	if d.provider == nil {
		if err := sdk.EnvConfigured(); err != nil {
			return fmt.Errorf("the OVH driver needs OpenStack credentials in the environment "+
				"(OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, OS_PROJECT_ID, OS_USER_DOMAIN_NAME): %w", err)
		}
		d.provider = &lazyProvider{}
	}
	d.quota = NewQuotaReader(d.provider, d.clock)
	if d.prices == nil && cfg.KeeperURL != "" {
		d.keeper = newKeeperPrices(cfg.KeeperURL, func() { d.epoch.Add(1) })
		d.prices = d.keeper
	}
	return nil
}

// aliasesFromEnv parses KEEPER=API pairs.
func aliasesFromEnv(v string) (map[string]string, error) {
	out := map[string]string{}
	for pair := range strings.SplitSeq(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, api, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("%s: %q is not KEEPER=API", RegionAliasesEnv, pair)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(api)
	}
	return out, nil
}

// lazyProvider authenticates with the first call that needs the cloud and
// keeps the provider; a failed authentication is retried by the next call.
type lazyProvider struct {
	mu sync.Mutex
	p  osapi.Provider
}

func (l *lazyProvider) get(ctx context.Context) (osapi.Provider, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.p != nil {
		return l.p, nil
	}
	p, err := sdk.FromEnv(ctx, sdk.Options{UserAgent: "scarpenter/" + version.Version})
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, CodeUnauthorized, fmt.Errorf("authenticating to OpenStack: %w", err))
	}
	l.p = p
	return p, nil
}

// Regions implements osapi.Provider.
func (l *lazyProvider) Regions(ctx context.Context) ([]string, error) {
	p, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return p.Regions(ctx)
}

// Clients implements osapi.Provider.
func (l *lazyProvider) Clients(ctx context.Context, region string) (osapi.Clients, error) {
	p, err := l.get(ctx)
	if err != nil {
		return osapi.Clients{}, err
	}
	return p.Clients(ctx, region)
}

// clients returns the (lazily created) clients of an API region, with a deadline
// per call. A region the catalog has no endpoint for is RegionUnavailable.
func (d *Driver) clients(ctx context.Context, region string) (osapi.Clients, error) {
	if region == "" {
		return osapi.Clients{}, vendors.Errorf(vendors.Fatal, "no OVH region given")
	}
	d.mu.Lock()
	if c, ok := d.cl[region]; ok {
		d.mu.Unlock()
		return c, nil
	}
	d.mu.Unlock()
	if d.provider == nil {
		return osapi.Clients{}, vendors.Errorf(vendors.Fatal, "the OVH driver is not initialised")
	}
	c, err := d.provider.Clients(ctx, region)
	if err != nil {
		return osapi.Clients{}, Classify(err)
	}
	c = withTimeouts(c, d.callTimeout)
	d.mu.Lock()
	d.cl[region] = c
	d.mu.Unlock()
	return c, nil
}

// regionGone reports that the error says the region is not in the catalog: for
// an instance or resource of that region it means "does not exist".
func regionGone(err error) bool {
	return vendors.IsClass(err, vendors.RegionUnavailable) && vendors.CodeOf(err) == CodeNoEndpoint
}

// TagKey implements vendors.Driver: scarpenter-<key>, Nova's metadata charset.
func (d *Driver) TagKey(k vendors.TagKey) string { return TagKey(k) }

// Regions implements vendors.Driver: the compute regions of the project's
// Keystone catalog that the region table maps (plan 14 A8), by their API names,
// cached for an hour. A catalog region without a table row is left out and
// logged once: it has no offerings or prices.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	d.mu.Lock()
	if v, ok := d.regions.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return slices.Clone(v), nil
	}
	d.mu.Unlock()
	if d.provider == nil {
		return nil, vendors.Errorf(vendors.Fatal, "the OVH driver is not initialised")
	}
	regions, unknown, err := CatalogRegions(ctx, d.provider, d.regionMap)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(regions, func(a, b vendors.Region) int { return strings.Compare(a.Name, b.Name) })
	d.mu.Lock()
	d.regions.set(regions, d.clock.Now().Add(regionsTTL))
	var fresh []string
	for _, u := range unknown {
		if !d.unknownLog[u] {
			d.unknownLog[u], fresh = true, append(fresh, u)
		}
	}
	d.mu.Unlock()
	if len(fresh) > 0 {
		d.log.Info("regions of the catalog without a row in the region table are not used (they have no Keeper offerings)",
			"regions", fresh, "hint", RegionAliasesEnv)
	}
	d.kickPrices()
	return slices.Clone(regions), nil
}

// Zones implements vendors.Driver: Nova's availability zones of the region,
// mapped to Keeper's zone IDs (ZoneIDs): the one dummy zone of a 1-AZ region,
// -a to -c of the 3-AZ ones. The zone's Name is Nova's.
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	d.mu.Lock()
	if v, ok := d.zones[region].get(d.clock.Now()); ok {
		d.mu.Unlock()
		return slices.Clone(v), nil
	}
	d.mu.Unlock()
	keeper, ok := d.regionMap.Keeper(region)
	if !ok {
		return nil, vendors.NewError(vendors.RegionUnavailable, CodeNoEndpoint,
			fmt.Errorf("%s is not a region of the region table", region))
	}
	cl, err := d.clients(ctx, region)
	if err != nil {
		return nil, err
	}
	azs, err := cl.Compute.ListAvailabilityZones(ctx)
	if err != nil {
		return nil, Classify(err)
	}
	var names []string
	for _, z := range azs {
		if z.Available {
			names = append(names, z.Name)
		}
	}
	zones := ZoneIDs(keeper, names)
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

// zoneOf is the zone name of a server: Nova's, or the region's only zone when
// Nova says none (a create without availability_zone in a 1-AZ region).
func (d *Driver) zoneOf(ctx context.Context, region string, s osapi.Server) string {
	if s.AvailabilityZone != "" {
		return s.AvailabilityZone
	}
	zs, err := d.Zones(ctx, region)
	if err != nil || len(zs) != 1 {
		return ""
	}
	return zs[0].Name
}

// NodeOptionsEpoch implements vendors.EpochReporter: PriceDelta depends on the
// volume prices, which load at runtime.
func (d *Driver) NodeOptionsEpoch() uint64 { return d.epoch.Load() }

// kickPrices starts the background load of the volume prices.
func (d *Driver) kickPrices() {
	if d.keeper != nil {
		d.keeper.kick(d.clock.Now(), func(err error) { d.log.Error(err, "loading the volume prices from Keeper") })
	}
}

// LoadPrices reads the volume prices from Keeper now and waits for the result;
// the driver otherwise loads them in the background from the first read of the
// regions. A no-op without a Keeper URL.
func (d *Driver) LoadPrices(ctx context.Context) error {
	if d.keeper == nil {
		return nil
	}
	return d.keeper.load(ctx)
}

// VendorArch implements vendors.ArchNamer: the value of the Glance image property
// architecture.
func (d *Driver) VendorArch(arch string) (string, bool) {
	switch arch {
	case archAMD64:
		return "x86_64", true
	case archARM64:
		return "aarch64", true
	}
	return "", false
}

// MetadataAddress is the address of the OpenStack metadata service, where the
// node's user data (a bootstrap token, the Tailscale key) is readable from Pods:
// OpenStack has nothing like AWS's hop limit.
const MetadataAddress = "169.254.169.254"

// Hardenings implements vendors.NodeHardener: metadata-from-pods, opt-in per
// NodeClass (spec.bootstrap.hardening).
func (d *Driver) Hardenings() []string { return []string{vendors.HardeningMetadataFromPods} }

// BootFragments implements vendors.NodeHardener: the unit that drops Pod traffic
// forwarded to the metadata address.
func (d *Driver) BootFragments(req vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	var out []vendors.BootFragment
	if slices.Contains(req.Hardening, vendors.HardeningMetadataFromPods) {
		f, err := vendors.BlockMetadataFromPods(MetadataAddress)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

var (
	_ vendors.NodeHardener       = (*Driver)(nil)
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
