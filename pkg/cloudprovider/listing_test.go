//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

// flakyDriver fails the listing of the regions in down, counts the listings,
// and fails the Prepare of a launch (in prepareDown, all regions when empty)
// while prepareErr is set and the image resolution of imageDown. It lists per
// region, as the drivers that call the vendor per region do.
type flakyDriver struct {
	*fakedriver.Driver
	down        atomic.Pointer[map[string]bool]
	listCalls   atomic.Int32
	prepareErr  atomic.Pointer[error]
	prepareDown atomic.Pointer[map[string]bool]
	imageDown   atomic.Pointer[map[string]bool]
	onPrepare   func()
}

func (d *flakyDriver) ListsPerRegion() {}

var timeout error = vendors.NewError(vendors.Retryable, "CallTimeout",
	fmt.Errorf("DescribeVSwitches: %w", context.DeadlineExceeded))

func (d *flakyDriver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	d.listCalls.Add(1)
	if down := d.down.Load(); down != nil {
		for _, r := range regions {
			if (*down)[r] {
				return nil, timeout
			}
		}
	}
	return d.Driver.ListInstances(ctx, regions, tags)
}

func (d *flakyDriver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	if d.onPrepare != nil {
		d.onPrepare()
	}
	if err := d.prepareErr.Load(); err != nil {
		if down := d.prepareDown.Load(); down == nil || (*down)[req.Region] {
			return nil, *err
		}
	}
	return d.Driver.Prepare(ctx, req)
}

func (d *flakyDriver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	if down := d.imageDown.Load(); down != nil && (*down)[req.Region] {
		return nil, timeout
	}
	return d.Driver.ResolveImage(ctx, req)
}

// plainDriver lists the whole account in one call, like the drivers that do not
// implement vendors.RegionLister.
type plainDriver struct {
	*fakedriver.Driver
	listCalls atomic.Int32
}

func (d *plainDriver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	d.listCalls.Add(1)
	return d.Driver.ListInstances(ctx, regions, tags)
}

func newFlakyEnv(t *testing.T) (*env, *flakyDriver) {
	d := &flakyDriver{}
	e := newEnv(t, envOptions{wrap: func(f *fakedriver.Driver) vendors.Driver { d.Driver = f; return d }})
	return e, d
}

func regionOf(nc *karpv1.NodeClaim) string { return labelOf(nc, corev1.LabelTopologyRegion) }

// A region that cannot be listed keeps the instances of the last listing: core's
// garbage collector deletes the registered NodeClaims missing from List, so a
// slow region must not make them look gone.
func TestListKeepsTheInstancesOfARegionThatCannotBeListed(t *testing.T) {
	e, d := newFlakyEnv(t)
	nc, err := e.cp.Create(t.Context(), e.newClaim("claim-a", []string{launchType}))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.cp.List(t.Context()); err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v, want the instance", got, err)
	}
	d.down.Store(&map[string]bool{regionOf(nc): true})
	e.advance(time.Hour) // the cache is stale: the next List lists again

	listed, err := e.cp.List(t.Context())
	if err != nil {
		t.Fatalf("List with a region down = %v, want the listing of the others", err)
	}
	if len(listed) != 1 {
		t.Errorf("List = %d NodeClaims, want the one of the last listing", len(listed))
	}
}

// A region that was never listed has nothing to keep: it is empty unless a
// NodeClaim lives in it, and one down region does not block the others.
func TestListOfANeverListedRegionDownFailsOnlyWithANodeClaimInIt(t *testing.T) {
	e, d := newFlakyEnv(t)
	d.down.Store(&map[string]bool{region1: true})
	if _, err := e.cp.List(t.Context()); err != nil {
		t.Fatalf("List with a region down and no NodeClaim in it = %v, want an empty region", err)
	}

	e, d = newFlakyEnv(t)
	d.down.Store(&map[string]bool{region1: true})
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim-x"}}
	if err := e.client.Create(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	claim.Status.ProviderID = scope.ProviderID("aws", region1, "claim-x")
	if err := e.client.Status().Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cp.List(t.Context()); err == nil {
		t.Error("List succeeded with a NodeClaim in a region that cannot be listed: it would look gone")
	}
}

// A region that failed is not asked again for a few minutes, so that a region
// that hangs does not make every listing slow; the cache is fresh from the end
// of the listing.
func TestAFailedRegionIsNotListedAgainForAWhile(t *testing.T) {
	e, d := newFlakyEnv(t)
	if _, err := e.cp.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.down.Store(&map[string]bool{region1: true})
	e.advance(time.Hour)
	if _, err := e.cp.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := d.listCalls.Load()
	e.advance(time.Minute)
	if _, err := e.cp.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := d.listCalls.Load(); got != before+1 {
		t.Errorf("%d listings after a minute, want only the healthy region's (%d)", got-before, 1)
	}
	e.advance(5 * time.Minute)
	before = d.listCalls.Load()
	if _, err := e.cp.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := d.listCalls.Load(); got != before+2 {
		t.Errorf("%d listings after the retry delay, want both regions", got-before)
	}
}

// A driver that lists the whole account is asked once, not once per region.
func TestADriverListingTheAccountIsAskedOnce(t *testing.T) {
	d := &plainDriver{}
	e := newEnv(t, envOptions{wrap: func(f *fakedriver.Driver) vendors.Driver { d.Driver = f; return d }})
	if _, err := e.cp.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := d.listCalls.Load(); got != 1 {
		t.Errorf("%d listings, want 1", got)
	}
}

// A preparing call that times out sets the region aside and the same Create
// launches in the next one; the failure is an Event naming the slow action.
func TestARetryablePrepareMovesTheLaunchToAnotherRegion(t *testing.T) {
	e, d := newFlakyEnv(t)
	cheapest := e.offerings(launchType)[0]
	d.prepareErr.Store(&timeout)
	d.prepareDown.Store(&map[string]bool{cheapest.region: true})

	nc, cerr := e.cp.Create(t.Context(), e.newClaim("claim-move", []string{launchType}))
	if cerr != nil {
		t.Fatalf("Create = %v, want a launch in another region", cerr)
	}
	if got := regionOf(nc); got == cheapest.region {
		t.Errorf("launched in the unreachable region %s", got)
	}
	if !e.un.IsUnavailable(unavailableOfRegion(cheapest)) {
		t.Error("the unreachable region is not set aside")
	}
	if !slices.Contains(e.events.reasons(), cloudprovider.EventLaunchFailed) {
		t.Errorf("events = %v, want a LaunchFailed warning", e.events.reasons())
	}
	attempts := e.events.byReason(cloudprovider.EventLaunchAttempts)
	if len(attempts) != 1 || !strings.Contains(attempts[0].Message, "|Retryable|") ||
		strings.Contains(attempts[0].Message, "|Fatal|") {
		t.Errorf("LaunchAttempts = %+v, want the timeout recorded as Retryable", attempts)
	}
	e.advance(unavailable.DefaultUnreachableTTL)
	if e.un.IsUnavailable(unavailableOfRegion(cheapest)) {
		t.Error("the region stays set aside after the TTL")
	}
}

// The image resolution that times out is handled like the preparing.
func TestAnImageTimeoutMovesTheLaunchToAnotherRegion(t *testing.T) {
	e, d := newFlakyEnv(t)
	cheapest := e.offerings(launchType)[0]
	d.imageDown.Store(&map[string]bool{cheapest.region: true})
	nc, cerr := e.cp.Create(t.Context(), e.newClaim("claim-image", []string{launchType}))
	if cerr != nil {
		t.Fatalf("Create = %v, want a launch in another region", cerr)
	}
	if regionOf(nc) == cheapest.region || !e.un.IsUnavailable(unavailableOfRegion(cheapest)) {
		t.Errorf("launched in %s, region set aside: %v", regionOf(nc), e.un.IsUnavailable(unavailableOfRegion(cheapest)))
	}
}

// When an earlier attempt of the NodeClaim may have requested an instance (its
// bootstrap token exists), a timeout does not move the launch to another
// region: an instance could be created twice.
func TestAPrepareTimeoutAfterAPossibleCreateDoesNotMove(t *testing.T) {
	e, d := newFlakyEnv(t)
	cheapest := e.offerings(launchType)[0]
	d.prepareErr.Store(&timeout)
	d.prepareDown.Store(&map[string]bool{cheapest.region: true})
	e.tokens.minted = append(e.tokens.minted, "claim-kept") // the token an earlier attempt kept

	_, cerr := e.cp.Create(t.Context(), e.newClaim("claim-kept", []string{launchType}))
	if cerr == nil || !isCreateError(cerr) {
		t.Fatalf("Create = %v, want a CreateError for a retry", cerr)
	}
	if n := len(e.calls("create")); n != 0 {
		t.Errorf("%d instances were created", n)
	}
	if e.un.IsUnavailable(unavailableOfRegion(cheapest)) {
		t.Error("the region was set aside")
	}
}

// Only a deadline moves the launch: another Retryable error keeps its handling.
func TestAPrepareErrorThatIsNoDeadlineDoesNotMove(t *testing.T) {
	e, d := newFlakyEnv(t)
	var throttled error = vendors.NewError(vendors.Retryable, "Throttling", errors.New("slow down"))
	d.prepareErr.Store(&throttled)
	_, cerr := e.cp.Create(t.Context(), e.newClaim("claim-throttled", []string{launchType}))
	if cerr == nil || !isCreateError(cerr) {
		t.Fatalf("Create = %v, want a CreateError for a retry", cerr)
	}
	if len(e.calls("create")) != 0 {
		t.Error("an instance was created")
	}
}

// A launch whose preparing times out in every region creates nothing: the
// regions are set aside for a few minutes and Create fails with a message that
// names the slow action; a Create after the TTL launches.
func TestARetryablePrepareInEveryRegionLaunchesNothing(t *testing.T) {
	e, d := newFlakyEnv(t)
	d.prepareErr.Store(&timeout)
	_, cerr := e.cp.Create(t.Context(), e.newClaim("claim-prep", []string{launchType}))
	if cerr == nil || !strings.Contains(cerr.Error(), "DescribeVSwitches") {
		t.Fatalf("Create = %v, want a failure naming the slow action", cerr)
	}
	if n := len(e.calls("create")); n != 0 {
		t.Errorf("%d instances were created after a failed Prepare", n)
	}
	d.prepareErr.Store(nil)
	e.advance(unavailable.DefaultUnreachableTTL)
	if _, cerr := e.cp.Create(t.Context(), e.newClaim("claim-prep", []string{launchType})); cerr != nil {
		t.Errorf("the retry = %v, want a launch", cerr)
	}
}

// No new candidate starts once the Create has used most of core's launch
// timeout: a create call that follows would not fit.
func TestCreateStopsStartingCandidatesWhenTheLaunchBudgetIsUsed(t *testing.T) {
	e, d := newFlakyEnv(t)
	d.prepareErr.Store(&timeout)
	d.onPrepare = func() { e.advance(3 * time.Minute) }
	_, cerr := e.cp.Create(t.Context(), e.newClaim("claim-slow", []string{launchType}))
	if cerr == nil || !strings.Contains(cerr.Error(), "tried 1 offerings") || !strings.Contains(cerr.Error(), "launchBudget") {
		t.Errorf("Create = %v, want one attempt, stopped by the launch budget", cerr)
	}
}

// A region that failed while empty is asked again at once when a NodeClaim
// appears in it, not after the retry delay.
func TestANodeClaimInAFailedEmptyRegionMakesTheNextListingAskAgain(t *testing.T) {
	e, d := newFlakyEnv(t)
	d.down.Store(&map[string]bool{region1: true})
	if _, err := e.cp.List(t.Context()); err != nil {
		t.Fatal(err) // empty region, remembered as failed
	}
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim-y"}}
	if err := e.client.Create(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	claim.Status.ProviderID = scope.ProviderID("aws", region1, "claim-y")
	if err := e.client.Status().Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	if _, err := e.cp.List(t.Context()); err == nil {
		t.Fatal("List succeeded with a NodeClaim in a region that cannot be listed")
	}
	d.down.Store(nil)
	if _, err := e.cp.List(t.Context()); err != nil {
		t.Errorf("List after the region came back = %v, want it asked again at once", err)
	}
}

// A Prepare that fails with InvalidOffering (a firewall rule the driver cannot
// translate) is marked per offering, tried on to the next one, reported as an
// Event on the NodeClass and ends in InsufficientCapacity, so core drops the
// claim. Once the offerings are marked a later Create makes no further Prepare
// call, and the listing no longer offers them.
func TestAPrepareThatCannotSucceedMarksTheOfferingsAndStopsTheRetries(t *testing.T) {
	e, d := newFlakyEnv(t)
	var prepares atomic.Int32
	d.onPrepare = func() { prepares.Add(1) }
	invalid := error(vendors.NewError(vendors.InvalidOffering, "InvalidRequest", errors.New("rule 1 uses the source")).AsClassScoped())
	d.prepareErr.Store(&invalid)
	before := e.offerings(launchType)

	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{launchType}))
	if !karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Fatalf("Create = %v, want InsufficientCapacityError", err)
	}
	if got := prepares.Load(); got < 2 {
		t.Errorf("%d Prepare calls, want the next offerings tried", got)
	}
	if after := e.offerings(launchType); len(after) >= len(before) {
		t.Errorf("%d offerings available, were %d: none was marked", len(after), len(before))
	}
	// The mark is the NodeClass's: another NodeClass still sees the offerings.
	marked := 0
	for _, o := range before {
		u := unavailableOf(o)
		u.NodeClass = className
		if e.un.IsUnavailable(u) {
			marked++
		}
		u.NodeClass = "other"
		if e.un.IsUnavailable(u) {
			t.Errorf("%+v is unavailable for another NodeClass", o)
		}
	}
	if marked == 0 {
		t.Error("no offering is marked for the NodeClass")
	}
	if !slices.Contains(e.events.reasons(), cloudprovider.EventOfferingRejected) {
		t.Errorf("events = %v, want %s", e.events.reasons(), cloudprovider.EventOfferingRejected)
	}

	// Keep creating claims until every offering is marked: then Create no longer
	// calls Prepare.
	for i := range len(before) {
		_, _ = e.cp.Create(t.Context(), e.newClaim(fmt.Sprintf("claim-more-%d", i), []string{launchType}))
	}
	if n := len(e.offerings(launchType)); n != 0 {
		t.Fatalf("%d offerings still available", n)
	}
	prepares.Store(0)
	_, err = e.cp.Create(t.Context(), e.newClaim("claim-last", []string{launchType}))
	if !karpcloudprovider.IsInsufficientCapacityError(err) || prepares.Load() != 0 {
		t.Errorf("Create = %v with %d Prepare calls, want InsufficientCapacity without any", err, prepares.Load())
	}
}
