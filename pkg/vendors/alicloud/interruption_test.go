/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// metadataFixture answers like ECS's metadata service: a token on PUT, and the
// termination time (status and body as given) for a GET with that token.
func metadataFixture(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" &&
			r.Header.Get("X-aliyun-ecs-metadata-token-ttl-seconds") != "":
			_, _ = w.Write([]byte("tok123"))
		case r.URL.Path == "/latest/meta-data/instance/spot/termination-time" &&
			r.Header.Get("X-aliyun-ecs-metadata-token") == "tok123":
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// runCheck runs the driver's check with sh against the fixture and returns its
// output and exit status.
func runCheck(t *testing.T, addr string) (string, int) {
	t.Helper()
	script := strings.ReplaceAll(New().InterruptionNotices().Script, "100.100.100.200", addr)
	out, err := exec.Command("sh", "-c", "scarpenter_check() {\n"+script+"\n}\nscarpenter_check").Output()
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

func TestNoticeCheckAgainstTheMetadataService(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
		code   int
	}{
		{"no notice", 404, "404 Not Found", "", 1},
		{"a notice", 200, "2026-11-23T10:15:00Z", "SpotInterrupted 2026-11-23T10:15:00Z\n", 0},
		{"a notice without seconds", 200, "2026-11-23T10:15Z", "SpotInterrupted 2026-11-23T10:15:00Z\n", 0},
		{"a server error", 500, "boom", "", 2},
		{"a refused token", 401, "", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runCheck(t, metadataFixture(t, tc.status, tc.body))
			if out != tc.want || code != tc.code {
				t.Errorf("check = %q, %d; want %q, %d", out, code, tc.want, tc.code)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		addr := strings.TrimPrefix(srv.URL, "http://")
		srv.Close()
		if out, code := runCheck(t, addr); out != "" || code != 2 {
			t.Errorf("check = %q, %d; want a transient error", out, code)
		}
	})
}

func TestInterruptionNoticesDeclaration(t *testing.T) {
	c := New().InterruptionNotices()
	if c.Interval.Seconds() != 5 || c.MaxNotice.Minutes() != 5 || c.MinNotice <= 0 || c.MinNotice > c.MaxNotice {
		t.Errorf("check = %+v", c)
	}
	if _, err := vendors.InterruptionAgentScript(c); err != nil {
		t.Error(err)
	}
	if w := vendors.NoticeWindowOf(New()); w != c.NoticeWindow {
		t.Errorf("window = %+v", w)
	}
}

func TestGoneReasonIsRecyclingOfASpotInstance(t *testing.T) {
	locked := func(spot string, reasons ...string) *ecs.DescribeInstancesResponseBodyInstancesInstance {
		in := &ecs.DescribeInstancesResponseBodyInstancesInstance{
			InstanceId: new("i-1"), Status: new("Running"), SpotStrategy: new(spot),
			OperationLocks: &ecs.DescribeInstancesResponseBodyInstancesInstanceOperationLocks{},
		}
		for _, r := range reasons {
			in.OperationLocks.LockReason = append(in.OperationLocks.LockReason,
				&ecs.DescribeInstancesResponseBodyInstancesInstanceOperationLocksLockReason{LockReason: new(r)})
		}
		return in
	}
	for _, tc := range []struct {
		name string
		in   *ecs.DescribeInstancesResponseBodyInstancesInstance
		want string
	}{
		{"spot, recycling", locked("SpotAsPriceGo", "Recycling"), "SpotInterrupted"},
		{"spot, other lock first", locked("SpotAsPriceGo", "security", "Recycling"), "SpotInterrupted"},
		{"spot, security lock", locked("SpotAsPriceGo", "security"), ""},
		{"spot, no lock", locked("SpotAsPriceGo"), ""},
		{"on-demand, recycling", locked("NoSpot", "Recycling"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toInstance("eu-central-1", tc.in).GoneReason; got != tc.want {
				t.Errorf("GoneReason = %q, want %q", got, tc.want)
			}
		})
	}
	if got := toInstance("eu-central-1", &ecs.DescribeInstancesResponseBodyInstancesInstance{}).GoneReason; got != "" {
		t.Errorf("an instance without locks: %q", got)
	}
}
