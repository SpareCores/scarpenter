#!/usr/bin/env bash
# Prints the RAM permissions policy of the L5 lane's CI role
# (docs/how-to-guides/testing.md#the-l5-lane), in the RAM policy language (Version "1"):
#
#   test/e2e-alicloud/ci-policy.sh <Alibaba Cloud account ID> [region] > ci-policy.json
#
# Nothing in this lane has run, and the RAM condition keys below were written from the
# documentation of RAM without a live check. What is UNVERIFIED, to be settled by the first
# run (the role's denies are the cost guard, so a wrong key fails open or closed):
#   - that acs:RequestTag/<key> is evaluated for ecs:RunInstances (the keys ecs:InstanceType,
#     ecs:ImageOwnerId, acs:RequestTag and acs:ResourceTag are documented for RAM; their exact
#     use with RunInstances is the open point) and that StringLike takes a wildcard there;
#   - that ecs:InstanceType is the key of the instance type in a RunInstances request;
#   - the resource ARN patterns (acs:ecs:<region>:<account>:<type>/<id>, acs:vpc:...);
#   - that Describe* and the quota reads take only "*" as the resource (a region cannot
#     limit them: the region is limited where the ARN can say it, the writes).
# If the deny on the instance type does not bite, the account's vCPU quota (16 on-demand, 4
# spot) is the ceiling that bounds a runaway, and the sweeper and the budget alert catch the rest.
#
# Statements:
#   LaneReads              read-only calls: ecs:Describe*, ecs:ListTagResources,
#                          vpc:Describe*, quotas:ListProductQuotas and quotas:GetProductQuota;
#   LaunchTaggedInstances  ecs:RunInstances and ecs:TagResources in the lane's region, only
#                          with the tag scarpenter.sparecores.com/cluster like scarpenter-ci-*
#                          on the request;
#   MutateTaggedResources  deleting or changing instances, security groups, vSwitches and VPCs
#                          that carry the cluster tag (acs:ResourceTag), in the region; creating
#                          a security group, a VPC or a vSwitch needs the tag in the request
#                          (the driver and the byo scenario tag at creation);
#   DeploymentSets         creating and deleting deployment sets in the region: they take no
#                          tags (the driver puts a marker in the description), so no tag
#                          condition can apply; the region ARN limits them, and the sweeper
#                          only deletes sets whose marker names a scarpenter-ci-* cluster;
#   DenyOtherInstanceTypes an explicit deny of ecs:RunInstances for every instance type the
#                          lane does not launch (the types of defaults.sh);
# Every write Allow names the lane's region in its ARNs, so a write in another region is
# denied by default (RAM has no NotResource element that I could rely on, unverified, so
# there is no explicit region deny). The statements have no Sid: RAM policies do not take one.
# The output goes to `aliyun ram CreatePolicy`; nothing here calls Alibaba Cloud.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
account="${1:?usage: ci-policy.sh <Alibaba Cloud account ID> [region]}"
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

types=("${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_DB}")
IFS=, read -ra described <<<"${E2E_TYPES_DESCRIBED}"
types+=("${described[@]}")
[[ -z "${E2E_LOCAL_STORAGE_TYPE}" ]] || types+=("${E2E_LOCAL_STORAGE_TYPE}")
allowed="$(printf '%s\n' "${types[@]}" | sed 's/^alicloud\.//' | sort -u | jq -R . | jq -s .)"

jq -n --argjson allowed "${allowed}" --arg region "${region}" --arg account "${account}" '
  "scarpenter.sparecores.com/cluster" as $tag
  | ("acs:ecs:" + $region + ":" + $account + ":*") as $ecs
  | ("acs:vpc:" + $region + ":" + $account + ":*") as $vpc
  | {
    Version: "1",
    Statement: [
      {Effect: "Allow", Action: ["ecs:Describe*", "ecs:ListTagResources", "vpc:Describe*",
         "quotas:ListProductQuotas", "quotas:GetProductQuota"], Resource: "*"},
      {Effect: "Allow", Action: ["ecs:RunInstances", "ecs:TagResources"], Resource: [$ecs, $vpc],
       Condition: {StringLike: {("acs:RequestTag/" + $tag): ["scarpenter-ci-*"]}}},
      {Effect: "Allow",
       Action: ["ecs:DeleteInstance", "ecs:AuthorizeSecurityGroup", "ecs:RevokeSecurityGroup", "ecs:DeleteSecurityGroup",
         "vpc:DeleteVpc", "vpc:DeleteVSwitch", "vpc:TagResources"],
       Resource: [$ecs, $vpc],
       Condition: {StringLike: {("acs:ResourceTag/" + $tag): ["scarpenter-ci-*"]}}},
      {Effect: "Allow", Action: ["ecs:CreateSecurityGroup", "vpc:CreateVpc", "vpc:CreateVSwitch"], Resource: [$ecs, $vpc],
       Condition: {StringLike: {("acs:RequestTag/" + $tag): ["scarpenter-ci-*"]}}},
      {Effect: "Allow", Action: ["ecs:CreateDeploymentSet", "ecs:DeleteDeploymentSet"], Resource: $ecs},
      {Effect: "Deny", Action: "ecs:RunInstances", Resource: "*",
       Condition: {StringNotEquals: {"ecs:InstanceType": $allowed}}}
    ]
  }'
