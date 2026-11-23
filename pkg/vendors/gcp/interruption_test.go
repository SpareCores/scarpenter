/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// runCheck runs the driver's notice check against a stand-in of the metadata
// server and returns what it printed and its exit status.
func runCheck(t *testing.T, d *Driver, handler http.HandlerFunc) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	script := strings.ReplaceAll(d.InterruptionNotices().Script, "http://169.254.169.254", srv.URL)
	out, err := exec.Command("sh", "-c", "scarpenter_check() {\n"+script+"\n}\nscarpenter_check").Output()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimSpace(string(out)), 0
	case errors.As(err, &ee):
		return strings.TrimSpace(string(out)), ee.ExitCode()
	}
	t.Fatal(err)
	return "", 0
}

// metadata answers instance/preempted with the value, for requests that carry
// the header; a change-wait is answered at once.
func metadata(value string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Path != "/computeMetadata/v1/instance/preempted" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", "abc")
		_, _ = w.Write([]byte(value))
	}
}

func TestNoticeCheck(t *testing.T) {
	d := New()
	if out, rc := runCheck(t, d, metadata("FALSE")); rc != 1 || out != "" {
		t.Errorf("no preemption: %q, %d; want nothing and 1", out, rc)
	}
	if out, rc := runCheck(t, d, metadata("TRUE")); rc != 0 || out != "SpotInterrupted" {
		t.Errorf("preempted without a notice: %q, %d; want SpotInterrupted and 0", out, rc)
	}
	if _, rc := runCheck(t, d, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }); rc != 2 {
		t.Errorf("a failing metadata server: %d, want 2", rc)
	}
	if _, rc := runCheck(t, d, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }); rc != 2 {
		t.Errorf("a refused request: %d, want 2", rc)
	}
}

func TestNoticeCheckDeadline(t *testing.T) {
	d := New(WithPreemptionNotice(90 * time.Second))
	out, rc := runCheck(t, d, metadata("TRUE"))
	reason, deadline, _ := strings.Cut(out, " ")
	if rc != 0 || reason != "SpotInterrupted" {
		t.Fatalf("got %q, %d", out, rc)
	}
	at, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		t.Fatalf("the deadline %q: %v", deadline, err)
	}
	if left := time.Until(at); left < 80*time.Second || left > 90*time.Second {
		t.Errorf("the deadline is %s ahead, want about 90s", left)
	}
}

func TestInterruptionNoticesWindow(t *testing.T) {
	for _, tc := range []struct {
		notice, max time.Duration
	}{{0, 30 * time.Second}, {120 * time.Second, 150 * time.Second}} {
		d := New(WithPreemptionNotice(tc.notice))
		c := d.InterruptionNotices()
		if c.MinNotice != 30*time.Second || c.MaxNotice != tc.max {
			t.Errorf("notice %s: window %+v, want 30s to %s", tc.notice, c.NoticeWindow, tc.max)
		}
		if w := vendors.NoticeWindowOf(d); w != c.NoticeWindow {
			t.Errorf("NoticeWindowOf = %+v, want %+v", w, c.NoticeWindow)
		}
		if _, err := vendors.InterruptionAgentScript(c); err != nil {
			t.Errorf("notice %s: %v", tc.notice, err)
		}
	}
}

// The agent's size is part of the user-data budget (plan 26 section 2).
func TestNoticeAgentScriptSize(t *testing.T) {
	script, err := vendors.InterruptionAgentScript(New(WithPreemptionNotice(120 * time.Second)).InterruptionNotices())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(script); n > 1536 {
		t.Errorf("the GCP agent script is %d bytes, over 1.5 KiB", n)
	} else {
		t.Logf("the GCP agent script is %d bytes", n)
	}
}

func TestGoneReason(t *testing.T) {
	for _, tc := range []struct{ capacity, state, want string }{
		{vendors.CapacitySpot, vendors.StateTerminating, "SpotInterrupted"},
		{vendors.CapacitySpot, vendors.StateRunning, ""},
		{vendors.CapacityOnDemand, vendors.StateTerminating, ""},
	} {
		if got := goneReasonOf(tc.capacity, tc.state); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.capacity, tc.state, got, tc.want)
		}
	}
}

func TestSpotPreemptionNoticeAtLaunch(t *testing.T) {
	s := baseSpec()
	s.Spot, s.CapacityType = true, vendors.CapacitySpot
	req, err := BuildInsert(s)
	if err != nil {
		t.Fatal(err)
	}
	if req.GetInstanceResource().GetScheduling().PreemptionNoticeDuration != nil {
		t.Error("a notice is set without being asked for")
	}
	s.PreemptionNotice = 120 * time.Second
	req, _ = BuildInsert(s)
	if got := req.GetInstanceResource().GetScheduling().GetPreemptionNoticeDuration().GetSeconds(); got != 120 {
		t.Errorf("the notice is %d s, want 120", got)
	}
	s.Spot = false
	req, _ = BuildInsert(s)
	if req.GetInstanceResource().GetScheduling().PreemptionNoticeDuration != nil {
		t.Error("an on-demand VM asks for a preemption notice")
	}
}
