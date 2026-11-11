#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5: the scenarios, run against the cluster that bootstrap.sh and install.sh built
# (docs/plans/v1/wip/20-vultr.md section 9; docs/how-to-guides/testing.md#the-l5-lane lists
# what each proves and how to read a failure).
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 described byo fw vpc negative drift scalezero uninstall
#                   (preflight always runs first, and ends the run when it fails)
#
# Each scenario is a function that runs in a subshell with `set -e`: the first failed
# assertion ends it, the runner records the result and goes on. Waits have explicit
# deadlines. Workloads of all selected scenarios are started together (the prelude), so that
# the nodes boot in parallel; a scenario then only waits and checks. The exit status is 1
# when a scenario failed; with E2E_FAIL_FAST=true (the default) the scenarios after the first
# failure are reported as SKIP and not run. There is no arm64, spot, placement, storage or
# quota scenario: Vultr has none of them in v1 (negative checks the refusals). A run launches
# at most six small instances at once; the account's own limits are the cost ceiling.
#
# Needs: kubectl, jq, helm, curl, envsubst, and VULTR_API_KEY.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-vultr/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT VULTR_REGION VULTR_API_KEY KUBECONFIG
mkdir -p "${E2E_OUT}/state" "${E2E_OUT}/logs"
state="${E2E_OUT}/state"

# ------------------------------------------------------------- state --------

setstate() { printf '%s' "$2" >"${state}/$1"; } # setstate <name> <value>
getstate() { cat "${state}/$1" 2>/dev/null || true; }

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

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }

start_fw() {
  render "${here}/manifests/nodeclass-fw.yaml" | kubectl apply -f -
  probe e2e-fw e2e-fw "${E2E_TYPE_AMD64}" on-demand
}

start_vpc() {
  render "${here}/manifests/nodeclass-vpc.yaml" | kubectl apply -f -
  probe e2e-vpc e2e-vpc "${E2E_TYPE_AMD64}" on-demand
}

# byo_ids: the IDs of the objects the scenario brings, by their description "<cluster>-byo".
byo_group() { groups_json | jq -r --arg d "${E2E_CLUSTER}-byo" '[.[] | select(.description == $d)] | first | .id // empty'; }
byo_vpc() {
  vpcs_json | jq -r --arg d "${E2E_CLUSTER}-byo" --arg r "${VULTR_REGION}" \
    '[.[] | select(.description == $d and .region == $r)] | first | .id // empty'
}

# start_byo: the firewall group and the VPC the scenario creates through the API, described
# "<cluster>-byo" (the sweeper and the teardown find them by it), the NodeClass that uses
# them, and the workload.
start_byo() {
  local g v
  g="$(byo_group)"
  if [[ -z "${g}" ]]; then
    g="$(vapi_post /firewalls "$(jq -n --arg d "${E2E_CLUSTER}-byo" '{description: $d}')" | jq -r '.firewall_group.id')"
    vapi_post "/firewalls/${g}/rules" '{"ip_type":"v4","protocol":"tcp","subnet":"192.0.2.0","subnet_size":24,"port":"22"}' >/dev/null
  fi
  v="$(byo_vpc)"
  if [[ -z "${v}" ]]; then
    v="$(vapi_post /vpcs "$(jq -n --arg d "${E2E_CLUSTER}-byo" --arg r "${VULTR_REGION}" '{region: $r, description: $d}')" | jq -r '.vpc.id')"
  fi
  assert_nonempty "the brought firewall group" "${g}"
  assert_nonempty "the brought VPC" "${v}"
  setstate byo.group "${g}"
  setstate byo.vpc "${v}"
  E2E_BYO_GROUP_ID="${g}" render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

start_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  for t in kubectl jq helm envsubst curl; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  # The key and its allowlist: the call the driver's VendorReady makes.
  vapi '/instances?per_page=1' >/dev/null || fail "the Vultr API key does not work (the key, its ACL, or the runner's address is not in its IP allowlist)"

  # 1. The Flatcar snapshot: exactly one complete snapshot with the pinned description.
  local desc n
  desc="flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-amd64"
  n="$(vapi '/snapshots?per_page=500' | jq --arg d "${desc}" '[.snapshots[] | select(.description == $d and .status == "complete")] | length')"
  assert_eq "complete snapshots described ${desc}" 1 "${n}"

  # 2. The region exists and the lane's plans are in stock in it.
  local avail p
  avail="$(vapi "/regions/${VULTR_REGION}/availability")"
  for p in "${E2E_TYPE_AMD64#vultr.}" "${E2E_TYPE_SECOND#vultr.}"; do
    jq -e --arg p "${p}" '.available_plans | index($p) != null' <<<"${avail}" >/dev/null || fail "the plan ${p} is not in stock in ${VULTR_REGION}"
    echo "ok: ${p} is in stock in ${VULTR_REGION}"
  done

  # 3. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc
  for nc in e2e-amd64 e2e-drift; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.vultr.conditions[?(@.type=="VendorReady")].status}')"
    assert_eq "scnodeclass/${nc} ImagesReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.vultr.conditions[?(@.type=="ImagesReady")].status}')"
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

# node_unit_checks <node>: the tailscale sysext merged, tailscaled upheld, the kubelet ordered
# after and bound to the join unit and started with the tailnet address, through the node shell.
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

# group_of <instance json>: the firewall group the instance carries, as JSON.
group_of() { vapi "/firewalls/$(jq -r '.firewall_group_id' <<<"$1")" | jq '.firewall_group'; }

scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim iid j g
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  iid="$(getstate e2e-amd64.iid)"

  log "node Ready and its labels: the zone is the region"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "region" "${VULTR_REGION}" "$(node_label "${node}" topology.kubernetes.io/region)"
  assert_eq "zone (Vultr has none: the region)" "${VULTR_REGION}" "$(node_label "${node}" topology.kubernetes.io/zone)"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" vultr "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${E2E_TYPE_AMD64#vultr.}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_eq "nodepool" e2e-amd64 "$(node_label "${node}" karpenter.sh/nodepool)"
  assert_match "the provider ID" "^scarpenter://vultr/${VULTR_REGION}/${claim}\$" "$(kubectl get node "${node}" -o jsonpath='{.spec.providerID}')"

  log "the instance: state, plan, tags, firewall group"
  j="$(instance_json "${iid}")"
  assert_eq "instance status" active "$(jq -r '.status' <<<"${j}")"
  assert_eq "power status" running "$(jq -r '.power_status' <<<"${j}")"
  assert_eq "plan" "${E2E_TYPE_AMD64#vultr.}" "$(jq -r '.plan' <<<"${j}")"
  assert_eq "region" "${VULTR_REGION}" "$(jq -r '.region' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" "${ctag}")"
  assert_eq "tag nodeclaim" "${claim}" "$(tag_of "${j}" "${label_domain}/nodeclaim")"
  assert_eq "tag nodepool" e2e-amd64 "$(tag_of "${j}" "${label_domain}/nodepool")"
  assert_eq "tag nodeclass" e2e-amd64 "$(tag_of "${j}" "${label_domain}/nodeclass")"
  assert_eq "tag managed-by" scarpenter "$(tag_of "${j}" "${label_domain}/managed-by")"
  assert_eq "tag idempotency-key (the NodeClaim UID)" "$(getstate e2e-amd64.uid)" "$(tag_of "${j}" "${label_domain}/idempotency-key")"
  assert_eq "tag ci-created-at (the NodeClass's spec.tags)" "${E2E_CREATED_AT}" "$(tag_of "${j}" ci-created-at)"
  assert_nonempty "public IPv4" "$(jq -r 'select(.main_ip != "0.0.0.0") | .main_ip' <<<"${j}")"
  assert_eq "no VPC with isolation none" "" "$(jq -r '.internal_ip // ""' <<<"${j}")"
  g="$(group_of "${j}")"
  assert_nonempty "the firewall group of the instance" "$(jq -r '.id' <<<"${g}")"
  assert_match "the group is SCarpenter's (the marker in its description)" \
    "cluster=${E2E_CLUSTER} nodeclass=e2e-amd64 managed-by=scarpenter resource-kind=firewallGroups region=${VULTR_REGION}\$" "$(jq -r '.description' <<<"${g}")"
  assert_eq "a deny-all group has no rule" 0 "$(jq -r '.rule_count' <<<"${g}")"
  setstate e2e-amd64.group "$(jq -r '.id' <<<"${g}")"

  log "the claim: price, billing start, bootstrap token, serving certificate"
  local price
  price="$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/price" '.metadata.annotations[$k] // empty')"
  assert_nonempty "price annotation" "${price}"
  awk -v p="${price}" 'BEGIN { exit !(p + 0 > 0) }' || fail "the price ${price} is not positive"
  assert_nonempty "billing-start annotation (the instance's date_created)" \
    "$(kubectl get nodeclaim "${claim}" -o json | jq -r --arg k "${label_domain}/billing-start" '.metadata.annotations[$k] // empty')"
  wait_until "the bootstrap token Secret gone" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim=${claim}" -o name
  wait_until "the serving CSR approved and issued" 120 csr_approved "${node}"

  log "kubectl logs and exec (the API server reaches the kubelet over the tailnet)"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_match "the log of the probe" scarpenter-e2e-up "$(kubectl logs "${pod}")"
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"

  log "on the node: the units, the tailnet address and the kubelet"
  node_unit_checks "${node}"
  local dns
  dns="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  assert_match "the kubelet's clusterDNS is the kube-dns Service" "${dns//./\\.}" "$(node_shell "${node}" "grep -A3 '^clusterDNS' /var/lib/kubelet/config.yaml" | tr '\n' ' ')"
  wait_until "a Pod resolves a cluster Service" 120 kubectl exec "${pod}" -- nslookup kubernetes.default.svc.cluster.local
  drop_nodeshells
}

scn_described() {
  start_described
  wait_node "the described Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-described "" pod_running app=e2e-described
  local pod node
  pod="$(kubectl get pod -l app=e2e-described -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  assert_eq "the plan with 2 vCPUs and more than 3000 MiB" vultr.vc2-2c-4gb "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "vcpus label" 2 "$(node_label "${node}" "${label_domain}/vcpus")"
}

scn_byo() {
  start_byo
  settle e2e-use
  local iid j g v claim
  iid="$(getstate e2e-use.iid)"
  claim="$(getstate e2e-use.claim)"
  g="$(getstate byo.group)"
  v="$(getstate byo.vpc)"
  j="$(instance_json "${iid}")"
  assert_eq "the instance carries the brought firewall group" "${g}" "$(jq -r '.firewall_group_id' <<<"${j}")"
  assert_nonempty "the instance is in the brought VPC (an internal address)" "$(jq -r '.internal_ip // empty' <<<"${j}")"
  assert_eq "the brought VPC has the instance attached" "${iid}" \
    "$(vapi "/vpcs/${v}/attachments" | jq -r --arg i "${iid}" '[.vpc_attachments[]? | select((.linked_subscription.id // .id) == $i)] | first | (.linked_subscription.id // .id) // empty')"
  assert_eq "no managed firewall group was made for e2e-use" 0 "$(managed_of groups e2e-use | jq 'length')"
  assert_eq "no managed VPC was made for e2e-use" 0 "$(managed_of vpcs e2e-use | jq 'length')"
  assert_eq "status.resources shows the group as brought" use \
    "$(kubectl get scnodeclass e2e-use -o json | jq -r '[.status.vendors.vultr.resources[]? | select(.kind == "firewallGroups")] | first | .source // empty')"
  # The brought objects are never deleted by SCarpenter: scalezero checks they are still there.
  note "byo: group ${g}, VPC ${v}, claim ${claim}"
}

scn_fw() {
  start_fw
  settle e2e-fw
  local iid j g gid
  iid="$(getstate e2e-fw.iid)"
  j="$(instance_json "${iid}")"
  gid="$(jq -r '.firewall_group_id' <<<"${j}")"
  g="$(vapi "/firewalls/${gid}" | jq '.firewall_group')"
  assert_match "the managed group's description" \
    "^scarpenter-.* \\| cluster=${E2E_CLUSTER} nodeclass=e2e-fw managed-by=scarpenter resource-kind=firewallGroups region=${VULTR_REGION}\$" "$(jq -r '.description' <<<"${g}")"
  assert_eq "the group has the one rule of spec.networking.inbound" 1 "$(jq -r '.rule_count' <<<"${g}")"
  local rule
  rule="$(vapi "/firewalls/${gid}/rules" | jq -c '.firewall_rules[0] | {ip_type, protocol, port, subnet, subnet_size}')"
  assert_eq "the rule" '{"ip_type":"v4","protocol":"tcp","port":"22","subnet":"192.0.2.0","subnet_size":24}' "${rule}"
  setstate e2e-fw.group "${gid}"

  log "perUse: the group goes after the last node"
  kubectl delete deployment e2e-fw --wait=false
  wait_until "the instance gone" "${E2E_SCALE_DOWN_DEADLINE}" is_empty instances_of_claim "$(getstate e2e-fw.claim)"
  wait_until "the managed group of e2e-fw deleted" 600 is_value 0 managed_count groups e2e-fw
}

# managed_count <groups|vpcs> <nodeclass>
managed_count() { managed_of "$1" "$2" | jq 'length'; }

scn_vpc() {
  start_vpc
  settle e2e-vpc
  local iid j v
  iid="$(getstate e2e-vpc.iid)"
  j="$(instance_json "${iid}")"
  v="$(managed_of vpcs e2e-vpc)"
  assert_eq "one managed VPC for the NodeClass" 1 "$(jq 'length' <<<"${v}")"
  assert_eq "the VPC is in the region" "${VULTR_REGION}" "$(jq -r '.[0].region' <<<"${v}")"
  assert_match "the VPC's description carries the marker" "resource-kind=networks region=${VULTR_REGION}\$" "$(jq -r '.[0].description' <<<"${v}")"
  assert_nonempty "the instance has an address in the VPC" "$(jq -r '.internal_ip // empty' <<<"${j}")"
  assert_nonempty "the VPC's range" "$(jq -r '.[0].v4_subnet // empty' <<<"${v}")"
  setstate e2e-vpc.vpc "$(jq -r '.[0].id' <<<"${v}")"
}

# What Vultr cannot do is refused, never silently degraded (ADR-017).
scn_negative() {
  start_negative
  wait_until "e2e-noipv4 reconciled" 120 nodeclass_observed e2e-noipv4
  assert_eq "e2e-noipv4 VendorReady" False \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.vultr.conditions[?(@.type=="VendorReady")].status}')"
  assert_match "the reason names publicIPv4" 'publicIPv4' \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.vultr.conditions[?(@.type=="VendorReady")].message}')"

  # A Pod that needs arm64 on Vultr, and one that selects a storage profile Vultr cannot
  # honour, stay Pending and no NodeClaim is made for them.
  kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata: {name: e2e-arm64, labels: {scarpenter-e2e: "true", app: e2e-arm64}}
spec:
  terminationGracePeriodSeconds: 0
  nodeSelector: {karpenter.sh/nodepool: e2e-arm64, kubernetes.io/arch: arm64}
  containers:
    - {name: probe, image: public.ecr.aws/docker/library/busybox:1.37.0, command: [sh, -c, "exec sleep 86400"], resources: {requests: {cpu: 100m, memory: 64Mi}}}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e-profile, labels: {scarpenter-e2e: "true", app: e2e-profile}}
spec:
  terminationGracePeriodSeconds: 0
  nodeSelector:
    karpenter.sh/nodepool: e2e-profile
    scarpenter.sparecores.com/storage-profile: data
  containers:
    - {name: probe, image: public.ecr.aws/docker/library/busybox:1.37.0, command: [sh, -c, "exec sleep 86400"], resources: {requests: {cpu: 100m, memory: 64Mi}}}
YAML
  sleep 90
  local p
  for p in e2e-arm64 e2e-profile; do
    assert_eq "the Pod ${p} is Pending" Pending "$(kubectl get pod "${p}" -o jsonpath='{.status.phase}')"
    assert_eq "no NodeClaim for the pool ${p}" "" "$(kubectl get nodeclaims -l "karpenter.sh/nodepool=${p}" -o name)"
  done
  kubectl delete pod e2e-arm64 e2e-profile --wait=false --ignore-not-found >/dev/null
}

scn_drift() {
  start_drift
  settle e2e-drift
  local old_claim old_node old_iid old_pod old_group
  old_claim="$(getstate e2e-drift.claim)"
  old_node="$(getstate e2e-drift.node)"
  old_iid="$(getstate e2e-drift.iid)"
  old_pod="$(getstate e2e-drift.pod)"
  old_group="$(jq -r '.firewall_group_id' <<<"$(instance_json "${old_iid}")")"

  log "the networking of the vultr block changes: the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p \
    '{"spec":{"vendors":{"vultr":{"networking":{"inbound":{"rules":[{"protocol":"tcp","fromPort":22,"cidrs":["192.0.2.0/24"]}]}}}}}}'
  restart_during_create e2e-drift "${old_claim}"
  wait_node "the Pod on another node" "${E2E_NODE_TIMEOUT}" app=e2e-drift "${old_claim}" pod_moved_from app=e2e-drift "${old_node}"
  release_do_not_disrupt
  local pod node claim iid new_group
  pod="$(kubectl get pod -l app=e2e-drift -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  iid="$(instance_of_claim "${claim}")"
  [[ "${iid}" != "${old_iid}" ]] || fail "the replacement runs on the old instance"
  new_group="$(jq -r '.firewall_group_id' <<<"$(instance_json "${iid}")")"
  [[ "${new_group}" != "${old_group}" ]] || fail "the replacement carries the old firewall group"
  assert_eq "the replacement's group has the new rule" 1 "$(vapi "/firewalls/${new_group}" | jq -r '.firewall_group.rule_count')"
  wait_until "the old instance deleted" 300 is_empty instances_of_id "${old_iid}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  assert_no_orphans
  setstate e2e-drift.node "${node}"
  setstate e2e-drift.claim "${claim}"
  setstate e2e-drift.iid "${iid}"
}

# instances_of_id <id>: the ID when the instance still exists.
instances_of_id() { cluster_instances | grep -Fx "$1" || true; }

# release_do_not_disrupt: removes karpenter.sh/do-not-disrupt from the drift replacement's
# NodeClaim and from every node of the lane (core copies a NodeClaim's annotations to its Node
# at registration, so removing it from the claim alone would keep the node from ever being
# consolidated). Idempotent.
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

# restart_during_create <nodepool> <old claim>: waits for the replacement NodeClaim of the pool,
# protects it from consolidation, and as soon as it exists and is not launched yet kills the
# controller Pod hard; the Deployment starts a new one. Create is idempotent (the NodeClaim UID
# is the instance's idempotency-key tag, found by an account-wide list before every create):
# after the restart the claim must not own two instances. The window is narrow, so the replay
# assertion is opportunistic: a missed window is noted, never a failure. What is always
# asserted: a claim that is still there owns at most one instance.
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
  note "restart: the controller was killed (window ${window}); instances of ${new} at that moment: '${before}'"
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=180s
  wait_until "the new controller reconciled the claim (launched, or gone)" 300 claim_settled "${new}"
  launched="$(kubectl get nodeclaim "${new}" -o jsonpath='{.status.conditions[?(@.type=="Launched")].status}' --ignore-not-found)"
  if [[ "${launched}" != True ]]; then
    note "restart (opportunistic): the claim ${new} is gone after the restart; only the no-orphan check applies"
    return 0
  fi
  after="$(instances_of_claim "${new}")"
  [[ "$(wc -w <<<"${after}" | tr -d ' ')" -le 1 ]] || fail "the claim ${new} owns several instances after the restart: ${after}"
  assert_eq "the claim's instance is the one in its annotation" "$(instance_of_claim "${new}")" "${after}"
  if [[ "${window}" == yes && -n "${before}" ]]; then
    assert_eq "the replay of Create found the first launch's instance (no second create)" "${before}" "${after}"
  fi
}

# claim_settled <claim>: launched, or no longer there.
claim_settled() {
  local out
  out="$(kubectl get nodeclaim "$1" -o jsonpath='{.status.conditions[?(@.type=="Launched")].status}' --ignore-not-found)" || return 1
  [[ "${out}" == True ]] || [[ -z "$(kubectl get nodeclaim "$1" -o name --ignore-not-found)" ]]
}

# instances_of_claim <claim>: the instances tagged with the claim.
instances_of_claim() {
  vapi "/instances?per_page=500&tag=$(uri "${label_domain}/nodeclaim=$1")" | jq -r '.instances[].id' | xargs
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

# scalezero (the roadmap's exit criterion): deleting the workloads deletes every instance within
# E2E_SCALE_DOWN_DEADLINE seconds of the Pods' deletion (WhenEmpty, 0 s); the nodes, NodeClaims
# and bootstrap tokens go too. No snapshot is deleted, and neither is a firewall group, a VPC or
# a brought object (their lifecycle is longer than the two minutes).
scn_scalezero() {
  local t0 elapsed
  drop_nodeshells
  release_do_not_disrupt
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every instance of the cluster deleted" "${E2E_SCALE_DOWN_DEADLINE}" is_empty cluster_instances
  elapsed=$(($(now) - t0))
  note "scale to zero: every instance deleted ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s)"
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name

  log "nothing that outlives the nodes was deleted"
  local desc g
  desc="flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-amd64"
  assert_eq "the Flatcar snapshot is still there" 1 \
    "$(vapi '/snapshots?per_page=500' | jq --arg d "${desc}" '[.snapshots[] | select(.description == $d and .status == "complete")] | length')"
  g="$(getstate e2e-amd64.group)"
  if [[ -n "${g}" ]]; then
    assert_eq "the managed firewall group of e2e-amd64 is still there (idleTTL)" 200 \
      "$(vapi "/firewalls/${g}" -o /dev/null -w '%{http_code}' || true)"
  fi
  g="$(getstate byo.group)"
  if [[ -n "${g}" ]]; then
    assert_eq "the brought firewall group is still there" 200 "$(vapi "/firewalls/${g}" -o /dev/null -w '%{http_code}' || true)"
    assert_eq "the brought VPC is still there" 200 "$(vapi "/vpcs/$(getstate byo.vpc)" -o /dev/null -w '%{http_code}' || true)"
  fi
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
  # With the NodeClasses gone the managed groups and VPCs fall under the default idle lifetime
  # (managedResources.defaultTTL, 5 minutes in the lane's values; 24 hours by default).
  wait_until "no managed firewall group of SCarpenter left" 900 is_value 0 managed_total groups
  wait_until "no managed VPC of SCarpenter left" 600 is_value 0 managed_total vpcs
  log "3. helm uninstall"
  helm uninstall scarpenter --namespace "${ns}" --wait --timeout 3m
  wait_until "the controller gone" 120 is_empty kubectl -n "${ns}" get pods -l app.kubernetes.io/name=scarpenter -o name
  # What the uninstall leaves behind (the CRDs, the brought objects, the snapshot) is what the
  # user created or owns; the brought ones are the teardown's.
}

# managed_total <groups|vpcs>: the objects SCarpenter created for the cluster, any NodeClass.
managed_total() { managed_of "$1" | jq 'length'; }

# scarpenter_claims: how many NodeClaims belong to an SCNodeClass.
scarpenter_claims() {
  kjson nodeclaims | jq '[.items[] | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'
}

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 described byo fw vpc negative drift scalezero uninstall)
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
