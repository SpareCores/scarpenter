#!/usr/bin/env bash
# L5 teardown (the workflow runs it with `if: always()`), in the
# order of overview section 13.4:
#   1. delete SCarpenter's NodePools and wait for the NodeClaims to go (the controller
#      deletes the instances), if the controller still runs;
#   2. force-delete anything labelled with the run's cluster and delete what it left
#      (resource policies, firewall rules, subnets, the managed network, and what the
#      byo scenario brought) with the sweeper, ignoring the age limit;
#   3. check that nothing of the run remains: no instance, no disk (the sweeper cannot
#      list disks: they go with their instance by autoDelete, and a disk that does not is a
#      leak this step deletes and reports), no resource policy, firewall rule, subnet or
#      network by the run's name prefixes (plan 16 section 9): the job fails when something does;
#   4. revoke the nodes' auth key and take the runner off the tailnet.
# Every step runs even when an earlier one failed; the exit status is the sum.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-gcp/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-gcp/tailscale-api.sh
source "${here}/tailscale-api.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_OUT GCP_PROJECT GCP_REGION
root="${e2e_root}"
failures=0
problem() {
  echo "PROBLEM: $*" >&2
  failures=$((failures + 1))
}

log "1. NodePools and NodeClaims"
if kubectl get --raw /readyz >/dev/null 2>&1; then
  kjson nodepools 2>/dev/null | jq -r '.items[] | select(.spec.template.spec.nodeClassRef.group == "scarpenter.sparecores.com") | .metadata.name' |
    xargs -r kubectl delete nodepool --wait=false
  if kubectl -n "${ns}" get deploy scarpenter >/dev/null 2>&1; then
    deadline=$((SECONDS + 300))
    until [[ -z "$(kubectl get nodeclaims -o name 2>/dev/null)" ]]; do
      if [[ "${SECONDS}" -ge "${deadline}" ]]; then
        echo "NodeClaims are left after 300 s: the instances are force-deleted next" >&2
        break
      fi
      sleep 5
    done
  else
    echo "the controller is gone (uninstalled): nothing deletes NodeClaims any more"
  fi
else
  echo "the API server does not answer: going straight to Google Cloud"
fi

log "2. force-delete and sweep what the cluster label and the run's marker find"
left="$(cluster_instances | wc -l | tr -d ' ')"
[[ "${left}" == 0 ]] || echo "NOTE: ${left} instance(s) were still there and are deleted by the sweeper"
(cd "${root}" && go run ./test/e2e-gcp/sweeper -project "${GCP_PROJECT}" -cluster "${E2E_CLUSTER}" -max-age 0 -regions "${GCP_REGION}" -wait 5m) ||
  problem "the sweeper failed"

log "3. nothing of ${E2E_CLUSTER} may remain"
leaks() { # leaks <what> <command printing the names>
  local out
  out="$("${@:2}" 2>&1)" || {
    problem "could not list ${1}: ${out}"
    return
  }
  [[ -z "${out}" ]] || problem "${1} left: ${out}"
}
# Names: SCarpenter's managed resources and the lane's own byo resources.
managed="^scarpenter-${E2E_CLUSTER}-"
byo="^scarpenter-e2e-byo-${E2E_RUN_ID}"
names="name~${managed} OR name~${byo}"
leaks instances cluster_instances
leaks "firewall rules" gc compute firewall-rules list --filter="${names}" --format='value(name)'
leaks "resource policies" gc compute resource-policies list --filter="name~${managed}" --format='value(name)'
leaks subnets gc compute networks subnets list --filter="${names}" --format='value(name)'
leaks networks gc compute networks list --filter="${names}" --format='value(name)'
# Disks go with their instance (autoDelete), a little after it. One that stays is deleted
# here and reported.
deadline=$((SECONDS + 120))
disks_left() { gc compute disks list --filter="labels.${ctag}=${E2E_CLUSTER}" --format='value(name,zone.basename())'; }
until [[ -z "$(disks_left 2>/dev/null)" ]]; do
  if [[ "${SECONDS}" -ge "${deadline}" ]]; then
    leaked="$(disks_left 2>&1)"
    problem "disks left (deleting them now): ${leaked}"
    while read -r name zone; do
      [[ -z "${name}" ]] || gc compute disks delete "${name}" --zone "${zone}" >/dev/null 2>&1 || true
    done <<<"${leaked}"
    break
  fi
  sleep 10
done

log "4. the auth key and the tailnet"
if [[ -n "${TS_OAUTH_CLIENT_ID:-}" && -n "${TS_AUDIENCE:-}" ]]; then
  ts_revoke_key "${E2E_OUT}/ts-authkey.id" || echo "could not revoke the auth key (it expires after an hour anyway)"
fi
sudo tailscale logout >/dev/null 2>&1 || true

if [[ "${failures}" -ne 0 ]]; then
  echo "teardown: ${failures} problem(s)" >&2
  exit 1
fi
echo "teardown: clean"
