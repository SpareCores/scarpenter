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
| `ResolveImage(ctx, req)` | The image per region, architecture, Flatcar channel and version: the NodeClass's `images` selector when it has one (`req.Use`), else the driver's own convention. See [Images](#images). |
| `CreateInstance(ctx, req)` | Launch one instance. Replaying an `IdempotencyKey` returns the original instance, whatever zone, type or capacity type the replay asks for (the CloudProvider builds the NodeClaim from the returned instance). A failed create leaves the key unused, so the next attempt with another offering launches. |
| `GetInstance`, `FindInstance`, `ListInstances` | By reference, by tags, by tags in regions. `GetInstance` also returns terminated instances; `ListInstances` excludes only the terminated ones (it returns `terminating` instances, see [instance states](#instance-states)). |
| `DeleteInstance(ctx, ref)` | Idempotent: deleting an instance that is gone is not an error. |
| `TagKey(k)` | Maps the logical keys `cluster`, `nodeclaim`, `nodepool`, `nodeclass`, `managed-by` and `idempotency-key` to the vendor's charset (`vendors.DefaultTagKey`: `scarpenter.sparecores.com/<key>`; a vendor whose labels allow no `/` or `.`, or only 32 characters, maps to `scarpenter-<key>`, which is 26 characters for the longest). Keys must stay distinct after the mapping. |

A reference the driver cannot parse (no region, no ID, an ID without the parts the
vendor needs, a region the account cannot use) is an instance that does not exist:
`GetInstance` returns `NotFound` and `DeleteInstance` succeeds (or returns `NotFound`),
never `Fatal`, so that a stale annotation does not put an Event on a NodeClass. The
same holds for the resource calls (`Resolve` of an ID that cannot exist selects nothing:
`NoMatch`, with the ID named).

`InstanceRef` is `<region>/<vendor id>`; the core stores it in the annotation
`scarpenter.sparecores.com/instance-ref`. `CreateRequest.InstanceType` is the
vendor's API reference, not the catalog name. `CreateRequest.Node` describes
the future Node (name, provider ID, labels, resources); real drivers ignore it.

Optional capabilities, found by type assertion: `NodeClassValidator` (the NodeClass
controller calls it for `VendorReady`), `ImageReporter` (`ReportImages(ctx, nc)`
returns the boot images of the NodeClass's release per region and architecture
for `status.vendors.<vendor>.images` and `ImagesReady`; a driver without it
resolves images at launch with `ResolveImage` and counts as ready; the [AWS
driver](aws-driver.md) implements it), `NodeOptions` and `ManagedResources` (below),
`EpochReporter`, `QuotaReporter`, `OfferingLister`, `ZoneChecker` (below), `NodeHardener` ([Boot
fragments](#boot-fragments)), `ImageImporter` (reserved) and
`CatalogProvider` (`CatalogYAML()`: a development driver that brings its own
static catalog instead of Keeper's; plan 07).

## Images

`ResolveImage(ctx, ImageRequest{Region, Arch, Channel, Version, Owner, Use})` returns one `Image`
for the pinned Flatcar release ([ADR-021](../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10)).
`Use` is the NodeClass's `spec.vendors.<vendor>.resources.images.use` for the region and the request's
architecture, with the IDs of the region gathered (`ids` under `*` and under the region) and the
placeholders expanded (`{arch}`, `{vendorArch}`, `{channel}`, `{version}`, `{region}`, `{nodeclass}`);
the core builds it with `selector.ImageUse`, and a driver that lists images for the status
(`ReportImages`) builds the same. It is the [one selector](resource-selectors.md) of every resource
kind, and `images` takes `use` only: no `create` (that would be rung 4 of the ladder), `select: all` is
rejected (a launch needs one image), and the default `select` is `exactlyOne`. The kind is declared in
`ManagedResources.Selectors()` like any other (it is not one of `Kinds()`: nothing is created, listed
for garbage collection or deleted).

- **`Use.IsDefault()`.** The NodeClass gave no selector: a driver of rung 1 or 2 resolves the pinned
  release itself (AWS by name and owner), a driver of rung 3 has a documented naming convention or
  reports the image not found.
- **The request's architecture narrows the selector** (`selector.NarrowArchitecture`): an `ids` list that
  holds both architectures is not ambiguous, the image's own architecture decides. A
  `match.architecture` that excludes it finds nothing.
- **The version check ([ADR-017](../explanation/DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05)).**
  Whatever the lookup, the driver reads the Flatcar channel and version back from the chosen image's own
  labels, tags, properties or name (`Image.Channel`, `Image.Version`) and calls
  `vendors.CheckImageVersion(id, channel, version, req)`: another release is a `Fatal` error with the code
  `ImageVersionMismatch`, an image that says no version `ImageVersionUnknown`, and the image is not
  returned. The channel is compared when the image gives one. An explicitly given image (IDs or name)
  is held to the same rule as a found one: never booted on a guess.
- **Errors.** No match is `NotFound` (`ImageNotFound`), several under `exactlyOne` `Fatal`
  (`ImageAmbiguous`): `selector.ImageError` maps the matcher's typed errors. A launch that cannot resolve
  its image skips the candidate without counting an attempt.
- **`ReportImages` and the status.** `ImageReporter.ReportImages(ctx, nc)` reports each region and
  architecture; for an image refused by the check it sets `ImageStatus.Reason` to the code
  (`ImageVersionMismatch` or `ImageVersionUnknown`) and `Message` names the region and the image. The
  NodeClass controller makes that the reason of `ImagesReady=False` (also for a region the NodeClass does not
  name: the image is what the NodeClass chose, not a gap of the vendor), and the region's offerings are
  unavailable.
- **Drift.** Image IDs are not part of the drift hash; the pinned version is
  ([ADR-012](../explanation/DECISIONS.md)). A rebuilt image of the same version is the same OS.

The conformance suite (`images` subtests, fixture hook `SeedImage`) runs for a driver that declares the
kind: an explicit ID, both architectures in one list, an excluded architecture, a missing ID, another
version, another channel, no version, `exactlyOne`, `newest` and `oldest`, placeholders, and the default
image.

## Boot fragments

A driver may add Butane to the node's configuration through the optional `NodeHardener`
capability (plan 14 A7): the core must not branch on a vendor, and the user-data (a one-hour bootstrap
token and the Tailscale key) is readable from Pods on vendors without something like AWS's metadata hop
limit.

| Method | Meaning |
|---|---|
| `Hardenings()` | The names of the opt-in hardenings the driver offers (`metadata-from-pods`). |
| `BootFragments(req)` | The `vendors.BootFragment{Name, Butane}` list for the node: the fragments that are always on (the vendor's image needs them: the udev rules of `/dev/disk/by-id/google-<name>` on GCP arm64) and those of the hardenings in `req.Hardening`, in a fixed order, depending only on the request (`Region`, `Zone`, `InstanceType`, `Arch`). |

`spec.bootstrap.hardening` lists the hardenings a NodeClass enables; off by default. The core passes a
driver only the names it offers (`vendors.BootFragmentsFor`); a vendor that offers none of them ignores the
list and the NodeClass controller says so in the Event `HardeningNotOffered`. The fragments are rendered like
the user's own Butane and merged after the base and volume configs, before the network template and the
user's Butane, which can override them ([node bootstrap](node-bootstrap.md#vendor-fragments)). They
count in the size budget, and the trial rendering of the NodeClass controller includes them, so a fragment
that does not translate or does not fit makes `ValidationSucceeded` false. `vendors.BlockMetadataFromPods(address)` is the fragment of
`metadata-from-pods` for a vendor's metadata address. The enabled hardenings the vendor's driver
offers are part of that vendor's drift hash. The fake driver offers `metadata-from-pods` so that the hook is tested without a cloud; no
real vendor uses it yet.

## The idempotency label

A vendor with a create token (AWS `ClientToken`, Alibaba Cloud `ClientToken`) or a
deterministic instance name (Azure) keeps the replay rule of `CreateInstance` with that.
A vendor without either stores the NodeClaim UID (`CreateRequest.IdempotencyKey`, 36
lower-case characters, which fits every vendor's value rules) on the instance as the
logical tag `idempotency-key` (`vendors.IdempotencyTag`) and looks it up before every create
(`vendors.FindByIdempotencyKey`): any zone, type or capacity type of the replay returns the
original instance, and a failed create leaves the key unused. The key is read back with
`vendors.IdempotencyKeyOf`. A driver that writes the label sets the fixture's
`StoresIdempotencyKey`, and the suite then requires that the key round-trips through
`GetInstance`, `FindInstance` and the tag filter of `ListInstances`, and that a failed create
leaves no instance with it. The AWS driver does not write it (it has the client token).

## Instance states

`pending`, `running`, `terminating`, `terminated`. An instance that is stopped,
powered off, suspended or deallocated by itself (a guest `poweroff`, a crash; its
disks and addresses, and on most vendors the compute too, keep billing) is
`terminating` on every vendor, as is one the vendor is deleting: each driver's state
table maps stopped, off, powered-off, suspended and deallocated to `terminating`. A node that is off cannot
take pods and does not come back. `Instance.Gone()` is true for `terminating` and
`terminated`, so `Get` and `List` of the CloudProvider skip such instances, and Karpenter
core's NodeClaim garbage collection deletes the NodeClaim of a stopped node.

`ListInstances` returns terminating instances (the garbage collector must find them),
and the garbage collector's orphan sweep and `CloudProvider.Delete` call `DeleteInstance`
again for an instance that has been seen terminating for longer than `--terminating-grace`
(default 10 minutes; the first sighting is in memory, so a restart restarts the clock).
`DeleteInstance` of a terminating instance must therefore work and be idempotent.
Instances the vendor is really deleting disappear inside the grace and cost no extra call.
The rule saves what is charged from now on (the following billing periods, the disks, the
addresses), never the billing period that has started ([ADR-024](../explanation/DECISIONS.md#adr-024-billing-period-aware-node-retention-accepted-2026-11-10)).

## Node options and vendor resources

`NodeOptions` is the capability behind node settings
([node settings](node-settings.md)); a driver fills in its own blocks:

| Method | Meaning |
|---|---|
| `Defaults()` | The built-in settings (layer 1) in the shape of the vendor block (`Raw` JSON, regions and resources left out) and the root disk. |
| `RootDiskBytes(s)` | The root disk of resolved settings: the ephemeral storage the scheduler assumes. |
| `RootDiskForType(s, type)` | The root disk of an instance type whose root disk the type defines (`ok` false when it does not). Asked first by the facts, per type: for `ok` the answer wins, else `RootDiskBytes(s)`, and for 0 the catalog's storage size of the type. Needed where pods that request ephemeral storage would otherwise never fit (OVH `i1` flavors: the root is the 50 GB `ssd` entry, not `storage_size`). |
| `Supported(type, zone, profiles)` | The valid profile names per kind for the type and zone. `profiles` holds the profile blocks of the NodeClass for the vendor (kind, name, the vendor's JSON); a profile without a block for the vendor is not in it. Anything the driver cannot honour is left out (ADR-017). |
| `PriceDelta(settings, offering)` | The price of the settings on top of the instance price per hour, and whether it is complete. |
| `Prepare(ctx, req)` | Makes the pre-instance resources ready and returns them with the device hints. `req` carries the resolved settings, the zone, the type (`CatalogType`: family, vCPUs, local disks), the placement run key and a `ResourceProvider` that gives the vendor resources the NodeClass configures (`Require(kind, key, params)`; see [vendor resources](vendor-resources.md)). |

The settings the core passes (`CreateRequest.Settings`, `PrepareRequest.Settings`)
are `VendorSettings`: `Raw` (the merged vendor block; its `storage` member holds
the selected storage profile), `Network` and `Placement` (the selected profile
blocks), the generic `Networking`, the applied `Profiles` per kind and
`RootDiskBytes`. `Prepared` holds the resource references, a `DeviceHint` per
extra volume (`Name`, `Filesystem`, `MountPath`, and either `ByID` or
`SerialPattern` with `SizeBytes`) and a `LocalHint` for instance-local disks;
the core renders them into the user-data
([first-boot volume unit](node-settings.md#extra-volumes-and-local-storage)).
`Prepare` must be idempotent: a retry with the same request returns the same
resources. A driver whose `Supported` or `PriceDelta` answers can change at
runtime implements `EpochReporter` (`NodeOptionsEpoch()`), so that the catalog
rebuilds.

`ManagedResources` ([vendor resources](vendor-resources.md#driver-contract)):
`Kinds`, `Resolve`, `Selectors`, `Ensure`, `InUse`, `Delete`, `List`. A resource a launch
uses must show as in use (`InUse`) while its instance lives, so the garbage
collector never deletes it; deleting one that is in use is refused (`Retryable`).

### The `use` selector

`Resolve(kind, region, ref)` takes a `vendors.UseRef`: the NodeClass's `use`
([resource selectors](resource-selectors.md)) for one region and zone, with the IDs of the region
gathered (the key `*` and the region's own), every string's placeholders expanded and `select`
defaulted from the declaration. A default `UseRef` (nothing that selects) asks for the vendor's own
default resource, as before. For anything else the driver does the vendor call and then
`selector.Apply` (package `pkg/vendors/selector`); it writes no matching of its own:

- It asks the vendor for what the vendor can filter (the server-side part), and passes a `Fields`
  accessor per item (ID, name, tags, creation time, attributes). `Apply` ANDs every field, re-applies
  each one it has data for (so the vendor may narrow to a superset), then applies `select` and
  `orderBy`: `exactlyOne`, `all`, `newest`, `oldest`, with names and `label:<key>` compared in natural
  order (`4081.3.1` before `4081.10.0`).
- Zero matches return `*selector.NoMatchError`, too many for the `select` (or a tie for `newest` and
  `oldest`) `*selector.AmbiguousError`: both carry the number of candidates after each stage
  (`14 listed, 3 match tags, 0 match match.zone [euw1-az1]`), which is the text of `ResourcesReady`;
  `NoMatchError` also names the requested IDs that were not in the answer and those that did not match.
  A missing ID does not fail the others. The core treats both as the verdict that the region is
  unusable, not as a failure of the vendor.
- `Selectors()` declares, per kind that accepts `use`, a `SelectorCaps`: whether the kind is zonal, global
  at the vendor (the core lists it once per check) and has a creation time, its tag form (`tags`,
  `labels`, plain `strings`, none), where `name` is evaluated and the field it is matched against, each
  supported `match` attribute with the native field and the side (`server`, `server+client`, `client`),
  the accepted `filters` names (exact, or a prefix pattern such as `tag:*`) with their side, whether
  `expression` is accepted and in which language, and the default `select`. The core rejects everything
  outside it when it validates a NodeClass (`ValidationSucceeded` stays true; the vendor's `VendorReady`
  says `ValidationFailed` with the vendor, kind, field and the supported names), and the AWS driver does the
  same in `ValidateNodeClass`. A driver whose vendor spells architectures its own way implements `ArchNamer`
  (`VendorArch`) for `{vendorArch}`.

## Error classes

Drivers return `*vendors.Error` (`vendors.NewError(class, code, err)`);
`vendors.ClassOf(err)` and `vendors.IsClass(err, class)` read it, and an error
without a class is `Fatal`. Classification uses vendor error codes, not message
text, with one documented exception.

**Message-based classification.** A vendor whose API has no error codes (a body of
free text and an HTTP status, or an OpenStack fault with only the status) may be
classified by HTTP status plus message text: today the `qemu` driver, and the vendors
whose APIs are like that. The conditions: the driver documents each text it matches
(in its reference page), a table-driven test pins every text, the HTTP status is captured
per request (a text alone never decides), and a text that matches nothing is `Fatal`,
never `InsufficientCapacity` (a quota limit must never be reported as capacity,
ADR-013). The `qemu` driver predates the rule and keeps its own documented fallback
(an unmatched failure to start the VM is `Retryable`, [below](#the-qemu-driver)). The
conformance suite has no extra case for it; the per-driver tests are the pin.

| Class | Core behaviour |
|---|---|
| `InsufficientCapacity` | Offering unavailable for the TTL (3 min), next candidate. |
| `Unsupported` | Type unavailable in the zone (24 h), next candidate. |
| `QuotaExceeded` | Quota refresh of the region (vendor with quota knowledge for the type) or region and capacity type unavailable (30 min); next candidate in another region or capacity type. A quota limit must never be reported as `InsufficientCapacity`. A driver of a vendor whose limits are account-wide (project or account limits, not per region) sets `Error.AccountWide` (`err.AsAccountWide()`): the capacity type is then unavailable in every region of the vendor for the same 30 minutes, so that each region does not fail once before all are gated. A vendor that reports account-wide buckets in `GlobalQuotas` is gated by the quota tracker; the flag covers the reactive path, where the vendor reports no usage. |
| `ZoneUnusable` | Zone unavailable (10 min). |
| `RegionUnavailable` | Only from `Zones`: the credentials cannot read the region. The facts skip the region (one warning) and load the others; if every region is unavailable the load fails. |
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
idempotent create and the two key rules, NotFound semantics, malformed references
(`NotFound`, never `Fatal`), the idempotency label (drivers that store it), list by tags, the stopped-node rule (the fixture's `PowerOff`
hook stops an instance; it must report `terminating`, stay listed and delete cleanly), delete idempotency, tag
round trip, user-data limit, error classification (the fixture's `Inject` hook
makes the next launch fail with a class), and for `QuotaReporter` drivers: every
type maps to buckets or is on the fixture's unknown list, launches show up in
`Used` with their refs, a quota error is `QuotaExceeded` (with `AccountWide` exactly when the fixture's
`AccountWideQuota` says the vendor's limits are account-wide); and `OfferingLister`.
For `ManagedResources` with a `Seed` hook (a resource made the way a user would, with a name, tags and
the attributes the vendor has) the selector cases: every declaration is well formed and validation rejects
what it does not support (unknown filter, attribute, placeholder, `{zone}` on a regional kind, `created` on
a kind without a creation time, an empty `use`); fields are ANDed; `ids` are ANDed with the rest, a
missing ID does not fail the others and is named, an ID that does not match is named; `*` under `ids`
is every region; zero matches and two matches under `exactlyOne` are `NoMatch` and `Ambiguous` with the
counts; `newest`, `oldest` and a tie (by name in natural order, by label, by creation time); `{zone}` per
zone; glob escapes; tags `"*"`; the CIDR attributes; and, for the fake driver, the same assertions with the
vendor evaluating the selector exactly, narrowing to a superset, or not at all
(`conformance.RunSelectors`).
For a driver that declares the `images` kind with a `SeedImage` hook the [image cases](#images) (explicit IDs, both
architectures, the version check, ordering, placeholders, the default image).
For `NodeOptions`: the defaults decode and match `RootDiskBytes`, and `RootDiskForType` is deterministic and positive when it answers; for the
fixture's sample `Profiles` and `ProfileTypes`, `Supported` is deterministic,
returns only defined profiles and nothing without profiles, and every supported
profile resolves, has a finite non-negative price delta and prepares
idempotently (the same resources and device hints the second time). For
`ManagedResources` (fixture kind and hooks): kinds are well formed; `Ensure` is
idempotent and tags round-trip; `List` filters by region and marker; resources
the user brought (`PreCreate`) are found by ID and tags, are never managed, never
listed as managed and refuse `Delete`; managed resources are found again after a
restart (`Restart`: adoption); an instance launched with a prepared resource
makes it in use, `Delete` is refused while it lives and works (twice) after it is
gone. Every driver runs it against its in-process fake.

Fixture details a driver may need: an `UnknownQuotaTypes` entry is a type
(unknown for both capacity types) or `type/capacity-type` (unknown for that one
only: AWS types that exist on-demand only have no spot quota); `ResourceParams`
returns the create parameters of the fixture's resource kind (the AWS security
groups need their VPC). The AWS driver runs the suite once per resource kind over
the complete AWS type list of the committed snapshot
(`pkg/vendors/aws/conformance_test.go`).

## Zone checks

`ZoneChecker` (`ZoneGaps(ctx, region, settings, found)`) lets a driver say, before any
launch, which zones of a region cannot host a node with the NodeClass's resolved
default settings, given the resources the resource check found in the region (by kind).
The AWS driver uses it for `publicIPv6` on subnets without an IPv6 CIDR block. The
result maps zone names to a reason: the resource check makes those zones' offerings
unavailable and shows the reason in the resource status; a region where every zone is a
gap is not ready. An error is no verdict (the zones keep their last state). The
conformance suite calls it with nothing found (no gaps, no error) and with the vendor's
default resources, and requires deterministic answers that name zones of the region with a
reason.

## The qemu driver

The `qemu` driver (build tag `scarpenter_dev`; register name and vendor ID
`qemu`) boots real Flatcar VMs on the machine SCarpenter runs on; it is the
subject of the L4 lane ([testing](../how-to-guides/testing.md#the-l4-lane)). It
brings its own catalog (the optional `CatalogProvider` capability: `CatalogYAML()`
returns the document of `pkg/catalog/static`; the operator serves that vendor from
it and every other vendor from Keeper, so a lane without a Keeper runs): region
`local`, zone `local-a`, types `qemu.small` (2 vCPUs, 2 GiB), `qemu.medium` (2 vCPUs,
4 GiB) and `qemu.local` (2 vCPUs, 4 GiB, two local disks of 4 GB), on-demand only.

- **Images.** `ResolveImage` downloads `flatcar_production_qemu_uefi_image.img.bz2`
  (a compressed qcow2) and the UEFI firmware files of the release from the Flatcar
  release base (`--flatcar-base-url`), verifies each against the SHA512 of the
  release's `.DIGESTS` file (a file already in the cache is verified, not fetched
  again), decompresses the image and caches the release in `--qemu-cache-dir`.
  `Image.Ref` is the release's directory in the cache; `RootDiskBytes` the image's
  virtual size.
- **Instances.** `CreateInstance` writes a record in `--qemu-state-dir` first, then
  makes a qcow2 overlay of the image grown to the root disk size (`format: raw`
  makes a full copy), extra disks, a copy of the firmware variables, the Ignition
  file (user-data limit 16 KiB, like AWS), a tap device on the bridge, and starts
  `qemu-system-<arch>` as a daemon (`-daemonize`; KVM when `/dev/kvm` is usable, TCG
  otherwise, or as `accelerator` says) with the user-data in
  `fw_cfg name=opt/org.flatcar-linux/config` and the serial port in a file. The ID is
  derived from the idempotency key; a replayed key returns the running VM, a failed
  launch removes everything it made. The VM processes are independent of the
  controller: after a restart `Init` finds them again by their command line
  (`-name <id>`), and removes what is left of launches that never got a process and
  of VMs whose process is gone (a VM that powers itself off is reported once as
  terminated). Addresses come from the ARP table of the bridge, by the VM's MAC.
  `DeleteInstance` ends the process (SIGTERM, SIGKILL after 15 s), deletes the tap
  and the directory and moves the serial log to `--qemu-log-dir`.
- **Errors.** QEMU has no error codes, so launch failures are classified by its
  messages, the documented [exception](#error-classes) to the rule of the error classes: `cannot
  allocate memory` is `InsufficientCapacity`, a KVM that cannot be used is
  `Unsupported`, `no space left on device` while making disks is `QuotaExceeded`, a
  missing bridge is `ZoneUnusable`, other disk failures are `Fatal` and any other
  failure to start the VM is `Retryable`.
- **Node options.** Settings: `accelerator` and `storage` (`root.size` default 20 GiB,
  `root.format`, `data[]` with name, size, filesystem and mount path, `localStorage`),
  network profiles (`model`: the NIC) and placement profiles (`pinCPUs`: the VM runs
  under `taskset` on consecutive host CPUs). Extra disks are virtio disks with a
  serial. A data disk whose size no other data disk of the node has is announced
  to the guest by its by-id link (`/dev/disk/by-id/virtio-<serial>`), disks of one
  size by the serial pattern `scv*` and the size, local disks by the pattern
  `virtio-scl*` and their count: the two ways of the first-boot unit both run on every
  lane. A storage profile with local storage is supported only on `qemu.local`; a
  profile that pins CPUs only when the host has enough. Settings are free of charge.
- **Networks.** The `networks` kind ([vendor resources](vendor-resources.md#the-qemu-drivers-networks)).

## The fake driver

The `fake` driver (build tag `scarpenter_dev`; register name `fake`) keeps
instances in memory and creates a Node object for KWOK for each. It serves the
vendor ID of `--fake-catalog-vendor` (default `aws`) with that vendor's zones and
servers from Keeper, so NodeClasses use `spec.vendors.aws` and types are
`aws.<api_reference>`; run it with `--vendors=fake`. Flags (all
`SCARPENTER_FAKE_*`, also listed in [the controller reference](controller.md#development-builds)): `--fake-catalog-vendor`, `--fake-config-namespace`
(`scarpenter-system`), `--fake-config-name` (`scarpenter-fake`),
`--fake-state-name` (`scarpenter-fake-state`), `--fake-resources-name`
(`scarpenter-fake-resources`), `--fake-launch-delay`.

The fake's Go API has `PowerOff(ref)`, which stops an instance the way a guest
poweroff would (it stays, reported `terminating`, until deleted); the conformance suite
and the unit tests use it.

The ConfigMap `scarpenter-fake` steers it (every key optional, YAML):

- `errors`: rules `{match: {instanceType, region, zone, capacityType}, error:
  <class>, accountWide: true, count: N}` (`accountWide` marks a `QuotaExceeded` as
  account-wide; a quota error of a `global` bucket is account-wide too); the first match wins, `count` N fails the next N calls
  (changing the text resets the counts).
- `quota`: `buckets` (`id`, `scope`, `unit`, `limit`, `foreignUsed`, per-region
  `regions` overrides), `consumption` (first match by `families` and
  `capacityTypes` -> `buckets`; amount = the type's vCPUs), `notLaunchable`
  (`region`, `zone`, `types`). Usage = foreign usage plus the fake's own
  instances. Without the key the driver has no quota knowledge.
- `launchDelay`, `orphans` (instances without a NodeClaim, for the GC scenarios; `poweredOff: true`
  creates one stopped, reported `terminating`).
- `powerOff`: a list of NodeClaim names; the running instance of each is powered off
  once (reported `terminating`, as `PowerOff` does) and its Node is deleted, the way a
  stopped node stops reporting (a KWOK Node would otherwise stay Ready).
- `unsupported`: rules `{kind, profile, instanceTypes, families, zones}` (empty
  lists match everything) that make a profile invalid for the matching types and
  zones. Besides, a storage profile with local storage is valid only on types
  with local disks. A change of the rules rebuilds the catalog.
- `rootDisks`: rules `{instanceTypes, families, size}` (empty lists match everything; the
  first match wins) that make matching types define their own root disk, answered by
  `RootDiskForType`.
- `images`: the account's boot images, `{id, region (or "*": every region), name, arch (amd64 or arm64),
  channel, version, owner, deprecated, tags, created, description, state}`: what the `images` selector and the
  version check resolve against. An image says its release in `channel` and `version` or in the
  tags `flatcar-channel` and `flatcar-version`; without either it says none (`ImageVersionUnknown`).
  Without an `images` selector in the NodeClass every release has a synthesised image. The fake
  also implements `ImageReporter` and `NodeHardener`.
- `existingResources`: the account's own resources, `{kind, region, id, name,
  tags, default, created, description, zone, network, cidr, state}`: what `use` selectors and the
  vendor's default resource (`default: true`) resolve to; the attributes feed `match` (state
  defaults to `available`). The fake declares every `match` attribute, the filters `state`, `zone`
  and `tag:<key>`, and a creation time on `subnets` and `placementGroups`; its Go API has
  `SetSelectorMode` (the fake evaluates the selector exactly, narrows to a superset, or lists everything), which
  the conformance cases use.

**Settings and resources.** The fake serves the blocks of the vendor it
impersonates: with the default `aws` its NodeClass blocks, profiles and resource
keys are AWS's (`storage.root.volumeSize`, `storage.data`, `placementGroup`, ...),
which it reads through its own tolerant structs (it also understands the shape
of the `fake` API package: `size`, `group`). Defaults: a 64 GiB root disk. Price
delta: 0.10 USD per GiB-month for volumes (and for a root disk above the default),
none for the defaults; provisioned `iops` or `throughput` and `tenancy:
dedicated` are not priced, which makes the price incomplete. Resource kinds,
named like the AWS block's resource keys: `subnets` (the regional network; shared,
`idleTTL`), `placementGroups` (per run key, `perUse`; a placement profile with a
group needs a run key; `use` references an existing group) and `volumes` (per
node and volume, `perUse`, from `storage.data`; the device hint is
`/dev/disk/by-id/fake-<id>`); `localStorage` yields a hint for `fake-local-*`. The
fake refuses to delete a resource that an instance uses (`Retryable`) and one it
did not create. Managed resources persist in the ConfigMap
`scarpenter-fake-resources`, so they survive a restart of the controller like the
resources of a real account (adoption); instances do not.

The fake re-reads its ConfigMap on every `CreateInstance`, so a rule written just
before a launch is in effect.

**State.** It publishes `calls`, `instances` and `resources` in the ConfigMap
`scarpenter-fake-state` (an instance records the size of the user-data it was
launched with, `userDataBytes`, the resolved `settings`, the applied `profiles`,
its `resources` and `devices`; calls on resources are `resource-ensure`
(`created` or `found`) and `resource-delete`, with `resource` and `kind`; the
fake boots nothing). Writes are debounced and retried with backoff until they
succeed. The ConfigMap carries a `revision`; the controller exposes the fake's
current and published revision as the gauges `scarpenter_fake_state_revision` and
`scarpenter_fake_state_published_revision`, so a test reads the current revision
and waits until the ConfigMap has reached it instead of sleeping. Revisions count
up from the start time of the process, so a restarted controller never publishes
a lower one. Development builds run the real NodeClass controller (plan 05 removed
the stand-in of plan 04). See [Test and develop](../how-to-guides/testing.md#the-l3-lane).
