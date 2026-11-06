#!/usr/bin/env bash
# Helpers shared by the scripts of the local test lanes (test/qemu/run.sh,
# test/kwok/run.sh). Source it; it only defines functions.

log() { printf '\n==> %s\n' "$*"; }

# kind_up <cluster> <base kind config> <kubeconfig> <scratch dir>
#
# Creates the cluster. kubelet cannot start in a kind node whose /var lives on
# a ZFS-backed docker root ("cannot find filesystem info for device"); on such
# hosts the node's containerd and kubelet directories are bind-mounted from
# tmpfs (/dev/shm) through a generated copy of the config.
kind_up() {
  local cluster="$1" base="$2" kubeconfig="$3" scratch="$4"
  local config="${scratch}/kind-${cluster}.yaml"
  mkdir -p "${scratch}"
  cp "${base}" "${config}"
  if docker info --format '{{range .DriverStatus}}{{index . 0}}={{index . 1}}{{"\n"}}{{end}}' 2>/dev/null |
    grep -qi '^Backing Filesystem=zfs'; then
    cat >>"${config}" <<YAML
    extraMounts:
      - {hostPath: /dev/shm/${cluster}/containerd, containerPath: /var/lib/containerd}
      - {hostPath: /dev/shm/${cluster}/kubelet, containerPath: /var/lib/kubelet}
YAML
  fi
  kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true
  kind create cluster --name "${cluster}" --config "${config}" --kubeconfig "${kubeconfig}" --wait 180s
}

# kind_down <cluster> <scratch dir>: deletes the cluster and what kind_up left
# behind on the host.
kind_down() {
  local cluster="$1" scratch="$2" node_image
  node_image="$(docker inspect "${cluster}-control-plane" --format '{{.Config.Image}}' 2>/dev/null || true)"
  kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true
  # The tmpfs directories were created by docker, as root.
  if [[ -n "${node_image}" ]] && grep -q '/dev/shm' "${scratch}/kind-${cluster}.yaml" 2>/dev/null; then
    docker run --rm -v /dev/shm:/shm --entrypoint rm "${node_image}" -rf "/shm/${cluster}" >/dev/null 2>&1 || true
  fi
}
