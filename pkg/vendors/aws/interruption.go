/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package aws

import (
	"time"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/SpareCores/scarpenter/pkg/apis/v1alpha1"
	"github.com/SpareCores/scarpenter/pkg/vendors"
)

var _ vendors.InterruptionNotices = (*Driver)(nil)

// spotTerminationCode is the StateReason.Code of an instance that EC2 terminated
// because it needed the capacity back.
const spotTerminationCode = "Server.SpotInstanceTermination"

// interruptionCheck is the node agent's check (plan 26): an IMDSv2 token, then the
// spot instance action, which is a 404 until EC2 announces the interruption and
// then {"action":"terminate","time":"<RFC 3339>"}. The user-data budget is tight
// on spot GPU nodes (the size test renders this script), so keep it short.
//
//nolint:lll // a shell script: its lines are the vendor's URLs and headers
const interruptionCheck = `  t=$(curl -sf --max-time 3 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token) || return 2
  s=$(curl -s --max-time 3 -w '%{http_code}' -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/spot/instance-action) || return 2
  case $s in *404) return 1 ;; *200) ;; *) return 2 ;; esac
  echo "SpotInterrupted $(echo "$s" | sed -n 's/.*"time": *"\([^"]*\)".*/\1/p')"`

// InterruptionNotices implements vendors.InterruptionNotices. EC2 gives a
// two-minute notice, best effort, and AWS advises polling every 5 s; the agent
// polls every 2 s. Unverified until spike S2 (the format of the response).
func (d *Driver) InterruptionNotices() vendors.NoticeCheck {
	return vendors.NoticeCheck{
		Script:       interruptionCheck,
		Interval:     2 * time.Second,
		NoticeWindow: vendors.NoticeWindow{MinNotice: 2 * time.Minute, MaxNotice: 2 * time.Minute},
	}
}

// goneReasonOf is the reason the instance is gone: SpotInterrupted when EC2
// says it terminated the instance for its capacity, else empty.
func goneReasonOf(in ec2types.Instance) string {
	if in.StateReason != nil && deref(in.StateReason.Code) == spotTerminationCode {
		return v1alpha1.ReasonSpotInterrupted
	}
	return ""
}
