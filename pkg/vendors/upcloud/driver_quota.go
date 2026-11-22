/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// limitsTTL is how long the driver reuses the account limits when the caller does
// not ask for fresh ones.
const limitsTTL = 15 * time.Minute

type limitsSnapshot = upcloud.ResourceLimits

// Quotas implements vendors.QuotaReporter: every limit of UpCloud is the
// account's, so the buckets are global and a region has none.
func (d *Driver) Quotas(context.Context, string) (*vendors.QuotaSnapshot, error) {
	return &vendors.QuotaSnapshot{}, nil
}

// GlobalQuotas implements vendors.QuotaReporter: the account's resource limits
// (GET /account, cached for 15 minutes unless the caller asks for fresh ones)
// against the usage computed from the lists of servers, storages, addresses and
// networks: the account call has no usage. A bucket is reported only when its limit
// is positive.
func (d *Driver) GlobalQuotas(ctx context.Context) (*vendors.QuotaSnapshot, error) {
	d.mu.Lock()
	limits, ok := d.limits.get(d.clock.Now())
	d.mu.Unlock()
	if !ok || vendors.QuotaRefreshFrom(ctx).Limits {
		l, err := ReadLimits(ctx, d.api)
		if err != nil {
			return nil, err
		}
		limits = l
		d.mu.Lock()
		d.limits.set(l, d.clock.Now().Add(limitsTTL))
		d.mu.Unlock()
	}
	// The plan list (cached by planTable) is needed only to count the GPUs of an account that has a GPU limit.
	var plans Plans
	if limits.GPUs > 0 {
		var err error
		if plans, err = d.planTable(ctx); err != nil {
			return nil, err
		}
	}
	return SnapshotOf(ctx, d.api, limits, plans)
}

// Consumption implements vendors.QuotaReporter: a launch of the plan takes its
// cores, its memory in MB, one public IPv4 address (none on Cloud Native plans,
// where the address is billed apart) and the plan's default root disk on its
// tier. A NodeClass's own root size is not known here, so the disk bucket is the
// default one, and its GPUs. A plan that is not in the table (not read yet, spot)
// is unknown and the reactive path applies.
func (d *Driver) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	if capacityType != vendors.CapacityOnDemand {
		return nil, false
	}
	p, ok := d.knownPlans()[instanceType]
	if !ok || !p.Supported() {
		return nil, false
	}
	return Consumption(p, Root{Tier: p.DefaultRootTier(), SizeGB: p.DefaultRootGB()}, true), true
}

// LaunchableTypes implements vendors.OfferingLister: every plan of the account's
// list that the driver launches, in the zone (the plan list is account-wide:
// Keeper's price list decides where a plan is offered, and a launch that disagrees
// is Unsupported). A GPU plan is listed only where the device stock
// (GetDevicesAvailability, cached for a minute) has it; when that call fails the last
// good stock is used, and without one the GPU plans are left out (the other plans are
// not affected either way).
// Spot GPU plans and families the driver has not classified are left out. Reading
// the plans also fills the table the quota consumption and the prices need.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	if !d.knownRegion(ctx, region) {
		return nil, vendors.Errorf(vendors.RegionUnavailable, "the account cannot use the zone %q", region)
	}
	plans, err := d.planTable(ctx)
	if err != nil {
		return nil, err
	}
	stock, err := d.gpuStock(ctx)
	if err != nil {
		// V(1): an account without GPU access gets an error here at every refresh.
		d.log.V(1).Info("reading the GPU stock failed; the GPU plans of the last good read stay, none without one",
			"zone", region, "error", err.Error())
	}
	out := map[string][]string{}
	for name, p := range plans {
		if p.Supported() && (p.GPUs == 0 || stock[region][name]) {
			out[name] = []string{region}
		}
	}
	return out, nil
}

// gpuStock returns the GPU plans in stock per zone (GET /device/availability answers
// for every zone at once, cached for a minute). A failed call returns the last good
// answer with the error, nil without one.
func (d *Driver) gpuStock(ctx context.Context) (map[string]map[string]bool, error) {
	d.mu.Lock()
	if v, ok := d.stock.get(d.clock.Now()); ok {
		d.mu.Unlock()
		return v, nil
	}
	stale := d.stock
	d.mu.Unlock()
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	avail, err := d.api.GetDevicesAvailability(cctx)
	if err != nil {
		return stale.v, Classify(err)
	}
	stock := map[string]map[string]bool{}
	for zone, devices := range *avail {
		stock[zone] = map[string]bool{}
		for plan, a := range devices.GPUPlans {
			stock[zone][plan] = a.Amount > 0
		}
	}
	d.mu.Lock()
	d.stock.set(stock, d.clock.Now().Add(stockTTL))
	d.mu.Unlock()
	return stock, nil
}
