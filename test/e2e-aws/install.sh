#!/usr/bin/env bash
# L5 steps 3 and 4: credentials, image and chart. In order:
#   1. the namespace, the tailscale auth key Secret and the AWS credentials Secret
#      (the temporary credentials of the CI role, from the environment the
#      configure-aws-credentials action sets);
#   2. the controller image, built from this checkout (the production build: the AWS
#      driver only) and imported into k3s's containerd;
#   3. the CRDs (the chart's, applied first because the NodeClasses must exist
#      before the controller starts: the label keys of their metrics are registered
#      with Karpenter core at start-up), then the NodeClasses;
#   4. the chart (not manifests), with the cluster name scarpenter-ci-<run id> (and the image
#      copies when E2E_SCENARIOS names the manual imagecopy scenario);
#   5. the NodePools, and the wait for the NodeClasses to be Ready.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT KUBECONFIG E2E_API_HOST AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
root="${e2e_root}"

log "namespace and Secrets"
kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "${ns}" create secret generic ts-node-key --from-file=authkey="${E2E_OUT}/ts-authkey" \
  --dry-run=client -o yaml | kubectl apply -f -
keeper_key_args "${ns}"
# printf is a builtin: the values do not appear in a process list.
kubectl -n "${ns}" create secret generic aws-credentials \
  --from-env-file=<(printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\nAWS_SESSION_TOKEN=%s\n' \
    "${AWS_ACCESS_KEY_ID}" "${AWS_SECRET_ACCESS_KEY}" "${AWS_SESSION_TOKEN}") \
  --dry-run=client -o yaml | kubectl apply -f -

log "building the controller image and importing it into k3s"
version="e2e-${E2E_RUN_ID}"
docker build -q --build-arg VERSION="${version}" --build-arg COMMIT="${GITHUB_SHA:-unknown}" \
  -t scarpenter-controller:e2e "${root}"
docker save scarpenter-controller:e2e | sudo k3s ctr images import - >/dev/null

log "CRDs and NodeClasses"
kubectl apply --server-side -f "${root}/charts/scarpenter/crds/"
wait_condition Established 60 crd/scnodeclasses.scarpenter.sparecores.com \
  crd/nodepools.karpenter.sh crd/nodeclaims.karpenter.sh
for f in nodeclass-amd64 nodeclass-arm64 nodeclass-drift; do
  render "${here}/manifests/${f}.yaml" | kubectl apply -f -
done

# The manual imagecopy scenario (plan 29 step 4) needs the controller's image copies on, with a
# 1 minute idle lifetime so that the deletion happens within the scenario (the GC interval is 1m in
# the lane's values). No other run turns them on: the other NodeClasses name a region with an AMI.
copy_args=()
if [[ ",${E2E_SCENARIOS:-}," == *,imagecopy,* ]]; then
  copy_args=(--set vendors.aws.imageCopies.enabled=true --set imageCopies.idleTTL=1m)
fi

log "installing the chart"
values="$(mktemp)"
render "${here}/manifests/chart-values.yaml" >"${values}"
helm upgrade --install scarpenter "${root}/charts/scarpenter" --namespace "${ns}" \
  -f "${values}" --set image.repository=scarpenter-controller --set image.tag=e2e \
  --set image.pullPolicy=Never "${keeper_helm_args[@]}" "${copy_args[@]}" --wait --timeout 6m
rm -f "${values}"

log "NodePools"
render "${here}/manifests/nodepools.yaml" | kubectl apply -f -
for nc in e2e-amd64 e2e-arm64 e2e-drift; do
  wait_condition Ready 600 "scnodeclass/${nc}" ||
    {
      kubectl get scnodeclass "${nc}" -o yaml
      exit 1
    }
done
kubectl -n "${ns}" get pods -o wide
