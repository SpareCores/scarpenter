# Flatcar snapshots on Hetzner Cloud

Hetzner offers no Flatcar image, and a project cannot share or publish images, so the
`hcloud` driver boots **snapshots in your own project**: one per architecture and Flatcar
release, global in the project (not per location). SCarpenter finds them by labels and
refuses any image that does not say it is the pinned release. This guide makes them with
`hcloud-upload-image`, names the label convention and shows how the driver finds and checks
them. The driver is the [Hetzner Cloud driver](../reference/hcloud-driver.md); the image
rungs are ADR-021.

- [The label convention](#the-label-convention)
- [Make the snapshots](#make-the-snapshots)
- [Check them](#check-them)
- [Bring your own selector](#bring-your-own-selector)
- [Cost and limits](#cost-and-limits)
- [Troubleshooting](#troubleshooting)

## The label convention

The driver takes the snapshot of the pinned release for an architecture with this selector, and
it must return exactly one image:

```text
GET /images?type=snapshot&status=available&architecture=<x86|arm>
    &label_selector=os=flatcar,flatcar-channel=<channel>,flatcar-version=<version>
```

| Label | Value |
|---|---|
| `os` | `flatcar` |
| `flatcar-channel` | `stable`, `lts`, `beta` or `alpha` (the NodeClass's `flatcar.channel`) |
| `flatcar-version` | the pinned version, for example `4757.2.3` (the NodeClass's `flatcar.version`) |

The architecture is the snapshot's own (`x86` for the `amd64` Flatcar image, `arm` for
`arm64`): an x86 snapshot boots only x86 server types, an arm snapshot only `cax*`. Two snapshots
with the same labels and architecture are an error (`ImageAmbiguous`), never a guess; delete the
older one.

## Make the snapshots

Flatcar publishes `flatcar_production_hetzner_image.bin.bz2` (a raw disk, bzip2) for both
architectures and every channel. Hetzner has no upload or import API: the tool
[`hcloud-upload-image`](https://github.com/apricote/hcloud-upload-image) creates a temporary
server in rescue mode, writes the image, powers it off, takes the snapshot and deletes the server.
It is a community tool, not an official Hetzner product. For each architecture:

```sh
export HCLOUD_TOKEN=...   # a read-write token of the project
VERSION=4757.2.3          # the version your NodeClass pins
CHANNEL=stable
for arch in amd64 arm64; do
  case "$arch" in amd64) a=x86; dir=amd64-usr;; arm64) a=arm; dir=arm64-usr;; esac
  hcloud-upload-image upload \
    --image-url "https://${CHANNEL}.release.flatcar-linux.net/${dir}/${VERSION}/flatcar_production_hetzner_image.bin.bz2" \
    --architecture "$a" --compression bz2 \
    --labels "os=flatcar,flatcar-channel=${CHANNEL},flatcar-version=${VERSION}" \
    --description "Flatcar ${CHANNEL} ${VERSION} ${arch}"
done
```

(The flags are those of `hcloud-upload-image` v1; `hcloud-upload-image upload --help` lists the current ones. Nothing here has run against a project yet: the L5 lane does it first.)

It takes a few minutes per architecture (run them in parallel if you like) and bills one started
hour of a small server type. Flatcar's own page for Hetzner also describes Packer with
`flatcar-install -o hetzner`; the result is the same: a snapshot with the three labels. A
Flatcar image booted on Hetzner reads its Ignition from the server's user-data (the `hetzner`
platform), which is what SCarpenter sends; no kernel command line work is needed.

Do it from your IaC, or a CI job next to it, for the `(channel, version)` of every cluster, and keep the
snapshots: they are the node image. Some IaC tools have no resource that uploads an image; a command
step or a CI job is then needed. Looking them up again in your IaC is possible with the provider's image
data source. SCarpenter's own L5 lane creates them the same way when the pinned version has none.

## Check them

```sh
hcloud image list --type snapshot --selector os=flatcar \
  --output columns=id,architecture,labels,description
```

Exactly one line per architecture and pinned version. SCarpenter reports the result in the
NodeClass status: `status.vendors.hcloud.images` lists each location with each architecture, the image
ID found and, when it is not ready, why (`ImageNotFound`, `ImageAmbiguous`, `ImageVersionMismatch`,
`ImageVersionUnknown`); the condition `ImagesReady` is false while a named architecture has none.

## Bring your own selector

Without `spec.vendors.hcloud.resources.images` the convention above applies. To use other labels, a
fixed list or an ordered choice, say so with the one selector of every resource kind
([resource selectors](../reference/resource-selectors.md#hcloud)):

```yaml
spec:
  vendors:
    hcloud:
      resources:
        images:
          use:
            tags: {team: platform}            # labels on the snapshot
            match: {architecture: [amd64]}    # optional: the request's own architecture applies anyway
            select: newest
            orderBy: created
        # or by ID: snapshots are global, so under the key "*" (both architectures may be listed)
        # images: {use: {ids: {"*": ["123456789", "987654321"]}}}
```

Whatever selects the image, its `flatcar-channel` and `flatcar-version` labels must still say the pinned
release: an image without them, or with another version, is never booted.

## Cost and limits

A snapshot bills per GB-month of its compressed size (about half a gigabyte for Flatcar): cents a month.
The temporary upload server bills one started hour. A project may hold a limited number of snapshots
(Hetzner's documentation names 30 per account; check your project). A snapshot of one project cannot be used by
another account.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `ImagesReady` false, `ImageNotFound` | no snapshot has the three labels for the pinned release and architecture; run the upload for that version |
| `ImageAmbiguous` | two snapshots carry the same labels for one architecture; delete one |
| `ImageVersionMismatch` | the image your selector found says another release than the NodeClass pins |
| a launch fails `invalid_input` on `image` | the snapshot's architecture does not match the server type (an x86 snapshot on a `cax*` type) |
| the first server of a snapshot boots slowly | Hetzner loads the snapshot on first use: a minute or two |
