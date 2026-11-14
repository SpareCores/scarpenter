#!/usr/bin/env bash
# Prints the policy of the L5 lane's CI identity (docs/how-to-guides/testing.md#the-l5-lane-on-oci)
# as the statements of the policy language (plain, one per line) and as the policy for a group.
# Nothing here calls OCI.
#
#   test/e2e-oci/ci-policy.sh statements
#   test/e2e-oci/ci-policy.sh policy <group> <compartment name or OCID> [region]
#
# The statements are the driver's (docs/reference/oci-iam-policy.txt: instances, boot volumes, the
# network, the region subscriptions, the limits, and the optional block that creates networks) plus what
# the lane's scenarios, the sweeper and the teardown need beyond it: the lane's own network of the byo
# scenario (virtual-network-family, covered by the driver's optional block) and the sweeper's deletion
# of a tagged boot volume that a terminate left behind (manage volume-family: the driver only needs use). No
# `manage all-resources`, nothing in the identity services (no users, groups, policies or dynamic groups),
# nothing tenancy-wide that writes: a statement either reads the tenancy or acts in the one compartment.
#
# What OCI cannot do, unlike the AWS lane: a policy condition cannot test the freeform tags of an
# instance on every verb, and cannot deny a shape. So there is no "only with the cluster tag" wall:
# the compartment is the boundary (a dedicated one, with a compartment quota as the ceiling), and the
# driver's own ownership check is the rest. The policy's condition `where request.region = '<region>'`
# limits the compartment statements to the lane's region; which verbs accept it is UNVERIFIED, and the
# first live run corrects this file (the audit log has the denials).
set -euo pipefail

# <verb> <resource type> in compartment|tenancy
statements=(
  "manage instance-family in compartment"
  "manage volume-family in compartment"
  "manage virtual-network-family in compartment"
  "inspect tenancies in tenancy"
  "inspect compartments in tenancy"
  "inspect resource-availability in tenancy"
  "inspect limits in tenancy"
)

usage() {
  echo "usage: $0 statements" >&2
  echo "       $0 policy <group> <compartment name or OCID> [region]" >&2
  exit 2
}

cmd_policy() {
  local group="${1:-}" comp="${2:-}" region="${3:-eu-frankfurt-1}" s where cond
  [[ -n "${group}" && -n "${comp}" ]] || usage
  [[ "${group}" =~ ^[A-Za-z0-9_.\'/-]+$ ]] || {
    echo "the group must be a group name ('Default'/'name' with an identity domain)" >&2
    exit 2
  }
  if [[ "${comp}" =~ ^ocid1\.compartment\.oc[0-9]+\.[a-z0-9-]*\.[A-Za-z0-9]+$ ]]; then
    where="compartment id ${comp}"
  elif [[ "${comp}" =~ ^[A-Za-z0-9_.:-]+$ ]]; then
    where="compartment ${comp}"
  else
    echo "the compartment must be a name (a path with :) or a compartment OCID" >&2
    exit 2
  fi
  [[ "${region}" =~ ^[a-z]{2}(-[a-z]+)+-[0-9]+$ ]] || {
    echo "the region must look like eu-frankfurt-1" >&2
    exit 2
  }
  cond="where request.region = '${region}'"
  echo "# The L5 lane's CI group in ${comp}, compartment writes limited to ${region}."
  echo "# Review before creating; the conditions are UNVERIFIED (see the header of ci-policy.sh)."
  for s in "${statements[@]}"; do
    case "${s}" in
      *" in tenancy") printf 'Allow group %s to %s\n' "${group}" "${s}" ;;
      *) printf 'Allow group %s to %s %s\n' "${group}" "${s/ in compartment/ in ${where}}" "${cond}" ;;
    esac
  done
}

case "${1:-}" in
  statements) printf '%s\n' "${statements[@]}" ;;
  policy)
    shift
    cmd_policy "$@"
    ;;
  *) usage ;;
esac
