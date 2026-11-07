//go:build qemu && scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	qemuapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
)

const probeImage = "registry.k8s.io/e2e-test-images/busybox:1.36.1-1"

// userButane is the NodeClass's user Butane: it lets the lane in by ssh, and
// leaves a marker the scenarios look for (it proves that the third merged config
// of the user-data, read from a ConfigMap through the controller's Role, is
// applied).
func userButane(t *testing.T) string {
	t.Helper()
	pub, err := os.ReadFile(lane.sshKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`variant: flatcar
version: 1.1.0
passwd:
  users:
    - name: core
      ssh_authorized_keys:
        - %q
storage:
  files:
    - path: /etc/scarpenter/lane-marker
      mode: 0644
      contents:
        inline: user-butane
`, strings.TrimSpace(string(pub)))
}

// customNetwork is a custom network template: the direct template plus a marker.
const customNetwork = `variant: flatcar
version: 1.1.0
storage:
  files:
    - path: /etc/scarpenter/network-template
      mode: 0644
      contents:
        inline: custom-for-lane
`

// configMap creates a ConfigMap of the controller's namespace for the test.
func configMap(t *testing.T, name, key, data string) v1alpha1.ConfigMapKeyRef {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string]string{key: data}}
	if err := k8s.Create(ctx(), cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx(), cm) })
	return v1alpha1.ConfigMapKeyRef{Namespace: ns, Name: name, Key: key}
}

// nodeClassOpts edits a NodeClass before it is created.
type nodeClassOpts func(*v1alpha1.SCNodeClass)

// createNodeClass creates a NodeClass for the qemu vendor with the direct
// template and the user Butane, waits for Ready and deletes it with the test.
func createNodeClass(t *testing.T, name string, edit ...nodeClassOpts) *v1alpha1.SCNodeClass {
	t.Helper()
	return createNodeClassWithButane(t, name, userButane(t), edit...)
}

// createNodeClassWithButane is createNodeClass with another user Butane.
func createNodeClassWithButane(t *testing.T, name, userConfig string, edit ...nodeClassOpts) *v1alpha1.SCNodeClass {
	t.Helper()
	butane := configMap(t, name+"-butane", "config.bu", userConfig)
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar:    v1alpha1.FlatcarSpec{Version: flatcarVer},
			Kubernetes: v1alpha1.KubernetesSpec{Version: lane.k8sVersion},
			Network:    v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateDirect},
			Butane:     &v1alpha1.ButaneSpec{ConfigMapRef: butane},
			// A VM that never joins must not live forever.
			PoweroffAfter: &metav1.Duration{Duration: 40 * time.Minute},
			Vendors:       v1alpha1.VendorBlocks{QEMU: &qemuapi.Settings{}},
		},
	}
	for _, e := range edit {
		e(nc)
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

func nodeClassCondition(name, typ string) (metav1.ConditionStatus, string, string, error) {
	got := &v1alpha1.SCNodeClass{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: name}, got); err != nil {
		return "", "", "", err
	}
	for _, c := range got.Status.Conditions {
		if c.Type == typ {
			return c.Status, c.Reason, c.Message, nil
		}
	}
	return "", "", "", nil
}

func waitNodeClassReady(t *testing.T, name string) {
	t.Helper()
	eventually(t, 5*time.Minute, time.Second, "NodeClass "+name+" Ready", func() (bool, string) {
		status, reason, msg, err := nodeClassCondition(name, "Ready")
		if err != nil {
			return false, err.Error()
		}
		return status == metav1.ConditionTrue, fmt.Sprintf("Ready=%s %s %s", status, reason, msg)
	})
}

func req(key string, values ...string) karpv1.NodeSelectorRequirementWithMinValues {
	return karpv1.NodeSelectorRequirementWithMinValues{Key: key, Operator: corev1.NodeSelectorOpIn, Values: values}
}

// createNodePool creates a NodePool on the NodeClass for the qemu vendor that
// removes empty nodes quickly. On cleanup it deletes the pool and waits until its
// VMs are gone.
func createNodePool(t *testing.T, name, class string, reqs ...karpv1.NodeSelectorRequirementWithMinValues) *karpv1.NodePool {
	t.Helper()
	all := append([]karpv1.NodeSelectorRequirementWithMinValues{req(v1alpha1.LabelVendor, "qemu")}, reqs...)
	np := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name}}
	np.Spec.Template.Spec.NodeClassRef = &karpv1.NodeClassReference{
		Group: v1alpha1.Group, Kind: "SCNodeClass", Name: class,
	}
	np.Spec.Template.Spec.Requirements = all
	np.Spec.Disruption.ConsolidationPolicy = karpv1.ConsolidationPolicyWhenEmpty
	np.Spec.Disruption.ConsolidateAfter = karpv1.MustParseNillableDuration("10s")
	if err := k8s.Create(ctx(), np); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k8s.Delete(ctx(), np); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting NodePool %s: %v", name, err)
		}
		waitPoolGone(t, name)
	})
	return np
}

func nodeClaims(pool string) ([]karpv1.NodeClaim, error) {
	var list karpv1.NodeClaimList
	if err := k8s.List(ctx(), &list, client.MatchingLabels{karpv1.NodePoolLabelKey: pool}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// waitPoolGone waits for the NodeClaims of the pool and their VMs to be gone.
func waitPoolGone(t *testing.T, pool string) {
	t.Helper()
	eventually(t, 10*time.Minute, 2*time.Second, "the NodeClaims and VMs of "+pool+" gone", func() (bool, string) {
		claims, err := nodeClaims(pool)
		if err != nil {
			return false, err.Error()
		}
		if len(claims) > 0 {
			return false, fmt.Sprintf("%d NodeClaims remain", len(claims))
		}
		// A VM that outlives its NodeClaim would skew the next scenario's counts.
		for _, rec := range instanceRecords() {
			if strings.HasPrefix(rec.NodeName, pool+"-") {
				return false, "the VM of " + rec.NodeName + " remains"
			}
		}
		return true, ""
	})
}

// podSpec is a pod that sleeps, in the probe image (a shell, for kubectl exec).
func podSpec(name string, selector map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Labels: map[string]string{"app": name}},
		Spec: corev1.PodSpec{
			NodeSelector:                  selector,
			TerminationGracePeriodSeconds: new(int64(0)),
			Containers: []corev1.Container{{
				Name: "probe", Image: probeImage, Command: []string{"sh", "-c", "echo up; sleep 1000000"},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
				}},
			}},
		},
	}
}

func createPod(t *testing.T, name string, selector map[string]string) *corev1.Pod {
	t.Helper()
	pod := podSpec(name, selector)
	if err := k8s.Create(ctx(), pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx(), pod) })
	return pod
}

// createDeployment runs one replica of the probe, which the cluster recreates
// when its node is drained.
func createDeployment(t *testing.T, name string, selector map[string]string) {
	t.Helper()
	p := podSpec(name, selector)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: p.Labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: p.Labels}, Spec: p.Spec},
		},
	}
	if err := k8s.Create(ctx(), d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8s.Delete(ctx(), d)
		eventually(t, 3*time.Minute, time.Second, "the pods of "+name+" gone", func() (bool, string) {
			var pods corev1.PodList
			if err := k8s.List(ctx(), &pods, client.InNamespace("default"), client.MatchingLabels(p.Labels)); err != nil {
				return false, err.Error()
			}
			return len(pods.Items) == 0, fmt.Sprintf("%d pods", len(pods.Items))
		})
	})
}

func nodeReady(n *corev1.Node) (bool, time.Time) {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue, c.LastTransitionTime.Time
		}
	}
	return false, time.Time{}
}

func nodeIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// readyNodes returns the Ready nodes the pool made.
func readyNodes(pool string) ([]corev1.Node, error) {
	var list corev1.NodeList
	if err := k8s.List(ctx(), &list, client.MatchingLabels{karpv1.NodePoolLabelKey: pool}); err != nil {
		return nil, err
	}
	var out []corev1.Node
	for i := range list.Items {
		if ok, _ := nodeReady(&list.Items[i]); ok {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// waitReadyNode waits for a Ready node of the pool, not in the excluded set, and
// returns it with the NodeClaim that made it.
func waitReadyNode(t *testing.T, pool string, timeout time.Duration, exclude ...string) (*corev1.Node, *karpv1.NodeClaim) {
	t.Helper()
	var node *corev1.Node
	var claim *karpv1.NodeClaim
	eventually(t, timeout, 2*time.Second, "a Ready node of "+pool, func() (bool, string) {
		nodes, err := readyNodes(pool)
		if err != nil {
			return false, err.Error()
		}
		for i := range nodes {
			if !contains(exclude, nodes[i].Name) {
				node = &nodes[i]
				return true, ""
			}
		}
		claims, _ := nodeClaims(pool)
		var states []string
		for _, c := range claims {
			states = append(states, fmt.Sprintf("%s(launched=%v registered=%v)", c.Name,
				c.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue(),
				c.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue()))
		}
		return false, fmt.Sprintf("%d Ready nodes, claims %v", len(nodes), states)
	})
	claims, err := nodeClaims(pool)
	if err != nil {
		t.Fatal(err)
	}
	for i := range claims {
		if claims[i].Status.NodeName == node.Name || claims[i].Name == node.Name {
			claim = &claims[i]
		}
	}
	if claim == nil {
		t.Fatalf("no NodeClaim for the node %s", node.Name)
	}
	return node, claim
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }

// vmDir returns the instance directory of the VM that belongs to the NodeClaim,
// "" when there is none.
func vmDir(claim string) string {
	for _, rec := range instanceRecords() {
		if rec.NodeName == claim {
			return rec.dir
		}
	}
	return ""
}

func serialLog(claim string) string {
	dir := vmDir(claim)
	if dir == "" {
		// The VM was deleted: the driver kept its log.
		matches, _ := filepath.Glob(filepath.Join(lane.serialDir, claim+"-*.serial.log"))
		if len(matches) == 0 {
			return ""
		}
		b, _ := os.ReadFile(matches[0])
		return string(b)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "serial.log"))
	return string(b)
}
