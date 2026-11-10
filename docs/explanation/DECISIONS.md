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

Rejected: provisioning VMs explicitly with an orchestrator and an IaC tool (no
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

*Superseded in part by [ADR-021](#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10):
SCarpenter imports no images (the image ladder decides per vendor where the
image comes from), our own published images are accepted on vendors that let
anyone publish public images, and some vendors depend on images that Spare
Cores builds (GCP arm64, UpCloud, Vultr, Alibaba).*

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
`ImageImporter`). *Superseded in part by
[ADR-021](#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10): no
image-import plan precedes any vendor; the image ladder decides where each
vendor's images come from.*
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
`use` (pre-created: found by one selector,
[ADR-023](#adr-023-one-selector-for-every-vendor-resource-accepted-2026-11-10): IDs per
region, a `{region}`/`{zone}` name pattern, tags, portable attributes or the vendor's own
filters; validated, never modified or deleted) or `create`
(SCarpenter creates it on demand or ahead of time, with a deterministic name,
an ownership marker, adoption of its own leftovers, and a lifecycle policy:
`perUse`, `idleTTL` or `retain`). A kind with neither follows the resolution order of
[ADR-022](#adr-022-resolving-vendor-resources-explicit-discovered-default-created-or-a-clear-failure-accepted-2026-11-10)
(it first read "defaults to `create`"); managed shared infrastructure defaults
to `idleTTL` 24 h. Details: `00a` §9.

Amended by ADR-022 (what a kind without `use` or `create` resolves to) and by
ADR-023 (the forms of `use`: `ids`, `name` and `selector.tags` become the one selector
`ids`, `name`, `tags`, `match`, `filters`, `expression`, `select`, `orderBy`).

Why: operators who manage their infrastructure as code (one stack, in Pulumi,
Terraform or any other tool, creating networks and keys in every region) must be able to hand those to
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

## ADR-021: Flatcar images: the image ladder (accepted, 2026-11-10)

Every node boots the Flatcar version pinned in its NodeClass (ADR-012) from an
image the vendor can boot. Where that image comes from is decided per vendor by
the **first rung of this ladder that works**; "works" means the pinned channel
and version can be named and read back from the image (ADR-012, ADR-017):

1. **A public Flatcar image the vendor offers**: use it (AWS, Azure, GCP amd64).
2. **The vendor lets anyone publish public images**: Spare Cores publishes
   Flatcar images there, built and released from a separate GitHub repository
   (GCP arm64; Alibaba community images later, if Spare Cores is eligible).
3. **Images can be created in the user's own account**: pre-create them (per
   region where the vendor's images are regional) with infrastructure as code or a
   CI job of the operator's choice, and pass the IDs to SCarpenter (hcloud, OVH, UpCloud, Vultr, Alibaba). The source
   is Flatcar's official image for that platform; where Flatcar ships none, the
   image built by Spare Cores' images repository: the generic Flatcar image with
   the OEM id set (`vultr`, `upcloud`, `aliyun`).
4. **SCarpenter imports or builds images itself** (`SCImage`, `ImageImporter`):
   reserved, not built in v1. No vendor needs it.

Details:

- **Images repository.** A new Spare Cores repository (proposed name
  `SpareCores/flatcar-images`; plan 22). Per Flatcar release and channel it builds
  the OEM-patched images for vultr, upcloud and aliyun in the format each vendor
  imports, and the GCP arm64 image; it publishes the downloadable artifacts with
  sha256 sums and signatures (cosign keyless from GitHub Actions), publishes the
  GCP arm64 image publicly from a Spare Cores GCP project
  (`allAuthenticatedUsers`), and later Alibaba community images. It also proposes
  the missing platforms upstream to Flatcar; once Flatcar ships one, that vendor
  moves to the official image.
- **API.** One generic resource kind `images` in
  `spec.vendors.<vendor>.resources`, in the shape of ADR-018: `use` with the one
  selector of ADR-023 (`ids` per region, a `name` pattern, `tags` where the vendor has
  tags or labels on images, `match` attributes such as `architecture`, `owner` and
  `state`, the vendor's native `filters`, `select`, `orderBy`). There is no `create` (that would be rung 4). On rungs 1
  and 2, and on rung 3 where the vendor has a naming convention the driver can
  look up, `images` is optional: without it the driver resolves the pinned
  version itself. A vendor with global images lists the ID once under the
  region key `*` (every region in scope), or repeats it per region. The pinned version is always checked against the image's
  labels or name: an image of another version is not ready, never booted
  (ADR-017).
- **Hand-off.** The operator's IaC lists the images of each region as
  `resources.images.use.ids` of the NodeClass; nothing is needed on rungs 1 and 2
  ([hand-off contract](../reference/infrastructure-handoff.md#images)).
- **GCP arm64** is published by Spare Cores (rung 2), not left out of v1.
- **Order.** GCP and Azure (rung 1) first, then Hetzner, then OVH, UpCloud, Vultr
  and Alibaba (the order among these four is open).

Why: every vendor gets the most official image it can. A vendor's own public
image costs nothing and needs no maintenance by us; where only our own build
exists, publishing it once for everyone (rung 2) beats every user building it;
where nothing can be shared, the user's account holds a copy made by tooling that
already exists (IaC), which keeps credentials with write access to images out of
the controller. The reference per vendor is
[Flatcar images](flatcar-images.md).

Rejected: SCarpenter-side import in v1 (a CRD, a controller, image garbage
collection and write permissions on images in every account, for flows every
vendor can do without; the `ImageImporter` interface stays reserved); publishing
our own images everywhere (impossible where the vendor cannot share publicly:
Hetzner, UpCloud, Vultr, OVH); a vendor's own unpinned "Flatcar" entry as the
default (its version cannot be read back, so ADR-012 and ADR-017 are broken);
a per-vendor image field instead of one generic kind (every rung-3 vendor would
get its own variant of the same three lookups).

Consequences: supersedes ADR-002's sentence on importing the official image and
its rejection of "our own published images" (still rejected where the vendor
cannot share publicly, accepted on vendors that allow public images); amends
ADR-011's "preceded by an image-import plan": no plan precedes any vendor. The
images repository is a maintenance commitment: new Flatcar releases for up to
four platforms, signatures and publishing credentials for one GCP project.
Changing image IDs is not drift: the pinned version is (ADR-012).

## ADR-022: Resolving vendor resources: explicit, discovered, default, created, or a clear failure (accepted, 2026-11-10)

SCarpenter is open source and does not know how an operator provisions the
vendor account: Pulumi, Terraform or OpenTofu, a CLI script, the console, or
nothing. One contract serves all of them: the operator creates resources any way
they like and names them in the NodeClass with the one selector of
[ADR-023](#adr-023-one-selector-for-every-vendor-resource-accepted-2026-11-10) (ID, name
pattern, tags, portable attributes such as a CIDR, or the vendor's own filters)
(`spec.vendors.<vendor>.resources.<kind>.use`, see the
[hand-off contract](../reference/infrastructure-handoff.md)); what is not named,
SCarpenter finds or manages itself. SCarpenter's code, docs and plans do not
depend on any particular IaC project, its outputs or its layout.

For each kind, in each region, the resolution order is (steps 2 to 4 only for a kind
without `use`):

1. **Explicit `use`** (the selector of ADR-023), as given: validated,
   never modified or deleted.
2. **Discovery**: resources carrying the well-known opt-in tag or label
   `scarpenter.sparecores.com/use: "true"` in the region (encoded per vendor's
   key rules, as the other SCarpenter tags; vendors without tags or labels skip
   this step). Putting the tag on a resource is an explicit opt-in by the
   operator, so it wins over a vendor default. Treated as `use`: never modified. No
   tagged resource goes on to step 3; more than the kind's `select` allows fails
   (the operator's opt-in is ambiguous, step 5) rather than falling through.
3. **Vendor defaults** where the vendor has them and they are safe: the AWS
   default VPC and its default subnets, GCP's `default` network, Alibaba's
   default VPC and vSwitches. A default that cannot honour the requested
   settings (ADR-017: isolation, inbound rules, `publicIPv4: false`, IPv6 on a
   default firewall or network that has none) is skipped rather than degraded.
   Treated as `use`: never modified.
4. **Create** (managed, with the ADR-018 lifecycle), where the vendor and the
   credentials allow it (`forbid: true` removes this step).
5. **Fail gracefully**: the NodeClass reports `ResourcesReady=False`, reason
   `ResourcesUnavailable`, naming the kind, the region and what was tried; no
   launch is attempted for the region, and nothing half-created is left behind
   (a creation that fails midway is rolled back or adopted and finished by the
   next attempt, ADR-018).

An explicit `create` skips steps 2 and 3. An explicit `use` that matches nothing
fails; it never falls through to the next step. A kind that lives in a network
(subnets, security groups) is resolved after the network and only within it,
whichever step found the network. `images` has no step 4: its step 3 is the driver's
own lookup of the pinned version (ADR-021). A `policy` knob (for example to
forbid vendor defaults) is not added: `use`, `create` and `forbid` cover the
known cases, and one is proposed only when a real case needs it.

Today: the AWS driver does steps 1, 3, 4 and 5 (the e2e lane launches into the
default VPC); step 2 is new and so is the same order in the other drivers, added by
[plan 14](../plans/v1/wip/14-multi-vendor-groundwork.md) in the core and in
per-driver hooks, with conformance cases.

Why: the maintainer's rule is that every user can pre-provision with their own
IaC and pass the result in one documented way, and otherwise get something
working without configuration. Discovery by an explicit tag is the one place an
IaC tool that cannot render a NodeClass (a Terraform module that only creates
subnets) still reaches SCarpenter, and it must beat a vendor default because the
operator asked for it. Failing with a stated reason beats creating a surprise
resource that the credentials cannot manage.

Rejected: SCarpenter reading an IaC tool's state or outputs (couples the open
source code to one operator's layout); defaults before discovery (a tagged
resource would be ignored in favour of a shared default network); silently
degrading a request on a default resource (ADR-017); a `policy` field now.

Consequences: amends ADR-018, whose "a kind with neither defaults to `create`" is
replaced by this order. The discovery step is the selector `use: {tags:
{scarpenter.sparecores.com/use: "true"}}` with the kind's default `select`, built in (ADR-023). Discovery adds one list call per kind and region to the
resource check; the tag key is reserved for the opt-in.

## ADR-023: One selector for every vendor resource (accepted, 2026-11-10)

Every kind of vendor resource on every vendor is found with the same `use`
selector. It replaces ADR-018's three forms (`ids`, `name`, `selector.tags`, exactly
one of them) and can express every way the vendor's API offers to find the kind:

```yaml
use:
  ids: {<region or *>: [id, ...]}   # checked, and ANDed with the other fields
  name: "flatcar-{channel}-*"       # glob; exact without wildcards
  tags: {key: value, key2: "*"}     # "*" = the key exists; labels where there are no tags
  match:                            # portable attributes, one spelling on every vendor
    architecture: [amd64]
    owner: [self]
    state: [available]
    createdAfter: 2026-01-01T00:00:00Z
    cidr: 10.0.0.0/16               # also within, zone, network, description, includeDeprecated
  filters: [{name: root-device-type, values: [ebs]}]   # the vendor's own filters, passed through
  expression: "..."                 # the vendor's query language, where it has one
  select: exactlyOne                # exactlyOne | all | newest | oldest; default per kind
  orderBy: created                  # created | name | label:<key>, for newest and oldest
```

Rules. All given fields are ANDed, list values are ORed. Placeholders `{region}`,
`{zone}`, `{arch}`, `{vendorArch}`, `{channel}`, `{version}` and `{nodeclass}` are
expanded in every string value. Each driver declares, per kind, which `match`
attributes and which `filters` names it supports and whether the vendor or the
client evaluates each; an unknown or unsupported name is rejected when the NodeClass is
validated (CEL for the shape, the driver for the vendor), never ignored. The driver
sends to the vendor what it can and the shared matcher then re-applies every given
field to the answer, so a vendor-side filter may narrow to a superset (fuzzy and
substring matching, an API that ignores a parameter) and never to a subset; a filter a
vendor may silently ignore is offered only where it can be re-applied. Resolution is per
region: zero matches, or more than `select` allows, makes the region unusable with a
condition that says what excluded the rest (ADR-022 step 5); nothing is guessed. The
resolved IDs are in the NodeClass status and a launched node records what it used;
images stay pinned per node. An ID list that a vendor rejects as a whole when one ID
is missing (AWS `*.NotFound`) is looked up through the ID filter, so one missing ID
is reported by name. The format and the per-vendor, per-kind support tables are in the
[resource selectors reference](../reference/resource-selectors.md).

Why: one format for every kind and vendor, so that an operator's tool, the docs, the
CRD, the validator and the conformance suite have one thing to know; the native
`filters` and `expression` escape hatches keep every capability a vendor has (EC2
root-device-type, a GCP regex over names, a Hetzner label selector with negation) without a
new field per vendor; strict validation and the always-re-apply rule keep
[ADR-017](#adr-017-never-degrade-an-option-silently-accepted-2026-11-05) true on APIs that
silently ignore what they do not understand.

Rejected: a bespoke `use` shape per vendor (what the first vendor plans drifted into:
five variants of the same three image lookups); native filters only (not portable: a NodeClass
could not move between vendors, and the same intent has seven spellings); portable
attributes only (loses what the vendor can do, and the per-vendor field list would grow
for ever); a single cross-vendor query language (none exists; GCP filters and Azure
Resource Graph are two incompatible languages, the others have none, so it would be a
SCarpenter-invented language to translate into each).

Findings from the research that shaped the design (2026-11-10, eight vendors):

- A flat `ids` list for global kinds cannot share a field with a per-region map in a structural CRD
  schema (one field cannot be both an array and an object). Global kinds use the region key
  `*` instead (`ids: {"*": [id]}`), and a region key adds to it.
- Vultr has no filter at all but one or two hard-coded parameters, UpCloud and hcloud have only
  labels and exact names, Azure has no per-kind filter, OVH's Glance and Neutron have no name wildcard:
  `name` and most `match` attributes are client-side on those vendors, which is why the client
  always re-applies the selector (a vendor-side narrowing is an optimisation only).
- GCP cannot mix its AIP-160 and RE2 filters in one request and has no glob: a glob is a literal
  substring on the server plus the client match. Publicly shared GCP images are not listed at all, only
  readable by exact name or family: `ids` and exact `name` work for them, a glob does not.
- Tags are not one thing: GCP networks, subnets and firewalls have no labels, Azure subnets none
  (the VNet's tags are matched), Vultr only on instances, OVH tags are plain strings with no key and
  value, Alibaba deployment sets none. A kind without tags rejects `tags` rather than ignoring it.
- No creation time on many kinds (UpCloud templates, AWS subnets, Azure marketplace images):
  `newest` and `oldest` need `orderBy: name` or `label:<key>` there, and the format rejects
  `created` on those kinds.
- `cidr` is an equality test (the plan of addresses an IaC knows); containment is a separate
  `within`, client-side everywhere, because no API offers it.

Consequences: amends ADR-018's `use` forms and the wording of ADR-021 and ADR-022
(`selector.tags` is `tags`; the opt-in discovery of ADR-022 step 2 is a built-in `tags`
selector). The `ResourceUse` API type, its CEL rules, the driver capability declaration, the
shared matcher and the AWS driver's migration are
[plan 14](../plans/v1/wip/14-multi-vendor-groundwork.md) (A10); the AWS driver's today
forms keep their meaning and the one renamed field, `selector.tags`, becomes `tags` with **no deprecated alias**
(decided by the maintainer, 2026-11-10): the API is `v1alpha1`, nothing is released, and a second spelling would
live in the CRD, the docs and the code for ever. The old field is pruned or rejected, and a `use` that is
then empty is invalid (`ValidationSucceeded=False`), never "nothing given", so a stored NodeClass fails loudly
instead of selecting everything. Adds the `use` fields
`match`, `filters`, `expression`, `select`, `orderBy` to every kind of every vendor, a
capability table per driver to maintain, and one test matrix per driver in the conformance
suite.

## ADR-024: Billing-period-aware node retention (accepted, 2026-11-10)

Some vendors bill a server by the started period: hcloud, UpCloud and Vultr by the hour,
OVH's US regions probably (the data disagrees, see below). Others bill a minimum and then per second:
Alibaba (2 to 10 minutes by vCPU, charged only to a short-lived instance; unverified). A job that runs one minute every ten
minutes and gets a fresh node each time pays for six hours of node per hour; the same job on one
node kept until its hour is about to end costs one. SCarpenter therefore offers, as an opt-in,
to keep such nodes until the end of the billing period (or of the minimum) they have already paid for,
so that Karpenter reuses them, and to delete them before the next charge starts.

```yaml
kind: SCNodeClass
spec:
  billing:
    retainUntilPeriodEnd: true   # default false: nothing below happens
    margin: 3m                   # default 3m, at least 90s: stop this long before the boundary
    minPeriod: 10m               # default 10m: only billing increments at least this long are held to their end
    # maxRetainedNodes: 20       # unset by default; optional safety valve (below)
```

**Where.** On the `SCNodeClass`, not the NodePool. The vendor and the region, which decide
whether a period exists at all, are known only there (a NodePool names no vendor), and one
NodeClass can span several vendors: the setting is one intent ("reuse nodes within their paid
period"), and each vendor's rule decides whether it has any effect. The NodePool keeps what
is Karpenter's (consolidation policy, budgets, `expireAfter`).

**Billing facts come from data.** Keeper's `/table/vendor` has `billing_increment_seconds`,
`minimum_billing_seconds`, `stopped_server_charged` and `billing_comment` (the vendor table is
live today; the catalog does not read it yet). SCarpenter caches them with the catalog. A vendor's
rule is a minimum `M` and an increment `I` (the charge for a run of `t` seconds is `M` up to `M`, then
rounded up to whole increments; for hourly vendors `M` is `I` or none).

**One rule for both shapes: the release deadlines.** A node is worth keeping only until a point where the
charge is about to step up. These points are the *boundaries*. Two shapes are supported, and no others:

- **increments held** (`I >= minPeriod`, with `M == I` or no minimum): the boundaries are `start + n*I` for `n >= 1`;
- **minimum only** (`I < minPeriod`): one boundary, `start + M`.

Any other combination (a minimum different from a held increment, such as 90 min with 60 min, or 30 min with 60 min) has an
ambiguous formula (increments counted after the minimum, or rounded up with a floor at `M`), so `Resolve` marks it `Verified=false`
with the status reason `UnsupportedShape` and nothing is held; a field for it is added when a vendor needs one.

For an hourly vendor (`M` = `I` = 1 h) that is every hour. For a **minimum, then per second** vendor
(`I` < `minPeriod`; Alibaba) there is one boundary, `start + M`: after it
every second costs, so a node is held until `start + M - margin`, reused meanwhile, and released at once
(no later deadline exists). The pure boundary function is the same for both, with no vendor name in the
core. A per-second vendor with no minimum (AWS and GCP bill 60 seconds at least, below the hold floor)
has nothing to hold and never qualifies.

`minPeriod` therefore gates holding **per increment** (default 10 minutes, unchanged), not the minimum hold. The
minimum hold has its own floor: it is worth it only when `M - margin >= 2 minutes` (the effective margin, below; an internal
constant, not an API field; the window a job has to arrive in). With the default 3-minute margin a minimum of 5 minutes holds for
2 minutes, 10 minutes holds for 7, and 120 seconds never holds (the status says `MinimumTooShort`). The margin
default stays 3 minutes for both shapes: its floor is 90 seconds either way; on a minimum-then-per-second vendor
missing the deadline costs only the seconds of overrun, not a period, so the floor is conservative there, and a
smaller margin would not enlarge the window of a short minimum enough to matter.

Keeper's value is per vendor and "worst case", but the rule can differ below that (OVH US versus
EU, Alibaba's minimum by vCPU), so a driver may override per region and family
(`BillingRule`) until Keeper has finer data (plan 13). A vendor fact that no live check has confirmed
stays **off**: the driver reports it as unverified and retention does not apply to that vendor (the NodeClass
status says so), never a guess ([ADR-017](#adr-017-never-degrade-an-option-silently-accepted-2026-11-05)).
**The anchor is closed (2026-11-10):** no vendor documents clock-hour billing (OVH US's own timed example refutes it), so there
is no `anchor` field and periods always count from the server's billing start. That is an inference for Hetzner, and the
docs of Vultr and UpCloud are silent on partial hours, so **from-start is verified live per hourly vendor before retention
is enabled for it** (Hetzner, Vultr, UpCloud; plans 15, 20, 19).

**The period starts when the vendor says.** `Instance.BillingStart` (new, driver contract) is the
vendor's own creation or billing-start stamp, never the NodeClaim's time. Where the stamp is earlier
than the real start (hcloud, OVH, Azure: creation precedes the billed state) the computed boundaries fall
early, the safe side. A vendor with no stamp (UpCloud) gets one the driver writes itself in the create call;
no stamp means no retention for that node, with an Event. A stamp that may be *later* than the real start
(Vultr's `date_created` against "when deployed") is the unsafe side and keeps the vendor unverified until its
live check bounds the offset. `Create` returns the stamp as the NodeClaim annotation
`scarpenter.sparecores.com/billing-start` (Karpenter core merges the annotations the cloud provider returns into
the NodeClaim and copies them to the Node at registration), so the controller never asks the vendor and a
restart loses nothing. Clock skew between the controller and the vendor is covered by the margin.

**Mechanism: hold, then delete ourselves.** A new retention controller, per qualifying node (a
registered, on-demand node of a non-static NodePool, owned by a NodeClaim of such a NodeClass, whose
`BillingStart` is known):

1. While the current period (or the minimum) has more than `margin` left, it holds the node with Karpenter's
   `karpenter.sh/do-not-disrupt: "true"` on the Node, plus its own annotation
   `scarpenter.sparecores.com/retain-until` (the deadline, so the state is visible and survives a
   restart; the reconcile is idempotent, there are no in-memory timers: every decision is a function of
   `billing-start`, the rule, the clock and the pods). The hold is put on at registration, before the node
   is Initialized (Karpenter never disrupts a node that is not), not at emptiness: with
   `consolidateAfter: 0s` Karpenter would otherwise pick a node the moment it empties. Karpenter binds pending pods to existing capacity first, so a held empty
   node takes the next job. Only annotations SCarpenter put there are removed; a `do-not-disrupt` it did
   not set is the user's, and the node is left entirely alone (decision 3 below).
2. At `boundary - margin` (the deadline): an **empty**, Initialized node (no pod except DaemonSet, static and
   finished pods; a node not yet Initialized may be waiting for pods that have not bound, and counts as busy) is deleted by SCarpenter itself, if the NodePool's disruption budget allows it (`NodeClaim` delete: Karpenter taints, drains and calls
   the driver). Deleting it ourselves is deterministic; relying on `consolidateAfter` would not be, it
   is NodePool-wide and could land after the boundary. A **busy** node gets its hold released for the window
   `[deadline, boundary)` so that Karpenter's own consolidation may replace it at a moment that costs
   nothing (the period is nearly over), and the hold is put back after the boundary for the next period.
   If it empties inside the window, SCarpenter deletes it then, as at the deadline: the period is paid
   either way, and the delete saves the next one. Moving the hold straight to the next boundary
   would block consolidation of a node for as long as it stays busy. At the **last** boundary (a
   minimum-then-per-second vendor has only the minimum) a busy node's hold is released for good: after it every
   second costs, and Karpenter's own consolidation takes over under its own rules.
3. The delete is a *release sequence*. The controller shares the operator's `state.Cluster`: it checks Karpenter's nomination
   (`IsNodeNominated`; a nominated node is busy), marks the node for deletion so that the scheduler stops using it, puts its own
   `NoSchedule` taint (`scarpenter.sparecores.com/releasing`), waits 5 seconds (Karpenter's `MinDrainTime`), re-reads the pods from
   the API server (not the cache) and re-checks the nomination. A hit undoes the mark and the taint and the busy path applies, so a
   node with work is never deleted by SCarpenter. It costs about 25 seconds of the margin. A pod that tolerates the taint or binds in
   an older scheduling cycle is evicted and rescheduled by the drain, delayed and never lost.

*Budget accounting.* The budget comes from the same cluster-state snapshot with Karpenter's own helpers, counting nodes marked for
deletion or already disrupted, and SCarpenter marks the node before deleting the NodeClaim. Karpenter re-validates its own commands 15 seconds
after computing them and marks only when it starts one, so a SCarpenter mark between that re-validation and Karpenter's mark can overshoot a budget by one node: accepted
(core has no hook), documented and counted by `scarpenter_billing_budget_overshoot_total`.

**Margin.** Missing a boundary costs a whole extra period, so the bias is early. The margin covers the
drain and the termination finalizer (the instance delete starts only after the Node drains), the vendor's delete
latency (billing usually stops at the delete, or at completion on a vendor whose delete is an asynchronous
action; UpCloud stops the server first) and clock skew. The floor of 90 seconds is enforced by CEL. A driver
may declare its delete latency (`BillingRule.DeleteLatency`); the effective margin is the larger of `margin`
and that latency plus the 90-second floor (the drain and skew are on top of the vendor's delete).

**Interactions.**

- *Disruption budgets (decided 2026-11-10).* SCarpenter's deadline deletes **respect** the NodePool's
  disruption budgets, for reason `Empty`, computed as Karpenter core does and counting nodes already being
  deleted. When a budget blocks the delete, the node is not deleted and is **held into the next period**: one more
  period is paid (an Event `ReleaseBlockedByBudget` and a metric record it), and the next deadline is tried. A
  budget of 0 (a maintenance window) therefore costs a period per node per window, which is the operator's
  choice; on a minimum-then-per-second vendor a blocked node's hold is released and Karpenter's consolidation,
  under the same budget, deletes it when allowed. A budget is never bypassed.
- *Left alone (decided 2026-11-10).* A NodePool with `consolidateAfter: Never`, or a node with a
  `karpenter.sh/do-not-disrupt` that SCarpenter did not set, is left completely alone: no hold, no delete, no
  release for pressure. The NodeClass condition `BillingRetention` counts such nodes per reason, and a Node Event
  `RetentionSkipped` (`ConsolidationNever` or `UserDoNotDisrupt`) says why on the node.
- *Drift.* A node whose NodeClaim is `Drifted` is not held (and a hold is removed): a drifted node is
  replaced as it would be without retention. A held node would otherwise take new pods with stale settings.
- *Consolidation.* Underutilised consolidation of a held node waits for the window. Karpenter cannot
  learn that the node's remaining period is free (offerings are per type, not per node), so it may
  replace a busy, cheaper-to-keep node with a smaller one inside a period; recognising sunk cost is
  a follow-up (below).
- *`expireAfter`, `terminationGracePeriod`, interruption and node repair* are forceful in Karpenter and
  ignore the hold; SCarpenter does not fight them. `expireAfter` counts from the NodeClaim's
  creation, which is before the billing start, so an `expireAfter` of a multiple of the period minus the margin
  (`23h55m` for an hour and a 5-minute margin) ends a node before a boundary; any other value wastes part of
  its last period, or crosses into a new one. The docs say so.
- *The stopped-node rule* ([plan 14](../plans/v1/wip/14-multi-vendor-groundwork.md), A4): a stopped node
  cannot take pods and no hourly vendor pauses billing by stopping, so it is still reported `terminating`
  and deleted, and it is never held. What that saves is not the current period, which is paid whether the
  node is deleted now or at its end; it saves the following periods, the disks and the addresses. The
  rule's text in the plans said otherwise and was corrected.
- *Limits and quotas: release under pressure (decided 2026-11-10).* Held nodes count against NodePool `limits`
  and the quota tracker, so retention must never starve a new shape of capacity. There is **no fixed cap by
  default**: when a launch is blocked by a NodePool limit (pods pending while the pool's resources are at its
  limit) or by a vendor quota (the quota tracker refused `Create`), SCarpenter releases held **empty** nodes
  first, enough to cover the blocked request and only those whose release frees what is blocking (the same
  NodePool for a limit; the same vendor, region and quota family for a quota). The order is the **nearest
  deadline first** (ties: the oldest start): the node nearest its deadline has the least paid time left, so
  the release loses the least value, and for equal periods that is the oldest-paid-up node. Releases follow the same
  rules as any delete (budgets, nodes left alone, the release sequence). `maxRetainedNodes`, an
  optional **safety valve, unset by default** (`>= 1` when set), caps the held *empty* nodes per NodeClass; over it,
  the same order applies. A release under pressure that the candidate's budget blocks drops that node's hold (it is then an
  ordinary empty node, deleted by Karpenter's emptiness when the budget opens) and moves to the next candidate; the launch waits
  for the budget as it would without retention, so retention never adds a wait of its own and cannot deadlock it. A pod that does not fit the held node, because of its shape, taints or requirements, gets a
  new node: the saving is for repeated work of the same shape.

**Observability.** Node Events `RetainedUntil`, `ReleasedAtPeriodEnd`, `ReleasedUnderPressure`, `ReleaseBlockedByBudget`,
`RetentionSkipped` (and a Warning `BillingStartUnknown`); metrics `scarpenter_billing_retained_nodes{vendor,nodeclass}`,
`scarpenter_billing_retained_seconds_total`, `scarpenter_billing_reuse_total` (a node that had been empty
under the hold received a pod), `scarpenter_billing_periods_saved_estimate_total` (equal to the reuses: one
new node, and so one period, avoided each; an estimate, not an invoice),
`scarpenter_billing_deletes_at_deadline_total`, `scarpenter_billing_blocked_by_budget_total`, `scarpenter_billing_released_under_pressure_total`, `scarpenter_billing_skipped_nodes`, `scarpenter_billing_budget_overshoot_total`, `scarpenter_billing_boot_seconds` (registration time
against the billing start) and `scarpenter_billing_boundary_missed_total` (an empty node still
alive 30 s after its boundary: the margin is too small). The NodeClass status has, per vendor, whether
retention is active and why not (`Disabled`, `PerSecondBilling`, `MinimumTooShort`, `Unverified`, `UnsupportedShape`, `NoKeeperData`, `NoBillingStart`; `Active` otherwise),
the period and where it came from (Keeper, driver).

**Testing.** The boundary arithmetic is a pure function with table tests. The controller runs against the fake
driver with a fake clock; the conformance and L3 scenarios are: a job every 10 minutes reuses one node, a busy
node rolls over to the next period, an empty node is deleted before its boundary, a per-second vendor with no
minimum ignores the setting, a job every 3 minutes on a 10-minute-minimum vendor reuses one node and the node is
gone right after the minimum, a budget of 0 holds an empty node into the next period, a blocked launch releases the
nearest-deadline empty node first, and `consolidateAfter: Never` or a user `do-not-disrupt` is left alone.

Why: the saving is real and large for exactly the workloads Karpenter is good at (short, repeated
jobs), it is invisible to Karpenter (offerings carry no per-node cost), and the data to do it exactly
(period length, start time) exists. The hold reuses Karpenter's own reuse and validation; SCarpenter
adds only what Karpenter cannot know, the vendor's clock.

Rejected: a NodePool-level `consolidateAfter` of one period (it is pool-wide, not per node, and ignores where
in its period a node is); an own delete only (loses Karpenter's reuse, which needs the node to stay);
delaying `CloudProvider.Delete` after Karpenter removed the Node (the instance can no longer take pods);
hard-coding hours (the period comes from data, and any other period works); stopping nodes to pause billing
(no hourly vendor pauses on stop); applying retention to per-second vendors with no minimum (no saving); a
fixed default `maxRetainedNodes` (release under pressure serves the same purpose without guessing a number);
deleting regardless of budgets (an operator's budget is a promise).

Data issues found (raised with the data owners, plan 13): OVH is 3600/3600 in Keeper but OVH's page says per
second everywhere (EU, CA and APAC: per second with a 1-minute minimum; US: hourly per the US terms, per second per the US
support guide, so conflicting); Azure is 1/300 in Keeper but the page says per full minute and no 300 s minimum is documented; the
vendor row is "worst case" and cannot express regions, families, monthly caps (Hetzner, Vultr 672 hours, UpCloud Simple plan) or the
billing start event.

Follow-up, not designed here: consolidation awareness, so that Karpenter treats a held node's remaining
period as free (a hook that lowers its cost for replacement decisions); monthly caps (no deadline once the cap
is reached); per-region and per-family Keeper data replacing the driver overrides.

Consequences: a controller with node and NodeClaim write access and a pod cache; the `spec.billing` API field and
CEL; `Instance.BillingStart` and `BillingRule` in the driver contract; the vendor table in the catalog, so a
Keeper client method and, with plan 13, the snapshot; one conformance scenario set. **First vendor: Hetzner** (decided 2026-11-10), switched on when
[plan 15](../plans/v1/wip/15-hcloud.md)'s live lane has verified its billing start and that its periods count from it; Alibaba becomes a
minimum-only candidate, still gated on verified data; Azure bills per full minute with no documented minimum and is not one. Implemented by
[plan 23](../plans/v1/wip/23-billing-retention.md); the user-facing description is
[Reuse nodes within the billing period](../how-to-guides/reuse-nodes-within-billing-period.md).
