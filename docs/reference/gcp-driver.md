# GCP driver (Google Compute Engine)

The driver in `pkg/vendors/gcp`, registered as `gcp` (`--vendors=gcp`, Helm `vendors.gcp.enabled`).
It launches instances with `instances.insert`, finds Flatcar's public GCE images by name, creates and
garbage-collects the networks, firewall rules and placement policies it owns, and reads quotas from
`regions.get`. Everything below is tested against the in-memory Compute API (`pkg/vendors/gcp/fake`)
and the shared conformance suite; nothing has called Google yet. What is **unverified** (error codes,
quota metrics, guest behaviour, the arm64 image) is listed at the end and is what the live L5 run
confirms ([plan 16](../plans/v1/wip/16-gcp.md)).

## Table of Contents

- [Capabilities](#capabilities)
- [Credentials and clients](#credentials-and-clients)
- [Regions and zones](#regions-and-zones)
- [Images](#images)
- [Launch](#launch)
- [Node settings](#node-settings)
- [Resources and network](#resources-and-network)
- [Network addresses and egress](#network-addresses-and-egress)
- [Labels and tags](#labels-and-tags)
- [Quotas](#quotas)
- [Error mapping](#error-mapping)
- [Metadata exposure](#metadata-exposure)
- [IAM](#iam)
- [Billing](#billing)
- [Interruption](#interruption)
- [Deferred](#deferred)
- [What is not verified against GCP yet](#what-is-not-verified-against-gcp-yet)

## Capabilities

`vendors.Driver` and `NodeClassValidator`, `ImageReporter`, `NodeOptions` (with `EpochReporter`),
`ManagedResources` (with `ResourceDefaults` and `DefaultChecker`), `ArchNamer`, `QuotaReporter`,
`OfferingLister`, `BillingDescriber` and `InstanceMetadata`. There is no `ZoneChecker`: subnetworks are
regional, so an IPv6 gap is the same in every zone and fails the launch's `Prepare` instead.

The Compute API is reached through the narrow `ComputeAPI` interface of `api.go` (the `computepb`
messages, mutating calls return the operation message, lists return one page). Every call runs under a
deadline: `WithTimeouts` gives 45 s to a call and 120 s to one operation wait, and the driver bounds
the whole wait for an insert (5 minutes). A call that runs into its deadline is `Retryable`
(`CallTimeout`).

## Credentials and clients

Application Default Credentials through the Google client libraries, with the user agent
`scarpenter/<version>`. The recommended setup for a cluster outside Google Cloud is workload identity
federation ([set up GCP](../how-to-guides/set-up-gcp.md#2-credentials)); a service account key and
the metadata server (GKE, GCE) work too. The clients are created with the first call: an unreachable
Google does not keep the controller from starting.

The controller targets **one project**: `--gcp-project` (`SCARPENTER_GCP_PROJECT`, Helm
`vendors.gcp.project`), else the project of the credentials (a service account key, the metadata
server). Workload identity federation carries no project: the flag is then required, and the
controller refuses to start without a project. The NodeClass field `vendors.gcp.project`, when set, must
equal it (the vendor's `VendorReady` is false otherwise, reason `ValidationFailed`, and a launch with
another project is `Fatal`), so a rendered manifest says which project it is for and a mismatch is a visible condition.

Nodes carry no service account unless `vendors.gcp.serviceAccount` is set (it needs
`iam.serviceAccounts.actAs` for the controller).

## Nested virtualization

`vendors.gcp.nestedVirtualization: true` sets `advancedMachineFeatures.enableNestedVirtualization` on
the instances of the NodeClass, so that the nodes expose the `vmx` flags and `/dev/kvm`. It is off by
default and is never forced on a pool.

```yaml
spec:
  vendors:
    gcp:
      nestedVirtualization: true
```

Not every machine series supports it, and the driver keeps no table of the series that do. When
Compute Engine refuses an insert (a `Fatal` or `Unsupported` error) whose message names nested
virtualization (case, spaces and underscores ignored, so `NESTED_VIRTUALIZATION` and the organization
policy constraint `compute.disableNestedVirtualization` match too), the driver logs it and launches the
same node once more without the feature, so the node still comes up. This covers an organization
policy that forbids the feature. Each launch of an unsupported type therefore costs one refused insert
first. If the second insert fails, its error is the one returned. The matched text is UNVERIFIED live:
the wording of the refusal has not been confirmed against Compute Engine. The node does not record
whether the feature was applied.

## Regions and zones

`Regions` lists the regions that are `UP` (`regions.list`); `Zones` lists the zones of a region that are
`UP` and not deprecated (`zones.list`). Both are cached for an hour. A zone is
`Zone{ID: <numeric zone ID>, Name: <zone name>}`: Keeper's `zone_id` for GCP is Google's own numeric
zone ID (`2000` for `us-central1-a`), and zone names are global, so there is no per-project mapping
as on AWS. A region the credentials cannot read is `RegionUnavailable`.

## Images

Rung 1 for amd64, rung 2 for arm64 ([ADR-021](../explanation/DECISIONS.md)).

- **amd64**: Flatcar's public project `kinvolk-public`, the image of the pinned release by name,
  `flatcar-<channel>-<major>-<minor>-<patch>` (dashes for dots, for example
  `flatcar-stable-4757-2-3`), read with `images.get`. Publicly shared images cannot be listed
  (`images.list` fails with a missing permission), so the lookup is one `get` per release, never a
  filter. The name shape of `lts`, `beta` and `alpha` is unverified.
- **arm64**: Flatcar publishes no GCE arm64 image. Spare Cores publishes them from its own project
  (plan 22), named `flatcar-<channel>-<major>-<minor>-<patch>-arm64`. That project does not exist
  yet (`SpareCoresImageProject` is empty): until it does, arm64 resolves only from
  `vendors.gcp.imageProject` or an `images` selector. Without either, `status.vendors.gcp.images`
  lists amd64 only, so a NodeClass that names its regions can still be ready; an arm64 launch fails
  with `ImageNotFound` and its offerings are unavailable (ADR-017: no silent fallback). With either,
  arm64 is reported, and a missing image makes `ImagesReady` false.
- `vendors.gcp.imageProject` replaces the default project for both architectures (rung 3: images the
  user made, with the same name pattern).
- **Global**: an image serves every region; `ReportImages` resolves per region only because a
  selector may name region keys.
- **The release check**: the image says its release by its name (the pattern above) or by the labels
  `flatcar-channel` and `flatcar-version` (the version may spell the dots as `-` or `_`: label values
  have none). Another release is `ImageVersionMismatch`, none `ImageVersionUnknown`, a name and labels
  that disagree `ImageVersionUnknown`; the image is never booted. `OBSOLETE` and `DELETED` images
  and images that are not `READY` are not found; `DEPRECATED` ones are (old pinned releases stay).
- **The `images` kind** ([selectors](resource-selectors.md#gcp)): `ids` and an exact `name` are read
  with `images.get`, `filters: family` with `images.getFromFamily`, everything else (`tags` as labels,
  a glob `name`, `match`, `expression`) is listed in the owner projects (`match.owner`, else
  `imageProject`, else the controller's project: the public project is reachable by name only) with
  the filter Compute can evaluate and finished by the shared matcher.
- **Guest OS features** (`GVNIC`, `UEFI_COMPATIBLE`) of `kinvolk-public`'s images are **unverified**:
  the C3, C4 and later series need `GVNIC`; if the images lack it the options are `Supported` removing
  those series, or a Spare Cores amd64 image.

## Launch

`CreateInstance` is `instances.insert` in the zone, after a lookup of the idempotency label.

| Field | Value |
|---|---|
| `name` | the NodeClaim name when it is a valid RFC 1035 name, else `scarpenter-<hash>` |
| `machineType` | `zones/<zone>/machineTypes/<type>` |
| `disks[0]` | boot, `autoDelete`, the image, the disk type (`pd-balanced`, or `hyperdisk-balanced` where Keeper lists no `pd-balanced` for the type), 64 GiB, labels |
| `disks[1..]` | the data volumes: `autoDelete: true` (not the API's default), `deviceName` = the volume's name, `diskName` `<instance>-<name>`, the interface `NVME` for the NVMe-only series |
| `networkInterfaces[0]` | the subnetwork, `IPV4_ONLY` or `IPV4_IPV6` with an external IPv6 access config, one `ONE_TO_ONE_NAT` unless `publicIPv4: false`, the NIC type and Tier 1 of a network profile |
| `tags.items` | the firewall's network tag and the run's tag |
| `labels` | the logical labels, the NodeClass's tags, the idempotency label |
| `scheduling` | on-demand: `STANDARD`, `automaticRestart: false`, `MIGRATE` or `TERMINATE` (series that cannot migrate, compact placement); Spot: `SPOT`, **`instanceTerminationAction: DELETE`** (the default `STOP` would leave a stopped instance and its disks), `TERMINATE`, and `preemptionNoticeDuration` when `--gcp-preemption-notice` is set ([interruption](#interruption)) |
| `metadata.items` | `user-data` (the Ignition), `block-project-ssh-keys: TRUE`, `enable-oslogin: FALSE`, the block's `metadata` |
| `advancedMachineFeatures` | `enableNestedVirtualization: true`, only with `vendors.gcp.nestedVirtualization: true` |
| `serviceAccounts` | only with `vendors.gcp.serviceAccount` |
| `resourcePolicies` | the placement policy of a placement profile |

- **User-data** is at most 256 KB (the metadata value limit); more is `Fatal`.
- **Spot VMs only** (`provisioningModel: SPOT`); not the legacy preemptible model and not Flex-start.
- **Project SSH keys and OS Login**: the instance metadata `block-project-ssh-keys=TRUE` and
  `enable-oslogin=FALSE` override the project's, so a project shared with other workloads gives no
  logins on the nodes. Nodes are reached by Tailscale and Ignition-installed keys. That the guest agent
  honours both is unverified.
- **Idempotency** ([the contract](driver-contract.md#the-idempotency-label)): GCP has a `requestId` and unique
  names per zone, and neither alone gives the contract. The driver first runs one project-wide filtered
  `instances.aggregatedList` (`labels.scarpenter-idempotency-key = <uid>`) and returns the instance if it
  exists, whatever zone, type or capacity type the replay asks for; a failed create leaves the key
  unused. It inserts with a `requestId` that is a UUIDv5 of the NodeClaim UID, zone, type, capacity type,
  **the driver's epoch** (random per process start) and **an attempt counter**. GCE answers a replay of a
  request ID with the first request's operation, and whether that holds for an operation that failed is
  unverified, so the counter counts up after a definitive failure (a failed operation, a non-retryable
  HTTP error): the next try of the same offering is a new request, not the replay of a failure. After a
  lost response (a timeout, a 5xx) the counter stays, so the retry is the replay of the original request.
  A restart changes the epoch; what an earlier process created is found by its label. On `409
  alreadyExists` the instance is read: it is returned when its label says it is this key's, else the create
  is `Fatal`. `bulkInsert` is not used.
- **Asynchronous errors**: capacity and quota errors are in the finished operation's `error.errors[]`; the
  driver waits for the operation and classifies the codes ([Error mapping](#error-mapping)), then reads
  the instance for its addresses.
- **States**: `PROVISIONING` and `STAGING` are pending; `RUNNING` and `REPAIRING` running; `STOPPING`,
  `STOPPED`, `TERMINATED`, `SUSPENDING` and `SUSPENDED` terminating (a stopped instance is not coming
  back and its disks still cost, so the garbage collector deletes it); an unknown status is running. A
  deleted instance is gone: `GetInstance` answers `NotFound`.
- **Delete** reads the instance first and refuses one without the cluster's label (`Fatal`): GCP IAM
  conditions cannot test labels, so this check and a dedicated project are the walls. A deleted
  instance and one that does not exist are success; the disks go with the instance.
- **No `LeftoverCollector`**: a launch makes nothing besides the instance. Its disks are created in the
  insert (`initializeParams`, `autoDelete`), its external addresses are ephemeral (`accessConfigs`), and GCE
  has no network interface object, so no separate call can outlive a failed or interrupted launch. That a
  failed insert operation leaves no disk behind is **unverified**; the lane's teardown lists disks by the
  cluster label and fails on one.
- **List and find** are one `instances.aggregatedList` with the label filter, narrowed to the regions
  asked; every status is listed.
- **Provider ID** `scarpenter://gcp/<region>/<nodeclaim>`; **InstanceRef** `<region>/<zone>/<name>` (the
  zone is in the ID because every instance call needs it); a malformed reference is `NotFound`.

## Node settings

Built-in defaults: a 64 GiB boot disk, `pd-balanced` (or `hyperdisk-balanced` where the type's compatible
storage from Keeper lacks `pd-balanced`, e.g. `c4`, `m4`, `x4`), `publicIPv4: true`, the series' NIC type.
The catalog reads Keeper's `compatible_storage_ids` of the type and maps them to the disk type names of
Keeper's storage table; an empty list means unknown: the default is then `pd-balanced` even for Hyperdisk-only types (the launch fails with the API's error), with no disk type
excluded. A pinned `diskType` that is not in the type's compatible storage, or a pinned root `diskType` that
cannot boot (only the pd types and `hyperdisk-balanced` can; a short disk-type table until Keeper has a
bootable flag), makes the profile unsupported for that type, never a substitute. The NodeClass block's own root disk is not
checked per type (`Supported` sees profiles only): a launch with the wrong type fails with the API's
error (`Unsupported`).

| Block | Fields |
|---|---|
| `storage.root`, `storage.data[]` | `diskType` (`pd-standard`, `pd-balanced`, `pd-ssd`, `hyperdisk-balanced`, `hyperdisk-extreme`, `hyperdisk-throughput`, `hyperdisk-ml`), `sizeGb` (GiB), `provisionedIops`, `provisionedThroughput` (Hyperdisk), `kmsKeyName`; data volumes also `name`, `filesystem`, `mountPath` |
| `storage.localStorage` | `policy` (`ignore`, `mount`, `raid0`), `mountPath`: the local SSDs of types that bundle them |
| placement profile | `resourcePolicy: {groupPlacement: {collocation: COLLOCATED}}` (compact), `{spread: {availabilityDomainCount}}` or `{use: {name}}` |
| network profile | `nicType: GVNIC \| VIRTIO_NET`, `tier1: true` |
| block | `regions`, `project`, `imageProject`, `serviceAccount`, `nestedVirtualization`, `metadata`, `networking`, `resources` |

- **Validation** (`ValidateNodeClass`): regions, the project (equals the controller's), the image project,
  the service account e-mail, reserved metadata keys (`user-data`, `ssh-keys`, `enable-oslogin`,
  `block-project-ssh-keys`), tags (label charset, no `scarpenter-` prefix, at most 40), the shape of
  resource references, and disk sizes, IOPS and throughput against the disk table (`pd-balanced` and
  `hyperdisk-balanced` from Keeper's storage rows of 2026-11-10, the others unverified); `publicIPv4:
  false` without your own subnets is refused (see [egress](#network-addresses-and-egress)).
- **`Supported`**: a pinned root disk type the type can attach and boot from, data disks it can attach
  (both by Keeper's compatible storage), local storage only on types that bundle local SSDs, Tier 1 only on
  the series and sizes that offer it (30 vCPUs or more), compact placement only on the series that support
  it. The series tables for these are in `series.go` and **unverified**.
- **`PriceDelta`**: disks at Keeper's GB-month price of the region and disk type divided by 730, plus the
  external IPv4 address (Keeper's `ipv4_price` row of the region; `SCARPENTER_GCP_IPV4_PRICE` /
  `vendors.gcp.ipv4Price` overrides it, also where Keeper has no row). Both come from the snapshot of the
  Keeper data version the catalog loaded, which the catalog hands to the driver after every refresh.
  **Incomplete** for provisioned Hyperdisk IOPS or throughput above the baseline, Tier 1, and any price Keeper
  lacks.
- **Device hints**: GCP lets the driver name the device. A data volume's hint is
  `ByID: /dev/disk/by-id/google-<name>`; no serial pattern, no equal-size ambiguity. It relies on the
  guest's udev rules (the amd64 OEM sysext; the path naming of NVMe-attached disks is **unverified**). Local
  SSDs: the pattern `google-local-*` (unverified). Disks attach in the insert, so the volume unit's
  timeout is not at risk.

## Resources and network

| Kind | Scope | Default lifecycle | `use` | `create` |
|---|---|---|---|---|
| `subnets` | region | `idleTTL` | `ids` (`projects/<p>/regions/<r>/subnetworks/<n>` or the short name), `name`, `match` (`network`, `cidr`, `within`, `state`, `description`), `filters`, `expression`; **no `tags`** (no labels) | the cluster's global VPC `scarpenter-<cluster>-subnets` and one subnetwork per region named `scarpenter-<cluster>-subnets-<region>`; `create.pool` sets the range they are taken from |
| `firewalls` | region | `idleTTL` | `ids` are **network tags**, `name` a glob on the tags the rules target, `match.network`; no `tags` | rules for the tag `scarpenter-<cluster>-firewalls-<region>-...` |
| `images` | global | never | see [Images](#images) | nothing |
| `runFirewalls` (internal) | run | `perUse` | not configurable | one rule per run key |
| `resourcePolicies` | run | `perUse` | `ids`/`name` through the placement profile | a policy per (region, profile, run key) |

- **Ownership marker.** Networks, subnetworks, firewall rules and resource policies have **no labels**. The
  marker is the name prefix `scarpenter-<cluster>-` **plus a description**
  `scarpenter: cluster=<c> nodeclass=<n> managed-by=scarpenter resource-kind=<kind> region=<r>` (the global network's has no region). Both are
  required before the garbage collector lists, uses or deletes a resource; a resource of the name without
  the description is a `Fatal` conflict on create and is never deleted. Instances and disks carry real labels.
- **The managed network** is **one global custom-mode VPC per cluster**, `scarpenter-<cluster>-subnets` (Compute
  networks are global), shared by the cluster's NodeClasses, with **one subnetwork per region**, dual stack
  with an external IPv6 range and private Google access. The VPC's description carries the marker without a
  region. Each subnetwork is a `/20`: the first `/20` of the pool (default `10.240.0.0/12`) that overlaps no
  primary or secondary range of the VPC's subnetworks, found by reading the VPC's subnetworks, so the choice is
  the same after a restart (an existing subnetwork of the name is adopted with its range) and never collides
  with one that exists. The allocation and the insert are one critical section in the controller; when another
  process wins a range (Compute answers `Invalid IPCidrRange: ... conflicts with existing subnetwork ...`) the
  insert is retried with a fresh listing, up to five times. An exhausted pool is `Fatal`.
  The pool is set per NodeClass with `resources.subnets.create.pool`: an IPv4 CIDR inside `10.0.0.0/8`,
  `172.16.0.0/12` or `192.168.0.0/16`, in canonical form, no smaller than a `/20` (checked by the CRD and again
  by `ValidateNodeClass`). A NodeClass with a pool gets its own subnetwork per region (the pool is part of the
  resource's identity), taken from that pool in the same VPC. Set it when a network your VPC is peered with, or
  reaches by VPN, uses `10.240.0.0/12`. Nodes in different regions have a private path through the VPC; the
  firewall rules still allow only what the tag rules say. The VPC and its subnetworks are one resource kind:
  `List` returns the subnetworks, a VPC with no subnetwork left is listed as the network so it is not leaked,
  and `Delete` removes the subnetwork, then the VPC when it was the last one and carries the marker. A VPC
  that something else still uses (rules of another NodeClass, or a subnetwork another controller added since
  the read) stays, and the subnetwork's deletion is done; the VPC lists as an orphan and goes later. The
  check of the VPC and the insert of a subnetwork, and the deletion of the VPC with its last subnetwork, are
  one critical section in the controller; when another controller deletes the VPC in between, the insert
  finds no VPC and it is made again.
- **The managed firewall** is a set of rules for one network tag: a rule per inbound rule of
  `spec.networking` (CIDR sources; Compute refuses a rule that mixes IPv4 and IPv6, so a rule with both
  is two, the IPv6 one named `<tag>-in<n>v6`), a rule that lets the nodes of the tag reach each other for isolation
  `nodeclass` (and for isolation `run` without a run key), and a deny rule at the lowest priority that says
  what the implied rule does (`<tag>-deny` for IPv4, `<tag>-denyv6` for IPv6), so the NodeClass's firewall is never an empty object. The run tag is one
  rule, source tag and target tag the same. A rule that went missing is made again on the next `Ensure`.
  `Delete` removes the rules of the tag and is `Retryable` while an instance still carries the tag.
- **Resolution without `use`** ([ADR-022](../explanation/DECISIONS.md)): GCP resources have no labels, so
  there is no discovery; then the project's **default network** and its subnetwork `default` of the region,
  when the network exists **and can honour the request**: it is skipped, with the reason, when `publicIPv6`
  is on (its subnetworks are IPv4 only) or when it has an enabled ingress rule that applies to every instance
  (`default-allow-internal`, `default-allow-ssh`: GCP rules only add allows, so isolation and the inbound
  rules could not be honoured); then `create`. The firewalls kind has no default. Projects that removed
  the default network (`constraints/compute.skipDefaultNetworkCreation`) fall to `create`.
- **`use` of subnets and firewalls**: IDs of a Shared VPC host project's subnetworks work (`ids` are read
  with `subnetworks.get`). A firewall `use` names a tag your rules target; the rules should allow the nodes'
  traffic (self, UDP 41641, ICMP).

## Network addresses and egress

Addresses come from `networkInterfaces[]` (`networkIP`, `accessConfigs[].natIP`, `ipv6Address`,
`ipv6AccessConfigs[].externalIpv6`), read after the insert. `publicIPv6` needs a subnetwork with
`stackType: IPV4_IPV6` and an external IPv6 range (the managed one has them; a launch into another is
`Unsupported`, `ResourceMissingInZone`). Node IP: The node's only interface has the subnet's private address (the public one is NAT'ed), which the kubelet of the `direct` template registers without a hint, so the driver reports no node IP range ([node bootstrap](node-bootstrap.md#the-node-ip-on-a-private-network)).

`publicIPv4: false` is refused unless the NodeClass brings its own subnetworks
(`resources.subnets.use`): the default and the managed network have no Cloud NAT, so the node would have no
egress for its sysexts and container images. With your own subnetworks, a Cloud NAT or a proxy is your job.

## Labels and tags

`TagKey(k)` is `scarpenter-<k>`: `scarpenter-cluster`, `-nodeclaim`, `-nodepool`, `-nodeclass`,
`-managed-by`, `-resource-kind`, `-idempotency-key`, on instances and their disks. Values go through one
deterministic `LabelValue()` (valid values unchanged, else lower-cased with a short hash), used for writing
and for filtering, so `FindInstance` and `ListInstances` match. NodeClass `tags` are validated against the
label charset (lower-case letters, digits, `_`, `-`; keys start with a letter) and may not start with
`scarpenter-`.

## Quotas

`regions.get` returns `quotas[]` with limit **and usage** (no usage is computed from instances) and
`projects.get` the project-wide ones. The reader reports the CPU buckets (`CPUS`, `<SERIES>_CPUS`,
`PREEMPTIBLE_CPUS`), the GPU buckets (below) and `INSTANCES` per region, and `CPUS_ALL_REGIONS` as the account-wide bucket when the
project has it. `Counted` are the cluster's instances in the region that hold quota (not stopped ones), so
the tracker releases their in-flight reservations.

**Which metrics one machine type consumes is unverified** (does `n2-standard-8` consume `CPUS` as well as
`N2_CPUS`? how does a project show preemptible quota?). `Consumption` therefore answers only for series in
`gcp.ConfirmedSeries`, which is **empty**: every type is unknown quota and takes the reactive path, where
`QUOTA_EXCEEDED` names the metric (`Quota 'CPUS' exceeded. Limit: N in region R`) and the unavailable
cache holds the offering back, account-wide for an `_ALL_REGIONS` metric. A series is added only with the
lane's `quota` scenario as its proof. Spot is always unknown (it depends on the region's preemptible quota,
which `Consumption` cannot see).

## GPU machine types

The accelerator-optimized series `a2`, `a3`, `a4`, `a4x`, `g2` and `g4` are offered like any series; the TPU series
(`ct3`, `ct3p`, `ct5l`, `ct5lp`, `ct5p`, `ct6e`, `tpu7`, `tpu7x`) stay dropped (`droppedSeries`). The GPUs are part of the
machine type, so the launch needs no `guestAccelerators`, and N1 with attached GPUs is not offered (Keeper has no such type).
The core drops what SCarpenter cannot serve (the reasons of [labels and offerings](labels-and-offerings.md)): Blackwell
(`a4` B200, `g4` RTX PRO 6000, `a4x` GB200 on arm64) and fractional types. The 8- and 16-GPU types of A100, H100 and H200 are
offered with Fabric Manager ([node bootstrap](node-bootstrap.md#nvidia-gpu-nodes)). What the series add to a launch is
already in the driver's tables: `onHostMaintenance: TERMINATE` (the series cannot live migrate; `automaticRestart` is false for
every instance), compact placement where the series supports it, and NVMe or Hyperdisk disks for `a3` and `a4`. The NodePool
lists the GPU startup taint ([run GPU workloads](../how-to-guides/run-gpu-workloads.md#the-gpu-pending-taint)).
The GPU types are offered, not yet verified by a real run.

The quota buckets of GPUs are read like the CPU ones, with the unit `GPUs`: per region `NVIDIA_<MODEL>_GPUS` and
`PREEMPTIBLE_NVIDIA_<MODEL>_GPUS`, and the account-wide `GPUS_ALL_REGIONS`. **The metric names per model are unverified** (the
A100 40 and 80 GB models in particular), and no GPU type is in `gcp.ConfirmedSeries`: a GPU launch takes the reactive path, where
`QUOTA_EXCEEDED` names the metric. A series joins the confirmed list when a real run confirms what it consumes.

## Error mapping

Asynchronous errors are in the finished operation (`error.errors[].code`), synchronous ones are
`*googleapi.Error` with a status and a reason. Both go through the one table in `errors.go`, covered entry
by entry by `errors_test.go`; every entry is unverified live:

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `ZONE_RESOURCE_POOL_EXHAUSTED`, `ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS`, `REGION_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS`, `POOL_CAPACITY_INSUFFICIENT` |
| `QuotaExceeded` | `QUOTA_EXCEEDED` (the metric is read from `Quota '<m>' exceeded`); account-wide when the metric ends in `_ALL_REGIONS` |
| `Unsupported` | `UNSUPPORTED_OPERATION`; a 404 that names the machine type of the call |
| `ZoneUnusable` | `IP_SPACE_EXHAUSTED`, `IP_SPACE_EXHAUSTED_WITH_DETAILS` |
| `Retryable` | HTTP 429 and 5xx, `RESOURCE_OPERATION_RATE_EXCEEDED`, `INTERNAL_ERROR`, `backendError`, `rateLimitExceeded`, `resourceInUseByAnotherResource` (clears when the users are gone), the driver's own timeout |
| `NotFound` | 404 |
| `RegionUnavailable` | 401 and 403 on the calls that read a region's facts |
| `Fatal` | `ALREADY_EXISTS` with a foreign instance, `CONDITION_NOT_MET` (reported with the code `OrganizationPolicyViolation`: an organization policy such as `constraints/compute.trustedImageProjects` may forbid the image project), permission denied, everything else |

## Metadata exposure

The GCE metadata server has no hop limit (AWS's hop limit 1 has no equivalent). It serves `user-data`: the
bootstrap token (valid for at most an hour, deleted at registration) and the Tailscale key (ephemeral and
tag-scoped, but reusable in v1). Nodes have no service account unless you attach one, so no cloud credentials
are exposed. By default (`spec.instanceMetadata.access: Restricted`) the driver's addresses `169.254.169.254`
and `fd20:ce::254` are reachable only by Afterburn's units and `oem-gce.service` (the guest agent and its
daemons); Pods in the Pod network and in the host network and every other host process are dropped, and DNS
and NTP at the same addresses keep working ([instance metadata access](node-bootstrap.md#instance-metadata-access)).
OS Login is off (`enable-oslogin=FALSE`), so no process outside the agent needs the metadata server. With `Open`,
keep service accounts off the nodes and treat the metadata as readable by Pods.

## IAM

Two custom roles ([`gcp-iam-role.yaml`](gcp-iam-role.yaml), and the optional
[`gcp-iam-role-managed.yaml`](gcp-iam-role-managed.yaml) for the networks and rules SCarpenter creates),
not `roles/compute.instanceAdmin.v1` (wider). `iam_test.go` keeps the files and the driver's calls in
step: every `ComputeAPI` method has its permissions in a table, and the roles hold nothing else.
`iam.serviceAccounts.actAs` is needed only with `serviceAccount`.

**Least privilege is weaker than on AWS**: IAM conditions can test the resource name, type and
resource-manager tags, not labels, so "delete only with the cluster label" has no equivalent. The driver's
ownership check is the wall, and a **dedicated project** for the nodes is recommended (and used by the
lane): it also keeps project-wide SSH keys and quotas of other workloads out of the way.

## Billing

Per-second billing with a one-minute minimum (Keeper's row 1/60). The compute pricing page was not read,
so the rule is reported **unverified**: billing retention stays off for GCP and the NodeClass status says
so. A period under `minPeriod` (default 10 minutes) would not qualify anyway. `Instance.BillingStart` is
`creationTimestamp`.

## Interruption

A preempted Spot VM is announced in the instance metadata, so GCP uses the [node agent](node-bootstrap.md#the-interruption-agent)
([plan 26](../plans/v1/done/26-interruption.md), [handle spot interruptions](../how-to-guides/handle-spot-interruptions.md)); the
driver implements `InterruptionNotices` and `Instance.GoneReason`.

- **The check.** The agent's `scarpenter_check` long-polls `computeMetadata/v1/instance/preempted?wait_for_change=true`
  (`Metadata-Flavor: Google`, at `169.254.169.254`, a 60 s wait, started from the ETag of a first read) and reads the value
  again afterwards; `TRUE` is a notice, the reason `SpotInterrupted`. The agent runs in the metadata slice, so it works with the
  metadata restriction on (the default) and off. The check pauses 1 s after a poll that found nothing, so a failing long poll cannot spin. Google sends
  the ACPI G2 signal right after the preemption notice, then stops the VM up to 30 s later.
- **The notice and the deadline.** The metadata value gives no time, so the deadline is derived. `--gcp-preemption-notice` (chart
  `vendors.gcp.preemptionNotice`) is the **switch**: `0`, the default, launches Spot VMs without a notice, so the value turns `TRUE`
  together with the ACPI signal, the check prints no deadline and the controller assumes the 30 s of the shutdown window; the kubelet's
  graceful shutdown (25 s, rendered on spot launches) is the backstop if the agent misses it. With a notice of `N` seconds (at most `2m`),
  the launch sets `scheduling.preemptionNoticeDuration` and the check prints `now + N` as the deadline, so Pods get `N` seconds
  before the ACPI signal; the window is 30 s to `30 s + N`.
- **Why the default is off.** Plan 26 asks for the 120 s notice by default only if spike S3 shows that it costs nothing extra; S3 is a manual
  live check that has not run, so the notice is off until the maintainer sets `--gcp-preemption-notice=2m` (or changes the default
  after S3). Nothing else changes between the two.
- **`GoneReason`.** A Spot VM that is stopping, stopped or terminated reports `SpotInterrupted`; SCarpenter's own deletes are filtered by the
  core, as for OCI. A person stopping a Spot VM by hand gets the same reason: telling them apart needs the zone operation
  `compute.instances.preempted`, which would add an API method and the `compute.zoneOperations.list` permission, and is not done. A
  preempted VM is deleted (`instanceTerminationAction: DELETE`) and is often gone from the API before the next call: the
  interruption is then recorded as `ReclaimedWithoutNotice` if no notice reached the node.
- **Size.** The agent adds 1.5 KiB at most to the user-data; the worst case of a spot node (the default templates' worst case with the
  agent and the notice) is about 13.0 KiB against the limit of 256 KB for a metadata value (`TestWorstCaseGCPSpotUserDataStaysInTheBudget`).

Unverified until a live run (spike S3): the field `preemptionNoticeDuration` (from the Compute API's `Scheduling`; whether 120 s is its
limit and whether it is billed), that `gcloud compute instances simulate-maintenance-event` preempts a Spot VM with `preempted=TRUE`, the
long poll's behaviour on its timeout, and the timing of the zone operation.

## Deferred

Multi-project (one project per NodeClass); sole-tenant nodes; TPU series (`ct*`, `tpu7*`), which the offerings drop; N1 with attached GPUs
(`guestAccelerators`; Keeper's catalog has no such type); bare metal; custom machine types;
Flex-start and the legacy preemptible model; `maxRunDuration`; firewall rules by service account
(`sourceServiceAccounts`); resource-manager tags on instances; the Cloud Quotas API (only if `regions.get`
proves insufficient).

## What is not verified against GCP yet

The [interruption](#interruption) facts (the notice field and its cost, the simulated preemption, the long poll).

Every error code and reason above; the quota metrics a machine type consumes (the GPU metric names per model included) and the Spot/preemptible rule;
`requestId` replay for a failed operation and how long request IDs are kept (the attempt counter is the
defence); the machine series tables (`hyperdiskOnlySeries`, NVMe-only, no-Hyperdisk, no-migrate, compact,
Tier 1); the guest OS features of `kinvolk-public`'s images (`GVNIC`); the name shape of the `lts`, `beta` and
`alpha` images; the arm64 image route; the guest agent's handling of `block-project-ssh-keys` and
`enable-oslogin`; the device path names of NVMe data disks and local SSDs; whether `aggregatedList` tolerates
a down zone (the generated request of `cloud.google.com/go/compute` v1.75.0 has no `returnPartialSuccess`
field); the disk size and IOPS limits outside `pd-balanced` and `hyperdisk-balanced`; the external IPv4
address price; that every method the lane's CI role uses supports IAM name conditions; the pricing page's
billing rule; that a failed instance insert leaves no disk (hence no `LeftoverCollector`); whether a range
conflict between two concurrent subnetwork inserts is refused in the call or in the operation (both are
retried). The L5 lane (`test/e2e-gcp`) is written and statically validated, and it is not in `test/e2e-vendors.txt` until its
cost guards (the regional role's name conditions, the quotas and a budget alert of the dedicated project, the
sweeper with the real identity, the read-only canary role) are verified on a live project:
[Per-vendor cost guards](../how-to-guides/testing-and-releasing.md#gcp). Its first live run is the
coordinator's and reports each of these.
