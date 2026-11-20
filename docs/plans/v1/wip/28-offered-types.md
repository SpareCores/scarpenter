# 28 — Offered types: what a NodeClass offers Karpenter, and why a type is not

Depends on: 03 (the catalog), 04 (CloudProvider, the quota tracker, the unavailable cache), 06 (the resource engine's
gates), 14 (contract groundwork), 25 (OCI, the first live case). Read `docs/plans/v1/CONVENTIONS.md` first. Spec: this
plan; overview §4 (instance types and the pre-filter), §5.6 (unavailable cache), §5.9 (quota); `docs/reference/labels-and-offerings.md`
([which servers become instance types](../../../reference/labels-and-offerings.md#which-servers-become-instance-types),
[availability](../../../reference/labels-and-offerings.md#availability)); the [controller reference](../../../reference/controller.md)
(Catalog, Quota, Metrics).

Written 2026-11-20 from the repository at `origin/main`. No cloud API was called. Everything marked **unverified** is
settled by the step that names it.

## Goal

Two consumers, two needs:

1. **Automation** (a benchmark planner that pins `scarpenter.sparecores.com/api-reference=<type>`, a dashboard): a
   cheap, up-front list per NodeClass and vendor of the types SCarpenter hands to Karpenter and, per type, whether each
   offering (zone, capacity type) is available now. One `kubectl get`, no scheduling attempt.
2. **Operators debugging a miss**: for a type that is not offered or only unavailable, **one reason code** that names
   the gate that removed it, with the data the gate used (the quota bucket, the cache entry's class and expiry).

Today neither exists. Karpenter core logs "no instance type met all requirements" without saying which requirement or
availability filter removed what; the upstream per-type gauges (`karpenter_cloudprovider_instance_type_*`) belong to
the AWS provider and are not exported here. SCarpenter itself drops types and marks offerings unavailable in eight
places (section 2) and records them nowhere except a GPU-only gauge (`scarpenter_catalog_dropped_types`) and, per
launch, `scarpenter_launch_skipped_total`.

## 1. Today (as found)

- `catalog.buildVendor` (`pkg/catalog/build.go`) builds the types of a class and vendor once per snapshot, class and
  facts epoch and silently `continue`s past a server for five reasons; only the GPU ones are counted
  (`vendorBuild.dropped`, `build.go:51`).
- `Catalog.apply` (`pkg/catalog/catalog.go:686`) sets `Offering.Available` from three `bool` gates (the unavailable cache,
  the quota `Gate`, the resource engine's `ClassGate`) and a selection-objective rule. The reason is lost in the `||`.
- `Catalog.InstanceTypes` (`catalog.go:638`) caches its result per generation (snapshot, facts epoch, unavailable
  sequence), so the data an explanation needs is computed anyway; the reasons just are not kept.
- `GetInstanceTypes` (`pkg/cloudprovider/cloudprovider.go:209`) additionally removes the types of vendors that are not
  ready. `candidates` (`pkg/cloudprovider/candidates.go:70`) removes more at Create.
- `status.vendors.<v>` (`VendorStatus`, `pkg/apis/v1alpha1/scnodeclass_types.go:149`) carries conditions, images,
  resources and `quota`, which lists only scopes where every offering is blocked (`pkg/controllers/quotastatus`).
- Size: one OCI vendor of one class is 286 types and 3661 offerings; AWS is about 800 types and tens of thousands of
  offerings per class. An object holding every offering does not fit a status field (etcd's object limit is 1.5 MiB and
  a status write rewrites the whole object).

## 2. Reason taxonomy

One closed set of CamelCase codes, shared by every surface (status buckets, listing, log, metric label). The code names
the gate; the detail carries its data. Lines are at `origin/main` of 2026-11-20.

| Code | Level | Where it happens | Detail |
|---|---|---|---|
| `NotInCatalog` | server | Keeper server with a status other than active or retiring is not loaded (`pkg/catalog/keeper.go:523`); sources that cannot decode a server skip it | none (not listed per type) |
| `NoPrice` | type | no active USD price above zero in scope: filtered when the rows are read (`appendPrice`, `keeper.go:932`), then `len(prices) == 0` (`build.go:134`) | none |
| `RegionNotAllowed` | type | every price row is in a region the class does not allow or an inactive one (`build.go:117`; row skipped at `:297`) | regions with a row |
| `ZoneNotAvailable` | offering | a zone-level price row whose zone the driver's account does not have (`resolveZones`, `build.go:361`) or that Keeper marked inactive (`:353`) | zone ID |
| `IncompletePrice` | offering | the default settings' price delta is incomplete and `pricing.allowIncomplete` is false (`build.go:303`) | zone, capacity type |
| `GPU*` | type | `gpuSupport` (`build.go:137`, `:401`): `GPUManufacturer`, `GPUModelUnknown`, `GPUFractional`, `GPUArch`, `GPUNoDriverBranch`, `GPUNVSwitch`, `GPUVirtual` (the existing `nvidia.Reason`s) | the reason |
| `NotLaunchable` | offering | the driver's `OfferingLister` says the type cannot launch in the zone (`build.go:147`, `Tracker.Launchable`, `pkg/quota/tracker.go:487`) | zone ID |
| `NoOffering` | type | every offering of the type was removed above (`build.go:153`); the type's dominant earlier reason is recorded with it | the earlier reason |
| `UnavailableCache` | offering | a live entry of `unavailable.Cache.IsUnavailable` (`pkg/unavailable/unavailable.go:167`), `catalog.go:700` | the entry's class (`InsufficientCapacity` 3 min, `Interrupted` 10 min, `Unsupported` 24 h, `QuotaExceeded` 30 min, `ZoneUnusable` 10 min; `unavailable.go:25`) and its expiry |
| `QuotaGate` | offering | the quota is known and has no headroom (`Tracker.Unavailable`, `tracker.go:576`), `catalog.go:702` | bucket, headroom, need (`Tracker.Blocked`, `tracker.go:605`) |
| `ImageGap` | offering | the class has no ready boot image in the region (`Engine.UnavailableFor`, `pkg/resources/engine.go:681`), `catalog.go:704`; **region-level, not per architecture** (`imageGaps` in `pkg/controllers/nodeclass/controller.go:657` marks a region only when no architecture's image is ready) | region |
| `RegionNotReady` | offering | the class's vendor resources are not ready in the region (`engine.go:689`) | region |
| `ZoneMissingResource` | offering | a required resource is missing in the zone (`engine.go:689`, `setZoneMissing`) | zone, the engine's reason |
| `Unrankable` | offering | the NodePool pins a selection profile and the type has no score (`catalog.go:707`) | profile |
| `VendorNotReady` | type | `GetInstanceTypes` drops the types of vendors without `VendorReady` (`cloudprovider.go:231`, `readyVendors`, `nodeclass.go:91`) | the condition's reason |
| `DriverNotEnabled` | type | no driver for the vendor (`candidates.go:92`; Create only) | vendor |
| `NodePoolRequirement` | type | **Explain only**: the type's requirements do not intersect the NodePool's, with the first key that fails (`Requirements.Intersects`, as `candidates.go:97`) | the label key |
| `NodePoolPreFilter` | type, offering | `Filter.allowsType` (architecture, instance type, `filter.go:67`) and `allowsOffering` (region, zone, capacity type, `filter.go:71`): safe to drop, core would too | the key |
| `ProfileMismatch`, `SettingsFailed`, `IncompleteSettingsPrice` | offering | Create's `settle` (`candidates.go:140`, `:146`, `:151`): a profile no offering supports, unresolvable settings, an incomplete price under the selected profiles | profile, error class |
| `CatalogNotReady` | class | no snapshot, unknown class (`catalog.go:643`), or the class's scope not yet refreshed (`Status`, `catalog.go:429`: `NoSnapshot`, `ScopePending`, `Stale`) | the catalog's reason |

Not knowable by SCarpenter, and said so in the reference: Karpenter-side removals after `GetInstanceTypes` (resource
requests that do not fit the allocatable, `minValues`, NodePool limits, NodeOverlays, reserved capacity). Explain
emulates the two that are pure functions of the type (requirement intersection, resource fit for a given request list),
so a "no instance type met all requirements" can be reproduced offline against a NodePool.

`Unavailable*` stays an **availability** reason, not a removal: the type is listed, its offering has `Available=false`.

## 3. The label automation keys on

- `node.kubernetes.io/instance-type` carries the **instance type name**: `<vendor>.<api_reference>` (ADR-008), with
  characters outside the label charset replaced and a name over 63 characters truncated to 56 plus a hash
  (`catalog/names.go`). It is set on every type, is unique across vendors and is what `Catalog.Lookup` resolves.
- `scarpenter.sparecores.com/api-reference` carries the **raw API reference**, only when it is a valid label value and
  unchanged by sanitising; otherwise the key is `DoesNotExist` on the type (`labels/attributes.go:51`). A NodePool that
  pins it to a value matches nothing for such a type, with no message. It is also not vendor-qualified.
- **Recommendation: key on the instance type name.** The listing prints, per type, `instanceType`, `apiReference`,
  `apiReferenceLabel` (the label's value or `null`) and `pin`: the requirement a NodePool must carry to select exactly
  this type (`node.kubernetes.io/instance-type In [<name>]`, always valid). Automation that already pins `api-reference`
  keeps working where `apiReferenceLabel` is not `null` and adds the vendor label; the listing tells it which types
  those are. OCI sized types have unique, valid references (rule R), so the two coincide there apart from the `oci.`
  prefix.

## 4. Options

**Mechanism common to all options (decided).** One pure function, `Catalog.Explain(ctx, Query) Report`, and the
existing `apply` rewritten so that **availability is computed once, with its reason**: the three gates change from
`bool` to a reason (`Gate.Unavailable(...) (Reason, bool)`; `ClassGate.UnavailableFor` the same; the unavailable cache
gets `Why(Offering) (class, expiry, bool)`), and `InstanceTypes` is `Explain` minus the reasons. Build drops are kept in
`vendorBuild` (generalising `dropped` to a list of `{apiReference, reason, detail}`: a few hundred small structs per
class). One code path means a listing can never disagree with what Karpenter was given.

| | (a) status summary | (b) per-class ConfigMap or CR | (c) Prometheus gauges | (d) log and debug endpoint |
|---|---|---|---|---|
| Serves | humans, `kubectl describe` | automation, debugging | dashboards, alerts | debugging |
| Size | bounded: counts, reasons, at most 8 sample types per reason (about 2 KiB per vendor) | one type per line, offerings compacted: about 60 KiB per vendor for 286 types, 400 KiB for AWS | per-type series: 286 per class and vendor without zones, 3661 with | none stored |
| Cost | one status write on change | one write on change, rate limited | scrape size | none until asked |
| Fresh | within the interval | within the interval | scrape | exact |
| Risk | churn of the status; owner conflicts (own field manager, like quotastatus) | the 1 MiB ConfigMap limit for a very large vendor; a CR adds an API and versioning | cardinality | an open port: reasons only, no driver messages |

Assessment:

- **(a) Yes.** Counts per state (`types`, `offerings`, `availableOfferings`) and per reason (`dropped`,
  `unavailable`), with sample type names per reason. It is what `kubectl describe scnodeclass` shows and what the
  Event for a bad class can point at. Written by a new controller with server-side apply under its own field manager
  (`scarpenter-offered`), only when the content changes, at most every 30 s. The unavailable-cache buckets change as
  entries expire (TTL 3 min), so unavailable counts are written rounded to the reason, never per type.
- **(b) ConfigMap, not a CR.** A read-only CR would need a CRD, a schema, a version and a conversion story for what is
  derived data; a ConfigMap is one object, no API surface and readable with the RBAC the operator already knows. One
  ConfigMap per class and vendor (`scarpenter-offered-<class>-<vendor>`, owned by the SCNodeClass so it is garbage
  collected), one key `types.json`. Compact encoding: per type `{n, ref, label, arch, vcpus, memMiB, o: [{z, c, ok,
  r?}]}`, offerings grouped per capacity type with the zones that share a state (`zones: [...]`, `down: {zone: reason}`)
  so a 12-zone type is two entries, not 24. A hard cap (800 KiB) fails closed: the ConfigMap keeps counts, lists
  the types that are not available first and says `truncated: true`. It is **opt-in per class** (annotation
  `scarpenter.sparecores.com/publish-offered: "true"`, decision 2): not every cluster wants 400 KiB of derived data
  rewritten every minute.
- **(c) A small, fixed set only.** `scarpenter_catalog_offerings_by_state{nodeclass,vendor,state}` (`state`: `available`
  or a reason code; about 20 series per class and vendor) and a drop counter (`scarpenter_catalog_dropped_total{nodeclass,
  vendor,reason}`, incremented when a type enters a reason, not per query). **Not** the per-offering gauge
  `scarpenter_instance_type_offering_available{nodeclass,instance_type,zone,capacity_type}` by default: 3661 series per
  OCI vendor and class, an order of magnitude more for AWS, for data the ConfigMap already holds. It stays a flag
  (`--metrics-offering-detail`, default off) for operators who run Prometheus for it (decision 3). The existing
  `scarpenter_catalog_dropped_types` becomes the `dropped` view of the new gauge with the same reason labels (it is
  kept, not renamed, so dashboards survive).
- **(d) Log on transition; endpoint deferred.** At V(1) one line per type when its reason **changes** (entering or
  leaving a reason, recorded from the previous report per class), never per query: `offered types changed`, with
  `nodeclass`, `vendor`, `type`, `zone`, `capacityType`, `reason`, `detail`. A burst of more than 50 changes in one
  report is one line with the counts and the first 10 types (a refresh that touches 300 types must not flood). A debug
  HTTP endpoint or CLI: not in this plan. The runtime gates (quota, unavailable cache, image gaps) live in the running
  controller, so an offline CLI could explain only the catalog reasons, which the existing catalog harness already
  does; an endpoint needs a port, a Service and an answer to "who may read it" that the ConfigMap's RBAC already gives.

**Recommendation: (a) + (b) as an opt-in ConfigMap + (d) logging on transition + the small (c) set, in that order of
steps;** per-offering gauges and an endpoint only on demand. The common mechanism is step 1, and it is the part that
makes incidents diagnosable by itself, with the log line alone.

## 5. The live incident, and what would have shown it

On OCI, every `VM.Standard4.Ax.Flex.*` type and the `VM.Standard.E3.Flex.114-*` sizes get "no instance type met all
requirements" for the bench class: no NodeClaim, no launch attempt. The Keeper data, a harness class, the quota metrics
and a static read say they are offered.

**Hypothesis (from reading the code, unverified against the live cluster): a 24 hour `Unsupported` entry of the
unavailable cache.** What a harness, Keeper and the quota metrics do not model is the in-memory cache of failed launches.
Create checks a type against the availability domain's shape table before any cloud call
(`pkg/vendors/oci/shapes.go:111`): a shape the AD does not offer is `Unsupported` with `ShapeNotInAvailabilityDomain`
(the crawler's AD table said `Standard4.Ax.Flex` is missing in places, plan 25 §2), and a size beyond one NUMA node is
`Unsupported` with `ExtendedMemoryUnsupported` (the `E3.Flex.114-*` sizes exactly, plan 25 §2). `Create` marks every
`Unsupported` attempt for the (type, zone) with `MarkUnsupported` (`create.go:209`) for **24 hours**
(`unavailable.go:30`). After one broad Create (for example a cheapest-first NodePool that tried those types first, or an
earlier probe) the NodeClaim is deleted and the entries remain, with no log line and no event: later requests that pin
those types see `Available=false` on every offering, and Karpenter reports the generic message. The cache is in memory,
so a controller restart clears it, which is also the quickest check. A second candidate, the quota gate (`Tracker.Unavailable`
on a bucket with zero headroom) is less likely, because the quota metrics show headroom for those buckets.

Step 1's `UnavailableCache` reason with its class (`Unsupported`), expiry and the entry's origin would have shown it on
the first look; the V(1) transition line (or the `Unsupported` count in the status) would have shown it when it began.
Independent of the cause, **marking `Unsupported` for 24 hours on a driver's pre-launch check is itself questionable**:
the check is deterministic per shape table and AD, but an AD table that was read while a region was briefly unreadable
poisons a day. Decision 5.

## 6. Steps (one agent each, each ending green)

1. **Reasons in the catalog (the diagnosis step).** The reason enum (section 2) in `pkg/catalog`; the gates return a
   reason (`Gate`, `ClassGate`, `unavailable.Cache.Why`, `Engine.WhyUnavailableFor`, `Tracker.Blocked` reused); build
   drops kept in `vendorBuild`; `Catalog.Explain` and `InstanceTypes` rewritten over one path, with the existing
   results unchanged (golden: the instance types of the existing catalog test fixtures are byte-identical before and
   after); the V(1) transition log with the burst rule; the ADR (next free number) and the reference docs (reasons,
   the label to key on, the logs). L1: every reason in the table has a test that produces it, `Explain` and
   `InstanceTypes` agree on every offering of a randomised fixture, the log fires once per transition and not per
   query. L2: the cloudprovider's `GetInstanceTypes` with a vendor not ready. No new metric, no new API.
   **With this step an operator who raises the log level, or reads one `Explain` in a test, sees the Standard4.Ax
   reason.**
2. **The NodePool view in Explain.** `NodePoolRequirement` and `NodePoolPreFilter` with the failing key, and the
   resource-fit check for a request list; used by the tests and by step 4. L1 only; catches the "label pinned but the
   type has none" class (section 3) by reproducing core's intersection.
3. **Status summary (a).** The `offered` block of `VendorStatus` (counts, reason buckets, sample names, `observedAt`
   rounded to the minute), the controller `pkg/controllers/offered` (SSA, field manager `scarpenter-offered`, writes on
   change, at most every 30 s, started with the leader), the CRD and `docs/reference/api.md`. L2 envtest: the block
   follows a quota block and an unavailable mark, no write loop (a quiet catalog causes no writes), the block's size
   under 3 KiB for the 286-type fixture. L3 KWOK: the fake driver's capacity error shows as `UnavailableCache`
   `InsufficientCapacity` in the status and clears after the TTL on the fake clock.
4. **The ConfigMap listing (b), opt-in.** The same controller writes the ConfigMap for annotated classes (compact
   encoding, cap, owner reference, deleted when the annotation or the vendor goes); `pin` and `apiReferenceLabel` per type;
   a `jq` recipe per consumer in a how-to ("Which types does a class offer"). RBAC: create, update, delete on
   ConfigMaps in the release namespace only, Helm values and `docs/reference/controller.md` updated. L1 for the encoder
   (round trip, size for the 286-type and a 3000-type fixture, truncation); L2 for ownership and the cap. L3: a scale-up
   test reads the ConfigMap, picks an `available` type from it and pins its `pin` requirement: the NodeClaim launches.
5. **Metrics (c).** `scarpenter_catalog_offerings_by_state`, `scarpenter_catalog_dropped_total`, the flag
   `--metrics-offering-detail` and its per-offering gauge; the metric tables in the controller reference. L1: series
   counts for the fixtures (fail the test if the default set exceeds 30 series per class and vendor).
6. **Live check (optional, needs the maintainer's approval per run, no GPU or other costly type).** On the live OCI
   cluster with the step 1 logs and the status block: read the reason of the Standard4.Ax and E3.Flex.114 types. No
   launch is needed to read it. Record the result in the decision on the `Unsupported` TTL.

Each step updates in the same change: the reference docs of the surfaces it adds (SCNodeClass, controller: flags,
Helm values, metrics, RBAC, Events) and, from step 3, the labels reference where a label or annotation is added.

## 7. Tests

- **L1**: reason producers, `Explain` against `InstanceTypes`, transition log, encoder sizes, metric series counts.
- **L2** (envtest): the status block, the ConfigMap lifecycle, no write loops.
- **L3** (KWOK, fake driver, fake clock): availability reasons through a capacity error and the TTL; a pin taken from
  the listing launches.
- **L4, L5**: none. No live test is part of the gates; step 6 is a read-only look.

## Not here

A debug HTTP endpoint or CLI; a read-only CRD for the listing; explaining Karpenter-side removals beyond the two
pure ones; per-pod "why not scheduled" reports; changing what is offered (this plan only reports it); catalog diffs
between data versions.

## Decisions of the maintainer

Open; each with the recommendation.

1. **Scope of the first merge.** Options: step 1 alone (reasons and logs, no new API surface) now and the rest later;
   or steps 1 to 3 together. Recommendation: step 1 alone first, so the live incident can be diagnosed from logs on
   the next release, then 2 and 3.
2. **ConfigMap: opt-in or always.** Options: always per class and vendor; opt-in by annotation; opt-in by a controller
   flag. Recommendation: opt-in by annotation on the SCNodeClass, so the cost is a choice per class and a class needs
   no spec change (no drift hash change).
3. **Per-offering gauges.** Options: never; a flag, default off; default on with a cap. Recommendation: a flag, default
   off; the reason-count gauge is always on.
4. **Which label automation keys on.** Options: `api-reference` (today's habit, not vendor-qualified, absent for an
   invalid reference); the instance type name. Recommendation: the instance type name, printed with a ready-made `pin`
   in the listing; `api-reference` stays valid where the listing says its label exists.
5. **The 24 hour `Unsupported` TTL for pre-launch checks.** Options: keep (`DefaultUnsupportedTTL`); shorten it to the
   `ZoneUnusable` 10 minutes for entries that come from a driver's own pre-launch check and keep 24 hours for the
   vendor's explicit "not offered" answer; stop marking on a pre-launch check whose verdict is a pure function of the
   shape table (the catalog could drop the type instead). Recommendation: decide after step 6 shows the live cause;
   until then keep it and log every `Unsupported` mark at V(0) with its code, once per (type, zone) per TTL (a
   24 hour silent block is the real fault).
6. **`ImageGap` per architecture.** The gate is region-level, so an Arm type is offered in a region that has only an
   amd64 image and an amd64 type is blocked by an Arm-only gap. Options: leave; make the gate architecture-aware
   (`ClassGate` gets the type's architecture). Recommendation: make it architecture-aware in a follow-up plan; this plan
   reports the reason as it is today and names the limitation in the reference.
7. **Reason code stability.** Options: codes are API (documented, additions only); codes are internal. Recommendation:
   API, since automation will key on them; a new gate adds a code, a code is never renamed.

## Gates

CONVENTIONS gates 1 to 4 for every step; `hack/check-docs.py` clean; the instance types of the existing fixtures
byte-identical after step 1 (golden); the default metric set under 30 series per class and vendor; no driver message,
account identifier or credential in a reason, log line, status block, ConfigMap or metric label (reasons are codes, the
detail is a quota bucket name, a zone, a region or a cache class); the `docs/plans/v1/README.md` status `review`.
Report: which reasons the live cluster showed, for step 6.
