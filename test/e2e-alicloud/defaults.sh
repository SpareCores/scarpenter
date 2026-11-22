#!/usr/bin/env bash
# The pins of the L5 lane of Alibaba Cloud. Every value can be overridden through the
# environment (the workflow's inputs do it for the minimum-Kubernetes run).
#
# NOTHING IN THIS LANE HAS RUN YET. It was written and validated statically (shellcheck,
# actionlint, envtest, the selftest against stubs) before an account existed; every pin
# and every cloud fact below that is marked "unverified" is settled by the first live run.
# shellcheck disable=SC2034  # used by the scripts that source lib.sh

# The region of the lane (the workflow's variable ALICLOUD_REGION). Zone IDs equal zone
# names here (ap-southeast-1a). The home region only serves the global DescribeRegions.
: "${ALICLOUD_REGION:=ap-southeast-1}"
: "${E2E_K3S_VERSION:=v1.37.2+k3s1}"
# The kubelet of the nodes (a sysext of the sysext-bakery): at most the API server's
# version, at least three minors below it.
: "${E2E_KUBERNETES_VERSION:=v1.37.1}"
: "${E2E_TAILSCALE_VERSION:=v1.98.10}"
# The Flatcar release of the images. The images are the operator's own (rung 3 of plan 21):
# the operator imports and copies them before the run, the lane's preflight only checks that
# they resolve (docs/how-to-guides/testing-and-releasing.md#alibaba-cloud-provisioning).
# The Flatcar release: test/flatcar-pin.sh (CI sets it from the repository variables FLATCAR_CHANNEL and FLATCAR_VERSION).
# shellcheck source=test/flatcar-pin.sh
source "$(dirname "${BASH_SOURCE[0]}")/../flatcar-pin.sh"
# Instance types the suite launches (ecs.<family>.<size>, with the vendor prefix); the
# role's instance type deny must allow them. Availability per zone is unverified.
#   amd64 on-demand: ecs.u1-c1m2.large (2 vCPU, 4 GiB; on-demand in ap-southeast-1a/b/c)
#   arm64 spot:      ecs.c8y.large (2 vCPU, 4 GiB, Yitian; spot in ap-southeast-1b only)
#   placement:       ecs.u1-c1m4.large (2 vCPU, 8 GiB; on-demand in ap-southeast-1a/b/c), the
#                    type of the deployment-set scenarios
#   Active types per Keeper data; the e-c1m* and g7 types have no price or no stock here.
: "${E2E_TYPE_AMD64:=alicloud.ecs.u1-c1m2.large}"
: "${E2E_TYPE_ARM64:=alicloud.ecs.c8y.large}"
: "${E2E_TYPE_DB:=alicloud.ecs.u1-c1m4.large}"
# The manual gpu scenario (never in `all` or the smoke set): the GPU type it launches (the
# cheapest supported one, 1 T4 with 4 vCPUs; the gpu_instance_type input of e2e.yml (an E2E_GPU_TYPE override) replaces
# it, and the role's instance type deny must allow it) and how long its node may take from
# the Pod to the end of the CUDA run (the driver download and the image pull come on top of
# a boot). Stock and the GPU quota of the account in ap-southeast-1 are unverified.
: "${E2E_GPU_TYPE:=alicloud.ecs.gn6i-c4g1.xlarge}"
: "${E2E_GPU_TIMEOUT:=1500}"
# The types the described-not-named scenario may pick from (small to large).
: "${E2E_TYPES_DESCRIBED:=alicloud.ecs.u1-c1m2.large,alicloud.ecs.u1-c1m4.large}"
# How long a node may take from its Pod to Running, and the exit criterion for
# scale to zero (seconds from the Pod's deletion to every instance gone).
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
# also notes how long it took until every instance was stopping or gone, and until every
# one was released.
: "${E2E_SCALE_DOWN_DEADLINE:=120}"
# One refresh of the quota usage (the chart values set the same interval) plus a
# margin for the debounce and the API.
: "${E2E_QUOTA_REFRESH:=30}"
# Optional scenarios, off unless set: a type with local disks (not supported by the driver
# yet: the variable exists for the workflow's input and the canary's config).
: "${E2E_LOCAL_STORAGE_TYPE:=}"
# The scenarios of the smoke mode (docs/how-to-guides/testing-and-releasing.md): the
# shortest path through the node path (the preflight always runs first, then one on-demand
# node from the pods to Ready, then scale to zero). Order is the runner's, not this list's.
: "${E2E_SMOKE_SCENARIOS:=amd64,scalezero}"
# The vCPU quotas the suite needs (the preflight and the canary check them): pay-as-you-go
# 16 and spot 4, the account's ceiling for the lane (DescribeAccountAttributes).
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
export ALICLOUD_REGION E2E_K3S_VERSION E2E_KUBERNETES_VERSION E2E_TAILSCALE_VERSION E2E_FLATCAR_CHANNEL \
  E2E_FLATCAR_VERSION E2E_TYPE_AMD64 E2E_TYPE_ARM64 E2E_TYPE_DB E2E_TYPES_DESCRIBED E2E_TYPES_DESCRIBED_YAML \
  E2E_NODE_TIMEOUT E2E_LAUNCH_TIMEOUT E2E_PROGRESS_INTERVAL E2E_FAIL_FAST E2E_SCALE_DOWN_DEADLINE E2E_QUOTA_REFRESH E2E_LOCAL_STORAGE_TYPE E2E_GPU_TYPE E2E_GPU_TIMEOUT E2E_SMOKE_SCENARIOS E2E_MIN_VCPU_OD E2E_MIN_VCPU_SPOT E2E_TAG_RUNNER E2E_TAG_NODE
