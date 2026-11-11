# 18 — OVHcloud Public Cloud driver (`ovh`)

Depends on: 14 (the shared groundwork: the `images` kind, `RootDiskForType`, the idempotency
label, message-based classification, region aliases, the stopped-node rule), 06 (driver
contract, resources), 08 (the model driver), 09 (chart, credentials how-to), 10 (the L5
lane pattern). Read `docs/plans/v1/CONVENTIONS.md` first. Spec: overview §7, §8, §9, §11 (as the model), 00a §6, §9, §11; ADR-002,
ADR-017, ADR-018, ADR-020, ADR-021 (the image ladder). Research date: 2026-11-10. Everything marked *unverified* must be
settled by the spikes of "Spikes" before the driver code depends on it.

## Goal

`pkg/vendors/ovh`: a driver for OVHcloud Public Cloud, complete against an in-memory
OpenStack fake (Keystone, Nova, Neutron, Glance) and the conformance suite, plus the
reference docs, the credentials how-to and the e2e lane. OVH Public Cloud is OpenStack
(one Nova, Neutron, Glance and Cinder per region, under one project), so the driver is
an OpenStack driver with OVH's identity, region and flavor rules. It would also be the
base of any later generic `openstack` driver; v1 stays OVH-only.

No OVH calls in the plan's coding phase (CONVENTIONS rule 8); the spikes and L5 are
run by the coordinator.

## Findings

### Image rung: 3 (confidence: medium-high; one spike could lift it to 2)

| Rung | Result |
|---|---|
| 1. Public Flatcar image offered by OVH | **No.** OVH's public image lifecycle page lists AlmaLinux, CloudLinux, Debian, Fedora, FreeBSD, Windows Server, Rocky and Ubuntu, and no Flatcar or CoreOS ([image life cycle](https://docs.ovhcloud.com/en/guides/public-cloud/compute/image-life-cycle); the page was read only in part, so the check is `openstack image list --public \| grep -i flatcar` in a spike). Flatcar's own docs list OVHcloud as a *community-supported* platform where you import your own image ([Flatcar on OVHcloud](https://www.flatcar.org/docs/latest/deploy/cloud/ovhcloud/)). |
| 2. Anyone may publish a public image | **Not documented, probably not.** OVH documents only `shared` visibility with explicit members, inside one region, member must accept ([sharing images](https://docs.ovhcloud.com/en/guides/public-cloud/compute/share-images-between-projects.md)); no `public`. Upstream Glance makes `publicize_image` admin-only by default. **`community` visibility is untested** (upstream: owner may set it by default, and any project can boot a community image by ID). If OVH allows it, Spare Cores can publish community images from one project in every region and the driver finds them by owner: that is rung 2. Spike S1. Sharing a private image to named projects is not a substitute: the member must accept per region, and the instances of a member break if the owner deletes the image or the project ([same page](https://docs.ovhcloud.com/en/guides/public-cloud/compute/share-images-between-projects.md)). |
| 3. Images in the user's own account, per region | **Yes.** Glance accepts raw and qcow2 uploads per region ([upload your own image](https://docs.ovhcloud.com/en/guides/public-cloud/compute/upload-own-image.md)). Images are regional. The Flatcar docs show exactly this procedure. |
| 4. SCarpenter imports | Not needed. Later, an `ImageImporter` for OVH is a small step on top (Glance upload or `web-download`); reserved by ADR-021. |

**Decided (ADR-021): v1 is rung 3**, from Flatcar's official OpenStack image: the
operator's IaC, a CI job or the three-line `openstack image create` of the how-to creates
one image per region and architecture, and passes the IDs to SCarpenter. S1 stays as the
check for a **later move** to "Spare Cores publishes community images" (rung 2), which would
make `images` below optional for most users. Nothing in the driver changes except the
lookup's visibility filter, which is why it is in the design from the start
(`includeCommunityImages`).

### Flatcar on this platform (verified unless marked)

- Flatcar publishes `flatcar_production_openstack_image.img` (qcow2, uncompressed), `.img.gz`,
  `.img.bz2` and a `_mini_image` variant, with `.DIGESTS`, for **amd64 and arm64**, on
  every channel (stable, beta, alpha, lts), under
  `https://<channel>.release.flatcar-linux.net/<arch>-usr/<version>/` (checked 2026-11-10:
  stable 4757.2.3, amd64 594,542,592 bytes, arm64 491,847,680 bytes; qcow2 v2 header,
  virtual size 12.3 GiB for both). The docs page for OpenStack shows only amd64
  ([Flatcar on OpenStack](https://www.flatcar.org/docs/latest/deploy/virt-options/openstack/)).
- The OVHcloud docs page uploads that file with `--container-format bare --disk-format qcow2
  --property image_original_user=core --property hw_qemu_guest_agent=yes`, boots it from the
  portal or `openstack server create --user-data ignition.json`, and says Ignition is passed as
  user data ([Flatcar on OVHcloud](https://www.flatcar.org/docs/latest/deploy/cloud/ovhcloud/)).
  That page is one example per channel (beta), and community-supported: boot is **unverified
  by us** until S2.
- Ignition has the platform `openstack`: "read its configuration from the instance userdata
  via either metadata service or config drive"
  ([Ignition platforms](https://coreos.github.io/ignition/supported-platforms/)); it has no
  OVH-specific ID. The openstack image sets it itself (OEM ID `openstack`), so
  `ignition.platform.id` needs no change. The Flatcar OpenStack page says the cloud must
  support config drive or the metadata service; with a config drive,
  `coreos-metadata.service` (SSH keys, `/run/metadata/flatcar`) is unsupported, which we do
  not use (Ignition writes the keys, the hostname and the provider ID).
- User-data size: Ignition imposes none. Nova's API accepts up to 65,535 bytes of base64
  user data upstream; OVH's limit is **unverified** (S2). Our budget is 16 KiB (overview §8.2).
- arm64: the image exists, but Keeper's OVH servers are all `x86_64` (135 in the live API, 104
  in the committed snapshot of 2026-11-05), and none of the OVH docs read mention arm flavors. v1 is **amd64 only**; the driver reports no arm64
  types because the catalog has none. (arm64 would also need `hw_firmware_type=uefi`: unverified.)
- Root disk: the image's virtual size is 12.3 GiB; the smallest flavors have 25 GB (`d2-2`) and 50 GB
  disks, so every x86 flavor of the catalog fits. Flatcar grows its root partition to the
  disk at first boot.

### How an image gets in, what it costs, who can see it

| Question | Answer |
|---|---|
| Formats | RAW or QCOW2; OVH recommends QCOW2 and says to keep images small ([upload your own image](https://docs.ovhcloud.com/en/guides/public-cloud/compute/upload-own-image.md)). Glance stores the bytes as given, so a `.gz` or `.bz2` file would be an unbootable image: decompress first (Flatcar's own docs say `bunzip2`; upstream Glance behaviour, not tested on OVH). |
| Import from URL | OVH's page says nothing. Glance `web-download` is the upstream default import method, **unverified on Public Cloud** (a different OVH product, the SecNumCloud platform, says it does not work). Plan: direct upload with `openstack image create --file` (the documented method). Spike S3. |
| Size limit, time | No limit stated. The file is ~0.6 GB; direct upload is minutes. Unverified. |
| Cost | Images are billed as snapshots at a flat per-GB-month rate regardless of image type ([billing](https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/analyze-billing)); the figure was not found: unverified. For a 0.6 GB image in every region it is cents per month. |
| Scope | Regional: one Glance per region, nothing is copied across. Every region in scope needs its own upload. |
| Sharing | `shared` to named projects only, same region, member accepts. No public (S1 tests `community`). |

### Pinned Flatcar version to image ID

Images are named `flatcar-<channel>-<version>-<arch>` (`flatcar-stable-4757.2.3-amd64`) and
carry the Glance tags `flatcar` and `flatcar-<channel>-<version>` and the properties
`flatcar_channel`, `flatcar_version`, `architecture` (`x86_64`). `ResolveImage` lists the
region's images with `status=active`, the tag and the exact name, and requires **exactly
one** match (as AWS); explicit IDs per region and architecture override the lookup.
Names are free-form in Glance, so the convention is ours and is documented; the operator's
image step produces it.

## The `spec.vendors.ovh` block

OVH has no zones in most regions, no required SSH key object (Nova does not need one; the
Ignition config carries the keys), no required network (a node attaches to the public
network `Ext-Net`, which is in every region and gives a public IPv4 and IPv6 without
floating IPs) and a default security group. So the zero-configuration route works with
**only an image**. The block:

```yaml
vendors:
  ovh:
    regions: [GRA11, SBG5]             # optional allow-list; empty = every region the credentials' catalog has and Keeper prices
    includeCommunityImages: false      # also look at visibility=community (a later rung 2, S1)
    imageOwner: ""                     # project ID of the images; "" = the credentials' project (as `aws.imageOwner`)
    storage:                           # ADR-020 defaults, overridden here
      root: {bootFromVolume: {volumeType: high-speed, size: 64Gi}}   # absent = the flavor's local disk (default)
    networking: {}                     # generic overrides; publicIPv4 must stay true
    configDrive: true                  # Nova config_drive; default true (S2 may change it)
    resources:
      images:                          # rung 3; use only (plan 14, A1), SCarpenter never creates images; optional: the default is the name convention below
        use:
          ids: {GRA11: [9c1f..., 4be2...]}   # per region, both architectures as listed; OR
          # name: "flatcar-{channel}-{version}-{arch}"   # the default; with the Glance tags, default [flatcar]
          # (match: {architecture: [amd64]}, tags: {...}, filters: [{name: visibility, values: [private]}], select, orderBy: created)
      securityGroups: {use: {ids: {GRA11: [<uuid>]}}}    # or name pattern / tags; or create (default)
      serverGroups: {create: {lifecycle: {policy: perUse}}}   # placement profiles
      networks: {...}                  # optional second NIC on a private network: deferred (see below)
```

**Core change: decided, in plan 14 (A1).** The core resolved images at launch with
`ImageRequest{Region, Arch, Channel, Version, Owner}` only; `Owner` comes from the block's
`ImageOwnerGetter`, so `imageOwner` needs nothing new. The `images` kind's ids, name pattern
and the rest of the selector (`UseRef`, plan 14 A10) now reach `ResolveImage` through `ImageRequest`, for every vendor; this plan
adds no core change of its own.

`regions`: the vendor's unit is the region; its API names are the Keystone region names,
upper case. **Zones**: 1-AZ regions have one dummy zone (`gra11-a`, Keeper's name);
`eu-west-par` and `eu-south-mil` have `-a/-b/-c`. `Zones` reads Nova's availability
zones (`os-availability-zone`) and maps them to Keeper's zone IDs; for a 1-AZ region the
create call omits `availability_zone` (Nova's AZ name there is unverified, S4).

### Resource kinds

| Kind | Needed to launch? | `use` forms | `create` | What your IaC must output |
|---|---|---|---|---|
| `images` (not a managed kind) | **yes** (rung 3) | `ids` per region (a list, both architectures), `name` (exact on the server, a glob client-side; `{channel}`, `{version}`, `{arch}`, `{region}`), `tags` (Glance tag strings `k=v`), `match` (`architecture` through the image property, `owner`, `state`, `createdAfter`), `filters` (`visibility`, `member_status`, `os_hidden`, `property:<k>`, ...), `orderBy: created` on the server; `imageOwner` stays a block field | no (never) | the Glance image IDs per region, or images named by the convention |
| `securityGroups` | no (the project's `default` group is used as found, like the AWS default VPC) | `ids`, name pattern, tags (Neutron tags: only if the extension is present, S5), name | yes: one per NodeClass and region, `idleTTL` 24 h; a per-run group for `isolation: run` (`perUse`), like AWS | the Neutron UUID of the nodes group per region |
| `serverGroups` | only with a placement profile | `ids`, name | yes: per region, profile and run key, `perUse` | nothing |
| `networks` (second NIC on a vRack private network) | no | `ids` (Neutron network UUID per region), name pattern | no in v1 | the Neutron network UUID per region (later; not used in v1) |
| ports, floating IPs, gateways, SSH keys, volumes | no | n/a | n/a (volumes are part of the create call, below) | nothing |

Notes:

- **Why no private network in v1.** OVH documents that when a node has both `Ext-Net` and
  a vRack private interface, OpenStack may pick the private one as the default route and make
  the instance unreachable ([vRack and networking](https://docs.ovhcloud.com/en/guides/public-cloud/network-services/vrack)).
  Nodes join over Tailscale or over their public address, so they do not need it. A
  private-only node (`publicIPv4: false`) needs a billed Gateway for egress: **unsupported
  in v1** (ADR-017: the offering is infeasible, `publicIPv4: false` fails validation).
  A network the operator made stays unused until the later `networks` kind.
- **Security groups on public ports.** OVH's docs disagree whether the default group
  allows all inbound or only outbound; one says every port, "public and private", is open
  by default ([security group and private network](https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/security-group-private-network.md),
  [security group guide](https://docs.ovhcloud.com/en/guides/public-cloud/compute/setup-security-group)).
  Whether a group attached to the server also filters the `Ext-Net` port is therefore the
  first thing S5 checks; if it does not, "deny all inbound" cannot be met and the
  driver must say so (ADR-017) instead of launching open nodes.
- Nova accepts `security_groups` by name (and, upstream, by UUID in the name field). The
  driver resolves ID to name per region and refuses an ambiguous name (S5). The alternative
  (create ports ourselves) adds a port lifecycle per node and is rejected.
- A wide-open group meant for other workloads must never be selected; where OVH resources
  have no tags, a `tags` selector cannot protect it, so hand over IDs only.

### What the operator's IaC must provide, and what SCarpenter self-manages

Provided by the operator (any tool, the OpenStack CLI or the Manager):

1. **Images** (rung 3, mandatory): one private Glance image per region and architecture,
   from the Flatcar release file downloaded once and verified against the release `.DIGESTS`
   (a step of the operator's IaC or a CI job: an OpenStack provider's image resource with a
   project user, or the `openstack image create` of the how-to; whether an IaC tool can
   create that user and its password is unverified). Passed as `resources.images.use.ids`
   per region, or found by the naming convention.
2. **Region names.** The block's `regions` and the `ids` keys must be the **Keystone
   catalog's** names. Keeper's priced OVH regions (16 for `b3-8`, checked 2026-11-10) are
   mostly the **legacy** names: `GRA7`, `GRA9`, `GRA11`, `SBG5`, `SBG7`, `DE1`, `UK1`, `WAW1`,
   `BHS5`, `SGP1`, `SYD1`, `RBX-A`, `AP-SOUTH-MUM-1`, `AP-SOUTHEAST-SYD-2`, plus `EU-WEST-PAR`
   and `EU-SOUTH-MIL` (the only new-style names Keeper prices; both 3-AZ). The trigrams
   (`GRA`, `SBG`, `RBX`, `DE`, `UK`, `WAW`, `SGP`, `SYD`, `BHS`) exist in Keeper without
   prices. OVH's announcement gives each region a name (`eu-west-gra`) and a code (`GRA`)
   "as often called in API and UI", promises API compatibility, and gives no mapping to
   `GRA11` ([regionalisation](https://www.ovhcloud.com/en/about-us/global-infrastructure/expansion-regions-az/));
   the OVH service-account page uses `GRA` and `GRA1` as OpenStack region names. No source
   shows `EU-WEST-GRA` (or any 1-AZ region's long name) as a Keystone or OVH API region:
   unverified. The OpenStack region names a project sees are whatever its Keystone catalog
   says, so the operator's tooling should read the project's regions from the API
   (`GET /cloud/project/{serviceName}/region`) rather than from a static list, and the driver
   treats the Keystone catalog as the truth, with a small alias table (Keeper name to catalog
   name) only where the two differ (the contract allowance is plan 14, A8; the table lives in
   the driver and the hand-off uses API names). Spike S4. This is the highest risk of the plan.
3. **Neutron UUIDs.** Whatever tool created a security group or network, the value handed
   over must be the **Neutron UUID** of the region, not an OVH-side resource ID (whether a
   given IaC provider exposes the UUID is unverified; check). Needed for `securityGroups`
   now and for the deferred `networks` kind.
4. **A controller identity**, documented as a requirement: a dedicated OVH service account
   for SCarpenter (not the IaC's own) with an IAM policy on the project URN for the
   OpenStack rights it needs: `computeOperator`, `networkOperator`,
   `networkSecurityOperator`, `imageOperator` (the driver only reads images; no read-only
   image role is documented), `volumeOperator` (see
   [service accounts and OpenStack](https://docs.ovhcloud.com/en/guides/manage-and-operate/iam/authenticate-api-openstack-with-service-account),
   which lists these rights). Exact least-privilege set: S6.
5. **A security group for nodes**, optionally (by Neutron UUID per region); hand over only a
   group nodes may use, never a wide-open one meant for other workloads (OVH resources have
   no tags in some setups, so a `tags` selector cannot protect it). No `subnets` kind.
6. **The group must fit `Ext-Net` nodes.** v1 nodes have only an `Ext-Net` port, so a group
   that admits everything only from private (vRack) ranges, plus UDP 41641 and ICMP from
   anywhere, drops node-to-node and control-plane traffic over public addresses: with it only
   the `tailscale` join works, not `direct`. Either the docs say so, or the group carries a
   self-reference rule (S5 must show it works between `Ext-Net` ports).

SCarpenter self-manages, without `use`: no discovery that depends on tags unless S5 shows
Neutron tags work; the project's `default` security group where it suits the request
(skipped when `isolation` or inbound rules need what it cannot give), else a managed group
per NodeClass and region (`idleTTL`), server groups and volumes. Images are never
self-managed.

## Credentials and account scope

The controller authenticates to **OpenStack** (Keystone `https://auth.cloud.ovh.net/v3`),
scoped to one Public Cloud project (the project ID is the account scope; one controller
serves one project). Two modes, same as the AWS `credentials.mode` idea:

1. **`serviceAccount` (recommended)**: an OVH IAM service account (OAuth2 client credentials,
   the usual kind for automation). OVH documents the OpenStack side: auth type
   `v3oidcclientcredentials`, protocol `openid`, access token type `id_token`, scope
   `openid profile email publicCloudProject/all`, identity provider `ovhcloud-emea` (EU) or
   `ovhcloud-world`, discovery endpoints under `iam.ovh.net` / `iam.ovh.ca`, and one IAM action
   per OpenStack role (`publicCloudProject:openstack:computeOperator`, ...) on the project URN.
   **gophercloud has no OIDC auth plugin** (checked in v2.15.0: only federation
   management APIs). The driver implements the small flow itself: client-credentials token
   from the discovery document's token endpoint, federated Keystone auth with that
   `id_token`, then a project-scoped token (the keystoneauth plugin's steps; unverified
   against OVH, S6), wired into gophercloud through `ProviderClient.ReauthFunc`. Tokens last
   about a day (user tokens: [managing tokens](https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/managing-tokens);
   service-account token lifetime is unverified); the reauth func covers expiry.
2. **`openstackUser`**: a project's OpenStack user and password through gophercloud's
   standard environment (`OS_AUTH_URL`, `OS_USERNAME`, `OS_PASSWORD`, `OS_PROJECT_ID`,
   `OS_USER_DOMAIN_NAME`). Works with no custom code and is the fallback if S6 fails. Keystone
   application credentials are standard OpenStack; OVH does not document them: unverified.

Credentials come from a Secret (`mode: secret`) as for AWS static keys; there is no
workload identity from the cluster (OVH IAM has no documented trust of a Kubernetes
service account issuer: unverified). Flags (all `SCARPENTER_OVH_*`): `--ovh-auth-url`,
`--ovh-idp`, `--ovh-discovery-url`, `--ovh-project-id`, `--ovh-regions-home` (the region whose
endpoint reads the shared catalog). One credential covers every region (the Keystone
catalog lists per-region endpoints), as in AWS.

## OVH API or OpenStack API

**The driver uses the OpenStack APIs only** (Nova, Neutron, Glance, Cinder through
gophercloud). Reasons, from the OVH API schema (`https://eu.api.ovh.com/v1/cloud.json`,
read 2026-11-10):

- `POST /cloud/project/{serviceName}/region/{regionName}/instance` (and the older
  `.../instance`) take name, flavor, image, `sshKey`, `userData`, network, group, volume and
  billing period; they have **no metadata, no tags, no config drive control, no block
  device mapping and no idempotency key**, and return an asynchronous `cloud.Operation`.
  We need tags/metadata (find, GC, sweeper) and `config_drive`.
- The OVH API has no image import or Glance control, no Neutron security groups other than
  an IaC provider's, and its instance statuses mirror Nova's anyway.
- What only the OVH API offers: the region list of a project, per-flavor stock (`available`)
  and per-flavor quota (`quota`: "number of instances you can spawn"), and quota changes
  (`/region/{r}/quota`, `/quota/allowed`). The driver does not need them in v1: regions come
  from the Keystone catalog, quotas from Nova's absolute limits, stock from `NoValidHost`.
  A later optional `OfferingLister` could read `flavor.available` with the same service
  account (OVH API rights are a different IAM action set): an open question.

SDK: **`github.com/gophercloud/gophercloud/v2`**, v2.15.0 (2026-09-18), mature and the
SDK of most OpenStack tooling; packages used: `identity/v3/tokens`, `compute/v2/{servers,
flavors,servergroups,limits,availabilityzones}`, `blockstorage/v3/volumetypes`,
`networking/v2/{networks,subnets,extensions/security/groups,rules,extensions/attributestags}`,
`image/v2/{images,imagedata}`. No IaC code in the driver. `go-ovh` is not a
dependency.

## Driver

- **Client layer**: narrow interfaces over gophercloud (`novaAPI`, `neutronAPI`, `glanceAPI`,
  `cinderAPI`, `keystone`), per-region service clients created lazily from the catalog;
  user agent `scarpenter/<version>`; per-call timeouts; `MaxBackoffRetries` for 429, 502,
  503, 504 with jitter; list calls paginated.
- **`Regions`**: the Keystone catalog's compute regions, intersected with Keeper's active
  regions through the alias table (S4); a catalog region without a Nova endpoint, or a
  token that is not valid for it, is `RegionUnavailable`.
- **`Zones`**: see above. Keeper zone IDs for OVH are `<region>-a` (1-AZ) and
  `eu-west-par-a|b|c`, `eu-south-mil-a|b|c`.
- **`ResolveImage`**: see "Pinned Flatcar version". `Image.RootDiskBytes` is 0 (the root is the flavor's disk, see Node options); `ReportImages` implemented for the status and `ImagesReady`.
- **Flavor lookup**: `CreateRequest.InstanceType` is Keeper's `api_reference`, which is the Nova flavor **name** (`b3-8`, `c3-16-flex`); flavor IDs differ per region, so the driver caches name to ID from `GET /flavors/detail` per region (1 h). A name missing in a region is `Unsupported` for that region.
- **`CreateInstance`**: `POST /servers` with
  `name = <nodeclaim>`, `flavorRef`, `imageRef` (or a block device mapping, below),
  `networks: [{uuid: Ext-Net}]` (the `Ext-Net` network is found per region as the external network
  named `Ext-Net`, cached), `security_groups`, `user_data` (base64), `config_drive`,
  `availability_zone` (multi-AZ regions only), `metadata` (every logical tag through `TagKey`
  as `scarpenter-<key>`, since Nova metadata keys are restricted; the NodeClass's own tags
  prefixed `scarpenter-tag-`; and `scarpenter-idempotency-key` = the idempotency key, plan 14 A3), server tags
  (Nova microversion ≥ 2.26: `scarpenter-cluster=<cluster>`, shortened with a hash to 60
  characters, to filter lists server side; falls back to client-side filtering if the
  microversion is missing, S4), `os:scheduler_hints` `group=<server group>`. ProviderID
  `scarpenter://ovh/<region>/<nodeclaim>`, rendered before the instance exists as always.
- **Idempotency**: Nova has no client token. `CreateInstance` first lists servers by name
  (`name=^<nodeclaim>$`, anchored regex filter) and the tag filter and returns the one whose
  `scarpenter-idempotency-key` equals the key (whatever the flavor or zone of the replay, as the
  contract demands); only then it creates. The core serialises launches per NodeClaim, so the
  find-then-create window is the one that a crash opens, which the lookup closes. A server
  of the same name with another UID is a leftover of an earlier NodeClaim: it is deleted
  (it carries the cluster marker) and the call returns `Retryable`.
- **Waiting for the scheduler**: Nova answers `202` before a host is chosen. A capacity
  failure appears seconds later as status `ERROR` with a fault. `CreateInstance` therefore
  polls (2 s) until the task state leaves `scheduling` or the status is `ERROR`, at most
  45 s (`--ovh-schedule-wait`), so the attempt loop can try the next candidate. An `ERROR`
  server is deleted before the error is returned. A server that is still `BUILD`/`spawning` after that is
  returned as `pending`; the rest of the boot (about a minute, unverified) is the core's launch timeout.
- **Addresses**: from `addresses` of the server (`Ext-Net` list: public IPv4 and IPv6, both
  fixed); `Instance.Addresses` holds all of them for the serving-certificate check. Under
  `direct`, the node's primary address is the public IPv4.
- **`GetInstance`/`FindInstance`/`ListInstances`**: by ID, by name plus tags, by the cluster
  tag over the regions (paginated `servers/detail`, 1000 per page). `ListInstances` excludes
  `DELETED` and `SOFT_DELETED`.
- **State mapping**: `BUILD`, `REBUILD`, `RESIZE`... to `pending`; `ACTIVE` to `running`;
  `DELETING`, `SHUTOFF`, `STOPPED`, `SUSPENDED`, `SHELVED*`, `ERROR` to `terminating`
  (anything not running or booting is not a node; the stopped-node rule of plan 14, A4). OpenStack has **no shutdown behaviour
  `terminate`**: a guest `poweroff` (the `poweroffAfter` fail-safe, a crashed node) leaves
  a `SHUTOFF` server that OVH keeps billing. These servers must stay in `ListInstances`
  so that the garbage collector deletes them; plan 14 (A4) makes the garbage collector delete
  instances that stay `terminating` beyond a grace period.
- **`DeleteInstance`**: `DELETE /servers/{id}`; `404` is success; `409` (a server in a
  transient task state) is `Retryable`. Block devices created by the call are
  `delete_on_termination` so nothing is left; ports Nova created go with the server.
- **Tags**: `TagKey` maps to `scarpenter-<key>` (`cluster`, `nodeclaim`, ...).
- **Rate limits**: not documented by OVH for the OpenStack endpoints: unverified. The status
  check and GC reuse the resolve caches of the engine; a full list of every region is
  at most every 10th GC, like AWS; polling is bounded.
- **Billing**: instances are billed hourly from creation until deletion; a stopped or
  paused instance is still billed, a shelved one only for its snapshot
  ([billing](https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/analyze-billing),
  [shelve](https://docs.ovhcloud.com/en/guides/public-cloud/compute/suspend-or-pause-an-instance);
  per-minute granularity: unverified). The GC rule above is therefore a cost rule for the following hours; the started hour is paid
  either way (the billing period section below).

### Quotas (`QuotaReporter`)

OVH enforces per-project, per-region quotas on instances, cores and RAM (`GET
/cloud/project/{serviceName}/region/{regionName}/quota` has `maxInstances`, `maxCores`, `maxRam`,
`used*`), which Nova reports as absolute limits. New projects start with low
limits and OVH raises them on request or automatically
([increasing quotas](https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/increasing-public-cloud-quota)).

- Buckets per region: `instances`, `cores`, `ram` (MiB). Limits and usage from Nova `GET
  /limits` (`maxTotalInstances`, `maxTotalCores`, `maxTotalRAMSize`, `total*Used`): usage
  is the whole project, so foreign servers count (the contract's rule). Counted refs: the servers in
  the region that are not `DELETED`.
- `Consumption(type, onDemand)`: 1 instance, the flavor's vCPUs, its RAM in MiB. Spot does not exist: `false` for spot.
- GPU flavors have a separate quota (the 4-GPU default of the AI docs) and are out of v1.
- The conformance `UnknownQuotaTypes` list holds only the families the driver does not
  launch (below). A test asserts every Keeper OVH family of the snapshot is launched, excluded, or mapped.
- Stock: not exposed through Nova. A flavor with no capacity fails at the scheduler.

### Error classification

Nova and Neutron give HTTP statuses and prose, no stable codes for most failures, so this
driver is, like qemu, a documented exception to "codes, not text": the cases below are matched
on the exception names or phrases Nova uses, each with a test and each marked for S7 to confirm
on the real service. The exception becomes a contract amendment for every vendor without error
codes in plan 14 (A6: HTTP status plus message text, texts pinned by tests).

| Class | Case |
|---|---|
| `InsufficientCapacity` | server `ERROR` with fault `NoValidHost` ("No valid host was found"); an anti-affinity or affinity server group that cannot be satisfied (same fault) |
| `QuotaExceeded` | HTTP 403 with "Quota exceeded for instances, cores, ram, server_groups" (Nova `OverQuota`); Neutron 409 `OverQuota` for ports or security groups |
| `Unsupported` | 400 flavor disk too small for the image; flavor not found in the region; flavor or image with a hardware requirement the region lacks |
| `ZoneUnusable` | Neutron "no more IP addresses available" on `Ext-Net` (unverified) |
| `RegionUnavailable` | no endpoint for the region in the catalog; Keystone 401/403 for that region |
| `Retryable` | 429, 500, 502, 503, 504, connection errors, 409 conflict, token refresh in flight |
| `NotFound` | 404 on a server |
| `Fatal` | everything else: 400 on a malformed request, 401 with fresh credentials, 403 without the quota phrase, image missing or not `active` |

Spot, preemption and interruption: **none** (OVH Public Cloud has no spot and Keeper's OVH
prices are on-demand only). The `spot` capacity type is not offered.

### Billing period (ADR-024, plan 23)

- **Rule**: Keeper says 3600/3600, with a comment that EU, CA and APAC regions bill per second (60 s
  minimum) and the US hourly. OVH's billing page confirms per second with a 1-minute minimum for EU, CA and APAC (no retention
  there: the minimum is under the hold floor). For the US, the terms say hourly rounded up (their timed example, 9:50 to 10:10 is one hour,
  refutes clock hours) but the US support guide says per second: **conflicting, unverified**.
  The driver reports the rule as unverified, so retention stays **off** for OVH until the live check confirms an
  hourly US region, and then only for that region (a driver override per region, until Keeper has `billing_rule`).
  Stopped and paused instances bill; shelved ones only for the snapshot.
- **Start field**: `BillingStart` is Nova's `created`, which is at `BUILD`; the meter starts at `ACTIVE`
  (**unverified**; not an API field). `created` is earlier, so boundaries fall early: the safe side. Not for
  per-second regions, where there is no period.
- **Delete latency**: `DELETE /servers/{id}` is asynchronous (`DELETING`); when billing stops is not documented.
- **Verify live**: per region, create and delete at 1, 59 and 61 minutes and read the usage per instance
  (the Public Cloud billing page's per-instance view): whether a started hour is rounded up in the US
  and nowhere else; the offset from `created` to `ACTIVE`; the deletion latency.

## Catalog

Keeper has 135 OVH servers in the live API (all `x86_64`; 31 of them `-flex`), and `b3-8` is
priced in 16 regions (20 zones), on-demand only, in USD; zones are `<region>-a` (1-AZ) or
`-a/-b/-c`. The committed snapshot (2026-11-05) has 104 OVH servers (72 active) and **no
`-flex` types**: the family and conformance tests need a refreshed snapshot (plan 13) to cover
them. Storage rows: `classic` (500 IOPS, 64 MB/s),
`high-speed` (3000, 128), `high-speed-gen2` (20000, 512), 10 to 12000 GB.

- **Dropped in the OVH catalog (v1)**: GPU families (`t1`, `t2`, `a10`, `a100`, `h100`, `h200`,
  `l4`, `l40s`, `rtx5000`, `g1`..`g3`: any server with accelerators; the driver has no NVIDIA
  sysext path, as on AWS before plan 12), Metal (`bm-*`: Ironic bare metal; boot from our
  image unverified), and Keeper's `inactive` legacy families (`eg`, `hg`, `s1`, `sp`, `vps`).
- **Kept**: `b2/b3`, `c2/c3`, `r2/r3`, `d2` (Discovery, shared CPU), `i1` (storage optimised),
  with the `-flex` variants (a 50 GB disk instead of the bigger fixed one: their API
  reference is the flavor name, so they are separate types). Which of those exist in which region: the
  `OfferingLister` returns the Nova flavor names of the region (`GET /flavors/detail`) mapped
  to Keeper's zone IDs of the region (all zones: OVH does not say which zone lacks a
  flavor).
- **Region mismatch** (S4): Keeper regions priced today are mostly legacy names; the alias
  table lives in the driver, with a test that every Keeper OVH region with prices maps to a
  name or is listed as unmapped.
- Price: Keeper's hourly price per region; no spot, no per-GB root disk (the flavor's disk is part of the price).

## Node options (ADR-020)

Built-in defaults: the root disk is the **flavor's local NVMe disk** (what OVH sells with the
flavor), public IPv4 on, no extra volumes. `RootDiskBytes` returns 0 for it because the
size depends on the type, not on the settings; **the catalog must use the type's root disk
from Keeper** as the ephemeral storage the scheduler assumes: `storage_size` (25 to 400 GB) for
`b`, `c`, `r` and `d2`, but for `i1` `storage_size` (1950 to 7650 GB) includes the local NVMe
disks, so the root is the `storages` entry of 50 GB (`ssd`), not the total: check that the core does so for a driver that returns 0
(hcloud and vultr need it too); if not, extend the contract (`RootDiskBytes(settings, type)`) in
this plan.

**Storage** (`vendors.ovh.storage`, and the storage profile's `ovh` block):

| Setting | Meaning |
|---|---|
| `root.bootFromVolume: {volumeType, size}` | boot from a Cinder volume created from the image (`classic`, `high-speed`, `high-speed-gen2`; 10 to 12000 GiB). The flavor's disk is then unused. The only way to choose the root size and type. |
| `data[]` (`name`, `volumeType`, `size`, `filesystem`, `mountPath`) | extra volumes: Nova block device mappings `source_type: blank`, `destination_type: volume`, `delete_on_termination: true`, so they are part of the create call and nothing is left. At most 8. |
| `localStorage` | **not supported** in v1 (Keeper's local disks of OVH flavors are the root disk; extra ephemeral NVMe on `i1` unverified): a profile that asks for it is invalid for every type (ADR-017). |

`Supported`: a volume type or size the Cinder `volume types` list of the region lacks is
unsupported; `bootFromVolume` needs Nova ≥ 2.67 for `volume_type` in the mapping (S8).
`PriceDelta`: Keeper's GB-month storage price of the type per region, hourly = /730;
complete (IOPS scales with size; there is no provisioning knob). A volume is zonal in
3-AZ regions; the zone of the instance is given as the volume's availability zone.

**Device hints** (`Prepare`): the volume IDs exist only after the call, so the hint is a serial
pattern plus size: virtio disks carry the first 20 characters of the volume UUID as the
serial (`/dev/disk/by-id/virtio-<20 chars>` for virtio-blk; with `hw_disk_bus=scsi` the by-id name is
different). Which bus Flatcar sees on OVH is unverified (S8), so the hint is
`SerialPattern: "*"` with `SizeBytes`, excluding the root device, until S8 fixes it; equal-size volumes
rely on the volume unit's lock, as on AWS.

**Network**: generic only. `publicIPv4: true` is the only accepted value (above);
`publicIPv6` is **informational**: `Ext-Net` ports carry both families (unverified, S5); a
`publicIPv6: false` request does not remove the address, so the validator accepts it but
the reference documents that the node still has one. `inbound.rules` and `isolation` go to
the managed security group (CIDR rules; self-reference for `nodeclass`/`run`, which also
works over public addresses because the group matches ports, S5), and do not apply to a
brought group, as with AWS. No network profiles (nothing per-NIC to choose; bandwidth is
per flavor): a network profile without an `ovh` block does not exist on OVH.

**Placement profiles** (`ovh: {serverGroup: {policy}}`): Nova server groups, `affinity` or
`anti-affinity` (the two OVH's API documents; `soft-*` are Nova's and unverified here),
one per (region, profile, run key), `perUse`, passed as the scheduler hint `group`. Groups are
free. A group that cannot be satisfied (anti-affinity beyond the hosts, affinity on a full
host) fails the launch with `NoValidHost`, i.e. `InsufficientCapacity`. The
documented size limits are "bounded by hypervisors and quota" (vendor-capabilities): no
fixed number to track. Dedicated tenancy, partitions: not on OVH.

**Boot parameters on the image**: the image carries `hw_qemu_guest_agent=yes`,
`image_original_user=core` (as the Flatcar docs); `hw_disk_bus`, `hw_scsi_model`,
`hw_firmware_type`: S2/S8 decide, the how-to states the values that worked.

## Verified on paper, to be proven live (spikes)

Run by the coordinator in the Spare Cores OVH project before the driver code; the answers
are written back into this plan and the reference doc.

| # | Spike | Decides |
|---|---|---|
| S1 | `openstack image set --community` (and `--public`) a private Flatcar image; boot it from a second project by ID. List `--public` for Flatcar. | rung 2 or 3 |
| S2 | Boot `flatcar_production_openstack_image.img` (stable, amd64) with Ignition user data through Nova on a `d2-2`/`b3-8` flavor: with and without `config_drive`, with only `Ext-Net`; Ignition applied; hostname, provider ID; user-data size limit (16 KiB, 64 KiB); time to boot; that `ignition.platform.id` is `openstack`. | image properties, `configDrive` default |
| S3 | Glance import methods (`image-import-info`), `web-download` from the Flatcar release URL, direct upload time; `community` and `shared` visibility; image storage price. | how the image step uploads |
| S4 | Keystone catalog region names of the project today vs Keeper vs the operator's tooling; Nova AZ names in 1-AZ and 3-AZ regions; Nova microversions (tags ≥ 2.26, `volume_type` ≥ 2.67). | region alias table, zones |
| S5 | Security groups: attached by name and by UUID; does a group filter the `Ext-Net` port; default group rules; `Ext-Net` IPv6; Neutron tags extension; self-reference between public addresses; port quota. | managed groups, ADR-017 on firewalls |
| S6 | Service-account OIDC flow against Keystone with a Go client; token lifetime; the minimal IAM action set (compute, network, network security, image read, volume); the canary's read-only role (`infrastructureSupervisor`: can it list images, networks, flavors?). | credentials mode |
| S7 | Error texts and statuses: quota exceeded, `NoValidHost` (which flavor can be forced to fail?), a flavor missing in a region, `Ext-Net` exhaustion; rate limits. | error table |
| S8 | `bootFromVolume` with each volume type and `volume_type` in the mapping; device names and by-id of data volumes; image `hw_disk_bus`. | storage, device hints |

## In-memory fake (`pkg/vendors/ovh/fake`)

Keystone token and catalog (regions with and without endpoints, expiry and re-auth, a
failing region), Nova (flavors per region with their own IDs, servers with the state
machine `BUILD` to `ACTIVE`/`ERROR` and the `scheduling` task state, name and tag filters,
metadata, server groups with hints, `OverQuota` and `NoValidHost` injection, absolute
limits, foreign servers, `SHUTOFF` after a guest poweroff, anchored name regex, the 65,535
byte user-data check), Neutron (`Ext-Net`, security groups with rules and the creation
defaults, tags optional, a group in use refusing delete), Glance (images by name, tags,
properties, status, visibility, owner), Cinder (volume types, volumes attached by mappings).
Used by the conformance suite (`conformance.Run` per resource kind and over the complete
list of kept Keeper OVH types of the committed snapshot) and the driver's table tests (the
create-request golden: every field asserted).

## Tests

- **Conformance**: identity, zone mapping (1-AZ dummy zone, 3-AZ), idempotent create (a replay
  with another flavor returns the original; a stale same-name server of another UID is removed),
  NotFound semantics, list by tags, the state mapping including `SHUTOFF`, delete idempotency,
  the user-data limit, error classes, quotas, `OfferingLister`, `ImageReporter`, node options and
  resources for `securityGroups` and `serverGroups`.
- **Unit**: the OIDC flow against a fake IdP and Keystone, the alias table over the
  committed Keeper snapshot, the family classification test (like AWS' quota family test).
- Coverage of `pkg/vendors/ovh/...` at least 80 % of statements (report the number).
- A `test/data/sc-snapshot` subset for OVH regions and servers for the catalog mapping test.

## L5 lane (`test/e2e-ovh/`, workflow job `e2e-cloud.yml` with `vendor: ovh`)

Same shape as plan 10 (k3s on the runner, Tailscale join, the `tailscale` template, scenarios as
a bash runner), tier and policy per [testing and releasing](../../../how-to-guides/testing-and-releasing.md).
Scenarios: `amd64` (an exact flavor, on-demand; node labels, server metadata and tags, config
drive/metadata, `kubectl logs`/`exec`), `scalezero`, `drift` (change `storage.root`
to `bootFromVolume`: replacement, old server and volume gone), `db` (anti-affinity or affinity
server group + a `high-speed` data volume mounted by the first-boot unit), `described`,
`byo` (a workflow-created security group `use`d; SCarpenter does not delete it), `negative`
(`publicIPv4: false` invalid; a profile that is unsupported stays Pending), `quota` (Nova
limits equal the Control Panel/API quota; `used` follows launch and delete), `uninstall`,
and `poweroff` (a node that powers itself off is deleted by the GC: the OVH-specific case
above). No `arm64`, no `spot`, no `local`.

**Account and identity** (provisioned by the operator's IaC, not here): a **dedicated Public Cloud project**
for the lane with one active region, low project quotas and a spending alert (OVH has per-project
consumption alerts: unverified); the region restriction is "only the lane's region is activated in
the project"; the instance-type restriction cannot be an IAM deny (IAM has no flavor condition:
unverified), so the guard is the lane's own types (`defaults.sh`) plus the project's core and
instance quota as the ceiling. Credentials: an OVH service account for the lane with the IAM
policy of S6 on that project only; **no GitHub OIDC trust to OVH is documented**, so
the client ID and secret are secrets of the `e2e` environment (`OVH_CLIENT_ID`,
`OVH_CLIENT_SECRET`, variables `OVH_PROJECT_ID`, `OVH_REGION`), unlike AWS's keyless role:
an open question. The canary uses a second service account with the read-only role
and a driver guard that refuses non-GET calls (`test/canary/ovh_guard.go`, with the reflection test
like AWS). Canary checks: Keystone login, catalog regions, flavors of the lane's types, the pinned image
resolved to exactly one, Nova limits against `E2E_MIN_*`, artifacts (shared).

**Cost**: a `d2-2` or `b3-8` flavor is cents per run (`b3-8` is 0.0656 USD per hour in Keeper); a
run is about 25 minutes. Images stay (cents per month). Guards of the cost table: budget alert
(unverified feature), flavor list, run tag (`scarpenter-cluster=scarpenter-ci-<run id>` in
metadata, tag and the managed groups' names), sweeper, run limit, teardown that fails on leftovers,
project quota, read-only canary, approval by the `e2e` environment.

**Sweeper** (`e2e-ovh-sweeper.yml`, every 3 hours, environment `e2e-sweeper`, `main` only): in every active region,
delete servers whose tag or metadata marks a `scarpenter-ci-*` cluster and whose `created`
is older than 2 hours, then their volumes (unattached, same marker in the name prefix and
the description: Cinder volumes may have no tags), server groups and
security groups (name prefix `scarpenter-scarpenter-ci-` plus the description marker). It never
touches images.

## Docs

`docs/reference/ovh-driver.md` (the sections of the AWS reference: regions and zones, images and the
naming convention, launch, node settings, resources, network and addresses, tags, quotas, error
mapping, IAM actions, deferred, not verified), `docs/how-to-guides/ovh-credentials.md` (service
account and user modes; the IAM policy as JSON under `docs/reference/ovh-iam-policy.json`,
validated in `make lint`), `docs/how-to-guides/ovh-images.md` (upload per region with
`openstack`, naming, tags, the IaC-neutral route), the OVH lane section of testing, the add-a-vendor
checklist entries, the chart's `vendors.ovh` values. Deferred in the docs: private networks and
gateways, IPv6-only, GPU and Metal flavors, arm64, `localStorage`, Glance community publishing
(S1), automatic image import (`ImageImporter`), flavor stock from the OVH API.

## Dependencies and ordering

1. S1 to S8 first (they change the plan's rung, region handling and credentials).
2. The operator's IaC: the OVH image step, the region discovery, the controller service account
   (the driver needs none of it to be coded, the lane needs the project and account).
3. The driver, fake, conformance, docs (this plan's code).
4. The lane and sweeper, canary.
- No dependency on an image-import plan (ADR-021: rung 4 is reserved). Were it ever built, it
  would replace the operator's image step; the driver's `ImageImporter` is a Glance upload from
  the artifact URL.
- The cross-vendor generics this plan needs from the core are plan 14's: the `images` kind
  reaching `ResolveImage` (A1); `RootDiskForType`, with the catalog's root disk as the
  fallback for `0` (A2); the garbage collector deleting instances that stay `terminating`
  (A4); message-based classification (A6); the region alias allowance (A8). Plan 14 comes
  first.

## Gates

CONVENTIONS gates 1 to 4 (L3 green: the OVH driver must not change core behaviour);
`go test ./pkg/vendors/ovh/...` with at least 80 % coverage (report the number);
`actionlint` and `shellcheck` clean on the lane's workflows and scripts; the lane's
non-OVH parts run in the dry run (`make test-e2e-ovh-dryrun`); report the expected cost per run.

## Open questions for the maintainers

| # | Question | Recommendation |
|---|---|---|
| 1 | Rung 3 now, or wait for S1 and publish community images (rung 2)? | **Decided (ADR-021): rung 3 now.** Run S1 anyway (an hour): if `community` works, Spare Cores can later publish from one project in every region and `imageOwner` becomes the default hand-off; rung 3 stays for users who want their own. |
| 2 | Region names: trust the Keystone catalog and keep an alias table, or move Keeper to the new names? | **Decided (plan 14, A8):** the catalog is the truth; the operator's tooling reads regions from the API; the alias table lives in the driver and the hand-off uses API names; ask the crawler owners to keep one row per catalog name. |
| 3 | Credentials: OIDC service account (custom Keystone flow) or OpenStack user/password first? | Ship `openstackUser` first (no custom code) and add `serviceAccount` as soon as S6 proves the flow; service accounts suit automation, so make that the documented default. |
| 4 | CI credentials: OVH client secret in the `e2e` environment, or wait for a keyless option? | Accept the secret with a project-only, one-region, quota-limited account; revisit if OVH documents GitHub federation. |
| 5 | Private network and `isolation: run` over a vRack network, and Gateway-based private-only nodes: v1 or later? | Later. v1 is public `Ext-Net` + security groups; the default-route problem and the billed Gateway are real costs. |
| 6 | Wait for the scheduler inside `CreateInstance` (45 s) so capacity errors fall through to the next candidate? | Yes; the alternative leaves the node to the 5-minute launch timeout and loses the fallback. |
| 7 | Metal (`bm-*`) and GPU flavors: exclude in v1? | Exclude; a later plan when the sysext and Ironic boot are proven. |
| 8 | Read flavor stock from the OVH API (`flavor.available`) for `OfferingLister`? | Not in v1; revisit when the first `NoValidHost` storm shows in the lane. |
| 9 | Should the image step live with the operator's main IaC (per Flatcar pin) or in a separate job with its own OpenStack user? | Operator's choice; document both. Either way the pin is the same as the cluster's NodeClass pin. |
| 10 | Generic OpenStack driver later (other OpenStack clouds, same gophercloud code)? | Keep the package layout (`auth`, `nova`, `neutron`, `glance`) so OVH is a thin layer; decide after a second OpenStack vendor appears. |

Decided by default, maintainer to confirm (the executing change took the recommendation, or the
conservative option where the plan gave none):

- **1** rung 3 stays (ADR-021). `includeCommunityImages` is not a block field: the lookup's
  `visibility` filter (`filters: [{name: visibility, values: [community]}]`) is the same switch, so a later rung 2 needs no API change.
- **2** the alias table is in the driver (`regions.go`, `SCARPENTER_OVH_REGION_ALIASES`, the chart's
  `vendors.ovh.regionAliases`); `Regions` returns API names. A row that differs from Keeper's name needs the Keeper name in
  `vendors.Region`, the core change of A8, which this change does not make (core files are out of scope for a driver):
  while every row is an identity (today it is) nothing is missing.
- **3** `openstackUser` only (the standard `OS_*` environment, from a Secret). The `serviceAccount` OIDC flow is not built;
  `docs/reference/ovh-iam-policy.json` is its policy, for later.
- **4** the lane's identity is therefore an OpenStack user (`OVH_OS_USERNAME`, `OVH_OS_PASSWORD` of the `e2e` environment, a
  second read-only user for the canary), not a service account; the same project-only, one-region, quota-limited rule applies. The
  lane installs `python-openstackclient` with a pinned version that nobody has run yet.
- **5** later: no vRack network, gateway or private-only node.
- **6** yes: `CreateAndWait`, 45 s, `SCARPENTER_OVH_SCHEDULE_WAIT`.
- **7** Metal and GPU flavors are excluded.
- **8** no flavor stock from the OVH API.
- **9** operator's choice; both are documented in `ovh-images.md` (any tool).
- **10** `osapi` (interfaces and types), `osapi/sdk` (gophercloud) and `fake` are the layers; they are not split further until a second OpenStack vendor exists.

Not in the table, also taken by default:

- Configuration is read from the environment (`SCARPENTER_OVH_SCHEDULE_WAIT`, `SCARPENTER_OVH_REGION_ALIASES`, the `OS_*`
  variables), not from `--ovh-*` flags: `pkg/operator/options` is core and a vendor does not extend it. The flags the plan names
  for the service-account mode (`--ovh-auth-url`, `--ovh-idp`, ...) go with that mode.
- Neutron security groups and Nova server groups have no tags the driver relies on, so a managed security group's description holds
  the ownership marker (with its creation time) and a managed server group is recognised by its name prefix; `tags` are not offered in the
  selector for either kind, the NodeClass's `spec.tags` are not put on them, and the NodeClass of a managed server group is not recoverable
  (it is `perUse`, which does not need it, and the conformance suite's managed-resource checks therefore run for the security group kinds only).
- The project's `default` security group is not used as a default: nodes always get a managed group, or the one you `use`.
- `ReportImages` reports amd64 only (v1 has no arm64 type); `ResolveImage` resolves both.
- `ListInstances` and `FindInstance` leave out servers being deleted (task state `deleting`) for their first 5 minutes; one stuck
  longer is listed as terminating for the garbage collector's second delete (driver contract); a `SHUTOFF` server stays.
- The driver offers the `metadata-from-pods` hardening (plan 14 said no vendor needed it from OVH, but OpenStack's metadata service
  has no hop limit and holds the user data).
- The sweeper does not sweep volumes (the driver's volumes go with their server and carry no marker); the teardown fails the job on an
  available volume in the lane's own project instead.
- Every billing rule is reported unverified, so retention stays off for OVH.

## Sources

- Flatcar on OVHcloud: https://www.flatcar.org/docs/latest/deploy/cloud/ovhcloud/
- Flatcar on OpenStack: https://www.flatcar.org/docs/latest/deploy/virt-options/openstack/
- Flatcar release files (stable): https://stable.release.flatcar-linux.net/amd64-usr/current/ and `.../arm64-usr/current/`
- Ignition platforms: https://coreos.github.io/ignition/supported-platforms/
- OVH, upload your own image: https://docs.ovhcloud.com/en/guides/public-cloud/compute/upload-own-image.md
- OVH, share images between projects: https://docs.ovhcloud.com/en/guides/public-cloud/compute/share-images-between-projects.md
- OVH, service accounts and OpenStack: https://docs.ovhcloud.com/en/guides/manage-and-operate/iam/authenticate-api-openstack-with-service-account
- OVH, tokens: https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/managing-tokens
- OVH, image life cycle: https://docs.ovhcloud.com/en/guides/public-cloud/compute/image-life-cycle
- OVH, quotas: https://docs.ovhcloud.com/en/guides/public-cloud/cross-functional/increasing-public-cloud-quota
- OVH, networking and vRack: https://docs.ovhcloud.com/en/guides/public-cloud/network-services/vrack
- OVH, regions: https://www.ovhcloud.com/en/about-us/global-infrastructure/expansion-regions-az/
- OVH API schema: https://eu.api.ovh.com/v1/cloud.json
- gophercloud v2.15.0: https://github.com/gophercloud/gophercloud/releases/tag/v2.15.0
- Keeper (regions, zones, servers, prices): https://keeper.sparecores.net
