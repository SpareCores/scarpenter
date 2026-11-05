# 01 — `sc-keeper-go`: extract the Keeper client and the selection engine into a shared module

Depends on: 00. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §3.2, §5.1, §13.5; 00a §7; ADR-005, ADR-019.

## Goal

One Keeper client and one selection engine for SCarpenter and
`sc-karpenter-nodepool`: the public Go module `github.com/SpareCores/sc-keeper-go` (repository already created, empty
except `LICENSE`, MPL-2.0, cloned next to this repository as
`../sc-keeper-go`). `sc-karpenter-nodepool` switches to it with no behaviour
change. The module gains the `/table/*` endpoints SCarpenter needs.

This plan touches three working trees: `../sc-keeper-go` (new code),
`../sc-karpenter-nodepool` (switch to the module), and this repository (only
the facts recorded for overview §15 Q1 go into the report). Commit nothing in
any of them.

## Scope

### `sc-keeper-go`

- Move `sc-karpenter-nodepool/internal/keeper` (client, fetcher, query,
  types, benchmarks) to the module root package `keeper` and
  `internal/keeper/keepertest` to `keepertest`, preserving history-free but
  otherwise unchanged code and tests. Public API stays as it is today; only
  package paths change.
- New endpoints on `Client` and `Fetcher` (memoised, `data_version` pinned
  like the existing ones):
  - `TableServers(ctx)` → `GET /table/server` → `[]TableServer` (the
    `Server` table columns: vendor, api_reference, family, vcpus, cpu_cores,
    memory_amount, cpu_architecture, cpu_manufacturer, cpu_family,
    accelerator_*, status, …; decode by field name, ignore unknown fields).
  - `TableServerPrices(ctx, PriceQuery)` → `GET /table/server_prices` with
    `vendor`, `vendor_regions` (chunked if Keeper limits them; find out),
    `allocation`, `currency` → `[]ServerPrice` (vendor, region, zone, server,
    allocation, price, currency, status).
  - `Zones`/`Regions` exist; check they expose `zone_id`, `name`,
    `api_reference` and region status.
- `keepertest`: replay for the new endpoints (match on path + vendor +
  vendor_regions + allocation + currency).
- **Selection engine** (ADR-019): move `sc-karpenter-nodepool/internal/selection`
  (candidates from Keeper rows, filters, ranking, band, data-quality policy,
  churn) to package `selection`, and the `Selector` and `Ranking` API types it
  uses (plus `Normalize`, `Objective`, `PriceBasis` and friends) out of
  `sc-karpenter-nodepool/api/v1alpha1` into package `selection/api` with their
  kubebuilder markers and generated deepcopy, so that both projects embed them
  in their CRDs (controller-gen must keep generating the same
  `SCNodePool` schema: prove it with an unchanged CRD diff). The package has no
  Kubernetes client dependency. Its tests move along; the reference document
  `docs/reference/selectors.md` stays in sc-karpenter-nodepool and gains a
  note that SCarpenter uses the same objects.
- Snapshot fetcher, generalised: `cmd/sc-snapshot` (Go, preferred so both
  consumers need only Go) or the existing Python script moved to
  `hack/sc-snapshot/fetch.py`, driven by a **query list file** (YAML: path +
  params per entry, plus vendor/region expansions) and writing the same
  `manifest.json` format the replay server reads (path, params, file, rows,
  SHA-256, fetch time, Keeper database hash; fail if the hash changes during
  the fetch; byte-identical output for identical data).
  `sc-karpenter-nodepool`'s extra (EKS Auto Mode fees from the AWS price
  list) stays in that repository as a separate step of its workflow.
- Repository basics: `go.mod` (Go version as in `sc-karpenter-nodepool`),
  `README.md` (what it is, usage example, replay server usage, stability
  note: v0, minor versions may break), golangci config, a CI workflow (lint,
  test, `go mod tidy` diff check), a release convention (tags `v0.x.y`), a
  small committed test snapshot under `keepertest/testdata/` so the module's
  own tests are offline.

### `sc-karpenter-nodepool`

- Replace `internal/keeper` and `internal/selection` imports, and the moved
  API types, with `github.com/SpareCores/sc-keeper-go` packages
  (a `replace ../sc-keeper-go` directive in `go.mod` for now, with a comment;
  the coordinator removes it after tagging `v0.1.0`). Delete the moved
  packages.
- Point `refresh-sc-snapshot.yml` and its docs at the shared fetcher with a
  committed query list reproducing today's snapshot exactly (the manifest's
  query set and file names must not change; prove it by regenerating the
  manifest's query list and diffing).
- Its gates (its own `CONVENTIONS.md`): `make generate manifests` no diff,
  `make lint`, `make test`, `make test-crd`, `make test-kwok` — all green with
  no behaviour change.

### Facts to record (overview §15 Q1)

Allowed live access for this plan: the public Keeper API, read-only, a
handful of requests. Record: `/table/server_prices` filter parameters and
limits; row count and gzipped size for AWS over all regions with both
allocations; whether on-demand rows are per zone or per region; whether
`currency=USD` converts; HTTP cache headers of `/table/*`.

## Gates

1. `sc-keeper-go`: `go vet ./...`, golangci-lint, `go test ./...` (offline),
   `go mod tidy` no diff.
2. `sc-karpenter-nodepool`: all gates of its conventions, green.
3. Report: the §15 Q1 facts, the public API of the module, anything that
   changed in the moved code (should be nothing but import paths).
