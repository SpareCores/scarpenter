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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
)

const (
	labelNodePool = karpv1.NodePoolLabelKey
	labelType     = corev1.LabelInstanceTypeStable
	labelRegion   = corev1.LabelTopologyRegion
	labelZone     = corev1.LabelTopologyZone
	labelCapacity = karpv1.CapacityTypeLabelKey
	labelOwner    = "scarpenter-e2e"
)

// createNodeClass creates an SCNodeClass for the aws vendor (served by the fake
// driver) restricted to the regions (none: every region) and removes it with
// the test. The dev status controller makes it Ready.
func createNodeClass(t *testing.T, name string, regions ...string) *v1alpha1.SCNodeClass {
	t.Helper()
	return createNodeClassWith(t, name, nil, regions...)
}

// createNodeClassWith is createNodeClass with a hook that edits the NodeClass
// before it is created (profiles, resources, pricing).
func createNodeClassWith(t *testing.T, name string, edit func(*v1alpha1.SCNodeClass), regions ...string) *v1alpha1.SCNodeClass {
	t.Helper()
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: "4757.2.3"},
			Kubernetes: v1alpha1.KubernetesSpec{Version: "v1.37.1"},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Vendors:    v1alpha1.VendorBlocks{AWS: &awsapi.Settings{Regions: regions}},
		},
	}
	if edit != nil {
		edit(nc)
	}
	if err := k8s.Create(ctx(), nc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k8s.Delete(ctx(), nc); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting NodeClass %s: %v", name, err)
		}
	})
	waitNodeClassReady(t, name)
	return nc
}

// waitNodeClassReady waits for the NodeClass to be Ready.
func waitNodeClassReady(t *testing.T, name string) {
	t.Helper()
	nc := &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	eventually(t, 5*time.Minute, time.Second, "NodeClass "+name+" Ready", func() (bool, string) {
		got := &v1alpha1.SCNodeClass{}
		if err := k8s.Get(ctx(), client.ObjectKeyFromObject(nc), got); err != nil {
			return false, err.Error()
		}
		for _, c := range got.Status.Conditions {
			if c.Type == "Ready" && c.Status == metav1.ConditionTrue {
				return true, ""
			}
		}
		return false, fmt.Sprintf("conditions %+v", got.Status.Conditions)
	})
}

// req builds a NodePool requirement.
func req(key string, op corev1.NodeSelectorOperator, values ...string) karpv1.NodeSelectorRequirementWithMinValues {
	return karpv1.NodeSelectorRequirementWithMinValues{Key: key, Operator: op, Values: values}
}

func in(key string, values ...string) karpv1.NodeSelectorRequirementWithMinValues {
	return req(key, corev1.NodeSelectorOpIn, values...)
}

// createNodePool creates a NodePool on the NodeClass that consolidates empty
// nodes at once. On cleanup it deletes the NodePool and waits for its NodeClaims
// (and so its instances) to be gone.
func createNodePool(t *testing.T, name, class string, reqs ...karpv1.NodeSelectorRequirementWithMinValues) *karpv1.NodePool {
	t.Helper()
	return createNodePoolWith(t, name, class, nil, reqs...)
}

// createNodePoolWith is createNodePool with taints on the nodes of the pool.
func createNodePoolWith(t *testing.T, name, class string, taints []corev1.Taint, reqs ...karpv1.NodeSelectorRequirementWithMinValues) *karpv1.NodePool {
	t.Helper()
	all := append([]karpv1.NodeSelectorRequirementWithMinValues{in(v1alpha1.LabelVendor, vendor)}, reqs...)
	np := &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
				NodeClassRef: &karpv1.NodeClassReference{Group: v1alpha1.Group, Kind: "SCNodeClass", Name: class},
				Requirements: all,
				Taints:       taints,
			}},
			Disruption: karpv1.Disruption{
				ConsolidationPolicy: karpv1.ConsolidationPolicyWhenEmpty,
				ConsolidateAfter:    karpv1.MustParseNillableDuration("0s"),
			},
			Limits: karpv1.Limits{corev1.ResourceCPU: resource.MustParse("100000")},
		},
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
		if got.StatusConditions().IsTrue(karpv1.ConditionTypeNodeClassReady) &&
			got.StatusConditions().IsTrue(karpv1.ConditionTypeValidationSucceeded) {
			return true, ""
		}
		return false, fmt.Sprintf("conditions %+v", got.Status.Conditions)
	})
	return np
}

// podSpec is the pod the scenarios create: a pause container asking for a
// little cpu, pinned by node selector.
func podSpec(selector map[string]string, cpu string) corev1.PodSpec {
	return corev1.PodSpec{
		TerminationGracePeriodSeconds: ptr.To[int64](0),
		NodeSelector:                  selector,
		Containers: []corev1.Container{{
			Name: "pause", Image: "registry.k8s.io/pause:3.10",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
		}},
	}
}

// ownerLabel is the test's name as a label value.
func ownerLabel(t *testing.T) string {
	v := strings.Map(func(r rune) rune {
		if r == '/' || r == ',' {
			return '_'
		}
		return r
	}, t.Name())
	if len(v) > 63 {
		v = v[:63]
	}
	return strings.Trim(v, "_.-")
}

// createPod creates a pod that is deleted with the test.
func createPod(t *testing.T, name string, selector map[string]string, cpu string) *corev1.Pod {
	t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{labelOwner: ownerLabel(t)}},
		Spec:       podSpec(selector, cpu),
	}
	if err := k8s.Create(ctx(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k8s.Delete(ctx(), p, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting pod %s: %v", name, err)
		}
	})
	return p
}

// createDeployment creates a one-replica Deployment of the pod, deleted with the test.
func createDeployment(t *testing.T, name string, selector map[string]string, cpu string) *appsv1.Deployment {
	t.Helper()
	lbl := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}, Spec: podSpec(selector, cpu)},
		},
	}
	if err := k8s.Create(ctx(), d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k8s.Delete(ctx(), d, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil &&
			!apierrors.IsNotFound(err) {
			t.Logf("deleting deployment %s: %v", name, err)
		}
	})
	return d
}

func podNode(name string) (string, error) {
	p := &corev1.Pod{}
	if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: name}, p); err != nil {
		return "", err
	}
	return p.Spec.NodeName, nil
}

func listClaims(pool string) ([]karpv1.NodeClaim, error) {
	var l karpv1.NodeClaimList
	if err := k8s.List(ctx(), &l, client.MatchingLabels{labelNodePool: pool}); err != nil {
		return nil, err
	}
	return l.Items, nil
}

func waitNoClaims(t *testing.T, pool string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cs, err := listClaims(pool); err == nil && len(cs) == 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("NodeClaims of %s still exist after %s", pool, timeout)
}

// waitClaims waits for at least n NodeClaims of the pool that are not being
// deleted and returns them.
func waitClaims(t *testing.T, pool string, n int, timeout time.Duration) []karpv1.NodeClaim {
	t.Helper()
	var got []karpv1.NodeClaim
	eventually(t, timeout, time.Second, fmt.Sprintf("%d NodeClaims of %s", n, pool), func() (bool, string) {
		cs, err := listClaims(pool)
		if err != nil {
			return false, err.Error()
		}
		got = got[:0]
		for _, c := range cs {
			if c.DeletionTimestamp.IsZero() {
				got = append(got, c)
			}
		}
		return len(got) >= n, fmt.Sprintf("%d NodeClaims", len(got))
	})
	return got
}

// nodeReady reports whether the node exists and is Ready.
func nodeReady(name string) (bool, string) {
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: name}, n); err != nil {
		return false, err.Error()
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue, string(c.Status)
		}
	}
	return false, "no Ready condition"
}

// waitPodRunning waits for the pod to run and returns its node's name.
func waitPodRunning(t *testing.T, name string, timeout time.Duration) string {
	t.Helper()
	node := ""
	eventually(t, timeout, time.Second, "pod "+name+" Running", func() (bool, string) {
		p := &corev1.Pod{}
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: name}, p); err != nil {
			return false, err.Error() + podEvictions(name)
		}
		node = p.Spec.NodeName
		return p.Status.Phase == corev1.PodRunning, string(p.Status.Phase)
	})
	return node
}

// podEvictions describes the eviction Events of a pod, for the message of a wait
// that failed because the pod disappeared.
func podEvictions(name string) string {
	evs, err := clientset.CoreV1().Events("default").List(ctx(), metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name})
	if err != nil {
		return ""
	}
	out := ""
	for _, e := range evs.Items {
		if strings.Contains(e.Reason, "Eviction") || e.Reason == "Killing" || e.Reason == "Evicted" {
			out += fmt.Sprintf(" [event %s: %s]", e.Reason, e.Message)
		}
	}
	return out
}

// vendorCondition returns a condition of status.vendors.aws of the NodeClass.
func vendorCondition(class, typ string) (cond metav1.Condition, ok bool, quota []v1alpha1.QuotaStatus) {
	nc := &v1alpha1.SCNodeClass{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: class}, nc); err != nil {
		return cond, false, nil
	}
	vs := nc.Status.Vendors[vendor]
	for _, c := range vs.Conditions {
		if c.Type == typ {
			return metav1.Condition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message}, true, vs.Quota
		}
	}
	return cond, false, vs.Quota
}

// --------------------------------------------------------- reference data ----

// offering is an offering of the reference catalog: what the controller's
// catalog holds for the same snapshot.
type offering struct {
	Type         string // catalog name, aws.m7i.large
	API          string // m7i.large
	Region, Zone string
	Capacity     string
	Price        float64
	VCPUs        int64
	// AllocMem is the memory pods can use (capacity minus overhead).
	AllocMem int64
}

var (
	refMu    sync.Mutex
	refCache = map[string][]offering{}
)

// reference returns every offering of the snapshot in the regions (none: all),
// computed in process by the same catalog code the controller runs.
func reference(t *testing.T, regions ...string) []offering {
	t.Helper()
	refMu.Lock()
	defer refMu.Unlock()
	key := strings.Join(regions, ",")
	if o, ok := refCache[key]; ok {
		return o
	}
	cl := catalog.NewClass("ref", catalog.Scope{vendor: regions})
	cat := catalog.New(catalog.Options{Source: catalogtest.SourceAt(replay.URL), Facts: catalogtest.Facts(t, vendor)})
	cat.SetClasses([]catalog.Class{cl})
	if err := cat.Refresh(ctx()); err != nil {
		t.Fatal(err)
	}
	its, err := cat.InstanceTypes(ctx(), catalog.Query{Class: "ref"})
	if err != nil {
		t.Fatal(err)
	}
	var out []offering
	for _, it := range its {
		cpu := it.Capacity.Cpu().Value()
		alloc := it.Allocatable()
		mem := alloc.Memory().Value()
		for _, o := range it.Offerings {
			out = append(out, offering{
				Type: it.Name, API: strings.TrimPrefix(it.Name, vendor+"."),
				Region: o.Requirements.Get(labelRegion).Any(), Zone: o.Requirements.Get(labelZone).Any(),
				Capacity: o.Requirements.Get(labelCapacity).Any(), Price: o.Price, VCPUs: cpu, AllocMem: mem,
			})
		}
	}
	refCache[key] = out
	return out
}

func filterOfferings(os []offering, keep func(offering) bool) []offering {
	var out []offering
	for _, o := range os {
		if keep(o) {
			out = append(out, o)
		}
	}
	return out
}

// typeWithUniqueCheapestRegion returns a type with the given vCPUs that has a unique cheapest
// on-demand region among the regions: its cheapest region is strictly cheaper
// than every other, so "the cheapest region" is well defined. Types in
// `avoid` are skipped (they are reserved by other scenarios).
func typeWithUniqueCheapestRegion(t *testing.T, regions []string, vcpus int64, avoid ...string) (typ string, cheap, other string) {
	t.Helper()
	offs := filterOfferings(reference(t, regions...), func(o offering) bool {
		return o.Capacity == "on-demand" && o.VCPUs == vcpus && !slices.Contains(avoid, o.API) && o.AllocMem >= minAllocMem
	})
	byType := map[string]map[string]float64{}
	for _, o := range offs {
		if byType[o.API] == nil {
			byType[o.API] = map[string]float64{}
		}
		if p, ok := byType[o.API][o.Region]; !ok || o.Price < p {
			byType[o.API][o.Region] = o.Price
		}
	}
	for _, name := range slices.Sorted(mapsKeys(byType)) {
		prices := byType[name]
		if len(prices) != len(regions) {
			continue
		}
		var lo, hi string
		for r, p := range prices {
			if lo == "" || p < prices[lo] {
				lo = r
			}
			_ = hi
		}
		strict := true
		for r, p := range prices {
			if r != lo && p <= prices[lo] {
				strict = false
			}
		}
		if strict {
			for r := range prices {
				if r != lo {
					return name, lo, r
				}
			}
		}
	}
	t.Skipf("no %d-vCPU type with a strictly cheapest region among %v in the snapshot", vcpus, regions)
	return "", "", ""
}

func mapsKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
