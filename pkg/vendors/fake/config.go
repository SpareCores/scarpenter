//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// The keys of the steering ConfigMap.
const (
	keyErrors      = "errors"
	keyQuota       = "quota"
	keyLaunchDelay = "launchDelay"
	keyOrphans     = "orphans"
	// keyPowerOff lists NodeClaims whose instances are powered off once (as
	// PowerOff does) and lose their Node, the way a stopped node goes away.
	keyPowerOff = "powerOff"
	// keyUnsupported lists profiles that instance types cannot host, and
	// keyExisting the resources the account had before SCarpenter (what `use`
	// references and the vendor's default resources resolve to).
	keyUnsupported = "unsupported"
	keyExisting    = "existingResources"
	// keyImages lists the boot images the account has (id, region or "*", name,
	// arch, channel, version, tags, created, ...): what the `images` selector and
	// the version check resolve against.
	keyImages = "images"
	// keyRootDisks lists instance types whose root disk the type defines
	// (RootDiskForType), as a vendor with fixed plan disks does.
	keyRootDisks = "rootDisks"
	// keyBilling sets the vendor's billing rule (BillingDescriber) and the
	// billing-start stamp new instances get (billingConfig).
	keyBilling = "billing"
)

// billingConfig is the fake vendor's billing rule, so that the L2 and L3 lanes
// can impersonate an hourly vendor (or one with a minimum) with periods of
// minutes. Instances launched before a change keep the stamp they got.
type billingConfig struct {
	Increment      *metav1.Duration `json:"increment,omitempty"`
	Minimum        *metav1.Duration `json:"minimum,omitempty"`
	StoppedCharged *bool            `json:"stoppedCharged,omitempty"`
	DeleteLatency  *metav1.Duration `json:"deleteLatency,omitempty"`
	// Verified makes the rule count (an unverified one keeps retention off).
	Verified bool   `json:"verified,omitempty"`
	Source   string `json:"source,omitempty"`
	// StampOffset moves the billing start from the creation time: negative is
	// the early stamp of a vendor that stamps before the billed state.
	StampOffset *metav1.Duration `json:"stampOffset,omitempty"`
	// NoStamp makes the driver record no billing start (it stays zero).
	NoStamp bool `json:"noStamp,omitempty"`
}

// rootDiskRule gives the matching types a fixed root disk; empty lists match
// everything.
type rootDiskRule struct {
	InstanceTypes []string          `json:"instanceTypes,omitempty"`
	Families      []string          `json:"families,omitempty"`
	Size          resource.Quantity `json:"size"`
}

// unsupportedRule says that a profile is not valid for the matching types and
// zones (Supported leaves it out): a profile kind and name, with the instance
// types (API references or catalog names), families and zones it covers; empty
// lists match everything.
type unsupportedRule struct {
	Kind          string   `json:"kind"`
	Profile       string   `json:"profile"`
	InstanceTypes []string `json:"instanceTypes,omitempty"`
	Families      []string `json:"families,omitempty"`
	Zones         []string `json:"zones,omitempty"`
}

// errorRule makes matching CreateInstance calls fail with an error class.
type errorRule struct {
	Match struct {
		InstanceType string `json:"instanceType,omitempty"`
		Region       string `json:"region,omitempty"`
		Zone         string `json:"zone,omitempty"`
		CapacityType string `json:"capacityType,omitempty"`
	} `json:"match"`
	Error vendors.Class `json:"error"`
	// AccountWide marks a QuotaExceeded error as account-wide (vendors.Error).
	AccountWide bool `json:"accountWide,omitempty"`
	// Count is the number of calls that fail; 0 means every matching call.
	Count int `json:"count,omitempty"`
}

type regionOverride struct {
	Limit       *int64 `json:"limit,omitempty"`
	ForeignUsed *int64 `json:"foreignUsed,omitempty"`
}

type bucketConfig struct {
	ID          string                    `json:"id"`
	Scope       vendors.QuotaScope        `json:"scope,omitempty"`
	Unit        string                    `json:"unit,omitempty"`
	Limit       int64                     `json:"limit"`
	ForeignUsed int64                     `json:"foreignUsed,omitempty"`
	Regions     map[string]regionOverride `json:"regions,omitempty"`
}

type consumptionRule struct {
	Families      []string `json:"families,omitempty"`
	CapacityTypes []string `json:"capacityTypes,omitempty"`
	Buckets       []string `json:"buckets"`
}

type notLaunchable struct {
	Region string   `json:"region"`
	Zone   string   `json:"zone,omitempty"`
	Types  []string `json:"types"`
}

type quotaConfig struct {
	Buckets       []bucketConfig    `json:"buckets,omitempty"`
	Consumption   []consumptionRule `json:"consumption,omitempty"`
	NotLaunchable []notLaunchable   `json:"notLaunchable,omitempty"`
}

type orphanConfig struct {
	Name         string `json:"name"`
	Region       string `json:"region"`
	Zone         string `json:"zone"`
	InstanceType string `json:"instanceType"`
	CapacityType string `json:"capacityType"`
	AgeSeconds   int64  `json:"ageSeconds"`
	// PoweredOff creates the instance stopped (reported terminating), the case
	// of the terminating grace.
	PoweredOff bool `json:"poweredOff,omitempty"`
}

// steeringKeys are the keys of the steering ConfigMap.
var steeringKeys = []string{
	keyErrors, keyQuota, keyLaunchDelay, keyOrphans, keyPowerOff, keyUnsupported, keyExisting, keyRootDisks, keyImages,
	keyBilling,
}

// orEmpty is c, or an empty configuration when there is none yet.
func orEmpty(c *config) *config {
	if c == nil {
		return &config{}
	}
	return c
}

// config is the parsed steering ConfigMap. Every key is parsed on its own, so a
// broken one keeps its last good value.
type config struct {
	errors      []errorRule
	errorsRaw   string
	quota       *quotaConfig
	launchDelay *time.Duration
	orphans     []orphanConfig
	powerOff    []string
	rules       []unsupportedRule
	rootDisks   []rootDiskRule
	existing    []existingResource
	images      []existingResource
	billing     *billingConfig
	raw         map[string]string
}

// unsupported reports whether a rule makes the profile invalid for the type in
// the zone.
func (c *config) unsupported(kind, profile string, it vendors.CatalogType, zone string) bool {
	for _, r := range c.rules {
		if r.Kind != kind || r.Profile != profile {
			continue
		}
		if len(r.InstanceTypes) > 0 && !slices.ContainsFunc(r.InstanceTypes, func(t string) bool {
			return strings.TrimPrefix(t, it.Vendor+".") == it.APIReference
		}) {
			continue
		}
		if len(r.Families) > 0 && !slices.Contains(r.Families, it.Family) {
			continue
		}
		if len(r.Zones) > 0 && !slices.Contains(r.Zones, zone) {
			continue
		}
		return true
	}
	return false
}

// rootDisk is the root disk the first matching rule gives the type.
func (c *config) rootDisk(it vendors.CatalogType) (int64, bool) {
	for _, r := range c.rootDisks {
		if len(r.InstanceTypes) > 0 && !slices.ContainsFunc(r.InstanceTypes, func(t string) bool {
			return strings.TrimPrefix(t, it.Vendor+".") == it.APIReference
		}) {
			continue
		}
		if len(r.Families) > 0 && !slices.Contains(r.Families, it.Family) {
			continue
		}
		return r.Size.Value(), true
	}
	return 0, false
}

// refreshConfig re-reads the steering ConfigMap unless it was read within the
// cache TTL. A missing ConfigMap means an empty configuration; a key that does
// not parse is logged once and keeps its last good value.
func (d *Driver) refreshConfig(ctx context.Context) { d.reloadConfig(ctx, false) }

// reloadConfig is refreshConfig; force ignores the cache TTL. CreateInstance
// forces, so a rule a test wrote just before it creates a Pod is in effect for
// the launch (one GET per launch is fine for a development driver).
func (d *Driver) reloadConfig(ctx context.Context, force bool) {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	now := d.clock.Now()
	if !force && d.cfgLoaded && now.Sub(d.cfgAt) < d.opts.ConfigTTL {
		return
	}
	d.cfgAt, d.cfgLoaded = now, true
	reader := d.cfg.Reader
	if reader == nil {
		reader = d.cfg.Client
	}
	if reader == nil {
		return
	}
	cm := &corev1.ConfigMap{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: d.opts.ConfigNamespace, Name: d.opts.ConfigName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{}
	case err != nil:
		d.cfg.Log.Error(err, "reading the fake driver's ConfigMap, keeping the last one")
		return
	}
	next := &config{raw: map[string]string{}}
	prev := d.conf
	for _, key := range steeringKeys {
		next.raw[key] = cm.Data[key]
	}
	parse := func(key string, apply func(string) error, keep func()) {
		raw := cm.Data[key]
		if prev != nil && prev.raw[key] == raw {
			keep()
			return
		}
		if err := apply(raw); err != nil {
			d.cfg.Log.Error(err, "the fake driver's ConfigMap key does not parse, keeping the last good value", "key", key)
			keep()
		}
	}
	parse(keyErrors, func(raw string) error {
		var rules []errorRule
		if err := yaml.UnmarshalStrict([]byte(raw), &rules); err != nil {
			return err
		}
		for _, r := range rules {
			if !r.Error.Valid() {
				return fmt.Errorf("unknown error class %q", r.Error)
			}
		}
		next.errors, next.errorsRaw = rules, raw
		d.mu.Lock()
		d.ruleHits = map[int]int{}
		d.mu.Unlock()
		return nil
	}, func() {
		if prev != nil {
			next.errors, next.errorsRaw = prev.errors, prev.errorsRaw
		}
	})
	parse(keyQuota, func(raw string) error {
		if strings.TrimSpace(raw) == "" {
			next.quota = nil
			return nil
		}
		q := &quotaConfig{}
		if err := yaml.UnmarshalStrict([]byte(raw), q); err != nil {
			return err
		}
		next.quota = q
		return nil
	}, func() {
		if prev != nil {
			next.quota = prev.quota
		}
	})
	parse(keyLaunchDelay, func(raw string) error {
		if strings.TrimSpace(raw) == "" {
			next.launchDelay = nil
			return nil
		}
		v, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		next.launchDelay = &v
		return nil
	}, func() {
		if prev != nil {
			next.launchDelay = prev.launchDelay
		}
	})
	parse(keyOrphans, func(raw string) error {
		var o []orphanConfig
		if err := yaml.UnmarshalStrict([]byte(raw), &o); err != nil {
			return err
		}
		next.orphans = o
		return nil
	}, func() {
		if prev != nil {
			next.orphans = prev.orphans
		}
	})
	parse(keyPowerOff, func(raw string) error { return yaml.UnmarshalStrict([]byte(raw), &next.powerOff) },
		func() { next.powerOff = orEmpty(prev).powerOff })
	d.parseProfileRules(parse, next, prev)
	parseBilling(parse, next, prev)
	parse(keyExisting, func(raw string) error {
		var list []existingResource
		if err := yaml.UnmarshalStrict([]byte(raw), &list); err != nil {
			return err
		}
		next.existing = list
		return nil
	}, func() {
		if prev != nil {
			next.existing = prev.existing
		}
	})
	parse(keyImages, func(raw string) error {
		var list []existingResource
		if err := yaml.UnmarshalStrict([]byte(raw), &list); err != nil {
			return err
		}
		for i := range list {
			list[i].Kind = string(KindImages)
		}
		next.images = list
		return nil
	}, func() {
		if prev != nil {
			next.images = prev.images
		}
	})
	d.conf = next
	d.createOrphans(ctx, next.orphans)
	d.powerOffClaims(ctx, next.powerOff)
}

// parseProfileRules parses the keys that change what the catalog says about a
// type (the unsupported profiles and the root disks): the catalog rebuilds when
// they do.
func (d *Driver) parseProfileRules(
	parse func(key string, apply func(string) error, keep func()), next, prev *config,
) {
	parse(keyUnsupported, func(raw string) error {
		var rules []unsupportedRule
		if err := yaml.UnmarshalStrict([]byte(raw), &rules); err != nil {
			return err
		}
		next.rules = rules
		if prev == nil || prev.raw[keyUnsupported] != raw {
			d.unsupportedEpoch.Add(1)
		}
		return nil
	}, func() {
		if prev != nil {
			next.rules = prev.rules
		}
	})
	parse(keyRootDisks, func(raw string) error {
		var rules []rootDiskRule
		if err := yaml.UnmarshalStrict([]byte(raw), &rules); err != nil {
			return err
		}
		next.rootDisks = rules
		if prev == nil || prev.raw[keyRootDisks] != raw {
			d.unsupportedEpoch.Add(1)
		}
		return nil
	}, func() {
		if prev != nil {
			next.rootDisks = prev.rootDisks
		}
	})
}

// current returns the last parsed configuration (never nil).
func (d *Driver) current() *config {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	if d.conf == nil {
		return &config{raw: map[string]string{}}
	}
	return d.conf
}

func matchOne(want, got string) bool { return want == "" || want == got }

// matchRule returns the index of the first rule matching the request.
func (c *config) matchRule(prefix string, req vendors.CreateRequest) (int, bool) {
	for i := range c.errors {
		m := &c.errors[i].Match
		typ := strings.TrimPrefix(m.InstanceType, prefix)
		if matchOne(typ, req.InstanceType) && matchOne(m.Region, req.Region) &&
			matchOne(m.Zone, req.Zone) && matchOne(m.CapacityType, req.CapacityType) {
			return i, true
		}
	}
	return 0, false
}

// consumes returns the buckets a type consumes (first matching rule).
func (c *config) consumes(family, capacityType string) ([]string, bool) {
	if c.quota == nil {
		return nil, false
	}
	for _, r := range c.quota.Consumption {
		if (len(r.Families) == 0 || slices.Contains(r.Families, "*") || slices.Contains(r.Families, family)) &&
			(len(r.CapacityTypes) == 0 || slices.Contains(r.CapacityTypes, "*") ||
				slices.Contains(r.CapacityTypes, capacityType)) {
			return r.Buckets, true
		}
	}
	return nil, false
}

// bucket returns the bucket definition resolved for a region.
func (c *config) bucket(id, region string) (b vendors.Bucket, ok bool) {
	if c.quota == nil {
		return b, false
	}
	for _, bc := range c.quota.Buckets {
		if bc.ID != id {
			continue
		}
		scope := bc.Scope
		if scope == "" {
			scope = vendors.QuotaScopeRegion
		}
		b = vendors.Bucket{ID: bc.ID, Scope: scope, Unit: bc.Unit, Limit: bc.Limit, Used: bc.ForeignUsed}
		if o, has := bc.Regions[region]; has && scope == vendors.QuotaScopeRegion {
			if o.Limit != nil {
				b.Limit = *o.Limit
			}
			if o.ForeignUsed != nil {
				b.Used = *o.ForeignUsed
			}
		}
		return b, true
	}
	return b, false
}

// parseBilling parses the billing key.
func parseBilling(parse func(key string, apply func(string) error, keep func()), next, prev *config) {
	parse(keyBilling, func(raw string) error {
		if strings.TrimSpace(raw) == "" {
			next.billing = nil
			return nil
		}
		b := &billingConfig{}
		if err := yaml.UnmarshalStrict([]byte(raw), b); err != nil {
			return err
		}
		next.billing = b
		return nil
	}, func() {
		if prev != nil {
			next.billing = prev.billing
		}
	})
}
