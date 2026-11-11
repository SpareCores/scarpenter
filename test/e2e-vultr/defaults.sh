#!/usr/bin/env bash
# The pins of the Vultr L5 lane. Every value can be overridden through the environment
# (the workflow's inputs do it for the minimum-Kubernetes run).
# shellcheck disable=SC2034  # used by the scripts that source lib.sh

# The region of the run and of the Flatcar snapshot's account. A Vultr region is the unit
# (no zones): the zone of a node is its region.
: "${VULTR_REGION:=ewr}"
: "${E2E_K3S_VERSION:=v1.37.2+k3s1}"
# The kubelet of the nodes (a sysext of the sysext-bakery): at most the API server's
# version, at least three minors below it.
: "${E2E_KUBERNETES_VERSION:=v1.37.1}"
: "${E2E_TAILSCALE_VERSION:=v1.98.10}"
# The Flatcar release the account's snapshot holds, described
# flatcar-<channel>-<version>-amd64 (the account's operator creates it once per pinned
# version from the images repository's URL; docs/how-to-guides/vultr-credentials.md).
: "${E2E_FLATCAR_CHANNEL:=stable}"
: "${E2E_FLATCAR_VERSION:=4757.2.3}"
# Plans the suite launches (about a cent per hour each; plan 20, section 9). Vultr has no
# Arm instances, no spot and no quota API, so there is no arm64, spot or quota pin.
: "${E2E_TYPE_AMD64:=vultr.vc2-1c-1gb}"
: "${E2E_TYPE_SECOND:=vultr.vhf-1c-1gb}"
# The types the described-not-named scenario may pick from (small to large): the workload
# wants 2 vCPUs and more than 3000 MiB, which only the last one has.
: "${E2E_TYPES_DESCRIBED:=vultr.vc2-1c-1gb,vultr.vc2-1c-2gb,vultr.vc2-2c-4gb}"
# How long a node may take from its Pod to Running, and the exit criterion for scale to
# zero (seconds from the Pod's deletion to every instance deleted).
: "${E2E_NODE_TIMEOUT:=720}"
# A node's NodeClaim must be launched (the instance created) this long after a wait began,
# or the wait fails at once as "never launched". A progress block is printed to the job log
# this often during a wait.
: "${E2E_LAUNCH_TIMEOUT:=180}"
: "${E2E_PROGRESS_INTERVAL:=30}"
# After the first failed scenario the remaining ones are skipped and the run ends.
: "${E2E_FAIL_FAST:=true}"
: "${E2E_SCALE_DOWN_DEADLINE:=120}"
# The scenarios of the smoke mode: the preflight always runs first, then one on-demand
# node from the Pod to Ready, then scale to zero.
: "${E2E_SMOKE_SCENARIOS:=amd64,scalezero}"
# The tailnet tags (docs/how-to-guides/testing.md#the-l5-lane).
: "${E2E_TAG_RUNNER:=tag:scarpenter-ci}"
: "${E2E_TAG_NODE:=tag:scarpenter-ci-node}"

# The list of described types as a YAML flow sequence for the NodePool manifest.
IFS=, read -ra _described <<<"${E2E_TYPES_DESCRIBED}"
E2E_TYPES_DESCRIBED_YAML="$(printf '"%s", ' "${_described[@]}")"
E2E_TYPES_DESCRIBED_YAML="${E2E_TYPES_DESCRIBED_YAML%, }"

# envsubst (manifests) and the child processes read exported variables.
export VULTR_REGION E2E_K3S_VERSION E2E_KUBERNETES_VERSION E2E_TAILSCALE_VERSION E2E_FLATCAR_CHANNEL \
  E2E_FLATCAR_VERSION E2E_TYPE_AMD64 E2E_TYPE_SECOND E2E_TYPES_DESCRIBED E2E_TYPES_DESCRIBED_YAML \
  E2E_NODE_TIMEOUT E2E_LAUNCH_TIMEOUT E2E_PROGRESS_INTERVAL E2E_FAIL_FAST E2E_SCALE_DOWN_DEADLINE \
  E2E_SMOKE_SCENARIOS E2E_TAG_RUNNER E2E_TAG_NODE
