#!/usr/bin/env bash
# The parts of the OVH L5 lane that need no OVH, no Tailscale and no cluster: the helpers
# of lib.sh against fixtures and stubs, the consistency of the scenario list with the
# workflow, and the chart rendered with the lane's values. The manifests are checked
# against an API server by manifests_test.go (go test ./test/e2e-ovh/). Run through
# `make test-e2e-ovh-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-ovh/lib.sh
source "${here}/lib.sh"

failures=0
check() { # check <name> <command...>
  local name="$1"
  shift
  if ("$@") >"${tmp}/last" 2>&1; then
    echo "ok   ${name}"
  else
    echo "FAIL ${name}"
    sed 's/^/     /' "${tmp}/last"
    failures=$((failures + 1))
  fi
}
check_fails() { # check_fails <name> <command...>: the command must fail
  local name="$1"
  shift
  if ("$@") >"${tmp}/last" 2>&1; then
    echo "FAIL ${name} (it succeeded)"
    failures=$((failures + 1))
  else
    echo "ok   ${name}"
  fi
}

# ---- render ------------------------------------------------------------------
export E2E_CLUSTER=scarpenter-ci-1 E2E_RUN_ID=1 E2E_CREATED_AT=1800000000 E2E_API_HOST=100.64.0.1
export E2E_BYO_SG=4be2a1c0-7f3d-4e9b-8a16-5c2d9e0f1b34
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=ovh.d2-2 CAPACITY=on-demand render "${here}/manifests/probe.yaml")"
  grep -q 'name: probe1' <<<"${out}" && grep -q 'karpenter.sh/nodepool: pool1' <<<"${out}" && grep -q 'capacity-type: on-demand' <<<"${out}" &&
    ! grep -qF -e "\${" <<<"${out}"
}
t_render_unset() { (unset NAME POOL TYPE CAPACITY; render "${here}/manifests/probe.yaml"); }
t_render_keeps_other_dollars() { # only the manifest's own ${NAME} are replaced
  # shellcheck disable=SC2016  # the dollar signs are the test's input
  printf 'a: ${NAME}\nb: $HOME\n' >"${tmp}/m.yaml"
  [[ "$(NAME=x render "${tmp}/m.yaml")" == $'a: x\nb: $HOME' ]]
}
check "render fills every variable" t_render
check_fails "render refuses an unset variable" t_render_unset
check "render leaves other dollar signs alone" t_render_keeps_other_dollars
for f in nodeclass-amd64 nodeclass-drift nodeclass-use nodeclass-negative nodeclass-negative-override nodepool-use nodepools db described chart-values; do
  check "render ${f}" render "${here}/manifests/${f}.yaml"
done

# ---- numbers and metrics -----------------------------------------------------
cat >"${tmp}/metrics.txt" <<'METRICS'
# HELP scarpenter_quota_limit Quota limit of a bucket.
scarpenter_quota_limit{bucket="cores",region="GRA11",vendor="ovh"} 64
scarpenter_quota_limit{bucket="instances",region="GRA11",vendor="ovh"} 20
scarpenter_quota_limit{bucket="cores",region="SBG5",vendor="ovh"} 8
scarpenter_quota_used{bucket="cores",region="GRA11",vendor="ovh"} 4
scarpenter_quota_reserved{bucket="cores",region="GRA11",vendor="ovh"} 0
scarpenter_quota_used_total{bucket="cores",region="GRA11",vendor="ovh"} 99
METRICS
t_metric() {
  [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit GRA11 cores)" == 64 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit GRA11 instances)" == 20 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit SBG5 cores)" == 8 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_used GRA11 cores)" == 4 ]] &&
    [[ -z "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit GRA11 ram)" ]]
}
t_num() { [[ "$(num 5)" == 5 && "$(num 5.0)" == 5 && "$(num 32e+00)" == 32 && "$(num 0)" == 0 ]]; }
check "metric_value finds the series of a region and bucket (and not a longer metric name)" t_metric
check "num reads Prometheus and OpenStack numbers" t_num

# ---- waiting -----------------------------------------------------------------
t_wait_ok() { wait_until "true" 5 true >/dev/null; }
t_wait_timeout() { grep -q 'timed out after 4s waiting for never' <<<"$( (wait_until never 4 false) 2>&1 || true)"; }
t_wait_lazy() { # the command is run at every poll
  local f="${tmp}/count"
  echo 0 >"${f}"
  # shellcheck disable=SC2329  # called by wait_until
  bump() { echo $(($(cat "${f}") + 1)) >"${f}"; [[ "$(cat "${f}")" -ge 2 ]]; }
  wait_until "second poll" 20 bump >/dev/null
}
t_is_value() { is_value 7 echo 7 && ! is_value 7 echo 8 && is_empty true && ! is_empty echo x && ! is_empty false; }
check "wait_until returns when the command succeeds" t_wait_ok
check "wait_until times out with the reason" t_wait_timeout
check "wait_until polls the command again" t_wait_lazy
check "is_value and is_empty" t_is_value
check_fails "fail ends the scenario's subshell" bash -c "source '${here}/lib.sh'; fail boom"

# ---- stubs: the OpenStack CLI the helpers call ------------------------------
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/openstack" <<'STUB'
#!/usr/bin/env bash
# a stub of the OpenStack CLI for selftest.sh
case "$*" in
  *"server list"*) echo '[{"ID":"s-aaa","Status":"ACTIVE"},{"ID":"s-bbb","Status":"BUILD"},{"ID":"s-ccc","Status":"SHUTOFF"}]' ;;
  *"limits show"*) echo '[{"Name":"maxTotalCores","Value":64},{"Name":"totalCoresUsed","Value":6}]' ;;
  *"server show"*) echo '{"properties":{"scarpenter-cluster":"scarpenter-ci-1"},"flavor":{"original_name":"b3-8"}}' ;;
  *) echo "unexpected openstack call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/openstack"
t_cluster_instances() {
  [[ "$(PATH="${tmp}/bin:${PATH}" cluster_instances | paste -sd,)" == "s-aaa,s-bbb,s-ccc" ]] &&
    [[ "$(PATH="${tmp}/bin:${PATH}" cluster_instances 'ACTIVE|BUILD' | paste -sd,)" == "s-aaa,s-bbb" ]]
}
t_limit_and_meta() {
  [[ "$(PATH="${tmp}/bin:${PATH}" limit_of maxTotalCores)" == 64 && "$(PATH="${tmp}/bin:${PATH}" limit_of totalCoresUsed)" == 6 ]] &&
    [[ "$(meta_of '{"properties":{"scarpenter-x":"y"}}' scarpenter-x)" == y ]] &&
    [[ "$(flavor_of '{"flavor":{"original_name":"b3-8"}}')" == b3-8 && "$(flavor_of '{"flavor":"d2-2 (abc123)"}')" == d2-2 ]]
}
t_cluster_tag() { [[ "$(cluster_tag)" == "scarpenter-cluster=scarpenter-ci-1" ]]; }
check "cluster_instances lists the IDs of the cluster's servers, filtered by status" t_cluster_instances
check "limit_of, meta_of and flavor_of read the CLI's JSON" t_limit_and_meta
check "cluster_tag is the server tag of the driver" t_cluster_tag

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop" boot="abcdef"".0123456789abcdef"
  local fernet="gAAAAA""BhZ3J1Ym5vdGFyZWFsdG9rZW5ub3RhcmVhbHRva2Vu" projid="0123456789abcdef""0123456789abcdef"
  local in out leak
  in="key ${tskey}; node host1.tail1234.ts.net; project ${projid}
Authorization: Bearer abc.def-ghi_jkl
authorization=Basic dXNlcjpwYXNz
header \"Authorization\": \"Bearer secret-token-value\"
curl -H 'Bearer zzz999' x
OS_PASSWORD=hunter2hunter2 OS_USERNAME=user-abc OS_PROJECT_ID=${projid}
X-Subject-Token: ${fernet}
\"password\": \"hunter3hunter3\"
bare ${fernet}
token ${boot} here
client-key-data: LS0tLS1CRUdJTg==
mail user@example.com"
  out="$(redact <<<"${in}")"
  for leak in "${tskey}" "tail1234" "${projid}" "abc.def-ghi_jkl" "dXNlcjpwYXNz" "secret-token-value" \
    "zzz999" "hunter2hunter2" "user-abc" "${fernet}" "hunter3hunter3" "${boot}" "LS0tLS1CRUdJTg" "user@example.com"; do
    if grep -qF -e "${leak}" <<<"${out}"; then
      echo "not redacted: ${leak}"
      echo "${out}"
      return 1
    fi
  done
  grep -q '<os-id>' <<<"${out}" && grep -q '<os-token>' <<<"${out}" && grep -q '<bootstrap-token>' <<<"${out}" &&
    grep -q '<tskey>' <<<"${out}"
}
t_redact_keeps_text() { [[ "$(redact <<<'the node scarpenter-ci-1 is Ready after 42s')" == 'the node scarpenter-ci-1 is Ready after 42s' ]]; }
check "redact removes OpenStack IDs, passwords and tokens, auth keys, Authorization and Bearer, bootstrap tokens" t_redact
check "redact leaves ordinary text alone" t_redact_keeps_text

# ---- waits for nodes, progress and snapshots, against a stub kubectl ----------
cat >"${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# a stub of kubectl for selftest.sh: one Pending pod of pool1, the claims of $STUB_CLAIMS
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$*" in
  *"get pods"*) echo '{"items":[{"metadata":{"name":"p1","namespace":"default","creationTimestamp":"'"${now}"'"},"spec":{"nodeSelector":{"karpenter.sh/nodepool":"pool1"}},"status":{"phase":"Pending"}}]}' ;;
  *"get nodeclaims"*) echo "${STUB_CLAIMS}" ;;
  *"get events"*) echo '{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes, key hunter4hunter4","involvedObject":{"kind":"Pod","name":"p1","namespace":"default"},"lastTimestamp":"'"${now}"'","metadata":{"creationTimestamp":"'"${now}"'"}}]}' ;;
  *"get nodepools"* | *"get scnodeclasses"*) echo '{"items":[]}' ;;
  *" logs "*) printf 'level=info all fine\nlevel=error msg="create failed" token=abcdef.0123456789abcdef\n' ;;
  *"describe pod"*) printf 'Name: p1\n    Environment:\n      SECRET_VALUE: hunter2\n    Mounts:\n' ;;
  *) ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"openstack:///GRA11/s-aaa","conditions":[{"type":"Launched","status":"True"}]}}]}'
claim_pending='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"conditions":[{"type":"Launched","status":"Unknown","reason":"Launching"}]}}]}'
# wait_node_stub <claims json> <timeout> <claim to ignore>: the output of a wait that never succeeds
wait_node_stub() {
  PATH="${tmp}/bin:${PATH}" STUB_CLAIMS="$1" E2E_LAUNCH_TIMEOUT=1 E2E_PROGRESS_INTERVAL=1000 \
    bash -c "source '${here}/lib.sh'; wait_node 'the pod' $2 app=x '$3' false" 2>&1
}
t_wait_never_launched() {
  local out
  out="$(wait_node_stub "${claim_pending}" 20 "")" && return 1
  grep -q 'never launched' <<<"${out}" && ! grep -q 'launched but not ready' <<<"${out}"
}
t_wait_launched_not_ready() {
  local out
  out="$(wait_node_stub "${claim_launched}" 5 "")" && return 1
  grep -q 'launched but not ready' <<<"${out}" && ! grep -q 'never launched' <<<"${out}"
}
t_wait_ignores_claim() { # the only claim is the ignored one: nothing is launched for the wait
  local out
  out="$(wait_node_stub "${claim_launched}" 20 c1)" && return 1
  grep -q 'never launched' <<<"${out}"
}
t_progress_redacted() {
  local out
  out="$(PATH="${tmp}/bin:${PATH}" STUB_CLAIMS="${claim_pending}" progress_block app=x 30 2>&1)"
  grep -q '^::group::' <<<"${out}" && grep -q '^::endgroup::' <<<"${out}" && grep -q 'create failed' <<<"${out}" &&
    grep -q 'FailedScheduling' <<<"${out}" && grep -q 'Launched=Unknown/Launching' <<<"${out}" &&
    ! grep -qE 'abcdef\.0123456789abcdef' <<<"${out}"
}
t_snapshot_no_env() {
  local out
  out="$(PATH="${tmp}/bin:${PATH}" STUB_CLAIMS="${claim_pending}" failure_snapshot amd64 app=x 2>&1)"
  grep -q 'kubectl describe pod default/p1' <<<"${out}" && ! grep -q 'hunter2' <<<"${out}" && grep -q '^::endgroup::' <<<"${out}"
}
check "wait_node: no launched NodeClaim fails as never launched" t_wait_never_launched
check "wait_node: a launched NodeClaim that never gets the node fails as launched but not ready" t_wait_launched_not_ready
check "wait_node: an ignored (older) NodeClaim does not count as launched" t_wait_ignores_claim
check "progress_block prints a collapsible, redacted block" t_progress_redacted
check "failure_snapshot describes the pending Pods without their environment" t_snapshot_no_env

# ---- the runner and the workflow ---------------------------------------------
t_scenarios_defined() {
  local names n
  names="$(sed -n 's/^all_scenarios=(\(.*\))$/\1/p' "${here}/scenarios.sh")"
  [[ -n "${names}" ]]
  for n in ${names} preflight; do
    grep -q "^scn_${n}() {" "${here}/scenarios.sh" || {
      echo "no function scn_${n}"
      return 1
    }
    grep -q "${n}" "${root}/.github/workflows/e2e.yml" || [[ "${n}" == preflight ]] || {
      echo "the workflow's scenarios input does not name ${n}"
      return 1
    }
  done
}
t_pins_pinned() { # every action of the L5 workflows is pinned by a full commit SHA (a local reusable workflow has no pin)
  local w="${root}/.github/workflows"
  ! grep -hE '^\s*-?\s*uses:' "${w}/e2e.yml" "${w}/e2e-cloud.yml" "${w}/e2e-canary.yml" "${w}/e2e-ovh-sweeper.yml" "${w}/release.yml" |
    grep -vE 'uses: \./' | grep -vE 'uses: [^@ ]+@[0-9a-f]{40} # v[0-9]'
}
t_smoke_scenarios() { # the smoke set names known scenarios, and not the implicit preflight
  local names s
  names="$(sed -n 's/^all_scenarios=(\(.*\))$/\1/p' "${here}/scenarios.sh")"
  [[ -n "${E2E_SMOKE_SCENARIOS}" ]]
  for s in ${E2E_SMOKE_SCENARIOS//,/ }; do
    [[ " ${names} " == *" ${s} "* ]] || {
      echo "the smoke scenario ${s} is not one of: ${names}"
      return 1
    }
  done
}
t_workflows_agree() { # the callers pass the reusable workflow its inputs; nothing names the old file
  local w="${root}/.github/workflows" i
  for i in vendor mode scenarios fail_fast; do
    grep -qE "^      ${i}:" "${w}/e2e-cloud.yml" || { echo "e2e-cloud.yml has no input ${i}"; return 1; }
    grep -qE "^      ${i}: " "${w}/e2e.yml" || { echo "e2e.yml does not pass ${i}"; return 1; }
    grep -qE "^      ${i}: " "${w}/release.yml" || { echo "release.yml does not pass ${i}"; return 1; }
  done
  [[ ! -e "${w}/e2e-aws.yml" ]] || { echo "e2e-aws.yml still exists"; return 1; }
  grep -qE '^    environment: e2e-sweeper$' "${w}/e2e-aws-sweeper.yml" || { echo "the AWS sweeper is not in the environment e2e-sweeper"; return 1; }
  # A labeled run would put a newer green e2e-result over a failed one (testing-and-releasing.md).
  ! grep -qE '^    types: .*labeled' "${w}/e2e.yml" || { echo "e2e.yml runs on labeled"; return 1; }
  # The sweeper has no reviewers to wait for: its own environment, limited to main.
  grep -qE '^    environment: e2e-sweeper$' "${w}/e2e-ovh-sweeper.yml" || { echo "the sweeper is not in the environment e2e-sweeper"; return 1; }
  ! grep -rn 'e2e-aws\.yml' "${root}/.github" "${root}/test" "${root}/hack" "${root}/Makefile" "${root}/docs/how-to-guides" |
    grep -v 'selftest.sh'
}
t_no_secrets_in_scripts() { # nothing that looks like an ID, a key or a local path
  ! grep -rnE '(tskey-(auth|client)-[A-Za-z0-9]{6,}|AKIA[A-Z0-9]{16}|[0-9a-f]{32}|/home/[a-z]+|/tmp/)' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' | grep -v 'selftest.sh'
}
check "every scenario has a function and is named by the workflow" t_scenarios_defined
check "every action in the L5 workflows is pinned by SHA with a version comment" t_pins_pinned
check "the smoke scenarios are scenarios of the runner" t_smoke_scenarios
check "e2e.yml and release.yml pass what e2e-cloud.yml takes; no labeled trigger; the sweeper's environment; nothing names the old e2e-aws.yml" t_workflows_agree
check "no account ID, key or local path in the lane's files" t_no_secrets_in_scripts

# ---- the lane's identity -----------------------------------------------------
t_no_policy_scripts() { # OVH has no IAM condition language to print: the roles are in the docs
  [[ ! -e "${here}/ci-policy.sh" && ! -e "${here}/canary-policy.sh" ]] && grep -q 'infrastructureSupervisor\|Infrastructure Supervisor' "${root}/docs/how-to-guides/testing.md"
}
check "the canary's read-only role is documented (there is no policy script)" t_no_policy_scripts

# ---- the chart with the lane's values ----------------------------------------
t_chart() {
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep -q -- '--cluster-name=scarpenter-ci-1' "${tmp}/chart.yaml" &&
    grep -q 'ovh-openstack' "${tmp}/chart.yaml" && grep -q -- '--vendors=ovh' "${tmp}/chart.yaml" && grep -q 'https://100.64.0.1:6443' "${tmp}/chart.yaml" &&
    grep -q 'CriticalAddonsOnly' "${tmp}/chart.yaml"
}
check "the chart renders with the lane's values (schema, credentials, endpoint, toleration)" t_chart

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
