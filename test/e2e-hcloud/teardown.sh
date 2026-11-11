#!/usr/bin/env bash
# L5 teardown (the workflow runs it with `if: always()`), in the order of overview section 13.4:
#   1. delete SCarpenter's NodePools and wait for the NodeClaims to go (the controller deletes the
#      servers), if the controller still runs;
#   2. delete everything labelled with the run's cluster, whatever its age, with the sweeper
#      (servers, volumes, placement groups, firewalls, Primary IPs), then the brought network with the CLI;
#   3. check that nothing labelled with the cluster remains: the job fails when something does;
#   4. revoke the nodes' auth key and take the runner off the tailnet.
# Every step runs even when an earlier one failed; the exit status is the sum. The Flatcar
# snapshots are kept: they are the node image of every run and are never swept.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-hcloud/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-hcloud/tailscale-api.sh
source "${here}/tailscale-api.sh"
need E2E_CLUSTER E2E_OUT HCLOUD_TOKEN
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
  echo "the API server does not answer: going straight to Hetzner"
fi

log "2. force-delete and sweep what the cluster label finds"
left="$(cluster_servers | wc -l | tr -d ' ')"
[[ "${left}" == 0 ]] || echo "NOTE: ${left} server(s) were still there and are deleted by the sweeper"
(cd "${root}" && go run ./test/e2e-hcloud/sweeper -cluster "${E2E_CLUSTER}" -max-age 0 -wait 5m) ||
  problem "the sweeper failed"
# The brought network of the byo scenario (the sweeper cannot delete networks): the servers are
# gone, so it has nothing attached.
for id in $(cluster_leaks network 2>/dev/null); do
  hc network delete "${id}" >/dev/null 2>&1 || problem "could not delete the network ${id}"
done

log "3. nothing labelled with ${E2E_CLUSTER} may remain"
for kind in server volume placement-group firewall network primary-ip; do
  out="$(cluster_leaks "${kind}" 2>&1)" || {
    problem "could not list the ${kind}s: ${out}"
    continue
  }
  [[ -z "${out}" ]] || problem "${kind}s left: ${out//$'\n'/ }"
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
