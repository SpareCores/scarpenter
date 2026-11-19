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

// S21: a pod that requests nvidia.com/gpu: 4 gets the cheapest supported type
// with four GPUs and its NodeClaim initializes (the node's allocatable has the
// GPUs, as the device plugin would make them appear); the type carries the
// driver labels of the pinned release. A type the vendor says has a virtual GPU
// is left out of the catalog and counted, and the next-cheapest type is used.
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

	createNodeClass(t, "s20", us1)
	createNodePool(t, "s20", "s20", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	gpuPod(t, "s20", 4)

	node := waitPodRunning(t, "s20", long)
	claims := waitClaims(t, "s20", 1, time.Minute)
	eventually(t, time.Minute, time.Second, "node initialized", func() (bool, string) {
		return nodeLabels(t, node)[karpv1.NodeInitializedLabelKey] == "true", "not initialized yet"
	})
	l := nodeLabels(t, node)
	if l[labelType] != want.Type {
		t.Errorf("launched %s, want %s (the cheapest four-GPU type, %s excluded as virtual)", l[labelType], want.Type, virtual)
	}
	for k, v := range map[string]string{
		v1alpha1.LabelAcceleratorCount:    "4",
		v1alpha1.LabelNVIDIADriverBranch:  "570",
		v1alpha1.LabelNVIDIADriverVersion: "570.211.01",
		v1alpha1.LabelNVIDIADriverFlavor:  "open",
	} {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: node}, n); err != nil {
		t.Fatal(err)
	}
	if q := n.Status.Allocatable[gpuResource]; q.Value() != 4 {
		t.Errorf("node allocatable nvidia.com/gpu = %v, want 4", q.Value())
	}
	if q := claims[0].Status.Capacity[gpuResource]; q.Value() != 4 {
		t.Errorf("NodeClaim capacity nvidia.com/gpu = %v, want 4", q.Value())
	}

	// The catalog counted what it left out for this NodeClass.
	m := metrics(t)
	prefix := `scarpenter_catalog_dropped_types{nodeclass="s20",reason="`
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

	deletePodAndWaitNode(t, "s20", node)
	waitNoClaims(t, "s20", long)
}
