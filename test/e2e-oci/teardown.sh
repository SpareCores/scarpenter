#!/usr/bin/env bash
# L5 teardown (the workflow runs it with `if: always()`), in the
# order of overview section 13.4:
#   1. delete SCarpenter's NodePools and wait for the NodeClaims to go (the controller
#      deletes the instances), if the controller still runs;
#   2. force-delete anything tagged with the run's cluster and delete what it left (groups,
#      subnets, route tables, gateways, security lists, the managed VCN, what the byo scenario
#      brought, and boot volumes with the run's cluster tag) with the sweeper, ignoring the age limit;
#   3. check that nothing of the run remains: no instance, no network resource by the run's tags and
#      name prefixes (plan 25 section 11), and no boot volume created since the run began (a
#      terminate deletes them; one without the cluster tag is never deleted by a script, so a leak
#      is reported for a person to look at): the job fails when something does;
#   4. revoke the nodes' auth key, end the session loop and take the runner off the tailnet.
# Every step runs even when an earlier one failed; the exit status is the sum.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-oci/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-oci/tailscale-api.sh
source "${here}/tailscale-api.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT OCI_COMPARTMENT OCI_TENANCY OCI_REGION
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
  echo "the API server does not answer: going straight to OCI"
fi

log "2. force-delete and sweep what the cluster tag and the run's marker find"
left="$(cluster_instances | wc -l | tr -d ' ')"
[[ "${left}" == 0 ]] || echo "NOTE: ${left} instance(s) were still there and are deleted by the sweeper"
(cd "${root}" && go run ./test/e2e-oci/sweeper -cluster "${E2E_CLUSTER}" -max-age 0 -regions "${OCI_REGION}" -boot-volumes -wait 5m) ||
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
# net_left <oci network collection>: the display names of the run's resources in the collection that
# are not terminated: SCarpenter's managed ones (the tag and the name prefix) and the lane's own byo ones.
net_left() {
  oc network "$1" list --compartment-id "${OCI_COMPARTMENT}" --all --output json |
    jq -r --arg c "${E2E_CLUSTER}" --arg p "scarpenter-${E2E_CLUSTER}-" --arg b "scarpenter-e2e-byo-${E2E_RUN_ID}" '
      (.data // [])[] | select(."lifecycle-state" != "TERMINATED")
      | select((."freeform-tags"["scarpenter-cluster"] == $c) or ((."display-name" // "") | startswith($p)) or ((."display-name" // "") | startswith($b)))
      | ."display-name"'
}
leaks instances cluster_instances
for kind in nsg subnet route-table internet-gateway security-list vcn; do
  leaks "${kind}s" net_left "${kind}"
done
# Boot volumes go with their instance (the deletion runs after the terminate). One that stays is
# reported, not deleted: without the run's tag nothing proves it is the run's.
deadline=$((SECONDS + 120))
until [[ -z "$(new_boot_volumes 2>/dev/null)" ]]; do
  if [[ "${SECONDS}" -ge "${deadline}" ]]; then
    problem "boot volumes created since the run began are left (delete them by hand once checked): $(new_boot_volumes 2>&1)"
    break
  fi
  sleep 10
done

log "4. the auth key, the session and the tailnet"
if [[ -n "${TS_OAUTH_CLIENT_ID:-}" && -n "${TS_AUDIENCE:-}" ]]; then
  ts_revoke_key "${E2E_OUT}/ts-authkey.id" || echo "could not revoke the auth key (it expires after an hour anyway)"
fi
[[ -z "${OCI_SESSION_DIR:-}" ]] || "${here}/oci-session.sh" stop "${OCI_SESSION_DIR}" || true
sudo tailscale logout >/dev/null 2>&1 || true

if [[ "${failures}" -ne 0 ]]; then
  echo "teardown: ${failures} problem(s)" >&2
  exit 1
fi
echo "teardown: clean"
