/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"sync"
	"time"

	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

// Quota buckets per region (plan 18, "Quotas"): OVH enforces per-project,
// per-region limits on instances, cores and RAM, which Nova reports as absolute
// limits. The usage is the whole project, so foreign servers count (the
// contract's rule). GPU flavors have a separate quota and are not launched.
const (
	BucketInstances = "instances"
	BucketCores     = "cores"
	BucketRAM       = "ram"
	UnitInstances   = "instances"
	UnitVCPU        = "vCPU"
	UnitMiB         = "MiB"
)

// flavorTTL is how long the driver reuses a region's flavor list.
const flavorTTL = time.Hour

// Snapshot turns Nova's absolute limits and the region's servers into the
// region's quota snapshot: the usage is Nova's (the whole project); Counted are
// the references of the servers that are not DELETED, so the in-flight ledger can
// release the reservations of instances the usage now includes.
func Snapshot(region string, l osapi.Limits, servers []osapi.Server) *vendors.QuotaSnapshot {
	snap := &vendors.QuotaSnapshot{Buckets: []vendors.Bucket{
		{ID: BucketInstances, Scope: vendors.QuotaScopeRegion, Unit: UnitInstances,
			Limit: l.MaxInstances, Used: l.UsedInstances},
		{ID: BucketCores, Scope: vendors.QuotaScopeRegion, Unit: UnitVCPU, Limit: l.MaxCores, Used: l.UsedCores},
		{ID: BucketRAM, Scope: vendors.QuotaScopeRegion, Unit: UnitMiB, Limit: l.MaxRAMMiB, Used: l.UsedRAMMiB},
	}}
	for _, s := range servers {
		if InstanceState(s.Status) != vendors.StateTerminated {
			snap.Counted = append(snap.Counted, InstanceRef(region, s.ID))
		}
	}
	return snap
}

// FlavorConsumption is what one instance of the flavor consumes: 1 instance, its
// vCPUs, its RAM in MiB.
func FlavorConsumption(f osapi.Flavor) []vendors.Consumption {
	return []vendors.Consumption{
		{Bucket: BucketInstances, Amount: 1},
		{Bucket: BucketCores, Amount: f.VCPUs},
		{Bucket: BucketRAM, Amount: f.RAMMiB},
	}
}

// QuotaReader implements vendors.QuotaReporter on a provider. The region
// arguments are API region names. Nova returns limits and usage in one call, so
// the "reuse cached limits" allowance of the contract is not needed: every call
// reads both. The flavor facts (vCPUs, RAM) that Consumption needs are read once
// per region and cached for an hour; flavor names are the same in every region.
type QuotaReader struct {
	provider osapi.Provider
	clock    clock.Clock

	mu      sync.Mutex
	flavors map[string]flavorCache // region
	byName  map[string]osapi.Flavor
}

type flavorCache struct {
	at time.Time
}

// NewQuotaReader returns a reader; a nil clock is the real one.
func NewQuotaReader(p osapi.Provider, c clock.Clock) *QuotaReader {
	if c == nil {
		c = clock.RealClock{}
	}
	return &QuotaReader{provider: p, clock: c, flavors: map[string]flavorCache{}, byName: map[string]osapi.Flavor{}}
}

// LoadFlavors reads (or refreshes, after the TTL) the region's flavors into the
// type table Consumption uses.
func (q *QuotaReader) LoadFlavors(ctx context.Context, region string) error {
	q.mu.Lock()
	fc, ok := q.flavors[region]
	q.mu.Unlock()
	if ok && q.clock.Since(fc.at) < flavorTTL {
		return nil
	}
	cl, err := q.provider.Clients(ctx, region)
	if err != nil {
		return Classify(err)
	}
	fl, err := cl.Compute.ListFlavors(ctx)
	if err != nil {
		return Classify(err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, f := range fl {
		q.byName[f.Name] = f
	}
	q.flavors[region] = flavorCache{at: q.clock.Now()}
	return nil
}

// Quotas implements vendors.QuotaReporter.
func (q *QuotaReader) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	cl, err := q.provider.Clients(ctx, region)
	if err != nil {
		return nil, Classify(err)
	}
	l, err := cl.Compute.GetLimits(ctx)
	if err != nil {
		return nil, Classify(err)
	}
	servers, err := cl.Compute.ListServers(ctx, osapi.ServerFilter{})
	if err != nil {
		return nil, Classify(err)
	}
	if err := q.LoadFlavors(ctx, region); err != nil {
		return nil, err
	}
	return Snapshot(region, *l, servers), nil
}

// GlobalQuotas implements vendors.QuotaReporter: every OVH limit is per region.
func (q *QuotaReader) GlobalQuotas(context.Context) (*vendors.QuotaSnapshot, error) {
	return &vendors.QuotaSnapshot{}, nil
}

// Consumption implements vendors.QuotaReporter. Spot does not exist on OVH, so
// the answer for it is false; so it is for a type whose flavor has not been read
// yet (the reactive path applies) and for a family the driver does not launch.
func (q *QuotaReader) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	if capacityType != vendors.CapacityOnDemand {
		return nil, false
	}
	if ok, _ := Launchable(instanceType); !ok {
		return nil, false
	}
	q.mu.Lock()
	f, ok := q.byName[instanceType]
	q.mu.Unlock()
	if !ok || f.VCPUs <= 0 {
		return nil, false
	}
	return FlavorConsumption(f), true
}

var _ vendors.QuotaReporter = (*QuotaReader)(nil)
