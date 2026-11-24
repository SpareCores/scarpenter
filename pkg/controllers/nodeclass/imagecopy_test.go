/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// newGatedEnv is newEnv with a gate that records the gaps.
func newGatedEnv(t *testing.T) (*env, *gateRecorder) {
	t.Helper()
	e := newEnv(t)
	gate := &gateRecorder{}
	e.ctrl = e.controllerWith(func(o *nodeclass.Options) { o.ImageGate = gate })
	return e, gate
}

func copyStatus(arch, reason string, ready bool) v1alpha1.ImageStatus {
	st := v1alpha1.ImageStatus{Region: "r2", Arch: arch, Reason: reason, Ready: ready, Message: "the copy of " + arch}
	if ready {
		st.Ref, st.Reason, st.Message = "ami-copy-"+arch, "", ""
	}
	return st
}

func (g *gateRecorder) gapsOf(class string) map[string]catalog.Code {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gaps[class+"/aws"]
}

// The reasons of plan 29, section 5, one by one: what ImagesReady says and what
// the gate is told, for a region the NodeClass does not name (no region list).
func TestImageCopyReasonsOfAnUnnamedRegion(t *testing.T) {
	ready := []v1alpha1.ImageStatus{
		{Region: "r1", Arch: "amd64", Ref: "ami-1", Ready: true}, {Region: "r1", Arch: "arm64", Ref: "ami-2", Ready: true},
	}
	for name, tc := range map[string]struct {
		r2     []v1alpha1.ImageStatus
		status metav1.ConditionStatus
		reason string
		gap    catalog.Code
	}{
		"on demand": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", vendors.CodeImageCopyOnDemand, false), copyStatus("arm64", vendors.CodeImageCopyOnDemand, false)},
			metav1.ConditionTrue, nodeclass.ReasonImagesPartial, "",
		},
		"copying": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", vendors.CodeImageCopying, false), copyStatus("arm64", vendors.CodeImageCopying, false)},
			metav1.ConditionTrue, nodeclass.ReasonImagesCopying, catalog.ImageCopyPending,
		},
		"one architecture copied": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", "", true), copyStatus("arm64", vendors.CodeImageCopying, false)},
			metav1.ConditionTrue, nodeclass.ReasonImagesCopying, "",
		},
		"failed": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", vendors.CodeImageCopyFailed, false), copyStatus("arm64", vendors.CodeImageCopyFailed, false)},
			metav1.ConditionTrue, nodeclass.ReasonImagesPartial, catalog.ImageGap,
		},
		"source missing": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", vendors.CodeImageCopySourceMissing, false), copyStatus("arm64", vendors.CodeImageCopySourceMissing, false)},
			metav1.ConditionTrue, nodeclass.ReasonImagesPartial, catalog.ImageGap,
		},
		"available": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", "", true), copyStatus("arm64", "", true)},
			metav1.ConditionTrue, nodeclass.ReasonReady, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, gate := newGatedEnv(t)
			e.drv.images = append(slices.Clone(ready), tc.r2...)
			e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = nil }))
			c := vcond(e.reconcile(e.name), "aws", v1alpha1.VendorConditionImagesReady)
			if c == nil || c.Status != tc.status || c.Reason != tc.reason {
				t.Errorf("ImagesReady = %+v, want %s %s", c, tc.status, tc.reason)
			}
			gaps := gate.gapsOf(e.name)
			if tc.gap == "" && len(gaps) != 0 || tc.gap != "" && (len(gaps) != 1 || gaps["r2"] != tc.gap) {
				t.Errorf("gaps = %v, want %q in r2", gaps, tc.gap)
			}
		})
	}
}

// A copy that is pending is not a fault, not even in a region the NodeClass
// names; a failed one is (as a missing image is), and the others stay True.
func TestImageCopyReasonsOfANamedRegion(t *testing.T) {
	for name, tc := range map[string]struct {
		r2     []v1alpha1.ImageStatus
		status metav1.ConditionStatus
		reason string
	}{
		"on demand": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", vendors.CodeImageCopyOnDemand, false), copyStatus("arm64", vendors.CodeImageCopyOnDemand, false)},
			metav1.ConditionTrue, nodeclass.ReasonImagesPartial,
		},
		"copying": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", vendors.CodeImageCopying, false), copyStatus("arm64", vendors.CodeImageCopying, false)},
			metav1.ConditionTrue, nodeclass.ReasonImagesCopying,
		},
		"failed": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", vendors.CodeImageCopyFailed, false), copyStatus("arm64", vendors.CodeImageCopyFailed, false)},
			metav1.ConditionFalse, vendors.CodeImageCopyFailed,
		},
		"source missing": {
			[]v1alpha1.ImageStatus{copyStatus("amd64", vendors.CodeImageCopySourceMissing, false), copyStatus("arm64", vendors.CodeImageCopySourceMissing, false)},
			metav1.ConditionFalse, vendors.CodeImageCopySourceMissing,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := newGatedEnv(t)
			e.drv.images = tc.r2
			e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"r2"} }))
			nc := e.reconcile(e.name)
			c := vcond(nc, "aws", v1alpha1.VendorConditionImagesReady)
			if c == nil || c.Status != tc.status || c.Reason != tc.reason {
				t.Errorf("ImagesReady = %+v, want %s %s", c, tc.status, tc.reason)
			}
			if c := vcond(nc, "aws", v1alpha1.VendorConditionReady); c == nil || c.Status != metav1.ConditionTrue {
				t.Errorf("the vendor must stay ready: %+v", c)
			}
		})
	}
}

// The Events of a copy: Normal while copying, Warning when it failed, one per
// architecture and outcome however often the NodeClass is reconciled.
func TestImageCopyEventsAreDeduplicated(t *testing.T) {
	e, _ := newGatedEnv(t)
	e.drv.images = []v1alpha1.ImageStatus{
		copyStatus("amd64", vendors.CodeImageCopying, false), copyStatus("arm64", vendors.CodeImageCopyFailed, false),
	}
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"r2"} }))
	e.reconcile(e.name)
	e.reconcile(e.name)
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	got := map[string]string{}
	for _, ev := range e.rec.list {
		if ev.Reason != nodeclass.EventImageCopying && ev.Reason != nodeclass.EventImageCopyFailed {
			continue
		}
		got[ev.Reason] = ev.Type
		if len(ev.DedupeValues) == 0 || ev.DedupeTimeout < 30*time.Minute || !strings.Contains(ev.Message, "the copy of") {
			t.Errorf("an Event per copy and architecture, not per reconcile: %+v", ev)
		}
	}
	if got[nodeclass.EventImageCopying] != corev1.EventTypeNormal || got[nodeclass.EventImageCopyFailed] != corev1.EventTypeWarning {
		t.Errorf("events: %v", got)
	}
}

// A copy in flight in one named region does not hide the failure of another: the
// reason is the failed copy's, never ImagesCopying; and a class without a region
// list whose regions all wait for a copy on demand is not False.
func TestAPendingCopyIsNeverTheReasonOfAFalseImagesReady(t *testing.T) {
	t.Run("named", func(t *testing.T) {
		e, _ := newGatedEnv(t)
		e.drv.images = []v1alpha1.ImageStatus{
			copyStatus("amd64", vendors.CodeImageCopying, false), copyStatus("arm64", vendors.CodeImageCopying, false),
			{Region: "r3", Arch: "amd64", Reason: vendors.CodeImageCopyFailed, Message: "the copy failed"},
			{Region: "r3", Arch: "arm64", Reason: vendors.CodeImageCopyFailed, Message: "the copy failed"},
		}
		e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"r2", "r3"} }))
		c := vcond(e.reconcile(e.name), "aws", v1alpha1.VendorConditionImagesReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != vendors.CodeImageCopyFailed ||
			strings.Contains(c.Message, "r2") {
			t.Errorf("ImagesReady = %+v, want False with ImageCopyFailed naming r3 only", c)
		}
	})
	t.Run("on demand only", func(t *testing.T) {
		e, _ := newGatedEnv(t)
		e.drv.images = []v1alpha1.ImageStatus{
			copyStatus("amd64", vendors.CodeImageCopyOnDemand, false), copyStatus("arm64", vendors.CodeImageCopyOnDemand, false),
		}
		e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = nil }))
		if c := vcond(e.reconcile(e.name), "aws", v1alpha1.VendorConditionImagesReady); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("ImagesReady = %+v, want True", c)
		}
	})
}
