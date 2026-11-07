# AWS driver

The first production driver (`pkg/vendors/aws`, vendor ID and registration name
`aws`): instances through `RunInstances` without launch templates, regions and
zones of the account, Flatcar images by name, vendor-native node settings,
managed resources (subnets with their VPC, security groups, placement groups)
and quota-aware placement from Service Quotas. Design: overview section 11,
ADR-013, ADR-018, ADR-020. The contract it implements is in the
[driver contract](driver-contract.md); the settings it reads are in
[node settings](node-settings.md); the resource engine around it is in
[vendor resources](vendor-resources.md). The IAM policy it needs is
[`aws-iam-policy.json`](aws-iam-policy.json).

It is enabled with `--vendors=aws` (the default). Tests run against the in-memory
EC2 and Service Quotas of `pkg/vendors/aws/fake` and the shared conformance suite;
nothing in the repository's tests calls AWS (the first live run is the L5 lane).

## Table of Contents

- [Capabilities](#capabilities)
- [Credentials and clients](#credentials-and-clients)
- [Regions and zones](#regions-and-zones)
- [Images](#images)
- [Launch](#launch)
- [Node settings](#node-settings)
- [Resources and network](#resources-and-network)
- [Network addresses and egress](#network-addresses-and-egress)
- [Tags](#tags)
- [Quotas](#quotas)
- [Error mapping](#error-mapping)
- [IAM](#iam)
- [Deferred](#deferred)
- [What is not verified against AWS yet](#what-is-not-verified-against-aws-yet)

## Capabilities

| Interface | AWS |
|---|---|
| `Driver` | regions, zones, `ResolveImage`, `CreateInstance`, `GetInstance`, `FindInstance`, `ListInstances`, `DeleteInstance`, `TagKey` |
| `NodeClassValidator` | syntax of the `aws` block and of the AWS blocks of the profiles |
| `ImageReporter` | `status.vendors.aws.images`, condition `ImagesReady` |
| `NodeOptions`, `EpochReporter` | defaults, `Supported`, `PriceDelta`, `Prepare` with device hints; the epoch changes when the instance type table or the prices load |
| `ManagedResources` | kinds `subnets`, `securityGroups`, `runSecurityGroups`, `placementGroups` |
| `QuotaReporter`, `OfferingLister` | 18 vCPU quota buckets per region; launchable types by zone ID |

## Credentials and clients

- Credentials come from the SDK's default chain (environment, web identity token
  file, shared configuration, static keys from a Secret). One credential covers
  all regions. `Init` builds the configuration but makes no call: credentials are
  resolved with the first API call, so an unreachable AWS does not keep the
  controller from starting.
- Clients (`ec2`, `servicequotas`) are created lazily, one per region. Retries use
  the SDK's adaptive mode with its defaults. The user agent carries
  `scarpenter/<version>`.
- **Per-call timeouts.** Every API call runs under its own deadline: 45 seconds
  (90 for `RunInstances`), covering the SDK's retries of that call; a paginated
  call gets it per page. A call that runs into it fails with a `Retryable` error
  (code `CallTimeout`).
- `--aws-home-region` (default `us-east-1`) is the region the global listing
  (`DescribeRegions`) is read from.
- The driver reads the EBS prices from Keeper's anonymous
  `/storage_prices?vendor=aws&currency=USD&limit=-1` (`--keeper-url`; one row per
  region and volume type, `GB/month`), in the background from the first read of the
  regions and every six hours. Until they are loaded, and for any volume type Keeper
  lacks, the price of an offering is incomplete (`price-incomplete: "true"`).
  **Keeper has no public IPv4 price**: the driver uses a constant, 0.005 USD per
  hour (what AWS charges in every commercial region since 2024-02-01), which
  `SCARPENTER_AWS_IPV4_PRICE` overrides. It is not from Keeper (a Keeper/crawler
  addition is on the backlog).
- `Init` gives the SDK configuration the home region as its default region: the
  web identity provider (the recommended credentials) builds its STS client from
  that configuration, so without it a cluster that does not set `AWS_REGION` fails
  every call with "Missing Region". An explicit `AWS_REGION` wins.

## Regions and zones

- `Regions`: `DescribeRegions` from the home region, enabled regions only (an
  opt-in region the account has not enabled is not usable), cached for 1 hour.
- `Zones`: `DescribeAvailabilityZones` per region with `zone-type=availability-zone`,
  opted in and `state=available`, cached for 1 hour. The driver maps Keeper's
  portable zone ID (`use1-az1`) to the zone name of **this account**
  (`us-east-1a`; names differ between accounts). `topology.kubernetes.io/zone`
  carries the name, `scarpenter.sparecores.com/zone-id` the ID. A zone the account
  does not have, or has not opted in to, is dropped; local zones and Wavelength
  zones are not offered.
- A launch into a zone name the account does not have is `ZoneUnusable`.

## Images

- `ResolveImage`: `DescribeImages` with owner `075585003325` (the official Flatcar
  account; `aws.imageOwner` overrides it, for the status and for the launch), `state=available`, `architecture`
  `x86_64` or `arm64`, and the name of the release:

  | Architecture | Name |
  |---|---|
  | amd64 | `Flatcar-<channel>-<version>-hvm` |
  | arm64 | `Flatcar-<channel>-<version>-arm64-hvm` |

  `<channel>` is `stable`, `beta`, `alpha` or `lts`, as the NodeClass spells it.
  Exactly one image must match: none is `NotFound` (code `ImageNotFound`), more
  than one is `Fatal` (`ImageAmbiguous`). The root device name and size come from
  the image. Results are cached for 1 hour.
- **Published regions** (verified 2026-11-07 from `flatcar_production_ami_all.json`
  of the public release server, both architectures; no AWS call): the lists of the
  `amd64-usr` and `arm64-usr` releases name the same regions per channel: 20 for
  stable (4757.2.3), 31 for lts (4081.3.12). A region without a published image
  (for example `ap-northeast-3` in stable) reports the image not ready. The
  public lists carry AMI IDs only, not names: the name pattern follows overview
  section 11.3 and Flatcar's `Flatcar-<channel>-*` naming and is asserted in
  `image_test.go`; plan 10 confirms it with a real `DescribeImages`.
- `ReportImages` (`ImageReporter`): the image of the NodeClass's release in every
  region of its `aws` block (every region of the account without a list) for
  both architectures, cached like `ResolveImage`. A region or architecture
  without an image is reported `ready: false` with the reason (a missing image is
  not asked for again for 5 minutes). The NodeClass controller turns that into
  `ImagesReady=False` only when a region the NodeClass **names** has no image, or
  when no region has one; otherwise `ImagesReady` is `True` with reason
  `ImagesPartial` naming the gaps, and the offerings of a region in which no image
  is ready are unavailable (per region, not per architecture: a region with an amd64
  but no arm64 image is not gated, an arm64 launch there is skipped when it fails),
  and a transient `DescribeImages` error is not an answer: the last good image is
  served, or the report fails and nothing is gated (so a default NodeClass is not permanently `False`
  because stable is not published in a default-enabled region).

## Launch

`CreateInstance` makes one `RunInstances` call (`MinCount=MaxCount=1`):

| Field | Value |
|---|---|
| `ClientToken` | the NodeClaim's UID (the idempotency key) |
| `ImageId`, `InstanceType` | the resolved image, the type's `api_reference` |
| `NetworkInterfaces[0]` | device 0, the chosen subnet, the security groups, `AssociatePublicIpAddress` from `networking.publicIPv4` (default true), `Ipv6AddressCount=1` for `publicIPv6`, ENA Express from a network profile, delete on termination |
| `BlockDeviceMappings` | the root volume under the image's root device name, then the data volumes as `/dev/sdf`, `/dev/sdg`, ...; all `DeleteOnTermination` |
| `Placement` | the zone, the placement group's name, `Tenancy=dedicated` from a placement profile |
| `MetadataOptions` | `HttpTokens=required`, `HttpPutResponseHopLimit=1`, endpoint enabled, tags in metadata off (overridable in `aws.metadataOptions`) |
| `InstanceInitiatedShutdownBehavior` | `terminate` |
| `InstanceMarketOptions` | for spot: one-time, terminate on interruption |
| `IamInstanceProfile` | `aws.instanceProfile`, only if set (nodes hold no cloud credentials otherwise) |
| `TagSpecifications` | instance, volume and network interface: the NodeClass's tags, the [logical tags](#tags) and `Name=<nodeclaim>` |
| `UserData` | the rendered Ignition, base64; refused with `Fatal` (`UserDataTooLarge`) above 16 KiB raw, the budget of the bootstrap renderer |

No launch templates, no `CpuOptions` (custom CPU options do not reduce the quota
use and are never set).

- **Ownership.** `DeleteInstance` first reads the instance and refuses (`Fatal`) one
  that does not carry this cluster's tag, so a wrong or tampered instance reference
  cannot terminate a foreign instance under broad credentials.
- **Spot requests** are tagged like the instance (IAM allows `RunInstances` on them
  only with the cluster tag); EC2 refuses tags on the request of an on-demand launch,
  so only spot launches carry them.
- **Idempotency.** A replay of the same request returns the original instance
  (EC2 answers it). A replay with other parameters (another zone, type or capacity
  type) is refused by EC2 with `IdempotentParameterMismatch`; the driver resolves
  it to the instance the token created (`DescribeInstances` by `client-token`,
  also when that instance is terminated). A failed create leaves the token
  unused.
- **States.** `pending` is `pending`; `running`, `stopping` and `stopped` are
  `running` (they still exist and cost, and are deleted with their NodeClaim);
  `shutting-down` is `terminating`; `terminated` is `terminated`.
- **Addresses.** `Instance.Addresses` lists the private and public IPv4 and the
  IPv6 addresses of every interface; the serving certificate approver checks the
  node's IP SANs against them.
- `FindInstance` and `ListInstances` filter by the logical tags over pending,
  running, stopping and stopped instances; regions the account cannot use list
  nothing. `DeleteInstance` treats an unknown, malformed or terminated instance as
  success.

## Node settings

Built-in defaults (layer 1): a **gp3 root volume of 64 GiB, encrypted** with the
account's default EBS key, and IMDSv2 with hop limit 1. The `aws` block and the
storage profiles override them key by key.

| Block | Fields |
|---|---|
| `storage.root`, `storage.data[]` | `volumeType` (`gp2`, `gp3`, `io1`, `io2`, `st1`, `sc1`, `standard`), `volumeSize`, `iops`, `throughput`, `encrypted` (default true), `kmsKeyID`; data volumes also `name`, `filesystem`, `mountPath` |
| `storage.localStorage` | `policy` (`ignore`, `mount`, `raid0`), `mountPath`: instance store disks |
| placement profile | `placementGroup: {strategy: cluster\|spread\|partition, partitionCount}` or `{use: {name}}`; `tenancy: dedicated` |
| network profile | `enaExpress: {enabled, udp}` |
| `metadataOptions`, `instanceProfile`, `imageOwner` | as in the [API](api.md) |

**Validation** (`ValidateNodeClass`, shown in the NodeClass status): region names,
instance profile name (`[\w+=,.@-]{1,128}`), hop limit 1 to 64, tag keys and values
(no `aws:` prefix, no `scarpenter.sparecores.com/` keys, at most 40 tags), inbound
CIDRs, resource references (`subnet-`, `sg-`, `pg-` IDs per region; selector tags),
and the EBS limits per volume type:

| Type | Size (GiB) | IOPS | Throughput (MiB/s) | Boot volume |
|---|---|---|---|---|
| gp2 | 1 to 16384 | none | none | yes |
| gp3 | 1 to 65536 | 3000 to 80000, at most 500 per GiB | 125 to 2000, at least 4 IOPS per MiB/s | yes |
| io1 | 4 to 16384 | 100 to 64000 (needed), at most 50 per GiB | none | yes |
| io2 | 4 to 65536 | 100 to 256000 (needed), at most 1000 per GiB | none | yes |
| st1, sc1 | 125 to 16384 | none | none | no |
| standard | 1 to 1024 | none | none | yes |

(The limits are those of the AWS documentation, cross-checked on 2026-11-07 with
Keeper's anonymous `/table/storage` rows, which give GB and MB/s: gp3 70369 GB,
80000 IOPS, 2097 MB/s; io2 256000 IOPS; io1 64000; st1/sc1 from 134 GB.)

**`Supported`** per instance type (facts from `DescribeInstanceTypes`, read at
runtime, so nothing is guessed from names): local storage needs instance store
disks that appear as NVMe devices; `io2` above 64000 IOPS needs a Nitro type; ENA
Express needs a type that supports it; a placement group of a strategy needs a
type that supports the strategy (burstable types have no cluster groups); a group
by name and `tenancy: dedicated` are accepted. Until the table is read a profile
that depends on it is not offered, and the epoch of the node options changes when
it arrives.

**`PriceDelta`**: every EBS volume (root and data) at Keeper's GB-month price of
the region and volume type divided by 730, plus the public IPv4 address price when
`publicIPv4` is on. The price is **incomplete** for `gp3` IOPS above 3000 or
throughput above 125 MiB/s, for every `io1` and `io2` volume (IOPS are priced
separately), for dedicated tenancy and for any price Keeper does not have.

**Attach timing.** The first-boot volume unit waits for its device for a bounded
time measured from the volume service, not from boot (240 s after plan 07's
change). The driver never attaches volumes after the launch: every EBS volume is a
block device mapping of the `RunInstances` call, so it exists before the instance
boots and its NVMe device (serial `vol` + volume ID without the dash, size) shows up
with the other devices, normally within seconds. The hints (`vol*` plus the size)
need nothing that exists only after the call. A slow attach is therefore an EC2
anomaly, listed for plan 10 ("slow EBS attach vs device timeout").

**`Prepare`** returns the resources (subnet of the zone, security groups,
placement group) and the device hints for the first-boot unit: an EBS volume
matches by NVMe serial `vol*` plus its size (the root volume has partitions and is
skipped; volumes of equal size rely on the volume unit's lock), instance store
disks by `nvme-Amazon_EC2_NVMe_Instance_Storage_*` with the type's disk count. A
profile that creates placement groups needs a run key. A spread group takes seven
instances per zone: the eighth in a zone fails before the call with
`InsufficientCapacity` (code `PlacementGroupFull`), so only that offering is
skipped. Errors of the resource provider, in particular `ResourceMissingInZone`,
are returned unchanged so that a launch skips only the zone.

## Resources and network

| Kind | Scope | Default lifecycle | What it is |
|---|---|---|---|
| `subnets` | region | `idleTTL` | the regional network: the account's default VPC with its default subnet per zone, as found; where there is none (or the NodeClass says `create`), a managed VPC |
| `securityGroups` | region | `idleTTL` | one group per NodeClass and VPC |
| `runSecurityGroups` | run | `perUse` | one extra group per run key (isolation `run`); not configurable |
| `placementGroups` | run | `perUse` | one group per region, profile and run key |

Deletion order: placement groups, run groups, NodeClass groups, then the network.

- **Subnets.** `use` by IDs per region, name pattern (tag `Name`, wildcards, `{zone}`
  expanded per zone) or tags. In each zone the launch takes the matching subnet
  with the most free addresses (cached 5 minutes); a zone without one is
  `ZoneUnusable` (`NoSubnetInZone`), and a `{zone}` pattern with no match in a zone
  gates only that zone. Without `use`, the default VPC's default subnets are used
  as found and never modified. The **managed network** is one resource, the VPC
  (`10.240.0.0/16`, tagged with the ownership marker), with a `/20` public subnet
  per usable zone, an attached internet gateway and a route table with
  `0.0.0.0/0` to it associated with every subnet. Each part carries the marker from
  its creating call; `Ensure` completes whatever a creation interrupted. Deleting
  it removes subnets, route table, gateway (detached first) and the VPC, only parts
  that carry the marker; anything else in the VPC (an instance, a group, a subnet
  someone added) is left alone and makes the deletion `Retryable`. A gateway whose
  attach failed is found again by name and marker and attached on the next call; the
  route table is created without a `ClientToken` (EC2 would replay the answer for a
  table deleted since, after a half-finished deletion), so the network can be
  recreated; the default route is checked on every call: a missing one is added, one
  to another gateway is replaced; deleting also removes a gateway of the network that
  was never attached; a zone that appears later gets the first free `/20`.
- **Security groups** are created in the VPC of the launch's subnet with no
  inbound rule except `spec.networking.inbound`, all outbound (EC2's default),
  and: for `isolation: nodeclass` a rule that allows all traffic between members
  of the group; for `isolation: run` and a run key, a second group per run key with
  that rule. **`isolation: none` means nobody**: no node may reach another privately
  (it adds no rule, the strictest setting; the word does not mean "not isolated"),
  only `inbound` applies. **`isolation: run` without a run key** is one implicit run:
  the NodeClass's group gets the member rule, as for `nodeclass`, so that nodes of the
  `direct` template (VPC-routed CNI) reach each other. The
  rules are part of the group's identity (the engine hashes the parameters into the
  name): a changed rule set is a new group, the old one is collected by its
  lifecycle. Groups the NodeClass brings (`use`) must be in the subnet's VPC; if
  not, the zone is skipped.
- **Placement groups**: `create` per (region, profile, run key) with the strategy
  and `partitionCount` of the profile; or `use` a group by name, never modified.
- **Names and marker.** Managed resources are named
  `scarpenter-<cluster>-<kind>-<region>[-<hash>]` (at most 63 characters, by the
  engine) and carry the ownership marker as tags **in the creating call**
  (`TagSpecifications`): `scarpenter.sparecores.com/cluster`, `nodeclass`,
  `managed-by` and `resource-kind`. `Ensure` finds by name and marker (adoption
  after a restart); a name that exists without the marker is `Fatal`. `Delete`
  refuses anything that does not carry the cluster's marker.
- `InUse` asks `DescribeInstances` for instances in the VPC, using the group or
  in the placement group. EC2 does not say when a resource was last used, so an
  unused resource reports the **zero time** and the engine counts the idle time
  from its first sighting. `Delete` of a resource in use, or with dependencies, is
  `Retryable` (`DependencyViolation`, `InvalidPlacementGroup.InUse`,
  `InvalidGroup.InUse`).
- **API volume.** The status check resolves each `use` reference per (kind, region)
  every `--resource-check-interval` (shared through the engine's resolve cache);
  a full sweep lists the managed resources of every region every 10th garbage
  collection.

## Network addresses and egress

- **Public IPv4** is the default (`AssociatePublicIpAddress` on the primary
  interface), because nodes need egress and the default VPC and the managed network
  have an internet gateway and no NAT.
- **IPv6.** `publicIPv6: true` requests one address (`Ipv6AddressCount=1`).
  - The **managed network** has IPv6: an Amazon-provided `/56` on the VPC, a `/64` per
    subnet (the index of the subnet's `/20`, so a zone added later gets its own),
    and a `::/0` route to the internet gateway. Subnets do not assign addresses on
    their own (`AssignIpv6AddressOnCreation` stays off): the launch asks for them. A
    managed network from before this (IPv4 only) gets all of it at the next
    `Ensure`; while EC2 is still associating the block `Ensure` fails `Retryable`
    (`VpcIPv6Pending`). Deleting the subnets and the VPC releases the IPv6 parts.
  - The account's **default VPC** has no IPv6 until the user associates one. With
    `publicIPv6` a zone whose subnets (default or `use`) have no IPv6 CIDR block is
    unavailable, with the reason in the resource status (`ResourcesReady` reports the
    zone; a region where no zone qualifies is not ready), and a launch is refused for
    that zone before the call. Where both kinds of subnet exist in a zone the one with
    an IPv6 block is preferred.
  - Security group rules take IPv4 and IPv6 CIDRs (an `icmp` rule with an IPv6 CIDR
    is ICMPv6); the serving certificate approver compares IPv6 addresses by value.
- **No public IPv4** (`publicIPv4: false`) is refused (`VendorReady=False`, reason
  `ValidationFailed`, the message names the fix) unless `resources.subnets.use` brings
  subnets: with the default VPC or the managed network the nodes would have no egress
  for sysexts and images. With `use` it is allowed: whether the subnets have a NAT
  gateway or a proxy is the user's job.
- **IPv6-only** (`publicIPv4: false`, `publicIPv6: true`): `use` subnets with IPv6 only,
  and the sysexts need a mirror reachable over IPv6 (the Warning Event
  `IPv6OnlyNeedsMirror` otherwise).

## Tags

`TagKey(k)` is `scarpenter.sparecores.com/<key>` for the logical keys `cluster`,
`nodeclaim`, `nodepool`, `nodeclass`, `managed-by` (and `resource-kind` on
resources); these characters are valid in EC2 tag keys. The NodeClass's
`spec.tags` are added to instances, volumes, network interfaces and managed
resources; the logical tags and `Name` win over them.

## Quotas

EC2's vCPU quotas are **per class, per region**, separately for On-Demand and Spot
(`GlobalQuota=false`). HPC and High Memory have no Spot quota.

| Class | Families | On-demand code | Spot code |
|---|---|---|---|
| Standard | A, C, D, H, I, M, R, T, Z | `L-1216C47A` | `L-34B43A08` |
| F | F | `L-74FC7D96` | `L-88CF9481` |
| G and VT | G, VT | `L-DB2E81BA` | `L-3819A6DF` |
| Inf | Inf | `L-1945791B` | `L-B5D1601B` |
| P | P | `L-417A185B` | `L-7212CCBC` |
| X | X | `L-7295265B` | `L-E3A00192` |
| Trn | Trn | `L-2C3B7624` | `L-6B0D517C` |
| DL | DL | `L-6E869C2A` | `L-85EED4F7` |
| HPC | HPC | `L-F7808C92` | none |
| High Memory | U (`u-*`, `u7i-*`, `u7in-*`) | `L-43DA4232` | none |

A new account has 5 Standard vCPUs and 0 for every other class, so `QuotaZero` is
the expected state of GPU and accelerator types until an increase is granted.

- **Family rule** (family = the type name before the first dot): the prefixes
  `mac`, `hpc`, `dl`, `trn`, `inf`, `vt` are checked first, in this order, then
  the first letter: `g` G and VT, `f` F, `p` P, `x` X, `u` High Memory, `a c d h i m r t z`
  Standard. A family the rule does not know has unknown quota (reactive path), and
  a test over the full AWS list of the committed Keeper snapshot (200 families)
  fails until a new family is classified. `mac*` types are dropped from the
  catalog: they need Dedicated Hosts and run macOS, not Flatcar (the driver leaves
  them out of the launchable types).
- **Limits**: `GetServiceQuota` (`ec2`) for the 18 codes, effective values, every
  15 minutes (or when the tracker asks); a code the service does not know is looked
  up in `ListServiceQuotas`, and a bucket missing there is not reported (its types
  are unknown quota).
- **Usage** is computed over **every instance of the region**, not only
  SCarpenter's, per class:
  - On-Demand: instances in `pending`, `running` (verified) and, conservatively,
    `stopping` and `shutting-down`, without `InstanceLifecycle`; never `stopped`
    (verified). Spot: the same states with `InstanceLifecycle=spot`. Scheduled and
    capacity-block instances are not counted.
  - Spot requests count only while `open` **and without an instance**; a fulfilled
    request stays `active` for minutes after its instance ended, so counting
    requests next to instances would double-count and then over-count (verified).
  - Unused capacity of active On-Demand Capacity Reservations (available
    instances x vCPUs; from the documentation, not tested live); capacity blocks
    are not counted.
  - vCPUs are the type's `DefaultVCpus` (verified: custom CPU options do not
    reduce the use), read with `DescribeInstanceTypes` (an addition to the plan's
    call list, see the [IAM policy](#iam)) and kept for the life of the process.
  - The instances counted are returned as the refs of the in-flight ledger.
- **Launchable types**: `DescribeInstanceTypeOfferings` with
  `LocationType=availability-zone-id` (matches Keeper's zone IDs), paginated, read
  hourly by the tracker; reading them also reads the vCPUs of the offered types.
- `GlobalQuotas` is empty: EC2's vCPU quotas are all regional.
- Errors: `VcpuLimitExceeded` and `MaxSpotInstanceCountExceeded` are `QuotaExceeded`
  and trigger an immediate refresh of the region.

## Error mapping

Classification matches EC2 **error codes** (`smithy.APIError`), never message
text, with one documented exception: `InvalidParameterCombination` is
`Unsupported` only when its message names both the instance type and the zone of
the call (EC2 gives that error for a type that does not exist in a zone only as
text); otherwise it is `Fatal`. Anything not listed is `Fatal` (for example
`UnauthorizedOperation`, `InvalidAMIID.*`, `InvalidParameterValue`,
`AuthFailure.ServiceLinkedRoleCreationNotPermitted`, `OptInRequired`,
`InvalidUserData.Malformed`). Every code of the table has a test, and the table is
checked against a second, independent copy.

| Class | EC2 codes |
|---|---|
| `InsufficientCapacity` | `InsufficientInstanceCapacity`, `InsufficientHostCapacity`, `InsufficientReservedInstanceCapacity`, `InsufficientCapacity`, `SpotMaxPriceTooLow`, `UnfulfillableCapacity`, `ReservationCapacityExceeded`, `InsufficientAddressCapacity`, `InsufficientVolumeCapacity` |
| `Unsupported` | `Unsupported`; `InvalidParameterCombination` naming the type and the zone |
| `QuotaExceeded` | `VcpuLimitExceeded`, `MaxSpotInstanceCountExceeded`, `InstanceLimitExceeded`, `PendingVerification`, `VolumeLimitExceeded`, `MaxIOPSLimitExceeded`; the region-wide resource limits `VpcLimitExceeded`, `SubnetLimitExceeded`, `InternetGatewayLimitExceeded`, `RouteTableLimitExceeded`, `SecurityGroupLimitExceeded`, `RulesPerSecurityGroupLimitExceeded`, `PlacementGroupLimitExceeded`, `AddressLimitExceeded` |
| `ZoneUnusable` | `InsufficientFreeAddressesInSubnet` (the subnet's cached free count is dropped); `NoSubnetInZone` |
| `Retryable` | `RequestLimitExceeded`, `Throttling`, `ThrottlingException`, `ThrottledException`, `EC2ThrottledException`, `RequestThrottled`, `RequestThrottledException`, `TooManyRequestsException`, `PriorRequestNotComplete`, `InternalError`, `InternalFailure`, `ServiceUnavailable`, `Unavailable`, `RequestTimeout`, `RequestExpired`, HTTP 5xx, network errors, the per-call timeout and a canceled context; the dependency codes `DependencyViolation`, `InvalidPlacementGroup.InUse`, `InvalidGroup.InUse`, `IncorrectState`, `IncorrectInstanceState` |
| `NotFound` | `InvalidInstanceID.NotFound`, `InvalidInstanceID.Malformed`, `InvalidGroup.NotFound`, `InvalidGroupId.Malformed`, `InvalidVpcID.NotFound`, `InvalidSubnetID.NotFound`, `InvalidInternetGatewayID.NotFound`, `InvalidRouteTableID.NotFound`, `InvalidAssociationID.NotFound`, `InvalidPlacementGroup.Unknown`, `Gateway.NotAttached`, `NoSuchResourceException` |
| `Fatal` | everything else |

Cross-check (plan 08): against the AWS Karpenter provider's `pkg/errors/errors.go`
and sc-inspector's markers, both at the commits the plan pins. The provider groups
`MaxSpotInstanceCountExceeded`, `VcpuLimitExceeded` and `InsufficientFreeAddressesInSubnet`
with capacity errors; SCarpenter keeps quota and capacity apart because they are
refreshed differently (overview section 5.9), and adds the provider's
`UnfulfillableCapacity`, `ReservationCapacityExceeded` and `SpotMaxPriceTooLow` to
`InsufficientCapacity`. sc-inspector's region-fatal markers (`VpcLimitExceeded`,
`SubnetLimitExceeded`, `InternetGatewayLimitExceeded`, `RouteTableLimitExceeded`,
`SecurityGroupLimitExceeded`, `AddressLimitExceeded`) are region-wide limits and map
to `QuotaExceeded`. A spread placement group that is full is reported by EC2 as
`InsufficientInstanceCapacity` with a message; the driver counts the group's
members before the call instead.

## IAM

[`aws-iam-policy.json`](aws-iam-policy.json) is the policy for the controller's
role. Replace `CLUSTER_NAME` with `--cluster-name` and `123456789012` with the
account ID.

| Statement | Allows |
|---|---|
| `Describe` | the `Describe*` calls the driver uses (`DescribeInstances`, `DescribeInstanceTypes`, `DescribeInstanceTypeOfferings`, `DescribeImages`, `DescribeRegions`, `DescribeAvailabilityZones`, `DescribeSubnets`, `DescribeVpcs`, `DescribeSecurityGroups`, `DescribePlacementGroups`, `DescribeInternetGateways`, `DescribeRouteTables`, `DescribeSpotInstanceRequests`, `DescribeCapacityReservations`) |
| `QuotaLimits` | `servicequotas:GetServiceQuota`, `servicequotas:ListServiceQuotas` |
| `RunInstancesTaggedResources` | `ec2:RunInstances` on instance, volume, network interface and spot request only with `aws:RequestTag/scarpenter.sparecores.com/cluster=CLUSTER_NAME` |
| `RunInstancesUsedResources` | `ec2:RunInstances` on the image, snapshot, subnet, security group and placement group it uses |
| `TagAtCreation` | `ec2:CreateTags` only with `ec2:CreateAction` `RunInstances`, `CreateSecurityGroup` or `CreatePlacementGroup`, and the cluster tag |
| `TerminateInstances` | `ec2:TerminateInstances` only on instances with `aws:ResourceTag/scarpenter.sparecores.com/cluster=CLUSTER_NAME` |
| `CreateSecurityGroups`, `CreateSecurityGroupsInAVpc` | `ec2:CreateSecurityGroup` with the cluster tag, in any VPC |
| `ChangeSecurityGroups` | authorize and revoke ingress and delete, on groups with the cluster tag |
| `CreatePlacementGroups`, `DeletePlacementGroups` | create with the cluster tag, delete groups with it |
| `OptionalManagedNetwork*` | **optional**: VPC, subnet, internet gateway and route table creation, `ReplaceRoute` for a stale default route, `AssociateVpcCidrBlock` and `AssociateSubnetCidrBlock` for IPv6, and deletion, only with the cluster tag; needed only where a NodeClass uses (or defaults to) `create` for `subnets`, that is in regions without a default VPC |
| `OptionalCustomerKmsKey`, `OptionalCustomerKmsKeyGrant` | **optional**: `kms:Decrypt`, `DescribeKey`, `GenerateDataKeyWithoutPlaintext` and, for AWS resources only (`kms:GrantIsForAWSResource`), `kms:CreateGrant` on the customer key of `kmsKeyID`, only through EC2 (`kms:ViaService`). The resource is the key's ARN even when `kmsKeyID` is an alias; the default `aws/ebs` key needs none |
| `OptionalPassInstanceProfileRole` | **optional**: `iam:PassRole` for the role of `aws.instanceProfile`, only to `ec2.amazonaws.com`; remove when no instance profile is used |

`ec2:DescribeInstanceTypes` is the one action beyond the plan's list: the driver
needs the vCPUs of the types for the quota use and the type features for
`Supported`. Spot launches also need the account's service-linked role for Spot
(`AWSServiceRoleForEC2Spot`); creating it is an account setup step, not a
permission of this policy (a missing role fails with
`AuthFailure.ServiceLinkedRoleCreationNotPermitted`, `Fatal`).

## Deferred

Spot placement scores as an ordering signal, interruption handling (EventBridge
and SQS, or a node-side IMDS watcher), capacity reservations as a launch target,
EFA, dedicated hosts, multiple network interfaces, attaching existing volumes or
snapshots, Capacity Blocks and Dedicated Host quotas.

## What is not verified against AWS yet

Everything above runs against the in-memory fake only; plan 10 (L5, the workflow
`e2e-aws.yml`, see [the L5 lane](../how-to-guides/testing.md#the-l5-lane)) confirms
it live. Specifically:

- the exact `Flatcar-<channel>-<version>[-arm64]-hvm` AMI names (the public
  release lists carry AMI IDs, not names);
- the public IPv4 constant against a real bill (Keeper has no such price);
- the serving certificates of the `tailscale` template are approved against the
  Node's addresses (the tailnet IP cannot be verified without the Tailscale API);
  only `direct` is held to the instance's addresses;
- the real size of the user-data with an EKS cluster CA bundle (the budget is
  16 KiB, the default templates are about 5 KiB);
- that `stopping`/`shutting-down` instances count against the quota, and the
  unused capacity of On-Demand Capacity Reservations (from the documentation);
- `RunInstances` with `Encrypted=true` on the root volume of a Flatcar AMI whose
  snapshot is not encrypted, and the IAM conditions of the policy.
