#!/usr/bin/env bash
# Fails when an in-repo Flatcar pin differs from the release the operator of the CI
# account provides: the repository variables FLATCAR_CHANNEL and FLATCAR_VERSION.
#
#   hack/check-flatcar-pins.sh <channel> <version>
#   hack/check-flatcar-pins.sh "$(gh variable get FLATCAR_CHANNEL)" "$(gh variable get FLATCAR_VERSION)"
#
# With an empty channel or version (a fork has no variables) it prints a notice and
# exits 0.
#
# Checked:
#   - test/flatcar-pin.sh, the in-repo default of the e2e lanes and the canary, and that
#     every test/e2e-*/defaults.sh sources it and pins nothing of its own;
#   - charts/scarpenter/values.yaml (examples.nodeClass.flatcarChannel and
#     flatcarVersion);
#   - every release number and channel in config/samples, docs/examples and
#     docs/how-to-guides: "4757.2.3", "flatcar-stable-4757-2-3" (dashes), and the channel
#     of "channel: stable", "channel=stable", "flatcar-stable-..." and "Flatcar-stable-...".
#
# Excluded on purpose:
#   - docs/plans/: history;
#   - docs/explanation/ and docs/reference/: dated research and illustrations of a driver's
#     naming (they also name other releases, such as the LTS one);
#   - the CRD schema, the API reference and the chart's CRD copy: generated from the "e.g."
#     of pkg/apis/v1alpha1/scnodeclass.go, an example and not a pin;
#   - *_test.go and the fixtures of the Go tests: they pin a version on purpose, to test
#     the code against a fixed release.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <channel> <version>" >&2
  exit 2
fi
channel="$1"
version="$2"
if [[ -z "${channel}" || -z "${version}" ]]; then
  echo "::notice::the repository variables FLATCAR_CHANNEL and FLATCAR_VERSION are empty (a fork?): the Flatcar pins are not checked"
  exit 0
fi
[[ "${channel}" =~ ^[a-z]+$ ]] || { echo "the channel '${channel}' is not a lowercase word" >&2; exit 2; }
[[ "${version}" =~ ^[0-9]{4}\.[0-9]+\.[0-9]+$ ]] || { echo "the version '${version}' is not <major>.<minor>.<patch>" >&2; exit 2; }
dashed="${version//./-}"

cd "$(dirname "${BASH_SOURCE[0]}")/.."

bad=0
report() { # report <file>:<line> <found> <expected>
  echo "${1}: found '${2}', expected '${3}'" >&2
  bad=$((bad + 1))
}

# The default of the lanes: read it the way the lanes do.
pin="test/flatcar-pin.sh"
got_channel="$(env -u E2E_FLATCAR_CHANNEL -u E2E_FLATCAR_VERSION bash -c ". ${pin}; printf %s \"\${E2E_FLATCAR_CHANNEL}\"")"
got_version="$(env -u E2E_FLATCAR_CHANNEL -u E2E_FLATCAR_VERSION bash -c ". ${pin}; printf %s \"\${E2E_FLATCAR_VERSION}\"")"
[[ "${got_channel}" == "${channel}" ]] || report "${pin}:$(grep -n 'E2E_FLATCAR_CHANNEL:=' "${pin}" | cut -d: -f1)" "${got_channel}" "${channel}"
[[ "${got_version}" == "${version}" ]] || report "${pin}:$(grep -n 'E2E_FLATCAR_VERSION:=' "${pin}" | cut -d: -f1)" "${got_version}" "${version}"

for f in test/e2e-*/defaults.sh; do
  grep -q 'flatcar-pin.sh' "${f}" || report "${f}:1" "no source of test/flatcar-pin.sh" "source of test/flatcar-pin.sh"
  while IFS=: read -r n _; do
    report "${f}:${n}" "its own pin" "none (test/flatcar-pin.sh holds it)"
  done < <(grep -nE 'E2E_FLATCAR_(CHANNEL|VERSION):=' "${f}" || true)
done

# The chart's example NodeClass.
values="charts/scarpenter/values.yaml"
check_value() { # check_value <file> <key> <expected>
  local n v
  while IFS=: read -r n v; do
    v="${v#*:}"
    v="$(sed -E 's/[[:space:]]*#.*$//; s/^[[:space:]]*//; s/"//g' <<<"${v}")"
    [[ "${v}" == "${3}" ]] || report "${1}:${n}" "${v}" "${3}"
  done < <(grep -nE "^[[:space:]]+${2}:" "${1}" || true)
}
check_value "${values}" flatcarChannel "${channel}"
check_value "${values}" flatcarVersion "${version}"

# Samples, examples and guides.
mapfile -t files < <(find config/samples docs/examples docs/how-to-guides -type f \( -name '*.yaml' -o -name '*.md' \) | sort)
for f in "${files[@]}"; do
  while IFS=: read -r n found; do
    [[ "${found}" == "${version}" ]] || report "${f}:${n}" "${found}" "${version}"
  done < <(grep -noE '\b[0-9]{4}\.[0-9]+\.[0-9]+\b' "${f}" || true)
  while IFS=: read -r n found; do
    d="$(sed -E 's/^[Ff]latcar-[A-Za-z]+-//' <<<"${found}")"
    [[ "${d}" == "${dashed}" ]] || report "${f}:${n}" "${d}" "${dashed}"
  done < <(grep -noiE 'flatcar-[a-z]+-[0-9]{4}-[0-9]+-[0-9]+' "${f}" || true)
  while IFS=: read -r n found; do
    c="$(grep -oE '(stable|lts|beta|alpha)$' <<<"${found}")"
    [[ "${c}" == "${channel}" ]] || report "${f}:${n}" "${c}" "${channel}"
  done < <(grep -noiE '(channel"?[:=] *"?|flatcar-)(stable|lts|beta|alpha)' "${f}" || true)
done

if [[ "${bad}" -ne 0 ]]; then
  echo "${bad} Flatcar pin(s) differ from FLATCAR_CHANNEL=${channel} FLATCAR_VERSION=${version}. Update the pins above, or the repository variables." >&2
  exit 1
fi
echo "the Flatcar pins match ${channel} ${version}"
