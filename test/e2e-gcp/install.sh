#!/usr/bin/env bash
# L5 steps 3 and 4: credentials, image and chart. In order:
#   1. the namespace, the tailscale auth key Secret and the Google credentials Secret
#      (the external_account credential file that the google-github-actions/auth step wrote:
#      the job's GitHub OIDC token exchanged through workload identity federation, no key
#      file and no long-lived secret; the file is what GOOGLE_APPLICATION_CREDENTIALS names);
#   2. the controller image, built from this checkout (the production build: the supported
#      vendors' drivers only) and imported into k3s's containerd;
#   3. the CRDs (the chart's, applied first because the NodeClasses must exist
#      before the controller starts: the label keys of their metrics are registered
#      with Karpenter core at start-up), then the NodeClasses;
#   4. the chart (not manifests), with the cluster name scarpenter-ci-<run id>;
#   5. the NodePools, and the wait for the NodeClasses to be Ready.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-gcp/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT KUBECONFIG E2E_API_HOST GCP_PROJECT GCP_REGION GOOGLE_APPLICATION_CREDENTIALS
root="${e2e_root}"
[[ -s "${GOOGLE_APPLICATION_CREDENTIALS}" ]] || {
  echo "GOOGLE_APPLICATION_CREDENTIALS names no file: the google-github-actions/auth step did not run" >&2
  exit 1
}

# The classes that exist before the controller starts, and the ones that must become Ready.
classes=(e2e-amd64 e2e-drift)
nodeclass_files=(nodeclass-amd64 nodeclass-drift)
if [[ "${E2E_ARM64}" == true ]]; then
  classes+=(e2e-arm64)
  nodeclass_files+=(nodeclass-arm64)
fi

log "namespace and Secrets"
kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "${ns}" create secret generic ts-node-key --from-file=authkey="${E2E_OUT}/ts-authkey" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "${ns}" create secret generic gcp-credentials --from-file=credentials.json="${GOOGLE_APPLICATION_CREDENTIALS}" \
  --dry-run=client -o yaml | kubectl apply -f -

log "building the controller image and importing it into k3s"
version="e2e-${E2E_RUN_ID}"
docker build -q --build-arg VERSION="${version}" --build-arg COMMIT="${GITHUB_SHA:-unknown}" \
  -t scarpenter-controller:e2e "${root}"
docker save scarpenter-controller:e2e | sudo k3s ctr images import - >/dev/null

log "CRDs and NodeClasses"
kubectl apply --server-side -f "${root}/charts/scarpenter/crds/"
kubectl wait --for=condition=Established crd/scnodeclasses.scarpenter.sparecores.com \
  crd/nodepools.karpenter.sh crd/nodeclaims.karpenter.sh --timeout=60s
for f in "${nodeclass_files[@]}"; do
  render "${here}/manifests/${f}.yaml" | kubectl apply -f -
done

log "installing the chart"
values="$(mktemp)"
render "${here}/manifests/chart-values.yaml" >"${values}"
helm upgrade --install scarpenter "${root}/charts/scarpenter" --namespace "${ns}" \
  -f "${values}" --set image.repository=scarpenter-controller --set image.tag=e2e \
  --set image.pullPolicy=Never --wait --timeout 6m
rm -f "${values}"

log "NodePools"
render "${here}/manifests/nodepools.yaml" | kubectl apply -f -
[[ "${E2E_ARM64}" != true ]] || render "${here}/manifests/nodepool-arm64.yaml" | kubectl apply -f -
for nc in "${classes[@]}"; do
  kubectl wait --for=condition=Ready "scnodeclass/${nc}" --timeout=600s ||
    {
      kubectl get scnodeclass "${nc}" -o yaml
      exit 1
    }
done
kubectl -n "${ns}" get pods -o wide
