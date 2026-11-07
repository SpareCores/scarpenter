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

## Gates

CONVENTIONS gates 1–4; `hack/check-docs.py` clean (links, no local paths, no
private links); the local quickstart doc test passes.
