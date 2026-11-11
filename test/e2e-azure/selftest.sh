#!/usr/bin/env bash
# The parts of the Azure L5 lane that need no Azure, no Tailscale and no cluster: the helpers
# of lib.sh against fixtures and stubs (az, kubectl, curl), the federated token script, the
# consistency of the scenario list with the workflows, the CI and canary policies the lane
# prints, and the chart rendered with the lane's values. The manifests are checked against an
# API server by manifests_test.go (go test ./test/e2e-azure/). Run through
# `make test-e2e-azure-dryrun`. Nothing here talks to a cloud: no credential is read, `az` is
# a stub, and a real one is never on the PATH of a check that could reach it.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-azure/lib.sh
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

# The stubs come first on the PATH of every check that calls a tool: a stub that does not know
# a call fails it, so that nothing can fall through to a real az, kubectl or curl.
mkdir -p "${tmp}/bin"

# ---- render ------------------------------------------------------------------
export E2E_CLUSTER=scarpenter-ci-1 E2E_RUN_ID=1 E2E_CREATED_AT=1800000000 E2E_API_HOST=100.64.0.1
export E2E_PG_ZONE=1 AZURE_SUBSCRIPTION_ID=00000000-0000-0000-0000-000000000000 AZURE_RESOURCE_GROUP=scarpenter-ci-lane
export AZURE_TENANT_ID=00000000-0000-0000-0000-000000000001 AZURE_CLIENT_ID=00000000-0000-0000-0000-000000000002
E2E_CREATED_AT_RFC3339="$(date -u -d "@${E2E_CREATED_AT}" +%Y-%m-%dT%H:%M:%SZ)"
export E2E_CREATED_AT_RFC3339
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=azure.Standard_B2ats_v2 CAPACITY=spot render "${here}/manifests/probe.yaml")"
  grep -q 'name: probe1' <<<"${out}" && grep -q 'karpenter.sh/nodepool: pool1' <<<"${out}" && grep -q 'capacity-type: spot' <<<"${out}" &&
    ! grep -qF -e "\${" <<<"${out}"
}
t_render_unset() { (unset NAME POOL TYPE CAPACITY; render "${here}/manifests/probe.yaml"); }
t_render_keeps_other_dollars() { # only the manifest's own ${NAME} are replaced
  # shellcheck disable=SC2016  # the dollar signs are the test's input
  printf 'a: ${NAME}\nb: $HOME\n' >"${tmp}/m.yaml"
  [[ "$(NAME=x render "${tmp}/m.yaml")" == $'a: x\nb: $HOME' ]]
}
t_created_at() { # the sweeper reads the tag as RFC 3339
  [[ "${E2E_CREATED_AT_RFC3339}" == "2027-01-15T08:00:00Z" ]] &&
    grep -q 'scarpenter-ci-created-at: "2027-01-15T08:00:00Z"' <(render "${here}/manifests/nodeclass-amd64.yaml")
}
check "render fills every variable" t_render
check_fails "render refuses an unset variable" t_render_unset
check "render leaves other dollar signs alone" t_render_keeps_other_dollars
check "the created-at tag is rendered as RFC 3339" t_created_at
for f in nodeclass-amd64 nodeclass-arm64 nodeclass-drift nodeclass-negative nodepools nodepool-negative; do
  check "render ${f}" render "${here}/manifests/${f}.yaml"
done
t_render_use() {
  E2E_BYO_SUBNET_ID=/subscriptions/x/subnets/nodes E2E_BYO_NSG_ID=/subscriptions/x/nsg/n render "${here}/manifests/nodeclass-use.yaml" |
    grep -q '/subscriptions/x/subnets/nodes'
}
check "render nodeclass-use with the brought IDs" t_render_use

# ---- numbers and metrics -----------------------------------------------------
cat >"${tmp}/metrics.txt" <<'METRICS'
# HELP scarpenter_quota_limit Quota limit of a bucket.
scarpenter_quota_limit{bucket="cores",region="westeurope",vendor="azure"} 32
scarpenter_quota_limit{bucket="lowPriorityCores",region="westeurope",vendor="azure"} 8
scarpenter_quota_limit{bucket="cores",region="northeurope",vendor="azure"} 5
scarpenter_quota_used{bucket="cores",region="westeurope",vendor="azure"} 4
scarpenter_quota_reserved{bucket="cores",region="westeurope",vendor="azure"} 0
scarpenter_quota_used_total{bucket="cores",region="westeurope",vendor="azure"} 99
METRICS
t_metric() {
  [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit westeurope cores)" == 32 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit westeurope lowPriorityCores)" == 8 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit northeurope cores)" == 5 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_used westeurope cores)" == 4 ]] &&
    [[ -z "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit westeurope StandardPublicIPAddresses)" ]]
}
t_num() { [[ "$(num 5)" == 5 && "$(num 5.0)" == 5 && "$(num 32e+00)" == 32 && "$(num 0)" == 0 ]]; }
check "metric_value finds the series of a region and bucket (and not a longer metric name)" t_metric
check "num reads Prometheus and Azure numbers" t_num

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

# ---- stubs: the az CLI the helpers call --------------------------------------
# The stub prints what the test prepared for the kind of call (the JMESPath query is az's own
# business and is not run here): STUB_VMS, STUB_NICS, STUB_PIPS, STUB_DISKS, STUB_USAGE,
# STUB_LIMIT, STUB_RESOURCES, STUB_SKUS; STUB_FAIL=1 makes every call fail like an expired login.
cat >"${tmp}/bin/az" <<'STUB'
#!/usr/bin/env bash
[[ "${STUB_FAIL:-}" != 1 ]] || { echo "ERROR: Please run 'az login' to setup account." >&2; exit 1; }
echo "$*" >>"${STUB_LOG:-/dev/null}"
case "$*" in
  "vm list "*) printf '%b' "${STUB_VMS-}" ;;
  "network nic list "*) printf '%b' "${STUB_NICS-}" ;;
  "network public-ip list "*) printf '%b' "${STUB_PIPS-}" ;;
  "disk list "*) printf '%b' "${STUB_DISKS-}" ;;
  "resource list "*) printf '%b' "${STUB_RESOURCES-}" ;;
  "vm list-usage "*) if [[ "$*" == *"currentValue"* ]]; then printf '%s\n' "${STUB_USAGE-}"; else printf '%s\n' "${STUB_LIMIT-}"; fi ;;
  "vm list-skus "*) printf '%s' "${STUB_SKUS-[]}" ;;
  *) echo "unexpected az call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/az"
cluster_hash_of() { printf '%s' "$1" | sha256sum | cut -c1-8; }
t_cluster_hash() { # the first 8 hex digits of the SHA-256 of the cluster name (the driver's hash8)
  [[ "$(cluster_hash)" == "$(cluster_hash_of scarpenter-ci-1)" && "$(cluster_hash)" =~ ^[0-9a-f]{8}$ ]] &&
    [[ "$(vm_name_of node-abc)" == "node-abc-$(cluster_hash)" ]]
}
t_cluster_vms() {
  [[ "$(PATH="${tmp}/bin:${PATH}" STUB_VMS=$'vm-a\r\nvm-b\n' cluster_vms | paste -sd,)" == "vm-a,vm-b" ]] &&
    [[ -z "$(PATH="${tmp}/bin:${PATH}" STUB_VMS="" cluster_vms)" ]]
}
t_cluster_vms_error() { # a failed listing is an error, not "no VM": is_empty fails
  ! PATH="${tmp}/bin:${PATH}" STUB_FAIL=1 is_empty cluster_vms all &&
    PATH="${tmp}/bin:${PATH}" STUB_VMS="" is_empty cluster_vms all
}
t_leftovers() {
  local out
  out="$(PATH="${tmp}/bin:${PATH}" STUB_NICS=$'vm-a-nic-1234abcd\n' STUB_PIPS="" STUB_DISKS=$'vm-a-os\nvm-a-pgdata\n' vm_leftovers vm-a)"
  [[ "${out}" == $'nic vm-a-nic-1234abcd\ndisk vm-a-os\ndisk vm-a-pgdata' ]] &&
    [[ -z "$(PATH="${tmp}/bin:${PATH}" STUB_NICS="" STUB_PIPS="" STUB_DISKS="" vm_leftovers vm-a)" ]] &&
    ! PATH="${tmp}/bin:${PATH}" STUB_FAIL=1 vm_leftovers vm-a
}
t_usage() {
  [[ "$(PATH="${tmp}/bin:${PATH}" STUB_USAGE=6 usage_of compute cores)" == 6 ]] &&
    [[ "$(PATH="${tmp}/bin:${PATH}" STUB_LIMIT=32 limit_of compute cores)" == 32 ]]
}
t_skus() { # the SKU rows of the lane's sizes are read once and cached
  local skus='[{"name":"Standard_B2ats_v2","family":"standardBasv2Family","capabilities":[{"name":"vCPUs","value":"2"}],"restrictions":[],"locationInfo":[{"zones":["2","1","3"]}]},{"name":"Standard_Other","family":"x"}]'
  local f
  f="$(PATH="${tmp}/bin:${PATH}" E2E_OUT="${tmp}/skus-out" STUB_SKUS="${skus}" bash -c "source '${here}/lib.sh'; skus_file")"
  [[ "$(jq length "${f}")" == 1 ]] &&
    [[ "$(PATH="${tmp}/bin:${PATH}" E2E_OUT="${tmp}/skus-out" STUB_FAIL=1 bash -c "source '${here}/lib.sh'; vcpus_of Standard_B2ats_v2")" == 2 ]] &&
    [[ "$(PATH="${tmp}/bin:${PATH}" E2E_OUT="${tmp}/skus-out" bash -c "source '${here}/lib.sh'; sku_family Standard_B2ats_v2")" == standardBasv2Family ]] &&
    [[ "$(PATH="${tmp}/bin:${PATH}" E2E_OUT="${tmp}/skus-out" bash -c "source '${here}/lib.sh'; sku_field Standard_B2ats_v2 '[.locationInfo[]?.zones[]?] | sort | first'")" == 1 ]]
}
check "cluster_hash and vm_name_of follow the driver's naming" t_cluster_hash
check "cluster_vms lists names one per line" t_cluster_vms
check "a failed listing is an error and never passes for empty" t_cluster_vms_error
check "vm_leftovers names the NICs, public IPs and disks of a VM" t_leftovers
check "usage_of and limit_of read the Usage APIs' rows" t_usage
check "the SKU rows of the lane's sizes are cached (vCPUs, family, zones)" t_skus

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop" boot="abcdef"".0123456789abcdef"
  local guid="12345678-1234-1234-""1234-123456789abc" jwt="eyJhbGciOiJSUzI1""NiJ9.eyJzdWIiOiJzY2FycGVudGVyIn0.c2lnbmF0dXJl"
  local in out leak
  in="key ${tskey}; node host1.tail1234.ts.net; subscription /subscriptions/${guid}/resourceGroups/rg tenant ${guid}
Authorization: Bearer abc.def-ghi_jkl
authorization=Basic dXNlcjpwYXNz
header \"Authorization\": \"Bearer secret-token-value\"
curl -H 'Bearer zzz999' x
AZURE_CLIENT_SECRET=Zq8~notarealsecretvalue AZURE_FEDERATED_TOKEN=${jwt}
\"access_token\": \"${jwt}\", \"client_secret\": \"Zq8~notarealsecretvalue\", \"adminPassword\": \"hunter2hunter2\"
https://store.blob.core.windows.net/x?sv=2022&sig=AbCdEf%2Bghij%3D&se=2027
bare ${jwt}
token ${boot} here
client-key-data: LS0tLS1CRUdJTg==
mail user@example.com"
  out="$(redact <<<"${in}")"
  for leak in "${tskey}" "tail1234" "${guid}" "abc.def-ghi_jkl" "dXNlcjpwYXNz" "secret-token-value" \
    "zzz999" "${jwt}" "Zq8~notarealsecretvalue" "hunter2hunter2" "AbCdEf" "${boot}" "LS0tLS1CRUdJTg" "user@example.com"; do
    if grep -qF -e "${leak}" <<<"${out}"; then
      echo "not redacted: ${leak}"
      echo "${out}"
      return 1
    fi
  done
  grep -q '<guid>' <<<"${out}" && grep -q '<bootstrap-token>' <<<"${out}" && grep -q '<tskey>' <<<"${out}" && grep -q '<jwt>' <<<"${out}"
}
t_redact_keeps_text() { [[ "$(redact <<<'the node scarpenter-ci-1 is Ready after 42s in westeurope')" == 'the node scarpenter-ci-1 is Ready after 42s in westeurope' ]]; }
check "redact removes GUIDs, secrets, tokens, SAS signatures, Authorization and Bearer, bootstrap tokens" t_redact
check "redact leaves ordinary text alone" t_redact_keeps_text

# ---- waits for nodes, progress and snapshots, against a stub kubectl ----------
cat >"${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# a stub of kubectl for selftest.sh: one Pending pod of pool1, the claims of $STUB_CLAIMS
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$*" in
  *"get pods"*) echo '{"items":[{"metadata":{"name":"p1","namespace":"default","creationTimestamp":"'"${now}"'"},"spec":{"nodeSelector":{"karpenter.sh/nodepool":"pool1"}},"status":{"phase":"Pending"}}]}' ;;
  *"get nodeclaims"*) echo "${STUB_CLAIMS}" ;;
  *"get events"*) echo '{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes, AZURE_CLIENT_SECRET=Zq8~notarealsecretvalue","involvedObject":{"kind":"Pod","name":"p1","namespace":"default"},"lastTimestamp":"'"${now}"'","metadata":{"creationTimestamp":"'"${now}"'"}}]}' ;;
  *"get nodepools"* | *"get scnodeclasses"*) echo '{"items":[]}' ;;
  *" logs "*) printf 'level=info all fine\nlevel=error msg="create failed" token=abcdef.0123456789abcdef\n' ;;
  *"describe pod"*) printf 'Name: p1\n    Environment:\n      SECRET_VALUE: hunter2\n    Mounts:\n' ;;
  *) ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"scarpenter://azure/westeurope/c1","conditions":[{"type":"Launched","status":"True"}]}}]}'
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
    ! grep -qE 'Zq8~notarealsecretvalue|abcdef\.0123456789abcdef' <<<"${out}"
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

# ---- the federated token the controller reads (stub curl, no network) ----------
cat >"${tmp}/bin/curl" <<'STUB'
#!/usr/bin/env bash
# a stub of curl for selftest.sh: the GitHub OIDC token endpoint, one new token per call
n="$(cat "${STUB_CURL_COUNT}" 2>/dev/null || echo 0)"
echo $((n + 1)) >"${STUB_CURL_COUNT}"
echo "$*" >>"${STUB_CURL_LOG}"
printf '{"count":1,"value":"eyJhbGciOiJub25lIn0.eyJjb3VudCI6%s9.c2ln"}\n' "$((n + 1))"
STUB
chmod +x "${tmp}/bin/curl"
tok_env() { # tok_env <command...>: the job's OIDC variables (fake) and the stubs
  PATH="${tmp}/bin:${PATH}" STUB_CURL_COUNT="${tmp}/curl-count" STUB_CURL_LOG="${tmp}/curl-log" \
    ACTIONS_ID_TOKEN_REQUEST_URL="https://token.example.com/oidc?api-version=2" ACTIONS_ID_TOKEN_REQUEST_TOKEN="request-token-value" "$@"
}
t_token_once() {
  rm -f "${tmp}/curl-count" "${tmp}/curl-log" "${tmp}/tok"
  local out
  out="$(tok_env "${here}/azure-token.sh" once "${tmp}/tok")"
  [[ "$(stat -c %a "${tmp}/tok")" == 600 ]] &&
    [[ "$(cat "${tmp}/tok")" == eyJhbGciOiJub25lIn0.eyJjb3VudCI6* ]] &&
    grep -q 'audience=api%3A%2F%2FAzureADTokenExchange' "${tmp}/curl-log" &&
    ! grep -q 'request-token-value' "${tmp}/curl-log" &&
    grep -q "^::add-mask::$(cat "${tmp}/tok")$" <<<"${out}" &&
    ! ls "${tmp}"/tok.* >/dev/null 2>&1 # no temporary file is left
}
t_token_needs_oidc() { env -u ACTIONS_ID_TOKEN_REQUEST_URL -u ACTIONS_ID_TOKEN_REQUEST_TOKEN PATH="${tmp}/bin:${PATH}" "${here}/azure-token.sh" once "${tmp}/tok2"; }
t_token_loop() { # start detaches a renewal loop, the file is replaced, stop ends it and only it
  rm -f "${tmp}/curl-count" "${tmp}/tok3"
  tok_env env E2E_AZURE_TOKEN_INTERVAL=1 "${here}/azure-token.sh" start "${tmp}/tok3" >/dev/null
  local pid first
  pid="$(cat "${tmp}/tok3.pid")"
  first="$(cat "${tmp}/tok3")"
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    [[ "$(cat "${tmp}/tok3")" != "${first}" ]] && break
    sleep 1
  done
  [[ "$(cat "${tmp}/tok3")" != "${first}" ]] || { echo "the token was not renewed"; return 1; }
  [[ "$(stat -c %a "${tmp}/tok3")" == 600 ]] || return 1
  "${here}/azure-token.sh" stop "${tmp}/tok3"
  for _ in 1 2 3 4 5; do kill -0 "${pid}" 2>/dev/null || break; sleep 1; done
  ! kill -0 "${pid}" 2>/dev/null && [[ ! -e "${tmp}/tok3.pid" ]]
}
t_token_stop_foreign() { # a process ID that is not the loop is left alone
  sleep 30 &
  local pid=$!
  echo "${pid}" >"${tmp}/tok4.pid"
  "${here}/azure-token.sh" stop "${tmp}/tok4"
  local alive=0
  kill -0 "${pid}" 2>/dev/null && alive=1
  kill "${pid}" 2>/dev/null || true
  [[ "${alive}" == 1 ]]
}
check "azure-token.sh once writes a 0600 token for the Entra audience without the request token on a command line" t_token_once
check_fails "azure-token.sh once needs the job's OIDC permission" t_token_needs_oidc
check "azure-token.sh start renews the token in the background and stop ends the loop" t_token_loop
check "azure-token.sh stop leaves a foreign process alone" t_token_stop_foreign

# ---- the runner, the workflows and the vendor list ----------------------------
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
  local w="${root}/.github/workflows" f files=()
  for f in e2e.yml e2e-cloud.yml e2e-canary.yml e2e-aws-sweeper.yml e2e-azure-sweeper.yml release.yml; do
    [[ -f "${w}/${f}" ]] && files+=("${w}/${f}")
  done
  ! grep -hE '^\s*-?\s*uses:' "${files[@]}" |
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
t_workflows_azure() { # the Azure steps: OIDC login with the variables, the token file, the region, the teardown login
  local w="${root}/.github/workflows" f
  for f in e2e-cloud.yml e2e-canary.yml; do
    grep -q 'azure/login@[0-9a-f]\{40\} # v' "${w}/${f}" || { echo "${f}: no pinned azure/login"; return 1; }
    for v in AZURE_CLIENT_ID AZURE_TENANT_ID AZURE_SUBSCRIPTION_ID AZURE_REGION; do
      grep -q "vars.${v}" "${w}/${f}" || { echo "${f}: the variable ${v} is not read"; return 1; }
    done
    ! grep -qE 'secrets\.AZURE' "${w}/${f}" || { echo "${f}: an Azure secret (the identity is federated)"; return 1; }
    grep -q "azure-token.sh start" "${w}/${f}" || { echo "${f}: no federated token renewal"; return 1; }
  done
  grep -q "inputs.vendor == 'azure'" "${w}/e2e-cloud.yml" && grep -q "matrix.vendor == 'azure'" "${w}/e2e-canary.yml" || return 1
  # the teardown logs in again, and the renewal loop is told which Secret to keep fresh
  grep -q 'Log in to Azure again for the teardown (azure)' "${w}/e2e-cloud.yml" &&
    grep -q 'AZURE_TOKEN_SECRET: scarpenter-system/azure-federated-token' "${w}/e2e-cloud.yml"
}
t_vendor_list() { # azure joins test/e2e-vendors.txt only when its cost guards are verified live; defaults.sh says so until then
  if grep -qxF azure "${root}/test/e2e-vendors.txt"; then
    ! grep -q 'THIS LANE IS NOT IN test/e2e-vendors.txt' "${here}/defaults.sh" || { echo "azure is in the list: remove the note in defaults.sh"; return 1; }
  else
    grep -q 'THIS LANE IS NOT IN test/e2e-vendors.txt' "${here}/defaults.sh" || { echo "azure is not in the list and defaults.sh does not say why"; return 1; }
  fi
}
t_no_secrets_in_scripts() { # nothing that looks like a real subscription ID, a key or a local path
  ! grep -rnE '(tskey-(auth|client)-[A-Za-z0-9]{6,}|/home/[a-z]+|/tmp/|(^|[^0-9a-fA-F-])[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}([^0-9a-fA-F-]|$))' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' |
    grep -vE 'selftest.sh|00000000-0000-0000-0000-00000000000[0-9]|12345678-1234-1234-|policyDefinitions/(cccc23c7|e56962a6)-'  # the two built-in policy definitions are public
}
check "every scenario has a function and is named by the workflow" t_scenarios_defined
check "every action in the L5 workflows is pinned by SHA with a version comment" t_pins_pinned
check "the smoke scenarios are scenarios of the runner" t_smoke_scenarios
check "the workflows have the Azure steps: federated login, variables, token renewal, teardown login" t_workflows_azure
check "azure is not in test/e2e-vendors.txt, and defaults.sh lists the guards still to verify" t_vendor_list
check "no real ID, key or local path in the lane's files" t_no_secrets_in_scripts

# ---- the CI identity's policies ----------------------------------------------
t_policy() {
  local p="${tmp}/policy.json" sub=00000000-0000-0000-0000-000000000000
  "${here}/ci-policy.sh" "${sub}" >"${p}"
  # the lane role is assigned to the lane's resource group only, the reads at the subscription;
  # the size policy lists exactly the lane's sizes (and not the denied one) and the location
  # policy the lane's region; both at the resource group
  jq -e --arg sub "${sub}" '
    (.roleAssignments | map(select(.role == "scarpenter-ci-lane")) | .[0].scope) as $lane
    | ($lane | test("^/subscriptions/" + $sub + "/resourceGroups/[^/]+$")) and
      (.roleAssignments | map(select(.role == "scarpenter-ci-reads")) | .[0].scope) == "/subscriptions/\($sub)" and
      (.policyAssignments | map(.scope) | all(. == $lane)) and
      ([.roleDefinitions[].AssignableScopes[]] | all(. == "/subscriptions/\($sub)"))' "${p}" >/dev/null &&
    jq -e '.policyAssignments[] | select(.displayName == "Allowed virtual machine size SKUs")
      | .policyDefinitionId == "/providers/Microsoft.Authorization/policyDefinitions/cccc23c7-8427-4f53-ad12-b6a63eb452b3"
        and (.parameters.listOfAllowedSKUs.value | sort) == ["Standard_B2as_v2", "Standard_B2ats_v2", "Standard_B2pts_v2", "Standard_D2as_v5"]' "${p}" >/dev/null &&
    jq -e '.policyAssignments[] | select(.displayName == "Allowed locations")
      | .parameters.listOfAllowedLocations.value == ["westeurope"]' "${p}" >/dev/null &&
    # the lane role writes only compute, network, and reads resources and the activity log; the reads role only reads
    jq -e '(.roleDefinitions[] | select(.Name == "scarpenter-ci-lane") | .Actions | all(test("^Microsoft\\.(Compute|Network|Resources|Insights)/"))) and
      (.roleDefinitions[] | select(.Name == "scarpenter-ci-reads") | .Actions | all(test("/read$"))) and
      (.roleDefinitions | all(.DataActions == [] and .NotActions == []))' "${p}" >/dev/null
}
t_policy_region() {
  "${here}/ci-policy.sh" 00000000-0000-0000-0000-000000000000 northeurope scarpenter-ci-other |
    jq -e '(.policyAssignments[] | select(.displayName == "Allowed locations") | .parameters.listOfAllowedLocations.value == ["northeurope"]) and
      (.policyAssignments | all(.scope | endswith("/resourceGroups/scarpenter-ci-other")))' >/dev/null
}
check "ci-policy.sh prints the roles and the size and location policies for the lane's resource group only" t_policy
check "ci-policy.sh takes the region and the resource group" t_policy_region
check_fails "ci-policy.sh refuses a malformed subscription ID" "${here}/ci-policy.sh" 12345
check_fails "ci-policy.sh refuses a malformed region" "${here}/ci-policy.sh" 00000000-0000-0000-0000-000000000000 "west europe"

# ---- the canary's read-only role ---------------------------------------------
t_canary_policy() {
  local p="${tmp}/canary-policy.json"
  "${here}/canary-policy.sh" 00000000-0000-0000-0000-000000000000 >"${p}"
  # Reader plus a custom role with the two marketplace agreement reads: nothing that writes,
  # no wildcard, no data action
  jq -e '
    (.roleAssignments | map(.role) | sort) == ["Reader", "scarpenter-canary-marketplace-reads"] and
    (.roleDefinitions | length) == 1 and
    (.roleDefinitions[0].Actions | sort) == ["Microsoft.MarketplaceOrdering/agreements/read", "Microsoft.MarketplaceOrdering/offerTypes/publishers/offers/plans/agreements/read"] and
    (.roleDefinitions[0].Actions | all(endswith("/read") and (contains("*") | not))) and
    .roleDefinitions[0].DataActions == [] and .roleDefinitions[0].NotActions == []' "${p}" >/dev/null
}
check "canary-policy.sh prints Reader plus the marketplace agreement reads, nothing that writes" t_canary_policy
check_fails "canary-policy.sh refuses a malformed subscription ID" "${here}/canary-policy.sh" 12345

# ---- the chart with the lane's values ----------------------------------------
t_chart() {
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep -q -- '--cluster-name=scarpenter-ci-1' "${tmp}/chart.yaml" &&
    grep -q 'AZURE_FEDERATED_TOKEN_FILE' "${tmp}/chart.yaml" && grep -q 'AZURE_SUBSCRIPTION_ID' "${tmp}/chart.yaml" &&
    grep -q 'secretName: azure-federated-token' "${tmp}/chart.yaml" &&
    grep -q 'https://100.64.0.1:6443' "${tmp}/chart.yaml" && grep -q 'CriticalAddonsOnly' "${tmp}/chart.yaml"
}
# The chart's Azure values are written next to the driver; until the chart knows vendors.azure
# the lane's values cannot be rendered (its schema refuses them), and the check says so.
if grep -qE '^  azure:' "${root}/charts/scarpenter/values.yaml"; then
  check "the chart renders with the lane's values (schema, credentials, token file, endpoint, toleration)" t_chart
else
  echo "skip the chart renders with the lane's values (the chart has no vendors.azure yet)"
fi

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
