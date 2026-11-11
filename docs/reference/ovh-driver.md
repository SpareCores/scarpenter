# OVHcloud driver

The OVHcloud Public Cloud driver (`pkg/vendors/ovh`, vendor ID `ovh`, registered as `ovh`), built on the
OpenStack APIs (Nova, Neutron, Glance, Cinder) through gophercloud v2. Plan:
`docs/plans/v1/wip/18-ovh.md`. It implements the [driver contract](driver-contract.md) and passes the
shared conformance suite against an in-memory OpenStack cloud (`pkg/vendors/ovh/fake`); **nothing here has run
against OVH yet**: the plan's spikes S1 to S8 and the live L5 run are still open, and every fact that
depends on them is under [Not verified against OVH](#not-verified-against-ovh).

## Table of Contents

- [Packages](#packages)
- [The API layer](#the-api-layer)
- [Credentials](#credentials)
- [Regions and zones](#regions-and-zones)
- [Images](#images)
- [Launch](#launch)
- [Instances, states and references](#instances-states-and-references)
- [Node settings](#node-settings)
- [Resources](#resources)
- [Network and addresses](#network-and-addresses)
- [Tags](#tags)
- [Quotas](#quotas)
- [Types and offerings](#types-and-offerings)
- [Billing](#billing)
- [Error classification](#error-classification)
- [Controller configuration](#controller-configuration)
- [Required rights](#required-rights)
- [Not built yet](#not-built-yet)
- [Not verified against OVH](#not-verified-against-ovh)

## Packages

| Package | Contents |
|---|---|
| `pkg/vendors/ovh` | the driver (`vendors.Driver` and its capabilities) and the mapping code under it: region table, error classification, launch request builder, state mapping, IDs, families, settings, the quota reader, the image lookup, node options, managed resources |
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
(`openstackUser` mode; the service-account OIDC flow is not built, see [Not built yet](#not-built-yet)), asks for
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

## Credentials

The controller authenticates to one Public Cloud project (the project is the account scope) with the
standard OpenStack environment: `OS_AUTH_URL` (`https://auth.cloud.ovh.net/v3`), `OS_USERNAME`,
`OS_PASSWORD`, `OS_PROJECT_ID` and `OS_USER_DOMAIN_NAME` (the `openstackUser` mode of the plan; the chart wires
them from a Secret, see [Set up OVHcloud credentials](../how-to-guides/ovh-credentials.md)). `Init` checks that the variables are
there and makes no call; the authentication happens with the first call, is retried by the next one after a
failure, and gophercloud re-authenticates when the token expires. One credential covers every region (the catalog
lists per-region endpoints). The OVH IAM service-account flow (OIDC client credentials, the plan's recommended
mode) needs a small Keystone client of our own, because gophercloud has no OIDC plugin; it is not built (plan 18,
open question 3), and [`ovh-iam-policy.json`](ovh-iam-policy.json) is the policy for it.

## Regions and zones

The Keystone catalog is the truth for what a project can use; the NodeClass `regions`
list and the hand-off use the API (catalog) names, upper case. Keeper prices mostly the
legacy names (`GRA11`, `SBG5`, ...) plus `EU-WEST-PAR` and `EU-SOUTH-MIL`.
`RegionMap` (`regions.go`) holds Keeper name to API name rows, `API`, `Keeper` and
`Join` (the catalog's regions with their Keeper rows, in the catalog's order). Today
every priced Keeper region maps to itself; the trigram and inactive Keeper regions
are listed as unmapped. `NewRegionMap` takes overrides (an empty value unmaps; the
controller reads them from `SCARPENTER_OVH_REGION_ALIASES`, the chart's
`vendors.ovh.regionAliases`) and refuses two Keeper names on one API name. The driver's `Init` refuses a configured row whose
API name differs from the Keeper name: `vendors.Region` has one name, which the facts join Keeper's offerings on, so such a row
would hide the region's offerings without an error until plan 14's A8 core change adds the Keeper name to it. A test over
the committed Keeper snapshot (`test/data/sc-snapshot`) makes every Keeper OVH region appear in the table, mapped or
unmapped, and every row exist in the snapshot.

`Regions` returns the compute regions of the project's catalog that the table maps, by their API names,
cached for an hour; a catalog region without a table row is logged once and not used, because it has no offerings or
prices. A region without a Nova endpoint is not in the catalog's compute list; `Zones` and the other calls for it
are `RegionUnavailable`, and a reference into it is `NotFound`. The facts of the core join the catalog on the
region's name, which is the API name: that agrees with Keeper while the table rows are identities. A row whose API
name differs needs the Keeper name in `vendors.Region` (the A8 text of plan 14, a core change this change does not
make): until then a runtime alias would hide the region's offerings.

`Zones` reads Nova's availability zones and maps them to Keeper's zone IDs with `ZoneIDs`:
`<region>-a` for a 1-AZ region (whatever Nova calls its one zone, usually `nova`: that is the zone's `Name`),
`-a/-b/-c` for `EU-WEST-PAR` and `EU-SOUTH-MIL`. A test checks it reproduces the snapshot's zones for every
active region. The create call names the zone in multi-AZ regions only.

## Images

SCarpenter never creates images (rung 3 of the ladder, ADR-021): the operator uploads Flatcar's OpenStack image to
Glance once per region and architecture ([Upload the Flatcar image to OVH](../how-to-guides/ovh-images.md)).
`ResolveImage` finds it in the region and requires exactly one match:

- **By convention**, without an `images` selector: the project's own private image, `status` active, named
  `flatcar-<channel>-<version>-<arch>` (`flatcar-stable-4757.2.3-amd64`) with the property `architecture` (`x86_64` or `aarch64`).
- **By the NodeClass's selector** `vendors.ovh.resources.images.use`: IDs per region, a name pattern, `k=v` tags, `match.architecture`,
  `match.owner`, `match.state`, `match.createdAfter` and `createdBefore`, the `visibility` and `property:<k>` filters and
  `select`/`orderBy` (the [selector reference](resource-selectors.md#ovh)). The block's `imageOwner` is the owner the lookup
  uses when the selector names none. A community, public or shared image needs its owner or a `visibility` filter, so
  `includeCommunityImages` of the plan is the filter `visibility: community` (rung 2 stays a later step).

Whatever finds the image, its release is read from its name, its properties `flatcar_channel` and `flatcar_version`, or its tag
`flatcar-<channel>-<version>`, and must be the pinned one: another release is `ImageVersionMismatch`, an image that says
none or contradicts itself is `ImageVersionUnknown`, and neither is ever booted (ADR-017). `ReportImages` reports the amd64
image of every region of the NodeClass for the status and the `ImagesReady` condition (v1 launches no arm64 type: Keeper
has none for OVH); `ResolveImage` itself resolves arm64 too. Answers are cached for an hour, a miss for five minutes,
and a transient failure serves the last answer.

## Launch

`CreateInstance` is, in order: the user-data budget (16 KiB; Nova's own limit is 65,535 bytes base64); a **replay check**
(servers of the NodeClaim's name and cluster; the one that carries the idempotency key is returned, whatever the flavor or zone of the
replay; a server with the key in `ERROR` is deleted; a server of the same name of an earlier NodeClaim, with the cluster
marker and another key, is deleted and the call is `Retryable`); the flavor lookup by name (IDs differ per region, cached for an
hour; a name the region lacks is `Unsupported`); the region's `Ext-Net` network (the external network of that name, cached);
`BuildCreateRequest`; and `CreateAndWait`.

`BuildCreateRequest` builds the Nova create from a `vendors.CreateRequest` and the
resolved per-region ids (`LaunchPlan`): name is the NodeClaim; flavor ID; the image, or a
block device mapping when the root boots from a volume; `Ext-Net`; security group names (Nova attaches by name; a name
that matches two groups is `AmbiguousSecurityGroup`, Fatal); user data; `config_drive`
(default true); the availability zone in multi-AZ regions only; metadata (see [Tags](#tags));
the server tag `scarpenter-cluster=<cluster>` (shortened with a hash to 60 characters); data volumes as blank
volume mappings with `delete_on_termination`; the server group as scheduler hint. Golden files of the wire
body are in `pkg/vendors/ovh/testdata`.

`CreateAndWait` creates the server and polls it (2 s, at most 45 s: `SCARPENTER_OVH_SCHEDULE_WAIT`) until the task state
leaves `scheduling` or the status is `ERROR`: Nova answers 202 before a host is chosen, and a capacity failure shows seconds
later, so waiting lets the attempt loop try the next candidate. A server in `ERROR` is deleted before the
fault's class is returned, so the idempotency key is not left on a broken server; if that
delete fails the error is `Retryable` (`ErrorServerNotDeleted`). A server still
scheduling at the timeout, or whose poll fails, is returned as last seen, so its ID is
never lost, and the rest of the boot is the core's launch timeout. `FindByKey` picks the live server carrying the
idempotency key, skips servers being deleted, and returns servers with the key in `ERROR` as stale ones.

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
`GetInstance` answers `NotFound` for a deleted server; `FindInstance` and `ListInstances` leave out servers that are gone or
being deleted (nothing needs to delete them again), except a server stuck in task state `deleting` for 5 minutes or more,
which comes back as terminating so that the garbage collector deletes it again (the first sighting is in memory), and keep a server that is off by itself (`SHUTOFF`): OpenStack has
no shutdown behaviour `terminate`, so a guest poweroff (`poweroffAfter`, a crashed node) leaves a server OVH keeps
billing, and the garbage collector deletes it after the terminating grace (plan 14, A4). `DeleteInstance` treats a 404
and a reference that cannot exist as success and a 409 (a transient task state) as `Retryable`; volumes made by the create
call go with the server.

## Node settings

`vendors.ovh` of the NodeClass (the API reference is [`api.md`](api.md)): `regions` (catalog names), `imageOwner`, `storage`,
`networking`, `configDrive` (Nova `config_drive`, default true) and `resources`. The merged block is decoded by
`ParseSettings`/`ParseVendorSettings`; a test keeps its shape equal to the API type's.

| Setting | Meaning |
|---|---|
| default root disk | the flavor's local NVMe disk (what OVH sells with the flavor); `RootDiskForType` answers only for `i1` (50 GB: Keeper's `storage_size` of an `i1` adds up its NVMe disks) and for a boot-from-volume root, else the catalog's own storage size is used |
| `storage.root.bootFromVolume {volumeType, size}` | boot from a Cinder volume made from the image (`classic`, `high-speed`, `high-speed-gen2`; 10 to 12000 GiB); the flavor's disk is then unused; needs Nova 2.67 for `volume_type` in the mapping |
| `storage.data[]` | extra volumes (at most 8) as blank block device mappings with `delete_on_termination`, part of the create call so nothing is left; the hint for the first-boot unit is the serial pattern `*` and the size until spike S8 fixes the bus |
| `localStorage` | not supported: a profile that asks for it is unsupported for every type, and the vendor block is invalid with it (ADR-017) |
| placement profile `serverGroup {policy}` | a Nova server group per (profile, run key), `affinity` or `anti-affinity`, `perUse`, passed as the scheduler hint; `use {name}` references an existing group; a group that cannot be satisfied fails the launch with `NoValidHost` (`InsufficientCapacity`) |
| network profiles | none exist on OVH (bandwidth is per flavor) |

`PriceDelta` is Keeper's GB-month price of the volume type in the region (`/storage_prices?vendor=ovh`, loaded in the
background) divided by 730, for the boot volume and every data volume; incomplete where a price is missing. The
public address is free. `Supported` removes the profiles that are invalid or ask for `localStorage`.
`ValidateNodeClass` rejects, besides the above: `networking.publicIPv4: false` (a private-only node needs a billed gateway),
region names that are not the catalog's, malformed `use` selectors and IDs (Neutron and Glance UUIDs), and tags that do not fit Nova metadata.

## Resources

| Kind | Needed to launch? | `use` | Managed |
|---|---|---|---|
| `images` | yes | IDs, name, tags, `match`, filters (above) | never created |
| `securityGroups` | no | IDs (UUID), name, `match.description` | one per NodeClass and region, `idleTTL`; for `isolation: run` a per-run group, `perUse` |
| `serverGroups` | only with a placement profile | IDs, name | one per profile and run key, `perUse` |

Neutron security groups and Nova server groups have no tags the driver can rely on (the tag extension is unverified,
spike S5), so ownership is kept where the resource has room: a managed security group's **description** holds a marker
(`SCarpenter managed {"cluster":...,"nodeclass":...,"managed-by":"scarpenter","resource-kind":...}`), and a managed server group,
which has only a name, is recognised by the cluster's deterministic name prefix `scarpenter-<cluster>-`. A group without the marker
is never adopted or deleted. Consequences: `tags` are not offered in the selector for these kinds, the NodeClass's own tags
(`spec.tags`) cannot be put on them, and the NodeClass of a managed **server group** is not recoverable (its lifecycle,
`perUse`, does not need it). A managed security group holds Neutron's egress defaults, a self-reference rule for the isolation
`nodeclass` (and `run` without a run key), and the NodeClass's `networking.inbound` rules; the rules follow the parameters
(missing rules are created, others removed). The project's `default` group is not used as a default: nodes always get a managed
group, or the one you `use`. `InUse` is whether a server of the region lists the group (or a server group has members); Neutron refuses to delete a
group a server uses and the answer is `Retryable`.

## Network and addresses

A node attaches to the public `Ext-Net` network only: a public IPv4 and IPv6 without floating IPs, one port per node (the port quota
counts). `publicIPv4: true` is the only accepted value; `publicIPv6` is informational because `Ext-Net` ports carry both families
(unverified, S5). `networking.inbound.rules` and `isolation` go to the managed security group (CIDR rules; self-reference for
`nodeclass` and `run`, which also works over the public addresses because the rule matches the group), and do not apply to a group you `use`.
A group that admits everything only from private ranges drops node-to-node and control-plane traffic over public addresses: with
it only the `tailscale` join works, not `direct`. A private (vRack) network can make OpenStack choose the private interface as
the default route: private networks, gateways and IPv6-only nodes are not supported in v1. `Instance.Addresses` holds every address of
the server, IPv4 first; under `direct` the node's address is the public IPv4. With the metadata service reachable from Pods
(`169.254.169.254`, no hop limit as on AWS), the driver offers the hardening `metadata-from-pods` (`spec.bootstrap.hardening`).

## Tags

Nova metadata keys are restricted in practice, so every logical tag goes through `TagKey` as `scarpenter-<key>` (`cluster`, `nodeclaim`,
`nodepool`, `nodeclass`, `managed-by`) and the idempotency key as `scarpenter-idempotency-key`; the NodeClass's own tags are
`scarpenter-tag-<key>` (at most 100, each key with its prefix and each value at most 255 characters). The server tag
`scarpenter-cluster=<cluster>` lets a list ask the cluster's servers server side (microversion 2.26); a Nova that ignores the
filter returns everything and the driver filters on the metadata too.

## Quotas

`QuotaReader` implements `vendors.QuotaReporter` on a provider: per region the buckets
`instances`, `cores` and `ram` (MiB) from Nova's absolute limits, the usage being the
whole project (foreign servers count); a limit of -1 (unlimited) gives no bucket;
`Counted` are the servers that are not deleted.
`GlobalQuotas` is empty: every OVH limit is per project and region, so `QuotaExceeded` is never account-wide. `Consumption` is 1 instance, the flavor's vCPUs and its RAM;
`false` for spot, for families the driver does not launch and for flavors not read yet
(flavors are read per region and cached for an hour; `Quotas` and `LaunchableTypes` read them). GPU flavors have a separate quota
and are not launched. Stock is not exposed by Nova: a flavor with no capacity fails at the scheduler.

## Types and offerings

Launched families: `b2`, `b3`, `c2`, `c3`, `r2`, `r3`, `d2` and `i1`, with their `-flex`
variants. Excluded: GPU families, Metal (`bm`) and Keeper's inactive legacy families
(`eg`, `hg`, `s1`, `sp`, `vps`). A test over the snapshot asserts every Keeper OVH family
is in one list, and the conformance suite checks that every type is launched or listed as unknown for the quota. `RootDiskGB` gives the local root disk
of a type from Keeper's data: the `storage_size`, except `i1`, whose `storage_size` includes the local NVMe disks and
whose root is the 50 GB entry. `LaunchableTypes` is the flavors of the region that the driver launches, mapped to
every zone of the region (OVH does not say which zone lacks a flavor); there is no spot, no preemption and no arm64 type.

## Billing

`BillingStart` is Nova's `created`, which is at `BUILD`: the meter starts at `ACTIVE` (unverified), so the boundaries fall early, the safe side.
`BillingRule` reports every region unverified, so [billing retention](../how-to-guides/reuse-nodes-within-billing-period.md)
stays off for OVH: per second with a one-minute minimum for EU, CA and APAC (the OVH billing page; the minimum is under the hold floor,
so retention would not hold a node there anyway), hourly for `US-` regions per OVH's terms, conflicting with the US support
guide. A stopped or paused instance is billed. The live check (create and delete at 1, 59 and 61 minutes per region and read the
usage) decides the US rule and the delete latency.

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

## Controller configuration

Enabled with `--vendors=ovh` (the chart: `vendors.ovh.enabled`). The credentials are the `OS_*` variables above; the driver reads two
more from its own environment instead of flags (the core's options are not extended by a vendor): `SCARPENTER_OVH_SCHEDULE_WAIT`
(a Go duration, default 45s) and `SCARPENTER_OVH_REGION_ALIASES` (`KEEPER=API` pairs, an empty API name leaves the region unused). See the
[controller reference](controller.md).

## Required rights

The driver calls Nova (flavors, servers, availability zones, limits, server groups), Neutron (networks, security groups and
rules), Glance (read images) and Cinder (volume types). For an OpenStack user of the project those are the roles Compute
Operator, Network Operator, Network Security Operator, Image Operator (read is enough, but no read-only image role is documented)
and Volume Operator; for the IAM service-account mode the actions of [`ovh-iam-policy.json`](ovh-iam-policy.json)
(`publicCloudProject:openstack:computeOperator`, `networkOperator`, `networkSecurityOperator`, `imageOperator`, `volumeOperator`).
The exact least-privilege set is spike S6.

## Not built yet

- The service-account OIDC credentials mode (Keystone federation with an `id_token`; `sdk.New` takes any authenticated provider
  client, so it plugs in with a `ReauthFunc`).
- `vendors.Region` carrying the Keeper name (plan 14, A8): needed only for an alias row that differs from the Keeper name.
- Private networks (a second NIC on a vRack network), gateways, IPv6-only nodes, GPU and Metal flavors, arm64, `localStorage`,
  Glance community publishing (S1), automatic image import (`ImageImporter`), flavor stock from the OVH API (`OfferingLister` over `flavor.available`).
- The live L5 run, the spikes S1 to S8 and the OVH-side IaC (images, controller account), which are the maintainers'.

## Not verified against OVH

Everything in the plan's spikes: the Keystone catalog's region names and the Nova
availability zone names; the microversions 2.26, 2.64 and 2.67; the exact texts and
statuses of the errors above; whether a security group filters the `Ext-Net` port and
whether the default group is open; the Neutron tag extension; Glance `community` visibility and image import;
user data size limits; `config_drive`; boot of the Flatcar image and its disk bus (the data-volume
device hint); `Ext-Net` IPv6 and address exhaustion; the billing rules; the least-privilege rights; token lifetime.
