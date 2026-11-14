#!/usr/bin/env bash
# The parts of the L5 lane (OCI) that need no OCI tenancy, no Tailscale and no cluster: the
# helpers of lib.sh against fixtures and stubs of the oci CLI, the session script against a stub of
# curl, the consistency of the scenario list, the workflows, the sweeper's names and the lane's pins,
# the CI and canary policies, the chart rendered with the lane's values, and that the lane is
# ready to be listed in test/e2e-vendors.txt. The manifests are checked against an API server by
# manifests_test.go (go test ./test/e2e-oci/). Run through `make test-e2e-oci-dryrun`.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export E2E_OUT="${tmp}/out"
export OCI_COMPARTMENT=ocid1.compartment.oc1..aaaaexamplecompartment OCI_TENANCY=ocid1.tenancy.oc1..aaaaexampletenancy
mkdir -p "${E2E_OUT}"
# shellcheck source=test/e2e-oci/lib.sh
source "${here}/lib.sh"

failures=0
# grep_all <file> <pattern>...: every pattern (a basic regular expression) matches a line of the file.
grep_all() {
  local f="$1" p
  shift
  for p in "$@"; do
    grep -q -e "${p}" "${f}" || return 1
  done
}
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
export E2E_BYO_SUBNET=ocid1.subnet.oc1.eu-frankfurt-1.aaaaexamplesubnet E2E_BYO_NSG=ocid1.networksecuritygroup.oc1.eu-frankfurt-1.aaaaexamplegroup
export OCI_IDENTITY_DOMAIN_URL=https://idcs-0123456789abcdef0123456789abcdef.identity.oraclecloud.com OCI_CLIENT_ID=client-1
t_render() {
  local out
  out="$(NAME=probe1 POOL=pool1 TYPE=oci.VM.Standard.E5.Flex.1-4 CAPACITY=spot render "${here}/manifests/probe.yaml")"
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
for f in nodeclass-amd64 nodeclass-arm64 nodeclass-drift nodeclass-create nodeclass-use nodeclass-negative \
  nodepools nodepool-arm64 nodepool-use nodepool-create described; do
  check "render ${f}" render "${here}/manifests/${f}.yaml"
done
check "render probe-arm64" env NAME=e2e-arm64 POOL=e2e-arm64 bash -c "source '${here}/lib.sh'; render '${here}/manifests/probe-arm64.yaml'"

# ---- the pins ----------------------------------------------------------------
t_pins() {
  [[ "${E2E_FLATCAR_IMAGE_AMD64}" == "flatcar-stable-4757.2.3-amd64" && "${E2E_FLATCAR_IMAGE_ARM64}" == "flatcar-stable-4757.2.3-arm64" ]] ||
    { echo "images ${E2E_FLATCAR_IMAGE_AMD64} ${E2E_FLATCAR_IMAGE_ARM64}"; return 1; }
  [[ "$(E2E_FLATCAR_VERSION=4800.1.0 E2E_FLATCAR_CHANNEL=beta bash -c "source '${here}/defaults.sh'; echo \${E2E_FLATCAR_IMAGE_AMD64}")" == flatcar-beta-4800.1.0-amd64 ]] ||
    { echo "the image name of another version"; return 1; }
  [[ "$(env -u OCI_HOME_REGION OCI_REGION=us-ashburn-1 bash -c "source '${here}/defaults.sh'; echo \${OCI_HOME_REGION}")" == us-ashburn-1 ]] ||
    { echo "the home region does not follow the region"; return 1; }
  [[ "${E2E_TYPES_DESCRIBED_YAML}" == '"oci.VM.Standard.E5.Flex.1-4", "oci.VM.Standard.E5.Flex.1-8", "oci.VM.Standard.E5.Flex.2-8"' ]] ||
    { echo "described: ${E2E_TYPES_DESCRIBED_YAML}"; return 1; }
  [[ "${E2E_TYPES_ARM64_YAML}" == '"oci.VM.Standard.A1.Flex.1-2", "oci.VM.Standard.A2.Flex.1-8"' ]] || { echo "arm64: ${E2E_TYPES_ARM64_YAML}"; return 1; }
  # No compartment, tenancy, identity domain or client is pinned in the repository: the workflow's variables provide them.
  [[ "$(env -u OCI_COMPARTMENT -u OCI_TENANCY -u OCI_IDENTITY_DOMAIN_URL -u OCI_CLIENT_ID bash -c "source '${here}/defaults.sh'; echo \"\${OCI_COMPARTMENT}\${OCI_TENANCY}\${OCI_IDENTITY_DOMAIN_URL}\${OCI_CLIENT_ID}\"")" == "" ]]
}
check "the pins: the image names, the home region, the described types, no account" t_pins

# ---- numbers and metrics -----------------------------------------------------
cat >"${tmp}/metrics.txt" <<'METRICS'
# HELP scarpenter_quota_limit Quota limit of a bucket.
scarpenter_quota_limit{bucket="standard-e5-core-count",region="eu-frankfurt-1",vendor="oci"} 32
scarpenter_quota_limit{bucket="standard-e5-memory-count",region="eu-frankfurt-1",vendor="oci"} 512
scarpenter_quota_limit{bucket="standard-e5-core-count",region="us-ashburn-1",vendor="oci"} 24
scarpenter_quota_used{bucket="standard-e5-core-count",region="eu-frankfurt-1",vendor="oci"} 4
scarpenter_quota_reserved{bucket="standard-e5-core-count",region="eu-frankfurt-1",vendor="oci"} 0
scarpenter_quota_used_total{bucket="standard-e5-core-count",region="eu-frankfurt-1",vendor="oci"} 99
METRICS
t_metric() {
  [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit eu-frankfurt-1 standard-e5-core-count)" == 32 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit eu-frankfurt-1 standard-e5-memory-count)" == 512 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit us-ashburn-1 standard-e5-core-count)" == 24 ]] &&
    [[ "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_used eu-frankfurt-1 standard-e5-core-count)" == 4 ]] &&
    [[ -z "$(metric_value "${tmp}/metrics.txt" scarpenter_quota_limit eu-frankfurt-1 standard-a1-core-count)" ]]
}
t_num() { [[ "$(num 5)" == 5 && "$(num 5.0)" == 5 && "$(num 32e+00)" == 32 && "$(num 0)" == 0 ]]; }
check "metric_value finds the series of a region and bucket (and not a longer metric name)" t_metric
check "num reads Prometheus and CLI numbers" t_num

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

# ---- stubs: the oci CLI the helpers call -------------------------------------
mkdir -p "${tmp}/bin"
cat >"${tmp}/bin/oci" <<'STUB'
#!/usr/bin/env bash
# a stub of the oci CLI for selftest.sh: answers with fixtures and records its arguments
echo "$*" >>"${STUB_OCI_LOG}"
case "$*" in
  *"compute instance list-vnics"*) echo '{"data":[{"is-primary":false,"public-ip":"1.1.1.1"},{"is-primary":true,"public-ip":"203.0.113.7","nsg-ids":["ocid1.networksecuritygroup.oc1.eu-frankfurt-1.aaaa"],"subnet-id":"ocid1.subnet.oc1.eu-frankfurt-1.aaaa"}]}' ;;
  *"compute instance list"*)
    echo '{"data":[
      {"id":"ocid1.instance.oc1.eu-frankfurt-1.a","lifecycle-state":"RUNNING","freeform-tags":{"scarpenter-cluster":"scarpenter-ci-1","scarpenter-nodeclaim":"c1"}},
      {"id":"ocid1.instance.oc1.eu-frankfurt-1.b","lifecycle-state":"TERMINATING","freeform-tags":{"scarpenter-cluster":"scarpenter-ci-1"}},
      {"id":"ocid1.instance.oc1.eu-frankfurt-1.c","lifecycle-state":"TERMINATED","freeform-tags":{"scarpenter-cluster":"scarpenter-ci-1"}},
      {"id":"ocid1.instance.oc1.eu-frankfurt-1.d","lifecycle-state":"RUNNING","freeform-tags":{"scarpenter-cluster":"scarpenter-ci-2"}},
      {"id":"ocid1.instance.oc1.eu-frankfurt-1.e","lifecycle-state":"PROVISIONING","freeform-tags":{}}]}' ;;
  *"compute instance get"*) echo '{"data":{"id":"ocid1.instance.oc1.eu-frankfurt-1.a","availability-domain":"aBcD:EU-FRANKFURT-1-AD-2","lifecycle-state":"RUNNING","freeform-tags":{"scarpenter-cluster":"scarpenter-ci-1"},"metadata":{"user_data":"e30=","other":"x"},"shape":"VM.Standard.E5.Flex","shape-config":{"ocpus":1.0,"memory-in-gbs":4.0}}}' ;;
  *"iam availability-domain list"*) echo '{"data":[{"name":"aBcD:EU-FRANKFURT-1-AD-1"},{"name":"aBcD:EU-FRANKFURT-1-AD-2"}]}' ;;
  *"limits definition list"*) echo '{"data":[{"name":"standard-e5-core-count","scope-type":"AD"},{"name":"regional-thing-count","scope-type":"REGION"}]}' ;;
  *"limits resource-availability get"*"--availability-domain aBcD:EU-FRANKFURT-1-AD-1"*) echo '{"data":{"used":2,"available":6}}' ;;
  *"limits resource-availability get"*"--availability-domain aBcD:EU-FRANKFURT-1-AD-2"*) echo '{"data":{"used":3,"available":9}}' ;;
  *"limits resource-availability get"*"--limit-name regional-thing-count"*) echo '{"data":{"used":1,"available":4}}' ;;
  *"compute boot-volume-attachment list"*) echo '{"data":[{"boot-volume-id":"ocid1.bootvolume.oc1.eu-frankfurt-1.v1"}]}' ;;
  *"bv boot-volume get"*) echo '{"data":{"id":"ocid1.bootvolume.oc1.eu-frankfurt-1.v1","size-in-gbs":60,"vpus-per-gb":10,"freeform-tags":{}}}' ;;
  *"bv boot-volume list"*"--availability-domain aBcD:EU-FRANKFURT-1-AD-1"*)
    echo '{"data":[
      {"id":"ocid1.bootvolume.oc1.eu-frankfurt-1.new","lifecycle-state":"AVAILABLE","time-created":"2027-01-15T08:00:00.123000+00:00"},
      {"id":"ocid1.bootvolume.oc1.eu-frankfurt-1.old","lifecycle-state":"AVAILABLE","time-created":"2020-01-01T00:00:00.000000+00:00"},
      {"id":"ocid1.bootvolume.oc1.eu-frankfurt-1.gone","lifecycle-state":"TERMINATED","time-created":"2027-01-15T08:00:00.000000+00:00"}]}' ;;
  *"bv boot-volume list"*) ;; # an empty list prints nothing
  *) echo "unexpected oci call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/oci"
export STUB_OCI_LOG="${tmp}/oci.log"
withstub() { PATH="${tmp}/bin:${PATH}" "$@"; }
# shellcheck disable=SC2329  # called through check
t_cluster_instances() {
  : >"${STUB_OCI_LOG}"
  [[ "$(withstub cluster_instances | paste -sd,)" == "ocid1.instance.oc1.eu-frankfurt-1.a,ocid1.instance.oc1.eu-frankfurt-1.b" ]] &&
    grep -q -- "--compartment-id ${OCI_COMPARTMENT}" "${STUB_OCI_LOG}" && grep -q -- '--all' "${STUB_OCI_LOG}"
}
t_cluster_instances_state() {
  [[ "$(withstub cluster_instances TERMINATED | paste -sd,)" == "ocid1.instance.oc1.eu-frankfurt-1.c" ]] &&
    [[ "$(withstub alive_instances | paste -sd,)" == "ocid1.instance.oc1.eu-frankfurt-1.a" ]]
}
t_region_quota() {
  # AD scoped: used 2+3, limit = used + the largest availability (9); region scoped: one read.
  [[ "$(withstub region_quota standard-e5-core-count)" == "14 5" ]] && [[ "$(withstub region_quota regional-thing-count)" == "5 1" ]]
}
t_instance_json() {
  local j
  j="$(withstub instance_json ocid1.instance.oc1.eu-frankfurt-1.a)"
  grep -q -- '--instance-id ocid1.instance.oc1.eu-frankfurt-1.a' "${STUB_OCI_LOG}" && [[ "$(tag_of "${j}" scarpenter-cluster)" == scarpenter-ci-1 ]] &&
    [[ "$(meta_keys "${j}" | paste -sd,)" == "other,user_data" ]]
}
t_vnic() { [[ "$(withstub vnic_json x | jq -r '."public-ip"')" == 203.0.113.7 ]]; }
t_zones() {
  [[ "$(withstub availability_domains | paste -sd,)" == "aBcD:EU-FRANKFURT-1-AD-1,aBcD:EU-FRANKFURT-1-AD-2" ]] && [[ "$(zone_name aBcD:EU-FRANKFURT-1-AD-2)" == EU-FRANKFURT-1-AD-2 ]] &&
    [[ "$(zone_name EU-FRANKFURT-1-AD-2)" == EU-FRANKFURT-1-AD-2 ]]
}
t_boot_volumes() {
  # the run began at 2027-01-15 00:00 UTC (1800000000 is 2027-01-15 08:00:00 UTC: the stub's "new" is exactly then)
  [[ "$(E2E_CREATED_AT=1800000000 withstub new_boot_volumes | sort -u | paste -sd,)" == "ocid1.bootvolume.oc1.eu-frankfurt-1.new" ]] &&
    [[ "$(E2E_CREATED_AT=1900000000 withstub new_boot_volumes | paste -sd,)" == "" ]]
}
t_boot_volume_of() { [[ "$(withstub boot_volume_of "$(withstub instance_json ocid1.instance.oc1.eu-frankfurt-1.a)" | jq -r '."size-in-gbs"')" == 60 ]]; }
check "cluster_instances lists the OCIDs of the cluster's instances that are not terminated" t_cluster_instances
check "cluster_instances takes states, and alive_instances asks for those that are not on their way out" t_cluster_instances_state
check "region_quota: the usage is the sum over the availability domains, the limit adds the largest availability" t_region_quota
check "instance_json, tag_of and meta_keys" t_instance_json
check "vnic_json is the primary VNIC" t_vnic
check "availability_domains and zone_name" t_zones
check "new_boot_volumes: the volumes created since the run began that are not terminated" t_boot_volumes
check "boot_volume_of follows the attachment" t_boot_volume_of

# ---- redaction ---------------------------------------------------------------
# The test strings are assembled from parts: they must not look like real credentials.
t_redact() {
  local tskey="tskey-auth-""kABCDEF123456CNTRL-abcdefghijklmnop" boot="abcdef"".0123456789abcdef"
  local jwt="eyJ""hbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlYWJj" fp="aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"
  local pk="-----BEGIN PRIVATE"" KEY-----MIIEvQIBADANBgkqhkiG9w0BAQEFAASC-----END PRIVATE"" KEY-----"
  local in out leak
  in="${tskey}; node host1.tail1234.ts.net; tenancy ocid1.tenancy.oc1..aaaaexampletenancy user ocid1.user.oc1..aaaaexampleuser
group ocid1.group.oc1..aaaaexamplegroup compartment ocid1.compartment.oc1..aaaaexamplecompartment
instance ocid1.instance.oc1.eu-frankfurt-1.aaaaexampleinstance
domain https://idcs-0123456789abcdef0123456789abcdef.identity.oraclecloud.com/oauth2/v1/token
Authorization: Bearer abc.def-ghi_jkl
authorization=Basic dXNlcjpwYXNz
Authorization: Signature version=\"1\",keyId=\"ST\$${jwt}\",algorithm=\"rsa-sha256\",signature=\"c2lnbmF0dXJl\"
curl -H 'Bearer zzz999' x
\"client_secret\": \"opaque-secret-value\", \"security_token\": \"opaque-session-value\"
fingerprint ${fp}
bare ${pk}
token ${boot} here
client-key-data: LS0tLS1CRUdJTg==
mail ci@example.com"
  out="$(redact <<<"${in}")"
  for leak in "${tskey}" "tail1234" "aaaaexampletenancy" "aaaaexampleuser" "aaaaexamplegroup" "aaaaexamplecompartment" "0123456789abcdef0123456789abcdef" \
    "abc.def-ghi_jkl" "dXNlcjpwYXNz" "zzz999" "${jwt}" "c2lnbmF0dXJl" "opaque-secret-value" "opaque-session-value" "${fp}" "MIIEvQIBADANBg" \
    "${boot}" "LS0tLS1CRUdJTg" "ci@"; do
    if grep -qF -e "${leak}" <<<"${out}"; then
      echo "not redacted: ${leak}"
      echo "${out}"
      return 1
    fi
  done
  # The OCID of an instance is what a failure is read by: it stays.
  grep -qF 'ocid1.instance.oc1.eu-frankfurt-1.aaaaexampleinstance' <<<"${out}" && grep -q '<ocid-tenancy>' <<<"${out}" &&
    grep -q '<ocid-compartment>' <<<"${out}" && grep -q '<identity-domain>' <<<"${out}" && grep -q '<fingerprint>' <<<"${out}" &&
    grep -q '<tskey>' <<<"${out}" && grep -q '<private-key>' <<<"${out}" && grep -q '<email>' <<<"${out}" && grep -q '<bootstrap-token>' <<<"${out}"
}
t_redact_keeps_text() { [[ "$(redact <<<'the node scarpenter-ci-1 is Ready after 42s in EU-FRANKFURT-1-AD-1')" == 'the node scarpenter-ci-1 is Ready after 42s in EU-FRANKFURT-1-AD-1' ]]; }
t_redact_literals() { # the lane's own OCIDs are replaced even in a shape no rule knows
  [[ "$(OCI_COMPARTMENT=ocid1.thing.oc1..x1 OCI_TENANCY=ocid1.tenancy.oc1..y2 redact <<<'in ocid1.thing.oc1..x1 and ocid1.thing.oc1..x12')" == 'in <ocid> and <ocid>2' ]]
}
t_redact_odd_value() { # a value that is not an OCID is never put into a sed expression
  [[ "$(OCI_COMPARTMENT='a/b;c' redact <<<'a/b;c stays')" == 'a/b;c stays' ]]
}
check "redact removes tenancy, compartment and user OCIDs, the identity domain, tokens, signatures, fingerprints and keys" t_redact
check "redact leaves ordinary text alone" t_redact_keeps_text
check "redact replaces the lane's own compartment and tenancy" t_redact_literals
check "redact ignores a compartment that is not an OCID" t_redact_odd_value

# ---- waits for nodes, progress and snapshots, against a stub kubectl ----------
cat >"${tmp}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
# a stub of kubectl for selftest.sh: one Pending pod of pool1, the claims of $STUB_CLAIMS
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$*" in
  *"get pods"*) echo '{"items":[{"metadata":{"name":"p1","namespace":"default","creationTimestamp":"'"${now}"'"},"spec":{"nodeSelector":{"karpenter.sh/nodepool":"pool1"}},"status":{"phase":"Pending"}}]}' ;;
  *"get nodeclaims"*) echo "${STUB_CLAIMS}" ;;
  *"get events"*) echo '{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes, token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlYWJj","involvedObject":{"kind":"Pod","name":"p1","namespace":"default"},"lastTimestamp":"'"${now}"'","metadata":{"creationTimestamp":"'"${now}"'"}}]}' ;;
  *"get nodepools"* | *"get scnodeclasses"*) echo '{"items":[]}' ;;
  *" logs "*) printf 'level=info all fine\nlevel=error msg="create failed" token=abcdef.0123456789abcdef\n' ;;
  *"describe pod"*) printf 'Name: p1\n    Environment:\n      SECRET_VALUE: hunter2\n    Mounts:\n' ;;
  *) ;;
esac
STUB
chmod +x "${tmp}/bin/kubectl"
claim_launched='{"items":[{"metadata":{"name":"c1","labels":{"karpenter.sh/nodepool":"pool1"},"creationTimestamp":"2020-01-01T00:00:00Z"},"status":{"providerID":"scarpenter://oci/eu-frankfurt-1/c1","conditions":[{"type":"Launched","status":"True"}]}}]}'
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
    ! grep -qE 'eyJ[a-zA-Z0-9]{10}|abcdef\.0123456789abcdef' <<<"${out}"
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

# ---- the session script: the token exchange against a stub of curl ------------
cat >"${tmp}/bin/curl" <<'STUB'
#!/usr/bin/env bash
# a stub of curl for selftest.sh: the job's token endpoint gives a token, the identity domain's gives a session token
echo "ARGS $*" >>"${STUB_CURL_LOG}"
args="$*"
for a in "$@"; do
  case "${a}" in
    @*) [[ -r "${a#@}" ]] && sed 's/^/HEADER /' "${a#@}" >>"${STUB_CURL_LOG}" ;;
  esac
done
case "${args}" in
  *oauth2/v1/token*) cat >"${STUB_CURL_BODY}"; echo '{"token":"UPST-FROM-THE-DOMAIN"}' ;;
  *audience=*) echo "AUDIENCE ${args##*audience=}" >>"${STUB_CURL_LOG}"; echo '{"value":"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"}' ;;
  *) echo "unexpected curl call: ${args}" >&2; exit 1 ;;
esac
STUB
chmod +x "${tmp}/bin/curl"
export STUB_CURL_LOG="${tmp}/curl.log" STUB_CURL_BODY="${tmp}/curl.body"
session_env() { # the environment of the workflow's session step
  env PATH="${tmp}/bin:${PATH}" ACTIONS_ID_TOKEN_REQUEST_URL='https://actions.example/token?x=1' ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token-value \
    OCI_TENANCY="${OCI_TENANCY}" OCI_REGION=eu-frankfurt-1 OCI_IDENTITY_DOMAIN_URL="${OCI_IDENTITY_DOMAIN_URL}" OCI_CLIENT_ID=client-1 \
    OCI_CLIENT_SECRET=the-client-secret-value OCI_OIDC_AUDIENCE=oci-federation GITHUB_ENV="${tmp}/github-env" E2E_OCI_TOKEN_INTERVAL=1000 "$@"
}
t_session() {
  local d="${tmp}/session"
  : >"${STUB_CURL_LOG}" && : >"${tmp}/github-env"
  session_env "${here}/oci-session.sh" start "${d}" >"${tmp}/session.out"
  [[ -s "${d}/loop.pid" ]] || { echo "no loop"; return 1; }
  [[ "$(stat -c %a "${d}/oidc-token" "${d}/session-token" "${d}/session.pem" "${d}/client-secret" "${d}/config" | sort -u)" == 600 ]] || { echo "modes"; return 1; }
  [[ "$(cat "${d}/session-token")" == UPST-FROM-THE-DOMAIN ]] || { echo "the session token"; return 1; }
  grep_all "${d}/config" '^\[CI\]$' "^tenancy=${OCI_TENANCY}$" "^key_file=${d}/session.pem$" "^security_token_file=${d}/session-token$" || { echo "the config"; cat "${d}/config"; return 1; }
  # What the exchange sent: the token and the public key in the body, the application's credentials as a Basic header file.
  grep_all "${STUB_CURL_BODY}" 'grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Atoken-exchange' 'requested_token_type=urn%3Aoci%3Atoken-type%3Aoci-upst' 'subject_token_type=jwt' 'subject_token=eyJ' 'public_key=MII' || { echo "the body"; cat "${STUB_CURL_BODY}"; return 1; }
  # The body is the form and nothing else: no trailing newline in the public key.
  [[ "$(tail -c1 "${STUB_CURL_BODY}" | od -An -c | tr -d ' ')" != '\n' ]] || { echo "the body ends with a newline"; return 1; }
  grep -q "^HEADER Authorization: Basic $(printf '%s:%s' client-1 the-client-secret-value | base64 -w0)\$" "${STUB_CURL_LOG}" || { echo "the Basic header"; return 1; }
  grep -q '^AUDIENCE oci-federation$' "${STUB_CURL_LOG}" || { echo "the audience"; cat "${STUB_CURL_LOG}"; return 1; }
  # No secret on a command line: not the client secret, not the request token, not the OIDC token.
  ! grep '^ARGS' "${STUB_CURL_LOG}" | grep -qE 'the-client-secret-value|request-token-value|eyJ' || { echo "a secret is on a command line"; return 1; }
  # The tokens are masked in the log of the step; nothing else is printed of them.
  grep_all "${tmp}/session.out" '^::add-mask::eyJ' '^::add-mask::UPST-FROM-THE-DOMAIN$' || { echo "the masks"; return 1; }
  ! grep -v '^::add-mask::' "${tmp}/session.out" | grep -qE 'eyJ|UPST-FROM|the-client-secret' || { echo "a token is printed"; return 1; }
  # The variables of the next steps.
  grep_all "${tmp}/github-env" "^OCI_CLI_CONFIG_FILE=${d}/config$" '^OCI_CLI_PROFILE=CI$' '^OCI_CLI_AUTH=security_token$' "^OCI_TOKEN_FILE=${d}/oidc-token$" "^OCI_CLIENT_SECRET_FILE=${d}/client-secret$" || { echo "GITHUB_ENV"; cat "${tmp}/github-env"; return 1; }
  # stop ends the loop and removes the directory with the key, the tokens and the secret.
  local pid
  pid="$(cat "${d}/loop.pid")"
  "${here}/oci-session.sh" stop "${d}"
  [[ ! -e "${d}" ]] || { echo "the session directory is left"; return 1; }
  ! kill -0 "${pid}" 2>/dev/null || { echo "the loop still runs"; return 1; }
}
t_session_refuses() {
  ! (env -u OCI_CLIENT_SECRET PATH="${tmp}/bin:${PATH}" OCI_TENANCY=x OCI_REGION=r OCI_IDENTITY_DOMAIN_URL=https://x OCI_CLIENT_ID=c OCI_OIDC_AUDIENCE=a "${here}/oci-session.sh" once "${tmp}/s2") &&
    ! session_env OCI_IDENTITY_DOMAIN_URL=http://insecure.example "${here}/oci-session.sh" once "${tmp}/s3" &&
    ! "${here}/oci-session.sh" frobnicate "${tmp}/s4"
}
check "oci-session.sh: the exchange, the files, the config, the masks, nothing on a command line" t_session
check "oci-session.sh refuses a missing secret, a plain-http domain and an unknown command" t_session_refuses

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
  # The scenarios of plan 25 section 11 item 4 (no db: no data volumes in v1; no default: no default network).
  for n in amd64 spot arm64 scalezero drift quota described byo create negative uninstall; do
    [[ " ${names} " == *" ${n} "* ]] || {
      echo "the plan's scenario ${n} is missing"
      return 1
    }
  done
  for n in db default; do
    [[ " ${names} " != *" ${n} "* ]] || { echo "the scenario ${n} does not exist on OCI"; return 1; }
  done
}
t_unknown_scenario() { # exit status 2, and the message names the known ones
  local out rc=0
  out="$(E2E_SCENARIOS=nosuch E2E_CLUSTER=x E2E_RUN_ID=1 E2E_CREATED_AT=1 KUBECONFIG=/dev/null "${here}/scenarios.sh" 2>&1)" || rc=$?
  [[ "${rc}" == 2 ]] && grep -q 'unknown scenario nosuch (known: amd64 spot' <<<"${out}"
}
t_pins_pinned() { # every action of the L5 workflows is pinned by a full commit SHA (a local reusable workflow has no pin)
  local w="${root}/.github/workflows"
  ! grep -hE '^\s*-?\s*uses:' "${w}/e2e.yml" "${w}/e2e-cloud.yml" "${w}/e2e-canary.yml" "${w}/e2e-oci-sweeper.yml" "${w}/release.yml" |
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
t_workflows_oci() { # keyless credentials, no key file, a CLI pinned by version, the sweeper's environment
  local w="${root}/.github/workflows" f
  for f in e2e-cloud.yml e2e-canary.yml; do
    grep -qE "(inputs|matrix)\.vendor == 'oci'" "${w}/${f}" || { echo "${f}'s oci steps are not conditional on the vendor"; return 1; }
    grep -q 'oci-session.sh' "${w}/${f}" || { echo "${f} does not run oci-session.sh"; return 1; }
    grep -q 'OCI_COMPARTMENT' "${w}/${f}" || { echo "${f} does not read the compartment variable"; return 1; }
    grep -q 'VAR_OCI_REGION' "${w}/${f}" || { echo "${f} does not read the region variable"; return 1; }
    grep -q 'OCI_CLIENT_SECRET' "${w}/${f}" || { echo "${f} does not read the client secret"; return 1; }
  done
  # The lane installs the CLI of its scripts, pinned by version (the canary and the sweeper are Go programs and need none).
  grep -qE 'pipx install "?oci-cli==[0-9]+\.[0-9]+\.[0-9]+' "${w}/e2e-cloud.yml" || { echo "e2e-cloud.yml does not install a pinned oci CLI"; return 1; }
  grep -q 'oci-session.sh' "${w}/e2e-oci-sweeper.yml" || { echo "the sweeper does not run oci-session.sh"; return 1; }
  grep -qE '^    environment: e2e-sweeper$' "${w}/e2e-oci-sweeper.yml" || { echo "the sweeper is not in the environment e2e-sweeper"; return 1; }
  grep -q 'id-token: write' "${w}/e2e-oci-sweeper.yml" || { echo "the sweeper cannot request the OIDC token"; return 1; }
  # Every job that starts a session ends it (the teardown does in e2e-cloud.yml).
  for f in e2e-canary.yml e2e-oci-sweeper.yml; do
    grep -q 'oci-session.sh stop' "${w}/${f}" || { echo "${f} does not end the session"; return 1; }
  done
  grep -q 'oci-session.sh" stop' "${here}/teardown.sh" || { echo "the teardown does not end the session"; return 1; }
  # The teardown asks for a fresh session first.
  grep_all "${w}/e2e-cloud.yml" 'Teardown' 'again for the teardown (oci)' || { echo "no fresh session before the teardown"; return 1; }
  # No API key of the account is stored anywhere: no private key, fingerprint or user OCID variable.
  ! grep -rnEi 'OCI_(USER|FINGERPRINT|PRIVATE_KEY|KEY_FILE|KEY_CONTENT)|OCI_CLI_(USER|FINGERPRINT|KEY)' "${w}" || { echo "an API key variable is used"; return 1; }
}
t_names_agree() { # the sweeper's constants, the scripts' names and the controller's tags are one set
  local go="${root}/pkg/vendors/oci/sweep/sweep.go" prefix lane
  prefix="$(sed -n 's/^[[:space:]]*ByoNamePrefix = "\(.*\)"$/\1/p' "${go}")"
  lane="$(sed -n 's/^[[:space:]]*LaneTag = "\(.*\)"$/\1/p' "${go}")"
  [[ -n "${prefix}" && -n "${lane}" ]] || { echo "constants not found in sweep.go"; return 1; }
  grep -qF "${prefix}\${E2E_RUN_ID}" "${here}/scenarios.sh" || { echo "scenarios.sh does not name its byo resources ${prefix}<run id>"; return 1; }
  grep -qF "\"${lane}\\\":\\\"\${E2E_CLUSTER}" "${here}/scenarios.sh" || { echo "scenarios.sh does not tag them with ${lane}=<cluster>"; return 1; }
  grep -qF 'ClusterTag = oci.TagPrefix + string(vendors.TagCluster)' "${go}" && [[ "${ctag}" == "scarpenter-cluster" ]] || { echo "the cluster tag differs"; return 1; }
  if ! { grep -qF 'CreatedAtTag = "ci-created-at"' "${go}" && grep -q 'ci-created-at' "${here}/manifests/nodeclass-amd64.yaml"; }; then
    echo "ci-created-at differs"
    return 1
  fi
  grep -qF 'DefaultClusterPrefix = "scarpenter-ci-"' "${go}" || { echo "the cluster prefix differs"; return 1; }
  [[ "${E2E_CLUSTER}" == scarpenter-ci-* ]] || return 1
}
t_no_secrets_in_scripts() { # nothing that looks like a key, a token, a tenancy OCID or a local path
  ! grep -rnE '(tskey-(auth|client)-[A-Za-z0-9]{6,}|eyJ[A-Za-z0-9_-]{20,}\.|-----BEGIN [A-Z ]*PRIVATE|/home/[a-z]+|/tmp/|ocid1\.(tenancy|compartment|user|group)\.oc1\.\.[a-z0-9]{20,}|idcs-[a-f0-9]{16,})' \
    "${here}" --include='*.sh' --include='*.yaml' --include='*.go' | grep -v 'selftest.sh\|manifests_test.go\|ocienv_test.go'
}
check "every scenario has a function, and the plan's scenarios are all there" t_scenarios_defined
check "an unknown scenario is refused with status 2" t_unknown_scenario
check "every action in the L5 workflows is pinned by SHA with a version comment" t_pins_pinned
check "the smoke scenarios are scenarios of the runner" t_smoke_scenarios
check "the workflows use the keyless exchange for oci (a pinned CLI, no API key) and the sweeper's own environment" t_workflows_oci
check "the sweeper's names, the byo tag and the cluster tag are the scripts' and the controller's" t_names_agree
check "no key, token, tenancy OCID, identity domain or local path in the lane's files" t_no_secrets_in_scripts

# ---- the lane is ready for its line in test/e2e-vendors.txt --------------------
# Not listed until the cost guards are true (docs/how-to-guides/testing-and-releasing.md#per-vendor-cost-guards):
# everything the list's consumers need is here, so that the single line is enough.
t_ready_for_the_list() {
  local f w="${root}/.github/workflows"
  for f in bootstrap mint-authkey install scenarios diagnostics teardown selftest oci-session; do
    [[ -x "${here}/${f}.sh" ]] || { echo "${f}.sh is missing or not executable"; return 1; }
  done
  [[ -f "${here}/lib.sh" && -f "${here}/defaults.sh" ]] || { echo "lib.sh or defaults.sh is missing"; return 1; }
  [[ "$(declare -F redact)" == "redact" ]] || { echo "lib.sh does not define redact"; return 1; }
  [[ -n "${E2E_SMOKE_SCENARIOS}" && -n "${E2E_MIN_OCPU}" && -n "${E2E_MIN_MEMORY_GB}" ]] || { echo "pins are missing"; return 1; }
  [[ -f "${w}/e2e-oci-sweeper.yml" ]] || { echo "no sweeper workflow"; return 1; }
  grep -qE '^[[:space:]]+vendorOCI: +checkOCI,' "${root}/test/canary/main.go" || { echo "the canary does not register oci"; return 1; }
  grep -qE 'vendorOCI: +"OCI_REGION"' "${root}/test/canary/config.go" || { echo "the canary's config has no oci region variable"; return 1; }
  grep -q 'echo "OCI_COMPARTMENT=' "${w}/e2e-cloud.yml" || { echo "the Prepare step of e2e-cloud.yml does not set the compartment"; return 1; }
  grep -q 'echo "OCI_COMPARTMENT=' "${w}/e2e-canary.yml" || { echo "the Region step of e2e-canary.yml does not set the compartment"; return 1; }
  [[ -x "${root}/test/canary/run.sh" && -d "${here}/sweeper" ]] || { echo "canary run.sh or sweeper missing"; return 1; }
}
check "the lane has everything the vendor list's consumers read (one line in test/e2e-vendors.txt is enough)" t_ready_for_the_list
t_vendor_list() { # the cost guards are not verified on a live tenancy: the CI cost-guard policy is unmet, so oci stays out of the list
  # Whoever verifies them adds the line, removes the note in defaults.sh and changes this check (plan 25, section 11).
  ! grep -qxF oci "${root}/test/e2e-vendors.txt" || { echo "oci is in test/e2e-vendors.txt, but its cost guards are not verified"; return 1; }
  grep -q 'THIS LANE IS NOT IN test/e2e-vendors.txt' "${here}/defaults.sh" || { echo "defaults.sh does not say why oci is not in the list"; return 1; }
}
check "oci is not in test/e2e-vendors.txt until its cost guards are verified, and defaults.sh lists them" t_vendor_list

# ---- the policies -------------------------------------------------------------
# covered <statement> <file of statements>: the file has the statement, or one with a stronger verb (inspect, read, use, manage)
# for the same resource type and scope.
covered() {
  local verb="${1%% *}" rest="${1#* }" v
  local -a order=(inspect read use manage)
  local i=0
  for v in "${order[@]}"; do
    [[ "${v}" == "${verb}" ]] && break
    i=$((i + 1))
  done
  for v in "${order[@]:${i}}"; do
    grep -qxF "${v} ${rest}" "$2" && return 0
  done
  return 1
}
t_ci_policy() {
  local out="${tmp}/ci-statements" s ref="${root}/docs/reference/oci-iam-policy.txt" line
  "${here}/ci-policy.sh" statements >"${out}"
  [[ -s "${out}" ]] || { echo "no statements"; return 1; }
  # Plain statements of the shape <verb> <resource> in compartment|tenancy, no wildcard, no all-resources.
  ! grep -vE '^(inspect|read|use|manage) [a-z-]+ in (compartment|tenancy)$' "${out}" || { echo "a statement of another shape"; return 1; }
  ! grep -E 'all-resources|users|groups|policies|dynamic-groups|domains|identity|tag-namespaces|tenancy-wide' "${out}" || { echo "a statement the lane must not have"; return 1; }
  # Nothing tenancy-wide that writes.
  ! grep -E '^(use|manage) .* in tenancy$' "${out}" || { echo "a write in the tenancy"; return 1; }
  # Every statement of the driver's policy file is in the CI policy.
  while read -r line; do
    s="$(sed -n 's/^Allow group <group> to \([a-z]*\) \([a-z-]*\) in \(compartment\|tenancy\)\( <compartment>\)\?$/\1 \2 in \3/p' <<<"${line}")"
    [[ -n "${s}" ]] || continue
    covered "${s}" "${out}" || { echo "the driver's policy has '${s}', the CI policy does not"; return 1; }
  done <"${ref}"
  [[ -n "${s}" ]] || { echo "the driver's policy file has no statement the check could read"; return 1; }
  # The lane's own needs beyond the driver's: boot volumes (the sweeper's deletion of a tagged one that a terminate left).
  grep -qxF 'manage volume-family in compartment' "${out}" || { echo "no manage volume-family"; return 1; }
}
t_ci_policy_text() {
  local out="${tmp}/ci-policy" ocid=ocid1.compartment.oc1..aaaaexample
  "${here}/ci-policy.sh" policy "'Default'/'scarpenter-ci'" lane:ci eu-frankfurt-1 >"${out}"
  grep_all "${out}" "^Allow group 'Default'/'scarpenter-ci' to manage instance-family in compartment lane:ci where request.region = 'eu-frankfurt-1'$" "^Allow group 'Default'/'scarpenter-ci' to inspect tenancies in tenancy$" || { echo "the statements"; cat "${out}"; return 1; }
  # A tenancy statement carries no region condition (the subscription list is a home region call); an OCID is `compartment id`.
  ! grep 'in tenancy' "${out}" | grep -q where || { echo "a tenancy statement has a condition"; return 1; }
  "${here}/ci-policy.sh" policy g "${ocid}" us-ashburn-1 | grep -q "in compartment id ${ocid} where request.region = 'us-ashburn-1'$" || { echo "the OCID form and the region"; return 1; }
  ! "${here}/ci-policy.sh" policy g "${ocid}" us-ashburn-1 | grep -q eu-frankfurt-1 || { echo "the default region leaks into another region's policy"; return 1; }
}
t_ci_policy_images() {
  local ocid=ocid1.compartment.oc1..aaaaexample img=ocid1.compartment.oc1..bbbbimages out
  out="$("${here}/ci-policy.sh" policy g "${ocid}" us-ashburn-1 "${img}")"
  grep -qx "Allow group g to read instance-images in compartment id ${img} where request.region = 'us-ashburn-1'" <<<"${out}" || { echo "the image statement"; return 1; }
  [[ "$(grep -c "${img}" <<<"${out}")" == 1 ]] || { echo "the image compartment has more than one statement"; return 1; }
  # none without the argument, with an empty one, or the same compartment
  ! "${here}/ci-policy.sh" policy g "${ocid}" us-ashburn-1 | grep -q "${img}" || { echo "an image statement without an image compartment"; return 1; }
  [[ "$("${here}/ci-policy.sh" policy g "${ocid}" us-ashburn-1 "" | wc -l)" == "$("${here}/ci-policy.sh" policy g "${ocid}" us-ashburn-1 | wc -l)" ]] || { echo "an empty image compartment adds a line"; return 1; }
  [[ "$("${here}/ci-policy.sh" policy g "${ocid}" us-ashburn-1 "${ocid}" | wc -l)" == "$("${here}/ci-policy.sh" policy g "${ocid}" us-ashburn-1 | wc -l)" ]] || { echo "the same compartment adds a line"; return 1; }
}
check "ci-policy.sh adds read instance-images in the image compartment only when it is another one" t_ci_policy_images
check_fails "ci-policy.sh refuses a malformed image compartment" "${here}/ci-policy.sh" policy g comp eu-frankfurt-1 'bad image;'
check "ci-policy.sh prints the driver's statements and the lane's, no IAM, nothing tenancy-wide that writes" t_ci_policy
check "ci-policy.sh prints the policy: the region condition on the compartment statements only, names and OCIDs" t_ci_policy_text
check_fails "ci-policy.sh refuses an unknown command" "${here}/ci-policy.sh" everything
check_fails "ci-policy.sh refuses a malformed group" "${here}/ci-policy.sh" policy 'bad group;' comp
check_fails "ci-policy.sh refuses a malformed compartment" "${here}/ci-policy.sh" policy g 'bad compartment;'
check_fails "ci-policy.sh refuses a malformed region" "${here}/ci-policy.sh" policy g comp "eu frankfurt"

t_canary_policy() {
  local p="${tmp}/canary-statements"
  "${here}/canary-policy.sh" statements >"${p}"
  # only inspect and read: nothing that writes, nothing wider than the canary's calls
  [[ -s "${p}" ]] && ! grep -vE '^(inspect|read) [a-z-]+ in (compartment|tenancy)$' "${p}" &&
    grep -qxF 'inspect tenancies in tenancy' "${p}" && grep -qxF 'read instance-images in compartment' "${p}" &&
    [[ "$(LC_ALL=C sort -u "${p}" | diff - "${p}" >/dev/null && echo ok)" == ok ]]
}
t_canary_policy_text() {
  local out
  out="$("${here}/canary-policy.sh" policy canary-group lane)"
  [[ "$(grep -c '^Allow group canary-group to ' <<<"${out}")" == "$("${here}/canary-policy.sh" statements | wc -l)" ]] &&
    grep -q '^Allow group canary-group to inspect instances in compartment lane$' <<<"${out}" &&
    ! grep -qE 'manage|use |where' <<<"${out}"
}
t_canary_policy_images() {
  local img=ocid1.compartment.oc1..bbbbimages out
  out="$("${here}/canary-policy.sh" policy g lane "${img}")"
  grep -qx "Allow group g to read instance-images in compartment id ${img}" <<<"${out}" || { echo "the image statement"; return 1; }
  [[ "$("${here}/canary-policy.sh" policy g lane | wc -l)" == "$("${here}/canary-policy.sh" policy g lane "" | wc -l)" ]]
}
check "canary-policy.sh adds read instance-images in the image compartment when given" t_canary_policy_images
check_fails "canary-policy.sh refuses a malformed image compartment" "${here}/canary-policy.sh" policy g lane 'bad image;'
check "canary-policy.sh prints read-only statements (inspect, read), sorted and unique" t_canary_policy
check "canary-policy.sh prints the policy for a group and a compartment" t_canary_policy_text
check_fails "canary-policy.sh refuses a malformed group" "${here}/canary-policy.sh" policy 'bad group;' comp
check_fails "canary-policy.sh without a command" "${here}/canary-policy.sh"

# ---- the sweeper and the canary refuse to run without an account ---------------
t_sweeper_setup() {
  local rc=0 out
  out="$(cd "${root}" && env -u OCI_COMPARTMENT -u OCI_TENANCY go run ./test/e2e-oci/sweeper -dry-run 2>&1)" || rc=$?
  # go run reports the program's status 2 as its own 1.
  [[ "${rc}" != 0 ]] && grep -q 'OCI_COMPARTMENT, OCI_TENANCY' <<<"${out}" && grep -q 'exit status 2' <<<"${out}"
}
check "the sweeper refuses to run without the compartment and the tenancy (usage status 2)" t_sweeper_setup

# ---- the chart with the lane's values ----------------------------------------
t_chart() {
  render "${here}/manifests/chart-values.yaml" >"${tmp}/values.yaml"
  helm template scarpenter "${root}/charts/scarpenter" --namespace scarpenter-system --kube-version 1.37.0 \
    -f "${tmp}/values.yaml" --set kubeletClientCA.source=inline --set-string kubeletClientCA.inline=PEM --set image.repository=scarpenter-controller --set image.tag=e2e >"${tmp}/chart.yaml"
  grep_all "${tmp}/chart.yaml" '--cluster-name=scarpenter-ci-1' "--oci-compartment=${OCI_COMPARTMENT}" 'https://100.64.0.1:6443' 'CriticalAddonsOnly' || { echo "the basics"; return 1; }
  # The credentials: the chart's mode none renders no --oci-auth, so the lane's is the only one (no flag is overridden), and no key is mounted.
  local auth
  auth="$(grep -o -- '--oci-auth=[A-Za-z]*' "${tmp}/chart.yaml")"
  [[ "${auth}" == --oci-auth=workloadIdentityFederation ]] || { echo "not exactly one --oci-auth, the federation: ${auth}"; return 1; }
  grep_all "${tmp}/chart.yaml" '--oci-token-file=/var/run/secrets/oci/token' "--oci-identity-domain-url=${OCI_IDENTITY_DOMAIN_URL}" '--oci-client-id=client-1' '--oci-client-secret-file=/etc/scarpenter/oci-client/client-secret' '--oci-region=eu-frankfurt-1' || { echo "the federation flags"; return 1; }
  grep_all "${tmp}/chart.yaml" 'secretName: oci-federated-token' 'secretName: oci-client' || { echo "the secrets"; return 1; }
  ! grep -q -- '--oci-credentials-dir=' "${tmp}/chart.yaml" || { echo "an API key directory is wired"; return 1; }
  # Every flag the lane passes is one the controller has (a typo would be a start-up failure on the first live run).
  local flag
  for flag in oci-auth oci-region oci-token-file oci-identity-domain-url oci-client-id oci-client-secret-file oci-compartment; do
    grep -q "\"${flag}\"" "${root}/pkg/operator/options/options.go" || { echo "the controller has no flag --${flag}"; return 1; }
  done
}
check "the chart renders with the lane's values (the keyless flags alone, the secrets, the compartment, the endpoint)" t_chart

if [[ "${failures}" -ne 0 ]]; then
  echo "selftest: ${failures} failure(s)" >&2
  exit 1
fi
echo "selftest: all passed"
