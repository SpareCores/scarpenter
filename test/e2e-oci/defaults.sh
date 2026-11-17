#!/usr/bin/env bash
# The pins of the L5 lane (OCI). Every value can be overridden through the environment
# (the workflow's inputs do it for the minimum-Kubernetes run).
#
# This lane is a line of test/e2e-vendors.txt (pull request tiers, nightly canary, release gate). Each cost guard of
# docs/how-to-guides/testing-and-releasing.md#per-vendor-cost-guards was shown by:
#   - a dedicated compartment and a compartment quota on cores and memory (the ceiling: no IAM statement can
#     deny a shape): the Quotas service accepted the quota names and the quota scenario reached the limit;
#   - the CI policy's region and compartment conditions (ci-policy.sh): the guards scenario, which makes the CI
#     identity attempt a VCN create in another region and in the tenancy root and got a refusal (404) each time
#     (an instance launch cannot be probed, the clause is read from the live policy);
#   - a budget alert on the compartment (50 % and 100 % actual, 100 % forecast): configured, its delivery is NOT
#     shown, the operator checks that an email arrives (the amount is in the tenancy's currency);
#   - the sweeper (e2e-oci-sweeper.yml, environment e2e-sweeper) deleting a dead run's resources with the real
#     CI identity: the scheduled sweep on main did;
#   - the canary's group reads and writes nothing: its live policy has only inspect and read verbs, and the
#     canary's own read-only checks;
#   - e2e-sweeper and e2e-canary limited to main (the deployment branch policy), the identity domain's trust
#     mapping exactly the three environments' subjects of this repository to the CI and canary users.
# shellcheck disable=SC2034  # used by the scripts that source lib.sh

# The account of the lane: a dedicated compartment of a tenancy (the compartment is the only
# ownership wall: no policy can protect an instance by freeform tag on every verb, plan 25
# section 11), its region, and the identity domain and confidential application of the keyless
# token exchange. None has a default: the workflow's variables provide them, so that no OCID,
# domain or client lives in this repository.
: "${OCI_COMPARTMENT:=}"
: "${OCI_TENANCY:=}"
# Optional: the compartment the Flatcar images are imported into when it is not the lane's (the
# controller's --oci-image-compartment, vendors.oci.imageCompartment). Empty: the lane's compartment.
: "${OCI_IMAGE_COMPARTMENT:=}"
: "${OCI_REGION:=eu-frankfurt-1}"
# The home region of the tenancy (where the identity domain lives); the lane's region when unset.
: "${OCI_HOME_REGION:=${OCI_REGION}}"
# The region the guards scenario writes in to show that the CI policy refuses it: not the lane's, and a region the tenancy IS
# subscribed to (an unsubscribed region refuses everything and proves nothing; the scenario checks it is READY).
: "${E2E_OCI_PROBE_REGION:=us-ashburn-1}"
: "${OCI_IDENTITY_DOMAIN_URL:=}"
: "${OCI_CLIENT_ID:=}"
# The audience of GitHub's OIDC token: what the identity domain's trust expects (unverified, S11).
: "${OCI_OIDC_AUDIENCE:=oci-federation}"
: "${E2E_K3S_VERSION:=v1.37.2+k3s1}"
# The kubelet of the nodes (a sysext of the sysext-bakery): at most the API server's
# version, at least three minors below it.
: "${E2E_KUBERNETES_VERSION:=v1.37.1}"
: "${E2E_TAILSCALE_VERSION:=v1.98.10}"
: "${E2E_FLATCAR_CHANNEL:=stable}"
# The first release with the oraclecloud image. The image is imported into the compartment (or OCI_IMAGE_COMPARTMENT) by the
# lane's operator, named flatcar-<channel>-<version>-<arch> (docs/how-to-guides/set-up-oci.md).
: "${E2E_FLATCAR_VERSION:=4757.2.3}"
# Instance types the suite launches; the NodePools name them. A flexible shape with a size:
# 1 OCPU (2 vCPUs on x86) and 4 GB is the smallest node that boots Flatcar and joins.
: "${E2E_TYPE_AMD64:=oci.VM.Standard.E5.Flex.1-4}"
# arm64 runs unless E2E_ARM64=false (the image needs a capability schema and A1 in its shape
# compatibility, set-up-oci.md; verified live on A1). A1 only: A2 and A4 are untested. The
# compartment quota must set A1 under both the plain and the regional name pairs. A1 capacity in eu-frankfurt-1
# is often short; the scenario is then skipped, not failed.
: "${E2E_TYPE_ARM64:=oci.VM.Standard.A1.Flex.1-2}"
# The types the spot scenario may launch, in order of preference: the same size (1 OCPU, 4 GB) of the E5, E4
# and E3 flexible shapes, all offered as preemptible in eu-frankfurt-1 by Keeper's catalog
# (test/data/sc-snapshot-oci has the rows). OCI regularly has no preemptible E5 capacity ("Out of host capacity"); the
# NodePool lists them all and the engine tries the others once an offering is unavailable. The
# scenario takes whichever launches, and is skipped when none has capacity.
: "${E2E_SPOT_TYPES:=oci.VM.Standard.E5.Flex.1-4,oci.VM.Standard.E4.Flex.1-4,oci.VM.Standard.E3.Flex.1-4}"
# The types the described-not-named scenario may pick from: 2 vCPUs (1 OCPU) and more than
# 6000 MiB rules out the first (4096 MiB) and the third (4 vCPUs).
: "${E2E_TYPES_DESCRIBED:=oci.VM.Standard.E5.Flex.1-4,oci.VM.Standard.E5.Flex.1-8,oci.VM.Standard.E5.Flex.2-8}"
: "${E2E_ARM64:=true}"
# The root volume of the amd64 class and the size the drift scenario changes it to (the minimum
# is 50 GB: the image is about 47 GiB).
: "${E2E_ROOT_GB:=60}"
: "${E2E_ROOT_GB_DRIFT:=70}"
# How long a node may take from its Pod to Running, and the exit criterion for
# scale to zero (seconds from the Pod's deletion to every instance gone).
: "${E2E_NODE_TIMEOUT:=900}"
# A node's NodeClaim must be launched (the cloud instance created) this long after a wait
# began, or the wait fails at once as "never launched" instead of running into
# E2E_NODE_TIMEOUT. A progress block (NodeClaims, Pods, events, controller errors) is
# printed to the job log this often during a wait.
: "${E2E_LAUNCH_TIMEOUT:=180}"
: "${E2E_PROGRESS_INTERVAL:=30}"
# After the first failed scenario the remaining ones are skipped and the run ends, so
# that the diagnostics and the teardown start at once; false runs every scenario.
: "${E2E_FAIL_FAST:=true}"
# The one knob to tune after the first live run: the roadmap says 2 minutes. The scenario
# also notes how long it took until every instance was terminating or gone, and until every
# one was gone.
: "${E2E_SCALE_DOWN_DEADLINE:=120}"
# One refresh of the quota usage (the chart values set the same interval) plus a
# margin for the debounce and the API.
: "${E2E_QUOTA_REFRESH:=30}"
# The scenarios of the smoke mode (docs/how-to-guides/testing-and-releasing.md): the
# shortest path through the node path (the preflight always runs first, then the guards, one on-demand
# node from the pods to Ready, then scale to zero). Standalone-safe: amd64 needs only its own
# workload, and scalezero reads the amd64 node's state when it exists and checks the
# cluster-wide exit criterion whatever ran before. Order is the runner's, not this list's.
: "${E2E_SMOKE_SCENARIOS:=guards,amd64,scalezero}"
# The limits the suite needs in the region (the preflight and the canary check them): OCPUs and
# GB of the E5 flexible shape family at once (the on-demand, spot, drift, described, byo and
# create nodes, and a replacement next to the node it replaces). OCI has no spot quota of its own
# that the lane relies on (unverified, plan 25 S9): preemptible nodes count against the same limits.
: "${E2E_MIN_OCPU:=8}"
: "${E2E_MIN_MEMORY_GB:=48}"
# The tailnet tags (docs/how-to-guides/testing.md#the-l5-lane).
: "${E2E_TAG_RUNNER:=tag:scarpenter-ci}"
: "${E2E_TAG_NODE:=tag:scarpenter-ci-node}"

# The list of described types as a YAML flow sequence for the NodePool manifest.
IFS=, read -ra _described <<<"${E2E_TYPES_DESCRIBED}"
E2E_TYPES_DESCRIBED_YAML="$(printf '"%s", ' "${_described[@]}")"
E2E_TYPES_DESCRIBED_YAML="${E2E_TYPES_DESCRIBED_YAML%, }"
# The spot types as a YAML flow sequence for the e2e-spot NodePool.
IFS=, read -ra _spot <<<"${E2E_SPOT_TYPES}"
E2E_SPOT_TYPES_YAML="$(printf '"%s", ' "${_spot[@]}")"
E2E_SPOT_TYPES_YAML="${E2E_SPOT_TYPES_YAML%, }"
# The arm64 types of the pool as a YAML flow sequence.
E2E_TYPES_ARM64_YAML="$(printf '"%s"' "${E2E_TYPE_ARM64}")"
# The image names of the pinned release (the naming contract of the default lookup).
E2E_FLATCAR_IMAGE_AMD64="flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-amd64"
E2E_FLATCAR_IMAGE_ARM64="flatcar-${E2E_FLATCAR_CHANNEL}-${E2E_FLATCAR_VERSION}-arm64"

# envsubst (manifests) and the child processes read exported variables.
export OCI_COMPARTMENT OCI_IMAGE_COMPARTMENT OCI_TENANCY OCI_REGION OCI_HOME_REGION E2E_OCI_PROBE_REGION OCI_IDENTITY_DOMAIN_URL OCI_CLIENT_ID OCI_OIDC_AUDIENCE \
  E2E_K3S_VERSION E2E_KUBERNETES_VERSION E2E_TAILSCALE_VERSION E2E_FLATCAR_CHANNEL E2E_FLATCAR_VERSION \
  E2E_FLATCAR_IMAGE_AMD64 E2E_FLATCAR_IMAGE_ARM64 E2E_TYPE_AMD64 E2E_TYPE_ARM64 E2E_TYPES_DESCRIBED \
  E2E_TYPES_DESCRIBED_YAML E2E_TYPES_ARM64_YAML E2E_SPOT_TYPES E2E_SPOT_TYPES_YAML E2E_ARM64 E2E_ROOT_GB E2E_ROOT_GB_DRIFT E2E_NODE_TIMEOUT E2E_LAUNCH_TIMEOUT \
  E2E_PROGRESS_INTERVAL E2E_FAIL_FAST E2E_SCALE_DOWN_DEADLINE E2E_QUOTA_REFRESH E2E_SMOKE_SCENARIOS E2E_MIN_OCPU \
  E2E_MIN_MEMORY_GB E2E_TAG_RUNNER E2E_TAG_NODE
