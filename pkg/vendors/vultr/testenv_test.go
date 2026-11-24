/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vendortest"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

const (
	testCluster = "my-cluster"
	testRegion  = "ewr"
	testType    = "vc2-1c-1gb"
	testRelease = "stable-4081.2.0"
)

// snapshotRow is the part of a snapshot server row the tests need.
type snapshotRow struct {
	VendorID     string  `json:"vendor_id"`
	APIReference string  `json:"api_reference"`
	Status       string  `json:"status"`
	StorageSize  float64 `json:"storage_size"`
}

var (
	snapshotOnce sync.Once
	snapshot     []snapshotRow
)

// vultrServers returns the Vultr rows of the committed Keeper snapshot: the full
// type list of the vendor, active and inactive.
func vultrServers(t testing.TB) []snapshotRow {
	t.Helper()
	snapshotOnce.Do(func() {
		rows := vendortest.SnapshotRows[snapshotRow](t, "server", "vultr")
		for _, r := range rows {
			if r.VendorID == "vultr" {
				snapshot = append(snapshot, r)
			}
		}
	})
	if len(snapshot) == 0 {
		t.Fatal("the snapshot has no Vultr servers")
	}
	return snapshot
}

// drvEnv is a driver over a fake account.
type drvEnv struct {
	acct *fake.Account
	d    *vultr.Driver
	clk  *clocktesting.FakeClock
	// snap is the snapshot of testRelease.
	snap string
}

// newAccount builds the fake account: two regions, three plans with a disk, a
// diskless vx1 and a GPU plan, and the pinned Flatcar snapshot.
func newAccount(clk *clocktesting.FakeClock) (*fake.Account, string) {
	a := fake.New()
	a.SetClock(clk.Now)
	for _, p := range []struct {
		id   string
		disk int
	}{{testType, 25}, {"vc2-2c-4gb", 80}, {"vhf-1c-1gb", 32}, {"vx1-g-2c-8g", 1}, {"vcg-a16-6c-64g-16vram", 350}} {
		a.AddPlan(p.id, 1, 1024, p.disk)
	}
	a.AddRegion("ewr", testType, "vc2-2c-4gb", "vhf-1c-1gb", "vx1-g-2c-8g", "vcg-a16-6c-64g-16vram")
	a.AddRegion("fra", testType)
	return a, a.AddSnapshotAs("", "flatcar-"+testRelease+"-amd64", fake.SnapshotComplete, time.Time{})
}

// driverOver initialises a driver on an account.
func driverOver(t testing.TB, a *fake.Account, clk *clocktesting.FakeClock, cluster string) *vultr.Driver {
	t.Helper()
	d := vultr.New(vultr.WithAPI(a))
	if err := d.Init(context.Background(), vendors.Config{ClusterName: cluster, Clock: clk, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	return d
}

func newDrvEnv(t testing.TB) *drvEnv {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	a, snap := newAccount(clk)
	return &drvEnv{acct: a, d: driverOver(t, a, clk, testCluster), clk: clk, snap: snap}
}
