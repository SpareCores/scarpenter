# GCP driver (Google Compute Engine)

Status: **lower layers only**. The driver is not registered and does not implement
`vendors.Driver` yet; there is no `spec.vendors.gcp` block, no NodeClass
field, no conformance wiring and no live lane. What exists is the package
`pkg/vendors/gcp`, which the driver will be built on, and the plan it follows
([plan 16](../plans/v1/wip/16-gcp.md)). Everything below is tested against the
in-memory Compute API only; nothing has called Google yet, and every code or
behaviour marked **unverified** is to be confirmed by the plan's spike or the
first live run.

## What exists

| Part | File | What it does |
|---|---|---|
| Compute API wrapper | `api.go`, `sdk.go` | `ComputeAPI`: the calls the plan needs (instances, zones, regions, machine types, images, subnetworks, networks, firewalls, resource policies, operations, projects) with the `computepb` messages. `SDK` adapts the generated REST clients of `cloud.google.com/go/compute/apiv1`: operations become their `computepb.Operation` message, iterators become one page. |
| In-memory fake | `fake/` | `fake.Compute` implements `ComputeAPI`. `fake.NewSeeded` gives two regions, three zones with numeric IDs, four machine types (one arm64), Flatcar images, quota limits and a custom VPC. |
| Error classification | `errors.go` | Operation error codes and HTTP reasons to the contract's classes; `QUOTA_EXCEEDED` is account-wide for `*_ALL_REGIONS` metrics. |
| Quota reader | `quota.go` | `QuotaReader`: region buckets from `regions.get`, project buckets from `projects.get`, and `Consumption` for confirmed series only. |
| Call deadlines | `timeouts.go` | `WithTimeouts`: every call under its own deadline (45 s; 120 s for an operation wait). The generated clients retry reads on HTTP 503 and 504 until the context ends (600 s without a deadline) and do not retry 429 or mutating calls; a call that runs into its deadline is `Retryable` (`CallTimeout`). |
| Mapping | `names.go`, `state.go` | Provider ID, instance reference, instance names, labels, ownership marker for resources without labels, request IDs, state mapping. |
| Launch request | `launch.go` | `BuildInsert`: the `instances.insert` request from a resolved `LaunchSpec`. |

## Provider ID and instance reference

- Provider ID: `scarpenter://gcp/<region>/<nodeclaim>`.
- `InstanceRef`: `<region>/<zone>/<instance name>`. The zone is part of the ID because
  every instance call needs it. A malformed reference (including the conformance
  suite's `<region>/i-never-existed`) does not parse and is answered with `NotFound`.
- The instance name is the NodeClaim name when it is a valid RFC 1035 name (at most 63
  characters), else `scarpenter-<hash>`.

## Launch request

`BuildInsert` builds, without calling the API:

- `machineType` `zones/<zone>/machineTypes/<type>`.
- Boot disk with `autoDelete`, the image, the disk type, size and labels; Hyperdisk
  IOPS and throughput when set; a customer-managed key when set.
- Data volumes with `autoDelete: true` (not the default), `deviceName` = the volume name
  (so the guest path `/dev/disk/by-id/google-<name>` is known before the call),
  `diskName` `<instance>-<name>`, the interface (`NVME` or `SCSI`).
- One network interface: the subnet, `IPV4_ONLY` or `IPV4_IPV6` with an external IPv6
  access config, one `ONE_TO_ONE_NAT` unless the public IPv4 is off, NIC type and the
  Tier 1 bandwidth when asked.
- Network tags (the firewall's tag and the run tag), labels, resource policies.
- On-demand: `provisioningModel: STANDARD`, `automaticRestart: false`,
  `onHostMaintenance` `MIGRATE` or `TERMINATE`. Spot: `provisioningModel: SPOT`,
  `instanceTerminationAction: DELETE` (the default `STOP` would leave a stopped instance
  and its disks), `onHostMaintenance: TERMINATE`.
- Metadata: `user-data` (the Ignition), `block-project-ssh-keys: TRUE`,
  `enable-oslogin: FALSE`, plus the block's own keys. The reserved keys
  (`user-data`, `ssh-keys`, `enable-oslogin`, `block-project-ssh-keys`) are refused in the
  extra metadata.
- A service account only when one is configured.
- `requestId`: UUIDv5 of the NodeClaim UID, zone, machine type and capacity type, so only a
  retry of the same offering shares an ID; no `requestId` without an idempotency key.

## Labels

`TagKey(k)` is `scarpenter-<k>`. Label values go through one deterministic `LabelValue()`:
valid values (lower-case ASCII letters, digits, `_`, `-`, up to 63 characters) are unchanged,
others are lower-cased, replaced and given a short hash. Writing and filtering use the same
function. NodeClass tags are validated against the label charset (no `scarpenter-` prefix, at
most 40 characters). Networks, subnetworks, firewall rules and resource policies have no
labels: they are marked by the name prefix `scarpenter-<cluster>-` plus a description
`scarpenter: cluster=<c> nodeclass=<n> managed-by=scarpenter resource-kind=<kind>`.

## Instance states

`PROVISIONING` and `STAGING` are pending; `RUNNING` and `REPAIRING` running; `STOPPING`,
`STOPPED`, `TERMINATED`, `SUSPENDING` and `SUSPENDED` terminating (a stopped instance is
not coming back and its disks still cost, so the garbage collector deletes it); an unknown
status is running.

## Error mapping

Asynchronous errors are in the finished operation (`error.errors[].code`), synchronous ones
are `*googleapi.Error` with a status and a reason. Both go through the one table in
`errors.go`, covered entry by entry by `errors_test.go`:

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `ZONE_RESOURCE_POOL_EXHAUSTED`, `ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS`, `REGION_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS`, `POOL_CAPACITY_INSUFFICIENT` |
| `QuotaExceeded` | `QUOTA_EXCEEDED` (the metric is read from `Quota '<m>' exceeded`); account-wide when the metric ends in `_ALL_REGIONS` |
| `Unsupported` | `UNSUPPORTED_OPERATION`; a 404 that names the machine type of the call |
| `ZoneUnusable` | `IP_SPACE_EXHAUSTED`, `IP_SPACE_EXHAUSTED_WITH_DETAILS` |
| `Retryable` | HTTP 429 and 5xx, `RESOURCE_OPERATION_RATE_EXCEEDED`, `INTERNAL_ERROR`, `backendError`, `rateLimitExceeded`, `resourceInUseByAnotherResource` (clears when the users are gone), the driver's own timeout |
| `NotFound` | 404 |
| `RegionUnavailable` | 401 and 403 on the calls that read a region's facts |
| `Fatal` | `ALREADY_EXISTS` (the caller looks the instance up by its idempotency label), `CONDITION_NOT_MET` (reported with the code `OrganizationPolicyViolation`), permission denied, everything else |

## Quotas

`QuotaReader.Quotas` returns the CPU buckets (`CPUS`, `<SERIES>_CPUS`, `PREEMPTIBLE_CPUS`)
and `INSTANCES` of a region from `regions.get`, with usage from the API; `GlobalQuotas`
returns `CPUS_ALL_REGIONS` from `projects.get` when the project has it.
`Consumption` returns buckets only for series listed in `QuotaReader.Confirmed`, which is
empty until the spike confirms which metrics one machine type consumes; every other type is
unknown and takes the reactive path. Spot is unknown for now (it depends on whether the
region has preemptible quota).

## The in-memory fake

`fake.Compute` follows what the plan relies on: unique instance names per zone (409
`alreadyExists`), `requestId` replay (returns the original operation, **unverified** for a
failed one), operations that finish on `Wait` or after a number of `Get` polls and carry
capacity, quota, organization-policy and IP exhaustion errors, instance states and
transitions (`PROVISIONING` to `RUNNING`, `STOPPING` until the delete finishes), quota usage
computed from the instances and foreign usage (the metrics an instance consumes are the
fake's **unverified** assumption), a subset of the list filter language (`=`, `!=`, `eq`,
`ne`, `AND`; `OR` is refused), paged lists, aggregated lists with an unreachable zone,
networks, subnetworks with CIDR overlap, firewall rules, resource policies with the in-use
refusal, a project without a `default` network, image lookup by name and family only (no
listing), and error injection per call.

## Not yet

The `vendors.Driver` implementation, registration, the `spec.vendors.gcp` block and the
NodeClass API types, image resolution through the `images` kind, resources, node options,
the IAM role, conformance, the L5 lane and the credentials how-to. Seams are marked
`TODO(plan 14)` in the code.

## What is not verified against GCP yet

Every error code and reason above; the quota metrics a machine type consumes and the
Spot rule; `requestId` replay for a failed operation; the Hyperdisk-only series list; the
device path names of NVMe data disks; whether `aggregatedList` tolerates a down zone (the
generated request in `cloud.google.com/go/compute` v1.75.0 has no `returnPartialSuccess`
field, so the driver cannot ask for it); the guest agent's handling of
`block-project-ssh-keys` and `enable-oslogin`.
