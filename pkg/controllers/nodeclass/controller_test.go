/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package nodeclass_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/SpareCores/scarpenter/internal/testutil"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
	"github.com/SpareCores/scarpenter/pkg/artifacts"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// driver is an "aws" vendor with the optional capabilities the controller uses.
type driver struct {
	mu          sync.Mutex
	validateErr error
	images      []v1alpha1.ImageStatus
	imagesErr   error
	warnings    []string
	// hardenings are the hardenings the driver offers, fragments what it
	// contributes for them.
	hardenings []string
	fragments  []vendors.BootFragment
}

func (d *driver) Hardenings() []string { return d.hardenings }
func (d *driver) BootFragments(vendors.BootFragmentRequest) ([]vendors.BootFragment, error) {
	return d.fragments, nil
}

func (d *driver) Vendor() string                                    { return "aws" }
func (d *driver) Init(context.Context, vendors.Config) error        { return nil }
func (d *driver) Regions(context.Context) ([]vendors.Region, error) { return nil, nil }
func (d *driver) Zones(context.Context, string) ([]vendors.Zone, error) {
	return nil, nil
}
func (d *driver) ResolveImage(context.Context, vendors.ImageRequest) (*vendors.Image, error) {
	return nil, nil
}
func (d *driver) CreateInstance(context.Context, vendors.CreateRequest) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) GetInstance(context.Context, vendors.InstanceRef) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) FindInstance(context.Context, string, map[vendors.TagKey]string) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) ListInstances(context.Context, []string, map[vendors.TagKey]string) ([]*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) DeleteInstance(context.Context, vendors.InstanceRef) error { return nil }
func (d *driver) TagKey(k vendors.TagKey) string                            { return vendors.DefaultTagKey(k) }
func (d *driver) ValidateNodeClass(context.Context, *v1alpha1.SCNodeClass) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.validateErr
}
func (d *driver) NodeClassWarnings(*v1alpha1.SCNodeClass) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.warnings
}
func (d *driver) ReportImages(context.Context, *v1alpha1.SCNodeClass) ([]v1alpha1.ImageStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.images, d.imagesErr
}

// refs serves ConfigMap and Secret keys from memory.
type refs struct {
	mu sync.Mutex
	// failWith makes every read fail with this error.
	failWith error
	data     map[string][2]string // "kind/ns/name/key" -> value, resourceVersion
}

func (r *refs) set(kind, ns, name, key, value, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data == nil {
		r.data = map[string][2]string{}
	}
	r.data[kind+"/"+ns+"/"+name+"/"+key] = [2]string{value, version}
}

func (r *refs) get(kind, ns, name, key string) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return "", "", r.failWith
	}
	v, ok := r.data[kind+"/"+ns+"/"+name+"/"+key]
	if !ok {
		return "", "", fmt.Errorf("the %s %s/%s has no key %q: %w", kind, ns, name, key, bootstrap.ErrNotFound)
	}
	return v[0], v[1], nil
}

func (r *refs) ConfigMapKey(_ context.Context, ref v1alpha1.ConfigMapKeyRef) (string, string, error) {
	return r.get("cm", ref.Namespace, ref.Name, ref.Key)
}

func (r *refs) SecretKey(_ context.Context, ref v1alpha1.SecretKeyRef) (string, string, error) {
	return r.get("secret", ref.Namespace, ref.Name, ref.Key)
}

func (r *refs) ClusterCA(context.Context) (string, error) {
	return "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n", nil
}

type recorder struct {
	mu   sync.Mutex
	list []events.Event
}

func (r *recorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, evts...)
}

func (r *recorder) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.list))
	for _, e := range r.list {
		out = append(out, e.Reason)
	}
	return out
}

type env struct {
	t       *testing.T
	c       client.Client
	ctrl    *nodeclass.Controller
	drv     *driver
	refs    *refs
	rec     *recorder
	server  string
	cat     *catalog.Catalog
	version string
	// name is the NodeClass of the test.
	name string
	// failWith is the status the release server of a test answers with.
	failWith int
}

const testRegion = "us-east-1"

var (
	envOnce sync.Once
	envCfg  *rest.Config
	envStop func() error
	envErr  error
)

// TestMain stops the shared API server; the tests use NodeClasses of their own
// names.
func TestMain(m *testing.M) {
	code := m.Run()
	if envStop != nil {
		_ = envStop()
	}
	os.Exit(code)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	envOnce.Do(func() { envCfg, envStop, envErr = testutil.StartEnv() })
	if envErr != nil {
		t.Fatal(envErr)
	}
	cfg := envCfg
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	// The release server serves the real SHA256SUMS fixtures.
	srv := httptest.NewServer(http.FileServer(http.Dir("../../artifacts/testdata")))
	t.Cleanup(srv.Close)

	e := &env{t: t, c: c, name: strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())), drv: &driver{}, refs: &refs{}, rec: &recorder{}, server: srv.URL, version: "v1.37.1"}
	e.cat = catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws")})
	reg := vendors.NewRegistryFrom(e.drv)
	resolver := artifacts.New(artifacts.Config{SysextBase: srv.URL})
	renderer := bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: e.refs, APIServerEndpoint: "https://api.example.com:6443"})
	e.ctrl = nodeclass.New(nodeclass.Options{
		Client: c, Catalog: e.cat, Registry: reg, Artifacts: resolver, Renderer: renderer, Refs: e.refs,
		ServerVersion: func(context.Context) (string, error) { return e.version, nil },
		Recorder:      e.rec,
	})
	return e
}

func (e *env) newNodeClass(mutate func(*v1alpha1.SCNodeClass)) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: e.name},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors:    v1alpha1.VendorBlocks{AWS: &aws.Settings{Regions: []string{testRegion}}},
		},
	}
	if mutate != nil {
		mutate(nc)
	}
	return nc
}

// create stores the NodeClass and gives the catalog a snapshot for it.
func (e *env) create(nc *v1alpha1.SCNodeClass) {
	e.t.Helper()
	if err := e.c.Create(e.t.Context(), nc); err != nil {
		e.t.Fatal(err)
	}
	// The recorded Keeper snapshot has no qemu data: the catalog sees the
	// aws part only.
	forCatalog := nc.DeepCopy()
	forCatalog.Spec.Vendors.QEMU = nil
	class, err := catalog.ClassFromNodeClass(forCatalog)
	if err != nil {
		e.t.Fatal(err)
	}
	e.cat.SetClasses([]catalog.Class{class})
	if err := e.cat.Refresh(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) reconcile(name string) *v1alpha1.SCNodeClass {
	e.t.Helper()
	res, err := e.ctrl.Reconcile(e.t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: name}})
	if err != nil {
		e.t.Fatal(err)
	}
	if res.RequeueAfter <= 0 {
		e.t.Errorf("no requeue: the catalog and the references change without NodeClass events")
	}
	return e.get(name)
}

func (e *env) get(name string) *v1alpha1.SCNodeClass {
	e.t.Helper()
	nc := &v1alpha1.SCNodeClass{}
	if err := e.c.Get(e.t.Context(), client.ObjectKey{Name: name}, nc); err != nil {
		e.t.Fatal(err)
	}
	return nc
}

func cond(nc *v1alpha1.SCNodeClass, typ string) *status.Condition {
	for i := range nc.Status.Conditions {
		if nc.Status.Conditions[i].Type == typ {
			return &nc.Status.Conditions[i]
		}
	}
	return nil
}

func vcond(nc *v1alpha1.SCNodeClass, vendor, typ string) *status.Condition {
	for i, c := range nc.Status.Vendors[vendor].Conditions {
		if c.Type == typ {
			return &nc.Status.Vendors[vendor].Conditions[i]
		}
	}
	return nil
}

func wantCond(t *testing.T, nc *v1alpha1.SCNodeClass, typ string, st metav1.ConditionStatus, reason string) {
	t.Helper()
	c := cond(nc, typ)
	if c == nil || c.Status != st || (reason != "" && c.Reason != reason) {
		t.Errorf("condition %s = %+v, want %s %s", typ, c, st, reason)
	}
}

func TestReadyNodeClass(t *testing.T) {
	e := newEnv(t)
	e.drv.images = []v1alpha1.ImageStatus{{Region: testRegion, Arch: "amd64", Ref: "ami-0123456789abcdef0", Ready: true}}
	e.create(e.newNodeClass(nil))
	nc := e.reconcile(e.name)

	for _, typ := range []string{status.ConditionReady, v1alpha1.ConditionTypeCatalogReady, v1alpha1.ConditionTypeArtifactsReady,
		v1alpha1.ConditionTypeValidationSucceeded} {
		wantCond(t, nc, typ, metav1.ConditionTrue, "")
		if c := cond(nc, typ); c != nil && c.ObservedGeneration != nc.Generation {
			t.Errorf("%s observedGeneration = %d, want %d", typ, c.ObservedGeneration, nc.Generation)
		}
	}
	if got := strings.Join(nc.Status.VendorNames, ","); got != "aws" {
		t.Errorf("vendorNames = %q", got)
	}
	r := nc.Status.Resolved
	if r == nil || r.Kubernetes["amd64"].SHA256 != "75072a32a1d87cfbe67907a0f6ebe3f0a860c2b2e15dbf1d8a354d5162e63b3b" ||
		r.Kubernetes["arm64"].SHA256 != "2e2a6faf04230c6b719656dbea20b76f1fbf9a5b3e61c0787c0850f052e40f6b" ||
		!strings.HasSuffix(r.Kubernetes["amd64"].URL, "/kubernetes-v1.37.1/kubernetes-v1.37.1-x86-64.raw") || len(r.Tailscale) != 0 {
		t.Errorf("resolved = %+v", r)
	}
	vs := nc.Status.Vendors["aws"]
	if vs.Hash == "" {
		t.Error("no drift hash")
	}
	wantCond(t, &v1alpha1.SCNodeClass{Status: v1alpha1.SCNodeClassStatus{Conditions: vs.Conditions}}, v1alpha1.VendorConditionReady, metav1.ConditionTrue, "")
	if c := vcond(nc, "aws", v1alpha1.VendorConditionImagesReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ImagesReady = %+v", c)
	}
	if len(vs.Images) != 1 || vs.Images[0].Ref != "ami-0123456789abcdef0" {
		t.Errorf("images = %+v", vs.Images)
	}
	// Ready is what core and Create read: the NodeClass type's own condition set
	// must agree with what was written.
	if !nc.StatusConditions().Root().IsTrue() {
		t.Errorf("the root condition is %+v", nc.StatusConditions().Root())
	}

	// A second reconcile with nothing changed writes nothing.
	again := e.reconcile(e.name)
	if again.ResourceVersion != nc.ResourceVersion {
		t.Errorf("an unchanged status was written again (%s -> %s)", nc.ResourceVersion, again.ResourceVersion)
	}
	// The hash follows the spec.
	nc.Spec.Kubelet.MaxPods = new(int32(50))
	if err := e.c.Update(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	changed := e.reconcile(e.name)
	if changed.Status.Vendors["aws"].Hash == vs.Hash {
		t.Error("the hash did not change with kubelet.maxPods")
	}
	if c := cond(changed, status.ConditionReady); c == nil || c.ObservedGeneration != changed.Generation {
		t.Errorf("Ready = %+v, want observedGeneration %d", c, changed.Generation)
	}
}

// The quota status controller writes its own fields under its own manager; the
// NodeClass controller must neither remove them nor be blocked by them.
func TestCoexistsWithTheQuotaStatusWriter(t *testing.T) {
	e := newEnv(t)
	e.create(e.newNodeClass(nil))
	nc := e.reconcile(e.name)

	quota := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(), "kind": "SCNodeClass",
		"metadata": map[string]any{"name": e.name},
		"status": map[string]any{"vendors": map[string]any{"aws": map[string]any{
			"conditions": []any{map[string]any{
				"type": "QuotaAvailable", "status": "False", "reason": "QuotaExhausted", "message": "m",
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339), "observedGeneration": nc.Generation,
			}},
			"quota": []any{map[string]any{"region": testRegion, "capacityType": "spot", "bucket": "b", "used": int64(1), "limit": int64(1)}},
		}}},
	}}
	if err := e.c.SubResource("status").Apply(t.Context(), client.ApplyConfigurationFromUnstructured(quota),
		client.FieldOwner("scarpenter-quota"), client.ForceOwnership); err != nil {
		t.Fatal(err)
	}

	// Force a write by this controller: a new generation.
	nc = e.get(e.name)
	nc.Spec.Kubelet.MaxPods = new(int32(60))
	if err := e.c.Update(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	got := e.reconcile(e.name)
	if c := vcond(got, "aws", v1alpha1.VendorConditionQuotaAvailable); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("the quota condition was lost: %+v", got.Status.Vendors["aws"].Conditions)
	}
	if len(got.Status.Vendors["aws"].Quota) != 1 {
		t.Errorf("the quota entries were lost: %+v", got.Status.Vendors["aws"].Quota)
	}
	if c := vcond(got, "aws", v1alpha1.VendorConditionReady); c == nil || c.ObservedGeneration != got.Generation {
		t.Errorf("VendorReady was not refreshed: %+v", c)
	}
	managers := map[string]bool{}
	for _, mf := range got.ManagedFields {
		if mf.Subresource == "status" {
			managers[mf.Manager] = true
		}
	}
	if !managers[nodeclass.FieldManager] || !managers["scarpenter-quota"] {
		t.Errorf("status managers = %v", managers)
	}
}

func TestArtifactsNotFound(t *testing.T) {
	e := newEnv(t)
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubernetes.Version = "v1.37.9" }))
	nc := e.reconcile(e.name)
	wantCond(t, nc, v1alpha1.ConditionTypeArtifactsReady, metav1.ConditionFalse, nodeclass.ReasonReleaseNotFound)
	wantCond(t, nc, status.ConditionReady, metav1.ConditionFalse, "")
	if nc.Status.Resolved != nil {
		t.Errorf("resolved = %+v for an unpublished release", nc.Status.Resolved)
	}
	if !contains(e.rec.reasons(), nodeclass.EventArtifactsFailed) {
		t.Errorf("events = %v", e.rec.reasons())
	}
}

func TestTailscaleArtifactsAndAuthKeyHash(t *testing.T) {
	e := newEnv(t)
	e.refs.set("secret", "scarpenter", "ts-node-key", "authkey", "tskey-auth-EXAMPLE", "100")
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
			Version: "v1.98.10", Tags: []string{"tag:k8s-node"},
			AuthKeySecretRef: v1alpha1.SecretKeyRef{Name: "ts-node-key", Namespace: "scarpenter", Key: "authkey"},
		}}
	}))
	nc := e.reconcile(e.name)
	wantCond(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
	if got := nc.Status.Resolved.Tailscale["arm64"].SHA256; got != "580eade1a34f60161427f17d11c14581164c02133811670a5f554f4ff05d7ce6" {
		t.Errorf("tailscale arm64 sha256 = %s", got)
	}
	h1 := nc.Status.Vendors["aws"].Hash
	// Rotating the Secret (a new resourceVersion) drifts the nodes; the key
	// itself is not part of the hash.
	e.refs.set("secret", "scarpenter", "ts-node-key", "authkey", "tskey-auth-ROTATED", "101")
	h2 := e.reconcile(e.name).Status.Vendors["aws"].Hash
	if h2 == h1 {
		t.Error("rotating the auth key Secret did not change the hash")
	}
	// A missing Secret is a validation failure that names it.
	e.refs.data = nil
	nc = e.reconcile(e.name)
	wantCond(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonReferenceNotFound)
	if c := cond(nc, v1alpha1.ConditionTypeValidationSucceeded); c == nil || !strings.Contains(c.Message, "ts-node-key") {
		t.Errorf("the message does not name the Secret: %+v", c)
	}
}

func TestKubernetesVersionSkew(t *testing.T) {
	tests := []struct {
		server string
		ok     bool
	}{
		{"v1.37.1", true},
		{"v1.37.0", true}, // a patch release older than the kubelet is fine: only minors count
		{"v1.38.4", true},
		{"v1.40.0", true}, // three minors
		{"v1.41.0", false},
		{"v1.36.5", false}, // the kubelet must not be newer
		{"v1.37.2+k3s1", true},
		{"v2.37.1", false},
	}
	for _, tc := range tests {
		t.Run(tc.server, func(t *testing.T) {
			err := nodeclass.CheckSkew("v1.37.1", tc.server)
			if (err == nil) != tc.ok {
				t.Errorf("CheckSkew(v1.37.1, %s) = %v, want ok=%v", tc.server, err, tc.ok)
			}
		})
	}
	if err := nodeclass.CheckSkew("nope", "v1.37.1"); err == nil {
		t.Error("an invalid kubelet version was accepted")
	}

	e := newEnv(t)
	e.version = "v1.36.2"
	e.create(e.newNodeClass(nil))
	nc := e.reconcile(e.name)
	wantCond(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonVersionSkew)
	wantCond(t, nc, status.ConditionReady, metav1.ConditionFalse, "")
	if c := cond(nc, v1alpha1.ConditionTypeValidationSucceeded); c == nil || !strings.Contains(c.Message, "must not be newer") {
		t.Errorf("message = %+v", c)
	}
}

func TestInvalidKubeletSettings(t *testing.T) {
	e := newEnv(t)
	// The CRD does not check quantities: the controller does.
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubelet.SystemReserved = map[string]string{"cpu": "100m"} }))
	nc := e.get(e.name)
	nc.Spec.Kubelet.SystemReserved = map[string]string{"cpu": "lots"}
	if err := e.c.Update(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	got := e.reconcile(e.name)
	wantCond(t, got, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonInvalidKubelet)
}

func TestTemplatesAreValidated(t *testing.T) {
	e := newEnv(t)
	e.refs.set("cm", "scarpenter", "net", "net.bu", "variant: flatcar\nversion: 1.1.0\n# {{ .Nope }}\n", "1")
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateCustom, Custom: &v1alpha1.CustomNetworkSpec{
			ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "net", Namespace: "scarpenter", Key: "net.bu"}}}
	}))
	nc := e.reconcile(e.name)
	wantCond(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonTemplateInvalid)
	if c := cond(nc, v1alpha1.ConditionTypeValidationSucceeded); c == nil || !strings.Contains(c.Message, "Nope") {
		t.Errorf("the message does not say what is wrong: %+v", c)
	}
	// Fixing the ConfigMap fixes the NodeClass, and changes the hash.
	before := nc.Status.Vendors["aws"].Hash
	e.refs.set("cm", "scarpenter", "net", "net.bu", "variant: flatcar\nversion: 1.1.0\n", "2")
	fixed := e.reconcile(e.name)
	wantCond(t, fixed, status.ConditionReady, metav1.ConditionTrue, "")
	if fixed.Status.Vendors["aws"].Hash == before {
		t.Error("the hash did not change with the template")
	}
	// A missing ConfigMap key.
	e.refs.data = nil
	missing := e.reconcile(e.name)
	wantCond(t, missing, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonReferenceNotFound)
}

func TestVendorReadiness(t *testing.T) {
	e := newEnv(t)
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.QEMU = &qemu.Settings{} }))

	// A vendor without a driver is not ready; the NodeClass and the other vendor
	// are (overview section 6.2): it only makes its own offerings unavailable.
	nc := e.reconcile(e.name)
	wantCond(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
	if c := vcond(nc, "aws", v1alpha1.VendorConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("aws VendorReady = %+v", c)
	}
	c := vcond(nc, "qemu", v1alpha1.VendorConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != nodeclass.ReasonVendorNotEnabled {
		t.Errorf("qemu VendorReady = %+v", c)
	}
	if nc.Status.Vendors["qemu"].Hash == "" || nc.Status.Vendors["qemu"].Hash == nc.Status.Vendors["aws"].Hash {
		t.Error("each vendor has its own hash")
	}
	if !contains(e.rec.reasons(), nodeclass.EventVendorNotReady) {
		t.Errorf("events = %v", e.rec.reasons())
	}

	// The driver's own validation.
	e.drv.validateErr = errors.New("root volume of 1 GiB is below the minimum")
	nc = e.reconcile(e.name)
	c = vcond(nc, "aws", v1alpha1.VendorConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != nodeclass.ReasonVendorInvalid || !strings.Contains(c.Message, "root volume") {
		t.Errorf("aws VendorReady = %+v", c)
	}
	wantCond(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
}

func TestImagesReportedByTheDriver(t *testing.T) {
	e := newEnv(t)
	e.drv.images = []v1alpha1.ImageStatus{
		{Region: testRegion, Arch: "amd64", Ref: "ami-1", Ready: true},
		{Region: testRegion, Arch: "arm64", Ready: false, Message: "no image"},
	}
	e.create(e.newNodeClass(nil))
	nc := e.reconcile(e.name)
	// A region without an image has unavailable offerings, not a NodeClass that
	// is not ready.
	wantCond(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
	c := vcond(nc, "aws", v1alpha1.VendorConditionImagesReady)
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, testRegion+"/arm64") {
		t.Errorf("ImagesReady = %+v", c)
	}
	// A lookup that fails is no verdict: the condition and the images of the last
	// good answer stay.
	e.drv.imagesErr = errors.New("describing images: throttled")
	nc = e.reconcile(e.name)
	c = vcond(nc, "aws", v1alpha1.VendorConditionImagesReady)
	if c == nil || c.Reason == nodeclass.ReasonImagesFailed || !strings.Contains(c.Message, testRegion+"/arm64") {
		t.Errorf("ImagesReady after a failed lookup = %+v, want the previous verdict", c)
	}
	if len(nc.Status.Vendors["aws"].Images) != 2 {
		t.Errorf("images after a failed lookup: %+v, want the previous two", nc.Status.Vendors["aws"].Images)
	}
}

func TestAFailedImageLookupWithoutAPreviousVerdictIsReported(t *testing.T) {
	e := newEnv(t)
	e.drv.imagesErr = errors.New("describing images: throttled")
	e.create(e.newNodeClass(nil))
	c := vcond(e.reconcile(e.name), "aws", v1alpha1.VendorConditionImagesReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != nodeclass.ReasonImagesFailed {
		t.Errorf("ImagesReady = %+v", c)
	}
}

type gateRecorder struct {
	mu   sync.Mutex
	gaps map[string]map[string]string
}

func (g *gateRecorder) SetImageGaps(class, vendor string, gaps map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gaps == nil {
		g.gaps = map[string]map[string]string{}
	}
	g.gaps[class+"/"+vendor] = gaps
}

// Without a region list the NodeClass covers every region of the vendor: a region
// without an image costs its offerings, not the condition, unless no region has
// one. A region the NodeClass names must have its images.
func TestImagesReadyWithGapsInRegionsTheNodeClassDoesNotName(t *testing.T) {
	e := newEnv(t)
	gate := &gateRecorder{}
	clk := clocktesting.NewFakeClock(time.Now())
	e.ctrl = nodeclass.New(nodeclass.Options{
		Client: e.c, Catalog: e.cat, Registry: vendors.NewRegistryFrom(e.drv), ImageGate: gate,
		Artifacts: artifacts.New(artifacts.Config{SysextBase: e.server, Clock: clk}),
		Renderer:  bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: e.refs, APIServerEndpoint: "https://api.example.com:6443"}),
		Refs:      e.refs, Recorder: e.rec,
	})
	e.drv.images = []v1alpha1.ImageStatus{
		{Region: "r1", Arch: "amd64", Ref: "ami-1", Ready: true}, {Region: "r1", Arch: "arm64", Ref: "ami-2", Ready: true},
		{Region: "r2", Arch: "amd64", Message: "no image"}, {Region: "r2", Arch: "arm64", Message: "no image"},
		{Region: "r3", Arch: "amd64", Ref: "ami-3", Ready: true}, {Region: "r3", Arch: "arm64", Message: "no arm image"},
	}
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = nil }))
	c := vcond(e.reconcile(e.name), "aws", v1alpha1.VendorConditionImagesReady)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != nodeclass.ReasonImagesPartial ||
		!strings.Contains(c.Message, "r2/amd64") || !strings.Contains(c.Message, "r3/arm64") {
		t.Errorf("ImagesReady = %+v, want True naming the gaps", c)
	}
	// Only r2 has no image at all: that is the region whose offerings are gated.
	if g := gate.gaps[e.name+"/aws"]; len(g) != 1 || g["r2"] == "" {
		t.Errorf("image gaps = %v, want only r2", g)
	}
	// No region has an image.
	e.drv.images = []v1alpha1.ImageStatus{{Region: "r1", Arch: "amd64", Message: "none"}, {Region: "r2", Arch: "amd64", Message: "none"}}
	if c := vcond(e.reconcile(e.name), "aws", v1alpha1.VendorConditionImagesReady); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("no image anywhere: %+v", c)
	}
}

func TestCatalogNotReady(t *testing.T) {
	e := newEnv(t)
	nc := e.newNodeClass(nil)
	if err := e.c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	// The catalog has not read Keeper yet.
	got := e.reconcile(e.name)
	wantCond(t, got, v1alpha1.ConditionTypeCatalogReady, metav1.ConditionFalse, "")
	wantCond(t, got, status.ConditionReady, metav1.ConditionFalse, "")
	// The other conditions are independent of the catalog.
	wantCond(t, got, v1alpha1.ConditionTypeArtifactsReady, metav1.ConditionTrue, "")
	res, err := e.ctrl.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: e.name}})
	if err != nil || res.RequeueAfter > 30*time.Second {
		t.Errorf("a NodeClass that is not ready is looked at again soon, got %v, %v", res.RequeueAfter, err)
	}
}

// A Keeper that refuses the catalog's requests shows as KeeperAuthorized=False
// next to CatalogReady=False (KeeperAPIKeyRequired); a key that works makes it
// True. It is informational: Ready does not depend on it.
func TestKeeperAuthorizedCondition(t *testing.T) {
	e := newEnv(t)
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	h.RequireAPIKey("secret")
	srv := keepertest.NewServer(t, h)
	keyFile := filepath.Join(t.TempDir(), "key")
	controllerOver := func(cat *catalog.Catalog) *nodeclass.Controller {
		return nodeclass.New(nodeclass.Options{
			Client: e.c, Catalog: cat, Registry: vendors.NewRegistryFrom(e.drv),
			Artifacts: artifacts.New(artifacts.Config{SysextBase: e.server}),
			Renderer:  bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: e.refs, APIServerEndpoint: "https://api.example.com:6443"}),
			Refs:      e.refs, ServerVersion: func(context.Context) (string, error) { return e.version, nil }, Recorder: e.rec,
		})
	}
	nc := e.newNodeClass(nil)
	if err := e.c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	class, err := catalog.ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	// No key file configured.
	noKey := catalog.New(catalog.Options{
		Source: catalog.NewKeeperSource(catalog.KeeperOptions{URL: srv.URL}), Facts: catalogtest.Facts(t, "aws"),
	})
	noKey.SetClasses([]catalog.Class{class})
	if err := noKey.Refresh(t.Context()); err == nil {
		t.Fatal("the refresh succeeded without a key")
	}
	ctrl := controllerOver(noKey)
	if _, err := ctrl.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: e.name}}); err != nil {
		t.Fatal(err)
	}
	got := e.get(e.name)
	wantCond(t, got, v1alpha1.ConditionTypeCatalogReady, metav1.ConditionFalse, "KeeperAPIKeyRequired")
	wantCond(t, got, v1alpha1.ConditionTypeKeeperAuthorized, metav1.ConditionFalse, "APIKeyMissing")
	if c := cond(got, v1alpha1.ConditionTypeKeeperAuthorized); !strings.Contains(c.Message, "--keeper-api-key-file") {
		t.Errorf("message = %q", c.Message)
	}

	// A key that works.
	if err := os.WriteFile(keyFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withKey := catalog.New(catalog.Options{
		Source: catalog.NewKeeperSource(catalog.KeeperOptions{URL: srv.URL, APIKeyFile: keyFile}),
		Facts:  catalogtest.Facts(t, "aws"),
	})
	withKey.SetClasses([]catalog.Class{class})
	if err := withKey.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctrl = controllerOver(withKey)
	if _, err := ctrl.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: e.name}}); err != nil {
		t.Fatal(err)
	}
	got = e.get(e.name)
	wantCond(t, got, v1alpha1.ConditionTypeCatalogReady, metav1.ConditionTrue, "Ready")
	wantCond(t, got, v1alpha1.ConditionTypeKeeperAuthorized, metav1.ConditionTrue, "Authorized")
	if c := cond(got, v1alpha1.ConditionTypeCatalogReady); !strings.Contains(c.Message, "Keeper data version") {
		t.Errorf("CatalogReady message = %q", c.Message)
	}
}

func TestDeletedNodeClassIsIgnored(t *testing.T) {
	e := newEnv(t)
	if _, err := e.ctrl.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "gone"}}); err != nil {
		t.Fatal(err)
	}
}

func contains(list []string, want string) bool { return slices.Contains(list, want) }

// A release server that fails must not make the nodes drift: the hash covers the
// resolved digests, which stay in the status while resolving fails.
func TestFailingReleaseServerDoesNotChangeTheHash(t *testing.T) {
	e := newEnv(t)
	var fail atomic.Bool
	files := http.FileServer(http.Dir("../../artifacts/testdata"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case fail.Load() && e.failWith == http.StatusNotFound:
			http.NotFound(w, r)
		case fail.Load():
			http.Error(w, "boom", e.failWith)
		default:
			files.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	clk := clocktesting.NewFakeClock(time.Now())
	e.ctrl = nodeclass.New(nodeclass.Options{
		Client: e.c, Catalog: e.cat, Registry: vendors.NewRegistryFrom(e.drv),
		Artifacts: artifacts.New(artifacts.Config{SysextBase: srv.URL, Clock: clk, TTL: time.Minute, ErrorTTL: time.Second}),
		Renderer:  bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: e.refs, APIServerEndpoint: "https://api.example.com:6443"}),
		Refs:      e.refs, Recorder: e.rec,
	})
	e.create(e.newNodeClass(nil))
	ok := e.reconcile(e.name)
	wantCond(t, ok, status.ConditionReady, metav1.ConditionTrue, "")
	hash := ok.Status.Vendors["aws"].Hash

	for _, code := range []int{http.StatusBadGateway, http.StatusNotFound} {
		e.failWith = code
		fail.Store(true)
		clk.Step(2 * time.Minute) // the cached checksums expire
		got := e.reconcile(e.name)
		if got.Status.Vendors["aws"].Hash != hash {
			t.Errorf("a failing release server (%d) changed the drift hash", code)
		}
		if got.Status.Resolved == nil {
			t.Errorf("a failing release server (%d) dropped status.resolved", code)
		}
		if code == http.StatusBadGateway {
			// Only unreachable: the pinned digests are still good.
			wantCond(t, got, v1alpha1.ConditionTypeArtifactsReady, metav1.ConditionTrue, nodeclass.ReasonResolved)
			wantCond(t, got, status.ConditionReady, metav1.ConditionTrue, "")
		} else {
			// The release is gone: nodes could not download it.
			wantCond(t, got, v1alpha1.ConditionTypeArtifactsReady, metav1.ConditionFalse, nodeclass.ReasonReleaseNotFound)
			wantCond(t, got, status.ConditionReady, metav1.ConditionFalse, "")
		}
		fail.Store(false)
		clk.Step(2 * time.Minute)
		back := e.reconcile(e.name)
		wantCond(t, back, status.ConditionReady, metav1.ConditionTrue, "")
		if back.Status.Vendors["aws"].Hash != hash {
			t.Errorf("recovering from %d changed the drift hash", code)
		}
	}
}

// An input that cannot be loaded for a moment (a Secret deleted and re-created,
// an API error, a failing skew check) must not change the drift hash: core would
// mark every node Drifted and replace them.
func TestUnloadableInputsKeepTheHash(t *testing.T) {
	e := newEnv(t)
	e.refs.set("secret", "scarpenter", "ts-node-key", "authkey", "tskey-auth-EXAMPLE", "100")
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
			Version: "v1.98.10", AuthKeySecretRef: v1alpha1.SecretKeyRef{Name: "ts-node-key", Namespace: "scarpenter", Key: "authkey"},
		}}
	}))
	hash := e.reconcile(e.name).Status.Vendors["aws"].Hash
	if hash == "" {
		t.Fatal("no hash")
	}
	saved := e.refs.data
	// The server version is cached for five minutes: a fresh controller sees a
	// changed one.
	fresh := func() {
		e.ctrl = nodeclass.New(nodeclass.Options{
			Client: e.c, Catalog: e.cat, Registry: vendors.NewRegistryFrom(e.drv),
			Artifacts: artifacts.New(artifacts.Config{SysextBase: e.server}),
			Renderer:  bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: e.refs, APIServerEndpoint: "https://api.example.com:6443"}),
			Refs:      e.refs, ServerVersion: func(context.Context) (string, error) { return e.version, nil }, Recorder: e.rec,
		})
	}
	for name, tc := range map[string]struct {
		breakIt func()
		restore func()
		reason  string
	}{
		"the Secret is missing": {
			func() { e.refs.data = nil }, func() { e.refs.data = saved }, nodeclass.ReasonReferenceNotFound},
		"the API server fails": {
			func() { e.refs.failWith = errors.New("etcdserver: request timed out") }, func() { e.refs.failWith = nil },
			nodeclass.ReasonReferenceUnreadable},
		"the version skew check fails": {
			func() { e.version = "v1.36.0" }, func() { e.version = "v1.37.1" }, nodeclass.ReasonVersionSkew},
	} {
		tc.breakIt()
		fresh()
		got := e.reconcile(e.name)
		wantCond(t, got, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, tc.reason)
		if h := got.Status.Vendors["aws"].Hash; h != hash {
			t.Errorf("%s: the hash changed (%s -> %s)", name, hash, h)
		}
		tc.restore()
		fresh()
		back := e.reconcile(e.name)
		wantCond(t, back, status.ConditionReady, metav1.ConditionTrue, "")
		if h := back.Status.Vendors["aws"].Hash; h != hash {
			t.Errorf("%s: recovery gave another hash (%s -> %s)", name, hash, h)
		}
	}
}

// Switching to a mirror of the same release changes URLs, not nodes.
func TestMirrorSwitchKeepsTheHash(t *testing.T) {
	nc := base()
	a := &v1alpha1.ResolvedArtifacts{Kubernetes: map[string]v1alpha1.Artifact{"amd64": {URL: "https://a.example.com/k.raw", SHA256: "aa"}}}
	b := &v1alpha1.ResolvedArtifacts{Kubernetes: map[string]v1alpha1.Artifact{"amd64": {URL: "https://mirror.example.com/k.raw", SHA256: "aa"}}}
	ha, _ := nodeclass.Hash(nc, "aws", nodeclass.Inputs{Resolved: a})
	hb, _ := nodeclass.Hash(nc, "aws", nodeclass.Inputs{Resolved: b})
	if ha != hb {
		t.Error("the hash follows the artifact URL")
	}
}

func TestIPv6OnlyNodesWarnAboutTheDefaultSysextSource(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*v1alpha1.SCNodeClass)
		warn   bool
	}{
		"ipv6 only": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Networking.PublicIPv4, nc.Spec.Networking.PublicIPv6 = new(false), new(true)
		}, true},
		"default":    {nil, false},
		"dual stack": {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Networking.PublicIPv6 = new(true) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.create(e.newNodeClass(tc.mutate))
			e.reconcile(e.name)
			if got := contains(e.rec.reasons(), nodeclass.EventIPv6OnlyNeedsMirror); got != tc.warn {
				t.Errorf("warned = %v, want %v (events %v)", got, tc.warn, e.rec.reasons())
			}
		})
	}
}

// With no --cluster-dns the controller looks the DNS Service up; a NodeClass is
// not valid while there is none, and says what to set.
func TestClusterDNSMustBeKnown(t *testing.T) {
	e := newEnv(t)
	lookup := func(context.Context) ([]string, error) {
		return nil, errors.New("no cluster DNS Service found: set --cluster-dns (Helm value cluster.dns)")
	}
	e.ctrl = nodeclass.New(nodeclass.Options{
		Client: e.c, Catalog: e.cat, Registry: vendors.NewRegistryFrom(e.drv), Artifacts: artifacts.New(artifacts.Config{SysextBase: e.server}),
		Renderer: bootstrap.NewRenderer(bootstrap.RendererOptions{
			Refs: e.refs, APIServerEndpoint: "https://api.example.com:6443", ClusterDNSLookup: lookup}),
		Refs:          e.refs,
		ServerVersion: func(context.Context) (string, error) { return e.version, nil },
		Recorder:      e.rec,
	})
	e.create(e.newNodeClass(nil))
	nc := e.reconcile(e.name)
	wantCond(t, nc, v1alpha1.ConditionTypeValidationSucceeded, metav1.ConditionFalse, nodeclass.ReasonClusterDNSUnknown)
	if c := cond(nc, v1alpha1.ConditionTypeValidationSucceeded); c == nil || !strings.Contains(c.Message, "cluster.dns") {
		t.Errorf("the message does not name the Helm value: %+v", c)
	}
	wantCond(t, nc, status.ConditionReady, metav1.ConditionFalse, "")
}

// An image that is not the pinned release is a fault of what the NodeClass named,
// not a gap of the vendor: ImagesReady is False with the image's own reason, also
// for a region the NodeClass does not name, and the region's offerings are
// gated.
func TestAnImageOfAnotherVersionMakesTheImagesNotReady(t *testing.T) {
	for name, tc := range map[string]struct {
		regions []string
		reason  string
	}{
		"named region":   {[]string{"r1"}, vendors.CodeImageVersionMismatch},
		"unnamed region": {nil, vendors.CodeImageVersionUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			gate := &gateRecorder{}
			e.ctrl = nodeclass.New(nodeclass.Options{
				Client: e.c, Catalog: e.cat, Registry: vendors.NewRegistryFrom(e.drv), ImageGate: gate,
				Artifacts: artifacts.New(artifacts.Config{SysextBase: e.server}),
				Renderer:  bootstrap.NewRenderer(bootstrap.RendererOptions{Refs: e.refs, APIServerEndpoint: "https://api.example.com:6443"}),
				Refs:      e.refs, Recorder: e.rec,
			})
			msg := "the image ami-9 in r1 is Flatcar stable 4500.1.0, but stable 4757.2.3 is pinned"
			e.drv.images = []v1alpha1.ImageStatus{
				{Region: "r1", Arch: "amd64", Ref: "ami-1", Ready: true},
				{Region: "r1", Arch: "arm64", Message: msg, Reason: tc.reason},
				{Region: "r2", Arch: "amd64", Ref: "ami-2", Ready: true}, {Region: "r2", Arch: "arm64", Ref: "ami-3", Ready: true},
			}
			e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = tc.regions }))
			nc := e.reconcile(e.name)
			c := vcond(nc, "aws", v1alpha1.VendorConditionImagesReady)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tc.reason || !strings.Contains(c.Message, "ami-9") {
				t.Errorf("ImagesReady = %+v, want False with %s naming the image", c, tc.reason)
			}
			// The NodeClass itself stays ready; the vendor's offerings are what is gated.
			wantCond(t, nc, status.ConditionReady, metav1.ConditionTrue, "")
			if c := vcond(nc, "aws", v1alpha1.VendorConditionReady); c == nil || c.Status != metav1.ConditionTrue {
				t.Errorf("VendorReady = %+v", c)
			}
		})
	}
}

// A driver's own reason of an image that is not ready (not a version code) is the
// reason of ImagesReady=False when the images are not ready, with its message; a
// region gap alone keeps the generic reason.
func TestADriverReasonBecomesTheImagesReason(t *testing.T) {
	e := newEnv(t)
	msg := "the image needs the marketplace terms: az vm image terms accept ..."
	e.drv.images = []v1alpha1.ImageStatus{
		{Region: "r1", Arch: "amd64", Message: msg, Reason: "MarketplaceTermsNotAccepted"},
		{Region: "r1", Arch: "arm64", Message: "no image"},
	}
	e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"r1"} }))
	nc := e.reconcile(e.name)
	c := vcond(nc, "aws", v1alpha1.VendorConditionImagesReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "MarketplaceTermsNotAccepted" ||
		!strings.Contains(c.Message, "terms accept") || !strings.Contains(c.Message, "r1/amd64") {
		t.Errorf("ImagesReady = %+v, want False with the driver's reason and message", c)
	}
}

func TestConfigurationWarningShowsInTheVendorStatus(t *testing.T) {
	e := newEnv(t)
	e.drv.warnings = []string{"networking.isolation run has no effect on the brought group"}
	e.create(e.newNodeClass(nil))
	nc := e.reconcile(e.name)
	c := vcond(nc, "aws", v1alpha1.VendorConditionConfigurationWarning)
	if c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "has no effect") {
		t.Fatalf("ConfigurationWarning = %+v, want True with the driver's message", c)
	}
	if r := vcond(nc, "aws", v1alpha1.VendorConditionReady); r == nil || r.Status != metav1.ConditionTrue {
		t.Errorf("VendorReady = %+v, a warning must not make the vendor not ready", r)
	}
	if c.Reason != nodeclass.ReasonSettingHasNoEffect {
		t.Errorf("ConfigurationWarning reason = %s, want %s", c.Reason, nodeclass.ReasonSettingHasNoEffect)
	}
	// A second reconcile with the same warning publishes no second event.
	e.reconcile(e.name)
	n := 0
	for _, r := range e.rec.reasons() {
		if r == nodeclass.EventConfigurationWarning {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d ConfigurationWarning events over two reconciles, want 1", n)
	}
	e.drv.mu.Lock()
	e.drv.warnings = nil
	e.drv.mu.Unlock()
	nc = e.reconcile(e.name)
	if c := vcond(nc, "aws", v1alpha1.VendorConditionConfigurationWarning); c != nil {
		t.Errorf("ConfigurationWarning = %+v after the warning is gone, want none", c)
	}
}

// A driver's reason is promoted only from an image that blocks (a named region,
// or no region ready) and only when the API server accepts it as a reason.
func TestADriverReasonIsPromotedOnlyFromABlockingImage(t *testing.T) {
	cases := map[string]struct {
		images  []v1alpha1.ImageStatus
		regions []string
		want    string
	}{
		"reason of an unnamed region": {
			images: []v1alpha1.ImageStatus{
				{Region: "r1", Arch: "amd64", Message: "no image"},
				{Region: "r2", Arch: "amd64", Message: "terms", Reason: "MarketplaceTermsNotAccepted"},
				{Region: "r3", Arch: "amd64", Ready: true},
			},
			regions: []string{"r1"},
			want:    nodeclass.ReasonImagesNotReady,
		},
		"invalid reason": {
			images:  []v1alpha1.ImageStatus{{Region: "r1", Arch: "amd64", Message: "terms", Reason: "not a reason!"}},
			regions: []string{"r1"},
			want:    nodeclass.ReasonImagesNotReady,
		},
		"no region ready": {
			images:  []v1alpha1.ImageStatus{{Region: "r2", Arch: "amd64", Message: "terms", Reason: "MarketplaceTermsNotAccepted"}},
			regions: nil,
			want:    "MarketplaceTermsNotAccepted",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.drv.images = tc.images
			e.create(e.newNodeClass(func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = tc.regions }))
			nc := e.reconcile(e.name)
			c := vcond(nc, "aws", v1alpha1.VendorConditionImagesReady)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tc.want {
				t.Errorf("ImagesReady = %+v, want False with reason %s", c, tc.want)
			}
		})
	}
}
