/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstraptoken_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/controllers/bootstraptoken"
)

type recorder struct {
	mu   sync.Mutex
	list []events.Event
}

func (r *recorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, evts...)
}

func (r *recorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.list))
	for _, e := range r.list {
		out = append(out, e.Reason+": "+e.Message)
	}
	return out
}

func claim(name, class string) *karpv1.NodeClaim {
	group := v1alpha1.Group
	kind := "SCNodeClass"
	if class != "" {
		group, kind = "karpenter.k8s.aws", class
	}
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: group, Kind: kind, Name: "default"},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				{Key: "kubernetes.io/arch", Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
			},
		},
	}
}

// TestTokenLifecycle runs the controller in a manager against envtest: the
// token of a NodeClaim goes away when it registers, and when it is deleted;
// the token of a NodeClaim that is neither stays.
func TestTokenLifecycle(t *testing.T) {
	cfg, stop, err := testutil.StartEnvWith(testutil.EnvOptions{CRDs: testutil.KarpenterCRDs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop() })
	scheme := clientgoscheme.Scheme
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	minter := bootstrap.NewMinter(bootstrap.MinterOptions{Client: c, TTL: time.Hour, Recorder: rec})

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstraptoken.New(mgr.GetAPIReader(), minter).Register(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	tokenCount := func(name string) int {
		var list corev1.SecretList
		if err := c.List(ctx, &list, client.InNamespace("kube-system"), client.MatchingLabels{bootstrap.LabelNodeClaim: name}); err != nil {
			t.Fatal(err)
		}
		return len(list.Items)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	launch := func(name, class string) *karpv1.NodeClaim {
		t.Helper()
		nc := claim(name, class)
		if err := c.Create(ctx, nc); err != nil {
			t.Fatal(err)
		}
		if _, err := minter.Mint(ctx, nc); err != nil {
			t.Fatal(err)
		}
		return nc
	}

	registering := launch("registering", "")
	deleted := launch("deleted", "")
	foreign := launch("foreign", "EC2NodeClass")

	// A sentinel proves the controller has caught up: a NodeClaim that is already
	// Registered when the controller sees it gets its token deleted. The tokens
	// of the NodeClaims that are not registered must still be there then.
	sentinel := launch("sentinel", "")
	sentinel.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	if err := c.Status().Update(ctx, sentinel); err != nil {
		t.Fatal(err)
	}
	waitFor("the sentinel's token to go", func() bool { return tokenCount("sentinel") == 0 })
	for _, name := range []string{"registering", "deleted", "foreign"} {
		if n := tokenCount(name); n != 1 {
			t.Fatalf("%s: %d tokens, want 1: nothing happened to that NodeClaim", name, n)
		}
	}

	// Registered: the token is deleted, with an Event.
	registering.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	if err := c.Status().Update(ctx, registering); err != nil {
		t.Fatal(err)
	}
	waitFor("the token of the registered NodeClaim to go", func() bool { return tokenCount("registering") == 0 })

	// Deleted: gone.
	if err := c.Delete(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	waitFor("the token of the deleted NodeClaim to go", func() bool { return tokenCount("deleted") == 0 })

	// The NodeClaim of another provider is none of our business.
	if n := tokenCount("foreign"); n != 1 {
		t.Errorf("foreign: %d tokens, want 1", n)
	}
	_ = foreign

	var registered, gone int // the sentinel registered too
	for _, m := range rec.messages() {
		switch {
		case strings.Contains(m, bootstrap.ReasonRegistered):
			registered++
		case strings.Contains(m, bootstrap.ReasonNodeClaimGone):
			gone++
		}
	}
	if registered != 2 || gone != 1 {
		t.Errorf("events: %d registered, %d gone: %v", registered, gone, rec.messages())
	}
}
