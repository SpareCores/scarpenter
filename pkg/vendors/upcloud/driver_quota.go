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
	return SnapshotOf(ctx, d.api, limits)
}

// Consumption implements vendors.QuotaReporter: a launch of the plan takes its
// cores, its memory in MB, one public IPv4 address (none on Cloud Native plans,
// where the address is billed apart) and the plan's default root disk on its
// tier. A NodeClass's own root size is not known here, so the disk bucket is the
// default one. A plan that is not in the table (not read yet, GPU, spot) is
// unknown and the reactive path applies.
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
// list that the driver launches, in the zone (UpCloud has no stock API, and the
// plan list is account-wide: Keeper's price list decides where a plan is offered,
// and a launch that disagrees is Unsupported). GPU plans and families the driver
// has not classified are left out. Reading the plans also fills the table the
// quota consumption and the prices need.
func (d *Driver) LaunchableTypes(ctx context.Context, region string) (map[string][]string, error) {
	if !d.knownRegion(ctx, region) {
		return nil, vendors.Errorf(vendors.RegionUnavailable, "the account cannot use the zone %q", region)
	}
	plans, err := d.planTable(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for name, p := range plans {
		if p.Supported() {
			out[name] = []string{region}
		}
	}
	return out, nil
}
