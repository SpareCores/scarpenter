#!/usr/bin/env bash
# Decides which real-cloud (L5) runs a trigger needs and prints them for a workflow:
#
#   matrix=[{"vendor":"aws","mode":"smoke","scenarios":""}, ...]
#   any=true|false
#
# The vendors come from test/e2e-vendors.txt (the only list); the path rules are below.
# It needs no secret and calls no API, so it runs in a job without credentials.
#
#   test/e2e-plan.sh pr                          changed files on stdin (one per line), the
#                                                labels of the pull request as a JSON array
#                                                in $E2E_LABELS (l5:skip, l5:full)
#   test/e2e-plan.sh dispatch <vendor|all> <full|smoke> [scenarios]
#   test/e2e-plan.sh release                     every vendor, full
#   test/e2e-plan.sh vendors [vendor]            the vendors as {"vendor":...} entries (the
#                                                canary); one vendor or all
#
# Tiers (docs/how-to-guides/testing-and-releasing.md):
#   full   a pull request touched what is specific to the vendor (its driver, its lane), or
#          the lane's shared workflows and helpers
#   smoke  a pull request touched code every vendor's nodes go through
# full wins over smoke for a vendor. Test files (*_test.go, testdata) never trigger a run:
# L1 to L4 cover them. A change that is only documentation triggers nothing.
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

# tier_of <file> <vendor>: prints full or smoke, or nothing.
tier_of() {
  local f="$1" v="$2"
  case "${f}" in
    *_test.go | */testdata/*) return 0 ;;
  esac
  case "${f}" in
    # What is specific to the vendor: its driver, its lane, its sweeper workflow.
    pkg/vendors/"${v}"/* | test/e2e-"${v}"/* | .github/workflows/e2e-"${v}"-sweeper.yml)
      echo full
      return 0
      ;;
    # The lane's shared machinery: every vendor's run changes with it.
    .github/workflows/e2e.yml | .github/workflows/e2e-cloud.yml | test/e2e-plan.sh | test/e2e-vendors.txt | test/lib.sh)
      echo full
      return 0
      ;;
    # The shared driver contract (pkg/vendors/*.go, not a vendor's package) and the facts.
    pkg/vendors/*/*) ;;
    pkg/vendors/*.go)
      echo smoke
      return 0
      ;;
  esac
  case "${f}" in
    pkg/vendors/facts/*)
      echo smoke
      return 0
      ;;
    # Everything a node goes through from the NodeClaim to Ready: the CloudProvider and the
    # controllers around it, the bootstrap (Ignition, join), the artifacts, the resources,
    # quota, catalog and unavailability tracking, the API types, the options and the wiring.
    pkg/bootstrap/* | pkg/cloudprovider/* | pkg/artifacts/* | pkg/resources/* | pkg/quota/* | pkg/controllers/* | \
      pkg/catalog/* | pkg/unavailable/* | pkg/apis/* | pkg/options/* | pkg/operator/* | pkg/scope/* | cmd/*)
      echo smoke
      return 0
      ;;
    # The chart and what it is generated from, the image, the Karpenter pin.
    charts/* | config/crd/* | config/rbac* | Dockerfile | go.mod | go.sum)
      echo smoke
      return 0
      ;;
  esac
}

# entry <vendor> <mode> <scenarios>
entry() { jq -cn --arg v "$1" --arg m "$2" --arg s "$3" '{vendor: $v, mode: $m, scenarios: $s}'; }

# emit: JSON entries on stdin, one per line; prints the two outputs.
emit() {
  local m
  m="$(jq -cs '.')"
  echo "matrix=${m}"
  if [[ "${m}" == "[]" ]]; then echo "any=false"; else echo "any=true"; fi
}

cmd_pr() {
  local labels="${E2E_LABELS:-[]}" files f v t forced=false
  jq -e 'type == "array"' <<<"${labels}" >/dev/null || die "E2E_LABELS must be a JSON array of label names"
  files="$(cat)"
  if jq -e 'index("l5:skip")' <<<"${labels}" >/dev/null; then
    emit </dev/null
    return
  fi
  if jq -e 'index("l5:full")' <<<"${labels}" >/dev/null; then forced=true; fi
  while read -r v; do
    if [[ "${forced}" == true ]]; then
      entry "${v}" full ""
      continue
    fi
    t=""
    while IFS= read -r f; do
      [[ -n "${f}" ]] || continue
      case "$(tier_of "${f}" "${v}")" in
        full)
          t=full
          break
          ;;
        smoke) t=smoke ;;
      esac
    done <<<"${files}"
    if [[ -n "${t}" ]]; then entry "${v}" "${t}" ""; fi
  done < <(vendor_list) | emit
}

cmd_dispatch() {
  [[ $# -ge 2 ]] || die "usage: dispatch <vendor|all> <full|smoke> [scenarios]"
  local vendor="$1" mode="$2" scenarios="${3:-}" v
  [[ "${mode}" == full || "${mode}" == smoke ]] || die "mode must be full or smoke"
  [[ "${scenarios}" =~ ^[a-z0-9,]*$ ]] || die "scenarios must match ^[a-z0-9,]*\$"
  if [[ "${vendor}" != all ]]; then
    is_vendor "${vendor}" || die "unknown vendor '${vendor}' (known: $(vendor_list | paste -sd, -))"
  fi
  while read -r v; do
    if [[ "${vendor}" == all || "${vendor}" == "${v}" ]]; then entry "${v}" "${mode}" "${scenarios}"; fi
  done < <(vendor_list) | emit
}

cmd_release() {
  local v
  while read -r v; do entry "${v}" full ""; done < <(vendor_list) | emit
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
  pr)
    shift
    cmd_pr "$@"
    ;;
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
  *) die "usage: $0 pr|dispatch|release|vendors (see the header of this file)" ;;
esac
