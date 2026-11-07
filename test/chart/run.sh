#!/usr/bin/env bash
# Helm chart checks (no cluster needed): run them in the dev container,
#
#   hack/dev.sh make test-chart
#
#   UPDATE=1   rewrite the golden files (test/chart/testdata/*.yaml) from the
#              current templates; review the diff like code
#
# 1. helm lint --strict for every values file of test/chart/testdata/values;
# 2. the Go tests of this directory (build tag chart): golden `helm template`
#    output, the values schema and the template checks against bad values,
#    every controller flag settable and accepted by the controller's own flag
#    parser, the chart's RBAC and CRDs equal to config/ and Karpenter core's.
# The install into a kind cluster is part of the L3 lane (make test-kwok).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
chart="${root}/charts/scarpenter"
cd "${root}"

echo "==> helm lint"
for f in "${here}"/testdata/values/*.yaml; do
  helm lint --strict "${chart}" -f "${f}"
done

echo "==> go test (golden output, schema, flags, RBAC and CRD sync)"
if [[ "${UPDATE:-}" == "1" ]]; then
  export UPDATE_GOLDEN=1
fi
go test -tags chart -count=1 ./test/chart/

echo "chart checks passed"
