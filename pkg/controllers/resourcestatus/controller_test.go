/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resourcestatus_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/alicloud"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/controllers/resourcestatus"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/resources/resourcestest"
	"github.com/SpareCores/scarpenter/pkg/vendors"
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

type regions []string

func (r regions) Regions(string) []string { return r }

type view struct {
	status, reason, message string
	resources               []v1alpha1.ResourceStatus
}

// watcher reads the ResourcesReady state of a NodeClass.
type watcher struct {
	t     *testing.T
	ctx   context.Context
	c     client.Client
	class string
}

func (w *watcher) read() (view, error) {
	var got v1alpha1.SCNodeClass
	if err := w.c.Get(w.ctx, client.ObjectKey{Name: w.class}, &got); err != nil {
		return view{}, err
	}
	vs, ok := got.Status.Vendors["aws"]
	if !ok {
		return view{}, fmt.Errorf("no vendor status yet")
	}
	for _, cond := range vs.Conditions {
		if cond.Type == v1alpha1.VendorConditionResourcesReady {
			return view{string(cond.Status), cond.Reason, cond.Message, vs.Resources}, nil
		}
	}
	return view{}, fmt.Errorf("no ResourcesReady condition yet")
}

func (w *watcher) expect(what string, ok func(view) bool) view {
	w.t.Helper()
	var last view
	var lastErr error
	err := wait.PollUntilContextTimeout(w.ctx, 50*time.Millisecond, 60*time.Second, true, func(context.Context) (bool, error) {
		last, lastErr = w.read()
		return lastErr == nil && ok(last), nil
	})
	if err != nil {
		w.t.Fatalf("%s: timed out; last %+v, %v", what, last, lastErr)
	}
	return last
}

func TestResourceStatus(t *testing.T) {
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

	const className = "rs"
	allRegions := regions{"us-east-1", "us-east-2"}
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: className},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors: v1alpha1.VendorBlocks{AWS: &aws.Settings{Resources: &aws.Resources{
				Subnets: &common.Resource{Use: &common.ResourceUse{
					Tags: map[string]string{"scarpenter.sparecores.com/use": "true"}}},
				SecurityGroups: &common.Resource{Create: &common.ResourceCreate{
					Timing: &common.Timing{Mode: common.TimingPreCreate, Regions: []string{"us-east-1"}}}},
			}}},
		},
	}
	if err := c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}

	clk := clocktesting.NewFakeClock(time.Now())
	drv := resourcestest.New("aws", clk)
	drv.Existing = []resourcestest.Existing{{
		Kind: resourcestest.Subnets, Region: "us-east-1", ID: "subnet-a",
		Tags: map[string]string{"scarpenter.sparecores.com/use": "true"},
	}}
	rec := &recorder{}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(drv), Reader: mgr.GetClient(), Clock: clk, ClusterName: "my-cluster", Recorder: rec,
	})
	if err := resourcestatus.New(resourcestatus.Options{
		Client: mgr.GetClient(), Engine: engine, Regions: allRegions, Recorder: rec, Interval: 100 * time.Millisecond,
	}).Register(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	w := &watcher{t: t, ctx: ctx, c: c, class: className}

	// us-east-2 has no subnet that matches: its offerings are unavailable, with the reason.
	v := w.expect("a region without its use resource", func(v view) bool { return v.status == "False" })
	if v.reason != resourcestatus.ReasonUnavailable || !strings.Contains(v.message, "us-east-2") ||
		!strings.Contains(v.message, "subnets") || strings.Contains(v.message, "us-east-1") {
		t.Errorf("condition %q: %q", v.reason, v.message)
	}
	if !down(engine.UnavailableFor(className, "aws", "m7i.large", "us-east-2", "z", "on-demand")) ||
		down(engine.UnavailableFor(className, "aws", "m7i.large", "us-east-1", "z", "on-demand")) {
		t.Error("the engine must gate us-east-2 only")
	}
	kinds := map[string]bool{}
	for _, r := range v.resources {
		kinds[r.Kind+"/"+r.Region] = r.Managed
	}
	if managed, ok := kinds["subnets/us-east-1"]; !ok || managed {
		t.Errorf("the used subnet must be listed unmanaged: %+v", v.resources)
	}
	// The preCreate security group exists in us-east-1 only, and is managed.
	if managed, ok := kinds["securityGroups/us-east-1"]; !ok || !managed || len(kinds) != 2 {
		t.Errorf("the preCreate resource is not in the status: %+v", v.resources)
	}
	if msgs := rec.messages(); !slices.ContainsFunc(msgs, func(m string) bool {
		return strings.HasPrefix(m, "ResourcesUnavailable:") && strings.Contains(m, "us-east-2")
	}) {
		t.Errorf("no Event for the unavailable region: %v", msgs)
	}

	// The user brings the missing subnet: the region becomes available (the
	// engine trusts a recent answer for 30 s, the fake clock moves past it).
	drv.Existing = append(drv.Existing, resourcestest.Existing{
		Kind: resourcestest.Subnets, Region: "us-east-2", ID: "subnet-b",
		Tags: map[string]string{"scarpenter.sparecores.com/use": "true"},
	})
	clk.Step(time.Minute)
	v = w.expect("the subnet appears", func(v view) bool { return v.status == "True" })
	if v.reason != resourcestatus.ReasonReady || v.message != "" || len(v.resources) != 3 {
		t.Errorf("ready state: %+v", v)
	}
	if down(engine.UnavailableFor(className, "aws", "m7i.large", "us-east-2", "z", "on-demand")) {
		t.Error("the region is still gated")
	}

	checkOwnership(ctx, t, c, className)
}

// checkOwnership checks that the status is owned by this controller's manager.
func checkOwnership(ctx context.Context, t *testing.T, c client.Client, name string) {
	t.Helper()
	var final v1alpha1.SCNodeClass
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &final); err != nil {
		t.Fatal(err)
	}
	owned := false
	for _, mf := range final.ManagedFields {
		owned = owned || (mf.Manager == resourcestatus.FieldManager && mf.Subresource == "status")
	}
	if !owned {
		t.Errorf("the status is not owned by %s: %+v", resourcestatus.FieldManager, final.ManagedFields)
	}
}

// TestResourceStatusCountsPastTheCap: the status lists at most 128 resources;
// with more, resourceCounts says how many there are per region and kind, and
// every entry carries the name the vendor gave the resource.
func TestResourceStatusCountsPastTheCap(t *testing.T) {
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
	const className = "counts"
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: className},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors: v1alpha1.VendorBlocks{AWS: &aws.Settings{Resources: &aws.Resources{
				Subnets: &common.Resource{Use: &common.ResourceUse{
					Tags: map[string]string{"use": "true"}, Select: common.SelectAll}},
			}}},
		},
	}
	if err := c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	clk := clocktesting.NewFakeClock(time.Now())
	drv := resourcestest.New("aws", clk)
	for i := range 130 {
		drv.Existing = append(drv.Existing, resourcestest.Existing{
			Kind: resourcestest.Subnets, Region: "us-east-1", ID: fmt.Sprintf("subnet-%03d", i), Name: fmt.Sprintf("n-%03d", i),
			Tags: map[string]string{"use": "true"}})
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	engine := resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(drv), Reader: mgr.GetClient(), Clock: clk, ClusterName: "my-cluster", Recorder: rec,
	})
	if err := resourcestatus.New(resourcestatus.Options{
		Client: mgr.GetClient(), Engine: engine, Regions: regions{"us-east-1"}, Recorder: rec, Interval: 100 * time.Millisecond,
	}).Register(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	var got v1alpha1.SCNodeClass
	err = wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 60*time.Second, true, func(context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKey{Name: className}, &got); err != nil {
			return false, err
		}
		return len(got.Status.Vendors["aws"].Resources) > 0, nil
	})
	if err != nil {
		t.Fatalf("no resources in the status: %v", err)
	}
	vs := got.Status.Vendors["aws"]
	if len(vs.Resources) != resources.MaxStatusEntries {
		t.Errorf("%d entries, want the cap of %d", len(vs.Resources), resources.MaxStatusEntries)
	}
	if vs.Resources[0].Name != "n-000" {
		t.Errorf("the entry has no name: %+v", vs.Resources[0])
	}
	if len(vs.ResourceCounts) != 1 || vs.ResourceCounts[0] != (v1alpha1.ResourceCount{Kind: "subnets", Region: "us-east-1", Count: 130}) {
		t.Errorf("counts %+v, want 130 subnets in us-east-1", vs.ResourceCounts)
	}
}

// TestResourceStatusOfTwoVendors: the status of every vendor of a class goes in
// one apply, so both vendors keep their condition and resources; a quiet
// reconcile writes nothing; a vendor that leaves the class loses its entries and
// the other keeps its own.
func TestResourceStatusOfTwoVendors(t *testing.T) {
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
	const className = "rs2"
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: className},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors: v1alpha1.VendorBlocks{
				AWS: &aws.Settings{Regions: []string{"us-east-1"}, Resources: &aws.Resources{
					Subnets: &common.Resource{Use: &common.ResourceUse{
						Tags: map[string]string{"scarpenter.sparecores.com/use": "true"}}},
				}},
				Alicloud: &alicloud.Settings{Regions: []string{"cn-hangzhou"}, Resources: &alicloud.Resources{
					Subnets: &common.Resource{Use: &common.ResourceUse{
						Tags: map[string]string{"scarpenter.sparecores.com/use": "true"}}},
				}},
			},
		},
	}
	if err := c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}

	clk := clocktesting.NewFakeClock(time.Now())
	awsDrv := resourcestest.New("aws", clk)
	awsDrv.Existing = []resourcestest.Existing{{
		Kind: resourcestest.Subnets, Region: "us-east-1", ID: "subnet-a",
		Tags: map[string]string{"scarpenter.sparecores.com/use": "true"},
	}}
	aliDrv := resourcestest.New("alicloud", clk)
	aliDrv.Existing = []resourcestest.Existing{{
		Kind: resourcestest.Subnets, Region: "cn-hangzhou", ID: "vsw-a",
		Tags: map[string]string{"scarpenter.sparecores.com/use": "true"},
	}}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	engine := resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(awsDrv, aliDrv), Reader: mgr.GetClient(), Clock: clk,
		ClusterName: "my-cluster", Recorder: rec,
	})
	if err := resourcestatus.New(resourcestatus.Options{
		Client: mgr.GetClient(), Engine: engine, Recorder: rec, Interval: 100 * time.Millisecond,
	}).Register(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	get := func() v1alpha1.SCNodeClass {
		var got v1alpha1.SCNodeClass
		if err := c.Get(ctx, client.ObjectKey{Name: className}, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	ready := func(got v1alpha1.SCNodeClass, vendor string) bool {
		vs := got.Status.Vendors[vendor]
		return len(vs.Resources) > 0 && slices.ContainsFunc(vs.Conditions, func(c status.Condition) bool {
			return c.Type == v1alpha1.VendorConditionResourcesReady && c.Status == metav1.ConditionTrue
		})
	}
	waitFor := func(what string, ok func(v1alpha1.SCNodeClass) bool) v1alpha1.SCNodeClass {
		t.Helper()
		var got v1alpha1.SCNodeClass
		err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 60*time.Second, true, func(context.Context) (bool, error) {
			got = get()
			return ok(got), nil
		})
		if err != nil {
			t.Fatalf("%s: timed out; vendors %+v", what, got.Status.Vendors)
		}
		return got
	}

	got := waitFor("both vendors", func(got v1alpha1.SCNodeClass) bool { return ready(got, "aws") && ready(got, "alicloud") })
	time.Sleep(time.Second)
	again := get()
	if again.ResourceVersion != got.ResourceVersion {
		t.Errorf("the object was written again while nothing changed: %s -> %s", got.ResourceVersion, again.ResourceVersion)
	}
	for _, vendor := range []string{"aws", "alicloud"} {
		if !again.Status.Vendors[vendor].Conditions[0].LastTransitionTime.Equal(&got.Status.Vendors[vendor].Conditions[0].LastTransitionTime) {
			t.Errorf("%s: lastTransitionTime moved while nothing changed", vendor)
		}
	}

	cur := get()
	cur.Spec.Vendors.Alicloud = nil
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	waitFor("alicloud removed", func(got v1alpha1.SCNodeClass) bool {
		as := got.Status.Vendors["alicloud"]
		return len(as.Resources) == 0 && len(as.Conditions) == 0 && ready(got, "aws")
	})
}

// alicloudRegions knows the regions of alicloud only once its driver is loaded.
type alicloudRegions struct{ loaded atomic.Bool }

func (r *alicloudRegions) Regions(vendor string) []string {
	if vendor == "alicloud" && r.loaded.Load() {
		return []string{"cn-hangzhou"}
	}
	return nil
}

// TestResourceStatusKeepsAVendorWithUnknownRegions: a vendor whose regions are
// not known (its driver is not loaded) keeps its entries, with their old
// observedGeneration, while the other vendor is updated.
func TestResourceStatusKeepsAVendorWithUnknownRegions(t *testing.T) {
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
	const className = "rs3"
	use := &common.Resource{Use: &common.ResourceUse{Tags: map[string]string{"scarpenter.sparecores.com/use": "true"}}}
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: className},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors: v1alpha1.VendorBlocks{
				AWS:      &aws.Settings{Regions: []string{"us-east-1"}, Resources: &aws.Resources{Subnets: use}},
				Alicloud: &alicloud.Settings{Resources: &alicloud.Resources{Subnets: use}},
			},
		},
	}
	if err := c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}

	clk := clocktesting.NewFakeClock(time.Now())
	awsDrv := resourcestest.New("aws", clk)
	awsDrv.Existing = []resourcestest.Existing{{
		Kind: resourcestest.Subnets, Region: "us-east-1", ID: "subnet-a",
		Tags: map[string]string{"scarpenter.sparecores.com/use": "true"},
	}}
	aliDrv := resourcestest.New("alicloud", clk)
	aliDrv.Existing = []resourcestest.Existing{{
		Kind: resourcestest.Subnets, Region: "cn-hangzhou", ID: "vsw-a",
		Tags: map[string]string{"scarpenter.sparecores.com/use": "true"},
	}}
	known := &alicloudRegions{}
	known.loaded.Store(true)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	engine := resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(awsDrv, aliDrv), Reader: mgr.GetClient(), Clock: clk,
		ClusterName: "my-cluster", Recorder: rec,
	})
	if err := resourcestatus.New(resourcestatus.Options{
		Client: mgr.GetClient(), Engine: engine, Regions: known, Recorder: rec, Interval: 100 * time.Millisecond,
	}).Register(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor := func(what string, ok func(v1alpha1.SCNodeClass) bool) v1alpha1.SCNodeClass {
		t.Helper()
		var got v1alpha1.SCNodeClass
		err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 60*time.Second, true, func(context.Context) (bool, error) {
			if err := c.Get(ctx, client.ObjectKey{Name: className}, &got); err != nil {
				return false, err
			}
			return ok(got), nil
		})
		if err != nil {
			t.Fatalf("%s: timed out; vendors %+v", what, got.Status.Vendors)
		}
		return got
	}
	observed := func(got v1alpha1.SCNodeClass, vendor string) int64 {
		vs := got.Status.Vendors[vendor]
		if len(vs.Resources) == 0 || len(vs.Conditions) == 0 {
			return 0
		}
		return vs.Conditions[0].ObservedGeneration
	}
	waitFor("both vendors", func(got v1alpha1.SCNodeClass) bool {
		return observed(got, "aws") == 1 && observed(got, "alicloud") == 1
	})

	// The regions of alicloud become unknown, and a spec change makes aws move on.
	known.loaded.Store(false)
	var cur v1alpha1.SCNodeClass
	if err := c.Get(ctx, client.ObjectKey{Name: className}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Kubernetes.Version = "v1.37.2"
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	waitFor("aws updated, alicloud kept", func(got v1alpha1.SCNodeClass) bool {
		return observed(got, "aws") == 2 && observed(got, "alicloud") == 1
	})
}

// down drops the reason of a gate's answer.
func down(_ catalog.Reason, isDown bool) bool { return isDown }
