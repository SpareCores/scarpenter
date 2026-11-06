# 08 — AWS driver

Depends on: 06 (independent of 07). Read `docs/plans/v1/CONVENTIONS.md`
first. Spec: overview §11 (all, including §11.8 quotas), §5.8, §5.9, §7, §9,
§15 Q5; 00a §6, §9, §11; ADR-013, ADR-018, ADR-020.

## Goal

The first production driver: `pkg/vendors/aws`, complete against an
in-memory EC2 fake and the conformance suite, plus the AWS reference docs and
IAM policy. No AWS calls in this plan (CONVENTIONS rule 8); real AWS is
plan 10.

## Scope

- **Client layer**: `aws-sdk-go-v2` (`config`, `ec2`); a narrow `ec2API`
  interface holding only the calls used (`DescribeRegions`,
  `DescribeAvailabilityZones`, `DescribeImages`, `DescribeSubnets`,
  `DescribeVpcs`, `DescribeSecurityGroups`, `CreateSecurityGroup`,
  `AuthorizeSecurityGroupIngress`, `RevokeSecurityGroupIngress`,
  `DeleteSecurityGroup`, `CreatePlacementGroup`, `DeletePlacementGroup`,
  `DescribePlacementGroups`, `CreateVpc`, `CreateSubnet`,
  `CreateInternetGateway`, `AttachInternetGateway`, `CreateRouteTable`,
  `CreateRoute`, `AssociateRouteTable` and their delete counterparts,
  `RunInstances`, `DescribeInstances` (paginated),
  `TerminateInstances`, `DescribeInstanceTypeOfferings`,
  `DescribeSpotInstanceRequests`, `DescribeCapacityReservations`) and a
  `serviceQuotasAPI` interface
  (`GetServiceQuota`, `ListServiceQuotas`); per-region clients created lazily; credentials from
  the default chain; adaptive retry mode with SDK defaults; a user agent
  `scarpenter/<version>`.
- **Driver** (overview §11.1–§11.4): regions from the home region, zones
  with the Keeper zone-ID mapping, `ResolveImage` (owner, name patterns for amd64
  and arm64, exactly one available match, root device name from the image),
  `CreateInstance` with every `RunInstances` field of §11.4 (`ClientToken`,
  network interface with public IP flag and ENA Express, block device
  mappings from the resolved storage settings, `Placement` from the
  placement profile, IMDSv2 hop limit, `terminate` shutdown behaviour, spot
  market options, tag specifications for instance/volume/ENI), `GetInstance`,
  `FindInstance` and `ListInstances` by tag filters over the given regions,
  `DeleteInstance` (NotFound and already-terminated are success),
  `TagKey` mapping to `scarpenter.sparecores.com/<key>`,
  `NodeClassValidator` for the `aws` block and the AWS profile blocks
  (selector syntax, instance profile name format, metadata options ranges,
  EBS types, sizes, IOPS and throughput against Keeper's storage rows and
  EBS limits).
- **Node settings** (`NodeOptions`, 00a §6): built-in defaults (gp3 64 GiB
  encrypted root; public IPv4 on); storage settings (root and data EBS:
  `volumeType`, `volumeSize`, `iops`, `throughput`, `encrypted`, `kmsKeyID`;
  instance store `localStorage`); placement profiles (`placementGroup`
  cluster/spread/partition with partition count, `tenancy: dedicated`);
  network profiles (ENA Express); `Supported` per instance type (cluster
  placement not on burstable types, ENA Express and instance store only where
  `DescribeInstanceTypes`-equivalent driver tables say so, partition counts);
  `PriceDelta` (EBS GB-month from Keeper storage prices, public IPv4 price;
  provisioned IOPS/throughput mark the price incomplete); device hints for
  the first-boot unit (NVMe serial = volume ID pattern + size).
- **Resources** (`ManagedResources`, overview §11.2, 00a §9): kinds
  `subnets` (with VPC, internet gateway and route table when created),
  `securityGroups` (per NodeClass and VPC; per run key for `isolation: run`)
  and `placementGroups` (per region, profile and run key); vendor defaults
  (default VPC and subnets used as found); `use` by IDs, name pattern or tags;
  `create` with deterministic names, ownership tags, adoption, dependency
  order on delete.
- **Quotas** (§11.8, verified facts — implement them as written):
  `QuotaReporter` with the family → class rule, the 18 quota codes, limits via
  `GetServiceQuota`, usage over all instances of the region (`pending`,
  `running`, and conservatively `stopping`/`shutting-down`; never `stopped`;
  on-demand = no `InstanceLifecycle`, spot = `InstanceLifecycle=spot`), spot
  requests counted only while `open` without an instance, unused On-Demand
  Capacity Reservation capacity, vCPUs from the type's `DefaultVCpus`
  (catalog; never `CpuOptions`), and the counted refs; `OfferingLister` from
  `DescribeInstanceTypeOfferings` by zone ID. Drop `mac*` types from the AWS
  catalog. A test asserts every AWS family in the committed snapshot maps
  under the rule (no unknowns expected; a new family must fail the test until
  classified). Fake-EC2 tests cover each counting rule, including a spot
  request still `active` minutes after its instance terminated.
- **Error mapping** (§11.5) from `smithy.APIError` codes; cross-check against
  the AWS provider's
  [`pkg/errors/errors.go`](https://github.com/aws/karpenter-provider-aws/blob/30c236cafacb3a23fb3a50370d09d71750b56b29/pkg/errors/errors.go)
  and sc-inspector's
  [markers](https://github.com/SpareCores/sc-inspector/blob/c583123c85b44c65bbf6fa0ec0b67418469e4abf/inspector/lib.py#L92-L138);
  every code in the table has a test.
- **Image hook**: the NodeClass controller's image resolution for AWS
  (regions in scope × arches of the catalog's types; lazy per region; status
  `images`; `ImagesReady`), using the driver's `ResolveImage`.
- **In-memory EC2 fake** (`pkg/vendors/aws/fake`): regions, AZs with IDs,
  default VPC and subnets, images (Flatcar names), instances with state
  transitions, tag filters, `ClientToken` idempotency, security groups,
  instances of other owners (for account-wide quota usage), spot requests,
  capacity reservations, placement groups, VPCs, subnets, internet gateways
  and route tables (with deletion dependencies), accounts without a default
  VPC in some regions, type offerings per zone ID, Service Quotas values, and error injection per
  call. Used by the conformance suite and the driver's
  own table tests (RunInstances input golden: every field asserted).
- **§15 Q5**: confirm the Flatcar AMI naming for arm64 and the LTS channel
  from public Flatcar documentation and the published AMI lists
  (`flatcar_production_ami_all.json` for both arches; public, no AWS call);
  encode it in tests.
- **Docs**: `docs/reference/aws-driver.md` (regions, zones, images, built-in
  defaults, storage/placement/network profile fields, resources and their
  defaults, network,
  tags, quotas and quota classes, error mapping, deferred features) and the IAM policy of §11.6 (with the optional statement for created VPC
  resources) as a
  JSON file under `docs/reference/aws-iam-policy.json` with the cluster name
  as a placeholder, validated as JSON in `make lint`.

## Carried over from the plan 04 review

- `IdempotentParameterMismatch` (a `ClientToken` replay) must resolve to the
  original instance (`DescribeInstances` by client token), per the driver
  contract's idempotency rules.
- Per-call timeouts on every vendor API call (review N4).
- Run the conformance suite against the full AWS type list of the snapshot,
  not a sample (review N7).

## Gates

CONVENTIONS gates 1–4 (L3 still green: the AWS driver must not change core
behaviour). `go test ./pkg/vendors/aws/...` coverage ≥ 80 % of statements
(report the number).
