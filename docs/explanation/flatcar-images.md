# Flatcar images

Where the boot image of a node comes from, per vendor, and why. The decision is
[ADR-021](DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10);
what your infrastructure as code must provide is in the
[hand-off reference](../reference/infrastructure-handoff.md). Facts about vendors
and Flatcar were researched on 2026-11-10 (Flatcar stable 4757.2.3, LTS
4081.3.12) and go stale: the vendor plans in
[`../plans/v1/wip/`](../plans/v1/wip/) list what is still unverified.

## Table of Contents

- [The ladder](#the-ladder)
- [Why a ladder](#why-a-ladder)
- [How a pinned version finds its image](#how-a-pinned-version-finds-its-image)
- [Overview](#overview)
- [Per vendor](#per-vendor)
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

## Why a ladder

- A vendor's own public image costs nothing, needs no maintenance by us and is
  what the vendor supports. Use it whenever the version can be pinned.
- Where only our own build exists, publishing it once for everybody (rung 2)
  beats every user building it. It needs a vendor that allows public images.
- Where nothing can be shared (Hetzner snapshots, UpCloud templates, Vultr
  snapshots and OVH images cannot be made public), the copy is made in the
  account by tooling that exists already, IaC. That keeps credentials that can
  write images out of the controller, which only reads them.
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
say, makes the region's images not ready (`ImagesReady`, with the reason): the
node is never booted from another version. An image ID is not part of the drift
hash; the pinned version is.

## Overview

| Vendor | Rung | Image source | Regional or global | Created by |
|---|---|---|---|---|
| aws | 1 | Flatcar's AMIs | regional | Flatcar |
| azure | 1 | Marketplace offers of publisher `kinvolk` | global offer, per-region versions | Flatcar |
| gcp, amd64 | 1 | public project `kinvolk-public` | global | Flatcar |
| gcp, arm64 | 2 | Spare Cores GCP project, shared with `allAuthenticatedUsers` | global | images repository |
| hcloud | 3 | Flatcar's `hetzner` image as a snapshot | global in the project | your IaC |
| ovh | 3 | Flatcar's `openstack` image in Glance | regional | your IaC |
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
| Pinned version | the name `Flatcar-<channel>-<version>-hvm` (amd64) or `Flatcar-<channel>-<version>-arm64-hvm`; exactly one match |
| Architectures | amd64, arm64 |
| Later | nothing |

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
| Notes | Flatcar has no GCE arm64 image and no `oem-gce` sysext for arm64 (no guest agent, no OS Login); the build is unproven until the spike of [plan 16](../plans/v1/wip/16-gcp.md) |
| Later | when Flatcar ships a GCE arm64 image it appears in `kinvolk-public` and arm64 moves to rung 1 |

### hcloud

| | |
|---|---|
| Rung | 3 (Hetzner offers no Flatcar image, and its snapshots cannot be shared or made public) |
| Source | Flatcar's official `flatcar_production_hetzner_image.bin.bz2`, uploaded as a snapshot with `hcloud-upload-image` (Hetzner has no import API) |
| Scope | global in the project, bound to an architecture; a snapshot boots in any location |
| Created by | your IaC or a CI job next to it (some IaC tools have no upload resource, Pulumi among them); the L5 lane's bootstrap does it for the CI project |
| Pinned version | snapshot labels `os=flatcar`, `flatcar-channel=<channel>`, `flatcar-version=<version>` and the architecture; exactly one match |
| Architectures | amd64, arm64 |
| Later | an `ImageImporter` with the same upload library stays possible (rung 4) |

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
| Later | if Spare Cores' account may publish community images, the repository publishes them per region (rung 2) and `images` becomes optional; an upstream `oem-aliyun` replaces the source |

## Later moves

Nothing in the ladder is final; a vendor moves up when the world changes, and the
only change for users is that `images` becomes optional or its source changes:

- **Upstream Flatcar ships a platform** (GCE arm64, `upcloud`, `vultr`, `aliyun`):
  the images repository proposes them and stops building a platform from the
  release that carries it. Versions already built stay available for pins.
- **OVH community visibility** or **Alibaba community images**: rung 3 to rung 2.
- **The Vultr Marketplace**: rung 3 to rung 2, if Vultr allows a base image.
- **Rung 4** (`SCImage`, `ImageImporter`) only if a vendor appears for which IaC
  cannot create the image.
