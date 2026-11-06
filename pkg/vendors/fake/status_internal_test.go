//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake

import (
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

func nodeClass(name string) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors:    v1alpha1.VendorBlocks{AWS: &aws.Settings{Regions: []string{"us-east-1"}}},
		},
	}
}

func cond(conds []status.Condition, typ string) (status.Condition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return status.Condition{}, false
}

func TestStatusControllerMarksNodeClassesReady(t *testing.T) {
	cfg, stop, err := testutil.StartEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop() })
	if err := v1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	cat := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws")})
	served := vendors.NewRegistryFrom(New(DefaultOptions()))
	r := &statusReconciler{client: c, cat: cat, reg: served, clock: clock.RealClock{}}

	nc := nodeClass("default")
	if err := c.Create(ctx, nc); err != nil {
		t.Fatal(err)
	}
	reconcileOnce := func(name string) *v1alpha1.SCNodeClass {
		t.Helper()
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(nc)})
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != statusInterval {
			t.Errorf("requeue after %s", res.RequeueAfter)
		}
		got := &v1alpha1.SCNodeClass{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The catalog does not know the class yet: not ready, with the reason.
	got := reconcileOnce("default")
	if c, ok := cond(got.Status.Conditions, v1alpha1.ConditionTypeCatalogReady); !ok || c.Status != metav1.ConditionFalse || c.Reason != "UnknownClass" {
		t.Fatalf("catalog condition before the catalog knows the class: %+v", c)
	}
	if c, _ := cond(got.Status.Conditions, "Ready"); c.Status != metav1.ConditionFalse {
		t.Errorf("Ready before the catalog: %+v", c)
	}

	class, err := catalog.ClassFromNodeClass(got)
	if err != nil {
		t.Fatal(err)
	}
	cat.SetClasses([]catalog.Class{class})
	if err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	got = reconcileOnce("default")
	for _, typ := range []string{"Ready", v1alpha1.ConditionTypeCatalogReady, v1alpha1.ConditionTypeArtifactsReady} {
		c, ok := cond(got.Status.Conditions, typ)
		if !ok || c.Status != metav1.ConditionTrue || c.ObservedGeneration != got.Generation {
			t.Errorf("condition %s: %+v", typ, c)
		}
	}
	vs := got.Status.Vendors["aws"]
	if vc, ok := cond(vs.Conditions, v1alpha1.VendorConditionReady); !ok || vc.Status != metav1.ConditionTrue {
		t.Errorf("vendor condition: %+v", vs.Conditions)
	}
	if vs.Hash == "" {
		t.Fatal("no hash")
	}
	first := vs.Hash
	ltt, _ := cond(got.Status.Conditions, "Ready")

	// An unchanged object keeps hash and transition time.
	time.Sleep(1100 * time.Millisecond)
	got = reconcileOnce("default")
	if again, _ := cond(got.Status.Conditions, "Ready"); !again.LastTransitionTime.Equal(&ltt.LastTransitionTime) {
		t.Errorf("transition time moved without a change: %s -> %s", ltt.LastTransitionTime, again.LastTransitionTime)
	}
	if got.Status.Vendors["aws"].Hash != first {
		t.Error("the hash changed without a change")
	}

	assertHashFollowsTheSpec(t, c, got, first, reconcileOnce)

	// A vendor without a driver is not ready; its siblings' fields stay.
	other := &statusReconciler{client: c, cat: cat, reg: vendors.NewRegistryFrom(), clock: clock.RealClock{}}
	if _, err := other.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(nc)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if vc, _ := cond(got.Status.Vendors["aws"].Conditions, v1alpha1.VendorConditionReady); vc.Status != metav1.ConditionFalse || vc.Reason != "VendorNotEnabled" {
		t.Errorf("vendor without a driver: %+v", vc)
	}
}

// assertHashFollowsTheSpec checks that a hashed field changes the hash and a
// region change does not.
func assertHashFollowsTheSpec(t *testing.T, c client.Client, got *v1alpha1.SCNodeClass, first string,
	reconcileOnce func(string) *v1alpha1.SCNodeClass) {
	t.Helper()
	ctx := t.Context()
	patch := client.MergeFrom(got.DeepCopy())
	got.Spec.Kubelet.MaxPods = ptr.To[int32](50)
	if err := c.Patch(ctx, got, patch); err != nil {
		t.Fatal(err)
	}
	got = reconcileOnce("default")
	if got.Status.Vendors["aws"].Hash == first {
		t.Error("a kubelet change did not change the hash")
	}
	second := got.Status.Vendors["aws"].Hash
	patch = client.MergeFrom(got.DeepCopy())
	got.Spec.Vendors.AWS.Regions = []string{"us-east-1", "eu-west-1"}
	if err := c.Patch(ctx, got, patch); err != nil {
		t.Fatal(err)
	}
	if got = reconcileOnce("default"); got.Status.Vendors["aws"].Hash != second {
		t.Error("a region change changed the hash")
	}

}
