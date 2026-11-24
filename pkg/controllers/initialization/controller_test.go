/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package initialization_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/initialization"
)

var now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type recorder struct {
	mu   sync.Mutex
	list []events.Event
}

func (r *recorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, evts...)
}

// scenario is one claim and its node; the zero value is a registered,
// uninitialized claim of a class without a launch policy (age 0: the node was created now).
type scenario struct {
	age          time.Duration // of the node (and, without one, of the Registered condition)
	timeout      *time.Duration
	initialized  bool
	unregistered bool
	deleting     bool
	noNode       bool
	foreign      bool // a NodeClaim of another provider's class
	rebooting    bool
	nodeGone     bool // NodeName is set, the Node is not there
}

// run reconciles the claim once and returns the claim's state.
func run(t *testing.T, sc scenario) (res reconcile.Result, deleted bool, rec *recorder, reg *prometheus.Registry) {
	t.Helper()
	ctl, c, rec, reg := build(t, sc, interceptor.Funcs{})
	res, err := ctl.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "claim"}})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Get(t.Context(), client.ObjectKey{Name: "claim"}, &karpv1.NodeClaim{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if sc.deleting { // the finalizer keeps it; "deleted" means this reconcile asked for it
		return res, len(rec.list) > 0, rec, reg
	}
	return res, apierrors.IsNotFound(err), rec, reg
}

// build returns the controller over a fake API server holding the scenario.
func build(t *testing.T, sc scenario, funcs interceptor.Funcs) (*initialization.Controller, client.Client, *recorder, *prometheus.Registry) {
	t.Helper()
	if err := v1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		t.Fatal(err)
	}
	class := &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "class"}}
	if sc.timeout != nil {
		class.Spec.Launch = &v1alpha1.LaunchSpec{InitializationTimeout: &metav1.Duration{Duration: *sc.timeout}}
	}
	providerID := "scarpenter://aws/us-east-1/claim"
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim"}}
	claim.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: "class"}
	if sc.foreign {
		claim.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: "other.example.com", Kind: "Other", Name: "class"}
	}
	if sc.deleting {
		claim.Finalizers = []string{"test/hold"}
		claim.DeletionTimestamp = &metav1.Time{Time: now}
	}
	claim.Status.ProviderID, claim.Status.NodeName = providerID, "node"
	if !sc.unregistered {
		claim.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	}
	if sc.initialized {
		claim.StatusConditions().SetTrue(karpv1.ConditionTypeInitialized)
	}
	if sc.rebooting {
		claim.StatusConditions().SetTrue(karpv1.ConditionTypeRebooting)
	}
	for i := range claim.Status.Conditions {
		claim.Status.Conditions[i].LastTransitionTime = metav1.NewTime(now.Add(-sc.age))
	}
	objs := []client.Object{class, claim}
	if !sc.noNode && !sc.nodeGone {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node", CreationTimestamp: metav1.NewTime(now.Add(-sc.age))},
			Spec:       corev1.NodeSpec{ProviderID: providerID},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady", Message: "image pull failed",
			}}},
		}
		objs = append(objs, node)
	}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	rec, reg := &recorder{}, prometheus.NewRegistry()
	ctl := initialization.New(initialization.Options{
		Client: c, Recorder: rec, Clock: clocktesting.NewFakeClock(now), Registerer: reg,
	})
	return ctl, c, rec, reg
}

func TestTimeout(t *testing.T) {
	for name, tc := range map[string]struct {
		sc      scenario
		deleted bool
		requeue time.Duration
	}{
		"after the default timeout":      {scenario{age: time.Hour}, true, 0},
		"long after the default timeout": {scenario{age: 5 * time.Hour}, true, 0},
		"before the default timeout":     {scenario{age: 55 * time.Minute}, false, 5 * time.Minute},
		"wait is capped":                 {scenario{age: 20 * time.Minute}, false, initialization.MaxRecheck},
		"a second before the deadline":   {scenario{age: time.Hour - time.Second}, false, time.Second},
		"class timeout longer, not yet":  {scenario{age: 2 * time.Hour, timeout: new(3 * time.Hour)}, false, initialization.MaxRecheck},
		"class timeout shorter, over":    {scenario{age: 15 * time.Minute, timeout: new(10 * time.Minute)}, true, 0},
		"class timeout 0 disables":       {scenario{age: 48 * time.Hour, timeout: new(time.Duration(0))}, false, initialization.MaxRecheck},
		"far deadline is rechecked":      {scenario{age: time.Minute, timeout: new(24 * time.Hour)}, false, initialization.MaxRecheck},
		"rebooting node is skipped":      {scenario{age: 5 * time.Hour, rebooting: true}, false, 0},
		"node gone: the Registered time": {scenario{age: 2 * time.Hour, nodeGone: true}, true, 0},
		"initialized claim is untouched": {scenario{age: 5 * time.Hour, initialized: true}, false, 0},
		"not registered is core's":       {scenario{age: 5 * time.Hour, unregistered: true}, false, 0},
		"claim being deleted is skipped": {scenario{age: 5 * time.Hour, deleting: true}, false, 0},
		"another provider's class":       {scenario{age: 5 * time.Hour, foreign: true}, false, 0},
		"no node: the Registered time":   {scenario{age: 2 * time.Hour, noNode: true}, true, 0},
		"no node, not yet":               {scenario{age: 50 * time.Minute, noNode: true}, false, 10 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			res, deleted, _, _ := run(t, tc.sc)
			if deleted != tc.deleted {
				t.Errorf("deleted = %v, want %v", deleted, tc.deleted)
			}
			if res.RequeueAfter != tc.requeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.requeue)
			}
		})
	}
}

// The deletion is explained on the NodeClaim and the NodeClass, in a log-able message with the
// node's Ready condition, and counted.
func TestObservability(t *testing.T) {
	_, deleted, rec, reg := run(t, scenario{age: 90 * time.Minute})
	if !deleted {
		t.Fatal("NodeClaim not deleted")
	}
	kinds := map[string]bool{}
	for _, e := range rec.list {
		if e.Reason != initialization.EventTimeout || e.Type != corev1.EventTypeWarning {
			t.Errorf("event %s/%s, want Warning %s", e.Type, e.Reason, initialization.EventTimeout)
		}
		for _, want := range []string{"within 1h0m0s", "1h30m0s since", "KubeletNotReady", "image pull failed"} {
			if !strings.Contains(e.Message, want) {
				t.Errorf("message %q lacks %q", e.Message, want)
			}
		}
		switch e.InvolvedObject.(type) {
		case *karpv1.NodeClaim:
			kinds["claim"] = true
		case *v1alpha1.SCNodeClass:
			kinds["class"] = true
		}
	}
	if !kinds["claim"] || !kinds["class"] {
		t.Errorf("events on %v, want the NodeClaim and the NodeClass", kinds)
	}
	want := `
# HELP scarpenter_nodeclaims_initialization_timeout_total NodeClaims deleted because their node did not become Initialized within the initialization timeout.
# TYPE scarpenter_nodeclaims_initialization_timeout_total counter
scarpenter_nodeclaims_initialization_timeout_total{nodeclass="class",vendor="aws"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}

// Nothing is published or counted when the claim is left alone.
func TestNoObservabilityWithoutDeletion(t *testing.T) {
	_, _, rec, reg := run(t, scenario{age: time.Minute})
	if len(rec.list) != 0 {
		t.Errorf("events = %v, want none", rec.list)
	}
	if n, err := testutil.GatherAndCount(reg); err != nil || n != 0 {
		t.Errorf("series = %d (%v), want none", n, err)
	}
}

func TestMissingNodeClaim(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	ctl := initialization.New(initialization.Options{Client: c, Clock: clocktesting.NewFakeClock(now)})
	if _, err := ctl.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "gone"}}); err != nil {
		t.Fatal(err)
	}
}

// Without a Node the message says the time is counted from registration.
func TestMessageWithoutNode(t *testing.T) {
	_, _, rec, _ := run(t, scenario{age: 2 * time.Hour, noNode: true})
	if len(rec.list) == 0 || !strings.Contains(rec.list[0].Message, "since registration") {
		t.Errorf("events = %v, want a message with %q", rec.list, "since registration")
	}
}

// A failed delete is an error (the manager retries) without Event or count; the retry deletes.
func TestDeleteFailureThenRetry(t *testing.T) {
	fail := true
	ctl, c, rec, reg := build(t, scenario{age: 2 * time.Hour}, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if fail {
				return errors.New("api down")
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "claim"}}
	if _, err := ctl.Reconcile(t.Context(), req); err == nil {
		t.Fatal("no error from a failed delete")
	}
	if n, _ := testutil.GatherAndCount(reg); n != 0 || len(rec.list) != 0 {
		t.Fatalf("series = %d, events = %v after a failed delete, want none", n, rec.list)
	}
	fail = false
	if _, err := ctl.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "claim"}, &karpv1.NodeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("NodeClaim after the retry: %v, want not found", err)
	}
	if len(rec.list) != 2 {
		t.Errorf("events = %d, want 2 (NodeClaim, NodeClass)", len(rec.list))
	}
}

// The delete carries the resource version read; a conflict requeues, a NotFound is done, both
// without Event or count.
func TestDeleteConflictAndNotFound(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		requeue time.Duration
	}{
		"conflict": {apierrors.NewConflict(schema.GroupResource{Resource: "nodeclaims"}, "claim", errors.New("changed")), time.Second},
		"gone":     {apierrors.NewNotFound(schema.GroupResource{Resource: "nodeclaims"}, "claim"), 0},
	} {
		t.Run(name, func(t *testing.T) {
			var gotRV *string
			ctl, _, rec, reg := build(t, scenario{age: 2 * time.Hour}, interceptor.Funcs{
				Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, opts ...client.DeleteOption) error {
					o := &client.DeleteOptions{}
					o.ApplyOptions(opts)
					if o.Preconditions != nil {
						gotRV = o.Preconditions.ResourceVersion
					}
					return tc.err
				},
			})
			res, err := ctl.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "claim"}})
			if err != nil {
				t.Fatal(err)
			}
			if gotRV == nil || *gotRV == "" {
				t.Error("the delete has no resourceVersion precondition")
			}
			if res.RequeueAfter != tc.requeue {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.requeue)
			}
			if n, _ := testutil.GatherAndCount(reg); n != 0 || len(rec.list) != 0 {
				t.Errorf("series = %d, events = %v, want none", n, rec.list)
			}
		})
	}
}
