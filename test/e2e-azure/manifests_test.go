/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2eazure_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/azure"
)

// createdAtTag is the tag the NodeClasses put on every resource of a run and the sweeper
// (pkg/vendors/azure/sweep, e2e-azure-sweeper.yml) reads to age a resource. It is a literal
// here, not an import, so that the lane's scripts stand on their own; the sweeper's constant
// must have the same value (plan 17 section 9: scarpenter-ci-created-at).
const createdAtTag = "scarpenter-ci-created-at"

// The manifests of the L5 lane (test/e2e-azure/manifests) are templates for envsubst.
// These tests render them with sample values and send every object to an envtest API
// server as a server-side dry run, with strict field validation, the CRDs of SCarpenter
// and Karpenter core installed: the CEL rules, the schema and the core types accept
// what the workflow will apply, before an Azure bill is run up to find out.

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
	"AZURE_REGION": "westeurope", "AZURE_SUBSCRIPTION_ID": "00000000-0000-0000-0000-000000000000",
	"AZURE_TENANT_ID": "00000000-0000-0000-0000-000000000001", "AZURE_CLIENT_ID": "00000000-0000-0000-0000-000000000002",
	"AZURE_RESOURCE_GROUP": "scarpenter-ci-lane",
	"E2E_CLUSTER":          "scarpenter-ci-1234567890", "E2E_RUN_ID": "1234567890",
	"E2E_CREATED_AT": "1800000000", "E2E_CREATED_AT_RFC3339": "2027-01-15T08:00:00Z", "E2E_API_HOST": "100.64.0.1",
	"E2E_K3S_VERSION": "v1.37.2+k3s1", "E2E_KUBERNETES_VERSION": "v1.37.1", "E2E_TAILSCALE_VERSION": "v1.98.10",
	"E2E_FLATCAR_CHANNEL": "stable", "E2E_FLATCAR_VERSION": "4757.2.3",
	"E2E_TYPE_AMD64": "azure.Standard_B2ats_v2", "E2E_TYPE_ARM64": "azure.Standard_B2pts_v2",
	"E2E_TYPE_SPOT": "azure.Standard_D2as_v5", "E2E_TYPE_DB": "azure.Standard_D2as_v5", "E2E_TYPE_DENIED": "azure.Standard_B2ls_v2",
	"E2E_TYPES_DESCRIBED_YAML": `"azure.Standard_B2ats_v2", "azure.Standard_B2as_v2"`,
	"E2E_TAG_RUNNER":           "tag:scarpenter-ci", "E2E_TAG_NODE": "tag:scarpenter-ci-node",
	"E2E_PG_ZONE":       "1",
	"E2E_BYO_SUBNET_ID": "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/scarpenter-ci-lane/providers/Microsoft.Network/virtualNetworks/scarpenter-ci-1234567890-byo/subnets/nodes",
	"E2E_BYO_NSG_ID":    "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/scarpenter-ci-lane/providers/Microsoft.Network/networkSecurityGroups/scarpenter-ci-1234567890-byo",
	"NAME":              "e2e-amd64", "POOL": "e2e-amd64", "TYPE": "azure.Standard_B2ats_v2", "CAPACITY": "on-demand",
	"NODE": "node-1", "PODNAME": "nodeshell-node-1",
}

var varRE = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

func manifests(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "test", "e2e-azure", "manifests", "*.yaml"))
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
	defaults, err := os.ReadFile(filepath.Join(testutil.RepoRoot(), "test", "e2e-azure", "defaults.sh"))
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^: "\$\{([A-Z0-9_]+):=`).FindAllStringSubmatch(string(defaults), -1) {
		pins[m[1]] = true
	}
	pins["E2E_TYPES_DESCRIBED_YAML"] = true // derived in defaults.sh
	// Set by the workflow, the scenarios or the render call.
	for _, v := range []string{"E2E_CLUSTER", "E2E_RUN_ID", "E2E_CREATED_AT", "E2E_CREATED_AT_RFC3339", "E2E_API_HOST", "E2E_PG_ZONE",
		"AZURE_SUBSCRIPTION_ID", "AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_RESOURCE_GROUP", "E2E_BYO_SUBNET_ID", "E2E_BYO_NSG_ID",
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

// policySizes runs ci-policy.sh (print only) and returns the sizes of the "Allowed virtual
// machine size SKUs" assignment.
func policySizes(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("bash", filepath.Join(testutil.RepoRoot(), "test", "e2e-azure", "ci-policy.sh"),
		sample["AZURE_SUBSCRIPTION_ID"]).Output()
	if err != nil {
		t.Fatalf("ci-policy.sh: %v", err)
	}
	var doc struct {
		PolicyAssignments []struct {
			DisplayName string `json:"displayName"`
			Parameters  struct {
				Sizes struct {
					Value []string `json:"value"`
				} `json:"listOfAllowedSKUs"`
			} `json:"parameters"`
		} `json:"policyAssignments"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range doc.PolicyAssignments {
		if p.DisplayName == "Allowed virtual machine size SKUs" {
			return p.Parameters.Sizes.Value
		}
	}
	t.Fatal("ci-policy.sh prints no size policy")
	return nil
}

// TestNodePoolsOnlyNameTheTypesTheSuiteLaunches keeps the pools and the size policy the
// CI account's operator applies (ci-policy.sh, built from defaults.sh) together: a size in a
// pool that the policy does not allow would be refused by Azure on the first run, and the
// one size the negative pool names must be refused, so it must not be in the policy.
func TestNodePoolsOnlyNameTheTypesTheSuiteLaunches(t *testing.T) {
	allowed := policySizes(t)
	sizeRE := regexp.MustCompile(`azure\.(Standard_[A-Za-z0-9_]+)`)
	var types []string
	for _, f := range manifests(t) {
		if !strings.HasPrefix(filepath.Base(f), "nodepool") {
			continue
		}
		for _, m := range sizeRE.FindAllStringSubmatch(render(t, f), -1) {
			if filepath.Base(f) == "nodepool-negative.yaml" && m[1] == strings.TrimPrefix(sample["E2E_TYPE_DENIED"], "azure.") {
				if slices.Contains(allowed, m[1]) {
					t.Errorf("the denied size %s is in the size policy: the negative scenario would launch it", m[1])
				}
				continue
			}
			if !slices.Contains(types, m[1]) {
				types = append(types, m[1])
			}
		}
	}
	for _, typ := range types {
		if !slices.Contains(allowed, typ) {
			t.Errorf("the size %s is launched by the lane but not in the size policy %v", typ, allowed)
		}
	}
	if len(types) < 4 {
		t.Errorf("expected the pools to name at least four sizes, got %v", types)
	}
}

// TestNodeClassesCarryTheSweepersTag ties the tags the NodeClasses put on every resource to
// the sweeper's tag: without it on managed networks and groups the sweeper would leave them
// alone for ever.
func TestNodeClassesCarryTheSweepersTag(t *testing.T) {
	for _, f := range manifests(t) {
		if !strings.HasPrefix(filepath.Base(f), "nodeclass-") {
			continue
		}
		if !strings.Contains(render(t, f), createdAtTag+":") {
			t.Errorf("%s does not tag its resources with %s", filepath.Base(f), createdAtTag)
		}
	}
}

// TestLaneNodeClassesBringTheResourceGroup checks that every NodeClass the lane launches on
// brings the lane's resource group (the CI role is scoped to it: a managed resource group
// would need rights the role does not have) and is limited to the lane's region.
func TestLaneNodeClassesBringTheResourceGroup(t *testing.T) {
	for _, f := range manifests(t) {
		base := filepath.Base(f)
		if !strings.HasPrefix(base, "nodeclass-") || base == "nodeclass-negative.yaml" {
			continue
		}
		out := render(t, f)
		if !strings.Contains(out, `resourceGroups: {use: {ids: {`+sample["AZURE_REGION"]+`: ["`+sample["AZURE_RESOURCE_GROUP"]+`"]}}}`) {
			t.Errorf("%s does not bring the lane's resource group", base)
		}
		if !strings.Contains(out, "regions: ["+sample["AZURE_REGION"]+"]") {
			t.Errorf("%s is not limited to the lane's region", base)
		}
	}
}

// libValue sources lib.sh with a sample cluster and prints one expression.
func libValue(t *testing.T, expr string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", `source "$1"; eval "$2"`, "bash",
		filepath.Join(testutil.RepoRoot(), "test", "e2e-azure", "lib.sh"), expr)
	cmd.Env = append(os.Environ(), "E2E_CLUSTER=scarpenter-ci-1234567890")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("lib.sh %q: %v", expr, err)
	}
	return strings.TrimSpace(string(out))
}

// TestLibAgreesWithTheDriver ties what the scripts look for to what the driver writes: the VM's
// name, the tag names and the quota bucket names. A drift here would make the scenarios and the
// teardown look for the wrong things, and find nothing.
func TestLibAgreesWithTheDriver(t *testing.T) {
	const cluster, claim = "scarpenter-ci-1234567890", "default-abc12"
	if got, want := libValue(t, "vm_name_of "+claim), azure.VMName(cluster, claim); got != want {
		t.Errorf("vm_name_of = %q, the driver's VMName = %q", got, want)
	}
	if got, want := libValue(t, `echo "${ctag}"`), azure.TagKey(vendors.TagCluster); got != want {
		t.Errorf("the cluster tag is %q in lib.sh and %q in the driver", got, want)
	}
	for _, k := range []vendors.TagKey{vendors.TagNodeClaim, vendors.TagNodePool, vendors.TagNodeClass, vendors.TagManagedBy, vendors.TagResource} {
		if !strings.HasPrefix(azure.TagKey(k), libValue(t, `echo "${tag_prefix}"`)) {
			t.Errorf("the tag %q does not start with lib.sh's tag_prefix", azure.TagKey(k))
		}
	}
	// Every logical tag the scripts read (${tag_prefix}<key>) is one the driver writes.
	known := map[string]bool{string(vendors.TagResource): true}
	for _, k := range vendors.TagKeys {
		known[string(k)] = true
	}
	scripts, err := filepath.Glob(filepath.Join(testutil.RepoRoot(), "test", "e2e-azure", "*.sh"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no scripts: %v", err)
	}
	used := 0
	for _, f := range scripts {
		raw, _ := os.ReadFile(f)
		for _, m := range regexp.MustCompile(`\$\{tag_prefix\}([a-z-]+)`).FindAllStringSubmatch(string(raw), -1) {
			used++
			if !known[m[1]] {
				t.Errorf("%s reads the tag %s%s, which is not a logical tag key of the driver", filepath.Base(f), "scarpenter-", m[1])
			}
		}
	}
	if used == 0 {
		t.Error("no script reads a logical tag: the check looks at nothing")
	}
	for variable, want := range map[string]string{
		"bucket_cores": azure.BucketRegionalCores, "bucket_spot": azure.BucketSpotCores, "bucket_pip": azure.BucketPublicIPs,
	} {
		if got := libValue(t, `echo "${`+variable+`}"`); got != want {
			t.Errorf("%s is %q in lib.sh and %q in the driver", variable, got, want)
		}
	}
	if got, want := libValue(t, `echo "${created_tag}"`), "scarpenter-ci-created-at"; got != want || got != createdAtTag {
		t.Errorf("created_tag is %q, want %q", got, want)
	}
	if string(azure.KindPlacement) != "proximityPlacementGroups" {
		t.Errorf("the db scenario expects the resource kind proximityPlacementGroups, the driver's is %q", azure.KindPlacement)
	}
}
