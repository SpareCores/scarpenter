//go:build chart

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package chart

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

var crdChartDir = filepath.Join(root, "charts", "scarpenter-crd")

const (
	keepAnnotation = "    helm.sh/resource-policy: keep"
	nodeClassCRD   = "scnodeclasses.scarpenter.sparecores.com"
)

// TestCRDChartIsSynced: the templates of scarpenter-crd are the files of the main
// chart's crds/ (so the same as config/crd, see TestCRDsAreSynced) plus the
// annotation helm.sh/resource-policy: keep and, for Karpenter core's CRDs, the
// karpenterCore condition (make chart-crds generates them).
func TestCRDChartIsSynced(t *testing.T) {
	crds, err := filepath.Glob(filepath.Join(chartDir, "crds", "*.yaml"))
	if err != nil || len(crds) == 0 {
		t.Fatalf("no CRDs in the chart's crds/: %v", err)
	}
	templates, err := filepath.Glob(filepath.Join(crdChartDir, "templates", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != len(crds) {
		t.Errorf("scarpenter-crd has %d templates, want %d (make chart-crds)", len(templates), len(crds))
	}
	for _, crd := range crds {
		want, err := os.ReadFile(crd) //nolint:gosec // repository files
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(crdChartDir, "templates", filepath.Base(crd))) //nolint:gosec // repository files
		if err != nil {
			t.Errorf("%v (make chart-crds)", err)
			continue
		}
		if n := strings.Count(string(got), keepAnnotation+"\n"); n != 1 {
			t.Errorf("%s has the keep annotation %d times, want 1", filepath.Base(crd), n)
		}
		var lines []string
		for l := range strings.SplitSeq(string(got), "\n") {
			if l == keepAnnotation || l == "{{- if .Values.karpenterCore }}" || l == "{{- end }}" {
				continue
			}
			lines = append(lines, l)
		}
		if strings.Join(lines, "\n") != string(want) {
			t.Errorf("templates/%s differs from the crds/ file (make chart-crds)", filepath.Base(crd))
		}
	}
}

// TestCRDChartRenders: the CRDs are kept on uninstall, and karpenterCore=false
// leaves only the SCNodeClass.
func TestCRDChartRenders(t *testing.T) {
	out := helmTemplateCRDs(t)
	if n := strings.Count(out, "kind: CustomResourceDefinition"); n != 3 {
		t.Errorf("rendered %d CRDs, want 3", n)
	}
	if n := strings.Count(out, keepAnnotation); n != 3 {
		t.Errorf("%d CRDs carry helm.sh/resource-policy: keep, want 3", n)
	}
	out = helmTemplateCRDs(t, "--set", "karpenterCore=false")
	if n := strings.Count(out, "kind: CustomResourceDefinition"); n != 1 || !strings.Contains(out, nodeClassCRD) {
		t.Errorf("karpenterCore=false must render the SCNodeClass CRD only, got %d CRDs", n)
	}
}

func helmTemplateCRDs(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("helm", append([]string{"template", "scarpenter-crd", crdChartDir}, args...)...).CombinedOutput() //nolint:gosec // fixed arguments
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return string(out)
}

// TestCRDUpgradeKeepsNewFields is the upgrade of the 0.1.0 -> 0.2.0 report: an
// API server that has the CRD of an older release (no spec.kubelet.footprint, no
// spec.instanceMetadata, the default maxPods: 110) gets the CRD that
// scarpenter-crd renders, and a new NodeClass then keeps the new fields and no
// stored maxPods. The old CRD is derived from the current one by removing
// exactly those, so no old release has to be stored.
func TestCRDUpgradeKeepsNewFields(t *testing.T) {
	current := nodeClassCRDFromChart(t)
	old := current.DeepCopy()
	spec := old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	delete(spec.Properties, "instanceMetadata")
	kubelet := spec.Properties["kubelet"]
	delete(kubelet.Properties, "footprint")
	maxPods := kubelet.Properties["maxPods"]
	maxPods.Default = &apiextensionsv1.JSON{Raw: []byte("110")}
	kubelet.Properties["maxPods"] = maxPods
	spec.Properties["kubelet"] = kubelet
	old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec

	env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{old}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest (is KUBEBUILDER_ASSETS set?): %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	newObj := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "scarpenter.sparecores.com/v1alpha1",
			"kind":       "SCNodeClass",
			"metadata":   map[string]any{"name": name},
			"spec": map[string]any{
				"flatcar":    map[string]any{"version": "4757.2.3"},
				"kubernetes": map[string]any{"version": "v1.37.1"},
				"network":    map[string]any{"template": "direct"},
				"vendors":    map[string]any{"aws": map[string]any{}},
				"kubelet":    map[string]any{"footprint": "Lean"},
				"instanceMetadata": map[string]any{
					"access": "Open",
				},
			},
		}}
	}
	fetch := func(name string) *unstructured.Unstructured {
		got := newObj(name)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Before the upgrade the new fields are pruned and maxPods is defaulted.
	if err := c.Create(ctx, newObj("before")); err != nil {
		t.Fatal(err)
	}
	before := fetch("before")
	if _, found, _ := unstructured.NestedString(before.Object, "spec", "kubelet", "footprint"); found {
		t.Fatal("the simulated old CRD keeps spec.kubelet.footprint: the test does not model the problem")
	}
	if v, _, _ := unstructured.NestedInt64(before.Object, "spec", "kubelet", "maxPods"); v != 110 {
		t.Fatalf("the simulated old CRD does not default maxPods to 110: %v", before.Object["spec"])
	}

	// The upgrade: the CRD of the chart replaces the stored one.
	stored := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, types.NamespacedName{Name: nodeClassCRD}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec = current.Spec
	if err := c.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}

	if err := c.Create(ctx, newObj("after")); err != nil {
		t.Fatal(err)
	}
	after := fetch("after")
	if v, _, _ := unstructured.NestedString(after.Object, "spec", "kubelet", "footprint"); v != "Lean" {
		t.Errorf("spec.kubelet.footprint did not survive: %v", after.Object["spec"])
	}
	if v, _, _ := unstructured.NestedString(after.Object, "spec", "instanceMetadata", "access"); v != "Open" {
		t.Errorf("spec.instanceMetadata did not survive: %v", after.Object["spec"])
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(after.Object, "spec", "kubelet", "maxPods"); found {
		t.Errorf("spec.kubelet.maxPods is stored although nobody set it: %v", after.Object["spec"])
	}
}

// nodeClassCRDFromChart is the SCNodeClass CRD as scarpenter-crd renders it.
func nodeClassCRDFromChart(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	out := helmTemplateCRDs(t, "--set", "karpenterCore=false")
	i := strings.Index(out, "apiVersion:")
	if i < 0 {
		t.Fatalf("no object rendered:\n%.200s", out)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal([]byte(out[i:]), crd); err != nil {
		t.Fatal(err)
	}
	if crd.Name != nodeClassCRD {
		t.Fatalf("rendered %s, want %s", crd.Name, nodeClassCRD)
	}
	return crd
}
