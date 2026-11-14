#!/usr/bin/env bash
# shellcheck disable=SC2329  # the scenario and start functions are called by name from the runner
# L5: the scenarios, run against the cluster that bootstrap.sh and install.sh built
# (docs/plans/v1/wip/25-oci.md section 11; docs/how-to-guides/testing.md#the-l5-lane-on-oci lists
# what each proves and how to read a failure).
#
#   E2E_SCENARIOS   comma-separated names to run (default: all), in this order:
#                   amd64 spot arm64 described byo create negative quota drift scalezero uninstall
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
# after E2E_LAUNCH_TIMEOUT. A run needs about 8 OCPUs and 48 GB of the E5 limits in the
# region (preflight checks). There is no `db` scenario (no data volumes in v1), no `default` one
# (a tenancy has no default network) and no bare metal (cost and boot time).
#
# Needs: kubectl, jq, helm, the oci CLI with the CI identity's session (oci-session.sh), envsubst.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/e2e-oci/lib.sh
source "${here}/lib.sh"
need E2E_CLUSTER E2E_RUN_ID E2E_CREATED_AT E2E_OUT OCI_COMPARTMENT OCI_TENANCY OCI_REGION KUBECONFIG
mkdir -p "${E2E_OUT}/state" "${E2E_OUT}/logs"
state="${E2E_OUT}/state"

# The lane's own resources of the byo scenario (the sweeper's names and tag:
# pkg/vendors/oci/sweep, ByoNamePrefix and LaneTag; selftest.sh keeps the two together).
byo_net="scarpenter-e2e-byo-${E2E_RUN_ID}"
byo_tags="{\"e2e-byo-cluster\":\"${E2E_CLUSTER}\",\"ci-created-at\":\"${E2E_CREATED_AT}\"}"

# ------------------------------------------------------------- state --------

setstate() { printf '%s' "$2" >"${state}/$1"; } # setstate <name> <value>
getstate() { cat "${state}/$1" 2>/dev/null || true; }

# drop_nodeshells: the privileged Pods of node_shell would keep a node busy, and an
# empty node is what consolidation removes.
drop_nodeshells() { kubectl delete pods -l scarpenter-e2e=nodeshell --ignore-not-found --wait=false >/dev/null; }

# settle <app label>: waits for the Pod, records its node, claim, UID and instance (the OCID).
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

# The instance's cloud-side facts every scenario checks (plan 25 sections 4, 5 and 11): running, the
# freeform tags of the logical tags, the idempotency tag, the metadata keys (the Ignition and no SSH
# keys), the legacy metadata endpoints disabled, a public address on the primary VNIC.
check_instance() { # check_instance <instance ocid> <claim> <uid> <nodepool> <nodeclass>
  local j keys vnic
  j="$(instance_json "$1")"
  assert_eq "instance state" RUNNING "$(jq -r '."lifecycle-state"' <<<"${j}")"
  assert_eq "region" "${OCI_REGION}" "$(jq -r '.region' <<<"${j}")"
  assert_eq "tag cluster" "${E2E_CLUSTER}" "$(tag_of "${j}" scarpenter-cluster)"
  assert_eq "tag nodeclaim" "$2" "$(tag_of "${j}" scarpenter-nodeclaim)"
  assert_eq "tag nodepool" "$4" "$(tag_of "${j}" scarpenter-nodepool)"
  assert_eq "tag nodeclass" "$5" "$(tag_of "${j}" scarpenter-nodeclass)"
  assert_eq "tag managed-by" scarpenter "$(tag_of "${j}" scarpenter-managed-by)"
  assert_eq "tag idempotency key (the NodeClaim's UID)" "$3" "$(tag_of "${j}" scarpenter-idempotency-key)"
  assert_eq "tag ci-created-at (the NodeClass's spec.tags)" "${E2E_CREATED_AT}" "$(tag_of "${j}" ci-created-at)"
  assert_eq "tag ci-run" "${E2E_RUN_ID}" "$(tag_of "${j}" ci-run)"
  # Metadata (plan 25 section 5): the Ignition, no SSH keys (Ignition installs keys, never the metadata).
  keys="$(meta_keys "${j}" | sort | paste -sd, -)"
  assert_match "metadata keys hold user_data" '(^|,)user_data(,|$)' "${keys}"
  assert_eq "no ssh_authorized_keys in the instance metadata" "" "$(jq -r '.metadata.ssh_authorized_keys // empty' <<<"${j}")"
  assert_eq "the legacy metadata endpoints are disabled" true "$(jq -r '."instance-options"."are-legacy-imds-endpoints-disabled"' <<<"${j}")"
  vnic="$(vnic_json "$1")"
  assert_nonempty "public IPv4" "$(jq -r '."public-ip" // empty' <<<"${vnic}")"
  assert_match "a network security group on the VNIC" '^ocid1\.networksecuritygroup\.' "$(jq -r '(."nsg-ids" // [])[0] // empty' <<<"${vnic}")"
}

# check_shape <instance json> <type name>: the shape and the shapeConfig of a sized type
# (oci.VM.Standard.E5.Flex.1-4: shape VM.Standard.E5.Flex with 1 OCPU and 4 GB).
check_shape() {
  local name="${2#oci.}" shape size
  shape="${name%.*}"
  size="${name##*.}"
  assert_eq "shape" "${shape}" "$(jq -r '.shape' <<<"$1")"
  assert_eq "OCPUs of the shapeConfig" "${size%%-*}" "$(num "$(jq -r '."shape-config".ocpus' <<<"$1")")"
  assert_eq "memory of the shapeConfig (GB)" "${size##*-}" "$(num "$(jq -r '."shape-config"."memory-in-gbs"' <<<"$1")")"
}

# check_node_labels <node> <type> <capacity> <arch> <nodepool>: the labels of overview section 5.5,
# the zone name and the Keeper zone ID consistent with the instance's availability domain.
check_node_labels() {
  local node="$1" zone zoneid
  assert_eq "the node is Ready" True "$(kubectl get node "${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
  assert_eq "instance-type" "$2" "$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_eq "region" "${OCI_REGION}" "$(node_label "${node}" topology.kubernetes.io/region)"
  assert_eq "capacity-type" "$3" "$(node_label "${node}" karpenter.sh/capacity-type)"
  assert_eq "arch" "$4" "$(node_label "${node}" kubernetes.io/arch)"
  assert_eq "os" linux "$(node_label "${node}" kubernetes.io/os)"
  assert_eq "vendor" oci "$(node_label "${node}" "${label_domain}/vendor")"
  assert_eq "api-reference" "${2#oci.}" "$(node_label "${node}" "${label_domain}/api-reference")"
  assert_eq "nodepool" "$5" "$(node_label "${node}" karpenter.sh/nodepool)"
  zone="$(node_label "${node}" topology.kubernetes.io/zone)"
  zoneid="$(node_label "${node}" "${label_domain}/zone-id")"
  assert_nonempty "zone name" "${zone}"
  assert_match "the zone is an availability domain of the region (without the tenancy's prefix)" '-AD-[0-9]+$' "${zone}"
  assert_eq "the Keeper zone ID of the zone name" "${OCI_REGION}-ad-${zone##*-AD-}" "${zoneid}"
  assert_match "the zone is one of the tenancy's availability domains" "(^|,)${zone}(,|$)" "$(zone_names | paste -sd, -)"
}

# ------------------------------------------------------------ the starts ----
# Idempotent: they apply what the scenario then waits for.

start_amd64() { probe e2e-amd64 e2e-amd64 "${E2E_TYPE_AMD64}" on-demand; }
start_spot() { probe e2e-spot e2e-spot "${E2E_TYPE_AMD64}" spot; }
start_arm64() {
  [[ "${E2E_ARM64}" == true ]] || return 0
  # The pool allows A1 and A2: the Pod selects the pool and takes whichever launches.
  NAME=e2e-arm64 POOL=e2e-arm64 render "${here}/manifests/probe-arm64.yaml" | kubectl apply -f -
}
start_described() { render "${here}/manifests/described.yaml" | kubectl apply -f -; }
start_drift() { probe e2e-drift e2e-drift "${E2E_TYPE_AMD64}" on-demand; }

# byo_find <oci network collection> <display name>: the OCID of the not terminated resource of the name.
byo_find() {
  oc network "$1" list --compartment-id "${OCI_COMPARTMENT}" --display-name "$2" --all --output json |
    jq -r '[(.data // [])[] | select(."lifecycle-state" != "TERMINATED") | .id] | first // empty'
}

# byo_ensure <collection> <name> <create arguments...>: the OCID of the resource of the name, created
# with the lane's tag when it does not exist (named scarpenter-e2e-byo-<run id>*, tagged with the
# cluster of the run: networks have no description, the tag is the marker the sweeper reads).
byo_ensure() {
  local kind="$1" name="$2" id
  shift 2
  id="$(byo_find "${kind}" "${name}")"
  if [[ -z "${id}" ]]; then
    id="$(oc network "${kind}" create --compartment-id "${OCI_COMPARTMENT}" --display-name "${name}" --freeform-tags "${byo_tags}" \
      "$@" --wait-for-state AVAILABLE --output json | jq -r '.data.id')"
  fi
  printf '%s' "${id}"
}

# start_byo: the VCN, the internet gateway, the route table, the subnet and the network security group
# the workflow creates with the oci CLI, the NodeClass that uses the subnet and the group by their
# OCIDs, and the workload. The OCIDs are exported for the manifests and recorded for the scenario.
start_byo() {
  local vcn igw rt sub nsg
  vcn="$(byo_ensure vcn "${byo_net}" --cidr-blocks '["10.200.0.0/16"]')"
  igw="$(byo_ensure internet-gateway "${byo_net}-igw" --vcn-id "${vcn}" --is-enabled true)"
  rt="$(byo_ensure route-table "${byo_net}-rt" --vcn-id "${vcn}" \
    --route-rules "[{\"destination\":\"0.0.0.0/0\",\"destinationType\":\"CIDR_BLOCK\",\"networkEntityId\":\"${igw}\"}]")"
  # A regional subnet (no availability domain) with the route table above and the VCN's default security list
  # (egress all, ingress ICMP and SSH: the group carries the rest).
  sub="$(byo_ensure subnet "${byo_net}" --vcn-id "${vcn}" --cidr-block 10.200.0.0/24 --route-table-id "${rt}")"
  nsg="$(byo_ensure nsg "${byo_net}-nodes" --vcn-id "${vcn}")"
  if [[ -z "$(getstate byo.rules)" ]]; then
    oc network nsg rules add --nsg-id "${nsg}" --security-rules '[
      {"direction":"INGRESS","protocol":"17","source":"0.0.0.0/0","sourceType":"CIDR_BLOCK",
       "udpOptions":{"destinationPortRange":{"min":41641,"max":41641}}},
      {"direction":"EGRESS","protocol":"all","destination":"0.0.0.0/0","destinationType":"CIDR_BLOCK"}]' >/dev/null
    setstate byo.rules added
  fi
  setstate byo.vcn "${vcn}"
  setstate byo.subnet "${sub}"
  setstate byo.nsg "${nsg}"
  E2E_BYO_SUBNET="${sub}" E2E_BYO_NSG="${nsg}"
  export E2E_BYO_SUBNET E2E_BYO_NSG
  render "${here}/manifests/nodeclass-use.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-use.yaml" | kubectl apply -f -
  probe e2e-use e2e-use "${E2E_TYPE_AMD64}" on-demand
}

start_create() {
  render "${here}/manifests/nodeclass-create.yaml" | kubectl apply -f -
  render "${here}/manifests/nodepool-create.yaml" | kubectl apply -f -
  probe e2e-create e2e-create "${E2E_TYPE_AMD64}" on-demand
}

# ----------------------------------------------------------- scenarios ------

scn_preflight() {
  for t in oci kubectl jq helm envsubst; do command -v "${t}" >/dev/null || fail "${t} is not installed"; done
  local subs ads
  subs="$(oc iam region-subscription list --tenancy-id "${OCI_TENANCY}" --output json)" ||
    fail "the OCI session does not work for the tenancy (or the policy lacks inspect tenancies)"
  assert_eq "the region ${OCI_REGION} is subscribed and READY" READY \
    "$(jq -r --arg r "${OCI_REGION}" '[(.data // [])[] | select(."region-name" == $r) | .status] | first // "absent"' <<<"${subs}")"
  ads="$(availability_domains)" || fail "the availability domains cannot be listed"
  [[ -n "${ads}" ]] || fail "the region ${OCI_REGION} has no availability domain"
  note "availability domains of ${OCI_REGION}: $(zone_names | paste -sd' ' -)"

  # 1. The Flatcar images (plan 25 section 3): the naming and tag contract in the compartment,
  # available, and registered for the shapes of the lane.
  local arch img name shapes t types=("${E2E_TYPE_AMD64}")
  IFS=, read -ra described <<<"${E2E_TYPES_DESCRIBED}"
  types+=("${described[@]}")
  for arch in amd64 arm64; do
    if [[ "${arch}" == arm64 ]]; then
      if [[ "${E2E_ARM64}" != true ]]; then
        note "arm64: skipped (E2E_ARM64 is not true: the arm64 image needs a capability schema and the Arm shapes added, docs/how-to-guides/oci-images.md)"
        continue
      fi
      types+=("${E2E_TYPE_ARM64}" "${E2E_TYPE_ARM64_ALT}")
    fi
    name="flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-${arch}"
    img="$(oc compute image list --compartment-id "${OCI_COMPARTMENT}" --display-name "${name}" --lifecycle-state AVAILABLE --all --output json |
      jq '[(.data // [])[]] | first // empty')" || fail "the images of the compartment cannot be listed"
    [[ -n "${img}" ]] || fail "the image ${name} does not exist (or is not AVAILABLE) in the compartment: import it (docs/how-to-guides/oci-images.md)"
    assert_eq "the ${arch} image's version tag" "${E2E_FLATCAR_VERSION}" "$(tag_of "${img}" flatcar-version)"
    assert_eq "the ${arch} image's arch tag" "${arch}" "$(tag_of "${img}" arch)"
    shapes="$(oc compute image-shape-compatibility-entry list --image-id "$(jq -r '.id' <<<"${img}")" --all --output json | jq -r '(.data // [])[].shape')" ||
      fail "the shape compatibility of ${name} cannot be listed"
    for t in "${types[@]}"; do
      t="${t#oci.}"
      [[ "${arch}" == arm64 ]] && [[ "${t}" != VM.Standard.A* ]] && continue
      [[ "${arch}" == amd64 ]] && [[ "${t}" == VM.Standard.A* ]] && continue
      grep -qxF -- "${t%.*}" <<<"${shapes}" || fail "the image ${name} is not registered for the shape ${t%.*}"
    done
  done

  # 2. The shapes are offered in an availability domain of the region.
  local ad found
  for t in "${types[@]}"; do
    found=no
    for ad in ${ads}; do
      t="${t#oci.}"
      if oc compute shape list --compartment-id "${OCI_COMPARTMENT}" --availability-domain "${ad}" --all --output json |
        jq -e --arg s "${t%.*}" '[(.data // [])[] | select(.shape == $s)] | length > 0' >/dev/null; then
        found=yes
        break
      fi
    done
    [[ "${found}" == yes ]] || fail "the shape ${t%.*} is not offered in any availability domain of ${OCI_REGION}"
  done

  # 3. The limits the suite needs (a new tenancy's default may not be enough).
  local limit used
  read -r limit used <<<"$(region_quota "${bucket_cores}")"
  [[ -n "${limit}" ]] || fail "the Limits service reports no ${bucket_cores}"
  [[ "${limit}" -ge "${E2E_MIN_OCPU}" ]] || fail "the limit ${bucket_cores} of ${OCI_REGION} is ${limit}; the suite needs ${E2E_MIN_OCPU}"
  [[ "${limit}" -ge $((used + E2E_MIN_OCPU)) ]] || note "the limit ${bucket_cores} is ${limit} with ${used} in use: less than ${E2E_MIN_OCPU} are free"
  read -r limit used <<<"$(region_quota "${bucket_memory}")"
  [[ "${limit}" -ge "${E2E_MIN_MEMORY_GB}" ]] || fail "the limit ${bucket_memory} of ${OCI_REGION} is ${limit}; the suite needs ${E2E_MIN_MEMORY_GB}"
  [[ "${limit}" -ge $((used + E2E_MIN_MEMORY_GB)) ]] || note "the limit ${bucket_memory} is ${limit} with ${used} in use: less than ${E2E_MIN_MEMORY_GB} are free"

  # 4. The controller and the NodeClasses.
  kubectl -n "${ns}" rollout status deploy/scarpenter --timeout=60s
  local nc classes=(e2e-amd64 e2e-drift)
  [[ "${E2E_ARM64}" != true ]] || classes+=(e2e-arm64)
  for nc in "${classes[@]}"; do
    assert_eq "scnodeclass/${nc} Ready" True "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
    assert_eq "scnodeclass/${nc} VendorReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].status}')"
    assert_eq "scnodeclass/${nc} ImagesReady" True \
      "$(kubectl get scnodeclass "${nc}" -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="ImagesReady")].status}')"
  done
  note "compartment of the lane, region ${OCI_REGION}"
}

# zone_names: the zone names of the region's availability domains, one per line.
zone_names() { availability_domains | while read -r ad; do zone_name "${ad}"; echo; done | sed '/^$/d'; }

scn_amd64() {
  start_amd64
  settle e2e-amd64
  local pod node claim iid uid j d price dns mdq
  pod="$(getstate e2e-amd64.pod)"
  node="$(getstate e2e-amd64.node)"
  claim="$(getstate e2e-amd64.claim)"
  iid="$(getstate e2e-amd64.iid)"
  uid="$(getstate e2e-amd64.uid)"

  log "node Ready, labels (overview section 5.5)"
  check_node_labels "${node}" "${E2E_TYPE_AMD64}" on-demand amd64 e2e-amd64
  assert_eq "vcpus (one OCPU is two vCPUs on x86)" 2 "$(node_label "${node}" "${label_domain}/vcpus")"
  assert_eq "memory-mib" 4096 "$(node_label "${node}" "${label_domain}/memory-mib")"

  log "the instance: tags, shapeConfig, metadata, availability domain, boot volume (plan 25 sections 3 and 5)"
  check_instance "${iid}" "${claim}" "${uid}" e2e-amd64 e2e-amd64
  j="$(instance_json "${iid}")"
  check_shape "${j}" "${E2E_TYPE_AMD64}"
  assert_eq "the instance's availability domain is the node's zone" "$(node_label "${node}" topology.kubernetes.io/zone)" \
    "$(zone_name "$(jq -r '."availability-domain"' <<<"${j}")")"
  assert_eq "no preemptible configuration on an on-demand instance" null "$(jq -c '."preemptible-instance-config"' <<<"${j}")"
  assert_eq "the image of the instance is the pinned release" "${E2E_FLATCAR_IMAGE_AMD64}" \
    "$(oc compute image get --image-id "$(jq -r '."source-details"."image-id"' <<<"${j}")" --output json | jq -r '.data."display-name"')"
  d="$(boot_volume_of "${j}")" || fail "the instance has no boot volume attachment"
  assert_eq "boot volume size" "${E2E_ROOT_GB}" "$(jq -r '."size-in-gbs"' <<<"${d}")"
  assert_eq "boot volume performance (VPUs per GB, balanced)" 10 "$(jq -r '."vpus-per-gb"' <<<"${d}")"
  # Whether OCI copies the instance's tags to its boot volume is unverified: recorded for the first live run.
  note "the boot volume's tag scarpenter-cluster: '$(tag_of "${d}" scarpenter-cluster)' (the sweeper's boot volume sweep relies on it)"

  log "the claim: price, bootstrap token, serving certificate"
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

  log "cluster DNS, and the metadata service (plan 25 section 5: user_data through the service only, no SSH keys)"
  dns="$(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')"
  assert_match "the kubelet's clusterDNS is the kube-dns Service" "${dns//./\\.}" "$(node_shell "${node}" "grep -A3 '^clusterDNS' /var/lib/kubelet/config.yaml" | tr '\n' ' ')"
  wait_until "a Pod resolves a cluster Service" 120 kubectl exec "${pod}" -- nslookup kubernetes.default.svc.cluster.local
  # The user_data is readable through the metadata service (from the node, whose own the data is). OCI's
  # service has no hop limit (IMDS v2 needs only a header): a Pod can read it too, which the reference
  # documents (the hardening metadata-from-pods is opt-in). The Pod's answer is recorded, not asserted:
  # it depends on the pod network's masquerade.
  mdq='wget -qO- -T 5 --header "Authorization: Bearer Oracle" http://169.254.169.254/opc/v2/instance'
  assert_eq "the metadata service answers on the node (instance OCID)" "${iid}" "$(node_pod_exec "${node}" "${mdq}/id" || true)"
  assert_match "the user_data is the base64 of the Ignition" '^eyJ' "$(node_pod_exec "${node}" "${mdq}/metadata/user_data" || true)"
  note "metadata service from a Pod (exposure, documented): '$(kubectl exec "${pod}" -- sh -c "${mdq}/id" 2>&1 | head -c 80 || true)'"
  note "the legacy endpoint /opc/v1 on the node (areLegacyImdsEndpointsDisabled): '$(node_pod_exec "${node}" 'wget -qO- -T 5 http://169.254.169.254/opc/v1/instance/id' 2>&1 | head -c 80 || true)'"
  assert_eq "no login users on the node (uid 1000 and up): the nodes carry no SSH keys" "" \
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

# scn_spot: the same size as preemptible capacity (plan 25 section 5): preemptibleInstanceConfig with the
# action TERMINATE and the boot volume not preserved, so that a reclaimed instance leaves nothing behind.
scn_spot() {
  start_spot
  settle e2e-spot
  local pod node claim iid uid j
  pod="$(getstate e2e-spot.pod)"
  node="$(getstate e2e-spot.node)"
  claim="$(getstate e2e-spot.claim)"
  iid="$(getstate e2e-spot.iid)"
  uid="$(getstate e2e-spot.uid)"
  check_node_labels "${node}" "${E2E_TYPE_AMD64}" spot amd64 e2e-spot
  check_instance "${iid}" "${claim}" "${uid}" e2e-spot e2e-amd64
  j="$(instance_json "${iid}")"
  check_shape "${j}" "${E2E_TYPE_AMD64}"
  assert_eq "preemption action" TERMINATE "$(jq -r '."preemptible-instance-config"."preemption-action".type' <<<"${j}")"
  assert_eq "the boot volume is not preserved on preemption" false "$(jq -r '."preemptible-instance-config"."preemption-action"."preserve-boot-volume"' <<<"${j}")"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  note "spot claim ${claim}: ready after $(ready_seconds "${claim}" "${node}")s"
}

# scn_arm64: optional (E2E_ARM64=true): the arm64 image on A1, or on A2 where A1's capacity is short.
scn_arm64() {
  if [[ "${E2E_ARM64}" != true ]]; then
    note "arm64: skipped (E2E_ARM64 is not true: the arm64 image needs a capability schema and the Arm shapes added)"
    return 0
  fi
  start_arm64
  settle e2e-arm64
  local pod node claim iid uid j t
  pod="$(getstate e2e-arm64.pod)"
  node="$(getstate e2e-arm64.node)"
  claim="$(getstate e2e-arm64.claim)"
  iid="$(getstate e2e-arm64.iid)"
  uid="$(getstate e2e-arm64.uid)"
  t="$(node_label "${node}" node.kubernetes.io/instance-type)"
  assert_match "the type is one of the Arm types of the pool" "^(${E2E_TYPE_ARM64}|${E2E_TYPE_ARM64_ALT})\$" "${t}"
  note "arm64 got ${t}"
  check_node_labels "${node}" "${t}" on-demand arm64 e2e-arm64
  check_instance "${iid}" "${claim}" "${uid}" e2e-arm64 e2e-arm64
  j="$(instance_json "${iid}")"
  check_shape "${j}" "${t}"
  # The five launch options of a Flatcar arm64 image (plan 25 section 5).
  assert_eq "firmware" UEFI_64 "$(jq -r '."launch-options".firmware' <<<"${j}")"
  assert_eq "network type" PARAVIRTUALIZED "$(jq -r '."launch-options"."network-type"' <<<"${j}")"
  assert_eq "boot volume type" PARAVIRTUALIZED "$(jq -r '."launch-options"."boot-volume-type"' <<<"${j}")"
  assert_eq "remote data volume type" PARAVIRTUALIZED "$(jq -r '."launch-options"."remote-data-volume-type"' <<<"${j}")"
  assert_eq "consistent volume naming" true "$(jq -r '."launch-options"."is-consistent-volume-naming-enabled"' <<<"${j}")"
  assert_eq "the image of the instance is the pinned arm64 release" "${E2E_FLATCAR_IMAGE_ARM64}" \
    "$(oc compute image get --image-id "$(jq -r '."source-details"."image-id"' <<<"${j}")" --output json | jq -r '.data."display-name"')"
  wait_until "kubectl logs" 120 kubectl logs "${pod}" --tail=5
  assert_eq "kubectl exec" ok "$(kubectl exec "${pod}" -- echo ok)"
  assert_eq "tailscaled, the join unit and the kubelet are active (arm64 sysext)" "active,active,active" \
    "$(node_shell "${node}" 'systemctl is-active tailscaled scarpenter-tailscale-up.service kubelet' | paste -sd, -)"
  drop_nodeshells
}

# The sized types of the pool of the described scenario: the one with 2 vCPUs and more than 6000 MiB is 1-8.
scn_described() {
  start_described
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-described "" pod_running app=e2e-described
  local pod node claim iid t vcpus mem
  pod="$(kubectl get pod -l app=e2e-described -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  claim="$(claim_of_node "${node}")"
  iid="$(instance_of_claim "${claim}")"
  t="$(node_label "${node}" node.kubernetes.io/instance-type)"
  vcpus="$(node_label "${node}" "${label_domain}/vcpus")"
  mem="$(node_label "${node}" "${label_domain}/memory-mib")"
  note "the described workload got ${t}: ${vcpus} vCPUs, ${mem} MiB"
  assert_match "the type is one of the allowed test types" ",${t}," ",${E2E_TYPES_DESCRIBED},"
  [[ "${vcpus}" -gt 1 && "${vcpus}" -lt 3 ]] || fail "vcpus ${vcpus} is outside (1, 3)"
  [[ "${mem}" -gt 6000 ]] || fail "memory ${mem} MiB is not above 6000"
  check_shape "$(instance_json "${iid}")" "${t}"
  # The same facts from Compute, independent of the catalog: two vCPUs are one OCPU on x86.
  assert_eq "Compute's OCPUs of the instance" $((vcpus / 2)) "$(num "$(instance_json "${iid}" | jq -r '."shape-config".ocpus')")"
}

scn_byo() {
  start_byo
  wait_until "ResourcesReady on the NodeClass" 180 is_value True \
    kubectl get scnodeclass e2e-use -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="ResourcesReady")].status}'
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-use "" pod_running app=e2e-use
  local pod node iid vnic sub nsg snapshot_before
  sub="$(getstate byo.subnet)"
  nsg="$(getstate byo.nsg)"
  pod="$(kubectl get pod -l app=e2e-use -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  iid="$(instance_of_claim "$(claim_of_node "${node}")")"
  vnic="$(vnic_json "${iid}")"
  assert_eq "the instance is in the brought subnet" "${sub}" "$(jq -r '."subnet-id"' <<<"${vnic}")"
  assert_eq "the instance carries the brought group (and no other)" "${nsg}" "$(jq -r '(."nsg-ids" // []) | join(",")' <<<"${vnic}")"
  assert_nonempty "public IPv4" "$(jq -r '."public-ip" // empty' <<<"${vnic}")"
  snapshot_before="$(byo_snapshot "${sub}" "${nsg}")"

  log "SCarpenter must not touch what it did not create"
  drop_nodeshells
  kubectl delete deployment e2e-use --wait=false
  wait_until "the instance gone" 300 is_empty instances_of "${iid}"
  kubectl delete nodepool e2e-use --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-use --wait=true --timeout=120s
  # Past a garbage collection (1 m), a resource check (30 s) and the status controller:
  # a `use` reference is never modified or deleted.
  sleep 120
  assert_eq "the VCN still exists" AVAILABLE "$(oc network vcn get --vcn-id "$(getstate byo.vcn)" --output json | jq -r '.data."lifecycle-state"')"
  assert_eq "the subnet and the group still exist, unchanged" "${snapshot_before}" "$(byo_snapshot "${sub}" "${nsg}")"
  note "the brought VCN ${byo_net}, its gateway, route table, subnet and group are deleted by the teardown (the sweeper, by the lane's tag)"
}

# byo_snapshot <subnet> <group>: the parts of the brought subnet and group a change by SCarpenter would alter.
byo_snapshot() {
  {
    oc network subnet get --subnet-id "$1" --output json | jq -S -c '.data | {"display-name", "cidr-block", "route-table-id", "security-list-ids", "freeform-tags", "lifecycle-state"}'
    oc network nsg rules list --nsg-id "$2" --all --output json | jq -S -c '[(.data // [])[] | del(."time-created", .id)] | sort_by(.direction, .protocol)'
    oc network nsg get --nsg-id "$2" --output json | jq -S -c '.data | {"display-name", "freeform-tags", "lifecycle-state"}'
  } | sha256sum | cut -d' ' -f1
}

# scn_create: no `use`: SCarpenter creates the VCN, its internet gateway, route table, security list and
# regional subnet, and the NodeClass's group; the node uses them, and they go by their lifecycle (idleTTL)
# once the NodeClass is deleted. Every part carries the ownership tags (cluster, managed-by, nodeclass,
# resource-kind) and a display name that starts with scarpenter-<cluster>- (ADR-018).
scn_create() {
  start_create
  wait_node "the Pod running" "${E2E_NODE_TIMEOUT}" app=e2e-create "" pod_running app=e2e-create
  local pod node iid vnic vcn sn g pfx="scarpenter-${E2E_CLUSTER}-"
  pod="$(kubectl get pod -l app=e2e-create -o jsonpath='{.items[0].metadata.name}')"
  node="$(pod_node "${pod}")"
  iid="$(instance_of_claim "$(claim_of_node "${node}")")"
  vnic="$(vnic_json "${iid}")"

  log "the managed network: a VCN of the default range, its parts, named and tagged by SCarpenter"
  vcn="$(managed_json vcn | jq -c '.[0]')"
  assert_eq "managed VCNs of the cluster" 1 "$(managed_json vcn | jq 'length')"
  assert_match "the VCN's name" "^${pfx}" "$(jq -r '."display-name"' <<<"${vcn}")"
  assert_eq "tag managed-by on the VCN" scarpenter "$(tag_of "${vcn}" scarpenter-managed-by)"
  assert_eq "tag cluster on the VCN" "${E2E_CLUSTER}" "$(tag_of "${vcn}" scarpenter-cluster)"
  assert_nonempty "tag resource-kind on the VCN" "$(tag_of "${vcn}" scarpenter-resource-kind)"
  assert_eq "the VCN's range" 10.240.0.0/16 "$(jq -r '."cidr-block"' <<<"${vcn}")"
  for part in internet-gateway route-table security-list; do
    assert_eq "one managed ${part} in the VCN" 1 "$(managed_json "${part}" | jq --arg v "$(jq -r .id <<<"${vcn}")" '[.[] | select(."vcn-id" == $v)] | length')"
    assert_eq "every managed ${part} is marked" true "$(managed_json "${part}" | jq -r 'all(.[]; ."freeform-tags"["scarpenter-managed-by"] == "scarpenter")')"
  done

  log "the regional subnet and the group of the NodeClass"
  sn="$(managed_json subnet | jq -c --arg v "$(jq -r .id <<<"${vcn}")" '[.[] | select(."vcn-id" == $v)] | first')"
  assert_eq "the subnet is regional (no availability domain)" null "$(jq -c '."availability-domain"' <<<"${sn}")"
  assert_eq "the subnet is the first /20 of the VCN" 10.240.0.0/20 "$(jq -r '."cidr-block"' <<<"${sn}")"
  assert_eq "the instance is in the managed subnet" "$(jq -r .id <<<"${sn}")" "$(jq -r '."subnet-id"' <<<"${vnic}")"
  g="$(managed_json nsg | jq -c '[.[] | select(."freeform-tags"["scarpenter-nodeclass"] == "e2e-create")] | first')"
  assert_nonempty "the managed group of e2e-create" "$(jq -r '.id // empty' <<<"${g}")"
  assert_eq "the group is in the managed VCN" "$(jq -r .id <<<"${vcn}")" "$(jq -r '."vcn-id"' <<<"${g}")"
  assert_eq "the instance carries the managed group" true "$(jq -r --arg g "$(jq -r .id <<<"${g}")" '(."nsg-ids" // []) | index($g) != null' <<<"${vnic}")"

  log "delete the workload, the pool and the NodeClass: the group goes by its lifecycle"
  drop_nodeshells
  kubectl delete deployment e2e-create --wait=false
  wait_until "the instance gone" 300 is_empty instances_of "${iid}"
  kubectl delete nodepool e2e-create --wait=true --timeout=300s
  kubectl delete scnodeclass e2e-create --wait=true --timeout=300s
  # The group of the class (3 minutes idle), then the VCN and its parts once no class uses them: the
  # other NodeClasses of the run use the same VCN, so it stays until the uninstall scenario; the group
  # of this class does not.
  wait_until "the managed group of e2e-create deleted" 600 is_empty managed_groups_of e2e-create
}

# managed_json <oci network collection>: SCarpenter's resources of the cluster in the collection (the
# ownership tags and the name prefix, all required), not terminated, as a JSON list.
managed_json() {
  oc network "$1" list --compartment-id "${OCI_COMPARTMENT}" --all --output json |
    jq --arg c "${E2E_CLUSTER}" --arg p "scarpenter-${E2E_CLUSTER}-" '[(.data // [])[]
      | select(."lifecycle-state" != "TERMINATED" and ."lifecycle-state" != "TERMINATING")
      | select(."freeform-tags"["scarpenter-cluster"] == $c and ."freeform-tags"["scarpenter-managed-by"] == "scarpenter" and ((."display-name" // "") | startswith($p)))]'
}

managed_groups_of() { # managed_groups_of <nodeclass>: the managed groups of the NodeClass
  managed_json nsg | jq -r --arg n "$1" '.[] | select(."freeform-tags"["scarpenter-nodeclass"] == $n) | ."display-name"'
}

# managed_names <oci network collection>: the display names of SCarpenter's resources of the cluster (the
# byo resources carry another prefix and stay for the teardown).
managed_names() { managed_json "$1" | jq -r '.[]."display-name"'; }

# negative: NodeClasses the driver must refuse, and nothing is launched (ADR-022).
scn_negative() {
  render "${here}/manifests/nodeclass-negative.yaml" | kubectl apply -f -
  wait_until "a NodeClass with a region the driver cannot use is refused" 180 is_value False \
    kubectl get scnodeclass e2e-badregion -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].status}'
  note "e2e-badregion: VendorReady=False, reason $(kubectl get scnodeclass e2e-badregion -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].reason}'): $(kubectl get scnodeclass e2e-badregion -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].message}')"
  assert_match "the message names the region" "xx-nowhere-1" \
    "$(kubectl get scnodeclass e2e-badregion -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].message}')"
  # publicIPv4: false with the managed network, which has no NAT gateway: the driver refuses it.
  wait_until "e2e-noipv4 is refused" 180 is_value False \
    kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].status}'
  note "e2e-noipv4 (publicIPv4: false): VendorReady=$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].status}')/$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].reason}'): $(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].message}')"
  assert_match "the message names publicIPv4" "publicIPv4" \
    "$(kubectl get scnodeclass e2e-noipv4 -o jsonpath='{.status.vendors.oci.conditions[?(@.type=="VendorReady")].message}')"
  assert_eq "no instance was launched for them" "" "$(instances_of_classes e2e-badregion e2e-noipv4)"
  kubectl delete scnodeclass e2e-badregion e2e-noipv4 --wait=true --timeout=120s
}

# instances_of_classes <nodeclass>...: the OCIDs of the instances (not terminated) of the cluster tagged with one of the NodeClasses.
instances_of_classes() {
  local want
  want="$(printf '%s\n' "$@" | jq -R . | jq -sc .)"
  oc compute instance list --compartment-id "${OCI_COMPARTMENT}" --all --output json |
    jq -r --argjson want "${want}" '(.data // [])[] | select(."lifecycle-state" != "TERMINATED")
      | select(."freeform-tags"["scarpenter-nodeclass"] as $n | $want | index($n)) | .id'
}

# quota (overview section 13.4, plan 25 section 7): what the controller reports equals what the Limits
# service says, bucket by bucket (the usage of the region is the sum over the availability domains, the
# limit the usage plus the largest availability). Run with the nodes of the other scenarios up and no
# launch in flight.
scn_quota() {
  settle_fixtures
  local m metric
  m="$(mktemp)"
  log "the limits of the E5 buckets equal the Limits service"
  local timeout=240
  for metric in "${bucket_cores}" "${bucket_memory}"; do
    wait_until "scarpenter_quota_limit ${metric} equals the Limits service" "${timeout}" quota_field_is "${m}" limit "${metric}"
    timeout=90 # the first one waited for the controller's first limits refresh
  done
  [[ -n "$(metric_value "${m}" scarpenter_quota_limit "${OCI_REGION}" "${bucket_cores}")" ]] || fail "no scarpenter_quota_limit series for the ${bucket_cores} bucket"

  log "usage: what the controller reports is what the Limits service says is used"
  for metric in "${bucket_cores}" "${bucket_memory}"; do
    wait_until "scarpenter_quota_used ${metric} equals the Limits service" 300 quota_field_is "${m}" used "${metric}"
  done
  # The consumption mapping (plan 25 section 7): the E5 nodes count their OCPUs and GB. The on-demand and the
  # spot node (the same type; preemptible capacity is counted like any other until S9 says otherwise) use
  # at least 2 OCPUs and 8 GB.
  local limit used
  read -r limit used <<<"$(region_quota "${bucket_cores}")"
  [[ "${used}" -ge 2 ]] || fail "two 1-OCPU nodes are running but the ${bucket_cores} usage is ${used}"
  wait_until "the in-flight reservations are gone" 240 quota_reserved_zero "${m}" "${bucket_cores}"
  note "the quota after the nodes of the quota scenario: ${bucket_cores} $(region_quota "${bucket_cores}"), ${bucket_memory} $(region_quota "${bucket_memory}") (limit used)"

  log "the launched types are offered in the availability domain they run in"
  local t ad
  for t in e2e-amd64 e2e-spot; do
    ad="$(jq -r '."availability-domain"' <<<"$(instance_json "$(getstate "${t}.iid")")")"
    assert_eq "${E2E_TYPE_AMD64#oci.} (its shape) in the shapes of ${ad}" 1 \
      "$(oc compute shape list --compartment-id "${OCI_COMPARTMENT}" --availability-domain "${ad}" --all --output json |
        jq --arg s "${E2E_TYPE_AMD64#oci.}" '[(.data // [])[] | select(.shape == ($s | sub("\\.[0-9]+-[0-9]+$"; "")))] | length')"
  done
  rm -f "${m}"
}

# quota_field_is <metrics file> limit|used <limit name>: the controller's series equals the number the
# Limits service reports now (a bucket the controller has no series for is not compared).
quota_field_is() {
  local want col=2 got
  [[ "$2" == limit ]] && col=1
  want="$(region_quota "$3" | cut -d' ' -f"${col}")"
  metrics >"$1"
  got="$(metric_value "$1" "scarpenter_quota_$2" "${OCI_REGION}" "$3")"
  [[ -n "${got}" ]] || return 0
  [[ "$(num "${got}")" == "$(num "${want}")" ]]
}

quota_reserved_zero() { # quota_reserved_zero <metrics file> <bucket>
  metrics >"$1"
  [[ "$(num "$(metric_value "$1" scarpenter_quota_reserved "${OCI_REGION}" "$2")")" == 0 ]]
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
# the vendor block (the boot volume size) replaces the node, and the controller is killed while the
# replacement's Create is in flight. OCI has no profiles in v1, so there is no "a profile no node uses
# drifts nothing" half.
scn_drift() {
  start_drift
  settle e2e-drift
  local old_claim old_node old_iid old_pod
  old_claim="$(getstate e2e-drift.claim)"
  old_node="$(getstate e2e-drift.node)"
  old_iid="$(getstate e2e-drift.iid)"
  old_pod="$(getstate e2e-drift.pod)"

  log "the boot volume size changes: the node is replaced"
  kubectl patch scnodeclass e2e-drift --type merge -p "{\"spec\":{\"vendors\":{\"oci\":{\"storage\":{\"root\":{\"sizeGb\":${E2E_ROOT_GB_DRIFT}}}}}}}"
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
  assert_eq "the replacement's boot volume (the new size)" "${E2E_ROOT_GB_DRIFT}" "$(jq -r '."size-in-gbs"' <<<"$(boot_volume_of "${j}")")"
  wait_until "the old instance gone" 300 is_empty instances_of "${old_iid}"
  wait_until "the old NodeClaim gone" 120 is_empty kubectl get nodeclaim "${old_claim}" -o name --ignore-not-found
  wait_until "the old Node gone" 120 is_empty kubectl get node "${old_node}" -o name --ignore-not-found
  assert_eq "the old Pod is not the new one" "$([[ "${pod}" != "${old_pod}" ]] && echo yes)" yes
  assert_no_orphans
  # Not opportunistic: a terminated instance stays listed for about twelve hours, so every instance ever
  # launched for the replacement claim is still visible here, even one the garbage collector already
  # deleted as an orphan. More than one is a second launch for the same claim (a replay that did not
  # find the first launch).
  local repl ever
  repl="$(getstate drift.replacement)"
  ever="$(instances_ever_of_claim "${repl}" | wc -w | tr -d ' ')"
  [[ "${ever}" -le 1 ]] || fail "the claim ${repl} was launched ${ever} times (a second LaunchInstance after the restart)"
  setstate e2e-drift.node "${node}"
  setstate e2e-drift.claim "${claim}"
  setstate e2e-drift.iid "${iid}"
}

# instances_ever_of_claim <claim>: the OCIDs of the instances tagged with the claim, in any state
# (a terminated instance stays listed for about twelve hours).
instances_ever_of_claim() {
  oc compute instance list --compartment-id "${OCI_COMPARTMENT}" --all --output json |
    jq -r --arg c "$1" '(.data // [])[] | select(."freeform-tags"["scarpenter-nodeclaim"] == $c) | .id'
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
    # The one assertion that can fail on the window: a second launch for the claim.
    assert_eq "the replay of Create found the first launch's instance (no second launch)" "${before}" "${after}"
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

# instances_of_claim <claim>: the OCIDs of the instances (not terminated) tagged with the claim.
instances_of_claim() {
  instances_ever_of_claim "$1" | while read -r id; do
    [[ "$(oc compute instance get --instance-id "${id}" --output json | jq -r '.data."lifecycle-state"')" != TERMINATED ]] && echo "${id}"
  done | xargs
}

# instances_of <ocid>...: those of the instances that are not terminated (a terminated one stays listed).
instances_of() {
  local i s out=""
  for i in "$@"; do
    s="$(oc compute instance get --instance-id "${i}" --output json | jq -r '.data."lifecycle-state"')" || return 1
    [[ "${s}" == TERMINATED ]] || out+="${i} "
  done
  printf '%s' "${out}"
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
# (WhenEmpty, 0 s); the nodes, NodeClaims and bootstrap tokens go too, and so do the boot volumes;
# the quota usage falls with the instances.
scn_scalezero() {
  local t0 elapsed
  drop_nodeshells
  release_do_not_disrupt
  kubectl delete deployments,statefulsets -l scarpenter-e2e=true --wait=false
  t0="$(now)"
  kubectl delete pods -l scarpenter-e2e=true --wait=false --ignore-not-found >/dev/null || true
  wait_until "every instance terminating or gone" "${E2E_SCALE_DOWN_DEADLINE}" is_empty alive_instances
  note "scale to zero: every instance was terminating (the terminate was issued) $(($(now) - t0))s after the Pods' deletion"
  # The deadline counts from the Pods' deletion for both.
  wait_until "every instance of the cluster terminated" "$((E2E_SCALE_DOWN_DEADLINE > $(now) - t0 ? E2E_SCALE_DOWN_DEADLINE - ($(now) - t0) : 1))" is_empty cluster_instances
  elapsed=$(($(now) - t0))
  note "scale to zero: every instance terminated ${elapsed}s after the Pods' deletion (deadline ${E2E_SCALE_DOWN_DEADLINE}s)"
  wait_until "no node of a NodePool left" 120 is_empty kubectl get nodes -l karpenter.sh/nodepool -o name
  wait_until "no NodeClaim left" 120 is_empty kubectl get nodeclaims -o name
  wait_until "no bootstrap token Secret left" 120 is_empty kubectl -n kube-system get secrets -l "${label_domain}/nodeclaim" -o name
  # Boot volumes go with their instance (preserveBootVolume=false): the leak a spot reclaim or a failed
  # launch would leave is a volume without an instance.
  wait_until "no boot volume created by the run is left" 180 is_empty new_boot_volumes
  log "the quota usage follows the instances down"
  local m metric
  m="$(mktemp)"
  for metric in "${bucket_cores}" "${bucket_memory}"; do
    wait_until "the ${metric} usage equals the Limits service" $((E2E_QUOTA_REFRESH * 4 + 60)) quota_field_is "${m}" used "${metric}"
  done
  wait_until "no reservation is left" 120 quota_reserved_zero "${m}" "${bucket_cores}"
  rm -f "${m}"
}

# uninstall: the procedure of docs/how-to-guides/install.md#uninstall, on the live tenancy.
scn_uninstall() {
  log "1. SCarpenter's NodePools (selected by nodeClassRef.group), then wait for the NodeClaims"
  kjson nodepools | jq -r '.items[] | select(.spec.template.spec.nodeClassRef.group == "scarpenter.sparecores.com") | .metadata.name' |
    xargs -r kubectl delete nodepool --wait=true --timeout=300s
  wait_until "no NodeClaim of SCarpenter left" 300 is_value 0 scarpenter_claims
  log "2. the SCNodeClasses; the managed resources go by their lifecycle"
  kubectl delete scnodeclass --all --wait=true --timeout=300s
  wait_until "no instance of the cluster" 300 is_empty cluster_instances
  # With the NodeClasses gone the managed groups, subnets and VCNs fall under the default
  # idle lifetime (managedResources.defaultTTL, 5 minutes in the lane's values; 24 hours by
  # default), parts in the order of the driver's deletion.
  wait_until "no managed network security group left" 900 is_empty managed_names nsg
  wait_until "no managed subnet left" 300 is_empty managed_names subnet
  wait_until "no managed route table left" 300 is_empty managed_names route-table
  wait_until "no managed internet gateway left" 300 is_empty managed_names internet-gateway
  wait_until "no managed security list left" 300 is_empty managed_names security-list
  wait_until "no managed VCN left" 300 is_empty managed_names vcn
  log "3. helm uninstall"
  helm uninstall scarpenter --namespace "${ns}" --wait --timeout 3m
  wait_until "the controller gone" 120 is_empty kubectl -n "${ns}" get pods -l app.kubernetes.io/name=scarpenter -o name
  # What the uninstall leaves behind (the CRDs and what the user created) is listed in
  # the how-to; what carries the cluster's name or tag is checked by the teardown.
}

# scarpenter_claims: how many NodeClaims belong to an SCNodeClass.
scarpenter_claims() {
  kjson nodeclaims | jq '[.items[] | select(.spec.nodeClassRef.group == "scarpenter.sparecores.com")] | length'
}

# --------------------------------------------------------------- runner -----

all_scenarios=(amd64 spot arm64 described byo create negative quota drift scalezero uninstall)
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
