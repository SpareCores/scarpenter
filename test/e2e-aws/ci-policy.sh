#!/usr/bin/env bash
# Prints the permissions policy of the L5 lane's CI role (docs/how-to-guides/testing.md#the-l5-lane):
#
#   test/e2e-aws/ci-policy.sh <AWS account ID> [region] > ci-policy.json
#
# It is docs/reference/aws-iam-policy.json (the controller's own policy, overview
# section 11.6) for every cluster named scarpenter-ci-* (the tag conditions become
# StringLike), without the statements the lane does not use (an instance profile, a
# customer KMS key), plus four statements that belong to the test and not to the product:
#   LaneReads              the read-only calls the scenarios and the diagnostics make
#                          beyond the controller's (volumes, network interfaces, console output);
#   LaneBringYourOwn       the workflow creates a subnet in the default VPC for the
#                          bring-your-own-resources scenario; the policy's own statements
#                          allow CreateSubnet only in a VPC that carries the cluster tag,
#                          and the default VPC does not (the new subnet itself must still
#                          carry the tag);
#   DenyOtherInstanceTypes an explicit deny of RunInstances for every type the lane does
#                          not launch (the types of defaults.sh, and E2E_LOCAL_STORAGE_TYPE);
#   DenyOutsideRegion      EC2 and Service Quotas calls outside the lane's region (the
#                          sweeper looks at the other regions, finds itself refused, and
#                          skips them).
# (LaneBringYourOwn is limited to the region.) The output goes to `aws iam create-policy`; nothing here calls AWS.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
account="${1:?usage: ci-policy.sh <AWS account ID> [region]}"
[[ "${account}" =~ ^[0-9]{12}$ ]] || {
  echo "the account ID must be 12 digits" >&2
  exit 2
}
# shellcheck source=test/e2e-aws/defaults.sh
source "${here}/defaults.sh"
region="${2:-${AWS_REGION}}"

types=("${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_DB}")
IFS=, read -ra described <<<"${E2E_TYPES_DESCRIBED}"
types+=("${described[@]}")
[[ -z "${E2E_LOCAL_STORAGE_TYPE}" ]] || types+=("${E2E_LOCAL_STORAGE_TYPE}")
allowed="$(printf '%s\n' "${types[@]}" | sed 's/^aws\.//' | sort -u | jq -R . | jq -s .)"

sed -e 's/CLUSTER_NAME/scarpenter-ci-*/g' -e "s/123456789012/${account}/g" "${root}/docs/reference/aws-iam-policy.json" |
  jq --argjson allowed "${allowed}" --arg region "${region}" --arg account "${account}" '
    # The cluster tag is matched with a wildcard: StringEquals becomes StringLike for that key.
    (.Statement[] | select(.Condition.StringEquals?) | .Condition) |=
      ((.StringLike = ((.StringLike // {}) + (.StringEquals | with_entries(select(.key | endswith("scarpenter.sparecores.com/cluster")))))) |
       (.StringEquals |= with_entries(select(.key | endswith("scarpenter.sparecores.com/cluster") | not))) |
       (if (.StringEquals | length) == 0 then del(.StringEquals) else . end) |
       (if (.StringLike | length) == 0 then del(.StringLike) else . end))
    | .Statement |= map(select(.Sid | test("PassInstanceProfile|CustomerKms") | not))
    | .Statement += [
        {Sid: "LaneReads", Effect: "Allow",
         Action: ["ec2:DescribeVolumes", "ec2:DescribeNetworkInterfaces", "ec2:GetConsoleOutput"], Resource: "*"},
        {Sid: "LaneBringYourOwn", Effect: "Allow", Action: "ec2:CreateSubnet", Resource: ("arn:aws:ec2:" + $region + ":" + $account + ":vpc/*")},
        {Sid: "DenyOtherInstanceTypes", Effect: "Deny", Action: "ec2:RunInstances", Resource: "arn:aws:ec2:*:*:instance/*",
         Condition: {StringNotEquals: {"ec2:InstanceType": $allowed}}},
        {Sid: "DenyOutsideRegion", Effect: "Deny", Action: ["ec2:*", "servicequotas:*"], Resource: "*",
         Condition: {StringNotEquals: {"aws:RequestedRegion": [$region]}}}
      ]'
