#!/usr/bin/env bash
# Prints the permissions of the L5 lane's CI identity (docs/how-to-guides/testing.md#the-l5-lane)
# as custom role definitions (YAML, for `gcloud iam roles create --file`) and as the gcloud
# commands that create the roles and bind them with IAM conditions. Nothing here calls Google.
#
#   test/e2e-gcp/ci-policy.sh role reads|regional|global     one role definition (YAML)
#   test/e2e-gcp/ci-policy.sh bindings <project id> <principal> [region]
#                                                            the gcloud commands
#
# The permissions are the driver's (docs/plans/v1/wip/16-gcp.md section 8: instances, disks, images,
# subnetworks, networks, firewall rules, resource policies, the reads and the operations)
# plus what the lane's scenarios and diagnostics need beyond it. No predefined role: the
# instance admin role is wider (all-disk powers, IAM-adjacent permissions). Three roles,
# because one IAM condition belongs to one binding:
#
#   scarpenterCiReads     get and list calls (and the serial console): no condition, since a list
#                         is checked against the project and no resource name can be tested
#   scarpenterCiRegional  the writes on instances, disks, subnetworks and resource policies:
#                         conditioned on the resource name, projects/<project>/zones/<region>-* or
#                         projects/<project>/regions/<region>/*, so that the identity writes
#                         nowhere but the lane's region
#   scarpenterCiGlobal    the writes on networks and firewall rules, which are global: conditioned
#                         on the name prefix scarpenter- (the managed resources scarpenter-<cluster>-*
#                         and the lane's brought scarpenter-e2e-byo-*)
#
# What GCP cannot do, unlike the AWS lane: an IAM condition tests the resource name, type and
# resource-manager tags, not labels (so no "only with the cluster label" wall: the driver's own
# ownership check is the wall, and a dedicated project is the boundary) and not the machine
# type (so no deny of other types: the pools name the types, the vCPU quotas of the project are
# the ceiling). That every method supports a name condition is UNVERIFIED; the first live run
# reads the denials in the audit log. The create of a resource is checked against the name the
# request gives it, which is the case the prefix condition relies on.
set -euo pipefail

# The permissions, by role.
reads=(
  compute.disks.get compute.disks.list
  compute.diskTypes.get compute.diskTypes.list
  compute.firewalls.get compute.firewalls.list
  compute.globalOperations.get compute.globalOperations.list
  compute.images.get compute.images.getFromFamily compute.images.list compute.images.useReadOnly
  compute.instances.get compute.instances.getSerialPortOutput compute.instances.list
  compute.machineTypes.get compute.machineTypes.list
  compute.networks.get compute.networks.list compute.networks.use
  compute.projects.get
  compute.regionOperations.get compute.regionOperations.list
  compute.regions.get compute.regions.list
  compute.resourcePolicies.get compute.resourcePolicies.list
  compute.subnetworks.get compute.subnetworks.list
  compute.zoneOperations.get compute.zoneOperations.list
  compute.zones.get compute.zones.list
)
regional=(
  compute.disks.create compute.disks.delete compute.disks.setLabels compute.disks.use
  compute.instances.create compute.instances.delete
  compute.instances.setLabels compute.instances.setMetadata compute.instances.setScheduling
  compute.instances.setServiceAccount compute.instances.setTags
  compute.resourcePolicies.create compute.resourcePolicies.delete compute.resourcePolicies.use
  compute.subnetworks.create compute.subnetworks.delete compute.subnetworks.use compute.subnetworks.useExternalIp
)
global=(
  compute.firewalls.create compute.firewalls.delete
  compute.networks.create compute.networks.delete compute.networks.updatePolicy
)

# sq <text>: the text in single quotes, for the commands that are printed.
sq() { printf "'%s'" "${1//\'/\'\\\'\'}"; }

usage() {
  echo "usage: $0 role reads|regional|global" >&2
  echo "       $0 bindings <project id> <principal> [region]" >&2
  exit 2
}

# role_title <name>
role_title() {
  case "$1" in
    reads) echo "SCarpenter CI reads" ;;
    regional) echo "SCarpenter CI regional writes" ;;
    global) echo "SCarpenter CI global network writes" ;;
  esac
}
role_description() {
  case "$1" in
    reads) echo "Reads of the L5 lane's CI identity (get and list, the serial console); no condition." ;;
    regional) echo "Writes of the L5 lane's CI identity on instances, disks, subnetworks and resource policies; bind it with a condition on the lane's region." ;;
    global) echo "Writes of the L5 lane's CI identity on networks and firewall rules; bind it with a condition on the name prefix scarpenter-." ;;
  esac
}
role_permissions() { # role_permissions <name>: one per line, sorted
  case "$1" in
    reads) printf '%s\n' "${reads[@]}" ;;
    regional) printf '%s\n' "${regional[@]}" ;;
    global) printf '%s\n' "${global[@]}" ;;
    *) usage ;;
  esac | LC_ALL=C sort
}
role_id() { case "$1" in reads) echo scarpenterCiReads ;; regional) echo scarpenterCiRegional ;; global) echo scarpenterCiGlobal ;; esac; }

cmd_role() {
  local name="${1:-}" p
  [[ "${name}" =~ ^(reads|regional|global)$ ]] || usage
  echo "title: $(role_title "${name}")"
  echo "description: $(role_description "${name}")"
  echo "stage: GA"
  echo "includedPermissions:"
  while read -r p; do echo "- ${p}"; done < <(role_permissions "${name}")
}

cmd_bindings() {
  local project="${1:-}" principal="${2:-}" region="${3:-us-central1}" r
  [[ -n "${project}" && -n "${principal}" ]] || usage
  [[ "${project}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || {
    echo "the project id must be a Google Cloud project id (6 to 30 characters, lower case letters, digits, hyphens)" >&2
    exit 2
  }
  [[ "${principal}" =~ ^(principal|principalSet|serviceAccount|group|user)[:] ]] || {
    echo "the principal must look like principalSet://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/attribute.<name>/<value>" >&2
    exit 2
  }
  [[ "${region}" =~ ^[a-z]+(-[a-z]+)*[0-9]+$ ]] || {
    echo "the region must look like us-central1" >&2
    exit 2
  }
  # The conditions (CEL, https://cloud.google.com/iam/docs/conditions-overview).
  local cond_regional cond_global
  cond_regional="resource.name.startsWith(\"projects/${project}/zones/${region}-\") || resource.name.startsWith(\"projects/${project}/regions/${region}/\")"
  cond_global="resource.name.startsWith(\"projects/${project}/global/networks/scarpenter-\") || resource.name.startsWith(\"projects/${project}/global/firewalls/scarpenter-\")"
  echo "# The L5 lane's CI identity in the project ${project}, writes limited to ${region}."
  echo "# Review before running; the custom roles are created once, the bindings are the access."
  for r in reads regional global; do
    printf 'gcloud iam roles create %s --project=%s --title=%s \\\n  --description=%s --stage=GA \\\n  --permissions=%s\n' \
      "$(role_id "${r}")" "${project}" "$(sq "$(role_title "${r}")")" "$(sq "$(role_description "${r}")")" "$(role_permissions "${r}" | paste -sd, -)"
  done
  printf 'gcloud projects add-iam-policy-binding %s --member=%s --role=%s --condition=None\n' \
    "${project}" "$(sq "${principal}")" "projects/${project}/roles/$(role_id reads)"
  printf 'gcloud projects add-iam-policy-binding %s --member=%s --role=%s \\\n  --condition=%s\n' \
    "${project}" "$(sq "${principal}")" "projects/${project}/roles/$(role_id regional)" \
    "$(sq "expression=${cond_regional},title=scarpenter-ci-region-${region},description=Writes in ${region} only")"
  printf 'gcloud projects add-iam-policy-binding %s --member=%s --role=%s \\\n  --condition=%s\n' \
    "${project}" "$(sq "${principal}")" "projects/${project}/roles/$(role_id global)" \
    "$(sq "expression=${cond_global},title=scarpenter-ci-network-names,description=Networks and firewall rules named scarpenter- only")"
}

case "${1:-}" in
  role)
    shift
    cmd_role "$@"
    ;;
  bindings)
    shift
    cmd_bindings "$@"
    ;;
  *) usage ;;
esac
