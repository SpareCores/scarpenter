# 25 — OCI driver (Oracle Cloud Infrastructure)

Depends on: 14 (the shared groundwork: the `images` kind, `RootDiskForType`, the idempotency label,
the stopped-node rule, account-wide quota errors, message-based classification, malformed
references, the one `use` selector), 23 (`Instance.BillingStart`), 10 (the L5 lane framework and
the canary), 13 part A (on main: local selection over the refresh's data) and, for step (a) only,
13 part B (branch `plan13-part-b`: the snapshot-reading `KeeperSource`; see "Dependencies").
Not on 22 (Flatcar publishes the OCI image itself), not on 24 (see "Sizes"). Read
`docs/plans/v1/CONVENTIONS.md` first. Spec: overview §5.8, §5.9, §7, §9, §11 (as the model),
00a §6, §9, §11; ADR-009, ADR-013, ADR-017, ADR-018, ADR-020, ADR-021, ADR-022, ADR-023, ADR-024;
`docs/reference/driver-contract.md`; the plans 15, 16 and 20 as the pattern (16 most of all).

Status: **draft for review.** Research date: 2026-11-14, from public documentation, the
`oci-go-sdk` v65.130.0 source, the sample Keeper data of OCI (SQLite, 2026-11-13), the crawler pull
request `SpareCores/sc-crawler#180`, plan 24's research (cited, not repeated) and the notes of the
Flatcar-on-OCI investigation (2026-11-13). No cloud API was called. Everything marked
**unverified** is settled by the spike (section 12) or by the first live run, and listed again in
"Unverified facts".

## Goal

Make OCI work end to end in SCarpenter soon: `pkg/vendors/oci`, a production driver (vendor ID
and registration name `oci`) with the in-memory fake and the conformance suite; the API block
`spec.vendors.oci` (`pkg/apis/v1alpha1/oci`), CRD and CEL; images (Flatcar's own OCI image,
imported into the user's tenancy); the resources (VCN, subnet, network security group; brought or
managed); a **static OCI catalog** that stands in for Keeper until Keeper serves OCI; the reference
docs, the IAM policy and the credentials how-to; and the L5 lane with sweeper and canary. This plan
writes and statically validates everything; **no live run happens without the maintainer's
go-ahead** (CONVENTIONS rule 8).

Decided by the maintainer on 2026-11-14 (fixed; the sections below apply them):

1. **Fixed, made-up instance types, not a size grid.** OCI's flexible shapes take any OCPU count and
   memory within bounds; Spare Cores defines a fixed list of named sizes per shape, as AWS has a
   fixed list of types. The names and sizes are the `server` rows of Keeper's OCI data
   (`VM.Standard.E5.Flex.2-16`). Plan 24's NodeClass-configurable grid is **not needed** for OCI now;
   plan 24 stays a draft and the grid stays a later option (note added to plan 24's status and §7).
2. **Keeper does not serve OCI yet.** SCarpenter ships a static, embedded OCI catalog, generated
   from the sample data by a small committed generator, plugged in at the seam where Keeper data
   enters the catalog, and swapped for real Keeper data later by a flag.
3. **Images**: Flatcar's `oraclecloud` image, created in the user's tenancy (rung 3 of ADR-021).
4. SCarpenter stays IaC-neutral (ADR-022): this plan names no infrastructure repository; "the
   operator's IaC" means whatever creates the resources (Pulumi, Terraform or OpenTofu, the CLI, the
   console).

## 1. What is different on OCI (read this first)

1. **Sizes are fixed names per shape** (section 2): `VM.Standard.E5.Flex.2-16` is shape
   `VM.Standard.E5.Flex` with 2 OCPUs and 16 GB. The driver launches the shape with a `shapeConfig`
   derived from the name; the non-flexible shapes (`BM.*`, `VM.Standard2.*`, ...) are launched as
   they are.
2. **Keeper's `api_reference` is not unique per vendor for OCI**: every size of a shape has the
   shape as its `api_reference` (`VM.Standard.E5.Flex`), and only `server_id` is unique. The catalog
   assumes one `APIReference` per vendor and **silently keeps the lowest ID** of rows that share it
   (`pkg/catalog/snapshot.go`, the `byAPIRef` loop): OCI would collapse to one type per shape.
   Section 6 adds the rule that fixes this once, in the catalog's conversion.
3. **Keeper has no OCI data**: the embedded catalog (section 6).
4. **Images are regional and cannot be shared with other tenancies** (no `allAuthenticatedUsers`
   equivalent; Community Applications and cross-tenancy PAR imports are the only routes). Flatcar
   ships an official OCI image (qcow2) from Stable 4757.2.3 on. Rung 3 (section 3).
5. **Availability domains (AD) are named per tenancy** (`Xxxx:EU-FRANKFURT-1-AD-1`: a prefix that
   differs by tenancy) and most regions have one. Keeper's zone is `<region>-ad-<N>` with the name
   `EU-FRANKFURT-1-AD-1` (no prefix). The driver maps both ways (section 4).
6. **Limits are per AD and per shape family, in cores and in GB**, read with the Limits service's
   `GetResourceAvailability`; `QuotaReporter.Consumption` has no zone argument (section 7: a
   contract gap, with a safe v1 answer).
7. **"Out of host capacity" is `InternalError`, HTTP 500**, and the SDK's default retry policy
   retries every 5xx (up to eight times, about a minute and a half): the launch call runs with
   `common.NoRetryPolicy()` and the driver classifies (section 5, section 7).
8. **No server-side tag filter** on `ListInstances`, `ListSubnets`, `ListImages` and the others:
   tags are matched client-side, and the idempotency lookup lists the compartment's instances.
9. **A launch is asynchronous** (the instance is `PROVISIONING`; the work request finishes it); the
   boot volume is deleted with the instance only when asked (`preserveBootVolume=false`).
10. **Billing is per second with a 60 second minimum** (Keeper's vendor row: increment 1, minimum
    60, stopped server not charged); bare metal has a one-hour minimum that the contract cannot
    express by family. Retention (ADR-024) never applies to OCI in v1 (section 5).
11. **Preemptible capacity** is a fixed 50 % discount on the flexible VM shapes (no spot market),
    terminated on reclaim, never combinable with bare metal, burstable or extended memory.
12. **No default network.** A tenancy has no default VCN; the resolution order of ADR-022 goes from
    discovery straight to `create` (section 9).
13. **The instance metadata service has no hop limit** (IMDS v2 needs only a header): Pods can read
    `user_data`. The hook `block-metadata-from-pods` is offered (section 8).
14. **Regions need a subscription** (one-way, tenancy-wide; pay-as-you-go and trial tenancies are
    limited to one region by default). `Regions()` returns the subscribed ones.

## 2. Sizes and instance types (decision 1)

**Source of truth: the `server` rows of Keeper's OCI data.** Per the crawler (`_oci.py`:
`_shape_configs`, `_FLEX_OCPUS`, `_FLEX_MEMORY_GB_PER_VCPU`), each flexible shape is expanded to
the OCPU counts `1, 2, 4, 8, 16, 32, 64` and the shape's maximum, each at 2, 4 and 8 GiB of memory
per vCPU, clipped to the shape's `ocpuOptions` and `memoryOptions` from `ListShapes`:

| What | Value | Check against the sample data (2026-11-13) |
|---|---|---|
| Server ID and `display_name` | `<shape>.<ocpus>-<memoryGB>` | `VM.Standard.E5.Flex.2-16`: 2 OCPUs, 16 GB |
| `api_reference` | the shape (`VM.Standard.E5.Flex`) | the same for every size of the shape |
| `api_reference_object` | `{"shape": ..., "shape_config": {"ocpus": 2, "memory_in_gbs": 16}}`; fixed shapes `{"shape": ...}` | present on every row |
| vCPUs per OCPU | x86 (AMD, Intel): 2; `A1`: 1; `A2`, `A4`, `A4.Ax`: 2 | `E5.Flex.2-16` 4 vCPUs; `A1.Flex.4-8` 4 vCPUs; `A2.Flex.2-8` 4 vCPUs |
| Memory per OCPU | x86, `A2`, `A4`, `A4.Ax`: 4, 8 or 16 GB; `A1`: 2, 4 or 8 GB (2, 4, 8 per vCPU) | holds for all 256 flexible rows |
| `memory_amount` | `memoryGB x 1024` MiB (the crawler's GB are GiB) | `E5.Flex.2-16` 16384 |
| Fixed shapes | server ID = shape (`BM.Standard3.64`, `VM.Standard2.1`) | 16 bare metal and 15 VM rows are active |

Counts: 288 server rows, 287 active (the Always Free micro shape is inactive), **256 sized rows
of 12 flexible shapes** (17 to 24 sizes each), 16 bare metal, 15 fixed VM. One active row
(`BM.DenseIO.A4.Ax.72`) has no price. Dense I/O VMs, GPU shapes and the burstable variants are not in
the data (`ListShapes` returns only shapes with a limit in the crawler's tenancy; burstable
configurations are skipped by the crawler). The OCPU is not the vCPU; the facts are plan 24 §1.1.

**The parse rule** (`pkg/vendors/oci`, `ParseType`; one function, used by launch, `Get`, quota and
validation):

```
name   := fixed | sized
sized  := shape "." ocpus "-" gb          # split at the LAST "."; the tail matches ^[1-9][0-9]*-[1-9][0-9]*$
fixed  := shape                           # anything whose tail is not ocpus-gb: BM.Standard3.64, VM.Standard2.1
shape  := ^(VM|BM)\.[A-Za-z0-9._-]+$
```

- A **sized** name must name a shape that `ListShapes` reports `isFlexible` (the driver reads the
  shape table of the AD, cached for an hour). `ocpus` and `gb` are integers (no `1.5`, no `2.0`);
  `ocpus` within `ocpuOptions.min..max`; `gb` within `memoryOptions.minInGBs..maxInGBs`; `gb/ocpus`
  within `minPerOcpuInGBs..maxPerOcpuInGBs`. A violation is `Unsupported` with the code
  `InvalidInstanceType`, raised before any call. A **fixed** name must be a non-flexible shape
  of the AD, else `Unsupported`.
- **Names that do not parse are rejected, never guessed** (ADR-017): `VM.Standard.E5.Flex` (a
  flexible shape without a size), `VM.Standard.E5.Flex.2` , `VM.Standard.E5.Flex.2-16-3`,
  `VM.Standard.E5.Flex.0-4`, `VM.Standard.E5.Flex.02-16`, `e5.2-16` all fail with `InvalidInstanceType`
  (`Fatal` when the shape prefix itself is not `VM.` or `BM.`).
- `FormatType(shape, ocpus, gb)` is the inverse. `Instance.Type` of a returned instance is rebuilt
  from the instance's `shape` and `shapeConfig` (`ocpus`, `memoryInGBs` as integers) and must equal
  the `CreateRequest.InstanceType` byte for byte; a foreign instance with fractional sizes gets
  `%g` formatting and is never one of ours.
- **Keeper-conformance test**: every row of the embedded catalog parses; for sized rows
  `vcpus == ocpus x k(shape)` with `k` from the table above and `memory_amount == gb x 1024`; a
  row that fails is a data error the test names. The table of `k` is the driver's only vCPU
  knowledge (an unknown Arm generation is `Fatal` at startup of the test, not at launch).
- **`shapeConfig`** of the launch: `{ocpus: <n>, memoryInGBs: <gb>}` for sized names (both float32
  in the SDK), nothing for fixed shapes. `vcpus` is never sent. `baselineOcpuUtilization` is never
  sent (full OCPUs: no burstable types in the catalog).
- **Extended memory** (OCPU or memory beyond one NUMA node: `maxPerNumaNode` of `ocpuOptions` and
  `maxPerNumaNodeInGBs` of `memoryOptions`; for E5, 94 OCPUs and 1049 GB; plan 24 §1.1) needs a
  separate limit and cannot be preemptible. The rows are in the data (`E5.Flex.126-*`,
  `E4.Flex.114-*`, `Standard3.Flex.56-*`; they are the rows of a preemptible series **without** a spot
  price) and in the catalog. v1 does not launch them: the pre-launch check above also compares with
  the per-NUMA limits and returns `Unsupported` with the code `ExtendedMemoryUnsupported` (24 hours
  unavailable per zone, no cloud call). Open question 3 asks the crawler to flag them so the
  catalog can drop them.
- **No `OfferingLister`** in v1. `LaunchableTypes` must list the exact catalog names per zone (a
  zone with a list makes every unlisted name unlaunchable, `pkg/quota/tracker.go`), and the driver
  cannot enumerate sized names without the catalog. What replaces it: the quota buckets (a family
  with limit 0 in an AD has no headroom, section 7) and the reactive `Unsupported`
  (open question 4).

**Plan 24.** Its status line and §7 get a note (this change): plan 25 uses fixed named sizes per
the maintainer's decision of 2026-11-14; `server_shape`, `server_shape_price`, `sc-keeper-go/flex`,
the grid, sticky live sizes and `ShapeCapacityMonotonic` are not needed for OCI now; the grid
remains a later option (it would also serve GCP custom machine types). The data model of this plan
is forward compatible: a size is a `Server` row either way, and the name `<shape>.<ocpus>-<gb>`
is a subset of what plan 24 proposed (`3o-24g`).

## 3. Images: the rung

**Rung 3** (ADR-021): images created in the user's own tenancy, **per region and architecture**,
from Flatcar's official OCI image. Confidence: high for amd64, medium for arm64 (works for Flatcar's
CI on `A1`; `A2`, `A4` untested).

| Question | Answer | Source, confidence |
|---|---|---|
| Rung 1: a public Flatcar image offered by OCI? | No: not in the platform images, the Marketplace or Community Applications | the investigation's searches (platform image lists of three regions held no Flatcar entry; absence of a listing is not proof); medium |
| Rung 2: can anyone publish public images? | Not in the way GCP can. Custom images are regional and private. Sharing across tenancies: export to Object Storage and import by pre-authenticated request (per consumer, expiring); or a Community Application per region and release (no review or fee documented). **Not built in v1** | OCI docs (image import/export, Community Applications); medium |
| Rung 3: images in the user's tenancy? | Yes: `ImportImage` from an Object Storage object (QCOW2 or VMDK, up to 1 TB) | high |
| Does Flatcar publish an OCI artifact? | Yes: `flatcar_production_oraclecloud_image.img` (qcow2, about 594 MB) with `.DIGESTS`, `.DIGESTS.asc`, `.sig`, for `amd64-usr` and `arm64-usr`, channels stable, beta, alpha, since June 2026; Stable 4757.2.3 has it; 4459, 4593 do not | release server and Flatcar scripts PR 3846/4123/4143; high |
| Is it fit to boot? | OEM id `oraclecloud`; Ignition platform `oraclecloud` reads user data from the instance metadata; Afterburn has the provider (attributes, hostname, SSH keys). Flatcar's CI boots it on the sponsored account (amd64 and arm64 on `A1`) | Ignition and Afterburn docs, Flatcar mantle; high |

**What the operator's IaC must create, per region and per architecture** (the naming and tag
contract SCarpenter's default lookup relies on; nothing here names a tool):

1. Download the release's `flatcar_production_oraclecloud_image.img` (verify the signature and
   the digest against Flatcar's published key), upload it to an Object Storage bucket (a bucket in
   one region plus a read-only pre-authenticated request imports into other regions: **verified
   live** once: a cross-region import by PAR worked and took about 6.5 minutes).
2. Import it in each region as a custom image in the controller's compartment: source type
   `QCOW2`, launch mode `PARAVIRTUALIZED`, operating system `Flatcar`, operating system version =
   the Flatcar version (`4757.2.3`), display name **`flatcar-<channel>-<version>-<arch>`**
   (`flatcar-stable-4757.2.3-amd64`), freeform tags **`flatcar-channel`**, **`flatcar-version`**,
   **`arch`** (`amd64` or `arm64`) and `os=flatcar`.
3. Shape compatibility: the import lists the shapes it was tested with (65 for amd64, including 16
   bare metal). Add every shape SCarpenter may launch (`VM.Standard.E5.Flex` and so on).
4. **arm64 only**: an image capability schema (firmware `UEFI_64`, paravirtualized launch mode,
   network attachment, boot volume and remote data volume types) and shape compatibility for the Arm
   shapes (`VM.Standard.A1.Flex` is what Flatcar's CI adds; `A2` and `A4` are untested and must be
   added by the operator). Flatcar's docs omit these steps; the mechanics are in its mantle code
   (`platform/api/oraclecloud/api.go`). That the schema survives the launch with the per-instance
   options of section 5 is **unverified** (spike S7).
5. Cost: custom image storage is billed like Object Storage Standard on the image's billable size
   (observed 1 GB for the amd64 image: cents per version and region); quota `custom-image-count` is
   100 per region (25 on pay-as-you-go and trial); import is about 6.5 minutes.
6. Keep the version the NodeClass pins (`spec.flatcar.version`); an old image can stay until its
   nodes are gone. Changing image OCIDs is not drift (ADR-012).

**How SCarpenter finds the image** (`ResolveImage`, the `images` kind, ADR-021, ADR-023):

- `spec.vendors.oci.resources.images.use`, evaluated per region and architecture:
  `ids: {<region>: [ocid1.image...]}` (OCI images are regional: no `*`), `name` (glob on the display
  name, placeholders `{channel}`, `{version}`, `{arch}`, `{region}`), `tags` (freeform; client-side),
  `match` (`architecture`, `state`, `createdAfter`; **no `owner`**), `select` default `exactlyOne`,
  `orderBy: created`. Native `filters`: `operating-system`, `operating-system-version`, `shape`,
  `lifecycle-state` (server-side: `ListImages` parameters); no `expression`.
- **Default** (no `images`, `Use.IsDefault()`): `ListImages(compartmentId = the controller's
  compartment, displayName = flatcar-<channel>-<version>-<arch>, lifecycleState = AVAILABLE)`,
  then the selector as above. The platform images the API also returns for any compartment are
  ignored (their operating system is not `Flatcar`).
- **Architecture** is not an `Image` field. It is the freeform tag `arch` when present, else
  derived from the image's compatible shapes (`ListImageShapeCompatibilityEntries`: any shape
  starting `VM.Standard.A` or `BM.Standard.A` means arm64, else amd64), else unknown, and an image of
  unknown architecture never satisfies a request. `match.architecture` and the request's
  architecture narrow the same way (`selector.NarrowArchitecture`).
- **The version check** (ADR-017, `vendors.CheckImageVersion`): version from the tag
  `flatcar-version`, else `operatingSystemVersion`; channel from the tag `flatcar-channel`, else
  empty (compared only when present). An explicit ID is held to the same rule. No version is
  `ImageVersionUnknown`, another is `ImageVersionMismatch`.
- `ReportImages` (`ImageReporter`) reports each region and architecture for `ImagesReady` and
  `status.vendors.oci.images`; `Image.RootDiskBytes` is the image's `sizeInMBs` (about 46.6 GiB
  for the Flatcar image; the root volume may be larger, never smaller: section 8).

**What boots.** Flatcar's `oraclecloud` image with the OEM id `oraclecloud`. Ignition reads the
user data from the instance metadata service (the `user_data` metadata key, base64; the exact
endpoint version is **unverified**, spike S6). SCarpenter's Ignition (the tiny outer config that
merges the gzip+base64 parts, then the sysext download from GitHub) needs **egress**: a public
IPv4 address on an internet-gatewayed subnet, or a NAT gateway (section 9). Afterburn is not used
(ADR-007). `/etc/hostname` comes from Ignition, so the OCI hostname is irrelevant to the join. The
kubelet's DNS is the VCN resolver (169.254.169.254 by DHCP); Flatcar is not told about swap,
sysctls, modules or MTU by SCarpenter (the node checklist of the investigation lists what to read
back on a real node; spike S6 and the lane settle it: DNS, time sync, the NIC name for flannel,
VNIC MTU, ports 10250 and 8472 in the network security group).

The image placement is added to ADR-021's ladder (OCI: rung 3) and to
`docs/explanation/flatcar-images.md` by the coordinator when this plan lands.

## 4. The `spec.vendors.oci` block and the hand-off

OCI's units: **region** (`eu-frankfurt-1`, Keeper `api_reference`; aliases are the three-letter
keys: `FRA`), **availability domain** (zone) and **compartment** (the scope of every call).

```yaml
vendors:
  oci:
    regions: [eu-frankfurt-1, us-ashburn-1]   # allow-list; empty = every subscribed region
    compartmentId: ocid1.compartment.oc1..example   # optional; must equal the controller's compartment
    storage:
      root: {sizeGb: 64, performance: balanced}      # lower-cost | balanced | higher-performance | ultra-high-performance
    networking: {}                                    # per-vendor override of spec.networking
    metadata: {}                                      # extra instance metadata (never user_data, ssh_authorized_keys)
    resources:
      subnets:               {use: {ids: {eu-frankfurt-1: [ocid1.subnet.oc1.eu-frankfurt-1.example]}}}
      networkSecurityGroups: {use: {ids: {eu-frankfurt-1: [ocid1.networksecuritygroup.oc1.eu-frankfurt-1.example]}}}
      images:                {use: {ids: {eu-frankfurt-1: [ocid1.image.oc1.eu-frankfurt-1.example]}}}   # optional: default is the naming lookup of section 3
```

`spec.tags` become freeform tags (keys and values as OCI allows: key up to 100 characters, value up to 256;
a `scarpenter-` prefix is reserved; defined tags are not supported and rejected, ADR-017).
`spec.vendors.oci.metadata` keys `user_data`, `ssh_authorized_keys` and the keys OCI reserves are rejected.

### Resource kinds

| Kind | Scope | Default lifecycle | `use` forms | `create` | What your IaC must output |
|---|---|---|---|---|---|
| `subnets` (the network, the VCN, is implied by the subnet and returned as `ResourceRef.Network`) | region (a regional subnet serves every AD; an AD-specific subnet serves one: `ZoneGaps`) | `idleTTL` | `ids` (subnet OCIDs), `name` (glob on the display name; `{region}`), `tags` (freeform, client-side), `match` (`cidr`, `within`, `network` = the VCN OCID, `state`, `zone` = the subnet's AD, `createdAfter`), `filters` (`vcn-id`, `lifecycle-state`, `display-name`); `select` default `exactlyOne` | yes: a VCN `scarpenter-<cluster>-<region>` (default `10.240.0.0/16`, `create.cidr` overrides), an internet gateway, a route table (`0.0.0.0/0` to the gateway), an empty security list, and one regional public subnet of the first `/20` | the subnet OCID per region |
| `networkSecurityGroups` | region, in the subnet's VCN (`InNetwork: subnets`) | `idleTTL` | `ids`, `name`, `tags`, `match` (`network`, `state`, `createdAfter`), `filters` (`vcn-id`, `lifecycle-state`, `display-name`); `select` default `all` (at most 5 per VNIC) | yes: one group `scarpenter-<cluster>-<nodeclass>-<region>` with the rules of `spec.networking.inbound`; for `isolation: nodeclass` an allow-all rule whose source is the group itself | the group OCIDs per region |
| `runNetworkSecurityGroups` (internal, like AWS's `runSecurityGroups`) | run | `perUse` | not configurable | one group per run key (source = itself), attached to the run's instances | nothing |
| `images` (not managed) | region | not applicable | section 3 | never (ADR-021) | the image OCIDs, or images named by the contract of section 3 |

Without `use` (ADR-022): step 2 (discovery) is the selector `tags: {scarpenter-use: "true"}` on the
kind (`TagKey(TagUse)` = `scarpenter-use`, the same key every vendor's discovery uses); step 3
(vendor defaults) does not exist on OCI (no default VCN: no `ResourceDefaults`); step 4 `create`;
step 5 `ResourcesUnavailable`. A tenancy's other VCNs are never used without an opt-in.

**Ownership and marker** (ADR-018). Every managed resource (VCN, internet gateway, route table,
security list, subnet, group) carries the freeform tags `scarpenter-cluster`, `scarpenter-nodeclass`,
`scarpenter-managed-by=scarpenter` and `scarpenter-resource-kind`, and a display name starting
`scarpenter-<cluster>-` (the engine's hash rule shortens long names). Both are required before
the garbage collector touches a resource; `List` lists the compartment's resources of the type
(no server-side tag filter) and checks the tags. Instances carry the logical tags of the driver
contract as freeform tags (section 5).

### What the operator's IaC must provide, and what SCarpenter self-manages

The operator's IaC provides, all optional except the first two:

1. **The compartment** (`compartmentId`, or the controller's `--oci-compartment`) and the
   **region subscriptions** of every region in the allow-list (a one-way tenancy action).
2. **The images** per region and architecture (section 3).
3. **The controller's identity** and its policy (section 4, "Credentials"; `docs/reference/oci-iam-policy.txt`).
4. **A subnet per region and the network security group** (`subnets.use`,
   `networkSecurityGroups.use`, or the discovery tag): egress to GitHub (sysexts) and the Kubernetes
   API server, inbound 10250 from the control plane and the CNI ports, UDP 41641 for the `tailscale`
   template, ICMP. A brought subnet with `prohibitPublicIpOnVnic` needs a NAT gateway.
5. **Quotas and limits**: service limit increases and compartment quotas as the operator wants
   them (section 7); compartment quotas are also the hard cost ceiling of the lane (section 11).

SCarpenter self-manages, without `use`: the VCN, gateway, route table, empty security list and
subnet (`create`), the groups, and the run groups.

### Account scope and credentials

- **Compartment.** Instances, boot volumes, the managed network and the default image lookup live in
  **one compartment**: `--oci-compartment` / `SCARPENTER_OCI_COMPARTMENT` (Helm
  `vendors.oci.compartment`), **required** (the tenancy root is not a default). The NodeClass field
  `compartmentId`, when set, must equal it (`VendorReady` says otherwise). Brought subnets, groups
  and images may live in other compartments the credentials can read. The **tenancy** OCID comes from
  the credentials. Multiple compartments are post-v1.
- **Authentication.** The SDK's `common.ConfigurationProvider`, chosen by `vendors.oci.credentials.mode`
  (Helm; flag `--oci-auth`):
  1. `workloadIdentityFederation` (**recommended for a controller anywhere**, no API key): the
     SDK's `auth.TokenExchangeConfigurationProviderFromIssuer` with a `TokenIssuer` that reads the
     projected ServiceAccount token file, exchanged at an IAM Identity Domain's token endpoint
     (a confidential application's client ID and secret, a `urn:oci:token-type:oci-upst` request) for a
     short-lived user principal session token. The identity domain needs a trust for the cluster's
     OIDC issuer. This is also how CI gets in (section 11). The one stored secret is the application's
     client secret (a Kubernetes Secret); the SDK refreshes the token. The exact builder fields
     and the audience are **unverified** (spike S11).
  2. `instancePrincipal`: `auth.InstancePrincipalConfigurationProvider()`, for a controller on an OCI
     compute instance in a dynamic group.
  3. `okeWorkloadIdentity`: `auth.OkeWorkloadIdentityConfigurationProvider()`, for a controller on
     OKE.
  4. `apiKey`: a Secret with `tenancy`, `user`, `fingerprint`, `region` (the home region) and the
     private key; `common.NewRawConfigurationProvider`. Works anywhere; the key is a long-lived
     credential (documented as the fallback).
  5. `configFile` (development): `~/.oci/config` with a profile, including session-token profiles
     (`ConfigurationProviderForSessionTokenWithProfile`).
- **Nodes carry no instance principal** and no credentials (the metadata service is the exposure,
  section 8).
- **Startup check**: `ListRegionSubscriptions` (proves the identity and the tenancy), then
  `ListInstances(limit 1)` in the compartment (proves the policy); a failure is `VendorReady=False`
  with the reason (`NotAuthenticated`, `NotAuthorizedOrNotFound`).

## 5. Driver

**SDK**: `github.com/oracle/oci-go-sdk/v65` (v65.130.0, 2026-11-10; Go 1.26; the controller's Go version
is checked in step (b)): `core` (`ComputeClient`, `VirtualNetworkClient`), `identity`
(`IdentityClient`), `limits` (`LimitsClient`), `common`, `common/auth`. The generated clients are
concrete structs, so narrow interfaces wrap them (`computeAPI`, `networkAPI`, `identityAPI`,
`limitsAPI`), as `ec2API` does. One client of each kind **per region** (`SetRegion`); the identity
client of the home region for the subscription list. User agent `scarpenter/<version>`. Per-call
deadline 45 s. **Retry policy**: reads use `common.DefaultRetryPolicy()`; `LaunchInstance` uses
`common.NoRetryPolicy()` (finding 7 of section 1: the default retries the capacity error), and the
driver's classification (HTTP 429 and 5xx other than capacity: `Retryable`) and the core's backoff
do the rest. `TerminateInstance` and the deletes use the default policy (idempotent).

**Launch** (`CreateInstance`): `LaunchInstance` with:

| Field | Value |
|---|---|
| `compartmentId` | the controller's compartment |
| `availabilityDomain` | the full AD name of the request's zone (a per-region map name -> full name from `ListAvailabilityDomains`, cached for an hour) |
| `faultDomain` | unset (Oracle's advice for capacity) |
| `shape`, `shapeConfig` | from `ParseType` (section 2) |
| `displayName` | the NodeClaim name (the lookup key of `FindInstance` by name; not unique in OCI, so never trusted alone) |
| `sourceDetails` | `InstanceSourceViaImageDetails{imageId, bootVolumeSizeInGBs, bootVolumeVpusPerGB, kmsKeyId}` |
| `createVnicDetails` | `subnetId`, `assignPublicIp` from `networking.publicIPv4` (default true), `assignIpv6Ip` from `publicIPv6`, `nsgIds` (the groups, plus the run group), `displayName`, `freeformTags`; no `hostnameLabel`; `skipSourceDestCheck` false |
| `metadata` | `user_data` = base64 of the rendered Ignition; the block's `metadata`; never `ssh_authorized_keys` (Ignition installs keys) |
| `instanceOptions` | `areLegacyImdsEndpointsDisabled: true` **only after spike S6 shows Ignition and Afterburn use the v2 endpoints**; false until then |
| `launchOptions` | amd64: none (the image's); arm64: `firmware: UEFI_64`, `bootVolumeType`, `networkType`, `remoteDataVolumeType` all `PARAVIRTUALIZED` (what Flatcar's CI sets; **unverified**, S7) |
| `preemptibleInstanceConfig` | spot: `PreemptibleInstanceConfigDetails{PreemptionAction: TerminatePreemptionAction{PreserveBootVolume: false}}`; on-demand: unset |
| `availabilityConfig` | unset |
| `freeformTags` | the logical tags, the NodeClass tags and the idempotency tag (below); `definedTags` unset |
| header `opc-retry-token` | `scarpenter-` + 32 hex of SHA-256 over (NodeClaim UID, region, AD, shape, ocpus, memory, capacity type, per-process epoch, attempt counter), 43 characters |

- **Tag keys** (`TagKey`): `scarpenter-<key>` (the restrictive mapping: `scarpenter-cluster`,
  `scarpenter-nodeclaim`, `scarpenter-nodepool`, `scarpenter-nodeclass`, `scarpenter-managed-by`,
  `scarpenter-idempotency-key`, `scarpenter-use`). Whether OCI accepts `scarpenter.sparecores.com/<key>`
  in freeform tag keys is unverified and not needed. Values are written as given (at most 256
  characters; the UID is 36). `spec.tags` keys are validated against OCI's limits and the
  reserved prefix.
- **Idempotency** (plan 14 A3). `opc-retry-token` protects one request against a lost response
  (tokens live 24 hours); it cannot give the contract: a replay in **another AD, shape or capacity
  type** must return the original, and a failed create must leave the key unused. So, as GCP:
  (a) before every create, list the region's instances (`ListInstances` in the compartment, all states
  but `TERMINATED`; paged, up to 1000 per page; **no tag filter exists**, so the filter on
  `scarpenter-idempotency-key` is client-side; the call is `FindInstance` of
  `vendors.FindByIdempotencyKey`; `displayName` narrows it server-side when the tags carry the
  NodeClaim name) and return the instance when it exists; (b) launch with the token above, so only
  the same offering shares a token; (c) a token **conflict** (HTTP 409, the token reused with a
  different request: the exact code is unverified) is resolved by (a), and an instance found with the
  key is returned, else `Fatal`. The token carries an attempt counter that counts up after a
  definitive failure and stays after a lost response, because the replay of a failed launch's token is
  unverified (S4). Open question 5 considers the Search service or one cached list for scale.
- **Instance states** (`LifecycleState`): `PROVISIONING`, `STARTING` -> `pending`; `RUNNING`,
  `CREATING_IMAGE`, `MOVING` -> `running`; `STOPPING`, `STOPPED`, `TERMINATING` -> `terminating`
  (the stopped-node rule of plan 14 A4: a stopped Standard instance stops compute billing but its boot
  volume still costs, so the garbage collector deletes it); `TERMINATED` -> `terminated` (OCI keeps a
  terminated instance listed for about twelve hours: `GetInstance` returns it,
  `ListInstances` of the driver drops it).
- **Addresses**: `GetInstance` reads `ListVnicAttachments(instanceId)` and `GetVnic` (private IP, public
  IP, IPv6 addresses); the CSR approver needs them (`pkg/controllers/csr`). `ListInstances` leaves
  them empty.
- **Delete**: read the instance, refuse one without this cluster's tag (the only wall: OCI policy
  conditions cannot test freeform tags on all verbs: the compartment is the boundary, section 11), then
  `TerminateInstance(preserveBootVolume=false, preserveDataVolumesCreatedAtLaunch=false)`. `404`, and
  `TERMINATING` or `TERMINATED`, are success. A reference that is not `<region>/ocid1.instance....`,
  or names a region the credentials cannot use, is `NotFound` (plan 14 A9), never `Fatal`.
- **Provider ID** `scarpenter://oci/<region>/<nodeclaim>`; **InstanceRef** `<region>/<instance OCID>`;
  `BillingStart` = `timeCreated` (see "Billing period").
- **Names**: the node name is the NodeClaim name (Ignition writes `/etc/hostname`); the OCI hostname
  label is unset.
- **Asynchronous errors**: an accepted launch can still end in a work request failure or an instance that goes
  `TERMINATED` at once. `CreateInstance` returns the `PROVISIONING` instance and does not wait; the
  node that never registers is the core's 15 minute rule, and the work request errors
  (`workrequests.ListWorkRequestErrors`, header `opc-work-request-id`) are logged by the driver when
  `GetInstance` finds a terminated instance of this cluster that never ran. Capacity and limit
  errors are **synchronous** on the launch call (documented; S5 verifies the status codes).
- **Rate limits**: Compute request limits per tenancy and region apply (values not read:
  unverified); the driver caches the shape, AD and limit tables (an hour), reuses one instance list
  per create attempt and adds no polling beyond `GetInstance`.

### Billing period (ADR-024, plan 23)

Keeper's vendor row: increment 1 second, minimum 60 seconds, stopped server not charged. A VM never
qualifies: its minimum less the margin is below the two minutes the hold needs
(`MinimumTooShort`). Bare metal has a one-hour minimum (the row's comment); `BillingRule(region,
family)` is keyed by Keeper's `family`, which does not tell `BM.Standard.*` from `VM.Standard.*`
(`Standard` for both), so the driver implements no `BillingDescriber`: retention is off for OCI,
reported unverified, and a bare metal minimum is a follow-up that needs the series in the rule's key.
`BillingStart` is set for the record.

## 6. Catalog: the embedded stub and the seam

**What Keeper has** (the sample data; the real Keeper will be bigger): vendor `oci`: 44 regions (3
active), 9 zones (three ADs in each of Frankfurt, Ashburn, Phoenix), 288 servers, 2180 on-demand and
1481 spot hourly price rows (USD, per AD; the same price in every AD and region, from the public
price list: plan 24 §1.2), 4 block volume performance levels with GB-month prices
(`lower-cost` 0.0255, `balanced` 0.0425, `higher-performance` 0.0595, `ultra-high-performance`
0.0765; per region), IPv4 (free) and traffic prices, the vendor row with the billing facts. No
benchmark scores: the benchmark and selection labels of OCI types are `DoesNotExist` (a pinned
objective makes unscored types unavailable, as the static source does).

**The seam.** `catalog.Source` (`pkg/catalog/source.go`) is where data enters; `MuxSource` routes
vendors to their own source and the rest to Keeper (main and plan 13 part B). The stub is a
`catalog.Source` for the vendor `oci`, registered in the mux, and produces `model.Data` **in the shape
of Keeper's snapshot rows**, including part B's `StoragePrices` and `IPv4Prices` and the vendor row,
so the driver's `KeeperPriceConsumer` and the billing code see the same thing from both. Nothing in
`KeeperSource` changes except the one rule below.

- **The data format is Keeper's own**: the generator writes the files of Keeper's snapshot layout for
  the vendor (`vendor` with the one row, `region/oci`, `zone/oci`, `server/oci`, `server_price/oci`,
  `storage_price/oci`, `ipv4_price/oci`: gzip of columnar JSON, `{column: [values]}`, values as
  Keeper's API serialises them) plus a manifest (path, rows, SHA-256), under
  `pkg/catalog/embeddeddata/oci/` (`//go:embed`). The source decodes them with
  `keeper.DecodeSnapshotRows[keeper.TableServer]` and the other row types of `sc-keeper-go` (already
  pinned by main), and converts with the same functions as `KeeperSource` (`serverFromTable`, the price
  conversion; after part B, the shared body of `loadVendor`): **no second conversion**. A real Keeper
  snapshot of OCI therefore parses unchanged.
- **The generator**: `hack/oci-catalog/generate.py` (Python 3 standard library only: `sqlite3`,
  `gzip`, `json`, `hashlib`; no new Go dependency). `python3 hack/oci-catalog/generate.py --db
  <sc-data-oci.db> --out pkg/catalog/embeddeddata/oci` rewrites the files deterministically (sorted,
  fixed gzip mtime). Conversions from the SQLite enum names to the API values: allocation
  `ONDEMAND`/`SPOT` -> `ondemand`/`spot`, status `ACTIVE`/`INACTIVE` -> `active`/`inactive`,
  `cpu_architecture` `X86_64`/`ARM64` -> `x86_64`/`arm64`, `cpu_allocation` `DEDICATED` -> `Dedicated`,
  units `HOUR`/`MONTH`/`GB_MONTH` -> `hour`/`month`/`GB/month`, storage type lower case. It keeps
  every column the Keeper files have and `api_reference_object` as is. It **drops** nothing: the
  catalog and the driver decide (inactive servers are skipped by the converter). It prints the counts
  and the rows it could not convert. The committed README of the directory (in the generator's
  docstring, not a Markdown file) says where the input comes from and that the prices are as of the
  data's `observed_at`.
- **No other tooling**: the input database is not in the repository, so no CI step regenerates; a
  test checks that every file matches the manifest's SHA-256 and row count, that every file decodes into
  `sc-keeper-go`'s types, and pins the counts of section 2 and the spot rule (spot rows exist exactly for the
  series of `_PREEMPTIBLE_SERIES` and for non-extended sizes).
- **Mapping to the catalog**: `Server.ID` = Keeper `server_id`, `Server.APIReference` = the **type
  name** (rule R below). `Zone.ID` = Keeper's `zone_id` (`eu-frankfurt-1-ad-1`), `Zone.Name` = the zone's
  `api_reference` (`EU-FRANKFURT-1-AD-1`). Prices are per AD, so the catalog builds an offering per
  active AD of each region in the allow-list.

**Rule R (in the catalog's conversion, one place, used by every source).** Active rows of one
vendor that **share an `api_reference` take their `server_id` as `APIReference`**; rows with a
unique `api_reference` keep it. Today no vendor in Keeper's data has duplicates (checked on the
committed snapshot), so nothing existing changes; before this rule the loser rows of a duplicate were
dropped silently. It is data-driven, names no vendor, applies the same to the stub and to a real
Keeper snapshot of OCI, and keeps the byte-for-byte equality "type name = server ID = what the driver
parses". The metrics path (`byAPIReference`) maps through the converted servers, so scores follow. A test feeds a
Keeper-shaped duplicate set and asserts the names; a second assertion makes any remaining
duplicate **loud** (an error log with the rows, not a silent drop).

**Wiring.** `pkg/operator/catalog.go` builds the mux; the embedded vendors are one option:
`--embedded-catalog-vendors` / `SCARPENTER_EMBEDDED_CATALOG_VENDORS`, default **`oci`**, Helm
`catalog.embeddedVendors`. Each listed vendor with embedded data gets `MuxSource.Vendors[<vendor>]`; a
request that concerns only embedded vendors never reaches Keeper (so an OCI-only deployment needs no
Keeper key; the static dev catalogs of the fake and qemu drivers work the same way). The source
reports `Data.Version` as a digest of the manifest and `PublishedAt` as the generator's data date;
how part B's `SnapshotRead` and the freshness logic treat the embedded data is decided in step (a) so
that the data neither claims to be Keeper's nor trips a staleness alarm. At start the controller logs
`vendor oci is served from the embedded catalog (data of 2026-11-13); prices and sizes are static`,
exports the gauge `scarpenter_catalog_embedded_vendor{vendor}` = 1, and the NodeClass condition
`VendorReady` message for OCI says the same.

**When Keeper starts serving OCI.** The flag decides, and it is a one-line change: remove `oci` from the
default of `--embedded-catalog-vendors` (or set it empty in the Helm values); the mux then routes
`oci` to Keeper, the embedded files are unused, and rule R makes the real rows (one `api_reference`
per shape) into the same type names, so node labels, NodePool requirements and running nodes keep
resolving. A later cleanup commit deletes `pkg/catalog/embeddeddata/oci`, `hack/oci-catalog` and the
option. Until then the stub is **ignored**, never merged with Keeper's rows of the same vendor.
Nothing in the driver changes: it reads `model.Data` through `Facts` and `SetKeeperPrices`.
A consistency check at the swap: the embedded type names must be a subset of Keeper's (a test of the
swap's runbook in the docs: compare `kubectl get` of the instance types before and after).

**Zones and regions.** `Regions()` = the subscribed regions (`ListRegionSubscriptions`, `READY`)
intersected with the allow-list; `Zones(region)` = `ListAvailabilityDomains` with
`Zone{ID: <region>-ad-<N>, Name: <name without the tenancy prefix>}`, `N` from the suffix `-AD-<N>`
(`PHX-AD-1` -> `us-phoenix-1-ad-1`, as the crawler's `_zone_id`). The full name goes in the launch.
The tenancy-specific AD numbering maps to different datacenters per tenancy (prices are the same,
so Keeper's per-AD rows stay valid). The zone name is the value of `topology.kubernetes.io/zone`, so
the prefix with its colon (not a label-value character) must not be in it. A zone without a Keeper
row (an AD the sample lacks) has no offerings.

**Spot offerings** exist where Keeper has a spot price (flexible and fixed VM series of the
preemptible list: `Standard1`, `Standard.B1`, `Standard2`, `Standard3`, `Standard.E2..E6`, `Standard.A1`,
`Standard.A4`, `Optimized3`, not `A2`, `Standard4.Ax`, `E6.Ax`, `A4.Ax`, not bare metal). The sample
data also prices old fixed VMs as spot (`VM.Standard2.*`); whether OCI accepts them is learned from the
error (`Unsupported`).

## 7. Quotas and errors

**Quotas** (`QuotaReporter`, ADR-013). The Limits service: `LimitsClient.GetResourceAvailability(
serviceName "compute", limitName, compartmentId, availabilityDomain)` returns `used` and `available`
(and `effectiveQuotaValue` when a compartment quota policy applies; fractional variants exist). The
limit names come from `ListShapes`: each shape lists its `quotaNames` (`standard-e5-core-count` and
`standard-e5-memory-count` for the E5 flexible shape; the fixed shapes have one count name each).

- **Buckets** (region scope): one per quota name seen in the region's shapes. `ID` = the quota
  name; `Unit` = `OCPU` for `*-core-count`, `GB` for `*-memory-count`, `instances` for the others (a
  name of another form makes every type of that shape **unknown**, so reactive). `Limit` and `Used`
  from the **AD with the most headroom** (`Limit = used + available` of that AD): the tracker then
  blocks a family only when no AD has room, and never wrongly. Per-AD buckets would be exact, but
  `Consumption(instanceType, capacityType)` has no zone argument, and a launch refused in one AD
  would refresh the region's quota and try the same AD again (open question 6 proposes the
  contract extension: an optional zone-aware consumption and a zone on `QuotaExceeded`; the decision
  is the maintainers').
- **`Consumption`**: for a sized flexible type, the shape's core bucket `+= ocpus` and memory bucket
  `+= gb`; for a fixed shape its count bucket `+= 1`. Spot consumes the same buckets (whether
  preemptible capacity has its own limit: **unverified**, S9, so spot types are **unknown** until the
  spike confirms it, the GCP rule). Types whose shape is not in the cached shape table are unknown.
- **Which usage**: `used` of `GetResourceAvailability` is "in the given compartment"; the limit is the
  tenancy's. Whether the call on the controller's compartment sees the whole tenancy's usage is
  **unverified** (S3); if not, the driver asks with the tenancy OCID where the policy allows it (open
  question 7).
- `GlobalQuotas`: none. The test asserts that every shape of the embedded catalog is mapped or on an
  explicit unknown list (as AWS and GCP).

**Error mapping** (`oci/errors.go`, from `common.ServiceError`: HTTP status, `GetCode()`, message; every
row gets a test; the codes are from Oracle's API error page and community reports: **unverified live**).
The message-based rows follow the contract's rule (A6: the HTTP status is captured per request, each
text is documented here and pinned by a table-driven test, an unmatched text is `Fatal` or
`Retryable`, never `InsufficientCapacity`):

| Class | HTTP, code, message |
|---|---|
| `InsufficientCapacity` | 500 `InternalError` with the message containing `Out of host capacity` (also `Out of capacity`); 500 `InternalError` with `Out of capacity for shape` |
| `QuotaExceeded` | 400 `LimitExceeded` (the message names the limits: `The following service limits were exceeded: standard-e5-core-count`; not account-wide); 400 `QuotaExceeded` (a compartment quota policy; also the limit name) |
| `Unsupported` | 400 `InvalidParameter` with `Shape <x> is not compatible with image`, `shape ... not available`, `shape is not supported in the availability domain`, or an extended memory message; the driver's own pre-checks (`InvalidInstanceType`, `ExtendedMemoryUnsupported`) |
| `ZoneUnusable` | the subnet has no free IP addresses (400 or 409 `InvalidParameter`/`Conflict` naming the subnet: unverified; the core's zone rule skips the region for a regional subnet) |
| `Retryable` | 429 `TooManyRequests`; 5xx and network errors other than the capacity rows; 409 `IncorrectState` and `LockConflict` on deletes of resources still in use; the driver's own timeout (`CallTimeout`) |
| `NotFound` | 404 `NotAuthorizedOrNotFound` on get, terminate and the resource gets; a malformed instance reference |
| `RegionUnavailable` | from `Zones`: 404 `NotAuthorizedOrNotFound` or `NotAuthenticated` for a region not subscribed or not readable |
| `Fatal` | 404 `NotAuthorizedOrNotFound` on the launch (a missing subnet, image or compartment, or no permission: OCI does not say which; the message names the request), 401 `NotAuthenticated`, 403, other 400 `InvalidParameter`, 409 on a token reuse with a foreign instance, everything else |

**Spot.** Interruption handling is outside v1 (ADR-011): a reclaimed instance is terminated (and its
boot volume deleted); the node goes away and Karpenter replaces the claim. OCI's two-minute event is
post-v1. Preemptible instances cannot be stopped: the launch sets `TERMINATE`.

## 8. Node options and profiles (ADR-020)

The built-in defaults (layer 1; 00a §6.1): root volume 64 GiB (OCI's minimum is 50 GB; the Flatcar
image is about 46.6 GiB), performance `balanced` (10 VPUs/GB), Oracle-managed encryption;
`publicIPv4: true`. `RootDiskBytes` = `sizeGb x 2^30`; `RootDiskForType` is not needed (the root volume is
the NodeClass's, not the type's).

| Block | Fields |
|---|---|
| `storage.root` | `sizeGb` (50 to 32768 from Keeper's storage rows), `performance` (the four Keeper storage IDs: VPUs 0, 10, 20, 30), `vpusPerGb` (only with `ultra-high-performance`: 30 to 120 in steps of 10), `kmsKeyId` |
| `storage.data[]`, `storage.localStorage` | **not supported in v1**: rejected by validation (ADR-017). Data volumes need `LaunchVolumeAttachments` and device hints for paravirtualized attachments (unverified); the Dense I/O VMs are not in Keeper's data |
| placement profile | none (no placement groups, fault domains or capacity reservations in v1) |
| network profile | none |
| `networking.publicIPv4`, `publicIPv6`, `isolation`, `inbound` | as the generic block |
| `metadata` | as above |

- **Validation** (`ValidateNodeClass`): regions (subscribed), compartment equals the controller's,
  OCID formats, reserved metadata keys, tags, resource references, root size against the storage
  rows (min 50, max 32768), the VPU rule, and that no data volume or local storage is configured.
- **`Supported`**: `ultra-high-performance` above 30 VPUs only where the shape and region accept it
  (unverified: the rows say 120 VPUs/GB at most); `publicIPv6` only on a subnet with an IPv6 CIDR (a
  managed subnet has none in v1: the offerings are unavailable with a reason); `publicIPv4: false`
  only on a brought subnet (a managed subnet is public: a NAT gateway is not created in v1);
  `isolation: run` and `nodeclass` always (groups).
- **`PriceDelta`**: root volume GB x Keeper's GB-month price of the region and performance level /
  730 (the level's price includes its VPUs: `balanced` 0.0425 = 0.0255 + 10 x 0.0017); incomplete for
  VPUs above the level's and for a boot volume's included free allowance (not modelled); IPv4 is free
  (Keeper's price is 0).
- **Device hints**: none (no extra volumes).
- **`NodeHardener`**: the opt-in `metadata-from-pods` hardening
  (`vendors.BlockMetadataFromPods("169.254.169.254")`): OCI's metadata service answers any process
  with the `Authorization: Bearer Oracle` header and has no hop limit, and `user_data` holds a
  one-hour bootstrap token and the Tailscale key. Off by default; documented.
- **Resources and defaults**: section 4 and 9.

## 9. Resources (`ManagedResources`)

Kinds: `subnets`, `networkSecurityGroups`, `runNetworkSecurityGroups` (internal, `perUse`) and the
selector-only `images`. `KindSpec.InNetwork`: groups live in the VCN of the subnet (`ResourceRef.Network`
= the VCN OCID).

- **`Resolve`** by ID: `GetSubnet`, `GetNetworkSecurityGroup`; the malformed or unknown ID is `NoMatch`
  with the ID named. By selector: `ListSubnets` and `ListNetworkSecurityGroups` in the compartment of
  the VCN (the filters `vcnId`, `displayName`, `lifecycleState` server-side), then
  `selector.Apply`.
- **`Ensure` of `subnets`** (create): serialised per (cluster, region), idempotent, by marker:
  find the tagged VCN `scarpenter-<cluster>-<region>`; create what is missing in this order: VCN
  (`CreateVcn`, CIDR from `create.cidr`, default `10.240.0.0/16`, no IPv6), internet gateway
  (`CreateInternetGateway`), route table (`CreateRouteTable`, `0.0.0.0/0` to the gateway), an
  **empty security list** (no ingress, egress allow-all: the subnet must not inherit the VCN default
  list, which allows SSH from anywhere; groups carry the policy), subnet (`CreateSubnet`: regional, the
  first free `/20` of the VCN, `prohibitPublicIpOnVnic` false, the route table and the empty list). A
  part left by a failed creation is adopted and finished by the next call (all parts carry the
  marker); `Ensure` while a part is `PROVISIONING` is `Retryable`.
- **`Ensure` of `networkSecurityGroups`**: `CreateNetworkSecurityGroup` and
  `AddNetworkSecurityGroupSecurityRules` (the rules of `spec.networking.inbound`; egress allow-all;
  `isolation: nodeclass` adds an ingress rule from the group itself; ICMP and UDP 41641 per the
  `tailscale` template).
- **`InUse`**: groups by `ListNetworkSecurityGroupVnics`; subnets by one `ListVnicAttachments` of the
  compartment, client-filtered on the subnet. **`Delete`** is `Retryable` while attached (409
  `Conflict`), in the dependency order: groups, subnet, route table, internet gateway, security list,
  VCN. A VCN is deleted only when no subnet of ours remains and it carries the marker.
- **`ZoneGaps`** (`ZoneChecker`): an AD-specific brought subnet serves only its AD; the other ADs are
  gaps with the reason.
- **No `ResourceDefaults`, no `DefaultChecker`, no `LeftoverCollector`**: a launch makes no
  object besides the instance and its VNIC and boot volume (deleted with it); whether a failed launch can
  leave a boot volume is unverified (the lane's teardown looks).
- **IPv6, NAT gateways, service gateways, DRGs, peering** are post-v1; the contract for brought
  networks covers them (the user's subnet may route anywhere).

## 10. In-memory fake and conformance

`pkg/vendors/oci/fake`, behind the narrow interfaces: regions with subscriptions (`READY`, and one
unsubscribed), ADs with tenancy prefixes (one tenancy-numbering case), shapes per AD with `isFlexible`,
`ocpuOptions`, `memoryOptions`, `quotaNames`, per-NUMA limits, images (custom, with tags, a platform
image, an arm64 image reachable only through compatibility entries), instances with states and
freeform tags, the display-name filter, `opc-retry-token` semantics as documented, launches that fail
with the codes of section 7 (capacity 500, `LimitExceeded`, `InvalidParameter` incompatible shape,
429), a terminated instance still listed, `GetResourceAvailability` with used and available computed
from the fake's instances, VCNs, subnets, groups with attached VNICs (409 on delete), gateways,
route tables and security lists, and error injection per call. Used by the conformance suite
(idempotent create across AD, shape and capacity type; failed create leaves the key unused; the two key
rules; the tag round trip with `StoresIdempotencyKey`; delete idempotency; malformed references;
quota mapping; the resource fixture hooks and the `images` subtests) and the driver's tests: the
launch request golden (every field asserted, sized and fixed, spot, arm64), `ParseType` over
every row of the embedded catalog and the reject table, the error table (every row), tag and
retry-token determinism, state mapping, the delete ownership refusal, AD mapping, the quota
mapping, the managed network order and adoption, the IAM policy file (section 11). **Coverage
target 85 %** of statements of `pkg/vendors/oci/...` (as the vendor plans' latest).

## 11. The L5 lane, canary and sweeper

All in this repository, as `docs/how-to-guides/testing-and-releasing.md` says ("Adding a vendor
lane"; `test/e2e-gcp` is the template: the k3s control plane on the CI runner reached over the
tailnet, nodes with the `tailscale` template).

1. `test/e2e-oci/`: `bootstrap.sh`, `mint-authkey.sh`, `install.sh`, `scenarios.sh`,
   `diagnostics.sh`, `teardown.sh`, `lib.sh`, `defaults.sh` (pins: region `eu-frankfurt-1` by default,
   Flatcar channel and version, the types below, the quota knobs, the compartment and credentials
   variables; no OCID in the repository), `manifests/`, `selftest.sh`, `sweeper/`.
2. A line `oci` in `test/e2e-vendors.txt` **only when every cost guard below is true on the live
   tenancy** (the AWS line stays alone until then); the credentials step in `e2e-cloud.yml` and
   `e2e-canary.yml`; `e2e-oci-sweeper.yml`; `make test-e2e-oci-dryrun`.
3. **CI identity**: keyless, the `workloadIdentityFederation` mode of section 4 with GitHub's OIDC
   token: the repository's subject (`repo:<owner>/scarpenter:environment:e2e`, and a read-only one for the
   canary) is exchanged at the identity domain for a user principal session token (the SDK provider with
   a `TokenIssuer` that returns the job's token); the CI account's operator creates the trust, the
   confidential application, a CI group and policy, and the canary's read-only group and policy. Nothing
   about that account lives in this repository.
4. **Scenarios** (the AWS and GCP ones that apply): `amd64` (`oci.VM.Standard.E5.Flex.1-4`: node
   Ready, labels with the zone name and Keeper zone ID consistent, freeform tags on the instance,
   `shapeConfig` equal to the name via `oci compute instance get`, metadata keys as section 5, the boot
   volume deleted), `spot` (the same size preemptible), `arm64` (`oci.VM.Standard.A1.Flex.1-2`; runs only
   when the lane's arm64 image and capability schema exist: capacity of `A1` may be short, the scenario
   retries `A2.Flex.1-8`), `scalezero`, `drift` (change `storage.root.sizeGb`), `quota` (the reported
   buckets equal `GetResourceAvailability` for the lane's family, section 7), `described`
   (`oci.VM.Standard.E5.Flex.1-4`, `.1-8`, `.2-8`), `byo` (the workflow creates a VCN, subnet and group
   with the CLI; a NodeClass `use`s them by ids; SCarpenter leaves them), `create` (no `use`: the managed
   VCN, gateway, route table, security list, subnet and group are created, used and deleted by their
   lifecycle), `negative`, `uninstall`. Checks on a real node: user data readable only through
   the metadata service, no SSH keys in the metadata. No `db` scenario (no data volumes in v1); no bare
   metal (cost and boot time; unverified).
5. **Cost guards** (all true before the line in `e2e-vendors.txt`): a **dedicated compartment**
   (the compartment is the only ownership wall: IAM cannot protect an instance by freeform tag on
   every verb) with **compartment quotas** (the Quotas service) capping `compute` cores and memory at
   what the pins need: the hard ceiling, stronger than GCP's; a budget alert on the compartment; every
   resource carries `scarpenter-cluster=scarpenter-ci-<run id>` and `ci-created-at`; the CI policy limited to the
   compartment and the lane's region (`where request.region = ...` and `target.compartment.id = ...`: **unverified**
   which verbs accept the conditions); sweeper every 3 hours; 90/55 minute limits; teardown fails
   the job when anything with the run's marker remains, including **unattached boot volumes** (they have no
   tags of ours: the dedicated compartment makes any boot volume older than 2 hours the lane's); approval
   through the `e2e` environment's reviewers. **The lane is not in `test/e2e-vendors.txt` until the
   maintainer has verified these live**; `make test-e2e-oci-dryrun` and the selftest check that the
   line is absent.
6. **Sweeper** (`pkg/vendors/oci/sweep`, as AWS and GCP): instances by tag cluster `scarpenter-ci-*` and
   age (over 2 hours), then groups, subnets, route tables, internet gateways, security lists, VCNs by
   name prefix `scarpenter-scarpenter-ci-` and the marker tags and age, unattached boot volumes of the
   dedicated compartment older than 2 hours, regions narrowed by `-regions`, dependency order.
7. **Canary** (`test/canary/oci.go`, `oci_guard.go`, the reflection test of `guard_test.go`): facts
   (subscriptions, ADs), `ResolveImage` for amd64 (the pinned version by the naming contract) and
   arm64 when configured (`ListImageShapeCompatibilityEntries` has the Arm shapes), quotas against
   `E2E_MIN_*` minimums, the shape table for the lane's types with the quota mapping, and a
   write-refusing client wrapper.
8. **Provisioned in the CI account by its operator (not in this repo)**: the compartment, the compartment
   quotas, the identity domain trust and the groups and policies, the images per region, the region
   subscriptions, the environment secrets and variables (compartment, identity domain URL, client ID and
   secret, region), and a VCN with a `nodes` group for the `byo` scenario.

**Cost per run**: `1-4` and `1-2` sizes on-demand and preemptible: cents (E5 1 OCPU + 4 GB is $0.038
per hour; the lane runs minutes per scenario); a full run is expected in cents (unverified; the first
live run reports it). **The IAM policy** (`docs/reference/oci-iam-policy.txt`, validated in `make
lint` by `iam_test.go`, which maps every method of the narrow interfaces to the statement that covers
it): `manage instance-family` in the compartment (instances, boot volumes, VNIC attachments, image
reads), `use virtual-network-family` in the compartment (and in the compartments of brought
resources), `inspect compartments` and `inspect tenancies` in the tenancy (region subscriptions and
ADs), `inspect`/`read` on `resource-availability` or `limits` for the quotas, an **optional** second
block `manage virtual-network-family` in the compartment only for NodeClasses that use `create`. Which
verb each call needs is **unverified** (S12); the file is corrected by the first live run, as AWS's.

## 12. Spike (before coding the driver's cloud calls; read-only unless said; with credentials the coordinator provides)

S1. `ListRegionSubscriptions`, `ListAvailabilityDomains` in two regions: names, prefixes, numbering,
    single-AD regions (what `Zones` returns).
S2. `ListShapes` per AD: `isFlexible`, `ocpuOptions`, `memoryOptions` (incl. per-NUMA limits),
    `quotaNames`, `isSubcore`, `billingType`; which shapes are missing without a limit; whether the
    grid rows of the sample data are inside the bounds.
S3. `GetResourceAvailability` for `standard-e5-core-count` and `-memory-count` per AD, on the
    compartment and on the tenancy: does `used` see the whole tenancy; the effect of a compartment quota.
S4. `opc-retry-token`: replay of a successful launch; replay of a **failed** launch (capacity or
    limit) with the same body; a token reused with another shape (status and code); retention.
S5. The error shapes of a limit failure (reduce a quota to 0 in the dedicated compartment), of capacity
    (needs luck: `A1` or a large shape; HTTP status and exact message of "Out of host capacity"), of an
    incompatible shape for the image, an extended memory size, a missing subnet, a full subnet, and of a
    token conflict.
S6. A launch of Flatcar amd64 (the investigation's node checklist is the script): the endpoint version
    Ignition and Afterburn use (`/opc/v2/...`), user data base64, `areLegacyImdsEndpointsDisabled`,
    DNS, time sync, NIC name and MTU, device names, kubelet join against a control plane;
    metadata reachable from a Pod.
S7. The same for arm64 on `A1`, then `A2` and `A4` (the schema and compatibility entries; the launch
    options of section 5 against the image's own).
S8. The freeform tag key charset (`scarpenter.sparecores.com/x` accepted?), 100/256 limits, tag
    propagation to the boot volume (none expected).
S9. Preemptible launch: limit consumed (own or the same as on-demand), the reclaim path
    (instance state afterwards, boot volume).
S10. `ListInstances` volume: 500 instances paged, latency, rate limit behaviour; `displayName` filter
     exactness; time to appear after launch (eventual consistency of the idempotency lookup).
S11. The token exchange for the keyless modes (cluster OIDC and GitHub OIDC): builder fields, audience,
     token lifetime and refresh, a policy for the resulting principal.
S12. The policy statements per call (a dedicated group with the proposed statements; the minimal set);
     whether `where` conditions on compartment and region hold.
S13. Managed network: create and delete order, the conflicts, a subnet's behaviour with the empty
     security list and a group; NSG limits per VCN and per VNIC.

## 13. Scope (what the executing models build, one step at a time, each ending green)

Every step: a **fresh agent per step**, CONVENTIONS rules (no commit, no live calls), the gates below. Steps
(a) and (b) are independent; (c) needs (b) and, for the catalog, (a); (d) needs (c).

**(a) The embedded catalog.** `hack/oci-catalog/generate.py` and its committed output
`pkg/catalog/embeddeddata/oci/`; `pkg/catalog/embedded.go` (`NewEmbeddedSource`), rule R in the
conversion (`uniqueAPIReferences`), the option, the Helm value and the docs
(`controller.md`: the flag and the gauge; `labels-and-offerings.md`: OCI's names, zone names and the
missing scores). Tests: the files against the manifest; the counts of section 2; the catalog built
from the embedded data has `oci.VM.Standard.E5.Flex.2-16` with on-demand and spot prices in three ADs of
three regions, the fixed shapes, no collapsed shapes (before rule R: one type per shape, as a
regression test of the Keeper-shaped duplicate set); rule R and the loud duplicate; a request for `oci`
alone never reaches the default source; `MuxSource` merges the embedded `StoragePrices` and
`IPv4Prices`. **Requires 13 part B on main** (or the coordinator's go-ahead to build on `plan13-part-b`);
if part B is late, build the source against `model.Data` and the main branch's conversion and rebase.

**(b) The driver foundation** (no CRD, no registration): `pkg/vendors/oci`: `ParseType`/`FormatType` and
the vCPU table, the narrow interfaces, `fake/`, the SDK adapters behind the interfaces (compile-checked,
unit-tested against the fake), `errors.go` (the table of section 7), `names.go`, tags and the
retry token, the shape and AD caches, `quota.go` (buckets, `Consumption`, the unknown list), `iam_test.go`
(with the policy file), state mapping. The conformance suite runs over the fake with the embedded
catalog's types. No exported driver registration yet.

**(c) The full driver and the surfaces**: `Init`, `Regions`, `Zones`, `ResolveImage`/`ReportImages`,
`CreateInstance`, `Get`/`Find`/`List`/`Delete`, `NodeOptions`, `ManagedResources` with the managed network,
`NodeClassValidator`, `ZoneChecker`, `NodeHardener`; `pkg/apis/v1alpha1/oci` (types, deepcopy, CEL: OCID
patterns for `compartmentId`, region name pattern, `create` and `use` mutually exclusive as the other
vendors, no `use.ids` without a region key, rejected unknown fields), the `vendors.oci` block in
`SCNodeClass` and the CRD, the registration (`cmd/controller/oci.go`), the controller flags
(`--oci-compartment`, `--oci-auth`, the identity-domain options) and the chart (`vendors.oci`,
credentials modes, values schema, golden `helm template`), the published example NodeClasses and
schema, `docs/reference/oci-driver.md`, `oci-iam-policy.txt`, `docs/how-to-guides/oci-credentials.md` and
`oci-images.md` (the contract of section 3, as a how-to for a tool-neutral operator), rows in
`vendor-resources.md`, `node-settings.md`, `resource-selectors.md`, `infrastructure-handoff.md`, and the
overview's quota table (this change adds the row), the lane section pointers. The coordinator updates
ADR-021 and `docs/explanation/flatcar-images.md`.

**(d) L5** (statically validated; **no live run without the maintainer's go-ahead**): `test/e2e-oci`,
the sweeper, the canary and its guard, the workflows (`e2e-cloud.yml` and `e2e-canary.yml` get a credentials
step; the new sweeper workflow), `make test-e2e-oci-dryrun`, the testing docs' lane section; the line
`oci` is **not** added to `test/e2e-vendors.txt`.

## Gates

Per step, in this order, all green: `hack/dev.sh make generate manifests docs-api chart-crds` leaves no
diff (from step (c); step (a) adds the Helm value, so `make test-chart` from (a));
`hack/dev.sh make lint` (includes `hack/check-docs.py`); `hack/dev.sh make test` (L1 and L2; no network);
`hack/dev.sh make test-chart` (from (a)); coverage of `pkg/vendors/oci/...` **85 % or more** (from (b);
report the number); the conformance suite over the full OCI type list of the embedded catalog (from
(b)); `hack/dev.sh make test-kwok` still green; `make test-e2e-oci-dryrun` and the lane selftest, `actionlint`
and `shellcheck` clean (from (d)); `python3 hack/check-docs.py`. Set the README status to `review` when (d)
is green. The first live run is the coordinator's.

## Dependencies

- **14** (done): the contract items this plan uses. **23** (done): `BillingStart`.
- **13 part A** (on main): the snapshot of the refresh's data. **13 part B** (branch `plan13-part-b`, being
  implemented elsewhere; read-only for this plan): the snapshot-reading `KeeperSource`, the `MuxSource`
  change, `model.Data.StoragePrices`/`IPv4Prices`, `PriceSink`/`KeeperPriceConsumer`. Step (a) is
  written against the shapes it fixes (the `catalog.Source` seam exists on main today and survives in
  B) and lands after it.
- **24** (draft): **not a dependency** (decision 1); the later grid option would replace the fixed list
  per NodeClass and keep the name format.
- **22**: not needed (Flatcar's official image); an optional later job there is an import helper or a
  Community Application.
- **Outside the repository**: the operator's IaC for images, subscriptions, identity and quotas
  (section 3 and 4); Flatcar's OCI image work and node test (the other session's checklist feeds spike S6
  and S7); the crawler's OCI data in Keeper (the swap of section 6) and its `api_reference` and
  extended-memory flags (open questions 2 and 3).

## Open questions for the maintainers

1. **One compartment per controller in v1?** Recommendation: yes (flag, Helm value; the NodeClass field
   must match). Multiple compartments multiply clients, quota views and garbage collection.
2. **Keeper's `api_reference` for sized rows.** The crawler stores the shape for every size, so the
   catalog needs rule R (section 6). Recommendation: keep rule R (it also fixes the silent drop for
   any vendor); optionally make the crawler's `api_reference` of a sized row equal to its server ID, which
   makes the rule a no-op for OCI.
3. **Extended-memory sizes.** The data has them without a flag; the driver rejects them at launch
   (`ExtendedMemoryUnsupported`). Recommendation: ask the crawler to mark them (an `extended_memory`
   key in `api_reference_object`) and drop them in the catalog; no other change.
4. **No `OfferingLister` for OCI.** Types not offered in a tenancy or AD are found by quota (limit 0) and
   by `Unsupported`. A real list needs either the catalog's names inside the driver or a predicate
   capability (`TypeLaunchable(zone, type) bool`). Recommendation: v1 as written; add the predicate
   only if the first live runs show wasted attempts.
5. **Idempotency lookup scale.** One `ListInstances` per create attempt (client-side tag filter). For a
   compartment with thousands of instances use the Resource Search service (eventually consistent) or one
   cached list with the in-flight ledger. Recommendation: as written; revisit at 500 instances.
6. **Per-AD quota in the contract.** `Consumption(type, capacityType)` has no zone; OCI limits are per AD.
   The v1 answer is the best AD's headroom per region (safe, can retry an exhausted AD). Recommendation:
   an optional zone-aware consumption and a zone on `QuotaExceeded` (a core change with a conformance
   case), decided after the first live runs.
7. **Tenancy-wide usage.** If `GetResourceAvailability` on the compartment sees only its own usage, the
   buckets over-report headroom when other compartments share the limit. Recommendation: query with the
   tenancy OCID when the policy allows it (`inspect` on the tenancy), else document the limit of the view.
8. **Image sharing beyond the user's tenancy.** A Community Application per region and release
   (rung 2) would remove the per-tenancy import. Recommendation: not in v1; plan 22 may take it.
9. **Bare metal in the catalog.** 16 `BM.*` rows are in the data; Flatcar's CI does not test them; they
   boot slowly and bill a one-hour minimum. Recommendation: keep them in the catalog (the driver launches
   them as plain shapes), exclude them from the lane, and mark them unverified in the reference.
10. **NAT gateway for `publicIPv4: false` on a managed subnet** and **IPv6 on a managed VCN.**
    Recommendation: post-v1 (offerings unavailable with a reason, ADR-017); brought subnets cover both.

### Decided by default, maintainer to confirm

1. The tag keys are `scarpenter-<key>` (the restrictive mapping), the discovery tag `scarpenter-use`.
2. `areLegacyImdsEndpointsDisabled` false until spike S6.
3. Default root volume 64 GiB `balanced`; `storage.data` and local storage rejected in v1.
4. `VM.Standard2.*`, `VM.Standard.E2.*`, `VM.Standard1.*` and bare metal stay in the catalog as fixed shapes;
   what OCI refuses becomes `Unsupported`.
5. `compartmentId` required at the controller; every managed resource in it.
6. Fault domains unset; no capacity reservations, no placement.
7. Preemptible = `spot`, `TERMINATE` with the boot volume deleted.
8. The embedded vendors flag defaults to `oci`.

## Unverified facts (settled by the spike or the first live run)

- The exact HTTP status, code and message of "Out of host capacity" and of each error row of section 7
  (S5), including the token conflict and the subnet-full code.
- That capacity and limit errors are synchronous on the launch call, not on the work request (S5).
- The replay semantics and retention of `opc-retry-token` for a failed launch (S4).
- Whether `GetResourceAvailability` on a compartment shows the tenancy's usage, and the unit of every
  quota name form (S3); whether preemptible capacity has its own limit (S9).
- The IMDS endpoint version Ignition uses and that disabling the legacy endpoints is safe (S6); the
  encoding of `user_data` Ignition expects (base64 assumed).
- The arm64 launch options and capability schema with `A1`, and the same for `A2`, `A4` (S7).
- Whether `ListImageShapeCompatibilityEntries` is a sufficient architecture signal for images without
  the `arch` tag (S7).
- The freeform tag key charset, including `/` and `.` (S8); that no tag reaches the boot volume.
- That `maxPerNumaNode` and `maxPerNumaNodeInGBs` mark exactly the extended-memory sizes (S2).
- That the grid sizes of the sample data are inside every AD's bounds and that Keeper's `A1` row
  `VM.Standard.A1.Flex.80-320` (80 OCPUs) is launchable (plan 24 lists 76 as the maximum) (S2).
- The policy verbs per call and that the `where` conditions hold (S12).
- The token exchange parameters of the keyless modes (S11).
- Whether a failed launch can leave a boot volume or a VNIC behind (the lane's teardown).
- That the prices are the same in every region and AD (plan 24 §1.2: the public price API has no
  region field; the sample data repeats them) and that the free allowance of `A1` is per tenancy (not
  modelled).
- The spot price of the old fixed VMs in the sample data (`VM.Standard2.*`) and whether OCI accepts them
  as preemptible.
