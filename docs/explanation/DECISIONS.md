# Decisions (ADRs)

Short architecture decision records. Status is one of **accepted**,
**proposed** (waiting for the maintainer), **deferred**, or **superseded**.

## ADR-001: Our own multi-vendor Karpenter provider (accepted, 2026-09-24)

SCarpenter is one Karpenter `CloudProvider` with pluggable vendor drivers. It
uses the Spare Cores catalog for instance types, offerings and prices, Flatcar
and Ignition for the OS, and a generic kubelet join, and targets self-managed
clusters of any distribution.

Why: of the surveyed providers (September 2026: aws, azure, gcp, two alibaba,
three hetzner, oci, ovhcloud, upcloud, vultr), only the Hetzner providers
support self-managed clusters out of the box; the others are tied to their
vendor's managed Kubernetes (EKS, AKS, GKE, ACK, OKE, MKS, UKS). Running
several Karpenter providers in one cluster is not a supported setup either.

Rejected: provisioning VMs explicitly with an orchestrator and Pulumi (no
Karpenter semantics, per-instance state); a hybrid of upstream providers and
custom code (two code paths, upstream providers still need patching for
non-managed clusters); Cluster API + cluster-autoscaler (an infrastructure
provider per vendor, several missing; MachineDeployments per instance type).

Consequences: a significant Go project that tracks Karpenter core releases; in
return price-driven placement across vendors, capacity-error fallback,
immediate removal of empty nodes, orphan GC and a reusable open-source
component.

## ADR-002: Flatcar from official disk images, configured by Ignition (accepted, 2026-09-24)

Nodes run Flatcar Container Linux, pinned per `SCNodeClass` (channel +
version), booted from disk images and configured by Ignition in user-data.
Components (kubelet, tailscale, later NVIDIA drivers) are systemd sysexts from
Flatcar's sysext-bakery, pinned by URL and sha256. Vendors with official
Flatcar images (AWS, GCP, Azure) use them; for the others SCarpenter will
import the official image (a later plan, before the first such vendor).

Why: one kernel and userspace on every vendor; minimal, immutable,
fast-booting; nothing that depends on services or images only Spare Cores can
publish.

Rejected: vendor Ubuntu + cloud-init (slow boots, per-vendor kernels, fragile
install steps); iPXE/PXE (needs a boot server, RAM-rooted OS); kexec from a
stock image (quirks, RAM root); our own published images (not shareable across
accounts at several vendors); Talos (API-driven configuration, opinionated
join).

## ADR-003: Generic kubelet TLS bootstrap join (accepted, 2026-09-24)

Nodes join with kubelet TLS bootstrap: SCarpenter mints a bootstrap token per
NodeClaim (TTL 1 h, deleted after registration), Ignition writes the bootstrap
kubeconfig, the built-in CSR approver signs client certificates (via
ClusterRoleBindings in the chart), and SCarpenter approves kubelet-serving
CSRs for its own nodes. CNI and kube-proxy are the cluster's DaemonSets.

Why: the primitive under k3s agent joins and `kubeadm join` alike; one tested
path for every distribution.

Rejected: distribution-specific joins (k3s agent, kubeadm) as the default
(would make SCarpenter distribution-specific or need several tested paths).
A `kubeadm` join mode may be added later for kubeadm clusters.

## ADR-004: Vendor drivers are in-tree Go packages (accepted, 2026-11-05)

Each vendor is a package under `pkg/vendors/<vendor>` implementing the
`Driver` interface (overview §7), registered at init and enabled by
configuration. Optional behaviour (firewalls, image import, quotas,
NodeClass validation) is expressed as capability interfaces discovered by type
assertion. One binary, one Go module. Development drivers (`fake`, `qemu`) are
compiled only with the `scarpenter_dev` build tag.

Why: simplest to build, test and release; typed contract; the core never
imports a vendor SDK; a conformance suite makes adding a vendor mechanical.

Rejected: nested Go modules per vendor (isolates SDK dependencies but adds
release bookkeeping; reconsider if the binary or dependency graph becomes a
problem); out-of-process gRPC plugins (third-party drivers without forking,
but many more moving parts, versioned wire contract, extra processes).

## ADR-005: Keeper client in a shared module, `sc-keeper-go` (accepted, 2026-11-05)

The Keeper API client and its snapshot-replay test server move from
`sc-karpenter-nodepool/internal/keeper` into the public module
[`github.com/SpareCores/sc-keeper-go`](https://github.com/SpareCores/sc-keeper-go)
(MPL-2.0), imported by both projects.

Why: one implementation of retries, `Retry-After`, `data_version` pinning,
region chunking and replay testing; versioned; usable by others.

Rejected: importing `sc-karpenter-nodepool` after moving the code to `pkg/`
(couples SCarpenter to that project's release cycle and dependencies);
copying (two copies drift).

## ADR-006: License MPL-2.0 (accepted, 2026-11-05)

SCarpenter is MPL-2.0, like `sc-karpenter-nodepool` and `sc-keeper-go`.
Karpenter core and the AWS provider are Apache-2.0; importing them is fine.
Code adapted from them keeps an attribution comment naming the source file at
a pinned commit. (An earlier design note said Apache-2.0; the committed
license decides.)

## ADR-007: Provider ID from the NodeClaim name (accepted, 2026-11-05)

`providerID = scarpenter://<vendor>/<region>/<nodeclaim>`. The vendor's
instance ID is kept in a NodeClaim annotation and in vendor tags.

Why: the provider ID must be in the kubelet configuration, which is rendered
before the instance exists. Using the NodeClaim name needs no vendor metadata
service at boot, works the same for every vendor, and keeps user-data
identical across launch attempts.

Rejected: vendor-native IDs (`aws:///<zone>/<instance-id>`) filled in at boot
from the metadata service (afterburn): would need per-vendor boot logic and
metadata support that not every target vendor has; compatibility with
vendor cloud-controller-managers is a non-goal.

## ADR-008: Instance-type names are `<vendor>.<api_reference>` (accepted, 2026-11-05)

`node.kubernetes.io/instance-type` is `aws.m7i.large`, `hcloud.cx22`, etc.,
sanitised to the label charset and hashed when longer than 63 characters. The
raw name is the `scarpenter.sparecores.com/api-reference` label.

Why: one cluster spans vendors, so names must be unique across vendors; the
prefix makes the vendor visible wherever the type appears.

Cost: NodePools written for the AWS provider (`m7i.large`) must be adapted;
selecting by `scarpenter.sparecores.com/api-reference` plus `vendor` gives the
raw names back.

## ADR-009: Zones are matched by vendor zone ID (accepted, 2026-11-05)

Keeper stores AWS zones by their portable zone ID (`use1-az1`); zone names are
account-specific. Drivers map Keeper zone IDs to their own account's zone
names. `topology.kubernetes.io/zone` carries the account's name (what other
tools in that account use), `scarpenter.sparecores.com/zone-id` the ID.

## ADR-010: Test strategy L1–L5 (accepted, 2026-11-05)

L1 unit/golden with replayed Keeper data, L2 envtest, L3 real Karpenter core
in kind with a fake driver that creates KWOK nodes, L4 real Flatcar VMs in
QEMU joining a local k3s through a `qemu` development driver, L5 real AWS with
a k3s control plane on the CI runner joined over Tailscale (overview §13).

Why: L3 tests our Karpenter semantics at full catalog scale for free; L4 tests
the riskiest part (Ignition, sysexts, TLS bootstrap, CSR approval) on every
bootstrap change without a cloud bill; L5 is the only layer that needs
credentials and money, and it also validates the tailscale template.

Rejected: L5 on the `direct` template with a control plane in the same VPC
(simpler secrets, but leaves the tailscale path untested); moto/LocalStack for
the AWS driver (an in-memory fake behind the driver's EC2 interface is
simpler and enough for L1; L5 is the real check).

## ADR-011: v1 scope (accepted, 2026-11-05)

v1 = the vendor-neutral core (including quota-aware placement, ADR-013) + the
development drivers + vendor drivers, **AWS first**. Further vendors are added
as later v1 plans on the same driver contract; vendors without official
Flatcar images are preceded by an image-import plan (`SCImage`,
`ImageImporter`).
Not planned yet: spot interruption handling, per-node tailnet keys, GPU
extended resources (an optional AWS GPU plan exists). These are designed for
(capability interfaces, API room) but not built. Spot prices are not fetched
from vendor APIs at all: Keeper's spot price database is kept up to date
(overview §5.2).

## ADR-012: Pinned Flatcar and Kubernetes versions (accepted, 2026-11-05)

`spec.flatcar.version` and `spec.kubernetes.version` are required and pinned;
changing them drifts the NodeClass's nodes. No "latest" resolution in v1.

Why: deterministic nodes (essential for benchmarking, useful for everyone);
drift happens only when a person changes the spec, never because upstream
published a release. Kubernetes version skew against the API server is
checked and reported as a condition.

## ADR-013: Quota-aware placement from vendor quota APIs (accepted, 2026-11-05)

Drivers whose vendor exposes quotas implement `QuotaReporter` (buckets with
limit and usage per region and account-wide, and which buckets each instance
type and capacity type consumes) and, where available, `OfferingLister`
(which types the account can launch in which zone). A quota tracker refreshes
them every couple of minutes, keeps an in-flight ledger of launches not yet
visible in usage, and makes offerings without headroom **unavailable**
(overview §5.9). Consequences:

- Flexible requirements (several regions or capacity types allowed): nodes
  are placed only where the account has quota; price decides among those.
- A single allowed region without headroom: nothing is launched and the pods
  stay Pending until a refresh shows headroom. This is Karpenter's normal
  handling of pods without a compatible offering; SCarpenter reports it as
  `QuotaAvailable=False` on the NodeClass (`QuotaZero` when the limit is 0).
- Usage covers the whole account (other workloads use the same quota), not
  only SCarpenter's instances.

Why: sc-inspector's experience (Azure pre-flight checks of regional and
family vCPU usage and SKU restrictions; regions ordered by headroom before
price) showed that try-and-fall-back burns hours on regions that cannot
succeed and cannot tell "no quota now" from "no quota at all".

Rejected: reactive-only fallback with a TTL (kept only for vendors without a
quota API, and for capacity errors, which no quota API predicts); launching
in a pinned region and failing repeatedly (wastes API calls, noisy, and
looks like a capacity problem); reading AWS usage from CloudWatch `AWS/Usage`
metrics (minutes of lag, another service; usage is computed from
`DescribeInstances` instead).

## ADR-014: One SCNodeClass can serve many vendors (accepted, 2026-11-05)

An `SCNodeClass` holds shared settings plus a `vendors.<vendor>` block per
vendor; NodePools select vendors with a `scarpenter.sparecores.com/vendor`
requirement (one NodePool per vendor recommended, for per-vendor limits and
budgets). Drift hashes and readiness are per vendor. Replaces "one NodeClass
serves exactly one vendor" in the overview. Details:
[`00a-node-options.md`](../plans/v1/wip/00a-node-options.md) §4.

Why: one place for the shared node definition (OS, Kubernetes, bootstrap,
storage and network defaults, profiles, policy) across eight vendors; no
NodePool lists instance types.

Rejected: one NodeClass per vendor (eight copies of shared settings to keep in
sync); one NodePool for all vendors as the recommended layout (loses
per-vendor limits, budgets and weights, and scans every vendor's types per
pod).

## ADR-015: Workloads choose through labels: exact choices, selection, profile names, run keys (accepted, 2026-11-05)

Pods choose with nodeSelector/affinity on well-known label keys that
SCarpenter registers in a restricted domain: exact instance, vendor, region,
zone and capacity type; selection labels (ADR-019); **profile names**
(`storage-profile`, `network-profile`, `placement-profile`); and the
placement **run key**. Workloads never set vendor parameters directly: those
live in admin-defined profiles (ADR-020). Enumerable keys are defined on every
instance type (`In [valid values for this type]`, `DoesNotExist` where none),
so the scheduler itself rejects unavailable or unknown choices; the run key is
the only free-form, undefined key. `Create` reads the values from the
NodeClaim requirements and stamps concrete labels back. Details: `00a` §2, §3,
§5.

Why: it is the only channel from pods to `Create` that Karpenter core
supports, it works in every object with a pod template, and it is the
established provider pattern (AWS tenancy and placement partition, Azure ultra
SSD, GCP local SSD).

Rejected: annotations read in `Create` (core never passes pods or their
annotations to the provider); standalone NodeClaims created by the workload
(no disruption, repair, limits or budgets); scalar per-pod parameters such as
a disk size or type label (they would need a cross-vendor meaning, see
ADR-020); one NodeClass per option combination (combinatorial explosion).

## ADR-016: Annotation translation controller (deferred, 2026-11-05)

Proposed first: an optional controller that expands one pod annotation
(`scarpenter.sparecores.com/instance: aws/us-east-1/m7i.large` plus options)
into nodeSelector, tolerations, `do-not-disrupt` and anti-affinity behind a
scheduling gate.

Deferred because the maintainer's need is "whatever is easily possible", not
annotations specifically: plain labels in pod templates (nodeSelector,
affinity) already express every choice, work in every higher-level object
(Job, Deployment, StatefulSet, Helm charts), and need no component in the
pod-creation path. Invalid choices are rejected by the scheduler because
options are defined on instance types (ADR-015). The controller can be added
later as sugar. Details: `00a` §8.

## ADR-017: Never degrade an option silently (accepted, 2026-11-05)

A profile or option a vendor or instance type cannot honour makes the
offering infeasible: the pod stays Pending (or picks another vendor if it
allowed several). No best-effort substitution: no plan swap for a bigger root
disk, no cluster placement without co-location, no profile applied "as close
as possible". A profile without a block for a vendor does not exist on that
vendor. What was applied is stamped on the node (profile names, or `default`).

Why: for benchmarks a silently different node is a wrong result; for other
users, an explicit Pending is easier to diagnose than a node that is not what
was asked for.

## ADR-018: Vendor resources are brought by the user or managed with an explicit lifecycle (accepted, 2026-11-05)

For every kind of vendor resource a node depends on (resource groups,
networks, subnets, routes, security groups/firewalls, placement constructs,
separate volumes, public IPs, NICs), the NodeClass vendor block says either
`use` (pre-created: explicit IDs per region, a `{region}`/`{zone}` name
pattern, or a tag selector; validated, never modified or deleted) or `create`
(SCarpenter creates it on demand or ahead of time, with a deterministic name,
an ownership marker, adoption of its own leftovers, and a lifecycle policy:
`perUse`, `idleTTL` or `retain`). A kind with neither defaults to `create`;
managed shared infrastructure defaults to `idleTTL` 24 h. Details: `00a` §9.

Why: operators who manage their infrastructure as code (one Pulumi stack
creating networks and keys in every region) must be able to hand those to
SCarpenter; everyone else needs a zero-configuration route. Lifecycles make
the trade-off between creation latency, churn and leaked resources explicit
instead of hard-coding one.

Rejected: create-only (no infrastructure-as-code integration); use-only (every
user would have to pre-create resources in every region); deleting shared
resources as soon as the last node of a region goes (network churn on bursty
workloads; available as `perUse` where wanted).

## ADR-019: sc-karpenter-nodepool's selection built in (accepted, 2026-11-05)

Instance selection by description, not only by name, at three levels, all as
label requirements (details: `00a` §7):

1. **Attribute labels** on every instance type (vCPUs, memory, CPU
   manufacturer/family/clock/caches/flags, accelerators, network, local
   storage), `DoesNotExist` when unknown, so ranges like "2–16 vCPUs" are plain
   `Gt`/`Lt` requirements.
2. **Metric labels** from NodeClass-defined benchmark metrics: the score, its
   percentile across the NodeClass's catalog, and score per USD on each
   offering. "Fast single core" is `metric-single-core-pct Gt 89`.
3. **Selection profiles** using sc-karpenter-nodepool's `selector` and
   `ranking` objects verbatim; membership is stamped as `selection In [...]`.
   A profile's objective orders candidates in `Create`, and prices the
   offerings of a NodePool that pins the profile, so scheduling and
   consolidation optimise the same objective.

The selection engine and its API types move into `sc-keeper-go` (package
`selection`) and are shared with sc-karpenter-nodepool.

Why: the maintainer's workloads describe needs ("fast single core", "a
benchmark number", "rank by score per price"); the same data and semantics
already exist in sc-karpenter-nodepool and should not be reimplemented.

Rejected: running sc-karpenter-nodepool next to SCarpenter to generate
NodePools with instance-type lists (a second controller and NodePools that
list types, against R1 of `00a`); ranking only inside `Create` (the scheduler
and consolidation would still optimise raw price); one label per benchmark
and config in the catalog (hundreds of labels per node; metrics are chosen
per NodeClass instead).

## ADR-020: Vendor-native node settings: defaults, overrides and explicit profiles (accepted, 2026-11-05)

Node settings whose meaning differs between vendors (disk types and their
tunables, extra volumes, NIC features, placement constructs, tenancy) are
configured only in the vendor's own terms: built-in per-vendor defaults
(documented), overridden by the NodeClass vendor block, overridden by named
profiles that hold an explicit block per vendor. Generic settings exist only
where the meaning is identical everywhere (public IPv4/IPv6 on/off, inbound
CIDR rules, guest-side filesystem and mount path, the placement run key).
Details: `00a` §3, §6.

Why: a cross-vendor "class" (standard/balanced/fast/extreme) maps to
different performance on each vendor and silently makes nodes non-comparable.
The easy route stays easy (defaults), and the exact route is exact: whoever
needs io2 with 32000 IOPS writes exactly that for AWS, and decides
explicitly what the other vendors get.

Rejected: generic storage classes and numeric hints resolved per vendor
(ambiguous mapping, the original 00a proposal); per-pod vendor parameters
(R9 of `00a`: workloads must not bypass the admin's configuration).
