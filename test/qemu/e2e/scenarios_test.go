//go:build qemu && scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/common"
	qemuapi "github.com/SpareCores/scarpenter/pkg/apis/v1alpha1/qemu"
)

// bootTimeout is how long a VM may take from the NodeClaim to a Ready node:
// Karpenter gives a NodeClaim 15 minutes to register, the lane asks for less.
const bootTimeout = 12 * time.Minute

// start makes the controller run and the image available.
func start(t *testing.T) {
	t.Helper()
	prefetchImage(t)
	startController(t)
}

var qemuOnly = map[string]string{v1alpha1.LabelVendor: "qemu"}

// report appends a line to the lane's report file.
func report(t *testing.T, format string, args ...any) {
	t.Helper()
	line := fmt.Sprintf(format, args...)
	t.Log(line)
	f, err := os.OpenFile(filepath.Join(lane.out, "report.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintln(f, line)
}

// TestQ01JoinAndServe: a pending Pod makes Karpenter launch a VM that boots
// Flatcar with the rendered Ignition, joins k3s with its bootstrap token and
// becomes Ready; kubectl logs and exec work (the serving certificate was
// approved); the bootstrap token is gone; deleting the Pod removes the Node, the
// NodeClaim and the VM.
func TestQ01JoinAndServe(t *testing.T) {
	start(t)
	createNodeClass(t, "q01")
	createNodePool(t, "q01", "q01")
	createPod(t, "q01-probe", qemuOnly)

	node, claim := waitReadyNode(t, "q01", bootTimeout)
	_, readySince := nodeReady(node)
	launched := claim.StatusConditions().Get(karpv1.ConditionTypeLaunched)
	registered := claim.StatusConditions().Get(karpv1.ConditionTypeRegistered)
	report(t, "Q01 boot-to-Ready: NodeClaim created to Ready %s (launched after %s, registered after %s)",
		readySince.Sub(claim.CreationTimestamp.Time).Round(time.Second),
		launched.LastTransitionTime.Sub(claim.CreationTimestamp.Time).Round(time.Second),
		registered.LastTransitionTime.Sub(claim.CreationTimestamp.Time).Round(time.Second))
	if node.Spec.ProviderID != "scarpenter://qemu/local/"+claim.Name {
		t.Errorf("the node's provider ID is %q", node.Spec.ProviderID)
	}
	for k, want := range map[string]string{
		v1alpha1.LabelVendor: "qemu", corev1.LabelInstanceTypeStable: "qemu.small", corev1.LabelTopologyRegion: "local",
		corev1.LabelTopologyZone: "local-a", corev1.LabelArchStable: "amd64",
	} {
		if node.Labels[k] != want {
			t.Errorf("label %s = %q, want %q", k, node.Labels[k], want)
		}
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == karpv1.UnregisteredTaintKey {
			t.Errorf("the node still has the unregistered taint")
		}
	}

	eventually(t, 10*time.Minute, 3*time.Second, "the Pod running", func() (bool, string) {
		pod := &corev1.Pod{}
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: "q01-probe"}, pod); err != nil {
			return false, err.Error()
		}
		return pod.Status.Phase == corev1.PodRunning, fmt.Sprintf("phase %s", pod.Status.Phase)
	})

	// kubectl logs and exec need the API server to trust the kubelet's serving
	// certificate (approved by SCarpenter's csr controller) and the kubelet to
	// trust the API server's client certificate (the k3s client CA, N2 of the
	// review of plan 05).
	eventually(t, 3*time.Minute, 3*time.Second, "kubectl logs", func() (bool, string) {
		out, err := kubectl(t, "logs", "q01-probe")
		return err == nil && strings.Contains(out, "up"), fmt.Sprintf("%v: %s", err, out)
	})
	eventually(t, 3*time.Minute, 3*time.Second, "kubectl exec", func() (bool, string) {
		out, err := kubectl(t, "exec", "q01-probe", "--", "echo", "hello")
		return err == nil && out == "hello", fmt.Sprintf("%v: %s", err, out)
	})
	servingApproved(t, node.Name)
	tokenGone(t, claim.Name)
	rootCAIsTheServerCA(t)

	ip := nodeIP(node)
	checkBootedNode(t, ip, claim.Name)

	// The excerpt of a successful boot for the report (no secrets: the token
	// lives in the Ignition, not on the console).
	if log := serialLog(claim.Name); log != "" {
		lines := strings.Split(log, "\n")
		_ = os.WriteFile(filepath.Join(lane.out, "serial-excerpt.txt"), []byte(strings.Join(lines[max(0, len(lines)-80):], "\n")), 0o644)
	}

	// Deleting the Pod: the empty node is consolidated, the VM goes.
	if err := k8s.Delete(ctx(), &corev1.Pod{ObjectMeta: podMeta("q01-probe")}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Minute, 3*time.Second, "the node, the NodeClaim and the VM gone", func() (bool, string) {
		claims, _ := nodeClaims("q01")
		nodeErr := k8s.Get(ctx(), client.ObjectKey{Name: node.Name}, &corev1.Node{})
		return len(claims) == 0 && nodeErr != nil && vmDir(claim.Name) == "" && vmProcesses(t) == 0,
			fmt.Sprintf("%d claims, node error %v, vm dir %q, %d processes", len(claims), nodeErr, vmDir(claim.Name), vmProcesses(t))
	})
}

func servingApproved(t *testing.T, node string) {
	t.Helper()
	out, err := kubectl(t, "get", "csr", "-o", "jsonpath={range .items[*]}{.spec.signerName} {.spec.username} {.status.conditions[*].type}{\"\\n\"}{end}")
	if err != nil {
		t.Fatal(err)
	}
	want := "kubernetes.io/kubelet-serving system:node:" + node + " Approved"
	if !strings.Contains(out, want) {
		t.Errorf("no approved kubelet-serving request of %s:\n%s", node, out)
	}
}

func tokenGone(t *testing.T, claim string) {
	t.Helper()
	out, err := kubectl(t, "-n", "kube-system", "get", "secrets", "-l", v1alpha1.Group+"/nodeclaim="+claim, "-o", "name")
	if err != nil || out != "" {
		t.Errorf("the bootstrap token of %s: %q, %v; want none", claim, out, err)
	}
}

// rootCAIsTheServerCA: the CA nodes get (kube-root-ca.crt) is the k3s server CA,
// the one that signs the API server's serving certificate (overview 15, Q4).
func rootCAIsTheServerCA(t *testing.T) {
	t.Helper()
	got, err := kubectl(t, "-n", ns, "get", "configmap", "kube-root-ca.crt", "-o", "jsonpath={.data.ca\\.crt}")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(filepath.Dir(lane.kubeletClientCA), "server-ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, strings.TrimSpace(string(want))) {
		t.Errorf("kube-root-ca.crt does not hold the k3s server CA")
	}
}

// checkBootedNode looks inside a node that joined: what plan 05 only rendered.
func checkBootedNode(t *testing.T, ip, claim string) {
	t.Helper()
	sh := func(cmd string) string { return mustSSH(t, ip, cmd) }
	if got := sh("hostname"); got != claim {
		t.Errorf("hostname %q, want %q", got, claim)
	}
	if got := sh("cat /etc/scarpenter/lane-marker"); got != "user-butane" {
		t.Errorf("the user Butane (third merged config) was not applied: %q", got)
	}
	if got := sh("cat /etc/scarpenter/network-template"); got != "direct" {
		t.Errorf("the network template marker is %q", got)
	}
	if got := sh("systemd-sysext status 2>&1 | grep -c kubernetes || true"); got == "0" {
		t.Errorf("the kubernetes sysext is not merged: %s", sh("systemd-sysext status 2>&1"))
	}
	if got := sh("systemctl is-active kubelet containerd"); got != "active\nactive" {
		t.Errorf("kubelet and containerd: %q", got)
	}
	if got := sh("systemctl show kubelet.service -p UpheldBy --value"); got == "" {
		t.Errorf("nothing upholds kubelet.service (the sysext's Upholds= did not apply)")
	}
	for _, unit := range []string{"update-engine.service", "locksmithd.service"} {
		if got, _ := ssh(t, ip, "systemctl is-enabled "+unit); got != "masked" {
			t.Errorf("%s is %q, want masked", unit, got)
		}
	}
	ca := sh("sudo cat /etc/kubernetes/pki/ca.crt")
	if !strings.HasPrefix(ca, "-----BEGIN CERTIFICATE-----") {
		t.Errorf("the cluster CA file: %q", ca)
	}
	wantClient, _ := os.ReadFile(lane.kubeletClientCA)
	if got := sh("sudo cat /etc/kubernetes/pki/kubelet-client-ca.crt"); strings.TrimSpace(got) != strings.TrimSpace(string(wantClient)) {
		t.Errorf("the kubelet client CA on the node is not the k3s client CA")
	}
	if got := sh("grep clientCAFile /var/lib/kubelet/config.yaml"); !strings.Contains(got, "kubelet-client-ca.crt") {
		t.Errorf("the kubelet config: %q", got)
	}
	// The tools the first-boot scripts use exist on Flatcar.
	for _, tool := range []string{"blkid", "lsblk", "findmnt", "flock", "mdadm", "mkfs.ext4", "mkfs.xfs", "mkfs.btrfs"} {
		if _, err := ssh(t, ip, "command -v "+tool+" || test -x /usr/sbin/"+tool+" || test -x /sbin/"+tool); err != nil {
			t.Errorf("%s does not exist on Flatcar", tool)
		}
	}
	if got := sh("sudo ls /var/lib/kubelet/pki"); !strings.Contains(got, "kubelet-client-current.pem") || !strings.Contains(got, "kubelet-server-current.pem") {
		t.Errorf("the kubelet's certificates: %q", got)
	}
}

func podMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: "default", Name: name}
}

// volumeLabel is the filesystem label the first-boot unit gives a volume
// (pkg/bootstrap/volumes.go): the first 8 hex digits of the name's SHA256.
func volumeLabel(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "scv-" + hex.EncodeToString(sum[:])[:8]
}

// storageProfile is a profile with three data disks (two of one size, which
// the guest finds by serial pattern and size, and one of its own size, found by
// its by-id link), each with another filesystem, and the local disks of the type
// striped into one volume.
func storageProfile(nc *v1alpha1.SCNodeClass) {
	disk := func(name, fs, mount, size string) qemuapi.DataDisk {
		return qemuapi.DataDisk{
			GuestVolume: common.GuestVolume{Name: name, Filesystem: common.Filesystem(fs), MountPath: mount},
			Size:        resource.MustParse(size),
		}
	}
	nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{"disks": {QEMU: &qemuapi.Storage{
		Data: []qemuapi.DataDisk{
			disk("alpha", "ext4", "/var/data/alpha", "1Gi"),
			disk("beta", "xfs", "/var/data/beta", "1Gi"),
			disk("gamma", "btrfs", "/var/data/gamma", "2Gi"),
		},
		LocalStorage: &common.LocalStorage{Policy: common.LocalStorageRaid0, MountPath: "/var/scratch"},
	}}}
}

// TestQ02FirstBootVolumes: a Pod that selects a storage profile gets a node whose
// data disks and local disks were formatted and mounted by the first-boot unit on
// real Flatcar, and that nothing is wiped or created twice across a reboot.
func TestQ02FirstBootVolumes(t *testing.T) {
	start(t)
	custom := configMap(t, "q02-network", "network.bu", customNetwork)
	createNodeClass(t, "q02", storageProfile, func(nc *v1alpha1.SCNodeClass) {
		// Also proves a custom network template from a ConfigMap, read through the
		// controller's ClusterRole.
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateCustom, Custom: &v1alpha1.CustomNetworkSpec{ConfigMapRef: custom}}
	})
	createNodePool(t, "q02", "q02")
	createPod(t, "q02-probe", map[string]string{v1alpha1.LabelVendor: "qemu", v1alpha1.LabelStorageProfile: "disks"})

	node, claim := waitReadyNode(t, "q02", bootTimeout)
	t.Logf("node labels: %v", node.Labels)
	if node.Labels[corev1.LabelInstanceTypeStable] != "qemu.local" {
		t.Errorf("the profile needs local disks, the node is %s", node.Labels[corev1.LabelInstanceTypeStable])
	}
	ip := nodeIP(node)
	sh := func(cmd string) string { return mustSSH(t, ip, cmd) }
	if got := sh("cat /etc/scarpenter/network-template"); got != "custom-for-lane" {
		t.Errorf("the custom network template was not applied: %q", got)
	}

	mounts := map[string]string{"alpha": "/var/data/alpha", "beta": "/var/data/beta", "gamma": "/var/data/gamma"}
	fstypes := map[string]string{"alpha": "ext4", "beta": "xfs", "gamma": "btrfs"}
	uuids := map[string]string{}
	for name, mount := range mounts {
		if got := sh("findmnt -n -o FSTYPE " + mount); got != fstypes[name] {
			t.Errorf("%s: %s is %q, want %s", name, mount, got, fstypes[name])
		}
		if got := sh("findmnt -n -o SOURCE " + mount); got == "" {
			t.Errorf("%s: nothing mounted at %s", name, mount)
		}
		if got := sh("sudo blkid -s LABEL -o value -L " + volumeLabel(name)); got == "" {
			t.Errorf("%s: no filesystem with the label %s", name, volumeLabel(name))
		}
		// The label's device unit waits for a volume that attaches late (N1 of the
		// review): the drop-in lifts the default 90 s of the start job.
		dev := "dev-disk-by\\x2dlabel-" + strings.ReplaceAll(volumeLabel(name), "-", "\\x2d") + ".device"
		if got := sh("systemctl show '" + dev + "' -p JobRunningTimeoutUSec --value"); got != "4min" {
			t.Errorf("%s: the device unit's job timeout is %q, want 4min", name, got)
		}
		uuids[name] = sh("findmnt -n -o UUID " + mount)
		sh(fmt.Sprintf("echo %s-data | sudo tee %s/marker >/dev/null", name, mount))
	}
	// Two disks of the same size ended up on two different devices.
	if a, b := sh("findmnt -n -o SOURCE /var/data/alpha"), sh("findmnt -n -o SOURCE /var/data/beta"); a == b {
		t.Errorf("alpha and beta are on the same device %s", a)
	}
	if got := sh("findmnt -n -o FSTYPE /var/scratch"); got != "ext4" {
		t.Errorf("the local storage at /var/scratch is %q", got)
	}
	if got := sh("sudo mdadm --detail /dev/md/scarpenter-local | grep -E 'Raid Level|Raid Devices'"); !strings.Contains(got, "raid0") || !regexp.MustCompile(`Raid Devices : 2\b`).MatchString(got) {
		t.Errorf("the local array: %s", got)
	}
	arrayUUID := sh("sudo mdadm --detail /dev/md/scarpenter-local | grep UUID")
	scratchUUID := sh("findmnt -n -o UUID /var/scratch")
	sh("echo scratch-data | sudo tee /var/scratch/marker >/dev/null")
	if got := sh("systemctl --failed --no-legend | grep -c scarpenter || true"); got != "0" {
		t.Errorf("failed units: %s", sh("systemctl --failed --no-legend"))
	}

	// A reboot must neither wipe a filesystem nor create one twice. Before it,
	// the filesystem of alpha is destroyed: of the two disks of one size, one
	// still carries beta's label and one is blank, and the unit of alpha must take
	// the blank one and leave beta's alone.
	bootID := sh("cat /proc/sys/kernel/random/boot_id")
	sh("sudo umount /var/data/alpha && sudo wipefs -a /dev/disk/by-label/" + volumeLabel("alpha") + " >/dev/null")
	_, _ = ssh(t, ip, "sudo systemctl reboot")
	eventually(t, 5*time.Minute, 3*time.Second, "the node back after the reboot", func() (bool, string) {
		id, err := ssh(t, ip, "cat /proc/sys/kernel/random/boot_id")
		return err == nil && id != bootID && id != "", fmt.Sprintf("%v %s", err, id)
	})
	eventually(t, 5*time.Minute, 3*time.Second, "the kubelet active again", func() (bool, string) {
		out, err := ssh(t, ip, "systemctl is-active kubelet")
		return err == nil && out == "active", out
	})

	for _, name := range []string{"beta", "gamma"} {
		if got := sh("cat " + mounts[name] + "/marker"); got != name+"-data" {
			t.Errorf("%s lost its data across the reboot: %q", name, got)
		}
		if got := sh("findmnt -n -o UUID " + mounts[name]); got != uuids[name] {
			t.Errorf("%s got a new filesystem (UUID %s, was %s)", name, got, uuids[name])
		}
		if got := sh("sudo journalctl -b -u scarpenter-volume-" + name + ".service --no-pager"); !strings.Contains(got, "already carries the label") {
			t.Errorf("the unit of %s did not recognise its filesystem:\n%s", name, got)
		}
	}
	if got := sh("findmnt -n -o FSTYPE /var/data/alpha"); got != "ext4" {
		t.Errorf("alpha is not mounted again: %q", got)
	}
	if _, err := ssh(t, ip, "test -e /var/data/alpha/marker"); err == nil {
		t.Error("alpha still has its data: the wiped disk was not formatted again")
	}
	if got := sh("findmnt -n -o UUID /var/data/alpha"); got == uuids["alpha"] {
		t.Error("alpha has the old UUID although its filesystem was wiped")
	}
	if got := sh("cat /var/scratch/marker"); got != "scratch-data" {
		t.Errorf("the local array lost its data across the reboot: %q", got)
	}
	if got := sh("findmnt -n -o UUID /var/scratch"); got != scratchUUID {
		t.Errorf("the local array's filesystem changed across the reboot")
	}
	if got := sh("sudo mdadm --detail /dev/md/scarpenter-local | grep UUID"); got != arrayUUID {
		t.Errorf("the array's identity changed across the reboot: %s, was %s", got, arrayUUID)
	}
	if got := sh("ls /dev/md | wc -l"); got != "1" {
		t.Errorf("/dev/md has %s entries after the reboot:\n%s", got, sh("ls -l /dev/md"))
	}
	if got := sh("systemctl --failed --no-legend | grep -c scarpenter || true"); got != "0" {
		t.Errorf("failed units after the reboot: %s", sh("systemctl --failed --no-legend"))
	}
	report(t, "Q02 volumes: 3 data disks (2 of one size by serial pattern, 1 by by-id) and a raid0 of 2 local disks mounted, kept across a reboot; node %s", claim.Name)
}

// TestQ03DriftReplacesTheNode: changing the root disk size of the qemu block
// drifts the NodeClaim; Karpenter launches a replacement and drains the old node,
// whose Pod moves.
func TestQ03DriftReplacesTheNode(t *testing.T) {
	start(t)
	createNodeClass(t, "q03")
	createNodePool(t, "q03", "q03")
	createDeployment(t, "q03-probe", qemuOnly)
	first, firstClaim := waitReadyNode(t, "q03", bootTimeout)
	if got := mustSSH(t, nodeIP(first), "lsblk -b -d -n -o SIZE /dev/vda"); got != fmt.Sprint(int64(20)<<30) {
		t.Errorf("the root disk of the first node is %s bytes, want the 20 GiB default", got)
	}

	nc := &v1alpha1.SCNodeClass{}
	if err := k8s.Get(ctx(), client.ObjectKey{Name: "q03"}, nc); err != nil {
		t.Fatal(err)
	}
	size := resource.MustParse("21Gi")
	nc.Spec.Vendors.QEMU.Storage = &qemuapi.Storage{Root: &qemuapi.RootDisk{Size: &size}}
	if err := k8s.Update(ctx(), nc); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Minute, 2*time.Second, "the NodeClaim drifted", func() (bool, string) {
		claim := &karpv1.NodeClaim{}
		if err := k8s.Get(ctx(), client.ObjectKey{Name: firstClaim.Name}, claim); err != nil {
			return false, err.Error()
		}
		return claim.StatusConditions().Get(karpv1.ConditionTypeDrifted).IsTrue(), "not drifted yet"
	})
	second, secondClaim := waitReadyNode(t, "q03", bootTimeout, first.Name)
	if got := mustSSH(t, nodeIP(second), "lsblk -b -d -n -o SIZE /dev/vda"); got != fmt.Sprint(int64(21)<<30) {
		t.Errorf("the replacement's root disk is %s bytes, want 21 GiB", got)
	}
	eventually(t, 10*time.Minute, 3*time.Second, "the first node, its NodeClaim and its VM gone", func() (bool, string) {
		err := k8s.Get(ctx(), client.ObjectKey{Name: first.Name}, &corev1.Node{})
		claimErr := k8s.Get(ctx(), client.ObjectKey{Name: firstClaim.Name}, &karpv1.NodeClaim{})
		return err != nil && claimErr != nil && vmDir(firstClaim.Name) == "", fmt.Sprintf("node: %v claim: %v vm: %q", err, claimErr, vmDir(firstClaim.Name))
	})
	eventually(t, 5*time.Minute, 3*time.Second, "the Pod running on the replacement", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx(), &pods, client.InNamespace("default"), client.MatchingLabels{"app": "q03-probe"}); err != nil {
			return false, err.Error()
		}
		for _, p := range pods.Items {
			if p.Spec.NodeName == second.Name && p.Status.Phase == corev1.PodRunning {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d pods", len(pods.Items))
	})
	_ = secondClaim
}

// TestQ04RestartDuringBoot: SCarpenter is killed while the launch of the VM is in
// progress (the VM runs, Create has not returned: the driver's launch delay holds
// it there, so that the window is certain); after the restart there is still one
// VM per NodeClaim (the replayed launch finds it by its idempotency key) and the
// node joins.
func TestQ04RestartDuringBoot(t *testing.T) {
	prefetchImage(t)
	restartController(t, true, "--qemu-launch-delay=1m")
	createNodeClass(t, "q04")
	createNodePool(t, "q04", "q04")
	createPod(t, "q04-probe", qemuOnly)
	var claim string
	eventually(t, 5*time.Minute, 20*time.Millisecond, "the VM of the NodeClaim running", func() (bool, string) {
		for _, rec := range instanceRecords() {
			if rec.Phase == "running" && rec.NodeName != "" {
				claim = rec.NodeName
				return true, ""
			}
		}
		return false, "no VM yet"
	})
	// The replay is certain only if core has not recorded the launch: Create is
	// still in the driver.
	var nc karpv1.NodeClaim
	if err := k8s.Get(ctx(), client.ObjectKey{Name: claim}, &nc); err != nil {
		t.Fatal(err)
	}
	if nc.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() {
		t.Fatalf("%s is already launched: the kill would not exercise the replay of Create", claim)
	}
	stopController()
	t.Logf("killed SCarpenter while the launch of %s was in progress", claim)
	if n := vmProcesses(t); n != 1 {
		t.Fatalf("%d VM processes after the kill, want the one that boots", n)
	}
	restartController(t, true)

	node, _ := waitReadyNode(t, "q04", bootTimeout)
	if node.Name != claim {
		t.Errorf("the node is %s, the NodeClaim of the first launch was %s: the launch was not replayed onto its VM", node.Name, claim)
	}
	records := instanceRecords()
	names := make([]string, 0, len(records))
	for _, rec := range records {
		names = append(names, rec.NodeName)
	}
	if len(names) != 1 || names[0] != node.Name {
		t.Errorf("the VMs after the restart: %v, want only %s", names, node.Name)
	}
	if n := vmProcesses(t); n != 1 {
		t.Errorf("%d VM processes after the restart, want 1", n)
	}
	claims, _ := nodeClaims("q04")
	if len(claims) != 1 {
		t.Errorf("%d NodeClaims, want 1", len(claims))
	}
}

// TestQ05UnreachableSysextNeverJoins: a NodeClass whose extra sysext cannot be
// fetched keeps Ignition retrying in the initrd; the node never joins. Only that is
// asserted (core replaces a NodeClaim that does not register after 15 minutes;
// the lane does not wait for it). The sysext's URL is the mirror's address with
// https, which the mirror (plain http) answers with a protocol error: its log
// shows that the VM did try to fetch the sysext, so that "no node" is not just
// "too early to tell".
func TestQ05UnreachableSysextNeverJoins(t *testing.T) {
	start(t)
	mirrorLog := filepath.Join(filepath.Dir(lane.stateDir), "mirror.log")
	before, _ := os.ReadFile(mirrorLog)
	createNodeClass(t, "q05", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Sysexts = []v1alpha1.Sysext{{
			Name: "unreachable", Arch: "amd64", URL: strings.Replace(lane.sysextBase, "http://", "https://", 1) + "/unreachable.raw",
			SHA256: strings.Repeat("ab", 32),
		}}
	})
	createNodePool(t, "q05", "q05")
	createPod(t, "q05-probe", qemuOnly)
	// Ignition retries a failing fetch for as long as it takes (there is no
	// terminal failure on the console). A VM that is merely slow would fetch and go
	// on within the 26 s a good boot takes to be Ready (Q01): a fetch request of the
	// VM still logged by the mirror at least minStuck after its first one means the
	// boot is stuck in the initrd, where no kubelet can start.
	const minStuck = 60 * time.Second
	eventually(t, 5*time.Minute, time.Second, "the VM still fetching the sysext a minute after its first request", func() (bool, string) {
		after, _ := os.ReadFile(mirrorLog)
		times := mirrorRequestTimes(strings.TrimPrefix(string(after), string(before)))
		if len(times) == 0 {
			return false, "no request of the VM in the mirror's log yet"
		}
		span := times[len(times)-1].Sub(times[0])
		return span >= minStuck, fmt.Sprintf("%d requests spanning %s, want %s", len(times), span, minStuck)
	})
	if log := serialLog(vmClaim(t, "q05")); strings.Contains(log, "login:") {
		t.Errorf("the VM reached the login prompt although its Ignition cannot fetch the sysext: %s", tail(log, 6))
	}
	var nodes corev1.NodeList
	if err := k8s.List(ctx(), &nodes, client.MatchingLabels{karpv1.NodePoolLabelKey: "q05"}); err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 0 {
		t.Errorf("a node of the pool exists although the sysext cannot be fetched: %s", nodes.Items[0].Name)
	}
	if ready, _ := readyNodes("q05"); len(ready) != 0 {
		t.Errorf("%d Ready nodes", len(ready))
	}
	claims, err := nodeClaims("q05")
	if err != nil || len(claims) != 1 || claims[0].StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
		t.Errorf("the NodeClaim of a VM that cannot boot: %d claims, %v; want one that is not registered", len(claims), err)
	}
}

// TestQ06SecretsThroughTheNamespacedRole: a NodeClass whose tailscale auth key
// is a Secret of the controller's namespace is read through the Role of
// config/rbac; a Secret in another namespace is refused until a Role grants it.
func TestQ06SecretsThroughTheNamespacedRole(t *testing.T) {
	startController(t)
	secret := func(namespace, name string) {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, StringData: map[string]string{"key": "tskey-auth-EXAMPLE"}}
		if err := k8s.Create(ctx(), s); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8s.Delete(ctx(), s) })
	}
	tailscale := func(namespace, name string) nodeClassOpts {
		return func(nc *v1alpha1.SCNodeClass) {
			nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
				Version: "v1.98.10", AuthKeySecretRef: v1alpha1.SecretKeyRef{Namespace: namespace, Name: name, Key: "key"},
			}}
		}
	}
	// The sysext checksums of the tailscale release come from the mirror: it has
	// no tailscale release unless the tailscale scenario prepared one, so this
	// scenario only reads the Secret (ValidationSucceeded), not the artifacts.
	secret(ns, "q06-auth-key")
	secret("default", "q06-foreign")
	nc := &v1alpha1.SCNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "q06"},
		Spec: v1alpha1.SCNodeClassSpec{
			Flatcar: v1alpha1.FlatcarSpec{Version: flatcarVer}, Kubernetes: v1alpha1.KubernetesSpec{Version: lane.k8sVersion},
			Vendors: v1alpha1.VendorBlocks{QEMU: &qemuapi.Settings{}},
		},
	}
	tailscale(ns, "q06-auth-key")(nc)
	foreign := nc.DeepCopy()
	foreign.Name = "q06-foreign"
	tailscale("default", "q06-foreign")(foreign)
	for _, o := range []*v1alpha1.SCNodeClass{nc, foreign} {
		if err := k8s.Create(ctx(), o); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8s.Delete(ctx(), o) })
	}

	validation := func(name string) (metav1.ConditionStatus, string, string) {
		status, reason, msg, err := nodeClassCondition(name, "ValidationSucceeded")
		if err != nil {
			return "", err.Error(), ""
		}
		return status, reason, msg
	}
	eventually(t, 2*time.Minute, time.Second, "the Secret of the controller's namespace read", func() (bool, string) {
		status, reason, msg := validation("q06")
		return status == metav1.ConditionTrue, fmt.Sprintf("%s %s %s", status, reason, msg)
	})
	eventually(t, 2*time.Minute, time.Second, "the foreign Secret refused", func() (bool, string) {
		status, reason, msg := validation("q06-foreign")
		return status == metav1.ConditionFalse && reason == "ReferenceUnreadable" && strings.Contains(msg, "Role"),
			fmt.Sprintf("%s %s %s", status, reason, msg)
	})
	// The documented grant (config/rbac/controller.yaml): a Role that names the
	// Secret and a RoleBinding to the controller's ServiceAccount.
	for _, args := range [][]string{
		{"-n", "default", "create", "role", "q06-auth-key", "--verb=get", "--resource=secrets", "--resource-name=q06-foreign"},
		{"-n", "default", "create", "rolebinding", "q06-auth-key", "--role=q06-auth-key", "--serviceaccount=" + ns + ":scarpenter"},
	} {
		if out, err := kubectl(t, args...); err != nil {
			t.Fatalf("kubectl %v: %v\n%s", args, err, out)
		}
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", "default", "delete", "rolebinding,role", "q06-auth-key", "--ignore-not-found")
	})
	eventually(t, 3*time.Minute, time.Second, "the foreign Secret read after the grant", func() (bool, string) {
		status, reason, msg := validation("q06-foreign")
		return status == metav1.ConditionTrue, fmt.Sprintf("%s %s %s", status, reason, msg)
	})
}

// TestQ07KubeletClientCAIsNeededOnK3s proves why --kubelet-client-ca-file exists
// (N2 of the review of plan 05): k3s signs the API server's kubelet client
// certificate with its client CA, not with the CA of kube-root-ca.crt that nodes
// get as the cluster CA, so a kubelet that trusts only the cluster CA answers
// kubectl logs and exec with 401 ("You must be logged in"). Without the flag the node joins and works, but
// logs fail.
func TestQ07KubeletClientCAIsNeededOnK3s(t *testing.T) {
	prefetchImage(t)
	restartController(t, false)
	t.Cleanup(func() { restartController(t, true) })
	createNodeClass(t, "q07")
	createNodePool(t, "q07", "q07")
	createPod(t, "q07-probe", qemuOnly)
	node, _ := waitReadyNode(t, "q07", bootTimeout)
	eventually(t, 10*time.Minute, 3*time.Second, "the Pod running", func() (bool, string) {
		pod := &corev1.Pod{}
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: "q07-probe"}, pod); err != nil {
			return false, err.Error()
		}
		return pod.Status.Phase == corev1.PodRunning, string(pod.Status.Phase)
	})
	servingApproved(t, node.Name)
	eventually(t, 2*time.Minute, 3*time.Second, "kubectl logs refused by the kubelet", func() (bool, string) {
		out, err := kubectl(t, "logs", "q07-probe")
		return err != nil && strings.Contains(out, "provide credentials"), fmt.Sprintf("%v: %s", err, out)
	})
	if got := mustSSH(t, nodeIP(node), "grep clientCAFile /var/lib/kubelet/config.yaml"); !strings.Contains(got, "/etc/kubernetes/pki/ca.crt") {
		t.Errorf("without the flag the kubelet trusts the cluster CA: %q", got)
	}
}

// TestQ08AFailedVolumeKeepsTheKubeletDown: a data disk that already carries a
// filesystem SCarpenter did not create (the lane puts a FAT filesystem on it
// before the volume unit runs) is never overwritten, the volume's mount fails, and
// the kubelet, which requires the mounts, does not start: the node never
// registers, so no Pod writes to the root disk instead of its volume. The node
// stays reachable by ssh for debugging.
func TestQ08AFailedVolumeKeepsTheKubeletDown(t *testing.T) {
	start(t)
	// The disk is the only one of its size: the guest finds it by its by-id link,
	// where the foreign filesystem is already there when the unit looks.
	serial := "scv" + func() string { sum := sha256.Sum256([]byte("foreign")); return hex.EncodeToString(sum[:])[:12] }()
	device := "/dev/disk/by-id/virtio-" + serial
	foreign := userButane(t) + `systemd:
  units:
    - name: scarpenter-lane-foreign.service
      enabled: true
      contents: |
        [Unit]
        Description=Put a foreign filesystem on the data disk (lane)
        DefaultDependencies=no
        After=local-fs-pre.target
        Before=scarpenter-volume-foreign.service
        [Service]
        Type=oneshot
        ExecStart=/bin/sh -c 'until [ -e ` + device + ` ]; do sleep 1; done; mkfs.vfat -n FOREIGN ` + device + `'
        [Install]
        WantedBy=local-fs.target
`
	createNodeClassWithButane(t, "q08", foreign, func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Profiles.Storage = map[string]v1alpha1.StorageProfile{"foreign": {QEMU: &qemuapi.Storage{
			Data: []qemuapi.DataDisk{{
				GuestVolume: common.GuestVolume{Name: "foreign", Filesystem: common.FilesystemExt4, MountPath: "/var/data/foreign"},
				Size:        resource.MustParse("1Gi"),
			}},
		}}}
	})
	createNodePool(t, "q08", "q08")
	createPod(t, "q08-probe", map[string]string{v1alpha1.LabelVendor: "qemu", v1alpha1.LabelStorageProfile: "foreign"})

	var claim, ip string
	eventually(t, 5*time.Minute, 2*time.Second, "the VM reachable by ssh", func() (bool, string) {
		for _, rec := range instanceRecords() {
			if rec.NodeName == "" {
				continue
			}
			claim = rec.NodeName
			if ip = vmAddress(claim); ip == "" {
				return false, "no address yet"
			}
			out, err := ssh(t, ip, "hostname")
			return err == nil && out == claim, fmt.Sprintf("%v %s", err, out)
		}
		return false, "no VM yet"
	})
	eventually(t, 5*time.Minute, 3*time.Second, "the volume unit failed", func() (bool, string) {
		out, _ := ssh(t, ip, "systemctl is-failed scarpenter-volume-foreign.service")
		return out == "failed", out
	})
	sh := func(cmd string) string { return mustSSH(t, ip, cmd) }
	if got := sh("sudo journalctl -b -u scarpenter-volume-foreign.service --no-pager"); !strings.Contains(got, "refusing to overwrite") {
		t.Errorf("the unit did not refuse to overwrite the foreign filesystem:\n%s", got)
	}
	if got := sh("sudo blkid -p -o value -s TYPE " + device); got != "vfat" {
		t.Errorf("the foreign filesystem was overwritten: the disk is %q now", got)
	}
	// Not "not started yet": systemd logged that the kubelet's start failed on its
	// dependency, the volume's mount.
	eventually(t, 2*time.Minute, 2*time.Second, "the kubelet's start failed on the volume's mount", func() (bool, string) {
		out, _ := ssh(t, ip, "sudo journalctl -b -u kubelet.service --no-pager 2>&1; systemctl list-jobs --no-legend 2>&1")
		return strings.Contains(out, "Dependency failed for"), out
	})
	if got := sh("systemctl show kubelet.service -p RequiresMountsFor --value"); !strings.Contains(got, "/var/data/foreign") {
		t.Errorf("the kubelet does not require the volume's mount: %q", got)
	}
	if _, err := ssh(t, ip, "findmnt /var/data/foreign"); err == nil {
		t.Error("the volume is mounted")
	}
	var nodes corev1.NodeList
	if err := k8s.List(ctx(), &nodes, client.MatchingLabels{karpv1.NodePoolLabelKey: "q08"}); err != nil || len(nodes.Items) != 0 {
		t.Errorf("the node registered although its volume failed: %d nodes, %v", len(nodes.Items), err)
	}
}

// TestQ09Tailscale boots a node with the tailscale template against the lane's
// Headscale (run.sh starts it, mints an ephemeral pre-authorized key and puts the
// container itself on the tailnet, so that the API server reaches the kubelets at
// their tailscale addresses): the sysext, tailscaled, the oneshot unit that joins
// the tailnet and writes the kubelet's --node-ip, the unit ordering, and a node
// that is Ready over the tailnet with working kubectl logs and exec and an
// approved serving certificate. No external network, no secrets.
func TestQ09Tailscale(t *testing.T) {
	keyFile := mustEnv("QEMU_LANE_HEADSCALE_KEY_FILE")
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	tailscaleNode(t, "q09", strings.TrimSpace(string(key)), mustEnv("QEMU_LANE_HEADSCALE_URL"), true)
}

// TestQ11TailscaleSaaS is Q09 against a tailnet of your own: a pre-authorized,
// ephemeral auth key (SCARPENTER_QEMU_TAILSCALE_AUTHKEY, and
// SCARPENTER_QEMU_TAILSCALE_LOGIN_SERVER for a Headscale). The control plane of the
// lane is not on that tailnet, so only the node's side is checked. It skips without
// a key; the scenario never prints the key.
func TestQ11TailscaleSaaS(t *testing.T) {
	key := os.Getenv("SCARPENTER_QEMU_TAILSCALE_AUTHKEY")
	if key == "" {
		t.Skip("SCARPENTER_QEMU_TAILSCALE_AUTHKEY is not set: this scenario needs a pre-authorized, ephemeral " +
			"auth key of a tailnet the VMs can reach (Q09 proves the template against the lane's Headscale)")
	}
	tailscaleNode(t, "q11", key, os.Getenv("SCARPENTER_QEMU_TAILSCALE_LOGIN_SERVER"), false)
}

// tailscaleNode boots one node of the tailscale template and checks it; with
// overTailnet the control plane is on the tailnet too and the cluster's own
// commands must work over it.
func tailscaleNode(t *testing.T, name, key, loginServer string, overTailnet bool) {
	t.Helper()
	start(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-auth-key"}, StringData: map[string]string{"key": key}}
	if err := k8s.Create(ctx(), secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx(), secret) })
	createNodeClass(t, name, func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
			Version: "v1.98.10", LoginServer: loginServer,
			AuthKeySecretRef: v1alpha1.SecretKeyRef{Namespace: ns, Name: name + "-auth-key", Key: "key"},
		}}
	})
	createNodePool(t, name, name)
	createPod(t, name+"-probe", qemuOnly)
	node, claim := waitReadyNode(t, name, bootTimeout)
	nodeAddr := nodeIP(node)
	if !strings.HasPrefix(nodeAddr, "100.") {
		t.Errorf("the node's address %s is not a tailscale address (100.64.0.0/10)", nodeAddr)
	}
	addr := vmAddress(claim.Name)
	if addr == "" {
		t.Fatal("the VM has no address on the bridge")
	}
	sh := func(cmd string) string { return mustSSH(t, addr, cmd) }
	if got := sh("systemctl is-active tailscaled scarpenter-tailscale-up.service kubelet"); got != "active\nactive\nactive" {
		t.Errorf("tailscaled, the join unit and the kubelet: %q", got)
	}
	if got := sh("tailscale ip -4"); got != nodeAddr {
		t.Errorf("tailscale's address %s is not the node's %s", got, nodeAddr)
	}
	if got := sh("systemctl show kubelet.service -p After -p Requires --value"); !strings.Contains(got, "scarpenter-tailscale-up.service") {
		t.Errorf("the kubelet is not ordered after and bound to the tailscale unit: %q", got)
	}
	if got := sh("tr '\\0' ' ' </proc/$(pgrep -x kubelet)/cmdline"); !strings.Contains(got, "--node-ip="+nodeAddr) {
		t.Errorf("the kubelet's --node-ip is not the tailscale address: %q", got)
	}
	if !overTailnet {
		return
	}
	eventually(t, 10*time.Minute, 3*time.Second, "the Pod running", func() (bool, string) {
		pod := &corev1.Pod{}
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: name + "-probe"}, pod); err != nil {
			return false, err.Error()
		}
		return pod.Status.Phase == corev1.PodRunning, string(pod.Status.Phase)
	})
	// The API server reaches the kubelet at its tailscale address.
	eventually(t, 3*time.Minute, 3*time.Second, "kubectl logs over the tailnet", func() (bool, string) {
		out, err := kubectl(t, "logs", name+"-probe")
		return err == nil && strings.Contains(out, "up"), fmt.Sprintf("%v: %s", err, out)
	})
	eventually(t, 3*time.Minute, 3*time.Second, "kubectl exec over the tailnet", func() (bool, string) {
		out, err := kubectl(t, "exec", name+"-probe", "--", "echo", "hello")
		return err == nil && out == "hello", fmt.Sprintf("%v: %s", err, out)
	})
	servingApproved(t, node.Name)
	tokenGone(t, claim.Name)
}

// TestQ10TailscaleTemplateWithoutATailnet boots the tailscale template with a dummy
// auth key and a control server nobody listens on: the sysext is merged and
// tailscaled runs (the sysext's Upholds= and the template's units work), the join
// unit cannot finish and the kubelet, ordered after it and requiring it, never
// starts, so the node never registers. What needs a tailnet is Q09.
func TestQ10TailscaleTemplateWithoutATailnet(t *testing.T) {
	start(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "q10-auth-key"},
		StringData: map[string]string{"key": "tskey-auth-EXAMPLEEXAMPLE-0123456789"}}
	if err := k8s.Create(ctx(), secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx(), secret) })
	createNodeClass(t, "q10", func(nc *v1alpha1.SCNodeClass) {
		nc.Spec.Network = v1alpha1.NetworkSpec{Template: v1alpha1.NetworkTemplateTailscale, Tailscale: &v1alpha1.TailscaleSpec{
			Version: "v1.98.10", LoginServer: "http://" + laneHost(t) + ":9",
			AuthKeySecretRef: v1alpha1.SecretKeyRef{Namespace: ns, Name: "q10-auth-key", Key: "key"},
		}}
	})
	createNodePool(t, "q10", "q10")
	createPod(t, "q10-probe", qemuOnly)

	var claim, ip string
	eventually(t, 5*time.Minute, 2*time.Second, "the VM reachable by ssh", func() (bool, string) {
		for _, rec := range instanceRecords() {
			if rec.NodeName == "" {
				continue
			}
			claim = rec.NodeName
			if ip = vmAddress(claim); ip == "" {
				return false, "no address yet"
			}
			out, err := ssh(t, ip, "hostname")
			return err == nil && out == claim, fmt.Sprintf("%v %s", err, out)
		}
		return false, "no VM yet"
	})
	sh := func(cmd string) string { return mustSSH(t, ip, cmd) }
	eventually(t, 2*time.Minute, 2*time.Second, "the join unit running", func() (bool, string) {
		out, _ := ssh(t, ip, "systemctl is-active scarpenter-tailscale-up.service")
		return out == "activating", out
	})
	if got := sh("systemd-sysext status 2>&1 | grep -c tailscale || true"); got == "0" {
		t.Errorf("the tailscale sysext is not merged:\n%s", sh("systemd-sysext status 2>&1"))
	}
	if got := sh("systemctl is-active tailscaled"); got != "active" {
		t.Errorf("tailscaled is %q", got)
	}
	if got := sh("systemctl show tailscaled.service -p UpheldBy --value"); got == "" {
		t.Errorf("nothing upholds tailscaled.service")
	}
	if got, err := ssh(t, ip, "systemctl is-active kubelet"); err == nil || got == "active" {
		t.Errorf("the kubelet is %q before the tailnet is joined", got)
	}
	unit := sh("systemctl show kubelet.service -p After -p Requires --value")
	if !strings.Contains(unit, "scarpenter-tailscale-up.service") {
		t.Errorf("the kubelet is not ordered after and bound to the join unit: %q", unit)
	}
	if got := sh("sudo stat -c %a /etc/scarpenter/tailscale-authkey"); got != "600" {
		t.Errorf("the auth key file has the mode %s", got)
	}
	var nodes corev1.NodeList
	if err := k8s.List(ctx(), &nodes, client.MatchingLabels{karpv1.NodePoolLabelKey: "q10"}); err != nil || len(nodes.Items) != 0 {
		t.Errorf("a node registered without a tailnet: %d nodes, %v", len(nodes.Items), err)
	}
}

// laneHost is the bridge address of the control plane, from the API endpoint.
func laneHost(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(lane.api)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

// vmClaim returns the NodeClaim name of the only VM of the pool.
func vmClaim(t *testing.T, pool string) string {
	t.Helper()
	for _, rec := range instanceRecords() {
		if strings.HasPrefix(rec.NodeName, pool+"-") {
			return rec.NodeName
		}
	}
	return ""
}

// tail returns the last n lines of the console, without escape sequences.
func tail(log string, n int) string {
	log = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`).ReplaceAllString(log, "")
	lines := strings.Split(strings.TrimSpace(log), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], " | ")
}

var mirrorTime = regexp.MustCompile(`\[(\d{2}/\w{3}/\d{4} \d{2}:\d{2}:\d{2})\]`)

// mirrorRequestTimes returns the times of the protocol errors the mirror logged
// for the VM's https requests to its plain http port.
func mirrorRequestTimes(log string) []time.Time {
	var out []time.Time
	for line := range strings.SplitSeq(log, "\n") {
		if !strings.Contains(line, "Bad request") && !strings.Contains(line, "Bad HTTP") {
			continue
		}
		if m := mirrorTime.FindStringSubmatch(line); m != nil {
			if ts, err := time.ParseInLocation("02/Jan/2006 15:04:05", m[1], time.Local); err == nil {
				out = append(out, ts)
			}
		}
	}
	return out
}
