#!/usr/bin/env bash
# L5: what to look at after a failed run, collected into E2E_OUT/diag: the controller's
# logs, events, the NodeClasses, NodePools and NodeClaims, the CSRs, the pods, the k3s
# journal and the serial console output of every instance of the cluster
# (get-serial-port-output; a deleted instance is gone from the API and so is its console),
# and the cluster's resources in the project.
#
# The repository is public and so are the artifacts of its runs: everything passes
# through redact() (lib.sh, shared with the live output of the scenarios), which removes the
# project id and number, tailnet host names, auth keys, Google tokens and keys, and other
# credential-shaped strings. Review a file before sharing it anyway.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-gcp/lib.sh
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

# The instances of the cluster in any state, and the serial console of each; the disks,
# the managed network and what the lane brought. Every call has its own timeout, so that
# a hung gcloud cannot take the time of the teardown.
if command -v gcloud >/dev/null 2>&1 && [[ -n "${GCP_PROJECT:-}" && -n "${E2E_CLUSTER:-}" ]]; then
  gcq() { timeout 120 gcloud --project "${GCP_PROJECT}" --quiet "$@"; }
  prefix="scarpenter-${E2E_CLUSTER}-"
  collect instances.json gcq compute instances list --filter="labels.${ctag}=${E2E_CLUSTER}" \
    --format='json(name,zone.basename(),machineType.basename(),status,scheduling,creationTimestamp,labels,tags,metadata.items[].key,disks[].deviceName,networkInterfaces[].subnetwork.basename(),resourcePolicies)'
  collect disks.json gcq compute disks list --filter="labels.${ctag}=${E2E_CLUSTER}" \
    --format='json(name,zone.basename(),type.basename(),sizeGb,status,users,labels)'
  collect network.json bash -c "
    gcloud --project '${GCP_PROJECT}' --quiet compute networks list --filter='name~^${prefix} OR name~^scarpenter-e2e-byo-${E2E_RUN_ID:-0}' --format=json;
    gcloud --project '${GCP_PROJECT}' --quiet compute networks subnets list --filter='name~^${prefix} OR name~^scarpenter-e2e-byo-${E2E_RUN_ID:-0}' --format=json;
    gcloud --project '${GCP_PROJECT}' --quiet compute firewall-rules list --filter='name~^${prefix} OR name~^scarpenter-e2e-byo-${E2E_RUN_ID:-0}' --format=json;
    gcloud --project '${GCP_PROJECT}' --quiet compute resource-policies list --filter='name~^${prefix}' --format=json"
  collect quotas.json gcq compute regions describe "${GCP_REGION}" --format='json(name,quotas)'
  # The recent operations that failed: the asynchronous errors of insert (capacity, quota,
  # organization policy) are in the operation, not in the HTTP answer.
  collect failed-operations.json gcq compute operations list --filter="error:* AND insertTime>-PT3H" --limit=50 \
    --format='json(name,operationType,targetLink.basename(),status,error,httpErrorStatusCode,insertTime)'
  for inst in $(gcq compute instances list --filter="labels.${ctag}=${E2E_CLUSTER}" --format='value(zone.basename(),name)' 2>/dev/null | tr '\t' '/'); do
    collect "console-${inst//\//-}.txt" gcq compute instances get-serial-port-output "${inst#*/}" --zone "${inst%%/*}"
  done
fi
# The scenario logs, the report and the notes are uploaded too: the same redaction
# (the job log masks the secrets it knows, a file does not).
for f in "${E2E_OUT}"/logs/*.log "${E2E_OUT}/report.txt" "${E2E_OUT}/notes.txt"; do
  [[ -f "${f}" ]] || continue
  redact <"${f}" >"${f}.redacted" && mv "${f}.redacted" "${f}"
done
echo "diagnostics in ${diag}"
