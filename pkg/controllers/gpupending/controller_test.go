/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gpupending_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/gpupending"
	"github.com/SpareCores/scarpenter/pkg/scope"
)

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
				ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: tc.labels},
				Spec:       corev1.NodeSpec{Taints: tc.taints, ProviderID: scope.ProviderIDPrefix + "aws/us-east-1/n1"},
			}
			c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(node).Build()
			ctl := gpupending.New(c)
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
	if _, err := gpupending.New(c).Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "n1"}}); err != nil {
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
	if _, err := gpupending.New(c).Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "gone"}}); err != nil {
		t.Fatal(err)
	}
}
