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
	DefaultQuotaExceededTTL        = 30 * time.Minute
	DefaultZoneUnusableTTL         = 10 * time.Minute
)

// TTLs are the lifetimes of the entries per error class. A zero field uses the
// default.
type TTLs struct {
	InsufficientCapacity time.Duration
	Unsupported          time.Duration
	QuotaExceeded        time.Duration
	ZoneUnusable         time.Duration
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
	return t
}

type class int

const (
	classInsufficientCapacity class = iota
	classUnsupported
	classQuotaExceeded
	classZoneUnusable
)

// key identifies an entry; fields that a class does not use stay empty.
type key struct {
	class        class
	vendor       string
	instanceType string
	region       string
	zone         string
	capacityType string
}

// Cache holds the unavailable offerings. It is safe for concurrent use.
type Cache struct {
	clock clock.Clock
	ttls  TTLs

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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[k] = c.clock.Now().Add(ttl)
	c.sequence.Add(1)
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

// MarkQuotaExceeded records that the region has no quota left for the capacity
// type. It is for vendors without a quota API; vendors with one trigger a quota
// refresh instead (overview section 5.9).
func (c *Cache) MarkQuotaExceeded(vendor, region, capacityType string) {
	c.mark(key{class: classQuotaExceeded, vendor: vendor, region: region, capacityType: capacityType},
		c.ttls.QuotaExceeded)
}

// MarkZoneUnusable records that the zone cannot be used at all (no subnet, no
// free addresses).
func (c *Cache) MarkZoneUnusable(vendor, zone string) {
	c.mark(key{class: classZoneUnusable, vendor: vendor, zone: zone}, c.ttls.ZoneUnusable)
}

// Offering identifies an offering the way the catalog builds it.
type Offering struct {
	Vendor       string
	InstanceType string
	Region       string
	Zone         string
	CapacityType string
}

// IsUnavailable reports whether any live entry covers the offering.
func (c *Cache) IsUnavailable(o Offering) bool {
	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range [...]key{
		{
			class: classInsufficientCapacity, vendor: o.Vendor, instanceType: o.InstanceType, zone: o.Zone,
			capacityType: o.CapacityType,
		},
		{class: classUnsupported, vendor: o.Vendor, instanceType: o.InstanceType, zone: o.Zone},
		{class: classQuotaExceeded, vendor: o.Vendor, region: o.Region, capacityType: o.CapacityType},
		{class: classZoneUnusable, vendor: o.Vendor, zone: o.Zone},
	} {
		if exp, ok := c.entries[k]; ok && now.Before(exp) {
			return true
		}
	}
	return false
}

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
