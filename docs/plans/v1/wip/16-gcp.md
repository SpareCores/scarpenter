# 16 — GCP driver (Google Compute Engine)

Depends on: 14 (the shared groundwork: the `images` kind, the idempotency label, the
stopped-node rule, the Butane hook), 10 (the AWS driver, the L5 lane framework and the
canary), and, for arm64 only, 22 (the images repository that builds and publishes the
arm64 image); no image-import plan is needed. Read `docs/plans/v1/CONVENTIONS.md` first. Spec: overview §5.8, §5.9,
§7, §9, §11 (as the model), 00a §6, §9, §11; ADR-002, ADR-009, ADR-013, ADR-017,
ADR-018, ADR-020, ADR-021; the AWS plans 08 and 10 as the pattern.

Research date: 2026-11-10, from public documentation and public artifacts only. No
cloud API was called. Everything marked **unverified** must be checked by the spike
(section "Spike") or by the first live run, as plan 10 did for AWS.

## Goal

`pkg/vendors/gcp`: a production driver with the in-memory fake, the conformance suite,
reference docs and an IAM role, and a real-cloud lane (`test/e2e-gcp`, sweeper, canary)
that is added to `test/e2e-vendors.txt` only when the cost guards are true. This plan
writes and statically validates everything; the coordinator runs the first live run.

## 1. Images: the rung

**Rung 1 for amd64** (public images the vendor's ecosystem already offers), **rung 2
for arm64** (Spare Cores publishes them, until Flatcar does). Confidence: high for amd64,
high that arm64 has no official image, medium on the arm64 build route.

Facts, with sources:

- Flatcar publishes GCE images in the public project `kinvolk-public`, one family per
  channel: `flatcar-stable`, `flatcar-beta`, `flatcar-alpha`, `flatcar-lts`
  (<https://www.flatcar.org/docs/latest/deploy/cloud/gcp/>, the official Flatcar page for
  this vendor; the old URL `/docs/latest/installing/cloud/gcp/` redirects to it). Example
  command on that page: `--image-project kinvolk-public --image-family flatcar-stable`
  with `--metadata-from-file user-data=config.ign`. The page's current versions equal the
  release server's (stable 4757.2.3, lts 4081.3.12 on the research date).
- **A version pins to an image by name**: `projects/kinvolk-public/global/images/flatcar-<channel>-<major>-<minor>-<patch>`
  with dashes for dots, e.g. `flatcar-stable-3510-2-8`
  (<https://github.com/flatcar/Flatcar/issues/1205>, a Flatcar maintainer's answer; older
  versions stay available). The name pattern for `lts`, `beta` and `alpha` is the same
  shape: **unverified** (only a stable example was seen).
- **The images cannot be listed**: `images.list` on `kinvolk-public` fails with a missing
  `compute.images.list` permission; `images.get` by name and `images.getFromFamily` work
  (same issue, 2023; Google documents that publicly shared images do not appear in lists,
  <https://docs.cloud.google.com/compute/docs/images/managing-access-custom-images>). So
  `ResolveImage` is one `images.get` per (channel, version), never a filter. This is
  stricter than AWS's name pattern: no ambiguity is possible, a missing image is
  `NotFound` (`ImageNotFound`).
- **Images are global, not regional.** One image serves every region; `ReportImages`
  returns the same reference for each region in scope. (Storage location is a property of
  the image, invisible to launches.)
- **Architectures: amd64 only.** The release server has `flatcar_production_gce.tar.gz`
  for `amd64-usr` in all four channels and **nothing for `arm64-usr`**
  (<https://stable.release.flatcar-linux.net/arm64-usr/current/>, checked for stable,
  lts, beta, alpha; the arm64 listing has generic, openstack, qemu, azure, hetzner, akamai
  and other images but no GCE one). The `oem-gce` package is keyworded `amd64` only in
  Flatcar's scripts (`coreos-base/oem-gce`). The Flatcar GCP page says nothing about arm64.
  The request is open as <https://github.com/flatcar/Flatcar/issues/2000> ("Provide ARM
  releases for GCP", 2026-01; a maintainer agreed and doubted that the current GCE OEM tools
  build for arm64; no timeline). The committed Keeper snapshot has 75 arm64 GCP types
  (`t2a`, `c4a`, `n4a`, and the GPU family `a4x`, which is dropped anyway), so this is a
  real gap.
- **Channels**: stable, beta, alpha and lts all have the amd64 image
  (`flatcar_production_gce.tar.gz`: a tar.gz of one `disk.raw`, 638 MB compressed, about
  12.3 GiB raw for stable 4757.2.3).
- **Ignition supports GCP**: platform id `gcp`, "reads its configuration from the instance
  metadata entry named user-data"; SSH keys are handled separately by Afterburn and the
  guest agent (<https://github.com/coreos/ignition/blob/main/docs/supported-platforms.md>).
  The Flatcar images set the platform themselves; nothing to configure for amd64.
- **User-data size**: a metadata value is limited to 256 KB, all metadata to 512 KB
  (<https://docs.cloud.google.com/compute/docs/metadata/setting-custom-metadata>). The
  16 KiB budget of overview §8.2 is far below it; GCP is not the limiting vendor.

Why the higher rung does not exist for arm64: there is no official GCE arm64 image
(above). Rung 2 works because GCP lets anyone share an image with `allAuthenticatedUsers`
(role `roles/compute.imageUser` on the image; `allUsers` is refused; a whole project
cannot be shared that way): Spare Cores publishes the arm64 images in a Spare Cores
project, built and released by the images repository (plan 22; ADR-021 covers the
policy). **Decided: arm64 is in v1.** Rung 3 (pre-create in the user's project) stays possible through the
`imageProject` override.

**arm64 route (unverified, needs a spike; built by plan 22)**: take the generic arm64 image
(`flatcar_production_image.bin.bz2`), set the OEM to GCE so that Ignition runs as
platform `gcp` (the OEM partition's `grub.cfg` with `oem_id` or the kernel argument
`flatcar.oem.id=gce`; how Flatcar maps that to `ignition.platform.id=gcp` was not found
in the docs), convert it to `disk.raw` in a `tar.gz`, upload to Cloud Storage, create the
image (`architecture: ARM64`, guest OS features, see below). The `oem-gce` sysext
(guest agent, OS Login, `google-guest-configs` udev rules that create
`/dev/disk/by-id/google-<name>`) does not exist for arm64: the data-volume device paths
(section 5) and the account manager are therefore unavailable until someone builds it. The
cheapest working design is to ship the udev rule in the image and ignore OS Login.

**How an image gets into a project** (for rung 2, and for rung 3 by the user):

- `images.insert` with `rawDisk.source` = a `tar.gz` containing a file named `disk.raw`
  in a Cloud Storage bucket (the file name `disk.raw` is what Flatcar's own tarball uses;
  Google's page lists "a compressed RAW image in Cloud Storage" as a source). It cannot
  pull from an arbitrary URL: a bucket is needed, and the Flatcar uploader image
  `ghcr.io/flatcar/google-cloud-flatcar-image-upload` does exactly this (download release
  file, `gsutil` copy, create image; flags `--bucket-name`, `--project-id`, `--channel`,
  `--version`, `--image-name`, `--image-url`).
- The image is global; `storageLocations` can be one region or a multi-region.
- Limits and cost: Google documents one image creation per 10 minutes (burst of six per
  60 minutes) for images from one source; time is minutes; storage is a few cents per
  month for a compressed image (**unverified**: price not read; Google bills images on
  their compressed size). Bucket storage is extra, and deletable after creation.
- Sharing: `images.setIamPolicy` with `roles/compute.imageUser` to `allAuthenticatedUsers`.
  An organization policy of the **consumer** (`constraints/compute.trustedImageProjects`,
  from general knowledge, not read in the docs) can forbid images from other projects,
  including `kinvolk-public`: the launch then fails with a `CONDITION_NOT_MET` style
  error; the driver maps it to `Fatal` with a message naming the constraint
  (**unverified** code).
- **guest OS features** of the images (the biggest image risk): the C3, C4, C4A, N4, A3
  and later series need `GVNIC` on the boot image (some newer series `IDPF`, unverified);
  NVMe-attached disks need NVMe drivers in the guest, which is not a guest OS feature (the
  `computepb` enum has none); arm64 images need `architecture: ARM64`; `UEFI_COMPATIBLE`
  selects UEFI. Flatcar's image tool (`flatcar/mantle`, `platform/api/gcloud/image.go`)
  creates images with `VIRTIO_SCSI_MULTIQUEUE`, `UEFI_COMPATIBLE` and `GVNIC`; that the
  `kinvolk-public` images are made by it is **unverified**: read `guestOsFeatures` with
  `images.get` in the spike. Without `GVNIC` the driver cannot launch those series from
  them, and the options are: `Supported` removes them (ADR-017), or Spare Cores publishes
  its own amd64 image too (rung 2 for amd64).

## 2. The `spec.vendors.gcp` block and the hand-off

GCP's units: **region** (Keeper `api_reference`: `europe-west3`) and **zone**
(`europe-west3-a`, zonal instances and disks). Keeper's `zone_id` for GCP is Google's own
numeric zone ID (`2000` for `us-central1-a`) and its zone `api_reference` and `name`
are the zone names, which are global: `Zone{ID: <numeric id>, Name: <zone name>}` from
`zones.list`, no per-account mapping (unlike AWS). The numeric IDs are read from the API
(**unverified** that Keeper's `zone_id` is exactly `Zone.id`; the sample row
`us-central1-a` = `2000` matches Google's published numbering).

```yaml
vendors:
  gcp:
    regions: [europe-west3, europe-west4, us-east1]   # allow-list; empty = every region the credentials can read
    project: my-project                               # see "Account scope"
    imageProject: ""                                  # "" = kinvolk-public (amd64) and the Spare Cores project (arm64, a driver constant once the project exists); or a user's own
    storage: {root: {diskType: pd-balanced, sizeGb: 64}}
    networking: {}                                    # per-vendor override of spec.networking
    serviceAccount: ""                                # optional e-mail attached to nodes; none by default
    metadata: {}                                      # extra instance metadata (never user-data, ssh-keys, enable-oslogin, block-project-ssh-keys)
    resources:
      subnets:   {use: {ids: {europe-west3: [projects/my-project/regions/europe-west3/subnetworks/my-env-europe-west3]}}}
      firewalls: {use: {ids: {europe-west3: [nodes]}}}
      images:    {use: {ids: {"*": [projects/my-project/global/images/flatcar-stable-4757-2-3]}}}   # optional (plan 14, A1, A10): global, so the IDs under the key "*" (every region); default is the lookup of section 1
      resourcePolicies: {create: {lifecycle: {policy: perUse}}}
```

### Resource kinds

| Kind | Scope | Default lifecycle | `use` forms | `create` | What your IaC must output |
|---|---|---|---|---|---|
| `subnets` (the network is implied by the subnet) | region | `idleTTL` | `ids` (`projects/<p>/regions/<r>/subnetworks/<n>`, also the short name), `name` (glob, `{region}`), `match` (`network`, `cidr`, `within`, `state`, `owner` = the Shared VPC host project) and `filters` (`purpose`, `role`, `stackType`, ...), `expression` (a GCP filter); **no `tags`** (subnetworks have no labels, rejected) | yes: custom-mode VPC `scarpenter-<cluster>-subnets` (global) plus one subnet per region with the next free `/20` of the pool (default `10.240.0.0/12`, `create.pool` overrides it; found by reading the VPC's subnets, as AWS finds the first free `/20`), dual stack with external IPv6 and private Google access | the subnet resource ID per region (the network is implied) |
| `firewalls` | region (the rules are global; one rule set per region, named with the region, to fit the engine's per-region key) | `idleTTL` | `ids` = **network tags** (for example the tag `nodes` on your firewall rules), `name` (glob on the tags found in the rules' `targetTags`), `match.network`, `filters` on the rules (`direction`, `priority`, `disabled`); no `tags` (firewall rules have no labels, rejected) | yes: rules for one network tag `scarpenter-<cluster>-<nodeclass>-<region>` (hashed to 63 characters): inbound CIDR rules of `spec.networking.inbound`, for `isolation: nodeclass` allow-all from the same tag | the network tag per region |
| `images` (not managed) | global, region key | not applicable | `ids` under `*` or per region (`projects/<p>/global/images/<name>`), `name` (exact: `images.get`; a glob lists the project, which does not show publicly shared images), `tags` (labels), `match` (`architecture`, `owner` = the project listed, `state`, `includeDeprecated`, `createdAfter`), `filters` (`family`, `deprecated.state`, ...), `expression`; `newest` by `orderBy=creationTimestamp desc`. A publicly shared image (`kinvolk-public`, the Spare Cores project) is reachable only by `ids`, exact `name` or `family` | never (ADR-021); without `images`: `kinvolk-public` by name (amd64), the Spare Cores project (arm64) | nothing (rungs 1 and 2), or the image IDs |
| `runFirewalls` (internal, like AWS `runSecurityGroups`) | run | `perUse` | not configurable | one rule per run key (source tag = target tag = `scarpenter-run-<hash>`), the tag goes on the instance | nothing |
| `resourcePolicies` (placement) | run | `perUse` | `ids`/`name` through the placement profile (`use: {name}`), regional | yes: per (region, profile, run key) | nothing (SCarpenter-owned) |

Without `use` (ADR-022): no discovery (no labels on these kinds), then the vendor default.
`Resolve` with an empty reference: the project's `default` network and its subnetwork
named `default` in the region (an auto-mode network has one per region) and, for
firewalls, nothing (the `default` network's rules target no tag: a launch into it works
with the implied rules and the user's own, so an empty reference returns a synthetic
"no tag" firewall that adds no network tag, with a warning; unverified whether this
default is worth supporting: **open question 4**). Where `default` is removed by
organization policy (`constraints/compute.skipDefaultNetworkCreation`, common), the kind
falls to `create`.

Ownership and marker. **Networks, subnetworks, firewall rules and resource policies have
no labels** (Google's label page lists only
instances, images, disks and snapshots, buckets, some forwarding rules, VPN tunnels and
static addresses): the marker is the name prefix `scarpenter-<cluster>-` **plus a
`description`** holding `scarpenter: cluster=<c> nodeclass=<n> managed-by=scarpenter
resource-kind=<kind>` (ADR-018's fallback). Both are required before the garbage collector
touches a resource; `List` filters by name prefix server-side and checks the description.
Instances and disks carry real labels (section 3).

### What the operator's IaC must provide, and what SCarpenter self-manages

The operator's IaC (any tool, or the console) provides, all optional (the driver works
with the `default` network or its own):

1. **A subnet per region and the firewall tag** (`subnets.use.ids`, `firewalls.use.ids`; no
   `tags`, no discovery: networks have no labels). The tag's rules should allow self, UDP
   41641 and ICMP. The `project` goes into the block and must equal the controller's.
2. **The controller's identity**: the workload identity pool, provider and the service
   account or principal with the custom role (section 8), documented as a requirement; the
   driver creates none of it.
3. **Egress for nodes without a public IPv4**: a Cloud NAT on the subnet, or
   `publicIPv4: false` fails for lack of egress (as AWS needs a NAT). The reference
   documents it.
4. **For arm64 (rung 2)** nothing: Spare Cores publishes the images from its own project
   (plan 22, which owns that project and the keyless identity of the repository's
   workflow); the image name pattern is plan 22's
   (`flatcar-<channel>-<major>-<minor>-<patch>-arm64`). A user's own images go through
   `images.use`.

Not needed: an SSH key (nodes get Ignition), a zone list (the driver reads zones). IPv6
needs a dual-stack subnet with an external `/64`.

SCarpenter self-manages, without `use`: the `default` network where it exists and suits the
request; else a custom-mode VPC, the subnets and the firewall rules (above); resource
policies and run firewalls always.

### Account scope and credentials

- **Project.** Every call names a project. The controller targets **one project in v1**:
  `--gcp-project` / `SCARPENTER_GCP_PROJECT` (Helm `vendors.gcp.project`); without it the
  project of the credentials (`google.FindDefaultCredentials().ProjectID`, which exists for
  a service-account key and on GCE, not for workload identity federation: then the flag is
  required). The NodeClass field `project`, when set, must equal it (the vendor condition
  `VendorReady` says otherwise), so a rendered manifest is self-describing and a
  wrong-project mismatch is a visible condition, not a launch error. (**Open question 2.**)
- **Authentication**: Application Default Credentials through the Google client libraries.
  Recommended: **workload identity federation** with an `external_account` credential
  configuration file (`GOOGLE_APPLICATION_CREDENTIALS`): the controller's projected
  ServiceAccount token as `credential_source.file`, audience
  `//iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>`,
  subject token type `urn:ietf:params:oauth:token-type:jwt`, and either service account
  impersonation or direct resource access with the federated `principal://` identity
  (whether Compute accepts the direct form for every method used: **unverified**; the
  impersonation form is the safe default). The cluster's OIDC issuer must be reachable by
  Google or its JWKS uploaded to the provider. This is the counterpart of AWS web identity
  and of `docs/how-to-guides/aws-credentials.md`; a `gcp-credentials.md` how-to is part of
  the plan. Also supported by the chart: a service-account key Secret (`mode: secret`) and
  node credentials from the metadata server when the controller runs on GCE or GKE
  (`mode: none`).
- **Nodes carry no service account** by default (`serviceAccounts` omitted: no credentials
  on the node, as on AWS without an instance profile). `serviceAccount` attaches one
  (needs `iam.serviceAccounts.actAs`).
- The CI identity (section 9) is a federated GitHub identity of the same kind.

## 3. Driver

**SDK**: `cloud.google.com/go/compute/apiv1` and `.../apiv1/computepb` (v1.75.0, 2026-11-05;
generated, maintained by Google, REST transport, stable v1 API) with `google.golang.org/api/option`
for credentials, and `cloud.google.com/go/cloudquotas` only if the spike shows `regions.get`
lacks a quota the driver needs (v1.14.1). The generated clients are concrete structs, so a
narrow `computeAPI` interface (methods with the `computepb` requests) wraps them, as `ec2API`
does: `Instances.{Insert,Delete,Get,List,AggregatedList}`, `Zones.List`, `Regions.{Get,List}`,
`MachineTypes.List`, `Images.{Get,GetFromFamily}`, `Subnetworks.{Get,List,Insert,Delete}`,
`Networks.{Get,List,Insert,Delete}`, `Firewalls.{List,Insert,Delete}`,
`ResourcePolicies.{Get,List,Insert,Delete}`, the zone, region and global operations
(`Wait`/`Get`), `Projects.Get`. Per-region clients are not needed (one endpoint); a per-call
deadline as on AWS (45 s, 120 s for the insert's operation wait; the operation `wait` method
long-polls up to two minutes server side). User agent `scarpenter/<version>`. Retries: the
REST client retries only 503 and 504 on reads, never insert or delete, so the
driver's own classification (429, 5xx as `Retryable`) and backoff do the rest.

**Launch** (`CreateInstance`): `instances.insert` in the zone with:

| Field | Value |
|---|---|
| `name` | the NodeClaim name when it is RFC 1035 (lower case, letters, digits, dashes, starts with a letter, at most 63), else `scarpenter-<hash>` |
| `machineType` | `zones/<zone>/machineTypes/<api_reference>` |
| `disks[0]` | boot, `autoDelete: true`, `initializeParams` {`sourceImage` (the resolved image), `diskType` `zones/<zone>/diskTypes/<type>`, `diskSizeGb`, `provisionedIops`/`provisionedThroughput` for Hyperdisk, `diskName` = instance name, `labels`}, `kmsKeyName` when set |
| `disks[1..]` | the data volumes: `autoDelete: true` (**not the default**), `deviceName` = the profile's volume name (we choose it, so the guest path `/dev/disk/by-id/google-<name>` is known before the call), `diskName` `<instance>-<name>`, labels, the interface (`NVME` or `SCSI`) the series needs |
| `networkInterfaces[0]` | `subnetwork`, `stackType` (`IPV4_ONLY`, or `IPV4_IPV6` with `ipv6AccessConfigs` EXTERNAL for `publicIPv6`), `accessConfigs` one `ONE_TO_ONE_NAT` unless `publicIPv4: false`, `nicType` and `networkPerformanceConfig` from a network profile |
| `tags.items` | the firewall's network tag, the run tag when there is one |
| `labels` | the logical labels, the NodeClass tags, and the idempotency label `scarpenter-idempotency-key` (plan 14, A3; section below) |
| `scheduling` | on-demand: `provisioningModel: STANDARD`, `automaticRestart: false`, `onHostMaintenance` `MIGRATE` or `TERMINATE` by series; spot: `provisioningModel: SPOT`, **`instanceTerminationAction: DELETE`** (the default is `STOP`, which leaves a stopped instance and its disks behind), `automaticRestart: false`, `onHostMaintenance: TERMINATE` |
| `metadata.items` | `user-data` = the rendered Ignition; `block-project-ssh-keys: TRUE`; `enable-oslogin: FALSE` (see below); the block's `metadata` |
| `serviceAccounts` | omitted unless `serviceAccount` is set |
| `resourcePolicies` | the placement policy's URL, from a placement profile |
| `shieldedInstanceConfig` | not set (the Flatcar image's defaults) |

- **Spot VMs only** (`provisioningModel: SPOT`); not the legacy preemptible model and not
  Flex-start. The Spot offering must exist for the machine type in Keeper's prices
  (Keeper has spot prices for GCP); whether some series refuse Spot is learned from the
  error (`Unsupported`).
- **Project-wide SSH keys and OS Login.** the project may be shared with other
  workloads that use project-wide `ssh-keys` metadata (sc-runner). Flatcar's GCE image
  runs the guest account manager, so project keys would create logins on every node; OS
  Login support is built in (Flatcar docs: OS Login is used when enabled for the instance,
  else the account manager; the unit `oem-gce-enable-oslogin.service`), so a project with
  `enable-oslogin=TRUE` would grant logins through IAM. Nodes are reached by Tailscale and Ignition-installed
  keys only: the driver sets the instance metadata `block-project-ssh-keys=TRUE` and
  `enable-oslogin=FALSE` (instance metadata overrides the project's). That the Flatcar
  guest agent honours both: **unverified**, check in the spike.
- **No IMDSv2 equivalent.** AWS's hop limit 1 keeps Pods away from user-data (overview
  §8.6). The GCE metadata server is reachable from any Pod and has no hop limit; it serves
  `user-data` (bootstrap token for at most an hour, deleted at registration; the Tailscale
  key, which is ephemeral and tag-scoped but reusable in v1). Without a service account no
  credentials are exposed. The reference states this; a node-side rule blocking
  `169.254.169.254` for Pod traffic is the vendor Butane fragment `block-metadata-from-pods`
  of the hook in plan 14 (A7): off by default, enabled per NodeClass, documented in the
  reference. (**Open question 6, decided.**)
- **Names.** Instance names are unique per zone, among live instances; a deleted
  instance's name can be reused. Names are the NodeClaim names (as hostnames the DHCP
  name is the instance name; Ignition also writes `/etc/hostname`).
- **Labels** (instances, disks): lower-case letters, digits, `_` and `-`, keys start with
  a letter, 1 to 63 characters, values up to 63, at most 64 labels
  (<https://docs.cloud.google.com/compute/docs/labeling-resources>). `TagKey(k)` =
  `scarpenter-<k>` (the contract's restrictive-charset mapping): `scarpenter-cluster`,
  `scarpenter-nodeclaim`, `scarpenter-nodepool`, `scarpenter-nodeclass`,
  `scarpenter-managed-by`, `scarpenter-resource-kind`. Values go through one
  deterministic `labelValue()` (valid values unchanged; otherwise lower-cased with other
  characters replaced and, when that loses information or exceeds 63, a short hash), used
  for writing and for filtering, so FindInstance and ListInstances match. `spec.tags`
  keys and values are validated against the charset (no `scarpenter-` prefix, at most 40).
- **Provider ID** `scarpenter://gcp/<region>/<nodeclaim>`. **InstanceRef**
  `<region>/<zone>/<instance name>` (the region part is the Keeper region; the ID part
  contains the zone because every instance call needs it; `InstanceRef.ID()` cuts at the
  first `/`, so an ID with a slash is fine). A malformed ID (the conformance suite's
  `<region>/i-never-existed`) is `NotFound`, not `Fatal`.
- **Idempotency.** GCP has two mechanisms; neither alone gives the contract. (1) A
  request ID (`requestId`, a UUID; Google says a retry with the same ID of a request
  already received is ignored, and does not state how long IDs are kept) protects against a
  lost response, per method call. (2) The unique name per zone makes a second insert in the
  same zone fail with `alreadyExists` (HTTP 409). The contract also wants a replay in
  **another zone, type or capacity type** to return the original and a failed create to
  leave the key unused, which a request ID cannot promise (**unverified**: whether a replay
  of a request ID whose operation failed returns the failed operation). The driver
  therefore: (a) before inserting, runs one project-wide filtered
  `instances.aggregatedList` (`labels.scarpenter-idempotency-key=<uid>`, server-side filter; cached for
  the attempt) and returns the instance if it exists; (b) inserts with `requestId` =
  UUIDv5 of (NodeClaim UID, zone, type, capacity type), so only the same offering shares an
  ID; (c) on `alreadyExists` does the lookup of (a) in that zone and returns the instance
  when the UID label matches, else `Fatal`. `bulkInsert` (zonal or regional,
  `namePattern`, location policy) is **not used**: it creates several instances from one
  template and has no per-instance idempotency; the core's attempt loop is per offering.
- **Asynchronous errors.** `instances.insert` returns an operation; capacity and quota
  errors appear in the finished operation's `error.errors[]` (`code`, `message`), not in
  the HTTP response. The driver waits for the operation (`Wait` on the zone operation)
  and classifies the codes, then reads the instance.
- **Instance states**: `PROVISIONING`, `STAGING` -> `pending`; `RUNNING` -> `running`;
  `STOPPING` -> `terminating` (also what a deleting instance shows); `STOPPED` and
  `TERMINATED` (GCP's names for a stopped VM) and `SUSPENDED` -> `terminating` too (the
  stopped-node rule of plan 14, A4: their disks still cost for as long as they exist, so the garbage collector deletes
  them; **open question 8, decided**); `REPAIRING` -> `running`. A deleted instance is gone from the API: `GetInstance`
  answers `NotFound` (the contract allows `NotFound` for a deleted instance; the
  conformance suite accepts either).
- **Addresses**: `networkInterfaces[].networkIP`, `accessConfigs[].natIP`,
  `ipv6Address`, `ipv6AccessConfigs[].externalIpv6`, read after the insert (a second `get`).
- **Delete**: `instances.delete` (an operation), after reading the instance and refusing
  one without this cluster's label (the same ownership rule as AWS; **GCP IAM conditions
  cannot test labels**, so this check is the only wall: a dedicated project is
  recommended, section 9); `NotFound` is success. Disks with `autoDelete` go with it.
- **List/Find**: one `instances.aggregatedList` with the label filter (all zones in one
  paged call; `returnPartialSuccess` for zones that are down), filtered to the regions
  asked; every state is listed, `terminating` included (a stopped VM must stay listed
  for the garbage collector, plan 14 A4).
- **Rate limits**: Compute API request quotas per project and per method apply (values
  not read: **unverified**); the driver reuses the AWS patterns (resolve caches, hourly
  machine type lists) and adds no per-call polling beyond the operation `wait`.

### Billing period (ADR-024, plan 23)

Per-second billing with a one-minute minimum (Keeper 1/60; the compute pricing page could not be read, so
**unverified**). A period under the `minPeriod` default of 10 minutes does not qualify, so retention does
not apply; `BillingStart` is `creationTimestamp` for the record only. Nothing to verify for retention.

## 4. Catalog, offerings and availability

- **Keeper** has GCP: 132 zones (130 active), 615 servers (75 arm64), regions named as
  Google's, storage rows for `pd-standard`, `pd-ssd`, `pd-balanced`, `hyperdisk-extreme`,
  `hyperdisk-throughput`, `hyperdisk-balanced`, `hyperdisk-ml`,
  `hyperdisk-balanced-high-availability` (no `pd-extreme`, no local SSD price), and GB-month
  storage prices per region (`/storage_prices?vendor=gcp`). The crawler drops sole-tenancy
  SKUs (vendor-capabilities): no dedicated tenancy.
- `Regions`: `regions.list`, only `status: UP`; `Zones`: `zones.list` per region (filter
  by region), `status: UP`, not deprecated; cached for 1 hour. A region the credentials
  cannot read (permission denied) is `RegionUnavailable` as on AWS.
- **`OfferingLister`**: `machineTypes.list` per zone (`fields=items/name`, hourly; about
  130 calls per hour for all zones, only the regions in scope are listed), keyed by the
  numeric Keeper zone ID. Machine types a zone does not offer are removed from its
  offerings. **Stock is not available through an API** (no stock status like Alibaba's);
  capacity stays reactive (`InsufficientCapacity`).
- **Drops**: custom machine types and types that need attached accelerators or
  reservations in a form the driver does not create. GPU machine types (`a2`, `a3`, `a4x`,
  `g2`, `g4`) launch like any type but have no driver (NVIDIA) support: they stay in the
  catalog only if plan 12's approach lands; until then the driver removes them from the
  catalog (like AWS's `mac*`), unverified list. Bare-metal (`-metal`) types: dropped.
- **Spot**: offerings carry both capacity types where Keeper has a spot price.

## 5. Quotas, errors, spot

**Quotas** (`QuotaReporter`, ADR-013). GCP gives usage with the limit, unlike AWS:
`regions.get` returns `quotas[]` of `{metric, limit, usage, owner}` (the page does not
enumerate the metrics) and `projects.get` the project-wide ones. No usage computation from
instances. Buckets: `CPUS` (region, vCPU), the family metrics (`N2_CPUS`, `C2_CPUS`, and the
`CPUS_PER_VM_FAMILY` quota of newer series, whose per-family dimension may be only in the
Cloud Quotas API), `PREEMPTIBLE_CPUS` (Spot), and the account-wide `CPUS_ALL_REGIONS`
(`GlobalQuotas`; the docs name it "CPUs (All Regions)" and say only some projects have it).
Facts from Google's quota page: E2 and N1 share a pool and most other types have their own;
M1 consumes `M1_CPUS`; Spot VMs consume standard quota **until the project has preemptible
quota in the region, and only preemptible quota after**; the instance-count quota is 10 times
the CPU quota.

**Unverified and decisive: which metrics one machine type consumes** (does `n2-standard-8`
consume `CPUS` as well as `N2_CPUS`, as overview §5.9 notes, or only `N2_CPUS`?) and how to
tell that a project has preemptible quota (a `PREEMPTIBLE_CPUS` limit above 0?). The spike
reads `regions.get` before and after a launch. Design that is safe either way:
`Consumption` returns buckets only for families whose mapping the spike confirmed
(and the quota scenario of the lane re-confirms, like plan 10's 4a); every other type is
**unknown** and takes the reactive path (`QuotaExceeded` from `QUOTA_EXCEEDED`, which
names the metric: `Quota 'CPUS' exceeded. Limit: N in region R` — the message form
sc-inspector matches with `Quota '`). A test asserts every GCP family of the committed
snapshot is mapped or on an explicit unknown list, as for AWS.

**Error mapping** (from `operation.error.errors[].code` and HTTP status; every code gets a
test; the codes below come from Google's availability page and general knowledge and are
**unverified live**):

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `ZONE_RESOURCE_POOL_EXHAUSTED`, `ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS` (its details may recommend zones: logged only), `REGION_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS`, `POOL_CAPACITY_INSUFFICIENT` |
| `QuotaExceeded` | `QUOTA_EXCEEDED` (any metric: also `IN_USE_ADDRESSES`, `DISKS_TOTAL_GB`, ...; the metric from the message triggers the region's quota refresh) |
| `Unsupported` | machine type not found in the zone (HTTP 404 on the machine type, code `NOT_FOUND`), `UNSUPPORTED_OPERATION`, an unsupported disk type for the series, Spot refused for the type (codes to be collected by the spike) |
| `ZoneUnusable` | zone down or in maintenance; `IP_SPACE_EXHAUSTED` (the subnet is full, like AWS's `InsufficientFreeAddressesInSubnet`; region-wide for a regional subnet: the whole region is skipped for 10 minutes by the core's zone rule, so the driver reports it for the zone it was asked for) |
| `Retryable` | HTTP 429, `RESOURCE_OPERATION_RATE_EXCEEDED`, 5xx, `INTERNAL_ERROR`, `backendError`, `rateLimitExceeded`, deadline exceeded of the driver's own timeout (`CallTimeout`) |
| `NotFound` | 404 on an instance get or delete |
| `Fatal` | `ALREADY_EXISTS` with a foreign instance, `CONDITION_NOT_MET` (organization policy), permission denied (403 without a quota code), invalid arguments, everything else |

**Spot** is a launch variation only (above); interruption handling is outside v1
(ADR-011): a preempted Spot VM is deleted (`instanceTerminationAction: DELETE`), the node
goes away and Karpenter replaces the claim. GCP's 30 second notice and `maxRunDuration`
are post-v1.

## 6. Node options and profiles (ADR-020)

The built-in defaults (layer 1; 00a §6.1): root disk `pd-balanced`, 64 GiB (`sizeGb`; GCP's
"GB" are GiB), encrypted by Google by default (CMEK optional); `publicIPv4: true`; NIC
type unset. **The one type-dependent default**: series that boot only from Hyperdisk
(`c4`, `n4`, `c4a`, `n4a`, `a3` and later, per vendor-capabilities; the list is a driver
table, **unverified**, to be filled from the spike and kept honest by a test against
Keeper's types) get `hyperdisk-balanced`, same size. A pinned `diskType` the series cannot
boot from makes the offering infeasible (`Supported`, ADR-017), never a substitute.

| Block | Fields |
|---|---|
| `storage.root`, `storage.data[]` | `diskType` (`pd-standard`, `pd-balanced`, `pd-ssd`, `hyperdisk-balanced`, `hyperdisk-extreme`, `hyperdisk-throughput`, `hyperdisk-ml`), `sizeGb`, `provisionedIops`, `provisionedThroughput` (Hyperdisk, and `pd-extreme` is not in Keeper: not offered), `kmsKeyName`; data volumes also `name`, `filesystem`, `mountPath` |
| `storage.localStorage` | `policy` (`ignore`, `mount`, `raid0`), `mountPath`: the bundled local SSDs of `-lssd` types (and of other series where the machine type includes them) |
| placement profile | `resourcePolicy: {groupPlacement: {collocation: COLLOCATED}}` (compact: 22 instances without `maxDistance`) or `{spread: {availabilityDomainCount}}` (limit unverified, about 8), or `{use: {name}}` |
| network profile | `nicType: GVNIC\|VIRTIO_NET`, `tier1: true` (`networkPerformanceConfig.totalEgressBandwidthTier: TIER_1`) |
| `serviceAccount`, `metadata`, `imageProject` | as above |

- **Validation** (`ValidateNodeClass`): regions, project (equals the controller's), image
  project format, `serviceAccount` e-mail format, reserved metadata keys, tag charset,
  resource references, and disk sizes, IOPS and throughput against Keeper's storage rows
  (min size, max size, max IOPS, max throughput: `pd-balanced` 10 to 65536 GB, 80000 IOPS,
  1200 MB/s; `hyperdisk-balanced` 4 to 65536, 160000, 2400; from `/table/storage`,
  2026-11-10) and the zone's `diskTypes`.
- **`Supported`**: compact placement only on series that support it and with
  `onHostMaintenance: TERMINATE` (unverified list), GVNIC only where the series supports
  it, Hyperdisk only on series that attach it, local storage only where the type includes
  local SSDs (Keeper's per-server local disks), spread/compact group-size limits tracked as
  `PlacementGroupFull` for the offering only (AWS's pattern).
- **`PriceDelta`**: disks at Keeper's GB-month price of the region and disk type divided by
  730; **incomplete** for provisioned Hyperdisk IOPS/throughput above the included
  baseline, for `tier1`, and for external IPv4 (Keeper has no GCP IPv4 price: the driver
  uses a documented constant, about 0.005 USD per hour, **unverified**, overridable like
  `SCARPENTER_AWS_IPV4_PRICE`) — the AWS note about the missing Keeper IPv4 price applies.
- **Device hints (00a §11)**: GCP is the vendor that **lets us name the device**: data
  volumes use `ByID: /dev/disk/by-id/google-<deviceName>` (no serial pattern, no equal-size
  ambiguity). That relies on the udev rules of `google-guest-configs` in the amd64 OEM
  sysext (present in the image's RDEPEND; the path naming for **NVMe**-attached disks is
  **unverified**: spike) and is the reason arm64 (no OEM sysext) needs the rule shipped in
  the Spare Cores image. Local SSD hint: `DevicePattern` over `/dev/disk/by-id/google-local-*`
  or the NVMe `nvme-nvme_card*` names (unverified), count from the type. Disk attach is
  part of `instances.insert`, so the volume unit's 240 s timeout is not at risk.
- **Resources and defaults**: section 2.

## 7. In-memory fake and conformance

`pkg/vendors/gcp/fake`, behind the `computeAPI` interface: regions with quotas (limit and
usage computed from the fake's instances, other-owner instances for project-wide usage),
zones with numeric IDs, machine types per zone, images (`kinvolk-public`, names and
families, `images.get` only, `list` denied, an arm64 image in a second project),
instances with states, labels and tags, a subset of the list filter language
(`labels.k=v`, `name`, `tags.items`), operations that finish later and carry error codes,
unique names per zone (409), the `requestId` rules as documented, `aggregatedList` with
partial success, networks, subnetworks (with `stackType`), firewall rules with target tags,
resource policies with the in-use refusal (`resourceInUseByAnotherResource`), a project
without a `default` network, an organization-policy violation, the `Quota '<m>' exceeded`
message, and error injection per call. Used by the conformance suite (idempotent create
across zones, failed create leaves the key unused, the two key rules, tags, delete
idempotency, quota mapping, the resource fixture hooks) and the driver's tests: the insert
request golden (every field asserted), the error table, `labelValue()` round trips and
collisions, instance-state mapping, the `delete` ownership refusal, `default` network
resolution. Coverage target as plan 08: 80 % of statements.

## 8. IAM and credentials

A **custom role** (`docs/reference/gcp-iam-role.yaml`, validated in `make lint`), not the
predefined `roles/compute.instanceAdmin.v1` (too wide: it includes `setIamPolicy`-adjacent and
all-disk powers): `compute.instances.{create,delete,get,list,setLabels,setMetadata,setTags,
setServiceAccount,setScheduling}`, `compute.disks.{create,delete,get,list,use,setLabels}`,
`compute.images.{get,getFromFamily,useReadOnly}` (the last one grants use of
`kinvolk-public`; the project's own images too), `compute.subnetworks.{get,list,use,
useExternalIp}`, `compute.networks.{get,list,use}`, `compute.firewalls.{list,get}`,
`compute.regions.{get,list}`, `compute.zones.{list,get}`, `compute.machineTypes.{list,get}`,
`compute.diskTypes.list`, `compute.projects.get`, `compute.zoneOperations.{get,list}`,
`compute.regionOperations.get`, `compute.globalOperations.get`,
`compute.resourcePolicies.{create,delete,get,list,use}` and, for managed resources only
(a second, optional role like AWS's optional statement), `compute.networks.{create,delete,
updatePolicy}`, `compute.subnetworks.{create,delete}`, `compute.firewalls.{create,delete}`;
`iam.serviceAccounts.actAs` only with `serviceAccount`; `cloudquotas.quotaInfos.get` and
`serviceusage.quotas.get` only if the Cloud Quotas API is used (permission list
**unverified**: checked by the lane's first run, by the "every permission used" test below).

**Least privilege is weaker than on AWS.** Compute IAM conditions can test the resource
name, type and resource-manager tags, **not labels**: the AWS statement "terminate only
with the cluster tag" has no direct equivalent. The driver's ownership check is the wall
(section 3), plus: a **dedicated project** for SCarpenter nodes (recommended in the docs
and used by the lane), and optionally resource-manager tags on instances (post-v1). A test
asserts that every permission the driver's interface methods need is in the role file and
that the role contains nothing else (the permission of each `computeAPI` method listed in
a table in the test, as `iam_test.go` does for AWS).

## 9. The L5 lane, canary and sweeper

All in this repository, as the "Adding a vendor lane" list of
`docs/how-to-guides/testing-and-releasing.md` says (the AWS lane's `test/e2e-aws/` is the
template; the control plane, the tailnet and the scenarios' shape are the same):

1. `test/e2e-gcp/` with the reusable workflow's scripts (`bootstrap.sh`, `mint-authkey.sh`,
   `install.sh`, `scenarios.sh`, `diagnostics.sh`, `teardown.sh`, `lib.sh` with `redact`),
   `defaults.sh` pins (Flatcar version, types, `E2E_SMOKE_SCENARIOS`, the quota knobs, the
   region and project variables), manifests (NodeClasses with the `tailscale` template).
2. A line `gcp` in `test/e2e-vendors.txt`, a credentials step in `e2e-cloud.yml` and
   `e2e-canary.yml` (`google-github-actions/auth` with the federated provider; no key
   file), and `e2e-gcp-sweeper.yml`.
3. The CI policy and the canary policy, each with a selftest: a **CI custom role** with
   the driver's permissions and, where GCP allows it, IAM conditions on the resource name
   for the lane's region (the zone is in the resource path; **unverified** that every
   used method supports conditions) and a **read-only canary role** (`get`/`list` only)
   plus a write-refusing client wrapper `test/canary/gcp_guard.go` with the reflection test
   of `guard_test.go`.
4. **Scenarios** (the AWS ones that apply; names as in the lane): `amd64` on-demand exact
   type (`gcp.e2-small`: node Ready, labels with the zone name and numeric zone ID
   consistent, instance labels and tags, metadata keys as §3 via `gcloud compute instances
   describe`), `spot` (an `e2-small` Spot VM; `arm64` only when its image exists:
   `gcp.t2a-standard-1`, a region where it exists), `scalezero`, `drift` (change
   `storage.root.sizeGb`), `quota` (`regions.get` limits and usage equal the reported
   buckets; the family consumption mapping of section 5 per tested family), `db` (a
   `n2` or `c3` type with `hyperdisk-balanced` data volume through the profile and a
   compact placement policy: the resource policy named and described by SCarpenter, both
   instances in it, the device at the `google-<name>` path, the policy deleted after),
   `described`, `byo` (the workflow creates a network, subnet and firewall with `gcloud`,
   a NodeClass `use`s them by ids; SCarpenter leaves them), `negative`, `uninstall`.
   A new scenario, **`create`**: no `use`: the managed network, subnet and firewall are
   created, used and deleted by their lifecycle (AWS's `create` path), and **`default`**
   if the lane's project has a `default` network. Checks of §3 on a real node: user-data
   readable only through the metadata server, no project SSH keys or OS Login users.
5. **Cost guards** (all must be true before the line in `e2e-vendors.txt`): a budget alert
   on the **dedicated project** (separate from sc-runner's: project-wide `ssh-keys`, shared
   quotas); CPU quotas set low (the ceiling), the CI role limited to the lane's region and
   types where conditions allow; every resource carries `scarpenter-cluster=scarpenter-ci-<run id>`
   and `ci-created-at` (labels on instances and disks; name prefix plus description on
   networks, subnets, firewalls and policies); sweeper every 3 hours; 90/55 minute limits;
   teardown fails the job when anything with the run's marker remains; approval through the
   `e2e` environment's reviewers.
   **The lane is not in `test/e2e-vendors.txt` yet**: none of these is shown on a live project.
   Before the line `gcp` is added, the operator verifies that the regional role's name
   conditions refuse a write outside the lane's region and a network resource not named
   `scarpenter-*`; that the project holds nothing else, its `CPUS` and `PREEMPTIBLE_CPUS` quotas
   are no higher than the lane needs (24 and 4) and the budget alert reaches a person; that the
   sweeper, with the real CI identity, deletes a dead run's instances, rules, subnetworks,
   networks and policies, and the teardown finds a leftover disk; that the canary role reads what
   the canary reads; and that `e2e-sweeper` and `e2e-canary` are limited to `main` and the
   provider's attribute condition names only this repository and its environments. Until then
   `make test-e2e-gcp-dryrun` works and the lane's selftest checks that the line is absent (the
   list in `test/e2e-gcp/defaults.sh` is the same).
6. **Sweeper** (`pkg/vendors/gcp/sweep`, as AWS): instances and disks by label cluster
   `scarpenter-ci-*` and age (over 2 hours), firewall rules, subnets, VPCs and resource
   policies by name prefix `scarpenter-scarpenter-ci-`, description marker and age, region
   narrowed by `-regions`, dependency order (instances, policies, firewalls, subnets,
   networks).
7. **Canary** (`test/canary/gcp.go`): facts (regions, zones), `ResolveImage` for amd64
   (`images.get` on `kinvolk-public` by the pinned version, and its `guestOsFeatures`),
   arm64 when configured, quotas against `E2E_MIN_VCPU_OD`/`_SPOT`-style minimums, machine
   type offerings for the lane's types with the quota mapping, and the read-only guard.
8. **Provisioned in the CI account by its operator (not in this repo)**: the project, a
   workload identity pool and provider for the GitHub issuer (subject
   `repo:SpareCores@.../scarpenter@...:environment:e2e`, attribute condition on the
   repository and environment), the CI and canary roles, the quotas, the environment
   secrets and variables (project, provider, service account), and a network with a
   `nodes` tag rule for the `byo` scenario (the lane's own `create` scenario makes its
   own).

**Cost per run**: `e2e-small` and `e2e-micro` Spot, on-demand cents per hour; the
dearest scenario (`db`, one `n2-standard-2`-sized type and a small Hyperdisk) a few cents; a
full run is expected in the cents range (unverified; the first live run reports it).

## 10. Docs

`docs/reference/gcp-driver.md` (regions, zones and the numeric IDs, images and the arm64
status, launch table, node settings, resources and defaults including the description
marker, network and egress, labels and tags, quotas and what is unverified, error mapping,
metadata exposure, deferred), `docs/reference/gcp-iam-role.yaml`,
`docs/how-to-guides/gcp-credentials.md` (workload identity federation for a cluster
outside GCP, the service-account key route, the metadata-server route), the lane section of
`docs/how-to-guides/testing.md`, the vendor-resources and node-settings references' GCP
rows; the image placement (GCP amd64 rung 1, arm64 rung 2) is ADR-021 and
`docs/explanation/flatcar-images.md`.

## Spike (before coding; read-only, with credentials the coordinator provides)

1. `images.get` on `kinvolk-public` for `flatcar-{stable,lts}-<version>`: existence, `architecture`,
   `guestOsFeatures` (GVNIC, UEFI, IDPF), `diskSizeGb`, the `lts`/`beta`/`alpha` name shapes.
2. `regions.get` and `projects.get` of a fresh project: the metric names, `CPUS_ALL_REGIONS`,
   per-family metrics for `n2`, `c3`, `c4`, `t2a`, `c4a`; the Cloud Quotas API for the rest.
3. One launch of each of `e2-small`, `n2-standard-2`, `c3-standard-4` and a Hyperdisk-only
   series with the Flatcar amd64 image and `regions.get` before/after: consumption mapping,
   Spot preemptible-quota behaviour, `onHostMaintenance` per series, GVNIC and NVMe boot,
   device paths of data disks (SCSI and NVMe), guest agent behaviour with
   `block-project-ssh-keys` and `enable-oslogin`, metadata reachability from a Pod.
4. The error codes of a quota, capacity, wrong machine type for the zone and organization
   policy failures (a capacity failure needs luck; use the scarce types in a small zone).
5. The arm64 route: a hand-made image from the generic arm64 release on a `t2a` and a
   `c4a` VM: does Ignition run as `gcp`, does networking come up, does the disk path rule
   work without the OEM sysext.
6. `requestId` replay semantics for a failed operation, and the retention period.

## Gates

CONVENTIONS gates 1 to 4 (L3 still green); `go test ./pkg/vendors/gcp/...` coverage 80 %
or more (report the number); `actionlint`, `shellcheck` and the selftests clean on the lane;
the role file valid and consistent with the driver (test); the conformance suite over the
full GCP type list of the snapshot; `make test-e2e-gcp-dryrun`. The first live run is the
coordinator's.

## Open questions for the maintainers

1. **arm64 in v1?** **Decided (ADR-021): yes, rung 2.** No official image; Spare Cores
   publishes its own from the images repository (plan 22), after spike 5 proves the build
   route. While no arm64 image resolves for the pinned version, the driver removes arm64
   GCP types from the catalog (ADR-017, no silent fallback).
2. **Project: controller flag or NodeClass field?** Recommendation: the controller targets
   one project (flag and Helm value); the NodeClass `project` is optional and must match.
   Multi-project (a project per NodeClass) multiplies clients, quotas and garbage
   collection and is post-v1.
3. **A dedicated project for the nodes and for the lane?** Recommendation: yes, separate
   from sc-runner's (project-wide SSH keys, quota sharing, and no IAM condition can protect
   instances by label).
4. **The `default` network and firewalls.** Recommendation: support `default` for subnets
   (as AWS's default VPC) and treat firewalls as `create`-only without a `use`, since the
   `default` network's rules target no tag (zero-configuration then means one small managed
   rule set per region). To decide with it: the `default` network's own rules
   (`default-allow-internal` from `10.128.0.0/9`, `default-allow-ssh` and `-rdp` from
   anywhere, unless the project removed them) apply to every instance, and GCP rules only add
   allows, so on that network `isolation: none|run` and an empty `inbound` cannot be honoured.
   Under ADR-017 that needs either a validation error for those settings on `default`, or
   only `nodeclass` isolation with a documented warning; a "synthetic no-tag firewall" with a
   warning (section 2) is a silent downgrade.
5. **Dependence on `kinvolk-public`** (an organization policy can block it, and the
   publisher's continuity is Flatcar's). Recommendation: rely on it, document the
   `imageProject` override and the constraint error, and do not mirror unless the spike
   shows `GVNIC` missing (then publish a Spare Cores amd64 image too).
6. **Metadata server exposure to Pods.** **Decided (plan 14, A7):** a vendor Butane fragment
   hook in the driver contract, `block-metadata-from-pods`, off by default; document the
   exposure.
7. **Firewall by network tag versus service account.** Tags are what an IaC typically provisions and
   what `isolation` maps to; anyone who can edit instance tags can join a tag. Recommendation:
   tags in v1; service-account-based rules (`sourceServiceAccounts`) need a service account
   on every node and are post-v1.
8. **`STOPPED`/`TERMINATED` instances.** **Decided (plan 14, A4):** reported `terminating`
   and garbage-collected, on every vendor (AWS's `stopped` changes too).
9. **Quota consumption rules.** Recommendation: ship only the spike-confirmed families as
   known; the rest reactive; add the Cloud Quotas API only if `regions.get` is insufficient.
10. **Managed network CIDR pool** `10.240.0.0/12` with the first free `/20`: it may overlap what a
    user's own networks use. **Decided and implemented:** keep it, and `create.pool` overrides it.

### Decided by default, maintainer to confirm

Taken while executing this plan (the lower layers, the driver, the lane); each is the plan's
recommendation or, where it gave none, the conservative option.

1. **arm64**: implemented as decided: the default lookup of an arm64 image has no project
   (`SpareCoresImageProject` is empty until plan 22 publishes), so `ImagesReady` reports arm64 not
   ready and its offerings are unavailable; `imageProject` or an `images` selector serve it.
2. **Project**: the controller flag `--gcp-project` (`SCARPENTER_GCP_PROJECT`, Helm
   `vendors.gcp.project`), else the credentials' project; the NodeClass `project` is optional and must
   equal it. The option is a separate core commit (`pkg/operator/options`).
3. **Dedicated project**: recommended in the docs and used by the lane; nothing else in the driver
   depends on it beyond the ownership check at delete.
4. **The `default` network**: used for `subnets` only when it can honour the request. `DefaultSuits`
   skips it, with the reason, when `publicIPv6` is on or when an enabled ingress rule of the default network
   applies to every instance (`default-allow-internal`, `default-allow-ssh`), which isolation and the
   inbound rules cannot override (GCP rules only add allows). No synthetic "no tag" firewall: that
   would be a silent downgrade (ADR-017). Firewalls are `create`-only without a `use`. On a standard
   project the zero-configuration path therefore creates a managed network.
5. **`kinvolk-public`**: relied on; `imageProject` overrides; `CONDITION_NOT_MET` is `Fatal` with the code
   `OrganizationPolicyViolation`. No mirror (the spike's `GVNIC` finding decides).
6. **Metadata exposure**: the hook `block-metadata-from-pods` is offered (`NodeHardener`), off by default.
7. **Tags, not service accounts, for firewalls** in v1.
8. **`STOPPED`/`TERMINATED`**: terminating, as decided.
9. **Quota consumption**: `ConfirmedSeries` is empty, so every machine type is unknown quota and takes
   the reactive path (`QUOTA_EXCEEDED` names the metric); Spot is always unknown; the Cloud Quotas API is
   not used.
10. **Managed network CIDR**: as planned: one global VPC per cluster (`scarpenter-<cluster>-subnets`) with one
    subnetwork per region, each the first free `/20` of the pool `10.240.0.0/12` found from the VPC's
    subnetworks (deterministic across restarts, never overlapping), and `resources.subnets.create.pool`
    (a private IPv4 CIDR of at least a `/20`) overrides the pool per NodeClass; the open question 10 is closed.
11. **No `LeftoverCollector`** (the optional garbage collector capability other drivers use for network
    interfaces and addresses a failed launch leaves): a GCP launch makes nothing besides the instance (its
    disks are part of the insert, its external addresses ephemeral, and GCE has no NIC object). Whether a
    failed insert can leave a disk is unverified; the lane's teardown checks disks by label, and a disk
    found there is the reason to add one.

Other defaults taken: the request ID of an insert carries a random per-process epoch and an attempt
counter that counts up after a definitive failure (a failed operation, a non-retryable HTTP error) and
stays after a lost response, because the replay of a failed operation's request ID is unverified; the
billing rule is reported unverified (retention off); a managed firewall always has a lowest-priority deny
rule, so it exists as an object the garbage collector can find; GPU (`a2`, `a3`, `a4`, `a4x`, `g2`, `g4`) and
TPU series, custom machine types and bare metal are dropped from the offerings; the role does not hold
`compute.firewalls.get` or `compute.diskTypes.list`, which no call uses; `gcp` is **not** in
`test/e2e-vendors.txt` (its cost guards are provisioned outside this repository), one line adds it.
