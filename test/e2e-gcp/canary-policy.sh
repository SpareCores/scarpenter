#!/usr/bin/env bash
# Prints the permissions of the canary's READ-ONLY identity
# (docs/how-to-guides/testing-and-releasing.md#the-canary) as a custom role definition (YAML,
# for `gcloud iam roles create --file`) and as the gcloud commands that create and bind it:
#
#   test/e2e-gcp/canary-policy.sh role
#   test/e2e-gcp/canary-policy.sh bindings <project id> <principal>
#
# The canary (test/canary, vendor gcp) runs the GCP driver's read paths: the regions and zones,
# the Flatcar images, the region's quotas (regions.get and projects.get), the machine type
# offerings, and the list of the cluster's instances that the quota snapshot counts. This is exactly the set of permissions of the calls it makes (canary_test.go runs it
# against the in-memory Compute API and compares the calls it made with this output) and nothing
# else: no permission outside get and list. A list is checked against the project, so no IAM
# condition limits it to the lane's region (the AWS policy's region condition has no equivalent):
# the role is read-only, which is the wall. Nothing here calls Google.
set -euo pipefail

permissions=(
  compute.images.get
  compute.instances.list
  compute.machineTypes.list
  compute.projects.get
  compute.regions.get
  compute.regions.list
  compute.zones.list
)

# sq <text>: the text in single quotes, for the commands that are printed.
sq() { printf "'%s'" "${1//\'/\'\\\'\'}"; }

usage() {
  echo "usage: $0 role" >&2
  echo "       $0 bindings <project id> <principal>" >&2
  exit 2
}

perms() { printf '%s\n' "${permissions[@]}" | LC_ALL=C sort; }

cmd_role() {
  local p
  echo "title: SCarpenter canary reads"
  echo "description: Read-only calls of the L5 canary: regions, zones, machine types, images and quotas. No write permission."
  echo "stage: GA"
  echo "includedPermissions:"
  while read -r p; do echo "- ${p}"; done < <(perms)
}

cmd_bindings() {
  local project="${1:-}" principal="${2:-}"
  [[ -n "${project}" && -n "${principal}" ]] || usage
  [[ "${project}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || {
    echo "the project id must be a Google Cloud project id (6 to 30 characters, lower case letters, digits, hyphens)" >&2
    exit 2
  }
  [[ "${principal}" =~ ^(principal|principalSet|serviceAccount|group|user)[:] ]] || {
    echo "the principal must look like principalSet://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/attribute.<name>/<value>" >&2
    exit 2
  }
  echo "# The canary's read-only identity in the project ${project}."
  printf 'gcloud iam roles create scarpenterCanaryReads --project=%s --title=%s \\\n  --description=%s --stage=GA \\\n  --permissions=%s\n' \
    "${project}" "$(sq "SCarpenter canary reads")" "$(sq "Read-only calls of the L5 canary")" "$(perms | paste -sd, -)"
  printf 'gcloud projects add-iam-policy-binding %s --member=%s --role=%s --condition=None\n' \
    "${project}" "$(sq "${principal}")" "projects/${project}/roles/scarpenterCanaryReads"
}

case "${1:-}" in
  role) cmd_role ;;
  bindings)
    shift
    cmd_bindings "$@"
    ;;
  *) usage ;;
esac
