/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	apicommon "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	ociapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/oci"
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

var seedCounter atomic.Int32

// fixture is the conformance fixture of one resource kind. The selector, image and resolution-order
// cases run once, in the run of the subnets kind (they cover the other kinds themselves).
func fixture(t *testing.T, kind vendors.ResourceKind) conformance.Fixture {
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
	fx := conformance.Fixture{
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
		// OCI has no profile blocks in v1: nothing is supported, and the types are checked for that.
		ProfileTypes: []vendors.CatalogType{
			{Vendor: "oci", APIReference: testType, Family: "Standard", VCPUs: 4},
			{Vendor: "oci", APIReference: "VM.Standard.A1.Flex.4-8", Family: "Standard", VCPUs: 4, Arch: "arm64"},
			{Vendor: "oci", APIReference: "BM.Standard3.64", Family: "Standard", VCPUs: 128},
		},
		ResourceKind: kind,
		ResourceParams: func(t *testing.T, d vendors.Driver, _ string) json.RawMessage {
			t.Helper()
			if kind == driver.KindSubnets {
				return nil
			}
			raw, err := json.Marshal(driver.GroupParams{VCN: envOf(t, d).c.SeededVCN(), SelfReference: kind == driver.KindRunNetworkSecurityGroup})
			if err != nil {
				t.Fatal(err)
			}
			return raw
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			e := restart(t, envOf(t, d).c, testCluster)
			if _, err := e.d.Quotas(t.Context(), testRegion); err != nil {
				t.Fatal(err)
			}
			nd := testDriver{e.d}
			register(e, nd)
			return nd
		},
		PreCreate: func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
			t.Helper()
			return seed(t, envOf(t, d), k, region, conformance.SeedSpec{Name: fmt.Sprintf("user-%d", seedCounter.Add(1)), Tags: tags})
		},
	}
	if kind != driver.KindSubnets {
		return fx
	}
	fx.Seed = func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
		t.Helper()
		return seed(t, envOf(t, d), k, region, spec)
	}
	fx.SeedKinds = []vendors.ResourceKind{driver.KindSubnets, driver.KindNetworkSecurityGroups}
	fx.SeedAttrs = []string{"created", "zone", "cidr"}
	fx.SeedCIDRs, fx.SeedWithin = [2]string{"10.0.16.0/20", "10.0.32.0/20"}, "10.0.0.0/16"
	fx.SeedImage = func(t *testing.T, d vendors.Driver, region string, spec conformance.ImageSpec) string {
		t.Helper()
		img := core.Image{DisplayName: new(fmt.Sprintf("conformance-image-%d", seedCounter.Add(1))), OperatingSystem: new("Flatcar"),
			FreeformTags: map[string]string{"arch": spec.Arch}, SizeInMBs: new(int64(47719))}
		maps.Copy(img.FreeformTags, spec.Tags)
		if spec.Version != "" {
			img.OperatingSystemVersion = new(spec.Version)
			img.FreeformTags["flatcar-channel"], img.FreeformTags["flatcar-version"] = spec.Channel, spec.Version
		}
		if !spec.Created.IsZero() {
			img.TimeCreated = &common.SDKTime{Time: spec.Created}
		}
		return envOf(t, d).c.AddImage(region, img, "VM.Standard.E5.Flex")
	}
	fx.OrderClass = func(t *testing.T, _ vendors.Driver, res map[vendors.ResourceKind]*apicommon.Resource, ipv6 bool) *v1alpha1.SCNodeClass {
		t.Helper()
		r := &ociapi.Resources{NetworkSecurityGroups: res[driver.KindNetworkSecurityGroups]}
		if s := res[driver.KindSubnets]; s != nil {
			r.Subnets = &ociapi.SubnetResource{Use: s.Use, Forbid: s.Forbid}
			if s.Create != nil {
				r.Subnets.Create = &ociapi.SubnetCreate{ResourceCreate: *s.Create}
			}
		}
		nc := &v1alpha1.SCNodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "class"},
			Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{OCI: &ociapi.Settings{Resources: r}}},
		}
		nc.Spec.Networking.PublicIPv6 = &ipv6
		return nc
	}
	// OCI has no default network: SetDefault stays nil, and the cases about the default are skipped.
	fx.FailCreate = func(t *testing.T, d vendors.Driver, _ vendors.ResourceKind) {
		t.Helper()
		// The VCN exists when the internet gateway fails.
		envOf(t, d).c.Inject("CreateInternetGateway", fake.Error(http.StatusInternalServerError, "InternalError", "try again"), 1)
	}
	return fx
}

// seed makes a resource the way a user would, in the seeded VCN.
func seed(t *testing.T, e *env, kind vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
	t.Helper()
	c := e.c
	var id string
	switch kind {
	case driver.KindSubnets:
		ad := ""
		if spec.Zone != "" {
			ad = fake.ADFull(spec.Zone)
		}
		cidr := spec.CIDR
		if cidr == "" {
			cidr = fmt.Sprintf("10.0.%d.0/24", 100+seedCounter.Add(1)%100)
		}
		id = c.SeedSubnet(region, c.SeededVCN(), spec.Name, cidr, ad, spec.Tags, false, false)
	default:
		id = c.SeedNSG(region, c.SeededVCN(), spec.Name, spec.Tags)
	}
	if !spec.Created.IsZero() {
		c.SetCreated(id, spec.Created)
	}
	return vendors.ResourceRef{Kind: kind, Region: region, ID: id, Name: spec.Name}
}

// TestConformance runs the shared driver suite over the complete OCI type list of the embedded
// catalog, once per resource kind the driver creates: the suite's managed-resource subtests take one
// kind.
func TestConformance(t *testing.T) {
	for _, kind := range []vendors.ResourceKind{driver.KindSubnets, driver.KindNetworkSecurityGroups,
		driver.KindRunNetworkSecurityGroup} {
		t.Run(string(kind), func(t *testing.T) { conformance.Run(t, fixture(t, kind)) })
	}
}
