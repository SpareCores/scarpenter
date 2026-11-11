#!/usr/bin/env bash
# The pins of the L5 lane of Hetzner Cloud. Every value can be overridden through the
# environment (the workflow's inputs do it for the minimum-Kubernetes run).
# shellcheck disable=SC2034  # used by the scripts that source lib.sh

# The locations the lane may use (fsn1, nbg1 and hel1 have the arm types and the cheap
# ones). Hetzner cannot restrict a token to locations: this is the lane's own limit.
: "${E2E_HCLOUD_LOCATIONS:=fsn1,nbg1,hel1}"
: "${E2E_K3S_VERSION:=v1.37.2+k3s1}"
# The kubelet of the nodes (a sysext of the sysext-bakery): at most the API server's
# version, at least three minors below it.
: "${E2E_KUBERNETES_VERSION:=v1.37.1}"
: "${E2E_TAILSCALE_VERSION:=v1.98.10}"
: "${E2E_FLATCAR_CHANNEL:=stable}"
: "${E2E_FLATCAR_VERSION:=4757.2.3}"
# The tools of bootstrap.sh, installed with `go install` (the module proxy's checksum database
# verifies them). hcloud-upload-image makes the Flatcar snapshots; the hcloud CLI is what the
# scripts ask the API with. Pin the CLI after the first live run (latest until then).
: "${E2E_HCLOUD_UPLOAD_IMAGE_VERSION:=v1.5.0}"
: "${E2E_HCLOUD_CLI_VERSION:=latest}"
# Server types the suite launches (the lane's NodePools list them: the token cannot deny others).
: "${E2E_TYPE_AMD64:=hcloud.cx23}"
: "${E2E_TYPE_ARM64:=hcloud.cax11}"
: "${E2E_TYPE_DB:=hcloud.cx23}"
# The types the described-not-named scenario may pick from (two vCPUs, more than 3000 MiB).
: "${E2E_TYPES_DESCRIBED:=hcloud.cx23,hcloud.cpx22}"
# An arm type and a location that does not have it (scenario negative).
: "${E2E_NEGATIVE_LOCATION:=ash}"
# The server limit of the CI project, as the Console shows it (--hcloud-server-limit): the suite
# runs up to nine servers at once (prelude: amd64, arm64, two for db, described, byo, drift and
# the drift replacement), so ask Hetzner for at least ten.
: "${E2E_HCLOUD_SERVER_LIMIT:=10}"
# How long a node may take from its Pod to Running, and the exit criterion for
# scale to zero (seconds from the Pod's deletion to every server deleted).
: "${E2E_NODE_TIMEOUT:=720}"
# A node's NodeClaim must be launched (the server created) this long after a wait began, or the
# wait fails at once as "never launched". A progress block is printed this often during a wait.
: "${E2E_LAUNCH_TIMEOUT:=180}"
: "${E2E_PROGRESS_INTERVAL:=30}"
# After the first failed scenario the remaining ones are skipped and the run ends.
: "${E2E_FAIL_FAST:=true}"
: "${E2E_SCALE_DOWN_DEADLINE:=120}"
# The scenarios of the smoke mode: the preflight always runs first, then one on-demand node from
# the Pod to Ready, then scale to zero.
: "${E2E_SMOKE_SCENARIOS:=amd64,scalezero}"
# The tailnet tags (docs/how-to-guides/testing.md#the-l5-lane).
: "${E2E_TAG_RUNNER:=tag:scarpenter-ci}"
: "${E2E_TAG_NODE:=tag:scarpenter-ci-node}"

# The lists as YAML flow sequences for the manifests.
IFS=, read -ra _described <<<"${E2E_TYPES_DESCRIBED}"
E2E_TYPES_DESCRIBED_YAML="$(printf '"%s", ' "${_described[@]}")"
E2E_TYPES_DESCRIBED_YAML="${E2E_TYPES_DESCRIBED_YAML%, }"
IFS=, read -ra _locations <<<"${E2E_HCLOUD_LOCATIONS}"
E2E_HCLOUD_LOCATIONS_YAML="$(printf '"%s", ' "${_locations[@]}")"
E2E_HCLOUD_LOCATIONS_YAML="${E2E_HCLOUD_LOCATIONS_YAML%, }"
# The location of the bring-your-own network: the first of the lane's.
E2E_BYO_LOCATION="${_locations[0]}"

# envsubst (manifests) and the child processes read exported variables.
export E2E_HCLOUD_LOCATIONS E2E_HCLOUD_LOCATIONS_YAML E2E_BYO_LOCATION E2E_NEGATIVE_LOCATION E2E_HCLOUD_SERVER_LIMIT \
  E2E_HCLOUD_UPLOAD_IMAGE_VERSION E2E_HCLOUD_CLI_VERSION \
  E2E_K3S_VERSION E2E_KUBERNETES_VERSION E2E_TAILSCALE_VERSION E2E_FLATCAR_CHANNEL E2E_FLATCAR_VERSION \
  E2E_TYPE_AMD64 E2E_TYPE_ARM64 E2E_TYPE_DB E2E_TYPES_DESCRIBED E2E_TYPES_DESCRIBED_YAML \
  E2E_NODE_TIMEOUT E2E_LAUNCH_TIMEOUT E2E_PROGRESS_INTERVAL E2E_FAIL_FAST E2E_SCALE_DOWN_DEADLINE \
  E2E_SMOKE_SCENARIOS E2E_TAG_RUNNER E2E_TAG_NODE
