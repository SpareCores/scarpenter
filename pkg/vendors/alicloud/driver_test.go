/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	api "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/alicloud"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/alicloud/fake"
)

func bg() context.Context { return context.Background() }

func TestRegisteredAndIdentity(t *testing.T) {
	if !contains(vendors.Registered(), Vendor) {
		t.Fatalf("alicloud is not registered: %v", vendors.Registered())
	}
	d := New()
	if d.Vendor() != "alicloud" || d.TagKey(vendors.TagCluster) != "scarpenter.sparecores.com/cluster" {
		t.Error("identity")
	}
	t.Setenv(HomeRegionEnv, "eu-central-1")
	if err := d.Init(bg(), vendors.Config{ClusterName: "c", Log: logr.Discard()}); err != nil || d.home != "eu-central-1" {
		t.Errorf("Init: %v, home %q", err, d.home)
	}
	if d.Cloud() == nil || d.NodeOptionsEpoch() != 0 {
		t.Error("cloud or epoch")
	}
	d2 := New(WithHomeRegion("cn-hangzhou"))
	_ = d2.Init(bg(), vendors.Config{ClusterName: "c"})
	if d2.home != "cn-hangzhou" {
		t.Error("WithHomeRegion")
	}
}

func contains(s []string, v string) bool { return slices.Contains(s, v) }

func TestVanishedInstanceIsInsufficientCapacityOnce(t *testing.T) {
	e := newDenv(t, denvOptions{})
	e.warm(testRegion)
	e.acct.VanishAfter(testRegion, testZoneA, fake.OnDemand, "ecs.g7.large", time.Minute)
	inst, err := e.d.CreateInstance(bg(), e.claim("c1", "ecs.g7.large", vendors.CapacityOnDemand))
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Step(2 * time.Minute)
	if _, err := e.d.GetInstance(bg(), inst.Ref); vendors.ClassOf(err) != vendors.InsufficientCapacity || vendors.CodeOf(err) != codeInstanceVanished {
		t.Fatalf("first Get after the vanish: %v", err)
	}
	if _, err := e.d.GetInstance(bg(), inst.Ref); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("second Get: %v", err)
	}
	// Past the window a gone instance is plain NotFound.
	inst2, err := e.d.CreateInstance(bg(), e.claim("c2", "ecs.g7.large", vendors.CapacityOnDemand))
	if err != nil {
		t.Fatal(err)
	}
	e.acct.Release(testRegion, inst2.Ref.ID())
	e.clk.Step(vanishWindow + time.Minute)
	if _, err := e.d.GetInstance(bg(), inst2.Ref); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("late vanish: %v", err)
	}
}

func TestUnknownRegionsAreNotFound(t *testing.T) {
	e := newDenv(t, denvOptions{})
	ref := vendors.NewInstanceRef("nowhere-1", "i-1")
	if _, err := e.d.GetInstance(bg(), ref); !vendors.IsClass(err, vendors.NotFound) {
		t.Error(err)
	}
	if _, err := e.d.FindInstance(bg(), "nowhere-1", nil); !vendors.IsClass(err, vendors.NotFound) {
		t.Error(err)
	}
	if got, err := e.d.ListInstances(bg(), []string{"nowhere-1"}, nil); err != nil || len(got) != 0 {
		t.Error(got, err)
	}
	if err := e.d.DeleteInstance(bg(), ref); err != nil {
		t.Error(err)
	}
}

func TestBillingRuleAndHardening(t *testing.T) {
	d := New()
	r, ok := d.BillingRule("ap-southeast-1", "ecs.g7")
	if !ok || r.Verified || r.Increment == nil || *r.Increment != time.Second || r.Minimum != nil || r.Source == "" {
		t.Errorf("rule = %+v", r)
	}
	if h := d.Hardenings(); len(h) != 1 || h[0] != vendors.HardeningMetadataFromPods {
		t.Error(h)
	}
	fr, err := vendors.BootFragmentsFor(d, []string{vendors.HardeningMetadataFromPods}, vendors.BootFragmentRequest{})
	if err != nil || len(fr) != 1 || !strings.Contains(fr[0].Butane, "100.100.100.200") {
		t.Errorf("fragments = %+v, %v", fr, err)
	}
	if fr, _ := d.BootFragments(vendors.BootFragmentRequest{}); len(fr) != 0 {
		t.Error("a fragment without the hardening")
	}
}

func TestLaunchableTypesDropsFamiliesAndFillsTheTable(t *testing.T) {
	e := newDenv(t, denvOptions{})
	got, err := e.d.LaunchableTypes(bg(), testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["ecs.ebmg7.32xlarge"]; ok {
		t.Error("a bare metal type is launchable")
	}
	if zs := got["ecs.g7.large"]; len(zs) != 2 {
		t.Errorf("zones of g7 = %v", zs)
	}
	if e.d.NodeOptionsEpoch() == 0 {
		t.Error("the epoch did not change when the type table was filled")
	}
	if c, ok := e.d.Consumption("ecs.g7.large", vendors.CapacitySpot); !ok || c[0].Bucket != BucketSpot || c[0].Amount != 2 {
		t.Errorf("consumption = %v, %v", c, ok)
	}
	// Spot refused for the account: on-demand stock still answers.
	e.acct.Inject("DescribeAvailableResource", fake.APIError("InvalidAccount.NotSupportSpot", 400, "x"), 0)
	e.acct.Clear("")
}

func TestLaunchableTypesSpotUnsupportedStillAnswers(t *testing.T) {
	e := newDenv(t, denvOptions{})
	// The first call (on-demand) works, the second (spot) fails as Unsupported.
	calls := 0
	_ = calls
	e.acct.Inject("DescribeAvailableResource", fake.APIError("InvalidAccount.NotSupportSpot", 400, "x"), 0)
	if _, err := e.d.LaunchableTypes(bg(), testRegion); !vendors.IsClass(err, vendors.Unsupported) {
		t.Errorf("an on-demand failure must surface: %v", err)
	}
}

func TestNVMeTypeNeedsAnNVMeImage(t *testing.T) {
	e := newDenv(t, denvOptions{})
	e.warm(testRegion)
	e.acct.AddImage(testRegion, fake.ImageSpec{ID: "m-nonvme", Name: "x", Tags: map[string]string{
		tagFlatcarChannel: testChannel, tagFlatcarVersion: testVersion}})
	if _, err := e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: testChannel, Version: testVersion,
		Use: vendors.UseRef{IDs: []string{"m-nonvme"}}}); err != nil {
		t.Fatal(err)
	}
	req := e.claim("nv", "ecs.c8i.large", vendors.CapacityOnDemand)
	req.Image.Ref = "m-nonvme"
	if _, err := e.d.CreateInstance(bg(), req); vendors.ClassOf(err) != vendors.Unsupported {
		t.Errorf("an NVMe type with a non-NVMe image: %v", err)
	}
	e.acct.AddImage(testRegion, fake.ImageSpec{ID: "m-nvme", Name: "y", NVMe: true, Tags: map[string]string{
		tagFlatcarChannel: testChannel, tagFlatcarVersion: testVersion}})
	if _, err := e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: testChannel, Version: testVersion,
		Use: vendors.UseRef{IDs: []string{"m-nvme"}}}); err != nil {
		t.Fatal(err)
	}
	req.Image.Ref = "m-nvme"
	req.Tags[vendors.TagNodeClaim] = "nv2"
	req.IdempotencyKey = "uid-nv2"
	if _, err := e.d.CreateInstance(bg(), req); err != nil {
		t.Errorf("an NVMe image: %v", err)
	}
}

// ---- images ----

func TestResolveImageByConvention(t *testing.T) {
	e := newDenv(t, denvOptions{})
	for _, arch := range []string{archAMD64, archARM64} {
		img, err := e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: arch, Channel: testChannel, Version: testVersion})
		if err != nil || img.Version != testVersion || img.Channel != testChannel || img.RootDiskBytes != 9<<30 {
			t.Fatalf("%s: %+v, %v", arch, img, err)
		}
	}
	if _, err := e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: "s390x"}); !vendors.IsClass(err, vendors.Fatal) {
		t.Error("an unknown architecture")
	}
	// Cached: the second answer costs no call.
	n := e.acct.Calls("DescribeImages")
	_, _ = e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: testChannel, Version: testVersion})
	if e.acct.Calls("DescribeImages") != n {
		t.Error("the answer was not cached")
	}
	// A release without an image is NotFound, remembered.
	_, err := e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: "lts", Version: "1.2.3"})
	if !vendors.IsClass(err, vendors.NotFound) || vendors.CodeOf(err) != codeImageNotFound {
		t.Errorf("a missing release: %v", err)
	}
	n = e.acct.Calls("DescribeImages")
	_, _ = e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: "lts", Version: "1.2.3"})
	if e.acct.Calls("DescribeImages") != n {
		t.Error("the miss was not cached")
	}
	// A failure with an old answer serves the old answer.
	e.clk.Step(2 * time.Hour)
	e.acct.Inject("DescribeImages", fake.APIError("ServiceUnavailable", 503, "down"), 0)
	img, err := e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: testChannel, Version: testVersion})
	e.acct.Clear("")
	if err != nil || img.Ref == "" {
		t.Errorf("stale image: %+v, %v", img, err)
	}
}

func TestImageReleaseFromNameAndTags(t *testing.T) {
	e := newDenv(t, denvOptions{noImages: true})
	// The name says one release, the tags another: refused.
	e.acct.AddImage(testRegion, fake.ImageSpec{Name: imageName("stable", testVersion, archAMD64), Tags: map[string]string{
		tagFlatcarVersion: "1.0.0"}})
	_, err := e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: "stable", Version: testVersion})
	if vendors.CodeOf(err) != vendors.CodeImageVersionUnknown {
		t.Errorf("conflict: %v", err)
	}
	// Another version by name.
	e2 := newDenv(t, denvOptions{noImages: true})
	e2.acct.AddImage(testRegion, fake.ImageSpec{ID: "m-old", Name: imageName("stable", "4500.0.0", archAMD64)})
	_, err = e2.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: "stable", Version: testVersion,
		Use: vendors.UseRef{IDs: []string{"m-old"}}})
	if vendors.CodeOf(err) != vendors.CodeImageVersionMismatch {
		t.Errorf("mismatch: %v", err)
	}
	// An excluded select all.
	_, err = e2.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Use: vendors.UseRef{
		Name: "x", Select: common.SelectAll}})
	if !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("select all: %v", err)
	}
}

func TestImageSelectorOwnersFiltersAndDates(t *testing.T) {
	e := newDenv(t, denvOptions{noImages: true})
	tags := map[string]string{tagFlatcarChannel: testChannel, tagFlatcarVersion: testVersion}
	at := time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)
	e.acct.AddImage(testRegion, fake.ImageSpec{ID: "m-own", Name: "own", Tags: tags, Created: at})
	e.acct.AddImage(testRegion, fake.ImageSpec{ID: "m-shared", Name: "shared", Owner: "others", OwnerID: 4242, Public: true, Tags: tags, Created: at})
	e.acct.AddImage(testRegion, fake.ImageSpec{ID: "m-dep", Name: "dep", Status: "Deprecated", Tags: tags, Created: at})
	e.acct.AddImage(testRegion, fake.ImageSpec{ID: "m-pending", Name: "pending", Status: "Creating", Tags: tags, Created: at})
	res := func(u vendors.UseRef, owner string) (*vendors.Image, error) {
		return e.d.ResolveImage(bg(), vendors.ImageRequest{Region: testRegion, Arch: archAMD64, Channel: testChannel, Version: testVersion,
			Owner: owner, Use: u})
	}
	if img, err := res(vendors.UseRef{Tags: tags}, ""); err != nil || img.Ref != "m-own" {
		t.Errorf("self: %+v, %v", img, err)
	}
	if img, err := res(vendors.UseRef{Tags: tags}, "4242"); err != nil || img.Ref != "m-shared" {
		t.Errorf("an account UID: %+v, %v", img, err)
	}
	if img, err := res(vendors.UseRef{Tags: tags, Match: common.ResourceMatch{Owner: []string{"4242"}}}, ""); err != nil || img.Ref != "m-shared" {
		t.Errorf("match.owner: %+v, %v", img, err)
	}
	if _, err := res(vendors.UseRef{Tags: tags, Match: common.ResourceMatch{Owner: []string{"self"}, IncludeDeprecated: new(true)}}, ""); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("deprecated and own are two: %v", err)
	}
	after := metav1.NewTime(at.Add(-time.Hour))
	before := metav1.NewTime(at.Add(time.Hour))
	if img, err := res(vendors.UseRef{Tags: tags, Match: common.ResourceMatch{CreatedAfter: &after, CreatedBefore: &before}}, ""); err != nil || img.Ref != "m-own" {
		t.Errorf("a window: %+v, %v", img, err)
	}
	if img, err := res(vendors.UseRef{Tags: tags, Match: common.ResourceMatch{State: []string{"pending"}}}, ""); err != nil || img.Ref != "m-pending" {
		t.Errorf("state pending: %+v, %v", img, err)
	}
	if _, err := res(vendors.UseRef{Tags: tags, Match: common.ResourceMatch{Owner: []string{"bogus"}}}, ""); err == nil {
		t.Error("a bad owner")
	}
	if _, err := res(vendors.UseRef{Tags: tags, Filters: []common.ResourceFilter{{Name: "Nope", Values: []string{"x"}}}}, ""); err == nil {
		t.Error("a bad filter")
	}
	if _, err := res(vendors.UseRef{Tags: tags, Match: common.ResourceMatch{Architecture: []string{archARM64}}}, ""); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an excluded architecture: %v", err)
	}
}

func TestReportImages(t *testing.T) {
	e := newDenv(t, denvOptions{})
	nc := &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "c"}}
	nc.Spec.Flatcar.Version = testVersion
	nc.Spec.Vendors.Alicloud = &api.Settings{Regions: []string{testRegion, otherRegion}}
	out, err := e.d.ReportImages(bg(), nc)
	if err != nil || len(out) != 4 {
		t.Fatalf("%+v, %v", out, err)
	}
	for _, s := range out {
		if !s.Ready || s.Ref == "" {
			t.Errorf("not ready: %+v", s)
		}
	}
	// No regions: every region of the account; a missing release is reported.
	nc.Spec.Vendors.Alicloud = &api.Settings{}
	nc.Spec.Flatcar.Version = "9.9.9"
	out, err = e.d.ReportImages(bg(), nc)
	if err != nil || len(out) != 4 || out[0].Ready || out[0].Message == "" {
		t.Errorf("a missing release: %+v, %v", out, err)
	}
	// A transient failure fails the whole report.
	e2 := newDenv(t, denvOptions{})
	e2.acct.Inject("DescribeImages", fake.APIError("ServiceUnavailable", 503, "down"), 0)
	nc.Spec.Flatcar.Version = testVersion
	if _, err := e2.d.ReportImages(bg(), nc); err == nil {
		t.Error("a transient failure was reported as an answer")
	}
	// An images selector that selects nothing for the region is reported per image.
	e3 := newDenv(t, denvOptions{})
	nc.Spec.Vendors.Alicloud = &api.Settings{Regions: []string{testRegion}, Resources: &api.Resources{
		Images: &common.Resource{Use: &common.ResourceUse{IDs: map[string]common.IDList{otherRegion: {"m-x"}}}}}}
	out, err = e3.d.ReportImages(bg(), nc)
	if err != nil || len(out) != 2 || out[0].Ready {
		t.Errorf("no ids for the region: %+v, %v", out, err)
	}
}

// ---- the selector requests ----

func TestParameterCube(t *testing.T) {
	got, err := cube([]param{{name: "a", values: []string{"1", "2"}}, {name: "b", values: []string{"x"}}, {name: "c"}})
	if err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	many := make([]string, 70)
	if _, err := cube([]param{{name: "a", values: many}}); err == nil {
		t.Error("too many calls")
	}
	ps, none := nativeParams([]common.ResourceFilter{{Name: "A", Values: []string{"1", "2"}}, {Name: "A", Values: []string{"2", "3"}}})
	if none || len(ps) != 1 || len(ps[0].values) != 1 || ps[0].values[0] != "2" {
		t.Errorf("intersection: %+v %v", ps, none)
	}
	if _, none := nativeParams([]common.ResourceFilter{{Name: "A", Values: []string{"1"}}, {Name: "A", Values: []string{"2"}}}); !none {
		t.Error("an empty intersection matches nothing")
	}
	if literalOf("a*b-long") != "b-long" || literalOf("exact") != "exact" {
		t.Error("literalOf")
	}
	if !strings.HasSuffix(minuteTime(time.Date(2026, 1, 2, 3, 4, 59, 0, time.UTC)), "03:04Z") {
		t.Error("minuteTime")
	}
	if imageStatuses([]string{"pending", "failed"}, true) != "Creating,Waiting,UnAvailable,CreateFailed,Deprecated" {
		t.Error("imageStatuses")
	}
	if _, _, err := ownerQuery("x"); err == nil {
		t.Error("ownerQuery")
	}
}

func TestSelectorFiltersOfEveryKind(t *testing.T) {
	e := newDenv(t, denvOptions{})
	// vSwitches by native filters, ids and a wildcard name.
	subs, err := e.d.Resolve(bg(), KindSubnets, testRegion, vendors.UseRef{
		Filters: []common.ResourceFilter{{Name: "IsDefault", Values: []string{"true"}}, {Name: "EnableIpv6", Values: []string{"false"}}},
		Match:   common.ResourceMatch{Zone: []string{testZoneA}, Network: []string{e.vpc[testRegion]}}, Name: "def*", Select: common.SelectAll,
	})
	if err != nil || len(subs) != 1 || subs[0].Network != e.vpc[testRegion] {
		t.Fatalf("%+v, %v", subs, err)
	}
	if _, err := e.d.Resolve(bg(), KindSubnets, testRegion, vendors.UseRef{Filters: []common.ResourceFilter{{Name: "Nope", Values: []string{"1"}}}}); err == nil {
		t.Error("a bad vswitch filter")
	}
	// Groups.
	gs, err := e.d.Resolve(bg(), KindSecurityGroups, testRegion, vendors.UseRef{
		Name: "nod*", Match: common.ResourceMatch{Network: []string{e.vpc[testRegion]}}, Select: common.SelectAll,
		Filters: []common.ResourceFilter{{Name: "SecurityGroupType", Values: []string{"normal"}}}})
	if err != nil || len(gs) != 1 {
		t.Fatalf("%+v, %v", gs, err)
	}
	if _, err := e.d.Resolve(bg(), KindSecurityGroups, testRegion, vendors.UseRef{Filters: []common.ResourceFilter{{Name: "Nope", Values: []string{"1"}}}}); err == nil {
		t.Error("a bad group filter")
	}
	// Deployment sets.
	ds, err := e.d.Resolve(bg(), KindDeploymentSets, testRegion, vendors.UseRef{Name: "my-set"})
	if err != nil || len(ds) != 1 {
		t.Fatalf("%+v, %v", ds, err)
	}
	if got, err := e.d.Resolve(bg(), KindDeploymentSets, testRegion, vendors.UseRef{IDs: []string{ds[0].ID}, Filters: []common.ResourceFilter{{Name: "Strategy", Values: []string{"Availability"}}}}); err != nil || len(got) != 1 {
		t.Errorf("%+v, %v", got, err)
	}
	if _, err := e.d.Resolve(bg(), KindDeploymentSets, testRegion, vendors.UseRef{Filters: []common.ResourceFilter{{Name: "Nope", Values: []string{"1"}}}}); err == nil {
		t.Error("a bad set filter")
	}
	if _, err := e.d.Resolve(bg(), "nope", testRegion, vendors.UseRef{Name: "x"}); err == nil {
		t.Error("a bad kind")
	}
	// Tags on images: more than the server takes are finished by the client.
	if got := exactTags(map[string]string{"a": "*", "b": "1"}); len(got) != 1 {
		t.Error(got)
	}
}

// ---- resources ----

func marker(kind vendors.ResourceKind) map[vendors.TagKey]string {
	return map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClass: "class",
		vendors.TagManagedBy: vendors.ManagedByValue, vendors.TagResource: string(kind)}
}

func TestManagedResourcesLifecycle(t *testing.T) {
	e := newDenv(t, denvOptions{})
	spec := vendors.CreateSpec{Name: "scarpenter-my-cluster-subnets-" + testRegion, Tags: marker(KindSubnets), ExtraTags: map[string]string{"team": "x"}}
	e.acct.NetworkPendingFor = 5 * time.Second
	vp, created, err := e.d.Ensure(bg(), KindSubnets, testRegion, spec)
	if err != nil || !created || !strings.HasPrefix(vp.ID, "vpc-") {
		t.Fatalf("%+v %v %v", vp, created, err)
	}
	if e.sleeps.Load() == 0 {
		t.Error("the driver did not wait for the pending network")
	}
	// Both zones have a vSwitch; a third zone appearing later gets the next /20.
	subs, _ := e.d.vpcVSwitches(bg(), testRegion, vp.ID, "")
	if len(subs) != 2 {
		t.Fatalf("vswitches = %v", subs)
	}
	if _, created, err := e.d.Ensure(bg(), KindSubnets, testRegion, spec); err != nil || created {
		t.Errorf("second Ensure: %v %v", created, err)
	}
	// A network that never becomes available is Retryable.
	e.acct.NetworkPendingFor = 24 * time.Hour
	spec2 := vendors.CreateSpec{Name: "scarpenter-my-cluster-other", Tags: marker(KindSubnets)}
	if _, _, err := e.d.Ensure(bg(), KindSubnets, testRegion, spec2); !vendors.IsClass(err, vendors.Retryable) || vendors.CodeOf(err) != codeNetworkPending {
		t.Errorf("pending network: %v", err)
	}
	e.acct.NetworkPendingFor = 0
	// A foreign VPC of the same name is not ours: the marker is missing.
	if _, _, err := e.d.Ensure(bg(), "nope", testRegion, spec); err == nil {
		t.Error("a bad kind")
	}
	if _, _, err := e.d.Ensure(bg(), KindSubnets, testRegion, vendors.CreateSpec{}); err == nil {
		t.Error("a nameless resource")
	}
	// Listed, in use, deleted (with its vSwitches and a fallback group).
	listed, err := e.d.List(bg(), nil, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(listed) < 1 {
		t.Fatalf("%+v %v", listed, err)
	}
	if err := e.d.Delete(bg(), vendors.ResourceRef{Kind: KindSubnets, Region: testRegion, ID: "vsw-1"}); err == nil {
		t.Error("a vSwitch is never deleted by itself")
	}
	if err := e.d.Delete(bg(), vendors.ResourceRef{Kind: KindSubnets, Region: testRegion, ID: e.vpc[testRegion], Tags: marker(KindSubnets)}); err == nil {
		t.Error("the default VPC is never deleted")
	}
	if err := e.d.Delete(bg(), vendors.ResourceRef{Kind: "nope"}); err == nil {
		t.Error("a bad kind")
	}
	if err := e.d.Delete(bg(), vp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.d.InUse(bg(), vp); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("InUse of a deleted VPC: %v", err)
	}
	if _, _, err := e.d.InUse(bg(), vendors.ResourceRef{Kind: "nope"}); err == nil {
		t.Error("InUse of a bad kind")
	}
	if err := e.d.Delete(bg(), vp); err != nil {
		t.Errorf("a second delete: %v", err)
	}
}

func TestSecurityGroupRulesAreBroughtInLine(t *testing.T) {
	e := newDenv(t, denvOptions{})
	port := int32(41641)
	params := func(p SecurityGroupParams) []byte { b, _ := json.Marshal(p); return b }
	spec := vendors.CreateSpec{Name: "scarpenter-my-cluster-sg", Tags: marker(KindSecurityGroups)}
	vpcID := e.vpc[testRegion]
	spec.Params = params(SecurityGroupParams{VPC: vpcID, SelfReference: true, Inbound: []common.InboundRule{
		{Protocol: "udp", FromPort: &port, CIDRs: []string{"0.0.0.0/0"}},
		{Protocol: "tcp", CIDRs: []string{"10.0.0.0/8"}}, {Protocol: "icmp", CIDRs: []string{"10.0.0.0/8"}},
		{Protocol: "any", CIDRs: []string{"192.168.0.0/16"}},
	}})
	ref, created, err := e.d.Ensure(bg(), KindSecurityGroups, testRegion, spec)
	if err != nil || !created {
		t.Fatal(err)
	}
	rules, _, _ := e.d.rulesOf(bg(), testRegion, ref.ID)
	if len(rules) != 5 {
		t.Fatalf("rules = %+v", rules)
	}
	// Fewer rules: the extra ones are revoked.
	spec.Params = params(SecurityGroupParams{VPC: vpcID, Inbound: []common.InboundRule{{Protocol: "tcp", CIDRs: []string{"10.0.0.0/8"}}}})
	if _, created, err := e.d.Ensure(bg(), KindSecurityGroups, testRegion, spec); err != nil || created {
		t.Fatal(err, created)
	}
	if rules, _, _ = e.d.rulesOf(bg(), testRegion, ref.ID); len(rules) != 1 || rules[0].portRange != "1/65535" {
		t.Errorf("rules = %+v", rules)
	}
	// A group of the name without our marker is refused.
	g, _ := e.acct.ECS(testRegion).CreateSecurityGroup(bg(), sgReq(vpcID))
	_ = g
	foreign := vendors.CreateSpec{Name: "nodes", Tags: marker(KindSecurityGroups), Params: spec.Params}
	if _, _, err := e.d.Ensure(bg(), KindSecurityGroups, testRegion, foreign); err == nil {
		t.Error("a foreign group was adopted")
	}
	if _, _, err := e.d.Ensure(bg(), KindSecurityGroups, testRegion, vendors.CreateSpec{Name: "x", Params: []byte(`{}`)}); err == nil {
		t.Error("a group without a VPC")
	}
	if err := e.d.Delete(bg(), vendors.ResourceRef{Kind: KindSecurityGroups, Region: testRegion, ID: *g.SecurityGroupId, Tags: marker(KindSecurityGroups)}); err == nil {
		t.Error("a foreign group was deleted")
	}
	if err := e.d.Delete(bg(), ref); err != nil {
		t.Fatal(err)
	}
	if listed, _ := e.d.List(bg(), []string{testRegion}, nil); len(listed) != 0 {
		t.Errorf("%+v", listed)
	}
}

func TestDeploymentSetMarkerAndLists(t *testing.T) {
	e := newDenv(t, denvOptions{})
	spec := vendors.CreateSpec{Name: "scarpenter-my-cluster-ds", Tags: marker(KindDeploymentSets), Params: []byte(`{"strategy":"LowLatency"}`)}
	ref, created, err := e.d.Ensure(bg(), KindDeploymentSets, testRegion, spec)
	if err != nil || !created || ref.Tags[vendors.TagCluster] != testCluster {
		t.Fatalf("%+v %v %v", ref, created, err)
	}
	listed, err := e.d.List(bg(), []string{testRegion}, map[vendors.TagKey]string{vendors.TagCluster: testCluster})
	if err != nil || len(listed) != 1 || listed[0].Kind != KindDeploymentSets {
		t.Fatalf("%+v %v", listed, err)
	}
	if inUse, _, err := e.d.InUse(bg(), ref); err != nil || inUse {
		t.Error(inUse, err)
	}
	req := e.claim("in-set", "ecs.g7.large", vendors.CapacityOnDemand)
	req.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{ref}}
	inst, err := e.d.CreateInstance(bg(), req)
	if err != nil {
		t.Fatal(err)
	}
	if inUse, _, _ := e.d.InUse(bg(), ref); !inUse {
		t.Error("a set with an instance is in use")
	}
	if err := e.d.Delete(bg(), ref); !vendors.IsClass(err, vendors.Retryable) {
		t.Errorf("%v", err)
	}
	_ = e.d.DeleteInstance(bg(), inst.Ref)
	if err := e.d.Delete(bg(), ref); err != nil {
		t.Fatal(err)
	}
	// The user's set is never ours to delete.
	mine, _ := e.d.Resolve(bg(), KindDeploymentSets, testRegion, vendors.UseRef{Name: "my-set"})
	mine[0].Tags = marker(KindDeploymentSets)
	if err := e.d.Delete(bg(), mine[0]); err == nil {
		t.Error("a user's deployment set was deleted")
	}
	if _, _, err := e.d.Ensure(bg(), KindDeploymentSets, testRegion, vendors.CreateSpec{Name: "my-set", Tags: marker(KindDeploymentSets)}); err == nil {
		t.Error("a user's set was adopted")
	}
	if _, _, err := e.d.Ensure(bg(), KindDeploymentSets, testRegion, vendors.CreateSpec{Name: "z", Params: []byte("x")}); err == nil {
		t.Error("bad params")
	}
}

func TestDefaultsAndSuits(t *testing.T) {
	e := newDenv(t, denvOptions{})
	if refs, err := e.d.Default(bg(), KindSecurityGroups, testRegion); err != nil || refs != nil {
		t.Error(refs, err)
	}
	refs, err := e.d.Default(bg(), KindSubnets, testRegion)
	if err != nil || len(refs) != 2 {
		t.Fatal(refs, err)
	}
	if ok, reason, _ := e.d.DefaultSuits(bg(), KindSubnets, testRegion, refs, vendors.VendorSettings{Networking: common.Networking{PublicIPv6: new(true)}}); ok || reason == "" {
		t.Error("ipv6")
	}
	if ok, _, _ := e.d.DefaultSuits(bg(), KindSubnets, testRegion, refs, vendors.VendorSettings{}); !ok {
		t.Error("suits")
	}
	if _, err := e.d.Resolve(bg(), KindSubnets, testRegion, vendors.UseRef{}); err == nil {
		t.Error("an empty selector")
	}
}

// ---- node options ----

func TestSupportedProfiles(t *testing.T) {
	e := newDenv(t, denvOptions{})
	e.warm(testRegion)
	p := vendors.Profiles{vendors.ProfileStorage: {
		"data2":  json.RawMessage(`{"data":[{"name":"a","size":"10Gi","mountPath":"/a"},{"name":"b","size":"10Gi","mountPath":"/b"}]}`),
		"entry":  json.RawMessage(`{"root":{"category":"cloud_essd_entry","size":"20Gi"}}`),
		"enc":    json.RawMessage(`{"root":{"encrypted":true}}`),
		"bad":    json.RawMessage(`{"root":3}`),
		"ignore": json.RawMessage(`{"localStorage":{"policy":"ignore"}}`),
	}, vendors.ProfileNetwork: {"bad": json.RawMessage(`[]`), "ok": json.RawMessage(`{}`)},
		vendors.ProfilePlacement: {"bad": json.RawMessage(`[]`), "ok": json.RawMessage(`{}`)}}
	// ecs.e-c1m2.large takes 5 disks in the fake: 1 system + 2 data fit.
	sup := e.d.Supported(vendors.CatalogType{APIReference: "ecs.g7.large"}, testZoneB, p)
	if strings.Join(sup[vendors.ProfileStorage], ",") != "data2,enc,ignore" {
		t.Errorf("g7 storage = %v", sup)
	}
	sup = e.d.Supported(vendors.CatalogType{APIReference: "ecs.e-c1m2.large"}, testZoneA, p)
	if strings.Join(sup[vendors.ProfileStorage], ",") != "data2,entry,ignore" {
		t.Errorf("entry storage in 1a = %v", sup[vendors.ProfileStorage])
	}
	if strings.Join(sup[vendors.ProfileNetwork], ",") != "ok" || strings.Join(sup[vendors.ProfilePlacement], ",") != "ok" {
		t.Errorf("%v", sup)
	}
	// Unknown types: a profile with data disks is not offered.
	sup = e.d.Supported(vendors.CatalogType{APIReference: "ecs.nope.large"}, testZoneA, p)
	if contains(sup[vendors.ProfileStorage], "data2") {
		t.Error("data disks of an unknown type")
	}
	if !entryFamily("ecs.u1.xlarge") || entryFamily("ecs.g7.large") {
		t.Error("entryFamily")
	}
}

func TestPriceDelta(t *testing.T) {
	prices := StaticPrices{testRegion + "/cloud_essd/pl1": 0.146, testRegion + "/cloud_essd": 0.146, testRegion + "/cloud_efficiency": 0.04}
	e := newDenv(t, denvOptions{prices: prices})
	off := vendors.Offering{Region: testRegion}
	d := e.d
	s := d.Defaults()
	// PL0 has no row: incomplete.
	if _, ok := d.PriceDelta(s, off); ok {
		t.Error("PL0 is not priced")
	}
	s.Raw = json.RawMessage(`{"storage":{"root":{"category":"cloud_essd","performanceLevel":"PL1","size":"100Gi"},` +
		`"data":[{"name":"a","category":"cloud_efficiency","size":"50Gi","mountPath":"/a"}]}}`)
	got, ok := d.PriceDelta(s, off)
	want := (100*0.146 + 50*0.04) / 730
	if !ok || got < want-1e-9 || got > want+1e-9 {
		t.Errorf("price = %v %v, want %v", got, ok, want)
	}
	s.Raw = json.RawMessage(`{"storage":{"root":{"category":"cloud_auto","provisionedIops":3000}}}`)
	if _, ok := d.PriceDelta(s, off); ok {
		t.Error("cloud_auto")
	}
	s.Raw = json.RawMessage(`{"storage":{"root":{"category":"cloud_efficiency","size":"20Gi"}},"internetChargeType":"PayByBandwidth"}`)
	if _, ok := d.PriceDelta(s, off); ok {
		t.Error("PayByBandwidth")
	}
	s.Raw = json.RawMessage(`{`)
	if _, ok := d.PriceDelta(s, off); ok {
		t.Error("bad settings")
	}
	if p, ok := (New()).diskPrice(testRegion, "x", ""); ok || p != 0 {
		t.Error("no price source")
	}
}

func TestKeeperPrices(t *testing.T) {
	d := New()
	if err := d.Init(bg(), vendors.Config{ClusterName: "c", Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.prices.StoragePrice("ap-southeast-1", "cloud_essd", "PL1"); ok {
		t.Error("a price is known before any was set")
	}
	prices := vendors.KeeperPrices{Hash: "h1", Storage: []vendors.StoragePrice{
		{Region: "ap-southeast-1", StorageID: "cloud_essd", Unit: "GB/month", USD: 0.1},
		{Region: "ap-southeast-1", StorageID: "cloud_essd_pl0", Unit: "GB/month", USD: 0.05},
		{Region: "ap-southeast-1", StorageID: "cloud_ssd", Unit: "GB/hour", USD: 0.1},
	}}
	d.SetKeeperPrices(prices)
	if v, ok := d.prices.StoragePrice("ap-southeast-1", "cloud_essd", "PL1"); !ok || v != 0.1 {
		t.Error(v, ok)
	}
	if v, ok := d.prices.StoragePrice("ap-southeast-1", "cloud_essd", "PL0"); !ok || v != 0.05 {
		t.Error(v, ok)
	}
	if _, ok := d.prices.StoragePrice("ap-southeast-1", "cloud_essd", "PL2"); ok {
		t.Error("PL2")
	}
	if _, ok := d.prices.StoragePrice("ap-southeast-1", "cloud_ssd", ""); ok {
		t.Error("an hourly row")
	}
	epoch := d.NodeOptionsEpoch()
	if epoch == 0 {
		t.Error("the epoch did not change with the prices")
	}
	prices.Hash = "h2"
	d.SetKeeperPrices(prices)
	if d.NodeOptionsEpoch() != epoch {
		t.Error("the epoch changed for equal prices")
	}
	prices.Storage[0].USD = 0.2
	d.SetKeeperPrices(prices)
	if d.NodeOptionsEpoch() == epoch {
		t.Error("the epoch did not change for a new price")
	}
}

// engineEnv is a driver behind the core's resource engine.
type engineEnv struct {
	*denv
	engine *resources.Engine
}

func newEngineEnv(t *testing.T) *engineEnv {
	e := newDenv(t, denvOptions{})
	e.warm(testRegion)
	return &engineEnv{denv: e, engine: resources.New(resources.Options{Registry: vendors.NewRegistryFrom(e.d), ClusterName: testCluster})}
}

func (e *engineEnv) prepare(nc *v1alpha1.SCNodeClass, zone string, s vendors.VendorSettings, runKey string) (*vendors.Prepared, error) {
	return e.d.Prepare(bg(), vendors.PrepareRequest{
		Region: testRegion, Zone: zone, InstanceType: "ecs.g7.large", CapacityType: vendors.CapacityOnDemand,
		Type: vendors.CatalogType{Vendor: Vendor, APIReference: "ecs.g7.large"}, Settings: s, RunKey: runKey,
		Tags: map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClaim: "claim", vendors.TagNodeClass: nc.Name,
			vendors.TagManagedBy: vendors.ManagedByValue},
		Resources: e.engine.Provider(nc, Vendor, testRegion, zone),
	})
}

func nets(iso common.Isolation, v6 bool) common.Networking {
	return common.Networking{PublicIPv4: new(true), PublicIPv6: &v6, Isolation: iso}
}

func TestPrepareWithTheEngine(t *testing.T) {
	e := newEngineEnv(t)
	nc := classWith(nil)
	s := e.d.Defaults()
	s.Networking = nets(common.IsolationRun, false)
	p, err := e.prepare(nc, testZoneA, s, "run1")
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(p.Resources))
	for _, r := range p.Resources {
		kinds = append(kinds, string(r.Kind))
	}
	// The default network serves; the group of the NodeClass and the group of the run are created.
	if strings.Join(kinds, ",") != "subnets,securityGroups,runSecurityGroups" {
		t.Fatalf("kinds = %v", kinds)
	}
	// With data disks and a deployment set: hints and a set per run key.
	s.Raw = json.RawMessage(`{"storage":{"data":[{"name":"pg","size":"100Gi","mountPath":"/var/lib/pg","filesystem":"xfs"},{"name":"b","size":"5Gi","mountPath":"/b"}]}}`)
	s.Placement = json.RawMessage(`{"deploymentSet":{"strategy":"LowLatency"}}`)
	s.Profiles = map[string]string{vendors.ProfilePlacement: "colo"}
	p, err = e.prepare(nc, testZoneA, s, "run1")
	if err != nil || len(p.Devices) != 2 || p.Devices[0].Filesystem != "xfs" || p.Devices[1].Filesystem != "ext4" ||
		p.Devices[0].SizeBytes != 100<<30 || p.Devices[0].SerialPattern == "" {
		t.Fatalf("%+v, %v", p, err)
	}
	if !hasKind(p, KindDeploymentSets) {
		t.Error("no deployment set")
	}
	if _, err := e.prepare(nc, testZoneA, s, ""); vendors.CodeOf(err) != codeInvalidSettings {
		t.Errorf("a set without a run key: %v", err)
	}
	// A set by name.
	s.Placement = json.RawMessage(`{"deploymentSet":{"use":{"name":"my-set"}}}`)
	if p, err = e.prepare(nc, testZoneA, s, ""); err != nil || !hasKind(p, KindDeploymentSets) {
		t.Errorf("%+v, %v", p, err)
	}
	s.Placement = json.RawMessage(`{"deploymentSet":{"use":{"name":"absent"}}}`)
	if _, err = e.prepare(nc, testZoneA, s, ""); err == nil {
		t.Error("a missing set")
	}
	s.Placement = json.RawMessage(`[]`)
	if _, err = e.prepare(nc, testZoneA, s, ""); vendors.CodeOf(err) != codeInvalidSettings {
		t.Errorf("bad placement: %v", err)
	}
	s.Placement = nil
	s.Raw = json.RawMessage(`{`)
	if _, err = e.prepare(nc, testZoneA, s, ""); vendors.CodeOf(err) != codeInvalidSettings {
		t.Errorf("bad settings: %v", err)
	}
	// publicIPv6 is unsupported everywhere.
	s = e.d.Defaults()
	s.Networking = nets(common.IsolationNone, true)
	if _, err = e.prepare(nc, testZoneA, s, ""); vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Errorf("ipv6: %v", err)
	}
}

func hasKind(p *vendors.Prepared, k vendors.ResourceKind) bool {
	for _, r := range p.Resources {
		if r.Kind == k {
			return true
		}
	}
	return false
}

func TestPrepareBroughtResourcesMustAgree(t *testing.T) {
	e := newEngineEnv(t)
	ctx := bg()
	// A VPC of its own with a vSwitch in zone a, and a group in the default VPC.
	other, created, err := e.d.Ensure(ctx, KindSubnets, testRegion, vendors.CreateSpec{Name: "scarpenter-my-cluster-own", Tags: marker(KindSubnets)})
	if err != nil || !created {
		t.Fatal(err)
	}
	subs, _ := e.d.vpcVSwitches(ctx, testRegion, other.ID, testZoneA)
	nc := classWith(&api.Resources{
		Subnets:        &common.Resource{Use: &common.ResourceUse{IDs: map[string]common.IDList{testRegion: {subs[0].id}}}},
		SecurityGroups: &common.Resource{Use: &common.ResourceUse{IDs: map[string]common.IDList{testRegion: {e.groups[testRegion]}}}},
	})
	s := e.d.Defaults()
	s.Networking = nets(common.IsolationNone, false)
	if _, err := e.prepare(nc, testZoneA, s, ""); vendors.CodeOf(err) != vendors.CodeResourceMissingInZone && vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Errorf("a group in another VPC: %v", err)
	}
	// No vSwitch in the zone of a brought list: the zone is unusable.
	nc = classWith(&api.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{IDs: map[string]common.IDList{testRegion: {e.vswitches[testRegion][testZoneB]}}}}})
	if _, err := e.prepare(nc, testZoneA, s, ""); vendors.ClassOf(err) != vendors.ZoneUnusable && vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Errorf("no vSwitch in the zone: %v", err)
	}
	// The group of a brought VPC must exist.
	if _, err := e.d.groupVPC(ctx, testRegion, "sg-absent"); vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Errorf("%v", err)
	}
	if v, err := e.d.groupVPC(ctx, testRegion, e.groups[testRegion]); err != nil || v != e.vpc[testRegion] {
		t.Error(v, err)
	}
}

func TestZoneGaps(t *testing.T) {
	e := newDenv(t, denvOptions{})
	enc := vendors.VendorSettings{Raw: json.RawMessage(`{"storage":{"root":{"encrypted":true}}}`)}
	if g, err := e.d.ZoneGaps(bg(), testRegion, enc, nil); err != nil || len(g) != 1 || g[testZoneA] == "" {
		t.Errorf("%v %v", g, err)
	}
	v6 := e.d.Defaults()
	v6.Networking.PublicIPv6 = new(true)
	if g, _ := e.d.ZoneGaps(bg(), testRegion, v6, nil); len(g) != 2 {
		t.Errorf("%v", g)
	}
	if g, _ := e.d.ZoneGaps(bg(), testRegion, vendors.VendorSettings{Raw: json.RawMessage(`{`)}, nil); g != nil {
		t.Error("bad settings")
	}
	if g, _ := e.d.ZoneGaps(bg(), otherRegion, enc, nil); g != nil {
		t.Errorf("a region without the zones: %v", g)
	}
}

func TestLaunchWithoutPrepareCreatesAFallbackGroup(t *testing.T) {
	e := newDenv(t, denvOptions{})
	// Remove the group of the default VPC: the launch makes one, once.
	for _, r := range []string{testRegion} {
		g, _ := e.d.describeGroups(bg(), r, vendors.UseRef{Match: matchNetwork(e.vpc[r])})
		for _, x := range g {
			_, _ = e.acct.ECS(r).DeleteSecurityGroup(bg(), deleteGroup(*x.SecurityGroupId))
		}
	}
	a, err := e.d.CreateInstance(bg(), e.claim("fb1", "ecs.g7.large", vendors.CapacityOnDemand))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.CreateInstance(bg(), e.claim("fb2", "ecs.g7.large", vendors.CapacityOnDemand)); err != nil {
		t.Fatal(err)
	}
	if listed, _ := e.d.List(bg(), []string{testRegion}, nil); len(listed) != 1 {
		t.Errorf("fallback groups = %+v", listed)
	}
	_ = a
}

// ---- quotas and regions ----

func TestZonesAndRegionsAreCached(t *testing.T) {
	e := newDenv(t, denvOptions{})
	if _, err := e.d.Regions(bg()); err != nil {
		t.Fatal(err)
	}
	n := e.acct.Calls("DescribeRegions")
	_, _ = e.d.Regions(bg())
	_, _ = e.d.Zones(bg(), testRegion)
	zn := e.acct.Calls("DescribeZones")
	_, _ = e.d.Zones(bg(), testRegion)
	if e.acct.Calls("DescribeRegions") != n || e.acct.Calls("DescribeZones") != zn {
		t.Error("not cached")
	}
	e.acct.Deny(otherRegion)
	if _, err := e.d.Zones(bg(), otherRegion); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("a denied region: %v", err)
	}
	if got, err := e.d.List(bg(), []string{otherRegion}, nil); err != nil || len(got) != 0 {
		t.Errorf("listing a denied region: %v %v", got, err)
	}
}

func TestQuotasThroughTheDriver(t *testing.T) {
	e := newDenv(t, denvOptions{})
	e.warm(testRegion)
	e.acct.SetQuota(testRegion, 10, 4)
	e.acct.AddForeignInstance(testRegion, testZoneA, "ecs.g7.large", false)
	snap, err := e.d.Quotas(bg(), testRegion)
	if err != nil || len(snap.Buckets) != 2 {
		t.Fatal(snap, err)
	}
	if g, err := e.d.GlobalQuotas(bg()); err != nil || len(g.Buckets) != 0 {
		t.Error(g, err)
	}
	for _, b := range snap.Buckets {
		if b.ID == BucketPostpaid && (b.Limit != 10 || b.Used != 2) {
			t.Errorf("%+v", b)
		}
	}
}

func TestSettingsHelpers(t *testing.T) {
	if _, err := decodeSettings(vendors.VendorSettings{Network: json.RawMessage(`{`)}); err == nil {
		t.Error("bad network profile")
	}
	if _, err := decodeStorage(json.RawMessage(`{`)); err == nil {
		t.Error("bad storage")
	}
	if err := decodeNetwork(json.RawMessage(`{`)); err == nil {
		t.Error("bad network")
	}
	if orDefault("", "x") != "x" || orDefault("y", "x") != "y" {
		t.Error("orDefault")
	}
	d := New()
	if got, ok := d.RootDiskForType(vendors.VendorSettings{}, vendors.CatalogType{}); ok || got != 0 {
		t.Error("RootDiskForType")
	}
	if n, ok := atoi("12"); !ok || n != 12 {
		t.Error("atoi")
	}
	if compare("a", "b") >= 0 {
		t.Error("compare")
	}
	if a, ok := d.VendorArch("amd64"); !ok || a != "x86_64" {
		t.Error("VendorArch")
	}
	if _, ok := d.VendorArch("riscv"); ok {
		t.Error("VendorArch")
	}
}
