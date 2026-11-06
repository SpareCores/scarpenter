//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// offeringOf is an offering of the catalog with its price, as the scheduler
// sees it.
type offeringOf struct {
	typeName, region, zone, capacityType string
	price                                float64
}

// offerings lists the available offerings of the types, cheapest first (the
// catalog's own order of equal prices is by name, zone, region and capacity
// type, like Create's).
func (e *env) offerings(types ...string) []offeringOf {
	e.t.Helper()
	its, err := e.cat.InstanceTypes(e.t.Context(), catalog.Query{Class: className})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []offeringOf
	for _, it := range its {
		if !slices.Contains(types, it.Name) {
			continue
		}
		for _, o := range it.Offerings {
			if o.Available {
				out = append(out, offeringOf{
					typeName: it.Name, region: o.Requirements.Get(corev1.LabelTopologyRegion).Any(), zone: o.Zone(),
					capacityType: o.CapacityType(), price: o.Price,
				})
			}
		}
	}
	slices.SortFunc(out, func(a, b offeringOf) int {
		return cmp.Or(cmp.Compare(a.price, b.price), cmp.Compare(a.typeName, b.typeName), cmp.Compare(a.zone, b.zone),
			cmp.Compare(a.capacityType, b.capacityType))
	})
	return out
}

func labelOf(nc *karpv1.NodeClaim, key string) string { return nc.Labels[key] }

func TestCreateReturnsTheNodeClaimOfTheCheapestOffering(t *testing.T) {
	e := newEnv(t, envOptions{})
	types := []string{"aws.m7i.large", "aws.c7i.large", "aws.m6i.large"}
	want := e.offerings(types...)
	if len(want) < 3 {
		t.Fatalf("the snapshot has too few offerings of %v: %v", types, want)
	}
	cheapest := want[0]

	nc := e.newClaim("claim-1", types)
	created, err := e.cp.Create(t.Context(), nc)
	if err != nil {
		t.Fatal(err)
	}
	if got := created.Status.ProviderID; got != scope.ProviderID("aws", cheapest.region, "claim-1") {
		t.Errorf("provider ID = %q", got)
	}
	for key, want := range map[string]string{
		corev1.LabelInstanceTypeStable: cheapest.typeName,
		corev1.LabelTopologyZone:       cheapest.zone,
		corev1.LabelTopologyRegion:     cheapest.region,
		karpv1.CapacityTypeLabelKey:    cheapest.capacityType,
		corev1.LabelArchStable:         "amd64",
		corev1.LabelOSStable:           "linux",
		corev1.LabelHostname:           "claim-1",
		v1alpha1.LabelVendor:           "aws",
		v1alpha1.LabelAPIReference:     strings.TrimPrefix(cheapest.typeName, "aws."),
		v1alpha1.LabelStorageProfile:   v1alpha1.DefaultProfile,
	} {
		if got := labelOf(created, key); got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
	if created.Labels[v1alpha1.LabelZoneID] == "" {
		t.Error("the zone-id label is missing")
	}
	ref := created.Annotations[v1alpha1.AnnotationInstanceRef]
	if !strings.HasPrefix(ref, cheapest.region+"/") {
		t.Errorf("instance-ref = %q, want a reference in %s", ref, cheapest.region)
	}
	if created.Annotations[v1alpha1.AnnotationPrice] == "" || created.Annotations[v1alpha1.AnnotationNodeClassHash] == "" {
		t.Errorf("annotations = %v", created.Annotations)
	}
	if created.Status.Capacity.Cpu().Value() != 2 || created.Status.Allocatable.Cpu().IsZero() {
		t.Errorf("capacity = %v, allocatable = %v", created.Status.Capacity, created.Status.Allocatable)
	}

	creates := e.calls("create")
	if len(creates) != 1 || creates[0].InstanceType != strings.TrimPrefix(cheapest.typeName, "aws.") ||
		creates[0].Zone != cheapest.zone || creates[0].Result != "ok" {
		t.Errorf("vendor calls = %+v, want one create of the cheapest offering %+v", creates, cheapest)
	}
	if !contains(e.tokens.minted, "claim-1") {
		t.Errorf("no token minted: %v", e.tokens.minted)
	}
}

func TestCreateIsIdempotentPerNodeClaim(t *testing.T) {
	e := newEnv(t, envOptions{})
	nc := e.newClaim("claim-1", []string{"aws.m7i.large"})
	first, err := e.cp.Create(t.Context(), nc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.cp.Create(t.Context(), nc)
	if err != nil {
		t.Fatal(err)
	}
	if first.Annotations[v1alpha1.AnnotationInstanceRef] != second.Annotations[v1alpha1.AnnotationInstanceRef] {
		t.Errorf("a second Create launched another instance: %v vs %v", first.Annotations, second.Annotations)
	}
}

func TestCreateFallsBackOverCapacityErrors(t *testing.T) {
	e := newEnv(t, envOptions{})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	offs := e.offerings(types...)
	first, second := offs[0], offs[1]
	e.setConfig("errors", fmt.Sprintf(`
- match: {instanceType: %s, zone: %s, capacityType: %s}
  error: InsufficientCapacity
`, strings.TrimPrefix(first.typeName, "aws."), first.zone, first.capacityType))

	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types))
	if err != nil {
		t.Fatal(err)
	}
	if labelOf(created, corev1.LabelInstanceTypeStable) != second.typeName || labelOf(created, corev1.LabelTopologyZone) != second.zone ||
		labelOf(created, karpv1.CapacityTypeLabelKey) != second.capacityType {
		t.Errorf("launched %v, want the second cheapest %+v after the first %+v failed", created.Labels, second, first)
	}
	calls := e.calls("create")
	if len(calls) != 2 || calls[0].Result != string(vendors.InsufficientCapacity) || calls[1].Result != "ok" {
		t.Errorf("vendor calls = %+v", calls)
	}
	if !e.un.IsUnavailable(unavailableOf(first)) {
		t.Errorf("the failed offering %+v was not marked unavailable", first)
	}
	// The next scheduling round does not see it.
	for _, o := range e.offerings(first.typeName) {
		if o.zone == first.zone && o.capacityType == first.capacityType {
			t.Errorf("the unavailable offering is still offered: %+v", o)
		}
	}
}

func TestCreateWithEveryAttemptFailingIsInsufficientCapacity(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 3})
	types := []string{"aws.m7i.large", "aws.c7i.large", "aws.m6i.large"}
	e.setConfig("errors", "- {error: InsufficientCapacity}")

	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types))
	if !karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Fatalf("Create error = %v, want InsufficientCapacityError", err)
	}
	if n := len(e.calls("create")); n != 3 {
		t.Errorf("%d vendor calls, want max-launch-attempts (3)", n)
	}
	if !contains(e.tokens.deleted, "claim-1") {
		t.Error("the token of the failed launch was not deleted")
	}
	// Three offerings are marked, the rest is still offered: the next round
	// picks another.
	if len(e.offerings(types...)) == 0 {
		t.Error("everything became unavailable after three attempts")
	}
}

func TestIceTTLExpiryMakesOfferingsAvailableAgain(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 1})
	types := []string{"aws.m7i.large"}
	before := len(e.offerings(types...))
	e.setConfig("errors", "- {error: InsufficientCapacity, count: 1}")
	if _, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types)); !karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Fatal(err)
	}
	if got := len(e.offerings(types...)); got != before-1 {
		t.Fatalf("%d offerings after the failure, want %d", got, before-1)
	}
	e.advance(2 * time.Minute)
	if got := len(e.offerings(types...)); got != before {
		t.Errorf("%d offerings after the TTL, want %d", got, before)
	}
}

func TestCreateMapsErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		class vendors.Class
		check func(error) bool
		event bool
	}{
		{vendors.Retryable, func(err error) bool {
			return err != nil && !karpcloudprovider.IsInsufficientCapacityError(err) && !isCreateError(err)
		}, false},
		{vendors.Fatal, isCreateError, true},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			e := newEnv(t, envOptions{})
			e.setConfig("errors", fmt.Sprintf("- {error: %s}", tc.class))
			_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
			if !tc.check(err) {
				t.Errorf("Create error = %v", err)
			}
			if n := len(e.calls("create")); n != 1 {
				t.Errorf("%d vendor calls, want 1: neither class is retried inside Create", n)
			}
			if tc.event != contains(e.events.reasons(), cloudprovider.EventLaunchFailed) {
				t.Errorf("events = %v", e.events.reasons())
			}
		})
	}
}

func isCreateError(err error) bool {
	var ce *karpcloudprovider.CreateError
	return errors.As(err, &ce)
}

func TestCreateMarksUnsupportedAndUnusableZones(t *testing.T) {
	for _, class := range []vendors.Class{vendors.Unsupported, vendors.ZoneUnusable} {
		t.Run(string(class), func(t *testing.T) {
			e := newEnv(t, envOptions{attempts: 1})
			target := e.offerings("aws.m7i.large")[0]
			e.setConfig("errors", fmt.Sprintf("- {error: %s, count: 1}", class))
			_, _ = e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
			if !e.un.IsUnavailable(unavailableOf(target)) {
				t.Errorf("%+v was not marked unavailable after %s", target, class)
			}
		})
	}
}

func TestCreateWithoutQuotaKnowledgeMarksTheRegionOnQuotaErrors(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 3})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	target := e.offerings(types...)[0]
	e.setConfig("errors", fmt.Sprintf("- match: {region: %s}\n  error: QuotaExceeded", target.region))

	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types))
	if err != nil {
		t.Fatal(err)
	}
	if labelOf(created, corev1.LabelTopologyRegion) == target.region {
		t.Errorf("launched in %s, which returned a quota error", target.region)
	}
	// One vendor call failed, the other candidates of the region and capacity
	// type were skipped without a call.
	creates := e.calls("create")
	failed := 0
	for _, c := range creates {
		if c.Result == string(vendors.QuotaExceeded) {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("vendor calls = %+v, want exactly one quota error", creates)
	}
	if !e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Error("the region and capacity type were not marked unavailable for the TTL")
	}
}

func TestQuotaExhaustedRegionIsNeverCalled(t *testing.T) {
	e := newEnv(t, envOptions{})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	offs := e.offerings(types...)
	cheapest := offs[0]
	e.setConfig("quota", exhausted(cheapest.region, cheapest.capacityType))
	if err := e.tracker.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The scheduler no longer sees the offerings of the exhausted bucket...
	for _, o := range e.offerings(types...) {
		if o.region == cheapest.region && o.capacityType == cheapest.capacityType {
			t.Fatalf("offering %+v of the exhausted bucket is still offered", o)
		}
	}
	// ...and a NodeClaim that may use the region still never reaches the vendor
	// there, even when the scheduler planned it before the refresh.
	nc := e.newClaim("claim-1", types)
	created, err := e.cp.Create(t.Context(), nc)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelOf(created, corev1.LabelTopologyRegion); got == cheapest.region && labelOf(created, karpv1.CapacityTypeLabelKey) == cheapest.capacityType {
		t.Errorf("launched in the exhausted bucket (%s %s)", got, cheapest.capacityType)
	}
	for _, c := range e.calls("create") {
		if c.Region == cheapest.region && c.CapacityType == cheapest.capacityType {
			t.Errorf("a vendor call went to the exhausted bucket: %+v", c)
		}
	}
}

// exhausted returns a quota document in which the bucket of the capacity type
// is full in the region and has plenty elsewhere.
func exhausted(region, capacityType string) string {
	return quotaWith(region, capacityType, 16, 16)
}

// quotaWith is standardQuota with the bucket of the capacity type limited in the
// region: limit vCPUs of which foreign are used by other workloads.
func quotaWith(region, capacityType string, limit, foreign int) string {
	od, spot := "{id: std-od, scope: region, unit: vCPU, limit: 100000}", "{id: std-spot, scope: region, unit: vCPU, limit: 100000}"
	override := fmt.Sprintf(", regions: {%s: {limit: %d, foreignUsed: %d}}}", region, limit, foreign)
	if capacityType == "spot" {
		spot = strings.TrimSuffix(spot, "}") + override
	} else {
		od = strings.TrimSuffix(od, "}") + override
	}
	return fmt.Sprintf(`
buckets:
  - %s
  - %s
consumption:
  - {families: ["*"], capacityTypes: [on-demand], buckets: [std-od]}
  - {families: ["*"], capacityTypes: [spot], buckets: [std-spot]}
`, od, spot)
}

func TestConcurrentCreatesShareTheLastHeadroomThroughTheLedger(t *testing.T) {
	e := newEnv(t, envOptions{})
	typeName := "aws.m7i.large" // 2 vCPUs
	cheapest := e.offerings(typeName)[0]
	// 4 vCPUs left in the cheapest bucket: two instances fit, a third must go
	// elsewhere without a quota error.
	e.setConfig("quota", quotaWith(cheapest.region, cheapest.capacityType, 16, 12))
	if err := e.tracker.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	const claims = 5
	var wg sync.WaitGroup
	results := make([]*karpv1.NodeClaim, claims)
	errs := make([]error, claims)
	for i := range claims {
		wg.Go(func() {
			results[i], errs[i] = e.cp.Create(t.Context(), e.newClaimSafe(fmt.Sprintf("claim-%d", i), []string{typeName}))
		})
	}
	wg.Wait()

	inCheapest := 0
	for i := range claims {
		if errs[i] != nil {
			t.Fatalf("claim %d: %v", i, errs[i])
		}
		if results[i].Labels[corev1.LabelTopologyRegion] == cheapest.region &&
			results[i].Labels[karpv1.CapacityTypeLabelKey] == cheapest.capacityType {
			inCheapest++
		}
	}
	if inCheapest != 2 {
		t.Errorf("%d of %d nodes in the bucket with room for 2", inCheapest, claims)
	}
	for _, c := range e.calls("create") {
		if c.Result != "ok" {
			t.Errorf("a vendor call failed: %+v; the ledger should have prevented every quota error", c)
		}
	}
}

func TestGetListAndDeleteFollowTheInstance(t *testing.T) {
	e := newEnv(t, envOptions{})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	providerID := created.Status.ProviderID

	got, err := e.cp.Get(t.Context(), providerID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "claim-1" || got.Status.ProviderID != providerID || got.Labels[corev1.LabelInstanceTypeStable] != "aws.m7i.large" {
		t.Errorf("Get = %+v", got)
	}
	listed, err := e.cp.List(t.Context())
	if err != nil || len(listed) != 1 || listed[0].Status.ProviderID != providerID {
		t.Errorf("List = %v, %v", listed, err)
	}

	claim := &karpv1.NodeClaim{}
	claim.Name = "claim-1"
	claim.Status.ProviderID = providerID
	claim.Annotations = created.Annotations
	// Delete terminates and says "deleted" with nil; core asks again.
	if err := e.cp.Delete(t.Context(), claim); err != nil {
		t.Fatalf("first Delete = %v, want nil", err)
	}
	if err := e.cp.Delete(t.Context(), claim); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("second Delete = %v, want NodeClaimNotFound", err)
	}
	if _, err := e.cp.Get(t.Context(), providerID); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("Get after Delete = %v, want NodeClaimNotFound", err)
	}
	if listed, _ := e.cp.List(t.Context()); len(listed) != 0 {
		t.Errorf("List after Delete = %v", listed)
	}
	if _, err := e.cp.Get(t.Context(), scope.ProviderID("aws", region1, "missing")); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("Get of an unknown instance = %v, want NodeClaimNotFound", err)
	}
	if _, err := e.cp.Get(t.Context(), "aws:///i-0abc"); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("Get of a foreign provider ID = %v, want NodeClaimNotFound", err)
	}
}

func TestDeleteFindsTheInstanceByTagWithoutTheAnnotation(t *testing.T) {
	e := newEnv(t, envOptions{})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	claim := &karpv1.NodeClaim{}
	claim.Name = "claim-1"
	claim.Status.ProviderID = created.Status.ProviderID
	if err := e.cp.Delete(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if got := e.calls("delete"); len(got) != 1 {
		t.Errorf("delete calls = %+v", got)
	}
}

func TestIsDriftedComparesTheHashOfTheVendor(t *testing.T) {
	e := newEnv(t, envOptions{})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	claim := e.newClaim("claim-1", []string{"aws.m7i.large"})
	claim.Annotations = created.Annotations
	claim.Status.ProviderID = created.Status.ProviderID

	if reason, err := e.cp.IsDrifted(t.Context(), claim); err != nil || reason != "" {
		t.Fatalf("IsDrifted = %q, %v before any change", reason, err)
	}
	nc := &v1alpha1.SCNodeClass{}
	if err := e.client.Get(t.Context(), clientKey(className), nc); err != nil {
		t.Fatal(err)
	}
	nc.Status.Vendors["aws"] = v1alpha1.VendorStatus{Hash: "changed", Conditions: nc.Status.Vendors["aws"].Conditions}
	if err := e.client.Status().Update(t.Context(), nc); err != nil {
		t.Fatal(err)
	}
	if reason, err := e.cp.IsDrifted(t.Context(), claim); err != nil || reason != cloudprovider.NodeClassDrift {
		t.Errorf("IsDrifted = %q, %v after a hash change, want NodeClassDrift", reason, err)
	}
	// A NodeClaim without the annotation (launched by an earlier version) is
	// not drifted.
	claim.Annotations = nil
	if reason, _ := e.cp.IsDrifted(t.Context(), claim); reason != "" {
		t.Errorf("IsDrifted = %q without the annotation", reason)
	}
}

func TestCreateRequiresAReadyNodeClass(t *testing.T) {
	t.Run("not ready", func(t *testing.T) {
		e := newEnv(t, envOptions{notReady: true})
		_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
		if err == nil || karpcloudprovider.IsInsufficientCapacityError(err) {
			t.Errorf("Create error = %v, want a not-ready error", err)
		}
		if n := len(e.calls("create")); n != 0 {
			t.Errorf("%d vendor calls for a NodeClass that is not ready", n)
		}
	})
	t.Run("vendor not ready", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		nc := &v1alpha1.SCNodeClass{}
		if err := e.client.Get(t.Context(), clientKey(className), nc); err != nil {
			t.Fatal(err)
		}
		nc.Status.Vendors["aws"] = v1alpha1.VendorStatus{}
		if err := e.client.Status().Update(t.Context(), nc); err != nil {
			t.Fatal(err)
		}
		_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
		if !karpcloudprovider.IsNodeClassNotReadyError(err) {
			t.Errorf("Create error = %v: a vendor that is not ready is NodeClassNotReady", err)
		}
		its, err := e.cp.GetInstanceTypes(t.Context(), nodePool())
		if err != nil || len(its) != 0 {
			t.Errorf("GetInstanceTypes = %d types, %v for a vendor that is not ready", len(its), err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		claim := e.newClaim("claim-1", []string{"aws.m7i.large"})
		claim.Spec.NodeClassRef.Name = "gone"
		if _, err := e.cp.Create(t.Context(), claim); !karpcloudprovider.IsInsufficientCapacityError(err) {
			t.Errorf("Create error = %v, want InsufficientCapacityError", err)
		}
		pool := nodePool()
		pool.Spec.Template.Spec.NodeClassRef.Name = "gone"
		if its, err := e.cp.GetInstanceTypes(t.Context(), pool); err != nil || its != nil {
			t.Errorf("GetInstanceTypes = %v, %v for a missing NodeClass", its, err)
		}
	})
}

func TestGetInstanceTypesFiltersByTheNodePoolRequirements(t *testing.T) {
	e := newEnv(t, envOptions{})
	pool := nodePool()
	all, err := e.cp.GetInstanceTypes(t.Context(), pool)
	if err != nil || len(all) < 100 {
		t.Fatalf("GetInstanceTypes = %d types, %v", len(all), err)
	}
	pool.Spec.Template.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		req(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "aws.m7i.large"),
		req(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region2),
		req(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, "on-demand"),
	}
	its, err := e.cp.GetInstanceTypes(t.Context(), pool)
	if err != nil || len(its) != 1 {
		t.Fatalf("GetInstanceTypes = %d types, %v, want the one type", len(its), err)
	}
	for _, o := range its[0].Offerings {
		if o.Requirements.Get(corev1.LabelTopologyRegion).Any() != region2 || o.CapacityType() != "on-demand" {
			t.Errorf("offering %v escapes the pre-filter", o.Requirements)
		}
	}
}

func TestCandidateOrder(t *testing.T) {
	// Equal prices are ordered by larger headroom, type name, zone: the order
	// must not depend on map iteration.
	e := newEnv(t, envOptions{})
	names := []string{"aws.m7i.large", "aws.m7i.xlarge"}
	first := e.offerings(names...)
	for range 3 {
		again := e.offerings(names...)
		if !slices.Equal(first, again) {
			t.Fatal("the offering order is not deterministic")
		}
	}
	_ = catalog.Ref{}
}

func TestQuotaErrorWithAStaleViewIsCachedReactively(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 1})
	typeName := "aws.m7i.large"
	target := e.offerings(typeName)[0]
	e.setConfig("quota", quotaWith(target.region, target.capacityType, 100000, 0))
	if err := e.tracker.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	// No refresh for longer than the staleness limit: the region is unknown.
	e.advance(20 * time.Minute)
	e.setConfig("errors", fmt.Sprintf("- match: {region: %s}\n  error: QuotaExceeded", target.region))
	if _, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{typeName})); err == nil {
		t.Fatal("expected the launch to fail")
	}
	if !e.un.IsUnavailable(unavailableOfRegion(target)) {
		t.Error("a quota error with an unknown quota view did not mark the region and capacity type unavailable")
	}
}

func TestRetryableErrorsKeepTheBootstrapToken(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.setConfig("errors", "- {error: Retryable, count: 1}")
	if _, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"})); err == nil {
		t.Fatal("expected a retryable error")
	}
	if contains(e.tokens.deleted, "claim-1") {
		t.Error("the token was deleted although the instance may exist")
	}
	e.setConfig("errors", "- {error: Fatal, count: 1}")
	_, _ = e.cp.Create(t.Context(), e.newClaim("claim-2", []string{"aws.m7i.large"}))
	if !contains(e.tokens.deleted, "claim-2") {
		t.Error("the token of a failed launch was not deleted")
	}
}

func TestReplayedIdempotencyKeyDescribesTheOriginalInstance(t *testing.T) {
	e := newEnv(t, envOptions{})
	typeName := "aws.m7i.large"
	claim := e.newClaim("claim-1", []string{typeName})
	first, err := e.cp.Create(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	// The offering of the first launch is no longer a candidate, so the retry
	// attempts another one; the driver returns the original instance.
	e.un.MarkInsufficientCapacity("aws", typeName, first.Labels[corev1.LabelTopologyZone], first.Labels[karpv1.CapacityTypeLabelKey])
	second, err := e.cp.Create(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{corev1.LabelTopologyZone, corev1.LabelTopologyRegion, karpv1.CapacityTypeLabelKey, corev1.LabelInstanceTypeStable} {
		if first.Labels[key] != second.Labels[key] {
			t.Errorf("label %s = %q after the replay, want the original %q", key, second.Labels[key], first.Labels[key])
		}
	}
	if first.Annotations[v1alpha1.AnnotationInstanceRef] != second.Annotations[v1alpha1.AnnotationInstanceRef] ||
		first.Status.ProviderID != second.Status.ProviderID {
		t.Errorf("the replay describes another instance: %v vs %v", first.Annotations, second.Annotations)
	}
}
