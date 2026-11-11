/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

// drivers maps the drivers the conformance fixture built to their environments,
// so that the hooks (which only get the driver) can reach the fake project.
var (
	driversMu sync.Mutex
	drivers   = map[vendors.Driver]*env{}
)

func register(e *env) {
	driversMu.Lock()
	drivers[e.d] = e
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

// conformanceTypes returns the API references of every GCP type of the snapshot
// and the ones whose quota is unknown by design: the series the table does not
// confirm, and Spot of every type (it depends on the region's preemptible quota).
func conformanceTypes(t *testing.T) (all, unknown []string) {
	t.Helper()
	for _, r := range gcpServers(t) {
		all = append(all, r.APIReference)
		if _, ok := confirmed[gcp.Family(r.APIReference)]; ok {
			unknown = append(unknown, r.APIReference+"/"+vendors.CapacitySpot)
		} else {
			unknown = append(unknown, r.APIReference)
		}
	}
	return all, unknown
}

// opCodes are the finished-operation codes each class is injected with.
var opCodes = map[vendors.Class]struct {
	code, msg string
	status    int32
}{
	vendors.InsufficientCapacity: {"ZONE_RESOURCE_POOL_EXHAUSTED", "The zone does not have enough resources", 503},
	vendors.Unsupported:          {"UNSUPPORTED_OPERATION", "not supported", 400},
	vendors.QuotaExceeded:        {"QUOTA_EXCEEDED", "Quota 'CPUS' exceeded.  Limit: 24.0 in region europe-west3.", 403},
	vendors.ZoneUnusable:         {"IP_SPACE_EXHAUSTED", "IP space exhausted", 400},
	vendors.Retryable:            {"INTERNAL_ERROR", "internal error", 500},
}

var seedImage atomic.Int32

func conformanceFixture(t *testing.T, kind vendors.ResourceKind) conformance.Fixture {
	all, unknown := conformanceTypes(t)
	newDriver := func(t *testing.T) vendors.Driver {
		e := newEnv(t, envOptions{})
		if _, err := e.d.LaunchableTypes(t.Context(), testRegion); err != nil {
			t.Fatal(err)
		}
		register(e)
		return e.d
	}
	fx := conformance.Fixture{
		New:    newDriver,
		Region: testRegion, Zone: testZone, InstanceType: testType, MaxUserData: gcp.MaxUserData,
		StoresIdempotencyKey: true,
		Now:                  time.Now,
		InstanceTypes:        all, UnknownQuotaTypes: unknown,
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, _ vendors.CreateRequest) {
			t.Helper()
			e := envOf(t, d)
			if class == vendors.Fatal {
				e.c.Inject("InsertInstance", fake.Error(403, "forbidden", "Required 'compute.instances.create' permission"), 1)
				return
			}
			f := opCodes[class]
			e.c.InjectOpError("InsertInstance", f.code, f.msg, f.status)
		},
		PowerOff: func(t *testing.T, d vendors.Driver, ref vendors.InstanceRef) {
			t.Helper()
			_, zone, name, ok := gcp.ParseRef(ref)
			if !ok || !envOf(t, d).c.SetStatus(zone, name, gcp.StatusStopped) {
				t.Fatalf("cannot stop %s", ref)
			}
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, _ vendors.CreateRequest) {
			t.Helper()
			envOf(t, d).c.SetLimit(testRegion, "CPUS", 0)
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":    json.RawMessage(`{"data":[{"name":"pgdata","diskType":"hyperdisk-balanced","sizeGb":100,"provisionedIops":4000,"mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"local": json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/mnt/local"}}`),
				"big":   json.RawMessage(`{"root":{"diskType":"hyperdisk-balanced","sizeGb":200,"provisionedIops":5000}}`),
			},
			vendors.ProfilePlacement: {
				"colocated": json.RawMessage(`{"resourcePolicy":{"groupPlacement":{"collocation":"COLLOCATED"}}}`),
				"spread":    json.RawMessage(`{"resourcePolicy":{"spread":{"availabilityDomainCount":2}}}`),
			},
			vendors.ProfileNetwork: {"fast": json.RawMessage(`{"nicType":"GVNIC","tier1":true}`)},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: "gcp", APIReference: "n2-standard-8", Family: "n2", VCPUs: 8},
			{Vendor: "gcp", APIReference: "n2-standard-32", Family: "n2", VCPUs: 32},
			{Vendor: "gcp", APIReference: "c4a-standard-4", Family: "c4a", VCPUs: 4, Arch: "arm64"},
			{Vendor: "gcp", APIReference: "c3-standard-8-lssd", Family: "c3", VCPUs: 8, LocalStorageGB: 750, LocalStorageCount: 2},
			{Vendor: "gcp", APIReference: "e2-small", Family: "e2", VCPUs: 2},
		},
		ResourceKind: kind,
		ResourceParams: func(t *testing.T, _ vendors.Driver, _ string) json.RawMessage {
			t.Helper()
			switch kind {
			case gcp.KindFirewalls, gcp.KindRunFirewalls:
				return json.RawMessage(`{"network":"default","selfReference":true}`)
			case gcp.KindResourcePolicies:
				return json.RawMessage(`{"collocated":true}`)
			}
			return nil
		},
		SeedImage: func(t *testing.T, d vendors.Driver, _ string, spec conformance.ImageSpec) string {
			t.Helper()
			labels := map[string]string{}
			maps.Copy(labels, spec.Tags)
			if spec.Version != "" {
				labels["flatcar-channel"], labels["flatcar-version"] = spec.Channel, strings.ReplaceAll(spec.Version, ".", "-")
			}
			arch := "X86_64"
			if spec.Arch == "arm64" {
				arch = "ARM64"
			}
			name := fmt.Sprintf("conformance-%d", seedImage.Add(1))
			envOf(t, d).c.AddImageSpec(fake.ImageSpec{Project: testProject, Name: name, Arch: arch, Labels: labels,
				Created: spec.Created})
			return "projects/" + testProject + "/global/images/" + name
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			e := restart(t, envOf(t, d).c, envOptions{})
			if _, err := e.d.LaunchableTypes(t.Context(), testRegion); err != nil {
				t.Fatal(err)
			}
			register(e)
			return e.d
		},
	}
	if kind != gcp.KindSubnets {
		fx.SeedImage = nil // the image cases run once, in the subnets run
	}
	return fx
}

// TestConformance runs the shared driver suite over the complete GCP type list of
// the committed snapshot, once per resource kind the driver creates: the suite's
// managed-resource subtests take one kind.
func TestConformance(t *testing.T) {
	for _, kind := range []vendors.ResourceKind{gcp.KindSubnets, gcp.KindFirewalls, gcp.KindRunFirewalls,
		gcp.KindResourcePolicies} {
		t.Run(string(kind), func(t *testing.T) {
			conformance.Run(t, conformanceFixture(t, kind))
		})
	}
}
