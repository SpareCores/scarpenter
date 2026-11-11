#!/usr/bin/env bash
# Helpers of the L5 lane (UpCloud e2e over Tailscale): docs/how-to-guides/testing.md#the-l5-lane.
# Source it; it only defines functions and constants. Nothing here is run by `make
# test`: the scripts that use it talk to a real UpCloud account and Tailscale, and only
# the e2e workflow (vendor upcloud) runs them. `selftest.sh` runs the pure helpers against stubs.
#
# The environment the workflow (and a developer who runs the lane by hand with
# their own account) provides:
#
#   E2E_CLUSTER             cluster name, scarpenter-ci-<run id>
#   E2E_RUN_ID              the workflow run id (a unique suffix)
#   E2E_CREATED_AT          Unix seconds of the start
#   UPCLOUD_TOKEN           the API token of the CI sub-account (a secret; never printed)
#   UPCLOUD_ZONE            the zone of the run (fi-hel1): UpCloud's regions are its zones
#   KUBECONFIG              the k3s cluster
#   E2E_OUT                 directory for logs and the report (default test/e2e-upcloud/.out)
#
# Pins (defaults in defaults.sh).

# shellcheck disable=SC2034  # constants used by the scripts that source this file

e2e_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
e2e_root="$(cd "${e2e_dir}/../.." && pwd)"
# shellcheck source=test/lib.sh
source "${e2e_root}/test/lib.sh"
# shellcheck source=test/e2e-upcloud/defaults.sh
source "${e2e_dir}/defaults.sh"

ns="scarpenter-system"
ctag="scarpenter-cluster" # the label of every UpCloud resource of the cluster (upcloud.TagKey)
label_domain="scarpenter.sparecores.com"

# ------------------------------------------------------------ redaction -----

# redact: stdin to stdout without what must not be published (the repository, its job
# logs and its artifacts are public): tailnet host names, Tailscale auth
# keys, UpCloud API tokens, JWTs, Authorization and
# Bearer credentials, kubeconfig key material, e-mail addresses and bootstrap tokens.
# The one place for it: diagnostics.sh and the live output of observe.sh use it, and
# selftest.sh tests it. Review a file before sharing it anyway.
read -r -d '' _redact_rules <<'SED' || true
s#(authorization["']?[:=] *["']?)((bearer|basic|token) +)?[^ ,"']+#\1<redacted>#Ig
s#(bearer +)[A-Za-z0-9._~+/=-]+#\1<redacted>#Ig
s#((client-key|client-certificate|certificate-authority)-data: *)[A-Za-z0-9+/=]+#\1<redacted>#g
s#(secret|session_?token|api_?token|password)(["']?[:= ]+["']?)[^ ,"'}]+#\1\2<redacted>#Ig
s/ucat_[A-Za-z0-9_-]{16,}/<upcloud-token>/g
s/(UPCLOUD_TOKEN|UPCLOUD_PASSWORD)=[^ ]+/\1=<redacted>/g
s/tskey-[A-Za-z0-9_-]+/<tskey>/g
s/[A-Za-z0-9-]+\.ts\.net/<tailnet-host>/g
s/eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*/<jwt>/g
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

# ------------------------------------------------------------- UpCloud -----

upcloud_api="${UPCLOUD_API:-https://api.upcloud.com/1.3}"

# up <METHOD> <path> [curl arguments]: one call of the UpCloud API with the bearer token. The
# token goes to curl through a config file on a process substitution (printf is a builtin):
# it appears neither in a process list nor in the log. A non-2xx answer fails the call.
up() {
  local method="$1" path="$2"
  shift 2
  curl -fsS --retry 3 --max-time 60 -X "${method}" -K <(printf 'header = "Authorization: Bearer %s"\n' "${UPCLOUD_TOKEN}") \
    -H 'Content-Type: application/json' "$@" "${upcloud_api}${path}"
}

# up_json <METHOD> <path> <body>: with a JSON body.
up_json() { up "$1" "$2" --data-binary "$3"; }

# The list calls answer {"servers":{"server":[...]}} and the like; these read them.
cluster_label_filter() { printf 'label=%s=%s' "${ctag}" "${E2E_CLUSTER}"; }

# servers_json [label filter]: the server list (summaries without labels), all of the account
# without a filter.
servers_json() { up GET "/server${1:+?label=$1}" | jq -c '.servers.server // []'; }

# cluster_servers [states]: UUIDs of the cluster's servers in the states (comma separated;
# default: every state; a deleted server is gone from the list, UpCloud has no terminated state).
cluster_servers() {
  local states="${1:-}"
  servers_json "${ctag}=${E2E_CLUSTER}" | jq -r --arg s "${states}" \
    '.[] | select($s == "" or (.state as $x | ($s | split(",") | index($x)) != null)) | .uuid'
}

server_json() { up GET "/server/$1" | jq -c '.server'; } # server_json <uuid>

# label_of <json of a resource> <key>: the value of a label (the API spells them
# {"labels":{"label":[{"key":..,"value":..}]}}; a request takes a plain array).
label_of() {
  jq -r --arg k "$2" '(.labels.label // .labels // [])[] | select(.key == $k) | .value' <<<"$1"
}

# storage_json <uuid>
storage_json() { up GET "/storage/$1" | jq -c '.storage'; }

# cluster_storages: UUIDs of the cluster's private disks (templates are never ours).
cluster_storages() {
  up GET "/storage/private?label=${ctag}=${E2E_CLUSTER}" | jq -r '.storages.storage[]? | select(.type != "template") | .uuid'
}

# cluster_networks, cluster_rulesets, cluster_groups: UUIDs of the cluster's SDN networks,
# firewall rulesets and server groups. The ruleset list has no label filter: the labels are
# read from the answer where it carries them, else by name (the managed ones are named
# scarpenter-<cluster>-...).
cluster_networks() {
  up GET "/network?label=${ctag}=${E2E_CLUSTER}" | jq -r '.networks.network[]? | select(.type == "private") | .uuid'
}
cluster_rulesets() {
  up GET "/firewall-ruleset" | jq -r --arg c "${E2E_CLUSTER}" --arg k "${ctag}" '
    (if type == "array" then . else (.firewall_rulesets // []) end)[]
    | select(((.labels // []) | map(select(.key == $k and .value == $c)) | length > 0) or ((.name // "") | startswith("scarpenter-" + $c)))
    | .uuid'
}
cluster_groups() {
  up GET "/server-group?label=${ctag}=${E2E_CLUSTER}" | jq -r '(if type == "array" then . else (.server_groups.server_group // []) end)[]? | .uuid'
}

# up_exists <path>: prints the path when it answers 200, nothing for 404; any other answer
# (a throttle, an expired token) is an error, so that it never passes for "gone".
up_exists() {
  local code
  code="$(curl -sS --max-time 60 -o /dev/null -w '%{http_code}' -K <(printf 'header = "Authorization: Bearer %s"\n' "${UPCLOUD_TOKEN}") \
    "${upcloud_api}$1")" || return 1
  case "${code}" in
    200) echo "$1" ;;
    404) ;;
    *) echo "unexpected HTTP ${code} for $1" >&2; return 1 ;;
  esac
}

# servers_existing <uuid>...: those of the servers that still exist.
servers_existing() {
  local id
  for id in "$@"; do up_exists "/server/${id}" || return 1; done
}

# storage_existing <uuid>: the path when the storage still exists.
storage_existing() { up_exists "/storage/$1"; }

# The details of everything of the cluster, as JSON arrays (diagnostics).
cluster_server_details() {
  local id
  for id in $(cluster_servers); do server_json "${id}"; done | jq -s '.'
}
cluster_storage_details() {
  local id
  for id in $(cluster_storages); do storage_json "${id}"; done | jq -s '.'
}

# account_limits: the sub-account's resource limits (the cost guard of the lane).
account_limits() { up GET /account | jq -c '.account.resource_limits'; }

# byo_networks, byo_rulesets: what the bring-your-own scenario created (labelled
# scarpenter-e2e-byo=<cluster>; the ruleset list has no label filter, so a ruleset is found
# by its name scarpenter-<cluster>-byo, which is also what cluster_rulesets matches).
byo_networks() {
  up GET "/network?label=scarpenter-e2e-byo=${E2E_CLUSTER}" | jq -r '.networks.network[]? | select(.type == "private") | .uuid'
}
byo_rulesets() {
  up GET "/firewall-ruleset" | jq -r --arg n "scarpenter-${E2E_CLUSTER}-byo" \
    '(if type == "array" then . else (.firewall_rulesets // []) end)[] | select(.name == $n) | .uuid'
}

# delete_brought: deletes them; success when they are gone (a ruleset still attached or a
# network with servers is retried for two minutes: the servers are being deleted).
delete_brought() {
  local id deadline=$((SECONDS + 120)) rc=0
  for id in $(byo_rulesets); do
    until up DELETE "/firewall-ruleset/${id}" >/dev/null 2>&1; do
      [[ "${SECONDS}" -lt "${deadline}" ]] || { rc=1; break; }
      sleep 5
    done
  done
  for id in $(byo_networks); do
    until up DELETE "/network/${id}" >/dev/null 2>&1; do
      [[ "${SECONDS}" -lt "${deadline}" ]] || { rc=1; break; }
      sleep 5
    done
  done
  return "${rc}"
}

# ---------------------------------------------------------- Kubernetes ------

kjson() { kubectl get "$@" -o json; }

# pod_node <pod name> [namespace]
pod_node() { kubectl -n "${2:-default}" get pod "$1" -o jsonpath='{.spec.nodeName}'; }

# claim_of_node <node>: the NodeClaim that registered as the node.
claim_of_node() {
  kjson nodeclaims | jq -r --arg n "$1" '.items[] | select(.status.nodeName == $n) | .metadata.name'
}

# instance_of_claim <claim>: the server UUID from the driver's annotation
# (scarpenter.sparecores.com/instance-ref = <zone>/<server uuid>).
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

# metric_value <metrics text file> <metric> <bucket>: the value of a quota series of the
# bucket, empty when it does not exist (exposition format: name{labels} value). UpCloud's
# limits are account-wide: the series carries whatever region label the tracker gives a
# global bucket, so only the bucket is matched (the name is matched up to the brace, so that
# scarpenter_quota_used does not match scarpenter_quota_used_total).
metric_value() {
  awk -v n="$2" -v b="$3" '
    index($0, n "{") == 1 && index($0, "bucket=\"" b "\"") { print $NF; exit }' "$1"
}

# num <value>: an integer from a Prometheus number (5, 5.0, 5e+00).
num() { awk -v v="$1" 'BEGIN { printf "%d", v + 0.5 }'; }

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

# shellcheck source=test/e2e-upcloud/observe.sh
source "${e2e_dir}/observe.sh"
