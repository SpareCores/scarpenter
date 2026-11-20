/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package operator_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samber/lo"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/model"
	"github.com/SpareCores/scarpenter/pkg/operator"
	"github.com/SpareCores/scarpenter/pkg/operator/options"
)

// TestCatalogFollowsTheNodeClasses runs the catalog and its controller in a
// manager against envtest and the replayed Keeper snapshot: an SCNodeClass makes
// the catalog read its vendor and regions, and the types appear.
func TestCatalogFollowsTheNodeClasses(t *testing.T) {
	cfg, stop, err := testutil.StartEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	lo.Must0(v1alpha1.AddToScheme(scheme.Scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&operator.Operator{Operator: &coreoperator.Operator{Manager: mgr}, Options: &options.Options{KeeperAPIKeyFile: "/does/not/exist"}}).
		WithCatalog(catalog.StaticFacts{}, nil, nil, nil); err == nil {
		t.Error("a missing API key file was accepted")
	}

	srv := catalogtest.Server(t)
	op := &operator.Operator{Operator: &coreoperator.Operator{Manager: mgr}, Options: &options.Options{
		KeeperURL: srv.URL, CatalogRefreshInterval: time.Hour, CatalogMaxAge: 24 * time.Hour,
	}}
	cat, err := op.WithCatalog(catalogtest.Facts(t, "aws"), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(t.Context()) }()

	nodeClass := object(t, `
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: bench}
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network: {template: direct}
  vendors: {aws: {regions: [us-east-1]}}
`)
	if err := c.Create(t.Context(), nodeClass); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the catalog to be ready", func() error {
		if st := cat.Status("bench"); !st.Ready {
			return &notReady{st}
		}
		return nil
	})
	its, err := cat.InstanceTypes(t.Context(), catalog.Query{Class: "bench"})
	if err != nil || len(its) < 1000 {
		t.Fatalf("%d instance types, %v", len(its), err)
	}
}

// hangingSource is a Keeper that never answers.
type hangingSource struct{}

func (hangingSource) Load(ctx context.Context, _ catalog.Request) (*model.Data, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestWarmCatalogLoadsBeforeTheManagerStarts: with an SCNodeClass in the
// cluster, WarmCatalog fills the catalog while the manager (and with it the
// catalog-sync controller) has not started, so the first instance-type query
// of Karpenter core sees the types. When Keeper hangs, it returns at the
// timeout and the catalog stays empty.
func TestWarmCatalogLoadsBeforeTheManagerStarts(t *testing.T) {
	cfg, stop, err := testutil.StartEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	lo.Must0(v1alpha1.AddToScheme(scheme.Scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(t.Context(), object(t, `
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: warm}
spec:
  flatcar: {version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network: {template: direct}
  vendors: {aws: {regions: [us-east-1]}}
`)); err != nil {
		t.Fatal(err)
	}
	op := &operator.Operator{Operator: &coreoperator.Operator{Manager: mgr}, Options: &options.Options{}}

	srv := catalogtest.Server(t)
	cat := catalog.New(catalog.Options{
		Source: catalog.NewKeeperSource(catalog.KeeperOptions{URL: srv.URL}),
		Facts:  catalogtest.Facts(t, "aws"),
	})
	op.WarmCatalog(t.Context(), cat, time.Minute)
	its, err := cat.InstanceTypes(t.Context(), catalog.Query{Class: "warm"})
	if err != nil || len(its) < 1000 {
		t.Fatalf("after the warm-up: %d instance types, %v", len(its), err)
	}

	slow := catalog.New(catalog.Options{Source: hangingSource{}, Facts: catalogtest.Facts(t, "aws")})
	begin := time.Now()
	op.WarmCatalog(t.Context(), slow, 200*time.Millisecond)
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("the warm-up took %s, want about the timeout", d)
	}
	its, err = slow.InstanceTypes(t.Context(), catalog.Query{Class: "warm"})
	if err != nil || len(its) != 0 {
		t.Errorf("after the timeout: %d instance types, %v; want none", len(its), err)
	}
}

type notReady struct{ st catalog.Status }

func (n *notReady) Error() string { return "catalog not ready: " + n.st.Reason + " " + n.st.Message }
