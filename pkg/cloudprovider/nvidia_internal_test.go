/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

func gpuType(sel *nvidia.Selection, model string, count int64) *cloudprovider.InstanceType {
	reqs := scheduling.NewRequirements(labels.NVIDIADriver(sel)...)
	reqs.Add(labels.Enum(v1alpha1.LabelAcceleratorModel, model))
	capacity := corev1.ResourceList{}
	if sel != nil {
		capacity[catalog.ResourceNVIDIAGPU] = *resource.NewQuantity(count, resource.DecimalSI)
	}
	return &cloudprovider.InstanceType{Name: "aws.test", Requirements: reqs, Capacity: capacity}
}

func nodeClassOn(flatcar string) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{}
	nc.Spec.Flatcar.Version = flatcar
	return nc
}

// The launch reads the driver of a type from the labels the catalog stamped on it, with
// the pins of the NodeClass's release; a type without the labels has none.
func TestNVIDIASelectionFromTheTypeLabels(t *testing.T) {
	want, reason := nvidia.Select(nvidia.GPU{Manufacturer: "NVIDIA", Model: "V100", Count: 4, AMD64: true}, "4757.2.3")
	if reason != nvidia.Supported {
		t.Fatal(reason)
	}
	got, err := nvidiaSelection(nodeClassOn("4757.2.3"), gpuType(&want, "V100", 4))
	if err != nil || got == nil {
		t.Fatalf("selection = %v, %v", got, err)
	}
	if *got != want {
		t.Errorf("selection = %+v, want %+v", *got, want)
	}

	// The model label tells an NVSwitch system, which the launch needs for Fabric Manager.
	hgx, reason := nvidia.Select(nvidia.GPU{Manufacturer: "NVIDIA", Model: "H100", Count: 8, AMD64: true}, "4757.2.3")
	if reason != nvidia.Supported || !hgx.NVSwitch {
		t.Fatalf("H100 x8: %+v, %q", hgx, reason)
	}
	got, err = nvidiaSelection(nodeClassOn("4757.2.3"), gpuType(&hgx, "H100", 8))
	if err != nil || got == nil || *got != hgx {
		t.Errorf("NVSwitch selection = %v, %v, want %+v", got, err, hgx)
	}

	if got, err := nvidiaSelection(nodeClassOn("4757.2.3"), gpuType(nil, "", 0)); got != nil || err != nil {
		t.Errorf("a type without driver labels: %v, %v", got, err)
	}
	// Labels the table of the NodeClass's release cannot back stop the launch.
	if _, err := nvidiaSelection(nodeClassOn("4000.0.0"), gpuType(&want, "V100", 4)); err == nil {
		t.Error("a release without a table launched")
	}
}

// ADR-031: a NodePool that does not list the gpu-pending startup taint is not offered the
// NVIDIA types (it fails closed), and the log line comes once per NodePool.
func TestGPUTypesNeedTheStartupTaint(t *testing.T) {
	sel, reason := nvidia.Select(nvidia.GPU{Manufacturer: "NVIDIA", Model: "T4", Count: 1, AMD64: true}, "4757.2.3")
	if reason != nvidia.Supported {
		t.Fatal(reason)
	}
	gpu, cpu := gpuType(&sel, "T4", 1), gpuType(nil, "", 0)
	pool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "mixed"}}
	var lines int
	ctx := logr.NewContext(t.Context(), funcr.New(func(_, _ string) { lines++ }, funcr.Options{}))
	c := &CloudProvider{}
	for range 2 {
		got := c.withoutUnguardedGPUs(ctx, pool, []*cloudprovider.InstanceType{gpu, cpu})
		if len(got) != 1 || got[0] != cpu {
			t.Fatalf("without the startup taint: %d types, want only the CPU type", len(got))
		}
	}
	if lines != 1 {
		t.Errorf("%d log lines for two calls, want 1", lines)
	}
	pool.Spec.Template.Spec.StartupTaints = []corev1.Taint{{Key: v1alpha1.TaintGPUPending, Effect: corev1.TaintEffectNoSchedule}}
	if got := c.withoutUnguardedGPUs(ctx, pool, []*cloudprovider.InstanceType{gpu, cpu}); len(got) != 2 {
		t.Errorf("with the startup taint: %d types, want both", len(got))
	}
}
