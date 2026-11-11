/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"slices"
	"strconv"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Quota buckets (plan 21 section 6): ECS limits vCPUs per region, separately
// for pay-as-you-go and spot instances, and DescribeAccountAttributes gives
// limit and usage of both in one call. Every type consumes its vCPUs from the
// bucket of its capacity type.
//
// Open (step 0): whether these attributes are the binding limit, or Quota
// Center (ListProductQuotas, per-family limits such as q_ecs_restrict_postpay_c)
// must add family buckets. The Quota Center SDK is not a dependency yet.
const (
	BucketPostpaid = "postpaid-vcpu"
	BucketSpot     = "spot-vcpu"
)

// Names of the account attributes the reader asks for.
const (
	attrMaxPostpaid  = "max-postpaid-instance-vcpu-count"
	attrUsedPostpaid = "used-postpaid-instance-vcpu-count"
	attrMaxSpot      = "max-spot-instance-vcpu-count"
	attrUsedSpot     = "used-spot-instance-vcpu-count"
)

// bucketOf maps a capacity type to its bucket.
func bucketOf(capacityType string) string {
	if capacityType == vendors.CapacitySpot {
		return BucketSpot
	}
	return BucketPostpaid
}

// Quotas is the quota reader of the driver (vendors.QuotaReporter).
//
// Limit and usage come from one call, so nothing is cached here: the tracker
// decides the cadence (limits every 15 minutes, and at once after a QuotaExceed
// error; usage every 2 minutes).
type Quotas struct {
	cloud *Cloud
}

// NewQuotas returns the reader on the client layer.
func NewQuotas(cloud *Cloud) *Quotas { return &Quotas{cloud: cloud} }

var _ vendors.QuotaReporter = (*Quotas)(nil)

// Consumption implements vendors.QuotaReporter: an instance consumes its vCPUs
// of the bucket of its capacity type. A type whose vCPUs the driver has not
// read (the type table is filled by Cloud.LoadTypes) is unknown: the reactive
// path of overview section 5.6 applies.
func (q *Quotas) Consumption(instanceType, capacityType string) ([]vendors.Consumption, bool) {
	ti, ok := q.cloud.Types.Get(instanceType)
	if !ok || ti.VCPUs <= 0 {
		return nil, false
	}
	return []vendors.Consumption{{Bucket: bucketOf(capacityType), Amount: ti.VCPUs}}, true
}

// GlobalQuotas implements vendors.QuotaReporter: the vCPU limits are regional,
// there are no account-wide buckets.
func (q *Quotas) GlobalQuotas(context.Context) (*vendors.QuotaSnapshot, error) {
	return &vendors.QuotaSnapshot{}, nil
}

// countedStatuses are the instance states that count for the in-flight ledger:
// everything not released, stopped ones included (whether a stopped instance
// counts against the vCPU quota is unverified, so the ledger stays
// conservative).
var countedStatuses = []string{statusPending, statusStarting, statusRunning, statusStopping, statusStopped}

// Quotas implements vendors.QuotaReporter: the limit and the usage of the
// account in the region (DescribeAccountAttributes; Used covers the whole
// account), and the references of the instances that count, from a region-wide
// DescribeInstances.
func (q *Quotas) Quotas(ctx context.Context, region string) (*vendors.QuotaSnapshot, error) {
	cl, err := q.cloud.Clients(ctx, region)
	if err != nil {
		return nil, err
	}
	in := &ecs.DescribeAccountAttributesRequest{
		RegionId:      new(region),
		AttributeName: dara.StringSlice([]string{attrMaxPostpaid, attrUsedPostpaid, attrMaxSpot, attrUsedSpot}),
	}
	out, err := call(ctx, q.cloud, callTimeout, errContext{},
		func(ctx context.Context) (*ecs.DescribeAccountAttributesResponseBody, error) {
			return cl.ECS.DescribeAccountAttributes(ctx, in)
		})
	if err != nil {
		return nil, err
	}
	attrs := map[string]int64{}
	if out.AccountAttributeItems != nil {
		for _, it := range out.AccountAttributeItems.AccountAttributeItem {
			if it.AttributeValues == nil || len(it.AttributeValues.ValueItem) == 0 {
				continue
			}
			if v, err := strconv.ParseInt(deref(it.AttributeValues.ValueItem[0].Value), 10, 64); err == nil {
				attrs[deref(it.AttributeName)] = v
			}
		}
	}
	counted, err := q.counted(ctx, region)
	if err != nil {
		return nil, err
	}
	snap := &vendors.QuotaSnapshot{Counted: counted}
	for _, b := range []struct {
		id, max, used string
	}{{BucketPostpaid, attrMaxPostpaid, attrUsedPostpaid}, {BucketSpot, attrMaxSpot, attrUsedSpot}} {
		limit, ok := attrs[b.max]
		if !ok {
			continue // the attribute is not there: no bucket, the types are unknown quota
		}
		snap.Buckets = append(snap.Buckets, vendors.Bucket{
			ID: b.id, Scope: vendors.QuotaScopeRegion, Unit: "vCPU", Limit: limit, Used: attrs[b.used],
		})
	}
	return snap, nil
}

// counted lists the references of the region's counted instances, whoever owns
// them.
func (q *Quotas) counted(ctx context.Context, region string) ([]vendors.InstanceRef, error) {
	insts, err := q.cloud.describe(ctx, region, &ecs.DescribeInstancesRequest{})
	if err != nil {
		return nil, err
	}
	var refs []vendors.InstanceRef
	for _, in := range insts {
		if sliceContains(countedStatuses, deref(in.Status)) {
			refs = append(refs, vendors.NewInstanceRef(region, deref(in.InstanceId)))
		}
	}
	return refs, nil
}

func sliceContains(s []string, v string) bool {
	return slices.Contains(s, v)
}
