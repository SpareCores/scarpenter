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

	v1alpha1 "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/retention"
)

// A node is held only when everything lines up: the setting, a verified rule
// that is worth holding, on-demand capacity, a NodePool that is neither static
// nor Never, a claim that is not drifted and no do-not-disrupt of the user.
func TestNoHoldWhereRetentionDoesNotApply(t *testing.T) {
	for name, tc := range map[string]struct {
		billing *v1alpha1.BillingSpec
		node    nodeOpts
		pool    *poolOpts
		skipped string // the RetentionSkipped event this leaves, "" for none
	}{
		"setting off":              {billing: &v1alpha1.BillingSpec{}, node: nodeOpts{}},
		"no billing block":         {billing: nil, node: nodeOpts{}},
		"per second vendor":        {billing: enabled(), node: nodeOpts{vendor: vSecond}},
		"minimum under the floor":  {billing: enabled(), node: nodeOpts{vendor: vShort}},
		"unverified vendor":        {billing: enabled(), node: nodeOpts{vendor: vUnverif}},
		"vendor Keeper has no row": {billing: enabled(), node: nodeOpts{vendor: "unknown"}},
		"spot":                     {billing: enabled(), node: nodeOpts{spot: true}, skipped: retention.SkipSpot},
		"static pool":              {billing: enabled(), node: nodeOpts{}, pool: &poolOpts{static: true}, skipped: retention.SkipStaticPool},
		"consolidateAfter Never":   {billing: enabled(), node: nodeOpts{}, pool: &poolOpts{never: true}, skipped: retention.SkipConsolidationNever},
		"drifted claim":            {billing: enabled(), node: nodeOpts{drifted: true}, skipped: retention.SkipDrifted},
		"do-not-disrupt of a user": {billing: enabled(), node: nodeOpts{annotations: map[string]string{karpv1.DoNotDisruptAnnotationKey: "true"}}, skipped: retention.SkipUserDoNotDisrupt},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.billing)
			if tc.pool != nil {
				h.replacePool(*tc.pool)
			}
			w := h.addNode(tc.node)
			for range 3 {
				w.reconcile()
			}
			n := w.node()
			if n.Annotations[v1alpha1.AnnotationRetainUntil] != "" {
				t.Errorf("the node got a retain-until marker: %v", n.Annotations)
			}
			_, userDND := tc.node.annotations[karpv1.DoNotDisruptAnnotationKey]
			if _, has := n.Annotations[karpv1.DoNotDisruptAnnotationKey]; has != userDND {
				t.Errorf("do-not-disrupt present=%v, want %v (the user's is never touched, none is added)", has, userDND)
			}
			// Even at the deadline nothing is released.
			h.advanceTo(58 * time.Minute)
			w.reconcile()
			if w.deleted() || w.tainted() {
				t.Error("a node retention leaves alone is never deleted or tainted")
			}
			want := 0
			if tc.skipped != "" {
				want = 1
			}
			if got := h.events.count(retention.EventRetentionSkipped, w.name); got != want {
				t.Errorf("RetentionSkipped events = %d, want %d (once, never spammed)", got, want)
			}
		})
	}
}

// Turning the setting off releases every hold at the next reconcile, and a
// release in flight is undone.
func TestTurningItOffReleasesTheHold(t *testing.T) {
	h := newHarness(t, enabled())
	held := h.addNode(nodeOpts{})
	held.reconcile()
	if !held.held() {
		t.Fatal("not held")
	}
	releasing := h.addNode(nodeOpts{})
	releasing.reconcile()
	h.advanceTo(57 * time.Minute)
	releasing.reconcile()
	if !releasing.tainted() {
		t.Fatal("release not started")
	}

	nc := &v1alpha1.SCNodeClass{}
	h.must(h.c.Get(t.Context(), clientKey(h.class), nc))
	nc.Spec.Billing.RetainUntilPeriodEnd = false
	h.must(h.c.Update(t.Context(), nc))

	held.reconcile()
	releasing.reconcile()
	if held.held() || held.node().Annotations[v1alpha1.AnnotationRetainUntil] != "" {
		t.Error("the hold must be released")
	}
	if releasing.tainted() || releasing.deleted() {
		t.Errorf("a release in flight must be undone: tainted=%v deleted=%v", releasing.tainted(), releasing.deleted())
	}
}

// The hold is released when the NodePool turns out to be Never, and a do-not-disrupt
// that SCarpenter did not set is never removed.
func TestHoldIsReleasedWhenTheNodeStopsQualifying(t *testing.T) {
	h := newHarness(t, enabled())
	w := h.addNode(nodeOpts{})
	w.reconcile()
	if !w.held() {
		t.Fatal("not held")
	}
	h.replacePool(poolOpts{never: true})
	w.reconcile()
	if w.held() || w.node().Annotations[karpv1.DoNotDisruptAnnotationKey] != "" {
		t.Error("a Never pool loses its hold")
	}
}

// The billing start comes from the node, the claim, or once from the cloud
// provider for a claim launched before the annotation existed.
func TestBillingStartSources(t *testing.T) {
	t.Run("from the cloud provider, once", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{noStart: true})
		h.starts.by[w.pid] = h.t0.Add(-30 * time.Minute).Format(time.RFC3339)
		res := w.reconcile()
		if !w.held() {
			t.Fatal("not held with a start from the cloud provider")
		}
		if got := w.node().Annotations[v1alpha1.AnnotationRetainUntil]; got != h.t0.Add(27*time.Minute).Format(time.RFC3339) {
			t.Errorf("deadline %s, want 30 minutes before the one of a fresh node", got)
		}
		if got := w.node().Annotations[v1alpha1.AnnotationBillingStart]; got == "" {
			t.Error("the start is written to the Node")
		}
		delete(h.starts.by, w.pid) // never asked again
		w.reconcile()
		if !w.held() {
			t.Error("the start was remembered on the objects")
		}
		_ = res
	})
	t.Run("from the node only", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{noStart: true, annotations: map[string]string{
			v1alpha1.AnnotationBillingStart: h.t0.Add(-10 * time.Minute).Format(time.RFC3339),
		}})
		w.reconcile()
		if !w.held() {
			t.Error("a start on the Node is enough")
		}
	})
	t.Run("unknown: no guess, an Event, a retry", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{noStart: true})
		res := w.reconcile()
		w.reconcile()
		if w.held() {
			t.Error("a node without a billing start is never held")
		}
		if got := h.events.count(retention.EventBillingStartUnknown, w.name); got != 1 {
			t.Errorf("BillingStartUnknown events = %d, want 1", got)
		}
		if res.RequeueAfter < time.Minute {
			t.Errorf("requeue %s: the start is asked for again later", res.RequeueAfter)
		}
	})
	t.Run("in the future beyond the skew", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{start: 10 * time.Minute})
		w.reconcile()
		if w.held() {
			t.Error("a billing start 10 minutes ahead is not believed")
		}
		if got := h.events.count(retention.EventBillingStartUnknown, w.name); got != 1 {
			t.Errorf("BillingStartUnknown events = %d", got)
		}
	})
	t.Run("in the future within the skew", func(t *testing.T) {
		h := newHarness(t, enabled())
		w := h.addNode(nodeOpts{start: 30 * time.Second})
		w.reconcile()
		if !w.held() {
			t.Error("a small skew is allowed")
		}
	})
}

// The hold is on before the node is Initialized, so there is no window in which
// Karpenter at consolidateAfter 0s could pick the node the moment it empties.
func TestHoldIsPutOnBeforeInitialized(t *testing.T) {
	h := newHarness(t, enabled())
	no := false
	w := h.addNode(nodeOpts{initial: &no})
	w.reconcile()
	if !w.held() {
		t.Error("a registered node is held before it is Initialized")
	}
}
