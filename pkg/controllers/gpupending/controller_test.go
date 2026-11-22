/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gpupending_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/gpupending"
	"github.com/SpareCores/scarpenter/pkg/scope"
)

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func TestReconcile(t *testing.T) {
	pending := corev1.Taint{Key: v1alpha1.TaintGPUPending, Effect: corev1.TaintEffectNoSchedule}
	other := corev1.Taint{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule}
	driver := map[string]string{v1alpha1.LabelNVIDIADriverBranch: "570"}
	ready := map[string]string{v1alpha1.LabelNVIDIADriverBranch: "570", v1alpha1.LabelGPUReady: "true"}
	for name, tc := range map[string]struct {
		labels map[string]string
		taints []corev1.Taint
		want   []corev1.Taint
	}{
		"ready: the taint goes, others stay": {ready, []corev1.Taint{other, pending}, []corev1.Taint{other}},
		"GPU node not labelled: stays":       {driver, []corev1.Taint{pending}, []corev1.Taint{pending}},
		"label is not true: the taint stays": {map[string]string{v1alpha1.LabelNVIDIADriverBranch: "570", v1alpha1.LabelGPUReady: "false"}, []corev1.Taint{pending}, []corev1.Taint{pending}},
		"ready without the taint: unchanged": {ready, []corev1.Taint{other}, []corev1.Taint{other}},
		"CPU node: cleared without a label":  {nil, []corev1.Taint{other, pending}, []corev1.Taint{other}},
	} {
		t.Run(name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: tc.labels, CreationTimestamp: metav1.NewTime(now)},
				Spec:       corev1.NodeSpec{Taints: tc.taints, ProviderID: scope.ProviderIDPrefix + "aws/us-east-1/n1"},
			}
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(node).Build()
			ctl := gpupending.New(c, nil, clocktesting.NewFakeClock(now))
			for range 2 { // idempotent
				if _, err := ctl.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
					t.Fatal(err)
				}
			}
			var got corev1.Node
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(node), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Spec.Taints) != len(tc.want) {
				t.Fatalf("taints = %v, want %v", got.Spec.Taints, tc.want)
			}
			for i := range tc.want {
				if !got.Spec.Taints[i].MatchTaint(&tc.want[i]) {
					t.Errorf("taints = %v, want %v", got.Spec.Taints, tc.want)
				}
			}
		})
	}
}

// A node that is not SCarpenter's is left alone, labelled or not.
func TestForeignNodeIsLeftAlone(t *testing.T) {
	pending := corev1.Taint{Key: v1alpha1.TaintGPUPending, Effect: corev1.TaintEffectNoSchedule}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       corev1.NodeSpec{Taints: []corev1.Taint{pending}, ProviderID: "aws:///us-east-1a/i-1"},
	}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(node).Build()
	if _, err := gpupending.New(c, nil, clocktesting.NewFakeClock(now)).Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
		t.Fatal(err)
	}
	var got corev1.Node
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(node), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Taints) != 1 {
		t.Errorf("taints = %v, want the taint kept", got.Spec.Taints)
	}
}

func TestMissingNode(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	if _, err := gpupending.New(c, nil, clocktesting.NewFakeClock(now)).Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "gone"}}); err != nil {
		t.Fatal(err)
	}
}

// A GPU node whose check failed, or that still has the taint after the timeout, loses its
// NodeClaim (and only its own); a node that is merely young is looked at again later.
func TestFailedGPUNodeLosesItsNodeClaim(t *testing.T) {
	pending := corev1.Taint{Key: v1alpha1.TaintGPUPending, Effect: corev1.TaintEffectNoSchedule}
	for name, tc := range map[string]struct {
		ready   string // the gpu-ready label; empty: not set
		age     time.Duration
		deleted bool
		requeue time.Duration
	}{
		"check failed, young node": {"false", time.Minute, true, 0},
		"no result before timeout": {"", 10 * time.Minute, false, gpupending.Timeout - 10*time.Minute},
		"no result after timeout":  {"", gpupending.Timeout, true, 0},
		"check passed, old node":   {"true", 2 * gpupending.Timeout, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			labels := map[string]string{v1alpha1.LabelNVIDIADriverBranch: "570"}
			if tc.ready != "" {
				labels[v1alpha1.LabelGPUReady] = tc.ready
			}
			providerID := scope.ProviderIDPrefix + "aws/us-east-1/n1"
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: labels, CreationTimestamp: metav1.NewTime(now.Add(-tc.age))},
				Spec:       corev1.NodeSpec{Taints: []corev1.Taint{pending}, ProviderID: providerID},
			}
			mine := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "mine"}, Status: karpv1.NodeClaimStatus{ProviderID: providerID}}
			other := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "other"},
				Status: karpv1.NodeClaimStatus{ProviderID: scope.ProviderIDPrefix + "aws/us-east-1/n2"}}
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(node, mine, other).Build()
			res, err := gpupending.New(c, nil, clocktesting.NewFakeClock(now)).Reconcile(t.Context(),
				reconcile.Request{NamespacedName: types.NamespacedName{Name: "n1"}})
			if err != nil {
				t.Fatal(err)
			}
			if res.RequeueAfter != tc.requeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.requeue)
			}
			err = c.Get(t.Context(), client.ObjectKeyFromObject(mine), &karpv1.NodeClaim{})
			if gone := apierrors.IsNotFound(err); gone != tc.deleted {
				t.Errorf("NodeClaim deleted = %v, want %v", gone, tc.deleted)
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(other), &karpv1.NodeClaim{}); err != nil {
				t.Errorf("another node's NodeClaim: %v", err)
			}
		})
	}
}
