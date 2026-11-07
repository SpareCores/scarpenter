#!/usr/bin/env bash
# The pins of the L5 lane. Every value can be overridden through the environment
# (the workflow's inputs do it for the minimum-Kubernetes run).
# shellcheck disable=SC2034  # used by the scripts that source lib.sh

: "${AWS_REGION:=us-east-2}"
: "${E2E_K3S_VERSION:=v1.37.2+k3s1}"
# The kubelet of the nodes (a sysext of the sysext-bakery): at most the API server's
# version, at least three minors below it.
: "${E2E_KUBERNETES_VERSION:=v1.37.1}"
: "${E2E_TAILSCALE_VERSION:=v1.98.10}"
: "${E2E_FLATCAR_CHANNEL:=stable}"
: "${E2E_FLATCAR_VERSION:=4757.2.3}"
# Instance types the suite launches; the role's instance type deny must allow them.
: "${E2E_TYPE_AMD64:=aws.t3a.small}"
: "${E2E_TYPE_ARM64:=aws.t4g.small}"
: "${E2E_TYPE_DB:=aws.r6a.large}"
# The types the described-not-named scenario may pick from (small to large).
: "${E2E_TYPES_DESCRIBED:=aws.t3a.small,aws.t3a.medium,aws.t3a.large}"
# How long a node may take from its Pod to Running, and the exit criterion for
# scale to zero (seconds from the Pod's deletion to every instance terminated).
: "${E2E_NODE_TIMEOUT:=720}"
# The one knob to tune after the first live run: the roadmap says 2 minutes. The scenario
# also notes how long it took until every instance was shutting down or gone, and until
# every one was terminated.
: "${E2E_SCALE_DOWN_DEADLINE:=120}"
# One refresh of the quota usage (the chart values set the same interval) plus a
# margin for the debounce and the API.
: "${E2E_QUOTA_REFRESH:=30}"
# Optional scenarios, off unless set: a type with local disks (d or gd families).
: "${E2E_LOCAL_STORAGE_TYPE:=}"
# The tailnet tags (docs/how-to-guides/testing.md#the-l5-lane).
: "${E2E_TAG_RUNNER:=tag:scarpenter-ci}"
: "${E2E_TAG_NODE:=tag:scarpenter-ci-node}"

# The list of described types as a YAML flow sequence for the NodePool manifest.
IFS=, read -ra _described <<<"${E2E_TYPES_DESCRIBED}"
E2E_TYPES_DESCRIBED_YAML="$(printf '"%s", ' "${_described[@]}")"
E2E_TYPES_DESCRIBED_YAML="${E2E_TYPES_DESCRIBED_YAML%, }"

# envsubst (manifests) and the child processes read exported variables.
export AWS_REGION E2E_K3S_VERSION E2E_KUBERNETES_VERSION E2E_TAILSCALE_VERSION E2E_FLATCAR_CHANNEL \
  E2E_FLATCAR_VERSION E2E_TYPE_AMD64 E2E_TYPE_ARM64 E2E_TYPE_DB E2E_TYPES_DESCRIBED E2E_TYPES_DESCRIBED_YAML \
  E2E_NODE_TIMEOUT E2E_SCALE_DOWN_DEADLINE E2E_QUOTA_REFRESH E2E_LOCAL_STORAGE_TYPE E2E_TAG_RUNNER E2E_TAG_NODE
