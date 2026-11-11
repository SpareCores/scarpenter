# Alibaba Cloud driver (in progress)

The lower layers of the `alicloud` driver (`pkg/vendors/alicloud`, design in
plan 21): the ECS and VPC clients behind narrow interfaces, an in-memory
fake of both, error classification, the pure mappings, the instance
operations and the quota reader. **The `vendors.Driver` itself is not there
yet**: nothing is registered, `--vendors` does not know `alicloud`, there is no
`alicloud` block in the NodeClass API and no conformance wiring. Those follow the
core changes of plan 14 (the unified `use` selector, the `images` kind, the
Butane hook). Nothing here has run against Alibaba Cloud: no cloud call was made
to write it.

## Table of Contents

- [What exists](#what-exists)
- [Clients](#clients)
- [Launch](#launch)
- [Instances and idempotency](#instances-and-idempotency)
- [Names and states](#names-and-states)
- [Quotas](#quotas)
- [Error mapping](#error-mapping)
- [The in-memory fake](#the-in-memory-fake)
- [Differences from plan 21](#differences-from-plan-21)
- [Not yet](#not-yet)

## What exists

| Part | Where |
|---|---|
| SDK interfaces `ECSAPI`, `VPCAPI`, the SDK adapters, the client factory | `api.go`, `api_client.go`, `client.go` |
| Client layer: lazy clients per region, bounded retry, per-call deadline | `cloud.go` |
| Error classification | `errors.go` |
| Provider ID, tag keys, host name, `ClientToken` | `names.go` |
| Catalog filter (families the driver never launches), state mapping | `catalog.go` |
| Settings types and defaults, `RunInstances` request builder | `settings.go`, `launch.go` |
| Instance operations (create, get, find, list, delete) | `instance.go` |
| Regions, zones, launchable types per zone | `region.go` |
| Instance type table, quota reader | `typetable.go`, `quota.go` |
| In-memory ECS and VPC | `fake/` |

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

## Quotas

`DescribeAccountAttributes` gives limit and usage in one call, per region:
buckets `postpaid-vcpu` and `spot-vcpu` (unit vCPU, scope region). An instance
consumes its vCPUs (`DescribeInstanceTypes` `CpuCoreCount`) from the bucket of its
capacity type. `Used` covers the whole account; the instances that count for the
in-flight ledger come from a region-wide `DescribeInstances` (pending, starting,
running, stopping and stopped). Whether these attributes are the binding limit, or
Quota Center per-family limits are needed, is open (plan 21 step 0). No code is
known to be account-wide: the vCPU and instance limits are per region.

## Error mapping

By `Code`, never by text. The table is in `errors.go` and a second, independent
copy in `errors_test.go`; every code is tested.

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `OperationDenied.NoStock`, `Zone.NotOnSale`, `InvalidOperation.PublicIpAddressNoStock`, `QuotaExceed.DeploymentSetInstanceQuotaFull`, `LackResource` |
| `Unsupported` | `InvalidInstanceType.ZoneNotSupported`, `InstanceType.Offline`, `InvalidInstanceType.ValueNotSupported`, `ImageNotSupportInstanceType`, `InvalidDiskCategory.NotSupported`, `CategoryNotSupported`, `InvalidInstanceType.NotSupportDiskCategory`, `OperationDenied.CloudSSDNotSupported`, `OperationDenied.ZoneSystemCategoryNotMatch`, `InvalidAccount.NotSupportSpot` |
| `QuotaExceeded` | `QuotaExceed.ElasticQuota`, `QuotaExceed.AfterpayInstance`, `QuotaExceeded`, `QuotaExceed.PostPaidDisk`, `QuotaExceeded.PostpaidDataDiskCapacity`, `QuotaExceeded.InternetBandwidth`, `InstanceDiskNumLimitExceed`, `InstanceDiskLimitExceeded`, `QuotaExceed.Tags` |
| `ZoneUnusable` | `QuotaExceeded.PrivateIpAddress`, `OperationDenied.ZoneNotAllowed`, `Zone.NotOpen`, `SecurityGroupInstanceLimitExceed` |
| `RegionUnavailable` (zone reads) | `RegionUnauthorized`, `InvalidRegion.NotSupport`, `RealNameAuthenticationError` |
| `Retryable` | `Throttling`, `Throttling.*`, HTTP 429, `ServiceUnavailable`, `InternalError`, HTTP 5xx, `IdempotentProcessing`, `IncorrectInstanceStatus`, `DependencyViolation.*` |
| `NotFound` | `InvalidInstanceId.NotFound` |
| `Fatal` | everything else, for example `InvalidImageId.NotFound`, `InvalidVSwitchId.NotFound`, `InvalidSecurityGroup.VpcMismatch`, `Account.Arrearage`, `Forbidden.RAM`; `IdempotentParameterMismatch` is resolved by the create path and never classified |

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
- `QuotaExceed.Tags` and `NumberExceed.Tags` are classified `QuotaExceeded` by the
  plan, which stops the whole offering for a hand-made configuration error;
  the launch validation refuses more than 20 tags before the call, so the class
  should never be seen.

## Not yet

Marked in the code with `TODO(plan 14)`:

- the `vendors.Driver` implementation, `Init`, `ResolveImage` and the registration;
- the `alicloud` block in `spec.vendors`, the storage, network and placement
  profiles in `pkg/apis`, and the decoding that replaces the local settings types;
- image resolution through the coming `images` kind, `ImageReporter`;
- `Prepare`, `ManagedResources` (vSwitches and their VPC, security groups,
  deployment sets), the vSwitch zone check, the placement profile;
- `NodeOptions` (`Supported`, `PriceDelta`, device hints), the Butane hook for the
  IMDS gap, conformance wiring, the L5 lane and the sweeper;
- the RAM policy, the credentials how-to, the chart's credential mode and the
  controller's `--alicloud-home-region` flag;
- Quota Center, billing period and retention, step 0 of the plan (the spike).
