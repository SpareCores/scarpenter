# 13 — Keeper catalog snapshots

Depends on: 03, and on Keeper's snapshot routes (below). Read
`docs/plans/v1/CONVENTIONS.md` first. Spec: overview §5.1–§5.2. The
coordinator folds overview §5.1 in when part B lands; until then §5.1 still
describes the live endpoints.

Status: **blocked.** The whole plan waits until the Keeper specs are merged
and implemented in sc-keeper: the sc-specs ADRs of 2026-11-08
`keeper-snapshots`, `benchmark-config-id` and `keeper-select-routes`. That is
expected in 1–3 days. Meanwhile the other plans (10, 11) keep going. Once
Keeper is ready, part A comes first, then part B.

## Goal

Every catalog refresh reads one pinned Keeper data version, from files Keeper
builds once per database hash, and SCarpenter selects instance types locally
from that data. No live `/servers` queries and no `/server_prices`.

## Keeper (as specified; not implemented yet)

- **Headers.** Every response has `SC-Data-Hash` (the database hash) and
  `SC-Data-Published-At` (RFC 3339, when that sc-data file was published).
- **Snapshot routes.**
  - Tables with a `vendor_id` column: `/snapshot/{hash}/{table}/{vendor}`.
  - Benchmark scores: `/snapshot/{hash}/benchmark_score/{vendor}/{benchmark_config_id}`.
    The spec's open question 2 proposes per benchmark instead,
    `.../{vendor}/{benchmark_id}`, and we agreed.
  - Columnar JSON: one array per column, all the same length.
  - Gzip only: a request that does not accept gzip gets `406`.
  - `Cache-Control: private, max-age=86400, immutable`.
- **Contents.**
  - Only `ACTIVE` and `PLANNED_FOR_RETIREMENT` rows, and only rows whose
    references are included. Every reference resolves within a hash.
  - Every column except `observed_at`.
  - Prices converted to USD.
  - Not filtered by unit, allocation, operating system or price.
- **Access.** An API key is required, and a request costs 1 credit (the limit
  is 60 credits a minute).
- **Availability.** Every replica serves a hash for at least a day after it
  first appears. A hash a replica can't serve gets
  `404 /problems/snapshot-not-available`, with `current` (that replica's hash)
  in the body. `/healthcheck` lists the replica's `snapshot_hashes`.
- **Benchmark configs.**
  - `/benchmark_configs` rows gain `benchmark_config_id`
    (`<benchmark_id>~<8 hex>`). Clients copy the id and never compute it.
  - The ids change when the benchmark restructure lands, so we don't persist
    them.
  - `/benchmark_configs` will list every benchmark; today it hides the
    stress-ng benchmarks other than `stress_ng:best*`.

**Open at Keeper.** Our review asked for these; part B follows whatever is
decided.
- **The API key.** Three options:
  1. Snapshots with a key for SCarpenter and sc-karpenter-nodepool. We lean to
     this one.
  2. Snapshots for SCarpenter, and `/servers/select` for sc-karpenter-nodepool.
  3. No key: both stay on today's routes, and part B is dropped.
- **Enum form.** The database stores `ACTIVE`, `HOUR`, `ONDEMAND`, `GB_MONTH`
  and `X86_64`, while the API uses `active`, `hour`, `ondemand` and
  `GB/month`. We asked for the API form.
- **Pinning.** We asked that clients be told to pin from `/healthcheck`,
  because it is the only uncached route.
- **Unchanged files.** We asked for ETags, or a manifest at
  `/snapshot/{hash}` with a checksum per file.

## Part A — local selection

Selection profiles now send `/servers` queries
(`selection.PlanQueries` → `ServersAll` in `pkg/catalog/keeper.go`): region
chunks of three, push-down bounds loosened by the churn margin, and data that
is not pinned to the refresh's hash. Part A selects from the catalog data the
refresh already holds instead. Part A is useful whatever Keeper decides, and
part B then only swaps the transport.

1. **`sc-keeper-go/selection`:** a candidate builder from catalog data. For
   each server it takes the specs, the lowest price per allocation over the
   profile's regions, and the scores. `Evaluate` stays shared. The `/servers`
   builder stays, for sc-karpenter-nodepool under option 2.
2. **Filtering rules, written down and tested.** These are the rules
   `/servers` applies:
   - orderable status;
   - Linux or blank operating system;
   - hourly prices;
   - price above zero;
   - which allocation counts as the price.

   Read them in sc-keeper. Don't guess them.
3. **`pkg/catalog`:** `loadSelections` uses the new builder on `model.Data`.
   Metric scores keep today's source until part B.
4. **Equivalence test:** on the recorded Keeper data of
   `test/data/sc-snapshot/`, the local builder and the `/servers` path select
   the same members for the profiles in the tests. Document every difference
   and justify it.
5. **Score join:** key scores by `server_id`, and map them to `api_reference`
   through the server rows. They differ for GCP and Hetzner.

Part A must not change the labels or offerings of instance types. Selection
membership may change only where step 4 documents why.

## Part B — snapshots

**A refresh, in order:**
1. `GET /healthcheck`, and pin `database_hash`. If it equals the current
   catalog's hash, stop: nothing changed.
2. `GET /benchmark_configs`, which is live, and resolve each metric to its
   config. A config with no rows in the pinned hash is an invalid metric; the
   rest of the refresh goes on.
3. For each vendor in scope, fetch `vendor` (billing facts, below), `server`, `region`, `zone`,
   `server_price`, `storage_price`, `ipv4_price`, and one score file for each
   benchmark a metric uses.
4. **Re-pin once.** On `snapshot-not-available`, pin the `current` hash from
   the body and fetch everything again. A second such `404` keeps the previous
   catalog until the next refresh.
5. **Local filters:**
   - architectures `x86_64` and `arm64`;
   - prices that are hourly, `ondemand` or `spot`, Linux or blank operating
     system, and above zero;
   - regions in scope;
   - a blank zone is copied into every zone the driver reports;
   - scores with `resource_type=server`, matched by config text (never by
     re-encoded JSON).
6. Selection profiles run on the same data, with part A's builder.

**Things that change with it:**
- **AWS storage prices.** They come from `storage_price` and replace the
  `/storage_prices` call in `pkg/vendors/aws/prices.go`.
- **The AWS IPv4 price.** It comes from `ipv4_price` and replaces the
  constant 0.005 USD/h and `SCARPENTER_AWS_IPV4_PRICE`. The rows are in
  USD and CNY today, and Keeper converts them to USD.
- **The API key** is required under options 1 and 2:
  - `--keeper-api-key-file` becomes mandatory, and the chart value is
    required;
  - a missing or rejected key shows as a clear condition;
  - plan 10 adds a Keeper key secret;
  - L3 and L4 keep replaying recorded data, so they need new recordings but
    no key.
- **Data age.** `SC-Data-Published-At` becomes a metric and is shown in the
  catalog status.
- **Removed:**
  - `/server_prices`, `/table/server_prices` and the full `/table/server`;
  - the 3-region chunking and the `--keeper-prices-regions-per-request` flag;
  - selection's `/servers` path in SCarpenter.
- **Memory.** Decode the columnar files as streams; Azure's `server_price` is
  32 MB of JSON. Report the resident heap for AWS with three regions and for
  all vendors, and compare it with plan 06's 217 and 377 MiB.
- **On-disk copy.** Keep the gzipped files of the pinned hash on disk, and
  replace the set when the hash changes. A restart within the same hash then
  needs no Keeper call. This replaces the earlier per-hash SQLite idea.
  Dropping the Go price slice after the offerings are built is out of scope:
  revisit it only if the memory numbers call for it.

## Vendor billing data (ADR-024, plan 23)

[ADR-024](../../../explanation/DECISIONS.md#adr-024-billing-period-aware-node-retention-accepted-2026-11-10)
needs each vendor's billing rule. `GET /table/vendor` is live today (checked 2026-11-10) and has
`billing_increment_seconds`, `minimum_billing_seconds`, `stopped_server_charged` and `billing_comment`;
there is no `/vendors` route. Plan 13 does not gate it: [plan 23](23-billing-retention.md) reads it from
the live route as soon as plan 14 is done. This plan makes it part of the snapshot.

What this plan adds:

1. **`sc-keeper-go`: `Vendors(ctx)`** over `/table/vendor`, with the same version pinning as the other
   table methods, and a `Vendor` type whose billing fields are pointers (null is legal: "unknown"). Plan 23
   adds the method (it is small and must not wait for snapshots); this plan's part B only moves it to
   the snapshot transport.
2. **The `vendor` table in the snapshot.** The refresh fetches `/snapshot/{hash}/vendor/{vendor}` (step 3
   above). The table has a `vendor_id` column, so it rides on the "every column" rule today; the Keeper
   spec must state that the four billing columns are part of the snapshot contract, so that a rename is a
   breaking change. A catalog from a hash that lacks the table (an older Keeper) has no billing data and
   retention is off with the status reason `NoKeeperData`, never a guess.
3. **Finer data Keeper lacks** (raised as [SpareCores/sc-keeper#112](https://github.com/SpareCores/sc-keeper/issues/112)). The vendor row is one
   worst-case value per vendor and cannot express:
   - OVH regions (EU, CA and APAC per second with a 60 s minimum, the US hourly, per Keeper's comment);
   - Alibaba's minimum by vCPU count (600, 300, 120 s);
   - UpCloud's stopped-server billing by family (Developer and General Purpose billed, Cloud Native not);
   - monthly caps (Hetzner, Vultr 672 hours, GPU 730, UpCloud Simple plan 672 hours);
   - what starts the clock (create call, provisioning complete, running). Not how periods are counted: no vendor documents
     clock-hour billing (OVH US's own example refutes it), so the shape has no `anchor` column and periods always count from
     the start.

   Proposed shape, a table next to `vendor`, whose row is the default (NULL in `region_id` or `family` means any,
   the most specific match wins):

   ```
   billing_rule(vendor_id, region_id NULL, family NULL,
                increment_seconds, minimum_seconds,
                start_event,                 -- create_call | provisioning_complete | running
                monthly_cap_hours NULL,      -- cap over calendar_month
                stopped_server_charged,
                comment)
   ```

   Until it exists, drivers override per region and family in code (plan 23, `BillingRule`), and every
   override has a plan or ADR reference so it can be deleted when Keeper has the data. `ServerPrice.unit` is
   the price's time unit, not a billing granularity (it is `HOUR` for every row), so it cannot stand in.
4. **Data inconsistencies to raise with the data owners** (found 2026-11-10 against the vendors' own pages):
   - OVH is 3600/3600 in Keeper, but EU, CA and APAC are per second with a 1-minute minimum, and the US is
     hourly per the US terms while the US support guide says per second (conflicting: open);
   - Azure is 1/300 in Keeper, but Azure's pricing page says full minutes (increment 60) and no 300 s minimum is
     documented;
   - `minimum_billing_seconds` for Alibaba (600, the worst case of three) is a minimum charged on an early release;
     plan 23 uses it as the one boundary of a minimum-then-per-second rule, not as a period.

   Retention stays off for a vendor until its plan's live check settles the value, whatever Keeper says.

## Scope

- **sc-keeper-go.** Part A: the local candidate builder. Part B: the
  snapshot client, with re-pinning, gzip, columnar decoding and the headers,
  and `benchmark_config_id`.
- **SCarpenter.** `pkg/catalog` (part A and part B), the vendor table and the AWS storage and
  IPv4 prices. Part B also covers the options, the chart and the docs.
- **Not here.**
  - sc-keeper itself.
  - sc-karpenter-nodepool: under option 1 it moves to snapshots in its own
    repository; under option 2 it moves to `/servers/select`.

## Gates

CONVENTIONS gates 1–3, plus L3 (`make test-kwok`), because the catalog feeds
every scenario. Also:
- **Part A:** the equivalence report from step 4.
- **Part B:** the memory numbers, the size and time of one refresh for AWS and
  for all vendors, and new recordings in `test/data/sc-snapshot/`.
