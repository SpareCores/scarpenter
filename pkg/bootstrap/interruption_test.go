/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package bootstrap_test

import (
	"strings"
	"testing"
	"time"

	"github.com/SpareCores/scarpenter/internal/testutil"
	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/bootstrap"
	"github.com/SpareCores/scarpenter/pkg/vendors"
	"github.com/SpareCores/scarpenter/pkg/vendors/hcloud"
)

// noticeDriver is a driver with metadata addresses (hcloud's) and a notice check.
type noticeDriver struct{ *hcloud.Driver }

func (noticeDriver) InterruptionNotices() vendors.NoticeCheck {
	return vendors.NoticeCheck{
		// As long as the longest check expected: AWS's IMDSv2 (a token, then the action).
		Script: `  t=$(curl -sf --max-time 3 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token) || return 2
  s=$(curl -s --max-time 3 -w '%{http_code}' -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/spot/instance-action) || return 2
  case $s in *404) return 1 ;; *200) ;; *) return 2 ;; esac
  echo "SpotInterrupted $(echo "$s" | sed -n 's/.*"time": *"\([^"]*\)".*/\1/p')"`,
		Interval:     time.Second,
		NoticeWindow: vendors.NoticeWindow{MinNotice: 30 * time.Second, MaxNotice: 30 * time.Second},
	}
}

// TestInterruptionAgentFragment pins the Butane of the node agent and renders it
// into a node's user-data: the script, its settings and the unit in the metadata
// slice.
func TestInterruptionAgentFragment(t *testing.T) {
	f, ok, err := vendors.InterruptionAgent(noticeDriver{&hcloud.Driver{}}, false)
	if err != nil || !ok {
		t.Fatalf("InterruptionAgent = %v, %v", ok, err)
	}
	testutil.Golden(t, "interruption-agent.golden", []byte(f.Butane))

	p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
	p.Spot = true
	p.VendorFragments = []bootstrap.Fragment{{Name: f.Name, Butane: f.Butane}}
	r, err := bootstrap.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	var ignition string
	for _, part := range r.Parts {
		if part.Name == "vendor-"+f.Name {
			ignition = string(part.Ignition)
		}
	}
	for _, want := range []string{"scarpenter-interruption-agent", "scarpenter-interruption.service",
		"NODE_NAME=default-abc12", "API_SERVER=https://api.my-cluster.example.com:6443", "Slice=scarpenter-metadata.slice"} {
		if !strings.Contains(unescape(ignition), want) {
			t.Errorf("the Ignition of the agent lacks %q", want)
		}
	}
}

// unescape reverses the percent-encoding of Ignition's inline data URLs.
func unescape(s string) string {
	return strings.NewReplacer("%0A", "\n", "%3D", "=", "%3A", ":", "%2F", "/", "%2E", ".").Replace(s)
}

// A spot node gets the graceful node shutdown settings, an on-demand node not.
func TestKubeletShutdownGracePeriodOfSpotNodes(t *testing.T) {
	for spot, want := range map[bool]bool{true: true, false: false} {
		got, err := bootstrap.KubeletConfiguration(v1alpha1.KubeletSpec{}, 0, nil, "", "", spot)
		if err != nil {
			t.Fatal(err)
		}
		has := strings.Contains(got, "shutdownGracePeriod: 25s") && strings.Contains(got, "shutdownGracePeriodCriticalPods: 5s")
		if has != want {
			t.Errorf("spot=%v: shutdown settings present = %v, want %v:\n%s", spot, has, want, got)
		}
		if !spot && strings.Contains(got, "shutdownGracePeriod") {
			t.Errorf("an on-demand node has shutdown settings:\n%s", got)
		}
	}
}

// The renderer passes Spot on: the kubelet configuration of the user-data has
// the settings for a spot launch only.
func TestRenderPutsTheShutdownSettingsIntoSpotUserData(t *testing.T) {
	for spot, want := range map[bool]bool{true: true, false: false} {
		p := baseParams("amd64", v1alpha1.NetworkTemplateDirect)
		p.Spot = spot
		r, err := bootstrap.Render(p)
		if err != nil {
			t.Fatal(err)
		}
		var all strings.Builder
		for _, part := range r.Parts {
			all.Write(part.Ignition)
		}
		if got := strings.Contains(all.String(), "shutdownGracePeriod"); got != want {
			t.Errorf("spot=%v: the user-data has the shutdown settings = %v", spot, got)
		}
	}
}
