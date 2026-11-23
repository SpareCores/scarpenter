/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package azure

import (
	"time"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// noticeCheck is the node agent's scarpenter_check for Azure (plan 26 section 6):
// Scheduled Events, where a Spot eviction is an event of type Preempt that names
// the VM in Resources.
//
// The first request of a VM registers it for Scheduled Events; Azure answers
// only after that (up to two minutes), so the failure of the first calls is a
// transient error, and the agent starts at boot. The Node's name is the
// NodeClaim's, not the VM's, so the VM name comes from the instance metadata.
// The response is cut into one line per object and matched with grep, as the
// agent has no JSON parser (field spacing is not relied on). NotBefore is
// RFC 1123 ("Mon, 02 Jan 2006 15:04:05 GMT"), which date turns into RFC 3339; a
// date that cannot convert leaves the deadline out and the controller assumes
// the minimum notice. Preempt is not acknowledged (StartRequests): approving
// an event would end its window early.
const noticeCheck = `  c() { curl -sf -m3 -H Metadata:true http://169.254.169.254/metadata/"$1"; }
  [ -n "$v" ] || v=$(c "instance/compute/name?api-version=2021-02-01&format=text") || return 2
  m=$(c "scheduledevents?api-version=2020-07-01") || return 2
  m=$(printf %s "$m" | sed 's/" *: */":/g' | tr '\n{' ' \n' |
   grep "\"EventType\":\"Preempt\".*\"Resources\":\[[^]]*\"$v\"") || return 1
  m=$(echo "$m" | sed -n 's/.*"NotBefore":"\([^"]*\)".*/\1/p')
  [ -z "$m" ] || m=$(date -u -d "$m" +%FT%TZ)
  echo SpotInterrupted $m`

// InterruptionNotices implements vendors.InterruptionNotices: Azure announces a
// Spot eviction at least 30 s ahead, best effort (plan 26 section 6).
func (d *Driver) InterruptionNotices() vendors.NoticeCheck {
	return vendors.NoticeCheck{
		Script:       noticeCheck,
		Interval:     time.Second,
		NoticeWindow: vendors.NoticeWindow{MinNotice: 30 * time.Second, MaxNotice: 2 * time.Minute},
	}
}

var _ vendors.InterruptionNotices = (*Driver)(nil)
