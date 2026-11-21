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
| `NotInCatalog` | server | Keeper server with a status other than active or retiring is not loaded (`pkg/catalog/keeper.go:523`), or with a CPU architecture other than `x86_64` or `arm64` (`serverFromTable`, `keeper.go:879`); sources that cannot decode a server skip it | none (not listed per type) |
| `NoPrice` | type | no active USD price above zero in scope: filtered when the rows are read (`appendPrice`, `keeper.go:932`), then `len(prices) == 0` (`build.go:134`) | none |
| `RegionNotAllowed` | type | every price row is in a region the class does not allow or an inactive one (`build.go:117`; row skipped at `:297`) | regions with a row |
| `ZoneNotAvailable` | offering | a zone-level price row whose zone the driver's account does not have (`resolveZones`, `build.go:361`) or that Keeper marked inactive (`:353`); also every row of a region for which the driver lists no zones: the driver facts are not loaded yet (`Facts.Zones` returns an empty map, `pkg/vendors/facts/facts.go:323`) or the credentials cannot read the region (`SkippedRegions`) | zone ID, or the region when it has no zones |
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

**Which codes a surface can carry.** `Unrankable`, `NodePoolRequirement` and `NodePoolPreFilter` depend on the
NodePool (its pinned selection profile, its requirements), and `DriverNotEnabled`, `ProfileMismatch`, `SettingsFailed`
and `IncompleteSettingsPrice` happen only at Create. The class-level surfaces (status, ConfigMap, metrics, the
transition log) evaluate the class with the zero `Filter` and no pin, so they carry only the other codes; the
NodePool-dependent ones exist only in an `Explain` that is given a NodePool (step 2). The offering-level codes a
class-level surface can show are `ZoneNotAvailable`, `IncompletePrice`, `NotLaunchable`, `UnavailableCache`,
`QuotaGate`, `ImageGap`, `RegionNotReady` and `ZoneMissingResource`.

**What Karpenter's message does and does not tell.** Core v1.15.1 filters each NodePool's types before it looks at
a pod (`NewScheduler`, `pkg/controllers/provisioning/scheduling/scheduler.go:181`, with empty resource requests):
a type with no `Available` offering compatible with the NodePool's requirements is gone from the NodePool's
options. A pod that then pins it (for example by `api-reference`) gets "no instance type met all requirements",
the same message as for a type that was never returned or whose labels do not match the pin. The message
therefore does not separate "not offered" from "offered but unavailable"; it does rule out a resource misfit of
the pinned type (that gives "... had the required resources" instead, `nodeclaim.go:588`).

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
- **Recommendation: key on the instance type name.** The listing is a map keyed by it, and each type carries
  `apiReference` only when the label exists. The requirement a NodePool must carry to select exactly this type is
  `node.kubernetes.io/instance-type In [<name>]`, always valid. Automation that already pins `api-reference` keeps working
  where `apiReference` is present and adds the vendor label. OCI sized types have unique, valid references (rule R), so the two coincide there apart from the `oci.`
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
| Size | bounded: counts, reasons, at most 5 sample types per non-zero reason (about 1 KiB per vendor typically; worst case 20 class-level codes x 5 names x 63 bytes, about 7 KiB per vendor) | one type per line, offerings compacted: about 60 KiB per vendor for 286 types, 400 KiB for AWS | per-type series: 286 per class and vendor without zones, 3661 with | none stored |
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
  collected), one key `types.json`: a map of the types by instance type name with `apiReference` (the label's value, omitted when
  there is none), `available`, `capacityTypes`, the dominant `reason` and `detail`, and the offerings grouped by capacity type and
  state with the zones that share it (the binding layout is in `docs/reference/labels-and-offerings.md`, "The offered
  listing"). A hard cap (800 KiB) fails closed: the ConfigMap keeps the types that are not available first and says
  `truncated: true`; it is written on change and at least every 10 minutes (`generatedAt`). It is **opt-in per class** (annotation
  `scarpenter.sparecores.com/publish-offered: "true"`, decision 2): not every cluster wants 400 KiB of derived data
  rewritten every minute.
- **(c) A small, fixed set only.** `scarpenter_catalog_offerings_by_state{nodeclass,vendor,state}` (`state`: `available`
  or one of the eight class-level offering codes of section 2: at most 9 series per class and vendor) and the existing
  `scarpenter_catalog_dropped_types{nodeclass,vendor,reason}` widened from the GPU reasons to every type-level build
  reason (at most 12 more series; the GPU reason values are unchanged, so dashboards survive). No new drop counter:
  a counter "incremented when a type enters a reason" needs the transition tracking of (d) and says nothing the
  gauge and the log do not. **Not** the per-offering gauge
  `scarpenter_instance_type_offering_available{nodeclass,instance_type,zone,capacity_type}` by default: 3661 series per
  OCI vendor and class, an order of magnitude more for AWS, for data the ConfigMap already holds. It stays a flag
  (`--metrics-offering-detail`, default off) for operators who run Prometheus for it (decision 3).
- **(d) Log on transition; endpoint deferred.** At V(1) one line per type when its reason **changes** (entering or
  leaving a reason, recorded from the previous report per class), never per query: `offered types changed`, with
  `nodeclass`, `vendor`, `type`, `zone`, `capacityType`, `reason`, `detail`. A burst of more than 50 changes in one
  report, and the first report of a class after start, are one line **per reason** with the count and the type
  names (at most 50 per line): raising the log level means a restart, after which every
  state is a "change", and a type that never changes again would otherwise never be named. The report is the
  class-level one (zero filter, no pin), computed once per catalog generation (snapshot, facts epoch, unavailable
  sequence) when `InstanceTypes` first sees the new generation, so no new loop or controller is needed. A debug
  HTTP endpoint or CLI: not in this plan. The runtime gates (quota, unavailable cache, image gaps) live in the running
  controller, so an offline CLI could explain only the catalog reasons, which the existing catalog harness already
  does; an endpoint needs a port, a Service and an answer to "who may read it" that the ConfigMap's RBAC already gives.

**Recommendation: (a) + (b) as an opt-in ConfigMap + (d) logging on transition + the small (c) set, in that order of
steps;** per-offering gauges and an endpoint only on demand. The common mechanism is step 1; with the per-reason
lines it names every class-level reason in the log. It does not show NodePool-dependent causes (a pinned selection
profile, labels that do not match a pin, section 2) or a pod-level pin that matches nothing: those need step 2's
`Explain` with a NodePool, or a full listing (b) to check the pin against.

## 5. The live incident, and what would have shown it

On OCI, every `VM.Standard4.Ax.Flex.*` type and the `VM.Standard.E3.Flex.114-*` sizes get "no instance type met all
requirements" for the bench class, which pins `scarpenter.sparecores.com/api-reference`: no NodeClaim was created and
no launch attempt was logged by a controller that had been running for 3 hours. The Keeper data, a harness class, the
quota metrics and a static read say they are offered.

**Ruled out: the unavailable cache.** The cache is in memory and only Create marks `Unsupported`, `QuotaExceeded`,
`InsufficientCapacity` and `ZoneUnusable` (`create.go:196`, `:209`, `:211`, `:293`, `:296`); the only other writer is
the interruption controller, for spot offerings (`interruption/controller.go:343`, `gone.go:67`). Without a Create in
the controller's lifetime there is no entry, so the 24 hour `Unsupported` mark from the OCI driver's pre-launch shape
check cannot explain this incident.

**Ruled out: the capacity type.** Checked live: the bench class's anchor allows both capacity types, and other
on-demand-only types launch from the same class and regions, so a spot or capacity-type mismatch is not the cause.

**The cause is unknown.** That is why step 1 exists: it names every class-level reason in the log, and step 2's
`Explain` shows the NodePool-dependent ones.

**What the message allows** (section 2): the pinned type was not returned, was returned with no available offering
compatible with the NodePool, or does not match the pod's or the NodePool's requirements; not a resource misfit.
The candidates that need no Create, from reading the code (unverified against the live cluster):

1. **A NodePool-dependent requirement on benchmark data.** The family and the largest sizes are plausibly not
   benchmarked. With a NodePool that pins one selection profile whose objective is not price, an unscored type has
   every offering unavailable (`Unrankable`, `catalog.go:707`; the pin is read at `filter.go:57`); with a requirement
   on a metric or selection label, an unscored type has the key `DoesNotExist` (`build.go:250` to `:257`) and fails
   intersection. A harness class without the bench NodePool sees neither. Check: the bench NodePool's requirements and
   whether Keeper has scores for these types.
2. **The quota gate, for the `E3.Flex.114-*` sizes.** A sized type needs its OCPUs and GB (`oci/quota.go:259`), here
   114 OCPUs, against the headroom of the AD with the most room (`bucketOf`, `oci/quota.go:149`). Quota metrics that
   show headroom do not rule this out unless the headroom is at least the need, for the core and the memory bucket.
   It does not explain the small `Standard4.Ax` sizes unless that family's limit is 0.
3. **The region-level class gates** (`ImageGap`, `RegionNotReady`, `ZoneMissingResource`) would block every type of a
   region, not one family; unlikely given that `E5.Flex.2-8` launches in the same regions.

Nothing in SCarpenter caps or truncates the list: `GetInstanceTypes` removes only the types of vendors that are not
ready, offerings are deduplicated per (region, zone ID, capacity type) keeping the cheapest row (`build.go:308`), and
core's `MaxInstanceTypes` (600 in v1.15.1) truncates only the options of a NodeClaim that is being created.

Step 1's per-reason log lines show candidates 2 and 3 (`QuotaGate` with bucket, headroom and need). Candidate 1 is
NodePool-dependent and shows only in step 2's `Explain` with the bench NodePool, or by checking the pin against a
full listing (step 4). Step 6 settles which one it is.

Independent of this incident, **marking `Unsupported` for 24 hours on a driver's pre-launch check is questionable**
(`pkg/vendors/oci/shapes.go:111`: `ShapeNotInAvailabilityDomain`, `ExtendedMemoryUnsupported`; `create.go:209`,
`unavailable.go:30`): the check makes no cloud call and is deterministic per shape table and AD, so retrying it costs
nothing, but an AD table read while a region was briefly unreadable blocks the type for a day, silently. Decision 5 shortens it.

## 6. Steps (one agent each, each ending green)

1. **Reasons in the catalog (the diagnosis step). Done:** the reasons, gates, `Explain`, the V(1) transition log, ADR-029 and the reference are in; the decision 5 TTL change is in too. Deviations: `NotInCatalog` is defined but not produced (sources skip those servers before the catalog sees them); the burst line is chunked at 50 types per line instead of truncated, so every type is named; `ZoneMissingResource` carries the zone only (the engine's reason can hold a driver message); a driver's pre-launch check is marked with a new `vendors.Error.PreLaunch` flag, set by the OCI driver for its shape check. The reason enum (section 2) in `pkg/catalog`; the gates return a
   reason (`Gate.Unavailable` and `ClassGate.UnavailableFor` change their result, no second method;
   `unavailable.Cache.Why`; `Tracker.Blocked` reused for the bucket); build
   drops kept in `vendorBuild`; `Catalog.Explain` and `InstanceTypes` rewritten over one path, with the existing
   results unchanged (golden: the instance types of the existing catalog test fixtures are byte-identical before and
   after); the V(1) transition log with the burst rule; the ADR (next free number) and the reference docs (reasons,
   the label to key on, the logs). L1: every reason in the table has a test that produces it, `Explain` and
   `InstanceTypes` agree on every offering of a randomised fixture, the log fires once per transition and not per
   query. L2: the cloudprovider's `GetInstanceTypes` with a vendor not ready. No new metric, no new API.
   **With this step an operator who raises the log level sees every class-level reason, including a quota or
   class gate on the Standard4.Ax and E3.Flex.114 types; a NodePool-dependent cause (section 5, candidate 1) needs
   step 2.**
2. **The NodePool view in Explain. Done:** `Query` takes the NodePool's `Requirements` and a pod's `Requests`; `Explain` reports `NodePoolPreFilter` and `NodePoolRequirement` with the key, `TypeReport.FitsRequests` is the resource-fit check (a flag, not a reason code, as core's message for it differs), and `GetInstanceTypes` logs `Unrankable` and `NodePoolRequirement` once per generation and NodePool. `NodePoolRequirement` is type-level only; an offering that core's compatibility check rejects is not reported separately. Vendors a NodePool excludes are not listed. `NodePoolRequirement` and `NodePoolPreFilter` with the failing key, and the
   resource-fit check for a request list; used by the tests and by step 4; catches the "label pinned but the type
   has none" class (section 3) by reproducing core's intersection. Live surface: `GetInstanceTypes` (which has the
   NodePool) logs the per-reason lines of step 1 for the NodePool-dependent codes (`Unrankable`,
   `NodePoolRequirement`) once per catalog generation and NodePool, at V(1). L1, plus an L2 case for the log.
3. **Status summary (a). Done:** `status.vendors.<v>.offered`, the `offered-types` controller and `Report.Summaries` are in; the L3 scenario is not (see the plan's final report). Deviations: `dropped` and `unavailable` list type-level and offering-level codes separately, the left-out offerings (`ZoneNotAvailable`, `IncompletePrice`, `NotLaunchable`) count under `unavailable`, `observedAt` is when the summary last changed (a rewrite per minute would be a write loop), and the controller waits for the driver facts. The `offered` block of `VendorStatus` (counts, reason buckets, sample names, `observedAt`
   rounded to the minute), the controller `pkg/controllers/offered` (SSA, field manager `scarpenter-offered`, writes on
   change, at most every 30 s, started with the leader), the CRD and `docs/reference/api.md`. L2 envtest: the block
   follows a quota block and an unavailable mark, no write loop (a quiet catalog causes no writes), the block's size
   under 3 KiB for the 286-type fixture. L3 KWOK: the fake driver's capacity error shows as `UnavailableCache`
   `InsufficientCapacity` in the status and clears after the TTL on the fake clock.
4. **The ConfigMap listing (b), opt-in. Done:** the same controller writes the ConfigMap for annotated classes (layout of the
   coordinator's consumer contract: a map by instance type name, written on change and every 10 minutes, labels
   `managed-by`, `scnodeclass` and `vendor`, owner reference, deleted when the annotation or the vendor goes), the Role in the
   release namespace, the how-to "Which types does a class offer" and the reference. Deviations: the per-type `pin`,
   `arch`, `vcpus` and `memoryMiB` of the first draft are not in the listing (the map key is the instance type name, and
   a consumer builds the requirement); RBAC is list, create, patch, delete (server-side apply needs patch; the start-up
   search needs list); the L3 scale-up scenario is not done. L1 for the encoder (round trip, the 286-type fixture, the cap, the
   dominant reason); L2 for ownership, labels, the gate in the listing, no rewrite when quiet, the heartbeat and the delete.
5. **Metrics (c).** `scarpenter_catalog_offerings_by_state`, `scarpenter_catalog_dropped_types` widened to every build reason, the flag
   `--metrics-offering-detail` and its per-offering gauge; the metric tables in the controller reference. L1: series
   counts for the fixtures (fail the test if the default set exceeds 30 series per class and vendor).
6. **Live check (optional, needs the maintainer's approval per run, no GPU or other costly type).** On the live OCI
   cluster with the step 1 logs and the status block: read the reason of the Standard4.Ax and E3.Flex.114 types, including step 2's
   NodePool lines for the bench NodePool. No launch is needed. Record the cause in the
   plan.

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

Answered by the maintainer on 2026-11-20:

| # | Question | Decision |
|---|---|---|
| 1 | Scope of the first merge | steps 1 and 2 (reasons and logs, then `Explain`) merge first, both without new API surface; step 3 follows |
| 2 | ConfigMap listing | opt-in by the annotation `scarpenter.sparecores.com/publish-offered: "true"` on the SCNodeClass (no spec change, no drift hash change) |
| 3 | Per-offering gauges | behind a flag (`--metrics-offering-detail`), default off; the reason-count gauge is always on |
| 4 | Label automation keys on | the instance type name, the key of the listing's `types` map (no per-type `pin`: the requirement is `node.kubernetes.io/instance-type In [<name>]`); `api-reference` stays valid where the listing says its label exists |
| 5 | 24 hour `Unsupported` TTL for pre-launch checks | shortened now, in step 1: entries from a driver's own pre-launch check use the `ZoneUnusable` 10 minutes; the vendor's explicit "not offered" answer keeps 24 hours; every `Unsupported` mark is logged at V(0) with its code, once per (type, zone) per TTL |
| 6 | `ImageGap` per architecture | a follow-up plan makes the gate architecture-aware (`ClassGate` gets the type's architecture); this plan reports the reason as it is today and names the limitation in the reference |
| 7 | Reason code stability | codes are API: documented, additions only; a new gate adds a code, a code is never renamed |

## Gates

CONVENTIONS gates 1 to 4 for every step; `hack/check-docs.py` clean; the instance types of the existing fixtures
byte-identical after step 1 (golden); the default metric set under 30 series per class and vendor; no driver message,
account identifier or credential in a reason, log line, status block, ConfigMap or metric label (reasons are codes, the
detail is a quota bucket name, a zone, a region or a cache class); the `docs/plans/v1/README.md` status `review`.
Report: which reasons the live cluster showed, for step 6.
