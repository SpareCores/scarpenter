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
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	awsapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/aws"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	qemuapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
	"github.com/SpareCores/scarpenter/pkg/catalog"
	"github.com/SpareCores/scarpenter/pkg/catalog/catalogtest"
	"github.com/SpareCores/scarpenter/pkg/catalog/labels"
)

// The scenarios of plan 06: node options and vendor resources, each with an
// assertion on the fake driver's state (its instances, resources and calls).

const (
	labelDedicated = v1alpha1.Group + "/dedicated"
	// describedClass is applied by run.sh before the controller starts: the
	// metric keys of a NodeClass are registered with core at start-up (00a
	// section 7.2).
	describedClass = "l3-described"
)

func gi(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

// podWith creates a pod that is deleted with the test; edit changes its spec.
func podWith(t *testing.T, name string, selector map[string]string, edit func(*corev1.PodSpec)) {
	t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{labelOwner: ownerLabel(t)}},
		Spec:       podSpec(selector, "100m"),
	}
	if edit != nil {
		edit(&p.Spec)
	}
	if err := k8s.Create(ctx(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k8s.Delete(ctx(), p, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting pod %s: %v", name, err)
		}
	})
}

// podPhase returns the pod's phase and node.
func podPhase(name string) (corev1.PodPhase, string) {
	p := &corev1.Pod{}
	if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: name}, p); err != nil {
		return "", ""
	}
	return p.Status.Phase, p.Spec.NodeName
}

// instanceOfNode returns the live fake instance behind the node (the node and
// the NodeClaim have the same name).
func instanceOfNode(t *testing.T, node string) instance {
	t.Helper()
	for _, i := range liveInstances(t) {
		if i.NodeClaim == node {
			return i
		}
	}
	t.Fatalf("the fake has no live instance of %s", node)
	return instance{}
}

func claimOf(t *testing.T, name string) karpv1.NodeClaim {
	t.Helper()
	c := karpv1.NodeClaim{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: name}, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func parsePrice(t *testing.T, c karpv1.NodeClaim) float64 {
	t.Helper()
	p, err := strconv.ParseFloat(c.Annotations[v1alpha1.AnnotationPrice], 64)
	if err != nil {
		t.Fatalf("NodeClaim %s has no price annotation: %v", c.Name, c.Annotations)
	}
	return p
}

// S12: exact placement. A Job with vendor, type, region and capacity type gets
// exactly that offering; hostname anti-affinity and a taint give one pod per
// node.
func TestS12ExactPlacement(t *testing.T) {
	createNodeClass(t, "s12", us1)
	taint := corev1.Taint{Key: labelDedicated, Value: "bench", Effect: corev1.TaintEffectNoSchedule}
	pool := createNodePoolWith(t, "s12", "s12", []corev1.Taint{taint}, in(labelRegion, us1), in(labelCapacity, "on-demand"))
	// KWOK completes the pods of a Job at once; the nodes stay for the assertions.
	eventually(t, time.Minute, time.Second, "the NodePool keeps empty nodes", func() (bool, string) {
		got := &karpv1.NodePool{}
		if err := k8s.Get(ctx(), client.ObjectKeyFromObject(pool), got); err != nil {
			return false, err.Error()
		}
		got.Spec.Disruption.ConsolidateAfter = karpv1.MustParseNillableDuration("10m")
		if err := k8s.Update(ctx(), got); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	typ := takeTypes(t, []string{us1}, 2, 1)[0]
	mark := time.Now()

	lbl := map[string]string{"app": "s12-bench"}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "s12", Namespace: "default"},
		Spec: batchv1.JobSpec{
			Parallelism: ptr.To[int32](2), Completions: ptr.To[int32](2), BackoffLimit: ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}, Spec: podSpec(map[string]string{
				v1alpha1.LabelVendor: vendor, labelType: vendor + "." + typ, labelRegion: us1, labelCapacity: "on-demand",
				labelNodePool: "s12",
			}, "100m")},
		},
	}
	job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	job.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Key: labelDedicated, Operator: corev1.TolerationOpExists}}
	job.Spec.Template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey: corev1.LabelHostname, LabelSelector: &metav1.LabelSelector{MatchLabels: lbl},
		}},
	}}
	if err := k8s.Create(ctx(), job); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx(), job, client.PropagationPolicy(metav1.DeletePropagationForeground))
	})

	// Hostname anti-affinity makes Karpenter create one NodeClaim per pod. (KWOK
	// completes the pods of a Job at once, and the kube-scheduler ignores
	// completed pods, so where the pods end up is not what is asserted.)
	var nodes []string
	eventually(t, long, 2*time.Second, "two NodeClaims, each with its own registered node", func() (bool, string) {
		claims, err := listClaims("s12")
		if err != nil {
			return false, err.Error()
		}
		nodes = nodes[:0]
		for _, c := range claims {
			if c.DeletionTimestamp.IsZero() && c.Status.NodeName != "" {
				nodes = append(nodes, c.Status.NodeName)
			}
		}
		return len(nodes) == 2, fmt.Sprintf("%d NodeClaims with a node", len(nodes))
	})
	if nodes[0] == nodes[1] {
		t.Fatalf("both NodeClaims have the node %s", nodes[0])
	}
	eventually(t, long, 2*time.Second, "both pods of the Job placed on the nodes", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx(), &pods, client.InNamespace("default"), client.MatchingLabels(lbl)); err != nil {
			return false, err.Error()
		}
		placed := 0
		for _, p := range pods.Items {
			if slices.Contains(nodes, p.Spec.NodeName) {
				placed++
			}
		}
		return placed == 2, fmt.Sprintf("%d of %d pods placed", placed, len(pods.Items))
	})
	for _, node := range nodes {
		l := nodeLabels(t, node)
		for k, v := range map[string]string{
			labelType: vendor + "." + typ, labelRegion: us1, labelCapacity: "on-demand", v1alpha1.LabelVendor: vendor,
		} {
			if l[k] != v {
				t.Errorf("node %s: label %s = %q, want %q", node, k, l[k], v)
			}
		}
		n := &corev1.Node{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: node}, n); err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(n.Spec.Taints, func(x corev1.Taint) bool { return x.Key == labelDedicated }) {
			t.Errorf("node %s lacks the NodePool's taint: %v", node, n.Spec.Taints)
		}
		// The fake driver launched exactly the requested offering.
		inst := instanceOfNode(t, node)
		if inst.InstanceType != typ || inst.Region != us1 || inst.CapacityType != "on-demand" {
			t.Errorf("instance %+v, want %s in %s on-demand", inst, typ, us1)
		}
	}
	creates := callsSince(t, mark, "create")
	if len(creates) != 2 {
		t.Fatalf("%d launch calls for two pods: %+v", len(creates), creates)
	}
	for _, c := range creates {
		if c.Result != "ok" || c.InstanceType != typ || c.Region != us1 || c.CapacityType != "on-demand" {
			t.Errorf("launch call %+v, want exactly %s in %s on-demand", c, typ, us1)
		}
	}
}

// eligible returns the offerings of the types that satisfy the requirements,
// as Create sees them: core's type intersection and offering compatibility.
func eligible(its []*cloudprovider.InstanceType, reqs scheduling.Requirements, minMem int64) []offering {
	var out []offering
	for _, it := range its {
		alloc := it.Allocatable()
		if it.Requirements.Intersects(reqs) != nil || alloc.Memory().Value() < minMem {
			continue
		}
		for _, o := range it.Offerings {
			if !o.Available || !reqs.IsCompatible(o.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
				continue
			}
			out = append(out, offering{
				Type: it.Name, API: strings.TrimPrefix(it.Name, vendor+"."), Region: o.Requirements.Get(labelRegion).Any(),
				Zone: o.Zone(), Capacity: o.CapacityType(), Price: o.Price,
			})
		}
	}
	return byPrice(out)
}

func req1(key string, op corev1.NodeSelectorOperator, values ...string) *scheduling.Requirement {
	return scheduling.NewRequirement(key, op, values...)
}

// S13: described, not named. The pod gives a continent, a vCPU range and a
// benchmark percentile (and, in a second pod, a selection profile); the cheapest
// offering that matches is launched. It confirms with real Karpenter core that
// Gt and Lt are matched against the numeric requirements of types and offerings,
// and that a type without the label (DoesNotExist) is excluded for pods that
// require the key (overview section 15 Q11).
func TestS13DescribedNotNamed(t *testing.T) {
	waitNodeClassReady(t, describedClass)
	// Later scenarios (the scale numbers of S10) run without it: its metric
	// labels make the catalog about 1.7 times as large as a plain one.
	t.Cleanup(func() {
		got := &v1alpha1.SCNodeClass{ObjectMeta: metav1.ObjectMeta{Name: describedClass}}
		if err := k8s.Delete(ctx(), got); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting NodeClass %s: %v", describedClass, err)
		}
	})
	nc := &v1alpha1.SCNodeClass{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: describedClass}, nc); err != nil {
		t.Fatal(err)
	}
	// The same class, read from the cluster, in process: what the controller's
	// catalog holds.
	class, err := catalog.ClassFromNodeClass(nc)
	if err != nil {
		t.Fatal(err)
	}
	if missing := labels.MissingKeys(class.MetricNames()); len(missing) > 0 {
		labels.RegisterKeys(missing...) // the test process, not the controller
	}
	cat := catalog.New(catalog.Options{Source: catalogtest.SourceAt(replay.URL), Facts: catalogtest.Facts(t, vendor)})
	cat.SetClasses([]catalog.Class{class})
	if err := cat.Refresh(ctx()); err != nil {
		t.Fatal(err)
	}
	its, err := cat.InstanceTypes(ctx(), catalog.Query{Class: class.Name})
	if err != nil {
		t.Fatal(err)
	}
	const pctLabel = v1alpha1.LabelMetricPrefix + "single-core-pct"
	const minMem = 256 << 20

	createNodePool(t, "s13", describedClass, in(labelCapacity, "on-demand"))
	mark := time.Now()

	// Pod A: Europe, 2 to 16 vCPUs, the top decile of single-core performance.
	want := reqs(
		req1(v1alpha1.LabelContinent, corev1.NodeSelectorOpIn, "europe"),
		req1(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "1"), req1(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpLt, "17"),
		req1(pctLabel, corev1.NodeSelectorOpGt, "89"),
		req1(labelCapacity, corev1.NodeSelectorOpIn, "on-demand"), req1(v1alpha1.LabelVendor, corev1.NodeSelectorOpIn, vendor))
	candidates := eligible(its, want, minMem)
	if len(candidates) == 0 {
		t.Fatal("no offering satisfies the description in the reference catalog")
	}
	// Without the benchmark condition a cheaper type exists that has no score at
	// all: it must be excluded (DoesNotExist), not matched.
	unbenchmarked := reqs(
		req1(v1alpha1.LabelContinent, corev1.NodeSelectorOpIn, "europe"),
		req1(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpGt, "1"), req1(v1alpha1.LabelVCPUs, corev1.NodeSelectorOpLt, "17"),
		req1(v1alpha1.MetricLabel("single-core"), corev1.NodeSelectorOpDoesNotExist),
		req1(labelCapacity, corev1.NodeSelectorOpIn, "on-demand"))
	cheaperUnscored := false
	for _, o := range eligible(its, unbenchmarked, minMem) {
		if o.Price < candidates[0].Price {
			cheaperUnscored = true
		}
	}
	t.Logf("the description matches %d offerings, the cheapest %+v; a cheaper type without a score exists: %v",
		len(candidates), candidates[0], cheaperUnscored)

	podWith(t, "s13-a", map[string]string{labelNodePool: "s13", labelCapacity: "on-demand"}, func(s *corev1.PodSpec) {
		s.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
		s.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: v1alpha1.LabelContinent, Operator: corev1.NodeSelectorOpIn, Values: []string{"europe"}},
					{Key: v1alpha1.LabelVCPUs, Operator: corev1.NodeSelectorOpGt, Values: []string{"1"}},
					{Key: v1alpha1.LabelVCPUs, Operator: corev1.NodeSelectorOpLt, Values: []string{"17"}},
					{Key: pctLabel, Operator: corev1.NodeSelectorOpGt, Values: []string{"89"}},
				},
			}}},
		}}
	})
	node := waitPodRunning(t, "s13-a", long)
	inst := instanceOfNode(t, node)
	l := nodeLabels(t, node)
	if inst.InstanceType != candidates[0].API || inst.Region != candidates[0].Region || inst.Zone != candidates[0].Zone {
		t.Errorf("launched %s in %s/%s, the cheapest offering of the description is %+v", inst.InstanceType, inst.Region, inst.Zone, candidates[0])
	}
	if pct, err := strconv.Atoi(l[pctLabel]); err != nil || pct <= 89 {
		t.Errorf("node label %s = %q: the percentile condition was not honoured", pctLabel, l[pctLabel])
	}
	if vcpus, _ := strconv.Atoi(l[v1alpha1.LabelVCPUs]); vcpus < 2 || vcpus > 16 || l[v1alpha1.LabelContinent] != "europe" {
		t.Errorf("node labels vcpus=%q continent=%q", l[v1alpha1.LabelVCPUs], l[v1alpha1.LabelContinent])
	}
	if cheaperUnscored && l[v1alpha1.MetricLabel("single-core")] == "" {
		t.Errorf("the node of an unscored type was chosen")
	}

	// Pod B: only members of the selection profile.
	member := reqs(req1(v1alpha1.LabelSelection, corev1.NodeSelectorOpIn, "fast-single-core"),
		req1(labelCapacity, corev1.NodeSelectorOpIn, "on-demand"), req1(v1alpha1.LabelVendor, corev1.NodeSelectorOpIn, vendor))
	// A NodeClaim that pins one selection profile with a non-Price objective
	// orders its candidates by that objective (00a section 7.3): the catalog
	// prices the offerings by it for such a query.
	pinned, err := cat.InstanceTypes(ctx(), catalog.Query{Class: class.Name, Filter: catalog.NewFilter(member)})
	if err != nil {
		t.Fatal(err)
	}
	members := eligible(pinned, member, minMem)
	if len(members) == 0 {
		t.Fatal("the selection profile has no member offering in the reference catalog")
	}
	all := eligible(its, reqs(req1(labelCapacity, corev1.NodeSelectorOpIn, "on-demand")), minMem)
	podWith(t, "s13-b", map[string]string{
		labelNodePool: "s13", labelCapacity: "on-demand", v1alpha1.LabelSelection: "fast-single-core",
	}, func(s *corev1.PodSpec) {
		s.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
	})
	nodeB := waitPodRunning(t, "s13-b", long)
	instB := instanceOfNode(t, nodeB)
	if instB.InstanceType != members[0].API || instB.Region != members[0].Region || instB.Zone != members[0].Zone {
		t.Errorf("selection pod launched %s in %s/%s, the best member by the profile's objective is %+v (the cheapest offering overall is %+v)",
			instB.InstanceType, instB.Region, instB.Zone, members[0], all[0])
	}
	if got := nodeLabels(t, nodeB)[v1alpha1.LabelSelection]; got != "fast-single-core" {
		t.Errorf("node label selection = %q", got)
	}
	if creates := callsSince(t, mark, "create"); len(creates) != 2 {
		t.Errorf("%d launch calls for two pods: %+v", len(creates), creates)
	}
}

func reqs(rs ...*scheduling.Requirement) scheduling.Requirements {
	return scheduling.NewRequirements(rs...)
}

// S14: storage profiles. A profile with a block for the vendor is applied (the
// resolved settings are in the fake instance, the profile on the node); a
// profile without a block for the vendor leaves the pod Pending; a type that
// cannot host a profile is never chosen, even when it is the cheapest.
func TestS14StorageProfile(t *testing.T) {
	types := takeTypes(t, []string{us1}, 2, 2)
	cheap, next := types[0], types[1]
	// Written before the NodeClass: the catalog sees the rule from its first
	// build.
	setFake(t, map[string]string{"unsupported": fmt.Sprintf("- {kind: storage, profile: bench-vol, instanceTypes: [%s.%s]}\n", vendor, cheap)})
	createNodeClassWith(t, "s14", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{
			"bench-vol": {AWS: &awsapi.Storage{
				Root: &awsapi.RootVolume{VolumeSize: gi("128Gi")},
				Data: []awsapi.DataVolume{{
					GuestVolume: common.GuestVolume{Name: "bench", MountPath: "/var/lib/bench", Filesystem: common.FilesystemXFS},
					VolumeType:  awsapi.VolumeTypeGP3, VolumeSize: *gi("100Gi"),
				}},
			}},
			// A profile for another vendor only: it does not exist on aws.
			"qemu-only": {QEMU: &qemuapi.Storage{Root: &qemuapi.RootDisk{Size: gi("10Gi")}}},
		}
	}, us1)
	both := in(labelType, apiNames([]string{cheap, next})...)
	createNodePool(t, "s14", "s14", in(labelRegion, us1), in(labelCapacity, "on-demand"), both)
	createNodePool(t, "s14-none", "s14", in(labelRegion, us1), in(labelCapacity, "on-demand"), both)
	createNodePool(t, "s14-unsupported", "s14", in(labelRegion, us1), in(labelCapacity, "on-demand"), both)
	mark := time.Now()
	profileLabel := v1alpha1.LabelStorageProfile

	// With the profile: only the type that can host it, although the other is cheaper.
	podWith(t, "s14-db", map[string]string{labelNodePool: "s14", profileLabel: "bench-vol"}, nil)
	node := waitPodRunning(t, "s14-db", long)
	assertProfileApplied(t, node, cheap, next)

	// Without a profile the cheapest type is used and no volume is attached.
	podWith(t, "s14-plain", map[string]string{labelNodePool: "s14-none"}, nil)
	nodeP := waitPodRunning(t, "s14-plain", long)
	instP := instanceOfNode(t, nodeP)
	if instP.InstanceType != cheap || len(instP.Devices) != 0 || strings.Contains(instP.Settings, "bench") {
		t.Errorf("default route: %+v, want %s without volumes", instP, cheap)
	}
	if nodeLabels(t, nodeP)[profileLabel] != "default" {
		t.Errorf("profile label on the default route: %v", nodeLabels(t, nodeP))
	}

	// A profile that has no block for the vendor does not exist on it.
	podWith(t, "s14-qemu", map[string]string{labelNodePool: "s14-none", profileLabel: "qemu-only"}, nil)
	// The unsupported type pinned with the profile: nothing can host it.
	podWith(t, "s14-unsupported", map[string]string{
		labelNodePool: "s14-unsupported", profileLabel: "bench-vol", labelType: vendor + "." + cheap,
	}, nil)
	consistently(t, 25*time.Second, 2*time.Second, "the pods without a feasible offering stay Pending", func() (bool, string) {
		for _, pod := range []string{"s14-qemu", "s14-unsupported"} {
			if phase, node := podPhase(pod); node != "" || phase == corev1.PodRunning {
				return true, fmt.Sprintf("%s got the node %q", pod, node)
			}
		}
		return false, ""
	})
	if cs, _ := listClaims("s14-unsupported"); len(cs) != 0 {
		t.Errorf("NodeClaims for the unsupported type: %d", len(cs))
	}
	for _, c := range callsSince(t, mark, "create") {
		if c.InstanceType == cheap && c.NodeClaim != nodeP {
			t.Errorf("the type that cannot host the profile was launched: %+v", c)
		}
	}
}

// assertProfileApplied checks that the node was launched with the bench-vol
// profile: on the type that can host it, with the settings, device hints,
// volume, labels and price of the profile.
func assertProfileApplied(t *testing.T, node, cheap, next string) {
	t.Helper()
	profileLabel := v1alpha1.LabelStorageProfile
	inst := instanceOfNode(t, node)
	if inst.InstanceType != next {
		t.Errorf("launched %s, want %s: %s cannot host the profile and must never be chosen", inst.InstanceType, next, cheap)
	}
	l := nodeLabels(t, node)
	if l[profileLabel] != "bench-vol" || l[v1alpha1.LabelNetworkProfile] != "default" || l[v1alpha1.LabelPlacementProfile] != "default" {
		t.Errorf("profile labels on the node: %v", l)
	}
	if inst.Profiles["storage"] != "bench-vol" || !strings.Contains(inst.Settings, `"bench"`) || !strings.Contains(inst.Settings, "128Gi") {
		t.Errorf("the fake got the profiles %v and the settings %s", inst.Profiles, inst.Settings)
	}
	if len(inst.Devices) != 1 || !strings.Contains(inst.Devices[0], "bench=/dev/disk/by-id/fake-") || !strings.Contains(inst.Devices[0], "xfs /var/lib/bench") {
		t.Errorf("device hints %v", inst.Devices)
	}
	awaitState(t)
	volumes := 0
	for _, r := range readResources(t) {
		if r.Kind == "volumes" && r.Managed && slices.Contains(inst.Resources, r.ID) {
			volumes++
		}
	}
	if volumes != 1 || len(inst.Resources) != 2 {
		t.Errorf("the instance's resources %v: want the regional network and one managed volume", inst.Resources)
	}
	claim := claimOf(t, node)
	ref := byPrice(filterOfferings(reference(t, us1), func(o offering) bool {
		return o.API == next && o.Capacity == "on-demand" && o.Zone == inst.Zone
	}))[0]
	// The volume's price (100 GiB at 0.10 per GiB-month) and the larger root disk
	// (64 GiB more) are on top of the instance price.
	if want := ref.Price + (100+64)*0.10/730; absDiff(parsePrice(t, claim), want) > 1e-6 {
		t.Errorf("price annotation %v, want %v (instance %v plus the disks)", parsePrice(t, claim), want, ref.Price)
	}
	if claim.Annotations[v1alpha1.AnnotationPriceIncomplete] != "" || claim.Annotations[v1alpha1.AnnotationProfileHashes] == "" {
		t.Errorf("annotations %v", claim.Annotations)
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// S15: the pair of 00a section 8.2. A StatefulSet and a Deployment with one
// placement profile and run key share one group, land in one zone on two nodes;
// the group is deleted (perUse) after both are gone.
func TestS15PlacementPair(t *testing.T) {
	types := takeTypes(t, []string{us1}, 2, 2)
	server, client2 := types[0], types[1]
	createNodeClassWith(t, "s15", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Profiles.Placement = map[string]v1alpha1.PlacementProfile{
			"colocated": {AWS: &awsapi.Placement{PlacementGroup: &awsapi.PlacementGroup{Strategy: "cluster"}}},
		}
	}, us1)
	createNodePool(t, "s15", "s15", in(labelRegion, us1), in(labelCapacity, "on-demand"))

	// One zone with both types: the deterministic alternative to a zone affinity.
	zonesOf := func(typ string) []string {
		var zs []string
		for _, o := range reference(t, us1) {
			if o.API == typ && o.Capacity == "on-demand" {
				zs = append(zs, o.Zone)
			}
		}
		return zs
	}
	var zone string
	for _, z := range zonesOf(server) {
		if slices.Contains(zonesOf(client2), z) {
			zone = z
			break
		}
	}
	if zone == "" {
		t.Skipf("%s and %s have no on-demand zone in common", server, client2)
	}
	runKey := fmt.Sprintf("pg-%d", time.Now().Unix()%100000)
	mark := time.Now()
	sel := func(typ string) map[string]string {
		return map[string]string{
			labelNodePool: "s15", labelType: vendor + "." + typ, labelCapacity: "on-demand", labelZone: zone,
			v1alpha1.LabelPlacementProfile: "colocated", v1alpha1.LabelPlacementGroup: runKey,
		}
	}
	lbl := map[string]string{"app": "pg"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "pg", Replicas: ptr.To[int32](1), Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}, Spec: podSpec(sel(server), "100m")},
		},
	}
	if err := k8s.Create(ctx(), sts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx(), sts, client.PropagationPolicy(metav1.DeletePropagationForeground)) })
	dep := createDeployment(t, "pg-client", sel(client2), "100m")

	nodeServer := waitPodRunning(t, "pg-0", long)
	var nodeClient string
	eventually(t, long, 2*time.Second, "the client pod running", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx(), &pods, client.InNamespace("default"), client.MatchingLabels(dep.Spec.Selector.MatchLabels)); err != nil {
			return false, err.Error()
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning {
				nodeClient = p.Spec.NodeName
				return true, ""
			}
		}
		return false, "no running pod"
	})
	if nodeServer == nodeClient {
		t.Fatal("the pair landed on one node")
	}

	group := assertGroupShared(t, mark, zone, runKey, nodeServer, nodeClient)

	// Both workloads go: the nodes are consolidated, then the group is deleted
	// after the perUse grace.
	_ = k8s.Delete(ctx(), sts, client.PropagationPolicy(metav1.DeletePropagationForeground))
	_ = k8s.Delete(ctx(), dep, client.PropagationPolicy(metav1.DeletePropagationForeground))
	waitNodeGone(t, nodeServer)
	waitNodeGone(t, nodeClient)
	eventually(t, 3*time.Minute, 2*time.Second, "the group deleted", func() (bool, string) {
		for _, g := range resourcesOf(t, "placementGroups", us1) {
			if g.ID == group.ID {
				return false, "the group still exists"
			}
		}
		return true, ""
	})
	var lastInstanceDelete, groupDelete time.Time
	for _, c := range callsSince(t, mark, "") {
		switch {
		case c.Op == "delete" && c.Time.After(lastInstanceDelete):
			lastInstanceDelete = c.Time
		case c.Op == "resource-delete" && c.Resource == group.ID && c.Result == "ok":
			groupDelete = c.Time
		}
	}
	if groupDelete.IsZero() || groupDelete.Before(lastInstanceDelete) {
		t.Fatalf("the group was deleted at %v, the last instance at %v", groupDelete, lastInstanceDelete)
	}
	if grace := groupDelete.Sub(lastInstanceDelete); grace < 19*time.Second {
		t.Errorf("the group was deleted %s after its last user, the perUse grace is 20s", grace)
	}
}

// assertGroupShared checks that the two nodes of the pair share one managed
// placement group, created once, in the zone, with the run key and the profile
// on the nodes; it returns the group.
func assertGroupShared(t *testing.T, mark time.Time, zone, runKey, nodeServer, nodeClient string) fakeResource {
	t.Helper()
	a, b := instanceOfNode(t, nodeServer), instanceOfNode(t, nodeClient)
	if a.Zone != zone || b.Zone != zone {
		t.Errorf("zones %s and %s, want both %s", a.Zone, b.Zone, zone)
	}
	groups := resourcesOf(t, "placementGroups", us1)
	var group fakeResource
	for _, g := range groups {
		if slices.Contains(a.Resources, g.ID) {
			group = g
		}
	}
	if group.ID == "" || !group.Managed || !slices.Contains(b.Resources, group.ID) {
		t.Fatalf("the nodes %v and %v do not share one managed group (groups: %+v)", a.Resources, b.Resources, groups)
	}
	if group.Tags["nodeclass"] != "s15" {
		t.Errorf("group tags %v", group.Tags)
	}
	created := 0
	for _, c := range callsSince(t, mark, "resource-ensure") {
		if c.Resource == group.ID && c.Result == "created" {
			created++
		}
	}
	if created != 1 {
		t.Errorf("the group was created %d times", created)
	}
	for _, node := range []string{nodeServer, nodeClient} {
		l := nodeLabels(t, node)
		if l[v1alpha1.LabelPlacementGroup] != runKey || l[v1alpha1.LabelPlacementProfile] != "colocated" {
			t.Errorf("node %s labels %v", node, l)
		}
	}
	return group
}

func resourceUseConfig(tags map[string]string) *awsapi.Resources {
	return &awsapi.Resources{Subnets: &common.Resource{Use: &common.ResourceUse{Selector: &common.TagSelector{Tags: tags}}}}
}

// waitResourcesReady waits for the vendor condition ResourcesReady of the class
// to have the status and a message that contains every string of contains and
// none of absent, and returns the message. A message may name more regions than
// the final one while the fake's configuration is still being read.
func waitResourcesReady(t *testing.T, class string, want metav1.ConditionStatus, contains []string, absent ...string) string {
	t.Helper()
	msg := ""
	eventually(t, 3*time.Minute, 2*time.Second, fmt.Sprintf("ResourcesReady=%s of %s", want, class), func() (bool, string) {
		cond, ok, _ := vendorCondition(class, v1alpha1.VendorConditionResourcesReady)
		if !ok || cond.Status != want {
			return false, fmt.Sprintf("%+v", cond)
		}
		for _, c := range contains {
			if !strings.Contains(cond.Message, c) {
				return false, fmt.Sprintf("message %q lacks %q", cond.Message, c)
			}
		}
		for _, c := range absent {
			if strings.Contains(cond.Message, c) {
				return false, fmt.Sprintf("message %q still names %q", cond.Message, c)
			}
		}
		msg = cond.Message
		return true, ""
	})
	return msg
}

func existing(items ...string) string { return strings.Join(items, "\n") + "\n" }

func subnetEntry(region, id string, tags map[string]string) string {
	kv := make([]string, 0, len(tags))
	for k, v := range tags {
		kv = append(kv, fmt.Sprintf("%q: %q", k, v))
	}
	return fmt.Sprintf("- {kind: subnets, region: %s, id: %s, tags: {%s}}", region, id, strings.Join(kv, ", "))
}

func managedOfClass(t *testing.T, kind, region, class string) []fakeResource {
	t.Helper()
	var out []fakeResource
	for _, r := range resourcesOf(t, kind, region) {
		if r.Managed && r.Tags["nodeclass"] == class {
			out = append(out, r)
		}
	}
	return out
}

// S16: vendor resources: use, missing, create, adoption after a restart,
// idleTTL, preCreate and forbid.
func TestS16Resources(t *testing.T) {
	t.Run("use by tag selector in two regions, never deleted; a missing resource makes the region unavailable", s16Use)
	t.Run("default create on the first launch, shared by the next; adoption after a restart", s16CreateAndAdopt)
	t.Run("forbid makes the offerings unavailable unless the vendor has a default", s16Forbid)
	// The rest has its own regions and classes and steers nothing: the waits for
	// TTLs overlap.
	t.Run("lifecycles", func(t *testing.T) {
		for name, f := range map[string]func(*testing.T){
			"idleTTL deletes an unused resource after the TTL":               s16IdleTTL,
			"preCreate keeps the resource while the region is listed":        s16PreCreate,
			"resources of a region that left the scope are still cleaned up": s16OutOfScope,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f(t)
			})
		}
	})
}

const useTag = "scarpenter.sparecores.com/use"

func s16Use(t *testing.T) {
	use := map[string]string{useTag: "true"}
	usEast, euWest := us1, eu1
	types := takeTypes(t, []string{usEast, euWest}, 2, 3)
	setFake(t, map[string]string{"existingResources": existing(
		subnetEntry(usEast, "subnet-use-us", use), subnetEntry(euWest, "subnet-use-eu", use))})
	createNodeClassWith(t, "s16-use", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = resourceUseConfig(use)
	}, usEast, euWest)
	waitResourcesReady(t, "s16-use", metav1.ConditionTrue, nil)
	createNodePool(t, "s16-use", "s16-use", in(labelCapacity, "on-demand"))
	mark := time.Now()

	for region, id := range map[string]string{usEast: "subnet-use-us", euWest: "subnet-use-eu"} {
		pod := "s16-use-" + region
		podWith(t, pod, map[string]string{labelNodePool: "s16-use", labelRegion: region, labelType: vendor + "." + types[0]}, nil)
		inst := instanceOfNode(t, waitPodRunning(t, pod, long))
		if !slices.Contains(inst.Resources, id) {
			t.Errorf("the instance in %s uses %v, want the subnet %s the user brought", region, inst.Resources, id)
		}
	}
	for _, c := range callsSince(t, mark, "") {
		if c.Kind == "subnets" {
			t.Errorf("a call on the subnets of a use configuration: %+v", c)
		}
	}
	if got := managedOfClass(t, "subnets", "", "s16-use"); len(got) != 0 {
		t.Errorf("SCarpenter created %+v for a use configuration", got)
	}

	// The user's subnet in eu-west-1 disappears.
	setFake(t, map[string]string{"existingResources": existing(subnetEntry(usEast, "subnet-use-us", use))})
	msg := waitResourcesReady(t, "s16-use", metav1.ConditionFalse, []string{euWest, "subnets"})
	if strings.Contains(msg, usEast) {
		t.Errorf("the message blames the region that is fine: %s", msg)
	}
	mark2 := time.Now()
	podWith(t, "s16-use-missing", map[string]string{labelNodePool: "s16-use", labelRegion: euWest, labelType: vendor + "." + types[1]}, nil)
	podWith(t, "s16-use-ok", map[string]string{labelNodePool: "s16-use", labelRegion: usEast, labelType: vendor + "." + types[2]}, nil)
	waitPodRunning(t, "s16-use-ok", long) // the region with its subnet is unaffected
	if phase, node := podPhase("s16-use-missing"); node != "" {
		t.Fatalf("the pod got the node %s (%s) although its region lacks the subnet", node, phase)
	}
	for _, c := range callsSince(t, mark2, "create") {
		if c.Region == euWest {
			t.Errorf("a launch in the region without its use resource: %+v", c)
		}
	}

	// The subnet is back: the region is available again and the pod runs.
	setFake(t, map[string]string{"existingResources": existing(
		subnetEntry(usEast, "subnet-use-us", use), subnetEntry(euWest, "subnet-use-eu", use))})
	waitResourcesReady(t, "s16-use", metav1.ConditionTrue, nil)
	inst := instanceOfNode(t, waitPodRunning(t, "s16-use-missing", long))
	if inst.Region != euWest || !slices.Contains(inst.Resources, "subnet-use-eu") {
		t.Errorf("the recovered launch: %+v", inst)
	}
}

func s16CreateAndAdopt(t *testing.T) {
	region := "ca-central-1"
	types := takeTypes(t, []string{region}, 2, 3)
	createNodeClass(t, "s16-create", region)
	createNodePool(t, "s16-create", "s16-create", in(labelRegion, region), in(labelCapacity, "on-demand"))
	if got := managedOfClass(t, "subnets", region, "s16-create"); len(got) != 0 {
		// The fake's account outlives a run on a kept cluster (KEEP_CLUSTER).
		t.Skipf("the account already has the subnet of an earlier run: %+v", got)
	}
	mark := time.Now()
	podWith(t, "s16-create-a", map[string]string{labelNodePool: "s16-create", labelType: vendor + "." + types[0]}, nil)
	nodeA := waitPodRunning(t, "s16-create-a", long)
	subnets := managedOfClass(t, "subnets", region, "s16-create")
	if len(subnets) != 1 {
		t.Fatalf("the first launch created %d subnets: %+v", len(subnets), subnets)
	}
	subnet := subnets[0]
	if want := "scarpenter-scarpenter-kwok-subnets-" + region; subnet.Name != want {
		t.Errorf("name %q, want the deterministic %q", subnet.Name, want)
	}
	if subnet.Tags["managed-by"] != "scarpenter" || subnet.Tags["cluster"] != "scarpenter-kwok" {
		t.Errorf("ownership marker %v", subnet.Tags)
	}
	if inst := instanceOfNode(t, nodeA); !slices.Contains(inst.Resources, subnet.ID) {
		t.Errorf("the instance does not use the created subnet: %v", inst.Resources)
	}
	// The next launch in the region uses it instead of creating another.
	podWith(t, "s16-create-b", map[string]string{labelNodePool: "s16-create", labelType: vendor + "." + types[1]}, nil)
	nodeB := waitPodRunning(t, "s16-create-b", long)
	if got := managedOfClass(t, "subnets", region, "s16-create"); len(got) != 1 || got[0].ID != subnet.ID {
		t.Errorf("the second launch changed the resources: %+v", got)
	}
	if inst := instanceOfNode(t, nodeB); !slices.Contains(inst.Resources, subnet.ID) {
		t.Errorf("the second instance does not share the subnet: %v", inst.Resources)
	}
	creates := 0
	for _, c := range callsSince(t, mark, "resource-ensure") {
		if c.Kind == "subnets" && c.Region == region && c.Result == "created" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("%d creations of the regional subnet, want 1", creates)
	}

	// The nodes go; the shared infrastructure stays (idleTTL, 24 h).
	deleteNodeFast(t, "s16-create-a", nodeA)
	deleteNodeFast(t, "s16-create-b", nodeB)
	waitNoClaims(t, "s16-create", long)
	if got := managedOfClass(t, "subnets", region, "s16-create"); len(got) != 1 {
		t.Fatalf("the idle shared subnet is gone after its users left: %+v", got)
	}

	// A restarted controller adopts it instead of creating a duplicate.
	restartController(t)
	waitNodeClassReady(t, "s16-create")
	mark2 := time.Now()
	podWith(t, "s16-create-c", map[string]string{labelNodePool: "s16-create", labelType: vendor + "." + types[2]}, nil)
	nodeC := waitPodRunning(t, "s16-create-c", long)
	after := managedOfClass(t, "subnets", region, "s16-create")
	if len(after) != 1 || after[0].ID != subnet.ID {
		t.Fatalf("after the restart the subnets are %+v, want %s adopted", after, subnet.ID)
	}
	if inst := instanceOfNode(t, nodeC); !slices.Contains(inst.Resources, subnet.ID) {
		t.Errorf("the instance after the restart does not use the adopted subnet: %v", inst.Resources)
	}
	for _, c := range callsSince(t, mark2, "resource-ensure") {
		if c.Resource == subnet.ID && c.Result != "found" {
			t.Errorf("after the restart: %+v, want the existing resource found", c)
		}
	}
	adopt := fmt.Sprintf(`scarpenter_resource_operations_total{kind="subnets",operation="adopt",region=%q,result="ok",vendor=%q}`, region, vendor)
	if n := metrics(t)[adopt]; n < 1 {
		t.Errorf("the adoption is not counted: %s = %v", adopt, n)
	}
}

func s16IdleTTL(t *testing.T) {
	region := "us-west-2"
	typ := takeTypes(t, []string{region}, 2, 1)[0]
	createNodeClassWith(t, "s16-ttl", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = &awsapi.Resources{Subnets: &common.Resource{Create: &common.ResourceCreate{
			Lifecycle: &common.Lifecycle{Policy: common.LifecycleIdleTTL, TTL: &metav1.Duration{Duration: 15 * time.Second}}}}}
	}, region)
	createNodePool(t, "s16-ttl", "s16-ttl", in(labelRegion, region), in(labelCapacity, "on-demand"))
	mark := time.Now()
	podWith(t, "s16-ttl", map[string]string{labelNodePool: "s16-ttl", labelType: vendor + "." + typ}, nil)
	node := waitPodRunning(t, "s16-ttl", long)
	subnets := managedOfClass(t, "subnets", region, "s16-ttl")
	if len(subnets) != 1 {
		t.Fatalf("subnets %+v", subnets)
	}
	deleteNodeFast(t, "s16-ttl", node)
	// Not before the TTL: the node is gone for a few seconds only.
	if got := managedOfClass(t, "subnets", region, "s16-ttl"); len(got) != 1 {
		t.Fatalf("the subnet was deleted at once: %+v", got)
	}
	eventually(t, 3*time.Minute, 2*time.Second, "the idle subnet deleted", func() (bool, string) {
		got := managedOfClass(t, "subnets", region, "s16-ttl")
		return len(got) == 0, fmt.Sprintf("%d subnets", len(got))
	})
	var instanceGone, deleted time.Time
	for _, c := range callsSince(t, mark, "") {
		switch {
		case c.Op == "delete" && c.Region == region:
			instanceGone = c.Time
		case c.Op == "resource-delete" && c.Resource == subnets[0].ID && c.Result == "ok":
			deleted = c.Time
		}
	}
	if idle := deleted.Sub(instanceGone); deleted.IsZero() || idle < 14*time.Second {
		t.Errorf("the subnet was deleted %s after its last user, the idleTTL is 15s", idle)
	}
}

// deleteNodeFast deletes the pod and the NodeClaim of its node instead of
// waiting for consolidation (the scenarios that use it are not about
// consolidation) and waits until the node is gone.
func deleteNodeFast(t *testing.T, pod, node string) {
	t.Helper()
	if err := k8s.Delete(ctx(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: "default"}},
		client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx(), &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: node}}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	waitNodeGone(t, node)
}

// s16OutOfScope: a region leaves the scope of the only NodeClass (it is not in
// the regions in use any more): the managed resource there is still cleaned up
// by its lifecycle, found by its ownership marker.
func s16OutOfScope(t *testing.T) {
	gone, kept := "eu-west-2", "eu-west-3"
	typ := takeTypes(t, []string{gone}, 2, 1)[0]
	createNodeClassWith(t, "s16-scope", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = &awsapi.Resources{Subnets: &common.Resource{Create: &common.ResourceCreate{
			Lifecycle: &common.Lifecycle{Policy: common.LifecycleIdleTTL, TTL: &metav1.Duration{Duration: 10 * time.Second}}}}}
	}, gone)
	createNodePool(t, "s16-scope", "s16-scope", in(labelRegion, gone), in(labelCapacity, "on-demand"))
	mark := time.Now()
	podWith(t, "s16-scope", map[string]string{labelNodePool: "s16-scope", labelType: vendor + "." + typ}, nil)
	node := waitPodRunning(t, "s16-scope", long)
	subnets := managedOfClass(t, "subnets", gone, "s16-scope")
	if len(subnets) != 1 {
		t.Fatalf("subnets %+v", subnets)
	}
	deleteNodeFast(t, "s16-scope", node)
	eventually(t, time.Minute, time.Second, "the NodeClass moved to another region", func() (bool, string) {
		got := &v1alpha1.SCNodeClass{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: "s16-scope"}, got); err != nil {
			return false, err.Error()
		}
		got.Spec.Vendors.AWS.Regions = []string{kept}
		if err := k8s.Update(ctx(), got); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	eventually(t, 3*time.Minute, 2*time.Second, "the subnet of the abandoned region deleted", func() (bool, string) {
		got := managedOfClass(t, "subnets", gone, "s16-scope")
		return len(got) == 0, fmt.Sprintf("%d subnets", len(got))
	})
	deleted := false
	for _, c := range callsSince(t, mark, "resource-delete") {
		deleted = deleted || (c.Resource == subnets[0].ID && c.Result == "ok")
	}
	if !deleted {
		t.Error("no recorded deletion of the subnet")
	}
}

func s16PreCreate(t *testing.T) {
	region := "ap-south-1"
	createNodeClassWith(t, "s16-pre", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = &awsapi.Resources{Subnets: &common.Resource{Create: &common.ResourceCreate{
			Lifecycle: &common.Lifecycle{Policy: common.LifecycleIdleTTL, TTL: &metav1.Duration{Duration: 10 * time.Second}},
			Timing:    &common.Timing{Mode: common.TimingPreCreate, Regions: []string{region}},
		}}}
	}, region)
	var created fakeResource
	eventually(t, 3*time.Minute, 2*time.Second, "the resource created ahead of any launch", func() (bool, string) {
		got := managedOfClass(t, "subnets", region, "s16-pre")
		if len(got) == 1 {
			created = got[0]
		}
		return len(got) == 1, fmt.Sprintf("%d subnets", len(got))
	})
	// Idle for longer than the TTL plus two collection intervals: kept.
	consistently(t, 25*time.Second, 5*time.Second, "the preCreate resource must be kept while its region is listed", func() (bool, string) {
		got := managedOfClass(t, "subnets", region, "s16-pre")
		return len(got) != 1 || got[0].ID != created.ID, fmt.Sprintf("%+v", got)
	})
	// Unlisted: it is idle for longer than the TTL and goes.
	eventually(t, time.Minute, time.Second, "the NodeClass update", func() (bool, string) {
		got := &v1alpha1.SCNodeClass{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: "s16-pre"}, got); err != nil {
			return false, err.Error()
		}
		got.Spec.Vendors.AWS.Resources.Subnets.Create.Timing = nil
		if err := k8s.Update(ctx(), got); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	eventually(t, 3*time.Minute, 2*time.Second, "the unlisted resource deleted", func() (bool, string) {
		got := managedOfClass(t, "subnets", region, "s16-pre")
		return len(got) == 0, fmt.Sprintf("%d subnets", len(got))
	})
}

func s16Forbid(t *testing.T) {
	noDefault, withDefault := "sa-east-1", "eu-north-1"
	types := takeTypes(t, []string{noDefault, withDefault}, 2, 2)
	setFake(t, map[string]string{"existingResources": fmt.Sprintf("- {kind: subnets, region: %s, id: subnet-default, default: true}\n", withDefault)})
	createNodeClassWith(t, "s16-forbid", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Vendors.AWS.Resources = &awsapi.Resources{Subnets: &common.Resource{Forbid: true}}
	}, noDefault, withDefault)
	waitResourcesReady(t, "s16-forbid", metav1.ConditionFalse, []string{noDefault, "forbidden"}, withDefault)
	createNodePool(t, "s16-forbid", "s16-forbid", in(labelCapacity, "on-demand"))
	mark := time.Now()
	podWith(t, "s16-forbid-no", map[string]string{labelNodePool: "s16-forbid", labelRegion: noDefault, labelType: vendor + "." + types[0]}, nil)
	podWith(t, "s16-forbid-ok", map[string]string{labelNodePool: "s16-forbid", labelRegion: withDefault, labelType: vendor + "." + types[1]}, nil)
	inst := instanceOfNode(t, waitPodRunning(t, "s16-forbid-ok", long))
	if !slices.Contains(inst.Resources, "subnet-default") {
		t.Errorf("the instance uses %v, want the vendor's default subnet", inst.Resources)
	}
	if phase, node := podPhase("s16-forbid-no"); node != "" {
		t.Fatalf("the pod got the node %s (%s) in a region where creation is forbidden", node, phase)
	}
	for _, c := range callsSince(t, mark, "") {
		if c.Region == noDefault || (c.Kind == "subnets" && c.Op == "resource-ensure") {
			t.Errorf("the fake was called for the forbidden region: %+v", c)
		}
	}
}

// restartController restarts the controller's pod and waits until the new one
// serves: the fake's in-memory state is gone, its account (the resources) is
// not.
func restartController(t *testing.T) {
	t.Helper()
	dep := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: ns, Name: "scarpenter"}
	if err := k8s.Get(ctx(), key, dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["scarpenter.sparecores.com/restarted-at"] = time.Now().Format(time.RFC3339Nano)
	if err := k8s.Update(ctx(), dep); err != nil {
		t.Fatal(err)
	}
	gen := dep.Generation // the update returned the object with its new generation
	eventually(t, 5*time.Minute, 2*time.Second, "the controller restarted", func() (bool, string) {
		got := &appsv1.Deployment{}
		if err := k8s.Get(ctx(), key, got); err != nil {
			return false, err.Error()
		}
		s := got.Status
		ok := s.ObservedGeneration >= gen && s.UpdatedReplicas == 1 && s.Replicas == 1 && s.AvailableReplicas == 1
		return ok, fmt.Sprintf("%+v", s)
	})
	eventually(t, 2*time.Minute, 2*time.Second, "the new controller serves its metrics", func() (bool, string) {
		_, ok := metrics(t)["scarpenter_fake_state_revision"]
		return ok, "no metrics yet"
	})
}

// S17: incomplete prices. Provisioned IOPS are not priced: the offerings are
// flagged (price-incomplete) and still used; pricing.allowIncomplete: false
// excludes them.
func TestS17IncompletePrice(t *testing.T) {
	typ := takeTypes(t, []string{us1}, 2, 1)[0]
	provisioned := func(allow *bool) func(*v1alpha1.SCNodeClass) {
		return func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Vendors.AWS.Storage = &awsapi.Storage{Root: &awsapi.RootVolume{VolumeType: awsapi.VolumeTypeGP3, IOPS: ptr.To[int32](4000)}}
			nc.Spec.Pricing.AllowIncomplete = allow
		}
	}
	createNodeClassWith(t, "s17-allow", provisioned(nil), us1)
	createNodeClassWith(t, "s17-deny", provisioned(new(false)), us1)
	createNodePool(t, "s17-allow", "s17-allow", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	createNodePool(t, "s17-deny", "s17-deny", in(labelRegion, us1), in(labelCapacity, "on-demand"))
	mark := time.Now()

	podWith(t, "s17-allow", map[string]string{labelNodePool: "s17-allow", labelType: vendor + "." + typ}, nil)
	node := waitPodRunning(t, "s17-allow", long)
	claim := claimOf(t, node)
	if claim.Annotations[v1alpha1.AnnotationPriceIncomplete] != "true" {
		t.Errorf("annotations %v: provisioned IOPS are not priced, the price must be flagged incomplete", claim.Annotations)
	}
	inst := instanceOfNode(t, node)
	ref := byPrice(filterOfferings(reference(t, us1), func(o offering) bool {
		return o.API == typ && o.Capacity == "on-demand" && o.Zone == inst.Zone
	}))[0]
	if price := parsePrice(t, claim); absDiff(price, ref.Price) > 1e-9 {
		t.Errorf("price annotation %v, want the instance price %v (the IOPS are not in the price)", price, ref.Price)
	}
	if !strings.Contains(inst.Settings, "4000") {
		t.Errorf("the settings %s lack the provisioned IOPS", inst.Settings)
	}

	// The same NodeClass without incomplete prices has no offering at all.
	podWith(t, "s17-deny", map[string]string{labelNodePool: "s17-deny", labelType: vendor + "." + typ}, nil)
	consistently(t, 25*time.Second, 2*time.Second, "no NodeClaim for a class that excludes incomplete prices", func() (bool, string) {
		cs, _ := listClaims("s17-deny")
		return len(cs) > 0, fmt.Sprintf("%d NodeClaims", len(cs))
	})
	for _, c := range callsSince(t, mark, "create") {
		if c.NodeClaim != node {
			t.Errorf("a launch for the class that excludes incomplete prices: %+v", c)
		}
	}
}
