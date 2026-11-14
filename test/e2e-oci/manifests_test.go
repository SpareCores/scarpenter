/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2eoci_test

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
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/sweep"
)

// The manifests of the L5 lane (test/e2e-oci/manifests) are templates for envsubst.
// These tests render them with sample values and send every object to an envtest API
// server as a server-side dry run, with strict field validation, the CRDs of SCarpenter
// and Karpenter core installed: the CEL rules, the schema and the core types accept
// what the workflow will apply, before a cloud bill is run up to find out.

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
// defaults.sh and what each render call adds (lib.sh probe and node_shell, the byo scenario's
// resources).
var sample = map[string]string{
	"OCI_COMPARTMENT": "ocid1.compartment.oc1..aaaaexample", "OCI_IMAGE_COMPARTMENT": "ocid1.compartment.oc1..aaaaimages", "OCI_TENANCY": "ocid1.tenancy.oc1..aaaaexample",
	"OCI_REGION": "eu-frankfurt-1", "OCI_HOME_REGION": "eu-frankfurt-1",
	"OCI_IDENTITY_DOMAIN_URL": "https://idcs-0123456789abcdef0123456789abcdef.identity.oraclecloud.com",
	"OCI_CLIENT_ID":           "0123456789abcdef", "OCI_OIDC_AUDIENCE": "oci-federation",
	"E2E_CLUSTER": "scarpenter-ci-1234567890", "E2E_RUN_ID": "1234567890",
	"E2E_CREATED_AT": "1800000000", "E2E_API_HOST": "100.64.0.1",
	"E2E_K3S_VERSION": "v1.37.2+k3s1", "E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10",
	"E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
	"E2E_TYPE_AMD64": "oci.VM.Standard.E5.Flex.1-4", "E2E_TYPE_ARM64": "oci.VM.Standard.A1.Flex.1-2",
	"E2E_TYPE_ARM64_ALT":       "oci.VM.Standard.A2.Flex.1-8",
	"E2E_TYPES_DESCRIBED_YAML": `"oci.VM.Standard.E5.Flex.1-4", "oci.VM.Standard.E5.Flex.1-8", "oci.VM.Standard.E5.Flex.2-8"`,
	"E2E_TYPES_ARM64_YAML":     `"oci.VM.Standard.A1.Flex.1-2", "oci.VM.Standard.A2.Flex.1-8"`,
	"E2E_ROOT_GB":              "60", "E2E_ROOT_GB_DRIFT": "70",
	"E2E_TAG_RUNNER": "tag:scarpenter-ci", "E2E_TAG_NODE": "tag:scarpenter-ci-node",
	"E2E_BYO_SUBNET": "ocid1.subnet.oc1.eu-frankfurt-1.aaaaexamplesubnet",
	"E2E_BYO_NSG":    "ocid1.networksecuritygroup.oc1.eu-frankfurt-1.aaaaexamplegroup",
	"NAME":           "e2e-amd64", "POOL": "e2e-amd64", "TYPE": "oci.VM.Standard.E5.Flex.1-4", "CAPACITY": "on-demand",
	"NODE": "node-1", "PODNAME": "nodeshell-node-1",
}

var varRE = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

func manifests(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "test", "e2e-oci", "manifests", "*.yaml"))
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
	defaults, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "test", "e2e-oci", "defaults.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^: "\$\{([A-Z0-9_]+):=`).FindAllStringSubmatch(string(defaults), -1) {
		pins[m[1]] = true
	}
	pins["E2E_TYPES_DESCRIBED_YAML"] = true // derived in defaults.sh
	pins["E2E_TYPES_ARM64_YAML"] = true
	// Set by the workflow, the scenarios or the render call.
	for _, v := range []string{"E2E_CLUSTER", "E2E_RUN_ID", "E2E_CREATED_AT", "E2E_API_HOST", "E2E_BYO_SUBNET", "E2E_BYO_NSG",
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

// TestNodePoolsOnlyNameTheTypesTheSuiteLaunches keeps the pools and the instance types
// the quota notes list (docs/how-to-guides/testing.md) together: a type in a pool that the docs
// do not name is one the quota and cost notes do not cover.
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
		for _, m := range regexp.MustCompile(`oci\.VM\.[A-Za-z0-9.]+\.[0-9]+-[0-9]+`).FindAllString(render(t, f), -1) {
			if !slices.Contains(types, m) {
				types = append(types, m)
			}
		}
	}
	for _, typ := range types {
		if !strings.Contains(string(doc), strings.TrimPrefix(typ, "oci.")) {
			t.Errorf("the instance type %s is launched by the lane but not named in docs/how-to-guides/testing.md", typ)
		}
	}
	if len(types) < 5 {
		t.Errorf("expected the pools to name at least five types, got %v", types)
	}
}

// TestPoolsNameNodeClassesThatExist catches a typo between a NodePool's nodeClassRef and the
// SCNodeClasses of the manifests, which no API server dry run sees (the reference is a name).
func TestPoolsNameNodeClassesThatExist(t *testing.T) {
	classes := map[string]bool{}
	refs := map[string][]string{}
	for _, f := range manifests(t) {
		if filepath.Base(f) == "chart-values.yaml" {
			continue
		}
		dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(render(t, f))), 4096)
		for {
			obj := &unstructured.Unstructured{}
			if err := dec.Decode(&obj.Object); err != nil {
				break
			}
			switch obj.GetKind() {
			case "SCNodeClass":
				classes[obj.GetName()] = true
			case "NodePool":
				name, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "spec", "nodeClassRef", "name")
				refs[obj.GetName()] = append(refs[obj.GetName()], name)
			}
		}
	}
	if len(classes) < 6 || len(refs) < 5 {
		t.Fatalf("found %d NodeClasses and %d NodePools", len(classes), len(refs))
	}
	for pool, names := range refs {
		for _, n := range names {
			if !classes[n] {
				t.Errorf("the NodePool %s names the NodeClass %q, which no manifest defines", pool, n)
			}
		}
	}
}

// TestNodeClassesNameTheLanesRegion: every NodeClass but the negative one that tests the
// refusal of a region names the lane's region (and only that one), so that a NodeClass cannot
// launch anywhere else.
func TestNodeClassesNameTheLanesRegion(t *testing.T) {
	for _, f := range manifests(t) {
		if !strings.HasPrefix(filepath.Base(f), "nodeclass-") {
			continue
		}
		raw, _ := os.ReadFile(f)
		classes := strings.Count(string(raw), "kind: SCNodeClass")
		got := strings.Count(string(raw), "regions: [${OCI_REGION}]")
		if filepath.Base(f) == "nodeclass-negative.yaml" {
			// e2e-badregion names a region nobody subscribed to on purpose, e2e-noipv4 the lane's.
			if got != 1 || !strings.Contains(string(raw), "regions: [xx-nowhere-1]") {
				t.Errorf("nodeclass-negative.yaml: want one class with the lane's region and one with another, got %d", got)
			}
			continue
		}
		if got != classes {
			t.Errorf("%s: %d NodeClasses but %d name the lane's region", filepath.Base(f), classes, got)
		}
	}
}

// TestNodeClassesCarryTheSweepersTag ties the tags the NodeClasses put on every instance
// (their freeform tags) to the sweeper's constants: the tag is what the sweeper falls back
// to for an instance without a creation time.
func TestNodeClassesCarryTheSweepersTag(t *testing.T) {
	for _, f := range manifests(t) {
		if !strings.HasPrefix(filepath.Base(f), "nodeclass-") {
			continue
		}
		if !strings.Contains(render(t, f), sweep.CreatedAtTag+":") {
			t.Errorf("%s does not tag its resources with %s", filepath.Base(f), sweep.CreatedAtTag)
		}
	}
}
