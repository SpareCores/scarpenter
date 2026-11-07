//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

const (
	us1 = "us-east-1"
	us2 = "us-east-2"
	eu1 = "eu-west-1"
	// generous: the typical time of every wait below is seconds.
	long = 5 * time.Minute
)

// taken are the instance types scenarios have claimed, so that the ICE and
// quota marks of one scenario never touch another.
// minAllocMem skips types whose kube-reserved memory (11 MiB per pod plus
// 255 MiB) leaves no room for a pod.
const minAllocMem = 512 << 20

var (
	taken   = map[string]bool{}
	takenMu sync.Mutex
)

func isOnDemand(o offering) bool { return o.Capacity == "on-demand" }

// takeTypes returns n distinct on-demand types with the given vCPUs that exist
// in every region, cheapest first, and claims them.
func takeTypes(t *testing.T, regions []string, vcpus int64, n int) []string {
	t.Helper()
	takenMu.Lock()
	defer takenMu.Unlock()
	price := map[string]float64{}
	seen := map[string]map[string]bool{}
	for _, o := range reference(t, regions...) {
		if !isOnDemand(o) || o.VCPUs != vcpus || taken[o.API] || o.AllocMem < minAllocMem {
			continue
		}
		if p, ok := price[o.API]; !ok || o.Price < p {
			price[o.API] = o.Price
		}
		if seen[o.API] == nil {
			seen[o.API] = map[string]bool{}
		}
		seen[o.API][o.Region] = true
	}
	var names []string
	for name := range price {
		if len(seen[name]) == len(regions) {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		if price[a] != price[b] {
			if price[a] < price[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	if len(names) < n {
		t.Fatalf("the snapshot has %d usable %d-vCPU types in %v, need %d", len(names), vcpus, regions, n)
	}
	names = names[:n]
	for _, nme := range names {
		taken[nme] = true
	}
	return names
}

func apiNames(types []string) []string {
	out := make([]string, len(types))
	for i, ty := range types {
		out[i] = vendor + "." + ty
	}
	return out
}

// byPrice orders offerings the way Create does: price, then the catalog name
// of the type, then the zone (overview section 4.1; the fake has no quota
// headroom to break ties with before that).
func byPrice(os []offering) []offering {
	out := slices.Clone(os)
	slices.SortFunc(out, func(a, b offering) int {
		switch {
		case a.Price != b.Price:
			if a.Price < b.Price {
				return -1
			}
			return 1
		case a.Type != b.Type:
			return strings.Compare(a.Type, b.Type)
		}
		return strings.Compare(a.Zone, b.Zone)
	})
	return out
}

func priceOf(t *testing.T, os []offering, typ, region, zone, capacity string) float64 {
	t.Helper()
	for _, o := range os {
		if o.Type == typ && o.Region == region && o.Zone == zone && o.Capacity == capacity {
			return o.Price
		}
	}
	t.Fatalf("no offering %s %s %s %s in the reference catalog", typ, region, zone, capacity)
	return 0
}

func nodeLabels(t *testing.T, node string) map[string]string {
	t.Helper()
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: node}, n); err != nil {
		t.Fatal(err)
	}
	return n.Labels
}

func waitNodeGone(t *testing.T, node string) {
	t.Helper()
	eventually(t, long, time.Second, "node "+node+" removed", func() (bool, string) {
		err := k8s.Get(ctx(), client.ObjectKey{Name: node}, &corev1.Node{})
		return apierrors.IsNotFound(err), fmt.Sprint(err)
	})
}

// deletePodAndWaitNode deletes the pod and waits until its node is gone
// (WhenEmpty consolidation at once).
func deletePodAndWaitNode(t *testing.T, pod, node string) {
	t.Helper()
	if err := k8s.Delete(ctx(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: "default"}},
		client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	waitNodeGone(t, node)
}

// S01: a pod with an exact aws.* instance type gets a NodeClaim and a Ready
// node with every expected label; deleting the pod removes the node, the
// NodeClaim and the instance.
func TestS01ExactType(t *testing.T) {
	createNodeClass(t, "s01", us1)
	createNodePool(t, "s01", "s01", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	typ := takeTypes(t, []string{us1}, 2, 1)[0]
	createPod(t, "s01", map[string]string{labelType: vendor + "." + typ, labelNodePool: "s01"}, "100m")

	node := waitPodRunning(t, "s01", long)
	claims := waitClaims(t, "s01", 1, time.Minute)
	if len(claims) != 1 {
		t.Fatalf("want one NodeClaim, got %d", len(claims))
	}
	eventually(t, time.Minute, time.Second, "node Ready", func() (bool, string) { return nodeReady(node) })

	// Core sets the initialized label a moment after the node is Ready.
	eventually(t, time.Minute, time.Second, "node initialized", func() (bool, string) {
		return nodeLabels(t, node)[karpv1.NodeInitializedLabelKey] == "true", "not initialized yet"
	})
	l := nodeLabels(t, node)
	want := map[string]string{
		labelType: vendor + "." + typ, labelRegion: us1, labelCapacity: "on-demand", corev1.LabelOSStable: "linux",
		v1alpha1.LabelVendor: vendor, v1alpha1.LabelAPIReference: typ, corev1.LabelHostname: claims[0].Name,
		labelNodePool: "s01", karpv1.NodeRegisteredLabelKey: "true", karpv1.NodeInitializedLabelKey: "true",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	for _, k := range []string{
		labelZone, v1alpha1.LabelZoneID, corev1.LabelArchStable, v1alpha1.LabelFamily, v1alpha1.LabelVCPUs,
		v1alpha1.LabelMemoryMiB,
	} {
		if l[k] == "" {
			t.Errorf("label %s is missing; labels: %v", k, l)
		}
	}
	if l[v1alpha1.LabelVCPUs] != "2" {
		t.Errorf("vcpus label = %q, want 2", l[v1alpha1.LabelVCPUs])
	}
	live := liveInstances(t)
	if i := slices.IndexFunc(live, func(i instance) bool { return i.NodeClaim == claims[0].Name }); i < 0 {
		t.Errorf("the fake has no live instance for %s: %+v", claims[0].Name, live)
	} else if live[i].InstanceType != typ || live[i].Region != us1 {
		t.Errorf("fake instance %+v, want %s in %s", live[i], typ, us1)
	}

	deletePodAndWaitNode(t, "s01", node)
	waitNoClaims(t, "s01", long)
	eventually(t, time.Minute, time.Second, "the fake instance terminated", func() (bool, string) {
		for _, i := range liveInstances(t) {
			if i.NodeClaim == claims[0].Name {
				return false, fmt.Sprintf("%+v", i)
			}
		}
		return true, ""
	})
}

// S02: the cheapest offering wins among several allowed types, zones and
// capacity types.
func TestS02CheapestWins(t *testing.T) {
	types := takeTypes(t, []string{us1, us2}, 2, 4)
	createNodeClass(t, "s02", us1, us2)
	createNodePool(t, "s02", "s02", in(labelType, apiNames(types)...))
	createPod(t, "s02", map[string]string{labelNodePool: "s02"}, "100m")

	node := waitPodRunning(t, "s02", long)
	l := nodeLabels(t, node)
	all := filterOfferings(reference(t, us1, us2), func(o offering) bool { return slices.Contains(apiNames(types), o.Type) })
	min := byPrice(all)[0].Price
	got := priceOf(t, all, l[labelType], l[labelRegion], l[labelZone], l[labelCapacity])
	if got != min {
		t.Errorf("launched %s %s %s %s at %v, the cheapest offering costs %v",
			l[labelType], l[labelRegion], l[labelZone], l[labelCapacity], got, min)
	}
}

// S03: capacity errors fall back to the next offering within one Create, a
// failed Create makes its offerings unavailable, the next round picks others,
// and the TTL expiry makes them available again.
func TestS03InsufficientCapacity(t *testing.T) {
	createNodeClass(t, "s03", us1)

	t.Run("fallback within one Create", func(t *testing.T) {
		typ := takeTypes(t, []string{us1}, 2, 2)
		createNodePool(t, "s03a", "s03", in(labelRegion, us1), in(labelCapacity, "on-demand"), in(labelType, apiNames(typ)...))
		order := byPrice(filterOfferings(reference(t, us1), func(o offering) bool {
			return isOnDemand(o) && slices.Contains(apiNames(typ), o.Type)
		}))
		first, second := order[0], order[1]
		setFake(t, map[string]string{"errors": fmt.Sprintf(
			"- match: {instanceType: %s, region: %s, zone: %s, capacityType: on-demand}\n  error: InsufficientCapacity\n",
			first.API, us1, first.Zone)})
		mark := time.Now()
		createPod(t, "s03a", map[string]string{labelNodePool: "s03a"}, "100m")
		node := waitPodRunning(t, "s03a", long)

		creates := callsSince(t, mark, "create")
		if len(creates) < 2 {
			t.Fatalf("want at least two create calls, got %+v", creates)
		}
		if c := creates[0]; c.InstanceType != first.API || c.Zone != first.Zone || c.Result != "InsufficientCapacity" {
			t.Errorf("the first attempt %+v, want ICE on the cheapest offering %+v", c, first)
		}
		last := creates[len(creates)-1]
		if last.Result != "ok" || last.NodeClaim != creates[0].NodeClaim {
			t.Errorf("the last attempt %+v did not succeed in the same Create as %+v", last, creates[0])
		}
		if last.InstanceType != second.API || last.Zone != second.Zone {
			t.Errorf("fell back to %s/%s, want the second cheapest %s/%s", last.InstanceType, last.Zone, second.API, second.Zone)
		}
		if l := nodeLabels(t, node); l[labelZone] == first.Zone && l[labelType] == vendor+"."+first.API {
			t.Errorf("the node is in the failed offering")
		}
	})

	t.Run("every attempt fails, the next round picks another offering", func(t *testing.T) {
		typ := takeTypes(t, []string{us1}, 2, 1)[0]
		createNodePool(t, "s03b", "s03", in(labelRegion, us1), in(labelCapacity, "on-demand"), in(labelType, vendor+"."+typ))
		setFake(t, map[string]string{"errors": fmt.Sprintf(
			"- match: {instanceType: %s}\n  error: InsufficientCapacity\n", typ)})
		mark := time.Now()
		createPod(t, "s03b", map[string]string{labelNodePool: "s03b"}, "100m")
		eventually(t, long, time.Second, "attempts of two different NodeClaims", func() (bool, string) {
			claims := map[string]bool{}
			for _, c := range callsSince(t, mark, "create") {
				claims[c.NodeClaim] = true
			}
			return len(claims) >= 2, fmt.Sprint(len(claims), " NodeClaims attempted")
		})
		creates := callsSince(t, mark, "create")
		perClaim := map[string][]call{}
		for _, c := range creates {
			if c.Result != "InsufficientCapacity" {
				t.Errorf("unexpected result %+v", c)
			}
			perClaim[c.NodeClaim] = append(perClaim[c.NodeClaim], c)
		}
		seen := map[string]string{}
		for claim, cs := range perClaim {
			if len(cs) > 3 {
				t.Errorf("NodeClaim %s made %d attempts, the maximum is 3", claim, len(cs))
			}
			for _, c := range cs {
				if prev, dup := seen[c.Zone]; dup && prev != claim {
					t.Errorf("zone %s was attempted again by %s after %s failed there (offerings are unavailable)", c.Zone, claim, prev)
				}
				seen[c.Zone] = claim
			}
		}
		if node, err := podNode("s03b"); err != nil || node != "" {
			t.Errorf("the pod must stay pending (node %q, error %v)", node, err)
		}
		// With the error gone the pod is served as soon as offerings are
		// available again.
		clearFake(t, "errors")
		waitPodRunning(t, "s03b", long)
	})

	t.Run("TTL expiry makes the offering available", func(t *testing.T) {
		typ := takeTypes(t, []string{us1}, 2, 1)[0]
		zone := byPrice(filterOfferings(reference(t, us1), func(o offering) bool { return isOnDemand(o) && o.API == typ }))[0].Zone
		createNodePool(t, "s03c", "s03", in(labelRegion, us1), in(labelCapacity, "on-demand"),
			in(labelType, vendor+"."+typ), in(labelZone, zone))
		setFake(t, map[string]string{"errors": fmt.Sprintf(
			"- match: {instanceType: %s}\n  error: InsufficientCapacity\n  count: 1\n", typ)})
		mark := time.Now()
		createPod(t, "s03c", map[string]string{labelNodePool: "s03c"}, "100m")
		waitPodRunning(t, "s03c", long)
		creates := callsSince(t, mark, "create")
		if len(creates) < 2 || creates[0].Result != "InsufficientCapacity" || creates[len(creates)-1].Result != "ok" {
			t.Fatalf("want an ICE and then a success, got %+v", creates)
		}
		gap := creates[len(creates)-1].Time.Sub(creates[0].Time)
		t.Logf("ICE to the next attempt: %s (the TTL of the lane is 20s)", gap.Round(time.Second))
		if gap < 15*time.Second {
			t.Errorf("the offering was retried after %s, before its 20s TTL", gap)
		}
	})
}

// quotaYAML is a quota configuration with one regional bucket "std" that every
// type consumes; overrides sets per-region limits and foreign usage.
func quotaYAML(limit, foreign int64, overrides map[string][2]int64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "buckets:\n  - {id: std, scope: region, unit: vCPU, limit: %d, foreignUsed: %d", limit, foreign)
	if len(overrides) > 0 {
		sb.WriteString(", regions: {")
		first := true
		for r, v := range overrides {
			if !first {
				sb.WriteString(", ")
			}
			first = false
			fmt.Fprintf(&sb, "%s: {limit: %d, foreignUsed: %d}", r, v[0], v[1])
		}
		sb.WriteString("}")
	}
	sb.WriteString("}\nconsumption:\n  - {families: [\"*\"], capacityTypes: [on-demand, spot], buckets: [std]}\n")
	return sb.String()
}

func nodeRegion(t *testing.T, node string) string { return nodeLabels(t, node)[labelRegion] }

// S04: quota with flexible regions: the cheapest region's bucket is exhausted
// by foreign usage, so nodes go to the next region without any vendor call in
// the exhausted one; five pods that need more than the headroom of one region
// spread over both without a quota error (the ledger).
func TestS04QuotaFlexibleRegion(t *testing.T) {
	typ, cheap, other := typeWithUniqueCheapestRegion(t, []string{us1, eu1}, 2, keys2(taken)...)
	taken[typ] = true
	createNodeClass(t, "s04", us1, eu1)
	createNodePool(t, "s04", "s04", in(labelType, vendor+"."+typ), in(labelCapacity, "on-demand"))
	sel := map[string]string{labelNodePool: "s04"}

	t.Run("exhausted region is not used", func(t *testing.T) {
		setFake(t, map[string]string{"quota": quotaYAML(1000, 0, map[string][2]int64{cheap: {4, 4}})})
		mark := time.Now()
		// Let the tracker see the exhausted bucket before the pod exists.
		time.Sleep(15 * time.Second)
		createPod(t, "s04-one", sel, "1500m")
		node := waitPodRunning(t, "s04-one", long)
		if got := nodeRegion(t, node); got != other {
			t.Errorf("node in %s, want %s (the cheapest region %s has no quota)", got, other, cheap)
		}
		for _, c := range callsSince(t, mark, "create") {
			if c.Region == cheap {
				t.Errorf("a vendor call went to the exhausted region: %+v", c)
			}
		}
		deletePodAndWaitNode(t, "s04-one", node)
	})

	t.Run("the ledger spreads five pods without quota errors", func(t *testing.T) {
		// The cheapest region has room for two nodes of two vCPUs.
		setFake(t, map[string]string{"quota": quotaYAML(1000, 0, map[string][2]int64{cheap: {4, 0}})})
		time.Sleep(15 * time.Second)
		mark := time.Now()
		for i := range 5 {
			createPod(t, fmt.Sprintf("s04-five-%d", i), sel, "1500m")
		}
		regions := map[string]int{}
		for i := range 5 {
			regions[nodeRegion(t, waitPodRunning(t, fmt.Sprintf("s04-five-%d", i), long))]++
		}
		t.Logf("nodes per region: %v", regions)
		if regions[cheap] != 2 || regions[other] != 3 {
			t.Errorf("nodes per region %v, want 2 in %s and 3 in %s", regions, cheap, other)
		}
		for _, c := range callsSince(t, mark, "create") {
			if c.Result != "ok" {
				t.Errorf("a launch failed: %+v (the ledger must prevent quota errors)", c)
			}
		}
	})
}

func keys2(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// S05: quota with a pinned region: pods stay Pending and nothing is launched
// while the region is exhausted, QuotaAvailable says why, and the pods are
// served once usage drops without touching them (overview 15 Q9). A limit of 0
// reports QuotaZero.
func TestS05QuotaPinnedRegion(t *testing.T) {
	typ := takeTypes(t, []string{us1}, 2, 1)[0]
	createNodeClass(t, "s05", us1)
	createNodePool(t, "s05", "s05", in(labelRegion, us1), in(labelType, vendor+"."+typ), in(labelCapacity, "on-demand"))
	sel := map[string]string{labelNodePool: "s05"}

	setFake(t, map[string]string{"quota": quotaYAML(4, 4, nil)})
	time.Sleep(15 * time.Second)
	mark := time.Now()
	createPod(t, "s05-a", sel, "1500m")
	consistently(t, 30*time.Second, 3*time.Second, "an exhausted pinned region", func() (bool, string) {
		if cs, _ := listClaims("s05"); len(cs) > 0 {
			return true, fmt.Sprintf("a NodeClaim was created: %s", cs[0].Name)
		}
		return false, ""
	})
	if cs := callsSince(t, mark, "create"); len(cs) > 0 {
		t.Fatalf("vendor calls while the region is exhausted: %+v", cs)
	}
	eventually(t, 3*time.Minute, 2*time.Second, "QuotaAvailable=False/QuotaExhausted", func() (bool, string) {
		c, ok, q := vendorCondition("s05", v1alpha1.VendorConditionQuotaAvailable)
		return ok && c.Status == metav1.ConditionFalse && c.Reason == "QuotaExhausted" && len(q) > 0,
			fmt.Sprintf("%+v %+v", c, q)
	})
	if _, _, q := vendorCondition("s05", v1alpha1.VendorConditionQuotaAvailable); len(q) > 0 &&
		(q[0].Region != us1 || q[0].CapacityType != "on-demand" || q[0].Used != 4 || q[0].Limit != 4) {
		t.Errorf("status.quota[0] = %+v", q[0])
	}

	// Q9: usage drops; the pod is not touched.
	freed := time.Now()
	setFake(t, map[string]string{"quota": quotaYAML(4, 0, nil)})
	var claims []karpv1.NodeClaim
	eventually(t, 3*time.Minute, time.Second, "a NodeClaim of s05", func() (bool, string) {
		cs, _ := listClaims("s05")
		claims = cs
		c, _, q := vendorCondition("s05", v1alpha1.VendorConditionQuotaAvailable)
		return len(cs) > 0, fmt.Sprintf("%+v %+v %s", c, q, quotaMetrics(t))
	})
	created := time.Since(freed)
	node := waitPodRunning(t, "s05-a", long)
	ready := time.Since(freed)
	t.Logf("Q9: usage freed -> NodeClaim created after %s, pod Running on %s after %s (quota refresh interval 10s, core batch windows 500ms/5s, NodeClaim %s)",
		created.Round(100*time.Millisecond), node, ready.Round(100*time.Millisecond), claims[0].Name)
	appendOut(t, "q9.txt", fmt.Sprintf("quota freed -> NodeClaim created: %s; -> pod Running: %s", created, ready))
	eventually(t, 3*time.Minute, 2*time.Second, "QuotaAvailable=True again", func() (bool, string) {
		c, ok, _ := vendorCondition("s05", v1alpha1.VendorConditionQuotaAvailable)
		return ok && c.Status == metav1.ConditionTrue, fmt.Sprintf("%+v %s", c, quotaMetrics(t))
	})

	// A limit of 0 resolves only with a quota increase.
	deletePodAndWaitNode(t, "s05-a", node)
	setFake(t, map[string]string{"quota": quotaYAML(0, 0, nil)})
	time.Sleep(15 * time.Second)
	createPod(t, "s05-b", sel, "1500m")
	eventually(t, 3*time.Minute, 2*time.Second, "QuotaAvailable=False/QuotaZero", func() (bool, string) {
		c, ok, q := vendorCondition("s05", v1alpha1.VendorConditionQuotaAvailable)
		return ok && c.Status == metav1.ConditionFalse && c.Reason == "QuotaZero" && len(q) > 0 && q[0].Reason == "QuotaZero",
			fmt.Sprintf("%+v %+v", c, q)
	})
	if cs, _ := listClaims("s05"); len(cs) > 0 {
		t.Errorf("a NodeClaim exists although the limit is 0: %s", cs[0].Name)
	}
}

// S06: a type that the driver says is not launchable in a zone is never
// attempted there.
func TestS06NotLaunchable(t *testing.T) {
	typ := takeTypes(t, []string{us1}, 2, 1)[0]
	order := byPrice(filterOfferings(reference(t, us1), func(o offering) bool { return isOnDemand(o) && o.API == typ }))
	bad := order[0].Zone
	setFake(t, map[string]string{"quota": quotaYAML(1000, 0, nil)})
	cm := &corev1.ConfigMap{}
	if err := k8s.Get(ctx(), client.ObjectKey{Namespace: ns, Name: fakeConfig}, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["quota"] += fmt.Sprintf("notLaunchable:\n  - {region: %s, zone: %s, types: [%s]}\n", us1, bad, typ)
	if err := k8s.Update(ctx(), cm); err != nil {
		t.Fatal(err)
	}
	// The lane's launchable refresh interval is 10s.
	time.Sleep(20 * time.Second)
	start := time.Now()

	createNodeClass(t, "s06", us1)
	createNodePool(t, "s06", "s06", in(labelRegion, us1), in(labelType, vendor+"."+typ), in(labelCapacity, "on-demand"))
	createPod(t, "s06", map[string]string{labelNodePool: "s06"}, "100m")
	node := waitPodRunning(t, "s06", long)
	if z := nodeLabels(t, node)[labelZone]; z == bad {
		t.Errorf("the node is in %s, where %s is not launchable", z, typ)
	}
	for _, c := range callsSince(t, start, "create") {
		if c.InstanceType == typ && c.Zone == bad {
			t.Errorf("a launch of %s was attempted in %s: %+v", typ, bad, c)
		}
	}
}

// S07: a driver without quota knowledge: an injected QuotaExceeded makes the
// region and capacity type unavailable for the TTL.
func TestS07ReactiveQuota(t *testing.T) {
	typ, cheap, other := typeWithUniqueCheapestRegion(t, []string{us1, eu1}, 2, keys2(taken)...)
	taken[typ] = true
	createNodeClass(t, "s07", us1, eu1)
	createNodePool(t, "s07", "s07", in(labelType, vendor+"."+typ), in(labelCapacity, "on-demand"))
	sel := map[string]string{labelNodePool: "s07"}
	setFake(t, map[string]string{"errors": fmt.Sprintf("- match: {region: %s}\n  error: QuotaExceeded\n", cheap)})

	mark := time.Now()
	createPod(t, "s07-a", sel, "1500m")
	node := waitPodRunning(t, "s07-a", long)
	if got := nodeRegion(t, node); got != other {
		t.Errorf("node in %s, want %s", got, other)
	}
	// The fake publishes its state debounced: wait for the calls to appear.
	var creates []call
	eventually(t, 15*time.Second, 500*time.Millisecond, "the vendor calls in the fake's state", func() (bool, string) {
		creates = callsSince(t, mark, "create")
		return len(creates) >= 2, fmt.Sprintf("%+v", creates)
	})
	if len(creates) < 2 || creates[0].Region != cheap || creates[0].Result != "QuotaExceeded" || creates[len(creates)-1].Result != "ok" {
		t.Fatalf("want a QuotaExceeded in %s and then a success in %s, got %+v", cheap, other, creates)
	}

	// Within the TTL the region is not tried again.
	mark = time.Now()
	createPod(t, "s07-b", sel, "1500m")
	waitPodRunning(t, "s07-b", long)
	if time.Since(mark) < 10*time.Second {
		for _, c := range callsSince(t, mark, "create") {
			if c.Region == cheap {
				t.Errorf("the region %s was tried again within the TTL: %+v", cheap, c)
			}
		}
	}
}

// S08: a NodeClass change drifts the node and replaces it.
func TestS08Drift(t *testing.T) {
	typ := takeTypes(t, []string{us1}, 2, 1)[0]
	nc := createNodeClass(t, "s08", us1)
	createNodePool(t, "s08", "s08", in(labelRegion, us1), in(labelType, vendor+"."+typ), in(labelCapacity, "on-demand"))
	d := createDeployment(t, "s08", map[string]string{labelNodePool: "s08"}, "100m")

	claims := waitClaims(t, "s08", 1, long)
	old := claims[0].Name
	eventually(t, long, time.Second, "the deployment running", func() (bool, string) {
		got := &appsv1.Deployment{}
		if err := k8s.Get(ctx(), client.ObjectKeyFromObject(d), got); err != nil {
			return false, err.Error()
		}
		return got.Status.ReadyReplicas == 1, fmt.Sprint(got.Status)
	})

	eventually(t, time.Minute, time.Second, "NodeClass update", func() (bool, string) {
		got := &v1alpha1.SCNodeClass{}
		if err := k8s.Get(ctx(), client.ObjectKeyFromObject(nc), got); err != nil {
			return false, err.Error()
		}
		got.Spec.Kubelet.MaxPods = ptr.To[int32](100)
		if err := k8s.Update(ctx(), got); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	eventually(t, 3*time.Minute, 2*time.Second, "the NodeClaim Drifted", func() (bool, string) {
		cs, _ := listClaims("s08")
		for _, c := range cs {
			if c.Name == old {
				cond := c.StatusConditions().Get(karpv1.ConditionTypeDrifted)
				return cond != nil && cond.IsTrue(), fmt.Sprintf("%+v", cond)
			}
		}
		return true, "the old NodeClaim is already gone"
	})
	eventually(t, long, 2*time.Second, "the node replaced", func() (bool, string) {
		cs, _ := listClaims("s08")
		var names []string
		fresh := false
		for _, c := range cs {
			names = append(names, c.Name)
			if c.Name != old && c.DeletionTimestamp.IsZero() {
				fresh = true
			}
			if c.Name == old {
				return false, "old NodeClaim " + old + " still exists: " + strings.Join(names, ",")
			}
		}
		return fresh, strings.Join(names, ",")
	})
	eventually(t, long, time.Second, "the deployment running again", func() (bool, string) {
		got := &appsv1.Deployment{}
		if err := k8s.Get(ctx(), client.ObjectKeyFromObject(d), got); err != nil {
			return false, err.Error()
		}
		return got.Status.ReadyReplicas == 1 && got.Status.UpdatedReplicas == 1, fmt.Sprint(got.Status)
	})
}

// S09: an instance in the fake driver with the cluster tag and no NodeClaim is
// deleted by the garbage collector after the grace period.
func TestS09OrphanGC(t *testing.T) {
	// The collector looks in the regions the NodeClasses allow.
	createNodeClass(t, "s09", us1)
	typ := takeTypes(t, []string{us1}, 2, 1)[0]
	zone := byPrice(filterOfferings(reference(t, us1), func(o offering) bool { return isOnDemand(o) && o.API == typ }))[0].Zone
	suffix := time.Now().UnixNano() % 1_000_000
	old, young := fmt.Sprintf("orphan-old-%d", suffix), fmt.Sprintf("orphan-young-%d", suffix)
	setFake(t, map[string]string{"orphans": fmt.Sprintf(
		"- {name: %s, region: %s, zone: %s, instanceType: %s, capacityType: on-demand, ageSeconds: 600}\n"+
			"- {name: %s, region: %s, zone: %s, instanceType: %s, capacityType: on-demand, ageSeconds: 0}\n",
		old, us1, zone, typ, young, us1, zone, typ)})

	state := func(name string) string {
		_, insts := readState(t)
		for _, i := range insts {
			if i.NodeClaim == name {
				return i.State
			}
		}
		return "absent"
	}
	eventually(t, 2*time.Minute, time.Second, "the orphans exist", func() (bool, string) {
		s1, s2 := state(old), state(young)
		return s2 != "absent", fmt.Sprintf("%s=%s %s=%s", old, s1, young, s2)
	})
	appeared := time.Now()
	eventually(t, 3*time.Minute, time.Second, "the old orphan collected", func() (bool, string) {
		return state(old) == "terminated", state(old)
	})
	oldAfter := time.Since(appeared)
	eventually(t, 3*time.Minute, time.Second, "the young orphan collected", func() (bool, string) {
		return state(young) == "terminated", state(young)
	})
	youngAfter := time.Since(appeared)
	t.Logf("GC: the old orphan after %s, the young one after %s (grace period 30s, interval 10s)",
		oldAfter.Round(time.Second), youngAfter.Round(time.Second))
	if youngAfter < 15*time.Second {
		t.Errorf("the young orphan was deleted after %s, before the grace period", youngAfter)
	}
}

// S11: the node bootstrap. The NodeClass resolves its artifacts into the
// status and publishes a drift hash; a launch gets rendered user-data within the
// size budget and a bootstrap token, and the token is deleted, with an Event,
// when the NodeClaim registers.
func TestS11Bootstrap(t *testing.T) {
	nc := createNodeClass(t, "s11", us1)
	createNodePool(t, "s11", "s11", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	typ := takeTypes(t, []string{us1}, 2, 1)[0]

	got := &v1alpha1.SCNodeClass{}
	if err := k8s.Get(ctx(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if r := got.Status.Resolved; r == nil || r.Kubernetes["amd64"].SHA256 == "" || r.Kubernetes["arm64"].SHA256 == "" {
		t.Errorf("status.resolved = %+v, want the kubernetes sysext of both architectures", r)
	}
	if got.Status.Vendors[vendor].Hash == "" {
		t.Errorf("no drift hash: %+v", got.Status.Vendors)
	}

	createPod(t, "s11", map[string]string{labelType: vendor + "." + typ, labelNodePool: "s11"}, "100m")
	node := waitPodRunning(t, "s11", long)
	claims := waitClaims(t, "s11", 1, time.Minute)
	claim := claims[0].Name

	// The fake received the rendered user-data: more than an empty config, within
	// the 12 KiB the default templates are held to.
	eventually(t, time.Minute, time.Second, "the instance with its user-data", func() (bool, string) {
		for _, i := range liveInstances(t) {
			if i.NodeClaim == claim {
				return i.UserDataBytes > 1000 && i.UserDataBytes <= 12*1024, fmt.Sprintf("%d bytes of user-data", i.UserDataBytes)
			}
		}
		return false, "no instance of " + claim
	})

	// The token of a registered NodeClaim is gone, and the controller said so.
	eventually(t, time.Minute, time.Second, "the bootstrap token deleted", func() (bool, string) {
		var tokens corev1.SecretList
		if err := k8s.List(ctx(), &tokens, client.InNamespace("kube-system"),
			client.MatchingLabels{v1alpha1.Group + "/nodeclaim": claim}); err != nil {
			return false, err.Error()
		}
		return len(tokens.Items) == 0, fmt.Sprintf("%d tokens left", len(tokens.Items))
	})
	eventually(t, time.Minute, time.Second, "the Event of the deletion", func() (bool, string) {
		evs, err := clientset.CoreV1().Events("").List(ctx(), metav1.ListOptions{FieldSelector: "reason=BootstrapTokenDeleted"})
		if err != nil {
			return false, err.Error()
		}
		for _, e := range evs.Items {
			if e.InvolvedObject.Name == claim {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d BootstrapTokenDeleted events, none for %s", len(evs.Items), claim)
	})
	deletePodAndWaitNode(t, "s11", node)
	waitNoClaims(t, "s11", long)
}

// quotaMetrics renders the tracker's gauges, for failure messages.
func quotaMetrics(t *testing.T) string {
	var sb strings.Builder
	for k, v := range metrics(t) {
		if strings.HasPrefix(k, "scarpenter_quota_") && !strings.Contains(k, "age_seconds") {
			fmt.Fprintf(&sb, "%s=%v ", k, v)
		}
	}
	return sb.String()
}
