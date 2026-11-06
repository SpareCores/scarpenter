# Driver contract

Every vendor is a Go package under `pkg/vendors/<vendor>` that implements
`vendors.Driver`, optional capability interfaces, and passes the conformance
suite (overview section 7, ADR-004). The core imports no vendor SDK and
branches on no vendor name.

## The interface

| Method | Meaning |
|---|---|
| `Vendor()` | The Keeper vendor ID (`aws`). |
| `Init(ctx, cfg)` | Prepare; `cfg` has the cluster name, a Kubernetes client (for drivers that need one), a clock, a logger and the Keeper URL. Must not fail on transient network errors. |
| `Regions(ctx)` | Regions the credentials can use (Keeper region `api_reference`). |
| `Zones(ctx, region)` | The account's zones with Keeper zone IDs (`use1-az1`) and the account's names (`us-east-1a`). A vendor without zones returns none. |
| `ResolveImage(ctx, req)` | The official image per region, architecture, Flatcar channel and version. |
| `CreateInstance(ctx, req)` | Launch one instance. Replaying an `IdempotencyKey` returns the original instance, whatever zone, type or capacity type the replay asks for (the CloudProvider builds the NodeClaim from the returned instance). A failed create leaves the key unused, so the next attempt with another offering launches. |
| `GetInstance`, `FindInstance`, `ListInstances` | By reference, by tags, by tags in regions. `GetInstance` also returns terminated instances; `ListInstances` excludes them. |
| `DeleteInstance(ctx, ref)` | Idempotent: deleting an instance that is gone is not an error. |
| `TagKey(k)` | Maps the logical keys `cluster`, `nodeclaim`, `nodepool`, `nodeclass`, `managed-by` to the vendor's charset (`vendors.DefaultTagKey`: `scarpenter.sparecores.com/<key>`). |

`InstanceRef` is `<region>/<vendor id>`; the core stores it in the annotation
`scarpenter.sparecores.com/instance-ref`. `CreateRequest.InstanceType` is the
vendor's API reference, not the catalog name. `CreateRequest.Node` describes
the future Node (name, provider ID, labels, resources); real drivers ignore it.

Optional capabilities, found by type assertion: `NodeClassValidator` (the NodeClass
controller calls it for `VendorReady`), `ImageReporter` (`ReportImages(ctx, nc)`
returns the boot images of the NodeClass's release per region and architecture
for `status.vendors.<vendor>.images` and `ImagesReady`; a driver without it
resolves images at launch with `ResolveImage` and counts as ready; plan 08's AWS
driver implements it), `NodeOptions` (defaults, supported profiles, price delta, `Prepare`; plan 06
fills them in), `ManagedResources` (plan 06), `QuotaReporter`, `OfferingLister`,
`ImageImporter` (reserved).

## Error classes

Drivers return `*vendors.Error` (`vendors.NewError(class, code, err)`);
`vendors.ClassOf(err)` and `vendors.IsClass(err, class)` read it, and an error
without a class is `Fatal`. Classification uses vendor error codes, not message
text.

| Class | Core behaviour |
|---|---|
| `InsufficientCapacity` | Offering unavailable for the TTL (3 min), next candidate. |
| `Unsupported` | Type unavailable in the zone (24 h), next candidate. |
| `QuotaExceeded` | Quota refresh of the region (vendor with quota knowledge for the type) or region and capacity type unavailable (30 min); next candidate in another region or capacity type. A quota limit must never be reported as `InsufficientCapacity`. |
| `ZoneUnusable` | Zone unavailable (10 min). |
| `Retryable` | Returned to core, which retries the NodeClaim. |
| `NotFound` | The instance does not exist. |
| `Fatal` | `CreateError` and an Event on the SCNodeClass. |

## Quotas

`QuotaReporter.Quotas(ctx, region)` returns the buckets `{ID, Scope, Unit, Limit,
Used}` of a region and the references of the instances counted in `Used`;
`GlobalQuotas` the account-wide ones; `Consumption(type, capacityType)` the
buckets and amounts one instance consumes (`false`: unknown, reactive path).
The tracker passes `vendors.WithQuotaRefresh(ctx, ...)`: a driver may reuse
cached limits unless `QuotaRefreshFrom(ctx).Limits` is set. `Used` counts the
whole account. `OfferingLister.LaunchableTypes` maps API references to Keeper
zone IDs. See [the controller reference](controller.md#quota).

## Registry

Drivers register in `init()` with `vendors.Register(name, factory)`.
`--vendors` lists the enabled names; the registry keys the initialised drivers by
`Vendor()`. A driver may serve another vendor's ID than its name (the fake
driver does).

## Conformance suite

`conformance.Run(t, conformance.Fixture{...})` runs: identity, zone mapping,
idempotent create and the two key rules, NotFound semantics, list by tags, delete idempotency, tag
round trip, user-data limit, error classification (the fixture's `Inject` hook
makes the next launch fail with a class), and for `QuotaReporter` drivers: every
type maps to buckets or is on the fixture's unknown list, launches show up in
`Used` with their refs, a quota error is `QuotaExceeded`; and `OfferingLister`.
Every driver runs it against its in-process fake.

## The fake driver

The `fake` driver (build tag `scarpenter_dev`; register name `fake`) keeps
instances in memory and creates a Node object for KWOK for each. It serves the
vendor ID of `--fake-catalog-vendor` (default `aws`) with that vendor's zones and
servers from Keeper, so NodeClasses use `spec.vendors.aws` and types are
`aws.<api_reference>`; run it with `--vendors=fake`. Flags (all
`SCARPENTER_FAKE_*`, also listed in [the controller reference](controller.md#development-builds)): `--fake-catalog-vendor`, `--fake-config-namespace`
(`scarpenter-system`), `--fake-config-name` (`scarpenter-fake`),
`--fake-state-name` (`scarpenter-fake-state`), `--fake-launch-delay`.

The ConfigMap `scarpenter-fake` steers it (every key optional, YAML):

- `errors`: rules `{match: {instanceType, region, zone, capacityType}, error:
  <class>, count: N}`; the first match wins, `count` N fails the next N calls
  (changing the text resets the counts).
- `quota`: `buckets` (`id`, `scope`, `unit`, `limit`, `foreignUsed`, per-region
  `regions` overrides), `consumption` (first match by `families` and
  `capacityTypes` -> `buckets`; amount = the type's vCPUs), `notLaunchable`
  (`region`, `zone`, `types`). Usage = foreign usage plus the fake's own
  instances. Without the key the driver has no quota knowledge.
- `launchDelay`, `orphans` (instances without a NodeClaim, for the GC scenario).

Decisions for later plans: with `--fake-catalog-vendor=aws` the fake serves
the vendor ID `aws` (types `aws.*`, NodeClass block `spec.vendors.aws`), which
is what the L3 lane of plan 04 uses. `spec.vendors.fake` and the fake profile
and resource blocks stay in the API for plan 06, which decides how the fake
gets its own vendor ID (for example `fake` with a catalog alias) so those
blocks become meaningful. The fake re-reads its ConfigMap on every
`CreateInstance`, so a rule written just before a launch is in effect.

It publishes `calls` and `instances` in the ConfigMap `scarpenter-fake-state`
(an instance records the size of the user-data it was launched with,
`userDataBytes`; the fake boots nothing). Development builds run the real NodeClass
controller (plan 05 removed the stand-in of plan 04). See
[Test and develop](../how-to-guides/testing.md#the-l3-lane).
