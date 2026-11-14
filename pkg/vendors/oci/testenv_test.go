/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/catalog/embeddeddata/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

const (
	testRegion  = fake.SeedRegion
	testZone    = fake.SeedZoneA
	testType    = "VM.Standard.E5.Flex.2-16"
	testCluster = "conformance"
)

// row is the part of an embedded server row the tests need.
type row struct {
	ID        string
	VCPUs     int
	MemoryMiB float64
	Arm       bool
}

var (
	rowsOnce sync.Once
	rowsAll  []row
)

// catalogRows returns the active rows of the committed embedded OCI catalog (the
// files of step (a1)): the full type list of the vendor.
func catalogRows(t testing.TB) []row {
	t.Helper()
	rowsOnce.Do(func() {
		body := func(path string) *keeper.SnapshotBody {
			b, err := oci.FS.ReadFile(path + ".json.gz")
			if err != nil {
				t.Fatal(err)
			}
			return &keeper.SnapshotBody{Gzip: b}
		}
		m, err := keeper.DecodeManifest(body("manifest"))
		if err != nil {
			t.Fatal(err)
		}
		f, ok := m.Lookup("server/oci")
		if !ok {
			t.Fatal("server/oci is not in the manifest")
		}
		servers, err := keeper.DecodeSnapshotRows[keeper.TableServer](body("server/oci"), f.Rows)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range servers {
			if s.Status == "active" {
				rowsAll = append(rowsAll, row{ID: s.ServerID, VCPUs: s.VCPUs, MemoryMiB: s.MemoryAmount,
					Arm: s.CPUArchitecture == "arm64"})
			}
		}
	})
	if len(rowsAll) == 0 {
		t.Fatal("the embedded catalog has no active OCI servers")
	}
	return rowsAll
}

// limitTag is the stem of the limit names of a shape: VM.Standard.E5.Flex ->
// standard-e5.
func limitTag(shape string) string {
	s := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(shape, "VM."), "BM."), ".Flex")
	return strings.ToLower(strings.ReplaceAll(s, ".", "-"))
}

// seedShapes offers every shape of the catalog in every region of the fake, with
// the bounds the rows imply and limit names of the forms OCI uses.
func seedShapes(t testing.TB, c *fake.Cloud) {
	t.Helper()
	type bounds struct {
		maxO, maxG, minPer, maxPer float32
	}
	flex := map[string]*bounds{}
	var fixed []row
	for _, r := range catalogRows(t) {
		typ, err := driver.ParseType(r.ID)
		if err != nil {
			t.Fatalf("%s: %v", r.ID, err)
		}
		if !typ.Sized {
			fixed = append(fixed, r)
			continue
		}
		b := flex[typ.Shape]
		per := float32(typ.GB) / float32(typ.OCPUs)
		if b == nil {
			b = &bounds{minPer: per, maxPer: per}
			flex[typ.Shape] = b
		}
		b.maxO, b.maxG = max(b.maxO, float32(typ.OCPUs)), max(b.maxG, float32(typ.GB))
		b.minPer, b.maxPer = min(b.minPer, per), max(b.maxPer, per)
	}
	for _, region := range []string{fake.SeedRegion, fake.SeedRegionB, fake.SeedRegionOne} {
		for _, shape := range slices.Sorted(maps.Keys(flex)) {
			b := flex[shape]
			tag := limitTag(shape)
			var numaO, numaG float32
			if shape == "VM.Standard.E5.Flex" {
				numaO, numaG = 94, 1049 // plan 25, section 2
			}
			c.AddShape(region, fake.FlexShape(shape, b.maxO, b.maxG, b.minPer, b.maxPer, numaO, numaG,
				tag+"-core-count", tag+"-memory-count"))
		}
		for _, r := range fixed {
			k, ok := driver.VCPUsPerOCPU(r.ID)
			if !ok {
				t.Fatalf("%s: unknown vCPUs per OCPU", r.ID)
			}
			names := []string{limitTag(r.ID) + "-core-count"}
			if strings.HasPrefix(r.ID, "BM.") {
				names = []string{"bm-" + limitTag(r.ID) + "-count"}
			}
			c.AddShape(region, fake.FixedShape(r.ID, float32(r.VCPUs/k), float32(r.MemoryMiB/1024), names...))
		}
	}
}

// testDriver is the driver as the conformance suite drives it: OCI has no
// default subnet (no ResourceDefaults), so a request that Prepare did not
// resolve gets the seeded subnet here, as the core's Prepare would give it.
type testDriver struct {
	*driver.Driver
}

func (d testDriver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	if req.Prepared == nil {
		req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{
			{Kind: driver.KindSubnets, ID: fake.SeedSubnet, Region: req.Region},
		}}
	}
	return d.Driver.CreateInstance(ctx, req)
}

// env is a driver over a fake tenancy.
type env struct {
	d     *driver.Driver
	c     *fake.Cloud
	clock *clocktesting.FakeClock
}

func newCloud(t testing.TB) *fake.Cloud {
	t.Helper()
	c := fake.NewSeeded()
	seedShapes(t, c)
	return c
}

func newEnv(t testing.TB, opts ...driver.Option) *env {
	t.Helper()
	return restart(t, newCloud(t), testCluster, opts...)
}

func restart(t testing.TB, c *fake.Cloud, cluster string, opts ...driver.Option) *env {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Now().Truncate(time.Second))
	c.SetClock(clk.Now)
	opts = append([]driver.Option{
		driver.WithClients(driver.Clients{Compute: c, Network: c, Identity: c, Limits: c}),
		driver.WithCompartment(fake.SeedCompartment), driver.WithTenancy(fake.SeedTenancy),
	}, opts...)
	d := driver.New(opts...)
	if err := d.Init(context.Background(), vendors.Config{ClusterName: cluster, Clock: clk, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	return &env{d: d, c: c, clock: clk}
}

// request is a launch of the seeded subnet, on-demand, in the first zone.
func request(typ string) vendors.CreateRequest {
	return vendors.CreateRequest{
		Region: testRegion, Zone: testZone, InstanceType: typ, CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image:    vendors.Image{Ref: fake.SeedImage},
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: testCluster, vendors.TagNodeClaim: "claim-1", vendors.TagNodePool: "pool",
			vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: "uid-1",
		Node:           vendors.NodeInfo{Name: "claim-1"},
		Prepared: &vendors.Prepared{Resources: []vendors.ResourceRef{
			{Kind: driver.KindSubnets, ID: fake.SeedSubnet},
			{Kind: driver.KindNetworkSecurityGroups, ID: "ocid1.networksecuritygroup.oc1.eu-frankfurt-1.aaaaone"},
			{Kind: driver.KindRunNetworkSecurityGroup, ID: "ocid1.networksecuritygroup.oc1.eu-frankfurt-1.aaaarun"},
		}},
	}
}
