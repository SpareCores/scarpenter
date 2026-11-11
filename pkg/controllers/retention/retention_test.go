/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package retention_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/billing"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/controllers/retention"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var (
	envCfg  *rest.Config
	envStop func() error
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		var err error
		envCfg, envStop, err = testutil.StartEnvWith(testutil.EnvOptions{CRDs: testutil.KarpenterCRDs()})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = envStop() }()
		return m.Run()
	}())
}

// seq makes the names unique across the tests, which share one API server.
var seq atomic.Int64

const (
	hour = time.Hour
	// vendors of the stub rules.
	vHourly  = "hourly"  // 1h increments and minimum, verified
	vMinimum = "minimum" // per second after a 10 minute minimum, verified
	vSecond  = "second"  // per second, no minimum, verified
	vShort   = "short"   // per second after a 2 minute minimum (under the floor)
	vUnverif = "unverif" // hourly, not verified
	region   = "r1"
)

// stubRules gives the vendors of the tests their rules.
type stubRules struct {
	rules map[string]billing.Rule
	// hide makes Keeper have no row for the vendor.
	hide map[string]bool
}

func (s *stubRules) Rule(vendor, _, _ string) (billing.Rule, bool) {
	r, ok := s.rules[vendor]
	return r, ok && !s.hide[vendor]
}

func rule(inc, minimum time.Duration, verified bool) billing.Rule {
	ov := &vendors.BillingRule{Increment: &inc, Verified: verified, Source: "test"}
	if minimum > 0 {
		ov.Minimum = &minimum
	}
	return billing.Resolve(catalog.BillingFacts{}, ov)
}

// stubStarts is the StartSource: the instances the cloud provider knows.
type stubStarts struct {
	mu sync.Mutex
	by map[string]string
}

func (s *stubStarts) BillingStart(_ context.Context, providerID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.by[providerID], nil
}

type stubQuota struct {
	mu     sync.Mutex
	scopes []retention.QuotaScope
}

func (s *stubQuota) BlockedScopes(context.Context) ([]retention.QuotaScope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]retention.QuotaScope(nil), s.scopes...), nil
}

// harness is one test's world: its own NodeClass, NodePool, nodes, cluster state
// and controller over the shared API server, with a fake clock.
type harness struct {
	t        *testing.T
	c        client.Client
	clk      *clocktesting.FakeClock
	cluster  *state.Cluster
	ctrl     *retention.Controller
	reg      *prometheus.Registry
	rules    *stubRules
	starts   *stubStarts
	signals  *retention.Signals
	quota    *stubQuota
	events   *eventLog
	prefix   string
	class    string
	pool     string
	t0       time.Time
	claimSeq int
	sc       *retention.StatusController
}

type eventLog struct {
	mu   sync.Mutex
	list []events.Event
}

func (e *eventLog) Publish(evts ...events.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, evts...)
}

// count is the number of Events with the reason on the node (all nodes if empty).
func (e *eventLog) count(reason, node string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.list {
		obj, _ := ev.InvolvedObject.(client.Object)
		if ev.Reason == reason && (node == "" || (obj != nil && obj.GetName() == node)) {
			n++
		}
	}
	return n
}

type poolOpts struct {
	never    bool
	static   bool
	budgets  []karpv1.Budget
	limits   corev1.ResourceList
	taints   []corev1.Taint
	classRef string
}

type classOpts struct {
	billing *v1alpha1.BillingSpec
	vendors v1alpha1.VendorBlocks
}

func newHarness(t *testing.T, billingSpec *v1alpha1.BillingSpec) *harness {
	t.Helper()
	n := seq.Add(1)
	scheme := clientgoscheme.Scheme
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(envCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, c: c, clk: clocktesting.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)),
		reg: prometheus.NewRegistry(), prefix: fmt.Sprintf("t%d", n), events: &eventLog{}, quota: &stubQuota{},
		starts: &stubStarts{by: map[string]string{}},
		rules: &stubRules{rules: map[string]billing.Rule{
			vHourly: rule(hour, hour, true), vMinimum: rule(time.Second, 10*time.Minute, true),
			vSecond: rule(time.Second, 0, true), vShort: rule(time.Second, 2*time.Minute, true),
			vUnverif: rule(hour, hour, false),
		}},
	}
	h.t0 = h.clk.Now()
	h.class, h.pool = h.prefix+"-class", h.prefix+"-pool"
	h.cluster = state.NewCluster(h.clk, c, nil)
	h.signals = retention.NewSignals(h.clk)
	h.newController()
	h.createClass(classOpts{billing: billingSpec})
	h.createPool(poolOpts{})
	return h
}

func (h *harness) newController() { h.newControllerWith(h.cluster) }

func (h *harness) newControllerWith(cluster retention.Cluster) {
	h.ctrl = retention.New(retention.Options{
		Client: h.c, Reader: h.c, Cluster: cluster, Rules: h.rules, Starts: h.starts, Clock: h.clk,
		Recorder: events.NewRecorder(&fakeRecorder{h.events}), Signals: h.signals, Quota: h.quota, Registerer: h.reg,
	})
}

// fakeRecorder adapts the events log to client-go's recorder, behind core's
// recorder, which deduplicates the Events the way the operator's does.
type fakeRecorder struct{ log *eventLog }

func (f *fakeRecorder) Event(obj runtime.Object, typ, reason, msg string) {
	f.log.Publish(events.Event{InvolvedObject: obj, Type: typ, Reason: reason, Message: msg})
}
func (f *fakeRecorder) Eventf(obj runtime.Object, typ, reason, format string, args ...any) {
	f.Event(obj, typ, reason, fmt.Sprintf(format, args...))
}
func (f *fakeRecorder) AnnotatedEventf(obj runtime.Object, _ map[string]string, typ, reason, format string, args ...any) {
	f.Eventf(obj, typ, reason, format, args...)
}

func (h *harness) createClass(o classOpts) {
	h.t.Helper()
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: h.class},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar: v1alpha1.FlatcarSpec{Version: "4757.2.3"}, Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network: v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors: v1alpha1.VendorBlocks{AWS: &aws.Settings{}}, Billing: o.billing,
		},
	}
	h.must(h.c.Create(h.t.Context(), nc))
	h.t.Cleanup(func() { _ = h.c.Delete(context.Background(), nc) })
}

func (h *harness) createPool(o poolOpts) { h.createPoolNamed(h.pool, o) }

func (h *harness) createPoolNamed(name string, o poolOpts) {
	h.t.Helper()
	np := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name}}
	np.Spec.Template.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: h.class}
	np.Spec.Template.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{}
	np.Spec.Template.Spec.Taints = o.taints
	np.Spec.Limits = karpv1.Limits(o.limits)
	np.Spec.Disruption.ConsolidationPolicy = karpv1.ConsolidationPolicyWhenEmptyOrUnderutilized
	np.Spec.Disruption.ConsolidateAfter = karpv1.MustParseNillableDuration("0s")
	if o.never {
		np.Spec.Disruption.ConsolidateAfter = karpv1.MustParseNillableDuration("Never")
	}
	np.Spec.Disruption.Budgets = o.budgets
	if len(np.Spec.Disruption.Budgets) == 0 {
		np.Spec.Disruption.Budgets = []karpv1.Budget{{Nodes: "100%"}}
	}
	if o.static {
		np.Spec.Replicas = new(int64(1))
		np.Spec.Disruption.ConsolidateAfter = karpv1.NillableDuration{}
		np.Spec.Disruption.ConsolidationPolicy = ""
	}
	h.must(h.c.Create(h.t.Context(), np))
	h.t.Cleanup(func() { _ = h.c.Delete(context.Background(), np) })
}

// replacePool recreates the pool with other options (the tests start with the
// default one).
func (h *harness) replacePool(o poolOpts) {
	h.t.Helper()
	np := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: h.pool}}
	h.must(client.IgnoreNotFound(h.c.Delete(h.t.Context(), np)))
	h.createPool(o)
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

// nodeOpts shapes a node.
type nodeOpts struct {
	vendor  string
	spot    bool
	initial *bool // Initialized (default true)
	// start is the billing start as an offset from t0 (default 0).
	start time.Duration
	// noStart leaves the billing-start annotation off.
	noStart bool
	// onlyOnNode puts the annotation on the Node only.
	annotations map[string]string
	drifted     bool
	pool        string
	cpu         string
}

type world struct {
	h     *harness
	name  string
	claim string
	pid   string
	opts  nodeOpts
}

// addNode creates a NodeClaim and its Node, registered and initialized, and
// feeds them to the cluster state.
func (h *harness) addNode(o nodeOpts) *world {
	h.t.Helper()
	h.claimSeq++
	if o.vendor == "" {
		o.vendor = vHourly
	}
	if o.cpu == "" {
		o.cpu = "2"
	}
	if o.pool == "" {
		o.pool = h.pool
	}
	name := fmt.Sprintf("%s-n%d", h.prefix, h.claimSeq)
	pid := fmt.Sprintf("scarpenter://%s/%s/%s", o.vendor, region, name)
	initial := o.initial == nil || *o.initial
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: map[string]string{
			karpv1.NodePoolLabelKey: o.pool, karpv1.CapacityTypeLabelKey: ternary(o.spot, "spot", "on-demand"),
			v1alpha1.LabelFamily: "f",
		},
		Annotations: map[string]string{}, Finalizers: []string{"test/hold"},
	}}
	claim.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: h.class}
	claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{}
	if !o.noStart {
		claim.Annotations[v1alpha1.AnnotationBillingStart] = h.t0.Add(o.start).UTC().Format(time.RFC3339)
	}
	h.must(h.c.Create(h.t.Context(), claim))
	h.t.Cleanup(func() {
		got := &karpv1.NodeClaim{}
		if h.c.Get(context.Background(), client.ObjectKey{Name: name}, got) == nil {
			got.Finalizers = nil
			_ = h.c.Update(context.Background(), got)
			_ = h.c.Delete(context.Background(), got)
		}
	})
	claim.Status.ProviderID, claim.Status.NodeName = pid, name
	cs := claim.StatusConditions()
	cs.SetTrue(karpv1.ConditionTypeLaunched)
	cs.SetTrue(karpv1.ConditionTypeRegistered)
	if initial {
		cs.SetTrue(karpv1.ConditionTypeInitialized)
	}
	if o.drifted {
		cs.SetTrue(karpv1.ConditionTypeDrifted)
	}
	for i := range claim.Status.Conditions {
		claim.Status.Conditions[i].LastTransitionTime = metav1.NewTime(h.t0)
	}
	h.must(h.c.Status().Update(h.t.Context(), claim))

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: name, Annotations: o.annotations, Labels: map[string]string{
			karpv1.NodePoolLabelKey: o.pool, karpv1.NodeRegisteredLabelKey: "true",
			karpv1.CapacityTypeLabelKey:    ternary(o.spot, "spot", "on-demand"),
			corev1.LabelInstanceTypeStable: "type", v1alpha1.LabelFamily: "f",
		},
	}, Spec: corev1.NodeSpec{ProviderID: pid}}
	if initial {
		node.Labels[karpv1.NodeInitializedLabelKey] = "true"
	}
	h.must(h.c.Create(h.t.Context(), node))
	node.Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(o.cpu), corev1.ResourcePods: resource.MustParse("110")}
	node.Status.Allocatable = node.Status.Capacity
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	h.must(h.c.Status().Update(h.t.Context(), node))
	h.t.Cleanup(func() { _ = h.c.Delete(context.Background(), node) })
	w := &world{h: h, name: name, claim: name, pid: pid, opts: o}
	h.feed()
	return w
}

func ternary[T any](c bool, a, b T) T {
	if c {
		return a
	}
	return b
}

// ctx is the context of the controller and of the cluster state: core reads its
// options from it.
func (h *harness) ctx() context.Context {
	return coreoptions.ToContext(h.t.Context(), &coreoptions.Options{})
}

// feed updates Karpenter's cluster state from the API server, which the
// operator's informers do.
func (h *harness) feed() {
	h.t.Helper()
	ctx := h.ctx()
	var claims karpv1.NodeClaimList
	h.must(h.c.List(ctx, &claims))
	for i := range claims.Items {
		if strings.HasPrefix(claims.Items[i].Name, h.prefix+"-") {
			h.cluster.UpdateNodeClaim(&claims.Items[i])
		}
	}
	var nodes corev1.NodeList
	h.must(h.c.List(ctx, &nodes))
	for i := range nodes.Items {
		if strings.HasPrefix(nodes.Items[i].Name, h.prefix+"-") {
			h.must(h.cluster.UpdateNode(ctx, &nodes.Items[i]))
		}
	}
}

// reconcile runs one reconcile of the node and returns its result.
func (w *world) reconcile() reconcile.Result {
	w.h.t.Helper()
	w.h.feed()
	res, err := w.h.ctrl.Reconcile(w.h.ctx(), reconcile.Request{NamespacedName: types.NamespacedName{Name: w.name}})
	if err != nil {
		w.h.t.Fatalf("Reconcile(%s): %v", w.name, err)
	}
	return res
}

func (w *world) node() *corev1.Node {
	w.h.t.Helper()
	n := &corev1.Node{}
	w.h.must(w.h.c.Get(w.h.t.Context(), client.ObjectKey{Name: w.name}, n))
	return n
}

func (w *world) held() bool {
	n := w.node()
	return n.Annotations[karpv1.DoNotDisruptAnnotationKey] == "true" && n.Annotations[v1alpha1.AnnotationRetainUntil] != ""
}

func (w *world) tainted() bool {
	for _, t := range w.node().Spec.Taints {
		if t.Key == v1alpha1.TaintReleasing {
			return true
		}
	}
	return false
}

// deleted reports whether the NodeClaim was deleted (marked, or gone).
func (w *world) deleted() bool {
	w.h.t.Helper()
	cl := &karpv1.NodeClaim{}
	err := w.h.c.Get(w.h.t.Context(), client.ObjectKey{Name: w.claim}, cl)
	if apierrors.IsNotFound(err) {
		return true
	}
	w.h.must(err)
	return !cl.DeletionTimestamp.IsZero()
}

// addPod binds a pod to the node.
func (w *world) addPod(daemonSet bool) *corev1.Pod {
	w.h.t.Helper()
	w.h.claimSeq++
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-p%d", w.h.prefix, w.h.claimSeq), Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:   w.name,
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
		},
	}
	if daemonSet {
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ds", UID: "ds-uid"}}
	}
	w.h.must(w.h.c.Create(w.h.t.Context(), pod))
	w.h.t.Cleanup(func() { _ = w.h.c.Delete(context.Background(), pod, client.GracePeriodSeconds(0)) })
	return pod
}

func (w *world) removePods() {
	w.h.t.Helper()
	var pods corev1.PodList
	w.h.must(w.h.c.List(w.h.t.Context(), &pods, client.MatchingFields{"spec.nodeName": w.name}))
	for i := range pods.Items {
		w.h.must(w.h.c.Delete(w.h.t.Context(), &pods.Items[i], client.GracePeriodSeconds(0)))
	}
}

// metric reads a counter or gauge of the test's registry by name and labels.
func (h *harness) metric(name string, labels ...string) float64 {
	h.t.Helper()
	families, err := h.reg.Gather()
	h.must(err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			ok := true
			for i := 0; i+1 < len(labels); i += 2 {
				ok = ok && got[labels[i]] == labels[i+1]
			}
			if !ok {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

func (h *harness) advanceTo(offset time.Duration) { h.clk.SetTime(h.t0.Add(offset)) }

func clientKey(name string) client.ObjectKey { return client.ObjectKey{Name: name} }
