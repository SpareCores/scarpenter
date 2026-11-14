# OCI driver

The `oci` driver (`pkg/vendors/oci`, vendor ID and registration name `oci`): instances through
`LaunchInstance` from Flatcar's own `oraclecloud` image imported into your tenancy, the tenancy's
subscribed regions and availability domains, fixed named sizes of the flexible shapes, managed and
brought networks (VCN, subnet, network security groups) and quota-aware placement from the Limits
service. Design: plan 25, overview section 11, ADR-020 to ADR-024. The contract it implements is the
[driver contract](driver-contract.md); the settings it reads are in [node settings](node-settings.md);
the resource engine around it is in [vendor resources](vendor-resources.md); the selector is in
[resource selectors](resource-selectors.md#oci). How to make the images:
[Flatcar images on OCI](../how-to-guides/oci-images.md). How to give the controller an identity:
[OCI credentials](../how-to-guides/oci-credentials.md).

It is enabled with `--vendors=oci` and `--oci-compartment` (the chart: `vendors.oci.enabled`,
`vendors.oci.compartment`; see the [controller reference](controller.md)). Tests run against the in-memory
API of `pkg/vendors/oci/fake` and the shared conformance suite; nothing in the repository's tests calls
Oracle (the first live run is the L5 lane).

## Table of Contents

- [Capabilities](#capabilities)
- [What is different on OCI](#what-is-different-on-oci)
- [Regions and zones](#regions-and-zones)
- [Instance types](#instance-types)
- [Images](#images)
- [Launch](#launch)
- [Node settings](#node-settings)
- [Resources and network](#resources-and-network)
- [What your IaC provides](#what-your-iac-provides)
- [Tags and names](#tags-and-names)
- [Quotas and availability](#quotas-and-availability)
- [Error mapping](#error-mapping)
- [The embedded catalog](#the-embedded-catalog)
- [Deferred](#deferred)
- [What is not verified against OCI yet](#what-is-not-verified-against-oci-yet)

## Capabilities

| Interface | OCI |
|---|---|
| `Driver` | subscribed regions, availability domains as zones, `ResolveImage`, `CreateInstance`, `GetInstance`, `FindInstance`, `ListInstances`, `DeleteInstance`, `TagKey` |
| `NodeClassValidator` | syntax of the `oci` block: regions, boot volume, metadata keys, tags, resource references, and the networking options a managed network cannot honour |
| `ImageReporter` | `status.vendors.oci.images`, condition `ImagesReady`: one image per region and architecture |
| `NodeOptions`, `EpochReporter`, `KeeperPriceConsumer` | defaults (64 GB `balanced` boot volume), `PriceDelta` from Keeper's storage rows, `Prepare` (subnet and groups); no profiles |
| `ManagedResources`, `ZoneChecker` | kinds `subnets`, `networkSecurityGroups` and the internal `runNetworkSecurityGroups`; the use-only `images` selector; zones no brought subnet can serve |
| `QuotaReporter` | region buckets per quota name from the Limits service (see [Quotas](#quotas-and-availability)) |
| `NodeHardener` | the opt-in hardening `metadata-from-pods` |
| `RegionPriced` | prices are per region: the catalog offers every type in every availability domain of the region |

No `OfferingLister` (the types are named sizes and the driver cannot list them without the catalog:
the quota buckets and the reactive `Unsupported` stand in), no `BillingDescriber` (retention is off,
see [below](#what-is-not-verified-against-oci-yet)), no `ResourceDefaults` (a tenancy has no default VCN).

## What is different on OCI

- **Sizes are named per shape.** `VM.Standard.E5.Flex.2-16` is the shape with 2 OCPUs and 16 GB; the
  names come from the catalog (see [Instance types](#instance-types)).
- **Availability domains are named per tenancy** (`Xxxx:EU-FRANKFURT-1-AD-1`) and most regions have one.
  The zone name is the name without the prefix; the zone ID is `<region>-ad-<N>`, "AD number N of the
  tenancy". Keeper's prices are treated per region, and the driver checks, in your tenancy, which ADs
  offer the shape.
- **Images are regional and private.** Rung 3 of ADR-021: you import Flatcar's image in every region
  and architecture.
- **No default network and no server-side tag filter.** A tenancy has no default VCN, so the order of
  ADR-022 goes from discovery straight to `create`; tags are matched on the client.
- **"Out of host capacity" is HTTP 500.** The launch runs without the SDK's retry policy so that the
  driver classifies it.

## Regions and zones

`Regions` is the subscribed regions (`READY`), cached for an hour; the NodeClass's `regions` narrows
them. `Zones(region)` is the region's availability domains: `Zone{ID: <region>-ad-<N>, Name: <name
without the tenancy prefix>}`, `N` from the suffix `-AD-<N>`. The full name goes in the launch.

The catalog maps a price row of any availability domain of a region to every zone the tenancy lists for
it (the driver says so with `vendors.RegionPriced`); whether a type can launch in a zone is the driver's
check against `ListShapes` of the AD (cached for an hour): a shape the AD does not offer is `Unsupported`
(`ShapeNotInAvailabilityDomain`) without a launch call.

## Instance types

The catalog of OCI is [embedded](#the-embedded-catalog): 256 sized types of 12 flexible shapes (1 to 64
OCPUs and the shape's maximum, at 2, 4 and 8 GiB per vCPU), the bare metal shapes and the fixed VM
shapes. The parse rule (`ParseType`): the name is split at the last dot; a tail of the form
`<ocpus>-<gb>` (integers from 1, no leading zero) is a size of a flexible shape, anything else is a fixed
shape (`BM.Standard3.64`, `VM.Standard2.1`). Names that do not parse are rejected, never guessed
(`InvalidInstanceType`). A size is checked against the shape table of the availability domain before the
launch: the OCPU, memory and memory-per-OCPU bounds, and the NUMA limits (sizes beyond one NUMA node,
"extended memory", need a separate limit and are `Unsupported` with `ExtendedMemoryUnsupported` in v1).
The launch sends `shapeConfig {ocpus, memoryInGBs}` for a sized name and nothing for a fixed shape. The
vCPUs are never sent: an OCPU is two vCPUs on x86 and on Arm A2 and A4, one on A1.

Spot is OCI's preemptible capacity (a 50 % discount on the flexible VM shapes), terminated on reclaim
with the boot volume deleted.

## Images

`ResolveImage` finds the image of the pinned release in the controller's compartment, per region and
architecture:

- **Default** (no `resources.images`): `ListImages` by the display name
  `flatcar-<channel>-<version>-<arch>` and the operating system `Flatcar`, available images only. The
  platform images the API also lists are ignored.
- **`resources.images.use`**: `ids` (a region key, never `*`; each is read with `GetImage`, so an image
  in another compartment works), `name` (a glob), `tags` (freeform tags), `match` (`architecture`, `state`,
  `createdAfter`, `createdBefore`; no `owner`), the native `filters` `operating-system`,
  `operating-system-version`, `shape` and `lifecycle-state`, `select` (default `exactlyOne`) and `orderBy`.
- **The architecture** of an image is the freeform tag `arch`, else derived from the shapes it is
  compatible with (any Arm shape means arm64, else amd64), else unknown; an image of unknown architecture
  never satisfies a request.
- **The version check** (ADR-017): the version is the tag `flatcar-version`, else the operating system
  version; the channel is the tag `flatcar-channel` and is compared only when present. An image of another
  version is `ImageVersionMismatch`, one that says none is `ImageVersionUnknown`; it is never booted.
- Answers are cached for an hour, misses for five minutes, and a transient failure serves the last answer.
  `Image.RootDiskBytes` is the image's `sizeInMBs` (about 46.6 GiB); the boot volume may be larger, never
  smaller.

## Launch

`CreateInstance` checks the user data (16 KiB) and the metadata total (32,000 bytes), looks the
idempotency key up in the region's instances (a replay in another AD, shape or capacity type returns the
original), checks the type against the AD's shape table, then calls `LaunchInstance` once without a retry
policy. The request: the controller's compartment, the AD's full name, the shape and `shapeConfig`, the
image with the boot volume size, performance (VPUs per GB) and key, the VNIC (the subnet that serves the
zone, the public address, the groups, the tags), the `user_data` metadata (the Ignition, base64; never SSH
keys), `areLegacyImdsEndpointsDisabled: true`, the five launch options of a Flatcar arm64 image
(`UEFI_64` and paravirtualized volumes and network), the preemptible configuration for spot, the freeform
tags and the header `opc-retry-token` (`scarpenter-` and 32 hex of a hash of the NodeClaim, the offering
and an attempt counter). The call returns the `PROVISIONING` instance; a node that never registers is the
core's 15 minute rule. A launch into a managed VCN takes the subnet of the VCN that serves the zone.

`DeleteInstance` reads the instance first and refuses one without this cluster's tag (the compartment and
this check are the only walls: OCI policy cannot test freeform tags on every verb), then terminates it with
the boot volume. An instance that does not exist, or is terminating or terminated, is success; a reference
that is not `<region>/ocid1.instance....` is `NotFound`.

States: `PROVISIONING`, `STARTING` are pending; `RUNNING`, `CREATING_IMAGE`, `MOVING` running;
`STOPPING`, `STOPPED`, `TERMINATING` terminating (the stopped-node rule: a stopped instance's boot volume
still costs, so it is deleted); `TERMINATED` terminated (OCI lists it for about twelve hours; the driver's
list drops it). Provider ID `scarpenter://oci/<region>/<nodeclaim>`, instance reference
`<region>/<instance OCID>`. `GetInstance` reads the addresses from the VNIC attachments (the serving
certificate approver needs them).

## Node settings

`spec.vendors.oci`: `regions`, `storage.root` (`sizeGb` 50 to 32768, default 64; `performance`
`lower-cost`, `balanced` (default), `higher-performance` or `ultra-high-performance`; `vpusPerGb` 30 to 120
in steps of 10 for the last; `kmsKeyId`), `networking` (the generic block; `publicIPv4` defaults to true),
`metadata` (`user_data` and `ssh_authorized_keys` are reserved) and `resources`. Data volumes, local
storage, profiles and a per-NodeClass compartment are not supported in v1 (the compartment is the
controller's, `--oci-compartment`). `spec.tags` become freeform tags; a `scarpenter-` prefix is reserved.

`PriceDelta` is the boot volume's GB times Keeper's GB-month price of the region and level over 730 hours
(the level's price includes its VPUs); it is incomplete when the price is unknown and for VPUs above the
level's. IPv4 is free. `RootDiskBytes` is the volume's size. Validation also refuses what a managed
network cannot honour (`publicIPv4: false` needs a brought subnet with a NAT gateway, `publicIPv6: true`
needs one with an IPv6 prefix) and the shapes of OCIDs, regions and tags.

The opt-in hardening `metadata-from-pods` (`spec.bootstrap.hardening`) drops Pod traffic to the metadata
service (169.254.169.254), which answers any process and holds the node's `user_data`.

## Resources and network

| Kind | Scope | Default lifecycle | `use` | `create` |
|---|---|---|---|---|
| `subnets` | region | `idleTTL` | `ids`, `name`, `tags`, `match` (`network`, `cidr`, `within`, `state`, `zone`, `createdAfter`, `createdBefore`), `filters` (`vcn-id`, `lifecycle-state`, `display-name`); default `exactlyOne` | a VCN (`create.cidr`, default `10.240.0.0/16`), an internet gateway, a route table (`0.0.0.0/0` to the gateway), a minimal security list (egress all, ingress ICMP type 3 code 4) and one regional public subnet of the first /20 |
| `networkSecurityGroups` | region, in the subnet's VCN | `idleTTL` | the same fields without `zone` and `cidr`; default `all` | one group per NodeClass and VCN: egress all, ICMP 3/4, the rules of `networking.inbound`, the group itself for `isolation: nodeclass` |
| `runNetworkSecurityGroups` | run | `perUse` | not configurable | one group per run key, allowing the members to reach each other |
| `images` | region | | [Images](#images) | never |

A **regional subnet serves every availability domain**; an AD-specific one serves its own (the other ADs
are gaps, `ZoneGaps`). A brought subnet that prohibits public addresses with `publicIPv4: true`, or has no
IPv6 prefix with `publicIPv6: true`, cannot host a node: the zone is skipped and the resource check reports
it. Without `use` (ADR-022): subnets and groups that carry the freeform tag `scarpenter-use=true`, else
`create`, else `ResourcesReady=False` naming what was tried; a tenancy's other VCNs are never used without an
opt-in. A managed VCN is the resource of the `subnets` kind (it stands for its parts, as the VPC does on
AWS); `Delete` removes the subnet, the route table, the gateway, the security list and the VCN in that
order, only those that carry the ownership marker (cluster and managed-by), and the VCN only when no subnet
is left in it. A 409 on a delete is `Retryable`. `InUse` reads the compartment's VNIC attachments (subnets)
or the group's VNICs. The gateway, NAT gateways, IPv6 on a managed VCN and peering are not created in v1.

## What your IaC provides

The controller's compartment and the subscriptions of the regions, the images per region and
architecture, the controller's identity and its [policy](oci-iam-policy.txt), optionally a subnet and
groups (with egress: Ignition needs it at first boot), and the limits and compartment quotas you want. See
[infrastructure hand-off](infrastructure-handoff.md#oci).

## Tags and names

Instances carry the freeform tags `scarpenter-cluster`, `scarpenter-nodeclaim`, `scarpenter-nodepool`,
`scarpenter-nodeclass`, `scarpenter-managed-by` and `scarpenter-idempotency-key`, and the NodeClass's own tags.
Every managed resource carries `scarpenter-cluster`, `scarpenter-nodeclass`, `scarpenter-managed-by` and
`scarpenter-resource-kind` and a display name that starts with `scarpenter-<cluster>-`; both the marker and
the name are required before anything is deleted. The node name is the NodeClaim name; no OCI hostname label
is set.

## Quotas and availability

The Limits service: `GetResourceAvailability(compute, <limit name>, <tenancy>, <AD>)` per quota name the
region's shapes list (`standard-e5-core-count`, `standard-e5-memory-count`, ...). A bucket is region-scoped
with `Used` the sum over the ADs and `Limit` = `Used` plus the largest `available` of any AD, so a family is
blocked only when no AD has room. Consumption: the OCPUs for `*-core-count`, the GB for `*-memory-count`, one
for other names. A shape whose limit is not readable, and every spot type until the preemptible limit is
known, has unknown quota (reactive). The usage is read on the tenancy (the root compartment).

## Error mapping

| Class | OCI |
|---|---|
| `InsufficientCapacity` | HTTP 500 `InternalError` whose message says "Out of host capacity" |
| `QuotaExceeded` | `LimitExceeded`, `QuotaExceeded` (not account-wide) |
| `Unsupported` | 400 `InvalidParameter` for a shape not compatible with the image or not available in the AD; the driver's own `InvalidInstanceType`, `ShapeNotInAvailabilityDomain`, `ExtendedMemoryUnsupported` |
| `ZoneUnusable` | a subnet with no free IP addresses |
| `Retryable` | 429, 5xx, network errors, `IncorrectState`, `LockConflict`, a 409 on a delete, the driver's call timeout |
| `NotFound` | 404 on get, terminate and the resource gets; a malformed instance reference |
| `RegionUnavailable` | a region that is not subscribed or readable |
| `Fatal` | everything else: 404 on a launch (a missing subnet, image or compartment, or no permission), 401, 403, other 400, a token conflict with a foreign instance |

Every SDK call runs under a 45 second deadline; reads and deletes are retried by the adapter (not by the
SDK, whose retry loop cannot run under a deadline without a data race), a launch never.

## The embedded catalog

Keeper does not serve OCI yet: the catalog comes from `pkg/catalog/embeddeddata/oci` (generated by
`hack/oci-catalog/generate.py`), served for the vendors of `--embedded-catalog-vendors` (default `oci`; see the
[controller reference](controller.md)). Prices and sizes are static and as of the data's observation date; the
start-up log and the gauge `scarpenter_catalog_embedded_vendor` say so. The swap to Keeper is removing `oci`
from the flag.

## Deferred

Instance principal and OKE workload identity credentials, data volumes and local storage, profiles, a
per-NodeClass compartment, NAT gateways and IPv6 on a managed VCN, bare metal's one-hour minimum, an image
import or Community Application, and a zone-aware quota (plan 25, open questions).

## What is not verified against OCI yet

Everything here comes from public documentation and the SDK source; the first live run settles it: the exact
status, code and message of "Out of host capacity" and of each error row, that capacity and limit errors are
synchronous on the launch call, the replay semantics of `opc-retry-token`, that the tenancy's
`GetResourceAvailability` includes the compartments' usage and the scope of every compute limit, preemptible
limits, that a node boots with the legacy IMDS endpoints disabled, the arm64 image on `A2` and `A4`, the
policy verbs per call ([policy](oci-iam-policy.txt)), the freeform tag key charset, the token exchange
parameters of the keyless credentials, and the minimum billing of OCI (billing retention is off).
