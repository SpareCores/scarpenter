/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/aws"
)

// runCheck runs the driver's check function with sh against a stand-in for the
// instance metadata service and returns its output and exit status. The action
// is the service's answer for meta-data/spot/instance-action; the token request
// answers tokenStatus.
func runCheck(t *testing.T, tokenStatus, actionStatus int, action string) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" &&
			r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") != "":
			w.WriteHeader(tokenStatus)
			_, _ = fmt.Fprint(w, "tok")
		case r.Method == http.MethodGet && r.URL.Path == "/latest/meta-data/spot/instance-action" &&
			r.Header.Get("X-aws-ec2-metadata-token") == "tok":
			w.WriteHeader(actionStatus)
			_, _ = fmt.Fprint(w, action)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	body := strings.ReplaceAll((&aws.Driver{}).InterruptionNotices().Script, "169.254.169.254", strings.TrimPrefix(srv.URL, "http://"))
	out, err := exec.Command("sh", "-c", "scarpenter_check() {\n"+body+"\n}\nscarpenter_check").Output()
	if err == nil {
		return strings.TrimSpace(string(out)), 0
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), ee.ExitCode()
}

// The check against IMDSv2's answers: 404 is no notice, the document gives the
// deadline, anything else is a transient error.
func TestInterruptionCheck(t *testing.T) {
	const doc = `{"action":"terminate","time":"2026-11-23T10:02:00Z"}`
	for name, tc := range map[string]struct {
		token, status int
		body, out     string
		rc            int
	}{
		"no notice":         {200, 404, "<html>Not Found</html>", "", 1},
		"a notice":          {200, 200, doc, "SpotInterrupted 2026-11-23T10:02:00Z", 0},
		"spaced JSON":       {200, 200, `{"action": "terminate", "time": "2026-11-23T10:02:00Z"}`, "SpotInterrupted 2026-11-23T10:02:00Z", 0},
		"no time":           {200, 200, `{"action":"terminate"}`, "SpotInterrupted", 0},
		"server error":      {200, 500, "oops", "", 2},
		"unauthorized":      {200, 401, "", "", 2},
		"token unavailable": {503, 200, doc, "", 2},
	} {
		t.Run(name, func(t *testing.T) {
			out, rc := runCheck(t, tc.token, tc.status, tc.body)
			if out != tc.out || rc != tc.rc {
				t.Errorf("check = %q, %d; want %q, %d", out, rc, tc.out, tc.rc)
			}
		})
	}
}

// The notice window is the two minutes EC2 documents, polled every 2 s.
func TestInterruptionNoticesWindow(t *testing.T) {
	var d vendors.Driver = &aws.Driver{}
	n, ok := d.(vendors.InterruptionNotices)
	if !ok {
		t.Fatal("the driver has no InterruptionNotices")
	}
	c := n.InterruptionNotices()
	if c.Interval != 2*time.Second || c.MinNotice != 2*time.Minute || c.MaxNotice != 2*time.Minute {
		t.Errorf("check = %+v", c)
	}
}

// A reclaimed spot instance is gone with the reason SpotInterrupted; a running one,
// and an on-demand or a spot instance that SCarpenter ended, have none.
func TestGoneReason(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := t.Context()
	create := func(capacity string) *vendors.Instance {
		t.Helper()
		inst, err := e.d.CreateInstance(ctx, request(testRegion, testZone, "m7i.large", capacity))
		if err != nil {
			t.Fatal(err)
		}
		return inst
	}
	spotRunning, spotReclaimed, spotEnded, od := create(vendors.CapacitySpot), create(vendors.CapacitySpot), create(vendors.CapacitySpot), create(vendors.CapacityOnDemand)
	for _, tc := range []struct {
		name string
		inst *vendors.Instance
		stop func(t *testing.T)
		want string
	}{
		{"spot running", spotRunning, func(*testing.T) {}, ""},
		{"spot reclaimed", spotReclaimed, func(*testing.T) { e.acct.ReclaimSpotInstance(testRegion, spotReclaimed.Ref.ID()) },
			v1alpha1.ReasonSpotInterrupted},
		{"spot ended by SCarpenter", spotEnded, func(t *testing.T) {
			if err := e.d.DeleteInstance(ctx, spotEnded.Ref); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"on-demand terminated", od, func(*testing.T) { e.acct.SetInstanceState(testRegion, od.Ref.ID(), "terminated") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.stop(t)
			got, err := e.d.GetInstance(ctx, tc.inst.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if got.GoneReason != tc.want {
				t.Errorf("GoneReason = %q, want %q", got.GoneReason, tc.want)
			}
		})
	}
}
