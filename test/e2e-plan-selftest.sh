#!/usr/bin/env bash
# Tests test/e2e-plan.sh: the dispatch plan (vendors, mode, overrides: the lanes) and the release and canary plans (the vendor list). No network, no cloud. Run through `make test-e2e-aws-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
plan="${here}/e2e-plan.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
failures=0

# The list of the release gate and the canary: two vendors, none of them azure. The manual
# dispatch ignores it and uses the lane directories (test/e2e-<vendor>/).
printf '# vendors\naws\n\noci\n' >"${tmp}/vendors.txt"
export E2E_VENDORS_FILE="${tmp}/vendors.txt"

expect() { # expect <name> <want> <got>
  if [[ "$2" == "$3" ]]; then
    echo "ok   $1"
  else
    echo "FAIL $1: want '$2', got '$3'"
    failures=$((failures + 1))
  fi
}
words() { sed -n 's/^matrix=//p' | jq -r '[.[] | .vendor + ":" + .mode + ":" + .scenarios] | join(" ")'; }

dispatch() { "${plan}" dispatch "$@" | sed -n 's/^matrix=//p' | jq -c .; }
dplan() { "${plan}" dispatch "$@" | words; }
overrides() { "${plan}" dispatch "$@" | sed -n 's/^env_overrides=//p'; }
refused() { "${plan}" dispatch "$@" >/dev/null 2>&1 && echo accepted || echo refused; }
lanes="$(cd "${here}" && for d in e2e-*/; do echo "${d%/}"; done | sed 's/^e2e-//')"
lane_words() { for v in ${lanes}; do printf '%s:%s:%s ' "${v}" "$1" "$2"; done | sed 's/ $//'; }
expect "dispatch one vendor" '[{"vendor":"oci","mode":"smoke","scenarios":""}]' "$(dispatch oci smoke)"
expect "dispatch all is every lane, not the list" "$(lane_words smoke '')" "$(dplan all smoke)"
expect "dispatch all, full" "$(lane_words full '')" "$(dplan all full)"
expect "dispatch all includes an off-list lane" "azure" "$(dplan all smoke | tr ' ' '\n' | grep -o '^azure')"
expect "dispatch a vendor outside the list" "azure:smoke:" "$(dplan azure smoke)"
expect "dispatch a list with a vendor outside the list" "aws:smoke: azure:smoke:" "$(dplan azure,aws smoke)"
expect "dispatch a vendor list" "aws:smoke: oci:smoke:" "$(dplan aws,oci smoke)"
expect "dispatch a list keeps the lane order" "aws:smoke: oci:smoke:" "$(dplan oci,aws smoke)"
expect "dispatch a scenario list is a full run of those scenarios" "oci:full:gpu" "$(dplan oci gpu)"
expect "dispatch several scenarios" "$(lane_words full amd64,drift)" "$(dplan all amd64,drift)"
expect "dispatch refuses an unknown vendor" "refused" "$(refused nosuchcloud full)"
expect "dispatch refuses one unknown vendor in a list" "refused" "$(refused aws,nosuchcloud full)"
msg="$("${plan}" dispatch nosuchcloud full 2>&1 >/dev/null || true)"
expect "the refusal names the lanes" "yes" "$([[ "${msg}" == *"lanes: "*azure* ]] && echo yes)"
expect "dispatch refuses a bad vendor list" "refused" "$(refused 'aws;oci' full)"
expect "dispatch refuses a bad mode" "refused" "$(refused aws 'a;b')"
expect "dispatch refuses an empty mode" "refused" "$(refused aws '')"
expect "no overrides: empty" "" "$(overrides aws smoke)"
expect "named type overrides" "E2E_GPU_TYPE=oci.VM.GPU.A10.1,E2E_TYPE_CVM=azure.Standard_DC2as_v5" \
  "$(overrides oci gpu oci.VM.GPU.A10.1 azure.Standard_DC2as_v5)"
expect "generic overrides" "E2E_NODE_TIMEOUT=1200,E2E_ROOT_GB=80" "$(overrides aws full '' '' E2E_NODE_TIMEOUT=1200,E2E_ROOT_GB=80)"
expect "named and generic overrides together" "E2E_GPU_TYPE=x,E2E_ROOT_GB=80" "$(overrides aws gpu x '' E2E_ROOT_GB=80)"
expect "overrides refuse a key outside E2E_" "refused" "$(refused aws full '' '' PATH=/tmp)"
expect "overrides refuse a lowercase key" "refused" "$(refused aws full '' '' e2e_x=1)"
expect "overrides refuse a key the workflow sets" "refused" "$(refused aws full '' '' E2E_VENDOR=oci)"
expect "overrides refuse a value with a quote" "refused" "$(refused aws full '' '' "E2E_X='a")"
expect "overrides refuse an entry without =" "refused" "$(refused aws full '' '' E2E_X)"
expect "overrides refuse an empty value" "refused" "$(refused aws full '' '' E2E_X=)"
expect "overrides refuse a key given twice" "refused" "$(refused aws gpu x '' E2E_GPU_TYPE=y)"
expect "a type needs one vendor, not all" "refused" "$(refused all gpu x)"
expect "a type needs one vendor, not a list" "refused" "$(refused aws,oci gpu '' x)"
expect "generic overrides may span vendors" "E2E_ROOT_GB=80" "$(overrides all full '' '' E2E_ROOT_GB=80)"
expect "overrides refuse a bad type" "refused" "$(refused aws gpu 'x;y')"
expect "release: unset (empty) is none: no vendor" "any=false" "$("${plan}" release '' | grep '^any=')"
expect "release: none" "[]" "$("${plan}" release none | sed -n 's/^matrix=//p')"
expect "release: smoke, every vendor of the list" "aws:smoke: oci:smoke:" "$("${plan}" release smoke | words)"
expect "release: full, every vendor of the list" "aws:full: oci:full:" "$("${plan}" release full | words)"
expect "release refuses another mode" "refused" "$("${plan}" release weekly 2>/dev/null || echo refused)"
expect "canary: every vendor of the list" '[{"vendor":"aws"},{"vendor":"oci"}]' "$("${plan}" vendors | sed -n 's/^matrix=//p')"

# A vendor added to the list is planned by the release gate and the canary (GCP's lane is ready
# for its line, not listed until its cost guards are true,
# docs/how-to-guides/testing-and-releasing.md#per-vendor-cost-guards).
printf 'aws\ngcp\n' >"${tmp}/vendors-gcp.txt"
expect "gcp listed: dispatch" "gcp:smoke:" "$(E2E_VENDORS_FILE="${tmp}/vendors-gcp.txt" "${plan}" dispatch gcp smoke | words)"
expect "gcp listed: the release gate" "aws:full: gcp:full:" "$(E2E_VENDORS_FILE="${tmp}/vendors-gcp.txt" "${plan}" release full | words)"
expect "gcp listed: the canary matrix" '[{"vendor":"aws"},{"vendor":"gcp"}]' \
  "$(E2E_VENDORS_FILE="${tmp}/vendors-gcp.txt" "${plan}" vendors | sed -n 's/^matrix=//p')"
# The manual cvm scenario of the Azure lane: a scenario mode plus its instance type override.
printf 'aws\nazure\n' >"${tmp}/vendors-azure.txt"
expect "azure cvm: plan" "azure:full:cvm" "$(E2E_VENDORS_FILE="${tmp}/vendors-azure.txt" "${plan}" dispatch azure cvm | words)"
expect "azure cvm: type override" "E2E_TYPE_CVM=azure.Standard_DC2as_v5" \
  "$(E2E_VENDORS_FILE="${tmp}/vendors-azure.txt" "${plan}" dispatch azure cvm '' azure.Standard_DC2as_v5 | sed -n 's/^env_overrides=//p')"
# What a listed vendor needs next to its directory: the sweeper workflow and the credentials
# step of the canary (test/e2e-gcp/selftest.sh checks the gcp ones in detail).
for v in $(env -u E2E_VENDORS_FILE "${plan}" vendors | sed -n 's/^matrix=//p' | jq -r '.[].vendor') gcp; do
  [[ -f "${here}/../.github/workflows/e2e-${v}-sweeper.yml" ]] || {
    echo "FAIL the vendor ${v} has no .github/workflows/e2e-${v}-sweeper.yml"
    failures=$((failures + 1))
  }
  grep -qE "vendor == '${v}'" "${here}/../.github/workflows/e2e-canary.yml" || {
    echo "FAIL e2e-canary.yml has no credentials step for the vendor ${v}"
    failures=$((failures + 1))
  }
done
# Every lane can be dispatched: e2e-cloud.yml has a credentials step for it.
for v in ${lanes}; do
  grep -qE "vendor == '${v}'" "${here}/../.github/workflows/e2e-cloud.yml" || {
    echo "FAIL e2e-cloud.yml has no credentials step for the lane ${v}"
    failures=$((failures + 1))
  }
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
