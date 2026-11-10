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
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestDefaultsAndRootDisk(t *testing.T) {
	d := newHarness(t, nil).d
	def := d.Defaults()
	if def.RootDiskBytes != 64<<30 || d.RootDiskBytes(def) != 64<<30 {
		t.Errorf("default root disk %d", def.RootDiskBytes)
	}
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{`{"storage":{"root":{"volumeSize":"128Gi"}}}`, 128 << 30},
		{`{"storage":{"root":{"size":"32Gi"}}}`, 32 << 30},
		{`{}`, 64 << 30},
	} {
		if got := d.RootDiskBytes(vendors.VendorSettings{Raw: raw(tc.raw)}); got != tc.want {
			t.Errorf("RootDiskBytes(%s) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestSupportedHonoursTheTypeAndTheSteeredRules(t *testing.T) {
	h := newHarness(t, nil)
	profiles := vendors.Profiles{
		vendors.ProfileStorage: {
			"db":    raw(`{"data":[{"name":"d","size":"10Gi","mountPath":"/d"}]}`),
			"local": raw(`{"localStorage":{"policy":"raid0","mountPath":"/l"}}`),
		},
		vendors.ProfilePlacement: {"colocated": raw(`{"group":{"strategy":"cluster"}}`)},
	}
	plain := vendors.CatalogType{Vendor: "aws", APIReference: "m7i.large", Family: "m7i"}
	withDisks := vendors.CatalogType{Vendor: "aws", APIReference: "m6id.large", Family: "m6id", LocalStorageGB: 118, LocalStorageCount: 1}
	got := h.d.Supported(plain, "z", profiles)
	if !slices.Equal(got[vendors.ProfileStorage], []string{"db"}) || !slices.Equal(got[vendors.ProfilePlacement], []string{"colocated"}) {
		t.Errorf("a type without local disks: %v; local storage must not be hosted", got)
	}
	if got := h.d.Supported(withDisks, "z", profiles); !slices.Equal(got[vendors.ProfileStorage], []string{"db", "local"}) {
		t.Errorf("a type with local disks: %v", got)
	}

	before := h.d.NodeOptionsEpoch()
	h.setConfig(t, "unsupported", `
- {kind: storage, profile: db, instanceTypes: [aws.m7i.large]}
- {kind: placement, profile: colocated, families: [c7i], zones: [z]}
`)
	if h.d.NodeOptionsEpoch() == before {
		t.Error("the epoch did not change with the rules: the catalog would keep its old profile labels")
	}
	got = h.d.Supported(plain, "z", profiles)
	if slices.Contains(got[vendors.ProfileStorage], "db") || !slices.Contains(got[vendors.ProfilePlacement], "colocated") {
		t.Errorf("after the rule: %v", got)
	}
	if got := h.d.Supported(vendors.CatalogType{Vendor: "aws", APIReference: "c7i.large", Family: "c7i"}, "z", profiles); slices.Contains(got[vendors.ProfilePlacement], "colocated") {
		t.Errorf("the family rule did not apply: %v", got)
	}
	if got := h.d.Supported(vendors.CatalogType{Vendor: "aws", APIReference: "c7i.large", Family: "c7i"}, "other-zone", profiles); !slices.Contains(got[vendors.ProfilePlacement], "colocated") {
		t.Errorf("the zone rule applied elsewhere: %v", got)
	}
}

func TestPriceDelta(t *testing.T) {
	d := newHarness(t, nil).d
	off := vendors.Offering{Region: testRegion}
	if delta, ok := d.PriceDelta(d.Defaults(), off); delta != 0 || !ok {
		t.Errorf("the defaults cost %v, complete=%v; the reference catalog of the L3 lane needs 0", delta, ok)
	}
	volume := vendors.VendorSettings{Raw: raw(`{"storage":{"root":{"volumeSize":"64Gi"},"data":[{"name":"d","volumeSize":"730Gi"}]}}`)}
	if delta, ok := d.PriceDelta(volume, off); !ok || !strings.HasPrefix(strconv.FormatFloat(delta, 'f', 6, 64), "0.100000") {
		t.Errorf("730 GiB cost %v (complete=%v), want 0.10 per hour", delta, ok)
	}
	root := vendors.VendorSettings{Raw: raw(`{"storage":{"root":{"volumeSize":"128Gi"}}}`)}
	if delta, _ := d.PriceDelta(root, off); delta <= 0 {
		t.Errorf("a larger root disk is free: %v", delta)
	}
	for name, s := range map[string]vendors.VendorSettings{
		"iops":       {Raw: raw(`{"storage":{"root":{"volumeSize":"64Gi","iops":4000}}}`)},
		"throughput": {Raw: raw(`{"storage":{"data":[{"name":"d","volumeSize":"1Gi","throughput":250}]}}`)},
		"tenancy":    {Raw: raw(`{}`), Placement: raw(`{"tenancy":"dedicated"}`)},
	} {
		if _, ok := d.PriceDelta(s, off); ok {
			t.Errorf("%s is not priced: the price must be incomplete", name)
		}
	}
}

// provider hands out resources by kind and key, like the engine would.
type provider struct {
	d    *fakedriver.Driver
	reqs []vendors.ResourceRequest
}

func (p *provider) Require(ctx context.Context, r vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	p.reqs = append(p.reqs, r)
	if r.Use != nil {
		return p.d.Resolve(ctx, r.Kind, testRegion, *r.Use)
	}
	ref, _, err := p.d.Ensure(ctx, r.Kind, testRegion, vendors.CreateSpec{
		Name: "n-" + string(r.Kind) + "-" + r.Zone + "-" + strings.ReplaceAll(r.Key, "/", "_"),
		Tags: map[vendors.TagKey]string{vendors.TagCluster: "my-cluster", vendors.TagManagedBy: vendors.ManagedByValue},
		Zone: r.Zone, Params: r.Params,
	})
	return []vendors.ResourceRef{ref}, err
}

// preparer runs Prepare for a type with two local disks in the harness's zone.
type preparer struct {
	t    *testing.T
	h    *harness
	zone string
}

func newPreparer(t *testing.T) *preparer {
	h := newHarness(t, nil)
	return &preparer{t: t, h: h, zone: h.zone(t)}
}

func (p *preparer) prepare(settings vendors.VendorSettings, runKey string) (*vendors.Prepared, *provider, error) {
	pr := &provider{d: p.h.d}
	prep, err := p.h.d.Prepare(p.t.Context(), vendors.PrepareRequest{
		Region: testRegion, Zone: p.zone, InstanceType: "m6id.large", CapacityType: "on-demand",
		Type:     vendors.CatalogType{LocalStorageGB: 118, LocalStorageCount: 2},
		Settings: settings, RunKey: runKey, Resources: pr,
		Tags: map[vendors.TagKey]string{vendors.TagNodeClaim: "claim-1"},
	})
	return prep, pr, err
}

func TestPrepareTheDefaultRouteNeedsTheRegionalNetworkOnly(t *testing.T) {
	p := newPreparer(t)
	def := p.h.d.Defaults()
	def.Profiles = map[string]string{"placement": "default"}
	prep, pr, err := p.prepare(def, "")
	if err != nil || len(prep.Resources) != 1 || prep.Resources[0].Kind != fakedriver.KindNetwork ||
		len(prep.Devices) != 0 || prep.Local != nil {
		t.Fatalf("the default route needs the regional network only: %+v, %v", prep, err)
	}
	if len(pr.reqs) != 1 {
		t.Errorf("requests %+v", pr.reqs)
	}
}

var withVolumes = vendors.VendorSettings{
	Raw: raw(`{"storage":{"data":[` +
		`{"name":"pgdata","volumeSize":"500Gi","filesystem":"xfs","mountPath":"/var/lib/pgdata"},` +
		`{"name":"b","size":"1Gi","mountPath":"/mnt/b"}],"localStorage":{"policy":"mount","mountPath":"/mnt/local"}}}`),
	Placement: raw(`{"placementGroup":{"strategy":"cluster"}}`),
	Profiles:  map[string]string{"placement": "colocated"},
}

func TestPrepareAGroupNeedsARunKey(t *testing.T) {
	p := newPreparer(t)
	if _, _, err := p.prepare(withVolumes, ""); !vendors.IsClass(err, vendors.Fatal) || !strings.Contains(err.Error(), "run key") {
		t.Errorf("a group without a run key: %v, want a Fatal error naming it", err)
	}
}

func TestPrepareVolumesGroupAndLocalStorage(t *testing.T) {
	p := newPreparer(t)
	prep, pr, err := p.prepare(withVolumes, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[vendors.ResourceKind]int{}
	for _, r := range prep.Resources {
		kinds[r.Kind]++
	}
	if kinds[fakedriver.KindNetwork] != 1 || kinds[fakedriver.KindGroup] != 1 || kinds[fakedriver.KindVolume] != 2 {
		t.Errorf("resources %v", kinds)
	}
	if len(prep.Devices) != 2 || prep.Devices[0].Name != "pgdata" || prep.Devices[0].Filesystem != "xfs" ||
		!strings.HasPrefix(prep.Devices[0].ByID, "/dev/disk/by-id/fake-fake-volumes-") || prep.Devices[1].Filesystem != "ext4" {
		t.Errorf("device hints %+v", prep.Devices)
	}
	if prep.Local == nil || prep.Local.Policy != "mount" || prep.Local.Count != 2 || prep.Local.MountPath != "/mnt/local" {
		t.Errorf("local hint %+v", prep.Local)
	}
	for _, r := range pr.reqs {
		if r.Kind == fakedriver.KindGroup && (r.Key != "colocated/run-1" || !strings.Contains(string(r.Params), "cluster")) {
			t.Errorf("the group request %+v", r)
		}
		if r.Kind == fakedriver.KindVolume && !strings.HasPrefix(r.Key, "claim-1/"+p.zone+"/") {
			t.Errorf("a volume's key must tie it to the node and zone: %+v", r)
		}
	}
}

func TestPrepareNeverCreatesAGroupThatAProfileReferences(t *testing.T) {
	p := newPreparer(t)
	existing := vendors.VendorSettings{
		Raw: raw(`{}`), Placement: raw(`{"placementGroup":{"use":{"name":"my-pg"}}}`), Profiles: map[string]string{"placement": "mine"},
	}
	p.h.setConfig(t, "existingResources", `- {kind: placementGroups, region: us-east-1, id: pg-mine, name: my-pg}`)
	prep, _, err := p.prepare(existing, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var group vendors.ResourceRef
	for _, r := range prep.Resources {
		if r.Kind == fakedriver.KindGroup {
			group = r
		}
	}
	if group.ID != "pg-mine" || group.Managed {
		t.Errorf("group %+v, want the user's", group)
	}
}

func TestResourcesAreUsedPersistedAndDeletedByTheirUsers(t *testing.T) {
	h := newHarness(t, nil)
	zone := h.zone(t)
	spec := vendors.CreateSpec{Name: "scarpenter-my-cluster-subnets-us-east-1", Tags: map[vendors.TagKey]string{
		vendors.TagCluster: "my-cluster", vendors.TagManagedBy: vendors.ManagedByValue,
	}}
	ref, created, err := h.d.Ensure(t.Context(), fakedriver.KindNetwork, testRegion, spec)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	// Another name collides without the marker.
	if _, _, err := h.d.Ensure(t.Context(), fakedriver.KindNetwork, testRegion, vendors.CreateSpec{Name: spec.Name}); err == nil {
		t.Error("a resource of the same name without the ownership marker was adopted")
	}
	req := h.req("claim-1", testType, zone, "on-demand")
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{ref}}
	inst, err := h.d.CreateInstance(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.Delete(t.Context(), ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("deleting a resource in use: %v", err)
	}
	// A powered-off (terminating) instance still holds its resources.
	h.d.PowerOff(inst.Ref)
	if inUse, _, err := h.d.InUse(t.Context(), ref); err != nil || !inUse {
		t.Fatalf("InUse with a powered-off user = %v, %v; want in use", inUse, err)
	}
	if err := h.d.DeleteInstance(t.Context(), inst.Ref); err != nil {
		t.Fatal(err)
	}
	inUse, last, err := h.d.InUse(t.Context(), ref)
	if err != nil || inUse || last.IsZero() {
		t.Fatalf("InUse = %v, %v, %v", inUse, last, err)
	}
	// A new process sees the resource; deleting it there removes it for everyone.
	d2 := restart(t, h.d)
	listed, err := d2.List(t.Context(), nil, nil)
	if err != nil || len(listed) != 1 || listed[0].ID != ref.ID {
		t.Fatalf("after a restart: %+v, %v", listed, err)
	}
	if err := d2.Delete(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	d3 := restart(t, h.d)
	if listed, _ := d3.List(t.Context(), nil, nil); len(listed) != 0 {
		t.Errorf("the deletion was not persisted: %+v", listed)
	}
}

func TestResolveByNamePatternIDsTagsAndDefaults(t *testing.T) {
	h := newHarness(t, map[string]string{"existingResources": `
- {kind: subnets, region: us-east-1, id: s-1, name: sc-us-east-1, tags: {use: "true"}}
- {kind: subnets, region: us-east-1, id: s-2, name: other, tags: {use: "false"}}
- {kind: subnets, region: us-east-1, id: s-default, default: true}
- {kind: subnets, region: eu-west-1, id: s-3, name: sc-eu-west-1}
`})
	resolve := func(ref vendors.UseRef, region string) []string {
		refs, err := h.d.Resolve(t.Context(), fakedriver.KindNetwork, region, ref)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(refs))
		for _, r := range refs {
			if r.Managed {
				t.Errorf("%s is managed", r.ID)
			}
			ids = append(ids, r.ID)
		}
		return ids
	}
	for name, tc := range map[string]struct {
		ref    vendors.UseRef
		region string
		want   []string
	}{
		"ids":         {vendors.UseRef{IDs: []string{"s-2"}}, "us-east-1", []string{"s-2"}},
		"name":        {vendors.UseRef{NamePattern: "sc-us-*"}, "us-east-1", []string{"s-1"}},
		"tags":        {vendors.UseRef{Tags: map[string]string{"use": "true"}}, "us-east-1", []string{"s-1"}},
		"default":     {vendors.UseRef{}, "us-east-1", []string{"s-default"}},
		"no default":  {vendors.UseRef{}, "eu-west-1", nil},
		"other zone":  {vendors.UseRef{NamePattern: "sc-*"}, "eu-west-1", []string{"s-3"}},
		"no matching": {vendors.UseRef{Tags: map[string]string{"use": "maybe"}}, "us-east-1", nil},
	} {
		if got := resolve(tc.ref, tc.region); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// flaky fails the first writes of the state ConfigMap.
func flakyDriver(t *testing.T, failures int32, written chan<- string) (*fakedriver.Driver, client.Client) {
	t.Helper()
	var failed atomic.Int32
	cl := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == fakedriver.DefaultStateName {
				if failed.Add(1) <= failures {
					return errors.New("the API server is down")
				}
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				written <- cm.Data["revision"]
				return nil
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == fakedriver.DefaultStateName {
				if failed.Add(1) <= failures {
					return errors.New("the API server is down")
				}
				if err := c.Update(ctx, obj, opts...); err != nil {
					return err
				}
				written <- cm.Data["revision"]
				return nil
			}
			return c.Update(ctx, obj, opts...)
		},
	}).Build()
	opts := fakedriver.DefaultOptions()
	opts.ConfigTTL = 0
	opts.StateInterval = time.Millisecond
	d := fakedriver.New(opts)
	if err := d.Init(t.Context(), vendors.Config{ClusterName: "my-cluster", Client: cl, Reader: cl, Log: logr.Discard(), KeeperURL: keeperURL}); err != nil {
		t.Fatal(err)
	}
	return d, cl
}

func TestAFailedStateWriteIsRetriedUntilItSucceeds(t *testing.T) {
	written := make(chan string, 32)
	d, cl := flakyDriver(t, 3, written)
	zones, err := d.Zones(t.Context(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{d: d, c: cl}
	if _, err := d.CreateInstance(t.Context(), h.req("claim-1", testType, zones[0].Name, "on-demand")); err != nil {
		t.Fatal(err)
	}
	// The scheduled writes fail three times, then the state reaches the API
	// server by itself: nothing here waits for a debounce interval.
	select {
	case rev := <-written:
		if rev == "" || rev == "0" {
			t.Errorf("the written state carries the revision %q", rev)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the state was never written although the writes are retried")
	}
	cm := &corev1.ConfigMap{}
	if err := cl.Get(t.Context(), types.NamespacedName{Namespace: cmNS, Name: fakedriver.DefaultStateName}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["instances"], "claim-1") {
		t.Errorf("the state lacks the instance: %s", cm.Data["instances"])
	}
}

func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := metrics.Registry.(prometheus.Gatherer).Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no metric %s", name)
	return 0
}

func TestStateRevisionIsPublishedAndAcknowledged(t *testing.T) {
	h := newHarness(t, nil)
	zone := h.zone(t)
	if _, err := h.d.CreateInstance(t.Context(), h.req("claim-1", testType, zone, "on-demand")); err != nil {
		t.Fatal(err)
	}
	current := gaugeValue(t, "scarpenter_fake_state_revision")
	if current < 1 {
		t.Fatalf("revision %v after a launch", current)
	}
	if published := gaugeValue(t, "scarpenter_fake_state_published_revision"); published >= current {
		t.Fatalf("published %v before any write, current %v", published, current)
	}
	if err := h.d.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: cmNS, Name: fakedriver.DefaultStateName}, cm); err != nil {
		t.Fatal(err)
	}
	rev, err := strconv.ParseFloat(cm.Data["revision"], 64)
	if err != nil || rev != current {
		t.Errorf("the ConfigMap's revision %q, current %v: a waiter that reads the current revision and waits for the ConfigMap to reach it must be released", cm.Data["revision"], current)
	}
	if published := gaugeValue(t, "scarpenter_fake_state_published_revision"); published != current {
		t.Errorf("published %v, want %v", published, current)
	}
	// Another change moves the revision on.
	if err := h.d.DeleteInstance(t.Context(), vendors.NewInstanceRef(testRegion, "i-fake-000001")); err != nil {
		t.Fatal(err)
	}
	if now := gaugeValue(t, "scarpenter_fake_state_revision"); now <= current {
		t.Errorf("revision %v did not grow after a deletion", now)
	}
}

func TestStateRecordsSettingsResourcesAndDevices(t *testing.T) {
	h := newHarness(t, nil)
	zone := h.zone(t)
	ref := vendors.ResourceRef{Kind: fakedriver.KindVolume, Region: testRegion, ID: "vol-1"}
	req := h.req("claim-1", testType, zone, "on-demand")
	req.Settings = vendors.VendorSettings{Raw: raw(`{"storage":{}}`), Profiles: map[string]string{"storage": "db"}}
	req.Prepared = &vendors.Prepared{
		Resources: []vendors.ResourceRef{ref},
		Devices:   []vendors.DeviceHint{{Name: "pgdata", ByID: "/dev/disk/by-id/x", Filesystem: "xfs", MountPath: "/data"}},
		Local:     &vendors.LocalHint{DevicePattern: "fake-local-*", Policy: "raid0", MountPath: "/l", Count: 2},
	}
	if _, err := h.d.CreateInstance(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: cmNS, Name: fakedriver.DefaultStateName}, cm); err != nil {
		t.Fatal(err)
	}
	var insts []struct {
		Settings  string
		Profiles  map[string]string
		Resources []string
		Devices   []string
	}
	if err := json.Unmarshal([]byte(cm.Data["instances"]), &insts); err != nil || len(insts) != 1 {
		t.Fatalf("%v: %s", err, cm.Data["instances"])
	}
	i := insts[0]
	if i.Settings != `{"storage":{}}` || i.Profiles["storage"] != "db" || !slices.Equal(i.Resources, []string{"vol-1"}) ||
		len(i.Devices) != 2 || i.Devices[0] != "pgdata=/dev/disk/by-id/x xfs /data" || !strings.HasPrefix(i.Devices[1], "local=fake-local-* raid0 /l") {
		t.Errorf("instance record %+v", i)
	}
}
