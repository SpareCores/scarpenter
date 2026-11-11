#!/usr/bin/env bash
# Helpers of the L5 lane (Vultr e2e over Tailscale): docs/how-to-guides/testing.md#the-l5-lane.
# Source it; it only defines functions and constants. Nothing here is run by `make
# test`: the scripts that use it talk to a real Vultr account and Tailscale, and only
# the e2e workflow runs them. `selftest.sh` runs the pure helpers against stubs.
#
# The environment the workflow (and a developer who runs the lane by hand with
# their own account) provides:
#
#   E2E_CLUSTER             cluster name, scarpenter-ci-<run id>
#   E2E_RUN_ID              the workflow run id (a unique suffix)
#   E2E_CREATED_AT          Unix seconds of the start; tags every resource (sweeper)
#   VULTR_API_KEY           the API key of the CI account (a dedicated sub-user)
#   VULTR_REGION            the region of the run (ewr)
#   KUBECONFIG              the k3s cluster
#   E2E_OUT                 directory for logs and the report (default test/e2e-vultr/.out)
#
# Pins (defaults in defaults.sh).

# shellcheck disable=SC2034  # constants used by the scripts that source this file

e2e_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
e2e_root="$(cd "${e2e_dir}/../.." && pwd)"
# shellcheck source=test/lib.sh
source "${e2e_root}/test/lib.sh"
# shellcheck source=test/e2e-vultr/defaults.sh
source "${e2e_dir}/defaults.sh"

ns="scarpenter-system"
ctag="scarpenter.sparecores.com/cluster"
label_domain="scarpenter.sparecores.com"
vultr_api="${VULTR_API_URL:-https://api.vultr.com/v2}"

# ------------------------------------------------------------ redaction -----

# redact: stdin to stdout without what must not be published (the repository, its job
# logs and its artifacts are public): tailnet host names, Tailscale auth
# keys, the Vultr API key and instance passwords, JWTs, Authorization and
# Bearer credentials, kubeconfig key material, e-mail addresses and bootstrap tokens.
# The one place for it: diagnostics.sh and the live output of observe.sh use it, and
# selftest.sh tests it. Review a file before sharing it anyway.
read -r -d '' _redact_rules <<'SED' || true
s#(authorization["']?[:=] *["']?)((bearer|basic|token) +)?[^ ,"']+#\1<redacted>#Ig
s#(bearer +)[A-Za-z0-9._~+/=-]+#\1<redacted>#Ig
s#((client-key|client-certificate|certificate-authority)-data: *)[A-Za-z0-9+/=]+#\1<redacted>#g
s#(secret_?access_?key|session_?token)(["']?[:= ]+["']?)[^ ,"'}]+#\1\2<redacted>#Ig
s#(VULTR_API_KEY|api_?key)(["']?[:= ]+["']?)[A-Za-z0-9]{20,}#\1\2<redacted>#Ig
s#(default_password|root_pass)(["']?[:= ]+["']?)[^ ,"'}]+#\1\2<redacted>#Ig
s/tskey-[A-Za-z0-9_-]+/<tskey>/g
s/[A-Za-z0-9-]+\.ts\.net/<tailnet-host>/g
s/eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*/<jwt>/g
s/(VULTR_API_KEY)=[^ ]+/\1=<redacted>/g
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

# --------------------------------------------------------------- Vultr -------

# vapi <path> [curl args...]: a call of the Vultr API (v2). The key goes through a curl
# config on a pipe, so that it is in no process list.
vapi() {
  local path="$1"
  shift
  curl -fsS --retry 3 --retry-delay 2 --max-time 60 \
    --config <(printf 'header = "Authorization: Bearer %s"\n' "${VULTR_API_KEY}") "$@" "${vultr_api}${path}"
}

# vapi_post <path> <json body>, vapi_delete <path>
vapi_post() { vapi "$1" -X POST -H 'Content-Type: application/json' -d "$2"; }
vapi_delete() { vapi "$1" -X DELETE; }

# uri <text>: percent-encoded.
uri() { jq -rn --arg t "$1" '$t | @uri'; }

# cluster_instances: IDs of the cluster's instances (a deleted instance is gone from the
# list: Vultr has no terminated state).
cluster_instances() {
  vapi "/instances?per_page=500&tag=$(uri "${ctag}=${E2E_CLUSTER}")" | jq -r '.instances[].id'
}

instance_json() { vapi "/instances/$1" | jq '.instance'; }

# tag_of <instance json> <logical tag key>: the value of the tag string "<domain>/<key>=<value>"
# (or a plain key=value tag of the NodeClass's spec.tags).
tag_of() {
  jq -r --arg p "$2=" '(.tags // [])[] | select(startswith($p)) | .[($p | length):]' <<<"$1"
}

# groups_json / vpcs_json: the account's firewall groups and VPCs (no tags: the description
# carries the ownership marker "<name> | cluster=<c> nodeclass=<n> managed-by=scarpenter
# resource-kind=<k> region=<r>").
groups_json() { vapi '/firewalls?per_page=500' | jq '.firewall_groups'; }
vpcs_json() { vapi '/vpcs?per_page=500' | jq '.vpcs'; }

# managed_of <groups|vpcs> [nodeclass]: the objects SCarpenter created for the cluster
# (and the NodeClass), as a JSON array.
managed_of() {
  local list="${1}_json" class="${2:-}"
  "${list}" | jq --arg c "cluster=${E2E_CLUSTER} " --arg n "${class:+nodeclass=${class} }" --arg m "managed-by=scarpenter" \
    '[.[] | select(.description | contains($c) and contains($m) and contains($n))]'
}

# ---------------------------------------------------------- Kubernetes ------

kjson() { kubectl get "$@" -o json; }

# pod_node <pod name> [namespace]
pod_node() { kubectl -n "${2:-default}" get pod "$1" -o jsonpath='{.spec.nodeName}'; }

# claim_of_node <node>: the NodeClaim that registered as the node.
claim_of_node() {
  kjson nodeclaims | jq -r --arg n "$1" '.items[] | select(.status.nodeName == $n) | .metadata.name'
}

# instance_of_claim <claim>: the Vultr instance ID from the driver's annotation
# (scarpenter.sparecores.com/instance-ref = <region>/<instance id>).
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

# ------------------------------------------------------------ scenarios -----

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

# shellcheck source=test/e2e-vultr/observe.sh
source "${e2e_dir}/observe.sh"
