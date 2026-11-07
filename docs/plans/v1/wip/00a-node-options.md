# 00a — Node options, instance selection and vendor resources

Status: **accepted** (2026-11-05; ADR-014, ADR-015 and ADR-017 to ADR-020
accepted, ADR-016 deferred). Its decisions are folded into
[`00-overview.md`](00-overview.md) and the plan sequence; this document is the
detailed design of node options, instance selection and vendor resources, and
decides where the overview is shorter.

It covers what a workload needs to say about its node, and what the node
needs from the vendor account:

- **which instance**: an exact one, or one chosen by attributes, benchmark
  scores and a ranking (sc-karpenter-nodepool's selectors, built in);
- **how the node is set up**: disks, network, placement, tenancy, location;
- **which vendor resources it uses**: networks, subnets, firewalls, resource
  groups, placement groups, volumes — pre-created by the user, or created by
  SCarpenter on demand.

Facts per vendor: [`../../../explanation/vendor-capabilities.md`](../../../explanation/vendor-capabilities.md).
Karpenter mechanics were traced in core v1.15.1 source; file references are to
`kubernetes-sigs/karpenter` at that tag.

## Table of Contents

- [1. Requirements](#1-requirements)
- [2. What Karpenter allows](#2-what-karpenter-allows)
- [3. Principles and model](#3-principles-and-model)
- [4. One SCNodeClass, many vendors](#4-one-scnodeclass-many-vendors)
- [5. Option registry and the driver capability contract](#5-option-registry-and-the-driver-capability-contract)
- [6. Node options](#6-node-options)
- [7. Instance selection](#7-instance-selection)
- [8. Workload interface](#8-workload-interface)
- [9. Vendor resources: bring your own, or managed](#9-vendor-resources-bring-your-own-or-managed)
- [10. Pricing](#10-pricing)
- [11. Device identity for Ignition](#11-device-identity-for-ignition)
- [12. Spare Cores data: use what exists](#12-spare-cores-data-use-what-exists)
- [13. Impact on the v1 plans](#13-impact-on-the-v1-plans)
- [14. Decisions](#14-decisions)

## 1. Requirements

From sc-inspector (the first user) and generic multi-cloud use:

| # | Requirement |
|---|---|
| R1 | NodePools admit every instance type of every vendor without listing types; catalog changes need no NodePool edits. |
| R2 | A workload can demand an exact vendor, instance type, region, zone and capacity type; nothing is ever substituted. |
| R3 | A workload can instead describe what it needs: attribute ranges (vCPUs 2–16, memory, CPU features, GPUs), benchmark thresholds or relative needs ("fast single core"), and a ranking (score, price, score per price). |
| R4 | Node setup has an easy route and an exact route: **vendor defaults**, or **explicit vendor-native settings** (EBS io2 with given IOPS, ESSD PL2, hyperdisk-balanced with provisioned throughput, …). No ambiguous cross-vendor mapping: a vendor without explicit settings uses its defaults, and that is visible. |
| R5 | Per-node choices a workload makes: disk setup, network setup, placement group, tenancy, location. |
| R6 | All of this works from any pod template: Pod, Job, Deployment, StatefulSet, Helm charts (e.g. a PostgreSQL StatefulSet and its client Deployment in one placement group). |
| R7 | One pod per node and no disruption mid-run, when asked for. |
| R8 | **Never a silent degradation**: what a node was asked to be, it is, or it is not launched. |
| R9 | No workload can widen the security posture (open inbound ports, pick networks or keys) or exceed what the admin configured. |
| R10 | Every vendor resource a node depends on can be **pre-created by the user** (referenced by ID, name or tags, per region), or **created by SCarpenter on demand** with an explicit lifecycle. |

## 2. What Karpenter allows

Traced in core v1.15.1:

- The **only** channel from a pod to `CloudProvider.Create` is its scheduling
  requirements (nodeSelector, node affinity). Annotations and pod references
  never reach the provider (`pkg/controllers/state/cluster.go` keeps the pod ↔
  NodeClaim mapping in memory only).
- Label keys the provider registers as well-known can be used in pod
  requirements (`pkg/scheduling/requirements.go`,
  `AllowUndefinedWellKnownLabels`). If an instance type defines the key, the
  pod's requirement is matched against it (`In`, `NotIn`, `Exists`,
  `DoesNotExist`, integer `Gt`/`Lt` stored as `Gte`/`Lte`). If no instance type
  defines it, any value passes and reaches `NodeClaim.Spec.Requirements`.
  **An undefined key matches every type**, so every selectable key must be
  defined on every instance type, with `DoesNotExist` where it does not apply
  (as the AWS provider does for GPU labels on non-GPU types).
- Offerings can carry extra requirement keys; pod requirements are checked
  against them, so per-offering values (score per dollar) are selectable.
- Core does not stamp well-known keys as node labels: `Create` returns a
  concrete label for every requested key, or kube-scheduler never binds the
  pod. Non-well-known keys in NodePool requirements get random `Any()` values
  (`nodeclaimtemplate.go`) and must be avoided.
- Pods with different values of a key never share a NodeClaim; a pod that
  does not mention the key can still join (exclusivity needs a taint or
  anti-affinity).
- A NodeClaim lists at most the **600 cheapest** compatible instance types
  (`MaxInstanceTypes`); `Create` chooses among them.
- Label values: ≤ 63 characters, `[A-Za-z0-9_.-]`; numeric comparisons are
  integer-only.
- A NodePool has one `nodeClassRef`; limits, budgets and weights are per
  NodePool.
- An exact instance-type selector yields a NodeClaim with exactly that type,
  or a Pending pod.
- `instanceTypeNotFound` drift replaces nodes whose type vanished from the
  catalog: long benchmarks carry `karpenter.sh/do-not-disrupt: "true"`.
- `CreateError` retries until the liveness timeout: invalid choices must be
  rejected by the scheduler (§5.2), never in `Create`.

## 3. Principles and model

**Generic only where the meaning is identical on every vendor.** Generic:
vendor, region, zone, location, hardware attributes, benchmark metrics,
capacity type, public IPv4/IPv6 on or off, inbound CIDR rules, and the
placement **run key**. Everything else — disk types and their tunables, extra
volumes, NIC features, placement constructs, tenancy — is configured in the
vendor's own terms (ADR-020). There are no cross-vendor "classes".

Configuration layers, each overriding the previous one, per vendor:

| Layer | Where | Holds | Who sets it |
|---|---|---|---|
| 1. Built-in defaults | the driver (documented per vendor in the reference) | a sensible node per vendor: root disk type and size, network settings | SCarpenter |
| 2. NodeClass vendor block | `SCNodeClass.spec.vendors.<v>.{storage,networking,…}` | overrides of the defaults for every node of that vendor | admin |
| 3. Profiles | `SCNodeClass.spec.profiles.{storage,network,placement}.<name>.<v>` | named, explicit, vendor-native setups that workloads opt into | admin |
| 4. Labels | pod nodeSelector / affinity | exact instance, selection ranges, **profile names**, placement run key | workload |

Rules:

- A workload never sets a vendor parameter directly; it picks a profile by
  name. Profiles are the admin's explicit, reviewed configurations (R9).
- A profile applies to the vendors it has a block for. On a vendor without a
  block the profile **does not exist**: workloads that require it cannot land
  there (R8). The default route (no profile) always exists and means layers 1
  and 2.
- Every node carries labels saying what was applied: the profile names, or
  `default` (§6).

## 4. One SCNodeClass, many vendors

Replaces the original overview's "one NodeClass serves exactly one vendor" (ADR-014):

```yaml
apiVersion: scarpenter.sparecores.com/v1alpha1
kind: SCNodeClass
metadata: {name: bench}
spec:
  flatcar: {channel: stable, version: "4757.2.3"}
  kubernetes: {version: "v1.37.1"}
  network: {template: tailscale, tailscale: {...}}
  networking: {publicIPv4: true, publicIPv6: false, inbound: {rules: []}, isolation: run}
  metrics:                                  # §7.2
    single-core: {benchmark: {id: "geekbench:score", config: {cores: single}}}
  selections:                               # §7.3, sc-karpenter-nodepool's selector + ranking
    fast-single-core:
      selector: {vcpus: {min: 2}, price: {maxHourly: "0.50"}}
      ranking:
        score: {benchmark: {id: "geekbench:score", config: {cores: single}}}
        objective: Score
        band: {withinPercent: 12}
        families: {min: 3, max: 6}
  vendors:
    aws:
      regions: []
      storage:                              # layer 2: overrides the built-in AWS defaults
        root: {volumeType: gp3, volumeSize: 128Gi, iops: 3000, throughput: 125, encrypted: true}
      resources:                            # §9
        subnets: {selector: {tags: {scarpenter.sparecores.com/use: "true"}}}
        securityGroups: {selector: {tags: {scarpenter.sparecores.com/use: "true"}}}
        placementGroups: {create: {lifecycle: perUse}}
    gcp:
      regions: [europe-west3]
      storage: {root: {diskType: pd-balanced, sizeGb: 128}}
    hcloud:
      regions: [fsn1, nbg1, hel1]           # root disk is fixed by the server type: nothing to override
  profiles:
    storage:
      db-io2:
        aws:
          root: {volumeType: gp3, volumeSize: 64Gi}
          data:
            - {name: pgdata, volumeType: io2, volumeSize: 500Gi, iops: 32000, filesystem: xfs, mountPath: /var/lib/pgdata}
        gcp:
          root: {diskType: hyperdisk-balanced, sizeGb: 64}
          data:
            - {name: pgdata, diskType: hyperdisk-extreme, sizeGb: 500, provisionedIops: 32000, filesystem: xfs, mountPath: /var/lib/pgdata}
        hcloud:
          data:
            - {name: pgdata, size: 500, filesystem: xfs, mountPath: /var/lib/pgdata}
    placement:
      colocated:
        aws: {placementGroup: {strategy: cluster}}
        alicloud: {deploymentSet: {strategy: LowLatency}}
        gcp: {resourcePolicy: {groupPlacement: {collocation: COLLOCATED}}}
      spread:
        aws: {placementGroup: {strategy: spread}}
        hcloud: {placementGroup: {type: spread}}
        ovh: {serverGroup: {policy: anti-affinity}}
      dedicated:
        aws: {tenancy: dedicated}
    network:
      ena-express:
        aws: {enaExpress: {enabled: true, udp: true}}
```

NodePools (one per vendor recommended, for per-vendor limits and budgets):

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: {name: bench-aws}
spec:
  template:
    spec:
      nodeClassRef: {group: scarpenter.sparecores.com, kind: SCNodeClass, name: bench}
      requirements:
        - {key: scarpenter.sparecores.com/vendor, operator: In, values: [aws]}
      taints: [{key: scarpenter.sparecores.com/dedicated, effect: NoSchedule}]   # optional
  disruption: {consolidationPolicy: WhenEmpty, consolidateAfter: 0s}
  limits: {cpu: "512"}
```

- `GetInstanceTypes(nodePool)` returns the types of the vendors the NodePool
  allows and the NodeClass configures. No instance-type list anywhere (R1).
- Vendor blocks and profile blocks are typed per vendor (Go structs per
  driver, CRD schema generated per vendor key), validated by CEL and by the
  driver's `ValidateNodeClass` against the catalog (e.g. Keeper's storage
  min/max sizes and IOPS limits).
- Drift hash per vendor (shared fields + that vendor's block + its blocks in
  the profiles); readiness per vendor, so a broken vendor makes only its
  offerings unavailable.

## 5. Option registry and the driver capability contract

### 5.1 Option specs

Every workload-selectable key is registered at start-up as a well-known label
in a restricted domain (typos are rejected). Kinds: `Enum` (profile names —
the NodeClass knows them), `Int`, `Bool`, `Name` (free slug: the placement run
key only).

### 5.2 Feasibility on instance types

Every enumerable key is **defined on every instance type** by the catalog:

- `storage-profile In [default, <profiles with a valid block for this vendor and type>]`,
  likewise `network-profile` and `placement-profile`;
- the driver decides validity per type and zone (`Supported`): e.g. a GCP
  profile using `hyperdisk-extreme` is valid only on series that support it,
  an AWS `cluster` placement profile not on T types, an hcloud data volume
  profile in every location with block storage.

A workload asking for a profile a type cannot host finds no compatible type:
the pod stays Pending with Karpenter's normal scheduling event, and no launch
is attempted (R8, R9).

### 5.3 Driver capability

```go
type NodeOptions interface {
    // Typed vendor block and profile block schemas, defaults, validation.
    Defaults() VendorSettings
    Validate(ctx context.Context, s VendorSettings) error
    // Which profiles (by kind) are valid for this instance type and zone.
    Supported(it CatalogType, zone string, profiles Profiles) map[string][]string
    // Price of the resolved settings for an offering, and whether it is complete (§10).
    PriceDelta(s VendorSettings, o Offering) (usdPerHour float64, complete bool)
    // Resources needed before the instance exists (§9), returning device hints (§11).
    Prepare(ctx context.Context, req PrepareRequest) (*Prepared, error)
}
```

`Create` resolves the vendor settings (defaults ← NodeClass vendor block ←
selected profiles) and passes them with `Prepared` to `CreateInstance`.

## 6. Node options

Prefix `scarpenter.sparecores.com/` omitted.

### 6.1 Storage

Vendor-native settings only (field names follow each vendor's API):

| Vendor | Root disk settings | Extra volumes | Built-in default (proposal, finalised in each driver plan) |
|---|---|---|---|
| aws | `volumeType`, `volumeSize`, `iops`, `throughput`, `encrypted`, `kmsKeyID` | in the create call | gp3, 64 GiB, encrypted |
| gcp | `diskType`, `sizeGb`, `provisionedIops`, `provisionedThroughput`, `kmsKeyName` | in the create call | pd-balanced, 64 GB; hyperdisk-balanced on Hyperdisk-only series (the one type-dependent default, documented) |
| azure | `storageAccountType`, `diskSizeGB`, `diskIOPSReadWrite`, `diskMBpsReadWrite`, ephemeral OS disk placement | in the create call; PremiumV2/Ultra as separate disks | Premium_LRS, 64 GiB |
| alicloud | `category`, `size`, `performanceLevel`, `provisionedIops`, `burstingEnabled`, `encrypted` | in the create call | cloud_essd PL0, 64 GiB |
| upcloud | `tier`, `size`, `encrypted` | in the create call | maxiops, 64 GB |
| ovh | flavor disk; `bootFromVolume: {volumeType, size}` | in the create call (block device mapping) | flavor disk |
| hcloud | — (fixed by server type) | separate volumes (`size`, `format`) | server type disk |
| vultr | — (fixed by plan) | separate block storage (`blockType`, `sizeGb`) | plan disk |

Every extra volume also has `name`, `filesystem` and `mountPath` (generic:
they describe the guest, not the vendor). Local instance storage:
`localStorage: {policy: ignore | mount | raid0, mountPath}` per vendor block or
profile, valid only on types with local disks.

Labels: `storage-profile` (selectable; `default` when none), stamped on every
node.

### 6.2 Networking

Generic (identical meaning everywhere), NodeClass-level, overridable per
vendor block:

```yaml
networking:
  publicIPv4: true
  publicIPv6: false
  inbound: {rules: []}                  # CIDR rules; empty = deny all (a deny firewall is attached where the vendor default is open)
  isolation: none | nodeclass | run     # who may reach a node privately
```

Vendor-native in vendor blocks and network profiles: ENA Express and EFA
(aws), NIC type and Tier_1 (gcp), accelerated networking (azure), internet
bandwidth and charge type (alicloud), interfaces (upcloud). The resources a
node is attached to (networks, subnets, firewalls) are covered in §9.

Labels: `network-profile` (selectable; `default` when none). Inbound rules,
networks and firewalls are never workload-selectable (R9).

### 6.3 Placement and tenancy

| Label | Values | Notes |
|---|---|---|
| `placement-profile` | profile name, `default` | which native construct, in the vendor's terms (cluster placement group, LowLatency deployment set, anti-affinity server group, dedicated tenancy, …) |
| `placement-group` | run key (free slug) | all pods with the same key and profile share one native group |

A profile that creates groups (placement groups, deployment sets, resource
policies, server groups) needs a run key; one that does not (e.g.
`aws: {tenancy: dedicated}`) ignores it. The native group is created on
first use per (vendor, region, profile, run key), or referenced if the profile
names an existing group (§9). Group size limits (hcloud 10, AWS spread 7 per
zone, Alibaba 20, GCP compact 22) are tracked; a full group makes further
offerings for that key unavailable. Zonal constructs need all members in one
zone: the pods carry the same zone requirement or a zone pod affinity (§8.2).

### 6.4 Location

Stamped on offerings from Keeper's region and country tables:

| Label | Example | Source |
|---|---|---|
| `country` | `de` | region `country_id`, lower-case |
| `continent` | `europe`, `north-america` | country → continent name, slugified |
| `city`, `state` | `frankfurt` | slugified; `DoesNotExist` when unknown |
| `green-energy` | `true` | `DoesNotExist` when unknown (tri-state data) |

## 7. Instance selection

The selection power of [sc-karpenter-nodepool](https://github.com/SpareCores/sc-karpenter-nodepool)
is built in, at three levels; all are label requirements, combinable with each
other and with exact choices.

### 7.1 Attribute labels

Every instance type carries labels for the hardware facts Keeper has, defined
on every type, `DoesNotExist` when unknown:

| Label | Kind | Example |
|---|---|---|
| `vcpus`, `cpu-cores` | int | `8` |
| `memory-mib`, `memory-per-vcpu-mib` | int | `32768` |
| `cpu-manufacturer`, `cpu-family`, `cpu-allocation` | enum | `AMD`, `EPYC`, `Dedicated` |
| `cpu-clock-mhz` | int | `3700` |
| `cpu-l2-per-core-kib`, `cpu-l3-mib` | int | `1024`, `256` |
| `hyperthreading` | bool | `false` |
| `cpu-flag-<flag>` | bool, for the flags in NodeClass `catalog.cpuFlagLabels` (default: a curated set such as `avx2`, `avx512f`, `amx_tile`, `sha_ni`) | `true` |
| `accelerator-count`, `accelerator-memory-mib` | int | `1`, `24576` |
| `accelerator-manufacturer`, `accelerator-model`, `accelerator-type` | enum | `NVIDIA`, `L4`, `GPU` |
| `network-baseline-mbps`, `network-peak-mbps` | int | `12500` |
| `local-storage-gb`, `local-storage-count` | int | `474`, `1` |
| `family`, `api-reference`, `vendor` | enum | `m7i`, `m7i.large`, `aws` |

"2–16 vCPUs" is `vcpus Gt 1` and `vcpus Lt 17`. The labels are also stamped on
the node, so a node describes its own hardware.

### 7.2 Metric labels (benchmark scores)

The NodeClass defines **metrics**: a benchmark (id + config, resolved against
Keeper's `/benchmark_configs` as sc-karpenter-nodepool does) or a workload
profile, optionally normalised per vCPU. For each metric `<m>`:

| Label | Scope | Value |
|---|---|---|
| `metric-<m>` | instance type | the score as an integer (a per-metric `scale`, e.g. ×100, keeps precision) |
| `metric-<m>-pct` | instance type | percentile 0–100 among the catalog types of the NodeClass's vendors (100 = best, also for lower-is-better benchmarks) |
| `metric-<m>-per-usd` | **offering** | score per USD/hour of that offering |

Unbenchmarked types get `DoesNotExist`, so any threshold excludes them.
Metric keys are registered with Karpenter core at start-up from the
NodeClasses that exist then; a metric added later needs a controller restart,
which the NodeClass reports as `RestartRequired` (core's well-known label set
cannot change while its controllers run).
Examples: "fast single core" is `metric-single-core-pct Gt 89`; an explicit
number is `metric-single-core Gt 1999`.

### 7.3 Selection profiles

For set-level logic labels cannot express — ranking with a band of families,
a minimum number of families as capacity fallback, data-quality policy — the
NodeClass defines **selection profiles** with sc-karpenter-nodepool's
`selector` and `ranking` objects verbatim. Each catalog refresh evaluates
them, and each type gets `selection In [<profiles it belongs to>]`
(`DoesNotExist` for none). As in sc-karpenter-nodepool, the band decides which
types are acceptable and Karpenter picks the cheapest acceptable one; a
profile's `objective` also orders the candidates in `Create`. When a NodePool
pins exactly one selection profile with a non-`Price` objective, its
offerings are priced by that objective (e.g. price ÷ score), so scheduling,
`Create` and consolidation all optimise it; the real price stays in the
`price` annotation and metrics.

### 7.4 Reuse

sc-karpenter-nodepool's selection engine and its `Selector` / `Ranking` API
types move into `sc-keeper-go` (package `selection`), shared by both projects
(plan 01, extended).

### 7.5 Scale

About 25 attribute labels, three per metric, one membership label and three
profile labels per instance type; the L3 scale scenario (overview §5.7)
measures them with the full AWS catalog.

## 8. Workload interface

### 8.1 Labels in pod templates

The interface is standard Kubernetes scheduling, in every object with a pod
template (R6); no SCarpenter component sits in the pod-creation path.

Exact instance, one pod per node, no disruption:

```yaml
apiVersion: batch/v1
kind: Job
metadata: {name: bench-m7i-large}
spec:
  template:
    metadata:
      labels: {app: bench}
      annotations: {karpenter.sh/do-not-disrupt: "true"}
    spec:
      restartPolicy: Never
      nodeSelector:
        scarpenter.sparecores.com/vendor: aws
        node.kubernetes.io/instance-type: aws.m7i.large
        topology.kubernetes.io/region: us-east-1
        karpenter.sh/capacity-type: on-demand
      tolerations: [{key: scarpenter.sparecores.com/dedicated, operator: Exists}]
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - {topologyKey: kubernetes.io/hostname, labelSelector: {matchLabels: {app: bench}}}
      containers: [...]
```

Described instead of named (any vendor in Europe, 2–16 vCPUs, top 10 %
single-core, cheapest):

```yaml
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - {key: scarpenter.sparecores.com/continent, operator: In, values: [europe]}
            - {key: scarpenter.sparecores.com/vcpus, operator: Gt, values: ["1"]}
            - {key: scarpenter.sparecores.com/vcpus, operator: Lt, values: ["17"]}
            - {key: scarpenter.sparecores.com/metric-single-core-pct, operator: Gt, values: ["89"]}
```

### 8.2 A database and its client in one placement group

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: pg}
spec:
  serviceName: pg
  replicas: 1
  selector: {matchLabels: {app: pg, run: pg-bench-1}}
  template:
    metadata: {labels: {app: pg, run: pg-bench-1}}
    spec:
      nodeSelector:
        node.kubernetes.io/instance-type: aws.r7i.2xlarge
        karpenter.sh/capacity-type: on-demand
        scarpenter.sparecores.com/placement-profile: colocated
        scarpenter.sparecores.com/placement-group: pg-bench-1
        scarpenter.sparecores.com/storage-profile: db-io2
      containers: [...]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: pg-client}
spec:
  replicas: 1
  selector: {matchLabels: {app: pg-client, run: pg-bench-1}}
  template:
    metadata: {labels: {app: pg-client, run: pg-bench-1}}
    spec:
      nodeSelector:
        node.kubernetes.io/instance-type: aws.c7i.xlarge
        karpenter.sh/capacity-type: on-demand
        scarpenter.sparecores.com/placement-profile: colocated
        scarpenter.sparecores.com/placement-group: pg-bench-1
      affinity:
        podAffinity:          # same zone as the server
          requiredDuringSchedulingIgnoredDuringExecution:
            - {topologyKey: topology.kubernetes.io/zone, labelSelector: {matchLabels: {app: pg, run: pg-bench-1}}}
        podAntiAffinity:      # never on the server's node
          requiredDuringSchedulingIgnoredDuringExecution:
            - {topologyKey: kubernetes.io/hostname, labelSelector: {matchLabels: {run: pg-bench-1}}}
      containers: [...]
```

Both nodes go into one AWS cluster placement group (created on the first
launch, deleted when both are gone, §9). Karpenter resolves the zone affinity
for pending pods; pinning `topology.kubernetes.io/zone` on both is the
deterministic alternative. There is no gang scheduling: if one node cannot
launch, the other runs anyway. Run keys should be unique per run.

### 8.3 Errors

An unavailable or disallowed choice leaves the pod Pending with Karpenter's
scheduling event. SCarpenter adds a diagnostic Event on the NodeClass naming
the label and the reason when pending pods use SCarpenter keys no type
satisfies.

### 8.4 Optional sugar (deferred)

A controller that expands a short annotation into these labels (ADR-016) is
deferred: labels already work everywhere.

## 9. Vendor resources: bring your own, or managed

### 9.1 What a node depends on

Per vendor, a launch needs some of: resource group (azure), network/VPC,
subnet/vSwitch, route table and gateway, security group / firewall / NSG,
placement construct, separate volumes, public IP / NIC (azure), SSH key
objects (only where a vendor requires one at create time; keys themselves go
into Ignition). The vendor table is in
[vendor-capabilities](../../../explanation/vendor-capabilities.md#prerequisite-resources).

### 9.2 Per resource kind: `use` or `create`

Each vendor block has `resources.<kind>` with exactly one of:

```yaml
vendors:
  aws:
    resources:
      subnets:
        use:                                     # bring your own (never modified, never deleted)
          selector: {tags: {scarpenter.sparecores.com/use: "true"}}   # per region, evaluated in every region in scope
      securityGroups:
        use:
          ids: {us-east-1: [sg-0123], eu-central-1: [sg-0456]}       # explicit per region
  azure:
    resources:
      resourceGroup:
        use: {name: "sc-{region}"}               # name pattern with {region} / {zone}
      network:
        create:                                  # managed by SCarpenter
          lifecycle: {policy: idleTTL, ttl: 24h}
          timing: onDemand                       # or preCreate: [westeurope, eastus]
```

- **`use`** — by explicit IDs per region, a name pattern (`{region}`,
  `{zone}` placeholders), or a tag/label selector. SCarpenter validates them
  per region (`status.vendors.<v>.resources`, condition `ResourcesReady`); a
  region where a referenced resource is missing has unavailable offerings and
  a clear reason. It never modifies or deletes them.
- **`create`** — SCarpenter creates the resource when a launch needs it, with
  a deterministic name (`scarpenter-<cluster>-<kind>-<region>[-<hash>]`) and
  an ownership marker (tags or labels, else name prefix plus description). It
  adopts an existing resource carrying both (e.g. after a restart) instead of
  creating a duplicate.
- **Neither** — `create` with the kind's default lifecycle below (the
  zero-configuration route; decided 2026-11-05). `forbid: true` opts a kind
  out of auto-creation: offerings needing it are then unavailable.
- **The vendor's own default resource** (AWS default VPC and its default
  subnets, GCP `default` network, Alibaba default VPC and vSwitches) is part
  of a driver's built-in defaults (§3, layer 1): where it exists and the kind
  has neither `use` nor `create`, the driver uses it as found, like `use`
  (never modified or deleted). Only where it does not exist does the default
  `create` apply.

### 9.3 Lifecycle of managed resources

| Policy | Deleted when | Default for |
|---|---|---|
| `perUse` | no instance uses it any more (after a 2-minute grace for in-flight launches) | per-node resources (separate volumes, Azure NICs/public IPs) and per-run resources (placement groups, run networks) |
| `idleTTL` (`ttl`) | no instance has used it for `ttl` | shared regional infrastructure: networks, subnets, routes, security groups, firewalls (default `ttl: 24h`) |
| `retain` | its NodeClass is deleted (or SCarpenter is uninstalled with cleanup) | Azure resource groups; anything the admin wants stable |

Timing:

- `onDemand` (default): the first launch in a region (or zone) that needs the
  resource creates it; that launch waits for it. Creation is serialised per
  (vendor, kind, region) and idempotent (find by name and marker, else
  create).
- `preCreate: [regions]`: the NodeClass controller creates the resources in
  those regions immediately and keeps them while the region stays listed
  (idle TTL does not apply to them); useful to take creation latency out of the
  first launch.

Decided in plan 06:

- A shared regional resource (one name per cluster, kind and region) is governed
  by every live NodeClass that maps to it: the most conservative lifecycle wins
  (`retain`, then `idleTTL` with the longest TTL, then `perUse`), and `preCreate`
  of any of them protects it.
- A resource that no live NodeClass maps to follows the NodeClass that created it
  while that exists, else the kind's default lifecycle; that is later than "deleted
  when its NodeClass is deleted" and is accepted.
- The idle time never counts from before the controller process first saw the
  resource (a restart restarts the clock).
- `{zone}` in a `use` name pattern: a region is unavailable only when no zone
  matches; a zone without a match is unavailable on its own.
- Backlog: placement-group size limits (§6.3), NodeClass Events for pending pods
  that use SCarpenter keys no type satisfies (§8.3), and the cleanup of managed
  resources on uninstall.

Deletion respects dependencies (instances → NICs/volumes → security groups →
subnets → networks → resource groups), retries with backoff, and is recorded
in Events and metrics. A resource that is still in use by something
SCarpenter does not own (the vendor refuses deletion) is left in place, marked
in status, and retried.

### 9.4 Interaction with placement and profiles

Placement profiles may reference a pre-created group instead of creating one
(`aws: {placementGroup: {use: {name: my-pg}}}`); then the run key only
separates NodeClaims and the group is never deleted. Storage profiles always
create their volumes (`perUse`); referencing existing volumes (snapshots,
datasets) is post-v1.

## 10. Pricing

Offering price = instance price + the price delta of the resolved settings:

- root and data volumes not bundled with the instance: Keeper storage
  GB-month price ÷ 730 per hour, for the exact vendor storage type;
- public IPv4 where billed: Keeper IPv4 price. (Verified 2026-11-07: Keeper has
  **no** IPv4 price table; its only IPv4 field is the number of addresses a server
  includes. The AWS driver uses a documented, configurable constant until Keeper and
  its crawler get an IPv4 price; backlog.)

`GetInstanceTypes` prices offerings with the NodeClass defaults (or by a
pinned selection objective, §7.3); `Create` re-orders candidates with the
selected profiles. Components missing from the data (provisioned
IOPS/throughput, dedicated tenancy, paid bandwidth) make the price
**incomplete**: the price excludes them and the NodeClaim gets
`scarpenter.sparecores.com/price-incomplete: "true"`. Such offerings are
allowed by default (decided 2026-11-05); `pricing.allowIncomplete: false` in
the NodeClass excludes them. Managed shared
resources (§9) are not attributed to offerings.

## 11. Device identity for Ignition

| Vendor | Strategy |
|---|---|
| gcp | name the device; `/dev/disk/by-id/google-<name>` |
| azure | choose the LUN; `/dev/disk/azure/scsi1/lun<N>` |
| hcloud | pre-create the volume; `/dev/disk/by-id/scsi-0HC_Volume_<id>` |
| vultr | pre-create the block volume; by-id from its ID (to verify) |
| aws, alicloud, upcloud, ovh | IDs exist only after create: a first-boot unit matches each volume by vendor serial pattern and size and creates the filesystem label |

Mounts use filesystem labels everywhere.

## 12. Spare Cores data: use what exists

Decision (2026-11-05): v1 does **not** depend on changes to sc-crawler or
Keeper. Where the data is missing, drivers carry small, documented,
version-controlled tables, and labels whose value is unknown get
`DoesNotExist`. Known gaps, for a later data round:

1. `compatible_storage_ids` is never filled (GCP Hyperdisk-only series,
   Alibaba per-family disk categories live in driver tables).
2. Storage rows lack bootable flags, provisionable IOPS/throughput ranges and
   their prices; `cloud_auto`, `pd-extreme` and `cloud_essd_entry` are
   missing or unpriced.
3. No per-server local disks for hcloud, vultr, upcloud and alicloud; no max
   data volumes per instance type.
4. No NIC capability flags; no network speed for gcp, azure, hcloud and
   upcloud.
5. No dedicated instance or host prices (incomplete prices, §10).
6. No continent codes (a static map in SCarpenter slugifies continent names).
7. No placement-construct support per instance family (driver tables).

## 13. Impact on the v1 plans

Folded into the plan sequence (overview §14):

- **Plan 01** (`sc-keeper-go`): also extract sc-karpenter-nodepool's selection
  engine and `Selector`/`Ranking` types; the benchmark queries metrics need.
  No changes to Keeper itself (§12).
- **Plan 02** (scaffold): `SCNodeClass` with `vendors` (typed per vendor),
  `profiles`, `metrics`, `selections`, `resources`, `catalog.cpuFlagLabels`.
- **Plan 03** (catalog): attribute, metric, selection, location and profile
  labels; per-NodePool vendor scoping; selection objective pricing.
- **Plan 06 (new): node options and vendor resources** — settings
  resolution, `NodeOptions`, `Prepare`, the `use`/`create` resource framework
  with lifecycles (§9), pricing deltas, the first-boot device unit; the fake
  driver implementing all of it; L3 scenarios: exact placement, exclusivity,
  ranges and percentiles, selection profiles, the two-workload placement group,
  a profile missing for a vendor stays Pending, `use` vs `create` and every
  lifecycle policy.
- **Plan 08** (AWS driver): AWS storage settings (root and data EBS,
  instance store), placement groups and tenancy profiles, ENA Express,
  resources for VPC/subnets/security groups/placement groups with `use` and
  `create`; EFA and dedicated hosts deferred.
- **Plan 10** (AWS e2e): the §8.2 database + client run, a described-not-named
  selection, and one run against pre-created resources (`use`).

## 14. Decisions

Answered by the maintainer on 2026-11-05:

| # | Question | Decision |
|---|---|---|
| 1 | Default lifecycle of managed shared infrastructure | `idleTTL` with `ttl: 24h` |
| 2 | A resource kind with neither `use` nor `create` | defaults to `create` (zero-configuration route) |
| 3 | Multi-vendor `SCNodeClass` | yes (ADR-014) |
| 4 | Metric percentiles | across all vendors of the NodeClass |
| 5 | Offerings with an incomplete price | allowed by default (flagged with `price-incomplete`) |
| 6 | Changes to the Spare Cores data | none for now; use what exists (§12) |
