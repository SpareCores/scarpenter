#!/usr/bin/env bash
# The pins of the L5 lane (UpCloud). Every value can be overridden through the
# environment (the workflow's inputs do it for the minimum-Kubernetes run).
# shellcheck disable=SC2034  # used by the scripts that source lib.sh
#
# Prerequisite, provided by the operator of the CI account (not by this repository): in the
# lane's zone, persistent Flatcar templates of the pinned version, one per storage tier
# (both, where the account can make a Standard one; MaxIOPS is the one the lane needs),
# built by the images repository (plan 22) and imported by the operator, with the labels
#   scarpenter-use=true           (never touched by the sweeper)
#   scarpenter-image=flatcar
#   flatcar-channel=<channel>     flatcar-version=<version>
#   flatcar-arch=amd64            flatcar-tier=maxiops|standard
# The lane's NodeClasses leave resources.images unset: the driver finds them by this label
# convention. The preflight fails, naming the template to import, when there is none.

# UpCloud's regions are its zones; the lane runs in one.
: "${UPCLOUD_ZONE:=fi-hel1}"
: "${E2E_K3S_VERSION:=v1.37.2+k3s1}"
# The kubelet of the nodes (a sysext of the sysext-bakery): at most the API server's
# version, at least three minors below it.
: "${E2E_KUBERNETES_VERSION:=v1.37.1}"
: "${E2E_TAILSCALE_VERSION:=v1.98.10}"
: "${E2E_FLATCAR_CHANNEL:=stable}"
: "${E2E_FLATCAR_VERSION:=4757.2.3}"
# Plans the suite launches. Plan 19 names 1xCPU-1GB, but the committed Keeper snapshot lists
# only the CLOUDNATIVE, PREMIUM, STARTER and GPU families as active: STARTER-1xCPU-1GB is the
# smallest plan that is both offered and carries an included disk and a public IPv4 address.
# Change it here if the live account's plan list says otherwise.
: "${E2E_TYPE_AMD64:=upcloud.STARTER-1xCPU-1GB}"
# The types the described-not-named scenario may pick from (small to large): the workload
# asks for 2 vCPUs and more than 3000 MiB, which rules out the first.
: "${E2E_TYPES_DESCRIBED:=upcloud.STARTER-1xCPU-1GB,upcloud.STARTER-2xCPU-4GB,upcloud.STARTER-2xCPU-8GB}"
# How long a node may take from its Pod to Running (a server is cloned from the template
# first), and the exit criterion for scale to zero (seconds from the Pod's deletion to
# every server stopped and deleted with its disks).
: "${E2E_NODE_TIMEOUT:=900}"
# A node's NodeClaim must be launched (the server created) this long after a wait began, or
# the wait fails at once as "never launched". A progress block is printed to the job log
# this often during a wait.
: "${E2E_LAUNCH_TIMEOUT:=180}"
: "${E2E_PROGRESS_INTERVAL:=30}"
# After the first failed scenario the remaining ones are skipped and the run ends, so that
# the diagnostics and the teardown start at once; false runs every scenario.
: "${E2E_FAIL_FAST:=true}"
# Plan 19: servers gone within 2 minutes of the Pod's deletion. A server is hard-stopped,
# then deleted with its storages, the slowest delete of the hourly vendors; the scenario
# notes how long it took, to tune this after the first live run.
: "${E2E_SCALE_DOWN_DEADLINE:=120}"
# One refresh of the quota usage (the chart values set the same interval) plus a margin.
: "${E2E_QUOTA_REFRESH:=30}"
# The scenarios of the smoke mode (docs/how-to-guides/testing-and-releasing.md): the
# shortest path through the node path. Standalone-safe, as on AWS.
: "${E2E_SMOKE_SCENARIOS:=amd64,scalezero}"
# The account limits the suite needs (the sub-account's resource_limits, set by the main
# account as the cost guard; the preflight and the canary check them): cores and memory in MB
# for the servers of the scenarios that run at once (about nine cores).
: "${E2E_MIN_CORES:=10}"
: "${E2E_MIN_MEMORY_MB:=12288}"
# The tailnet tags (docs/how-to-guides/testing.md#the-l5-lane).
: "${E2E_TAG_RUNNER:=tag:scarpenter-ci}"
: "${E2E_TAG_NODE:=tag:scarpenter-ci-node}"

# The list of described types as a YAML flow sequence for the NodePool manifest.
IFS=, read -ra _described <<<"${E2E_TYPES_DESCRIBED}"
E2E_TYPES_DESCRIBED_YAML="$(printf '"%s", ' "${_described[@]}")"
E2E_TYPES_DESCRIBED_YAML="${E2E_TYPES_DESCRIBED_YAML%, }"

# envsubst (manifests) and the child processes read exported variables.
export UPCLOUD_ZONE E2E_K3S_VERSION E2E_KUBERNETES_VERSION E2E_TAILSCALE_VERSION E2E_FLATCAR_CHANNEL \
  E2E_FLATCAR_VERSION E2E_TYPE_AMD64 E2E_TYPES_DESCRIBED E2E_TYPES_DESCRIBED_YAML \
  E2E_NODE_TIMEOUT E2E_LAUNCH_TIMEOUT E2E_PROGRESS_INTERVAL E2E_FAIL_FAST E2E_SCALE_DOWN_DEADLINE E2E_QUOTA_REFRESH \
  E2E_SMOKE_SCENARIOS E2E_MIN_CORES E2E_MIN_MEMORY_MB E2E_TAG_RUNNER E2E_TAG_NODE
