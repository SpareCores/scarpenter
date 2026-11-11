#!/usr/bin/env bash
# L5 teardown (the workflow runs it with `if: always()`), in the
# order of overview section 13.4:
#   1. delete SCarpenter's NodePools and wait for the NodeClaims to go (the controller
#      deletes the servers), if the controller still runs;
#   2. delete anything tagged with the run's cluster and what it left (security groups,
#      server groups) with the sweeper, ignoring the age limit, and the security group
#      the bring-your-own scenario made;
#   3. check that nothing of the cluster remains: no server, security group or server
#      group, and no volume attached to nothing with the cluster's marker in its name:
#      the job fails when something does;
#   4. revoke the nodes' auth key and take the runner off the tailnet.
# Every step runs even when an earlier one failed; the exit status is the sum.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-ovh/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-ovh/tailscale-api.sh
source "${here}/tailscale-api.sh"
need E2E_CLUSTER E2E_OUT OS_REGION_NAME
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
        echo "NodeClaims are left after 300 s: the servers are deleted next" >&2
        break
      fi
      sleep 5
    done
  else
    echo "the controller is gone (uninstalled): nothing deletes NodeClaims any more"
  fi
else
  echo "the API server does not answer: going straight to OVH"
fi

log "2. delete and sweep what the cluster marker finds"
left="$(cluster_instances | wc -l | tr -d ' ')"
[[ "${left}" == 0 ]] || echo "NOTE: ${left} server(s) were still there and are deleted by the sweeper"
(cd "${root}" && go run ./test/e2e-ovh/sweeper -cluster "${E2E_CLUSTER}" -max-age 0 -regions "${OS_REGION_NAME}" -wait 5m) ||
  problem "the sweeper failed"
# The group the bring-your-own scenario made with the CLI carries no ownership marker, so the
# sweeper never touches it: its name is the run's.
byo="$(os security group list -f json 2>/dev/null | jq -r --arg n "${E2E_CLUSTER}-byo" '.[] | select(.Name == $n) | .ID')"
for id in ${byo}; do
  os security group delete "${id}" || problem "could not delete the brought security group ${id}"
done

log "3. nothing of ${E2E_CLUSTER} may remain"
leaks() { # leaks <what> <command printing the IDs>
  local out
  out="$("${@:2}" 2>&1)" || {
    problem "could not list ${1}: ${out}"
    return
  }
  [[ -z "${out}" ]] || problem "${1} left: ${out}"
}
leaks servers cluster_instances
# The deleted servers drop out of the list once Nova has removed them.
deadline=$((SECONDS + 180))
until [[ -z "$(cluster_instances 2>/dev/null)" ]]; do
  [[ "${SECONDS}" -lt "${deadline}" ]] || break
  sleep 10
done
leaks "servers (after the wait)" cluster_instances
# Managed groups: the marker is in the description (security groups) or the name (server
# groups, whose names carry a hash of the long cluster name, not the run ID: the project is the
# lane's own and one run at a time, so any group of the lane's prefix is a leak).
leaks "security groups" bash -c "openstack --os-region-name '${OS_REGION_NAME}' security group list -f json | jq -r --arg c '${E2E_CLUSTER}' '.[] | select((.Description // \"\") | contains(\"\\\"cluster\\\":\\\"\" + \$c + \"\\\"\")) | .ID'"
leaks "server groups" bash -c "openstack --os-region-name '${OS_REGION_NAME}' server group list -f json | jq -r '.[] | select(.Name | startswith(\"scarpenter-scarpenter-ci-\")) | .ID'"
# The driver's volumes go with their server (delete_on_termination), so a volume that is
# available (attached to nothing) in the lane's own project is a leak of the create call.
leaks "volumes" bash -c "openstack --os-region-name '${OS_REGION_NAME}' volume list -f json | jq -r '.[] | select(.Status == \"available\") | .ID'"

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
