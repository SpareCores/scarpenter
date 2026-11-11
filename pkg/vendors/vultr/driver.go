/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The environment of the driver.
const (
	// APIKeyEnv holds the API key of the account (a Kubernetes Secret in the
	// chart). Vultr has no workload identity for callers, so there is no other
	// credential mode.
	APIKeyEnv = "VULTR_API_KEY"
	// AvailabilityTTLEnv overrides how long the stock of a region is cached
	// (default 5 minutes: Vultr's stock moves faster than AWS's).
	AvailabilityTTLEnv = "SCARPENTER_VULTR_AVAILABILITY_TTL"
)

// regionsTTL is how long the region list is cached.
const regionsTTL = time.Hour

func init() {
	vendors.Register(Vendor, func() vendors.Driver { return New() })
}

// Option configures a Driver (the tests and the sweeper use them).
type Option func(*Driver)

// WithAPI replaces the API client, so that Init builds none and needs no key.
func WithAPI(api API) Option { return func(d *Driver) { d.api = api } }

// WithAvailabilityTTL sets how long the stock of a region is cached.
func WithAvailabilityTTL(ttl time.Duration) Option { return func(d *Driver) { d.availTTL = ttl } }

// Driver implements vendors.Driver and the capabilities NodeClassValidator,
// ImageReporter, NodeOptions, ManagedResources, OfferingLister, NodeHardener and
// BillingDescriber. It has no QuotaReporter (Vultr's limits are visible in the
// console only) and no ResourceDefaults (Vultr has no default firewall group or
// VPC to find).
type Driver struct {
	cfg      vendors.Config
	clock    clock.Clock
	log      logr.Logger
	api      API
	ops      *Ops
	cluster  string
	availTTL time.Duration

	mu       sync.Mutex
	regions  cached[[]vendors.Region]
	images   map[imageKey]cachedImage
	keyCheck cached[error]
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
	d := &Driver{images: map[imageKey]cachedImage{}}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Vendor implements vendors.Driver.
func (d *Driver) Vendor() string { return Vendor }

// Init implements vendors.Driver. It builds the client from VULTR_API_KEY but
// makes no call: an unreachable Vultr does not keep the controller from
// starting, and the key is checked by ValidateNodeClass (VendorReady).
func (d *Driver) Init(_ context.Context, cfg vendors.Config) error {
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	d.cluster = cfg.ClusterName
	if d.cluster == "" {
		return errors.New("the Vultr driver needs the cluster name: it tags every instance with it and deletes only those")
	}
	if d.api == nil {
		key := strings.TrimSpace(os.Getenv(APIKeyEnv))
		if key == "" {
			return fmt.Errorf("%s is not set: the Vultr driver needs the API key of the account", APIKeyEnv)
		}
		api, err := NewClient(ClientConfig{APIKey: key})
		if err != nil {
			return err
		}
		d.api = api
	}
	if d.availTTL == 0 {
		if v := os.Getenv(AvailabilityTTLEnv); v != "" {
			ttl, err := time.ParseDuration(v)
			if err != nil || ttl <= 0 {
				return fmt.Errorf("%s=%q is not a positive duration", AvailabilityTTLEnv, v)
			}
			d.availTTL = ttl
		}
	}
	d.ops = NewOps(d.api, d.cluster)
	d.ops.Clock = d.clock
	d.ops.AvailabilityTTL = d.availTTL
	return nil
}

func (d *Driver) ready() error {
	if d.ops == nil {
		return vendors.Errorf(vendors.Fatal, "the Vultr driver is not initialised")
	}
	return nil
}

// TagKey implements vendors.Driver: scarpenter.sparecores.com/<key>, the key of
// the tag string "<key>=<value>" (Vultr tags are plain strings).
func (d *Driver) TagKey(k vendors.TagKey) string { return d.tagStyle().Key(k) }

func (d *Driver) tagStyle() TagStyle {
	if d.ops != nil {
		return d.ops.Tags
	}
	return TagStyleDomain
}

// Regions implements vendors.Driver: the regions of GET /v2/regions (public,
// the same for every account), cached for an hour.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	v, ok := d.regions.get(d.clock.Now())
	d.mu.Unlock()
	if ok {
		return slices.Clone(v), nil
	}
	regions, err := d.ops.Regions(ctx, nil)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.regions.set(regions, d.clock.Now().Add(regionsTTL))
	d.mu.Unlock()
	return slices.Clone(regions), nil
}

// Zones implements vendors.Driver: one zone per region, named like it (Keeper's
// zone ID equals the region ID).
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	regions, err := d.Regions(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == region }) {
		return nil, nil
	}
	return []vendors.Zone{Zone(region)}, nil
}

// CreateInstance implements vendors.Driver (Ops.Launch): an idempotent create
// of the NodeClaim's instance from the snapshot ResolveImage chose.
func (d *Driver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	lo, err := launchOptions(req)
	if err != nil {
		return nil, err
	}
	return d.ops.Launch(ctx, req, lo)
}

// GetInstance implements vendors.Driver.
func (d *Driver) GetInstance(ctx context.Context, ref vendors.InstanceRef) (*vendors.Instance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.ops.GetInstance(ctx, ref)
}

// FindInstance implements vendors.Driver.
func (d *Driver) FindInstance(
	ctx context.Context, region string, tags map[vendors.TagKey]string,
) (*vendors.Instance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.ops.FindInstance(ctx, region, tags)
}

// ListInstances implements vendors.Driver.
func (d *Driver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	return d.ops.ListInstances(ctx, regions, tags)
}

// DeleteInstance implements vendors.Driver.
func (d *Driver) DeleteInstance(ctx context.Context, ref vendors.InstanceRef) error {
	if err := d.ready(); err != nil {
		return err
	}
	return d.ops.DeleteInstance(ctx, ref)
}

// LaunchableTypes implements vendors.OfferingLister: the plans in stock in the
// region that the driver launches. The zone of a plan is the region.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	if err := d.ready(); err != nil {
		return nil, err
	}
	plans, err := d.ops.LaunchableTypes(ctx, region)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(plans))
	for _, p := range plans {
		out[p] = []string{region}
	}
	return out, nil
}

// Hardenings implements vendors.NodeHardener: Pods can reach the metadata
// service (169.254.169.254), which serves the node's user-data (Ignition,
// with the join credentials) and has nothing like AWS's hop limit.
func (d *Driver) Hardenings() []string { return []string{vendors.HardeningMetadataFromPods} }

// metadataAddress is Vultr's metadata service (Ignition reads
// /user-data/user-data from it).
const metadataAddress = "169.254.169.254"

// BootFragments implements vendors.NodeHardener.
func (d *Driver) BootFragments(req vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	if !slices.Contains(req.Hardening, vendors.HardeningMetadataFromPods) {
		return nil, nil
	}
	f, err := vendors.BlockMetadataFromPods(metadataAddress)
	if err != nil {
		return nil, err
	}
	return []vendors.BootFragment{f}, nil
}

// keyCheckOK and keyCheckFailed are how long the verdict on the API key is
// trusted. A transient failure is no verdict and is never cached.
const (
	keyCheckOK     = 5 * time.Minute
	keyCheckFailed = time.Minute
)

// checkKey proves the key and its ACL with GET /v2/instances?per_page=1 (the
// regions endpoint is public and proves only connectivity). Only a definitive
// refusal (401, 403) is an error: throttling or a timeout says nothing about the
// key, and must not gate every offering of the vendor.
func (d *Driver) checkKey(ctx context.Context) error {
	d.mu.Lock()
	v, ok := d.keyCheck.get(d.clock.Now())
	d.mu.Unlock()
	if ok {
		return v
	}
	_, _, err := d.api.ListInstances(ctx, listOne())
	if err != nil {
		err = classify(err, opOther)
		if vendors.IsClass(err, vendors.Retryable) {
			return nil
		}
		err = fmt.Errorf("the API key in %s is refused by Vultr (the key, its ACL or the controller's egress address "+
			"missing from the key's IP allowlist): %w", APIKeyEnv, err)
		d.mu.Lock()
		d.keyCheck.set(err, d.clock.Now().Add(keyCheckFailed))
		d.mu.Unlock()
		return err
	}
	d.mu.Lock()
	d.keyCheck.set(nil, d.clock.Now().Add(keyCheckOK))
	d.mu.Unlock()
	return nil
}

var (
	_ vendors.Driver             = (*Driver)(nil)
	_ vendors.NodeClassValidator = (*Driver)(nil)
	_ vendors.ImageReporter      = (*Driver)(nil)
	_ vendors.NodeOptions        = (*Driver)(nil)
	_ vendors.ManagedResources   = (*Driver)(nil)
	_ vendors.OfferingLister     = (*Driver)(nil)
	_ vendors.NodeHardener       = (*Driver)(nil)
	_ vendors.BillingDescriber   = (*Driver)(nil)
)
