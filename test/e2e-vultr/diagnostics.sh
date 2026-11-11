#!/usr/bin/env bash
# L5: what to look at after a failed run, collected into E2E_OUT/diag: the controller's
# logs, events, the NodeClasses, NodePools and NodeClaims, the CSRs, the pods, the k3s
# journal and the instances, firewall groups and VPCs of the cluster (Vultr has no
# console output call; the instance list shows status, power and server status).
#
# The repository is public and so are the artifacts of its runs: everything passes
# through redact() (lib.sh, shared with the live output of the scenarios), which removes tailnet host names, auth keys and
# credential-shaped strings, the API key and instance passwords. Review a file before sharing it anyway.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-vultr/lib.sh
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

# The instances of the cluster and the groups and VPCs SCarpenter made for it. The default
# password of an instance is not in the list answer, and redact() removes it anyway.
diag_instances() {
  vapi "/instances?per_page=500&tag=$(uri "${ctag}=${E2E_CLUSTER}")" |
    jq '[.instances[] | {id, plan, region, status, power_status, server_status, date_created, firewall_group_id, tags}]'
}
diag_network() {
  groups_json | jq --arg c "${E2E_CLUSTER}" '[.[] | select(.description | contains($c))]'
  vpcs_json | jq --arg c "${E2E_CLUSTER}" '[.[] | select(.description | contains($c))]'
}
if [[ -n "${VULTR_API_KEY:-}" ]]; then
  collect instances.json diag_instances
  collect network.json diag_network
fi
# The scenario logs, the report and the notes are uploaded too: the same redaction
# (the job log masks the account ID, a file does not).
for f in "${E2E_OUT}"/logs/*.log "${E2E_OUT}/report.txt" "${E2E_OUT}/notes.txt"; do
  [[ -f "${f}" ]] || continue
  redact <"${f}" >"${f}.redacted" && mv "${f}.redacted" "${f}"
done
echo "diagnostics in ${diag}"
