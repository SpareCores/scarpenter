# Image copies

Why and how SCarpenter copies the boot image of the pinned Flatcar release into a
region that has none, only when a launch wants that region, and deletes the copy
when nothing uses it. The decision is
[ADR-032](DECISIONS.md#adr-032-scarpenter-copies-boot-images-it-can-already-read-into-regions-of-the-account-opt-in-accepted-2026-11-23);
the settings, reasons, tags, metrics and Events are in the
[reference](../reference/image-copies.md).

## The problem

Flatcar publishes its AMI in most AWS regions, not in all of them, and the regions
it leaves out are not the dear ones: some of them have the cheapest spot prices of
the vendor. Without an image a region's offerings are unavailable
([`ImageGap`](../reference/labels-and-offerings.md#why-a-type-or-an-offering-is-not-offered)), however cheap.
On Alibaba Cloud the image is imported into one region by your IaC and copied into
every region of every NodeClass, most of which never launch a node.

With copies on, for a vendor whose driver can copy, SCarpenter does what the IaC
did, but for the regions that are used: it copies the image from a region that has
it, tags the copy as its own and deletes it when it is idle.

## Why on demand

Copies to every region ahead of time would cost storage in regions that never
launch (about $1.30 a month per region for both architectures on AWS) and would copy
every release again at every Flatcar bump. A copy made when a launch asks for the
region costs nothing for the regions nobody wants, and the idle TTL
(`--image-copy-idle-ttl`, 30 days) pays for itself: a copy is billed once for the
transfer and then monthly for the snapshot, so deleting it earlier than about 12 days
and copying again later costs more than keeping it.

Copies are opt-in per vendor, a flag of the controller and not a field of the
NodeClass: a copy belongs to the account, every class that pins the release uses it,
the garbage collector deletes it account-wide, and the write permissions are the
controller's.

## The lookup, in order

For a region, architecture and pinned release the driver looks for the image in this
order and stops at the first answer:

1. `resources.images.use` (the images you name): that selector only. Never copies.
2. `imageOwner`: that owner's image by the vendor's convention only. Never copies.
   Spelling out the default owner counts: any `imageOwner` opts the class out.
3. **The primary image**, the vendor's own convention (the official AMI, your
   imported image). If it exists it is used, whatever copies exist.
4. **The copy manager**: this cluster's available copy of the release, answered from
   memory.
5. With copies on, the state of a copy decides the answer; with them off the image is
   not found, as before.

The name of a copy is never the official one, so steps 3 and 4 cannot collide, and
the version check of [ADR-017](DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05)
reads the copy's `flatcar-channel` and `flatcar-version` tags like any other image.

## Pending is a gate, not a wait

A launch never waits for a copy. The first launch that wants an image-less region
finds the region in step 5: the manager queues the copy and answers
`ImageCopying`, which the CloudProvider treats like any image failure: the candidate
is skipped, no attempt is counted against `spec.launch.maxAttempts`, and the walk
goes on to the next offering. Every other candidate in that region gets the same
answer from memory, without a call to the vendor.

Seconds later the manager signals the NodeClass controller, which reports the region
as `ImageCopying` and tells the resources engine, which marks the region's offerings
unavailable with the catalog reason
[`ImageCopyPending`](../reference/labels-and-offerings.md#why-a-type-or-an-offering-is-not-offered). Karpenter's
next solve no longer sees them and places the pods elsewhere; a pod that fits nowhere
else stays Pending while the copy runs. If a NodeClaim had only candidates in that
region, `Create` fails with `ImageResolutionFailed`, core retries, finds no candidate
and deletes the NodeClaim, and the scheduler re-solves without the region.

When the copy is available the manager signals again, the gate lifts and the pending
pods launch there.

Why the image gate and not the unavailable-offerings cache (`pkg/unavailable`): the
gate already exists for regions without an image, is per class and region, and
reaches Karpenter through the catalog generation. The price is the window between
the first skip and the gate, seconds in which other launches skip the region the
same way.

Before any launch wants the region, a report shows `ImageCopyOnDemand` and the
offerings stay available: the gate must not hide the region from the demand that
starts the copy.

## Demand is wider than one launch

One `Create` walks every candidate in price order, so a single NodeClaim can start
copies into several image-less regions cheaper than the one it ends up launching in,
and consolidation's replacement NodeClaims do the same. That is intended: they are the
cheaper regions. The number is bounded by the image-less regions of the class, two
copies each (both architectures); AWS has 13 such regions today, about 26 copies, $7
once and $17 a month in the worst case. A class that must not cause copies keeps
those regions out of its NodePools' `topology.kubernetes.io/region` requirements,
lists its own regions, or names its images.

## Both architectures together

The gate is per region, not per architecture: a region is gated only when no
architecture has a ready image. Copying only the demanded architecture would ungate
the region for both and send the other architecture's launches into skips. So demand
for a region queues the release for every architecture the driver serves there, as one
unit; the region's images are `ImageCopying` together and the gate lifts when all of
them are available or failed.

## Garbage collection

The leader runs a collection every `--gc-interval` over the copies of this cluster in
every region of the vendor. A copy is **never** deleted while an instance of the
account (any cluster, any state but terminated) runs from it. Otherwise it goes when:

| Rule | Condition |
|---|---|
| idle | the latest of the vendor's last-launch record, the manager's last resolve and the creation is older than `--image-copy-idle-ttl` |
| primary appeared | the primary lookup now finds an image in the region (the vendor published its own, or your IaC imported one) and no launch resolved the copy for 2 hours, the launch cache |
| unpinned | no SCNodeClass of this cluster has pinned the release for 2 hours without a break (the clock restarts with the process) |
| duplicate | another available copy of the same release, region and architecture is older (a race) |
| failed | the image is in a failed state, or pending longer than the 2 hour copy timeout |

Each deletion is logged, counted, announced by an Event on the controller's
Deployment (a copy belongs to no class), and drops the image from every cache that
holds its ID: the manager's state and the CloudProvider's image cache. Without that a
deleted image could be served for up to an hour and its invalid-image error, a `Fatal`
launch error, would end the `Create` walk. A launch after a deletion resolves afresh.

Not seen by the collector: launch templates or other users' configuration that
reference a copy (only running instances protect it), and the copies of a deleted
cluster (nobody deletes them; the [reference](../reference/image-copies.md#tags) gives
the tags to list them by).

## Per cluster

The copy's ownership tag is the cluster name. Two clusters in one account copy twice
(cents a month each) but never delete each other's copy, and the IAM conditions on the
cluster tag work as they do for instances. Sharing across clusters would need a
reference count nobody can maintain once a cluster is gone. Cluster names (`--cluster-name`)
must therefore be unique per account.

## The release bump

When a class pins a new Flatcar release, the new release has no copy. A region served
by a copy would be gated, drift would replace its nodes elsewhere, and consolidation
would move them back later. To avoid the evacuation, a release that is new to the
controller is handed to the manager when the NodeClass controller sees it (and the
manager is handed the pinned releases at every signal and every collection): the
manager queues it, at once, in every region where this cluster has a copy of another
release that an instance runs from. Drift then finds the region gated for the copy's
duration (about six minutes on AWS), instead of losing it. The copy of the old release is protected while
its instances run, and goes by the idle or unpinned rule afterwards.

The bump covers SCarpenter's own copies. A region whose nodes run from an image your
IaC copied is gated for the copy's duration at the bump.

## What a copy costs

On AWS a copy takes about six minutes (measured: 365 s) and both architectures of a
region are copied together. Each copy's snapshot is 13 GiB (measured, standard tier), so
26 GiB per region. A copy is billed once for the transfer and monthly for the snapshot
in the destination. At the assumed prices of $0.02/GB for the transfer (from US and EU
sources) and $0.05/GB-month for standard snapshot storage, that is about $0.52 once and
about **$1.30 a month per region** for both architectures. The copies are unencrypted
unless the account's EBS encryption by default is on; a customer KMS key as that default
is untested. On Alibaba Cloud the storage is about a third of AWS's and
the price of a copy is unverified; on UpCloud a template costs €0.33/GB-month, so the
break-even is short and a shorter idle TTL may pay.

## Vendors

| Vendor | Images are | Copy API | Source of a copy |
|---|---|---|---|
| aws | regional | `CopyImage` (implemented) | Flatcar's official AMI in another region (a fixed preference list: `us-east-1`, `us-east-2`, `us-west-2`, `eu-west-1`, `eu-central-1`, then any region that has it) |
| alicloud | regional | ECS `CopyImage` | the operator's imported image in a source region |
| oci | regional, per tenancy | to be confirmed | the operator's imported image in one region |
| upcloud | per zone and storage tier | to be confirmed | the operator's template in one zone |
| ovh | regional (Glance) | none: an import, outside ADR-032 | |
| gcp, azure, hcloud, vultr | global, or built by the driver | not needed | |

Importing an image from outside the account (rung 4 of the
[image ladder](flatcar-images.md)) stays hcloud's alone ([ADR-030](DECISIONS.md)).

## Migrating from IaC copies

Enable the vendor's flag with the IaC still copying: nothing changes, an IaC copy is
the primary image and wins in every region that has one. At the next Flatcar bump the
IaC imports into the source region only and stops copying; the regions without the new
release become `ImageCopyOnDemand` and are copied when used. The old release's IaC
copies stay until drift has replaced every node, then the IaC removes them as it does at
every bump. Removing an IaC copy of the current release is safe in a region once no
instance runs from it, or once the region's image status shows SCarpenter's copy
(`ImageCopying` first: the region is gated for the copy's duration, so go region by
region). A NodeClass that names IaC images in `images.use` must drop that selector first,
or it never gets copies.
