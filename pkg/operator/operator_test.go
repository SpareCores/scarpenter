/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
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

	"github.com/awslabs/operatorpkg/status"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/operator"
	"github.com/SpareCores/scarpenter/pkg/vendors"
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
	file := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, file); err != nil {
		t.Fatal(err)
	}
	return file
}

func object(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
		t.Fatal(err)
	}
	return obj
}

// TestManagerStartsWithTheProductionWiring runs the production wiring (core's
// operator, the components, core's controllers, the CloudProvider) against envtest and
// checks that the manager becomes ready and that core resolves the SCNodeClass
// through GetSupportedNodeClasses: a NodePool that references a ready
// SCNodeClass becomes Ready.
func TestManagerStartsWithTheProductionWiring(t *testing.T) {
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
	t.Setenv("SCARPENTER_VENDORS", stubVendor)
	t.Setenv("SCARPENTER_KEEPER_URL", catalogtest.Server(t).URL)
	t.Setenv("SCARPENTER_KEEPER_PRICES_REGIONS_PER_REQUEST", "1")
	// The node bootstrap: a release server that publishes every release (the
	// envtest API server is v1.36, and the kubelet must not be newer), and the
	// cluster CA from a file (envtest has no kube-root-ca.crt ConfigMap).
	t.Setenv("SCARPENTER_SYSEXT_BASE_URL", releaseServer(t).URL)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, []byte("-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCARPENTER_CLUSTER_CA_FILE", caFile)
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
		components, err := op.NewComponents(runCtx)
		if err != nil {
			t.Error(err)
			return
		}
		if err := op.WithComponentControllers(runCtx, components); err != nil {
			t.Error(err)
			return
		}
		op.WithCoreControllers(runCtx, op.NewCloudProvider(components)).Start(runCtx)
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
	waitFor(t, "readyz", func() error {
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
  kubernetes: {version: "v1.36.0"}
  network: {template: direct}
  vendors: {aws: {regions: [us-east-1]}}
`)
	if err := c.Create(ctx, nodeClass); err != nil {
		t.Fatal(err)
	}
	// The NodeClass controller of the same manager makes it Ready: it resolves the
	// sysext digests from the release server of the test, validates the NodeClass
	// and reports the vendor.
	waitFor(t, "SCNodeClass Ready", func() error {
		nc := &v1alpha1.SCNodeClass{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(nodeClass), nc); err != nil {
			return err
		}
		if !nc.StatusConditions().IsTrue(status.ConditionReady) {
			return fmt.Errorf("not Ready: %+v", nc.Status.Conditions)
		}
		if nc.Status.Resolved == nil || nc.Status.Vendors[stubVendor].Hash == "" {
			return fmt.Errorf("no resolved artifacts or hash yet: %+v", nc.Status)
		}
		return nil
	})
	nodePool := object(t, `
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: default}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: default}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [aws]}
`)
	if err := c.Create(ctx, nodePool); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "NodePool Ready", func() error {
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
        - {key: scarpenter.sparecores.com/vendr, operator: In, values: [aws]}
`)
	if err := c.Create(ctx, typo); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "NodePool ValidationSucceeded=False", func() error {
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

// releaseServer serves a SHA256SUMS for every sysext release, like the
// sysext-bakery would.
func releaseServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release := path.Base(path.Dir(r.URL.Path))
		if path.Base(r.URL.Path) != "SHA256SUMS" {
			http.NotFound(w, r)
			return
		}
		for _, arch := range []string{"x86-64", "arm64"} {
			sum := sha256.Sum256([]byte(release + arch))
			_, _ = fmt.Fprintf(w, "%x  %s-%s.raw\n", sum, release, arch)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitFor polls check for up to a minute.
func waitFor(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", what, err)
}

// stubVendor is the registration name and vendor ID of the test driver; the
// NodeClass of the smoke test uses it as its vendor block.
const stubVendor = "aws"

func init() {
	vendors.Register(stubVendor, func() vendors.Driver { return &stubDriver{} })
}

// stubDriver serves a region without zones and launches nothing.
type stubDriver struct{}

func (*stubDriver) Vendor() string                                        { return stubVendor }
func (*stubDriver) Init(context.Context, vendors.Config) error            { return nil }
func (*stubDriver) TagKey(k vendors.TagKey) string                        { return vendors.DefaultTagKey(k) }
func (*stubDriver) Regions(context.Context) ([]vendors.Region, error)     { return nil, nil }
func (*stubDriver) Zones(context.Context, string) ([]vendors.Zone, error) { return nil, nil }
func (*stubDriver) ResolveImage(context.Context, vendors.ImageRequest) (*vendors.Image, error) {
	return &vendors.Image{Ref: "image"}, nil
}
func (*stubDriver) CreateInstance(context.Context, vendors.CreateRequest) (*vendors.Instance, error) {
	return nil, vendors.Errorf(vendors.Fatal, "the stub launches nothing")
}
func (*stubDriver) GetInstance(context.Context, vendors.InstanceRef) (*vendors.Instance, error) {
	return nil, vendors.Errorf(vendors.NotFound, "no such instance")
}
func (*stubDriver) FindInstance(context.Context, string, map[vendors.TagKey]string) (*vendors.Instance, error) {
	return nil, vendors.Errorf(vendors.NotFound, "no such instance")
}
func (*stubDriver) ListInstances(context.Context, []string, map[vendors.TagKey]string) ([]*vendors.Instance, error) {
	return nil, nil
}
func (*stubDriver) DeleteInstance(context.Context, vendors.InstanceRef) error { return nil }
