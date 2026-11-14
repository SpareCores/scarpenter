#!/usr/bin/env bash
# L5 steps 3 and 4: credentials, image and chart. In order:
#   1. the namespace, the tailscale auth key Secret and the two Secrets of the keyless
#      credentials (oci-federated-token: the OIDC token that oci-session.sh keeps fresh, the token
#      file the controller exchanges at the identity domain; oci-client: the confidential
#      application's client secret). No API key exists anywhere: the cluster's own ServiceAccount
#      issuer is not public, so the job's GitHub token stands in for the projected token;
#   2. the controller image, built from this checkout (the production build: the supported
#      vendors' drivers only) and imported into k3s's containerd;
#   3. the CRDs (the chart's, applied first because the NodeClasses must exist
#      before the controller starts: the label keys of their metrics are registered
#      with Karpenter core at start-up), then the NodeClasses;
#   4. the chart (not manifests), with the cluster name scarpenter-ci-<run id>;
#   5. the NodePools, and the wait for the NodeClasses to be Ready.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-oci/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT KUBECONFIG E2E_API_HOST OCI_COMPARTMENT OCI_TENANCY OCI_REGION \
  OCI_IDENTITY_DOMAIN_URL OCI_CLIENT_ID OCI_TOKEN_FILE OCI_CLIENT_SECRET_FILE
root="${e2e_root}"
[[ -s "${OCI_TOKEN_FILE}" && -s "${OCI_CLIENT_SECRET_FILE}" ]] || {
  echo "OCI_TOKEN_FILE or OCI_CLIENT_SECRET_FILE names no file: the session step (oci-session.sh start) did not run" >&2
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
keeper_key_args "${ns}"
# kubectl reads the files: no secret is on a command line. The token Secret is updated by the session loop.
kubectl -n "${ns}" create secret generic oci-federated-token --from-file=token="${OCI_TOKEN_FILE}" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "${ns}" create secret generic oci-client --from-file=client-secret="${OCI_CLIENT_SECRET_FILE}" \
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
  --set image.pullPolicy=Never "${keeper_helm_args[@]}" --wait --timeout 6m
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
