//go:build qemu && scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package e2e holds the scenarios of the L4 lane (docs/plans/v1/wip/
// 00-overview.md section 13.3). test/qemu/run.sh builds what they run in: a
// bridge with DHCP and NAT, a k3s control plane, flannel and kube-proxy, a
// sysext mirror. The scenarios start SCarpenter (a scarpenter_dev build with the
// qemu driver) themselves, because several of them restart it, and look into
// the VMs with ssh.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/qemu"

	// Registers Karpenter core's API types in client-go's scheme.
	_ "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	ns          = "scarpenter-system"
	clusterName = "scarpenter-qemu"
	flatcarVer  = "4757.2.3"
)

// lane is what run.sh passes in the environment.
var lane struct {
	adminKubeconfig, controllerKubeconfig, binary, api, bridge, sysextBase, k8sVersion string
	kubeletClientCA, stateDir, cacheDir, serialDir, sshKey, out                        string
}

var (
	k8s       client.Client
	clientset kubernetes.Interface
)

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "%s is not set: run the lane with hack/dev.sh --privileged make test-qemu\n", name)
		os.Exit(1)
	}
	return v
}

func TestMain(m *testing.M) {
	lane.adminKubeconfig = mustEnv("QEMU_LANE_ADMIN_KUBECONFIG")
	lane.controllerKubeconfig = mustEnv("QEMU_LANE_CONTROLLER_KUBECONFIG")
	lane.binary = mustEnv("QEMU_LANE_BINARY")
	lane.api = mustEnv("QEMU_LANE_API")
	lane.bridge = mustEnv("QEMU_LANE_BRIDGE")
	lane.sysextBase = mustEnv("QEMU_LANE_SYSEXT_BASE")
	lane.k8sVersion = mustEnv("QEMU_LANE_KUBERNETES_VERSION")
	lane.kubeletClientCA = mustEnv("QEMU_LANE_KUBELET_CLIENT_CA")
	lane.stateDir = mustEnv("QEMU_LANE_STATE_DIR")
	lane.cacheDir = mustEnv("QEMU_LANE_CACHE_DIR")
	lane.serialDir = mustEnv("QEMU_LANE_SERIAL_DIR")
	lane.sshKey = mustEnv("QEMU_LANE_SSH_KEY")
	lane.out = mustEnv("QEMU_LANE_OUT")

	if err := v1alpha1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg, err := clientConfig(lane.adminKubeconfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubeconfig:", err)
		os.Exit(1)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme.Scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	stopController()
	os.Exit(code)
}

func clientConfig(path string) (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	cfg.QPS, cfg.Burst = 50, 100
	return cfg, nil
}

func ctx() context.Context { return context.Background() }

// eventually polls cond until it returns true; on timeout the test fails with
// the last explanation cond gave. It returns how long it waited.
func eventually(t *testing.T, timeout, interval time.Duration, what string, cond func() (bool, string)) time.Duration {
	t.Helper()
	start := time.Now()
	last := ""
	for {
		ok, why := cond()
		if ok {
			d := time.Since(start)
			t.Logf("%s (%s)", what, d.Round(100*time.Millisecond))
			return d
		}
		last = why
		if time.Since(start) > timeout {
			t.Fatalf("timed out after %s waiting for: %s; last state: %s", timeout, what, last)
		}
		time.Sleep(interval)
	}
}

// ----------------------------------------------------------- controller ----

var (
	ctlMu   sync.Mutex
	ctlCmd  *exec.Cmd
	ctlN    int
	ctlDone chan struct{}
	// ctlClientCA says whether the controller gets --kubelet-client-ca-file: the
	// lane's k3s needs it for kubectl logs and exec.
	ctlClientCA = true
	// ctlExtra are more flags of the controller (the launch delay of Q04).
	ctlExtra []string
)

// startController starts SCarpenter out of cluster, authenticating as its
// ServiceAccount, unless it already runs.
func startController(t *testing.T) {
	t.Helper()
	ctlMu.Lock()
	defer ctlMu.Unlock()
	if ctlCmd != nil {
		return
	}
	ctlN++
	logFile, err := os.Create(filepath.Join(lane.out, "logs", fmt.Sprintf("controller-%d.log", ctlN)))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--cluster-name=" + clusterName,
		"--api-server-endpoint=" + lane.api,
		"--vendors=qemu",
		"--sysext-base-url=" + lane.sysextBase,
		"--qemu-state-dir=" + lane.stateDir,
		"--qemu-cache-dir=" + lane.cacheDir,
		"--qemu-log-dir=" + lane.serialDir,
		"--qemu-bridge=" + lane.bridge,
		// qemu has no Keeper catalog: the default URL is never read.
		"--keeper-url=http://127.0.0.1:1",
		"--gc-interval=15s", "--gc-grace-period=1m",
		"--resource-check-interval=10s",
		"--batch-max-duration=5s", "--batch-idle-duration=500ms",
		"--log-level=debug",
		// The lane's k3s runs without CoreDNS, so there is no DNS Service to find
		// (the controller reads it itself when --cluster-dns is not set).
		"--cluster-dns=none",
	}
	if ctlClientCA {
		args = append(args, "--kubelet-client-ca-file="+lane.kubeletClientCA)
	}
	cmd := exec.Command(lane.binary, append(args, ctlExtra...)...)
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+lane.controllerKubeconfig,
		"POD_NAMESPACE="+ns, "LEADER_ELECTION_NAMESPACE="+ns, "DISABLE_LEADER_ELECTION=true",
		"MEMORY_LIMIT=4294967296", "CPU_REQUESTS=1000",
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctlCmd, ctlDone = cmd, make(chan struct{})
	done := ctlDone
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
		close(done)
	}()
	t.Logf("started SCarpenter (pid %d, log controller-%d.log)", cmd.Process.Pid, ctlN)
}

// restartController restarts SCarpenter with or without the kubelet client CA
// and with more flags.
func restartController(t *testing.T, clientCA bool, extra ...string) {
	t.Helper()
	stopController()
	ctlMu.Lock()
	ctlClientCA, ctlExtra = clientCA, extra
	ctlMu.Unlock()
	startController(t)
}

// stopController ends SCarpenter with SIGKILL, like a crash.
func stopController() {
	ctlMu.Lock()
	defer ctlMu.Unlock()
	if ctlCmd == nil {
		return
	}
	_ = ctlCmd.Process.Signal(syscall.SIGKILL)
	<-ctlDone
	ctlCmd = nil
}

// ---------------------------------------------------------------- tools ----

func run(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// kubectl runs kubectl as the cluster admin.
func kubectl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return run(t, "kubectl", append([]string{"--kubeconfig", lane.adminKubeconfig}, args...)...)
}

// ssh runs a command on a VM as the core user.
func ssh(t *testing.T, ip, command string) (string, error) {
	t.Helper()
	return run(t, "ssh", "-i", lane.sshKey, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5", "-o", "BatchMode=yes", "-o", "LogLevel=ERROR", "core@"+ip, command)
}

// mustSSH fails the test when the command fails.
func mustSSH(t *testing.T, ip, command string) string {
	t.Helper()
	out, err := ssh(t, ip, command)
	if err != nil {
		t.Fatalf("ssh %s %q: %v\n%s", ip, command, err, out)
	}
	return out
}

// prefetchImage makes the driver download the release before the controller
// needs it (a launch must not wait for 600 MB), with the driver's own code: the
// download, the digest check and the decompression are part of what the lane
// proves.
func prefetchImage(t *testing.T) {
	t.Helper()
	// Its own state directory: Init removes the unfinished launches of a state
	// directory, which must not be the running controller's.
	d := qemu.New(qemu.Options{StateDir: t.TempDir(), CacheDir: lane.cacheDir, Bridge: lane.bridge})
	if err := d.Init(ctx(), vendors.Config{Log: logr.Discard()}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	img, err := d.ResolveImage(ctx(), vendors.ImageRequest{Region: qemu.Region, Arch: "amd64", Channel: "stable", Version: flatcarVer})
	if err != nil {
		t.Fatalf("resolving the Flatcar image: %v", err)
	}
	t.Logf("Flatcar %s image %s ready in %s (image disk %d bytes)", flatcarVer, img.Ref, time.Since(start).Round(time.Second), img.RootDiskBytes)
}
