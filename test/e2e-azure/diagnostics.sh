#!/usr/bin/env bash
# L5: what to look at after a failed run, collected into E2E_OUT/diag: the controller's
# logs, events, the NodeClasses, NodePools and NodeClaims, the CSRs, the pods, the k3s
# journal, the VMs of the cluster, the activity log of the lane's resource group and the
# boot log (serial console) of every VM that still exists (az vm boot-diagnostics get-boot-log).
#
# The repository is public and so are the artifacts of its runs: everything passes
# through redact() (lib.sh, shared with the live output of the scenarios), which removes subscription and tenant
# IDs, tailnet host names, auth keys and credential-shaped strings. Review a file before sharing it anyway.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-azure/lib.sh
source "${here}/lib.sh"
need E2E_OUT
diag="${E2E_OUT}/diag"
mkdir -p "${diag}"

collect() { # collect <file> <command...>
  local f="${diag}/$1"
  shift
  { "$@" 2>&1 || true; } | redact >"${f}"
}

if kubectl get --raw /readyz >/dev/null 2>&1; then
  collect nodes.txt kubectl get nodes -o wide
  collect pods.txt kubectl get pods -A -o wide
  collect scarpenter-objects.yaml kubectl get scnodeclasses,nodepools,nodeclaims -o yaml
  collect events.txt kubectl get events -A --sort-by=.lastTimestamp
  collect csr.txt kubectl get csr
  collect controller.log kubectl -n "${ns}" logs deploy/scarpenter --tail=-1
  collect controller-previous.log kubectl -n "${ns}" logs deploy/scarpenter --previous --tail=-1
  collect helm-values.yaml helm -n "${ns}" get values scarpenter
  collect metrics.txt kubectl get --raw "/api/v1/namespaces/${ns}/services/scarpenter:metrics/proxy/metrics"
  collect describe-nodes.txt kubectl describe nodes
  collect flannel.log kubectl -n kube-flannel logs -l app=flannel --tail=200 --prefix
else
  echo "the API server does not answer" >"${diag}/apiserver.txt"
fi
collect k3s.log sudo journalctl -u k3s-e2e --no-pager
# Only the runner itself: the peer list can name other devices and users of the tailnet.
collect tailscale-status.txt tailscale status --peers=false

# net_overview: the network resources of the cluster in the lane's group, by selected fields.
net_overview() {
  local q="[?tags.\"${ctag}\"=='${E2E_CLUSTER}']"
  az network nsg list --resource-group "${rg}" --query "${q}.{name:name,state:provisioningState,rules:securityRules[].{name:name,priority:priority,access:access,direction:direction}}"
  az network vnet list --resource-group "${rg}" --query "${q}.{name:name,prefixes:addressSpace.addressPrefixes,subnets:subnets[].name}"
  az ppg list --resource-group "${rg}" --query "${q}.{name:name,state:provisioningState}"
  az network nic list --resource-group "${rg}" --query "${q}.{name:name,state:provisioningState}"
  az network public-ip list --resource-group "${rg}" --query "${q}.{name:name,state:provisioningState}"
}
# The VMs of the cluster, what surrounds them, the activity log of the group and the boot
# log (the serial console) of every VM that still exists. Azure keeps no terminated VM, so a
# VM that is already gone has no boot log: the activity log is what is left of it.
if command -v az >/dev/null 2>&1; then
  rg="${AZURE_RESOURCE_GROUP:-}"
  if [[ -n "${rg}" ]]; then
    collect vms.json az vm list --resource-group "${rg}" --show-details \
      --query "[?tags.\"${ctag}\"=='${E2E_CLUSTER}'].{name:name,size:hardwareProfile.vmSize,zones:zones,priority:priority,state:provisioningState,power:powerState,tags:tags}"
    collect network.json net_overview
    collect disks.json az disk list --resource-group "${rg}" \
      --query "[?contains(name, '-$(cluster_hash)-')].{name:name,size:diskSizeGb,sku:sku.name,state:diskState,managedBy:managedBy}"
    # The activity log of the group: why a create or a delete failed (policy denials, quota,
    # allocation) when the controller's own log is not enough.
    collect activity-log.json az monitor activity-log list --resource-group "${rg}" --offset 3h \
      --query "[].{time:eventTimestamp,op:operationName.value,status:status.value,sub:subStatus.value,resource:resourceId,message:properties.statusMessage}"
    for name in $(az vm list --resource-group "${rg}" --query "[?tags.\"${ctag}\"=='${E2E_CLUSTER}'].name" --output tsv 2>/dev/null | tr -d '\r'); do
      collect "bootlog-${name}.txt" az vm boot-diagnostics get-boot-log --resource-group "${rg}" --name "${name}"
    done
  fi
fi
# The scenario logs, the report and the notes are uploaded too: the same redaction
# (the job log masks only what the workflow registered, a file nothing).
for f in "${E2E_OUT}"/logs/*.log "${E2E_OUT}/report.txt" "${E2E_OUT}/notes.txt"; do
  [[ -f "${f}" ]] || continue
  redact <"${f}" >"${f}.redacted" && mv "${f}.redacted" "${f}"
done
echo "diagnostics in ${diag}"
