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
driver implements it), `NodeOptions` and `ManagedResources` (below),
`EpochReporter`, `QuotaReporter`, `OfferingLister`, `ImageImporter` (reserved) and
`CatalogProvider` (`CatalogYAML()`: a development driver that brings its own
static catalog instead of Keeper's; plan 07).

## Node options and vendor resources

`NodeOptions` is the capability behind node settings
([node settings](node-settings.md)); a driver fills in its own blocks:

| Method | Meaning |
|---|---|
| `Defaults()` | The built-in settings (layer 1) in the shape of the vendor block (`Raw` JSON, regions and resources left out) and the root disk. |
| `RootDiskBytes(s)` | The root disk of resolved settings: the ephemeral storage the scheduler assumes. |
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
`Kinds`, `Resolve`, `Ensure`, `InUse`, `Delete`, `List`. A resource a launch
uses must show as in use (`InUse`) while its instance lives, so the garbage
collector never deletes it; deleting one that is in use is refused (`Retryable`).

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
For `NodeOptions`: the defaults decode and match `RootDiskBytes`; for the
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
  messages, the documented exception to the rule of the error classes: `cannot
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
- `unsupported`: rules `{kind, profile, instanceTypes, families, zones}` (empty
  lists match everything) that make a profile invalid for the matching types and
  zones. Besides, a storage profile with local storage is valid only on types
  with local disks. A change of the rules rebuilds the catalog.
- `existingResources`: the account's own resources, `{kind, region, id, name,
  tags, default}`: what `use` references and the vendor's default resource
  (`default: true`) resolve to.

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
