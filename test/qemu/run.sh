#!/usr/bin/env bash
# L4: SCarpenter with the qemu driver boots real Flatcar VMs that join a k3s
# control plane through the whole Karpenter path (docs/plans/v1/wip/
# 00-overview.md section 13.3). Everything runs in the privileged dev
# container, in its private network namespace:
#
#   hack/dev.sh --privileged make test-qemu
#
#   QEMU_SCENARIOS    a go test -run regexp selecting scenarios (default: all)
#   QEMU_SETUP_ONLY=1 stop once the control plane and the network run, and wait
#                     (to experiment: docker exec into the container)
#   SCARPENTER_QEMU_TAILSCALE_AUTHKEY, SCARPENTER_QEMU_TAILSCALE_LOGIN_SERVER
#                     optional: a pre-authorized, ephemeral key of a tailnet or
#                     Headscale of your own for scenario Q11, which skips without it
#                     (Q09 uses the Headscale this script starts)
#
# In the container's network namespace the lane creates a bridge with a DHCP and
# DNS server (dnsmasq) and NAT, a k3s server (server only, configured like a
# production k3s for remote workers), flannel and kube-proxy as DaemonSets, and a
# local HTTP mirror of the sysext releases (downloaded once into .cache) and a
# Headscale with the container itself on its tailnet. The
# scenarios in e2e/ start SCarpenter themselves (they restart it).
#
# Downloads (Flatcar releases, sysexts) are cached in test/qemu/.cache; logs,
# serial consoles of the VMs and test output land in test/qemu/.out/.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
cd "${root}"

# shellcheck source=test/lib.sh
source "${root}/test/lib.sh"

if [[ -z "${SCARPENTER_DEV_PRIVILEGED:-}" || -z "${SCARPENTER_DEV_CONTAINER:-}" ]]; then
  echo "run the L4 lane through hack/dev.sh --privileged make test-qemu" >&2
  exit 1
fi
# The lane creates a bridge, NAT rules and sets the FORWARD policy: only ever in the
# container's own network namespace, never in the host's (which has docker0).
if [[ -e /sys/class/net/docker0 ]]; then
  echo "refusing to run in the host's network namespace" >&2
  exit 1
fi
if [[ ! -r /dev/kvm || ! -w /dev/kvm ]]; then
  echo "warning: /dev/kvm is not usable: the VMs run under TCG emulation, which is very slow" >&2
fi

KUBERNETES_VERSION="v1.37.1"
bridge="scbr0"
host_ip="10.77.0.1"
cidr="10.77.0.0/24"
api="https://${host_ip}:6443"
mirror_port=8099
sysext_base="http://${host_ip}:${mirror_port}"

cache="${here}/.cache"
out="${here}/.out"
run="${cache}/run"
mkdir -p "${cache}" "${out}"
rm -rf "${out:?}"/* "${run}"
mkdir -p "${run}" "${out}/logs" "${out}/serial"

start="$(date +%s)"
pids=()
status=0

collect() {
  log "collecting logs into ${out}"
  export KUBECONFIG="${run}/admin.kubeconfig"
  if [[ -s "${KUBECONFIG}" ]]; then
    kubectl get nodes -o wide >"${out}/logs/nodes.txt" 2>&1 || true
    kubectl get pods -A -o wide >"${out}/logs/pods.txt" 2>&1 || true
    kubectl get scnodeclasses,nodepools,nodeclaims -o yaml >"${out}/logs/karpenter-objects.yaml" 2>&1 || true
    kubectl get events -A --sort-by=.lastTimestamp >"${out}/logs/events.txt" 2>&1 || true
    kubectl get csr >"${out}/logs/csr.txt" 2>&1 || true
  fi
  # Serial consoles of the VMs that are still running (deleted ones were moved
  # to out/serial by the driver).
  for f in "${run}"/state/instances/*/serial.log; do
    [[ -e "${f}" ]] || continue
    cp "${f}" "${out}/serial/running-$(basename "$(dirname "${f}")").log"
  done
  cp "${run}"/k3s.log "${out}/logs/k3s.log" 2>/dev/null || true
  cp "${run}"/dnsmasq.log "${out}/logs/dnsmasq.log" 2>/dev/null || true
  cp "${run}"/mirror.log "${out}/logs/mirror.log" 2>/dev/null || true
  cp "${run}"/headscale.log "${out}/logs/headscale.log" 2>/dev/null || true
  cp "${run}"/tailscaled.log "${out}/logs/tailscaled.log" 2>/dev/null || true
}

cleanup() {
  status=$?
  if [[ "${status}" -ne 0 ]]; then
    collect
  fi
  log "stopping the control plane and the VMs"
  # The VMs are daemonised and not our children: end them by their command line.
  pkill -f 'qemu-system-.* -name i-' 2>/dev/null || true
  for pid in "${pids[@]}"; do
    kill "${pid}" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  printf '\nL4: %s after %ss\n' "$([[ "${status}" -eq 0 ]] && echo PASS || echo FAIL)" "$(($(date +%s) - start))"
}
trap cleanup EXIT

download() { # download <url> <file>
  if [[ ! -s "$2" ]]; then
    curl -fsSL --retry 3 -o "$2.part" "$1"
    mv "$2.part" "$2"
  fi
}

wait_for() { # wait_for <what> <timeout seconds> <command...>
  local what="$1" timeout="$2" deadline
  shift 2
  deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    if [[ "${SECONDS}" -ge "${deadline}" ]]; then
      echo "timed out after ${timeout}s waiting for ${what}" >&2
      return 1
    fi
    sleep 1
  done
}

# ---------------------------------------------------------------- binary ----
log "building the controller (scarpenter_dev: fake and qemu drivers)"
go mod download
mkdir -p "${cache}/bin"
CGO_ENABLED=0 go build -trimpath -tags scarpenter_dev -ldflags="-s -w" -o "${cache}/bin/scarpenter" ./cmd/controller

# --------------------------------------------------------------- mirror -----
# The VMs fetch the sysexts from here, like from a mirror configured with
# --sysext-base-url, and the controller reads the release checksums from it.
log "preparing the sysext mirror"
sysext="kubernetes-${KUBERNETES_VERSION}"
mkdir -p "${cache}/mirror/${sysext}"
upstream="https://github.com/flatcar/sysext-bakery/releases/download/${sysext}"
download "${upstream}/SHA256SUMS" "${cache}/mirror/${sysext}/SHA256SUMS"
file="${sysext}-x86-64.raw"
download "${upstream}/${file}" "${cache}/mirror/${sysext}/${file}"
(cd "${cache}/mirror/${sysext}" && grep " ${file}\$" SHA256SUMS | sha256sum -c -)

# The tailscale template needs its sysext (scenario Q10 boots it without a tailnet,
# Q09 with one).
ts="tailscale-v1.98.10"
mkdir -p "${cache}/mirror/${ts}"
upstream="https://github.com/flatcar/sysext-bakery/releases/download/${ts}"
download "${upstream}/SHA256SUMS" "${cache}/mirror/${ts}/SHA256SUMS"
download "${upstream}/${ts}-x86-64.raw" "${cache}/mirror/${ts}/${ts}-x86-64.raw"
(cd "${cache}/mirror/${ts}" && grep " ${ts}-x86-64.raw\$" SHA256SUMS | sha256sum -c -)

# --------------------------------------------------------------- network ----
log "creating the bridge ${bridge} (${host_ip}) with DHCP, DNS and NAT"
ip link add "${bridge}" type bridge
ip addr add "${host_ip}/24" dev "${bridge}"
ip link set "${bridge}" up
iptables -t nat -A POSTROUTING -s "${cidr}" ! -o "${bridge}" -j MASQUERADE
iptables -P FORWARD ACCEPT 2>/dev/null || true
# The upstream resolver is the container's; --no-resolv and --no-poll keep dnsmasq
# from watching resolv.conf with inotify, whose instances the host may have used up.
upstream_dns="$(awk '/^nameserver/ {print $2; exit}' /etc/resolv.conf)"
dnsmasq --keep-in-foreground --interface="${bridge}" --bind-interfaces --except-interface=lo \
  --no-resolv --no-poll --server="${upstream_dns:-1.1.1.1}" \
  --listen-address="${host_ip}" --dhcp-range=10.77.0.100,10.77.0.200,12h \
  --dhcp-option=3,"${host_ip}" --dhcp-option=6,"${host_ip}" \
  --dhcp-leasefile="${run}/dnsmasq.leases" --pid-file="${run}/dnsmasq.pid" \
  --no-hosts --log-dhcp >"${run}/dnsmasq.log" 2>&1 &
pids+=($!)

mirror_dir="${cache}/mirror"
python3 -m http.server "${mirror_port}" --bind "${host_ip}" --directory "${mirror_dir}" >"${run}/mirror.log" 2>&1 &
pids+=($!)
wait_for "the sysext mirror" 30 curl -fs "${sysext_base}/${sysext}/SHA256SUMS"

# ------------------------------------------------------------- headscale ----
# A Headscale in the lane, so that the tailscale template is proven end to end on
# every run: the VMs join a real tailnet, and the control plane (this container,
# through tailscaled) reaches the kubelets over it. No external network, no secrets:
# the keys are minted here and live in the git-ignored run directory.
hs_version="0.29.7"
ts_version="1.98.10"
tools="${cache}/tools"
mkdir -p "${tools}"
hs_bin="${tools}/headscale-${hs_version}"
if [[ ! -x "${hs_bin}" ]]; then
  hs_url="https://github.com/juanfont/headscale/releases/download/v${hs_version}"
  curl -fsSL --retry 3 -o "${hs_bin}.part" "${hs_url}/headscale_${hs_version}_linux_amd64"
  curl -fsSL --retry 3 "${hs_url}/checksums.txt" | grep " headscale_${hs_version}_linux_amd64\$" |
    awk -v f="${hs_bin}.part" '{print $1 "  " f}' | sha256sum -c -
  chmod +x "${hs_bin}.part"
  mv "${hs_bin}.part" "${hs_bin}"
fi
ts_dir="${tools}/tailscale_${ts_version}_amd64"
if [[ ! -x "${ts_dir}/tailscaled" ]]; then
  ts_url="https://pkgs.tailscale.com/stable/tailscale_${ts_version}_amd64.tgz"
  curl -fsSL --retry 3 -o "${tools}/ts.tgz" "${ts_url}"
  want="$(curl -fsSL --retry 3 "${ts_url}.sha256")"
  echo "${want}  ${tools}/ts.tgz" | sha256sum -c -
  tar -xzf "${tools}/ts.tgz" -C "${tools}"
  rm -f "${tools}/ts.tgz"
fi
hs_port=8085
hs_url_lane="http://${host_ip}:${hs_port}"
hs="${run}/headscale"
mkdir -p "${hs}"
cat >"${hs}/config.yaml" <<YAML
server_url: ${hs_url_lane}
listen_addr: ${host_ip}:${hs_port}
metrics_listen_addr: 127.0.0.1:9095
grpc_listen_addr: 127.0.0.1:50443
grpc_allow_insecure: true
unix_socket: ${hs}/headscale.sock
noise:
  private_key_path: ${hs}/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
database:
  type: sqlite
  sqlite:
    path: ${hs}/db.sqlite
derp:
  server:
    enabled: true
    region_id: 999
    region_code: lane
    region_name: lane
    private_key_path: ${hs}/derp_private.key
    stun_listen_addr: ${host_ip}:3478
    verify_clients: false
    ipv4: ${host_ip}
  urls: []
  paths: []
  auto_update_enabled: false
dns:
  magic_dns: false
  base_domain: lane.example.com
  override_local_dns: false
  nameservers:
    global: []
log:
  level: info
YAML
"${hs_bin}" -c "${hs}/config.yaml" serve >"${run}/headscale.log" 2>&1 &
pids+=($!)
wait_for "headscale" 60 curl -fs "${hs_url_lane}/health"
"${hs_bin}" -c "${hs}/config.yaml" users create lane >/dev/null
hs_user="$("${hs_bin}" -c "${hs}/config.yaml" users list -o json | jq -r '.[] | select(.name=="lane") | .id')"
mintkey() { # mintkey <file>: a reusable, ephemeral, pre-authorized key
  "${hs_bin}" -c "${hs}/config.yaml" preauthkeys create --user "${hs_user}" --reusable --ephemeral --expiration 6h -o json |
    jq -r '.key' >"$1"
  chmod 600 "$1"
}
mintkey "${hs}/node-authkey"
mintkey "${hs}/host-authkey"
# This container joins the tailnet, so that the API server reaches the kubelets at
# their tailscale addresses (the kubelet's --node-ip with the tailscale template).
"${ts_dir}/tailscaled" --tun=tailscale0 --state="${hs}/tailscaled.state" --socket="${hs}/tailscaled.sock" \
  >"${run}/tailscaled.log" 2>&1 &
pids+=($!)
wait_for "tailscaled" 30 test -S "${hs}/tailscaled.sock"
"${ts_dir}/tailscale" --socket="${hs}/tailscaled.sock" up --login-server="${hs_url_lane}" \
  --auth-key="file:${hs}/host-authkey" --hostname=lane-host --accept-dns=false --netfilter-mode=off >/dev/null
wait_for "the tailnet address" 30 "${ts_dir}/tailscale" --socket="${hs}/tailscaled.sock" ip -4

# ------------------------------------------------------------------- k3s ----
# Server only (no agent: the nodes are the VMs), configured like a production
# k3s for remote workers: no embedded flannel and kube-proxy, no network policy
# controller, no egress selector (the API server reaches the kubelets directly).
# k3s starts bootstrap-token authentication and the tokencleaner controller by
# default (overview section 15, Q4); nothing is passed for them.
log "starting k3s"
data="${run}/k3s"
mkdir -p "${data}"
k3s server --disable-agent --data-dir "${data}" --write-kubeconfig "${run}/admin.kubeconfig" \
  --node-ip "${host_ip}" --advertise-address "${host_ip}" --bind-address "${host_ip}" --tls-san "${host_ip}" \
  --flannel-backend=none --disable-kube-proxy --disable-network-policy --egress-selector-mode=disabled \
  --disable=traefik,servicelb,metrics-server,local-storage,coredns --disable-cloud-controller \
  >"${run}/k3s.log" 2>&1 &
pids+=($!)
wait_for "the k3s kubeconfig" 120 test -s "${run}/admin.kubeconfig"
sed -i "s#https://127.0.0.1:6443#${api}#" "${run}/admin.kubeconfig"
export KUBECONFIG="${run}/admin.kubeconfig"
wait_for "the API server" 120 kubectl get --raw /readyz
wait_for "the default ServiceAccount" 60 kubectl -n default get serviceaccount default
tls="${data}/server/tls"

# ------------------------------------------------------------- cluster ------
log "installing the CRDs, the RBAC, flannel and kube-proxy"
core_dir="$(go list -m -f '{{.Dir}}' sigs.k8s.io/karpenter)"
kubectl apply --server-side -f "${core_dir}/pkg/apis/crds/"
kubectl apply --server-side -f "${root}/config/crd/bases/"
kubectl wait --for=condition=Established crd/nodepools.karpenter.sh crd/nodeclaims.karpenter.sh \
  crd/scnodeclasses.scarpenter.sparecores.com --timeout=60s
kubectl apply -f "${here}/manifests/namespace.yaml"
# The production RBAC (SCarpenter's permissions, the namespaced Roles, the
# node-join bindings) and, on top of it, core's permissions.
kubectl apply --server-side -k "${root}/config/rbac"
kubectl apply -f "${here}/manifests/rbac.yaml"
for f in flannel kube-proxy; do
  sed "s#@API_HOST@#${host_ip}#g" "${here}/manifests/${f}.yaml" | kubectl apply -f -
done

# SCarpenter authenticates as its ServiceAccount, not as the cluster admin, so
# that a permission missing from config/rbac fails the lane.
token="$(kubectl -n scarpenter-system create token scarpenter --duration=6h)"
ctl="${run}/controller.kubeconfig"
kubectl config --kubeconfig="${ctl}" set-cluster lane --server="${api}" --certificate-authority="${tls}/server-ca.crt" --embed-certs=true >/dev/null
kubectl config --kubeconfig="${ctl}" set-credentials scarpenter --token="${token}" >/dev/null
kubectl config --kubeconfig="${ctl}" set-context lane --cluster=lane --user=scarpenter >/dev/null
kubectl config --kubeconfig="${ctl}" use-context lane >/dev/null

# The key that gets into the VMs (the lane's NodeClass adds it through a Butane
# ConfigMap), for the scenarios that look inside.
[[ -f "${run}/vm-key" ]] || ssh-keygen -q -t ed25519 -N '' -C scarpenter-lane -f "${run}/vm-key"

export QEMU_LANE_ADMIN_KUBECONFIG="${run}/admin.kubeconfig"
export QEMU_LANE_CONTROLLER_KUBECONFIG="${ctl}"
export QEMU_LANE_BINARY="${cache}/bin/scarpenter"
export QEMU_LANE_API="${api}"
export QEMU_LANE_BRIDGE="${bridge}"
export QEMU_LANE_SYSEXT_BASE="${sysext_base}"
export QEMU_LANE_KUBERNETES_VERSION="${KUBERNETES_VERSION}"
export QEMU_LANE_KUBELET_CLIENT_CA="${tls}/client-ca.crt"
export QEMU_LANE_STATE_DIR="${run}/state"
export QEMU_LANE_CACHE_DIR="${cache}/images"
export QEMU_LANE_SERIAL_DIR="${out}/serial"
export QEMU_LANE_SSH_KEY="${run}/vm-key"
export QEMU_LANE_OUT="${out}"
export QEMU_LANE_HEADSCALE_URL="${hs_url_lane}"
export QEMU_LANE_HEADSCALE_KEY_FILE="${hs}/node-authkey"
mkdir -p "${QEMU_LANE_STATE_DIR}" "${QEMU_LANE_CACHE_DIR}"

if [[ "${QEMU_SETUP_ONLY:-}" == "1" ]]; then
  printf '\nthe control plane is up after %ss (api %s, kubeconfig %s); stop with ctrl-c\n' "$(($(date +%s) - start))" "${api}" "${run}/admin.kubeconfig"
  sleep infinity
fi

# ----------------------------------------------------------------- tests ----
log "running the scenarios"
run_args=()
[[ -z "${QEMU_SCENARIOS:-}" ]] || run_args=(-run "${QEMU_SCENARIOS}")
go test -tags qemu,scarpenter_dev -count=1 -timeout 90m -v "${run_args[@]}" ./test/qemu/e2e/ 2>&1 | tee "${out}/e2e.log"
