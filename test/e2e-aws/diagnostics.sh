#!/usr/bin/env bash
# L5: what to look at after a failed run, collected into E2E_OUT/diag: the controller's
# logs, events, the NodeClasses, NodePools and NodeClaims, the CSRs, the pods, the k3s
# journal and the serial console output of every instance of the cluster
# (get-console-output, also of terminated ones within the hour EC2 keeps them).
#
# The repository is public and so are the artifacts of its runs: everything passes
# through redact(), which removes account IDs, tailnet host names, auth keys and
# credential-shaped strings. Review a file before sharing it anyway.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
need E2E_OUT
diag="${E2E_OUT}/diag"
mkdir -p "${diag}"

# redact: stdin to stdout without what must not be published.
redact() {
  sed -E \
    -e 's/[0-9]{12}/<account>/g' \
    -e 's/tskey-[A-Za-z0-9_-]+/<tskey>/g' \
    -e 's/[A-Za-z0-9-]+\.ts\.net/<tailnet-host>/g' \
    -e 's/(AKIA|ASIA)[A-Z0-9]{16}/<aws-key-id>/g' \
    -e 's/eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*/<jwt>/g' \
    -e 's/(AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN)=[^ ]+/\1=<redacted>/g' \
    -e 's/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/<email>/g' \
    -e 's/\b[a-z0-9]{6}\.[a-z0-9]{16}\b/<bootstrap-token>/g'
}

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

# The instances of the cluster in any state, and the serial console of each.
if command -v aws >/dev/null 2>&1; then
  collect instances.json ec2 describe-instances --filters "Name=tag:${ctag},Values=${E2E_CLUSTER}" \
    --query 'Reservations[].Instances[].{id:InstanceId,type:InstanceType,state:State.Name,reason:StateTransitionReason,launched:LaunchTime,az:Placement.AvailabilityZone,lifecycle:InstanceLifecycle,tags:Tags}'
  collect network.json bash -c "
    aws --region '${AWS_REGION}' ec2 describe-security-groups --filters 'Name=tag:${ctag},Values=${E2E_CLUSTER}' --query 'SecurityGroups[].{id:GroupId,name:GroupName,vpc:VpcId,tags:Tags}';
    aws --region '${AWS_REGION}' ec2 describe-placement-groups --filters 'Name=tag:${ctag},Values=${E2E_CLUSTER}';
    aws --region '${AWS_REGION}' ec2 describe-vpcs --filters 'Name=tag:${ctag},Values=${E2E_CLUSTER}' --query 'Vpcs[].{id:VpcId,cidr:CidrBlock,tags:Tags}'"
  for id in $(ec2 describe-instances --filters "Name=tag:${ctag},Values=${E2E_CLUSTER}" \
    --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null); do
    collect "console-${id}.txt" ec2 get-console-output --instance-id "${id}" --latest --query Output --output text
  done
fi
# The scenario logs, the report and the notes are uploaded too: the same redaction
# (the job log masks the account ID, a file does not).
for f in "${E2E_OUT}"/logs/*.log "${E2E_OUT}/report.txt" "${E2E_OUT}/notes.txt"; do
  [[ -f "${f}" ]] || continue
  redact <"${f}" >"${f}.redacted" && mv "${f}.redacted" "${f}"
done
echo "diagnostics in ${diag}"
