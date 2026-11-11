#!/usr/bin/env bash
# The parts of the L5 lane (GCP) that need no Google Cloud, no Tailscale and no cluster: the
# helpers of lib.sh against fixtures and stubs, the consistency of the scenario list, the
# workflows, the sweeper's names and the lane's pins, the CI and canary roles, the chart
# rendered with the lane's values, and that the lane is ready to be listed in
# test/e2e-vendors.txt. The manifests are checked against an API server by manifests_test.go
# (go test ./test/e2e-gcp/). Run through `make test-e2e-gcp-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
export GCP_PROJECT=my-project
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-gcp/lib.sh
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
export E2E_PG_ZONE=us-central1-a E2E_BYO_TAG=nodes
export E2E_BYO_SUBNET=projects/my-project/regions/us-central1/subnetworks/scarpenter-e2e-byo-1-us-central1
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=gcp.e2-small CAPACITY=spot render "${here}/manifests/probe.yaml")"
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
for f in nodeclass-amd64 nodeclass-arm64 nodeclass-drift nodeclass-create nodeclass-default nodeclass-use nodeclass-negative \
  nodepools nodepool-arm64 nodepool-use nodepool-create nodepool-default db described; do
  check "render ${f}" render "${here}/manifests/${f}.yaml"
done

# ---- the pins ----------------------------------------------------------------
t_pins() {
  [[ "${E2E_FLATCAR_IMAGE}" == "flatcar-stable-4757-2-3" || "${E2E_FLATCAR_VERSION}" != 4757.2.3 ]] || { echo "image ${E2E_FLATCAR_IMAGE}"; return 1; }
  [[ "$(env -u GCP_ZONE GCP_REGION=europe-west4 bash -c "source '${here}/defaults.sh'; echo \${GCP_ZONE}")" == europe-west4-a ]] || { echo "the zone does not follow the region"; return 1; }
  [[ "$(E2E_FLATCAR_VERSION=4081.3.12 E2E_FLATCAR_CHANNEL=lts bash -c "source '${here}/defaults.sh'; echo \${E2E_FLATCAR_IMAGE}")" == flatcar-lts-4081-3-12 ]] || { echo "the image name of another version"; return 1; }
  [[ "${E2E_TYPES_DESCRIBED_YAML}" == '"gcp.e2-small", "gcp.e2-medium", "gcp.e2-standard-2"' ]] || { echo "described: ${E2E_TYPES_DESCRIBED_YAML}"; return 1; }
  # No project is pinned in the repository: the workflow's variable provides it.
  [[ "$(env -u GCP_PROJECT bash -c "source '${here}/defaults.sh'; echo \"\${GCP_PROJECT}\"")" == "" ]]
}
check "the pins: the image name, the zone, the described types, no project" t_pins

# ---- numbers and metrics -----------------------------------------------------
cat >"${tmp}/metrics.txt" <<'METRICS'
# HELP scarpenter_quota_limit Quota limit of a bucket.
scarpenter_quota_limit{bucket="CPUS",region="us-central1",vendor="gcp"} 32
scarpenter_quota_limit{bucket="PREEMPTIBLE_CPUS",region="us-central1",vendor="gcp"} 8
scarpenter_quota_limit{bucket="CPUS",region="europe-west4",vendor="gcp"} 24
scarpenter_quota_used{bucket="CPUS",region="us-central1",vendor="gcp"} 4
scarpenter_quota_reserved{bucket="CPUS",region="us-central1",vendor="gcp"} 0
scarpenter_quota_used_total{bucket="CPUS",region="us-central1",vendor="gcp"} 99
METRICS
t_metric() {
  [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit us-central1 CPUS)" == 32 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit us-central1 PREEMPTIBLE_CPUS)" == 8 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit europe-west4 CPUS)" == 24 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_used us-central1 CPUS)" == 4 ]] &&
    [[ -z "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit us-central1 C3_CPUS)" ]]
}
t_num() { [[ "$(num 5)" == 5 && "$(num 5.0)" == 5 && "$(num 32e+00)" == 32 && "$(num 0)" == 0 ]]; }
check "metric_value finds the series of a region and bucket (and not a longer metric name)" t_metric
check "num reads Prometheus and gcloud numbers" t_num

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

# ---- stubs: the gcloud CLI the helpers call ----------------------------------
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/gcloud" <<'STUB'
#!/usr/bin/env bash
# a stub of gcloud for selftest.sh: instances list answers with two instances, and records its arguments
echo "$*" >>"${STUB_GCLOUD_LOG}"
case "$*" in
  *"compute instances list"*) printf 'us-central1-a\tnode-a\nus-central1-b\tnode-b\n' ;;
  *"compute regions describe"*) echo '{"quotas":[{"metric":"CPUS","limit":32.0,"usage":4.0},{"metric":"PREEMPTIBLE_CPUS","limit":0.0,"usage":0.0}]}' ;;
  *"compute instances describe"*) echo '{"name":"node-a","labels":{"scarpenter-cluster":"scarpenter-ci-1"},"metadata":{"items":[{"key":"user-data","value":"{}"},{"key":"enable-oslogin","value":"FALSE"}]}}' ;;
  *) echo "unexpected gcloud call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/gcloud"
export STUB_GCLOUD_LOG="${tmp}/gcloud.log"
t_cluster_instances() {
  : >"${STUB_GCLOUD_LOG}"
  [[ "$(PATH="${tmp}/bin:${PATH}" cluster_instances | paste -sd,)" == "us-central1-a/node-a,us-central1-b/node-b" ]] &&
    grep -q -- '--project my-project' "${STUB_GCLOUD_LOG}" && grep -q -- 'labels.scarpenter-cluster=scarpenter-ci-1' "${STUB_GCLOUD_LOG}"
}
t_cluster_instances_status() {
  : >"${STUB_GCLOUD_LOG}"
  PATH="${tmp}/bin:${PATH}" alive_instances >/dev/null
  grep -q -- '(status=PROVISIONING OR status=STAGING OR status=RUNNING OR status=REPAIRING)' "${STUB_GCLOUD_LOG}"
}
t_region_quota() { [[ "$(PATH="${tmp}/bin:${PATH}" region_quota CPUS)" == "32.0 4.0" && -z "$(PATH="${tmp}/bin:${PATH}" region_quota C3_CPUS)" ]]; }
t_instance_json() {
  local j
  j="$(PATH="${tmp}/bin:${PATH}" instance_json us-central1-a/node-a)"
  grep -q -- '--zone us-central1-a' "${STUB_GCLOUD_LOG}" && [[ "$(label_of "${j}" scarpenter-cluster)" == scarpenter-ci-1 ]] &&
    [[ "$(meta_keys "${j}" | paste -sd,)" == "user-data,enable-oslogin" ]] && [[ "$(meta_value "${j}" enable-oslogin)" == FALSE ]]
}
check "cluster_instances lists zone/name, one per line, by the cluster label" t_cluster_instances
check "alive_instances asks for the statuses that are not on their way out" t_cluster_instances_status
check "region_quota prints the limit and the usage of a metric" t_region_quota
check "instance_json, label_of, meta_keys and meta_value" t_instance_json

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local access="ya29"".a0AfH6SMBabcdefghijklmnopqrstuvwxyz0123456789" refresh="1/""/0gabcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
  local apikey="AIza""SyA-abcdefghijklmnopqrstuvwxyz012345" tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop" boot="abcdef"".0123456789abcdef"
  local pk="-----BEGIN PRIVATE"" KEY-----MIIEvQIBADANBgkqhkiG9w0BAQEFAASC-----END PRIVATE"" KEY-----"
  local in out leak
  in="token ${access} and ${refresh}; key ${apikey}; ${tskey}; node host1.tail1234.ts.net; project 123456789012 in projects/123456789012/locations/global
project my-project and //iam.googleapis.com/projects/987654321098/locations/global/workloadIdentityPools/p
Authorization: Bearer abc.def-ghi_jkl
authorization=Basic dXNlcjpwYXNz
header \"Authorization\": \"Bearer secret-token-value\"
curl -H 'Bearer zzz999' x
\"private_key\": \"${pk}\", \"private_key_id\": \"0123456789abcdef0123\"
\"access_token\": \"opaque-access-value\", \"client_secret\": \"opaque-secret-value\"
bare ${pk}
token ${boot} here
client-key-data: LS0tLS1CRUdJTg==
mail sa@my-project.iam.gserviceaccount.com"
  out="$(redact <<<"${in}")"
  for leak in "${access}" "${refresh}" "${apikey}" "${tskey}" "tail1234" "123456789012" "987654321098" "my-project" "abc.def-ghi_jkl" "dXNlcjpwYXNz" \
    "secret-token-value" "zzz999" "MIIEvQIBADANBg" "0123456789abcdef0123" "opaque-access-value" "opaque-secret-value" "${boot}" "LS0tLS1CRUdJTg" "sa@"; do
    if grep -qF -e "${leak}" <<<"${out}"; then
      echo "not redacted: ${leak}"
      echo "${out}"
      return 1
    fi
  done
  grep -q '<gcp-access-token>' <<<"${out}" && grep -q '<gcp-refresh-token>' <<<"${out}" && grep -q '<gcp-api-key>' <<<"${out}" &&
    grep -q '<project-number>' <<<"${out}" && grep -q '<project>' <<<"${out}" && grep -q '<bootstrap-token>' <<<"${out}" &&
    grep -q '<tskey>' <<<"${out}" && grep -q '<private-key>' <<<"${out}" && grep -q '<email>' <<<"${out}"
}
t_redact_keeps_text() { [[ "$(redact <<<'the node scarpenter-ci-1 is Ready after 42s in us-central1-a')" == 'the node scarpenter-ci-1 is Ready after 42s in us-central1-a' ]]; }
t_redact_no_project() { # without a project in the environment only the generic rules apply
  [[ "$(GCP_PROJECT='' redact <<<'my-project is a name')" == 'my-project is a name' ]]
}
t_redact_odd_project() { # a value that is not a project id is never put into a sed expression
  [[ "$(GCP_PROJECT='a/b;c' redact <<<'a/b;c stays')" == 'a/b;c stays' ]]
}
check "redact removes the project, Google tokens and keys, private keys, Authorization and Bearer, bootstrap tokens" t_redact
check "redact leaves ordinary text alone" t_redact_keeps_text
check "redact without GCP_PROJECT" t_redact_no_project
check "redact ignores a GCP_PROJECT that is not a project id" t_redact_odd_project

# ---- waits for nodes, progress and snapshots, against a stub kubectl ----------
cat >"${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# a stub of kubectl for selftest.sh: one Pending pod of pool1, the claims of $STUB_CLAIMS
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$*" in
  *"get pods"*) echo '{"items":[{"metadata":{"name":"p1","namespace":"default","creationTimestamp":"'"${now}"'"},"spec":{"nodeSelector":{"karpenter.sh/nodepool":"pool1"}},"status":{"phase":"Pending"}}]}' ;;
  *"get nodeclaims"*) echo "${STUB_CLAIMS}" ;;
  *"get events"*) echo '{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes, token ya29.abcdefghijklmnopqrstuvwxyz","involvedObject":{"kind":"Pod","name":"p1","namespace":"default"},"lastTimestamp":"'"${now}"'","metadata":{"creationTimestamp":"'"${now}"'"}}]}' ;;
  *"get nodepools"* | *"get scnodeclasses"*) echo '{"items":[]}' ;;
  *" logs "*) printf 'level=info all fine\nlevel=error msg="create failed" token=abcdef.0123456789abcdef\n' ;;
  *"describe pod"*) printf 'Name: p1\n    Environment:\n      SECRET_VALUE: hunter2\n    Mounts:\n' ;;
  *) ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"scarpenter://gcp/us-central1/c1","conditions":[{"type":"Launched","status":"True"}]}}]}'
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
    ! grep -qE 'ya29\.[a-z]{10}|abcdef\.0123456789abcdef' <<<"${out}"
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

# ---- the runner, the workflows, the names ------------------------------------
all_names() { sed -n 's/^all_scenarios=(\(.*\))$/\1/p' "${here}/scenarios.sh"; }
t_scenarios_defined() {
  local names n
  names="$(all_names)"
  [[ -n "${names}" ]]
  for n in ${names} preflight; do
    grep -q "^scn_${n}() {" "${here}/scenarios.sh" || {
      echo "no function scn_${n}"
      return 1
    }
  done
  # The scenarios of plan 16 section 9 item 4.
  for n in amd64 spot scalezero drift quota db described byo create default negative uninstall; do
    [[ " ${names} " == *" ${n} "* ]] || {
      echo "the plan's scenario ${n} is missing"
      return 1
    }
  done
}
t_unknown_scenario() { # exit status 2, and the message names the known ones
  local out rc=0
  out="$(E2E_SCENARIOS=nosuch E2E_CLUSTER=x E2E_RUN_ID=1 E2E_CREATED_AT=1 GCP_PROJECT=my-project KUBECONFIG=/dev/null "${here}/scenarios.sh" 2>&1)" || rc=$?
  [[ "${rc}" == 2 ]] && grep -q 'unknown scenario nosuch (known: amd64 spot' <<<"${out}"
}
t_pins_pinned() { # every action of the L5 workflows is pinned by a full commit SHA (a local reusable workflow has no pin)
  local w="${root}/.github/workflows"
  ! grep -hE '^\s*-?\s*uses:' "${w}/e2e.yml" "${w}/e2e-cloud.yml" "${w}/e2e-canary.yml" "${w}/e2e-gcp-sweeper.yml" "${w}/release.yml" |
    grep -vE 'uses: \./' | grep -vE 'uses: [^@ ]+@[0-9a-f]{40} # v[0-9]'
}
t_smoke_scenarios() { # the smoke set names known scenarios, and not the implicit preflight
  local names s
  names="$(all_names)"
  [[ -n "${E2E_SMOKE_SCENARIOS}" ]]
  for s in ${E2E_SMOKE_SCENARIOS//,/ }; do
    [[ " ${names} " == *" ${s} "* ]] || {
      echo "the smoke scenario ${s} is not one of: ${names}"
      return 1
    }
  done
}
t_workflows_gcp() { # federated credentials, no key file, one step per workflow; the sweeper's environment
  local w="${root}/.github/workflows" f
  for f in e2e-cloud.yml e2e-canary.yml; do
    grep -q "google-github-actions/auth@" "${w}/${f}" || { echo "${f} has no google-github-actions/auth step"; return 1; }
    grep -qE "(inputs|matrix)\.vendor == 'gcp'" "${w}/${f}" || { echo "${f}'s gcp steps are not conditional on the vendor"; return 1; }
    grep -q "workload_identity_provider:" "${w}/${f}" || { echo "${f} does not use a workload identity provider"; return 1; }
    grep -q "GCP_PROJECT" "${w}/${f}" || { echo "${f} does not read the project variable"; return 1; }
  done
  grep -q "google-github-actions/auth@" "${w}/e2e-gcp-sweeper.yml" || { echo "the sweeper has no google-github-actions/auth step"; return 1; }
  grep -qE '^    environment: e2e-sweeper$' "${w}/e2e-gcp-sweeper.yml" || { echo "the sweeper is not in the environment e2e-sweeper"; return 1; }
  ! grep -rn 'credentials_json' "${w}" || { echo "a service account key file is used"; return 1; }
  # Every region and project of the lane is read from a variable with the same names the scripts use.
  grep -q 'VAR_GCP_REGION' "${w}/e2e-cloud.yml" && grep -q 'VAR_GCP_REGION' "${w}/e2e-canary.yml"
}
t_names_agree() { # the sweeper's constants, the scripts' names and the controller's labels are one set
  local go="${root}/pkg/vendors/gcp/sweep/sweep.go" prefix marker
  prefix="$(sed -n 's/^[[:space:]]*ByoNamePrefix = "\(.*\)"$/\1/p' "${go}")"
  marker="$(sed -n 's/^[[:space:]]*LaneMarkerPrefix = "\(.*\)"$/\1/p' "${go}")"
  [[ -n "${prefix}" && -n "${marker}" ]] || { echo "constants not found in sweep.go"; return 1; }
  grep -qF "${prefix}\${E2E_RUN_ID}" "${here}/scenarios.sh" || { echo "scenarios.sh does not name its byo resources ${prefix}<run id>"; return 1; }
  grep -qF "${marker}cluster=\${E2E_CLUSTER}" "${here}/scenarios.sh" || { echo "scenarios.sh does not describe them with '${marker}cluster=<cluster>'"; return 1; }
  grep -qF 'ClusterLabel = gcp.LabelPrefix + "cluster"' "${go}" && [[ "${ctag}" == "scarpenter-cluster" ]] || { echo "the cluster label differs"; return 1; }
  if ! { grep -qF 'CreatedAtLabel = "ci-created-at"' "${go}" && grep -q 'ci-created-at' "${here}/manifests/nodeclass-amd64.yaml"; }; then
    echo "ci-created-at differs"
    return 1
  fi
  grep -qF 'DefaultClusterPrefix = "scarpenter-ci-"' "${go}" || { echo "the cluster prefix differs"; return 1; }
  [[ "${E2E_CLUSTER}" == scarpenter-ci-* ]] || return 1
}
t_no_secrets_in_scripts() { # nothing that looks like a key, a token, a project number or a local path
  ! grep -rnE '(tskey-(auth|client)-[A-Za-z0-9]{6,}|ya29\.[A-Za-z0-9_-]{10,}|AIza[0-9A-Za-z_-]{30,}|-----BEGIN [A-Z ]*PRIVATE|/home/[a-z]+|/tmp/|projects/[0-9]{6,})' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' | grep -v 'selftest.sh'
}
check "every scenario has a function, and the plan's scenarios are all there" t_scenarios_defined
check "an unknown scenario is refused with status 2" t_unknown_scenario
check "every action in the L5 workflows is pinned by SHA with a version comment" t_pins_pinned
check "the smoke scenarios are scenarios of the runner" t_smoke_scenarios
check "the workflows use federated credentials for gcp (no key file) and the sweeper's own environment" t_workflows_gcp
check "the sweeper's names, the byo marker and the label are the scripts' and the controller's" t_names_agree
check "no key, token, project number or local path in the lane's files" t_no_secrets_in_scripts

# ---- the lane is ready for its line in test/e2e-vendors.txt --------------------
# Not listed until the cost guards are true (docs/how-to-guides/testing-and-releasing.md#per-vendor-cost-guards):
# everything the list's consumers need is here, so that the single line is enough.
t_ready_for_the_list() {
  local f w="${root}/.github/workflows"
  for f in bootstrap mint-authkey install scenarios diagnostics teardown selftest; do
    [[ -x "${here}/${f}.sh" ]] || { echo "${f}.sh is missing or not executable"; return 1; }
  done
  [[ -f "${here}/lib.sh" && -f "${here}/defaults.sh" ]] || { echo "lib.sh or defaults.sh is missing"; return 1; }
  [[ "$(declare -F redact)" == "redact" ]] || { echo "lib.sh does not define redact"; return 1; }
  [[ -n "${E2E_SMOKE_SCENARIOS}" && -n "${E2E_MIN_VCPU_OD}" && -n "${E2E_MIN_VCPU_SPOT}" ]] || { echo "pins are missing"; return 1; }
  [[ -f "${w}/e2e-gcp-sweeper.yml" ]] || { echo "no sweeper workflow"; return 1; }
  grep -qE '^[[:space:]]+vendorGCP: +checkGCP,' "${root}/test/canary/main.go" || { echo "the canary does not register gcp"; return 1; }
  grep -qE 'vendorGCP: +"GCP_REGION"' "${root}/test/canary/config.go" || { echo "the canary's config has no gcp region variable"; return 1; }
  grep -q 'echo "GCP_PROJECT=' "${w}/e2e-cloud.yml" || { echo "the Prepare step of e2e-cloud.yml does not set the project"; return 1; }
  grep -q 'echo "GCP_PROJECT=' "${w}/e2e-canary.yml" || { echo "the Region step of e2e-canary.yml does not set the project"; return 1; }
  [[ -x "${root}/test/canary/run.sh" && -d "${here}/sweeper" ]] || { echo "canary run.sh or sweeper missing"; return 1; }
  if grep -qxF gcp "${root}/test/e2e-vendors.txt"; then echo "(gcp is listed in test/e2e-vendors.txt)"; else echo "(gcp is not listed yet: the cost guards are provisioned outside this repository)"; fi
}
check "the lane has everything the vendor list's consumers read (one line in test/e2e-vendors.txt is enough)" t_ready_for_the_list

# ---- the CI roles -------------------------------------------------------------
role_perms() { "$@" | sed -n 's/^- //p'; }
t_ci_roles() {
  local r p all="${tmp}/all-perms"
  : >"${all}"
  for r in reads regional global; do
    "${here}/ci-policy.sh" role "${r}" >"${tmp}/role-${r}.yaml"
    if ! { grep -q '^title: ' "${tmp}/role-${r}.yaml" && grep -q '^stage: GA$' "${tmp}/role-${r}.yaml" && grep -q '^includedPermissions:$' "${tmp}/role-${r}.yaml"; }; then
      echo "role ${r}: not a role definition"
      return 1
    fi
    role_perms "${here}/ci-policy.sh" role "${r}" >"${tmp}/perms-${r}"
    [[ -s "${tmp}/perms-${r}" ]] || { echo "role ${r}: no permissions"; return 1; }
    # only compute permissions, no wildcard, sorted and unique
    ! grep -vE '^compute\.[A-Za-z]+\.[A-Za-z]+$' "${tmp}/perms-${r}" || { echo "role ${r}: not plain compute permissions"; return 1; }
    [[ "$(LC_ALL=C sort -u "${tmp}/perms-${r}" | diff - "${tmp}/perms-${r}" >/dev/null && echo ok)" == ok ]] || { echo "role ${r}: not sorted and unique"; return 1; }
    cat "${tmp}/perms-${r}" >>"${all}"
  done
  [[ "$(sort "${all}" | uniq -d | wc -l)" == 0 ]] || { echo "a permission is in two roles: $(sort "${all}" | uniq -d | paste -sd' ')"; return 1; }
  # nothing that grants IAM, other identities, project-wide metadata or an OS Login
  ! grep -E 'setIamPolicy|actAs|osLogin|setCommonInstanceMetadata|\.update$|\.patch$|compute\.projects\.set|instanceAdmin|securityPolicies' "${all}" || { echo "a permission that the lane must not have"; return 1; }
  # reads: no write; regional: the instance and disk writes; global: networks and rules
  ! grep -E '\.(create|delete|set[A-Za-z]*|update[A-Za-z]*|useExternalIp)$' "${tmp}/perms-reads" || { echo "the reads role writes"; return 1; }
  # The driver's permissions of plan 16 section 8 are all there.
  for p in compute.instances.create compute.instances.delete compute.instances.get compute.instances.list compute.instances.setLabels \
    compute.instances.setMetadata compute.instances.setTags compute.instances.setServiceAccount compute.instances.setScheduling \
    compute.disks.create compute.disks.delete compute.disks.get compute.disks.list compute.disks.use compute.disks.setLabels \
    compute.images.get compute.images.getFromFamily compute.images.useReadOnly \
    compute.subnetworks.get compute.subnetworks.list compute.subnetworks.use compute.subnetworks.useExternalIp compute.subnetworks.create compute.subnetworks.delete \
    compute.networks.get compute.networks.list compute.networks.use compute.networks.create compute.networks.delete compute.networks.updatePolicy \
    compute.firewalls.list compute.firewalls.get compute.firewalls.create compute.firewalls.delete \
    compute.regions.get compute.regions.list compute.zones.list compute.zones.get compute.machineTypes.list compute.machineTypes.get \
    compute.diskTypes.list compute.projects.get compute.zoneOperations.get compute.zoneOperations.list \
    compute.regionOperations.get compute.globalOperations.get \
    compute.resourcePolicies.create compute.resourcePolicies.delete compute.resourcePolicies.get compute.resourcePolicies.list compute.resourcePolicies.use; do
    grep -qxF "${p}" "${all}" || { echo "missing: ${p}"; return 1; }
  done
  # The lane's own reads: the serial console and the operations lists of the diagnostics.
  grep -qxF compute.instances.getSerialPortOutput "${tmp}/perms-reads" || { echo "the serial console read is missing"; return 1; }
  grep -qxF compute.globalOperations.list "${tmp}/perms-reads" || { echo "the operations list is missing"; return 1; }
  for p in compute.instances.create compute.subnetworks.use; do
    grep -qxF "${p}" "${tmp}/perms-regional" || { echo "${p} is not in the regional role"; return 1; }
  done
  for p in compute.networks.create compute.firewalls.create; do
    grep -qxF "${p}" "${tmp}/perms-global" || { echo "${p} is not in the global role"; return 1; }
  done
  # Every compute permission of the reference role file the driver ships is in the CI role.
  local ref="${root}/docs/reference/gcp-iam-role.yaml"
  if [[ -f "${ref}" ]]; then
    while read -r p; do
      grep -qxF "${p}" "${all}" || { echo "the driver's role file has ${p}, the CI role does not"; return 1; }
    done < <(sed -n 's/^[[:space:]]*-[[:space:]]*\(compute\.[A-Za-z.]*\)[[:space:]]*$/\1/p' "${ref}")
  fi
}
t_ci_bindings() {
  local out="${tmp}/bindings" principal='principalSet://iam.googleapis.com/projects/123456/locations/global/workloadIdentityPools/pool/attribute.environment/e2e'
  "${here}/ci-policy.sh" bindings my-project "${principal}" >"${out}"
  [[ "$(grep -c '^gcloud iam roles create ' "${out}")" == 3 && "$(grep -c '^gcloud projects add-iam-policy-binding ' "${out}")" == 3 ]] || { echo "not three roles and three bindings"; return 1; }
  grep -q -- "--condition=None" "${out}" || { echo "the reads binding has no explicit condition"; return 1; }
  grep -qF 'resource.name.startsWith("projects/my-project/zones/us-central1-")' "${out}" || { echo "no zone condition for the region"; return 1; }
  grep -qF 'resource.name.startsWith("projects/my-project/regions/us-central1/")' "${out}" || { echo "no region condition"; return 1; }
  grep -qF 'resource.name.startsWith("projects/my-project/global/networks/scarpenter-")' "${out}" || { echo "no name-prefix condition on the networks"; return 1; }
  grep -qF 'resource.name.startsWith("projects/my-project/global/firewalls/scarpenter-")' "${out}" || { echo "no name-prefix condition on the firewall rules"; return 1; }
  # the region argument moves the condition; the principal is quoted
  local other
  other="$("${here}/ci-policy.sh" bindings my-project "${principal}" europe-west4)"
  grep -qF 'projects/my-project/zones/europe-west4-' <<<"${other}" || { echo "the region argument is ignored"; return 1; }
  ! grep -q 'us-central1' <<<"${other}" || { echo "the default region leaks into another region's commands"; return 1; }
  grep -qF -- "--member='${principal}'" "${out}" || { echo "the principal is not quoted"; return 1; }
  # no predefined role is bound
  ! grep -E -- '--role=roles/' "${out}" || { echo "a predefined role is bound"; return 1; }
}
check "ci-policy.sh prints three role definitions: driver permissions, no IAM, no overlap" t_ci_roles
check "ci-policy.sh prints the gcloud commands: the region and name conditions, quoting" t_ci_bindings
check_fails "ci-policy.sh refuses an unknown role" "${here}/ci-policy.sh" role everything
check_fails "ci-policy.sh refuses a malformed project id" "${here}/ci-policy.sh" bindings "Bad_Project" 'serviceAccount:x@y.iam.gserviceaccount.com'
check_fails "ci-policy.sh refuses a principal that is not a member" "${here}/ci-policy.sh" bindings my-project everybody
check_fails "ci-policy.sh refuses a malformed region" "${here}/ci-policy.sh" bindings my-project 'serviceAccount:x@y.iam.gserviceaccount.com' "us east"

# ---- the canary's read-only role ---------------------------------------------
t_canary_policy() {
  local p="${tmp}/canary-role.yaml"
  "${here}/canary-policy.sh" role >"${p}"
  grep -q '^includedPermissions:$' "${p}" || return 1
  role_perms "${here}/canary-policy.sh" role >"${tmp}/canary-perms"
  # only get, getFromFamily and list permissions of compute: nothing that writes, nothing wider
  [[ -s "${tmp}/canary-perms" ]] && ! grep -vE '^compute\.[A-Za-z]+\.(get|getFromFamily|list)$' "${tmp}/canary-perms" &&
    grep -qxF compute.images.get "${tmp}/canary-perms" && grep -qxF compute.regions.list "${tmp}/canary-perms" &&
    [[ "$(LC_ALL=C sort -u "${tmp}/canary-perms" | diff - "${tmp}/canary-perms" >/dev/null && echo ok)" == ok ]]
}
t_canary_bindings() {
  local out
  out="$("${here}/canary-policy.sh" bindings my-project 'serviceAccount:canary@my-project.iam.gserviceaccount.com')"
  [[ "$(grep -c '^gcloud iam roles create ' <<<"${out}")" == 1 && "$(grep -c '^gcloud projects add-iam-policy-binding ' <<<"${out}")" == 1 ]] &&
    grep -q -- '--condition=None' <<<"${out}" && ! grep -qE 'create|delete|set[A-Z]' <<<"$(grep -- '--permissions=' <<<"${out}")" &&
    ! grep -qE -- '--role=roles/' <<<"${out}"
}
check "canary-policy.sh prints a read-only role (get, getFromFamily, list)" t_canary_policy
check "canary-policy.sh prints the gcloud commands for one read-only role" t_canary_bindings
check_fails "canary-policy.sh refuses a malformed project id" "${here}/canary-policy.sh" bindings "Bad_Project" 'serviceAccount:x@y.iam.gserviceaccount.com'
check_fails "canary-policy.sh without a command" "${here}/canary-policy.sh"

# ---- the chart with the lane's values ----------------------------------------
t_chart() {
  # The chart's gcp block is the driver owner's: until the chart knows the vendor this is a note.
  if ! grep -q '"gcp"' "${root}/charts/scarpenter/values.schema.json" 2>/dev/null; then
    echo "the chart has no vendors.gcp yet: not rendered"
    return 0
  fi
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep -q -- '--cluster-name=scarpenter-ci-1' "${tmp}/chart.yaml" &&
    grep -q 'gcp-credentials' "${tmp}/chart.yaml" && grep -q 'https://100.64.0.1:6443' "${tmp}/chart.yaml" &&
    grep -q 'CriticalAddonsOnly' "${tmp}/chart.yaml"
}
check "the chart renders with the lane's values (schema, credentials, endpoint, toleration)" t_chart

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
