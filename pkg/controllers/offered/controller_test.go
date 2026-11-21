/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package offered_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/controllers/offered"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
)

const className = "offered"

var regions = []string{"us-east-1", "us-east-2"}

// regionGate blocks every offering of the region it is set to, as the quota
// gate does when the bucket has no headroom.
type regionGate struct{ region atomic.Value }

func (g *regionGate) Unavailable(_, _, region, _, _ string) (catalog.Reason, bool) {
	if r, _ := g.region.Load().(string); r != "" && r == region {
		return catalog.Reason{Code: catalog.QuotaGate, Detail: "bucket std, headroom 0, need 2"}, true
	}
	return catalog.Reason{}, false
}

// env is a manager running the controller against an envtest API server, with
// a catalog over the AWS fixtures.
type env struct {
	c      client.Client
	cat    *catalog.Catalog
	un     *unavailable.Cache
	clk    *clocktesting.FakeClock
	gate   *regionGate
	ctx    context.Context
	scheme *runtime.Scheme
}

func start(t *testing.T, annotations map[string]string, tune func(*offered.Options, *env)) *env {
	t.Helper()
	cfg, stop, err := testutil.StartEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop() })

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
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: className, Annotations: annotations},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors:    v1alpha1.VendorBlocks{AWS: &aws.Settings{Regions: regions}},
		},
	}
	if err := c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}

	e := &env{c: c, clk: clocktesting.NewFakeClock(time.Now()), gate: &regionGate{}, scheme: scheme}
	e.un = unavailable.New(e.clk, unavailable.TTLs{})
	e.cat = catalog.New(catalog.Options{
		Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws"), Unavailable: e.un, Gate: e.gate, Clock: e.clk,
	})
	e.cat.SetClasses([]catalog.Class{catalog.NewClass(className, catalog.Scope{"aws": regions})})
	if err := e.cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := offered.Options{Client: mgr.GetClient(), Catalog: e.cat, Interval: 200 * time.Millisecond}
	if tune != nil {
		tune(&opts, e)
	}
	if err := offered.New(opts).Register(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	e.ctx = ctx
	return e
}

func (e *env) offered() (*v1alpha1.OfferedStatus, string, error) {
	var got v1alpha1.SCNodeClass
	if err := e.c.Get(e.ctx, client.ObjectKey{Name: className}, &got); err != nil {
		return nil, "", err
	}
	return got.Status.Vendors["aws"].Offered, got.ResourceVersion, nil
}

// eventually polls until the check passes.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	var last error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if last = check(); last == nil {
			return
		}
	}
	t.Fatalf("%s: timed out; last: %v", what, last)
}

func (e *env) expect(t *testing.T, what string, ok func(*v1alpha1.OfferedStatus) bool) *v1alpha1.OfferedStatus {
	t.Helper()
	var got *v1alpha1.OfferedStatus
	eventually(t, what, func() error {
		o, _, err := e.offered()
		if err != nil {
			return err
		}
		if o == nil || !ok(o) {
			return fmt.Errorf("status %+v", o)
		}
		got = o
		return nil
	})
	return got
}

func reason(list []v1alpha1.OfferedReason, code catalog.Code) *v1alpha1.OfferedReason {
	for i := range list {
		if list[i].Reason == string(code) {
			return &list[i]
		}
	}
	return nil
}

func TestOfferedStatusFollowsTheGatesAndSettles(t *testing.T) {
	e := start(t, nil, nil)

	first := e.expect(t, "the summary", func(o *v1alpha1.OfferedStatus) bool { return o.Types > 0 })
	if first.AvailableOfferings == 0 || first.AvailableOfferings > first.Offerings {
		t.Errorf("counts: %+v", first)
	}
	if raw, _ := json.Marshal(first); len(raw) > 3<<10 {
		t.Errorf("the block is %d bytes, want under 3 KiB", len(raw))
	}

	// A quota gate on one region.
	e.gate.region.Store(regions[0])
	blocked := e.expect(t, "QuotaGate", func(o *v1alpha1.OfferedStatus) bool { return reason(o.Unavailable, catalog.QuotaGate) != nil })
	q := reason(blocked.Unavailable, catalog.QuotaGate)
	if int(q.Count) != int(blocked.Offerings-blocked.AvailableOfferings) || len(q.Types) == 0 || len(q.Types) > 5 {
		t.Errorf("QuotaGate %+v in %+v", q, blocked)
	}
	e.gate.region.Store("")

	// An unavailable mark shows as the cache class and clears with the TTL.
	its, err := e.cat.InstanceTypes(e.ctx, catalog.Query{Class: className})
	if err != nil || len(its) == 0 {
		t.Fatal(err)
	}
	o := its[0].Offerings[0]
	e.un.MarkInsufficientCapacity("aws", its[0].Name, o.Requirements.Get(corev1.LabelTopologyZone).Any(), o.CapacityType())
	e.expect(t, "UnavailableCache", func(o *v1alpha1.OfferedStatus) bool {
		r := reason(o.Unavailable, catalog.UnavailableCache)
		return r != nil && r.Count == 1 && slices.Equal(r.Types, []string{its[0].Name})
	})
	e.clk.Step(4 * time.Minute)
	e.expect(t, "the mark expires", func(o *v1alpha1.OfferedStatus) bool {
		return reason(o.Unavailable, catalog.UnavailableCache) == nil && reason(o.Unavailable, catalog.QuotaGate) == nil
	})

	// A quiet catalog causes no writes.
	_, rv, err := e.offered()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if _, again, _ := e.offered(); again != rv {
		t.Errorf("the object was written again while nothing changed: %s -> %s", rv, again)
	}

	// The apply owns nothing but its field.
	var final v1alpha1.SCNodeClass
	if err := e.c.Get(e.ctx, client.ObjectKey{Name: className}, &final); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(final.ManagedFields, func(mf metav1.ManagedFieldsEntry) bool {
		return mf.Manager == offered.FieldManager && mf.Subresource == "status"
	}) {
		t.Errorf("no managed fields for %s: %+v", offered.FieldManager, final.ManagedFields)
	}
}

const namespace = "default"

func (e *env) listing(name string) (*offered.Listing, *corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	if err := e.c.Get(e.ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm); err != nil {
		return nil, nil, err
	}
	var l offered.Listing
	if err := json.Unmarshal([]byte(cm.Data[offered.ListingKey]), &l); err != nil {
		return nil, nil, err
	}
	return &l, &cm, nil
}

func TestListingConfigMapLifecycle(t *testing.T) {
	annotated := map[string]string{v1alpha1.AnnotationPublishOffered: "true"}
	e := start(t, annotated, func(o *offered.Options, _ *env) {
		o.Reader, o.Namespace = o.Client, namespace
	})
	name := offered.ConfigMapName(className, "aws")

	var l *offered.Listing
	var cm *corev1.ConfigMap
	eventually(t, "the listing", func() (err error) {
		l, cm, err = e.listing(name)
		return err
	})
	if l.Class != className || l.Vendor != "aws" || l.Truncated || len(l.Types) < 100 || l.GeneratedAt == "" {
		t.Fatalf("listing: %q %q truncated=%v types=%d", l.Class, l.Vendor, l.Truncated, len(l.Types))
	}
	t.Logf("%d types, %d bytes", len(l.Types), len(cm.Data[offered.ListingKey]))
	if len(cm.Data[offered.ListingKey]) > offered.MaxListingBytes {
		t.Errorf("%d bytes, over the cap", len(cm.Data[offered.ListingKey]))
	}
	wantLabels := map[string]string{
		offered.LabelManagedBy: "scarpenter", offered.LabelClass: className, offered.LabelVendor: "aws",
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Kind != "SCNodeClass" || cm.OwnerReferences[0].Name != className ||
		!reflect.DeepEqual(cm.Labels, wantLabels) {
		t.Errorf("owner or labels: %+v %+v", cm.OwnerReferences, cm.Labels)
	}
	if !slices.ContainsFunc(cm.ManagedFields, func(mf metav1.ManagedFieldsEntry) bool { return mf.Manager == offered.FieldManager }) {
		t.Errorf("no managed fields for %s", offered.FieldManager)
	}
	var first string
	for name, lt := range l.Types {
		if strings.HasPrefix(name, "aws.") && lt.Available && len(lt.CapacityTypes) > 0 {
			first = name
			break
		}
	}
	if first == "" {
		t.Fatal("no available aws type")
	}

	// A gate shows in the listing.
	e.gate.region.Store(regions[0])
	eventually(t, "the gate in the listing", func() error {
		l, _, err := e.listing(name)
		if err != nil {
			return err
		}
		for _, lt := range l.Types {
			if !lt.Available && lt.Reason == "QuotaGate" {
				return nil
			}
		}
		return fmt.Errorf("no type is gated yet")
	})
	e.gate.region.Store("")
	eventually(t, "the gate clears", func() error {
		l, _, err := e.listing(name)
		if err != nil {
			return err
		}
		for _, lt := range l.Types {
			if !lt.Available && lt.Reason == "QuotaGate" {
				return fmt.Errorf("still gated")
			}
		}
		return nil
	})

	// A quiet catalog does not rewrite it before the heartbeat.
	_, cm, _ = e.listing(name)
	time.Sleep(time.Second)
	if _, again, _ := e.listing(name); again.ResourceVersion != cm.ResourceVersion {
		t.Errorf("the ConfigMap was written again while nothing changed")
	}

	// Without the annotation it goes.
	var nc v1alpha1.SCNodeClass
	if err := e.c.Get(e.ctx, client.ObjectKey{Name: className}, &nc); err != nil {
		t.Fatal(err)
	}
	delete(nc.Annotations, v1alpha1.AnnotationPublishOffered)
	if err := e.c.Update(e.ctx, &nc); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ConfigMap is deleted", func() error {
		if _, _, err := e.listing(name); !apierrors.IsNotFound(err) {
			return fmt.Errorf("still there: %v", err)
		}
		return nil
	})
}

// The listing is written again after ten minutes without a change, with a new
// generatedAt.
func TestListingHeartbeat(t *testing.T) {
	annotated := map[string]string{v1alpha1.AnnotationPublishOffered: "true"}
	e := start(t, annotated, func(o *offered.Options, e *env) {
		o.Reader, o.Namespace, o.Clock = o.Client, namespace, e.clk
	})
	name := offered.ConfigMapName(className, "aws")
	var first *offered.Listing
	eventually(t, "the listing", func() (err error) {
		first, _, err = e.listing(name)
		return err
	})
	e.clk.Step(11 * time.Minute)
	eventually(t, "the heartbeat", func() error {
		l, _, err := e.listing(name)
		if err != nil {
			return err
		}
		if l.GeneratedAt == first.GeneratedAt {
			return fmt.Errorf("generatedAt %s", l.GeneratedAt)
		}
		return nil
	})
}

func TestNoListingWithoutTheAnnotation(t *testing.T) {
	e := start(t, nil, func(o *offered.Options, _ *env) { o.Reader, o.Namespace = o.Client, namespace })
	e.expect(t, "the summary", func(o *v1alpha1.OfferedStatus) bool { return o.Types > 0 })
	var list corev1.ConfigMapList
	if err := e.c.List(e.ctx, &list, client.InNamespace(namespace), client.MatchingLabels{offered.LabelManagedBy: "scarpenter"}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d ConfigMaps without the annotation", len(list.Items))
	}
}
