/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package azure is the Azure driver (plan 17): Flatcar VMs in one subscription
// through the track-2 Azure SDK, with the Azure Marketplace as the image source
// (rung 1 of the image ladder, ADR-021), vendor-native node settings (managed
// disks, accelerated networking, proximity placement groups), managed resources
// (resource groups, a virtual network with its subnet, network security groups
// with their application security groups, proximity placement groups) and
// quota-aware placement from the Usage APIs. Every API call goes through the
// narrow interfaces of api.go, which the in-memory ARM in pkg/vendors/azure/fake
// implements for the tests.
package azure

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The environment of the driver. The subscription is the Azure SDK's own
// variable; the others are set by the chart (the credential values and the
// development flag of the controller).
const (
	// EnvSubscriptionID is the subscription of the controller: one controller,
	// one subscription, no discovery.
	EnvSubscriptionID = "AZURE_SUBSCRIPTION_ID"
	// EnvCredentialsMode is workloadIdentity (the default and recommended), secret
	// or none (the same chain as workloadIdentity).
	EnvCredentialsMode = "SCARPENTER_AZURE_CREDENTIALS_MODE"
	// EnvDevelopment lets the credential chain end in the Azure CLI's login.
	EnvDevelopment = "SCARPENTER_AZURE_DEVELOPMENT"
	// EnvIPv4Price overrides the price of a Standard public IPv4 address (USD per
	// hour).
	EnvIPv4Price = "SCARPENTER_AZURE_IPV4_PRICE"
)

// MaxUserData is the budget the bootstrap renderer holds the user-data to: the
// limit of the raw user-data of plan 17 section 3 (16 KiB).
const MaxUserData = maxUserData

// Cache lifetimes.
const (
	regionsTTL = time.Hour
	skusTTL    = time.Hour
)

func init() {
	vendors.Register(Name, func() vendors.Driver { return New() })
}

// Name is the registration name and the Keeper vendor ID of the driver.
const Name = Vendor

// Option configures a Driver (used by the tests and by wiring).
type Option func(*Driver)

// WithClients replaces the SDK clients, for the tests.
func WithClients(c Clients) Option { return func(d *Driver) { d.clients = c } }

// WithSubscription sets the subscription instead of reading AZURE_SUBSCRIPTION_ID.
func WithSubscription(id string) Option { return func(d *Driver) { d.subscription = id } }

// WithPrices sets the source of the disk and public IP prices; by default the
// driver reads the disk prices from Keeper (the IPv4 price is a constant).
func WithPrices(p PriceSource) Option { return func(d *Driver) { d.prices = p } }

// WithCreateTimeout sets how long a launch waits for the VM's long-running
// operation (and a delete for its own).
func WithCreateTimeout(t time.Duration) Option {
	return func(d *Driver) { d.createTimeout, d.deleteTimeout = t, t }
}

// Driver implements vendors.Driver and the capabilities NodeClassValidator,
// ImageReporter, NodeOptions, EpochReporter, ManagedResources, QuotaReporter,
// OfferingLister, BillingDescriber and ArchNamer.
type Driver struct {
	cfg          vendors.Config
	clock        clock.Clock
	log          logr.Logger
	subscription string
	clients      Clients
	compute      *Compute
	images       *Images
	quota        *QuotaReader
	sshKey       string
	prices       PriceSource
	keeper       *keeperPrices

	createTimeout, deleteTimeout time.Duration

	// epoch changes when the answers of Supported and PriceDelta may have.
	epoch atomic.Uint64

	mu        sync.Mutex
	locations cached[[]*armsubscriptions.Location]
	skus      map[string]cachedSKUs
	allSKUs   map[string]SKU
	imgCache  map[string]cachedResolution
	warned    map[string]bool
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

type cachedSKUs struct {
	skus  map[string]SKU
	until time.Time
}

// New returns an uninitialised driver.
func New(opts ...Option) *Driver {
	d := &Driver{
		skus: map[string]cachedSKUs{}, allSKUs: map[string]SKU{}, imgCache: map[string]cachedResolution{},
		warned: map[string]bool{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Vendor implements vendors.Driver.
func (d *Driver) Vendor() string { return Name }

// Init implements vendors.Driver. It builds the credential chain and the
// clients but makes no API call: tokens are requested with the first call, so
// an unreachable Azure does not keep the controller from starting. The
// subscription is required (AZURE_SUBSCRIPTION_ID): one controller, one
// subscription.
func (d *Driver) Init(_ context.Context, cfg vendors.Config) error {
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	if cfg.ClusterName == "" {
		return errors.New("the Azure driver needs the cluster name: it is the owner tag of every VM")
	}
	if d.subscription == "" {
		d.subscription = os.Getenv(EnvSubscriptionID)
	}
	if d.subscription == "" {
		return fmt.Errorf("the Azure driver needs the subscription ID: set %s", EnvSubscriptionID)
	}
	if d.clients.VMs == nil {
		cred, err := NewCredential(os.Getenv(EnvCredentialsMode), isTrue(os.Getenv(EnvDevelopment)))
		if err != nil {
			return fmt.Errorf("building the Azure credentials: %w", err)
		}
		c, err := d.sdkClients(cred)
		if err != nil {
			return err
		}
		d.clients = c
	}
	key, err := GenerateDiscardedKey()
	if err != nil {
		return fmt.Errorf("generating the VM key: %w", err)
	}
	d.sshKey = key
	d.compute = &Compute{Clients: d.clients, Cluster: cfg.ClusterName, Clock: d.clock,
		CreateTimeout: d.createTimeout, DeleteTimeout: d.deleteTimeout}
	d.images = &Images{API: d.clients.Images, Terms: d.clients.Agreements, Clock: d.clock}
	d.quota = NewQuotaReader(d.clients.Usages)
	if d.prices == nil && cfg.KeeperURL != "" {
		ipv4 := DefaultIPv4PricePerHour
		if v := os.Getenv(EnvIPv4Price); v != "" {
			p, err := strconv.ParseFloat(v, 64)
			if err != nil || p < 0 || math.IsNaN(p) || math.IsInf(p, 0) {
				return fmt.Errorf("%s=%q is not a price in USD per hour (a finite number, 0 for free)", EnvIPv4Price, v)
			}
			ipv4 = p
		}
		// An approximation, not a Keeper price: say so where the operator sees it.
		d.log.Info("the price of a public IPv4 address is a constant, not from Keeper (it has none)",
			"usdPerHour", ipv4, "override", EnvIPv4Price)
		d.keeper = newKeeperPrices(cfg.KeeperURL, ipv4, func() { d.epoch.Add(1) })
		d.prices = d.keeper
	}
	return nil
}

func isTrue(s string) bool { return strings.EqualFold(s, "true") || s == "1" }

// sdkClients builds the real clients of the subscription with the user agent
// scarpenter/<version> (azcore retries 429 and 5xx and honours Retry-After).
func (d *Driver) sdkClients(cred azcore.TokenCredential) (Clients, error) {
	return NewSDKClients(cred, d.subscription, SDKOptions{ApplicationID: "scarpenter/" + version.Version})
}

// NewClients builds the real clients of a subscription from the credential chain
// of the driver (the mode of SCARPENTER_AZURE_CREDENTIALS_MODE), for the sweeper
// CLI of the L5 lane: it loads the credentials and the SDK through the driver
// package, and the sweep engine only sees the narrow interfaces.
func NewClients(subscription string) (Clients, error) {
	cred, err := NewCredential(os.Getenv(EnvCredentialsMode), isTrue(os.Getenv(EnvDevelopment)))
	if err != nil {
		return Clients{}, err
	}
	return NewSDKClients(cred, subscription, SDKOptions{ApplicationID: "scarpenter/" + version.Version})
}

// TagKey implements vendors.Driver: scarpenter-<key> (the opt-in tag
// scarpenter-use included).
func (d *Driver) TagKey(k vendors.TagKey) string { return TagKey(k) }

// locationList reads the subscription's locations (cached for an hour).
func (d *Driver) locationList(ctx context.Context) ([]*armsubscriptions.Location, error) {
	d.mu.Lock()
	if v, ok := d.locations.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return v, nil
	}
	d.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	locs, err := d.clients.Locations.List(cctx)
	if err != nil {
		return nil, classify(err, errContext{})
	}
	physical := make([]*armsubscriptions.Location, 0, len(locs))
	for _, l := range locs {
		if l == nil || l.Name == nil {
			continue
		}
		// Staging and logical regions are not places a VM can go.
		if m := l.Metadata; m != nil && m.RegionType != nil && *m.RegionType != armsubscriptions.RegionTypePhysical {
			continue
		}
		physical = append(physical, l)
	}
	slices.SortFunc(physical, func(a, b *armsubscriptions.Location) int { return strings.Compare(*a.Name, *b.Name) })
	d.mu.Lock()
	d.locations.set(physical, d.clock.Now().Add(regionsTTL))
	d.mu.Unlock()
	d.kickPrices()
	return physical, nil
}

// Regions implements vendors.Driver: the physical locations of the subscription.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	locs, err := d.locationList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.Region, 0, len(locs))
	for _, l := range locs {
		out = append(out, vendors.Region{Name: *l.Name})
	}
	return out, nil
}

// Zones implements vendors.Driver: the logical zones of the region
// (availabilityZoneMappings of the locations API), with their own names as IDs
// (Keeper's zone names are 1, 2 and 3; overview section 5.8). A region without
// zones has the zone 0, the name Keeper gives it.
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	locs, err := d.locationList(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range locs {
		if !strings.EqualFold(*l.Name, region) {
			continue
		}
		var out []vendors.Zone
		for _, m := range l.AvailabilityZoneMappings {
			if m != nil && deref(m.LogicalZone) != "" {
				out = append(out, vendors.Zone{ID: *m.LogicalZone, Name: *m.LogicalZone})
			}
		}
		if len(out) == 0 {
			out = []vendors.Zone{{ID: ZoneNone, Name: ZoneNone}}
		}
		slices.SortFunc(out, func(a, b vendors.Zone) int { return strings.Compare(a.Name, b.Name) })
		return out, nil
	}
	return nil, vendors.NewError(vendors.RegionUnavailable, "RegionNotInSubscription",
		fmt.Errorf("the subscription has no region %q", region))
}

// NodeOptionsEpoch implements vendors.EpochReporter: Supported and PriceDelta
// depend on the SKU table and the prices, which load at runtime.
func (d *Driver) NodeOptionsEpoch() uint64 { return d.epoch.Load() }

// kickPrices starts the background load of the disk prices.
func (d *Driver) kickPrices() {
	if d.keeper != nil {
		d.keeper.kick(d.clock.Now(), func(err error) { d.log.Error(err, "loading the disk prices from Keeper") })
	}
}

// LoadPrices reads the disk prices from Keeper now and waits for the result;
// the driver otherwise loads them in the background from the first read of the
// regions. A no-op without a Keeper URL.
func (d *Driver) LoadPrices(ctx context.Context) error {
	if d.keeper == nil {
		return nil
	}
	return d.keeper.load(ctx)
}

// skuTable reads the size table of a region (cached for an hour) and feeds the
// facts the options and the quota mapping read. A region whose sizes are all
// restricted, or that the subscription is denied, is RegionUnavailable.
func (d *Driver) skuTable(ctx context.Context, region string) (map[string]SKU, error) {
	key := strings.ToLower(region)
	d.mu.Lock()
	if c, ok := d.skus[key]; ok && d.clock.Now().Before(c.until) {
		d.mu.Unlock()
		return c.skus, nil
	}
	d.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := d.clients.SKUs.List(cctx, region)
	if err != nil {
		return nil, classifyRegionRead(err)
	}
	skus := ParseSKUs(raw, region)
	if !RegionUsable(skus) {
		return nil, vendors.NewError(vendors.RegionUnavailable, "NoLaunchableSize",
			fmt.Errorf("the subscription can launch no virtual machine size in %s", region))
	}
	d.quota.LoadSKUs(skus)
	d.mu.Lock()
	d.skus[key] = cachedSKUs{skus: skus, until: d.clock.Now().Add(skusTTL)}
	grew := false
	for n, s := range skus {
		if _, ok := d.allSKUs[n]; !ok {
			grew = true
		}
		d.allSKUs[n] = s
	}
	d.mu.Unlock()
	if grew {
		d.epoch.Add(1)
	}
	return skus, nil
}

// skuFacts returns the capabilities of a size from any region read so far.
func (d *Driver) skuFacts(name string) (SKU, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.allSKUs[name]
	return s, ok
}

// LaunchableTypes implements vendors.OfferingLister: the sizes the subscription
// can launch in the region, by zone (Gen2, a SCSI controller, no restriction).
// Azure has no stock check: capacity is learnt reactively.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	skus, err := d.skuTable(ctx, region)
	if err != nil {
		return nil, err
	}
	return LaunchableTypes(skus), nil
}

// VendorArch implements vendors.ArchNamer: how Azure spells the CPU
// architecture of an image (the architecture field of a marketplace image).
func (d *Driver) VendorArch(arch string) (string, bool) {
	switch arch {
	case archAMD64:
		return "x64", true
	case archARM64:
		return "Arm64", true
	}
	return "", false
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
)
