//go:build qemu && scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/nvidia"
)

// The GPU-less boot (plan 12 step (d)): the qemu catalog has two NVIDIA types, a T4
// (qemu.gpu) and an 8-GPU A100 HGX system (qemu.gpu-nvswitch), and the VMs have no GPU.
// The development build of the controller offers them with the driver labels
// (SCARPENTER_DEV_NVIDIA_BOOTSTRAP=1; nvidia.NodeBootstrap is false until the step that
// ships the whole GPU path), and run.sh serves the release's driver sysext and Fabric
// Manager's archive as the mirror (--nvidia-driver-base-url). The node registers with the
// gpu-pending taint (ADR-031), its check fails, and the node is labelled gpu-ready=false.
//
// The GPU pending controller deletes the NodeClaim of a node labelled gpu-ready=false,
// within seconds of the label: the VM would vanish while the scenario looks inside it. So
// Q12 and Q15 run without that controller (SCARPENTER_DEV_NO_GPU_PENDING=1, a
// development switch); Q13 runs with it and asserts the deletion, and Q14 (no check, so
// no label) runs with it to show it leaves the node alone.

const (
	gpuNodeSelector = corev1.LabelInstanceTypeStable
	// The table's choice for a T4 and an A100 on the lane's Flatcar release.
	gpuDriverSysext = "nvidia-drivers-570-open"
	gpuDriverBranch = "570"
)

// gpuLane restarts SCarpenter as the GPU scenarios need it and restores the plain
// controller when the test ends. The test is skipped when the lane has no mirror.
func gpuLane(t *testing.T, gpuPending bool) {
	t.Helper()
	if lane.nvidiaBase == "" {
		t.Skip("the GPU scenarios are opt-in: run with QEMU_SCENARIOS=GPU")
	}
	// run.sh serves the driver of this selection; a table change needs the lane's versions updated.
	sel, reason := nvidia.Select(nvidia.GPU{Manufacturer: nvidia.Manufacturer, Model: "T4", Count: 1, AMD64: true}, flatcarVer)
	if reason != nvidia.Supported || sel.Sysext != gpuDriverSysext {
		t.Fatalf("the table selects %q (%s) for a T4 on Flatcar %s: update the NVIDIA mirror of test/qemu/run.sh and gpuDriverSysext",
			sel.Sysext, reason, flatcarVer)
	}
	prefetchImage(t)
	env := []string{"SCARPENTER_DEV_NVIDIA_BOOTSTRAP=1"}
	if !gpuPending {
		env = append(env, "SCARPENTER_DEV_NO_GPU_PENDING=1")
	}
	restartControllerWithEnv(t, env, "--nvidia-driver-base-url="+lane.nvidiaBase)
	// Registered first, so that it runs after the pool's cleanup.
	t.Cleanup(func() {
		stopController()
		ctlMu.Lock()
		ctlClientCA, ctlExtra, ctlEnv = true, nil, nil
		ctlMu.Unlock()
	})
}

// gpuPool creates a NodePool of one NVIDIA type. It lists the gpu-pending taint as a
// startup taint, as every pool that offers NVIDIA types must (without it the types are
// left out of the offerings).
func gpuPool(t *testing.T, name, instanceType string) {
	t.Helper()
	createNodePoolWith(t, name, name, func(np *karpv1.NodePool) {
		np.Spec.Template.Spec.StartupTaints = []corev1.Taint{{Key: v1alpha1.TaintGPUPending, Effect: corev1.TaintEffectNoSchedule}}
	}, req(gpuNodeSelector, instanceType))
	createPod(t, name+"-probe", map[string]string{gpuNodeSelector: instanceType})
}

// gpuNode waits for the VM of the pool to be reachable by ssh and its Node to be registered.
func gpuNode(t *testing.T, pool string) (claim, ip string, node *corev1.Node) {
	t.Helper()
	eventually(t, bootTimeout, 2*time.Second, "the VM reachable by ssh", func() (bool, string) {
		if claim = vmClaim(t, pool); claim == "" {
			return false, "no VM yet"
		}
		if ip = vmAddress(claim); ip == "" {
			return false, "no address yet"
		}
		out, err := ssh(t, ip, "hostname")
		return err == nil && out == claim, fmt.Sprintf("%v %s", err, out)
	})
	eventually(t, bootTimeout, 2*time.Second, "the Node registered", func() (bool, string) {
		n := &corev1.Node{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: claim}, n); err != nil {
			return false, err.Error()
		}
		node = n
		return true, ""
	})
	return claim, ip, node
}

func hasPendingTaint(n *corev1.Node) bool {
	return slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool {
		return t.Key == v1alpha1.TaintGPUPending && t.Effect == corev1.TaintEffectNoSchedule
	})
}

func getNode(t *testing.T, name string) *corev1.Node {
	t.Helper()
	n := &corev1.Node{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: name}, n); err != nil {
		t.Fatal(err)
	}
	return n
}

// checkGPUBoot asserts what every GPU-less boot shows, whatever happens to the check: both
// sysexts are merged and the driver image is the pinned one, nouveau is not loaded and
// modprobe does not run the driver's nvidia-drm install line, nvidia.service ends harmlessly,
// containerd has runc as the default and the nvidia handler, and the Node registered with
// the driver branch label and the gpu-pending taint.
func checkGPUBoot(t *testing.T, ip string, node *corev1.Node) {
	t.Helper()
	sh := func(cmd string) string { return mustSSH(t, ip, cmd) }
	status := sh("systemd-sysext status 2>&1")
	for _, want := range []string{gpuDriverSysext, "nvidia-runtime", "kubernetes"} {
		if !strings.Contains(status, want) {
			t.Errorf("the sysext %s is not merged:\n%s", want, status)
		}
	}
	// The mirror path: Ignition wrote the image, verified by the table's sha256 of the
	// extracted bytes, where the initrd looks for it before it downloads anything.
	sel, _ := nvidia.Select(nvidia.GPU{Manufacturer: nvidia.Manufacturer, Model: "T4", Count: 1, AMD64: true}, flatcarVer)
	file := fmt.Sprintf("/etc/flatcar/sysext/flatcar-%s-%s.raw", gpuDriverSysext, flatcarVer)
	if got := strings.Fields(sh("sudo sha256sum " + file))[0]; got != sel.RawSHA256 {
		t.Errorf("%s has the sha256 %s, the table pins %s", file, got, sel.RawSHA256)
	}

	// Not ordered before the kubelet: it may still be running when the node registers.
	eventually(t, 3*time.Minute, 2*time.Second, "nvidia.service done without a GPU", func() (bool, string) {
		out, _ := ssh(t, ip, "systemctl show nvidia.service -p Result -p ActiveState --value")
		return out == "active\nsuccess" || out == "success\nactive", out
	})
	if got := sh("lsmod | grep -c '^nouveau' || true"); got != "0" {
		t.Errorf("nouveau is loaded")
	}
	if _, err := ssh(t, ip, "test -d /sys/module/nouveau"); err == nil {
		t.Errorf("/sys/module/nouveau exists")
	}
	if got := sh("modprobe -n -v nvidia-drm 2>&1"); !strings.Contains(got, "install /bin/true") {
		t.Errorf("modprobe does not resolve nvidia-drm to /bin/true: %q", got)
	}

	// D7's rewrite: the file's line says runc, and so does the running containerd, with the
	// nvidia handler next to it.
	if got := sh("grep -x 'default_runtime_name = \"runc\"' /etc/containerd/config-nvidia.toml"); got == "" {
		t.Errorf("config-nvidia.toml has no default_runtime_name = \"runc\" line")
	}
	var info struct {
		Config struct {
			Containerd struct {
				DefaultRuntimeName string                     `json:"defaultRuntimeName"`
				Runtimes           map[string]json.RawMessage `json:"runtimes"`
			} `json:"containerd"`
		} `json:"config"`
	}
	raw := sh("sudo crictl --runtime-endpoint unix:///run/containerd/containerd.sock info 2>/dev/null")
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("crictl info: %v\n%s", err, raw)
	}
	if got := info.Config.Containerd.DefaultRuntimeName; got != "runc" {
		t.Errorf("containerd's default runtime is %q, want runc", got)
	}
	if _, ok := info.Config.Containerd.Runtimes["nvidia"]; !ok {
		t.Errorf("containerd has no nvidia runtime handler: %s", raw)
	}

	// ADR-031: the kubelet does not wait for the check; the node registers with the taint
	// and the driver branch label, and the controller (or nobody) decides after.
	if got := sh("systemctl is-active kubelet"); got != "active" {
		t.Errorf("the kubelet is %q: it must not wait for the GPU check", got)
	}
	if !hasPendingTaint(node) {
		t.Errorf("the node registered without the taint %s: %v", v1alpha1.TaintGPUPending, node.Spec.Taints)
	}
	if got := node.Labels[v1alpha1.LabelNVIDIADriverBranch]; got != gpuDriverBranch {
		t.Errorf("the node's %s label is %q, want %s", v1alpha1.LabelNVIDIADriverBranch, got, gpuDriverBranch)
	}
}

// checkCheckFailed waits for the check to fail on a GPU-less VM and asserts its message and
// the label; the node keeps the taint.
func checkCheckFailed(t *testing.T, ip, claim string) {
	t.Helper()
	eventually(t, 5*time.Minute, 3*time.Second, "scarpenter-nvidia-check failed", func() (bool, string) {
		out, _ := ssh(t, ip, "systemctl is-failed scarpenter-nvidia-check.service")
		return out == "failed", out
	})
	journal := mustSSH(t, ip, "sudo journalctl -b -u scarpenter-nvidia-check.service --no-pager")
	for _, want := range []string{"the loaded NVIDIA driver is not 570.211.01", "the node is labelled gpu-ready=false"} {
		if !strings.Contains(journal, want) {
			t.Errorf("the check's journal lacks %q:\n%s", want, journal)
		}
	}
	eventually(t, 2*time.Minute, 2*time.Second, "the node labelled gpu-ready=false", func() (bool, string) {
		n := getNode(t, claim)
		return n.Labels[v1alpha1.LabelGPUReady] == "false", fmt.Sprintf("gpu-ready=%q", n.Labels[v1alpha1.LabelGPUReady])
	})
	if n := getNode(t, claim); !hasPendingTaint(n) {
		t.Errorf("a node whose check failed lost the taint: %v", n.Spec.Taints)
	}
}

// reportGPUTimeline records the boot timeline: the claim's launch and registration times,
// when the check ran and failed, and systemd's own breakdown.
func reportGPUTimeline(t *testing.T, tag, ip string, claim *karpv1.NodeClaim) {
	t.Helper()
	registered := claim.StatusConditions().Get(karpv1.ConditionTypeRegistered)
	launched := claim.StatusConditions().Get(karpv1.ConditionTypeLaunched)
	report(t, "%s boot timeline: launched after %s, registered after %s", tag,
		launched.LastTransitionTime.Sub(claim.CreationTimestamp.Time).Round(time.Second),
		registered.LastTransitionTime.Sub(claim.CreationTimestamp.Time).Round(time.Second))
	report(t, "%s systemd-analyze: %s", tag, strings.ReplaceAll(mustSSH(t, ip, "systemd-analyze time"), "\n", " "))
	report(t, "%s check unit: %s", tag, strings.ReplaceAll(mustSSH(t, ip,
		"systemctl show scarpenter-nvidia-check.service -p ExecMainStartTimestamp -p ExecMainExitTimestamp"), "\n", "; "))
}

func nodeClaimOf(t *testing.T, pool, node string) *karpv1.NodeClaim {
	t.Helper()
	claims, err := nodeClaims(pool)
	if err != nil {
		t.Fatal(err)
	}
	for i := range claims {
		if claims[i].Name == node {
			return &claims[i]
		}
	}
	t.Fatalf("no NodeClaim %s in pool %s", node, pool)
	return nil
}

// TestQ12GPUlessBootFailsTheCheck: a T4 type boots without a GPU. Both sysexts merge, the
// units behave (see checkGPUBoot), the check fails with its message, the node stays
// tainted and is labelled gpu-ready=false, and, without the GPU pending controller, keeps
// its NodeClaim. Q13 covers the controller's deletion.
func TestQ12GPUlessBootFailsTheCheck(t *testing.T) {
	gpuLane(t, false)
	createNodeClass(t, "q12")
	gpuPool(t, "q12", "qemu.gpu")

	claim, ip, node := gpuNode(t, "q12")
	checkGPUBoot(t, ip, node)
	checkCheckFailed(t, ip, claim)
	reportGPUTimeline(t, "Q12", ip, nodeClaimOf(t, "q12", claim))

	// Nothing may delete the NodeClaim: the controller that would is off.
	time.Sleep(30 * time.Second)
	if c := nodeClaimOf(t, "q12", claim); !c.DeletionTimestamp.IsZero() {
		t.Errorf("the NodeClaim is being deleted although the GPU pending controller is off")
	}
	n := getNode(t, claim)
	if !hasPendingTaint(n) || n.Labels[v1alpha1.LabelGPUReady] != "false" {
		t.Errorf("the node ended with taints %v and gpu-ready=%q, want the taint and false", n.Spec.Taints, n.Labels[v1alpha1.LabelGPUReady])
	}
	// The system DaemonSets tolerate everything: the tainted node still becomes Ready.
	eventually(t, 5*time.Minute, 3*time.Second, "the tainted node Ready", func() (bool, string) {
		ok, _ := nodeReady(getNode(t, claim))
		return ok, "not Ready"
	})
}

// TestQ13GPUCheckFailureDeletesTheNodeClaim: the same boot with the GPU pending controller
// of production: once the node is labelled gpu-ready=false it deletes the NodeClaim (Event
// GPUCheckFailed) and the VM goes. The harness keeps the controller here, rather than
// disabling it, because this deletion is the behaviour that gives a failed check any effect:
// without it the node would sit at GPU prices until expireAfter.
func TestQ13GPUCheckFailureDeletesTheNodeClaim(t *testing.T) {
	gpuLane(t, true)
	createNodeClass(t, "q13")
	gpuPool(t, "q13", "qemu.gpu")

	claim, _, node := gpuNode(t, "q13")
	if !hasPendingTaint(node) {
		t.Errorf("the node registered without the taint: %v", node.Spec.Taints)
	}
	eventually(t, 10*time.Minute, 2*time.Second, "the NodeClaim deleted after the failed check", func() (bool, string) {
		c := &karpv1.NodeClaim{}
		err := k8s.Get(ctx(), client.ObjectKey{Name: claim}, c)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err.Error()
		}
		return apierrors.IsNotFound(err) || !c.DeletionTimestamp.IsZero(), "the NodeClaim exists"
	})
	var events corev1.EventList
	if err := k8s.List(ctx(), &events); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events.Items {
		if e.Reason == "GPUCheckFailed" && e.InvolvedObject.Name == claim {
			found = true
		}
	}
	if !found {
		t.Errorf("no GPUCheckFailed Event for %s", claim)
	}
	// A pending Pod would make core launch the next one: stop that before the pool goes.
	if err := k8s.Delete(ctx(), &corev1.Pod{ObjectMeta: podMeta("q13-probe")}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Minute, 3*time.Second, "the VM gone", func() (bool, string) {
		return vmDir(claim) == "", "the VM remains"
	})
}

// TestQ14GPUMaskedCheckStillRegisters: a user's Butane masks the check unit (an operator who
// brings their own driver stack). The node registers, becomes Ready, never gets a
// gpu-ready label and keeps the taint, and is not touched by the controller within the
// scenario (its timeout is 30 minutes).
func TestQ14GPUMaskedCheckStillRegisters(t *testing.T) {
	gpuLane(t, true)
	masked := userButane(t) + `systemd:
  units:
    - name: scarpenter-nvidia-check.service
      mask: true
`
	createNodeClassWithButane(t, "q14", masked)
	gpuPool(t, "q14", "qemu.gpu")

	claim, ip, node := gpuNode(t, "q14")
	checkGPUBoot(t, ip, node)
	if got, _ := ssh(t, ip, "systemctl is-enabled scarpenter-nvidia-check.service"); got != "masked" {
		t.Errorf("the check unit is %q, want masked", got)
	}
	eventually(t, 5*time.Minute, 3*time.Second, "the node with the masked check Ready", func() (bool, string) {
		ok, _ := nodeReady(getNode(t, claim))
		return ok, "not Ready"
	})
	time.Sleep(30 * time.Second)
	n := getNode(t, claim)
	if v, ok := n.Labels[v1alpha1.LabelGPUReady]; ok {
		t.Errorf("a node whose check never ran has the label gpu-ready=%q", v)
	}
	if !hasPendingTaint(n) {
		t.Errorf("the node lost the taint without a passed check: %v", n.Spec.Taints)
	}
	if c := nodeClaimOf(t, "q14", claim); !c.DeletionTimestamp.IsZero() {
		t.Errorf("the NodeClaim is being deleted within the controller's timeout")
	}
}

// TestQ15NVSwitchFabricManagerWithoutGPU: an 8-GPU A100 HGX type boots without a GPU. On top
// of the T4's checks: Ignition wrote the archive's sha256 file, the unit downloaded the
// archive from the mirror, verified and unpacked it, ran nv-fabricmanager (it exits because
// there is no driver handle and the unit restarts it), the check wants the unit and is
// told to wait for the fabric, and it fails with the driver message (the fabric stage needs
// a loaded driver).
func TestQ15NVSwitchFabricManagerWithoutGPU(t *testing.T) {
	gpuLane(t, false)
	createNodeClass(t, "q15")
	gpuPool(t, "q15", "qemu.gpu-nvswitch")

	claim, ip, node := gpuNode(t, "q15")
	checkGPUBoot(t, ip, node)
	sh := func(cmd string) string { return mustSSH(t, ip, cmd) }
	const dir = "/opt/nvidia-fabricmanager"
	file := nvidia.FabricManagerFile("570.211.01")
	eventually(t, 5*time.Minute, 3*time.Second, "Fabric Manager's archive downloaded, verified and unpacked", func() (bool, string) {
		out, err := ssh(t, ip, "cd "+dir+" && sha256sum -c --quiet "+file+".sha256 && test -x bin/nv-fabricmanager && echo ok")
		return err == nil && strings.HasSuffix(out, "ok"), out
	})
	eventually(t, 3*time.Minute, 3*time.Second, "nv-fabricmanager running and failing without a GPU", func() (bool, string) {
		out, _ := ssh(t, ip, "sudo journalctl -b -u scarpenter-nvidia-fabricmanager.service --no-pager")
		return strings.Contains(out, "failed to allocate handle"), out
	})
	eventually(t, 2*time.Minute, 3*time.Second, "the Fabric Manager unit restarted", func() (bool, string) {
		out, _ := ssh(t, ip, "systemctl show scarpenter-nvidia-fabricmanager.service -p NRestarts --value")
		return out != "" && out != "0", "NRestarts " + out
	})
	if got := sh("systemctl show scarpenter-nvidia-check.service -p Wants -p After --value"); strings.Count(got, "scarpenter-nvidia-fabricmanager.service") < 2 {
		t.Errorf("the check does not want and follow the Fabric Manager unit: %q", got)
	}
	if got := sh("systemctl cat scarpenter-nvidia-check.service"); !strings.Contains(got, "scarpenter-nvidia-check 570.211.01 8 "+claim+" fabric") {
		t.Errorf("the check is not run for an 8-GPU fabric:\n%s", got)
	}
	checkCheckFailed(t, ip, claim)
	reportGPUTimeline(t, "Q15", ip, nodeClaimOf(t, "q15", claim))
}
