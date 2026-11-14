/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

// drivers maps the drivers the conformance fixture built to their fakes, so that
// the hooks (which only get the driver) can reach the tenancy.
var (
	driversMu sync.Mutex
	drivers   = map[vendors.Driver]*env{}
)

func register(e *env, d vendors.Driver) {
	driversMu.Lock()
	drivers[d] = e
	driversMu.Unlock()
}

func envOf(t *testing.T, d vendors.Driver) *env {
	t.Helper()
	driversMu.Lock()
	defer driversMu.Unlock()
	e, ok := drivers[d]
	if !ok {
		t.Fatal("an unknown driver")
	}
	return e
}

// conformanceTypes returns the names of every active type of the embedded catalog
// and the ones whose quota is unknown by design: every spot type (whether
// preemptible capacity has a limit of its own is unverified, spike S9).
func conformanceTypes(t *testing.T) (all, unknown []string) {
	t.Helper()
	for _, r := range catalogRows(t) {
		all = append(all, r.ID)
		unknown = append(unknown, r.ID+"/"+vendors.CapacitySpot)
	}
	return all, unknown
}

// injected are the errors each class is injected with, as OCI answers them.
var injected = map[vendors.Class]error{
	vendors.InsufficientCapacity: fake.Error(http.StatusInternalServerError, "InternalError", "Out of host capacity."),
	vendors.Unsupported: fake.Error(http.StatusBadRequest, "InvalidParameter",
		"Shape VM.Standard.E5.Flex is not compatible with image ocid1.image.oc1..x"),
	vendors.QuotaExceeded: fake.Error(http.StatusBadRequest, "LimitExceeded",
		"The following service limits were exceeded: standard-e5-core-count"),
	vendors.ZoneUnusable: fake.Error(http.StatusBadRequest, "InvalidParameter",
		"The subnet ocid1.subnet.oc1..x has no available IP addresses"),
	vendors.Retryable: fake.Error(http.StatusInternalServerError, "InternalError", "An internal error occurred."),
	vendors.Fatal:     fake.Error(http.StatusNotFound, "NotAuthorizedOrNotFound", "Authorization failed or requested resource not found"),
}

func TestConformance(t *testing.T) {
	all, unknown := conformanceTypes(t)
	newDriver := func(t *testing.T) vendors.Driver {
		e := newEnv(t)
		// The quota consumption reads the shape tables the quotas read.
		if _, err := e.d.Quotas(t.Context(), testRegion); err != nil {
			t.Fatal(err)
		}
		d := testDriver{e.d}
		register(e, d)
		return d
	}
	conformance.Run(t, conformance.Fixture{
		New:    newDriver,
		Region: testRegion, Zone: testZone, InstanceType: testType, MaxUserData: driver.MaxUserData,
		StoresIdempotencyKey: true,
		Now:                  time.Now,
		InstanceTypes:        all, UnknownQuotaTypes: unknown,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, _ vendors.CreateRequest) {
			t.Helper()
			envOf(t, d).c.Inject("LaunchInstance", injected[class], 1)
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			if !envOf(t, d).c.SetState(ref.ID(), core.InstanceLifecycleStateStopped) {
				t.Fatalf("cannot stop %s", ref)
			}
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, _ vendors.CreateRequest) {
			t.Helper()
			c := envOf(t, d).c
			c.SetLimit(testRegion, "", "standard-e5-core-count", 0)
			c.SetLimit(testRegion, "", "standard-e5-memory-count", 0)
		},
	})
}
