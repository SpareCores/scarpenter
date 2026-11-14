# Flatcar images on OCI

OCI has no public Flatcar image and its custom images cannot be shared with other tenancies, so the
image is created in your own tenancy (rung 3 of [ADR-021](../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10)),
per region and architecture, from Flatcar's official `oraclecloud` image, by your own tooling or a CI job.
SCarpenter finds the image by the naming and tag contract below, or by the
[`images` selector](../reference/resource-selectors.md#oci). It never creates or changes an image. The
reference is the [OCI driver](../reference/oci-driver.md#images) and
[Flatcar images](../explanation/flatcar-images.md).

## What to create, per region and architecture

1. Download the release's `flatcar_production_oraclecloud_image.img` (qcow2, about 600 MB) for the
   architecture from Flatcar's release server (stable releases from 4757.2.3 on; older ones have none) and
   verify the signature and the digest against Flatcar's published key. Upload it to an Object Storage
   bucket. One bucket in one region and a read-only pre-authenticated request import into the other
   regions (a cross-region import took about 6.5 minutes).
2. Import it in each region as a custom image in the controller's compartment (`--oci-compartment`):
   source type `QCOW2`, launch mode `PARAVIRTUALIZED`, operating system `Flatcar`, operating system version
   the Flatcar version (`4757.2.3`), display name **`flatcar-<channel>-<version>-<arch>`**
   (`flatcar-stable-4757.2.3-amd64`) and the freeform tags **`flatcar-channel`**, **`flatcar-version`**,
   **`arch`** (`amd64` or `arm64`) and `os=flatcar`.
3. Add the shapes the image may launch to its shape compatibility list: every shape SCarpenter may launch
   (`VM.Standard.E5.Flex` and so on).
4. **arm64 only**: give the image an image capability schema (firmware `UEFI_64`, paravirtualized launch
   mode, network attachment, boot volume and remote data volume types) and the compatibility entries of the
   Arm shapes (`VM.Standard.A1.Flex` works; `A2` and `A4` are untested). SCarpenter launches arm64 nodes
   with the matching launch options.
5. Keep the version a NodeClass pins (`spec.flatcar.version`); an old image can stay until its nodes are
   gone. Custom image storage is billed like Object Storage Standard on the image's billable size (about 1 GB
   for the amd64 image). The limit `custom-image-count` is 100 per region (25 on pay-as-you-go and trial).

## How SCarpenter reads it

Without `resources.images`, SCarpenter lists the compartment's available images by the display name of the
pinned release and architecture and the operating system `Flatcar`. The version comes from the tag
`flatcar-version` (else the operating system version), the architecture from the tag `arch` (else the
compatible shapes); an image of another version, or that says none, is not ready. Check the NodeClass
condition `ImagesReady` and `status.vendors.oci.images`.

To use images you named yourself, give their OCIDs per region in the NodeClass (the images may be in
another compartment the controller can read; `*` is not valid because images are regional):

```yaml
vendors:
  oci:
    resources:
      images:
        use:
          ids:
            eu-frankfurt-1: [ocid1.image.oc1.eu-frankfurt-1.aaaaexample]
```
