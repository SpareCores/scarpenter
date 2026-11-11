# 21 — Alibaba Cloud driver (`alicloud`)

Depends on: 14 (the shared groundwork: the `images` kind, the Butane hook, the stopped-node
rule), 22 (the images repository that builds the Flatcar image for Alibaba), 06 (node options
and resources), 08 (the AWS driver is the model), 10 (the L5 lane is the model). Read
`docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §5.8, §5.9, §7, §8, §9, §11 (as the AWS model); 00a §6, §9, §11;
ADR-002, ADR-009, ADR-013, ADR-017, ADR-018, ADR-020, ADR-021 (the four rungs below).
Status of this document: research done on 2026-11-10 from public documentation
and public repositories only; no cloud call was made. Every fact that could not be
verified is marked **unverified**, and step 0 below is the spike that closes them.

## Goal

`pkg/vendors/alicloud`: a driver for ECS (Alibaba Cloud's VMs) complete against an
in-memory ECS/VPC fake and the conformance suite, with the `alicloud` block of
`spec.vendors`, quota-aware placement, vendor resources, the reference docs and
RAM policy, plus the opt-in L5 lane and sweeper. The nodes boot Flatcar from an
image that the user pre-creates (rung 3, decided for v1), built from the artifact of the
images repository (plan 22); Spare Cores publishing community images (rung 2) is a later move.

## 1. Where the vendor sits on the image ladder

**Rung 3 for v1** (decided, ADR-021: the user's own image in each region, created by IaC
from the artifact of the images repository, plan 22), **rung 2 later** (Spare Cores
publishes the images as ECS community images, if its account is eligible). Rung 4 is not
needed. Confidence: the negative findings for rung 1 are high; that rung 2 is *allowed* for
Spare Cores is medium-low (the rules disagree, see below), which is why it is not the v1
path; the technical route is medium (it rests on documentation and code reading, not on a
boot).

### Rung 1 does not apply (confidence: high)

- Flatcar publishes no image for Alibaba Cloud. The release server lists, per
  channel and architecture, images for ami, azure, akamai, brightbox, cloudsigma,
  cloudstack, digitalocean, exoscale, gce, hetzner, hyperv, kubevirt, nutanix,
  openstack, oraclecloud, proxmoxve, qemu, scaleway, stackit, vmware, virtualbox,
  xen and a generic `flatcar_production_image.bin.bz2`; nothing with `aliyun` or
  `alibaba` (checked on 2026-11-10 at
  `https://stable.release.flatcar-linux.net/amd64-usr/current/` and
  `.../arm64-usr/current/`, stable 4757.2.3).
- The Flatcar build scripts have no `oem-aliyun` package (OEM packages:
  akamai, ami, azure, cloudsigma, cloudstack, digitalocean, exoscale, gce,
  hetzner, hyperv, kubevirt, nutanix, openstack, oraclecloud, proxmoxve, qemu,
  scaleway, stackit, vagrant, virtualbox, vmware; coreos-overlay `coreos-base/oem-*`
  in `https://github.com/flatcar/scripts`).
- Flatcar's cloud provider docs have no Alibaba page
  (`https://www.flatcar.org/docs/latest/installing/cloud/` lists AWS, Azure,
  GCE, DigitalOcean, Hetzner, OpenStack, Brightbox, Akamai, STACKIT).
- No Flatcar image was found in the Alibaba Cloud Marketplace or the community
  image catalog by web search (weak evidence: the catalog itself is only visible
  in a console). The search is repeated in step 0.

### Rung 2 is the later move, with open rules (confidence: medium-low)

ECS **community images**: a custom image can be published with
`ModifyImageSharePermission(IsPublic=true)`; it is then usable by every Alibaba
Cloud account **in the image's region**
(`https://help.aliyun.com/en/ecs/developer-reference/api-ecs-2014-05-26-modifyimagesharepermission`,
`https://help.aliyun.com/en/ecs/user-guide/overview-12`). Facts and conflicts:

- **Regional.** One image per region; a community image cannot be shared, exported
  or copied. So the pipeline imports once, copies the *private* image to each
  region with `CopyImage`, and publishes each copy.
- **Not every region allows it**: the API has the error
  `CurrentRegion.NotSupportPublicImage`. Which regions: unverified.
- **Who may publish is contradictory.** The user guide says only identity-verified
  *enterprise* users may publish and must file a ticket first (in US (Virginia) and US
  (Silicon Valley) an enterprise user may sign the agreements instead); the API reference
  lists only "sign the Community Image Terms of Service"
  (`PublicImageAgreement.NotSigned`). Whether Spare Cores' account qualifies is
  unverified and is the first thing step 0 settles. Alibaba gives no support for
  community images; the publisher owns quality and updates.
- **Not encrypted images** (our images are not).
- A deprecated (delisted) image stops being public; existing sharing relations stay.
- **Sites.** Cross-site sharing between the China site and the international site
  is refused (`InvalidImageId.BidMismatch`); whether a community image crosses
  sites is unverified. an operator's regions may be international plus a few China
  regions; China regions may need a real-name-verified China-site account anyway.
- **Finding them** (`DescribeImages`): community images are `ImageOwnerAlias=others`
  with `IsPublic=true`; `ImageOwnerId` (the publisher's account UID) narrows the
  query; `ImageName` is a fuzzy search, so the driver post-filters to the exact
  name. Image IDs start with `m-` and are regional.
- **Fallback inside rung 2/3: private sharing.** `ModifyImageSharePermission` with
  `AddAccount` shares a custom image with up to **50 accounts** per image (10 per
  call), same region, same site. Shared images are found the same way (`others`,
  `ImageOwnerId`) and cannot be copied across regions by the receiver. This serves
  Spare Cores' own accounts and a few partners, not the public.

### Rung 3 works today (confidence: high that it is possible)

Custom images are created in the user's account, per region, from an OSS object
with `ImportImage`, or copied across regions with `CopyImage` (own images only).
By default the operator's IaC or a CI job creates them (some IaC tools have an image import resource;
`ImageCopy` and `ImageSharePermission` resources: unverified) and passes IDs.

### How the image gets into an account

| Topic | Fact | Source |
|---|---|---|
| Formats | RAW, VHD, QCOW2 (VMDK: invitational preview). `Format` may be left empty to auto-detect. Compressed `.bz2` is not accepted (the pages do not mention compression); the pipeline decompresses. | ImportImage API and `.../user-guide/import-a-custom-image` |
| Size | Each disk image 1 to 2048 GiB; the system disk must be at least the image file size (the OSS object size). No numeric file-size cap was found. | same |
| Where from | An OSS object **in the same region** as the import; first use needs the RAM role `AliyunECSImageImportDefaultRole` (ECS reads OSS with it). `ImportImage` returns `ImageId` and `TaskId`; `CancelTask` cancels. | same |
| Time | "Depends on file size and concurrent tasks"; no figure. Unverified, measured in step 0. | `.../import-a-custom-image` |
| Cost | OSS storage and requests for the uploaded object (delete it after the import) plus **snapshot storage** for every image, equal to the image file size, per region, per month. No prices checked: unverified; order of magnitude is cents per image and month. | same |
| Regional or global | Regional. `CopyImage` copies your own image to another region (max 5 concurrent copies per region, source `Available`, `ClientToken` supported). "VHD-format images are not supported; use RAW" for copies, so import RAW (or verify QCOW2 copies in step 0). | CopyImage API |
| Sharing / public | Share with up to 50 accounts; publish as a community image (above). | ModifyImageSharePermission API |
| Arch | `Architecture` `x86_64` or `arm64`; arm64 is UEFI-only and the RTC must be UTC. `BootMode` BIOS (default for x86_64) or UEFI. | ImportImage API |
| NVMe | `Features.NvmeSupport=supported` is required to use NVMe instance types (8th generation and later, e.g. c8i, r9i); default `unsupported`. | ImportImage API |
| Metadata | `Features.ImdsSupport` `v1` (default) or `v2` (hardened only). Must stay `v1`: see section 5. | ImportImage API |
| Check | `DetectionStrategy=Standard` runs the image check (cloud-init, virtio, ...). Flatcar has no cloud-init, so the findings are expected to be advisory: unverified. Platform `Others Linux`; the console path says "Customized Linux" for images without cloud-init. | ImportImage API, `.../import-an-image` |
| Idempotency | `ClientToken` (64 ASCII), `DryRun`, up to 20 tags. | ImportImage API |
| Quota | `QuotaExceed.Image`: the custom image count per account and region is a Quota Center item (number not documented in the pages read: unverified). | ImportImage API, `.../limitations` |

### Flatcar and Ignition on Alibaba Cloud (confidence: medium-high)

- **Ignition supports the platform.** Ignition has an `aliyun` provider that GETs
  `http://100.100.100.200/latest/user-data` and parses it
  (`https://github.com/coreos/ignition/blob/main/internal/providers/aliyun/aliyun.go`,
  registered in `internal/register/providers.go`). Flatcar's `ignition-setup-pre`
  turns the kernel argument `flatcar.oem.id=<id>` into `PLATFORM_ID=<id>`
  (`https://github.com/flatcar/bootengine`, `dracut/53ignition/ignition-setup-pre.sh`),
  and bootengine's `flatcar-metadata-hostname.service` already lists
  `flatcar.oem.id=aliyun` (hostname from the metadata through afterburn).
  Flatcar 4757 ships Ignition 2.24 (overview Q3; the main ebuild is 2.27);
  the provider is unchanged since 2019.
- **The kernel command line must carry `flatcar.oem.id=aliyun`.** The generic image
  has no OEM id, and `ignition-setup-pre` then falls back to `qemu` on a KVM guest
  (ECS is KVM), which reads `fw_cfg`, never the ECS user-data. The id comes from the
  GRUB variable `oem_id`, set in `grub.cfg` on the OEM partition (label `OEM`,
  `https://github.com/flatcar/scripts/blob/main/build_library/grub.cfg`: it sources
  `($oem)/grub.cfg` and appends `flatcar.oem.id=$oem_id`). So the image is
  `flatcar_production_image.bin.bz2` with one file written into its OEM partition:
  `set oem_id="aliyun"`. The main `grub.cfg` already defaults the console to
  `console=ttyS0,115200n8 console=tty0` (x86) or `ttyAMA0` (arm64) when the OEM file sets no
  `linux_console`; whether ECS shows that serial console: unverified. Flatcar ships OEM
  software as an OEM sysext on that partition; how the initrd and `ensure-sysext` treat an OEM
  id with no sysext and no `oem-release`: unverified (step 0). Do **not** reuse the `openstack` image: its OEM id would make
  Ignition look for OpenStack metadata that ECS does not have.
- **Image files** (all channels): amd64 `flatcar_production_image.bin.bz2`
  (587 MB for stable 4757.2.3; about 9 GiB raw, to be measured), arm64 the same name
  under `arm64-usr`. Verification: the `.DIGESTS` files next to them (as the qemu
  driver does). Channels: stable, lts, beta, alpha (the generic image exists for
  all, checked for stable only).
- **Arches**: amd64 and arm64 (arm64 families include Yitian c8y, g8y, r8y and c6r, g6r,
  c8m; 85 arm64 types in the committed Keeper snapshot).
- **User-data size.** `RunInstances.UserData` is Base64 and at most 32 KB before
  encoding (RunInstances API, checked 2026-11-10); other Alibaba pages still say 16 KB (Auto Scaling
  pages). SCarpenter's 16 KiB budget (overview §8.2) fits both, so no indirect
  Ignition `replace` from a URL is needed. The driver refuses larger data as AWS
  does (`UserDataTooLarge`).
- **Boot unknowns** for step 0: BIOS or UEFI for x86 (the generic image has both a
  BIOS boot partition and an ESP; ECS x86 defaults to BIOS), DHCP on the primary ENI
  with Flatcar's default networkd unit, serial/VNC console, the system disk growing
  at first boot, NVMe vs virtio disks, ECS reading `Customized Linux` images without
  cloud-init.

### Mapping a pinned Flatcar version to an image

Name `Flatcar-<channel>-<version>-<arch>` with `<arch>` `amd64` or `arm64`
(allowed characters in `ImageName`: letters, digits, `.:_-`, 2 to 128, must not
start with `aliyun` or `acs:`), tags `flatcar-channel`, `flatcar-version`, `arch`,
`built-by`. `ResolveImage` filters `DescribeImages` by owner (below), name
(fuzzy server-side, exact client-side), `Architecture`, `Status=Available`;
exactly one match, else `ImageNotFound` or `ImageAmbiguous`. The root disk size
comes from `DiskDeviceMappings` of the image; `BootMode` and `Features.NvmeSupport`
are read from it and reported (a type that needs NVMe with an image that lacks it is
`Unsupported` for that image).

## 2. Hand-off: `spec.vendors.alicloud`

Proposed block (field names follow the vendor's API, ADR-020; nothing here is
cross-vendor except `networking`, `tags`, `regions`):

```yaml
vendors:
  alicloud:
    regions: [ap-southeast-1, eu-central-1]    # API region IDs; empty = every region the account can use
    imageOwner: ""                              # account UID of the images' owner; "" = this account (v1); Spare Cores' account for community images later (rung 2)
    storage:                                    # defaults: root cloud_essd PL0, 64 GiB, not encrypted
      root: {category: cloud_essd, performanceLevel: PL0, size: 64Gi, encrypted: false, kmsKeyID: ""}
      data: [{name: d, category: cloud_auto, size: 100Gi, provisionedIops: 5000, burstingEnabled: false,
              filesystem: xfs, mountPath: /var/lib/d}]
    internetMaxBandwidthOut: 100                # Mbit/s; 0 = no public IPv4 (same as networking.publicIPv4: false)
    internetChargeType: PayByTraffic            # or PayByBandwidth
    metadataOptions: {httpTokens: optional}     # see section 5
    resourceGroupId: ""                         # optional enterprise resource group of created resources
    resources:
      images:                                   # rung 3 (plan 14, A1); optional: the default is the name convention
        use:
          ids: {ap-southeast-1: [m-aaa, m-bbb]}     # per region, both architectures as listed; or
          # name: "my-flatcar-{channel}-{version}-{arch}"
          # tags: {flatcar-version: "{version}"}, match: {architecture: [amd64], owner: [self]}, select: newest
      subnets:        {use: {ids: {ap-southeast-1: [vsw-1, vsw-2]}}}     # vSwitches (+ their VPC)
      securityGroups: {use: {ids: {ap-southeast-1: [sg-1]}}}
      deploymentSets: {create: {lifecycle: {policy: perUse}}}
```

Profiles: storage (`root`, `data[]` as above), network (`internetMaxBandwidthOut`,
`internetChargeType`), placement (`deploymentSet: {strategy: LowLatency|Availability}`
or `{use: {name}}`).

### Resource kinds

The unit is the region; subnets are zonal (vSwitch per zone, so `{zone}` patterns
and `ResourceMissingInZone` apply as on AWS). Zone IDs are the zone names
(`cn-hangzhou-h`, `ap-southeast-1a`; Keeper's `zone_id` equals its `api_reference`,
checked in the committed snapshot): there is no per-account zone shuffling to
map (account independence of zone IDs: unverified, though nothing says otherwise).

| Kind | `use` forms (what ECS/VPC can filter on) | `create` | Without `use` (ADR-022) | What your IaC must output |
|---|---|---|---|---|
| `subnets` | `ids` per region (`VSwitchId`); `name` (`VSwitchName`, exact or fuzzy and finished client-side, `{zone}` expanded); `tags` (`Tag.N` on `DescribeVSwitches`: up to 20 tags, exact); `match` (`zone` = `ZoneId`, `network` = `VpcId`, `owner`, `cidr`/`within` client-side); `filters` `IsDefault`/`EnableIpv6`/`RouteTableId` | VPC (`10.240.0.0/16`) + one vSwitch per zone (a /20 each), lifecycle `idleTTL` 24 h. **No internet gateway or route table is needed**: ECS gives a public IPv4 through `InternetMaxBandwidthOut > 0`, and the VPC has a default route table | the account's **default VPC** (`DescribeVpcs IsDefault=true`) and its default vSwitch per zone (`IsDefault=true`), used as found, never modified; where there is no default VPC, `create`; discovery by the opt-in tag comes first (planned) | the vSwitch IDs per region, or the tag `scarpenter.sparecores.com/use=true` on vSwitches and the VPC |
| `securityGroups` | `ids` (`SecurityGroupId`); `name`; `tags` (`DescribeSecurityGroups` has `Tag.N`; name matching: unverified, finished client-side); `match.network` (`VpcId`: the group must be in the vSwitches' VPC); `filters` `SecurityGroupType`/`ServiceManaged` | one **normal** group per (NodeClass, VPC), `idleTTL` 24 h: inbound only `spec.networking.inbound`, a self-referencing rule (`SourceGroupId`) for `isolation: nodeclass`, outbound is open by default; one more per run key for `isolation: run` (`perUse`, internal kind `runSecurityGroups`) | tagged groups (discovery, planned); none exists by default: `create` | the group ID per region, or the opt-in tag on it (only a group nodes may use) |
| `deploymentSets` | `use: {name}` or `{ids}` via a placement profile | per (region, profile, run key), `perUse` | none (per-run kinds have no default) | nothing |
| `images` (not a `ManagedResources` kind) | `ids` per region (a list), `name` (`ImageName` is fuzzy: finished client-side), `tags` (`Tag.N` on `DescribeImages`), `match` (`architecture`, `owner`: `self`/`system`/`marketplace`/an account UID, `state`, `createdAfter` with minute precision), `filters` (`ImageFamily`, `OSType`, `IsPublic`, ...); newest by `CreationTime` client-side | never (ADR-021; rung 4 would) | `imageOwner` + the name convention | the image IDs per region, or images named by the convention |

A vSwitch must be in the same VPC as the group (`InvalidSecurityGroup.VpcMismatch`,
`OperationDenied.InconsistentNetwork`): a brought group in another VPC skips the zone.
No SSH key, volume, EIP, NAT gateway or RAM instance role is needed to launch
(`KeyPairName` and `RamRoleName` are optional; nodes get Ignition-installed keys and no
cloud credentials). Data disks are part of `RunInstances` (`DataDisk.N`,
`DeleteWithInstance=true`), so there is no volume resource.

### What the operator's IaC must provide, and what SCarpenter self-manages

Provided by the operator (any tool, the CLI or the console), all but the images optional:

1. **Image IDs per region and architecture** (rung 3), passed as `resources.images.use.ids`
   or found by the naming convention: the operator's IaC or a CI job imports or copies them
   (an OSS bucket per region for `ImportImage`, the RAM role `AliyunECSImageImportDefaultRole`)
   from the images repository's artifact. Some IaC tools have an image import resource
   (Pulumi's `alicloud.ecs.ImageImport`); `ImageCopy` and `ImageSharePermission` resources:
   unverified.
2. **A VPC, a vSwitch per zone and a security group** (`subnets.use`, `securityGroups.use`
   by ID, `VSwitchName` or `name` pattern with `{zone}` as the zone ID, or the tag
   `scarpenter.sparecores.com/use=true` on the vSwitches, the VPC and the group for
   discovery, planned). A group must be in the vSwitches' VPC. A group for nodes should allow
   all traffic from itself, UDP 41641 (Tailscale) and ICMP; outbound open; hand over only a
   group nodes may use. The `direct` template works inside the VPC; `tailscale` needs only
   outbound.
3. **The controller's identity**, documented as a requirement: a RAM OIDC identity provider
   for the cluster's service account issuer, a RAM role with the policy of this plan, and
   its ARNs (`ALIBABA_CLOUD_ROLE_ARN`, `ALIBABA_CLOUD_OIDC_PROVIDER_ARN`).
4. **vSwitch size**: a /25 leaves 124 usable addresses per zone (Alibaba reserves 4 of
   every vSwitch; unverified count): at most about 120 nodes per zone, then
   `QuotaExceeded.PrivateIpAddress` (a `ZoneUnusable` for the driver). Size the vSwitches
   to the expected nodes; document it.
5. `account_id` is not needed (images use `imageOwner: self`). An optional resource group
   (the account's default group is used if unset).
6. IPv6: a VPC without an IPv6 CIDR makes `publicIPv6` unsupported (section 4).

SCarpenter self-manages, without `use`: tagged resources (discovery); else the account's
default VPC and its default vSwitches where they exist and suit the request; else a managed
VPC (`10.240.0.0/16`), a vSwitch per zone and a group per NodeClass; deployment sets always;
else a stated failure. Images are never self-managed.

### Account scope and credentials

One credential covers all regions of one account (and one site: international or
China; unverified whether a site can be mixed). The driver uses
`github.com/aliyun/credentials-go`'s default chain: `ALIBABA_CLOUD_ACCESS_KEY_ID`/
`_SECRET` (and `_SECURITY_TOKEN`), then the RAM role of an OIDC identity provider
(`ALIBABA_CLOUD_OIDC_PROVIDER_ARN`, `ALIBABA_CLOUD_ROLE_ARN`,
`ALIBABA_CLOUD_OIDC_TOKEN_FILE`: the projected service account token, the analogue
of AWS web identity), the shared credentials file, the ECS instance RAM role and a
credentials URI. Recommended: the OIDC role (no static keys), with the chart's
`vendors.alicloud.credentials.mode` like AWS (`webIdentity`, `secret`, `none`).
Verified from the SDK README (`https://github.com/aliyun/credentials-go`); the
self-managed-cluster OIDC provider setup is unverified (step 0, as plan 10 did for AWS).
`--alicloud-home-region` (default `ap-southeast-1`) is where the global
`DescribeRegions` is read.

## 3. Driver

### Client layer

- **SDK**: the official Darabonba-generated Go SDKs, Apache-2.0, released
  continuously: `github.com/alibabacloud-go/ecs-20140526/v7` (v7.11.7, 2026-10),
  `github.com/alibabacloud-go/vpc-20160428/v7` (v7.3.1), config from
  `github.com/alibabacloud-go/darabonba-openapi/v2`, credentials from
  `github.com/aliyun/credentials-go` (v1.4.14), and optionally
  `github.com/alibabacloud-go/quotas-20200510/v2` (Quota Center). Not
  `aliyun/alibaba-cloud-sdk-go`: archived in February 2026. Maturity: the packages are
  machine-generated and large; the typed requests use pointer fields and a
  `tea.SDKError` carries `Code`/`StatusCode`/`RequestId` (classification reads the
  code). A thin test of the pinned versions ships in step 1.
- Narrow interfaces `ecsAPI`, `vpcAPI`, `quotasAPI` hold only the calls used:
  `DescribeRegions`, `DescribeZones`, `DescribeInstanceTypes`,
  `DescribeAvailableResource`, `DescribeAccountAttributes`, `DescribeImages`,
  `RunInstances`, `DescribeInstances`, `DeleteInstance`, `DescribeSecurityGroups`,
  `CreateSecurityGroup`, `AuthorizeSecurityGroup`, `RevokeSecurityGroup`,
  `DeleteSecurityGroup`, `DescribeSecurityGroupAttribute`, `CreateDeploymentSet`,
  `DescribeDeploymentSets`, `DeleteDeploymentSet`, `TagResources`/`ListTagResources`,
  `DescribeVpcs`, `CreateVpc`, `DeleteVpc`, `DescribeVSwitches`, `CreateVSwitch`,
  `DeleteVSwitch`, and (quota spike) `ListProductQuotas`. Clients per region and
  service, created lazily (`ecs.<region>.aliyuncs.com`, `vpc.<region>.aliyuncs.com`;
  the endpoint pattern on the China site: unverified).
- Retries: the SDK has per-call `RuntimeOptions` retry settings but no adaptive mode:
  the driver wraps calls in its own bounded backoff on `Throttling*`, `ServiceUnavailable`,
  `InternalError` (the codes are in section 6), with a per-call deadline (45 s,
  90 s for `RunInstances`, as AWS) returning `Retryable` (`CallTimeout`). The ECS API
  rate limit cannot be raised (`.../user-guide/limitations`); the numbers are
  unpublished in the pages read: unverified, so the driver also limits its own
  full-region `DescribeInstances` and `DescribeAvailableResource` loops.
- User agent: `scarpenter/<version>` through the config's `UserAgent`.

### NodeClaim to instance

| Item | Value |
|---|---|
| `RunInstances` | `Amount=1`, `MinAmount=1`, `RegionId`, `ZoneId`, `ImageId`, `InstanceType` (the type's `api_reference`, `ecs.g8y.large`), `VSwitchId`, `SecurityGroupIds.N`, `InstanceChargeType=PostPaid`, `InstanceName=<nodeclaim>`, `HostName=<nodeclaim>`, `UserData` (Base64 Ignition), `SystemDisk.*`, `DataDisk.N.*` (all `DeleteWithInstance=true`), `InternetMaxBandwidthOut`, `InternetChargeType`, `SecurityOptions` metadata options, `DeploymentSetId`, `Tag.N`, `ClientToken`, `ResourceGroupId` if set; spot: `SpotStrategy=SpotAsPriceGo`, `SpotDuration=0` (no protection period), `SpotInterruptionBehavior=Terminate`. No launch templates. |
| Provider ID, ref | `scarpenter://alicloud/<region>/<nodeclaim>`; `InstanceRef` `<region>/i-xxxx` |
| Tags | `Tag.N` on the instance, its disks and the primary ENI (one parameter does all three): the logical tags through `TagKey` = `scarpenter.sparecores.com/<key>` (slash keys are valid, per Alibaba's tag rules: no `aliyun`/`acs:` prefix, no `http://`) and the NodeClass `spec.tags`. At most **20 tags**, 128 characters each (`QuotaExceed.Tags`, `NumberExceed.Tags`: `Fatal` configuration errors, not quota): five logical tags leave 15, so validation limits `spec.tags` to 15 (unverified that the ENI and disks count the same way) |
| Idempotency | `ClientToken` (64 ASCII) is documented: same token and parameters returns the same instance; same token with other parameters is `IdempotentParameterMismatch`, after which the idempotence page says to change the token; it says to retry (same token) only on `InternalError`, `ServiceUnavailable` or no response. There is **no `client-token` filter** in `DescribeInstances`. So: token = hex SHA-256 of (idempotency key, region, zone, type, capacity type, image, vSwitch); before each launch `FindInstance` by the nodeclaim tag in the region and return that instance if it exists (the contract's "replay with another offering returns the original"); on `IdempotentParameterMismatch` resolve through the same lookup, and if nothing is found retry once with a token salted by an attempt counter (the contract's "a failed create leaves the key unused"). Whether a failed call consumes its token: unverified (step 0) |
| Async | `RunInstances` returns IDs while the instance is `Pending`; a stock shortage after acceptance appears as a `SystemFailure.Delete` event (`DescribeInstanceHistoryEvents`; documented on the RunInstances page) and the instance is released: the driver treats a created instance that disappears within the launch timeout as `InsufficientCapacity` (how fast, and whether `GetInstance` sees it first: unverified, step 0) |
| States | `Pending`, `Starting` are `pending`; `Running` is `running`; `Stopping`, `Stopped` are `terminating` (the stopped-node rule of plan 14, A4: they exist and their disks cost, so the garbage collector deletes them); a released instance is gone: `GetInstance` returns `NotFound` (the conformance suite allows it after delete); whether `DescribeInstances` ever returns released instances: unverified |
| Addresses | `VpcAttributes.PrivateIpAddress`, `PublicIpAddress`, `EipAddress`, `NetworkInterfaces[].PrimaryIpAddress`, `Ipv6Sets`; the serving certificate approver checks them (overview §8.5) |
| List / find | `DescribeInstances` with `Tag.N` (AND semantics: unverified), `NextToken`/`MaxResults` 100, per region; `InstanceIds` filter up to 100 |
| Delete | read the instance, refuse one without this cluster's tag (as AWS), `DeleteInstance(Force=true)`; `InvalidInstanceId.NotFound` is success; disks and the primary ENI are released with it (`DeleteWithInstance`, `DeleteOnRelease` default true) |
| Limits | one instance per call; per-region RAM and quota limits in section 6; names 2 to 128 characters (hostnames 2 to 64: a long NodeClaim name is shortened with a hash in `HostName` only) |

### Regions and zones

- `Regions`: `DescribeRegions` from the home region (and `RegionUnauthorized` /
  `RealNameAuthenticationError`-style failures per region: `RegionUnavailable`,
  skipped with one warning, as AWS). The Keeper catalog has regions the account never
  sees (`cn-wuhan-lr`, `cn-hangzhou-acdr-ut-3` are in the snapshot): the driver
  intersects Keeper's active regions with what the account can use.
- `Zones`: `DescribeZones` per region; `Zone{ID: <zone id>, Name: <zone id>}`.
  A vSwitch-less zone is unusable for launch, not for listing.
- `OfferingLister`: `DescribeAvailableResource` per region (`DestinationResource=
  InstanceType`, `ResourceType=instance`, `InstanceChargeType=PostPaid`, and
  `SpotStrategy` for spot) returns per zone the types with `Status`
  (`Available`/`SoldOut`) and `StatusCategory` (`WithStock`, `ClosedWithStock`,
  `WithoutStock`): `LaunchableTypes` returns `Available` ones, refreshed every 5 minutes
  (stock changes faster than AWS offerings; ADR-013 treats capacity as reactive, so this
  is an extra signal: open question 8).

### Billing period (ADR-024, plan 23)

Per-second billing settled hourly; a minimum of 10, 5 or 2 minutes by vCPU is charged when the billed time is below it, so a short-lived instance pays it (the pay-as-you-go page, read 2026-11-10;
whether "the cycle" is the instance's life or the last hourly settlement is **unverified**, and only the first fits the rule below). Increment 1 s is under `minPeriod`, so no increment is held, but Alibaba is a
**minimum-only retention candidate** (ADR-024, plan 23): a node is held to `start + minimum - margin`, reused meanwhile and
deleted at once, because after the minimum every second costs. With the default 3-minute margin that is 7 minutes for a 10-minute
minimum, 2 for 5, and none for 2 minutes (`MinimumTooShort`); the driver's `BillingRule` override gives the minimum by vCPU
(plan 13). Gated on a verified minimum and billing start; until then retention is off. `BillingStart` is `CreationTime`
(early against the billed start, the safe side; unverified).

Live check for the cycle question: create one instance at hh:50 and release it at hh:57, create another at hh:57 and release it at
hh:04, then compare the hourly consumption details of both. If the minimum applies per instance life, each pays one minimum; if per
hourly record, the second pays a minimum in each of its two hours. SCarpenter deletes only before `start + minimum`, so no new paid
period starts either way; the exposure under the per-cycle reading is at most one minimum extra when the start falls within one minimum of
a clock hour.

### Catalog

Keeper already has the vendor (committed snapshot of 2026-11-05: 34 regions, 111 zones,
1998 servers of which 971 `inactive` and 85 arm64, 385 families). Needed from the driver:

- Drop what Flatcar on ECS cannot or should not run, from the launchable set (the
  catalog keeps them, as AWS drops `mac*`): bare-metal `ecs.ebm*` and `ecs.scc*`
  (Super Computing Cluster; Flatcar on them: unverified), types the account cannot
  buy in a VPC with an I/O-optimized disk (classic-network-only legacy families such
  as `ecs.c1`, `ecs.s1`..: unverified list), and the test families `ecs.poc-test`,
  `ecs.e-test`, `ecs.vfx*`, `ecs.hbm*`. A test asserts every family of the snapshot is
  classified (launchable, or in the explicit drop table); a new family fails it until
  classified.
- Region/zone names are the Keeper `api_reference` (`ap-southeast-1`,
  `ap-southeast-1a`, `cn-hangzhou-h`).
- Spot prices are Keeper's (ADR-011); Keeper has no `cloud_auto` price and per-PL
  ESSD prices were not checked (`docs/explanation/vendor-capabilities.md`): price
  incomplete where missing.

## 4. Node options and profiles (ADR-020, ADR-017)

Built-in defaults (layer 1): root disk **`cloud_essd` PL0, 64 GiB, not encrypted**;
public IPv4 on with `internetMaxBandwidthOut` 100 and `PayByTraffic`; `HttpTokens` `optional`.
Decisions that differ from AWS, each an open question:

- **Encryption default false.** The RunInstances page says an encrypted system disk
  "cannot be used in Zone D of Hong Kong or Zone A of Singapore"; default true would
  take `ap-southeast-1a` out. `encrypted: true` is allowed; `Supported` then excludes
  those zones (ADR-017: explicit, not silent).
- **Bandwidth.** sc-runner's 5 Mbit/s default caps benchmarks
  (`vendor-capabilities.md`); 100 is the ceiling for pay-by-traffic instances
  (`.../user-guide/limitations`: 100 Mbit/s generally 200).

| Block | Fields | Validation |
|---|---|---|
| `storage.root`, `storage.data[]` | `category` (`cloud_essd`, `cloud_auto`, `cloud_essd_entry`, `cloud_efficiency`, `cloud_ssd`), `size`, `performanceLevel` (PL0 to PL3, ESSD only; default PL1 in the API, PL0 here), `provisionedIops` and `burstingEnabled` (`cloud_auto` only; range 0 to min(50000, 1000 x size - baseline), baseline min(1800 + 50 x size, 50000)), `encrypted`, `kmsKeyID`; data also `name`, `filesystem`, `mountPath` | sizes per category and level; Linux system disk 20 to 2048 GiB and at least the image size; `cloud_essd_entry` only for `ecs.u1` and `ecs.e` |
| placement profile | `deploymentSet: {strategy: LowLatency}` (cluster) or `Availability` (spread), or `{use: {name}}`; partition (`AvailabilityGroup`, `groupCount` 1 to 7, `DeploymentSetGroupNo` per instance) deferred | needs a run key; strategy limits: `QuotaExceed.DeploymentSetInstanceQuotaFull` is a per-offering `InsufficientCapacity` (code `PlacementGroupFull`); the 20-instance limits are from `vendor-capabilities.md` (unverified) |
| network profile | `internetMaxBandwidthOut` 0 to 100, `internetChargeType` | `PayByBandwidth` makes the price incomplete |
| `metadataOptions`, `resourceGroupId`, `imageOwner` | as the block above | |

- **`Supported`**: per instance type and zone from `DescribeInstanceTypes` (disk
  categories supported, NVMe, ENI limits, `DiskQuantity`) and `DescribeAvailableResource`
  (`DestinationResource=SystemDisk`/`DataDisk`: which categories each zone sells);
  whether one call per region returns the (type, zone, category) cube or one call per type
  is needed is unverified: if the latter, only a family table is precomputed and a
  `CategoryNotSupported`/`InvalidDiskCategory.NotSupported` launch error marks the offering
  `Unsupported` (24 h). Types with local disks (`i`, `d` families) are **not** supported
  for local-storage profiles in v1 (deferred: ADR-017 makes them infeasible, not degraded).
  Placement profiles are supported only where the strategy accepts the family (table,
  unverified). `publicIPv6: true` is refused by the NodeClass validator (and by `ZoneGaps`
  for a brought vSwitch without an IPv6 CIDR): IPv6 on ECS needs an IPv6 gateway the
  driver does not manage (deferred).
- **`PriceDelta`**: disks at Keeper's GB-month storage price per region and category
  divided by 730 (price incomplete when Keeper has no row: `cloud_auto`, per-level ESSD,
  any `provisionedIops` or bursting); the public IP is free, traffic is billed per GB and
  is not priced (not priced for any vendor); `PayByBandwidth` is incomplete.
- **Device hints** (`Prepare`): disks exist at launch (part of `RunInstances`); the
  guest sees virtio-blk (older types) or NVMe (8th generation and later) devices. The
  serial of a virtio disk is the disk ID without the `d-` prefix (`/dev/disk/by-id/virtio-<id>`)
  per `vendor-capabilities.md` (marked unverified there too); the NVMe by-id pattern is
  unverified. The hint is a serial pattern plus size, resolved in step 0, with the first-boot
  unit's size match and lock as the fallback.
- **Attach timing**: nothing attaches after launch, as on AWS.

## 5. Security notes and the IMDS gap

- Ignition's `aliyun` provider reads the user-data with a plain GET, with no metadata
  token. ECS "hardened mode" (`HttpTokens=required`, IMDSv2) stops the plain mode
  (`403`), so **`HttpTokens` must stay `optional` at boot**, and the image must have
  `ImdsSupport=v1`. The AWS driver's IMDSv2 with hop limit 1 (overview §8.6) therefore
  has no equivalent: `HttpPutResponseHopLimit` is "not available" on the RunInstances
  page while its error table says 1 to 64 (unverified). Pods without host networking could
  read the user-data (bootstrap token, TTL 1 h, deleted at registration; tailscale key,
  ephemeral and tagged) and the metadata (no RAM role attached, so no credentials).
- Options, in preference order: (a) a vendor-supplied Butane fragment in the base config
  that drops forwarded traffic to `100.100.100.200` (the generic hook of plan 14, A7, since the
  core must not branch on the vendor; decided: this one, off by default); (b) `ModifyInstanceMetadataOptions(HttpEndpoint=disabled)`
  by a later reconcile once the node is registered (the kubelet needs no metadata; hostname
  is set by Ignition); (c) document the gap. Open question 4.
- A `nodes` group as described in section 2 allows all traffic from itself, UDP 41641 (Tailscale)
  and ICMP; outbound is open. The `direct` template works inside the VPC; `tailscale` needs
  only outbound.

## 6. Quotas, stock and errors (ADR-013)

**Quota source.** Two candidates, to be decided by step 0 against a real account:

1. `DescribeAccountAttributes(RegionId, AttributeName=max-postpaid-instance-vcpu-count,
   used-postpaid-instance-vcpu-count, max-spot-instance-vcpu-count,
   used-spot-instance-vcpu-count)`: limit and usage in one call, per region
   (`https://www.alibabacloud.com/help/en/ecs/developer-reference/api-ecs-2014-05-26-describeaccountattributes`,
   verified as documented, not as accurate). Buckets `postpaid-vcpu` and `spot-vcpu`,
   scope region, unit vCPU; every type consumes its vCPUs (Keeper `vcpus`) from the bucket
   of its capacity type.
2. Quota Center (`ListProductQuotas`, `ProductCode=ecs`, `Dimensions regionId`): vCPU quotas
   "by instance type" per region and billing method, with `TotalQuota` and `TotalUsage`
   (`https://help.aliyun.com/en/ecs/user-guide/quota-management`,
   `.../api-quotas-2020-05-10-listproductquotas`). It may carry per-family limits
   (`q_ecs_restrict_postpay_c` and `q_ecs_restrict_spot_c` were seen in search results for
   restricted-purchase families); the codes and defaults are unverified.

The plan implements (1) and adds family buckets from (2) only if step 0 shows the account
attributes are not the binding limit. `Used` covers the whole account; the instance IDs
counted for the in-flight ledger come from a region-wide `DescribeInstances` every 2
minutes (states `Pending`, `Starting`, `Running`, and conservatively `Stopping`/`Stopped`;
whether stopped instances count against the vCPU quota: unverified). Limits refresh every
15 minutes; `QuotaExceed.ElasticQuota` and the other codes below trigger an immediate
refresh. A new account's defaults: unknown (the one number seen, 100 vCPU, was on an
unrelated page).

**Stock.** `OfferingLister` above; a sold-out (type, zone) is not offered.
`OperationDenied.NoStock` and `Zone.NotOnSale` at launch are `InsufficientCapacity`.

**Spot.** `SpotAsPriceGo` (pay the market price, capped at the on-demand price), no
protection period (`SpotDuration=0`), `Terminate` on interruption; its own vCPU bucket.
`InvalidAccount.NotSupportSpot` (business rules) is `Unsupported` for the capacity type of
the region. Interruption handling stays out of scope (ADR-011).

**Error mapping** (by `Code`, never by text; `V` = seen in the RunInstances error table,
`U` = unverified, from general knowledge of the API):

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `OperationDenied.NoStock` V, `Zone.NotOnSale` V, `InvalidOperation.PublicIpAddressNoStock` V, `QuotaExceed.DeploymentSetInstanceQuotaFull` V (as `PlacementGroupFull`), `LackResource` V |
| `Unsupported` | `InvalidInstanceType.ZoneNotSupported` V, `InstanceType.Offline` V, `InvalidInstanceType.ValueNotSupported` V, `ImageNotSupportInstanceType` V, `InvalidDiskCategory.NotSupported` V, `CategoryNotSupported` V, `InvalidInstanceType.NotSupportDiskCategory` V, `OperationDenied.CloudSSDNotSupported` V, `OperationDenied.ZoneSystemCategoryNotMatch` V, `InvalidAccount.NotSupportSpot` V |
| `QuotaExceeded` | `QuotaExceed.ElasticQuota` V, `QuotaExceed.AfterpayInstance` V, `QuotaExceeded` (instances in the VPC) V, `QuotaExceed.PostPaidDisk` V, `QuotaExceeded.PostpaidDataDiskCapacity` V, `QuotaExceeded.InternetBandwidth` V, `InstanceDiskNumLimitExceed`, `InstanceDiskLimitExceeded` V |
| `ZoneUnusable` | `QuotaExceeded.PrivateIpAddress` V (vSwitch full), `OperationDenied.ZoneNotAllowed` V, `Zone.NotOpen` V, `SecurityGroupInstanceLimitExceed` V (the group is full: skip the zone rather than fail the NodeClaim) |
| `RegionUnavailable` (from `Zones`) | `RegionUnauthorized` V, `InvalidRegion.NotSupport` U, real-name verification failures U |
| `Retryable` | `Throttling.Resource` V (429), `Throttling`, `Throttling.User`, `Throttling.Api` U, `ServiceUnavailable`, `InternalError`, `IdempotentProcessing` U, `IncorrectInstanceStatus` U on delete |
| `NotFound` | `InvalidInstanceId.NotFound` U (get, delete) |
| `Fatal` | everything else, for example `QuotaExceed.Tags` and `NumberExceed.Tags` V (a configuration error: more than 20 tags, not a quota), `InvalidImageId.NotFound` V, `OperationDenied.ImageNotValid` V, `InvalidVSwitchId.NotFound` V, `InvalidSecurityGroup.VpcMismatch` V, `OperationDenied.InconsistentNetwork` V, `InvalidUserData.Forbidden` V, `Account.Arrearage` V, `InsufficientBalance.AgentCredit` V, `Forbidden.RAM` U |

`IdempotentParameterMismatch` is resolved as in the idempotency row, never classified.
Every code has a test; the table is cross-checked against a second independent copy
(plan 08's approach). sc-inspector's markers for Alibaba (in sc-runner's and
sc-inspector's code) are the second source to read at implementation.

## 7. Scope (what the plan implements)

- **Driver** (`pkg/vendors/alicloud`): `Driver`, `NodeClassValidator` (the `alicloud` block
  and its profile blocks: region IDs, resource references `vsw-`, `sg-`, `m-`, `ds-` per
  region, tag keys and counts, ranges above, `HttpTokens` values, `publicIPv6` refusal),
  `ImageReporter` (`status.vendors.alicloud.images`, `ImagesReady`, same partial-region rule
  as AWS), `NodeOptions` with `EpochReporter`, `ManagedResources` (kinds `subnets`,
  `securityGroups`, `runSecurityGroups`, `deploymentSets`), `QuotaReporter`,
  `OfferingLister`, `ZoneChecker`. Spec block types in `pkg/apis/v1alpha1/alicloud` plus
  one line in each of `VendorBlocks`, `StorageProfile`, `NetworkProfile`, `PlacementProfile`
  and the accessors of `vendors.go`.
- **Core extension: none here.** The `images` kind reaching `ResolveImage` is plan 14 (A1,
  decided; open question 9). `imageOwner` defaults to the credentials' own account in v1; a
  driver constant for Spare Cores' account UID is added with rung 2 (its value is not in this
  repository until the publishing account exists).
- **Resources** as the table in section 2: `Ensure` creates with the ownership marker
  (`cluster`, `nodeclass`, `managed-by`, `resource-kind`) and deterministic names
  `scarpenter-<cluster>-<kind>-<region>[-<hash>]` (names 2 to 128 characters; deployment
  set names allow letters, digits, `:_-`). Marker atomicity: `CreateSecurityGroup` and
  `CreateDeploymentSet` take tags or are tagged by a following `TagResources`; whether
  `CreateVpc`/`CreateVSwitch` take `Tag.N` is unverified, otherwise `Ensure` tags straight
  after creation and the sweeper treats name prefix plus age as the fallback (a resource
  with the name but no marker is never deleted by the GC). Delete order: deployment sets,
  run groups, NodeClass groups, vSwitches, VPC. `InUse` asks `DescribeInstances` by VPC,
  vSwitch, group or deployment set (`DeploymentSetId` filter: unverified); `Delete` of a
  resource in use is `Retryable` (`DependencyViolation.*`: codes unverified).
- **In-memory fake** (`pkg/vendors/alicloud/fake`): regions, zones, images (owners `self`,
  `others` with `IsPublic`, system), instances with `Pending` to `Running` and released
  instances that vanish, `ClientToken` semantics including `IdempotentParameterMismatch`,
  tags with AND filters, paging, VPCs (default or none), vSwitches with free-address counts,
  groups, deployment sets with strategy and size limits, account attributes, optional Quota
  Center items, `DescribeAvailableResource` stock states, instances of other owners for
  account-wide usage, spot, and error injection per call. Used by the conformance suite
  (run per resource kind over every launchable type of the snapshot) and by the driver's
  own table tests (a `RunInstances` input golden with every field asserted).
- **Docs**: `docs/reference/alicloud-driver.md` (regions, zones, images and the pipeline's
  contract, defaults, profiles, resources, network, tags, quotas, errors, the IMDS gap,
  deferred, not verified), `docs/reference/alicloud-ram-policy.json` (the `ecs:`/`vpc:` actions of the client layer's calls, mutations conditioned on the cluster tag as in the AWS policy; the CI policy of section 9 is a narrower variant; validated
  as JSON in `make lint`), `docs/how-to-guides/alicloud-credentials.md` (OIDC provider and
  role for the controller, RAM user keys), the vendor line in the vendor table.
- **Deferred**: IPv6, local-storage profiles, partition placement, dedicated hosts and
  capacity reservations (`PrivatePoolOptions`), multiple ENIs, eRDMA/`HighPerformance` traffic
  mode, EIPs and NAT gateways, RAM instance roles, interruption handling, `ebm` and `scc` types,
  `DryRun` pre-flights.

## 8. The image pipeline (contract; built by the images repository, plan 22)

The build (steps 1 and 2) is done by the Spare Cores images repository of plan 22 (proposed
`SpareCores/flatcar-images`), which publishes the artifact for `aliyun` (amd64 and arm64,
`flatcar-aliyun-<channel>-<version>-<arch>.qcow2` and `.img.gz`) with checksums and
signatures. In v1 (rung 3) steps 3 to 5 are the user's IaC or a CI job;
steps 6 to 8 are the later rung 2, done by the repository in a Spare Cores account. Per
Flatcar release and channel (stable and lts to start), per architecture:

1. Download `flatcar_production_image.bin.bz2` from the release server, verify the SHA512
   from `.DIGESTS` (the qemu driver's rule), decompress.
2. Mount the OEM partition of the raw image (loop device, partition label `OEM`), write
   `grub.cfg` with `set oem_id="aliyun"` (and the console arguments), unmount; no other
   change (Flatcar updates itself off: the base config masks `update-engine`).
3. Upload the RAW (or a compressed QCOW2, step 0 decides) to OSS in a home region; the
   import region equals the bucket's region.
4. `ImportImage` with `Architecture`, `BootMode` (x86_64 BIOS or UEFI as step 0 shows, arm64
   UEFI), `Platform=Others Linux`, `Features.NvmeSupport=supported`, `Features.ImdsSupport=v1`,
   `DetectionStrategy=Standard`, name and tags of section 1, `ClientToken`; wait for
   `Available`.
5. `CopyImage` to every target region (the operator's regions list), same name and tags.
6. Rung 2 (later): `ModifyImageSharePermission(IsPublic=true)` per region; rung 2 fallback:
   `AddAccount`. Rung 3 consumers skip this.
7. Verify from a second account: `DescribeImages` finds each image (`others`, `IsPublic`),
   and boot one instance per architecture (this is also the L5 `image` scenario).
8. Retire old versions: delete or deprecate images beyond the last three per channel; the
   image quota and snapshot storage are per region (cost grows with regions x arches x
   channels x versions).

SCarpenter's side is only `ResolveImage`/`ReportImages` by owner and name. An upstream
Flatcar `oem-aliyun` package (an official image, rung 1 later) is the better long-term
answer; the pipeline's OEM-partition edit is what such a package would produce.

## 9. Testing: L5 lane, canary, sweeper

Policy: `docs/how-to-guides/testing-and-releasing.md` (tiers, per-vendor cost guards, "Adding
a vendor lane"). Nothing here runs in this plan's gates; the coordinator triggers the first
runs, as for AWS.

- **Lane**: `test/e2e-alicloud/` (the scripts and `defaults.sh` the reusable workflow calls:
  `bootstrap.sh`, `mint-authkey.sh`, `install.sh`, `scenarios.sh`, `diagnostics.sh`,
  `teardown.sh`, `lib.sh`, `selftest.sh`, manifests), a line `alicloud` in
  `test/e2e-vendors.txt`, a credentials step for `inputs.vendor == 'alicloud'` in `e2e-cloud.yml`
  and `e2e-canary.yml`, `.github/workflows/e2e-alicloud-sweeper.yml`, `test/canary/alicloud.go`
  behind a read-only guard. Control plane (k3s on the runner), tailnet join, key minting and
  the chart install are the AWS lane's. The cloud calls use the Alibaba Cloud CLI (`aliyun`)
  for the byo scenario and diagnostics.
- **CI identity**: a RAM role assumed through a RAM OIDC identity provider for GitHub
  Actions (subject of the `e2e` environment), the usual pattern (a role of the CI account, `ALIBABA_CLOUD_ROLE_ARN`, `ALIBABA_CLOUD_OIDC_PROVIDER_ARN`,
  `ALIBABA_CLOUD_OIDC_TOKEN_FILE`; a GitHub OIDC token written to a file by a composite
  action); role ARNs live in the environment's secrets, never in the repository. A
  separate read-only role for the canary. The policy (RAM policy JSON, written by
  `ci-policy.sh`): `ecs:RunInstances` allowed with `acs:RequestTag/scarpenter.sparecores.com/cluster`
  like `scarpenter-ci-*` and denied for `ecs:InstanceType` outside the small test types
  (the keys `ecs:InstanceType`, `ecs:ImageOwnerId`, `acs:RequestTag` and `acs:ResourceTag` are
  documented for RAM; their exact use with `RunInstances` is unverified), resources limited to
  the lane's region by the ARN (`acs:ecs:<region>:*:...`), mutations of security groups, vSwitches,
  VPCs and deployment sets by resource tag, read-only `Describe*`, `ecs:DescribeAccountAttributes`,
  `vpc:Describe*`, `quotas:ListProductQuotas`/`GetProductQuota`, `ecs:TagResources`.
- **Cost guards** (all required before the vendor is in the list): a budget alert on the
  account; the type deny above; the region limit; run tag
  `scarpenter.sparecores.com/cluster=scarpenter-ci-<run id>` and `ci-created-at` on everything;
  the sweeper; the same 90 min job and 55 min scenario limits; spot where possible; a teardown
  that fails the job on leftovers; the account's vCPU quota as ceiling (pay-as-you-go 16 vCPU,
  spot 4 vCPU needed: Alibaba accounts need real-name verification and a balance, and spot can
  be refused by account rules: step 0); an approval gate via the `e2e` environment.
- **Test types** (from the committed snapshot; per-zone availability unverified): x86 on-demand
  `ecs.e-c1m2.large` (2 vCPU, 4 GiB, shared), arm64 `ecs.c8y.large` (2 vCPU, 4 GiB, Yitian) or
  `ecs.c8y.small`, spot for one of them, `ecs.g7.large`-class for the placement scenario if
  `LowLatency` needs a family the small types do not support.
- **Scenarios** (AWS lane, mapped; "S" = the numbering of plan 10):
  `preflight` (image found, quotas, NodeClasses Ready), `amd64` (S1: node Ready, Pod, `logs`/
  `exec`, labels incl. zone ID, instance tags and metadata options checked with `aliyun ecs`;
  plus **the Ignition check**: `PLATFORM_ID=aliyun` and the aliyun provider fetched the user-data),
  `arm64` and spot (S2), `scalezero` (S3: within 2 minutes of Pod deletion, tokens gone),
  `drift` (S4: change `storage.root.size`), `quota` (S4a: `DescribeAccountAttributes` equals the
  metrics; usage follows launch and release; launched type in `DescribeAvailableResource`),
  `db` (S4b: LowLatency deployment set with two nodes in one zone and a `cloud_essd` PL1
  data disk, mounted at the profile path; the set deleted afterwards), `described` (S4c),
  `byo` (S4d: workflow-created tagged VPC, vSwitch, group; `use` by tag; untouched afterwards),
  `negative`, `image` (rung 3: ids and name; rung 2, owner + name, when it exists), `imds` (what a Pod can reach at
  `100.100.100.200`: records the gap of section 5), `uninstall`, and a final no-leftover check
  (instances, disks, groups, vSwitches, VPCs, deployment sets tagged with the run).
- **Sweeper**: every 3 hours and on dispatch, environment `e2e-sweeper`, deletes by tag
  `scarpenter.sparecores.com/cluster` like `scarpenter-ci-*`, older than 2 hours (instances by
  `CreationTime`, others by `ci-created-at`), in the lane's region: instances, deployment sets,
  groups, vSwitches, VPCs (never default VPCs or groups; never an unmarked resource), driven
  through the driver's narrow API so the fake runs it. Lives in `pkg/vendors/alicloud/sweep`.
- **Canary** (nightly, read-only, no instance): regions and zones (`facts.Load`), the pinned
  image resolves in the lane region for both arches (owner and name), `DescribeAccountAttributes`
  and the quota mapping of every lane type, `DescribeAvailableResource` for the lane types.
- Tier of the lane: Alibaba bills pay-as-you-go instances per second on most types (unverified), so
  the smoke and full sizes of AWS are expected to apply; the cost per run is measured at the
  first run and reported.

## 10. Step 0: the spike (before any implementation; maintainers with an account)

Settle, and record in `docs/reference/alicloud-driver.md`:

1. Does Spare Cores' account qualify to publish community images; in which regions; is a
   community image usable from another account and from the other site.
2. A hand-built Flatcar image (generic image plus the OEM `grub.cfg`) boots on ECS for amd64 and
   arm64: Ignition reads the user-data (`PLATFORM_ID=aliyun`), hostname, DHCP, console, BIOS or
   UEFI, NVMe and virtio types, system disk growth; `Customized Linux` and the image check
   findings.
3. RAW versus QCOW2: import time, snapshot size, and whether `CopyImage` accepts each.
4. `HttpTokens=required` breaks the user-data read as expected; `HttpPutResponseHopLimit`.
5. `ClientToken`: failed call then retry with the same token; mismatch; whether released
   instances are listed; tag filter semantics and consistency right after launch.
6. Quota: `DescribeAccountAttributes` against Quota Center for a new account; family-level limits;
   whether stopped instances count; default values; custom image quota.
7. Disk serial / by-id patterns for virtio and NVMe; `DescribeAvailableResource` for
   `SystemDisk` per region.
8. Deployment sets: strategy by family, limits, spot compatibility, the `DeploymentSetId` filter.
9. Whether `CreateVpc`, `CreateVSwitch`, `CreateSecurityGroup` take tags at creation; the
   `Dependency*` delete codes.
10. API rate limits of `RunInstances`, `DescribeInstances`, `DescribeAvailableResource`.
11. The self-managed-cluster OIDC provider and RAM role path for the controller (RRSA-style
    from a non-ACK cluster).

## Dependencies

- ADR-021 (the image ladder) and plan 11's add-a-vendor guide (reuse its checklist).
- Plan 14 (the groundwork) and plan 22 (the images repository); the rung 3 images are made
  from its artifact by the operator's IaC (the points of section 2) and plan 14's part B contract.
- No `SCImage`/`ImageImporter` plan (rung 4 is not used; ADR-021).
- Keeper: nothing new for v1 (region, zone, server and storage data exist); a `cloud_auto`
  and per-level ESSD price would complete prices.

## Open questions for the maintainers

1. **Publishing rights.** Is Spare Cores' account (or a new one) an identity-verified
   enterprise account allowed to publish community images, and in which regions? **Partly
   decided (ADR-021):** v1 starts with rung 3 (own images, or shared ones up to 50 accounts) and the
   driver supports `imageOwner` from day one; rung 2 comes only if the account is eligible. The
   eligibility question stays open: file the ticket now.
2. **Pipeline scope.** **Decided (ADR-021, plan 22):** the repository is the Spare Cores
   images repository (proposed `SpareCores/flatcar-images`); channels and versions are plan
   22's (stable and lts automatically, others on request). Which account publishes community
   images later, and in which regions, stays with question 1.
3. **OEM id.** **Decided:** bake `flatcar.oem.id=aliyun` into the OEM partition ourselves (plan
   22) and open an upstream Flatcar issue for an official `aliyun` image.
4. **IMDS hardening.** **Decided (plan 14, A7):** (a), a generic vendor Butane fragment hook
   with an nftables drop of `100.100.100.200` for pods, off by default, documented.
5. **Default root encryption** `false` (ECS default; Singapore zone A and Hong Kong zone D refuse
   it) versus `true` as on AWS. Recommend `false`.
6. **Default bandwidth**: 100 Mbit/s `PayByTraffic`, or lower. Recommend 100.
7. **Quota source**: account attributes first, Quota Center only if step 0 shows family limits
   bind. Recommend as written.
8. **Stock as launchable filter**: treat `DescribeAvailableResource` sold-out as not offered
   (5 minute refresh), or keep stock reactive only (ADR-013 line). Recommend using it, and report
   the effect in L5.
9. **Explicit image IDs in the core**: **Decided (plan 14, A1):** the generic `images` kind
   with `ids` per region (and the rest of the selector, `UseRef` of plan 14 A10) reaches `ResolveImage` through
   `ImageRequest`, for every vendor.
10. **Raw versus compressed QCOW2** for the import, decided by step 0 (copy support and time).
11. **Deferred set**: IPv6, local storage, partition placement, bare-metal families. Recommend
    deferring all four.
12. **China regions**: expose them (needs a China-site, real-name-verified account) or limit
    the default `regions` to international ones. Recommend international only in the docs and
    the lane, no code branch.

## Gates

CONVENTIONS gates 1 to 4 (L3 stays green; no core change in this plan, the `images` kind and
`ImageRequest` are plan 14's; the vendor line in `vendors.go`). `go test ./pkg/vendors/alicloud/...`
coverage at least 80 % of statements (report the number), the conformance suite per resource
kind over the full launchable type list of the snapshot, the family-classification test, the
`RunInstances` golden, every error code of section 6 tested. The L5 lane and sweeper are
written and statically validated (`actionlint`, `shellcheck`, the dry-run target), not run.
Step 0's results are a precondition; the plan's code starts only after items 1 to 5 are
recorded.
