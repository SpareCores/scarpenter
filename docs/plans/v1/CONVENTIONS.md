# Conventions for plan-executing models

You are executing one implementation plan for **SCarpenter**, a multi-vendor
Karpenter cloud provider written in Go. Read this file fully before starting.
Every plan assumes you did.

## Ground rules

1. **Do NOT `git commit`, `git push`, or rewrite history.** Leave all changes in
   the working tree. A coordinator reviews and commits. (Plan 01 also changes
   the `sc-keeper-go` and `sc-karpenter-nodepool` working trees; same rule.)
2. Do not start on a dirty tree unless the plan says so. If `git status` shows
   unrelated changes, stop and report.
3. Work until **all gates are green**. Never report success with failing or
   skipped-because-broken tests. If stuck after several distinct attempts, stop
   and write an honest report.
4. Never weaken a test to make it pass. Fixing a test's bug is fine.
5. The design is fixed by `docs/plans/v1/wip/00-overview.md` and
   `docs/explanation/DECISIONS.md`. Do not edit them to match an
   implementation detail. If reality contradicts them (a fact in overview §15
   turns out different, an upstream API differs), implement the closest thing
   to the plan's intent and record the contradiction in your report; the
   coordinator updates the overview.
6. **No sensitive data in the repository**: no cloud account IDs, ARNs,
   cluster names, endpoints, tailnet names, e-mail addresses, credentials or
   anything taken from a real cluster or a developer machine. The repository
   is public. Examples use `example.com`, `my-cluster`, `123456789012`.
7. **Run everything in the dev container** (below). Do not install tools on
   the host. The host only needs Docker (and `/dev/kvm` for the L4 lane).
8. **Never touch live systems**: no AWS calls (CLI, SDK, including read-only
   and dry-run calls), no Tailscale API calls, no kubectl or Helm against any
   cluster other than local kind/k3s clusters you created, no triggering of
   workflows that use cloud credentials. The only live network access allowed
   is to public, unauthenticated artifact URLs (Flatcar releases,
   sysext-bakery releases, Go modules, container images) and, only when a
   plan says so, the public Keeper API.

## Orientation (read in this order)

- `docs/plans/v1/wip/00-overview.md` — the design. Each plan names its
  sections.
- `docs/explanation/DECISIONS.md` — decisions (ADRs).
- `docs/plans/v1/README.md` — plan sequence and status.
- `docs/how-to-guides/testing.md` (from plan 02 on) — dev container, lanes.

## Repository layout

See overview §3.1. Module path: `github.com/SpareCores/scarpenter`. License:
MPL-2.0 (the repository `LICENSE`); add the standard MPL-2.0 header to new Go
files. Code adapted from Apache-2.0 projects (Karpenter core, the AWS
provider) carries a comment naming the source file at a pinned commit.

## Dev container

Ported from `sc-karpenter-nodepool` by plan 02: `hack/devcontainer/Dockerfile`
with every tool pinned (Go, controller-gen, setup-envtest with assets,
golangci-lint, kind, kubectl, helm, kwok, k3s, qemu, python3, actionlint,
shellcheck, yq, jq, butane). `hack/dev.sh <command...>` runs a command in it as
the calling user, with the repository at `/src`, a named volume for Go caches,
`--network host` and the Docker socket. `hack/dev.sh --privileged …` adds
`/dev/kvm`, `NET_ADMIN` and a private network namespace for the L4 lane.

```bash
hack/dev.sh make test
hack/dev.sh make lint
hack/dev.sh go test ./pkg/catalog/...
```

## Gates (definition of done)

Run in this order; all must pass:

1. `hack/dev.sh make generate manifests` produces no diff (generated code and
   CRDs are committed).
2. `hack/dev.sh make lint` (golangci-lint, `go vet`, `actionlint`,
   `shellcheck`, `hack/check-docs.py`) — clean.
3. `hack/dev.sh make test` — L1 and L2 (unit, golden, envtest), zero
   failures, no network access (Keeper calls go to the snapshot replay
   server; artifact fetches go to local fixtures).
4. From plan 04 on: `hack/dev.sh make test-kwok` (L3).
5. From plan 07 on, when the plan touches `pkg/bootstrap`,
   `pkg/vendors/qemu` or `test/qemu`: `hack/dev.sh --privileged make
   test-qemu` (L4).
6. Plan-specific gates listed in the plan.
7. `docs/plans/v1/README.md`: set the plan's status to `review`.

## Code style

- Go, controller-runtime, Karpenter core v1.15.x. Small packages; pure
  functions where possible (`pkg/catalog`, `pkg/bootstrap` rendering and
  `pkg/unavailable` have no Kubernetes client).
- The core is vendor-neutral: nothing outside `pkg/vendors/<name>/` may import
  a vendor SDK or branch on a vendor name. If you need to, the driver contract
  is missing something: extend it (and the conformance suite) instead.
- Comments explain non-obvious intent and trade-offs, never narrate code.
- Errors: wrap with `fmt.Errorf("...: %w", err)`; driver errors carry their
  class (overview §7.2); no panics in the manager.
- Tests: table-driven, co-located `_test.go`; envtest for controllers; golden
  files under `testdata/` (update with `make golden`), reviewed like code.
- Configuration: flags with `SCARPENTER_*` env fallbacks, documented in the
  controller reference.
- Every new metric, condition reason, Event reason, label, annotation, flag and
  Helm value is added to the reference docs in the same plan.

## Reporting back

End with a concise report: files added or changed; design decisions where the
plan was ambiguous; facts from overview §15 you verified (with the result);
exact gate results (command + pass/fail + test counts); anything deliberately
deferred, one line each.
