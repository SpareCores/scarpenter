#!/usr/bin/env bash
# Helpers of the L5 lane (OVH e2e over Tailscale): docs/how-to-guides/testing.md#the-l5-lane.
# Source it; it only defines functions and constants. Nothing here is run by `make
# test`: the scripts that use it talk to a real OVH project and Tailscale, and only
# the e2e workflow (vendor ovh) runs them. `selftest.sh` runs the pure helpers against stubs.
#
# The environment the workflow (and a developer who runs the lane by hand with
# their own account) provides:
#
#   E2E_CLUSTER             cluster name, scarpenter-ci-<run id>
#   E2E_RUN_ID              the workflow run id (a unique suffix)
#   E2E_CREATED_AT          Unix seconds of the start; tags every resource (sweeper)
#   OS_REGION_NAME         the region of the run, a name of the project's Keystone catalog (GRA11)
#   OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, OS_PROJECT_ID, OS_USER_DOMAIN_NAME
#                           the OpenStack user of the lane's project (the openstack CLI and the
#                           controller read the same variables)
#   KUBECONFIG              the k3s cluster
#   E2E_OUT                 directory for logs and the report (default test/e2e-ovh/.out)
#
# Pins (defaults in defaults.sh).

# shellcheck disable=SC2034  # constants used by the scripts that source this file

e2e_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
e2e_root="$(cd "${e2e_dir}/../.." && pwd)"
# shellcheck source=test/lib.sh
source "${e2e_root}/test/lib.sh"
# shellcheck source=test/e2e-ovh/defaults.sh
source "${e2e_dir}/defaults.sh"

ns="scarpenter-system"
ctag="scarpenter.sparecores.com/cluster"
label_domain="scarpenter.sparecores.com"
# Nova microversion 2.67: server tags (2.26) and the block device mapping volume type, as the driver asks.
export OS_COMPUTE_API_VERSION=2.67

# ------------------------------------------------------------ redaction -----

# redact: stdin to stdout without what must not be published (the repository, its job
# logs and its artifacts are public): the OpenStack project and user IDs, passwords and
# tokens, tailnet host names, Tailscale auth keys, JWTs, Authorization and Bearer
# credentials, kubeconfig key material, e-mail addresses and bootstrap tokens.
# The one place for it: diagnostics.sh and the live output of observe.sh use it, and
# selftest.sh tests it. Review a file before sharing it anyway.
read -r -d '' _redact_rules <<'SED' || true
s#(authorization["']?[:=] *["']?)((bearer|basic|token) +)?[^ ,"']+#\1<redacted>#Ig
s#(bearer +)[A-Za-z0-9._~+/=-]+#\1<redacted>#Ig
s#((client-key|client-certificate|certificate-authority)-data: *)[A-Za-z0-9+/=]+#\1<redacted>#g
s#(password|secret|session_?token|x-auth-token|x-subject-token)(["']?[:= ]+["']?)[^ ,"'}]+#\1\2<redacted>#Ig
s/gAAAA[A-Za-z0-9_=-]{20,}/<os-token>/g
s/\b[0-9a-f]{32}\b/<os-id>/g
s/tskey-[A-Za-z0-9_-]+/<tskey>/g
s/[A-Za-z0-9-]+\.ts\.net/<tailnet-host>/g
s/eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*/<jwt>/g
s/(OS_PASSWORD|OS_USERNAME|OS_PROJECT_ID)=[^ ]+/\1=<redacted>/g
s/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/<email>/g
s/\b[a-z0-9]{6}\.[a-z0-9]{16}\b/<bootstrap-token>/g
SED
redact() { sed -E -e "${_redact_rules}"; }

# need <VAR>...: the variables must be set and non-empty.
need() {
  local v
  for v in "$@"; do
    [[ -n "${!v:-}" ]] || {
      echo "the variable ${v} is required" >&2
      exit 2
    }
  done
}

now() { date +%s; }

# fail <message>: ends the running scenario (they run in subshells).
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# note <message>: something to read in the report that is not a failure.
note() {
  echo "NOTE: $*"
  [[ -z "${E2E_OUT:-}" ]] || echo "$*" >>"${E2E_OUT}/notes.txt"
}

assert_eq() { # assert_eq <what> <want> <got>
  [[ "$2" == "$3" ]] || fail "$1: want '$2', got '$3'"
  echo "ok: $1 = $2"
}

assert_nonempty() { # assert_nonempty <what> <value>
  [[ -n "$2" && "$2" != "null" ]] || fail "$1 is empty"
  echo "ok: $1 = $2"
}

assert_match() { # assert_match <what> <extended regexp> <got>
  [[ "$3" =~ $2 ]] || fail "$1: '$3' does not match $2"
  echo "ok: $1 = $3"
}

# wait_until <what> <timeout seconds> <command...>: polls every 3 s; on timeout
# prints the last output of the command and ends the scenario.
wait_until() {
  local what="$1" timeout="$2" deadline last
  shift 2
  deadline=$((SECONDS + timeout))
  while true; do
    if last="$("$@" 2>&1)"; then
      echo "ok: ${what} (after $((timeout - (deadline - SECONDS)))s)"
      return 0
    fi
    if [[ "${SECONDS}" -ge "${deadline}" ]]; then
      echo "last output: ${last}" >&2
      fail "timed out after ${timeout}s waiting for ${what}"
    fi
    sleep 3
  done
}

# --------------------------------------------------------------- OVH --------

# os <openstack arguments>: the OpenStack CLI in the lane's region; -f json for machines.
os() { openstack --os-region-name "${OS_REGION_NAME}" "$@"; }

# The Nova server tag of a cluster (the driver's ClusterTag: scarpenter-cluster=<cluster>, at
# most 60 characters), which lists the cluster's servers server side.
cluster_tag() { printf 'scarpenter-cluster=%s' "${E2E_CLUSTER}"; }

# cluster_instances [status regexp]: IDs of the cluster's servers whose status matches
# (default: every server that is listed, which excludes the deleted ones).
cluster_instances() {
  os server list --tags "$(cluster_tag)" -f json | jq -r --arg re "${1:-.}" '.[] | select(.Status | test($re)) | .ID'
}

instance_json() { # instance_json <server id>: the server, as `server show -f json`
  os server show "$1" -f json
}

# meta_of <server json> <metadata key>: the server's metadata value.
meta_of() { jq -r --arg k "$2" '(.properties // {})[$k] // empty' <<<"$1"; }

# flavor_of <server json>: the flavor name (the CLI shows "name (id)" or the embedded flavor).
flavor_of() { jq -r '(if (.flavor | type) == "object" then (.flavor.original_name // .flavor.name // "") else (.flavor // "") end) | tostring | split(" ")[0]' <<<"$1"; }

# limit_of <name>: a Nova absolute limit of the project in the region (maxTotalCores, totalCoresUsed, ...).
limit_of() { os limits show --absolute -f json | jq -r --arg n "$1" '.[] | select(.Name == $n) | .Value'; }

# ---------------------------------------------------------- Kubernetes ------

kjson() { kubectl get "$@" -o json; }

# pod_node <pod name> [namespace]
pod_node() { kubectl -n "${2:-default}" get pod "$1" -o jsonpath='{.spec.nodeName}'; }

# claim_of_node <node>: the NodeClaim that registered as the node.
claim_of_node() {
  kjson nodeclaims | jq -r --arg n "$1" '.items[] | select(.status.nodeName == $n) | .metadata.name'
}

# instance_of_claim <claim>: the server ID from the driver's annotation
# (scarpenter.sparecores.com/instance-ref = <region>/<server id>).
instance_of_claim() {
  kubectl get nodeclaim "$1" -o json | jq -r --arg k "${label_domain}/instance-ref" '.metadata.annotations[$k] // empty' | sed 's#.*/##'
}

node_label() { # node_label <node> <label>
  kubectl get node "$1" -o json | jq -r --arg k "$2" '.metadata.labels[$k] // empty'
}

# ready_seconds <claim> <node>: seconds from the NodeClaim's creation to the node's Ready.
ready_seconds() {
  local created ready
  created="$(kubectl get nodeclaim "$1" -o jsonpath='{.metadata.creationTimestamp}')"
  ready="$(kubectl get node "$2" -o json | jq -r '.status.conditions[] | select(.type == "Ready") | .lastTransitionTime')"
  echo $(($(date -d "${ready}" +%s) - $(date -d "${created}" +%s)))
}

# probe <name> <pool> <instance type> <capacity type>: a one-replica Deployment
# pinned to a NodePool and an exact type (manifests/probe.yaml).
probe() {
  NAME="$1" POOL="$2" TYPE="$3" CAPACITY="$4" render "${e2e_dir}/manifests/probe.yaml" | kubectl apply -f -
}

# render <file>: envsubst with the variables the manifests use, and nothing else
# (an unset one is an error, not an empty string).
render() {
  local -a vars
  local v list=""
  # shellcheck disable=SC2016  # the pattern is a literal ${NAME}
  mapfile -t vars < <(grep -o '\${[A-Z0-9_]*}' "$1" | sort -u | tr -d '${}' || true)
  for v in "${vars[@]}"; do
    [[ -n "${!v+x}" ]] || {
      echo "render $1: ${v} is not set" >&2
      return 1
    }
    # shellcheck disable=SC2016  # envsubst takes the names as ${NAME}
    list+="$(printf '${%s} ' "${v}")"
  done
  envsubst "${list}" <"$1"
}

pod_running() { # pod_running <label selector> [namespace]: every pod of the selector Running
  local out
  out="$(kubectl -n "${2:-default}" get pods -l "$1" -o json)"
  [[ "$(jq '.items | length' <<<"${out}")" -gt 0 ]] &&
    [[ "$(jq '[.items[] | select(.status.phase != "Running")] | length' <<<"${out}")" == 0 ]]
}

# node_shell <node> <command>: runs a command in the host's namespaces and root
# directory of the node (a privileged Pod with the host's root mounted, chroot),
# for the checks that look at systemd and files. Over kubectl exec, so it proves
# the exec path as well.
node_shell() {
  local node="$1" pod="nodeshell-$1"
  if ! kubectl get pod "${pod}" >/dev/null 2>&1; then
    NODE="${node}" PODNAME="${pod}" render "${e2e_dir}/manifests/nodeshell.yaml" | kubectl apply -f - >/dev/null
    kubectl wait --for=condition=Ready "pod/${pod}" --timeout=240s >/dev/null
  fi
  kubectl exec "${pod}" -- chroot /host sh -c "$2"
}

# ------------------------------------------------------------- metrics ------

# metrics: the controller's metrics, through the API server's service proxy.
metrics() { kubectl get --raw "/api/v1/namespaces/${ns}/services/scarpenter:metrics/proxy/metrics"; }

# metric_value <metrics text file> <metric> <region> <bucket>: the value of a
# quota series, empty when it does not exist (exposition format: name{labels} value).
metric_value() {
  awk -v n="$2" -v r="$3" -v b="$4" '
    index($0, n "{") == 1 && index($0, "region=\"" r "\"") && index($0, "bucket=\"" b "\"") { print $NF; exit }' "$1"
}

# num <value>: an integer from a Prometheus or OpenStack number (5, 5.0, 5e+00).
num() { awk -v v="$1" 'BEGIN { printf "%d", v + 0.5 }'; }

# ------------------------------------------------------------ scenarios -----

# The quota buckets of the driver (docs/reference/ovh-driver.md#quotas): Nova's absolute limits of
# the project in the region, and the Nova name of the limit and of its usage.
quota_buckets=(instances cores ram)
declare -A quota_limit_name=([instances]=maxTotalInstances [cores]=maxTotalCores [ram]=maxTotalRAMSize)
declare -A quota_used_name=([instances]=totalInstancesUsed [cores]=totalCoresUsed [ram]=totalRAMUsed)

# is_empty <command...>: true when the command succeeds and prints nothing. A command
# that fails (expired credentials, a throttle, a wrong selector) is not "empty": the check
# fails, so that an error can never pass for "everything is gone". kubectl checks that mean
# "the object is gone" must use --ignore-not-found. The command is run at every poll,
# unlike a $(...) in the arguments of wait_until.
is_empty() {
  local out
  out="$("$@" 2>/dev/null)" || return 1
  [[ -z "${out}" ]]
}

# is_value <want> <command...>: true when the command prints exactly <want>.
is_value() {
  local want="$1"
  shift
  [[ "$("$@" 2>/dev/null)" == "${want}" ]]
}

# ------------------------------------------------- live output, failures -----

# shellcheck source=test/e2e-ovh/observe.sh
source "${e2e_dir}/observe.sh"
