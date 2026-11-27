/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package offered

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/quota"
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
			FitsRequests: true,
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

// listingOf encodes the report and decodes the result as a reader does: the
// compressed bytes, the JSON and the listing.
func listingOf(t *testing.T, r catalog.Report, limit int) (gz, raw []byte, l Listing) {
	t.Helper()
	gz, _, err := encode(newListing("bench", "oci", "2026-11-21T10:00:00Z", r.Types, nil), limit)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	if raw, err = io.ReadAll(zr); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return gz, raw, l
}

func TestListingRoundTrip(t *testing.T) {
	r := fixture(286)
	gz, raw, l := listingOf(t, r, MaxListingBytes)
	t.Logf("286 types: %d bytes, %d compressed", len(raw), len(gz))
	if l.Truncated || l.ZonesOmitted || len(gz) > MaxListingBytes {
		t.Fatalf("truncated=%v at %d bytes", l.Truncated, len(gz))
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
	lt := typeWithoutQuotas(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", APIReference: "x y", Offerings: []catalog.OfferingReport{
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
	lt = typeWithoutQuotas(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", Offerings: []catalog.OfferingReport{
		{CapacityType: "on-demand", Zone: "z1", Reason: gate},
		{CapacityType: "on-demand", Zone: "z2", Removed: true, Reason: left},
		{CapacityType: "on-demand", Zone: "z3", Removed: true, Reason: left},
	}})
	if lt.Reason != "QuotaGate" || lt.Detail != gate.Detail {
		t.Errorf("gated with zones left out: %+v", lt)
	}
	// Only left-out zones: their reason.
	lt = typeWithoutQuotas(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", Offerings: []catalog.OfferingReport{
		{CapacityType: "on-demand", Zone: "z2", Removed: true, Reason: left},
	}})
	if lt.Reason != "ZoneNotAvailable" {
		t.Errorf("only left out: %+v", lt)
	}
}

// The 8000-type report fits compressed, with every available type.
func TestListingFitsCompressed(t *testing.T) {
	r := fixture(8000)
	gz, raw, l := listingOf(t, r, MaxListingBytes)
	t.Logf("8000 types: %d bytes, %d compressed", len(raw), len(gz))
	if len(gz) > MaxListingBytes || len(raw) <= MaxListingBytes {
		t.Fatalf("%d compressed, %d raw", len(gz), len(raw))
	}
	if l.Truncated || l.ZonesOmitted || len(l.Types) != 8000+400 {
		t.Fatalf("truncated=%v zonesOmitted=%v, %d listed", l.Truncated, l.ZonesOmitted, len(l.Types))
	}
	for name, lt := range l.Types {
		if lt.Available && len(lt.Offerings[0].Zones) == 0 {
			t.Fatalf("%s has no zones", name)
		}
	}
}

// Over the cap the zones go, every type stays, with its state and expiry.
func TestListingWithoutZonesWhenOverTheCap(t *testing.T) {
	r := fixture(300)
	r.Types[0].Offerings[0] = catalog.OfferingReport{CapacityType: "spot", Zone: "AD-1",
		Reason: catalog.Reason{Code: catalog.UnavailableCache, Class: "InsufficientCapacity", Until: time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)}}
	// Zone names that do not repeat, so that they weigh on the compressed size.
	for i := range r.Types {
		for j := range r.Types[i].Offerings {
			o := &r.Types[i].Offerings[j]
			o.Zone = fmt.Sprintf("%s-%d-%d", o.Zone, i, j)
		}
	}
	full, _, _ := listingOf(t, r, MaxListingBytes)
	gz, _, l := listingOf(t, r, len(full)-1)
	if !l.ZonesOmitted || l.Truncated || len(l.Types) != 300+15 || len(gz) >= len(full) {
		t.Fatalf("zonesOmitted=%v truncated=%v, %d types, %d bytes", l.ZonesOmitted, l.Truncated, len(l.Types), len(gz))
	}
	for name, lt := range l.Types {
		for _, o := range lt.Offerings {
			if len(o.Zones) != 0 {
				t.Fatalf("%s keeps zones %v", name, o.Zones)
			}
		}
	}
	var until string
	for _, o := range l.Types["oci.VM.Standard.E5.Flex.1-8"].Offerings {
		until = cmp.Or(until, o.Until)
	}
	if until != "2026-11-21T11:00:00Z" {
		t.Errorf("until %q", until)
	}
	if strings.Contains(string(mustJSON(t, l)), `"zones":null`) {
		t.Error("a null zone list")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A listing that does not fit even without zones is an error, not a cut.
func TestListingOverTheCapIsAnError(t *testing.T) {
	_, _, err := encode(newListing("bench", "oci", "", fixture(50).Types, nil), 100)
	if err == nil || !strings.Contains(err.Error(), "bench") || !strings.Contains(err.Error(), "oci") ||
		!strings.Contains(err.Error(), "compressed") {
		t.Fatalf("error %v", err)
	}
}

func TestListingIsStable(t *testing.T) {
	r := fixture(50)
	a, _, _ := listingOf(t, r, MaxListingBytes)
	b, _, _ := listingOf(t, r, MaxListingBytes)
	if !bytes.Equal(a, b) {
		t.Error("the same report encodes differently")
	}
}

// Canary: the AWS catalog over every region of the fixtures compresses to a
// fraction of the cap, so that the bench scale (~200 regions) still fits.
func TestListingOfAllAWSRegionsFitsWithRoomToSpare(t *testing.T) {
	facts := catalogtest.Facts(t, "aws")
	regions := slices.Sorted(maps.Keys(facts.ZonesByVendor["aws"]))
	cat := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: facts})
	cat.SetClasses([]catalog.Class{catalog.NewClass("canary", catalog.Scope{"aws": regions})})
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	report, err := cat.Explain(t.Context(), catalog.Query{Class: "canary"})
	if err != nil {
		t.Fatal(err)
	}
	types := slices.DeleteFunc(report.Types, func(tr catalog.TypeReport) bool { return tr.Vendor != "aws" })
	l := newListing("canary", "aws", "2026-11-21T10:00:00Z", types, nil)
	gz, zonesOmitted, err := encode(l, MaxListingBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d regions, %d types: %d bytes compressed", len(regions), len(types), len(gz))
	if zonesOmitted || len(gz) > MaxListingBytes/4 {
		t.Errorf("zonesOmitted=%v, %d bytes compressed, want at most %d", zonesOmitted, len(gz), MaxListingBytes/4)
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

func TestListingCarriesTheClassAndTheExpiryOfCacheMarks(t *testing.T) {
	soon := time.Date(2026, 11, 21, 10, 5, 0, 0, time.UTC)
	late := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	mark := func(class string, until time.Time) catalog.Reason {
		return catalog.Reason{Code: catalog.UnavailableCache, Detail: class + " until " + until.Format(time.RFC3339),
			Class: class, Until: until}
	}
	lt := typeWithoutQuotas(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", Offerings: []catalog.OfferingReport{
		{CapacityType: "on-demand", Zone: "z1", Reason: mark("QuotaExceeded", soon)},
		{CapacityType: "on-demand", Zone: "z2", Reason: mark("QuotaExceeded", soon)},
		{CapacityType: "on-demand", Zone: "z3", Reason: mark("DailyLimit", late)},
		{CapacityType: "on-demand", Zone: "z4", Reason: mark("QuotaExceeded", late)},
		{CapacityType: "on-demand", Zone: "z5", Available: true},
	}})
	want := []ListingOffering{
		{CapacityType: "on-demand", Zones: []string{"z5"}, Available: true},
		{CapacityType: "on-demand", Zones: []string{"z3"}, Reason: "DailyLimit", Until: "2026-11-22T00:00:00Z"},
		{CapacityType: "on-demand", Zones: []string{"z1", "z2"}, Reason: "QuotaExceeded", Until: "2026-11-21T10:05:00Z"},
		{CapacityType: "on-demand", Zones: []string{"z4"}, Reason: "QuotaExceeded", Until: "2026-11-22T00:00:00Z"},
	}
	if !reflect.DeepEqual(lt.Offerings, want) {
		t.Errorf("offerings %+v, want %+v", lt.Offerings, want)
	}
	raw, err := json.Marshal(lt.Offerings[0])
	if err != nil || strings.Contains(string(raw), "until") {
		t.Errorf("an available offering carries until: %s", raw)
	}
}

// A type whose offerings are all marked is not available and says when the
// first mark expires; an available type has no until.
func TestListingTypeUntilIsTheEarliestMarkOfAnUnavailableType(t *testing.T) {
	soon := time.Date(2026, 11, 21, 10, 5, 0, 0, time.UTC)
	late := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	mark := func(until time.Time) catalog.Reason {
		return catalog.Reason{Code: catalog.UnavailableCache, Class: "InsufficientCapacity", Until: until}
	}
	lt := typeWithoutQuotas(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", Offerings: []catalog.OfferingReport{
		{CapacityType: "on-demand", Zone: "z1", Reason: mark(late)},
		{CapacityType: "spot", Zone: "z1", Reason: mark(soon)},
	}})
	if lt.Available || lt.Until != "2026-11-21T10:05:00Z" {
		t.Errorf("available = %v, until = %q, want the earliest mark", lt.Available, lt.Until)
	}
	lt = typeWithoutQuotas(catalog.TypeReport{Vendor: "oci", InstanceType: "oci.x", Offerings: []catalog.OfferingReport{
		{CapacityType: "on-demand", Zone: "z1", Reason: mark(late)},
		{CapacityType: "spot", Zone: "z1", Available: true},
	}})
	if !lt.Available || lt.Until != "" {
		t.Errorf("available type: %+v", lt)
	}
}

func typeWithoutQuotas(t catalog.TypeReport) ListingType { return listingType(t, nil) }

// fakeQuotas answers per region; a region or type it does not know has no quota.
type fakeQuotas map[string][]quota.NeedFact

func (f fakeQuotas) Needs(_, region, apiReference, capacityType string) []quota.NeedFact {
	return f[region+"/"+apiReference+"/"+capacityType]
}

func TestListingQuotas(t *testing.T) {
	gate := catalog.Reason{Code: catalog.QuotaGate}
	q := fakeQuotas{
		"r1/big/on-demand": {
			{Bucket: "L-B", Unit: "GB", Need: 8, Limit: 64, Headroom: 0},
			{Bucket: "L-A", Unit: "vCPU", Need: 4, Limit: 384, Headroom: 4},
		},
		"r2/big/on-demand": {
			{Bucket: "L-A", Unit: "vCPU", Need: 4, Limit: 100, Headroom: 100},
			{Bucket: "L-B", Unit: "GB", Need: 8, Limit: 128, Headroom: 0},
		},
		"r1/gated/spot": {{Bucket: "L-A", Unit: "vCPU", Need: 512, Limit: 384, Headroom: 0}},
	}
	tr := func(ref string, offerings ...catalog.OfferingReport) catalog.TypeReport {
		return catalog.TypeReport{Vendor: "stub", InstanceType: "stub." + ref, APIReference: ref, Offerings: offerings}
	}
	l := newListing("c", "stub", "", []catalog.TypeReport{
		// Two regions: the largest limit and headroom of each bucket, buckets sorted.
		tr("big",
			catalog.OfferingReport{Region: "r1", Zone: "z1", CapacityType: "on-demand", Available: true},
			catalog.OfferingReport{Region: "r2", Zone: "z2", CapacityType: "on-demand", Available: true}),
		// An unavailable group still carries its quotas.
		tr("gated", catalog.OfferingReport{Region: "r1", Zone: "z1", CapacityType: "spot", Reason: gate}),
		// No bucket applies: the field is absent, never an empty list.
		tr("none", catalog.OfferingReport{Region: "r1", Zone: "z1", CapacityType: "on-demand", Available: true}),
	}, q)
	wantBig := []ListingQuota{
		{Bucket: "L-A", Unit: "vCPU", Need: 4, MaxLimit: 384, MaxHeadroom: 100, Regions: []ListingQuotaRegion{
			{Region: "r1", Limit: 384, Headroom: 4}, {Region: "r2", Limit: 100, Headroom: 100}}},
		{Bucket: "L-B", Unit: "GB", Need: 8, MaxLimit: 128, MaxHeadroom: 0, Regions: []ListingQuotaRegion{
			{Region: "r1", Limit: 64, Headroom: 0}, {Region: "r2", Limit: 128, Headroom: 0}}},
	}
	if got := l.Types["stub.big"].Offerings[0].Quotas; !reflect.DeepEqual(got, wantBig) {
		t.Errorf("big: %+v", got)
	}
	gated := l.Types["stub.gated"]
	if o := gated.Offerings[0]; o.Available || len(o.Quotas) != 1 || o.Quotas[0].Need != 512 || o.Quotas[0].MaxLimit != 384 {
		t.Errorf("gated: %+v", gated.Offerings)
	}
	raw, err := json.Marshal(l.Types["stub.none"])
	if err != nil || strings.Contains(string(raw), "quotas") {
		t.Errorf("none: %s", raw)
	}
	raw, _ = json.Marshal(l.Types["stub.big"].Offerings[0])
	if !strings.Contains(string(raw), `"quotas":[{"bucket":"L-A","unit":"vCPU","need":4,"maxLimit":384,"maxHeadroom":100,"regions":[{"region":"r1","limit":384,"headroom":4},`) {
		t.Errorf("layout: %s", raw)
	}
	if without := newListing("c", "stub", "", nil, nil); without.Types == nil {
		t.Error("no types gives a nil map")
	}
}

func TestListingQuotaRegions(t *testing.T) {
	q := fakeQuotas{
		// r3 is stale or unknown: it has no facts and stays out.
		"r2/t/on-demand": {{Bucket: "reg", Unit: "vCPU", Need: 4, Limit: 20, Headroom: 12}, {Bucket: "all", Unit: "vCPU", Need: 4, Limit: 50, Headroom: 30, Global: true}},
		"r1/t/on-demand": {{Bucket: "reg", Unit: "vCPU", Need: 4, Limit: 10, Headroom: 2}, {Bucket: "all", Unit: "vCPU", Need: 4, Limit: 50, Headroom: 30, Global: true}},
	}
	tr := catalog.TypeReport{Vendor: "stub", InstanceType: "stub.t", APIReference: "t", Offerings: []catalog.OfferingReport{
		{Region: "r3", Zone: "z3", CapacityType: "on-demand", Available: true},
		{Region: "r2", Zone: "z2", CapacityType: "on-demand", Available: true},
		{Region: "r1", Zone: "z1", CapacityType: "on-demand", Available: true},
	}}
	got := listingType(tr, q).Offerings[0].Quotas
	want := []ListingQuota{
		// A global bucket has no regions: the maximums are the account figures.
		{Bucket: "all", Unit: "vCPU", Need: 4, MaxLimit: 50, MaxHeadroom: 30},
		{Bucket: "reg", Unit: "vCPU", Need: 4, MaxLimit: 20, MaxHeadroom: 12, Regions: []ListingQuotaRegion{
			{Region: "r1", Limit: 10, Headroom: 2}, {Region: "r2", Limit: 20, Headroom: 12}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("quotas: %+v", got)
	}
	raw, _ := json.Marshal(got[0])
	if strings.Contains(string(raw), "regions") {
		t.Errorf("global bucket: %s", raw)
	}
}

func TestListingDigestIgnoresRegionHeadroomButNotLimit(t *testing.T) {
	digest := func(limit, headroom int64) string {
		q := fakeQuotas{"r1/t/on-demand": {{Bucket: "b", Unit: "u", Need: 1, Limit: limit, Headroom: headroom}}}
		l := newListing("c", "stub", "", []catalog.TypeReport{{Vendor: "stub", InstanceType: "stub.t", APIReference: "t",
			Offerings: []catalog.OfferingReport{{Region: "r1", Zone: "z1", CapacityType: "on-demand", Available: true}}}}, q)
		raw, err := json.Marshal(l.withoutHeadroom())
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if digest(10, 5) != digest(10, 1) {
		t.Error("a headroom change alters the digest")
	}
	if digest(10, 5) == digest(20, 5) {
		t.Error("a limit change leaves the digest unchanged")
	}
}
