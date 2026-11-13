#!/usr/bin/env bash
# L5 steps 3 and 4: credentials, image and chart. In order:
#   1. the namespace, the tailscale auth key Secret and the Alibaba Cloud credentials
#      Secret (a RAM role session of two hours: ali_login exchanges the GitHub OIDC token
#      the workflow's credentials step wrote, because the cluster's own service account
#      issuer is not public and the chart's `oidc` mode cannot be tested on a runner);
#   2. the controller image, built from this checkout (the production build) and
#      imported into k3s's containerd;
#   3. the CRDs (the chart's, applied first because the NodeClasses must exist
#      before the controller starts: the label keys of their metrics are registered
#      with Karpenter core at start-up), then the NodeClasses;
#   4. the chart (not manifests), with the cluster name scarpenter-ci-<run id>;
#   5. the NodePools, and the wait for the NodeClasses to be Ready.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-alicloud/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT KUBECONFIG E2E_API_HOST ALICLOUD_REGION
root="${e2e_root}"
# First: the OIDC token the workflow wrote lives only minutes.
ali_login
need ALIBABA_CLOUD_ACCESS_KEY_ID ALIBABA_CLOUD_ACCESS_KEY_SECRET ALIBABA_CLOUD_SECURITY_TOKEN

log "namespace and Secrets"
kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "${ns}" create secret generic ts-node-key --from-file=authkey="${E2E_OUT}/ts-authkey" \
  --dry-run=client -o yaml | kubectl apply -f -
keeper_key_args "${ns}"
# printf is a builtin: the values do not appear in a process list.
kubectl -n "${ns}" create secret generic alicloud-credentials \
  --from-env-file=<(printf 'ALIBABA_CLOUD_ACCESS_KEY_ID=%s\nALIBABA_CLOUD_ACCESS_KEY_SECRET=%s\nALIBABA_CLOUD_SECURITY_TOKEN=%s\n' \
    "${ALIBABA_CLOUD_ACCESS_KEY_ID}" "${ALIBABA_CLOUD_ACCESS_KEY_SECRET}" "${ALIBABA_CLOUD_SECURITY_TOKEN}") \
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
for f in nodeclass-amd64 nodeclass-arm64 nodeclass-drift nodeclass-hardened; do
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
for nc in e2e-amd64 e2e-arm64 e2e-drift e2e-hardened; do
  kubectl wait --for=condition=Ready "scnodeclass/${nc}" --timeout=600s ||
    {
      kubectl get scnodeclass "${nc}" -o yaml
      exit 1
    }
done
kubectl -n "${ns}" get pods -o wide
