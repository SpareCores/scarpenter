/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

func (e *env) create(t *testing.T, req vendors.CreateRequest) *vendors.Instance {
	t.Helper()
	inst, err := e.d.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatalf("CreateInstance(%s): %v", req, err)
	}
	return inst
}

// What a launch puts on the wire, as the fake received it.
func TestCreateInstanceRequest(t *testing.T) {
	e := newEnv(t)
	req := request(testType)
	req.ExtraTags = map[string]string{"team": "infra"}
	req.Settings.Raw = json.RawMessage(`{"storage":{"root":{"sizeGb":100,"performance":"higher-performance"}},` +
		`"metadata":{"owner":"me"}}`)
	req.Image.RootDiskBytes = 50 << 30
	inst := e.create(t, req)

	launches := e.c.Launches()
	if len(launches) != 1 {
		t.Fatalf("%d launches", len(launches))
	}
	l := launches[0]
	if *l.AvailabilityDomain != fake.ADFull(testZone) || *l.Shape != "VM.Standard.E5.Flex" ||
		*l.ShapeConfig.Ocpus != 2 || *l.ShapeConfig.MemoryInGBs != 16 {
		t.Errorf("availability domain %q, shape %q, config %+v", *l.AvailabilityDomain, *l.Shape, l.ShapeConfig)
	}
	if *l.DisplayName != "claim-1" || *l.CompartmentId != fake.SeedCompartment {
		t.Errorf("display name %q, compartment %q", *l.DisplayName, *l.CompartmentId)
	}
	src := l.SourceDetails.(core.InstanceSourceViaImageDetails)
	if *src.BootVolumeSizeInGBs != 100 || *src.BootVolumeVpusPerGB != 20 || *src.ImageId != fake.SeedImage {
		t.Errorf("source %+v", src)
	}
	if want := []string{"ocid1.networksecuritygroup.oc1.eu-frankfurt-1.aaaaone",
		"ocid1.networksecuritygroup.oc1.eu-frankfurt-1.aaaarun"}; !slices.Equal(l.CreateVnicDetails.NsgIds, want) {
		t.Errorf("nsgIds %v, want %v", l.CreateVnicDetails.NsgIds, want)
	}
	if !*l.CreateVnicDetails.AssignPublicIp || *l.CreateVnicDetails.SubnetId != fake.SeedSubnet {
		t.Errorf("vnic %+v", l.CreateVnicDetails)
	}
	for k, v := range map[string]string{
		"scarpenter-cluster": testCluster, "scarpenter-nodeclaim": "claim-1", "scarpenter-nodepool": "pool",
		"scarpenter-nodeclass": "class", "scarpenter-managed-by": "scarpenter", "scarpenter-idempotency-key": "uid-1",
		"team": "infra",
	} {
		if l.FreeformTags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, l.FreeformTags[k], v)
		}
	}
	if l.Metadata["owner"] != "me" || l.Metadata["user_data"] == "" || len(l.Metadata) != 2 {
		t.Errorf("metadata keys %v", l.Metadata)
	}
	if want := fmt.Sprintf("%s/%s", testRegion, strings.TrimPrefix(string(inst.Ref), testRegion+"/")); string(inst.Ref) != want {
		t.Errorf("ref %q", inst.Ref)
	}
	if inst.State != vendors.StatePending {
		t.Errorf("a launch returns a PROVISIONING instance; the driver says %q", inst.State)
	}
	if len(*l.OpcRetryToken) != 43 {
		t.Errorf("retry token %q", *l.OpcRetryToken)
	}
	if got := e.c.BootVolumes(); len(got) != 1 {
		t.Errorf("boot volumes %v", got)
	}
}

func TestCreateInstanceSpotAndArm(t *testing.T) {
	e := newEnv(t)
	req := request("VM.Standard.A1.Flex.4-8")
	req.CapacityType, req.Arch = vendors.CapacitySpot, "arm64"
	inst := e.create(t, req)
	l := e.c.Launches()[0]
	if l.PreemptibleInstanceConfig == nil || l.LaunchOptions == nil {
		t.Errorf("preemptible %+v, launch options %+v", l.PreemptibleInstanceConfig, l.LaunchOptions)
	}
	if inst.CapacityType != vendors.CapacitySpot || inst.Type != "VM.Standard.A1.Flex.4-8" {
		t.Errorf("instance %+v", inst)
	}
	got, err := e.d.GetInstance(t.Context(), inst.Ref)
	if err != nil || got.CapacityType != vendors.CapacitySpot {
		t.Errorf("GetInstance: %+v, %v", got, err)
	}
}

// The checks that need no cloud call: a type that cannot launch fails against the
// shape table, and the launch call is never made.
func TestCreateInstanceRejectsBeforeTheCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		typ    string
		edit   func(*vendors.CreateRequest)
		class  vendors.Class
		code   string
		errHas string
	}{
		{"extended memory", "VM.Standard.E5.Flex.126-1008", nil, vendors.Unsupported, driver.CodeExtendedMemory, "NUMA"},
		{"an unparsable name", "VM.Standard.E5.Flex", nil, vendors.Unsupported, driver.CodeInvalidInstanceType, "size"},
		{"a foreign prefix", "e5.2-16", nil, vendors.Fatal, driver.CodeInvalidInstanceType, ""},
		{"memory per OCPU", "VM.Standard.E5.Flex.2-4", nil, vendors.Unsupported, driver.CodeInvalidInstanceType, "per OCPU"},
		{"memory beyond the shape", "VM.Standard.E5.Flex.2-2000", nil, vendors.Unsupported, driver.CodeInvalidInstanceType, ""},
		{"a size beyond the OCPUs", "VM.Standard.E5.Flex.500-2000", nil, vendors.Unsupported, driver.CodeInvalidInstanceType, ""},
		{"an unknown shape", "VM.Standard.Z9.Flex.2-16", nil, vendors.Unsupported, driver.CodeShapeNotInAD, "not offered"},
		{"a size on a fixed shape", "VM.Standard2.4.4-8", nil, vendors.Unsupported, driver.CodeInvalidInstanceType,
			"no sizes"},
		{"an unknown zone", testType, func(r *vendors.CreateRequest) { r.Zone = "EU-FRANKFURT-1-AD-9" }, vendors.Unsupported,
			"UnknownZone", "no availability domain"},
		{"a zone with the prefix", testType, func(r *vendors.CreateRequest) { r.Zone = fake.ADFull(testZone) },
			vendors.Unsupported, "UnknownZone", ""},
		{"an unsubscribed region", testType, func(r *vendors.CreateRequest) { r.Region = fake.SeedRegionUnsubscribed },
			vendors.Unsupported, "UnknownRegion", "not subscribed"},
		{"no subnet", testType, func(r *vendors.CreateRequest) { r.Prepared = nil }, vendors.Unsupported,
			vendors.CodeResourceMissing, "subnet"},
		{"a bad block", testType, func(r *vendors.CreateRequest) { r.Settings.Raw = json.RawMessage(`{"storage":{"data":[]}}`) },
			vendors.Fatal, "InvalidSettings", "unknown field"},
		{"a small root", testType, func(r *vendors.CreateRequest) { r.Image.RootDiskBytes = 80 << 30 }, vendors.Fatal,
			"InvalidSettings", "smaller than the image"},
		{"a reserved tag", testType, func(r *vendors.CreateRequest) { r.ExtraTags = map[string]string{"scarpenter-x": "y"} },
			vendors.Fatal, "InvalidSettings", "reserved"},
		{"six groups", testType, func(r *vendors.CreateRequest) {
			for i := range 4 {
				r.Prepared.Resources = append(r.Prepared.Resources,
					vendors.ResourceRef{Kind: driver.KindNetworkSecurityGroups, ID: fmt.Sprint("nsg-", i)})
			}
		}, vendors.Fatal, "InvalidSettings", "network security groups"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			req := request(tc.typ)
			if tc.edit != nil {
				tc.edit(&req)
			}
			_, err := e.d.CreateInstance(t.Context(), req)
			if !vendors.IsClass(err, tc.class) || vendors.CodeOf(err) != tc.code ||
				(tc.errHas != "" && !strings.Contains(err.Error(), tc.errHas)) {
				t.Fatalf("CreateInstance = %v (class %s, code %s); want %s %s ~ %q", err, vendors.ClassOf(err),
					vendors.CodeOf(err), tc.class, tc.code, tc.errHas)
			}
			if n := e.c.Calls("LaunchInstance"); n != 0 {
				t.Errorf("%d launch calls for a type that cannot launch", n)
			}
		})
	}
}

// A size beyond one NUMA node of its shape is refused with the shape's own limit,
// is not launchable by LaunchableType once the shape table is read.
func TestSizeBeyondNumaNode(t *testing.T) {
	c := fake.NewSeeded()
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.E3.Flex", 114, 1776, 1, 64, 64, 1024,
		"standard-e3-core-ad-count", "standard-e3-memory-count"))
	e := restart(t, c, testCluster)
	const over, within = "VM.Standard.E3.Flex.114-456", "VM.Standard.E3.Flex.64-256"
	if _, refused := launchable(e.d, over); refused {
		t.Fatal("refused before any shape table was read")
	}
	if _, err := e.d.Quotas(t.Context(), testRegion); err != nil {
		t.Fatal(err)
	}
	if why, refused := launchable(e.d, over); !refused || !strings.Contains(why, "NUMA") {
		t.Errorf("LaunchableType(%s) = %q, %v; want refused", over, why, refused)
	}
	if why, refused := launchable(e.d, within); refused {
		t.Errorf("LaunchableType(%s) = %q; want offered", within, why)
	}
	// Memory alone beyond the node (within the OCPUs) is refused too.
	if _, refused := launchable(e.d, "VM.Standard.E3.Flex.64-1100"); !refused {
		t.Error("a size with memory beyond the node is offered")
	}
	_, err := e.d.CreateInstance(t.Context(), request(over))
	if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != driver.CodeExtendedMemory {
		t.Errorf("CreateInstance = %v; want Unsupported %s", err, driver.CodeExtendedMemory)
	}
}

// launchable is LaunchableType of an API reference, inverted to read as "refused".
func launchable(d *driver.Driver, apiReference string) (string, bool) {
	ok, why := d.LaunchableType(vendors.CatalogType{Vendor: driver.Name, APIReference: apiReference})
	return why, !ok
}

// Fixed shapes and Flex shapes without NUMA fields are never refused.
func TestRefusedIgnoresShapesWithoutNumaLimits(t *testing.T) {
	c := fake.NewSeeded()
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.E3.Flex", 114, 1776, 1, 64, 0, 0,
		"standard-e3-core-ad-count", "standard-e3-memory-count"))
	c.AddShape(fake.SeedRegion, fake.FixedShape("VM.Standard2.4", 4, 60, "standard2-core-count"))
	e := restart(t, c, testCluster)
	if _, err := e.d.Quotas(t.Context(), testRegion); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"VM.Standard.E3.Flex.114-456", "VM.Standard2.4"} {
		if why, refused := launchable(e.d, typ); refused {
			t.Errorf("LaunchableType(%s) = %q; want offered", typ, why)
		}
	}
}

// The epoch moves when a shape is first read or its NUMA limits change, not on an
// identical re-read.
func TestShapeEpoch(t *testing.T) {
	c := fake.NewSeeded()
	numa := func(ocpus float32) {
		c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.E3.Flex", 114, 1776, 1, 64, ocpus, 1024,
			"standard-e3-core-ad-count", "standard-e3-memory-count"))
	}
	numa(64)
	e := restart(t, c, testCluster)
	reread := func() uint64 {
		e.clock.Step(2 * time.Hour) // past the shape tables' TTL
		if _, err := e.d.Quotas(t.Context(), testRegion); err != nil {
			t.Fatal(err)
		}
		return e.d.NodeOptionsEpoch()
	}
	first := reread()
	if first == 0 {
		t.Fatal("the epoch did not move on the first read")
	}
	if got := reread(); got != first {
		t.Errorf("an identical re-read moved the epoch: %d, want %d", got, first)
	}
	numa(32)
	if got := reread(); got == first {
		t.Error("a NUMA change did not move the epoch")
	}
}

// A shape the user's AD does not offer is Unsupported without a launch call, while
// another AD of the region still launches it (plan 25, section 6).
func TestShapeOnlyInSomeADs(t *testing.T) {
	c := fake.NewSeeded()
	c.AddShape(fake.SeedRegion, fake.FlexShape("VM.Standard.E5.Flex", 94, 1049, 1, 64, 94, 1049,
		"standard-e5-core-count", "standard-e5-memory-count"), fake.ADFull(fake.SeedZoneB))
	e := restart(t, c, testCluster)
	_, err := e.d.CreateInstance(t.Context(), request(testType))
	if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != driver.CodeShapeNotInAD {
		t.Fatalf("launch in an AD without the shape: %v", err)
	}
	if !vendors.IsPreLaunch(err) {
		t.Error("the driver's own shape check is not marked PreLaunch")
	}
	if e.c.Calls("LaunchInstance") != 0 {
		t.Error("a launch call was made")
	}
	req := request(testType)
	req.Zone = fake.SeedZoneB
	if inst := e.create(t, req); inst.Zone != fake.SeedZoneB {
		t.Errorf("zone %q", inst.Zone)
	}
}

// A replay in another AD, shape or capacity type returns the original; a failed
// create leaves the key unused (the contract; plan 25, section 5).
func TestIdempotencyAcrossOfferings(t *testing.T) {
	e := newEnv(t)
	first := e.create(t, request(testType))
	for name, edit := range map[string]func(*vendors.CreateRequest){
		"zone":     func(r *vendors.CreateRequest) { r.Zone = fake.SeedZoneB },
		"shape":    func(r *vendors.CreateRequest) { r.InstanceType = "VM.Standard.E4.Flex.2-16" },
		"capacity": func(r *vendors.CreateRequest) { r.CapacityType = vendors.CapacitySpot },
	} {
		req := request(testType)
		edit(&req)
		got, err := e.d.CreateInstance(t.Context(), req)
		if err != nil || got.Ref != first.Ref || got.Zone != testZone || got.Type != testType {
			t.Errorf("a replay with another %s: %+v, %v; want the original %s", name, got, err, first.Ref)
		}
	}
	if n := e.c.Calls("LaunchInstance"); n != 1 {
		t.Errorf("%d launch calls for one NodeClaim", n)
	}
}

// A response lost on the way back leaves an instance; the replay finds it by its
// tag and launches nothing.
func TestLostLaunchResponse(t *testing.T) {
	e := newEnv(t)
	e.c.LoseLaunchResponses(1)
	_, err := e.d.CreateInstance(t.Context(), request(testType))
	if !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("a lost response: %v, want Retryable", err)
	}
	inst, err := e.d.CreateInstance(t.Context(), request(testType))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := e.d.ListInstances(t.Context(), []string{testRegion}, nil)
	if len(list) != 1 || list[0].Ref != inst.Ref || e.c.Calls("LaunchInstance") != 1 {
		t.Errorf("%d instances, %d launch calls after the replay", len(list), e.c.Calls("LaunchInstance"))
	}
}

// The token of a replay after a lost response is the same one; after a definitive
// failure it is a new one (plan 25, section 5).
func TestRetryTokenAcrossAttempts(t *testing.T) {
	e := newEnv(t)
	token := func() string {
		l := e.c.Launches()
		return *l[len(l)-1].OpcRetryToken
	}
	e.c.Inject("LaunchInstance", fake.Error(500, "InternalError", "Out of host capacity."), 1)
	if _, err := e.d.CreateInstance(t.Context(), request(testType)); !vendors.IsClass(err, vendors.InsufficientCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	failed := token()
	e.c.Inject("LaunchInstance", fake.Error(503, "ServiceUnavailable", "later"), 1)
	if _, err := e.d.CreateInstance(t.Context(), request(testType)); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("503: %v", err)
	}
	afterFailure := token()
	e.c.Inject("LaunchInstance", fake.Error(429, "TooManyRequests", "slow"), 1)
	_, _ = e.d.CreateInstance(t.Context(), request(testType))
	afterTransient := token()
	if failed == afterFailure {
		t.Error("the token of a launch that failed definitively was reused")
	}
	if afterFailure != afterTransient {
		t.Error("the token changed after a transient failure")
	}
	e.create(t, request(testType))
	if token() != afterTransient {
		t.Error("the token changed on the retry that succeeded")
	}
}

// racing launches the instance in the fake and then answers 409, as the service does
// when another request with the same token won between the driver's lookup and its launch.
type racing struct{ *fake.Cloud }

func (r racing) LaunchInstance(ctx context.Context, region string, req core.LaunchInstanceRequest,
) (core.LaunchInstanceResponse, error) {
	if _, err := r.Cloud.LaunchInstance(ctx, region, req); err != nil {
		return core.LaunchInstanceResponse{}, err
	}
	return core.LaunchInstanceResponse{}, fake.Error(http.StatusConflict, "Conflict", "the retry token is in use")
}

// A token the service says another request used is resolved by the lookup of the key:
// the instance of the key is the answer, and without one the create is Fatal.
func TestRetryTokenConflict(t *testing.T) {
	t.Run("the instance of the key is returned", func(t *testing.T) {
		c := newCloud(t)
		e := restart(t, c, testCluster, driver.WithClients(driver.Clients{
			Compute: racing{c}, Network: c, Identity: c, Limits: c}))
		inst, err := e.d.CreateInstance(t.Context(), request(testType))
		if err != nil {
			t.Fatal(err)
		}
		list, _ := e.d.ListInstances(t.Context(), []string{testRegion}, nil)
		if len(list) != 1 || list[0].Ref != inst.Ref {
			t.Errorf("the conflict resolved to %s; the region has %v", inst.Ref, list)
		}
	})
	t.Run("no instance of the key is Fatal", func(t *testing.T) {
		e := newEnv(t)
		e.c.Inject("LaunchInstance", fake.Error(http.StatusConflict, "Conflict", "token reused"), 1)
		_, err := e.d.CreateInstance(t.Context(), request(testType))
		if !vendors.IsClass(err, vendors.Fatal) || vendors.CodeOf(err) != "RetryTokenConflict" {
			t.Fatalf("a conflict with no instance of the key: %v, want Fatal RetryTokenConflict", err)
		}
	})
	t.Run("the still-in-use conflicts are not token conflicts", func(t *testing.T) {
		e := newEnv(t)
		e.c.Inject("LaunchInstance", fake.Error(http.StatusConflict, "IncorrectState", "busy"), 1)
		if _, err := e.d.CreateInstance(t.Context(), request(testType)); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("IncorrectState: %v, want Retryable", err)
		}
	})
}

func TestListFindGet(t *testing.T) {
	e := newEnv(t)
	e.c.SetPageSize(2)
	refs := make([]vendors.InstanceRef, 0, 5)
	for i := range 5 {
		req := request(testType)
		req.IdempotencyKey = fmt.Sprint("uid-", i)
		req.Tags[vendors.TagNodeClaim], req.Node.Name = fmt.Sprint("claim-", i), fmt.Sprint("claim-", i)
		refs = append(refs, e.create(t, req).Ref)
	}
	list, err := e.d.ListInstances(t.Context(), []string{testRegion, fake.SeedRegionB, fake.SeedRegionUnsubscribed, "nowhere"},
		map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(list) != 5 {
		t.Fatalf("ListInstances over pages of 2: %d instances, %v", len(list), err)
	}
	if e.c.Calls("ListInstances") < 3 {
		t.Errorf("%d list calls for five instances in pages of two", e.c.Calls("ListInstances"))
	}
	found, err := e.d.FindInstance(t.Context(), testRegion, map[vendors.TagKey]string{vendors.TagNodeClaim: "claim-3"})
	if err != nil || found.Ref != refs[3] || found.Tags[vendors.TagNodeClaim] != "claim-3" {
		t.Errorf("FindInstance: %+v, %v", found, err)
	}
	// The display name narrows the call; it must not hide a tag mismatch.
	_, err = e.d.FindInstance(t.Context(), testRegion, map[vendors.TagKey]string{
		vendors.TagNodeClaim: "claim-3", vendors.TagCluster: "another"})
	if !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("FindInstance with a foreign cluster: %v", err)
	}
	if _, err := e.d.FindInstance(t.Context(), fake.SeedRegionUnsubscribed, nil); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("FindInstance in an unsubscribed region: %v", err)
	}
	if err := e.d.DeleteInstance(t.Context(), refs[0]); err != nil {
		t.Fatal(err)
	}
	if list, _ = e.d.ListInstances(t.Context(), []string{testRegion}, nil); len(list) != 4 {
		t.Errorf("%d instances after a delete; a terminated one is not listed", len(list))
	}
}

func TestGetInstanceAddresses(t *testing.T) {
	e := newEnv(t)
	req := request(testType)
	req.Settings.Networking.PublicIPv6 = new(true)
	inst := e.create(t, req)
	if len(inst.Addresses) != 0 {
		t.Errorf("the create returns addresses %v; they come with GetInstance", inst.Addresses)
	}
	if !inst.PublicUnknown {
		t.Error("the create does not say that its public addresses are unknown")
	}
	got, err := e.d.GetInstance(t.Context(), inst.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.PublicUnknown {
		t.Error("GetInstance reads the addresses, so the public ones are known")
	}
	if len(got.Addresses) != 3 || !strings.HasPrefix(got.Addresses[0], "10.240.") ||
		!strings.HasPrefix(got.Addresses[1], "203.0.113.") || !strings.HasPrefix(got.Addresses[2], "2001:db8:") {
		t.Errorf("addresses %v; want the private, public and IPv6 address", got.Addresses)
	}
	if !strings.HasPrefix(got.PublicIPv4, "203.0.113.") || !strings.HasPrefix(got.PublicIPv6, "2001:db8:") {
		t.Errorf("public addresses %q %q", got.PublicIPv4, got.PublicIPv6)
	}
	if got.State != vendors.StateRunning || got.Zone != testZone || got.Region != testRegion || got.BillingStart.IsZero() {
		t.Errorf("instance %+v", got)
	}
	// Without a public address.
	req = request(testType)
	req.IdempotencyKey, req.Tags[vendors.TagNodeClaim], req.Node.Name = "uid-2", "claim-2", "claim-2"
	req.Settings.Networking.PublicIPv4 = new(false)
	inst = e.create(t, req)
	got, _ = e.d.GetInstance(t.Context(), inst.Ref)
	if len(got.Addresses) != 1 || !strings.HasPrefix(got.Addresses[0], "10.240.") {
		t.Errorf("addresses %v without publicIPv4", got.Addresses)
	}
	if got.PublicIPv4 != "" || got.PublicIPv6 != "" {
		t.Errorf("public addresses %q %q without a public address", got.PublicIPv4, got.PublicIPv6)
	}
	// A VNIC deleted between the listing and the read: the instance still exists.
	e.c.Inject("GetVnic", fake.Error(404, "NotAuthorizedOrNotFound", "gone"), 1)
	if got, err = e.d.GetInstance(t.Context(), inst.Ref); err != nil || len(got.Addresses) != 0 {
		t.Errorf("a vanished VNIC: %v, %v; want the instance without addresses", got, err)
	}
	e.c.Inject("GetVnic", fake.Error(500, "InternalError", "x"), 1)
	if _, err := e.d.GetInstance(t.Context(), inst.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("a failing VNIC read: %v, want Retryable", err)
	}
}

// A terminated instance is returned by Get (OCI keeps it for about twelve hours) and
// not listed; a stopped one is terminating and listed.
func TestInstanceStates(t *testing.T) {
	e := newEnv(t)
	inst := e.create(t, request(testType))
	e.c.SetState(inst.Ref.ID(), core.InstanceLifecycleStateStopped)
	got, err := e.d.GetInstance(t.Context(), inst.Ref)
	if err != nil || got.State != vendors.StateTerminating || !got.Gone() {
		t.Fatalf("a stopped instance: %+v, %v", got, err)
	}
	if list, _ := e.d.ListInstances(t.Context(), []string{testRegion}, nil); len(list) != 1 {
		t.Errorf("a stopped instance is listed %d times", len(list))
	}
	if err := e.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	got, err = e.d.GetInstance(t.Context(), inst.Ref)
	if err != nil || got.State != vendors.StateTerminated || len(got.Addresses) != 0 {
		t.Errorf("a terminated instance: %+v, %v", got, err)
	}
	if list, _ := e.d.ListInstances(t.Context(), []string{testRegion}, nil); len(list) != 0 {
		t.Errorf("a terminated instance is listed: %v", list)
	}
}

func TestDeleteInstance(t *testing.T) {
	e := newEnv(t)
	inst := e.create(t, request(testType))
	if len(e.c.BootVolumes()) != 1 {
		t.Fatal("no boot volume")
	}
	// An instance of another cluster is refused, never deleted: the compartment and
	// this check are the only walls.
	other := restart(t, e.c, "another-cluster")
	if err := other.d.DeleteInstance(t.Context(), inst.Ref); !vendors.IsClass(err, vendors.Fatal) ||
		vendors.CodeOf(err) != "NotOwned" {
		t.Errorf("deleting another cluster's instance: %v, want Fatal NotOwned", err)
	}
	if e.c.Calls("TerminateInstance") != 0 {
		t.Error("a foreign instance was terminated")
	}
	if err := e.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	if len(e.c.BootVolumes()) != 0 {
		t.Error("the boot volume survived the instance")
	}
	// Idempotent: the second delete makes no terminate call.
	if err := e.d.DeleteInstance(t.Context(), inst.Ref); err != nil || e.c.Calls("TerminateInstance") != 1 {
		t.Errorf("second delete: %v, %d terminate calls", err, e.c.Calls("TerminateInstance"))
	}
	// An instance that is gone, in a region the tenancy has, or a malformed reference.
	gone := driver.NewRef(testRegion, "ocid1.instance.oc1.eu-frankfurt-1.aaaanotthere")
	if err := e.d.DeleteInstance(t.Context(), gone); err != nil {
		t.Errorf("deleting an unknown instance: %v", err)
	}
	if _, err := e.d.GetInstance(t.Context(), gone); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("GetInstance of an unknown instance: %v", err)
	}
	// A failing terminate is reported with its class.
	next := request(testType)
	next.IdempotencyKey = "uid-next"
	live := e.create(t, next)
	e.c.Inject("TerminateInstance", fake.Error(409, "IncorrectState", "busy"), 1)
	if err := e.d.DeleteInstance(t.Context(), live.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("a busy instance: %v, want Retryable", err)
	}
	e.c.Inject("TerminateInstance", fake.Error(404, "NotAuthorizedOrNotFound", "gone"), 1)
	if err := e.d.DeleteInstance(t.Context(), live.Ref); err != nil {
		t.Errorf("a 404 on terminate: %v, want success", err)
	}
	// Without a cluster name nothing is deleted.
	anon := restart(t, e.c, "")
	if err := anon.d.DeleteInstance(t.Context(), live.Ref); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("deleting with no cluster name: %v", err)
	}
}

func TestUserDataLimits(t *testing.T) {
	e := newEnv(t)
	req := request(testType)
	req.UserData = []byte(strings.Repeat("x", driver.MaxUserData+1))
	if _, err := e.d.CreateInstance(t.Context(), req); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("oversized user data: %v", err)
	}
	// 16 KiB of user data is 21.8 KB of base64: with 11 KB of block metadata the total
	// is over OCI's 32,000 bytes, which the driver sees before the call.
	req = request(testType)
	req.UserData = []byte(strings.Repeat("x", driver.MaxUserData))
	req.Settings.Raw = json.RawMessage(`{"metadata":{"big":"` + strings.Repeat("y", 11000) + `"}}`)
	_, err := e.d.CreateInstance(t.Context(), req)
	if !vendors.IsClass(err, vendors.Fatal) || !strings.Contains(err.Error(), "32000") {
		t.Errorf("metadata over the limit: %v", err)
	}
	if e.c.Calls("LaunchInstance") != 0 {
		t.Error("a launch call was made")
	}
}

// Capacity, limits and the rest reach the driver as their classes.
func TestLaunchErrorsFromTheFake(t *testing.T) {
	t.Run("a stockout is capacity, once", func(t *testing.T) {
		e := newEnv(t)
		e.c.SetStockout(testRegion, fake.ADFull(testZone), "VM.Standard.E5.Flex", true)
		if _, err := e.d.CreateInstance(t.Context(), request(testType)); !vendors.IsClass(err, vendors.InsufficientCapacity) {
			t.Fatalf("a stockout: %v", err)
		}
		if n := e.c.Calls("LaunchInstance"); n != 1 {
			t.Errorf("%d launch calls: the capacity error was retried", n)
		}
		req := request(testType)
		req.Zone = fake.SeedZoneB
		e.create(t, req)
	})
	t.Run("a limit is quota", func(t *testing.T) {
		e := newEnv(t)
		e.c.SetLimit(testRegion, fake.ADFull(testZone), "standard-e5-core-count", 1)
		err := func() error { _, err := e.d.CreateInstance(t.Context(), request(testType)); return err }()
		if !vendors.IsClass(err, vendors.QuotaExceeded) || !strings.Contains(err.Error(), "standard-e5-core-count") {
			t.Errorf("a limit: %v", err)
		}
	})
	t.Run("a full subnet is the zone, not a token conflict", func(t *testing.T) {
		e := newEnv(t)
		e.c.Inject("LaunchInstance", fake.Error(409, "Conflict", "subnet ocid1.subnet.oc1..x: not enough free IP addresses"), 1)
		if _, err := e.d.CreateInstance(t.Context(), request(testType)); !vendors.IsClass(err, vendors.ZoneUnusable) {
			t.Errorf("a full subnet: %v", err)
		}
	})
	t.Run("a timeout is retryable", func(t *testing.T) {
		e := newEnv(t)
		e.c.Inject("LaunchInstance", fmt.Errorf("deadline: %w", context.DeadlineExceeded), 1)
		if _, err := e.d.CreateInstance(t.Context(), request(testType)); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("a timeout: %v", err)
		}
	})
	t.Run("a listing that fails stops the create", func(t *testing.T) {
		e := newEnv(t)
		e.c.Inject("ListInstances", fake.Error(500, "InternalError", "x"), 1)
		if _, err := e.d.CreateInstance(t.Context(), request(testType)); !vendors.IsClass(err, vendors.Retryable) {
			t.Errorf("a failing lookup: %v", err)
		}
		if e.c.Calls("LaunchInstance") != 0 {
			t.Error("a launch call was made without the lookup of the idempotency key")
		}
	})
}

// A shape that is not in the image's compatibility list is refused before the
// launch call (OCI answers it with a bare 400 InvalidParameter).
func TestCreateInstanceRefusesAShapeTheImageDoesNotRun(t *testing.T) {
	e := newEnv(t)
	e.c.ReplaceImageShapes(fake.SeedImage, "VM.Standard.A1.Flex")
	before := e.c.Calls("LaunchInstance")
	_, err := e.d.CreateInstance(t.Context(), request(testType))
	if !vendors.IsClass(err, vendors.InvalidOffering) || vendors.CodeOf(err) != "ImageNotCompatible" {
		t.Fatalf("CreateInstance = %v, want InvalidOffering ImageNotCompatible", err)
	}
	if e.c.Calls("LaunchInstance") != before {
		t.Error("a launch call was made for a shape the image does not run")
	}
}

// A 400 InvalidParameter without a known reason is the offering's: InvalidOffering.
func TestCreateInstanceUnknownInvalidParameterIsInvalidOffering(t *testing.T) {
	e := newEnv(t)
	e.c.Inject("LaunchInstance", fake.Error(400, "InvalidParameter", "Invalid shapeConfig"), 1)
	if _, err := e.d.CreateInstance(t.Context(), request(testType)); !vendors.IsClass(err, vendors.InvalidOffering) {
		t.Fatalf("CreateInstance = %v, want InvalidOffering", err)
	}
}
