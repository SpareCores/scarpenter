#!/usr/bin/env bash
# L3: SCarpenter (a scarpenter_dev build: real Karpenter core as a library plus
# the fake driver) in a kind cluster, with kwok-controller simulating the
# nodes and the Keeper replay server serving the committed snapshot
# (docs/plans/v1/wip/00-overview.md section 13.2). Run it in the dev container:
#
#   hack/dev.sh make test-kwok
#
#   KWOK_SCENARIOS     a go test -run regexp selecting scenarios (default: all)
#   KEEP_CLUSTER=1     keep the kind cluster (and reuse it with REUSE_CLUSTER=1)
#   KWOK_SETUP_ONLY=1  stop once the cluster and the controller run, and keep
#                      the cluster (make kwok-up)
#   KIND_CLUSTER_NAME  cluster name (default scarpenter-kwok)
#
# Unlike the lane of sc-karpenter-nodepool there is no core "kwok" image, no
# core version matrix and no chart values: SCarpenter links core at the version
# of go.mod, so the controller image is the system under test and core's
# settings are controller flags (manifests/controller/deployment.yaml).
#
# Logs and test output land in test/kwok/.out/.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
cd "${root}"

KWOK_VERSION="v0.8.0"
cluster="${KIND_CLUSTER_NAME:-scarpenter-kwok}"
ns="scarpenter-system"

cache="${here}/.cache"
out="${here}/.out"
mkdir -p "${cache}" "${out}"
export KUBECONFIG="${cache}/${cluster}.kubeconfig"

manager_image="scarpenter-controller:kwok"
replay_image="scarpenter-keeper-replay:kwok"

# shellcheck source=test/lib.sh
source "${root}/test/lib.sh"

start="$(date +%s)"
phase_start="${start}"
phase() {
  local now
  now="$(date +%s)"
  printf '    (%ss)\n' "$((now - phase_start))"
  phase_start="${now}"
}

collect() {
  log "collecting logs into ${out}"
  mkdir -p "${out}/logs"
  kubectl get nodes -o wide >"${out}/logs/nodes.txt" 2>&1 || true
  kubectl get pods -A -o wide >"${out}/logs/pods.txt" 2>&1 || true
  kubectl get scnodeclasses,nodepools,nodeclaims -o yaml >"${out}/logs/karpenter-objects.yaml" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp >"${out}/logs/events.txt" 2>&1 || true
  kubectl -n "${ns}" logs deploy/scarpenter --tail=-1 >"${out}/logs/controller.log" 2>&1 || true
  kubectl -n "${ns}" logs deploy/scarpenter --previous --tail=-1 >"${out}/logs/controller-previous.log" 2>&1 || true
  kubectl -n kube-system logs deploy/kwok-controller --tail=-1 >"${out}/logs/kwok-controller.log" 2>&1 || true
  kubectl -n "${ns}" logs deploy/keeper-replay --tail=-1 >"${out}/logs/keeper-replay.log" 2>&1 || true
  kubectl -n "${ns}" get configmap scarpenter-fake scarpenter-fake-state -o yaml >"${out}/logs/fake-configmaps.yaml" 2>&1 || true
}

if [[ "${KWOK_SETUP_ONLY:-}" == "1" ]]; then
  export KEEP_CLUSTER=1
fi

status=0
cleanup() {
  status=$?
  if [[ "${status}" -ne 0 ]] && kubectl version >/dev/null 2>&1; then
    collect
  fi
  if [[ "${KEEP_CLUSTER:-}" != "1" ]]; then
    log "deleting kind cluster ${cluster} and the images"
    kind_down "${cluster}" "${cache}"
    docker rmi -f "${manager_image}" "${replay_image}" >/dev/null 2>&1 || true
    rm -f "${KUBECONFIG}"
  fi
  if [[ "${KWOK_SETUP_ONLY:-}" == "1" && "${status}" -eq 0 ]]; then
    printf '\ncluster %s is up after %ss\nkubeconfig: %s\n' "${cluster}" "$(($(date +%s) - start))" "${KUBECONFIG#"${root}/"}"
    return
  fi
  printf '\nL3: %s after %ss\n' "$([[ "${status}" -eq 0 ]] && echo PASS || echo FAIL)" "$(($(date +%s) - start))"
}
trap cleanup EXIT

gobuild() { # gobuild <output> <package> [tags]
  # The module proxy occasionally drops a download; retry before building.
  local attempt
  for attempt in 1 2 3; do
    go mod download && break
    [[ "${attempt}" -lt 3 ]] || return 1
    echo "go mod download failed (attempt ${attempt}), retrying" >&2
    sleep $((attempt * 10))
  done
  CGO_ENABLED=0 go build -trimpath -tags "${3:-}" -ldflags="-s -w" -o "$1" "$2"
}

download() { # download <url> <file>
  [[ -s "$2" ]] || curl -fsSL --retry 3 -o "$2" "$1"
}

# ---------------------------------------------------------------- images ----
log "building the controller and replay images"
ctx="${cache}/ctx"
rm -rf "${ctx}"
mkdir -p "${ctx}/manager" "${ctx}/replay"
gobuild "${ctx}/manager/manager" ./cmd/controller scarpenter_dev
docker build -q -t "${manager_image}" -f "${here}/Dockerfile.manager" "${ctx}/manager"
gobuild "${ctx}/replay/replay" ./test/kwok/replay
cp -r "${root}/test/data/sc-snapshot" "${ctx}/replay/snapshot"
cp -r "${root}/pkg/artifacts/testdata" "${ctx}/replay/sysext"
docker build -q -t "${replay_image}" -f "${here}/Dockerfile.replay" "${ctx}/replay"
phase

# --------------------------------------------------------------- cluster ----
if [[ "${REUSE_CLUSTER:-}" == "1" ]] && kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
  log "reusing kind cluster ${cluster}"
  kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
else
  log "creating kind cluster ${cluster}"
  kind_up "${cluster}" "${here}/kind.yaml" "${KUBECONFIG}" "${cache}"
fi
for img in "${manager_image}" "${replay_image}"; do
  kind load docker-image "${img}" --name "${cluster}"
done
# Workloads must not land on the real node, or Karpenter never provisions for
# them: taint it, and let the infrastructure tolerate the taint.
kubectl taint nodes --all node-role.kubernetes.io/control-plane:NoSchedule- >/dev/null 2>&1 || true
kubectl taint nodes --all CriticalAddonsOnly=true:NoSchedule --overwrite
phase

# ------------------------------------------------------------------ kwok ----
log "installing kwok-controller ${KWOK_VERSION} and the stage-fast stages"
for f in kwok.yaml stage-fast.yaml; do
  download "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/${f}" "${cache}/${KWOK_VERSION}-${f}"
done
kubectl apply --server-side -f "${cache}/${KWOK_VERSION}-kwok.yaml"
kubectl wait --for=condition=Established crd/stages.kwok.x-k8s.io --timeout=60s
kubectl apply -f "${cache}/${KWOK_VERSION}-stage-fast.yaml"
# The kwok-controller must run on the real node and manage fake ones only.
kubectl -n kube-system patch deployment kwok-controller --type strategic -p '{"spec":{"template":{"spec":{
  "tolerations":[{"key":"CriticalAddonsOnly","operator":"Exists"}],
  "affinity":{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[
    {"matchExpressions":[{"key":"kwok.x-k8s.io/node","operator":"DoesNotExist"}]}]}}}}}}}'
kubectl -n kube-system rollout status deployment/kwok-controller --timeout=180s
phase

# ------------------------------------------------------------ controller ----
log "installing the CRDs, the controller and the Keeper replay server"
core_dir="$(go list -m -f '{{.Dir}}' sigs.k8s.io/karpenter)"
kubectl apply --server-side -f "${core_dir}/pkg/apis/crds/"
kubectl apply --server-side -f "${root}/config/crd/bases/"
kubectl wait --for=condition=Established crd/nodepools.karpenter.sh crd/nodeclaims.karpenter.sh \
  crd/scnodeclasses.scarpenter.sparecores.com --timeout=60s
# SCarpenter's own RBAC is the production one (config/rbac); the lane's file adds
# core's permissions and the fake driver's.
# The NodeClass of the described-not-named scenario exists before the controller
# starts, so that its metric labels are registered with core.
kubectl apply -f "${here}/manifests/nodeclass-described.yaml"
kubectl apply -f "${here}/manifests/controller/namespace.yaml"
kubectl apply --server-side -k "${root}/config/rbac"
kubectl kustomize "${here}/manifests/controller" | sed "s|controller:latest|${manager_image}|" | kubectl apply --server-side -f -
kubectl apply -f "${here}/manifests/keeper-replay.yaml"
kubectl -n "${ns}" rollout status deployment/keeper-replay --timeout=120s
kubectl -n "${ns}" rollout status deployment/scarpenter --timeout=180s
phase

if [[ "${KWOK_SETUP_ONLY:-}" == "1" ]]; then
  exit 0
fi

# ----------------------------------------------------------------- tests ----
log "running the scenarios"
run_args=()
[[ -z "${KWOK_SCENARIOS:-}" ]] || run_args=(-run "${KWOK_SCENARIOS}")
KWOK_OUT_DIR="${out}" \
  go test -tags kwok,scarpenter_dev -count=1 -timeout 60m -v "${run_args[@]}" ./test/kwok/e2e/ 2>&1 | tee "${out}/e2e.log"
phase
