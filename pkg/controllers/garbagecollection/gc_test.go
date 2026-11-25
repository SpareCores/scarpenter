/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package garbagecollection_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	gc "github.com/SpareCores/scarpenter/pkg/controllers/garbagecollection"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var now = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// stubDriver lists a fixed set of instances and records deletions.
type stubDriver struct {
	vendor    string
	mu        sync.Mutex
	instances map[string][]*vendors.Instance // by region
	listErr   map[string]error
	deleteErr error
	deleted   []vendors.InstanceRef
	// alive are the NodeClaim names whose instance FindInstance finds; findErr
	// fails the lookup.
	alive   map[string]bool
	findErr error
	// blind says which instance types FindInstance cannot see (LookupLimiter),
	// like Vultr's bare metal with a short registration timeout.
	blind func(instanceType string) bool
}

// CannotFind implements vendors.LookupLimiter.
func (d *stubDriver) CannotFind(instanceType string) bool {
	return d.blind != nil && d.blind(instanceType)
}

func (d *stubDriver) Vendor() string                                        { return d.vendor }
func (d *stubDriver) Init(context.Context, vendors.Config) error            { return nil }
func (d *stubDriver) Regions(context.Context) ([]vendors.Region, error)     { return nil, nil }
func (d *stubDriver) Zones(context.Context, string) ([]vendors.Zone, error) { return nil, nil }
func (d *stubDriver) ResolveImage(context.Context, vendors.ImageRequest) (*vendors.Image, error) {
	return &vendors.Image{}, nil
}
func (d *stubDriver) CreateInstance(context.Context, vendors.CreateRequest) (*vendors.Instance, error) {
	return nil, errors.New("not used")
}
func (d *stubDriver) GetInstance(context.Context, vendors.InstanceRef) (*vendors.Instance, error) {
	return nil, vendors.Errorf(vendors.NotFound, "no")
}
func (d *stubDriver) FindInstance(_ context.Context, _ string, tags map[vendors.TagKey]string) (*vendors.Instance, error) {
	if d.findErr != nil {
		return nil, d.findErr
	}
	if d.alive[tags[vendors.TagNodeClaim]] {
		return &vendors.Instance{State: vendors.StateRunning}, nil
	}
	return nil, vendors.Errorf(vendors.NotFound, "no")
}
func (d *stubDriver) ListInstances(_ context.Context, regions []string, tags map[vendors.TagKey]string) ([]*vendors.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*vendors.Instance
	for _, r := range regions {
		if err := d.listErr[r]; err != nil {
			return nil, err
		}
		for _, i := range d.instances[r] {
			if i.Tags[vendors.TagCluster] == tags[vendors.TagCluster] {
				out = append(out, i)
			}
		}
	}
	return out, nil
}
func (d *stubDriver) DeleteInstance(_ context.Context, ref vendors.InstanceRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deleteErr != nil {
		return d.deleteErr
	}
	d.deleted = append(d.deleted, ref)
	return nil
}
func (d *stubDriver) TagKey(k vendors.TagKey) string { return vendors.DefaultTagKey(k) }

func inst(id, region, claim string, age time.Duration) *vendors.Instance {
	tags := map[vendors.TagKey]string{vendors.TagCluster: "c1", vendors.TagNodeClass: "nc"}
	if claim != "" {
		tags[vendors.TagNodeClaim] = claim
	}
	return &vendors.Instance{
		Ref: vendors.NewInstanceRef(region, id), Region: region, State: vendors.StateRunning,
		CreatedAt: now.Add(-age), Tags: tags,
	}
}

type recorder struct {
	mu   sync.Mutex
	evts []events.Event
}

func (r *recorder) Publish(e ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evts = append(r.evts, e...)
}

// scheme is client-go's global scheme: importing the Karpenter API registers
// NodeClaim in it, as core does.
func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	if err := v1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		t.Fatal(err)
	}
	return clientgoscheme.Scheme
}

func claim(name string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func token(name, claimName string, age time.Duration, typ corev1.SecretType) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: gc.TokenNamespace, UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(now.Add(-age)),
			Labels:            map[string]string{gc.LabelNodeClaim: claimName},
		},
		Type: typ,
	}
}

type env struct {
	drv   *stubDriver
	kube  client.Client
	rec   *recorder
	reg   *prometheus.Registry
	clk   *clocktesting.FakeClock
	c     *gc.Collector
	notes []string
}

func newEnv(t *testing.T, drv *stubDriver, objs []client.Object, tweak func(*gc.Options)) *env {
	t.Helper()
	e := &env{drv: drv, rec: &recorder{}, reg: prometheus.NewRegistry(), clk: clocktesting.NewFakeClock(now)}
	e.kube = fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	opts := gc.Options{
		Client: e.kube, Registry: vendors.NewRegistryFrom(drv), Clock: e.clk, ClusterName: "c1",
		Recorder: e.rec, Registerer: e.reg,
		Regions:   func(string) []string { return []string{"r1", "r2"} },
		OnDeleted: func(v, r string) { e.notes = append(e.notes, v+"/"+r) },
	}
	if tweak != nil {
		tweak(&opts)
	}
	e.c = gc.New(opts)
	return e
}

func (e *env) count(t *testing.T, vendor, kind, reason, result string) float64 {
	t.Helper()
	mfs, err := e.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "scarpenter_gc_deletions_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			if l["vendor"] == vendor && l["kind"] == kind && l["reason"] == reason && l["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func refs(rs []vendors.InstanceRef) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.String()
	}
	slices.Sort(out)
	return out
}

func TestInstanceSweep(t *testing.T) {
	old := 10 * time.Minute
	young := time.Minute
	gone := inst("i-gone", "r1", "orphan-gone", old)
	gone.State = vendors.StateTerminated
	other := inst("i-other", "r1", "orphan-other", old)
	other.Tags[vendors.TagCluster] = "other-cluster"

	for _, tc := range []struct {
		name      string
		instances []*vendors.Instance
		want      []string
		reason    string
	}{
		{"orphan older than the grace period", []*vendors.Instance{inst("i-1", "r1", "gone", old)}, []string{"r1/i-1"}, gc.ReasonNoNodeClaim},
		{"orphan younger than the grace period", []*vendors.Instance{inst("i-1", "r1", "gone", young)}, nil, ""},
		{"instance with a NodeClaim", []*vendors.Instance{inst("i-1", "r1", "live", old)}, nil, ""},
		{"instance without the nodeclaim tag", []*vendors.Instance{inst("i-1", "r2", "", old)}, []string{"r2/i-1"}, gc.ReasonUntagged},
		{"terminated instance", []*vendors.Instance{gone}, nil, ""},
		{"another cluster's instance", []*vendors.Instance{other}, nil, ""},
		{"several regions", []*vendors.Instance{inst("i-1", "r1", "x", old), inst("i-2", "r2", "y", old)},
			[]string{"r1/i-1", "r2/i-2"}, gc.ReasonNoNodeClaim},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{}}
			for _, i := range tc.instances {
				drv.instances[i.Region] = append(drv.instances[i.Region], i)
			}
			e := newEnv(t, drv, []client.Object{claim("live")}, nil)
			res, err := e.c.Sweep(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := refs(drv.deleted); !slices.Equal(got, tc.want) {
				t.Fatalf("deleted %v, want %v", got, tc.want)
			}
			if res.Instances != len(tc.want) || len(e.notes) != len(tc.want) {
				t.Errorf("result %+v, notifications %v", res, e.notes)
			}
			if n := e.count(t, "aws", gc.KindInstance, tc.reason, gc.ResultDeleted); tc.reason != "" && int(n) != len(tc.want) {
				t.Errorf("deletions counter %v, want %d", n, len(tc.want))
			}
		})
	}
}

func TestDeletingNodeClaimKeepsItsInstance(t *testing.T) {
	c := claim("dying")
	now := metav1.NewTime(now)
	c.DeletionTimestamp, c.Finalizers = &now, []string{"karpenter.sh/termination"}
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {inst("i-1", "r1", "dying", time.Hour)}}}
	e := newEnv(t, drv, []client.Object{c}, nil)
	if _, err := e.c.Sweep(context.Background()); err != nil || len(drv.deleted) != 0 {
		t.Fatalf("err %v, deleted %v", err, drv.deleted)
	}
}

func TestDryRun(t *testing.T) {
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {inst("i-1", "r1", "x", time.Hour)}}}
	e := newEnv(t, drv, []client.Object{token("t1", "x", time.Hour, gc.TokenType)}, func(o *gc.Options) { o.DryRun = true })
	res, err := e.c.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(drv.deleted) != 0 || len(e.notes) != 0 {
		t.Errorf("dry run deleted %v", drv.deleted)
	}
	if err := e.kube.Get(context.Background(), client.ObjectKey{Namespace: gc.TokenNamespace, Name: "t1"}, &corev1.Secret{}); err != nil {
		t.Errorf("dry run deleted the token: %v", err)
	}
	if res.Instances != 1 || res.Tokens != 1 {
		t.Errorf("result %+v", res)
	}
	if e.count(t, "aws", gc.KindInstance, gc.ReasonNoNodeClaim, gc.ResultDryRun) != 1 ||
		e.count(t, "", gc.KindToken, gc.ReasonNoNodeClaim, gc.ResultDryRun) != 1 {
		t.Error("dry-run counters missing")
	}
}

func TestErrorsAreIsolated(t *testing.T) {
	good := &stubDriver{vendor: "azure", instances: map[string][]*vendors.Instance{"r1": {inst("i-ok", "r1", "x", time.Hour)}}}
	bad := &stubDriver{vendor: "aws", listErr: map[string]error{"r1": errors.New("boom")},
		instances: map[string][]*vendors.Instance{"r2": {inst("i-2", "r2", "x", time.Hour)}}}
	e := newEnv(t, bad, nil, nil)
	e.c = gc.New(gc.Options{
		Client: e.kube, Registry: vendors.NewRegistryFrom(bad, good), Clock: e.clk, ClusterName: "c1",
		Regions: func(string) []string { return []string{"r1", "r2"} },
	})
	res, err := e.c.Sweep(context.Background())
	if err == nil {
		t.Fatal("the listing error is lost")
	}
	if res.Instances != 2 || len(bad.deleted) != 1 || len(good.deleted) != 1 {
		t.Errorf("result %+v, aws %v, azure %v", res, bad.deleted, good.deleted)
	}
}

func TestDeleteErrorIsCountedAndNotNotified(t *testing.T) {
	drv := &stubDriver{vendor: "aws", deleteErr: vendors.Errorf(vendors.Fatal, "denied"),
		instances: map[string][]*vendors.Instance{"r1": {inst("i-1", "r1", "x", time.Hour)}}}
	e := newEnv(t, drv, nil, nil)
	if _, err := e.c.Sweep(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if e.count(t, "aws", gc.KindInstance, gc.ReasonNoNodeClaim, gc.ResultError) != 1 || len(e.notes) != 0 {
		t.Error("error not counted or notified")
	}
	// NotFound counts as deleted: somebody else was faster.
	drv.deleteErr = vendors.Errorf(vendors.NotFound, "gone")
	if _, err := e.c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTokenSweep(t *testing.T) {
	objs := []client.Object{
		claim("live"),
		token("orphan", "gone", time.Hour, gc.TokenType),
		token("young", "gone", time.Minute, gc.TokenType),
		token("owned", "live", time.Hour, gc.TokenType),
		token("not-a-token", "gone", time.Hour, corev1.SecretTypeOpaque),
	}
	e := newEnv(t, &stubDriver{vendor: "aws"}, objs, nil)
	res, err := e.c.Sweep(context.Background())
	if err != nil || res.Tokens != 1 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	var left corev1.SecretList
	if err := e.kube.List(context.Background(), &left); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(left.Items))
	for _, s := range left.Items {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	if want := []string{"not-a-token", "owned", "young"}; !slices.Equal(names, want) {
		t.Errorf("left %v, want %v", names, want)
	}
	if e.count(t, "", gc.KindToken, gc.ReasonNoNodeClaim, gc.ResultDeleted) != 1 {
		t.Error("token deletion not counted")
	}
}

func TestEventTarget(t *testing.T) {
	nc := &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "nc"}}
	ref := &corev1.ObjectReference{Kind: "Deployment", Namespace: "scarpenter-system", Name: "scarpenter"}
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		ref     func() *corev1.ObjectReference
		wantNum int
		wantObj any
	}{
		{"on the NodeClass", []client.Object{nc}, func() *corev1.ObjectReference { return ref }, 1, "SCNodeClass"},
		{"on the Deployment when the NodeClass is gone", nil, func() *corev1.ObjectReference { return ref }, 1, "ObjectReference"},
		{"nowhere without a reference", nil, nil, 0, nil},
		{"nowhere with a nil reference", nil, func() *corev1.ObjectReference { return nil }, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {inst("i-1", "r1", "x", time.Hour)}}}
			e := newEnv(t, drv, tc.objs, func(o *gc.Options) { o.DeploymentRef = tc.ref })
			if _, err := e.c.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(e.rec.evts) != tc.wantNum {
				t.Fatalf("%d events, want %d", len(e.rec.evts), tc.wantNum)
			}
			if tc.wantNum == 0 {
				return
			}
			ev := e.rec.evts[0]
			var kind string
			switch ev.InvolvedObject.(type) {
			case *v1alpha1.SCNodeClass:
				kind = "SCNodeClass"
			case *corev1.ObjectReference:
				kind = "ObjectReference"
			}
			if kind != tc.wantObj || ev.Reason != gc.EventReason {
				t.Errorf("event on %s with reason %s", kind, ev.Reason)
			}
		})
	}
}

func TestMetricsAndExtensionPoint(t *testing.T) {
	var got sets.Set[string]
	e := newEnv(t, &stubDriver{vendor: "aws"}, []client.Object{claim("a")}, func(o *gc.Options) {
		o.Sweepers = []gc.Sweeper{func(_ context.Context, n sets.Set[string]) error { got = n; return nil }}
	})
	e.clk.Step(time.Second)
	if _, err := e.c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !got.Has("a") {
		t.Errorf("the sweeper got %v", got)
	}
	mfs, err := e.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]float64{}
	for _, mf := range mfs {
		switch mf.GetName() {
		case "scarpenter_gc_sweep_duration_seconds":
			seen[mf.GetName()] = float64(mf.GetMetric()[0].GetHistogram().GetSampleCount())
		case "scarpenter_gc_last_sweep_timestamp_seconds":
			seen[mf.GetName()] = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	if seen["scarpenter_gc_sweep_duration_seconds"] != 1 || seen["scarpenter_gc_last_sweep_timestamp_seconds"] != float64(now.Unix()+1) {
		t.Errorf("sweep metrics: %v", seen)
	}
	if !e.c.NeedLeaderElection() {
		t.Error("the collector must run on the leader only")
	}
}

func TestStartSweepsEveryInterval(t *testing.T) {
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {inst("i-1", "r1", "x", time.Hour)}}}
	e := newEnv(t, drv, nil, func(o *gc.Options) { o.Interval = time.Minute })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.c.Start(ctx) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for range 500 {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	deletedN := func() int { drv.mu.Lock(); defer drv.mu.Unlock(); return len(drv.deleted) }
	waitFor("the first sweep", func() bool { return deletedN() == 1 })

	// A new orphan appears; it is found only after the interval passes.
	drv.mu.Lock()
	drv.instances["r2"] = []*vendors.Instance{inst("i-2", "r2", "y", time.Hour)}
	drv.mu.Unlock()
	waitFor("the timer", e.clk.HasWaiters)
	e.clk.Step(time.Minute)
	waitFor("the second sweep", func() bool { return deletedN() == 3 })

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A terminating instance without a NodeClaim is left alone for the terminating
// grace (the vendor may be deleting it) and deleted again after it; the clock
// starts at the first sighting and restarts when the instance stops terminating.
func TestTerminatingInstanceIsDeletedAfterTheGrace(t *testing.T) {
	stopped := inst("i-stopped", "r1", "gone", time.Hour)
	stopped.State = vendors.StateTerminating
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {stopped}}}
	e := newEnv(t, drv, nil, func(o *gc.Options) { o.TerminatingGrace = vendors.DefaultTerminatingGrace })
	sweep := func() {
		t.Helper()
		if _, err := e.c.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	sweep() // first sighting
	e.clk.Step(vendors.DefaultTerminatingGrace - time.Second)
	sweep()
	if len(drv.deleted) != 0 {
		t.Fatalf("deleted %v inside the grace", refs(drv.deleted))
	}
	e.clk.Step(time.Second)
	sweep()
	if got := refs(drv.deleted); !slices.Equal(got, []string{"r1/i-stopped"}) {
		t.Fatalf("deleted %v, want the stopped instance", got)
	}
	if n := e.count(t, "aws", gc.KindInstance, gc.ReasonTerminating, gc.ResultDeleted); n != 1 {
		t.Errorf("Terminating deletions counter %v, want 1", n)
	}
}

func TestTerminatingInstanceWithANodeClaimIsKept(t *testing.T) {
	stopped := inst("i-1", "r1", "live", time.Hour)
	stopped.State = vendors.StateTerminating
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {stopped}}}
	e := newEnv(t, drv, []client.Object{claim("live")}, nil)
	for range 2 {
		if _, err := e.c.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.clk.Step(time.Hour)
	}
	if len(drv.deleted) != 0 {
		t.Errorf("deleted %v; Karpenter core deletes the NodeClaim first", refs(drv.deleted))
	}
}

// A failed listing does not reset the clocks of the region.
func TestTerminatingClockSurvivesAListingError(t *testing.T) {
	stopped := inst("i-1", "r1", "gone", time.Hour)
	stopped.State = vendors.StateTerminating
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {stopped}}, listErr: map[string]error{}}
	e := newEnv(t, drv, nil, nil)
	_, _ = e.c.Sweep(context.Background())
	e.clk.Step(vendors.DefaultTerminatingGrace / 2)
	drv.listErr["r1"] = errors.New("throttled")
	_, _ = e.c.Sweep(context.Background())
	delete(drv.listErr, "r1")
	e.clk.Step(vendors.DefaultTerminatingGrace / 2)
	_, _ = e.c.Sweep(context.Background())
	if len(drv.deleted) != 1 {
		t.Errorf("deleted %v, want the instance after the grace counted from the first sighting", refs(drv.deleted))
	}
}

// The clock restarts when an instance stops terminating (it was started again).
func TestTerminatingClockRestartsWhenTheInstanceRuns(t *testing.T) {
	flapping := inst("i-1", "r1", "live", time.Hour)
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {flapping}}}
	e := newEnv(t, drv, []client.Object{claim("live")}, nil)
	sweep := func(state string, step time.Duration) {
		t.Helper()
		flapping.State = state
		if _, err := e.c.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.clk.Step(step)
	}
	sweep(vendors.StateTerminating, vendors.DefaultTerminatingGrace-time.Second)
	sweep(vendors.StateRunning, time.Second)
	sweep(vendors.StateTerminating, 0) // the NodeClaim goes away from here on
	if err := e.kube.Delete(context.Background(), claim("live")); err != nil {
		t.Fatal(err)
	}
	e.clk.Step(vendors.DefaultTerminatingGrace - time.Second)
	sweep(vendors.StateTerminating, 0)
	if len(drv.deleted) != 0 {
		t.Fatalf("deleted %v: the clock was not restarted", refs(drv.deleted))
	}
	e.clk.Step(time.Second)
	sweep(vendors.StateTerminating, 0)
	if len(drv.deleted) != 1 {
		t.Errorf("deleted %v, want the instance after a full grace", refs(drv.deleted))
	}
}

// leftoverDriver is a stub driver that also collects launch leftovers.
type leftoverDriver struct {
	stubDriver
	calls []string // region, then whether each asked NodeClaim exists
	err   error
	// listed runs after the driver's listing and before it asks (a NodeClaim
	// created in between).
	listed func()
}

func (d *leftoverDriver) CollectLeftovers(_ context.Context, region string, exists func(string) bool) (int, error) {
	if d.listed != nil {
		d.listed()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, fmt.Sprintf("%s live=%t gone=%t", region, exists("live"), exists("gone")))
	return 1, d.err
}

func TestLeftoverCollectorIsCalledPerRegion(t *testing.T) {
	drv := &leftoverDriver{stubDriver: stubDriver{vendor: "azure", instances: map[string][]*vendors.Instance{}},
		err: errors.New("boom")}
	e := gc.New(gc.Options{
		Client:   fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(claim("live")).Build(),
		Registry: vendors.NewRegistryFrom(drv), Clock: clocktesting.NewFakeClock(now), ClusterName: "c1",
		Recorder: &recorder{}, Registerer: prometheus.NewRegistry(),
		Regions: func(string) []string { return []string{"r1", "r2"} },
	})
	if _, err := e.Sweep(context.Background()); err == nil {
		t.Fatal("the collector's error is not reported")
	}
	want := []string{"r1 live=true gone=false", "r2 live=true gone=false"}
	if !slices.Equal(drv.calls, want) {
		t.Fatalf("calls %v, want %v", drv.calls, want)
	}

	// Not in dry-run.
	drv.calls = nil
	dry := gc.New(gc.Options{
		Client:   fake.NewClientBuilder().WithScheme(scheme(t)).Build(),
		Registry: vendors.NewRegistryFrom(drv), Clock: clocktesting.NewFakeClock(now), ClusterName: "c1",
		Recorder: &recorder{}, Registerer: prometheus.NewRegistry(), DryRun: true,
		Regions: func(string) []string { return []string{"r1"} },
	})
	if _, err := dry.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(drv.calls) != 0 {
		t.Fatalf("dry-run collected leftovers: %v", drv.calls)
	}
}

// A NodeClaim created after the sweep's snapshot (its launch may be making the
// resources the driver just listed) exists for the collector: the check is live.
func TestLeftoverCollectorSeesANodeClaimCreatedDuringTheSweep(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	drv := &leftoverDriver{stubDriver: stubDriver{vendor: "azure", instances: map[string][]*vendors.Instance{}}}
	drv.listed = func() {
		if err := cl.Create(context.Background(), claim("gone")); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Error(err)
		}
	}
	reg := prometheus.NewRegistry()
	notified := 0
	e := gc.New(gc.Options{
		Client: cl, Registry: vendors.NewRegistryFrom(drv), Clock: clocktesting.NewFakeClock(now), ClusterName: "c1",
		Recorder: &recorder{}, Registerer: reg, Regions: func(string) []string { return []string{"r1"} },
		OnDeleted: func(string, string) { notified++ },
	})
	if _, err := e.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"r1 live=false gone=true"}; !slices.Equal(drv.calls, want) {
		t.Fatalf("calls %v, want %v", drv.calls, want)
	}
	if got := (&env{reg: reg}).count(t, "azure", gc.KindLeftover, gc.ReasonNoNodeClaim, gc.ResultDeleted); got != 1 {
		t.Errorf("leftover deletions counted %v, want 1", got)
	}
	if notified != 1 {
		t.Errorf("OnDeleted called %d times, want 1", notified)
	}
}

func node(name, providerID string, age time.Duration) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types.UID("uid-" + name), CreationTimestamp: metav1.NewTime(now.Add(-age)),
		},
		Spec: corev1.NodeSpec{ProviderID: providerID},
	}
}

func claimWithProviderID(name, providerID string) *karpv1.NodeClaim {
	c := claim(name)
	c.Status.ProviderID = providerID
	return c
}

func nodeNames(t *testing.T, e *env) []string {
	t.Helper()
	var left corev1.NodeList
	if err := e.kube.List(context.Background(), &left); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(left.Items))
	for _, n := range left.Items {
		names = append(names, n.Name)
	}
	slices.Sort(names)
	return names
}

func TestNodeSweep(t *testing.T) {
	const old = 10 * time.Minute
	objs := []client.Object{
		claimWithProviderID("live", "scarpenter://aws/r1/live"),
		node("orphan", "scarpenter://aws/r1/gone", old),
		node("young", "scarpenter://aws/r1/young", time.Minute),
		node("owned", "scarpenter://aws/r1/live", old),
		node("instance-exists", "scarpenter://aws/r1/alive", old),
		node("other-vendor", "scarpenter://other/r1/gone", old),
		node("foreign", "aws:///eu-west-1a/i-123", old),
		node("no-provider-id", "", old),
	}
	drv := &stubDriver{vendor: "aws", alive: map[string]bool{"alive": true}}
	e := newEnv(t, drv, objs, nil)
	res, err := e.c.Sweep(context.Background())
	if err != nil || res.Nodes != 1 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	want := []string{"foreign", "instance-exists", "no-provider-id", "other-vendor", "owned", "young"}
	if got := nodeNames(t, e); !slices.Equal(got, want) {
		t.Errorf("left %v, want %v", got, want)
	}
	if e.count(t, "aws", gc.KindNode, gc.ReasonNoInstance, gc.ResultDeleted) != 1 {
		t.Error("node deletion not counted")
	}
}

func TestNodeSweepKeepsTheNodeWhenTheLookupFails(t *testing.T) {
	drv := &stubDriver{vendor: "aws", findErr: errors.New("vendor down")}
	e := newEnv(t, drv, []client.Object{node("n", "scarpenter://aws/r1/gone", time.Hour)}, nil)
	if res, err := e.c.Sweep(context.Background()); err == nil || res.Nodes != 0 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if got := nodeNames(t, e); !slices.Equal(got, []string{"n"}) {
		t.Errorf("left %v", got)
	}
}

func TestNodeSweepDryRun(t *testing.T) {
	drv := &stubDriver{vendor: "aws"}
	e := newEnv(t, drv, []client.Object{node("n", "scarpenter://aws/r1/gone", time.Hour)},
		func(o *gc.Options) { o.DryRun = true })
	if res, err := e.c.Sweep(context.Background()); err != nil || res.Nodes != 1 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if got := nodeNames(t, e); !slices.Equal(got, []string{"n"}) {
		t.Errorf("left %v", got)
	}
	if e.count(t, "aws", gc.KindNode, gc.ReasonNoInstance, gc.ResultDryRun) != 1 {
		t.Error("dry-run not counted")
	}
}

func TestNodeSweepSkipsADeletingNodeAndIgnoresAnEmptyClaimProviderID(t *testing.T) {
	deleting := node("deleting", "scarpenter://aws/r1/gone", time.Hour)
	deleting.Finalizers = []string{"test/hold"}
	deleting.DeletionTimestamp = &metav1.Time{Time: now}
	objs := []client.Object{
		claimWithProviderID("launching", ""), // no provider ID yet
		deleting,
		node("no-provider-id", "", time.Hour),
	}
	e := newEnv(t, &stubDriver{vendor: "aws"}, objs, nil)
	if res, err := e.c.Sweep(context.Background()); err != nil || res.Nodes != 0 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if got := nodeNames(t, e); !slices.Equal(got, []string{"deleting", "no-provider-id"}) {
		t.Errorf("left %v", got)
	}
}

func TestNodeSweepKeepsTheNodeOfATypeTheLookupCannotSee(t *testing.T) {
	blind := node("blind", "scarpenter://aws/r1/gone-1", time.Hour)
	blind.Labels = map[string]string{corev1.LabelInstanceTypeStable: "aws.big"}
	unlabelled := node("unlabelled", "scarpenter://aws/r1/gone-2", time.Hour)
	seen := node("seen", "scarpenter://aws/r1/gone-3", time.Hour)
	seen.Labels = map[string]string{corev1.LabelInstanceTypeStable: "aws.small"}
	drv := &stubDriver{vendor: "aws", blind: func(typ string) bool { return typ != "aws.small" }}
	e := newEnv(t, drv, []client.Object{blind, unlabelled, seen}, nil)
	if res, err := e.c.Sweep(context.Background()); err != nil || res.Nodes != 1 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if got := nodeNames(t, e); !slices.Equal(got, []string{"blind", "unlabelled"}) {
		t.Errorf("left %v", got)
	}
}

func TestStoppingOrphanIsNeitherAnErrorNorWaitsForTheGrace(t *testing.T) {
	drv := &stubDriver{vendor: "aws", instances: map[string][]*vendors.Instance{"r1": {inst("i-1", "r1", "gone", time.Hour)}},
		deleteErr: vendors.NewError(vendors.Retryable, vendors.CodeStopping, errors.New("still stopping"))}
	e := newEnv(t, drv, nil, nil)
	res, err := e.c.Sweep(context.Background())
	if err != nil || res.Instances != 0 || len(e.notes) != 0 {
		t.Fatalf("result %+v, err %v, notes %v", res, err, e.notes)
	}
	if e.count(t, "aws", gc.KindInstance, gc.ReasonNoNodeClaim, gc.ResultError) != 0 {
		t.Error("a stopping answer counted as an error")
	}
	// Next sweep: it is stopped (terminating) and is deleted at once, not after the grace.
	drv.deleteErr = nil
	drv.instances["r1"][0].State = vendors.StateTerminating
	res, err = e.c.Sweep(context.Background())
	if err != nil || res.Instances != 1 || len(drv.deleted) != 1 {
		t.Fatalf("result %+v, err %v, deleted %v", res, err, drv.deleted)
	}
}

// rejoinEnv is an env whose Rejoin treats the provider IDs of deleted as
// recently deleted.
func rejoinEnv(t *testing.T, deleted map[string]bool, objs ...client.Object) (*env, *gc.Rejoin) {
	t.Helper()
	e := newEnv(t, &stubDriver{vendor: "aws"}, objs, func(o *gc.Options) {
		o.RecentlyDeleted = func(id string) bool { return deleted[id] }
	})
	return e, e.c.NewRejoin()
}

func reconcileNode(t *testing.T, r *gc.Rejoin, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatal(err)
	}
}

func TestRejoinedNodeOfADeletedInstanceIsRemovedAtOnce(t *testing.T) {
	// A young Node: the sweep's grace does not apply.
	e, r := rejoinEnv(t, map[string]bool{"scarpenter://aws/r1/gone": true},
		node("back", "scarpenter://aws/r1/gone", time.Second))
	reconcileNode(t, r, "back")
	if got := nodeNames(t, e); len(got) != 0 {
		t.Errorf("left %v", got)
	}
	if e.count(t, "aws", gc.KindNode, gc.ReasonRejoined, gc.ResultDeleted) != 1 {
		t.Error("deletion not counted")
	}
}

func TestRejoinKeepsTheNodeOfANodeClaim(t *testing.T) {
	e, r := rejoinEnv(t, map[string]bool{"scarpenter://aws/r1/live": true},
		claimWithProviderID("live", "scarpenter://aws/r1/live"), node("owned", "scarpenter://aws/r1/live", time.Second))
	reconcileNode(t, r, "owned")
	if got := nodeNames(t, e); len(got) != 1 {
		t.Errorf("left %v, want the Node kept", got)
	}
}

func TestRejoinKeepsANodeThatWasNotDeleted(t *testing.T) {
	e, r := rejoinEnv(t, map[string]bool{"scarpenter://aws/r1/gone": true},
		node("other", "scarpenter://aws/r1/other", time.Second), node("no-provider-id", "", time.Second))
	reconcileNode(t, r, "other")
	reconcileNode(t, r, "no-provider-id")
	reconcileNode(t, r, "missing")
	if got := nodeNames(t, e); len(got) != 2 {
		t.Errorf("left %v, want both kept", got)
	}
}

func TestRejoinIgnoresAnExpiredEntry(t *testing.T) {
	// The CloudProvider's RecentlyDeleted answers false after its window.
	e, r := rejoinEnv(t, map[string]bool{}, node("late", "scarpenter://aws/r1/gone", time.Second))
	reconcileNode(t, r, "late")
	if got := nodeNames(t, e); len(got) != 1 {
		t.Errorf("left %v, want the Node kept", got)
	}
}

func TestRejoinRemovesEveryRecreationAndLogsEachDelete(t *testing.T) {
	var buf bytes.Buffer
	ctx := logr.NewContext(context.Background(), funcr.New(func(prefix, args string) { buf.WriteString(args + "\n") }, funcr.Options{}))
	e, r := rejoinEnv(t, map[string]bool{"scarpenter://aws/r1/gone": true})
	for range 3 {
		if err := e.kube.Create(ctx, node("back", "scarpenter://aws/r1/gone", 0)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "back"}}); err != nil {
			t.Fatal(err)
		}
		if got := nodeNames(t, e); len(got) != 0 {
			t.Fatalf("left %v", got)
		}
	}
	if n := strings.Count(buf.String(), "deleted the Node of a deleted instance"); n != 3 {
		t.Errorf("%d log lines, want 3:\n%s", n, buf.String())
	}
	if e.count(t, "aws", gc.KindNode, gc.ReasonRejoined, gc.ResultDeleted) != 3 {
		t.Error("deletions not all counted")
	}
}

func TestRejoinKeepsADeletingNode(t *testing.T) {
	n := node("going", "scarpenter://aws/r1/gone", time.Second)
	n.Finalizers = []string{"test/hold"}
	n.DeletionTimestamp = &metav1.Time{Time: now}
	e, r := rejoinEnv(t, map[string]bool{"scarpenter://aws/r1/gone": true}, n)
	reconcileNode(t, r, "going")
	if e.count(t, "aws", gc.KindNode, gc.ReasonRejoined, gc.ResultDeleted) != 0 {
		t.Error("a deleting Node was counted as deleted")
	}
}

func TestRejoinWithoutRecentlyDeletedKeepsTheNode(t *testing.T) {
	e := newEnv(t, &stubDriver{vendor: "aws"}, []client.Object{node("n", "scarpenter://aws/r1/gone", time.Second)}, nil)
	reconcileNode(t, e.c.NewRejoin(), "n")
	if got := nodeNames(t, e); len(got) != 1 {
		t.Errorf("left %v, want the Node kept", got)
	}
}

func TestRejoinDryRunCountsOncePerProviderID(t *testing.T) {
	e := newEnv(t, &stubDriver{vendor: "aws"}, []client.Object{node("n", "scarpenter://aws/r1/gone", time.Second)},
		func(o *gc.Options) {
			o.DryRun = true
			o.RecentlyDeleted = func(string) bool { return true }
		})
	r := e.c.NewRejoin()
	reconcileNode(t, r, "n")
	reconcileNode(t, r, "n")
	if got := nodeNames(t, e); len(got) != 1 {
		t.Errorf("left %v, want the Node kept", got)
	}
	if n := e.count(t, "aws", gc.KindNode, gc.ReasonRejoined, gc.ResultDryRun); n != 1 {
		t.Errorf("dry-run counted %v, want 1", n)
	}
}

func TestRejoinPublishesAnEventAndCountsOnlyARealDelete(t *testing.T) {
	e := newEnv(t, &stubDriver{vendor: "aws"}, []client.Object{node("n", "scarpenter://aws/r1/gone", 0)},
		func(o *gc.Options) {
			o.RecentlyDeleted = func(string) bool { return true }
			o.DeploymentRef = func() *corev1.ObjectReference { return &corev1.ObjectReference{Kind: "Deployment", Name: "sc"} }
		})
	r := e.c.NewRejoin()
	reconcileNode(t, r, "n")
	reconcileNode(t, r, "n") // already gone: counts nothing
	if len(e.rec.evts) != 1 || e.rec.evts[0].Reason != gc.EventReason {
		t.Errorf("events %+v, want one GarbageCollected", e.rec.evts)
	}
	if n := e.count(t, "aws", gc.KindNode, gc.ReasonRejoined, gc.ResultDeleted); n != 1 {
		t.Errorf("deleted counted %v, want 1", n)
	}
}

func TestRejoinDeleteConflictRequeuesWithoutAnError(t *testing.T) {
	conflict := interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "n", errors.New("uid mismatch"))
	}}
	kube := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(conflict).
		WithObjects(node("n", "scarpenter://aws/r1/gone", 0)).Build()
	reg := prometheus.NewRegistry()
	c := gc.New(gc.Options{
		Client: kube, Registry: vendors.NewRegistryFrom(&stubDriver{vendor: "aws"}), Clock: clocktesting.NewFakeClock(now),
		Registerer: reg, RecentlyDeleted: func(string) bool { return true },
	})
	res, err := c.NewRejoin().Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "n"}})
	if err != nil || res.RequeueAfter == 0 {
		t.Errorf("result %+v, err %v, want a requeue and no error", res, err)
	}
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "scarpenter_gc_deletions_total" {
			t.Errorf("a conflict was counted: %v", mf.GetMetric())
		}
	}
}
