#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5: the scenarios, run against the cluster that bootstrap.sh and install.sh built
# (docs/plans/v1/wip/18-ovh.md, "L5 lane"; docs/how-to-guides/testing.md#the-l5-lane lists
# what each proves and how to read a failure).
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 db described byo negative quota drift poweroff scalezero uninstall
#                   (preflight always runs first, and ends the run when it fails)
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
# after E2E_LAUNCH_TIMEOUT. A run needs about E2E_MIN_VCPU_OD cores and E2E_MIN_INSTANCES
# instances of the project's quota in the region (preflight checks). There is no arm64, no
# spot and no local-storage scenario: OVH has none of them.
#
# Needs: kubectl, jq, helm, the openstack CLI with the lane's credentials, envsubst.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-ovh/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT OS_REGION_NAME KUBECONFIG
mkdir -p "${E2E_OUT}/state" "${E2E_OUT}/logs"
state="${E2E_OUT}/state"

# ------------------------------------------------------------- state --------

setstate() { printf '%s' "$2" >"${state}/$1"; } # setstate <name> <value>
getstate() { cat "${state}/$1" 2>/dev/null || true; }

# drop_nodeshells: the privileged Pods of node_shell would keep a node busy, and an
# empty node is what consolidation removes.
drop_nodeshells() { kubectl delete pods -l scarpenter-e2e=nodeshell --ignore-not-found --wait=false >/dev/null; }

# settle <app label>: waits for the Pod, records its node, claim, UID and server.
settle() {
  local app="$1" pod node claim sid
  wait_node "the Pod ${app} running" "${E2E_NODE_TIMEOUT}" "app=${app}" "" pod_running "app=${app}"
  pod="$(kubectl get pod -l "app=${app}" -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  assert_nonempty "the NodeClaim of ${node}" "${claim}"
  sid="$(instance_of_claim "${claim}")"
  assert_nonempty "the server of ${claim}" "${sid}"
  setstate "${app}.pod" "${pod}"
  setstate "${app}.node" "${node}"
  setstate "${app}.claim" "${claim}"
  setstate "${app}.uid" "$(kubectl get nodeclaim "${claim}" -o jsonpath='{.metadata.uid}')"
  setstate "${app}.sid" "${sid}"
  note "${app}: pod ${pod}, node ${node}, claim ${claim}, ready after $(ready_seconds "${claim}" "${node}")s"
}

# vcpus_of <flavor name without the vendor prefix>
vcpus_of() { os flavor show "$1" -f value -c vcpus; }

# ready: the NodeClass status of the vendor, one condition.
vendor_condition() { # vendor_condition <nodeclass> <type> [field]
  kubectl get scnodeclass "$1" -o jsonpath="{.status.vendors.ovh.conditions[?(@.type==\"$2\")].${3:-status}}"
}

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_db() { render "${here}/manifests/db.yaml" | kubectl apply -f -; }
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }
start_poweroff() { probe e2e-poweroff e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }

# start_byo: the security group the workflow creates with the OpenStack CLI (it carries no
# ownership marker, so neither the driver nor the sweeper touches it; the teardown deletes it
# by name), the NodeClass that uses it by its Neutron UUID, and the workload.
start_byo() {
  local sg
  sg="$(os security group list -f json | jq -r --arg n "${E2E_CLUSTER}-byo" '[.[] | select(.Name == $n)] | first | .ID // empty')"
  if [[ -z "${sg}" ]]; then
    sg="$(os security group create "${E2E_CLUSTER}-byo" --description "scarpenter e2e: brought by the workflow" -f value -c id)"
  fi
  setstate byo.sg "${sg}"
  E2E_BYO_SG="${sg}" render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  for t in openstack kubectl jq helm envsubst; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  os token issue -f value -c project_id >/dev/null || fail "the OpenStack credentials do not work"

  # 1. The Flatcar image (docs/how-to-guides/ovh-images.md): exactly one active private image of the name.
  local name n
  name="flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-amd64"
  n="$(os image list --name "${name}" --status active --private -f json | jq 'length')"
  assert_eq "private active images named ${name} in ${OS_REGION_NAME}" 1 "${n}"

  # 2. The flavors the suite launches exist in the region.
  local t
  for t in "${E2E_TYPE_AMD64}" "${E2E_TYPE_DB}" ${E2E_TYPES_DESCRIBED//,/ }; do
    os flavor show "${t#ovh.}" -f value -c name >/dev/null || fail "the flavor ${t#ovh.} does not exist in ${OS_REGION_NAME}"
  done

  # 3. The quota the suite needs (a new project's default is low: raise it, docs/how-to-guides/testing.md#the-l5-lane).
  local cores_free inst_free
  cores_free=$(($(limit_of maxTotalCores) - $(limit_of totalCoresUsed)))
  inst_free=$(($(limit_of maxTotalInstances) - $(limit_of totalInstancesUsed)))
  [[ "${cores_free}" -ge "${E2E_MIN_VCPU_OD}" ]] || fail "${cores_free} cores are free in ${OS_REGION_NAME}; the suite needs ${E2E_MIN_VCPU_OD}"
  [[ "${inst_free}" -ge "${E2E_MIN_INSTANCES}" ]] || fail "${inst_free} instances are free in ${OS_REGION_NAME}; the suite needs ${E2E_MIN_INSTANCES}"
  note "free quota in ${OS_REGION_NAME}: ${cores_free} cores, ${inst_free} instances"

  # 4. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc
  for nc in e2e-amd64 e2e-drift; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True "$(vendor_condition "${nc}" VendorReady)"
    assert_eq "scnodeclass/${nc} ImagesReady" True "$(vendor_condition "${nc}" ImagesReady)"
  done
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

# node_unit_checks <node>: what L4 scenarios Q09 and Q10 check inside a VM, on an OVH
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

scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim sid j
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  sid="$(getstate e2e-amd64.sid)"

  log "node Ready, labels (overview section 5.5)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "region" "${OS_REGION_NAME}" "$(node_label "${node}" topology.kubernetes.io/region)"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" ovh "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${E2E_TYPE_AMD64#ovh.}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_eq "storage-profile" default "$(node_label "${node}" "${label_domain}/storage-profile")"
  assert_eq "placement-profile" default "$(node_label "${node}" "${label_domain}/placement-profile")"
  assert_eq "nodepool" e2e-amd64 "$(node_label "${node}" karpenter.sh/nodepool)"
  assert_nonempty "zone ID" "$(node_label "${node}" "${label_domain}/zone-id")"

  log "the server: metadata, tags, addresses, the managed group (docs/reference/ovh-driver.md)"
  j="$(instance_json "${sid}")"
  assert_eq "server status" ACTIVE "$(jq -r '.status' <<<"${j}")"
  assert_eq "flavor" "${E2E_TYPE_AMD64#ovh.}" "$(flavor_of "${j}")"
  assert_eq "server name is the claim" "${claim}" "$(jq -r '.name' <<<"${j}")"
  assert_eq "metadata cluster" "${E2E_CLUSTER}" "$(meta_of "${j}" scarpenter-cluster)"
  assert_eq "metadata nodeclaim" "${claim}" "$(meta_of "${j}" scarpenter-nodeclaim)"
  assert_eq "metadata nodepool" e2e-amd64 "$(meta_of "${j}" scarpenter-nodepool)"
  assert_eq "metadata nodeclass" e2e-amd64 "$(meta_of "${j}" scarpenter-nodeclass)"
  assert_eq "metadata managed-by" scarpenter "$(meta_of "${j}" scarpenter-managed-by)"
  assert_eq "metadata idempotency key is the claim's UID" "$(getstate e2e-amd64.uid)" "$(meta_of "${j}" scarpenter-idempotency-key)"
  assert_eq "metadata ci-created-at (the NodeClass's spec.tags)" "${E2E_CREATED_AT}" "$(meta_of "${j}" scarpenter-tag-ci-created-at)"
  assert_eq "the server tag of the cluster" true "$(jq -r --arg t "$(cluster_tag)" '(.tags // []) | index($t) != null' <<<"${j}")"
  assert_match "a public IPv4 address (Ext-Net)" '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' "$(jq -r '.addresses | tostring' <<<"${j}")"
  note "config_drive of the server: $(jq -r '.config_drive // "unknown"' <<<"${j}")"
  local sg
  sg="$(os security group list -f json | jq -r --arg c "${E2E_CLUSTER}" '[.[] | select((.Description // "") | contains("\"cluster\":\"" + $c + "\""))] | first')"
  assert_nonempty "the managed security group of the NodeClass" "$(jq -r '.ID // empty' <<<"${sg}")"
  assert_eq "the server uses the managed group" true \
    "$(jq -r --arg n "$(jq -r '.Name' <<<"${sg}")" '[(.security_groups // [])[] | (.name // .)] | index($n) != null' <<<"${j}")"
  assert_match "the group's marker names the resource kind" '"resource-kind":"securityGroups"' "$(jq -r '.Description' <<<"${sg}")"

  log "the claim: price, bootstrap token, serving certificate"
  local price
  price="$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price" '.metadata.annotations[$k] // empty')"
  assert_nonempty "price annotation" "${price}"
  awk -v p="${price}" 'BEGIN { exit !(p + 0 > 0) }' || fail "the price ${price} is not positive"
  assert_eq "price-incomplete (the flavor's own disk is complete)" "" \
    "$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price-incomplete" '.metadata.annotations[$k] // empty')"
  wait_until "the bootstrap token Secret gone" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim=${claim}" -o name
  wait_until "the serving CSR approved and issued" 120 csr_approved "${node}"

  log "kubectl logs and exec (the API server reaches the kubelet over the tailnet)"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_match "the log of the probe" scarpenter-e2e-up "$(kubectl logs "${pod}")"
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"

  log "on the node: the unit-level checks of L4 scenarios Q09 and Q10, on a real node"
  node_unit_checks "${node}"

  log "cluster DNS, and the metadata service (hardening metadata-from-pods)"
  local dns
  dns="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  assert_match "the kubelet's clusterDNS is the kube-dns Service" "${dns//./\\.}" "$(node_shell "${node}" "grep -A3 '^clusterDNS' /var/lib/kubelet/config.yaml" | tr '\n' ' ')"
  wait_until "a Pod resolves a cluster Service" 120 kubectl exec "${pod}" -- nslookup kubernetes.default.svc.cluster.local
  # The NodeClass enables the hardening: a Pod's traffic to the metadata address is dropped
  # in FORWARD, the node's own is not (unverified on OVH: the node's answer is a note).
  local get='printf "GET /openstack/latest/meta_data.json HTTP/1.0\r\nHost: 169.254.169.254\r\n\r\n" | nc -w 4 169.254.169.254 80 | head -n1'
  note "the metadata service from the node itself: '$(node_pod_exec "${node}" "${get}" || true)'"
  assert_eq "the metadata service from a Pod (dropped by the hardening)" "" "$(kubectl exec "${pod}" -- sh -c "${get}" || true)"
  drop_nodeshells
}

scn_db() {
  start_db
  wait_node "the database and the client running" $((E2E_NODE_TIMEOUT + 300)) "scarpenter-e2e=true,app in (e2e-db,e2e-client)" "" \
    pod_running "scarpenter-e2e=true,app in (e2e-db,e2e-client)"
  local dbpod clpod dbnode clnode dbsid clsid dbclaim dbj clj
  dbpod="$(kubectl get pod -l app=e2e-db -o jsonpath='{.items[0].metadata.name}')"
  clpod="$(kubectl get pod -l app=e2e-client -o jsonpath='{.items[0].metadata.name}')"
  dbnode="$(pod_node "${dbpod}")"
  clnode="$(pod_node "${clpod}")"
  [[ "${dbnode}" != "${clnode}" ]] || fail "the database and the client share the node ${dbnode}"
  dbclaim="$(claim_of_node "${dbnode}")"
  dbsid="$(instance_of_claim "${dbclaim}")"
  clsid="$(instance_of_claim "$(claim_of_node "${clnode}")")"
  dbj="$(instance_json "${dbsid}")"
  clj="$(instance_json "${clsid}")"

  log "one anti-affinity server group, named by SCarpenter"
  local grp gname gid
  grp="$(os server group list -f json | jq -c '[.[] | select(.Name | startswith("scarpenter-scarpenter-ci-"))]')"
  assert_eq "server groups of the cluster" 1 "$(jq 'length' <<<"${grp}")"
  gname="$(jq -r '.[0].Name' <<<"${grp}")"
  gid="$(jq -r '.[0].ID' <<<"${grp}")"
  [[ "${#gname}" -le 63 ]] || fail "the group's name ${gname} is longer than 63 characters"
  assert_match "the group's name" "-servergroups-" "${gname}"
  assert_match "the policy" anti-affinity "$(os server group show "${gid}" -f json | jq -r '(.policy // .policies) | tostring')"
  assert_match "both servers are members" "${dbsid}" "$(os server group show "${gid}" -f json | jq -r '.members | tostring')"
  assert_match "both servers are members (client)" "${clsid}" "$(os server group show "${gid}" -f json | jq -r '.members | tostring')"

  log "node labels of the applied profiles"
  assert_eq "database storage-profile" db-volume "$(node_label "${dbnode}" "${label_domain}/storage-profile")"
  assert_eq "database placement-profile" spread "$(node_label "${dbnode}" "${label_domain}/placement-profile")"
  assert_eq "client storage-profile" default "$(node_label "${clnode}" "${label_domain}/storage-profile")"
  assert_eq "client placement-profile" spread "$(node_label "${clnode}" "${label_domain}/placement-profile")"
  assert_eq "database run key" "pg-${E2E_RUN_ID}" "$(node_label "${dbnode}" "${label_domain}/placement-group")"
  assert_eq "profile-hashes annotation names the storage profile" true \
    "$(kubectl get nodeclaim "${dbclaim}" -o json | jq -r --arg k "${label_domain}/profile-hashes" '.metadata.annotations[$k] | fromjson | has("storage/db-volume")')"

  log "the data volume: attached, a high-speed volume, mounted at the profile's path"
  local vols vol volid src
  vols="$(jq -r '(.volumes_attached // []) | map(.id // .) | .[]' <<<"${dbj}")"
  assert_eq "volumes attached to the database server (the data volume)" 1 "$(grep -c . <<<"${vols}")"
  volid="${vols}"
  vol="$(os volume show "${volid}" -f json)"
  assert_eq "data volume type" high-speed "$(jq -r '.type' <<<"${vol}")"
  assert_eq "data volume size" 10 "$(jq -r '.size' <<<"${vol}")"
  assert_eq "data volume status" in-use "$(jq -r '.status' <<<"${vol}")"
  src="$(node_shell "${dbnode}" 'findmnt -no SOURCE /var/lib/pgdata')"
  assert_nonempty "the device mounted at /var/lib/pgdata" "${src}"
  assert_eq "the filesystem at /var/lib/pgdata" xfs "$(node_shell "${dbnode}" 'findmnt -no FSTYPE /var/lib/pgdata')"
  # Which bus Flatcar sees on OVH is unverified (the plan's S8): the device's serial and name are notes.
  note "the data device ${src}: serial '$(node_shell "${dbnode}" "lsblk -dno SERIAL ${src}" | tr -d ' ')', by-id '$(node_shell "${dbnode}" 'ls /dev/disk/by-id | tr "\n" " "')'"
  assert_eq "the Pod's file is on the mounted volume" hello-from-the-volume "$(node_shell "${dbnode}" 'cat /var/lib/pgdata/hello')"
  assert_eq "the client has no volume (it boots from the flavor's disk)" 0 "$(jq -r '(.volumes_attached // []) | length' <<<"${clj}")"

  log "isolation: the NodeClass's managed security group allows its members"
  assert_eq "a managed group with a rule that refers to itself" true "$(os security group list -f json | jq -r --arg c "${E2E_CLUSTER}" '
    [.[] | select((.Description // "") | contains("\"cluster\":\"" + $c + "\""))] | length > 0')"

  log "delete both workloads: the server group and the volume go"
  drop_nodeshells
  kubectl delete statefulset e2e-db --wait=false
  kubectl delete deployment e2e-client --wait=false
  wait_until "the servers deleted" 300 is_empty cluster_instances_of "${dbsid}" "${clsid}"
  # perUse: two minutes of grace for launches in flight, then the next resource check and
  # garbage collection (30 s and 1 m here).
  wait_until "the server group deleted" 420 server_group_gone "${gid}"
  wait_until "the volume gone" 180 volume_gone "${volid}"
}

# server_group_gone <id>, volume_gone <id>: the group or volume is no longer listed.
server_group_gone() { os server group list -f json | jq -e --arg id "$1" 'all(.[]; .ID != $id)' >/dev/null; }
volume_gone() { os volume list -f json | jq -e --arg id "$1" 'all(.[]; .ID != $id)' >/dev/null; }

# server_status <id>: Nova's status of the server.
server_status() { os server show "$1" -f value -c status; }

# cluster_instances_of <id>...: those of the servers that are still listed.
cluster_instances_of() {
  local id
  for id in "$@"; do
    os server list -f json | jq -r --arg id "${id}" '.[] | select(.ID == $id) | .ID'
  done
}

scn_described() {
  start_described
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-described "" pod_running app=e2e-described
  local pod node claim sid t vcpus mem pct
  pod="$(kubectl get pod -l app=e2e-described -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  sid="$(instance_of_claim "${claim}")"
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
  assert_eq "the server is of the chosen flavor" "${t#ovh.}" "$(flavor_of "$(instance_json "${sid}")")"
  # The same fact from Nova, independent of the catalog.
  assert_eq "Nova's vCPUs of the flavor" "${vcpus}" "$(vcpus_of "${t#ovh.}")"
}

scn_byo() {
  start_byo
  local sg
  sg="$(getstate byo.sg)"
  wait_until "ResourcesReady on the NodeClass" 180 is_value True vendor_condition e2e-use ResourcesReady
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-use "" pod_running app=e2e-use
  local pod node sid j
  pod="$(kubectl get pod -l app=e2e-use -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  sid="$(instance_of_claim "$(claim_of_node "${node}")")"
  j="$(instance_json "${sid}")"
  assert_eq "the server uses the brought security group" true \
    "$(jq -r --arg n "${E2E_CLUSTER}-byo" '[(.security_groups // [])[] | (.name // .)] | index($n) != null' <<<"${j}")"
  assert_eq "the managed group of the NodeClass is not used" 0 \
    "$(os security group list -f json | jq -r --arg c "${E2E_CLUSTER}" '[.[] | select((.Description // "") | contains("\"nodeclass\":\"e2e-use\""))] | length')"

  log "SCarpenter must not touch what it did not create"
  drop_nodeshells
  kubectl delete deployment e2e-use --wait=false
  wait_until "the server deleted" 300 is_empty cluster_instances_of "${sid}"
  kubectl delete nodepool e2e-use --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-use --wait=true --timeout=120s
  # Past a garbage collection (1 m), a resource check (30 s) and the status controller:
  # a `use` reference is never modified or deleted.
  sleep 120
  assert_eq "the security group still exists" "${sg}" "$(os security group show "${sg}" -f value -c id)"
  note "the brought security group ${sg} is deleted by the teardown"
}

# negative: what the driver refuses or leaves unserved (ADR-017), and nothing is launched:
# a private-only NodeClass is not ready, and a workload that selects a storage profile OVH
# cannot serve (local storage) stays Pending without a NodeClaim.
scn_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
  wait_until "publicIPv4: false is refused" 180 is_value False vendor_condition e2e-noipv4 VendorReady
  assert_eq "the reason" ValidationFailed "$(vendor_condition e2e-noipv4 VendorReady reason)"
  assert_match "the message names the gateway" "gateway" "$(vendor_condition e2e-noipv4 VendorReady message)"
  # The vendor block's own publicIPv4 override is refused by the API server's CEL rule.
  if render "${here}/manifests/nodeclass-negative-override.yaml" | kubectl apply -f - 2>"${E2E_OUT}/negative.err"; then
    fail "the API server accepted vendors.ovh.networking.publicIPv4: false"
  fi
  assert_match "the CEL message" "publicIPv4 must stay true on OVH" "$(cat "${E2E_OUT}/negative.err")"
  kubectl delete scnodeclass e2e-noipv4 --wait=true --timeout=120s

  # A profile that is unsupported for every type: the Pod stays Pending, no NodeClaim.
  kubectl apply -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: e2e-unsupported
  labels: {app: e2e-unsupported, scarpenter-e2e: "true"}
spec:
  replicas: 1
  selector:
    matchLabels: {app: e2e-unsupported}
  template:
    metadata:
      labels: {app: e2e-unsupported, scarpenter-e2e: "true"}
    spec:
      terminationGracePeriodSeconds: 0
      nodeSelector:
        karpenter.sh/nodepool: e2e-amd64
        scarpenter.sparecores.com/storage-profile: local
      containers:
        - name: probe
          image: public.ecr.aws/docker/library/busybox:1.37.0
          command: [sh, -c, "exec sleep 86400"]
          resources:
            requests: {cpu: 100m, memory: 64Mi}
YAML
  sleep 90
  assert_eq "no NodeClaim for the unsupported profile" 0 \
    "$(kjson nodeclaims | jq '[.items[] | select(.metadata.labels["scarpenter.sparecores.com/storage-profile"] == "local")] | length')"
  assert_eq "the Pod is Pending" Pending "$(kubectl get pod -l app=e2e-unsupported -o jsonpath='{.items[0].status.phase}')"
  kubectl delete deployment e2e-unsupported --wait=false
}

# quota (overview section 13.4): what the controller reports equals what Nova says. Run with
# the nodes of the other scenarios up and no launch in flight.
scn_quota() {
  settle_fixtures
  local m b
  m="$(mktemp)"
  log "the limits of the three buckets equal Nova's absolute limits"
  local timeout=240
  for b in "${quota_buckets[@]}"; do
    wait_until "scarpenter_quota_limit ${b} = Nova's ${quota_limit_name[${b}]}" "${timeout}" quota_limit_is "${m}" "${b}"
    timeout=90 # the first one waited for the controller's first limits refresh
  done
  log "usage: what the controller counts is what Nova says the project uses"
  for b in "${quota_buckets[@]}"; do
    wait_until "scarpenter_quota_used ${b} = Nova's ${quota_used_name[${b}]}" 300 quota_used_is "${m}" "${b}"
    wait_until "no reservation of ${b} is in flight" 240 quota_reserved_zero "${m}" "${b}"
  done
  local cores
  cores="$(metric_value "${m}" scarpenter_quota_used "${OS_REGION_NAME}" cores)"
  [[ "$(num "${cores}")" -ge "$(vcpus_of "${E2E_TYPE_AMD64#ovh.}")" ]] || fail "the cores usage ${cores} does not include the launched server"

  log "the launched flavor is offered in its zone"
  local zid
  zid="$(node_label "$(getstate e2e-amd64.node)" "${label_domain}/zone-id")"
  assert_nonempty "the zone ID of the amd64 node" "${zid}"
  assert_eq "the flavor is listed by Nova" "${E2E_TYPE_AMD64#ovh.}" "$(os flavor show "${E2E_TYPE_AMD64#ovh.}" -f value -c name)"
  rm -f "${m}"
}

quota_limit_is() { # quota_limit_is <metrics file> <bucket>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_limit "${OS_REGION_NAME}" "$2")")" == "$(num "$(limit_of "${quota_limit_name[$2]}")")" ]]
}

quota_used_is() { # quota_used_is <metrics file> <bucket>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_used "${OS_REGION_NAME}" "$2")")" == "$(num "$(limit_of "${quota_used_name[$2]}")")" ]]
}

quota_reserved_zero() { # quota_reserved_zero <metrics file> <bucket>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_reserved "${OS_REGION_NAME}" "$2")")" == 0 ]]
}

# settle_fixtures: the amd64 node of the first scenario (started if the scenario runs alone).
settle_fixtures() {
  [[ -n "$(getstate e2e-amd64.sid)" ]] || {
    start_amd64
    settle e2e-amd64
  }
}

# drift (plan 10 scenario 4): a change to a profile no node uses drifts nothing; a change to
# the vendor block (the root disk becomes a volume made from the image) replaces the node.
scn_drift() {
  start_drift
  settle e2e-drift
  local old_claim old_node old_sid old_pod
  old_claim="$(getstate e2e-drift.claim)"
  old_node="$(getstate e2e-drift.node)"
  old_sid="$(getstate e2e-drift.sid)"
  old_pod="$(getstate e2e-drift.pod)"
  assert_eq "the first server boots from the flavor's disk (no volume)" 0 \
    "$(jq -r '(.volumes_attached // []) | length' <<<"$(instance_json "${old_sid}")")"

  log "a profile no running node uses changes: nothing drifts"
  kubectl patch scnodeclass e2e-drift --type merge -p \
    '{"spec":{"profiles":{"storage":{"scratch":{"ovh":{"data":[{"name":"scratch","volumeType":"classic","size":"10Gi","filesystem":"ext4","mountPath":"/var/lib/scratch"}]}}}}}}'
  wait_until "the NodeClass reconciled the change" 120 nodeclass_observed e2e-drift
  local i
  for i in 1 2 3 4 5 6; do
    sleep 10
    assert_eq "the claim is not drifted (${i}/6)" "" \
      "$(kubectl get nodeclaim "${old_claim}" -o jsonpath='{.status.conditions[?(@.type=="Drifted")].status}' | grep -x True || true)"
  done
  assert_eq "still the same claim" "${old_claim}" "$(claim_of_node "${old_node}")"

  log "the root disk becomes a volume: the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p \
    '{"spec":{"vendors":{"ovh":{"storage":{"root":{"bootFromVolume":{"volumeType":"high-speed","size":"25Gi"}}}}}}}'
  wait_node "the Pod on another node" "${E2E_NODE_TIMEOUT}" app=e2e-drift "${old_claim}" pod_moved_from app=e2e-drift "${old_node}"
  local pod node claim sid vol
  pod="$(kubectl get pod -l app=e2e-drift -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  sid="$(instance_of_claim "${claim}")"
  [[ "${sid}" != "${old_sid}" ]] || fail "the replacement runs on the old server"
  assert_eq "the replacement boots from one volume" 1 "$(jq -r '(.volumes_attached // []) | length' <<<"$(instance_json "${sid}")")"
  vol="$(jq -r '(.volumes_attached // []) | map(.id // .) | .[0]' <<<"$(instance_json "${sid}")")"
  assert_eq "the root volume's type" high-speed "$(os volume show "${vol}" -f value -c type)"
  assert_eq "the root volume's size" 25 "$(os volume show "${vol}" -f value -c size)"
  wait_until "the old server deleted" 300 is_empty cluster_instances_of "${old_sid}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  assert_no_orphans
  setstate e2e-drift.node "${node}"
  setstate e2e-drift.claim "${claim}"
  setstate e2e-drift.sid "${sid}"
}

nodeclass_observed() { # the status is for the generation of the spec
  [[ "$(kubectl get scnodeclass "$1" -o jsonpath='{.metadata.generation}')" == "$(kubectl get scnodeclass "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].observedGeneration}')" ]]
}

pod_moved_from() { # pod_moved_from <label selector> <node>: running, and not on <node>
  pod_running "$1" && [[ "$(kubectl get pod -l "$1" -o jsonpath='{.items[0].spec.nodeName}')" != "$2" ]]
}

# assert_no_orphans: every server of the cluster belongs to a NodeClaim that exists.
assert_no_orphans() {
  wait_until "no orphaned server" 240 no_orphans
}

no_orphans() {
  local claims ids id
  claims="$(kjson nodeclaims | jq -r --arg k "${label_domain}/instance-ref" '.items[] | .metadata.annotations[$k] // empty' | sed 's#.*/##')"
  ids="$(cluster_instances 'ACTIVE|BUILD')"
  for id in ${ids}; do
    grep -qx "${id}" <<<"${claims}" || {
      echo "server ${id} has no NodeClaim"
      return 1
    }
  done
}

# poweroff (plan 18): a node whose guest powers itself off leaves a SHUTOFF server that OVH keeps
# billing (OpenStack has no shutdown behaviour terminate). The server stays listed, the driver
# reports it terminating, and the garbage collector deletes it after the terminating grace
# (gc.terminatingGrace of the lane's values) plus a collection.
scn_poweroff() {
  start_poweroff
  settle e2e-poweroff
  local node claim sid
  node="$(getstate e2e-poweroff.node)"
  claim="$(getstate e2e-poweroff.claim)"
  sid="$(getstate e2e-poweroff.sid)"
  log "the guest powers itself off"
  node_shell "${node}" 'systemctl poweroff' >/dev/null 2>&1 || true
  wait_until "the server is SHUTOFF" 180 is_value SHUTOFF server_status "${sid}"
  note "poweroff: the server ${sid} is SHUTOFF (still billed by OVH); waiting for the terminating grace"
  wait_until "the server deleted by the garbage collector" 900 is_empty cluster_instances_of "${sid}"
  wait_until "the NodeClaim ${claim} gone" 300 is_empty kubectl get nodeclaim "${claim}" -o name --ignore-not-found
  kubectl delete deployment e2e-poweroff --wait=false --ignore-not-found
  assert_no_orphans
}

# scalezero (plan 10 scenario 3, the roadmap's exit criterion): deleting the workloads
# deletes every server within E2E_SCALE_DOWN_DEADLINE seconds of the Pods' deletion
# (WhenEmpty, 0 s); the nodes, NodeClaims and bootstrap tokens go too; the quota usage falls
# with the servers.
scn_scalezero() {
  local t0 elapsed
  drop_nodeshells
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every server deleted" "${E2E_SCALE_DOWN_DEADLINE}" is_empty cluster_instances
  elapsed=$(($(now) - t0))
  note "scale to zero: every server was gone ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s)"
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name
  log "the quota usage follows the servers down"
  local m b
  m="$(mktemp)"
  for b in "${quota_buckets[@]}"; do
    wait_until "the ${b} usage equals Nova's" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" "${b}"
    wait_until "no reservation of ${b} is left" 120 quota_reserved_zero "${m}" "${b}"
  done
  rm -f "${m}"
}

# uninstall: the procedure of docs/how-to-guides/install.md#uninstall, on the live project.
scn_uninstall() {
  log "1. SCarpenter's NodePools (selected by nodeClassRef.group), then wait for the NodeClaims"
  kjson nodepools | jq -r '.items[] | select(.spec.template.spec.nodeClassRef.group == "scarpenter.sparecores.com") | .metadata.name' |
    xargs -r kubectl delete nodepool --wait=true --timeout=300s
  wait_until "no NodeClaim of SCarpenter left" 300 is_value 0 scarpenter_claims
  log "2. the SCNodeClasses; the managed resources go by their lifecycle"
  kubectl delete scnodeclass --all --wait=true --timeout=300s
  wait_until "no server of the cluster" 300 is_empty cluster_instances
  # With the NodeClasses gone the managed security groups fall under the default idle
  # lifetime (managedResources.defaultTTL, 5 minutes in the lane's values; 24 hours by default).
  wait_until "no security group of SCarpenter left" 900 is_value 0 managed_groups
  wait_until "no server group left" 60 is_value 0 managed_server_groups
  log "3. helm uninstall"
  helm uninstall scarpenter --namespace "${ns}" --wait --timeout 3m
  wait_until "the controller gone" 120 is_empty kubectl -n "${ns}" get pods -l app.kubernetes.io/name=scarpenter -o name
}

# scarpenter_claims: how many NodeClaims belong to an SCNodeClass.
scarpenter_claims() {
  kjson nodeclaims | jq '[.items[] | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'
}

# managed_groups: the security groups SCarpenter created (the brought one carries no marker).
managed_groups() {
  os security group list -f json | jq --arg c "${E2E_CLUSTER}" '[.[] | select((.Description // "") | contains("\"cluster\":\"" + $c + "\""))] | length'
}

# managed_server_groups: the server groups SCarpenter created (by the deterministic name prefix).
managed_server_groups() {
  os server group list -f json | jq '[.[] | select(.Name | startswith("scarpenter-scarpenter-ci-"))] | length'
}

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 db described byo negative quota drift poweroff scalezero uninstall)
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
