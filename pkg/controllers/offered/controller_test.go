/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package offered_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
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

// hcloudLoading are facts whose hcloud vendor is loaded only once loaded is set,
// as the driver facts of a vendor whose load keeps failing.
type hcloudLoading struct {
	catalog.StaticFacts
	loaded *atomic.Bool
}

func (f hcloudLoading) Loaded(vendor string) bool { return vendor != "hcloud" || f.loaded.Load() }

// env is a manager running the controller against an envtest API server, with
// a catalog over the AWS fixtures.
type env struct {
	c    client.Client
	cat  *catalog.Catalog
	un   *unavailable.Cache
	clk  *clocktesting.FakeClock
	gate *regionGate
	// hcloudLoaded says that the facts of hcloud are loaded (startWith only).
	hcloudLoaded atomic.Bool
	ctx          context.Context
	scheme       *runtime.Scheme
}

func start(t *testing.T, annotations map[string]string, tune func(*offered.Options, *env)) *env {
	t.Helper()
	return startWith(t, false, annotations, tune)
}

// hcloudRegions is the second vendor of startWith.
var hcloudRegions = []string{"fsn1", "nbg1"}

// startWith adds hcloud as a second vendor of the class when twoVendors is set.
func startWith(t *testing.T, twoVendors bool, annotations map[string]string, tune func(*offered.Options, *env)) *env {
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
	scope, factVendors := catalog.Scope{"aws": regions}, []string{"aws"}
	if twoVendors {
		nc.Spec.Vendors.HCloud = &hcloud.Settings{Regions: hcloudRegions}
		scope["hcloud"], factVendors = hcloudRegions, append(factVendors, "hcloud")
	}
	if err := c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}

	e := &env{c: c, clk: clocktesting.NewFakeClock(time.Now()), gate: &regionGate{}, scheme: scheme}
	e.un = unavailable.New(e.clk, unavailable.TTLs{})
	e.cat = catalog.New(catalog.Options{
		Source: catalogtest.Source(t), Facts: hcloudLoading{catalogtest.Facts(t, factVendors...), &e.hcloudLoaded},
		Unavailable: e.un, Gate: e.gate, Clock: e.clk,
	})
	e.cat.SetClasses([]catalog.Class{catalog.NewClass(className, scope)})
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
	zr, err := gzip.NewReader(bytes.NewReader(cm.BinaryData[offered.ListingKey]))
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(raw, &l); err != nil {
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
	t.Logf("%d types, %d bytes", len(l.Types), len(cm.BinaryData[offered.ListingKey]))
	if len(cm.BinaryData[offered.ListingKey]) > offered.MaxListingBytes {
		t.Errorf("%d bytes, over the cap", len(cm.BinaryData[offered.ListingKey]))
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
	checkContract(t, cm)
	checkAgreesWithInstanceTypes(t, e, l)
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

	// A gate on one region shows in the listing, on the zones of that region
	// only: the types offered in both regions stay available.
	e.gate.region.Store(regions[0])
	gatedZones := map[string]bool{}
	for _, z := range catalogtest.Facts(t, "aws").ZonesByVendor["aws"][regions[0]] {
		gatedZones[z.Name] = true
	}
	eventually(t, "the gate in the listing", func() error {
		l, cm, err := e.listing(name)
		if err != nil {
			return err
		}
		if err := gatedIn(l, gatedZones); err != nil {
			return err
		}
		checkContract(t, cm)
		return nil
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

// An earlier version wrote data.types.json: the apply of the new layout removes
// it, since the same field manager owned it.
func TestListingApplyRemovesTheOldDataKey(t *testing.T) {
	annotated := map[string]string{v1alpha1.AnnotationPublishOffered: "true"}
	name := offered.ConfigMapName(className, "aws")
	e := start(t, annotated, func(o *offered.Options, e *env) {
		o.Reader, o.Namespace = o.Client, namespace
		old := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"data":     map[string]any{"types.json": `{"old":true}`},
		}}
		if err := e.c.Apply(t.Context(), client.ApplyConfigurationFromUnstructured(old),
			client.FieldOwner(offered.FieldManager), client.ForceOwnership); err != nil {
			t.Fatal(err)
		}
	})
	eventually(t, "the new layout", func() error {
		_, cm, err := e.listing(name)
		if err != nil {
			return err
		}
		if len(cm.Data) != 0 {
			return fmt.Errorf("data keys %v", slices.Collect(maps.Keys(cm.Data)))
		}
		return nil
	})
}

// listingBytesVendors are the vendors that have a scarpenter_offered_listing_bytes series.
func listingBytesVendors(reg *prometheus.Registry) ([]string, error) {
	families, err := reg.Gather()
	if err != nil {
		return nil, err
	}
	var vendors []string
	for _, f := range families {
		if f.GetName() != "scarpenter_offered_listing_bytes" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "vendor" {
					vendors = append(vendors, l.GetValue())
				}
			}
		}
	}
	slices.Sort(vendors)
	return vendors, nil
}

// A vendor whose listing is over the cap is refused and does not hold back the
// other: hcloud has far fewer types than aws, the limit is between the two.
func TestListingOfOneVendorOverTheCapDoesNotBlockTheOther(t *testing.T) {
	annotated := map[string]string{v1alpha1.AnnotationPublishOffered: "true"}
	reg := prometheus.NewRegistry()
	e := startWith(t, true, annotated, func(o *offered.Options, e *env) {
		o.Reader, o.Namespace, o.ListingLimit, o.Registerer = o.Client, namespace, 4000, reg
		e.hcloudLoaded.Store(true)
	})
	// Both vendors are handled in one pass: the gauge has hcloud and, as aws was
	// refused, not aws.
	eventually(t, "the hcloud listing", func() error {
		vendors, err := listingBytesVendors(reg)
		if err != nil || !slices.Equal(vendors, []string{"hcloud"}) {
			return fmt.Errorf("vendors %v: %v", vendors, err)
		}
		return nil
	})
	if _, _, err := e.listing(offered.ConfigMapName(className, "hcloud")); err != nil {
		t.Error(err)
	}
	if _, _, err := e.listing(offered.ConfigMapName(className, "aws")); !apierrors.IsNotFound(err) {
		t.Errorf("aws listing over the cap: %v", err)
	}
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

// Two vendors of one class keep both summaries, and a quiet catalog then writes
// nothing: each vendor's apply must not drop the other's block. A vendor whose
// facts are not loaded is not reported and holds back no other.
func TestOfferedStatusOfTwoVendors(t *testing.T) {
	e := startWith(t, true, nil, nil)
	e.expect(t, "aws before hcloud loads", func(o *v1alpha1.OfferedStatus) bool { return o.Types > 0 })
	var nc v1alpha1.SCNodeClass
	if err := e.c.Get(e.ctx, client.ObjectKey{Name: className}, &nc); err != nil {
		t.Fatal(err)
	}
	if o := nc.Status.Vendors["hcloud"].Offered; o != nil {
		t.Fatalf("hcloud reported before its facts loaded: %+v", o)
	}
	e.hcloudLoaded.Store(true)
	var rv string
	eventually(t, "both summaries", func() error {
		var got v1alpha1.SCNodeClass
		if err := e.c.Get(e.ctx, client.ObjectKey{Name: className}, &got); err != nil {
			return err
		}
		for _, v := range []string{"aws", "hcloud"} {
			if o := got.Status.Vendors[v].Offered; o == nil || o.Types == 0 {
				return fmt.Errorf("%s: %+v", v, o)
			}
		}
		rv = got.ResourceVersion
		return nil
	})
	time.Sleep(time.Second)
	if _, again, _ := e.offered(); again != rv {
		t.Errorf("the object was written again while nothing changed: %s -> %s", rv, again)
	}
}

// checkContract decodes the ConfigMap as a reader does and checks the layout of
// the consumer contract: the keys, the field names and when they are omitted.
func checkContract(t *testing.T, cm *corev1.ConfigMap) {
	t.Helper()
	if len(cm.Data) != 0 || len(cm.BinaryData) != 1 {
		t.Fatalf("keys %v %v", cm.Data, slices.Collect(maps.Keys(cm.BinaryData)))
	}
	zr, err := gzip.NewReader(bytes.NewReader(cm.BinaryData["types.json.gz"]))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }
	if got := keys(doc); !slices.Equal(got, []string{"class", "generatedAt", "truncated", "types", "vendor"}) {
		t.Fatalf("top-level keys %v", got)
	}
	if _, err := time.Parse(time.RFC3339, doc["generatedAt"].(string)); err != nil {
		t.Errorf("generatedAt: %v", err)
	}
	if doc["class"] != className || doc["vendor"] != "aws" || doc["truncated"] != false {
		t.Errorf("header %v %v %v", doc["class"], doc["vendor"], doc["truncated"])
	}
	for name, v := range doc["types"].(map[string]any) {
		ty := v.(map[string]any)
		available := ty["available"].(bool)
		want := []string{"available", "capacityTypes", "offerings"}
		if _, ok := ty["apiReference"]; ok {
			want = append(want, "apiReference")
			if "aws."+ty["apiReference"].(string) != name {
				t.Errorf("%s: apiReference %v", name, ty["apiReference"])
			}
		}
		if !available {
			want = append(want, "reason")
			if _, ok := ty["detail"]; ok {
				want = append(want, "detail")
			}
		}
		slices.Sort(want)
		if got := keys(ty); !slices.Equal(got, want) {
			t.Fatalf("%s: keys %v, want %v", name, got, want)
		}
		if !strings.HasPrefix(name, "aws.") || ty["reason"] == "" {
			t.Errorf("%s: %v", name, ty)
		}
		cts, offerings := ty["capacityTypes"].([]any), ty["offerings"].([]any)
		if available == (len(cts) == 0) {
			t.Errorf("%s: available %v with capacity types %v", name, available, cts)
		}
		for _, raw := range offerings {
			o := raw.(map[string]any)
			want := []string{"available", "capacityType", "zones"}
			if !o["available"].(bool) {
				want = append(want, "reason")
			} else if !slices.Contains(cts, o["capacityType"]) {
				t.Errorf("%s: available offering %v not in %v", name, o, cts)
			}
			slices.Sort(want)
			if got := keys(o); !slices.Equal(got, want) || len(o["zones"].([]any)) == 0 {
				t.Fatalf("%s: offering %v", name, o)
			}
		}
	}
}

// checkAgreesWithInstanceTypes checks the listing against what the catalog
// hands Karpenter: the same types, and an available zone in the listing is one
// with an available offering of that capacity type.
func checkAgreesWithInstanceTypes(t *testing.T, e *env, l *offered.Listing) {
	t.Helper()
	its, err := e.cat.InstanceTypes(e.ctx, catalog.Query{Class: className})
	if err != nil {
		t.Fatal(err)
	}
	given := map[string]map[string]bool{} // type -> capacity type/zone -> available
	for _, it := range its {
		given[it.Name] = map[string]bool{}
		for _, o := range it.Offerings {
			given[it.Name][o.CapacityType()+"/"+o.Zone()] = o.Available
		}
	}
	for name, lt := range l.Types {
		g, ok := given[name]
		if ok == (len(lt.Offerings) == 0) {
			t.Errorf("%s: given to Karpenter %v, listed %+v", name, ok, lt)
			continue
		}
		for _, o := range lt.Offerings {
			for _, z := range o.Zones {
				if avail := g[o.CapacityType+"/"+z]; o.Available != avail {
					t.Errorf("%s %s/%s: listed available=%v, given available=%v", name, o.CapacityType, z, o.Available, avail)
				}
			}
		}
	}
	for name, g := range given {
		lt, ok := l.Types[name]
		for key, avail := range g {
			ct, z, _ := strings.Cut(key, "/")
			listed := slices.ContainsFunc(lt.Offerings, func(o offered.ListingOffering) bool {
				return o.CapacityType == ct && o.Available == avail && slices.Contains(o.Zones, z)
			})
			if !ok || !listed {
				t.Errorf("%s %s is given to Karpenter (available=%v) but not listed so", name, key, avail)
			}
		}
	}
}

// gatedIn checks a listing with a quota gate on the zones gatedZones: QuotaGate
// is on those zones only, none of them is available, a type gated everywhere
// says QuotaGate with the bucket, and some type stays available elsewhere.
func gatedIn(l *offered.Listing, gatedZones map[string]bool) error {
	partly := false
	for n, lt := range l.Types {
		for _, o := range lt.Offerings {
			for _, z := range o.Zones {
				if (o.Reason == "QuotaGate" && !gatedZones[z]) || (o.Available && gatedZones[z]) {
					return fmt.Errorf("zone %s of %s: %+v", z, n, o)
				}
			}
			partly = partly || (lt.Available && o.Reason == "QuotaGate")
		}
		allGated := len(lt.Offerings) > 0 && !slices.ContainsFunc(lt.Offerings, func(o offered.ListingOffering) bool {
			return o.Reason != "QuotaGate"
		})
		if allGated && (lt.Available || lt.Reason != "QuotaGate" || !strings.HasPrefix(lt.Detail, "bucket ")) {
			return fmt.Errorf("%s: %+v", n, lt)
		}
	}
	if !partly {
		return fmt.Errorf("no type is gated in one region yet")
	}
	return nil
}
