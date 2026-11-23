# Labels and offerings

How the Spare Cores catalog (the Keeper API) becomes Karpenter instance types
and priced offerings: names, capacity, every label with its source field and
what happens when the value is unknown, how metrics and selection profiles are
evaluated, how often it refreshes and what happens when Keeper is down. The
names of the labels are listed in [Labels and annotations](labels.md); the
design is in the [overview](../plans/v1/wip/00-overview.md#5-catalog-keeper-data-to-instance-types-and-offerings)
and [00a](../plans/v1/wip/00a-node-options.md#7-instance-selection). The code is
`pkg/catalog` and `pkg/catalog/labels`.

## Table of Contents

- [Terminology](#terminology)
- [Instance types](#instance-types)
  - [Names](#names)
  - [Capacity and overhead](#capacity-and-overhead)
  - [Which servers become instance types](#which-servers-become-instance-types)
  - [GPU types](#gpu-types)
- [Offerings](#offerings)
  - [Zones and zone IDs](#zones-and-zone-ids)
  - [Prices](#prices)
  - [Availability](#availability)
  - [Why a type or an offering is not offered](#why-a-type-or-an-offering-is-not-offered)
  - [The offered summary in the status](#the-offered-summary-in-the-status)
  - [The offered listing](#the-offered-listing)
- [Labels](#labels)
  - [Unknown values](#unknown-values)
  - [Scheduling and topology labels](#scheduling-and-topology-labels)
  - [Attribute labels](#attribute-labels)
  - [Metric labels](#metric-labels)
  - [Selection profiles](#selection-profiles)
  - [Location labels](#location-labels)
  - [Profile labels](#profile-labels)
  - [Registration with Karpenter core](#registration-with-karpenter-core)
- [The catalog](#the-catalog)
  - [Scope](#scope)
  - [Refresh and staleness](#refresh-and-staleness)
  - [The pre-filter](#the-pre-filter)
  - [Metrics](#metrics)
  - [Scale](#scale)
- [Troubleshooting](#troubleshooting)
- [References](#references)

## Terminology

- **Keeper**: the public API of Spare Cores
  (`https://keeper.sparecores.net`), the only source of instance types and
  prices.
- **Instance type**: a Karpenter `InstanceType`: what a node of one server type
  looks like, with requirements (labels), capacity, overhead and offerings.
- **Offering**: where an instance type can be launched and at what price: one
  per zone and capacity type.
- **Snapshot**: the data of one successful refresh, read at one Keeper
  `data_version` (the database hash of `/healthcheck`).
- **Scope**: the vendors and regions the SCNodeClasses allow; only those are
  read.

## Instance types

### Names

One instance type per vendor server type: `<vendor>.<api_reference>`, for
example `aws.m7i.large` ([ADR-008](../explanation/DECISIONS.md)). The name is the
value of `node.kubernetes.io/instance-type`, so it must be a label value:

- characters outside `[A-Za-z0-9_.-]` become `-`, and the name starts and ends
  with an alphanumeric;
- a name longer than 63 characters is truncated to 56 characters and suffixed
  with `-` and the first 6 hex characters of the SHA-256 of the full name;
- a name that sanitising changed also gets that suffix, so `a b` and `a/b` can
  never collide.

The raw reference is the `scarpenter.sparecores.com/api-reference` label. When
it does not fit a label value that key is `DoesNotExist` on the type;
`Catalog.Lookup(name)` still resolves the name to `(vendor, api_reference)`.

### OCI types

OCI is read from Keeper like the other vendors (it needs a Keeper API key):

- OCI's names are `oci.<shape>` for the fixed shapes (`oci.BM.Standard3.64`) and
  `oci.<shape>.<ocpus>-<memory GB>` for the flexible ones
  (`oci.VM.Standard.E5.Flex.2-16`; the shape may contain a hyphen, as in
  `oci.VM.Standard.E6-HM.Ax.Flex.2-32`): Keeper gives all the sizes of a flexible
  shape one `api_reference`, so rule R names each size by its server ID (256 priced
  sized types of 12 shapes in the data of 2026-11-17). The `api-reference` label is
  the same name without `oci.`. A server Keeper lists without a price (the
  `VM.Standard.E6-HM.Ax.Flex` sizes on that date) is not a type until it has one.
- Zones are the zone table's: `topology.kubernetes.io/zone` is the zone's name
  as the driver reports it (`EU-FRANKFURT-1-AD-1`, `US-ASHBURN-AD-1`,
  `PHX-AD-1`) and the zone ID Keeper's (`eu-frankfurt-1-ad-1`). Prices are per
  availability domain in the data, the same in all of them.
- Keeper has no benchmark scores of OCI yet: the `metric-*` labels and selection
  profiles of these types are `DoesNotExist`, so a NodeClass that pins a metric or
  a selection finds no type of the vendor.

### Capacity and overhead

| Resource | Value |
|---|---|
| `cpu` | Keeper `vcpus` |
| `memory` | Keeper `memory_amount` (MiB) less `kubelet.vmMemoryOverheadPercent` (default 7.5 %, rounded down) |
| `pods` | `kubelet.maxPods` (default 110) |
| `ephemeral-storage` | the root disk of the type under the vendor's resolved default settings: the driver's per-type answer where the type defines its root disk, else the settings' root disk, else Keeper's `storage_size` of the type; omitted when none says |
| `nvidia.com/gpu` | the GPU count of an NVIDIA GPU type the catalog keeps ([GPU types](#gpu-types)); nothing is reserved. Other types have no GPU resource |

The NVIDIA device plugin (shipped by the chart) registers the resource on the
node; Karpenter initializes a NodeClaim that requests it only when the plugin
has. The node bootstrap installs the driver, so a supported GPU type carries the resource and the driver labels
and a Pod that requests GPUs gets such a node ([run GPU workloads](../how-to-guides/run-gpu-workloads.md)).
The catalog offers these types only to a NodePool that lists the
`gpu-pending` startup taint ([ADR-031](../explanation/DECISIONS.md#adr-031-gpu-nodes-register-before-the-check-and-fabric-manager-work-are-done-accepted-2026-11-22)):
for any other NodePool they are left out, which is expected. Accelerators that are not GPUs (NPUs, FPGAs, VPUs, TPUs) have no extended
resource and are selected by label.

Overhead (what the scheduler subtracts from capacity to get allocatable):

- **kube-reserved**: the formula of the AWS provider. Memory is 11 MiB per pod
  plus 255 MiB (1465 MiB at 110 pods); CPU is 6 % of the first core, 1 % of the
  second, 0.5 % of the third and fourth and 0.25 % of every core above (70m for 2
  vCPUs, 90m for 8); ephemeral storage 1 GiB. `kubelet.kubeReserved` replaces
  the computed value of each resource it names.
- **system-reserved**: `kubelet.systemReserved` (default: nothing).
- **eviction threshold**: `kubelet.evictionHard`; `memory.available` defaults to
  100Mi. A percentage is taken of the capacity. Only the memory and nodefs
  signals reserve resources the scheduler counts.

### Which servers become instance types

A server type is offered when all of these hold:

- its Keeper `status` is `active` or `planned-for-retirement` (`inactive` and
  `retired` are dropped; retiring types stay so that running nodes do not drift
  with `instanceTypeNotFound`);
- its CPU architecture is `x86_64` (`amd64`) or `arm64`; `i386` and the `*_mac`
  architectures have no Kubernetes equivalent;
- it has at least one usable price in scope (below);
- if it has GPUs, the rules of [GPU types](#gpu-types) hold.

Servers of a vendor that share an `api_reference` take their server ID as the
name (rule R, applied by every source before scores and selections key by the
name): the sized types of OCI (`VM.Standard.E5.Flex.2-16`, one per OCPU and
memory size) share the name of their shape in Keeper's rows and so get one type
each. Where the references are unique, which is every other vendor's data, the
reference is kept.

### GPU types

A type whose `accelerator_type` is `GPU` is offered only when SCarpenter can run
it: it boots with Flatcar's prebuilt NVIDIA driver sysext of the NodeClass's
Flatcar release (`spec.flatcar.version`), so what the release builds decides.
The table is `pkg/nvidia`; all of these must hold, checked in this order, and the
first that fails is the drop reason:

| Reason | The type is dropped when |
|---|---|
| `GPUManufacturer` | the manufacturer is not NVIDIA (AMD GPUs, or no manufacturer) |
| `GPUModelUnknown` | the model is not in the table (a test over the committed snapshots fails on a new NVIDIA model) |
| `GPUFractional` | the GPU count is not a whole number: a slice of a GPU |
| `GPUArch` | the CPU is not amd64 |
| `GPUNoDriverBranch` | the release builds no driver branch for the model's architecture |
| `GPUNVSwitch` | the type has the model's NVSwitch GPU count (8 or more A100, H100, H200, B200, B300) and either the release has no pinned [Fabric Manager](node-bootstrap.md#nvidia-gpu-nodes) archive for its driver, or the model needs more than Fabric Manager (B200 and B300, the NVLink5 systems, which also need NVIDIA's NVLink subnet manager, `nvlsm`) |
| `GPUVirtual` | the driver says the GPU is virtual (a vGPU that needs a GRID driver; the optional [`GPUVirtualizer`](driver-contract.md) capability) |

Dropped types are not offered and never launched; they are counted per NodeClass
in `scarpenter_catalog_dropped_types` (below). Dropping, not marking
unavailable: an offering that can never become ready would be retried as
unavailable for ever, and a node whose GPUs cannot be used is a silent degrade.
NPUs, FPGAs, VPUs and TPUs are not GPUs here and keep their behaviour.

The driver is the highest branch the release builds within the architecture's
range, with the open kernel modules for Turing and later (Blackwell needs them)
and the proprietary ones before. It is stamped as three labels on the type, and
so on the NodeClaim and the Node: `nvidia-driver-branch` (`570`),
`nvidia-driver-version` (`570.211.01`) and `nvidia-driver-flavor` (`open` or
`proprietary`); `DoesNotExist` on every other type. Like `nvidia.com/gpu`, the
labels are set only once nodes boot the driver (above); until then they are
`DoesNotExist` on every type, while the drop rules already apply.

<!-- nvidia-table:4757.2.3 -->
| Model | Architecture | Driver | NVSwitch from |
|---|---|---|---|
| `A10` | Ampere | 570 (570.211.01, open) | - |
| `A100` | Ampere | 570 (570.211.01, open) | 8 GPUs, with Fabric Manager |
| `A10G` | Ampere | 570 (570.211.01, open) | - |
| `A16` | Ampere | 570 (570.211.01, open) | - |
| `A30` | Ampere | 570 (570.211.01, open) | - |
| `A40` | Ampere | 570 (570.211.01, open) | - |
| `B200` | Blackwell | none | 8 GPUs, not offered (needs nvlsm) |
| `B300` | Blackwell | none | 8 GPUs, not offered (needs nvlsm) |
| `GB200` | Blackwell | none | - |
| `GB300` | Blackwell | none | - |
| `H100` | Hopper | 570 (570.211.01, open) | 8 GPUs, with Fabric Manager |
| `H200` | Hopper | 570 (570.211.01, open) | 8 GPUs, with Fabric Manager |
| `K80` | Kepler | none | - |
| `L20` | Ada | 570 (570.211.01, open) | - |
| `L4` | Ada | 570 (570.211.01, open) | - |
| `L40` | Ada | 570 (570.211.01, open) | - |
| `L40S` | Ada | 570 (570.211.01, open) | - |
| `M60` | Maxwell | 570 (570.211.01, proprietary) | - |
| `P100` | Pascal | 570 (570.211.01, proprietary) | - |
| `P4` | Pascal | 570 (570.211.01, proprietary) | - |
| `P40` | Pascal | 570 (570.211.01, proprietary) | - |
| `RTX 5000` | Turing | 570 (570.211.01, open) | - |
| `RTX PRO 4500` | Blackwell | none | - |
| `RTX PRO 6000` | Blackwell | none | - |
| `T4` | Turing | 570 (570.211.01, open) | - |
| `T4G` | Turing | 570 (570.211.01, open) | - |
| `V100` | Volta | 570 (570.211.01, proprietary) | - |
| `V100S` | Volta | 570 (570.211.01, proprietary) | - |
<!-- /nvidia-table -->

The table covers the models; the CPU architecture is checked separately, so
`T4G` (arm64) is never offered. Stable 4757.2.3 builds the branches 535, 550 and 570, no 580, so Blackwell is not
offered on it. The same table on an alpha release that has 580 (4865.0.0):

<!-- nvidia-table:4865.0.0 -->
| Model | Architecture | Driver | NVSwitch from |
|---|---|---|---|
| `A10` | Ampere | 580 (580.178.04, open) | - |
| `A100` | Ampere | 580 (580.178.04, open) | 8 GPUs, with Fabric Manager |
| `A10G` | Ampere | 580 (580.178.04, open) | - |
| `A16` | Ampere | 580 (580.178.04, open) | - |
| `A30` | Ampere | 580 (580.178.04, open) | - |
| `A40` | Ampere | 580 (580.178.04, open) | - |
| `B200` | Blackwell | 580 (580.178.04, open) | 8 GPUs, not offered (needs nvlsm) |
| `B300` | Blackwell | 580 (580.178.04, open) | 8 GPUs, not offered (needs nvlsm) |
| `GB200` | Blackwell | 580 (580.178.04, open) | - |
| `GB300` | Blackwell | 580 (580.178.04, open) | - |
| `H100` | Hopper | 580 (580.178.04, open) | 8 GPUs, with Fabric Manager |
| `H200` | Hopper | 580 (580.178.04, open) | 8 GPUs, with Fabric Manager |
| `K80` | Kepler | none | - |
| `L20` | Ada | 580 (580.178.04, open) | - |
| `L4` | Ada | 580 (580.178.04, open) | - |
| `L40` | Ada | 580 (580.178.04, open) | - |
| `L40S` | Ada | 580 (580.178.04, open) | - |
| `M60` | Maxwell | 580 (580.178.04, proprietary) | - |
| `P100` | Pascal | 580 (580.178.04, proprietary) | - |
| `P4` | Pascal | 580 (580.178.04, proprietary) | - |
| `P40` | Pascal | 580 (580.178.04, proprietary) | - |
| `RTX 5000` | Turing | 580 (580.178.04, open) | - |
| `RTX PRO 4500` | Blackwell | 580 (580.178.04, open) | - |
| `RTX PRO 6000` | Blackwell | 580 (580.178.04, open) | - |
| `T4` | Turing | 580 (580.178.04, open) | - |
| `T4G` | Turing | 580 (580.178.04, open) | - |
| `V100` | Volta | 580 (580.178.04, proprietary) | - |
| `V100S` | Volta | 580 (580.178.04, proprietary) | - |
<!-- /nvidia-table -->

## Offerings

One offering per (instance type, zone, capacity type) with a price in scope.
Keeper `ondemand` prices become `on-demand` offerings and `spot` prices `spot`
offerings.

Because offerings come only from Keeper's prices, which vendors have spot is Keeper's data, not a list in SCarpenter: AWS, GCP, Azure, Alibaba and OCI have spot
rows (UpCloud has them for GPU plans only, not launched), Hetzner, Vultr and OVH have none and get no spot offering. A
spot offering is priced from the spot row alone and the NodeClaim gets `karpenter.sh/capacity-type: spot` from
`Instance.CapacityType`. What each driver sends at create for spot is in its reference (AWS one-time request, terminate;
GCP `SPOT` with `DELETE`; Azure `Spot`, eviction `Delete`, max price -1; OCI preemptible, boot volume not preserved;
Alibaba `SpotAsPriceGo`, `Terminate`). Tests: `TestSpotOfferingsComeFromKeepersSpotRows` over the committed snapshot.

### Zones and zone IDs

Keeper rows carry the zone's portable **zone ID** (`use1-az1` on AWS). Zone names
are account-specific at AWS, so the driver supplies the zones of its own account
as `region → [{ID, Name}]` ([ADR-009](../explanation/DECISIONS.md)):

- `topology.kubernetes.io/zone` is the driver's zone **name**, and
  `scarpenter.sparecores.com/zone-id` the Keeper zone **ID**;
- a price for a zone ID the driver does not know is dropped;
- a zone that Keeper's zone table marks inactive is dropped;
- a region-level price (a row without a zone) is replicated into every zone the
  driver reports for the region;
- a vendor without zones is offered with zone = region (the zone ID is Keeper's,
  or the region name when Keeper has none).

The region label (`topology.kubernetes.io/region`) is the vendor's name of the
region (Keeper's `api_reference`: `europe-west3` at GCP, whose Keeper region ID
is `1300`). A NodeClass's region allow-list accepts the vendor's name, Keeper's
region ID or an alias; only active regions match.

### Prices

`Price` is the hourly USD price of the `server_price` file of Keeper's snapshot
for the vendor, plus the price delta of the driver's default settings (root disk,
public IPv4 where billed). The disk and IPv4 parts come from the snapshot's
`storage_price` and `ipv4_price` files of the same data version (handed to the
drivers by the catalog, `SetKeeperPrices`); an explicit `SCARPENTER_*_IPV4_PRICE`
overrides Keeper's IPv4 price, and a region without a Keeper row makes the delta
incomplete. Only active, hourly, USD, Linux rows with a price above zero count.
Snapshot prices are converted to USD by Keeper and not rounded, so offering
prices of converted vendors differ from the old rounded routes in the last digits
(selection still rounds to 4 digits). With
`pricing.allowIncomplete: false` offerings whose delta is incomplete are
dropped. When several rows give the same zone and capacity type the lowest
wins.

When a NodePool pins one selection profile whose objective is not `Price`,
offerings are priced by the objective instead (see
[Selection profiles](#selection-profiles)).

### Availability

An offering is unavailable (`Available=false`) while the
[unavailable-offerings cache](#unavailable-offerings) has an entry for it, while
the quota tracker knows the quota and it has no headroom for one more instance,
while the class's vendor resources are not ready in the region
(`ImageGap`, `RegionNotReady`, `ZoneMissingResource`), and, for a NodePool that
pins a selection profile whose objective is not `Price`, when the type has no
score under it (`Unrankable`). Each of these is a
[reason](#why-a-type-or-an-offering-is-not-offered).

#### Unavailable offerings

`pkg/unavailable` holds the offerings that recently failed. Marking, expiry and
the sequence number the catalog's cache key uses are clock-injectable.

| Error class | Key | Default TTL |
|---|---|---|
| `InsufficientCapacity` | (vendor, instance type, zone, capacity type) | 3 min |
| `Unsupported` | (vendor, instance type, zone) | 24 h; 10 min when the driver's own pre-launch check found it (`vendors.Error.PreLaunch`) |
| `QuotaExceeded` | (vendor, region, quota bucket): the bucket the vendor counts the launch against (the quota tracker's `Consumption` mapping); (vendor, region, capacity type) for a vendor without a bucket mapping; (vendor, capacity type) in every region when account-wide | 5 min; a NodeClaim of the vendor and region confirmed gone clears it |
| `DailyLimit` | (vendor, region), every offering | until the vendor's reset time; the next 00:00 UTC when the driver gives none (OCI documents none); 5 min when the error arrives within 5 min after 00:00 UTC. Deleting NodeClaims does not clear it |
| `ZoneUnusable` | (vendor, zone) | 10 min |

### Why a type or an offering is not offered

Every gate that removes a type, removes an offering or marks an offering
unavailable gives one **reason**: a code naming the gate and a detail with the
data it used (a quota bucket, a zone, a region, a cache class; never a driver
message or an account identifier). The codes are API: they are only added, never
renamed. `Catalog.Explain` returns them per type and offering from the same
evaluation `InstanceTypes` uses, so a report cannot disagree with what Karpenter
was given.

| Code | Level | What removed it | Detail |
|---|---|---|---|
| `NoPrice` | type | no active USD price above zero in scope | |
| `RegionNotAllowed` | type | every price row is in a region the class does not allow, or an inactive one | the regions with a row |
| `ZoneNotAvailable` | offering | a price row's zone is not one of the account's, Keeper marked it inactive, or the driver lists no zones for the region (facts not loaded yet, or the credentials cannot read it) | the zone ID, or the region |
| `IncompletePrice` | offering | the price delta is incomplete and `pricing.allowIncomplete` is false | zone ID, capacity type |
| `GPUManufacturer`, `GPUModelUnknown`, `GPUFractional`, `GPUArch`, `GPUNoDriverBranch`, `GPUNVSwitch`, `GPUVirtual` | type | [GPU types](#gpu-types) | |
| `NotLaunchable` | offering | the driver's offering lister says the type cannot launch in the zone | zone ID |
| `NoOffering` | type | every offering of the type was removed above | the dominant earlier code |
| `UnavailableCache` | offering | a live entry of the unavailable-offerings cache | its class (`InsufficientCapacity`, `Interrupted`, `Unsupported`, `QuotaExceeded`, `DailyLimit`, `ZoneUnusable`, `InvalidOffering`) and its expiry |
| `QuotaGate` | offering | the quota is known and has no headroom | the limiting bucket, the headroom and the need |
| `ImageGap` | offering | the class has no ready boot image in the region; **region-level, not per architecture**: a region is marked only when no architecture's image is ready, so a type of an architecture without an image can still be offered there | the region |
| `RegionNotReady` | offering | the class's vendor resources are not ready in the region | the region |
| `ZoneMissingResource` | offering | a required resource is missing in the zone | the zone |
| `Unrankable` | offering | the NodePool pins a selection profile and the type has no score | the profile |
| `VendorNotReady` | type | `GetInstanceTypes` leaves out the types of a vendor without `VendorReady` | the condition's reason |
| `NodePoolRequirement` | type | **Explain only**: the type's requirements do not intersect the NodePool's, by core's rule | the first label key that fails |
| `NodePoolPreFilter` | type, offering | the NodePool's requirement on architecture, instance type, region, zone or capacity type; safe to drop, core would too | the key |

`Unavailable*`, `QuotaGate`, the class gates and `Unrankable` are availability
reasons, not removals: the type is returned, its offering has `Available=false`.
The class-level report (logs, status) has no NodePool and so cannot show
`Unrankable`, `NodePoolRequirement` or `NodePoolPreFilter`.

What Karpenter's message does not tell: core drops a type that has no available
offering compatible with the NodePool before it looks at a pod, and then says "no
instance type met all requirements" for a type that was never returned, for one
whose offerings are all unavailable and for one whose labels do not match the
pod's or the NodePool's requirements alike. It does say "had the required
resources" for a resource misfit. SCarpenter cannot see what Karpenter removes
after `GetInstanceTypes` (requests that do not fit the allocatable, `minValues`,
NodePool limits, NodeOverlays, reserved capacity); `Explain` reproduces the two
that are pure functions of the type: the requirement intersection
(`NodePoolRequirement`) and the resource fit for a request list
(`TypeReport.FitsRequests`).

**The label to key on** is the instance type name,
`node.kubernetes.io/instance-type`: `<vendor>.<api_reference>` with characters
outside the label charset replaced and a name over 63 characters cut to 56 plus a
hash. Every type has it and it is unique across vendors. The
`scarpenter.sparecores.com/api-reference` label carries the raw reference, only
when it is a valid label value that sanitising leaves unchanged, and is not
vendor-qualified; a NodePool that pins it to a value matches nothing for any
other type, with no message.

**Logs.** At V(1) (core's `--log-level=debug`: the chart value `karpenter.logLevel: debug`, or `LOG_LEVEL=debug`) a restart logs, for each class, every type and offering that has a
reason, one line per vendor, reason and 50 types
(`offered types changed`, with `nodeclass`, `vendor`, `reason`, `count` and
`types` with the detail in parentheses). Afterwards a line appears when a
type's or offering's code changes (`type`, `zone`, `capacityType`, `reason`,
`detail`, `was`; the reason `Available` means the code cleared), at most 50 changes
per report, and a larger burst is logged per reason again. Nothing is logged per
query. `GetInstanceTypes` adds a line per NodePool for `Unrankable` and
`NodePoolRequirement` (`nodepool` is set) and one for the vendors that are not
ready (`VendorNotReady`), each once per change. Every `Unsupported` mark of the
launch path is logged at V(0) (`marked the offering unsupported`, with the vendor's
`code` and `preLaunch`).

### The offered summary in the status

`kubectl describe scnodeclass` shows, per vendor, `status.vendors.<vendor>.offered`:
a bounded summary of what the catalog hands to Karpenter for the class (no
NodePool filter, no pinned selection profile, so the class-level codes only).

| Field | Meaning |
|---|---|
| `types` | instance types offered |
| `offerings`, `availableOfferings` | (region, zone, capacity type) offerings of the offered types, and how many are available now |
| `dropped` | per reason code, how many types are not offered at all (`NoPrice`, `RegionNotAllowed`, the `GPU*` codes, `NoOffering`) |
| `unavailable` | per reason code, how many offerings of the offered types are not available, listed unavailable (`UnavailableCache`, `QuotaGate`, `ImageGap`, `RegionNotReady`, `ZoneMissingResource`) or left out (`ZoneNotAvailable`, `IncompletePrice`, `NotLaunchable`) |
| `dropped[].types`, `unavailable[].types` | up to five instance type names with the code, in name order |
| `observedAt` | when the summary last changed, to the minute |

The block is at most a few KiB however large the catalog is: there is no
per-type or per-offering data in it (the details of a reason, a quota bucket or
a cache expiry, are in the logs and the listing). The controller `offered-types`
evaluates a class at most every 30 s, writes with server-side apply under the
field manager `scarpenter-offered`, the blocks of all vendors in one apply,
and only when a count or a name changed. It reports nothing for a vendor until
its driver facts are loaded (before that every offering of it would look like
`ZoneNotAvailable`; a vendor whose facts keep failing holds back no other), and
nothing while the catalog has no data for the class. `VendorNotReady` is not in the block: the vendor's conditions say it.

### The offered listing

For a class with the annotation `scarpenter.sparecores.com/publish-offered: "true"` the controller writes one ConfigMap per vendor,
`scarpenter-offered-<class>-<vendor>` (cut to 253 characters with a hash), in its own namespace (`POD_NAMESPACE`), owned by the
SCNodeClass, with the labels `app.kubernetes.io/managed-by: scarpenter`, `scarpenter.sparecores.com/scnodeclass: <class>` (left out when
the class name is longer than a label value) and `scarpenter.sparecores.com/vendor: <vendor>`. The annotation is not part of the spec:
it changes no hash. Without the annotation, or when the vendor leaves the class, the ConfigMap is deleted. The ConfigMap has no `data`: the key `types.json.gz` of its `binaryData` holds the gzip of this JSON document
(deterministic: no time in the gzip header). To read it:

```sh
kubectl -n <ns> get cm scarpenter-offered-<class>-<vendor> -o jsonpath='{.binaryData.types\.json\.gz}' | base64 -d | gunzip | jq
```

The document:

```json
{
  "generatedAt": "2026-11-21T00:00:00Z", "class": "bench", "vendor": "oci", "truncated": false,
  "types": {
    "oci.VM.Standard4.Ax.Flex.16-64": {
      "apiReference": "VM.Standard4.Ax.Flex.16-64", "available": false, "capacityTypes": [],
      "reason": "UnavailableCache", "detail": "DailyLimit until 2026-11-22T00:00:00Z",
      "offerings": [
        {"capacityType": "on-demand", "zones": ["eu-frankfurt-1-AD-1", "eu-frankfurt-1-AD-2", "eu-frankfurt-1-AD-3"], "available": false,
         "reason": "DailyLimit", "until": "2026-11-22T00:00:00Z"}
      ]
    }
  }
}
```

| Field | Meaning |
|---|---|
| `generatedAt` | when the listing was written: on every change and at least every 10 minutes, so it trails by about 10 minutes at most while the controller runs and has the vendor's driver facts; treat one older than 30 minutes as stale |
| `truncated` | always `false`, kept for readers that index it: no type is ever left out |
| `zonesOmitted` | only when `true`: the listing was over the 800 KiB cap (compressed), so every offering has an empty `zones` list; types, capacity types, availability, reasons and `until` are all kept |
| `types` | by instance type name (`node.kubernetes.io/instance-type`, `<vendor>.<server id>`): every type of the catalog, offered or dropped at build time |
| `apiReference` | the value of the `scarpenter.sparecores.com/api-reference` label; omitted when the type has no such label |
| `available`, `capacityTypes` | at least one offering is available; the capacity types that have one |
| `reason`, `detail` | only when `available` is false: the dominant class-level [reason code](#why-a-type-or-an-offering-is-not-offered) (the one of the most offerings, those Karpenter is given before the left-out ones, so a gated type says `QuotaGate`) and its data as text for humans, never to be parsed |
| `offerings` | grouped by capacity type, state, reason and expiry, with the zones that share them; empty for a type dropped at build time (its `reason` is the build's) |
| `offerings[].reason` | for an offering a launch error marked (reason code `UnavailableCache`), the launch-error class: `InsufficientCapacity`, `Interrupted`, `Unsupported`, `QuotaExceeded`, `DailyLimit`, `ZoneUnusable` or `InvalidOffering`; for any other unavailable offering the reason code (for example `QuotaGate`) |
| `offerings[].until` | RFC3339 time (UTC) the mark expires, only on a group whose `reason` is a launch-error class. A planner skips the offering until then without launching |

The cap is 800 KiB of compressed bytes (a ConfigMap holds 1 MiB). The controller writes the full document when it fits; otherwise the one
without zones (`zonesOmitted`), logging an error; when even that does not fit it writes nothing for that vendor (the others are still
written) and fails with an error naming the class, vendor and size, so the old ConfigMap ages out and readers treat it as stale. No
available type is ever dropped. The AWS catalog over all 33 regions of the test fixtures is about 31 KiB compressed;
`scarpenter_offered_listing_bytes` shows the size of each listing.

Quota is evaluated per region and capacity type, so the reasons of the offerings of one type can differ by zone. A type that is
available in some zones is `available: true`; its blocked zones are in `offerings`. The class-level view has no NodePool, so no
`Unrankable`, `NodePoolRequirement` or `NodePoolPreFilter`. How to read it:
[Which types does a class offer](../how-to-guides/which-types-does-a-class-offer.md). A reader needs `get`, `list` and `watch` on the
ConfigMaps in a Role of the release namespace (optionally with `resourceNames`) bound to its ServiceAccount; the chart grants nothing to readers.

## Labels

Every key is defined on every instance type or, for the offering-level keys, on every offering, with `DoesNotExist` where the value
is unknown or nothing applies: an undefined key would match every type
([00a section 2](../plans/v1/wip/00a-node-options.md#2-what-karpenter-allows)).
Labels are stamped on the NodeClaim and copied to the Node by core.

### Unknown values

| Fact | When it is `DoesNotExist` |
|---|---|
| any nullable Keeper field | Keeper has no value (`null`) or an empty one |
| a numeric value that rounds to 0 | never: a positive value is at least 1 |
| `cpu-cores`, `hyperthreading` | the core count is unknown |
| `cpu-flag-<flag>` | the type's flag list is empty (not inspected); a measured type without the flag is `false` |
| `accelerator-*` | the type has no accelerator |
| `nvidia-driver-*` | the type is not an NVIDIA GPU type the catalog keeps |
| `local-storage-count` | the type has a disk of unknown count; no disk is `0` |
| metric labels | the type has no score |
| `selection` | the type is in no profile |
| location labels | Keeper has no value |

`local-storage-gb` is `0` for a type without local storage, so `Lt 1` selects
diskless types.

### Scheduling and topology labels

| Label | Level | Source |
|---|---|---|
| `node.kubernetes.io/instance-type` | type | [name](#names) |
| `kubernetes.io/arch` | type | `cpu_architecture` (`x86_64` is `amd64`) |
| `kubernetes.io/os` | type | `linux` |
| `karpenter.sh/capacity-type` | type (all), offering (one) | the price's allocation |
| `topology.kubernetes.io/region` | type (all), offering (one) | region `api_reference` |
| `topology.kubernetes.io/zone` | type (all), offering (one) | the driver's zone name |
| `scarpenter.sparecores.com/zone-id` | type (all), offering (one) | zone `zone_id` |

### Attribute labels

Type-level. Integers are rounded to the nearest; use `Gt` and `Lt` for ranges
(2 to 16 vCPUs is `vcpus Gt 1` and `vcpus Lt 17`).

| Label | Keeper field | Conversion |
|---|---|---|
| `vendor` | vendor id | |
| `api-reference` | `api_reference` | only when it is a valid label value |
| `family` | `family` | sanitised |
| `vcpus` | `vcpus` | |
| `cpu-cores` | `cpu_cores` | |
| `memory-mib` | `memory_amount` | MiB |
| `memory-per-vcpu-mib` | `memory_amount` / `vcpus` | |
| `cpu-manufacturer`, `cpu-family`, `cpu-allocation` | `cpu_manufacturer`, `cpu_family`, `cpu_allocation` | sanitised: `-` for other characters |
| `cpu-clock-mhz` | `cpu_speed` (GHz) | x 1000 |
| `cpu-l2-per-core-kib` | `cpu_l2_cache` | KiB per core |
| `cpu-l3-mib` | `cpu_l3_cache_total` (else `cpu_l3_cache`) | KiB / 1024 |
| `hyperthreading` | `vcpus` > `cpu_cores` | `true` / `false` |
| `cpu-flag-<flag>` | `cpu_flags` | `true` / `false`, for the flags in `spec.catalog.cpuFlagLabels` |
| `accelerator-count` | `accelerator_count` | |
| `accelerator-memory-mib` | `accelerator_memory_total` | MiB |
| `accelerator-manufacturer`, `accelerator-model`, `accelerator-type` | `accelerator_manufacturer`, `accelerator_model`, `accelerator_type` | |
| `nvidia-driver-branch`, `nvidia-driver-version`, `nvidia-driver-flavor` | `pkg/nvidia` table | the driver of the NodeClass's Flatcar release for the model ([GPU types](#gpu-types)) |
| `network-baseline-mbps`, `network-peak-mbps` | `network_speed_baseline`, `network_speed_max` (Gbps) | x 1000 |
| `local-storage-gb` | `storage_size` | GB |
| `local-storage-count` | number of `storages` | |

The flags with a label are the closed, curated set of the `cpuFlagLabels` enum
(defaults: `avx2`, `avx512f`, `amx_tile`, `sha_ni`).

### Metric labels

Defined by `spec.metrics` of the SCNodeClass ([API](api.md)): a benchmark (id and
config, resolved against Keeper's `/benchmark_configs`) or a workload profile.
For a metric `<m>`:

| Label | Level | Value |
|---|---|---|
| `metric-<m>` | type | the score, multiplied by `scale` and rounded to an integer |
| `metric-<m>-pct` | type | the percentile 0 to 100, 100 best |
| `metric-<m>-per-usd` | offering | the scaled score divided by the offering's USD per hour |

- Scores belong to the type, not to a price: a type that has only spot prices
  gets metric labels too.
- `normalize: PerVcpu` divides the score by the vCPU count before everything
  else.
- The percentile is across all types of **all vendors of the NodeClass**
  (whatever their regions: scores do not depend on the region). A type beats the
  types with a worse score; ties share the lower rank; the best type is 100 and
  the worst 0. For lower-is-better benchmarks (`membench:latency`, ...) the
  direction is flipped so that 100 is still the best.
- `metric-<m>-per-usd` is `DoesNotExist` for lower-is-better benchmarks, where a
  score per dollar means nothing.
- Unbenchmarked types get `DoesNotExist` for all three labels, so any threshold
  excludes them: "fast single core" is `metric-single-core-pct Gt 89`.
- A metric the data source cannot answer for a reason of the metric itself (an
  unknown benchmark config) is `DoesNotExist` on every type and reported by
  `Catalog.Invalid` for the NodeClass controller.
- Benchmark scores are read with one `/servers` query per benchmark and vendor,
  without regions.

### Selection profiles

`spec.selections` hold sc-karpenter-nodepool's `selector` and `ranking` objects
verbatim; the semantics are those of its
[selector reference](https://github.com/SpareCores/sc-karpenter-nodepool/blob/main/docs/reference/selectors.md).
On every refresh each profile is evaluated by the selection engine of
`sc-keeper-go` per vendor of the NodeClass, over the vendor's allowed regions
(all regions when it lists none), on the on-demand basis unless the selector says
otherwise. Each type gets `selection In [<profiles it belongs to>]`, or
`DoesNotExist`. A profile the engine rejects (for example a ranking without a
band or `families.max`) has no members and is reported by `Catalog.Invalid`,
naming the vendor; each vendor is evaluated on its own, so an invalid vendor does
not affect the others' members. A profile without a `ranking` is priced at the
real price when pinned.

A NodePool that pins **exactly one** profile with a non-`Price` objective gets
its offerings priced by the objective, so scheduling, `Create` and
consolidation all optimise it. The price becomes a number whose lowest value is
the best:

| Objective | Price of an offering |
|---|---|
| `ScorePerPrice` | price / score (price x score for a lower-is-better benchmark) |
| `Score` | 1 / score (score for a lower-is-better benchmark) |
| `Price` | the real price |

The score is the profile's ranking score (normalised as the ranking says). A type
without a score cannot be ranked, so its offerings are unavailable. The real price
stays in the `price` annotation and metrics (plan 04).

### Location labels

Offering-level, from Keeper's region table. Unknown is `DoesNotExist`.

| Label | Source |
|---|---|
| `country` | `country_id`, lower case |
| `continent` | a static country to continent map (the UN geoscheme: Russia is Europe; Turkey, Cyprus, Georgia, Armenia and Azerbaijan are Asia; Mexico and the Caribbean are North America): `africa`, `asia`, `europe`, `north-america`, `south-america`, `oceania`, `antarctica` |
| `city`, `state` | `city`, `state`, as slugs (`New York` is `new-york`) |
| `green-energy` | `green_energy`: `true` or `false`; unknown (Keeper's data is tri-state) is `DoesNotExist` |

`city`, `state` and `green_energy` are fields of sc-keeper-go's `Region`
(`region` file of the snapshot, nullable).

### Profile labels

`storage-profile`, `network-profile` and `placement-profile` are `In [default,
<profiles valid for the type>]` on every type. The driver decides validity
through its `Supported` hook, given the profile blocks of the NodeClass for the
vendor (a profile without a block for the vendor is not offered to the driver and
does not exist on it; [node settings](node-settings.md#which-profiles-a-type-can-host)).
The type carries the union over its zones; an offering whose zone supports fewer
profiles carries its own list, so a profile a zone cannot host never matches
there. `placement-group` is a free-form run key and is not defined on types.

### Registration with Karpenter core

All keys are registered with core as well-known labels so that NodePools and pods
may use them. Core reads the set when its controllers are built and the set is an
unlocked global map, so keys are never registered at runtime:

- the fixed keys and the `cpu-flag-<flag>` keys of the whole curated set are
  registered at start-up;
- `metric-<m>`, `-pct` and `-per-usd` are registered by
  `Operator.RegisterLabelKeys`, which lists the SCNodeClasses with the API reader
  and runs first in `WithCoreControllers`, before core's controllers exist;
- **a key core does not know is never stamped**: core checks offerings with
  `IsCompatible`, which rejects an offering that defines a key it does not know
  (`label does not have known values`), so an unregistered `metric-<m>-per-usd`
  would exclude every benchmarked type; type requirements are only intersected,
  which ignores unknown keys, so an unregistered type-level key would be
  unselectable. A metric added after start-up is therefore not stamped until a
  restart has registered it; `Catalog.RestartRequired(class)` lists its keys, and the
  NodeClass controller reports them as `RestartRequired=True`.

## The catalog

### Scope

The catalog reads the **union** of the scopes of all SCNodeClasses: for each
vendor the union of the allowed regions (every active region as soon as one
class lists none). A NodePool sees the vendors it allows that its NodeClass
configures, in the class's regions; the pre-filter narrows further. When the
union changes, a refresh starts at once.

### Refresh and staleness

- A refresh reads one pinned Keeper data version: `/healthcheck` pins the
  `database_hash`, then the manifest `/snapshot/{hash}` and the files of that
  hash the request needs: the global `vendor` table; per vendor in scope
  `region`, `zone`, `server`, `server_price`, `storage_price`, `ipv4_price`; one
  `benchmark_score/{vendor}/{benchmark_id}` per vendor and benchmark that a metric
  or selection uses. `/benchmark_configs` is the one other live read, and only
  when metrics or selections exist. Files are streamed into rows (never held
  uncompressed) and filtered locally; scores are matched to the resolved
  benchmark config byte for byte. A metric without any matching score row is an
  invalid metric. A data version that Keeper's replica no longer serves
  (`snapshot-not-available`) is re-pinned once to the replica's current one.
- With `--keeper-cache-dir` the gzip files are kept per hash and checked against
  the manifest's SHA-256: a restart within the same version asks Keeper for
  `/healthcheck` only, and a new version fetches only the files that changed. When
  the hash and the request equal the current catalog's, a refresh stops after
  `/healthcheck`.
- It runs every `--catalog-refresh-interval` (default 1 h) with 10 % jitter, at
  start-up, and when the scope, a metric or a selection changes. After a failure
  it retries within a minute.
- **Stale-while-error**: a failed refresh keeps the last good snapshot and counts
  in `scarpenter_catalog_refresh_errors_total`. `Catalog.Status(class)` reports
  `Ready` until the snapshot is older than `--catalog-max-age` (default 24 h);
  then `Stale`, which the NodeClass controller turns into `CatalogReady=False`
  with the last error. A class the snapshot does not cover yet is `ScopePending`.
- **Cold start**: without any snapshot the status is `NoSnapshot` (`KeeperAPIKeyRequired` when Keeper answered 401, see [Keeper API key](../how-to-guides/install.md#keeper-api-key)) and
  `GetInstanceTypes` returns nothing for the class, so pods stay pending instead
  of nodes being launched blind.
- The largest file is Azure's `server_price` (295k rows); it is decoded as a
  stream, see the memory numbers in the plan.

### The pre-filter

`Catalog.InstanceTypes(query)` narrows the result by the NodePool's requirements
on vendor, region, zone, architecture, capacity type and instance type (`In`,
`NotIn`, `Exists`, `DoesNotExist`). Dropping what they exclude is safe because
core would drop it anyway; the tests prove that what core keeps of the
pre-filtered list equals what it keeps of the unfiltered one, offering by
offering. Results are cached per generation (snapshot, driver facts epoch,
sequence of the unavailable cache) and filter; a handed-out type is never
modified.

### Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_catalog_age_seconds` | gauge | `vendor` | seconds since the snapshot was read |
| `scarpenter_catalog_refresh_errors_total` | counter | | failed refreshes |
| `scarpenter_catalog_instance_types` | gauge | `vendor` | types with a price in scope |
| `scarpenter_catalog_offerings` | gauge | `vendor` | price rows in scope, before the driver's zone mapping |
| `scarpenter_catalog_dropped_types` | gauge | `nodeclass`, `vendor`, `reason` | types a NodeClass's catalog leaves out at build time (`NoPrice`, `RegionNotAllowed`, `NoOffering`, the [GPU reasons](#gpu-types)), by [reason](#why-a-type-or-an-offering-is-not-offered); exported for the NodeClasses the catalog has built, only for reasons that occur |
| `scarpenter_catalog_offerings_by_state` | gauge | `nodeclass`, `vendor`, `state` | offerings of the offered types: `available`, or the offering-level reason code (at most nine series); written by the offered types controller |
| `scarpenter_offered_listing_bytes` | gauge | `nodeclass`, `vendor` | compressed bytes of the last offered listing ConfigMap written (cap 800 KiB) |
| `scarpenter_instance_type_offering_available` | gauge | `nodeclass`, `instance_type`, `zone`, `capacity_type` | 1 available, 0 unavailable; only with `--metrics-offering-detail` (thousands of series per class and vendor) |

### Scale

Measured on the committed snapshot (2026-11-06, Keeper database
`d320364be39310dc02b1fd3019d69fca27edf32d`), AWS only, on a 32-thread desktop
CPU; see `pkg/catalog/bench_test.go` (`go test ./pkg/catalog -bench BuildCatalog
-benchmem`):

| Scope | Types | Offerings | Build time | Allocated per build | Retained |
|---|---|---|---|---|---|
| all 33 active regions | 1,382 | 134,727 | about 104 ms | 216 MB | 63 MiB |
| three regions | 1,375 | 26,299 | about 50 ms | 80 MB | 28 MiB |

"Retained" includes the loaded data. Reading the data from Keeper dominates in
production: 138,970 price rows for AWS in the snapshot.

## Troubleshooting

- *Pods stay pending with no instance types*: `Catalog.Status` for the class:
  `NoSnapshot` (Keeper unreachable since start), `ScopePending` (refresh not done
  yet), `Stale`. Check `scarpenter_catalog_refresh_errors_total` and the
  controller log (`catalog refresh failed`).
- *A pod requiring a metric label is never scheduled*: the type is unbenchmarked
  (`DoesNotExist`), or the metric was added after start-up (`RestartRequired`).
- *A pinned type gets "no instance type met all requirements"*: raise the log
  level to V(1) and restart; the log names the type and its
  [reason](#why-a-type-or-an-offering-is-not-offered). A type that is not named was
  returned with every offering available: check the pin (key on the instance type
  name, not `api-reference`) and the NodePool's own requirements (a metric or
  selection label the type lacks, a profile the type has no score under).
- *An instance type is missing*: its Keeper status is not `active` or `planned-for-retirement`, its
  architecture is unsupported, it has no active Linux hourly USD price in the
  allowed regions, or the driver does not report its zones.
- *A selection profile has no members*: `Catalog.Invalid` names the reason (an
  unknown benchmark config, a ranking without a band or `families.max`).

## References

- [Overview section 5](../plans/v1/wip/00-overview.md#5-catalog-keeper-data-to-instance-types-and-offerings) and [00a sections 6.4 and 7](../plans/v1/wip/00a-node-options.md#7-instance-selection)
- [ADR-008, ADR-009 and ADR-019](../explanation/DECISIONS.md)
- [Labels and annotations](labels.md), [Controller reference](controller.md)
- [sc-keeper-go](https://github.com/SpareCores/sc-keeper-go) and the
  [selector reference](https://github.com/SpareCores/sc-karpenter-nodepool/blob/main/docs/reference/selectors.md)
