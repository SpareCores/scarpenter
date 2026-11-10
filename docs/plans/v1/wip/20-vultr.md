# 20 — Vultr driver

Depends on: 14 (the shared groundwork: the `images` kind, `RootDiskForType`, the idempotency
label, account-wide quota errors, message-based classification), 22 (the images repository
that builds the Flatcar image for Vultr), 08 (the model driver) and 10 (the model lane).
Read
`docs/plans/v1/CONVENTIONS.md` first. Spec: overview §7, §9, §11 (as the AWS
model), 00a §6, §9, §11; ADR-002, ADR-011, ADR-012, ADR-013, ADR-017, ADR-018,
ADR-020, ADR-021. Plan 08 (AWS driver) and plan 10 (AWS e2e) are the structure and the
standard of completeness.

Status of the research: 2026-11-10, from public docs, the public Vultr catalog
endpoints (`GET /v2/regions`, `/v2/plans`, `/v2/os`, `/v2/regions/{id}/availability`,
which need no key), the `govultr` v3.34.0 source, Flatcar's release server and
sources, and Keeper's public tables. No account, key or authenticated call was used.
Everything marked **unverified** has to be confirmed by a spike (section 11) before
the code that depends on it is written; nothing marked unverified is a fact.

## Goal

`pkg/vendors/vultr`: a production driver for Vultr Cloud Compute (`/v2/instances`)
that boots pinned Flatcar from a snapshot in the user's account, with a firewall group
and an optional VPC, complete against an in-memory fake and the conformance suite,
plus an opt-in L5 lane and sweeper. amd64 only: Vultr has no Arm instances (all 205
Vultr rows of Keeper, 204 x86_64; the single arm64 row is the GH200 bare-metal plan).
No Vultr calls in the implementation plan except the spikes of section 11, which a
maintainer runs by hand with a throwaway key.

## 1. Findings in one page

| Question | Answer | Sure? |
|---|---|---|
| Image rung | **Rung 3** for the default: a snapshot created from a public raw image URL, in the user's account, global (not per region). Rung 1 exists in a weak form (below) and is a measured opt-in, not the default. | medium |
| Rung 1: public Flatcar image | Vultr lists `Flatcar Container Linux LTS/Stable/Beta/Alpha x64` as operating systems (`os_id` 2075, 2077, 2078, 2079; `GET /v2/os`, 2026-11-10). It is Vultr's build, not Flatcar's. Nothing says which Flatcar version it holds, how often Vultr rebuilds it, or whether its OEM id is `vultr`. The Fedora CoreOS docs say Vultr's FCOS images are out of date and recommend a custom snapshot; Flatcar's own Vultr page still describes cloud-config and iPXE. A version that SCarpenter cannot pin or read back breaks ADR-012 and ADR-017, so it is not the default. | high for the listing, unverified for the content |
| Rung 2: publish a public image | Not available to "anyone". Vultr snapshots cannot be shared with or made public to other accounts ("Vultr does not support transferring an instance or its snapshot to another user"). The Vultr Marketplace needs an approved verified-vendor account, Vultr's review of each app, requires a cloud-init OS and a small filesystem, and delivers an app, not a base OS. A route to ask Vultr about (open question 1), not a plan dependency. | medium |
| Does Flatcar publish a Vultr image? | **No.** The release server has no `flatcar_production_vultr_image*` for any channel or arch; the OEM list of Flatcar's `coreos-overlay` has no `oem-vultr`; Flatcar's docs list Vultr only as "community support". Arches published for the generic image: amd64 and arm64; Vultr only needs amd64. Channels: stable, beta, alpha, lts. | high |
| Ignition platform | Ignition has a `vultr` provider (reads `http://169.254.169.254/user-data/user-data`; in every Flatcar channel, LTS's Ignition 2.19 included), and Afterburn has `vultr`. Flatcar's main `grub.cfg` sources the OEM partition's `grub.cfg` and turns `set oem_id="vultr"` into `flatcar.oem.id=vultr` on the kernel command line; bootengine's `ignition-setup-pre` makes that the `PLATFORM_ID` of `ignition --platform=...` (`ignition.platform.id` is not used on Flatcar). The generic image sets no OEM id, so on KVM it runs Ignition as `qemu` (fw_cfg) and gets no config: the pipeline patches the OEM partition (section 3.3). | high (source); the live boot is S1 |
| User-data size limit | **Unverified.** The API wants user data base64 encoded in `user_data` (documented). No limit is documented in what was found; the default templates are about 5 KiB, the budget is 16 KiB (spike S3). | unverified |
| Import from URL | `POST /v2/snapshots/create-from-url` with `url`, optional `description` and `uefi`; no region field; snapshots are region-independent and billed at $0.05 per GB-month of the compressed size. The Fedora CoreOS docs use an uncompressed `.raw` file at a public object-storage URL. Accepted formats, size cap, snapshot count limit and import time: **unverified** (S1). A custom ISO (a different feature) is limited to 10 GB and two per account. | medium |
| Version to image ID | A snapshot has only `id`, `description`, `size`, `compressed_size`, `status`, `date_created` (no tags). The pipeline names it by the description `flatcar-<channel>-<version>-<arch>` (the Vultr list filter `description` exists; exact or substring match is unverified). | medium |
| Resources | VPC (`/v2/vpcs`, `description` only: no name, no tags), firewall group (`/v2/firewalls`, `description` only, account-wide, inbound only, at most 50 rules), block storage (regional), reserved IPs (regional). "VPC 2.0" is gone: govultr removed VPC2 in v3.33.0 (2026-09-01); `attach_vpc` on instance create is the current field. | high |
| SDK | `github.com/vultr/govultr/v3`, v3.34.0 (2026-11-05), official, MIT licence, active (a release every few weeks). It drops the HTTP status from errors (section 4.2). | high |
| Zones | None. The region is the unit; Keeper's zone ID equals the region ID (`ewr`). | high |
| Quotas | No quota API. Account limits (maximum instances, maximum instance cost) are shown in the console only. No `QuotaReporter`. Availability per region: `GET /v2/regions/{id}/availability` (stock), usable as `OfferingLister`. | medium |
| Spot | None for cloud instances. `deploy_preemptible` exists only on five GPU bare-metal plans (type `vdm`), outside v1. | medium |

Sources: [Flatcar on Vultr](https://www.flatcar.org/docs/latest/deploy/cloud/vultr/),
[Flatcar release server](https://stable.release.flatcar-linux.net/amd64-usr/current/),
[Ignition `vultr` provider](https://github.com/coreos/ignition/blob/main/internal/providers/vultr/vultr.go),
[Flatcar `grub.cfg`](https://github.com/flatcar/scripts/blob/main/build_library/grub.cfg),
[bootengine `ignition-setup-pre.sh`](https://github.com/flatcar/bootengine/blob/flatcar-master/dracut/53ignition/ignition-setup-pre.sh),
[Afterburn platforms](https://github.com/coreos/afterburn/blob/main/docs/platforms.md),
[Fedora CoreOS on Vultr](https://github.com/coreos/fedora-coreos-docs/blob/main/modules/ROOT/pages/provisioning-vultr.adoc),
[Vultr snapshot FAQ](https://docs.vultr.com/products/storage/snapshots/faq.md),
[snapshots across regions](https://docs.vultr.com/support/products/orchestration/can-i-use-a-snapshot-in-a-different-vultr-region),
[transfer to another user](https://docs.vultr.com/support/products/compute/how-do-i-transfer-a-vultr-compute-instance-to-someone-else),
[Marketplace vendor account](https://docs.vultr.com/platform/marketplace/vendor-account/create-a-vendor-account),
[Marketplace publishing](https://docs.vultr.com/platform/marketplace/publishing/make-the-app-public),
[rate limits](https://docs.vultr.com/support/platform/api/what-rate-limits-apply-to-the-vultr-api),
[account limits](https://docs.vultr.com/platform/billing/manage-account-limits),
[govultr](https://github.com/vultr/govultr).

## 2. Scope

- **Client layer**: `govultr/v3` behind a narrow `vultrAPI` interface holding only the calls
  used: `Instance.Create/Get/Delete/List`, `Region.List/Availability`, `Plan.List`,
  `Snapshot.List/Get`, `FirewallGroup.Create/Get/Delete/List`, `FirewallRule.Create/List/Delete`,
  `VPC.Create/Get/Delete/List/ListAttachments`. One client (Vultr has one global endpoint,
  no regional clients); the API key from the environment; a user agent `scarpenter/<version>`;
  an own `http.Client` whose transport records the HTTP status (section 4.2), a client-side
  limiter of 10 requests per second (the documented limit is 30 per second per source IP,
  HTTP 429 above it), per-call timeouts like the AWS driver (review N4).
- **Driver** (section 4): regions, the one dummy zone per region, `ResolveImage` and
  `ImageReporter` (snapshot by description), `CreateInstance`, `GetInstance`, `FindInstance`,
  `ListInstances`, `DeleteInstance`, `TagKey`, `NodeClassValidator` for the `vultr` block.
- **Node settings** (section 7): defaults only (plan disk, public IPv4); `Supported` empty
  for every profile kind in v1, so a profile with a `vultr` block makes its offerings
  unavailable with a reason (ADR-017); `PriceDelta` zero and complete.
- **Resources** (section 3.2): kinds `firewallGroups` and `networks` (VPC).
- **Quotas and stock** (section 6): `OfferingLister` from the availability endpoint; no
  `QuotaReporter`; message-based error classification.
- **In-memory fake** (`pkg/vendors/vultr/fake`) and the conformance suite (section 8).
- **L5 lane and sweeper** (section 9).
- **Docs**: `docs/reference/vultr-driver.md` (regions, images, defaults, resources, tags,
  limits, error mapping, deferred features) and `docs/how-to-guides/vultr-credentials.md`.
- **API**: `pkg/apis/v1alpha1/vultr` (the `Settings` type), one line in each of
  `VendorBlocks`, `StorageProfile`, `NetworkProfile`, `PlacementProfile`, `Names`, `Get`
  and the `For` methods of `vendors.go` (the profile types hold an empty block in v1: see
  section 7), the CEL rules, `make generate manifests`.

Not in scope: bare metal (`/v2/bare-metals`, plans `vbm-*`), the GPU plans, block storage
volumes, reserved IPs, VPC-only nodes behind a NAT gateway, per-run VPCs, backups, DDoS
protection (section 10, Deferred).

## 3. Hand-off: what the operator's IaC gives SCarpenter

### 3.1 Account scope and credentials

- **One API key = one account.** There is no project, subscription or tenant to name:
  the key decides the account. The block has no scope.
- **Authentication.** `VULTR_API_KEY` from a Kubernetes Secret (chart value
  `vendors.vultr.credentials.mode: secret`, secret key `apiKey`); no other mode:
  Vultr has no workload identity for callers (its `/v2/oidc` makes Vultr the issuer of
  tokens, it does not accept external ones; unverified that there is no consumer side).
  The key is bound to an **IP allowlist**: the controller's egress
  address must be in it, which is a deployment fact to document in the how-to.
- **Least privilege.** Vultr API keys belong to a user whose ACLs are coarse
  (`provisioning`, `subscriptions`, `subscriptions_view`, `firewall`, ...; a broad sub-user may hold many more than the controller needs). The controller needs instances, firewall groups, VPCs and
  snapshots (read). Which ACL covers snapshots and VPCs is **unverified** (S5); the how-to
  names the minimal set once measured. There is no per-region or per-plan deny.
- **Startup check**: `GET /v2/regions` (public) proves connectivity; `GET /v2/instances?per_page=1`
  proves the key and the ACL; a failure is `VendorReady=False` with the reason.

### 3.2 The `spec.vendors.vultr` block

```yaml
vendors:
  vultr:
    regions: [ewr, fra]                     # Vultr region IDs; empty = every region the API lists
    networking: {...}                       # override of spec.networking, as the AWS block
    resources:
      images:                               # rung 3: the snapshot(s) (plan 14, A1); optional; global, so the same ID in every region
        use:
          ids: {"*": [1f2e3d4c-...]}   # explicit snapshot ID(s), global in the account: the key "*" (every region); or
          # name: "flatcar-{channel}-{version}-{arch}"   # description pattern (default)
      firewallGroups:
        use: {ids: {"*": [<group id>]}}   # the same ID in every region
        # or: create: {lifecycle: {policy: idleTTL}}  (the default: a managed group)
      networks:                             # optional: no VPC unless configured
        use: {ids: {ewr: [<vpc id>]}}
        # or: create: {lifecycle: {policy: idleTTL}}
```

`regions` replaces the "zones" of the AWS block; the zone of every node is its region
(Keeper zone ID `ewr`, label `topology.kubernetes.io/zone=ewr`).

| Kind | Scope | Needed to launch? | `use` | `create` | Default |
|---|---|---|---|---|---|
| `images` (the generic kind of plan 14, A1; not managed) | account (global), region key | **yes** (rung 3) | `ids` under `*` or per region (snapshot UUIDs); `name` (glob on the description, `{channel}`, `{version}`, `{arch}`, `{region}`), `match.state`/`createdAfter` and `orderBy: created` (client-side, `date_created`); no `tags` (rejected: no tags), `match.architecture` only `amd64` | never (ADR-021; rung 4 would add `ImageImporter`) | the name pattern `flatcar-{channel}-{version}-{arch}` |
| `firewallGroups` | region (the object is account-wide; one managed group per region by name, see below) | yes, because **inbound is open without a group** (vendor-capabilities) | `ids` under `*` (the same ID everywhere); `name` (glob against the description, `{region}`); `match.createdAfter` (client-side); **no `tags`** (rejected: no tags) | yes: a group with the rules of `spec.networking.inbound` (empty = deny all) | `create`, `idleTTL` 24 h |
| `networks` (VPC) | region | no | `ids` per region; `name` (glob against the description), `match` (`cidr` = `v4_subnet`/`v4_subnet_mask`, `within`) | yes: a VPC per NodeClass and region (Vultr picks a free range unless the block gives `v4Subnet`: unverified) | none: no VPC, nodes use the public network |
| SSH key | global | **no** (Ignition installs keys) | not modelled | not modelled | none |
| placement groups, volumes, reserved IPs | | no (v1) | | | |

- `use.tags` is rejected by `NodeClassValidator` for Vultr (every kind but instances): VPCs, firewall groups and
  snapshots carry no tags, only a `description`. An IaC can mark them in the description
  (for example `<env>-<thing>`), which `use.name` can match.
- **Ownership marker of managed resources** (ADR-018): the name prefix
  `scarpenter-<cluster>-<kind>-<region>` plus the marker `cluster`, `nodeclass`, `managed-by`,
  `resource-kind` written into the **description** (`scarpenter-my-cluster-firewall-groups-ewr
  | cluster=my-cluster nodeclass=default managed-by=scarpenter resource-kind=firewallGroups`).
  Both prefix and marker are checked before the garbage collector touches anything. The
  description length and charset limits are **unverified** (S6); the format shortens the
  cluster name with the engine's hash rule if needed.
- **Firewall groups are account-wide**: a managed group per region by name keeps the engine's
  region scope (a few more free objects: up to 33). The per-account group limit is
  unverified (S6). `Delete` returns `Retryable` while `instance_count > 0`; `InUse` is
  `instance_count > 0` (last use unknown: the zero time, the engine counts from its first
  sighting). Rules: Vultr has no "all protocols" rule and at most 50 per group; a generic
  inbound rule becomes `tcp`, `udp` (with `1:65535` when it has no ports) and `icmp` rules
  per CIDR and family; more than 50 rules is `NodeClassValidator` invalid.
- **VPC** `InUse` from `VPC.ListAttachments`; `Delete` is `Retryable` while attached.
  Firewall groups filter the public interface only (vendor-capabilities, unverified), so a
  VPC is the "same run only" boundary, not the firewall.
- **`networks` and `isolation`**: `none` works everywhere. `nodeclass` needs the VPC (the
  `networks` kind configured, then every node attaches it); without it `isolation: nodeclass`
  makes the offerings unavailable (ADR-017). `isolation: run` needs a VPC per run key
  (the AWS `runSecurityGroups` analogue): not in v1.
- **IPv6**: `publicIPv6: true` sets `enable_ipv6` at create (a /64 on the public interface;
  no subnet to configure). `publicIPv4: false` needs `vpc_only` plus a NAT gateway: not in
  v1; the offerings are unavailable with a reason.

### 3.3 Images: the policy applied

Rung 3 (ADR-021), with one addition to it: Vultr fetches the raw image
itself from a **public URL**, and Flatcar's stock URLs are not usable as they are
(a compressed generic image with an empty OEM id), so the raw image needs a producer.

1. **The pipeline** (decided: the Spare Cores images repository of plan 22, proposed
   `SpareCores/flatcar-images`, the same build as plan 19's UpCloud image with another
   `oem_id`; reduced to publishing a URL): per Flatcar channel and
   version, download `flatcar_production_image.bin.bz2` from the release server, verify the
   signature, decompress, loop-mount the OEM partition (label `OEM`, btrfs) and write
   `set oem_id="vultr"` into its `grub.cfg` (the mechanism of Flatcar's OEM images;
   `flatcar-install -o` only downloads `flatcar_production_<oem>_image.bin.bz2`, which does
   not exist for Vultr; Flatcar's own Vultr page describes only the Vultr OS entry and iPXE
   with cloud-config, which ADR-002 rules out), unmount, upload
   the artifact `flatcar-vultr-<channel>-<version>-amd64.img.gz` (plan 22's naming) plus its
   SHA-256 and signature as a release asset, and the raw file at a public URL if S1 shows the
   import needs raw. The decompressed image is large (the stable generic bz2 is 587 MB; the qemu
   image's virtual size is 13.2 GB; the raw size is unverified, S1) and may hit Vultr's
   import size cap (S1); the pipeline then needs whatever compression Vultr accepts.
2. **Per account, the snapshot**: Vultr snapshots cannot be shared, so every account that
   runs SCarpenter on Vultr makes its own, once per (channel, version): in an IaC tool a
   snapshot-from-URL resource where a provider has one (Pulumi's community `vultr` package has
   `SnapshotFromUrl`; `use_uefi` is a field) with the description
   `flatcar-<channel>-<version>-amd64`; or `vultr-cli snapshot create-url` in a CI job. Cost: $0.05 per GB-month of the compressed size, so
   cents per version. Import time and the deployability on every plan family (vc2, vhf,
   vhp, voc, vx1) are **unverified** (S1, S2).
3. **BIOS or UEFI**: Flatcar's image boots both; Vultr's `uefi` flag is set explicitly at
   import and the choice is measured (S1).
4. **Resolution**: `ResolveImage(arch=amd64, channel, version)` lists snapshots with the
   description filter, requires exactly one with `status` complete and the exact
   description (client-side exact compare), else `ImageNotFound` or `ImageAmbiguous` as on AWS.
   `images.use.ids` skips the lookup. `arm64` is `Unsupported`.
5. **Rung 1 as an opt-in** (a Vultr block field, for example `vendorImage: true`, outside the generic `images` kind of plan 14, which has no such form; mapping channel to `os_id` 2075 lts,
   2077 stable, 2078 beta, 2079 alpha): allowed only if S0 shows that Vultr's image takes
   Ignition from user-data and is current. Even then the version is not readable, so the
   NodeClass gets the condition `ImagesReady` with reason `VersionNotPinned` (Warning) and
   `spec.flatcar.version` is not enforced. Default off (open question 2).
6. **Rung 4** (`SCImage`, `ImageImporter`) would let SCarpenter create the snapshot itself
   from the pipeline's URL (`Snapshot.CreateFromURL`, `ListImages` by description,
   `DeleteImage`): the API fits the reserved interface, so Vultr would be the first
   candidate; reserved by ADR-021, not built in v1, not required here.

### 3.4 What the operator's IaC must provide, and what SCarpenter self-manages

| Driver needs | What your IaC must output | Without it (ADR-022) |
|---|---|---|
| boot image (one snapshot per pinned version, global in the account) | the snapshot ID, under `*` (or repeated per region) in `resources.images.use.ids`, or a snapshot described `flatcar-<channel>-<version>-amd64`; made from the pipeline's public URL with a snapshot-from-URL call per pinned version (a step of the IaC, or `vultr-cli snapshot create-url`) | `ImagesReady=False`: images are never self-managed |
| VPC per region (`networks.use.ids`), optional | the VPC ID per region, or a description the `name` pattern matches | no VPC; a managed VPC when `isolation: nodeclass` needs one; no discovery (no tags) |
| firewall group (`firewallGroups.use.ids`), optional | the group ID (the same in every region); hand over only a group nodes may use | a managed group per NodeClass and region |
| subnet | nothing (Vultr has no subnet object) | n/a |
| account scope | nothing (the key is the account) | n/a |
| tags for `use.tags` | nothing: `use.tags` is rejected, ids and description patterns are used | n/a |
| the pipeline's public URL | the URL of the pinned image, a parameter of the IaC step, not a secret | n/a |
| the API key and its allowlist | the key of a user with the minimal ACLs (3.1) as a Secret, and the controller's egress address in the allowlist | n/a |

## 4. Driver

### 4.1 Launch, delete, list

- **Region and plan**: `Region`, `Plan` = Keeper's `api_reference` (`vc2-2c-4gb`), `SnapshotID`
  from `ResolveImage`, `Label` and `Hostname` = the NodeClaim name, `UserData` = base64 of
  the rendered Ignition (the API requires base64; the console and CLI accept plain text),
  `FirewallGroupID` from `Prepare`, `AttachVPC` = `[vpc id]` when a VPC resource was
  prepared, `EnableIPv6` from `publicIPv6`, `Backups: "disabled"`, `ActivationEmail: false`,
  `DDOSProtection: false`, no `SSHKeys`, no `ScriptID`. The response carries a
  `default_password`; never logged, never stored.
- **Tags**: `Instance.tags` are plain strings (no key/value). Each logical tag becomes the
  string `<TagKey(k)>=<value>`; `TagKey(k)` is `scarpenter.sparecores.com/<k>`. The charset
  and length limits of tags are **unverified** (S6); if `/` or `=` is refused the mapping
  falls back to `scarpenter-<k>=<value>` (the GCP-style rule of overview §7.1). The
  NodeClass's `spec.tags` are added as `key=value` strings (logical tags win). The driver adds
  `scarpenter.sparecores.com/idempotency-key=<NodeClaim UID>` for idempotency (the convention of
  plan 14, A3, also used by hcloud; UpCloud's is `scarpenter-idempotency-key`).
- **Idempotency** (no client token exists): `CreateInstance` first lists instances by the
  idempotency-key tag account-wide (the list filter `tag` takes one value; other tags are
  compared client-side). A hit returns that instance whatever region, plan or capacity type
  the replay asks for (the driver contract). A miss creates. A create whose HTTP call
  failed after the server accepted it is found by the same lookup on the retry; the window
  is a create that the list does not show yet (eventual consistency, unverified, S4): the
  garbage collector's orphan sweep (instances of the cluster without a NodeClaim) is the
  backstop, and the in-process cache of the last 10 minutes of creates closes the window
  inside one controller process.
- **`GetInstance`** by `Instance.Get`; a 404 is `NotFound` (a deleted instance disappears,
  there is no terminated state, so `GetInstance` cannot return a terminated one).
  **State map**: `status` `pending` or `server_status` not `ok` is `pending`; `status`
  `active` with `power_status` `running` and `server_status` `ok` is `running`; `suspended`
  or `power_status` `stopped` is `terminating` (the stopped-node rule of plan 14, A4: a stopped
  instance still bills for the following hours, which the deletion ends (the started hour is
  paid either way), so the garbage collector deletes it; unverified that SCarpenter can ever
  see one).
- **Addresses**: `main_ip` (`0.0.0.0` while pending: treated as none), `v6_main_ip`,
  `internal_ip` (VPC). They feed the serving-certificate approver; `Instance.Region` is
  the region, `Zone` the same string.
- **`ListInstances(regions, tags)`**: one paginated call (`per_page=500`, cursor) with the
  filter `tag=cluster=<name>` (or `region=` when one region is asked), the rest client-side;
  not one call per region. **`FindInstance`** the same with the `nodeclaim` tag.
- **`DeleteInstance`**: `DELETE /v2/instances/{id}`; 404 is success. Whether a still
  installing instance can be deleted is **unverified** (S4); a refusal is `Retryable`.
  The driver refuses to delete an instance that does not carry the cluster tag.
- **provider ID** `scarpenter://vultr/<region>/<nodeclaim>`; instance ref
  `<region>/<instance id>` (a UUID).
- **Rate limits and retries**: 30 requests per second per source IP, HTTP 429 above it;
  govultr retries 429 and 5xx three times with a short wait (`SetRateLimit`, `SetRetryLimit`).
  The driver's own limiter keeps launches, the 2-minute list and the status checks far below
  it (a full sweep is one list call). The cost of a status check per region is one
  availability call, hourly.

### 4.2 Error classification (the text exception)

govultr turns every API error into a plain error (`error returned from api : <message>`, or
the raw body) and returns a nil `*http.Response` with it, so the HTTP status is lost, and
Vultr's error body is `{"error": "<text>", "status": <n>}` with no error code. The driver
therefore (1) captures the status per request, either with govultr's
`Client.OnRequestCompleted(func(*http.Request, *http.Response))` hook, which sees every
response before the error is built, or with a transport in the HTTP client, keyed by a
per-request value in the context, and wraps the error (`*vultrError{Status, Message}`), and (2) classifies by status first, then by message
substring for the cases a status cannot tell apart. This is the documented exception to
"never by message text" (overview §7.2), made a contract amendment for every vendor without
error codes by plan 14 (A6), and the substring table is the part to
verify live (S4); the table below is a **hypothesis** for those rows.

| Class | Rule |
|---|---|
| `Retryable` | 429, 5xx, network errors, the per-call timeout, a canceled context; 409/423 "locked" and dependency messages on delete (firewall group with instances, VPC with attachments) |
| `NotFound` | 404 on get/delete of an instance, group, VPC, snapshot |
| `InsufficientCapacity` | the plan is not in `available_plans` of the region (checked **before** the call, from the cached availability) or the create message says the plan is unavailable or out of stock in the location (message, unverified) |
| `Unsupported` | the plan does not exist or cannot deploy this snapshot or this location (invalid plan, snapshot/plan incompatibility, message, unverified) |
| `QuotaExceeded` | the account limit of instances or monthly cost (message, unverified); code `AccountLimit` |
| `ZoneUnusable` | never (no zones) |
| `Fatal` | 400/401/403 not above, an invalid `user_data`, anything else |

Quota versus capacity stay apart as in ADR-013: the pre-call availability check is capacity,
a limit message is quota. A single plan family refused by name (for example VX1 only on
some regions) is `Unsupported`, cached 24 h by core.

### Billing period (ADR-024, plan 23)

- **Rule**: "The minimum billing unit is one hour"; hourly charges begin at deploy, powered on or off;
  billing stops only by destroying the server; monthly cap 672 hours (GPU 730), so once the cap is reached
  more hours are free until month end (the marginal cost of keeping a node is then zero; later, plan 13's
  `monthly_cap_hours`). Whether the started hour is billed in full on destroy is **unverified** (S7), and so is
  the period anchor. Keeper has 3600/3600: retention stays off until S7 confirms it.
- **Start field**: `BillingStart` is `date_created`. "When deployed" could be later than `date_created`: this
  is the one vendor where the stamp might be after the real start (a late boundary, the unsafe side), so S7
  must measure the offset before retention is enabled.
- **Delete latency**: `DELETE /instances/{id}` is immediate from the caller's view; the billing stop time is
  **unverified**.
- **Verify live (add to S7)**: the offset between `date_created` and the billing start; the hours billed for
  a destroy at 59 and 61 minutes; behaviour at the monthly cap.

## 5. Catalog

Keeper already has Vultr: 205 servers (132 active), 36 regions (33 in the Vultr API;
Keeper also holds the inactive `cmh`, `msp`, `stl`), zone ID equal to region ID (zone
`api_reference` is the city name; match on `zone_id`, ADR-009), prices per region (the
plans API has per-location overrides in `location_cost`, e.g. `vhp-12c-24gb-intel` in `sao`).
Driver rules:

- `Regions()` = `GET /v2/regions` (public); `Zones(region)` = one zone `{ID: region,
  Name: region}`. The catalog intersects with Keeper's active regions.
- **Which Keeper rows are launchable** (an explicit classification by plan-ID prefix, with a
  test that every family in the committed snapshot is classified, as AWS's `mac*` rule):
  `vc2`, `vhf`, `vhp`, `voc` supported; `vx1` supported only when the plan has local disk
  (the API says `storage_type`: 18 of 36 VX1 plans are `block_storage`, diskless, boot from
  a volume: unsupported in v1; `local_and_block_storage` is **unverified**, S2); `vcg-*` Cloud
  GPU, `vdm` GPU bare metal and `vbm-*` bare metal (a different API) dropped. The free plan
  `vc2-1c-0.5gb-free` is dropped (no stock guarantee).
- `OfferingLister.LaunchableTypes(region)`: `GET /v2/regions/{region}/availability`
  (`available_plans`, plus `available_vpc_only_plans`, which are not launchable here); type
  to `[region]`. Refreshed hourly like AWS; Vultr's stock moves faster, so the interval is a
  flag (default 5 minutes: 33 calls per refresh, negligible).
- Root disk: the plan's `disk`. **Contract gap**: `NodeOptions.RootDiskBytes(settings)` is per
  NodeClass, and a fixed-disk vendor needs it per type (hcloud and UpCloud have the same
  problem). Proposal: an optional `RootDiskForType(CatalogType, VendorSettings)` (plan 15's
  signature), fed from Keeper's `storage_size`
  (GB; 1 for diskless VX1, which is excluded anyway). Open question 6.

## 6. Quotas, stock, capacity

- No `QuotaReporter`: nothing readable gives the limit (the account endpoint has balance
  and ACLs only; the limits page is console-only; unverified that no API exposes it).
  Types are "unknown quota": the reactive path of overview §5.6 applies.
- A limit hit is `QuotaExceeded`, which core treats per region and capacity type (30 min),
  while Vultr's limits are **account-wide**: every region fails once before all are gated.
  Cheap (no billing, a few failed calls), but the status says the wrong scope. Open question 5
  proposes a global flag on the error.
- Capacity: availability before the call (above) and a message rule after it. No zone, so
  a capacity miss gates the (plan, region) offering for the 3-minute TTL.
- Spot/preemptible: none in v1. Keeper has no Vultr spot prices for cloud instances, and
  `deploy_preemptible` is true only for five `vdm` GPU plans (no create-field for it in
  govultr 3.34; unverified).
- Billing facts the lane's cost guards need: hourly billing with a monthly cap per plan
  (`hourly_cost`, `monthly_cost` in the plans API); a stopped instance still bills; whether
  a destroyed instance bills the started hour is **unverified** (S7).

## 7. Node options and profiles (ADR-020)

- **Defaults** (`Defaults()`): root disk = the plan disk (not configurable: the root size
  is a property of the plan, and swapping to a bigger sibling plan is forbidden by ADR-017);
  `publicIPv4: true`; no IPv6; the managed deny-all firewall group; no VPC; no data
  volumes.
- **Storage profiles**: `vultr.Storage` is an empty struct in v1 (the CRD needs a type); a
  storage profile with a `vultr` block is `Unsupported` for every Vultr offering. Planned
  (section 10): `data: [{name, sizeGb, blockType: high_perf|storage_opt, filesystem,
  mountPath}]` as pre-created regional block storage attached after create; the region's
  `options` (`block_storage_high_perf`, `block_storage_storage_opt`) say which types exist
  where; the device path (`/dev/disk/by-id/virtio-<first 20 characters of the volume
  ID>`) is **unverified**; the first-boot volume unit's serial-pattern route is the fallback.
- **Network profiles**: none (no NIC features on Vultr's create call). **Placement
  profiles**: none (no groups, no tenancy; dedicated CPU is a plan). Both are empty structs.
- `Supported` returns nothing for every kind and type; the conformance fixture's sample
  profiles are therefore expected `Unsupported`.
- `PriceDelta`: zero and complete (the public IPv4 is included in the plan price; reserved
  IPs and IPv6 are not used or free: unverified for IPv6 pricing, in vendor-capabilities
  "IPv6 and paid-bandwidth prices" are missing from the data).
- `NodeClassValidator`: region IDs against `GET /v2/regions` syntax, `images` shape, no
  `tags` (and any selector field the capability table does not list), firewall rule count and protocols, `isolation` against the configured
  resources.

## 8. In-memory fake and conformance

`pkg/vendors/vultr/fake`, behind `vultrAPI`: regions and per-region availability lists,
plans, snapshots (with `status` pending to complete, descriptions, the exact-versus-substring
filter as a switch), instances (status transitions `pending` to `active`, `main_ip`
`0.0.0.0` while pending, tag filter on one tag, `region` and `label` filters, cursor
pagination), the idempotency lookup, firewall groups (rule limit 50, `instance_count`,
delete refused while attached), VPCs (attachments, delete refused while attached), instances
of other owners (account-wide usage is not needed, but the sweeper and `FindInstance` must
ignore them), error injection per call **with status and message** (including 429, a text-only
error and a lost status), a create that succeeds server-side but times out for the client.
Conformance fixtures: kinds `firewallGroups` and `networks`; `UnknownQuotaTypes` is every
type (no `QuotaReporter`, so the quota rows of the suite are skipped as for a driver
without it); `ResourceParams` empty; hooks `PreCreate`, `Restart`. Golden file of the
`Instance.Create` request (every field asserted, base64 `user_data`). The suite runs
over the complete Vultr type list of the committed snapshot, with the classification test of
section 5. Coverage of `./pkg/vendors/vultr/...` at least 80 % (report the number).

## 9. L5 lane and sweeper

Tier and mechanics: `docs/how-to-guides/testing-and-releasing.md` ("Adding a vendor lane"):
`test/e2e-vultr/` (scripts, `defaults.sh`, `lib.sh` with `redact`), one line in
`test/e2e-vendors.txt`, a credentials step in `e2e-cloud.yml` and `e2e-canary.yml`, the
sweeper `e2e-vultr-sweeper.yml`, the canary's `test/canary/vultr.go`. The scenarios follow the AWS
lane that apply: amd64 exact type (`vultr.vc2-1c-1gb`: node Ready, Pod, logs and exec,
labels with zone = region, instance tags), scale to zero (instance deleted within 2
minutes; no snapshot or group deleted), drift (a changed `images` ID replaces the node),
described not named, bring your own (a tagged-by-description group and VPC made by the
workflow, a second NodeClass `use`s them by ID and `name`, not deleted), managed firewall
group (created, rules as `spec.networking.inbound`, deleted after the last node when
`perUse`), the negative scenarios (arm64 `Unsupported`, a profile with a `vultr` block
unavailable, `publicIPv4: false` unavailable). No spot, no placement, no quota scenario.

**Identity and the cost guards** do not map one to one, and this is the main difference
from AWS:

| Guard (testing-and-releasing) | Vultr |
|---|---|
| CI identity by OIDC, no secret | **Not possible**: a static API key of a dedicated sub-user, stored as the secret `VULTR_API_KEY` of the `e2e` environment; its IP allowlist must be `0.0.0.0/0` for GitHub-hosted runners; required reviewers and the fork rule are the wall |
| Deny other instance types, deny other regions | **Not enforceable** by credentials (ACLs are coarse). Replaced by: the lane's NodeClass and NodePool restrict types and region, and a **dedicated Vultr account (or sub-account) with a low account limit** (maximum instances and maximum instance cost, `Accounts > Limits`) as the ceiling |
| Budget alert to a person | account level (Vultr alert ACL: unverified) |
| Run tag on every resource | instance tag `cluster=scarpenter-ci-<run id>` and `ci-created-at=<unix>`; groups and VPCs carry the marker in their description |
| Sweeper by tag and age | every 3 hours, environment `e2e-sweeper`: instances with a cluster tag `scarpenter-ci-*` older than 2 hours; firewall groups and VPCs whose description marker names such a cluster and that have no instance; **never snapshots** (the Flatcar snapshot is permanent) |
| Run limit, teardown that fails on leftovers | as AWS (90 and 55 minutes) |
| Read-only identity for the canary | a second sub-user with `subscriptions_view` only (unverified that it reads firewall groups, VPCs and snapshots: S5); the canary's guard wraps `vultrAPI` and refuses every non-read method, with a reflection test as `aws_guard.go` |
| Cheap types | `vc2-1c-1gb` and `vhf-1c-1gb`, about a cent per hour; a run costs cents (unverified minimum billing, S7) |

Canary checks: sysext and k3s as shared; regions (`GET /v2/regions`, the lane's region
present); the Flatcar snapshot (`ResolveImage`: exactly one, `complete`); availability of the
lane's types (`LaunchableTypes`); the read-only guard. The lane's account needs the
snapshot, created by the CI account's operator (a snapshot-from-URL call), and the
variables `VULTR_REGION` and the pinned Flatcar version.

## 10. Deferred

Bare metal and GPU plans; block storage volumes and `data` profiles (section 7); VPC-only
nodes with a NAT gateway (`vpc_only`, the regions' `connectivity` lists `nat_gateway`);
`isolation: run` (VPC per run key); reserved IPs; backups; DDoS protection; spot
(`deploy_preemptible` on GPU plans); VX1 diskless plans (boot from a created bootable
block storage via `block_devices`); a `QuotaReporter` if Vultr ever exposes limits; rung 4.

## 11. Spikes (live verification, before or with the first driver PR)

Run by a maintainer on a throwaway account with a restricted key; none belongs to CI.

- **S0** Deploy `os_id` 2077 with a minimal Ignition config in `user_data`: does Ignition
  run (platform `vultr`)? What `/etc/os-release` says; is `update-engine` active; does a
  second deploy a week later show a new version? Decides rung 1's role.
- **S1** Import: build the patched raw image; `create-from-url` with a raw file, then with
  `.gz`, `.bz2`, `.xz`; the size cap; import time; `uefi` true and false; whether
  `description` can be set and filtered (exact or substring); snapshot count limit; the
  Ignition run and `core` login on the result; `flatcar.oem.id=vultr` on the command line.
- **S2** Deploy the snapshot on one plan of each family (vc2, vhf, vhp, voc, vx1 local and
  `local_and_block_storage`, the smallest disk); the plan disk versus the image size; the
  first-boot root growth.
- **S3** User-data: 5, 16, 32 and 64 KiB; where it fails and with what message.
- **S4** Errors: the real messages and statuses for an unavailable plan in a region, an
  unknown plan, the account limit, a bad `user_data`, a duplicate create, a delete while
  `pending` and a double delete; tag list consistency right after create; 429 behaviour.
- **S5** ACLs: the minimal set for create, delete, list, firewall, VPC, snapshot read; what
  `subscriptions_view` reads.
- **S6** Limits and charsets: tag, description and label length and characters (`/`, `=`,
  `|`); firewall groups per account and rules per group (about 50);
  VPCs per region; VPC creation without `v4_subnet`; what the firewall group filters (public
  interface only?) and whether an empty group drops all inbound.
- **S7** Billing: the minimum charge of a destroyed instance, IPv6 and IPv4 prices.
- **S8** Concurrency: the Create latency, N parallel creates; the `main_ip` appearing time;
  time from `Create` to Node Ready over the tailnet.

## Gates

CONVENTIONS gates 1 to 4 (L3 stays green: the Vultr driver changes no core behaviour; the
contract gap of section 5 is a separate core change or a documented follow-up), coverage of
`pkg/vendors/vultr` at least 80 %, `make generate manifests` clean (a new vendor key in the
CRD), `actionlint` and `shellcheck` clean on the new workflows and scripts, the
`test/e2e-plan-selftest.sh` rules for the vendor, and the dry-run of the lane against the fake.
The L5 run is the coordinator's, after the spikes and the operator's setup (section 3.4).

## Open questions for the maintainers

1. **Marketplace.** Ask Vultr whether a verified vendor may publish a Flatcar base image as
   an app, and whether the app is deployable with Ignition `user_data`. *Recommendation:* ask
   in parallel (it would give rung 2 and remove the per-account snapshot), but build rung 3
   first; do not wait.
2. **Rung 1 role.** Allow Vultr's own `Flatcar ... x64` OS as an unpinned opt-in?
   *Recommendation:* no default; decide after S0, and if allowed, only with the
   `VersionNotPinned` Warning and never for benchmark NodeClasses (ADR-017).
3. **Who hosts the raw image URL**, and for how long are versions kept? *Recommendation:*
   the Spare Cores bucket of the pipeline repository, latest three versions per channel,
   with the SHA-256; per-account snapshots are the users' to create. The repository is decided
   (plan 22); GitHub release assets are limited to 2 GiB, so the raw URL needs the bucket or a
   compressed format Vultr accepts (S1): plan 22 open question 2.
4. **Where `images` lives in the API**: **Decided (ADR-021, plan 14 A1):** the generic
   `images` resource kind for every vendor (the selector of ADR-023: `ids`, `name`, `tags`, `match`, `filters`, ...; `ids` per region or under `*`),
   not a Vultr-only block field.
5. **Account-wide quota errors.** **Decided (plan 14, A5):** a driver marks a
   `QuotaExceeded` as account-wide: one boolean on `vendors.Error` and one core branch
   (hcloud and UpCloud need it too).
6. **Per-type root disk** (`RootDiskForType`, section 5), shared with hcloud and UpCloud.
   **Decided (plan 14, A2):** added in the core before Vultr and hcloud.
7. **Weaker lane guards** (no OIDC, no type or region deny). *Recommendation:* accept with a
   dedicated account, low account limits, a restricted sub-user and the sweeper; do not put
   the lane in `test/e2e-vendors.txt` before the account limit is set.
8. **`isolation: nodeclass` through a VPC** while the public interface stays reachable.
   *Recommendation:* support it only with the VPC configured and say so in the docs; no
   `run` until VPC limits are known (S6).
9. **Zone naming**: the label `topology.kubernetes.io/zone=<region>` for a vendor without
   zones (hcloud, UpCloud, Vultr). *Recommendation:* yes, as Keeper's dummy zone.
