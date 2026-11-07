/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws/fake"
)

// The quota codes of overview section 11.8, written out here independently of
// the driver's table.
const (
	codeStdOD, codeStdSpot = "L-1216C47A", "L-34B43A08"
	codeGOD, codeGSpot     = "L-DB2E81BA", "L-3819A6DF"
	codePOD                = "L-417A185B"
	codeHPCOD              = "L-F7808C92"
	codeHighMemOD          = "L-43DA4232"
)

func TestFamilyToClassRule(t *testing.T) {
	for typ, want := range map[string]string{
		// the pitfalls the order of the rule handles
		"d2.xlarge": aws.ClassStandard, "d3.xlarge": aws.ClassStandard, "d3en.xlarge": aws.ClassStandard,
		"i3.large": aws.ClassStandard, "im4gn.large": aws.ClassStandard, "is4gen.large": aws.ClassStandard,
		"i4i.large": aws.ClassStandard, "h1.2xlarge": aws.ClassStandard, "t3.micro": aws.ClassStandard,
		"t4g.nano": aws.ClassStandard, "m7i.large": aws.ClassStandard, "m5zn.large": aws.ClassStandard,
		"a1.medium": aws.ClassStandard, "c7i-flex.large": aws.ClassStandard, "z1d.large": aws.ClassStandard,
		"r8g.large": aws.ClassStandard, "gr6.4xlarge": aws.ClassG, "gr6f.4xlarge": aws.ClassG, "g5.xlarge": aws.ClassG,
		"g6e.xlarge": aws.ClassG, "vt1.3xlarge": aws.ClassG, "f1.2xlarge": aws.ClassF, "f2.6xlarge": aws.ClassF,
		"p4d.24xlarge": aws.ClassP, "p6-b200.48xlarge": aws.ClassP, "x2idn.16xlarge": aws.ClassX, "x8g.large": aws.ClassX,
		"inf1.xlarge": aws.ClassInf, "inf2.xlarge": aws.ClassInf, "trn1.2xlarge": aws.ClassTrn, "trn2.48xlarge": aws.ClassTrn,
		"dl1.24xlarge": aws.ClassDL, "dl2q.24xlarge": aws.ClassDL, "hpc7a.96xlarge": aws.ClassHPC, "hpc6id.32xlarge": aws.ClassHPC,
		"u-3tb1.56xlarge": aws.ClassHighMem, "u7i-12tb.224xlarge": aws.ClassHighMem, "u7in-16tb.224xlarge": aws.ClassHighMem,
	} {
		if got, ok := aws.ClassOf(typ); !ok || got != want {
			t.Errorf("%s: class %q (%v), want %q", typ, got, ok, want)
		}
	}
	// mac has no vCPU quota and a family the rule does not know is not guessed.
	for _, typ := range []string{"mac1.metal", "mac2-m2pro.metal", "mac-m4.metal", "k9.large", "q1.large", ""} {
		if got, ok := aws.ClassOf(typ); ok {
			t.Errorf("%s: class %q, want none", typ, got)
		}
	}
}

// TestEveryFamilyOfTheSnapshotIsClassified fails when the committed Keeper
// snapshot has an AWS family the rule does not know: a new family must be
// classified before it is offered (overview section 11.8).
func TestEveryFamilyOfTheSnapshotIsClassified(t *testing.T) {
	families := map[string]string{}
	for _, r := range awsServers(t) {
		fam, _, _ := strings.Cut(r.APIReference, ".")
		families[fam] = r.APIReference
	}
	if len(families) < 150 {
		t.Fatalf("the snapshot has %d AWS families, want the full list", len(families))
	}
	for fam, typ := range families {
		_, ok := aws.ClassOf(typ)
		if !ok && !strings.HasPrefix(fam, "mac") {
			t.Errorf("the family %s (%s) is not classified; add it to the family rule and to the quota table", fam, typ)
		}
	}
}

func quotaEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, envOptions{})
	e.acct.SetQuota(testRegion, codeStdOD, 100)
	e.acct.SetQuota(testRegion, codeStdSpot, 50)
	if _, err := e.d.LaunchableTypes(context.Background(), testRegion); err != nil {
		t.Fatal(err)
	}
	return e
}

func bucket(t *testing.T, s *vendors.QuotaSnapshot, id string) vendors.Bucket {
	t.Helper()
	for _, b := range s.Buckets {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("no bucket %s in %+v", id, s.Buckets)
	return vendors.Bucket{}
}

func TestQuotasLimitsAndTheEighteenCodes(t *testing.T) {
	e := quotaEnv(t)
	snap, err := e.d.Quotas(context.Background(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Buckets) != 18 {
		t.Errorf("%d buckets, want the 18 quota codes: %+v", len(snap.Buckets), snap.Buckets)
	}
	for _, b := range snap.Buckets {
		if b.Scope != vendors.QuotaScopeRegion || b.Unit != "vCPU" {
			t.Errorf("bucket %+v: want a regional vCPU bucket", b)
		}
	}
	if b := bucket(t, snap, codeStdOD); b.Limit != 100 || b.Used != 0 {
		t.Errorf("standard on-demand = %+v, want limit 100 used 0", b)
	}
	if b := bucket(t, snap, codeStdSpot); b.Limit != 50 {
		t.Errorf("standard spot = %+v", b)
	}
	// A new account's defaults: nothing for GPU instances.
	if b := bucket(t, snap, codeGOD); b.Limit != 0 {
		t.Errorf("G on-demand limit = %d, want 0 (QuotaZero)", b.Limit)
	}
	g, err := e.d.GlobalQuotas(context.Background())
	if err != nil || len(g.Buckets) != 0 {
		t.Errorf("GlobalQuotas = %+v, %v; EC2's vCPU quotas are all regional", g, err)
	}
}

func TestLimitsAreCachedUnlessRefreshed(t *testing.T) {
	e := quotaEnv(t)
	ctx := context.Background()
	if _, err := e.d.Quotas(ctx, testRegion); err != nil {
		t.Fatal(err)
	}
	before := e.acct.Calls("GetServiceQuota")
	e.acct.SetQuota(testRegion, codeStdOD, 7)
	usageOnly := vendors.WithQuotaRefresh(ctx, vendors.QuotaRefresh{Limits: false})
	snap, err := e.d.Quotas(usageOnly, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if e.acct.Calls("GetServiceQuota") != before || bucket(t, snap, codeStdOD).Limit != 100 {
		t.Errorf("a usage refresh read the limits again (calls %d -> %d, limit %d)",
			before, e.acct.Calls("GetServiceQuota"), bucket(t, snap, codeStdOD).Limit)
	}
	snap, err = e.d.Quotas(vendors.WithQuotaRefresh(ctx, vendors.QuotaRefresh{Limits: true}), testRegion)
	if err != nil || bucket(t, snap, codeStdOD).Limit != 7 {
		t.Errorf("a limits refresh = %+v, %v; want limit 7", bucket(t, snap, codeStdOD), err)
	}
	// After the TTL a usage refresh reads them again by itself.
	e.acct.SetQuota(testRegion, codeStdOD, 9)
	e.clock.Step(16 * time.Minute)
	snap, err = e.d.Quotas(usageOnly, testRegion)
	if err != nil || bucket(t, snap, codeStdOD).Limit != 9 {
		t.Errorf("an old cache was not refreshed: %+v, %v", bucket(t, snap, codeStdOD), err)
	}
}

func TestAMissingQuotaIsFoundInTheListingOrOmitted(t *testing.T) {
	e := quotaEnv(t)
	// GetServiceQuota says the quota does not exist in the region: the driver has
	// no bucket for it (its types stay unknown quota) instead of failing.
	e.acct.Fail(fake.Failure{Op: "GetServiceQuota", Code: "NoSuchResourceException", Count: -1,
		Match: func(in any) bool { return deref(in.(*servicequotas.GetServiceQuotaInput).QuotaCode) == codeHPCOD }})
	snap, err := e.d.Quotas(context.Background(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	// The listing knows it: the fallback fills it in.
	if b := bucket(t, snap, codeHPCOD); b.ID != codeHPCOD {
		t.Error("the quota the listing knows has no bucket")
	}
	e.acct.RemoveQuota(testRegion, codeHighMemOD)
	snap, err = e.d.Quotas(vendors.WithQuotaRefresh(context.Background(), vendors.QuotaRefresh{Limits: true}), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(snap.Buckets, func(b vendors.Bucket) bool { return b.ID == codeHighMemOD }); i >= 0 {
		t.Errorf("a quota unknown to Service Quotas has a bucket: %+v", snap.Buckets[i])
	}
	if len(snap.Buckets) != 17 {
		t.Errorf("%d buckets, want 17", len(snap.Buckets))
	}
}

func launch(t *testing.T, e *env, typ, capacity string) *vendors.Instance {
	t.Helper()
	inst, err := e.d.CreateInstance(context.Background(), request(testRegion, testZone, typ, capacity))
	if err != nil {
		t.Fatalf("launching %s/%s: %v", typ, capacity, err)
	}
	return inst
}

func used(t *testing.T, e *env, code string) int64 {
	t.Helper()
	snap, err := e.d.Quotas(context.Background(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	return bucket(t, snap, code).Used
}

func TestUsageCountsOnDemandAndSpotSeparately(t *testing.T) {
	e := quotaEnv(t)
	od := launch(t, e, "m7i.xlarge", vendors.CapacityOnDemand) // 4 vCPUs
	sp := launch(t, e, "m7i.large", vendors.CapacitySpot)      // 2
	if got := used(t, e, codeStdOD); got != 4 {
		t.Errorf("on-demand used = %d, want 4", got)
	}
	if got := used(t, e, codeStdSpot); got != 2 {
		t.Errorf("spot used = %d, want 2 (a spot instance never counts as on-demand)", got)
	}
	snap, _ := e.d.Quotas(context.Background(), testRegion)
	if !slices.Contains(snap.Counted, od.Ref) || !slices.Contains(snap.Counted, sp.Ref) {
		t.Errorf("Counted = %v, want both instances", snap.Counted)
	}
}

func TestUsageCountsTheWholeAccount(t *testing.T) {
	e := quotaEnv(t)
	e.acct.AddForeignInstance(testRegion, "m7i.xlarge", false, ec2types.InstanceStateNameRunning)
	e.acct.AddForeignInstance(testRegion, "m7i.large", true, ec2types.InstanceStateNameRunning)
	if got := used(t, e, codeStdOD); got != 4 {
		t.Errorf("on-demand used = %d, want the foreign instance's 4", got)
	}
	if got := used(t, e, codeStdSpot); got != 2 {
		t.Errorf("spot used = %d, want the foreign spot instance's 2", got)
	}
}

func TestUsageStatesAndClasses(t *testing.T) {
	e := quotaEnv(t)
	ctx := context.Background()
	for state, want := range map[ec2types.InstanceStateName]int64{
		ec2types.InstanceStateNamePending: 2, ec2types.InstanceStateNameRunning: 2,
		// Too short to observe: they count conservatively until they leave.
		ec2types.InstanceStateNameStopping: 2, ec2types.InstanceStateNameShuttingDown: 2,
		// Verified: a stopped instance does not count, and a terminated one is gone.
		ec2types.InstanceStateNameStopped: 0, ec2types.InstanceStateNameTerminated: 0,
	} {
		e2 := quotaEnv(t)
		e2.acct.AddForeignInstance(testRegion, "m7i.large", false, state)
		if got := used(t, e2, codeStdOD); got != want {
			t.Errorf("an instance %s: used %d, want %d", state, got, want)
		}
	}
	// Other classes have their own buckets: a P instance does not use the
	// Standard bucket, nor a G one the P bucket.
	e.acct.SetQuota(testRegion, codePOD, 200)
	e.acct.AddForeignInstance(testRegion, "p4d.24xlarge", false, ec2types.InstanceStateNameRunning)
	snap, err := e.d.Quotas(ctx, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if got := bucket(t, snap, codePOD).Used; got != 96 {
		t.Errorf("P used = %d, want 96", got)
	}
	if got := bucket(t, snap, codeStdOD).Used; got != 0 {
		t.Errorf("standard used = %d, want 0", got)
	}
	// An On-Demand-only class has no spot bucket.
	e.acct.AddForeignInstance(testRegion, "hpc7a.96xlarge", false, ec2types.InstanceStateNameRunning)
	snap, _ = e.d.Quotas(ctx, testRegion)
	if got := bucket(t, snap, codeHPCOD).Used; got != 192 {
		t.Errorf("HPC used = %d, want 192", got)
	}
}

func TestVCPUsComeFromTheTypeNotFromCPUOptions(t *testing.T) {
	// m6id.large has 2 vCPUs (DefaultVCpus); an instance launched with one core
	// and one thread still counts 2 (verified live on 2026-11-05). The driver
	// never sets or reads CpuOptions, so the count is the type's.
	e := quotaEnv(t)
	launch(t, e, "m6id.large", vendors.CapacityOnDemand)
	if got := used(t, e, codeStdOD); got != 2 {
		t.Errorf("used = %d, want the type's 2 vCPUs", got)
	}
	for _, in := range e.acct.Inputs("RunInstances") {
		if in.(*ec2.RunInstancesInput).CpuOptions != nil {
			t.Error("a launch sets CpuOptions, which quota counting must not depend on")
		}
	}
}

func TestSpotRequestsAreCountedOnlyWhileOpenWithoutAnInstance(t *testing.T) {
	e := quotaEnv(t)
	ctx := context.Background()
	// An open request without an instance counts.
	e.acct.AddOpenSpotRequest(testRegion, "m7i.xlarge")
	if got := used(t, e, codeStdSpot); got != 4 {
		t.Fatalf("an open spot request: used %d, want 4", got)
	}
	// A fulfilled request stays active with its instance: instance and request
	// together count once.
	inst := launch(t, e, "m7i.large", vendors.CapacitySpot)
	if got := used(t, e, codeStdSpot); got != 4+2 {
		t.Errorf("an instance with its active request: used %d, want 6 (open 4 + instance 2, not double)", got)
	}
	// The instance ends; its request stays active for minutes, then closes. In
	// both phases nothing of it counts: counting requests next to instances would
	// double-count and then over-count.
	if err := e.d.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	states := e.acct.SpotRequests(testRegion)
	active := 0
	for _, s := range states {
		if s == "active" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("the request of a terminated instance is %v, want it still active", states)
	}
	if got := used(t, e, codeStdSpot); got != 4 {
		t.Errorf("a request still active after its instance terminated: used %d, want 4 (the open request only)", got)
	}
	e.clock.Step(4 * time.Minute)
	for _, s := range e.acct.SpotRequests(testRegion) {
		if s == "active" {
			t.Fatal("the request did not close")
		}
	}
	if got := used(t, e, codeStdSpot); got != 4 {
		t.Errorf("after the request closed: used %d, want 4", got)
	}
}

func TestUnusedCapacityReservationsCount(t *testing.T) {
	e := quotaEnv(t)
	e.acct.AddCapacityReservation(testRegion, fake.CapacityReservation{ID: "cr-1", Type: "m7i.xlarge", AZ: testZone, Total: 5, Free: 3})
	e.acct.AddCapacityReservation(testRegion, fake.CapacityReservation{ID: "cr-2", Type: "m7i.large", AZ: testZone, Total: 2, Free: 2, State: "expired"})
	e.acct.AddCapacityReservation(testRegion, fake.CapacityReservation{ID: "cr-3", Type: "m7i.large", AZ: testZone, Total: 2, Free: 2, Kind: "capacity-block"})
	// Only the unused part of an active On-Demand reservation counts: the used
	// part is a running instance, which is counted as an instance.
	if got := used(t, e, codeStdOD); got != 3*4 {
		t.Errorf("used = %d, want the unused 3 instances x 4 vCPUs (not the expired reservation, not the capacity block)", got)
	}
}

func TestQuotaPaginationOverManyInstances(t *testing.T) {
	e := quotaEnv(t)
	for range 11 { // pages of three
		e.acct.AddForeignInstance(testRegion, "m7i.large", false, ec2types.InstanceStateNameRunning)
	}
	if got := used(t, e, codeStdOD); got != 22 {
		t.Errorf("used = %d, want 22", got)
	}
}

func TestConsumption(t *testing.T) {
	e := quotaEnv(t)
	for _, tc := range []struct {
		typ, capacity string
		want          []vendors.Consumption
	}{
		{"m7i.large", "on-demand", []vendors.Consumption{{Bucket: codeStdOD, Amount: 2}}},
		{"m7i.large", "spot", []vendors.Consumption{{Bucket: codeStdSpot, Amount: 2}}},
		{"p4d.24xlarge", "on-demand", []vendors.Consumption{{Bucket: codePOD, Amount: 96}}},
		{"hpc7a.96xlarge", "on-demand", []vendors.Consumption{{Bucket: codeHPCOD, Amount: 192}}},
		{"u-3tb1.56xlarge", "on-demand", []vendors.Consumption{{Bucket: codeHighMemOD, Amount: 448}}},
		// no spot quota for these classes
		{"hpc7a.96xlarge", "spot", nil}, {"u-3tb1.56xlarge", "spot", nil},
		// mac: no vCPU quota; a type nobody told the driver about: unknown
		{"mac2.metal", "on-demand", nil}, {"zz9.large", "on-demand", nil},
	} {
		got, known := e.d.Consumption(tc.typ, tc.capacity)
		if known != (tc.want != nil) || !slices.Equal(got, tc.want) {
			t.Errorf("Consumption(%s, %s) = %v, %v; want %v", tc.typ, tc.capacity, got, known, tc.want)
		}
	}
}

func TestConsumptionIsUnknownUntilTheTypesAreRead(t *testing.T) {
	e := newEnv(t, envOptions{})
	if _, known := e.d.Consumption("m7i.large", "on-demand"); known {
		t.Error("the consumption is known before the vCPUs of the type were read")
	}
	if _, err := e.d.LaunchableTypes(context.Background(), testRegion); err != nil {
		t.Fatal(err)
	}
	if _, known := e.d.Consumption("m7i.large", "on-demand"); !known {
		t.Error("the consumption is unknown after the types were read")
	}
}

func TestLaunchableTypesByZoneID(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.acct.SetOffering(testRegion, "p4d.24xlarge", "use1-az2")
	got, err := e.d.LaunchableTypes(context.Background(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"use1-az1", "use1-az2", "use1-az3"}; !slices.Equal(got["m7i.large"], want) {
		t.Errorf("m7i.large launchable in %v, want every zone ID %v", got["m7i.large"], want)
	}
	if want := []string{"use1-az2"}; !slices.Equal(got["p4d.24xlarge"], want) {
		t.Errorf("p4d.24xlarge launchable in %v, want %v", got["p4d.24xlarge"], want)
	}
	for typ := range got {
		if strings.HasPrefix(typ, "mac") {
			t.Errorf("%s is listed: mac types are dropped (no vCPU quota, macOS, Dedicated Hosts)", typ)
		}
	}
	in := e.acct.Inputs("DescribeInstanceTypeOfferings")[0].(*ec2.DescribeInstanceTypeOfferingsInput)
	if in.LocationType != ec2types.LocationTypeAvailabilityZoneId {
		t.Errorf("location type %q, want availability-zone-id", in.LocationType)
	}
}

func TestLaunchableTypesPaginates(t *testing.T) {
	e := newEnv(t, envOptions{types: snapshotTypes(t)})
	got, err := e.d.LaunchableTypes(context.Background(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 1300 {
		t.Errorf("%d launchable types of the %d in the snapshot", len(got), len(snapshotTypes(t)))
	}
	if e.acct.Calls("DescribeInstanceTypeOfferings") < 2 {
		t.Error("the offerings were not read in pages")
	}
}

func TestQuotasOfAnUnknownRegionFail(t *testing.T) {
	e := quotaEnv(t)
	if _, err := e.d.Quotas(context.Background(), "mars-north-1"); err == nil {
		t.Error("quotas of a region the account does not have")
	}
}

func TestAnInstanceTypeNobodyKnowsIsLoggedAndNotCounted(t *testing.T) {
	e := quotaEnv(t)
	// A type the account runs but DescribeInstanceTypes does not list (retired):
	// its usage cannot be counted, which must not fail the refresh.
	e.acct.AddForeignInstance(testRegion, "m99.large", false, ec2types.InstanceStateNameRunning)
	e.acct.AddForeignInstance(testRegion, "m7i.large", false, ec2types.InstanceStateNameRunning)
	if got := used(t, e, codeStdOD); got != 2 {
		t.Errorf("used = %d, want the known instance's 2", got)
	}
}

func TestAnOpenRequestThatAlreadyHasAnInstanceIsCountedOnce(t *testing.T) {
	e := quotaEnv(t)
	req := e.acct.AddOpenSpotRequest(testRegion, "m7i.large")
	id := e.acct.AddForeignInstance(testRegion, "m7i.large", true, ec2types.InstanceStateNameRunning)
	e.acct.AssignSpotRequest(testRegion, req, id)
	// The foreign helper added a request of its own for the instance (active); the
	// open one that points at it must not add another 2.
	if got := used(t, e, codeStdSpot); got != 2 {
		t.Errorf("used = %d, want 2", got)
	}
}

func TestOneUnknownTypeDoesNotHideTheKnownOnes(t *testing.T) {
	// A name EC2 does not know fails a whole by-name lookup: the known type asked
	// for in the same call must still be counted (not counting it under-counts the
	// quota), and the unknown one is not asked for at every refresh.
	e := quotaEnv(t)
	e.acct.SetInstanceTypes(append(slices.Clone(smallTypes), fake.InstanceType{Name: "c7i.xlarge", VCPUs: 4}))
	e.acct.AddForeignInstance(testRegion, "m99.large", false, ec2types.InstanceStateNameRunning)
	e.acct.AddForeignInstance(testRegion, "c7i.xlarge", false, ec2types.InstanceStateNameRunning)
	if got := used(t, e, codeStdOD); got != 4 {
		t.Errorf("used = %d, want the known type's 4", got)
	}
	calls := e.acct.Calls("DescribeInstanceTypes")
	if got := used(t, e, codeStdOD); got != 4 || e.acct.Calls("DescribeInstanceTypes") != calls {
		t.Errorf("a second refresh: used %d, %d new lookups; the unknown name must be remembered",
			got, e.acct.Calls("DescribeInstanceTypes")-calls)
	}
	e.clock.Step(2 * time.Hour)
	_ = used(t, e, codeStdOD)
	if e.acct.Calls("DescribeInstanceTypes") == calls {
		t.Error("an unknown name was never asked for again")
	}
}
