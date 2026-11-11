#!/usr/bin/env bash
# L5 teardown (the workflow runs it with `if: always()`), in the order of overview section
# 13.4:
#   1. delete SCarpenter's NodePools and wait for the NodeClaims to go (the controller
#      deletes the VMs), if the controller still runs;
#   2. force-delete anything of the lane's resource group tagged with the run's cluster with
#      the sweeper (test/e2e-azure/sweeper, whatever the age: VMs, NICs, public IPs, disks by
#      name prefix, then the VNets, NSGs, application security groups and proximity placement
#      groups, the brought VNet and NSG of the byo scenario among them), and again by hand
#      with az for what it left. The resource group itself is the operator's and stays. The
#      scheduled sweeper (e2e-azure-sweeper.yml) does the same by age for runs that died
#      without a teardown;
#   3. check that nothing tagged with the cluster remains: no VM, NIC, public IP, NSG, VNet,
#      proximity placement group, and no disk of a VM of the run (disks carry no tag: their
#      names end in the cluster's hash): the job fails when something does;
#   4. stop the federated token's renewal, revoke the nodes' auth key and take the runner
#      off the tailnet.
# Every step runs even when an earlier one failed; the exit status is the sum. The az session
# was logged in again by the workflow (the first one may be an hour old).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-azure/lib.sh
source "${here}/lib.sh"
# shellcheck source=test/e2e-azure/tailscale-api.sh
source "${here}/tailscale-api.sh"
need E2E_CLUSTER E2E_OUT AZURE_REGION AZURE_RESOURCE_GROUP
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
        echo "NodeClaims are left after 300 s: the VMs are force-deleted next" >&2
        break
      fi
      sleep 5
    done
  else
    echo "the controller is gone (uninstalled): nothing deletes NodeClaims any more"
  fi
else
  echo "the API server does not answer: going straight to Azure"
fi

log "2. force-delete what the cluster tag finds in ${AZURE_RESOURCE_GROUP}"
left="$(cluster_vms all 2>/dev/null | wc -l | tr -d ' ')"
[[ "${left}" == 0 ]] || echo "NOTE: ${left} VM(s) were still there and are force-deleted by the sweeper"
# The sweeper of the lane, for this one cluster and without the age limit (the az session of the
# runner is its credential: the driver's chain accepts the Azure CLI under the development flag).
(cd "${root}" && SCARPENTER_AZURE_DEVELOPMENT=true go run ./test/e2e-azure/sweeper -cluster "${E2E_CLUSTER}" -max-age 0) ||
  problem "the sweeper failed"
# And the same by hand, in case the sweeper could not run or left something (nothing to do when
# it was thorough): the leftovers of the cascade and of a failed create, in dependency order.

# delete_type <resource type> [force]: deletes the cluster's resources of a type, one by one
# (no --no-wait: the next type depends on this one being gone); a failure is a problem only
# if the resource is still there at the checks of step 3.
delete_type() {
  local type="$1" id ids
  ids="$(az resource list --resource-group "${AZURE_RESOURCE_GROUP}" --tag "${ctag}=${E2E_CLUSTER}" --resource-type "${type}" --query '[].id' --output tsv 2>/dev/null | tr -d '\r')"
  for id in ${ids}; do
    echo "deleting ${id##*/} (${type})"
    if [[ "${type}" == Microsoft.Compute/virtualMachines ]]; then
      az vm delete --ids "${id}" --force-deletion true --yes >/dev/null 2>&1 || echo "could not delete ${id##*/}" >&2
    else
      az resource delete --ids "${id}" >/dev/null 2>&1 || echo "could not delete ${id##*/}" >&2
    fi
  done
}
delete_disks() { # the disks of the run's VMs, by the hash their names end with
  local d
  for d in $(cluster_disks 2>/dev/null); do
    echo "deleting the disk ${d}"
    az disk delete --resource-group "${AZURE_RESOURCE_GROUP}" --name "${d}" --yes >/dev/null 2>&1 || echo "could not delete the disk ${d}" >&2
  done
}
for pass in 1 2 3; do
  delete_type Microsoft.Compute/virtualMachines
  delete_type Microsoft.Network/networkInterfaces
  delete_type Microsoft.Network/publicIPAddresses
  delete_disks
  delete_type Microsoft.Network/virtualNetworks
  delete_type Microsoft.Network/networkSecurityGroups
  delete_type Microsoft.Network/applicationSecurityGroups
  delete_type Microsoft.Compute/proximityPlacementGroups
  [[ -z "$(cluster_resources 2>/dev/null)$(cluster_disks 2>/dev/null)" ]] && break
  echo "pass ${pass}: resources are left (a dependency still held them); trying again"
  sleep 20
done

log "3. nothing tagged with ${E2E_CLUSTER} may remain"
leaks() { # leaks <what> <command printing the names or IDs>
  local out
  out="$("${@:2}" 2>&1)" || {
    problem "could not list ${1}: ${out}"
    return
  }
  [[ -z "${out}" ]] || problem "${1} left: ${out}"
}
# The deletes are asynchronous at Azure: give the cascade (NIC, public IP, disks) a while.
deadline=$((SECONDS + 180))
until [[ -z "$(cluster_resources 2>/dev/null)$(cluster_disks 2>/dev/null)" ]]; do
  [[ "${SECONDS}" -lt "${deadline}" ]] || break
  sleep 10
done
leaks "VMs" cluster_vms all
leaks "resources tagged with the cluster" cluster_resources
leaks "disks of the run's VMs" cluster_disks
leaks "managed resources" managed_resources

log "4. the token renewal, the auth key and the tailnet"
"${here}/azure-token.sh" stop "${E2E_OUT}/azure-token" || true
if [[ -n "${TS_OAUTH_CLIENT_ID:-}" && -n "${TS_AUDIENCE:-}" ]]; then
  ts_revoke_key "${E2E_OUT}/ts-authkey.id" || echo "could not revoke the auth key (it expires after an hour anyway)"
fi
sudo tailscale logout >/dev/null 2>&1 || true

if [[ "${failures}" -ne 0 ]]; then
  echo "teardown: ${failures} problem(s)" >&2
  exit 1
fi
echo "teardown: clean"
