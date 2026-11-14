#!/usr/bin/env bash
# Prints the policy of the canary's READ-ONLY identity
# (docs/how-to-guides/testing-and-releasing.md#the-canary) as the statements of the policy
# language (plain, one per line) and as the policy for a group:
#
#   test/e2e-oci/canary-policy.sh statements
#   test/e2e-oci/canary-policy.sh policy <group> <compartment name or OCID> [image compartment name or OCID]
#
# The optional image compartment (the lane's OCI_IMAGE_COMPARTMENT; empty or the same as the compartment:
# none) adds `read instance-images` in it: the canary lists and reads the images there.
#
# The canary (test/canary, vendor oci) runs the OCI driver's read paths: the region subscriptions
# and availability domains, the shape tables, the Flatcar images of the compartment and their shape
# compatibility, the Limits service (limit definitions and availability) and the list of the
# cluster's instances that the quota snapshot counts. This is exactly the set of statements of the
# calls it makes (oci_test.go runs it against the in-memory API and compares the statements of its
# calls with this output, at the highest verb of each resource type: read includes inspect) and
# nothing else: no verb above read, no manage, use or create. Nothing here calls OCI.
set -euo pipefail

# <verb> <resource type> in compartment|tenancy
statements=(
  "inspect compartments in tenancy"
  "inspect instances in compartment"
  "inspect limits in tenancy"
  "inspect resource-availability in tenancy"
  "inspect tenancies in tenancy"
  "read instance-images in compartment"
)

usage() {
  echo "usage: $0 statements" >&2
  echo "       $0 policy <group> <compartment name or OCID> [image compartment name or OCID]" >&2
  exit 2
}

# compartment_clause <name or OCID>: the policy's `compartment <name>` or `compartment id <OCID>`.
compartment_clause() {
  if [[ "$1" =~ ^ocid1\.compartment\.oc[0-9]+\.[a-z0-9-]*\.[A-Za-z0-9]+$ ]]; then
    echo "compartment id $1"
  elif [[ "$1" =~ ^[A-Za-z0-9_.:-]+$ ]]; then
    echo "compartment $1"
  else
    echo "the compartment must be a name (a path with :) or a compartment OCID" >&2
    return 1
  fi
}

cmd_policy() {
  local group="${1:-}" comp="${2:-}" imgcomp="${3:-}" s where imgwhere
  [[ -n "${group}" && -n "${comp}" ]] || usage
  [[ "${group}" =~ ^[A-Za-z0-9_.\'/-]+$ ]] || {
    echo "the group must be a group name ('Default'/'name' with an identity domain)" >&2
    exit 2
  }
  where="$(compartment_clause "${comp}")" || exit 2
  if [[ -n "${imgcomp}" && "${imgcomp}" != "${comp}" ]]; then
    imgwhere="$(compartment_clause "${imgcomp}")" || exit 2
  fi
  echo "# The canary's read-only group: reads only, nothing that writes."
  for s in "${statements[@]}"; do
    case "${s}" in
      *" in tenancy") printf 'Allow group %s to %s\n' "${group}" "${s}" ;;
      *) printf 'Allow group %s to %s\n' "${group}" "${s/ in compartment/ in ${where}}" ;;
    esac
  done
  if [[ -n "${imgwhere:-}" ]]; then
    printf 'Allow group %s to read instance-images in %s\n' "${group}" "${imgwhere}"
  fi
}

case "${1:-}" in
  statements) printf '%s\n' "${statements[@]}" ;;
  policy)
    shift
    cmd_policy "$@"
    ;;
  *) usage ;;
esac
