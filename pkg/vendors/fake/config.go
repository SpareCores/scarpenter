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
	// keyUnsupported lists profiles that instance types cannot host, and
	// keyExisting the resources the account had before SCarpenter (what `use`
	// references and the vendor's default resources resolve to).
	keyUnsupported = "unsupported"
	keyExisting    = "existingResources"
)

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

// config is the parsed steering ConfigMap. Every key is parsed on its own, so a
// broken one keeps its last good value.
type config struct {
	errors      []errorRule
	errorsRaw   string
	quota       *quotaConfig
	launchDelay *time.Duration
	orphans     []orphanConfig
	rules       []unsupportedRule
	existing    []existingResource
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
	for _, key := range []string{keyErrors, keyQuota, keyLaunchDelay, keyOrphans, keyUnsupported, keyExisting} {
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
	d.conf = next
	d.createOrphans(ctx, next.orphans)
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
