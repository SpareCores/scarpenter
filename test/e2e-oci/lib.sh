#!/usr/bin/env bash
# Helpers of the L5 lane (OCI e2e over Tailscale): docs/how-to-guides/testing.md#the-l5-lane-on-oci.
# Source it; it only defines functions and constants. Nothing here is run by `make
# test`: the scripts that use it talk to a real OCI tenancy and Tailscale, and
# only the e2e workflow runs them. `selftest.sh` runs the pure helpers against stubs.
#
# The environment the workflow (and a developer who runs the lane by hand with
# their own account) provides:
#
#   E2E_CLUSTER             cluster name, scarpenter-ci-<run id>
#   E2E_RUN_ID              the workflow run id (a unique suffix)
#   E2E_CREATED_AT          Unix seconds of the start; tags every instance (sweeper)
#   OCI_COMPARTMENT         the dedicated compartment of the lane (OCID)
#   OCI_TENANCY             the tenancy (OCID)
#   OCI_REGION              the region of the run (eu-frankfurt-1)
#   KUBECONFIG              the k3s cluster
#   E2E_OUT                 directory for logs and the report (default test/e2e-oci/.out)
#
# The oci CLI finds the credentials through its own variables (the workflow's session step,
# oci-session.sh, exports OCI_CLI_CONFIG_FILE, OCI_CLI_PROFILE and OCI_CLI_AUTH=security_token),
# the Go programs through OCI_TOKEN_FILE and the identity domain's variables (test/e2e-oci/ocienv).
#
# Pins (defaults in defaults.sh).

# shellcheck disable=SC2034  # constants used by the scripts that source this file

e2e_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
e2e_root="$(cd "${e2e_dir}/../.." && pwd)"
# shellcheck source=test/lib.sh
source "${e2e_root}/test/lib.sh"
# shellcheck source=test/e2e-oci/defaults.sh
source "${e2e_dir}/defaults.sh"

ns="scarpenter-system"
# The freeform tag every instance of the cluster carries (oci.TagKey of the cluster tag).
ctag="scarpenter-cluster"
label_domain="scarpenter.sparecores.com"
export OCI_CLI_SUPPRESS_FILE_PERMISSIONS_WARNING=True OCI_CLI_REGION="${OCI_REGION}"

# ------------------------------------------------------------ redaction -----

# redact: stdin to stdout without what must not be published (the repository, its job
# logs and its artifacts are public): the tenancy, compartment, user and group OCIDs, the identity
# domain's host name, tailnet host names, Tailscale auth keys, JWTs (GitHub's token and the session
# token of the exchange), request signatures, API key fingerprints, client secrets, Authorization and
# Bearer credentials, private keys, kubeconfig key material, e-mail addresses and bootstrap tokens.
# The OCIDs of instances, subnets and the like stay: they are what a failure is read by. The one
# place for it: diagnostics.sh and the live output of observe.sh use it, and selftest.sh tests it.
# Review a file before sharing it anyway.
read -r -d '' _redact_rules <<'SED' || true
s#(authorization["']?[:=] *["']?)((bearer|basic|token|signature) +)?[^"']+#\1<redacted>#Ig
s#(bearer +)[A-Za-z0-9._~+/=-]+#\1<redacted>#Ig
s#(keyid|signature)="[^"]*"#\1="<redacted>"#Ig
s#-----BEGIN [A-Z ]*PRIVATE KEY-----[^-]*(-----END [A-Z ]*PRIVATE KEY-----)?#<private-key>#g
s#((client-key|client-certificate|certificate-authority)-data: *)[A-Za-z0-9+/=]+#\1<redacted>#g
s#("?(private_key|client_secret|refresh_token|access_token|id_token|security_token|subject_token|pass_phrase)"?[:=] *"?)[^ ,"'}]+#\1<redacted>#Ig
s#ocid1\.(tenancy|compartment|user|group|domain|domaingroup|dynamicgroup|policy|credential|identitydomain|apikey)\.[a-z0-9]+\.[a-z0-9-]*\.[A-Za-z0-9._-]+#<ocid-\1>#g
s#idcs-[a-f0-9]{16,}\.identity\.[a-z.]*oraclecloud\.com#<identity-domain>#g
s#\b([0-9a-f]{2}:){15}[0-9a-f]{2}\b#<fingerprint>#g
s/tskey-[A-Za-z0-9_-]+/<tskey>/g
s/[A-Za-z0-9-]+\.ts\.net/<tailnet-host>/g
s/eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*/<jwt>/g
s/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/<email>/g
s/\b[a-z0-9]{6}\.[a-z0-9]{16}\b/<bootstrap-token>/g
SED
# redact: the rules above, and the lane's compartment and tenancy when they are set (literals,
# escaped by their shape: an OCID holds letters, digits, dots, hyphens and underscores only).
redact() {
  local -a extra=()
  local v
  for v in "${OCI_COMPARTMENT:-}" "${OCI_TENANCY:-}"; do
    if [[ "${v}" =~ ^ocid1\.[a-z]+\.oc[0-9]+\.[a-z0-9-]*\.[A-Za-z0-9_]+$ ]]; then
      extra+=(-e "s/${v//./\\.}/<ocid>/g")
    fi
  done
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

# --------------------------------------------------------------- OCI --------

# oc <oci arguments>: the oci CLI, which prints JSON (an empty list prints nothing).
oc() { oci "$@"; }

# cluster_instances [state...]: the OCIDs of the cluster's instances, one per line, in the lifecycle
# states (default: every one but TERMINATED: OCI lists a terminated instance for about twelve
# hours, so "gone" means "terminated").
cluster_instances() {
  local want='[]'
  [[ $# -eq 0 ]] || want="$(printf '%s\n' "$@" | jq -R . | jq -sc .)"
  oc compute instance list --compartment-id "${OCI_COMPARTMENT}" --all --output json |
    jq -r --arg k "${ctag}" --arg c "${E2E_CLUSTER}" --argjson want "${want}" '
      (.data // [])[] | select(."freeform-tags"[$k] == $c)
      | select(if ($want | length) > 0 then (."lifecycle-state" as $s | $want | index($s)) else ."lifecycle-state" != "TERMINATED" end)
      | .id' | sed '/^$/d'
}

# alive_instances: the instances that are not on their way out: PROVISIONING, STARTING, RUNNING,
# CREATING_IMAGE or MOVING.
alive_instances() { cluster_instances PROVISIONING STARTING RUNNING CREATING_IMAGE MOVING; }

# instance_json <ocid>: the instance (the "data" member of oci compute instance get).
instance_json() { oc compute instance get --instance-id "$1" --output json | jq '.data'; }

# tag_of <json of a resource with freeform tags> <tag key>
tag_of() { jq -r --arg k "$2" '."freeform-tags"[$k] // empty' <<<"$1"; }

# meta_keys <instance json>: the metadata keys of the instance, one per line.
meta_keys() { jq -r '(.metadata // {}) | keys[]' <<<"$1"; }

# vnic_json <instance ocid>: the primary VNIC of the instance.
vnic_json() {
  oc compute instance list-vnics --instance-id "$1" --output json |
    jq '[(.data // [])[] | select(."is-primary" == true)] | first'
}

# availability_domains: the full names of the tenancy's availability domains in the region, one per line.
availability_domains() {
  oc iam availability-domain list --compartment-id "${OCI_TENANCY}" --output json | jq -r '(.data // [])[].name'
}

# zone_name <full availability domain name>: the zone name (the tenancy's prefix and its colon removed).
zone_name() { printf '%s' "${1#*:}"; }

# boot_volume_of <instance json>: the boot volume of an instance, as JSON.
boot_volume_of() {
  local ad id
  ad="$(jq -r '."availability-domain"' <<<"$1")"
  id="$(oc compute boot-volume-attachment list --availability-domain "${ad}" --compartment-id "${OCI_COMPARTMENT}" \
    --instance-id "$(jq -r '.id' <<<"$1")" --output json | jq -r '(.data // [])[0]."boot-volume-id" // empty')"
  [[ -n "${id}" ]] || return 1
  oc bv boot-volume get --boot-volume-id "${id}" --output json | jq '.data'
}

# new_boot_volumes: the ids of the boot volumes of the compartment that exist (available or being
# created) and were created since the run began. In the dedicated compartment these are the lane's.
new_boot_volumes() {
  local ad
  for ad in $(availability_domains); do
    oc bv boot-volume list --compartment-id "${OCI_COMPARTMENT}" --availability-domain "${ad}" --all --output json |
      jq -r --argjson since "${E2E_CREATED_AT}" '
        (.data // [])[] | select(."lifecycle-state" != "TERMINATED" and ."lifecycle-state" != "TERMINATING")
        | select((."time-created" | sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | fromdateiso8601) >= $since) | .id'
  done
}

# limit_scope <limit name>: AD, REGION or GLOBAL (the scope of the limit definition; AD when unknown).
limit_scope() {
  oc limits definition list --compartment-id "${OCI_TENANCY}" --service-name compute --all --output json |
    jq -r --arg n "$1" '[(.data // [])[] | select(.name == $n) | ."scope-type"] | first // "AD"'
}

# region_quota <limit name>: the limit and the usage of a compute limit in the lane's region as the
# controller's bucket states them (plan 25 section 7): usage = the sum over the availability
# domains (one read for a region-scoped limit), limit = usage + the largest availability. "limit usage".
region_quota() {
  local name="$1" scope ad used=0 best=0 u a out
  scope="$(limit_scope "${name}")"
  if [[ "${scope}" == AD ]]; then
    for ad in $(availability_domains); do
      out="$(oc limits resource-availability get --service-name compute --limit-name "${name}" \
        --compartment-id "${OCI_TENANCY}" --availability-domain "${ad}" --output json)" || return 1
      u="$(jq -r '.data.used // 0' <<<"${out}")"
      a="$(jq -r '.data.available // 0' <<<"${out}")"
      used=$((used + $(num "${u}")))
      best=$((best > $(num "${a}") ? best : $(num "${a}")))
    done
  else
    out="$(oc limits resource-availability get --service-name compute --limit-name "${name}" \
      --compartment-id "${OCI_TENANCY}" --output json)" || return 1
    used="$(num "$(jq -r '.data.used // 0' <<<"${out}")")"
    best="$(num "$(jq -r '.data.available // 0' <<<"${out}")")"
  fi
  echo "$((used + best)) ${used}"
}

# ---------------------------------------------------------- Kubernetes ------

kjson() { kubectl get "$@" -o json; }

# pod_node <pod name> [namespace]
pod_node() { kubectl -n "${2:-default}" get pod "$1" -o jsonpath='{.spec.nodeName}'; }

# claim_of_node <node>: the NodeClaim that registered as the node.
claim_of_node() {
  kjson nodeclaims | jq -r --arg n "$1" '.items[] | select(.status.nodeName == $n) | .metadata.name'
}

# instance_of_claim <claim>: the instance OCID from the driver's annotation
# (scarpenter.sparecores.com/instance-ref = <region>/<instance OCID>).
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

# num <value>: an integer from a Prometheus or CLI number (5, 5.0, 5e+00).
num() { awk -v v="$1" 'BEGIN { printf "%d", v + 0.5 }'; }

# ------------------------------------------------------------ scenarios -----

# The quota buckets of the lane's amd64 family the controller reports (the names of the compute
# limits the E5 flexible shape consumes, plan 25 section 7): OCPUs and gigabytes.
bucket_cores="standard-e5-core-count"
bucket_memory="standard-e5-memory-count"

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

# shellcheck source=test/e2e-oci/observe.sh
source "${e2e_dir}/observe.sh"
