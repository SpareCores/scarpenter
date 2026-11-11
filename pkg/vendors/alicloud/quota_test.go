/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"slices"
	"testing"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud/fake"
)

func bucketsByID(s *vendors.QuotaSnapshot) map[string]vendors.Bucket {
	m := map[string]vendors.Bucket{}
	for _, b := range s.Buckets {
		m[b.ID] = b
	}
	return m
}

func TestQuotasLimitAndAccountWideUsage(t *testing.T) {
	e := newEnv(t)
	e.f.SetQuota(testRegion, 100, 40)
	ctx := context.Background()
	mine, _ := e.create("default-abc12", "uid-1") // 2 vCPUs on demand
	req, lr := e.request("default-spot", "uid-2")
	req.CapacityType = vendors.CapacitySpot
	req.InstanceType = "ecs.g7.xlarge" // 4 vCPUs spot
	spot, err := e.cloud.CreateInstance(ctx, req, lr)
	if err != nil {
		t.Fatal(err)
	}
	foreign := e.f.AddForeignInstance(testRegion, testZoneA, "ecs.g7.xlarge", false) // 4 vCPUs of someone else
	snap, err := e.q.Quotas(ctx, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	b := bucketsByID(snap)
	if got := b[BucketPostpaid]; got.Limit != 100 || got.Used != 6 || got.Scope != vendors.QuotaScopeRegion || got.Unit != "vCPU" {
		t.Errorf("postpaid = %+v", got)
	}
	if got := b[BucketSpot]; got.Limit != 40 || got.Used != 4 {
		t.Errorf("spot = %+v", got)
	}
	want := []vendors.InstanceRef{mine.Ref, spot.Ref, vendors.NewInstanceRef(testRegion, foreign)}
	slices.Sort(want)
	got := slices.Clone(snap.Counted)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("counted = %v, want %v", got, want)
	}
	// A stopped instance is counted conservatively.
	e.f.Stop(testRegion, mine.Ref.ID())
	snap, _ = e.q.Quotas(ctx, testRegion)
	if !slices.Contains(snap.Counted, mine.Ref) || bucketsByID(snap)[BucketPostpaid].Used != 6 {
		t.Errorf("a stopped instance must count: %+v", snap)
	}
	// A released one does not.
	e.f.Release(testRegion, mine.Ref.ID())
	snap, _ = e.q.Quotas(ctx, testRegion)
	if slices.Contains(snap.Counted, mine.Ref) || bucketsByID(snap)[BucketPostpaid].Used != 4 {
		t.Errorf("a released instance must not: %+v", snap)
	}
}

func TestQuotasPageThroughManyInstances(t *testing.T) {
	e := newEnv(t)
	for range 250 {
		e.f.AddForeignInstance(testRegion, testZoneA, testType, false)
	}
	snap, err := e.q.Quotas(context.Background(), testRegion)
	if err != nil || len(snap.Counted) != 250 {
		t.Fatalf("counted %d, %v", len(snap.Counted), err)
	}
}

func TestQuotasMissingAttributeHasNoBucket(t *testing.T) {
	e := newEnv(t)
	c := e.cloudWith(func(a ECSAPI) ECSAPI { return noSpotAttrs{a} })
	snap, err := NewQuotas(c).Quotas(context.Background(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	b := bucketsByID(snap)
	if _, ok := b[BucketSpot]; ok {
		t.Error("an attribute ECS does not return has no bucket")
	}
	if _, ok := b[BucketPostpaid]; !ok {
		t.Error("the postpaid bucket is missing")
	}
}

type noSpotAttrs struct{ ECSAPI }

func (n noSpotAttrs) DescribeAccountAttributes(ctx context.Context, in *ecs.DescribeAccountAttributesRequest) (*ecs.DescribeAccountAttributesResponseBody, error) {
	out, err := n.ECSAPI.DescribeAccountAttributes(ctx, in)
	if err != nil {
		return nil, err
	}
	items := out.AccountAttributeItems.AccountAttributeItem[:0:0]
	for _, it := range out.AccountAttributeItems.AccountAttributeItem {
		if *it.AttributeName != attrMaxSpot && *it.AttributeName != attrUsedSpot {
			items = append(items, it)
		}
	}
	out.AccountAttributeItems.AccountAttributeItem = items
	return out, nil
}

func TestConsumption(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, ok := e.q.Consumption(testType, vendors.CapacityOnDemand); ok {
		t.Fatal("a type whose vCPUs are not read is unknown quota")
	}
	if err := e.cloud.LoadTypes(ctx, testRegion, []string{testType}); err != nil {
		t.Fatal(err)
	}
	for capacity, bucket := range map[string]string{vendors.CapacityOnDemand: BucketPostpaid, vendors.CapacitySpot: BucketSpot} {
		c, ok := e.q.Consumption(testType, capacity)
		if !ok || len(c) != 1 || c[0].Bucket != bucket || c[0].Amount != 2 {
			t.Errorf("%s: %v %v", capacity, c, ok)
		}
	}
	g, err := e.q.GlobalQuotas(ctx)
	if err != nil || len(g.Buckets) != 0 {
		t.Errorf("global quotas = %+v, %v", g, err)
	}
}

func TestQuotaErrorsAreClassified(t *testing.T) {
	e := newEnv(t)
	e.f.Inject("DescribeAccountAttributes", fake.APIError("Forbidden.RAM", 403, "no"), 1)
	if _, err := e.q.Quotas(context.Background(), testRegion); vendors.ClassOf(err) != vendors.Fatal {
		t.Fatalf("err = %v", err)
	}
}

// The launch that hits the vCPU limit reports exactly the limit the reader
// shows, so the tracker's refresh on QuotaExceeded and the reader agree.
func TestLaunchHitsTheLimitTheReaderShows(t *testing.T) {
	e := newEnv(t)
	e.f.SetQuota(testRegion, 4, 4)
	ctx := context.Background()
	for i, claim := range []string{"a-one", "a-two"} {
		if _, err := e.create(claim, "uid-"+claim); err != nil {
			t.Fatalf("launch %d: %v", i, err)
		}
	}
	_, err := e.create("a-three", "uid-three")
	if !vendors.IsClass(err, vendors.QuotaExceeded) {
		t.Fatalf("the third launch: %v", err)
	}
	snap, _ := e.q.Quotas(ctx, testRegion)
	if b := bucketsByID(snap)[BucketPostpaid]; b.Used != b.Limit {
		t.Fatalf("bucket = %+v", b)
	}
}
