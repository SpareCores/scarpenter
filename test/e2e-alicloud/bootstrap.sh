#!/usr/bin/env bash
# L5 step 1: a k3s control plane on the CI runner that nodes reach over the tailnet
# (overview section 13.4): the server bound to the runner's tailscale address (TLS
# SAN, node IP, advertise address), configured like a production k3s for remote
# workers (no embedded flannel, kube-proxy or network policy controller, no egress
# selector), flannel over tailscale0 and kube-proxy as DaemonSets. The runner has
# joined the tailnet already (tailscale/github-action, tag tag:scarpenter-ci).
#
# The runner's own node is the server's agent: the controller Pod needs one. It is
# tainted CriticalAddonsOnly, so that test workloads only land on SCarpenter's nodes.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-alicloud/lib.sh
source "${here}/lib.sh"
need E2E_OUT KUBECONFIG
mkdir -p "${E2E_OUT}"

tsip="$(tailscale ip -4 | head -n1)"
[[ -n "${tsip}" ]] || {
  echo "the runner has no tailscale address" >&2
  exit 1
}
echo "the control plane's tailnet address is ${tsip}" # an address of the tailnet, not a secret
if [[ -n "${GITHUB_ENV:-}" ]]; then
  echo "E2E_API_HOST=${tsip}" >>"${GITHUB_ENV}"
fi
echo "${tsip}" >"${E2E_OUT}/api-host"

log "installing k3s ${E2E_K3S_VERSION}"
url="https://github.com/k3s-io/k3s/releases/download/${E2E_K3S_VERSION//+/%2B}"
tmp="$(mktemp -d)"
curl -fsSL --retry 3 -o "${tmp}/k3s" "${url}/k3s"
curl -fsSL --retry 3 -o "${tmp}/sums" "${url}/sha256sum-amd64.txt"
(cd "${tmp}" && grep -E ' k3s$' sums | sha256sum -c -)
sudo install -m 0755 "${tmp}/k3s" /usr/local/bin/k3s
rm -rf "${tmp}"

log "starting the k3s server"
# CriticalAddonsOnly: k3s's CoreDNS tolerates it, flannel and kube-proxy tolerate
# everything, the chart gets the toleration from manifests/chart-values.yaml.
sudo systemd-run --unit=k3s-e2e --collect \
  /usr/local/bin/k3s server \
  --node-ip "${tsip}" --advertise-address "${tsip}" --tls-san "${tsip}" \
  --node-taint CriticalAddonsOnly=true:NoSchedule \
  --flannel-backend=none --disable-kube-proxy --disable-network-policy --egress-selector-mode=disabled \
  --disable=traefik,servicelb,metrics-server,local-storage \
  --write-kubeconfig /etc/rancher/k3s/k3s.yaml --write-kubeconfig-mode 0644
wait_until "the k3s kubeconfig" 180 test -s /etc/rancher/k3s/k3s.yaml
# Without its embedded flannel, k3s's containerd uses the default CNI paths
# (/etc/cni/net.d, /opt/cni/bin). The flannel DaemonSet installs only its own plugin
# there; the ones it delegates to (bridge, host-local, portmap, loopback) are k3s's
# bundled plugins, which a runner does not have in /opt/cni/bin.
k3s_bin=/var/lib/rancher/k3s/data/current/bin
sudo mkdir -p /opt/cni/bin
for p in bridge host-local loopback portmap bandwidth; do
  sudo test -e "/opt/cni/bin/${p}" || sudo ln -s "${k3s_bin}/${p}" "/opt/cni/bin/${p}"
done
# shellcheck disable=SC2153  # KUBECONFIG is the variable the workflow sets
install -m 0600 /etc/rancher/k3s/k3s.yaml "${KUBECONFIG}"
sed -i "s#https://127.0.0.1:6443#https://${tsip}:6443#" "${KUBECONFIG}"
wait_until "the API server" 180 kubectl get --raw /readyz
wait_until "the default ServiceAccount" 60 kubectl -n default get serviceaccount default

log "installing flannel (--iface=tailscale0) and kube-proxy"
kube_proxy_version="${E2E_K3S_VERSION%%+*}"
sed "s#@API_HOST@#${tsip}#g" "${here}/manifests/flannel.yaml" | kubectl apply -f -
sed -e "s#@API_HOST@#${tsip}#g" -e "s#@KUBE_PROXY_VERSION@#${kube_proxy_version}#g" \
  "${here}/manifests/kube-proxy.yaml" | kubectl apply -f -

wait_until "the runner's node Ready" 300 kubectl wait --for=condition=Ready node --all --timeout=5s
wait_until "CoreDNS" 300 kubectl -n kube-system rollout status deploy/coredns --timeout=5s
kubectl get nodes -o wide
