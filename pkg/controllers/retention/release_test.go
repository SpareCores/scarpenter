/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention_test

import (
	"testing"
	"time"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"

	v1alpha1 "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/retention"
)

func nodes(n string) []karpv1.Budget { return []karpv1.Budget{{Nodes: n}} }

// A budget of 0 for Empty holds the node into the next period (one more is paid)
// and the node goes at the next deadline once the budget opens.
func TestBudgetOfZeroHoldsIntoTheNextPeriod(t *testing.T) {
	h := newHarness(t, enabled())
	h.replacePool(poolOpts{budgets: []karpv1.Budget{{Nodes: "0", Reasons: []karpv1.DisruptionReason{karpv1.DisruptionReasonEmpty}}}})
	w := h.addNode(nodeOpts{})
	w.reconcile()

	h.advanceTo(57 * time.Minute)
	res := w.reconcile()
	if w.deleted() || w.tainted() {
		t.Fatal("a budget of 0 forbids the delete, and nothing is marked or tainted")
	}
	if !w.held() {
		t.Error("the node stays held into the next period")
	}
	if h.events.count(retention.EventReleaseBlockedByBudget, w.name) != 1 {
		t.Error("expected ReleaseBlockedByBudget once")
	}
	if got := h.metric("scarpenter_billing_blocked_by_budget_total", "vendor", vHourly); got != 1 {
		t.Errorf("blocked_by_budget_total = %v", got)
	}
	if want := 3*time.Minute + time.Second; res.RequeueAfter != want {
		t.Errorf("requeue %s, want the boundary (%s)", res.RequeueAfter, want)
	}

	// The next period: held with the next deadline.
	h.advanceTo(hour + time.Second)
	w.reconcile()
	if got := w.node().Annotations[v1alpha1.AnnotationRetainUntil]; got != h.t0.Add(2*hour-3*time.Minute).Format(time.RFC3339) {
		t.Errorf("retain-until %s, want the second deadline", got)
	}
	// The window is open again; the node goes at the second deadline.
	h.replacePool(poolOpts{budgets: nodes("100%")})
	h.advanceTo(2*hour - 3*time.Minute)
	w.reconcile()
	h.clk.Step(6 * time.Second)
	w.reconcile()
	if !w.deleted() {
		t.Error("deleted at the next deadline once the budget opened")
	}
	if got := h.metric("scarpenter_billing_boundary_missed_total", "vendor", vHourly); got != 0 {
		t.Errorf("a budget block is not a missed boundary, got %v", got)
	}
}

// A budget of 1 with two empty nodes and a mark by Karpenter in the same tick:
// exactly one delete in all.
func TestBudgetOfOneAllowsExactlyOneDelete(t *testing.T) {
	t.Run("two of ours", func(t *testing.T) {
		h := newHarness(t, enabled())
		h.replacePool(poolOpts{budgets: nodes("1")})
		a, b := h.addNode(nodeOpts{}), h.addNode(nodeOpts{})
		a.reconcile()
		b.reconcile()
		h.advanceTo(57 * time.Minute)
		a.reconcile()
		b.reconcile()
		if !a.tainted() || b.tainted() {
			t.Errorf("one release at a time: a=%v b=%v", a.tainted(), b.tainted())
		}
		h.clk.Step(6 * time.Second)
		a.reconcile()
		b.reconcile()
		if !a.deleted() || b.deleted() {
			t.Errorf("exactly one delete: a=%v b=%v", a.deleted(), b.deleted())
		}
	})
	t.Run("one of Karpenter's", func(t *testing.T) {
		h := newHarness(t, enabled())
		h.replacePool(poolOpts{budgets: nodes("1")})
		a, b := h.addNode(nodeOpts{}), h.addNode(nodeOpts{})
		a.reconcile()
		b.reconcile()
		h.advanceTo(57 * time.Minute)
		h.feed()
		h.cluster.MarkForDeletion(b.pid) // Karpenter's consolidation took the slot
		a.reconcile()
		if a.tainted() || a.deleted() {
			t.Error("Karpenter's mark takes the budget: our delete must wait")
		}
		if h.events.count(retention.EventReleaseBlockedByBudget, a.name) != 1 {
			t.Error("expected ReleaseBlockedByBudget")
		}
	})
	t.Run("a node Karpenter has tainted as disrupted counts", func(t *testing.T) {
		h := newHarness(t, enabled())
		h.replacePool(poolOpts{budgets: nodes("1")})
		a, b := h.addNode(nodeOpts{}), h.addNode(nodeOpts{})
		a.reconcile()
		b.reconcile()
		n := b.node()
		n.Spec.Taints = append(n.Spec.Taints, karpv1.DisruptedNoScheduleTaint)
		h.must(h.c.Update(t.Context(), n))
		h.advanceTo(57 * time.Minute)
		a.reconcile()
		if a.tainted() {
			t.Error("a disrupting node (taint before the mark) takes the budget")
		}
	})
}

// racyCluster marks another node in the middle of a release, as Karpenter does
// between our budget check and our mark.
type racyCluster struct {
	*state.Cluster
	other  string
	marked bool
}

func (r *racyCluster) MarkForDeletion(ids ...string) {
	if !r.marked {
		r.marked = true
		r.Cluster.MarkForDeletion(r.other)
	}
	r.Cluster.MarkForDeletion(ids...)
}

// The race with Karpenter's own disruption (its 15 s re-validation) can exceed
// the budget by one node: accepted, and counted.
func TestBudgetOvershootIsCounted(t *testing.T) {
	h := newHarness(t, enabled())
	h.replacePool(poolOpts{budgets: nodes("1")})
	a, b := h.addNode(nodeOpts{}), h.addNode(nodeOpts{})
	a.reconcile()
	b.reconcile()
	h.advanceTo(57 * time.Minute)
	h.feed()
	h.newControllerWith(&racyCluster{Cluster: h.cluster, other: b.pid})
	a.reconcile()
	if !a.tainted() {
		t.Fatal("the check passed before Karpenter's mark: the release goes on")
	}
	if got := h.metric("scarpenter_billing_budget_overshoot_total", "vendor", vHourly); got != 1 {
		t.Errorf("budget_overshoot_total = %v, want 1", got)
	}
}

// An empty node that is not Initialized is treated as busy and never deleted by
// SCarpenter.
func TestUninitializedNodeIsNeverDeleted(t *testing.T) {
	h := newHarness(t, enabled())
	no := false
	w := h.addNode(nodeOpts{initial: &no})
	w.reconcile()
	h.advanceTo(58 * time.Minute)
	w.reconcile()
	if w.deleted() || w.tainted() {
		t.Error("a node that is not Initialized may be waiting for pods that have not bound yet")
	}
	if w.held() {
		t.Error("it is treated as busy: the hold is released for the window")
	}
}

// A nominated node is busy: not deleted until the nomination has lapsed.
func TestNominatedNodeIsNotDeletedUntilTheWindowLapses(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	w.reconcile()
	h.advanceTo(57 * time.Minute)
	h.feed()
	h.cluster.NominateNodeForPod(h.ctx(), w.pid)
	res := w.reconcile()
	if w.tainted() || w.deleted() {
		t.Fatal("a nominated node is about to get a pod")
	}
	if res.RequeueAfter != 20*time.Second {
		t.Errorf("requeue %s, want 20s", res.RequeueAfter)
	}
	if !w.held() {
		t.Error("the hold stays while the node is nominated")
	}
	h.clk.Step(25 * time.Second)
	w.reconcile()
	if !w.tainted() {
		t.Error("after the nomination lapsed the release starts")
	}
	h.clk.Step(6 * time.Second)
	w.reconcile()
	if !w.deleted() {
		t.Error("and completes")
	}
}

// A nomination that arrives during the five seconds also stops the delete.
func TestNominationDuringTheReleaseStopsIt(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	w.reconcile()
	h.advanceTo(57 * time.Minute)
	w.reconcile()
	if !w.tainted() {
		t.Fatal("not started")
	}
	h.cluster.NominateNodeForPod(h.ctx(), w.pid)
	h.clk.Step(6 * time.Second)
	w.reconcile()
	if w.deleted() || w.tainted() {
		t.Error("a nomination in the window undoes the release")
	}
}

// Daemon set pods and finished pods do not make a node busy.
func TestDaemonSetPodsDoNotMakeANodeBusy(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	w.addPod(true)
	w.reconcile()
	h.advanceTo(57 * time.Minute)
	w.reconcile()
	h.clk.Step(6 * time.Second)
	w.reconcile()
	if !w.deleted() {
		t.Error("only a daemon set pod: the node is empty")
	}
}

// Minimum-only: one boundary, start + minimum, and the hold is on from registration.
func TestMinimumOnlyRule(t *testing.T) {
	t.Run("an empty node goes at start + minimum - margin", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{vendor: vMinimum})
		res := w.reconcile()
		if !w.held() {
			t.Fatal("not held")
		}
		if want := 7*time.Minute + time.Second; res.RequeueAfter != want {
			t.Errorf("requeue %s, want 7 minutes (a 600 s minimum less the 3 minute margin)", res.RequeueAfter)
		}
		h.advanceTo(6 * time.Minute)
		w.reconcile()
		if w.tainted() {
			t.Fatal("too early")
		}
		h.advanceTo(7 * time.Minute)
		w.reconcile()
		h.clk.Step(6 * time.Second)
		w.reconcile()
		if !w.deleted() {
			t.Error("deleted before the minimum is over")
		}
	})
	t.Run("a busy node loses its hold for good", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{vendor: vMinimum})
		w.addPod(false)
		w.reconcile()
		h.advanceTo(7 * time.Minute)
		res := w.reconcile()
		if w.held() || w.deleted() {
			t.Error("busy at the deadline")
		}
		if res.RequeueAfter != 0 {
			t.Errorf("there is no next period: requeue %s", res.RequeueAfter)
		}
		h.advanceTo(11 * time.Minute)
		w.reconcile()
		h.advanceTo(2 * hour)
		w.reconcile()
		if w.held() {
			t.Error("after the last boundary the node is an ordinary node")
		}
	})
	t.Run("blocked by a budget at the last boundary loses its hold for good", func(t *testing.T) {
		h := newHarness(t, enabled())
		h.replacePool(poolOpts{budgets: nodes("0")})
		w := h.addNode(nodeOpts{vendor: vMinimum})
		w.reconcile()
		h.advanceTo(7 * time.Minute)
		w.reconcile()
		if w.held() || w.deleted() || w.tainted() {
			t.Errorf("blocked: held=%v deleted=%v tainted=%v", w.held(), w.deleted(), w.tainted())
		}
		if h.events.count(retention.EventReleaseBlockedByBudget, w.name) != 1 {
			t.Error("expected ReleaseBlockedByBudget")
		}
	})
}

// A restart in the middle changes nothing: the state is on the objects.
func TestRestartInTheMiddle(t *testing.T) {
	t.Run("while held", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{})
		w.reconcile()
		before := w.node().Annotations[v1alpha1.AnnotationRetainUntil]
		h.newController()
		w.reconcile()
		if w.node().Annotations[v1alpha1.AnnotationRetainUntil] != before || !w.held() {
			t.Error("a new controller finds the node as it left it")
		}
		if h.events.count(retention.EventRetainedUntil, w.name) != 1 {
			t.Error("and does not announce it again")
		}
	})
	t.Run("during the release sequence", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{})
		w.reconcile()
		h.advanceTo(57 * time.Minute)
		w.reconcile()
		if !w.tainted() {
			t.Fatal("not started")
		}
		// The mark in cluster state is lost with the process.
		h.cluster = state.NewCluster(h.clk, h.c, nil)
		h.newController()
		h.clk.Step(6 * time.Second)
		w.reconcile()
		if !w.deleted() {
			t.Error("the new controller finishes the release from the taint alone")
		}
		if h.events.count(retention.EventReleasedAtPeriodEnd, w.name) != 1 {
			t.Error("expected ReleasedAtPeriodEnd")
		}
		// The claim is being deleted (Karpenter's termination has the node): the
		// taint is not taken back.
		w.reconcile()
		if !w.tainted() {
			t.Error("the releasing taint stays until the node is gone")
		}
	})
}

// An empty node alive 30 s past its boundary means the margin is too small.
func TestBoundaryMissedIsCounted(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	w.reconcile() // held, deadline 57m
	// The controller was away through the deadline.
	h.advanceTo(hour + 40*time.Second)
	w.reconcile()
	if got := h.metric("scarpenter_billing_boundary_missed_total", "vendor", vHourly); got != 1 {
		t.Errorf("boundary_missed_total = %v, want 1", got)
	}
	w.reconcile()
	if got := h.metric("scarpenter_billing_boundary_missed_total", "vendor", vHourly); got != 1 {
		t.Errorf("it is counted once, got %v", got)
	}
}

func TestBootSecondsIsObserved(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	h.advanceTo(100 * time.Second)
	w.reconcile()
	w.reconcile()
	if got := h.metric("scarpenter_billing_boot_seconds", "vendor", vHourly); got != 1 {
		t.Errorf("boot_seconds samples = %v, want one per node", got)
	}
}
