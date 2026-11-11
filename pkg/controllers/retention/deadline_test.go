/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention_test

import (
	"testing"
	"time"

	v1alpha1 "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/retention"
)

func enabled() *v1alpha1.BillingSpec { return &v1alpha1.BillingSpec{RetainUntilPeriodEnd: true} }

// The hold goes on at registration, comes off in the window before the
// boundary for a busy node, and goes on again for the next period.
func TestHoldAnnotationOnOffOn(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	pod := w.addPod(false)

	res := w.reconcile()
	if !w.held() {
		t.Fatal("a registered node of an hourly vendor must be held at once")
	}
	if want := 57*time.Minute + time.Second; res.RequeueAfter != want {
		t.Errorf("requeue %s, want the deadline (%s)", res.RequeueAfter, want)
	}
	if got := w.node().Annotations[v1alpha1.AnnotationRetainUntil]; got != h.t0.Add(57*time.Minute).Format(time.RFC3339) {
		t.Errorf("retain-until = %s", got)
	}
	if h.events.count(retention.EventRetainedUntil, w.name) != 1 {
		t.Error("expected the RetainedUntil event once")
	}
	// Idempotent: a second reconcile changes nothing.
	w.reconcile()
	if h.events.count(retention.EventRetainedUntil, w.name) != 1 {
		t.Error("a repeated reconcile must not repeat the event")
	}

	// At the deadline the node is busy: the hold is released for the window.
	h.advanceTo(57 * time.Minute)
	res = w.reconcile()
	if w.held() || w.node().Annotations[v1alpha1.AnnotationRetainUntil] != "" {
		t.Error("a busy node loses its hold at the deadline")
	}
	if w.deleted() || w.tainted() {
		t.Error("a busy node is never deleted")
	}
	if want := 3*time.Minute + time.Second; res.RequeueAfter != want {
		t.Errorf("requeue %s, want the boundary (%s)", res.RequeueAfter, want)
	}

	// After the boundary the next period starts and the node is held again.
	h.advanceTo(hour + time.Second)
	res = w.reconcile()
	if !w.held() {
		t.Error("the hold must be back for the next period")
	}
	if got := w.node().Annotations[v1alpha1.AnnotationRetainUntil]; got != h.t0.Add(2*hour-3*time.Minute).Format(time.RFC3339) {
		t.Errorf("retain-until = %s, want the next deadline", got)
	}
	_ = pod
	_ = res
}

// An empty node is deleted at the deadline, after the release sequence, when
// the budget allows it.
func TestEmptyNodeIsDeletedAtTheDeadline(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	w.reconcile()
	if !w.held() {
		t.Fatal("not held")
	}

	h.advanceTo(57 * time.Minute)
	res := w.reconcile()
	if !w.tainted() || w.deleted() {
		t.Fatalf("the sequence marks and taints first: tainted=%v deleted=%v", w.tainted(), w.deleted())
	}
	if res.RequeueAfter < 5*time.Second {
		t.Errorf("requeue %s: the sequence waits Karpenter's 5 s drain time", res.RequeueAfter)
	}
	// Too early: the pods are not read again before 5 seconds passed.
	h.clk.Step(2 * time.Second)
	w.reconcile()
	if w.deleted() {
		t.Fatal("deleted before the 5 second wait")
	}
	h.clk.Step(4 * time.Second)
	w.reconcile()
	if !w.deleted() {
		t.Fatal("the empty node must be deleted after the wait")
	}
	if h.events.count(retention.EventReleasedAtPeriodEnd, w.name) != 1 {
		t.Error("expected ReleasedAtPeriodEnd once")
	}
	if got := h.metric("scarpenter_billing_deletes_at_deadline_total", "vendor", vHourly); got != 1 {
		t.Errorf("deletes_at_deadline = %v", got)
	}
}

// A pod that arrives while the node is being released keeps the node: the mark
// and the taint are undone and the metrics count the reuse.
func TestPodArrivingDuringTheReleaseKeepsTheNode(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	w.reconcile()
	h.advanceTo(10 * time.Minute)
	w.reconcile() // held, empty: seen empty under the hold

	h.advanceTo(57 * time.Minute)
	w.reconcile()
	if !w.tainted() {
		t.Fatal("release not started")
	}
	w.addPod(false)
	h.clk.Step(6 * time.Second)
	w.reconcile()
	if w.deleted() || w.tainted() {
		t.Fatalf("the node has a pod now: deleted=%v tainted=%v", w.deleted(), w.tainted())
	}
	if h.cluster.IsNodeNominated(w.pid) {
		t.Error("nominated?")
	}
	if got := h.metric("scarpenter_billing_reuse_total", "vendor", vHourly); got != 1 {
		t.Errorf("reuse_total = %v, want 1", got)
	}
	if got := h.metric("scarpenter_billing_periods_saved_estimate_total", "vendor", vHourly); got != 1 {
		t.Errorf("periods_saved_estimate_total = %v, want 1", got)
	}
}
