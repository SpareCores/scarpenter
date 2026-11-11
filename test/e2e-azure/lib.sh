#!/usr/bin/env bash
# Helpers of the Azure L5 lane (Azure e2e over Tailscale): docs/how-to-guides/testing.md#the-l5-lane.
# Source it; it only defines functions and constants. Nothing here is run by `make
# test`: the scripts that use it talk to a real Azure subscription and Tailscale, and only
# the e2e-cloud workflow runs them. `selftest.sh` runs the pure helpers against stubs.
#
# The environment the workflow (and a developer who runs the lane by hand with
# their own subscription) provides:
#
#   E2E_CLUSTER             cluster name, scarpenter-ci-<run id>
#   E2E_RUN_ID              the workflow run id (a unique suffix)
#   E2E_CREATED_AT          Unix seconds of the start; tags every resource (sweeper), as RFC 3339
#   AZURE_REGION            the region of the run (westeurope)
#   AZURE_SUBSCRIPTION_ID   the subscription of the lane (a variable, not a secret)
#   AZURE_RESOURCE_GROUP    the lane's resource group in that region: the CI role's scope
#   AZURE_CLIENT_ID,
#   AZURE_TENANT_ID         the CI identity (federated with GitHub's OIDC; no secret)
#   KUBECONFIG              the k3s cluster
#   E2E_OUT                 directory for logs and the report (default test/e2e-azure/.out)
#
# Pins (defaults in defaults.sh).

# shellcheck disable=SC2034  # constants used by the scripts that source this file

e2e_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
e2e_root="$(cd "${e2e_dir}/../.." && pwd)"
# shellcheck source=test/lib.sh
source "${e2e_root}/test/lib.sh"
# shellcheck source=test/e2e-azure/defaults.sh
source "${e2e_dir}/defaults.sh"

ns="scarpenter-system"
# Azure tag names are scarpenter-<logical key> (pkg/vendors/azure TagKey); the label
# domain is the Kubernetes side of the same facts.
ctag="scarpenter-cluster"
tag_prefix="scarpenter-"
label_domain="scarpenter.sparecores.com"
# The tags the NodeClasses put on every resource of the run (the sweeper reads them): an
# Azure resource has a creation time in no API the sweeper can rely on for every kind.
created_tag="scarpenter-ci-created-at"
# The sweeper reads the tag as RFC 3339 (pkg/vendors/azure/sweep), not as Unix seconds.
if [[ -n "${E2E_CREATED_AT:-}" ]]; then
  E2E_CREATED_AT_RFC3339="$(date -u -d "@${E2E_CREATED_AT}" +%Y-%m-%dT%H:%M:%SZ)"
  export E2E_CREATED_AT_RFC3339
fi
export AZURE_CORE_OUTPUT=json
export AZURE_CORE_ONLY_SHOW_ERRORS=true
export AZURE_CORE_COLLECT_TELEMETRY=false
export AZURE_CORE_NO_COLOR=true

# ------------------------------------------------------------ redaction -----

# redact: stdin to stdout without what must not be published (the repository, its job
# logs and its artifacts are public): subscription, tenant and other GUIDs, tailnet host
# names, Tailscale auth keys, Azure client secrets, storage keys, SAS signatures, access
# and refresh tokens, JWTs, Authorization and Bearer credentials, passwords, kubeconfig key
# material, e-mail addresses and bootstrap tokens. The one place for it: diagnostics.sh
# and the live output of observe.sh use it, and selftest.sh tests it. Review a file before
# sharing it anyway.
read -r -d '' _redact_rules <<'SED' || true
s#(authorization["']?[:=] *["']?)((bearer|basic|token) +)?[^ ,"']+#\1<redacted>#Ig
s#(bearer +)[A-Za-z0-9._~+/=-]+#\1<redacted>#Ig
s#((client-key|client-certificate|certificate-authority)-data: *)[A-Za-z0-9+/=]+#\1<redacted>#g
s#(client_?secret|admin_?password|password|access_?token|refresh_?token|id_?token|account_?key|shared_?access_?key|custom_?data)(["']?[:= ]+["']?)[^ ,"'}]+#\1\2<redacted>#Ig
s#([?&]sig=)[A-Za-z0-9%+/=_-]+#\1<redacted>#g
s#(AZURE_CLIENT_SECRET|AZURE_FEDERATED_TOKEN)=[^ ]+#\1=<redacted>#g
s/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/<guid>/g
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

# --------------------------------------------------------------- Azure ------

# The lane's group and region are the scope of every call: the CI role cannot see more.
# jq is the one JSON tool; the az queries (JMESPath) only select what is cheap to select
# there and print tab separated text.

# cluster_hash: the 8 hex digits the driver puts at the end of every VM name (the first 8
# of the SHA-256 of the cluster name; pkg/vendors/azure names.go hash8).
cluster_hash() { printf '%s' "${E2E_CLUSTER}" | sha256sum | cut -c1-8; }

# vm_name_of <claim>: the VM of a NodeClaim, <claim>-<hash8(cluster)> (at most 64
# characters; the lane's claim names are short).
vm_name_of() { echo "$1-$(cluster_hash)"; }

# cluster_vms [all]: the names of the cluster's VMs in the lane's group, one per line;
# without "all" the ones whose delete is already running (provisioningState Deleting) are
# left out: a VM exists until its delete finished, and Azure keeps no terminated ones.
cluster_vms() {
  local filter="tags.\"${ctag}\"=='${E2E_CLUSTER}'"
  [[ "${1:-}" == all ]] || filter+=" && provisioningState!='Deleting'"
  az vm list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?${filter}].name" --output tsv | tr -d '\r' | sed '/^$/d'
}

# vm_json <vm name>: the VM with its instance view's fields that az prints without -d.
vm_json() { az vm show --resource-group "${AZURE_RESOURCE_GROUP}" --name "$1" --output json; }

# vm_exists <vm name>: the VM is there (a 404 is "gone"; any other failure is an error and
# is not "gone": the caller's check fails instead of passing on a broken login).
vm_exists() {
  local out
  out="$(az vm list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?name=='$1'].name" --output tsv)" || return 2
  [[ -n "${out}" ]]
}

# vms_present <vm name>...: those of the VMs that still exist (listing failure: an error).
vms_present() {
  local all n
  all="$(az vm list --resource-group "${AZURE_RESOURCE_GROUP}" --query '[].name' --output tsv | tr -d '\r')" || return 1
  for n in "$@"; do
    grep -qx "${n}" <<<"${all}" && echo "${n}"
  done
  return 0
}

# vm_leftovers <vm name>: the NICs, public IPs and disks that carry the VM's name as a
# prefix (<vm>-nic-<hash>, <vm>-pip, <vm>-os, <vm>-<disk>): what the deleteOption cascade
# must have removed with the VM. One "<kind> <name>" per line. A listing that fails fails.
vm_leftovers() {
  local vm="$1" out kind cmd
  for kind in nic public-ip; do
    cmd=(az network "${kind}" list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?starts_with(name, '${vm}-')].name" --output tsv)
    out="$("${cmd[@]}")" || return 1
    [[ -z "${out}" ]] || awk -v k="${kind}" '{ sub(/\r$/, ""); print k " " $0 }' <<<"${out}"
  done
  out="$(az disk list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?starts_with(name, '${vm}-')].name" --output tsv)" || return 1
  [[ -z "${out}" ]] || awk '{ sub(/\r$/, ""); print "disk " $0 }' <<<"${out}"
  return 0
}

# cluster_disks: the disks of the run, by the hash the VM names end with (inline disks carry
# no tag: the driver names them <vm>-os and <vm>-<name>).
cluster_disks() {
  az disk list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?contains(name, '-$(cluster_hash)-')].name" --output tsv | tr -d '\r'
}

# cluster_resources: the IDs of every resource of the group tagged with the cluster.
cluster_resources() {
  az resource list --resource-group "${AZURE_RESOURCE_GROUP}" --tag "${ctag}=${E2E_CLUSTER}" --query '[].id' --output tsv | tr -d '\r' | sed '/^$/d'
}

# managed_resources: the ones SCarpenter created (the brought ones carry no managed-by tag).
managed_resources() {
  az resource list --resource-group "${AZURE_RESOURCE_GROUP}" --tag "${ctag}=${E2E_CLUSTER}" \
    --query "[?tags.\"${tag_prefix}managed-by\"=='scarpenter'].id" --output tsv | tr -d '\r' | sed '/^$/d'
}

# usage_of compute|network <usage name>: currentValue of a usage row of the region, as the
# Usage APIs report it (the number the driver reads).
usage_of() {
  local cmd=(az vm list-usage)
  [[ "$1" == network ]] && cmd=(az network list-usages)
  "${cmd[@]}" --location "${AZURE_REGION}" --query "[?name.value=='$2'].currentValue | [0]" --output tsv | tr -d '\r'
}

# limit_of compute|network <usage name>: the limit of a usage row.
limit_of() {
  local cmd=(az vm list-usage)
  [[ "$1" == network ]] && cmd=(az network list-usages)
  "${cmd[@]}" --location "${AZURE_REGION}" --query "[?name.value=='$2'].limit | [0]" --output tsv | tr -d '\r'
}

# The sizes the suite launches, with their SKU rows, read once per run (az vm list-skus is
# slow and big) and kept in E2E_OUT/state/skus.json.
lane_sizes() {
  local t
  for t in "${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_SPOT}" "${E2E_TYPE_DB}" "${E2E_TYPE_DENIED}" ${E2E_TYPES_DESCRIBED//,/ }; do
    echo "${t#azure.}"
  done | sort -u
}

skus_file() {
  local f="${E2E_OUT:?E2E_OUT is required}/state/skus.json" sizes
  if [[ ! -s "${f}" ]]; then
    mkdir -p "$(dirname "${f}")"
    sizes="$(lane_sizes | jq -R . | jq -s .)"
    az vm list-skus --location "${AZURE_REGION}" --resource-type virtualMachines |
      jq --argjson sizes "${sizes}" '[.[] | select(.name as $n | $sizes | index($n))]' >"${f}.tmp" && mv "${f}.tmp" "${f}"
  fi
  echo "${f}"
}

# sku_field <size without the vendor prefix> <jq filter on the SKU row>
sku_field() {
  jq -r --arg n "$1" "[.[] | select(.name == \$n)] | first | $2 // empty" "$(skus_file)"
}

# vcpus_of <size without the vendor prefix>: the vCPUs capability of the size.
vcpus_of() { sku_field "$1" '(.capabilities[] | select(.name == "vCPUs") | .value)'; }

# sku_family <size without the vendor prefix>: the quota family of the size (the name of
# its row in the Compute usages, for example standardBasv2Family).
sku_family() { sku_field "$1" '.family'; }

# ---------------------------------------------------------- Kubernetes ------

kjson() { kubectl get "$@" -o json; }

# pod_node <pod name> [namespace]
pod_node() { kubectl -n "${2:-default}" get pod "$1" -o jsonpath='{.spec.nodeName}'; }

# claim_of_node <node>: the NodeClaim that registered as the node.
claim_of_node() {
  kjson nodeclaims | jq -r --arg n "$1" '.items[] | select(.status.nodeName == $n) | .metadata.name'
}

# vm_of_claim <claim>: <resource group>/<vm name> from the driver's annotation
# (scarpenter.sparecores.com/instance-ref = <region>/<resource group>/<vm name>).
vm_of_claim() {
  kubectl get nodeclaim "$1" -o json | jq -r --arg k "${label_domain}/instance-ref" '.metadata.annotations[$k] // empty' | sed 's#^[^/]*/##'
}

# vm_rg and vm_name split what vm_of_claim prints.
vm_rg() { echo "${1%%/*}"; }
vm_name() { echo "${1##*/}"; }

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

# num <value>: an integer from a Prometheus or Azure number (5, 5.0, 5e+00).
num() { awk -v v="$1" 'BEGIN { printf "%d", v + 0.5 }'; }

# ------------------------------------------------------------ scenarios -----

# The quota buckets of the driver (pkg/vendors/azure quota.go: the Usage API's names):
# the regional vCPUs, the Spot vCPUs and the Standard public IPv4 addresses; the family
# buckets are the lane types' (sku_family). Names to confirm on the live account.
bucket_cores="cores"
bucket_spot="lowPriorityCores"
bucket_pip="StandardPublicIPAddresses"

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

# shellcheck source=test/e2e-azure/observe.sh
source "${e2e_dir}/observe.sh"
