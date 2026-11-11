//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
)

const testType = "aws.m7i.large"

func qty(s string) resource.Quantity { return resource.MustParse(s) }

// withProfiles adds the profiles of these tests to the NodeClass.
func withProfiles(nc *v1alpha1.SCNodeClass) {
	nc.Spec.Profiles = v1alpha1.Profiles{
		Storage: map[string]v1alpha1.StorageProfile{
			"db": {AWS: &aws.Storage{Data: []aws.DataVolume{{
				GuestVolume: common.GuestVolume{Name: "pgdata", MountPath: "/var/lib/pgdata", Filesystem: common.FilesystemXFS},
				VolumeType:  aws.VolumeTypeGP3, VolumeSize: qty("100Gi"),
			}}}},
			"provisioned": {AWS: &aws.Storage{Root: &aws.RootVolume{VolumeType: aws.VolumeTypeGP3, IOPS: new(int32(4000))}}},
			"qemu-only":   {QEMU: &qemu.Storage{Root: &qemu.RootDisk{Size: new(qty("10Gi"))}}},
		},
		Placement: map[string]v1alpha1.PlacementProfile{
			"colocated": {AWS: &aws.Placement{PlacementGroup: &aws.PlacementGroup{Strategy: "cluster"}}},
		},
	}
}

func with(reqs ...karpv1.NodeSelectorRequirementWithMinValues) claimOption {
	return func(nc *karpv1.NodeClaim) { nc.Spec.Requirements = append(nc.Spec.Requirements, reqs...) }
}

func in(key string, values ...string) karpv1.NodeSelectorRequirementWithMinValues {
	return req(key, corev1.NodeSelectorOpIn, values...)
}

// fakeInstance is an instance of the fake driver's state ConfigMap.
type fakeInstance struct {
	Ref       string            `json:"ref"`
	NodeClaim string            `json:"nodeClaim"`
	State     string            `json:"state"`
	Settings  string            `json:"settings"`
	Profiles  map[string]string `json:"profiles"`
	Resources []string          `json:"resources"`
	Devices   []string          `json:"devices"`
}

func (e *env) instanceOf(claim string) fakeInstance {
	e.t.Helper()
	if err := e.driver.Flush(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := e.client.Get(e.t.Context(), client.ObjectKey{Namespace: cmNS, Name: "scarpenter-fake-state"}, cm); err != nil {
		e.t.Fatal(err)
	}
	var list []fakeInstance
	if err := json.Unmarshal([]byte(cm.Data["instances"]), &list); err != nil {
		e.t.Fatal(err)
	}
	for _, i := range list {
		if i.NodeClaim == claim && i.State != "terminated" {
			return i
		}
	}
	e.t.Fatalf("the fake has no live instance of %s: %+v", claim, list)
	return fakeInstance{}
}

func (e *env) catalogPrice(created *karpv1.NodeClaim) catalog.OfferingPrice {
	e.t.Helper()
	p, ok := e.cat.Price(className, created.Labels[corev1.LabelInstanceTypeStable], created.Labels[corev1.LabelTopologyRegion],
		created.Labels[corev1.LabelTopologyZone], created.Labels[karpv1.CapacityTypeLabelKey])
	if !ok {
		e.t.Fatalf("no catalog price for %v", created.Labels)
	}
	return p
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCreateAppliesTheSelectedProfile(t *testing.T) {
	e := newEnv(t, envOptions{transform: withProfiles})
	claim := e.newClaim("claim-db", []string{testType}, with(in(v1alpha1.LabelStorageProfile, "db")))
	created, err := e.cp.Create(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		v1alpha1.LabelStorageProfile: "db", v1alpha1.LabelNetworkProfile: "default", v1alpha1.LabelPlacementProfile: "default",
	} {
		if created.Labels[key] != want {
			t.Errorf("label %s = %q, want %q", key, created.Labels[key], want)
		}
	}
	// The vendor call got the resolved settings: the default root and the
	// profile's extra volume; its resources were prepared first.
	inst := e.instanceOf("claim-db")
	if inst.Profiles["storage"] != "db" || inst.Profiles["placement"] != "default" {
		t.Errorf("applied profiles = %v", inst.Profiles)
	}
	if !strings.Contains(inst.Settings, "pgdata") || !strings.Contains(inst.Settings, "64Gi") {
		t.Errorf("resolved settings %s: want the profile's volume on top of the default root", inst.Settings)
	}
	if len(inst.Resources) != 2 {
		t.Errorf("prepared resources %v, want the regional network and the volume", inst.Resources)
	}
	if len(inst.Devices) != 1 || !strings.HasPrefix(inst.Devices[0], "pgdata=/dev/disk/by-id/fake-") ||
		!strings.Contains(inst.Devices[0], "xfs /var/lib/pgdata") {
		t.Errorf("device hints %v", inst.Devices)
	}
	// The user-data is rendered for the attempt, with the hints and the
	// type's kube-reserved (carry-over of plan 05).
	rr := e.renderer.last()
	if len(rr.Volumes) != 1 || rr.Volumes[0].Name != "pgdata" || rr.Volumes[0].ByID == "" {
		t.Errorf("volumes of the render request: %+v", rr.Volumes)
	}
	if rr.InstanceType != strings.TrimPrefix(testType, "aws.") || rr.Zone != created.Labels[corev1.LabelTopologyZone] ||
		rr.KubeReserved["cpu"] == "" || rr.KubeReserved["memory"] == "" {
		t.Errorf("render request %+v lacks the offering or the kube-reserved", rr)
	}
	// The price has the volumes' delta (100 GiB at 0.10 per GiB-month).
	cp := e.catalogPrice(created)
	price, _ := strconv.ParseFloat(created.Annotations[v1alpha1.AnnotationPrice], 64)
	if want := cp.Base + 100*0.10/730; !near(price, want) {
		t.Errorf("price annotation %v, want %v (instance %v plus the volume)", price, want, cp.Base)
	}
	if _, set := created.Annotations[v1alpha1.AnnotationPriceIncomplete]; set {
		t.Error("a complete price was flagged incomplete")
	}
	var used map[string][]string
	if err := json.Unmarshal([]byte(created.Annotations[v1alpha1.AnnotationResources]), &used); err != nil ||
		len(used["subnets"]) != 1 || len(used["volumes"]) != 1 {
		t.Errorf("resources annotation %q: want the IDs of the network and the volume the node launched with",
			created.Annotations[v1alpha1.AnnotationResources])
	}
	var hashes map[string]string
	if err := json.Unmarshal([]byte(created.Annotations[v1alpha1.AnnotationProfileHashes]), &hashes); err != nil || hashes["storage/db"] == "" {
		t.Errorf("profile hashes annotation %q", created.Annotations[v1alpha1.AnnotationProfileHashes])
	}
}

func TestCreateWithoutAProfileIsTheDefaultRoute(t *testing.T) {
	e := newEnv(t, envOptions{transform: withProfiles})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{testType}))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{v1alpha1.LabelStorageProfile, v1alpha1.LabelNetworkProfile, v1alpha1.LabelPlacementProfile} {
		if created.Labels[key] != v1alpha1.DefaultProfile {
			t.Errorf("%s = %q", key, created.Labels[key])
		}
	}
	if _, set := created.Annotations[v1alpha1.AnnotationProfileHashes]; set {
		t.Error("the default route carries profile hashes")
	}
	price, _ := strconv.ParseFloat(created.Annotations[v1alpha1.AnnotationPrice], 64)
	if cp := e.catalogPrice(created); !near(price, cp.USD) {
		t.Errorf("price annotation %v, want the catalog's %v", price, cp.USD)
	}
	inst := e.instanceOf("claim-1")
	if len(inst.Devices) != 0 || strings.Contains(inst.Settings, "pgdata") || len(inst.Resources) != 1 {
		t.Errorf("default route instance: %+v", inst)
	}
}

func TestSelectedProfilesOrderTheCandidatesByTheirPrice(t *testing.T) {
	// The cheaper type of the two with the default settings stays cheaper with
	// the same volume on both, but the order is by price plus the delta: two
	// offerings of one type differ only by their instance price.
	e := newEnv(t, envOptions{transform: withProfiles})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types, with(in(v1alpha1.LabelStorageProfile, "db"))))
	if err != nil {
		t.Fatal(err)
	}
	want := e.offerings(types...)[0]
	if created.Labels[corev1.LabelInstanceTypeStable] != want.typeName || created.Labels[corev1.LabelTopologyZone] != want.zone {
		t.Errorf("launched %v, the cheapest offering is %+v", created.Labels, want)
	}
}

func TestIncompletePricesAreFlaggedAndCanBeExcluded(t *testing.T) {
	e := newEnv(t, envOptions{transform: withProfiles})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{testType}, with(in(v1alpha1.LabelStorageProfile, "provisioned"))))
	if err != nil {
		t.Fatal(err)
	}
	if created.Annotations[v1alpha1.AnnotationPriceIncomplete] != "true" {
		t.Errorf("provisioned IOPS are not priced: the annotation must say so, got %v", created.Annotations)
	}

	// pricing.allowIncomplete: false takes such offerings out of the catalog, so
	// the scheduler never offers them and Create finds no candidate.
	strict := newEnv(t, envOptions{transform: func(nc *v1alpha1.SCNodeClass) {
		withProfiles(nc)
		nc.Spec.Pricing.AllowIncomplete = new(false)
	}})
	_, err = strict.cp.Create(t.Context(), strict.newClaim("claim-2", []string{testType}, with(in(v1alpha1.LabelStorageProfile, "provisioned"))))
	if !karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Errorf("Create = %v, want InsufficientCapacityError", err)
	}
	if n := len(strict.calls("create")); n != 0 {
		t.Errorf("%d vendor calls for an excluded offering", n)
	}
	// The complete profile and the default route are unaffected.
	if _, err := strict.cp.Create(t.Context(), strict.newClaim("claim-3", []string{testType}, with(in(v1alpha1.LabelStorageProfile, "db")))); err != nil {
		t.Errorf("a complete profile with allowIncomplete=false: %v", err)
	}
}

func TestAProfileWithoutABlockForTheVendorDoesNotExist(t *testing.T) {
	e := newEnv(t, envOptions{transform: withProfiles})
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{testType}, with(in(v1alpha1.LabelStorageProfile, "qemu-only"))))
	if !karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Errorf("Create = %v, want InsufficientCapacityError: no type can host a profile the vendor has no block for", err)
	}
	if n := len(e.calls("create")); n != 0 {
		t.Errorf("%d vendor calls", n)
	}
	for _, it := range func() []*karpcloudprovider.InstanceType {
		its, _ := e.cat.InstanceTypes(t.Context(), catalog.Query{Class: className})
		return its
	}() {
		if r := it.Requirements.Get(v1alpha1.LabelStorageProfile); r.Has("qemu-only") {
			t.Fatalf("%s defines the profile qemu-only on aws", it.Name)
		}
	}
}

func TestPlacementGroupsAreSharedPerRunKey(t *testing.T) {
	e := newEnv(t, envOptions{transform: withProfiles})
	claim := func(name, runKey string) *karpv1.NodeClaim {
		return e.newClaim(name, []string{testType}, with(
			in(v1alpha1.LabelPlacementProfile, "colocated"), in(v1alpha1.LabelPlacementGroup, runKey),
			in(corev1.LabelTopologyRegion, region1)))
	}
	a, err := e.cp.Create(t.Context(), claim("claim-a", "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cp.Create(t.Context(), claim("claim-b", "run-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cp.Create(t.Context(), claim("claim-c", "run-2")); err != nil {
		t.Fatal(err)
	}
	group := func(i fakeInstance) []string {
		return slices.DeleteFunc(slices.Clone(i.Resources), func(id string) bool { return !strings.Contains(id, "placementgroups") })
	}
	ga, gb, gc := group(e.instanceOf("claim-a")), group(e.instanceOf("claim-b")), group(e.instanceOf("claim-c"))
	if len(ga) != 1 || !slices.Equal(ga, gb) {
		t.Errorf("one run key must share one group: %v %v", ga, gb)
	}
	if len(gc) != 1 || slices.Equal(ga, gc) {
		t.Errorf("another run key must get another group: %v %v", ga, gc)
	}
	if a.Labels[v1alpha1.LabelPlacementGroup] != "run-1" || a.Labels[v1alpha1.LabelPlacementProfile] != "colocated" {
		t.Errorf("labels %v: the run key and the profile must be stamped on the node", a.Labels)
	}
}

func TestAPlacementProfileWithAGroupNeedsARunKey(t *testing.T) {
	e := newEnv(t, envOptions{transform: withProfiles})
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{testType}, with(in(v1alpha1.LabelPlacementProfile, "colocated"))))
	if err == nil || karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Fatalf("Create = %v, want a CreateError that says what is missing", err)
	}
	if !strings.Contains(err.Error(), "run key") {
		t.Errorf("error %q does not name the run key", err)
	}
	if n := len(e.calls("create")); n != 0 {
		t.Errorf("%d vendor calls before the resources were prepared", n)
	}
	if !contains(e.events.reasons(), "LaunchFailed") {
		t.Errorf("events %v", e.events.reasons())
	}
	// An ambiguous run key is rejected before anything is launched.
	_, err = e.cp.Create(t.Context(), e.newClaim("claim-2", []string{testType}, with(in(v1alpha1.LabelPlacementGroup, "a", "b"))))
	if err == nil || !strings.Contains(err.Error(), "exactly one value") {
		t.Errorf("two run keys: %v", err)
	}
}

func TestMissingUseResourceSkipsTheRegionWithoutMarkingTypes(t *testing.T) {
	e := newEnv(t, envOptions{transform: func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = &aws.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{
			Tags: map[string]string{"use": "true"}}}}
	}})
	e.setConfig("existingResources", "- {kind: subnets, region: "+region1+", id: subnet-a, tags: {use: \"true\"}}")
	// Nothing is wrong with the type or the zone: only region2 lacks the subnet.
	claim := e.newClaim("claim-1", []string{testType})
	created, err := e.cp.Create(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever region was chosen, a launch in it used the existing subnet.
	if got := created.Labels[corev1.LabelTopologyRegion]; got == region2 {
		inst := e.instanceOf("claim-1")
		t.Errorf("launched in %s without the subnet: %+v", got, inst)
	}
	inst := e.instanceOf("claim-1")
	if !slices.Contains(inst.Resources, "subnet-a") {
		t.Errorf("resources %v: the used subnet must be attached", inst.Resources)
	}
	if n := e.un.Len(); n != 0 {
		t.Errorf("%d unavailable-offering marks: a missing resource must not mark types or zones", n)
	}
}

func TestIsDriftedFollowsOnlyTheProfilesTheNodeUses(t *testing.T) {
	e := newEnv(t, envOptions{transform: withProfiles})
	db, err := e.cp.Create(t.Context(), e.newClaim("claim-db", []string{testType}, with(in(v1alpha1.LabelStorageProfile, "db"))))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := e.cp.Create(t.Context(), e.newClaim("claim-plain", []string{testType}))
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.cp.Create(t.Context(), e.newClaim("claim-prov", []string{testType}, with(in(v1alpha1.LabelStorageProfile, "provisioned"))))
	if err != nil {
		t.Fatal(err)
	}
	drifted := func(created *karpv1.NodeClaim) karpcloudprovider.DriftReason {
		t.Helper()
		nc := &karpv1.NodeClaim{}
		nc.Annotations = created.Annotations
		nc.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: className}
		nc.Status.ProviderID = created.Status.ProviderID
		reason, err := e.cp.IsDrifted(t.Context(), nc)
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}
	for name, c := range map[string]*karpv1.NodeClaim{"db": db, "plain": plain, "provisioned": other} {
		if got := drifted(c); got != "" {
			t.Errorf("%s drifted before any change: %q", name, got)
		}
	}
	// Edit the db profile only.
	cur := &v1alpha1.SCNodeClass{}
	if err := e.client.Get(t.Context(), clientKey(className), cur); err != nil {
		t.Fatal(err)
	}
	prof := cur.Spec.Profiles.Storage["db"]
	prof.AWS.Data[0].VolumeSize = qty("200Gi")
	cur.Spec.Profiles.Storage["db"] = prof
	if err := e.client.Update(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	if got := drifted(db); got != cloudprovider.NodeClassDrift {
		t.Errorf("the node of the edited profile: %q, want NodeClassDrift", got)
	}
	for name, c := range map[string]*karpv1.NodeClaim{"plain": plain, "provisioned": other} {
		if got := drifted(c); got != "" {
			t.Errorf("%s drifted although its profile did not change: %q", name, got)
		}
	}
	// A removed profile drifts the nodes that used it.
	delete(cur.Spec.Profiles.Storage, "provisioned")
	if err := e.client.Update(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	if got := drifted(other); got != cloudprovider.NodeClassDrift {
		t.Errorf("a removed profile: %q, want NodeClassDrift", got)
	}
	if got := drifted(plain); got != "" {
		t.Errorf("the default route drifted: %q", got)
	}
}

func TestFallbackPathRecordsThePrice(t *testing.T) {
	e := newEnv(t, envOptions{})
	typeName := testType
	claim := e.newClaim("claim-1", []string{typeName})
	first, err := e.cp.Create(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	// Every candidate offering of the retry is unavailable except one that is
	// not the first instance's: the instance matches none of the candidates.
	for _, o := range e.offerings(typeName) {
		if o.zone != first.Labels[corev1.LabelTopologyZone] || o.capacityType != first.Labels[karpv1.CapacityTypeLabelKey] {
			continue
		}
		e.un.MarkInsufficientCapacity("aws", typeName, o.zone, o.capacityType)
	}
	retry := e.newClaim("claim-1", []string{typeName})
	retry.UID = claim.UID
	second, err := e.cp.Create(t.Context(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if second.Annotations[v1alpha1.AnnotationInstanceRef] != first.Annotations[v1alpha1.AnnotationInstanceRef] {
		t.Fatalf("the replay describes another instance")
	}
	price, err := strconv.ParseFloat(second.Annotations[v1alpha1.AnnotationPrice], 64)
	if err != nil || price <= 0 {
		t.Errorf("the NodeClaim described from the instance has no price annotation: %v", second.Annotations)
	}
	if cp := e.catalogPrice(second); !near(price, cp.USD) {
		t.Errorf("price %v, want the catalog's %v", price, cp.USD)
	}
}

func TestReplayedKeyMovesTheQuotaReservationToTheReturnedInstance(t *testing.T) {
	e := newEnv(t, envOptions{})
	big, small := "aws.m7i.2xlarge", "aws.m7i.large" // 8 and 2 vCPUs
	target := e.offerings(big)[0]
	e.setConfig("quota", quotaWith(target.region, target.capacityType, 1000, 0))
	if err := e.tracker.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	claim := e.newClaim("claim-1", []string{big},
		with(in(corev1.LabelTopologyRegion, target.region), in(karpv1.CapacityTypeLabelKey, target.capacityType)))
	first, err := e.cp.Create(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	reserved := func() int64 {
		var sum int64
		for _, b := range e.tracker.Buckets() {
			if b.Region == target.region {
				sum += b.Reserved
			}
		}
		return sum
	}
	if got := reserved(); got != 8 {
		t.Fatalf("after the first launch %d vCPUs are reserved, want 8", got)
	}
	// The NodeClaim is created again with the same UID but now allows only the
	// small type: the driver replays the first instance (the big one).
	retry := e.newClaim("claim-1", []string{small},
		with(in(corev1.LabelTopologyRegion, target.region), in(karpv1.CapacityTypeLabelKey, target.capacityType)))
	retry.UID = claim.UID
	second, err := e.cp.Create(t.Context(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if second.Annotations[v1alpha1.AnnotationInstanceRef] != first.Annotations[v1alpha1.AnnotationInstanceRef] {
		t.Fatal("the replay launched another instance")
	}
	// The attempted small offering's reservation is gone, the instance's is
	// held once: not 8 + 2, and not 2.
	if got := reserved(); got != 8 {
		t.Errorf("after the replay %d vCPUs are reserved, want the returned instance's 8 (once)", got)
	}
}

func TestAZoneWithoutItsResourceIsSkippedNotTheRegion(t *testing.T) {
	e := newEnv(t, envOptions{transform: func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = &aws.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{Name: "sub-{zone}"}}}
	}})
	cheapest := e.offerings(testType)[0]
	var lines []string
	zones := map[string]bool{}
	for _, o := range e.offerings(testType) {
		if o.region == cheapest.region && o.zone != cheapest.zone && !zones[o.zone] {
			zones[o.zone] = true
			lines = append(lines, "- {kind: subnets, region: "+o.region+", id: s-"+o.zone+", name: sub-"+o.zone+"}")
		}
	}
	if len(lines) == 0 {
		t.Skip("the cheapest region has one zone")
	}
	e.setConfig("existingResources", strings.Join(lines, "\n"))
	// The cheapest zone has no subnet: the launch must land in another zone of
	// the same region, not fail the region.
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{testType},
		with(in(corev1.LabelTopologyRegion, cheapest.region), in(karpv1.CapacityTypeLabelKey, cheapest.capacityType))))
	if err != nil {
		t.Fatalf("Create = %v: the zones with a subnet were not tried", err)
	}
	if got := created.Labels[corev1.LabelTopologyZone]; got == cheapest.zone || !zones[got] {
		t.Errorf("launched in %s, want a zone with a subnet (not %s)", got, cheapest.zone)
	}
	if n := e.un.Len(); n != 0 {
		t.Errorf("%d unavailable-offering marks: a missing zone resource must not mark types or zones", n)
	}
}
