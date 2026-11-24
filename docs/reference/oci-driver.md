# OCI driver

The `oci` driver (`pkg/vendors/oci`, vendor ID and registration name `oci`): instances through
`LaunchInstance` from Flatcar's own `oraclecloud` image imported into your tenancy, the tenancy's
subscribed regions and availability domains, fixed named sizes of the flexible shapes, managed and
brought networks (VCN, subnet, network security groups) and quota-aware placement from the Limits
service. Design: plan 25, overview section 11, ADR-020 to ADR-024. The contract it implements is the
[driver contract](driver-contract.md); the settings it reads are in [node settings](node-settings.md);
the resource engine around it is in [vendor resources](vendor-resources.md); the selector is in
[resource selectors](resource-selectors.md#oci). How to make the images:
[Set up OCI: the Flatcar image](../how-to-guides/set-up-oci.md#1-the-flatcar-image). How to give the controller an identity:
[Set up OCI: credentials](../how-to-guides/set-up-oci.md#2-credentials).

It is enabled with `--vendors=oci` and `--oci-compartment` (the chart: `vendors.oci.enabled`,
`vendors.oci.compartment`, and optionally `vendors.oci.imageCompartment` for the images; see the [controller reference](controller.md)). The unit tests run against the in-memory
API of `pkg/vendors/oci/fake` and the shared conformance suite; the real-cloud L5 lane (`test/e2e-oci/`) runs on
pull requests that touch OCI code, nightly and before every release.

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
- [The catalog and billing](#the-catalog-and-billing)
- [Interruption](#interruption)
- [Deferred](#deferred)
- [What is not verified against OCI yet](#what-is-not-verified-against-oci-yet)

## Capabilities

| Interface | OCI |
|---|---|
| `Driver` | subscribed regions, availability domains as zones, `ResolveImage`, `CreateInstance`, `GetInstance`, `FindInstance`, `ListInstances`, `DeleteInstance`, `TagKey` |
| `NodeClassValidator` | syntax of the `oci` block: regions, boot volume, metadata keys, tags, resource references, and the networking options a managed network cannot honour |
| `ImageReporter` | `status.vendors.oci.images`, condition `ImagesReady`: one image per region and architecture |
| `NodeOptions`, `TypeChecker`, `EpochReporter`, `KeeperPriceConsumer` | defaults (64 GB `balanced` boot volume), `PriceDelta` from Keeper's storage rows, `Prepare` (subnet and groups); no profiles; `LaunchableType` drops sizes beyond one NUMA node of their shape |
| `ManagedResources`, `ZoneChecker` | kinds `subnets`, `networkSecurityGroups` and the internal `runNetworkSecurityGroups`; the use-only `images` selector; zones no brought subnet can serve |
| `QuotaReporter` | region buckets per quota name from the Limits service (see [Quotas](#quotas-and-availability)) |
| `InstanceMetadata` | the metadata addresses `169.254.169.254` and `fd00:c1::a9fe:a9fe`, no allowed unit besides Afterburn's |
| `InterruptionPoller`, `InterruptionWindow` | preempted preemptible instances found by polling the compartment's instances ([Interruption](#interruption)); notice window 5 s to 2 min |
| `RegionPriced` | prices are per region: the catalog offers every type in every availability domain of the region |

No `OfferingLister` (the types are named sizes and the driver cannot list them without the catalog:
the quota buckets and the reactive `Unsupported` stand in), a `BillingDescriber` that is not verified (retention is off,
see [below](#the-catalog-and-billing)), no `ResourceDefaults` (a tenancy has no default VCN).

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

The catalog of OCI is [Keeper's](#the-catalog-and-billing): 256 priced sized types of 12 flexible shapes (1 to 64
OCPUs and the shape's maximum, at 2, 4 and 8 GiB per vCPU), the bare metal shapes and the fixed VM
shapes. The parse rule (`ParseType`): the name is split at the last dot; a tail of the form
`<ocpus>-<gb>` (integers from 1, no leading zero) is a size of a flexible shape, anything else is a fixed
shape (`BM.Standard3.64`, `VM.Standard2.1`). Names that do not parse are rejected, never guessed
(`InvalidInstanceType`). A size is checked against the shape table of the availability domain before the
launch: the OCPU, memory and memory-per-OCPU bounds, and the NUMA limits (sizes beyond one NUMA node,
"extended memory", need a separate limit and are `Unsupported` with `ExtendedMemoryUnsupported` in v1). The driver also says so
before any launch (`vendors.TypeChecker`) from the NUMA limits of the shape tables it has read, so the
catalog does not offer such sizes (reason `NotLaunchable`); a size is offered until its shape table has been
read, and the epoch changes when a shape's NUMA limits do.
The launch sends `shapeConfig {ocpus, memoryInGBs}` for a sized name and nothing for a fixed shape. The
vCPUs are never sent: an OCPU is two vCPUs on x86 and on Arm A2 and A4, one on A1.

Spot is OCI's preemptible capacity (a 50 % discount on the flexible VM shapes), terminated on reclaim
with the boot volume deleted.

## Images

`ResolveImage` finds the image of the pinned release in the image compartment, per region and
architecture. The image compartment is `--oci-image-compartment` (chart `vendors.oci.imageCompartment`) and
defaults to the controller's compartment; set it when your IaC imports the images into another one. Only the
image listing reads it: instances, boot volumes and the network stay in `--oci-compartment`, and the
controller's identity needs `read instance-images` in the image compartment ([policy](oci-iam-policy.txt)).
The option also applies to every `images.use` selector that lists images (`name`, `tags`, `match`, `filters`):

- **Default** (no `resources.images`): `ListImages` in the image compartment by the display name
  `flatcar-<channel>-<version>-<arch>` and the operating system `Flatcar`, available images only. The
  platform images the API also lists are ignored.
- **`resources.images.use`**: `ids` (a region key, never `*`; each is read with `GetImage`, so an image
  in any compartment the identity can read works, with or without the image compartment), `name` (a glob), `tags` (freeform tags), `match` (`architecture`, `state`,
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
certificate approver needs them). Node IP: The node's only interface has the subnet's private address (the public one is NAT'ed), which the kubelet of the `direct` template registers without a hint, so the driver reports no node IP range ([node bootstrap](node-bootstrap.md#the-node-ip-on-a-private-network)).

## Node settings

`spec.vendors.oci`: `regions`, `storage.root` (`sizeGb` 50 to 32768, default 64; `performance`
`balanced` (default), `higher-performance` or `ultra-high-performance`, not `lower-cost`, which OCI
offers for block volumes only; `vpusPerGb` 30 to 120 in steps of 10 for the last; `kmsKeyId`), `networking` (the generic block; `publicIPv4` defaults to true),
`metadata` (`user_data` and `ssh_authorized_keys` are reserved) and `resources`. Data volumes, local
storage, profiles and a per-NodeClass compartment are not supported in v1 (the compartment is the
controller's, `--oci-compartment`). `spec.tags` become freeform tags; a `scarpenter-` prefix is reserved.

`PriceDelta` is the boot volume's GB times Keeper's GB-month price of the region and level over 730 hours
(the level's price includes its VPUs); it is incomplete when the price is unknown and for VPUs above the
level's. IPv4 is free. `RootDiskBytes` is the volume's size. Validation also refuses what a managed
network cannot honour (`publicIPv4: false` needs a brought subnet with a NAT gateway, `publicIPv6: true`
needs one with an IPv6 prefix) and the shapes of OCIDs, regions and tags.

The metadata service (169.254.169.254 and `fd00:c1::a9fe:a9fe`) answers any process and holds the node's
`user_data`; by default (`spec.instanceMetadata.access: Restricted`) only Afterburn's units reach it, and
DNS and NTP at the address keep working ([instance metadata access](node-bootstrap.md#instance-metadata-access)).

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
AWS); `Delete` removes the subnet, the route table, the security list, the gateway and the VCN in that
order, only those that carry the ownership marker (cluster and managed-by), and the VCN only when no subnet
is left in it. The VCN's default route table and security list (OCI creates them, with the VCN's tags) are
neither used nor deleted: they go with the VCN. The garbage collection skips a region whose listing answers
`NotAuthorizedOrNotFound` (a policy limited to one region). A 409 on a delete is `Retryable`. `InUse` reads the compartment's VNIC attachments (subnets)
or the group's VNICs. The gateway, NAT gateways, IPv6 on a managed VCN and peering are not created in v1.

## What your IaC provides

The controller's compartment and the subscriptions of the regions, the images per region and
architecture (in the controller's compartment, or in another one that you name with `--oci-image-compartment`), the controller's identity and its [policy](oci-iam-policy.txt), optionally a subnet and
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
for other names; a scope infix before `-count` (`standard-e3-core-ad-count`, `standard-a1-core-regional-count`)
keeps the core or memory form. A shape whose limit is not readable, and every spot type until the preemptible limit is
known, has unknown quota (reactive). The usage is read on the tenancy (the root compartment).

## Error mapping

Before a launch the driver reads the image's compatibility list
(`ListImageShapeCompatibilityEntries`, one uncached read per launch, allowed by `read instance-images`, which the
launch needs anyway); a shape that is not in it is `InvalidOffering` (`ImageNotCompatible`) without a launch call.
OCI itself answers that with a bare 400 `InvalidParameter`. Custom images are only compatible with the shapes you
add: add every shape SCarpenter may launch, including `VM.Optimized3.Flex` (see
[Set up OCI](../how-to-guides/set-up-oci.md)). A list the controller cannot read does not stop the launch. When a
launch is still rejected, find the `opc-request-id` of the error in the OCI audit log of the compartment
for OCI's message.

| Class | OCI |
|---|---|
| `InsufficientCapacity` | HTTP 500 `InternalError` (or `InternalServerError`, the code on Oracle's API errors page) whose message says "Out of host capacity" |
| `DailyLimit` | `LimitExceeded` whose message says "daily resource creation limit" (matched case-insensitively on that phrase only). Oracle documents no reset time for it (its [API errors page](https://docs.oracle.com/en-us/iaas/Content/API/References/apierrors.htm) only says to request an increase or delete resources), so the region is unavailable until the next 00:00 UTC |
| `QuotaExceeded` | any other `LimitExceeded`, and `QuotaExceeded` (not account-wide) |
| `Unsupported` | 400 `InvalidParameter` for a shape not compatible with the image, not available in the AD or not available as preemptible; the driver's own `InvalidInstanceType`, `ShapeNotInAvailabilityDomain`, `ExtendedMemoryUnsupported` |
| `InvalidOffering` | any other 400 `InvalidParameter` on a launch, and the driver's own `ImageNotCompatible` (OCI's text is in the Event `OfferingRejected`): the offering is unavailable for 1 h and the next candidate is tried |
| `ZoneUnusable` | a subnet with no free IP addresses (on a launch) |
| `Retryable` | 429, 5xx, network errors, `IncorrectState`, `ExternalServerIncorrectState`, `LockConflict`, a 409 on a delete, the driver's call timeout |
| `NotFound` | 404 on get, terminate and the resource gets; a malformed instance reference |
| `RegionUnavailable` | a region that is not subscribed or readable |
| `Fatal` | everything else: 404 on a launch (a missing subnet, image or compartment, or no permission), 401, 403, other 400 on a call that is not a launch (a read's `InvalidParameter` is a malformed request of ours, never an offering's: its text is not matched), a token conflict with a foreign instance |

Every SDK call runs under a 45 second deadline; reads and deletes are retried by the adapter (not by the
SDK, whose retry loop cannot run under a deadline without a data race), a launch never.

## The catalog and billing

OCI is read from Keeper's snapshot like every vendor, so the deployment needs a Keeper API key
(`keeper.apiKey.existingSecret`). Keeper gives every size of a flexible shape one `api_reference`; rule R
names the sizes by their server ID. Keeper has no benchmark scores of OCI yet. Keeper lists servers it has no
price of (the `VM.Standard.E6-HM.Ax.Flex` sizes on 2026-11-17): they are not types until it prices them. The
data is a moving snapshot, so prices and sizes can change under a running NodePool; the names stay.

Keeper's vendor row has one billing value for OCI: a one hour minimum and stopped servers charged. The driver
implements `BillingDescriber` and overrides it by family, from Oracle's documentation (not a live check, so
the rule is not verified and billing retention stays off): per second with a 60 second minimum for VMs
([release note](https://docs.oracle.com/iaas/releasenotes/changes/20facdce-34bf-4648-a0a7-de5db73657ce/index.htm));
bare metal has a one hour minimum, which Keeper's `family` (Standard, Optimized, Dense I/O) cannot tell from a
VM, so it is not held either. A stopped server is not charged for Standard shapes and charged for Dense I/O ([Oracle](https://docs.oracle.com/iaas/Content/Compute/Tasks/resource-billing-stopped-instances.htm); the
Ampere, burstable and standard bare metal shapes are not named there and count as Standard); Optimized
mixes both (VM.Optimized3 pauses, BM.Optimized3 continues) and keeps Keeper's row. Nothing reads the stopped charge
today, and the driver terminates instances, never stops them.

## Interruption

A preemptible instance is spot on OCI (`preemptibleInstanceConfig` with the `TERMINATE` action, boot volume not preserved). OCI documents
that it cannot be stopped, started or rebooted, that it is reclaimed with an Events message of type
`com.oraclecloud.computeapi.instancepreemptionaction` (`data.additionalDetails.preemptionAction` `TERMINATE`) about two minutes before the
termination begins, and that a preemption cannot be simulated. SCarpenter does not read the Events service (no queue, rule or
permission to set up); the [interruption controller](controller.md#interruption) reacts to what the driver's poller sees through the
Compute API.

**The signal.** The Compute SDK shows nothing of a preemption before the termination: `Instance` has no preemption field, the Compute
service's work requests and maintenance events are not documented for it, and the Audit service lags. The cheapest reliable signal is
the lifecycle state in the list the driver makes anyway: `PollInterruptions(region)` is one paged `ListInstances` of the compartment
(`--oci-interruption-poll`, default 10 s, about six calls a minute per region whatever the number of nodes, no new IAM statement) and
reports every preemptible instance of this cluster (the cluster tag) that is `STOPPING`, `STOPPED` or `TERMINATING` as a
`SpotInterrupted` notice. Nobody can stop a preemptible instance, so these states mean the preemption has begun, except for the instances
SCarpenter terminated itself: the driver remembers its own terminations (for an hour, in memory), and the poller skips every NodeClaim with
core's `InstanceTerminating` condition, which is stored on the NodeClaim and so also covers a restart. The list is of the whole compartment
(the Compute API has no tag filter); instances without this cluster's tag are dropped client-side.

**What it gives (decided by the maintainer 2026-11-20).** The poller sees a preemption only once the instance is `STOPPING`, `STOPPED` or
`TERMINATING`: there is **no lead time**, because the two-minute notice exists only as the Events message. The notice has no deadline, so the
controller takes the window's minimum, 5 s, from the moment it sees it; with the default `--interruption-margin` of 5 s the termination
deadline is that moment, and core deletes the Pods at once with a 1 s grace. So on OCI today: the reason is recorded (`SpotInterrupted`,
`source=vendor-api`), the node is cleaned up fast (well before garbage collection, which waits for the Node to be NotReady and then up to two
minutes) and the offering is marked unavailable, but there is **no graceful drain window yet**. A throttled poll (429) is retryable and backs
the region's poller off.

**The reason an instance is gone.** `Instance.GoneReason` is `SpotInterrupted` for a preemptible instance in a terminating or terminated state,
so a reclaim the poller missed (the instance gone between two polls, or SCarpenter restarted) is still recorded as `SpotInterrupted` with
`source=vendor-api` instead of `ReclaimedWithoutNotice`. A person who terminates a preemptible instance in the console is indistinguishable
from a preemption without the Audit service.

**Next step: an in-VM check.** None for OCI yet. The next step (plan 26) is a node agent check of the instance metadata for a preemption key
(`instancePreemptionAction`; sources disagree), verified on the first real preemption; if it holds, the node gets the two minutes as a drain
window and the poller stays as the backstop.

The fake models the signal: `fake.Cloud.Preempt(id)` puts a preemptible instance into `TERMINATING`; the conformance fixture's `Interrupt`
hook uses it, and the fixture launches preemptible instances.

## Deferred

Instance principal and OKE workload identity credentials, data volumes and local storage, profiles, a
per-NodeClass compartment, NAT gateways and IPv6 on a managed VCN, bare metal's one-hour minimum in the billing rule, an image
import or Community Application, and a zone-aware quota (plan 25, open questions).

## What is not verified against OCI yet

**Preemption, until the first real preemption in a real run:** that the instance goes `TERMINATING` (and not `STOPPING`) when OCI
reclaims it, how long that takes after the Events message, and that nothing before it shows in the API. No spike simulated a preemption
(OCI offers no way to; plan 26, spike S1 was skipped); the poller follows the documented behaviour only. The instance metadata's
preemption key is unverified too.

The L5 lane runs launches on demand and preemptible (preemptible "Out of host capacity" is seen and falls
back to another shape), arm64 on `A1`, nodes with the legacy IMDS endpoints disabled, quotas, the managed
network and its teardown, and the keyless exchange with a GitHub OIDC token in a file. Still from public
documentation and the SDK source only: the exact status, code and message of each error row, that capacity
and limit errors are synchronous on the launch call, the replay semantics of `opc-retry-token`, that the
tenancy's `GetResourceAvailability` includes the compartments' usage and the scope of every compute limit,
preemptible limits, the arm64 image on `A2` and `A4`, the policy verbs beyond what the lane exercises (its
policy has `manage volume-family` where [the driver's](oci-iam-policy.txt) has `use`), the freeform tag key
charset, the chart's projected ServiceAccount token with an identity domain, and the billing rule
(documentation only, so billing retention is off).
