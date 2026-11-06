//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"context"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/awslabs/operatorpkg/status"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/controllers/nodeclass"
	"github.com/SpareCores/scarpenter/pkg/quota"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

// keeperURL is a replay server of the committed snapshot shared by every test.
var keeperURL string

func TestMain(m *testing.M) {
	h, err := keepertest.NewHandler(catalogtest.SnapshotDir())
	if err != nil {
		panic(err)
	}
	srv := httptest.NewServer(h)
	keeperURL = srv.URL
	code := m.Run()
	srv.Close()
	os.Exit(code)
}

const (
	cluster   = "my-cluster"
	cmNS      = "scarpenter-system"
	region1   = "us-east-1"
	region2   = "us-east-2"
	className = "default"
)

// env is a CloudProvider over the real catalog (the committed AWS snapshot,
// two regions), the fake driver and the real quota tracker.
type env struct {
	t        *testing.T
	cp       *cloudprovider.CloudProvider
	client   client.Client
	driver   *fakedriver.Driver
	un       *unavailable.Cache
	tracker  *quota.Tracker
	cat      *catalog.Catalog
	registry *vendors.Registry
	tokens   *recordingTokens
	events   *recordingEvents
	nextUID  int
	mu       sync.Mutex
	clock    *testingclock.FakeClock
}

func (e *env) advance(d time.Duration) { e.clock.Step(d) }

func unavailableOf(o offeringOf) unavailable.Offering {
	return unavailable.Offering{
		Vendor: "aws", InstanceType: o.typeName, Region: o.region, Zone: o.zone, CapacityType: o.capacityType,
	}
}

func unavailableOfRegion(o offeringOf) unavailable.Offering {
	return unavailable.Offering{Vendor: "aws", InstanceType: "aws.none", Region: o.region, Zone: "none", CapacityType: o.capacityType}
}

type envOptions struct {
	config   map[string]string
	regions  []string
	attempts int
	// ready=false leaves the vendor not ready.
	notReady bool
	// transform edits the NodeClass before it is created.
	transform func(*v1alpha1.SCNodeClass)
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	if o.regions == nil {
		o.regions = []string{region1, region2}
	}
	ctx := t.Context()
	scheme := clientgoscheme.Scheme
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cmNS, Name: fakedriver.DefaultConfigName}, Data: o.config}
	nc := nodeClass(o.regions)
	if o.transform != nil {
		o.transform(nc)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, nc, nodePool()).
		WithStatusSubresource(&v1alpha1.SCNodeClass{}, &karpv1.NodeClaim{}).Build()

	opts := fakedriver.DefaultOptions()
	opts.ConfigTTL = 0
	opts.StateInterval = time.Hour
	drv := fakedriver.New(opts)
	if err := drv.Init(ctx, vendors.Config{ClusterName: cluster, Client: kube, Reader: kube, Log: logr.Discard(), KeeperURL: keeperURL}); err != nil {
		t.Fatal(err)
	}
	reg := vendors.NewRegistryFrom(drv)
	clk := testingclock.NewFakeClock(time.Now())
	un := unavailable.New(clk, unavailable.TTLs{InsufficientCapacity: time.Minute})
	e := &env{t: t, client: kube, driver: drv, un: un, registry: reg, tokens: &recordingTokens{}, events: &recordingEvents{}, clock: clk}

	e.tracker = quota.New(quota.Options{
		Registry: reg, Unavailable: un, Clock: clk,
		Scope:    func() map[string][]string { return map[string][]string{"aws": o.regions} },
		Debounce: time.Millisecond,
	})
	f := facts.New(facts.Options{Registry: reg, Launchable: e.tracker})
	if err := f.Load(ctx); err != nil {
		t.Fatal(err)
	}
	e.cat = catalog.New(catalog.Options{
		Source: catalogtest.SourceAt(keeperURL), Facts: f, Unavailable: un, Gate: e.tracker, Clock: clk,
	})
	class, err := catalog.ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	e.cat.SetClasses([]catalog.Class{class})
	if err := e.cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.tracker.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if o.notReady {
		return e.build(kube, f, reg, o)
	}
	e.markReady(nc)
	return e.build(kube, f, reg, o)
}

func (e *env) build(kube client.Client, f *facts.Facts, reg *vendors.Registry, o envOptions) *env {
	e.cp = cloudprovider.New(cloudprovider.Options{
		Client: kube, Catalog: e.cat, Registry: reg, Unavailable: e.un, Quota: e.tracker,
		Scope:    &scope.Resolver{Reader: kube, Registry: reg, Regions: f},
		Recorder: e.events, Tokens: e.tokens, ClusterName: cluster, MaxLaunchAttempts: o.attempts, Clock: e.clock,
	})
	return e
}

// markReady stands in for the NodeClass controller: Ready, the vendor ready, the
// hash.
func (e *env) markReady(nc *v1alpha1.SCNodeClass) {
	e.t.Helper()
	hash, err := nodeclass.Hash(nc, "aws", nodeclass.Inputs{})
	if err != nil {
		e.t.Fatal(err)
	}
	cur := &v1alpha1.SCNodeClass{}
	if err := e.client.Get(e.t.Context(), types.NamespacedName{Name: nc.Name}, cur); err != nil {
		e.t.Fatal(err)
	}
	// Ready is derived from its dependents, so they must all be true.
	for _, typ := range []string{"Ready", "CatalogReady", "ArtifactsReady", "ValidationSucceeded"} {
		cur.Status.Conditions = append(cur.Status.Conditions, status.Condition{
			Type: typ, Status: metav1.ConditionTrue, Reason: typ, ObservedGeneration: cur.Generation, LastTransitionTime: metav1.Now(),
		})
	}
	cur.Status.Vendors = map[string]v1alpha1.VendorStatus{"aws": {
		Hash: hash,
		Conditions: []status.Condition{{
			Type: v1alpha1.VendorConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now(),
		}},
	}}
	if err := e.client.Status().Update(e.t.Context(), cur); err != nil {
		e.t.Fatal(err)
	}
}

func nodeClass(regions []string) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: className, Generation: 1},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Vendors:    v1alpha1.VendorBlocks{AWS: &aws.Settings{Regions: regions}},
		},
	}
}

func nodePool() *karpv1.NodePool {
	return &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: className},
		}}},
	}
}

func req(key string, op corev1.NodeSelectorOperator, values ...string) karpv1.NodeSelectorRequirementWithMinValues {
	return karpv1.NodeSelectorRequirementWithMinValues{Key: key, Operator: op, Values: values}
}

// claimOption edits a NodeClaim before it is created.
type claimOption func(*karpv1.NodeClaim)

// newClaim returns a NodeClaim of the NodeClass that allows the instance types.
func (e *env) newClaim(name string, typeNames []string, opts ...claimOption) *karpv1.NodeClaim {
	e.nextUID++
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types2UID(e.nextUID), Labels: map[string]string{karpv1.NodePoolLabelKey: "default"},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: className},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				req(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, typeNames...),
			},
		},
	}
	for _, o := range opts {
		o(nc)
	}
	return nc
}

func types2UID(n int) types.UID {
	return types.UID("uid-" + string(rune('a'+n%26)) + "-" + time.Duration(n).String())
}

// calls returns the fake driver's recorded vendor calls of an operation.
func (e *env) calls(op string) []fakedriver.CallRecord {
	e.t.Helper()
	return e.driver.Calls(op)
}

// setConfig writes a key of the fake's steering ConfigMap.
func (e *env) setConfig(key, value string) {
	e.t.Helper()
	cm := &corev1.ConfigMap{}
	k := types.NamespacedName{Namespace: cmNS, Name: fakedriver.DefaultConfigName}
	if err := e.client.Get(e.t.Context(), k, cm); err != nil {
		e.t.Fatal(err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = value
	if err := e.client.Update(e.t.Context(), cm); err != nil {
		e.t.Fatal(err)
	}
}

// recordingTokens counts mints and deletes.
type recordingTokens struct {
	mu              sync.Mutex
	minted, deleted []string
}

func (r *recordingTokens) Mint(_ context.Context, nc *karpv1.NodeClaim) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.minted = append(r.minted, nc.Name)
	return "abcdef.0123456789abcdef", nil
}

func (r *recordingTokens) Delete(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, name)
	return nil
}

var _ cloudprovider.TokenMinter = (*recordingTokens)(nil)

func contains(list []string, s string) bool { return slices.Contains(list, s) }

// recordingEvents keeps the published events.
type recordingEvents struct {
	mu        sync.Mutex
	published []events.Event
}

func (r *recordingEvents) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, evts...)
}

func (r *recordingEvents) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.published))
	for _, e := range r.published {
		out = append(out, e.Reason)
	}
	return out
}

func clientKey(name string) types.NamespacedName { return types.NamespacedName{Name: name} }

// newClaimSafe is newClaim for concurrent callers.
func (e *env) newClaimSafe(name string, typeNames []string, opts ...claimOption) *karpv1.NodeClaim {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.newClaim(name, typeNames, opts...)
}
