#!/usr/bin/env bash
# Copies what the Helm chart takes from config/ into the chart, so the chart
# never drifts from it (CI runs it and fails on a diff):
#
#   hack/dev.sh make chart-crds chart-rbac
#
# crds:  config/crd/bases/*.yaml and Karpenter core's NodePool and
#        NodeClaim CRDs (from the core version of go.mod, Apache-2.0) ->
#        charts/scarpenter/crds/. Helm installs the files of crds/ before the
#        templates, skips those that exist and never upgrades or deletes them.
#        The files are copied unchanged. The CapacityBuffer CRD (another SIG's
#        API) and NodeOverlay are not shipped: both are behind core feature gates
#        that are off, and a stray copy of a CRD another Karpenter's chart manages
#        would break that chart's upgrades. Add them when a gate is turned on.
# rbac:  the rules of config/rbac and config/rbac-core and the node-join
#        bindings -> charts/scarpenter/files/rbac.yaml. The templates add names
#        and namespaces; test/chart/run.sh renders the chart and compares the
#        result with config/.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${root}/charts/scarpenter"

crds() {
  mkdir -p "${chart}/crds"
  # Remove stale CRDs (a renamed or deleted type), then copy.
  find "${chart}/crds" -name '*.yaml' -delete
  cp "${root}"/config/crd/bases/*.yaml "${chart}/crds/"
  local core f
  core="$(cd "${root}" && go list -m -f '{{.Dir}}' sigs.k8s.io/karpenter)"
  for f in karpenter.sh_nodeclaims karpenter.sh_nodepools; do
    cp "${core}/pkg/apis/crds/${f}.yaml" "${chart}/crds/${f}.yaml"
    chmod u+w "${chart}/crds/${f}.yaml"
  done
}

rbac() {
  local ctl="${root}/config/rbac/controller.yaml"
  local core="${root}/config/rbac-core/core.yaml"
  local join="${root}/config/rbac/node-join.yaml"
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' RETURN
  yq 'select(.kind == "ClusterRole" and .metadata.name == "scarpenter") | .rules' "${ctl}" >"${tmp}/controller.yaml"
  yq 'select(.kind == "Role" and .metadata.name == "scarpenter-bootstrap-tokens") | .rules' "${ctl}" >"${tmp}/bootstrap-tokens.yaml"
  yq 'select(.kind == "Role" and .metadata.name == "scarpenter-auth-key") | .rules' "${ctl}" >"${tmp}/auth-key.yaml"
  yq 'select(.kind == "Role" and .metadata.name == "scarpenter-cluster-dns") | .rules' "${ctl}" >"${tmp}/cluster-dns.yaml"
  yq 'select(.kind == "ClusterRole" and .metadata.name == "scarpenter-core") | .rules' "${core}" >"${tmp}/core.yaml"
  yq 'select(.kind == "Role" and .metadata.name == "scarpenter-leader-election") | .rules' "${core}" >"${tmp}/leader-election.yaml"
  yq ea '[select(.kind == "ClusterRoleBinding") | {"name": .metadata.name, "roleRef": .roleRef, "subjects": .subjects}]' "${join}" >"${tmp}/node-join.yaml"
  for f in controller bootstrap-tokens auth-key cluster-dns core leader-election node-join; do
    [[ "$(yq 'length' "${tmp}/${f}.yaml")" -gt 0 ]] || { echo "no ${f} rules found in config/" >&2; exit 1; }
  done
  mkdir -p "${chart}/files"
  {
    echo '# Generated from config/rbac and config/rbac-core by hack/chart-sync.sh (make chart-rbac); do not edit.'
    yq -n '{
      "controller": {"rules": load("'"${tmp}"'/controller.yaml")},
      "bootstrapTokens": {"rules": load("'"${tmp}"'/bootstrap-tokens.yaml")},
      "authKey": {"rules": load("'"${tmp}"'/auth-key.yaml")},
      "clusterDNS": {"rules": load("'"${tmp}"'/cluster-dns.yaml")},
      "core": {"rules": load("'"${tmp}"'/core.yaml")},
      "leaderElection": {"rules": load("'"${tmp}"'/leader-election.yaml")},
      "nodeJoin": load("'"${tmp}"'/node-join.yaml")
    }'
  } >"${chart}/files/rbac.yaml"
}

case "${1:-all}" in
  crds) crds ;;
  rbac) rbac ;;
  all) crds; rbac ;;
  *) echo "usage: $0 [crds|rbac|all]" >&2; exit 2 ;;
esac
