/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package gcp

import (
	"fmt"
	"time"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// Interruption notice of a Spot VM (plan 26 sections 2 and 6). Google sets the
// metadata value instance/preempted to TRUE when it takes the VM and sends the
// ACPI G2 soft-off signal, after the preemption notice duration of the
// scheduling (0 s unless MaxPreemptionNotice is asked for), and stops it up to
// shutdownWindow after that. The kubelet's graceful node shutdown (spot launches
// render it) covers a notice that was missed.
const (
	// shutdownWindow is the time between the ACPI G2 signal and the stop of a
	// Spot VM (documented).
	shutdownWindow = 30 * time.Second
	// MaxPreemptionNotice is the longest notice the driver asks for (--gcp-preemption-notice):
	// the 120 s of plan 26. UNVERIFIED (spike S3): the limit of the API field.
	MaxPreemptionNotice = 120 * time.Second
)

// noticeScript is the check of the node agent. It reads the value; TRUE is the
// notice at once (the change may have come before the first read, and a long
// poll from TRUE would wait for a change away from it). Otherwise it waits (long
// poll, up to 60 s) for the value to change from the ETag of that read, then
// reads the value again, so a failing long poll only costs the agent's one
// second pause. 169.254.169.254 is the metadata server (the name needs DNS). The
// deadline is the end of the preemption notice; without a notice there is none,
// and the controller assumes the shortest notice of the window (the shutdown
// window). The variable names stay clear of the agent loop's.
const noticeScript = `  h='Metadata-Flavor: Google'
  p=http://169.254.169.254/computeMetadata/v1/instance/preempted
  r=$(curl -sf -D - -m3 -H "$h" $p) || return 2
  case $r in *TRUE) ;; *)
    t=$(echo "$r" | tr -d '\r' | sed -n 's/^[Ee][Tt]ag: //p')
    curl -sf -o /dev/null -m70 -H "$h" "$p?wait_for_change=true&timeout_sec=60&last_etag=$t"
    r=$(curl -sf -m3 -H "$h" $p) || return 2 ;;
  esac
  case $r in *TRUE) echo SpotInterrupted%s ;; *) return 1 ;; esac
`

// InterruptionNotices implements vendors.InterruptionNotices: the node agent of a
// spot node blocks on the metadata value instance/preempted.
func (d *Driver) InterruptionNotices() vendors.NoticeCheck {
	deadline := ""
	if n := int64(d.preemptionNotice / time.Second); n > 0 {
		deadline = fmt.Sprintf(` $(date -u -d @$(($(date +%%s)+%d)) +%%Y-%%m-%%dT%%H:%%M:%%SZ)`, n)
	}
	return vendors.NoticeCheck{
		Script: fmt.Sprintf(noticeScript, deadline),
		// The check blocks; the pause only keeps a failing one from spinning.
		Interval: time.Second,
		NoticeWindow: vendors.NoticeWindow{
			MinNotice: shutdownWindow, MaxNotice: shutdownWindow + d.preemptionNotice,
		},
	}
}

// goneReasonOf is the Instance.GoneReason of a gone instance. A Spot VM that is
// stopping, stopped or terminated was reclaimed (the launch deletes it on
// reclaim); SCarpenter's own deletes are filtered by the core (plan 26 section
// 5). A Spot VM that a person stops by hand is indistinguishable without the
// zone operations, which the driver does not read.
func goneReasonOf(capacityType, state string) string {
	if capacityType == vendors.CapacitySpot && state == vendors.StateTerminating {
		return v1alpha1.ReasonSpotInterrupted
	}
	return ""
}
