/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package offered

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/SpareCores/scarpenter/pkg/catalog"
)

// fixture builds a report like a vendor's: n types over a few regions of three
// zones and both capacity types (about 13 offerings a type for 286 types would
// be 3661), every seventh type with a gated zone and some types dropped.
func fixture(n int) catalog.Report {
	var r catalog.Report
	for i := range n {
		ref := fmt.Sprintf("VM.Standard.E5.Flex.%d-%d", i+1, (i+1)*8)
		tr := catalog.TypeReport{
			Vendor: "oci", InstanceType: "oci." + ref, APIReference: ref, APIReferenceLabel: ref,
			Arch: "amd64", VCPUs: int64(i + 1), MemoryMiB: int64(i+1) * 8192, FitsRequests: true,
		}
		for _, region := range []string{"eu-frankfurt-1", "us-ashburn-1"} {
			for _, ct := range []string{"on-demand", "spot"} {
				for z := range 3 {
					o := catalog.OfferingReport{Region: region, CapacityType: ct, Zone: fmt.Sprintf("AD-%d", z+1), Available: true}
					if (i%7 == 0 && z == 2) || i == 1 {
						o.Available = false
						o.Reason = catalog.Reason{Code: catalog.QuotaGate, Detail: "bucket standard-x12-ax-core-count, headroom 6, need 16"}
					}
					tr.Offerings = append(tr.Offerings, o)
				}
			}
		}
		r.Types = append(r.Types, tr)
	}
	for i := range n / 20 {
		r.Types = append(r.Types, catalog.TypeReport{Vendor: "oci", InstanceType: fmt.Sprintf("oci.Unpriced.%d", i),
			APIReference: fmt.Sprintf("Unpriced.%d", i), Dropped: catalog.Reason{Code: catalog.NoPrice}})
	}
	return r
}

func listingOf(t *testing.T, r catalog.Report, limit int) ([]byte, Listing) {
	t.Helper()
	raw, err := encode("bench", "oci", "2026-11-21T10:00:00Z", r.Types, limit)
	if err != nil {
		t.Fatal(err)
	}
	var l Listing
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return raw, l
}

func TestListingRoundTrip(t *testing.T) {
	r := fixture(286)
	raw, l := listingOf(t, r, MaxListingBytes)
	t.Logf("286 types: %d bytes", len(raw))
	if l.Truncated || len(raw) > MaxListingBytes {
		t.Fatalf("truncated=%v at %d bytes", l.Truncated, len(raw))
	}
	if l.Class != "bench" || l.Vendor != "oci" || l.GeneratedAt != "2026-11-21T10:00:00Z" || len(l.Types) != 286+14 {
		t.Fatalf("%q %q %q, %d types", l.Class, l.Vendor, l.GeneratedAt, len(l.Types))
	}
	// A type with a gated zone is available through the other zones.
	gated := l.Types["oci.VM.Standard.E5.Flex.1-8"]
	if !gated.Available || gated.Reason != "" || gated.APIReference != "VM.Standard.E5.Flex.1-8" ||
		strings.Join(gated.CapacityTypes, ",") != "on-demand,spot" {
		t.Errorf("gated type: %+v", gated)
	}
	want := []ListingOffering{
		{CapacityType: "on-demand", Zones: []string{"AD-1", "AD-2"}, Available: true},
		{CapacityType: "on-demand", Zones: []string{"AD-3"}, Reason: "QuotaGate"},
		{CapacityType: "spot", Zones: []string{"AD-1", "AD-2"}, Available: true},
		{CapacityType: "spot", Zones: []string{"AD-3"}, Reason: "QuotaGate"},
	}
	if !reflect.DeepEqual(gated.Offerings, want) {
		t.Errorf("offerings: %+v", gated.Offerings)
	}
	// A type with every offering gated is not available, and says why.
	blocked := l.Types["oci.VM.Standard.E5.Flex.2-16"]
	if blocked.Available {
		t.Fatalf("%+v", blocked)
	}
	dropped := l.Types["oci.Unpriced.0"]
	if dropped.Available || dropped.Reason != "NoPrice" || len(dropped.Offerings) != 0 || dropped.CapacityTypes == nil {
		t.Errorf("dropped type: %+v", dropped)
	}
	if strings.Contains(string(raw), `"capacityTypes":null`) || strings.Contains(string(raw), `"offerings":null`) {
		t.Error("an empty list is null")
	}
}

func TestListingTypeStates(t *testing.T) {
	gate := catalog.Reason{Code: catalog.QuotaGate, Detail: "bucket b, headroom 6, need 16"}
	lt := listingType(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", APIReference: "x y", Offerings: []catalog.OfferingReport{
		{CapacityType: "on-demand", Zone: "z1", Reason: gate},
		{CapacityType: "on-demand", Zone: "z2", Reason: gate},
		{CapacityType: "on-demand", Zone: "z3", Removed: true, Reason: catalog.Reason{Code: catalog.ZoneNotAvailable, Detail: "r"}},
	}})
	if lt.Available || lt.Reason != "QuotaGate" || lt.Detail != gate.Detail || lt.APIReference != "" || len(lt.CapacityTypes) != 0 {
		t.Errorf("%+v", lt)
	}
	if len(lt.Offerings) != 2 {
		t.Errorf("offerings %+v", lt.Offerings)
	}
	// More zones left out than gated: the type is still gated.
	left := catalog.Reason{Code: catalog.ZoneNotAvailable, Detail: "r"}
	lt = listingType(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", Offerings: []catalog.OfferingReport{
		{CapacityType: "on-demand", Zone: "z1", Reason: gate},
		{CapacityType: "on-demand", Zone: "z2", Removed: true, Reason: left},
		{CapacityType: "on-demand", Zone: "z3", Removed: true, Reason: left},
	}})
	if lt.Reason != "QuotaGate" || lt.Detail != gate.Detail {
		t.Errorf("gated with zones left out: %+v", lt)
	}
	// Only left-out zones: their reason.
	lt = listingType(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", Offerings: []catalog.OfferingReport{
		{CapacityType: "on-demand", Zone: "z2", Removed: true, Reason: left},
	}})
	if lt.Reason != "ZoneNotAvailable" {
		t.Errorf("only left out: %+v", lt)
	}
}

// A listing past the cap is cut, says so, and keeps the types that are not
// available.
func TestListingIsCutAtTheCap(t *testing.T) {
	r := fixture(8000)
	raw, l := listingOf(t, r, MaxListingBytes)
	t.Logf("8000 types: %d bytes, %d listed", len(raw), len(l.Types))
	if len(raw) > MaxListingBytes {
		t.Fatalf("%d bytes, over the cap", len(raw))
	}
	if !l.Truncated || len(l.Types) >= 8000 {
		t.Fatalf("truncated=%v, %d listed", l.Truncated, len(l.Types))
	}
	// The dropped types come first and are all there.
	for i := range 400 {
		if _, ok := l.Types[fmt.Sprintf("oci.Unpriced.%d", i)]; !ok {
			t.Fatalf("dropped type %d is missing", i)
		}
	}
}

func TestListingIsStable(t *testing.T) {
	r := fixture(50)
	a, _ := listingOf(t, r, MaxListingBytes)
	b, _ := listingOf(t, r, MaxListingBytes)
	if string(a) != string(b) {
		t.Error("the same report encodes differently")
	}
}

func TestConfigMapNameIsBounded(t *testing.T) {
	if got := ConfigMapName("bench", "oci"); got != "scarpenter-offered-bench-oci" {
		t.Errorf("name %q", got)
	}
	long := ConfigMapName(strings.Repeat("a", 250), "oci")
	if len(long) != maxNameLength || long == ConfigMapName(strings.Repeat("a", 249)+"b", "oci") {
		t.Errorf("long name %q", long)
	}
}
