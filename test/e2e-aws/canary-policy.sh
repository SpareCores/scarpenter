#!/usr/bin/env bash
# Prints the permissions policy of the canary's READ-ONLY role
# (docs/how-to-guides/testing-and-releasing.md#the-canary):
#
#   test/e2e-aws/canary-policy.sh <AWS account ID> [region] > canary-policy.json
#
# The canary (test/canary, vendor aws) runs the AWS driver's read paths: the regions and
# zones, the Flatcar images, the Standard vCPU quotas and their usage, the instance type
# offerings. This is exactly the set of actions it calls (canary_test.go runs it against
# the in-memory EC2 and compares the calls it made with this output) and nothing else: no
# action outside Describe, Get and List, and every call is limited to the lane's region
# through aws:RequestedRegion like the CI role's policy (ci-policy.sh), so that the zone
# reads of the other regions the driver tries are refused and skipped, as for the CI role.
# The account ID is checked and not otherwise used: a read-only policy names no resources.
# The output goes to `aws iam create-policy`; nothing here calls AWS.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
account="${1:?usage: canary-policy.sh <AWS account ID> [region]}"
[[ "${account}" =~ ^[0-9]{12}$ ]] || {
  echo "the account ID must be 12 digits" >&2
  exit 2
}
# shellcheck source=test/e2e-aws/defaults.sh
source "${here}/defaults.sh"
region="${2:-${AWS_REGION}}"
[[ "${region}" =~ ^[a-z]{2}(-[a-z]+)+-[0-9]+$ ]] || {
  echo "the region must look like us-east-2" >&2
  exit 2
}

jq -n --arg region "${region}" '{
  Version: "2012-10-17",
  Statement: [
    {Sid: "CanaryReads", Effect: "Allow",
     Action: [
       "ec2:DescribeAvailabilityZones",
       "ec2:DescribeCapacityReservations",
       "ec2:DescribeImages",
       "ec2:DescribeInstanceTypeOfferings",
       "ec2:DescribeInstanceTypes",
       "ec2:DescribeInstances",
       "ec2:DescribeRegions",
       "ec2:DescribeSpotInstanceRequests",
       "servicequotas:GetServiceQuota",
       "servicequotas:ListServiceQuotas"
     ],
     Resource: "*",
     Condition: {StringEquals: {"aws:RequestedRegion": [$region]}}}
  ]
}'
