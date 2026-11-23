//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
)

const gpuResource = catalog.ResourceNVIDIAGPU

// gpuNodePool is a NodePool that lists the gpu-pending startup taint, which the
// catalog requires before it offers NVIDIA types (ADR-031).
func gpuNodePool(t *testing.T, name, class string, reqs ...karpv1.NodeSelectorRequirementWithMinValues) {
	t.Helper()
	pending := corev1.Taint{Key: v1alpha1.TaintGPUPending, Effect: corev1.TaintEffectNoSchedule}
	createNodePoolWith(t, name, class, nil, []corev1.Taint{pending}, reqs...)
}

// markGPUReady plays the node's GPU check, which no KWOK node runs: it labels every Node
// of the pool gpu-ready=true, so that the GPU pending controller lifts the startup taint.
// It stops with the test.
func markGPUReady(t *testing.T, pool string) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			var nodes corev1.NodeList
			if err := k8s.List(ctx(), &nodes, client.MatchingLabels{labelNodePool: pool}); err == nil {
				for i := range nodes.Items {
					n := &nodes.Items[i]
					if n.Labels[v1alpha1.LabelGPUReady] == "true" {
						continue
					}
					patch := client.MergeFrom(n.DeepCopy())
					n.Labels[v1alpha1.LabelGPUReady] = "true"
					_ = k8s.Patch(ctx(), n, patch)
				}
			}
			select {
			case <-done:
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
}

// gpuPod is a pod that asks for gpus GPUs and little else, on nodes with the labels.
func gpuPod(t *testing.T, name string, gpus int64, selector map[string]string) {
	t.Helper()
	spec := podSpec(selector, "100m")
	spec.Containers[0].Resources.Requests[gpuResource] = *resource.NewQuantity(gpus, resource.DecimalSI)
	spec.Containers[0].Resources.Limits = corev1.ResourceList{gpuResource: *resource.NewQuantity(gpus, resource.DecimalSI)}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{labelOwner: ownerLabel(t)}},
		Spec:       spec,
	}
	if err := k8s.Create(ctx(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx(), p, client.GracePeriodSeconds(0)) })
}

// S21: GPU types. A type the vendor says has a virtual GPU is left out of the
// catalog and counted with the other drop reasons, and the next-cheapest
// four-GPU type is used. A Pod that requests nvidia.com/gpu: 4 gets that type,
// the type carries the driver labels of the pinned release and the NodeClaim
// initializes once the startup taint is lifted (the node's allocatable has the
// GPUs, as the device plugin would make them appear).
func TestS21GPUType(t *testing.T) {
	fourGPUs := byPrice(filterOfferings(reference(t, us1), func(o offering) bool {
		return isOnDemand(o) && o.GPUs == 4
	}))
	if len(fourGPUs) < 2 {
		t.Fatalf("the snapshot has %d four-GPU offerings in %s, need two types", len(fourGPUs), us1)
	}
	virtual := fourGPUs[0].API
	var want offering
	for _, o := range fourGPUs {
		if o.API != virtual {
			want = o
			break
		}
	}
	if want.API == "" {
		t.Fatalf("every four-GPU offering is of %s", virtual)
	}
	setFake(t, map[string]string{"virtualGPU": "[" + virtual + "]"})
	// The fake reads its ConfigMap at most once a second.
	time.Sleep(3 * time.Second)

	createNodeClass(t, "s21", us1)
	gpuNodePool(t, "s21", "s21", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	markGPUReady(t, "s21")
	wantLabels := map[string]string{
		v1alpha1.LabelAcceleratorCount:    "4",
		v1alpha1.LabelNVIDIADriverBranch:  "570",
		v1alpha1.LabelNVIDIADriverVersion: "570.211.01",
		v1alpha1.LabelNVIDIADriverFlavor:  "open",
	}
	const wantGPUs = 4
	gpuPod(t, "s21", wantGPUs, nil)

	node := waitPodRunning(t, "s21", long)
	claims := waitClaims(t, "s21", 1, time.Minute)
	eventually(t, time.Minute, time.Second, "node initialized", func() (bool, string) {
		return nodeLabels(t, node)[karpv1.NodeInitializedLabelKey] == "true", "not initialized yet"
	})
	l := nodeLabels(t, node)
	if l[labelType] != want.Type {
		t.Errorf("launched %s, want %s (the cheapest four-GPU type, %s excluded as virtual)", l[labelType], want.Type, virtual)
	}
	for k, v := range wantLabels {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: node}, n); err != nil {
		t.Fatal(err)
	}
	if q := n.Status.Allocatable[gpuResource]; q.Value() != wantGPUs {
		t.Errorf("node allocatable nvidia.com/gpu = %v, want %d", q.Value(), wantGPUs)
	}
	if q := claims[0].Status.Capacity[gpuResource]; q.Value() != wantGPUs {
		t.Errorf("NodeClaim capacity nvidia.com/gpu = %v, want %d", q.Value(), wantGPUs)
	}

	// The catalog counted what it left out for this NodeClass.
	m := metrics(t)
	prefix := `scarpenter_catalog_dropped_types{nodeclass="s21",reason="`
	var reasons []string
	for k, v := range m {
		if r, ok := strings.CutPrefix(k, prefix); ok && v > 0 {
			reasons = append(reasons, strings.SplitN(r, `"`, 2)[0])
		}
	}
	// 8-GPU NVSwitch systems are no longer dropped: Fabric Manager is part of the node (S21b).
	for _, r := range []string{"GPUVirtual", "GPUFractional", "GPUArch", "GPUNoDriverBranch"} {
		if m[fmt.Sprintf(`%s%s",vendor="%s"}`, prefix, r, vendor)] < 1 {
			t.Errorf("scarpenter_catalog_dropped_types for %s is missing; exported reasons: %v", r, reasons)
		}
	}
	if got := m[fmt.Sprintf(`%sGPUVirtual",vendor="%s"}`, prefix, vendor)]; got != 1 {
		t.Errorf("GPUVirtual = %v, want the one steered type", got)
	}

	deletePodAndWaitNode(t, "s21", node)
	waitNoClaims(t, "s21", long)
}

// S21b: an 8-GPU NVSwitch system (A100 HGX) is offered, because the table pins a Fabric
// Manager archive for the driver of the release, and a Pod that asks for it gets its
// NodeClaim, which initializes.
func TestS21bNVSwitchType(t *testing.T) {
	a100 := byPrice(filterOfferings(reference(t, us1), func(o offering) bool {
		return isOnDemand(o) && o.GPUs == 8 && strings.HasPrefix(o.API, "p4d")
	}))
	if len(a100) == 0 {
		t.Fatalf("the snapshot has no 8-GPU A100 offering in %s: the NVSwitch type was dropped", us1)
	}
	want := a100[0]

	createNodeClass(t, "s21b", us1)
	gpuNodePool(t, "s21b", "s21b", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	markGPUReady(t, "s21b")
	const wantGPUs = 8
	gpuPod(t, "s21b", wantGPUs, map[string]string{v1alpha1.LabelAcceleratorModel: "A100"})
	node := waitPodRunning(t, "s21b", long)
	waitClaims(t, "s21b", 1, time.Minute)
	eventually(t, time.Minute, time.Second, "node initialized", func() (bool, string) {
		return nodeLabels(t, node)[karpv1.NodeInitializedLabelKey] == "true", "not initialized yet"
	})
	l := nodeLabels(t, node)
	if l[labelType] != want.Type {
		t.Errorf("launched %s, want %s (the cheapest 8-GPU A100 type)", l[labelType], want.Type)
	}
	if l[v1alpha1.LabelAcceleratorCount] != "8" || l[v1alpha1.LabelAcceleratorModel] != "A100" {
		t.Errorf("accelerator labels = %q x %q", l[v1alpha1.LabelAcceleratorCount], l[v1alpha1.LabelAcceleratorModel])
	}
	if l[v1alpha1.LabelNVIDIADriverBranch] == "" {
		t.Error("the driver branch label is not set")
	}
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: node}, n); err != nil {
		t.Fatal(err)
	}
	if q := n.Status.Allocatable[gpuResource]; q.Value() != wantGPUs {
		t.Errorf("node allocatable nvidia.com/gpu = %v, want %d", q.Value(), wantGPUs)
	}
	deletePodAndWaitNode(t, "s21b", node)
	waitNoClaims(t, "s21b", long)
}
