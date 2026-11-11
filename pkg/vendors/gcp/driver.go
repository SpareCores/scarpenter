/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/go-logr/logr"
	"golang.org/x/oauth2/google"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/internal/version"
	operatoroptions "github.com/SpareCores/scarpenter/pkg/operator/options"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Name is the registration name and the Keeper vendor ID of the driver.
const Name = "gcp"

// MaxUserData is the largest metadata value Compute Engine accepts (256 KB): the
// limit of the user-data. The 16 KiB budget of the bootstrap renderer is far
// below it; GCP is not the limiting vendor.
const MaxUserData = 256 * 1024

// DefaultImageProject is the public project in which Flatcar publishes its GCE
// images (amd64).
const DefaultImageProject = "kinvolk-public"

// SpareCoresImageProject is the project in which Spare Cores publishes the arm64
// images (plan 16, section 1; built by the images repository, plan 22). It is
// empty until that project exists: while it is, an arm64 image resolves only
// from imageProject or an images selector, and arm64 is not ready otherwise.
const SpareCoresImageProject = ""

// IPv4PriceEnv overrides the price of an external IPv4 address (USD per hour).
const IPv4PriceEnv = "SCARPENTER_GCP_IPV4_PRICE"

// DefaultLaunchWait bounds how long CreateInstance waits for the insert
// operation; the operation is long-polled in rounds of the wait deadline.
const DefaultLaunchWait = 5 * time.Minute

// Cache lifetimes (plan 16, section 4).
const (
	factsTTL = time.Hour
	// maxAttempts bounds the attempt counters kept for request IDs.
	maxAttempts = 4096
)

func init() {
	vendors.Register(Name, func() vendors.Driver { return New() })
}

// Option configures a Driver (used by the tests and by wiring).
type Option func(*Driver)

// WithAPI replaces the Compute API client, for the tests and the canary. The
// driver wraps it with the per-call deadlines unless it is already wrapped
// (WithTimeouts is idempotent only in effect: a shorter deadline wins).
func WithAPI(api ComputeAPI) Option { return func(d *Driver) { d.api = api } }

// WithProject sets the project of the nodes; the default is --gcp-project, then
// the project of the credentials.
func WithProject(p string) Option { return func(d *Driver) { d.project = p } }

// WithCallTimeouts sets the deadline of one API call and of one operation wait.
func WithCallTimeouts(call, wait time.Duration) Option {
	return func(d *Driver) { d.callTimeout, d.waitTimeout = call, wait }
}

// WithLaunchWait bounds the wait for an insert operation.
func WithLaunchWait(w time.Duration) Option { return func(d *Driver) { d.launchWait = w } }

// WithPrices sets the source of the disk and IPv4 prices; by default the driver
// reads the disk prices from Keeper (the IPv4 price is a constant).
func WithPrices(p PriceSource) Option { return func(d *Driver) { d.prices = p } }

// Driver implements vendors.Driver and the capabilities NodeClassValidator,
// ImageReporter, NodeOptions, EpochReporter, ManagedResources, ResourceDefaults,
// DefaultChecker, ArchNamer, QuotaReporter, OfferingLister and BillingDescriber.
type Driver struct {
	cfg         vendors.Config
	clock       clock.Clock
	log         logr.Logger
	api         ComputeAPI
	apiMu       sync.Mutex
	project     string
	callTimeout time.Duration
	waitTimeout time.Duration
	launchWait  time.Duration
	prices      PriceSource
	keeper      *keeperPrices
	// epochID is random per process start; with the attempt counters it makes
	// the request IDs of inserts (RequestID).
	epochID string

	// epoch changes when the answers of Supported and PriceDelta may have.
	epoch atomic.Uint64

	mu       sync.Mutex
	regions  cached[[]vendors.Region]
	zones    cached[map[string][]vendors.Zone]
	machines map[string]machineInfo
	images   map[imageKey]cachedImage
	attempts map[string]int
	userSeen map[string]bool
	// confirmed overrides ConfirmedSeries, for the tests.
	confirmed map[string][]string
}

// machineInfo is what the driver keeps of a machine type: the vCPUs (quota
// consumption) and the architecture.
type machineInfo struct {
	VCPUs int64
	Arch  string
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
		callTimeout: DefaultCallTimeout, waitTimeout: DefaultWaitTimeout, launchWait: DefaultLaunchWait,
		machines: map[string]machineInfo{}, images: map[imageKey]cachedImage{},
		attempts: map[string]int{}, userSeen: map[string]bool{},
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	d.epochID = hex.EncodeToString(b[:])
	for _, o := range opts {
		o(d)
	}
	return d
}

// Vendor implements vendors.Driver.
func (d *Driver) Vendor() string { return Name }

// Project returns the project the driver works in (after Init).
func (d *Driver) Project() string { return d.project }

// Init implements vendors.Driver. It resolves the project (the option, the
// controller's --gcp-project, then the project of the credentials) but makes no
// API call: the Compute clients are created with the first call, so an
// unreachable Google does not keep the controller from starting.
func (d *Driver) Init(ctx context.Context, cfg vendors.Config) error {
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	if d.api != nil {
		d.api = WithTimeouts(d.api, d.callTimeout, d.waitTimeout)
	}
	if d.project == "" {
		if o := operatoroptions.FromContext(ctx); o != nil {
			d.project = o.GCPProject
		}
	}
	if d.project == "" && d.api == nil {
		cred, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return fmt.Errorf("finding the Google credentials (set --gcp-project and the credentials, "+
				"see docs/how-to-guides/gcp-credentials.md): %w", err)
		}
		d.project = cred.ProjectID
	}
	if d.project == "" {
		return fmt.Errorf("the GCP project is not known: set --gcp-project (SCARPENTER_GCP_PROJECT); " +
			"the credentials carry none (workload identity federation has no project)")
	}
	if d.prices == nil && cfg.KeeperURL != "" {
		ipv4 := DefaultIPv4PricePerHour
		if v := os.Getenv(IPv4PriceEnv); v != "" {
			p, err := strconv.ParseFloat(v, 64)
			if err != nil || p < 0 || math.IsNaN(p) || math.IsInf(p, 0) {
				return fmt.Errorf("%s=%q is not a price in USD per hour (a finite number, 0 for free)", IPv4PriceEnv, v)
			}
			ipv4 = p
		}
		d.log.Info("the price of an external IPv4 address is a constant, not from Keeper (it has none)",
			"usdPerHour", ipv4, "override", IPv4PriceEnv)
		d.keeper = newKeeperPrices(cfg.KeeperURL, ipv4, func() { d.epoch.Add(1) })
		d.prices = d.keeper
	}
	return nil
}

// compute returns the Compute API client, created on first use with per-call
// deadlines and the user agent scarpenter/<version>.
func (d *Driver) compute(ctx context.Context) (ComputeAPI, error) {
	d.apiMu.Lock()
	defer d.apiMu.Unlock()
	if d.api != nil {
		return d.api, nil
	}
	sdk, err := NewSDK(ctx, "scarpenter/"+version.Version)
	if err != nil {
		return nil, vendors.NewError(vendors.Fatal, "ClientFactory", fmt.Errorf("creating the Compute clients: %w", err))
	}
	d.api = WithTimeouts(sdk, d.callTimeout, d.waitTimeout)
	return d.api, nil
}

// TagKey implements vendors.Driver: scarpenter-<key>.
func (d *Driver) TagKey(k vendors.TagKey) string { return TagKey(k) }

// Regions implements vendors.Driver: the regions that are UP, cached for an hour.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	d.mu.Lock()
	if v, ok := d.regions.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return slices.Clone(v), nil
	}
	d.mu.Unlock()
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	var regions []vendors.Region
	req := &computepb.ListRegionsRequest{Project: d.project}
	for {
		list, err := api.ListRegions(ctx, req)
		if err != nil {
			return nil, classifyRegionRead(err)
		}
		for _, r := range list.GetItems() {
			if r.GetStatus() == "UP" || r.GetStatus() == "" {
				regions = append(regions, vendors.Region{Name: r.GetName()})
			}
		}
		if list.GetNextPageToken() == "" {
			break
		}
		req.PageToken = list.NextPageToken
	}
	slices.SortFunc(regions, func(a, b vendors.Region) int { return strings.Compare(a.Name, b.Name) })
	d.mu.Lock()
	d.regions.set(regions, d.clock.Now().Add(factsTTL))
	d.mu.Unlock()
	d.kickPrices()
	return slices.Clone(regions), nil
}

// allZones reads every zone of the project once and groups the usable ones by
// region: Keeper's zone ID is Google's numeric zone ID, the name is the zone
// name, both global (no per-account mapping).
func (d *Driver) allZones(ctx context.Context) (map[string][]vendors.Zone, error) {
	d.mu.Lock()
	if v, ok := d.zones.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return v, nil
	}
	d.mu.Unlock()
	api, err := d.compute(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]vendors.Zone{}
	req := &computepb.ListZonesRequest{Project: d.project}
	for {
		list, err := api.ListZones(ctx, req)
		if err != nil {
			return nil, classifyRegionRead(err)
		}
		for _, z := range list.GetItems() {
			if z.GetStatus() != "UP" && z.GetStatus() != "" {
				continue
			}
			if st := z.GetDeprecated().GetState(); st != "" && st != "ACTIVE" {
				continue
			}
			region := lastSegment(z.GetRegion())
			if region == "" || region == z.GetRegion() && !strings.Contains(z.GetName(), "-") {
				region = RegionOfZone(z.GetName())
			}
			out[region] = append(out[region], vendors.Zone{ID: strconv.FormatUint(z.GetId(), 10), Name: z.GetName()})
		}
		if list.GetNextPageToken() == "" {
			break
		}
		req.PageToken = list.NextPageToken
	}
	for _, zs := range out {
		slices.SortFunc(zs, func(a, b vendors.Zone) int { return strings.Compare(a.Name, b.Name) })
	}
	d.mu.Lock()
	d.zones.set(out, d.clock.Now().Add(factsTTL))
	d.mu.Unlock()
	return out, nil
}

// Zones implements vendors.Driver: the zones of the region that are UP and not
// deprecated.
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	all, err := d.allZones(ctx)
	if err != nil {
		return nil, err
	}
	return slices.Clone(all[region]), nil
}

// NodeOptionsEpoch implements vendors.EpochReporter: Supported and PriceDelta
// depend on the machine types and the prices, which load at runtime.
func (d *Driver) NodeOptionsEpoch() uint64 { return d.epoch.Load() }

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

// knownRegion says whether the region is one the project can use; a failure to
// list is taken as known, so that a transient error surfaces from the call.
func (d *Driver) knownRegion(ctx context.Context, region string) bool {
	regions, err := d.Regions(ctx)
	if err != nil {
		return strings.TrimSpace(region) != ""
	}
	return slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == region })
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
	_ vendors.ArchNamer          = (*Driver)(nil)
	_ vendors.BillingDescriber   = (*Driver)(nil)
)
