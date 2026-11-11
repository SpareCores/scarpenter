#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5 for Azure: the scenarios, run against the cluster that bootstrap.sh and install.sh built
# (docs/plans/v1/wip/17-azure.md section 9; docs/how-to-guides/testing.md#the-l5-lane lists
# what each proves and how to read a failure). Ported from test/e2e-aws/scenarios.sh;
# NOTHING HERE HAS RUN AGAINST A REAL SUBSCRIPTION: every az query, JSON path and
# expected value is to be confirmed by the first live run (plan 17, live verification list).
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 arm64 db described byo negative quota drift scalezero uninstall
#                   (arm64 is the arm64 on-demand node and then the spot node; preflight
#                   always runs first, and ends the run when it fails)
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
# after E2E_LAUNCH_TIMEOUT. A run needs about 16 regional on-demand vCPUs (and in each lane
# family), 4 Spot vCPUs and a few Standard public IPs of quota in the region (the preflight
# checks).
#
# Needs: kubectl, jq, helm, the az CLI logged in as the CI identity, envsubst.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-azure/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT KUBECONFIG \
  AZURE_REGION AZURE_SUBSCRIPTION_ID AZURE_RESOURCE_GROUP
mkdir -p "${E2E_OUT}/state" "${E2E_OUT}/logs"
state="${E2E_OUT}/state"

# ------------------------------------------------------------- state --------

setstate() { printf '%s' "$2" >"${state}/$1"; } # setstate <name> <value>
getstate() { cat "${state}/$1" 2>/dev/null || true; }

# pick_zone: a logical zone of the region for the scenarios that pin one (the proximity
# placement group): the first zone (by name) the database's size is offered in, from the SKU
# row of the subscription; "0" when the size has no zones.
pick_zone() {
  local z
  z="$(getstate zone)"
  if [[ -z "${z}" ]]; then
    z="$(sku_field "${E2E_TYPE_DB#azure.}" '[.locationInfo[]?.zones[]?] | sort | first')"
    z="${z:-0}"
    setstate zone "${z}"
  fi
  E2E_PG_ZONE="${z}"
  export E2E_PG_ZONE
}

# drop_nodeshells: the privileged Pods of node_shell would keep a node busy, and an
# empty node is what consolidation removes.
drop_nodeshells() { kubectl delete pods -l scarpenter-e2e=nodeshell --ignore-not-found --wait=false >/dev/null; }

# settle <app label>: waits for the Pod, records its node, claim, UID and VM.
settle() {
  local app="$1" pod node claim vm
  wait_node "the Pod ${app} running" "${E2E_NODE_TIMEOUT}" "app=${app}" "" pod_running "app=${app}"
  pod="$(kubectl get pod -l "app=${app}" -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  assert_nonempty "the NodeClaim of ${node}" "${claim}"
  vm="$(vm_of_claim "${claim}")"
  assert_nonempty "the VM of ${claim} (resource group/name)" "${vm}"
  assert_eq "the VM's resource group is the lane's" "${AZURE_RESOURCE_GROUP}" "$(vm_rg "${vm}")"
  setstate "${app}.pod" "${pod}"
  setstate "${app}.node" "${node}"
  setstate "${app}.claim" "${claim}"
  setstate "${app}.uid" "$(kubectl get nodeclaim "${claim}" -o jsonpath='{.metadata.uid}')"
  setstate "${app}.vm" "${vm}"
  note "${app}: pod ${pod}, node ${node}, claim ${claim}, ready after $(ready_seconds "${claim}" "${node}")s"
}

# tag_of <json with a tags map> <tag key>
tag_of() { jq -r --arg k "$2" '(.tags // {})[$k] // empty' <<<"$1"; }

# same_id <a> <b>: ARM resource IDs are case insensitive (a group is resourceGroups in one
# answer and resourcegroups in another).
same_id() { [[ "${1,,}" == "${2,,}" ]]; }

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_arm64() {
  probe e2e-arm64 e2e-arm64 "${E2E_TYPE_ARM64}" on-demand
  probe e2e-spot e2e-spot "${E2E_TYPE_SPOT}" spot
}
start_db() {
  pick_zone
  render "${here}/manifests/db.yaml" | kubectl apply -f -
}
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }

# start_byo: the VNet with its subnet and the NSG the scenario creates with the az CLI in
# the lane's resource group (tagged with the cluster, the run's creation time and the
# selector tag; the tags of the VNet are what the NodeClass's subnet selector matches),
# the NodeClasses that use them and the workload.
start_byo() {
  local vnet nsg tags subnet_id nsg_id
  vnet="${E2E_CLUSTER}-byo"
  nsg="${E2E_CLUSTER}-byo"
  tags=("${ctag}=${E2E_CLUSTER}" "scarpenter-e2e-byo=${E2E_CLUSTER}" "${created_tag}=${E2E_CREATED_AT_RFC3339}")
  if [[ -z "$(az network vnet list --resource-group "${AZURE_RESOURCE_GROUP}" \
    --query "[?tags.\"scarpenter-e2e-byo\"=='${E2E_CLUSTER}'].name" --output tsv)" ]]; then
    # 10.240.0.0/16 is the managed network's: a different block here.
    az network vnet create --resource-group "${AZURE_RESOURCE_GROUP}" --name "${vnet}" --location "${AZURE_REGION}" \
      --address-prefixes 10.250.0.0/16 --subnet-name nodes --subnet-prefixes 10.250.0.0/24 --tags "${tags[@]}" >/dev/null
  fi
  if [[ -z "$(az network nsg list --resource-group "${AZURE_RESOURCE_GROUP}" \
    --query "[?tags.\"scarpenter-e2e-byo\"=='${E2E_CLUSTER}'].name" --output tsv)" ]]; then
    az network nsg create --resource-group "${AZURE_RESOURCE_GROUP}" --name "${nsg}" --location "${AZURE_REGION}" \
      --tags "${tags[@]}" >/dev/null
  fi
  subnet_id="$(az network vnet subnet show --resource-group "${AZURE_RESOURCE_GROUP}" --vnet-name "${vnet}" --name nodes --query id --output tsv | tr -d '\r')"
  nsg_id="$(az network nsg show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${nsg}" --query id --output tsv | tr -d '\r')"
  [[ -n "${subnet_id}" && -n "${nsg_id}" ]] || fail "the brought VNet or NSG has no ID"
  setstate byo.subnet "${subnet_id}"
  setstate byo.nsg "${nsg_id}"
  E2E_BYO_SUBNET_ID="${subnet_id}" E2E_BYO_NSG_ID="${nsg_id}" render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  local t
  for t in az kubectl jq helm envsubst; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  assert_eq "the az session's subscription is the lane's" "${AZURE_SUBSCRIPTION_ID}" "$(az account show --query id --output tsv | tr -d '\r')"

  # 1. The resource providers (plan 17 section 2: a MissingSubscriptionRegistration is Fatal)
  # and the lane's resource group.
  local p
  for p in Microsoft.Compute Microsoft.Network; do
    assert_eq "the provider ${p} is registered" Registered "$(az provider show --namespace "${p}" --query registrationState --output tsv | tr -d '\r')"
  done
  assert_eq "the lane's resource group is in the lane's region" "${AZURE_REGION}" \
    "$(az group show --name "${AZURE_RESOURCE_GROUP}" --query location --output tsv | tr -d '\r')"

  # 2. The Flatcar images (plan 17 section 1): the marketplace offer of each architecture has
  # exactly the pinned release, and no terms are pending (SCarpenter never accepts terms).
  local arch urn plan accepted
  for arch in amd64 arm64; do
    urn="kinvolk:flatcar-container-linux-corevm-amd64:${E2E_FLATCAR_CHANNEL}-gen2:${E2E_FLATCAR_VERSION}"
    [[ "${arch}" == amd64 ]] || urn="kinvolk:flatcar-container-linux-corevm:${E2E_FLATCAR_CHANNEL}:${E2E_FLATCAR_VERSION}"
    assert_eq "the marketplace image ${urn} (${arch}) exists" "${E2E_FLATCAR_VERSION}" \
      "$(az vm image show --location "${AZURE_REGION}" --urn "${urn}" --query name --output tsv | tr -d '\r')"
    plan="$(az vm image show --location "${AZURE_REGION}" --urn "${urn}" --query 'plan.name' --output tsv | tr -d '\r')"
    if [[ -n "${plan}" && "${plan}" != None ]]; then
      accepted="$(az vm image terms show --urn "${urn}" --query accepted --output tsv | tr -d '\r')"
      assert_eq "the marketplace terms of ${urn} are accepted (az vm image terms accept --urn ${urn}, by the account's owner)" true "${accepted,,}"
    else
      echo "ok: ${urn} has no plan, so no terms"
    fi
  done

  # 3. The quotas the suite needs (a new subscription's defaults are not enough): both tiers
  # of the on-demand sizes, the Spot vCPUs, and the public IPs.
  local od spot fam
  od="$(limit_of compute "${bucket_cores}")"
  spot="$(limit_of compute "${bucket_spot}")"
  [[ "$(num "${od:-0}")" -ge "${E2E_MIN_VCPU_OD}" ]] || fail "the Total Regional vCPUs quota of ${AZURE_REGION} is '${od}'; the suite needs ${E2E_MIN_VCPU_OD} (raise it, docs/how-to-guides/testing.md#the-l5-lane)"
  [[ "$(num "${spot:-0}")" -ge "${E2E_MIN_VCPU_SPOT}" ]] || fail "the Total Regional Spot vCPUs quota of ${AZURE_REGION} is '${spot}'; the suite needs ${E2E_MIN_VCPU_SPOT}"
  for t in "${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_DB}"; do
    fam="$(sku_family "${t#azure.}")"
    assert_nonempty "the quota family of ${t}" "${fam}"
    [[ "$(num "$(limit_of compute "${fam}")")" -ge "${E2E_MIN_VCPU_FAMILY}" ]] ||
      fail "the ${fam} vCPUs quota of ${AZURE_REGION} is '$(limit_of compute "${fam}")'; the suite needs ${E2E_MIN_VCPU_FAMILY}"
  done
  [[ "$(num "$(limit_of network "${bucket_pip}")")" -ge 8 ]] || fail "the Standard public IPv4 address quota is below 8"

  # 4. The sizes: offered in the region, no restriction for the subscription, Gen2 and
  # SCSI (the driver drops others), and the spot size is LowPriorityCapable, which the
  # B-series are not.
  for t in "${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_SPOT}" "${E2E_TYPE_DB}" ${E2E_TYPES_DESCRIBED//,/ }; do
    assert_nonempty "the size ${t} is offered in ${AZURE_REGION}" "$(sku_field "${t#azure.}" '.name')"
    assert_eq "restrictions of ${t}" 0 "$(sku_field "${t#azure.}" '(.restrictions | length)')"
  done
  assert_eq "the spot size ${E2E_TYPE_SPOT} is LowPriorityCapable" True \
    "$(sku_field "${E2E_TYPE_SPOT#azure.}" '(.capabilities[] | select(.name == "LowPriorityCapable") | .value)')"
  assert_nonempty "the premium data disk's size ${E2E_TYPE_DB} supports Premium IO" \
    "$(sku_field "${E2E_TYPE_DB#azure.}" '(.capabilities[] | select(.name == "PremiumIO" and .value == "True") | .value)')"

  # 5. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc
  for nc in e2e-amd64 e2e-arm64 e2e-drift; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.azure.conditions[?(@.type=="VendorReady")].status}')"
    assert_eq "scnodeclass/${nc} ImagesReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.azure.conditions[?(@.type=="ImagesReady")].status}')"
  done
  pick_zone
  note "zone of the proximity placement group: ${E2E_PG_ZONE}"
}

# Prelude: the baseline of nothing; the workloads of every selected scenario start now.
scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim vm zone j
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  vm="$(vm_name "$(getstate e2e-amd64.vm)")"

  log "node Ready, labels (overview section 5.5)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "region" "${AZURE_REGION}" "$(node_label "${node}" topology.kubernetes.io/region)"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" azure "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${E2E_TYPE_AMD64#azure.}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_nonempty "family (Keeper's, not Azure's quota family)" "$(node_label "${node}" "${label_domain}/family")"
  assert_eq "storage-profile" default "$(node_label "${node}" "${label_domain}/storage-profile")"
  assert_eq "placement-profile" default "$(node_label "${node}" "${label_domain}/placement-profile")"
  assert_eq "nodepool" e2e-amd64 "$(node_label "${node}" karpenter.sh/nodepool)"
  zone="$(node_label "${node}" topology.kubernetes.io/zone)"
  assert_match "the logical zone name" '^[0-3]$' "${zone}"

  log "the VM: name, size, zone, tags, fields of plan 17 section 3 (az vm show)"
  j="$(vm_json "${vm}")"
  assert_eq "the VM's name is <nodeclaim>-<hash8(cluster)>" "${claim}-$(cluster_hash)" "${vm}"
  assert_eq "provisioning state" Succeeded "$(jq -r '.provisioningState' <<<"${j}")"
  assert_eq "power state" "VM running" "$(jq -r '.powerState' <<<"${j}")"
  assert_eq "size" "${E2E_TYPE_AMD64#azure.}" "$(jq -r '.hardwareProfile.vmSize' <<<"${j}")"
  assert_eq "location" "${AZURE_REGION}" "$(jq -r '.location' <<<"${j}")"
  assert_eq "zone (a region without zones has none)" "${zone}" "$(jq -r '.zones[0] // "0"' <<<"${j}")"
  assert_eq "priority (on-demand is Regular)" Regular "$(jq -r '.priority // "Regular"' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" "${ctag}")"
  assert_eq "tag nodeclaim" "${claim}" "$(tag_of "${j}" "${tag_prefix}nodeclaim")"
  assert_eq "tag nodepool" e2e-amd64 "$(tag_of "${j}" "${tag_prefix}nodepool")"
  assert_eq "tag nodeclass" e2e-amd64 "$(tag_of "${j}" "${tag_prefix}nodeclass")"
  assert_eq "tag managed-by" scarpenter "$(tag_of "${j}" "${tag_prefix}managed-by")"
  assert_eq "tag Name" "${claim}" "$(tag_of "${j}" Name)"
  assert_eq "tag ${created_tag} (the NodeClass's spec.tags, RFC 3339)" "${E2E_CREATED_AT_RFC3339}" "$(tag_of "${j}" "${created_tag}")"
  assert_eq "admin user" core "$(jq -r '.osProfile.adminUsername' <<<"${j}")"
  assert_eq "no password logins" true "$(jq -r '.osProfile.linuxConfiguration.disablePasswordAuthentication' <<<"${j}")"
  assert_eq "extensions are not allowed" false "$(jq -r '.osProfile.allowExtensionOperations' <<<"${j}")"
  assert_eq "the image's publisher" kinvolk "$(jq -r '.storageProfile.imageReference.publisher' <<<"${j}")"
  assert_eq "the image's version is the pin" "${E2E_FLATCAR_VERSION}" "$(jq -r '.storageProfile.imageReference.exactVersion // .storageProfile.imageReference.version' <<<"${j}")"
  assert_eq "boot diagnostics (managed storage)" true "$(jq -r '.diagnosticsProfile.bootDiagnostics.enabled' <<<"${j}")"
  assert_nonempty "public IPv4" "$(jq -r '.publicIps // empty' <<<"${j}")"
  assert_nonempty "private IPv4" "$(jq -r '.privateIps // empty' <<<"${j}")"

  log "the OS disk, the NIC and the public IP: deleteOption Delete, the node's settings"
  assert_eq "OS disk name" "${vm}-os" "$(jq -r '.storageProfile.osDisk.name' <<<"${j}")"
  assert_eq "OS disk deleteOption" Delete "$(jq -r '.storageProfile.osDisk.deleteOption' <<<"${j}")"
  assert_eq "OS disk caching" ReadWrite "$(jq -r '.storageProfile.osDisk.caching' <<<"${j}")"
  assert_eq "NIC deleteOption" Delete "$(jq -r '.networkProfile.networkInterfaces[0].deleteOption' <<<"${j}")"
  assert_eq "the NIC is primary" true "$(jq -r '.networkProfile.networkInterfaces[0].primary' <<<"${j}")"
  local disk nic pip
  disk="$(az disk show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${vm}-os" --output json)"
  assert_eq "OS disk SKU" Premium_LRS "$(jq -r '.sku.name' <<<"${disk}")"
  assert_eq "OS disk size" 32 "$(jq -r '.diskSizeGb' <<<"${disk}")"
  same_id "$(jq -r '.managedBy' <<<"${disk}")" "$(jq -r '.id' <<<"${j}")" || fail "the OS disk is not attached to the VM"
  nic="$(az network nic show --ids "$(jq -r '.networkProfile.networkInterfaces[0].id' <<<"${j}")" --output json)"
  assert_eq "tag cluster on the NIC" "${E2E_CLUSTER}" "$(tag_of "${nic}" "${ctag}")"
  assert_nonempty "the NIC's network security group" "$(jq -r '.networkSecurityGroup.id // empty' <<<"${nic}")"
  assert_nonempty "the NIC joins an application security group (isolation)" "$(jq -r '.ipConfigurations[0].applicationSecurityGroups[0].id // empty' <<<"${nic}")"
  pip="$(az network public-ip show --ids "$(jq -r '.ipConfigurations[0].publicIPAddress.id' <<<"${nic}")" --output json)"
  assert_eq "public IP SKU" Standard "$(jq -r '.sku.name' <<<"${pip}")"
  assert_eq "public IP allocation" Static "$(jq -r '.publicIPAllocationMethod' <<<"${pip}")"
  assert_eq "public IP deleteOption" Delete "$(jq -r '.deleteOption' <<<"${pip}")"
  assert_eq "tag cluster on the public IP" "${E2E_CLUSTER}" "$(tag_of "${pip}" "${ctag}")"

  log "the claim: price, bootstrap token, serving certificate"
  local price
  price="$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price" '.metadata.annotations[$k] // empty')"
  assert_nonempty "price annotation" "${price}"
  awk -v p="${price}" 'BEGIN { exit !(p + 0 > 0) }' || fail "the price ${price} is not positive"
  wait_until "the bootstrap token Secret gone" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim=${claim}" -o name
  wait_until "the serving CSR approved and issued" 120 csr_approved "${node}"

  log "kubectl logs and exec (the API server reaches the kubelet over the tailnet)"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_match "the log of the probe" scarpenter-e2e-up "$(kubectl logs "${pod}")"
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"

  log "on the node: the unit-level checks of L4 scenarios Q09 and Q10, on a real node"
  node_unit_checks "${node}"

  log "cluster DNS and the instance metadata service"
  local dns
  dns="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  assert_match "the kubelet's clusterDNS is the kube-dns Service" "${dns//./\\.}" "$(node_shell "${node}" "grep -A3 '^clusterDNS' /var/lib/kubelet/config.yaml" | tr '\n' ' ')"
  wait_until "a Pod resolves a cluster Service" 120 kubectl exec "${pod}" -- nslookup kubernetes.default.svc.cluster.local
  assert_match "IMDS (Azure's, header Metadata: true) from the node says its size" "\"vmSize\":\"${E2E_TYPE_AMD64#azure.}\"" \
    "$(node_shell "${node}" "curl -s --noproxy '*' -H Metadata:true 'http://169.254.169.254/metadata/instance/compute?api-version=2021-02-01'" || true)"
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

# node_unit_checks <node>: what L4 scenarios Q09 and Q10 check inside a VM, on an Azure
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

# arm64: the arm64 on-demand node, then the spot node (a small D size: B-series have no Spot).
scn_arm64() {
  start_arm64
  settle e2e-arm64
  local pod node claim vm j
  pod="$(getstate e2e-arm64.pod)"
  node="$(getstate e2e-arm64.node)"
  claim="$(getstate e2e-arm64.claim)"
  vm="$(vm_name "$(getstate e2e-arm64.vm)")"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_ARM64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "arch" arm64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  j="$(vm_json "${vm}")"
  assert_eq "size" "${E2E_TYPE_ARM64#azure.}" "$(jq -r '.hardwareProfile.vmSize' <<<"${j}")"
  assert_eq "priority (on-demand is Regular)" Regular "$(jq -r '.priority // "Regular"' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" "${ctag}")"
  assert_eq "the image's offer is the arm64 one" flatcar-container-linux-corevm "$(jq -r '.storageProfile.imageReference.offer' <<<"${j}")"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  assert_eq "tailscaled, the join unit and the kubelet are active (arm64 sysext)" "active,active,active" \
    "$(node_shell "${node}" 'systemctl is-active tailscaled scarpenter-tailscale-up.service kubelet' | paste -sd, -)"
  note "arm64 on-demand claim ${claim}"

  log "spot"
  settle e2e-spot
  pod="$(getstate e2e-spot.pod)"
  node="$(getstate e2e-spot.node)"
  claim="$(getstate e2e-spot.claim)"
  vm="$(vm_name "$(getstate e2e-spot.vm)")"
  assert_eq "the spot node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_SPOT}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "capacity-type" spot "$(node_label "${node}" karpenter.sh/capacity-type)"
  j="$(vm_json "${vm}")"
  assert_eq "priority" Spot "$(jq -r '.priority' <<<"${j}")"
  assert_eq "eviction policy (no deallocated VMs left billing their disks)" Delete "$(jq -r '.evictionPolicy' <<<"${j}")"
  assert_eq "maxPrice (never evicted for price)" -1 "$(jq -r '.billingProfile.maxPrice' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" "${ctag}")"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  drop_nodeshells
  note "spot claim ${claim}, size ${E2E_TYPE_SPOT} (to confirm from the canary)"
}

# db: the database and its client in one proximity placement group; the database has a
# Premium data disk at LUN 0. NEEDS THE DEVICE-PATH SPIKE (plan 17 live list item 9): that
# Flatcar's udev gives /dev/disk/azure/scsi1/lun0 for a data disk, and that the first-boot
# unit finds the disk there. Until then this scenario is expected to fail at the device.
scn_db() {
  start_db
  wait_node "the database and the client running" $((E2E_NODE_TIMEOUT + 300)) "scarpenter-e2e=true,app in (e2e-db,e2e-client)" "" \
    pod_running "scarpenter-e2e=true,app in (e2e-db,e2e-client)"
  local dbpod clpod dbnode clnode dbvm clvm dbclaim ppg ppgid dbj clj
  dbpod="$(kubectl get pod -l app=e2e-db -o jsonpath='{.items[0].metadata.name}')"
  clpod="$(kubectl get pod -l app=e2e-client -o jsonpath='{.items[0].metadata.name}')"
  dbnode="$(pod_node "${dbpod}")"
  clnode="$(pod_node "${clpod}")"
  [[ "${dbnode}" != "${clnode}" ]] || fail "the database and the client share the node ${dbnode}"
  dbclaim="$(claim_of_node "${dbnode}")"
  dbvm="$(vm_name "$(vm_of_claim "${dbclaim}")")"
  clvm="$(vm_name "$(vm_of_claim "$(claim_of_node "${clnode}")")")"
  setstate db.vm "${dbvm}"
  setstate client.vm "${clvm}"
  dbj="$(vm_json "${dbvm}")"
  clj="$(vm_json "${clvm}")"

  log "one proximity placement group, named and tagged by SCarpenter"
  ppg="$(az ppg list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?tags.\"${ctag}\"=='${E2E_CLUSTER}']" --output json)"
  assert_eq "proximity placement groups of the cluster" 1 "$(jq 'length' <<<"${ppg}")"
  ppgid="$(jq -r '.[0].id' <<<"${ppg}")"
  # scarpenter-<cluster>-<kind>-<region> (and a hash when shortened by the engine).
  assert_match "the group's name" "^scarpenter-" "$(jq -r '.[0].name' <<<"${ppg}")"
  [[ "$(jq -r '.[0].name | length' <<<"${ppg}")" -le 80 ]] || fail "the group's name is longer than Azure's 80 characters"
  assert_eq "tag managed-by" scarpenter "$(tag_of "$(jq '.[0]' <<<"${ppg}")" "${tag_prefix}managed-by")"
  assert_eq "tag resource-kind" proximityPlacementGroups "$(tag_of "$(jq '.[0]' <<<"${ppg}")" "${tag_prefix}resource-kind")"
  same_id "$(jq -r '.proximityPlacementGroup.id' <<<"${dbj}")" "${ppgid}" || fail "the database is not in the group ${ppgid}"
  same_id "$(jq -r '.proximityPlacementGroup.id' <<<"${clj}")" "${ppgid}" || fail "the client is not in the group ${ppgid}"
  assert_eq "one zone" "${E2E_PG_ZONE}" "$(jq -r '.zones[0] // "0"' <<<"${dbj}")"
  assert_eq "one zone (client)" "${E2E_PG_ZONE}" "$(jq -r '.zones[0] // "0"' <<<"${clj}")"

  log "node labels of the applied profiles"
  assert_eq "database storage-profile" db-premium "$(node_label "${dbnode}" "${label_domain}/storage-profile")"
  assert_eq "database placement-profile" colocated "$(node_label "${dbnode}" "${label_domain}/placement-profile")"
  assert_eq "client storage-profile" default "$(node_label "${clnode}" "${label_domain}/storage-profile")"
  assert_eq "client placement-profile" colocated "$(node_label "${clnode}" "${label_domain}/placement-profile")"
  assert_eq "database run key" "pg-${E2E_RUN_ID}" "$(node_label "${dbnode}" "${label_domain}/placement-group")"
  assert_eq "client run key" "pg-${E2E_RUN_ID}" "$(node_label "${clnode}" "${label_domain}/placement-group")"
  assert_eq "profile-hashes annotation names the storage profile" true \
    "$(kubectl get nodeclaim "${dbclaim}" -o json | jq -r --arg k "${label_domain}/profile-hashes" '.metadata.annotations[$k] | fromjson | has("storage/db-premium")')"

  log "the Premium data disk: attached at LUN 0, sized, mounted at the profile's path"
  assert_eq "the data disk's name" "${dbvm}-pgdata" "$(jq -r '.storageProfile.dataDisks[0].name' <<<"${dbj}")"
  assert_eq "the data disk's LUN" 0 "$(jq -r '.storageProfile.dataDisks[0].lun' <<<"${dbj}")"
  assert_eq "data disk deleteOption" Delete "$(jq -r '.storageProfile.dataDisks[0].deleteOption' <<<"${dbj}")"
  local disk src dev
  disk="$(az disk show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${dbvm}-pgdata" --output json)"
  assert_eq "data disk SKU" Premium_LRS "$(jq -r '.sku.name' <<<"${disk}")"
  assert_eq "data disk size" 4 "$(jq -r '.diskSizeGb' <<<"${disk}")"
  same_id "$(jq -r '.managedBy' <<<"${disk}")" "$(jq -r '.id' <<<"${dbj}")" || fail "the data disk is not attached to the database's VM"
  # The device path is the spike: the udev rules of Flatcar's Azure OEM give the by-path name.
  dev="$(node_shell "${dbnode}" 'readlink -f /dev/disk/azure/scsi1/lun0' || true)"
  assert_nonempty "the device /dev/disk/azure/scsi1/lun0 exists (needs the device-path spike)" "${dev}"
  src="$(node_shell "${dbnode}" 'findmnt -no SOURCE /var/lib/pgdata')"
  assert_nonempty "the device mounted at /var/lib/pgdata" "${src}"
  assert_eq "the mounted device is the one at LUN 0" "${dev}" "$(node_shell "${dbnode}" "readlink -f ${src}")"
  assert_eq "the filesystem at /var/lib/pgdata" xfs "$(node_shell "${dbnode}" 'findmnt -no FSTYPE /var/lib/pgdata')"
  assert_eq "the Pod's file is on the mounted disk" hello-from-the-volume "$(node_shell "${dbnode}" 'cat /var/lib/pgdata/hello')"

  log "isolation: the managed NSG denies the VNet's other nodes and allows the NodeClass's group"
  assert_eq "a managed NSG with DenyVnetInbound at 4000 and an allow rule from an application security group" true \
    "$(az network nsg list --resource-group "${AZURE_RESOURCE_GROUP}" \
      --query "[?tags.\"${ctag}\"=='${E2E_CLUSTER}' && tags.\"${tag_prefix}managed-by\"=='scarpenter']" --output json |
      jq 'any(.[]; any(.securityRules[]?; .priority == 4000 and .access == "Deny") and
        any(.securityRules[]?; .access == "Allow" and ((.sourceApplicationSecurityGroups // []) | length) > 0))')"

  log "delete both workloads: the VMs, their disks and the group go"
  drop_nodeshells
  kubectl delete statefulset e2e-db --wait=false
  kubectl delete deployment e2e-client --wait=false
  wait_until "the VMs deleted" 420 is_empty vms_present "${dbvm}" "${clvm}"
  wait_until "no NIC, public IP or disk of the VMs left" 180 is_empty vm_leftovers_of "${dbvm}" "${clvm}"
  # perUse: two minutes of grace for launches in flight, then the next resource check
  # and garbage collection (30 s and 1 m here).
  wait_until "the proximity placement group deleted" 420 is_value 0 \
    az ppg list --resource-group "${AZURE_RESOURCE_GROUP}" --query "length([?tags.\"${ctag}\"=='${E2E_CLUSTER}'])" --output tsv
}

# vm_leftovers_of <vm>...: the leftovers of every VM, one per line.
vm_leftovers_of() {
  local v out
  for v in "$@"; do
    out="$(vm_leftovers "${v}")" || return 1
    [[ -z "${out}" ]] || awk -v v="${v}" '{ print v ": " $0 }' <<<"${out}"
  done
  return 0
}

scn_described() {
  start_described
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-described "" pod_running app=e2e-described
  local pod node claim vm t vcpus mem pct
  pod="$(kubectl get pod -l app=e2e-described -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  vm="$(vm_name "$(vm_of_claim "${claim}")")"
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
  assert_eq "the VM is of the chosen size" "${t#azure.}" "$(jq -r '.hardwareProfile.vmSize' <<<"$(vm_json "${vm}")")"
  # The same fact from Azure's SKU table, independent of the catalog.
  assert_eq "Azure's vCPUs of the size" "${vcpus}" "$(vcpus_of "${t#azure.}")"
}

# byo: a VNet with a subnet and an NSG created by this scenario in the lane's group are used
# by ID (e2e-use) and by tag (e2e-use-tags, readiness only); SCarpenter never deletes them.
scn_byo() {
  start_byo
  local subnet nsg
  subnet="$(getstate byo.subnet)"
  nsg="$(getstate byo.nsg)"
  wait_until "ResourcesReady on the NodeClass brought by ID" 180 is_value True \
    kubectl get scnodeclass e2e-use -o jsonpath='{.status.vendors.azure.conditions[?(@.type=="ResourcesReady")].status}'
  wait_until "ResourcesReady on the NodeClass brought by tag" 180 is_value True \
    kubectl get scnodeclass e2e-use-tags -o jsonpath='{.status.vendors.azure.conditions[?(@.type=="ResourcesReady")].status}'
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-use "" pod_running app=e2e-use
  local pod node vm j nic
  pod="$(kubectl get pod -l app=e2e-use -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  vm="$(vm_name "$(vm_of_claim "$(claim_of_node "${node}")")")"
  j="$(vm_json "${vm}")"
  assert_eq "the VM is in the lane's resource group" "${AZURE_RESOURCE_GROUP}" "$(jq -r '.resourceGroup' <<<"${j}")"
  nic="$(az network nic show --ids "$(jq -r '.networkProfile.networkInterfaces[0].id' <<<"${j}")" --output json)"
  same_id "$(jq -r '.ipConfigurations[0].subnet.id' <<<"${nic}")" "${subnet}" || fail "the NIC is not in the brought subnet ${subnet}"
  same_id "$(jq -r '.networkSecurityGroup.id' <<<"${nic}")" "${nsg}" || fail "the NIC does not use the brought NSG ${nsg}"
  echo "ok: the NIC is in the brought subnet and uses the brought NSG"

  log "SCarpenter must not touch what it did not create"
  drop_nodeshells
  kubectl delete deployment e2e-use --wait=false
  wait_until "the VM deleted" 300 is_empty vms_present "${vm}"
  kubectl delete nodepool e2e-use --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-use e2e-use-tags --wait=true --timeout=120s
  # Past a garbage collection (1 m), a resource check (30 s) and the status controller:
  # a `use` reference is never modified or deleted.
  sleep 120
  assert_eq "the resource group still exists" "${AZURE_RESOURCE_GROUP}" "$(az group show --name "${AZURE_RESOURCE_GROUP}" --query name --output tsv | tr -d '\r')"
  assert_eq "the subnet still exists" "${subnet,,}" "$(az network vnet subnet show --ids "${subnet}" --query id --output tsv | tr -d '\r' | tr '[:upper:]' '[:lower:]')"
  assert_eq "the NSG still exists" "${nsg,,}" "$(az network nsg show --ids "${nsg}" --query id --output tsv | tr -d '\r' | tr '[:upper:]' '[:lower:]')"
  note "the brought VNet and NSG ${E2E_CLUSTER}-byo are deleted by the teardown"
}

# negative: what must never launch. A NodeClass for another subscription is refused; Spot on
# a B-series size (no Spot there) and a size outside the Azure Policy's allow-list get no VM.
scn_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
  wait_until "a subscriptionID of another subscription is refused" 180 is_value False \
    kubectl get scnodeclass e2e-wrongsub -o jsonpath='{.status.vendors.azure.conditions[?(@.type=="VendorReady")].status}'
  assert_eq "the reason" ValidationFailed \
    "$(kubectl get scnodeclass e2e-wrongsub -o jsonpath='{.status.vendors.azure.conditions[?(@.type=="VendorReady")].reason}')"
  assert_match "the message names the subscription" "[Ss]ubscription" \
    "$(kubectl get scnodeclass e2e-wrongsub -o jsonpath='{.status.vendors.azure.conditions[?(@.type=="VendorReady")].message}')"

  render "${here}/manifests/nodepool-negative.yaml" | kubectl apply -f -
  probe e2e-neg-spot e2e-neg-spot "${E2E_TYPE_AMD64}" spot
  probe e2e-neg-denied e2e-neg-denied "${E2E_TYPE_DENIED}" on-demand
  log "watching for ${E2E_NEGATIVE_WAIT}s: no VM of these sizes may appear"
  local deadline=$((SECONDS + E2E_NEGATIVE_WAIT)) n
  while [[ "${SECONDS}" -lt "${deadline}" ]]; do
    n="$(negative_vms)"
    [[ -z "${n}" ]] || fail "a VM of a size that must not launch exists: ${n}"
    sleep 10
  done
  assert_eq "Spot on ${E2E_TYPE_AMD64}: the VM does not exist" "" "$(negative_vms)"
  # The Spot pool on a B-series size has no offering: no NodeClaim is launched for it.
  assert_eq "NodeClaims launched for Spot on a B-series" "0 $(claims_total e2e-neg-spot)" "$(claims_launched e2e-neg-spot "")"
  # The denied size: either the catalog does not offer it (no claim), or the policy refused the
  # VM (a claim that never launches). Both are the guard working; which one is a note.
  assert_eq "NodeClaims launched for ${E2E_TYPE_DENIED}" "0 $(claims_total e2e-neg-denied)" "$(claims_launched e2e-neg-denied "")"
  note "negative: ${E2E_TYPE_DENIED} got $(claims_total e2e-neg-denied) NodeClaim(s), none launched (0 means the catalog does not offer it; more means the size policy or Azure refused the VM)"
  assert_eq "the Pods are still Pending" "2" "$(kubectl get pods -l 'app in (e2e-neg-spot,e2e-neg-denied)' -o json | jq '[.items[] | select(.status.phase == "Pending")] | length')"
  kubectl delete deployment e2e-neg-spot e2e-neg-denied --wait=false
  kubectl delete nodepool e2e-neg-spot e2e-neg-denied --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-wrongsub --wait=true --timeout=120s
}

# negative_vms: the cluster's VMs of the sizes the negative pools ask for.
negative_vms() {
  az vm list --resource-group "${AZURE_RESOURCE_GROUP}" \
    --query "[?tags.\"${ctag}\"=='${E2E_CLUSTER}' && (hardwareProfile.vmSize=='${E2E_TYPE_DENIED#azure.}' || (priority=='Spot' && hardwareProfile.vmSize=='${E2E_TYPE_AMD64#azure.}'))].name" \
    --output tsv | tr -d '\r' | paste -sd, -
}

# claims_total <pool>: how many NodeClaims (not being deleted) the pool has.
claims_total() {
  claims_launched "$1" "" | awk '{ print $2 }'
}

# quota (plan 17 section 5): what the controller reports equals what Azure's Usage APIs say
# (the SDK's source), both tiers and spot. Run with the nodes of the other scenarios up and
# no launch in flight.
scn_quota() {
  settle_fixtures
  local m b fam t
  m="$(mktemp)"
  log "the limits of the buckets equal the Usage APIs: regional, Spot, public IPs and the lane's families"
  local timeout=240
  for t in "${E2E_TYPE_AMD64}" "${E2E_TYPE_ARM64}" "${E2E_TYPE_DB}"; do
    fam="$(sku_family "${t#azure.}")"
    wait_until "scarpenter_quota_limit ${fam} = Azure's" "${timeout}" quota_limit_is "${m}" compute "${fam}"
    timeout=90 # the first one waited for the controller's first limits refresh
  done
  wait_until "scarpenter_quota_limit ${bucket_cores} = Azure's" "${timeout}" quota_limit_is "${m}" compute "${bucket_cores}"
  wait_until "scarpenter_quota_limit ${bucket_spot} = Azure's" "${timeout}" quota_limit_is "${m}" compute "${bucket_spot}"
  wait_until "scarpenter_quota_limit ${bucket_pip} = Azure's" "${timeout}" quota_limit_is "${m}" network "${bucket_pip}"

  log "usage: what the controller reports is what the Usage APIs say, both tiers and Spot"
  for b in "${bucket_cores}" "${bucket_spot}" "$(sku_family "${E2E_TYPE_AMD64#azure.}")" "$(sku_family "${E2E_TYPE_SPOT#azure.}")"; do
    wait_until "scarpenter_quota_used ${b} = Azure's currentValue" 300 quota_used_is "${m}" compute "${b}"
  done
  wait_until "scarpenter_quota_used ${bucket_pip} = Azure's currentValue" 300 quota_used_is "${m}" network "${bucket_pip}"
  local od spot fam_used
  od="$(metric_value "${m}" scarpenter_quota_used "${AZURE_REGION}" "${bucket_cores}")"
  spot="$(metric_value "${m}" scarpenter_quota_used "${AZURE_REGION}" "${bucket_spot}")"
  fam_used="$(metric_value "${m}" scarpenter_quota_used "${AZURE_REGION}" "$(sku_family "${E2E_TYPE_AMD64#azure.}")")"
  [[ "$(num "${od}")" -ge "$(vcpus_of "${E2E_TYPE_AMD64#azure.}")" ]] || fail "the regional usage ${od} does not include the launched VM"
  [[ "$(num "${fam_used}")" -ge "$(vcpus_of "${E2E_TYPE_AMD64#azure.}")" ]] || fail "the family usage ${fam_used} does not include the launched VM (both tiers consume)"
  [[ "$(num "${spot}")" -ge "$(vcpus_of "${E2E_TYPE_SPOT#azure.}")" ]] || fail "the Spot usage ${spot} does not include the launched Spot VM"
  wait_until "the in-flight reservations are gone (regional)" 240 quota_reserved_zero "${m}" "${bucket_cores}"
  wait_until "the in-flight reservations are gone (Spot)" 240 quota_reserved_zero "${m}" "${bucket_spot}"

  log "the launched sizes are offered, unrestricted, in their zones"
  local zone
  for t in e2e-amd64:"${E2E_TYPE_AMD64}" e2e-spot:"${E2E_TYPE_SPOT}"; do
    zone="$(node_label "$(getstate "${t%%:*}.node")" topology.kubernetes.io/zone)"
    assert_eq "${t#*:} has no restriction for the subscription" 0 "$(sku_field "${t#*:azure.}" '(.restrictions | length)')"
    if [[ "${zone}" != 0 ]]; then
      assert_eq "${t#*:} is offered in zone ${zone}" 1 "$(sku_field "${t#*:azure.}" "[.locationInfo[]?.zones[]? | select(. == \"${zone}\")] | length")"
    fi
  done
  rm -f "${m}"
}

# quota_limit_is <metrics file> compute|network <usage name>: the controller's limit is the
# Usage API's.
quota_limit_is() {
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_limit "${AZURE_REGION}" "$3")")" == "$(num "$(limit_of "$2" "$3")")" ]]
}

# quota_used_is <metrics file> compute|network <usage name>: the controller's usage is the
# Usage API's currentValue (the whole subscription's: nothing to count by hand).
quota_used_is() {
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_used "${AZURE_REGION}" "$3")")" == "$(num "$(usage_of "$2" "$3")")" ]]
}

quota_reserved_zero() { # quota_reserved_zero <metrics file> <bucket>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_reserved "${AZURE_REGION}" "$2")")" == 0 ]]
}

# settle_fixtures: the amd64 and spot nodes of the first scenarios (started if the
# scenario runs alone).
settle_fixtures() {
  [[ -n "$(getstate e2e-amd64.vm)" ]] || {
    start_amd64
    settle e2e-amd64
  }
  [[ -n "$(getstate e2e-spot.vm)" ]] || {
    probe e2e-spot e2e-spot "${E2E_TYPE_SPOT}" spot
    settle e2e-spot
  }
}

# drift (plan 10 scenario 4) and the opportunistic controller restart of plan 07's review: a change to
# a profile no node uses drifts nothing; a change to the vendor block (the OS disk's size)
# replaces the node, and the controller is killed while the replacement's Create is in flight.
scn_drift() {
  start_drift
  settle e2e-drift
  local old_claim old_node old_vm old_pod
  old_claim="$(getstate e2e-drift.claim)"
  old_node="$(getstate e2e-drift.node)"
  old_vm="$(vm_name "$(getstate e2e-drift.vm)")"
  old_pod="$(getstate e2e-drift.pod)"

  log "a profile no running node uses changes: nothing drifts"
  kubectl patch scnodeclass e2e-drift --type merge -p \
    '{"spec":{"profiles":{"storage":{"scratch":{"azure":{"data":[{"name":"scratch","storageAccountType":"StandardSSD_LRS","diskSizeGB":2,"filesystem":"ext4","mountPath":"/var/lib/scratch"}]}}}}}}'
  wait_until "the NodeClass reconciled the change" 120 nodeclass_observed e2e-drift
  local i
  for i in 1 2 3 4 5 6; do
    sleep 10
    assert_eq "the claim is not drifted (${i}/6)" "" \
      "$(kubectl get nodeclaim "${old_claim}" -o jsonpath='{.status.conditions[?(@.type=="Drifted")].status}' | grep -x True || true)"
  done
  assert_eq "still the same claim" "${old_claim}" "$(claim_of_node "${old_node}")"

  log "the OS disk's size changes (spec.vendors.azure.storage.root.diskSizeGB): the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p '{"spec":{"vendors":{"azure":{"storage":{"root":{"diskSizeGB":40}}}}}}'
  restart_during_create e2e-drift "${old_claim}"
  wait_node "the Pod on another node" "${E2E_NODE_TIMEOUT}" app=e2e-drift "${old_claim}" pod_moved_from app=e2e-drift "${old_node}"
  release_do_not_disrupt
  local pod node claim vm
  pod="$(kubectl get pod -l app=e2e-drift -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  vm="$(vm_name "$(vm_of_claim "${claim}")")"
  [[ "${vm}" != "${old_vm}" ]] || fail "the replacement runs on the old VM"
  assert_eq "the replacement's OS disk (the new size)" 40 \
    "$(az disk show --resource-group "${AZURE_RESOURCE_GROUP}" --name "${vm}-os" --query diskSizeGb --output tsv | tr -d '\r')"
  wait_until "the old VM deleted" 300 is_empty vms_present "${old_vm}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  assert_no_orphans
  # Azure keeps no deleted VMs to list, so the "launched twice" check of AWS is the live
  # count: the claim owns at most one VM now (a replay that did not find the first launch
  # would have left two).
  local repl ever
  repl="$(getstate drift.replacement)"
  ever="$(vms_of_claim "${repl}" | wc -w | tr -d ' ')"
  [[ "${ever}" -le 1 ]] || fail "the claim ${repl} owns ${ever} VMs (a second create after the restart)"
  setstate e2e-drift.node "${node}"
  setstate e2e-drift.claim "${claim}"
  setstate e2e-drift.vm "$(vm_of_claim "${claim}")"
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
# (the VM's name is deterministic, the controller replays it by a GET): after the restart
# the claim must not own two VMs, and no VM may be orphaned.
#
# The window (the VM exists, Create has not returned) is wider on Azure than on AWS (the
# driver waits up to 4 minutes for the long-running create) but still not forced (the lane
# builds the production image; no test-only driver flag exists), so the replay assertions
# are OPPORTUNISTIC: when the attempt misses the window the scenario notes it and never
# fails for it. What always holds and is always asserted: no VM is left without a
# NodeClaim (the caller), and a claim that is still there owns one VM.
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
  before="$(vms_of_claim "${new}")"
  if [[ "${window}" == yes ]]; then
    note "restart: the controller was killed while ${new} was not launched; VMs of the claim at that moment: '${before}'"
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
  after="$(vms_of_claim "${new}")"
  [[ "$(wc -w <<<"${after}" | tr -d ' ')" -le 1 ]] || fail "the claim ${new} owns several VMs after the restart: ${after}"
  assert_eq "the claim's VM is the one in its annotation" "$(vm_name "$(vm_of_claim "${new}")")" "${after}"
  if [[ "${window}" == yes && -n "${before}" ]]; then
    # The one assertion that can fail on the window: a second VM for the claim.
    assert_eq "the replay of Create found the first launch's VM (no second VM)" "${before}" "${after}"
    note "restart: the replay of Create was exercised (VM ${after})"
  elif [[ "${window}" == yes ]]; then
    note "restart (opportunistic): the VM did not exist yet when the controller was killed; Create ran from the start"
  fi
}

# claim_settled <claim>: launched, or no longer there.
claim_settled() {
  local out
  out="$(kubectl get nodeclaim "$1" -o jsonpath='{.status.conditions[?(@.type=="Launched")].status}' --ignore-not-found)" || return 1
  [[ "${out}" == True ]] || [[ -z "$(kubectl get nodeclaim "$1" -o name --ignore-not-found)" ]]
}

# vms_of_claim <claim>: the VMs tagged with the claim, space separated.
vms_of_claim() {
  az vm list --resource-group "${AZURE_RESOURCE_GROUP}" --query "[?tags.\"${tag_prefix}nodeclaim\"=='$1'].name" --output tsv | tr -d '\r' | xargs
}

# assert_no_orphans: every VM of the cluster belongs to a NodeClaim that exists.
assert_no_orphans() {
  wait_until "no orphaned VM" 240 no_orphans
}

no_orphans() {
  local claims vms v
  claims="$(kjson nodeclaims | jq -r --arg k "${label_domain}/instance-ref" '.items[] | .metadata.annotations[$k] // empty' | sed 's#.*/##')"
  vms="$(cluster_vms)"
  for v in ${vms}; do
    grep -qx "${v}" <<<"${claims}" || {
      echo "VM ${v} has no NodeClaim"
      return 1
    }
  done
}

# scalezero (plan 10 scenario 3, the roadmap's exit criterion): deleting the workloads
# deletes every VM within E2E_SCALE_DOWN_DEADLINE seconds of the Pods' deletion
# (WhenEmpty, 0 s); the nodes, NodeClaims and bootstrap tokens go too; and NO NIC, public IP
# or disk of a VM remains (the deleteOption cascade); the quota usage falls with the VMs.
scn_scalezero() {
  local uid vm t0 elapsed
  uid="$(getstate e2e-amd64.uid)"
  vm="$(getstate e2e-amd64.vm)"
  # Every VM the run knows of, before it goes: what the leftovers are looked up by.
  local known
  known="$( (cluster_vms all; for f in "${state}"/*.vm; do [[ -f "${f}" ]] && vm_name "$(cat "${f}")"; done) | sort -u)"
  drop_nodeshells
  release_do_not_disrupt
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every VM deleting or gone" "${E2E_SCALE_DOWN_DEADLINE}" is_empty cluster_vms
  note "scale to zero: every VM was deleting (or gone) $(($(now) - t0))s after the Pods' deletion"
  # The deadline counts from the Pods' deletion for both.
  wait_until "every VM of the cluster gone" "$((E2E_SCALE_DOWN_DEADLINE > $(now) - t0 ? E2E_SCALE_DOWN_DEADLINE - ($(now) - t0) : 1))" is_empty cluster_vms all
  elapsed=$(($(now) - t0))
  note "scale to zero: every VM was gone ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s)"
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name

  log "no NIC, public IP or disk of a VM remains (the deleteOption cascade)"
  # shellcheck disable=SC2086  # the names are one word each
  wait_until "no NIC, public IP or disk of the run's VMs" 180 is_empty vm_leftovers_of ${known}
  wait_until "no disk of the cluster" 60 is_empty cluster_disks
  wait_until "no NIC of the cluster" 60 is_empty cluster_resources_of Microsoft.Network/networkInterfaces
  wait_until "no public IP of the cluster" 60 is_empty cluster_resources_of Microsoft.Network/publicIPAddresses
  if [[ -n "${uid}" ]]; then
    # The driver finds the VM of a replayed Create by its deterministic name; a deleted VM
    # is a 404, which is "not found" (and the claim is gone): nothing to look up here.
    note "scale to zero: the VM of the first scenario was ${vm}"
  fi

  log "the quota usage follows the VMs down"
  local m
  m="$(mktemp)"
  wait_until "the regional usage equals Azure's" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" compute "${bucket_cores}"
  wait_until "the Spot usage equals Azure's" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" compute "${bucket_spot}"
  wait_until "the public IP usage equals Azure's" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" network "${bucket_pip}"
  wait_until "no reservation is left" 120 quota_reserved_zero "${m}" "${bucket_cores}"
  rm -f "${m}"
}

# cluster_resources_of <resource type>: the IDs of the cluster's resources of a type.
cluster_resources_of() {
  az resource list --resource-group "${AZURE_RESOURCE_GROUP}" --tag "${ctag}=${E2E_CLUSTER}" --resource-type "$1" --query '[].id' --output tsv | tr -d '\r'
}

# unbrought_resources: what is tagged with the cluster and is not the scenario's own brought
# VNet and NSG (those carry scarpenter-e2e-byo and are deleted by the teardown).
unbrought_resources() {
  az resource list --resource-group "${AZURE_RESOURCE_GROUP}" --tag "${ctag}=${E2E_CLUSTER}" \
    --query "[?tags.\"scarpenter-e2e-byo\"==null].id" --output tsv | tr -d '\r'
}

# uninstall: the procedure of docs/how-to-guides/install.md#uninstall, on the live subscription:
# when it is done NOTHING TAGGED with the cluster remains but what the scenarios brought.
scn_uninstall() {
  log "1. SCarpenter's NodePools (selected by nodeClassRef.group), then wait for the NodeClaims"
  kjson nodepools | jq -r '.items[] | select(.spec.template.spec.nodeClassRef.group == "scarpenter.sparecores.com") | .metadata.name' |
    xargs -r kubectl delete nodepool --wait=true --timeout=300s
  wait_until "no NodeClaim of SCarpenter left" 300 is_value 0 scarpenter_claims
  log "2. the SCNodeClasses; the managed resources go by their lifecycle"
  kubectl delete scnodeclass --all --wait=true --timeout=300s
  wait_until "no VM of the cluster" 300 is_empty cluster_vms all
  # With the NodeClasses gone the managed VNets and NSGs fall under the default idle
  # lifetime (managedResources.defaultTTL, 5 minutes in the lane's values; 24 hours by default).
  wait_until "no managed resource of SCarpenter left" 900 is_empty managed_resources
  wait_until "nothing tagged with the cluster remains (but the brought VNet and NSG)" 120 is_empty unbrought_resources
  wait_until "no disk of the cluster left" 60 is_empty cluster_disks
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

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 arm64 db described byo negative quota drift scalezero uninstall)
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
