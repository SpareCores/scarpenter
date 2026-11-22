/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package quotastatus_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/hcloud"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/controllers/quotastatus"
	"github.com/SpareCores/scarpenter/pkg/quota"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// driver is an AWS-like vendor whose every type consumes one unit of the bucket
// "std" per instance, in every capacity type.
type driver struct {
	vendor  string
	mu      sync.Mutex
	buckets map[string]vendors.Bucket
}

func (d *driver) set(region string, limit, used int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buckets[region] = vendors.Bucket{ID: "std", Scope: vendors.QuotaScopeRegion, Unit: "vCPU", Limit: limit, Used: used}
}

func (d *driver) Vendor() string                                    { return d.vendor }
func (d *driver) Init(context.Context, vendors.Config) error        { return nil }
func (d *driver) Regions(context.Context) ([]vendors.Region, error) { return nil, nil }
func (d *driver) Zones(context.Context, string) ([]vendors.Zone, error) {
	return nil, nil
}
func (d *driver) ResolveImage(context.Context, vendors.ImageRequest) (*vendors.Image, error) {
	return nil, nil
}
func (d *driver) CreateInstance(context.Context, vendors.CreateRequest) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) GetInstance(context.Context, vendors.InstanceRef) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) FindInstance(context.Context, string, map[vendors.TagKey]string) (*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) ListInstances(context.Context, []string, map[vendors.TagKey]string) ([]*vendors.Instance, error) {
	return nil, nil
}
func (d *driver) DeleteInstance(context.Context, vendors.InstanceRef) error { return nil }
func (d *driver) TagKey(k vendors.TagKey) string                            { return vendors.DefaultTagKey(k) }
func (d *driver) GlobalQuotas(context.Context) (*vendors.QuotaSnapshot, error) {
	return &vendors.QuotaSnapshot{}, nil
}
func (d *driver) Consumption(string, string) ([]vendors.Consumption, bool) {
	return []vendors.Consumption{{Bucket: "std", Amount: 1}}, true
}
func (d *driver) Quotas(_ context.Context, region string) (*vendors.QuotaSnapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return &vendors.QuotaSnapshot{Buckets: []vendors.Bucket{d.buckets[region]}}, nil
}

type recorder struct {
	mu   sync.Mutex
	list []events.Event
}

func (r *recorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, evts...)
}

func (r *recorder) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.list))
	for _, e := range r.list {
		out = append(out, e.Reason+": "+e.Message)
	}
	return out
}

func TestQuotaStatus(t *testing.T) {
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

	const className = "qs"
	regions := []string{"us-east-1", "us-east-2"}
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: className},
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

	cat := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws")})
	cat.SetClasses([]catalog.Class{catalog.NewClass(className, catalog.Scope{"aws": regions})})
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	drv := &driver{vendor: "aws", buckets: map[string]vendors.Bucket{}}
	for _, r := range regions {
		drv.set(r, 100, 0)
	}
	tracker := quota.New(quota.Options{
		Registry: vendors.NewRegistryFrom(drv),
		Scope:    func() map[string][]string { return map[string][]string{"aws": regions} },
	})
	refresh := func(region string, limit, used int64) {
		drv.set(region, limit, used)
		if err := tracker.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	rec := &recorder{}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := quotastatus.New(quotastatus.Options{
		Client: mgr.GetClient(), Catalog: cat, Tracker: tracker, Recorder: rec, Interval: 300 * time.Millisecond,
	}).Register(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	type entry struct{ region, capacityType, reason string }
	state := func() (condStatus, reason string, entries []entry, err error) {
		var got v1alpha1.SCNodeClass
		if err := c.Get(ctx, client.ObjectKey{Name: className}, &got); err != nil {
			return "", "", nil, err
		}
		v, ok := got.Status.Vendors["aws"]
		if !ok {
			return "", "", nil, fmt.Errorf("no vendor status yet")
		}
		for _, cond := range v.Conditions {
			if cond.Type == v1alpha1.VendorConditionQuotaAvailable {
				condStatus, reason = string(cond.Status), cond.Reason
			}
		}
		for _, q := range v.Quota {
			entries = append(entries, entry{q.Region, q.CapacityType, q.Reason})
		}
		return condStatus, reason, entries, nil
	}
	expect := func(what, wantStatus, wantReason string, want ...entry) {
		t.Helper()
		var lastErr error
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			condStatus, reason, entries, err := state()
			if err == nil && condStatus == wantStatus && reason == wantReason && slices.Equal(entries, want) {
				return
			}
			lastErr = fmt.Errorf("status=%q reason=%q entries=%v err=%v", condStatus, reason, entries, err)
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("%s: timed out; last state %v", what, lastErr)
	}

	refresh("us-east-1", 100, 0)
	expect("plenty of quota", "True", quotastatus.ReasonAvailable)

	refresh("us-east-1", 10, 10)
	expect("one region exhausted", "False", quotastatus.ReasonExhausted,
		entry{"us-east-1", "on-demand", quotastatus.ReasonExhausted}, entry{"us-east-1", "spot", quotastatus.ReasonExhausted})

	refresh("us-east-1", 0, 0)
	expect("limit zero", "False", quotastatus.ReasonZero,
		entry{"us-east-1", "on-demand", quotastatus.ReasonZero}, entry{"us-east-1", "spot", quotastatus.ReasonZero})

	refresh("us-east-1", 100, 3)
	expect("freed", "True", quotastatus.ReasonAvailable)

	drv.set("us-east-2", 5, 5)
	refresh("us-east-1", 7, 7)
	expect("both exhausted", "False", quotastatus.ReasonExhausted,
		entry{"us-east-1", "on-demand", quotastatus.ReasonExhausted}, entry{"us-east-1", "spot", quotastatus.ReasonExhausted},
		entry{"us-east-2", "on-demand", quotastatus.ReasonExhausted}, entry{"us-east-2", "spot", quotastatus.ReasonExhausted})

	// The controller publishes events after the apply the status wait observed,
	// so wait for them instead of reading once.
	var got []string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if got = rec.reasons(); eventsOK(got) {
			break
		}
	}
	checkEvents(t, got)
	checkOwnership(ctx, t, c, className)
}

func checkEvents(t *testing.T, got []string) {
	t.Helper()
	if !eventsOK(got) {
		t.Errorf("events: want at least 4 exhausted and exactly 2 zero: %v", got)
	}
}

// eventsOK: us-east-1 (x2 capacity types) blocked, zero, then both regions
// again. A stale cache read may repeat an exhausted event, so that count is a
// minimum.
func eventsOK(got []string) bool {
	var exhausted, zero int
	for _, r := range got {
		switch {
		case strings.HasPrefix(r, quotastatus.ReasonExhausted+":"):
			exhausted++
		case strings.HasPrefix(r, quotastatus.ReasonZero+":"):
			zero++
		}
	}
	return exhausted >= 4 && zero == 2
}

// The apply owns only its fields: the quota manager is on the status.
func checkOwnership(ctx context.Context, t *testing.T, c client.Client, name string) {
	t.Helper()
	var final v1alpha1.SCNodeClass
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &final); err != nil {
		t.Fatal(err)
	}
	for _, mf := range final.ManagedFields {
		if mf.Manager == quotastatus.FieldManager && mf.Subresource == "status" {
			return
		}
	}
	t.Errorf("no managed fields for %s: %+v", quotastatus.FieldManager, final.ManagedFields)
}

// TestQuotaStatusOfTwoVendors: the status of every vendor of a class goes in
// one apply, so both vendors keep their condition and quota entries; a quiet
// reconcile writes nothing; a vendor that leaves the class loses its entries
// and the other keeps its own.
func TestQuotaStatusOfTwoVendors(t *testing.T) {
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

	const className = "qs2"
	awsRegions, hcloudRegions := []string{"us-east-1"}, []string{"fsn1"}
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: className},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors: v1alpha1.VendorBlocks{
				AWS: &aws.Settings{Regions: awsRegions}, HCloud: &hcloud.Settings{Regions: hcloudRegions},
			},
		},
	}
	if err := c.Create(t.Context(), nc); err != nil {
		t.Fatal(err)
	}

	cat := catalog.New(catalog.Options{Source: catalogtest.Source(t), Facts: catalogtest.Facts(t, "aws", "hcloud")})
	cat.SetClasses([]catalog.Class{catalog.NewClass(className, catalog.Scope{"aws": awsRegions, "hcloud": hcloudRegions})})
	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Both vendors are out of quota in their only region.
	awsDrv := &driver{vendor: "aws", buckets: map[string]vendors.Bucket{}}
	awsDrv.set("us-east-1", 10, 10)
	hcloudDrv := &driver{vendor: "hcloud", buckets: map[string]vendors.Bucket{}}
	hcloudDrv.set("fsn1", 0, 0)
	tracker := quota.New(quota.Options{
		Registry: vendors.NewRegistryFrom(awsDrv, hcloudDrv),
		Scope:    func() map[string][]string { return map[string][]string{"aws": awsRegions, "hcloud": hcloudRegions} },
	})
	if err := tracker.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := quotastatus.New(quotastatus.Options{
		Client: mgr.GetClient(), Catalog: cat, Tracker: tracker, Interval: 200 * time.Millisecond,
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
	blocked := func(got v1alpha1.SCNodeClass, vendor string) bool {
		vs := got.Status.Vendors[vendor]
		return len(vs.Quota) > 0 && slices.ContainsFunc(vs.Conditions, func(c status.Condition) bool {
			return c.Type == v1alpha1.VendorConditionQuotaAvailable && c.Status == metav1.ConditionFalse
		})
	}
	waitFor := func(what string, ok func(v1alpha1.SCNodeClass) bool) v1alpha1.SCNodeClass {
		t.Helper()
		var got v1alpha1.SCNodeClass
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if got = get(); ok(got) {
				return got
			}
		}
		t.Fatalf("%s: timed out; vendors %+v", what, got.Status.Vendors)
		return got
	}

	got := waitFor("both vendors blocked", func(got v1alpha1.SCNodeClass) bool {
		return blocked(got, "aws") && blocked(got, "hcloud")
	})
	time.Sleep(time.Second)
	if again := get(); again.ResourceVersion != got.ResourceVersion {
		t.Errorf("the object was written again while nothing changed: %s -> %s", got.ResourceVersion, again.ResourceVersion)
	}

	cur := get()
	cur.Spec.Vendors.HCloud = nil
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	cat.SetClasses([]catalog.Class{catalog.NewClass(className, catalog.Scope{"aws": awsRegions})})
	waitFor("hcloud removed", func(got v1alpha1.SCNodeClass) bool {
		hs := got.Status.Vendors["hcloud"]
		return len(hs.Quota) == 0 && len(hs.Conditions) == 0 && blocked(got, "aws")
	})
}
