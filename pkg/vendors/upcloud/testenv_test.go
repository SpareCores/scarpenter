/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package upcloud_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	uc "github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/vendortest"
)

const (
	testCluster  = "my-cluster"
	testZone     = "fi-hel1"
	testType     = "PREMIUM-2xCPU-4GB"
	testTemplate = "image"
	testVersion  = "4757.2.3"
)

// snapshotRow is the part of a /table/server row the tests need.
type snapshotRow struct {
	VendorID     string  `json:"vendor_id"`
	APIReference string  `json:"api_reference"`
	Status       string  `json:"status"`
	VCPUs        int     `json:"vcpus"`
	Memory       int     `json:"memory_amount"`
	StorageSize  float64 `json:"storage_size"`
}

var (
	snapshotOnce sync.Once
	snapshot     []snapshotRow
)

// upcloudServers returns the UpCloud rows of the committed Keeper snapshot: the
// full plan list of the vendor, active and inactive.
func upcloudServers(t testing.TB) []snapshotRow {
	t.Helper()
	snapshotOnce.Do(func() {
		rows := vendortest.SnapshotRows[snapshotRow](t, "server", "upcloud")
		for _, r := range rows {
			if r.VendorID == "upcloud" {
				snapshot = append(snapshot, r)
			}
		}
	})
	if len(snapshot) == 0 {
		t.Fatal("the snapshot has no UpCloud servers")
	}
	return snapshot
}

// snapshotPlans builds the fake's plans from the snapshot: cores, memory and the
// included storage; the tier by family (Starter and Developer plans include
// Standard storage, the others MaxIOPS: plan 19, unverified), none for Cloud Native
// and GPU plans.
func snapshotPlans(t testing.TB) []fake.PlanSpec {
	t.Helper()
	rows := upcloudServers(t)
	out := make([]fake.PlanSpec, 0, len(rows))
	for _, r := range rows {
		p := upcloud.Plan{Name: r.APIReference, CoreNumber: r.VCPUs, MemoryAmount: r.Memory}
		if r.StorageSize > 0 {
			p.StorageSize = int(r.StorageSize)
			p.StorageTier = upcloud.StorageTierMaxIOPS
			if strings.HasPrefix(r.APIReference, "STARTER-") || strings.HasPrefix(r.APIReference, "DEV-") {
				p.StorageTier = upcloud.StorageTierStandard
			}
		}
		if strings.HasPrefix(r.APIReference, "GPU-") {
			p.GPUAmount = 1
		}
		out = append(out, fake.PlanSpec{Plan: p})
	}
	return out
}

// bigLimits are account limits high enough to launch.
var bigLimits = upcloud.ResourceLimits{
	Cores: 100000, Memory: 100000000, PublicIPv4: 1000, Networks: 1000,
	StorageMaxIOPS: 1000000, StorageSSD: 1000000, StorageHDD: 1000000,
}

// flatcarLabels are the labels of the template of the pinned release.
func flatcarLabels(tier, version string) map[string]string {
	return map[string]string{
		uc.LabelImage: uc.ImageFlatcar, uc.LabelFlatcarChannel: "stable", uc.LabelFlatcarVersion: version,
		uc.LabelFlatcarArch: "amd64", uc.LabelFlatcarTier: tier, uc.LabelUse: "true",
	}
}

// env is a driver over a fake account.
type env struct {
	t     *testing.T
	d     *uc.Driver
	f     *fake.Fake
	clock *clocktesting.FakeClock
	opts  envOptions
}

type envOptions struct {
	fake    fake.Options
	cluster string
	// noTemplate leaves the account without a Flatcar template.
	noTemplate bool
	// templateID gives the template a fixed ID ("image", the conformance suite's reference).
	templateID bool
	// shortKeys lets the fake take one-character label keys (the conformance seeds use them).
	shortKeys bool
	prices    uc.PriceSource
	reader    client.Reader
	driver    []uc.Option
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	if o.cluster == "" {
		o.cluster = testCluster
	}
	if o.fake.Plans == nil {
		o.fake.Plans = snapshotPlans(t)
	}
	if o.fake.Limits == (upcloud.ResourceLimits{}) {
		o.fake.Limits = bigLimits
	}
	o.fake.AutoSettle = true
	o.fake.ShortLabelKeys = o.shortKeys
	o.fake.Zones = []string{"fi-hel1", "de-fra1", "nl-ams1"}
	if o.fake.MaxNetworks == 0 {
		o.fake.MaxNetworks = 100
	}
	f := fake.New(o.fake)
	if !o.noTemplate {
		if o.templateID {
			f.AddTemplateWithID(testTemplate, testZone, "flatcar-stable-"+testVersion+"-amd64-maxiops",
				upcloud.StorageTierMaxIOPS, 10, flatcarLabels(upcloud.StorageTierMaxIOPS, testVersion))
		} else {
			f.AddTemplate(testZone, "flatcar-stable-"+testVersion+"-amd64-maxiops", upcloud.StorageTierMaxIOPS, 10,
				flatcarLabels(upcloud.StorageTierMaxIOPS, testVersion))
		}
	}
	return attach(t, f, o)
}

// attach builds a driver over an existing fake (a restart of the controller).
func attach(t *testing.T, f *fake.Fake, o envOptions) *env {
	t.Helper()
	clock := clocktesting.NewFakeClock(time.Now())
	opts := append([]uc.Option{uc.WithAPI(f)}, o.driver...)
	if o.prices != nil {
		opts = append(opts, uc.WithPrices(o.prices))
	}
	d := uc.New(opts...)
	if err := d.Init(context.Background(), vendors.Config{ClusterName: o.cluster, Clock: clock, Log: logr.Discard(), Reader: o.reader}); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, d: d, f: f, clock: clock, opts: o}
}

func (e *env) ctx() context.Context { return context.Background() }

// template returns the UUID of the first template of the zone.
func (e *env) template(zone string) string {
	e.t.Helper()
	for _, s := range e.f.Storages() {
		if s.Zone == zone && s.Type == upcloud.StorageTypeTemplate {
			return s.UUID
		}
	}
	e.t.Fatalf("no template in %s", zone)
	return ""
}

// request builds a create request for the plan in the zone with the template.
func (e *env) request(name, plan string) vendors.CreateRequest {
	e.t.Helper()
	return vendors.CreateRequest{
		Region: testZone, Zone: testZone, InstanceType: plan, CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image:    vendors.Image{Ref: e.template(testZone)},
		UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: e.opts.cluster, vendors.TagNodeClaim: name, vendors.TagNodePool: "pool",
			vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: "uid-" + name,
		Node:           vendors.NodeInfo{Name: name, ProviderID: uc.ProviderID(testZone, name)},
	}
}
