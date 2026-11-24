/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package vendors_test

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

type noticeDriver struct {
	vendors.Driver
	check vendors.NoticeCheck
}

func (d noticeDriver) Vendor() string                           { return "x" }
func (d noticeDriver) InterruptionNotices() vendors.NoticeCheck { return d.check }
func checkWith(script string) vendors.NoticeCheck {
	return vendors.NoticeCheck{
		Script: script, Interval: time.Second,
		NoticeWindow: vendors.NoticeWindow{MinNotice: 30 * time.Second, MaxNotice: 2 * time.Minute},
	}
}

func TestInterruptionAgentFragment(t *testing.T) {
	if _, ok, err := vendors.InterruptionAgent(plainDriver{}, false); ok || err != nil {
		t.Errorf("a driver without the capability: %v, %v", ok, err)
	}
	if _, ok, err := vendors.InterruptionAgent(noticeDriver{check: checkWith("  ")}, false); ok || err != nil {
		t.Errorf("a driver with an empty check: %v, %v", ok, err)
	}
	for name, c := range map[string]vendors.NoticeCheck{
		"a template action": checkWith("echo '{{ .NodeName }}'"),
		"a negative pause":  {Script: "return 1", Interval: -1},
	} {
		if _, _, err := vendors.InterruptionAgent(noticeDriver{check: c}, false); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	d := noticeDriver{check: checkWith("  return 1")}
	plain, ok, err := vendors.InterruptionAgent(d, false)
	if !ok || err != nil {
		t.Fatalf("InterruptionAgent = %v, %v", ok, err)
	}
	restricted, _, _ := vendors.InterruptionAgent(d, true)
	for _, want := range []string{"Slice=scarpenter-metadata.slice", "scarpenter-interruption.service", "Restart=always",
		"network-online.target", "NODE_NAME={{ .NodeName }}", "API_SERVER={{ .APIServerEndpoint }}", "          return 1\n"} {
		if !strings.Contains(plain.Butane, want) {
			t.Errorf("the fragment lacks %q:\n%s", want, plain.Butane)
		}
	}
	if strings.Contains(plain.Butane, "scarpenter-metadata-firewall") {
		t.Errorf("an unrestricted node's agent waits for the firewall")
	}
	if !strings.Contains(restricted.Butane, "After=network-online.target scarpenter-metadata-firewall.service") {
		t.Errorf("a restricted node's agent does not start after the firewall:\n%s", restricted.Butane)
	}
}

func TestBootFragmentsOfAddsTheAgentOnSpotOnly(t *testing.T) {
	d := noticeDriver{check: checkWith("return 1")}
	for spot, want := range map[bool]int{false: 0, true: 1} {
		got, err := vendors.BootFragmentsOf(d, true, spot, false)
		if err != nil || len(got) != want {
			t.Errorf("spot=%v: %d fragments, %v; want %d", spot, len(got), err, want)
		}
	}
}

// The agent's size is part of the user-data budget: the generic loop plus a
// vendor's check stay under 1.5 KiB (plan 26 section 2).
func TestAgentScriptSize(t *testing.T) {
	check := `  t=$(curl -sf --max-time 3 -H 'Metadata: true' http://169.254.169.254/metadata/scheduledevents) || return 2
  echo "$t" | grep -q Preempt || return 1
  echo "SpotInterrupted $(echo "$t" | sed -n 's/.*"NotBefore": *"\([^"]*\)".*/\1/p')"`
	script, err := vendors.InterruptionAgentScript(checkWith(check))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(script); n > 1536 {
		t.Errorf("the agent script is %d bytes with a %d byte check, over 1.5 KiB", n, len(check))
	} else {
		t.Logf("the agent script is %d bytes with a %d byte check", n, len(check))
	}
}

// node is a stand-in for the API server's Node endpoint.
type node struct {
	mu        sync.Mutex
	annotated bool
	forbidden bool
	patches   chan map[string]string
}

func newNode(t *testing.T, annotated, forbidden bool) (*node, string) {
	t.Helper()
	n := &node{annotated: annotated, forbidden: forbidden, patches: make(chan map[string]string, 10)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes/node-1" {
			http.NotFound(w, r)
			return
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if n.annotated {
				_, _ = io.WriteString(w, `{"metadata":{"annotations":{"scarpenter.sparecores.com/interruption-deadline":"x"}}}`)
			} else {
				_, _ = io.WriteString(w, `{"metadata":{"name":"node-1"}}`)
			}
		case http.MethodPatch:
			if n.forbidden {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			var body struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			if r.Header.Get("Content-Type") != "application/merge-patch+json" || json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "bad patch", http.StatusBadRequest)
				return
			}
			n.annotated = true
			n.patches <- body.Metadata.Annotations
		}
	}))
	t.Cleanup(srv.Close)
	return n, srv.URL
}

// runAgent runs the agent script with the check against the fake API server and
// returns what it patched, if anything within wait.
func runAgent(t *testing.T, check vendors.NoticeCheck, annotated bool, wait time.Duration) (map[string]string, string) {
	t.Helper()
	return runAgentOn(t, check, annotated, false, wait)
}

func runAgentOn(
	t *testing.T, check vendors.NoticeCheck, annotated, forbidden bool, wait time.Duration,
) (map[string]string, string) {
	t.Helper()
	script, err := vendors.InterruptionAgentScript(check)
	if err != nil {
		t.Fatal(err)
	}
	n, url := newNode(t, annotated, forbidden)
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(cert, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent")
	// The script has the node's paths: point them at the fixture's.
	script = strings.NewReplacer("/var/lib/kubelet/pki/kubelet-client-current.pem", cert, "/etc/kubernetes/pki/ca.crt", cert).Replace(script)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", path)
	cmd.Env = append(os.Environ(), "NODE_NAME=node-1", "API_SERVER="+url, "STATE="+dir)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = processGroup()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killGroup(cmd); _ = cmd.Wait() })
	var got map[string]string
	select {
	case got = <-n.patches:
	case <-time.After(wait):
	}
	// exec writes to out until Wait returns: read it only after that.
	killGroup(cmd)
	_ = cmd.Wait()
	return got, out.String()
}

func TestAgentAnnotatesTheNode(t *testing.T) {
	const reason = "SpotInterrupted"
	const deadline = "2026-11-19T10:00:00Z"
	for name, tc := range map[string]struct{ script, wantDeadline string }{
		"a notice with a deadline": {"echo " + reason + " " + deadline, deadline},
		"a notice without one":     {"echo " + reason, ""},
		"no reason":                {"echo", ""},
		"a transient error first": {
			`if [ -e "$STATE/seen" ]; then echo ` + reason + ` ` + deadline + `; return 0; fi; : > "$STATE/seen"; return 2`, deadline},
		"a blocking check that ends in a notice": {"sleep 1; echo " + reason + " " + deadline, deadline},
	} {
		t.Run(name, func(t *testing.T) {
			check := checkWith(tc.script)
			check.Interval = 0
			got, log := runAgent(t, check, false, 20*time.Second)
			if got == nil {
				t.Fatalf("the node was not annotated; the agent said:\n%s", log)
			}
			want := map[string]string{
				"scarpenter.sparecores.com/interruption":          reason,
				"scarpenter.sparecores.com/interruption-deadline": tc.wantDeadline,
				"scarpenter.sparecores.com/interruption-source":   "instance-metadata",
			}
			if !maps.Equal(got, want) {
				t.Errorf("annotations %v, want %v", got, want)
			}
		})
	}
}

func TestAgentCleansWhatTheCheckPrints(t *testing.T) {
	got, log := runAgent(t, checkWith(`echo 'Spot"Interrupted;x 2999-01-01T00:00:00Z"}}}'`), false, 20*time.Second)
	if got == nil {
		t.Fatalf("the node was not annotated; the agent said:\n%s", log)
	}
	if got["scarpenter.sparecores.com/interruption"] != "SpotInterruptedx" ||
		got["scarpenter.sparecores.com/interruption-deadline"] != "2999-01-01T00:00:00Z" {
		t.Errorf("annotations %v", got)
	}
}

func TestAgentIsQuietWithoutANotice(t *testing.T) {
	check := checkWith("return 1")
	check.Interval = 0
	if got, _ := runAgent(t, check, false, 1500*time.Millisecond); got != nil {
		t.Errorf("annotated without a notice: %v", got)
	}
	check = checkWith("return 2")
	check.Interval = 0
	if got, log := runAgent(t, check, false, 1500*time.Millisecond); got != nil || strings.Count(log, "the notice check failed") != 1 {
		t.Errorf("a failing check: %v, log %q (want one line)", got, log)
	}
}

func TestAgentDoesNotPatchTwice(t *testing.T) {
	got, _ := runAgent(t, checkWith("echo SpotInterrupted"), true, 1500*time.Millisecond)
	if got != nil {
		t.Errorf("a node that has the annotation was patched again: %v", got)
	}
}

// A patch that is refused (NodeRestriction, RBAC) is logged once and retried with
// a backoff, not every second.
func TestAgentLogsARefusedPatchOnce(t *testing.T) {
	got, log := runAgentOn(t, checkWith("echo SpotInterrupted"), false, true, 5*time.Second)
	if got != nil {
		t.Errorf("a refused patch was applied: %v", got)
	}
	if n := strings.Count(log, "annotating the Node failed"); n != 1 {
		t.Errorf("the agent logged the failure %d times, want once:\n%s", n, log)
	}
}

func TestBootFragmentsOfPutsTheAgentIntoTheRestrictionFragment(t *testing.T) {
	d := restrictedNoticeDriver{noticeDriver{check: checkWith("return 1")}}
	frags, err := vendors.BootFragmentsOf(d, true, true, false)
	if err != nil || len(frags) != 1 || frags[0].Name != "metadata-access" {
		t.Fatalf("spot + restricted: %d fragments %v, %v; want the one metadata-access", len(frags), frags, err)
	}
	for _, want := range []string{"scarpenter-interruption.service", "scarpenter-metadata-firewall.service", "scarpenter-interruption-agent"} {
		if !strings.Contains(frags[0].Butane, want) {
			t.Errorf("the fragment lacks %q", want)
		}
	}
	if frags, _ := vendors.BootFragmentsOf(d, true, false, false); len(frags) != 1 || strings.Contains(frags[0].Butane, "interruption") {
		t.Errorf("an on-demand node has the agent")
	}
	frags, _ = vendors.BootFragmentsOf(d, false, true, false)
	if len(frags) != 1 || frags[0].Name != "interruption-agent" || strings.Contains(frags[0].Butane, "firewall") {
		t.Errorf("spot, open metadata: %v", frags)
	}
}

type restrictedNoticeDriver struct{ noticeDriver }

func (restrictedNoticeDriver) MetadataEndpoints() vendors.MetadataEndpoints {
	return vendors.MetadataEndpoints{Addresses: addrs("169.254.169.254")}
}
