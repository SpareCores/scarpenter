# Copy images on demand

Let the controller copy the Flatcar image of the pinned release into a region that has
none, when a launch wants that region, and delete the copy when nothing uses it
(plan 29, [ADR-032](../explanation/DECISIONS.md#adr-032-scarpenter-copies-boot-images-it-can-already-read-into-regions-of-the-account-opt-in-accepted-2026-11-23)).
Why it works this way: [Image copies](../explanation/image-copies.md); every setting, reason,
tag, metric and Event: the [reference](../reference/image-copies.md).

Copies are off by default and opt-in per vendor, a setting of the controller and not of
the NodeClass. Today the AWS driver can copy; the sections of the other vendors follow as
their drivers land.

## Table of Contents

- [Before you start](#before-you-start)
- [AWS](#aws)
  - [1. Add the permissions](#1-add-the-permissions)
  - [2. Turn it on](#2-turn-it-on)
  - [3. See a copy](#3-see-a-copy)
  - [4. What it costs](#4-what-it-costs)
  - [5. Keep a class from causing copies](#5-keep-a-class-from-causing-copies)
  - [6. Migrate from your own copies](#6-migrate-from-your-own-copies)
  - [7. Uninstall and clean up](#7-uninstall-and-clean-up)
  - [Troubleshooting](#troubleshooting)

## Before you start

- `--cluster-name` (chart `cluster.name`) must be unique per account: copies are scoped by it, and
  the controller never touches an image that does not carry its cluster's tag.
- A copy is made for a NodeClass that does **not** name its images (`resources.images.use`) and has
  **no** `imageOwner`. Any `imageOwner`, also the default owner spelled out, opts the class out.
- A launch never waits for a copy. The region is skipped while the copy runs (minutes to tens of
  minutes) and Karpenter solves again; a pod that fits nowhere else stays Pending until the copy is ready.

## AWS

Flatcar publishes no AMI in some regions (for example `ap-northeast-3`, `ap-south-2`, `eu-south-2`, as of
2026-11-23); without copies a NodeClass reports them as `ImagesPartial` and their offerings are unavailable
(`ImageGap`), although some of them have the cheapest spot prices.

### 1. Add the permissions

The copy statements of [`aws-iam-policy.json`](../reference/aws-iam-policy.json) are optional and are
not needed without copies. Keep `OptionalImageCopies`, `OptionalImageCopiesTagAtCreation` and
`OptionalImageCopiesDelete` in the policy of the controller's role (and `ec2:DescribeImageAttribute` in
`Describe`, which the policy already lists), with `CLUSTER_NAME` replaced as for the other statements:

```bash
sed -e 's/CLUSTER_NAME/my-cluster/g' -e 's/123456789012/<account id>/g' aws-iam-policy.json > policy.json
aws iam create-policy-version --policy-arn arn:aws:iam::123456789012:policy/scarpenter-my-cluster \
  --policy-document file://policy.json --set-as-default
```

What they allow, and nothing else: `CopyImage` and the tags of the new image and snapshot only when the request
carries the cluster tag, with `OptionalImageCopiesSource` letting it read the official Flatcar account's images and snapshots as the
source; `DeregisterImage` and `DeleteSnapshot` only on images and snapshots that carry the tags of a copy (the cluster,
`managed-by` and `resource-kind=image-copy`). The
role cannot tag or change an existing image, so it cannot take over an image it did not create. The
[IAM table](../reference/aws-driver.md#iam) explains each statement.

The copy is made in the account that owns the controller's role, from Flatcar's public AMI in another region;
nothing is copied from outside the account's reach and no image is shared.

### 2. Turn it on

With the chart:

```yaml
vendors:
  aws:
    imageCopies:
      enabled: true
imageCopies:
  idleTTL: 720h   # how long an unused copy is kept (the default)
```

or the flags `--aws-copy-images` and `--image-copy-idle-ttl=720h` (environment variables
`SCARPENTER_AWS_COPY_IMAGES` and `SCARPENTER_IMAGE_COPY_IDLE_TTL`). The controller refuses to start when the
`aws` vendor is not enabled.

Nothing happens to regions that have an image. For a region without one the NodeClass status shows
`ImageCopyOnDemand`, and its offerings stay available so that demand can reach a launch. The first launch that
wants the region starts the copy of **both** architectures (the region is gated for both together).

### 3. See a copy

The status of the NodeClass, per region and architecture:

```bash
kubectl get scnodeclass default -o json |
  jq '.status.vendors.aws.images[] | select(.region == "ap-south-2") | {region, arch, ready, reason, message}'
```

The reasons walk `ImageCopyOnDemand`, `ImageCopying` (the region's offerings are unavailable with
`ImageCopyPending`), then ready. `ImageCopyFailed` keeps the region gated for 6 hours; the message has the
AWS error code. A Normal Event `ImageCopying` and a Warning Event `ImageCopyFailed` go on the NodeClass, and
`ImageCopyDeleted` on the controller's Deployment.

In AWS the copies are the account's own images named
`scarpenter-flatcar-<channel>-<version>-<arch>-<cluster>`, with the tags of the
[reference](../reference/image-copies.md#tags):

```bash
aws ec2 describe-images --region ap-south-2 --owners self \
  --filters "Name=tag:scarpenter.sparecores.com/cluster,Values=my-cluster" \
            "Name=tag:scarpenter.sparecores.com/resource-kind,Values=image-copy" \
  --query 'Images[].[ImageId,Name,State,SourceImageRegion,CreationDate]' --output table
```

The metrics are `scarpenter_image_copies_started_total`, `..._completed_total`, `..._failed_total{code}`,
`..._deleted_total{reason}`, the gauge `scarpenter_image_copies{state}` and the histogram
`scarpenter_image_copy_duration_seconds`.

A copy is deleted after `--image-copy-idle-ttl` without a launch (AWS records the last launch of an image within
about a day), when Flatcar's own AMI of the release appears in the region, or when no NodeClass has pinned the
release for 2 hours. It is never deleted while an instance of the account runs from it.

### 4. What it costs

A copy is billed once for the transfer (about $0.02/GB from US and EU sources) and then monthly for the snapshot
in the destination (about $0.05/GB-month). A Flatcar AMI is about 8 GiB provisioned and billed by the blocks
written, so at most about $0.16 per copy and $0.40 a month per region and architecture: **about $0.80 a month per
region**, both architectures together. If every one of the 13 regions without an image were used, that is about $4
once and about $10 a month, per cluster. Keeping a copy for less than about 12 days costs more than keeping it
(the transfer is worth that much storage), so a much shorter `idleTTL` rarely pays.

Two clusters in one account copy twice (cents a month each) and never delete each other's copy.

### 5. Keep a class from causing copies

A class that must not cause copies keeps the image-less regions out of its NodePools'
`topology.kubernetes.io/region` requirements, lists its own regions in `spec.vendors.aws.regions`, or names its
images with `resources.images.use` or `imageOwner`.

### 6. Migrate from your own copies

If your tooling copies the AMI today and a NodeClass names the copies in `images.use`, drop that selector first:
a class that names its images never gets copies. Enable the flag, keep the old copies until no instance runs from
them and remove them as before. The official AMI is always the primary image, so nothing changes in the regions
that have one.

### 7. Uninstall and clean up

The controller deletes its copies when they are idle, not when it is removed. Before you uninstall, set
`imageCopies.idleTTL` to `1m` and wait for the collection (`--gc-interval`, 2 minutes), or delete them by hand.
List every copy of the cluster in every region and deregister them with their snapshots:

```bash
for region in $(aws ec2 describe-regions --query 'Regions[].RegionName' --output text); do
  aws ec2 describe-images --region "$region" --owners self \
    --filters "Name=tag:scarpenter.sparecores.com/cluster,Values=my-cluster" \
              "Name=tag:scarpenter.sparecores.com/resource-kind,Values=image-copy" \
    --query 'Images[].ImageId' --output text | tr '\t' '\n' | while read -r ami; do
      [ -n "$ami" ] || continue
      aws ec2 deregister-image --region "$region" --image-id "$ami" --delete-associated-snapshots
    done
done
```

Check that no instance runs from an image before you deregister it.

### Troubleshooting

| Symptom | Cause |
|---|---|
| `ImageCopyFailed` with `UnauthorizedOperation` | the policy lacks the `OptionalImageCopies*` statements, or `CLUSTER_NAME` differs from `cluster.name` |
| `ImageCopyFailed` with `OptInRequired` or `AuthFailure` | the region is not enabled in the account |
| `ImageCopyFailed` naming a snapshot or encryption error | the account encrypts EBS by default with a customer key: the controller needs the [KMS statements](../reference/aws-driver.md#iam) for it |
| `ImageCopySourceMissing` | no region has the release's official AMI yet (a release that is not published); it is retried every 15 minutes |
| the copy stays `ImageCopying` for hours | a copy waiting to start, held by a limit or throttling backoff (1 to 15 minutes, doubling) or by a release whose sources cannot be listed, stays `ImageCopying` and has no timeout; the 2 hour copy timeout (`CopyTimeout`) applies only once a copy has started, after which it counts as failed |
| a region has `ImageCopyOnDemand` for ever | nothing launches there: a report never starts a copy |
