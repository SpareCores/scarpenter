/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package oci_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	apicommon "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	ociapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/oci"
	"github.com/SpareCores/scarpenter/pkg/resources"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	driver "github.com/SpareCores/scarpenter/pkg/vendors/oci"
	"github.com/SpareCores/scarpenter/pkg/vendors/oci/fake"
)

// ----- images -----

func imageReq(arch string, use vendors.UseRef) vendors.ImageRequest {
	return vendors.ImageRequest{Region: testRegion, Arch: arch, Channel: "stable", Version: fake.SeedVersion, Use: use}
}

// The default lookup finds the image of the convention by its name, ignores the platform images,
// and reads the architecture from the tag or from the shape compatibility entries; an image whose
// architecture is unknown never satisfies a request.
func TestResolveImageDefaultLookup(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	img, err := e.d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{}))
	if err != nil || img.Ref != fake.SeedImage || img.Version != fake.SeedVersion || img.Channel != "stable" ||
		img.RootDiskBytes != 47719<<20 {
		t.Fatalf("amd64: %+v, %v", img, err)
	}
	if _, err := e.d.ResolveImage(ctx, imageReq("arm64", vendors.UseRef{})); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("arm64 without an image: %v, want NotFound", err)
	}
	e.c.AddImage(testRegion, fake.PlatformImage("flatcar-stable-"+fake.SeedVersion+"-amd64-platform"))

	// An arm64 image with no arch tag is arm64 by its compatible shapes; one with no tag and no
	// entries has no architecture and is never used.
	arm := fake.FlatcarImage("stable", fake.SeedVersion, "arm64")
	delete(arm.FreeformTags, "arch")
	armID := e.c.AddImage(testRegion, arm, "VM.Standard.A1.Flex")
	e.clock.Step(2 * time.Hour)
	got, err := e.d.ResolveImage(ctx, imageReq("arm64", vendors.UseRef{}))
	if err != nil || got.Ref != armID {
		t.Fatalf("arm64 by compatibility entries: %+v, %v", got, err)
	}
	unknown := fake.FlatcarImage("stable", "4800.0.0", "amd64")
	delete(unknown.FreeformTags, "arch")
	e.c.AddImage(testRegion, unknown)
	req := imageReq("amd64", vendors.UseRef{})
	req.Version = "4800.0.0"
	if _, err := e.d.ResolveImage(ctx, req); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an image of unknown architecture: %v, want NotFound", err)
	}
	// Two images of the convention are ambiguous.
	e.c.AddImage(testRegion, fake.FlatcarImage("stable", fake.SeedVersion, "amd64"))
	e.clock.Step(2 * time.Hour)
	if _, err := e.d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{})); vendors.CodeOf(err) != "ImageAmbiguous" {
		t.Errorf("two images: %v, want ImageAmbiguous", err)
	}
	if _, err := e.d.ResolveImage(ctx, imageReq("ppc64", vendors.UseRef{})); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("another architecture: %v, want Fatal", err)
	}
	bad := imageReq("amd64", vendors.UseRef{})
	bad.Region = "ap-sydney-1"
	if _, err := e.d.ResolveImage(ctx, bad); !vendors.IsClass(err, vendors.Unsupported) {
		t.Errorf("a region that is not subscribed: %v, want Unsupported", err)
	}
}

// The answers, and the misses, are cached; a miss clears after five minutes.
func TestResolveImageCaches(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	req := imageReq("arm64", vendors.UseRef{})
	if _, err := e.d.ResolveImage(ctx, req); err == nil {
		t.Fatal("no arm64 image yet")
	}
	e.c.AddImage(testRegion, fake.FlatcarImage("stable", fake.SeedVersion, "arm64"))
	if _, err := e.d.ResolveImage(ctx, req); err == nil {
		t.Error("the miss is not cached")
	}
	e.clock.Step(6 * time.Minute)
	if _, err := e.d.ResolveImage(ctx, req); err != nil {
		t.Errorf("after the miss expired: %v", err)
	}
	calls := e.c.Calls("ListImages")
	if _, err := e.d.ResolveImage(ctx, req); err != nil || e.c.Calls("ListImages") != calls {
		t.Errorf("a hit lists again (%d calls)", e.c.Calls("ListImages")-calls)
	}
	// A transient failure after the entry expired serves the last answer.
	e.clock.Step(2 * time.Hour)
	e.c.Inject("ListImages", fake.Error(http.StatusServiceUnavailable, "ServiceUnavailable", "later"), 1)
	if img, err := e.d.ResolveImage(ctx, req); err != nil || img == nil {
		t.Errorf("a transient failure: %v, want the last answer", err)
	}
}

// The selector: the native filters, the state, the tags, the version check and the IDs.
func TestResolveImageSelector(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	old := fake.FlatcarImage("stable", "4500.1.0", "amd64")
	oldID := e.c.AddImage(testRegion, old)
	byTag := vendors.UseRef{Tags: map[string]string{"os": "flatcar", "flatcar-version": fake.SeedVersion}}
	if img, err := e.d.ResolveImage(ctx, imageReq("amd64", byTag)); err != nil || img.Ref != fake.SeedImage {
		t.Errorf("by tags: %+v, %v", img, err)
	}
	if _, err := e.d.ResolveImage(ctx, imageReq("amd64", vendors.UseRef{IDs: []string{oldID}})); vendors.CodeOf(err) != vendors.CodeImageVersionMismatch {
		t.Errorf("an image of another version: %v, want ImageVersionMismatch", err)
	}
	byFilter := vendors.UseRef{Filters: []apicommon.ResourceFilter{
		{Name: "operating-system", Values: []string{"Flatcar"}}, {Name: "operating-system-version", Values: []string{fake.SeedVersion}},
		{Name: "shape", Values: []string{"VM.Standard.E5.Flex"}}, {Name: "lifecycle-state", Values: []string{"available"}}}}
	if img, err := e.d.ResolveImage(ctx, imageReq("amd64", byFilter)); err != nil || img.Ref != fake.SeedImage {
		t.Errorf("by native filters: %+v, %v", img, err)
	}
	two := vendors.UseRef{Filters: []apicommon.ResourceFilter{{Name: "shape", Values: []string{"a", "b"}}}}
	if _, err := e.d.ResolveImage(ctx, imageReq("amd64", two)); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a filter with two values: %v, want Fatal", err)
	}
	all := vendors.UseRef{Tags: map[string]string{"os": "flatcar"}, Select: apicommon.SelectAll}
	if _, err := e.d.ResolveImage(ctx, imageReq("amd64", all)); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("select all: %v, want Fatal", err)
	}
	// An image that is still importing is not available.
	pending := fake.FlatcarImage("stable", fake.SeedVersion, "amd64")
	pending.FreeformTags["pending"] = "true"
	pid := e.c.AddImage(testRegion, pending)
	e.c.SetImageState(pid, core.ImageLifecycleStateImporting)
	byState := vendors.UseRef{Tags: map[string]string{"pending": "true"}}
	if _, err := e.d.ResolveImage(ctx, imageReq("amd64", byState)); !vendors.IsClass(err, vendors.NotFound) {
		t.Errorf("an importing image: %v, want NotFound (the default state is available)", err)
	}
	byState.Match.State = []string{"pending"}
	if img, err := e.d.ResolveImage(ctx, imageReq("amd64", byState)); err != nil || img.Ref != pid {
		t.Errorf("match.state pending: %+v, %v", img, err)
	}
}

func class(res *ociapi.Resources, regions ...string) *v1alpha1.SCNodeClass {
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec: v1alpha1.SCNodeClassSpec{Vendors: v1alpha1.VendorBlocks{OCI: &ociapi.Settings{Regions: regions, Resources: res}},
			Flatcar: v1alpha1.FlatcarSpec{Version: fake.SeedVersion}},
	}
	return nc
}

func TestReportImages(t *testing.T) {
	e := newEnv(t)
	got, err := e.d.ReportImages(t.Context(), class(nil, testRegion, fake.SeedRegionB))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{testRegion + "/amd64": true}
	if len(got) != 4 {
		t.Fatalf("%d statuses, want 4 (two regions, two architectures): %+v", len(got), got)
	}
	for _, s := range got {
		if s.Ready != want[s.Region+"/"+s.Arch] || (s.Ready && s.Ref != fake.SeedImage) || (!s.Ready && s.Message == "") {
			t.Errorf("status %+v", s)
		}
	}
	// With no regions named, every subscribed region is reported; a bad selector is a message.
	nc := class(&ociapi.Resources{Images: &apicommon.Resource{Use: &apicommon.ResourceUse{
		IDs: map[string]apicommon.IDList{testRegion: {fake.SeedImage}}}}})
	all, err := e.d.ReportImages(t.Context(), nc)
	if err != nil || len(all) != 6 {
		t.Fatalf("all regions: %d statuses, %v; want 6 (three subscribed regions)", len(all), err)
	}
	e.c.Inject("ListRegionSubscriptions", fake.Error(http.StatusInternalServerError, "InternalError", "x"), 1)
	e.clock.Step(2 * time.Hour)
	if _, err := e.d.ReportImages(t.Context(), nc); err == nil {
		t.Error("a failed subscription list is an error")
	}
	fresh := newEnv(t)
	fresh.c.Inject("ListImages", fake.Error(http.StatusServiceUnavailable, "ServiceUnavailable", "later"), 1)
	if _, err := fresh.d.ReportImages(t.Context(), class(nil, testRegion)); err == nil {
		t.Error("a transient failure is an error, not a not-ready image")
	}
}

// ----- prepare, managed network and groups -----

type engineEnv struct {
	*env
	engine *resources.Engine
}

func newEngine(t *testing.T) *engineEnv {
	t.Helper()
	e := newEnv(t)
	return &engineEnv{e, resources.New(resources.Options{Registry: vendors.NewRegistryFrom(e.d), ClusterName: testCluster})}
}

func (e *engineEnv) prepare(t *testing.T, nc *v1alpha1.SCNodeClass, zone, runKey string, net apicommon.Networking,
) (*vendors.Prepared, error) {
	t.Helper()
	s := vendors.VendorSettings{Networking: net, Raw: e.d.Defaults().Raw}
	if r := nc.Spec.Vendors.OCI.Resources; r != nil {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		s.Resources = raw
	}
	return e.d.Prepare(t.Context(), vendors.PrepareRequest{
		Region: testRegion, Zone: zone, InstanceType: testType, CapacityType: vendors.CapacityOnDemand,
		Settings: s, RunKey: runKey,
		Tags: map[vendors.TagKey]string{vendors.TagCluster: testCluster, vendors.TagNodeClass: nc.Name,
			vendors.TagManagedBy: vendors.ManagedByValue},
		Resources: e.engine.Provider(nc, "oci", testRegion, zone),
	})
}

func resourceKinds(p *vendors.Prepared) map[vendors.ResourceKind][]string {
	out := map[vendors.ResourceKind][]string{}
	for _, r := range p.Resources {
		out[r.Kind] = append(out[r.Kind], r.ID)
	}
	return out
}

func inbound22() *apicommon.Inbound {
	return &apicommon.Inbound{Rules: []apicommon.InboundRule{
		{Protocol: "tcp", FromPort: new(int32(22)), CIDRs: []string{"203.0.113.0/24"}},
		{Protocol: "udp", FromPort: new(int32(1000)), ToPort: new(int32(2000)), CIDRs: []string{"10.0.0.0/8"}},
		{Protocol: "icmp", CIDRs: []string{"0.0.0.0/0"}},
		{Protocol: "all", CIDRs: []string{"192.0.2.0/24"}},
	}}
}

// Without a use block the managed network is created (the VCN, the gateway, the route table, the
// minimal security list, the subnet) and the groups with it; a second Prepare finds them, and the
// groups carry the rules of the NodeClass.
func TestPrepareCreatesTheManagedNetworkAndGroups(t *testing.T) {
	e := newEngine(t)
	nc := class(&ociapi.Resources{Subnets: &ociapi.SubnetResource{Create: &ociapi.SubnetCreate{CIDR: "172.20.0.0/16"}}})
	net := apicommon.Networking{PublicIPv4: new(true), Isolation: apicommon.IsolationRun, Inbound: inbound22()}
	first, err := e.prepare(t, nc, testZone, "run-1", net)
	if err != nil {
		t.Fatal(err)
	}
	vcns, subnets, gateways, routes, lists, groups := e.c.NetworkCounts()
	// The seeded network has one VCN, a subnet and two groups.
	if vcns != 2 || subnets != 2 || gateways != 1 || routes != 1 || lists != 1 || groups != 4 {
		t.Errorf("counts after the first Prepare: %d %d %d %d %d %d", vcns, subnets, gateways, routes, lists, groups)
	}
	kinds := resourceKinds(first)
	if len(kinds[driver.KindSubnets]) != 1 || len(kinds[driver.KindNetworkSecurityGroups]) != 1 ||
		len(kinds[driver.KindRunNetworkSecurityGroup]) != 1 {
		t.Fatalf("prepared %v, want a subnet, a group and a run group", kinds)
	}
	sub, _ := e.c.Subnet(kinds[driver.KindSubnets][0])
	if *sub.CidrBlock != "172.20.0.0/20" || *sub.ProhibitPublicIpOnVnic || sub.AvailabilityDomain != nil {
		t.Errorf("the managed subnet: %s prohibit=%v ad=%v; want the first /20 of the range, public, regional",
			*sub.CidrBlock, *sub.ProhibitPublicIpOnVnic, sub.AvailabilityDomain)
	}
	rules := e.c.NSGRules(kinds[driver.KindNetworkSecurityGroups][0])
	protos := make([]string, 0, len(rules))
	for _, r := range rules {
		protos = append(protos, *r.Protocol)
	}
	slices.Sort(protos)
	// egress, ICMP type 3 code 4, the tcp, udp, icmp and all rules (no self rule: isolation run
	// puts it on the run group).
	if want := []string{"1", "1", "17", "6", "all", "all"}; !slices.Equal(protos, want) {
		t.Errorf("the group's protocols %v, want %v", protos, want)
	}
	run := e.c.NSGRules(kinds[driver.KindRunNetworkSecurityGroup][0])
	if len(run) != 3 || run[2].SourceType != core.AddSecurityRuleDetailsSourceTypeNetworkSecurityGroup {
		t.Errorf("the run group's rules: %+v", run)
	}

	second, err := e.prepare(t, nc, testZone, "run-1", net)
	if err != nil || resourceKinds(first)[driver.KindSubnets][0] != resourceKinds(second)[driver.KindSubnets][0] {
		t.Fatalf("a second Prepare: %v, %v", second, err)
	}
	if v, s, g, r, l, n := e.c.NetworkCounts(); v != vcns || s != subnets || g != gateways || r != routes || l != lists || n != groups {
		t.Errorf("a second Prepare created more: %d %d %d %d %d %d", v, s, g, r, l, n)
	}
	// Another range is another network.
	nc2 := class(&ociapi.Resources{Subnets: &ociapi.SubnetResource{Create: &ociapi.SubnetCreate{CIDR: "192.168.0.0/16"}}})
	if _, err := e.prepare(t, nc2, testZone, "", net); err != nil {
		t.Fatal(err)
	}
	if v, _, _, _, _, _ := e.c.NetworkCounts(); v != vcns+1 {
		t.Errorf("a second range: %d VCNs, want %d", v, vcns+1)
	}
}

// A network whose creation was interrupted is adopted and finished; a part still provisioning
// is Retryable; a VCN of the name without the marker is refused.
func TestManagedNetworkAdoptionAndOwnership(t *testing.T) {
	e := newEngine(t)
	mr := vendors.ManagedResources(e.d)
	spec := vendors.CreateSpec{Name: "scarpenter-conformance-net", Tags: map[vendors.TagKey]string{
		vendors.TagCluster: testCluster, vendors.TagNodeClass: "class", vendors.TagManagedBy: vendors.ManagedByValue,
		vendors.TagResource: "subnets"}, ExtraTags: map[string]string{"team": "x"}}
	e.c.Inject("CreateSubnet", fake.Error(http.StatusInternalServerError, "InternalError", "x"), 1)
	if _, _, err := mr.Ensure(t.Context(), driver.KindSubnets, testRegion, spec); err == nil {
		t.Fatal("the interrupted creation succeeded")
	}
	ref, created, err := mr.Ensure(t.Context(), driver.KindSubnets, testRegion, spec)
	if err != nil || !created || !ref.Managed {
		t.Fatalf("the next pass: %+v, %v, %v", ref, created, err)
	}
	if v, s, g, r, l, _ := e.c.NetworkCounts(); v != 2 || s != 2 || g != 1 || r != 1 || l != 1 {
		t.Errorf("the parts after adoption: %d %d %d %d %d, want one of each added to the seeded network", v, s, g, r, l)
	}
	if listed, err := mr.List(t.Context(), nil, map[vendors.TagKey]string{vendors.TagCluster: testCluster}); err != nil || len(listed) != 1 ||
		listed[0].ID != ref.ID || listed[0].Tags[vendors.TagNodeClass] != "class" {
		t.Errorf("List = %+v, %v", listed, err)
	}
	// Another cluster does not own it.
	other := restart(t, e.c, "another")
	if _, _, err := vendors.ManagedResources(other.d).Ensure(t.Context(), driver.KindSubnets, testRegion, spec); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a network of another cluster: %v, want Fatal", err)
	}
	if err := vendors.ManagedResources(other.d).Delete(t.Context(), ref); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("Delete of another cluster's network: %v, want Fatal", err)
	}
	// A subnet a user added to our VCN keeps the VCN and its gateway: only ours go.
	user := e.c.SeedSubnet(testRegion, ref.ID, "user-subnet", "10.240.100.0/24", "", nil, false, false)
	if err := mr.Delete(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if v, s, g, _, _, _ := e.c.NetworkCounts(); v != 2 || s != 2 || g != 1 {
		t.Errorf("after the delete with a foreign subnet: %d VCNs %d subnets %d gateways", v, s, g)
	}
	if _, ok := e.c.Subnet(user); !ok {
		t.Error("the user's subnet was deleted")
	}
	e.c.Inject("DeleteVcn", fake.Error(http.StatusConflict, "Conflict", "busy"), 1)
	if err := mr.Delete(t.Context(), vendors.ResourceRef{Kind: driver.KindSubnets, Region: testRegion, ID: "ocid1.vcn.oc1.eu-frankfurt-1.gone"}); err != nil {
		t.Errorf("Delete of a VCN that is gone: %v", err)
	}
	if _, _, err := mr.Ensure(t.Context(), driver.KindSubnets, "ap-sydney-1", spec); !vendors.IsClass(err, vendors.RegionUnavailable) {
		t.Errorf("a region not subscribed: %v", err)
	}
	if _, _, err := mr.Ensure(t.Context(), "nonsense", testRegion, spec); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("an unknown kind: %v", err)
	}
	if _, _, err := mr.Ensure(t.Context(), driver.KindSubnets, testRegion, vendors.CreateSpec{}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a resource without a name: %v", err)
	}
	spec.Params = []byte(`{"cidr":"10.0.0.0/28"}`)
	spec.Name = "other"
	if _, _, err := mr.Ensure(t.Context(), driver.KindSubnets, testRegion, spec); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("a range too small for a /20: %v", err)
	}
}

// Brought subnets: an AD-specific subnet serves only its AD, a subnet that prohibits public
// addresses cannot take publicIPv4, a missing IPv6 prefix cannot take publicIPv6; the zones that
// cannot host a node are gaps, and Prepare skips the zone.
func TestBroughtSubnetsZonesAndOptions(t *testing.T) {
	e := newEngine(t)
	vcn := e.c.SeededVCN()
	adOnly := e.c.SeedSubnet(testRegion, vcn, "ad1", "10.0.100.0/24", fake.ADFull(fake.SeedZoneA), nil, false, false)
	private := e.c.SeedSubnet(testRegion, vcn, "private", "10.0.101.0/24", "", nil, false, true)
	v6 := e.c.SeedSubnet(testRegion, vcn, "v6", "10.0.102.0/24", "", nil, true, false)
	use := func(ids ...string) *v1alpha1.SCNodeClass {
		return class(&ociapi.Resources{Subnets: &ociapi.SubnetResource{Use: &apicommon.ResourceUse{
			IDs: map[string]apicommon.IDList{testRegion: ids}, Select: apicommon.SelectAll}}})
	}
	found := func(ids ...string) map[vendors.ResourceKind][]vendors.ResourceRef {
		refs, err := e.d.Resolve(t.Context(), driver.KindSubnets, testRegion, vendors.UseRef{IDs: ids})
		if err != nil || len(refs) != len(ids) {
			t.Fatalf("Resolve(%v) = %v, %v", ids, refs, err)
		}
		return map[vendors.ResourceKind][]vendors.ResourceRef{driver.KindSubnets: refs}
	}
	net := apicommon.Networking{PublicIPv4: new(true), PublicIPv6: new(false)}
	settings := vendors.VendorSettings{Networking: net}
	gaps, err := e.d.ZoneGaps(t.Context(), testRegion, settings, found(adOnly))
	if err != nil || len(gaps) != 2 || gaps[fake.SeedZoneA] != "" || !strings.Contains(gaps[fake.SeedZoneB], "AD-specific") {
		t.Errorf("an AD-specific subnet: gaps %v, %v; want the other two ADs", gaps, err)
	}
	if _, err := e.prepare(t, use(adOnly), fake.SeedZoneB, "", net); vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Errorf("Prepare in an AD the subnet does not serve: %v", err)
	}
	if p, err := e.prepare(t, use(adOnly), fake.SeedZoneA, "", net); err != nil || resourceKinds(p)[driver.KindSubnets][0] != adOnly {
		t.Errorf("Prepare in the subnet's AD: %v, %v", p, err)
	}
	gaps, _ = e.d.ZoneGaps(t.Context(), testRegion, settings, found(private))
	if len(gaps) != 3 || !strings.Contains(gaps[fake.SeedZoneA], "prohibit public IP") {
		t.Errorf("a private subnet with publicIPv4: gaps %v", gaps)
	}
	if gaps, _ = e.d.ZoneGaps(t.Context(), testRegion, vendors.VendorSettings{Networking: apicommon.Networking{PublicIPv4: new(false)}},
		found(private)); len(gaps) != 0 {
		t.Errorf("a private subnet with publicIPv4 false: gaps %v", gaps)
	}
	v6net := apicommon.Networking{PublicIPv4: new(true), PublicIPv6: new(true)}
	gaps, _ = e.d.ZoneGaps(t.Context(), testRegion, vendors.VendorSettings{Networking: v6net}, found(adOnly))
	if len(gaps) != 3 || !strings.Contains(gaps[fake.SeedZoneA], "IPv6") {
		t.Errorf("publicIPv6 without a prefix: gaps %v", gaps)
	}
	// A regional subnet with the prefix and a public one: the first that honours the options wins,
	// the regional one before an AD-specific one.
	p, err := e.prepare(t, use(adOnly, v6), fake.SeedZoneA, "", v6net)
	if err != nil || resourceKinds(p)[driver.KindSubnets][0] != v6 {
		t.Errorf("publicIPv6: %v, %v", p, err)
	}
	if p, err = e.prepare(t, use(adOnly, private, v6), fake.SeedZoneA, "", net); err != nil || resourceKinds(p)[driver.KindSubnets][0] != v6 {
		t.Errorf("a regional subnet before an AD-specific one: %v, %v", p, err)
	}
	if g, err := e.d.ZoneGaps(t.Context(), testRegion, settings, nil); err != nil || g != nil {
		t.Errorf("nothing found: %v, %v", g, err)
	}
	// A brought group in another VCN skips the zone.
	other := e.c.SeedVcn(testRegion, "other", "10.9.0.0/16", nil)
	foreign := e.c.SeedNSG(testRegion, other, "foreign", nil)
	nc := class(&ociapi.Resources{
		Subnets: &ociapi.SubnetResource{Use: &apicommon.ResourceUse{IDs: map[string]apicommon.IDList{testRegion: {v6}}}},
		NetworkSecurityGroups: &apicommon.Resource{Use: &apicommon.ResourceUse{
			IDs: map[string]apicommon.IDList{testRegion: {foreign}}}}})
	if _, err := e.prepare(t, nc, fake.SeedZoneA, "", net); vendors.CodeOf(err) != vendors.CodeResourceMissingInZone {
		t.Errorf("a group in another VCN: %v", err)
	}
	// Six groups do not fit a VNIC.
	ids := make([]string, 0, 6)
	for i := range 6 {
		ids = append(ids, e.c.SeedNSG(testRegion, vcn, "g"+string(rune('a'+i)), nil))
	}
	many := class(&ociapi.Resources{
		Subnets: &ociapi.SubnetResource{Use: &apicommon.ResourceUse{IDs: map[string]apicommon.IDList{testRegion: {v6}}}},
		NetworkSecurityGroups: &apicommon.Resource{Use: &apicommon.ResourceUse{
			IDs: map[string]apicommon.IDList{testRegion: ids}, Select: apicommon.SelectAll}}})
	if _, err := e.prepare(t, many, fake.SeedZoneA, "", net); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("six groups: %v, want Fatal", err)
	}
}

// ----- node options -----

func TestNodeOptions(t *testing.T) {
	e := newEnv(t)
	d := e.d
	def := d.Defaults()
	if d.RootDiskBytes(def) != 64<<30 || def.RootDiskBytes != 64<<30 {
		t.Errorf("the default root disk is %d", d.RootDiskBytes(def))
	}
	if d.RootDiskBytes(vendors.VendorSettings{Raw: []byte(`{"storage":{"root":{"sizeGb":10}}}`)}) != 0 {
		t.Error("a root disk below the minimum has a size")
	}
	if _, ok := d.RootDiskForType(def, vendors.CatalogType{}); ok {
		t.Error("the root disk is not the type's")
	}
	if got := d.Supported(vendors.CatalogType{}, testZone, vendors.Profiles{vendors.ProfileStorage: {"x": nil}}); got != nil {
		t.Errorf("Supported = %v: OCI has no profiles", got)
	}
	if !d.PricesPerRegion() {
		t.Error("OCI prices are per region")
	}

	offering := vendors.Offering{Region: testRegion, Zone: testZone}
	if _, complete := d.PriceDelta(def, offering); complete {
		t.Error("a delta without prices is complete")
	}
	before := d.NodeOptionsEpoch()
	d.SetKeeperPrices(vendors.KeeperPrices{Storage: []vendors.StoragePrice{
		{Region: testRegion, StorageID: "balanced", Unit: "GB/month", USD: 0.0425},
		{Region: testRegion, StorageID: "ultra-high-performance", Unit: "GB/month", USD: 0.0765},
		{Region: testRegion, StorageID: "lower-cost", Unit: "GB/month", USD: 0}, // free rows are not prices
	}})
	if d.NodeOptionsEpoch() == before {
		t.Error("new prices do not change the epoch")
	}
	delta, complete := d.PriceDelta(def, offering)
	if want := 64 * 0.0425 / 730; !complete || delta < want-1e-12 || delta > want+1e-12 {
		t.Errorf("PriceDelta = %v, %v; want %v", delta, complete, want)
	}
	epoch := d.NodeOptionsEpoch()
	d.SetKeeperPrices(vendors.KeeperPrices{Storage: []vendors.StoragePrice{
		{Region: testRegion, StorageID: "balanced", Unit: "GB/month", USD: 0.0425},
		{Region: testRegion, StorageID: "ultra-high-performance", Unit: "GB/month", USD: 0.0765}}})
	if d.NodeOptionsEpoch() != epoch {
		t.Error("the same prices change the epoch")
	}
	ultra := vendors.VendorSettings{Raw: []byte(`{"storage":{"root":{"sizeGb":100,"performance":"ultra-high-performance","vpusPerGb":60}}}`)}
	if _, complete := d.PriceDelta(ultra, offering); complete {
		t.Error("more VPUs than the level's price covers is complete")
	}
	if _, complete := d.PriceDelta(vendors.VendorSettings{Raw: []byte(`{"storage":{"root":{"sizeGb":5}}}`)}, offering); complete {
		t.Error("invalid settings are complete")
	}
	if _, err := d.Prepare(t.Context(), vendors.PrepareRequest{Settings: vendors.VendorSettings{Raw: []byte(`{"nope":1}`)}}); !vendors.IsClass(err, vendors.Fatal) {
		t.Errorf("Prepare with unknown settings: %v", err)
	}
}

func TestHardening(t *testing.T) {
	d := driver.New()
	if got := d.Hardenings(); !slices.Equal(got, []string{vendors.HardeningMetadataFromPods}) {
		t.Errorf("Hardenings = %v", got)
	}
	frags, err := d.BootFragments(vendors.BootFragmentRequest{Hardening: []string{vendors.HardeningMetadataFromPods}})
	if err != nil || len(frags) != 1 || !strings.Contains(frags[0].Butane, driver.MetadataAddress) {
		t.Errorf("BootFragments = %+v, %v", frags, err)
	}
	if frags, err := d.BootFragments(vendors.BootFragmentRequest{}); err != nil || len(frags) != 0 {
		t.Errorf("without hardening: %+v, %v", frags, err)
	}
	if _, err := d.BootFragments(vendors.BootFragmentRequest{Hardening: []string{"nope"}}); err == nil {
		t.Error("an unknown hardening is accepted")
	}
}

// ----- validation -----

func TestValidateNodeClass(t *testing.T) {
	d := newEnv(t).d
	ocid := func(kind string) string { return "ocid1." + kind + ".oc1.eu-frankfurt-1.aaaa" }
	use := func(ids map[string]apicommon.IDList) *apicommon.Resource {
		return &apicommon.Resource{Use: &apicommon.ResourceUse{IDs: ids}}
	}
	for name, tc := range map[string]struct {
		mutate func(*v1alpha1.SCNodeClass)
		want   string
	}{
		"valid":    {func(*v1alpha1.SCNodeClass) {}, ""},
		"no block": {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.OCI = nil }, ""},
		"region":   {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.OCI.Regions = []string{"Frankfurt"} }, "not an OCI region name"},
		"root size": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Storage = &ociapi.Storage{Root: &ociapi.RootVolume{SizeGb: new(int64(10))}}
		}, "boot volumes are"},
		"vpus": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Storage = &ociapi.Storage{Root: &ociapi.RootVolume{Performance: "balanced", VpusPerGb: new(int64(40))}}
		}, "vpusPerGb"},
		"kms key": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Storage = &ociapi.Storage{Root: &ociapi.RootVolume{KmsKeyID: "key"}}
		}, "key OCID"},
		"vault key with its vault segment": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Storage = &ociapi.Storage{Root: &ociapi.RootVolume{KmsKeyID: "ocid1.key.oc1.iad.bbqzaaaa.abtheljraaaa"}}
		}, ""},
		"lower-cost boot volume": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Storage = &ociapi.Storage{Root: &ociapi.RootVolume{Performance: "lower-cost"}}
		}, "no lower-cost level"},
		"multi-word region": {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.OCI.Regions = []string{"us-gov-ashburn-1"} }, ""},
		"metadata":          {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.OCI.Metadata = map[string]string{"user_data": "x"} }, "reserved"},
		"tag prefix":        {func(nc *v1alpha1.SCNodeClass) { nc.Spec.Tags = map[string]string{"scarpenter-x": "y"} }, "reserved prefix"},
		"too many tags": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Tags = map[string]string{}
			for i := range 60 {
				nc.Spec.Tags[strings.Repeat("k", 3)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
			}
		}, "at most"},
		"inbound": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Networking = &apicommon.NetworkingOverride{Inbound: &apicommon.Inbound{Rules: []apicommon.InboundRule{{Protocol: "tcp", CIDRs: []string{"nope"}}}}}
		}, "not a CIDR"},
		"images star": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Resources = &ociapi.Resources{Images: use(map[string]apicommon.IDList{"*": {ocid("image")}})}
		}, "regional"},
		"image id": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Resources = &ociapi.Resources{Images: use(map[string]apicommon.IDList{testRegion: {ocid("subnet")}})}
		}, "image OCID"},
		"empty use": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Resources = &ociapi.Resources{NetworkSecurityGroups: &apicommon.Resource{Use: &apicommon.ResourceUse{}}}
		}, "selects nothing"},
		"tag key": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Resources = &ociapi.Resources{Images: &apicommon.Resource{Use: &apicommon.ResourceUse{Tags: map[string]string{strings.Repeat("k", 101): "v"}}}}
		}, "freeform tag key"},
		"create cidr public": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Resources = &ociapi.Resources{Subnets: &ociapi.SubnetResource{Create: &ociapi.SubnetCreate{CIDR: "8.8.0.0/16"}}}
		}, "private ranges"},
		"create cidr": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Resources = &ociapi.Resources{Subnets: &ociapi.SubnetResource{Create: &ociapi.SubnetCreate{CIDR: "10.0.0.0/24"}}}
		}, "at most /20"},
		"no public ipv4 on a managed network": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Networking = &apicommon.NetworkingOverride{PublicIPv4: new(false)}
		}, "no NAT gateway"},
		"ipv6 on a managed network": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Networking.PublicIPv6 = new(true)
		}, "no IPv6 prefix"},
		"no public ipv4 on a brought subnet": {func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.OCI.Networking = &apicommon.NetworkingOverride{PublicIPv4: new(false)}
			nc.Spec.Vendors.OCI.Resources = &ociapi.Resources{Subnets: &ociapi.SubnetResource{Use: &apicommon.ResourceUse{
				IDs: map[string]apicommon.IDList{testRegion: {ocid("subnet")}}}}}
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			nc := class(nil, testRegion)
			tc.mutate(nc)
			err := d.ValidateNodeClass(t.Context(), nc)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error %v, want one with %q", err, tc.want)
			}
		})
	}
}

// ----- the instance types in every availability domain of the region -----

// A type is offered, and launchable, only in the ADs of the user's tenancy whose shape table has
// the shape: the pre-launch check against ListShapes of the AD decides (plan 25, section 6).
func TestShapeMissingInAnADIsUnsupportedWithoutALaunchCall(t *testing.T) {
	e := newEnv(t)
	e.c.RemoveShape(testRegion, "VM.Standard.E5.Flex", fake.ADFull(fake.SeedZoneB))
	req := request(testType)
	req.Zone = fake.SeedZoneB
	launches := len(e.c.Launches())
	_, err := testDriver{e.d}.CreateInstance(t.Context(), req)
	if !vendors.IsClass(err, vendors.Unsupported) || vendors.CodeOf(err) != driver.CodeShapeNotInAD || len(e.c.Launches()) != launches {
		t.Errorf("CreateInstance in an AD without the shape: %v (%d launches)", err, len(e.c.Launches())-launches)
	}
	req.Zone = testZone
	req.IdempotencyKey = "other-key"
	if _, err := (testDriver{e.d}).CreateInstance(t.Context(), req); err != nil {
		t.Errorf("CreateInstance in an AD with the shape: %v", err)
	}
}

// Images kept in another compartment are found by the default lookup and by the selectors
// only with the image compartment; without it they are not, and ids keep working either way.
func TestResolveImageInImageCompartment(t *testing.T) {
	const imageComp = "ocid1.compartment.oc1..aaaaimagecompartment"
	for _, use := range []vendors.UseRef{{}, {Name: "flatcar-stable-" + fake.SeedVersion + "-arm64"}} {
		c := newCloud(t)
		c.AddImageCompartment(imageComp)
		img := fake.FlatcarImage("stable", fake.SeedVersion, "arm64")
		img.CompartmentId = new(imageComp)
		id := c.AddImage(testRegion, img)

		without := restart(t, c, testCluster)
		if _, err := without.d.ResolveImage(t.Context(), imageReq("arm64", use)); !vendors.IsClass(err, vendors.NotFound) {
			t.Errorf("use %+v without the option: %v, want NotFound", use, err)
		}
		with := restart(t, c, testCluster, driver.WithImageCompartment(imageComp))
		got, err := with.d.ResolveImage(t.Context(), imageReq("arm64", use))
		if err != nil || got.Ref != id {
			t.Errorf("use %+v with the option: %+v, %v", use, got, err)
		}
		if with.d.ImageCompartment() != imageComp || with.d.Compartment() != fake.SeedCompartment {
			t.Errorf("compartments %q, %q", with.d.ImageCompartment(), with.d.Compartment())
		}
		byID, err := without.d.ResolveImage(t.Context(), imageReq("arm64", vendors.UseRef{IDs: []string{id}}))
		if err != nil || byID.Ref != id {
			t.Errorf("ids without the option: %+v, %v", byID, err)
		}
	}
}

func TestInitRejectsABadImageCompartment(t *testing.T) {
	d := driver.New(driver.WithCompartment(fake.SeedCompartment), driver.WithImageCompartment("images"))
	if err := d.Init(t.Context(), vendors.Config{}); err == nil || !strings.Contains(err.Error(), "image compartment") {
		t.Errorf("Init = %v, want an image compartment error", err)
	}
	// The root compartment (the tenancy's OCID) is a compartment images can live in.
	c := newCloud(t)
	root := restart(t, c, testCluster, driver.WithImageCompartment(fake.SeedTenancy))
	if root.d.ImageCompartment() != fake.SeedTenancy {
		t.Errorf("image compartment %q, want the tenancy", root.d.ImageCompartment())
	}
}

// An image resolved in the image compartment launches in the compartment of the nodes: only the
// image listing reads the image compartment.
func TestLaunchWithAnImageOfTheImageCompartment(t *testing.T) {
	const imageComp = "ocid1.compartment.oc1..aaaaimagecompartment"
	c := newCloud(t)
	c.AddImageCompartment(imageComp)
	img := fake.FlatcarImage("stable", "4800.0.0", "amd64")
	img.CompartmentId = new(imageComp)
	id := c.AddImage(testRegion, img)
	e := restart(t, c, testCluster, driver.WithImageCompartment(imageComp))
	req := imageReq("amd64", vendors.UseRef{})
	req.Version = "4800.0.0"
	got, err := e.d.ResolveImage(t.Context(), req)
	if err != nil || got.Ref != id {
		t.Fatalf("ResolveImage = %+v, %v; want %s", got, err, id)
	}
	cr := request(testType)
	cr.Image = *got
	if _, err := (testDriver{e.d}).CreateInstance(t.Context(), cr); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	l := c.Launches()
	last := l[len(l)-1].LaunchInstanceDetails
	src, ok := last.SourceDetails.(core.InstanceSourceViaImageDetails)
	if !ok || src.ImageId == nil || *src.ImageId != id || last.CompartmentId == nil || *last.CompartmentId != fake.SeedCompartment {
		t.Errorf("launch %+v, want the compartment %q and the image %q", last, fake.SeedCompartment, id)
	}
}
