#!/usr/bin/env bash
# The parts of the L5 lane that need no AWS, no Tailscale and no cluster: the helpers of
# lib.sh against fixtures and stubs, the consistency of the scenario list with the
# workflow, and the chart rendered with the lane's values. The manifests are checked
# against an API server by manifests_test.go (go test ./test/e2e-aws/). Run through
# `make test-e2e-aws-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-aws/lib.sh
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
export E2E_BYO_ZONE=us-east-2a E2E_PG_ZONE=us-east-2a
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=aws.t3a.small CAPACITY=spot render "${here}/manifests/probe.yaml")"
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
for f in nodeclass-amd64 nodeclass-arm64 nodeclass-drift nodeclass-negative nodepools; do
  check "render ${f}" render "${here}/manifests/${f}.yaml"
done

# ---- numbers and metrics -----------------------------------------------------
cat >"${tmp}/metrics.txt" <<'METRICS'
# HELP scarpenter_quota_limit Quota limit of a bucket.
scarpenter_quota_limit{bucket="L-1216C47A",region="us-east-2",vendor="aws"} 32
scarpenter_quota_limit{bucket="L-34B43A08",region="us-east-2",vendor="aws"} 8
scarpenter_quota_limit{bucket="L-1216C47A",region="eu-west-1",vendor="aws"} 5
scarpenter_quota_used{bucket="L-1216C47A",region="us-east-2",vendor="aws"} 4
scarpenter_quota_reserved{bucket="L-1216C47A",region="us-east-2",vendor="aws"} 0
scarpenter_quota_used_total{bucket="L-1216C47A",region="us-east-2",vendor="aws"} 99
METRICS
t_metric() {
  [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit us-east-2 L-1216C47A)" == 32 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit us-east-2 L-34B43A08)" == 8 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit eu-west-1 L-1216C47A)" == 5 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_used us-east-2 L-1216C47A)" == 4 ]] &&
    [[ -z "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit us-east-2 L-DB2E81BA)" ]]
}
t_num() { [[ "$(num 5)" == 5 && "$(num 5.0)" == 5 && "$(num 32e+00)" == 32 && "$(num 0)" == 0 ]]; }
check "metric_value finds the series of a region and bucket (and not a longer metric name)" t_metric
check "num reads Prometheus and AWS numbers" t_num

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

# ---- stubs: the AWS CLI the helpers call -------------------------------------
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/aws" <<'STUB'
#!/usr/bin/env bash
# a stub of the AWS CLI for selftest.sh: describe-instances answers with one instance
case "$*" in
  *describe-instances*) printf 'i-0aaa\ti-0bbb\n' ;;
  *) echo "unexpected aws call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/aws"
t_cluster_instances() { [[ "$(PATH="${tmp}/bin:${PATH}" cluster_instances | paste -sd,)" == "i-0aaa,i-0bbb" ]]; }
check "cluster_instances lists IDs one per line" t_cluster_instances

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local akia="AKIA""ABCDEFGHIJKLMNOP" asia="ASIA""ABCDEFGHIJKLMNOP" tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop"
  local sts="IQoJb3JpZ2lu""X2VjEKz//////////wEaCXVzLWVhc3QtMiJHMEUCIQD" boot="abcdef"".0123456789abcdef"
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
mail user@example.com"
  out="$(redact <<<"${in}")"
  for leak in "${akia}" "${asia}" "${tskey}" "tail1234" "123456789012" "abc.def-ghi_jkl" "dXNlcjpwYXNz" "secret-token-value" \
    "zzz999" "${sts}" "wJalrXUtnFEMI" "${boot}" "LS0tLS1CRUdJTg" "user@example.com"; do
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
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"aws:///us-east-2a/i-0aaa","conditions":[{"type":"Launched","status":"True"}]}}]}'
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
    grep -q "${n}" "${root}/.github/workflows/e2e-aws.yml" || [[ "${n}" == preflight ]] || {
      echo "the workflow's scenarios input does not name ${n}"
      return 1
    }
  done
}
t_pins_pinned() { # every action of the L5 workflows is pinned by a full commit SHA
  ! grep -hE '^\s*-?\s*uses:' "${root}/.github/workflows/e2e-aws.yml" "${root}/.github/workflows/e2e-aws-sweeper.yml" |
    grep -vE 'uses: [^@ ]+@[0-9a-f]{40} # v[0-9]'
}
t_no_secrets_in_scripts() { # nothing that looks like an account ID, a key or a local path
  ! grep -rnE '(arn:aws:[a-z0-9-]*::[0-9]{12}|tskey-(auth|client)-[A-Za-z0-9]{6,}|AKIA[A-Z0-9]{16}|/home/[a-z]+|/tmp/)' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' | grep -v 'selftest.sh'
}
check "every scenario has a function and is named by the workflow" t_scenarios_defined
check "every action in the L5 workflows is pinned by SHA with a version comment" t_pins_pinned
check "no account ID, key or local path in the lane's files" t_no_secrets_in_scripts

# ---- the CI role's policy ----------------------------------------------------
t_policy() {
  local p="${tmp}/policy.json"
  "${here}/ci-policy.sh" 123456789012 >"${p}"
  # every cluster tag condition is a wildcard, nothing is left of the placeholders, the
  # lane's own statements are there, and the optional statements are gone
  jq -e '
    ([.Statement[].Condition.StringEquals? // {} | keys[] | select(endswith("scarpenter.sparecores.com/cluster"))] | length) == 0 and
    ([.Statement[].Condition.StringLike? // {} | to_entries[] | select(.key | endswith("scarpenter.sparecores.com/cluster")) | .value] | all(. == "scarpenter-ci-*")) and
    ([.Statement[].Sid] | (index("DenyOtherInstanceTypes") and index("DenyOutsideRegion") and index("LaneReads") and index("LaneBringYourOwn")) != null) and
    ([.Statement[].Sid] | map(select(test("PassInstanceProfile|CustomerKms"))) | length) == 0' "${p}" >/dev/null &&
    ! grep -q 'CLUSTER_NAME\|NODE_ROLE_NAME\|KEY_ID' "${p}" &&
    jq -e '.Statement[] | select(.Sid == "DenyOutsideRegion") | .Effect == "Deny" and (.Action | sort) == ["ec2:*", "servicequotas:*"] and
      .Resource == "*" and .Condition == {StringNotEquals: {"aws:RequestedRegion": ["us-east-2"]}}' "${p}" >/dev/null &&
    jq -e '.Statement[] | select(.Sid == "DenyOtherInstanceTypes") | .Effect == "Deny" and .Action == "ec2:RunInstances"' "${p}" >/dev/null &&
    jq -e '.Statement[] | select(.Sid == "DenyOtherInstanceTypes") | .Condition.StringNotEquals."ec2:InstanceType" | index("t3a.small") != null' "${p}" >/dev/null
}
check "ci-policy.sh prints the role's policy for scarpenter-ci-* with the lane's statements" t_policy
check_fails "ci-policy.sh refuses a malformed account ID" "${here}/ci-policy.sh" 12345

# ---- the chart with the lane's values ----------------------------------------
t_chart() {
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep -q -- '--cluster-name=scarpenter-ci-1' "${tmp}/chart.yaml" &&
    grep -q 'AWS_SESSION_TOKEN' "${tmp}/chart.yaml" && grep -q 'https://100.64.0.1:6443' "${tmp}/chart.yaml" &&
    grep -q 'CriticalAddonsOnly' "${tmp}/chart.yaml"
}
check "the chart renders with the lane's values (schema, credentials, endpoint, toleration)" t_chart

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
