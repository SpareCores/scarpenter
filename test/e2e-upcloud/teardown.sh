#!/usr/bin/env bash
# L5 teardown (the workflow runs it with `if: always()`):
#   1. delete SCarpenter's NodePools and wait for the NodeClaims to go (the controller
#      stops and deletes the servers), if the controller still runs;
#   2. delete what the workflow created for the bring-your-own scenario (the SDN network
#      and ruleset labelled scarpenter-e2e-byo), then force-delete anything labelled with
#      the run's cluster and what it left (servers, server groups, rulesets, networks,
#      disks) with the sweeper, ignoring the age limit;
#   3. check that nothing labelled with the cluster remains: no server, disk, network,
#      ruleset or server group (plan 19 scenario 5): the job fails when something does;
#   4. revoke the nodes' auth key and take the runner off the tailnet.
# Every step runs even when an earlier one failed; the exit status is the sum.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-upcloud/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-upcloud/tailscale-api.sh
source "${here}/tailscale-api.sh"
need E2E_CLUSTER E2E_OUT UPCLOUD_TOKEN
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
        echo "NodeClaims are left after 300 s: the servers are force-deleted next" >&2
        break
      fi
      sleep 5
    done
  else
    echo "the controller is gone (uninstalled): nothing deletes NodeClaims any more"
  fi
else
  echo "the API server does not answer: going straight to UpCloud"
fi

log "2. the brought resources, then force-delete and sweep what the cluster label finds"
left="$(cluster_servers | wc -l | tr -d ' ')"
[[ "${left}" == 0 ]] || echo "NOTE: ${left} server(s) were still there and are deleted by the sweeper"
# The brought network and ruleset must go after the servers that use them (the sweeper does
# the servers first, then these, so the explicit deletes run after one sweep).
(cd "${root}" && go run ./test/e2e-upcloud/sweeper -cluster "${E2E_CLUSTER}" -max-age 0) || problem "the sweeper failed"
delete_brought || problem "could not delete what the bring-your-own scenario created"
(cd "${root}" && go run ./test/e2e-upcloud/sweeper -cluster "${E2E_CLUSTER}" -max-age 0) || problem "the second sweep failed"

log "3. nothing labelled ${ctag}=${E2E_CLUSTER} may remain"
leaks() { # leaks <what> <command printing the IDs>
  local out
  out="$("${@:2}" 2>&1)" || {
    problem "could not list ${1}: ${out}"
    return
  }
  [[ -z "${out}" ]] || problem "${1} left: ${out}"
}
leaks servers cluster_servers
leaks "server groups" cluster_groups
leaks rulesets cluster_rulesets
leaks networks cluster_networks
# A disk of a deleted server goes with the delete call; give a late one a moment.
deadline=$((SECONDS + 120))
until [[ -z "$(cluster_storages 2>/dev/null)" ]]; do
  if [[ "${SECONDS}" -ge "${deadline}" ]]; then
    leaks storages cluster_storages
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
