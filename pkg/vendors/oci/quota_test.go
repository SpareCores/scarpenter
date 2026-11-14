/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"testing"

	"github.com/oracle/oci-go-sdk/v65/limits"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

func bucketNamed(snap *vendors.QuotaSnapshot, id string) (vendors.Bucket, bool) {
	for _, b := range snap.Buckets {
		if b.ID == id {
			return b, true
		}
	}
	return vendors.Bucket{}, false
}

// The buckets are the region's: the usage of the whole region (every AD), and the
// headroom of the AD with the most room, so that the tracker blocks a type only when no
// AD has room for it (plan 25, section 7; open question 6).
func TestQuotaBuckets(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	e.c.SetLimit(testRegion, fake.ADFull(fake.SeedZoneA), "standard-e5-core-count", 10)
	e.c.SetLimit(testRegion, fake.ADFull(fake.SeedZoneB), "standard-e5-core-count", 20)
	e.c.SetLimit(testRegion, fake.ADFull(fake.SeedZoneC), "standard-e5-core-count", 5)
	e.create(t, request(testType)) // 2 OCPUs, 16 GB in the first AD

	snap, err := e.d.Quotas(ctx, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	core, ok := bucketNamed(snap, "standard-e5-core-count")
	if !ok {
		t.Fatalf("no core bucket in %+v", snap.Buckets)
	}
	// Used 2 in the region; the best AD (the second) has 20 left.
	if core.Used != 2 || core.Limit != 22 || core.Unit != driver.UnitOCPU || core.Scope != vendors.QuotaScopeRegion {
		t.Errorf("core bucket %+v, want used 2, limit 22 (2 + 20), OCPU, region", core)
	}
	if mem, ok := bucketNamed(snap, "standard-e5-memory-count"); !ok || mem.Used != 16 || mem.Unit != driver.UnitGB {
		t.Errorf("memory bucket %+v", mem)
	}
	if bm, ok := bucketNamed(snap, "bm-standard3-64-count"); !ok || bm.Unit != driver.UnitInstances || bm.Used != 0 {
		t.Errorf("bare metal bucket %+v", bm)
	}
	for _, b := range snap.Buckets {
		if b.Limit < b.Used {
			t.Errorf("bucket %+v: the limit is below the usage", b)
		}
	}
	if len(snap.Counted) != 1 {
		t.Errorf("counted %v", snap.Counted)
	}
	if g, err := e.d.GlobalQuotas(ctx); err != nil || len(g.Buckets) != 0 {
		t.Errorf("GlobalQuotas = %+v, %v; want none", g, err)
	}
}

// A region-scoped limit is read once, without an availability domain.
func TestQuotaRegionScope(t *testing.T) {
	e := newEnv(t)
	e.c.SetScope("standard-e5-core-count", limits.LimitDefinitionSummaryScopeTypeRegion)
	e.c.SetLimit(testRegion, "", "standard-e5-core-count", 40)
	e.create(t, request(testType))
	before := e.c.Calls("GetResourceAvailability")
	snap, err := e.d.Quotas(t.Context(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := bucketNamed(snap, "standard-e5-core-count"); b.Used != 2 || b.Limit != 40 {
		t.Errorf("bucket %+v, want used 2, limit 40", b)
	}
	// One call for the region-scoped name, three (the ADs) for each other name.
	names := len(snap.Buckets)
	if got, want := e.c.Calls("GetResourceAvailability")-before, 1+3*(names-1); got != want {
		t.Errorf("%d availability calls for %d buckets, want %d", got, names, want)
	}
}

func TestQuotaCounted(t *testing.T) {
	e := newEnv(t)
	mine := e.create(t, request(testType))
	otherReq := request(testType)
	otherReq.IdempotencyKey = "uid-other"
	otherReq.Tags[vendors.TagCluster], otherReq.Tags[vendors.TagNodeClaim], otherReq.Node.Name = "another", "claim-o", "claim-o"
	e.create(t, otherReq)
	gone := request(testType)
	gone.IdempotencyKey, gone.Tags[vendors.TagNodeClaim], gone.Node.Name = "uid-gone", "claim-g", "claim-g"
	g := e.create(t, gone)
	if err := e.d.DeleteInstance(t.Context(), g.Ref); err != nil {
		t.Fatal(err)
	}
	snap, err := e.d.Quotas(t.Context(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Counted) != 1 || snap.Counted[0] != mine.Ref {
		t.Errorf("counted %v, want only %s", snap.Counted, mine.Ref)
	}
	// The usage is the account's: the other cluster's instance counts in it.
	if b, _ := bucketNamed(snap, "standard-e5-core-count"); b.Used != 4 {
		t.Errorf("used %d, want 4 (this cluster's and the other's instances)", b.Used)
	}
}

func TestConsumption(t *testing.T) {
	e := newEnv(t)
	cons := func(typ, capType string) ([]vendors.Consumption, bool) { return e.d.Consumption(typ, capType) }
	// Before any shape table was read, no type is known.
	if _, ok := cons(testType, vendors.CapacityOnDemand); ok {
		t.Error("a type is known before the shape tables were read")
	}
	if _, err := e.d.Quotas(t.Context(), testRegion); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		typ, capType string
		want         []vendors.Consumption
		known        bool
	}{
		{testType, vendors.CapacityOnDemand, []vendors.Consumption{
			{Bucket: "standard-e5-core-count", Amount: 2}, {Bucket: "standard-e5-memory-count", Amount: 16}}, true},
		{"VM.Standard.A1.Flex.80-320", vendors.CapacityOnDemand, []vendors.Consumption{
			{Bucket: "standard-a1-core-count", Amount: 80}, {Bucket: "standard-a1-memory-count", Amount: 320}}, true},
		// A fixed shape consumes its own size: 4 OCPUs for VM.Standard2.4.
		{"VM.Standard2.4", vendors.CapacityOnDemand, []vendors.Consumption{{Bucket: "standard2-4-core-count", Amount: 4}}, true},
		{"BM.Standard3.64", vendors.CapacityOnDemand, []vendors.Consumption{{Bucket: "bm-standard3-64-count", Amount: 1}}, true},
		// Whether preemptible capacity has a limit of its own is unverified: unknown.
		{testType, vendors.CapacitySpot, nil, false},
		{"VM.Standard.Unknown.Flex.2-16", vendors.CapacityOnDemand, nil, false},
		{"VM.Standard.E5.Flex", vendors.CapacityOnDemand, nil, false},
		{"not a type", vendors.CapacityOnDemand, nil, false},
		// A sized name on a fixed shape and the reverse never consume a guess.
		{"VM.Standard2.4.4-8", vendors.CapacityOnDemand, nil, false},
	} {
		got, known := cons(tc.typ, tc.capType)
		if known != tc.known || len(got) != len(tc.want) {
			t.Errorf("Consumption(%s, %s) = %v, %v; want %v, %v", tc.typ, tc.capType, got, known, tc.want, tc.known)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Consumption(%s, %s)[%d] = %+v, want %+v", tc.typ, tc.capType, i, got[i], tc.want[i])
			}
		}
	}
}

// A limit the service cannot answer for leaves its bucket out and makes the shapes that
// consume it unknown; so does a limit name of another form.
func TestConsumptionUnknownLimits(t *testing.T) {
	c := newCloud(t)
	c.AddShape(fake.SeedRegion, fake.FixedShape("VM.Standard.Odd.1", 1, 8, "odd-capacity"))
	c.AddShape(fake.SeedRegion, fake.FixedShape("VM.Standard.None.1", 1, 8))
	c.SetUnsupported("standard-e5-memory-count")
	e := restart(t, c, testCluster)
	snap, err := e.d.Quotas(t.Context(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bucketNamed(snap, "standard-e5-memory-count"); ok {
		t.Error("a bucket for a limit that cannot be read")
	}
	if _, ok := bucketNamed(snap, "odd-capacity"); ok {
		t.Error("a bucket for a limit name of another form")
	}
	for _, typ := range []string{testType, "VM.Standard.Odd.1", "VM.Standard.None.1"} {
		if got, ok := e.d.Consumption(typ, vendors.CapacityOnDemand); ok {
			t.Errorf("Consumption(%s) = %v, want unknown", typ, got)
		}
	}
	if _, ok := e.d.Consumption("VM.Standard.E4.Flex.2-16", vendors.CapacityOnDemand); !ok {
		t.Error("another shape became unknown")
	}
}

func TestQuotaErrors(t *testing.T) {
	e := newEnv(t)
	if _, err := e.d.Quotas(t.Context(), fake.SeedRegionUnsubscribed); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("Quotas of an unsubscribed region: %v", err)
	}
	e.c.Inject("GetResourceAvailability", fake.Error(500, "InternalError", "x"), 1)
	if _, err := e.d.Quotas(t.Context(), testRegion); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("a failing limit read: %v, want Retryable", err)
	}
	// A limit the service answers 400 for is skipped, not an error of the region.
	e.c.Inject("GetResourceAvailability", fake.Error(400, "InvalidParameter", "unsupported"), 1)
	if _, err := e.d.Quotas(t.Context(), testRegion); err != nil {
		t.Errorf("a limit with no availability: %v", err)
	}
	f := newEnv(t)
	f.c.Inject("ListLimitDefinitions", fake.Error(503, "ServiceUnavailable", "x"), 1)
	if _, err := f.d.Quotas(t.Context(), testRegion); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("failing limit definitions: %v, want Retryable", err)
	}
}
