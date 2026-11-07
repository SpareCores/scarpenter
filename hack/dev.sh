#!/usr/bin/env bash
# Run a command inside the dev container (hack/devcontainer/Dockerfile).
#
#   hack/dev.sh make test
#   hack/dev.sh go test ./pkg/catalog/...
#   hack/dev.sh --privileged make test-qemu     (L4 lane, see below)
#
# The image is built on first use and rebuilt when the Dockerfile changes. The
# repository is mounted at /src; Go module and build caches live in a named
# volume. The Docker socket is mounted so the container can run kind and
# `docker build`.
#
# --privileged is for the L4 lane (qemu VMs): it adds /dev/kvm (when the host
# has it), /dev/net/tun, the NET_ADMIN and NET_RAW capabilities (ambient, for
# the caller's uid; `make dev-check-privileged` proves it), and runs in a private
# network namespace (Docker's default bridge) instead of the host's, so the
# lane's bridges, taps, dnsmasq and iptables rules never touch the host network.
# Without it the container uses the host network (kind clusters, local ports).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="${repo_root}/hack/devcontainer/Dockerfile"

image_repo="${SCARPENTER_DEV_IMAGE_REPO:-scarpenter-dev}"
cache_volume="${SCARPENTER_DEV_CACHE_VOLUME:-scarpenter-dev-cache}"
tag="$(sha256sum "${dockerfile}" | cut -c1-12)"
image="${image_repo}:${tag}"

want_build_only=0
privileged=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --build) want_build_only=1; shift ;;   # used by `make dev-image`: only make sure the image exists
    --privileged) privileged=1; shift ;;
    *) break ;;
  esac
done

if ! docker image inspect "${image}" >/dev/null 2>&1; then
  docker build -t "${image}" -f "${dockerfile}" "${repo_root}/hack/devcontainer"
fi

if [[ "${want_build_only}" == 1 ]]; then
  exit 0
fi

if [[ $# -eq 0 ]]; then
  set -- bash
fi

tty_flags=(-i)
if [[ -t 0 && -t 1 ]]; then
  tty_flags=(-it)
fi

sock_args=()
if [[ -S /var/run/docker.sock ]]; then
  sock_args=(-v /var/run/docker.sock:/var/run/docker.sock --group-add "$(stat -c %g /var/run/docker.sock)")
fi

# Opt-in aid for developing sc-keeper-go together with this repository: with a
# temporary `replace github.com/SpareCores/sc-keeper-go => ../sc-keeper-go` in
# go.mod, the sibling checkout must be visible in the container at the same
# relative path (the repository is /src, so ../sc-keeper-go is /sc-keeper-go).
# It is mounted read-only when the directory exists; without a replace
# directive go.mod uses the tagged module and the mount is unused.
sibling_args=()
if [[ -d "${repo_root}/../sc-keeper-go" ]]; then
  sibling_args=(-v "$(cd "${repo_root}/../sc-keeper-go" && pwd):/sc-keeper-go:ro")
fi

net_args=(--network host)
user_args=(-u "$(id -u):$(id -g)")
priv_args=()
cmd_prefix=()
if [[ "${privileged}" == 1 ]]; then
  net_args=()
  # Docker grants added capabilities only to uid 0, so the container starts as
  # root and the command drops to the caller's uid/gid with the network
  # capabilities kept as ambient capabilities.
  user_args=(-u 0:0)
  priv_args=(--cap-add NET_ADMIN --cap-add NET_RAW --cap-add SETUID --cap-add SETGID
    -e SCARPENTER_DEV_PRIVILEGED=1)
  groups="$(id -G | tr ' ' ',')"
  if [[ -e /dev/kvm ]]; then
    priv_args+=(--device /dev/kvm)
    groups+=",$(stat -c %g /dev/kvm)"
  fi
  if [[ -e /dev/net/tun ]]; then
    priv_args+=(--device /dev/net/tun)
  fi
  caps=+net_admin,+net_raw,+net_bind_service
  # Two things the L4 lane needs that the image lacks for an arbitrary uid: k3s
  # keeps its node password below /etc/rancher and refuses to start when the
  # directory cannot be created, and ssh refuses to run for a uid without a
  # passwd entry. Both are made while still root and die with the container.
  # shellcheck disable=SC2016 # the script is for the container's shell
  cmd_prefix=(sh -c 'install -d -m 1777 /etc/rancher
    id -nu "$0" >/dev/null 2>&1 || echo "dev:x:$0:$1::/cache/home:/bin/bash" >>/etc/passwd
    shift; exec "$@"' "$(id -u)" "$(id -g)"
    setpriv "--reuid=$(id -u)" "--regid=$(id -g)" "--groups=${groups}"
    "--inh-caps=${caps}" "--ambient-caps=${caps}" --)
fi

# Settings of the local test lanes (test/kwok/run.sh, test/qemu/run.sh) that
# pass through when set in the caller's environment.
env_args=()
for name in KWOK_SCENARIOS KWOK_SETUP_ONLY KEEP_CLUSTER REUSE_CLUSTER KIND_CLUSTER_NAME \
  QEMU_SCENARIOS QEMU_SETUP_ONLY SCARPENTER_QEMU_TAILSCALE_AUTHKEY SCARPENTER_QEMU_TAILSCALE_LOGIN_SERVER; do
  if [[ -n "${!name:-}" ]]; then
    # -e NAME copies the value from this environment: it does not show up in the
    # process list or the container's command line (auth keys pass through here).
    env_args+=(-e "${name}")
  fi
done

exec docker run --rm "${tty_flags[@]}" \
  "${env_args[@]}" \
  "${priv_args[@]}" \
  "${user_args[@]}" \
  "${net_args[@]}" \
  "${sock_args[@]}" \
  -v "${repo_root}:/src" \
  "${sibling_args[@]}" \
  -v "${cache_volume}:/cache" \
  -w /src \
  -e HOME=/cache/home \
  -e GOPATH=/cache/go \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/go-mod \
  -e GOLANGCI_LINT_CACHE=/cache/golangci-lint \
  -e GOFLAGS=-buildvcs=false \
  -e GOTOOLCHAIN=local \
  -e SCARPENTER_DEV_CONTAINER=1 \
  "${image}" "${cmd_prefix[@]}" "$@"
