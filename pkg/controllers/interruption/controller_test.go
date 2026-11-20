//go:build scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package interruption_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/controllers/interruption"
	"github.com/SpareCores/scarpenter/pkg/unavailable"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/fake"
)

var envCfg *rest.Config

func TestMain(m *testing.M) {
	os.Exit(func() int {
		var stop func() error
		var err error
		envCfg, stop, err = testutil.StartEnvWith(testutil.EnvOptions{CRDs: testutil.KarpenterCRDs()})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer func() { _ = stop() }()
		return m.Run()
	}())
}

// seq makes the names unique across the tests, which share one API server.
var seq atomic.Int64

const (
	vendor       = "azure" // notice window 30 s .. 30 s
	instanceType = "azure.D2s_v5"
	zone         = "westeurope-1"
	margin       = 5 * time.Second
)

// recorder keeps the Events, suppressing a repeat of a dedupe key the way core's
// recorder does within its timeout.
type recorder struct {
	mu   sync.Mutex
	list []events.Event
	seen map[string]bool
}

func (r *recorder) Publish(evts ...events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	for _, e := range evts {
		if len(e.DedupeValues) > 0 {
			key := e.Reason + "/" + strings.Join(e.DedupeValues, "/")
			if r.seen[key] {
				continue
			}
			r.seen[key] = true
		}
		r.list = append(r.list, e)
	}
}

// count is the number of Events with the reason on the object of the kind.
func (r *recorder) count(reason, kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.list {
		if e.Reason != reason {
			continue
		}
		switch e.InvolvedObject.(type) {
		case *corev1.Node:
			if kind == "Node" {
				n++
			}
		case *karpv1.NodeClaim:
			if kind == "NodeClaim" {
				n++
			}
		}
	}
	return n
}

type harness struct {
	t    *testing.T
	c    client.Client
	clk  *clocktesting.FakeClock
	un   *unavailable.Cache
	reg  *prometheus.Registry
	rec  *recorder
	ctrl *interruption.Controller
	name string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if err := v1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(envCfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, c: c, clk: clocktesting.NewFakeClock(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)),
		rec: &recorder{}, name: fmt.Sprintf("intr-%d", seq.Add(1)),
	}
	h.un = unavailable.New(h.clk, unavailable.TTLs{})
	h.restart()
	return h
}

// restart builds a new controller over the same cluster, clock and cache: what a
// restart of the manager does, minus the lost cache.
func (h *harness) restart() {
	h.reg = prometheus.NewRegistry()
	h.ctrl = interruption.New(interruption.Options{
		Client:      h.c,
		Registry:    vendors.NewRegistryFrom(fake.New(fake.Options{CatalogVendor: vendor})),
		Unavailable: h.un, Clock: h.clk, Recorder: h.rec, Margin: margin, Registerer: h.reg,
	})
}

type nodeOpts struct {
	capacityType string // default spot
	annotations  map[string]string
	foreignClaim bool // the claim's provider ID is not the node's
	noNodeClass  bool
	termination  string // an existing termination timestamp
	claimAnn     map[string]string
	grace        *time.Duration // the NodeClaim's terminationGracePeriod
}

// add creates a NodeClaim (with a finalizer, as core's has, so that a delete
// leaves it terminating) and its Node.
func (h *harness) add(o nodeOpts) {
	h.t.Helper()
	ctx := h.t.Context()
	ct := o.capacityType
	if ct == "" {
		ct = karpv1.CapacityTypeSpot
	}
	pid := fmt.Sprintf("scarpenter://%s/westeurope/%s", vendor, h.name)
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: h.name, Finalizers: []string{"test/hold"}, Annotations: map[string]string{},
		Labels: map[string]string{
			karpv1.CapacityTypeLabelKey: ct, corev1.LabelInstanceTypeStable: instanceType, corev1.LabelTopologyZone: zone,
		},
	}}
	if o.termination != "" {
		claim.Annotations[karpv1.NodeClaimTerminationTimestampAnnotationKey] = o.termination
	}
	maps.Copy(claim.Annotations, o.claimAnn)
	if o.grace != nil {
		claim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: *o.grace}
	}
	claim.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: "class"}
	if o.noNodeClass {
		claim.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: "other.example.com", Kind: "Other", Name: "class"}
	}
	claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{}
	h.must(h.c.Create(ctx, claim))
	h.t.Cleanup(func() {
		got := &karpv1.NodeClaim{}
		if h.c.Get(context.Background(), client.ObjectKey{Name: h.name}, got) == nil {
			got.Finalizers = nil
			_ = h.c.Update(context.Background(), got)
			_ = h.c.Delete(context.Background(), got)
		}
	})
	claim.Status.ProviderID, claim.Status.NodeName = pid, h.name
	if o.foreignClaim {
		claim.Status.ProviderID = pid + "-other"
	}
	h.must(h.c.Status().Update(ctx, claim))
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: h.name, Annotations: o.annotations},
		Spec:       corev1.NodeSpec{ProviderID: pid},
	}
	h.must(h.c.Create(ctx, node))
	h.t.Cleanup(func() { _ = h.c.Delete(context.Background(), node) })
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) reconcile() {
	h.t.Helper()
	_, err := h.ctrl.Reconcile(h.t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: h.name}})
	h.must(err)
}

func (h *harness) claim() *karpv1.NodeClaim {
	h.t.Helper()
	got := &karpv1.NodeClaim{}
	h.must(h.c.Get(h.t.Context(), client.ObjectKey{Name: h.name}, got))
	return got
}

func (h *harness) node() *corev1.Node {
	h.t.Helper()
	got := &corev1.Node{}
	h.must(h.c.Get(h.t.Context(), client.ObjectKey{Name: h.name}, got))
	return got
}

// notice is the annotation set of a spot notice with the deadline d from now.
func (h *harness) notice(d time.Duration) map[string]string {
	return map[string]string{
		v1alpha1.AnnotationInterruption:         v1alpha1.ReasonSpotInterrupted,
		v1alpha1.AnnotationInterruptionDeadline: h.clk.Now().Add(d).Format(time.RFC3339),
		v1alpha1.AnnotationInterruptionSource:   v1alpha1.SourceInstanceMetadata,
	}
}

func (h *harness) offering() unavailable.Offering {
	return unavailable.Offering{
		Vendor: vendor, InstanceType: instanceType, Region: "westeurope", Zone: zone, CapacityType: karpv1.CapacityTypeSpot,
	}
}

// metric reads a counter, or a histogram's sample count, by name and labels.
func (h *harness) metric(name string, labels ...string) float64 {
	h.t.Helper()
	families, err := h.reg.Gather()
	h.must(err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			match := true
			for i := 0; i+1 < len(labels); i += 2 {
				match = match && got[labels[i]] == labels[i+1]
			}
			if !match {
				continue
			}
			if m.GetHistogram() != nil {
				return float64(m.GetHistogram().GetSampleCount())
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

func terminationOf(t *testing.T, claim *karpv1.NodeClaim) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, claim.Annotations[karpv1.NodeClaimTerminationTimestampAnnotationKey])
	if err != nil {
		t.Fatalf("termination timestamp: %v (annotations %v)", err, claim.Annotations)
	}
	return got
}

func TestSpotNoticeIsReactedTo(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{annotations: h.notice(30 * time.Second)})
	h.reconcile()

	claim := h.claim()
	if got, want := terminationOf(t, claim), h.clk.Now().Add(30*time.Second-margin); !got.Equal(want) {
		t.Errorf("termination timestamp = %s, want deadline - margin = %s", got, want)
	}
	for _, key := range []string{v1alpha1.AnnotationInterruption, v1alpha1.AnnotationInterruptionDeadline, v1alpha1.AnnotationInterruptionSource} {
		if claim.Annotations[key] != h.node().Annotations[key] || claim.Annotations[key] == "" {
			t.Errorf("annotation %s: claim %q, node %q", key, claim.Annotations[key], h.node().Annotations[key])
		}
	}
	for _, typ := range []string{v1alpha1.ConditionInterrupted, karpv1.ConditionTypeDisruptionReason} {
		cond := claim.StatusConditions().Get(typ)
		if !cond.IsTrue() || cond.Reason != v1alpha1.ReasonSpotInterrupted {
			t.Errorf("condition %s = %+v, want True with reason SpotInterrupted", typ, cond)
		}
	}
	if claim.DeletionTimestamp.IsZero() {
		t.Error("the NodeClaim was not deleted")
	}
	for _, kind := range []string{"Node", "NodeClaim"} {
		if n := h.rec.count(interruption.EventSpotInterrupted, kind); n != 1 {
			t.Errorf("%d SpotInterrupted Events on the %s, want 1", n, kind)
		}
	}
	if !h.un.IsUnavailable(h.offering()) {
		t.Error("the interrupted spot offering is still available")
	}
	od := h.offering()
	od.CapacityType = karpv1.CapacityTypeOnDemand
	if h.un.IsUnavailable(od) {
		t.Error("the on-demand offering of the same type and zone became unavailable")
	}
	if got := h.metric("scarpenter_interruptions_total", "vendor", vendor, "instance_type", instanceType, "zone", zone,
		"reason", v1alpha1.ReasonSpotInterrupted, "source", v1alpha1.SourceInstanceMetadata); got != 1 {
		t.Errorf("scarpenter_interruptions_total = %v, want 1", got)
	}
	if got := h.metric("scarpenter_interruption_lead_seconds", "vendor", vendor); got != 1 {
		t.Errorf("lead histogram has %v samples, want 1", got)
	}
	if got := h.metric("scarpenter_interruption_reaction_seconds"); got != 1 {
		t.Errorf("reaction histogram has %v samples, want 1", got)
	}
}

func TestInterruptedOfferingComesBackAfterTheTTL(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{annotations: h.notice(30 * time.Second)})
	h.reconcile()
	seq := h.un.Sequence()
	h.clk.Step(unavailable.DefaultInterruptedTTL - time.Second)
	if !h.un.IsUnavailable(h.offering()) {
		t.Fatal("the offering came back before the 10 minute TTL")
	}
	h.clk.Step(time.Second)
	if h.un.IsUnavailable(h.offering()) {
		t.Fatal("the offering is still unavailable after the TTL")
	}
	if h.un.Sequence() == seq {
		t.Error("the expiry did not change the cache sequence")
	}
}

func TestAnEarlierTerminationDeadlineIsKept(t *testing.T) {
	h := newHarness(t)
	early := h.clk.Now().Add(10 * time.Second).Format(time.RFC3339)
	h.add(nodeOpts{annotations: h.notice(30 * time.Second), termination: early})
	h.reconcile()
	if got := h.claim().Annotations[karpv1.NodeClaimTerminationTimestampAnnotationKey]; got != early {
		t.Errorf("termination timestamp = %s, want the earlier %s kept", got, early)
	}
	// A later one is tightened.
	h2 := newHarness(t)
	late := h2.clk.Now().Add(time.Hour).Format(time.RFC3339)
	h2.add(nodeOpts{annotations: h2.notice(30 * time.Second), termination: late})
	h2.reconcile()
	if got, want := terminationOf(t, h2.claim()), h2.clk.Now().Add(30*time.Second-margin); !got.Equal(want) {
		t.Errorf("termination timestamp = %s, want it tightened to %s", got, want)
	}
}

func TestTheTerminationGracePeriodStillBounds(t *testing.T) {
	h := newHarness(t)
	grace := 10 * time.Second
	h.add(nodeOpts{annotations: h.notice(30 * time.Second), grace: &grace})
	h.reconcile()
	if got, want := terminationOf(t, h.claim()), h.clk.Now().Add(grace); !got.Equal(want) {
		t.Errorf("termination timestamp = %s, want the NodeClaim's terminationGracePeriod from now, %s", got, want)
	}
}

func TestADeadlineOnAnUnmarkedClaimIsNotTrusted(t *testing.T) {
	h := newHarness(t)
	// For example from a NodePool template: the Node's notice decides.
	h.add(nodeOpts{
		annotations: h.notice(30 * time.Second),
		claimAnn:    map[string]string{v1alpha1.AnnotationInterruptionDeadline: h.clk.Now().Add(-time.Hour).Format(time.RFC3339)},
	})
	h.reconcile()
	if got, want := terminationOf(t, h.claim()), h.clk.Now().Add(30*time.Second-margin); !got.Equal(want) {
		t.Errorf("termination timestamp = %s, want %s from the Node's notice", got, want)
	}
}

func TestDeadlinesAreClampedToTheNoticeWindow(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		deadline string // the annotation; "" leaves it out
		want     time.Time
	}{
		"far in the future: at most the vendor's longest notice": {now.Add(24 * time.Hour).Format(time.RFC3339), now.Add(30 * time.Second)},
		"in the past: now":                         {now.Add(-time.Minute).Format(time.RFC3339), now},
		"unparsable: the vendor's shortest notice": {"soon", now.Add(30 * time.Second)},
		"missing: the vendor's shortest notice":    {"", now.Add(30 * time.Second)},
		"inside the window: as given":              {now.Add(30 * time.Second).Format(time.RFC3339), now.Add(30 * time.Second)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ann := h.notice(0)
			if tc.deadline == "" {
				delete(ann, v1alpha1.AnnotationInterruptionDeadline)
			} else {
				ann[v1alpha1.AnnotationInterruptionDeadline] = tc.deadline
			}
			h.add(nodeOpts{annotations: ann})
			h.reconcile()
			if got, want := terminationOf(t, h.claim()), tc.want.Add(-margin); !got.Equal(want) {
				t.Errorf("termination timestamp = %s, want %s", got, want)
			}
			// The effective deadline is what both objects record.
			if got := h.claim().Annotations[v1alpha1.AnnotationInterruptionDeadline]; got != tc.want.Format(time.RFC3339) {
				t.Errorf("claim deadline = %s, want %s", got, tc.want.Format(time.RFC3339))
			}
			if got := h.node().Annotations[v1alpha1.AnnotationInterruptionDeadline]; got != tc.want.Format(time.RFC3339) {
				t.Errorf("node deadline = %s, want %s", got, tc.want.Format(time.RFC3339))
			}
			if h.claim().DeletionTimestamp.IsZero() {
				t.Error("the NodeClaim was not deleted")
			}
		})
	}
}

func TestReconcilingAgainOrAfterARestartChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{annotations: h.notice(30 * time.Second)})
	h.reconcile()
	first := h.claim()
	seq := h.un.Sequence()

	h.clk.Step(5 * time.Second)
	h.reconcile()
	h.restart()
	h.reconcile()

	again := h.claim()
	for _, key := range []string{karpv1.NodeClaimTerminationTimestampAnnotationKey, v1alpha1.AnnotationInterruptionDeadline} {
		if first.Annotations[key] != again.Annotations[key] {
			t.Errorf("%s changed from %s to %s", key, first.Annotations[key], again.Annotations[key])
		}
	}
	if first.StatusConditions().Get(v1alpha1.ConditionInterrupted).LastTransitionTime !=
		again.StatusConditions().Get(v1alpha1.ConditionInterrupted).LastTransitionTime {
		t.Error("the Interrupted condition was set again")
	}
	if h.un.Sequence() != seq {
		t.Error("the cache was marked again (that would extend the TTL)")
	}
	for _, kind := range []string{"Node", "NodeClaim"} {
		if n := h.rec.count(interruption.EventSpotInterrupted, kind); n != 1 {
			t.Errorf("%d SpotInterrupted Events on the %s after repeats, want 1", n, kind)
		}
	}
	// The restarted controller has its own registry: it counted nothing.
	if got := h.metric("scarpenter_interruptions_total"); got != 0 {
		t.Errorf("a restarted controller counted the interruption again (%v)", got)
	}
}

func TestOnDemandNodeIsIgnoredOnce(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{capacityType: karpv1.CapacityTypeOnDemand, annotations: h.notice(30 * time.Second)})
	h.reconcile()
	h.reconcile()
	h.reconcile()
	claim := h.claim()
	if !claim.DeletionTimestamp.IsZero() {
		t.Error("an on-demand NodeClaim was deleted")
	}
	if _, ok := claim.Annotations[karpv1.NodeClaimTerminationTimestampAnnotationKey]; ok {
		t.Error("an on-demand NodeClaim got a termination deadline")
	}
	if claim.StatusConditions().Get(v1alpha1.ConditionInterrupted) != nil {
		t.Error("an on-demand NodeClaim was marked Interrupted")
	}
	if h.un.Len() != 0 {
		t.Error("an on-demand node marked an offering unavailable")
	}
	if n := h.rec.count(interruption.EventIgnored, "Node"); n != 1 {
		t.Errorf("%d InterruptionIgnored Events on the Node, want 1", n)
	}
	if n := h.rec.count(interruption.EventSpotInterrupted, "Node") + h.rec.count(interruption.EventSpotInterrupted, "NodeClaim"); n != 0 {
		t.Errorf("%d SpotInterrupted Events for an ignored notice", n)
	}
}

func TestUnknownReasonIsIgnored(t *testing.T) {
	h := newHarness(t)
	ann := h.notice(30 * time.Second)
	ann[v1alpha1.AnnotationInterruption] = "Whatever"
	h.add(nodeOpts{annotations: ann})
	h.reconcile()
	if !h.claim().DeletionTimestamp.IsZero() || h.un.Len() != 0 {
		t.Error("an unknown reason was acted on")
	}
	if n := h.rec.count(interruption.EventIgnored, "Node"); n != 1 {
		t.Errorf("%d InterruptionIgnored Events, want 1", n)
	}
}

func TestSourceIsLimitedToTheKnownOnes(t *testing.T) {
	h := newHarness(t)
	ann := h.notice(30 * time.Second)
	ann[v1alpha1.AnnotationInterruptionSource] = "made-up"
	h.add(nodeOpts{annotations: ann})
	h.reconcile()
	if got := h.claim().Annotations[v1alpha1.AnnotationInterruptionSource]; got != v1alpha1.SourceInstanceMetadata {
		t.Errorf("source = %q, want instance-metadata for an unknown one", got)
	}
	ann = h.notice(30 * time.Second)
	ann[v1alpha1.AnnotationInterruptionSource] = v1alpha1.SourceVendorAPI
	h2 := newHarness(t)
	h2.add(nodeOpts{annotations: ann})
	h2.reconcile()
	if got := h2.metric("scarpenter_interruptions_total", "source", v1alpha1.SourceVendorAPI); got != 1 {
		t.Errorf("vendor-api interruptions = %v, want 1", got)
	}
}

func TestNodesThatAreNotOursAreLeftAlone(t *testing.T) {
	for name, o := range map[string]nodeOpts{
		"a claim of another node class":     {noNodeClass: true},
		"a claim whose provider ID differs": {foreignClaim: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			o.annotations = h.notice(30 * time.Second)
			h.add(o)
			h.reconcile()
			claim := h.claim()
			if !claim.DeletionTimestamp.IsZero() || h.un.Len() != 0 || len(h.rec.list) != 0 {
				t.Error("acted on a node that is not SCarpenter's")
			}
		})
	}
	// A Node that does not exist, and one without a provider ID, are not errors.
	h := newHarness(t)
	h.reconcile()
	bare := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: h.name,
		Annotations: map[string]string{v1alpha1.AnnotationInterruption: v1alpha1.ReasonSpotInterrupted}}}
	h.must(h.c.Create(t.Context(), bare))
	t.Cleanup(func() { _ = h.c.Delete(context.Background(), bare) })
	h.reconcile()
}

func TestAClaimAlreadyDeletingIsStillTightenedAndMarked(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{annotations: h.notice(30 * time.Second)})
	h.must(h.c.Delete(t.Context(), h.claim())) // core's consolidation got there first
	h.reconcile()
	claim := h.claim()
	if got, want := terminationOf(t, claim), h.clk.Now().Add(30*time.Second-margin); !got.Equal(want) {
		t.Errorf("termination timestamp = %s, want %s", got, want)
	}
	if !claim.StatusConditions().Get(v1alpha1.ConditionInterrupted).IsTrue() {
		t.Error("not marked Interrupted")
	}
}

func TestNodeAndClaimGoneAreNoErrors(t *testing.T) {
	h := newHarness(t)
	h.add(nodeOpts{annotations: h.notice(30 * time.Second)})
	h.must(client.IgnoreNotFound(h.c.Delete(t.Context(), h.node())))
	h.reconcile()
	if _, err := h.ctrl.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "no-such-node"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Get(t.Context(), client.ObjectKey{Name: "no-such-node"}, &corev1.Node{}); !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}
