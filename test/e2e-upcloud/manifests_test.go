/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2eupcloud_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors/upcloud"
)

// The manifests of the L5 lane (test/e2e-upcloud/manifests) are templates for envsubst.
// These tests render them with sample values and send every object to an envtest API
// server as a server-side dry run, with strict field validation, the CRDs of SCarpenter
// and Karpenter core installed: the CEL rules, the schema and the core types accept
// what the workflow will apply, before an UpCloud bill is run up to find out.

var k8sClient client.Client

func TestMain(m *testing.M) {
	os.Exit(func() int {
		cfg, stop, err := testutil.StartEnvWith(testutil.EnvOptions{CRDs: testutil.KarpenterCRDs()})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() {
			if err := stop(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
		scheme := runtime.NewScheme()
		if err := errors.Join(v1alpha1.AddToScheme(scheme), clientgoscheme.AddToScheme(scheme)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}())
}

// The variables the scripts set before they render a manifest: the run's, the pins of
// defaults.sh and what each render call adds (lib.sh probe and node_shell).
var sample = map[string]string{
	"UPCLOUD_ZONE": "fi-hel1", "E2E_CLUSTER": "scarpenter-ci-1234567890", "E2E_RUN_ID": "1234567890",
	"E2E_CREATED_AT": "1800000000", "E2E_API_HOST": "100.64.0.1",
	"E2E_K3S_VERSION": "v1.37.2+k3s1", "E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10",
	"E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
	"E2E_TYPE_AMD64":           "upcloud.STARTER-1xCPU-1GB",
	"E2E_TYPES_DESCRIBED_YAML": `"upcloud.STARTER-1xCPU-1GB", "upcloud.STARTER-2xCPU-4GB", "upcloud.STARTER-2xCPU-8GB"`,
	"E2E_TAG_RUNNER":           "tag:scarpenter-ci", "E2E_TAG_NODE": "tag:scarpenter-ci-node",
	"NAME": "e2e-amd64", "POOL": "e2e-amd64", "TYPE": "upcloud.STARTER-1xCPU-1GB", "CAPACITY": "on-demand",
	"NODE": "node-1", "PODNAME": "nodeshell-node-1",
}

var varRE = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

func manifests(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "test", "e2e-upcloud", "manifests", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests: %v", err)
	}
	return files
}

func render(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	out := varRE.ReplaceAllStringFunc(string(raw), func(m string) string {
		name := varRE.FindStringSubmatch(m)[1]
		v, ok := sample[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		t.Fatalf("%s uses variables nobody sets: %v", filepath.Base(file), missing)
	}
	out = strings.NewReplacer("@API_HOST@", sample["E2E_API_HOST"], "@KUBE_PROXY_VERSION@", "v1.37.2").Replace(out)
	if m := regexp.MustCompile(`@[A-Z_]+@`).FindString(out); m != "" {
		t.Fatalf("%s has an unsubstituted placeholder %s", filepath.Base(file), m)
	}
	return out
}

// TestVariablesAreDeclared checks that every variable the manifests use is a pin of
// defaults.sh, a run variable the scripts export, or one of the render call's own: a
// typo in a manifest is otherwise found by the render guard of lib.sh on the first run.
func TestVariablesAreDeclared(t *testing.T) {
	defaults, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "test", "e2e-upcloud", "defaults.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^: "\$\{([A-Z0-9_]+):=`).FindAllStringSubmatch(string(defaults), -1) {
		pins[m[1]] = true
	}
	pins["E2E_TYPES_DESCRIBED_YAML"] = true // derived in defaults.sh
	// Set by the workflow, the scenarios or the render call.
	for _, v := range []string{"E2E_CLUSTER", "E2E_RUN_ID", "E2E_CREATED_AT", "E2E_API_HOST",
		"NAME", "POOL", "TYPE", "CAPACITY", "NODE", "PODNAME"} {
		pins[v] = true
	}
	for _, f := range manifests(t) {
		raw, _ := os.ReadFile(f)
		for _, m := range varRE.FindAllStringSubmatch(string(raw), -1) {
			if !pins[m[1]] {
				t.Errorf("%s: ${%s} is not set by defaults.sh or the scripts", filepath.Base(f), m[1])
			}
		}
	}
}

func TestManifestsAreAcceptedByAnAPIServer(t *testing.T) {
	ctx := context.Background()
	for _, n := range []string{"scarpenter-system", "kube-flannel"} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}
		if err := k8sClient.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range manifests(t) {
		if filepath.Base(f) == "chart-values.yaml" {
			continue // Helm values, not objects (the chart is rendered by selftest.sh)
		}
		t.Run(filepath.Base(f), func(t *testing.T) {
			dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(render(t, f))), 4096)
			n := 0
			for {
				obj := &unstructured.Unstructured{}
				err := dec.Decode(&obj.Object)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("decoding: %v", err)
				}
				if len(obj.Object) == 0 {
					continue
				}
				n++
				if namespaced, err := k8sClient.IsObjectNamespaced(obj); err != nil {
					t.Errorf("%s %s: %v", obj.GetKind(), obj.GetName(), err)
					continue
				} else if namespaced && obj.GetNamespace() == "" {
					obj.SetNamespace("default") // what kubectl apply does
				}
				// The namespaces the objects need were created above; a manifest that
				// declares one again only proves that it is valid.
				err = k8sClient.Create(ctx, obj, client.DryRunAll, client.FieldValidation("Strict"))
				if err != nil && (obj.GetKind() != "Namespace" || !apierrors.IsAlreadyExists(err)) {
					t.Errorf("%s %s: %v", obj.GetKind(), obj.GetName(), err)
				}
			}
			if n == 0 {
				t.Errorf("no objects in %s", f)
			}
		})
	}
}

// TestNodePoolsOnlyNameTheTypesTheSuiteLaunches keeps the pools and the plans the docs
// list together: a plan in a pool that docs/how-to-guides/testing.md does not name would be
// launched without anyone having judged its cost.
func TestNodePoolsOnlyNameTheTypesTheSuiteLaunches(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "docs", "how-to-guides", "testing.md"))
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, f := range manifests(t) {
		if !strings.HasPrefix(filepath.Base(f), "nodepool") {
			continue
		}
		for _, m := range regexp.MustCompile(`upcloud\.[A-Z0-9]+-[A-Za-z0-9-]+`).FindAllString(render(t, f), -1) {
			if !slices.Contains(types, m) {
				types = append(types, m)
			}
		}
	}
	for _, typ := range types {
		if !strings.Contains(string(doc), strings.TrimPrefix(typ, "upcloud.")) {
			t.Errorf("the plan %s is launched by the lane but not named in docs/how-to-guides/testing.md", typ)
		}
	}
	if len(types) < 3 {
		t.Errorf("expected the pools to name at least three plans, got %v", types)
	}
}

// TestNodeClassesUseValidLabels ties the NodeClasses' spec.tags to the driver's rules for
// UpCloud label keys (2 to 32 characters): a key the driver refuses would keep the class
// from being Ready. The negative class breaks the rule on purpose.
func TestNodeClassesUseValidLabels(t *testing.T) {
	for _, f := range manifests(t) {
		if !strings.HasPrefix(filepath.Base(f), "nodeclass-") {
			continue
		}
		dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(render(t, f))), 4096)
		for {
			obj := &unstructured.Unstructured{}
			if err := dec.Decode(&obj.Object); err != nil {
				break
			}
			tags, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "tags")
			err := upcloud.ValidateExtraTags(tags)
			if broken := obj.GetName() == "e2e-badlabel"; broken == (err == nil) {
				t.Errorf("%s: ValidateExtraTags(%v) = %v", obj.GetName(), tags, err)
			}
		}
	}
}
