#!/usr/bin/env bash
# Tests test/e2e-plan.sh: the path rules, the labels, the dispatch and release plans and
# the vendor list. No network, no cloud. Run through `make test-e2e-aws-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
plan="${here}/e2e-plan.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
failures=0

# A second vendor, so that the rules are tested for more than one.
printf '# vendors\naws\n\noci\n' >"${tmp}/vendors.txt"
export E2E_VENDORS_FILE="${tmp}/vendors.txt"

# pr_plan <labels JSON> <file>...: the matrix as "vendor:mode" words.
pr_plan() {
  local labels="$1"
  shift
  printf '%s\n' "$@" | E2E_LABELS="${labels}" "${plan}" pr | sed -n 's/^matrix=//p' |
    jq -r '[.[] | .vendor + ":" + .mode] | join(" ")'
}
expect() { # expect <name> <want> <got>
  if [[ "$2" == "$3" ]]; then
    echo "ok   $1"
  else
    echo "FAIL $1: want '$2', got '$3'"
    failures=$((failures + 1))
  fi
}

expect "docs only: nothing" "" "$(pr_plan '[]' docs/how-to-guides/testing.md README.md)"
expect "workflows of other things: nothing" "" "$(pr_plan '[]' .github/workflows/ci.yml .github/workflows/e2e-canary.yml)"
expect "tests only: nothing" "" "$(pr_plan '[]' pkg/bootstrap/render_test.go pkg/vendors/aws/testdata/x.json)"
expect "a vendor's driver: its full lane only" "aws:full" "$(pr_plan '[]' pkg/vendors/aws/driver.go)"
expect "another vendor's driver" "oci:full" "$(pr_plan '[]' pkg/vendors/oci/driver.go)"
expect "a vendor's lane scripts" "oci:full" "$(pr_plan '[]' test/e2e-oci/scenarios.sh)"
expect "a vendor's sweeper workflow" "aws:full" "$(pr_plan '[]' .github/workflows/e2e-aws-sweeper.yml)"
expect "the caller workflow: every vendor full" "aws:full oci:full" "$(pr_plan '[]' .github/workflows/e2e.yml)"
expect "the reusable workflow: every vendor full" "aws:full oci:full" "$(pr_plan '[]' .github/workflows/e2e-cloud.yml)"
expect "the planner itself: every vendor full" "aws:full oci:full" "$(pr_plan '[]' test/e2e-plan.sh)"
expect "bootstrap: smoke everywhere" "aws:smoke oci:smoke" "$(pr_plan '[]' pkg/bootstrap/render.go)"
expect "the cloudprovider" "aws:smoke oci:smoke" "$(pr_plan '[]' pkg/cloudprovider/cloudprovider.go)"
expect "artifacts" "aws:smoke oci:smoke" "$(pr_plan '[]' pkg/artifacts/artifacts.go)"
expect "resources" "aws:smoke oci:smoke" "$(pr_plan '[]' pkg/resources/x.go)"
expect "the chart" "aws:smoke oci:smoke" "$(pr_plan '[]' charts/scarpenter/values.yaml)"
expect "the shared driver contract" "aws:smoke oci:smoke" "$(pr_plan '[]' pkg/vendors/driver.go)"
expect "the driver facts" "aws:smoke oci:smoke" "$(pr_plan '[]' pkg/vendors/facts/facts.go)"
expect "go.mod" "aws:smoke oci:smoke" "$(pr_plan '[]' go.mod)"
expect "the image build" "aws:smoke oci:smoke" "$(pr_plan '[]' Dockerfile .dockerignore)"
expect "a dev-only vendor package: nothing" "" "$(pr_plan '[]' pkg/vendors/fake/driver.go pkg/vendors/qemu/driver.go)"
expect "full wins over smoke, per vendor" "aws:full oci:smoke" "$(pr_plan '[]' pkg/bootstrap/render.go pkg/vendors/aws/driver.go)"
expect "the label l5:full forces every vendor" "aws:full oci:full" "$(pr_plan '["l5:full"]' docs/x.md)"
expect "the label l5:skip skips" "" "$(pr_plan '["l5:skip"]' pkg/vendors/aws/driver.go)"
expect "l5:skip wins over l5:full" "" "$(pr_plan '["l5:full", "l5:skip"]' pkg/vendors/aws/driver.go)"
expect "unrelated labels change nothing" "aws:smoke oci:smoke" "$(pr_plan '["bug"]' pkg/bootstrap/render.go)"

dispatch() { "${plan}" dispatch "$@" | sed -n 's/^matrix=//p' | jq -c .; }
expect "dispatch one vendor" '[{"vendor":"oci","mode":"smoke","scenarios":"amd64"}]' "$(dispatch oci smoke amd64)"
expect "dispatch all" "2" "$(dispatch all full | jq length)"
expect "dispatch refuses an unknown vendor" "refused" "$(dispatch gcp full 2>/dev/null || echo refused)"
expect "dispatch refuses a bad mode" "refused" "$(dispatch aws weekly 2>/dev/null || echo refused)"
expect "dispatch refuses a bad scenario list" "refused" "$(dispatch aws full 'a;b' 2>/dev/null || echo refused)"
expect "release: every vendor, full" "aws:full oci:full" "$("${plan}" release | sed -n 's/^matrix=//p' | jq -r '[.[] | .vendor + ":" + .mode] | join(" ")')"
expect "canary: every vendor" '[{"vendor":"aws"},{"vendor":"oci"}]' "$("${plan}" vendors | sed -n 's/^matrix=//p')"
expect "any=false for an empty plan" "any=false" "$(printf '%s\n' docs/x.md | "${plan}" pr | grep '^any=')"

# The GCP lane is ready for its line in test/e2e-vendors.txt (not listed until its cost guards
# are true, docs/how-to-guides/testing-and-releasing.md#per-vendor-cost-guards): with a list that has
# gcp, its driver, its lane, its sweeper and its sweep package are full rules of its own, the shared
# paths reach it, and the canary, dispatch and release plans include it.
printf 'aws\ngcp\n' >"${tmp}/vendors-gcp.txt"
gcp_pr_plan() { # gcp_pr_plan <file>...
  printf '%s\n' "$@" | E2E_VENDORS_FILE="${tmp}/vendors-gcp.txt" E2E_LABELS='[]' "${plan}" pr | sed -n 's/^matrix=//p' |
    jq -r '[.[] | .vendor + ":" + .mode] | join(" ")'
}
expect "gcp listed: its driver" "gcp:full" "$(gcp_pr_plan pkg/vendors/gcp/driver.go)"
expect "gcp listed: its sweep package" "gcp:full" "$(gcp_pr_plan pkg/vendors/gcp/sweep/sweep.go)"
expect "gcp listed: its lane scripts" "gcp:full" "$(gcp_pr_plan test/e2e-gcp/scenarios.sh)"
expect "gcp listed: its sweeper workflow" "gcp:full" "$(gcp_pr_plan .github/workflows/e2e-gcp-sweeper.yml)"
expect "gcp listed: the AWS sweeper is not its business" "aws:full" "$(gcp_pr_plan .github/workflows/e2e-aws-sweeper.yml)"
expect "gcp listed: bootstrap is a smoke for both" "aws:smoke gcp:smoke" "$(gcp_pr_plan pkg/bootstrap/render.go)"
expect "gcp listed: the reusable workflow is full for both" "aws:full gcp:full" "$(gcp_pr_plan .github/workflows/e2e-cloud.yml)"
expect "gcp listed: dispatch" '[{"vendor":"gcp","mode":"smoke","scenarios":""}]' \
  "$(E2E_VENDORS_FILE="${tmp}/vendors-gcp.txt" "${plan}" dispatch gcp smoke | sed -n 's/^matrix=//p' | jq -c .)"
expect "gcp listed: the canary matrix" '[{"vendor":"aws"},{"vendor":"gcp"}]' \
  "$(E2E_VENDORS_FILE="${tmp}/vendors-gcp.txt" "${plan}" vendors | sed -n 's/^matrix=//p')"
expect "gcp listed: the release gate" "aws:full gcp:full" \
  "$(E2E_VENDORS_FILE="${tmp}/vendors-gcp.txt" "${plan}" release | sed -n 's/^matrix=//p' | jq -r '[.[] | .vendor + ":" + .mode] | join(" ")')"
# What a listed vendor needs next to its directory: the sweeper workflow and the credentials
# steps of the two reusable workflows (test/e2e-gcp/selftest.sh checks the gcp ones in detail).
for v in $(env -u E2E_VENDORS_FILE "${plan}" vendors | sed -n 's/^matrix=//p' | jq -r '.[].vendor') gcp; do
  [[ -f "${here}/../.github/workflows/e2e-${v}-sweeper.yml" ]] || {
    echo "FAIL the vendor ${v} has no .github/workflows/e2e-${v}-sweeper.yml"
    failures=$((failures + 1))
  }
  for w in e2e-cloud.yml e2e-canary.yml; do
    grep -qE "vendor == '${v}'" "${here}/../.github/workflows/${w}" || {
      echo "FAIL ${w} has no credentials step for the vendor ${v}"
      failures=$((failures + 1))
    }
  done
done

# The real list names a lane directory for each vendor.
unset E2E_VENDORS_FILE
real="$("${plan}" vendors | sed -n 's/^matrix=//p' | jq -r '.[].vendor')"
for v in ${real}; do
  [[ -d "${here}/e2e-${v}" ]] || {
    echo "FAIL the vendor ${v} of e2e-vendors.txt has no test/e2e-${v}/"
    failures=$((failures + 1))
  }
done

if [[ "${failures}" -ne 0 ]]; then
  echo "e2e-plan-selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "e2e-plan-selftest: all passed"
