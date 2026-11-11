//go:build kwok

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// The billing retention scenarios (plan 23 section 6, L3). Core's clock is real,
// so the fake vendor's billing rule is shortened: a period of 4 minutes held with
// a 90 second margin, so that a node is held for 2m30s after its billing start
// and released before the boundary at 4 minutes. The NodeClass minPeriod is 3m
// (the least the 90 second margin allows: twice the margin).
const (
	rPeriod   = 4 * time.Minute
	rMargin   = 90 * time.Second
	rDeadline = rPeriod - rMargin
)

// steerBilling sets the fake vendor's billing rule: the increment and the minimum,
// verified (the fake says it checked them live).
func steerBilling(t *testing.T, increment, minimum string) {
	t.Helper()
	setFake(t, map[string]string{"billing": fmt.Sprintf("increment: %s\nminimum: %s\nverified: true\nsource: the L3 lane\n", increment, minimum)})
}

// billingClass creates an SCNodeClass of the fake vendor that opts in to
// retention with the margin and minPeriod of the lane.
func billingClass(t *testing.T, name string) *v1alpha1.SCNodeClass {
	t.Helper()
	return createNodeClassWith(t, name, func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Billing = &v1alpha1.BillingSpec{
			RetainUntilPeriodEnd: true,
			Margin:               &metav1.Duration{Duration: rMargin},
			MinPeriod:            &metav1.Duration{Duration: 3 * time.Minute},
		}
	}, us1)
}

type rPoolOpts struct {
	budgets     []karpv1.Budget
	limitCPU    string
	expireAfter string
}

// retentionPool creates a NodePool that consolidates empty nodes at once (what a
// hold has to defeat) with the options, and removes it with the test.
func retentionPool(t *testing.T, name, class string, o rPoolOpts, reqs ...karpv1.NodeSelectorRequirementWithMinValues) *karpv1.NodePool {
	t.Helper()
	all := append([]karpv1.NodeSelectorRequirementWithMinValues{in(v1alpha1.LabelVendor, vendor)}, reqs...)
	np := &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
				NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: class},
				Requirements: all,
			}},
			Disruption: karpv1.Disruption{
				ConsolidationPolicy: karpv1.ConsolidationPolicyWhenEmpty,
				ConsolidateAfter:    karpv1.MustParseNillableDuration("0s"),
				Budgets:             o.budgets,
			},
			Limits: karpv1.Limits{corev1.ResourceCPU: resource.MustParse("100000")},
		},
	}
	if o.limitCPU != "" {
		np.Spec.Limits = karpv1.Limits{corev1.ResourceCPU: resource.MustParse(o.limitCPU)}
	}
	if o.expireAfter != "" {
		np.Spec.Template.Spec.ExpireAfter = karpv1.MustParseNillableDuration(o.expireAfter)
	}
	if err := k8s.Create(ctx(), np); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k8s.Delete(ctx(), np); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting NodePool %s: %v", name, err)
		}
		waitNoClaims(t, name, 5*time.Minute)
	})
	eventually(t, 2*time.Minute, time.Second, "NodePool "+name+" Ready", func() (bool, string) {
		got := &karpv1.NodePool{}
		if err := k8s.Get(ctx(), client.ObjectKeyFromObject(np), got); err != nil {
			return false, err.Error()
		}
		return got.StatusConditions().IsTrue(karpv1.ConditionTypeNodeClassReady) &&
			got.StatusConditions().IsTrue(karpv1.ConditionTypeValidationSucceeded), fmt.Sprintf("%+v", got.Status.Conditions)
	})
	return np
}

// jobDone ends a job: its pod goes away and the node is empty.
func jobDone(t *testing.T, pod string) {
	t.Helper()
	if err := k8s.Delete(ctx(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: "default"}},
		client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func getNode(t *testing.T, name string) *corev1.Node {
	t.Helper()
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: name}, n); err != nil {
		t.Fatal(err)
	}
	return n
}

// billingStart reads the billing-start annotation of the NodeClaim, which the
// fake driver stamped at creation.
func billingStart(t *testing.T, claim string) time.Time {
	t.Helper()
	nc := &karpv1.NodeClaim{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: claim}, nc); err != nil {
		t.Fatal(err)
	}
	raw := nc.Annotations[v1alpha1.AnnotationBillingStart]
	if raw == "" {
		t.Fatalf("NodeClaim %s has no %s annotation: %v", claim, v1alpha1.AnnotationBillingStart, nc.Annotations)
	}
	start, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	return start
}

// isHeld reports whether the node carries the hold: do-not-disrupt and the
// marker with the deadline.
func isHeld(name string) (held bool, deadline time.Time, why string) {
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: name}, n); err != nil {
		return false, time.Time{}, err.Error()
	}
	raw := n.Annotations[v1alpha1.AnnotationRetainUntil]
	if raw == "" || n.Annotations[karpv1.DoNotDisruptAnnotationKey] != "true" {
		return false, time.Time{}, fmt.Sprintf("annotations %v", n.Annotations)
	}
	d, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false, time.Time{}, err.Error()
	}
	return true, d, ""
}

func waitHeld(t *testing.T, node string) time.Time {
	t.Helper()
	var deadline time.Time
	eventually(t, time.Minute, 500*time.Millisecond, "node "+node+" held", func() (bool, string) {
		ok, d, why := isHeld(node)
		deadline = d
		return ok, why
	})
	return deadline
}

// claimGone reports whether the NodeClaim is gone or being deleted.
func claimGone(claim string) bool {
	nc := &karpv1.NodeClaim{}
	err := k8s.Get(ctx(), client.ObjectKey{Name: claim}, nc)
	return apierrors.IsNotFound(err) || (err == nil && !nc.DeletionTimestamp.IsZero())
}

// waitClaimGone waits for the NodeClaim to go and returns the time it was seen
// gone.
func waitClaimGone(t *testing.T, claim string, timeout time.Duration) time.Time {
	t.Helper()
	var at time.Time
	eventually(t, timeout, 500*time.Millisecond, "NodeClaim "+claim+" deleted", func() (bool, string) {
		if claimGone(claim) {
			at = time.Now()
			return true, ""
		}
		return false, "still there"
	})
	return at
}

// sleepUntil waits for a point in time.
func sleepUntil(at time.Time) {
	if d := time.Until(at); d > 0 {
		time.Sleep(d)
	}
}

// nodeEvents lists the Events of the Node with the reason.
func nodeEvents(t *testing.T, node, reason string) []corev1.Event {
	t.Helper()
	evs, err := clientset.CoreV1().Events("").List(ctx(), metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Node,involvedObject.name=" + node + ",reason=" + reason})
	if err != nil {
		t.Fatal(err)
	}
	return evs.Items
}

func metricValue(t *testing.T, name string) float64 {
	t.Helper()
	return metrics(t)[name]
}

const (
	mReuse    = `scarpenter_billing_reuse_total{vendor="aws"}`
	mDeletes  = `scarpenter_billing_deletes_at_deadline_total{vendor="aws"}`
	mMissed   = `scarpenter_billing_boundary_missed_total{vendor="aws"}`
	mBlocked  = `scarpenter_billing_blocked_by_budget_total{vendor="aws"}`
	mPressure = `scarpenter_billing_released_under_pressure_total{scope="limit",vendor="aws"}`
	mOvershot = `scarpenter_billing_budget_overshoot_total{vendor="aws"}`
)

// R01 (L3 a and c): jobs that come back within the period reuse one node, and
// when no job comes the empty node is gone before the boundary.
func TestR01ReuseThenReleaseBeforeTheBoundary(t *testing.T) {
	steerBilling(t, "4m", "4m")
	billingClass(t, "r01")
	retentionPool(t, "r01", "r01", rPoolOpts{}, in(labelRegion, us1), in(labelCapacity, "on-demand"),
		in(labelType, apiNames(takeTypes(t, []string{us1}, 2, 1))...))
	sel := map[string]string{labelNodePool: "r01"}
	reuses := metricValue(t, mReuse)
	deletes := metricValue(t, mDeletes)

	createPod(t, "r01-job1", sel, "100m")
	node := waitPodRunning(t, "r01-job1", long)
	claim := waitClaims(t, "r01", 1, time.Minute)[0].Name
	start := billingStart(t, claim)
	t.Logf("billing start %s, deadline %s, boundary %s", start.Format(time.TimeOnly),
		start.Add(rDeadline).Format(time.TimeOnly), start.Add(rPeriod).Format(time.TimeOnly))

	// The job ends, the node is empty and held: Karpenter, at consolidateAfter 0s,
	// would delete it at once without the hold.
	jobDone(t, "r01-job1")
	deadline := waitHeld(t, node)
	if want := start.Add(rDeadline); deadline.Sub(want).Abs() > 2*time.Second {
		t.Errorf("deadline %s, want the billing start %s + %s", deadline, start, rDeadline)
	}
	time.Sleep(20 * time.Second)
	if claimGone(claim) {
		t.Fatal("the empty held node was deleted before its deadline")
	}
	for i := 2; i <= 3; i++ {
		pod := fmt.Sprintf("r01-job%d", i)
		createPod(t, pod, sel, "100m")
		if got := waitPodRunning(t, pod, time.Minute); got != node {
			t.Fatalf("job %d ran on %s, want the held node %s", i, got, node)
		}
		if cs, _ := listClaims("r01"); len(cs) != 1 {
			t.Fatalf("a second NodeClaim appeared: %d", len(cs))
		}
		jobDone(t, pod)
		time.Sleep(15 * time.Second)
	}
	if time.Now().After(start.Add(rDeadline)) {
		t.Fatalf("the jobs took too long: the deadline %s has passed", start.Add(rDeadline))
	}

	// No more jobs: the empty node is released at the deadline, before the boundary.
	at := waitClaimGone(t, claim, long)
	if !at.Before(start.Add(rPeriod)) {
		t.Errorf("the node was gone at %s, after the boundary %s", at.Format(time.TimeOnly), start.Add(rPeriod).Format(time.TimeOnly))
	}
	if at.Before(start.Add(rDeadline - 5*time.Second)) {
		t.Errorf("the node was gone at %s, before the deadline %s", at.Format(time.TimeOnly), start.Add(rDeadline).Format(time.TimeOnly))
	}
	waitNodeGone(t, node)
	if got := metricValue(t, mReuse) - reuses; got < 2 {
		t.Errorf("reuse_total moved by %v, want at least 2", got)
	}
	if got := metricValue(t, mDeletes) - deletes; got != 1 {
		t.Errorf("deletes_at_deadline_total moved by %v, want 1", got)
	}
	if got := metricValue(t, mMissed); got != 0 {
		t.Errorf("boundary_missed_total = %v: the margin was too small", got)
	}
	if len(nodeEvents(t, node, "ReleasedAtPeriodEnd")) == 0 {
		t.Error("no ReleasedAtPeriodEnd Event on the node")
	}
}

// R02 (L3 a2): a vendor with a minimum and per-second billing after it: jobs
// within the minimum reuse the node, which is gone right before the minimum
// ends, and the next job gets a fresh one.
func TestR02MinimumOnly(t *testing.T) {
	steerBilling(t, "1s", "4m")
	billingClass(t, "r02")
	retentionPool(t, "r02", "r02", rPoolOpts{}, in(labelRegion, us1), in(labelCapacity, "on-demand"),
		in(labelType, apiNames(takeTypes(t, []string{us1}, 2, 1))...))
	sel := map[string]string{labelNodePool: "r02"}

	createPod(t, "r02-job1", sel, "100m")
	node := waitPodRunning(t, "r02-job1", long)
	claim := waitClaims(t, "r02", 1, time.Minute)[0].Name
	start := billingStart(t, claim)
	jobDone(t, "r02-job1")
	waitHeld(t, node)

	time.Sleep(30 * time.Second)
	createPod(t, "r02-job2", sel, "100m")
	if got := waitPodRunning(t, "r02-job2", time.Minute); got != node {
		t.Fatalf("the second job ran on %s, want the held node %s", got, node)
	}
	jobDone(t, "r02-job2")

	at := waitClaimGone(t, claim, long)
	if !at.Before(start.Add(rPeriod)) || at.Before(start.Add(rDeadline-5*time.Second)) {
		t.Errorf("the node went at %s, want it between the deadline %s and the end of the minimum %s", at.Format(time.TimeOnly),
			start.Add(rDeadline).Format(time.TimeOnly), start.Add(rPeriod).Format(time.TimeOnly))
	}
	waitNodeGone(t, node)
	sleepUntil(start.Add(rPeriod + 5*time.Second))
	createPod(t, "r02-job3", sel, "100m")
	if got := waitPodRunning(t, "r02-job3", long); got == node {
		t.Errorf("the third job ran on the released node %s", got)
	}
	if cs := waitClaims(t, "r02", 1, time.Minute); cs[0].Name == claim {
		t.Errorf("the third job reused NodeClaim %s", claim)
	}
}

// R03 (L3 b): a node kept busy across the deadline is not deleted, its hold is
// released for the window and put back after the boundary.
func TestR03BusyAcrossTheDeadline(t *testing.T) {
	steerBilling(t, "4m", "4m")
	billingClass(t, "r03")
	retentionPool(t, "r03", "r03", rPoolOpts{}, in(labelRegion, us1), in(labelCapacity, "on-demand"),
		in(labelType, apiNames(takeTypes(t, []string{us1}, 2, 1))...))
	createPod(t, "r03-job", map[string]string{labelNodePool: "r03"}, "100m")
	node := waitPodRunning(t, "r03-job", long)
	claim := waitClaims(t, "r03", 1, time.Minute)[0].Name
	start := billingStart(t, claim)
	waitHeld(t, node)

	sleepUntil(start.Add(rDeadline + 15*time.Second))
	if claimGone(claim) {
		t.Fatal("a busy node was deleted at its deadline")
	}
	if ok, _, _ := isHeld(node); ok {
		t.Error("the hold must be released for the window before the boundary")
	}
	if _, ok := getNode(t, node).Annotations[v1alpha1.AnnotationRetainUntil]; ok {
		t.Error("the marker must go with the hold")
	}

	sleepUntil(start.Add(rPeriod + 15*time.Second))
	deadline := waitHeld(t, node)
	if want := start.Add(2*rPeriod - rMargin); deadline.Sub(want).Abs() > 2*time.Second {
		t.Errorf("the hold is back with the deadline %s, want %s", deadline, want)
	}
	if claimGone(claim) {
		t.Error("the busy node was deleted")
	}
	if got, err := podNode("r03-job"); err != nil || got != node {
		t.Errorf("the job moved: %q %v", got, err)
	}
}

// R04 (L3 d): a vendor that bills per second ignores the setting.
func TestR04PerSecondVendorIgnoresTheSetting(t *testing.T) {
	steerBilling(t, "1s", "0s")
	billingClass(t, "r04")
	retentionPool(t, "r04", "r04", rPoolOpts{}, in(labelRegion, us1), in(labelCapacity, "on-demand"),
		in(labelType, apiNames(takeTypes(t, []string{us1}, 2, 1))...))
	createPod(t, "r04-job", map[string]string{labelNodePool: "r04"}, "100m")
	node := waitPodRunning(t, "r04-job", long)
	if ok, _, _ := isHeld(node); ok {
		t.Error("a per-second vendor's node must not be held")
	}
	jobDone(t, "r04-job")
	waitNodeGone(t, node)

	eventually(t, time.Minute, time.Second, "status.billing says PerSecondBilling", func() (bool, string) {
		nc := &v1alpha1.SCNodeClass{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: "r04"}, nc); err != nil {
			return false, err.Error()
		}
		if len(nc.Status.Billing) != 1 {
			return false, fmt.Sprintf("%+v", nc.Status.Billing)
		}
		b := nc.Status.Billing[0]
		return !b.Active && b.Reason == "PerSecondBilling", fmt.Sprintf("%+v", b)
	})
}

// R05 (L3 e): the hold does not stop expireAfter, and an expireAfter of the
// period minus the margin ends the node before the boundary.
func TestR05ExpireAfter(t *testing.T) {
	steerBilling(t, "4m", "4m")
	billingClass(t, "r05")
	for _, tc := range []struct {
		name, expire string
		by           func(start time.Time) time.Time
	}{
		{"shorter than the period", "100s", func(s time.Time) time.Time { return s.Add(rDeadline - 5*time.Second) }},
		{"the period minus the margin", "150s", func(s time.Time) time.Time { return s.Add(rPeriod) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := "r05-" + tc.expire
			retentionPool(t, pool, "r05", rPoolOpts{expireAfter: tc.expire}, in(labelRegion, us1), in(labelCapacity, "on-demand"),
				in(labelType, apiNames(takeTypes(t, []string{us1}, 2, 1))...))
			createPod(t, pool, map[string]string{labelNodePool: pool}, "100m")
			node := waitPodRunning(t, pool, long)
			claim := waitClaims(t, pool, 1, time.Minute)[0].Name
			start := billingStart(t, claim)
			jobDone(t, pool)
			waitHeld(t, node)
			at := waitClaimGone(t, claim, long)
			if !at.Before(tc.by(start)) {
				t.Errorf("the node was gone at %s, want before %s", at.Format(time.TimeOnly), tc.by(start).Format(time.TimeOnly))
			}
		})
	}
}

// R06 (L3 f): a budget of 0 for Empty holds the empty node into the next period
// and it goes at the next deadline once the budget opens.
func TestR06BudgetOfZero(t *testing.T) {
	steerBilling(t, "4m", "4m")
	billingClass(t, "r06")
	pool := retentionPool(t, "r06", "r06", rPoolOpts{budgets: []karpv1.Budget{
		{Nodes: "0", Reasons: []karpv1.DisruptionReason{karpv1.DisruptionReasonEmpty}}, {Nodes: "100%"},
	}}, in(labelRegion, us1), in(labelCapacity, "on-demand"), in(labelType, apiNames(takeTypes(t, []string{us1}, 2, 1))...))
	createPod(t, "r06-job", map[string]string{labelNodePool: "r06"}, "100m")
	node := waitPodRunning(t, "r06-job", long)
	claim := waitClaims(t, "r06", 1, time.Minute)[0].Name
	start := billingStart(t, claim)
	blocked := metricValue(t, mBlocked)
	jobDone(t, "r06-job")
	waitHeld(t, node)

	sleepUntil(start.Add(rDeadline + 20*time.Second))
	if claimGone(claim) {
		t.Fatal("a budget of 0 for Empty forbids the delete")
	}
	if ok, _, why := isHeld(node); !ok {
		t.Errorf("the node stays held into the next period: %s", why)
	}
	if len(nodeEvents(t, node, "ReleaseBlockedByBudget")) == 0 {
		t.Error("no ReleaseBlockedByBudget Event on the node")
	}
	if got := metricValue(t, mBlocked) - blocked; got < 1 {
		t.Errorf("blocked_by_budget_total moved by %v", got)
	}

	sleepUntil(start.Add(rPeriod + 15*time.Second))
	deadline := waitHeld(t, node)
	if want := start.Add(2*rPeriod - rMargin); deadline.Sub(want).Abs() > 2*time.Second {
		t.Errorf("the second deadline is %s, want %s", deadline, want)
	}
	if claimGone(claim) {
		t.Fatal("deleted in the second period although the budget is closed")
	}
	// The window opens: the node goes at the next deadline.
	got := &karpv1.NodePool{}
	if err := k8s.Get(ctx(), client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Disruption.Budgets = []karpv1.Budget{{Nodes: "100%"}}
	if err := k8s.Update(ctx(), got); err != nil {
		t.Fatal(err)
	}
	at := waitClaimGone(t, claim, long)
	if !at.Before(start.Add(2 * rPeriod)) {
		t.Errorf("the node went at %s, after the second boundary %s", at.Format(time.TimeOnly), start.Add(2*rPeriod).Format(time.TimeOnly))
	}
	if got := metricValue(t, mMissed); got != 0 {
		t.Errorf("boundary_missed_total = %v: a budget block is not a missed boundary", got)
	}
}

// R07 (L3 f, the budget race): a budget of 1 with two empty nodes at their
// deadlines: never two releases at once, and no overshoot.
func TestR07BudgetOfOneWithTwoEmptyNodes(t *testing.T) {
	steerBilling(t, "4m", "4m")
	billingClass(t, "r07")
	retentionPool(t, "r07", "r07", rPoolOpts{budgets: []karpv1.Budget{{Nodes: "1"}}}, in(labelRegion, us1),
		in(labelCapacity, "on-demand"), in(labelType, apiNames(takeTypes(t, []string{us1}, 2, 1))...))
	sel := map[string]string{labelNodePool: "r07"}
	overshoot := metricValue(t, mOvershot)
	// 1500m does not fit twice on a 2-vCPU node: two nodes.
	createPod(t, "r07-a", sel, "1500m")
	createPod(t, "r07-b", sel, "1500m")
	waitPodRunning(t, "r07-a", long)
	waitPodRunning(t, "r07-b", long)
	claims := waitClaims(t, "r07", 2, time.Minute)
	start := billingStart(t, claims[0].Name)
	if s := billingStart(t, claims[1].Name); s.Before(start) {
		start = s
	}
	jobDone(t, "r07-a")
	jobDone(t, "r07-b")

	sleepUntil(start.Add(rDeadline - 5*time.Second))
	maxReleasing, gone := 0, 0
	for end := start.Add(rDeadline + 45*time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
		releasing := map[string]bool{}
		var nodes corev1.NodeList
		if err := k8s.List(ctx(), &nodes, client.MatchingLabels{labelNodePool: "r07"}); err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes.Items {
			for _, taint := range n.Spec.Taints {
				if taint.Key == v1alpha1.TaintReleasing {
					releasing[n.Name] = true
				}
			}
		}
		cs, _ := listClaims("r07")
		gone = 2 - len(cs)
		for _, c := range cs {
			if !c.DeletionTimestamp.IsZero() {
				releasing[c.Name] = true
			}
		}
		maxReleasing = max(maxReleasing, len(releasing))
	}
	if maxReleasing > 1 {
		t.Errorf("%d nodes were being released at once under a budget of 1", maxReleasing)
	}
	if gone < 1 && maxReleasing < 1 {
		t.Error("no release happened at the deadline")
	}
	if got := metricValue(t, mOvershot) - overshoot; got != 0 {
		t.Errorf("budget_overshoot_total moved by %v", got)
	}
}

// R08 (L3 g): a NodePool at its limits with a held empty node: the pending pod
// gets the capacity after SCarpenter releases the node, long before its deadline.
func TestR08LimitReleasesAHeldNode(t *testing.T) {
	steerBilling(t, "4m", "4m")
	billingClass(t, "r08")
	types := takeTypes(t, []string{us1}, 2, 2)
	retentionPool(t, "r08", "r08", rPoolOpts{limitCPU: "2"}, in(labelRegion, us1), in(labelCapacity, "on-demand"),
		in(labelType, apiNames(types)...))
	pressure := metricValue(t, mPressure)

	createPod(t, "r08-a", map[string]string{labelNodePool: "r08", labelType: vendor + "." + types[0]}, "100m")
	node := waitPodRunning(t, "r08-a", long)
	claim := waitClaims(t, "r08", 1, time.Minute)[0].Name
	start := billingStart(t, claim)
	jobDone(t, "r08-a")
	waitHeld(t, node)

	// The next job needs another instance type: a new node would exceed the limit.
	createPod(t, "r08-b", map[string]string{labelNodePool: "r08", labelType: vendor + "." + types[1]}, "100m")
	other := waitPodRunning(t, "r08-b", 3*time.Minute)
	if other == node {
		t.Fatalf("the job ran on the held node %s of another type", node)
	}
	if time.Now().After(start.Add(rDeadline)) {
		t.Errorf("the job was served after the deadline %s: the node was not released under pressure", start.Add(rDeadline))
	}
	// The release taints the node at once (the pending pod's new node can come up
	// then), and deletes the NodeClaim after the drain wait: wait for it.
	if !claimGone(claim) {
		waitClaimGone(t, claim, time.Minute)
	}
	eventually(t, 30*time.Second, 500*time.Millisecond, "a ReleasedUnderPressure Event on the node", func() (bool, string) {
		return len(nodeEvents(t, node, "ReleasedUnderPressure")) > 0, ""
	})
	if got := metricValue(t, mPressure) - pressure; got < 1 {
		t.Errorf("released_under_pressure_total{scope=limit} moved by %v", got)
	}
}

// R09 (L3 h): a pending pod created one second before the deadline never ends
// on a node that is being deleted, and no boundary is missed.
func TestR09PodJustBeforeTheDeadline(t *testing.T) {
	steerBilling(t, "4m", "4m")
	billingClass(t, "r09")
	retentionPool(t, "r09", "r09", rPoolOpts{}, in(labelRegion, us1), in(labelCapacity, "on-demand"),
		in(labelType, apiNames(takeTypes(t, []string{us1}, 2, 1))...))
	sel := map[string]string{labelNodePool: "r09"}
	createPod(t, "r09-a", sel, "100m")
	node := waitPodRunning(t, "r09-a", long)
	jobDone(t, "r09-a")
	deadline := waitHeld(t, node)

	sleepUntil(deadline.Add(-time.Second))
	createPod(t, "r09-b", sel, "100m")
	where := waitPodRunning(t, "r09-b", long)
	t.Logf("the pod ran on %s (held node %s)", where, node)
	consistently(t, 45*time.Second, time.Second, "the pod must stay on a node that is not being deleted", func() (bool, string) {
		got := &corev1.Pod{}
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: "r09-b"}, got); err != nil {
			return true, err.Error()
		}
		if got.Status.Phase != corev1.PodRunning || got.Spec.NodeName != where {
			return true, fmt.Sprintf("pod %s on %q", got.Status.Phase, got.Spec.NodeName)
		}
		if got.DeletionTimestamp != nil {
			return true, "the pod is being deleted (evicted)"
		}
		n := &corev1.Node{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: where}, n); err != nil {
			return true, err.Error()
		}
		if !n.DeletionTimestamp.IsZero() || claimGone(n.Name) {
			return true, "the node is being deleted"
		}
		return false, ""
	})
	if got := metricValue(t, mMissed); got != 0 {
		t.Errorf("boundary_missed_total = %v", got)
	}
}
