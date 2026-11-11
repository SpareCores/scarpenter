# Upload the Flatcar image to OVH

SCarpenter never creates boot images (rung 3 of the image ladder, [ADR-021](../explanation/DECISIONS.md)): you put Flatcar's
OpenStack image into the Glance of **each region** of your Public Cloud project, and the driver finds it there. OVH has no public
Flatcar image, and Glance images are regional: nothing is copied across regions. This guide does it with the OpenStack
CLI; any tool that creates the same Glance image does the same (see [any tool](#any-tool)).

Nothing here has run against OVH yet (plan 18, spikes S2 and S3): the procedure is the one Flatcar documents for
OVHcloud and OVH documents for uploading your own image, and the properties are the plan's.

## Table of Contents

- [The image](#the-image)
- [Upload it](#upload-it)
- [What the driver reads](#what-the-driver-reads)
- [Any tool](#any-tool)
- [A new release](#a-new-release)

## The image

Flatcar publishes `flatcar_production_openstack_image.img` (qcow2, **not** the `.gz` or `.bz2`: Glance stores the bytes as given, so
a compressed file would be an unbootable image) with its `.DIGESTS`, per channel and architecture, under
`https://<channel>.release.flatcar-linux.net/<arch>-usr/<version>/`. Verify the file against the digests once; v1 launches amd64 only (Keeper has no arm64
OVH type), so use `amd64-usr`. The image's virtual size is about 12.3 GiB and every flavor of the catalog has a disk of at least 25 GB.

```sh
version=4757.2.3; channel=stable
base=https://$channel.release.flatcar-linux.net/amd64-usr/$version
curl -fLO $base/flatcar_production_openstack_image.img
curl -fLO $base/flatcar_production_openstack_image.img.DIGESTS
grep -A1 -i sha512 flatcar_production_openstack_image.img.DIGESTS | grep openstack_image.img$ | sha512sum -c
```

## Upload it

With the `OS_*` variables of [your OpenStack user](ovh-credentials.md) (a user with the Image Operator role can create images), once per
region name of the catalog:

```sh
for region in GRA11 EU-WEST-PAR; do
  OS_REGION_NAME=$region openstack image create "flatcar-$channel-$version-amd64" \
    --file flatcar_production_openstack_image.img \
    --disk-format qcow2 --container-format bare --private \
    --property architecture=x86_64 \
    --property flatcar_channel=$channel --property flatcar_version=$version \
    --property image_original_user=core --property hw_qemu_guest_agent=yes \
    --tag flatcar --tag "flatcar-$channel-$version"
done
```

The file is about 0.6 GB: a direct upload takes minutes. Images are billed as snapshots at a flat per-GB-month rate (cents per month
for one image per region). `web-download` (an import from the release URL) is unverified on OVH (spike S3); the upload above is
the documented method. Boot parameters on the image (`hw_disk_bus`, `hw_firmware_type`) are for the spikes S2 and S8 to settle.

## What the driver reads

Without an `images` block, the driver looks in the project for a **private**, **active** image named
`flatcar-<channel>-<version>-<arch>` with the property `architecture` (`x86_64`): exactly one must match. Whatever finds
the image, the pinned release must be readable from its name, its properties `flatcar_channel` and `flatcar_version`, or its tag
`flatcar-<channel>-<version>`, and the sources must not contradict each other, or the region's image is not ready
(`ImageVersionMismatch`, `ImageVersionUnknown`) and never booted. So the name convention above, or the properties, or the tag,
is what makes the driver trust an image: keep at least one.

To name the images yourself, hand the Glance UUIDs per region (not an OVH-side ID):

```yaml
vendors:
  ovh:
    regions: [GRA11, EU-WEST-PAR]
    resources:
      images:
        use:
          ids:
            GRA11: ["9c1f4b8e-2a7d-4c63-9a55-0d1e6f3b7a21"]
            EU-WEST-PAR: ["4be2a1c0-7f3d-4e9b-8a16-5c2d9e0f1b34"]
```

or a pattern (`name: "flatcar-{channel}-{version}-{arch}"`), `tags`, `match.owner`, a `visibility` filter: the
[selector reference](../reference/resource-selectors.md#ovh) has them all, and `imageOwner` names the project that owns the images when
it is not the credentials' own (a shared image must be accepted in the region by the member first).

## Any tool

Whatever creates the images (a CI job, an OpenStack provider of your infrastructure tool, the Control Panel) must produce the same
thing per region: a Glance image in the project with the file above, qcow2, `architecture=x86_64`, and a name, properties or tag that
say the release. Hand over the Glance UUIDs the tool reports, or rely on the name. SCarpenter does not read your tool's state.

## A new release

An image is one release: for a new pin, upload `flatcar-<channel>-<new version>-amd64` next to the old one, then change the NodeClass's
`flatcar.version`; nodes of the old release drift and are replaced. Delete the old image when no node and no NodeClass uses it (an
instance of a deleted image keeps running, but cannot be rebuilt).
