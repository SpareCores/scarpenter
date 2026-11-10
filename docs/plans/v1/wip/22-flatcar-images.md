# 22 — The Flatcar images repository

Depends on: nothing in SCarpenter's code. The one shared piece is the Spare Cores GCP
project, which sc-infra creates ([plan 14](14-multi-vendor-groundwork.md), B5). Needed
by: GCP arm64 ([16](16-gcp.md)), UpCloud ([19](19-upcloud.md)), Vultr
([20](20-vultr.md)) and Alibaba ([21](21-alicloud.md)): they come after it. Read
`docs/plans/v1/CONVENTIONS.md` first; its rules 1, 3 to 6 and 8 apply to the new
repository too (no commits by the executing model, no secrets, no live cloud calls
except where a step says the coordinator runs it). Spec: ADR-021,
`docs/explanation/flatcar-images.md`.

Written 2026-11-10. Flatcar facts (stable 4757.2.3, LTS 4081.3.12) are from the public
release server and Flatcar's sources, as the vendor plans cite them; anything not read
there is marked **unverified**.

## Goal

A new Spare Cores GitHub repository, **proposed name `SpareCores/flatcar-images`**
(the name is a proposal for the maintainer to confirm), that:

1. builds, per Flatcar release and channel, the OEM-patched images for **vultr**,
   **upcloud** and **aliyun**, in the format each vendor imports, and the **GCP arm64**
   image;
2. publishes the downloadable artifacts with sha256 sums and signatures;
3. publishes the GCP arm64 image publicly from a Spare Cores GCP project, and later
   Alibaba community images;
4. proposes the missing platforms upstream to Flatcar.

It has no cloud credentials except the keyless identity that publishes the GCP image.
It never boots an image on a vendor: that check is the vendor plans' L5 lanes.

## Scope: what it builds

| Platform | OEM id | Architectures | Artifact (per channel and version) | Used by |
|---|---|---|---|---|
| upcloud | `upcloud` | amd64 | `flatcar-upcloud-<channel>-<version>-amd64.img.gz` (raw, gzip: UpCloud imports raw, raw gzip and xz, not qcow2 or bz2) | plan 19 |
| vultr | `vultr` | amd64 | `flatcar-vultr-<channel>-<version>-amd64.img.gz`, and the raw image at a public URL if Vultr's import needs it (spike S1 of plan 20) | plan 20 |
| aliyun | `aliyun` | amd64, arm64 | `flatcar-aliyun-<channel>-<version>-<arch>.qcow2` and `.img.gz`; step 0 of plan 21 decides which one the import prefers (the compressed qcow2 is accepted by `ImportImage`; `.bz2` is not) | plan 21 |
| gce | `gce` (**unverified**, see GCP below) | arm64 | `flatcar-gce-<channel>-<version>-arm64.tar.gz` (one `disk.raw`) and the published GCP image | plan 16 |

Channels: stable and LTS are built for every release as it appears; beta and alpha only
on request (a workflow dispatch or a line in `config/channels.yaml`). Old versions are
built on request too (a NodeClass can pin any version). UpCloud's LTS is excluded while
the LTS release ships Ignition older than 2.23 (no `upcloud` platform; plan 19).

Out of scope: any platform Flatcar already ships (hcloud, OVH, Azure, AWS, GCP amd64
use the official files), booting on a vendor, importing into a customer account (that is
the user's IaC, rung 3 of ADR-021), and updating nodes (the base config masks
`update-engine`).

## Layout

```
flatcar-images/
  README.md                      what the artifacts are, how to verify and use them
  config/channels.yaml           channels, platforms, architectures, versions to build
  config/platforms.yaml          per platform: OEM id, formats, artifact names
  scripts/fetch.sh               download the generic image, verify digests and signature
  scripts/patch-oem.sh           write grub.cfg into the OEM partition
  scripts/package.sh             recompress per platform format
  scripts/publish-gcp.sh         upload, create the image, share it
  scripts/verify.sh              what a consumer runs: sha256 and cosign
  tests/                         the tests below
  .github/workflows/
    track-releases.yml           scheduled: find new Flatcar releases, dispatch builds
    build.yml                    one channel and version: build, sign, release
    publish-gcp.yml              the GCP arm64 image
```

Licence: MPL-2.0 for the scripts, like the sibling repositories. The images are
Flatcar's and stay under their licences.

## Build

Per (channel, version, platform, architecture):

1. **Fetch.** Download `flatcar_production_image.bin.bz2` of the architecture
   (`amd64-usr` or `arm64-usr`) from the release server, with its `.DIGESTS`; verify the
   SHA512 and the `.DIGESTS.asc` signature against Flatcar's image signing key (pinned
   by fingerprint in the repository). A file already cached by digest is not fetched
   again. The download is the only network access of the build besides the release
   feed.
2. **Patch the OEM partition.** Decompress to a raw disk, loop-mount partition 6 (label
   `OEM`) and write `grub.cfg` with `set oem_id="<id>"` (the console arguments too where
   a vendor needs them), unmount, and change nothing else. The chain is in Flatcar's
   sources: the main `grub.cfg` sources the OEM partition's `grub.cfg` and appends
   `flatcar.oem.id=$oem_id` to the kernel command line; bootengine's `ignition-setup-pre`
   turns it into the platform Ignition runs with; without an `oem-release` file no OEM
   sysext is looked for. The generic image sets no OEM id, so unpatched it runs Ignition
   as `qemu` (fw_cfg) and boots unconfigured; a stock image of another platform reads
   other metadata paths and does not work. The build checks afterwards that the partition
   table and every other partition are byte-identical to the source.
3. **Package** to the platform's format (table above) and compute the sha256 of each
   artifact.
4. **Sign.** `cosign sign-blob` keyless from the workflow (GitHub's OIDC identity), a
   bundle next to each artifact; `SHA256SUMS` is signed too. GitHub's build provenance
   attestation is added to the release.
5. **Release.** One GitHub release per (channel, version), tag `<channel>-<version>`,
   with every artifact, `SHA256SUMS` and the bundles. A release asset is limited to
   2 GiB, which the gzip and compressed qcow2 artifacts (about 0.6 GB) fit and a raw
   image of about 9 GiB does not: a platform that must be fetched as raw (possibly
   Vultr, spike S1 of plan 20) needs object storage, see the open questions.

Naming: artifacts `flatcar-<platform>-<channel>-<version>-<arch>.<ext>`. Images created
from them in a vendor account follow the per-vendor convention of
`docs/explanation/flatcar-images.md`, so a pinned version maps to one image:

| Vendor | Name or label of the image made from the artifact |
|---|---|
| upcloud | template labels `scarpenter-image=flatcar`, `flatcar-channel`, `flatcar-version`, `flatcar-arch`, `flatcar-tier`; title `flatcar-<channel>-<version>-amd64-<tier>` |
| vultr | snapshot description `flatcar-<channel>-<version>-<arch>` |
| alicloud | image name `Flatcar-<channel>-<version>-<arch>`, tags `flatcar-channel`, `flatcar-version`, `arch`, `built-by` |
| gcp (published) | image `flatcar-<channel>-<major>-<minor>-<patch>-arm64`, family `flatcar-<channel>-arm64`, labels `flatcar-channel`, `flatcar-version` (dots as dashes: the label charset), `built-by=spare-cores` |

## GCP arm64

Flatcar publishes no GCE arm64 image (the amd64 one lives in `kinvolk-public`;
Flatcar issue 2000 asks for arm64) and the `oem-gce` package is keyworded `amd64`
only. The build route is **unproven** and is the spike of plan 16:

1. Take the generic arm64 image, set the OEM id so that Ignition runs as the `gcp`
   platform (the `oem_id` of the GCE OEM; how Flatcar maps the OEM id to the Ignition
   platform name `gcp` was not found in the docs: **unverified**), convert to `disk.raw`
   in a `tar.gz`.
2. Upload it to a Cloud Storage bucket of the Spare Cores project and create the image
   (`images.insert` with `rawDisk.source`): `architecture: ARM64`, guest OS features
   `UEFI_COMPATIBLE`, `VIRTIO_SCSI_MULTIQUEUE` and `GVNIC` (newer series need it),
   storage in a multi-region. Google allows one image creation per 10 minutes from one
   source (a burst of six per hour): the workflow serialises.
3. Share it: `images.setIamPolicy` with `roles/compute.imageUser` to
   `allAuthenticatedUsers` (`allUsers` is refused; a whole project cannot be shared this
   way), so every authenticated account can launch from it by name.
4. The project is created by sc-infra and holds only the bucket and the published images
   (plan 14, B5). The workflow authenticates with **workload identity federation from
   GitHub Actions** (keyless: no service account key in the repository), with a role
   limited to that project's bucket and images.

Known gap: there is no `oem-gce` sysext for arm64, so no guest agent, no OS Login, and
none of `google-guest-configs`' udev rules, which create `/dev/disk/by-id/google-<name>`
(the data-volume paths of plan 16). `/usr` is read-only and verity-protected, so the
build cannot add the rule there; it can go on the OEM partition if Flatcar's initrd
honours it (**unverified**) or into SCarpenter's base Ignition (a vendor Butane
fragment, plan 14 A7). The spike decides; see the open questions.

Later, if Spare Cores becomes eligible, Alibaba community images: import once in a home
region, `CopyImage` to every region, `ModifyImageSharePermission(IsPublic=true)` per
region (not every region allows it), verify from a second account. Not in the first
milestone.

## Release tracking

`track-releases.yml` runs daily and on dispatch. It reads Flatcar's release feed (the
JSON release lists of the Flatcar website and the `version.txt` of each channel's release
server; the exact endpoints are confirmed in the first milestone), compares the releases
of the configured channels with the GitHub releases already present, and dispatches
`build.yml` for each missing (channel, version). A failed build opens an issue; nothing is
retried silently. A version is never rebuilt over an existing release: a fix is a new
release suffix (`<channel>-<version>-r2`), so an image ID someone created from the
old artifact still matches its checksum.

Retention: releases and artifacts are not deleted automatically (users pin old
versions, ADR-012). The published GCP images are kept the same way until a cost review.

## Tests

- **Unit (every pull request, no network):** `patch-oem.sh` on a small synthetic GPT
  disk with an `OEM`-labelled partition: `grub.cfg` is written with exactly the OEM id,
  the other partitions and the partition table are unchanged, a missing OEM partition
  fails; the packaging formats (gzip raw, qcow2) round-trip to the same bytes;
  `config/*.yaml` are consistent (every platform has artifact names, formats and an OEM
  id); the artifact and image names match the conventions above.
- **Real image (the build itself):** after patching, the workflow remounts read-only and
  checks `grub.cfg`, compares every other partition's checksum with the source, and
  verifies the signature bundle with `verify.sh` as a consumer would.
- **Boot smoke (optional, KVM runner, as overview Q6):** boot each patched image in QEMU
  and read the serial log: the kernel command line has `flatcar.oem.id=<id>`. This proves
  the patch, not the vendor's metadata service.
- **Boot on a vendor:** not here. Each vendor plan's L5 lane has an `image` scenario that
  creates the image from the release artifact (spikes S1 and S2 of plans 19 and 20, step
  0 of plan 21, the spike of plan 16) and boots a node to Ready. The repository's release
  notes link the vendor plans' results.

## Upstream proposals

Flatcar's own images are the better long-term answer; each proposal is an issue and a
small change in Flatcar's build scripts, with the evidence from the spikes:

- **GCE arm64** (the existing request, Flatcar issue 2000): an arm64 `oem-gce` and the
  image type, once the spike proves the build.
- **`upcloud`**, **`vultr`**, **`aliyun`** image types: one OEM ebuild and one image
  definition each, modelled on the existing `hetzner` and `akamai` ones. Ignition and
  Afterburn already support the platforms (Ignition 2.23 or later for UpCloud).

Once Flatcar ships a platform, the repository stops building it from the release that
carries it, its README points to the official file, and the vendor's source in the
hand-off docs switches (UpCloud, Vultr, Alibaba stay rung 3 with the official file as
source; GCP arm64 moves to rung 1 when it appears in `kinvolk-public`). Versions already
built stay available for pins.

## Milestones

1. Repository skeleton, `config`, `fetch.sh`, `patch-oem.sh`, unit tests, one platform
   (upcloud amd64) end to end for the current stable, signed and released; confirm the
   release feed endpoints.
2. vultr amd64 and aliyun amd64 and arm64; artifact formats confirmed with the spikes of
   plans 19, 20 and 21.
3. The GCP arm64 build and publication (needs the project from sc-infra and the spike of
   plan 16).
4. `track-releases.yml` on a schedule for stable and LTS; retention and failure handling.
5. The upstream issues and changes; later Alibaba community images.

## Dependencies

- sc-infra: the Spare Cores GCP project and the keyless identity (plan 14, B5); needed
  from milestone 3.
- Plans 16, 19, 20 and 21 supply the spike results that fix the formats and the GCE OEM
  id; the repository's first artifacts are what those spikes boot.
- Flatcar's signing key and release feed.

## Open questions for the maintainers

1. **Name and organization.** `SpareCores/flatcar-images` is a proposal.
2. **Hosting of raw images.** GitHub release assets carry up to 2 GiB per file, so only
   compressed artifacts. If Vultr's import needs a raw URL, or UpCloud's `http_import`
   mishandles the content type of release assets (plan 19 asks the same), the repository
   also publishes to a Spare Cores bucket with the right type, the latest three versions
   per channel; or users upload directly (`direct_upload` at UpCloud).
3. **Channels and retention.** Stable and LTS automatically, beta and alpha on request,
   nothing deleted. Revisit if the release count or the GCP image storage cost grows.
4. **Where the arm64 GCE udev rule lives** (OEM partition, or a vendor Butane fragment):
   decided by the spike of plan 16.
5. **Signing.** cosign keyless from GitHub Actions is the default; whether Spare Cores
   wants a long-lived key as well, and how consumers learn the identity to verify
   against (the README documents the workflow identity and the minimum cosign version).

## Gates

For the executing model, in the new repository: unit tests green with no network access;
`actionlint` and `shellcheck` clean; the build of milestone 1 run by the coordinator on
GitHub Actions with a signed release; `docs/plans/v1/README.md` status `review`. No cloud
call by the model; the GCP publication is the coordinator's.
