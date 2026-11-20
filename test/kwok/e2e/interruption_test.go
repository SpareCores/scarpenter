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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// The interruption scenarios (plan 26 section 10, I01 to I06). The harness plays
// the node agent by annotating the Node itself (I01, I02, I03, I06), or lets the
// fake driver announce the reclaim to the controller's poller (I04) or take the
// instance away without a word (I05). The chart values of the lane shorten the
// TTL of an interrupted offering to 30 s; the margin is the default 5 s.

const (
	// interruptionMargin is --interruption-margin, the default.
	interruptionMargin = 5 * time.Second
	// interruptedTTL is interruption.unavailableTTL of manifests/chart-values.yaml.
	interruptedTTL = 30 * time.Second
	// podHold keeps a Pod that was deleted visible with its deletionTimestamp
	// and grace period until the test lets it go.
	podHold = "scarpenter-e2e/hold"
)

// takeSpot returns the cheapest spot offering in us-east-1 of a 2-vCPU type no
// other scenario uses, and claims the type.
func takeSpot(t *testing.T) offering {
	t.Helper()
	takenMu.Lock()
	defer takenMu.Unlock()
	spot := byPrice(filterOfferings(reference(t, us1), func(o offering) bool {
		return o.Capacity == karpv1.CapacityTypeSpot && o.VCPUs == 2 && o.AllocMem >= minAllocMem && !taken[o.API]
	}))
	if len(spot) == 0 {
		t.Fatalf("the snapshot has no spot offering of a 2-vCPU type in %s", us1)
	}
	taken[spot[0].API] = true
	return spot[0]
}

// interruptionPool creates the NodeClass and a NodePool that allows only the
// offering (its type and zone, with its capacity type).
func interruptionPool(t *testing.T, name string, o offering) {
	t.Helper()
	createNodeClass(t, name, us1)
	createNodePool(t, name, name, in(labelRegion, us1), in(labelCapacity, o.Capacity),
		in(labelType, o.Type), in(labelZone, o.Zone))
}

// heldPod creates a Pod on the pool that a deletion leaves terminating, so the
// test can read the grace period the node's drain gave it. It is released with
// the test.
func heldPod(t *testing.T, name, pool string, grace int64) {
	t.Helper()
	spec := podSpec(map[string]string{labelNodePool: pool}, "100m")
	spec.TerminationGracePeriodSeconds = new(grace)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", Labels: map[string]string{labelOwner: ownerLabel(t)}, Finalizers: []string{podHold},
		},
		Spec: spec,
	}
	if err := k8s.Create(ctx(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releasePod(t, name) })
}

func releasePod(t *testing.T, name string) {
	t.Helper()
	p := &corev1.Pod{}
	if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: name}, p); err != nil {
		return
	}
	stored := p.DeepCopy()
	p.Finalizers = slices.DeleteFunc(p.Finalizers, func(f string) bool { return f == podHold })
	if err := k8s.Patch(ctx(), p, client.MergeFrom(stored)); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("releasing pod %s: %v", name, err)
	}
	if err := k8s.Delete(ctx(), p, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("deleting pod %s: %v", name, err)
	}
}

// spotNode starts a held Pod on a pool of one spot offering and returns its node
// and NodeClaim.
func spotNode(t *testing.T, name string, grace int64) (node, claim string, o offering) {
	t.Helper()
	o = takeSpot(t)
	interruptionPool(t, name, o)
	heldPod(t, name, name, grace)
	node = waitPodRunning(t, name, long)
	claims := waitClaims(t, name, 1, time.Minute)
	return node, claims[0].Name, o
}

// annotateNode writes the annotations the node agent writes.
func annotateNode(t *testing.T, node string, deadline time.Time) {
	t.Helper()
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: node}, n); err != nil {
		t.Fatal(err)
	}
	stored := n.DeepCopy()
	n.Annotations[v1alpha1.AnnotationInterruption] = v1alpha1.ReasonSpotInterrupted
	n.Annotations[v1alpha1.AnnotationInterruptionDeadline] = deadline.UTC().Format(time.RFC3339)
	n.Annotations[v1alpha1.AnnotationInterruptionSource] = v1alpha1.SourceInstanceMetadata
	if err := k8s.Patch(ctx(), n, client.MergeFrom(stored)); err != nil {
		t.Fatal(err)
	}
}

func getClaim(t *testing.T, name string) *karpv1.NodeClaim {
	t.Helper()
	c := &karpv1.NodeClaim{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: name}, c); err != nil {
		t.Fatal(err)
	}
	return c
}

// objEvents lists the Events of an object of the kind with the reason.
func objEvents(t *testing.T, kind, name, reason string) []corev1.Event {
	t.Helper()
	evs, err := clientset.CoreV1().Events("").List(ctx(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.kind=%s,involvedObject.name=%s,reason=%s", kind, name, reason)})
	if err != nil {
		t.Fatal(err)
	}
	return evs.Items
}

// waitPodTerminating waits for the pod to have a deletionTimestamp and returns
// it with the time the wait saw it.
func waitPodTerminating(t *testing.T, name string, timeout time.Duration) (*corev1.Pod, time.Time) {
	t.Helper()
	var pod *corev1.Pod
	var seen time.Time
	eventually(t, timeout, 50*time.Millisecond, "pod "+name+" deleted by the drain", func() (bool, string) {
		p := &corev1.Pod{}
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: name}, p); err != nil {
			return false, err.Error()
		}
		if p.DeletionTimestamp.IsZero() {
			return false, "not deleted yet"
		}
		pod, seen = p, time.Now()
		return true, ""
	})
	return pod, seen
}

// interruptedMetric is the key of scarpenter_interruptions_total for an offering.
func interruptedMetric(o offering, reason, source string) string {
	return fmt.Sprintf(`scarpenter_interruptions_total{instance_type="%s",reason="%s",source="%s",vendor="%s",zone="%s"}`,
		o.Type, reason, source, vendor, o.Zone)
}

// checkInterrupted asserts what every interruption leaves on the NodeClaim and
// Node: the condition, the three annotations (the Node's and the NodeClaim's
// agree), the termination deadline margin before the deadline, and the Events.
func checkInterrupted(t *testing.T, node, claim, source string) (deadline time.Time) {
	t.Helper()
	eventually(t, time.Minute, 200*time.Millisecond, "NodeClaim "+claim+" marked Interrupted", func() (bool, string) {
		c := &karpv1.NodeClaim{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: claim}, c); err != nil {
			return false, err.Error()
		}
		cond := c.StatusConditions().Get(v1alpha1.ConditionInterrupted)
		return cond.IsTrue() && cond.Reason == v1alpha1.ReasonSpotInterrupted, fmt.Sprintf("%+v", cond)
	})
	c := getClaim(t, claim)
	if cond := c.StatusConditions().Get(karpv1.ConditionTypeDisruptionReason); !cond.IsTrue() || cond.Reason != v1alpha1.ReasonSpotInterrupted {
		t.Errorf("DisruptionReason = %+v, want True with reason SpotInterrupted", cond)
	}
	if c.DeletionTimestamp.IsZero() {
		t.Error("the NodeClaim was not deleted")
	}
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: node}, n); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{v1alpha1.AnnotationInterruption, v1alpha1.AnnotationInterruptionDeadline, v1alpha1.AnnotationInterruptionSource} {
		if c.Annotations[key] == "" || c.Annotations[key] != n.Annotations[key] {
			t.Errorf("annotation %s: NodeClaim %q, Node %q", key, c.Annotations[key], n.Annotations[key])
		}
	}
	if got := c.Annotations[v1alpha1.AnnotationInterruptionSource]; got != source {
		t.Errorf("source = %q, want %q", got, source)
	}
	deadline, err := time.Parse(time.RFC3339, c.Annotations[v1alpha1.AnnotationInterruptionDeadline])
	if err != nil {
		t.Fatalf("deadline annotation: %v", err)
	}
	termination, err := time.Parse(time.RFC3339, c.Annotations[karpv1.NodeClaimTerminationTimestampAnnotationKey])
	if err != nil {
		t.Fatalf("termination timestamp: %v (annotations %v)", err, c.Annotations)
	}
	if !termination.Equal(deadline.Add(-interruptionMargin)) {
		t.Errorf("termination timestamp %s, want the deadline %s minus %s", termination, deadline, interruptionMargin)
	}
	eventually(t, 30*time.Second, 500*time.Millisecond, "SpotInterrupted Events on the Node and the NodeClaim", func() (bool, string) {
		nodes, claims := len(objEvents(t, "Node", node, "SpotInterrupted")), len(objEvents(t, "NodeClaim", claim, "SpotInterrupted"))
		return nodes == 1 && claims == 1, fmt.Sprintf("%d on the Node, %d on the NodeClaim", nodes, claims)
	})
	return deadline
}

// I01: a Node annotated with a deadline 60 s ahead. The NodeClaim is marked
// Interrupted with SpotInterrupted, its termination timestamp is the deadline minus 5 s, both
// objects get the Event, and the Pod's deletionTimestamp is set within 5 s of
// the annotation.
func TestI01NoticeDrainsAtOnce(t *testing.T) {
	node, claim, _ := spotNode(t, "i01", 30)
	deadline := time.Now().Add(60 * time.Second).UTC().Truncate(time.Second)
	annotated := time.Now()
	annotateNode(t, node, deadline)

	_, seen := waitPodTerminating(t, "i01", 30*time.Second)
	if took := seen.Sub(annotated); took > 5*time.Second {
		t.Errorf("the Pod's deletionTimestamp was set %s after the annotation, want within 5 s", took.Round(100*time.Millisecond))
	} else {
		t.Logf("annotation to the Pod's deletionTimestamp: %s", took.Round(50*time.Millisecond))
	}
	got := checkInterrupted(t, node, claim, v1alpha1.SourceInstanceMetadata)
	if !got.Equal(deadline) {
		t.Errorf("recorded deadline %s, want %s", got, deadline)
	}
}

// I02: a 30 s deadline and a Pod with terminationGracePeriodSeconds 20: the drain
// deletes it with its grace, 20 s, which fits before the termination deadline
// (25 s ahead).
func TestI02ShortNoticeKeepsThePodsGrace(t *testing.T) {
	node, claim, _ := spotNode(t, "i02", 20)
	deadline := time.Now().Add(30 * time.Second).UTC().Truncate(time.Second)
	annotated := time.Now()
	annotateNode(t, node, deadline)

	pod, seen := waitPodTerminating(t, "i02", 30*time.Second)
	if took := seen.Sub(annotated); took > 5*time.Second {
		t.Errorf("the Pod's deletionTimestamp was set %s after the annotation, want within 5 s", took.Round(100*time.Millisecond))
	}
	grace := ptr.Deref(pod.DeletionGracePeriodSeconds, -1)
	if grace < 1 || grace > 20 {
		t.Errorf("deletionGracePeriodSeconds = %d, want 20 (or the time left, which is at least 20 here)", grace)
	}
	checkInterrupted(t, node, claim, v1alpha1.SourceInstanceMetadata)
}

// I03: the replacement of an interrupted node avoids the interrupted offering
// (type and zone, as spot) until the TTL (30 s in the lane) ends, then uses it
// again. The pool allows exactly one offering, so the replacement Pod stays
// Pending while the offering is unavailable.
func TestI03ReplacementAvoidsTheInterruptedOffering(t *testing.T) {
	o := takeSpot(t)
	interruptionPool(t, "i03", o)
	createDeployment(t, "i03", map[string]string{labelNodePool: "i03"}, "100m")
	node := waitPodRunningByLabel(t, "i03", long)
	first := waitClaims(t, "i03", 1, time.Minute)[0].Name

	annotated := time.Now()
	annotateNode(t, node, annotated.Add(60*time.Second).UTC().Truncate(time.Second))
	consistently(t, interruptedTTL/2, time.Second, "no replacement NodeClaim while the offering is unavailable", func() (bool, string) {
		cs, err := listClaims("i03")
		if err != nil {
			return false, ""
		}
		for _, c := range cs {
			if c.Name != first {
				return true, fmt.Sprintf("NodeClaim %s was created %s after the interruption", c.Name, time.Since(annotated).Round(time.Second))
			}
		}
		return false, ""
	})
	eventually(t, long, time.Second, "the replacement NodeClaim after the TTL", func() (bool, string) {
		cs, err := listClaims("i03")
		if err != nil {
			return false, err.Error()
		}
		for _, c := range cs {
			if c.Name != first && c.DeletionTimestamp.IsZero() {
				if c.Labels[labelType] != o.Type || c.Labels[labelZone] != o.Zone || c.Labels[labelCapacity] != o.Capacity {
					return false, fmt.Sprintf("NodeClaim %s is %v", c.Name, c.Labels)
				}
				return true, ""
			}
		}
		return false, "no replacement yet"
	})
	if after := time.Since(annotated); after < interruptedTTL-5*time.Second {
		t.Errorf("the offering was used again %s after the interruption, before its %s TTL", after.Round(time.Second), interruptedTTL)
	}
	waitPodRunningByLabel(t, "i03", long)
}

// waitPodRunningByLabel waits for a running Pod of the Deployment of the name
// (not the one that was deleted) and returns its node.
func waitPodRunningByLabel(t *testing.T, app string, timeout time.Duration) string {
	t.Helper()
	node := ""
	eventually(t, timeout, time.Second, "a pod of "+app+" Running", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx(), &pods, client.InNamespace("default"), client.MatchingLabels{"app": app}); err != nil {
			return false, err.Error()
		}
		for _, p := range pods.Items {
			if p.DeletionTimestamp.IsZero() && p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
				node = p.Spec.NodeName
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d pods, none running", len(pods.Items))
	})
	return node
}

// I04: the vendor's API shows the notice (the fake's interrupt key, found by the
// controller's poller): the same record as I01, with source vendor-api, and
// counted with that source.
func TestI04PollerNoticeIsTheSameRecord(t *testing.T) {
	node, claim, o := spotNode(t, "i04", 30)
	key := interruptedMetric(o, v1alpha1.ReasonSpotInterrupted, v1alpha1.SourceVendorAPI)
	before := metrics(t)[key]
	annotated := time.Now()
	setFake(t, map[string]string{"interrupt": fmt.Sprintf("- {nodeClaim: %s, deadlineSeconds: 60}\n", claim)})

	eventually(t, time.Minute, 500*time.Millisecond, "the poller annotated the Node", func() (bool, string) {
		n := &corev1.Node{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: node}, n); err != nil {
			return false, err.Error()
		}
		return n.Annotations[v1alpha1.AnnotationInterruptionSource] == v1alpha1.SourceVendorAPI, fmt.Sprint(n.Annotations)
	})
	t.Logf("fake announces the reclaim to the Node's annotation: %s", time.Since(annotated).Round(100*time.Millisecond))
	deadline := checkInterrupted(t, node, claim, v1alpha1.SourceVendorAPI)
	if lead := time.Until(deadline); lead < 30*time.Second || lead > 60*time.Second {
		t.Errorf("the recorded deadline is %s ahead, want about 60 s", lead.Round(time.Second))
	}
	waitPodTerminating(t, "i04", 30*time.Second)
	eventually(t, 30*time.Second, 500*time.Millisecond, "the interruption counted with source vendor-api", func() (bool, string) {
		got := metrics(t)[key]
		return got == before+1, fmt.Sprintf("%v, want %v", got, before+1)
	})
}

// I05: the instance is taken away without notice (the fake's reclaim key). Core's
// garbage collection deletes the NodeClaim, and SCarpenter records
// ReclaimedWithoutNotice: an Event on the NodeClaim and the metric with source gc.
func TestI05SilentReclaim(t *testing.T) {
	_, claim, o := spotNode(t, "i05", 0)
	key := interruptedMetric(o, v1alpha1.ReasonReclaimedWithoutNotice, v1alpha1.SourceGC)
	before := metrics(t)[key]
	setFake(t, map[string]string{"reclaim": fmt.Sprintf("- {nodeClaim: %s}\n", claim)})

	eventually(t, 5*time.Minute, time.Second, "the reclaim recorded", func() (bool, string) {
		evs := len(objEvents(t, "NodeClaim", claim, v1alpha1.ReasonReclaimedWithoutNotice))
		got := metrics(t)[key]
		return evs == 1 && got == before+1, fmt.Sprintf("%d Events, metric %v (want %v)", evs, got, before+1)
	})
	waitClaimGone(t, claim, 2*time.Minute)
	waitNoClaims(t, "i05", time.Minute)
	if got := metrics(t)[key]; got != before+1 {
		t.Errorf("%s = %v, want it counted once (%v)", key, got, before+1)
	}
	for _, i := range liveInstances(t) {
		if i.NodeClaim == claim {
			t.Errorf("the instance of %s is still %s", claim, i.State)
		}
	}
}

// I06: an on-demand Node that is annotated is ignored: one InterruptionIgnored
// Event, no condition, and the NodeClaim is not deleted.
func TestI06OnDemandNodeIsIgnored(t *testing.T) {
	createNodeClass(t, "i06", us1)
	types := takeTypes(t, []string{us1}, 2, 1)
	createNodePool(t, "i06", "i06", in(labelRegion, us1), in(labelCapacity, "on-demand"), in(labelType, apiNames(types)...))
	heldPod(t, "i06", "i06", 0)
	node := waitPodRunning(t, "i06", long)
	claim := waitClaims(t, "i06", 1, time.Minute)[0].Name
	annotateNode(t, node, time.Now().Add(60*time.Second).UTC().Truncate(time.Second))

	eventually(t, time.Minute, 500*time.Millisecond, "the InterruptionIgnored Event", func() (bool, string) {
		n := len(objEvents(t, "Node", node, "InterruptionIgnored"))
		return n > 0, fmt.Sprintf("%d Events", n)
	})
	consistently(t, 15*time.Second, time.Second, "the on-demand NodeClaim is left alone", func() (bool, string) {
		c := &karpv1.NodeClaim{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: claim}, c); err != nil {
			return true, err.Error()
		}
		if !c.DeletionTimestamp.IsZero() || c.StatusConditions().Get(v1alpha1.ConditionInterrupted) != nil {
			return true, fmt.Sprintf("deleted %v, conditions %+v", c.DeletionTimestamp, c.Status.Conditions)
		}
		return false, ""
	})
	for _, kind := range []struct{ kind, name string }{{"Node", node}, {"NodeClaim", claim}} {
		count := int32(0)
		for _, e := range objEvents(t, kind.kind, kind.name, "InterruptionIgnored") {
			count += max(e.Count, 1)
		}
		if count != 1 {
			t.Errorf("%d InterruptionIgnored Events on the %s, want 1 (%s)", count, kind.kind, strings.TrimSpace(kind.name))
		}
	}
}
