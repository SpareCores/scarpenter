# 13 — Keeper catalog snapshot

Depends on: 03. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §5.1–§5.2. This plan does not change overview §4 or plan 04.
Do not start it while plan 04 is in review, and do not edit
`04-cloudprovider-and-fake-driver.md` or the CloudProvider, driver, quota, or
L3 code that plan owns.

Status: proposal. It is not the next plan after 04. Plans 05–12 do not depend
on it. The coordinator folds overview §5.1 in when this is accepted; until
then §5.1 still describes the live price endpoints.

## Goal

Feed the catalog with one consistent, cheap-to-serve snapshot per vendor, and
keep a small on-disk copy of the price rows that can be replaced on every
database hash. Karpenter keeps scheduling from the in-memory offerings.

## Why

A refresh runs about once an hour and pins `database_hash` from
`GET /healthcheck`. It needs, for the vendors that have an `SCNodeClass` and
the regions those classes allow (every active region when a class lists none):

- Specs of `active` and `planned-for-retirement` `x86_64` and `arm64` servers.
- Regions and zones.
- One hourly USD price per (server, region, zone, allocation) for `ondemand`
  and `spot`. The operating system is blank or Linux, the price row is
  `active`, and the price is positive. A blank zone is copied into every zone
  the driver reports. Planned-for-retirement servers keep their prices.
- One score per server for each benchmark a NodeClass asks for, keyed by
  `api_reference`. Selection profiles still query `GET /servers` and rank locally.

Anonymous `GET /server_prices` embeds the server, vendor, region, and zone in
every row (about 4 KB: 137 MB for four AWS regions, 32,970 rows) and holds one
of two heavy-job slots. `GET /table/server_prices` is the flat row, but the
Linux hourly slice is 135,239 rows for AWS and 316,326 for Azure. Selecting
the AWS slice takes about 0.75 s before the rows are encoded, and the route is
`private, no-store`, so every cluster would run that query on Keeper. It stays
behind an API key.

Numbers below are from a Keeper SQLite dump on 2026-11-06.

## Keeper

Built once, when Keeper loads a new SQLite file (it already copies the file,
adds indexes, and fills `ServerExtra` then). One document per vendor per
hash. The route sends those bytes. It does not query SQLite and does not take
a heavy-job slot.

`GET /table/server_prices` is unchanged: API key, 10 credits, `private, no-store`.

### `GET /table/server_prices/snapshot`

Anonymous. `vendor` and `data_version` are required. `data_version` must be
the loaded hash; otherwise `409` with the current hash. No `vendor_regions`,
`columns`, or `currency`: each would be a different object. The document is
already filtered to price row `active`, unit `hour`, allocation `ondemand` or
`spot`, price `> 0`, operating system blank or `Linux` (case-insensitive).

AWS is 15.2 MB of JSON and 0.86 MB gzipped; Azure is 35.2 MB and 1.70 MB.
Both are over 10 MB, which CloudFront will not compress, so the route sends
the gzip it built, with `Content-Encoding: gzip`, and
`Cache-Control: public, max-age=31536000, immutable`. One representation per
hash. `X-Database-Hash` is the hash.

```http
GET /table/server_prices/snapshot?vendor=aws&data_version=<database_hash>
```

```json
[
  {"region_id": "us-east-1", "zone_id": "use1-az1", "server_id": "m7i.large", "allocation": "ondemand", "price": 0.1008},
  {"region_id": "us-east-1", "zone_id": "use1-az1", "server_id": "m7i.large", "allocation": "spot", "price": 0.0314},
  {"region_id": "us-east-1", "zone_id": "", "server_id": "t4g.micro", "allocation": "ondemand", "price": 0.0084}
]
```

All eight vendors are 542,129 rows and about 3.2 MB gzipped. Azure is more
than half of the rows. A new vendor is one more file, built at the same load.

### The other reads

`GET /table/server` gains optional `columns` (as `/table/server/select`
already has). The full table is 6,114 rows and 13.2 MB, just past what
CloudFront compresses. The columns the catalog reads are 7.9 MB, one shared
cached URL. No vendor or status filter: that would split the cache, and the
client already drops other vendors, statuses, and architectures. `storages`
stays; the disk count is its length.

`GET /table/region` and `GET /table/zone` stay as they are (269 and 596 rows,
already one cached object each).

`GET /table/benchmark_score` is new. `benchmark_score` has 3,049,964 server
rows and is not dumped. One benchmark for one vendor is an indexed aggregate
(the index Keeper already builds at load): AWS `stress_ng:bestn` is 1,191
rows, 59 KB. Required: `vendor`, `benchmark_id`. Optional: `benchmark_config`
(canonical JSON, matched as on `/servers`), `resource_type=server`. The value
is `max(score)` of active rows. Response:
`{"server_id", "api_reference", "score"}`. Anonymous, cached for an hour.
Unknown benchmark or config: an empty list, which the client reports as an
invalid metric and continues the refresh.

`GET /servers` gains optional `columns`. With it, each object is those server
columns plus `price` and, when `benchmark_id` is set,
`selected_benchmark_score`. The filters, the 3-region anonymous cap, and the
3-credit cost stay. A selection cannot be precomputed: the filter set is per
NodeClass.

Cacheable responses carry `X-Database-Hash`. The client retries the whole
refresh when it differs from the healthcheck hash.

## SCarpenter

Per vendor in scope, after `/healthcheck`:

1. `GET /table/server` with `columns`, plus `/table/region` and `/table/zone`.
2. `GET /table/server_prices/snapshot` for that vendor. Gunzip, keep rows
   whose region is in scope, map `ondemand` → `on-demand` and `spot` → `spot`.
3. `GET /benchmark_configs`, then one `GET /table/benchmark_score` per metric.
4. `GET /servers` with `columns` for each selection, chunked by three
   `vendor_regions`, ranked locally as today.

The downloaded body is not kept. The snapshot holds `model.Data` until the
next refresh succeeds, and a failed refresh keeps the previous one, so both
exist during a refresh. The price rows are the resident cost: about 35 MB for
all AWS regions (135,239 prices, stored again in the per-server index), about
150 MB when every current vendor is in scope. Offerings built per NodeClass
are a second block of that order when they share requirement maps, and larger
when every offering carries its own metric requirements. Servers, regions,
zones, and a few score maps are a couple of megabytes. A region allow-list
cuts the price rows in proportion (three AWS regions are 26,459 prices).

### On-disk prices

The price rows also go to a new SQLite file per hash, written with
`journal_mode=OFF` and `synchronous=OFF`, then atomically renamed over the
current path. The previous file stays until the process drops it. A crash
during the write leaves the previous hash in place.

All eight vendors are 46 MB and took 4.1 s to write, including reading them
out. The same rows as gzipped TSV are 3.1 MB. Three AWS regions query back in
about 6 ms. Replacing that file on every hash (spot publishes about every
five minutes; the catalog refresh stays hourly) is the whole rewrite. Do not
restore `sc-data-all` (1.26 GB) on the cluster, and do not update one
database in place.

The file is the source for a restart and for a NodeClass that appears before
the next refresh. After the offerings for the current classes are built, the
Go price slice can be dropped; the scheduler still walks the in-memory
offerings. The page cache is the only extra RAM the file adds.

## Scope

- **sc-keeper**: render the snapshot at database load; the new routes and
  `columns`; `X-Database-Hash`. `GET /table/server_prices` stays keyed.
- **sc-keeper-go**: fetch the snapshot and the score route; `columns` on
  `/table/server` and `/servers`.
- **`pkg/catalog`**: `KeeperSource` reads the snapshot, filters regions, and
  writes the per-hash SQLite file. Behavior of the built instance types is
  unchanged. Tests replay a fixture of the snapshot document; no live Keeper
  except the existing snapshot-refresh workflow.

## Gates

CONVENTIONS gates 1–3, plus the Keeper test suite for the new routes. Report
the resident heap of an all-vendor catalog and of AWS with three regions, and
the time and size of one price-file replacement.
