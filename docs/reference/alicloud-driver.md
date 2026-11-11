# Alibaba Cloud driver

The `alicloud` driver (`pkg/vendors/alicloud`, design in plan 21) launches Flatcar
nodes on ECS: regions and zones of the account, the operator's own Flatcar images
(rung 3 of ADR-021), vendor-native node settings (system and data disks, public
bandwidth, deployment sets), managed resources (VPC with its vSwitches, security
groups, deployment sets), quota-aware placement from the vCPU limits of the account and
the stock of each zone. Every API call goes through the narrow `ECSAPI` and `VPCAPI`
interfaces, which the in-memory fake in `pkg/vendors/alicloud/fake` implements for the
tests and the conformance suite.

**Nothing here has run against Alibaba Cloud**: no cloud call was made to write it, and
step 0 of plan 21 (the spike) is not done. Every fact that could not be verified is marked
*unverified*, and the [not verified](#not-verified-against-alibaba-cloud-yet) list says
what the live L5 run must settle. Enable the driver with `--vendors alicloud` (or the chart's
`vendors.alicloud.enabled`).

## Table of Contents

- [What exists](#what-exists)
- [Images](#images)
- [Regions and zones](#regions-and-zones)
- [Launch](#launch)
- [Instances and idempotency](#instances-and-idempotency)
- [Node settings](#node-settings)
- [Resources and network](#resources-and-network)
- [The metadata service and hardening](#the-metadata-service-and-hardening)
- [Quotas](#quotas)
- [Billing](#billing)
- [Credentials and the RAM policy](#credentials-and-the-ram-policy)
- [Clients](#clients)
- [Names and states](#names-and-states)
- [Error mapping](#error-mapping)
- [The in-memory fake](#the-in-memory-fake)
- [Testing](#testing)
- [Differences from plan 21](#differences-from-plan-21)
- [Deferred](#deferred)
- [Not verified against Alibaba Cloud yet](#not-verified-against-alibaba-cloud-yet)

## What exists

| Part | Where |
|---|---|
| SDK interfaces `ECSAPI`, `VPCAPI`, the SDK adapters, the client factory | `api.go`, `api_client.go`, `client.go` |
| Client layer: lazy clients per region, bounded retry, per-call deadline | `cloud.go` |
| Error classification | `errors.go` |
| Provider ID, tag keys, host name, `ClientToken` | `names.go` |
| Catalog filter (families the driver never launches), state mapping | `catalog.go` |
| `vendors.Driver`, registration, caches | `driver.go` |
| Settings (API types), defaults, `RunInstances` request builder | `settings.go`, `launch.go`, `pkg/apis/v1alpha1/alicloud` |
| Instance operations (create, get, find, list, delete) | `instance.go` |
| Regions, zones, launchable types per zone | `region.go` |
| Image resolution and `ImageReporter` | `images.go` |
| The `use` selector of every kind (declarations, parameter cube, matcher fields) | `selectors.go` |
| Managed resources, VPC and vSwitches, groups, deployment sets | `resources.go`, `network.go`, `groups.go` |
| Node options (`Supported`, `PriceDelta`, `Prepare`, `ZoneGaps`), hardening | `options.go` |
| NodeClass validation | `validate.go` |
| Disk prices (Keeper) | `prices.go` |
| Instance type table, quota reader | `typetable.go`, `quota.go` |
| The L5 sweeper | `sweep/` |
| In-memory ECS and VPC | `fake/` |

## Images

Rung 3 of the [image ladder](../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10):
**the images are the operator's own**, one per region and architecture, in the account of the
credentials. Flatcar publishes no image for Alibaba Cloud; the Spare Cores images repository
(plan 22) builds an `aliyun` artifact (`flatcar-aliyun-<channel>-<version>-<arch>.qcow2`, with
`flatcar.oem.id=aliyun` baked into the OEM partition so that Ignition reads the ECS user data).
The pipeline the operator's tooling runs is: upload the image to an OSS bucket of the region,
`ImportImage` (architecture, `BootMode`, `Platform=Others Linux`, `Features.NvmeSupport=supported`,
`Features.ImdsSupport=v1`), then `CopyImage` to the other regions (the first use needs the RAM role
`AliyunECSImageImportDefaultRole`). Image import and copy are not done by SCarpenter.

What the driver needs of the images, and checks:

- **The name** `Flatcar-<channel>-<version>-<arch>` (`amd64` or `arm64`), or the tags
  `flatcar-channel` and `flatcar-version`. Without `resources.images` the driver finds the pinned
  release by this name among the images of `imageOwner` (default `self`: the account of the
  credentials), in state `Available`, for the architecture; exactly one must match
  (`ImageNotFound`, `ImageAmbiguous`).
- **`resources.images.use`** (explicit IDs per region, a name pattern, tags, `match`, native
  `filters`) replaces the convention; the [selector reference](resource-selectors.md#alicloud) lists
  what ECS evaluates and what the client finishes. An account UID in `imageOwner` or `match.owner` is
  searched as `others` plus `ImageOwnerId` (shared images, and community images later, rung 2).
- **The release check** (ADR-017): whatever the lookup, the image must say it is the pinned release
  by its name or its tags; another release is `ImageVersionMismatch`, none `ImageVersionUnknown`, a
  name and tags that disagree `ImageVersionUnknown`. The image is then never booted.
- **The system disk size** of the image (its `DiskDeviceMappings`, else its size) must fit in the
  root disk (`storage.root.size`, at least the image size).
- **NVMe**: the 8th generation types and later need an image with `Features.NvmeSupport=supported`;
  a type that needs it with an image known not to have it is `Unsupported` before the call
  (`ImageNotSupportInstanceType`).
- **`ImdsSupport` must stay `v1`**, see [the metadata service](#the-metadata-service-and-hardening).

`status.vendors.alicloud.images` and the condition `ImagesReady` come from `ReportImages`: the
image per region and architecture, resolved at most once an hour (a miss is remembered for five
minutes), with the reason for a region that has none. Both architectures are reported.

## Regions and zones

`Regions` is `DescribeRegions` from the home region (`SCARPENTER_ALICLOUD_HOME_REGION`, chart
`vendors.alicloud.homeRegion`, default `ap-southeast-1`), cached for an hour; the catalog
intersects it with Keeper's regions, so regions Keeper does not list are never used. Use
international regions: the China regions need a China-site, real-name-verified account, and the
driver has no code branch for them. `Zones` is `DescribeZones`, cached for an hour; the zone ID and
the zone name are the same text (`ap-southeast-1a`), so Keeper's zone IDs match without mapping. A
region the credentials cannot use is `RegionUnavailable`.

`LaunchableTypes` is `DescribeAvailableResource` per region for on-demand and for spot (a type with
stock for either is offered; the launch error settles the rest), refreshed by the quota tracker, minus the
families the driver never launches. Reading it fills the instance type table (vCPUs, architecture, NVMe,
disk quantity) from `DescribeInstanceTypes`.


## Node settings

The `alicloud` block of `spec.vendors` and of the storage, network and placement profiles
([API reference](api.md#specvendorsalicloud); field names follow the ECS API). Built-in defaults
(layer 1): a `cloud_essd` PL0 system disk of 64 GiB, **not encrypted**, a public IPv4 of up to 100 Mbit/s
`PayByTraffic`, `HttpTokens` `optional`.

| Setting | Meaning |
|---|---|
| `storage.root` | system disk: `category` (`cloud_essd`, `cloud_auto`, `cloud_essd_entry`, `cloud_efficiency`, `cloud_ssd`), `size`, `performanceLevel` (ESSD, PL0 to PL3), `provisionedIops` and `burstingEnabled` (`cloud_auto`), `encrypted`, `kmsKeyID` |
| `storage.data[]` | extra disks created with the instance (all deleted with it), plus `name`, `filesystem`, `mountPath` for the first-boot unit; at most 16 |
| `internetMaxBandwidthOut`, `internetChargeType` | the public IPv4 is a bandwidth: 0 means none, the same as `networking.publicIPv4: false`; 100 Mbit/s is the ceiling of pay-by-traffic |
| `metadataOptions.httpTokens` | `optional` only: `required` is refused |
| `resourceGroupId` | the enterprise resource group of created resources |
| placement profile | `deploymentSet: {strategy: LowLatency \| Availability}` creates a set per run key, or `{use: {name}}` joins an existing one |

Decisions that differ from AWS (each an open question of the plan, decided by default):

- **Encryption defaults to false**: ECS refuses an encrypted system disk in Zone D of Hong Kong and
  Zone A of Singapore. `encrypted: true` is allowed and then those zones are unavailable, reported
  by the NodeClass status through `ZoneGaps` and, per profile, by `Supported` (ADR-017: explicit, not
  silent).
- **Bandwidth 100 Mbit/s pay by traffic**: sc-runner's 5 Mbit/s caps benchmarks.
- **`Supported`** drops a storage profile for a type when `cloud_essd_entry` is asked for a family other than
  `ecs.u1` and `ecs.e`, when the disks exceed the type's `DiskQuantity` (the system disk counts: the conservative
  reading, *unverified*), when an encrypted system disk meets a zone that refuses it, and for every
  `localStorage` policy other than `ignore` (types with local disks are deferred). Which disk categories each zone
  sells, and which deployment strategies each family takes, are *unverified* and left to the launch errors
  (`CategoryNotSupported`, `InvalidDiskCategory.NotSupported` are `Unsupported`, 24 h).
- **`PriceDelta`** is the disks at Keeper's GB-month price divided by 730, per region and category (and level for
  ESSD: a row without a level serves PL1 only); incomplete for `cloud_auto`, a level Keeper has no row for, any
  provisioned IOPS or bursting, and `PayByBandwidth`. The public IP is free and traffic is not priced. Keeper's
  storage IDs for Alibaba Cloud were not read from the live API (*unverified*).
- **Device hints**: disks exist at launch; the hint is the serial pattern `*` plus the size (the disk ID, the serial of a
  virtio disk without `d-`, is unknown before the launch), and the first-boot unit's size match and lock separate equal
  sizes. The NVMe serial pattern is *unverified*.
- **Tags**: `Tag.N` carries the logical tags (`scarpenter.sparecores.com/<key>`) and `spec.tags` on the instance, its
  disks and its primary ENI; ECS takes at most 20, so `spec.tags` is limited to 15.

## Resources and network

| Kind | `use` | Without `use` (ADR-022) | Created by SCarpenter |
|---|---|---|---|
| `subnets` | vSwitches by ID, name (`{zone}`), tags, `match` (zone, network, cidr, within, state, creation time), filters | tagged vSwitches, else the default VPC's default vSwitches (never modified), else create | one VPC (`10.240.0.0/16`) with a `/20` vSwitch per zone, `idleTTL` 24 h. No gateway or route table: the public IPv4 is a bandwidth |
| `securityGroups` | groups by ID, name, tags, `match.network`, filters | tagged groups, else create | one normal group per (NodeClass, VPC): inbound only `spec.networking.inbound`, a self rule for `isolation: nodeclass`, outbound open by default; one more per run key for `isolation: run` (`runSecurityGroups`, `perUse`) |
| `deploymentSets` | sets by ID or name (no tags) | none | one per (profile, run key), `perUse` |
| `images` | see [Images](#images) | the name convention | never (rung 3) |

Details that matter:

- **A group must be in the VPC of the vSwitch** (`InvalidSecurityGroup.VpcMismatch`): a brought group in another
  VPC skips the zone (`ResourceMissingInZone`). A vSwitch holds its addresses minus 4 reserved (*unverified* count):
  a /25 allows about 120 nodes per zone, then `QuotaExceeded.PrivateIpAddress` marks the zone unusable for ten minutes.
- **Markers.** `CreateVpc`, `CreateVSwitch` and `CreateSecurityGroup` take `Tag.N`, so the marker is atomic with the creation.
  `CreateDeploymentSet` takes no tags and `TagResources` has no deployment set type: the marker of a managed set is its
  **description** (`scarpenter.sparecores.com: cluster=... nodeclass=... managed-by=scarpenter resource-kind=deploymentSets`), and
  a set without it is never deleted. A resource whose name matches without the marker is never adopted or deleted.
- **Creation is asynchronous**: `Ensure` waits (up to 90 s) for a new VPC and its vSwitches to be `Available` and
  otherwise returns `Retryable` (`NetworkPending`); the next call continues. An interrupted creation is completed by
  the next `Ensure`.
- **Delete order**: deployment sets, run groups, NodeClass groups, then the VPC (its marked vSwitches and any group of
  ours left in it first). `InUse` asks `DescribeInstances` by VPC or group, and `DescribeDeploymentSets` for a set (the
  `DeploymentSetId` filter of `DescribeInstances` does not exist). Dependency errors (`DependencyViolation.*`) are `Retryable`.
- **A launch nobody prepared** (callers outside the resource engine, the conformance suite) uses the default VPC's
  default vSwitch of the zone and a normal group of its VPC, creating one marked `scarpenter-<cluster>-nodes-<vpc>` when
  there is none: ECS has no default group to fall back on.
- **Nodes without a public IPv4** are refused unless the NodeClass brings its own vSwitches (the default VPC and the managed
  network have no NAT); `publicIPv6` is refused (IPv6 needs a gateway the driver does not manage).

## The metadata service and hardening

Ignition's `aliyun` provider reads the user data from `100.100.100.200` with a plain GET. ECS's hardened mode
(`HttpTokens=required`) stops that with a 403, so `HttpTokens` stays `optional` at boot and the image's
`ImdsSupport` stays `v1`; `required` is refused by the validation. AWS's hop limit of 1 has no equivalent
(`HttpPutResponseHopLimit` is documented as unavailable on the RunInstances page: *unverified*). A Pod without host
networking could therefore read the user data (the one-hour bootstrap token, the Tailscale key with the `tailscale`
template) and the metadata (no RAM role is attached, so no credentials).

The driver offers the **`metadata-from-pods` hardening** ([bootstrap hardenings](node-bootstrap.md)): a boot unit that drops
forwarded traffic to `100.100.100.200`. It is off by default; enable it with `spec.bootstrap.hardening: [metadata-from-pods]`.
The L5 `imds` scenario records what a Pod can reach with and without it.

## Quotas

`DescribeAccountAttributes` gives limit and usage in one call per region: the buckets `postpaid-vcpu` and `spot-vcpu` (unit
vCPU, scope region). An instance consumes its vCPUs from the bucket of its capacity type; types the driver has not read
are unknown quota (reactive). The instances that count for the in-flight ledger come from a region-wide `DescribeInstances`
(pending, starting, running, stopping, stopped: whether stopped instances count is *unverified*). **Quota Center** per-family limits
(`ListProductQuotas`) are not read: plan 21 adds them only if the live check shows the account attributes are not the binding limit.
Stock is an extra signal: a type sold out in a zone is not offered (5-minute refresh, the effect is to be reported by the L5 run).
`OperationDenied.NoStock` and `Zone.NotOnSale` at launch are `InsufficientCapacity`. ECS accepts the launch of an instance
and may release it for lack of stock after (`SystemFailure.Delete`): the first `GetInstance` of an instance the process
launched that is gone within 15 minutes says `InsufficientCapacity` (code `InstanceVanished`); the core does not consume this
class from `GetInstance` yet (see the core gap in the plan outcome).

## Billing

Pay-as-you-go instances are billed per second and a short-lived instance pays a minimum of 10, 5 or 2 minutes by vCPU count
(the vendor's pay-as-you-go page, 2026-11-10; which cycle the minimum applies to is *unverified*). `BillingStart` is the
`CreationTime` of the instance (minute precision, so never later than the billed start; *unverified* that it is the instant billing starts).
`BillingRule` reports the per-second increment and **no minimum, not verified**: billing retention (ADR-024) stays off and the NodeClass
status says why. A rule per region and family cannot express a minimum that depends on the vCPU count, which the core would have to
carry before retention can hold Alibaba nodes; the live check of plan 21 settles the cycle.

## Credentials and the RAM policy

The controller authenticates through the default chain of `credentials-go`: `ALIBABA_CLOUD_ACCESS_KEY_ID`/`_SECRET`, the RAM role of
an OIDC identity provider (`ALIBABA_CLOUD_OIDC_PROVIDER_ARN`, `ALIBABA_CLOUD_ROLE_ARN`, `ALIBABA_CLOUD_OIDC_TOKEN_FILE`: the projected
ServiceAccount token, the analogue of AWS web identity; recommended), the shared credentials file, the instance RAM role and a
credentials URI. [Set up Alibaba Cloud credentials](../how-to-guides/alicloud-credentials.md) covers the chart's three modes
(`oidc`, `secret`, `none`). [`alicloud-ram-policy.json`](alicloud-ram-policy.json) is the policy: the `ecs:` and `vpc:` actions of the
two API interfaces, reads for everything, creates under the request tag and mutations under the resource tag
`scarpenter.sparecores.com/cluster` (a test ties the file to the interfaces; the condition keys' exact use with `RunInstances` is *unverified*).
Deployment sets take no tags, so their create and delete are limited to the resource type only; the driver deletes only sets whose
description carries its marker.
The nodes get no RAM role. The home region is `SCARPENTER_ALICLOUD_HOME_REGION` (an environment variable, not a flag: no controller
flag was added).

## Clients

The driver calls the generated SDKs (`ecs-20140526/v7`, `vpc-20160428/v7`) only
through `ECSAPI` and `VPCAPI`. The generated clients take no context and return a
response wrapper; the adapters call the `...WithContext` variants and return the
response body. Credentials come from the default chain of
`github.com/aliyun/credentials-go` (access key variables, the OIDC role of
`ALIBABA_CLOUD_OIDC_PROVIDER_ARN`, `ALIBABA_CLOUD_ROLE_ARN` and
`ALIBABA_CLOUD_OIDC_TOKEN_FILE`, the shared file, the instance RAM role, a
credentials URI). The endpoint is `ecs.<region>.aliyuncs.com` and
`vpc.<region>.aliyuncs.com` unless overridden; the user agent is
`scarpenter/<version>`.

Every call has a deadline (45 s, 90 s for `RunInstances`) and is retried at most
four times with a doubling backoff on `Throttling*`, `ServiceUnavailable`,
`InternalError` and 5xx answers. Errors are classified once, in the call
wrapper, so no raw SDK error leaves the layer.

## Launch

`BuildRunInstances` builds the one call of a launch: `Amount=MinAmount=1`,
`InstanceChargeType=PostPaid`, the instance name (the NodeClaim, 2 to 128
characters) and host name (shortened with a hash above 64), Base64 user data
(16 KiB budget), `SystemDisk` and `DataDisk.N` (`DeleteWithInstance=true`), the
public IPv4 as `InternetMaxBandwidthOut` (0 without), the metadata options
(`HttpTokens` stays `optional`: Ignition's `aliyun` provider reads the user data
without a token), the vSwitch, the security groups, the optional deployment set
and resource group, and, for spot, `SpotAsPriceGo`, `SpotDuration=0` and
`Terminate`. Defaults: root disk `cloud_essd` PL0, 64 GiB, not encrypted;
bandwidth 100 Mbit/s, `PayByTraffic`. `Tag.N` carries the NodeClass tags and the
logical tags under `scarpenter.sparecores.com/<key>`, at most 20 in total (ECS
applies one parameter to the instance, its disks and its primary ENI). `publicIPv6`
is refused. The request is golden-tested for the defaults and for every option.

## Instances and idempotency

ECS has a `ClientToken` and no filter on it. Create therefore:

1. looks the NodeClaim up by its tag in the region and returns that instance;
2. otherwise calls `RunInstances` with the token, the SHA-256 of the
   idempotency key, region, zone, type, capacity type, image and vSwitch;
3. on `IdempotentParameterMismatch` looks the NodeClaim up again and returns it,
   and when there is none repeats the call once with a token salted by an attempt
   counter.

`CheckLaunched` turns an instance that is gone within the launch timeout of its
creation into `InsufficientCapacity` (code `InstanceVanished`): ECS accepts the
call while the instance is `Pending` and may release it for lack of stock
(`SystemFailure.Delete`). Delete reads the instance, refuses one without this
cluster's tag, and releases it with `Force`; a missing instance is success.

## Names and states

- Provider ID `scarpenter://alicloud/<region>/<nodeclaim>`, instance reference
  `<region>/i-...`.
- `Pending` and `Starting` are `pending`, `Running` is `running`, `Stopping` and
  `Stopped` are `terminating` (a stopped node is collected, plan 14 A4), an
  unknown status is `pending`. A released instance is not returned by ECS.
- Regions are `DescribeRegions` read from the home region (default
  `ap-southeast-1`), intersected with Keeper's active regions; zone ID and name
  are the zone name.
- Families the driver never launches: bare metal `ecs.ebm*`, `ecs.scc*`, the
  test families `ecs.poc-test*`, `ecs.e-test*`, `ecs.vfx*`, `ecs.hbm*`, and the
  classic-network-only `ecs.c1` and `ecs.s1` (the list of legacy families is not
  verified). Every family of the committed Keeper snapshot has its verdict in
  `pkg/vendors/alicloud/testdata/families.golden`; a family the snapshot gains
  fails the test until it is classified.

## Error mapping

By `Code`, never by text. The table is in `errors.go` and a second, independent
copy in `errors_test.go`; every code is tested.

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `OperationDenied.NoStock`, `Zone.NotOnSale`, `InvalidOperation.PublicIpAddressNoStock`, `QuotaExceed.DeploymentSetInstanceQuotaFull`, `LackResource` |
| `Unsupported` | `InvalidInstanceType.ZoneNotSupported`, `InstanceType.Offline`, `InvalidInstanceType.ValueNotSupported`, `ImageNotSupportInstanceType`, `InvalidDiskCategory.NotSupported`, `CategoryNotSupported`, `InvalidInstanceType.NotSupportDiskCategory`, `OperationDenied.CloudSSDNotSupported`, `OperationDenied.ZoneSystemCategoryNotMatch`, `InvalidAccount.NotSupportSpot` |
| `QuotaExceeded` | `QuotaExceed.ElasticQuota`, `QuotaExceed.AfterpayInstance`, `QuotaExceeded`, `QuotaExceed.PostPaidDisk`, `QuotaExceeded.PostpaidDataDiskCapacity`, `QuotaExceeded.InternetBandwidth`, `InstanceDiskNumLimitExceed`, `InstanceDiskLimitExceeded` |
| `ZoneUnusable` | `QuotaExceeded.PrivateIpAddress`, `OperationDenied.ZoneNotAllowed`, `Zone.NotOpen`, `SecurityGroupInstanceLimitExceed` |
| `RegionUnavailable` (zone reads) | `RegionUnauthorized`, `InvalidRegion.NotSupport`, `RealNameAuthenticationError` |
| `Retryable` | `Throttling`, `Throttling.*`, HTTP 429, `ServiceUnavailable`, `InternalError`, HTTP 5xx, `IdempotentProcessing`, `IncorrectInstanceStatus`, `DependencyViolation.*` |
| `NotFound` | `InvalidInstanceId.NotFound` |
| `Fatal` | everything else, for example `InvalidImageId.NotFound`, `InvalidVSwitchId.NotFound`, `InvalidSecurityGroup.VpcMismatch`, `Account.Arrearage`, `Forbidden.RAM`, `QuotaExceed.Tags`, `NumberExceed.Tags`; `IdempotentParameterMismatch` is resolved by the create path and never classified |

## The in-memory fake

`pkg/vendors/alicloud/fake` implements `ECSAPI` and `VPCAPI` for a whole account:
regions (denied ones), zones, instance types with per-zone, per-capacity-type
stock, vCPU quotas, images of several owners, instances that are `Pending` and
then `Running` (by a clock), stopped and released instances, instances that vanish
after acceptance, `ClientToken` replays and mismatches (with the option of a
failed call consuming its token), tags with AND filters, paging with the APIs'
default page sizes, default and created VPCs, vSwitches with free-address counts,
security groups with rules and a size limit, deployment sets with a size limit,
account attributes, instances of other owners, and error injection per call.
Codes the plan marks unverified (the dependency codes of deletes, the duplicate
rule answer) are guesses there and are commented as such.

## Differences from plan 21

Found on the SDK and its types, recorded here for the plan:

- The metadata options are request fields (`HttpTokens`, `HttpEndpoint`,
  `HttpPutResponseHopLimit`), not `SecurityOptions`; `SecurityOptions` only has
  `ConfidentialComputingMode`, `TrustedSystemMode` and `EnableSecureBoot`.
- `CreateVpc` and `CreateVSwitch` take `Tag.N`, so the ownership marker is atomic
  there (the plan listed it as unverified). `CreateSecurityGroup` takes it too.
  `CreateDeploymentSet` takes no tags and `TagResources` has no deployment set
  resource type: the marker of a managed deployment set can only be its name (and
  description).
- The generated SDKs return `*dara.SDKError` (`github.com/alibabacloud-go/tea/dara`)
  and, converted, `*tea.SDKError`; the plan says `tea.SDKError`. Both are read.
- `DescribeInstanceTypes` takes 1 to 10 names per call and has no vCPU field
  other than `CpuCoreCount`; `InstanceIds` of `DescribeInstances` is a JSON
  array in a string.
- Type names are not always `ecs.<family>.<size>`: Keeper has `ecs.u0.c1m1.large`
  and `ecs.ebmgn9ge-1.5t.64xlarge`, so the family is the second segment. A prefix
  rule for `c1` would also take `c10a`: the legacy families match exactly.
- `QuotaExceed.Tags` and `NumberExceed.Tags` are `QuotaExceeded` in the plan; the
  driver classifies them `Fatal`: more than 20 tags is a configuration error, and
  `QuotaExceeded` would stop every offering of the region for it. The launch
  validation refuses more than 20 tags before the call, so neither should be seen.
- The home region is an environment variable (`SCARPENTER_ALICLOUD_HOME_REGION`), not the planned flag `--alicloud-home-region`:
  the options live in the core (`pkg/operator/options`), which a vendor change does not touch.
- `BillingRule` carries no minimum: the interface is per region and family and the minimum depends on the vCPU count.
- A deployment set is marked by its description (it takes no tags); `DescribeInstances` has no `DeploymentSetId` filter, so `InUse`
  of a set reads the set's instance count.
- The storage block has a `localStorage` field that no type supports, so that a profile written for another vendor is refused
  instead of pruned silently.


## Testing

`go test ./pkg/vendors/alicloud/...` runs the shared conformance suite (`pkg/vendors/conformance`) once per resource kind over the full
type list of the committed Keeper snapshot (1998 types), the golden of the `RunInstances` request, every error code of the table
above, the family classification of the snapshot, the adapters against a local HTTP server, the sweeper against the fake, and the
published examples, selector reference and RAM policy against the declarations. Statement coverage of `pkg/vendors/alicloud` is
above 80 %. The opt-in L5 lane is `test/e2e-alicloud/` (`make test-e2e-alicloud-dryrun` validates it without a cloud), the nightly canary
is `test/canary/alicloud.go`, the sweeper is `pkg/vendors/alicloud/sweep` and `test/e2e-alicloud/sweeper`
([testing](../how-to-guides/testing.md#the-l5-lane)).

## Deferred

IPv6 (needs an IPv6 gateway), local-storage profiles (the `i` and `d` families), partition placement, dedicated hosts and capacity
reservations, multiple ENIs, eRDMA, EIPs and NAT gateways, RAM instance roles, interruption handling, `ebm` and `scc` types, `DryRun`
pre-flights, Quota Center, the China regions in the lane, and rung 2 (community images published by Spare Cores).

## Not verified against Alibaba Cloud yet

The live L5 run, and step 0 of plan 21, must settle: that a Flatcar image with `flatcar.oem.id=aliyun` boots and Ignition reads the user data
(`PLATFORM_ID=aliyun`), the console, BIOS or UEFI, DHCP, the system disk growth, NVMe and virtio disk serials; RAW versus QCOW2 import and
`CopyImage`; whether a failed `RunInstances` consumes its `ClientToken`; the tag filter semantics and their consistency right after a
launch; whether `DescribeInstances` ever lists released instances and how fast a stock shortage releases one; whether `DescribeAccountAttributes`
is the binding quota, and the defaults of a new account; whether stopped instances count; the billing minimum, its cycle and the billing start;
the codes of dependency and duplicate errors; Keeper's storage IDs; the RAM condition keys and the OIDC role path from a self-managed
cluster; the rate limits; `DiskQuantity` semantics; the strategy limits of deployment sets and whether they take spot instances; the
address count a vSwitch reserves.

