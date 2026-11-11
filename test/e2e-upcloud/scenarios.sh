#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5: the scenarios, run against the cluster that bootstrap.sh and install.sh built
# (docs/plans/v1/wip/19-upcloud.md, "L5 lane and sweeper"; docs/how-to-guides/testing.md#the-l5-lane
# lists what each proves and how to read a failure).
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 described byo data servergroup negative quota publicfw drift scalezero uninstall
#                   (preflight always runs first, and ends the run when it fails)
#
# Each scenario is a function that runs in a subshell with `set -e`: the first failed
# assertion ends it, the runner records the result and goes on. Waits have explicit
# deadlines. Workloads of all selected scenarios are started together (the prelude), so
# that the nodes boot in parallel; a scenario then only waits and checks. The exit status
# is 1 when a scenario failed; with E2E_FAIL_FAST=true (the default) the scenarios after the
# first failure are reported as SKIP, so that the diagnostics and the teardown start at once.
# A failed scenario prints one redacted snapshot of the cluster (observe.sh).
# A run needs about 10 cores and 12 GB of the account's resource limits (preflight checks).
#
# Needs: kubectl, jq, helm, curl, envsubst, and UPCLOUD_TOKEN. Every UpCloud call is a curl
# call of lib.sh (up); nothing here prints the token.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-upcloud/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT UPCLOUD_TOKEN UPCLOUD_ZONE KUBECONFIG
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
  local app="$1" pod node claim iid
  wait_node "the Pod ${app} running" "${E2E_NODE_TIMEOUT}" "app=${app}" "" pod_running "app=${app}"
  pod="$(kubectl get pod -l "app=${app}" -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  assert_nonempty "the NodeClaim of ${node}" "${claim}"
  iid="$(instance_of_claim "${claim}")"
  assert_nonempty "the server of ${claim}" "${iid}"
  setstate "${app}.pod" "${pod}"
  setstate "${app}.node" "${node}"
  setstate "${app}.claim" "${claim}"
  setstate "${app}.uid" "$(kubectl get nodeclaim "${claim}" -o jsonpath='{.metadata.uid}')"
  setstate "${app}.iid" "${iid}"
  note "${app}: pod ${pod}, node ${node}, claim ${claim}, ready after $(ready_seconds "${claim}" "${node}")s"
}

# plan_of <instance type without the vendor prefix>: the cores and the memory in MB of a plan,
# from the account's plan list (GET /plan).
plan_json() { up GET /plan | jq -c --arg p "$1" '.plans.plan[] | select(.name == $p)'; }

# public_ipv4 <server json>
public_ipv4() {
  jq -r '[.networking.interfaces.interface[] | select(.type == "public") | .ip_addresses.ip_address[] | select(.family == "IPv4") | .address] | first // empty' <<<"$1"
}

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }
start_data() { render "${here}/manifests/data.yaml" | kubectl apply -f -; }
start_servergroup() { render "${here}/manifests/servergroup.yaml" | kubectl apply -f -; }

# start_byo: the SDN network and the ruleset the workflow creates through the API (labelled
# with the cluster, so that the sweeper reaps them if the run dies, and scarpenter-e2e-byo),
# the NodeClass that uses them by that label, and the workload.
start_byo() {
  local net rs body
  net="$(byo_networks | head -n1)"
  if [[ -z "${net}" ]]; then
    body="$(jq -nc --arg z "${UPCLOUD_ZONE}" --arg c "${E2E_CLUSTER}" '{network: {
      name: ("scarpenter-" + $c + "-byo"), zone: $z,
      ip_networks: {ip_network: [{address: "10.99.0.0/24", dhcp: "yes", family: "IPv4"}]},
      labels: [{key: "scarpenter-cluster", value: $c}, {key: "scarpenter-e2e-byo", value: $c}]}}')"
    net="$(up_json POST /network "${body}" | jq -r '.network.uuid')"
  fi
  rs="$(byo_rulesets | head -n1)"
  if [[ -z "${rs}" ]]; then
    body="$(jq -nc --arg c "${E2E_CLUSTER}" '{name: ("scarpenter-" + $c + "-byo"), stateful: true, enabled: true,
      labels: [{key: "scarpenter-cluster", value: $c}, {key: "scarpenter-e2e-byo", value: $c}]}')"
    rs="$(up_json POST /firewall-ruleset "${body}" | jq -r '.uuid')"
    up_json POST "/firewall-ruleset/${rs}/rule" '{"rules":[{"action":"accept","direction":"in","family":"IPv4","protocol":"icmp","comment":"icmp","enabled":true}]}' >/dev/null
  fi
  assert_nonempty "the brought network" "${net}"
  assert_nonempty "the brought ruleset" "${rs}"
  setstate byo.network "${net}"
  setstate byo.ruleset "${rs}"
  render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  for t in kubectl jq helm envsubst curl; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  # The token works (and is not expired: UpCloud tokens last at most a year).
  local acct
  acct="$(up GET /account)" || fail "the UpCloud API token does not work (expired? rotate UPCLOUD_TOKEN, docs/how-to-guides/testing-and-releasing.md)"
  note "the sub-account's resource limits: $(jq -c '.account.resource_limits' <<<"${acct}")"

  # 1. The Flatcar templates (plan 19, "Images"): private templates of the lane's zone,
  # online, labelled with the pinned release. The driver needs one; both tiers are reported.
  local tpls n tiers
  tpls="$(up GET "/storage/private?label=scarpenter-image=flatcar" | jq -c --arg z "${UPCLOUD_ZONE}" --arg v "${E2E_FLATCAR_VERSION}" --arg c "${E2E_FLATCAR_CHANNEL}" \
    '[.storages.storage[]? | select(.type == "template" and .zone == $z and .state == "online")
      | select((.labels.label // []) | (map(select(.key == "flatcar-version" and .value == $v)) | length > 0)
        and (map(select(.key == "flatcar-channel" and .value == $c)) | length > 0))]')"
  n="$(jq 'length' <<<"${tpls}")"
  [[ "${n}" -ge 1 ]] || fail "no online Flatcar template ${E2E_FLATCAR_CHANNEL} ${E2E_FLATCAR_VERSION} in ${UPCLOUD_ZONE}: import the image of the images repository (plan 22) as a template with the labels of test/e2e-upcloud/defaults.sh"
  tiers="$(jq -r '[.[].tier] | unique | join(",")' <<<"${tpls}")"
  note "Flatcar templates in ${UPCLOUD_ZONE}: ${n}, tiers ${tiers}"
  assert_match "a MaxIOPS template exists (the lane's data disks and default root need it)" maxiops "${tiers}"

  # 2. The limits the suite needs (set by the main account on the sub-account).
  local cores mem
  cores="$(jq -r '.account.resource_limits.cores // 0' <<<"${acct}")"
  mem="$(jq -r '.account.resource_limits.memory // 0' <<<"${acct}")"
  [[ "$(num "${cores}")" -ge "${E2E_MIN_CORES}" ]] || fail "the cores limit of the account is ${cores}; the suite needs ${E2E_MIN_CORES}"
  [[ "$(num "${mem}")" -ge "${E2E_MIN_MEMORY_MB}" ]] || fail "the memory limit of the account is ${mem} MB; the suite needs ${E2E_MIN_MEMORY_MB}"
  assert_nonempty "the plan ${E2E_TYPE_AMD64#upcloud.} in the account's plan list" "$(plan_json "${E2E_TYPE_AMD64#upcloud.}")"

  # 3. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc
  for nc in e2e-amd64 e2e-drift; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.upcloud.conditions[?(@.type=="VendorReady")].status}')"
    assert_eq "scnodeclass/${nc} ImagesReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.upcloud.conditions[?(@.type=="ImagesReady")].status}')"
  done
}

scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim iid j plan zone
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  iid="$(getstate e2e-amd64.iid)"
  plan="${E2E_TYPE_AMD64#upcloud.}"

  log "node Ready, labels (overview section 5.5)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "region (UpCloud's regions are its zones)" "${UPCLOUD_ZONE}" "$(node_label "${node}" topology.kubernetes.io/region)"
  assert_eq "zone (the dummy zone has the region's ID)" "${UPCLOUD_ZONE}" "$(node_label "${node}" topology.kubernetes.io/zone)"
  assert_eq "zone-id" "${UPCLOUD_ZONE}" "$(node_label "${node}" "${label_domain}/zone-id")"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" upcloud "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${plan}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_nonempty "family" "$(node_label "${node}" "${label_domain}/family")"
  assert_eq "storage-profile" default "$(node_label "${node}" "${label_domain}/storage-profile")"
  assert_eq "placement-profile" default "$(node_label "${node}" "${label_domain}/placement-profile")"
  assert_eq "nodepool" e2e-amd64 "$(node_label "${node}" karpenter.sh/nodepool)"

  log "the server: plan, zone, labels, metadata, firewall, interfaces, disks (plan 19, CreateInstance)"
  j="$(server_json "${iid}")"
  assert_eq "server state" started "$(jq -r '.state' <<<"${j}")"
  assert_eq "plan" "${plan}" "$(jq -r '.plan' <<<"${j}")"
  zone="$(jq -r '.zone' <<<"${j}")"
  assert_eq "zone" "${UPCLOUD_ZONE}" "${zone}"
  assert_eq "hostname is the NodeClaim name" "${claim}" "$(jq -r '.hostname' <<<"${j}")"
  assert_eq "title is the NodeClaim name" "${claim}" "$(jq -r '.title' <<<"${j}")"
  assert_eq "the metadata service (Ignition reads its user data there)" yes "$(jq -r '.metadata' <<<"${j}")"
  assert_eq "the Public and Utility firewall is on" on "$(jq -r '.firewall' <<<"${j}")"
  assert_eq "label cluster" "${E2E_CLUSTER}" "$(label_of "${j}" "${ctag}")"
  assert_eq "label nodeclaim" "${claim}" "$(label_of "${j}" scarpenter-nodeclaim)"
  assert_eq "label nodepool" e2e-amd64 "$(label_of "${j}" scarpenter-nodepool)"
  assert_eq "label nodeclass" e2e-amd64 "$(label_of "${j}" scarpenter-nodeclass)"
  assert_eq "label managed-by" scarpenter "$(label_of "${j}" scarpenter-managed-by)"
  assert_eq "label idempotency-key is the NodeClaim UID" "$(getstate e2e-amd64.uid)" "$(label_of "${j}" scarpenter-idempotency-key)"
  assert_match "label created (the billing start, RFC 3339)" '^[0-9]{4}-[0-9]{2}-[0-9]{2}T' "$(label_of "${j}" scarpenter-created)"
  assert_eq "label from the NodeClass's spec.tags" "${E2E_RUN_ID}" "$(label_of "${j}" ci-run)"
  assert_nonempty "public IPv4" "$(public_ipv4 "${j}")"
  assert_eq "a utility interface" 1 "$(jq '[.networking.interfaces.interface[] | select(.type == "utility")] | length' <<<"${j}")"
  assert_eq "no private interface without a networks resource" 0 "$(jq '[.networking.interfaces.interface[] | select(.type == "private")] | length' <<<"${j}")"
  # The size of the user data against the (unknown) limit of the API, for the reader (S3).
  note "amd64: the user data of the server is $(jq -r '(.user_data // "") | length' <<<"${j}") bytes as the API returns it (0: not returned)"
  local root rj
  root="$(jq -r '[.storage_devices.storage_device[] | select(.storage_title | endswith("-root"))][0].storage // empty' <<<"${j}")"
  assert_nonempty "the root disk" "${root}"
  rj="$(storage_json "${root}")"
  assert_eq "root disk size (GB)" 25 "$(jq -r '.size' <<<"${rj}")"
  assert_eq "root disk encrypted" yes "$(jq -r '.encrypted' <<<"${rj}")"
  assert_nonempty "root disk tier (the template's)" "$(jq -r '.tier' <<<"${rj}")"
  assert_eq "label cluster on the disk" "${E2E_CLUSTER}" "$(label_of "${rj}" "${ctag}")"
  assert_eq "label nodeclaim on the disk" "${claim}" "$(label_of "${rj}" scarpenter-nodeclaim)"

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

# node_unit_checks <node>: what L4 scenarios Q09 and Q10 check inside a VM, on an UpCloud
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

scn_described() {
  start_described
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-described "" pod_running app=e2e-described
  local pod node claim iid t vcpus mem pct sj
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
  sj="$(server_json "${iid}")"
  assert_eq "the server is of the chosen plan" "${t#upcloud.}" "$(jq -r '.plan' <<<"${sj}")"
  # The same facts from UpCloud, independent of the catalog.
  assert_eq "UpCloud's core number of the plan" "${vcpus}" "$(jq -r '.core_number' <<<"${sj}")"
}

scn_byo() {
  start_byo
  local net rs
  net="$(getstate byo.network)"
  rs="$(getstate byo.ruleset)"
  wait_until "ResourcesReady on the NodeClass" 180 is_value True \
    kubectl get scnodeclass e2e-use -o jsonpath='{.status.vendors.upcloud.conditions[?(@.type=="ResourcesReady")].status}'
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-use "" pod_running app=e2e-use
  local pod node iid j priv
  pod="$(kubectl get pod -l app=e2e-use -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  iid="$(instance_of_claim "$(claim_of_node "${node}")")"
  j="$(server_json "${iid}")"
  priv="$(jq -c '[.networking.interfaces.interface[] | select(.type == "private")] | first // {}' <<<"${j}")"
  assert_eq "the private interface is on the brought network" "${net}" "$(jq -r '.network // empty' <<<"${priv}")"
  assert_nonempty "the private address" "$(jq -r '.ip_addresses.ip_address[0].address // empty' <<<"${priv}")"
  assert_eq "the private firewall is on" on "$(jq -r '.firewall_private // empty' <<<"${j}")"
  # Whether the ruleset lists its servers is unverified (plan 19); the note is for the reader.
  note "byo: the ruleset ${rs} as the API shows it: $(up GET "/firewall-ruleset/${rs}" | jq -c '{name, servers: (.servers // .attached_servers // "not shown")}')"
  assert_nonempty "the public interface stays (the public firewall is the driver's)" "$(public_ipv4 "${j}")"

  log "SCarpenter must not touch what it did not create"
  drop_nodeshells
  kubectl delete deployment e2e-use --wait=false
  wait_until "the server deleted" 300 is_empty servers_existing "${iid}"
  kubectl delete nodepool e2e-use --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-use --wait=true --timeout=120s
  # Past a garbage collection (1 m), a resource check (30 s) and the status controller:
  # a `use` reference is never modified or deleted.
  sleep 120
  assert_eq "the network still exists" "${net}" "$(up GET "/network/${net}" | jq -r '.network.uuid')"
  assert_eq "the ruleset still exists" "${rs}" "$(up GET "/firewall-ruleset/${rs}" | jq -r '.uuid')"
  note "the brought network ${net} and ruleset ${rs} are deleted by the teardown"
}

scn_data() {
  start_data
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-data "" pod_running app=e2e-data
  local pod node claim iid j dev vol uuid src serial
  pod="$(kubectl get pod -l app=e2e-data -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  iid="$(instance_of_claim "${claim}")"
  assert_eq "node storage-profile" scratch "$(node_label "${node}" "${label_domain}/storage-profile")"
  j="$(server_json "${iid}")"
  assert_eq "two disks on the server (root and data)" 2 "$(jq '.storage_devices.storage_device | length' <<<"${j}")"
  dev="$(jq -c '[.storage_devices.storage_device[] | select(.storage_title | endswith("-scratch"))][0] // {}' <<<"${j}")"
  uuid="$(jq -r '.storage // empty' <<<"${dev}")"
  assert_nonempty "the data disk" "${uuid}"
  vol="$(storage_json "${uuid}")"
  assert_eq "data disk size (GB)" 10 "$(jq -r '.size' <<<"${vol}")"
  assert_eq "data disk tier" maxiops "$(jq -r '.tier' <<<"${vol}")"
  assert_eq "data disk encrypted" yes "$(jq -r '.encrypted' <<<"${vol}")"
  assert_eq "label cluster on the data disk" "${E2E_CLUSTER}" "$(label_of "${vol}" "${ctag}")"
  assert_eq "label nodeclaim on the data disk" "${claim}" "$(label_of "${vol}" scarpenter-nodeclaim)"
  log "mounted at the profile's path"
  src="$(node_shell "${node}" 'findmnt -no SOURCE /var/lib/scratch')"
  assert_nonempty "the device mounted at /var/lib/scratch" "${src}"
  assert_eq "the filesystem at /var/lib/scratch" ext4 "$(node_shell "${node}" 'findmnt -no FSTYPE /var/lib/scratch')"
  assert_eq "the Pod's file is on the mounted volume" hello-from-the-volume "$(node_shell "${node}" 'cat /var/lib/scratch/hello')"
  # The metadata service's serial of a disk is the first 20 hex digits of its UUID (plan 19:
  # unverified as the by-id name): a note, so that the first live run shows the truth.
  serial="$(node_shell "${node}" "lsblk -dno SERIAL ${src}" | tr -d ' ')"
  note "data: the serial of ${src} is '${serial}'; the first 20 hex digits of the disk's UUID are '$(tr -d - <<<"${uuid}" | cut -c1-20)'"

  log "delete the workload: the disk goes with the server"
  drop_nodeshells
  kubectl delete deployment e2e-data --wait=false
  wait_until "the server deleted" 300 is_empty servers_existing "${iid}"
  wait_until "the data disk gone" 120 is_empty storage_existing "${uuid}"
}

scn_servergroup() {
  start_servergroup
  wait_node "both Pods running" $((E2E_NODE_TIMEOUT + 300)) "app=e2e-sg" "" pod_running "app=e2e-sg"
  local pods nodes iids groups g gj m1 m2 i1 i2
  mapfile -t pods < <(kubectl get pods -l app=e2e-sg -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
  assert_eq "two Pods" 2 "${#pods[@]}"
  nodes=("$(pod_node "${pods[0]}")" "$(pod_node "${pods[1]}")")
  [[ "${nodes[0]}" != "${nodes[1]}" ]] || fail "both Pods share the node ${nodes[0]}"
  i1="$(instance_of_claim "$(claim_of_node "${nodes[0]}")")"
  i2="$(instance_of_claim "$(claim_of_node "${nodes[1]}")")"
  iids=("${i1}" "${i2}")

  log "one server group, named and labelled by SCarpenter"
  groups="$(cluster_groups)"
  assert_eq "server groups of the cluster" 1 "$(wc -l <<<"${groups}" | tr -d ' ')"
  g="${groups}"
  gj="$(up GET "/server-group/${g}")"
  gj="$(jq -c 'if has("server_group") then .server_group else . end' <<<"${gj}")"
  assert_match "the group's name" "^scarpenter-" "$(jq -r '.title' <<<"${gj}")"
  assert_eq "label managed-by" scarpenter "$(label_of "${gj}" scarpenter-managed-by)"
  assert_eq "label resource-kind" serverGroups "$(label_of "${gj}" scarpenter-resource-kind)"
  assert_eq "label nodeclass" e2e-amd64 "$(label_of "${gj}" scarpenter-nodeclass)"
  assert_eq "label cluster" "${E2E_CLUSTER}" "$(label_of "${gj}" "${ctag}")"
  assert_eq "policy" strict "$(jq -r '.anti_affinity // .anti_affinity_policy' <<<"${gj}")"
  m1="$(jq -r --arg u "${iids[0]}" '[(.servers // .members // [])[] | select(. == $u)] | length' <<<"${gj}")"
  m2="$(jq -r --arg u "${iids[1]}" '[(.servers // .members // [])[] | select(. == $u)] | length' <<<"${gj}")"
  assert_eq "the first server is a member" 1 "${m1}"
  assert_eq "the second server is a member" 1 "${m2}"
  assert_eq "every member's anti-affinity is met (different hosts)" 0 \
    "$(jq '[(.anti_affinity_status // [])[] | select(.status != "met")] | length' <<<"${gj}")"
  assert_eq "node placement-profile" spread "$(node_label "${nodes[0]}" "${label_domain}/placement-profile")"
  assert_eq "run key" "sg-${E2E_RUN_ID}" "$(node_label "${nodes[0]}" "${label_domain}/placement-group")"

  log "delete the workload: the group goes (perUse)"
  drop_nodeshells
  kubectl delete deployment e2e-sg --wait=false
  wait_until "the servers deleted" 300 is_empty servers_existing "${iids[0]}" "${iids[1]}"
  # perUse: two minutes of grace for launches in flight, then the next resource check and
  # garbage collection (30 s and 1 m here).
  wait_until "the server group deleted" 420 is_empty cluster_groups
}

# negative: NodeClasses the driver refuses; nothing is launched.
scn_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
  wait_until "publicIPv4: false without a network is refused" 180 is_value False \
    kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.upcloud.conditions[?(@.type=="VendorReady")].status}'
  assert_match "the message names the fix" "(networks|private|publicIPv4)" \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.upcloud.conditions[?(@.type=="VendorReady")].message}')"
  wait_until "a label key over 32 characters is refused" 180 is_value False \
    kubectl get scnodeclass e2e-badlabel -o jsonpath='{.status.vendors.upcloud.conditions[?(@.type=="VendorReady")].status}'
  assert_eq "the reason" ValidationFailed \
    "$(kubectl get scnodeclass e2e-badlabel -o jsonpath='{.status.vendors.upcloud.conditions[?(@.type=="VendorReady")].reason}')"
  kubectl delete scnodeclass e2e-noipv4 e2e-badlabel --wait=true --timeout=120s
}

# quota (overview section 5.9, plan 19 scenario 4): what the controller reports equals what
# GET /account says, and usage moves with the servers. All limits are account-wide.
scn_quota() {
  settle_fixtures
  local m b lim cli
  m="$(mktemp)"
  log "the limits equal GET /account resource_limits"
  local timeout=240
  for b in cores memory; do
    cli="$(up GET /account | jq -r --arg b "${b}" '.account.resource_limits[$b] // 0')"
    if [[ "$(num "${cli}")" -le 0 ]]; then
      note "quota ${b}: the account reports no positive limit (0 or absent means unknown, no bucket is reported)"
      continue
    fi
    wait_until "scarpenter_quota_limit ${b} = ${cli}" "${timeout}" quota_limit_is "${m}" "${b}" "${cli}"
    timeout=90 # the first one waited for the controller's first limits refresh
  done

  log "usage: what the controller counts is what the account's server list says"
  wait_until "scarpenter_quota_used of cores equals the servers' cores" 300 quota_used_is "${m}" cores
  wait_until "scarpenter_quota_used of memory equals the servers' memory" 300 quota_used_is "${m}" memory
  lim="$(metric_value "${m}" scarpenter_quota_used cores)"
  [[ "$(num "${lim}")" -ge 1 ]] || fail "the cores usage ${lim} does not include the launched server"
  wait_until "the in-flight reservations are gone (cores)" 240 quota_reserved_zero "${m}" cores
  wait_until "the in-flight reservations are gone (memory)" 240 quota_reserved_zero "${m}" memory
  rm -f "${m}"
}

quota_limit_is() { # quota_limit_is <metrics file> <bucket> <limit from the API>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_limit "$2")")" == "$(num "$3")" ]]
}

# expected_usage cores|memory: the sum over every server of the account (the driver counts
# every state; which states UpCloud counts is unverified, so a server being stopped is the
# one case where the two could differ for a moment).
expected_usage() {
  local field=core_number
  [[ "$1" == memory ]] && field=memory_amount
  up GET /server | jq -r --arg f "${field}" '[.servers.server[]? | .[$f] | tonumber] | add // 0'
}

quota_used_is() { # quota_used_is <metrics file> cores|memory
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_used "$2")")" == "$(num "$(expected_usage "$2")")" ]]
}

quota_reserved_zero() { # quota_reserved_zero <metrics file> <bucket>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_reserved "$2")")" == 0 ]]
}

# settle_fixtures: the amd64 node of the first scenario (started if the scenario runs alone).
settle_fixtures() {
  [[ -n "$(getstate e2e-amd64.iid)" ]] || {
    start_amd64
    settle e2e-amd64
  }
}

# tcp_open <host> <port>: a TCP connection is accepted within 5 seconds.
tcp_open() { timeout 5 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; }

# fw_norm: the comparable form of a firewall rule list (the API answers
# {"firewall_rules":{"firewall_rule":[...]}}): the fields the driver sets, in order.
fw_norm() {
  jq -c '[(.firewall_rules.firewall_rule // [])[] | {position: (.position | tostring), action, direction, family: (.family // ""),
    protocol: (.protocol // ""), dps: (.destination_port_start // ""), dpe: (.destination_port_end // ""),
    sps: (.source_port_start // ""), spe: (.source_port_end // ""), comment: (.comment // "")}]'
}

# publicfw (plan 19 scenario 4a): the node's public ports do not answer from the runner,
# which is outside the tailnet's node network, while the node stays Ready over Tailscale; the
# rules on the server are the driver's list.
scn_publicfw() {
  settle_fixtures
  local node iid j ip want got p created t0 first_closed="" now_s
  node="$(getstate e2e-amd64.node)"
  iid="$(getstate e2e-amd64.iid)"
  j="$(server_json "${iid}")"
  ip="$(public_ipv4 "${j}")"
  assert_nonempty "public IPv4" "${ip}"

  log "the rules on the server equal the driver's list"
  want="$(cd "${e2e_root}" && go run ./test/e2e-upcloud/fwrules | fw_norm)"
  got="$(up GET "/server/${iid}/firewall_rule" | fw_norm)"
  assert_eq "the number of rules" "$(jq 'length' <<<"${want}")" "$(jq 'length' <<<"${got}")"
  assert_eq "the rules, in order" "${want}" "${got}"
  assert_eq "the last inbound rule is the default drop" drop "$(jq -r '[.[] | select(.direction == "in")] | last | .action' <<<"${got}")"

  log "the public ports do not answer, the node stays Ready over Tailscale"
  # The rollout takes 1 to 2 minutes (UpCloud's documentation, unverified): the window is the
  # time from the server's creation label to the first moment the probe of port 22 fails.
  created="$(date -d "$(label_of "${j}" scarpenter-created)" +%s)"
  t0="$(now)"
  for _ in $(seq 1 60); do
    if ! tcp_open "${ip}" 22; then
      first_closed="$(now)"
      break
    fi
    sleep 5
  done
  now_s="$(now)"
  if [[ -n "${first_closed}" ]]; then
    note "publicfw: port 22 was closed $((first_closed - created))s after the server's creation label (the probes started $((t0 - created))s after it)"
  else
    note "publicfw: port 22 still answered $((now_s - created))s after the server's creation label (the probes stopped)"
  fi
  for p in 22 10250; do
    ! tcp_open "${ip}" "${p}" || fail "the public TCP port ${p} of ${ip} answers from the runner"
    echo "ok: the public TCP port ${p} does not answer"
  done
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  wait_until "kubectl logs still works over the tailnet" 60 kubectl logs "$(getstate e2e-amd64.pod)" --tail=1
}

# drift (plan 10 scenario 4 on UpCloud) and the opportunistic controller restart: a change to
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
    '{"spec":{"profiles":{"storage":{"scratch":{"upcloud":{"data":[{"name":"scratch","tier":"maxiops","size":"12Gi","filesystem":"ext4","mountPath":"/var/lib/scratch"}]}}}}}}'
  wait_until "the NodeClass reconciled the change" 120 nodeclass_observed e2e-drift
  local i
  for i in 1 2 3 4 5 6; do
    sleep 10
    assert_eq "the claim is not drifted (${i}/6)" "" \
      "$(kubectl get nodeclaim "${old_claim}" -o jsonpath='{.status.conditions[?(@.type=="Drifted")].status}' | grep -x True || true)"
  done
  assert_eq "still the same claim" "${old_claim}" "$(claim_of_node "${old_node}")"

  log "the root disk size changes: the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p '{"spec":{"vendors":{"upcloud":{"storage":{"root":{"size":"30Gi"}}}}}}'
  restart_during_create e2e-drift "${old_claim}"
  wait_node "the Pod on another node" "${E2E_NODE_TIMEOUT}" app=e2e-drift "${old_claim}" pod_moved_from app=e2e-drift "${old_node}"
  release_do_not_disrupt
  local pod node claim iid sj
  pod="$(kubectl get pod -l app=e2e-drift -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  iid="$(instance_of_claim "${claim}")"
  [[ "${iid}" != "${old_iid}" ]] || fail "the replacement runs on the old server"
  sj="$(server_json "${iid}")"
  assert_eq "the replacement's root disk (the new size)" 30 \
    "$(jq -r '[.storage_devices.storage_device[] | select(.storage_title | endswith("-root"))][0].storage_size' <<<"${sj}")"
  wait_until "the old server deleted" 300 is_empty servers_existing "${old_iid}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  assert_no_orphans
  # The label lookup is the idempotency of the create: a server ever made for the replacement
  # claim is found by its nodeclaim label, and there must be one (a deleted server is gone
  # from the list, so this sees the live one only; a second live one would be a replay that
  # did not find the first).
  local repl ever
  repl="$(getstate drift.replacement)"
  ever="$(servers_json "scarpenter-nodeclaim=${repl}" | jq 'length')"
  [[ "${ever}" -le 1 ]] || fail "the claim ${repl} owns ${ever} servers (a second create after the restart)"
  setstate e2e-drift.node "${node}"
  setstate e2e-drift.claim "${claim}"
  setstate e2e-drift.iid "${iid}"
}

# release_do_not_disrupt: removes karpenter.sh/do-not-disrupt from the drift
# replacement's NodeClaim and from every node of the lane (core copies a NodeClaim's
# annotations to its Node at registration). Idempotent.
release_do_not_disrupt() {
  local repl n
  repl="$(getstate drift.replacement)"
  [[ -z "${repl}" ]] || kubectl annotate nodeclaim "${repl}" karpenter.sh/do-not-disrupt- >/dev/null 2>&1 || true
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
# kills the controller Pod hard; the Deployment starts a new one. Create is idempotent (the
# server carries the NodeClaim UID in its label scarpenter-idempotency-key and the driver
# looks it up before every create): after the restart the claim must own at most one
# server, and no server may be orphaned. The window is narrow, so the replay assertions
# are opportunistic, as on AWS: a missed window is noted, never a failure.
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
  before="$(servers_of_claim "${new}")"
  if [[ "${window}" == yes ]]; then
    note "restart: the controller was killed while ${new} was not launched; servers of the claim at that moment: '${before}'"
  else
    note "restart (opportunistic): ${new} was already launched when the controller was killed; the replay of Create was not exercised in this attempt"
  fi
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=180s
  note "restart: CatalogNotReady lines in the new controller's log: $(kubectl -n "${ns}" logs deploy/scarpenter 2>/dev/null | grep -c CatalogNotReady || true)"
  wait_until "the new controller reconciled the claim (launched, or gone)" 300 claim_settled "${new}"
  launched="$(kubectl get nodeclaim "${new}" -o jsonpath='{.status.conditions[?(@.type=="Launched")].status}' --ignore-not-found)"
  if [[ "${launched}" != True ]]; then
    note "restart (opportunistic): the claim ${new} is gone after the restart (consolidated or replaced); only the no-orphan check applies"
    return 0
  fi
  after="$(servers_of_claim "${new}")"
  [[ "$(wc -w <<<"${after}" | tr -d ' ')" -le 1 ]] || fail "the claim ${new} owns several servers after the restart: ${after}"
  assert_eq "the claim's server is the one in its annotation" "$(instance_of_claim "${new}")" "${after}"
  if [[ "${window}" == yes && -n "${before}" ]]; then
    assert_eq "the replay of Create found the first server (no second create)" "${before}" "${after}"
    note "restart: the replay of Create was exercised (server ${after})"
  elif [[ "${window}" == yes ]]; then
    note "restart (opportunistic): the server did not exist yet when the controller was killed; Create ran from the start"
  fi
}

# claim_settled <claim>: launched, or no longer there.
claim_settled() {
  local out
  out="$(kubectl get nodeclaim "$1" -o jsonpath='{.status.conditions[?(@.type=="Launched")].status}' --ignore-not-found)" || return 1
  [[ "${out}" == True ]] || [[ -z "$(kubectl get nodeclaim "$1" -o name --ignore-not-found)" ]]
}

# servers_of_claim <claim>: the servers labelled with the claim, one line.
servers_of_claim() {
  servers_json "scarpenter-nodeclaim=$1" | jq -r '[.[].uuid] | join(" ")'
}

# assert_no_orphans: every server of the cluster belongs to a NodeClaim that exists.
assert_no_orphans() {
  wait_until "no orphaned server" 240 no_orphans
}

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

# scalezero (the roadmap's exit criterion, plan 19 scenario 2): deleting the workloads stops
# and deletes every server, and its disks, within E2E_SCALE_DOWN_DEADLINE seconds of the
# Pods' deletion (WhenEmpty, 0 s); the nodes, NodeClaims and bootstrap tokens go too; the
# quota usage falls with the servers.
scn_scalezero() {
  local t0 elapsed
  drop_nodeshells
  release_do_not_disrupt
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every server stopped or gone" "${E2E_SCALE_DOWN_DEADLINE}" is_empty cluster_servers started,maintenance
  note "scale to zero: every server was stopped or gone $(($(now) - t0))s after the Pods' deletion"
  # The deadline counts from the Pods' deletion for both.
  wait_until "every server of the cluster deleted" "$((E2E_SCALE_DOWN_DEADLINE > $(now) - t0 ? E2E_SCALE_DOWN_DEADLINE - ($(now) - t0) : 1))" is_empty cluster_servers
  elapsed=$(($(now) - t0))
  note "scale to zero: every server was deleted ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s); this is the driver's DeleteLatency to compare with plan 19's billing margin"
  wait_until "no disk of the cluster left (deleted with the servers)" 120 is_empty cluster_storages
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name
  log "the quota usage follows the servers down"
  local m
  m="$(mktemp)"
  wait_until "the cores usage equals the account's servers" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" cores
  wait_until "the memory usage equals the account's servers" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_used_is "${m}" memory
  wait_until "no reservation is left" 120 quota_reserved_zero "${m}" cores
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
  wait_until "no server of the cluster" 300 is_empty cluster_servers
  # With the NodeClasses gone the managed server groups fall under the default idle
  # lifetime (managedResources.defaultTTL, 5 minutes in the lane's values; 24 hours by default).
  wait_until "no managed resource of SCarpenter left" 900 is_empty managed_resources
  log "3. helm uninstall"
  helm uninstall scarpenter --namespace "${ns}" --wait --timeout 3m
  wait_until "the controller gone" 120 is_empty kubectl -n "${ns}" get pods -l app.kubernetes.io/name=scarpenter -o name
}

# scarpenter_claims: how many NodeClaims belong to an SCNodeClass.
scarpenter_claims() {
  kjson nodeclaims | jq '[.items[] | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'
}

# managed_resources: the server groups, rulesets and networks of the cluster (the brought
# ones are deleted by the teardown and are not SCarpenter's to remove; they carry no managed-by).
managed_resources() {
  local id
  for id in $(cluster_groups); do echo "serverGroup ${id}"; done
  for id in $(cluster_rulesets); do
    [[ -n "$(byo_rulesets | grep -x "${id}" || true)" ]] || echo "ruleset ${id}"
  done
  for id in $(cluster_networks); do
    [[ -n "$(byo_networks | grep -x "${id}" || true)" ]] || echo "network ${id}"
  done
}

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 described byo data servergroup negative quota publicfw drift scalezero uninstall)
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
