#!/usr/bin/env bash
# The pins of the Azure L5 lane. Every value can be overridden through the environment
# (the workflow's inputs do it for the minimum-Kubernetes run).
#
# THIS LANE IS NOT IN test/e2e-vendors.txt, ON PURPOSE. A line there puts Azure into the
# pull request tiers, the nightly canary and the release gate, and may only be added when
# every cost guard of docs/how-to-guides/testing-and-releasing.md#per-vendor-cost-guards
# is true on a LIVE account. Nobody has verified them; the guards still to be verified
# live are:
#   - the Azure Policy "Allowed virtual machine size SKUs" (deny) assigned to the lane's
#     resource group refuses a VM of a size outside the pins below (ci-policy.sh prints
#     it; the negative scenario provokes it);
#   - the "Allowed locations" policy and the CI role's scope (the lane's resource group
#     only) keep the lane in its region;
#   - every resource, the inline disks aside, carries scarpenter-cluster and
#     scarpenter-ci-created-at, and the sweeper finds the disks by their name prefix;
#   - the sweeper (e2e-azure-sweeper.yml, every 3 hours, environment e2e-sweeper) deletes
#     what a dead run left, with the lane's real role;
#   - the vCPU quotas (regional 16, the lane's families 16, Spot 4, Standard public IPs)
#     are the ceiling, and a budget alert on the subscription goes to a person;
#   - the teardown finds nothing tagged and no disk of a VM of the run after a real run;
#   - the read-only canary role (canary-policy.sh) reads everything the canary reads;
#   - the environments e2e, e2e-sweeper and e2e-canary exist with the federated
#     credentials (immutable subjects) and the variables AZURE_CLIENT_ID,
#     AZURE_TENANT_ID, AZURE_SUBSCRIPTION_ID, AZURE_RESOURCE_GROUP and AZURE_REGION.
# Until then the lane runs by hand (workflow_dispatch of e2e-cloud.yml cannot name it:
# the Prepare step checks the list) or from a fork's own list.
# shellcheck disable=SC2034  # used by the scripts that source lib.sh

# The region of the lane and the dedicated resource group inside it (its own VNet, subnet
# and NSG are made by SCarpenter or by the scenarios; the CI role is scoped to the group).
# The subscription, the identity and the group have no default: the workflow sets them
# from variables, a developer from their own account.
: "${AZURE_REGION:=westeurope}"
: "${E2E_K3S_VERSION:=v1.37.2+k3s1}"
# The kubelet of the nodes (a sysext of the sysext-bakery): at most the API server's
# version, at least three minors below it.
: "${E2E_KUBERNETES_VERSION:=v1.37.1}"
: "${E2E_TAILSCALE_VERSION:=v1.98.10}"
: "${E2E_FLATCAR_CHANNEL:=stable}"
# A marketplace version is a Flatcar release number. Whether the corevm offers carry this
# release is on the live list of plan 17: the preflight and the canary check it.
: "${E2E_FLATCAR_VERSION:=4757.2.3}"
# Instance types the suite launches; the Azure Policy's size allow-list (ci-policy.sh) is
# built from them. B-series have no Spot, so the spot scenario uses a small D size that
# reports LowPriorityCapable: Standard_D2as_v5 (2 vCPUs, SCSI, Premium IO) is TO CONFIRM
# from the canary's output (Standard_D2ps_v6 is the arm64 alternative, if it has a SCSI
# disk controller).
: "${E2E_TYPE_AMD64:=azure.Standard_B2ats_v2}"
: "${E2E_TYPE_ARM64:=azure.Standard_B2pts_v2}"
: "${E2E_TYPE_SPOT:=azure.Standard_D2as_v5}"
# The database and its client: on-demand, Premium IO (the Premium data disk), proximity
# placement group.
: "${E2E_TYPE_DB:=azure.Standard_D2as_v5}"
# The types the described-not-named scenario may pick from (small to large): 2 vCPUs,
# 1 GiB and 8 GiB.
: "${E2E_TYPES_DESCRIBED:=azure.Standard_B2ats_v2,azure.Standard_B2as_v2}"
# A size OUTSIDE the policy's allow-list, for the negative scenario only: the policy must
# refuse it (or the catalog must not offer it); it is not in the size policy.
: "${E2E_TYPE_DENIED:=azure.Standard_B2ls_v2}"
# How long a node may take from its Pod to Running, and the exit criterion for
# scale to zero (seconds from the Pod's deletion to every VM gone).
: "${E2E_NODE_TIMEOUT:=720}"
# A node's NodeClaim must be launched (the VM created) this long after a wait began, or the
# wait fails at once as "never launched" instead of running into E2E_NODE_TIMEOUT. The
# driver waits up to 4 minutes for the VM's long-running operation, so this is longer than
# AWS's. A progress block (NodeClaims, Pods, events, controller errors) is printed to the
# job log this often during a wait.
: "${E2E_LAUNCH_TIMEOUT:=300}"
: "${E2E_PROGRESS_INTERVAL:=30}"
# After the first failed scenario the remaining ones are skipped and the run ends, so
# that the diagnostics and the teardown start at once; false runs every scenario.
: "${E2E_FAIL_FAST:=true}"
# The one knob to tune after the first live run: Azure has no "shutting down" state that
# returns early, a VM exists until its delete (with the NIC, public IP and disks)
# finished, so the deadline is longer than AWS's 2 minutes.
: "${E2E_SCALE_DOWN_DEADLINE:=240}"
# One refresh of the quota usage (the chart values set the same interval) plus a
# margin for the debounce and the API.
: "${E2E_QUOTA_REFRESH:=30}"
# How long the negative scenario watches a Pod that must never get a node.
: "${E2E_NEGATIVE_WAIT:=150}"
# The scenarios of the smoke mode (docs/how-to-guides/testing-and-releasing.md): the
# shortest path through the node path (the preflight always runs first, then one on-demand
# node from the pods to Ready, then scale to zero). Standalone-safe: amd64 needs only its own
# workload, and scalezero reads the amd64 node's state when it exists and checks the
# cluster-wide exit criterion whatever ran before. Order is the runner's, not this list's.
: "${E2E_SMOKE_SCENARIOS:=amd64,scalezero}"
# The vCPU quotas the suite needs (the preflight and the canary check them): the regional
# Total Regional vCPUs and the Spot vCPUs of the region, and every lane family's own quota.
: "${E2E_MIN_VCPU_OD:=16}"
: "${E2E_MIN_VCPU_SPOT:=4}"
: "${E2E_MIN_VCPU_FAMILY:=16}"
# The tailnet tags (docs/how-to-guides/testing.md#the-l5-lane).
: "${E2E_TAG_RUNNER:=tag:scarpenter-ci}"
: "${E2E_TAG_NODE:=tag:scarpenter-ci-node}"
# The audience of the federated credential (Entra's token exchange) and of the token
# the controller reads from AZURE_FEDERATED_TOKEN_FILE.
: "${E2E_AZURE_AUDIENCE:=api://AzureADTokenExchange}"
# How often the federated token the controller reads is renewed (GitHub's OIDC token is
# short-lived); how often the runner's own az session is logged in again (its access token
# lasts about an hour); and the longest the renewal loop runs.
: "${E2E_AZURE_TOKEN_INTERVAL:=240}"
: "${E2E_AZURE_RELOGIN_INTERVAL:=1800}"
: "${E2E_AZURE_TOKEN_MAX_SECONDS:=7200}"

# The list of described types as a YAML flow sequence for the NodePool manifest.
IFS=, read -ra _described <<<"${E2E_TYPES_DESCRIBED}"
E2E_TYPES_DESCRIBED_YAML="$(printf '"%s", ' "${_described[@]}")"
E2E_TYPES_DESCRIBED_YAML="${E2E_TYPES_DESCRIBED_YAML%, }"

# envsubst (manifests) and the child processes read exported variables.
export AZURE_REGION E2E_K3S_VERSION E2E_KUBERNETES_VERSION E2E_TAILSCALE_VERSION E2E_FLATCAR_CHANNEL \
  E2E_FLATCAR_VERSION E2E_TYPE_AMD64 E2E_TYPE_ARM64 E2E_TYPE_SPOT E2E_TYPE_DB E2E_TYPE_DENIED E2E_TYPES_DESCRIBED E2E_TYPES_DESCRIBED_YAML \
  E2E_NODE_TIMEOUT E2E_LAUNCH_TIMEOUT E2E_PROGRESS_INTERVAL E2E_FAIL_FAST E2E_SCALE_DOWN_DEADLINE E2E_QUOTA_REFRESH \
  E2E_NEGATIVE_WAIT E2E_SMOKE_SCENARIOS E2E_MIN_VCPU_OD E2E_MIN_VCPU_SPOT E2E_MIN_VCPU_FAMILY E2E_TAG_RUNNER E2E_TAG_NODE \
  E2E_AZURE_AUDIENCE E2E_AZURE_TOKEN_INTERVAL E2E_AZURE_RELOGIN_INTERVAL E2E_AZURE_TOKEN_MAX_SECONDS
