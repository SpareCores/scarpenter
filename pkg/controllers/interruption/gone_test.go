//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package interruption_test

import (
	"testing"
	"time"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

func TestSilentReclaimOfASpotNodeClaim(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{})
	h.ctrl.InstanceGone(t.Context(), h.claim(), vendor, "")
	h.ctrl.InstanceGone(t.Context(), h.claim(), vendor, "") // core calls Delete more than once

	claim := h.claim()
	cond := claim.StatusConditions().Get(v1alpha1.ConditionInterrupted)
	if !cond.IsTrue() || cond.Reason != v1alpha1.ReasonReclaimedWithoutNotice {
		t.Errorf("Interrupted = %+v, want True with reason ReclaimedWithoutNotice", cond)
	}
	if claim.StatusConditions().Get(karpv1.ConditionTypeDisruptionReason) != nil {
		t.Error("DisruptionReason was set on a node that is already gone")
	}
	if !claim.DeletionTimestamp.IsZero() {
		t.Error("the NodeClaim was deleted here: core's garbage collection does that")
	}
	if !h.un.IsUnavailable(h.offering()) {
		t.Error("the reclaimed spot offering is still available")
	}
	if got := h.rec.count(v1alpha1.ReasonReclaimedWithoutNotice, "NodeClaim"); got != 1 {
		t.Errorf("%d ReclaimedWithoutNotice Events, want 1", got)
	}
	if got := h.metric("scarpenter_interruptions_total", "vendor", vendor, "instance_type", instanceType, "zone", zone,
		"reason", v1alpha1.ReasonReclaimedWithoutNotice, "source", v1alpha1.SourceGC); got != 1 {
		t.Errorf("scarpenter_interruptions_total = %v, want 1", got)
	}
}

func TestReclaimTheDriverTellsIsSpotInterrupted(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{})
	h.ctrl.InstanceGone(t.Context(), h.claim(), vendor, v1alpha1.ReasonSpotInterrupted)

	cond := h.claim().StatusConditions().Get(v1alpha1.ConditionInterrupted)
	if !cond.IsTrue() || cond.Reason != v1alpha1.ReasonSpotInterrupted {
		t.Errorf("Interrupted = %+v, want True with reason SpotInterrupted", cond)
	}
	if got := h.metric("scarpenter_interruptions_total", "reason", v1alpha1.ReasonSpotInterrupted, "source", v1alpha1.SourceVendorAPI); got != 1 {
		t.Errorf("scarpenter_interruptions_total with source vendor-api = %v, want 1", got)
	}
}

func TestVanishedOnDemandInstanceIsAnEventAndACount(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{capacityType: karpv1.CapacityTypeOnDemand})
	for range 2 {
		h.ctrl.InstanceGone(t.Context(), h.claim(), vendor, "")
	}
	if h.claim().StatusConditions().Get(v1alpha1.ConditionInterrupted) != nil {
		t.Error("an on-demand NodeClaim got the Interrupted condition")
	}
	if got := h.rec.count(v1alpha1.ReasonInstanceVanished, "NodeClaim"); got != 1 {
		t.Errorf("%d InstanceVanished Events, want 1", got)
	}
	if got := h.metric("scarpenter_interruptions_total", "reason", v1alpha1.ReasonInstanceVanished, "source", v1alpha1.SourceGC); got != 1 {
		t.Errorf("scarpenter_interruptions_total = %v, want 1", got)
	}
	od := h.offering()
	od.CapacityType = karpv1.CapacityTypeOnDemand
	if h.un.IsUnavailable(od) || h.un.IsUnavailable(h.offering()) {
		t.Error("a vanished on-demand instance made an offering unavailable")
	}
}

func TestGoneAfterANoticeIsNotRecordedTwice(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{annotations: h.notice(30 * time.Second)})
	h.reconcile()
	h.ctrl.InstanceGone(t.Context(), h.claim(), vendor, "")
	if got := h.metric("scarpenter_interruptions_total", "source", v1alpha1.SourceGC); got != 0 {
		t.Errorf("a noticed interruption was counted again as %v silent reclaims", got)
	}
	if got := h.claim().StatusConditions().Get(v1alpha1.ConditionInterrupted); got.Reason != v1alpha1.ReasonSpotInterrupted {
		t.Errorf("Interrupted reason = %q, want the notice's SpotInterrupted", got.Reason)
	}
}
