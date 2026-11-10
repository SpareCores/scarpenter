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
# A node's NodeClaim must be launched (the cloud instance created) this long after a wait
# began, or the wait fails at once as "never launched" instead of running into
# E2E_NODE_TIMEOUT. A progress block (NodeClaims, Pods, events, controller errors) is
# printed to the job log this often during a wait.
: "${E2E_LAUNCH_TIMEOUT:=180}"
: "${E2E_PROGRESS_INTERVAL:=30}"
# After the first failed scenario the remaining ones are skipped and the run ends, so
# that the diagnostics and the teardown start at once; false runs every scenario.
: "${E2E_FAIL_FAST:=true}"
# The one knob to tune after the first live run: the roadmap says 2 minutes. The scenario
# also notes how long it took until every instance was shutting down or gone, and until
# every one was terminated.
: "${E2E_SCALE_DOWN_DEADLINE:=120}"
# One refresh of the quota usage (the chart values set the same interval) plus a
# margin for the debounce and the API.
: "${E2E_QUOTA_REFRESH:=30}"
# Optional scenarios, off unless set: a type with local disks (d or gd families).
: "${E2E_LOCAL_STORAGE_TYPE:=}"
# The scenarios of the smoke mode (docs/how-to-guides/testing-and-releasing.md): the
# shortest path through the node path (the preflight always runs first, then one on-demand
# node from the pods to Ready, then scale to zero). Standalone-safe: amd64 needs only its own
# workload, and scalezero reads the amd64 node's state when it exists and checks the
# cluster-wide exit criterion whatever ran before. Order is the runner's, not this list's.
: "${E2E_SMOKE_SCENARIOS:=amd64,scalezero}"
# The vCPU quotas of the Standard class the suite needs (the preflight and the canary check them).
: "${E2E_MIN_VCPU_OD:=16}"
: "${E2E_MIN_VCPU_SPOT:=4}"
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
  E2E_NODE_TIMEOUT E2E_LAUNCH_TIMEOUT E2E_PROGRESS_INTERVAL E2E_FAIL_FAST E2E_SCALE_DOWN_DEADLINE E2E_QUOTA_REFRESH E2E_LOCAL_STORAGE_TYPE E2E_SMOKE_SCENARIOS E2E_MIN_VCPU_OD E2E_MIN_VCPU_SPOT E2E_TAG_RUNNER E2E_TAG_NODE
