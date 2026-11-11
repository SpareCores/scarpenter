/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"context"
	"slices"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/api"
)

// Quota buckets. All of UpCloud's limits are account-wide: they are reported as
// global buckets and Quotas(region) returns none.
const (
	BucketCores      = "cores"
	BucketMemory     = "memory"
	BucketPublicIPv4 = "public-ipv4"
	BucketNetworks   = "networks"
	bucketStorage    = "storage-"
)

// StorageBucket is the bucket of a storage tier (storage-maxiops).
func StorageBucket(tier string) string { return bucketStorage + tier }

// Usage is the account's consumption, computed from the lists: UpCloud's
// account call has limits and no usage (S7 checks).
type Usage struct {
	Cores    int
	MemoryMB int
	// StorageGB is the private storage per tier, templates and server disks.
	StorageGB  map[string]int
	PublicIPv4 int
	Networks   int
	// Ours are the servers SCarpenter made (label scarpenter-managed-by), whose
	// reservations the in-flight ledger can release once counted.
	Ours []vendors.InstanceRef
}

// CollectUsage reads the account's usage: every server of every zone (all
// states count; which ones UpCloud counts is unverified), the private storages,
// the public IPv4 addresses and the private networks, plus a label-filtered
// server list for the references of SCarpenter's own.
func CollectUsage(ctx context.Context, a api.API) (*Usage, error) {
	u := &Usage{StorageGB: map[string]int{}}
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	servers, err := a.GetServersWithFilters(cctx, &request.GetServersWithFiltersRequest{})
	if err != nil {
		return nil, Classify(err)
	}
	for _, s := range servers.Servers {
		u.Cores += s.CoreNumber
		u.MemoryMB += s.MemoryAmount
	}
	storages, err := a.GetStorages(cctx, &request.GetStoragesRequest{Access: upcloud.StorageAccessPrivate})
	if err != nil {
		return nil, Classify(err)
	}
	for _, st := range storages.Storages {
		u.StorageGB[st.Tier] += st.Size
	}
	ips, err := a.GetIPAddresses(cctx)
	if err != nil {
		return nil, Classify(err)
	}
	for _, ip := range ips.IPAddresses {
		if ip.Access == upcloud.IPAddressAccessPublic && ip.Family == upcloud.IPAddressFamilyIPv4 && ip.Floating.Empty() {
			u.PublicIPv4++
		}
	}
	nets, err := a.GetNetworks(cctx)
	if err != nil {
		return nil, Classify(err)
	}
	for _, n := range nets.Networks {
		if n.Type == upcloud.NetworkTypePrivate {
			u.Networks++
		}
	}
	ours, err := a.GetServersWithFilters(cctx, &request.GetServersWithFiltersRequest{Filters: []request.QueryFilter{
		request.FilterLabel{Label: upcloud.Label{Key: TagKey(vendors.TagManagedBy), Value: vendors.ManagedByValue}},
	}})
	if err != nil {
		return nil, Classify(err)
	}
	for _, s := range ours.Servers {
		u.Ours = append(u.Ours, NewRef(s.Zone, s.UUID))
	}
	slices.Sort(u.Ours)
	return u, nil
}

// Snapshot builds the account-wide buckets: a bucket is reported only when its
// limit is positive, as a limit of 0 or an absent field means "unknown", not
// "none" (unverified). UpCloud's "ssd" storage limit is the standard tier.
func Snapshot(l upcloud.ResourceLimits, u Usage) *vendors.QuotaSnapshot {
	s := &vendors.QuotaSnapshot{Counted: slices.Clone(u.Ours)}
	add := func(id, unit string, limit, used int) {
		if limit > 0 {
			s.Buckets = append(s.Buckets, vendors.Bucket{
				ID: id, Scope: vendors.QuotaScopeGlobal, Unit: unit, Limit: int64(limit), Used: int64(used),
			})
		}
	}
	add(BucketCores, "vCPU", l.Cores, u.Cores)
	add(BucketMemory, "MB", l.Memory, u.MemoryMB)
	add(StorageBucket(upcloud.StorageTierMaxIOPS), "GB", l.StorageMaxIOPS, u.StorageGB[upcloud.StorageTierMaxIOPS])
	add(StorageBucket(upcloud.StorageTierStandard), "GB", l.StorageSSD, u.StorageGB[upcloud.StorageTierStandard])
	add(StorageBucket(upcloud.StorageTierHDD), "GB", l.StorageHDD, u.StorageGB[upcloud.StorageTierHDD])
	add(BucketPublicIPv4, "addresses", l.PublicIPv4, u.PublicIPv4)
	add(BucketNetworks, "networks", l.Networks, u.Networks)
	return s
}

// ReadQuota reads the limits and the usage and returns the account-wide
// snapshot (the driver's QuotaReporter caches the limits, GlobalQuotas).
func ReadQuota(ctx context.Context, a api.API) (*vendors.QuotaSnapshot, error) {
	limits, err := ReadLimits(ctx, a)
	if err != nil {
		return nil, err
	}
	return SnapshotOf(ctx, a, limits)
}

// ReadLimits reads the account's resource limits.
func ReadLimits(ctx context.Context, a api.API) (upcloud.ResourceLimits, error) {
	cctx, cancel := CallContext(ctx, CallTimeout)
	defer cancel()
	acct, err := a.GetAccount(cctx)
	if err != nil {
		return upcloud.ResourceLimits{}, Classify(err)
	}
	return acct.ResourceLimits, nil
}

// SnapshotOf reads the usage and builds the snapshot against the limits.
func SnapshotOf(ctx context.Context, a api.API, l upcloud.ResourceLimits) (*vendors.QuotaSnapshot, error) {
	u, err := CollectUsage(ctx, a)
	if err != nil {
		return nil, err
	}
	return Snapshot(l, *u), nil
}

// Consumption is what a launch of the plan takes: its cores, its memory in MB,
// one public IPv4 address (none for Cloud Native plans, where the address is a
// separate item; none when the node has no public IPv4) and the root disk on
// its tier. Data volumes add their own with ConsumptionOfVolume.
func Consumption(p Plan, root Root, publicIPv4 bool) []vendors.Consumption {
	out := []vendors.Consumption{
		{Bucket: BucketCores, Amount: int64(p.Cores)},
		{Bucket: BucketMemory, Amount: int64(p.MemoryMB)},
		{Bucket: StorageBucket(root.Tier), Amount: int64(root.SizeGB)},
	}
	if publicIPv4 && p.HasIPv4() {
		out = append(out, vendors.Consumption{Bucket: BucketPublicIPv4, Amount: 1})
	}
	return out
}

// ConsumptionOfVolume is the storage a data volume takes.
func ConsumptionOfVolume(v DataVolume) vendors.Consumption {
	return vendors.Consumption{Bucket: StorageBucket(v.Tier), Amount: int64(v.SizeGB)}
}
