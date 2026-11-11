#!/usr/bin/env bash
# L5: what to look at after a failed run, collected into E2E_OUT/diag: the controller's
# logs, events, the NodeClasses, NodePools and NodeClaims, the CSRs, the pods, the k3s
# journal and the serial console output of every server of the cluster.
#
# The repository is public and so are the artifacts of its runs: everything passes
# through redact() (lib.sh, shared with the live output of the scenarios), which removes OpenStack IDs and passwords, tailnet host names, auth keys and
# credential-shaped strings. Review a file before sharing it anyway.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-ovh/lib.sh
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

# The servers of the cluster, their groups and the project's limits, and the console log of
# each server (the serial console Nova keeps).
if command -v openstack >/dev/null 2>&1; then
  collect servers.json os server list --tags "$(cluster_tag)" --long -f json
  collect groups.json bash -c "
    openstack --os-region-name '${OS_REGION_NAME}' security group list -f json;
    openstack --os-region-name '${OS_REGION_NAME}' server group list -f json"
  collect limits.json os limits show --absolute -f json
  for id in $(cluster_instances 2>/dev/null); do
    collect "console-${id}.txt" os console log show "${id}"
  done
fi
# The scenario logs, the report and the notes are uploaded too: the same redaction
# (the job log masks the account ID, a file does not).
for f in "${E2E_OUT}"/logs/*.log "${E2E_OUT}/report.txt" "${E2E_OUT}/notes.txt"; do
  [[ -f "${f}" ]] || continue
  redact <"${f}" >"${f}.redacted" && mv "${f}.redacted" "${f}"
done
echo "diagnostics in ${diag}"
