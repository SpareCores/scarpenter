#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5: the scenarios, run against the cluster that bootstrap.sh and install.sh built
# (docs/plans/v1/wip/16-gcp.md section 9; docs/how-to-guides/testing.md#the-l5-lane lists
# what each proves and how to read a failure).
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 spot arm64 db described byo create default negative quota drift
#                   scalezero uninstall
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
# after E2E_LAUNCH_TIMEOUT. A run needs about 24 vCPUs of the CPUS quota and 4 of
# PREEMPTIBLE_CPUS in the region (preflight checks).
#
# Needs: kubectl, jq, helm, the gcloud CLI with the CI identity's credentials, envsubst.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-gcp/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT GCP_PROJECT GCP_REGION GCP_ZONE KUBECONFIG
mkdir -p "${E2E_OUT}/state" "${E2E_OUT}/logs"
state="${E2E_OUT}/state"

# The zone the db scenario pins (the compact policy's zone must be deterministic).
E2E_PG_ZONE="${GCP_ZONE}"
export E2E_PG_ZONE

# The lane's own resources of the byo scenario (the sweeper's names and marker:
# pkg/vendors/gcp/sweep, ByoNamePrefix and LaneMarker; selftest.sh keeps the two together).
byo_net="scarpenter-e2e-byo-${E2E_RUN_ID}"
byo_subnet="${byo_net}-${GCP_REGION}"
byo_fw="${byo_net}-nodes"
byo_desc="scarpenter-e2e-byo: cluster=${E2E_CLUSTER}"
E2E_BYO_TAG="nodes"
E2E_BYO_SUBNET="projects/${GCP_PROJECT}/regions/${GCP_REGION}/subnetworks/${byo_subnet}"
export E2E_BYO_TAG E2E_BYO_SUBNET

# ------------------------------------------------------------- state --------

setstate() { printf '%s' "$2" >"${state}/$1"; } # setstate <name> <value>
getstate() { cat "${state}/$1" 2>/dev/null || true; }

# drop_nodeshells: the privileged Pods of node_shell would keep a node busy, and an
# empty node is what consolidation removes.
drop_nodeshells() { kubectl delete pods -l scarpenter-e2e=nodeshell --ignore-not-found --wait=false >/dev/null; }

# settle <app label>: waits for the Pod, records its node, claim, UID and instance
# ("<zone>/<name>").
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
  note "${app}: pod ${pod}, node ${node}, claim ${claim}, instance ${iid}, ready after $(ready_seconds "${claim}" "${node}")s"
}

# disk_json <zone> <disk name>
disk_json() { gc compute disks describe "$2" --zone "$1" --format=json; }

# boot_disk <instance json>: the name of the boot disk (an attached disk named as the instance).
boot_disk() { jq -r '.disks[] | select(.boot == true) | .source | split("/") | last' <<<"$1"; }

# The instance's cloud-side facts every scenario checks (plan 16 sections 3 and 9): running, the labels
# of the logical tags, the idempotency label, the metadata keys, no service account, an
# external address.
check_instance() { # check_instance <zone/name> <claim> <uid> <nodepool> <nodeclass>
  local j keys
  j="$(instance_json "$1")"
  assert_eq "instance status" RUNNING "$(jq -r '.status' <<<"${j}")"
  assert_eq "label cluster" "${E2E_CLUSTER}" "$(label_of "${j}" scarpenter-cluster)"
  assert_eq "label nodeclaim" "$2" "$(label_of "${j}" scarpenter-nodeclaim)"
  assert_eq "label nodepool" "$4" "$(label_of "${j}" scarpenter-nodepool)"
  assert_eq "label nodeclass" "$5" "$(label_of "${j}" scarpenter-nodeclass)"
  assert_eq "label managed-by" scarpenter "$(label_of "${j}" scarpenter-managed-by)"
  assert_eq "label idempotency key (the NodeClaim's UID)" "$3" "$(label_of "${j}" scarpenter-idempotency-key)"
  assert_eq "label ci-created-at (the NodeClass's spec.tags)" "${E2E_CREATED_AT}" "$(label_of "${j}" ci-created-at)"
  assert_eq "label ci-run" "${E2E_RUN_ID}" "$(label_of "${j}" ci-run)"
  # Metadata (plan 16 section 3): the Ignition, project SSH keys blocked, OS Login off.
  keys="$(meta_keys "${j}" | sort | paste -sd, -)"
  assert_match "metadata keys hold user-data" '(^|,)user-data(,|$)' "${keys}"
  assert_eq "block-project-ssh-keys" TRUE "$(meta_value "${j}" block-project-ssh-keys)"
  assert_eq "enable-oslogin" FALSE "$(meta_value "${j}" enable-oslogin)"
  assert_eq "no ssh-keys in the instance metadata" "" "$(meta_value "${j}" ssh-keys)"
  assert_eq "no service account (the node holds no cloud credentials)" 0 "$(jq -r '(.serviceAccounts // []) | length' <<<"${j}")"
  assert_nonempty "external IPv4" "$(jq -r '.networkInterfaces[0].accessConfigs[0].natIP // empty' <<<"${j}")"
  assert_eq "the boot disk is deleted with the instance" true "$(jq -r '.disks[] | select(.boot == true) | .autoDelete' <<<"${j}")"
  assert_match "a network tag on the instance" '.' "$(jq -r '(.tags.items // []) | join(",")' <<<"${j}")"
}

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_spot() { probe e2e-spot e2e-spot "${E2E_TYPE_AMD64}" spot; }
start_arm64() {
  [[ "${E2E_ARM64}" == true ]] || return 0
  probe e2e-arm64 e2e-arm64 "${E2E_TYPE_ARM64}" spot
}
start_db() { render "${here}/manifests/db.yaml" | kubectl apply -f -; }
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }

# start_byo: the network, the subnet and the firewall rule (network tag) the workflow
# creates with gcloud (named scarpenter-e2e-byo-<run id>, described with the lane marker the
# sweeper reads; networks, subnets and rules have no labels), the NodeClass that uses
# them, and the workload.
start_byo() {
  gc compute networks describe "${byo_net}" >/dev/null 2>&1 ||
    gc compute networks create "${byo_net}" --subnet-mode=custom --description="${byo_desc}" >/dev/null
  gc compute networks subnets describe "${byo_subnet}" --region "${GCP_REGION}" >/dev/null 2>&1 ||
    gc compute networks subnets create "${byo_subnet}" --network "${byo_net}" --region "${GCP_REGION}" \
      --range 10.200.0.0/24 --description="${byo_desc}" >/dev/null
  gc compute firewall-rules describe "${byo_fw}" >/dev/null 2>&1 ||
    gc compute firewall-rules create "${byo_fw}" --network "${byo_net}" --direction INGRESS --allow udp:41641,icmp \
      --target-tags "${E2E_BYO_TAG}" --source-ranges 0.0.0.0/0 --description="${byo_desc}" >/dev/null
  render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

start_create() {
  render "${here}/manifests/nodeclass-create.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-create.yaml" | kubectl apply -f -
  probe e2e-create e2e-create "${E2E_TYPE_AMD64}" on-demand
}

start_default() {
  [[ "$(getstate default_network)" == yes ]] || return 0
  render "${here}/manifests/nodeclass-default.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-default.yaml" | kubectl apply -f -
  probe e2e-default e2e-default "${E2E_TYPE_AMD64}" on-demand
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  for t in gcloud kubectl jq helm envsubst; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  gc compute regions describe "${GCP_REGION}" --format='value(status)' >/dev/null || fail "the Google credentials do not work for the project, or the region ${GCP_REGION} is unknown"
  assert_eq "the zone ${GCP_ZONE} is UP" UP "$(gc compute zones describe "${GCP_ZONE}" --format='value(status)')"

  # 1. The Flatcar images (plan 16 section 1): read by name (they cannot be listed): ready,
  # amd64, with the guest OS features the series need.
  local img feats
  img="$(gc compute images describe "${E2E_FLATCAR_IMAGE}" --project "${E2E_FLATCAR_IMAGE_PROJECT}" --format=json)" ||
    fail "the image ${E2E_FLATCAR_IMAGE_PROJECT}/${E2E_FLATCAR_IMAGE} does not exist (the pinned Flatcar ${E2E_FLATCAR_CHANNEL} ${E2E_FLATCAR_VERSION})"
  assert_eq "the Flatcar image is READY" READY "$(jq -r '.status' <<<"${img}")"
  assert_match "the Flatcar image's architecture (empty means x86_64)" '^(X86_64)?$' "$(jq -r '.architecture // ""' <<<"${img}")"
  feats="$(jq -r '[.guestOsFeatures[]?.type] | sort | join(",")' <<<"${img}")"
  note "the guest OS features of ${E2E_FLATCAR_IMAGE}: ${feats:-none}"
  assert_match "GVNIC on the image (c3 and later series need it: the db scenario's ${E2E_TYPE_DB})" '(^|,)GVNIC(,|$)' "${feats}"
  if [[ "${E2E_ARM64}" == true ]]; then
    [[ -n "${E2E_ARM64_IMAGE_PROJECT}" ]] || fail "E2E_ARM64=true needs E2E_ARM64_IMAGE_PROJECT, the project that holds the arm64 image"
    img="$(gc compute images describe "${E2E_FLATCAR_IMAGE}-arm64" --project "${E2E_ARM64_IMAGE_PROJECT}" --format=json)" ||
      fail "the arm64 image ${E2E_ARM64_IMAGE_PROJECT}/${E2E_FLATCAR_IMAGE}-arm64 does not exist"
    assert_eq "the arm64 image's architecture" ARM64 "$(jq -r '.architecture' <<<"${img}")"
  else
    note "arm64: skipped (E2E_ARM64 is not true: no official Flatcar arm64 image on GCE, plan 16 section 1)"
  fi

  # 2. The machine types are offered in the zone the scenarios pin.
  local t types=("${E2E_TYPE_AMD64}" "${E2E_TYPE_DB}")
  IFS=, read -ra described <<<"${E2E_TYPES_DESCRIBED}"
  types+=("${described[@]}")
  [[ "${E2E_ARM64}" != true ]] || types+=("${E2E_TYPE_ARM64}")
  for t in "${types[@]}"; do
    gc compute machine-types describe "${t#gcp.}" --zone "${GCP_ZONE}" --format='value(name)' >/dev/null ||
      fail "the machine type ${t#gcp.} is not offered in ${GCP_ZONE}"
  done

  # 3. The quotas the suite needs (the default of a new project is not enough). Spot VMs run on
  # the CPUS quota until the region has preemptible quota (a PREEMPTIBLE_CPUS limit above 0).
  local od od_used pre need_od
  read -r od od_used <<<"$(region_quota CPUS)"
  read -r pre _ <<<"$(region_quota PREEMPTIBLE_CPUS)"
  [[ -n "${od}" ]] || fail "regions.get of ${GCP_REGION} reports no CPUS quota"
  need_od="${E2E_MIN_VCPU_OD}"
  if [[ "$(num "${pre:-0}")" -eq 0 ]]; then
    need_od=$((E2E_MIN_VCPU_OD + E2E_MIN_VCPU_SPOT))
    note "the region has no preemptible quota (PREEMPTIBLE_CPUS ${pre:-absent}): Spot VMs use the CPUS quota, so the suite needs ${need_od} vCPUs of it"
  else
    [[ "$(num "${pre}")" -ge "${E2E_MIN_VCPU_SPOT}" ]] || fail "the PREEMPTIBLE_CPUS quota of ${GCP_REGION} is ${pre}; the suite needs ${E2E_MIN_VCPU_SPOT}"
  fi
  [[ "$(num "${od}")" -ge "${need_od}" ]] || fail "the CPUS quota of ${GCP_REGION} is ${od}; the suite needs ${need_od} (raise it, docs/how-to-guides/testing.md#the-l5-lane)"
  [[ "$(num "${od}")" -ge "$(($(num "${od_used}") + need_od))" ]] || note "the CPUS quota of ${GCP_REGION} is ${od} with ${od_used} in use: less than ${need_od} are free"

  # 4. Does the project have the default network (the default scenario)?
  if gc compute networks describe default >/dev/null 2>&1; then
    setstate default_network yes
  else
    setstate default_network no
    note "default: skipped (the project has no default network)"
  fi

  # 5. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc classes=(e2e-amd64 e2e-drift)
  [[ "${E2E_ARM64}" != true ]] || classes+=(e2e-arm64)
  for nc in "${classes[@]}"; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="VendorReady")].status}')"
    assert_eq "scnodeclass/${nc} ImagesReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="ImagesReady")].status}')"
  done
  note "project, region ${GCP_REGION}, zone ${GCP_ZONE} (the pinned zone of the db scenario)"
}

scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim iid uid zone zoneid j
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  iid="$(getstate e2e-amd64.iid)"
  uid="$(getstate e2e-amd64.uid)"

  log "node Ready, labels (overview section 5.5)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "region" "${GCP_REGION}" "$(node_label "${node}" topology.kubernetes.io/region)"
  assert_eq "capacity-type" on-demand "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" gcp "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${E2E_TYPE_AMD64#gcp.}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_nonempty "family" "$(node_label "${node}" "${label_domain}/family")"
  assert_eq "storage-profile" default "$(node_label "${node}" "${label_domain}/storage-profile")"
  assert_eq "placement-profile" default "$(node_label "${node}" "${label_domain}/placement-profile")"
  assert_eq "nodepool" e2e-amd64 "$(node_label "${node}" karpenter.sh/nodepool)"
  zone="$(node_label "${node}" topology.kubernetes.io/zone)"
  zoneid="$(node_label "${node}" "${label_domain}/zone-id")"
  assert_nonempty "zone name" "${zone}"
  assert_nonempty "zone ID" "${zoneid}"
  assert_match "the zone is a zone of the region" "^${GCP_REGION}-[a-z]$" "${zone}"
  assert_eq "the numeric zone ID of the zone name, from Compute" "${zoneid}" \
    "$(gc compute zones describe "${zone}" --format='value(id)')"

  log "the instance: labels, metadata, scheduling, disks (plan 16 section 3)"
  check_instance "${iid}" "${claim}" "${uid}" e2e-amd64 e2e-amd64
  j="$(instance_json "${iid}")"
  assert_eq "the instance's zone" "${zone}" "$(jq -r '.zone | split("/") | last' <<<"${j}")"
  assert_eq "machine type" "${E2E_TYPE_AMD64#gcp.}" "$(jq -r '.machineType | split("/") | last' <<<"${j}")"
  assert_eq "provisioning model" STANDARD "$(jq -r '.scheduling.provisioningModel // "STANDARD"' <<<"${j}")"
  assert_eq "automatic restart" false "$(jq -r '.scheduling.automaticRestart' <<<"${j}")"
  assert_match "onHostMaintenance" '^(MIGRATE|TERMINATE)$' "$(jq -r '.scheduling.onHostMaintenance' <<<"${j}")"
  # The root disk: a pd-balanced disk of 20 GB from the pinned Flatcar image.
  local d
  d="$(disk_json "${zone}" "$(boot_disk "${j}")")"
  assert_eq "root disk type" pd-balanced "$(jq -r '.type | split("/") | last' <<<"${d}")"
  assert_eq "root disk size" 20 "$(jq -r '.sizeGb' <<<"${d}")"
  assert_match "root disk image" "/${E2E_FLATCAR_IMAGE}\$" "$(jq -r '.sourceImage' <<<"${d}")"
  assert_eq "label cluster on the disk" "${E2E_CLUSTER}" "$(label_of "${d}" scarpenter-cluster)"

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

  log "cluster DNS, and the metadata server (plan 16 section 3: no project SSH keys, no OS Login users)"
  local dns
  dns="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  assert_match "the kubelet's clusterDNS is the kube-dns Service" "${dns//./\\.}" "$(node_shell "${node}" "grep -A3 '^clusterDNS' /var/lib/kubelet/config.yaml" | tr '\n' ' ')"
  wait_until "a Pod resolves a cluster Service" 120 kubectl exec "${pod}" -- nslookup kubernetes.default.svc.cluster.local
  # The user-data is readable through the metadata server (from the node, whose own the
  # data is). GCE has no hop limit: a Pod can read it too, which the reference documents
  # (the vendor Butane fragment block-metadata-from-pods is off by default). The Pod's
  # answer is recorded, not asserted: it depends on the pod network's masquerade.
  local mdq='wget -qO- -T 5 --header "Metadata-Flavor: Google" http://169.254.169.254/computeMetadata/v1/instance'
  assert_eq "the metadata server answers on the node (instance name)" "${iid#*/}" "$(node_pod_exec "${node}" "${mdq}/name" || true)"
  assert_match "the user-data is the Ignition" '"ignition"' "$(node_pod_exec "${node}" "${mdq}/attributes/user-data" || true)"
  note "metadata server from a Pod (exposure, documented): '$(kubectl exec "${pod}" -- sh -c "${mdq}/name" 2>&1 | head -c 80 || true)'"
  assert_eq "no login users on the node (uid 1000 and up): project SSH keys and OS Login are off" "" \
    "$(node_shell "${node}" "awk -F: '\$3 >= 1000 && \$3 < 65000 { print \$1 }' /etc/passwd" || true)"
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

# node_unit_checks <node>: what L4 scenarios Q09 and Q10 check inside a VM, on a GCE
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

# scn_spot: an e2-small Spot VM (plan 16 section 3): provisioningModel SPOT and, the
# default being STOP, instanceTerminationAction DELETE, so that a preempted VM does not leave
# a stopped instance and its disks behind.
scn_spot() {
  start_spot
  settle e2e-spot
  local pod node claim iid uid j
  pod="$(getstate e2e-spot.pod)"
  node="$(getstate e2e-spot.node)"
  claim="$(getstate e2e-spot.claim)"
  iid="$(getstate e2e-spot.iid)"
  uid="$(getstate e2e-spot.uid)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_AMD64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "capacity-type" spot "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" amd64 "$(node_label "${node}" kubernetes.io/arch)"
  check_instance "${iid}" "${claim}" "${uid}" e2e-spot e2e-amd64
  j="$(instance_json "${iid}")"
  assert_eq "provisioning model" SPOT "$(jq -r '.scheduling.provisioningModel' <<<"${j}")"
  assert_eq "instance termination action" DELETE "$(jq -r '.scheduling.instanceTerminationAction' <<<"${j}")"
  assert_eq "automatic restart" false "$(jq -r '.scheduling.automaticRestart' <<<"${j}")"
  assert_eq "onHostMaintenance" TERMINATE "$(jq -r '.scheduling.onHostMaintenance' <<<"${j}")"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  note "spot claim ${claim}: ready after $(ready_seconds "${claim}" "${node}")s"
}

# scn_arm64: optional (E2E_ARM64=true): the Spare Cores arm64 image on a t2a Spot VM.
scn_arm64() {
  if [[ "${E2E_ARM64}" != true ]]; then
    note "arm64: skipped (E2E_ARM64 is not true: the arm64 image is not published yet, plan 16 section 1)"
    return 0
  fi
  start_arm64
  settle e2e-arm64
  local pod node claim iid uid j d
  pod="$(getstate e2e-arm64.pod)"
  node="$(getstate e2e-arm64.node)"
  claim="$(getstate e2e-arm64.claim)"
  iid="$(getstate e2e-arm64.iid)"
  uid="$(getstate e2e-arm64.uid)"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "${E2E_TYPE_ARM64}" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "arch" arm64 "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "capacity-type" spot "$(node_label "${node}" karpenter.sh/capacity-type)"
  check_instance "${iid}" "${claim}" "${uid}" e2e-arm64 e2e-arm64
  j="$(instance_json "${iid}")"
  assert_eq "provisioning model" SPOT "$(jq -r '.scheduling.provisioningModel' <<<"${j}")"
  d="$(disk_json "$(jq -r '.zone | split("/") | last' <<<"${j}")" "$(boot_disk "${j}")")"
  assert_eq "the boot image's architecture" ARM64 "$(gc compute images describe "$(jq -r '.sourceImage | split("/") | last' <<<"${d}")" \
    --project "$(jq -r '.sourceImage | capture("projects/(?<p>[^/]+)/") | .p' <<<"${d}")" --format='value(architecture)')"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  assert_eq "tailscaled, the join unit and the kubelet are active (arm64 sysext)" "active,active,active" \
    "$(node_shell "${node}" 'systemctl is-active tailscaled scarpenter-tailscale-up.service kubelet' | paste -sd, -)"
  drop_nodeshells
  note "arm64 spot claim ${claim}"
}

# policies_of_cluster: the resource policies SCarpenter made for the cluster, as JSON.
policies_of_cluster() {
  gc compute resource-policies list --filter="name~^scarpenter-${E2E_CLUSTER}-" --format=json
}

scn_db() {
  start_db
  wait_node "the database and the client running" $((E2E_NODE_TIMEOUT + 300)) "scarpenter-e2e=true,app in (e2e-db,e2e-client)" "" \
    pod_running "scarpenter-e2e=true,app in (e2e-db,e2e-client)"
  local dbpod clpod dbnode clnode dbiid cliid dbclaim pol polname dbj clj
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

  log "one compact resource policy, named and described by SCarpenter"
  pol="$(policies_of_cluster)"
  assert_eq "resource policies of the cluster" 1 "$(jq 'length' <<<"${pol}")"
  polname="$(jq -r '.[0].name' <<<"${pol}")"
  # scarpenter-<cluster>-<kind>-<region>-<hash>, shortened by the engine to 63 characters.
  assert_match "the policy's name" "^scarpenter-${E2E_CLUSTER}-" "${polname}"
  [[ "${#polname}" -le 63 ]] || fail "the policy's name ${polname} is longer than 63 characters"
  assert_eq "the policy's description is the ownership marker" "scarpenter: cluster=${E2E_CLUSTER} nodeclass=e2e-amd64 managed-by=scarpenter resource-kind=resourcePolicies" \
    "$(jq -r '.[0].description' <<<"${pol}")"
  assert_eq "collocation" COLLOCATED "$(jq -r '.[0].groupPlacementPolicy.collocation' <<<"${pol}")"
  assert_eq "the policy's region" "${GCP_REGION}" "$(jq -r '.[0].region | split("/") | last' <<<"${pol}")"
  assert_match "the database is in the policy" "/${polname}\$" "$(jq -r '.resourcePolicies[0] // empty' <<<"${dbj}")"
  assert_match "the client is in the policy" "/${polname}\$" "$(jq -r '.resourcePolicies[0] // empty' <<<"${clj}")"
  assert_eq "one zone" "${E2E_PG_ZONE}" "$(jq -r '.zone | split("/") | last' <<<"${dbj}")"
  assert_eq "one zone (client)" "${E2E_PG_ZONE}" "$(jq -r '.zone | split("/") | last' <<<"${clj}")"

  log "node labels of the applied profiles"
  assert_eq "database storage-profile" db-hd "$(node_label "${dbnode}" "${label_domain}/storage-profile")"
  assert_eq "database placement-profile" colocated "$(node_label "${dbnode}" "${label_domain}/placement-profile")"
  assert_eq "client storage-profile" default "$(node_label "${clnode}" "${label_domain}/storage-profile")"
  assert_eq "client placement-profile" colocated "$(node_label "${clnode}" "${label_domain}/placement-profile")"
  assert_eq "database run key" "pg-${E2E_RUN_ID}" "$(node_label "${dbnode}" "${label_domain}/placement-group")"
  assert_eq "client run key" "pg-${E2E_RUN_ID}" "$(node_label "${clnode}" "${label_domain}/placement-group")"
  assert_eq "profile-hashes annotation names the storage profile" true \
    "$(kubectl get nodeclaim "${dbclaim}" -o json | jq -r --arg k "${label_domain}/profile-hashes" '.metadata.annotations[$k] | fromjson | has("storage/db-hd")')"

  log "the Hyperdisk data disk: attached by device name, deleted with the instance, labelled"
  local dd ddname
  dd="$(jq -c '[.disks[] | select(.boot != true)] | .[0]' <<<"${dbj}")"
  assert_eq "the data disk's device name (the guest path is /dev/disk/by-id/google-<name>)" pgdata "$(jq -r '.deviceName' <<<"${dd}")"
  assert_eq "the data disk is deleted with the instance (not GCE's default)" true "$(jq -r '.autoDelete' <<<"${dd}")"
  ddname="$(jq -r '.source | split("/") | last' <<<"${dd}")"
  local dj
  dj="$(disk_json "${E2E_PG_ZONE}" "${ddname}")"
  assert_eq "data disk type" hyperdisk-balanced "$(jq -r '.type | split("/") | last' <<<"${dj}")"
  assert_eq "data disk size" 10 "$(jq -r '.sizeGb' <<<"${dj}")"
  assert_eq "label cluster on the data disk" "${E2E_CLUSTER}" "$(label_of "${dj}" scarpenter-cluster)"

  log "on the node: the device by its name, the filesystem, the Pod's file"
  local src
  src="$(node_shell "${dbnode}" 'findmnt -no SOURCE /var/lib/pgdata')"
  assert_nonempty "the device mounted at /var/lib/pgdata" "${src}"
  assert_eq "the filesystem at /var/lib/pgdata" xfs "$(node_shell "${dbnode}" 'findmnt -no FSTYPE /var/lib/pgdata')"
  assert_eq "/dev/disk/by-id/google-pgdata is the mounted device (a NVMe-attached Hyperdisk too)" "$(node_shell "${dbnode}" "readlink -f ${src}")" \
    "$(node_shell "${dbnode}" 'readlink -f /dev/disk/by-id/google-pgdata')"
  assert_eq "the Pod's file is on the mounted volume" hello-from-the-volume "$(node_shell "${dbnode}" 'cat /var/lib/pgdata/hello')"
  assert_match "JobRunningTimeoutSec=240 on the device unit of the label" "dev-disk-by.*JobRunningTimeoutSec=240|JobRunningTimeoutSec=240" \
    "$(node_shell "${dbnode}" 'grep -r JobRunningTimeoutSec /etc/systemd/system/ 2>/dev/null | grep dev-disk-by' | head -n1)"

  log "the quota the c3 nodes consume: what the controller reports equals regions.get"
  local c3 c3_used
  read -r c3 c3_used <<<"$(region_quota C3_CPUS)"
  if [[ -n "${c3}" ]]; then
    [[ "$(num "${c3_used}")" -ge 8 ]] || fail "two c3-standard-4 are running but C3_CPUS usage is ${c3_used} (the consumption mapping of plan 16 section 5)"
    note "C3_CPUS limit ${c3}, usage ${c3_used} with the two db instances up; CPUS usage $(region_quota CPUS | cut -d' ' -f2)"
  else
    note "C3_CPUS: the region reports no such metric; the c3 nodes count against CPUS only"
  fi

  log "delete both workloads: the policy and the disk go"
  drop_nodeshells
  kubectl delete statefulset e2e-db --wait=false
  kubectl delete deployment e2e-client --wait=false
  wait_until "the instances gone" 300 is_empty instances_of "${dbiid}" "${cliid}"
  # perUse: two minutes of grace for launches in flight, then the next resource check
  # and garbage collection (30 s and 1 m here).
  wait_until "the resource policy deleted" 420 is_value 0 policy_count
  wait_until "the data disk gone" 120 is_empty gc compute disks list --filter="name=${ddname}" --format='value(name)'
}

policy_count() { policies_of_cluster | jq 'length'; }

# instances_of <zone/name>...: those of the instances that still exist.
instances_of() {
  local i f="" out
  for i in "$@"; do f+="${f:+ OR }(name=${i#*/} AND zone~/${i%%/*}\$)"; done
  out="$(gc compute instances list --filter="${f}" --format='value(name)')" || return 1
  printf '%s' "${out}"
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
  [[ "${t}" != "${E2E_TYPE_AMD64}" ]] || fail "the type ${t} has too little memory for the request (memory-mib > 3000)"
  [[ "${vcpus}" -gt 1 && "${vcpus}" -lt 3 ]] || fail "vcpus ${vcpus} is outside (1, 3)"
  [[ "${mem}" -gt 3000 ]] || fail "memory ${mem} MiB is not above 3000"
  [[ "${pct}" -gt 0 ]] || fail "the percentile ${pct} is not above 0"
  assert_eq "the instance is of the chosen type" "${t#gcp.}" "$(jq -r '.machineType | split("/") | last' <<<"$(instance_json "${iid}")")"
  # The same facts from Compute, independent of the catalog (shared-core types report their
  # vCPUs as guestCpus too).
  assert_eq "Compute's vCPUs of the type" "${vcpus}" \
    "$(gc compute machine-types describe "${t#gcp.}" --zone "$(node_label "${node}" topology.kubernetes.io/zone)" --format='value(guestCpus)')"
}

scn_byo() {
  start_byo
  wait_until "ResourcesReady on the NodeClass" 180 is_value True \
    kubectl get scnodeclass e2e-use -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="ResourcesReady")].status}'
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-use "" pod_running app=e2e-use
  local pod node iid j fw_before
  pod="$(kubectl get pod -l app=e2e-use -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  iid="$(instance_of_claim "$(claim_of_node "${node}")")"
  j="$(instance_json "${iid}")"
  assert_eq "the instance is in the brought subnet" "${byo_subnet}" "$(jq -r '.networkInterfaces[0].subnetwork | split("/") | last' <<<"${j}")"
  assert_eq "the instance is in the brought network" "${byo_net}" "$(jq -r '.networkInterfaces[0].network | split("/") | last' <<<"${j}")"
  assert_eq "the instance carries the brought firewall's network tag" true "$(jq -r --arg t "${E2E_BYO_TAG}" '(.tags.items // []) | index($t) != null' <<<"${j}")"
  assert_match "the zone is in the region" "^${GCP_REGION}-[a-z]\$" "$(jq -r '.zone | split("/") | last' <<<"${j}")"
  fw_before="$(gc compute firewall-rules describe "${byo_fw}" --format=json | jq -S -c 'del(.creationTimestamp, .id, .selfLink)')"

  log "SCarpenter must not touch what it did not create"
  drop_nodeshells
  kubectl delete deployment e2e-use --wait=false
  wait_until "the instance gone" 300 is_empty instances_of "${iid}"
  kubectl delete nodepool e2e-use --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-use --wait=true --timeout=120s
  # Past a garbage collection (1 m), a resource check (30 s) and the status controller:
  # a `use` reference is never modified or deleted.
  sleep 120
  assert_eq "the subnet still exists" "${byo_subnet}" "$(gc compute networks subnets describe "${byo_subnet}" --region "${GCP_REGION}" --format='value(name)')"
  assert_eq "the network still exists" "${byo_net}" "$(gc compute networks describe "${byo_net}" --format='value(name)')"
  assert_eq "the firewall rule still exists, unchanged" "${fw_before}" \
    "$(gc compute firewall-rules describe "${byo_fw}" --format=json | jq -S -c 'del(.creationTimestamp, .id, .selfLink)')"
  note "the brought network ${byo_net}, its subnet and rule are deleted by the teardown (the sweeper, by the lane marker)"
}

# scn_create: no `use`: SCarpenter creates the custom-mode network, the subnet and the
# firewall rules, the node uses them, and they go by their lifecycle (idleTTL) once the
# NodeClass is deleted. Networks, subnets and rules carry no labels: the ownership is the
# name prefix scarpenter-<cluster>- and the description marker (ADR-018's fallback).
scn_create() {
  start_create
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-create "" pod_running app=e2e-create
  local pod node iid j net sn fws tag pfx="scarpenter-${E2E_CLUSTER}-"
  pod="$(kubectl get pod -l app=e2e-create -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  iid="$(instance_of_claim "$(claim_of_node "${node}")")"
  j="$(instance_json "${iid}")"

  log "the managed network: custom mode, named and described by SCarpenter"
  net="$(gc compute networks list --filter="name~^${pfx}" --format=json)"
  assert_eq "managed networks of the cluster" 1 "$(jq 'length' <<<"${net}")"
  assert_match "the network's name" "^${pfx}" "$(jq -r '.[0].name' <<<"${net}")"
  assert_match "the network's description is the ownership marker" "^scarpenter: cluster=${E2E_CLUSTER} nodeclass=[a-z0-9-]* managed-by=scarpenter resource-kind=[A-Za-z]+\$" \
    "$(jq -r '.[0].description' <<<"${net}")"
  assert_eq "custom subnet mode (no auto-created subnets)" false "$(jq -r '.[0].autoCreateSubnetworks' <<<"${net}")"
  assert_eq "the instance is in the managed network" "$(jq -r '.[0].name' <<<"${net}")" \
    "$(jq -r '.networkInterfaces[0].network | split("/") | last' <<<"${j}")"

  log "the subnet of the region, and the firewall rules for the network tag"
  sn="$(gc compute networks subnets list --filter="name~^${pfx} AND region~/${GCP_REGION}\$" --format=json)"
  assert_eq "managed subnets in the region" 1 "$(jq 'length' <<<"${sn}")"
  assert_match "the subnet is in the pool 10.240.0.0/12" '^10\.(24[0-9]|25[0-5])\.' "$(jq -r '.[0].ipCidrRange' <<<"${sn}")"
  assert_match "the subnet's description is the ownership marker" "managed-by=scarpenter" "$(jq -r '.[0].description' <<<"${sn}")"
  assert_eq "the instance is in the managed subnet" "$(jq -r '.[0].name' <<<"${sn}")" "$(jq -r '.networkInterfaces[0].subnetwork | split("/") | last' <<<"${j}")"
  fws="$(gc compute firewall-rules list --filter="name~^${pfx}" --format=json)"
  [[ "$(jq 'length' <<<"${fws}")" -ge 1 ]] || fail "no managed firewall rule"
  assert_eq "every managed rule is on the managed network and marked" true \
    "$(jq -r --arg n "$(jq -r '.[0].name' <<<"${net}")" 'all(.[]; (.network | split("/") | last) == $n and (.description | test("managed-by=scarpenter")))' <<<"${fws}")"
  tag="$(jq -r '[.[] | select(.description | test("nodeclass=e2e-create "))] | .[0].targetTags[0] // empty' <<<"${fws}")"
  assert_nonempty "the managed rules' target tag" "${tag}"
  assert_eq "the instance carries the network tag of the managed rules" true "$(jq -r --arg t "${tag}" '(.tags.items // []) | index($t) != null' <<<"${j}")"

  log "delete the workload, the pool and the NodeClass: the resources go by their lifecycle"
  drop_nodeshells
  kubectl delete deployment e2e-create --wait=false
  wait_until "the instance gone" 300 is_empty instances_of "${iid}"
  kubectl delete nodepool e2e-create --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-create --wait=true --timeout=300s
  # The rules of the class (3 minutes idle), then the subnet and the network once no class
  # uses them: the other NodeClasses of the run use the same network, so the network and
  # the subnet stay until the uninstall scenario; the rules of this class do not.
  wait_until "the managed firewall rules of e2e-create deleted" 600 is_empty managed_rules_of e2e-create
}

managed_rules_of() { # managed_rules_of <nodeclass>: the managed rules whose marker names the NodeClass
  gc compute firewall-rules list --filter="name~^scarpenter-${E2E_CLUSTER}- AND description~nodeclass=$1[[:space:]]" --format='value(name)'
}

# scn_default: no resources block: the project's default network (plan 16 section 2).
scn_default() {
  if [[ "$(getstate default_network)" != yes ]]; then
    note "default: skipped (the project has no default network)"
    return 0
  fi
  start_default
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-default "" pod_running app=e2e-default
  local pod node iid j
  pod="$(kubectl get pod -l app=e2e-default -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  iid="$(instance_of_claim "$(claim_of_node "${node}")")"
  j="$(instance_json "${iid}")"
  assert_eq "the instance is in the default network" default "$(jq -r '.networkInterfaces[0].network | split("/") | last' <<<"${j}")"
  assert_eq "the instance is in the default subnet of the region" "projects/${GCP_PROJECT}/regions/${GCP_REGION}/subnetworks/default" \
    "$(jq -r '.networkInterfaces[0].subnetwork | capture("(?<p>projects/.*)") | .p' <<<"${j}")"
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  note "default network: the instance's network tags: $(jq -r '(.tags.items // []) | join(",")' <<<"${j}")"
  # SCarpenter creates no network for it and leaves the default network alone.
  assert_eq "the default network still exists" default "$(gc compute networks describe default --format='value(name)')"
  drop_nodeshells
  kubectl delete deployment e2e-default --wait=false
  wait_until "the instance gone" 300 is_empty instances_of "${iid}"
}

# negative: NodeClasses the driver must refuse, or cannot serve, and nothing is launched
# (ADR-022).
scn_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
  wait_until "a NodeClass with another project is refused" 180 is_value False \
    kubectl get scnodeclass e2e-badproject -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="VendorReady")].status}'
  note "e2e-badproject: VendorReady=False, reason $(kubectl get scnodeclass e2e-badproject -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="VendorReady")].reason}')"
  assert_match "the message names the project" "project" \
    "$(kubectl get scnodeclass e2e-badproject -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="VendorReady")].message}')"
  # publicIPv4: false: no egress without a NAT. What the driver says is recorded for the
  # first live run; the scenario waits for the status controller to have decided.
  wait_until "the status of e2e-noipv4 is reconciled" 180 nodeclass_observed e2e-noipv4
  note "e2e-noipv4 (publicIPv4: false): VendorReady=$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="VendorReady")].status}')/$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="VendorReady")].reason}'): $(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.gcp.conditions[?(@.type=="VendorReady")].message}')"
  assert_eq "no instance was launched for them" "" \
    "$(gc compute instances list --filter="labels.scarpenter-nodeclass=e2e-badproject OR labels.scarpenter-nodeclass=e2e-noipv4" --format='value(name)')"
  kubectl delete scnodeclass e2e-badproject e2e-noipv4 --wait=true --timeout=120s
}

# quota (overview section 13.4, plan 16 section 5): what the controller reports equals what
# regions.get says, bucket by bucket (GCP gives the usage with the limit: nothing is computed
# from instances). Run with the nodes of the other scenarios up and no launch in flight.
scn_quota() {
  settle_fixtures
  local m metric
  m="$(mktemp)"
  log "the limits of the region's CPU buckets and the instance count equal regions.get"
  local timeout=240 seen=0
  for metric in $(quota_metrics); do
    wait_until "scarpenter_quota_limit ${metric} equals regions.get" "${timeout}" quota_field_is "${m}" limit "${metric}"
    timeout=90 # the first one waited for the controller's first limits refresh
    seen=$((seen + 1))
  done
  [[ "${seen}" -ge 1 ]] || fail "the controller reports none of the region's quota buckets"
  [[ -n "$(metric_value "${m}" scarpenter_quota_limit "${GCP_REGION}" CPUS)" ]] || fail "no scarpenter_quota_limit series for the CPUS bucket"

  log "usage: what the controller reports is what regions.get says is used"
  for metric in $(quota_metrics); do
    wait_until "scarpenter_quota_used ${metric} equals regions.get" 300 quota_field_is "${m}" used "${metric}"
  done
  # The consumption mapping (plan 16 section 5): the e2 nodes count against CPUS (E2 has no
  # metric of its own), two e2-small instances (the amd64 node and the spot one, if Spot runs on
  # the standard quota) use at least 2 vCPUs of it.
  local used
  used="$(region_quota CPUS | cut -d' ' -f2)"
  [[ "$(num "${used}")" -ge 2 ]] || fail "an e2-small is running but the CPUS usage is ${used}"
  wait_until "the in-flight reservations are gone" 240 quota_reserved_zero "${m}" CPUS
  note "the quota usage after the nodes of the quota scenario: CPUS $(region_quota CPUS), PREEMPTIBLE_CPUS $(region_quota PREEMPTIBLE_CPUS)"

  log "the launched types are offered in their zones"
  local t zone
  for t in e2e-amd64:"${E2E_TYPE_AMD64}" e2e-spot:"${E2E_TYPE_AMD64}"; do
    zone="$(node_label "$(getstate "${t%%:*}.node")" topology.kubernetes.io/zone)"
    assert_eq "${t#*:gcp.} in machineTypes of ${zone}" "${t#*:gcp.}" "$(gc compute machine-types describe "${t#*:gcp.}" --zone "${zone}" --format='value(name)')"
  done
  rm -f "${m}"
}

# quota_metrics: the metrics of regions.get that are buckets of the controller: the vCPU
# metrics and the instance count of the region (CPUS_ALL_REGIONS is a project-wide bucket).
quota_metrics() {
  gc compute regions describe "${GCP_REGION}" --format=json |
    jq -r '.quotas[].metric | select((. == "CPUS" or . == "INSTANCES" or test("_CPUS$")) and . != "CPUS_ALL_REGIONS")'
}

# quota_field_is <metrics file> limit|used <metric>: the controller's series equals the number
# regions.get reports now (a metric the controller has no series for is not compared).
quota_field_is() {
  local want col=2
  [[ "$2" == limit ]] && col=1
  want="$(region_quota "$3" | cut -d' ' -f"${col}")"
  metrics >"$1"
  local got
  got="$(metric_value "$1" "scarpenter_quota_$2" "${GCP_REGION}" "$3")"
  [[ -n "${got}" ]] || return 0
  [[ "$(num "${got}")" == "$(num "${want}")" ]]
}

quota_reserved_zero() { # quota_reserved_zero <metrics file> <bucket>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_reserved "${GCP_REGION}" "$2")")" == 0 ]]
}

# settle_fixtures: the amd64 and spot nodes of the first scenarios (started if the
# scenario runs alone).
settle_fixtures() {
  [[ -n "$(getstate e2e-amd64.iid)" ]] || {
    start_amd64
    settle e2e-amd64
  }
  [[ -n "$(getstate e2e-spot.iid)" ]] || {
    start_spot
    settle e2e-spot
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
    '{"spec":{"profiles":{"storage":{"scratch":{"gcp":{"data":[{"name":"scratch","diskType":"pd-balanced","sizeGb":20,"filesystem":"ext4","mountPath":"/var/lib/scratch"}]}}}}}}'
  wait_until "the NodeClass reconciled the change" 120 nodeclass_observed e2e-drift
  local i
  for i in 1 2 3 4 5 6; do
    sleep 10
    assert_eq "the claim is not drifted (${i}/6)" "" \
      "$(kubectl get nodeclaim "${old_claim}" -o jsonpath='{.status.conditions[?(@.type=="Drifted")].status}' | grep -x True || true)"
  done
  assert_eq "still the same claim" "${old_claim}" "$(claim_of_node "${old_node}")"

  log "the root disk size changes: the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p '{"spec":{"vendors":{"gcp":{"storage":{"root":{"sizeGb":24}}}}}}'
  restart_during_create e2e-drift "${old_claim}"
  wait_node "the Pod on another node" "${E2E_NODE_TIMEOUT}" app=e2e-drift "${old_claim}" pod_moved_from app=e2e-drift "${old_node}"
  release_do_not_disrupt
  local pod node claim iid j
  pod="$(kubectl get pod -l app=e2e-drift -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  iid="$(instance_of_claim "${claim}")"
  [[ "${iid}" != "${old_iid}" ]] || fail "the replacement runs on the old instance"
  j="$(instance_json "${iid}")"
  assert_eq "the replacement's root disk (the new size)" 24 "$(jq -r '.disks[] | select(.boot == true) | .diskSizeGb' <<<"${j}")"
  wait_until "the old instance gone" 300 is_empty instances_of "${old_iid}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  assert_no_orphans
  # Not opportunistic: a deleted instance is gone from the API, but the operations of the
  # project are listed, so every successful insert ever made for the replacement claim is
  # still visible here, even one the garbage collector already deleted as an orphan. More than
  # one is a second insert for the same claim (a replay that did not find the first launch).
  local repl ever
  repl="$(getstate drift.replacement)"
  ever="$(inserts_of_claim "${repl}")"
  [[ "${ever}" -le 1 ]] || fail "the claim ${repl} was inserted ${ever} times (a second instances.insert after the restart)"
  setstate e2e-drift.node "${node}"
  setstate e2e-drift.claim "${claim}"
  setstate e2e-drift.iid "${iid}"
}

# inserts_of_claim <claim>: the number of finished instances.insert operations without an
# error whose target is the claim's instance (the instance is named as the claim).
inserts_of_claim() {
  gc compute operations list --filter="operationType=insert AND targetLink~/instances/$1\$" --format=json |
    jq '[.[] | select(.error == null)] | length'
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
# (the idempotency label and the requestId, the controller replays it): after the restart the
# claim must not own two instances, and no instance may be orphaned.
#
# The window (the instance exists, Create has not returned) is narrow and no flag stretches it
# (the lane builds the production image; no test-only driver flag exists), so the replay
# assertions are OPPORTUNISTIC: when the attempt misses the window the scenario notes it and
# never fails for it. What always holds and is always asserted: no instance is left without
# a NodeClaim (the caller), and a claim that is still there owns one instance.
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
    # The one assertion that can fail on the window: a second insert for the claim.
    assert_eq "the replay of Create found the first launch's instance (no second instances.insert)" "${before}" "${after}"
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

# instances_of_claim <claim>: "<zone>/<name>" of the instances labelled with the claim.
instances_of_claim() {
  gc compute instances list --filter="labels.scarpenter-nodeclaim=$1" --format='value(zone.basename(),name)' | tr '\t' '/' | xargs
}

# assert_no_orphans: every instance of the cluster belongs to a NodeClaim that exists.
assert_no_orphans() {
  wait_until "no orphaned instance" 240 no_orphans
}

no_orphans() {
  local claims ids id
  claims="$(kjson nodeclaims | jq -r --arg k "${label_domain}/instance-ref" '.items[] | .metadata.annotations[$k] // empty' | sed 's#^[^/]*/##')"
  ids="$(alive_instances)"
  for id in ${ids}; do
    grep -qx "${id}" <<<"${claims}" || {
      echo "instance ${id} has no NodeClaim"
      return 1
    }
  done
}

# scalezero (plan 10 scenario 3, the roadmap's exit criterion): deleting the workloads
# deletes every instance within E2E_SCALE_DOWN_DEADLINE seconds of the Pods' deletion
# (WhenEmpty, 0 s); the nodes, NodeClaims and bootstrap tokens go too, and so do the disks;
# the quota usage falls with the instances.
scn_scalezero() {
  local t0 elapsed
  drop_nodeshells
  release_do_not_disrupt
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every instance stopping or gone" "${E2E_SCALE_DOWN_DEADLINE}" is_empty alive_instances
  note "scale to zero: every instance was stopping (the delete was issued) $(($(now) - t0))s after the Pods' deletion"
  # The deadline counts from the Pods' deletion for both.
  wait_until "every instance of the cluster gone" "$((E2E_SCALE_DOWN_DEADLINE > $(now) - t0 ? E2E_SCALE_DOWN_DEADLINE - ($(now) - t0) : 1))" is_empty cluster_instances
  elapsed=$(($(now) - t0))
  note "scale to zero: every instance gone ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s)"
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name
  # Disks go with their instance (autoDelete, boot and data): the leak a Spot or a failed
  # create would leave is a disk without an instance.
  wait_until "no disk of the cluster left (autoDelete)" 180 is_empty gc compute disks list --filter="labels.${ctag}=${E2E_CLUSTER}" --format='value(name)'
  log "the quota usage follows the instances down"
  local m metric
  m="$(mktemp)"
  for metric in CPUS PREEMPTIBLE_CPUS; do
    [[ -n "$(region_quota "${metric}")" ]] || continue
    wait_until "the ${metric} usage equals regions.get" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_field_is "${m}" used "${metric}"
  done
  wait_until "no reservation is left" 120 quota_reserved_zero "${m}" CPUS
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
  wait_until "no instance of the cluster" 300 is_empty cluster_instances
  # With the NodeClasses gone the managed rules, subnets and networks fall under the default
  # idle lifetime (managedResources.defaultTTL, 5 minutes in the lane's values; 24 hours by
  # default).
  wait_until "no managed firewall rule left" 900 is_empty managed_names firewall-rules
  wait_until "no managed subnet left" 300 is_empty managed_names networks subnets
  wait_until "no managed network left" 300 is_empty managed_names networks
  wait_until "no resource policy left" 60 is_value 0 policy_count
  log "3. helm uninstall"
  helm uninstall scarpenter --namespace "${ns}" --wait --timeout 3m
  wait_until "the controller gone" 120 is_empty kubectl -n "${ns}" get pods -l app.kubernetes.io/name=scarpenter -o name
  # What the uninstall leaves behind (the CRDs and what the user created) is listed in
  # the how-to; what carries the cluster's name or label is checked by the teardown.
}

# managed_names <gcloud compute collection words...>: the names of SCarpenter's resources of
# the cluster in the collection (the byo resources carry another prefix and stay for the teardown).
managed_names() {
  gc compute "$@" list --filter="name~^scarpenter-${E2E_CLUSTER}-" --format='value(name)'
}

# scarpenter_claims: how many NodeClaims belong to an SCNodeClass.
scarpenter_claims() {
  kjson nodeclaims | jq '[.items[] | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'
}

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 spot arm64 db described byo create default negative quota drift scalezero uninstall)
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
