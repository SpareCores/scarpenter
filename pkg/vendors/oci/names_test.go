/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"strings"
	"testing"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
)

func TestTagKeys(t *testing.T) {
	seen := map[string]vendors.TagKey{}
	for _, k := range append(vendors.TagKeys, vendors.TagUse, vendors.TagResource) {
		got := driver.TagKey(k)
		if got != "scarpenter-"+string(k) {
			t.Errorf("TagKey(%s) = %q", k, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s map to %q", prev, k, got)
		}
		seen[got] = k
	}
	if driver.TagKey(vendors.TagUse) != "scarpenter-use" {
		t.Error("the discovery tag is not scarpenter-use")
	}
}

func TestFreeformTags(t *testing.T) {
	tags := map[vendors.TagKey]string{vendors.TagCluster: "c", vendors.TagNodeClaim: "n"}
	got, err := driver.FreeformTags(tags, map[string]string{"team": "infra"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"scarpenter-cluster": "c", "scarpenter-nodeclaim": "n", "team": "infra"}
	if len(got) != len(want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tag %s = %q, want %q", k, got[k], v)
		}
	}
	back := driver.TagsOf(got)
	if back[vendors.TagCluster] != "c" || back[vendors.TagNodeClaim] != "n" || len(back) != 2 {
		t.Errorf("TagsOf = %v, want only our own keys", back)
	}

	many := map[string]string{}
	for i := range 65 {
		many[strings.Repeat("k", i+1)] = "v"
	}
	for name, extra := range map[string]map[string]string{
		"the reserved prefix": {"scarpenter-x": "v"},
		"a long key":          {strings.Repeat("k", 101): "v"},
		"a long value":        {"k": strings.Repeat("v", 257)},
		"too many tags":       many,
		"an empty key":        {"": "v"},
	} {
		if _, err := driver.FreeformTags(tags, extra); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRefs(t *testing.T) {
	const ocid = "ocid1.instance.oc1.eu-frankfurt-1.anrw4abc123"
	ref := driver.NewRef("eu-frankfurt-1", ocid)
	if string(ref) != "eu-frankfurt-1/"+ocid {
		t.Errorf("NewRef = %q", ref)
	}
	if region, id, ok := driver.ParseRef(ref); !ok || region != "eu-frankfurt-1" || id != ocid {
		t.Errorf("ParseRef = %q, %q, %v", region, id, ok)
	}
	for _, bad := range []vendors.InstanceRef{"", "/", "eu-frankfurt-1", "eu-frankfurt-1/", "/" + ocid,
		"eu-frankfurt-1/i-123", "eu-frankfurt-1/ocid1.subnet.oc1.eu-frankfurt-1.abc", "eu-frankfurt-1/" + ocid + "/x",
		"eu-frankfurt-1/ocid1.instance.oc1.eu-frankfurt-1.", "eu-frankfurt-1/ocid1.instance..abc"} {
		if _, _, ok := driver.ParseRef(bad); ok {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
	// The OCIDs OCI really uses have an empty region part for global resources.
	if !driver.IsOCID("ocid1.compartment.oc1..aaaaexample", "compartment") ||
		driver.IsOCID("ocid1.compartment.oc1..aaaaexample", "tenancy") ||
		driver.IsOCID("not-an-ocid", "") {
		t.Error("IsOCID")
	}
}

func TestProviderID(t *testing.T) {
	id := driver.ProviderID("eu-frankfurt-1", "claim-1")
	if id != "scarpenter://oci/eu-frankfurt-1/claim-1" {
		t.Errorf("ProviderID = %q", id)
	}
	if region, claim, err := driver.ParseProviderID(id); err != nil || region != "eu-frankfurt-1" || claim != "claim-1" {
		t.Errorf("ParseProviderID = %q, %q, %v", region, claim, err)
	}
	if _, _, err := driver.ParseProviderID("scarpenter://aws/eu-west-1/x"); err == nil {
		t.Error("a provider ID of another vendor parsed")
	}
	if driver.APIReference("oci.VM.Standard.E5.Flex.2-16") != "VM.Standard.E5.Flex.2-16" ||
		driver.APIReference("VM.Standard2.1") != "VM.Standard2.1" {
		t.Error("APIReference")
	}
}

// The AD names carry a prefix that differs by tenancy; the zone name never does,
// and the zone ID is <region>-ad-<N> (plan 25, section 4 and 6).
func TestADNames(t *testing.T) {
	for _, tc := range []struct {
		region, full, name, id string
		n                      int
	}{
		{"eu-frankfurt-1", "aBcD:EU-FRANKFURT-1-AD-1", "EU-FRANKFURT-1-AD-1", "eu-frankfurt-1-ad-1", 1},
		{"eu-frankfurt-1", "aBcD:EU-FRANKFURT-1-AD-3", "EU-FRANKFURT-1-AD-3", "eu-frankfurt-1-ad-3", 3},
		{"us-phoenix-1", "zXyW:PHX-AD-1", "PHX-AD-1", "us-phoenix-1-ad-1", 1},
		{"us-ashburn-1", "US-ASHBURN-AD-2", "US-ASHBURN-AD-2", "us-ashburn-1-ad-2", 2},
	} {
		if got := driver.ADName(tc.full); got != tc.name || strings.Contains(got, ":") {
			t.Errorf("ADName(%q) = %q, want %q", tc.full, got, tc.name)
		}
		if n, ok := driver.ADNumber(tc.full); !ok || n != tc.n {
			t.Errorf("ADNumber(%q) = %d, %v", tc.full, n, ok)
		}
		if id, ok := driver.ZoneID(tc.region, tc.full); !ok || id != tc.id {
			t.Errorf("ZoneID(%q, %q) = %q, %v; want %q", tc.region, tc.full, id, ok, tc.id)
		}
	}
	if _, ok := driver.ZoneID("eu-frankfurt-1", "Xxxx:NO-NUMBER"); ok {
		t.Error("a name without a number has a zone ID")
	}
}

func TestRetryToken(t *testing.T) {
	typ := driver.InstanceType{Shape: "VM.Standard.E5.Flex", Sized: true, OCPUs: 2, GB: 16}
	base := driver.RetryToken("uid", "eu-frankfurt-1", "x:AD-1", typ, vendors.CapacityOnDemand, "epoch", 0)
	if len(base) != 43 || !strings.HasPrefix(base, "scarpenter-") {
		t.Errorf("token %q is not scarpenter- and 32 hex (43 characters)", base)
	}
	if again := driver.RetryToken("uid", "eu-frankfurt-1", "x:AD-1", typ, vendors.CapacityOnDemand, "epoch", 0); again != base {
		t.Error("the token is not deterministic")
	}
	other := typ
	other.OCPUs = 4
	otherShape := typ
	otherShape.Shape = "VM.Standard.E4.Flex"
	for name, tok := range map[string]string{
		"key":      driver.RetryToken("uid2", "eu-frankfurt-1", "x:AD-1", typ, vendors.CapacityOnDemand, "epoch", 0),
		"region":   driver.RetryToken("uid", "us-ashburn-1", "x:AD-1", typ, vendors.CapacityOnDemand, "epoch", 0),
		"AD":       driver.RetryToken("uid", "eu-frankfurt-1", "x:AD-2", typ, vendors.CapacityOnDemand, "epoch", 0),
		"ocpus":    driver.RetryToken("uid", "eu-frankfurt-1", "x:AD-1", other, vendors.CapacityOnDemand, "epoch", 0),
		"shape":    driver.RetryToken("uid", "eu-frankfurt-1", "x:AD-1", otherShape, vendors.CapacityOnDemand, "epoch", 0),
		"capacity": driver.RetryToken("uid", "eu-frankfurt-1", "x:AD-1", typ, vendors.CapacitySpot, "epoch", 0),
		"epoch":    driver.RetryToken("uid", "eu-frankfurt-1", "x:AD-1", typ, vendors.CapacityOnDemand, "epoch2", 0),
		"attempt":  driver.RetryToken("uid", "eu-frankfurt-1", "x:AD-1", typ, vendors.CapacityOnDemand, "epoch", 1),
	} {
		if tok == base {
			t.Errorf("the token does not depend on the %s", name)
		}
	}
}

func TestStateOf(t *testing.T) {
	for state, want := range map[core.InstanceLifecycleStateEnum]string{
		core.InstanceLifecycleStateProvisioning:  vendors.StatePending,
		core.InstanceLifecycleStateStarting:      vendors.StatePending,
		core.InstanceLifecycleStateRunning:       vendors.StateRunning,
		core.InstanceLifecycleStateCreatingImage: vendors.StateRunning,
		core.InstanceLifecycleStateMoving:        vendors.StateRunning,
		core.InstanceLifecycleStateStopping:      vendors.StateTerminating,
		core.InstanceLifecycleStateStopped:       vendors.StateTerminating,
		core.InstanceLifecycleStateTerminating:   vendors.StateTerminating,
		core.InstanceLifecycleStateTerminated:    vendors.StateTerminated,
		"SOMETHING_NEW":                          vendors.StateRunning,
	} {
		if got := driver.StateOf(state); got != want {
			t.Errorf("StateOf(%s) = %q, want %q", state, got, want)
		}
	}
}

// The type of an instance is rebuilt from its shape and shape configuration; a
// foreign instance with fractional sizes is never one of ours.
func TestTypeOf(t *testing.T) {
	shape := func(s string) *string { return &s }
	f := func(v float32) *float32 { return &v }
	for _, tc := range []struct {
		name string
		in   core.Instance
		want string
	}{
		{"sized", core.Instance{Shape: shape("VM.Standard.E5.Flex"), ShapeConfig: &core.InstanceShapeConfig{
			Ocpus: f(2), MemoryInGBs: f(16)}}, "VM.Standard.E5.Flex.2-16"},
		{"fixed with a config", core.Instance{Shape: shape("VM.Standard2.4"), ShapeConfig: &core.InstanceShapeConfig{
			Ocpus: f(4), MemoryInGBs: f(60)}}, "VM.Standard2.4"},
		{"no config", core.Instance{Shape: shape("BM.Standard3.64")}, "BM.Standard3.64"},
		{"fractional", core.Instance{Shape: shape("VM.Standard.E5.Flex"), ShapeConfig: &core.InstanceShapeConfig{
			Ocpus: f(1.5), MemoryInGBs: f(8)}}, "VM.Standard.E5.Flex.1.5-8"},
	} {
		if got := driver.TypeOf(tc.in); got != tc.want {
			t.Errorf("%s: TypeOf = %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, err := driver.ParseType("VM.Standard.E5.Flex.1.5-8"); err == nil {
		t.Error("a fractional foreign type parses as ours")
	}
}
