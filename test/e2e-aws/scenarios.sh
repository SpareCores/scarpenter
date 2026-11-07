#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5: the scenarios, run against the cluster that bootstrap.sh and install.sh built
# (docs/plans/v1/wip/10-aws-e2e.md; docs/how-to-guides/testing.md#the-l5-lane lists
# what each proves and how to read a failure).
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 arm64 db described byo local negative quota drift scalezero uninstall
#                   (preflight always runs first, and ends the run when it fails)
#
# Each scenario is a function that runs in a subshell with `set -e`: the first failed
# assertion ends it, the runner records the result and goes on. Waits have explicit
# deadlines. Workloads of all selected scenarios are started together
# (the prelude), so that the nodes boot in parallel; a scenario then only waits and checks.
# The exit status is 1 when a scenario failed. A run needs about 16 Standard on-demand
# and 4 Standard spot vCPUs of quota in the region (preflight checks).
#
# Needs: kubectl, jq, helm, the aws CLI with the role's credentials, envsubst.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-aws/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT AWS_REGION KUBECONFIG
mkdir -p "${E2E_OUT}/state" "${E2E_OUT}/logs"
state="${E2E_OUT}/state"

# ------------------------------------------------------------- state --------

setstate() { printf '%s' "$2" >"${state}/$1"; } # setstate <name> <value>
getstate() { cat "${state}/$1" 2>/dev/null || true; }

# pick_zone: a zone of the region for the scenarios that pin one (the placement group,
# the subnet of the bring-your-own network): the first available zone by name.
pick_zone() {
  local z
  z="$(getstate zone)"
  if [[ -z "${z}" ]]; then
    z="$(ec2 describe-availability-zones --filters Name=zone-type,Values=availability-zone Name=state,Values=available \
      --query 'sort_by(AvailabilityZones,&ZoneName)[0].ZoneName' --output text)"
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
  wait_until "the Pod ${app} running" "${E2E_NODE_TIMEOUT}" pod_running "app=${app}"
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

# tag_of <instance json> <tag key>
tag_of() { jq -r --arg k "$2" '(.Tags // [])[] | select(.Key == $k) | .Value' <<<"$1"; }

# vcpus_of <instance type without the vendor prefix>
vcpus_of() { ec2 describe-instance-types --instance-types "$1" --query 'InstanceTypes[0].VCpuInfo.DefaultVCpus' --output text; }

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_arm64() { probe e2e-arm64 e2e-arm64 "${E2E_TYPE_ARM64}" spot; }
start_db() {
  pick_zone
  render "${here}/manifests/db.yaml" | kubectl apply -f -
}
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }

# start_byo: the subnet and the security group the workflow creates with the AWS CLI
# in the default VPC (tagged with the cluster, the run's creation time and the
# selector tag), the NodeClass that uses them, and the workload.
start_byo() {
  pick_zone
  local vpc cidr prefix subnet sg tags
  vpc="$(ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)"
  [[ "${vpc}" != "None" ]] || fail "the region has no default VPC: the bring-your-own scenario needs one"
  tags="{Key=${ctag},Value=${E2E_CLUSTER}},{Key=scarpenter-e2e-byo,Value=${E2E_CLUSTER}},{Key=ci-created-at,Value=${E2E_CREATED_AT}}"
  subnet="$(ec2 describe-subnets --filters "Name=tag:scarpenter-e2e-byo,Values=${E2E_CLUSTER}" --query 'Subnets[0].SubnetId' --output text)"
  if [[ "${subnet}" == "None" ]]; then
    cidr="$(ec2 describe-vpcs --vpc-ids "${vpc}" --query 'Vpcs[0].CidrBlock' --output text)"
    prefix="$(cut -d. -f1,2 <<<"${cidr}")"
    # The default subnets are /20 blocks from the start of the /16; 200.0/24 is free.
    subnet="$(ec2 create-subnet --vpc-id "${vpc}" --cidr-block "${prefix}.200.0/24" --availability-zone "${E2E_BYO_ZONE}" \
      --tag-specifications "ResourceType=subnet,Tags=[${tags},{Key=Name,Value=${E2E_CLUSTER}-byo}]" \
      --query 'Subnet.SubnetId' --output text)"
  fi
  sg="$(ec2 describe-security-groups --filters "Name=tag:scarpenter-e2e-byo,Values=${E2E_CLUSTER}" --query 'SecurityGroups[0].GroupId' --output text)"
  if [[ "${sg}" == "None" ]]; then
    sg="$(ec2 create-security-group --group-name "${E2E_CLUSTER}-byo" --description "scarpenter e2e: brought by the workflow" \
      --vpc-id "${vpc}" --tag-specifications "ResourceType=security-group,Tags=[${tags},{Key=Name,Value=${E2E_CLUSTER}-byo}]" \
      --query 'GroupId' --output text)"
  fi
  setstate byo.subnet "${subnet}"
  setstate byo.sg "${sg}"
  render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

start_local() {
  [[ -n "${E2E_LOCAL_STORAGE_TYPE}" ]] || return 0
  render "${here}/manifests/nodeclass-local.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-local.yaml" | kubectl apply -f -
  render "${here}/manifests/local.yaml" | kubectl apply -f -
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  for t in aws kubectl jq helm envsubst; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  aws sts get-caller-identity --query Arn --output text >/dev/null || fail "the AWS credentials do not work"

  # 1. The Flatcar AMIs (overview section 11.3): exactly one match per architecture.
  local arch suffix n
  for arch in amd64 arm64; do
    suffix=""
    [[ "${arch}" == "arm64" ]] && suffix="-arm64"
    n="$(ec2 describe-images --owners 075585003325 \
      --filters "Name=name,Values=Flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}${suffix}-hvm" Name=state,Values=available \
      --query 'length(Images)' --output text)"
    assert_eq "Flatcar AMIs named Flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}${suffix}-hvm" 1 "${n}"
  done

  # 2. The quota the suite needs (the default of a new account, 5 vCPUs, is not enough).
  local od spot
  od="$(aws --region "${AWS_REGION}" service-quotas get-service-quota --service-code ec2 --quota-code "${standard_od}" --query Quota.Value --output text)"
  spot="$(aws --region "${AWS_REGION}" service-quotas get-service-quota --service-code ec2 --quota-code "${standard_spot}" --query Quota.Value --output text)"
  [[ "$(num "${od}")" -ge 16 ]] || fail "the Standard on-demand vCPU quota of ${AWS_REGION} is ${od}; the suite needs 16 (raise it, docs/how-to-guides/testing.md#the-l5-lane)"
  [[ "$(num "${spot}")" -ge 4 ]] || fail "the Standard spot vCPU quota of ${AWS_REGION} is ${spot}; the suite needs 4"

  # 3. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc
  for nc in e2e-amd64 e2e-arm64 e2e-drift; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.aws.conditions[?(@.type=="VendorReady")].status}')"
    assert_eq "scnodeclass/${nc} ImagesReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.aws.conditions[?(@.type=="ImagesReady")].status}')"
  done
  pick_zone
  note "zone of the placement group and the bring-your-own subnet: ${E2E_PG_ZONE}"
}

# Prelude: the baseline of nothing; the workloads of every selected scenario start now.
scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim iid zone zoneid j
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  iid="$(getstate e2e-amd64.iid)"

  log "node Ready, labels (overview section 5.5)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "region" "${AWS_REGION}" "$(node_label "${node}" topology.kubernetes.io/region)"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" aws "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${E2E_TYPE_AMD64#aws.}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_eq "family" "${E2E_TYPE_AMD64#aws.}" "$(node_label "${node}" "${label_domain}/family").${E2E_TYPE_AMD64##*.}"
  assert_eq "storage-profile" default "$(node_label "${node}" "${label_domain}/storage-profile")"
  assert_eq "placement-profile" default "$(node_label "${node}" "${label_domain}/placement-profile")"
  assert_eq "nodepool" e2e-amd64 "$(node_label "${node}" karpenter.sh/nodepool)"
  zone="$(node_label "${node}" topology.kubernetes.io/zone)"
  zoneid="$(node_label "${node}" "${label_domain}/zone-id")"
  assert_nonempty "zone name" "${zone}"
  assert_nonempty "zone ID" "${zoneid}"
  assert_eq "the zone ID of the zone name, from EC2" "${zoneid}" \
    "$(ec2 describe-availability-zones --zone-names "${zone}" --query 'AvailabilityZones[0].ZoneId' --output text)"

  log "the instance: tags, IMDS options, volumes (overview section 11.4)"
  j="$(instance_json "${iid}")"
  assert_eq "instance state" running "$(jq -r '.State.Name' <<<"${j}")"
  assert_eq "instance type" "${E2E_TYPE_AMD64#aws.}" "$(jq -r '.InstanceType' <<<"${j}")"
  assert_eq "availability zone" "${zone}" "$(jq -r '.Placement.AvailabilityZone' <<<"${j}")"
  assert_eq "lifecycle (on-demand has none)" null "$(jq -r '.InstanceLifecycle // "null"' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" "${ctag}")"
  assert_eq "tag nodeclaim" "${claim}" "$(tag_of "${j}" "${label_domain}/nodeclaim")"
  assert_eq "tag nodepool" e2e-amd64 "$(tag_of "${j}" "${label_domain}/nodepool")"
  assert_eq "tag nodeclass" e2e-amd64 "$(tag_of "${j}" "${label_domain}/nodeclass")"
  assert_eq "tag managed-by" scarpenter "$(tag_of "${j}" "${label_domain}/managed-by")"
  assert_eq "tag Name" "${claim}" "$(tag_of "${j}" Name)"
  assert_eq "tag ci-created-at (the NodeClass's spec.tags)" "${E2E_CREATED_AT}" "$(tag_of "${j}" ci-created-at)"
  assert_eq "IMDSv2 required" required "$(jq -r '.MetadataOptions.HttpTokens' <<<"${j}")"
  assert_eq "IMDS hop limit" 1 "$(jq -r '.MetadataOptions.HttpPutResponseHopLimit' <<<"${j}")"
  assert_eq "IMDS tags in metadata" disabled "$(jq -r '.MetadataOptions.InstanceMetadataTags' <<<"${j}")"
  assert_nonempty "public IPv4" "$(jq -r '.PublicIpAddress // empty' <<<"${j}")"
  # The root volume: an encrypted gp3 volume made from the unencrypted Flatcar snapshot
  # (carried over from the plan 08 review), 20 GiB as the NodeClass says.
  local vols vol
  vols="$(jq -r '.BlockDeviceMappings[] | select(.Ebs.DeleteOnTermination == true) | .Ebs.VolumeId' <<<"${j}")"
  assert_eq "volumes with DeleteOnTermination" 1 "$(wc -l <<<"${vols}" | tr -d ' ')"
  vol="$(ec2 describe-volumes --volume-ids "${vols}" --query 'Volumes[0]')"
  assert_eq "root volume encrypted" true "$(jq -r '.Encrypted' <<<"${vol}")"
  assert_eq "root volume type" gp3 "$(jq -r '.VolumeType' <<<"${vol}")"
  assert_eq "root volume size" 20 "$(jq -r '.Size' <<<"${vol}")"
  assert_eq "tag cluster on the volume" "${E2E_CLUSTER}" "$(tag_of "${vol}" "${ctag}")"

  log "the claim: price, bootstrap token, serving certificate"
  local price
  price="$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price" '.metadata.annotations[$k] // empty')"
  assert_nonempty "price annotation" "${price}"
  awk -v p="${price}" 'BEGIN { exit !(p + 0 > 0) }' || fail "the price ${price} is not positive"
  assert_eq "price-incomplete (gp3 defaults are priced completely)" "" \
    "$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price-incomplete" '.metadata.annotations[$k] // empty')"
  wait_until "the bootstrap token Secret gone" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim=${claim}" -o name
  wait_until "the serving CSR approved and issued" 120 csr_approved "${node}"

  log "kubectl logs and exec (the API server reaches the kubelet over the tailnet)"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_match "the log of the probe" scarpenter-e2e-up "$(kubectl logs "${pod}")"
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"

  log "on the node: the unit-level checks of L4 scenarios Q09 and Q10, on a real node"
  node_unit_checks "${node}"

  log "cluster DNS and the instance metadata service from a Pod"
  local dns
  dns="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  assert_match "the kubelet's clusterDNS is the kube-dns Service" "${dns//./\\.}" "$(node_shell "${node}" "grep -A3 '^clusterDNS' /var/lib/kubelet/config.yaml" | tr '\n' ' ')"
  wait_until "a Pod resolves a cluster Service" 120 kubectl exec "${pod}" -- nslookup kubernetes.default.svc.cluster.local
  # An IMDSv2 token request (PUT) from the node answers; from a Pod it needs one hop
  # more than the limit of 1 and gets no answer.
  local put='printf "PUT /latest/api/token HTTP/1.1\r\nHost: 169.254.169.254\r\nX-aws-ec2-metadata-token-ttl-seconds: 60\r\nConnection: close\r\n\r\n" | nc -w 4 169.254.169.254 80 | head -n1'
  assert_match "IMDSv2 from the node itself" " 200 " "$(node_pod_exec "${node}" "${put}" || true)"
  assert_eq "IMDSv2 from a Pod (hop limit 1)" "" "$(kubectl exec "${pod}" -- sh -c "${put}" || true)"
  drop_nodeshells
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

# node_unit_checks <node>: what L4 scenarios Q09 and Q10 check inside a VM, on an AWS
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
  local pod node claim iid j req
  pod="$(getstate e2e-arm64.pod)"
  node="$(getstate e2e-arm64.node)"
  claim="$(getstate e2e-arm64.claim)"
  iid="$(getstate e2e-arm64.iid)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_ARM64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "arch" arm64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "capacity-type" spot "$(node_label "${node}" karpenter.sh/capacity-type)"
  j="$(instance_json "${iid}")"
  assert_eq "Architecture" arm64 "$(jq -r '.Architecture' <<<"${j}")"
  assert_eq "lifecycle" spot "$(jq -r '.InstanceLifecycle' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" "${ctag}")"
  # The spot request carries the cluster tag (the policy requires it on RunInstances).
  req="$(ec2 describe-spot-instance-requests --filters "Name=instance-id,Values=${iid}" --query 'SpotInstanceRequests[0]')"
  assert_nonempty "the spot request of the instance" "$(jq -r '.SpotInstanceRequestId // empty' <<<"${req}")"
  assert_eq "spot request tag cluster" "${E2E_CLUSTER}" "$(tag_of "${req}" "${ctag}")"
  assert_eq "spot request type" one-time "$(jq -r '.Type' <<<"${req}")"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  assert_eq "tailscaled, the join unit and the kubelet are active (arm64 sysext)" "active,active,active" \
    "$(node_shell "${node}" 'systemctl is-active tailscaled scarpenter-tailscale-up.service kubelet' | paste -sd, -)"
  drop_nodeshells
  note "arm64 spot claim ${claim}"
}

scn_db() {
  start_db
  wait_until "the database and the client running" $((E2E_NODE_TIMEOUT + 300)) pod_running "scarpenter-e2e=true,app in (e2e-db,e2e-client)"
  local dbpod clpod dbnode clnode dbiid cliid dbclaim pg pgname dbj clj vol
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

  log "one cluster placement group, named and tagged by SCarpenter"
  pg="$(ec2 describe-placement-groups --filters "Name=tag:${ctag},Values=${E2E_CLUSTER}" --query 'PlacementGroups')"
  assert_eq "placement groups of the cluster" 1 "$(jq 'length' <<<"${pg}")"
  pgname="$(jq -r '.[0].GroupName' <<<"${pg}")"
  # <vendor prefix>-<cluster>-placement-groups-<region>-<hash>, shortened by the engine to 63 characters.
  assert_match "the group's name" "^scarpenter-" "${pgname}"
  [[ "${#pgname}" -le 63 ]] || fail "the group's name ${pgname} is longer than 63 characters"
  assert_eq "strategy" cluster "$(jq -r '.[0].Strategy' <<<"${pg}")"
  assert_eq "tag managed-by" scarpenter "$(tag_of "$(jq '.[0]' <<<"${pg}")" "${label_domain}/managed-by")"
  assert_eq "tag resource-kind" placement-groups "$(tag_of "$(jq '.[0]' <<<"${pg}")" "${label_domain}/resource-kind")"
  assert_eq "tag nodeclass" e2e-amd64 "$(tag_of "$(jq '.[0]' <<<"${pg}")" "${label_domain}/nodeclass")"
  assert_eq "the database is in the group" "${pgname}" "$(jq -r '.Placement.GroupName' <<<"${dbj}")"
  assert_eq "the client is in the group" "${pgname}" "$(jq -r '.Placement.GroupName' <<<"${clj}")"
  assert_eq "one zone" "${E2E_PG_ZONE}" "$(jq -r '.Placement.AvailabilityZone' <<<"${dbj}")"
  assert_eq "one zone (client)" "${E2E_PG_ZONE}" "$(jq -r '.Placement.AvailabilityZone' <<<"${clj}")"

  log "node labels of the applied profiles"
  assert_eq "database storage-profile" db-io2 "$(node_label "${dbnode}" "${label_domain}/storage-profile")"
  assert_eq "database placement-profile" colocated "$(node_label "${dbnode}" "${label_domain}/placement-profile")"
  assert_eq "client storage-profile" default "$(node_label "${clnode}" "${label_domain}/storage-profile")"
  assert_eq "client placement-profile" colocated "$(node_label "${clnode}" "${label_domain}/placement-profile")"
  assert_eq "database run key" "pg-${E2E_RUN_ID}" "$(node_label "${dbnode}" "${label_domain}/placement-group")"
  assert_eq "client run key" "pg-${E2E_RUN_ID}" "$(node_label "${clnode}" "${label_domain}/placement-group")"
  assert_eq "profile-hashes annotation names the storage profile" true \
    "$(kubectl get nodeclaim "${dbclaim}" -o json | jq -r --arg k "${label_domain}/profile-hashes" '.metadata.annotations[$k] | fromjson | has("storage/db-io2")')"
  assert_eq "price-incomplete (provisioned IOPS are not priced)" true \
    "$(kubectl get nodeclaim "${dbclaim}" -o json | jq -r --arg k "${label_domain}/price-incomplete" '.metadata.annotations[$k] // empty')"

  log "the io2 volume: attached, encrypted, mounted at the profile's path"
  vol="$(ec2 describe-volumes --filters "Name=attachment.instance-id,Values=${dbiid}" Name=volume-type,Values=io2 --query 'Volumes[0]')"
  assert_nonempty "the io2 volume of the database" "$(jq -r '.VolumeId // empty' <<<"${vol}")"
  assert_eq "io2 size" 4 "$(jq -r '.Size' <<<"${vol}")"
  assert_eq "io2 IOPS" 100 "$(jq -r '.Iops' <<<"${vol}")"
  assert_eq "io2 encrypted" true "$(jq -r '.Encrypted' <<<"${vol}")"
  assert_eq "io2 attached" attached "$(jq -r '.Attachments[0].State' <<<"${vol}")"
  assert_eq "io2 DeleteOnTermination" true "$(jq -r '.Attachments[0].DeleteOnTermination' <<<"${vol}")"
  local volid src
  volid="$(jq -r '.VolumeId' <<<"${vol}")"
  src="$(node_shell "${dbnode}" 'findmnt -no SOURCE /var/lib/pgdata')"
  assert_nonempty "the device mounted at /var/lib/pgdata" "${src}"
  assert_eq "the filesystem at /var/lib/pgdata" xfs "$(node_shell "${dbnode}" 'findmnt -no FSTYPE /var/lib/pgdata')"
  # The NVMe serial of an EBS volume is its ID without the dash: the serial pattern
  # of the first-boot unit found this very device.
  assert_eq "the device's serial is the volume ID" "${volid//-/}" "$(node_shell "${dbnode}" "lsblk -dno SERIAL ${src}" | tr -d ' ')"
  assert_eq "the Pod's file is on the mounted volume" hello-from-the-volume "$(node_shell "${dbnode}" 'cat /var/lib/pgdata/hello')"
  # Plan 07 review N1: the device unit of the volume's label has the long job timeout.
  assert_match "JobRunningTimeoutSec=240 on the device unit of the label" "dev-disk-by.*scv.*JobRunningTimeoutSec=240|JobRunningTimeoutSec=240" \
    "$(node_shell "${dbnode}" 'grep -r JobRunningTimeoutSec /etc/systemd/system/ 2>/dev/null | grep dev-disk-by' | head -n1)"
  local attach launch
  attach="$(date -d "$(jq -r '.Attachments[0].AttachTime' <<<"${vol}")" +%s)"
  launch="$(date -d "$(jq -r '.LaunchTime' <<<"${dbj}")" +%s)"
  note "the data volume was attached $((attach - launch))s after the instance's launch (the unit waits for the device for 180 s; a slow attach of about 100 s cannot be provoked, only observed)"

  log "isolation: the NodeClass's managed security group allows its members"
  assert_eq "a managed group with a rule that refers to itself" true "$(ec2 describe-security-groups \
    --filters "Name=tag:${ctag},Values=${E2E_CLUSTER}" "Name=tag:${label_domain}/managed-by,Values=scarpenter" --query 'SecurityGroups' |
    jq 'any(.[]; .GroupId as $g | any(.IpPermissions[]?.UserIdGroupPairs[]?; .GroupId == $g))')"

  log "delete both workloads: the group goes"
  drop_nodeshells
  kubectl delete statefulset e2e-db --wait=false
  kubectl delete deployment e2e-client --wait=false
  wait_until "the instances terminated" 300 is_empty cluster_instances_of "${dbiid}" "${cliid}"
  # perUse: two minutes of grace for launches in flight, then the next resource check
  # and garbage collection (30 s and 1 m here).
  wait_until "the placement group deleted" 420 is_value 0 ec2 describe-placement-groups --filters "Name=tag:${ctag},Values=${E2E_CLUSTER}" --query 'length(PlacementGroups)' --output text
  wait_until "the volume gone" 120 is_empty ec2 describe-volumes --filters "Name=volume-id,Values=${volid}" --query 'Volumes[].VolumeId' --output text
}

# cluster_instances_of <id>...: those of the instances that are not terminated.
cluster_instances_of() {
  # A filter, not --instance-ids: a terminated instance is forgotten after an hour and
  # asking for it by ID would then be an error.
  local ids
  ids="$(IFS=,; echo "$*")"
  ec2 describe-instances --filters "Name=instance-id,Values=${ids}" 'Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down' \
    --query 'Reservations[].Instances[].InstanceId' --output text
}

scn_described() {
  start_described
  wait_until "the Pod running" "${E2E_NODE_TIMEOUT}" pod_running app=e2e-described
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
  [[ "${t}" != "${E2E_TYPE_AMD64}" ]] || fail "the type ${t} has too little memory for the request (memory-mib > 3000)"
  [[ "${vcpus}" -gt 1 && "${vcpus}" -lt 3 ]] || fail "vcpus ${vcpus} is outside (1, 3)"
  [[ "${mem}" -gt 3000 ]] || fail "memory ${mem} MiB is not above 3000"
  [[ "${pct}" -gt 0 ]] || fail "the percentile ${pct} is not above 0"
  assert_eq "the instance is of the chosen type" "${t#aws.}" "$(jq -r '.InstanceType' <<<"$(instance_json "${iid}")")"
  # The same facts from EC2, independent of the catalog.
  assert_eq "EC2's vCPUs of the type" "${vcpus}" "$(vcpus_of "${t#aws.}")"
}

scn_byo() {
  start_byo
  local subnet sg
  subnet="$(getstate byo.subnet)"
  sg="$(getstate byo.sg)"
  wait_until "ResourcesReady on the NodeClass" 180 is_value True \
    kubectl get scnodeclass e2e-use -o jsonpath='{.status.vendors.aws.conditions[?(@.type=="ResourcesReady")].status}'
  wait_until "the Pod running" "${E2E_NODE_TIMEOUT}" pod_running app=e2e-use
  local pod node iid j
  pod="$(kubectl get pod -l app=e2e-use -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  iid="$(instance_of_claim "$(claim_of_node "${node}")")"
  j="$(instance_json "${iid}")"
  assert_eq "the instance is in the brought subnet" "${subnet}" "$(jq -r '.SubnetId' <<<"${j}")"
  assert_eq "the instance uses the brought security group" true "$(jq -r --arg sg "${sg}" '[.SecurityGroups[].GroupId] | index($sg) != null' <<<"${j}")"
  assert_eq "the zone of the subnet" "${E2E_BYO_ZONE}" "$(jq -r '.Placement.AvailabilityZone' <<<"${j}")"

  log "SCarpenter must not touch what it did not create"
  drop_nodeshells
  kubectl delete deployment e2e-use --wait=false
  wait_until "the instance terminated" 300 is_empty cluster_instances_of "${iid}"
  kubectl delete nodepool e2e-use --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-use --wait=true --timeout=120s
  # Past a garbage collection (1 m), a resource check (30 s) and the status controller:
  # a `use` reference is never modified or deleted.
  sleep 120
  assert_eq "the subnet still exists" "${subnet}" "$(ec2 describe-subnets --subnet-ids "${subnet}" --query 'Subnets[0].SubnetId' --output text)"
  assert_eq "the security group still exists" "${sg}" "$(ec2 describe-security-groups --group-ids "${sg}" --query 'SecurityGroups[0].GroupId' --output text)"
  note "the brought subnet ${subnet} and group ${sg} are deleted by the teardown"
}

scn_local() {
  if [[ -z "${E2E_LOCAL_STORAGE_TYPE}" ]]; then
    note "local: skipped (E2E_LOCAL_STORAGE_TYPE is not set; it names a type with instance-store disks, for example aws.c6gd.medium or aws.m6id.large)"
    return 0
  fi
  start_local
  wait_until "the Pod running" "${E2E_NODE_TIMEOUT}" pod_running app=e2e-local
  local pod node
  pod="$(kubectl get pod -l app=e2e-local -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  assert_eq "the filesystem at /var/lib/scratch" ext4 "$(node_shell "${node}" 'findmnt -no FSTYPE /var/lib/scratch')"
  # Instance store devices have stable by-id names (docs/reference/node-settings.md).
  local n
  n="$(node_shell "${node}" 'ls /dev/disk/by-id | grep -c Amazon_EC2_NVMe_Instance_Storage' || true)"
  [[ "${n}" -ge 1 ]] || fail "no instance-store device in /dev/disk/by-id"
  note "local: ${n} instance-store device(s), mounted at /var/lib/scratch"
  drop_nodeshells
  kubectl delete deployment e2e-local --wait=false
}

# negative: NodeClasses that ask for what the default network cannot give are refused
# or gated with a reason, and nothing is launched (plan 08 review items 17 and 18).
scn_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
  wait_until "publicIPv4: false is refused on the default VPC" 180 is_value False \
    kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.aws.conditions[?(@.type=="VendorReady")].status}'
  assert_eq "the reason" ValidationFailed \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.aws.conditions[?(@.type=="VendorReady")].reason}')"
  assert_match "the message names the fix" "(use|subnets)" \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.aws.conditions[?(@.type=="VendorReady")].message}')"
  wait_until "publicIPv6 gates the zones of a default VPC without IPv6" 240 is_value False \
    kubectl get scnodeclass e2e-ipv6 -o jsonpath='{.status.vendors.aws.conditions[?(@.type=="ResourcesReady")].status}'
  assert_match "the message names IPv6" "[Ii][Pp][Vv]6" \
    "$(kubectl get scnodeclass e2e-ipv6 -o jsonpath='{.status.vendors.aws.conditions[?(@.type=="ResourcesReady")].message}')"
  kubectl delete scnodeclass e2e-noipv4 e2e-ipv6 --wait=true --timeout=120s
}

# quota (overview sections 13.4 and 11.8): what the controller reports equals what AWS
# says. Run with the nodes of the other scenarios up and no launch in flight.
scn_quota() {
  settle_fixtures
  local m code cli
  m="$(mktemp)"
  log "the limits of all 18 buckets equal GetServiceQuota"
  local timeout=240
  for code in "${quota_codes[@]}"; do
    cli="$(aws --region "${AWS_REGION}" service-quotas get-service-quota --service-code ec2 --quota-code "${code}" --query Quota.Value --output text 2>/dev/null || true)"
    if [[ -z "${cli}" ]]; then
      note "quota ${code}: GetServiceQuota has no answer in ${AWS_REGION}"
      continue
    fi
    wait_until "scarpenter_quota_limit ${code} = ${cli}" "${timeout}" quota_limit_is "${m}" "${code}" "${cli}"
    timeout=90 # the first one waited for the controller's first limits refresh
  done

  log "usage: what the controller counts is what EC2 says is running"
  wait_until "scarpenter_quota_used of the Standard on-demand bucket equals the instances of EC2" 300 quota_used_is "${m}" od
  wait_until "scarpenter_quota_used of the Standard spot bucket equals the instances of EC2 (no lingering spot request)" 300 quota_used_is "${m}" spot
  local od_used spot_used
  od_used="$(metric_value "${m}" scarpenter_quota_used "${AWS_REGION}" "${standard_od}")"
  spot_used="$(metric_value "${m}" scarpenter_quota_used "${AWS_REGION}" "${standard_spot}")"
  [[ "$(num "${od_used}")" -ge "$(vcpus_of "${E2E_TYPE_AMD64#aws.}")" ]] || fail "the on-demand usage ${od_used} does not include the launched instance"
  [[ "$(num "${spot_used}")" -ge "$(vcpus_of "${E2E_TYPE_ARM64#aws.}")" ]] || fail "the spot usage ${spot_used} does not include the launched spot instance"
  wait_until "the in-flight reservations are gone (on-demand)" 240 quota_reserved_zero "${m}" "${standard_od}"
  wait_until "the in-flight reservations are gone (spot)" 240 quota_reserved_zero "${m}" "${standard_spot}"

  log "the launched types are offered in their zones"
  local t zid
  for t in e2e-amd64:"${E2E_TYPE_AMD64}" e2e-arm64:"${E2E_TYPE_ARM64}"; do
    zid="$(node_label "$(getstate "${t%%:*}.node")" "${label_domain}/zone-id")"
    assert_eq "${t#*:} in DescribeInstanceTypeOfferings of ${zid}" 1 "$(ec2 describe-instance-type-offerings --location-type availability-zone-id \
      --filters "Name=instance-type,Values=${t#*:aws.}" "Name=location,Values=${zid}" --query 'length(InstanceTypeOfferings)' --output text)"
  done
  rm -f "${m}"
}

quota_limit_is() { # quota_limit_is <metrics file> <code> <value from the CLI>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_limit "${AWS_REGION}" "$2")")" == "$(num "$3")" ]]
}

# expected_usage od|spot: the vCPUs of the account's pending and running instances of
# the Standard class, the way section 11.8 counts: on-demand without a lifecycle, spot
# with InstanceLifecycle=spot (a spot request that has an instance is not counted twice).
expected_usage() {
  local want="normal" total=0 n t
  [[ "$1" == "spot" ]] && want="spot"
  while read -r n t; do
    [[ -n "${t}" ]] || continue
    # Only the Standard class (families a c d h i m r t z): the suite launches no other.
    [[ "${t}" =~ ^[acdhimrtz] && ! "${t}" =~ ^(inf|hpc|dl|trn|vt|mac) ]] || continue
    total=$((total + n * $(vcpus_of "${t}")))
  done < <(ec2 describe-instances --filters Name=instance-state-name,Values=pending,running --output json |
    jq -r --arg want "${want}" '.Reservations[].Instances[] | select((.InstanceLifecycle // "normal") == $want) | .InstanceType' |
    sort | uniq -c)
  echo "${total}"
}

quota_used_is() { # quota_used_is <metrics file> od|spot
  local code="${standard_od}"
  [[ "$2" == "spot" ]] && code="${standard_spot}"
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_used "${AWS_REGION}" "${code}")")" == "$(expected_usage "$2")" ]]
}

quota_reserved_zero() { # quota_reserved_zero <metrics file> <code>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_reserved "${AWS_REGION}" "$2")")" == 0 ]]
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

# drift (plan 10 scenario 4) and the opportunistic controller restart of plan 07's review: a change to
# a profile no node uses drifts nothing; a change to the vendor block replaces the node,
# and the controller is killed while the replacement's Create is in flight.
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
    '{"spec":{"profiles":{"storage":{"scratch":{"aws":{"data":[{"name":"scratch","volumeType":"gp3","volumeSize":"2Gi","filesystem":"ext4","mountPath":"/var/lib/scratch"}]}}}}}}'
  wait_until "the NodeClass reconciled the change" 120 nodeclass_observed e2e-drift
  local i
  for i in 1 2 3 4 5 6; do
    sleep 10
    assert_eq "the claim is not drifted (${i}/6)" "" \
      "$(kubectl get nodeclaim "${old_claim}" -o jsonpath='{.status.conditions[?(@.type=="Drifted")].status}' | grep -x True || true)"
  done
  assert_eq "still the same claim" "${old_claim}" "$(claim_of_node "${old_node}")"

  log "the root volume size changes: the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p '{"spec":{"vendors":{"aws":{"storage":{"root":{"volumeSize":"24Gi"}}}}}}'
  restart_during_create e2e-drift "${old_claim}"
  wait_until "the Pod on another node" "${E2E_NODE_TIMEOUT}" pod_moved_from app=e2e-drift "${old_node}"
  release_do_not_disrupt
  local pod node claim iid
  pod="$(kubectl get pod -l app=e2e-drift -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  iid="$(instance_of_claim "${claim}")"
  [[ "${iid}" != "${old_iid}" ]] || fail "the replacement runs on the old instance"
  assert_eq "the replacement's root volume (the new size)" 24 \
    "$(ec2 describe-volumes --filters "Name=attachment.instance-id,Values=${iid}" --query 'Volumes[0].Size' --output text)"
  wait_until "the old instance terminated" 300 is_empty cluster_instances_of "${old_iid}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  assert_no_orphans
  # Not opportunistic: EC2 lists terminated instances for about an hour, so every
  # instance ever launched for the replacement claim is still visible here, even one the
  # garbage collector already terminated as an orphan. More than one is a second
  # RunInstances for the same claim (a replay that did not find the first launch).
  local repl ever
  repl="$(getstate drift.replacement)"
  ever="$(ec2 describe-instances --filters "Name=tag:${label_domain}/nodeclaim,Values=${repl}" \
    --query 'Reservations[].Instances[].InstanceId' --output text | wc -w | tr -d ' ')"
  [[ "${ever}" -le 1 ]] || fail "the claim ${repl} was launched ${ever} times (a second RunInstances after the restart)"
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
# (ClientToken = the claim UID, the controller replays it): after the restart the claim
# must not own two instances, and no instance may be orphaned.
#
# The window (the instance exists, Create has not returned) is narrow on AWS and no flag
# stretches it (the lane builds the production image; no test-only driver flag exists), so
# the replay assertions are OPPORTUNISTIC: when the attempt misses the window the scenario
# notes it and never fails for it. These carry-over items depend on it: plan 07's "a
# restart must assert the replay of Create ran" and plan 08's IdempotentParameterMismatch
# with changed user-data. What always holds and is always asserted: no instance is left
# without a NodeClaim (the caller), and a claim that is still there owns one instance.
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

# instances_of_claim <claim>: the non-terminated instances tagged with the claim.
instances_of_claim() {
  ec2 describe-instances --filters "Name=tag:${label_domain}/nodeclaim,Values=$1" \
    'Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down' \
    --query 'Reservations[].Instances[].InstanceId' --output text | tr '\t' ' ' | xargs
}

# assert_no_orphans: every instance of the cluster belongs to a NodeClaim that exists.
assert_no_orphans() {
  wait_until "no orphaned instance" 240 no_orphans
}

no_orphans() {
  local claims ids id
  claims="$(kjson nodeclaims | jq -r --arg k "${label_domain}/instance-ref" '.items[] | .metadata.annotations[$k] // empty' | sed 's#.*/##')"
  ids="$(cluster_instances running,pending)"
  for id in ${ids}; do
    grep -qx "${id}" <<<"${claims}" || {
      echo "instance ${id} has no NodeClaim"
      return 1
    }
  done
}

# scalezero (plan 10 scenario 3, the roadmap's exit criterion): deleting the workloads
# terminates every instance within E2E_SCALE_DOWN_DEADLINE seconds of the Pods' deletion
# (WhenEmpty, 0 s); the nodes, NodeClaims and bootstrap tokens go too; the quota usage
# falls with the instances.
scn_scalezero() {
  local uid iid t0 elapsed
  uid="$(getstate e2e-amd64.uid)"
  iid="$(getstate e2e-amd64.iid)"
  drop_nodeshells
  release_do_not_disrupt
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every instance shutting down or gone" "${E2E_SCALE_DOWN_DEADLINE}" is_empty cluster_instances pending,running,stopping,stopped
  note "scale to zero: every instance was shutting down (TerminateInstances issued) $(($(now) - t0))s after the Pods' deletion"
  # The deadline counts from the Pods' deletion for both.
  wait_until "every instance of the cluster terminated" "$((E2E_SCALE_DOWN_DEADLINE > $(now) - t0 ? E2E_SCALE_DOWN_DEADLINE - ($(now) - t0) : 1))" is_empty cluster_instances
  elapsed=$(($(now) - t0))
  note "scale to zero: every instance terminated ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s)"
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name
  if [[ -n "${uid}" ]]; then
    # The driver finds the instance of a replayed Create by the client-token filter,
    # which must still see a terminated instance (plan 08 review).
    assert_eq "the client-token filter finds the terminated instance" "${iid}" \
      "$(ec2 describe-instances --filters "Name=client-token,Values=${uid}" --query 'Reservations[0].Instances[0].InstanceId' --output text)"
  fi
  log "the quota usage follows the instances down (no lingering spot request counts)"
  local m lingering
  m="$(mktemp)"
  lingering="$(ec2 describe-spot-instance-requests --filters "Name=tag:${ctag},Values=${E2E_CLUSTER}" Name=state,Values=open,active \
    --query 'length(SpotInstanceRequests)' --output text)"
  note "spot requests still active while the usage is compared: ${lingering}"
  wait_until "the on-demand usage equals EC2's" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" od
  wait_until "the spot usage equals EC2's" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" spot
  wait_until "no reservation is left" 120 quota_reserved_zero "${m}" "${standard_od}"
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
  wait_until "no security group of SCarpenter left" 900 is_value 0 managed_groups
  wait_until "no placement group left" 60 is_value 0 ec2 describe-placement-groups --filters "Name=tag:${ctag},Values=${E2E_CLUSTER}" --query 'length(PlacementGroups)' --output text
  log "3. helm uninstall"
  helm uninstall scarpenter --namespace "${ns}" --wait --timeout 3m
  wait_until "the controller gone" 120 is_empty kubectl -n "${ns}" get pods -l app.kubernetes.io/name=scarpenter -o name
  # What the uninstall leaves behind (the CRDs and what the user created) is listed in
  # the how-to; what is tagged with the cluster is checked by the teardown.
}

# scarpenter_claims: how many NodeClaims belong to an SCNodeClass.
scarpenter_claims() {
  kjson nodeclaims | jq '[.items[] | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'
}

# managed_groups: the security groups SCarpenter created (the brought one carries no managed-by tag).
managed_groups() {
  ec2 describe-security-groups --filters "Name=tag:${ctag},Values=${E2E_CLUSTER}" "Name=tag:${label_domain}/managed-by,Values=scarpenter" \
    --query 'length(SecurityGroups)' --output text
}

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 arm64 db described byo local negative quota drift scalezero uninstall)
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

results=()
failed=0
run_scenario() { # run_scenario <name>
  local name="$1" rc start
  start="$(now)"
  log "scenario ${name}"
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
  run_scenario "${s}"
done

printf '\n'
printf '%s\n' "${results[@]}" | tee "${E2E_OUT}/report.txt"
exit "${failed}"
