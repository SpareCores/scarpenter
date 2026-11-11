#!/usr/bin/env bash
# Helpers of the L5 lane (GCP e2e over Tailscale): docs/how-to-guides/testing.md#the-l5-lane.
# Source it; it only defines functions and constants. Nothing here is run by `make
# test`: the scripts that use it talk to a real Google Cloud project and Tailscale, and
# only the e2e workflow runs them. `selftest.sh` runs the pure helpers against stubs.
#
# The environment the workflow (and a developer who runs the lane by hand with
# their own account) provides:
#
#   E2E_CLUSTER             cluster name, scarpenter-ci-<run id>
#   E2E_RUN_ID              the workflow run id (a unique suffix)
#   E2E_CREATED_AT          Unix seconds of the start; labels every instance (sweeper)
#   GCP_PROJECT             the dedicated project of the lane
#   GCP_REGION, GCP_ZONE    the region of the run (us-central1) and its zone for the pinned scenarios
#   KUBECONFIG              the k3s cluster
#   E2E_OUT                 directory for logs and the report (default test/e2e-gcp/.out)
#
# gcloud and the clients of the controller find the credentials through Application
# Default Credentials (the workflow's google-github-actions/auth step exports them).
#
# Pins (defaults in defaults.sh).

# shellcheck disable=SC2034  # constants used by the scripts that source this file

e2e_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
e2e_root="$(cd "${e2e_dir}/../.." && pwd)"
# shellcheck source=test/lib.sh
source "${e2e_root}/test/lib.sh"
# shellcheck source=test/e2e-gcp/defaults.sh
source "${e2e_dir}/defaults.sh"

ns="scarpenter-system"
# The label every instance and disk of the cluster carries (gcp.TagKey of the cluster tag).
ctag="scarpenter-cluster"
label_domain="scarpenter.sparecores.com"
export CLOUDSDK_CORE_DISABLE_PROMPTS=1 CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true

# ------------------------------------------------------------ redaction -----

# redact: stdin to stdout without what must not be published (the repository, its job
# logs and its artifacts are public): the project id and project numbers, tailnet host
# names, Tailscale auth keys, Google OAuth access and refresh tokens, API keys, service
# account private keys, JWTs, Authorization and Bearer credentials, kubeconfig key material,
# e-mail addresses (service accounts) and bootstrap tokens. The one place for it:
# diagnostics.sh and the live output of observe.sh use it, and selftest.sh tests it.
# Review a file before sharing it anyway.
read -r -d '' _redact_rules <<'SED' || true
s#(authorization["']?[:=] *["']?)((bearer|basic|token) +)?[^ ,"']+#\1<redacted>#Ig
s#(bearer +)[A-Za-z0-9._~+/=-]+#\1<redacted>#Ig
s#-----BEGIN [A-Z ]*PRIVATE KEY-----[^-]*(-----END [A-Z ]*PRIVATE KEY-----)?#<private-key>#g
s#((client-key|client-certificate|certificate-authority)-data: *)[A-Za-z0-9+/=]+#\1<redacted>#g
s#("?(private_key|private_key_id|client_secret|refresh_token|access_token|id_token)"?[:=] *"?)[^ ,"'}]+#\1<redacted>#Ig
s#ya29\.[A-Za-z0-9._-]{10,}#<gcp-access-token>#g
s#1//[A-Za-z0-9_-]{20,}#<gcp-refresh-token>#g
s#AIza[0-9A-Za-z_-]{30,}#<gcp-api-key>#g
s#(projects/)[0-9]{6,}#\1<project-number>#g
s/\b[0-9]{12}\b/<project-number>/g
s/tskey-[A-Za-z0-9_-]+/<tskey>/g
s/[A-Za-z0-9-]+\.ts\.net/<tailnet-host>/g
s/eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*/<jwt>/g
s/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/<email>/g
s/\b[a-z0-9]{6}\.[a-z0-9]{16}\b/<bootstrap-token>/g
SED
# redact: the rules above, and the lane's project id when it is set (a literal, escaped:
# a project id holds lower-case letters, digits and hyphens only).
redact() {
  local -a extra=()
  if [[ "${GCP_PROJECT:-}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]]; then
    extra=(-e "s/${GCP_PROJECT}/<project>/g")
  fi
  sed -E -e "${_redact_rules}" "${extra[@]}"
}

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

# --------------------------------------------------------------- GCP --------

# gc <gcloud arguments>: gcloud against the lane's project, without prompts.
gc() { gcloud --project "${GCP_PROJECT}" --quiet "$@"; }

# cluster_instances [status...]: "<zone>/<name>" of the cluster's instances, one per line,
# in the statuses (default: every one; a deleted instance is gone from the API, a stopped
# one is TERMINATED, and its disks still cost).
cluster_instances() {
  local filter="labels.${ctag}=${E2E_CLUSTER}" s any=""
  for s in "$@"; do any+="${any:+ OR }status=${s}"; done
  [[ -z "${any}" ]] || filter+=" AND (${any})"
  gc compute instances list --filter="${filter}" --format='value(zone.basename(),name)' | tr '\t' '/' | sed '/^$/d'
}

# alive_instances: the instances that are not on their way out: PROVISIONING, STAGING,
# RUNNING or REPAIRING.
alive_instances() { cluster_instances PROVISIONING STAGING RUNNING REPAIRING; }

# instance_json <zone/name>
instance_json() { gc compute instances describe "${1#*/}" --zone "${1%%/*}" --format=json; }

# label_of <json of a resource with labels> <label key>
label_of() { jq -r --arg k "$2" '.labels[$k] // empty' <<<"$1"; }

# meta_keys <instance json>: the metadata keys of the instance, one per line.
meta_keys() { jq -r '(.metadata.items // [])[].key' <<<"$1"; }

# meta_value <instance json> <key>
meta_value() { jq -r --arg k "$2" '(.metadata.items // [])[] | select(.key == $k) | .value' <<<"$1"; }

# region_quota <metric>: the limit and usage of a metric of the lane's region, "limit usage".
region_quota() {
  gc compute regions describe "${GCP_REGION}" --format=json |
    jq -r --arg m "$1" '.quotas[] | select(.metric == $m) | "\(.limit) \(.usage)"'
}

# ---------------------------------------------------------- Kubernetes ------

kjson() { kubectl get "$@" -o json; }

# pod_node <pod name> [namespace]
pod_node() { kubectl -n "${2:-default}" get pod "$1" -o jsonpath='{.spec.nodeName}'; }

# claim_of_node <node>: the NodeClaim that registered as the node.
claim_of_node() {
  kjson nodeclaims | jq -r --arg n "$1" '.items[] | select(.status.nodeName == $n) | .metadata.name'
}

# instance_of_claim <claim>: "<zone>/<name>" from the driver's annotation
# (scarpenter.sparecores.com/instance-ref = <region>/<zone>/<instance name>).
instance_of_claim() {
  kubectl get nodeclaim "$1" -o json | jq -r --arg k "${label_domain}/instance-ref" '.metadata.annotations[$k] // empty' | sed 's#^[^/]*/##'
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

# num <value>: an integer from a Prometheus or gcloud number (5, 5.0, 5e+00).
num() { awk -v v="$1" 'BEGIN { printf "%d", v + 0.5 }'; }

# ------------------------------------------------------------ scenarios -----

# The quota buckets of the lane's region the controller reports (GCP gives the usage with
# the limit, regions.get): the vCPU metrics and the instance count.
standard_od="CPUS"
standard_spot="PREEMPTIBLE_CPUS"

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

# shellcheck source=test/e2e-gcp/observe.sh
source "${e2e_dir}/observe.sh"
