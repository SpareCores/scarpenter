/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/oracle/oci-go-sdk/v65/common"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

// The fake satisfies the narrow interfaces, and so does the SDK adapter.
var (
	_ driver.ComputeAPI  = (*fake.Cloud)(nil)
	_ driver.NetworkAPI  = (*fake.Cloud)(nil)
	_ driver.IdentityAPI = (*fake.Cloud)(nil)
	_ driver.LimitsAPI   = (*fake.Cloud)(nil)
	_ driver.ComputeAPI  = (*driver.SDK)(nil)
)

func TestIdentityOfTheDriver(t *testing.T) {
	d := driver.New()
	if d.Vendor() != "oci" || driver.Name != "oci" || driver.Vendor != "oci" {
		t.Errorf("vendor %q", d.Vendor())
	}
	if d.TagKey(vendors.TagCluster) != "scarpenter-cluster" || d.TagKey(vendors.TagUse) != "scarpenter-use" {
		t.Error("tag keys")
	}
	if _, ok := vendors.Driver(d).(vendors.QuotaReporter); !ok {
		t.Error("the driver is no QuotaReporter")
	}
	// Step (b) registers nothing: the registry does not know the vendor yet.
	if got := vendors.Registered(); slices.Contains(got, "oci") {
		t.Errorf("the registry already holds oci: %v", got)
	}
}

func TestInit(t *testing.T) {
	log := logr.Discard()
	clk := clocktesting.NewFakeClock(time.Now())
	c := fake.NewSeeded()
	clients := driver.WithClients(driver.Clients{Compute: c, Network: c, Identity: c, Limits: c})
	for name, tc := range map[string]struct {
		opts []driver.Option
		ok   bool
	}{
		"complete":       {[]driver.Option{clients, driver.WithCompartment(fake.SeedCompartment), driver.WithTenancy(fake.SeedTenancy)}, true},
		"no compartment": {[]driver.Option{clients, driver.WithTenancy(fake.SeedTenancy)}, false},
		"a tenancy as the compartment": {[]driver.Option{clients, driver.WithCompartment(fake.SeedTenancy),
			driver.WithTenancy(fake.SeedTenancy)}, false},
		"no tenancy":     {[]driver.Option{clients, driver.WithCompartment(fake.SeedCompartment)}, false},
		"no credentials": {[]driver.Option{driver.WithCompartment(fake.SeedCompartment), driver.WithTenancy(fake.SeedTenancy)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := driver.New(tc.opts...).Init(context.Background(), vendors.Config{ClusterName: "c", Clock: clk, Log: log})
			if (err == nil) != tc.ok {
				t.Errorf("Init = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	t.Run("the tenancy comes from the credentials, and Init makes no call", func(t *testing.T) {
		provider := common.NewRawConfigurationProvider(fake.SeedTenancy, "ocid1.user.oc1..u", "eu-frankfurt-1", "aa:bb", "key", nil)
		d := driver.New(driver.WithConfigProvider(provider), driver.WithCompartment(fake.SeedCompartment))
		if err := d.Init(context.Background(), vendors.Config{Clock: clk, Log: log}); err != nil {
			t.Fatal(err)
		}
		if d.Compartment() != fake.SeedCompartment {
			t.Errorf("compartment %q", d.Compartment())
		}
	})
}

func TestRegionsAndZones(t *testing.T) {
	e := newEnv(t)
	regions, err := e.d.Regions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(regions))
	for _, r := range regions {
		names = append(names, r.Name)
	}
	// The region that is not READY is not one the credentials can use.
	if want := []string{fake.SeedRegion, fake.SeedRegionOne, fake.SeedRegionB}; !slices.Equal(names, want) {
		t.Errorf("regions %v, want %v", names, want)
	}
	for _, tc := range []struct {
		region string
		want   []vendors.Zone
	}{
		{fake.SeedRegion, []vendors.Zone{
			{ID: "eu-frankfurt-1-ad-1", Name: "EU-FRANKFURT-1-AD-1"}, {ID: "eu-frankfurt-1-ad-2", Name: "EU-FRANKFURT-1-AD-2"},
			{ID: "eu-frankfurt-1-ad-3", Name: "EU-FRANKFURT-1-AD-3"}}},
		{fake.SeedRegionB, []vendors.Zone{
			{ID: "us-ashburn-1-ad-1", Name: "US-ASHBURN-AD-1"}, {ID: "us-ashburn-1-ad-2", Name: "US-ASHBURN-AD-2"},
			{ID: "us-ashburn-1-ad-3", Name: "US-ASHBURN-AD-3"}}},
		{fake.SeedRegionOne, []vendors.Zone{{ID: "uk-london-1-ad-1", Name: "UK-LONDON-1-AD-1"}}},
	} {
		zones, err := e.d.Zones(t.Context(), tc.region)
		if err != nil || !slices.Equal(zones, tc.want) {
			t.Errorf("Zones(%s) = %v, %v; want %v", tc.region, zones, err, tc.want)
		}
	}
	if _, err := e.d.Zones(t.Context(), fake.SeedRegionUnsubscribed); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("Zones of an unsubscribed region: %v", err)
	}
	if _, err := e.d.Zones(t.Context(), "no-such-region-1"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("Zones of an unknown region: %v", err)
	}
}

// The facts are cached for an hour: a new subscription shows after the cache expires.
func TestFactsAreCached(t *testing.T) {
	e := newEnv(t)
	for range 3 {
		if _, err := e.d.Regions(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.d.Zones(t.Context(), testRegion); err != nil {
			t.Fatal(err)
		}
	}
	if n, m := e.c.Calls("ListRegionSubscriptions"), e.c.Calls("ListAvailabilityDomains"); n != 1 || m != 1 {
		t.Errorf("%d subscription and %d availability domain calls, want 1 and 1", n, m)
	}
	e.create(t, request(testType))
	e.create(t, func() vendors.CreateRequest {
		r := request(testType)
		r.IdempotencyKey, r.Tags[vendors.TagNodeClaim], r.Node.Name = "uid-2", "claim-2", "claim-2"
		return r
	}())
	if n := e.c.Calls("ListShapes"); n != 1 {
		t.Errorf("%d ListShapes calls for two launches in one AD, want 1", n)
	}
	e.clock.Step(61 * time.Minute)
	if _, err := e.d.Regions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := e.c.Calls("ListRegionSubscriptions"); n != 2 {
		t.Errorf("%d subscription calls after the cache expired, want 2", n)
	}
}

func TestResolveImageIsStepC(t *testing.T) {
	e := newEnv(t)
	if _, err := e.d.ResolveImage(t.Context(), vendors.ImageRequest{}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("ResolveImage = %v, want Fatal until step (c)", err)
	}
}
