# Import Flatcar templates into UpCloud

UpCloud has no public Flatcar template and a template cannot be shared with another account, so the
nodes of the [UpCloud driver](../reference/upcloud-driver.md) boot from **templates in your own account**,
one per zone and storage tier, which you make from an image the Spare Cores images repository builds
([plan 22](../plans/v1/wip/22-flatcar-images.md), rung 3 of the [image policy](../explanation/flatcar-images.md#upcloud)).
SCarpenter never imports: it finds the templates by their labels and reports them in the NodeClass
status (`ImagesReady`). Nothing here has run against UpCloud yet (spikes S1 and S2 of
[plan 19](../plans/v1/wip/19-upcloud.md)); the operations are those of the UpCloud API and `upctl`, whose
exact flags you should check with `upctl help`.

- [What you need](#what-you-need)
- [Import one zone and tier](#import-one-zone-and-tier)
- [The labels](#the-labels)
- [Check what SCarpenter sees](#check-what-scarpenter-sees)
- [Upgrade the Flatcar version](#upgrade-the-flatcar-version)
- [Cost and limits](#cost-and-limits)

## What you need

- The release asset `flatcar-upcloud-<channel>-<version>-amd64.img.gz` of the images repository for the
  Flatcar version you pin (`spec.flatcar.version`), its SHA-256 and its signature. It is the generic Flatcar image with
  OEM id `upcloud`, raw and gzip, because UpCloud imports raw and raw gzip or xz, not qcow2 or bz2. The LTS channel is not built
  (its Ignition predates the `upcloud` platform).
- An account or sub-account that can create storages and templates in the zones you use. The token SCarpenter runs with must see
  the templates (a sub-account sees the private storages of its main account; **unverified** between sub-accounts).
- A place to read the image from: a direct upload (the importer's `PUT`, no hosting), or an object store URL served as
  `application/gzip` (UpCloud relies on the Content-Type of the host for `http_import`; a GitHub release asset is
  `application/octet-stream`, which may defeat the decompression, **unverified**).

## Import one zone and tier

Repeat for every zone you use and every tier you want (MaxIOPS for most plans; the Starter and Developer plans include Standard
storage, so a Standard template makes their included disk usable). A clone keeps the tier of its template.

1. **Create a storage** in the zone, size 10 GB (the image is about 9 GB raw, **unverified**; the least size of a storage is 10 GB),
   in the tier.
2. **Import the image into it**: a direct upload of the `.img.gz`, or an `http_import` of its URL, with the SHA-256 as the source
   hash. The import decompresses it.
3. **Make a template** of the storage (the API operation is "templatize"; the control panel calls it "make template"). A template is
   a storage billed per GB and hour at its tier.
4. **Label the template** (below), then delete the source storage if the template is a copy of it (it is billed until deleted).

Every template needs the labels; without `flatcar-version` the template says nothing about its release and is never booted
(`ImageVersionUnknown`).

## The labels

| Label | Value |
|---|---|
| `scarpenter-image` | `flatcar` |
| `flatcar-channel` | `stable`, `beta` or `alpha` |
| `flatcar-version` | the release, for example `4757.2.3` |
| `flatcar-arch` | `amd64` |
| `flatcar-tier` | `maxiops` or `standard` (informational: the tier of the storage is the one that counts) |
| `scarpenter-use` | `"true"`, optional: marks a resource you offer to SCarpenter; the sweeper of the L5 lane never touches it |

Use the title `flatcar-<channel>-<version>-amd64-<tier>`. Label keys are 2 to 32 characters. The API's templatize call takes
only a title, so labels are set by a second call: a template without them for a moment is not found yet.

Without `resources.images` the driver finds the template by exactly these labels, narrowed to the pinned channel and
version. To name the templates yourself, give them to the NodeClass; both tiers go in the same list, since one template per
tier is not ambiguous:

```yaml
spec:
  vendors:
    upcloud:
      resources:
        images:
          use:
            ids: {fi-hel1: [<MaxIOPS template UUID>, <Standard template UUID>]}
            # or: tags: {scarpenter-image: flatcar}, with orderBy: label:flatcar-version and select: newest
```

## Check what SCarpenter sees

The NodeClass reports the template of each zone:

```bash
kubectl get scnodeclass <name> -o jsonpath='{.status.vendors.upcloud.images}'
kubectl describe scnodeclass <name>     # the ImagesReady condition and its message
```

A zone without a template is `ready: false` with "import the Flatcar template for <version>"; a template that says another
release is `ImageVersionMismatch`. Its offerings are unavailable until the template exists. A root disk of a tier with no template is
`Unsupported` at launch, with the label to import.

## Upgrade the Flatcar version

Pinning a new `spec.flatcar.version` drifts the nodes. Import the templates of the new release first, in every zone and tier, then
change the pin; the old templates can stay until no NodeClass pins them. A zone is not ready for the new version until its template
is `online`.

## Cost and limits

A template is a storage billed per GB and hour at its tier (the price is **unverified**; about 10 GB in each zone and tier you
use), and a source storage is billed until you delete it. Importing into one zone and cloning to the others "takes considerably
longer", so import per zone. Storage limits of the account (`resource_limits`) count templates too.
