#!/usr/bin/env bash
# Prints the permissions of the canary's READ-ONLY identity
# (docs/how-to-guides/testing-and-releasing.md#the-canary). PRINT ONLY: nothing here calls
# Azure and nothing is applied.
#
#   test/e2e-azure/canary-policy.sh <subscription ID> > canary-policy.json
#
# The canary (test/canary, vendor azure) runs the Azure driver's read paths: the locations and
# zones, the Flatcar marketplace images and the state of their terms, the vCPU and public IP
# usages, the SKU table. That is the built-in role Reader on the subscription (every */read
# action) plus one custom role for what Reader does not include, the marketplace agreement
# reads. Nothing else: no write, no action, no data action. The output has the custom role's
# definition and the two assignments for the canary identity (a user-assigned managed
# identity federated with the repository's environment e2e-canary).
set -euo pipefail
sub="${1:?usage: canary-policy.sh <subscription ID>}"
[[ "${sub}" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] || {
  echo "the subscription ID must be a GUID" >&2
  exit 2
}

jq -n --arg sub "${sub}" '
  ("/subscriptions/" + $sub) as $subscope
  | {
    roleDefinitions: [
      {Name: "scarpenter-canary-marketplace-reads", IsCustom: true,
       Description: "SCarpenter canary: read the marketplace agreements (terms) of the Flatcar offers",
       Actions: [
         "Microsoft.MarketplaceOrdering/agreements/read",
         "Microsoft.MarketplaceOrdering/offerTypes/publishers/offers/plans/agreements/read"
       ],
       NotActions: [],
       DataActions: [],
       AssignableScopes: [$subscope]}
    ],
    roleAssignments: [
      {role: "Reader", scope: $subscope, principal: "the canary identity (user-assigned managed identity)"},
      {role: "scarpenter-canary-marketplace-reads", scope: $subscope, principal: "the canary identity (user-assigned managed identity)"}
    ]
  }'
