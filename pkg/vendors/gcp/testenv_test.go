/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

const (
	testProject = "my-project"
	testCluster = "conformance"
	testRegion  = fake.SeedRegion
	testZone    = fake.SeedZoneA
	testType    = "e2-small"
)

// confirmed is the quota table the tests give the driver: what the fake assumes
// (the real table is empty until a live run confirms a series).
var confirmed = map[string][]string{"e2": {"CPUS"}, "n2": {"CPUS", "N2_CPUS"}}

// snapshotRow is the part of a /table/server row the tests need.
type snapshotRow struct {
	VendorID     string `json:"vendor_id"`
	APIReference string `json:"api_reference"`
	VCPUs        int32  `json:"vcpus"`
	MemoryMiB    int32  `json:"memory_amount"`
	Arch         string `json:"cpu_architecture"`
}

var (
	snapshotOnce sync.Once
	snapshot     []snapshotRow
)

// gcpServers returns the GCP rows of the committed Keeper snapshot: the full type
// list of the vendor.
func gcpServers(t testing.TB) []snapshotRow {
	t.Helper()
	snapshotOnce.Do(func() {
		f, err := os.Open(filepath.Join(testutil.RepoRoot(), "test", "data", "sc-snapshot", "tables", "server.json.gz"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		var rows []snapshotRow
		if err := json.NewDecoder(zr).Decode(&rows); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.VendorID == "gcp" {
				snapshot = append(snapshot, r)
			}
		}
	})
	if len(snapshot) == 0 {
		t.Fatal("the snapshot has no GCP servers")
	}
	return snapshot
}

// env is a driver over a fake project.
type env struct {
	d     *gcp.Driver
	c     *fake.Compute
	clock *clocktesting.FakeClock
}

type envOptions struct {
	cluster string
	// bare skips the default network and the snapshot's machine types.
	bare   bool
	prices gcp.PriceSource
}

func newEnv(t testing.TB, o envOptions) *env {
	t.Helper()
	c := fake.NewSeeded(testProject)
	if !o.bare {
		c.AddDefaultNetwork()
		for _, r := range gcpServers(t) {
			arch := "X86_64"
			if r.Arch == "arm64" {
				arch = "ARM64"
			}
			for _, z := range []string{fake.SeedZoneA, fake.SeedZoneB} {
				c.AddMachineType(z, r.APIReference, r.VCPUs, r.MemoryMiB, arch)
			}
		}
	}
	// The image the conformance suite launches with, in the project's own scope.
	c.AddImageSpec(fake.ImageSpec{Project: testProject, Name: "image", Arch: "X86_64"})
	return restart(t, c, o)
}

func restart(t testing.TB, c *fake.Compute, o envOptions) *env {
	t.Helper()
	cluster := o.cluster
	if cluster == "" {
		cluster = testCluster
	}
	clock := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	opts := []gcp.Option{gcp.WithAPI(c), gcp.WithProject(testProject), gcp.WithConfirmedSeries(confirmed),
		gcp.WithCallTimeouts(5*time.Second, 5*time.Second), gcp.WithLaunchWait(10 * time.Second)}
	if o.prices != nil {
		opts = append(opts, gcp.WithPrices(o.prices))
	}
	d := gcp.New(opts...)
	if err := d.Init(context.Background(), vendors.Config{ClusterName: cluster, Clock: clock, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	return &env{d: d, c: c, clock: clock}
}
