# 13 — Keeper catalog snapshots

Depends on: 03, 14 to 21 (the vendor price fetchers this plan replaces) and
23 (the vendor table). Read `docs/plans/v1/CONVENTIONS.md` first. Spec:
overview §5.1–§5.2; the coordinator rewrites overview §5.1 when part B lands.

Status: **done** (all three steps; part B was reviewed). Part B measured on
the recording of Keeper database `40ecd194b481` (published 2026-11-13) and
live the same day:

- **Controller memory** (L3 scenario S10, at rest, RSS; plan 06: 217 and 377
  MiB): AWS three regions **239 MiB** (peak 310 MiB RSS, 184 MiB Go heap in use),
  all AWS regions **384 MiB** (peak 430 MiB RSS, 300 MiB heap), in the final L3 run
  (earlier runs rested at 188-193 and 360-367 MiB: the figure moves by tens of MiB
  with the Go GC); per-scope refresh
  and build numbers, Azure included, are in the controller reference ("Memory
  sizing"), and the chart limit is now 2Gi. No need to drop
  the price slice.
- **One refresh** (`TestRefreshCost`, three test metrics, empty disk copy):
  AWS three regions 13 requests, 0.8 MiB gzip, 0.17 s on the recording and 2.1 s
  live (peak 78 MiB RSS, 18 MiB heap retained by the data); AWS all regions the
  same requests, 0.2 s, peak 118 MiB, 42 MiB retained; all eight vendors 76
  requests (about 76 credits), 2.2 MiB gzip, 0.7 s on the recording and 7.8 s
  live (peak 224-279 MiB RSS, 131 MiB retained by the data; the 520k offerings
  built from it hold the heap at about 1.1 GiB).
- **Rounding:** 2,483 of the 2,488 hcloud, OVH and UpCloud hourly prices
  (79, 1,250 and 1,154 of 79, 1,252 and 1,157) have more than four decimals;
  every other vendor's prices are exact to four. Goldens that read converted
  vendors moved in the last digits.
- L3 (`make test-kwok`): green, 54 min 32 s (after the review fixes; the
  recording is Keeper database `2c401c216`).

## Goal

Every catalog refresh reads one pinned Keeper data version, from the files
Keeper builds once per database hash (snapshots), and SCarpenter selects
instance types locally from that data. Every SCarpenter read from Keeper comes
from the snapshot of the pinned hash, except two live routes:

- `GET /healthcheck`, to pin the hash;
- `GET /benchmark_configs`, to resolve metrics to benchmark configs.

Gone after part B: `/server_prices`, `/table/server_prices`, `/table/server`,
`/table/region`, `/table/zone`, `/table/vendor`, `/servers`, `/storage_prices`
and `/ipv4_prices`, the region chunking and its flag
`--keeper-prices-regions-per-request`.

## Keeper as shipped

sc-keeper `main` at `4d0836a` (live since 2026-11-13). Paths below are in
the sc-keeper repository. `benchmark_config_id` and `/servers/select` did
not ship; nothing here needs them.

**Routes** (`src/sc_keeper/routers/snapshot.py`, all `GET`, all need a key):

| Route | What |
|---|---|
| `/snapshot/{hash}` | The manifest (below). No trailing slash: `/snapshot/{hash}/` is `404 not-found`. |
| `/snapshot/{hash}/{table}` | Global tables: `vendor`, `country`, `compliance_framework`, `benchmark`, `currency`. |
| `/snapshot/{hash}/{table}/{vendor}` | Per-vendor tables, among them `region`, `zone`, `server`, `server_price`, `storage_price`, `ipv4_price` (list: `src/sc_keeper/snapshots.py`, `VENDOR_TABLES`). |
| `/snapshot/{hash}/benchmark_score/{vendor}/{benchmark_id}` | All scores of one benchmark at one vendor, every config in one file. |

- `vendor` is **global**: `/snapshot/{hash}/vendor`. `/snapshot/{hash}/vendor/aws`
  is `404 not-found`.
- `hash` must match `^[0-9a-f]{40,64}$`, else `404 snapshot-not-available`.
  `vendor` and `benchmark_id` must match `^[A-Za-z0-9_][A-Za-z0-9_:.-]*$`,
  else `404 not-found`. Benchmark ids contain `:` (`stress_ng:bestn`); send it
  unescaped.
- An unknown benchmark id at a known vendor answers `200` with an empty array
  for every column, also for a made-up id. An unknown vendor or table is
  `404 not-found`.

**File format** (`src/sc_keeper/snapshots.py`):

- Columnar JSON: one object, one key per column, each an array with one
  element per row, all of the same length. Columns in model order; rows
  sorted by primary key. Compact encoding.
- Every column except `observed_at`, plus Keeper's own columns
  (`server_price.price_monthly`, `null` for spot rows).
- Only rows with status `active` or `planned-for-retirement`; every foreign
  key resolves within the hash (a `null` key passes); a score's `resource_id`
  is a kept server or database of the same vendor.
- Enums in API form, the same strings `/table/*` returns: `status`
  `active`/`planned-for-retirement`, `allocation` `ondemand`/`spot`, `unit`
  `hour`/`month`/`GB/month`, `cpu_architecture` `x86_64`/`arm64`.
  `operating_system` is a plain string (`Linux`).
- JSON columns (`config`, `environment`, `price_tiered`, `storages`,
  `cpu_flags`, `aliases`, ...) are the stored text, verbatim. `config` is
  byte-exact with what `/benchmark_configs` returns as its `config` string.
  In `price_tiered` an unbounded `upper` is the string `"Infinity"`.
- Prices are converted to USD (`currency` is `USD`) and **not rounded**
  (hcloud storage: `0.04862879999999999`). Rows Keeper cannot convert are
  dropped. Nothing is filtered by unit, allocation, operating system or
  price.
- `ipv4_price.unit` differs per vendor: `hour` at AWS, `month` at hcloud.

**Manifest.** `/snapshot/{hash}` is itself columnar:
`{"path": [...], "rows": [...], "sha256": [...]}`, sorted by path. `path` is
the route below `/snapshot/{hash}/` (`server_price/aws`,
`benchmark_score/aws/stress_ng:bestn`, `vendor`); `sha256` is over the
**uncompressed** JSON (gzip bytes can differ between two builds of one hash).
There are no ETags and no `Last-Modified`, on purpose.

**Transport.**

- Gzip only. Every response is `Content-Encoding: gzip`. A request without
  `Accept-Encoding: gzip` (missing header included) gets `406`
  `application/problem+json`, `type` `about:blank`.
- `200`: `Cache-Control: private, max-age=86400, immutable`. Errors:
  `private, no-store`.
- Headers on every response: `SC-Data-Hash` and `SC-Data-Published-At`
  (RFC 3339). On a snapshot `200` they describe the requested hash; on an
  error, the replica's current database.
- Auth: `Authorization: Bearer <key>`. Without a key: `401`
  `{"detail":"Authentication required"}`; a rejected key: `401`
  `{"detail":"Invalid or expired token"}`. Both plain `application/json`, not
  problem+json. Auth is checked before gzip, so a keyless request gets 401.
- Credits (`src/sc_keeper/rate_limit.py`): a snapshot request costs 1, a
  `401` costs 11. Headers `X-RateLimit-Limit`, `X-RateLimit-Cost`,
  `X-RateLimit-Remaining`; the limit is 300 a minute on the public Keeper
  (60 is only the code default). `/healthcheck` is anonymous and costs 1.

**Availability and errors** (problem+json, `type` relative):

| Answer | Meaning | Client |
|---|---|---|
| `503 /problems/snapshot-preparing`, `Retry-After: 5`, body `requested`, `current` | The replica is downloading or building the hash. | Retry the same request after `Retry-After`, about a minute in total. |
| `404 /problems/snapshot-not-available`, body `requested`, `current`, `available` | The replica cannot serve the hash (also: a malformed hash). | Re-pin once to `current` and fetch again what was fetched; a second one: give up until later. |
| `404 /problems/not-found` | Unknown table, vendor or path. | Do not retry. |

A hash stays available for at least a day after it was first built
(`src/sc_keeper/api_description.md`, "Data Versions and Snapshots").

**`/healthcheck`** gains `database_published_at` (RFC 3339) and
`snapshot_hashes` (the replica's local hashes, newest first);
`database_hash` may be `null` (a Keeper without a published database). Pin
from `/healthcheck`, not from a response header.

**`/benchmark_configs`** is unchanged: `benchmark_id`, `category`,
`subcategory`, `config` (the stored text as a string). It still hides the
stress-ng benchmarks other than `stress_ng:best*`; a metric on a hidden one
stays invalid, as today.

## The API key

- SCarpenter never contains a key, a key's location or anything about Spare
  Cores' own infrastructure (no ARNs, parameter paths, account ids). The
  program only accepts a key.
- Injection is unchanged: `--keeper-api-key-file` /
  `SCARPENTER_KEEPER_API_KEY_FILE`; the chart's `keeper.apiKey.existingSecret`
  and `.key`, mounted as a file.
- The key stays **optional at startup**: a self-hosted Keeper may not need
  one. Read the file at every refresh, so a rotated Secret takes effect
  without a restart; an empty file is an error, as today.
- Every Keeper request that can need a key gets it. After part B those are
  the catalog's and the fake driver's; the vendor drivers no longer call
  Keeper.
- **401.** Never retried (it costs 11 credits): the refresh stops at the
  first 401, keeps the previous catalog, logs one line and sets the status
  below. The next scheduled refresh tries once more.
  - Log (error level, once per failed refresh): `Keeper needs an API key`
    with `keeperURL`, `keyConfigured` (bool) and a pointer to the docs
    section "Keeper API key".
  - SCNodeClass condition `KeeperAuthorized` (informational, not a dependent
    of `Ready`, like `RestartRequired`): `False` with reason `APIKeyMissing`
    (no key configured) or `APIKeyRejected` (a key was sent), the message
    naming `--keeper-api-key-file` / `keeper.apiKey.existingSecret` and the
    docs; `True` after the next successful snapshot read. The catalog
    computes it (`Catalog.Status`); the NodeClass controller writes it.
  - Without any catalog, `CatalogReady=False` gets reason
    `KeeperAPIKeyRequired` (instead of `NoSnapshot`) with the same message.
  - Optional metric: `scarpenter_catalog_keeper_unauthorized` (gauge, 1 after
    a 401, 0 after a success).
- The endpoint override stays `--keeper-url` / `SCARPENTER_KEEPER_URL` /
  chart `keeper.url`. The public host appears only as the default
  (`options.DefaultKeeperURL`); every caller takes the configured URL.
- **Getting a key:** https://docs.sparecores.com/api/#authentication.

## Steps

The coordinator commits and pushes each step. SCarpenter consumes
sc-keeper-go through the **pseudo-version of a pushed commit**
(`go get github.com/SpareCores/sc-keeper-go@<commit>`, as `go.mod` does
today); no tag is needed. A local `replace` to `../sc-keeper-go` is fine while
working and is never committed.

A stopped WIP of part A exists in both repositories:
`git stash list` shows "plan 13 part A (stopped WIP)". The new files are in
the stash's untracked tree (`git show 'stash@{0}^3'`). It may serve as a
starting point:

- sc-keeper-go: `selection/candidate.go` (factors out `newCandidate`),
  `selection/catalog.go` and `catalog_test.go` (`AcceptPrice`,
  `CatalogPrice`, `CatalogInput`, `BuildCatalogCandidates`, `BenchmarkKeys`,
  4-digit rounding). `selection/` has not changed since; it applies cleanly.
- SCarpenter: `pkg/catalog/keeper.go`, `pkg/catalog/model/model.go`,
  `pkg/catalog/equivalence_test.go`, `export_test.go`,
  `selection_local_test.go`. `keeper.go`, `model.go` and `go.mod` changed
  since: expect small conflicts. Its `go.mod` `replace` line must not be
  committed.

### Step 1 — sc-keeper-go

Repository `/home/bra/cvs/sparecores/sc-keeper-go` (module
`github.com/SpareCores/sc-keeper-go`). Keep every existing route and type:
sc-karpenter-nodepool still uses them. The existing package `snapshot` and
`cmd/sc-snapshot` are the **test recorder**; in this plan "recording" means
their output and "snapshot" means Keeper's files.

1. **Health.** `Health` gains `DatabasePublishedAt time.Time` and
   `SnapshotHashes []string`; a `null` `database_hash` decodes to `""`.
2. **Data headers.** A `DataVersion{Hash string; PublishedAt time.Time}` read
   from `SC-Data-Hash` and `SC-Data-Published-At`, returned by every snapshot
   method.
3. **Errors.** Typed errors, matched with `errors.As`:
   - `*UnauthorizedError{KeyConfigured bool, Detail string}` for `401`. The
     client never retries a 401 (check: the existing retry covers only
     429/5xx; add a test).
   - `*ProblemError{Status, Type, Title string, Requested, Current string,
     Available []string, RetryAfter time.Duration}` for problem+json bodies;
     helpers `IsSnapshotNotAvailable`, `IsSnapshotPreparing`, `IsNotFound`.
   - A `406` is a `ProblemError` (a bug in the client: it always sends
     gzip).
4. **Snapshot client** (methods on `*Client`, in the root package):
   - `SnapshotManifest(ctx, hash) (Manifest, DataVersion, error)`;
     `Manifest` has `Files []ManifestFile{Path string; Rows int; SHA256 string}`
     and `Lookup(path)`.
   - `SnapshotFile(ctx, hash, path) (*SnapshotBody, error)`:
     `SnapshotBody` holds the gzip bytes as received, the `DataVersion`, and
     `Open() (io.ReadCloser, error)` for the uncompressed stream, and
     `Verify(sha256 string) error` over the uncompressed bytes.
   - Path builders: `TablePath(table)`, `VendorTablePath(table, vendor)`,
     `ScorePath(vendor, benchmarkID)`.
   - Every request sends `Accept-Encoding: gzip` and, with `WithAPIKey`, the
     bearer key. No `data_version` parameter on snapshot routes.
   - `503 snapshot-preparing`: retry the same request after `Retry-After`
     (default 5 s) until about 60 s have passed in total for that request
     (`WithPreparingBudget(d)`, default 60 s; tests use a fake sleep), then
     return the `ProblemError`.
   - `429` and other 5xx: the existing retry.
5. **Columnar decoder**, new package `columnar`:
   - `DecodeRows[T any](r io.Reader, rows int) ([]T, error)`: one
     `json.Decoder` token walk; each column is matched to a field of `T` by
     its `json` tag, and each element decoded into `&out[i].Field`. Unknown
     columns are skipped; a missing column of `T` leaves the zero value; a
     column of the wrong length is an error. `rows` (from the manifest)
     sizes the slice; `-1` grows it.
   - It never holds the whole body: memory is the rows plus one element.
   - Repeated strings of a column share one string (intern per column):
     `vendor_id`, `region_id`, `zone_id`, `server_id`, `unit`, `currency`,
     `allocation`, `operating_system`, `status` repeat across 300k rows.
   - `json.RawMessage` fields keep the bytes verbatim (`config`).
   - Tests: the live samples' shapes (empty arrays, `null`, `"Infinity"` in
     `price_tiered`, nested JSON), wrong lengths, truncated input, and a
     benchmark comparing allocations with `json.Unmarshal` of a row slice.
6. **Row types.** Reuse the types whose `json` tags match the columns
   (`Region`, `Zone`, `Vendor`, `TableServer`, `ServerPrice`). Add
   `StoragePrice` (`vendor_id`, `region_id`, `storage_id`, `unit`, `price`,
   `currency`, `status`), `IPv4Price` (`vendor_id`, `region_id`, `unit`,
   `price`, `currency`, `status`) and `BenchmarkScore` (`vendor_id`,
   `benchmark_id`, `resource_type`, `resource_id`, `config`
   `json.RawMessage`, `score *float64`, `status`).
7. **Part A's candidate builder** (`selection`): from catalog data, for each
   server the specs, the lowest price per allocation over the profile's
   regions, and the scores. `Evaluate` stays shared; the `/servers` builder
   stays (sc-karpenter-nodepool). The price rules are those of `/servers`
   (read them in sc-keeper `src/sc_keeper/queries.py` and `views.py`, do not
   guess): status `active`, unit `hour`, currency `USD` or blank, Linux or
   blank operating system, price above zero, `ondemand` or `spot`, prices
   rounded to 4 decimal places as `/servers` does. Write each rule down in a
   doc comment and test it.
8. **`keepertest` serves the snapshot routes** from a recording:
   - the manifest, global and per-vendor tables and score files of the
     recorded hash, as the stored gzip bytes with `Content-Encoding: gzip`,
     `Content-Type: application/json`, `SC-Data-Hash`,
     `SC-Data-Published-At` and the `Cache-Control` above;
   - `406` without `Accept-Encoding: gzip`;
   - `404 snapshot-not-available` for another hash (`current` = recorded
     hash, `available` = it); `404 not-found` for a path outside the
     manifest; a recorded-manifest path that was not recorded is a `404`
     whose `title` names the path (a test bug, not a Keeper case);
   - `200` with empty arrays for an unrecorded benchmark id of a recorded
     vendor (the columns of a recorded score file);
   - `/healthcheck` synthesised with `database_hash`,
     `database_published_at` and `snapshot_hashes`;
   - test options: `RequireAPIKey(key)` (401 bodies as above, both kinds);
     `Preparing(hash, n)` (the first `n` requests of a hash get `503` with
     `Retry-After: 0`); `AddHash(hash, overrides map[string][]byte)` (a second
     hash: the recorded files with some paths replaced, the manifest's
     `sha256` recomputed), for re-pinning and incremental-fetch tests.
   - The existing routes keep working.
9. **The recorder records snapshots** (`snapshot` package, `cmd/sc-snapshot`):
   - a `snapshot:` block in the query list: `vendors` (the per-vendor tables to
     record: `region`, `zone`, `server`, `server_price`, `storage_price`,
     `ipv4_price`), `global` (`vendor`) and `benchmarks` (ids; one score file
     per vendor and id);
   - it pins `database_hash` from `/healthcheck`, records the manifest and
     the listed files as the gzip bytes received, plus the headers and
     `/healthcheck`; it checks each file against the manifest's `sha256`, and
     fails when a `SC-Data-Hash` differs from the pin;
   - the key comes from a file (`-api-key-file`) or `KEEPER_API_KEY` at run
     time. A recording never contains an `Authorization` header or the key:
     header allow-list (`SC-Data-*`, `Content-Type`, `Cache-Control`), and a
     test that greps every written file for the key;
   - without a key and a snapshot block it fails at once with
     "the snapshot routes need a Keeper API key", before any request.
10. Commit sc-keeper-go's own recording under `keepertest/testdata` with a
    small snapshot block (hcloud tables, `vendor`, one score file), recorded
    by the coordinator with a key; tests run on it offline.

Gates (in sc-keeper-go): `make lint`, `make test` (no network), the
keepertest cases above, and a run of `cmd/sc-snapshot` against `keepertest`
proving the round trip. The coordinator pushes; step 2 starts from the
pushed commit.

### Step 2 — SCarpenter part A: local selection

Selection profiles today send `/servers` queries
(`selection.PlanQueries` → `ServersAll` in `pkg/catalog/keeper.go`): region
chunks of three, push-down bounds loosened by the churn margin, data not
pinned to the refresh's hash. Part A selects from the catalog data the
refresh already holds. The transport stays as it is until part B.

1. `go get github.com/SpareCores/sc-keeper-go@<step 1 commit>`.
2. `pkg/catalog`: `loadSelections` uses step 1's builder on `model.Data`
   (servers, prices of the scope, scores). Metric scores keep today's source
   until part B.
3. **Score join.** Key scores by `server_id`, and map them to
   `api_reference` through the server rows; they differ at GCP and Hetzner
   (`"100"` and `"ccx53"`).
4. **Equivalence test** on the recordings of `test/data/sc-snapshot/`: the
   local builder and the `/servers` path select the same members for the
   profiles of the tests. Document every difference and why.

Part A changes no instance type labels and no offerings. Selection
membership changes only where step 4 says why.

Gates: CONVENTIONS 1–3, L3 (`make test-kwok`), and the equivalence report.

#### Part A equivalence

Result of `TestLocalSelectionEqualsTheServersPath` and
`TestLocalCandidatesEqualTheServersRows` (`pkg/catalog/equivalence_test.go`) on
`test/data/sc-snapshot/`: 40 selection runs (aws one region, aws three regions,
gcp, hcloud; twelve profiles, four for aws three regions), **36 identical, 4
differ**. No unexplained difference in members, prices or scores.

- **GCP, 4 runs:** six planned-for-retirement types are selected only by
  `/servers`: `n1-megamem-96`, `n1-ultramem-40`, `n1-ultramem-80`,
  `n1-ultramem-160`, `z3-highmem-88` and `z3-highmem-176` (12 member
  entries over the four runs). Cause: the anonymous `/server_prices`
  `only_active` filters on `Server.status` (sc-keeper `api.py`, about line
  1729), so these types have no price rows in the catalog and no offering,
  while `/servers` prices them from `ServerPrice` directly. They cannot be
  launched either way. Part B's snapshot `server_price` table has the rows,
  and the difference then disappears.
- **AWS, no run differs:** nine types (`t2.*`, `t1.micro`, `m1.*`, `c1.medium`,
  `c3.large`) are in `/servers` and absent locally, because the catalog drops
  architectures Kubernetes has no name for. Evaluate drops candidates with an
  empty architecture first, so no selection changes.
- **4th-digit price differences: 0 measured.** The recordings are `/server_prices`
  responses Keeper already converted and rounded, so snapshot-versus-`/servers`
  conversion differences cannot show here. Part B counts them on the snapshot
  recordings.

### Step 3 — SCarpenter part B: snapshots

**A refresh, in order** (`pkg/catalog/keeper.go`, renamed to
`snapshot_source.go` if that reads better):

1. `GET /healthcheck`; pin `database_hash`. An empty hash fails the refresh
   ("Keeper serves no data version"). If the hash and the request (scope,
   metrics, selections) equal the current catalog's, stop.
2. `GET /snapshot/{hash}` (the manifest).
3. `GET /benchmark_configs` (live) when metrics or selections use benchmarks,
   and resolve each to (`benchmark_id`, config text) with the existing
   `BenchmarkCatalog.Resolve`.
4. Fetch, for the pinned hash, every file the request needs that the
   on-disk copy lacks:
   - `vendor` (global, once);
   - for each vendor in scope: `region`, `zone`, `server`, `server_price`,
     `storage_price`, `ipv4_price`;
   - one `benchmark_score/{vendor}/{benchmark_id}` per vendor and benchmark
     that a metric or selection uses.
   Sequential requests; a refresh for all eight vendors and a few benchmarks
   is under 100 credits.
5. **Errors.**
   - `503 snapshot-preparing`: handled by the client (step 1).
   - `404 snapshot-not-available`: re-pin once to the body's `current` and
     restart at step 2. A second one fails the refresh; the previous catalog
     stays until the next refresh.
   - `404 not-found` for a table or vendor: fail the refresh, naming the
     path. A vendor that the manifest does not list is reported per vendor
     ("Keeper has no data for vendor x"), and the other vendors load.
   - `401`: section "The API key".
6. **Local filters.**
   - servers: status `active` or `planned-for-retirement`; architectures
     `x86_64` and `arm64`;
   - prices: unit `hour`, allocation `ondemand` or `spot`, Linux or blank
     operating system, currency `USD`, price above zero, regions in scope;
     a blank zone is copied into every zone the driver reports (none seen
     live; keep the rule);
   - scores: `resource_type` `server`, `config` equal **byte for byte** to the
     resolved config text (decoded as `json.RawMessage`, never re-encoded).
     A metric whose benchmark has no matching rows in the pinned hash
     (including the `200` with empty arrays of an unknown id) is an invalid
     metric in `Data.Invalid`, not an error; the rest of the refresh goes on.
7. Selection profiles run on the same data, with part A's builder.
8. The new data replaces the catalog only when every step succeeded.

**Rounding.** Snapshot prices are unrounded; SCarpenter adds no rounding.
Selection keeps its 4-digit rounding (sc-keeper-go `selection/status.go`).
The old routes rounded converted prices (`/table/server_prices` to 4 places,
`/storage_prices` and `/ipv4_prices` to 6), so offering prices of converted
vendors (hcloud, OVH, UpCloud, ...) move in the last digits: update the
goldens and say so in the report.

**On-disk copy.**

- New flag `--keeper-cache-dir` / `SCARPENTER_KEEPER_CACHE_DIR` (default
  empty: no copy, every new hash is fetched in full). The chart sets it to
  an `emptyDir` mounted at `/var/cache/scarpenter/keeper` (value
  `keeper.cacheDir.sizeLimit`, default `256Mi`).
- Layout: `<dir>/<hash>/manifest.json.gz` and `<dir>/<hash>/<path>.json.gz`,
  the gzip bytes as received; the manifest is written last. Writes go to a
  temporary file and are renamed.
- A file is used only after its uncompressed bytes match the manifest's
  `sha256`; a mismatch deletes it and refetches it once.
- On a new hash, files whose manifest `sha256` is unchanged are copied (or
  hard-linked) from the previous hash's directory; only changed files are
  fetched. After a successful refresh, directories of other hashes are
  deleted.
- A restart within the same hash reads only `/healthcheck` from Keeper.

**Memory.** Decode the files as streams with `columnar.DecodeRows` straight
from the gzip reader; never hold an uncompressed body. Azure's
`server_price` is 31 MB of JSON and 295k rows, AWS's 14 MB and 136k rows.
Report the resident heap for AWS with three regions and for all vendors, and
compare it with plan 06's 217 and 377 MiB. Dropping the price slice after the
offerings are built is out of scope unless the numbers call for it.

**Data age.** `SC-Data-Published-At` of the files (equal to `/healthcheck`'s
`database_published_at` for the pinned hash) is stored in `model.Data` as
`PublishedAt`, exported as `scarpenter_catalog_data_published_timestamp_seconds`
(gauge, Unix seconds) and named with the hash in `CatalogReady`'s message.

**Vendor storage and IPv4 prices.** Today seven drivers fetch
`/storage_prices` anonymously, with their own `net/http` client, every six
hours (`keeperPrices` in `pkg/vendors/{aws,gcp,azure,alicloud,ovh,hcloud,upcloud}/prices.go`;
UpCloud also `/ipv4_prices`). They move to the refresh's pinned data:

- The refresh puts each vendor's `storage_price` and `ipv4_price` rows
  (status `active`, currency `USD`, price above zero) into `model.Data`.
- New optional driver capability in `pkg/vendors/driver.go`:
  `KeeperPriceConsumer { SetKeeperPrices(KeeperPrices) }`, with
  `KeeperPrices{Hash string; Storage []StoragePrice; IPv4 []IPv4Price}` (rows:
  region, storage id, unit, USD price). The registry facts
  (`pkg/vendors/facts`) implement a new optional catalog extension
  `catalog.PriceSink` and forward to the drivers. The catalog calls it after
  a successful load and before the new snapshot is published, so offerings
  of a hash always use that hash's prices; the driver bumps its
  `NodeOptionsEpoch`, as `onLoad` does today.
- Each driver keeps its own conversion and its own `hoursPerMonth`: 730
  everywhere, 672 at UpCloud (its monthly cap). Storage rows are `GB/month`
  (other units ignored, as today). IPv4 rows: `hour` as is, `month` divided
  by the driver's `hoursPerMonth`, other units ignored.
- IPv4 from Keeper replaces the constants `DefaultIPv4PricePerHour` of AWS,
  GCP, Azure and hcloud. The overrides `SCARPENTER_AWS_IPV4_PRICE`,
  `SCARPENTER_GCP_IPV4_PRICE`, `SCARPENTER_AZURE_IPV4_PRICE`,
  `SCARPENTER_HCLOUD_IPV4_PRICE` (and the chart values `vendors.azure.ipv4Price`,
  `vendors.hcloud.ipv4Price`) stay as explicit overrides that win over
  Keeper; unset, a region without an IPv4 row makes the delta incomplete,
  like a missing storage price. Alibaba (IPv4 free or by traffic), OVH
  (included) and Vultr (included) do not read IPv4 prices.
- Delete `keeperPrices`' HTTP code, `kick`, `priceRefresh` and their tests;
  `StaticPrices` stays for tests. Until the first `SetKeeperPrices` the
  prices are unknown (incomplete deltas), as before the first load today.
- **The fake driver** (`pkg/vendors/fake/catalogdata.go`) reads `region`,
  `zone` and `server` of its vendor from the snapshot of the hash pinned by
  `/healthcheck`, through a `*keeper.Client` it gets from
  `vendors.Config.Keeper` (built by the operator from `--keeper-url` and the
  key, replacing `Config.KeeperURL`).

**Vendor billing data** (ADR-024, plan 23). `Vendors()` moves from
`/table/vendor` to `/snapshot/{hash}/vendor`; the four billing columns
(`billing_increment_seconds`, `minimum_billing_seconds`,
`stopped_server_charged`, `billing_comment`) are there. A catalog without the
file (an incomplete recording) has no billing data: retention is off with
`NoKeeperData`, never a guess.

**Options and chart.**

- Remove `--keeper-prices-regions-per-request` and
  `keeper.pricesRegionsPerRequest` (options, deployment template, schema,
  golden files, docs).
- Add `--keeper-cache-dir` and its chart volume (above).
- Keep `--keeper-url`, `--keeper-api-key-file`, `keeper.url`,
  `keeper.apiKey.existingSecret` and `.key`; their help text and
  `values.yaml` comments say the public Keeper needs a key and link the docs
  section.
- `pkg/operator/catalog.go` passes the key file path to the source (read at
  every refresh) instead of the key.

**Docs.**

- A section **"Keeper API key"** in `docs/how-to-guides/install.md` (next to
  "Credentials for the vendors"), linked from `docs/reference/controller.md`'s
  flag and chart rows:
  - the public Keeper (`https://keeper.sparecores.net`) requires a key; without
    one the catalog stays empty (`CatalogReady=False`, `KeeperAPIKeyRequired`);
  - how to request a key: https://docs.sparecores.com/api/#authentication;
  - injecting it: `kubectl create secret generic keeper-api-key
    --from-file=api-key=<file>` and `keeper.apiKey.existingSecret:
    keeper-api-key` (`key` defaults to `api-key`), or
    `--keeper-api-key-file` without the chart; rotation by updating the
    Secret;
  - Keeper is open source (https://github.com/SpareCores/sc-keeper) and can be
    self-hosted: set `keeper.url`; a self-hosted Keeper may need no key.
- `docs/reference/controller.md`: the new flag, condition, reasons and
  metrics; the removed flag; the IPv4 rows; outbound access (Keeper's
  snapshot routes, no `/storage_prices`).
- `docs/reference/labels-and-offerings.md` (`/server_prices`, the IPv4
  constant) and the driver references (`aws-driver.md`, `gcp-driver.md`,
  `azure-driver.md`, `hcloud-driver.md`, `upcloud-driver.md`, `ovh-driver.md`,
  `alicloud-driver.md`): prices from the snapshot.
- `docs/how-to-guides/testing.md`: recordings and the `KEEPER_API_KEY`
  secret.

**Recordings and tests.**

- `test/data/sc-snapshot/queries.yaml` gets the snapshot block: the six
  per-vendor tables for all eight vendors, `vendor`, and the score files of
  the test benchmarks (`geekbench:score`, `stress_ng:bestn`,
  `membench:latency`) for every vendor. Remove the `/table/*`,
  `/server_prices` and `/servers` queries and part A's equivalence test with
  them; keep `/benchmark_configs`.
- `make snapshot` passes the key from `KEEPER_API_KEY` (never written to
  disk in the repository). CI reads the key from AWS SSM, never from a
  GitHub secret holding the key itself:
  - A composite action `.github/actions/keeper-key` assumes the
    environment's existing CI role (`secrets.AWS_ROLE_ARN`, the same role
    the AWS lanes use) through GitHub's OIDC (`id-token: write`), and runs
    `aws ssm get-parameter --with-decryption` in the parameter's own region
    (taken from its ARN). It masks the value (`::add-mask::`) and writes it
    to a `0600` file under `$RUNNER_TEMP`, whose path it outputs; the key
    never goes into `$GITHUB_ENV`. It does not leave AWS credentials in the
    job's environment for non-AWS lanes.
  - The parameter comes from GitHub configuration, never from the
    repository: the repository variable `KEEPER_API_KEY_PARAMETER` (its
    ARN). The CI roles of the `e2e` and `e2e-canary` environments allow
    `ssm:GetParameter` on it.
  - `.github/workflows/refresh-sc-snapshot.yml` runs in the `e2e`
    environment and uses the action before `make snapshot`
    (`-api-key-file`); its comments drop the 1 GB note.
  - When the role or the variable is not set (forks), the action fails
    with a clear message in the refresh workflow, and is skipped in the L5
    lanes, which then run against Keeper without a key.
- L1–L4 replay recordings and need no key: the catalog tests, the kwok
  replay server (`test/kwok/replay/main.go`, `keepertest`) and the vendor
  price tests (now through `SetKeeperPrices`).
- Catalog tests on `keepertest`: a full refresh; same hash, no request but
  `/healthcheck`; a restart on a filled cache dir; a second hash with one
  changed file fetches only that file; a corrupted cache file is refetched;
  `snapshot-not-available` re-pins once, twice keeps the previous catalog;
  `snapshot-preparing` is retried; `not-found`; an unknown benchmark is an
  invalid metric; a config matched byte for byte (a config whose re-encoding
  differs); 401 without and with a key, each exactly one request, the
  previous catalog kept, `KeeperAuthorized=False` and the log line; a key
  file updated between refreshes.
- L5 lanes that install the chart (`test/e2e-*/install.sh`, workflows
  `e2e.yml` and `e2e-cloud.yml`): when a key file is given
  (`KEEPER_API_KEY_FILE`), `install.sh` creates the Secret `keeper-api-key`
  and sets `keeper.apiKey.existingSecret`; the workflows get the file from
  `.github/actions/keeper-key`. `docs/how-to-guides/testing.md` adds the
  `ssm:GetParameter` permission to the CI role's requirements and names
  the variable, without any account id or ARN.

Gates: CONVENTIONS 1–3, L3 (`make test-kwok`), and:

- the memory numbers; the size, request count and time of one refresh for
  AWS and for all vendors (recorded data, and once live by the coordinator);
- new recordings in `test/data/sc-snapshot/` (recorded by the coordinator
  with `make snapshot` and a key; the coder works against `keepertest`
  until then);
- `grep -rn 'storage_prices\|ipv4_prices\|server_prices\|/table/\|ServersAll'
  pkg cmd` finds no SCarpenter caller;
- `grep -rni 'keeper.sparecores.net'` finds only the default, its test,
  docs and recording metadata.

## Billing data Keeper lacks

Raised as [SpareCores/sc-keeper#112](https://github.com/SpareCores/sc-keeper/issues/112);
not part of the steps. The vendor row is one worst-case value per vendor and
cannot express:

- OVH regions (EU, CA and APAC per second with a 60 s minimum, the US
  hourly, per Keeper's comment);
- Alibaba's minimum by vCPU count (600, 300, 120 s);
- UpCloud's stopped-server billing by family (Developer and General Purpose
  billed, Cloud Native not);
- monthly caps (Hetzner, Vultr 672 hours, GPU 730, UpCloud Simple plan 672
  hours);
- what starts the clock (create call, provisioning complete, running).
  Periods always count from the start: no vendor documents clock-hour
  billing.

Proposed shape, a table next to `vendor`, whose row is the default (NULL in
`region_id` or `family` means any; the most specific match wins):

```
billing_rule(vendor_id, region_id NULL, family NULL,
             increment_seconds, minimum_seconds,
             start_event,                 -- create_call | provisioning_complete | running
             monthly_cap_hours NULL,      -- cap over calendar_month
             stopped_server_charged,
             comment)
```

Until it exists, drivers override per region and family in code (plan 23,
`BillingRule`), each override with a plan or ADR reference. `ServerPrice.unit`
is the price's time unit, not a billing granularity, and cannot stand in.

Data to raise with the data owners (checked 2026-11-10 against the vendors'
pages):

- OVH is 3600/3600 in Keeper, but EU, CA and APAC are per second with a
  1-minute minimum; the US is hourly per the US terms, per second per the US
  support guide (open);
- Azure is 1/300 in Keeper, but Azure bills full minutes (increment 60) and
  documents no 300 s minimum;
- Alibaba's `minimum_billing_seconds` (600, the worst of three) is a minimum
  charged on an early release; plan 23 uses it as the boundary of a
  minimum-then-per-second rule, not as a period.

Retention stays off for a vendor until its plan's live check settles the
value, whatever Keeper says.

## Scope

- **sc-keeper-go**: step 1.
- **SCarpenter**: steps 2 and 3: `pkg/catalog`, `pkg/vendors` (the price
  consumers, the fake driver, `Config.Keeper`), `pkg/operator`, the chart,
  the docs, the recordings, the refresh workflow and the L5 lanes' key
  handling.
- **Not here**: sc-keeper itself; sc-karpenter-nodepool (it needs a key for
  snapshots too, in its own repository); a tag of sc-keeper-go.

## Outcome

Landed in three steps: sc-keeper-go's snapshot client and recorder, part A (local
selection) and part B. A refresh now pins the hash from `/healthcheck`, reads the
snapshot files of that hash from an optional on-disk copy or from Keeper, and
selects locally; only `/benchmark_configs` is read live besides. Scores come from
the score files byte for byte, the vendor drivers get storage and IPv4 prices from
the refresh's data (`SetKeeperPrices`), and a 401 keeps the previous catalog and
shows as `KeeperAuthorized=False`.

- **Family and series.** sc-crawler 0.10.4 made Keeper's `family` a broad class
  ("General Purpose") and `series` the hardware line. The catalog's `family` label
  and selection's family filter and ranking use the series, else the family
  (`familyOf`); the labels reference says so.
- **Memory.** L3 S10 at rest: 239 MiB (three AWS regions) and 384 MiB (every AWS
  region; the final L3 run, earlier runs 188-193 and 360-367 MiB) against plan 06's 217 and 377. Building every region of all eight
  vendors (about 520 000 offerings) holds about 1.1 GiB of Go heap, so the chart's
  default limit is 2Gi (request 640Mi); the controller reference has the per-scope
  numbers.
- **Pricing fixes.** GCP rows are keyed by numeric region and storage ids: the
  catalog hands drivers the region name and the storage name, and Facts forwards
  both. Azure's driver overrides Keeper's 300 s minimum with none, so retention
  never holds Azure nodes.
- **CI.** `.github/actions/keeper-key` reads the key from the parameter named by
  the repository variable `KEEPER_API_KEY_PARAMETER` with the environment's role;
  the refresh workflow runs in the `e2e` environment and needs approval there.
- Main's rule R (`uniqueAPIReferences`) runs in the snapshot loader after the
  vendors load and before metrics and selections; the OCI embedded catalog data
  and generator from plan 25 came along unchanged (its source is plan 25's step a2,
  not on main, so nothing of it needed wiring).

Open items:

- sc-keeper-go's recorder writes its files with mode 0600 (kept on hold); only
  `test/kwok/run.sh` makes the recording readable for the replay image meanwhile
  (the Dockerfile has no `--chmod`).
- sc-keeper-go's selection package and sc-karpenter-nodepool still have to adapt
  to the family/series change.
- The offerings dominate memory and could be cut further (for example by not
  holding the price slice after the offerings are built).
