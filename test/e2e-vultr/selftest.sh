#!/usr/bin/env bash
# The parts of the Vultr L5 lane that need no Vultr, no Tailscale and no cluster: the helpers of
# lib.sh against fixtures and stubs, the consistency of the scenario list with the
# workflow, and the chart rendered with the lane's values. The manifests are checked
# against an API server by manifests_test.go (go test ./test/e2e-vultr/). Run through
# `make test-e2e-vultr-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-vultr/lib.sh
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
export E2E_BYO_GROUP_ID=5a000000-0000-4000-8000-000000000001 VULTR_REGION=ewr
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=vultr.vc2-1c-1gb CAPACITY=spot render "${here}/manifests/probe.yaml")"
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
for f in nodeclass-amd64 nodeclass-drift nodeclass-fw nodeclass-vpc nodeclass-use nodeclass-negative nodepools nodepool-use; do
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

# ---- stubs: the Vultr API the helpers call ---------------------------------------
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/curl" <<'STUB'
#!/usr/bin/env bash
# a stub of curl for selftest.sh: the list of instances of a tag, the groups and the VPCs
case "$*" in
  *"/instances?"*) echo '{"instances":[{"id":"1a000000-0000-4000-8000-000000000001"},{"id":"1a000000-0000-4000-8000-000000000002"}]}' ;;
  *"/firewalls?"*) echo '{"firewall_groups":[{"id":"g1","description":"scarpenter-x-firewall-groups-ewr | cluster=scarpenter-ci-1 nodeclass=default managed-by=scarpenter resource-kind=firewallGroups region=ewr"},{"id":"g2","description":"user group"},{"id":"g3","description":"scarpenter-y | cluster=scarpenter-ci-2 nodeclass=default managed-by=scarpenter resource-kind=firewallGroups region=ewr"}]}' ;;
  *"/vpcs?"*) echo '{"vpcs":[{"id":"v1","region":"ewr","description":"scarpenter-ci-1-byo"}]}' ;;
  *) echo "unexpected curl call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/curl"
t_cluster_instances() {
  [[ "$(PATH="${tmp}/bin:${PATH}" VULTR_API_KEY=k cluster_instances | paste -sd,)" == "1a000000-0000-4000-8000-000000000001,1a000000-0000-4000-8000-000000000002" ]]
}
t_managed_of() { # only the groups SCarpenter created for this cluster
  [[ "$(PATH="${tmp}/bin:${PATH}" VULTR_API_KEY=k managed_of groups | jq -r '.[].id' | paste -sd,)" == "g1" ]] &&
    [[ "$(PATH="${tmp}/bin:${PATH}" VULTR_API_KEY=k managed_of groups other | jq 'length')" == 0 ]] &&
    [[ "$(PATH="${tmp}/bin:${PATH}" VULTR_API_KEY=k managed_of vpcs | jq 'length')" == 0 ]]
}
t_tag_of() {
  [[ "$(tag_of '{"tags":["scarpenter.sparecores.com/cluster=c1","ci-run=5","x=a=b"]}' scarpenter.sparecores.com/cluster)" == c1 ]] &&
    [[ "$(tag_of '{"tags":["x=a=b"]}' x)" == "a=b" ]] && [[ -z "$(tag_of '{"tags":null}' x)" ]]
}
t_key_not_in_args() { # the key goes through a pipe, not an argument
  local log="${tmp}/curl-args"
  cat >"${tmp}/bin/curl" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$@" >"${log}"
echo '{}'
STUB
  PATH="${tmp}/bin:${PATH}" VULTR_API_KEY=SECRETKEY0123456789ABCDEFGHIJKLMNOP vapi /instances >/dev/null
  ! grep -q SECRETKEY "${log}"
}
check "cluster_instances lists IDs one per line" t_cluster_instances
check "managed_of finds only the marked objects of the cluster and the NodeClass" t_managed_of
check "tag_of reads a tag string's value" t_tag_of
check "vapi does not put the key in the arguments" t_key_not_in_args

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local vkey="ABCDEFGHIJ""KLMNOPQRSTUVWXYZ0123456789" tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop" boot="abcdef"".0123456789abcdef"
  local in out leak
  in="key VULTR_API_KEY=${vkey}; ${tskey}; node host1.tail1234.ts.net
Authorization: Bearer abc.def-ghi_jkl
authorization=Basic dXNlcjpwYXNz
header \"Authorization\": \"Bearer secret-token-value\"
curl -H 'Bearer zzz999' x
\"api_key\": \"${vkey}\", \"default_password\": \"hunter22\"
token ${boot} here
client-key-data: LS0tLS1CRUdJTg==
mail user@example.com"
  out="$(redact <<<"${in}")"
  for leak in "${vkey}" "${tskey}" "tail1234" "abc.def-ghi_jkl" "dXNlcjpwYXNz" "secret-token-value" \
    "zzz999" "hunter22" "${boot}" "LS0tLS1CRUdJTg" "user@example.com"; do
    if grep -qF -e "${leak}" <<<"${out}"; then
      echo "not redacted: ${leak}"
      echo "${out}"
      return 1
    fi
  done
  grep -q '<bootstrap-token>' <<<"${out}" && grep -q '<tskey>' <<<"${out}"
}
t_redact_keeps_text() { [[ "$(redact <<<'the node scarpenter-ci-1 is Ready after 42s')" == 'the node scarpenter-ci-1 is Ready after 42s' ]]; }
check "redact removes the API key, passwords, auth keys, Authorization and Bearer, bootstrap tokens" t_redact
check "redact leaves ordinary text alone" t_redact_keeps_text

# ---- waits for nodes, progress and snapshots, against a stub kubectl ----------
cat >"${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# a stub of kubectl for selftest.sh: one Pending pod of pool1, the claims of $STUB_CLAIMS
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$*" in
  *"get pods"*) echo '{"items":[{"metadata":{"name":"p1","namespace":"default","creationTimestamp":"'"${now}"'"},"spec":{"nodeSelector":{"karpenter.sh/nodepool":"pool1"}},"status":{"phase":"Pending"}}]}' ;;
  *"get nodeclaims"*) echo "${STUB_CLAIMS}" ;;
  *"get events"*) echo '{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes, key TOKENABCDEFGHIJKLMNOPQRSTUVWXYZ0","involvedObject":{"kind":"Pod","name":"p1","namespace":"default"},"lastTimestamp":"'"${now}"'","metadata":{"creationTimestamp":"'"${now}"'"}}]}' ;;
  *"get nodepools"* | *"get scnodeclasses"*) echo '{"items":[]}' ;;
  *" logs "*) printf 'level=info all fine\nlevel=error msg="create failed" token=abcdef.0123456789abcdef\n' ;;
  *"describe pod"*) printf 'Name: p1\n    Environment:\n      SECRET_VALUE: hunter2\n    Mounts:\n' ;;
  *) ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"scarpenter://vultr/ewr/c1","conditions":[{"type":"Launched","status":"True"}]}}]}'
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

# ---- the runner and the workflows --------------------------------------------
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
t_pins_pinned() { # every action of the Vultr workflows is pinned by a full commit SHA
  local w="${root}/.github/workflows"
  ! grep -hE '^\s*-?\s*uses:' "${w}/e2e-vultr-sweeper.yml" "${w}/e2e-cloud.yml" "${w}/e2e-canary.yml" |
    grep -vE 'uses: \./' | grep -vE 'uses: [^@ ]+@[0-9a-f]{40} # v[0-9]'
}
t_sweeper_workflow() { # its own environment (no reviewers), limited to main; the engine never calls a snapshot API
  local f="${root}/.github/workflows/e2e-vultr-sweeper.yml"
  grep -qE '^    environment: e2e-sweeper$' "${f}" && ! grep -q 'Snapshot' "${root}/pkg/vendors/vultr/sweep/sweep.go"
}
t_not_in_the_vendor_list() { # open question 7 of plan 20: not before the account's limits are set
  ! grep -qx 'vultr' "${root}/test/e2e-vendors.txt"
}
t_no_secrets_in_scripts() { # nothing that looks like a key or a local path
  ! grep -rnE '(tskey-(auth|client)-[A-Za-z0-9]{6,}|/home/[a-z]+|/tmp/)' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' | grep -v 'selftest.sh'
}
check "every scenario has a function" t_scenarios_defined
check "the smoke scenarios are scenarios of the runner" t_smoke_scenarios
check "every action in the Vultr workflows is pinned by SHA with a version comment" t_pins_pinned
check "the sweeper runs in the environment e2e-sweeper and its engine never calls a snapshot API" t_sweeper_workflow
check "vultr is not in test/e2e-vendors.txt until the account's limits are set" t_not_in_the_vendor_list
check "no key or local path in the lane's files" t_no_secrets_in_scripts

# ---- the chart with the lane's values ----------------------------------------
t_chart() {
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep -q -- '--cluster-name=scarpenter-ci-1' "${tmp}/chart.yaml" && grep -q -- '--vendors=vultr' "${tmp}/chart.yaml" &&
    grep -q 'VULTR_API_KEY' "${tmp}/chart.yaml" && grep -q 'vultr-credentials' "${tmp}/chart.yaml" &&
    ! grep -q 'AWS_REGION' "${tmp}/chart.yaml" && grep -q 'https://100.64.0.1:6443' "${tmp}/chart.yaml" &&
    grep -q 'CriticalAddonsOnly' "${tmp}/chart.yaml"
}
check "the chart renders with the lane's values (schema, credentials, endpoint, toleration)" t_chart

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
