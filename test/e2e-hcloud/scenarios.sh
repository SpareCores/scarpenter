#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5 of Hetzner Cloud: the scenarios, run against the cluster that bootstrap.sh and install.sh
# built (docs/plans/v1/wip/15-hcloud.md, "Testing: L5 lane, sweeper, canary";
# docs/how-to-guides/testing.md#the-l5-lane lists what each proves and how to read a failure).
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 arm64 db described byo offerings negative drift scalezero final
#                   (preflight always runs first, and ends the run when it fails)
#
# Each scenario is a function that runs in a subshell with `set -e`: the first failed
# assertion ends it, the runner records the result and goes on. Waits have explicit deadlines.
# Workloads of all selected scenarios are started together (the prelude), so that the nodes
# boot in parallel; a scenario then only waits and checks. The exit status is 1 when a scenario
# failed; with E2E_FAIL_FAST=true (the default) the scenarios after the first failure are
# reported as SKIP, so that the diagnostics and the teardown start at once. A failed scenario
# prints one redacted snapshot of the cluster (observe.sh), and the waits for nodes print a
# progress block every E2E_PROGRESS_INTERVAL seconds and fail early when no NodeClaim is
# launched after E2E_LAUNCH_TIMEOUT. A full run has up to nine servers at once: the project's
# server limit must allow E2E_HCLOUD_SERVER_LIMIT (10).
#
# Needs: kubectl, jq, helm, hcloud, curl, envsubst.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-hcloud/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT E2E_HCLOUD_LOCATIONS KUBECONFIG HCLOUD_TOKEN
mkdir -p "${E2E_OUT}/state" "${E2E_OUT}/logs"
state="${E2E_OUT}/state"
# The location of the placement group and the bring-your-own network: the first of the lane's.
E2E_PG_LOCATION="${E2E_BYO_LOCATION}"
export E2E_PG_LOCATION

# ------------------------------------------------------------- state --------

setstate() { printf '%s' "$2" >"${state}/$1"; } # setstate <name> <value>
getstate() { cat "${state}/$1" 2>/dev/null || true; }

# drop_nodeshells: the privileged Pods of node_shell would keep a node busy, and an
# empty node is what consolidation removes.
drop_nodeshells() { kubectl delete pods -l scarpenter-e2e=nodeshell --ignore-not-found --wait=false >/dev/null; }

# settle <app label>: waits for the Pod, records its node, claim, UID, server and Primary IPs.
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
  # The Primary IPs Hetzner made with the server: scalezero asserts they are gone with it.
  setstate "${app}.pips" "$(server_json "${sid}" | jq -r '[.public_net.ipv4.id, .public_net.ipv6.id] | map(select(. != null)) | join(" ")')"
  note "${app}: pod ${pod}, node ${node}, claim ${claim}, ready after $(ready_seconds "${claim}" "${node}")s"
}

# label_of <server json> <label key>
label_of() { jq -r --arg k "$2" '.labels[$k] // empty' <<<"$1"; }

# servers_left <id>...: those of the servers that still exist.
servers_left() {
  local id
  for id in "$@"; do
    hc server describe "${id}" -o json >/dev/null 2>&1 && echo "${id}"
  done
  return 0
}

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_arm64() { probe e2e-arm64 e2e-arm64 "${E2E_TYPE_ARM64}" on-demand; }
start_db() { render "${here}/manifests/db.yaml" | kubectl apply -f -; }
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }

# start_byo: the network (with a cloud subnet of the location's network zone) and the firewall the
# workflow creates with the hcloud CLI, labelled with the cluster, the selector label and the
# run's creation time (so that the sweeper finds them); the NodeClass that uses them, and the workload.
start_byo() {
  local net fw zone
  net="$(hc network list -l "scarpenter-e2e-byo=${E2E_CLUSTER}" -o noheader -o columns=id | head -n1)"
  if [[ -z "${net}" ]]; then
    zone="$(network_zone_of "${E2E_BYO_LOCATION}")"
    hc network create --name "${E2E_CLUSTER}-byo" --ip-range 10.77.0.0/21 \
      --label "${ctag}=${E2E_CLUSTER}" --label "scarpenter-e2e-byo=${E2E_CLUSTER}" --label "ci-created-at=${E2E_CREATED_AT}" >/dev/null
    hc network add-subnet "${E2E_CLUSTER}-byo" --type cloud --network-zone "${zone}" --ip-range 10.77.0.0/24 >/dev/null
    net="$(hc network describe "${E2E_CLUSTER}-byo" -o json | jq -r '.id')"
  fi
  fw="$(hc firewall list -l "scarpenter-e2e-byo=${E2E_CLUSTER}" -o noheader -o columns=id | head -n1)"
  if [[ -z "${fw}" ]]; then
    hc firewall create --name "${E2E_CLUSTER}-byo" \
      --label "${ctag}=${E2E_CLUSTER}" --label "scarpenter-e2e-byo=${E2E_CLUSTER}" --label "ci-created-at=${E2E_CREATED_AT}" >/dev/null
    fw="$(hc firewall describe "${E2E_CLUSTER}-byo" -o json | jq -r '.id')"
  fi
  setstate byo.network "${net}"
  setstate byo.firewall "${fw}"
  render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  for t in hcloud kubectl jq helm envsubst curl; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  hc server list -o noheader >/dev/null || fail "the Hetzner token does not work"

  # 1. The Flatcar snapshots (docs/how-to-guides/hcloud-images.md): exactly one per architecture.
  local arch n
  for arch in x86 arm; do
    n="$(hc image list -t snapshot -a "${arch}" \
      -l "os=flatcar,flatcar-channel=${E2E_FLATCAR_CHANNEL},flatcar-version=${E2E_FLATCAR_VERSION}" -o noheader -o columns=id | sed '/^$/d' | wc -l | tr -d ' ')"
    assert_eq "Flatcar ${E2E_FLATCAR_CHANNEL} ${E2E_FLATCAR_VERSION} snapshots (${arch})" 1 "${n}"
  done

  # 2. The lane's locations exist, and the project holds nothing of another run that would
  #    take the server limit (the limit itself cannot be read: it is asked for in the Console).
  local loc
  for loc in ${E2E_HCLOUD_LOCATIONS//,/ }; do
    hc location describe "${loc}" >/dev/null || fail "the location ${loc} does not exist"
  done
  n="$(hc server list -o noheader -o columns=id | sed '/^$/d' | wc -l | tr -d ' ')"
  [[ "${n}" -lt "${E2E_HCLOUD_SERVER_LIMIT}" ]] || fail "the project has ${n} servers: no room under the limit of ${E2E_HCLOUD_SERVER_LIMIT}"
  note "servers in the project at the start: ${n} (limit asked for: ${E2E_HCLOUD_SERVER_LIMIT})"

  # 3. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc
  for nc in e2e-amd64 e2e-arm64 e2e-drift; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.hcloud.conditions[?(@.type=="VendorReady")].status}')"
    assert_eq "scnodeclass/${nc} ImagesReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.hcloud.conditions[?(@.type=="ImagesReady")].status}')"
  done
  note "locations of the lane: ${E2E_HCLOUD_LOCATIONS}; placement group and bring-your-own network in ${E2E_PG_LOCATION}"
}

scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim sid loc j
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  sid="$(getstate e2e-amd64.sid)"

  log "node Ready, labels (overview section 5.5)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" hcloud "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${E2E_TYPE_AMD64#hcloud.}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_eq "storage-profile" default "$(node_label "${node}" "${label_domain}/storage-profile")"
  assert_eq "placement-profile" default "$(node_label "${node}" "${label_domain}/placement-profile")"
  assert_eq "nodepool" e2e-amd64 "$(node_label "${node}" karpenter.sh/nodepool)"
  loc="$(node_label "${node}" topology.kubernetes.io/region)"
  assert_match "region is one of the lane's locations" ",${loc}," ",${E2E_HCLOUD_LOCATIONS},"
  assert_eq "zone = region (a location is its own zone)" "${loc}" "$(node_label "${node}" topology.kubernetes.io/zone)"
  assert_eq "zone-id = location" "${loc}" "$(node_label "${node}" "${label_domain}/zone-id")"

  log "the server: labels, status, location, address"
  j="$(server_json "${sid}")"
  assert_eq "server status" running "$(jq -r '.status' <<<"${j}")"
  assert_eq "server type" "${E2E_TYPE_AMD64#hcloud.}" "$(jq -r '.server_type.name' <<<"${j}")"
  assert_eq "location" "${loc}" "$(location_of "${j}")"
  assert_eq "label cluster" "${E2E_CLUSTER}" "$(label_of "${j}" "${ctag}")"
  assert_eq "label nodeclaim" "${claim}" "$(label_of "${j}" "${label_domain}/nodeclaim")"
  assert_eq "label nodepool" e2e-amd64 "$(label_of "${j}" "${label_domain}/nodepool")"
  assert_eq "label nodeclass" e2e-amd64 "$(label_of "${j}" "${label_domain}/nodeclass")"
  assert_eq "label managed-by" scarpenter "$(label_of "${j}" "${label_domain}/managed-by")"
  assert_eq "label capacity-type" on-demand "$(label_of "${j}" "${label_domain}/capacity-type")"
  assert_eq "label idempotency-key is the NodeClaim UID" "$(getstate e2e-amd64.uid)" "$(label_of "${j}" "${label_domain}/idempotency-key")"
  assert_eq "label ci-created-at (the NodeClass's spec.tags)" "${E2E_CREATED_AT}" "$(label_of "${j}" ci-created-at)"
  assert_eq "the server's name is the NodeClaim's" "${claim}" "$(jq -r '.name' <<<"${j}")"
  assert_nonempty "public IPv4" "$(jq -r '.public_net.ipv4.ip // empty' <<<"${j}")"
  assert_eq "no private network (none configured)" 0 "$(jq -r '.private_net | length' <<<"${j}")"
  assert_eq "one firewall applied (the managed deny-inbound firewall)" 1 "$(jq -r '.public_net.firewalls | length' <<<"${j}")"
  assert_eq "no placement group" null "$(jq -r '.placement_group // "null"' <<<"${j}")"
  # Unverified until this run (plan 15): the first boot expands the root partition to the type's disk.
  local disk size
  disk="$(jq -r '.primary_disk_size' <<<"${j}")"
  size="$(node_shell "${node}" 'df -B1G --output=size / | tail -n1' | tr -d ' G')"
  note "root filesystem ${size} GB of the type's ${disk} GB disk"
  [[ "${size}" -ge $((disk * 8 / 10)) ]] || fail "the root filesystem (${size} GB) was not expanded to the type's disk (${disk} GB)"

  log "the claim: price, bootstrap token, serving certificate"
  local price
  price="$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price" '.metadata.annotations[$k] // empty')"
  assert_nonempty "price annotation" "${price}"
  awk -v p="${price}" 'BEGIN { exit !(p + 0 > 0) }' || fail "the price ${price} is not positive"
  assert_eq "price-incomplete (the IPv4 constant makes the default price complete)" "" \
    "$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price-incomplete" '.metadata.annotations[$k] // empty')"
  wait_until "the bootstrap token Secret gone" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim=${claim}" -o name
  wait_until "the serving CSR approved and issued" 120 csr_approved "${node}"

  log "kubectl logs and exec (the API server reaches the kubelet over the tailnet)"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_match "the log of the probe" scarpenter-e2e-up "$(kubectl logs "${pod}")"
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"

  log "on the node: the unit-level checks of L4 scenarios Q09 and Q10, on a real node"
  node_unit_checks "${node}"

  log "cluster DNS from a Pod"
  local dns
  dns="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  assert_match "the kubelet's clusterDNS is the kube-dns Service" "${dns//./\\.}" "$(node_shell "${node}" "grep -A3 '^clusterDNS' /var/lib/kubelet/config.yaml" | tr '\n' ' ')"
  wait_until "a Pod resolves a cluster Service" 120 kubectl exec "${pod}" -- nslookup kubernetes.default.svc.cluster.local
  drop_nodeshells
}

# csr_approved <node>: the node's serving CSR is approved and has a certificate.
csr_approved() {
  kubectl get csr -o json | jq -e --arg n "system:node:$1" '
    [.items[] | select(.spec.username == $n and .spec.signerName == "kubernetes.io/kubelet-serving")
      | select((.status.conditions // [] | map(.type) | index("Approved")) and (.status.certificate // "") != "")] | length > 0' >/dev/null
}

# node_unit_checks <node>: what L4 scenarios Q09 and Q10 check inside a VM, on a Hetzner node,
# through the node shell: the tailscale sysext merged, tailscaled upheld, the kubelet ordered
# after and bound to the join unit and started with the tailnet address.
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
  local pod node sid j
  pod="$(getstate e2e-arm64.pod)"
  node="$(getstate e2e-arm64.node)"
  sid="$(getstate e2e-arm64.sid)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_ARM64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "arch" arm64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "capacity-type (Hetzner has no spot)" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  j="$(server_json "${sid}")"
  assert_eq "server type" "${E2E_TYPE_ARM64#hcloud.}" "$(jq -r '.server_type.name' <<<"${j}")"
  assert_eq "architecture" arm "$(jq -r '.server_type.architecture' <<<"${j}")"
  assert_eq "label cluster" "${E2E_CLUSTER}" "$(label_of "${j}" "${ctag}")"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  assert_eq "tailscaled, the join unit and the kubelet are active (arm64 sysext)" "active,active,active" \
    "$(node_shell "${node}" 'systemctl is-active tailscaled scarpenter-tailscale-up.service kubelet' | paste -sd, -)"
  drop_nodeshells
}

scn_db() {
  start_db
  wait_node "the database and the client running" $((E2E_NODE_TIMEOUT + 300)) "scarpenter-e2e=true,app in (e2e-db,e2e-client)" "" \
    pod_running "scarpenter-e2e=true,app in (e2e-db,e2e-client)"
  local dbpod clpod dbnode clnode dbsid clsid dbclaim dbj clj pg pgid vol volid
  dbpod="$(kubectl get pod -l app=e2e-db -o jsonpath='{.items[0].metadata.name}')"
  clpod="$(kubectl get pod -l app=e2e-client -o jsonpath='{.items[0].metadata.name}')"
  dbnode="$(pod_node "${dbpod}")"
  clnode="$(pod_node "${clpod}")"
  [[ "${dbnode}" != "${clnode}" ]] || fail "the database and the client share the node ${dbnode}"
  dbclaim="$(claim_of_node "${dbnode}")"
  dbsid="$(instance_of_claim "${dbclaim}")"
  clsid="$(instance_of_claim "$(claim_of_node "${clnode}")")"
  dbj="$(server_json "${dbsid}")"
  clj="$(server_json "${clsid}")"

  log "one spread placement group, named and labelled by SCarpenter, with both servers"
  pg="$(hc placement-group list -l "${ctag}=${E2E_CLUSTER}" -o json)"
  assert_eq "placement groups of the cluster" 1 "$(jq 'length' <<<"${pg}")"
  pgid="$(jq -r '.[0].id' <<<"${pg}")"
  assert_match "the group's name" "^scarpenter-" "$(jq -r '.[0].name' <<<"${pg}")"
  [[ "$(jq -r '.[0].name | length' <<<"${pg}")" -le 63 ]] || fail "the group's name is longer than 63 characters"
  assert_eq "type" spread "$(jq -r '.[0].type' <<<"${pg}")"
  assert_eq "label managed-by" scarpenter "$(jq -r --arg k "${label_domain}/managed-by" '.[0].labels[$k]' <<<"${pg}")"
  assert_eq "label resource-kind" placementGroups "$(jq -r --arg k "${label_domain}/resource-kind" '.[0].labels[$k]' <<<"${pg}")"
  assert_eq "label nodeclass" e2e-amd64 "$(jq -r --arg k "${label_domain}/nodeclass" '.[0].labels[$k]' <<<"${pg}")"
  assert_eq "the database is in the group" "${pgid}" "$(jq -r '.placement_group.id' <<<"${dbj}")"
  assert_eq "the client is in the group" "${pgid}" "$(jq -r '.placement_group.id' <<<"${clj}")"
  assert_eq "one location" "${E2E_PG_LOCATION}" "$(location_of "${dbj}")"
  assert_eq "one location (client)" "${E2E_PG_LOCATION}" "$(location_of "${clj}")"

  log "node labels of the applied profiles"
  assert_eq "database storage-profile" db "$(node_label "${dbnode}" "${label_domain}/storage-profile")"
  assert_eq "database placement-profile" spread "$(node_label "${dbnode}" "${label_domain}/placement-profile")"
  assert_eq "client storage-profile" default "$(node_label "${clnode}" "${label_domain}/storage-profile")"
  assert_eq "client placement-profile" spread "$(node_label "${clnode}" "${label_domain}/placement-profile")"
  assert_eq "database run key" "pg-${E2E_RUN_ID}" "$(node_label "${dbnode}" "${label_domain}/placement-group")"
  assert_eq "client run key" "pg-${E2E_RUN_ID}" "$(node_label "${clnode}" "${label_domain}/placement-group")"
  assert_eq "profile-hashes annotation names the storage profile" true \
    "$(kubectl get nodeclaim "${dbclaim}" -o json | jq -r --arg k "${label_domain}/profile-hashes" '.metadata.annotations[$k] | fromjson | has("storage/db")')"

  log "the volume: created before the server, attached, formatted and mounted at the profile's path"
  vol="$(hc volume list -l "${ctag}=${E2E_CLUSTER}" -o json)"
  assert_eq "volumes of the cluster" 1 "$(jq 'length' <<<"${vol}")"
  volid="$(jq -r '.[0].id' <<<"${vol}")"
  assert_eq "volume size" 10 "$(jq -r '.[0].size' <<<"${vol}")"
  assert_eq "volume location" "${E2E_PG_LOCATION}" "$(jq -r '.[0].location.name' <<<"${vol}")"
  assert_eq "volume attached to the database's server" "${dbsid}" "$(jq -r '.[0].server' <<<"${vol}")"
  assert_eq "label resource-kind" volumes "$(jq -r --arg k "${label_domain}/resource-kind" '.[0].labels[$k]' <<<"${vol}")"
  local src byid
  src="$(node_shell "${dbnode}" 'findmnt -no SOURCE /var/lib/pgdata')"
  assert_nonempty "the device mounted at /var/lib/pgdata" "${src}"
  byid="$(node_shell "${dbnode}" "readlink -f /dev/disk/by-id/scsi-0HC_Volume_${volid}")"
  assert_eq "the mounted device is the volume's by-id link" "${byid}" "${src}"
  assert_eq "the filesystem at /var/lib/pgdata" xfs "$(node_shell "${dbnode}" 'findmnt -no FSTYPE /var/lib/pgdata')"
  assert_eq "the Pod's file is on the mounted volume" hello-from-the-volume "$(node_shell "${dbnode}" 'cat /var/lib/pgdata/hello')"

  log "delete both workloads: the group and the volume go"
  drop_nodeshells
  kubectl delete statefulset e2e-db --wait=false
  kubectl delete deployment e2e-client --wait=false
  wait_until "the servers deleted" 300 is_empty servers_left "${dbsid}" "${clsid}"
  # perUse: two minutes of grace for launches in flight, then the next resource check and
  # garbage collection (30 s and 1 m here).
  wait_until "the placement group deleted" 420 is_value 0 count_of placement-group
  wait_until "the volume gone" 240 is_value 0 count_of volume
}

# count_of <kind>: how many objects of the kind carry the cluster label.
count_of() { hc "$1" list -l "${ctag}=${E2E_CLUSTER}" -o noheader -o columns=id | sed '/^$/d' | wc -l | tr -d ' '; }

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
  [[ "${vcpus}" -gt 1 && "${vcpus}" -lt 3 ]] || fail "vcpus ${vcpus} is outside (1, 3)"
  [[ "${mem}" -gt 3000 ]] || fail "memory ${mem} MiB is not above 3000"
  [[ "${pct}" -gt 0 ]] || fail "the percentile ${pct} is not above 0"
  assert_eq "the server is of the chosen type" "${t#hcloud.}" "$(server_json "${sid}" | jq -r '.server_type.name')"
  # The same facts from Hetzner, independent of the catalog.
  assert_eq "Hetzner's vCPUs of the type" "${vcpus}" "$(hc server-type describe "${t#hcloud.}" -o json | jq -r '.cores')"
}

scn_byo() {
  start_byo
  local net fw
  net="$(getstate byo.network)"
  fw="$(getstate byo.firewall)"
  wait_until "ResourcesReady on the NodeClass" 180 is_value True \
    kubectl get scnodeclass e2e-use -o jsonpath='{.status.vendors.hcloud.conditions[?(@.type=="ResourcesReady")].status}'
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-use "" pod_running app=e2e-use
  local pod node sid j
  pod="$(kubectl get pod -l app=e2e-use -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  sid="$(instance_of_claim "$(claim_of_node "${node}")")"
  j="$(server_json "${sid}")"
  assert_eq "the server is in the brought network" "${net}" "$(jq -r '.private_net[0].network' <<<"${j}")"
  assert_match "the private IP is in the brought subnet" '^10\.77\.0\.' "$(jq -r '.private_net[0].ip' <<<"${j}")"
  assert_eq "the brought firewall is applied" true "$(jq -r --argjson fw "${fw}" '[.public_net.firewalls[].id] | index($fw) != null' <<<"${j}")"
  assert_eq "the location of the NodeClass" "${E2E_BYO_LOCATION}" "$(location_of "${j}")"
  assert_eq "SCarpenter created no firewall for this NodeClass" 0 \
    "$(hc firewall list -l "${label_domain}/nodeclass=e2e-use" -o noheader -o columns=id | sed '/^$/d' | wc -l | tr -d ' ')"

  log "SCarpenter must not touch what it did not create"
  drop_nodeshells
  kubectl delete deployment e2e-use --wait=false
  wait_until "the server deleted" 300 is_empty servers_left "${sid}"
  kubectl delete nodepool e2e-use --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-use --wait=true --timeout=120s
  # Past a garbage collection (1 m), a resource check (30 s) and the status controller: a
  # `use` reference is never modified or deleted.
  sleep 120
  assert_eq "the network still exists" "${net}" "$(hc network describe "${net}" -o json | jq -r '.id')"
  assert_eq "the firewall still exists" "${fw}" "$(hc firewall describe "${fw}" -o json | jq -r '.id')"
  note "the brought network ${net} and firewall ${fw} are deleted by the final scenario or the teardown"
}

# offerings: what the driver offers comes from GET /server_types: the launched type's entry for
# its location is available, and an offering the API lists as unavailable is not scheduled.
scn_offerings() {
  settle_fixtures
  local loc type t
  loc="$(node_label "$(getstate e2e-amd64.node)" topology.kubernetes.io/region)"
  type="${E2E_TYPE_AMD64#hcloud.}"
  t="$(hapi "server_types?name=${type}")"
  assert_eq "${type} is available in ${loc} (GET /server_types)" true \
    "$(jq -r --arg l "${loc}" '.server_types[0].locations[] | select(.name == $l) | .available' <<<"${t}")"
  local gone
  gone="$(jq -r --arg locs "${E2E_HCLOUD_LOCATIONS}" '
    ($locs | split(",")) as $l | [.server_types[0].locations[] | select(.available == false and (.name as $n | $l | index($n)))] | map(.name) | join(" ")' <<<"${t}")"
  if [[ -z "${gone}" ]]; then
    note "offerings: ${type} is available in every lane location now; the unavailable-offering case was not exercised"
    return 0
  fi
  note "offerings: ${type} is listed unavailable in: ${gone}"
  local l
  for l in ${gone}; do
    NAME="e2e-unavail" POOL=e2e-amd64 TYPE="${E2E_TYPE_AMD64}" CAPACITY=on-demand render "${here}/manifests/probe.yaml" |
      kubectl patch --local -f - -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"topology.kubernetes.io/zone\":\"${l}\"}}}}}" -o yaml |
      kubectl apply -f -
    sleep 60
    assert_eq "nothing is launched for the unavailable offering in ${l}" 0 \
      "$(kjson nodeclaims | jq --arg l "${l}" '[.items[] | select(.metadata.labels["topology.kubernetes.io/zone"] == $l and .metadata.labels["karpenter.sh/nodepool"] == "e2e-amd64")] | length')"
    kubectl delete deployment e2e-unavail --wait=false
  done
}

# negative: what the project cannot serve is refused, or not launched, and nothing is launched.
scn_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
  wait_until "a server without a public address is refused" 180 is_value False \
    kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.hcloud.conditions[?(@.type=="VendorReady")].status}'
  assert_eq "the reason" ValidationFailed \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.hcloud.conditions[?(@.type=="VendorReady")].reason}')"
  assert_match "the message names the fix" "public" \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.hcloud.conditions[?(@.type=="VendorReady")].message}')"
  # An arm type pinned to a location without it: the Pod stays Pending, no launch attempt.
  wait_until "ResourcesReady on e2e-noarm" 180 is_value True \
    kubectl get scnodeclass e2e-noarm -o jsonpath='{.status.vendors.hcloud.conditions[?(@.type=="ResourcesReady")].status}'
  probe e2e-noarm e2e-noarm "${E2E_TYPE_ARM64}" on-demand
  sleep 90
  assert_eq "the Pod is Pending" Pending "$(kubectl get pod -l app=e2e-noarm -o jsonpath='{.items[0].status.phase}')"
  assert_eq "no NodeClaim of the pool (no launch attempt)" 0 \
    "$(kjson nodeclaims | jq '[.items[] | select(.metadata.labels["karpenter.sh/nodepool"] == "e2e-noarm")] | length')"
  note "negative: the Pod's events: $(kubectl get events --field-selector involvedObject.kind=Pod --sort-by=.lastTimestamp -o json |
    jq -r '[.items[] | select(.involvedObject.name | startswith("e2e-noarm"))] | last | .message // "none"' | cut -c1-300)"
  kubectl delete deployment e2e-noarm --wait=false
  kubectl delete nodepool e2e-noarm --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-noipv4 e2e-noarm --wait=true --timeout=120s
}

# settle_fixtures: the amd64 and arm64 nodes of the first scenarios (started if the scenario
# runs alone).
settle_fixtures() {
  [[ -n "$(getstate e2e-amd64.sid)" ]] || {
    start_amd64
    settle e2e-amd64
  }
}

# drift: a change to a profile no node uses drifts nothing; a change to the vendor block
# (networking.publicIPv6, which every node uses) replaces the node, and the replacement has an
# IPv6 address.
scn_drift() {
  start_drift
  settle e2e-drift
  local old_claim old_node old_sid old_pod
  old_claim="$(getstate e2e-drift.claim)"
  old_node="$(getstate e2e-drift.node)"
  old_sid="$(getstate e2e-drift.sid)"
  old_pod="$(getstate e2e-drift.pod)"
  assert_eq "no IPv6 address before the change" "" "$(server_json "${old_sid}" | jq -r '.public_net.ipv6.ip // empty')"

  log "a profile no running node uses changes: nothing drifts"
  kubectl patch scnodeclass e2e-drift --type merge -p \
    '{"spec":{"profiles":{"storage":{"scratch":{"hcloud":{"data":[{"name":"scratch","size":"20G","filesystem":"ext4","mountPath":"/var/lib/scratch"}]}}}}}}'
  wait_until "the NodeClass reconciled the change" 120 nodeclass_observed e2e-drift
  local i
  for i in 1 2 3 4 5 6; do
    sleep 10
    assert_eq "the claim is not drifted (${i}/6)" "" \
      "$(kubectl get nodeclaim "${old_claim}" -o jsonpath='{.status.conditions[?(@.type=="Drifted")].status}' | grep -x True || true)"
  done
  assert_eq "still the same claim" "${old_claim}" "$(claim_of_node "${old_node}")"

  log "networking.publicIPv6 changes: the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p '{"spec":{"vendors":{"hcloud":{"networking":{"publicIPv6":true}}}}}'
  wait_node "the Pod on another node" "${E2E_NODE_TIMEOUT}" app=e2e-drift "${old_claim}" pod_moved_from app=e2e-drift "${old_node}"
  local pod node claim sid j
  pod="$(kubectl get pod -l app=e2e-drift -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  sid="$(instance_of_claim "${claim}")"
  [[ "${sid}" != "${old_sid}" ]] || fail "the replacement runs on the old server"
  j="$(server_json "${sid}")"
  assert_nonempty "the replacement's IPv6 address" "$(jq -r '.public_net.ipv6.ip // empty' <<<"${j}")"
  assert_match "the node reports an IPv6 address" ':' \
    "$(kubectl get node "${node}" -o jsonpath='{range .status.addresses[*]}{.address}{"\n"}{end}' | paste -sd' ' -)"
  wait_until "the old server deleted" 300 is_empty servers_left "${old_sid}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  wait_until "no orphaned server" 240 no_orphans
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

# no_orphans: every server of the cluster belongs to a NodeClaim that exists.
no_orphans() {
  local claims ids id
  claims="$(kjson nodeclaims | jq -r --arg k "${label_domain}/instance-ref" '.items[] | .metadata.annotations[$k] // empty' | sed 's#.*/##')"
  ids="$(cluster_servers)"
  for id in ${ids}; do
    grep -qx "${id}" <<<"${claims}" || {
      echo "server ${id} has no NodeClaim"
      return 1
    }
  done
}

# release_do_not_disrupt: removes karpenter.sh/do-not-disrupt from every node of the lane
# (a node would never be consolidated otherwise). Idempotent.
release_do_not_disrupt() {
  local n
  for n in $(kubectl get nodes -l karpenter.sh/nodepool -o name 2>/dev/null); do
    kubectl annotate "${n}" karpenter.sh/do-not-disrupt- >/dev/null 2>&1 || true
  done
}

# scalezero: deleting the workloads deletes every server within E2E_SCALE_DOWN_DEADLINE seconds
# of the Pods' deletion (WhenEmpty, 0 s); the nodes, NodeClaims and bootstrap tokens go too, and
# so does every Primary IP Hetzner made with a server (the auto-delete fact of plan 15, open
# question 6: if one stays, primaryIPs must become a managed kind).
scn_scalezero() {
  local t0 elapsed pips app
  drop_nodeshells
  release_do_not_disrupt
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every server of the cluster deleted" "${E2E_SCALE_DOWN_DEADLINE}" is_empty cluster_servers
  elapsed=$(($(now) - t0))
  note "scale to zero: every server was gone ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s)"
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name
  pips=""
  for app in e2e-amd64 e2e-arm64 e2e-drift e2e-described e2e-use; do pips+=" $(getstate "${app}.pips")"; done
  note "Primary IPs of the servers of this run: ${pips:-none recorded}"
  # shellcheck disable=SC2086  # the IDs are separate words
  wait_until "no Primary IP of the servers left" 180 primary_ips_gone ${pips}
  wait_until "no Primary IP labelled with the cluster left" 60 is_empty cluster_leaks primary-ip
}

# primary_ips_gone <id>...: none of the Primary IPs exists.
primary_ips_gone() {
  local id
  for id in "$@"; do
    ! hc primary-ip describe "${id}" -o json >/dev/null 2>&1 || {
      echo "the Primary IP ${id} still exists"
      return 1
    }
  done
}

# final: the procedure of docs/how-to-guides/install.md#uninstall on the live project, and the last
# assertion of the plan: nothing labelled with the run remains.
scn_final() {
  log "1. SCarpenter's NodePools (selected by nodeClassRef.group), then wait for the NodeClaims"
  kjson nodepools | jq -r '.items[] | select(.spec.template.spec.nodeClassRef.group == "scarpenter.sparecores.com") | .metadata.name' |
    xargs -r kubectl delete nodepool --wait=true --timeout=300s
  wait_until "no NodeClaim of SCarpenter left" 300 is_value 0 scarpenter_claims
  log "2. the SCNodeClasses; the managed resources go by their lifecycle"
  kubectl delete scnodeclass --all --wait=true --timeout=300s
  wait_until "no server of the cluster" 300 is_empty cluster_servers
  # The brought network and firewall are the workflow's to delete (a `use` is never ours).
  local id
  for id in $(hc firewall list -l "scarpenter-e2e-byo=${E2E_CLUSTER}" -o noheader -o columns=id); do hc firewall delete "${id}"; done
  for id in $(hc network list -l "scarpenter-e2e-byo=${E2E_CLUSTER}" -o noheader -o columns=id); do hc network delete "${id}"; done
  # With the NodeClasses gone the managed firewalls fall under the default idle lifetime
  # (managedResources.defaultTTL, 5 minutes in the lane's values; 24 hours by default).
  wait_until "no firewall of the cluster left" 900 is_value 0 count_of firewall
  wait_until "no placement group left" 120 is_value 0 count_of placement-group
  wait_until "no volume left" 120 is_value 0 count_of volume
  log "3. helm uninstall"
  helm uninstall scarpenter --namespace "${ns}" --wait --timeout 3m
  wait_until "the controller gone" 120 is_empty kubectl -n "${ns}" get pods -l app.kubernetes.io/name=scarpenter -o name
  log "4. nothing labelled with the run remains"
  local kind
  for kind in server volume placement-group firewall network primary-ip; do
    assert_eq "${kind}s labelled ${E2E_CLUSTER}" "" "$(cluster_leaks "${kind}" | paste -sd' ' -)"
  done
}

# scarpenter_claims: how many NodeClaims belong to an SCNodeClass.
scarpenter_claims() {
  kjson nodeclaims | jq '[.items[] | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'
}

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 arm64 db described byo offerings negative drift scalezero final)
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

# The prelude: start the workloads of the selected scenarios together; the scenarios wait for
# them (their own start is idempotent and reports a real failure).
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
