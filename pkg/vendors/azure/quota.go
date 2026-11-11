/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Quota bucket IDs: the Usage API's names (plan 17 section 5; the names of the
// compute usages are unverified, the canary fails on a lane type that does not
// map).
const (
	BucketRegionalCores   = "cores"
	BucketSpotCores       = "lowPriorityCores"
	BucketPublicIPs       = "StandardPublicIPAddresses"
	familySuffix          = "family"
	unitVCPU              = "vCPU"
	unitAddresses         = "addresses"
	publicIPsPerNodeLimit = 1
)

// QuotaReader reads the quota state of a subscription's regions and maps
// instance types to the buckets they consume. It implements the
// vendors.QuotaReporter methods the driver delegates to.
type QuotaReader struct {
	usages UsageAPI

	mu sync.RWMutex
	// skus holds the size table of every region read so far, by size name:
	// the family and the vCPUs of a size do not depend on the region.
	skus map[string]SKU
}

// NewQuotaReader creates a reader over the usage API.
func NewQuotaReader(usages UsageAPI) *QuotaReader {
	return &QuotaReader{usages: usages, skus: map[string]SKU{}}
}

// LoadSKUs records the size table of a region (from the SKU read), which
// Consumption needs. A size missing from the table has an unknown quota.
func (q *QuotaReader) LoadSKUs(skus map[string]SKU) {
	q.mu.Lock()
	defer q.mu.Unlock()
	maps.Copy(q.skus, skus)
}

// Quotas returns the region's buckets: the regional vCPUs, the Spot vCPUs, the
// family vCPUs and the Standard public IPv4 addresses. Used is the usage of the
// whole subscription, which the Usage API reports (nothing to count by hand),
// so Counted stays empty. Every call reads the usages: they are one call per
// region (limits and usage together), so there are no cached limits.
func (q *QuotaReader) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	cu, err := q.usages.ComputeUsages(cctx, region)
	cancel()
	if err != nil {
		return nil, classify(err, errContext{})
	}
	snap := &vendors.QuotaSnapshot{}
	for _, u := range cu {
		if u == nil || u.Name == nil || u.Name.Value == nil {
			continue
		}
		name := *u.Name.Value
		isFamily := strings.HasSuffix(strings.ToLower(name), familySuffix)
		if name != BucketRegionalCores && name != BucketSpotCores && !isFamily {
			continue
		}
		snap.Buckets = append(snap.Buckets, vendors.Bucket{
			ID: name, Scope: vendors.QuotaScopeRegion, Unit: unitVCPU,
			Limit: deref(u.Limit), Used: int64(deref(u.CurrentValue)),
		})
	}
	cctx, cancel = context.WithTimeout(ctx, callTimeout)
	nu, err := q.usages.NetworkUsages(cctx, region)
	cancel()
	if err != nil {
		return nil, classify(err, errContext{})
	}
	for _, u := range nu {
		if u == nil || u.Name == nil || u.Name.Value == nil || *u.Name.Value != BucketPublicIPs {
			continue
		}
		snap.Buckets = append(snap.Buckets, vendors.Bucket{
			ID: BucketPublicIPs, Scope: vendors.QuotaScopeRegion, Unit: unitAddresses,
			Limit: deref(u.Limit), Used: deref(u.CurrentValue),
		})
	}
	slices.SortFunc(snap.Buckets, func(a, b vendors.Bucket) int { return strings.Compare(a.ID, b.ID) })
	return snap, nil
}

// GlobalQuotas has nothing: every Azure vCPU and address limit is per region.
func (q *QuotaReader) GlobalQuotas(context.Context) (*vendors.QuotaSnapshot, error) {
	return &vendors.QuotaSnapshot{}, nil
}

// Consumption returns the buckets one instance consumes: for on-demand the
// regional vCPUs and the family's vCPUs (both tiers need headroom), for Spot
// the regional Spot vCPUs, plus one public IPv4 address (the default network
// profile; a node without one releases its reservation at the next read).
// false when the size is not in the SKU table read so far.
func (q *QuotaReader) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	q.mu.RLock()
	s, ok := q.skus[instanceType]
	q.mu.RUnlock()
	if !ok || s.VCPUs == 0 {
		return nil, false
	}
	n := int64(s.VCPUs)
	var out []vendors.Consumption
	if capacityType == vendors.CapacitySpot {
		out = append(out, vendors.Consumption{Bucket: BucketSpotCores, Amount: n})
	} else {
		if s.Family == "" {
			return nil, false
		}
		out = append(out,
			vendors.Consumption{Bucket: BucketRegionalCores, Amount: n},
			vendors.Consumption{Bucket: s.Family, Amount: n})
	}
	return append(out, vendors.Consumption{Bucket: BucketPublicIPs, Amount: publicIPsPerNodeLimit}), true
}
