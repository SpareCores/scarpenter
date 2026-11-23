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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

// flakyDriver fails the listing of the regions in down and the Prepare of a
// launch while prepareErr is set.
type flakyDriver struct {
	*fakedriver.Driver
	down       atomic.Pointer[map[string]bool]
	prepareErr atomic.Pointer[error]
	// prepareDown limits prepareErr to these regions (all when empty).
	prepareDown atomic.Pointer[map[string]bool]
}

func (d *flakyDriver) ListInstances(
	ctx context.Context, regions []string, tags map[vendors.TagKey]string,
) ([]*vendors.Instance, error) {
	if down := d.down.Load(); down != nil {
		for _, r := range regions {
			if (*down)[r] {
				return nil, vendors.NewError(vendors.Retryable, "CallTimeout", context.DeadlineExceeded)
			}
		}
	}
	return d.Driver.ListInstances(ctx, regions, tags)
}

func (d *flakyDriver) Prepare(ctx context.Context, req vendors.PrepareRequest) (*vendors.Prepared, error) {
	if err := d.prepareErr.Load(); err != nil {
		if down := d.prepareDown.Load(); down == nil || (*down)[req.Region] {
			return nil, *err
		}
	}
	return d.Driver.Prepare(ctx, req)
}

func newFlakyEnv(t *testing.T) (*env, *flakyDriver) {
	d := &flakyDriver{}
	e := newEnv(t, envOptions{wrap: func(f *fakedriver.Driver) vendors.Driver { d.Driver = f; return d }})
	return e, d
}

// A region that cannot be listed keeps the instances of the last listing: core's
// garbage collector deletes the registered NodeClaims missing from List, so a
// slow region must not make them look gone, nor hide the other regions.
func TestListKeepsTheInstancesOfARegionThatCannotBeListed(t *testing.T) {
	e, d := newFlakyEnv(t)
	nc, err := e.cp.Create(t.Context(), e.newClaim("claim-a", []string{launchType}))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.cp.List(t.Context()); err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v, want the instance", got, err)
	}
	d.down.Store(&map[string]bool{labelOf(nc, "topology.kubernetes.io/region"): true})
	e.advance(time.Hour) // the cache is stale: the next List lists again

	listed, err := e.cp.List(t.Context())
	if err != nil {
		t.Fatalf("List with a region down = %v, want the listing of the others", err)
	}
	if len(listed) != 1 {
		t.Errorf("List = %d NodeClaims, want the one of the last listing", len(listed))
	}
}

func TestListFailsWhenARegionCannotBeListedAndNothingIsKnownOfIt(t *testing.T) {
	e, d := newFlakyEnv(t)
	d.down.Store(&map[string]bool{region1: true})
	if _, err := e.cp.List(t.Context()); err == nil {
		t.Error("the first listing succeeded with a region down: its NodeClaims would look gone")
	}
}

// A launch whose preparing times out in every region creates nothing: the
// regions are set aside for a few minutes and Create fails with a message that
// names the error, which core answers by deleting the NodeClaim; a Create after
// the TTL launches.
func TestARetryablePrepareInEveryRegionLaunchesNothing(t *testing.T) {
	e, d := newFlakyEnv(t)
	var err error = vendors.NewError(vendors.Retryable, "CallTimeout", errors.New("DescribeVSwitches: context deadline exceeded"))
	d.prepareErr.Store(&err)
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

// A preparing call that times out sets the region aside and the same Create
// launches in the next one; the failure is an Event naming the slow action.
func TestARetryablePrepareMovesTheLaunchToAnotherRegion(t *testing.T) {
	e, d := newFlakyEnv(t)
	cheapest := e.offerings(launchType)[0]
	var err error = vendors.NewError(vendors.Retryable, "CallTimeout", errors.New("DescribeVSwitches: context deadline exceeded"))
	d.prepareErr.Store(&err)
	d.prepareDown.Store(&map[string]bool{cheapest.region: true})

	nc, cerr := e.cp.Create(t.Context(), e.newClaim("claim-move", []string{launchType}))
	if cerr != nil {
		t.Fatalf("Create = %v, want a launch in another region", cerr)
	}
	if got := labelOf(nc, corev1.LabelTopologyRegion); got == cheapest.region {
		t.Errorf("launched in the unreachable region %s", got)
	}
	if !e.un.IsUnavailable(unavailableOfRegion(cheapest)) {
		t.Error("the unreachable region is not set aside")
	}
	if !slices.Contains(e.events.reasons(), cloudprovider.EventLaunchFailed) {
		t.Errorf("events = %v, want a LaunchFailed warning", e.events.reasons())
	}
	e.advance(unavailable.DefaultUnreachableTTL)
	if e.un.IsUnavailable(unavailableOfRegion(cheapest)) {
		t.Error("the region stays set aside after the TTL")
	}
}
