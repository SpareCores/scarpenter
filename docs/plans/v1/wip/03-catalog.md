# 03 — Catalog: Keeper data to instance types and offerings

Depends on: 02. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §5 (all), §13.5; 00a §2, §5.2, §6.4, §7; ADR-019.

## Goal

Pure, fully tested code that turns Keeper data plus a scope (vendor, regions)
plus driver facts (zones per region with zone IDs) into Karpenter
`[]*cloudprovider.InstanceType` with priced offerings, and keeps it fresh.
No driver calls, no Kubernetes client in `pkg/catalog` itself.

## Scope

- **Snapshot**: `test/data/sc-snapshot/queries.yaml` and the fetched data for
  overview §5.1 (AWS all regions, both allocations; `/table/server`,
  `/table/zone`, `/table/region`, `/healthcheck`), plus a small per-vendor
  sample for hcloud and gcp (one region each, to keep the mapping honest for
  vendors without zones and with different zone formats). Also the benchmark responses the
  test metrics need (Geekbench single- and multi-core, stress-ng, one
  lower-is-better benchmark), as in sc-karpenter-nodepool's snapshot. Refresh workflow
  `.github/workflows/refresh-sc-snapshot.yml` using `sc-keeper-go`'s fetcher
  (port from `sc-karpenter-nodepool`). Fetching the snapshot is the one live
  Keeper access allowed in this plan.
- **`pkg/catalog`**:
  - `Source` interface with a Keeper implementation (via `sc-keeper-go`
    `Fetcher`, one `data_version` per refresh) and a static implementation
    (`pkg/catalog/static`: YAML file of types and offerings, for dev drivers).
  - Mapping of overview §5.3–§5.5: names (sanitise, truncate + hash), capacity
    (vCPU, memory with overhead percent, pods, ephemeral storage), overhead
    (kube-reserved formula with attribution, system-reserved, eviction),
    requirements and labels, offerings per zone and capacity type, region-level
    prices replicated to zones, USD prices, status filtering.
  - Zone mapping input: `map[region][]Zone{ID, Name}` supplied by the caller
    (the driver later); offerings for zone IDs not in the map are dropped;
    vendors without zones use zone = region.
  - Scope handling: the union of NodeClass scopes; recompute when it changes.
  - `Catalog` object: holds the last good snapshot per vendor, refresh loop
    with jitter, stale-while-error and max age (overview §5.2), metrics
    (`scarpenter_catalog_age_seconds`, `_refresh_errors_total`,
    `_instance_types`, `_offerings` by vendor).
  - `InstanceTypes(scope, filter)` with the requirement pre-filter of overview
    §4 and a cache keyed by (snapshot version, scope, filter, unavailable
    sequence). The scope is per NodePool: the vendors it allows ∩ the vendors
    its NodeClass configures.
- **`pkg/catalog/labels`** (00a §6.4, §7): attribute labels (00a §7.1, the
  CPU flag set from `catalog.cpuFlagLabels`), metric labels (`metric-<m>`,
  `-pct` as a percentile across all vendors of the NodeClass, `-per-usd` on
  offerings; benchmark queries through `sc-keeper-go` as sc-karpenter-nodepool
  does), selection membership (`selection In [...]`, evaluating each profile
  with `sc-keeper-go`'s selection engine on every refresh), location labels
  (country, continent via a static continent-name map, city, state,
  green energy), and the profile labels (`storage-profile`,
  `network-profile`, `placement-profile`: `default` plus the profiles a
  `Supported` hook allows; the hook returns only `default` until plan 06).
  **Registration with core** (decided in the plan 02 review): core reads
  `WellKnownLabels` when its controllers start (node metrics take their label
  names from it) and the set is an unlocked global map, so keys are never
  registered at runtime. Before `WithCoreControllers`, list the SCNodeClasses
  with the API reader and register their `metric-<m>` keys (and every fixed
  key); `cpu-flag-<f>` keys come from the static curated enum of
  `catalog.cpuFlagLabels`, all registered. A NodeClass that later defines a
  metric not registered at start gets `RestartRequired=True` naming the keys
  (its metric labels are not stamped until a restart).
  **Every key is defined on every instance type**, `DoesNotExist` where the
  value is unknown or nothing applies (00a §2). Selection objective pricing:
  when a NodePool pins one selection profile with a non-`Price` objective,
  its offerings are priced by that objective (00a §7.3).
- **§15 Q11**: a unit test with core's scheduling requirements proves that pod
  `Gt`/`Lt` match numeric instance-type and offering requirements and that
  `DoesNotExist` excludes types for pods requiring the key.
- **`pkg/unavailable`**: the cache of overview §5.6 (keys, TTLs, sequence
  number), clock-injectable.
- **Tests**: golden instance-type output for a fixed set of AWS types
  (`t3a.small`, `m7i.large`, `c7g.xlarge`, a GPU type, a type with a name
  needing sanitising if any exists — else synthetic), per-vendor mapping tests
  on the hcloud/gcp samples, label sanitising and hashing, zone-ID mapping,
  stale-while-error behaviour with an erroring replay server, unavailable-cache
  TTLs, pre-filter correctness (a filtered result equals core's own filtering
  of the unfiltered result for a table of NodePool requirements). Benchmark
  (`go test -bench`) of building the full AWS catalog: time and allocations
  recorded in the report.
- **Docs**: `docs/reference/labels-and-offerings.md` (every label, its source
  field in Keeper, unknown-value handling, metric scaling and percentiles,
  selection profiles with a link to sc-karpenter-nodepool's selector
  reference).

## Gates

CONVENTIONS gates 1–3. Report the full-catalog numbers (types, offerings,
build time, memory) for AWS all regions vs. three regions.
