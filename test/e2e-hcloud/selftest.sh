#!/usr/bin/env bash
# The parts of the L5 lane that need no Hetzner, no Tailscale and no cluster: the helpers of
# lib.sh against fixtures and stubs, the consistency of the scenario list with the
# workflow, and the chart rendered with the lane's values. The manifests are checked
# against an API server by manifests_test.go (go test ./test/e2e-hcloud/). Run through
# `make test-e2e-hcloud-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-hcloud/lib.sh
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
export E2E_PG_LOCATION=fsn1 E2E_HCLOUD_SERVER_LIMIT=10
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=hcloud.cx23 CAPACITY=on-demand render "${here}/manifests/probe.yaml")"
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
for f in nodeclass-amd64 nodeclass-arm64 nodeclass-drift nodeclass-negative nodeclass-use nodepools nodepool-use db described; do
  check "render ${f}" render "${here}/manifests/${f}.yaml"
done

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

# ---- stubs: the hcloud CLI the helpers call ---------------------------------
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/hcloud" <<'STUB'
#!/usr/bin/env bash
# a stub of the hcloud CLI for selftest.sh
case "$*" in
  "server list -l scarpenter.sparecores.com/cluster=scarpenter-ci-1 -o noheader -o columns=id") printf '101\n102\n\n' ;;
  "volume list -l scarpenter.sparecores.com/cluster=scarpenter-ci-1 -o noheader -o columns=id") ;;
  "server describe 101 -o json") echo '{"location":{"name":"fsn1"},"status":"running"}' ;;
  *) echo "unexpected hcloud call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/hcloud"
t_cluster_servers() { [[ "$(PATH="${tmp}/bin:${PATH}" cluster_servers | paste -sd,)" == "101,102" ]]; }
t_cluster_leaks_empty() { [[ -z "$(PATH="${tmp}/bin:${PATH}" cluster_leaks volume)" ]]; }
t_location_of() { [[ "$(location_of "$(PATH="${tmp}/bin:${PATH}" server_json 101)")" == fsn1 ]]; }
check "cluster_servers lists IDs one per line" t_cluster_servers
check "cluster_leaks is empty when nothing is labelled" t_cluster_leaks_empty
check "server_json and location_of read the location" t_location_of

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local akia="AKIA""ABCDEFGHIJKLMNOP" asia="ASIA""ABCDEFGHIJKLMNOP" tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop"
  local sts="IQoJb3JpZ2lu""X2VjEKz//////////wEaCXVzLWVhc3QtMiJHMEUCIQD" boot="abcdef"".0123456789abcdef"
  local hctok="aB3dE5gH7jK9mN1pQ3sT5vW7yZ9bC1dE3fG5hJ7kL9nP1qR3sT5uV7wX9yZ1aB3x"
  local in out leak
  in="key ${akia} and ${asia}; ${tskey}; node host1.tail1234.ts.net; account 123456789012 arn:aws:iam::123456789012:role/x
Authorization: Bearer abc.def-ghi_jkl
authorization=Basic dXNlcjpwYXNz
header \"Authorization\": \"Bearer secret-token-value\"
curl -H 'Bearer zzz999' x
AWS_SESSION_TOKEN=${sts} AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCY
\"SessionToken\": \"${sts}\", \"SecretAccessKey\": \"wJalrXUtnFEMI/K7MDENG\"
bare ${sts}
token ${boot} here
client-key-data: LS0tLS1CRUdJTg==
mail user@example.com
HCLOUD_TOKEN=${hctok}
token in a sentence ${hctok} end"
  out="$(redact <<<"${in}")"
  for leak in "${akia}" "${asia}" "${tskey}" "tail1234" "123456789012" "abc.def-ghi_jkl" "dXNlcjpwYXNz" "secret-token-value" \
    "zzz999" "${sts}" "wJalrXUtnFEMI" "${boot}" "LS0tLS1CRUdJTg" "user@example.com" "${hctok}"; do
    if grep -qF -e "${leak}" <<<"${out}"; then
      echo "not redacted: ${leak}"
      echo "${out}"
      return 1
    fi
  done
  grep -q '<aws-key-id>' <<<"${out}" && grep -q '<account>' <<<"${out}" && grep -q '<bootstrap-token>' <<<"${out}" &&
    grep -q '<tskey>' <<<"${out}" && grep -q '<aws-session-token>' <<<"${out}"
}
t_redact_keeps_text() { [[ "$(redact <<<'the node scarpenter-ci-1 is Ready after 42s')" == 'the node scarpenter-ci-1 is Ready after 42s' ]]; }
check "redact removes account IDs, keys, session tokens, Authorization and Bearer, bootstrap tokens" t_redact
check "redact leaves ordinary text alone" t_redact_keeps_text

# ---- waits for nodes, progress and snapshots, against a stub kubectl ----------
cat >"${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# a stub of kubectl for selftest.sh: one Pending pod of pool1, the claims of $STUB_CLAIMS
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$*" in
  *"get pods"*) echo '{"items":[{"metadata":{"name":"p1","namespace":"default","creationTimestamp":"'"${now}"'"},"spec":{"nodeSelector":{"karpenter.sh/nodepool":"pool1"}},"status":{"phase":"Pending"}}]}' ;;
  *"get nodeclaims"*) echo "${STUB_CLAIMS}" ;;
  *"get events"*) echo '{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes, key AKIAABCDEFGHIJKLMNOP","involvedObject":{"kind":"Pod","name":"p1","namespace":"default"},"lastTimestamp":"'"${now}"'","metadata":{"creationTimestamp":"'"${now}"'"}}]}' ;;
  *"get nodepools"* | *"get scnodeclasses"*) echo '{"items":[]}' ;;
  *" logs "*) printf 'level=info all fine\nlevel=error msg="create failed" token=abcdef.0123456789abcdef\n' ;;
  *"describe pod"*) printf 'Name: p1\n    Environment:\n      SECRET_VALUE: hunter2\n    Mounts:\n' ;;
  *) ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"scarpenter://hcloud/fsn1/c1","conditions":[{"type":"Launched","status":"True"}]}}]}'
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
    ! grep -qE 'AKIA[A-Z0-9]{16}|abcdef\.0123456789abcdef' <<<"${out}"
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
  done
}
t_pins_pinned() { # every action of the L5 workflows is pinned by a full commit SHA (a local reusable workflow has no pin)
  local w="${root}/.github/workflows"
  ! grep -hE '^\s*-?\s*uses:' "${w}/e2e.yml" "${w}/e2e-cloud.yml" "${w}/e2e-canary.yml" "${w}/e2e-aws-sweeper.yml" "${w}/e2e-hcloud-sweeper.yml" "${w}/release.yml" |
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
  # A labeled run would put a newer green e2e-result over a failed one (testing-and-releasing.md).
  ! grep -qE '^    types: .*labeled' "${w}/e2e.yml" || { echo "e2e.yml runs on labeled"; return 1; }
  # The sweeper has no reviewers to wait for: its own environment, limited to main.
  grep -qE '^    environment: e2e-sweeper$' "${w}/e2e-hcloud-sweeper.yml" || { echo "the sweeper is not in the environment e2e-sweeper"; return 1; }
  grep -qxF hcloud "${root}/test/e2e-vendors.txt" || { echo "hcloud is not in test/e2e-vendors.txt"; return 1; }
  grep -q 'inputs.vendor == .hcloud.' "${w}/e2e-cloud.yml" || { echo "e2e-cloud.yml has no hcloud credentials step"; return 1; }
  grep -q 'matrix.vendor == .hcloud.' "${w}/e2e-canary.yml" || { echo "e2e-canary.yml has no hcloud credentials step"; return 1; }
}
t_no_secrets_in_scripts() { # nothing that looks like an account ID, a key or a local path
  ! grep -rnE '(arn:aws:[a-z0-9-]*::[0-9]{12}|tskey-(auth|client)-[A-Za-z0-9]{6,}|AKIA[A-Z0-9]{16}|/home/[a-z]+|/tmp/)' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' | grep -v 'selftest.sh'
}
check "every scenario has a function and is named by the workflow" t_scenarios_defined
check "every action in the L5 workflows is pinned by SHA with a version comment" t_pins_pinned
check "the smoke scenarios are scenarios of the runner" t_smoke_scenarios
check "e2e.yml and release.yml pass what e2e-cloud.yml takes; no labeled trigger; the sweeper's environment; nothing names the old e2e-aws.yml" t_workflows_agree
check "no account ID, key or local path in the lane's files" t_no_secrets_in_scripts

# ---- the chart with the lane's values ----------------------------------------
t_chart() {
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep -q -- '--cluster-name=scarpenter-ci-1' "${tmp}/chart.yaml" && grep -q -- '--vendors=hcloud' "${tmp}/chart.yaml" &&
    grep -q -- '--hcloud-server-limit=10' "${tmp}/chart.yaml" &&
    grep -q 'HCLOUD_TOKEN' "${tmp}/chart.yaml" && grep -q 'hcloud-credentials' "${tmp}/chart.yaml" &&
    grep -q 'https://100.64.0.1:6443' "${tmp}/chart.yaml" && grep -q 'CriticalAddonsOnly' "${tmp}/chart.yaml"
}
check "the chart renders with the lane's values (schema, token Secret, server limit, endpoint, toleration)" t_chart

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
