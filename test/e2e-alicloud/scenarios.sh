#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5 of Alibaba Cloud: the scenarios, run against the cluster that bootstrap.sh and install.sh
# built (docs/plans/v1/wip/21-alicloud.md section 9; docs/how-to-guides/testing.md#the-l5-lane
# lists what each proves and how to read a failure). The shape is that of the AWS lane's
# scenarios.sh.
#
# NOTHING IN THIS LANE HAS RUN YET. It was written and validated statically; the cloud facts
# the assertions rest on (the shape of the CLI's answers, the fields of DescribeInstances,
# what ECS does with LowLatency deployment sets, the metadata service's reach from a Pod) are
# marked "unverified" where they are not certain, and the first live run settles them.
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 arm64 spot imds db described byo negative image quota drift
#                   scalezero uninstall
#                   (preflight always runs first, and ends the run when it fails; the final
#                   check that nothing of the run is left is the teardown's, step 3)
#
# Each scenario is a function that runs in a subshell with `set -e`: the first failed
# assertion ends it, the runner records the result and goes on. Waits have explicit
# deadlines. Workloads of all selected scenarios are started together
# (the prelude), so that the nodes boot in parallel; a scenario then only waits and checks.
# The exit status is 1 when a scenario failed; with E2E_FAIL_FAST=true (the default) the
# scenarios after the first failure are reported as SKIP and not run, so that the
# diagnostics and the teardown start at once. A failed scenario prints one redacted
# snapshot of the cluster (observe.sh), and the waits for nodes print a progress block
# every E2E_PROGRESS_INTERVAL seconds and fail early when no NodeClaim is launched
# after E2E_LAUNCH_TIMEOUT. A run needs about 16 pay-as-you-go and 4 spot vCPUs of quota in
# the region (preflight checks).
#
# Needs: kubectl, jq, helm, the Alibaba Cloud CLI (aliyun) with the role's credentials, envsubst.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-alicloud/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT ALICLOUD_REGION KUBECONFIG
mkdir -p "${E2E_OUT}/state" "${E2E_OUT}/logs"
state="${E2E_OUT}/state"
ali_login

# ------------------------------------------------------------- state --------

setstate() { printf '%s' "$2" >"${state}/$1"; } # setstate <name> <value>
getstate() { cat "${state}/$1" 2>/dev/null || true; }

# pick_zone: a zone of the region for the scenarios that pin one (the deployment set, the
# vSwitch of the bring-your-own network): the first zone, by name, where DescribeAvailableResource
# lists both the amd64 type and the placement type with stock. The zone IDs are the zone names
# (ap-southeast-1a). Without such a zone the first zone of the region is used and the scenario
# will say what it found.
pick_zone() {
  local z
  z="$(getstate zone)"
  if [[ -z "${z}" ]]; then
    z="$(comm -12 <(offered_zones "${E2E_TYPE_AMD64#alicloud.}") <(offered_zones "${E2E_TYPE_DB#alicloud.}") | head -n1)"
    if [[ -z "${z}" ]]; then
      z="$(ecs DescribeZones | jq -r '[.Zones.Zone[].ZoneId] | sort | .[0]')"
      note "no zone lists both ${E2E_TYPE_AMD64#alicloud.} and ${E2E_TYPE_DB#alicloud.} with stock: ${z}, the first zone, is used"
    fi
    setstate zone "${z}"
  fi
  E2E_PG_ZONE="${z}"
  E2E_BYO_ZONE="${z}"
  export E2E_PG_ZONE E2E_BYO_ZONE
}

# drop_nodeshells: the privileged Pods of node_shell would keep a node busy, and an
# empty node is what consolidation removes.
drop_nodeshells() { kubectl delete pods -l scarpenter-e2e=nodeshell --ignore-not-found --wait=false >/dev/null; }

# settle <app label>: waits for the Pod, records its node, claim, UID and instance.
settle() {
  local app="$1" pod node claim iid
  wait_node "the Pod ${app} running" "${E2E_NODE_TIMEOUT}" "app=${app}" "" pod_running "app=${app}"
  pod="$(kubectl get pod -l "app=${app}" -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  assert_nonempty "the NodeClaim of ${node}" "${claim}"
  iid="$(instance_of_claim "${claim}")"
  assert_nonempty "the instance of ${claim}" "${iid}"
  setstate "${app}.pod" "${pod}"
  setstate "${app}.node" "${node}"
  setstate "${app}.claim" "${claim}"
  setstate "${app}.uid" "$(kubectl get nodeclaim "${claim}" -o jsonpath='{.metadata.uid}')"
  setstate "${app}.iid" "${iid}"
  note "${app}: pod ${pod}, node ${node}, claim ${claim}, ready after $(ready_seconds "${claim}" "${node}")s"
}

# nodeclass_status <class> <condition type>: the vendor condition's status on an SCNodeClass.
nodeclass_status() {
  kubectl get scnodeclass "$1" -o jsonpath="{.status.vendors.alicloud.conditions[?(@.type==\"$2\")].status}"
}

# system_disk <instance id>: the system disk record of DescribeDisks.
system_disk() { instance_disks "$1" | jq '[.[] | select(.Type == "system")][0]'; }

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_arm64() { probe e2e-arm64 e2e-arm64 "${E2E_TYPE_ARM64}" spot; }
start_spot() { start_arm64; }
start_imds() { probe e2e-hardened e2e-hardened "${E2E_TYPE_AMD64}" on-demand; }
start_db() {
  pick_zone
  render "${here}/manifests/db.yaml" | kubectl apply -f -
}
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }

# byo_resources: the VPC, the vSwitch and the security group the workflow creates with the
# Alibaba Cloud CLI, tagged with the cluster (the sweeper and the teardown find them by it),
# the run's creation time and the selector tag the NodeClass uses; created only when no
# resource of the run carries the selector tag yet. Prints nothing; the IDs go to the state.
byo_resources() {
  local vpcid vsw sg
  local -a tags=(--Tag.1.Key "${ctag}" --Tag.1.Value "${E2E_CLUSTER}"
    --Tag.2.Key scarpenter-e2e-byo --Tag.2.Value "${E2E_CLUSTER}"
    --Tag.3.Key ci-created-at --Tag.3.Value "${E2E_CREATED_AT}")
  local -a sel=(--Tag.1.Key scarpenter-e2e-byo --Tag.1.Value "${E2E_CLUSTER}")
  vpcid="$(vpc DescribeVpcs "${sel[@]}" | jq -r '.Vpcs.Vpc[0].VpcId // empty')"
  if [[ -z "${vpcid}" ]]; then
    # A /16 of its own, away from the 10.240.0.0/16 of the managed network (plan 21 section 2).
    vpcid="$(vpc CreateVpc --CidrBlock 10.88.0.0/16 --VpcName "${E2E_CLUSTER}-byo" "${tags[@]}" | jq -er '.VpcId')"
  fi
  wait_until "the VPC ${vpcid} available" 120 is_value Available vpc_status "${vpcid}"
  vsw="$(vpc DescribeVSwitches "${sel[@]}" | jq -r '.VSwitches.VSwitch[0].VSwitchId // empty')"
  if [[ -z "${vsw}" ]]; then
    vsw="$(vpc CreateVSwitch --VpcId "${vpcid}" --ZoneId "${E2E_BYO_ZONE}" --CidrBlock 10.88.1.0/24 \
      --VSwitchName "${E2E_CLUSTER}-byo" "${tags[@]}" | jq -er '.VSwitchId')"
  fi
  wait_until "the vSwitch ${vsw} available" 120 is_value Available vswitch_status "${vsw}"
  sg="$(ecs DescribeSecurityGroups "${sel[@]}" | jq -r '.SecurityGroups.SecurityGroup[0].SecurityGroupId // empty')"
  if [[ -z "${sg}" ]]; then
    # Outbound is open by default, which the tailscale template needs and nothing else.
    sg="$(ecs CreateSecurityGroup --VpcId "${vpcid}" --SecurityGroupName "${E2E_CLUSTER}-byo" \
      --Description "scarpenter e2e: brought by the workflow" "${tags[@]}" | jq -er '.SecurityGroupId')"
  fi
  setstate byo.vpc "${vpcid}"
  setstate byo.subnet "${vsw}"
  setstate byo.sg "${sg}"
}

vpc_status() { vpc DescribeVpcs --VpcId "$1" | jq -r '.Vpcs.Vpc[0].Status // empty'; }
vswitch_status() { vpc DescribeVSwitches --VSwitchId "$1" | jq -r '.VSwitches.VSwitch[0].Status // empty'; }

start_byo() {
  pick_zone
  byo_resources
  render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  for t in aliyun kubectl jq helm envsubst curl; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  [[ "$(ecs DescribeRegions | jq '.Regions.Region | length')" -gt 0 ]] || fail "the Alibaba Cloud credentials do not work (DescribeRegions lists nothing)"

  # 1. The Flatcar images (plan 21 section 1, rung 3): the operator's own, named
  # Flatcar-<channel>-<version>-<arch> and tagged flatcar-channel and flatcar-version,
  # exactly one available per architecture in the lane's region. The operator imports and
  # copies them before the run (docs/how-to-guides/testing-and-releasing.md#alibaba-cloud-provisioning);
  # this only checks that they resolve. ImageName matches fuzzily, so the name is compared whole.
  local arch api name n imgs
  for arch in amd64 arm64; do
    api=x86_64
    [[ "${arch}" == "arm64" ]] && api=arm64
    name="Flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-${arch}"
    imgs="$(ecs DescribeImages --ImageOwnerAlias self --Architecture "${api}" --Status Available --ImageName "${name}" |
      jq --arg n "${name}" '[.Images.Image[]? | select(.ImageName == $n)]')"
    n="$(jq 'length' <<<"${imgs}")"
    assert_eq "available images named ${name} owned by this account" 1 "${n}"
    assert_eq "the image's tag flatcar-version" "${E2E_FLATCAR_VERSION}" "$(tag_of "$(jq '.[0]' <<<"${imgs}")" flatcar-version)"
    assert_eq "the image's tag flatcar-channel" "${E2E_FLATCAR_CHANNEL}" "$(tag_of "$(jq '.[0]' <<<"${imgs}")" flatcar-channel)"
    # Ignition's aliyun provider reads the user-data without a token: the image must not
    # require the hardened metadata mode (plan 21 section 5). Whether DescribeImages reports
    # ImdsSupport: unverified, a missing field is a note.
    note "the image ${name}: ImdsSupport $(jq -r '.[0].ImdsSupport // "not reported"' <<<"${imgs}") (v1 is required)"
    [[ "$(jq -r '.[0].ImdsSupport // "v1"' <<<"${imgs}")" == v1 ]] || fail "the image ${name} does not have ImdsSupport=v1: nodes would not read their user-data"
  done

  # 2. The quota the suite needs (a new account's defaults may be lower): the vCPU
  # attributes of DescribeAccountAttributes, the source the driver counts with.
  local od spot
  od="$(account_attr max-postpaid-instance-vcpu-count)"
  spot="$(account_attr max-spot-instance-vcpu-count)"
  [[ "$(num "${od:-0}")" -ge "${E2E_MIN_VCPU_OD}" ]] || fail "the pay-as-you-go vCPU quota of ${ALICLOUD_REGION} is '${od}'; the suite needs ${E2E_MIN_VCPU_OD} (raise it, docs/how-to-guides/testing-and-releasing.md#alibaba-cloud-provisioning)"
  [[ "$(num "${spot:-0}")" -ge "${E2E_MIN_VCPU_SPOT}" ]] || fail "the spot vCPU quota of ${ALICLOUD_REGION} is '${spot}'; the suite needs ${E2E_MIN_VCPU_SPOT} (the account may also be refused spot by its rules)"

  # 3. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc
  for nc in e2e-amd64 e2e-arm64 e2e-drift e2e-hardened; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True "$(nodeclass_status "${nc}" VendorReady)"
    assert_eq "scnodeclass/${nc} ImagesReady" True "$(nodeclass_status "${nc}" ImagesReady)"
  done
  pick_zone
  note "zone of the deployment set and the bring-your-own vSwitch: ${E2E_PG_ZONE}"
}

# Prelude: the baseline of nothing; the workloads of every selected scenario start now.
scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim iid zone zoneid j attr
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  iid="$(getstate e2e-amd64.iid)"

  log "node Ready, labels (overview section 5.5)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "region" "${ALICLOUD_REGION}" "$(node_label "${node}" topology.kubernetes.io/region)"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" alicloud "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${E2E_TYPE_AMD64#alicloud.}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_eq "storage-profile" default "$(node_label "${node}" "${label_domain}/storage-profile")"
  assert_eq "placement-profile" default "$(node_label "${node}" "${label_domain}/placement-profile")"
  assert_eq "nodepool" e2e-amd64 "$(node_label "${node}" karpenter.sh/nodepool)"
  zone="$(node_label "${node}" topology.kubernetes.io/zone)"
  zoneid="$(node_label "${node}" "${label_domain}/zone-id")"
  assert_nonempty "zone name" "${zone}"
  assert_nonempty "zone ID" "${zoneid}"
  # On Alibaba Cloud the zone ID is the zone name (Keeper's zone_id equals its api_reference).
  assert_eq "the zone ID equals the zone name" "${zone}" "${zoneid}"
  assert_eq "the zone is one of the region's, from ECS" 1 "$(ecs DescribeZones | jq --arg z "${zone}" '[.Zones.Zone[] | select(.ZoneId == $z)] | length')"

  log "the instance: tags, metadata options, disks (plan 21 sections 3 and 5)"
  j="$(instance_json "${iid}")"
  assert_eq "instance status" Running "$(jq -r '.Status' <<<"${j}")"
  assert_eq "instance type" "${E2E_TYPE_AMD64#alicloud.}" "$(jq -r '.InstanceType' <<<"${j}")"
  assert_eq "zone" "${zone}" "$(jq -r '.ZoneId' <<<"${j}")"
  assert_eq "pay as you go" PostPaid "$(jq -r '.InstanceChargeType' <<<"${j}")"
  assert_eq "no spot strategy (on-demand)" NoSpot "$(jq -r '.SpotStrategy' <<<"${j}")"
  assert_eq "instance name is the NodeClaim" "${claim}" "$(jq -r '.InstanceName' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" "${ctag}")"
  assert_eq "tag nodeclaim" "${claim}" "$(tag_of "${j}" "${label_domain}/nodeclaim")"
  assert_eq "tag nodepool" e2e-amd64 "$(tag_of "${j}" "${label_domain}/nodepool")"
  assert_eq "tag nodeclass" e2e-amd64 "$(tag_of "${j}" "${label_domain}/nodeclass")"
  assert_eq "tag managed-by" scarpenter "$(tag_of "${j}" "${label_domain}/managed-by")"
  assert_eq "tag ci-created-at (the NodeClass's spec.tags)" "${E2E_CREATED_AT}" "$(tag_of "${j}" ci-created-at)"
  # The metadata options (DescribeInstanceAttribute; that it reports them: unverified): the
  # token requirement stays optional, because Ignition's aliyun provider reads the user-data
  # without a token (plan 21 section 5), and the endpoint is enabled.
  attr="$(instance_attr "${iid}")"
  assert_eq "metadata HttpTokens (optional: Ignition reads the user-data without a token)" optional "$(jq -r '.MetadataOptions.HttpTokens' <<<"${attr}")"
  assert_eq "metadata HttpEndpoint" enabled "$(jq -r '.MetadataOptions.HttpEndpoint' <<<"${attr}")"
  assert_nonempty "public IPv4" "$(jq -r '(.PublicIpAddress.IpAddress[0] // .EipAddress.IpAddress // empty)' <<<"${j}")"
  assert_eq "the security group is attached" 1 "$(jq '.SecurityGroupIds.SecurityGroupId | length' <<<"${j}")"
  # The system disk: cloud_essd PL0 of 40 GiB as the NodeClass says, deleted with the instance.
  local disk
  disk="$(system_disk "${iid}")"
  assert_eq "system disk category" cloud_essd "$(jq -r '.Category' <<<"${disk}")"
  assert_eq "system disk level" PL0 "$(jq -r '.PerformanceLevel' <<<"${disk}")"
  assert_eq "system disk size" 40 "$(jq -r '.Size' <<<"${disk}")"
  assert_eq "system disk DeleteWithInstance" true "$(jq -r '.DeleteWithInstance' <<<"${disk}")"
  assert_eq "system disk encrypted (the default is off)" false "$(jq -r '.Encrypted' <<<"${disk}")"
  # Whether the disk inherits the instance's tags at RunInstances: unverified, recorded.
  note "the system disk's tag cluster: '$(tag_of "${disk}" "${ctag}")' (the teardown finds leaked disks by it)"

  log "the claim: price, bootstrap token, serving certificate"
  local price
  price="$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price" '.metadata.annotations[$k] // empty')"
  assert_nonempty "price annotation" "${price}"
  awk -v p="${price}" 'BEGIN { exit !(p + 0 > 0) }' || fail "the price ${price} is not positive"
  note "price-incomplete on the amd64 claim: '$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price-incomplete" '.metadata.annotations[$k] // empty')' (empty: the disk is priced completely)"
  wait_until "the bootstrap token Secret gone" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim=${claim}" -o name
  wait_until "the serving CSR approved and issued" 120 csr_approved "${node}"

  log "kubectl logs and exec (the API server reaches the kubelet over the tailnet)"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_match "the log of the probe" scarpenter-e2e-up "$(kubectl logs "${pod}")"
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"

  log "on the node: the unit-level checks of L4 scenarios Q09 and Q10, on a real node"
  node_unit_checks "${node}"

  log "the Ignition check: the aliyun platform, and its provider fetched the user-data"
  ignition_checks "${node}"

  log "cluster DNS"
  local dns
  dns="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  assert_match "the kubelet's clusterDNS is the kube-dns Service" "${dns//./\\.}" "$(node_shell "${node}" "grep -A3 '^clusterDNS' /var/lib/kubelet/config.yaml" | tr '\n' ' ')"
  wait_until "a Pod resolves a cluster Service" 120 kubectl exec "${pod}" -- nslookup kubernetes.default.svc.cluster.local
  drop_nodeshells
}

# ignition_checks <node>: the node booted on the aliyun platform and Ignition's aliyun
# provider read its configuration from the metadata service. What the boot leaves where
# (the platform ID in the kernel command line and the OEM release, the lines of the ignition
# journal tag) is unverified: the patterns accept the spellings I know of, and a failure
# prints what was found.
ignition_checks() {
  local node="$1" plat journal
  plat="$(node_shell "${node}" 'cat /proc/cmdline /etc/flatcar/oem-release /etc/os-release 2>/dev/null; cat /run/metadata/* 2>/dev/null' || true)"
  assert_match "PLATFORM_ID is aliyun (the platform Ignition used)" '(PLATFORM_ID|platform\.id|oem\.id|OEM_ID|^ID)=("?)aliyun' "${plat}"
  # The journal of the first boot: Ignition's fetch of the user-data from 100.100.100.200.
  journal="$(node_shell "${node}" 'journalctl -b -t ignition --no-pager 2>/dev/null | grep -iE "aliyun|100\.100\.100\.200|user-data" | head -n 20' || true)"
  assert_match "Ignition's aliyun provider fetched the user-data" '(100\.100\.100\.200|aliyun)' "${journal}"
  note "ignition journal lines about the fetch: $(wc -l <<<"${journal}" | tr -d ' ')"
}

# csr_approved <node>: the node's serving CSR is approved and has a certificate.
csr_approved() {
  kubectl get csr -o json | jq -e --arg n "system:node:$1" '
    [.items[] | select(.spec.username == $n and .spec.signerName == "kubernetes.io/kubelet-serving")
      | select((.status.conditions // [] | map(.type) | index("Approved")) and (.status.certificate // "") != "")] | length > 0' >/dev/null
}

# node_pod_exec <node> <sh command>: in the node shell Pod's own container (host network).
node_pod_exec() {
  node_shell "$1" true >/dev/null
  kubectl exec "nodeshell-$1" -- sh -c "$2"
}

# node_unit_checks <node>: what L4 scenarios Q09 and Q10 check inside a VM, on a real
# node, through the node shell: the tailscale sysext merged, tailscaled upheld, the
# kubelet ordered after and bound to the join unit and started with the tailnet address.
node_unit_checks() {
  local node="$1" ip pid
  assert_eq "tailscaled, the join unit and the kubelet are active" "active,active,active" \
    "$(node_shell "${node}" 'systemctl is-active tailscaled scarpenter-tailscale-up.service kubelet' | paste -sd, -)"
  assert_match "the tailscale sysext is merged" tailscale "$(node_shell "${node}" 'systemd-sysext status 2>&1')"
  assert_nonempty "what upholds tailscaled.service" "$(node_shell "${node}" 'systemctl show tailscaled.service -p UpheldBy --value')"
  assert_match "the kubelet is ordered after and bound to the join unit" scarpenter-tailscale-up.service \
    "$(node_shell "${node}" 'systemctl show kubelet.service -p After -p Requires --value')"
  ip="$(kubectl get node "${node}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
  assert_match "the node's address is a tailscale address" '^100\.' "${ip}"
  assert_eq "tailscale ip -4 is the node's address" "${ip}" "$(node_shell "${node}" 'tailscale ip -4')"
  pid="$(node_shell "${node}" 'systemctl show kubelet.service -p MainPID --value')"
  assert_match "the kubelet's --node-ip" "--node-ip=${ip//./\\.}" "$(node_shell "${node}" "tr '\\0' ' ' </proc/${pid}/cmdline")"
  assert_eq "the auth key file mode" 600 "$(node_shell "${node}" 'stat -c %a /etc/scarpenter/tailscale-authkey')"
}

scn_arm64() {
  start_arm64
  settle e2e-arm64
  local pod node claim iid j
  pod="$(getstate e2e-arm64.pod)"
  node="$(getstate e2e-arm64.node)"
  claim="$(getstate e2e-arm64.claim)"
  iid="$(getstate e2e-arm64.iid)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_ARM64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "arch" arm64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "capacity-type" spot "$(node_label "${node}" karpenter.sh/capacity-type)"
  j="$(instance_json "${iid}")"
  assert_eq "instance type" "${E2E_TYPE_ARM64#alicloud.}" "$(jq -r '.InstanceType' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" "${ctag}")"
  # The image the instance booted is the arm64 one (the architecture of the image, from ECS).
  assert_eq "the instance's image is an arm64 image" arm64 \
    "$(ecs DescribeImages --ImageId "$(jq -r '.ImageId' <<<"${j}")" | jq -r '.Images.Image[0].Architecture')"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  assert_eq "tailscaled, the join unit and the kubelet are active (arm64 sysext)" "active,active,active" \
    "$(node_shell "${node}" 'systemctl is-active tailscaled scarpenter-tailscale-up.service kubelet' | paste -sd, -)"
  drop_nodeshells
  note "arm64 spot claim ${claim}"
}

# spot (S2): what makes the arm64 node a spot instance. The account may be refused spot by
# its rules (InvalidAccount.NotSupportSpot): the NodeClaim is then never launched and
# the arm64 scenario fails first with `never launched`.
scn_spot() {
  settle_fixtures
  local iid j claim
  iid="$(getstate e2e-arm64.iid)"
  claim="$(getstate e2e-arm64.claim)"
  j="$(instance_json "${iid}")"
  assert_eq "SpotStrategy (pay the market price, capped at the on-demand price)" SpotAsPriceGo "$(jq -r '.SpotStrategy' <<<"${j}")"
  assert_eq "still running (no interruption yet)" Running "$(jq -r '.Status' <<<"${j}")"
  assert_eq "the node's capacity-type label" spot "$(node_label "$(getstate e2e-arm64.node)" karpenter.sh/capacity-type)"
  note "spot protection period of the instance: $(jq -r '.SpotDuration // "not reported"' <<<"${j}") (0 is none)"
  local price
  price="$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price" '.metadata.annotations[$k] // empty')"
  assert_nonempty "price annotation of the spot claim" "${price}"
  # The spot vCPUs count in their own bucket (the quota scenario compares the numbers).
  [[ "$(num "$(account_attr used-spot-instance-vcpu-count)")" -ge "$(vcpus_of "${E2E_TYPE_ARM64#alicloud.}")" ]] ||
    fail "the account's spot vCPU usage does not include the launched spot instance"
}

# imds (plan 21 section 5, the gap): what a Pod can reach at 100.100.100.200. On the plain
# NodeClass the ECS metadata service is open to Pods (no hop limit exists): the scenario RECORDS what
# a Pod reads (only the status and the size: the user-data holds the bootstrap token and the
# tailscale key, so it is never printed). With bootstrap.hardening metadata-from-pods on
# the second NodeClass the same Pod must reach nothing, while the node itself (a host-network Pod: its
# traffic is not forwarded) still does. The patterns of the metadata paths are unverified.
# pod_fetch_size <pod> <url>: how many bytes the Pod gets from the URL (0 when it gets nothing).
# The content itself is never printed: the user-data holds secrets.
pod_fetch_size() {
  kubectl exec "$1" -- sh -c "wget -q -T 4 -O- $2 2>/dev/null | wc -c" 2>/dev/null | tr -d ' ' || echo 0
}

scn_imds() {
  settle_fixtures
  start_imds
  settle e2e-hardened
  local plain_pod hard_pod hard_node hard_iid url id_plain
  plain_pod="$(getstate e2e-amd64.pod)"
  hard_pod="$(getstate e2e-hardened.pod)"
  hard_node="$(getstate e2e-hardened.node)"
  hard_iid="$(getstate e2e-hardened.iid)"
  url=http://100.100.100.200/latest
  id_plain="$(kubectl exec "${plain_pod}" -- sh -c "wget -q -T 4 -O- ${url}/meta-data/instance-id 2>/dev/null" || true)"
  if [[ -n "${id_plain}" ]]; then
    note "IMDS GAP (plan 21 section 5): a Pod on the plain NodeClass reads the instance ID from 100.100.100.200 and $(pod_fetch_size "${plain_pod}" "${url}/user-data") bytes of the user-data (the bootstrap token and the tailnet key; TTL 1 h, deleted at registration)"
  else
    note "a Pod on the plain NodeClass reached nothing at 100.100.100.200: the gap is closed on this account (unverified why)"
  fi
  # The positive control: the node itself reads its metadata (instance ID equals the instance).
  assert_eq "the node's own read of the metadata service on the hardened node" "${hard_iid}" \
    "$(node_pod_exec "${hard_node}" "wget -q -T 4 -O- ${url}/meta-data/instance-id" || true)"
  # With the hardening: a Pod reaches nothing.
  assert_eq "a Pod on the hardened NodeClass reads no instance ID" "" \
    "$(kubectl exec "${hard_pod}" -- sh -c "wget -q -T 4 -O- ${url}/meta-data/instance-id 2>/dev/null" || true)"
  assert_eq "a Pod on the hardened NodeClass reads no user-data (bytes)" 0 "$(pod_fetch_size "${hard_pod}" "${url}/user-data")"
  # The hardening is a rule that drops forwarded traffic to the address: its trace on the node.
  note "rules naming 100.100.100.200 on the hardened node: $(node_shell "${hard_node}" '(nft list ruleset 2>/dev/null || iptables-save 2>/dev/null) | grep -c 100.100.100.200 || true' | tr -d ' ')"
  drop_nodeshells
}

scn_db() {
  start_db
  wait_node "the database and the client running" $((E2E_NODE_TIMEOUT + 300)) "scarpenter-e2e=true,app in (e2e-db,e2e-client)" "" \
    pod_running "scarpenter-e2e=true,app in (e2e-db,e2e-client)"
  local dbpod clpod dbnode clnode dbiid cliid dbclaim ds dsid dbj clj
  dbpod="$(kubectl get pod -l app=e2e-db -o jsonpath='{.items[0].metadata.name}')"
  clpod="$(kubectl get pod -l app=e2e-client -o jsonpath='{.items[0].metadata.name}')"
  dbnode="$(pod_node "${dbpod}")"
  clnode="$(pod_node "${clpod}")"
  [[ "${dbnode}" != "${clnode}" ]] || fail "the database and the client share the node ${dbnode}"
  dbclaim="$(claim_of_node "${dbnode}")"
  dbiid="$(instance_of_claim "${dbclaim}")"
  cliid="$(instance_of_claim "$(claim_of_node "${clnode}")")"
  dbj="$(instance_json "${dbiid}")"
  clj="$(instance_json "${cliid}")"

  log "one LowLatency deployment set, named and described by SCarpenter"
  dsid="$(cluster_deployment_sets)"
  assert_eq "deployment sets of the cluster" 1 "$(wc -l <<<"${dsid}" | tr -d ' ')"
  ds="$(ecs DescribeDeploymentSets --DeploymentSetIds "[\"${dsid}\"]" | jq '.DeploymentSets.DeploymentSet[0]')"
  assert_match "the set's name" "^scarpenter-" "$(jq -r '.DeploymentSetName' <<<"${ds}")"
  assert_eq "strategy" LowLatency "$(jq -r '.Strategy' <<<"${ds}")"
  assert_match "the marker in the description (a set takes no tags)" "cluster=${E2E_CLUSTER}( |$)" "$(jq -r '.DeploymentSetDescription' <<<"${ds}")"
  assert_eq "the database is in the set" "${dsid}" "$(jq -r '.DeploymentSetId' <<<"${dbj}")"
  assert_eq "the client is in the set" "${dsid}" "$(jq -r '.DeploymentSetId' <<<"${clj}")"
  assert_eq "one zone" "${E2E_PG_ZONE}" "$(jq -r '.ZoneId' <<<"${dbj}")"
  assert_eq "one zone (client)" "${E2E_PG_ZONE}" "$(jq -r '.ZoneId' <<<"${clj}")"

  log "node labels of the applied profiles"
  assert_eq "database storage-profile" db-essd "$(node_label "${dbnode}" "${label_domain}/storage-profile")"
  assert_eq "database placement-profile" colocated "$(node_label "${dbnode}" "${label_domain}/placement-profile")"
  assert_eq "client storage-profile" default "$(node_label "${clnode}" "${label_domain}/storage-profile")"
  assert_eq "client placement-profile" colocated "$(node_label "${clnode}" "${label_domain}/placement-profile")"
  assert_eq "database run key" "pg-${E2E_RUN_ID}" "$(node_label "${dbnode}" "${label_domain}/placement-group")"
  assert_eq "client run key" "pg-${E2E_RUN_ID}" "$(node_label "${clnode}" "${label_domain}/placement-group")"
  assert_eq "profile-hashes annotation names the storage profile" true \
    "$(kubectl get nodeclaim "${dbclaim}" -o json | jq -r --arg k "${label_domain}/profile-hashes" '.metadata.annotations[$k] | fromjson | has("storage/db-essd")')"
  note "price-incomplete on the database claim: '$(kubectl get nodeclaim "${dbclaim}" -o json | jq -r --arg k "${label_domain}/price-incomplete" '.metadata.annotations[$k] // empty')'"

  log "the cloud_essd PL1 data disk: attached, mounted at the profile's path"
  local disk diskid src serial
  disk="$(instance_disks "${dbiid}" | jq '[.[] | select(.Type == "data")][0]')"
  diskid="$(jq -r '.DiskId // empty' <<<"${disk}")"
  assert_nonempty "the data disk of the database" "${diskid}"
  assert_eq "data disk category" cloud_essd "$(jq -r '.Category' <<<"${disk}")"
  assert_eq "data disk level" PL1 "$(jq -r '.PerformanceLevel' <<<"${disk}")"
  assert_eq "data disk size" 20 "$(jq -r '.Size' <<<"${disk}")"
  assert_eq "data disk DeleteWithInstance" true "$(jq -r '.DeleteWithInstance' <<<"${disk}")"
  assert_eq "data disk in use" In_use "$(jq -r '.Status' <<<"${disk}")"
  src="$(node_shell "${dbnode}" 'findmnt -no SOURCE /var/lib/pgdata')"
  assert_nonempty "the device mounted at /var/lib/pgdata" "${src}"
  assert_eq "the filesystem at /var/lib/pgdata" xfs "$(node_shell "${dbnode}" 'findmnt -no FSTYPE /var/lib/pgdata')"
  # The serial of a virtio disk is its ID without the "d-" (plan 21 section 4); an NVMe
  # serial may differ (unverified), so a different or empty serial is a note, not a failure.
  serial="$(node_shell "${dbnode}" "lsblk -dno SERIAL ${src}" | tr -d ' ')"
  if [[ "${serial}" == "${diskid#d-}" ]]; then
    echo "ok: the device's serial is the disk ID without d-"
  else
    note "the device ${src} has the serial '${serial}', the disk is ${diskid}: the by-serial match of the first-boot unit may differ on this type"
  fi
  assert_eq "the Pod's file is on the mounted disk" hello-from-the-volume "$(node_shell "${dbnode}" 'cat /var/lib/pgdata/hello')"
  assert_match "JobRunningTimeoutSec=240 on the device unit of the label" "dev-disk-by.*scv.*JobRunningTimeoutSec=240|JobRunningTimeoutSec=240" \
    "$(node_shell "${dbnode}" 'grep -r JobRunningTimeoutSec /etc/systemd/system/ 2>/dev/null | grep dev-disk-by' | head -n1)"

  log "isolation: the NodeClass's managed security group allows its members"
  assert_eq "a managed group with a rule that refers to itself" true "$(managed_group_self_referencing)"

  log "delete both workloads: the instances, the set and the disk go"
  drop_nodeshells
  kubectl delete statefulset e2e-db --wait=false
  kubectl delete deployment e2e-client --wait=false
  wait_until "the instances gone" 300 is_empty cluster_instances_of "${dbiid}" "${cliid}"
  # perUse: two minutes of grace for launches in flight, then the next resource check
  # and garbage collection (30 s and 1 m here).
  wait_until "the deployment set deleted" 420 is_empty cluster_deployment_sets
  wait_until "the data disk gone" 120 is_empty disks_existing "${diskid}"
}

# disks_existing <id>...: those of the disks that still exist.
disks_existing() {
  ecs DescribeDisks --DiskIds "$(jq -cn '$ARGS.positional' --args "$@")" | jq -r '.Disks.Disk[]?.DiskId'
}

# managed_group_self_referencing: true when a security group SCarpenter created for the
# cluster has a rule whose source is the group itself (isolation: nodeclass). Prints true or false.
managed_group_self_referencing() {
  local id ok=false
  for id in $(managed_groups_ids); do
    if [[ "$(ecs DescribeSecurityGroupAttribute --SecurityGroupId "${id}" | jq --arg g "${id}" 'any(.Permissions.Permission[]?; .SourceGroupId == $g)')" == true ]]; then
      ok=true
    fi
  done
  echo "${ok}"
}

# managed_groups_ids: the security groups SCarpenter created for the cluster (the brought one
# carries no managed-by tag).
managed_groups_ids() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  ecs DescribeSecurityGroups "${tag[@]}" --PageSize 100 |
    jq -r --arg k "${label_domain}/managed-by" '.SecurityGroups.SecurityGroup[]? | select(any(.Tags.Tag[]?; .TagKey == $k and .TagValue == "scarpenter")) | .SecurityGroupId'
}

# cluster_instances_of <id>...: those of the instances that still exist. A released instance
# is not listed, and asking for it by ID returns an empty list (not an error).
cluster_instances_of() {
  local ids
  ids="$(jq -cn '$ARGS.positional' --args "$@")"
  ecs DescribeInstances --InstanceIds "${ids}" | jq -r '.Instances.Instance[]?.InstanceId'
}

scn_described() {
  start_described
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-described "" pod_running app=e2e-described
  local pod node claim iid t vcpus mem pct
  pod="$(kubectl get pod -l app=e2e-described -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  iid="$(instance_of_claim "${claim}")"
  t="$(node_label "${node}" node.kubernetes.io/instance-type)"
  vcpus="$(node_label "${node}" "${label_domain}/vcpus")"
  mem="$(node_label "${node}" "${label_domain}/memory-mib")"
  pct="$(node_label "${node}" "${label_domain}/metric-single-core-pct")"
  note "the described workload got ${t}: ${vcpus} vCPUs, ${mem} MiB, single-core percentile ${pct}"
  assert_match "the type is one of the allowed test types" ",${t}," ",${E2E_TYPES_DESCRIBED},"
  [[ "${vcpus}" -gt 1 && "${vcpus}" -lt 3 ]] || fail "vcpus ${vcpus} is outside (1, 3)"
  [[ "${mem}" -gt 5000 ]] || fail "memory ${mem} MiB is not above 5000"
  [[ "${pct}" -gt 0 ]] || fail "the percentile ${pct} is not above 0 (the benchmark may not exist for these types: unverified)"
  assert_eq "the instance is of the chosen type" "${t#alicloud.}" "$(jq -r '.InstanceType' <<<"$(instance_json "${iid}")")"
  # The same facts from ECS, independent of the catalog.
  assert_eq "ECS's vCPUs of the type" "${vcpus}" "$(vcpus_of "${t#alicloud.}")"
}

scn_byo() {
  start_byo
  local subnet sg vpcid
  subnet="$(getstate byo.subnet)"
  sg="$(getstate byo.sg)"
  vpcid="$(getstate byo.vpc)"
  wait_until "ResourcesReady on the NodeClass" 180 is_value True \
    kubectl get scnodeclass e2e-use -o jsonpath='{.status.vendors.alicloud.conditions[?(@.type=="ResourcesReady")].status}'
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-use "" pod_running app=e2e-use
  local pod node iid j
  pod="$(kubectl get pod -l app=e2e-use -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  iid="$(instance_of_claim "$(claim_of_node "${node}")")"
  j="$(instance_json "${iid}")"
  assert_eq "the instance is in the brought vSwitch" "${subnet}" "$(jq -r '.VpcAttributes.VSwitchId' <<<"${j}")"
  assert_eq "the instance is in the brought VPC" "${vpcid}" "$(jq -r '.VpcAttributes.VpcId' <<<"${j}")"
  assert_eq "the instance uses the brought security group" true "$(jq -r --arg sg "${sg}" '[.SecurityGroupIds.SecurityGroupId[]] | index($sg) != null' <<<"${j}")"
  assert_eq "the zone of the vSwitch" "${E2E_BYO_ZONE}" "$(jq -r '.ZoneId' <<<"${j}")"

  log "SCarpenter must not touch what it did not create"
  drop_nodeshells
  kubectl delete deployment e2e-use --wait=false
  wait_until "the instance gone" 300 is_empty cluster_instances_of "${iid}"
  kubectl delete nodepool e2e-use --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-use --wait=true --timeout=120s
  # Past a garbage collection (1 m), a resource check (30 s) and the status controller:
  # a `use` reference is never modified or deleted.
  sleep 120
  assert_eq "the VPC still exists" "${vpcid}" "$(vpc DescribeVpcs --VpcId "${vpcid}" | jq -r '.Vpcs.Vpc[0].VpcId // empty')"
  assert_eq "the vSwitch still exists" "${subnet}" "$(vpc DescribeVSwitches --VSwitchId "${subnet}" | jq -r '.VSwitches.VSwitch[0].VSwitchId // empty')"
  assert_eq "the security group still exists" "${sg}" "$(ecs DescribeSecurityGroups --SecurityGroupIds "[\"${sg}\"]" | jq -r '.SecurityGroups.SecurityGroup[0].SecurityGroupId // empty')"
  # Untouched: no rule was added to the brought group (it was created with none).
  assert_eq "the brought group has no rule SCarpenter added" 0 \
    "$(ecs DescribeSecurityGroupAttribute --SecurityGroupId "${sg}" --Direction ingress | jq '[.Permissions.Permission[]?] | length')"
  note "the brought VPC ${vpcid}, vSwitch ${subnet} and group ${sg} are deleted by the teardown (they carry the cluster tag)"
}

# negative: NodeClasses the default network cannot serve are refused and nothing is
# launched (plan 21 section 4, ADR-022).
scn_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
  wait_until "publicIPv4: false is refused (no NAT gateway to give the nodes egress)" 180 is_value False \
    kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.alicloud.conditions[?(@.type=="VendorReady")].status}'
  assert_eq "the reason" ValidationFailed \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.alicloud.conditions[?(@.type=="VendorReady")].reason}')"
  assert_match "the message names the fix" "(use|subnets|publicIPv4)" \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.alicloud.conditions[?(@.type=="VendorReady")].message}')"
  wait_until "publicIPv6: true is refused (no IPv6 gateway the driver manages)" 180 is_value False \
    kubectl get scnodeclass e2e-ipv6 -o jsonpath='{.status.vendors.alicloud.conditions[?(@.type=="VendorReady")].status}'
  assert_match "the message names the field" "publicIPv6" \
    "$(kubectl get scnodeclass e2e-ipv6 -o jsonpath='{.status.vendors.alicloud.conditions[?(@.type=="VendorReady")].message}')"
  kubectl delete scnodeclass e2e-noipv4 e2e-ipv6 --wait=true --timeout=120s
}

# image (rung 3): the instances booted the images the lane's preflight found by name, and the
# NodeClass reports them. (Rung 2, owner and name of a community image, does not exist yet.)
scn_image() {
  settle_fixtures
  local arch app name want got ncimg
  for app in e2e-amd64:amd64 e2e-arm64:arm64; do
    arch="${app#*:}"
    name="Flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-${arch}"
    got="$(jq -r '.ImageId' <<<"$(instance_json "$(getstate "${app%%:*}.iid")")")"
    assert_nonempty "the image of the ${arch} instance" "${got}"
    want="$(ecs DescribeImages --ImageOwnerAlias self --ImageName "${name}" | jq -r --arg n "${name}" '[.Images.Image[]? | select(.ImageName == $n)][0].ImageId // empty')"
    assert_eq "the ${arch} instance booted the image named ${name}" "${want}" "${got}"
    assert_eq "its name" "${name}" "$(ecs DescribeImages --ImageId "${got}" | jq -r '.Images.Image[0].ImageName')"
    ncimg="$(kubectl get scnodeclass "${app%%:*}" -o json | jq -c '.status.vendors.alicloud.images // []')"
    assert_match "status.vendors.alicloud.images of ${app%%:*} names ${got}" "${got}" "${ncimg}"
  done
}

# quota (plan 21 section 6): what the controller reports equals what ECS says
# (DescribeAccountAttributes, the call the driver reads). Run with the nodes of the other
# scenarios up and no launch in flight.
scn_quota() {
  settle_fixtures
  local m
  m="$(mktemp)"
  log "the limits of both buckets equal DescribeAccountAttributes"
  wait_until "scarpenter_quota_limit postpaid-vcpu = DescribeAccountAttributes" 240 quota_limit_is "${m}" "${bucket_od}" max-postpaid-instance-vcpu-count
  wait_until "scarpenter_quota_limit spot-vcpu = DescribeAccountAttributes" 90 quota_limit_is "${m}" "${bucket_spot}" max-spot-instance-vcpu-count

  log "usage follows the launches: what the controller counts is what ECS says is used"
  wait_until "scarpenter_quota_used of the pay-as-you-go bucket equals ECS's" 300 quota_used_is "${m}" od
  wait_until "scarpenter_quota_used of the spot bucket equals ECS's" 300 quota_used_is "${m}" spot
  local od_used spot_used
  od_used="$(metric_value "${m}" scarpenter_quota_used "${ALICLOUD_REGION}" "${bucket_od}")"
  spot_used="$(metric_value "${m}" scarpenter_quota_used "${ALICLOUD_REGION}" "${bucket_spot}")"
  [[ "$(num "${od_used}")" -ge "$(vcpus_of "${E2E_TYPE_AMD64#alicloud.}")" ]] || fail "the pay-as-you-go usage ${od_used} does not include the launched instance"
  [[ "$(num "${spot_used}")" -ge "$(vcpus_of "${E2E_TYPE_ARM64#alicloud.}")" ]] || fail "the spot usage ${spot_used} does not include the launched spot instance"
  wait_until "the in-flight reservations are gone (pay-as-you-go)" 240 quota_reserved_zero "${m}" "${bucket_od}"
  wait_until "the in-flight reservations are gone (spot)" 240 quota_reserved_zero "${m}" "${bucket_spot}"

  log "the launched types are offered in their zones (DescribeAvailableResource)"
  local t zid
  for t in e2e-amd64:"${E2E_TYPE_AMD64}" e2e-arm64:"${E2E_TYPE_ARM64}"; do
    zid="$(node_label "$(getstate "${t%%:*}.node")" "${label_domain}/zone-id")"
    assert_match "${t#*:} is listed with stock in ${zid}" "(^|[[:space:]])${zid}($|[[:space:]])" \
      "$(offered_zones "${t#*:alicloud.}" "$([[ "${t%%:*}" == e2e-arm64 ]] && echo spot)" | tr '\n' ' ')"
  done
  rm -f "${m}"
}

quota_limit_is() { # quota_limit_is <metrics file> <bucket> <attribute name>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_limit "${ALICLOUD_REGION}" "$2")")" == "$(num "$(account_attr "$3")")" ]]
}

quota_used_is() { # quota_used_is <metrics file> od|spot
  local bucket="${bucket_od}" attr=used-postpaid-instance-vcpu-count
  [[ "$2" == "spot" ]] && bucket="${bucket_spot}" attr=used-spot-instance-vcpu-count
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_used "${ALICLOUD_REGION}" "${bucket}")")" == "$(num "$(account_attr "${attr}")")" ]]
}

quota_reserved_zero() { # quota_reserved_zero <metrics file> <bucket>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_reserved "${ALICLOUD_REGION}" "$2")")" == 0 ]]
}

# settle_fixtures: the amd64 and arm64 nodes of the first scenarios (started if the
# scenario runs alone).
settle_fixtures() {
  [[ -n "$(getstate e2e-amd64.iid)" ]] || {
    start_amd64
    settle e2e-amd64
  }
  [[ -n "$(getstate e2e-arm64.iid)" ]] || {
    start_arm64
    settle e2e-arm64
  }
}

# drift (plan 10 scenario 4) and the opportunistic controller restart of plan 07's review: a
# change to a profile no node uses drifts nothing; a change to the vendor block (the system disk size)
# replaces the node, and the controller is killed while the replacement's Create is in flight.
scn_drift() {
  start_drift
  settle e2e-drift
  local old_claim old_node old_iid old_pod
  old_claim="$(getstate e2e-drift.claim)"
  old_node="$(getstate e2e-drift.node)"
  old_iid="$(getstate e2e-drift.iid)"
  old_pod="$(getstate e2e-drift.pod)"

  log "a profile no running node uses changes: nothing drifts"
  kubectl patch scnodeclass e2e-drift --type merge -p \
    '{"spec":{"profiles":{"storage":{"scratch":{"alicloud":{"data":[{"name":"scratch","category":"cloud_essd","performanceLevel":"PL0","size":"30Gi","filesystem":"ext4","mountPath":"/var/lib/scratch"}]}}}}}}'
  wait_until "the NodeClass reconciled the change" 120 nodeclass_observed e2e-drift
  local i
  for i in 1 2 3 4 5 6; do
    sleep 10
    assert_eq "the claim is not drifted (${i}/6)" "" \
      "$(kubectl get nodeclaim "${old_claim}" -o jsonpath='{.status.conditions[?(@.type=="Drifted")].status}' | grep -x True || true)"
  done
  assert_eq "still the same claim" "${old_claim}" "$(claim_of_node "${old_node}")"

  log "the system disk size changes: the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p '{"spec":{"vendors":{"alicloud":{"storage":{"root":{"size":"44Gi"}}}}}}'
  restart_during_create e2e-drift "${old_claim}"
  wait_node "the Pod on another node" "${E2E_NODE_TIMEOUT}" app=e2e-drift "${old_claim}" pod_moved_from app=e2e-drift "${old_node}"
  release_do_not_disrupt
  local pod node claim iid
  pod="$(kubectl get pod -l app=e2e-drift -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  iid="$(instance_of_claim "${claim}")"
  [[ "${iid}" != "${old_iid}" ]] || fail "the replacement runs on the old instance"
  assert_eq "the replacement's system disk (the new size)" 44 "$(system_disk "${iid}" | jq -r '.Size')"
  wait_until "the old instance gone" 300 is_empty cluster_instances_of "${old_iid}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  assert_no_orphans
  # ECS lists no released instance, so a second RunInstances for the replacement claim that
  # the garbage collector already released cannot be counted afterwards: the check below is
  # the one that holds (at most one instance owned by the claim now).
  local repl
  repl="$(getstate drift.replacement)"
  [[ -z "${repl}" ]] || [[ "$(instances_of_claim "${repl}" | wc -w | tr -d ' ')" -le 1 ]] || fail "the claim ${repl} owns several instances"
  setstate e2e-drift.node "${node}"
  setstate e2e-drift.claim "${claim}"
  setstate e2e-drift.iid "${iid}"
}

# release_do_not_disrupt: removes karpenter.sh/do-not-disrupt from the drift
# replacement's NodeClaim and from every node of the lane. core copies a NodeClaim's
# annotations to its Node at registration and reads the Node's once registered, so
# removing it from the claim alone would keep the node from ever being consolidated
# (scalezero). Idempotent; scalezero calls it too, in case drift failed half-way.
release_do_not_disrupt() {
  local repl
  repl="$(getstate drift.replacement)"
  [[ -z "${repl}" ]] || kubectl annotate nodeclaim "${repl}" karpenter.sh/do-not-disrupt- >/dev/null 2>&1 || true
  local n
  for n in $(kubectl get nodes -l karpenter.sh/nodepool -o name 2>/dev/null); do
    kubectl annotate "${n}" karpenter.sh/do-not-disrupt- >/dev/null 2>&1 || true
  done
}

nodeclass_observed() { # the status is for the generation of the spec
  [[ "$(kubectl get scnodeclass "$1" -o jsonpath='{.metadata.generation}')" == "$(kubectl get scnodeclass "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].observedGeneration}')" ]]
}

pod_moved_from() { # pod_moved_from <label selector> <node>: running, and not on <node>
  pod_running "$1" && [[ "$(kubectl get pod -l "$1" -o jsonpath='{.items[0].spec.nodeName}')" != "$2" ]]
}

# restart_during_create <nodepool> <old claim>: waits for the replacement NodeClaim of the
# pool, protects it from consolidation, and as soon as it exists and is not launched yet
# kills the controller Pod hard (delete --grace-period=0 --force: no graceful stop that
# lets a running Create finish); the Deployment starts a new one. Create is idempotent
# (ClientToken derived from the claim, the controller replays it): after the restart the claim
# must not own two instances, and no instance may be orphaned.
#
# The window (the instance exists, Create has not returned) is narrow and no flag
# stretches it (the lane builds the production image; no test-only driver flag exists), so
# the replay assertions are OPPORTUNISTIC: when the attempt misses the window the scenario
# notes it and never fails for it. What always holds and is always asserted: no instance is
# left without a NodeClaim (the caller), and a claim that is still there owns one instance.
#
# core forgets its disruption command at a restart, and the replacement (no Pod on it yet)
# would be an empty node to consolidate (WhenEmpty, 0 s): the claim carries
# karpenter.sh/do-not-disrupt until the Pod has moved (the caller removes it).
restart_during_create() {
  local pool="$1" old="$2" deadline new="" window=no before="" after launched
  deadline=$((SECONDS + 300))
  while [[ -z "${new}" && "${SECONDS}" -lt "${deadline}" ]]; do
    new="$(kjson nodeclaims -l "karpenter.sh/nodepool=${pool}" | jq -r --arg old "${old}" \
      '[.items[] | select(.metadata.name != $old)] | first | .metadata.name // empty')"
    [[ -n "${new}" ]] || sleep 0.3
  done
  [[ -n "${new}" ]] || fail "no replacement NodeClaim appeared in 300 s"
  kubectl annotate nodeclaim "${new}" karpenter.sh/do-not-disrupt=true --overwrite >/dev/null || true
  setstate drift.replacement "${new}"
  [[ "$(kubectl get nodeclaim "${new}" -o jsonpath='{.status.conditions[?(@.type=="Launched")].status}')" != True ]] && window=yes
  kubectl -n "${ns}" delete pod -l app.kubernetes.io/name=scarpenter --grace-period=0 --force >/dev/null
  before="$(instances_of_claim "${new}")"
  if [[ "${window}" == yes ]]; then
    note "restart: the controller was killed while ${new} was not launched; instances of the claim at that moment: '${before}'"
  else
    note "restart (opportunistic): ${new} was already launched when the controller was killed; the replay of Create was not exercised in this attempt"
  fi
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=180s
  # core retries Create while the catalog is not ready again (bounded by its 5-minute
  # launch timeout); the number is for the reader.
  note "restart: CatalogNotReady lines in the new controller's log: $(kubectl -n "${ns}" logs deploy/scarpenter 2>/dev/null | grep -c CatalogNotReady || true)"
  wait_until "the new controller reconciled the claim (launched, or gone)" 300 claim_settled "${new}"
  launched="$(kubectl get nodeclaim "${new}" -o jsonpath='{.status.conditions[?(@.type=="Launched")].status}' --ignore-not-found)"
  if [[ "${launched}" != True ]]; then
    note "restart (opportunistic): the claim ${new} is gone after the restart (consolidated or replaced); only the no-orphan check applies"
    return 0
  fi
  after="$(instances_of_claim "${new}")"
  [[ "$(wc -w <<<"${after}" | tr -d ' ')" -le 1 ]] || fail "the claim ${new} owns several instances after the restart: ${after}"
  assert_eq "the claim's instance is the one in its annotation" "$(instance_of_claim "${new}")" "${after}"
  if [[ "${window}" == yes && -n "${before}" ]]; then
    # The one assertion that can fail on the window: a second RunInstances for the claim.
    assert_eq "the replay of Create found the first launch's instance (no second RunInstances)" "${before}" "${after}"
    note "restart: the replay of Create was exercised (instance ${after})"
  elif [[ "${window}" == yes ]]; then
    note "restart (opportunistic): the instance did not exist yet when the controller was killed; Create ran from the start"
  fi
}

# claim_settled <claim>: launched, or no longer there.
claim_settled() {
  local out
  out="$(kubectl get nodeclaim "$1" -o jsonpath='{.status.conditions[?(@.type=="Launched")].status}' --ignore-not-found)" || return 1
  [[ "${out}" == True ]] || [[ -z "$(kubectl get nodeclaim "$1" -o name --ignore-not-found)" ]]
}

# instances_of_claim <claim>: the instances tagged with the claim that exist.
instances_of_claim() {
  ecs DescribeInstances --Tag.1.Key "${label_domain}/nodeclaim" --Tag.1.Value "$1" |
    jq -r '.Instances.Instance[]?.InstanceId' | xargs
}

# assert_no_orphans: every instance of the cluster belongs to a NodeClaim that exists.
assert_no_orphans() {
  wait_until "no orphaned instance" 240 no_orphans
}

no_orphans() {
  local claims ids id
  claims="$(kjson nodeclaims | jq -r --arg k "${label_domain}/instance-ref" '.items[] | .metadata.annotations[$k] // empty' | sed 's#.*/##')"
  ids="$(cluster_instances)"
  for id in ${ids}; do
    grep -qx "${id}" <<<"${claims}" || {
      echo "instance ${id} has no NodeClaim"
      return 1
    }
  done
}

# active_instances: the cluster's instances that are not on their way out (ECS has no
# terminated state: Stopping is the closest, and a released instance is simply not listed).
active_instances() {
  local -a tag
  mapfile -t tag < <(cluster_tag_args)
  ecs DescribeInstances "${tag[@]}" --PageSize 100 | jq -r '.Instances.Instance[]? | select(.Status | IN("Pending", "Starting", "Running")) | .InstanceId'
}

# scalezero (plan 10 scenario 3, the roadmap's exit criterion): deleting the workloads
# releases every instance within E2E_SCALE_DOWN_DEADLINE seconds of the Pods' deletion
# (WhenEmpty, 0 s); the nodes, NodeClaims and bootstrap tokens go too; the quota usage
# falls with the instances.
scn_scalezero() {
  local t0 elapsed
  drop_nodeshells
  release_do_not_disrupt
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every instance stopping or gone" "${E2E_SCALE_DOWN_DEADLINE}" is_empty active_instances
  note "scale to zero: every instance was stopping (DeleteInstance issued) $(($(now) - t0))s after the Pods' deletion"
  # The deadline counts from the Pods' deletion for both.
  wait_until "every instance of the cluster released" "$((E2E_SCALE_DOWN_DEADLINE > $(now) - t0 ? E2E_SCALE_DOWN_DEADLINE - ($(now) - t0) : 1))" is_empty cluster_instances
  elapsed=$(($(now) - t0))
  note "scale to zero: every instance released ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s)"
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name
  log "the quota usage follows the instances down"
  local m
  m="$(mktemp)"
  wait_until "the pay-as-you-go usage equals ECS's" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" od
  wait_until "the spot usage equals ECS's" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" spot
  wait_until "no reservation is left" 120 quota_reserved_zero "${m}" "${bucket_od}"
  rm -f "${m}"
}

# uninstall: the procedure of docs/how-to-guides/install.md#uninstall, on the live account.
scn_uninstall() {
  log "1. SCarpenter's NodePools (selected by nodeClassRef.group), then wait for the NodeClaims"
  kjson nodepools | jq -r '.items[] | select(.spec.template.spec.nodeClassRef.group == "scarpenter.sparecores.com") | .metadata.name' |
    xargs -r kubectl delete nodepool --wait=true --timeout=300s
  wait_until "no NodeClaim of SCarpenter left" 300 is_value 0 scarpenter_claims
  log "2. the SCNodeClasses; the managed resources go by their lifecycle"
  kubectl delete scnodeclass --all --wait=true --timeout=300s
  wait_until "no instance of the cluster" 300 is_empty cluster_instances
  # With the NodeClasses gone the managed security groups fall under the default idle
  # lifetime (managedResources.defaultTTL, 5 minutes in the lane's values; 24 hours by default).
  wait_until "no security group of SCarpenter left" 900 is_empty managed_groups_ids
  wait_until "no deployment set left" 60 is_empty cluster_deployment_sets
  log "3. helm uninstall"
  helm uninstall scarpenter --namespace "${ns}" --wait --timeout 3m
  wait_until "the controller gone" 120 is_empty kubectl -n "${ns}" get pods -l app.kubernetes.io/name=scarpenter -o name
  # What the uninstall leaves behind (the CRDs and what the user created) is listed in
  # the how-to; what is tagged with the cluster is checked by the teardown, the final
  # no-leftover check of the lane.
}

# scarpenter_claims: how many NodeClaims belong to an SCNodeClass.
scarpenter_claims() {
  kjson nodeclaims | jq '[.items[] | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'
}

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 arm64 spot imds db described byo negative image quota drift scalezero uninstall)
selected=()
if [[ -z "${E2E_SCENARIOS:-}" || "${E2E_SCENARIOS}" == "all" ]]; then
  selected=("${all_scenarios[@]}")
else
  for s in "${all_scenarios[@]}"; do
    [[ ",${E2E_SCENARIOS}," == *",${s},"* ]] && selected+=("${s}")
  done
  for s in ${E2E_SCENARIOS//,/ }; do
    [[ " ${all_scenarios[*]} " == *" ${s} "* ]] || {
      echo "unknown scenario ${s} (known: ${all_scenarios[*]})" >&2
      exit 2
    }
  done
fi

case "${E2E_FAIL_FAST}" in
  true | false) ;;
  *)
    echo "E2E_FAIL_FAST must be true or false, not '${E2E_FAIL_FAST}'" >&2
    exit 2
    ;;
esac

results=()
failed=0
failed_name=""
run_scenario() { # run_scenario <name>
  local name="$1" rc start sel
  start="$(now)"
  log "scenario ${name}"
  export E2E_CURRENT_SCENARIO="${name}"
  set +e
  (
    set -euo pipefail
    "scn_${name}"
  ) 2>&1 | tee "${E2E_OUT}/logs/${name}.log"
  rc="${PIPESTATUS[0]}"
  set -e
  if [[ "${rc}" -eq 0 ]]; then
    results+=("PASS ${name} ($(($(now) - start))s)")
  else
    results+=("FAIL ${name} ($(($(now) - start))s)")
    failed=1
    [[ -n "${failed_name}" ]] || failed_name="${name}"
    # What the cluster looks like right now, redacted, before anything else happens.
    sel="$(getstate "${name}.selector")"
    failure_snapshot "${name}" "${sel:-scarpenter-e2e=true}" || true
  fi
}

run_scenario preflight
if [[ "${failed}" -ne 0 ]]; then
  printf '%s\n' "${results[@]}" | tee "${E2E_OUT}/report.txt"
  exit 1
fi

# The prelude: start the workloads of the selected scenarios together; the scenarios
# wait for them (their own start is idempotent and reports a real failure).
log "starting the workloads"
for s in "${selected[@]}"; do
  if declare -F "start_${s}" >/dev/null; then
    # In a subshell: a start that fails must not end the runner.
    ("start_${s}") || echo "the start of ${s} failed; the scenario will report it"
  fi
done

for s in "${selected[@]}"; do
  if [[ "${failed}" -ne 0 && "${E2E_FAIL_FAST}" == true ]]; then
    results+=("SKIP ${s} (fail-fast: ${failed_name} failed)")
    continue
  fi
  run_scenario "${s}"
done

printf '\n'
printf '%s\n' "${results[@]}" | tee "${E2E_OUT}/report.txt"
exit "${failed}"
