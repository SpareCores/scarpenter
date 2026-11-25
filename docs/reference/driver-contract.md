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

`Instance.BillingStart` is the vendor's own creation or billing-start stamp, which the billing periods of
the instance count from ([billing](#billing)); `CreatedAt` is not always that. Zero when the vendor has none and the driver
could not record one.

`Instance.PublicIPv4` and `PublicIPv6` are the instance's public addresses, set with `Instance.SetPublic` from the
fields the vendor documents as public (never guessed from `Addresses`), empty when there is none or it is not assigned yet.
The garbage collector publishes them as Node and NodeClaim annotations ([public addresses](controller.md#public-addresses)).
A driver that did not read them (a failed read, or a listing that leaves them out, as OCI's) sets `Instance.PublicUnknown`, and
`GetInstance` then returns them.

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
`TypeChecker` (an extension of `NodeOptions`: whether the driver launches a type at all; the catalog drops the
others as `NotLaunchable`), `EpochReporter`, `QuotaReporter`, `OfferingLister`, `ZoneChecker`, `BillingDescriber` (below), `InstanceMetadata` ([Metadata
endpoints](#metadata-endpoints)), `LeftoverCollector` (the garbage collector asks it, per region after the
instance sweep, to delete what a failed or interrupted launch made besides the instance; it lists first and
asks `nodeClaimExists` after, so a launch in flight keeps its resources; see
[garbage collection](controller.md#garbage-collection)), `NodeClassWarner` (settings that are valid but
have no effect for the vendor, from the NodeClass alone: the vendor condition `ConfigurationWarning`),
`ImageImporter` (reserved), `ImageCopier` (plan 29, ADR-032: a vendor with regional images and a copy API; the copy manager of `pkg/imagecopy` owns the policy and the driver maps `Architectures`, `Primary`, `CopySources`, `ListCopies`, `StartCopy`, `CopyInUse`, `TouchCopy` and `DeleteCopy` onto the vendor's API; `StartCopy` answers a limit or throttling error with `QuotaExceeded` or `Retryable` so that the copy waits with a backoff, `DeleteCopy` refuses an image without the cluster's ownership tags as `Fatal`; the conformance suite's `image copies` cases run for it), `GPUVirtualizer` (`Virtual(apiReference) bool`: the type's GPU is a
virtual one that needs a GRID driver, so the catalog drops it with the reason `GPUVirtual`; see
[GPU types](labels-and-offerings.md#gpu-types)) and
`CatalogProvider` (`CatalogYAML()`: a development driver that brings its own
static catalog instead of Keeper's; plan 07).

## Reaching the bootstrap artifacts

A node fetches its sysexts from `github.com` and `objects.githubusercontent.com`, its container images from
`ghcr.io`, and (with the qemu driver and the controller) Flatcar release files from
`stable.release.flatcar-linux.net`. None of these hosts has an AAAA record. A driver must not offer a type, or
accept a NodeClass configuration, that leaves the node without a path to them: that is a node without IPv4 egress
while no mirror (`--sysext-base-url`, `--flatcar-base-url`, `--nvidia-driver-base-url`) is configured.

- **A type** that has no IPv4 is `NotLaunchable` in the offered-types listing (the Vultr `-v6` plans).
- **A configuration** is refused with `VendorReady=False`, reason `ValidationFailed`. The NodeClass controller refuses
  `publicIPv4: false` with `publicIPv6: true` for every vendor unless `--sysext-base-url` is set (it covers the
  sysexts only) or the NodeClass brings its own subnets or networks (`resources.subnets.use`,
  `resources.networks.use`), whose IPv4 egress is the user's job: that case gets the Warning Event `IPv6OnlyEgress`.
  The driver refuses `publicIPv4: false` where nothing gives the nodes IPv4 egress.
- **With a mirror** reachable over IPv6 both are fine; the mirror is the operator's job.

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
  unavailable. A driver may set a reason of its own for another definitive cause (Azure's
  `MarketplaceTermsNotAccepted`): it becomes the reason of `ImagesReady=False` when the images are not
  ready (a region the NodeClass names, or none at all), with that image's message.
- **Drift.** Image IDs are not part of the drift hash; the pinned version is
  ([ADR-012](../explanation/DECISIONS.md)). A rebuilt image of the same version is the same OS.

The conformance suite (`images` subtests, fixture hook `SeedImage`) runs for a driver that declares the
kind: an explicit ID, both architectures in one list, an excluded architecture, a missing ID, another
version, another channel, no version, `exactlyOne`, `newest` and `oldest`, placeholders, and the default
image.

## Metadata endpoints

A driver whose vendor serves instance metadata, and with it the node's user-data (a one-hour bootstrap token and
the Tailscale key), to the node over the network declares where, through the optional `InstanceMetadata`
capability. The core must not branch on a vendor, so it builds the node's configuration from the declaration.

| Method | Meaning |
|---|---|
| `MetadataEndpoints()` | `vendors.MetadataEndpoints{Addresses, Units}`: the IPv4 and IPv6 addresses of the vendor's metadata services as the node reaches them (a second service such as Azure's WireServer included), and the systemd units of the vendor's Flatcar image, besides Afterburn's two, that must keep reaching them (`amazon-ssm-agent.service`, `oem-gce.service`, `waagent.service`). |

On nodes of a NodeClass whose `spec.instanceMetadata.access` is `Restricted` (the default) or `HostOnly` (the same fragment without the `OUTPUT` rules: `vendors.HostOnlyMetadata`), `vendors.RestrictMetadata`
turns the declaration into one Butane fragment (the slice of the allowed units, the iptables rules for each address
and the unit that loads them) that the core merges as the part `vendor-metadata-access`, after the base and volume
configs and before the network template and the user's Butane, which can override it. Only TCP connections the node opens,
except to port 53, are restricted, because some addresses also serve DNS and NTP; list the addresses of the metadata services only, never a
range. The fragment counts in the size budget and in the trial rendering of the NodeClass controller, so an
invalid declaration (a unit name that is not a service) makes `ValidationSucceeded` false. A driver without the
capability, or one that declares no address (qemu: its VMs have no metadata service), leaves its nodes alone.
[How it works and what it cannot stop](node-bootstrap.md#instance-metadata-access). The fake driver implements the
capability so that the hook is tested without a cloud.

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
for a terminating instance at once when SCarpenter has not asked for its delete yet, and
again, once SCarpenter's own delete was accepted, only after it has been seen terminating for
longer than `--terminating-grace` (default 10 minutes; the first sighting is in memory, so a
restart restarts the clock). A driver that stops an instance before it deletes it answers
`DeleteInstance` with a `Retryable` error of code `vendors.CodeStopping` until it is stopped.
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
| `Prepare(ctx, req)` | Makes the pre-instance resources ready and returns them with the device hints. `req` carries the resolved settings, the zone, the type (`CatalogType`: family, vCPUs, local disks, compatible storage types from Keeper, empty when unknown), the placement run key and a `ResourceProvider` that gives the vendor resources the NodeClass configures (`Require(kind, key, params)`; see [vendor resources](vendor-resources.md)). |

The settings the core passes (`CreateRequest.Settings`, `PrepareRequest.Settings`)
are `VendorSettings`: `Raw` (the merged vendor block; its `storage` member holds
the selected storage profile), `Network` and `Placement` (the selected profile
blocks), the generic `Networking`, the applied `Profiles` per kind and
`RootDiskBytes`. `Prepared` holds the resource references, a `DeviceHint` per
extra volume (`Name`, `Filesystem`, `MountPath`, and either `ByID` or
`SerialPattern` with `SizeBytes`) and a `LocalHint` for instance-local disks;
the core renders them into the user-data
([first-boot volume unit](node-settings.md#extra-volumes-and-local-storage)).
`Prepared.NodeIPCIDR` is the IPv4 range of the private network the instance joins
as a second interface next to its public one (`""` for none, the default): the
`direct` template then makes the node's address inside it the kubelet's
`--node-ip` ([node bootstrap](node-bootstrap.md#the-node-ip-on-a-private-network)).
Set it only when the private interface is not the default route's; a vendor whose
primary address is private already supplies none. It comes from the resolved
network of the launch, not from the NodeClass spec, and must be IPv4.
`Prepare` must be idempotent: a retry with the same request returns the same
resources. A driver whose `Supported` or `PriceDelta` answers can change at
runtime implements `EpochReporter` (`NodeOptionsEpoch()`), so that the catalog
rebuilds.

`ManagedResources` ([vendor resources](vendor-resources.md#driver-contract)):
`Kinds`, `Resolve`, `Selectors`, `Ensure`, `InUse`, `Delete`, `List`. A resource a launch
uses must show as in use (`InUse`) while its instance lives, so the garbage
collector never deletes it; deleting one that is in use is refused (`Retryable`).

### The resolution order and its hooks

The core resolves each kind in each region in the order of ADR-022 (explicit `use`, the opt-in tag, the
vendor's default, create, a failure; [vendor resources](vendor-resources.md#resolution-order)). A driver
takes part through optional capabilities on the driver value, none required:

- `ResourceDefaults.Default(kind, region)`: the vendor's own default resources of a regional kind (AWS: the
  default VPC's default subnets; qemu: the `--qemu-bridge` bridge), or none. Never modified or deleted.
- `DefaultChecker.DefaultSuits(kind, region, defaults, settings)`: whether the default can honour the
  settings (ADR-017: a default firewall that cannot isolate runs or restrict inbound rules, a default network
  without IPv6, `publicIPv4: false`). `false` with a reason skips the default; the order goes on to creation
  and never degrades the request. An error is the vendor's and stops the order.
- `ResourceDiscoverer.Discover(kind, region)`: only for a driver that cannot express the opt-in selector.
  Without it the core builds `tags: {<opt-in key>: "true"}` with the kind's default `select`, where the
  opt-in key is `Driver.TagKey(vendors.TagUse)` (`scarpenter.sparecores.com/use`, `scarpenter-use` where `/` is
  not allowed; reserved, and not one of `TagKeys`: instances do not carry it), and runs it through `Resolve`. A
  kind whose `Selectors()` declaration has no tags skips the step.
- `KindSpec.InNetwork` names the kind that provides the network a kind lives in (AWS security groups: `subnets`);
  the driver sets `ResourceRef.Network` on what it returns (a subnet's VPC, a group's VPC, a managed network's
  own ID), and a launch names the network it chose in `ResourceRequest.Network`. `ResourceRequest.Settings`
  carries the launch's resolved settings for `DefaultSuits`; without it the core asks about the NodeClass's
  default settings.
- `KindSpec.Optional` marks a kind a launch may go without and that is never created (Hetzner's private
  networks): when no `use`, opt-in tag or vendor default provides it, `Require` returns no resource and no
  error and the status check reports no problem. The driver asks for it on every launch and attaches what
  comes back; its validation refuses a `create` of the kind.

The failure and the order's rules (a vendor error stops it, an ambiguous opt-in fails, the sources recorded in
the status) belong to the core: a driver reports what it found and writes no policy.

### The `use` selector

`Resolve(kind, region, ref)` takes a `vendors.UseRef`: the NodeClass's `use`
([resource selectors](resource-selectors.md)) for one region and zone, with the IDs of the region
gathered (the key `*` and the region's own), every string's placeholders expanded and `select`
defaulted from the declaration. An empty `UseRef` selects nothing (the driver returns a `Fatal` error): the vendor's
own default resources are `Default`'s, below. The driver does the vendor call and then
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
| `Unsupported` | Type unavailable in the zone (24 h), next candidate. A driver that found it with its own check, before any vendor call, sets `Error.PreLaunch` (`vendors.AsPreLaunch(err)`): the mark then lasts 10 minutes, because such a check is deterministic from tables the driver read and a table read while a region was briefly unreadable must not block the type for a day. The vendor's explicit "not offered" answer keeps 24 h. A driver whose vendor has no spot (Hetzner, OVH, Vultr) answers a spot `CreateInstance` with `vendors.NoSpot(vendor)` (`Unsupported`, code `NoSpot`, `PreLaunch`) and never launches on-demand under a spot NodeClaim (ADR-017); Keeper has no spot prices for such a vendor, so the catalog does not offer spot there and this is a backstop. The conformance fixture flag `NoSpot` runs the subtest `no spot` for it. Every mark is logged at V(0) with the code. |
| `QuotaExceeded` | A limit that frees up when nodes end. The quota tracker refreshes its view of the region, and the bucket the vendor counts the launch against is marked unavailable for 5 min, also when the tracker showed headroom (its view was stale). The bucket is the one the tracker finds limiting for the type, else every bucket `Consumption` returns for it; a vendor without a bucket mapping has the region and capacity type marked. Once a NodeClaim of the vendor and region is confirmed gone (its instance terminated or not found; not while it is still terminating), the marks are cleared. Next candidate in another bucket. A quota limit must never be reported as `InsufficientCapacity`. A driver of a vendor whose limits are account-wide (project or account limits, not per region) sets `Error.AccountWide` (`err.AsAccountWide()`): the capacity type is then unavailable in every region of the vendor for the same 5 minutes, so that each region does not fail once before all are gated; a NodeClaim of the vendor confirmed gone clears it. |
| `DailyLimit` | The vendor refuses to create resources in the region until a daily limit resets (OCI: "You have reached your daily resource creation limit"). Every offering of the vendor's region is unavailable until `Error.ResetAt`, or until the next 00:00 UTC when the driver leaves it zero (5 min when the error comes within 5 min after 00:00 UTC). Deleting NodeClaims does not clear it. Use it only for a limit that a deletion cannot free. |
| `InvalidOffering` | The vendor rejected the launch of this offering for a reason the driver cannot attribute (OCI: a 400 `InvalidParameter` with no known text), or for a configuration the operator fixes (OCI: an image without the shape). The offering (type, zone, capacity type) is unavailable for 1 h, an Event `OfferingRejected` carries the vendor's message, next candidate; a Create whose every attempt failed so is `InsufficientCapacity`, so core picks other types. A driver must not use it for an error it can tell is global (a missing subnet or image: `Fatal`). A NodeClass configuration that only some types cannot use (Vultr: firewall rules a bare-metal host cannot translate) is an `InvalidOffering` with `Error.ClassScoped` (`err.AsClassScoped()`): the mark and the Event are the NodeClass's, and other NodeClasses still see the offering. |
| `ZoneUnusable` | Zone unavailable (10 min). |
| `RegionUnavailable` | Only from `Zones`: the credentials cannot read the region. The facts skip the region (one warning) and load the others; if every region is unavailable the load fails. |
| `Retryable` | No mark. Returned to core, which retries the NodeClaim. |
| `NotFound` | The instance does not exist. |
| `Fatal` | `CreateError` and an Event on the SCNodeClass. |

The driver facts (`Regions`, `Zones`) load in the background on the leader, all vendors concurrently and,
within a vendor, up to 8 regions at once. Each vendor is published as soon as it is read, so a vendor that hangs
or fails holds back no other. Every driver call of a region is bounded to 30 s (`facts.DefaultRegionTimeout`) and a
vendor load to 3 minutes (`facts.DefaultVendorTimeout`), or, for a driver that implements `facts.CallBounded`
(`LoadTimeout`, the longest one call takes), the longer of these and that, per region, and of 3 minutes and six times
it, per vendor; a reload round waits for its slowest vendor, so a vendor that times out delays the retries of the others by up to its timeout; the regions that finished when the vendor times out are
published. A region that fails or times out does not fail the vendor: the vendor is published with the regions
that loaded (`Loaded` is true once any did), and the failed region stays known (a NodeClass naming it stays
Ready) with its last-known zones, or with none, so that it has no offerings. A vendor with no readable region stays
unloaded. A failed region (or an unloaded vendor) is read again with a backoff of 10 s, doubling up to 15 min, and
only when due; every vendor is read in full every `Interval` (1 h by default), also while a region keeps
failing. One Error names the failed regions of a vendor when that set changes, and the first load of a vendor is
logged at Info with its region count; the rest of the progress is at V(1).

The text of a launch failure, on the NodeClaim's `Launched` condition and in the Events, is always
`<Class> (<Code>): <vendor message>`, where Code is `Unknown` when the vendor gave none, and Class is one of `InsufficientCapacity`, `ZoneUnusable`, `QuotaExceeded`,
`DailyLimit`, `Retryable`, `Unsupported`, `InvalidOffering` and `Fatal`; an error of another class (`NotFound`, `RegionUnavailable`) or without one reads `Fatal`. When every
attempt of a `Create` failed, core's `InsufficientCapacityError` carries the last failure's text, so its event reads
`NodeClaim <name> event: insufficient capacity, <Class> (<Code>): <vendor message>`. Automation reads the class from
this text; the same class and the expiry are in [the offered listing](labels-and-offerings.md#the-offered-listing).

## Quotas

`QuotaReporter.Quotas(ctx, region)` returns the buckets `{ID, Scope, Unit, Limit,
Used}` of a region and the references of the instances counted in `Used`;
`GlobalQuotas` the account-wide ones; `Consumption(type, capacityType)` the
buckets and amounts one instance consumes (`false`: unknown, reactive path).
The tracker passes `vendors.WithQuotaRefresh(ctx, ...)`: a driver may reuse
cached limits unless `QuotaRefreshFrom(ctx).Limits` is set. `Used` counts the
whole account. `OfferingLister.LaunchableTypes` maps API references to Keeper
zone IDs. See [the controller reference](controller.md#quota).

## Billing

Billing-period-aware node retention ([ADR-024](../explanation/DECISIONS.md#adr-024-billing-period-aware-node-retention-accepted-2026-11-10),
[the how-to](../how-to-guides/reuse-nodes-within-billing-period.md), [the controller](controller.md#billing-retention))
needs two things of a driver, and the core never branches on a vendor name for either.

**`Instance.BillingStart`.** The vendor's own creation or billing-start stamp. `Create` returns it as the NodeClaim annotation
`scarpenter.sparecores.com/billing-start`; core merges it into the NodeClaim and copies it to the Node at registration, so
the controller reads it without a vendor call and a restart loses nothing. The driver sets the earliest plausible stamp: an early stamp
only makes the computed boundaries early, the safe side; a stamp that may be later than the real start (Vultr's `date_created`) keeps
its vendor unverified until a live check bounds the offset. The conformance suite (`billing start`) requires that it is not after the
instance's first `GetInstance` (a minute of skew is allowed), is stable across calls and survives `FindInstance` and `ListInstances`. Zero is
allowed: the node is then not held and an Event says so. The AWS driver sets `LaunchTime` (EC2 bills per second, so
retention never applies there); the planned vendors set hcloud's `created`, OVH's Nova `created`, Vultr's `date_created` and UpCloud's own
`scarpenter-created` label.

**`BillingDescriber`** (optional). `BillingRule(region, family) (BillingRule, bool)` returns the driver's own rule where it is finer than Keeper's
vendor row or Keeper has none; empty region and family ask for the vendor-wide rule (what the NodeClass status shows). A `BillingRule` has the
optional fields `Increment`, `Minimum` and `StoppedCharged` (a set field overrides Keeper's value of the same name; a value lower than
Keeper's needs a code comment that links the vendor's documentation), `DeleteLatency` (how long a delete takes to stop the charge: UpCloud stops, then
deletes; the margin is at least this plus 90 s), `Verified` and `Source` (for the status). A driver that does not implement it uses Keeper's
row and is **unverified**: retention is off for it, and the status says `Unverified`. A rule is verified only after a live check of
the vendor's partial-period billing and of the period's anchor (from the server's start; no vendor documents clock-hour billing).
Only two shapes are accepted: increments held (an increment of at least the NodeClass's `minPeriod`) with a minimum equal to the
increment (or none), and minimum only (an increment shorter than `minPeriod`: one boundary at the minimum); a minimum different from
a held increment is `UnsupportedShape`. The AWS driver reports its per-second rule (1 s, 60 s minimum), verified, which is under
the hold floor.

## Interruption notice window

`InterruptionWindow` is the optional capability of a driver whose vendor has spot or preemptible instances: `NoticeWindow()` returns
`NoticeWindow{MinNotice, MaxNotice}`, the shortest and the longest lead time the vendor gives before it takes a reclaimed instance
([plan 26](../plans/v1/done/26-interruption.md)). The interruption controller clamps a node's claimed deadline into `[now, now +
MaxNotice]`, and a notice without a usable deadline is `now + MinNotice`. A driver that declares none (or an invalid window: no minimum,
or a maximum under the minimum) gets `vendors.DefaultNoticeWindow`, 30 s and 5 min. The fake declares the window of the vendor it
impersonates (AWS and OCI 2 min, GCP 30 s to 2 min 30 s, Azure 30 s, Alibaba 5 min), which `--fake-min-notice` and `--fake-max-notice`
override.

## Interruption notices (the node agent)

`InterruptionNotices` is the optional capability of a driver whose vendor announces the loss of a spot or preemptible instance in the
instance metadata ([plan 26](../plans/v1/done/26-interruption.md) section 2). `InterruptionNotices()` returns a `NoticeCheck`:

| Field | Meaning |
|---|---|
| `Script` | The body of the POSIX `sh` function `scarpenter_check`. It prints `<reason> <deadline>` and returns 0 when a notice is present (the reason is `SpotInterrupted`; the deadline is RFC 3339 and may be left out), returns 1 when there is none, 2 on a transient error (the endpoint did not answer). It may use `curl`, and the variable `NODE_NAME` (the node's name, which is the NodeClaim's: a vendor whose events name the instance reads its own name from the metadata, as Azure does). It must not contain `{{` (the fragment is rendered as a template). An empty script means no agent. |
| `Interval` | The pause after a check that found no notice, in whole seconds, rounded up (AWS 2 s, Azure 1 s, Alibaba 5 s). Zero for a check that blocks until something changes; GCP's blocks in `wait_for_change` and still sets 1 s, so that a failing long poll cannot spin. |
| `NoticeWindow` | Embedded: `MinNotice` and `MaxNotice`, the same window as [`InterruptionWindow`](#interruption-notice-window); `NoticeWindowOf` reads it from here when the driver has no `InterruptionWindow`. |

`vendors.InterruptionAgent(d, restricted)` wraps the check in the generic loop and the unit (the [node agent](node-bootstrap.md#the-interruption-agent)),
and `vendors.BootFragmentsOf(d, access, spot, tailnet)` adds it after the driver's own fragments **on spot launches
only**, inside the metadata restriction's fragment when there is one (one part of the user-data). The script may be about as long as AWS's IMDSv2 check
(a token request and the action request) and little longer: the largest spot GPU case has about 280 bytes to spare (AWS's real check is that long) ([size](node-bootstrap.md#the-interruption-agent)). The core never branches on the vendor: a vendor step adds its script and implements the capability, nothing else. A driver without the
capability gets no agent. Its tests run the script with `sh` against a local HTTP fixture (the check's own responses: no notice, notice,
malformed, 5xx); the agent's size, the generic loop plus the check, stays under 1.5 KiB (`TestAgentScriptSize`).

## Interruption poller and the reason an instance is gone

`InterruptionPoller` is the optional capability of a driver whose vendor shows an interruption in its API instead of, or besides, the
instance metadata ([plan 26](../plans/v1/done/26-interruption.md) section 4): `PollInterruptions(ctx, region)` returns the `Notice{Ref,
Reason, Deadline}` of the cluster's instances in the region that are announced or under way, and `PollInterval()` the time between two
polls. The core runs one poller per (vendor, region) that has a spot NodeClaim, however many instances there are (so the driver makes a
bounded number of calls per region, one paged list and not one call per instance), and stops it when the last one is gone. The poller maps
a notice to its NodeClaim by the `instance-ref` annotation and writes the interruption annotations on the Node with source `vendor-api`; the
[interruption controller](controller.md#interruption) reacts as it does to the node agent. A notice may be returned on every poll while the
instance lives (the poller keeps one it cannot write yet until the Node exists); one for an instance that is terminated must not be. `Reason` is `SpotInterrupted`; `Deadline` is zero when the API only
shows that the instance is going (a terminating state), and the controller then assumes the vendor's shortest notice. Errors carry their
class: any error backs the region's poller off (twice the interval, doubling up to 5 minutes), counted in
`scarpenter_interruption_poll_errors_total`; a failed poll never stops the controller.

`Instance.GoneReason` is set by a driver that can tell a vendor reclaim from a crash, only on an instance that is gone (`terminating` or
`terminated`): `SpotInterrupted` for a spot reclaim, empty otherwise and for any other state (AWS's `StateReason.Code` `Server.SpotInstanceTermination` is in;
Alibaba's `Recycling` operation lock on a stopping instance and GCP's preemption operation are vendor steps of plan 26). `CloudProvider.Delete` passes it on when it finds the instance gone without having
deleted it (see [silent reclaim](controller.md#silent-reclaim)).

The conformance suite checks a driver that implements the capability (subtest `interruption poller`): a positive interval, no notice for an
instance nobody announced and, with the fixture's `Interrupt` hook, the notice with its reason and deadline, no notice for a neighbour and none
after the delete.

## Registry

Drivers register in `init()` with `vendors.Register(name, factory)`.
`--vendors` lists the enabled names; the registry keys the initialised drivers by
`Vendor()`. A driver may serve another vendor's ID than its name (the fake
driver does).

## Conformance suite

`conformance.Run(t, conformance.Fixture{...})` runs: identity, zone mapping,
idempotent create and the two key rules, NotFound semantics, malformed references
(`NotFound`, never `Fatal`), the idempotency label (drivers that store it), list by tags, the billing start rules (see [billing](#billing)), the stopped-node rule (the fixture's `PowerOff`
hook stops an instance; it must report `terminating`, stay listed and delete cleanly), delete idempotency, tag
round trip, user-data limit, error classification (the fixture's `Inject` hook
makes the next launch fail with a class), and for `QuotaReporter` drivers: every
type maps to buckets or is on the fixture's unknown list, launches show up in
`Used` with their refs, a quota error is `QuotaExceeded` (with `AccountWide` exactly when the fixture's
`AccountWideQuota` says the vendor's limits are account-wide); and `OfferingLister`.
For `ManagedResources` with a `Seed` hook (a resource made the way a user would, with a name, tags and
the attributes the vendor has) the selector cases, one subtest per group of cases and per kind. Each group
runs whenever the kind's declaration and the fixture allow it and skips with its reason otherwise: a kind
with tags is selected by its tags, a kind without tags by name (`Seed` gets no tags and the cases name what
they seed `<group>-<id>-*`), the order by label and the tie by label need tags, the ordering by name and
`{zone}` need `name`, the creation-time cases need `created` (and the fixture's `created` attribute), and a
kind with neither tags nor `name` runs only the cases by ID. Every declaration is well formed and validation
rejects what it does not support (unknown filter, attribute, placeholder, `{zone}` on a regional kind, `created` on
a kind without a creation time, an empty `use`); fields are ANDed; `ids` are ANDed with the rest, a
missing ID does not fail the others and is named, an ID that does not match is named; `*` under `ids`
is every region; zero matches and two matches under `exactlyOne` are `NoMatch` and `Ambiguous` with the
counts; `newest`, `oldest` and a tie (by name in natural order, by label, by creation time); `{zone}` per
zone; glob escapes; tags `"*"`; the CIDR attributes (a fixture with two ranges resolves the right one by `cidr`
and by `within`, and a kind without a range rejects both); and, for the fake driver, the same assertions with the
vendor evaluating the selector exactly, narrowing to a superset, or not at all
(`conformance.RunSelectors`).
For a driver that declares the `images` kind with a `SeedImage` hook the [image cases](#images) (explicit IDs, both
architectures, the version check, ordering, placeholders, the default image).
For `ManagedResources` with `Seed` and `OrderClass` hooks (and `SetDefault` where the vendor has a default of the kind;
a vendor without one leaves it nil, and `SetDefault` skips only a state the vendor cannot arrange, such as a default
without IPv6), the resolution order behind the core's engine. Each case runs whenever its preconditions hold: the
cases about the default need `SetDefault`, the opt-in tag needs a kind with tags (a kind without tags is discovered
by nothing, and `forbid` names "discovery skipped"), an explicit `use` or `create` that beats the other steps needs
tags or a default to beat, and the others (nothing found creates, an explicit `use` that matches nothing, `forbid`,
an interrupted creation, kinds in a network) run for every kind, seeding by name where there are no tags: explicit
`use` beats the opt-in tag beats the default beats create; an explicit `use` that matches nothing fails and
does not fall through (the other kinds of the NodeClass report only their default); an explicit `create` skips
discovery and the default; a default that cannot honour `publicIPv6` is skipped; `forbid` ends at a failure that
names the kind, the region and the steps; a creation interrupted midway (`FailCreate`) is adopted and completed by
the next pass; a kind that lives in a network resolves only within it. `TagKey(TagUse)` must be non-empty for every
driver (`identity`).
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
4 GiB) and `qemu.local` (2 vCPUs, 4 GiB, two local disks of 4 GB), on-demand only, and the GPU types
`qemu.gpu` (a T4) and `qemu.gpu-nvswitch` (8 A100) that only describe a GPU for the lane's
GPU-less boot (the VMs have none). `qemu.spot` (2 vCPUs, 2 GiB, dearer than `qemu.small` in both capacity types, so no other scenario
schedules onto it) is the one type with a spot offering: the driver launches spot instances like on-demand ones, and with `--qemu-notice-url`
set it implements [`InterruptionNotices`](#interruption-notices-the-node-agent) for them: the check asks `<url>/<node name>` and an empty
answer means no notice, `SpotInterrupted <RFC 3339 deadline>` is one (window 30 s to 2 min, interval 1 s). L4 scenarios Q16 and Q17 use it.

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
(`scarpenter-fake-resources`), `--fake-launch-delay`, `--fake-min-notice`, `--fake-max-notice`.

The fake's Go API has `PowerOff(ref)`, which stops an instance the way a guest
poweroff would (it stays, reported `terminating`, until deleted), `Interrupt(ref, deadline)`, which
announces the reclaim of a running instance (the fake is an `InterruptionPoller`, polled every second,
and lists it from then on; it does not take the instance away at the deadline), and `Reclaim(ref)` and
`ReclaimWithReason(ref, reason)`, which take an instance away without notice (terminated at once, its Node
deleted, `GoneReason` the given one); the conformance suite and the unit tests use them.

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
- `billing`: the vendor's billing rule and the stamp new instances get: `{increment, minimum,
  stoppedCharged, deleteLatency, verified, source, stampOffset, noStamp}` (durations as `5m`; `verified: true`
  makes the rule count; a negative `stampOffset` is a vendor that stamps early, `noStamp` one that stamps
  nothing). The fake implements `BillingDescriber` from it, so the L2 and L3 lanes can impersonate an hourly
  vendor with periods of minutes; the stamp is the fake clock at creation and is kept when the key changes.
- `powerOff`: a list of NodeClaim names; the running instance of each is powered off
  once (reported `terminating`, as `PowerOff` does) and its Node is deleted, the way a
  stopped node stops reporting (a KWOK Node would otherwise stay Ready).
- `interrupt`: a list of `{nodeClaim, deadlineSeconds}`; the running instance of each NodeClaim is announced
  once with a deadline the given seconds ahead (`Interrupt`), and the controller's poller finds it.
- `reclaim`: a list of `{nodeClaim, reason}`; the instance of each NodeClaim is taken away once with no notice
  (`ReclaimWithReason`: `reason` is the `GoneReason`, empty for a vendor that cannot tell), and its Node is deleted.
- `unsupported`: rules `{kind, profile, instanceTypes, families, zones}` (empty
  lists match everything) that make a profile invalid for the matching types and
  zones. Besides, a storage profile with local storage is valid only on types
  with local disks. A change of the rules rebuilds the catalog.
- `rootDisks`: rules `{instanceTypes, families, size}` (empty lists match everything; the
  first match wins) that make matching types define their own root disk, answered by
  `RootDiskForType`.
- `virtualGPU`: a list of instance types (with or without the vendor prefix) whose GPU is virtual,
  answered by `GPUVirtualizer`; the catalog drops them (reason `GPUVirtual`). A change rebuilds the catalog.
- `images`: the account's boot images, `{id, region (or "*": every region), name, arch (amd64 or arm64),
  channel, version, owner, deprecated, tags, created, description, state}`: what the `images` selector and the
  version check resolve against. An image says its release in `channel` and `version` or in the
  tags `flatcar-channel` and `flatcar-version`; without either it says none (`ImageVersionUnknown`).
  Without an `images` selector in the NodeClass every release has a synthesised image. The fake
  also implements `ImageReporter` and `InstanceMetadata`.
- `imageCopies`: `{official: [{region, version}], copyDuration}`: with the controller's copy flag of the
  fake's vendor (`--aws-copy-images` for the default `aws`), the fake has official images only in the listed
  regions (every release, or the `version` given; both architectures) and the other regions are served by
  the copy manager: a launch there starts a copy (`ImageCopying`), a report says `ImageCopyOnDemand`, and the
  copy is available after `copyDuration` (default 5m). Without the key, or without the flag, every region has
  an image, as before ([image copies](image-copies.md)).
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
