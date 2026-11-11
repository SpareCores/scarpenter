/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/gcp/fake"
)

// The fake implements exactly the interface.
var _ ComputeAPI = (*fake.Compute)(nil)

func TestProviderID(t *testing.T) {
	id := ProviderID("europe-west3", "claim-1")
	if id != "scarpenter://gcp/europe-west3/claim-1" {
		t.Fatalf("id = %s", id)
	}
	region, claim, err := ParseProviderID(id)
	if err != nil || region != "europe-west3" || claim != "claim-1" {
		t.Fatalf("parse = %q %q %v", region, claim, err)
	}
	for _, bad := range []string{"", "aws://x", "scarpenter://aws/eu-west-1/claim", "scarpenter://gcp/europe-west3", "scarpenter://gcp//claim", "scarpenter://gcp/a/b/c"} {
		if _, _, err := ParseProviderID(bad); err == nil {
			t.Errorf("ParseProviderID(%q) must fail", bad)
		}
	}
}

func TestInstanceRef(t *testing.T) {
	ref := NewRef("europe-west3", "europe-west3-a", "claim-1")
	if ref != "europe-west3/europe-west3-a/claim-1" || ref.Region() != "europe-west3" || ref.ID() != "europe-west3-a/claim-1" {
		t.Fatalf("ref = %s (%s, %s)", ref, ref.Region(), ref.ID())
	}
	r, z, n, ok := ParseRef(ref)
	if !ok || r != "europe-west3" || z != "europe-west3-a" || n != "claim-1" {
		t.Fatalf("parse = %q %q %q %v", r, z, n, ok)
	}
	for _, bad := range []vendors.InstanceRef{
		"europe-west3/i-never-existed",            // the conformance suite's malformed ID
		"europe-west3/europe-west3-a/",            // no name
		"europe-west3/not-a-zone/claim",           // bad zone
		"europe-west3/europe-west4-a/claim",       // zone of another region
		"europe-west3/europe-west3-a/Bad_Name",    // not RFC 1035
		"europe-west3/europe-west3-a/claim/extra", // more parts
		"", // nothing
		vendors.NewInstanceRef("europe-west3", "a"),    // zone-less
		"us-central1/europe-west3-a/claim",             // region mismatch
		"europe-west3/europe-west3-a/-leading-dash",    // starts with a dash
		"europe-west3/europe-west3-a/trailing-dash-",   // ends with a dash
		vendors.NewInstanceRef("europe-west3", "/x"),   // empty zone
		vendors.NewInstanceRef("", "europe-west3-a/x"), // empty region
	} {
		if _, _, _, ok := ParseRef(bad); ok {
			t.Errorf("ParseRef(%q) must fail", bad)
		}
	}
}

func TestRegionOfZone(t *testing.T) {
	for zone, want := range map[string]string{
		"europe-west3-a": "europe-west3", "us-central1-f": "us-central1",
		"northamerica-northeast1-b": "northamerica-northeast1", "me-central2-c": "me-central2",
	} {
		if got := RegionOfZone(zone); got != want {
			t.Errorf("RegionOfZone(%s) = %s, want %s", zone, got, want)
		}
		if !zoneRe.MatchString(zone) {
			t.Errorf("zoneRe rejects %s", zone)
		}
	}
}

func TestInstanceName(t *testing.T) {
	if got := InstanceName("default-abc12"); got != "default-abc12" {
		t.Fatalf("valid name changed: %s", got)
	}
	long := strings.Repeat("a", 63)
	if got := InstanceName(long); got != long {
		t.Fatalf("63 characters are valid: %s", got)
	}
	seen := map[string]string{}
	for _, bad := range []string{"Upper", "has.dot", strings.Repeat("a", 64), "1starts-with-digit", "ends-with-", "", "under_score"} {
		got := InstanceName(bad)
		if !instanceNameRe.MatchString(got) || len(got) > 63 || !strings.HasPrefix(got, "scarpenter-") {
			t.Errorf("InstanceName(%q) = %q is not a valid generated name", bad, got)
		}
		if got != InstanceName(bad) {
			t.Errorf("InstanceName(%q) is not deterministic", bad)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("InstanceName collides for %q and %q", bad, prev)
		}
		seen[got] = bad
	}
}

func TestCatalogNames(t *testing.T) {
	if APIReference("gcp.e2-small") != "e2-small" || APIReference("e2-small") != "e2-small" {
		t.Fatal("APIReference")
	}
	for mt, want := range map[string]string{"n2-standard-8": "n2", "e2-small": "e2", "c4a-highcpu-4": "c4a", "t2a-standard-1": "t2a"} {
		if got := Family(mt); got != want {
			t.Errorf("Family(%s) = %s", mt, got)
		}
	}
	if MachineTypeURL("z-a", "e2-small") != "zones/z-a/machineTypes/e2-small" || DiskTypeURL("z-a", "pd-balanced") != "zones/z-a/diskTypes/pd-balanced" {
		t.Fatal("URLs")
	}
	tests := []struct{ in, want string }{
		{"nodes", "projects/p/regions/r/subnetworks/nodes"},
		{"projects/host/regions/r/subnetworks/shared", "projects/host/regions/r/subnetworks/shared"},
		{"https://www.googleapis.com/compute/v1/projects/host/regions/r/subnetworks/shared", "projects/host/regions/r/subnetworks/shared"},
		{"regions/r/subnetworks/nodes", "projects/p/regions/r/subnetworks/nodes"},
	}
	for _, tc := range tests {
		if got := SubnetworkURL("p", "r", tc.in); got != tc.want {
			t.Errorf("SubnetworkURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	imgs := []struct{ in, want string }{
		{"flatcar-stable-4757-2-3", "projects/p/global/images/flatcar-stable-4757-2-3"},
		{"projects/kinvolk-public/global/images/flatcar-stable-4757-2-3", "projects/kinvolk-public/global/images/flatcar-stable-4757-2-3"},
		{"https://www.googleapis.com/compute/v1/projects/k/global/images/x", "projects/k/global/images/x"},
	}
	for _, tc := range imgs {
		if got := ImageURL("p", tc.in); got != tc.want {
			t.Errorf("ImageURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLabelValue(t *testing.T) {
	// Valid values are unchanged, including a UID and the empty string.
	for _, v := range []string{"", "default", "my_pool-1", "0f8fad5b-d9cb-469f-a165-70867728950e", strings.Repeat("a", 63)} {
		if got := LabelValue(v); got != v {
			t.Errorf("LabelValue(%q) = %q, want unchanged", v, got)
		}
	}
	// Invalid values map to valid, deterministic, distinct values.
	inputs := []string{"Default", "default.", "my pool", "a/b", "A", "a", strings.Repeat("x", 64), strings.Repeat("x", 100), "ünï", "uni", "ÜNÏ"}
	seen := map[string]string{}
	for _, v := range inputs {
		got := LabelValue(v)
		if !labelValueRe.MatchString(got) {
			t.Errorf("LabelValue(%q) = %q is not a valid label value", v, got)
		}
		if got != LabelValue(v) {
			t.Errorf("LabelValue(%q) is not deterministic", v)
		}
		if prev, dup := seen[got]; dup && prev != v {
			t.Errorf("LabelValue collides for %q and %q: %q", v, prev, got)
		}
		seen[got] = v
	}
	// A valid value and an invalid one with the same normal form stay apart.
	if LabelValue("default") == LabelValue("Default") {
		t.Error("Default must not collapse onto default")
	}
	if len(LabelValue(strings.Repeat("x", 200))) > 63 {
		t.Error("long values must be shortened to 63 characters")
	}
}

func TestLabels(t *testing.T) {
	tags := map[vendors.TagKey]string{
		vendors.TagCluster: "Cluster.One", vendors.TagNodeClaim: "claim-1", vendors.TagIdempotencyKey: "0f8fad5b-d9cb-469f-a165-70867728950e",
	}
	labels, err := Labels(tags, map[string]string{"team": "data", "env": "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if labels["scarpenter-nodeclaim"] != "claim-1" || labels["scarpenter-idempotency-key"] != "0f8fad5b-d9cb-469f-a165-70867728950e" ||
		labels["scarpenter-cluster"] != LabelValue("Cluster.One") || labels["team"] != "data" || len(labels) != 5 {
		t.Fatalf("labels = %v", labels)
	}
	// Round trip through the instance.
	back := TagsOf(labels)
	if back[vendors.TagNodeClaim] != "claim-1" || back[vendors.TagCluster] != LabelValue("Cluster.One") {
		t.Fatalf("TagsOf = %v", back)
	}
	// The filter uses the same mapping as the write.
	f := LabelFilter(map[vendors.TagKey]string{vendors.TagCluster: "Cluster.One", vendors.TagNodeClaim: "claim-1"})
	want := `(labels.scarpenter-cluster = "` + LabelValue("Cluster.One") + `") AND (labels.scarpenter-nodeclaim = "claim-1")`
	if f != want {
		t.Fatalf("filter = %s", f)
	}
	if LabelFilter(nil) != "" {
		t.Fatal("an empty tag set is an empty filter")
	}
	// Bad user tags and too many labels.
	for _, bad := range []map[string]string{
		{"Team": "x"}, {"1team": "x"}, {"scarpenter-x": "y"}, {"team": "UPPER"}, {strings.Repeat("k", 41): "v"}, {"team": strings.Repeat("v", 41)},
	} {
		if _, err := Labels(nil, bad); err == nil {
			t.Errorf("Labels with %v must fail", bad)
		}
	}
	many := map[string]string{}
	for i := range 65 {
		many["k"+strings.Repeat("a", i%10)+string(rune('a'+i/10))+string(rune('a'+i%26))] = "v"
	}
	if len(many) > maxLabels {
		if _, err := Labels(nil, many); err == nil {
			t.Error("more than 64 labels must fail")
		}
	}
}

func TestMarker(t *testing.T) {
	desc := MarkerDescription("c1", "default", "subnets")
	if desc != "scarpenter: cluster=c1 nodeclass=default managed-by=scarpenter resource-kind=subnets" {
		t.Fatalf("description = %s", desc)
	}
	c, n, k, ok := ParseMarker(desc)
	if !ok || c != "c1" || n != "default" || k != "subnets" {
		t.Fatalf("parse = %q %q %q %v", c, n, k, ok)
	}
	for _, bad := range []string{"", "hello", "scarpenter: cluster=c1", "scarpenter: cluster=c1 managed-by=other resource-kind=x", "scarpenter: managed-by=scarpenter resource-kind=x"} {
		if _, _, _, ok := ParseMarker(bad); ok {
			t.Errorf("ParseMarker(%q) must fail", bad)
		}
	}
	if ResourceNamePrefix("c1") != "scarpenter-c1-" {
		t.Fatal("prefix")
	}
}

func TestRequestID(t *testing.T) {
	a := RequestID("uid-1", "europe-west3-a", "e2-small", "spot", "ep", 0)
	if a != RequestID("uid-1", "europe-west3-a", "e2-small", "spot", "ep", 0) {
		t.Fatal("not deterministic")
	}
	for _, other := range []string{
		RequestID("uid-2", "europe-west3-a", "e2-small", "spot", "ep", 0),
		RequestID("uid-1", "europe-west3-b", "e2-small", "spot", "ep", 0),
		RequestID("uid-1", "europe-west3-a", "n2-standard-2", "spot", "ep", 0),
		RequestID("uid-1", "europe-west3-a", "e2-small", "on-demand", "ep", 0),
		// A new attempt and a new epoch are new requests.
		RequestID("uid-1", "europe-west3-a", "e2-small", "spot", "ep", 1),
		RequestID("uid-1", "europe-west3-a", "e2-small", "spot", "ep2", 0),
	} {
		if other == a {
			t.Fatal("a different offering must have a different request ID")
		}
	}
	// A UUID of version 5, variant RFC 4122.
	if len(a) != 36 || a[14] != '5' || !strings.ContainsRune("89ab", rune(a[19])) {
		t.Fatalf("not a UUIDv5: %s", a)
	}
	// Known vector: uuid5(NAMESPACE_DNS, "python.org") = 886313e1-3b8a-5372-9b90-0c9aee199e5d.
	dns := [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	if got := uuidV5(dns, "python.org"); got != "886313e1-3b8a-5372-9b90-0c9aee199e5d" {
		t.Fatalf("uuidV5 = %s", got)
	}
}

func TestStateOf(t *testing.T) {
	tests := map[string]string{
		StatusProvisioning: vendors.StatePending, StatusStaging: vendors.StatePending,
		StatusRunning: vendors.StateRunning, StatusRepairing: vendors.StateRunning,
		StatusStopping: vendors.StateTerminating, StatusStopped: vendors.StateTerminating,
		StatusTerminated: vendors.StateTerminating, StatusSuspending: vendors.StateTerminating,
		StatusSuspended: vendors.StateTerminating,
		"SOMETHING_NEW": vendors.StateRunning, "": vendors.StateRunning,
	}
	for status, want := range tests {
		if got := StateOf(status); got != want {
			t.Errorf("StateOf(%q) = %s, want %s", status, got, want)
		}
	}
}

func TestToInstance(t *testing.T) {
	c := fake.NewSeeded("p")
	spec := baseSpec()
	spec.Project = "p"
	spec.Spot, spec.CapacityType = true, vendors.CapacitySpot
	req, err := BuildInsert(spec)
	if err != nil {
		t.Fatal(err)
	}
	op, err := c.InsertInstance(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitZoneOperation(t.Context(), &computepb.WaitZoneOperationRequest{Project: "p", Zone: spec.Zone, Operation: op.GetName()}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetInstance(t.Context(), &computepb.GetInstanceRequest{Project: "p", Zone: spec.Zone, Instance: spec.Name})
	if err != nil {
		t.Fatal(err)
	}
	inst := ToInstance("europe-west3", got)
	if inst.Ref != NewRef("europe-west3", spec.Zone, spec.Name) || inst.Zone != spec.Zone || inst.Type != "e2-small" ||
		inst.CapacityType != vendors.CapacitySpot || inst.State != vendors.StateRunning || inst.CreatedAt.IsZero() {
		t.Fatalf("instance = %+v", inst)
	}
	if inst.Tags[vendors.TagNodeClaim] != "claim-1" || inst.Tags[vendors.TagIdempotencyKey] != spec.IdempotencyKey {
		t.Fatalf("tags = %v", inst.Tags)
	}
	if len(inst.Addresses) != 2 { // private and NAT
		t.Fatalf("addresses = %v", inst.Addresses)
	}
	if _, _, _, ok := ParseRef(inst.Ref); !ok {
		t.Fatal("the reference of a real instance must parse")
	}
	// Stopped out of band: terminating, still listed.
	c.SetStatus(spec.Zone, spec.Name, "TERMINATED")
	got, _ = c.GetInstance(t.Context(), &computepb.GetInstanceRequest{Project: "p", Zone: spec.Zone, Instance: spec.Name})
	if s := ToInstance("europe-west3", got).State; s != vendors.StateTerminating {
		t.Fatalf("stopped state = %s", s)
	}
	if CapacityTypeOf(&computepb.Instance{}) != vendors.CapacityOnDemand {
		t.Fatal("no scheduling is on-demand")
	}
}
