#!/usr/bin/env bash
# L5 teardown (the workflow runs it with `if: always()`), in the
# order of overview section 13.4:
#   1. delete SCarpenter's NodePools and wait for the NodeClaims to go (the controller
#      terminates the instances), if the controller still runs;
#   2. delete anything tagged with the run's cluster and what it left (instances,
#      deployment sets, security groups, the managed network, the VPC, vSwitch and group
#      the byo scenario brought) with the sweeper, ignoring the age limit;
#   3. check that nothing of the run remains: no instance, disk, network interface,
#      security group, deployment set, vSwitch or VPC (plan 21 section 9): the job fails
#      when something does;
#   4. revoke the nodes' auth key and take the runner off the tailnet.
# Every step runs even when an earlier one failed; the exit status is the sum.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-alicloud/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-alicloud/tailscale-api.sh
source "${here}/tailscale-api.sh"
need E2E_CLUSTER E2E_OUT ALICLOUD_REGION
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
        echo "NodeClaims are left after 300 s: the instances are force-terminated next" >&2
        break
      fi
      sleep 5
    done
  else
    echo "the controller is gone (uninstalled): nothing deletes NodeClaims any more"
  fi
else
  echo "the API server does not answer: going straight to Alibaba Cloud"
fi

log "2. delete what the cluster tag finds"
# The temporary credentials may be an hour old: the workflow writes a fresh OIDC token before
# this step, and ali_login exchanges it again when the token file is newer than the cache.
ali_login || problem "could not get Alibaba Cloud credentials"
left="$(cluster_instances | wc -l | tr -d ' ')"
[[ "${left}" == 0 ]] || echo "NOTE: ${left} instance(s) were still there and are deleted by the sweeper"
(cd "${root}" && go run ./test/e2e-alicloud/sweeper -cluster "${E2E_CLUSTER}" -max-age 0 -regions "${ALICLOUD_REGION}" -wait 5m) ||
  problem "the sweeper failed"

log "3. nothing of ${E2E_CLUSTER} may remain"
leaks() { # leaks <what> <command printing the IDs>
  local out
  out="$("${@:2}" 2>&1)" || {
    problem "could not list ${1}: ${out}"
    return
  }
  [[ -z "${out}" ]] || problem "${1} left: ${out}"
}
leaks instances cluster_instances
leaks "security groups" cluster_security_groups
leaks "deployment sets" cluster_deployment_sets
leaks vSwitches cluster_vswitches
leaks VPCs cluster_vpcs
# Data disks go with their instance (DeleteWithInstance), a disk of a released instance is
# listed for a moment more; the interfaces likewise.
deadline=$((SECONDS + 180))
while true; do
  disks="$(cluster_disks 2>&1)"
  enis="$(cluster_enis 2>&1)"
  [[ -z "${disks}${enis}" ]] && break
  if [[ "${SECONDS}" -ge "${deadline}" ]]; then
    [[ -z "${disks}" ]] || problem "disks left: ${disks}"
    [[ -z "${enis}" ]] || problem "network interfaces left: ${enis}"
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
