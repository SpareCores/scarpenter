#!/usr/bin/env bash
# L5: what to look at after a failed run, collected into E2E_OUT/diag: the controller's
# logs, events, the NodeClasses, NodePools and NodeClaims, the CSRs, the pods, the k3s
# journal and, from the UpCloud API, the cluster's servers, storages, networks,
# rulesets and server groups with their labels. The VNC console is not available over the
# API: there is no console output to collect.
#
# The repository is public and so are the artifacts of its runs: everything passes
# through redact() (lib.sh, shared with the live output of the scenarios), which removes
# tailnet host names, auth keys, the API token and credential-shaped strings. Review a file
# before sharing it anyway.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-upcloud/lib.sh
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

# The cluster's servers in any state (details carry the labels, the plan, the state and the
# addresses), and what hangs off them.
if [[ -n "${UPCLOUD_TOKEN:-}" ]] && command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
  collect servers.json cluster_server_details
  # The user data of a server is the node's whole configuration, with the join secrets: it
  # is never collected (the details answer may carry it, so it is deleted here).
  jq 'map(del(.user_data))' "${diag}/servers.json" >"${diag}/servers.json.tmp" 2>/dev/null && mv "${diag}/servers.json.tmp" "${diag}/servers.json"
  collect storages.json cluster_storage_details
  collect networks.txt cluster_networks
  collect rulesets.txt cluster_rulesets
  collect server-groups.txt cluster_groups
  collect account.json account_limits
fi
# The scenario logs, the report and the notes are uploaded too: the same redaction.
for f in "${E2E_OUT}"/logs/*.log "${E2E_OUT}/report.txt" "${E2E_OUT}/notes.txt"; do
  [[ -f "${f}" ]] || continue
  redact <"${f}" >"${f}.redacted" && mv "${f}.redacted" "${f}"
done
echo "diagnostics in ${diag}"
