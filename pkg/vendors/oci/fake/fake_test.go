/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/limits"

	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

var ad = fake.ADFull(fake.SeedZoneA)

func seeded() *fake.Cloud {
	c := fake.NewSeeded()
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.E5.Flex", 94, 1049, 1, 64, 94, 1049,
		"standard-e5-core-count", "standard-e5-memory-count"))
	c.AddShape(fake.SeedRegion, fake.FixedShape("VM.Standard2.4", 4, 60, "standard2-core-count"))
	return c
}

func launch(token string, ocpus, mem float32) core.LaunchInstanceRequest {
	return core.LaunchInstanceRequest{
		LaunchInstanceDetails: core.LaunchInstanceDetails{
			CompartmentId: common.String(fake.SeedCompartment), AvailabilityDomain: &ad, Shape: new("VM.Standard.E5.Flex"),
			ShapeConfig:   &core.LaunchInstanceShapeConfigDetails{Ocpus: &ocpus, MemoryInGBs: &mem},
			SourceDetails: core.InstanceSourceViaImageDetails{ImageId: common.String(fake.SeedImage)},
			CreateVnicDetails: &core.CreateVnicDetails{SubnetId: common.String(fake.SeedSubnet),
				AssignPublicIp: new(true)},
			DisplayName: new("n"),
		},
		OpcRetryToken: &token,
	}
}

func status(err error) int {
	var se common.ServiceError
	if errors.As(err, &se) {
		return se.GetHTTPStatusCode()
	}
	return 0
}

// A replay of a launch that succeeded returns the original; the same token with another
// request is a 409; a failed launch records nothing.
func TestRetryTokenSemantics(t *testing.T) {
	ctx := context.Background()
	c := seeded()
	first, err := c.LaunchInstance(ctx, fake.SeedRegion, launch("tok", 2, 16))
	if err != nil || first.LifecycleState != core.InstanceLifecycleStateProvisioning {
		t.Fatalf("launch: %+v, %v", first.Instance, err)
	}
	again, err := c.LaunchInstance(ctx, fake.SeedRegion, launch("tok", 2, 16))
	if err != nil || *again.Id != *first.Id {
		t.Errorf("a replay returned %v, %v; want the original %s", again.Id, err, *first.Id)
	}
	if _, err := c.LaunchInstance(ctx, fake.SeedRegion, launch("tok", 4, 32)); status(err) != http.StatusConflict {
		t.Errorf("the same token with another size: %v, want 409", err)
	}
	// A launch that fails leaves its token unused.
	c.Inject("LaunchInstance", fake.Error(500, "InternalError", "Out of host capacity."), 1)
	if _, err := c.LaunchInstance(ctx, fake.SeedRegion, launch("tok2", 2, 16)); status(err) != 500 {
		t.Fatalf("injected: %v", err)
	}
	if _, err := c.LaunchInstance(ctx, fake.SeedRegion, launch("tok2", 4, 32)); err != nil {
		t.Errorf("the token of a failed launch is used: %v", err)
	}
}

func TestLaunchValidation(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		edit   func(*core.LaunchInstanceRequest)
		status int
	}{
		"memory per OCPU":    {func(r *core.LaunchInstanceRequest) { r.ShapeConfig.MemoryInGBs = common.Float32(1) }, 400},
		"too many OCPUs":     {func(r *core.LaunchInstanceRequest) { r.ShapeConfig.Ocpus = common.Float32(200) }, 400},
		"no shape config":    {func(r *core.LaunchInstanceRequest) { r.ShapeConfig = nil }, 400},
		"unknown shape":      {func(r *core.LaunchInstanceRequest) { r.Shape = new("VM.Nope") }, 400},
		"unknown AD":         {func(r *core.LaunchInstanceRequest) { r.AvailabilityDomain = new("x:NOPE-AD-1") }, 400},
		"wrong compartment":  {func(r *core.LaunchInstanceRequest) { r.CompartmentId = new("ocid1.compartment.oc1..other") }, 404},
		"no subnet":          {func(r *core.LaunchInstanceRequest) { r.CreateVnicDetails.SubnetId = nil }, 400},
		"no image":           {func(r *core.LaunchInstanceRequest) { r.SourceDetails = core.InstanceSourceViaImageDetails{} }, 400},
		"metadata too large": {func(r *core.LaunchInstanceRequest) { r.Metadata = map[string]string{"k": string(make([]byte, 32001))} }, 400},
	} {
		t.Run(name, func(t *testing.T) {
			r := launch("t", 2, 16)
			tc.edit(&r)
			if _, err := seeded().LaunchInstance(ctx, fake.SeedRegion, r); status(err) != tc.status {
				t.Errorf("status %d (%v), want %d", status(err), err, tc.status)
			}
		})
	}
	if _, err := seeded().LaunchInstance(ctx, fake.SeedRegionUnsubscribed, launch("t", 2, 16)); status(err) != 404 {
		t.Errorf("a launch in an unsubscribed region: %v", err)
	}
	fixed := launch("t", 4, 60)
	fixed.Shape = new("VM.Standard2.4")
	if _, err := seeded().LaunchInstance(ctx, fake.SeedRegion, fixed); status(err) != 400 {
		t.Errorf("a shapeConfig on a fixed shape: %v", err)
	}
	fixed.ShapeConfig = nil
	if _, err := seeded().LaunchInstance(ctx, fake.SeedRegion, fixed); err != nil {
		t.Errorf("a fixed shape: %v", err)
	}
}

// The availability of a limit is computed from the fake's instances, and a launch past it
// fails with LimitExceeded naming the limit.
func TestLimits(t *testing.T) {
	ctx := context.Background()
	c := seeded()
	c.SetLimit(fake.SeedRegion, ad, "standard-e5-core-count", 4)
	if _, err := c.LaunchInstance(ctx, fake.SeedRegion, launch("a", 2, 16)); err != nil {
		t.Fatal(err)
	}
	read := func(name string) (used, avail int64) {
		t.Helper()
		res, err := c.GetResourceAvailability(ctx, fake.SeedRegion, limits.GetResourceAvailabilityRequest{
			ServiceName: new("compute"), LimitName: &name, CompartmentId: common.String(fake.SeedTenancy),
			AvailabilityDomain: &ad})
		if err != nil {
			t.Fatal(err)
		}
		return *res.Used, *res.Available
	}
	if used, avail := read("standard-e5-core-count"); used != 2 || avail != 2 {
		t.Errorf("used %d, available %d; want 2 and 2", used, avail)
	}
	if used, _ := read("standard-e5-memory-count"); used != 16 {
		t.Errorf("memory used %d", used)
	}
	_, err := c.LaunchInstance(ctx, fake.SeedRegion, launch("b", 4, 16))
	var se common.ServiceError
	if !errors.As(err, &se) || se.GetCode() != "LimitExceeded" || status(err) != 400 {
		t.Errorf("a launch past the limit: %v", err)
	}
	// The wrong scope is refused, a region-scoped limit is read without an AD.
	c.SetScope("standard2-core-count", limits.LimitDefinitionSummaryScopeTypeRegion)
	if _, err := c.GetResourceAvailability(ctx, fake.SeedRegion, limits.GetResourceAvailabilityRequest{
		ServiceName: new("compute"), LimitName: new("standard2-core-count"),
		CompartmentId: common.String(fake.SeedTenancy), AvailabilityDomain: &ad}); status(err) != 400 {
		t.Errorf("an AD for a region-scoped limit: %v", err)
	}
	defs, err := c.ListLimitDefinitions(ctx, fake.SeedRegion, limits.ListLimitDefinitionsRequest{
		CompartmentId: common.String(fake.SeedTenancy), ServiceName: new("compute")})
	if err != nil || len(defs.Items) != 3 {
		t.Errorf("definitions %v, %v", defs.Items, err)
	}
}

func TestListInstancesFiltersAndPages(t *testing.T) {
	ctx := context.Background()
	c := seeded()
	c.SetPageSize(2)
	for i, name := range []string{"a", "b", "c", "a"} {
		r := launch(string(rune('t'+i)), 1, 4)
		r.DisplayName = new(name)
		if _, err := c.LaunchInstance(ctx, fake.SeedRegion, r); err != nil {
			t.Fatal(err)
		}
	}
	count := func(req core.ListInstancesRequest) (n, pages int) {
		req.CompartmentId = common.String(fake.SeedCompartment)
		for {
			res, err := c.ListInstances(ctx, fake.SeedRegion, req)
			if err != nil {
				t.Fatal(err)
			}
			n, pages = n+len(res.Items), pages+1
			if res.OpcNextPage == nil {
				return n, pages
			}
			req.Page = res.OpcNextPage
		}
	}
	if n, pages := count(core.ListInstancesRequest{}); n != 4 || pages != 2 {
		t.Errorf("%d instances in %d pages, want 4 in 2", n, pages)
	}
	if n, _ := count(core.ListInstancesRequest{DisplayName: new("a")}); n != 2 {
		t.Errorf("%d instances named a", n)
	}
	if n, _ := count(core.ListInstancesRequest{LifecycleState: core.InstanceLifecycleStateStopped}); n != 0 {
		t.Errorf("%d stopped instances", n)
	}
	if _, err := c.ListInstances(ctx, fake.SeedRegion, core.ListInstancesRequest{
		CompartmentId: new("ocid1.compartment.oc1..other")}); status(err) != 404 {
		t.Errorf("another compartment: %v", err)
	}
}

func TestInjectAndCalls(t *testing.T) {
	c := seeded()
	c.Inject("GetInstance", fake.Error(500, "InternalError", "x"), 2)
	for i := range 3 {
		_, err := c.GetInstance(context.Background(), fake.SeedRegion, core.GetInstanceRequest{InstanceId: new("x")})
		if (i < 2) != (status(err) == 500) {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if c.Calls("GetInstance") != 3 {
		t.Errorf("%d calls", c.Calls("GetInstance"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetInstance(ctx, fake.SeedRegion, core.GetInstanceRequest{}); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled context: %v", err)
	}
}
