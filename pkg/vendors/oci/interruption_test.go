/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

var (
	_ vendors.InterruptionPoller = (*driver.Driver)(nil)
	_ vendors.InterruptionWindow = (*driver.Driver)(nil)
)

// onDemandRequestForInterruptionTest is request with its own claim and idempotency key.
func onDemandRequestForInterruptionTest(n int) vendors.CreateRequest {
	req := request(testType)
	req.IdempotencyKey = fmt.Sprintf("uid-%d", n)
	req.Tags[vendors.TagNodeClaim] = fmt.Sprintf("claim-%d", n)
	return req
}

func spotRequestForInterruptionTest(n int) vendors.CreateRequest {
	req := onDemandRequestForInterruptionTest(n)
	req.CapacityType = vendors.CapacitySpot
	return req
}

func (e *env) notices(t *testing.T) []vendors.Notice {
	t.Helper()
	list, err := e.d.PollInterruptions(t.Context(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestNoticeWindowAndInterval(t *testing.T) {
	e := newEnv(t)
	w := vendors.NoticeWindowOf(e.d)
	if w.MaxNotice != 2*time.Minute || w.MinNotice <= 0 || w.MinNotice > w.MaxNotice {
		t.Errorf("window = %+v, want a short minimum and the documented two minutes", w)
	}
	if got := e.d.PollInterval(); got != driver.DefaultInterruptionPoll {
		t.Errorf("PollInterval = %s, want the default %s", got, driver.DefaultInterruptionPoll)
	}
	if got := newEnv(t, driver.WithInterruptionPoll(3*time.Second)).d.PollInterval(); got != 3*time.Second {
		t.Errorf("PollInterval with the option = %s, want 3s", got)
	}
}

// A preempted preemptible instance of the cluster is a notice without a
// deadline; one list call per poll whatever the number of instances.
func TestPollInterruptions(t *testing.T) {
	e := newEnv(t)
	spot := e.create(t, spotRequestForInterruptionTest(1))
	spot2 := e.create(t, spotRequestForInterruptionTest(2))
	od := e.create(t, onDemandRequestForInterruptionTest(3))
	if got := e.notices(t); len(got) != 0 {
		t.Fatalf("notices of running instances: %v", got)
	}
	before := e.c.Calls("ListInstances")
	e.c.SetState(od.Ref.ID(), core.InstanceLifecycleStateTerminating)
	if !e.c.Preempt(spot.Ref.ID()) || e.c.Preempt(od.Ref.ID()) != false {
		t.Fatal("the fake preempts the preemptible instance and not the on-demand one")
	}
	got := e.notices(t)
	if len(got) != 1 || got[0].Ref != spot.Ref || got[0].Reason != v1alpha1.ReasonSpotInterrupted || !got[0].Deadline.IsZero() {
		t.Fatalf("notices = %+v, want the preempted %s with no deadline", got, spot.Ref)
	}
	if calls := e.c.Calls("ListInstances") - before; calls != 1 {
		t.Errorf("a poll made %d ListInstances calls, want 1", calls)
	}
	if got := e.notices(t); len(got) != 1 {
		t.Errorf("the notice is listed while the instance lives: %v", got)
	}
	// The driver's own termination is not a preemption, nor is a preempted instance
	// the core deletes after it reacted.
	if err := e.d.DeleteInstance(t.Context(), spot2.Ref); err != nil {
		t.Fatal(err)
	}
	if err := e.d.DeleteInstance(t.Context(), spot.Ref); err != nil {
		t.Fatal(err)
	}
	if got := e.notices(t); len(got) != 0 {
		t.Errorf("notices after the deletes: %v", got)
	}
}

func TestPollInterruptionsOfOtherClustersAndRegions(t *testing.T) {
	c := newCloud(t)
	mine := restart(t, c, testCluster)
	other := restart(t, c, "other")
	req := spotRequestForInterruptionTest(1)
	req.Tags[vendors.TagCluster] = "other"
	foreign := other.create(t, req)
	c.Preempt(foreign.Ref.ID())
	if got := mine.notices(t); len(got) != 0 {
		t.Errorf("notices of another cluster's instance: %v", got)
	}
	if got, err := mine.d.PollInterruptions(t.Context(), "nowhere-1"); err != nil || len(got) != 0 {
		t.Errorf("an unsubscribed region: %v, %v", got, err)
	}
}

func TestPollInterruptionsErrors(t *testing.T) {
	e := newEnv(t)
	e.c.Inject("ListInstances", fake.Error(http.StatusTooManyRequests, "TooManyRequests", "slow down"), 1)
	_, err := e.d.PollInterruptions(t.Context(), testRegion)
	if !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("a throttled poll: %v, want a Retryable error", err)
	}
}

// The reason an instance is gone: preemptible only, never while it runs.
func TestGoneReason(t *testing.T) {
	e := newEnv(t)
	spot := e.create(t, spotRequestForInterruptionTest(1))
	od := e.create(t, onDemandRequestForInterruptionTest(2))
	for _, tc := range []struct {
		name  string
		inst  *vendors.Instance
		state core.InstanceLifecycleStateEnum
		want  string
	}{
		{"spot running", spot, core.InstanceLifecycleStateRunning, ""},
		{"spot terminating", spot, core.InstanceLifecycleStateTerminating, v1alpha1.ReasonSpotInterrupted},
		{"spot terminated", spot, core.InstanceLifecycleStateTerminated, v1alpha1.ReasonSpotInterrupted},
		{"on-demand terminating", od, core.InstanceLifecycleStateTerminating, ""},
		{"on-demand terminated", od, core.InstanceLifecycleStateTerminated, ""},
	} {
		e.c.SetState(tc.inst.Ref.ID(), tc.state)
		got, err := e.d.GetInstance(t.Context(), tc.inst.Ref)
		if err != nil || got.GoneReason != tc.want {
			t.Errorf("%s: GoneReason = %q (%v), want %q", tc.name, got.GoneReason, err, tc.want)
		}
	}
}
