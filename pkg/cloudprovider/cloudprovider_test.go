//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package cloudprovider_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testingclock "k8s.io/utils/clock/testing"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/cloudprovider"
	"github.com/SpareCores/scarpenter/pkg/scope"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/facts"
	fakedriver "github.com/SpareCores/scarpenter/pkg/vendors/fake"
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
	if !strings.HasPrefix(created.Annotations[v1alpha1.AnnotationPublicIPv4], "203.0.113.") ||
		created.Annotations[v1alpha1.AnnotationPublicIPv6] != "" {
		t.Errorf("public address annotations = %q, %q; want the instance's IPv4 and no IPv6",
			created.Annotations[v1alpha1.AnnotationPublicIPv4], created.Annotations[v1alpha1.AnnotationPublicIPv6])
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
			return err != nil && !karpcloudprovider.IsInsufficientCapacityError(err) && isCreateError(err)
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

// An InvalidOffering of one type is marked for the offerings tried (type, zone,
// capacity type) and another type launches in the same Create, with an Event that
// carries the vendor's message.
func TestCreateMarksAnInvalidOfferingAndMovesOn(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 100})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	// The cheapest offering is tried first (offerings are sorted by price): make
	// its type the invalid one.
	bad, good := e.offerings(types...)[0].typeName, types[0]
	if bad == good {
		good = types[1]
	}
	before := len(e.offerings(bad))
	e.setConfig("errors", "- match: {instanceType: "+bad+"}\n  error: InvalidOffering")
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types))
	if err != nil {
		t.Fatalf("Create: %v, want the other type to launch", err)
	}
	if got := created.Labels[corev1.LabelInstanceTypeStable]; got != good {
		t.Errorf("launched %q, want %s", got, good)
	}
	if after := len(e.offerings(bad)); after >= before {
		t.Errorf("%d offerings of %s are available, were %d: none was marked", after, bad, before)
	}
	if !contains(e.events.reasons(), cloudprovider.EventOfferingRejected) {
		t.Errorf("events = %v, want %s", e.events.reasons(), cloudprovider.EventOfferingRejected)
	}
}

// A Create in which every attempt, of several types, was an InvalidOffering marks
// every offering it tried and is InsufficientCapacity, so that core drops the
// NodeClaim and the next one picks other types. Treating it as a global
// misconfiguration (Fatal) would retry the same types forever when the types
// share the cause (two sizes of one flexible shape).
func TestCreateWhenEveryTypeIsInvalidMarksThemAll(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 100})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	offerings := e.offerings(types...)
	e.setConfig("errors", "- {error: InvalidOffering}")
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types))
	if !karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Fatalf("Create error = %v, want InsufficientCapacity", err)
	}
	for _, o := range offerings {
		if !e.un.IsUnavailable(unavailableOf(o)) {
			t.Errorf("%+v was not marked", o)
		}
	}
	if left := e.offerings(types...); len(left) != 0 {
		t.Errorf("%d offerings are still available", len(left))
	}
}

// Create returns the vendor's billing start as an annotation, which core copies
// to the NodeClaim and the Node; Get and List return it too, so a NodeClaim
// launched before the annotation existed can be given it.
func TestCreateReturnsTheBillingStart(t *testing.T) {
	e := newEnv(t, envOptions{config: map[string]string{"billing": "stampOffset: -30s\nverified: true\nincrement: 1h\n"}})
	before := time.Now()
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	raw := created.Annotations[v1alpha1.AnnotationBillingStart]
	start, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("billing-start %q: %v", raw, err)
	}
	if start.After(before.Add(-29*time.Second)) || start.Before(before.Add(-32*time.Second)) {
		t.Errorf("billing start %s, want about 30 s before %s", start, before)
	}
	got, err := e.cp.Get(t.Context(), created.Status.ProviderID)
	if err != nil || got.Annotations[v1alpha1.AnnotationBillingStart] != raw {
		t.Errorf("Get: %v, %v; want the same billing start %s", got, err, raw)
	}
	list, err := e.cp.List(t.Context())
	if err != nil || len(list) != 1 || list[0].Annotations[v1alpha1.AnnotationBillingStart] != raw {
		t.Errorf("List: %v, %v", list, err)
	}
}

func TestCreateWithoutABillingStartSetsNoAnnotation(t *testing.T) {
	e := newEnv(t, envOptions{config: map[string]string{"billing": "noStamp: true\n"}})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	if v, set := created.Annotations[v1alpha1.AnnotationBillingStart]; set {
		t.Errorf("a vendor without a stamp must not get an annotation, got %q", v)
	}
}

// A launch refused for quota, by the vendor or by the tracker, tells the hook
// which scope it was and how many vCPUs it wanted.
func TestQuotaRefusalIsReported(t *testing.T) {
	var mu sync.Mutex
	var got []cloudprovider.QuotaRefusal
	e := newEnv(t, envOptions{attempts: 3, onQuotaRefusal: func(r cloudprovider.QuotaRefusal) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
	}})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	target := e.offerings(types...)[0]
	e.setConfig("errors", fmt.Sprintf("- match: {region: %s}\n  error: QuotaExceeded", target.region))
	if _, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no refusal reported")
	}
	if r := got[0]; r.Vendor != "aws" || r.Region != target.region || r.CapacityType != target.capacityType ||
		r.NodeClass != className || r.CPU != 2 {
		t.Errorf("refusal %+v, want aws %s %s of %s, 2 vCPU", r, target.region, target.capacityType, className)
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

// A quota error of a vendor whose limits are account-wide gates the capacity type
// in every region at once: no region is called a second time, and every region is
// marked unavailable for the TTL.
func TestAccountWideQuotaErrorGatesEveryRegion(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 5})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	offs := e.offerings(types...)
	first := offs[0]
	e.setConfig("errors", fmt.Sprintf("- {match: {capacityType: %s}, error: QuotaExceeded, accountWide: true}", first.capacityType))

	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types))
	failed := 0
	for _, c := range e.calls("create") {
		if c.Result == string(vendors.QuotaExceeded) {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("vendor calls = %+v, want exactly one quota error before every region of the capacity type is skipped",
			e.calls("create"))
	}
	for _, o := range offs {
		if o.capacityType == first.capacityType && !e.un.IsUnavailable(unavailableOf(o)) {
			t.Errorf("%+v is still available after an account-wide quota error", o)
		}
	}
	if err == nil {
		if got := e.calls("create"); got[len(got)-1].CapacityType == first.capacityType {
			t.Errorf("the launch used the gated capacity type: %+v", got)
		}
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

// A powered-off instance is terminating for ever: it is invisible to Get and
// List, and Delete deletes it at once: only an instance whose delete SCarpenter
// has issued is left to the vendor for the terminating grace.
func TestDeleteDeletesAStoppedInstanceAtOnce(t *testing.T) {
	e := newEnv(t, envOptions{})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	ref := vendors.InstanceRef(created.Annotations[v1alpha1.AnnotationInstanceRef])
	if !e.driver.PowerOff(ref) {
		t.Fatalf("no instance %s to power off", ref)
	}
	if _, err := e.cp.Get(t.Context(), created.Status.ProviderID); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("Get of a stopped instance = %v, want NodeClaimNotFound", err)
	}
	if listed, _ := e.cp.List(t.Context()); len(listed) != 0 {
		t.Errorf("List = %v, want no stopped instances", listed)
	}

	claim := &karpv1.NodeClaim{}
	claim.Name = "claim-1"
	claim.Status.ProviderID = created.Status.ProviderID
	claim.Annotations = created.Annotations
	if err := e.cp.Delete(t.Context(), claim); err != nil {
		t.Fatalf("Delete of a stopped instance = %v, want nil", err)
	}
	if got := e.calls("delete"); len(got) != 1 {
		t.Fatalf("delete calls %+v, want one at once", got)
	}
	if err := e.cp.Delete(t.Context(), claim); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
		t.Errorf("Delete of the deleted instance = %v, want NodeClaimNotFound", err)
	}
}

// slowDeleter is a driver whose delete is accepted but leaves the instance to
// the vendor: it stays as it is, and the test powers it off.
type slowDeleter struct {
	*fakedriver.Driver
	deletes int
}

func (d *slowDeleter) DeleteInstance(context.Context, vendors.InstanceRef) error {
	d.deletes++
	return nil
}

// After SCarpenter's own accepted delete, a terminating instance is left to the
// vendor for the terminating grace and deleted again after it.
func TestDeleteWaitsTheTerminatingGraceAfterItsOwnDelete(t *testing.T) {
	var slow *slowDeleter
	e := newEnv(t, envOptions{wrap: func(d *fakedriver.Driver) vendors.Driver {
		slow = &slowDeleter{Driver: d}
		return slow
	}})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	claim := &karpv1.NodeClaim{}
	claim.Name = "claim-1"
	claim.Status.ProviderID = created.Status.ProviderID
	claim.Annotations = created.Annotations
	if err := e.cp.Delete(t.Context(), claim); err != nil || slow.deletes != 1 {
		t.Fatalf("Delete of a running instance = %v, %d deletes", err, slow.deletes)
	}
	if !e.driver.PowerOff(vendors.InstanceRef(created.Annotations[v1alpha1.AnnotationInstanceRef])) {
		t.Fatal("no instance to power off")
	}
	for _, step := range []time.Duration{0, vendors.DefaultTerminatingGrace - time.Second} {
		e.advance(step)
		if err := e.cp.Delete(t.Context(), claim); err != nil || slow.deletes != 1 {
			t.Fatalf("Delete inside the grace = %v, %d deletes, want nil and no new one", err, slow.deletes)
		}
	}
	e.advance(time.Second)
	if err := e.cp.Delete(t.Context(), claim); err != nil || slow.deletes != 2 {
		t.Fatalf("Delete after the grace = %v, %d deletes, want the second", err, slow.deletes)
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

// goneReports collects what the CloudProvider tells OnInstanceGone.
type goneReports struct {
	mu      sync.Mutex
	reasons []string
}

func (g *goneReports) hook(_ context.Context, _ *karpv1.NodeClaim, _, goneReason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reasons = append(g.reasons, goneReason)
}

func (g *goneReports) list() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.reasons)
}

// An instance that is gone before SCarpenter deleted it is reported, with the
// driver's reason; one SCarpenter deleted, or one core disrupts on purpose, is not.
func TestDeleteReportsAnInstanceThatIsGoneWithoutSCarpenterDeletingIt(t *testing.T) {
	for name, tc := range map[string]struct {
		reclaim func(e *env, ref vendors.InstanceRef)
		claim   func(created *karpv1.NodeClaim, claim *karpv1.NodeClaim)
		want    []string // what OnInstanceGone is told by the first Delete
	}{
		"silent reclaim": {
			reclaim: func(e *env, ref vendors.InstanceRef) { e.driver.Reclaim(ref) },
			want:    []string{""},
		},
		"reclaim the vendor names": {
			reclaim: func(e *env, ref vendors.InstanceRef) { e.driver.ReclaimWithReason(ref, v1alpha1.ReasonSpotInterrupted) },
			want:    []string{v1alpha1.ReasonSpotInterrupted},
		},
		"found by tag, without the instance reference": {
			reclaim: func(e *env, ref vendors.InstanceRef) { e.driver.Reclaim(ref) },
			claim:   func(_, claim *karpv1.NodeClaim) { claim.Annotations = nil },
			want:    []string{""},
		},
		"disrupted on purpose by core": {
			reclaim: func(e *env, ref vendors.InstanceRef) { e.driver.Reclaim(ref) },
			claim: func(_, claim *karpv1.NodeClaim) {
				claim.StatusConditions().SetTrueWithReason(karpv1.ConditionTypeDisruptionReason, "Drifted", "Drifted")
			},
			want: nil,
		},
		"seen by an earlier Delete, before a restart": {
			// Core sets InstanceTerminating after a Delete that did not fail: the
			// earlier Delete deleted the instance, the memory of it is gone.
			reclaim: func(e *env, ref vendors.InstanceRef) { e.driver.Reclaim(ref) },
			claim: func(_, claim *karpv1.NodeClaim) {
				claim.StatusConditions().SetTrue(karpv1.ConditionTypeInstanceTerminating)
			},
			want: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got goneReports
			e := newEnv(t, envOptions{onInstanceGone: got.hook})
			created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
			if err != nil {
				t.Fatal(err)
			}
			claim := &karpv1.NodeClaim{}
			claim.Name = "claim-1"
			claim.Status.ProviderID = created.Status.ProviderID
			claim.Annotations = created.Annotations
			if tc.claim != nil {
				tc.claim(created, claim)
			}
			tc.reclaim(e, vendors.InstanceRef(created.Annotations[v1alpha1.AnnotationInstanceRef]))
			if err := e.cp.Delete(t.Context(), claim); !karpcloudprovider.IsNodeClaimNotFoundError(err) {
				t.Fatalf("Delete of a reclaimed instance = %v, want NodeClaimNotFound", err)
			}
			if !slices.Equal(got.list(), tc.want) {
				t.Errorf("OnInstanceGone was told %q, want %q", got.list(), tc.want)
			}
		})
	}
}

// A stopped (terminating) instance that SCarpenter did not stop is reported by
// the first Delete, without a reason from the driver.
func TestDeleteReportsATerminatingInstanceItDidNotDelete(t *testing.T) {
	var got goneReports
	e := newEnv(t, envOptions{onInstanceGone: got.hook})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	claim := &karpv1.NodeClaim{}
	claim.Name = "claim-1"
	claim.Status.ProviderID = created.Status.ProviderID
	claim.Annotations = created.Annotations
	if !e.driver.PowerOff(vendors.InstanceRef(created.Annotations[v1alpha1.AnnotationInstanceRef])) {
		t.Fatal("PowerOff found no running instance")
	}
	if err := e.cp.Delete(t.Context(), claim); err != nil {
		t.Fatalf("Delete of a terminating instance = %v, want nil (core asks again)", err)
	}
	if reports := got.list(); !slices.Equal(reports, []string{""}) {
		t.Errorf("OnInstanceGone was told %q, want one report without a reason", reports)
	}
}

func TestDeleteDoesNotReportTheInstanceItDeleted(t *testing.T) {
	var got goneReports
	e := newEnv(t, envOptions{onInstanceGone: got.hook})
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err != nil {
		t.Fatal(err)
	}
	claim := &karpv1.NodeClaim{}
	claim.Name = "claim-1"
	claim.Status.ProviderID = created.Status.ProviderID
	claim.Annotations = created.Annotations
	for range 3 { // core calls Delete until it hears NotFound, and again from another controller
		_ = e.cp.Delete(t.Context(), claim)
	}
	if reports := got.list(); len(reports) != 0 {
		t.Errorf("OnInstanceGone was told %q about an instance SCarpenter deleted", reports)
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
	// Until the launch mints a key per node, a PerNode NodeClass launches nothing.
	t.Run("per-node tailnet keys are not launchable yet", func(t *testing.T) {
		e := newEnv(t, envOptions{transform: func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
				Version: "v1.98.10", KeySource: v1alpha1.TailnetKeyPerNode, Tags: []string{"tag:bench"},
			}}
		}})
		_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
		if !karpcloudprovider.IsNodeClassNotReadyError(err) {
			t.Errorf("Create error = %v, want NodeClassNotReady", err)
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
		var lines []string
		ctx := logr.NewContext(t.Context(), funcr.New(func(_, args string) { lines = append(lines, args) },
			funcr.Options{Verbosity: 1}))
		for range 2 {
			its, err := e.cp.GetInstanceTypes(ctx, nodePool())
			if err != nil || len(its) != 0 {
				t.Errorf("GetInstanceTypes = %d types, %v for a vendor that is not ready", len(its), err)
			}
		}
		// The vendor is named once, with the code, without the condition's message.
		lines = slices.DeleteFunc(lines, func(l string) bool { return !strings.Contains(l, "VendorNotReady") })
		if len(lines) != 1 || !strings.Contains(lines[0], "aws (NoStatus)") {
			t.Errorf("log lines %q, want one VendorNotReady line naming aws", lines)
		}
	})
	// A vendor that is not ready must not turn the failures of another vendor's
	// NodeClaim into NodeClassNotReady (plan 04 review, N5).
	t.Run("another vendor not ready", func(t *testing.T) {
		e := newEnv(t, envOptions{})
		// aws is ready (markReady), qemu has no status: not ready. The catalog
		// does not know the qemu block (it was built before), and nothing in it
		// matches the type, so there are no candidates.
		nc := &v1alpha1.SCNodeClass{}
		if err := e.client.Get(t.Context(), clientKey(className), nc); err != nil {
			t.Fatal(err)
		}
		nc.Spec.Vendors.QEMU = &qemu.Settings{}
		if err := e.client.Update(t.Context(), nc); err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			vendors  []string
			notReady bool
		}{
			"the ready vendor is allowed":                  {vendors: []string{"aws", "qemu"}},
			"only the ready vendor is allowed":             {vendors: []string{"aws"}},
			"only the vendor that is not ready is allowed": {vendors: []string{"qemu"}, notReady: true},
		} {
			claim := e.newClaim("claim-"+strings.ReplaceAll(name, " ", "-"), []string{"aws.does-not-exist"},
				func(c *karpv1.NodeClaim) {
					c.Spec.Requirements = append(c.Spec.Requirements, req(v1alpha1.LabelVendor, corev1.NodeSelectorOpIn, tc.vendors...))
				})
			_, err := e.cp.Create(t.Context(), claim)
			if got := karpcloudprovider.IsNodeClassNotReadyError(err); got != tc.notReady {
				t.Errorf("%s: Create error = %v, NodeClassNotReady = %v, want %v", name, err, got, tc.notReady)
			}
			if !tc.notReady && !karpcloudprovider.IsInsufficientCapacityError(err) {
				t.Errorf("%s: Create error = %v, want InsufficientCapacityError", name, err)
			}
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

// Core marks every NodeClaim Drifted (InstanceTypeNotFound) when the list lacks
// its type, so the list of a start that has not loaded the facts is an error.
func TestGetInstanceTypesFailsUntilTheDriverFactsAreLoaded(t *testing.T) {
	e := newEnv(t, envOptions{skipFacts: true})
	if its, err := e.cp.GetInstanceTypes(t.Context(), nodePool()); err == nil {
		t.Fatalf("GetInstanceTypes = %d types, nil error before the facts are loaded", len(its))
	}
	if err := e.facts.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if its, err := e.cp.GetInstanceTypes(t.Context(), nodePool()); err != nil || len(its) == 0 {
		t.Errorf("GetInstanceTypes = %d types, %v after the facts are loaded", len(its), err)
	}
}

// A driver that keeps failing must not hold the class back for ever.
func TestGetInstanceTypesOffersWithoutFailingFactsAfterTheGrace(t *testing.T) {
	e := newEnv(t, envOptions{skipFacts: true, wrap: func(d *fakedriver.Driver) vendors.Driver { return regionsFailing{d} }})
	if err := e.facts.Load(t.Context()); err == nil {
		t.Fatal("Load succeeded with a driver whose Regions fails")
	}
	if _, err := e.cp.GetInstanceTypes(t.Context(), nodePool()); err == nil {
		t.Fatal("GetInstanceTypes succeeded within the grace period")
	}
	e.advance(cloudprovider.FactsStartupGrace)
	if _, err := e.cp.GetInstanceTypes(t.Context(), nodePool()); err != nil {
		t.Errorf("GetInstanceTypes after the grace period: %v", err)
	}
}

func TestGetInstanceTypesIgnoresTheFactsOfAVendorThatIsNotReady(t *testing.T) {
	e := newEnv(t, envOptions{skipFacts: true, notReady: true})
	if its, err := e.cp.GetInstanceTypes(t.Context(), nodePool()); err != nil || len(its) != 0 {
		t.Errorf("GetInstanceTypes = %d types, %v, want none and no error", len(its), err)
	}
}

func TestGetInstanceTypesFailsWithoutTheCatalog(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.cat.SetClasses(nil)
	if _, err := e.cp.GetInstanceTypes(t.Context(), nodePool()); err == nil {
		t.Error("GetInstanceTypes succeeded for a class the catalog does not know")
	}
}

// regionsFailing is a driver that cannot list its regions.
type regionsFailing struct{ *fakedriver.Driver }

func (regionsFailing) Regions(context.Context) ([]vendors.Region, error) {
	return nil, vendors.NewError(vendors.Retryable, "RegionsUnavailable", errors.New("regions unavailable"))
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

// TestCreateWithACatalogThatIsNotReadyIsRetriedNotRefused: a controller that has
// just restarted has no snapshot yet. A NodeClaim whose launch is replayed then
// must not get InsufficientCapacity (core would delete it, and the instance its
// idempotency key finds would be orphaned): the launch is retried (L4 scenario
// Q04).
func TestCreateWithACatalogThatIsNotReadyIsRetriedNotRefused(t *testing.T) {
	e := newEnv(t, envOptions{})
	f := facts.New(facts.Options{Registry: e.registry})
	if err := f.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	fresh := catalog.New(catalog.Options{Source: catalogtest.SourceAt(keeperURL), Facts: f, Unavailable: e.un, Clock: e.clock})
	nc := &v1alpha1.SCNodeClass{}
	if err := e.client.Get(t.Context(), clientKey(className), nc); err != nil {
		t.Fatal(err)
	}
	class, err := catalog.ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	fresh.SetClasses([]catalog.Class{class})
	e.cat = fresh
	e.build(e.client, f, e.registry, envOptions{})

	_, err = e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	if err == nil || karpcloudprovider.IsInsufficientCapacityError(err) || karpcloudprovider.IsNodeClassNotReadyError(err) {
		t.Fatalf("Create error = %v, want a retryable error", err)
	}
	if !isCreateError(err) {
		t.Errorf("Create error = %v, want a CreateError that core retries", err)
	}
	if n := len(e.calls("create")); n != 0 {
		t.Errorf("%d vendor calls without a catalog", n)
	}

	// With the snapshot the launch goes through.
	if err := fresh.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"})); err != nil {
		t.Errorf("Create after the catalog was refreshed: %v", err)
	}
}

// ownerDriver records the image owner each ResolveImage asks for.
type ownerDriver struct {
	*fakedriver.Driver
	mu     sync.Mutex
	owners []string
}

func (d *ownerDriver) ResolveImage(ctx context.Context, req vendors.ImageRequest) (*vendors.Image, error) {
	d.mu.Lock()
	d.owners = append(d.owners, req.Owner)
	d.mu.Unlock()
	return d.Driver.ResolveImage(ctx, req)
}

// aws.imageOwner must reach the launch: the status reports the AMI of the override,
// and the instance must boot it.
func TestCreateResolvesTheImageOfTheNodeClassesOwner(t *testing.T) {
	var drv *ownerDriver
	e := newEnv(t, envOptions{
		transform: func(nc *v1alpha1.SCNodeClass) { nc.Spec.Vendors.AWS.ImageOwner = "123456789012" },
		wrap: func(d *fakedriver.Driver) vendors.Driver {
			drv = &ownerDriver{Driver: d}
			return drv
		},
	})
	if _, err := e.cp.Create(t.Context(), e.newClaim("claim-owner", []string{"aws.m7i.large"})); err != nil {
		t.Fatal(err)
	}
	drv.mu.Lock()
	defer drv.mu.Unlock()
	if len(drv.owners) == 0 || drv.owners[0] != "123456789012" {
		t.Errorf("ResolveImage owners = %v, want the NodeClass's 123456789012", drv.owners)
	}
}

// GetInstanceTypes logs, once per generation and NodePool, the types a
// NodePool's requirements remove that the catalog offers (plan 28, step 2).
func TestGetInstanceTypesLogsTheNodePoolReasonsOnce(t *testing.T) {
	e := newEnv(t, envOptions{})
	var lines []string
	ctx := logr.NewContext(t.Context(), funcr.New(func(_, args string) { lines = append(lines, args) },
		funcr.Options{Verbosity: 1}))
	pool := nodePool()
	pool.Spec.Template.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		{Key: v1alpha1.LabelZoneID, Operator: corev1.NodeSelectorOpIn, Values: []string{"nowhere"}},
	}
	nodePoolLines := func() (n int) {
		for _, l := range lines {
			if strings.Contains(l, `"nodepool"="default"`) && strings.Contains(l, `"reason"="NodePoolRequirement"`) &&
				strings.Contains(l, "aws.m7i.large") {
				n++
			}
		}
		return n
	}
	for range 2 {
		if _, err := e.cp.GetInstanceTypes(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if n := nodePoolLines(); n != 1 {
		t.Errorf("%d NodePool lines naming aws.m7i.large after two calls, want 1", n)
	}
}

// A launch whose user-data is over the vendor's limit (a spot GPU type with the
// agent) rejects that offering only: it is marked and Event'ed, and another type
// launches in the same Create.
func TestCreateRejectsAnOfferingWhoseUserDataIsTooLarge(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 100})
	types := []string{"aws.m7i.large", "aws.c7i.large"}
	bad, good := e.offerings(types...)[0].typeName, types[0]
	if bad == good {
		good = types[1]
	}
	e.renderer.tooLarge = strings.TrimPrefix(bad, "aws.")
	created, err := e.cp.Create(t.Context(), e.newClaim("claim-1", types))
	if err != nil {
		t.Fatalf("Create: %v, want the other type to launch", err)
	}
	if got := created.Labels[corev1.LabelInstanceTypeStable]; got != good {
		t.Errorf("launched %q, want %s", got, good)
	}
	if !contains(e.events.reasons(), cloudprovider.EventOfferingRejected) {
		t.Errorf("events = %v, want %s", e.events.reasons(), cloudprovider.EventOfferingRejected)
	}
}

// withLaunch sets the NodeClass's launch policy.
func withLaunch(l v1alpha1.LaunchSpec) func(*v1alpha1.SCNodeClass) {
	return func(nc *v1alpha1.SCNodeClass) { nc.Spec.Launch = &l }
}

func TestLaunchMaxAttemptsOverridesTheFlag(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 3, transform: withLaunch(v1alpha1.LaunchSpec{MaxAttempts: new(int32(2))})})
	e.setConfig("errors", "- {error: InsufficientCapacity}")
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large", "aws.c7i.large", "aws.m6i.large"}))
	if !karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Fatalf("Create error = %v, want InsufficientCapacityError", err)
	}
	if n := len(e.calls("create")); n != 2 {
		t.Errorf("%d vendor calls, want the class's maxAttempts (2)", n)
	}
	if !strings.Contains(err.Error(), "tried 2 offerings in ") ||
		!strings.Contains(err.Error(), "; classes: InsufficientCapacity:2; stopped by: maxAttempts") {
		t.Errorf("error = %q, want the summary", err)
	}
}

// slowDriver takes a minute of the fake clock per launch.
type slowDriver struct {
	*fakedriver.Driver
	// clock is read at the call: the env that owns the clock is built after the driver.
	clock func() *testingclock.FakeClock
}

func (d slowDriver) CreateInstance(ctx context.Context, req vendors.CreateRequest) (*vendors.Instance, error) {
	d.clock().Step(time.Minute)
	return d.Driver.CreateInstance(ctx, req)
}

func TestLaunchMaxDurationStopsTheLoop(t *testing.T) {
	var e *env
	e = newEnv(t, envOptions{
		attempts:  100,
		transform: withLaunch(v1alpha1.LaunchSpec{MaxDuration: &metav1.Duration{Duration: 150 * time.Second}}),
		wrap: func(d *fakedriver.Driver) vendors.Driver {
			return slowDriver{d, func() *testingclock.FakeClock { return e.clock }}
		},
	})
	e.setConfig("errors", "- {error: InsufficientCapacity}")
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large", "aws.c7i.large", "aws.m6i.large"}))
	if !karpcloudprovider.IsInsufficientCapacityError(err) {
		t.Fatalf("Create error = %v, want InsufficientCapacityError", err)
	}
	if n := len(e.calls("create")); n != 3 {
		t.Errorf("%d vendor calls, want 3 (150s budget, one minute per call)", n)
	}
	if !strings.Contains(err.Error(), "stopped by: maxDuration") {
		t.Errorf("error = %q, want stopped by maxDuration", err)
	}
}

// The first candidate is tried even when the budget is already spent.
func TestLaunchMaxDurationNeverSkipsTheFirstCandidate(t *testing.T) {
	e := newEnv(t, envOptions{
		attempts: 100, transform: withLaunch(v1alpha1.LaunchSpec{MaxDuration: &metav1.Duration{Duration: -time.Second}}),
	})
	e.setConfig("errors", "- {error: InsufficientCapacity}")
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large", "aws.c7i.large"}))
	if n := len(e.calls("create")); n != 1 || !strings.Contains(err.Error(), "stopped by: maxDuration") {
		t.Errorf("%d vendor calls, error %v, want one call", n, err)
	}
}

func TestLaunchWithoutPolicyStopsByTheFlag(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 3})
	e.setConfig("errors", "- {error: InsufficientCapacity}")
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large", "aws.c7i.large", "aws.m6i.large"}))
	if n := len(e.calls("create")); n != 3 || !strings.Contains(err.Error(), "stopped by: maxAttempts") {
		t.Errorf("%d vendor calls, error %v", n, err)
	}
}

func TestLaunchSummaryWhenNoCandidateIsLeft(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 100})
	e.setConfig("errors", "- {error: InsufficientCapacity}")
	_, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	want := fmt.Sprintf("tried %d offerings", len(e.calls("create")))
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "stopped by: no candidates left") ||
		!strings.Contains(err.Error(), "InsufficientCapacity (InjectedInsufficientCapacity)") {
		t.Errorf("error = %q", err)
	}
}

func TestRegionWideCapacityErrorMarksTheRegionAndSkipsItsOtherZones(t *testing.T) {
	e := newEnv(t, envOptions{attempts: 100})
	target := e.offerings("aws.m7i.large")[0]
	e.setConfig("errors", "- {match: {region: "+target.region+"}, error: InsufficientCapacity, regionWide: true}")
	if _, err := e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"})); err != nil {
		t.Fatalf("Create: %v, want the other region to launch", err)
	}
	// One failed call per capacity type: the zones of the region are skipped after the first.
	for _, call := range e.calls("create") {
		for _, other := range e.calls("create") {
			if call != other && call.Region == other.Region && call.CapacityType == other.CapacityType {
				t.Errorf("two calls for %s %s: %+v, %+v", call.Region, call.CapacityType, call, other)
			}
		}
	}
	for _, o := range e.offerings("aws.m7i.large") {
		if o.region == target.region && o.capacityType == target.capacityType {
			t.Errorf("%+v of the marked region is still offered", o)
		}
	}
}

func TestClassInsufficientCapacityTTLOutlastsTheSharedMark(t *testing.T) {
	e := newEnv(t, envOptions{
		attempts: 1, transform: withLaunch(v1alpha1.LaunchSpec{InsufficientCapacityTTL: &metav1.Duration{Duration: time.Hour}}),
	})
	target := e.offerings("aws.m7i.large")[0]
	e.setConfig("errors", "- {error: InsufficientCapacity, count: 1}")
	_, _ = e.cp.Create(t.Context(), e.newClaim("claim-1", []string{"aws.m7i.large"}))
	e.advance(2 * time.Minute) // the shared mark of the env is one minute
	shared, mine := unavailableOf(target), unavailableOf(target)
	mine.NodeClass = className
	if e.un.IsUnavailable(shared) {
		t.Error("the shared mark outlived its TTL")
	}
	if !e.un.IsUnavailable(mine) {
		t.Error("the class does not see its own longer mark")
	}
	e.advance(time.Hour)
	if e.un.IsUnavailable(mine) {
		t.Error("the class mark outlived its TTL")
	}
}
