# 29 — Image copies: Flatcar images copied on demand into regions without one

Depends on: 08 (the AWS driver and its in-memory EC2 fake), 14 (the `images` kind, `ImageRequest`, the version check),
21 (the Alibaba driver, with `fix/alicloud-slow-regions`, merged and released as 0.7.5), 28 (the reason taxonomy,
`ImageGap`), and the hcloud image builder of ADR-030 (the precedent). Read `docs/plans/v1/CONVENTIONS.md` first. Spec:
this plan; overview §9 (images), §10 (garbage collection), §4.1 (`Create`); ADR-012, ADR-017, ADR-021, ADR-030;
`docs/explanation/flatcar-images.md`; `docs/reference/aws-driver.md` (Images, Tags, IAM);
`docs/reference/alicloud-driver.md` (Images).

Written 2026-11-23 from the repository at `origin/main` (6e1b4c7) and the owner's design of the same day, revised
after review the same day. The plan-executing agents call no cloud API (CONVENTIONS rule 8); the live checks, the L5
runs and the OCI and UpCloud spikes are the coordinator's. Everything marked **unverified** is settled by the step or
spike that names it.

## Goal

Flatcar publishes no AMI in some AWS regions (today `ap-east-2`, `ap-northeast-3`, `ap-south-2`, `ap-southeast-4`
to `-7`, `ca-west-1`, `eu-central-2`, `eu-south-2`, `il-central-1`, `me-central-1`, `mx-central-1`; **verified
2026-11-23** by the coordinator for `ap-northeast-3`, `ap-south-2` and `eu-south-2`: `DescribeImages` of owner
`075585003325` returns no Flatcar stable 4757.2.3 AMI there). A NodeClass reports `ImagesPartial` and those regions'
offerings are unavailable (`ImageGap`), although some of them have the cheapest spot prices (`ap-south-2`,
`eu-south-2`: up to 78% cheaper for hundreds of types). On Alibaba Cloud the operator's IaC imports the image into one
region and copies it into every region of every NodeClass (21 today), most of which never launch a node.

With this plan, opted in per vendor, SCarpenter copies the pinned release's image into a region **when a launch
wants that region**, from a region that has it, tags the copy as its own, and deletes it when nothing uses it any more.
The launch never waits for a copy: the region is skipped until the copy is available, and Karpenter re-solves. The
owner chose on-demand copies with an idle TTL over eager copies to every region.

## Non-goals

- Importing images from outside the account or the vendor (rung 4 of ADR-021 stays hcloud's alone, ADR-030). OVH
  would need it (section 2).
- Copies for a NodeClass that names its images (`resources.images.use`) or an owner (`imageOwner`).
- Sharing copies between clusters (section 4, decision 2).
- Waiting for a copy inside `Create`. The pending state goes through the existing image gate, not through
  `pkg/unavailable`; `create.go` changes only to invalidate its image cache (step 2) and, later, for a distinct Event
  (step 6).
- Pre-warming regions ahead of demand (cut in review: not an owner goal; the release bump of section 6 covers the
  case that matters).
- Encrypting copies with a customer key, copying into another account, GPU images.

## 1. Today (as found)

- **AWS** (`pkg/vendors/aws/image.go`): `ResolveImage` runs `DescribeImages` with owner `075585003325`
  (`imageOwner` overrides it), the exact name `Flatcar-<channel>-<version>-hvm` or `-arm64-hvm`, `state=available`,
  exactly one match; or the NodeClass's `resources.images.use`. The version check accepts an AMI whose name is not
  the official one when its tags `flatcar-channel` and `flatcar-version` say the release (`imageRelease`), so a copy
  under another name passes ADR-017 already. The driver caches the final answer per key: a hit for 1 h (`imageTTL`,
  `image.go:315`), a definitive miss for 5 min (`imageMissTTL`, `:192` to `:200`). `ReportImages` resolves every
  region of the block for both architectures.
- **Gate.** The NodeClass controller turns the report into `ImagesReady` and calls
  `ImageGate.SetImageGaps(class, vendor, gaps)` (`pkg/controllers/nodeclass/controller.go:562`); `imageGaps`
  (`:875`) gates a region only when **no architecture** has a ready image, and the resources engine answers
  `ClassGate.UnavailableFor` with `ImageGap` per region (`pkg/resources/engine.go:679`). The catalog marks the
  offerings unavailable; `candidates` drops them (`off.Available`).
- **Create** (`pkg/cloudprovider/create.go`): an image resolution error is an `imageError`: the candidate is skipped
  without counting an attempt (`skipImage`), the walk goes on. When nothing launched and only images failed, the
  Create returns `ImageResolutionFailed` with a Warning Event `ImageFailed` (core retries the NodeClaim); with no
  candidates at all it returns `InsufficientCapacity` (core deletes the NodeClaim and re-solves). Successful
  resolutions are cached for `ImageCacheTTL` (1 h, `create.go:817`) with no invalidation. A `Fatal` launch error
  (`InvalidAMIID.*` is `Fatal`) ends the walk (`create.go:222` to `:225`). Image skips do not count against
  `spec.launch.maxAttempts` and cost no vendor call when cached, so `spec.launch.maxDuration` and Karpenter's 5 min
  launch timeout are not touched.
- **hcloud** (ADR-030, `pkg/vendors/hcloud/build.go`): the precedent. `ensureImage` is called when the lookup finds
  nothing, starts one build at a time, answers `ImageBuilding` or `ImageBuildFailed` (class `NotFound`), which the
  NodeClass controller turns into the image status, `ImagesReady` and the Events (`imageBuildEvents`). The retry
  after a failure is a constant (15 min). Superseded snapshots are never deleted.
- **Resources engine** (`pkg/resources/gc.go`): lifecycles `perUse`, `idleTTL`, `retain` for managed resources
  per NodeClass. Not reused for copies: a copy is shared by every class that pins its release, it is created
  asynchronously, and two of its delete rules (a primary image appeared, the release is no longer pinned) are not
  lifecycles. The pure `Decide` style is copied.
- **Vendored AWS SDK** (checked by the coordinator): `CopyImage` takes `TagSpecifications` and `ClientToken`;
  `DeregisterImage` takes `DeleteAssociatedSnapshots`; `Image` carries `SourceImageId` and `SourceImageRegion`.

## 2. Vendors

Researched from the driver references and `flatcar-images.md` (prices of 2026-11-16 there); the rest is
**unverified**.

| Vendor | Images are | Copy API | Source of truth for a copy | Storage | Transfer | This plan |
|---|---|---|---|---|---|---|
| aws | regional | yes: `CopyImage` | Flatcar's official AMI in another region | EBS snapshot about $0.05/GB-month | about $0.02/GB from US and EU sources, more from some APAC sources (owner's figures; per pair **unverified**) | steps 1 to 4, first |
| alicloud | regional | yes: ECS `CopyImage` (the operator's IaC uses it today) | the operator's imported image in a source region | snapshot about $0.017/GB-month | **unverified** (possibly free) | step 5 |
| oci | regional, per tenancy | cross-region custom-image copy **unverified** (likely none); export to Object Storage and import by a pre-authenticated request is documented (the IaC route) | the operator's imported image in one region | Object Storage about $0.0255/GB-month; the custom image itself **unverified** | **unverified** | step 7, after the coordinator's spike confirms a copy API |
| upcloud | per zone and storage tier | cross-zone template clone **unverified**; a per-zone import from the images repository URL exists | the operator's template in one zone | €0.33/GB-month per template | **unverified** | step 8, after the coordinator's spike confirms a copy API |
| ovh | regional (Glance) | no: Glance has no cross-region copy; `web-download` import **unverified**, else a re-upload through the controller | Flatcar's official OpenStack image URL | **unverified** | **unverified** | no: that is an import (rung 4), a follow-up with its own ADR |
| gcp | global (amd64 official, arm64 Spare Cores) | not needed | | | | nothing to do |
| azure | Marketplace offers, global with per-region versions | not applicable (a gallery copy of a Marketplace image needs its terms) | | | | nothing to do: a gap waits for the Marketplace |
| hcloud | global in the project, built by the driver | not needed | | | | nothing to do; deleting superseded built snapshots with rule (c) is a follow-up |
| vultr | global in the account | not needed | | | | nothing to do |

## 3. Settings

**Where (decision 1): controller flags and Helm values per vendor, not the SCNodeClass.** A copy belongs to the
account, not to a class: every class that pins the release uses it, the GC deletes it account-wide, and the write
permissions are the controller's. ADR-030 set the same precedent (`--hcloud-build-images`, chart
`vendors.hcloud.buildImages`). No CRD change, no drift-hash change.

| Flag (env `SCARPENTER_*`) | Helm value | Default | Validation |
|---|---|---|---|
| `--aws-copy-images` | `vendors.aws.imageCopies.enabled` | `false` | bool |
| `--alicloud-copy-images` | `vendors.alicloud.imageCopies.enabled` | `false` | bool |
| `--alicloud-image-copy-source-region` | `vendors.alicloud.imageCopies.sourceRegion` | the home region | a region ID; it must hold the operator's image of the pinned release, else every copy is `ImageCopySourceMissing` |
| `--image-copy-idle-ttl` | `imageCopies.idleTTL` | `720h` | a duration, at least `1m` (the L5 scenario uses `1m`) |

A flag that enables copies for a vendor whose driver lacks the capability refuses to start. Constants, not flags:
the retry after a failed copy (6 h, like hcloud's build retry), at most 4 copies in flight per vendor (queued beyond),
a backoff of 1 min doubling to 15 min on a limit or throttling error, a copy timeout of 2 h, the GC interval of 10 min,
the unpinned grace of 2 h.

**Opting a class out.** Copies are made for a region only when a launch of a class without `images.use` or
`imageOwner` wants it. A class that must not cause copies either keeps the image-less regions out of its NodePools'
`topology.kubernetes.io/region` requirements (no demand reaches them), or lists its regions in the vendor block, or
names its images (`images.use`). Note: `imageOwner: "075585003325"` (the default owner, spelled out) also disables
copies for the class, because any `imageOwner` does; the reference says so and an L1 case pins it.

**Requirement on the operator:** cluster names (`--cluster-name`) must be unique per account. Copies are scoped by it
(decision 2).

## 4. Lookup precedence and data model

For each (vendor, region, architecture, channel, version), the driver's lookup, in order:

1. **`resources.images.use`**: that selector only, as today. Never copies.
2. **`imageOwner`** (AWS; Alibaba with an owner other than `self`): that owner's image by the convention only.
   Never copies.
3. **The primary image** by the vendor's convention, unchanged: AWS the official AMI; Alibaba the operator's
   `Flatcar-<channel>-<version>-<arch>` (an IaC copy is primary). Found: used, whatever copies exist. Only this step's
   answer goes into the driver's hit and miss caches.
4. **The copy manager**, consulted on every call after a primary miss, **above the driver's cache**: an in-memory
   answer with no vendor call. An available copy of this cluster (owner `self`, the tags below, the architecture;
   several after a race: the oldest, the GC deletes the others) is returned and never put into the driver's 1 h hit
   cache, so a deleted copy is never served from it. The version check of ADR-017 reads the copy's tags.
5. **Copies enabled**: the copy's state decides the answer (section 5). Disabled: `ImageNotFound`, as today.

**Decision 2: copies are per cluster.** The ownership tag is the cluster's name, so two clusters in one account copy
twice (cents a month each) but never delete each other's copy, and IAM conditions on the cluster tag work as for
instances. Account-wide sharing would need a reference count nobody can maintain once a cluster is gone.

**Tags**, set only at creation (AWS: `CopyImage`'s `TagSpecifications` on the image and its snapshots; labels or
freeform tags on the other vendors); logical keys through `TagKey`:

| Key | Value |
|---|---|
| `TagKey(cluster)` | the cluster name (the IAM condition) |
| `TagKey(managed-by)` | as on every managed resource |
| `TagKey(resource-kind)` | `image-copy` |
| `flatcar-channel`, `flatcar-version` | the release (the existing version-check tags, unprefixed) |
| `TagKey(image-arch)` | `amd64` or `arm64` |
| `TagKey(image-source)` | `<region>/<image ID>` of the source; **not on AWS**, where `Image.SourceImageId` and `SourceImageRegion` say it |

The name is `scarpenter-flatcar-<channel>-<version>-<arch>-<cluster>` (truncated to the vendor's limit with a hash),
never the official name: an AWS name lookup of the official owner and Alibaba's exact-name convention cannot match a
copy, so steps 3 and 4 never collide.

**Last used** is the vendor's own record where it has one: on AWS the image attribute `lastLaunchedTime`
(`DescribeImageAttribute`), which AWS updates within about 24 h of a launch (**unverified** beyond that), good enough
for a 30-day TTL; no tag is written on use. Vendors without one (Alibaba, **unverified**) set a last-used tag at most
once a day from the manager (`TouchCopy`). The manager also remembers, in memory, when it last resolved each copy for
a launch; the GC takes the latest of the three times and the creation time.

**No other state.** The vendor's image is the record: a pending image is a copy in flight, so a restarted or new
leader adopts it instead of starting another. The queue, the failure times and the unpinned-since times are in
memory; after a restart a failure is retried on the next demand and an unpinned release waits its 2 h again.

## 5. Behaviour

**Image status reasons** (per region and architecture in `status.vendors.<v>.images`; codes in `pkg/vendors`):

| Reason | Meaning | Region gated | `ImagesReady` |
|---|---|---|---|
| (ready) | the primary image or this cluster's available copy | no | as today |
| `ImageCopyOnDemand` | no image; a copy will be made when a launch wants the region | **no**: the offerings stay available so that demand can reach a launch | `True`; reason `ImagesPartial` lists it as "copied on demand" |
| `ImageCopying` | a copy is queued or in flight (message: the source and the start) | yes, catalog reason **`ImageCopyPending`** (new code, ADR-029 allows additions) | `True` with `ImagesCopying`, also for a region the class names: a pending copy is not a fault |
| `ImageCopyFailed` | the last copy failed less than 6 h ago (message: the vendor's code and text) | yes, `ImageGap` | as a missing image today: `False` for a named region, else `ImagesPartial` |
| `ImageCopySourceMissing` | copies are enabled but no source region has the image | yes, `ImageGap` | as a missing image |

**Both architectures together (decision 3).** The gate is per region, not per architecture (`imageGaps`,
`UnavailableFor`, plan 28 decision 6). A copy of only the demanded architecture would ungate the region for both and
send the other architecture's launches into skips. So demand for a region enqueues the pinned release for **every
architecture the driver serves there** (AWS: amd64 and arm64) as one unit; the region's images go `ImageCopying`
together and the gate lifts when all are available or failed (a failed architecture is then `ImageCopyFailed`, a gap
only if no architecture is ready, as today). The cost is a second copy per region: about $0.16 once and $0.40 a month
more (section 8).

**Step by step**, for a NodeClass without a region list on AWS with copies enabled:

1. `ReportImages` (leader, NodeClass controller) finds no official AMI in `ap-south-2` and no copy: both images are
   `ImageCopyOnDemand`, the region is not gated, its offerings are offered to Karpenter. A report never starts a copy,
   except for the release bump of section 6.
2. Karpenter solves a pod; the NodeClaim's cheapest candidates are in `ap-south-2`. `Create` calls `ResolveImage`
   (the launch path); the driver's lookup reaches step 5 and asks the copy manager, which enqueues the region (both
   architectures) and answers `ImageCopying` (class `NotFound`). `create.go` treats it as today's `imageError`: the
   candidate is skipped, no attempt is counted, the walk tries the next candidate. Every other candidate of
   `ap-south-2` gets the same answer from memory.
3. The manager signals the NodeClass controller (a channel source that requeues the classes of the vendor). The next
   report says `ImageCopying`, `SetImageGaps` gates the region with `ImageCopyPending`, the catalog generation
   changes, and Karpenter's next solve no longer sees those offerings. Between steps 2 and 3 (seconds) other
   launches skip the region as in step 2.
4. If the NodeClaim had only `ap-south-2` candidates, `Create` returns `ImageResolutionFailed` (until step 6 with
   the Warning `ImageFailed`); core retries it, the retry finds no candidates (gated), returns
   `InsufficientCapacity`, core deletes the NodeClaim and re-solves without the region. A pod that fits nowhere else
   stays Pending while the copy runs.
5. The manager starts `CopyImage` when a slot is free, from the source (below), with the tags and a client token
   derived from (cluster, region, release, architecture); it polls the destination every 30 s (one `DescribeImages`
   per region with copies in flight).
6. Available: the manager signals again. The report says ready, the gate lifts, Karpenter offers the region, and
   pending pods launch there. Consolidation may then move nodes into the cheaper region; that is the intent.
7. Failed or timed out: the image (if any) is deregistered, the key is `ImageCopyFailed` for 6 h, a Warning Event
   `ImageCopyFailed` on each class that reports it, the metric. Afterwards the key returns to `ImageCopyOnDemand` and
   the next demand retries.

**Demand is wider than one launch.** One `Create` walks every candidate in price order, so a single NodeClaim can
enqueue several image-less regions that are cheaper than the one it launches in; consolidation's replacement
NodeClaims do the same, since they are Creates. That is intended (they are the cheaper regions) and bounded by the
image-less regions of the class: worst case today 13 AWS regions times 2 architectures (section 8).

**Why the image gate, not `pkg/unavailable`** (decision 4): the gate already exists for regions without an image, it
is per class and region, and it reaches Karpenter through the catalog generation. A pending mark there costs one new
reason code and a requeue signal. The cost is the window of step 3, in which launches skip the region without a
vendor call.

**Interaction with `spec.launch`:** `maxAttempts` does not count image skips; `maxDuration` and Karpenter's 5 min
launch timeout are unaffected because no launch waits for a copy; `insufficientCapacityTTL` is unrelated. The copy
duration (**unverified**, minutes to tens of minutes) bounds only how long the region stays gated.

**Source choice (AWS, decision 5): a fixed preference list** `us-east-1`, `us-east-2`, `us-west-2`, `eu-west-1`,
`eu-central-1`, then any region that has the official AMI, in name order. The image is identical everywhere, so only
transfer price and reliability matter: US and EU sources are the cheapest per GB (owner's figures), Keeper has no
inter-region transfer prices (**unverified**), and a fixed list is deterministic and testable. Alibaba: the
configured source region. The source must pass the same version check as a launch.

**Concurrency.** One copy per (vendor, region, channel, version, architecture): a second demand joins it; a pending
image of this cluster with the tags is adopted. At most 4 in flight per vendor; the rest wait in demand order. A
limit or throttling error from the copy call keeps the copy queued with the backoff; a refusal (a region not enabled,
a missing permission, an unsupported region) fails it (step 7). The error codes are **unverified** (step 4).

## 6. Lifecycle and GC

**Release bump (fix for drift).** When a class pins a new release, the new release has no copy, so a copy-only region
would be gated, drift would replace its nodes elsewhere, and consolidation would later move them back. To avoid that
evacuation, the manager (on each NodeClass signal and GC tick) enqueues the newly pinned release for **every region
where this cluster has a copy of another release that an instance runs from**. Drift then finds the region gated
only for the copy's duration, a few minutes, instead of losing it. The old release's copy is protected while its
instances run, and goes by rule (c) afterwards.

**GC loop** (leader, every 10 min, per vendor with copies enabled, every region of the account that has a copy of this
cluster). A copy is **never** deleted while an instance of the account (any cluster; any state but terminated) runs
from it. Otherwise it is deleted when one of:

| Rule | Condition | Delete reason |
|---|---|---|
| (a) idle | `now - last used >= idleTTL`, last used being the latest of the vendor's record (AWS `lastLaunchedTime`), the manager's in-memory last resolve, and the creation | `Idle` |
| (b) primary appeared | the primary lookup (section 4, step 3) now finds an image in the region, and the manager's last resolve is at least 2 h ago (the launch cache) | `PrimaryAvailable` |
| (c) unpinned | no SCNodeClass of this cluster has pinned the release for **2 h continuously** (tracked in memory; the clock restarts with the process) | `Unpinned` |
| (d) duplicate | another available copy of the key is older (section 4) | `Duplicate` |
| (e) failed | the image is in a failed state, or pending longer than the copy timeout | `Failed` |

A copy is deleted with `DeregisterImage` and `DeleteAssociatedSnapshots`. Only images with this cluster's `cluster`,
`managed-by` and `resource-kind=image-copy` tags are ever listed for deletion, and IAM enforces the cluster tag.
Each deletion: a log line, the metric, an Event `ImageCopyDeleted` on the controller's Deployment (the GC's existing
`DeploymentRef`), since a copy belongs to no class, and **the invalidation of every cache that holds the ID**: the
manager's state and the CloudProvider's image cache (a hook the manager calls; `create.go`'s cache gets a delete by
image ref). Without it a deleted AMI could be served for up to an hour and its `InvalidAMIID.*` (`Fatal`) would end
the Create walk.

**Not seen by the GC:** launch templates or other users' configuration referencing a copy (only running instances
protect it), and the copies of a deleted cluster (nobody deletes them; the how-to gives the listing by tag for the
operator's uninstall, and the e2e sweeper deletes the lanes').

## 7. Where the code lives

- **Contract** (`pkg/vendors/imagecopy.go`): the optional driver capability, small and vendor-neutral:

  ```go
  // ImageCopier copies a boot image between the regions of the account (plan 29).
  type ImageCopier interface {
      // Primary finds the image the convention resolves in the region (section 4, step 3), without copies.
      Primary(ctx context.Context, region string, rel Release) (ref string, found bool, err error)
      // CopySources lists the images a copy can be made from, in the order of preference.
      CopySources(ctx context.Context, rel Release) ([]ImageSource, error)
      // ListCopies lists this cluster's copies (any state) in the regions, found by the ownership tags,
      // with the vendor's last-used time where it has one.
      ListCopies(ctx context.Context, regions []string) ([]ImageCopy, error)
      // StartCopy starts a copy, tagged at creation; idempotent per token.
      StartCopy(ctx context.Context, src ImageSource, region string, rel Release, tags map[TagKey]string, token string) (ref string, err error)
      // CopyInUse says whether an instance of the account runs from the copy.
      CopyInUse(ctx context.Context, c ImageCopy) (bool, error)
      // TouchCopy records a use; a no-op for a vendor that records launches itself (AWS).
      TouchCopy(ctx context.Context, c ImageCopy, at time.Time) error
      // DeleteCopy deletes the image and its snapshots; only an image with the ownership tags.
      DeleteCopy(ctx context.Context, c ImageCopy) error
  }
  ```

  `Release` is (channel, version, arch); `ImageCopy` carries region, ref, release, source, state (`Pending`,
  `Available`, `Failed`), message, created and last used. Polling is `ListCopies` of one region. The codes
  `ImageCopyOnDemand`, `ImageCopying`, `ImageCopyFailed`, `ImageCopySourceMissing` join `CodeImageBuilding` in
  `pkg/vendors/images.go`. `vendors.Config` gains `ImageCopies` (the manager's narrow interface,
  `Lookup(ctx, req, demand bool) (*Image, error)`), nil when copies are off for the vendor.
- **Manager** (`pkg/imagecopy`, vendor-neutral, no Kubernetes client in the state machine): keys, the per-region
  unit of both architectures, queue, backoff, source choice, adoption, the release bump, the signal channel, the
  invalidation hooks, metrics; `Lookup` implements section 4 steps 4 and 5 for a request with the default selector and
  no owner, and nothing otherwise. A driver calls it after its primary lookup misses, before and outside its own
  answer cache, with `demand=true` from `ResolveImage` and `false` from `ReportImages` (the hcloud `ensureImage`
  pattern, shared).
- **GC** (`pkg/imagecopy/gc.go`: the pure `Decide` of section 6 and a leader-only runnable that lists the classes
  for rule (c) and the bump).
- **NodeClass controller**: the reasons of section 5, `ImagesCopying`, Events `ImageCopying` (Normal) and
  `ImageCopyFailed` (Warning) through the generalised `imageBuildEvents`, the requeue on the manager's signal.
  **Resources engine**: a gap carries its code, so `UnavailableFor` answers `ImageCopyPending` or `ImageGap`.
  **Catalog**: the code `ImageCopyPending` in the reason table. **CloudProvider**: a method that drops a cached image
  by ref, called by the manager's invalidation hook.
- **Operator**: flags, wiring (the manager is created per vendor with copies enabled and handed to `Init`), chart
  values and the deployment template.
- **Drivers**: AWS (`pkg/vendors/aws/imagecopy.go`), then Alibaba; the fake driver implements the capability with
  copy durations and failures on the fake clock, and the conformance suite gains the copy cases (adoption, idempotent
  start, tag scoping of delete, `ListCopies` after a restart).

**Metrics** (controller reference): `scarpenter_image_copies_started_total{vendor,region}`,
`scarpenter_image_copies_completed_total{vendor,region}`, `scarpenter_image_copies_failed_total{vendor,region,code}`,
`scarpenter_image_copy_duration_seconds{vendor}` (histogram), `scarpenter_image_copies_deleted_total{vendor,reason}`,
`scarpenter_image_copies{vendor,state}` (gauge: queued, pending, available, failed; the count of managed images).
Region labels are bounded by the vendor's regions.

**AWS IAM** (`aws-iam-policy.json`, optional statements, removed when copies are off):

| Statement | Allows |
|---|---|
| `Describe` (existing) | adds `ec2:DescribeImageAttribute` (for `lastLaunchedTime`) |
| `OptionalImageCopies` | `ec2:CopyImage`: on the new image and snapshots only with `aws:RequestTag/scarpenter.sparecores.com/cluster=CLUSTER_NAME`, on any source image |
| `OptionalImageCopiesTagAtCreation` | `ec2:CreateTags` only with `ec2:CreateAction=CopyImage` and the cluster tag |
| `OptionalImageCopiesDelete` | `ec2:DeregisterImage` on images and `ec2:DeleteSnapshot` on snapshots (for `DeleteAssociatedSnapshots`, **unverified** whether needed), with `aws:ResourceTag/...cluster=CLUSTER_NAME` |

No statement lets the role tag an existing image. Which condition keys `CopyImage` evaluates per resource is
**unverified** (step 4: a live call with the policy).

## 8. Costs

For the docs (figures from the owner and `flatcar-images.md`; per-vendor details **unverified** where marked in
section 2). AWS: a copy is billed once for the transfer (about $0.02/GB from US and EU sources) and then monthly for
snapshot storage in the destination (about $0.05/GB-month). One copy therefore costs as much as 0.02 / 0.05 months,
about **12 days of storage**. Deleting an idle copy earlier than that and copying again later costs more than keeping
it; the 30-day default `idleTTL` keeps a copy that is used at least monthly and pays at most one month of storage for
one that is not.

A Flatcar AMI's root volume is about 8 GiB provisioned, billed by the blocks written (smaller, **unverified**): at
most about $0.16 per copy and $0.40 per month per region and architecture. A region holds both architectures
(decision 3), so **about $0.80 a month per region**. The worst case, every one of today's 13 image-less AWS regions
demanded: 26 copies, about $4 once and **about $10 a month**, per cluster. On Alibaba the storage is about a third of
AWS's and the copy's price is **unverified**; on UpCloud a template costs €0.33/GB-month, so the break-even there is
short and a shorter `idleTTL` may pay.

## 9. Steps (one agent each, each ending green)

1. **Contract, manager, fake.** `vendors.ImageCopier`, `Release`, `ImageCopy`, the codes,
   `vendors.Config.ImageCopies`; `pkg/imagecopy` (keys, the per-region unit, queue, backoff, source order, adoption,
   timeout, the release bump, signal, invalidation hooks, `Lookup`), `Decide` and the GC runnable; metrics; the flag
   `--image-copy-idle-ttl`; the fake driver's capability (a fake "official" image per region it is told, copies that
   finish after a fake duration, injectable failures and limit errors); conformance cases; ADR-032 (below). L1: every
   row of the reason table and of the GC table on a fake clock, including rule (c)'s 2 h of continuous unpinning and
   an in-use copy never deleted; one copy per key under concurrent demand, both architectures enqueued together, the
   cap of 4 and the queue order, backoff on a limit error, adoption of a pending copy after a restart; a request with
   `use`, with an `imageOwner`, and with `imageOwner` set to the default owner never copies.
2. **Status, gate, Events, cache invalidation.** The NodeClass controller's reasons and `ImagesCopying`, the signal
   requeue, the engine's gap code and the catalog code `ImageCopyPending` (with its reference row), the Events, and
   the CloudProvider's drop-by-ref of its image cache wired to the manager's hook (the only `create.go` change). L2
   envtest: the status walks `ImageCopyOnDemand` to `ImageCopying` to ready for both architectures; a failed copy is
   `False` for a named region only; the gate follows within one reconcile of the signal; Events deduplicated. L3 KWOK
   (fake driver, two regions, the cheap one without an image): the first pod launches in the dear region, no attempt
   is counted for the skip, the copy finishes on the fake clock, the next pod launches in the cheap region; the copy
   is deleted after `idleTTL`, and earlier when the fake publishes the official image (rule b); **the release bump**:
   with a node running from a copy, pinning a new release enqueues its copy at once, drift replaces the node in the same
   region after the copy, and the old copy goes by rule (c). A launch after a GC delete resolves afresh (no stale ID).
   Docs: `docs/explanation/image-copies.md` (why on demand, the precedence, pending and Karpenter's re-solve, demand
   wider than one launch, GC rules including the switch to a primary image and the release bump, per-cluster scoping,
   the costs and break-even of section 8 with the per-vendor table of section 2) and `docs/reference/image-copies.md`
   (every setting with default and validation per vendor, the opt-out and the `imageOwner` caveat, the reasons, tags,
   metrics, Events).
3. **AWS driver.** `imagecopy.go`: `Primary` (the existing default lookup), `CopySources` (the preference list over
   `DescribeImages` of the official owner per region, cached 1 h), `ListCopies` (`DescribeImages` owner `self` with
   the tag filters, `lastLaunchedTime` by `DescribeImageAttribute` for the available ones), `StartCopy` (`CopyImage`
   with the name, description, `TagSpecifications` for image and snapshot, `ClientToken`; no `Encrypted` flag),
   `CopyInUse` (`DescribeInstances` filter `image-id`, every state but terminated), `TouchCopy` (no-op), `DeleteCopy`
   (`DeregisterImage` with `DeleteAssociatedSnapshots`); error mapping of the copy calls; the lookup's call into the
   manager from `ResolveImage` and `ReportImages`, outside the driver's answer cache. The in-memory EC2 fake gains
   `CopyImage` (pending, then available after a fake duration), `DeregisterImage` with snapshots, image attributes,
   the limit error. Flag `--aws-copy-images`, chart values, the IAM statements and their `iam_test.go` cases,
   `docs/reference/aws-driver.md` (Images, Tags, IAM), `flatcar-images.md` (aws row), `infrastructure-handoff.md`
   (nothing to provide), and `docs/how-to-guides/copy-images-on-demand.md` (enable it per vendor, the permissions per
   vendor, how to see a copy, cost, uninstall cleanup, migration; a section per vendor as they land). L1 with the
   fake EC2 for every call and error; conformance.
4. **AWS L5 scenario (written by the agent, run by the coordinator).** A scenario `imagecopy` in `test/e2e-aws`
   (never in `all` or the smoke set, run by `-f scenarios=imagecopy`): a NodeClass limited to `ap-northeast-3` (no
   Flatcar AMI, verified), copies on, `--image-copy-idle-ttl=1m`; one small amd64 on-demand pod; assert `ImageCopying`,
   then a node in the region, then scale to zero and the copy's deletion. The lane policy gets the copy statements,
   the sweeper deletes images and snapshots with the run's cluster tag. Cost: two copies (under $0.40), minutes of
   storage and of the smallest instance, well under $1. The agent's gates are the offline ones (scenario self-test,
   shellcheck); the coordinator's live run settles: the copy limit's error code, the code for a region that is not
   enabled (`OptInRequired`?), whether encryption by default applies to the copy and with which key, the condition
   keys `CopyImage` evaluates, whether `DeleteAssociatedSnapshots` needs `ec2:DeleteSnapshot`, the copy duration, and
   whether `ap-northeast-3` is enabled in the lane account. Recorded in the plan.
5. **Alibaba.** `ImageCopier` with ECS `CopyImage` from the source region (`DestinationImageName`, tags;
   **unverified**: tags on `CopyImage`, the concurrency limit, the price, whether `DeleteImage` leaves the snapshots,
   RAM tag conditions, a last-launched record, else `TouchCopy` with a daily tag), the flags and chart values,
   `alicloud-ram-policy.json`, the driver reference, `set-up-alicloud.md`, and the Alibaba sections of the how-to
   (permissions, migration, section 10). L1 with the fake ECS, conformance; a manual L5 scenario like step 4, run by
   the coordinator.
6. **A distinct Event while copying.** Now that `fix/alicloud-slow-regions` is merged (0.7.5) and `create.go` is free:
   a Create whose only failures are `ImageCopying` returns its CreateError with a Normal Event `ImageCopying` on the
   NodeClass instead of the Warning `ImageFailed`. L1 and the L3 scenario of step 2 extended.
7. **OCI, after the coordinator's spike confirms a copy API.** If OCI has a cross-region custom-image copy, an
   `ImageCopier` like step 5. If only export and import exist, the step needs an ADR-032 amendment (the controller
   writes Object Storage objects and pre-authenticated requests) and the maintainer's go-ahead before code.
8. **UpCloud, after the coordinator's spike confirms a copy API** (cross-zone template clone). Without one, the
   per-zone import from the images repository URL is an import, outside ADR-032; the step is not started.

Each step updates in the same change the reference docs of what it adds (flags, Helm values, metrics, Events,
conditions, reason codes, IAM), per CONVENTIONS.

**ADR-032 (step 1): SCarpenter copies boot images it can already read into regions of the account, opt-in.** It
amends ADR-021: rung 1 and the operator's rung 3 image stay the default and the primary image; with the per-vendor
flag the controller may copy an image that already exists in the account or is public in another region of the same
vendor, never import one from outside. Why: the credential argument of ADR-021 is weaker for a copy than for an
import (no external source, writes only at creation and on tag-scoped images) and the gain is real (the cheapest
regions; 21 IaC copies of which most are idle). Rejected: the SCNodeClass as the place of the setting (section 3);
waiting in `Create`; eager copies to every region (the owner chose on demand); a CRD per copy; the resources
engine's lifecycles (section 1); copying only the demanded architecture (decision 3). `flatcar-images.md` gains a
line under the ladder and the aws and alicloud rows.

## 10. Rollout

1. A release with steps 1 to 3 and copies **off by default**: nothing changes for anyone.
2. AWS users opt in per controller; the coordinator's L5 run (step 4) and one production cluster with a NodeClass
   without a region list (where `ImagesPartial` lists the gaps today) are the first users. Watch the metrics.
3. A release with step 5 (Alibaba) and step 6; then 7 and 8 as their spikes allow.

**Migrating from IaC copies to SCarpenter's** (Alibaba today; the same for an AWS operator who copies AMIs with IaC
and names them in `images.use`), without downtime:

1. Upgrade, enable `--alicloud-copy-images` with the source region set to the region where the IaC imports. Nothing
   changes: the IaC copies are primary (precedence step 3) in every region that has one.
2. At the next Flatcar bump, the IaC imports into the source region only and stops copying. Regions without the new
   release become `ImageCopyOnDemand` and are copied when used. The old release's IaC copies stay until drift has
   replaced every node (no instance runs from them), then the IaC removes them as it does at every bump today. (The
   release bump of section 6 covers SCarpenter's own copies only; a region whose nodes run from an IaC copy is gated
   for the copy's duration at the bump.)
3. Removing an IaC copy of the **current** release before a bump is safe in a region once no instance runs from it,
   or once the region's image status shows a SCarpenter copy (`ImageCopying` first: the region is gated for the copy's
   duration, so do it region by region). A NodeClass that names IaC images in `images.use` must drop that selector
   first, or it never gets copies.

## 11. Failure modes

| Failure | Effect | Handling |
|---|---|---|
| copy refused (region not enabled, permission missing, unsupported) | region gated | `ImageCopyFailed` for 6 h, Warning Event, metric with the code |
| copy limit or throttling | copy waits | queued with backoff |
| copy stuck pending past 2 h | region gated | deregistered, `ImageCopyFailed` |
| no source has the image (a release not yet published, the Alibaba source empty) | region gated | `ImageCopySourceMissing` |
| leader change mid-copy | none | the new leader adopts the pending image by its tags |
| GC deletes a copy a launch still holds | none | the delete invalidates the manager's and the CloudProvider's caches; the driver never caches copies |
| Flatcar bump | the copy-only region is gated for one copy's duration | the release bump of section 6 |
| two clusters in one account | two copies | by design; cluster names must be unique per account |
| a launch template references a copy | not protected | documented; only running instances protect a copy |
| cluster deleted | its copies remain | documented listing by tag; the e2e sweeper |
| a copy is deleted by hand | next lookup misses | back to `ImageCopyOnDemand` |
| encryption by default in the account | the copy may be encrypted with the default key | settled in step 4; a customer default key needs the existing KMS statements |

## 12. Tests per lane

- **L1** (unit, fakes, fake clock): the manager, `Decide`, source order, the opt-out cases, the AWS calls against the
  in-memory EC2, IAM policy cases, flag validation, the conformance copy cases on the fake driver and AWS.
- **L2** (envtest): the status reasons, the gate and the signal, Events.
- **L3** (KWOK, fake driver): the two-region scenario of step 2, with rules (a) and (b), the release bump and the cache
  invalidation.
- **L4**: none (qemu has no regions).
- **L5**: the manual `imagecopy` scenario per vendor (steps 4 and 5), never in `all` or smoke, run by the
  coordinator; the sweeper cleans up.

## 13. Open questions

**For the maintainer:**

1. Decision 1 (controller flags, not the SCNodeClass), decision 2 (per-cluster copies), decision 3 (both
   architectures per region), decision 5 (fixed source list): confirm.
2. Should the hcloud builder adopt rule (c) for superseded snapshots (a small follow-up)?

**Unverified facts** (settled by the step named):

- AWS, by the coordinator's live run in step 4: the concurrent-copy limit and its error code; the code for a region
  that is not enabled; encryption by default on `CopyImage`; the IAM condition keys `CopyImage` evaluates; whether
  `DeleteAssociatedSnapshots` needs `ec2:DeleteSnapshot`; the copy duration; the billed snapshot size; whether
  `ap-northeast-3` is enabled in the lane account. Per-pair transfer prices stay the owner's figures.
- AWS `lastLaunchedTime`: updated within about 24 h of a launch (AWS documentation; step 3 confirms the attribute in
  the vendored SDK, step 4 live).
- Settled by the vendored SDK: `CopyImage` `TagSpecifications` and `ClientToken`; `DeregisterImage`
  `DeleteAssociatedSnapshots`; `Image.SourceImageId` and `SourceImageRegion`.
- Alibaba (step 5): tags on `CopyImage`, its concurrency limit and price, snapshot deletion, RAM tag conditions, a
  last-launched record.
- OCI (step 7) and UpCloud (step 8): a copy API, by the coordinator's spikes. OVH: Glance `web-download` (follow-up).

## Not here

OVH and any import from outside the account; pre-warming regions; sharing copies across clusters or accounts; a CRD
for copies; customer KMS keys for copies; GPU-specific images; Azure gallery copies of Marketplace images.

## Gates

CONVENTIONS gates 1 to 4 for every step; `hack/check-docs.py` clean; no change under `pkg/unavailable`; the only
`create.go` changes are step 2's cache drop and step 6's Event; with every copy flag off, the golden instance types and
the status of the existing fixtures are byte-identical; no account ID, ARN, cluster name or credential in a tag
example, doc, log line or metric label (examples use `123456789012` and `my-cluster`); the `docs/plans/v1/README.md`
status `review`. Report: the unverified facts each step settled, with the result.
