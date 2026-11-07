# 11 — Docs and the v1 release

Depends on: 07, 10 (after the coordinator's first green L5 run).
Read `docs/plans/v1/CONVENTIONS.md` first. Spec: overview §1, §13.

## Goal

Documentation a newcomer can follow from zero to a node, and the first tagged
release.

## Scope

- **Tutorials**: `docs/tutorials/quickstart-aws.md` (k3s on a laptop or VM,
  Tailscale, AWS credentials, chart install, NodeClass, NodePool, a Pod with
  an exact instance type, watching the node come and go, cleanup) and
  `docs/tutorials/quickstart-local.md` (the same flow with the qemu driver
  and the dev image). Every command block is executed in a doc test where
  possible (local tutorial fully, AWS tutorial up to the first AWS call).
- **How-tos**: `docs/how-to-guides/select-instances.md` (exact choices,
  attribute ranges, metric thresholds and percentiles, selection profiles,
  objectives), review of `write-profiles.md` and
  `bring-your-own-resources.md` (plan 06) with real AWS examples,
  `docs/how-to-guides/add-a-vendor.md` (driver contract, built-in defaults,
  profile blocks, resource kinds,
  conformance suite, catalog mapping checks against the snapshot, error
  table, e2e workflow template, docs to write); review all how-tos and
  references written by earlier plans for consistency.
- **Explanation**: `docs/explanation/architecture.md` — the stable parts of
  the overview (components, Create flow, bootstrap, catalog), so readers no
  longer need the plan.
- **README**: status, features, a 10-line example, links.
- **Release**: `CHANGELOG.md`, tag `v0.1.0` (the coordinator pushes it),
  verify the release workflow output (image signatures, chart pull and
  install into kind).

## Carried over from the plan 07 review

- The add-a-vendor guide points at `pkg/vendors/qemu` and its fake-host conformance
  fixture (failures injected by failing the commands the driver runs), and at
  `vendors.CatalogProvider` for a development driver with its own static catalog.
- Split the conformance subtest "idempotency key semantics": its first half (a replay
  returns the original instance) needs no second zone, and drivers with one zone skip
  it entirely (N15).
- If the qemu driver or an image importer ever runs outside development, verify the
  Flatcar release's `.DIGESTS.asc` against the image signing key (the digest file
  alone proves integrity, not authenticity) (N13).
- Document the L4 lane's Headscale (pinned release, per-run keys) in the testing how-to
  and the `tailscale` template how-to as the way to try the template without a SaaS
  account.

## Carried over from the plan 08 review

- AWS how-to and tutorial pages (credentials, IAM policy, default versus managed VPC).
- Document the semantics: `isolation: none` means nobody (only `inbound` applies);
  `isolation: run` without a run key makes the NodeClass one run (its group gets the
  member rule); `ImagesReady` is `False` only for a named region without an image or
  when no region has one, else `True` with `ImagesPartial`, and regions without any
  image are gated; the IPv4 price is a constant; `aws.imageOwner` applies to status and
  launch.
- Add-a-vendor guide: type facts via the vendor API with an epoch, and why not via the
  catalog (tracker lock order, usage of off-catalog types); internal resource kinds
  (`runSecurityGroups`); the conformance hooks `ResourceParams` and
  `type/capacity-type`; per-call timeouts; an independent fake with documented fidelity
  gaps; an `ImageReporter` error that is transient must not gate a region.

## Carried over from the plan 09 review

Repository settings (they cannot be set from a work tree; do them before the first
stable tag, and list them in `docs/how-to-guides/testing.md#releases`):

- A tag ruleset on `v*`: creation, update and deletion restricted to maintainers (it also
  blocks re-pointing a tag, which would republish a version with a new digest).
- The environment `release` with required reviewers and "Selected tags: v*". The
  workflow's own checks (commit on main, the environment) are guardrails against mistakes:
  the workflow file of the tagged commit is the one that runs, and `packages: write` comes
  from `GITHUB_TOKEN`, which no environment gates.
- GHCR: the packages `scarpenter`, `scarpenter-dev` and `charts/scarpenter` public, linked
  to the repository, Actions write access limited to it; default workflow permissions
  read-only; "Require approval for all outside collaborators" for fork pull requests.

Verify with a throwaway pre-release tag (`v0.1.0-rc.1`) before v0.1.0:

- The tag path end to end: the verify job including the ancestry check, the environment
  approval, both images pushed multi-arch, the SBOM extraction non-empty, the `helm push`
  output parsing (`Digest:`), image and chart signatures, the release with notes and
  assets, `--prerelease` set, `latest` and the minor tag not moved (the highest-stable-tag
  logic of the `verify` job).
- `cosign verify` with the identity of the release notes for the image, the dev image and
  the chart, with the cosign version users will have (cosign 3 bundle format, GHCR
  referrers or tag fallback); state the minimum cosign version in the notes.
- `helm install` from `oci://ghcr.io/sparecores/charts/scarpenter` on a clean cluster at
  the minimum Kubernetes version (1.31) and on a cluster that already runs Karpenter
  (coexistence section of the install guide; the chart's `crds/` skips existing CRDs), with
  Helm 3 and Helm 4 (the chart is only tested with Helm 4.3 so far; `test/chart` honours
  `HELM=<binary>`). Apply `crds/` to a kind cluster at the minimum version in the release
  checklist.

Other:

- Decide the long-term home of core's CRDs (a separate CRD chart, or templated CRDs behind
  `crds.core.install`, so that ownership is explicit); NodeOverlay and CapacityBuffer are
  not shipped and must be added when SCarpenter turns the gates on.
- Chart README and icon for Artifact Hub; the install example version in the how-tos.
- The uninstall Job: not for v0.1.0. If wanted later, an opt-in `cleanup.onUninstall`
  implemented as a subcommand of the controller binary (`scarpenter-controller cleanup
  --wait`) so that the hook uses the same signed image, with a documented timeout;
  it must delete only NodePools whose `nodeClassRef.group` is SCarpenter's.
- L4: apply `config/rbac-core` instead of `test/qemu/manifests/rbac.yaml` (still a wider
  duplicate of core's rules).
- Overview section 12 and 15: the new values and `--cluster-dns=none`, `--version`,
  chart-owned RBAC (`config/rbac-core`), the cluster DNS discovery and the
  `ClusterDNSUnknown` reason, the minimum Kubernetes version 1.31 (CEL `isCIDR`), the
  coexistence rules, the lease name `<release>-leader-election`.
- Reproducible image builds (`SOURCE_DATE_EPOCH`, `rewrite-timestamp`) are optional.

## Gates

CONVENTIONS gates 1–4; `hack/check-docs.py` clean (links, no local paths, no
private links); the local quickstart doc test passes.
