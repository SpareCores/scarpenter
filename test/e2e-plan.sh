#!/usr/bin/env bash
# Decides which real-cloud (L5) runs a trigger needs and prints them for a workflow:
#
#   matrix=[{"vendor":"aws","mode":"smoke","scenarios":""}, ...]
#   any=true|false
#
# The vendors come from test/e2e-vendors.txt (the only list). It needs no secret and calls
# no API, so it runs in a job without credentials. No automatic trigger runs L5: the callers
# are the manual workflow e2e.yml (dispatch) and the release gate (release, none by default); the canary (vendors) reads the same list.
#
#   test/e2e-plan.sh dispatch <vendors> <mode> [gpu_type] [cvm_type] [env]
#       vendors  all, or a comma separated list of vendors (aws,oci)
#       mode     smoke, full, or a comma separated list of scenarios (gpu; amd64,drift)
#       gpu_type, cvm_type   instance types for the gpu and cvm scenarios (E2E_GPU_TYPE, E2E_TYPE_CVM)
#       env      KEY=VALUE pairs, comma separated: more overrides of the lane's defaults
#       Also prints env_overrides=KEY=VALUE,KEY=VALUE (every override, for the lane).
#   test/e2e-plan.sh release [none|full|smoke]   the release gate (RELEASE_E2E_MODE): every vendor
#                                                in the mode; none (the default, also for an
#                                                empty value): no vendor, no real instance
#   test/e2e-plan.sh vendors [vendor]            the vendors as {"vendor":...} entries (the
#                                                canary); one vendor or all
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
vendors_file="${E2E_VENDORS_FILE:-${here}/e2e-vendors.txt}"

die() {
  echo "e2e-plan.sh: $*" >&2
  exit 2
}

vendor_list() {
  local v
  while read -r v; do
    [[ -z "${v}" || "${v}" == \#* ]] && continue
    [[ "${v}" =~ ^[a-z][a-z0-9]*$ ]] || die "invalid vendor '${v}' in ${vendors_file}"
    echo "${v}"
  done <"${vendors_file}"
}

# is_vendor <name>
is_vendor() { vendor_list | grep -qxF -- "$1"; }

# entry <vendor> <mode> <scenarios>
entry() { jq -cn --arg v "$1" --arg m "$2" --arg s "$3" '{vendor: $v, mode: $m, scenarios: $s}'; }

# emit: JSON entries on stdin, one per line; prints the two outputs.
emit() {
  local m
  m="$(jq -cs '.')"
  echo "matrix=${m}"
  if [[ "${m}" == "[]" ]]; then echo "any=false"; else echo "any=true"; fi
}

# Keys the lane sets itself, and the values that reach GITHUB_ENV: e2e-cloud.yml checks the same.
valid_override() { # valid_override <key> <value>
  [[ "$1" =~ ^E2E_[A-Z0-9_]+$ ]] || die "override key '$1' must match ^E2E_[A-Z0-9_]+\$"
  case "$1" in
    E2E_VENDOR | E2E_MODE | E2E_SCENARIOS | E2E_FAIL_FAST | E2E_CLUSTER | E2E_RUN_ID | E2E_CREATED_AT | E2E_OUT | E2E_PREPARED)
      die "override key '$1' is set by the workflow"
      ;;
  esac
  [[ "$2" =~ ^[A-Za-z0-9._:/+-]+$ ]] || die "override value of '$1' must match ^[A-Za-z0-9._:/+-]+\$"
}

cmd_dispatch() {
  [[ $# -ge 2 ]] || die "usage: dispatch <vendors> <mode> [gpu_type] [cvm_type] [env]"
  local vendors="$1" mode="$2" gpu="${3:-}" cvm="${4:-}" extra="${5:-}"
  local scenarios="" v w pair key overrides=() seen=","
  [[ "${vendors}" =~ ^[a-z0-9]+(,[a-z0-9]+)*$ ]] || die "vendors must be all or a comma separated list of vendors"
  if [[ "${vendors}" != all ]]; then
    for w in ${vendors//,/ }; do
      is_vendor "${w}" || die "unknown vendor '${w}' (known: $(vendor_list | paste -sd, -))"
    done
  fi
  case "${mode}" in
    smoke | full) ;;
    *)
      [[ "${mode}" =~ ^[a-z0-9]+(,[a-z0-9]+)*$ ]] || die "mode must be smoke, full or a comma separated list of scenarios"
      scenarios="${mode}"
      mode=full
      ;;
  esac
  if [[ -n "${gpu}${cvm}" ]]; then
    [[ "${vendors}" != all && "${vendors}" != *,* ]] || die "gpu_instance_type and cvm_instance_type need exactly one vendor (the types carry the vendor's prefix)"
  fi
  [[ -z "${gpu}" ]] || overrides+=("E2E_GPU_TYPE=${gpu}")
  [[ -z "${cvm}" ]] || overrides+=("E2E_TYPE_CVM=${cvm}")
  if [[ -n "${extra}" ]]; then
    for pair in ${extra//,/ }; do
      [[ "${pair}" == *=* ]] || die "env entry '${pair}' is not KEY=VALUE"
      overrides+=("${pair}")
    done
  fi
  for pair in "${overrides[@]}"; do
    key="${pair%%=*}"
    valid_override "${key}" "${pair#*=}"
    [[ "${seen}" != *",${key},"* ]] || die "override '${key}' is given twice"
    seen+="${key},"
  done
  # The list is empty or not: the lane reads one comma separated string.
  echo "env_overrides=$(IFS=,; echo "${overrides[*]:-}")"
  while read -r v; do
    if [[ "${vendors}" == all || ",${vendors}," == *",${v},"* ]]; then entry "${v}" "${mode}" "${scenarios}"; fi
  done < <(vendor_list) | emit
}

cmd_release() {
  local mode="${1:-none}" v
  [[ "${mode}" == none || "${mode}" == full || "${mode}" == smoke ]] || die "RELEASE_E2E_MODE must be unset, none, full or smoke, not '${mode}'"
  while read -r v; do
    if [[ "${mode}" != none ]]; then entry "${v}" "${mode}" ""; fi
  done < <(vendor_list) | emit
}

cmd_vendors() {
  local want="${1:-all}" v
  if [[ "${want}" != all ]]; then
    is_vendor "${want}" || die "unknown vendor '${want}'"
  fi
  while read -r v; do
    if [[ "${want}" == all || "${want}" == "${v}" ]]; then jq -cn --arg v "${v}" '{vendor: $v}'; fi
  done < <(vendor_list) | emit
}

case "${1:-}" in
  dispatch)
    shift
    cmd_dispatch "$@"
    ;;
  release)
    shift
    cmd_release "$@"
    ;;
  vendors)
    shift
    cmd_vendors "$@"
    ;;
  *) die "usage: $0 dispatch|release|vendors (see the header of this file)" ;;
esac
