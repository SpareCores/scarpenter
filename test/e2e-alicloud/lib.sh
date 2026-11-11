#!/usr/bin/env bash
# Helpers of the L5 lane of Alibaba Cloud (e2e over Tailscale): docs/how-to-guides/testing.md#the-l5-lane.
# Source it; it only defines functions and constants. Nothing here is run by `make
# test`: the scripts that use it talk to a real Alibaba Cloud account and Tailscale, and
# only the e2e workflow runs them. `selftest.sh` runs the pure helpers against stubs.
#
# NOTHING IN THIS LANE HAS RUN YET: it was written and validated statically. The facts about
# the `aliyun` CLI (flag spellings, the shape of its JSON, its use of environment
# credentials) and about the ECS and VPC APIs marked "unverified" are settled by the first
# live run.
#
# The environment the workflow (and a developer who runs the lane by hand with
# their own account) provides:
#
#   E2E_CLUSTER             cluster name, scarpenter-ci-<run id>
#   E2E_RUN_ID              the workflow run id (a unique suffix)
#   E2E_CREATED_AT          Unix seconds of the start; tags every resource (sweeper)
#   ALICLOUD_REGION         the region of the run (ap-southeast-1)
#   KUBECONFIG              the k3s cluster
#   E2E_OUT                 directory for logs and the report (default test/e2e-alicloud/.out)
#   ALIBABA_CLOUD_ROLE_ARN, ALIBABA_CLOUD_OIDC_PROVIDER_ARN, ALIBABA_CLOUD_OIDC_TOKEN_FILE
#                           the RAM role and the file with a GitHub OIDC token that the
#                           workflow's credentials step writes (ali_login exchanges them)
#
# Pins (defaults in defaults.sh).

# shellcheck disable=SC2034  # constants used by the scripts that source this file

e2e_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
e2e_root="$(cd "${e2e_dir}/../.." && pwd)"
# shellcheck source=test/lib.sh
source "${e2e_root}/test/lib.sh"
# shellcheck source=test/e2e-alicloud/defaults.sh
source "${e2e_dir}/defaults.sh"

ns="scarpenter-system"
ctag="scarpenter.sparecores.com/cluster"
label_domain="scarpenter.sparecores.com"

# ------------------------------------------------------------ redaction -----

# redact: stdin to stdout without what must not be published (the repository, its job
# logs and its artifacts are public): account IDs (the UID of an Alibaba Cloud account has
# 16 digits), RAM ARNs, tailnet host names, Tailscale auth keys, access key IDs, access key
# secrets and security tokens, JWTs, Authorization and Bearer credentials, kubeconfig key
# material, e-mail addresses and bootstrap tokens. The one place for it: diagnostics.sh
# and the live output of observe.sh use it, test/canary/run.sh pipes the canary through it,
# and selftest.sh tests it. Review a file before sharing it anyway.
read -r -d '' _redact_rules <<'SED' || true
s#(authorization["']?[:=] *["']?)((bearer|basic|token) +)?[^ ,"']+#\1<redacted>#Ig
s#(bearer +)[A-Za-z0-9._~+/=-]+#\1<redacted>#Ig
s#((client-key|client-certificate|certificate-authority)-data: *)[A-Za-z0-9+/=]+#\1<redacted>#g
s#(access_?key_?secret|security_?token|oidc_?token|secret_?access_?key|session_?token)(["']?[:= ]+["']?)[^ ,"'}]+#\1\2<redacted>#Ig
s#acs:[a-z]+::[0-9]+:[A-Za-z0-9_/.*-]+#<acs-arn>#g
s#CAIS[A-Za-z0-9/+=_-]{40,}#<security-token>#g
s/[0-9]{12,20}/<account>/g
s/tskey-[A-Za-z0-9_-]+/<tskey>/g
s/[A-Za-z0-9-]+\.ts\.net/<tailnet-host>/g
s/(LTAI|STS\.)[A-Za-z0-9]{12,40}/<access-key-id>/g
s/eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*/<jwt>/g
s/(ALIBABA_CLOUD_ACCESS_KEY_SECRET|ALIBABA_CLOUD_SECURITY_TOKEN|ALIBABA_CLOUD_ACCESS_KEY_ID)=[^ ]+/\1=<redacted>/g
s/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/<email>/g
s/\b[a-z0-9]{6}\.[a-z0-9]{16}\b/<bootstrap-token>/g
SED
redact() { sed -E -e "${_redact_rules}"; }

# need <VAR>...: the variables must be set and non-empty.
need() {
  local v
  for v in "$@"; do
    [[ -n "${!v:-}" ]] || {
      echo "the variable ${v} is required" >&2
      exit 2
    }
  done
}

now() { date +%s; }

# fail <message>: ends the running scenario (they run in subshells).
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# note <message>: something to read in the report that is not a failure.
note() {
  echo "NOTE: $*"
  [[ -z "${E2E_OUT:-}" ]] || echo "$*" >>"${E2E_OUT}/notes.txt"
}

assert_eq() { # assert_eq <what> <want> <got>
  [[ "$2" == "$3" ]] || fail "$1: want '$2', got '$3'"
  echo "ok: $1 = $2"
}

assert_nonempty() { # assert_nonempty <what> <value>
  [[ -n "$2" && "$2" != "null" ]] || fail "$1 is empty"
  echo "ok: $1 = $2"
}

assert_match() { # assert_match <what> <extended regexp> <got>
  [[ "$3" =~ $2 ]] || fail "$1: '$3' does not match $2"
  echo "ok: $1 = $3"
}

# wait_until <what> <timeout seconds> <command...>: polls every 3 s; on timeout
# prints the last output of the command and ends the scenario.
wait_until() {
  local what="$1" timeout="$2" deadline last
  shift 2
  deadline=$((SECONDS + timeout))
  while true; do
    if last="$("$@" 2>&1)"; then
      echo "ok: ${what} (after $((timeout - (deadline - SECONDS)))s)"
      return 0
    fi
    if [[ "${SECONDS}" -ge "${deadline}" ]]; then
      echo "last output: ${last}" >&2
      fail "timed out after ${timeout}s waiting for ${what}"
    fi
    sleep 3
  done
}

# ----------------------------------------------------- Alibaba Cloud ------

# The calls go through the Alibaba Cloud CLI, `aliyun` (unverified on the runner: the
# workflow must provide it, docs/how-to-guides/testing-and-releasing.md#alibaba-cloud-provisioning).
# Its flags are the API's parameter names (--RegionId, --Tag.1.Key): the CLI turns a
# product and an action into the RPC call and prints the JSON answer. A list parameter is
# one flag per element, numbered (--Tag.1.Key k --Tag.1.Value v), or a JSON array for the
# parameters the API documents so (--InstanceIds '["i-1"]'); both are unverified here.

# ali_login: gives the CLI and the controller's Secret temporary credentials, from the
# RAM role of the run. The workflow's credentials step wrote a GitHub OIDC token to
# ALIBABA_CLOUD_OIDC_TOKEN_FILE (it lives minutes, and nothing refreshes the file); this
# exchanges it once at the STS endpoint (AssumeRoleWithOIDC, an unsigned call, so the token
# goes in the POST body and never in a URL or a command line) for a session of two hours
# and exports ALIBABA_CLOUD_ACCESS_KEY_ID, _ACCESS_KEY_SECRET and _SECURITY_TOKEN, which the
# CLI and the credentials library of the sweeper both read (the CLI's use of them:
# unverified). The result is cached in E2E_OUT (mode 0600) so that the steps of one job
# share it, and exchanged again when the token file is newer than the cache (the
# teardown's re-assume step writes a fresh token). Credentials already in the environment
# (a developer's own) are used as they are.
ali_login() {
  local cache="${E2E_OUT:-.}/ali-sts.env" token resp key secret sectok
  if [[ -n "${ALIBABA_CLOUD_ACCESS_KEY_ID:-}" && -z "${ALIBABA_CLOUD_OIDC_TOKEN_FILE:-}" ]]; then
    return 0
  fi
  if [[ -s "${cache}" && ! "${ALIBABA_CLOUD_OIDC_TOKEN_FILE:-/nonexistent}" -nt "${cache}" ]]; then
    # shellcheck disable=SC1090  # a file this function wrote
    source "${cache}"
    export ALIBABA_CLOUD_ACCESS_KEY_ID ALIBABA_CLOUD_ACCESS_KEY_SECRET ALIBABA_CLOUD_SECURITY_TOKEN
    return 0
  fi
  need ALIBABA_CLOUD_ROLE_ARN ALIBABA_CLOUD_OIDC_PROVIDER_ARN ALIBABA_CLOUD_OIDC_TOKEN_FILE
  token="$(cat "${ALIBABA_CLOUD_OIDC_TOKEN_FILE}")"
  [[ -n "${token}" ]] || {
    echo "the OIDC token file is empty" >&2
    return 1
  }
  echo "::add-mask::${token}" >&2
  # The token goes through the environment of jq, not its arguments (a process list shows those).
  resp="$(OIDC_TOKEN="${token}" jq -rn --arg r "${ALIBABA_CLOUD_ROLE_ARN}" --arg p "${ALIBABA_CLOUD_OIDC_PROVIDER_ARN}" --arg s "scarpenter-ci-${E2E_RUN_ID:-local}" '
      "Action=AssumeRoleWithOIDC&Format=JSON&Version=2015-04-01&DurationSeconds=7200"
      + "&RoleArn=\($r | @uri)&OIDCProviderArn=\($p | @uri)&OIDCToken=\(env.OIDC_TOKEN | @uri)&RoleSessionName=\($s | @uri)"' |
    curl -fsS --retry 3 --data @- "${ALIBABA_CLOUD_STS_ENDPOINT:-https://sts.aliyuncs.com}/")" || {
    echo "could not exchange the OIDC token for a RAM role session (AssumeRoleWithOIDC)" >&2
    return 1
  }
  key="$(jq -er '.Credentials.AccessKeyId' <<<"${resp}")" || return 1
  secret="$(jq -er '.Credentials.AccessKeySecret' <<<"${resp}")" || return 1
  sectok="$(jq -er '.Credentials.SecurityToken' <<<"${resp}")" || {
    echo "the STS answer holds no credentials" >&2
    return 1
  }
  echo "::add-mask::${secret}" >&2
  echo "::add-mask::${sectok}" >&2
  (
    umask 077
    printf 'ALIBABA_CLOUD_ACCESS_KEY_ID=%s\nALIBABA_CLOUD_ACCESS_KEY_SECRET=%s\nALIBABA_CLOUD_SECURITY_TOKEN=%s\n' \
      "${key}" "${secret}" "${sectok}" >"${cache}"
  )
  ALIBABA_CLOUD_ACCESS_KEY_ID="${key}" ALIBABA_CLOUD_ACCESS_KEY_SECRET="${secret}" ALIBABA_CLOUD_SECURITY_TOKEN="${sectok}"
  export ALIBABA_CLOUD_ACCESS_KEY_ID ALIBABA_CLOUD_ACCESS_KEY_SECRET ALIBABA_CLOUD_SECURITY_TOKEN
}

# The CLI of one product in the lane's region: ecs <Action> [flags]. The action comes
# first (`aliyun <product> <Action> --Flag value`). The credentials are read from the
# environment (ali_login); the output is JSON (the CLI's default).
ecs() { aliyun ecs "$1" --RegionId "${ALICLOUD_REGION}" "${@:2}"; }
vpc() { aliyun vpc "$1" --RegionId "${ALICLOUD_REGION}" "${@:2}"; }

# The cluster's tag as the CLI's filter flags.
cluster_tag_args() { printf '%s\n' --Tag.1.Key "${ctag}" --Tag.1.Value "${E2E_CLUSTER}"; }

# cluster_instances: IDs of the cluster's instances that exist. ECS has no "terminated"
# state: a released instance leaves DescribeInstances (how long it lingers in Stopping
# first: unverified), so "gone" and "not listed" are the same here. One page of 100: a lane
# run launches a handful.
cluster_instances() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  ecs DescribeInstances "${tag[@]}" --PageSize 100 | jq -r '.Instances.Instance[]?.InstanceId'
}

# The resources of the run, by the cluster tag (a deployment set takes no tags: the
# driver's marker in its description, "scarpenter.sparecores.com: cluster=<name> ...",
# names the cluster, matched whole so that run 12 is not run 123). Each prints IDs, one per
# line. Whether the tag filter (Tag.N) applies to every one of these calls: unverified.
cluster_security_groups() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  ecs DescribeSecurityGroups "${tag[@]}" --PageSize 100 | jq -r '.SecurityGroups.SecurityGroup[]?.SecurityGroupId'
}
cluster_deployment_sets() {
  ecs DescribeDeploymentSets --PageSize 100 | jq -r --arg c "${E2E_CLUSTER}" '
    .DeploymentSets.DeploymentSet[]? | select((.DeploymentSetDescription // "") | test("cluster=" + $c + "( |$)")) | .DeploymentSetId'
}
cluster_vswitches() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  vpc DescribeVSwitches "${tag[@]}" --PageSize 50 | jq -r '.VSwitches.VSwitch[]?.VSwitchId'
}
cluster_vpcs() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  vpc DescribeVpcs "${tag[@]}" --PageSize 50 | jq -r '.Vpcs.Vpc[]?.VpcId'
}
cluster_disks() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  ecs DescribeDisks "${tag[@]}" --PageSize 100 | jq -r '.Disks.Disk[]?.DiskId'
}
cluster_enis() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  ecs DescribeNetworkInterfaces "${tag[@]}" --MaxResults 100 | jq -r '.NetworkInterfaceSets.NetworkInterfaceSet[]?.NetworkInterfaceId'
}

# instance_json <id>: the DescribeInstances record of one instance (Tags.Tag[], ZoneId,
# InstanceType, SpotStrategy, DeploymentSetId, VpcAttributes, SecurityGroupIds, ...).
instance_json() {
  ecs DescribeInstances --InstanceIds "[\"$1\"]" | jq '.Instances.Instance[0]'
}

# cluster_instances_json: what the diagnostics keep of the cluster's instances.
cluster_instances_json() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  ecs DescribeInstances "${tag[@]}" --PageSize 100 | jq '[.Instances.Instance[]? | {id: .InstanceId, name: .InstanceName,
    type: .InstanceType, status: .Status, zone: .ZoneId, created: .CreationTime, spot: .SpotStrategy,
    deploymentSet: .DeploymentSetId, tags: .Tags.Tag}]'
}

# instance_console <id>: the console output of an instance, decoded (the API answers base64).
instance_console() { ecs GetInstanceConsoleOutput --InstanceId "$1" | jq -r '.ConsoleOutput // empty' | base64 -d; }

# instance_attr <id>: DescribeInstanceAttribute (the status, and the metadata options:
# HttpTokens, HttpEndpoint, HttpPutResponseHopLimit under MetadataOptions; unverified that
# the answer carries them).
instance_attr() { ecs DescribeInstanceAttribute --InstanceId "$1"; }

# instance_disks <id>: the disks of an instance (Disks.Disk[]: Category, PerformanceLevel,
# Size, Type system or data, DeleteWithInstance, Encrypted).
instance_disks() { ecs DescribeDisks --InstanceId "$1" | jq '.Disks.Disk'; }

# tag_of <record json> <tag key>: the value of a tag in a DescribeInstances,
# DescribeSecurityGroups or DescribeDisks record (Tags.Tag[] with TagKey and TagValue).
tag_of() { jq -r --arg k "$2" '(.Tags.Tag // [])[] | select(.TagKey == $k) | .TagValue' <<<"$1"; }

# vcpus_of <instance type without the vendor prefix>: from DescribeInstanceTypes.
vcpus_of() {
  ecs DescribeInstanceTypes --InstanceTypes "[\"$1\"]" | jq -r '.InstanceTypes.InstanceType[0].CpuCoreCount'
}

# account_attr <attribute name>: one value of DescribeAccountAttributes (the vCPU limits and
# usage of the region: max- and used-postpaid-instance-vcpu-count, max- and used-spot-...).
account_attr() {
  ecs DescribeAccountAttributes --AttributeName.1 "$1" |
    jq -r '.AccountAttributeItems.AccountAttributeItem[0].AttributeValues.ValueItem[0].Value // empty'
}

# offered_zones <type without the vendor prefix> [spot]: the zones DescribeAvailableResource
# lists the type in with stock (Status Available), one per line, sorted.
offered_zones() {
  local -a extra=()
  [[ "${2:-}" == spot ]] && extra=(--SpotStrategy SpotAsPriceGo)
  ecs DescribeAvailableResource --DestinationResource InstanceType --IoOptimized optimized \
    --InstanceChargeType PostPaid --InstanceType "$1" "${extra[@]}" |
    jq -r --arg t "$1" '[.AvailableZones.AvailableZone[]? | select(any(.AvailableResources.AvailableResource[]?.SupportedResources.SupportedResource[]?;
      .Value == $t and .Status == "Available")) | .ZoneId] | sort | .[]'
}

# ---------------------------------------------------------- Kubernetes ------

kjson() { kubectl get "$@" -o json; }

# pod_node <pod name> [namespace]
pod_node() { kubectl -n "${2:-default}" get pod "$1" -o jsonpath='{.spec.nodeName}'; }

# claim_of_node <node>: the NodeClaim that registered as the node.
claim_of_node() {
  kjson nodeclaims | jq -r --arg n "$1" '.items[] | select(.status.nodeName == $n) | .metadata.name'
}

# instance_of_claim <claim>: the ECS instance ID from the driver's annotation
# (scarpenter.sparecores.com/instance-ref = <region>/<instance id>).
instance_of_claim() {
  kubectl get nodeclaim "$1" -o json | jq -r --arg k "${label_domain}/instance-ref" '.metadata.annotations[$k] // empty' | sed 's#.*/##'
}

node_label() { # node_label <node> <label>
  kubectl get node "$1" -o json | jq -r --arg k "$2" '.metadata.labels[$k] // empty'
}

# ready_seconds <claim> <node>: seconds from the NodeClaim's creation to the node's Ready.
ready_seconds() {
  local created ready
  created="$(kubectl get nodeclaim "$1" -o jsonpath='{.metadata.creationTimestamp}')"
  ready="$(kubectl get node "$2" -o json | jq -r '.status.conditions[] | select(.type == "Ready") | .lastTransitionTime')"
  echo $(($(date -d "${ready}" +%s) - $(date -d "${created}" +%s)))
}

# probe <name> <pool> <instance type> <capacity type>: a one-replica Deployment
# pinned to a NodePool and an exact type (manifests/probe.yaml).
probe() {
  NAME="$1" POOL="$2" TYPE="$3" CAPACITY="$4" render "${e2e_dir}/manifests/probe.yaml" | kubectl apply -f -
}

# render <file>: envsubst with the variables the manifests use, and nothing else
# (an unset one is an error, not an empty string).
render() {
  local -a vars
  local v list=""
  # shellcheck disable=SC2016  # the pattern is a literal ${NAME}
  mapfile -t vars < <(grep -o '\${[A-Z0-9_]*}' "$1" | sort -u | tr -d '${}' || true)
  for v in "${vars[@]}"; do
    [[ -n "${!v+x}" ]] || {
      echo "render $1: ${v} is not set" >&2
      return 1
    }
    # shellcheck disable=SC2016  # envsubst takes the names as ${NAME}
    list+="$(printf '${%s} ' "${v}")"
  done
  envsubst "${list}" <"$1"
}

pod_running() { # pod_running <label selector> [namespace]: every pod of the selector Running
  local out
  out="$(kubectl -n "${2:-default}" get pods -l "$1" -o json)"
  [[ "$(jq '.items | length' <<<"${out}")" -gt 0 ]] &&
    [[ "$(jq '[.items[] | select(.status.phase != "Running")] | length' <<<"${out}")" == 0 ]]
}

# node_shell <node> <command>: runs a command in the host's namespaces and root
# directory of the node (a privileged Pod with the host's root mounted, chroot),
# for the checks that look at systemd and files. Over kubectl exec, so it proves
# the exec path as well.
node_shell() {
  local node="$1" pod="nodeshell-$1"
  if ! kubectl get pod "${pod}" >/dev/null 2>&1; then
    NODE="${node}" PODNAME="${pod}" render "${e2e_dir}/manifests/nodeshell.yaml" | kubectl apply -f - >/dev/null
    kubectl wait --for=condition=Ready "pod/${pod}" --timeout=240s >/dev/null
  fi
  kubectl exec "${pod}" -- chroot /host sh -c "$2"
}

# ------------------------------------------------------------- metrics ------

# metrics: the controller's metrics, through the API server's service proxy.
metrics() { kubectl get --raw "/api/v1/namespaces/${ns}/services/scarpenter:metrics/proxy/metrics"; }

# metric_value <metrics text file> <metric> <region> <bucket>: the value of a
# quota series, empty when it does not exist (exposition format: name{labels} value).
metric_value() {
  awk -v n="$2" -v r="$3" -v b="$4" '
    index($0, n "{") == 1 && index($0, "region=\"" r "\"") && index($0, "bucket=\"" b "\"") { print $NF; exit }' "$1"
}

# num <value>: an integer from a Prometheus or CLI number (5, 5.0, 5e+00).
num() { awk -v v="$1" 'BEGIN { printf "%d", v + 0.5 }'; }

# ------------------------------------------------------------ scenarios -----

# The quota buckets of the driver: the vCPUs of pay-as-you-go and of spot instances,
# per region (DescribeAccountAttributes).
quota_buckets=(postpaid-vcpu spot-vcpu)
bucket_od="postpaid-vcpu"
bucket_spot="spot-vcpu"

# is_empty <command...>: true when the command succeeds and prints nothing. A command
# that fails (expired credentials, a throttle, a wrong selector) is not "empty": the check
# fails, so that an error can never pass for "everything is gone". kubectl checks that mean
# "the object is gone" must use --ignore-not-found. The command is run at every poll,
# unlike a $(...) in the arguments of wait_until.
is_empty() {
  local out
  out="$("$@" 2>/dev/null)" || return 1
  [[ -z "${out}" ]]
}

# is_value <want> <command...>: true when the command prints exactly <want>.
is_value() {
  local want="$1"
  shift
  [[ "$("$@" 2>/dev/null)" == "${want}" ]]
}

# ------------------------------------------------- live output, failures -----

# shellcheck source=test/e2e-alicloud/observe.sh
source "${e2e_dir}/observe.sh"
