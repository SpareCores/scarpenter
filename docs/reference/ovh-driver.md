# OVHcloud driver (in progress)

The OVHcloud Public Cloud driver (`pkg/vendors/ovh`, vendor ID `ovh`), built on the
OpenStack APIs (Nova, Neutron, Glance, Cinder) through gophercloud v2. Plan:
`docs/plans/v1/wip/18-ovh.md`. **Only the lower layers exist so far**: the
`vendors.Driver` implementation, the registration, the NodeClass types, the
conformance wiring, the `images` resolution and the e2e lane are not built yet, and
nothing here has run against OVH (the plan's spikes S1 to S8 are still open). This page
describes what the package contains today, so the later change plugs into it.

The contract it will implement is in the [driver contract](driver-contract.md).

## Table of Contents

- [Packages](#packages)
- [The API layer](#the-api-layer)
- [Regions and zones](#regions-and-zones)
- [Launch request](#launch-request)
- [Instances, states and references](#instances-states-and-references)
- [Error classification](#error-classification)
- [Quotas](#quotas)
- [Types](#types)
- [Settings](#settings)
- [Not built yet](#not-built-yet)
- [Not verified against OVH](#not-verified-against-ovh)

## Packages

| Package | Contents |
|---|---|
| `pkg/vendors/ovh` | pure mapping code: region table, error classification, launch request builder, state mapping, IDs, families, settings, the quota reader |
| `pkg/vendors/ovh/osapi` | SDK-free types and the interfaces (`Compute`, `NetworkAPI`, `Images`, `Volumes`, `Provider`) the driver depends on |
| `pkg/vendors/ovh/osapi/sdk` | the only code that touches gophercloud; implements `osapi` |
| `pkg/vendors/ovh/fake` | an in-memory OpenStack cloud implementing `osapi.Provider` |

## The API layer

The driver and its tests call `osapi.Provider` (the Keystone catalog's compute regions
and per-region clients), never the SDK. `osapi.Compute` has flavors, servers (detailed
list with name and tag filters, get, create, delete), availability zones, absolute
limits and server groups; `NetworkAPI` has networks and security groups with their
rules; `Images` lists Glance images; `Volumes` lists Cinder volume types. Failures
that reach the service are `*osapi.APIError` (HTTP status, service, message); a region
without an endpoint is `osapi.ErrNoEndpoint`.

The gophercloud adapter (`sdk`) authenticates from the standard `OS_*` environment
(`openstackUser` mode; the service-account OIDC flow is not built, a seam is marked), asks for
Nova microversion 2.67 (server tags, `volume_type` in block device mappings, the
server group `policy`), encodes user data itself (gophercloud sends data that happens to
be valid base64 as is), refuses a create without a network (never `networks: auto`,
which asks Neutron's auto-allocated topology for a private network instead of `Ext-Net`),
retries a 429 (honouring `Retry-After`) and a 502, 503 or 504 of an idempotent call (GET,
HEAD, DELETE; never a POST: a create the gateway timed out may have happened, and the
idempotency-key lookup on the core's retry covers it) with jittered backoff, reads the server back after the create (the create answer holds only the ID; if
the read fails the minimal server is returned so the ID is never lost), and reads
server, network and server group lists from the raw JSON because gophercloud's structs
lack the fields the driver needs. From microversion 2.47 a server's flavor has no ID: the
driver reads the flavor name (`original_name`) into `Server.FlavorName`.

The fake models: catalog regions with and without endpoints, flavors with their own
IDs per region, the server state machine (`BUILD` with task state `scheduling`, then
`ERROR` with the `NoValidHost` fault or `ACTIVE`, advanced by reads), anchored name
regex and tag filters, metadata limits, server groups (anti-affinity bounded by a host
count), absolute limits with `OverQuota` (foreign servers count, `ERROR` servers count
until deleted), a delete that keeps the status with task state `deleting` (still counted,
volumes and groups held) for `DeleteReads` calls before the server is gone, a guest poweroff leaving a `SHUTOFF` server, the 65,535-byte base64
user data limit, flavor and image checks, volumes created by block device mappings and
deleted by `delete_on_termination`, Neutron's default group and creation defaults, a
group in use refusing delete, Glance visibility and owner rules, and injected errors per
region and call. Pagination and token re-authentication belong to gophercloud; the
adapter's tests cover paging with a test HTTP server.

## Regions and zones

The Keystone catalog is the truth for what a project can use; the NodeClass `regions`
list and the hand-off use the API (catalog) names, upper case. Keeper prices mostly the
legacy names (`GRA11`, `SBG5`, ...) plus `EU-WEST-PAR` and `EU-SOUTH-MIL`.
`RegionMap` (`regions.go`) holds Keeper name to API name rows, `API`, `Keeper` and
`Join` (the catalog's regions with their Keeper rows, in the catalog's order). Today
every priced Keeper region maps to itself; the trigram and inactive Keeper regions
are listed as unmapped. `NewRegionMap` takes overrides (an empty value unmaps) and
refuses two Keeper names on one API name. A test over the committed Keeper snapshot
(`test/data/sc-snapshot`) makes every Keeper OVH region appear in the table, mapped or
unmapped, and every row exist in the snapshot. The snapshot has no price table, so the
test covers every region row.

`ZoneIDs` maps Nova's availability zones of a region to Keeper's zone IDs
(`<region>-a`; `-a/-b/-c` for the two 3-AZ regions). A test checks it reproduces the
snapshot's zones for every active region.

## Launch request

`BuildCreateRequest` builds the Nova create from a `vendors.CreateRequest` and the
resolved per-region ids (`LaunchPlan`): name is the NodeClaim; flavor ID; the image, or a
block device mapping when the root boots from a volume; `Ext-Net`; security group names;
user data (budget 16 KiB, Nova's own limit is 65,535 bytes base64); `config_drive`
(default true); the availability zone in multi-AZ regions only; metadata (`scarpenter-<key>`
for every logical tag, `scarpenter-tag-<k>` for the NodeClass's tags, and the idempotency
key as `scarpenter-idempotency-key`); the server tag `scarpenter-cluster=<cluster>`
(shortened with a hash to 60 characters); data volumes as blank volume mappings with
`delete_on_termination`; the server group as scheduler hint. Golden files of the wire
body are in `pkg/vendors/ovh/testdata`.

## Instances, states and references

The provider ID is `scarpenter://ovh/<region>/<nodeclaim>`; the instance reference is
`<region>/<server UUID>`, and a reference that does not parse (no region, an ID that is
not a UUID) is `NotFound`. `InstanceState` maps Nova statuses: `BUILD`, `REBUILD`, the
resize and migration statuses and any unknown status to pending; `ACTIVE` and the reboot
statuses to running; `SHUTOFF`, `STOPPED`, `PAUSED`, `SUSPENDED`, `SHELVED*`, `RESCUE`,
`DELETING` and `ERROR` to terminating (a stopped node is billed and never comes back,
so the garbage collector deletes it after the grace period); `DELETED` and
`SOFT_DELETED` to terminated. `ServerState` adds the task state: Nova keeps a server's
status while it is deleted, so task state `deleting` is terminating whatever the status.

`CreateAndWait` creates the server and polls it (2 s, at most 45 s) until the task state
leaves `scheduling` or the status is `ERROR`. A server in `ERROR` is deleted before the
fault's class is returned, so the idempotency key is not left on a broken server; if that
delete fails the error is `Retryable` (`ErrorServerNotDeleted`). A server still
scheduling at the timeout, or whose poll fails, is returned as last seen, so its ID is
never lost. `FindByKey` picks the live server carrying the idempotency key, skips servers
being deleted, and returns servers with the key in `ERROR` as stale ones to delete before
the next create.

## Error classification

OpenStack gives HTTP statuses and prose, so the driver classifies by status plus message
text, under the rules of the [driver contract](driver-contract.md): each text is below,
pinned by `TestClassify`, decides only with its status (and service), and a text that
matches nothing is `Fatal`. All texts are Nova's and Neutron's upstream texts, to be
confirmed on OVH.

| Class | Status, service, text |
|---|---|
| `InsufficientCapacity` | 500 (a server's fault) "No valid host was found"; 500 "Exceeded maximum number of retries" |
| `QuotaExceeded` | Nova 403 "Quota exceeded for instances", "cores", "ram", "server_groups", "Quota exceeded, too many server groups", "Maximum number of ports exceeded"; Neutron 409 `OverQuota`. Never account-wide: OVH's limits are per project and region |
| `Unsupported` | Nova 400 "Invalid flavorRef provided", "Flavor's disk is too small", "Flavor's memory is too small" |
| `ZoneUnusable` | 409 or 500 "No more IP addresses available" |
| `RegionUnavailable` | no endpoint for the region in the catalog; Keystone 403 for a region |
| `NotFound` | any 404 |
| `Retryable` | 429, 5xx, a 409 that is neither a quota nor an ambiguous group name, timeouts, canceled calls, network errors |
| `Fatal` | everything else: other 400s (including a missing or inactive image: "Invalid imageRef provided", "Can not find requested image", "is not active"), Nova 409 "Multiple security groups found" (a group name more than one group has: Nova attaches by name, a retry fails the same way), any 401 (Keystone's included: the credentials, not a region), 403 of Nova without a quota text |

Reading the catalog's regions (`CatalogRegions`, through `ClassifyCatalog`) is not about
one region: a Keystone 401 or 403 there is `Fatal`; the contract keeps `RegionUnavailable`
for `Zones`.

`ClassifyFault` classifies the fault of a server in `ERROR` the same way, using the
fault's code as status.

## Quotas

`QuotaReader` implements `vendors.QuotaReporter` on a provider: per region the buckets
`instances`, `cores` and `ram` (MiB) from Nova's absolute limits, the usage being the
whole project (foreign servers count); a limit of -1 (unlimited) gives no bucket;
`Counted` are the servers that are not deleted.
`GlobalQuotas` is empty. `Consumption` is 1 instance, the flavor's vCPUs and its RAM;
`false` for spot, for families the driver does not launch and for flavors not read yet
(flavors are read per region and cached for an hour).

## Types

Launched families: `b2`, `b3`, `c2`, `c3`, `r2`, `r3`, `d2` and `i1`, with their `-flex`
variants. Excluded: GPU families, Metal (`bm`) and Keeper's inactive legacy families
(`eg`, `hg`, `s1`, `sp`, `vps`). A test over the snapshot asserts every Keeper OVH family
is in one list. `RootDiskGB` gives the local root disk of a type from Keeper's data: the
`storage_size`, except `i1`, whose `storage_size` includes the local NVMe disks and
whose root is the 50 GB entry.

## Settings

`Settings` (`ParseSettings`) reads the merged vendor block in the plan's shape:
`configDrive`, `storage.root.bootFromVolume {volumeType, size}`, `storage.data[]`
(at most 8, volume types `classic`, `high-speed`, `high-speed-gen2`, 10 to 12000 GiB),
`networking`. `localStorage` and `publicIPv4: false` are invalid. `RootDiskBytes` is the
`bootFromVolume` size, or 0 for the flavor's local disk.

## Not built yet

Marked `TODO(plan 14)` in the code or listed here: the `vendors.Driver` methods
(`Regions`, `Zones`, `ResolveImage` through the `images` kind, `CreateInstance` with the
find-then-create idempotency and the scheduler wait, `GetInstance`, `FindInstance`,
`ListInstances`, `DeleteInstance`); registration; the NodeClass `ovh` block and its
validation; node options (`Supported`, `PriceDelta`, `Prepare`, device hints);
`securityGroups` and `serverGroups` resources; `OfferingLister`; the service-account
OIDC credentials mode (gophercloud has no OIDC plugin); `vendors.Region` carrying the
Keeper name (plan 14, A8); conformance wiring; the e2e lane, the canary guard and the
sweeper; the chart values; the credentials and images how-tos.

## Not verified against OVH

Everything in the plan's spikes: the Keystone catalog's region names and the Nova
availability zone names; the microversions 2.26, 2.64 and 2.67; the exact texts and
statuses of the errors above; whether a security group filters the `Ext-Net` port;
user data size limits; `config_drive`; boot of the Flatcar image.
