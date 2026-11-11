/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package ovh

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"

	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/fake"
	"github.com/SpareCores/scarpenter/pkg/vendors/ovh/osapi"
)

var ctx = context.Background()

func launchReq(region, zone string, n int) vendors.CreateRequest {
	name := fmt.Sprintf("node-%d", n)
	return vendors.CreateRequest{
		Region: region, Zone: zone, InstanceType: testType, CapacityType: vendors.CapacityOnDemand, Arch: archAMD64,
		Image: vendors.Image{Ref: "image"}, UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Settings: vendors.VendorSettings{Raw: json.RawMessage(defaultSettings)},
		Tags: map[vendors.TagKey]string{
			vendors.TagCluster: conformanceCluster, vendors.TagNodeClaim: name, vendors.TagNodeClass: "class",
			vendors.TagManagedBy: vendors.ManagedByValue,
		},
		IdempotencyKey: "uid-" + name, Node: vendors.NodeInfo{Name: name},
	}
}

func TestInit(t *testing.T) {
	t.Setenv("OS_AUTH_URL", "")
	t.Setenv("OS_USERNAME", "")
	t.Setenv("OS_PASSWORD", "")
	t.Setenv("OS_PROJECT_ID", "")
	t.Setenv("OS_PROJECT_NAME", "")
	cfg := vendors.Config{ClusterName: "c", Log: logr.Discard()}
	if err := New().Init(ctx, cfg); err == nil || !strings.Contains(err.Error(), "OS_AUTH_URL") {
		t.Errorf("no credentials: %v", err)
	}
	t.Setenv("OS_AUTH_URL", "https://auth.example.test/v3")
	t.Setenv("OS_USERNAME", "u")
	t.Setenv("OS_PASSWORD", "p")
	t.Setenv("OS_PROJECT_ID", "0a1b")
	d := New()
	if err := d.Init(ctx, cfg); err != nil {
		t.Fatalf("with credentials and no network: %v", err)
	}
	if d.Vendor() != Vendor || d.TagKey(vendors.TagCluster) != "scarpenter-cluster" {
		t.Errorf("identity: %s %s", d.Vendor(), d.TagKey(vendors.TagCluster))
	}
	t.Setenv(ScheduleWaitEnv, "soon")
	if err := New(WithProvider(fake.New())).Init(ctx, cfg); err == nil {
		t.Error("a bad schedule wait is refused")
	}
	t.Setenv(ScheduleWaitEnv, "20s")
	t.Setenv(RegionAliasesEnv, "GRA11=GRA11X, SBG5=")
	d = New(WithProvider(fake.New()))
	if err := d.Init(ctx, cfg); err != nil || d.wait.Timeout != 20*time.Second {
		t.Fatalf("schedule wait %v: %v", d.wait, err)
	}
	if a, ok := d.regionMap.API("GRA11"); !ok || a != "GRA11X" {
		t.Errorf("the alias row: %s %v", a, ok)
	}
	if _, ok := d.regionMap.API("SBG5"); ok {
		t.Error("an empty API name leaves the region unmapped")
	}
	t.Setenv(RegionAliasesEnv, "GRA11")
	if err := New(WithProvider(fake.New())).Init(ctx, cfg); err == nil {
		t.Error("an alias that is not KEEPER=API is refused")
	}
	t.Setenv(RegionAliasesEnv, "GRA9=GRA11") // two Keeper regions on one API name
	if err := New(WithProvider(fake.New())).Init(ctx, cfg); err == nil {
		t.Error("two Keeper regions on one API region are refused")
	}
}

func TestRegionsAndZones(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	regions, err := d.Regions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(regions))
	for _, r := range regions {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{multiRegion, testRegion}) {
		t.Errorf("regions %v: SBG5 has no endpoint and XYZ1 is not in the table", names)
	}
	// Cached for an hour: a region added later does not show.
	w.cloud.AddRegion("GRA9", fake.RegionConfig{})
	if again, _ := d.Regions(ctx); len(again) != 2 {
		t.Errorf("regions are cached: %v", again)
	}

	zs, err := d.Zones(ctx, testRegion)
	if err != nil || len(zs) != 1 || zs[0].ID != "gra11-a" || zs[0].Name != "nova" {
		t.Errorf("a 1-AZ region has the dummy zone: %v %v", zs, err)
	}
	zs, err = d.Zones(ctx, multiRegion)
	if err != nil || len(zs) != 3 || zs[2].ID != "eu-west-par-c" || zs[2].Name != "eu-west-par-c" {
		t.Errorf("a 3-AZ region: %v %v", zs, err)
	}
	if _, err := d.Zones(ctx, "XYZ1"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("a region outside the table: %v", err)
	}
	if _, err := d.Zones(ctx, "SBG5"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("a region without an endpoint: %v", err)
	}

	bad := newWorld(t)
	bad.cloud.InjectError("", "Regions", &osapi.APIError{Service: "keystone", Status: 401, Message: "no"}, 0)
	if _, err := bad.driver(t).Regions(ctx); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("the catalog read with bad credentials: %v", err)
	}
}

func TestVendorArchAndBilling(t *testing.T) {
	d := New()
	for arch, want := range map[string]string{archAMD64: "x86_64", archARM64: "aarch64"} {
		if got, ok := d.VendorArch(arch); !ok || got != want {
			t.Errorf("VendorArch(%s) = %s", arch, got)
		}
	}
	if _, ok := d.VendorArch("riscv"); ok {
		t.Error("an unknown architecture has no name")
	}
	r, ok := d.BillingRule("GRA11", "b3")
	if !ok || r.Verified || r.Increment == nil || *r.Increment != time.Second || *r.Minimum != time.Minute ||
		r.StoppedCharged == nil || !*r.StoppedCharged || r.Source == "" {
		t.Errorf("a per-second region is unverified: %+v", r)
	}
	r, _ = d.BillingRule("US-EAST-VA-1", "b3")
	if r.Verified || *r.Increment != time.Hour {
		t.Errorf("the US rule is hourly and unverified: %+v", r)
	}
	frags, err := d.BootFragments(vendors.BootFragmentRequest{Hardening: []string{vendors.HardeningMetadataFromPods}})
	if err != nil || len(frags) != 1 || !strings.Contains(frags[0].Butane, MetadataAddress) {
		t.Errorf("the hardening fragment: %v %v", frags, err)
	}
	if frags, _ := d.BootFragments(vendors.BootFragmentRequest{}); len(frags) != 0 {
		t.Error("no hardening, no fragment")
	}
	if !slices.Equal(d.Hardenings(), []string{vendors.HardeningMetadataFromPods}) {
		t.Error("the offered hardenings")
	}
}

func TestCreateWaitsForTheScheduler(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	w.cloud.SetNoValidHost(testRegion, testType, true)
	req := launchReq(testRegion, testZone, 1)
	_, err := d.CreateInstance(ctx, req)
	if !vendors.IsClass(err, vendors.InsufficientCapacity) {
		t.Fatalf("a flavor with no host: %v", err)
	}
	if ids := w.cloud.ServerIDs(testRegion); len(ids) != 1 {
		// The failed server is deleted: it stays DELETING for the fake's delete reads.
		t.Logf("servers after the failure: %v", ids)
	}
	got, err := d.ListInstances(ctx, []string{testRegion}, nil)
	if err != nil || len(got) != 0 {
		t.Errorf("the failed server is not listed: %v %v", got, err)
	}
	w.cloud.SetNoValidHost(testRegion, testType, false)
	if _, err := d.CreateInstance(ctx, req); err != nil {
		t.Errorf("the same launch after the capacity came back: %v", err)
	}
}

func TestCreateCleansUpWhatAnEarlierAttemptLeft(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	w.cloud.SetNoValidHost(testRegion, testType, true)
	// The delete of the ERROR server fails: the key stays on a broken server.
	w.cloud.InjectError(testRegion, "DeleteServer", &osapi.APIError{Service: "nova", Status: 503, Message: "busy"}, 1)
	req := launchReq(testRegion, testZone, 2)
	_, err := d.CreateInstance(ctx, req)
	if !vendors.IsClass(err, vendors.Retryable) || vendors.CodeOf(err) != CodeErrorServerLeft {
		t.Fatalf("a failed delete of the ERROR server: %v", err)
	}
	w.cloud.SetNoValidHost(testRegion, testType, false)
	// The server is in ERROR for good (the fake only advances it on a read it already had).
	inst, err := d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatalf("the retry deletes the broken server and creates: %v", err)
	}
	live, _ := d.ListInstances(ctx, []string{testRegion}, map[vendors.TagKey]string{vendors.TagNodeClaim: "node-2"})
	if len(live) != 1 || live[0].Ref != inst.Ref {
		t.Errorf("one live server: %v", live)
	}

	// A server of the same name that belongs to an earlier NodeClaim is deleted and the
	// launch is retried.
	again := launchReq(testRegion, testZone, 2)
	again.IdempotencyKey = "uid-of-the-next-claim"
	_, err = d.CreateInstance(ctx, again)
	if !vendors.IsClass(err, vendors.Retryable) || vendors.CodeOf(err) != CodeStaleServer {
		t.Fatalf("a leftover of another claim: %v", err)
	}
	if _, err := d.CreateInstance(ctx, again); err != nil {
		t.Errorf("after the leftover is gone: %v", err)
	}
}

func TestCreateRefusals(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	req := launchReq(testRegion, testZone, 3)
	req.InstanceType = "b3-9999"
	if _, err := d.CreateInstance(ctx, req); !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != CodeFlavorUnusable {
		t.Errorf("a flavor the region lacks: %v", err)
	}
	req = launchReq(testRegion, testZone, 3)
	req.UserData = make([]byte, MaxUserData+1)
	if _, err := d.CreateInstance(ctx, req); vendors.CodeOf(err) != CodeUserDataTooLarge {
		t.Errorf("oversized user data: %v", err)
	}
	req = launchReq(testRegion, testZone, 3)
	f := false
	req.Settings.Networking.PublicIPv4 = &f
	if _, err := d.CreateInstance(ctx, req); vendors.CodeOf(err) != CodeInvalidSettings {
		t.Errorf("a private-only node: %v", err)
	}
	req = launchReq(testRegion, testZone, 3)
	req.Node.Name, req.Tags[vendors.TagNodeClaim] = "", ""
	if _, err := d.CreateInstance(ctx, req); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a launch without a name: %v", err)
	}
	req = launchReq("SBG5", "nova", 3)
	if _, err := d.CreateInstance(ctx, req); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("a region without an endpoint: %v", err)
	}
	req = launchReq(testRegion, testZone, 3)
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{{Kind: KindSecurityGroups, ID: "missing"}}}
	if _, err := d.CreateInstance(ctx, req); !vendors.IsClass(err, vendors.Unsupported) {
		t.Errorf("a security group that does not exist: %v", err)
	}
}

func TestCreateWithGroupsAndVolumes(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	mr := vendors.ManagedResources(d)
	tags := map[vendors.TagKey]string{
		vendors.TagCluster: conformanceCluster, vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: string(KindSecurityGroups),
	}
	sg, _, err := mr.Ensure(ctx, KindSecurityGroups, multiRegion, vendors.CreateSpec{Name: "scarpenter-conformance-sg", Tags: tags})
	if err != nil {
		t.Fatal(err)
	}
	sgTags := maps2(tags, vendors.TagResource, string(KindServerGroups))
	grp, created, err := mr.Ensure(ctx, KindServerGroups, multiRegion, vendors.CreateSpec{
		Name: "scarpenter-conformance-group", Tags: sgTags, Params: json.RawMessage(`{"policy":"affinity"}`),
	})
	if err != nil || !created {
		t.Fatal(err)
	}
	req := launchReq(multiRegion, "eu-west-par-b", 4)
	// The group is handed over by ID alone: its name is looked up.
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{{Kind: KindSecurityGroups, ID: sg.ID}, grp}}
	req.Settings.Raw = json.RawMessage(`{"configDrive":false,"storage":{"root":{"bootFromVolume":{"volumeType":"high-speed","size":"64Gi"}},` +
		`"data":[{"name":"d","volumeType":"classic","size":"20Gi","mountPath":"/data"}]}}`)
	inst, err := d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Zone != "eu-west-par-b" {
		t.Errorf("the zone of a 3-AZ region goes to Nova: %q", inst.Zone)
	}
	if w.cloud.VolumeCount(multiRegion) != 2 {
		t.Errorf("a root and a data volume, got %d", w.cloud.VolumeCount(multiRegion))
	}
	if in, _, err := mr.InUse(ctx, sg); err != nil || !in {
		t.Errorf("the group of a server is in use: %v %v", in, err)
	}
	if in, _, err := mr.InUse(ctx, grp); err != nil || !in {
		t.Errorf("the server group with a member is in use: %v %v", in, err)
	}
	if err := mr.Delete(ctx, grp); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("deleting a server group with members: %v", err)
	}
	if err := d.DeleteInstance(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetInstance(ctx, inst.Ref); err == nil {
		// still deleting on the first read after the delete
		t.Log("deleting")
	}
	for range 3 {
		_, _ = d.ListInstances(ctx, []string{multiRegion}, nil) // reads advance the fake's delete
	}
	if w.cloud.VolumeCount(multiRegion) != 0 {
		t.Errorf("the volumes go with the server: %d", w.cloud.VolumeCount(multiRegion))
	}
	if err := mr.Delete(ctx, grp); err != nil {
		t.Errorf("an unused server group is deleted: %v", err)
	}
	if err := mr.Delete(ctx, grp); err != nil {
		t.Errorf("deleting it again: %v", err)
	}
}

func maps2(m map[vendors.TagKey]string, k vendors.TagKey, v string) map[vendors.TagKey]string {
	out := maps.Clone(m)
	out[k] = v
	return out
}

func TestFindAndListStates(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	a, err := d.CreateInstance(ctx, launchReq(testRegion, testZone, 5))
	if err != nil {
		t.Fatal(err)
	}
	for range 5 { // reads advance the fake's state machine to ACTIVE
		_, _ = d.GetInstance(ctx, a.Ref)
	}
	w.cloud.PowerOff(testRegion, a.Ref.ID())
	got, err := d.FindInstance(ctx, testRegion, map[vendors.TagKey]string{vendors.TagNodeClaim: "node-5"})
	if err != nil || got.State != vendors.StateTerminating {
		t.Errorf("the powered-off server is found, terminating: %+v %v", got, err)
	}
	if _, err := d.FindInstance(ctx, "NO-SUCH-1", nil); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("a region outside the catalog has no instances: %v", err)
	}
	if err := d.DeleteInstance(ctx, vendors.NewInstanceRef("NO-SUCH-1", a.Ref.ID())); err != nil {
		t.Errorf("deleting in a region outside the catalog: %v", err)
	}
	w.cloud.InjectError(testRegion, "ListServers", &osapi.APIError{Service: "nova", Status: 503}, 1)
	if _, err := d.ListInstances(ctx, []string{testRegion}, nil); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("a failing list: %v", err)
	}
	w.cloud.InjectError(testRegion, "DeleteServer", &osapi.APIError{Service: "nova", Status: 409, Message: "task state"}, 1)
	if err := d.DeleteInstance(ctx, a.Ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("a delete in a transient task state: %v", err)
	}
}

func TestQuotaAndOfferings(t *testing.T) {
	w := newWorld(t)
	d := w.driver(t)
	types, err := d.LaunchableTypes(ctx, multiRegion)
	if err != nil {
		t.Fatal(err)
	}
	if zs := types[testType]; len(zs) != 3 || zs[0] != "eu-west-par-a" {
		t.Errorf("a launched type is offered in every zone: %v", zs)
	}
	for typ := range types {
		if ok, _ := Launchable(typ); !ok {
			t.Errorf("%s is offered but not launched", typ)
		}
	}
	snap, err := d.Quotas(ctx, testRegion)
	if err != nil || len(snap.Buckets) != 3 {
		t.Fatalf("%v %v", snap, err)
	}
	if g, err := d.GlobalQuotas(ctx); err != nil || len(g.Buckets) != 0 {
		t.Errorf("no global buckets: %v %v", g, err)
	}
	if c, ok := d.Consumption(testType, vendors.CapacityOnDemand); !ok || len(c) != 3 {
		t.Errorf("consumption %v %v", c, ok)
	}
	if _, ok := d.Consumption(testType, vendors.CapacitySpot); ok {
		t.Error("there is no spot")
	}
	if _, err := d.LaunchableTypes(ctx, "SBG5"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("a region without an endpoint: %v", err)
	}
}

func TestKeeperPrices(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		_, _ = w.Write([]byte(`[
		 {"vendor_id":"ovh","region_id":"gra11","storage_id":"High-Speed","unit":"GB/month","price":0.0916,"currency":"USD","status":"active"},
		 {"vendor_id":"ovh","region_id":"GRA11","storage_id":"classic","unit":"GB/month","price":0.0451,"currency":"USD","status":"inactive"},
		 {"vendor_id":"aws","region_id":"GRA11","storage_id":"classic","unit":"GB/month","price":1,"currency":"USD","status":"active"},
		 {"vendor_id":"ovh","region_id":"GRA11","storage_id":"classic","unit":"GB/hour","price":1,"currency":"USD","status":"active"}]`))
	}))
	defer srv.Close()
	w := newWorld(t)
	d := New(WithProvider(w.cloud))
	if err := d.Init(ctx, vendors.Config{ClusterName: "c", KeeperURL: srv.URL, Clock: clock.RealClock{}, Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	before := d.NodeOptionsEpoch()
	if err := d.LoadPrices(ctx); err != nil {
		t.Fatal(err)
	}
	if d.NodeOptionsEpoch() == before {
		t.Error("the options epoch changes when prices load")
	}
	if !strings.Contains(asked, "vendor=ovh") {
		t.Errorf("asked %s", asked)
	}
	if p, ok := d.storagePrice("GRA11", "high-speed"); !ok || p != 0.0916 {
		t.Errorf("price %v %v", p, ok)
	}
	if _, ok := d.storagePrice("GRA11", "classic"); ok {
		t.Error("inactive, foreign and non-monthly rows are not prices")
	}
	// The background load from the first read of the regions.
	d2 := New(WithProvider(w.cloud))
	_ = d2.Init(ctx, vendors.Config{ClusterName: "c", KeeperURL: srv.URL, Clock: clock.RealClock{}, Log: logr.Discard()})
	if _, err := d2.Regions(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d2.storagePrice("GRA11", "high-speed"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := d2.storagePrice("GRA11", "high-speed"); !ok {
		t.Error("the prices load in the background")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	k := newKeeperPrices(bad.URL, nil)
	if err := k.load(ctx); err == nil {
		t.Error("a failing Keeper is an error")
	}
	if err := New().LoadPrices(ctx); err != nil {
		t.Errorf("without Keeper: %v", err)
	}
	if p, ok := (StaticPrices{"GRA11/classic": 0.05}).StoragePrice("gra11", "Classic"); !ok || p != 0.05 {
		t.Error("static prices ignore case")
	}
}
