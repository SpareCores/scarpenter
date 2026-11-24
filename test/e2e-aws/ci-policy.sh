#!/usr/bin/env bash
# Prints the permissions policy of the L5 lane's CI role (docs/how-to-guides/testing.md#the-l5-lane):
#
#   test/e2e-aws/ci-policy.sh <AWS account ID> [region] > ci-policy.json
#
# It is docs/reference/aws-iam-policy.json (the controller's own policy, overview
# section 11.6) for every cluster named scarpenter-ci-* (the tag conditions become
# StringLike), without the statements the lane does not use (an instance profile, a
# customer KMS key; the OptionalImageCopies* statements of the manual imagecopy scenario stay), plus four statements that belong to the test and not to the product:
#   LaneReads              the read-only calls the scenarios and the diagnostics make
#                          beyond the controller's (volumes, network interfaces, console output, and for the
#                          manual imagecopy scenario snapshots and the account's EBS encryption default);
#   LaneBringYourOwn       the workflow creates a subnet in the default VPC for the
#                          bring-your-own-resources scenario; the policy's own statements
#                          allow CreateSubnet only in a VPC that carries the cluster tag,
#                          and the default VPC does not (the new subnet itself must still
#                          carry the tag);
#   LaneInterruption       only when E2E_FIS_ROLE_ARN is set: the manual interruption scenario creates,
#                          starts and deletes an AWS FIS experiment template in the region and passes
#                          that role to FIS (the role itself, which FIS assumes to send the interruption,
#                          is the operator's);
#   DenyOtherInstanceTypes an explicit deny of RunInstances for every type the lane does
#                          not launch (the types of defaults.sh, including the manual gpu scenario's, and E2E_LOCAL_STORAGE_TYPE);
#   DenyOutsideRegion      EC2 and Service Quotas calls outside the lane's region and the copy region
#                          (E2E_COPY_REGION: the manual imagecopy scenario copies the Flatcar image
#                          into it and launches one node there; the sweeper looks at the other regions,
#                          finds itself refused, and skips them).
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
copy_region="${E2E_COPY_REGION}"

types=("${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_DB}")
IFS=, read -ra described <<<"${E2E_TYPES_DESCRIBED}"
types+=("${described[@]}")
[[ -z "${E2E_LOCAL_STORAGE_TYPE}" ]] || types+=("${E2E_LOCAL_STORAGE_TYPE}")
types+=("${E2E_GPU_TYPE}" "${E2E_COPY_TYPE}")
allowed="$(printf '%s\n' "${types[@]}" | sed 's/^aws\.//' | sort -u | jq -R . | jq -s .)"

fis_role="${E2E_FIS_ROLE_ARN:-}"
[[ -z "${fis_role}" || "${fis_role}" =~ ^arn:aws:iam::${account}:role/[A-Za-z0-9+=,.@_/-]+$ ]] ||
  {
    echo "E2E_FIS_ROLE_ARN must be a role ARN of the account" >&2
    exit 2
  }

sed -e 's/CLUSTER_NAME/scarpenter-ci-*/g' -e "s/123456789012/${account}/g" "${root}/docs/reference/aws-iam-policy.json" |
  jq --argjson allowed "${allowed}" --arg copy_region "${copy_region}" --arg fis_role "${fis_role}" --arg region "${region}" --arg account "${account}" '
    # The cluster tag is matched with a wildcard: StringEquals becomes StringLike for that key.
    (.Statement[] | select(.Condition.StringEquals?) | .Condition) |=
      ((.StringLike = ((.StringLike // {}) + (.StringEquals | with_entries(select(.key | endswith("scarpenter.sparecores.com/cluster")))))) |
       (.StringEquals |= with_entries(select(.key | endswith("scarpenter.sparecores.com/cluster") | not))) |
       (if (.StringEquals | length) == 0 then del(.StringEquals) else . end) |
       (if (.StringLike | length) == 0 then del(.StringLike) else . end))
    | .Statement |= map(select(.Sid | test("PassInstanceProfile|CustomerKms") | not))
    | .Statement += [
        {Sid: "LaneReads", Effect: "Allow",
         Action: ["ec2:DescribeVolumes", "ec2:DescribeNetworkInterfaces", "ec2:GetConsoleOutput", "ec2:DescribeSnapshots",
                  "ec2:GetEbsEncryptionByDefault", "ec2:GetEbsDefaultKmsKeyId"], Resource: "*"},
        {Sid: "LaneBringYourOwn", Effect: "Allow", Action: "ec2:CreateSubnet", Resource: ("arn:aws:ec2:" + $region + ":" + $account + ":vpc/*")},
        {Sid: "DenyOtherInstanceTypes", Effect: "Deny", Action: "ec2:RunInstances", Resource: "arn:aws:ec2:*:*:instance/*",
         Condition: {StringNotEquals: {"ec2:InstanceType": $allowed}}},
        {Sid: "DenyOutsideRegion", Effect: "Deny", Action: ["ec2:*", "servicequotas:*"], Resource: "*",
         Condition: {StringNotEquals: {"aws:RequestedRegion": ([$region, $copy_region] | unique)}}}
      ]
    | if $fis_role == "" then . else .Statement += [
        {Sid: "LaneInterruption", Effect: "Allow",
         Action: ["fis:CreateExperimentTemplate", "fis:DeleteExperimentTemplate", "fis:StartExperiment", "fis:GetExperiment"],
         Resource: [("arn:aws:fis:" + $region + ":" + $account + ":experiment-template/*"), ("arn:aws:fis:" + $region + ":" + $account + ":experiment/*"),
                    ("arn:aws:fis:" + $region + ":*:action/*")]},
        {Sid: "LanePassFisRole", Effect: "Allow", Action: "iam:PassRole", Resource: $fis_role,
         Condition: {StringEquals: {"iam:PassedToService": "fis.amazonaws.com"}}}
      ] end'
