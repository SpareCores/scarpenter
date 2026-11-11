#!/usr/bin/env bash
# Prints the RAM permissions policy of the canary's READ-ONLY role
# (docs/how-to-guides/testing-and-releasing.md#the-canary):
#
#   test/e2e-alicloud/canary-policy.sh <Alibaba Cloud account ID> [region] > canary-policy.json
#
# The canary (test/canary, vendor alicloud) runs the Alibaba Cloud driver's read paths: the
# regions and zones, the Flatcar images, the vCPU quota attributes and their usage, the
# instance type offerings. This is exactly the set of actions it calls (canary_test.go runs
# it against the in-memory ECS and compares the calls it made with this output) and nothing
# else: no action outside Describe, Get and List. Nothing in this lane has run.
#
# UNVERIFIED: RAM can limit a read to a region only where the action takes a resource ARN,
# and the Describe actions of ECS take "*" only (as far as the documentation says), so the
# policy names no region condition and no resource: the lane's region is a parameter of the
# canary, not a limit of the role. The write guard of the canary's own clients is the second
# wall. [region] and the account ID are checked and not otherwise used.
# The output goes to `aliyun ram CreatePolicy`; nothing here calls Alibaba Cloud.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
account="${1:?usage: canary-policy.sh <Alibaba Cloud account ID> [region]}"
[[ "${account}" =~ ^[0-9]{12,20}$ ]] || {
  echo "the account ID must be 12 to 20 digits" >&2
  exit 2
}
# shellcheck source=test/e2e-alicloud/defaults.sh
source "${here}/defaults.sh"
region="${2:-${ALICLOUD_REGION}}"
[[ "${region}" =~ ^[a-z]{2}(-[a-z0-9]+)+-[0-9]+$ ]] || {
  echo "the region must look like ap-southeast-1" >&2
  exit 2
}

jq -n '{
  Version: "1",
  Statement: [
    {Effect: "Allow",
     Action: [
       "ecs:DescribeAccountAttributes",
       "ecs:DescribeAvailableResource",
       "ecs:DescribeImages",
       "ecs:DescribeInstanceTypes",
       "ecs:DescribeInstances",
       "ecs:DescribeRegions",
       "ecs:DescribeZones"
     ],
     Resource: "*"}
  ]
}'
