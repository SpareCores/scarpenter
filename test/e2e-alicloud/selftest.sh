#!/usr/bin/env bash
# The parts of the L5 lane of Alibaba Cloud that need no cloud, no Tailscale and no cluster: the
# helpers of lib.sh against fixtures and stubs of `aliyun`, `curl` and `kubectl`, the
# consistency of the scenario list with the docs and the workflows, the two RAM policies, and the
# chart rendered with the lane's values when the chart knows the vendor. The manifests are
# checked against an API server by manifests_test.go (go test ./test/e2e-alicloud/). Run
# through `make test-e2e-alicloud-dryrun`. Nothing in the lane has run against a real
# account: this proves the scripts' own logic, not the cloud facts they rest on.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-alicloud/lib.sh
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
export E2E_BYO_ZONE=ap-southeast-1a E2E_PG_ZONE=ap-southeast-1a
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=alicloud.ecs.e-c1m2.large CAPACITY=spot render "${here}/manifests/probe.yaml")"
  grep -q 'name: probe1' <<<"${out}" && grep -q 'karpenter.sh/nodepool: pool1' <<<"${out}" && grep -q 'capacity-type: spot' <<<"${out}" &&
    ! grep -qF -e "\${" <<<"${out}"
}
t_render_unset() { (
  unset NAME POOL TYPE CAPACITY
  render "${here}/manifests/probe.yaml"
); }
t_render_keeps_other_dollars() { # only the manifest's own ${NAME} are replaced
  # shellcheck disable=SC2016  # the dollar signs are the test's input
  printf 'a: ${NAME}\nb: $HOME\n' >"${tmp}/m.yaml"
  [[ "$(NAME=x render "${tmp}/m.yaml")" == $'a: x\nb: $HOME' ]]
}
check "render fills every variable" t_render
check_fails "render refuses an unset variable" t_render_unset
check "render leaves other dollar signs alone" t_render_keeps_other_dollars
for f in nodeclass-amd64 nodeclass-arm64 nodeclass-drift nodeclass-hardened nodeclass-negative nodepools; do
  check "render ${f}" render "${here}/manifests/${f}.yaml"
done

# ---- numbers and metrics -----------------------------------------------------
cat >"${tmp}/metrics.txt" <<'METRICS'
# HELP scarpenter_quota_limit Quota limit of a bucket.
scarpenter_quota_limit{bucket="postpaid-vcpu",region="ap-southeast-1",vendor="alicloud"} 16
scarpenter_quota_limit{bucket="spot-vcpu",region="ap-southeast-1",vendor="alicloud"} 4
scarpenter_quota_limit{bucket="postpaid-vcpu",region="eu-central-1",vendor="alicloud"} 8
scarpenter_quota_used{bucket="postpaid-vcpu",region="ap-southeast-1",vendor="alicloud"} 4
scarpenter_quota_reserved{bucket="postpaid-vcpu",region="ap-southeast-1",vendor="alicloud"} 0
scarpenter_quota_used_total{bucket="postpaid-vcpu",region="ap-southeast-1",vendor="alicloud"} 99
METRICS
t_metric() {
  [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit ap-southeast-1 postpaid-vcpu)" == 16 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit ap-southeast-1 spot-vcpu)" == 4 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit eu-central-1 postpaid-vcpu)" == 8 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_used ap-southeast-1 postpaid-vcpu)" == 4 ]] &&
    [[ -z "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit ap-southeast-1 other)" ]]
}
t_num() { [[ "$(num 5)" == 5 && "$(num 5.0)" == 5 && "$(num 16e+00)" == 16 && "$(num 0)" == 0 ]]; }
check "metric_value finds the series of a region and bucket (and not a longer metric name)" t_metric
check "num reads Prometheus and CLI numbers" t_num

# ---- waiting -----------------------------------------------------------------
t_wait_ok() { wait_until "true" 5 true >/dev/null; }
t_wait_timeout() { grep -q 'timed out after 4s waiting for never' <<<"$( (wait_until never 4 false) 2>&1 || true)"; }
t_wait_lazy() { # the command is run at every poll
  local f="${tmp}/count"
  echo 0 >"${f}"
  # shellcheck disable=SC2329  # called by wait_until
  bump() {
    echo $(($(cat "${f}") + 1)) >"${f}"
    [[ "$(cat "${f}")" -ge 2 ]]
  }
  wait_until "second poll" 20 bump >/dev/null
}
t_is_value() { is_value 7 echo 7 && ! is_value 7 echo 8 && is_empty true && ! is_empty echo x && ! is_empty false; }
check "wait_until returns when the command succeeds" t_wait_ok
check "wait_until times out with the reason" t_wait_timeout
check "wait_until polls the command again" t_wait_lazy
check "is_value and is_empty" t_is_value
check_fails "fail ends the scenario's subshell" bash -c "source '${here}/lib.sh'; fail boom"

# ---- stubs: the Alibaba Cloud CLI and curl the helpers call -------------------
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/aliyun" <<'STUB'
#!/usr/bin/env bash
# a stub of the Alibaba Cloud CLI for selftest.sh: it answers by product and action and
# records the arguments of its calls in $STUB_LOG
echo "$*" >>"${STUB_LOG:-/dev/null}"
case "$1 $2" in
  "ecs DescribeInstances")
    echo '{"Instances":{"Instance":[{"InstanceId":"i-aaa","Status":"Running","Tags":{"Tag":[{"TagKey":"scarpenter.sparecores.com/cluster","TagValue":"scarpenter-ci-1"}]}},{"InstanceId":"i-bbb","Status":"Stopping"}]}}' ;;
  "ecs DescribeDeploymentSets")
    echo '{"DeploymentSets":{"DeploymentSet":[
      {"DeploymentSetId":"ds-1","DeploymentSetDescription":"scarpenter.sparecores.com: cluster=scarpenter-ci-1 nodeclass=e2e-amd64"},
      {"DeploymentSetId":"ds-12","DeploymentSetDescription":"scarpenter.sparecores.com: cluster=scarpenter-ci-12 nodeclass=e2e-amd64"},
      {"DeploymentSetId":"ds-x","DeploymentSetDescription":"somebody else"}]}}' ;;
  "ecs DescribeAccountAttributes")
    echo '{"AccountAttributeItems":{"AccountAttributeItem":[{"AttributeName":"max-postpaid-instance-vcpu-count","AttributeValues":{"ValueItem":[{"Value":"16"}]}}]}}' ;;
  "ecs DescribeAvailableResource")
    echo '{"AvailableZones":{"AvailableZone":[
      {"ZoneId":"ap-southeast-1b","AvailableResources":{"AvailableResource":[{"SupportedResources":{"SupportedResource":[{"Value":"ecs.g7.large","Status":"Available"}]}}]}},
      {"ZoneId":"ap-southeast-1a","AvailableResources":{"AvailableResource":[{"SupportedResources":{"SupportedResource":[{"Value":"ecs.g7.large","Status":"Available"}]}}]}},
      {"ZoneId":"ap-southeast-1c","AvailableResources":{"AvailableResource":[{"SupportedResources":{"SupportedResource":[{"Value":"ecs.g7.large","Status":"SoldOut"}]}}]}}]}}' ;;
  *) echo "unexpected aliyun call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/aliyun"
t_cluster_instances() {
  [[ "$(PATH="${tmp}/bin:${PATH}" cluster_instances | paste -sd,)" == "i-aaa,i-bbb" ]]
}
t_active_instances() { # no scenario function in lib.sh: the runner's is defined in scenarios.sh
  [[ "$(PATH="${tmp}/bin:${PATH}" ecs DescribeInstances | jq -r '.Instances.Instance[] | select(.Status | IN("Pending", "Starting", "Running")) | .InstanceId')" == "i-aaa" ]]
}
t_cluster_tag_filter() { # the cluster's tag reaches the CLI as Tag.1.Key and Tag.1.Value, in the lane's region
  : >"${tmp}/calls"
  PATH="${tmp}/bin:${PATH}" STUB_LOG="${tmp}/calls" cluster_instances >/dev/null
  grep -q -- "--RegionId ${ALICLOUD_REGION}" "${tmp}/calls" &&
    grep -q -- '--Tag.1.Key scarpenter.sparecores.com/cluster --Tag.1.Value scarpenter-ci-1' "${tmp}/calls"
}
t_deployment_sets_match_whole_cluster() { # run 1 must not see the set of run 12
  [[ "$(PATH="${tmp}/bin:${PATH}" cluster_deployment_sets)" == "ds-1" ]]
}
t_account_attr() { [[ "$(PATH="${tmp}/bin:${PATH}" account_attr max-postpaid-instance-vcpu-count)" == 16 ]]; }
t_offered_zones() { # only the zones with stock, sorted
  [[ "$(PATH="${tmp}/bin:${PATH}" offered_zones ecs.g7.large | paste -sd,)" == "ap-southeast-1a,ap-southeast-1b" ]]
}
t_tag_of() {
  local j='{"Tags":{"Tag":[{"TagKey":"a","TagValue":"1"},{"TagKey":"b/c","TagValue":"2"}]}}'
  [[ "$(tag_of "${j}" b/c)" == 2 && -z "$(tag_of "${j}" zz)" && -z "$(tag_of '{}' a)" ]]
}
check "cluster_instances lists IDs one per line" t_cluster_instances
check "the status filter of active_instances keeps the live ones" t_active_instances
check "the cluster tag and the region reach the CLI as flags" t_cluster_tag_filter
check "cluster_deployment_sets matches the cluster marker whole" t_deployment_sets_match_whole_cluster
check "account_attr reads a value of DescribeAccountAttributes" t_account_attr
check "offered_zones lists the zones with stock" t_offered_zones
check "tag_of reads Tags.Tag" t_tag_of

# ali_login: the OIDC token of the run is exchanged at STS (a stub curl answers) for the credentials
# the CLI and the controller use; the token never reaches a command line, the cache is mode 0600.
cat >"${tmp}/bin/curl" <<'STUB'
#!/usr/bin/env bash
# a stub of curl for selftest.sh: records the form read from stdin and answers like STS
cat >"${STUB_CURL_BODY:-/dev/null}"
echo "$*" >"${STUB_CURL_ARGS:-/dev/null}"
echo '{"Credentials":{"AccessKeyId":"STS.stubkeyid1234567890","AccessKeySecret":"stubsecret","SecurityToken":"stubtoken","Expiration":"2099-01-01T00:00:00Z"}}'
STUB
chmod +x "${tmp}/bin/curl"
# login_env <dir> <body file> <command>: runs a command after ali_login in a clean process, with the stub curl,
# the run's role and a token file in <dir>.
login_env() {
  local d="$1" body="$2"
  shift 2
  env -u ALIBABA_CLOUD_ACCESS_KEY_ID PATH="${tmp}/bin:${PATH}" E2E_OUT="${d}" STUB_CURL_BODY="${body}" STUB_CURL_ARGS="${d}/args" \
    ALIBABA_CLOUD_ROLE_ARN=acs:ram::123456789012:role/ci ALIBABA_CLOUD_OIDC_PROVIDER_ARN=acs:ram::123456789012:oidc-provider/gha \
    ALIBABA_CLOUD_OIDC_TOKEN_FILE="${d}/token" bash -c "source '${here}/lib.sh'; ali_login 2>/dev/null; $*"
}
t_ali_login() {
  local d="${tmp}/login"
  mkdir -p "${d}"
  printf '%s' "header.payload.sig" >"${d}/token"
  # shellcheck disable=SC2016  # the command is evaluated in the child process
  [[ "$(login_env "${d}" "${d}/body" 'echo "${ALIBABA_CLOUD_ACCESS_KEY_ID} ${ALIBABA_CLOUD_ACCESS_KEY_SECRET} ${ALIBABA_CLOUD_SECURITY_TOKEN}"')" == "STS.stubkeyid1234567890 stubsecret stubtoken" ]] &&
    grep -q 'Action=AssumeRoleWithOIDC' "${d}/body" && grep -q 'OIDCToken=header.payload.sig' "${d}/body" &&
    ! grep -q 'header.payload' "${d}/args" && # not on the command line
    [[ "$(stat -c %a "${d}/ali-sts.env")" == 600 ]]
}
t_ali_login_cache() { # a second login uses the cache while the token file is not newer
  local d="${tmp}/login"
  touch -d '2 minutes ago' "${d}/token"
  # shellcheck disable=SC2016  # the command is evaluated in the child process
  [[ "$(login_env "${d}" "${d}/body2" 'echo "${ALIBABA_CLOUD_SECURITY_TOKEN}"')" == stubtoken && ! -e "${d}/body2" ]]
}
t_ali_login_refresh() { # a newer token file (the teardown's re-assume step) is exchanged again
  local d="${tmp}/login"
  touch "${d}/token"
  login_env "${d}" "${d}/body3" true && [[ -e "${d}/body3" ]]
}
check "ali_login exchanges the OIDC token, keeps it off the command line, caches with mode 0600" t_ali_login
check "ali_login uses the cache while the token file is not newer" t_ali_login_cache
check "ali_login exchanges a newer token file again" t_ali_login_refresh

# oidc-token.sh: the workflows' credentials step writes the job's OIDC token to a file (mode 0600) and
# exports the role variables; the request token never reaches a command line.
cat >"${tmp}/bin/curl-oidc" <<'STUB'
#!/usr/bin/env bash
echo "$*" >"${STUB_CURL_ARGS}"
echo '{"value":"header.payload.signature"}'
STUB
chmod +x "${tmp}/bin/curl-oidc"
t_oidc_token() {
  local d="${tmp}/oidc"
  mkdir -p "${d}/bin"
  cp "${tmp}/bin/curl-oidc" "${d}/bin/curl"
  : >"${d}/env"
  env PATH="${d}/bin:${PATH}" RUNNER_TEMP="${d}" GITHUB_ENV="${d}/env" STUB_CURL_ARGS="${d}/args" \
    ALICLOUD_ROLE_ARN=acs:ram::123456789012:role/ci ALICLOUD_OIDC_PROVIDER_ARN=acs:ram::123456789012:oidc-provider/gha \
    ALICLOUD_OIDC_AUDIENCE="sts.example.com" ACTIONS_ID_TOKEN_REQUEST_URL="https://token.example.com/?x=1" \
    ACTIONS_ID_TOKEN_REQUEST_TOKEN=requesttoken "${here}/oidc-token.sh" >"${d}/out" 2>&1 &&
    [[ "$(cat "${d}/alicloud-oidc-token")" == header.payload.signature && "$(stat -c %a "${d}/alicloud-oidc-token")" == 600 ]] &&
    grep -q "^ALIBABA_CLOUD_ROLE_ARN=acs:ram::123456789012:role/ci$" "${d}/env" &&
    grep -q "^ALIBABA_CLOUD_OIDC_TOKEN_FILE=${d}/alicloud-oidc-token$" "${d}/env" &&
    grep -q 'audience=sts.example.com' "${d}/args" && ! grep -q requesttoken "${d}/args" &&
    grep -q '^::add-mask::header.payload.signature$' "${d}/out"
}
t_oidc_token_needs_env() { env -u ALICLOUD_ROLE_ARN RUNNER_TEMP="${tmp}" GITHUB_ENV="${tmp}/env" "${here}/oidc-token.sh"; }
check "oidc-token.sh writes the token file (0600), masks it and exports the role variables" t_oidc_token
check_fails "oidc-token.sh refuses to run without the role" t_oidc_token_needs_env

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local ltai="LTAI""5tAbCdEfGhIjKlMnOpQr" tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop"
  local sts="CAIS""abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ" boot="abcdef"".0123456789abcdef"
  local in out leak
  in="key ${ltai}; ${tskey}; node host1.tail1234.ts.net; account 1234567890123456 acs:ram::1234567890123456:role/scarpenter-ci
Authorization: Bearer abc.def-ghi_jkl
authorization=Basic dXNlcjpwYXNz
header \"Authorization\": \"Bearer secret-token-value\"
curl -H 'Bearer zzz999' x
ALIBABA_CLOUD_SECURITY_TOKEN=${sts} ALIBABA_CLOUD_ACCESS_KEY_SECRET=wJalrXUtnFEMI/K7MDENG/bPxRfiCY
\"SecurityToken\": \"${sts}\", \"AccessKeySecret\": \"wJalrXUtnFEMI/K7MDENG\"
bare ${sts}
token ${boot} here
client-key-data: LS0tLS1CRUdJTg==
mail user@example.com"
  out="$(redact <<<"${in}")"
  for leak in "${ltai}" "${tskey}" "tail1234" "1234567890123456" "abc.def-ghi_jkl" "dXNlcjpwYXNz" "secret-token-value" \
    "zzz999" "${sts}" "wJalrXUtnFEMI" "${boot}" "LS0tLS1CRUdJTg" "user@example.com" "role/scarpenter-ci"; do
    if grep -qF -e "${leak}" <<<"${out}"; then
      echo "not redacted: ${leak}"
      echo "${out}"
      return 1
    fi
  done
  grep -q '<access-key-id>' <<<"${out}" && grep -q '<account>' <<<"${out}" && grep -q '<bootstrap-token>' <<<"${out}" &&
    grep -q '<tskey>' <<<"${out}" && grep -q '<security-token>' <<<"${out}"
}
t_redact_keeps_text() { [[ "$(redact <<<'the node scarpenter-ci-1 is Ready after 42s in ap-southeast-1a')" == 'the node scarpenter-ci-1 is Ready after 42s in ap-southeast-1a' ]]; }
check "redact removes account IDs, ARNs, keys, security tokens, Authorization and Bearer, bootstrap tokens" t_redact
check "redact leaves ordinary text alone" t_redact_keeps_text

# ---- waits for nodes, progress and snapshots, against a stub kubectl ----------
cat >"${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# a stub of kubectl for selftest.sh: one Pending pod of pool1, the claims of $STUB_CLAIMS
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$*" in
  *"get pods"*) echo '{"items":[{"metadata":{"name":"p1","namespace":"default","creationTimestamp":"'"${now}"'"},"spec":{"nodeSelector":{"karpenter.sh/nodepool":"pool1"}},"status":{"phase":"Pending"}}]}' ;;
  *"get nodeclaims"*) echo "${STUB_CLAIMS}" ;;
  *"get events"*) echo '{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes, key LTAI5tAbCdEfGhIjKlMnOpQr","involvedObject":{"kind":"Pod","name":"p1","namespace":"default"},"lastTimestamp":"'"${now}"'","metadata":{"creationTimestamp":"'"${now}"'"}}]}' ;;
  *"get nodepools"* | *"get scnodeclasses"*) echo '{"items":[]}' ;;
  *" logs "*) printf 'level=info all fine\nlevel=error msg="create failed" token=abcdef.0123456789abcdef\n' ;;
  *"describe pod"*) printf 'Name: p1\n    Environment:\n      SECRET_VALUE: hunter2\n    Mounts:\n' ;;
  *) ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"scarpenter://alicloud/ap-southeast-1/c1","conditions":[{"type":"Launched","status":"True"}]}}]}'
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
    ! grep -qE 'LTAI[A-Za-z0-9]{12}|abcdef\.0123456789abcdef' <<<"${out}"
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

# ---- the runner, the docs and the workflows ----------------------------------
t_scenarios_defined() {
  local names n
  names="$(sed -n 's/^all_scenarios=(\(.*\))$/\1/p' "${here}/scenarios.sh")"
  [[ -n "${names}" ]]
  for n in ${names} preflight; do
    grep -q "^scn_${n}() {" "${here}/scenarios.sh" || {
      echo "no function scn_${n}"
      return 1
    }
    grep -q "\`${n}\`\|| ${n} |" "${root}/docs/how-to-guides/testing.md" || [[ "${n}" == preflight ]] || {
      echo "docs/how-to-guides/testing.md does not describe the scenario ${n}"
      return 1
    }
  done
}
t_pins_pinned() { # every action of the L5 workflows is pinned by a full commit SHA (a local reusable workflow has no pin)
  local w="${root}/.github/workflows"
  ! grep -hE '^\s*-?\s*uses:' "${w}/e2e.yml" "${w}/e2e-cloud.yml" "${w}/e2e-canary.yml" "${w}/e2e-aws-sweeper.yml" "${w}/e2e-alicloud-sweeper.yml" "${w}/release.yml" |
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
t_vendor_wired() { # the vendor is in the list, and the workflows have its credentials, region and sweeper
  local w="${root}/.github/workflows"
  grep -qxF alicloud "${root}/test/e2e-vendors.txt" || { echo "alicloud is not in test/e2e-vendors.txt"; return 1; }
  grep -q "inputs.vendor == 'alicloud'" "${w}/e2e-cloud.yml" || { echo "e2e-cloud.yml has no credentials step for alicloud"; return 1; }
  grep -q "matrix.vendor == 'alicloud'" "${w}/e2e-canary.yml" || { echo "e2e-canary.yml has no credentials step for alicloud"; return 1; }
  grep -q 'alicloud) echo "ALICLOUD_REGION=' "${w}/e2e-cloud.yml" || { echo "e2e-cloud.yml does not set the region of alicloud"; return 1; }
  grep -q 'alicloud) echo "ALICLOUD_REGION=' "${w}/e2e-canary.yml" || { echo "e2e-canary.yml does not set the region of alicloud"; return 1; }
  grep -qE '^    environment: e2e-sweeper$' "${w}/e2e-alicloud-sweeper.yml" || { echo "the sweeper is not in the environment e2e-sweeper"; return 1; }
  grep -q 'go run ./test/e2e-alicloud/sweeper' "${w}/e2e-alicloud-sweeper.yml" || { echo "the sweeper workflow does not run the sweeper"; return 1; }
  grep -q 'test-e2e-alicloud-dryrun' "${w}/ci.yml" || { echo "ci.yml does not run the dry run"; return 1; }
}
t_no_secrets_in_scripts() { # nothing that looks like an account ID, a key or a local path
  ! grep -rnE '(acs:ram::[0-9]{12,}|tskey-(auth|client)-[A-Za-z0-9]{6,}|LTAI[A-Za-z0-9]{12,}|/home/[a-z]+|/tmp/)' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' | grep -v 'selftest.sh'
}
check "every scenario has a function and the docs describe it" t_scenarios_defined
check "every action in the L5 workflows is pinned by SHA with a version comment" t_pins_pinned
check "the smoke scenarios are scenarios of the runner" t_smoke_scenarios
check "the vendor list, the credentials steps, the region, the sweeper and the dry run in CI are wired" t_vendor_wired
check "no account ID, key or local path in the lane's files" t_no_secrets_in_scripts

# ---- the CI role's policy ----------------------------------------------------
t_policy() {
  local p="${tmp}/policy.json"
  "${here}/ci-policy.sh" 123456789012 >"${p}"
  # A RAM policy (Version 1); RunInstances is allowed only with the tag scarpenter-ci-* on the
  # request, in the lane's region; every type of the lane is allowed by the deny (the others
  # are denied); every write Allow names the region in its ARNs; the reads are Describe, List and Get.
  jq -e '
    .Version == "1" and
    ([.Statement[] | select(.Effect == "Allow" and (.Action | tostring | contains("ecs:RunInstances")))] | length) == 1 and
    ([.Statement[] | select(.Effect == "Allow" and (.Action | tostring | contains("ecs:RunInstances")))][0]
      | .Condition.StringLike["acs:RequestTag/scarpenter.sparecores.com/cluster"] == ["scarpenter-ci-*"]) and
    ([.Statement[] | select(.Effect == "Allow" and .Resource != "*") | .Resource | (if type == "array" then . else [.] end)[]]
      | length > 0 and all(startswith("acs:ecs:ap-southeast-1:123456789012:") or startswith("acs:vpc:ap-southeast-1:123456789012:"))) and
    ([.Statement[] | select(.Effect == "Allow" and .Resource == "*") | .Action[]]
      | length > 0 and all(test("^(ecs|vpc):Describe\\*$|^ecs:ListTagResources$|^quotas:(ListProductQuotas|GetProductQuota)$"))) and
    ([.Statement[] | select(.Effect == "Allow" and (.Action | tostring | test("Delete|Revoke|Authorize")) and (.Action | tostring | contains("DeploymentSet") | not)) | .Condition.StringLike["acs:ResourceTag/scarpenter.sparecores.com/cluster"]] | all(. == ["scarpenter-ci-*"])) and
    ([.Statement[] | select(.Effect == "Allow") | .Action | (if type == "array" then . else [.] end)[] | select(test("^ecs:(DescribeAccountAttributes|Describe\\*)$"))] | length) > 0 and
    ([.Statement[] | select(.Effect == "Allow") | .Action | (if type == "array" then . else [.] end)[]] | index("ecs:TagResources") != null)' "${p}" >/dev/null &&
    jq -e '.Statement[] | select(.Effect == "Deny") | .Action == "ecs:RunInstances" and .Resource == "*" and
      (.Condition.StringNotEquals."ecs:InstanceType" | index("ecs.e-c1m2.large") != null and index("ecs.c8y.large") != null and index("ecs.g7.large") != null)' "${p}" >/dev/null &&
    ! grep -q '"ecs:\*"\|"vpc:\*"\|"\*:\*"' "${p}"
}
t_policy_region() { # the region of a call is the one in the ARNs
  "${here}/ci-policy.sh" 123456789012 eu-central-1 | jq -e '[.Statement[] | .Resource | (if type == "array" then . else [.] end)[] | select(. != "*")] | all(test(":eu-central-1:"))' >/dev/null
}
t_policy_types() { # every type of the pins is allowed by the deny, so a type of defaults.sh is never refused
  local p="${tmp}/policy.json" t
  "${here}/ci-policy.sh" 123456789012 >"${p}"
  for t in "${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_DB}" ${E2E_TYPES_DESCRIBED//,/ }; do
    jq -e --arg t "${t#alicloud.}" '.Statement[] | select(.Effect == "Deny") | .Condition.StringNotEquals."ecs:InstanceType" | index($t) != null' "${p}" >/dev/null ||
      { echo "the type ${t} is denied by the policy"; return 1; }
  done
}
check "ci-policy.sh prints a RAM policy: tagged RunInstances in the region, the type deny, tagged mutations, reads" t_policy
check "ci-policy.sh limits the ARNs to the region it is given" t_policy_region
check "ci-policy.sh allows every type of defaults.sh" t_policy_types
check_fails "ci-policy.sh refuses a malformed account ID" "${here}/ci-policy.sh" 12345
check_fails "ci-policy.sh refuses a malformed region" "${here}/ci-policy.sh" 123456789012 "ap southeast"

# ---- the canary's read-only policy -------------------------------------------
t_canary_policy() {
  local p="${tmp}/canary-policy.json"
  "${here}/canary-policy.sh" 123456789012 eu-central-1 >"${p}"
  # valid JSON, one Allow statement, only Describe actions of ECS (no wildcard, no write)
  jq -e '
    .Version == "1" and (.Statement | length) == 1 and .Statement[0].Effect == "Allow" and
    (.Statement[0].Action | length > 0 and all(test("^ecs:Describe[A-Za-z]+$"))) and
    .Statement[0].Resource == "*"' "${p}" >/dev/null
}
check "canary-policy.sh prints a read-only policy" t_canary_policy
check_fails "canary-policy.sh refuses a malformed account ID" "${here}/canary-policy.sh" 12345
check_fails "canary-policy.sh refuses a malformed region" "${here}/canary-policy.sh" 123456789012 "ap southeast"

# ---- the chart with the lane's values ----------------------------------------
# The chart gets the vendor's values while this lane is written: until it knows them the
# check is skipped (and says so), it does not fail.
t_chart() {
  grep -q alicloud "${root}/charts/scarpenter/values.yaml" || {
    echo "SKIP: the chart has no alicloud values yet"
    return 0
  }
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep -q -- '--cluster-name=scarpenter-ci-1' "${tmp}/chart.yaml" &&
    grep -q 'ALIBABA_CLOUD_SECURITY_TOKEN' "${tmp}/chart.yaml" && grep -q 'https://100.64.0.1:6443' "${tmp}/chart.yaml" &&
    grep -q 'CriticalAddonsOnly' "${tmp}/chart.yaml"
}
check "the chart renders with the lane's values (schema, credentials, endpoint, toleration)" t_chart

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
