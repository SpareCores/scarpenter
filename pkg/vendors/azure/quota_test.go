/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure/fake"
)

func bucket(t *testing.T, s *vendors.QuotaSnapshot, id string) vendors.Bucket {
	t.Helper()
	for _, b := range s.Buckets {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("no bucket %s in %v", id, s.Buckets)
	return vendors.Bucket{}
}

func TestQuotasFollowLaunchesAndForeignUsage(t *testing.T) {
	e := newEnv(t)
	q := NewQuotaReader(e.clients.Usages)
	ctx := context.Background()

	snap, err := q.Quotas(ctx, testRegion)
	require.NoError(t, err)
	require.Equal(t, vendors.Bucket{ID: "cores", Scope: vendors.QuotaScopeRegion, Unit: "vCPU", Limit: 100}, bucket(t, snap, "cores"))
	require.EqualValues(t, 20, bucket(t, snap, "lowPriorityCores").Limit)
	require.EqualValues(t, 16, bucket(t, snap, "standardDSv5Family").Limit)
	require.Equal(t, vendors.Bucket{ID: "StandardPublicIPAddresses", Scope: vendors.QuotaScopeRegion, Unit: "addresses", Limit: 10}, bucket(t, snap, "StandardPublicIPAddresses"))
	require.Empty(t, snap.Counted, "Used is the account's: nothing counted by hand")
	for _, b := range snap.Buckets {
		require.NotEqual(t, "virtualMachines", b.ID, "only vCPU buckets and addresses")
	}

	// Usage that is not SCarpenter's counts (the whole subscription).
	e.sub.SetForeignUsage(testRegion, "cores", 30)
	e.sub.SetForeignUsage(testRegion, "standardDSv5Family", 6)
	_, err = e.launch(t, request{nodeClaim: "nc-1", size: "Standard_D4s_v5"})
	require.NoError(t, err)
	_, err = e.launch(t, request{nodeClaim: "nc-2", capacity: vendors.CapacitySpot})
	require.NoError(t, err)
	snap, err = q.Quotas(ctx, testRegion)
	require.NoError(t, err)
	require.EqualValues(t, 34, bucket(t, snap, "cores").Used, "30 foreign + 4 on-demand; the Spot VM has its own bucket")
	require.EqualValues(t, 10, bucket(t, snap, "standardDSv5Family").Used)
	require.EqualValues(t, 2, bucket(t, snap, "lowPriorityCores").Used)
	require.EqualValues(t, 2, bucket(t, snap, "StandardPublicIPAddresses").Used)

	// Termination releases it.
	list, err := e.c.List(ctx, []string{testRegion}, nil)
	require.NoError(t, err)
	for _, i := range list {
		require.NoError(t, e.c.Delete(ctx, i.Ref))
	}
	snap, err = q.Quotas(ctx, testRegion)
	require.NoError(t, err)
	require.EqualValues(t, 30, bucket(t, snap, "cores").Used)
	require.EqualValues(t, 0, bucket(t, snap, "StandardPublicIPAddresses").Used)

	// Another region is its own quota.
	other, err := q.Quotas(ctx, "northeurope")
	require.NoError(t, err)
	require.EqualValues(t, 0, bucket(t, other, "cores").Used)
}

func TestQuotaErrors(t *testing.T) {
	e := newEnv(t)
	q := NewQuotaReader(e.clients.Usages)
	e.sub.FailNext("usages.compute", fake.Error(http.StatusTooManyRequests, "TooManyRequests", "x"), 1)
	_, err := q.Quotas(context.Background(), testRegion)
	require.Equal(t, vendors.Retryable, vendors.ClassOf(err))
	e.sub.FailNext("usages.network", fake.Error(http.StatusForbidden, "AuthorizationFailed", "x"), 1)
	_, err = q.Quotas(context.Background(), testRegion)
	require.Equal(t, vendors.Fatal, vendors.ClassOf(err))
	global, err := q.GlobalQuotas(context.Background())
	require.NoError(t, err)
	require.Empty(t, global.Buckets)
}

// TestConsumptionMapsEverySizeOfTheTable is the plan's test: every size of the
// SKU table maps to buckets that exist in the usage listing, by the table and
// never by name.
func TestConsumptionMapsEverySizeOfTheTable(t *testing.T) {
	e := newEnv(t)
	q := NewQuotaReader(e.clients.Usages)
	ctx := context.Background()
	skus := skuTable(t, e, testRegion)
	q.LoadSKUs(skus)
	snap, err := q.Quotas(ctx, testRegion)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, b := range snap.Buckets {
		ids[b.ID] = true
	}
	require.NotEmpty(t, skus)
	for name, s := range skus {
		for _, capacity := range []string{vendors.CapacityOnDemand, vendors.CapacitySpot} {
			cs, ok := q.Consumption(name, capacity)
			require.True(t, ok, name)
			for _, c := range cs {
				require.True(t, ids[c.Bucket], "%s %s consumes %s, which the usage listing does not have", name, capacity, c.Bucket)
			}
			if capacity == vendors.CapacityOnDemand {
				require.Equal(t, []vendors.Consumption{
					{Bucket: "cores", Amount: int64(s.VCPUs)}, {Bucket: s.Family, Amount: int64(s.VCPUs)},
					{Bucket: "StandardPublicIPAddresses", Amount: 1},
				}, cs)
			} else {
				require.Equal(t, []vendors.Consumption{
					{Bucket: "lowPriorityCores", Amount: int64(s.VCPUs)}, {Bucket: "StandardPublicIPAddresses", Amount: 1},
				}, cs)
			}
		}
	}
	// A size missing from the table has an unknown quota (reactive path).
	_, ok := q.Consumption("Standard_Unknown", vendors.CapacityOnDemand)
	require.False(t, ok)
	_, ok = NewQuotaReader(e.clients.Usages).Consumption("Standard_D2s_v5", vendors.CapacityOnDemand)
	require.False(t, ok, "before any SKU read")
}
