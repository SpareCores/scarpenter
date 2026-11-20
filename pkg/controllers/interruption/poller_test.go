//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package interruption_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/interruption"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const pollEvery = 10 * time.Second

// pollDriver is a driver that can only poll: the embedded Driver is nil, the
// poller never calls anything else.
type pollDriver struct {
	vendors.Driver

	mu      sync.Mutex
	notices []vendors.Notice
	err     error
	polls   map[string]int
}

func (d *pollDriver) Vendor() string { return vendor }

func (d *pollDriver) PollInterval() time.Duration { return pollEvery }

func (d *pollDriver) PollInterruptions(_ context.Context, region string) ([]vendors.Notice, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.polls == nil {
		d.polls = map[string]int{}
	}
	d.polls[region]++
	return d.notices, d.err
}

func (d *pollDriver) set(notices []vendors.Notice, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notices, d.err = notices, err
}

func (d *pollDriver) count(region string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.polls[region]
}

func (h *harness) pollerWith(d vendors.Driver) *interruption.Poller {
	h.t.Helper()
	h.reg = prometheus.NewRegistry()
	h.ctrl = interruption.New(interruption.Options{
		Client: h.c, Registry: vendors.NewRegistryFrom(d), Unavailable: h.un, Clock: h.clk, Recorder: h.rec,
		Margin: margin, Registerer: h.reg,
	})
	p := h.ctrl.Poller(interruption.PollerOptions{MaxBackoff: time.Minute})
	h.t.Cleanup(func() {
		// Cancel the loops; Start would wait for them.
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		_ = p.Sync(cancelled)
	})
	return p
}

// tick waits until a poll loop sleeps on the fake clock, then lets d pass.
func (h *harness) tick(d time.Duration) {
	h.t.Helper()
	eventually(h.t, "a poll loop sleeps", func() bool { return h.clk.HasWaiters() })
	h.clk.Step(d)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) refOf(id string) vendors.InstanceRef {
	return vendors.NewInstanceRef("westeurope", id)
}

func (h *harness) addPolled(ref vendors.InstanceRef, o nodeOpts) {
	h.t.Helper()
	o.claimAnn = map[string]string{v1alpha1.AnnotationInstanceRef: ref.String()}
	h.add(o)
}

func TestPollerReportsANoticeThroughTheNode(t *testing.T) {
	h := newHarness(t)
	d := &pollDriver{}
	p := h.pollerWith(d)
	ref := h.refOf("i-" + h.name)
	h.addPolled(ref, nodeOpts{})
	d.set([]vendors.Notice{
		{Ref: ref, Reason: v1alpha1.ReasonSpotInterrupted, Deadline: h.clk.Now().Add(30 * time.Second)},
		{Ref: h.refOf("i-someone-elses"), Reason: v1alpha1.ReasonSpotInterrupted},
	}, nil)

	h.must(p.Sync(t.Context()))
	eventually(t, "the Node has the notice", func() bool {
		return h.node().Annotations[v1alpha1.AnnotationInterruption] != ""
	})
	ann := h.node().Annotations
	if ann[v1alpha1.AnnotationInterruption] != v1alpha1.ReasonSpotInterrupted ||
		ann[v1alpha1.AnnotationInterruptionSource] != v1alpha1.SourceVendorAPI ||
		ann[v1alpha1.AnnotationInterruptionDeadline] != h.clk.Now().Add(30*time.Second).Format(time.RFC3339) {
		t.Fatalf("Node annotations %v, want the notice with source vendor-api", ann)
	}

	// The controller reacts exactly as to the node agent's annotation.
	h.reconcile()
	claim := h.claim()
	if claim.DeletionTimestamp.IsZero() || !claim.StatusConditions().Get(v1alpha1.ConditionInterrupted).IsTrue() {
		t.Errorf("the NodeClaim was not marked and deleted: %+v", claim.Status.Conditions)
	}
	if got, want := terminationOf(t, claim), h.clk.Now().Add(30*time.Second-margin); !got.Equal(want) {
		t.Errorf("termination timestamp = %s, want %s", got, want)
	}
	if got := h.metric("scarpenter_interruptions_total", "vendor", vendor, "source", v1alpha1.SourceVendorAPI); got != 1 {
		t.Errorf("scarpenter_interruptions_total with source vendor-api = %v, want 1", got)
	}
}

func TestPollerKeepsTheAgentsNotice(t *testing.T) {
	h := newHarness(t)
	d := &pollDriver{}
	p := h.pollerWith(d)
	ref := h.refOf("i-" + h.name)
	agent := h.notice(60 * time.Second)
	h.addPolled(ref, nodeOpts{annotations: agent})
	d.set([]vendors.Notice{{Ref: ref, Reason: v1alpha1.ReasonSpotInterrupted, Deadline: h.clk.Now().Add(10 * time.Second)}}, nil)

	h.must(p.Sync(t.Context()))
	eventually(t, "the first poll", func() bool { return d.count("westeurope") > 0 })
	h.tick(pollEvery)
	eventually(t, "the second poll", func() bool { return d.count("westeurope") > 1 })
	if got := h.node().Annotations; got[v1alpha1.AnnotationInterruptionDeadline] != agent[v1alpha1.AnnotationInterruptionDeadline] ||
		got[v1alpha1.AnnotationInterruptionSource] != v1alpha1.SourceInstanceMetadata {
		t.Errorf("the agent's notice was overwritten: %v", got)
	}
}

// A notice for a NodeClaim whose Node is not registered yet is kept and
// written once the Node is, even if the vendor no longer shows it.
func TestPollerKeepsANoticeUntilTheNodeIsRegistered(t *testing.T) {
	h := newHarness(t)
	d := &pollDriver{}
	p := h.pollerWith(d)
	ref := h.refOf("i-" + h.name)
	h.addPolled(ref, nodeOpts{})
	claim := h.claim()
	claim.Status.NodeName = ""
	h.must(h.c.Status().Update(t.Context(), claim))
	d.set([]vendors.Notice{{Ref: ref, Reason: v1alpha1.ReasonSpotInterrupted}}, nil)

	h.must(p.Sync(t.Context()))
	eventually(t, "the first poll", func() bool { return d.count("westeurope") > 0 })
	d.set(nil, nil)
	h.tick(pollEvery)
	eventually(t, "the second poll", func() bool { return d.count("westeurope") > 1 })
	if _, ok := h.node().Annotations[v1alpha1.AnnotationInterruption]; ok {
		t.Fatal("the poller annotated a Node its NodeClaim does not name yet")
	}

	claim = h.claim()
	claim.Status.NodeName = h.name
	h.must(h.c.Status().Update(t.Context(), claim))
	h.tick(pollEvery)
	eventually(t, "the Node has the kept notice", func() bool {
		return h.node().Annotations[v1alpha1.AnnotationInterruptionSource] == v1alpha1.SourceVendorAPI
	})
}

// A node cannot hide the vendor's notice behind a value of its own that the
// controller ignores: the poller replaces it, deadline included.
func TestPollerReplacesANoticeTheControllerWouldIgnore(t *testing.T) {
	h := newHarness(t)
	d := &pollDriver{}
	p := h.pollerWith(d)
	ref := h.refOf("i-" + h.name)
	h.addPolled(ref, nodeOpts{annotations: map[string]string{
		v1alpha1.AnnotationInterruption:         "Nothing",
		v1alpha1.AnnotationInterruptionDeadline: h.clk.Now().Add(time.Hour).Format(time.RFC3339),
	}})
	d.set([]vendors.Notice{{Ref: ref, Reason: v1alpha1.ReasonSpotInterrupted}}, nil)

	h.must(p.Sync(t.Context()))
	eventually(t, "the Node has the vendor's notice", func() bool {
		return h.node().Annotations[v1alpha1.AnnotationInterruption] == v1alpha1.ReasonSpotInterrupted
	})
	if dl, ok := h.node().Annotations[v1alpha1.AnnotationInterruptionDeadline]; ok {
		t.Errorf("the node's deadline %s was kept for a notice without one", dl)
	}
}

func TestPollerWithoutADeadlineLeavesItToTheController(t *testing.T) {
	h := newHarness(t)
	d := &pollDriver{}
	p := h.pollerWith(d)
	ref := h.refOf("i-" + h.name)
	h.addPolled(ref, nodeOpts{})
	d.set([]vendors.Notice{{Ref: ref, Reason: v1alpha1.ReasonSpotInterrupted}}, nil)

	h.must(p.Sync(t.Context()))
	eventually(t, "the Node has the notice", func() bool {
		return h.node().Annotations[v1alpha1.AnnotationInterruption] != ""
	})
	if _, ok := h.node().Annotations[v1alpha1.AnnotationInterruptionDeadline]; ok {
		t.Error("the poller wrote a deadline for a notice that has none")
	}
	h.reconcile()
	if got, want := terminationOf(t, h.claim()), h.clk.Now().Add(30*time.Second-margin); !got.Equal(want) {
		t.Errorf("termination timestamp = %s, want the vendor's shortest notice from now, %s", got, want)
	}
}

func TestPollerRunsOnePerRegionWhileSpotNodeClaimsExist(t *testing.T) {
	h := newHarness(t)
	d := &pollDriver{}
	p := h.pollerWith(d)

	h.must(p.Sync(t.Context()))
	if got := p.Running(); len(got) != 0 {
		t.Fatalf("pollers without NodeClaims: %v", got)
	}

	// An on-demand NodeClaim does not need one.
	h.add(nodeOpts{capacityType: karpv1.CapacityTypeOnDemand})
	h.must(p.Sync(t.Context()))
	if got := p.Running(); len(got) != 0 {
		t.Fatalf("pollers for an on-demand NodeClaim: %v", got)
	}

	h2 := newHarness(t)
	h2.add(nodeOpts{})
	h2.must(p.Sync(t.Context()))
	if got := p.Running(); len(got) != 1 || got[0] != vendor+"/westeurope" {
		t.Fatalf("pollers for a spot NodeClaim: %v, want %s/westeurope", got, vendor)
	}
	h3 := newHarness(t)
	h3.add(nodeOpts{})
	h3.must(p.Sync(t.Context()))
	if got := p.Running(); len(got) != 1 {
		t.Errorf("a second spot NodeClaim of the region started another poller: %v", got)
	}

	// The last spot NodeClaim goes: the poller stops.
	for _, x := range []*harness{h2, h3} {
		c := x.claim()
		c.Finalizers = nil
		x.must(x.c.Update(t.Context(), c))
		x.must(x.c.Delete(t.Context(), c))
	}
	h.must(p.Sync(t.Context()))
	if got := p.Running(); len(got) != 0 {
		t.Errorf("pollers after the last spot NodeClaim is gone: %v", got)
	}
}

func TestPollerBacksOffAndCountsErrors(t *testing.T) {
	h := newHarness(t)
	d := &pollDriver{}
	p := h.pollerWith(d)
	h.add(nodeOpts{})
	d.set(nil, vendors.NewError(vendors.Retryable, "Throttling", errors.New("slow down")))

	h.must(p.Sync(t.Context()))
	eventually(t, "the first poll", func() bool { return d.count("westeurope") == 1 })
	eventually(t, "the error is counted", func() bool {
		return h.metric("scarpenter_interruption_poll_errors_total", "vendor", vendor, "region", "westeurope") == 1
	})
	// After a failure the loop waits twice the interval, then twice that.
	h.tick(pollEvery)
	time.Sleep(50 * time.Millisecond)
	if n := d.count("westeurope"); n != 1 {
		t.Fatalf("%d polls after one interval, want the backoff to hold the second back", n)
	}
	h.tick(pollEvery)
	eventually(t, "the second poll", func() bool { return d.count("westeurope") == 2 })
	eventually(t, "the second error", func() bool {
		return h.metric("scarpenter_interruption_poll_errors_total", "vendor", vendor, "region", "westeurope") == 2
	})
	// 4 intervals next, never past the maximum (1 minute), and a success resets.
	h.tick(3 * pollEvery)
	time.Sleep(50 * time.Millisecond)
	if n := d.count("westeurope"); n != 2 {
		t.Fatalf("%d polls, want the third held back for 4 intervals", n)
	}
	d.set(nil, nil)
	h.tick(pollEvery)
	eventually(t, "the third poll", func() bool { return d.count("westeurope") == 3 })
	h.tick(pollEvery)
	eventually(t, "the next poll after a success, one interval later", func() bool { return d.count("westeurope") == 4 })
}
