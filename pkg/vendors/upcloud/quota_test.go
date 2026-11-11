/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud

import (
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
)

func bucketsByID(s *vendors.QuotaSnapshot) map[string]vendors.Bucket {
	m := map[string]vendors.Bucket{}
	for _, b := range s.Buckets {
		m[b.ID] = b
	}
	return m
}

func TestReadQuota(t *testing.T) {
	e := newEnv(t, fake.Options{Limits: upcloud.ResourceLimits{
		Cores: 20, Memory: 40 * 1024, StorageMaxIOPS: 500, PublicIPv4: 5, Networks: 3,
	}})
	ours := e.launch("nc-a", "1xCPU-2GB")
	e.f.AddServer(fake.ServerSpec{Zone: "de-fra1", Plan: "2xCPU-4GB", Title: "operator-db", PublicIPv4: true})
	if _, err := e.f.CreateNetwork(e.ctx, &request.CreateNetworkRequest{
		Zone: "fi-hel1", IPNetworks: upcloud.IPNetworkSlice{{Address: "10.9.0.0/24"}},
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := ReadQuota(e.ctx, e.f)
	if err != nil {
		t.Fatal(err)
	}
	b := bucketsByID(snap)
	for id, want := range map[string][2]int64{
		BucketCores:              {20, 3},
		BucketMemory:             {40 * 1024, 6 * 1024},
		StorageBucket("maxiops"): {500, 10 + 50 + 80},
		BucketPublicIPv4:         {5, 2},
		BucketNetworks:           {3, 1},
	} {
		got, ok := b[id]
		if !ok || got.Limit != want[0] || got.Used != want[1] || got.Scope != vendors.QuotaScopeGlobal {
			t.Errorf("%s = %+v, want limit %d used %d global", id, got, want[0], want[1])
		}
	}
	// A limit of 0 (absent) is not reported: it means unknown, not none.
	if _, ok := b[StorageBucket("standard")]; ok {
		t.Error("a bucket without a limit was reported")
	}
	if len(snap.Counted) != 1 || snap.Counted[0] != NewRef("fi-hel1", ours.UUID) {
		t.Errorf("counted = %v: only SCarpenter's own servers", snap.Counted)
	}
}

func TestQuotaUsageMovesWithLaunchAndDeletion(t *testing.T) {
	e := newEnv(t, fake.Options{Limits: upcloud.ResourceLimits{Cores: 10}})
	used := func() int64 {
		s, err := ReadQuota(e.ctx, e.f)
		if err != nil {
			t.Fatal(err)
		}
		return bucketsByID(s)[BucketCores].Used
	}
	if used() != 0 {
		t.Fatal("usage of an empty account")
	}
	d := e.launch("nc-a", "2xCPU-4GB")
	if used() != 2 {
		t.Errorf("after launch: %d", used())
	}
	if err := Terminate(e.ctx, e.f, d.UUID); err != nil {
		t.Fatal(err)
	}
	if used() != 0 {
		t.Errorf("after deletion: %d", used())
	}
}

func TestConsumptionMatchesTheAccountsAccounting(t *testing.T) {
	e := newEnv(t, fake.Options{Limits: upcloud.ResourceLimits{Cores: 100, Memory: 1 << 20, StorageMaxIOPS: 1000, PublicIPv4: 10}})
	plan := e.plans["2xCPU-4GB"]
	root := Root{Tier: "maxiops", SizeGB: 80}
	cons := Consumption(plan, root, true)
	before, _ := ReadQuota(e.ctx, e.f)
	e.launch("nc-a", "2xCPU-4GB")
	after, _ := ReadQuota(e.ctx, e.f)
	b0, b1 := bucketsByID(before), bucketsByID(after)
	for _, c := range cons {
		if got := b1[c.Bucket].Used - b0[c.Bucket].Used; got != c.Amount {
			t.Errorf("bucket %s moved by %d, Consumption said %d", c.Bucket, got, c.Amount)
		}
	}
}

func TestConsumption(t *testing.T) {
	cn := Plan{Name: "CLOUDNATIVE-2xCPU-4GB", Family: FamilyCloudNative, Cores: 2, MemoryMB: 4096}
	got := Consumption(cn, Root{Tier: "maxiops", SizeGB: 25}, true)
	want := []vendors.Consumption{{Bucket: "cores", Amount: 2}, {Bucket: "memory", Amount: 4096}, {Bucket: "storage-maxiops", Amount: 25}}
	if len(got) != len(want) {
		t.Fatalf("cloud native consumption = %v: no included IPv4", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	gen := Plan{Name: "1xCPU-1GB", Family: FamilyGeneral, Cores: 1, MemoryMB: 1024}
	if c := Consumption(gen, Root{Tier: "standard", SizeGB: 25}, true); c[len(c)-1] != (vendors.Consumption{Bucket: "public-ipv4", Amount: 1}) {
		t.Errorf("general plan consumption = %v", c)
	}
	if c := Consumption(gen, Root{Tier: "standard", SizeGB: 25}, false); len(c) != 3 {
		t.Errorf("no public IPv4 requested: %v", c)
	}
	if c := ConsumptionOfVolume(DataVolume{Tier: "hdd", SizeGB: 100}); c != (vendors.Consumption{Bucket: "storage-hdd", Amount: 100}) {
		t.Errorf("volume consumption = %v", c)
	}
}

func TestReadQuotaErrors(t *testing.T) {
	e := newEnv(t, fake.Options{})
	for _, m := range []string{"GetAccount", "GetServersWithFilters", "GetStorages", "GetIPAddresses", "GetNetworks"} {
		e.f.InjectCode(m, "SERVICE_ERROR", 500, 1)
		if _, err := ReadQuota(e.ctx, e.f); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("%s: %v", m, err)
		}
	}
}
