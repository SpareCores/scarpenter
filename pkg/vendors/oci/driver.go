/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package oci is the Oracle Cloud Infrastructure driver (plan 25). This is its
// foundation (step b): the type names and their parsing, the narrow API
// interfaces with the SDK adapter and the in-memory fake, the error table, names
// and tags, the shape and availability domain caches, the instance calls and the
// quota reporter. The API block, the images, the resources and the registration
// follow in step (c).
package oci

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/internal/version"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Cache lifetimes (plan 25, section 5: the shape, AD and limit tables, an hour).
const (
	factsTTL = time.Hour
	// maxAttempts bounds the attempt counters kept for retry tokens.
	maxAttempts = 4096
)

// Option configures a Driver (used by the tests and by wiring).
type Option func(*Driver)

// WithClients replaces the API clients, for the tests and the canary.
func WithClients(c Clients) Option { return func(d *Driver) { d.api = c } }

// WithConfigProvider sets the credentials: the driver talks to OCI through the
// SDK adapter built over them, and reads the tenancy from them.
func WithConfigProvider(p common.ConfigurationProvider) Option {
	return func(d *Driver) { d.provider = p }
}

// WithCompartment sets the compartment of the nodes (--oci-compartment): the
// instances, boot volumes and the default image lookup live in it.
func WithCompartment(ocid string) Option { return func(d *Driver) { d.compartment = ocid } }

// WithTenancy sets the tenancy OCID; the default is the credentials'.
func WithTenancy(ocid string) Option { return func(d *Driver) { d.tenancy = ocid } }

// WithCallTimeout sets the deadline of one SDK call; the default is
// DefaultCallTimeout. It applies to the adapter the driver builds itself.
func WithCallTimeout(t time.Duration) Option { return func(d *Driver) { d.callTimeout = t } }

// Driver implements vendors.Driver and the capability QuotaReporter. The other
// capabilities (images, node options, managed resources, validation) come in
// step (c).
type Driver struct {
	cfg         vendors.Config
	clock       clock.Clock
	log         logr.Logger
	api         Clients
	provider    common.ConfigurationProvider
	compartment string
	tenancy     string
	callTimeout time.Duration
	// epochID is random per process start; with the attempt counters it makes
	// the retry tokens of launches.
	epochID string

	mu      sync.Mutex
	regions cached[[]vendors.Region]
	ads     map[string]cached[[]availabilityDomain]
	shapes  map[adKey]cached[map[string]*shapeInfo]
	index   map[string]*shapeInfo
	limits  map[string]cached[map[string]limitDef]
	// noQuota holds the limit names whose availability the service does not answer
	// for: the shapes that consume them have unknown quota.
	noQuota  map[string]bool
	attempts map[string]int
	prices   keeperPrices
	epoch    atomic.Uint64
	// images caches the resolved images (and the misses) by request; archs the
	// architecture of an image that has no arch tag, read from its shape
	// compatibility entries.
	images map[imageKey]cachedImage
	archs  map[string]cachedArch
	// subnets remembers what Resolve read of the subnets (the availability domain
	// and the options a launch into it depends on).
	subnets map[string]cached[subnetInfo]
}

// cached is a value with an expiry.
type cached[T any] struct {
	v     T
	until time.Time
	ok    bool
}

func (c cached[T]) get(now time.Time) (T, bool) {
	if c.ok && now.Before(c.until) {
		return c.v, true
	}
	var zero T
	return zero, false
}

func set[T any](v T, until time.Time) cached[T] { return cached[T]{v: v, until: until, ok: true} }

// New returns an uninitialised driver.
func New(opts ...Option) *Driver {
	d := &Driver{
		callTimeout: DefaultCallTimeout,
		ads:         map[string]cached[[]availabilityDomain]{}, shapes: map[adKey]cached[map[string]*shapeInfo]{},
		index: map[string]*shapeInfo{}, limits: map[string]cached[map[string]limitDef]{},
		attempts: map[string]int{}, noQuota: map[string]bool{}, images: map[imageKey]cachedImage{},
		archs: map[string]cachedArch{}, subnets: map[string]cached[subnetInfo]{},
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

// Compartment returns the compartment the driver works in.
func (d *Driver) Compartment() string { return d.compartment }

// Init implements vendors.Driver. It checks the configuration and builds the SDK
// adapter over the credentials, but makes no API call: the clients are made with
// the first call of a region, so an unreachable Oracle does not keep the
// controller from starting. The startup check of plan 25 (section 4) is the
// caller's: Regions proves the identity, ListInstances the policy.
func (d *Driver) Init(ctx context.Context, cfg vendors.Config) error {
	if err := d.applyContextOptions(ctx); err != nil {
		return err
	}
	d.cfg = cfg
	d.clock = cfg.Clock
	if d.clock == nil {
		d.clock = clock.RealClock{}
	}
	d.log = cfg.Log
	if !IsOCID(d.compartment, "compartment") {
		return fmt.Errorf("the OCI compartment is not set or not a compartment OCID (%q): "+
			"set --oci-compartment (SCARPENTER_OCI_COMPARTMENT)", d.compartment)
	}
	if d.api.Compute == nil {
		if d.provider == nil {
			return fmt.Errorf("the OCI credentials are not set: see docs/how-to-guides/oci-credentials.md")
		}
		sdk := NewSDK(d.provider, "scarpenter/"+version.Version, WithSDKTimeout(d.callTimeout))
		d.api = Clients{Compute: sdk, Network: sdk, Identity: sdk, Limits: sdk}
	}
	if d.tenancy == "" && d.provider != nil {
		t, err := d.provider.TenancyOCID()
		if err != nil {
			return fmt.Errorf("reading the tenancy of the OCI credentials: %w", err)
		}
		d.tenancy = t
	}
	if !IsOCID(d.tenancy, "tenancy") {
		return fmt.Errorf("the OCI tenancy is not known or not a tenancy OCID (%q)", d.tenancy)
	}
	return nil
}

// TagKey implements vendors.Driver: scarpenter-<key>.
func (d *Driver) TagKey(k vendors.TagKey) string { return TagKey(k) }

// availabilityDomain is an AD of a region as the tenancy sees it.
type availabilityDomain struct {
	// Full is the tenancy-specific name ("Xxxx:EU-FRANKFURT-1-AD-1"), which every
	// call takes; Name is the zone name without the prefix.
	Full, Name, ZoneID string
}

// Regions implements vendors.Driver: the subscribed regions (status READY), cached
// for an hour. The NodeClass's allow-list narrows them elsewhere.
func (d *Driver) Regions(ctx context.Context) ([]vendors.Region, error) {
	d.mu.Lock()
	if v, ok := d.regions.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return slices.Clone(v), nil
	}
	d.mu.Unlock()
	res, err := d.api.Identity.ListRegionSubscriptions(ctx,
		identity.ListRegionSubscriptionsRequest{TenancyId: &d.tenancy})
	if err != nil {
		return nil, fmt.Errorf("listing the region subscriptions: %w", classify(err, kindRead))
	}
	var regions []vendors.Region
	for _, r := range res.Items {
		if r.Status == identity.RegionSubscriptionStatusReady && r.RegionName != nil {
			regions = append(regions, vendors.Region{Name: *r.RegionName})
		}
	}
	slices.SortFunc(regions, func(a, b vendors.Region) int { return strings.Compare(a.Name, b.Name) })
	d.mu.Lock()
	d.regions = set(regions, d.clock.Now().Add(factsTTL))
	d.mu.Unlock()
	return slices.Clone(regions), nil
}

// subscribed reports whether the region is one the credentials can use. A
// region name that is not subscribed never reaches an endpoint.
func (d *Driver) subscribed(ctx context.Context, region string) (bool, error) {
	regions, err := d.Regions(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(regions, func(r vendors.Region) bool { return r.Name == region }), nil
}

// domains returns the availability domains of the region, cached for an hour.
func (d *Driver) domains(ctx context.Context, region string) ([]availabilityDomain, error) {
	d.mu.Lock()
	if v, ok := d.ads[region].get(d.clock.Now()); ok {
		d.mu.Unlock()
		return v, nil
	}
	d.mu.Unlock()
	ok, err := d.subscribed(ctx, region)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, vendors.NewError(vendors.RegionUnavailable, codeUnknownReg,
			fmt.Errorf("the region %q is not subscribed by the tenancy", region))
	}
	res, err := d.api.Identity.ListAvailabilityDomains(ctx, region,
		identity.ListAvailabilityDomainsRequest{CompartmentId: &d.tenancy})
	if err != nil {
		return nil, fmt.Errorf("listing the availability domains of %s: %w", region, classify(err, kindRegion))
	}
	var out []availabilityDomain
	for _, a := range res.Items {
		full := deref(a.Name)
		id, ok := ZoneID(region, full)
		if !ok {
			// A name without the -AD-<N> suffix has no zone ID: it is not used.
			d.log.Info("an availability domain has no number in its name and is skipped", "region", region, "name", full)
			continue
		}
		out = append(out, availabilityDomain{Full: full, Name: ADName(full), ZoneID: id})
	}
	slices.SortFunc(out, func(a, b availabilityDomain) int { return strings.Compare(a.ZoneID, b.ZoneID) })
	d.mu.Lock()
	d.ads[region] = set(out, d.clock.Now().Add(factsTTL))
	d.mu.Unlock()
	return out, nil
}

// Zones implements vendors.Driver: the availability domains of the region,
// Zone{ID: <region>-ad-<N>, Name: the name without the tenancy prefix}.
func (d *Driver) Zones(ctx context.Context, region string) ([]vendors.Zone, error) {
	ads, err := d.domains(ctx, region)
	if err != nil {
		return nil, err
	}
	out := make([]vendors.Zone, 0, len(ads))
	for _, a := range ads {
		out = append(out, vendors.Zone{ID: a.ZoneID, Name: a.Name})
	}
	return out, nil
}

// adOf finds the availability domain of a zone name in the region; the full name
// goes in the call.
func (d *Driver) adOf(ctx context.Context, region, zone string) (availabilityDomain, error) {
	ads, err := d.domains(ctx, region)
	if err != nil {
		return availabilityDomain{}, err
	}
	for _, a := range ads {
		if a.Name == zone {
			return a, nil
		}
	}
	return availabilityDomain{}, vendors.NewError(vendors.Unsupported, codeNoSuchZone,
		fmt.Errorf("the tenancy has no availability domain %q in %s", zone, region))
}

var (
	_ vendors.Driver        = (*Driver)(nil)
	_ vendors.QuotaReporter = (*Driver)(nil)
)
