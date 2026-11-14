#!/usr/bin/env bash
# L5: what to look at after a failed run, collected into E2E_OUT/diag: the controller's
# logs, events, the NodeClasses, NodePools and NodeClaims, the CSRs, the pods, the k3s
# journal and the console history of every instance of the cluster (the serial console output of
# an instance that booted; a terminated instance's console is gone with it), and the cluster's
# resources in the compartment.
#
# The repository is public and so are the artifacts of its runs: everything passes
# through redact() (lib.sh, shared with the live output of the scenarios), which removes the
# tenancy, compartment and user OCIDs, the identity domain, tailnet host names, auth keys, tokens
# and signatures, and other credential-shaped strings. Review a file before sharing it anyway.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-oci/lib.sh
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
collect session-loop.log cat "${OCI_SESSION_DIR:-/nonexistent}/loop.log"

# The instances of the cluster in any state, the console history of each, the boot volumes, the
# managed network and what the lane brought. Every call has its own timeout, so that a hung CLI
# cannot take the time of the teardown.
if command -v oci >/dev/null 2>&1 && [[ -n "${OCI_COMPARTMENT:-}" && -n "${E2E_CLUSTER:-}" ]]; then
  ocq() { timeout 120 oci "$@"; }
  prefix="scarpenter-${E2E_CLUSTER}-"
  byo="scarpenter-e2e-byo-${E2E_RUN_ID:-0}"
  collect instances.json bash -c "
    oci compute instance list --compartment-id '${OCI_COMPARTMENT}' --all --output json | jq --arg c '${E2E_CLUSTER}' '[(.data // [])[] |
      select(.\"freeform-tags\"[\"scarpenter-cluster\"] == \$c) | del(.metadata.user_data)]'"
  # The network of the cluster: by the ownership tag or the name prefix of a managed resource, or the lane's byo name.
  for kind in vcn subnet route-table internet-gateway security-list nsg; do
    collect "net-${kind}.json" bash -c "oci network ${kind} list --compartment-id '${OCI_COMPARTMENT}' --all --output json |
      jq --arg c '${E2E_CLUSTER}' --arg p '${prefix}' --arg b '${byo}' '[(.data // [])[] |
        select((.\"freeform-tags\"[\"scarpenter-cluster\"] == \$c) or ((.\"display-name\" // \"\") | startswith(\$p)) or ((.\"display-name\" // \"\") | startswith(\$b)))]'"
  done
  collect boot-volumes.json bash -c "
    for ad in \$(oci iam availability-domain list --compartment-id '${OCI_TENANCY}' --output json | jq -r '(.data // [])[].name'); do
      oci bv boot-volume list --compartment-id '${OCI_COMPARTMENT}' --availability-domain \"\$ad\" --all --output json |
        jq '[(.data // [])[] | {id, \"display-name\", \"lifecycle-state\", \"size-in-gbs\", \"time-created\", \"freeform-tags\"}]'
    done"
  collect limits-e5.txt bash -c "
    for ad in \$(oci iam availability-domain list --compartment-id '${OCI_TENANCY}' --output json | jq -r '(.data // [])[].name'); do
      for n in ${bucket_cores} ${bucket_memory}; do
        echo \"\$ad \$n\"; oci limits resource-availability get --service-name compute --limit-name \"\$n\" --compartment-id '${OCI_TENANCY}' --availability-domain \"\$ad\" --output json
      done
    done"
  # The console history of each instance: an instance that booted has one (capture, then read).
  for id in $(ocq compute instance list --compartment-id "${OCI_COMPARTMENT}" --all --output json 2>/dev/null |
    jq -r --arg c "${E2E_CLUSTER}" '(.data // [])[] | select(."freeform-tags"["scarpenter-cluster"] == $c and ."lifecycle-state" != "TERMINATED") | .id'); do
    short="${id##*.}"
    collect "console-${short:0:12}.txt" bash -c "
      h=\$(oci compute console-history capture --instance-id '${id}' --wait-for-state SUCCEEDED --output json | jq -r '.data.id');
      oci compute console-history get-content --instance-console-history-id \"\$h\" --file - --length 1000000"
  done
fi
# The scenario logs, the report and the notes are uploaded too: the same redaction
# (the job log masks the secrets it knows, a file does not).
for f in "${E2E_OUT}"/logs/*.log "${E2E_OUT}/report.txt" "${E2E_OUT}/notes.txt"; do
  [[ -f "${f}" ]] || continue
  redact <"${f}" >"${f}.redacted" && mv "${f}.redacted" "${f}"
done
echo "diagnostics in ${diag}"
