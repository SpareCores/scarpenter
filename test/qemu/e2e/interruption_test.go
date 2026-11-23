//go:build qemu && scarpenter_dev

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package e2e

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
)

// The interruption scenarios (plan 26 step 4) need no download beyond the other
// scenarios' (the same Flatcar image and sysext), so they run in the default set.

// noticeServer is the notice source of the qemu driver's node agent: what a
// vendor's instance metadata is on a real cloud. It answers an empty body until a
// scenario sets a notice for the node.
type noticeServer struct {
	url     string
	mu      sync.Mutex
	notices map[string]string
}

func startNoticeServer(t *testing.T) *noticeServer {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(laneHost(t), "0"))
	if err != nil {
		t.Fatal(err)
	}
	s := &noticeServer{url: "http://" + l.Addr().String() + "/notice", notices: map[string]string{}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, _ = fmt.Fprint(w, s.notices[strings.TrimPrefix(r.URL.Path, "/notice/")])
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

// interrupt makes the notice server announce the loss of the node at the deadline.
func (s *noticeServer) interrupt(node string, deadline time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices[node] = v1alpha1.ReasonSpotInterrupted + " " + deadline.UTC().Format(time.RFC3339)
}

// withNoticeServer restarts SCarpenter with the notice server's URL, so that its
// spot nodes get the agent, and puts the controller back to its defaults at the end.
func withNoticeServer(t *testing.T) *noticeServer {
	t.Helper()
	prefetchImage(t)
	s := startNoticeServer(t)
	restartController(t, true, "--qemu-notice-url="+s.url)
	t.Cleanup(func() {
		stopController()
		ctlMu.Lock()
		ctlExtra = nil
		ctlMu.Unlock()
	})
	return s
}

// spotPool creates a NodeClass and a NodePool that launch the qemu spot type.
func spotPool(t *testing.T, name string) {
	t.Helper()
	createNodeClass(t, name)
	createNodePool(t, name, name,
		req(karpv1.CapacityTypeLabelKey, karpv1.CapacityTypeSpot), req(corev1.LabelInstanceTypeStable, "qemu.spot"))
}

// logLine is a line of a Pod's log with the time it arrived.
type logLine struct {
	at   time.Time
	text string
}

// logFollower follows a Pod's log until the Pod's container ends.
type logFollower struct {
	mu    sync.Mutex
	lines []logLine
}

func followLogs(t *testing.T, pod string) *logFollower {
	t.Helper()
	f := &logFollower{}
	stream, err := clientset.CoreV1().Pods("default").GetLogs(pod, &corev1.PodLogOptions{Follow: true}).Stream(ctx())
	if err != nil {
		t.Fatalf("following the log of %s: %v", pod, err)
	}
	go func() {
		defer func() { _ = stream.Close() }()
		sc := bufio.NewScanner(stream)
		for sc.Scan() {
			f.mu.Lock()
			f.lines = append(f.lines, logLine{time.Now(), sc.Text()})
			f.mu.Unlock()
		}
	}()
	return f
}

// seen returns when the line arrived, if it did.
func (f *logFollower) seen(text string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.lines {
		if l.text == text {
			return l.at, true
		}
	}
	return time.Time{}, false
}

const sigtermLine = "got SIGTERM"

// sigtermPod runs a Pod that says when it gets SIGTERM and then exits, with a
// grace period like the first workload's (benchmarks flush in about 5 s).
func sigtermPod(t *testing.T, name string) *logFollower {
	t.Helper()
	pod := podSpec(name, qemuOnly)
	pod.Spec.TerminationGracePeriodSeconds = new(int64(20))
	pod.Spec.Containers[0].Command = []string{"sh", "-c", `trap 'echo ` + sigtermLine + `; exit 0' TERM; echo up; while :; do sleep 1; done`}
	if err := k8s.Create(ctx(), pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx(), pod) })
	eventually(t, 10*time.Minute, 3*time.Second, "the Pod "+name+" running", func() (bool, string) {
		got := &corev1.Pod{}
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: "default", Name: name}, got); err != nil {
			return false, err.Error()
		}
		return got.Status.Phase == corev1.PodRunning, fmt.Sprintf("phase %s", got.Status.Phase)
	})
	return followLogs(t, name)
}

// checkShutdownSettings looks at what a spot node has to survive a power-off
// without a notice: the kubelet's graceful shutdown settings, and a delay lock of
// the kubelet that logind honours.
func checkShutdownSettings(t *testing.T, ip string) {
	t.Helper()
	cfg := mustSSH(t, ip, "grep -E '^shutdownGracePeriod' /var/lib/kubelet/config.yaml")
	for _, want := range []string{"shutdownGracePeriod: 25s", "shutdownGracePeriodCriticalPods: 5s"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the kubelet configuration lacks %q:\n%s", want, cfg)
		}
	}
	eventually(t, 2*time.Minute, 3*time.Second, "the kubelet holding a shutdown delay lock", func() (bool, string) {
		out, err := ssh(t, ip, "sudo systemd-inhibit --list --no-pager")
		return err == nil && strings.Contains(out, "delay") && strings.Contains(out, "kubelet"), out
	})
}

// TestQ16InterruptionNoticeDrainsTheNode: the node agent of a spot node polls the
// notice source, and on a notice annotates its own Node with the kubelet's
// credentials; the interruption controller then marks the NodeClaim, tightens its
// termination deadline and deletes it, and the Pod gets its SIGTERM long before
// the deadline. NodeRestriction lets the kubelet's identity annotate its own Node
// and no other.
func TestQ16InterruptionNoticeDrainsTheNode(t *testing.T) {
	notices := withNoticeServer(t)
	spotPool(t, "q16")
	logs := sigtermPod(t, "q16-probe")
	node, claim := waitReadyNode(t, "q16", bootTimeout)
	ip := nodeIP(node)
	if got := claim.Labels[karpv1.CapacityTypeLabelKey]; got != karpv1.CapacityTypeSpot {
		t.Fatalf("the capacity type is %q", got)
	}

	// The agent runs in the metadata slice, with the kubelet's certificate at hand.
	if got := mustSSH(t, ip, "systemctl show scarpenter-interruption.service -p Slice --value"); got != "scarpenter-metadata.slice" {
		t.Errorf("the agent's slice is %q", got)
	}
	if got := mustSSH(t, ip, "systemctl is-active scarpenter-interruption.service"); got != "active" {
		t.Errorf("the agent is %q", got)
	}
	cgroup := mustSSH(t, ip, "cat /proc/$(systemctl show scarpenter-interruption.service -p MainPID --value)/cgroup")
	if !strings.Contains(cgroup, "scarpenter.slice/scarpenter-metadata.slice") {
		t.Errorf("the agent's cgroup is %q", cgroup)
	}
	checkShutdownSettings(t, ip)

	// NodeRestriction: the kubelet's identity annotates its own Node, not another.
	other := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "q16-bystander"}}
	if err := k8s.Create(ctx(), other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx(), other) })
	patch := func(name string) string {
		return mustSSH(t, ip, fmt.Sprintf("sudo curl -s -o /dev/null -w '%%{http_code}' --cacert /etc/kubernetes/pki/ca.crt "+
			"--cert /var/lib/kubelet/pki/kubelet-client-current.pem -X PATCH -H 'Content-Type: application/merge-patch+json' "+
			"%s/api/v1/nodes/%s -d '{\"metadata\":{\"annotations\":{\"q16\":\"probe\"}}}'", lane.api, name))
	}
	if got := patch(node.Name); got != "200" {
		t.Errorf("the kubelet's credential patching its own Node: HTTP %s, want 200", got)
	}
	if got := patch(other.Name); got != "403" {
		t.Errorf("the kubelet's credential patching another Node: HTTP %s, want 403", got)
	}

	// The notice: 60 s ahead.
	deadline := time.Now().Add(60 * time.Second).Truncate(time.Second)
	noticed := time.Now()
	notices.interrupt(node.Name, deadline)

	var annotated *corev1.Node
	eventually(t, time.Minute, 500*time.Millisecond, "the agent annotating the Node", func() (bool, string) {
		annotated = getNode(t, node.Name)
		return annotated.Annotations[v1alpha1.AnnotationInterruption] != "", fmt.Sprintf("%v", annotated.Annotations)
	})
	annotatedAfter := time.Since(noticed)
	if got := annotated.Annotations[v1alpha1.AnnotationInterruptionSource]; got != "instance-metadata" {
		t.Errorf("the notice's source is %q, want instance-metadata (the agent)", got)
	}
	if got := annotated.Annotations[v1alpha1.AnnotationInterruption]; got != v1alpha1.ReasonSpotInterrupted {
		t.Errorf("the notice's reason is %q", got)
	}

	var got karpv1.NodeClaim
	eventually(t, time.Minute, 250*time.Millisecond, "the NodeClaim interrupted and deleting", func() (bool, string) {
		if err := k8s.Get(ctx(), client.ObjectKey{Name: claim.Name}, &got); err != nil {
			return false, err.Error()
		}
		cond := got.StatusConditions().Get(v1alpha1.ConditionInterrupted)
		return cond.IsTrue() && !got.DeletionTimestamp.IsZero(), fmt.Sprintf("condition %v, deleting %v", cond, got.DeletionTimestamp)
	})
	deletingAfter := time.Since(noticed)
	if r := got.StatusConditions().Get(v1alpha1.ConditionInterrupted).Reason; r != v1alpha1.ReasonSpotInterrupted {
		t.Errorf("the Interrupted condition's reason is %q", r)
	}
	// The termination deadline is the notice's deadline less the margin (5 s).
	stamp, err := time.Parse(time.RFC3339, got.Annotations[karpv1.NodeClaimTerminationTimestampAnnotationKey])
	if err != nil {
		t.Fatalf("the termination timestamp %q: %v", got.Annotations[karpv1.NodeClaimTerminationTimestampAnnotationKey], err)
	}
	if d := stamp.Sub(deadline.Add(-5 * time.Second)); d < -time.Second || d > time.Second {
		t.Errorf("the termination timestamp is %s, want about %s", stamp, deadline.Add(-5*time.Second))
	}

	eventually(t, time.Minute, 500*time.Millisecond, "the Pod's SIGTERM", func() (bool, string) {
		_, ok := logs.seen(sigtermLine)
		return ok, "no SIGTERM yet"
	})
	at, _ := logs.seen(sigtermLine)
	if at.After(deadline) {
		t.Errorf("the Pod got its SIGTERM at %s, after the deadline %s", at, deadline)
	}
	report(t, "Q16 interruption: agent annotated the Node %s after the notice, NodeClaim deleting after %s, Pod SIGTERM after %s (%s before the deadline)",
		annotatedAfter.Round(100*time.Millisecond), deletingAfter.Round(100*time.Millisecond),
		at.Sub(noticed).Round(100*time.Millisecond), deadline.Sub(at).Round(time.Second))

	eventually(t, 5*time.Minute, 3*time.Second, "the Node, the NodeClaim and the VM gone", func() (bool, string) {
		nodeErr := k8s.Get(ctx(), client.ObjectKey{Name: node.Name}, &corev1.Node{})
		claimErr := k8s.Get(ctx(), client.ObjectKey{Name: claim.Name}, &karpv1.NodeClaim{})
		return nodeErr != nil && claimErr != nil && vmDir(claim.Name) == "", fmt.Sprintf("node: %v claim: %v", nodeErr, claimErr)
	})
	events, err := kubectl(t, "get", "events", "-A", "-o", "jsonpath={range .items[*]}{.reason} {.involvedObject.kind}{\"\\n\"}{end}")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SpotInterrupted Node", "SpotInterrupted NodeClaim"} {
		if !strings.Contains(events, want) {
			t.Errorf("no Event %q", want)
		}
	}
}

// TestQ17GracefulShutdownWithoutANotice: a spot node that is powered off with no
// notice (the vendor's ACPI shutdown, or a notice that was missed) still gives its
// Pods their SIGTERM: the kubelet holds a shutdown delay lock of logind. The
// lane has no QEMU monitor to press the power button, so the scenario runs
// systemctl poweroff, which logind treats the same way.
func TestQ17GracefulShutdownWithoutANotice(t *testing.T) {
	start(t)
	spotPool(t, "q17")
	logs := sigtermPod(t, "q17-probe")
	node, claim := waitReadyNode(t, "q17", bootTimeout)
	ip := nodeIP(node)
	checkShutdownSettings(t, ip)
	if got, _ := ssh(t, ip, "systemctl is-enabled scarpenter-interruption.service"); got == "enabled" {
		t.Errorf("the agent runs without --qemu-notice-url")
	}

	off := time.Now()
	_, _ = ssh(t, ip, "sudo systemctl poweroff")
	eventually(t, time.Minute, 500*time.Millisecond, "the Pod's SIGTERM before the power-off", func() (bool, string) {
		_, ok := logs.seen(sigtermLine)
		return ok, "no SIGTERM yet"
	})
	at, _ := logs.seen(sigtermLine)
	report(t, "Q17 graceful shutdown: Pod SIGTERM %s after systemctl poweroff", at.Sub(off).Round(100*time.Millisecond))
	_ = claim
}
