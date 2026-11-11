/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vultr_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/vultr/govultr/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	vultrapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr"
	"github.com/SpareCores/scarpenter/pkg/vendors/vultr/fake"
)

func TestInitNeedsTheKeyAndTheClusterName(t *testing.T) {
	t.Setenv(vultr.APIKeyEnv, "")
	if err := vultr.New().Init(context.Background(), vendors.Config{ClusterName: "c"}); err == nil ||
		!strings.Contains(err.Error(), vultr.APIKeyEnv) {
		t.Fatalf("no key: %v", err)
	}
	if err := vultr.New(vultr.WithAPI(fake.New())).Init(context.Background(), vendors.Config{}); err == nil {
		t.Fatal("no cluster name was accepted: the driver would delete nothing")
	}
	t.Setenv(vultr.APIKeyEnv, "a-key")
	t.Setenv(vultr.AvailabilityTTLEnv, "soon")
	if err := vultr.New().Init(context.Background(), vendors.Config{ClusterName: "c"}); err == nil {
		t.Fatal("a bad TTL was accepted")
	}
	t.Setenv(vultr.AvailabilityTTLEnv, "30s")
	d := vultr.New()
	if err := d.Init(context.Background(), vendors.Config{ClusterName: "c", Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	if d.Vendor() != "vultr" || d.TagKey(vendors.TagCluster) != "scarpenter.sparecores.com/cluster" {
		t.Fatalf("identity: %s %s", d.Vendor(), d.TagKey(vendors.TagCluster))
	}
}

func TestAnUninitialisedDriverFailsCleanly(t *testing.T) {
	d := vultr.New()
	ctx := context.Background()
	if _, err := d.Regions(ctx); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("Regions: %v", err)
	}
	if _, err := d.GetInstance(ctx, "ewr/x"); !vendors.IsClass(err, vendors.Fatal) {
		t.Fatalf("GetInstance: %v", err)
	}
	if got := d.TagKey(vendors.TagUse); got == "" {
		t.Fatal("TagKey needs no initialisation")
	}
}

func TestRegionsAndZones(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	regions, err := e.d.Regions(ctx)
	if err != nil || len(regions) != 2 || regions[0].Name != "ewr" || regions[1].Name != "fra" {
		t.Fatalf("regions: %v, %v", regions, err)
	}
	calls := e.acct.Calls("ListRegions")
	if _, err := e.d.Regions(ctx); err != nil || e.acct.Calls("ListRegions") != calls {
		t.Fatalf("the second call listed again: %d", e.acct.Calls("ListRegions"))
	}
	e.clk.Step(2 * time.Hour)
	if _, err := e.d.Regions(ctx); err != nil || e.acct.Calls("ListRegions") != calls+1 {
		t.Fatal("the cache does not expire")
	}
	zones, err := e.d.Zones(ctx, "ewr")
	if err != nil || len(zones) != 1 || zones[0].ID != "ewr" || zones[0].Name != "ewr" {
		t.Fatalf("zones: %v, %v", zones, err)
	}
	if zones, err = e.d.Zones(ctx, "nowhere"); err != nil || len(zones) != 0 {
		t.Fatalf("a region the API does not list has zones: %v, %v", zones, err)
	}
	e.acct.FailNext("ListRegions", 1, &fake.Error{Status: 503, Message: "down"})
	e.clk.Step(2 * time.Hour)
	if _, err := e.d.Zones(ctx, "ewr"); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("a failing list: %v", err)
	}
}

func TestLaunchableTypesAreTheZonesOfTheRegion(t *testing.T) {
	e := newDrvEnv(t)
	got, err := e.d.LaunchableTypes(context.Background(), "ewr")
	if err != nil {
		t.Fatal(err)
	}
	// vx1-g-2c-8g is diskless and vcg is a GPU plan: neither is launched.
	if len(got) != 3 || len(got[testType]) != 1 || got[testType][0] != "ewr" || got["vx1-g-2c-8g"] != nil {
		t.Fatalf("got %v", got)
	}
	if _, err := e.d.LaunchableTypes(context.Background(), "nowhere"); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Fatalf("an unknown region: %v", err)
	}
}

// ---- images ----

func imageReq(use vendors.UseRef) vendors.ImageRequest {
	return vendors.ImageRequest{Region: "ewr", Arch: "amd64", Channel: "stable", Version: "4081.2.0", Use: use}
}

func TestResolveImageByTheDefaultDescription(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	img, err := e.d.ResolveImage(ctx, imageReq(vendors.UseRef{}))
	if err != nil || img.Ref != e.snap || img.Channel != "stable" || img.Version != "4081.2.0" {
		t.Fatalf("got %+v, %v", img, err)
	}
	// The answer is cached: the lookup is not repeated.
	calls := e.acct.Calls("ListSnapshots")
	if _, err := e.d.ResolveImage(ctx, imageReq(vendors.UseRef{})); err != nil || e.acct.Calls("ListSnapshots") != calls {
		t.Fatalf("not cached: %v, %d calls", err, e.acct.Calls("ListSnapshots"))
	}
	// Another release, another snapshot.
	req := imageReq(vendors.UseRef{})
	req.Version = "9999.0.0"
	if _, err := e.d.ResolveImage(ctx, req); !vendors.IsClass(err, vendors.NotFound) || vendors.CodeOf(err) != "ImageNotFound" {
		t.Fatalf("none: %v", err)
	}
	// arm64: Vultr has none.
	req = imageReq(vendors.UseRef{})
	req.Arch = "arm64"
	if _, err := e.d.ResolveImage(ctx, req); !vendors.IsClass(err, vendors.Unsupported) {
		t.Fatalf("arm64: %v", err)
	}
}

func TestResolveImageExactDescriptionWhateverTheServerFilterDoes(t *testing.T) {
	e := newDrvEnv(t)
	e.acct.SubstringDescriptionFilter = true
	e.acct.AddSnapshotAs("", "flatcar-"+testRelease+"-amd64-old", fake.SnapshotComplete, time.Time{})
	img, err := e.d.ResolveImage(context.Background(), imageReq(vendors.UseRef{}))
	if err != nil || img.Ref != e.snap {
		t.Fatalf("substring filter: %+v, %v", img, err)
	}
}

func TestResolveImageAmbiguousAndPending(t *testing.T) {
	ctx := context.Background()
	e := newDrvEnv(t)
	e.acct.AddSnapshotAs("", "flatcar-"+testRelease+"-amd64", fake.SnapshotComplete, time.Time{})
	if _, err := e.d.ResolveImage(ctx, imageReq(vendors.UseRef{})); !vendors.IsClass(err, vendors.Fatal) ||
		vendors.CodeOf(err) != "ImageAmbiguous" {
		t.Fatalf("two complete snapshots: %v", err)
	}

	// A pending snapshot is not ready until it completes (the miss is cached for
	// five minutes), and a pending twin does not make a complete one ambiguous.
	clk := e.clk
	a, _ := newAccount(clk)
	pending := fake.New()
	pending.SetClock(clk.Now)
	pending.AddSnapshotAs("", "flatcar-"+testRelease+"-amd64", fake.SnapshotPendingState, time.Time{})
	d := driverOver(t, pending, clk, testCluster)
	if _, err := d.ResolveImage(ctx, imageReq(vendors.UseRef{})); !vendors.IsClass(err, vendors.NotFound) {
		t.Fatalf("pending: %v", err)
	}
	pending.Advance()
	clk.Step(6 * time.Minute)
	if _, err := d.ResolveImage(ctx, imageReq(vendors.UseRef{})); err != nil {
		t.Fatalf("complete: %v", err)
	}
	a.AddSnapshotAs("", "flatcar-"+testRelease+"-amd64", fake.SnapshotPendingState, time.Time{})
	if _, err := driverOver(t, a, clk, testCluster).ResolveImage(ctx, imageReq(vendors.UseRef{})); err != nil {
		t.Fatalf("a pending twin: %v", err)
	}
}

func TestResolveImageSelectors(t *testing.T) {
	ctx := context.Background()
	e := newDrvEnv(t)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	older := e.acct.AddSnapshotAs("", "flatcar-stable-4081.2.0-amd64 rebuilt-1", fake.SnapshotComplete, t0)
	newer := e.acct.AddSnapshotAs("", "flatcar-stable-4081.2.0-amd64 rebuilt-2", fake.SnapshotComplete, t0.Add(time.Hour))
	other := e.acct.AddSnapshotAs("", "flatcar-stable-4081.1.0-amd64", fake.SnapshotComplete, t0)
	silent := e.acct.AddSnapshotAs("", "my-golden-image", fake.SnapshotComplete, t0)
	failed := e.acct.AddSnapshotAs("", "flatcar-stable-4081.2.0-amd64 failed", "failed", t0)

	byName := func(sel common.Select, order string) vendors.UseRef {
		return vendors.UseRef{Name: "flatcar-stable-4081.2.0-amd64 rebuilt-*", Select: sel, OrderBy: order}
	}
	for name, tc := range map[string]struct {
		use  vendors.UseRef
		want string
	}{
		"an ID":                 {vendors.UseRef{IDs: []string{older}}, older},
		"the newest by created": {byName(common.SelectNewest, "created"), newer},
		"the oldest by created": {byName(common.SelectOldest, "created"), older},
		"the newest by name":    {byName(common.SelectNewest, "name"), newer},
		"a filter":              {vendors.UseRef{IDs: []string{older, newer}, Filters: []common.ResourceFilter{{Name: "os_id", Values: []string{"0"}}}, Select: common.SelectNewest}, newer},
	} {
		img, err := e.d.ResolveImage(ctx, imageReq(tc.use))
		if err != nil || img.Ref != tc.want {
			t.Errorf("%s: %+v, %v; want %s", name, img, err, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		use  vendors.UseRef
		code string
		cls  vendors.Class
	}{
		"another release":      {vendors.UseRef{IDs: []string{other}}, vendors.CodeImageVersionMismatch, vendors.Fatal},
		"no release":           {vendors.UseRef{IDs: []string{silent}}, vendors.CodeImageVersionUnknown, vendors.Fatal},
		"two under exactlyOne": {byName(common.SelectExactlyOne, ""), "ImageAmbiguous", vendors.Fatal},
		"a failed snapshot":    {vendors.UseRef{IDs: []string{failed}}, "ImageNotFound", vendors.NotFound},
		"select all":           {vendors.UseRef{Name: "x*", Select: common.SelectAll}, "", vendors.Fatal},
		"a filter that fails":  {vendors.UseRef{IDs: []string{older}, Filters: []common.ResourceFilter{{Name: "os_id", Values: []string{"1"}}}}, "ImageNotFound", vendors.NotFound},
		"an arm64-only match":  {vendors.UseRef{IDs: []string{older}, Match: common.ResourceMatch{Architecture: []string{"arm64"}}}, "ImageNotFound", vendors.NotFound},
	} {
		img, err := e.d.ResolveImage(ctx, imageReq(tc.use))
		if img != nil || vendors.ClassOf(err) != tc.cls || (tc.code != "" && vendors.CodeOf(err) != tc.code) {
			t.Errorf("%s: %+v, %v; want %s %s", name, img, err, tc.cls, tc.code)
		}
	}
}

func TestResolveImageServesTheLastGoodAnswerWhenTheListFails(t *testing.T) {
	ctx := context.Background()
	e := newDrvEnv(t)
	if _, err := e.d.ResolveImage(ctx, imageReq(vendors.UseRef{})); err != nil {
		t.Fatal(err)
	}
	e.clk.Step(2 * time.Hour)
	e.acct.FailNext("ListSnapshots", -1, &fake.Error{Status: 503, Message: "down"})
	img, err := e.d.ResolveImage(ctx, imageReq(vendors.UseRef{}))
	if err != nil || img.Ref != e.snap {
		t.Fatalf("stale answer: %+v, %v", img, err)
	}
	req := imageReq(vendors.UseRef{})
	req.Version = "1.2.3"
	if _, err := e.d.ResolveImage(ctx, req); !vendors.IsClass(err, vendors.Retryable) {
		t.Fatalf("no earlier answer: %v", err)
	}
}

func nodeClass(spec v1alpha1.SCNodeClassSpec) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{}
	nc.Name = "default"
	nc.Spec = spec
	if nc.Spec.Flatcar.Version == "" {
		nc.Spec.Flatcar.Version = "4081.2.0"
	}
	return nc
}

func TestReportImages(t *testing.T) {
	ctx := context.Background()
	e := newDrvEnv(t)
	nc := nodeClass(v1alpha1.SCNodeClassSpec{})
	nc.Spec.Vendors.Vultr = &vultrapi.Settings{}
	got, err := e.d.ReportImages(ctx, nc)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, st := range got {
		if !st.Ready || st.Ref != e.snap || st.Arch != "amd64" {
			t.Errorf("%+v", st)
		}
	}
	calls := e.acct.Calls("ListSnapshots")
	if _, err := e.d.ReportImages(ctx, nc); err != nil || e.acct.Calls("ListSnapshots") != calls {
		t.Fatalf("a second report listed again (%d): %v", e.acct.Calls("ListSnapshots"), err)
	}

	// Only the listed regions, and a version the account has no snapshot for.
	nc.Spec.Vendors.Vultr.Regions = []string{"fra"}
	nc.Spec.Flatcar.Version = "1.0.0"
	got, err = e.d.ReportImages(ctx, nc)
	if err != nil || len(got) != 1 || got[0].Region != "fra" || got[0].Ready || got[0].Message == "" {
		t.Fatalf("missing snapshot: %+v, %v", got, err)
	}

	// A snapshot that says another release carries the reason.
	id := e.acct.AddSnapshotAs("", "flatcar-stable-1.0.1-amd64", fake.SnapshotComplete, time.Time{})
	nc.Spec.Vendors.Vultr.Resources = &vultrapi.Resources{Images: &common.Resource{
		Use: &common.ResourceUse{IDs: map[string]common.IDList{"*": {id}}},
	}}
	got, err = e.d.ReportImages(ctx, nc)
	if err != nil || len(got) != 1 || got[0].Ready || got[0].Reason != vendors.CodeImageVersionMismatch {
		t.Fatalf("mismatch: %+v, %v", got, err)
	}

	// A transient failure is no verdict.
	e.clk.Step(time.Hour)
	e.acct.FailNext("ListSnapshots", -1, &fake.Error{Status: 503, Message: "down"})
	nc.Spec.Flatcar.Version = "2.0.0"
	if _, err := e.d.ReportImages(ctx, nc); err == nil {
		t.Fatal("a transient failure was reported as a verdict")
	}
}

// ---- launch and Prepare ----

// provider is a ResourceProvider that answers by kind.
type provider map[vendors.ResourceKind][]vendors.ResourceRef

func (p provider) Require(_ context.Context, req vendors.ResourceRequest) ([]vendors.ResourceRef, error) {
	if refs, ok := p[req.Kind]; ok {
		return refs, nil
	}
	return nil, vendors.NewError(vendors.Unsupported, vendors.CodeResourceMissing, errors.New("no "+string(req.Kind)))
}

func prepareReq(res vendors.ResourceProvider, net common.Networking) vendors.PrepareRequest {
	return vendors.PrepareRequest{
		Region: "ewr", Zone: "ewr", InstanceType: testType, CapacityType: vendors.CapacityOnDemand,
		Settings:  vendors.VendorSettings{Networking: net},
		Tags:      map[vendors.TagKey]string{vendors.TagNodeClass: "default"},
		Resources: res,
	}
}

func TestPrepareRequiresTheGroupAndTheVPCOnlyForAnIsolationThatNeedsOne(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	grp := vendors.ResourceRef{Kind: vultr.KindFirewallGroups, ID: "g1"}
	vpc := vendors.ResourceRef{Kind: vultr.KindNetworks, ID: "v1"}
	res := provider{vultr.KindFirewallGroups: {grp}, vultr.KindNetworks: {vpc}}

	for iso, want := range map[common.Isolation]int{common.IsolationNone: 1, common.IsolationNodeClass: 2, common.IsolationRun: 2, "": 2} {
		got, err := e.d.Prepare(ctx, prepareReq(res, common.Networking{Isolation: iso}))
		if err != nil || len(got.Resources) != want {
			t.Errorf("isolation %q: %+v, %v; want %d resources", iso, got, err, want)
		}
	}
	// A missing resource reaches the caller with its code.
	if _, err := e.d.Prepare(ctx, prepareReq(provider{}, common.Networking{})); vendors.CodeOf(err) != vendors.CodeResourceMissing {
		t.Errorf("no group: %v", err)
	}
	if _, err := e.d.Prepare(ctx, prepareReq(provider{vultr.KindFirewallGroups: {grp}}, common.Networking{})); vendors.CodeOf(err) !=
		vendors.CodeResourceMissing {
		t.Errorf("no VPC: %v", err)
	}
	// An instance takes one firewall group.
	two := provider{vultr.KindFirewallGroups: {grp, {Kind: vultr.KindFirewallGroups, ID: "g2"}}}
	if _, err := e.d.Prepare(ctx, prepareReq(two, common.Networking{Isolation: common.IsolationNone})); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("two groups: %v", err)
	}
}

func TestCreateInstanceWithPreparedResourcesAndIPv6(t *testing.T) {
	e := newDrvEnv(t)
	ctx := context.Background()
	g, _, err := e.d.Ensure(ctx, vultr.KindFirewallGroups, "ewr", managedSpec("scarpenter-my-cluster-firewall-groups-ewr", vultr.KindFirewallGroups, nil))
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := e.d.Ensure(ctx, vultr.KindNetworks, "ewr", managedSpec("scarpenter-my-cluster-networks-ewr", vultr.KindNetworks, nil))
	if err != nil {
		t.Fatal(err)
	}
	ipv6 := true
	req := vendors.CreateRequest{
		Region: "ewr", Zone: "ewr", InstanceType: testType, CapacityType: vendors.CapacityOnDemand, Arch: "amd64",
		Image: vendors.Image{Ref: e.snap}, UserData: []byte(`{"ignition":{"version":"3.4.0"}}`),
		Tags: map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClaim: "nc-1"}, IdempotencyKey: "uid-1",
		Node:     vendors.NodeInfo{Name: "nc-1"},
		Settings: vendors.VendorSettings{Networking: common.Networking{PublicIPv6: &ipv6}},
		Prepared: &vendors.Prepared{Resources: []vendors.ResourceRef{g, v, v}},
	}
	inst, err := e.d.CreateInstance(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	in := e.acct.Instances()[0]
	if in.FirewallGroupID != g.ID || in.V6MainIP == "" || inst.Ref.ID() != in.ID {
		t.Fatalf("the instance: %+v", in)
	}
	if used, _, err := e.d.InUse(ctx, v); err != nil || !used {
		t.Fatalf("the VPC is attached: %v, %v", used, err)
	}
	// Two groups, or no public IPv4, are refused before any call.
	bad := req
	bad.IdempotencyKey = "uid-2"
	bad.Prepared = &vendors.Prepared{Resources: []vendors.ResourceRef{g, {Kind: vultr.KindFirewallGroups, ID: "other"}}}
	if _, err := e.d.CreateInstance(ctx, bad); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("two groups: %v", err)
	}
	no := false
	bad.Prepared = nil
	bad.Settings.Networking.PublicIPv4 = &no
	if _, err := e.d.CreateInstance(ctx, bad); !vendors.IsClass(err, vendors.Unsupported) {
		t.Errorf("no public IPv4: %v", err)
	}
	if got := e.acct.Calls("CreateInstance"); got != 1 {
		t.Errorf("%d creates, want 1", got)
	}
}

// ---- options, billing, hardening ----

func TestNodeOptions(t *testing.T) {
	d := vultr.New()
	def := d.Defaults()
	if string(def.Raw) != `{}` || d.RootDiskBytes(def) != 0 {
		t.Fatalf("defaults: %+v", def)
	}
	if b, ok := d.RootDiskForType(def, vendors.CatalogType{LocalStorageGB: 25}); !ok || b != 25e9 {
		t.Fatalf("25 GB: %d %v", b, ok)
	}
	if _, ok := d.RootDiskForType(def, vendors.CatalogType{}); ok {
		t.Fatal("a type without a disk defines no root disk")
	}
	profiles := vendors.Profiles{vendors.ProfileStorage: {"x": json.RawMessage(`{}`)}}
	if got := d.Supported(vendors.CatalogType{APIReference: testType}, "ewr", profiles); len(got) != 0 {
		t.Fatalf("a profile with a vultr block is supported: %v", got)
	}
	if delta, complete := d.PriceDelta(def, vendors.Offering{}); delta != 0 || !complete {
		t.Fatalf("price delta: %v %v", delta, complete)
	}
}

func TestBillingRuleIsReportedUnverified(t *testing.T) {
	r, ok := vultr.New().BillingRule("ewr", "vc2")
	if !ok || r.Verified || r.Increment == nil || *r.Increment != time.Hour || r.StoppedCharged == nil || !*r.StoppedCharged ||
		r.Source == "" {
		t.Fatalf("rule: %+v", r)
	}
}

func TestBillingStartIsDateCreated(t *testing.T) {
	e := newDrvEnv(t)
	req := vendors.CreateRequest{
		Region: "ewr", InstanceType: testType, Image: vendors.Image{Ref: e.snap}, UserData: []byte("{}"),
		Tags: map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClaim: "n"}, IdempotencyKey: "u",
		Node: vendors.NodeInfo{Name: "n"},
	}
	inst, err := e.d.CreateInstance(context.Background(), req)
	if err != nil || inst.BillingStart.IsZero() || !inst.BillingStart.Equal(inst.CreatedAt) {
		t.Fatalf("%+v, %v", inst, err)
	}
}

func TestHardeningBlocksTheMetadataAddress(t *testing.T) {
	d := vultr.New()
	if got := d.Hardenings(); len(got) != 1 || got[0] != vendors.HardeningMetadataFromPods {
		t.Fatalf("hardenings: %v", got)
	}
	if frags, err := d.BootFragments(vendors.BootFragmentRequest{}); err != nil || len(frags) != 0 {
		t.Fatalf("no hardening requested: %v, %v", frags, err)
	}
	frags, err := d.BootFragments(vendors.BootFragmentRequest{Hardening: []string{vendors.HardeningMetadataFromPods}})
	if err != nil || len(frags) != 1 || !strings.Contains(frags[0].Butane, "169.254.169.254") {
		t.Fatalf("fragments: %v, %v", frags, err)
	}
}

// ---- the committed Keeper snapshot ----

// Every family of the vendor's rows in the committed snapshot is classified on
// purpose, and the launchable ones are the families of the plan; a row that has
// a family nobody classified would be silently dropped.
func TestEveryFamilyOfTheSnapshotIsClassified(t *testing.T) {
	launchable := 0
	for _, r := range vultrServers(t) {
		ok, why := vultr.ClassifyPlan(r.APIReference, int(r.StorageSize))
		if strings.Contains(why, "unclassified") {
			t.Errorf("%s: %s", r.APIReference, why)
		}
		if ok {
			launchable++
		}
	}
	if launchable == 0 {
		t.Fatal("no Vultr row is launchable")
	}
}

var _ = govultr.ListOptions{}
var _ = metav1.Time{}
