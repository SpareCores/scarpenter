#!/usr/bin/env bash
# L5 setup: the Flatcar template of the pinned release exists in the CI sub-account (zone
# UPCLOUD_ZONE, one per storage tier: MaxIOPS for the data disks, Standard for the root disk of
# the STARTER plans), imported from the images repository when it is missing. The
# lane's preflight and the driver need it (docs/how-to-guides/set-up-upcloud.md section 1).
#
# Idempotent: a template with the labels of defaults.sh (and scarpenter-use=true, which keeps
# the sweeper off it, and scarpenter-lane=ci, which marks it as the lane's own) in the zone ends the script at once. Otherwise: download the release's
# image and SHA256SUMS, check the hash, create a 10 GB MaxIOPS storage, upload the image
# (direct_upload: UpCloud decompresses it), templatize, label, and delete the source
# storage (it is billed until deleted, and goes on failure too).
#
# The release is the tag <channel>-<version> of the images repository: the later -rN
# releases of a version carry other platforms only, and E2E_UPCLOUD_IMAGE_RELEASE overrides it.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-upcloud/lib.sh
source "${here}/lib.sh"

: "${E2E_UPCLOUD_IMAGE_RELEASE:=${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}}"
: "${E2E_UPCLOUD_IMAGES_URL:=https://github.com/SpareCores/flatcar-images/releases/download}"
tier=maxiops # the tier being ensured (main loops over both)
# The title and the scarpenter-lane=ci label are the lane's own: the CI sub-account is shared
# with production resources, so the lane never uses (or counts) a template without the label.
template_title() { echo "flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-amd64-${tier}-ci"; }
image_file="flatcar-upcloud-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-amd64.img.gz"

# template_labels: the labels of the template, as the request spells them.
template_labels() {
  jq -nc --arg c "${E2E_FLATCAR_CHANNEL}" --arg v "${E2E_FLATCAR_VERSION}" --arg t "${tier}" \
    '[{key: "scarpenter-image", value: "flatcar"}, {key: "flatcar-channel", value: $c}, {key: "flatcar-version", value: $v},
      {key: "flatcar-arch", value: "amd64"}, {key: "flatcar-tier", value: $t}, {key: "scarpenter-use", value: "true"},
      {key: "scarpenter-lane", value: "ci"}]'
}

# matching_templates <zone>: stdin is the private storage list; prints the UUIDs of the
# templates of the zone that carry every label of template_labels (in any state).
matching_templates() {
  jq -r --arg z "$1" --argjson want "$(template_labels)" \
    '.storages.storage[]? | select(.type == "template" and .zone == $z)
      | select(. as $s | all($want[]; . as $w | ($s.labels | if type == "array" then . else (.label // []) end) | any(.key == $w.key and .value == $w.value)))
      | .uuid'
}

# existing_templates: the matching templates of the zone now.
existing_templates() { up GET "/storage/private?label=scarpenter-lane=ci" | matching_templates "${UPCLOUD_ZONE}"; }

storage_state() { up GET "/storage/$1" | jq -r '.storage.state'; }

source_uuid=""
cleanup() {
  [[ -n "${source_uuid}" ]] || return 0
  echo "deleting the source storage ${source_uuid}"
  # A storage being templatized is in maintenance and refuses the delete.
  local deadline=$((SECONDS + 300))
  while [[ "${SECONDS}" -lt "${deadline}" && "$(storage_state "${source_uuid}" 2>/dev/null)" == maintenance ]]; do
    sleep 5
  done
  up DELETE "/storage/${source_uuid}" >/dev/null || echo "::warning::the source storage ${source_uuid} is not deleted; the sweeper does not touch it" >&2
}

import_template() {
  local tmp url sum
  tmp="$(mktemp -d)"
  url="${E2E_UPCLOUD_IMAGES_URL}/${E2E_UPCLOUD_IMAGE_RELEASE}"

  log "downloading ${image_file} of the release ${E2E_UPCLOUD_IMAGE_RELEASE} and checking its hash"
  curl -fsSL --retry 3 -o "${tmp}/SHA256SUMS" "${url}/SHA256SUMS"
  curl -fsSL --retry 3 -o "${tmp}/${image_file}" "${url}/${image_file}"
  sum="$(awk -v f="${image_file}" '$2 == f {print $1}' "${tmp}/SHA256SUMS")"
  [[ "${sum}" =~ ^[0-9a-f]{64}$ ]] || fail "SHA256SUMS of the release ${E2E_UPCLOUD_IMAGE_RELEASE} has no entry for ${image_file}"
  (cd "${tmp}" && echo "${sum}  ${image_file}" | sha256sum -c -)

  log "creating the source storage (${tier}, ${UPCLOUD_ZONE}) and uploading the image"
  source_uuid="$(up_json POST /storage "$(jq -nc --arg z "${UPCLOUD_ZONE}" --arg t "${tier}" --arg n "$(template_title)-import-${E2E_RUN_ID:-0}" \
    '{storage: {size: 10, tier: $t, title: $n, zone: $z}}')" | jq -r '.storage.uuid')"
  assert_nonempty "the source storage" "${source_uuid}"
  trap cleanup EXIT
  local upload
  upload="$(up_json POST "/storage/${source_uuid}/import" '{"storage_import":{"source":"direct_upload"}}' | jq -r '.storage_import.direct_upload_url')"
  [[ -n "${upload}" && "${upload}" != null ]] || fail "the import call answered no upload URL"
  # The importer decompresses by the Content-Type of the upload (plan 19; the Terraform
  # provider sends application/gzip for a .gz): as octet-stream the gzip would be the disk.
  # The upload URL takes no token (the SDK's direct upload sends none either).
  curl -fsS --retry 3 --max-time 1800 -X PUT -H 'Content-Type: application/gzip' --data-binary "@${tmp}/${image_file}" "${upload}"
  rm -rf "${tmp}"
  wait_import "${source_uuid}" 1200
  # A storage is in maintenance for a while after its import, and refuses to be templatized (409).
  wait_until "the source storage to be online" 600 source_online

  log "templatizing and labelling"
  local tpl
  tpl="$(up_json POST "/storage/${source_uuid}/templatize" "$(jq -nc --arg t "$(template_title)" '{storage: {title: $t}}')" | jq -r '.storage.uuid')"
  assert_nonempty "the template" "${tpl}"
  wait_until "the template to be online" 600 template_online "${tpl}"
  up_json PUT "/storage/${tpl}" "$(jq -nc --argjson l "$(template_labels)" '{storage: {labels: $l}}')" >/dev/null

  cleanup
  source_uuid=""

  # A concurrent run may have imported the same release: keep one (the driver wants exactly one match).
  if [[ "$(existing_templates | wc -l)" -gt 1 ]]; then
    note "another template of $(template_title) appeared: deleting this run's ${tpl}"
    up DELETE "/storage/${tpl}" >/dev/null
  fi
}

# wait_import <storage uuid> <timeout seconds>: until the import completes; a failed import
# ends the script at once (wait_until runs its command in a subshell, where fail cannot).
wait_import() {
  local deadline=$((SECONDS + $2)) j state
  while true; do
    j="$(up GET "/storage/$1/import")"
    state="$(jq -r '.storage_import.state' <<<"${j}")"
    case "${state}" in
      completed)
        echo "ok: the import completed"
        return 0
        ;;
      failed | cancelled | cancelling)
        echo "import ${state}: $(jq -r '.storage_import | "\(.error_code) \(.error_message)"' <<<"${j}")" >&2
        fail "the import of ${image_file} ${state}"
        ;;
    esac
    [[ "${SECONDS}" -lt "${deadline}" ]] || fail "timed out after $2s waiting for the import (${state})"
    sleep 10
  done
}

source_online() { [[ "$(storage_state "${source_uuid}")" == online ]]; }

template_online() { [[ "$(storage_state "$1")" == online ]]; }

ensure_tier() {
  local have
  have="$(existing_templates)"
  if [[ -n "${have}" ]]; then
    echo "the template $(template_title) exists in ${UPCLOUD_ZONE}: ${have//$'\n'/ }"
    wait_until "the template to be online" 600 template_online "${have%%$'\n'*}"
    return 0
  fi
  import_template
  echo "the template $(template_title) is ready in ${UPCLOUD_ZONE}"
}

main() {
  need UPCLOUD_TOKEN UPCLOUD_ZONE E2E_FLATCAR_CHANNEL E2E_FLATCAR_VERSION
  for tier in maxiops standard; do ensure_tier; done
}

# Sourced by selftest.sh for the pure helpers; run by the workflow.
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
