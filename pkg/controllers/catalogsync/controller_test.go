/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package catalogsync_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
	"github.com/SpareCores/scarpenter/pkg/controllers/catalogsync"
)

type recorder struct{ scopes chan catalog.Scope }

func (r recorder) Load(_ context.Context, req catalog.Request) (*model.Data, error) {
	r.scopes <- req.Scope
	return &model.Data{Version: "v", Scores: map[string]map[string]float64{}, Selected: map[string]map[string]model.Member{}, Invalid: map[string]string{}}, nil
}

func nodeClass(name string, mutate func(*v1alpha1.SCNodeClass)) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{AWS: &aws.Settings{Regions: []string{"us-east-1"}}}},
	}
	if mutate != nil {
		mutate(nc)
	}
	return nc
}

func TestReconcileHandsTheCatalogEveryClass(t *testing.T) {
	other := nodeClass("b", func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.Regions = []string{"eu-west-1"} })
	bad := nodeClass("bad", func(nc *v1alpha1.SCNodeClass) { nc.Spec.Kubelet.KubeReserved = map[string]string{"cpu": "lots"} })
	s := scheme.Scheme
	_ = v1alpha1.AddToScheme(s)
	reader := fake.NewClientBuilder().WithScheme(s).WithObjects(nodeClass("a", nil), other, bad).Build()

	src := recorder{scopes: make(chan catalog.Scope, 4)}
	cat := catalog.New(catalog.Options{Source: src})
	ctrl := catalogsync.New(reader, cat)
	if _, err := ctrl.Reconcile(t.Context(), reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case scope := <-src.scopes:
		if got := scope["aws"]; len(got) != 2 || got[0] != "eu-west-1" || got[1] != "us-east-1" {
			t.Errorf("scope = %v, want the union of both classes", scope)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the catalog did not read")
	}
	if st := cat.Status("a"); !st.Ready {
		t.Errorf("a: %+v", st)
	}
	// The class with unparseable settings is not served.
	if st := cat.Status("bad"); st.Ready || st.Reason != "UnknownClass" {
		t.Errorf("bad: %+v", st)
	}
}
