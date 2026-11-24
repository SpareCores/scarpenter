# Image copies

Reference for the on-demand copies of the boot image (plan 29,
[ADR-032](../explanation/DECISIONS.md#adr-032-scarpenter-copies-boot-images-it-can-already-read-into-regions-of-the-account-opt-in-accepted-2026-11-23)).
Why and how it works: [Image copies](../explanation/image-copies.md).

## Settings

Controller flags and Helm values, per vendor; no field of the SCNodeClass, no change
of the drift hash. Every flag is also an environment variable `SCARPENTER_*`
([controller reference](controller.md#flags-and-environment-variables)).

| Flag | Helm value | Default | Validation |
|---|---|---|---|
| `--aws-copy-images` | `vendors.aws.imageCopies.enabled` | `false` | bool; the chart renders it only when `vendors.aws.enabled` is true |
| `--alicloud-copy-images` | `vendors.alicloud.imageCopies.enabled` | `false` | bool; the chart renders it only when `vendors.alicloud.enabled` is true |
| `--image-copy-idle-ttl` | `imageCopies.idleTTL` | `720h` | a duration, at least `1m` |

A flag for a vendor that is not enabled, or whose driver cannot copy images, refuses
to start the controller. `--cluster-name` must be unique per account: copies are scoped
by it.

Constants, not flags: the retry after a failed copy (6 h), at most 4 copies in flight
per vendor (the rest queue in demand order), a backoff of 1 min doubling to 15 min after
a limit or throttling error, a copy timeout of 2 h, the retry after a missing source of
15 min, 5 min after which a copy the vendor does not list counts as failed, how often
copies in flight are looked at (30 s), the unpinned grace of 2 h and the primary grace of
2 h. The collection runs every `--gc-interval`, on the leader only.

### Opting a class out

A copy is made for a region only when a launch of a class **without** `resources.images.use`
and **without** `imageOwner` wants it. A class that must not cause copies keeps the
image-less regions out of its NodePools' `topology.kubernetes.io/region` requirements,
lists its own regions in the vendor block, or names its images with `resources.images.use`.

`imageOwner` set to the default owner (for AWS, `075585003325`) also opts the class out:
any `imageOwner` does, the value is not compared.

## Image status

`status.vendors.<vendor>.images[].reason`, per region and architecture:

| Reason | Meaning | Region gated | `ImagesReady` |
|---|---|---|---|
| (ready) | the primary image, or this cluster's available copy | no | as without copies |
| `ImageCopyOnDemand` | no image; a copy is made when a launch wants the region | no: the offerings stay available so that demand can reach a launch | `True`; the reason is `ImagesPartial` and the message lists "copied on demand" |
| `ImageCopying` | a copy is queued or in flight (the message names the source and the start) | yes, offerings are unavailable with `ImageCopyPending` | `True` with reason `ImagesCopying`, also for a region the NodeClass names: a pending copy is not a fault |
| `ImageCopyFailed` | the last copy failed less than 6 h ago (the message has the vendor's code and text) | yes, `ImageGap` | as a missing image: `False` for a region the NodeClass names (reason `ImageCopyFailed`), else `True` with `ImagesPartial` |
| `ImageCopySourceMissing` | copies are on but no source region has the image | yes, `ImageGap` | as a missing image (reason `ImageCopySourceMissing` when it blocks) |

A region is gated only when **no architecture** has a ready image, as for any image gap.
Both architectures of a region are copied together. After `ImageCopyFailed` the key
returns to `ImageCopyOnDemand` and the next launch that wants the region retries.

The catalog reason `ImageCopyPending` is documented with the other
[offering reasons](labels-and-offerings.md#why-a-type-or-an-offering-is-not-offered).

## Tags

Set only when the copy is created; logical keys go through the driver's tag convention.

| Key | Value |
|---|---|
| `scarpenter.sparecores.com/cluster` | the cluster name (the IAM condition; use it to list the copies of a cluster that is gone) |
| `scarpenter.sparecores.com/managed-by` | as on every managed resource |
| `scarpenter.sparecores.com/resource-kind` | `image-copy` |
| `flatcar-channel`, `flatcar-version` | the release (the keys the version check reads, unprefixed) |
| `scarpenter.sparecores.com/image-arch` | `amd64` or `arm64` |
| `scarpenter.sparecores.com/image-source` | `<region>/<image ID>` of the source, for vendors whose image does not say it |

The name of a copy is `scarpenter-flatcar-<channel>-<version>-<arch>-<cluster>`,
truncated to the vendor's limit with a hash, never the official name. Only images with the
cluster, managed-by and resource-kind tags are ever listed for deletion.

## Deletion rules

Evaluated per copy by the collector; a copy that an instance of the account runs from
is never deleted. The reason is the label of the metric and the text of the Event.

| Reason | Condition |
|---|---|
| `Idle` | the latest of the vendor's last-launch record, the manager's last resolve and the creation is at least `--image-copy-idle-ttl` ago |
| `PrimaryAvailable` | the primary lookup finds an image in the region and the manager's last resolve is at least 2 h ago |
| `Unpinned` | no SCNodeClass of this cluster pinned the release for 2 h continuously (in memory: the clock restarts with the process) |
| `Duplicate` | another available copy of the same release, region and architecture is older |
| `Failed` | the image is in a failed state, or pending longer than 2 h |

A deletion drops the image from the manager's state and from the CloudProvider's image cache.

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `scarpenter_image_copies_started_total` | counter | `vendor`, `region` | copies started, by destination region |
| `scarpenter_image_copies_completed_total` | counter | `vendor`, `region` | copies that became available |
| `scarpenter_image_copies_failed_total` | counter | `vendor`, `region`, `code` | copies that failed (the vendor's error code, `CopyFailed` or `CopyTimeout`) |
| `scarpenter_image_copy_duration_seconds` | histogram | `vendor` | from the start to availability |
| `scarpenter_image_copies_deleted_total` | counter | `vendor`, `reason` | deletions, by the reason above |
| `scarpenter_image_copies` | gauge | `vendor`, `state` | images the manager knows: `queued`, `pending`, `available`, `failed` |

The region label is bounded by the vendor's regions. The series exist only for vendors
with copies on.

## Events

| Reason | Type | On | Meaning |
|---|---|---|---|
| `ImageCopying` | Normal | SCNodeClass | an image of the class is being copied; one Event per class, architecture and outcome (deduplicated for 30 minutes), not one per region or reconcile |
| `ImageCopyFailed` | Warning | SCNodeClass | a copy failed; deduplicated like `ImageCopying` |
| `ImageCopyDeleted` | Normal | the controller's Deployment | the collector deleted a copy, with the image, release, region and reason (a copy belongs to no class) |

A launch that failed only because an image is being copied returns `ImageResolutionFailed`
with the Warning Event of the same name, like any image failure.

## Where the code reads from

| Piece | What it does |
|---|---|
| `pkg/vendors/imagecopy.go` | the driver capability `ImageCopier` and the manager's interface `ImageCopies` that a driver finds in `vendors.Config.ImageCopies` (nil with copies off) |
| `pkg/imagecopy` | the vendor-neutral manager (queue, backoff, source order, adoption, release bump, signal, invalidation) and the garbage collector |
| NodeClass controller | the reasons above, `ImagesReady`, the Events; reconciles the NodeClasses of a vendor on every signal of its manager and hands the manager the pinned releases |
| resources engine | tells the catalog `ImageCopyPending` or `ImageGap` per region |
| CloudProvider | `ForgetImage`: drops a deleted image from the cache of resolved images |
| fake driver | official images only where the steering key `imageCopies` lists regions (see [the fake driver](driver-contract.md#the-fake-driver)) |
