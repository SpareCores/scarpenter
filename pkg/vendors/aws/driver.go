/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package aws is the AWS driver (overview section 11): instances through
// RunInstances without launch templates, regions and zones of the account
// (Keeper zone IDs mapped to the account's zone names), Flatcar images by name,
// vendor-native node settings (EBS volumes, placement groups, ENA Express),
// managed resources (subnets with their VPC, security groups, placement groups)
// and quota-aware placement from Service Quotas and the usage computed from
// DescribeInstances. Every API call goes through the narrow EC2API and
// ServiceQuotasAPI interfaces, which the in-memory fake in pkg/vendors/aws/fake
// implements for the tests.
package aws

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/smithy-go/middleware"
	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/internal/version"
	operatoroptions "github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Name is the registration name and the Keeper vendor ID of the driver.
const Name = "aws"

// MaxUserData is EC2's limit of raw (not yet base64-encoded) user-data: 16 KiB,
// the budget the bootstrap renderer holds the user-data to.
const MaxUserData = 16 * 1024

// IPv4PriceEnv overrides the price of a public IPv4 address (USD per hour).
const IPv4PriceEnv = "SCARPENTER_AWS_IPV4_PRICE"

// Cache lifetimes (overview sections 11.1 and 11.2).
const (
	regionsTTL = time.Hour
	zonesTTL   = time.Hour
	// logical names of the default home region and the SDK's retry mode.
	defaultHomeRegion = "us-east-1"
)

func init() {
	vendors.Register(Name, func() vendors.Driver { return New() })
}

// Option configures a Driver (used by the tests and by wiring).
type Option func(*Driver)

// WithClientFactory replaces the SDK clients, for the tests.
func WithClientFactory(f ClientFactory) Option { return func(d *Driver) { d.factory = f } }

// WithHomeRegion sets the region of the global endpoints (DescribeRegions); the
// default is --aws-home-region.
func WithHomeRegion(r string) Option { return func(d *Driver) { d.home = r } }

// WithCallTimeouts sets the deadline of one API call and of RunInstances.
func WithCallTimeouts(call, launch time.Duration) Option {
	return func(d *Driver) { d.callTimeout, d.launchTimeout = call, launch }
}

// WithPollInterval sets how often the driver looks again while it waits for an
// asynchronous EC2 state (the association of an IPv6 block).
func WithPollInterval(p time.Duration) Option { return func(d *Driver) { d.pollInterval = p } }

// WithPrices sets the source of the EBS and IPv4 prices; by default the driver
// takes the EBS and IPv4 prices from Keeper (SetKeeperPrices).
func WithPrices(p PriceSource) Option { return func(d *Driver) { d.prices = p } }

// Driver implements vendors.Driver and the capabilities NodeClassValidator,
// ImageReporter, NodeOptions, EpochReporter, ManagedResources, QuotaReporter
// and OfferingLister.
type Driver struct {
	cfg           vendors.Config
	clock         clock.Clock
	log           logr.Logger
	factory       ClientFactory
	home          string
	callTimeout   time.Duration
	launchTimeout time.Duration
	prices        PriceSource
	// pollInterval is how often a wait on an asynchronous EC2 state looks again.
	pollInterval time.Duration
	keeper       *keeperPrices

	// epoch changes when the answers of Supported and PriceDelta may have.
	epoch atomic.Uint64
	types *typeTable

	mu       sync.Mutex
	cl       map[string]Clients
	regions  cached[[]vendors.Region]
	zones    map[string]*cached[[]vendors.Zone]
	images   map[imageKey]cachedImage
	roots    map[string]string
	subnets  map[string]cachedSubnet
	quotas   map[string]*regionQuota
	pgs      map[string]cachedGroup
	sgCheck  map[string]cachedSG
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
	d := &Driver{
		pollInterval: 2 * time.Second,
		callTimeout:  DefaultCallTimeout, launchTimeout: DefaultLaunchTimeout, types: newTypeTable(),
		cl: map[string]Clients{}, zones: map[string]*cached[[]vendors.Zone]{}, images: map[imageKey]cachedImage{},
		roots: map[string]string{}, subnets: map[string]cachedSubnet{}, quotas: map[string]*regionQuota{},
		pgs: map[string]cachedGroup{}, sgCheck: map[string]cachedSG{},
		userSeen: map[string]bool{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Vendor implements vendors.Driver.
func (d *Driver) Vendor() string { return Name }

// Init implements vendors.Driver. It builds the SDK configuration from the
// default credential chain (environment, web identity token file, shared
// configuration, static keys) but makes no API call: credentials are resolved
// with the first call, so an unreachable AWS does not keep the controller from
// starting.
func (d *Driver) Init(ctx context.Context, cfg vendors.Config) error {
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	if d.home == "" {
		d.home = defaultHomeRegion
		if o := operatoroptions.FromContext(ctx); o != nil && o.AWSHomeRegion != "" {
			d.home = o.AWSHomeRegion
		}
	}
	if d.factory == nil {
		f, err := sdkFactory(ctx, d.home)
		if err != nil {
			return fmt.Errorf("loading the AWS configuration: %w", err)
		}
		d.factory = f
	}
	if d.prices == nil {
		var override *float64
		if v := os.Getenv(IPv4PriceEnv); v != "" {
			p, err := strconv.ParseFloat(v, 64)
			if err != nil || p < 0 || math.IsNaN(p) || math.IsInf(p, 0) {
				return fmt.Errorf("%s=%q is not a price in USD per hour (a finite number, 0 for free)", IPv4PriceEnv, v)
			}
			override = &p
			d.log.Info("the price of a public IPv4 address is set by the operator, Keeper's is not used",
				"usdPerHour", p, "override", IPv4PriceEnv)
		}
		d.keeper = newKeeperPrices(override)
		d.prices = d.keeper
	}
	return nil
}

// sdkConfig builds the base SDK configuration: the default credential chain,
// adaptive retries and the user agent scarpenter/<version>. The home region is
// the default region: the web identity provider (the recommended setup) builds
// its STS client from this configuration, so without a region in it (no
// AWS_REGION in the environment, a cluster that is not EKS) every call would
// fail with "Missing Region". The per-region clients override it.
func sdkConfig(ctx context.Context, home string) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithDefaultRegion(home),
		config.WithRetryMode(aws.RetryModeAdaptive),
		config.WithAPIOptions([]func(*middleware.Stack) error{
			awsmiddleware.AddUserAgentKeyValue("scarpenter", version.Version),
		}),
	)
}

// sdkFactory builds the real clients.
func sdkFactory(ctx context.Context, home string) (ClientFactory, error) {
	base, err := sdkConfig(ctx, home)
	if err != nil {
		return nil, err
	}
	return func(_ context.Context, region string) (Clients, error) {
		c := base.Copy()
		c.Region = region
		return Clients{EC2: ec2.NewFromConfig(c), Quotas: servicequotas.NewFromConfig(c)}, nil
	}, nil
}

// NewClientFactory returns the factory of real SDK clients (the default credential
// chain, adaptive retries, the home region as the SDK's default region): what the driver
// builds in Init. The sweeper CLI of the L5 lane (test/e2e-aws/sweeper) uses it to load the
// credentials and the configuration through the driver package; pkg/vendors/aws/sweep, its
// engine, only sees the narrow EC2API.
func NewClientFactory(ctx context.Context, home string) (ClientFactory, error) {
	return sdkFactory(ctx, home)
}

// clients returns the (lazily created) clients of a region, with per-call
// timeouts.
func (d *Driver) clients(ctx context.Context, region string) (Clients, error) {
	if region == "" {
		return Clients{}, vendors.Errorf(vendors.Fatal, "no AWS region given")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.cl[region]; ok {
		return c, nil
	}
	if d.factory == nil {
		return Clients{}, vendors.Errorf(vendors.Fatal, "the AWS driver is not initialised")
	}
	c, err := d.factory(ctx, region)
	if err != nil {
		return Clients{}, vendors.NewError(vendors.Fatal, "ClientFactory", fmt.Errorf("creating the AWS clients of %s: %w",
			region, err))
	}
	c = withTimeouts(c, d.callTimeout, d.launchTimeout)
	d.cl[region] = c
	return c, nil
}

// TagKey implements vendors.Driver: scarpenter.sparecores.com/<key>.
func (d *Driver) TagKey(k vendors.TagKey) string { return vendors.DefaultTagKey(k) }

// Regions implements vendors.Driver: the enabled regions of the account, read
// from the home region and cached for an hour.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	d.mu.Lock()
	if v, ok := d.regions.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return slices.Clone(v), nil
	}
	d.mu.Unlock()
	cl, err := d.clients(ctx, d.home)
	if err != nil {
		return nil, err
	}
	out, err := cl.EC2.DescribeRegions(ctx, &ec2.DescribeRegionsInput{})
	if err != nil {
		return nil, classify(err, errContext{})
	}
	regions := make([]vendors.Region, 0, len(out.Regions))
	for _, r := range out.Regions {
		if r.RegionName != nil {
			regions = append(regions, vendors.Region{Name: *r.RegionName})
		}
	}
	slices.SortFunc(regions, func(a, b vendors.Region) int { return compare(a.Name, b.Name) })
	d.mu.Lock()
	d.regions.set(regions, d.clock.Now().Add(regionsTTL))
	d.mu.Unlock()
	return slices.Clone(regions), nil
}

// Zones implements vendors.Driver: the zones of the region this account can use
// (availability zones only, opted in), with the portable zone ID and the
// account's zone name (overview section 5.8).
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	d.mu.Lock()
	if c := d.zones[region]; c != nil {
		if v, ok := c.get(d.clock.Now()); ok {
			d.mu.Unlock()
			return slices.Clone(v), nil
		}
	}
	d.mu.Unlock()
	cl, err := d.clients(ctx, region)
	if err != nil {
		return nil, err
	}
	out, err := cl.EC2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("zone-type"), Values: []string{"availability-zone"}},
			{Name: aws.String("opt-in-status"), Values: []string{"opt-in-not-required", "opted-in"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return nil, classifyRegionRead(err)
	}
	zones := make([]vendors.Zone, 0, len(out.AvailabilityZones))
	for _, z := range out.AvailabilityZones {
		if z.ZoneId != nil && z.ZoneName != nil {
			zones = append(zones, vendors.Zone{ID: *z.ZoneId, Name: *z.ZoneName})
		}
	}
	slices.SortFunc(zones, func(a, b vendors.Zone) int { return compare(a.Name, b.Name) })
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

// SetKeeperPrices implements vendors.KeeperPriceConsumer: the storage and IPv4 prices of the
// Keeper data version the catalog loaded. The node options epoch changes when
// they differ from the previous ones. A driver with injected prices
// (WithPrices) ignores them.
func (d *Driver) SetKeeperPrices(p vendors.KeeperPrices) {
	if d.keeper != nil && d.keeper.set(p) {
		d.epoch.Add(1)
	}
}

// NodeOptionsEpoch implements vendors.EpochReporter: Supported and PriceDelta
// depend on the instance type table and the prices, which load at runtime.
func (d *Driver) NodeOptionsEpoch() uint64 { return d.epoch.Load() }

func compare(a, b string) int { return strings.Compare(a, b) }

var (
	_ vendors.Driver             = (*Driver)(nil)
	_ vendors.NodeClassValidator = (*Driver)(nil)
	_ vendors.ImageReporter      = (*Driver)(nil)
	_ vendors.NodeOptions        = (*Driver)(nil)
	_ vendors.EpochReporter      = (*Driver)(nil)
	_ vendors.ManagedResources   = (*Driver)(nil)
	_ vendors.QuotaReporter      = (*Driver)(nil)
	_ vendors.OfferingLister     = (*Driver)(nil)
)
