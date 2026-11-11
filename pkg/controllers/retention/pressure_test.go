/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	v1alpha1 "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/retention"
)

// addPending creates a pod that Karpenter would provision capacity for.
func (h *harness) addPending(cpu string, selector map[string]string) *corev1.Pod {
	h.t.Helper()
	h.claimSeq++
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-pending%d", h.prefix, h.claimSeq), Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeSelector: selector,
			Containers: []corev1.Container{{Name: "c", Image: "busybox", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
			}}},
		},
	}
	h.must(h.c.Create(h.t.Context(), pod))
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
	}}
	h.must(h.c.Status().Update(h.t.Context(), pod))
	h.t.Cleanup(func() { _ = h.c.Delete(context.Background(), pod, client.GracePeriodSeconds(0)) })
	return pod
}

// heldEmpty adds an initialized node and holds it.
func (h *harness) heldEmpty(o nodeOpts) *world {
	h.t.Helper()
	w := h.addNode(o)
	w.reconcile()
	if !w.held() {
		h.t.Fatalf("node %s is not held", w.name)
	}
	return w
}

func (h *harness) pressure() {
	h.t.Helper()
	h.feed()
	h.must(h.ctrl.ReleasePressure(h.ctx()))
}

// A launch blocked by a NodePool limit releases the held empty node that has the
// least paid time left, and only as many as cover the request.
func TestLimitBlockedLaunchReleasesTheNearestDeadlineFirst(t *testing.T) {
	h := newHarness(t, enabled())
	h.replacePool(poolOpts{limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("6")}})
	// Three 2-vCPU nodes fill the limit; their deadlines differ by their start.
	late := h.heldEmpty(nodeOpts{start: 0})
	soon := h.heldEmpty(nodeOpts{start: -30 * time.Minute})
	mid := h.heldEmpty(nodeOpts{start: -10 * time.Minute})
	// A node of another pool, at its own limit, is not touched.
	other := h.addNode(nodeOpts{pool: h.prefix + "-other"})
	_ = other

	h.pressure()
	for _, w := range []*world{late, soon, mid} {
		if w.tainted() {
			t.Errorf("released %s without a pending pod", w.name)
		}
	}

	h.addPending("1", nil)
	h.pressure()
	if !soon.tainted() || mid.tainted() || late.tainted() {
		t.Errorf("only the nearest deadline goes: soon=%v mid=%v late=%v", soon.tainted(), mid.tainted(), late.tainted())
	}
	// The same tick again: the marked node no longer counts against the limit, so
	// the pod has room and nothing else is released.
	h.pressure()
	if mid.tainted() || late.tainted() {
		t.Error("only as many as cover the request")
	}
	h.clk.Step(6 * time.Second)
	soon.reconcile()
	if !soon.deleted() {
		t.Error("the release completes")
	}
	if h.events.count(retention.EventReleasedUnderPressure, soon.name) != 1 {
		t.Error("expected ReleasedUnderPressure")
	}
	if got := h.metric("scarpenter_billing_released_under_pressure_total", "vendor", vHourly, "scope", "limit"); got != 1 {
		t.Errorf("released_under_pressure_total{limit} = %v", got)
	}
}

// Ties are broken by the oldest start: the oldest-paid-up node goes first.
func TestOrderTiesByOldestStart(t *testing.T) {
	h := newHarness(t, enabled())
	h.replacePool(poolOpts{limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}})
	a := h.heldEmpty(nodeOpts{start: -time.Hour, vendor: vHourly})
	b := h.heldEmpty(nodeOpts{start: -2 * time.Hour})
	// Both are in the same phase of an hourly period (deadline 57m after t0 for
	// the first, 57m after t0 for the second: same boundary phase): the
	// deadlines are equal, the older start goes first.
	h.addPending("1", nil)
	h.pressure()
	if !b.tainted() || a.tainted() {
		t.Errorf("the oldest start first: a(older=false)=%v b(older)=%v", a.tainted(), b.tainted())
	}
}

// A pod that Karpenter has already mapped to a NodeClaim, a pod of another pool
// and a pod that fits are not blocked.
func TestPodsThatAreNotBlockedReleaseNothing(t *testing.T) {
	h := newHarness(t, enabled())
	h.replacePool(poolOpts{limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}})
	a, b := h.heldEmpty(nodeOpts{}), h.heldEmpty(nodeOpts{})
	pod := h.addPending("1", map[string]string{karpv1.NodePoolLabelKey: "another-pool"})
	h.pressure()
	if a.tainted() || b.tainted() {
		t.Error("a pod that selects another NodePool does not press this one")
	}
	h.must(h.c.Delete(t.Context(), pod))

	mapped := h.addPending("1", nil)
	h.feed()
	h.cluster.UpdatePodToNodeClaimMapping(map[string][]*corev1.Pod{"some-claim": {mapped}})
	h.pressure()
	if a.tainted() || b.tainted() {
		t.Error("a pod with a NodeClaim on its way is not blocked")
	}
}

// A vendor quota refusal reported by the cloud provider releases held empty nodes
// of its vendor and region only.
func TestQuotaRefusalReleasesNodesOfItsScope(t *testing.T) {
	h := newHarness(t, enabled())
	soon := h.heldEmpty(nodeOpts{start: -30 * time.Minute})
	later := h.heldEmpty(nodeOpts{start: 0})
	otherVendor := h.heldEmpty(nodeOpts{vendor: vMinimum, start: -time.Minute})
	_ = otherVendor

	h.addPending("1", nil)
	h.signals.ReportQuotaRefusal(vHourly, region, "on-demand", 2)
	h.signals.ReportQuotaRefusal(vHourly, "elsewhere", "on-demand", 2)
	h.pressure()
	if !soon.tainted() || later.tainted() {
		t.Errorf("nearest deadline of the scope first, and only as many as the 2 vCPUs: soon=%v later=%v", soon.tainted(), later.tainted())
	}
	if otherVendor.tainted() {
		t.Error("a node of another vendor does not free this quota")
	}
	// The refusal is answered: the next tick releases nothing more.
	h.pressure()
	if later.tainted() {
		t.Error("the signal was cleared by the release")
	}
	h.clk.Step(6 * time.Second)
	soon.reconcile()
	if got := h.metric("scarpenter_billing_released_under_pressure_total", "vendor", vHourly, "scope", "quota"); got != 1 {
		t.Errorf("released_under_pressure_total{quota} = %v", got)
	}
}

// The quota tracker's own view counts too, and a refusal that is old is forgotten.
func TestQuotaTrackerScopeReleases(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.heldEmpty(nodeOpts{})
	h.quota.scopes = []retention.QuotaScope{{Vendor: vHourly, Region: region, CapacityType: "on-demand"}}
	h.pressure()
	if w.tainted() {
		t.Error("no pending pod, no pressure")
	}
	h.addPending("1", nil)
	h.pressure()
	if !w.tainted() {
		t.Error("a pending pod and a vendor quota with no room release a held node")
	}
}

func TestOldRefusalIsForgotten(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.heldEmpty(nodeOpts{})
	h.addPending("1", nil)
	h.signals.ReportQuotaRefusal(vHourly, region, "on-demand", 2)
	h.clk.Step(10 * time.Minute)
	// Still inside the first period: held, but the refusal is stale.
	h.pressure()
	if w.tainted() {
		t.Error("a refusal older than a few minutes is not pressure")
	}
}

// A candidate that its budget blocks loses its hold (it becomes an ordinary empty
// node for Karpenter's emptiness) and the next candidate is tried.
func TestPressureReleaseBlockedByABudgetTakesTheNextCandidate(t *testing.T) {
	h := newHarness(t, enabled())
	// The nearest-deadline node is in a pool whose budget forbids the delete.
	h.replacePool(poolOpts{budgets: nodes("0")})
	pool2 := h.prefix + "-pool2"
	h.createPoolNamed(pool2, poolOpts{budgets: nodes("100%")})
	blocked := h.heldEmpty(nodeOpts{start: -30 * time.Minute})
	free := h.heldEmpty(nodeOpts{pool: pool2, start: -10 * time.Minute})

	h.addPending("1", nil)
	h.signals.ReportQuotaRefusal(vHourly, region, "on-demand", 2)
	h.pressure()
	if blocked.held() || blocked.tainted() || blocked.deleted() {
		t.Errorf("the blocked node loses its hold and is otherwise untouched: held=%v tainted=%v", blocked.held(), blocked.tainted())
	}
	if h.events.count(retention.EventReleaseBlockedByBudget, blocked.name) != 1 {
		t.Error("expected ReleaseBlockedByBudget on the blocked node")
	}
	if !free.tainted() {
		t.Error("the next candidate is released")
	}
}

// maxRetainedNodes is a safety valve, unset by default: with it set, the held
// empty nodes of the NodeClass are trimmed in deadline order.
func TestMaxRetainedNodes(t *testing.T) {
	t.Run("unset means no cap", func(t *testing.T) {
		h := newHarness(t, enabled())
		ws := make([]*world, 0, 4)
		for i := range 4 {
			ws = append(ws, h.heldEmpty(nodeOpts{start: -time.Duration(i) * 10 * time.Minute}))
		}
		h.pressure()
		for _, w := range ws {
			if w.tainted() {
				t.Errorf("%s released without a cap or pressure", w.name)
			}
		}
	})
	t.Run("set trims in deadline order", func(t *testing.T) {
		limit := int32(2)
		h := newHarness(t, &v1alpha1.BillingSpec{RetainUntilPeriodEnd: true, MaxRetainedNodes: &limit})
		a := h.heldEmpty(nodeOpts{start: -50 * time.Minute}) // deadline 7m
		b := h.heldEmpty(nodeOpts{start: -20 * time.Minute}) // 37m
		c := h.heldEmpty(nodeOpts{start: -40 * time.Minute}) // 17m
		d := h.heldEmpty(nodeOpts{start: -10 * time.Minute}) // 47m
		h.pressure()
		if !a.tainted() || !c.tainted() || b.tainted() || d.tainted() {
			t.Errorf("the two nearest deadlines go: a=%v c=%v (b=%v d=%v stay)", a.tainted(), c.tainted(), b.tainted(), d.tainted())
		}
		h.clk.Step(6 * time.Second)
		a.reconcile()
		c.reconcile()
		if got := h.metric("scarpenter_billing_released_under_pressure_total", "vendor", vHourly, "scope", "max-retained"); got != 2 {
			t.Errorf("released_under_pressure_total{max-retained} = %v", got)
		}
	})
	t.Run("busy nodes are not counted", func(t *testing.T) {
		limit := int32(1)
		h := newHarness(t, &v1alpha1.BillingSpec{RetainUntilPeriodEnd: true, MaxRetainedNodes: &limit})
		busy := h.heldEmpty(nodeOpts{start: -50 * time.Minute})
		busy.addPod(false)
		only := h.heldEmpty(nodeOpts{start: -10 * time.Minute})
		h.pressure()
		if only.tainted() || busy.tainted() {
			t.Error("one held empty node is within the cap of 1")
		}
	})
}
