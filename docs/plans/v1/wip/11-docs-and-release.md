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

## Gates

CONVENTIONS gates 1–4; `hack/check-docs.py` clean (links, no local paths, no
private links); the local quickstart doc test passes.
