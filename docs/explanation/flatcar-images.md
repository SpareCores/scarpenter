# Flatcar images

Where the boot image of a node comes from, per vendor, and why. The decision is
[ADR-021](DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10);
what your infrastructure as code must provide is in the
[hand-off reference](../reference/infrastructure-handoff.md). Facts about vendors
and Flatcar were researched on 2026-11-10 (Flatcar stable 4757.2.3, LTS
4081.3.12) and go stale: the vendor plans in
[`../plans/v1/wip/`](../plans/v1/wip/) list what is still unverified.

The release the CI of this repository tests comes from the repository variables
`FLATCAR_CHANNEL` and `FLATCAR_VERSION`, set by the operator of the CI account. A bump
means updating the in-repo pins so that `hack/check-flatcar-pins.sh` passes (see
[the e2e how-to](../how-to-guides/testing-and-releasing.md#the-flatcar-release-of-the-e2e-lanes)).

## Table of Contents

- [The ladder](#the-ladder)
- [Why a ladder](#why-a-ladder)
- [How a pinned version finds its image](#how-a-pinned-version-finds-its-image)
- [Overview](#overview)
- [Per vendor](#per-vendor)
- [Publishing an image for everyone, and what it costs](#publishing-an-image-for-everyone-and-what-it-costs)
- [Later moves](#later-moves)

## The ladder

Every node boots the Flatcar version pinned in its NodeClass
(`spec.flatcar.channel` and `version`, ADR-012). For each vendor the image comes
from the first rung that works. "Works" means the pinned version can be named and
read back from the image.

1. **A public Flatcar image the vendor offers.** SCarpenter uses it. Nobody has
   anything to create.
2. **The vendor lets anyone publish public images.** Spare Cores publishes
   Flatcar images there, built and released from a separate GitHub repository
   (proposed name `SpareCores/flatcar-images`, [plan 22](../plans/v1/wip/22-flatcar-images.md)).
   Nobody has anything to create.
3. **Images can be created in your own account.** You pre-create them, per region
   where the vendor's images are regional, with infrastructure as code (any
   tool) and pass their IDs to
   SCarpenter, or name them by the documented convention. The source is Flatcar's
   official image for the platform. Where Flatcar ships none, it is the image the
   images repository builds: the generic Flatcar image with the OEM id set
   (`vultr`, `upcloud`, `aliyun`).
4. **SCarpenter imports or builds the image itself** (`SCImage`,
   `ImageImporter`). Reserved, not built in v1: no vendor needs it.

Beside the ladder, with a per-vendor flag
([ADR-032](DECISIONS.md#adr-032-scarpenter-copies-boot-images-it-can-already-read-into-regions-of-the-account-opt-in-accepted-2026-11-23)),
SCarpenter copies an image that already exists in the account or is public in another region
of the same vendor into a region that has none, when a launch wants the region. It never
imports an image from outside the vendor. Rung 1 and your rung 3 image stay the primary
image wherever they exist.

## Why a ladder

- A vendor's own public image costs nothing, needs no maintenance by us and is
  what the vendor supports. Use it whenever the version can be pinned.
- Where only our own build exists, publishing it once for everybody (rung 2)
  beats every user building it. It needs a vendor that allows public images.
- Where nothing can be made public (Hetzner snapshots, UpCloud templates and
  Vultr snapshots document no sharing at all; OVH images are shared only with
  projects that accept them), the copy is made in the account by tooling that
  exists already, IaC. Which vendors could take a public image, and at what
  cost, is [below](#publishing-an-image-for-everyone-and-what-it-costs). That keeps credentials that can
  write images out of the controller, which only reads them.
- Hetzner is the exception (rung 4, ADR-030): it has no import API and no IaC resource
  for one, and its token is read-write for the whole project anyway, so the hcloud
  driver builds the snapshot itself.
- A vendor's own "Flatcar" entry whose version cannot be read back is not a rung:
  it would break ADR-012 and ADR-017 (never boot another version silently).
- The OEM id is the one thing a generic Flatcar image lacks on a platform
  without an official image. It sets the platform Ignition reads user-data from;
  without it the image runs Ignition as `qemu` and boots unconfigured. Setting it
  is one file on the OEM partition (`set oem_id="<id>"` in `grub.cfg`), which is
  all the images repository's build does besides recompressing the image into
  the format the vendor imports.

## How a pinned version finds its image

The driver resolves `(region, architecture, channel, version)` to an image. By
default it uses the vendor's convention (below); the optional `images` resource
kind of the vendor block brings the image with the one
[selector](../reference/resource-selectors.md) of every resource kind: explicit IDs, a name
pattern, tags or labels, portable attributes (`architecture`, `owner`, `state`) and native
filters, `select` and `orderBy`:

```yaml
spec:
  vendors:
    hcloud:
      resources:
        images:
          use:
            ids: {"*": ["123456789", "987654321"]}   # both architectures; a global image under the key "*" (every region)
            # or, found by labels:  tags: {os: flatcar, flatcar-version: "{version}"}, match: {architecture: [amd64]}
```

Whichever way the image was found, the driver compares the image's own labels or
name with the pinned channel and version. A mismatch, or an image that does not
say, makes the region's images not ready (`ImagesReady`, with the reason
`ImageVersionMismatch` or `ImageVersionUnknown`, naming the region and the image): the
node is never booted from another version. This holds for an image the NodeClass names
by ID as much as for one found by name: there is no softer rule that boots an unlabelled image
with a warning, because it would break ADR-017 (a benchmark must know what it ran on). An image ID is not part of the drift
hash; the pinned version is.

## Overview

| Vendor | Rung | Image source | Regional or global | Created by |
|---|---|---|---|---|
| aws | 1 | Flatcar's AMIs | regional | Flatcar |
| azure | 1 | Marketplace offers of publisher `kinvolk` | global offer, per-region versions | Flatcar |
| gcp, amd64 | 1 | public project `kinvolk-public` | global | Flatcar |
| gcp, arm64 | 2 | Spare Cores GCP project, shared with `allAuthenticatedUsers` | global | images repository |
| hcloud | 4 | Flatcar's `hetzner` image as a snapshot | global in the project | the hcloud driver (or you, with building off) |
| ovh | 3 | Flatcar's `openstack` image in Glance | regional | your IaC |
| oci | 3 | Flatcar's `oraclecloud` image, imported as a custom image | regional | your IaC |
| upcloud | 3 | images repository, OEM id `upcloud` | per zone and storage tier | your IaC |
| vultr | 3 | images repository, OEM id `vultr` | global in the account | your IaC |
| alicloud | 3 | images repository, OEM id `aliyun` | regional | your IaC |

## Per vendor

### aws

| | |
|---|---|
| Rung | 1 |
| Source | Flatcar's public AMIs, owner `075585003325` (`aws.imageOwner` overrides it) |
| Scope | regional: one AMI per region and architecture; a region without a published image reports its images not ready |
| Created by | Flatcar |
| Pinned version | the name `Flatcar-<channel>-<version>-hvm` (amd64) or `Flatcar-<channel>-<version>-arm64-hvm`; exactly one match. An AMI you name with `images` says its release by that name or by the tags `flatcar-channel` and `flatcar-version` |
| Architectures | amd64, arm64 |
| Later | opt-in copies into regions without a Flatcar AMI (ADR-032, plan 29, `--aws-copy-images`; [image copies](image-copies.md)) |

### azure

| | |
|---|---|
| Rung | 1 |
| Source | Azure Marketplace, publisher `kinvolk`: amd64 offer `flatcar-container-linux-corevm-amd64` (SKU `<channel>-gen2`), arm64 offer `flatcar-container-linux-corevm` (SKU `<channel>`, Generation 2 only) |
| Scope | global offers; a pinned version may be missing in a region (`ImagesPartial`) |
| Created by | Flatcar |
| Pinned version | the URN `kinvolk:<offer>:<sku>:<version>`: the marketplace version is the Flatcar release number |
| Architectures | amd64, arm64 (no arm64 LTS SKU listed) |
| Notes | SCarpenter never accepts marketplace terms; if an offer needs them the images are not ready with the command to accept them |
| Later | nothing |

### gcp, amd64

| | |
|---|---|
| Rung | 1 |
| Source | the public project `kinvolk-public`, one image per release |
| Scope | global: one image serves every region |
| Created by | Flatcar |
| Pinned version | the image name `flatcar-<channel>-<major>-<minor>-<patch>` (dots as dashes). Public images cannot be listed, so the driver reads one image by name: it exists or it does not |
| Architectures | amd64 |
| Notes | the images must carry the guest OS features newer machine series need (`GVNIC`); the first live spike checks it. An organization policy of the consumer can forbid images from other projects |
| Later | nothing |

### gcp, arm64

| | |
|---|---|
| Rung | 2 |
| Source | the generic arm64 image with the OEM id set for GCE, built by the images repository and published as a GCP image in a Spare Cores project, shared with `allAuthenticatedUsers` |
| Scope | global |
| Created by | the images repository (GitHub Actions, keyless identity into the project) |
| Pinned version | name `flatcar-<channel>-<major>-<minor>-<patch>-arm64` and the labels `flatcar-channel`, `flatcar-version` (dots as dashes: the label charset), `built-by` |
| Architectures | arm64 |
| Notes | Flatcar has no GCE arm64 image and no `oem-gce` sysext for arm64 (no guest agent, no OS Login); the build is unproven until the spike of [plan 16](../plans/v1/done/16-gcp.md) |
| Later | when Flatcar ships a GCE arm64 image it appears in `kinvolk-public` and arm64 moves to rung 1 |

### hcloud

| | |
|---|---|
| Rung | 4 (ADR-030): Hetzner offers no Flatcar image, its snapshots cannot be shared or made public, and it has no import API |
| Source | Flatcar's official `flatcar_production_hetzner_image.bin.bz2` from `<channel>.release.flatcar-linux.net`, written by a temporary server in rescue mode and snapshotted (the procedure and library of `hcloud-upload-image`) |
| Scope | global in the project, bound to an architecture; a snapshot boots in any location |
| Created by | the hcloud driver, when the label lookup finds none (one build at a time, leader only); with `--hcloud-build-images=false` or an `images` selector, you (`hcloud-upload-image`, [the how-to](../how-to-guides/set-up-hcloud.md#1-the-flatcar-image)) |
| Pinned version | snapshot labels `os=flatcar`, `flatcar-channel=<channel>`, `flatcar-version=<version>` and the architecture; exactly one match |
| Architectures | amd64, arm64 |

### ovh

| | |
|---|---|
| Rung | 3 (no public Flatcar image; community visibility is untested) |
| Source | Flatcar's official OpenStack image `flatcar_production_openstack_image.img` (qcow2), uploaded to Glance; decompress before upload |
| Scope | regional: one Glance per region, nothing is copied across |
| Created by | your IaC (Glance upload in every region) or `openstack image create` |
| Pinned version | name `flatcar-<channel>-<version>-<arch>`, Glance tags `flatcar` and `flatcar-<channel>-<version>`, properties `flatcar_channel`, `flatcar_version`; exactly one match |
| Architectures | amd64 only in v1 (the catalog has no OVH arm64 flavors) |
| Later | if OVH lets a project publish `community` images, Spare Cores publishes from one project in every region (rung 2) |

### upcloud

| | |
|---|---|
| Rung | 3 (no public Flatcar template, and templates cannot be shared) |
| Source | the images repository's `flatcar-upcloud-<channel>-<version>-amd64.img.gz`: the generic image with OEM id `upcloud`, raw and gzip (UpCloud imports raw, not qcow2 or bz2) |
| Scope | per zone, and per storage tier (a clone keeps the template's tier) |
| Created by | your IaC: an import into a storage, then a template, in every zone |
| Pinned version | template labels `scarpenter-image=flatcar`, `flatcar-channel`, `flatcar-version`, `flatcar-arch`, `flatcar-tier`; title `flatcar-<channel>-<version>-amd64-<tier>` |
| Architectures | amd64 |
| Notes | the LTS channel is unsupported until an LTS release ships Ignition 2.23 or later (the `upcloud` platform) |
| Later | when Flatcar ships an official `upcloud` image the source switches to it |

### vultr

| | |
|---|---|
| Rung | 3 (snapshots cannot be shared; Vultr's own "Flatcar" OS entries do not say which version they hold) |
| Source | the images repository's raw image with OEM id `vultr`, at a public URL; Vultr fetches it itself into a snapshot (`create-from-url`) |
| Scope | global in the account (a snapshot boots in any region) |
| Created by | your IaC, once per pinned version in each account |
| Pinned version | snapshot description `flatcar-<channel>-<version>-<arch>`; exactly one match |
| Architectures | amd64 |
| Later | the Vultr Marketplace could carry a Flatcar base image (rung 2) if Vultr allows it; Vultr's own OS entries only if their version can be read back; an official upstream image replaces the source |

### alicloud

| | |
|---|---|
| Rung | 3 now, rung 2 later |
| Source | the images repository's `aliyun` image (generic image with OEM id `aliyun`), imported from an OSS object in one region and copied to the others (`ImportImage`, `CopyImage`) |
| Scope | regional |
| Created by | your IaC |
| Pinned version | image name `Flatcar-<channel>-<version>-<arch>` and tags `flatcar-channel`, `flatcar-version`, `arch`, `built-by`; exactly one match |
| Architectures | amd64, arm64 |
| Later | if Spare Cores' account may publish community images, the repository publishes them per region (rung 2) and `images` becomes optional; an upstream `oem-aliyun` replaces the source. Opt-in copies from your imported image into the regions that have none, instead of IaC copies to every region (ADR-032, plan 29) |

### oci

| | |
|---|---|
| Rung | 3 (OCI images are regional and private to a tenancy; no public Flatcar image) |
| Source | Flatcar's official `flatcar_production_oraclecloud_image.img` (qcow2), uploaded to Object Storage and imported as a custom image in every region (a bucket in one region and a read-only pre-authenticated request import into the others) |
| Scope | regional, in the controller's compartment, bound to an architecture |
| Created by | your IaC; source type `QCOW2`, launch mode `PARAVIRTUALIZED`, operating system `Flatcar` and the Flatcar version as operating system version, shape compatibility for every shape that may launch; arm64 also needs an image capability schema |
| Pinned version | display name `flatcar-<channel>-<version>-<arch>` and freeform tags `flatcar-channel`, `flatcar-version`, `arch`, `os=flatcar`; exactly one match |
| Architectures | amd64, arm64 (A1 verified; A2 and A4 untested) |
| Later | a Community Application per region and release would make the import unnecessary (rung 2) |

The steps are in [Flatcar images on OCI](../how-to-guides/set-up-oci.md#1-the-flatcar-image).

## Publishing an image for everyone, and what it costs

Researched on 2026-11-16 from the vendors' public documentation. "Unverified" means the
documentation did not confirm it; prices are list prices that change, so treat them as
orders of magnitude.

**In short:** where a vendor lets a third party publish an image to every customer, doing so
costs Spare Cores the same as keeping the image private. The publisher pays the image's
storage, once per region where the vendor's images are regional, and nobody pays a fee
because the image is public. Consumers pay only for their own instances and disks. The
differences between vendors are the gates (a verified business, a partner programme, a review)
and the per-region work, not money. Where only private images exist, every user pays for and
maintains their own copy.

| Vendor | Public publishing (mechanism) | Gate | Extra cost of public vs private | Private route (per user) |
|---|---|---|---|---|
| AWS | yes: AMI launch permission `group=all` | "Block public access for AMIs" off in the region; no encrypted or product-code AMIs; public AMIs deprecate after two years by default | none: the publisher pays the EBS snapshots per region (about $0.05/GB-month, archive $0.0125) | not needed (Flatcar publishes) |
| Azure | yes: Compute Gallery community sharing, or the Marketplace | community gallery prerequisites (public name prefix, EULA, contact) unverified | none for the gallery: storage per replica per region, plus egress of the first replication | direct shared gallery (preview) or RBAC; not needed (Flatcar publishes) |
| GCP | yes: `allAuthenticatedUsers` with `roles/compute.imageUser` (`allUsers` is refused for images); public images cannot be listed, consumers name project and image | none documented | none documented; images are global (one copy serves every region), storage price unverified | an image in the user's project, imported from a Cloud Storage tarball |
| Hetzner | no mechanism documented: snapshots move only between projects of one account (unverified, probably impossible) | n/a | n/a | rescue-mode `dd` to a temporary server, then a snapshot per project and architecture (`hcloud-upload-image` automates it); about €0.0143/GB-month on the compressed size (unofficial figure) |
| OVHcloud | no public visibility for customers (unverified); an image can be shared, per region, with projects of any account that accept it | each target project accepts the share; revoking or deleting the image can break instances launched from it | no sharing fee documented; images are billed per GB (rate unverified) | `openstack image create --private` per region |
| OCI | yes: a Marketplace community application made from the custom image, in the regions where it is published | an IAM policy in the publishing tenancy; whether Oracle reviews community applications is unverified (paid listings need approval and carry a publisher fee) | none documented for community applications; storage per region (Object Storage about $0.0255/GB-month; whether the custom image itself is billed is unverified) | import from an Object Storage URL (a pre-authenticated request of the exported QCOW2) per region |
| UpCloud | no mechanism documented (unverified) | n/a | n/a | Storage Import of a raw image from a URL, or `dd` to an extra disk, then a custom template; €0.33/GB-month per user copy |
| Vultr | only through the Marketplace vendor programme (snapshots cannot be shared) | a vendor account, an application profile, review; fees, commission and who pays storage unverified | unverified | `snapshot create-url` from a raw image URL; $0.05/GB-month on the compressed size |
| Alibaba Cloud | yes: "Publish as Community Image", visible to every account in that region | an identity-verified enterprise account, by support ticket (US regions: by signing the terms); an image check; community images cannot be copied, shared or exported, so each region is imported and published on its own | no publishing fee documented; storage per region (snapshot about $0.017/GB-month, plus OSS during the import) | OSS upload and `ImportImage` per region, then `CopyImage` |

What this means for the ladder:

- **GCP arm64** (rung 2) stays the cheapest public image: one global copy per release.
- **OCI and Alibaba Cloud** could move to rung 2 at no extra running cost. OCI needs a
  community application per region; Alibaba Cloud needs a verified enterprise account and an
  import per region, because its community images cannot be copied.
- **Vultr** could move to rung 2 only through the Marketplace, whose terms are unverified.
- **UpCloud and OVHcloud** stay at rung 3: no public route is documented, and an OVH
  share needs every user's project to accept it. **Hetzner** has no public route either; its
  driver builds the snapshot in the user's project (rung 4, ADR-030).

Sources: [AWS sharing AMIs](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/sharingamis-intro.html),
[EBS snapshot archive pricing](https://docs.aws.amazon.com/ebs/latest/userguide/snapshot-archive-pricing.html),
[Azure Compute Gallery](https://learn.microsoft.com/en-us/azure/virtual-machines/azure-compute-gallery),
[GCP managing access to custom images](https://cloud.google.com/compute/docs/images/managing-access-custom-images),
[Hetzner snapshot FAQ](https://docs.hetzner.com/cloud/servers/backups-snapshots/faq/),
[hcloud-upload-image](https://pkg.go.dev/github.com/apricote/hcloud-upload-image/hcloudimages),
[OVHcloud sharing images between projects](https://docs.ovhcloud.com/en/guides/public-cloud/compute/share-images-between-projects),
[OVHcloud uploading your own image](https://docs.ovhcloud.com/en/guides/public-cloud/compute/upload-own-image),
[OCI publishing community applications](https://docs.oracle.com/en-us/iaas/Content/Marketplace/Tasks/publishingcommunityapplications.htm),
[OCI image import and export](https://docs.oracle.com/iaas/Content/Compute/Tasks/imageimportexport.htm),
[UpCloud storage import](https://upcloud.com/docs/guides/storage-import/),
[UpCloud pricing](https://www.upcloud.com/pricing/),
[Vultr Marketplace vendors](https://docs.vultr.com/platform/marketplace-vendor),
[Vultr snapshot FAQ](https://docs.vultr.com/products/storage/snapshots/faq),
[Alibaba Cloud community images](https://www.alibabacloud.com/help/en/ecs/user-guide/overview-12),
[Alibaba Cloud ImportImage](https://www.alibabacloud.com/help/en/ecs/developer-reference/api-ecs-2014-05-26-importimage).

## Later moves

Nothing in the ladder is final; a vendor moves up when the world changes, and the
only change for users is that `images` becomes optional or its source changes:

- **Upstream Flatcar ships a platform** (GCE arm64, `upcloud`, `vultr`, `aliyun`):
  the images repository proposes them and stops building a platform from the
  release that carries it. Versions already built stay available for pins.
- **OVH community visibility**, **Alibaba community images** or an **OCI Community Application**: rung 3 to rung 2.
- **The Vultr Marketplace**: rung 3 to rung 2, if Vultr allows a base image.
- **Rung 4** (`SCImage`, `ImageImporter`) only if a vendor appears for which IaC
  cannot create the image.
