#!/usr/bin/env bash
# L5: what to look at after a failed run, collected into E2E_OUT/diag: the controller's
# logs, events, the NodeClasses, NodePools and NodeClaims, the CSRs, the pods, the k3s
# journal and the console output of every instance of the cluster that still exists
# (GetInstanceConsoleOutput: the Ignition run and the kernel log, where the aliyun
# provider's fetch of the user-data shows).
#
# The repository is public and so are the artifacts of its runs: everything passes
# through redact() (lib.sh, shared with the live output of the scenarios), which removes account IDs, tailnet host names, auth keys and
# credential-shaped strings. Review a file before sharing it anyway.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-alicloud/lib.sh
source "${here}/lib.sh"
need E2E_OUT ALICLOUD_REGION
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

# The instances of the cluster and the resources tagged with it, and the console of each
# instance. The credentials come from ali_login (the OIDC token of the run, exchanged once
# per job; a failure is reported in the file, not fatal).
# ali_login prints ::add-mask:: commands with the secrets on stderr: never into a file.
if command -v aliyun >/dev/null 2>&1 && ali_login 2> >(grep -v '^::add-mask::' | redact >"${diag}/ali-login.txt"); then
  mapfile -t tag < <(cluster_tag_args)
  collect instances.json cluster_instances_json
  collect security-groups.json ecs DescribeSecurityGroups "${tag[@]}"
  collect deployment-sets.json ecs DescribeDeploymentSets --PageSize 100
  collect vpcs.json vpc DescribeVpcs "${tag[@]}"
  collect vswitches.json vpc DescribeVSwitches "${tag[@]}"
  for id in $(cluster_instances 2>/dev/null); do
    collect "disks-${id}.json" instance_disks "${id}"
    collect "console-${id}.txt" instance_console "${id}"
  done
else
  echo "the Alibaba Cloud CLI or its credentials are not available: no cloud diagnostics" >>"${diag}/ali-login.txt"
fi
# The scenario logs, the report and the notes are uploaded too: the same redaction
# (the job log masks the account ID, a file does not).
for f in "${E2E_OUT}"/logs/*.log "${E2E_OUT}/report.txt" "${E2E_OUT}/notes.txt"; do
  [[ -f "${f}" ]] || continue
  redact <"${f}" >"${f}.redacted" && mv "${f}.redacted" "${f}"
done
echo "diagnostics in ${diag}"
