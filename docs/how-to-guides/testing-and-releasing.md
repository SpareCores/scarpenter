# Testing and releasing

When the real-cloud tests run, what they cost, who approves them, how the nightly
canary watches the outside world, how a release is cut and what gates it, and how to
add the next vendor. The lanes themselves (what each proves, how to run L1 to L4,
the scenarios of L5) are in [Test and develop](testing.md); this page is the policy
around them.

- [Policy](#policy)
- [The tiers](#the-tiers)
- [What triggers what](#what-triggers-what)
- [Labels and manual runs](#labels-and-manual-runs)
- [Approval and fork pull requests](#approval-and-fork-pull-requests)
- [The canary](#the-canary)
- [Releasing](#releasing)
- [Per-vendor cost guards](#per-vendor-cost-guards)
- [Adding a vendor lane](#adding-a-vendor-lane)
- [What the CI account's infrastructure provides](#what-the-ci-accounts-infrastructure-provides)
- [Alibaba Cloud provisioning](#alibaba-cloud-provisioning)

## Policy

The real-cloud lane (L5: [the L5 lane](testing.md#the-l5-lane)) is the only test
that proves the whole node path on a real cloud, and the only one that costs money,
needs credentials and takes 20 to 30 minutes. Two things follow:

- **It runs when something relevant changed, not on a timer.** A weekly run
  spends money when nothing moved and says nothing about the change that broke it;
  a run on the pull request that touches the code says so while the author still
  has the context, and costs the same. So L5 runs on the pull requests that touch
  what it tests, and before every release.
- **The outside world drifts without a commit.** An AMI is deprecated, a quota is
  lowered, an instance type disappears from a region, a sysext release loses its
  checksum file. Nothing in the repository changed, so nothing would run. A
  nightly **canary** reads exactly those things with the real driver code and no
  instance: it costs nothing, so it can run every night on every vendor.

Cost and signal pull in different directions per vendor. AWS bills a started hour
per instance, so a full run is cents; a vendor that bills a full hour per instance
or has no cheap type, or a GPU lane later, can cost dollars per run. That is why
the tiers differ in size (a smoke run is two scenarios), why the vendor list is in
one file, and why every vendor lane must have the cost guards below before it is
added to the list.

## The tiers

| Trigger | What runs | Cost (AWS) |
|---|---|---|
| every pull request | L1 to L4 (`ci.yml`, `qemu.yml`), unchanged | $0 |
| a pull request that touches a vendor's driver, its lane or the e2e workflows | that vendor's **full** L5 | cents (about 22 minutes) |
| a pull request that touches code every vendor's nodes go through | a **smoke** L5 on every vendor | cents (a fraction of a full run: two scenarios) |
| nightly (and on demand) | the **canary** of every vendor: no instances, no cluster, read-only credentials | about $0 |
| a release tag | the **full** L5 of every vendor, a required gate before anything is published | cents on AWS, dollars on dearer vendors |
| manual (`workflow_dispatch`) | any vendor, `full` or `smoke`, optionally a scenario list | as chosen |

**Smoke** is the preflight (always first: the Flatcar images, the quotas, the
NodeClasses are `Ready`), `amd64` (one on-demand node from the Pod to Ready, with
its labels, tags, bootstrap, join and DNS checks) and `scalezero` (every instance
terminated within two minutes of the Pods' deletion, nothing left behind, the quota
usage follows). The set is `E2E_SMOKE_SCENARIOS` in the vendor's
`test/e2e-<vendor>/defaults.sh` (AWS: `amd64,scalezero`; the preflight is not
listed because it always runs). It is the shortest path through the shared node
code: NodeClaim, driver launch, Ignition, join, scale to zero. Both scenarios are
standalone: `amd64` starts its own workload, and `scalezero` reads the amd64 node's
state when it exists and otherwise checks the cluster-wide criterion (no instance,
node, NodeClaim or token left), so neither depends on a scenario that is not in
the set. **Full** is every scenario of the lane (AWS: `amd64 arm64 db described byo
local negative quota drift scalezero uninstall`; `local` only with a
`local_storage_type`).

## What triggers what

`e2e.yml` runs on every pull request (opened, synchronized, reopened, ready for review)
without a path filter, so that its result is a check that can be required.
Its first job, `plan`, has no secret and no environment: it reads the labels and the draft
state of the pull request live (so that a re-run sees the current ones), computes the
changed files (`git diff --no-renames` of the merge commit against its first parent, the
base it was merged onto; a moved file counts at its old and its new path) and runs
`test/e2e-plan.sh`, the only place the rules are written down. A vendor
is a line of `test/e2e-vendors.txt`; `<vendor>` below stands for each of them.

| Path | Tier for the vendor |
|---|---|
| `pkg/vendors/<vendor>/**` | full |
| `test/e2e-<vendor>/**` | full |
| `.github/workflows/e2e-<vendor>-sweeper.yml` | full |
| `.github/workflows/e2e.yml`, `.github/workflows/e2e-cloud.yml`, `test/e2e-plan.sh`, `test/e2e-vendors.txt`, `test/lib.sh` | full, for every vendor |
| `pkg/vendors/*.go` (the shared driver contract), `pkg/vendors/facts/**` | smoke |
| `pkg/bootstrap/**`, `pkg/cloudprovider/**`, `pkg/artifacts/**`, `pkg/resources/**` | smoke |
| `pkg/quota/**`, `pkg/controllers/**`, `pkg/catalog/**`, `pkg/unavailable/**`, `pkg/apis/**`, `pkg/options/**`, `pkg/operator/**`, `pkg/scope/**`, `cmd/**` | smoke |
| `charts/**`, `config/crd/**`, `config/rbac*`, `Dockerfile`, `.dockerignore`, `go.mod`, `go.sum` | smoke |
| anything else (documentation, `hack/`, the other lanes, the canary's files, another vendor's package, the fake and qemu drivers) | nothing |

- **Full wins over smoke**, per vendor: a pull request that touches `pkg/vendors/aws/**` and
  `pkg/bootstrap/**` runs the full AWS lane and a smoke on every other vendor.
- **Test files never trigger**: `*_test.go` and `testdata/` are covered by L1 and L2 and
  cost nothing to run there.
- **A documentation-only pull request** triggers nothing: the matrix is empty and the
  check `e2e-result` succeeds at once.
- `go.mod` and `go.sum` are in the smoke set because the Karpenter core pin changes
  what the controller does on every node; a bump of a test-only dependency costs one smoke run.
- The paths are code, not a checklist: `test/e2e-plan-selftest.sh` tests every rule
  (`make test-e2e-aws-dryrun` and CI run it). A new package that nodes go through belongs in
  the list of `tier_of` in `test/e2e-plan.sh` together with a line in the test.

The caller then runs `e2e-cloud.yml` once per entry of the matrix, one vendor per job
(`fail-fast: false`: one vendor's failure says nothing about another), and the last
job, **`e2e-result`**, always runs: it succeeds when the matrix was empty (also with
`l5:skip`) and when every run passed. It fails when a run failed or was cancelled, and when
the plan needs L5 but the pull request is a draft or a fork's, which never run it. Make
`e2e-result` the required status check; do not require the `e2e` jobs themselves, which do
not exist for a pull request that needs none.

## Labels and manual runs

| Label | Effect |
|---|---|
| `l5:skip` | runs nothing (the result is green): for a change the path rules call relevant that cannot change the node path. Wins over `l5:full`. |
| `l5:full` | the full lane on every vendor, whatever the paths say: put it on a pull request that changes behaviour the paths do not show (a refactor across packages, a Karpenter bump you want proven everywhere). |

**Adding or removing a label starts nothing**: push a commit, or "Re-run all jobs" of the
latest run ("Re-run failed jobs" keeps the old plan). The plan reads the labels live, so a
re-run uses the current ones. There is deliberately no `labeled` trigger: a required check
counts the newest check run of its name on the commit, so a run for an unrelated label would
put a green `e2e-result` over a failed L5 run of the same commit, or over one still waiting
for its approval. Anyone who can label pull requests (the triage role) can add `l5:skip`, so
review who has it. A draft pull request never runs L5; marking it ready for review does.

Manual: Actions, "L5 (e2e)", Run workflow, or

```bash
gh workflow run e2e.yml -f vendor=aws -f mode=full                       # everything
gh workflow run e2e.yml -f vendor=aws -f mode=smoke                      # the smoke set
gh workflow run e2e.yml -f vendor=all -f mode=full -f scenarios=amd64,arm64
gh workflow run e2e.yml -f vendor=aws -f fail_fast=false                 # every scenario despite failures
gh workflow run e2e-canary.yml                                           # the canary, now
```

`scenarios` overrides the mode's set. A manual run of `e2e.yml` runs on the branch you
pick, so the manual run is also how to run L5 on a branch without a pull request. The
pins of the minimum-Kubernetes run (`k3s_version`, `kubernetes_version`,
`local_storage_type`) are inputs of `e2e-cloud.yml`, which cannot be dispatched by itself:
a one-off run with them is a workflow change that calls it with those inputs.

## Approval and fork pull requests

The job that calls the lane waits in the GitHub environment **`e2e`**, whose
**required reviewers** approve every run: nothing reaches a cloud account until a
maintainer has looked at the pull request. The reason is the shape of GitHub Actions:
for a `pull_request` run the workflow files, the scripts under `test/e2e-<vendor>/` and
the Go code that is built and installed all come from the pull request, and the job
that runs them holds the cloud role and a Tailscale identity. Whoever approves is
approving that code, so read the diff of the lane's files and of the shared code, not
only the description. The environment cannot be limited to `main`: a pull request runs
on `refs/pull/<n>/merge`.

- **Forks never get credentials.** A pull request from a fork never runs the lane
  (`github.event.pull_request.head.repo.full_name != github.repository`), and a fork that
  enables Actions runs nothing (`github.repository != 'SpareCores/scarpenter'`). These
  conditions are guardrails: the workflow file of a pull request comes from the pull request,
  so the boundary is GitHub's, which gives a fork's run no secrets and no OIDC token, and the
  environment's reviewers. The plan still runs for a fork (it holds nothing), and when it needs
  L5 the check `e2e-result` fails: a maintainer who wants the change pushes it to a branch of
  the repository (and reviews it), or adds `l5:skip` and re-runs all jobs. Setting "Require
  approval for all outside collaborators" in the repository's Actions settings stays required:
  it covers the L1 to L4 workflows.
- **Drafts never run.** The plan runs; when it needs L5 the check fails until the pull
  request is marked ready for review, which starts a run.
- The plan job and `e2e-result` have no secrets and no environment, so they run for
  a pull request without approval; they only read file names and labels (`plan` has
  `pull-requests: read` for the labels).
- A run waits for the vendor's concurrency slot (`e2e-<vendor>`: one run per vendor at a
  time, because the account's quotas and the tailnet are shared). GitHub keeps one waiting run per
  group, so a third run replaces the second while it waits; after a push, the new run
  replaces the waiting one and the running one finishes. The release gate shares the slot
  (two runs at once in one account would fail the quota scenario), so a pull request run can
  replace a waiting gate: it shows as cancelled, and "Re-run failed jobs" of the release run
  queues it again.
- Every run's teardown deletes what it created; the [sweeper](testing.md#the-sweeper)
  deletes what a dead run left behind.

## The canary

`e2e-canary.yml` runs nightly (`41 3 * * *`) and on demand, one job per vendor, in the
environment **`e2e-canary`**. It exists to notice drift without spending anything: there is no
Tailscale, no k3s, no instance and no cluster, and the AWS credentials are a **read-only
role**. It runs `test/canary` (`test/canary/run.sh <vendor>`), a Go program that calls
the real code and prints a table, with the same pins as the lane (the vendor's
`defaults.sh`, the single source of truth). For AWS it checks:

| Check | What it calls | What it catches |
|---|---|---|
| sysext `kubernetes` and `tailscale`, both architectures | `artifacts.Resolver.Sysext`, the code behind the NodeClass condition `ArtifactsReady` (network only) | a release or its `SHA256SUMS` removed or renamed in the sysext-bakery |
| k3s release | the checksum file of the pinned control plane (network only) | the pinned k3s release gone |
| regions and zones | the driver facts (`facts.Load`: `DescribeRegions`, `DescribeAvailabilityZones`) | the lane's region disabled or denied, zones gone |
| Flatcar image, amd64 and arm64 | `Driver.ResolveImage` (`DescribeImages`: exactly one available image by name from the official owner) | the pinned Flatcar version unpublished or ambiguous |
| Standard vCPU quota | `Driver.Quotas` (`GetServiceQuota`, `ListServiceQuotas`, and the usage reads), compared with `E2E_MIN_VCPU_OD` and `E2E_MIN_VCPU_SPOT` | a quota lowered below what the suite needs |
| instance types | `Driver.LaunchableTypes` (`DescribeInstanceTypeOfferings`, `DescribeInstanceTypes`) for every type the lane's NodeClasses and NodePools use, plus the quota mapping of each | a type no longer offered in any zone of the region, or one the driver cannot count |
| read-only guard | the audit of the run | a check that tried a write |

The Keeper catalog (prices, benchmark scores) is a public service the lanes do not depend
on for launching, and is not part of the canary.

**Alibaba Cloud.** The same shape with the vendor's calls (`test/canary/alicloud.go`): the
region and its zones (`DescribeRegions`, `DescribeZones`), the Flatcar images by owner (the
account itself) and name for both architectures (`Driver.ResolveImage`: `DescribeImages`), every
type of the lane offered with stock in a zone and mapped to both quota buckets
(`Driver.LaunchableTypes`: `DescribeAvailableResource`, `DescribeInstanceTypes`; `Driver.Consumption`),
and the vCPU limits and usage of pay-as-you-go and spot (`Driver.Quotas`: `DescribeAccountAttributes`,
`DescribeInstances`) against `E2E_MIN_VCPU_OD` and `E2E_MIN_VCPU_SPOT`. The guard is
`test/canary/alicloud_guard.go` (it refuses every write method of the driver's ECS and VPC
interfaces; `guard_test.go` proves it by reflection) and the role's policy
(`test/e2e-alicloud/canary-policy.sh`) is the seven `ecs:Describe*` actions the canary calls, which
`canary_test.go` compares with the run. RAM cannot limit those reads to a region (unverified:
the Describe actions take `*` only), so the lane's region is a parameter of the canary, not a limit of
the role. The credentials are the job's OIDC token written to a file (`oidc-token.sh`), which
the SDK's credentials library exchanges for the read-only RAM role.

**It cannot write.** Two walls: the role's policy allows only `Describe*`, `Get*` and
`List*` actions of EC2 and Service Quotas, in the lane's region, with no resource scoping to
loosen (`test/e2e-aws/canary-policy.sh <account> <region>` prints it; its
actions are listed in the next paragraph), and the driver is given clients that refuse
every call that is not a Describe, Get or List before the SDK sees it
(`test/canary/aws_guard.go`; `guard_test.go` checks by reflection that every method of
the driver's EC2 interface is either passed through as a read or refused, so a new write method
cannot slip in). `canary_test.go` runs the whole canary against the in-memory EC2 and
compares the calls it made with the policy: every call is allowed, and the policy
allows nothing the canary does not call (but the one conditional listing).

The policy: `ec2:DescribeAvailabilityZones`, `ec2:DescribeCapacityReservations`,
`ec2:DescribeImages`, `ec2:DescribeInstanceTypeOfferings`, `ec2:DescribeInstanceTypes`,
`ec2:DescribeInstances`, `ec2:DescribeRegions`, `ec2:DescribeSpotInstanceRequests`,
`servicequotas:GetServiceQuota` and `servicequotas:ListServiceQuotas`, all with
`aws:RequestedRegion` equal to the lane's region. (The driver's facts also try the zones of the
other regions, and the policy refuses them; they are reported as skipped, as for the CI role.)

**Where failures show up.** A failed scheduled run opens, or comments on, **one** issue
labelled `e2e-canary` in this repository, with the table (redacted by `redact()` of
`test/e2e-<vendor>/lib.sh`: the repository is public); a green night closes it. The
run's step summary has the table too, and a manual run (never an issue) is read there.
Read a failure by its row: a Flatcar image gone means moving the pin in `defaults.sh`
(and the lane's manifests read it from there); a quota means a request to the cloud
provider; a type means changing the types of `defaults.sh` and the CI policy's deny list
(`ci-policy.sh` takes the allowed types from the same file).

**Hetzner Cloud.** The canary of vendor `hcloud` (`test/canary/hcloud.go`) uses a **read-only token** of the
CI project (the secret `HCLOUD_TOKEN` of the environment `e2e-canary`; the optional variable
`E2E_HCLOUD_LOCATIONS` names the locations). Besides the shared sysext and k3s checks it checks:

| Check | What it calls | What it catches |
|---|---|---|
| locations | `Driver.Regions` (`GET /locations`) | a lane location gone |
| Flatcar snapshot, amd64 and arm64 | `Driver.ResolveImage` (exactly one labelled snapshot of the pinned release, the path the NodeClass condition `ImagesReady` uses) | a snapshot deleted or doubled in the project |
| Flatcar release, amd64 and arm64 | HEAD of `flatcar_production_hetzner_image.bin.bz2` and GET of its `.DIGESTS` on the release server (shared artifact checks) | the image `hcloud-upload-image` makes snapshots from moved or removed |
| server types | `GET /server_types`: every type of the lane exists in a lane location and is not past `deprecation.unavailable_after`; a temporarily unavailable entry is noted, not a failure | a type retired or no longer offered |
| API rate limit | the `RateLimit-*` headers of the last response, at least 1000 of 3600 left | another run using the project |
| read-only guard | the audit | a check that tried a write |

It cannot write: the token is read-only, the HTTP transport of the real client refuses every method but `GET`
before the network (`test/canary/hcloud_guard.go`), and the driver is given an API that refuses every `Create*`
and `Delete*` call; `hcloud_test.go` checks both walls (every method of `hcloud.API` is a passed-through read or a
refused write). There is no policy to print: Hetzner tokens are read or read-write for the whole project.

**The GCP canary** (`test/canary/gcp.go`, `test/canary/run.sh gcp`; pins from `test/e2e-gcp/defaults.sh`, the project and region from the variables
`GCP_PROJECT` and `GCP_REGION`) runs the GCP driver's read paths over the real Compute client, every call under a deadline (`gcp.WithTimeouts`) and the whole
run under one more:

| Check | What it calls | What it catches |
|---|---|---|
| regions and zones | the driver facts (`regions.list`, `zones.list`): the zone names and numeric IDs | the lane's region down or unreadable, zones gone |
| Flatcar image, amd64 (and arm64 when `E2E_ARM64=true`) | `Driver.ResolveImage` (`images.get` by the pinned name from `kinvolk-public`, or the Spare Cores project for arm64) | the pinned version unpublished |
| the image's guest OS features | `images.get` of the resolved image: ready, architecture, and `GVNIC` when the lane's types (c3 and later series) need it | an image the lane's db type cannot boot from |
| vCPU quotas | `Driver.Quotas` (`regions.get`) and `GlobalQuotas` (`projects.get`): `CPUS` against `E2E_MIN_VCPU_OD`, `PREEMPTIBLE_CPUS` against `E2E_MIN_VCPU_SPOT` (when the region has none, Spot runs on `CPUS`, so the two add up), and `CPUS_ALL_REGIONS` when the project has it | a quota lowered below what the suite needs |
| machine types | `Driver.LaunchableTypes` (`machine-types.list` per zone) for every type the lane uses, and, where the driver maps a type's quota consumption, that the buckets exist and fit one instance (an unmapped series is said, not failed: it takes the reactive path) | a type no longer offered in any zone of the region |
| read-only guard | the audit of the run | a check that tried a write |

The role is **read-only** and cannot be narrowed to the region (a list is checked against the project): `compute.images.get`,
`compute.machineTypes.list`, `compute.projects.get`, `compute.regions.get`, `compute.regions.list` and `compute.zones.list`
(`test/e2e-gcp/canary-policy.sh role` prints it, `bindings <project> <principal>` the `gcloud` commands). The second wall is
`test/canary/gcp_guard.go`, which refuses every method of the driver's Compute interface that is not a get, a list or an operation read; `gcp_guard_test.go` checks by
reflection that every method is passed through as a read or refused (and has its IAM permission in the table), and `gcp_test.go` compares the permissions of the calls the canary made with the role.

**The OCI canary** (`test/canary/oci.go`, `test/canary/run.sh oci`; pins from `test/e2e-oci/defaults.sh`, the compartment, tenancy and region from the variables `OCI_COMPARTMENT`, `OCI_TENANCY`
and `OCI_REGION`) runs the OCI driver's read paths over the real SDK adapter, every call under a deadline (the adapter's) and the whole run under one more. The identity is the keyless token exchange
(`test/e2e-oci/oci-session.sh`, the read-only group of the environment `e2e-canary`):

| Check | What it calls | What it catches |
|---|---|---|
| regions and availability domains | `Driver.Regions` and `Zones` (`ListRegionSubscriptions`, `ListAvailabilityDomains`) | the lane's region not subscribed, availability domains gone |
| Flatcar image, amd64 (and arm64 when `E2E_ARM64=true`) | `Driver.ResolveImage` (`ListImages` by the naming contract `flatcar-<channel>-<version>-<arch>` in the compartment) | the pinned version not imported |
| the image's shape compatibility | `ListImageShapeCompatibilityEntries` of the resolved image: every lane shape of the architecture is registered | an image the lane's shape cannot boot from (a missing Arm shape) |
| quota | `Driver.Quotas` (`ListShapes`, `ListLimitDefinitions`, `GetResourceAvailability`): the E5 core and memory limits against `E2E_MIN_OCPU` and `E2E_MIN_MEMORY_GB` (an Arm family against twice its lane type) | a limit lowered below what the suite needs, or one the Limits service cannot answer |
| instance types | `ParseType` and `Driver.Consumption` for every type the lane uses (on demand): the shape is offered in an availability domain, and its buckets exist and fit one instance | a shape no longer offered in the region |
| read-only guard | the audit of the run | a check that tried a write |

The policy is **read-only** and cannot be narrowed to the region: `inspect instances` and `read instance-images` in the compartment and `inspect tenancies`, `inspect compartments`,
`inspect resource-availability` and `inspect limits` in the tenancy (`test/e2e-oci/canary-policy.sh policy <group> <compartment>` prints it). The second wall is `test/canary/oci_guard.go`, which refuses every
method of the driver's four API interfaces that is not a get or a list; `oci_guard_test.go` checks by reflection that every method is passed through as a read or refused (and has its policy statement in the table),
and `oci_test.go` compares the statements of the calls the canary made with the policy.

## Releasing

There is no version bump commit: the version comes from the tag alone (the Dockerfile
stamps the binary, the chart is packaged with the tag as `version` and `appVersion`).
`.github/workflows/release.yml` publishes on a tag `v<semver>` (`v0.2.0`, or
`v0.2.0-rc.1` for a pre-release) pushed by a maintainer.

1. **Make `main` releasable.** Everything for the release is merged; CI is green on the
   commit; the last nightly canary is green (no open `e2e-canary` issue); the L5 runs of the
   merged pull requests passed (a change the paths did not cover is covered by the gate
   in step 4).
2. **Dry run (optional, recommended for the first tag of a series).**
   `hack/dev.sh hack/release.sh dry-run v0.2.0-rc.1` locally, or Actions, "Release", Run
   workflow with `dryRun: true`: builds both images for amd64 and arm64 without pushing, checks the
   production image has no development driver flags and reports the stamped version, and packages
   and lints the chart and the schema files. Nothing is published and no cloud is touched.
3. **Tag the commit on `main` and push the tag.**
   `git tag -s v0.2.0 <commit> && git push origin v0.2.0`. A tag ruleset on `v*`
   (repository settings) bounds who can do this.
4. **The workflow runs.**
   - `verify`: the tag is a valid `v<major>.<minor>.<patch>[-<pre>]`; the tagged commit is on
     `main`; whether this is the highest stable version (it may move `latest` and the major
     tag) and the highest of its minor; generated files are up to date; lint, tests and the
     chart tests pass; the vendors of the gate are read from `test/e2e-vendors.txt`.
   - **`e2e` (the release gate)**: the full L5 lane of **every** vendor against the tagged
     commit, one job per vendor, in the environment `e2e`: the required reviewers approve it
     like any L5 run, and it waits for each vendor's concurrency slot (so a run of a pull request
     that holds the slot delays it, and a newer pull request run replaces it while it waits:
     re-run the failed jobs; avoid starting L5 on pull requests while a release is pending). It builds the controller from the tagged checkout, so it
     proves the commit, which the next jobs then build the images from.
   - `images`, `chart` and `release` **need `e2e`**: a failed or rejected gate publishes
     nothing. `images` runs in the environment `release` (add required reviewers there for a
     second approval before the first push): the controller and development images for
     both architectures, SBOM and provenance, cosign signatures by digest. `chart` packages
     the chart with the version and pushes it to `oci://ghcr.io/sparecores/charts`, signed.
     `schema` packs the CRD, the JSON Schema of the `SCNodeClass` and the example
     NodeClasses (`hack/release.sh package-schema`; files only). `release` creates the GitHub
     release with notes, the chart package, the SBOMs and those files
     ([published schema and examples](../reference/infrastructure-handoff.md#published-schema-and-examples);
     pre-release for a tag with a suffix, which does not move `latest`).
5. **If the gate fails**, nothing was published. Fix on `main`, and tag the next version (or
   the next `-rc.N`): do not move or reuse a tag. Re-running the failed jobs of the same run
   is right for a flake (a spot capacity error, an infrastructure hiccup that the diagnostics
   show); it re-runs the lane on the same commit.
6. **Verify**: the release notes carry the `cosign verify` commands for both images and the
   chart.

Why the gate is a job of `release.yml` and not a separate workflow: the release is triggered
by the tag push and the publishing jobs must not start without the gate's result; `needs:`
between jobs of one run is the dependency GitHub enforces (a separate workflow on the same tag
would run in parallel and could not stop the publishing). `release.yml` calls the same
reusable workflow as the pull requests, `e2e-cloud.yml`, so the release gate and the
pull request tiers run the same code. The `workflow_dispatch` dry run does not run the gate: it
cannot publish, and it must stay free.

## Per-vendor cost guards

A vendor lane is not added to `test/e2e-vendors.txt` before every item is true for it
(the AWS lane's are in the second column, the Alibaba Cloud lane's in the third; that
lane has not run yet, so its column says what the files do, not what a run showed, and it is
**not yet a line of `test/e2e-vendors.txt`**: whether RAM applies `ecs:InstanceType` to `ecs:RunInstances` is
unverified, so the operator checks the type deny bites and the budget alert fires first):

| Guard | AWS lane | Alibaba Cloud lane |
|---|---|---|
| A budget alert on the account that hosts the lane, to a person | account-level, in the CI account's infrastructure | account-level, in the CI account's infrastructure |
| The CI role denies instance types the lane does not use | `DenyOtherInstanceTypes` in `test/e2e-aws/ci-policy.sh` (from `defaults.sh`) | `Deny` of `ecs:RunInstances` outside the types of `defaults.sh` in `test/e2e-alicloud/ci-policy.sh` (the RAM condition key is unverified) |
| The CI role cannot act outside the lane's region | `DenyOutsideRegion` | every write `Allow` names the region in its ARNs (no explicit region deny) |
| Every resource carries a run tag, so cost and leftovers are attributable | `scarpenter.sparecores.com/cluster=scarpenter-ci-<run id>` and `ci-created-at` | the same tag and `ci-created-at`; a deployment set takes no tags, so its description carries the cluster |
| A sweeper that deletes what a dead run left behind, by tag and age | `e2e-aws-sweeper.yml`, every three hours, environment `e2e-sweeper` | `e2e-alicloud-sweeper.yml`, every three hours, environment `e2e-sweeper` |
| A run time limit | 90 minutes for the job (60 for a smoke), 55 (25) for the scenarios step; the teardown always runs | the same limits |
| Small, cheap types; spot where the vendor has it | `t3a.small`, `t4g.small` (spot), `r6a.large` for the placement-group scenario | `ecs.e-c1m2.large`, `ecs.c8y.large` (spot), `ecs.g7.large` for the deployment-set scenario |
| A teardown that fails the job when anything tagged remains | `teardown.sh` | `teardown.sh` (instances, disks, network interfaces, groups, deployment sets, vSwitches, VPCs) |
| A quota that bounds the damage of a bug | the vCPU quotas of the account (16 on-demand, 4 spot needed) are the ceiling | the vCPU quotas of the account (16 pay-as-you-go, 4 spot needed) are the ceiling |
| A read-only role for the canary | `canary-policy.sh`, a role of its own, environment `e2e-canary` | `canary-policy.sh`, a role of its own, environment `e2e-canary` |
| Approval for every run of pull request or release code that reaches the account | the environment `e2e`'s required reviewers (the sweeper runs only the code of `main`, environment `e2e-sweeper`) | the environment `e2e`'s required reviewers, the same |

**The Hetzner lane's guards** (plan 15), against the table above. The lane is **not yet a line
of `test/e2e-vendors.txt`**: a token cannot deny types, so the project's server limit and usage alert
are the ceiling, and the operator sets and checks both first (see [What the CI account's
infrastructure provides](#what-the-ci-accounts-infrastructure-provides)); then adds `hcloud` to the file.

| Guard | hcloud lane |
|---|---|
| Budget alert to a person | the project's usage alert (Console) |
| Type deny | **none possible** in the token: the lane's NodePools list the types (`cx23`, `cax11`, `cpx22`), and the project's server limit (at least 10, requested low) bounds a bug |
| Region restriction | `E2E_HCLOUD_LOCATIONS` (default `fsn1,nbg1,hel1`); not enforceable by the token |
| Run tag | label `scarpenter.sparecores.com/cluster=scarpenter-ci-<run id>` and `ci-created-at` on every resource |
| Sweeper | `e2e-hcloud-sweeper.yml` every three hours, environment `e2e-sweeper`: servers, volumes, placement groups, firewalls and unassigned Primary IPs older than two hours; never the Flatcar snapshots |
| Run time limit, teardown | as AWS (90 minutes, 55 for the scenarios, `if: always()`) |
| Teardown that fails on leftovers | `teardown.sh` |
| Read-only canary | a read-only token, a transport guard that refuses every method but GET, with a test |
| Approval | the environment `e2e`'s required reviewers |

**The OVH lane's guards** (plan 18; statically validated, not yet run live; **not yet a line of `test/e2e-vendors.txt`**: no flavor deny is possible and the project budget alert is unverified, see [What the CI account's infrastructure provides](#what-the-ci-accounts-infrastructure-provides)): the flavors are the lane's own (`defaults.sh`: `d2-2`, `d2-4`, `b3-8`; no
IAM condition limits flavors), the region is the only one activated in the lane's dedicated project, the run tag is the server
tag `scarpenter-cluster=scarpenter-ci-<run id>` with the managed groups' marker, `e2e-ovh-sweeper.yml` is the sweeper, the teardown fails
the job when a server, security group, server group or available volume remains, the project's core and instance quota is the ceiling,
the canary uses a read-only OpenStack user behind `test/canary/ovh_guard.go`, and the environments are the same
(`e2e`, `e2e-canary`, `e2e-sweeper`) with the secrets and variables of [the OVH lane](testing.md#the-l5-lane-of-ovhcloud). The account-level
budget alert (OVH has per-project consumption alerts: unverified) is in the CI project's own infrastructure.

**The UpCloud lane's guards** (plan 19; statically validated, not yet run live; **not yet a line of
`test/e2e-vendors.txt`**: the token can deny nothing, and the sub-account's limits and the spending alert are
unverified, see [UpCloud](#upcloud)): no type or zone deny is possible (the lane's NodePools name the
STARTER plans of `defaults.sh`, and `UPCLOUD_ZONE` is one zone), the `scarpenter-ci` sub-account's resource
limits (cores and memory, which the canary compares with the lane's needs) are the ceiling, the run tag is the
label `scarpenter.sparecores.com/cluster=scarpenter-ci-<run id>` (and `ci-created-at`), `e2e-upcloud-sweeper.yml`
is the sweeper (environment `e2e-sweeper`; never the templates labelled `scarpenter-use`), the run time limits
and the failing teardown are AWS's, the canary's read-only guard is `test/canary/upcloud_guard.go` (an UpCloud
token cannot be read-only), and the environment `e2e`'s required reviewers approve every run.

**The Azure lane's guards** (plan 17; statically validated, not yet run live; **not yet a line of
`test/e2e-vendors.txt`**: none of them has been shown on a live account, see [Azure](#azure)): the Azure Policy
"Allowed virtual machine size SKUs" (deny) on the lane's resource group, from the sizes of
`test/e2e-azure/defaults.sh` (`ci-policy.sh` prints it), is the type deny; "Allowed locations" and the CI
identity's scope (that resource group only) keep the lane in its region; the run tag is
`scarpenter-cluster=scarpenter-ci-<run id>` with `scarpenter-ci-created-at` (inline disks by their name prefix);
`e2e-azure-sweeper.yml` is the sweeper (environment `e2e-sweeper`); the run time limits and the failing teardown
are AWS's; the vCPU quotas (regional and the lane's families 16, Spot 4, Standard public IPs) are the ceiling,
with a budget alert on the subscription; the canary's identity has a read-only custom role
(`canary-policy.sh`) and a guard that refuses every write (`test/canary/azure_guard.go`); the identities are
federated (GitHub OIDC, no secret), and the environment `e2e`'s required reviewers approve every run.

The GCP lane's, which are **not all true yet**: `gcp` is not in `test/e2e-vendors.txt`, and the single line `gcp` there is all that is left to do once they are
(the workflows' credentials steps, the sweeper, the canary part and the planner's rules are in place and tested by `test/e2e-plan-selftest.sh` and `test/e2e-gcp/selftest.sh`):

| Guard | GCP lane |
|---|---|
| A budget alert on the project that hosts the lane, to a person | project-level, in the CI account's infrastructure (**to provision**) |
| The CI identity cannot create instances of types the lane does not use | **not expressible**: an IAM condition tests the resource name, not the machine type; the pools name the types, the vCPU quotas are the ceiling |
| The CI identity cannot act outside the lane's region | the regional role is bound with a condition on `projects/<project>/zones/<region>-*` and `projects/<project>/regions/<region>/*` (`ci-policy.sh`; that every method honours it is unverified until the first live run) |
| The project holds nothing but the lane's resources (no IAM condition can protect instances by label) | a dedicated project; the driver refuses to delete an instance without its cluster's label |
| Every resource carries a run marker, so cost and leftovers are attributable | the label `scarpenter-cluster=scarpenter-ci-<run id>` and `ci-created-at` on instances and disks; the name prefix `scarpenter-<cluster>-` plus the description marker on networks, subnetworks, firewall rules and resource policies |
| A sweeper that deletes what a dead run left behind, by label, marker and age | `e2e-gcp-sweeper.yml`, every three hours, environment `e2e-sweeper` |
| A run time limit | 90 minutes for the job (60 for a smoke), 55 (25) for the scenarios step; the teardown always runs |
| Small, cheap types; spot where the vendor has it | `e2-small` (on demand and Spot), `e2-medium`, `e2-standard-2`, `t2a-standard-1` (arm64, optional), `c3-standard-4` for the placement-policy scenario |
| A teardown that fails the job when anything of the run remains | `test/e2e-gcp/teardown.sh`: instances, disks, resource policies, firewall rules, subnetworks, networks |
| A quota that bounds the damage of a bug | the project's `CPUS` (24 needed) and `PREEMPTIBLE_CPUS` (4) quotas in the lane's region are the ceiling (**to provision**) |
| A read-only role for the canary | `canary-policy.sh`, a service account of its own, environment `e2e-canary` |
| Approval for every run of pull request or release code that reaches the project | the environment `e2e`'s required reviewers (the sweeper runs only the code of `main`, environment `e2e-sweeper`) |

The OCI lane's, which are **not all true yet**: `oci` is not in `test/e2e-vendors.txt`, and the single line `oci` there is all that is left to do once they are
(the workflows' credentials steps, the sweeper, the canary part and the planner's rules are in place and tested by `test/e2e-plan-selftest.sh` and `test/e2e-oci/selftest.sh`):

| Guard | OCI lane |
|---|---|
| A budget alert on the compartment that hosts the lane, to a person | compartment-level, in the CI account's infrastructure (**to provision**) |
| The CI identity cannot create instances of shapes the lane does not use | **not expressible**: a policy statement names a resource family, not a shape; the pools name the shapes and the **compartment quota** on compute cores and memory is the hard ceiling (**to provision**) |
| The CI identity cannot act outside the lane's region | the statements are bound with `where request.region = '<region>'` (`ci-policy.sh policy`; which verbs accept the condition is unverified until the first live run) |
| The compartment holds nothing but the lane's resources (no policy can protect instances by tag) | a dedicated compartment; the driver refuses to delete an instance without its cluster's tag |
| Every resource carries a run marker, so cost and leftovers are attributable | the freeform tag `scarpenter-cluster=scarpenter-ci-<run id>` and `ci-created-at` on instances; the ownership tags plus the name prefix `scarpenter-<cluster>-` on the network resources; a boot volume carries a tag only if OCI copies the instance's, so the compartment is the marker |
| A sweeper that deletes what a dead run left behind, by tag, name and age | `e2e-oci-sweeper.yml`, every three hours, environment `e2e-sweeper` (boot volumes older than two hours too: the compartment is dedicated) |
| A run time limit | 90 minutes for the job (60 for a smoke), 55 (25) for the scenarios step; the teardown always runs |
| Small, cheap shapes; preemptible where the vendor has it | `VM.Standard.E5.Flex` of 1 OCPU and 4 to 8 GB (on demand and preemptible; 1 OCPU and 4 GB cost cents per hour), `A1` and `A2` (arm64, optional) |
| A teardown that fails the job when anything of the run remains | `test/e2e-oci/teardown.sh`: instances, groups, subnets, route tables, gateways, security lists, VCNs and boot volumes created since the run began |
| A quota that bounds the damage of a bug | the compartment quota on compute cores and memory (8 OCPUs and 48 GB of E5 needed) and the service limits of the region (**to provision**) |
| A read-only policy for the canary | `canary-policy.sh`, a group of its own, environment `e2e-canary` |
| Approval for every run of pull request or release code that reaches the compartment | the environment `e2e`'s required reviewers (the sweeper runs only the code of `main`, environment `e2e-sweeper`) |

## Adding a vendor lane

Eight or more vendors will follow AWS (OCI is next); the mechanics are one directory, one
line and a few credentials. In this repository:

1. **The lane's scripts**: `test/e2e-<vendor>/` with `bootstrap.sh`, `mint-authkey.sh`,
   `install.sh`, `scenarios.sh`, `diagnostics.sh`, `teardown.sh` (the reusable workflow calls
   these by name), `lib.sh` (it must define `redact`), and `defaults.sh` with the pins:
   the Flatcar version, the types, `E2E_SMOKE_SCENARIOS`, `E2E_MIN_VCPU_OD` and
   `E2E_MIN_VCPU_SPOT` (or the vendor's own quota knobs, with the canary's config), and the
   region variable. Start from `test/e2e-aws/`; the control plane, the tailnet and the
   scenarios' shape are the same, the cloud calls differ. Its `selftest.sh` and manifests
   test belong in `make test-e2e-aws-dryrun`.
2. **The vendor list**: one line in `test/e2e-vendors.txt`. The pull request tiers, the canary matrix and the
   release gate all read it; the paths `pkg/vendors/<vendor>/**` and `test/e2e-<vendor>/**` are
   already rules (`test/e2e-plan.sh`), and `test/e2e-plan-selftest.sh` checks the list names a
   `test/e2e-<vendor>/` directory.
3. **The reusable workflow**: a credentials step in `e2e-cloud.yml` next to the AWS one
   (`if: inputs.vendor == '<vendor>'`, the vendor's own role action, its secret) and, in the
   Prepare step, a line for the region (or project, or compartment) variable. Do the same for
   the canary in `e2e-canary.yml`. Add the vendor's sweeper as `e2e-<vendor>-sweeper.yml`.
4. **The CI policy** (the vendor's equivalent of `ci-policy.sh`: instance-type deny, region
   restriction, tag conditions) and the **canary policy** (`canary-policy.sh`: reads only),
   each with a selftest that checks the properties above.
5. **The canary's vendor part**: a file `test/canary/<vendor>.go` with the vendor's checks
   (the driver's own read paths, behind a guard like `aws_guard.go` that refuses writes, with
   a test that proves it), registered in `vendorChecks` in `main.go`, and the region variable in
   `vendorEnv` in `config.go`. The artifact checks are shared.
6. **The cost guards** above, true.
7. **Docs**: the vendor's lane section in [Test and develop](testing.md#the-l5-lane) and the
   provisioning notes next to the AWS ones.

In the CI account's infrastructure (below): the cloud account or project, the CI role and the
canary role, the environment secrets, the quotas, and the tailnet tags and ACL.

## What the CI account's infrastructure provides

None of the following lives here; the operator of the CI account creates it, with whatever
tool they use, and this repository assumes it (a fork needs the same, in its own account):

- **OVHcloud** (vendor `ovh`): a dedicated Public Cloud project with one active region, low core and instance quotas and a
  spending alert that has been checked to fire, all set before `ovh` is added to `test/e2e-vendors.txt`, the two OpenStack users and
  their secrets and variables. Details in [Test and develop](testing.md#the-l5-lane-of-ovhcloud).
- **UpCloud** (vendor `upcloud`): the `scarpenter-ci` sub-account with low resource limits and a
  spending alert, both set and checked before `upcloud` is added to `test/e2e-vendors.txt`, the secret
  `UPCLOUD_TOKEN` and the Flatcar templates. Details in [UpCloud](#upcloud).
- **Azure** (vendor `azure`): a dedicated resource group in one region with the size and location
  policies, the quotas and a budget alert, the two user-assigned managed identities with their
  federated credentials, all set and checked before `azure` is added to `test/e2e-vendors.txt`.
  Details in [Azure](#azure).
- **GCP** (vendor `gcp`): a dedicated project with the quotas and a budget alert, the workload
  identity pool and provider, the CI and canary service accounts with the roles of `ci-policy.sh` and
  `canary-policy.sh`, all set and checked before `gcp` is added to `test/e2e-vendors.txt`. Details in
  [GCP](#gcp).
- **OCI** (vendor `oci`): a dedicated compartment with a compartment quota and a budget alert, the imported Flatcar images, the identity domain with its confidential application and trust, the CI and
  canary groups with the policies of `ci-policy.sh` and `canary-policy.sh`, all set and checked before `oci` is added to `test/e2e-vendors.txt`. Details in [OCI](#oci).
- **Hetzner Cloud** (vendor `hcloud`): a dedicated project with a low server limit and a usage alert, both set and checked before `hcloud` is added to `test/e2e-vendors.txt`, and the secret
  `HCLOUD_TOKEN` in the environments `e2e` and `e2e-sweeper` (read-write) and `e2e-canary` (read-only); the optional
  variable `E2E_HCLOUD_LOCATIONS`. Details in [Test and develop](testing.md#the-l5-lane-of-hetzner-cloud).
- **Environment `e2e`**: the secret `AWS_ROLE_ARN` (the CI role) and the variables
  `AWS_REGION`, `TS_OAUTH_CLIENT_ID` and `TS_AUDIENCE`, **required reviewers** (the
  maintainers who approve runs) with "Prevent self-review" on and administrators not allowed
  to bypass them, and no branch restriction to `main`: pull request runs
  are on `refs/pull/<n>/merge`, and the release gate's on the tag `refs/tags/v*`. A
  deployment-branch policy, if any, must allow those refs (a `v*` tag policy and the
  merge refs of pull requests), so the controls that matter are the reviewers, the fork
  rule and the role's own limits.
- **Environment `e2e-canary`**: the secret `AWS_ROLE_ARN` (the read-only role) and
  optionally `AWS_REGION`; no reviewers (it runs on a schedule); deployments limited to
  `main` (a scheduled run is on the default branch, and a manual run from another branch must
  not reach even the read-only role without a review).
- **Environment `e2e-sweeper`**: the secret `AWS_ROLE_ARN` (the CI role, the same as in `e2e`)
  and the variable `AWS_REGION` (the lane's region); **no reviewers** (it runs every three
  hours and must not wait for an approval); deployments limited to `main`, so that a manual
  run from any other branch is refused (its code would hold the role without a review).
- **Environment `release`** (existing): required reviewers for the publish.
- **IAM**: the OIDC provider, the CI role (trust: the subjects of the `e2e` and the
  `e2e-sweeper` environments; the policy printed by `ci-policy.sh`; two-hour maximum session)
  and the canary role (trust: the `e2e-canary` environment's subject; the policy printed by
  `canary-policy.sh`).
- **Quotas** in the lane's region (Standard on-demand 16 vCPUs, Standard spot 4), the
  Spot service-linked role, the budget alert.
- **Tailscale**: the tags `tag:scarpenter-ci` and `tag:scarpenter-ci-node`, the grants, and the
  federated identity for the `e2e` environment (the canary does not join the tailnet).
- **Repository settings**: a ruleset on `main` requiring the status check `e2e-result` from
  GitHub Actions, which repository administrators bypass entirely (they can still push to
  `main` directly), the tag ruleset on `v*`, approval for outside collaborators' workflows,
  and the label `e2e-canary` (the canary creates it if it is missing). There is no
  pull-request rule, so the reviews are not enforced.
- **`.github/CODEOWNERS`** (in this repository): the owners of `.github/workflows/`, `test/`,
  `hack/` and the file itself, the paths that run with the CI roles and the release
  credentials. GitHub requests their review on a pull request that touches them
  automatically; it is a request, not a gate, because the ruleset has no pull-request rule.

The sweeper (`e2e-aws-sweeper.yml`) runs in the environment `e2e-sweeper`, not `e2e`: the
reviewers of `e2e` would make every scheduled sweep wait for an approval. What keeps that
environment safe without reviewers is its limit to `main`: the only code that reaches the role
through it is the reviewed sweeper of `main`.

### Azure

The Azure lane ([The L5 lane of Azure](testing.md#the-l5-lane-of-azure)) uses GitHub's OIDC
straight to Azure (no secret), and it is **not yet a line of `test/e2e-vendors.txt`**: its
guards are Azure Policy assignments, a role scope and quotas that only a live subscription can
show. In the subscription of the CI infrastructure, the operator provides:

- **A dedicated resource group** in the lane's region (its own VNet, subnet and network security
  group), with the Azure Policy assignments "Allowed virtual machine size SKUs" (deny, the sizes of
  `test/e2e-azure/defaults.sh`, printed by `ci-policy.sh`) and "Allowed locations"; the resource
  providers `Microsoft.Compute` and `Microsoft.Network` registered; the vCPU quotas (Standard
  regional and the lane's families 16, Spot 4) and Standard public IPs; a budget alert on the
  subscription to a person; the marketplace terms of the Flatcar offer, if it has any.
- **Two user-assigned managed identities**: the CI identity (the role of `ci-policy.sh`, scoped to the
  resource group) with federated credentials for the subjects of `e2e` and `e2e-sweeper`, and the
  read-only canary identity (the custom role of `canary-policy.sh`) for `e2e-canary`; the audience
  `api://AzureADTokenExchange`.
- **The variables** `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`, `AZURE_RESOURCE_GROUP`
  and `AZURE_REGION` in the environments (IDs, not secrets); `e2e-sweeper` and `e2e-canary` limited to
  `main`; the tailnet items as for AWS.
- **Before `azure` is added to `test/e2e-vendors.txt`**, the operator checks: a VM of a size outside the
  pins is refused by the policy in the lane's resource group; a resource outside the region or the
  resource group is refused; the quotas are no higher than the lane needs and the budget alert fires; the
  sweeper, run with the real CI identity, deletes a planted tagged VM, NIC, public IP and disk; the canary
  identity can read everything the canary reads and write nothing. Then the line is added, with the note
  in `defaults.sh` and the selftest's check changed; until then the selftest checks that it is absent.

### GCP

The GCP lane ([The L5 lane on Google Cloud](testing.md#the-l5-lane-on-google-cloud)) uses GitHub's
OIDC through workload identity federation (no key file), and it is **not yet a line of
`test/e2e-vendors.txt`**: its guards are a dedicated project, IAM conditions on resource names and
quotas that only a live project can show. The operator provides (nothing here is created by this
repository):

- **Project**: a dedicated one, with the Compute Engine API, a budget alert and quotas in the lane's region (`CPUS` 24, `PREEMPTIBLE_CPUS` 4; a new project
  starts lower), and, for the `default` scenario, its `default` network.
- **Workload identity**: a pool and a GitHub OIDC provider whose attribute condition names the repository and the environments `e2e`, `e2e-sweeper` and `e2e-canary`; the CI
  service account, impersonable (`roles/iam.workloadIdentityUser`) by the principal sets of the environments `e2e` and `e2e-sweeper`; the canary service account, by the principal set of
  `e2e-canary` only.
- **Roles**: the three custom roles and the conditional bindings printed by `test/e2e-gcp/ci-policy.sh bindings`, and the read-only role printed by `test/e2e-gcp/canary-policy.sh bindings`.
- **Environments**: `e2e` (reviewers), `e2e-sweeper` (`main`, no reviewers) and `e2e-canary` (`main`) each with the variable `GCP_PROJECT`, the variable `GCP_REGION` (optional, default
  `us-central1`) and the secrets `GCP_WORKLOAD_IDENTITY_PROVIDER` and `GCP_SERVICE_ACCOUNT`.
- **Tailscale**: the same tags, grants and federated identity as for AWS.
- **Before `gcp` is added to `test/e2e-vendors.txt`**, the operator checks: the regional role's
  conditions bite (an instance insert in another region, and a network, subnetwork, firewall rule or
  resource policy whose name does not start with `scarpenter-`, are refused for the CI service
  account: that every Compute method honours a name condition is unverified); the project holds
  nothing but the lane, its `CPUS` and `PREEMPTIBLE_CPUS` quotas in the lane's region are no higher
  than the lane needs (24 and 4; no IAM condition can deny a machine type, so the quotas are the
  ceiling) and the budget alert reaches a person; the sweeper, run with the real CI identity, deletes
  a planted labelled instance and a marked firewall rule, subnetwork, network and resource policy,
  and the teardown finds a planted labelled disk; the canary service account reads what the canary
  reads and writes nothing; `e2e-sweeper` and `e2e-canary` are limited to `main` and the provider's
  attribute condition names only this repository and the three environments. Then the line is added,
  with the note in `test/e2e-gcp/defaults.sh` and the selftest's check changed; until then the
  selftest checks that it is absent.

### OCI

The OCI lane ([The L5 lane on OCI](testing.md#the-l5-lane-on-oci)) uses GitHub's OIDC through an IAM identity domain's token exchange (**no API key**), and it is **not yet a line of
`test/e2e-vendors.txt`**: its guards are a dedicated compartment, a compartment quota and policy conditions that only a live tenancy can show. The operator provides (nothing here is created by this
repository):

- **Compartment**: a dedicated one, with the region subscribed, a **compartment quota** (the Quotas service) that caps compute cores and memory at what the pins need (8 OCPUs and 48 GB of the E5 family; the Arm families too when `E2E_ARM64` is true) and a budget alert.
- **Images**: the Flatcar images of the pinned release in the compartment, per architecture ([oci-images.md](../how-to-guides/oci-images.md)).
- **Identity domain**: an application with the client credentials grant and a trust for `https://token.actions.githubusercontent.com` that maps this repository's subjects for the environments `e2e`, `e2e-sweeper` and `e2e-canary` (the audience: `OCI_OIDC_AUDIENCE`) to identity domain users;
  the CI group (the first two) with the statements printed by `test/e2e-oci/ci-policy.sh policy <group> <compartment> <region>`, the canary group with `test/e2e-oci/canary-policy.sh policy <group> <compartment>`.
- **Environments**: `e2e` (reviewers), `e2e-sweeper` (`main`, no reviewers) and `e2e-canary` (`main`) each with the variables `OCI_COMPARTMENT`, `OCI_TENANCY`, `OCI_IDENTITY_DOMAIN_URL`, `OCI_CLIENT_ID` and, optional, `OCI_REGION` (default `eu-frankfurt-1`),
  `OCI_HOME_REGION`, `OCI_OIDC_AUDIENCE` (default `oci-federation`), and the secret `OCI_CLIENT_SECRET`.
- **Tailscale**: the same tags, grants and federated identity as for AWS.
- **Before `oci` is added to `test/e2e-vendors.txt`**, the operator checks: the policy's region condition bites (a launch in another region, and a write in another compartment, are refused for the CI group: which verbs accept the conditions is
  unverified); the compartment holds nothing but the lane, the compartment quota is no higher than the lane needs (no policy can deny a shape, so the quota is the ceiling) and the budget alert reaches a person; the
  sweeper, run with the real CI identity, deletes a planted tagged instance, group, subnet, route table, gateway and VCN and a boot volume, and the teardown finds a planted boot volume; the canary's group reads what the canary reads and
  writes nothing; `e2e-sweeper` and `e2e-canary` are limited to `main` and the identity domain's trust names only this repository and the three environments. Then the line is added, with the note in `test/e2e-oci/defaults.sh` and the
  selftest's check changed; until then the selftest checks that it is absent.

### UpCloud

UpCloud's lane ([The UpCloud lane](testing.md#the-upcloud-lane)) needs none of the IAM and OIDC
items above, and it is **not yet a line of `test/e2e-vendors.txt`**: an UpCloud token can deny no
plan and no zone, so the sub-account's resource limits are the only bound on a bug, and the
operator verifies them first (below). In the UpCloud account of the CI infrastructure, the operator
provides:

- **A sub-account `scarpenter-ci`** with API access, server, storage and network access and no
  panel access, and small **resource limits** (cores, memory, networks) set by the main account:
  they are the cost guard, because an UpCloud token has no scopes, cannot be read-only and cannot be
  federated. The token is created by hand (a tenant of an infrastructure tool may not be able to mint it).
- **The secret `UPCLOUD_TOKEN`** in the environments `e2e` (reviewers as for AWS), `e2e-sweeper` (limited
  to `main`, no reviewers) and `e2e-canary` (limited to `main`), and the variable `UPCLOUD_ZONE`
  (optional, default `fi-hel1`). The token expires (a sub-account's tokens last up to a year): the
  canary's `API token` row fails when it does, and the rotation is manual: create a token, replace the
  three secrets, delete the old one.
- **The persistent Flatcar templates** of the pinned release in the lane's zone, labelled
  `scarpenter-use=true` (the sweeper never touches them), built by the images repository and imported
  by the operator.
- **The tailnet** items above (tags, grants, the federated identity for `e2e`); the canary does not join it.
- **Before `upcloud` is added to `test/e2e-vendors.txt`**, the operator checks: the sub-account's
  limits are set and no higher than the lane needs (`E2E_MIN_CORES` and `E2E_MIN_MEMORY_MB` of
  `test/e2e-upcloud/defaults.sh`, which the canary compares them with; networks and storage small); a
  launch beyond them is refused (`SERVER_CORE_LIMIT_REACHED`); a spending alert of the account reaches a
  person (whether UpCloud can alert on one sub-account's spend is unverified); `e2e-sweeper` is limited
  to `main`. Then the line is added; the lane's selftest checks that it is absent until then.

### Vultr

The Vultr lane (`test/e2e-vultr/`) differs from AWS where Vultr has no equivalent, and it is
**not yet a line of `test/e2e-vendors.txt`**: the guards that credentials cannot give (plan 20,
open question 7) are met by the account's own limits, which the operator sets first.

- **A dedicated Vultr account or sub-account** with a low account limit (maximum instances and
  maximum instance cost, in the console under Account, Limits): the ceiling that replaces the
  denied types and region. A key cannot deny plans or regions (ACLs are coarse).
- **Environment `e2e`**: the secret `VULTR_API_KEY`, the key of a dedicated sub-user, and the
  variable `VULTR_REGION` (default `ewr`). Vultr has no OIDC for callers, so the key is static and
  its **IP allowlist must admit GitHub-hosted runners** (`0.0.0.0/0`): the required reviewers and
  the fork rule are the wall. **Environment `e2e-sweeper`**: the same secret and variable, no
  reviewers, limited to `main`. **Environment `e2e-canary`**: the secret `VULTR_API_KEY` of a second
  sub-user with read access only, and optionally `VULTR_REGION`.
- **The Flatcar snapshot**, once per pinned version, created by the account's operator from the
  images repository's public URL (a snapshot-from-URL call), described
  `flatcar-<channel>-<version>-amd64` (the pins of `test/e2e-vultr/defaults.sh`). The sweeper
  never deletes a snapshot, and neither does a run.
- **Run tag**: the instances carry `scarpenter.sparecores.com/cluster=scarpenter-ci-<run id>`
  (and the NodeClass's `ci-created-at` and `ci-run`); firewall groups and VPCs have no tags, so
  their description carries the ownership marker (managed ones) or is `<cluster>-byo` (the ones the
  `byo` scenario brings), and the sweeper reads that.
- **Cost**: `vc2-1c-1gb` and `vhf-1c-1gb` (about a cent per hour); a full run launches at most six
  small instances. Whether a destroyed instance bills the started hour is unverified (plan 20,
  spike S7), so assume one hour each.
- **Before the first run**: the maintainer runs the spikes of plan 20, section 11 (S1 the image
  import, S4 the error texts, S5 the minimal ACLs), sets the account limit, and then adds `vultr`
  to `test/e2e-vendors.txt`.

## Alibaba Cloud provisioning

The Alibaba Cloud lane (`test/e2e-alicloud/`) is written and validated statically; **it has not run**,
and everything below that says "unverified" is settled by the first run. The control plane,
the tailnet and the environments are the AWS lane's; what differs is the account and the images.

- **Environments.** `e2e`, `e2e-canary` and `e2e-sweeper` as above, each with the secrets
  `ALICLOUD_ROLE_ARN` and `ALICLOUD_OIDC_PROVIDER_ARN` (the role of that environment: the CI role in
  `e2e` and `e2e-sweeper`, a read-only one in `e2e-canary`), and the variables `ALICLOUD_REGION`
  (default `ap-southeast-1`) and `ALICLOUD_OIDC_AUDIENCE` (the client ID the RAM OIDC provider lists;
  default `sts.aliyuncs.com`, unverified). The ARNs are secrets and never in the repository.
- **RAM.** An OIDC identity provider for `token.actions.githubusercontent.com` whose client ID is that
  audience, a CI role whose trust policy names that provider and the subjects of the `e2e` and
  `e2e-sweeper` environments (the immutable subject form of the AWS notes in
  [Test and develop](testing.md#provisioning-outside-this-repository); the claims are `oidc:iss`,
  `oidc:aud` and `oidc:sub`), and a canary role for the subject of `e2e-canary`. The maximum session
  duration must allow two hours (the controller keeps the session the scripts exchanged). The policies
  are printed by `test/e2e-alicloud/ci-policy.sh <account UID> [region]` and `canary-policy.sh`;
  neither calls Alibaba Cloud, and the condition keys of the CI policy (`acs:RequestTag/...`,
  `acs:ResourceTag/...`, `ecs:InstanceType` on `ecs:RunInstances`) are unverified: if the type deny
  does not bite, the vCPU quota below is the ceiling, and the lane stays out of `test/e2e-vendors.txt` until it has been shown to.
- **The account.** Alibaba Cloud accounts need **real-name verification** before they can buy ECS in some
  regions and a **balance** for pay-as-you-go resources (a minimum: unverified). **Spot may be refused by
  account rules** (`InvalidAccount.NotSupportSpot`): then the `arm64` and `spot` scenarios fail with
  `never launched`, and the spot type of the lane has to be on-demand until the account is allowed it.
  The lane needs a **vCPU quota** of **16 pay-as-you-go and 4 spot** in the region, which the preflight
  and the canary check (`DescribeAccountAttributes`); the defaults of a new account are unknown, and a
  raise is a request in Quota Center. The Alibaba Cloud CLI `aliyun` must be on the runner for the
  scenarios, the diagnostics and the teardown (whether `ubuntu-latest` has it, and how the workflow
  installs it: unverified).
- **The images** (rung 3 of the image ladder: the images are the operator's own; the lane's
  preflight only checks that they resolve, it imports nothing). Per region the lane runs in, and for both
  architectures:
  1. Take the Flatcar disk image of the release from the artifact of the Spare Cores images repository
     (the release is `E2E_FLATCAR_CHANNEL` and `E2E_FLATCAR_VERSION` of `defaults.sh`; the file name and the
     format are that repository's, unverified here; `ImportImage` takes RAW, VHD, QCOW2 and VMDK).
  2. Upload it to an **OSS bucket in the region** (a bucket per region; `ImportImage` reads it from there).
  3. The RAM role **`AliyunECSImageImportDefaultRole`** must exist and be allowed to read that bucket
     (its managed policy name: unverified).
  4. `ImportImage` with the architecture (`x86_64` or `arm64`), the platform `Others Linux`, and the name
     `Flatcar-<channel>-<version>-<arch>` with `<arch>` `amd64` or `arm64`; **the name is what the
     driver looks up**, with the owner `self` (the account of the credentials), and exactly one available
     image of that name may exist per region and architecture.
  5. Tag the image `flatcar-channel=<channel>` and `flatcar-version=<version>` (the driver requires the
     name or the tags to say the pinned release, and refuses the image if they disagree).
  6. `CopyImage` to every other region of the lane (it keeps the name as given in
     `DestinationImageName`; whether it copies the tags: unverified, so tag the copy too).
  7. The image must read its user-data without a token: `ImdsSupport=v1` (Ignition's aliyun provider
     does a plain GET; the hardened mode makes it fail with 403), and for an eighth-generation or later
     type such as `ecs.c8y.large` it must advertise NVMe support (otherwise the launch is refused as
     `ImageNotSupportInstanceType`). How the import sets these features: unverified.
