/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/vendortest"
)

// snapshotRow is the part of a snapshot server row the tests need.
type snapshotRow struct {
	VendorID     string  `json:"vendor_id"`
	APIReference string  `json:"api_reference"`
	VCPUs        int32   `json:"vcpus"`
	MemoryMiB    float32 `json:"memory_amount"`
	Arch         string  `json:"cpu_architecture"`
}

var (
	snapshotOnce sync.Once
	snapshot     []snapshotRow
)

// alicloudServers returns the alicloud rows of the committed Keeper snapshot: the
// full type list of the vendor, active and inactive.
func alicloudServers(t testing.TB) []snapshotRow {
	t.Helper()
	snapshotOnce.Do(func() {
		rows := vendortest.SnapshotRows[snapshotRow](t, "server", "alicloud")
		for _, r := range rows {
			if r.VendorID == Vendor {
				snapshot = append(snapshot, r)
			}
		}
	})
	if len(snapshot) == 0 {
		t.Fatal("the snapshot has no alicloud servers")
	}
	return snapshot
}

// snapshotTypes builds the fake's instance types from the snapshot: the vCPUs, the
// memory and the architecture; the 8th generation and later need NVMe, and the i
// and d families have local disks.
func snapshotTypes(t testing.TB) []fake.TypeSpec {
	t.Helper()
	rows := alicloudServers(t)
	out := make([]fake.TypeSpec, 0, len(rows))
	for _, r := range rows {
		ts := fake.TypeSpec{Name: r.APIReference, VCPUs: r.VCPUs, MemoryGiB: r.MemoryMiB / 1024, Disks: 17, Arch: "amd64"}
		if r.Arch == "arm64" {
			ts.Arch = "arm64"
		}
		if HasLocalDisks(r.APIReference) {
			ts.LocalDisk = 2
		}
		out = append(out, ts)
	}
	return out
}

const (
	otherRegion = "eu-central-1"
	otherZone   = "eu-central-1a"
)

// denv is a driver over a fake account.
type denv struct {
	t       testing.TB
	d       *Driver
	acct    *fake.Cloud
	clk     *clocktesting.FakeClock
	cluster string

	vpc       map[string]string            // the default VPC of a region
	vswitches map[string]map[string]string // region -> zone -> default vSwitch
	groups    map[string]string            // region -> a security group of the default VPC
	sleeps    *atomic.Int64                // the driver waits from concurrent goroutines
}

type denvOptions struct {
	cluster string
	types   []fake.TypeSpec
	prices  PriceSource
	// noDefaultNetwork leaves the regions without a default VPC.
	noDefaultNetwork bool
	// noImages leaves the regions without the Flatcar images.
	noImages bool
}

var smallTypes = []fake.TypeSpec{
	{Name: "ecs.g8y.large", VCPUs: 2, MemoryGiB: 8, Arch: "arm64", Disks: 17},
	{Name: "ecs.g8y.xlarge", VCPUs: 4, MemoryGiB: 16, Arch: "arm64", Disks: 17},
	{Name: "ecs.g7.large", VCPUs: 2, MemoryGiB: 8, Disks: 17},
	{Name: "ecs.g7.xlarge", VCPUs: 4, MemoryGiB: 16, Disks: 17},
	{Name: "ecs.e-c1m2.large", VCPUs: 2, MemoryGiB: 4, Disks: 5},
	{Name: "ecs.i4.xlarge", VCPUs: 4, MemoryGiB: 32, Disks: 17, LocalDisk: 1},
	{Name: "ecs.c8i.large", VCPUs: 2, MemoryGiB: 4, Disks: 17, NVMe: true},
	{Name: "ecs.ebmg7.32xlarge", VCPUs: 128, MemoryGiB: 512, Disks: 17},
}

// testRelease is the Flatcar release the images of the fixture are.
const (
	testChannel = "stable"
	testVersion = "4757.2.3"
)

func newDenv(t testing.TB, o denvOptions) *denv {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	acct := fake.New(clk)
	acct.AddRegion(testRegion, testZoneA, testZoneB)
	acct.AddRegion(otherRegion, otherZone)
	types := o.types
	if types == nil {
		types = smallTypes
	}
	for _, ts := range types {
		acct.AddType(ts)
	}
	e := &denv{
		sleeps: &atomic.Int64{},
		t:      t, acct: acct, clk: clk, cluster: o.cluster, vpc: map[string]string{},
		vswitches: map[string]map[string]string{}, groups: map[string]string{},
	}
	if e.cluster == "" {
		e.cluster = testCluster
	}
	for _, r := range []string{testRegion, otherRegion} {
		acct.OfferEverywhere(r)
		acct.SetQuota(r, 100000, 100000)
		if !o.noDefaultNetwork {
			e.vpc[r], e.vswitches[r] = acct.AddDefaultNetwork(r)
			g, err := acct.ECS(r).CreateSecurityGroup(context.Background(), sgReq(e.vpc[r]))
			if err != nil {
				t.Fatal(err)
			}
			e.groups[r] = *g.SecurityGroupId
		}
		// A deployment set the user made (not ours: no marker).
		if _, err := acct.ECS(r).CreateDeploymentSet(context.Background(), &ecs.CreateDeploymentSetRequest{
			DeploymentSetName: new("my-set"), Strategy: new("Availability"),
		}); err != nil {
			t.Fatal(err)
		}
		if !o.noImages {
			for _, arch := range []string{archAMD64, archARM64} {
				acct.AddImage(r, flatcarImage(arch, testChannel, testVersion))
			}
			acct.AddImage(r, fake.ImageSpec{ID: "image", Name: "image", Tags: map[string]string{
				tagFlatcarChannel: testChannel, tagFlatcarVersion: testVersion}})
		}
	}
	e.build(o)
	return e
}

// flatcarImage is the image of a release by the naming convention.
func flatcarImage(arch, channel, version string) fake.ImageSpec {
	a := "x86_64"
	if arch == archARM64 {
		a = "arm64"
	}
	return fake.ImageSpec{
		Name: imageName(channel, version, arch), Arch: a, SizeGiB: 9,
		Tags: map[string]string{tagFlatcarChannel: channel, tagFlatcarVersion: version, "arch": arch},
	}
}

func (e *denv) build(o denvOptions) {
	e.t.Helper()
	prices := o.prices
	if prices == nil {
		prices = StaticPrices{}
	}
	d := New(
		WithClientFactory(func(_ context.Context, region string) (Clients, error) {
			return Clients{ECS: e.acct.ECS(region), VPC: e.acct.VPC(region)}, nil
		}),
		// The waits of the driver move the fake clock instead of sleeping.
		WithSleep(func(_ context.Context, d time.Duration) error { e.sleeps.Add(1); e.clk.Step(d); return nil }),
		WithPrices(prices),
	)
	if err := d.Init(context.Background(), vendors.Config{ClusterName: e.cluster, Clock: e.clk, Log: logr.Discard()}); err != nil {
		e.t.Fatal(err)
	}
	e.d = d
}

// restart returns a second driver over the same account (a restarted controller).
func (e *denv) restart() *denv {
	n := *e
	n.build(denvOptions{cluster: e.cluster})
	return &n
}

// warm reads the offerings of the region, which fills the instance type table the
// way the quota tracker does at startup.
func (e *denv) warm(region string) {
	e.t.Helper()
	if _, err := e.d.LaunchableTypes(context.Background(), region); err != nil {
		e.t.Fatal(err)
	}
}

// claim is a request of the tests: on demand in the first zone, prepared with the
// default network and the group of the region.
func (e *denv) claim(name, typ, capacity string) vendors.CreateRequest {
	return vendors.CreateRequest{
		Region: testRegion, Zone: testZoneA, InstanceType: typ, CapacityType: capacity, Arch: archAMD64,
		Image: vendors.Image{Ref: "image", RootDiskBytes: 9 << 30}, UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: e.cluster, vendors.TagNodeClaim: name, vendors.TagNodePool: "pool",
			vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: "uid-" + name,
	}
}

func deleteGroup(id string) *ecs.DeleteSecurityGroupRequest {
	return &ecs.DeleteSecurityGroupRequest{SecurityGroupId: &id}
}
