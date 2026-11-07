/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

const (
	testCluster = "my-cluster"
	testRegion  = "us-east-1"
	testZone    = "us-east-1a"
	testType    = "m7i.large"
)

// snapshotRow is the part of a /table/server row the tests need.
type snapshotRow struct {
	VendorID     string  `json:"vendor_id"`
	APIReference string  `json:"api_reference"`
	Status       string  `json:"status"`
	VCPUs        int32   `json:"vcpus"`
	Arch         string  `json:"cpu_architecture"`
	StorageSize  float64 `json:"storage_size"`
	Storages     []struct {
		Size float64 `json:"size"`
	} `json:"storages"`
}

var (
	snapshotOnce sync.Once
	snapshot     []snapshotRow
)

// awsServers returns the AWS rows of the committed Keeper snapshot: the full type
// list of the vendor, active and inactive (plan 04 review N7).
func awsServers(t testing.TB) []snapshotRow {
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
			if r.VendorID == "aws" {
				snapshot = append(snapshot, r)
			}
		}
	})
	if len(snapshot) == 0 {
		t.Fatal("the snapshot has no AWS servers")
	}
	return snapshot
}

// snapshotTypes builds the fake's instance types from the snapshot: the vCPUs,
// the architecture, the instance store, and the features by family rules
// (burstable t families, ENA Express on the n and gen 7 families, Xen for the
// old generations).
func snapshotTypes(t testing.TB) []fake.InstanceType {
	t.Helper()
	rows := awsServers(t)
	out := make([]fake.InstanceType, 0, len(rows))
	for _, r := range rows {
		fam, _, _ := strings.Cut(r.APIReference, ".")
		it := fake.InstanceType{
			Name: r.APIReference, VCPUs: r.VCPUs, Arch: r.Arch, Burstable: strings.HasPrefix(fam, "t"),
			ENAExpress: strings.HasSuffix(fam, "n") && !strings.HasPrefix(fam, "t"),
			NonNitro:   fam == "m1" || fam == "m2" || fam == "m3" || fam == "c1" || fam == "t1" || fam == "t2",
		}
		if it.Arch == "" {
			it.Arch = "x86_64"
		}
		if r.StorageSize > 0 && len(r.Storages) > 0 && !it.NonNitro {
			it.LocalDisks = int32(len(r.Storages))
			it.DiskGB = int64(r.StorageSize)
		}
		out = append(out, it)
	}
	return out
}

// env is a driver over a fake account.
type env struct {
	cluster string
	d       *aws.Driver
	acct    *fake.Account
	clock   *clocktesting.FakeClock
}

type envOptions struct {
	cluster string
	types   []fake.InstanceType
	regions []fake.RegionSpec
	prices  aws.PriceSource
	timeout time.Duration
}

var smallTypes = []fake.InstanceType{
	{Name: "m7i.large", VCPUs: 2, ENAExpress: false},
	{Name: "m7i.xlarge", VCPUs: 4},
	{Name: "c7gn.large", VCPUs: 2, Arch: "arm64", ENAExpress: true},
	{Name: "t3.micro", VCPUs: 2, Burstable: true},
	{Name: "m6id.large", VCPUs: 2, LocalDisks: 1, DiskGB: 118},
	{Name: "p4d.24xlarge", VCPUs: 96},
	{Name: "hpc7a.96xlarge", VCPUs: 192},
	{Name: "u-3tb1.56xlarge", VCPUs: 448},
	{Name: "mac2.metal", VCPUs: 8},
	{Name: "m1.small", VCPUs: 1, NonNitro: true},
}

func newEnv(t testing.TB, o envOptions) *env {
	t.Helper()
	clk := clocktesting.NewFakeClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	opts := []fake.Option{fake.WithClock(clk)}
	if o.regions != nil {
		opts = append(opts, fake.WithRegions(o.regions...))
	}
	acct := fake.New(opts...)
	types := o.types
	if types == nil {
		types = smallTypes
	}
	acct.SetInstanceTypes(types)
	for _, r := range acct.Regions() {
		for _, code := range []string{"L-1216C47A", "L-34B43A08"} {
			acct.SetQuota(r, code, 1000)
		}
		for _, arch := range []string{"x86_64", "arm64"} {
			acct.AddImage(r, flatcarImage(r, arch, "stable", "4757.2.3"))
		}
	}
	return newEnvOver(t, acct, clk, o)
}

func newEnvOver(t testing.TB, acct *fake.Account, clk *clocktesting.FakeClock, o envOptions) *env {
	t.Helper()
	prices := o.prices
	if prices == nil {
		prices = aws.StaticPrices{}
	}
	cluster := o.cluster
	if cluster == "" {
		cluster = testCluster
	}
	d := aws.New(
		aws.WithClientFactory(func(_ context.Context, region string) (aws.Clients, error) {
			return aws.Clients{EC2: acct.EC2(region), Quotas: acct.Quotas(region)}, nil
		}),
		aws.WithPrices(prices),
	)
	if o.timeout > 0 {
		aws.WithCallTimeouts(o.timeout, o.timeout)(d)
	}
	if err := d.Init(context.Background(), vendors.Config{ClusterName: cluster, Clock: clk, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	return &env{d: d, acct: acct, clock: clk, cluster: cluster}
}

// restart returns a second driver over the same account (a restarted controller).
func (e *env) restart(t testing.TB) *env {
	return newEnvOver(t, e.acct, e.clock, envOptions{cluster: e.cluster})
}

func flatcarImage(region, arch, channel, version string) fake.Image {
	name := "Flatcar-" + channel + "-" + version + "-hvm"
	if arch == "arm64" {
		name = "Flatcar-" + channel + "-" + version + "-arm64-hvm"
	}
	return fake.Image{
		ID: "ami-" + strings.NewReplacer("-", "", "x86_64", "0", "arm64", "1").Replace(region+arch) + "0000", Name: name,
		Owner: aws.DefaultImageOwner, Arch: arch, RootDevice: "/dev/xvda", RootGiB: 8,
	}
}

var launchCounter int

// request is a launch request of the tests: on-demand in the first zone.
func request(region, zone, typ, capacity string) vendors.CreateRequest {
	launchCounter++
	name := "claim-" + typ + "-" + region + "-" + strings.Repeat("x", launchCounter%5) + string(rune('a'+launchCounter%26))
	return vendors.CreateRequest{
		Region: region, Zone: zone, InstanceType: typ, CapacityType: capacity, Arch: "amd64",
		Image: vendors.Image{Ref: "ami-0000000000000image"}, UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: testCluster, vendors.TagNodeClaim: name, vendors.TagNodePool: "pool",
			vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: "uid-" + name,
	}
}

func nil2clock() *clocktesting.FakeClock {
	return clocktesting.NewFakeClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
}
