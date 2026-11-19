/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	apicommon "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	gcpapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/gcp"
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
	switch kind {
	case gcp.KindSubnets:
		// The project's default network gives the default of the subnets.
		fx.SetDefault = setDefault
		// No FailCreate: the creation is the network and then the subnetwork, so an
		// interrupted one leaves the network, which List reports under its own ID and
		// the case (a leftover that the next pass completes under the same ID) does not
		// apply to; the driver's own tests cover the retry.
	case gcp.KindFirewalls, gcp.KindResourcePolicies:
		// Network tags and placement policies have no vendor default.
	default:
		return fx // the run firewalls have no selector declaration
	}
	fx.Seed = func(t *testing.T, d vendors.Driver, k vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
		t.Helper()
		return seed(t, envOf(t, d), k, region, spec)
	}
	fx.SeedAttrs = []string{"created", "description", "cidr"}
	fx.SeedCIDRs = [2]string{"10.201.0.0/24", "10.202.0.0/24"}
	fx.SeedWithin = "10.0.0.0/8"
	fx.OrderClass = orderClass
	return fx
}

// setDefault arranges the default network of the project: the fake has it, and a
// project without it has no default subnetwork. It has no IPv6 range, the fake's
// default and the real one alike.
func setDefault(t *testing.T, d vendors.Driver, _ vendors.ResourceKind, _ string, state conformance.DefaultState) {
	t.Helper()
	if state == conformance.DefaultAbsent {
		envOf(t, d).c.RemoveDefaultNetwork()
	}
}

func orderClass(
	t *testing.T, _ vendors.Driver, res map[vendors.ResourceKind]*apicommon.Resource, ipv6 bool,
) *v1alpha1.SCNodeClass {
	t.Helper()
	r := &gcpapi.Resources{Firewalls: res[gcp.KindFirewalls], ResourcePolicies: res[gcp.KindResourcePolicies]}
	if s := res[gcp.KindSubnets]; s != nil {
		r.Subnets = &gcpapi.SubnetResource{Use: s.Use, Forbid: s.Forbid}
		if s.Create != nil {
			r.Subnets.Create = &gcpapi.SubnetCreate{ResourceCreate: *s.Create}
		}
	}
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{GCP: &gcpapi.Settings{Resources: r}}},
	}
	nc.Spec.Networking.PublicIPv6 = &ipv6
	return nc
}

var seedN atomic.Int32

// seed makes a resource the way a user would: a subnetwork of the custom network
// "vpc", a firewall rule that targets the network tag of the name (the tag is the
// resource), or a placement policy. None of them has labels.
func seed(t *testing.T, e *env, kind vendors.ResourceKind, region string, spec conformance.SeedSpec) vendors.ResourceRef {
	t.Helper()
	ctx := context.Background()
	if !spec.Created.IsZero() {
		e.c.SetClock(func() time.Time { return spec.Created })
		defer e.c.SetClock(time.Now)
	}
	n := seedN.Add(1)
	ref := vendors.ResourceRef{Kind: kind, Region: region, Name: spec.Name}
	var err error
	switch kind {
	case gcp.KindSubnets:
		cidr := spec.CIDR
		if cidr == "" {
			cidr = fmt.Sprintf("10.230.%d.0/24", n)
		}
		_, err = e.c.InsertSubnetwork(ctx, &computepb.InsertSubnetworkRequest{Project: testProject, Region: region,
			SubnetworkResource: &computepb.Subnetwork{Name: &spec.Name, Network: new("vpc"), IpCidrRange: &cidr,
				Description: &spec.Description}})
		ref.ID, ref.Network = gcp.SubnetworkURL(testProject, region, spec.Name), "vpc"
	case gcp.KindFirewalls:
		_, err = e.c.InsertFirewall(ctx, &computepb.InsertFirewallRequest{Project: testProject,
			FirewallResource: &computepb.Firewall{Name: new(spec.Name + "-allow"), Network: new("vpc"),
				Direction: new("INGRESS"), TargetTags: []string{spec.Name},
				Allowed: []*computepb.Allowed{{IPProtocol: new("tcp")}}}})
		ref.ID, ref.Network = spec.Name, "vpc"
	case gcp.KindResourcePolicies:
		_, err = e.c.InsertResourcePolicy(ctx, &computepb.InsertResourcePolicyRequest{Project: testProject, Region: region,
			ResourcePolicyResource: &computepb.ResourcePolicy{Name: &spec.Name, Description: &spec.Description,
				GroupPlacementPolicy: &computepb.ResourcePolicyGroupPlacementPolicy{Collocation: new("COLLOCATED")}}})
		ref.ID = gcp.ProjectPrefix(testProject) + "regions/" + region + "/resourcePolicies/" + spec.Name
	default:
		t.Fatalf("seed: no kind %s", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return ref
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
