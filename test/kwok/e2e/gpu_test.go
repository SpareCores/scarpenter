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
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

const gpuResource = catalog.ResourceNVIDIAGPU

// gpuPod is a pod that asks for gpus GPUs and little else.
func gpuPod(t *testing.T, name string, gpus int64) {
	t.Helper()
	spec := podSpec(nil, "100m")
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
// four-GPU type is used. Once nodes boot the driver (nvidia.NodeBootstrap), a
// Pod that requests nvidia.com/gpu: 4 gets that type, the type carries the
// driver labels of the pinned release and the NodeClaim initializes (the
// node's allocatable has the GPUs, as the device plugin would make them
// appear). Until then the type is a plain machine: a Pod that requests GPUs
// gets no NodeClaim, and one that selects four GPUs by label gets the type
// with neither the resource nor the driver labels.
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
	createNodePool(t, "s21", "s21", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	wantLabels := map[string]string{
		v1alpha1.LabelAcceleratorCount:    "4",
		v1alpha1.LabelNVIDIADriverBranch:  "570",
		v1alpha1.LabelNVIDIADriverVersion: "570.211.01",
		v1alpha1.LabelNVIDIADriverFlavor:  "open",
	}
	var wantGPUs int64 = 4
	if nvidia.NodeBootstrap {
		gpuPod(t, "s21", 4)
	} else {
		gpuPod(t, "s21-gpu", 4)
		consistently(t, 20*time.Second, 2*time.Second, "no NodeClaim for a Pod that requests GPUs", func() (bool, string) {
			cs, err := listClaims("s21")
			return err == nil && len(cs) > 0, fmt.Sprintf("%d NodeClaims", len(cs))
		})
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "s21-gpu", Namespace: "default"}}
		if err := k8s.Delete(ctx(), p, client.GracePeriodSeconds(0)); err != nil {
			t.Fatal(err)
		}
		createPod(t, "s21", map[string]string{
			v1alpha1.LabelAcceleratorType: "GPU", v1alpha1.LabelAcceleratorCount: "4",
		}, "100m")
		wantLabels[v1alpha1.LabelNVIDIADriverBranch] = ""
		wantLabels[v1alpha1.LabelNVIDIADriverVersion] = ""
		wantLabels[v1alpha1.LabelNVIDIADriverFlavor] = ""
		wantGPUs = 0
	}

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
	for _, r := range []string{"GPUVirtual", "GPUFractional", "GPUArch", "GPUNVSwitch", "GPUNoDriverBranch"} {
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
