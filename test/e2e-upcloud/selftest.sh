#!/usr/bin/env bash
# The parts of the L5 lane that need no UpCloud, no Tailscale and no cluster: the helpers of
# lib.sh against fixtures and stubs, the consistency of the scenario list with the
# workflow, and the chart rendered with the lane's values. The manifests are checked
# against an API server by manifests_test.go (go test ./test/e2e-upcloud/). Run through
# `make test-e2e-upcloud-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-upcloud/lib.sh
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
export UPCLOUD_TOKEN=ucat_selftest_token_0123456789abcdef
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=upcloud.STARTER-1xCPU-1GB CAPACITY=spot render "${here}/manifests/probe.yaml")"
  grep -q 'name: probe1' <<<"${out}" && grep -q 'karpenter.sh/nodepool: pool1' <<<"${out}" && grep -q 'capacity-type: spot' <<<"${out}" &&
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
for f in nodeclass-amd64 nodeclass-drift nodeclass-negative nodeclass-use nodepools nodepool-use servergroup data described; do
  check "render ${f}" render "${here}/manifests/${f}.yaml"
done

# ---- numbers and metrics -----------------------------------------------------
cat >"${tmp}/metrics.txt" <<'METRICS'
# HELP scarpenter_quota_limit Quota limit of a bucket.
scarpenter_quota_limit{bucket="cores",region="",scope="global",vendor="upcloud"} 12
scarpenter_quota_limit{bucket="memory",region="",scope="global",vendor="upcloud"} 16384
scarpenter_quota_used{bucket="cores",region="",scope="global",vendor="upcloud"} 4
scarpenter_quota_reserved{bucket="cores",region="",scope="global",vendor="upcloud"} 0
scarpenter_quota_used_total{bucket="cores",region="",scope="global",vendor="upcloud"} 99
METRICS
t_metric() {
  [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit cores)" == 12 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit memory)" == 16384 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_used cores)" == 4 ]] &&
    [[ -z "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit storage-maxiops)" ]]
}
t_num() { [[ "$(num 5)" == 5 && "$(num 5.0)" == 5 && "$(num 32e+00)" == 32 && "$(num 0)" == 0 ]]; }
check "metric_value finds the series of a bucket (and not a longer metric name)" t_metric
check "num reads Prometheus numbers" t_num

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

# ---- stubs: the UpCloud API behind curl --------------------------------------
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/curl" <<'STUB'
#!/usr/bin/env bash
# a stub of curl for selftest.sh: answers the paths the helpers read, like the API does. The
# bearer token reaches curl in a config file (-K), never as an argument: the stub fails if it
# is on the command line.
case "$*" in *ucat_*) echo "the token is on the command line" >&2; exit 9 ;; esac
url="${*: -1}"
case "${url}" in
  *"/server?label="*) echo '{"servers":{"server":[{"uuid":"s1","state":"started"},{"uuid":"s2","state":"stopped"}]}}' ;;
  *"/server/s1"*) echo '{"server":{"uuid":"s1","labels":{"label":[{"key":"scarpenter-cluster","value":"scarpenter-ci-1"},{"key":"x","value":"y"}]}}}' ;;
  *"/storage/private?label="*) echo '{"storages":{"storage":[{"uuid":"d1","type":"disk"},{"uuid":"t1","type":"template"}]}}' ;;
  *"/network?label="*) echo '{"networks":{"network":[{"uuid":"n1","type":"private"},{"uuid":"n2","type":"public"}]}}' ;;
  *"/server-group?label="*) echo '[{"uuid":"g1"}]' ;;
  *"/firewall-ruleset") echo '[{"uuid":"r1","name":"scarpenter-scarpenter-ci-1-firewalls-fi","labels":[]},{"uuid":"r2","name":"other"},{"uuid":"r3","name":"x","labels":[{"key":"scarpenter-cluster","value":"scarpenter-ci-1"}]}]' ;;
  *"/server/gone") [[ "$*" == *"%{http_code}"* ]] && printf 404 ;;
  *"/server/here") [[ "$*" == *"%{http_code}"* ]] && printf 200 ;;
  *"/server/busy") [[ "$*" == *"%{http_code}"* ]] && printf 429 ;;
  *) echo "unexpected curl call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/curl"
export E2E_CLUSTER=scarpenter-ci-1
t_cluster_servers() { [[ "$(PATH="${tmp}/bin:${PATH}" cluster_servers | paste -sd,)" == "s1,s2" ]]; }
t_cluster_servers_state() { [[ "$(PATH="${tmp}/bin:${PATH}" cluster_servers started | paste -sd,)" == "s1" ]]; }
t_label_of() { [[ "$(PATH="${tmp}/bin:${PATH}" label_of "$(PATH="${tmp}/bin:${PATH}" server_json s1)" scarpenter-cluster)" == scarpenter-ci-1 ]]; }
t_lists() {
  [[ "$(PATH="${tmp}/bin:${PATH}" cluster_storages)" == d1 && "$(PATH="${tmp}/bin:${PATH}" cluster_networks)" == n1 &&
    "$(PATH="${tmp}/bin:${PATH}" cluster_groups)" == g1 && "$(PATH="${tmp}/bin:${PATH}" cluster_rulesets | paste -sd,)" == "r1,r3" ]]
}
t_up_exists() {
  local out
  [[ "$(PATH="${tmp}/bin:${PATH}" servers_existing here)" == "/server/here" && -z "$(PATH="${tmp}/bin:${PATH}" servers_existing gone)" ]] &&
    ! out="$(PATH="${tmp}/bin:${PATH}" servers_existing busy 2>&1)" && grep -q 'unexpected HTTP 429' <<<"${out}"
}
check "cluster_servers lists UUIDs one per line, by state too" t_cluster_servers
check "cluster_servers filters by state" t_cluster_servers_state
check "label_of reads a label of a server" t_label_of
check "the cluster's disks (no templates), networks (no public), groups and rulesets" t_lists
check "servers_existing: 404 is gone, any other error is an error" t_up_exists

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local tok="ucat_""0123456789abcdefABCDEF" tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop" boot="abcdef"".0123456789abcdef"
  local in out leak
  in="key ${tok}; ${tskey}; node host1.tail1234.ts.net
Authorization: Bearer abc.def-ghi_jkl
authorization=Basic dXNlcjpwYXNz
header \"Authorization\": \"Bearer secret-token-value\"
curl -H 'Bearer zzz999' x
UPCLOUD_TOKEN=${tok}
\"password\": \"hunter2hunter2\"
token ${boot} here
client-key-data: LS0tLS1CRUdJTg==
mail user@example.com"
  out="$(redact <<<"${in}")"
  for leak in "${tok}" "${tskey}" "tail1234" "abc.def-ghi_jkl" "dXNlcjpwYXNz" "secret-token-value" \
    "zzz999" "hunter2hunter2" "${boot}" "LS0tLS1CRUdJTg" "user@example.com"; do
    if grep -qF -e "${leak}" <<<"${out}"; then
      echo "not redacted: ${leak}"
      echo "${out}"
      return 1
    fi
  done
  grep -q '<upcloud-token>' <<<"${out}" && grep -q '<bootstrap-token>' <<<"${out}" && grep -q '<tskey>' <<<"${out}"
}
t_redact_keeps_text() { [[ "$(redact <<<'the node scarpenter-ci-1 is Ready after 42s')" == 'the node scarpenter-ci-1 is Ready after 42s' ]]; }
check "redact removes UpCloud tokens, Tailscale keys, Authorization and Bearer, passwords, bootstrap tokens" t_redact
check "redact leaves ordinary text alone" t_redact_keeps_text

# ---- waits for nodes, progress and snapshots, against a stub kubectl ----------
cat >"${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# a stub of kubectl for selftest.sh: one Pending pod of pool1, the claims of $STUB_CLAIMS
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$*" in
  *"get pods"*) echo '{"items":[{"metadata":{"name":"p1","namespace":"default","creationTimestamp":"'"${now}"'"},"spec":{"nodeSelector":{"karpenter.sh/nodepool":"pool1"}},"status":{"phase":"Pending"}}]}' ;;
  *"get nodeclaims"*) echo "${STUB_CLAIMS}" ;;
  *"get events"*) echo '{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes, key tskey-auth-kABCDEF123456CNTRL-abcdefghijklmnop","involvedObject":{"kind":"Pod","name":"p1","namespace":"default"},"lastTimestamp":"'"${now}"'","metadata":{"creationTimestamp":"'"${now}"'"}}]}' ;;
  *"get nodepools"* | *"get scnodeclasses"*) echo '{"items":[]}' ;;
  *" logs "*) printf 'level=info all fine\nlevel=error msg="create failed" token=abcdef.0123456789abcdef\n' ;;
  *"describe pod"*) printf 'Name: p1\n    Environment:\n      SECRET_VALUE: hunter2\n    Mounts:\n' ;;
  *) ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"scarpenter://upcloud/fi-hel1/claim1","conditions":[{"type":"Launched","status":"True"}]}}]}'
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
    ! grep -qE 'tskey-auth-[A-Za-z0-9]{6,}|abcdef\.0123456789abcdef' <<<"${out}"
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
  ! grep -hE '^\s*-?\s*uses:' "${w}/e2e.yml" "${w}/e2e-cloud.yml" "${w}/e2e-canary.yml" "${w}/e2e-upcloud-sweeper.yml" "${w}/release.yml" |
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
t_workflows_agree() { # the lane's pieces name each other consistently
  local w="${root}/.github/workflows"
  # The sweeper has no reviewers to wait for: its own environment, limited to main.
  grep -qE '^    environment: e2e-sweeper$' "${w}/e2e-upcloud-sweeper.yml" || { echo "the sweeper is not in the environment e2e-sweeper"; return 1; }
  grep -q "inputs.vendor == 'upcloud'" "${w}/e2e-cloud.yml" || { echo "e2e-cloud.yml has no UpCloud credentials step"; return 1; }
  grep -q "matrix.vendor == 'upcloud'" "${w}/e2e-canary.yml" || { echo "e2e-canary.yml has no UpCloud credentials step"; return 1; }
  grep -qxF upcloud "${root}/test/e2e-vendors.txt" || { echo "upcloud is not in test/e2e-vendors.txt"; return 1; }
  # The credential is the secret UPCLOUD_TOKEN and nothing else of UpCloud's is a secret.
  grep -q 'secrets.UPCLOUD_TOKEN' "${w}/e2e-cloud.yml"
}
t_no_secrets_in_scripts() { # nothing that looks like a key, a token or a local path
  ! grep -rnE '(tskey-(auth|client)-[A-Za-z0-9]{6,}|ucat_[A-Za-z0-9]{8,}|/home/[a-z]+|/tmp/)' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' | grep -v 'selftest.sh'
}
check "every scenario has a function" t_scenarios_defined
check "every action in the L5 workflows is pinned by SHA with a version comment" t_pins_pinned
check "the smoke scenarios are scenarios of the runner" t_smoke_scenarios
check "the sweeper's environment, the credentials steps and the vendor list name UpCloud" t_workflows_agree
check "no key, token or local path in the lane's files" t_no_secrets_in_scripts

# ---- the chart with the lane's values ----------------------------------------
t_chart() {
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep -q -- '--cluster-name=scarpenter-ci-1' "${tmp}/chart.yaml" &&
    grep -q 'UPCLOUD_TOKEN' "${tmp}/chart.yaml" && grep -q 'https://100.64.0.1:6443' "${tmp}/chart.yaml" &&
    grep -q 'CriticalAddonsOnly' "${tmp}/chart.yaml"
}
check "the chart renders with the lane's values (schema, credentials, endpoint, toleration)" t_chart

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
