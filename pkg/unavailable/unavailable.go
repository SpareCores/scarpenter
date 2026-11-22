/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package unavailable is the cache of offerings that recently failed to launch
// (overview section 5.6). Drivers' error classes decide what is marked; the
// catalog asks the cache when it builds offerings, and uses Sequence as part of
// its cache key so that the next scheduling round sees a change.
//
// The TTL defaults follow the AWS provider's unavailable-offerings cache
// (pkg/cache/cache.go of aws/karpenter-provider-aws at commit
// 30c236cafacb3a23fb3a50370d09d71750b56b29, Apache-2.0): three minutes for
// insufficient capacity. The other TTLs are SCarpenter's own (overview 5.6).
// The interrupted class is fed by the interruption controller, not by a launch.
package unavailable

import (
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/utils/clock"
)

// Default TTLs per error class (overview section 5.6).
const (
	DefaultInsufficientCapacityTTL = 3 * time.Minute
	DefaultUnsupportedTTL          = 24 * time.Hour
	DefaultQuotaExceededTTL        = 5 * time.Minute
	DefaultZoneUnusableTTL         = 10 * time.Minute
	// DefaultInvalidTTL: the vendor rejected the offering's launch parameters;
	// retrying the same request cannot succeed, only an operator change can.
	DefaultInvalidTTL = time.Hour
	// DefaultInterruptedTTL: a spot type that was just reclaimed is likely to be
	// reclaimed again (ADR-027).
	DefaultInterruptedTTL = 10 * time.Minute
)

// TTLs are the lifetimes of the entries per error class. A zero field uses the
// default.
type TTLs struct {
	InsufficientCapacity time.Duration
	Unsupported          time.Duration
	QuotaExceeded        time.Duration
	ZoneUnusable         time.Duration
	Invalid              time.Duration
	Interrupted          time.Duration
}

func (t TTLs) withDefaults() TTLs {
	if t.InsufficientCapacity <= 0 {
		t.InsufficientCapacity = DefaultInsufficientCapacityTTL
	}
	if t.Unsupported <= 0 {
		t.Unsupported = DefaultUnsupportedTTL
	}
	if t.QuotaExceeded <= 0 {
		t.QuotaExceeded = DefaultQuotaExceededTTL
	}
	if t.ZoneUnusable <= 0 {
		t.ZoneUnusable = DefaultZoneUnusableTTL
	}
	if t.Invalid <= 0 {
		t.Invalid = DefaultInvalidTTL
	}
	if t.Interrupted <= 0 {
		t.Interrupted = DefaultInterruptedTTL
	}
	return t
}

type class int

const (
	classInsufficientCapacity class = iota
	classUnsupported
	classQuotaExceeded
	classZoneUnusable
	classInterrupted
	classInvalid
	classDailyLimit
)

func (c class) String() string {
	return [...]string{
		"InsufficientCapacity", "Unsupported", "QuotaExceeded", "ZoneUnusable", "Interrupted", "InvalidOffering",
		"DailyLimit",
	}[c]
}

// key identifies an entry; fields that a class does not use stay empty.
type key struct {
	class        class
	vendor       string
	instanceType string
	region       string
	zone         string
	capacityType string
	// bucket is the quota bucket of a QuotaExceeded entry of one bucket.
	bucket string
}

// Cache holds the unavailable offerings. It is safe for concurrent use.
type Cache struct {
	clock clock.Clock
	ttls  TTLs

	// bucketsOf maps an instance type to the quota buckets it consumes; nil when
	// no quota tracker is wired in.
	bucketsOf func(vendor, apiReference, capacityType string) []string

	mu      sync.Mutex
	entries map[key]time.Time // key -> expiry
	// sequence changes whenever an entry is added or expires.
	sequence atomic.Uint64
}

// New returns an empty cache. A nil clock uses the real one.
func New(clk clock.Clock, ttls TTLs) *Cache {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &Cache{clock: clk, ttls: ttls.withDefaults(), entries: map[key]time.Time{}}
}

func (c *Cache) mark(k key, ttl time.Duration) {
	c.markUntil(k, c.clock.Now().Add(ttl))
}

func (c *Cache) markUntil(k key, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[k] = expiry
	c.sequence.Add(1)
}

// SetBucketsOf sets how the cache maps an instance type to the quota buckets it
// consumes (the quota tracker's mapping), so that an entry of a bucket covers
// the offerings that consume it.
func (c *Cache) SetBucketsOf(f func(vendor, apiReference, capacityType string) []string) {
	c.bucketsOf = f
}

// MarkInsufficientCapacity records that the vendor had no capacity for the
// instance type in the zone with the capacity type.
func (c *Cache) MarkInsufficientCapacity(vendor, instanceType, zone, capacityType string) {
	c.mark(key{
		class: classInsufficientCapacity, vendor: vendor, instanceType: instanceType, zone: zone, capacityType: capacityType,
	}, c.ttls.InsufficientCapacity)
}

// MarkUnsupported records that the instance type is not offered in the zone (for
// either capacity type).
func (c *Cache) MarkUnsupported(vendor, instanceType, zone string) {
	c.mark(key{class: classUnsupported, vendor: vendor, instanceType: instanceType, zone: zone}, c.ttls.Unsupported)
}

// MarkUnsupportedPreLaunch is MarkUnsupported for a failure of the driver's own
// check before any vendor call: it is deterministic from tables the driver may
// have read while a region was briefly unreadable, so the mark is as short as a
// zone-unusable one.
func (c *Cache) MarkUnsupportedPreLaunch(vendor, instanceType, zone string) {
	c.mark(key{class: classUnsupported, vendor: vendor, instanceType: instanceType, zone: zone}, c.ttls.ZoneUnusable)
}

// MarkQuotaExceeded records that the region has no quota left for the capacity
// type. It is for vendors without a quota API; vendors with one trigger a quota
// refresh instead (overview section 5.9).
func (c *Cache) MarkQuotaExceeded(vendor, region, capacityType string) {
	c.mark(key{class: classQuotaExceeded, vendor: vendor, region: region, capacityType: capacityType},
		c.ttls.QuotaExceeded)
}

// MarkQuotaBucket records that the quota bucket the vendor counts a launch
// against has no room left in the region. It covers the offerings that consume
// the bucket, for either capacity type.
func (c *Cache) MarkQuotaBucket(vendor, region, bucket string) {
	c.mark(key{class: classQuotaExceeded, vendor: vendor, region: region, bucket: bucket}, c.ttls.QuotaExceeded)
}

// ClearQuotaExceeded drops the QuotaExceeded entries of the vendor's region and
// the account-wide ones of the vendor: a NodeClaim of the region was deleted, so
// quota may be free again.
func (c *Cache) ClearQuotaExceeded(vendor, region string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cleared := false
	for k := range c.entries {
		if k.class == classQuotaExceeded && k.vendor == vendor && (k.region == region || k.region == "") {
			delete(c.entries, k)
			cleared = true
		}
	}
	if cleared {
		c.sequence.Add(1)
	}
}

// MarkDailyLimit records that the vendor refuses to create resources in the
// region until the limit resets. Deleting nodes does not clear it.
func (c *Cache) MarkDailyLimit(vendor, region string, until time.Time) {
	c.markUntil(key{class: classDailyLimit, vendor: vendor, region: region}, until)
}

// MarkQuotaExceededAccountWide records that the vendor's account has no quota
// left for the capacity type, in every region (hcloud project limits, Vultr and
// UpCloud account limits). The entry has no region and matches every one.
func (c *Cache) MarkQuotaExceededAccountWide(vendor, capacityType string) {
	c.mark(key{class: classQuotaExceeded, vendor: vendor, capacityType: capacityType}, c.ttls.QuotaExceeded)
}

// MarkZoneUnusable records that the zone cannot be used at all (no subnet, no
// free addresses).
func (c *Cache) MarkZoneUnusable(vendor, zone string) {
	c.mark(key{class: classZoneUnusable, vendor: vendor, zone: zone}, c.ttls.ZoneUnusable)
}

// MarkInvalid records that the vendor rejected the launch parameters of the
// instance type in the zone with the capacity type.
func (c *Cache) MarkInvalid(vendor, instanceType, zone, capacityType string) {
	c.mark(key{
		class: classInvalid, vendor: vendor, instanceType: instanceType, zone: zone, capacityType: capacityType,
	}, c.ttls.Invalid)
}

// MarkInterrupted records that the vendor reclaimed a node of the instance type
// in the zone with the capacity type (a spot or preemptible interruption). It is
// a class of its own, with its own TTL, but matches like insufficient capacity.
func (c *Cache) MarkInterrupted(vendor, instanceType, zone, capacityType string) {
	c.mark(key{
		class: classInterrupted, vendor: vendor, instanceType: instanceType, zone: zone, capacityType: capacityType,
	}, c.ttls.Interrupted)
}

// Offering identifies an offering the way the catalog builds it.
type Offering struct {
	Vendor string
	// APIReference is the vendor's name of the type, which the quota buckets are
	// mapped by.
	APIReference string
	InstanceType string
	Region       string
	Zone         string
	CapacityType string
}

// IsUnavailable reports whether any live entry covers the offering.
func (c *Cache) IsUnavailable(o Offering) bool {
	_, _, ok := c.Why(o)
	return ok
}

// Why returns the error class and the expiry of the first live entry that
// covers the offering. The class is one of InsufficientCapacity, Interrupted,
// Unsupported, QuotaExceeded, DailyLimit, InvalidOffering and ZoneUnusable.
func (c *Cache) Why(o Offering) (class string, expiry time.Time, ok bool) {
	var buckets []string
	if c.bucketsOf != nil {
		buckets = c.bucketsOf(o.Vendor, o.APIReference, o.CapacityType)
	}
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	live := func(k key) bool {
		exp, found := c.entries[k]
		expiry = exp
		return found && now.Before(exp)
	}
	for _, k := range [...]key{
		{
			class: classInsufficientCapacity, vendor: o.Vendor, instanceType: o.InstanceType, zone: o.Zone,
			capacityType: o.CapacityType,
		},
		{
			class: classInterrupted, vendor: o.Vendor, instanceType: o.InstanceType, zone: o.Zone,
			capacityType: o.CapacityType,
		},
		{
			class: classInvalid, vendor: o.Vendor, instanceType: o.InstanceType, zone: o.Zone,
			capacityType: o.CapacityType,
		},
		{class: classUnsupported, vendor: o.Vendor, instanceType: o.InstanceType, zone: o.Zone},
		{class: classQuotaExceeded, vendor: o.Vendor, region: o.Region, capacityType: o.CapacityType},
		{class: classQuotaExceeded, vendor: o.Vendor, capacityType: o.CapacityType}, // account-wide
		{class: classZoneUnusable, vendor: o.Vendor, zone: o.Zone},
		{class: classDailyLimit, vendor: o.Vendor, region: o.Region},
	} {
		if live(k) {
			return k.class.String(), expiry, true
		}
	}
	for _, b := range buckets {
		if live(key{class: classQuotaExceeded, vendor: o.Vendor, region: o.Region, bucket: b}) {
			return classQuotaExceeded.String(), expiry, true
		}
	}
	return "", time.Time{}, false
}

// Bump changes the sequence without changing the entries. Other components
// whose view feeds offering availability (the quota tracker, overview section
// 5.9) call it, so that the catalog's result cache, which is keyed by the
// sequence, drops what it computed.
func (c *Cache) Bump() { c.sequence.Add(1) }

// Sequence returns a number that changes whenever the set of live entries
// does, including when entries expire. Use it as part of a cache key.
func (c *Cache) Sequence() uint64 {
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, exp := range c.entries {
		if !now.Before(exp) {
			delete(c.entries, k)
			c.sequence.Add(1)
		}
	}
	return c.sequence.Load()
}

// Len returns the number of live entries.
func (c *Cache) Len() int {
	c.Sequence() // drops the expired entries
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
