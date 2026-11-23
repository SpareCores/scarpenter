/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

const (
	vmName = "claim-abc"
	// Scheduled Events as Azure documents them: pretty-printed, and compact.
	eventsPretty = `{
    "DocumentIncarnation": 2,
    "Events": [
        {
            "EventId": "A123",
            "EventStatus": "Scheduled",
            "EventType": "Preempt",
            "ResourceType": "VirtualMachine",
            "Resources": ["other-vm", "claim-abc"],
            "NotBefore": "Mon, 19 Sep 2016 18:29:47 GMT",
            "Description": "",
            "EventSource": "Platform",
            "DurationInSeconds": -1
        }
    ]
}`
	eventsCompact = `{"DocumentIncarnation":2,"Events":[{"EventId":"A1","EventStatus":"Scheduled","EventType":"Freeze",` +
		`"ResourceType":"VirtualMachine","Resources":["claim-abc"],"NotBefore":"Mon, 19 Sep 2016 18:29:47 GMT"},` +
		`{"EventId":"A2","EventStatus":"Scheduled","EventType":"Preempt","ResourceType":"VirtualMachine",` +
		`"Resources":["claim-abc"],"NotBefore":"Tue, 20 Sep 2016 08:00:01 GMT"}]}`
)

// runCheck runs scarpenter_check against a fake instance metadata service whose
// Scheduled Events answer is events with status, and returns its output and exit code.
func runCheck(t *testing.T, events string, status int) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			http.Error(w, "no Metadata header", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/metadata/instance/compute/name":
			if r.URL.Query().Get("format") != "text" || r.URL.Query().Get("api-version") == "" {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, vmName)
		case "/metadata/scheduledevents":
			if r.URL.Query().Get("api-version") != "2020-07-01" {
				http.Error(w, "bad api version", http.StatusBadRequest)
				return
			}
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, events)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	script := strings.ReplaceAll(noticeCheck, "http://169.254.169.254", srv.URL)
	out, err := exec.Command("sh", "-c", "scarpenter_check() {\n"+script+"\n}\nscarpenter_check").Output()
	if err == nil {
		return strings.TrimSpace(string(out)), 0
	}
	ee, ok := err.(*exec.ExitError)
	require.True(t, ok, err)
	return strings.TrimSpace(string(out)), ee.ExitCode()
}

func TestNoticeCheck(t *testing.T) {
	tests := map[string]struct {
		events string
		status int
		out    string
		code   int
	}{
		"a preemption of this VM, pretty-printed": {eventsPretty, 200, "SpotInterrupted 2016-09-19T18:29:47Z", 0},
		"a preemption among other events":         {eventsCompact, 200, "SpotInterrupted 2016-09-20T08:00:01Z", 0},
		"no events":                               {`{"DocumentIncarnation":1,"Events":[]}`, 200, "", 1},
		"a preemption of another VM": {
			strings.ReplaceAll(eventsCompact, `"claim-abc"`, `"claim-abcd"`), 200, "", 1},
		"another event of this VM": {`{"Events":[{"EventType":"Reboot","Resources":["claim-abc"]}]}`, 200, "", 1},
		"an unparsable deadline": {
			strings.ReplaceAll(eventsCompact, "Tue, 20 Sep 2016 08:00:01 GMT", "soon"), 200, "SpotInterrupted", 0},
		"a missing deadline":            {`{"Events":[{"EventType":"Preempt","Resources":["claim-abc"]}]}`, 200, "SpotInterrupted", 0},
		"the opt-in not done yet (404)": {"", 404, "", 2},
		"a server error":                {"", 500, "", 2},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			out, code := runCheck(t, tc.events, tc.status)
			require.Equal(t, tc.code, code)
			require.Equal(t, tc.out, out)
		})
	}
}

func TestNoticeCheckWithoutTheMetadataService(t *testing.T) {
	script := strings.ReplaceAll(noticeCheck, "http://169.254.169.254", "http://127.0.0.1:1")
	err := exec.Command("sh", "-c", "scarpenter_check() {\n"+script+"\n}\nscarpenter_check").Run()
	ee, ok := err.(*exec.ExitError)
	require.True(t, ok, err)
	require.Equal(t, 2, ee.ExitCode())
}

func TestInterruptionNoticesWindow(t *testing.T) {
	c := (&Driver{}).InterruptionNotices()
	require.NotEmpty(t, c.Script)
	require.Equal(t, 30, int(c.MinNotice.Seconds()))
	require.Equal(t, 1, int(c.Interval.Seconds()))
}

// The agent's script (the generic loop and this check) stays within the 1.5 KiB
// of plan 26 section 2.
func TestNoticeCheckSize(t *testing.T) {
	script, err := vendors.InterruptionAgentScript((&Driver{}).InterruptionNotices())
	require.NoError(t, err)
	require.LessOrEqual(t, len(script), 1536)
}
