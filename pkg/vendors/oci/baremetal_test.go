/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

const bmType = "BM.Standard.E2.64"

// bareMetalEnv is a driver whose controller has the given registration timeout
// and whose region has a fixed bare metal shape.
func bareMetalEnv(t *testing.T, timeout time.Duration) *env {
	t.Helper()
	c := newCloud(t)
	c.AddShape(fake.SeedRegion, fake.FixedShape(bmType, 64, 512, "standard-e2-core-ad-count"))
	clk := clocktesting.NewFakeClock(time.Now().Truncate(time.Second))
	c.SetClock(clk.Now)
	d := driver.New(
		driver.WithClients(driver.Clients{Compute: c, Network: c, Identity: c, Limits: c}),
		driver.WithCompartment(fake.SeedCompartment), driver.WithTenancy(fake.SeedTenancy),
	)
	cfg := vendors.Config{ClusterName: testCluster, Clock: clk, Log: logr.Discard(), RegistrationTimeout: timeout}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return &env{d: d, c: c, clock: clk}
}

// A bare metal shape launches on demand, as a fixed shape without a shape
// configuration, once the registration timeout covers its boot.
func TestBareMetalLaunchesOnDemand(t *testing.T) {
	e := bareMetalEnv(t, driver.MinBareMetalRegistrationTimeout)
	if why, refused := launchable(e.d, bmType); refused {
		t.Fatalf("LaunchableType(%s) = %q; want offered", bmType, why)
	}
	inst := e.create(t, request(bmType))
	l := e.c.Launches()[0]
	if *l.Shape != bmType || l.ShapeConfig != nil || l.PreemptibleInstanceConfig != nil {
		t.Errorf("shape %q, shape config %+v, preemptible %+v", *l.Shape, l.ShapeConfig, l.PreemptibleInstanceConfig)
	}
	if inst.CapacityType != vendors.CapacityOnDemand || inst.Type != bmType {
		t.Errorf("instance %+v", inst)
	}
}

// Bare metal has no preemptible capacity: a spot request is refused before any
// launch call.
func TestBareMetalIsNeverSpot(t *testing.T) {
	e := bareMetalEnv(t, driver.MinBareMetalRegistrationTimeout)
	req := request(bmType)
	req.CapacityType = vendors.CapacitySpot
	_, err := e.d.CreateInstance(t.Context(), req)
	if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != driver.CodeBareMetalNoSpot {
		t.Fatalf("CreateInstance = %v; want Unsupported %s", err, driver.CodeBareMetalNoSpot)
	}
	if n := e.c.Calls("LaunchInstance"); n != 0 {
		t.Errorf("%d launch calls for a spot bare metal shape", n)
	}
}

// A registration timeout shorter than a bare metal node needs (the default of 15
// minutes included) keeps bare metal shapes out; VM shapes are not affected.
func TestBareMetalNeedsARegistrationWindow(t *testing.T) {
	for _, timeout := range []time.Duration{0, 15 * time.Minute} {
		e := bareMetalEnv(t, timeout)
		if _, refused := launchable(e.d, bmType); !refused {
			t.Errorf("timeout %s: %s is offered", timeout, bmType)
		}
		if why, refused := launchable(e.d, testType); refused {
			t.Errorf("timeout %s: %s refused: %s", timeout, testType, why)
		}
		_, err := e.d.CreateInstance(t.Context(), request(bmType))
		if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != driver.CodeBareMetalWindow {
			t.Errorf("timeout %s: CreateInstance = %v; want Unsupported %s", timeout, err, driver.CodeBareMetalWindow)
		}
		if n := e.c.Calls("LaunchInstance"); n != 0 {
			t.Errorf("timeout %s: %d launch calls", timeout, n)
		}
	}
}

// An Ampere bare metal shape launches on demand with the arm64 launch options.
func TestBareMetalArm64Launch(t *testing.T) {
	const shape = "BM.Standard.A1.160"
	e := bareMetalEnv(t, driver.MinBareMetalRegistrationTimeout)
	e.c.AddShape(fake.SeedRegion, fake.FixedShape(shape, 160, 1024, "bm-standard-a1-160-count"))
	req := request(shape)
	req.Arch = "arm64"
	e.create(t, req)
	l := e.c.Launches()[0]
	if *l.Shape != shape || l.ShapeConfig != nil || l.LaunchOptions == nil {
		t.Errorf("shape %q, shape config %+v, launch options %+v", *l.Shape, l.ShapeConfig, l.LaunchOptions)
	}
}
