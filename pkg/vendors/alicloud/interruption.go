/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package alicloud

import (
	"time"

	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"

	"github.com/SpareCores/scarpenter/pkg/vendors"
)

// lockRecycling is the OperationLocks LockReason of a spot instance that ECS is
// about to reclaim.
const lockRecycling = "Recycling"

// noticeCheck is the node agent's check (plan 26 section 6): the instance
// metadata's spot termination time, read with a metadata token (the same call
// works in the hardened mode, where the token is required). The endpoint answers
// 404 while there is no notice and the time of the release (UTC, RFC 3339; ECS
// writes minute-precision times without seconds elsewhere, so a missing seconds
// field is added; whitespace around the time is dropped) once there is one. The agent's unit runs in the metadata
// slice, so the restricted firewall lets it through; after the tailnet join the
// address stays reachable (--netfilter-mode=nodivert, see tailnetFragment).
const noticeCheck = `  b=http://100.100.100.200/latest
  t=$(curl -sf --max-time 3 -X PUT -H 'X-aliyun-ecs-metadata-token-ttl-seconds: 60' $b/api/token) || t=
  set --
  [ -n "$t" ] && set -- -H "X-aliyun-ecs-metadata-token: $t"
  o=$(curl -s --max-time 3 -w ' %{http_code}' "$@" $b/meta-data/instance/spot/termination-time) || return 2
  case $o in
  *' 404') return 1 ;;
  *' 200') d=$(printf %s "${o% *}" | tr -d ' \r\n')
   case $d in ????-??-??T??:??Z) d=${d%Z}:00Z ;; esac
   echo "SpotInterrupted $d" ;;
  *) return 2 ;;
  esac`

// InterruptionNotices implements vendors.InterruptionNotices. The lead time is
// 5 minutes in Alibaba's product news (unverified, in particular with
// SpotDuration 0: spike S5), so MaxNotice is 5 minutes. MinNotice, used when the
// time is missing or unreadable, is a cautious minute.
func (d *Driver) InterruptionNotices() vendors.NoticeCheck {
	return vendors.NoticeCheck{
		Script:       noticeCheck,
		Interval:     5 * time.Second,
		NoticeWindow: vendors.NoticeWindow{MinNotice: time.Minute, MaxNotice: 5 * time.Minute},
	}
}

// goneReason is the Instance.GoneReason of an ECS instance: a spot instance
// whose OperationLocks hold the reason Recycling is being reclaimed (unverified,
// spike S5). A released instance is no longer returned by ECS, so this is only
// seen while the instance still exists. As the contract has it, a reason is
// only given for a gone (terminating or terminated) instance, so a running one
// with the lock has none yet.
func goneReason(capacity, state string, in *ecs.DescribeInstancesResponseBodyInstancesInstance) string {
	gone := state == vendors.StateTerminating || state == vendors.StateTerminated
	if !gone || capacity != vendors.CapacitySpot || in.OperationLocks == nil {
		return ""
	}
	for _, l := range in.OperationLocks.LockReason {
		if l != nil && deref(l.LockReason) == lockRecycling {
			return v1alpha1.ReasonSpotInterrupted
		}
	}
	return ""
}
