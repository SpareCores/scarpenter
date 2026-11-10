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
- [What the infrastructure repository provides](#what-the-infrastructure-repository-provides)

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
   and lints the chart. Nothing is published and no cloud is touched.
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
     `release` creates the GitHub release with notes, the chart package and the SBOMs
     (pre-release for a tag with a suffix, which does not move `latest`).
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
(the AWS lane's are in the second column):

| Guard | AWS lane |
|---|---|
| A budget alert on the account that hosts the lane, to a person | account-level, in the infrastructure repository |
| The CI role denies instance types the lane does not use | `DenyOtherInstanceTypes` in `test/e2e-aws/ci-policy.sh` (from `defaults.sh`) |
| The CI role cannot act outside the lane's region | `DenyOutsideRegion` |
| Every resource carries a run tag, so cost and leftovers are attributable | `scarpenter.sparecores.com/cluster=scarpenter-ci-<run id>` and `ci-created-at` |
| A sweeper that deletes what a dead run left behind, by tag and age | `e2e-aws-sweeper.yml`, every three hours, environment `e2e-sweeper` |
| A run time limit | 90 minutes for the job (60 for a smoke), 55 (25) for the scenarios step; the teardown always runs |
| Small, cheap types; spot where the vendor has it | `t3a.small`, `t4g.small` (spot), `r6a.large` for the placement-group scenario |
| A teardown that fails the job when anything tagged remains | `teardown.sh` |
| A quota that bounds the damage of a bug | the vCPU quotas of the account (16 on-demand, 4 spot needed) are the ceiling |
| A read-only role for the canary | `canary-policy.sh`, a role of its own, environment `e2e-canary` |
| Approval for every run of pull request or release code that reaches the account | the environment `e2e`'s required reviewers (the sweeper runs only the code of `main`, environment `e2e-sweeper`) |

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

In the infrastructure repository (below): the cloud account or project, the CI role and the
canary role, the environment secrets, the quotas, and the tailnet tags and ACL.

## What the infrastructure repository provides

None of the following lives here; it is created by the `scarpenter-e2e` tenant of the
infrastructure repository (`sc-infra`, `prod/k8s`) and this repository assumes it:

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
- **Repository settings**: the status check `e2e-result` required on `main`, the tag
  ruleset on `v*`, approval for outside collaborators' workflows, and the label
  `e2e-canary` (the canary creates it if it is missing).

The sweeper (`e2e-aws-sweeper.yml`) runs in the environment `e2e-sweeper`, not `e2e`: the
reviewers of `e2e` would make every scheduled sweep wait for an approval. What keeps that
environment safe without reviewers is its limit to `main`: the only code that reaches the role
through it is the reviewed sweeper of `main`.
