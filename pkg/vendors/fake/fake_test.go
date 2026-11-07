//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package fake_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/conformance"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

// keeperURL is a replay server of the committed snapshot shared by every test:
// the driver caches the catalog tables per URL.
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
	testRegion = "us-east-1"
	testType   = "m7i.large"
	cmNS       = "scarpenter-system"
)

const baseQuota = `
buckets:
  - {id: std-od, scope: region, unit: vCPU, limit: 1000}
  - {id: std-spot, scope: region, unit: vCPU, limit: 1000}
  - {id: all, scope: global, unit: vCPU, limit: 100000}
consumption:
  - {families: ["*"], capacityTypes: [on-demand], buckets: [std-od, all]}
  - {families: ["*"], capacityTypes: [spot], buckets: [std-spot]}
`

type harness struct {
	d *fakedriver.Driver
	c client.Client
}

func newHarness(t *testing.T, data map[string]string) *harness {
	t.Helper()
	cl := fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: cmNS, Name: fakedriver.DefaultConfigName}, Data: data,
	}).Build()
	opts := fakedriver.DefaultOptions()
	opts.ConfigTTL = 0
	opts.StateInterval = time.Hour // tests flush by hand
	d := fakedriver.New(opts)
	if err := d.Init(t.Context(), vendors.Config{
		ClusterName: "my-cluster", Client: cl, Reader: cl, Log: logr.Discard(), KeeperURL: keeperURL,
	}); err != nil {
		t.Fatal(err)
	}
	h := &harness{d: d, c: cl}
	registerClient(h)
	return h
}

func (h *harness) setConfig(t *testing.T, key, value string) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	k := types.NamespacedName{Namespace: cmNS, Name: fakedriver.DefaultConfigName}
	if err := h.c.Get(t.Context(), k, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = value
	if err := h.c.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) req(name, typ, zone, capType string) vendors.CreateRequest {
	return vendors.CreateRequest{
		Region: testRegion, Zone: zone, InstanceType: typ, CapacityType: capType, Arch: "amd64",
		IdempotencyKey: "uid-" + name,
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: "my-cluster", vendors.TagNodeClaim: name,
		},
		Node: vendors.NodeInfo{
			Name: name, ProviderID: "scarpenter://aws/" + testRegion + "/" + name,
			Labels:      map[string]string{"a": "b"},
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1900m")},
		},
	}
}

func (h *harness) zone(t *testing.T) string {
	t.Helper()
	zs, err := h.d.Zones(t.Context(), testRegion)
	if err != nil || len(zs) == 0 {
		t.Fatalf("zones: %v %v", zs, err)
	}
	return zs[0].Name
}

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Fixture{
		New: func(t *testing.T) vendors.Driver {
			return newHarness(t, map[string]string{"quota": baseQuota}).d
		},
		Region: testRegion, InstanceType: testType, MaxUserData: 16 * 1024,
		InstanceTypes: []string{"m7i.large", "c7g.xlarge", "t3.micro", "p4d.24xlarge"},
		Inject: func(t *testing.T, d vendors.Driver, class vendors.Class, req vendors.CreateRequest) {
			t.Helper()
			h := &harness{d: d.(*fakedriver.Driver)}
			h.c = clientOf(t, h.d)
			rule := []map[string]any{{
				"match": map[string]string{
					"instanceType": req.InstanceType, "region": req.Region, "zone": req.Zone, "capacityType": req.CapacityType,
				},
				"error": string(class), "count": 1,
			}}
			raw, _ := json.Marshal(rule)
			h.setConfig(t, "errors", string(raw))
		},
		Profiles: vendors.Profiles{
			vendors.ProfileStorage: {
				"db":    json.RawMessage(`{"data":[{"name":"pgdata","volumeSize":"100Gi","mountPath":"/var/lib/pgdata","filesystem":"xfs"}]}`),
				"local": json.RawMessage(`{"localStorage":{"policy":"mount","mountPath":"/mnt/local"}}`),
			},
			vendors.ProfilePlacement: {
				"colocated": json.RawMessage(`{"placementGroup":{"strategy":"cluster"}}`),
				"dedicated": json.RawMessage(`{"tenancy":"dedicated"}`),
			},
			vendors.ProfileNetwork: {"fast": json.RawMessage(`{"enaExpress":{"enabled":true}}`)},
		},
		ProfileTypes: []vendors.CatalogType{
			{Vendor: "aws", APIReference: "m7i.large", Family: "m7i", VCPUs: 2},
			{Vendor: "aws", APIReference: "m6id.large", Family: "m6id", VCPUs: 2, LocalStorageGB: 118, LocalStorageCount: 1},
		},
		ResourceKind: fakedriver.KindNetwork,
		PreCreate: func(t *testing.T, d vendors.Driver, kind vendors.ResourceKind, region string, tags map[string]string) vendors.ResourceRef {
			t.Helper()
			h := &harness{d: d.(*fakedriver.Driver)}
			h.c = clientOf(t, h.d)
			raw, _ := json.Marshal([]map[string]any{{"kind": string(kind), "region": region, "id": "subnet-user", "name": "user-subnet", "tags": tags}})
			h.setConfig(t, "existingResources", string(raw))
			return vendors.ResourceRef{Kind: kind, Region: region, ID: "subnet-user", Name: "user-subnet"}
		},
		Restart: func(t *testing.T, d vendors.Driver) vendors.Driver {
			t.Helper()
			return restart(t, d.(*fakedriver.Driver))
		},
		ExhaustQuota: func(t *testing.T, d vendors.Driver, _ vendors.CreateRequest) {
			t.Helper()
			h := &harness{d: d.(*fakedriver.Driver)}
			h.c = clientOf(t, h.d)
			h.setConfig(t, "quota", `
buckets:
  - {id: std-od, scope: region, unit: vCPU, limit: 0}
  - {id: std-spot, scope: region, unit: vCPU, limit: 0}
  - {id: all, scope: global, unit: vCPU, limit: 0}
consumption:
  - {families: ["*"], capacityTypes: [on-demand], buckets: [std-od, all]}
  - {families: ["*"], capacityTypes: [spot], buckets: [std-spot]}
`)
		},
	})
}

// restart returns a new driver over the same Kubernetes objects: the account's
// resources live in a ConfigMap, so the new process finds them.
func restart(t *testing.T, d *fakedriver.Driver) *fakedriver.Driver {
	t.Helper()
	cl := clientOf(t, d)
	opts := fakedriver.DefaultOptions()
	opts.ConfigTTL = 0
	opts.StateInterval = time.Hour
	d2 := fakedriver.New(opts)
	if err := d2.Init(t.Context(), vendors.Config{
		ClusterName: "my-cluster", Client: cl, Reader: cl, Log: logr.Discard(), KeeperURL: keeperURL,
	}); err != nil {
		t.Fatal(err)
	}
	clients[d2] = cl
	return d2
}

// The conformance hooks only get the driver; the harness registers the client
// of each driver it builds.
var clients = map[*fakedriver.Driver]client.Client{}

func clientOf(t *testing.T, d *fakedriver.Driver) client.Client {
	t.Helper()
	c, ok := clients[d]
	if !ok {
		t.Fatal("unknown driver")
	}
	return c
}

func init() {
	// Wrap newHarness' result so that clientOf works for conformance hooks.
	registerClient = func(h *harness) { clients[h.d] = h.c }
}

var registerClient = func(*harness) {}

func TestInjectedErrorsFollowTheirCount(t *testing.T) {
	h := newHarness(t, map[string]string{"errors": `
- match: {instanceType: aws.m7i.large, capacityType: spot}
  error: InsufficientCapacity
  count: 2
`})
	zone := h.zone(t)
	for i, want := range []vendors.Class{vendors.InsufficientCapacity, vendors.InsufficientCapacity, ""} {
		_, err := h.d.CreateInstance(t.Context(), h.req("n"+string(rune('a'+i)), testType, zone, "spot"))
		if got := vendors.ClassOf(err); got != want {
			t.Errorf("call %d: class %q, want %q (%v)", i, got, want, err)
		}
	}
	// On-demand does not match the rule.
	if _, err := h.d.CreateInstance(t.Context(), h.req("od", testType, zone, "on-demand")); err != nil {
		t.Errorf("on-demand: %v", err)
	}
}

func TestUnlimitedRuleNeverSpends(t *testing.T) {
	h := newHarness(t, map[string]string{"errors": "- {match: {zone: \"\"}, error: Retryable}\n"})
	for i := range 3 {
		_, err := h.d.CreateInstance(t.Context(), h.req("x"+string(rune('a'+i)), testType, h.zone(t), "spot"))
		if !vendors.IsClass(err, vendors.Retryable) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestBrokenConfigKeepsTheLastGoodValue(t *testing.T) {
	h := newHarness(t, map[string]string{"errors": "- {match: {}, error: Fatal}\n"})
	zone := h.zone(t)
	if _, err := h.d.CreateInstance(t.Context(), h.req("a", testType, zone, "spot")); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("rule not applied: %v", err)
	}
	h.setConfig(t, "errors", "this: [is not")
	if _, err := h.d.CreateInstance(t.Context(), h.req("b", testType, zone, "spot")); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("the last good rule was dropped: %v", err)
	}
	h.setConfig(t, "errors", "- {match: {}, error: NoSuchClass}\n")
	if _, err := h.d.CreateInstance(t.Context(), h.req("c", testType, zone, "spot")); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("an invalid class replaced the rule: %v", err)
	}
	h.setConfig(t, "errors", "")
	if _, err := h.d.CreateInstance(t.Context(), h.req("d", testType, zone, "spot")); err != nil {
		t.Errorf("an emptied key keeps rules: %v", err)
	}
}

func TestNodeShapeAndDeletion(t *testing.T) {
	h := newHarness(t, nil)
	zone := h.zone(t)
	inst, err := h.d.CreateInstance(t.Context(), h.req("claim-1", testType, zone, "on-demand"))
	if err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{}
	if err := h.c.Get(t.Context(), types.NamespacedName{Name: "claim-1"}, node); err != nil {
		t.Fatal(err)
	}
	if node.Spec.ProviderID != "scarpenter://aws/us-east-1/claim-1" {
		t.Errorf("provider ID %q", node.Spec.ProviderID)
	}
	if node.Labels["kwok.x-k8s.io/node"] != "fake" || node.Annotations["kwok.x-k8s.io/node"] != "fake" ||
		node.Labels["a"] != "b" || node.Labels[corev1.LabelHostname] != "claim-1" {
		t.Errorf("labels %v annotations %v", node.Labels, node.Annotations)
	}
	if len(node.Spec.Taints) != 1 || !node.Spec.Taints[0].MatchTaint(&karpv1.UnregisteredNoExecuteTaint) {
		t.Errorf("taints %v", node.Spec.Taints)
	}
	if cpu := node.Status.Allocatable[corev1.ResourceCPU]; cpu.MilliValue() != 1900 {
		t.Errorf("allocatable %v", node.Status.Allocatable)
	}
	if err := h.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Get(t.Context(), types.NamespacedName{Name: "claim-1"}, node); err == nil {
		t.Error("the node survived the instance")
	}
}

func TestQuotaCountsOwnAndForeignUsage(t *testing.T) {
	h := newHarness(t, map[string]string{"quota": `
buckets:
  - {id: std, scope: region, unit: vCPU, limit: 8, foreignUsed: 2, regions: {eu-west-1: {limit: 100, foreignUsed: 0}}}
  - {id: all, scope: global, unit: vCPU, limit: 50, foreignUsed: 10}
consumption:
  - {families: [m7i], capacityTypes: [on-demand], buckets: [std, all]}
`})
	zone := h.zone(t)
	// m7i.large has 2 vCPUs: 2 foreign + 3 launches = 8, the fourth does not fit.
	for i := range 3 {
		if _, err := h.d.CreateInstance(t.Context(), h.req("q"+string(rune('a'+i)), testType, zone, "on-demand")); err != nil {
			t.Fatalf("launch %d: %v", i, err)
		}
	}
	_, err := h.d.CreateInstance(t.Context(), h.req("qz", testType, zone, "on-demand"))
	if !vendors.IsClass(err, vendors.QuotaExceeded) {
		t.Fatalf("fourth launch: %v, want QuotaExceeded", err)
	}
	snap, err := h.d.Quotas(t.Context(), testRegion)
	if err != nil || len(snap.Buckets) != 1 || snap.Buckets[0].Used != 8 || snap.Buckets[0].Limit != 8 || len(snap.Counted) != 3 {
		t.Fatalf("regional snapshot: %+v, %v", snap, err)
	}
	g, err := h.d.GlobalQuotas(t.Context())
	if err != nil || len(g.Buckets) != 1 || g.Buckets[0].Used != 16 {
		t.Fatalf("global snapshot: %+v, %v", g, err)
	}
	other, err := h.d.Quotas(t.Context(), "eu-west-1")
	if err != nil || other.Buckets[0].Limit != 100 || other.Buckets[0].Used != 0 {
		t.Fatalf("override: %+v, %v", other, err)
	}
	// Spot and other families have unknown quota.
	if _, known := h.d.Consumption(testType, "spot"); known {
		t.Error("spot consumption should be unknown")
	}
	if _, known := h.d.Consumption("c7i.large", "on-demand"); known {
		t.Error("c7i consumption should be unknown")
	}
	if c, known := h.d.Consumption(testType, "on-demand"); !known || len(c) != 2 || c[0].Amount != 2 {
		t.Errorf("m7i consumption: %v %v", c, known)
	}
	// Lowering the foreign usage frees room without touching instances.
	h.setConfig(t, "quota", `
buckets:
  - {id: std, scope: region, unit: vCPU, limit: 8, foreignUsed: 0}
  - {id: all, scope: global, unit: vCPU, limit: 50}
consumption:
  - {families: [m7i], capacityTypes: [on-demand], buckets: [std, all]}
`)
	if _, err := h.d.CreateInstance(t.Context(), h.req("qy", testType, zone, "on-demand")); err != nil {
		t.Errorf("after freeing quota: %v", err)
	}
}

func TestNoQuotaConfigIsAVendorWithoutQuotaKnowledge(t *testing.T) {
	h := newHarness(t, nil)
	if _, known := h.d.Consumption(testType, "on-demand"); known {
		t.Error("consumption is known without a quota configuration")
	}
	snap, err := h.d.Quotas(t.Context(), testRegion)
	if err != nil || len(snap.Buckets) != 0 {
		t.Errorf("snapshot: %+v %v", snap, err)
	}
}

func TestNotLaunchable(t *testing.T) {
	h := newHarness(t, map[string]string{"quota": `
notLaunchable:
  - {region: us-east-1, zone: us-east-1a, types: [m7i.large]}
`})
	all, err := h.d.LaunchableTypes(t.Context(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	zs, _ := h.d.Zones(t.Context(), testRegion)
	var blocked string
	for _, z := range zs {
		if z.Name == "us-east-1a" {
			blocked = z.ID
		}
	}
	if blocked == "" {
		t.Skip("the snapshot has no zone us-east-1a")
	}
	if len(all[testType]) != len(zs)-1 {
		t.Errorf("m7i.large launchable in %v of %d zones", all[testType], len(zs))
	}
	for _, id := range all[testType] {
		if id == blocked {
			t.Error("the blocked zone is launchable")
		}
	}
	if len(all["c7i.large"]) != len(zs) {
		t.Errorf("c7i.large launchable in %v", all["c7i.large"])
	}
}

func TestOrphansAreCreatedOnce(t *testing.T) {
	h := newHarness(t, map[string]string{"orphans": `
- {name: orphan-1, region: us-east-1, zone: us-east-1a, instanceType: m7i.large, capacityType: on-demand, ageSeconds: 600}
`})
	tags := map[vendors.TagKey]string{vendors.TagCluster: "my-cluster"}
	{
		list, err := h.d.ListInstances(t.Context(), []string{testRegion}, tags)
		if err != nil || len(list) != 1 {
			t.Fatalf("list: %v %v", list, err)
		}
		if age := time.Since(list[0].CreatedAt); age < 9*time.Minute {
			t.Errorf("orphan age %s", age)
		}
		if list[0].Tags[vendors.TagNodeClaim] != "orphan-1" {
			t.Errorf("tags %v", list[0].Tags)
		}
		if err := h.d.DeleteInstance(t.Context(), list[0].Ref); err != nil {
			t.Fatal(err)
		}
	}
	// Deleted once, not resurrected by the next config read.
	if list, _ := h.d.ListInstances(t.Context(), nil, tags); len(list) != 0 {
		t.Errorf("the orphan came back: %v", list)
	}
}

func TestStateConfigMap(t *testing.T) {
	h := newHarness(t, nil)
	zone := h.zone(t)
	inst, err := h.d.CreateInstance(t.Context(), h.req("s1", testType, zone, "spot"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.d.CreateInstance(t.Context(), h.req("s2", "no.such", zone, "spot"))
	if err := h.d.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: cmNS, Name: fakedriver.DefaultStateName}, cm); err != nil {
		t.Fatal(err)
	}
	var calls []map[string]any
	if err := json.Unmarshal([]byte(cm.Data["calls"]), &calls); err != nil || len(calls) != 2 {
		t.Fatalf("calls %q: %v", cm.Data["calls"], err)
	}
	if calls[0]["result"] != "ok" || calls[0]["op"] != "create" || calls[0]["region"] != testRegion || calls[1]["result"] != "Fatal" {
		t.Errorf("calls %v", calls)
	}
	var instances []map[string]any
	if err := json.Unmarshal([]byte(cm.Data["instances"]), &instances); err != nil || len(instances) != 1 || instances[0]["ref"] != string(inst.Ref) {
		t.Fatalf("instances %q: %v", cm.Data["instances"], err)
	}
	// A later change is written again (update path).
	if err := h.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: cmNS, Name: fakedriver.DefaultStateName}, cm); err != nil {
		t.Fatal(err)
	}
	if !contains(cm.Data["instances"], "terminated") {
		t.Errorf("instances %s", cm.Data["instances"])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestInitDoesNotNeedKeeper(t *testing.T) {
	d := fakedriver.New(fakedriver.DefaultOptions())
	if err := d.Init(t.Context(), vendors.Config{KeeperURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := d.Regions(ctx); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("Regions without Keeper: %v, want Retryable", err)
	}
}

func TestRegistration(t *testing.T) {
	found := false
	for _, n := range vendors.Registered() {
		found = found || n == fakedriver.Name
	}
	if !found {
		t.Fatalf("not registered: %v", vendors.Registered())
	}
	opts := fakedriver.DefaultOptions()
	opts.CatalogVendor = "aws"
	ctx := opts.ToContext(t.Context())
	reg, err := vendors.NewRegistry(ctx, []string{"fake"}, vendors.Config{KeeperURL: keeperURL})
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Enabled("aws") || reg.Enabled("fake") {
		t.Errorf("registry serves %v, want the impersonated vendor aws", reg.Vendors())
	}
}
