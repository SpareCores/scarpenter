/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const launchType = "aws.m7i.large"

// launchedInTheCheapestRegion launches a node and returns its NodeClaim (ready
// to be deleted) and the offering it took, which is the cheapest one.
func launchedInTheCheapestRegion(e *env) (*karpv1.NodeClaim, offeringOf) {
	e.t.Helper()
	target := e.offerings(launchType)[0]
	created, err := e.cp.Create(e.t.Context(), e.newClaim("claim-launched", []string{launchType}))
	if err != nil {
		e.t.Fatal(err)
	}
	if labelOf(created, corev1.LabelTopologyRegion) != target.region {
		e.t.Fatalf("launched in %s, want the cheapest region %s", labelOf(created, corev1.LabelTopologyRegion), target.region)
	}
	claim := &karpv1.NodeClaim{}
	claim.Name = "claim-launched"
	claim.Status.ProviderID = created.Status.ProviderID
	claim.Annotations = created.Annotations
	return claim, target
}

// failNextLaunch makes the vendor answer the claim's launch in the region with
// the class and checks that Create fails.
func (e *env) failNextLaunch(target offeringOf, class, claim string) {
	e.t.Helper()
	e.setConfig("errors", fmt.Sprintf("- match: {region: %s}\n  error: %s", target.region, class))
	if _, err := e.cp.Create(e.t.Context(), e.newClaim(claim, []string{launchType})); err == nil {
		e.t.Fatal("expected the launch to fail")
	}
}

func TestQuotaErrorMarksTheBucketForFiveMinutes(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 1})
	target := e.offerings(launchType)[0]
	e.failNextLaunch(target, "QuotaExceeded", "claim-1")

	other := target
	other.capacityType = map[string]string{"spot": "on-demand", "on-demand": "spot"}[target.capacityType]
	if !e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Fatal("the bucket of the failed launch is not marked")
	}
	if e.un.IsUnavailable(unavailableOfRegion(other)) {
		t.Error("the other capacity type, which has its own bucket, is marked")
	}
	e.advance(5*time.Minute - time.Second)
	if !e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Error("the mark expired before 5 minutes")
	}
	e.advance(time.Second)
	if e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Error("the mark outlived 5 minutes")
	}
}

// A vendor that says "quota" while the tracker shows room has a stale tracker:
// the bucket is marked all the same and the tracker refreshes.
func TestQuotaErrorWithHeadroomRefreshesTheTrackerAndMarksTheBucket(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 1})
	target := e.offerings(launchType)[0]
	e.setConfig("quota", quotaWith(target.region, target.capacityType, 100000, 0))
	if err := e.tracker.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if head, known := e.tracker.Headroom("aws", target.region, "m7i.large", target.capacityType); !known || head < 1000 {
		t.Fatalf("headroom = %d, known %v, want plenty", head, known)
	}
	// The real usage is full by now; only a refresh shows it.
	e.setConfig("quota", exhausted(target.region, target.capacityType))
	e.failNextLaunch(target, "QuotaExceeded", "claim-1")

	if !e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Error("a quota error with headroom in the tracker did not mark the bucket")
	}
	e.advance(time.Second) // past the debounce of the refresh
	e.tracker.Tick(t.Context())
	if head, _ := e.tracker.Headroom("aws", target.region, "m7i.large", target.capacityType); head > 0 {
		t.Errorf("headroom = %d after the quota error, want the refreshed view (none)", head)
	}
}

func TestAConfirmedGoneNodeClaimClearsTheQuotaMarkOfItsRegion(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 1})
	claim, target := launchedInTheCheapestRegion(e)
	e.failNextLaunch(target, "QuotaExceeded", "claim-2")
	if !e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Fatal("the quota mark is missing")
	}
	// The delete call only starts the termination; the vendor still counts the quota.
	if err := e.cp.Delete(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if !e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Error("the quota mark was cleared before the instance was confirmed gone")
	}
	if err := e.cp.Delete(t.Context(), claim); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("second Delete = %v, want NodeClaimNotFound", err)
	}
	if e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Error("the quota mark survived a NodeClaim that is gone")
	}
}

func TestATerminatingNodeClaimKeepsTheQuotaMark(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 1})
	claim, target := launchedInTheCheapestRegion(e)
	e.driver.PowerOff(vendors.InstanceRef(claim.Annotations[v1alpha1.AnnotationInstanceRef]))
	e.failNextLaunch(target, "QuotaExceeded", "claim-2")
	// The delete of the stopped instance is sent at once; until core asks again
	// and finds it gone, the instance still holds its quota.
	if err := e.cp.Delete(t.Context(), claim); err != nil {
		t.Fatalf("Delete of a terminating instance = %v, want nil", err)
	}
	if !e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Fatal("the quota mark was cleared before the instance is gone")
	}
}

func TestClearingQuotaMarksLeavesTheOtherClassesAlone(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 1})
	claim, target := launchedInTheCheapestRegion(e)
	o := unavailableOf(target)
	e.un.MarkInsufficientCapacity("aws", o.InstanceType, o.Zone, o.CapacityType)
	e.un.MarkInvalid("aws", o.InstanceType, o.Zone, o.CapacityType)
	marks := e.un.Len()
	if err := e.cp.Delete(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if err := e.cp.Delete(t.Context(), claim); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("second Delete = %v", err)
	}
	if got := e.un.Len(); got != marks {
		t.Errorf("%d marks after the deletes, want the %d of before: the capacity and invalid marks stay", got, marks)
	}
	e.un.MarkZoneUnusable("aws", o.Zone)
	e.un.ClearQuotaExceeded("aws", target.region)
	e.advance(time.Minute) // the capacity mark ends, the others stay
	if cls, _, ok := e.un.Why(o); !ok || (cls != "InvalidOffering" && cls != "ZoneUnusable") {
		t.Errorf("Why = %q, %v, want InvalidOffering or ZoneUnusable to stay", cls, ok)
	}
}

func TestDailyLimitMarksTheRegionUntilMidnightUTCAndDeletesDoNotClearIt(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 1})
	claim, target := launchedInTheCheapestRegion(e)
	now := e.clock.Now().UTC()
	reset := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	e.failNextLaunch(target, "DailyLimit", "claim-2")

	everything := func(want bool) {
		t.Helper()
		for _, ct := range []string{"on-demand", "spot"} {
			o := unavailableOfRegion(offeringOf{region: target.region, capacityType: ct})
			o.InstanceType, o.APIReference = "aws.c7g.large", "c7g.large"
			if got := e.un.IsUnavailable(o); got != want {
				t.Errorf("%s %s unavailable = %v, want %v", o.Region, ct, got, want)
			}
		}
	}
	everything(true)
	if cls, expiry, _ := e.un.Why(unavailableOfRegion(target)); cls != "DailyLimit" || !expiry.Equal(reset) {
		t.Errorf("Why = %q until %v, want DailyLimit until %v", cls, expiry, reset)
	}
	if e.un.IsUnavailable(unavailable.Offering{
		Vendor: "aws", Region: region2, Zone: "z", InstanceType: launchType, APIReference: "m7i.large", CapacityType: "spot",
	}) && target.region != region2 {
		t.Error("the other region is marked")
	}

	if err := e.cp.Delete(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	everything(true)
	e.clock.SetTime(reset.Add(-time.Second))
	everything(true)
	e.clock.SetTime(reset)
	everything(false)
}

func TestLaunchFailuresCarryTheClassInTheDocumentedFormat(t *testing.T) {
	for _, tc := range []struct {
		class, prefix string
		// insufficient is set for the classes that end in core's
		// InsufficientCapacityError, whose text is "insufficient capacity, <message>".
		insufficient bool
	}{
		{"QuotaExceeded", "QuotaExceeded (InjectedQuotaExceeded): ", true},
		{"DailyLimit", "DailyLimit (InjectedDailyLimit): ", true},
		{"InsufficientCapacity", "InsufficientCapacity (InjectedInsufficientCapacity): ", true},
		{"Fatal", "Fatal (InjectedFatal): ", false},
		{"Retryable", "Retryable (InjectedRetryable): ", false},
	} {
		t.Run(tc.class, func(t *testing.T) {
			e := newEnv(t, envOptions{attempts: 1})
			e.setConfig("errors", fmt.Sprintf("- {error: %s}", tc.class))
			_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{launchType}))
			if err == nil {
				t.Fatal("expected the launch to fail")
			}
			if tc.insufficient {
				if !karpcloudprovider.IsInsufficientCapacityError(err) ||
					!strings.HasPrefix(err.Error(), "insufficient capacity, "+tc.prefix) {
					t.Errorf("error = %q, want insufficient capacity, %s...", err, tc.prefix)
				}
				return
			}
			ce, ok := errors.AsType[*karpcloudprovider.CreateError](err)
			if !ok || !strings.HasPrefix(ce.ConditionMessage, tc.prefix) {
				t.Fatalf("error %v, want a CreateError whose message has the prefix %q", err, tc.prefix)
			}
			for _, p := range e.events.published {
				if p.Reason != "LaunchAttempts" && strings.Contains(p.Message, tc.class) && !strings.HasPrefix(p.Message, tc.prefix) {
					t.Errorf("event message %q, want the prefix %q", p.Message, tc.prefix)
				}
			}
		})
	}
}

func TestDailyLimitAfterMidnightKeepsOnlyTheQuotaTTL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    time.Time
		expiry time.Time
	}{
		{"just after midnight", time.Date(2026, 11, 21, 0, 3, 0, 0, time.UTC), time.Date(2026, 11, 21, 0, 8, 0, 0, time.UTC)},
		{"at midnight", time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 21, 0, 5, 0, 0, time.UTC)},
		{"5 minutes after midnight", time.Date(2026, 11, 21, 0, 5, 0, 0, time.UTC), time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)},
		{"just before midnight", time.Date(2026, 11, 21, 23, 59, 0, 0, time.UTC), time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, envOptions{attempts: 1})
			e.clock.SetTime(tc.now)
			target := e.offerings(launchType)[0]
			e.failNextLaunch(target, "DailyLimit", "claim-1")
			if _, expiry, ok := e.un.Why(unavailableOfRegion(target)); !ok || !expiry.Equal(tc.expiry) {
				t.Errorf("expiry = %v, %v, want %v", expiry, ok, tc.expiry)
			}
		})
	}
}
