#!/usr/bin/env bash
# Copies the generated CRDs into the Helm chart, so the chart never drifts from
# config/ (CI runs it and fails on a diff):
#
#   hack/dev.sh make chart-crds
#
# crds:  config/crd/bases/*.yaml -> charts/scarpenter/crds/
#        Helm installs the files of crds/ before the templates and never
#        upgrades or deletes them. The files are copied unchanged.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${root}/charts/scarpenter"

crds() {
  mkdir -p "${chart}/crds"
  # Remove stale CRDs (a renamed or deleted type), then copy.
  find "${chart}/crds" -name '*.yaml' -delete
  cp "${root}"/config/crd/bases/*.yaml "${chart}/crds/"
}

case "${1:-all}" in
  crds|all) crds ;;
  *) echo "usage: $0 [crds|all]" >&2; exit 2 ;;
esac
