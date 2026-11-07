/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package resources_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/resources/resourcestest"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const (
	cluster = "my-cluster"
	region1 = "us-east-1"
	region2 = "eu-west-1"
)

var start = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

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
		out = append(out, e.Reason)
	}
	return out
}

func deployment() *corev1.ObjectReference {
	return &corev1.ObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "scarpenter", Namespace: "scarpenter-system"}
}

type env struct {
	t      *testing.T
	clock  *clocktesting.FakeClock
	drv    *resourcestest.Driver
	un     *unavailable.Cache
	events *recorder
	engine *resources.Engine
	kube   client.Client
}

type envOpts struct {
	classes []*v1alpha1.SCNodeClass
	dryRun  bool
	regions []string
	// allRegions are the regions of the driver, swept now and then; nil: none.
	allRegions []string
	zones      map[string][]string
	registerer prometheus.Registerer
}

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, c := range o.classes {
		b = b.WithObjects(c)
	}
	kube := b.Build()
	clk := clocktesting.NewFakeClock(start)
	drv := resourcestest.New("aws", clk)
	un := unavailable.New(clk, unavailable.TTLs{})
	rec := &recorder{}
	if o.regions == nil {
		o.regions = []string{region1, region2}
	}
	e := &env{t: t, clock: clk, drv: drv, un: un, events: rec, kube: kube}
	e.engine = resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(drv), Reader: kube, Clock: clk, ClusterName: cluster,
		Recorder: rec, Unavailable: un, DryRun: o.dryRun, DeploymentRef: deployment,
		Regions: func(string) []string { return o.regions }, FullSweepEvery: 100, Registerer: o.registerer,
		AllRegions: func(string) []string { return o.allRegions },
		Zones:      func(_, region string) []string { return o.zones[region] },
	})
	return e
}

func class(name string, res *aws.Resources) *v1alpha1.SCNodeClass {
	return &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{AWS: &aws.Settings{Resources: res}}},
	}
}

func (e *env) require(nc *v1alpha1.SCNodeClass, region string, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	e.t.Helper()
	return e.engine.Provider(nc, "aws", region, "").Require(context.Background(), req)
}

func subnets() vendors.ResourceRequest { return vendors.ResourceRequest{Kind: resourcestest.Subnets} }

func TestUseByIDsNameAndSelector(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-a", Name: "sc-us-east-1", Tags: map[string]string{"use": "true"}},
		{Kind: resourcestest.Subnets, Region: region2, ID: "subnet-b", Name: "sc-eu-west-1", Tags: map[string]string{"use": "true"}},
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-other", Name: "other", Tags: map[string]string{"use": "false"}},
	}
	for name, use := range map[string]*common.ResourceUse{
		"ids":      {IDs: map[string][]string{region1: {"subnet-a"}, region2: {"subnet-b"}}},
		"name":     {Name: "sc-{region}"},
		"selector": {Selector: &common.TagSelector{Tags: map[string]string{"use": "true"}}},
	} {
		t.Run(name, func(t *testing.T) {
			nc := class(name, &aws.Resources{Subnets: &common.Resource{Use: use}})
			for region, want := range map[string]string{region1: "subnet-a", region2: "subnet-b"} {
				refs, err := e.require(nc, region, subnets())
				if err != nil || len(refs) != 1 || refs[0].ID != want || refs[0].Managed {
					t.Errorf("%s: refs %+v, err %v; want the unmanaged %s", region, refs, err, want)
				}
			}
		})
	}
	if calls := e.drv.CallLog(); len(calls) != 0 {
		t.Errorf("use resources must never be created or deleted, calls: %v", calls)
	}
}

func TestMissingUseMakesTheRegionUnavailable(t *testing.T) {
	nc := class("c", &aws.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{
		Selector: &common.TagSelector{Tags: map[string]string{"use": "true"}}}}})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-a", Tags: map[string]string{"use": "true"}},
	}
	before := e.un.Sequence()
	_, err := e.require(nc, region2, subnets())
	if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Fatalf("a missing use resource: %v, want Unsupported/ResourceMissing", err)
	}
	if !e.engine.UnavailableFor("c", "aws", "m7i.large", region2, "z", "on-demand") {
		t.Error("the offerings of the region with the missing resource must be unavailable")
	}
	if e.engine.UnavailableFor("c", "aws", "m7i.large", region1, "z", "on-demand") {
		t.Error("another region is unaffected")
	}
	if e.un.Sequence() == before {
		t.Error("the catalog's cache was not invalidated")
	}
	if len(e.drv.CallLog()) != 0 {
		t.Errorf("nothing may be created for a missing use resource: %v", e.drv.CallLog())
	}
	// An ID list without an entry for the region must not mean "the default".
	nc2 := class("c2", &aws.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{
		IDs: map[string][]string{region1: {"subnet-a"}}}}})
	e.drv.Existing = append(e.drv.Existing, resourcestest.Existing{Kind: resourcestest.Subnets, Region: region2, ID: "dflt", Default: true})
	if _, err := e.require(nc2, region2, subnets()); vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Errorf("ids without the region: %v, want ResourceMissing", err)
	}
}

func TestForbidNeedsTheVendorDefault(t *testing.T) {
	nc := class("c", &aws.Resources{Subnets: &common.Resource{Forbid: true}})
	e := newEnv(t, envOpts{})
	e.drv.Existing = []resourcestest.Existing{{Kind: resourcestest.Subnets, Region: region1, ID: "default-subnet", Default: true}}
	refs, err := e.require(nc, region1, subnets())
	if err != nil || len(refs) != 1 || refs[0].ID != "default-subnet" {
		t.Fatalf("forbid with a vendor default: %+v, %v", refs, err)
	}
	_, err = e.require(nc, region2, subnets())
	if vendors.CodeOf(err) != vendors.CodeResourceForbidden || !vendors.IsClass(err, vendors.Unsupported) {
		t.Fatalf("forbid without a default: %v, want Unsupported/ResourceForbidden", err)
	}
	if len(e.drv.CallLog()) != 0 {
		t.Errorf("forbid must never create: %v", e.drv.CallLog())
	}
}

func TestNeitherUsesTheVendorDefaultAndOtherwiseCreates(t *testing.T) {
	nc := class("c", nil)
	nc.Spec.Tags = map[string]string{"owner": "example"}
	e := newEnv(t, envOpts{})
	e.drv.Existing = []resourcestest.Existing{{Kind: resourcestest.Subnets, Region: region1, ID: "default-subnet", Default: true}}

	refs, err := e.require(nc, region1, subnets())
	if err != nil || refs[0].ID != "default-subnet" || refs[0].Managed {
		t.Fatalf("with a default: %+v, %v; want it used as found", refs, err)
	}
	refs, err = e.require(nc, region2, subnets())
	if err != nil || len(refs) != 1 || !refs[0].Managed {
		t.Fatalf("without a default: %+v, %v; want a created resource", refs, err)
	}
	if want := "scarpenter-my-cluster-subnets-eu-west-1"; refs[0].Name != want {
		t.Errorf("name %q, want the deterministic %q", refs[0].Name, want)
	}
	listed, _ := e.drv.List(t.Context(), nil, map[vendors.TagKey]string{vendors.TagCluster: cluster})
	if len(listed) != 1 {
		t.Fatalf("listed %d managed resources, want 1", len(listed))
	}
	tags := listed[0].Tags
	if tags[vendors.TagCluster] != cluster || tags[vendors.TagNodeClass] != "c" || tags[vendors.TagManagedBy] != vendors.ManagedByValue ||
		tags[vendors.TagResource] != "subnets" {
		t.Errorf("ownership marker %v", tags)
	}
	if !slices.Contains(e.events.reasons(), resources.EventCreated) {
		t.Errorf("no Event for the creation: %v", e.events.reasons())
	}
}

func TestCreateIsSerialisedPerVendorKindAndRegion(t *testing.T) {
	nc := class("c", &aws.Resources{Subnets: &common.Resource{Create: &common.ResourceCreate{}}})
	e := newEnv(t, envOpts{})
	var inHook sync.WaitGroup
	release := make(chan struct{})
	inHook.Add(1)
	var once sync.Once
	e.drv.EnsureHook = func() {
		once.Do(func() { inHook.Done() })
		<-release
	}
	const callers = 12
	var wg sync.WaitGroup
	ids := make([]string, callers)
	for i := range callers {
		wg.Go(func() {
			refs, err := e.require(nc, region1, subnets())
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = refs[0].ID
		})
	}
	inHook.Wait() // one caller is inside Ensure, the others wait for its result
	close(release)
	wg.Wait()
	creates := 0
	for _, c := range e.drv.CallLog() {
		if strings.HasPrefix(c, "ensure:created") {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("%d creations for %d concurrent launches, want exactly 1: %v", creates, callers, e.drv.CallLog())
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Errorf("launches got different resources: %v", ids)
			break
		}
	}
}

func TestAdoptionAfterRestartReusesTheResource(t *testing.T) {
	nc := class("c", nil)
	e := newEnv(t, envOpts{})
	first, err := e.require(nc, region1, subnets())
	if err != nil {
		t.Fatal(err)
	}
	// A restarted controller has a fresh engine over the same account.
	e2 := newEnv(t, envOpts{})
	e2.drv = e.drv
	e2.engine = resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(e.drv), Clock: e.clock, ClusterName: cluster, Recorder: e2.events,
		DeploymentRef: deployment,
	})
	second, err := e2.require(nc, region1, subnets())
	if err != nil || second[0].ID != first[0].ID {
		t.Fatalf("after a restart: %+v, %v; want the same resource %s", second, err, first[0].ID)
	}
	calls := e.drv.CallLog()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "ensure:created") || !strings.HasPrefix(calls[1], "ensure:found") {
		t.Errorf("calls %v, want one creation then a find", calls)
	}
	if !slices.Contains(e2.events.reasons(), resources.EventAdopted) {
		t.Errorf("no adoption Event: %v", e2.events.reasons())
	}
}

func TestPerRunResourcesAreNamedByKey(t *testing.T) {
	nc := class("c", nil)
	e := newEnv(t, envOpts{})
	req := func(key string) vendors.ResourceRequest {
		return vendors.ResourceRequest{Kind: resourcestest.PlacementGroups, Key: key}
	}
	a1, _ := e.require(nc, region1, req("colocated/run-1"))
	a2, _ := e.require(nc, region1, req("colocated/run-1"))
	b, _ := e.require(nc, region1, req("colocated/run-2"))
	if a1[0].ID != a2[0].ID {
		t.Error("the same run key must share one group")
	}
	if a1[0].ID == b[0].ID || a1[0].Name == b[0].Name {
		t.Errorf("different run keys must get different groups: %s %s", a1[0].Name, b[0].Name)
	}
	if !strings.HasPrefix(a1[0].Name, "scarpenter-my-cluster-placement-groups-us-east-1-") {
		t.Errorf("name %q", a1[0].Name)
	}
	// A per-run kind referenced with use is never created.
	use := &vendors.UseRef{NamePattern: "my-pg"}
	e.drv.Existing = []resourcestest.Existing{{Kind: resourcestest.PlacementGroups, Region: region1, ID: "pg-mine", Name: "my-pg"}}
	refs, err := e.require(nc, region1, vendors.ResourceRequest{Kind: resourcestest.PlacementGroups, Use: use, Key: "x"})
	if err != nil || refs[0].ID != "pg-mine" || refs[0].Managed {
		t.Errorf("use override: %+v, %v", refs, err)
	}
}

func TestUnknownKindAndDriverErrors(t *testing.T) {
	nc := class("c", nil)
	e := newEnv(t, envOpts{})
	if _, err := e.require(nc, region1, vendors.ResourceRequest{Kind: "nope"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("unknown kind: %v, want Fatal", err)
	}
	if _, err := e.engine.Provider(nc, "gcp", region1, "").Require(t.Context(), subnets()); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a vendor without the capability: %v, want Fatal", err)
	}
}

func TestResourceNameIsDeterministicAndSafe(t *testing.T) {
	a := resources.ResourceName("My_Cluster.1", "placementGroups", "us-east-1", "", "x/y")
	b := resources.ResourceName("My_Cluster.1", "placementGroups", "us-east-1", "", "x/y")
	if a != b {
		t.Error("not deterministic")
	}
	if !strings.HasPrefix(a, resources.NamePrefix("My_Cluster.1")) || strings.ContainsAny(a, "_./ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Errorf("unsafe name %q", a)
	}
	if got := resources.ResourceName("c", "subnets", "r", "", ""); got != "scarpenter-c-subnets-r" {
		t.Errorf("shared kind name %q: no hash expected", got)
	}
}

func TestDecideLifecycles(t *testing.T) {
	grace := 2 * time.Minute
	now := start
	lc := func(p common.LifecyclePolicy, ttl time.Duration) resources.Lifecycle {
		return resources.Lifecycle{Policy: p, TTL: ttl}
	}
	tests := []struct {
		name                           string
		lc                             resources.Lifecycle
		classExists, preCreated, inUse bool
		idle                           time.Duration
		wantDelete                     bool
		wantReason                     string
	}{
		{"perUse in use", lc("perUse", 0), true, false, true, time.Hour, false, ""},
		{"perUse within the grace", lc("perUse", 0), true, false, false, time.Minute, false, ""},
		{"perUse after the grace", lc("perUse", 0), true, false, false, grace, true, resources.ReasonPerUse},
		{"idleTTL before", lc("idleTTL", time.Hour), true, false, false, 59 * time.Minute, false, ""},
		{"idleTTL after", lc("idleTTL", time.Hour), true, false, false, time.Hour, true, resources.ReasonIdleTTL},
		{"idleTTL in use", lc("idleTTL", time.Hour), true, false, true, 48 * time.Hour, false, ""},
		{"retain with its class", lc("retain", 0), true, false, false, 1000 * time.Hour, false, ""},
		{"retain without its class", lc("retain", 0), false, false, false, grace, true, resources.ReasonRetained},
		{"retain without its class but in use", lc("retain", 0), false, false, true, time.Hour, false, ""},
		{"preCreate keeps idle resources", lc("idleTTL", time.Hour), true, true, false, 48 * time.Hour, false, ""},
		{"preCreate keeps perUse ones too", lc("perUse", 0), true, true, false, time.Hour, false, ""},
		{"class gone, preCreate no longer applies", lc("idleTTL", time.Hour), false, true, false, 2 * time.Hour, true, resources.ReasonIdleTTL},
		{"class gone, perUse", lc("perUse", 0), false, false, false, grace, true, resources.ReasonOrphan},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resources.Decide(tc.lc, grace, tc.classExists, tc.preCreated, tc.inUse, now.Add(-tc.idle), now)
			if got.Delete != tc.wantDelete || got.Reason != tc.wantReason {
				t.Errorf("Decide = %+v, want delete=%v reason=%q", got, tc.wantDelete, tc.wantReason)
			}
		})
	}
}

func (e *env) sweepOnce() {
	e.t.Helper()
	if err := e.engine.Sweep(context.Background(), sets.New[string]()); err != nil {
		e.t.Fatal(err)
	}
}

// managedTags is the ownership marker of a resource of the class.
func managedTags(class, kind string) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{
		vendors.TagCluster: cluster, vendors.TagNodeClass: class, vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: kind,
	}
}

func TestSweepAppliesTheLifecycles(t *testing.T) {
	nc := class("c", &aws.Resources{
		Subnets: &common.Resource{Create: &common.ResourceCreate{
			Lifecycle: &common.Lifecycle{Policy: common.LifecycleIdleTTL, TTL: &metav1.Duration{Duration: time.Hour}}}},
		SecurityGroups: &common.Resource{Create: &common.ResourceCreate{
			Lifecycle: &common.Lifecycle{Policy: common.LifecycleRetain}}},
	})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	name := func(kind vendors.ResourceKind, suffix string) string {
		return resources.NamePrefix(cluster) + string(kind) + "-" + suffix
	}
	subnet := e.drv.Seed(resourcestest.Subnets, region1, name("subnets", "idle"), managedTags("c", "subnets"), start)
	busy := e.drv.Seed(resourcestest.Subnets, region2, name("subnets", "busy"), managedTags("c", "subnets"), start)
	sg := e.drv.Seed(resourcestest.SecurityGroups, region1, name("securityGroups", "kept"), managedTags("c", "securityGroups"), start)
	pg := e.drv.Seed(resourcestest.PlacementGroups, region1, name("placementGroups", "run"), managedTags("c", "placementGroups"), start)
	e.drv.SetUsers(busy.ID, 1)
	e.clock.SetTime(start) // SetUsers stamped the time

	sweep := func() {
		t.Helper()
		if err := e.engine.Sweep(t.Context(), sets.New[string]()); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing is old enough at the start.
	sweep()
	if got := e.drv.Managed(); len(got) != 4 {
		t.Fatalf("deleted %v at t=0", got)
	}
	// After the perUse grace only the unused placement group goes.
	e.clock.Step(3 * time.Minute)
	sweep()
	if got := e.drv.Managed(); slices.Contains(got, pg.ID) || len(got) != 3 {
		t.Fatalf("after the grace: %v, want only the placement group gone", got)
	}
	// After the TTL the idle subnet goes; the busy one, and the retained group stay.
	e.clock.Step(2 * time.Hour)
	sweep()
	got := e.drv.Managed()
	if slices.Contains(got, subnet.ID) || !slices.Contains(got, busy.ID) || !slices.Contains(got, sg.ID) {
		t.Fatalf("after the TTL: %v", got)
	}
	// The class goes away: its retained resource follows, the busy one stays.
	if err := e.kube.Delete(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	e.clock.Step(3 * time.Minute)
	sweep()
	got = e.drv.Managed()
	if slices.Contains(got, sg.ID) || !slices.Contains(got, busy.ID) {
		t.Fatalf("after the class was deleted: %v", got)
	}
	if !slices.Contains(e.events.reasons(), resources.EventDeleted) {
		t.Errorf("no deletion Event: %v", e.events.reasons())
	}
}

func TestSweepKeepsPreCreatedResourcesWhileListed(t *testing.T) {
	nc := class("c", &aws.Resources{Subnets: &common.Resource{Create: &common.ResourceCreate{
		Timing: &common.Timing{Mode: common.TimingPreCreate, Regions: []string{region1}}}}})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	listed := e.drv.Seed(resourcestest.Subnets, region1, resources.NamePrefix(cluster)+"subnets-a", managedTags("c", "subnets"), start)
	other := e.drv.Seed(resourcestest.Subnets, region2, resources.NamePrefix(cluster)+"subnets-b", managedTags("c", "subnets"), start)
	e.sweepOnce() // the process sees the resources now: idle time counts from here
	e.clock.Step(48 * time.Hour)
	if err := e.engine.Sweep(t.Context(), sets.New[string]()); err != nil {
		t.Fatal(err)
	}
	got := e.drv.Managed()
	if !slices.Contains(got, listed.ID) || slices.Contains(got, other.ID) {
		t.Errorf("after 48h: %v; want the preCreate region's resource kept and the other idle one deleted", got)
	}
}

func TestSweepNeverTouchesWhatIsNotOurs(t *testing.T) {
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{class("c", nil)}})
	ours := managedTags("c", "subnets")
	other := managedTags("c", "subnets")
	other[vendors.TagCluster] = "another-cluster"
	wrongName := e.drv.Seed(resourcestest.Subnets, region1, "unrelated-name", ours, start)
	otherCluster := e.drv.Seed(resourcestest.Subnets, region1, resources.NamePrefix(cluster)+"subnets-x", other, start)
	unmanaged := vendors.ResourceRef{Kind: resourcestest.Subnets, Region: region1, ID: "mine", Name: resources.NamePrefix(cluster) + "mine"}
	e.drv.Existing = []resourcestest.Existing{{Kind: resourcestest.Subnets, Region: region1, ID: unmanaged.ID, Default: true}}
	e.sweepOnce() // the process sees the resources now: idle time counts from here
	e.clock.Step(1000 * time.Hour)
	if err := e.engine.Sweep(t.Context(), sets.New[string]()); err != nil {
		t.Fatal(err)
	}
	got := e.drv.Managed()
	if !slices.Contains(got, wrongName.ID) || !slices.Contains(got, otherCluster.ID) {
		t.Errorf("deleted something that lacks the deterministic name or the marker: %v", got)
	}
	for _, c := range e.drv.CallLog() {
		if strings.Contains(c, unmanaged.ID) {
			t.Errorf("touched a use resource: %s", c)
		}
	}
}

func TestSweepDeletesInDependencyOrderAndRetriesFailures(t *testing.T) {
	nc := class("c", &aws.Resources{SecurityGroups: &common.Resource{Create: &common.ResourceCreate{
		Lifecycle: &common.Lifecycle{Policy: common.LifecyclePerUse}}}})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	prefix := resources.NamePrefix(cluster)
	// Seeded in the opposite order of the dependencies.
	subnet := e.drv.Seed(resourcestest.Subnets, region1, prefix+"subnets-x", managedTags("c", "subnets"), start)
	sg := e.drv.Seed(resourcestest.SecurityGroups, region1, prefix+"securityGroups-x", managedTags("c", "securityGroups"), start)
	pg := e.drv.Seed(resourcestest.PlacementGroups, region1, prefix+"placementGroups-x", managedTags("c", "placementGroups"), start)
	e.sweepOnce() // the process sees the resources now: idle time counts from here
	e.clock.Step(48 * time.Hour)

	// The placement group (order 20) cannot be deleted yet: what depends on it
	// in the region must wait.
	e.drv.DeleteErr[pg.ID] = vendors.NewError(vendors.Retryable, "InUse", errors.New("the group is in use"))
	if err := e.engine.Sweep(t.Context(), sets.New[string]()); err == nil {
		t.Fatal("a failed deletion must be reported")
	}
	if got := e.drv.Managed(); len(got) != 3 {
		t.Fatalf("deleted %v although the first dependency failed", got)
	}
	// Backoff: an immediate retry does not call the vendor again.
	before := len(e.drv.CallLog())
	if err := e.engine.Sweep(t.Context(), sets.New[string]()); err != nil {
		t.Fatalf("a deletion in backoff must not be an error: %v", err)
	}
	if len(e.drv.CallLog()) != before {
		t.Error("a failing deletion was retried without backoff")
	}
	// The failure is visible in the status.
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	foundMessage := false
	for _, r := range rep.Resources {
		if r.ID == pg.ID && strings.Contains(r.Message, "in use") {
			foundMessage = true
		}
	}
	if !foundMessage {
		t.Errorf("the failed deletion is not in the status: %+v", rep.Resources)
	}
	// The vendor lets go; after the backoff the whole chain is deleted in order.
	delete(e.drv.DeleteErr, pg.ID)
	e.clock.Step(time.Hour)
	if err := e.engine.Sweep(t.Context(), sets.New[string]()); err != nil {
		t.Fatal(err)
	}
	if got := e.drv.Managed(); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
	var order []string
	for _, c := range e.drv.CallLog() {
		if id, ok := strings.CutPrefix(c, "delete:"); ok {
			order = append(order, id)
		}
	}
	if want := []string{pg.ID, sg.ID, subnet.ID}; !slices.Equal(order, want) {
		t.Errorf("deletion order %v, want %v (placement group, security group, subnet)", order, want)
	}
}

func TestSweepDryRunDeletesNothing(t *testing.T) {
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{class("c", nil)}, dryRun: true})
	e.drv.Seed(resourcestest.Subnets, region1, resources.NamePrefix(cluster)+"subnets-x", managedTags("c", "subnets"), start)
	e.sweepOnce() // the process sees the resources now: idle time counts from here
	e.clock.Step(48 * time.Hour)
	if err := e.engine.Sweep(t.Context(), sets.New[string]()); err != nil {
		t.Fatal(err)
	}
	if len(e.drv.Managed()) != 1 {
		t.Error("dry-run deleted a resource")
	}
}

func TestCheckReportsUseForbidAndPreCreate(t *testing.T) {
	nc := class("c", &aws.Resources{
		Subnets: &common.Resource{Use: &common.ResourceUse{Selector: &common.TagSelector{Tags: map[string]string{"use": "true"}}}},
		SecurityGroups: &common.Resource{Create: &common.ResourceCreate{
			Timing: &common.Timing{Mode: common.TimingPreCreate, Regions: []string{region1}}}},
		PlacementGroups: &common.Resource{Forbid: true},
	})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	e.drv.Existing = []resourcestest.Existing{
		{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-a", Tags: map[string]string{"use": "true"}},
	}
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1, region2})
	if st := rep.Regions[region1]; st.Ready || !strings.Contains(st.Reason, "placementGroups") {
		t.Errorf("%s: %+v; the forbidden kind without a default must make it not ready", region1, st)
	}
	if st := rep.Regions[region2]; st.Ready || !strings.Contains(st.Reason, "subnets") {
		t.Errorf("%s: %+v; the missing use resource must make it not ready", region2, st)
	}
	if !e.engine.UnavailableFor("c", "aws", "x", region1, "z", "spot") || !e.engine.UnavailableFor("c", "aws", "x", region2, "z", "spot") {
		t.Error("the engine must gate the offerings of both regions")
	}
	kinds := map[string]bool{}
	for _, r := range rep.Resources {
		kinds[r.Kind+"/"+r.Region+"/"+r.ID] = r.Managed
	}
	if managed, ok := kinds["subnets/us-east-1/subnet-a"]; !ok || managed {
		t.Errorf("the used subnet is not reported as unmanaged: %v", kinds)
	}
	created := false
	for key, managed := range kinds {
		if strings.HasPrefix(key, "securityGroups/us-east-1/") && managed {
			created = true
		}
	}
	if !created {
		t.Errorf("the preCreate security group was not created in %s: %v", region1, kinds)
	}

	// The missing resource appears: the region becomes ready.
	e.drv.Existing = append(e.drv.Existing,
		resourcestest.Existing{Kind: resourcestest.Subnets, Region: region2, ID: "subnet-b", Tags: map[string]string{"use": "true"}},
		resourcestest.Existing{Kind: resourcestest.PlacementGroups, Region: region1, ID: "pg-default", Default: true},
		resourcestest.Existing{Kind: resourcestest.PlacementGroups, Region: region2, ID: "pg-default2", Default: true})
	e.clock.Step(2 * time.Minute) // past the cache of recent answers
	rep = e.engine.Check(t.Context(), nc, "aws", []string{region1, region2})
	if bad, why := rep.NotReady(); len(bad) != 0 {
		t.Errorf("still not ready: %v %v", bad, why)
	}
	if e.engine.UnavailableFor("c", "aws", "x", region2, "z", "spot") {
		t.Error("the region is still gated after its resource appeared")
	}
}

func TestSweepFindsResourcesOfRegionsNoNodeClassUsesAnyMore(t *testing.T) {
	nc := class("c", &aws.Resources{SecurityGroups: &common.Resource{Create: &common.ResourceCreate{
		Lifecycle: &common.Lifecycle{Policy: common.LifecycleRetain}}}})
	// Only region1 is in use; the account also has region2.
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}, regions: []string{region1}, allRegions: []string{region1, region2}})
	leaked := e.drv.Seed(resourcestest.SecurityGroups, region2, resources.NamePrefix(cluster)+"securityGroups-x",
		managedTags("c", "securityGroups"), start)
	e.sweepOnce()
	e.clock.Step(time.Hour)
	if err := e.kube.Delete(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	e.sweepOnce() // the NodeClass vanished: this sweep lists every region of the driver
	if slices.Contains(e.drv.Managed(), leaked.ID) {
		t.Errorf("the retained resource of a region no NodeClass uses any more leaked after its NodeClass was deleted: %v", e.drv.Managed())
	}

	// Without the driver's regions the engine cannot see it: the regions in use are all it knows.
	e2 := newEnv(t, envOpts{regions: []string{region1}})
	hidden := e2.drv.Seed(resourcestest.SecurityGroups, region2, resources.NamePrefix(cluster)+"securityGroups-y",
		managedTags("gone", "securityGroups"), start)
	e2.clock.Step(time.Hour)
	e2.sweepOnce()
	if !slices.Contains(e2.drv.Managed(), hidden.ID) {
		t.Error("a region outside the sweep scope was swept without a full sweep")
	}
}

func TestPeriodicFullSweepCoversTheDriversRegions(t *testing.T) {
	e := newEnv(t, envOpts{regions: []string{region1}, allRegions: []string{region1, region2}})
	e.engine = resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(e.drv), Reader: e.kube, Clock: e.clock, ClusterName: cluster,
		Regions: func(string) []string { return []string{region1} }, FullSweepEvery: 3,
		AllRegions: func(string) []string { return []string{region1, region2} },
	})
	e.sweepOnce() // sweep 1 is a full one
	orphan := e.drv.Seed(resourcestest.SecurityGroups, region2, resources.NamePrefix(cluster)+"securityGroups-z",
		managedTags("gone", "securityGroups"), start)
	e.clock.Step(time.Hour)
	e.sweepOnce() // 2
	e.sweepOnce() // 3
	if !slices.Contains(e.drv.Managed(), orphan.ID) {
		t.Fatal("swept outside the scope before the full sweep")
	}
	e.sweepOnce() // 4: full again, sees the resource for the first time
	if !slices.Contains(e.drv.Managed(), orphan.ID) {
		t.Fatal("deleted on the first sighting")
	}
	e.clock.Step(time.Hour)
	e.sweepOnce() // 5: not a full sweep, but the region is watched now
	if slices.Contains(e.drv.Managed(), orphan.ID) {
		t.Errorf("the region where a full sweep found resources is not swept every time: %v", e.drv.Managed())
	}
}

func subnetClass(name string, res *common.Resource, regions ...string) *v1alpha1.SCNodeClass {
	nc := class(name, &aws.Resources{Subnets: res})
	nc.Spec.Vendors.AWS.Regions = regions
	return nc
}

func TestSharedResourceIsGovernedByEveryNodeClassThatMapsToIt(t *testing.T) {
	creator := subnetClass("creator", nil, region1)
	keeper := subnetClass("keeper", &common.Resource{Create: &common.ResourceCreate{
		Lifecycle: &common.Lifecycle{Policy: common.LifecycleRetain}}}, region1)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{creator, keeper}, regions: []string{region1}})
	subnet := e.drv.Seed(resourcestest.Subnets, region1, resources.NamePrefix(cluster)+"subnets-"+region1,
		managedTags("creator", "subnets"), start)
	e.sweepOnce()
	e.clock.Step(48 * time.Hour)
	e.sweepOnce()
	if !slices.Contains(e.drv.Managed(), subnet.ID) {
		t.Fatal("the subnet was deleted after the creator's default 24h although another NodeClass retains it")
	}
	// The status of the other NodeClass shows the shared resource too.
	rep := e.engine.Check(t.Context(), keeper, "aws", []string{region1})
	if !slices.ContainsFunc(rep.Resources, func(r v1alpha1.ResourceStatus) bool { return r.ID == subnet.ID }) {
		t.Errorf("the shared subnet is not in the status of the NodeClass that retains it: %+v", rep.Resources)
	}
	if err := e.kube.Delete(t.Context(), keeper); err != nil {
		t.Fatal(err)
	}
	e.sweepOnce()
	if slices.Contains(e.drv.Managed(), subnet.ID) {
		t.Error("the subnet stayed although no NodeClass retains it any more and its idle time is over")
	}
}

func TestPreCreateOfANonCreatorProtectsTheSharedResource(t *testing.T) {
	creator := subnetClass("creator", nil, region1)
	pre := subnetClass("pre", &common.Resource{Create: &common.ResourceCreate{
		Timing: &common.Timing{Mode: common.TimingPreCreate, Regions: []string{region1}}}}, region1)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{creator, pre}, regions: []string{region1}})
	subnet := e.drv.Seed(resourcestest.Subnets, region1, resources.NamePrefix(cluster)+"subnets-"+region1,
		managedTags("creator", "subnets"), start)
	e.sweepOnce()
	e.clock.Step(72 * time.Hour)
	e.sweepOnce()
	if !slices.Contains(e.drv.Managed(), subnet.ID) {
		t.Error("the subnet listed by another NodeClass's preCreate was deleted after the creator's TTL")
	}
}

func TestARestartRestartsTheIdleClock(t *testing.T) {
	nc := subnetClass("c", &common.Resource{Create: &common.ResourceCreate{
		Lifecycle: &common.Lifecycle{Policy: common.LifecycleIdleTTL, TTL: &metav1.Duration{Duration: time.Hour}}}}, region1)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}, regions: []string{region1}})
	// Created long ago; a driver that cannot say when it was last used reports that.
	old := e.drv.Seed(resourcestest.Subnets, region1, resources.NamePrefix(cluster)+"subnets-"+region1,
		managedTags("c", "subnets"), start.Add(-100*time.Hour))
	e.sweepOnce()
	if !slices.Contains(e.drv.Managed(), old.ID) {
		t.Fatal("the first sweep of a new process deleted a resource it never saw in use")
	}
	e.clock.Step(30 * time.Minute)
	e.sweepOnce()
	if !slices.Contains(e.drv.Managed(), old.ID) {
		t.Fatal("deleted before the TTL counted from the first sighting")
	}
	e.clock.Step(31 * time.Minute)
	e.sweepOnce()
	if slices.Contains(e.drv.Managed(), old.ID) {
		t.Error("not deleted after the TTL")
	}

	// A resource in use is used now, however long it has been there.
	used := e.drv.Seed(resourcestest.Subnets, region1, resources.NamePrefix(cluster)+"subnets-other",
		managedTags("c", "subnets"), start.Add(-100*time.Hour))
	e.drv.SetUsers(used.ID, 1)
	e.sweepOnce()
	e.drv.SetUsers(used.ID, 0)
	e.clock.Step(30 * time.Minute)
	e.sweepOnce()
	if !slices.Contains(e.drv.Managed(), used.ID) {
		t.Error("a resource that was in use half an hour ago was deleted by the one-hour idle TTL")
	}
}

func TestZonePatternsResolvePerZone(t *testing.T) {
	nc := class("c", &aws.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{Name: "sub-{zone}"}}})
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}, zones: map[string][]string{region1: {"za", "zb"}}})
	e.drv.Existing = []resourcestest.Existing{{Kind: resourcestest.Subnets, Region: region1, ID: "subnet-za", Name: "sub-za"}}

	// A launch carries its zone (through the provider or the request).
	refs, err := e.engine.Provider(nc, "aws", region1, "za").Require(t.Context(), subnets())
	if err != nil || refs[0].ID != "subnet-za" {
		t.Fatalf("zone za: %+v, %v", refs, err)
	}
	if _, err := e.engine.Provider(nc, "aws", region1, "").Require(t.Context(), vendors.ResourceRequest{Kind: resourcestest.Subnets, Zone: "za"}); err != nil {
		t.Errorf("the zone of the request: %v", err)
	}
	// A zone without a subnet fails its launch but does not gate the region.
	_, err = e.engine.Provider(nc, "aws", region1, "zb").Require(t.Context(), subnets())
	if vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Fatalf("zone zb: %v, want ResourceMissingInZone", err)
	}
	if e.engine.UnavailableFor("c", "aws", "x", region1, "za", "on-demand") {
		t.Error("a zone without its subnet made the whole region unavailable")
	}
	if !e.engine.UnavailableFor("c", "aws", "x", region1, "zb", "on-demand") {
		t.Error("the zone without its subnet is still offered")
	}
	// The status check expands the pattern for every zone: one match is enough.
	rep := e.engine.Check(t.Context(), nc, "aws", []string{region1})
	if st := rep.Regions[region1]; !st.Ready || st.Zones["zb"] == "" || len(st.Zones) != 1 {
		t.Errorf("the region has a subnet in one zone: want ready with only zb unavailable, got %+v", st)
	}
	if e.engine.UnavailableFor("c", "aws", "x", region1, "za", "on-demand") ||
		!e.engine.UnavailableFor("c", "aws", "x", region1, "zb", "on-demand") {
		t.Error("the status check must gate zb only")
	}
	// No zone matches: the region is unavailable and the reason names the zones.
	e.drv.Existing = nil
	e.clock.Step(time.Minute)
	rep = e.engine.Check(t.Context(), nc, "aws", []string{region1})
	if st := rep.Regions[region1]; st.Ready || !strings.Contains(st.Reason, "za") || !strings.Contains(st.Reason, "zb") {
		t.Errorf("no zone matches: %+v", st)
	}
	// Without the zones the pattern cannot be expanded: stated, not silently empty.
	e3 := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	if st := e3.engine.Check(t.Context(), nc, "aws", []string{region1}).Regions[region1]; st.Ready || !strings.Contains(st.Reason, "zones") {
		t.Errorf("unknown zones: %+v", st)
	}
}

func TestAMissingProfileReferenceDoesNotGateTheRegion(t *testing.T) {
	nc := class("c", nil)
	e := newEnv(t, envOpts{})
	_, err := e.require(nc, region1, vendors.ResourceRequest{
		Kind: resourcestest.PlacementGroups, Key: "x", Use: &vendors.UseRef{NamePattern: "absent"}})
	if vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Fatalf("%v", err)
	}
	if e.engine.UnavailableFor("c", "aws", "x", region1, "z", "on-demand") {
		t.Error("a group a profile names made the region unavailable for every pod")
	}
}

func TestNamesStayWithinVendorLimitsAndFollowTheParams(t *testing.T) {
	long := strings.Repeat("a-very-long-cluster-name-", 4)
	n := resources.ResourceName(long, "placementGroups", "northamerica-northeast1", "zone-b", "colocated/run")
	if len(n) > 63 || !strings.HasPrefix(n, resources.NamePrefix(long)) {
		t.Errorf("%q (%d) is too long or does not start with the prefix the sweep checks", n, len(n))
	}
	if resources.NamePrefix(long) == resources.NamePrefix(long+"x") {
		t.Error("two long cluster names share a prefix")
	}
	nc := class("c", nil)
	e := newEnv(t, envOpts{})
	get := func(params string) string {
		refs, err := e.require(nc, region1, vendors.ResourceRequest{Kind: resourcestest.PlacementGroups, Key: "p/run", Params: []byte(params)})
		if err != nil {
			t.Fatal(err)
		}
		return refs[0].ID
	}
	if get(`{"strategy":"cluster"}`) == get(`{"strategy":"spread"}`) {
		t.Error("a changed strategy adopted the group of the old one")
	}
	first := get(`{"strategy":"cluster"}`)
	if again := get(`{"strategy":"cluster"}`); again != first {
		t.Error("the same parameters did not find their group")
	}
}

func TestManagedGaugeFallsToZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	nc := class("c", nil)
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}, regions: []string{region1}, registerer: reg})
	e.drv.Seed(resourcestest.PlacementGroups, region1, resources.NamePrefix(cluster)+"placementGroups-x",
		managedTags("c", "placementGroups"), start)
	value := func() float64 {
		fams, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fams {
			if f.GetName() == "scarpenter_resources_managed" && len(f.GetMetric()) > 0 {
				return f.GetMetric()[0].GetGauge().GetValue()
			}
		}
		return -1
	}
	e.sweepOnce()
	if got := value(); got != 1 {
		t.Fatalf("gauge %v, want 1", got)
	}
	e.clock.Step(time.Hour)
	e.sweepOnce() // deleted
	e.sweepOnce()
	if got := value(); got != 0 {
		t.Errorf("gauge %v after the last resource was deleted, want 0", got)
	}
}

func TestTheLaunchZoneDoesNotChangeTheNameOfASharedResource(t *testing.T) {
	nc := class("c", nil)
	e := newEnv(t, envOpts{})
	refs, err := e.engine.Provider(nc, "aws", region1, "za").Require(t.Context(), subnets())
	if err != nil || refs[0].Name != "scarpenter-my-cluster-subnets-us-east-1" {
		t.Errorf("%+v, %v: a regional resource is shared by every zone", refs, err)
	}
}

// Regions in use follow the NodeClasses, as in the controller: nothing is in
// use after the last NodeClass is deleted.
func TestResourcesOfTheLastDeletedNodeClassAreCleanedUp(t *testing.T) {
	nc := class("only", nil)
	nc.Spec.Vendors.AWS.Regions = []string{region1}
	e := newEnv(t, envOpts{classes: []*v1alpha1.SCNodeClass{nc}})
	inUse := func(string) []string {
		var list v1alpha1.SCNodeClassList
		if err := e.kube.List(context.Background(), &list); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range list.Items {
			regions, _ := c.Spec.Vendors.Regions("aws")
			out = append(out, regions...)
		}
		return out
	}
	e.engine = resources.New(resources.Options{
		Registry: vendors.NewRegistryFrom(e.drv), Reader: e.kube, Clock: e.clock, ClusterName: cluster,
		Regions: inUse, FullSweepEvery: 100, AllRegions: func(string) []string { return []string{region1, region2} },
	})
	subnet := e.drv.Seed(resourcestest.Subnets, region1, resources.NamePrefix(cluster)+"subnets-"+region1,
		managedTags("only", "subnets"), start)
	e.sweepOnce()
	if err := e.kube.Delete(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	e.sweepOnce() // the NodeClass vanished: the full sweep finds the subnet and starts watching its region
	e.clock.Step(25 * time.Hour)
	e.sweepOnce() // idle for longer than the kind's default TTL
	if slices.Contains(e.drv.Managed(), subnet.ID) {
		t.Errorf("the subnet of the last NodeClass leaked: %v", e.drv.Managed())
	}
}
