/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"
	"sigs.k8s.io/yaml"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/operator"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// writeKubeconfig lets core's operator, which builds its client from the
// environment, find the envtest API server.
func writeKubeconfig(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	kc.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData, Token: cfg.BearerToken}
	kc.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	kc.CurrentContext = "envtest"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func object(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
		t.Fatal(err)
	}
	return obj
}

// TestManagerStartsWithTheStubProvider runs the production wiring (core's
// operator, core's controllers, the stub CloudProvider) against envtest and
// checks that the manager becomes ready and that core resolves the SCNodeClass
// through GetSupportedNodeClasses: a NodePool that references a ready
// SCNodeClass becomes Ready.
func TestManagerStartsWithTheStubProvider(t *testing.T) {
	cfg, stop, err := testutil.StartEnvWith(testutil.EnvOptions{CRDs: testutil.KarpenterCRDs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})

	healthPort := freePort(t)
	t.Setenv("KUBECONFIG", writeKubeconfig(t, cfg))
	t.Setenv("METRICS_PORT", fmt.Sprint(freePort(t)))
	t.Setenv("HEALTH_PROBE_PORT", fmt.Sprint(healthPort))
	t.Setenv("DISABLE_LEADER_ELECTION", "true")
	t.Setenv("SCARPENTER_CLUSTER_NAME", "my-cluster")
	t.Setenv("SCARPENTER_API_SERVER_ENDPOINT", "https://10.0.0.1:6443")
	// Core parses os.Args[1:] as its flags; the test binary's own flags
	// would be rejected.
	args := os.Args
	os.Args = []string{"scarpenter"}
	t.Cleanup(func() { os.Args = args })

	ctx, op := operator.NewOperator(coreoperator.NewOperator())
	done := make(chan struct{})
	runCtx, cancelRun := context.WithCancel(ctx)
	go func() {
		defer close(done)
		op.WithCoreControllers(runCtx, cloudprovider.New()).Start(runCtx)
	}()
	t.Cleanup(func() {
		cancelRun()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the manager did not stop")
		}
	})

	if op.Options.ClusterName != "my-cluster" {
		t.Errorf("cluster name = %q", op.Options.ClusterName)
	}

	// The manager is ready once its caches are synced and core's CRDs exist.
	readyz := fmt.Sprintf("http://127.0.0.1:%d/readyz", healthPort)
	waitFor(t, 60*time.Second, "readyz", func() error {
		resp, err := http.Get(readyz) //nolint:noctx // test helper polling a local port
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("readyz returned %s", resp.Status)
		}
		return nil
	})

	// A client that knows both APIs.
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	nodeClass := object(t, `
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: default}
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network: {template: direct}
  vendors: {fake: {}}
`)
	if err := c.Create(ctx, nodeClass); err != nil {
		t.Fatal(err)
	}
	// Stand in for the NodeClass controller (a later plan). Ready is derived
	// from its dependents by operatorpkg, so they must all be true.
	conditions := make([]any, 0, 3)
	for _, typ := range []string{"Ready", "CatalogReady", "ArtifactsReady"} {
		conditions = append(conditions, map[string]any{
			"type": typ, "status": "True", "reason": typ, "message": "",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			"observedGeneration": nodeClass.GetGeneration(),
		})
	}
	nodeClass.Object["status"] = map[string]any{"conditions": conditions}
	if err := c.Status().Update(ctx, nodeClass); err != nil {
		t.Fatal(err)
	}
	nodePool := object(t, `
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: default}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: default}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [fake]}
`)
	if err := c.Create(ctx, nodePool); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "NodePool Ready", func() error {
		np := &karpv1.NodePool{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(nodePool), np); err != nil {
			return err
		}
		if !np.StatusConditions().IsTrue(karpv1.ConditionTypeNodeClassReady) {
			return fmt.Errorf("NodeClassReady is not true: %+v", np.Status.Conditions)
		}
		// Core's runtime validation accepts our well-known labels.
		if !np.StatusConditions().IsTrue(karpv1.ConditionTypeValidationSucceeded) {
			return fmt.Errorf("ValidationSucceeded is not true: %+v", np.Status.Conditions)
		}
		return nil
	})

	// A typo in the restricted domain is rejected by the same validation.
	typo := object(t, `
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: typo}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: default}
      requirements:
        - {key: scarpenter.sparecores.com/vendr, operator: In, values: [fake]}
`)
	if err := c.Create(ctx, typo); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "NodePool ValidationSucceeded=False", func() error {
		np := &karpv1.NodePool{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(typo), np); err != nil {
			return err
		}
		if cond := np.StatusConditions().Get(karpv1.ConditionTypeValidationSucceeded); cond == nil || cond.Status != metav1.ConditionFalse {
			return fmt.Errorf("ValidationSucceeded is not false: %+v", np.Status.Conditions)
		}
		return nil
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", what, err)
}
