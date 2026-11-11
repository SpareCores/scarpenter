#!/usr/bin/env bash
# Prints the permissions and the guardrails of the Azure L5 lane's CI identity
# (docs/how-to-guides/testing.md#the-l5-lane). PRINT ONLY: nothing here calls Azure and
# nothing is applied; the operator of the CI account applies it with whatever tool they use.
#
#   test/e2e-azure/ci-policy.sh <subscription ID> [region] [resource group] > ci-policy.json
#
# One JSON document with three lists:
#   roleDefinitions    two custom roles:
#                      scarpenter-ci-lane, assigned at the LANE'S RESOURCE GROUP only: what the
#                        controller writes (VMs, NICs, public IPs, disks, proximity placement
#                        groups, and the managed VNet, NSG and application security groups) and
#                        what the scenarios and the diagnostics read beyond it (disks, boot
#                        diagnostics, the activity log);
#                      scarpenter-ci-reads, assigned at the subscription: the reads that are not
#                        scoped to a group (the SKU table, the usages, the marketplace images and
#                        agreements, the provider registrations);
#   roleAssignments    the two roles for the CI identity (a user-assigned managed identity
#                      federated with the repository's environment e2e), by scope;
#   policyAssignments  the cost guards, Azure Policy at the lane's resource group, effect
#                      Deny: "Allowed virtual machine size SKUs" with the sizes of defaults.sh
#                      (not E2E_TYPE_DENIED: the negative scenario provokes the refusal), and
#                      "Allowed locations" with the lane's region. The role's scope is the third
#                      guard: the identity can write nowhere else.
# The action lists follow plan 17 section 2 and the calls the scenarios make; the exact list is
# UNVERIFIED and is what the first live run checks (the live verification list, item 11), as
# are the built-in policy definition IDs below.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
sub="${1:?usage: ci-policy.sh <subscription ID> [region] [resource group]}"
[[ "${sub}" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] || {
  echo "the subscription ID must be a GUID" >&2
  exit 2
}
# shellcheck source=test/e2e-azure/defaults.sh
source "${here}/defaults.sh"
region="${2:-${AZURE_REGION}}"
[[ "${region}" =~ ^[a-z0-9]+$ ]] || {
  echo "the region must look like westeurope" >&2
  exit 2
}
group="${3:-${AZURE_RESOURCE_GROUP:-scarpenter-ci}}"
[[ "${group}" =~ ^[A-Za-z0-9._()-]{1,90}$ ]] || {
  echo "the resource group name is not valid" >&2
  exit 2
}

# The sizes the lane launches (the policy denies every other), without the vendor prefix.
types=("${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_SPOT}" "${E2E_TYPE_DB}")
IFS=, read -ra described <<<"${E2E_TYPES_DESCRIBED}"
types+=("${described[@]}")
allowed="$(printf '%s\n' "${types[@]}" | sed 's/^azure\.//' | sort -u | jq -R . | jq -s .)"

jq -n --arg sub "${sub}" --arg region "${region}" --arg group "${group}" --argjson allowed "${allowed}" '
  ("/subscriptions/" + $sub) as $subscope
  | ($subscope + "/resourceGroups/" + $group) as $groupscope
  | {
    roleDefinitions: [
      {Name: "scarpenter-ci-lane", IsCustom: true,
       Description: "SCarpenter L5 lane: the controller and the scenarios, in the lane resource group only",
       Actions: [
         "Microsoft.Compute/virtualMachines/read",
         "Microsoft.Compute/virtualMachines/write",
         "Microsoft.Compute/virtualMachines/delete",
         "Microsoft.Compute/virtualMachines/instanceView/read",
         "Microsoft.Compute/virtualMachines/retrieveBootDiagnosticsData/action",
         "Microsoft.Compute/disks/read",
         "Microsoft.Compute/disks/write",
         "Microsoft.Compute/disks/delete",
         "Microsoft.Compute/proximityPlacementGroups/read",
         "Microsoft.Compute/proximityPlacementGroups/write",
         "Microsoft.Compute/proximityPlacementGroups/delete",
         "Microsoft.Network/networkInterfaces/*",
         "Microsoft.Network/publicIPAddresses/*",
         "Microsoft.Network/networkSecurityGroups/*",
         "Microsoft.Network/applicationSecurityGroups/*",
         "Microsoft.Network/virtualNetworks/*",
         "Microsoft.Network/virtualNetworks/subnets/join/action",
         "Microsoft.Network/networkSecurityGroups/join/action",
         "Microsoft.Network/publicIPAddresses/join/action",
         "Microsoft.Network/applicationSecurityGroups/joinIpConfiguration/action",
         "Microsoft.Network/applicationSecurityGroups/joinNetworkSecurityRule/action",
         "Microsoft.Resources/subscriptions/resourceGroups/read",
         "Microsoft.Resources/subscriptions/resources/read",
         "Microsoft.Insights/eventtypes/values/read"
       ],
       NotActions: [],
       DataActions: [],
       AssignableScopes: [$subscope]},
      {Name: "scarpenter-ci-reads", IsCustom: true,
       Description: "SCarpenter L5 lane: the subscription-wide reads the driver and the preflight make",
       Actions: [
         "Microsoft.Compute/skus/read",
         "Microsoft.Compute/locations/*/read",
         "Microsoft.Compute/locations/usages/read",
         "Microsoft.Network/locations/*/read",
         "Microsoft.Network/locations/usages/read",
         "Microsoft.Resources/subscriptions/read",
         "Microsoft.Resources/subscriptions/locations/read",
         "Microsoft.Resources/subscriptions/providers/read",
         "Microsoft.Resources/subscriptions/resources/read",
         "Microsoft.MarketplaceOrdering/agreements/read",
         "Microsoft.MarketplaceOrdering/offerTypes/publishers/offers/plans/agreements/read"
       ],
       NotActions: [],
       DataActions: [],
       AssignableScopes: [$subscope]}
    ],
    roleAssignments: [
      {role: "scarpenter-ci-lane", scope: $groupscope, principal: "the CI identity (user-assigned managed identity)"},
      {role: "scarpenter-ci-reads", scope: $subscope, principal: "the CI identity (user-assigned managed identity)"}
    ],
    policyAssignments: [
      {name: "scarpenter-ci-allowed-sizes", displayName: "Allowed virtual machine size SKUs",
       policyDefinitionId: "/providers/Microsoft.Authorization/policyDefinitions/cccc23c7-8427-4f53-ad12-b6a63eb452b3",
       scope: $groupscope, enforcementMode: "Default",
       parameters: {listOfAllowedSKUs: {value: $allowed}}},
      {name: "scarpenter-ci-allowed-locations", displayName: "Allowed locations",
       policyDefinitionId: "/providers/Microsoft.Authorization/policyDefinitions/e56962a6-4747-49cd-b67b-bf8b01975c4c",
       scope: $groupscope, enforcementMode: "Default",
       parameters: {listOfAllowedLocations: {value: [$region]}}}
    ]
  }'
