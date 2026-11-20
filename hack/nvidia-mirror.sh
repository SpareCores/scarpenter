#!/usr/bin/env bash
# Builds the mirror tree of the NVIDIA driver sysexts of a Flatcar release (plan 12, D5) and
# prints the pkg/nvidia table entries of the release. Needs network, docker and curl; it is
# not part of any gate.
#
#   hack/nvidia-mirror.sh <flatcar version> <output dir> [sysext name...]
#   hack/nvidia-mirror.sh 4757.2.3 ./mirror nvidia-drivers-570 nvidia-drivers-570-open
#
# Without names it takes every branch Flatcar builds: nvidia-drivers-{535,550,570,580}[-open]
# (a branch the release does not build is skipped). For each sysext it
#   1. downloads the payload (<server>/amd64-usr/<version>/flatcar-<name>.gz) and checks it
#      against the .gz.sha256 the server publishes;
#   2. extracts and verifies it with Flatcar's download_sysext (ue-rs, built in a container from
#      a pinned commit, against the release's update key, which is pinned by its sha256);
#   3. writes <output dir>/amd64-usr/<version>/flatcar-<name>.raw.gz, the layout of
#      --nvidia-driver-base-url (host this tree), and the SHA256SUMS of the files next to them.
# It prints, per sysext, the two pins of the table: the sha256 of the payload and of the extracted
# .raw (the hash Ignition verifies, over the decompressed bytes). The driver versions of the
# table's entries are not read from the images: take them from the release's ebuilds.
set -euo pipefail

SERVER="${FLATCAR_RELEASE_SERVER:-https://update.release.flatcar-linux.net}"
RUST_IMAGE="rust:1.95-bookworm@sha256:6258907abe69656e41cd992e0b705cdcfabcbbe3db374f92ed2d47121282d4a1"
UE_RS_REV="94d40218301f87c09bda8f186afa7a8249846041"
KEY_URL="https://raw.githubusercontent.com/flatcar/scripts/stable-4757.2.3/sdk_container/src/third_party/coreos-overlay/coreos-base/coreos-au-key/files/official-v2.pub.pem"
KEY_SHA256="b6a8227f835a4a56988241eaeeda57a19ef5ff413c8533fd791827be2c15dd6c"

if [[ $# -lt 2 ]]; then
  sed -n '2,/^set -euo/p' "$0" | sed '$d;s/^# \{0,1\}//' >&2
  exit 2
fi
version="$1"
out="$(mkdir -p "$2" && cd "$2" && pwd)"
shift 2
names=("$@")
if [[ ${#names[@]} -eq 0 ]]; then
  for branch in 535 550 570 580; do
    names+=("nvidia-drivers-${branch}" "nvidia-drivers-${branch}-open")
  done
fi
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "invalid Flatcar version ${version}" >&2
  exit 2
fi

cache="${out}/.cache"
tree="${out}/amd64-usr/${version}"
mkdir -p "${cache}" "${tree}"

if [[ ! -x "${cache}/download_sysext" ]]; then
  echo "building download_sysext (ue-rs ${UE_RS_REV})" >&2
  docker run --rm -v "${cache}:/out" "${RUST_IMAGE}" bash -ec "
    apt-get update -qq && apt-get install -y -qq protobuf-compiler >/dev/null
    cargo install --locked --git https://github.com/flatcar/ue-rs --rev ${UE_RS_REV} --root /tmp/ue --bin download_sysext ue-rs
    cp /tmp/ue/bin/download_sysext /out/
    chown $(id -u):$(id -g) /out/download_sysext"
fi
if [[ ! -f "${cache}/update-key.pub.pem" ]]; then
  curl -fsSL -o "${cache}/update-key.pub.pem" "${KEY_URL}"
fi
echo "${KEY_SHA256}  ${cache}/update-key.pub.pem" | sha256sum -c --quiet -

sums=()
entries=()
for name in "${names[@]}"; do
  payload_url="${SERVER}/amd64-usr/${version}/flatcar-${name}.gz"
  if ! expected="$(curl -fsL "${payload_url}.sha256" | awk '{print $1}')" || [[ -z "${expected}" ]]; then
    echo "skipping ${name}: the release does not build it" >&2
    continue
  fi
  work="${cache}/work-${name}"
  rm -rf "${work}"
  mkdir -p "${work}"
  echo "extracting ${name}" >&2
  docker run --rm --user "$(id -u):$(id -g)" \
    -v "${cache}:/cache:ro" -v "${work}:/out" "${RUST_IMAGE}" \
    /cache/download_sysext -o /out -n "flatcar-${name}.raw" -u "${payload_url}" -p /cache/update-key.pub.pem >/dev/null
  # download_sysext keeps the payload it verified the signature of next to the image.
  payload="${work}/.unverified/flatcar-${name}.gz"
  actual="$(sha256sum "${payload}" | awk '{print $1}')"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "${name}: the payload's sha256 ${actual} differs from the server's ${expected}" >&2
    exit 1
  fi
  raw_sha="$(sha256sum "${work}/flatcar-${name}.raw" | awk '{print $1}')"
  gzip -9n -c "${work}/flatcar-${name}.raw" >"${tree}/flatcar-${name}.raw.gz"
  sums+=("$(cd "${tree}" && sha256sum "flatcar-${name}.raw.gz")")
  entries+=("${name}: PayloadSHA256 ${actual} RawSHA256 ${raw_sha}")
  rm -rf "${work}"
done

printf '%s\n' "${sums[@]}" >"${tree}/SHA256SUMS"
echo "mirror tree: ${out}/amd64-usr/${version} (serve ${out} as --nvidia-driver-base-url)" >&2
echo "table entries of Flatcar ${version}:"
printf '  %s\n' "${entries[@]}"
