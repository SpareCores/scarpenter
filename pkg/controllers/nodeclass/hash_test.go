/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/fake"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
)

func base() *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{Spec: v1alpha1.SCNodeClassSpec{
		Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
		Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
		Vendors: v1alpha1.VendorBlocks{
			AWS:  &aws.Settings{Regions: []string{"us-east-1"}, InstanceProfile: "a"},
			Fake: &fake.Settings{},
		},
	}}
}

func hash(t *testing.T, nc *v1alpha1.SCNodeClass, vendor string) string {
	t.Helper()
	h, err := nodeclass.Hash(nc, vendor)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHashDependsOnWhatChangesTheNode(t *testing.T) {
	ref := hash(t, base(), "aws")
	if again := hash(t, base(), "aws"); again != ref {
		t.Fatalf("the hash is not deterministic: %s != %s", again, ref)
	}
	for name, change := range map[string]func(*v1alpha1.SCNodeClass){
		"flatcar version":     func(nc *v1alpha1.SCNodeClass) { nc.Spec.Flatcar.Version = "4757.2.4" },
		"kubernetes version":  func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubernetes.Version = "v1.37.2" },
		"kubelet":             func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubelet.MaxPods = ptr.To[int32](50) },
		"poweroff":            func(nc *v1alpha1.SCNodeClass) { nc.Spec.PoweroffAfter = &metav1.Duration{Duration: 3600} },
		"tags":                func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = map[string]string{"a": "b"} },
		"the vendor's block":  func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.InstanceProfile = "b" },
		"networking override": func(nc *v1alpha1.SCNodeClass) { nc.Spec.Networking.PublicIPv4 = new(false) },
	} {
		nc := base()
		change(nc)
		if got := hash(t, nc, "aws"); got == ref {
			t.Errorf("changing the %s does not change the hash", name)
		}
	}
}

func TestHashIgnoresWhatDoesNotChangeTheNode(t *testing.T) {
	ref := hash(t, base(), "aws")
	for name, change := range map[string]func(*v1alpha1.SCNodeClass){
		"regions":        func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"eu-west-1"} },
		"another vendor": func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.Fake = nil },
		"metrics":        func(nc *v1alpha1.SCNodeClass) { nc.Spec.Metrics = map[string]v1alpha1.Metric{"a": {}} },
		"selections":     func(nc *v1alpha1.SCNodeClass) { nc.Spec.Selections = map[string]v1alpha1.Selection{"a": {}} },
		"the catalog settings": func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Catalog = &v1alpha1.CatalogSpec{CPUFlagLabels: []string{"avx2"}}
		},
	} {
		nc := base()
		change(nc)
		if got := hash(t, nc, "aws"); got != ref {
			t.Errorf("changing the %s changes the hash", name)
		}
	}
}

func TestHashIsPerVendor(t *testing.T) {
	nc := base()
	if hash(t, nc, "aws") == hash(t, nc, "fake") {
		t.Error("two vendors share a hash")
	}
}
