/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

func gpuType(sel *nvidia.Selection, count int64) *cloudprovider.InstanceType {
	reqs := scheduling.NewRequirements(labels.NVIDIADriver(sel)...)
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
	got, err := nvidiaSelection(nodeClassOn("4757.2.3"), gpuType(&want, 4))
	if err != nil || got == nil {
		t.Fatalf("selection = %v, %v", got, err)
	}
	want.Architecture = "" // the labels do not carry it, the node does not need it
	if *got != want {
		t.Errorf("selection = %+v, want %+v", *got, want)
	}

	if got, err := nvidiaSelection(nodeClassOn("4757.2.3"), gpuType(nil, 0)); got != nil || err != nil {
		t.Errorf("a type without driver labels: %v, %v", got, err)
	}
	// Labels the table of the NodeClass's release cannot back stop the launch.
	if _, err := nvidiaSelection(nodeClassOn("4000.0.0"), gpuType(&want, 4)); err == nil {
		t.Error("a release without a table launched")
	}
}
