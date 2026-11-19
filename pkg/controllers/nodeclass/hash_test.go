/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"encoding/json"
	"strings"
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
	h, err := nodeclass.Hash(nc, vendor, nodeclass.Inputs{})
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
		"kubelet footprint":   func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubelet.Footprint = v1alpha1.FootprintLean },
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

// TestHashKeepsNodeClassesWithoutFootprint pins what the hash reads of the
// kubelet block: a NodeClass that does not set the footprint hashes the same as
// before the field existed, so that adding it drifts no node.
func TestHashKeepsNodeClassesWithoutFootprint(t *testing.T) {
	raw, err := json.Marshal(base().Spec.Kubelet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "footprint") {
		t.Errorf("an unset footprint appears in the hashed kubelet block: %s", raw)
	}
	// maxPods lost its CRD default: an unset maxPods hashes as the 110 that
	// NodeClasses stored before carry.
	stored, unset := base(), base()
	stored.Spec.Kubelet.MaxPods = ptr.To[int32](110)
	unset.Spec.Kubelet.MaxPods = nil
	if hash(t, stored, "aws") != hash(t, unset, "aws") {
		t.Error("an unset maxPods hashes differently from the stored default 110")
	}
}

func TestHashIgnoresWhatDoesNotChangeTheNode(t *testing.T) {
	ref := hash(t, base(), "aws")
	for name, change := range map[string]func(*v1alpha1.SCNodeClass){
		"regions":        func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"eu-west-1"} },
		"another vendor": func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.Fake = nil },
		"metrics":        func(nc *v1alpha1.SCNodeClass) { nc.Spec.Metrics = map[string]v1alpha1.Metric{"a": {}} },
		"selections":     func(nc *v1alpha1.SCNodeClass) { nc.Spec.Selections = map[string]v1alpha1.Selection{"a": {}} },
		"the billing retention": func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Billing = &v1alpha1.BillingSpec{RetainUntilPeriodEnd: true}
		},
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

// The hash covers what the spec points at, not only the spec: a changed custom
// template, Butane snippet or auth key Secret, or a re-published image, drifts
// the nodes (overview section 6.3).
func TestHashCoversTheResolvedInputs(t *testing.T) {
	nc := base()
	hashWith := func(in nodeclass.Inputs) string {
		t.Helper()
		h, err := nodeclass.Hash(nc, "aws", in)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	resolved := func(sum string) *v1alpha1.ResolvedArtifacts {
		return &v1alpha1.ResolvedArtifacts{Kubernetes: map[string]v1alpha1.Artifact{"amd64": {URL: "https://example.com/k.raw", SHA256: sum}}}
	}
	ref := hashWith(nodeclass.Inputs{Resolved: resolved("aa"), CustomTemplate: "a", UserButane: "b", AuthKeyVersion: "1"})
	if again := hashWith(nodeclass.Inputs{Resolved: resolved("aa"), CustomTemplate: "a", UserButane: "b", AuthKeyVersion: "1"}); again != ref {
		t.Fatal("the hash is not deterministic")
	}
	for name, in := range map[string]nodeclass.Inputs{
		"image digest":    {Resolved: resolved("bb"), CustomTemplate: "a", UserButane: "b", AuthKeyVersion: "1"},
		"custom template": {Resolved: resolved("aa"), CustomTemplate: "changed", UserButane: "b", AuthKeyVersion: "1"},
		"butane":          {Resolved: resolved("aa"), CustomTemplate: "a", UserButane: "changed", AuthKeyVersion: "1"},
		"auth key secret": {Resolved: resolved("aa"), CustomTemplate: "a", UserButane: "b", AuthKeyVersion: "2"},
	} {
		if hashWith(in) == ref {
			t.Errorf("changing the %s does not change the hash", name)
		}
	}
}

// The deprecated spec.bootstrap.hardening stays in the hash as it was, so that a
// stored NodeClass that sets it does not drift when SCarpenter is upgraded
// (ADR-025). The values are the hashes the code before the deprecation computed
// for this NodeClass: for the vendors whose drivers applied the hardening it is
// part of the hash, for the others it is not.
func TestDeprecatedHardeningKeepsTheOldHash(t *testing.T) {
	nc := base()
	nc.Spec.Bootstrap = &v1alpha1.BootstrapSpec{Hardening: []v1alpha1.Hardening{v1alpha1.HardeningMetadataFromPods}}
	const (
		wantFake = "8c08ddaeb1f0d99624a889eac76565afe6d5725c9ef37f49dc65e3dba9d4a9e5"
		wantAWS  = "73bc67b8b0c44d3368cf2b63d1ee6a37d90d1daae83a45a69e3f8c5c4a19ba84"
	)
	if got := hash(t, nc, "fake"); got != wantFake {
		t.Errorf("fake hash = %s, want %s", got, wantFake)
	}
	if got := hash(t, nc, "aws"); got != wantAWS {
		t.Errorf("aws hash = %s, want %s", got, wantAWS)
	}
	if hash(t, nc, "aws") != hash(t, base(), "aws") {
		t.Error("a vendor that never applied the hardening drifts")
	}
}
