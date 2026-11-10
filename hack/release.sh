#!/usr/bin/env bash
# The build steps of the release (.github/workflows/release.yml), as one script, so
# that the workflow's dry run and a developer run exactly the same commands:
#
#   hack/dev.sh hack/release.sh dry-run v0.1.0-rc.1
#
#   version <tag>                       validates the tag (v<major>.<minor>.<patch>[-<prerelease>])
#                                       and prints version= and prerelease= lines
#   latest <tag> <prerelease>           reads the existing tags from stdin and prints latest= (the
#                                       tag is the highest stable version: it may move "latest" and
#                                       the major tag) and minor= (the highest of its minor: it may
#                                       move the 0.1 tag), so that no tag ever moves backwards
#   package-chart <version> <dir>       packages the chart with that version and appVersion
#                                       into <dir> and checks the package
#   check-image <image> <dev|prod> <version>
#                                       checks a built image: the version it reports and the
#                                       drivers (flags) it contains
#   dry-run <tag>                       builds both images (linux/amd64 and linux/arm64) without
#                                       pushing, checks them, packages and checks the chart;
#                                       removes what it created
#
# Nothing here pushes, signs or publishes: that is the workflow's, on a tag. Nor does it run the
# release gate: the workflow runs the full real-cloud lane (L5) of every vendor on the tagged
# commit before it publishes (docs/how-to-guides/testing-and-releasing.md#releasing), and the
# dry run does not need it: it checks what the build can check without a cloud.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${root}"

IMAGE_REPO="${IMAGE_REPO:-ghcr.io/sparecores/scarpenter}"

die() {
  echo "release.sh: $*" >&2
  exit 1
}

version_of_tag() { # version_of_tag <tag>: prints "version prerelease"
  local tag="$1"
  [[ "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
    die "the tag ${tag} is not v<major>.<minor>.<patch>[-<prerelease>]"
  if [[ "${tag}" == *-* ]]; then
    echo "${tag#v} true"
  else
    echo "${tag#v} false"
  fi
}

cmd_version() {
  [[ $# -eq 1 ]] || die "usage: version <tag>"
  local out
  out="$(version_of_tag "$1")"
  echo "version=${out% *}"
  echo "prerelease=${out#* }"
}

cmd_latest() {
  [[ $# -eq 2 ]] || die "usage: latest <tag> <prerelease>"
  local tag="$1" prerelease="$2" stable latest=false minor=false prefix
  # The tag itself counts, whether or not it exists yet.
  # grep finds nothing for a pre-release tag in a repository without stable tags
  # (the first release candidate): that is no error.
  stable="$( { cat; echo "${tag}"; } | { grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true; } | sort -uV)"
  if [[ "${prerelease}" == "false" ]]; then
    [[ "$(tail -n 1 <<<"${stable}")" == "${tag}" ]] && latest=true
    prefix="${tag%.*}."
    [[ "$(awk -v p="${prefix}" 'index($0, p) == 1 && substr($0, length(p) + 1) ~ /^[0-9]+$/' <<<"${stable}" | tail -n 1)" == "${tag}" ]] && minor=true
  fi
  echo "latest=${latest}"
  echo "minor=${minor}"
}

cmd_package_chart() {
  [[ $# -eq 2 ]] || die "usage: package-chart <version> <dir>"
  local version="$1" dir="$2" pkg values
  mkdir -p "${dir}"
  helm package charts/scarpenter --version "${version}" --app-version "${version}" --destination "${dir}"
  pkg="${dir}/scarpenter-${version}.tgz"
  [[ -f "${pkg}" ]] || die "helm did not produce ${pkg}"
  values="${root}/test/chart/testdata/values/default.yaml"
  helm lint --strict "${pkg}" -f "${values}"
  # The packaged chart pulls the image of its own version.
  helm template scarpenter "${pkg}" -f "${values}" | grep -q "image: \"${IMAGE_REPO}:${version}\"" ||
    die "the packaged chart does not use ${IMAGE_REPO}:${version}"
  helm show chart "${pkg}" | grep -q "^appVersion: ${version}\$" || die "appVersion of the package is not ${version}"
  echo "chart package ${pkg}"
}

cmd_check_image() {
  [[ $# -eq 3 ]] || die "usage: check-image <image> <dev|prod> <version>"
  local image="$1" kind="$2" version="$3" reported help
  [[ "${kind}" == dev || "${kind}" == prod ]] || die "kind must be dev or prod"
  # The binary is built with -s -w, which strips the build settings (tags,
  # ldflags), so the stamped version and commit are read from what it reports,
  # and the build tags from the flags of the drivers it contains. The image runs
  # on this machine's architecture.
  reported="$(docker run --rm "${image}" --version)" || die "${image}: --version failed"
  [[ "${reported}" == "scarpenter ${version} (commit "* ]] ||
    die "${image}: --version reports \"${reported}\", want version ${version}"
  if [[ -n "${COMMIT_EXPECTED:-}" && "${reported}" != *"commit ${COMMIT_EXPECTED},"* ]]; then
    die "${image}: --version reports \"${reported}\", want commit ${COMMIT_EXPECTED}"
  fi
  help="$(docker run --rm "${image}" -help 2>&1)" || die "${image} does not run: ${help}"
  grep -q -- '-cluster-name' <<<"${help}" || die "${image}: -help does not list -cluster-name"
  if [[ "${kind}" == dev ]]; then
    grep -q -- '-fake-catalog-vendor' <<<"${help}" || die "${image}: the dev image has no fake driver flags"
  elif grep -Eq -- '-(fake|qemu)-' <<<"${help}"; then
    die "${image}: the production image has development driver flags"
  fi
  echo "image ${image} (${kind}): ${reported}"
}

cmd_dry_run() {
  [[ $# -eq 1 ]] || die "usage: dry-run <tag>"
  local out version commit work
  out="$(version_of_tag "$1")"
  version="${out% *}"
  commit="$(git -c safe.directory='*' rev-parse HEAD 2>/dev/null || echo 0000000000000000000000000000000000000000)"
  work="$(mktemp -d)"
  local prod="scarpenter-release-dryrun:${version}" dev="scarpenter-release-dryrun-dev:${version}"
  # Expanded now: the variables are locals, gone when the trap runs.
  # shellcheck disable=SC2064
  trap "docker rmi -f '${prod}' '${dev}' >/dev/null 2>&1 || true; rm -rf '${work}'" EXIT

  echo "==> building the images for ${version} (nothing is pushed)"
  local args=(--build-arg "VERSION=${version}" --build-arg "COMMIT=${commit}")
  docker buildx build --platform linux/amd64 --load "${args[@]}" -t "${prod}" .
  docker buildx build --platform linux/amd64 --load "${args[@]}" --build-arg GO_TAGS=scarpenter_dev -t "${dev}" .
  # The binaries are cross-compiled on the build platform: arm64 needs no emulation.
  docker buildx build --platform linux/arm64 --output type=cacheonly "${args[@]}" .
  docker buildx build --platform linux/arm64 --output type=cacheonly "${args[@]}" --build-arg GO_TAGS=scarpenter_dev .

  echo "==> checking the images"
  COMMIT_EXPECTED="${commit}" cmd_check_image "${prod}" prod "${version}"
  COMMIT_EXPECTED="${commit}" cmd_check_image "${dev}" dev "${version}"

  echo "==> packaging the chart"
  cmd_package_chart "${version}" "${work}/chart"
  echo "dry run of ${1} passed"
}

case "${1:-}" in
  version) shift; cmd_version "$@" ;;
  latest) shift; cmd_latest "$@" ;;
  package-chart) shift; cmd_package_chart "$@" ;;
  check-image) shift; cmd_check_image "$@" ;;
  dry-run) shift; cmd_dry_run "$@" ;;
  *) die "usage: $0 version|package-chart|check-image|dry-run (see the header of this file)" ;;
esac
