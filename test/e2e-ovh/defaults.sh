#!/usr/bin/env bash
# The pins of the OVH L5 lane. Every value can be overridden through the environment
# (the workflow's inputs do it for the minimum-Kubernetes run).
# shellcheck disable=SC2034  # used by the scripts that source lib.sh

# The region of the run: the Keystone catalog's name, the only region activated in the
# lane's project. The standard OpenStack variable, so the openstack CLI and gophercloud
# read it too.
: "${OS_REGION_NAME:=GRA11}"
: "${OS_AUTH_URL:=https://auth.cloud.ovh.net/v3}"
: "${OS_USER_DOMAIN_NAME:=Default}"
: "${E2E_K3S_VERSION:=v1.37.2+k3s1}"
# The kubelet of the nodes (a sysext of the sysext-bakery): at most the API server's
# version, at least three minors below it.
: "${E2E_KUBERNETES_VERSION:=v1.37.1}"
: "${E2E_TAILSCALE_VERSION:=v1.98.10}"
: "${E2E_FLATCAR_CHANNEL:=stable}"
: "${E2E_FLATCAR_VERSION:=4757.2.3}"
# Flavors the suite launches (the vendor prefix is the instance type's; the Nova flavor
# name is Keeper's api_reference). d2-2 is the Discovery family (shared CPU, the cheapest);
# the database and its client use two nodes of b3-8. The project's flavor quota is the
# ceiling: no IAM condition limits flavors.
: "${E2E_TYPE_AMD64:=ovh.d2-2}"
: "${E2E_TYPE_DB:=ovh.b3-8}"
# The types the described-not-named scenario may pick from (small to large): the request
# is 2 vCPUs and more than 3000 MiB, which rules out d2-2.
: "${E2E_TYPES_DESCRIBED:=ovh.d2-2,ovh.d2-4,ovh.b3-8}"
# How long a node may take from its Pod to Running, and the exit criterion for
# scale to zero (seconds from the Pod's deletion to every server deleted).
: "${E2E_NODE_TIMEOUT:=720}"
# A node's NodeClaim must be launched (the server created) this long after a wait began,
# or the wait fails at once as "never launched" instead of running into E2E_NODE_TIMEOUT.
# A progress block is printed to the job log this often during a wait.
: "${E2E_LAUNCH_TIMEOUT:=180}"
: "${E2E_PROGRESS_INTERVAL:=30}"
# After the first failed scenario the remaining ones are skipped and the run ends, so
# that the diagnostics and the teardown start at once; false runs every scenario.
: "${E2E_FAIL_FAST:=true}"
# The roadmap says 2 minutes. OpenStack deletes are asynchronous (DELETING); the scenario
# notes how long it took until every server was deleting or gone, and until every one was gone.
: "${E2E_SCALE_DOWN_DEADLINE:=180}"
# One refresh of the quota usage (the chart values set the same interval) plus a
# margin for the debounce and the API.
: "${E2E_QUOTA_REFRESH:=30}"
# Optional scenarios, off unless set: OVH has no local-storage profile (unsupported on
# purpose), so the lane has none; the name stays for the workflow's input.
: "${E2E_LOCAL_STORAGE_TYPE:=}"
# The scenarios of the smoke mode (docs/how-to-guides/testing-and-releasing.md): the
# shortest path through the node path (the preflight always runs first, then one on-demand
# node from the pods to Ready, then scale to zero).
: "${E2E_SMOKE_SCENARIOS:=amd64,scalezero}"
# What the suite needs of the project's quota in the region: the cores of the Nova limits
# (the preflight and the canary check them) and the instances (the canary).
: "${E2E_MIN_VCPU_OD:=16}"
: "${E2E_MIN_INSTANCES:=8}"
# The tailnet tags (docs/how-to-guides/testing.md#the-l5-lane).
: "${E2E_TAG_RUNNER:=tag:scarpenter-ci}"
: "${E2E_TAG_NODE:=tag:scarpenter-ci-node}"

# The list of described types as a YAML flow sequence for the NodePool manifest.
IFS=, read -ra _described <<<"${E2E_TYPES_DESCRIBED}"
E2E_TYPES_DESCRIBED_YAML="$(printf '"%s", ' "${_described[@]}")"
E2E_TYPES_DESCRIBED_YAML="${E2E_TYPES_DESCRIBED_YAML%, }"

# envsubst (manifests) and the child processes read exported variables.
export OS_REGION_NAME OS_AUTH_URL OS_USER_DOMAIN_NAME E2E_K3S_VERSION E2E_KUBERNETES_VERSION E2E_TAILSCALE_VERSION \
  E2E_FLATCAR_CHANNEL E2E_FLATCAR_VERSION E2E_TYPE_AMD64 E2E_TYPE_DB E2E_TYPES_DESCRIBED E2E_TYPES_DESCRIBED_YAML \
  E2E_NODE_TIMEOUT E2E_LAUNCH_TIMEOUT E2E_PROGRESS_INTERVAL E2E_FAIL_FAST E2E_SCALE_DOWN_DEADLINE E2E_QUOTA_REFRESH \
  E2E_LOCAL_STORAGE_TYPE E2E_SMOKE_SCENARIOS E2E_MIN_VCPU_OD E2E_MIN_INSTANCES E2E_TAG_RUNNER E2E_TAG_NODE
