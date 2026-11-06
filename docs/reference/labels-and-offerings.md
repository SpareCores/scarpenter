# Labels and offerings

How the Spare Cores catalog (the Keeper API) becomes Karpenter instance types
and priced offerings: names, capacity, every label with its source field and
what happens when the value is unknown, how metrics and selection profiles are
evaluated, how often it refreshes and what happens when Keeper is down. The
names of the labels are listed in [Labels and annotations](labels.md); the
design is in the [overview](../plans/v1/wip/00-overview.md#5-catalog-keeper-data-to-instance-types-and-offerings)
and [00a](../plans/v1/wip/00a-node-options.md#7-instance-selection). The code is
`pkg/catalog` and `pkg/catalog/labels`.

## Table of Contents

- [Terminology](#terminology)
- [Instance types](#instance-types)
  - [Names](#names)
  - [Capacity and overhead](#capacity-and-overhead)
  - [Which servers become instance types](#which-servers-become-instance-types)
- [Offerings](#offerings)
  - [Zones and zone IDs](#zones-and-zone-ids)
  - [Prices](#prices)
  - [Availability](#availability)
- [Labels](#labels)
  - [Unknown values](#unknown-values)
  - [Scheduling and topology labels](#scheduling-and-topology-labels)
  - [Attribute labels](#attribute-labels)
  - [Metric labels](#metric-labels)
  - [Selection profiles](#selection-profiles)
  - [Location labels](#location-labels)
  - [Profile labels](#profile-labels)
  - [Registration with Karpenter core](#registration-with-karpenter-core)
- [The catalog](#the-catalog)
  - [Scope](#scope)
  - [Refresh and staleness](#refresh-and-staleness)
  - [The pre-filter](#the-pre-filter)
  - [Metrics](#metrics)
  - [Scale](#scale)
- [Troubleshooting](#troubleshooting)
- [References](#references)

## Terminology

- **Keeper**: the public API of Spare Cores
  (`https://keeper.sparecores.net`), the only source of instance types and
  prices.
- **Instance type**: a Karpenter `InstanceType`: what a node of one server type
  looks like, with requirements (labels), capacity, overhead and offerings.
- **Offering**: where an instance type can be launched and at what price: one
  per zone and capacity type.
- **Snapshot**: the data of one successful refresh, read at one Keeper
  `data_version` (the database hash of `/healthcheck`).
- **Scope**: the vendors and regions the SCNodeClasses allow; only those are
  read.

## Instance types

### Names

One instance type per vendor server type: `<vendor>.<api_reference>`, for
example `aws.m7i.large` ([ADR-008](../explanation/DECISIONS.md)). The name is the
value of `node.kubernetes.io/instance-type`, so it must be a label value:

- characters outside `[A-Za-z0-9_.-]` become `-`, and the name starts and ends
  with an alphanumeric;
- a name longer than 63 characters is truncated to 56 characters and suffixed
  with `-` and the first 6 hex characters of the SHA-256 of the full name;
- a name that sanitising changed also gets that suffix, so `a b` and `a/b` can
  never collide.

The raw reference is the `scarpenter.sparecores.com/api-reference` label. When
it does not fit a label value that key is `DoesNotExist` on the type;
`Catalog.Lookup(name)` still resolves the name to `(vendor, api_reference)`.

### Capacity and overhead

| Resource | Value |
|---|---|
| `cpu` | Keeper `vcpus` |
| `memory` | Keeper `memory_amount` (MiB) less `kubelet.vmMemoryOverheadPercent` (default 7.5 %, rounded down) |
| `pods` | `kubelet.maxPods` (default 110) |
| `ephemeral-storage` | the root disk size of the vendor's resolved default settings, as the driver reports it; omitted when the driver does not say |

There are no GPU extended resources: without a device plugin they would never
appear on the node and pods requesting them would loop. GPU types are selected
by label.

Overhead (what the scheduler subtracts from capacity to get allocatable):

- **kube-reserved**: the formula of the AWS provider. Memory is 11 MiB per pod
  plus 255 MiB (1465 MiB at 110 pods); CPU is 6 % of the first core, 1 % of the
  second, 0.5 % of the third and fourth and 0.25 % of every core above (70m for 2
  vCPUs, 90m for 8); ephemeral storage 1 GiB. `kubelet.kubeReserved` replaces
  the computed value of each resource it names.
- **system-reserved**: `kubelet.systemReserved` (default: nothing).
- **eviction threshold**: `kubelet.evictionHard`; `memory.available` defaults to
  100Mi. A percentage is taken of the capacity. Only the memory and nodefs
  signals reserve resources the scheduler counts.

### Which servers become instance types

A server type is offered when all of these hold:

- its Keeper `status` is `active` or `planned-for-retirement` (`inactive` and
  `retired` are dropped; retiring types stay so that running nodes do not drift
  with `instanceTypeNotFound`);
- its CPU architecture is `x86_64` (`amd64`) or `arm64`; `i386` and the `*_mac`
  architectures have no Kubernetes equivalent;
- it has at least one usable price in scope (below).

When two servers of a vendor share an `api_reference`, the one with the lower
server ID is used.

## Offerings

One offering per (instance type, zone, capacity type) with a price in scope.
Keeper `ondemand` prices become `on-demand` offerings and `spot` prices `spot`
offerings.

### Zones and zone IDs

Keeper rows carry the zone's portable **zone ID** (`use1-az1` on AWS). Zone names
are account-specific at AWS, so the driver supplies the zones of its own account
as `region → [{ID, Name}]` ([ADR-009](../explanation/DECISIONS.md)):

- `topology.kubernetes.io/zone` is the driver's zone **name**, and
  `scarpenter.sparecores.com/zone-id` the Keeper zone **ID**;
- a price for a zone ID the driver does not know is dropped;
- a zone that Keeper's zone table marks inactive is dropped;
- a region-level price (a row without a zone) is replicated into every zone the
  driver reports for the region;
- a vendor without zones is offered with zone = region (the zone ID is Keeper's,
  or the region name when Keeper has none).

The region label (`topology.kubernetes.io/region`) is the vendor's name of the
region (Keeper's `api_reference`: `europe-west3` at GCP, whose Keeper region ID
is `1300`). A NodeClass's region allow-list accepts the vendor's name, Keeper's
region ID or an alias; only active regions match.

### Prices

`Price` is the hourly USD price of Keeper's `/server_prices` (or
`/table/server_prices` with a Keeper API key) plus the price delta of the
driver's default settings (root disk, public IPv4 where billed; zero until the
drivers exist). Only active, hourly, USD, Linux rows count. With
`pricing.allowIncomplete: false` offerings whose delta is incomplete are
dropped. When several rows give the same zone and capacity type the lowest
wins.

When a NodePool pins one selection profile whose objective is not `Price`,
offerings are priced by the objective instead (see
[Selection profiles](#selection-profiles)).

### Availability

An offering is unavailable (`Available=false`) while the
[unavailable-offerings cache](#unavailable-offerings) has an entry for it.
Quota headroom and launchable-type checks join in plan 04.

#### Unavailable offerings

`pkg/unavailable` holds the offerings that recently failed. Marking, expiry and
the sequence number the catalog's cache key uses are clock-injectable.

| Error class | Key | Default TTL |
|---|---|---|
| `InsufficientCapacity` | (vendor, instance type, zone, capacity type) | 3 min |
| `Unsupported` | (vendor, instance type, zone) | 24 h |
| `QuotaExceeded`, vendor without a quota API | (vendor, region, capacity type) | 30 min |
| `ZoneUnusable` | (vendor, zone) | 10 min |

## Labels

Every key is defined on every instance type or, for the offering-level keys, on every offering, with `DoesNotExist` where the value
is unknown or nothing applies: an undefined key would match every type
([00a section 2](../plans/v1/wip/00a-node-options.md#2-what-karpenter-allows)).
Labels are stamped on the NodeClaim and copied to the Node by core.

### Unknown values

| Fact | When it is `DoesNotExist` |
|---|---|
| any nullable Keeper field | Keeper has no value (`null`) or an empty one |
| a numeric value that rounds to 0 | never: a positive value is at least 1 |
| `cpu-cores`, `hyperthreading` | the core count is unknown |
| `cpu-flag-<flag>` | the type's flag list is empty (not inspected); a measured type without the flag is `false` |
| `accelerator-*` | the type has no accelerator |
| `local-storage-count` | the type has a disk of unknown count; no disk is `0` |
| metric labels | the type has no score |
| `selection` | the type is in no profile |
| location labels | Keeper has no value |

`local-storage-gb` is `0` for a type without local storage, so `Lt 1` selects
diskless types.

### Scheduling and topology labels

| Label | Level | Source |
|---|---|---|
| `node.kubernetes.io/instance-type` | type | [name](#names) |
| `kubernetes.io/arch` | type | `cpu_architecture` (`x86_64` is `amd64`) |
| `kubernetes.io/os` | type | `linux` |
| `karpenter.sh/capacity-type` | type (all), offering (one) | the price's allocation |
| `topology.kubernetes.io/region` | type (all), offering (one) | region `api_reference` |
| `topology.kubernetes.io/zone` | type (all), offering (one) | the driver's zone name |
| `scarpenter.sparecores.com/zone-id` | type (all), offering (one) | zone `zone_id` |

### Attribute labels

Type-level. Integers are rounded to the nearest; use `Gt` and `Lt` for ranges
(2 to 16 vCPUs is `vcpus Gt 1` and `vcpus Lt 17`).

| Label | Keeper field | Conversion |
|---|---|---|
| `vendor` | vendor id | |
| `api-reference` | `api_reference` | only when it is a valid label value |
| `family` | `family` | sanitised |
| `vcpus` | `vcpus` | |
| `cpu-cores` | `cpu_cores` | |
| `memory-mib` | `memory_amount` | MiB |
| `memory-per-vcpu-mib` | `memory_amount` / `vcpus` | |
| `cpu-manufacturer`, `cpu-family`, `cpu-allocation` | `cpu_manufacturer`, `cpu_family`, `cpu_allocation` | sanitised: `-` for other characters |
| `cpu-clock-mhz` | `cpu_speed` (GHz) | x 1000 |
| `cpu-l2-per-core-kib` | `cpu_l2_cache` | KiB per core |
| `cpu-l3-mib` | `cpu_l3_cache_total` (else `cpu_l3_cache`) | KiB / 1024 |
| `hyperthreading` | `vcpus` > `cpu_cores` | `true` / `false` |
| `cpu-flag-<flag>` | `cpu_flags` | `true` / `false`, for the flags in `spec.catalog.cpuFlagLabels` |
| `accelerator-count` | `accelerator_count` | |
| `accelerator-memory-mib` | `accelerator_memory_total` | MiB |
| `accelerator-manufacturer`, `accelerator-model`, `accelerator-type` | `accelerator_manufacturer`, `accelerator_model`, `accelerator_type` | |
| `network-baseline-mbps`, `network-peak-mbps` | `network_speed_baseline`, `network_speed_max` (Gbps) | x 1000 |
| `local-storage-gb` | `storage_size` | GB |
| `local-storage-count` | number of `storages` | |

The flags with a label are the closed, curated set of the `cpuFlagLabels` enum
(defaults: `avx2`, `avx512f`, `amx_tile`, `sha_ni`).

### Metric labels

Defined by `spec.metrics` of the SCNodeClass ([API](api.md)): a benchmark (id and
config, resolved against Keeper's `/benchmark_configs`) or a workload profile.
For a metric `<m>`:

| Label | Level | Value |
|---|---|---|
| `metric-<m>` | type | the score, multiplied by `scale` and rounded to an integer |
| `metric-<m>-pct` | type | the percentile 0 to 100, 100 best |
| `metric-<m>-per-usd` | offering | the scaled score divided by the offering's USD per hour |

- `normalize: PerVcpu` divides the score by the vCPU count before everything
  else.
- The percentile is across all types of **all vendors of the NodeClass**
  (whatever their regions: scores do not depend on the region). A type beats the
  types with a worse score; ties share the lower rank; the best type is 100 and
  the worst 0. For lower-is-better benchmarks (`membench:latency`, ...) the
  direction is flipped so that 100 is still the best.
- `metric-<m>-per-usd` is `DoesNotExist` for lower-is-better benchmarks, where a
  score per dollar means nothing.
- Unbenchmarked types get `DoesNotExist` for all three labels, so any threshold
  excludes them: "fast single core" is `metric-single-core-pct Gt 89`.
- A metric the data source cannot answer for a reason of the metric itself (an
  unknown benchmark config) is `DoesNotExist` on every type and reported by
  `Catalog.Invalid` for the NodeClass controller.
- Benchmark scores are read with one `/servers` query per benchmark and vendor,
  without regions.

### Selection profiles

`spec.selections` hold sc-karpenter-nodepool's `selector` and `ranking` objects
verbatim; the semantics are those of its
[selector reference](https://github.com/SpareCores/sc-karpenter-nodepool/blob/main/docs/reference/selectors.md).
On every refresh each profile is evaluated by the selection engine of
`sc-keeper-go` per vendor of the NodeClass, over the vendor's allowed regions
(all regions when it lists none), on the on-demand basis unless the selector says
otherwise. Each type gets `selection In [<profiles it belongs to>]`, or
`DoesNotExist`. A profile the engine rejects (for example a ranking without a
band or `families.max`) has no members and is reported by `Catalog.Invalid`,
naming the vendor; each vendor is evaluated on its own, so an invalid vendor does
not affect the others' members. A profile without a `ranking` is priced at the
real price when pinned.

A NodePool that pins **exactly one** profile with a non-`Price` objective gets
its offerings priced by the objective, so scheduling, `Create` and
consolidation all optimise it. The price becomes a number whose lowest value is
the best:

| Objective | Price of an offering |
|---|---|
| `ScorePerPrice` | price / score (price x score for a lower-is-better benchmark) |
| `Score` | 1 / score (score for a lower-is-better benchmark) |
| `Price` | the real price |

The score is the profile's ranking score (normalised as the ranking says). A type
without a score cannot be ranked, so its offerings are unavailable. The real price
stays in the `price` annotation and metrics (plan 04).

### Location labels

Offering-level, from Keeper's region table. Unknown is `DoesNotExist`.

| Label | Source |
|---|---|
| `country` | `country_id`, lower case |
| `continent` | a static country to continent map (the UN geoscheme: Russia is Europe; Turkey, Cyprus, Georgia, Armenia and Azerbaijan are Asia; Mexico and the Caribbean are North America): `africa`, `asia`, `europe`, `north-america`, `south-america`, `oceania`, `antarctica` |
| `city`, `state` | `city`, `state`, as slugs (`New York` is `new-york`) |
| `green-energy` | `green_energy`: `true` or `false`; unknown (Keeper's data is tri-state) is `DoesNotExist` |

`city`, `state` and `green_energy` are fields of sc-keeper-go's `Region`
(`/table/region`, nullable).

### Profile labels

`storage-profile`, `network-profile` and `placement-profile` are `In [default,
<profiles valid for the type>]` on every type. The driver decides validity
through its `Supported` hook; until plan 06 every driver reports only `default`.
The type carries the union over its zones; an offering whose zone supports fewer
profiles carries its own list, so a profile a zone cannot host never matches
there. `placement-group` is a free-form run key and is not defined on types.

### Registration with Karpenter core

All keys are registered with core as well-known labels so that NodePools and pods
may use them. Core reads the set when its controllers are built and the set is an
unlocked global map, so keys are never registered at runtime:

- the fixed keys and the `cpu-flag-<flag>` keys of the whole curated set are
  registered at start-up;
- `metric-<m>`, `-pct` and `-per-usd` are registered by
  `Operator.RegisterLabelKeys`, which lists the SCNodeClasses with the API reader
  and runs first in `WithCoreControllers`, before core's controllers exist;
- **a key core does not know is never stamped**: core checks offerings with
  `IsCompatible`, which rejects an offering that defines a key it does not know
  (`label does not have known values`), so an unregistered `metric-<m>-per-usd`
  would exclude every benchmarked type; type requirements are only intersected,
  which ignores unknown keys, so an unregistered type-level key would be
  unselectable. A metric added after start-up is therefore not stamped until a
  restart has registered it; `Catalog.RestartRequired(class)` lists its keys, and the
  NodeClass controller reports them as `RestartRequired=True`.

## The catalog

### Scope

The catalog reads the **union** of the scopes of all SCNodeClasses: for each
vendor the union of the allowed regions (every active region as soon as one
class lists none). A NodePool sees the vendors it allows that its NodeClass
configures, in the class's regions; the pre-filter narrows further. When the
union changes, a refresh starts at once.

### Refresh and staleness

- A refresh reads one consistent `data_version`: `/healthcheck`, `/table/server`,
  `/table/region`, `/table/zone`, `/server_prices` of the scope (flat
  `/table/server_prices` with `--keeper-api-key-file`), `/benchmark_configs` and
  the benchmark and selection queries.
- It runs every `--catalog-refresh-interval` (default 1 h) with 10 % jitter, at
  start-up, and when the scope, a metric or a selection changes. After a failure
  it retries within a minute.
- **Stale-while-error**: a failed refresh keeps the last good snapshot and counts
  in `scarpenter_catalog_refresh_errors_total`. `Catalog.Status(class)` reports
  `Ready` until the snapshot is older than `--catalog-max-age` (default 24 h);
  then `Stale`, which the NodeClass controller turns into `CatalogReady=False`
  with the last error. A class the snapshot does not cover yet is `ScopePending`.
- **Cold start**: without any snapshot the status is `NoSnapshot` and
  `GetInstanceTypes` returns nothing for the class, so pods stay pending instead
  of nodes being launched blind.
- `/server_prices` is large (about 4 KB per row, not compressed by Keeper) and
  `sc-keeper-go` decodes it as a stream into flat rows, so a NodeClass's region
  allow-list bounds the refresh directly.

### The pre-filter

`Catalog.InstanceTypes(query)` narrows the result by the NodePool's requirements
on vendor, region, zone, architecture, capacity type and instance type (`In`,
`NotIn`, `Exists`, `DoesNotExist`). Dropping what they exclude is safe because
core would drop it anyway; the tests prove that what core keeps of the
pre-filtered list equals what it keeps of the unfiltered one, offering by
offering. Results are cached per generation (snapshot, driver facts epoch,
sequence of the unavailable cache) and filter; a handed-out type is never
modified.

### Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_catalog_age_seconds` | gauge | `vendor` | seconds since the snapshot was read |
| `scarpenter_catalog_refresh_errors_total` | counter | | failed refreshes |
| `scarpenter_catalog_instance_types` | gauge | `vendor` | types with a price in scope |
| `scarpenter_catalog_offerings` | gauge | `vendor` | price rows in scope, before the driver's zone mapping |

### Scale

Measured on the committed snapshot (2026-11-06, Keeper database
`d320364be39310dc02b1fd3019d69fca27edf32d`), AWS only, on a 32-thread desktop
CPU; see `pkg/catalog/bench_test.go` (`go test ./pkg/catalog -bench BuildCatalog
-benchmem`):

| Scope | Types | Offerings | Build time | Allocated per build | Retained |
|---|---|---|---|---|---|
| all 33 active regions | 1,382 | 134,727 | about 104 ms | 216 MB | 63 MiB |
| three regions | 1,375 | 26,299 | about 50 ms | 80 MB | 28 MiB |

"Retained" includes the loaded data. Reading the data from Keeper dominates in
production: 138,970 price rows for AWS in the snapshot.

## Troubleshooting

- *Pods stay pending with no instance types*: `Catalog.Status` for the class:
  `NoSnapshot` (Keeper unreachable since start), `ScopePending` (refresh not done
  yet), `Stale`. Check `scarpenter_catalog_refresh_errors_total` and the
  controller log (`catalog refresh failed`).
- *A pod requiring a metric label is never scheduled*: the type is unbenchmarked
  (`DoesNotExist`), or the metric was added after start-up (`RestartRequired`).
- *An instance type is missing*: its Keeper status is not `active` or `planned-for-retirement`, its
  architecture is unsupported, it has no active Linux hourly USD price in the
  allowed regions, or the driver does not report its zones.
- *A selection profile has no members*: `Catalog.Invalid` names the reason (an
  unknown benchmark config, a ranking without a band or `families.max`).

## References

- [Overview section 5](../plans/v1/wip/00-overview.md#5-catalog-keeper-data-to-instance-types-and-offerings) and [00a sections 6.4 and 7](../plans/v1/wip/00a-node-options.md#7-instance-selection)
- [ADR-008, ADR-009 and ADR-019](../explanation/DECISIONS.md)
- [Labels and annotations](labels.md), [Controller reference](controller.md)
- [sc-keeper-go](https://github.com/SpareCores/sc-keeper-go) and the
  [selector reference](https://github.com/SpareCores/sc-karpenter-nodepool/blob/main/docs/reference/selectors.md)
