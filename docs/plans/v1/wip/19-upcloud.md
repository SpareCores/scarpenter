# 19 — UpCloud driver

Depends on: 14 (the shared groundwork: the `images` kind, `RootDiskForType`, the idempotency
label, account-wide quota errors, the stopped-node rule), 22 (the images repository that
builds the Flatcar image for UpCloud), 06 (core), 08 (the model driver) and 10 (the L5
lane). Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §5.9, §7, §9; 00a §6, §9, §11; ADR-002, ADR-011, ADR-017,
ADR-018, ADR-020, ADR-021. Research date: 2026-11-10. Every fact that was not checked
against UpCloud itself is marked **unverified**; section "Verified and
unverified" lists them, and the spikes of section "Spikes" settle the ones the
design depends on. No UpCloud call was made for this plan.

## Goal

`pkg/vendors/upcloud`: a production driver complete against an in-memory
UpCloud fake and the conformance suite, the typed `spec.vendors.upcloud` block,
the UpCloud reference docs, an opt-in L5 lane and a sweeper. Nodes run
Flatcar, configured by Ignition from the UpCloud metadata service and joined
over Tailscale like on AWS.

Vendor facts that shape everything:

- **Regions are zones.** UpCloud has 15 data centres (`fi-hel1`, `de-fra1`, ...),
  no availability zones inside them. Keeper has a region and a dummy zone with
  the same ID for each. `Zones(region)` returns one zone (ID and name = region).
- **x86_64 only.** The sc-crawler data has no ARM plan: amd64 only.
- **The server is a plan name.** Keeper's `api_reference` is the plan
  (`1xCPU-2GB`, `HICPU-8xCPU-16GB`, `CLOUDNATIVE-...`, `GPU-...`); the create
  call takes `plan`. Plans that include storage (25 GB MaxIOPS and so on) and
  plans that do not (Cloud Native, GPU) exist.
- **No idempotency token, no stock API** (except for GPU plans). Idempotency is by
  label; capacity is reactive.
- **Images are per zone, private and imported.** There is no Flatcar template,
  and nothing can be published to other accounts (next section).
- **Two firewalls.** The *Public & Utility* firewall is per server, stateless
  and cannot be shared; the *SDN* firewall (standalone rulesets) only filters SDN private interfaces.

## Images

**Rung 3 (ADR-021), with a Spare Cores built artifact; the source switches to Flatcar's own
file if upstream accepts an UpCloud image.** Confidence: high that rungs 1 and 2 are impossible,
medium on the mechanics of rung 3 until spikes S1 and S2 have run.

| Question | Answer | Sure? |
|---|---|---|
| Rung 1: a public Flatcar image at UpCloud | No. UpCloud's public templates are UpCloud's own; I found no Flatcar template (the public templates docs page returned 404, so the list itself was not read). Public storages are visible to all users, private ones only to the account and its sub-accounts, and "users can only create private storages". | high |
| Rung 2: anyone can publish a public image | No. Custom images exist only "in the data centre where the origin storage is located", per account; there is no sharing or publishing. | high |
| Rung 3: images in the user's own account, per region | Yes: import into a storage by HTTP URL or direct upload, then "templatize" it (a custom template), once per zone. | high |
| Does Flatcar publish an image for UpCloud | **No.** The release server (stable 4757.2.3) has no `upcloud` image for amd64 or arm64; `vm_image_util.sh` has no `upcloud` type; `coreos-overlay` has no `oem-upcloud`. A Flatcar docs page for UpCloud: none found. | high (not found is not proof for the docs page) |
| Does Ignition support UpCloud | **Yes, from Ignition v2.23.0.** Platform ID `upcloud`: reads user-data from `http://169.254.169.254/metadata/v1/user_data`; merged July 2025, first release v2.23.0 (2025-09). Flatcar stable 4757.2.3 ships Ignition 2.24.0 and Afterburn 5.10.0 (its UpCloud provider, PR 1227, merged 2025-08, is in 5.10.0). **LTS 4081.3.12 ships Ignition 2.19.0: no `upcloud` platform**, so the LTS channel is unsupported on UpCloud until an LTS with Ignition 2.23 or later (`NodeClassValidator` rejects it). The metadata service "is NOT enabled by default": servers need `metadata: yes`. | high (package lists of the releases, Ignition and Afterburn tags) |
| Can another Flatcar image be used | Yes, with one change. UpCloud cannot set kernel arguments, so the platform must come from the image: take the **generic** `flatcar_production_image.bin.bz2` (amd64; arm64 exists but is not needed) and write `grub.cfg` on its OEM partition (label `OEM`, partition 6) with `set oem_id="upcloud"`, the documented way ("Customizing the OEM partition") an OEM image sets its platform. The chain is in Flatcar's sources: the main `grub.cfg` sources the OEM partition's `grub.cfg` and adds `flatcar.oem.id=$oem_id` to the kernel command line; bootengine's `ignition-setup-pre` turns it into `PLATFORM_ID` and runs `ignition --platform=${PLATFORM_ID}`; without an `oem-release` file no OEM sysext is looked for. No `ignition.platform.id` is needed. Unpatched, the generic image on KVM runs Ignition with platform `qemu` (fw_cfg) and boots unconfigured. A stock image of another platform does not work (`openstack`, `aws` read other metadata paths). No simpler documented route exists: `flatcar-install -o <oem>` only downloads `flatcar_production_<oem>_image.bin.bz2`, which does not exist for `upcloud`. | high (source); the live boot is S2 |
| Import formats | UpCloud accepts **raw** (`.raw`, `.img`, `.iso`), and raw compressed with **gzip or xz** (the import transparently decompresses). **qcow2, vhd and bz2 are not accepted.** Flatcar publishes the generic image as `.bin.bz2` and the OpenStack image as qcow2, so no official file imports as it is; the pipeline recompresses (to `.gz`) after patching the OEM partition. | high (docs); "no qcow2" is by omission |
| `Content-Type` | For `http_import` UpCloud "relies on the content-type header" of the hosting server: a `.gz` must be served as `application/gzip`. GitHub Release assets are `application/octet-stream` (**unverified** for the importer): host on object storage with the type set, or use `direct_upload` (the importer's `PUT` takes the header; the Terraform/Pulumi provider sets it from the file extension). | medium |
| Size, time, cost | Image about 9 GB raw, 0.6 GB gzip (**unverified** raw size; the stable `.bin.gz` of other platforms is 0.59 GB). Storage size is 10 to 1024 GB; imported data must not exceed the storage. Import time: unknown (S1). A template is a storage billed per GB per hour at its tier (price **unverified**; about 10 GB times 15 zones); a source storage is billed until it is deleted after templatizing. | low |
| Regional or global | **Zonal** (15 imports, or one import plus cross-zone clones, which UpCloud says take "considerably longer"). Templates are usable only in their zone. | high |
| Shared or public | **No** (neither with other accounts nor public). Sub-accounts of the same main account see private storages. | high |
| Pinned version to image ID | The template is found by **labels** (`upcloud_storage_template` takes `labels`; the API templatize call takes only a title, so SDK users set labels afterwards or use the title): `scarpenter-image=flatcar`, `flatcar-channel`, `flatcar-version`, `flatcar-arch=amd64`, `flatcar-tier=maxiops|standard`. Title convention `flatcar-<channel>-<version>-amd64-<tier>`. The driver's `ResolveImage` lists private templates of the zone and requires exactly one match. | design |
| Tier | "Deployments from custom templates are restricted to use the same storage tier", and the API error table says "only maxiops storages can be templatized" while the prose says any tier. So the root disk tier is the tier the template was made with. Starter and Developer plans include Standard storage, the others MaxIOPS: a MaxIOPS-only template makes the Standard-tier plans' included disk unusable (**unverified**, S1). The design keeps one template per (zone, tier). | low |

The image hand-off (who builds, who imports, who passes the IDs):

1. **Build** (decided: the Spare Cores images repository, plan 22, proposed name
   `SpareCores/flatcar-images`; it also builds the `vultr` and `aliyun` images, one GitHub
   Actions workflow, no cloud credentials): for each Flatcar
   channel and version Spare Cores supports, download the generic image and its
   `.DIGESTS`, verify the signature, loop-mount partition 6 (the Flatcar
   "customizing the image" procedure), write `grub.cfg`, recompress as gzip,
   publish `flatcar-upcloud-<channel>-<version>-amd64.img.gz` with SHA-256 and a signature as
   a release asset. Never boot the image. This is the only Spare Cores specific
   artifact; ADR-021 amends ADR-002's "official image" statement for it.
2. **Upstream** (recommended in parallel, small): a Flatcar PR that adds the
   `upcloud` image type (a copy of `oem-hetzner`/`oem-akamai`: one ebuild, one
   `IMG_upcloud_*` block with a raw `.gz` output). Ignition and Afterburn
   already support the platform. When it ships, the pipeline's source becomes
   the official `flatcar_production_upcloud_image.bin.gz` and step 1 shrinks to a
   download.
3. **Import** (the operator's IaC or a CI job): per zone, a
   `upcloud.Storage` (size 10, tier `maxiops`, `import` block with `http_import`
   or `direct_upload`, `source_hash`) and an `upcloud.StorageTemplate` with the
   labels above; delete the source storage afterwards (the template is a copy).
   The template IDs go into the NodeClass (below).
4. **SCarpenter** only resolves and reports them (`ImageReporter`, condition
   `ImagesReady`, status `images`). It does not import. Rung 4 (`SCImage`,
   `ImageImporter`) would replace step 3 and could use the importer's
   `direct_upload` to avoid hosting; it would still need the patched file, so it
   does not remove step 1.

## Hand-off: `spec.vendors.upcloud`

```yaml
vendors:
  upcloud:
    regions: [fi-hel1, de-fra1]       # zones; default: all the credentials can use
    storage:
      root: {tier: maxiops, size: 25Gi, encrypted: true}   # size: GB
    networking: {publicIPv4: true}    # the generic override
    resources:
      images:                         # rung 3; `use` only (plan 14, A1); optional
        use:
          ids: {fi-hel1: [<uuid maxiops>, <uuid standard>]}   # per zone; the tier is read from the template's label
          # or tags: {scarpenter-image: flatcar}   (default: this selector,
          # narrowed by channel, version and tier from the NodeClass)
      networks:  {use: {tags: {scarpenter-use: "true"}}}
      firewalls: {use: {ids: {fi-hel1: [<ruleset uuid>]}}}
      serverGroups: {create: {lifecycle: {policy: perUse}}}
```

Resource kinds the driver needs to launch a node:

| Kind | Scope | `use` | `create` | Without `use` (ADR-022) | What your IaC must output |
|---|---|---|---|---|---|
| `images` (templates) | zone and tier | `ids` per zone (both tiers in the list); `tags` on template labels (default), `match.zone`, `filters` `tier`/`template_type` (client-side), `orderBy: label:flatcar-version` (templates have no creation time and no architecture field) | **no** (rung 3; ADR-021 reserves `SCImage`) | the label convention, narrowed by the NodeClass's Flatcar channel and version | the template IDs per zone, or labelled templates |
| `networks` (SDN private network) | zone | ids; `name` (glob, client-side; `{region}` and `{zone}` are the same here); `tags` (the list call filters by label), `match.zone` (server: `?zone=`), `match.cidr`/`within` (client-side), default `filters: type: [private]` | yes, `idleTTL`: IPv4, DHCP on, no router, CIDR from the parameters (a /24; the range is open question 7); a limit exists (`NETWORK_LIMIT_REACHED`) | labelled networks (discovery, planned), else **no network**: the node has the public and utility interfaces only. A private interface is added only when `networks` is configured | the network UUID per zone, or the label `scarpenter-use: "true"` on it |
| `firewalls` (SDN rulesets) | region (the ruleset is account-wide; one managed ruleset per region by name, as hcloud and Vultr do) | ids; name; `tags` (client side: the ruleset list has no label filter, **unverified** that its items carry labels) | yes, `idleTTL`: one stateful ruleset per NodeClass (inbound `spec.networking.inbound`, the network CIDR for `isolation: nodeclass`); attached to the private interface only | none attached (the SDN default is drop in, accept out, a server property) | the ruleset UUID (the same in every zone) |
| `serverGroups` (anti-affinity) | run | name; ids | yes, `perUse` per (zone, profile, run key) | none | nothing |

Not a resource kind: the **Public & Utility firewall** of each server. It is
per server, stateless, with at most 1000 rules, and cannot be brought or
shared, so the driver writes it for every node from `spec.networking.inbound`
plus built-in rules (below). A brought ruleset ("the brought group replaces SCarpenter's own", as for other vendors) therefore does not apply to the
public interface on UpCloud: the brought ruleset only ever covers the SDN
interface. State this in the reference docs.

Not needed: a subnet (a network is the unit), an SSH key (Ignition installs
keys; the create call sends `password_delivery: none` and
`login_user: {create_password: no}` so that no one-time password is created or
mailed), a project or resource group (the token is the account), IPs (the
public addresses come with the server), volumes (created in the call).

**What the operator's IaC must provide, and what SCarpenter self-manages.** Provided by
the operator (any tool, the API or the control panel):

1. **Images**: the templates, one per zone and tier, as in "Images" above, passed as
   `resources.images.use.ids` per zone (both tiers in the list) or found by their labels
   (amd64 only; the tier is part of the name, `flatcar-<channel>-<version>-amd64-<tier>`,
   and the template labels say the same).
2. **Template creation**: a storage import and a template per zone and tier, labelled as
   above, fed by the artifact URL and hash.
3. **Optionally an SDN network per zone and an SDN ruleset** (ids, name, or the
   `scarpenter-use: "true"` label for discovery, planned). The ruleset is attached to the
   private interface only: for UpCloud the public interface's firewall is the driver's
   business, and the reference says so.
4. **Account scope**: none (the token is the account). SCarpenter's own token must see what
   the IaC's token created (**unverified** across sub-accounts, S6).
5. **A range for SCarpenter's own managed networks** outside the operator's networks
   ("networks of an account must not overlap"): a documented `/16`, configurable (open
   question 7).

SCarpenter self-manages, without `use`: no network (the node has the public and utility
interfaces only); a managed SDN network and ruleset where the NodeClass asks for a private
interface or `isolation: nodeclass`, server groups. Images are never self-managed. There is
no vendor default for any kind; discovery uses labels where the list call filters them.

**Account scope and credentials.** UpCloud has no project: everything lives in the
account of the credential, and region limits are account limits. The controller
authenticates with an **API token** (bearer; `UPCLOUD_TOKEN`, a Secret in the
chart; username and password also work and are not recommended). Tokens expire
(a sub-account's tokens can last 364 days) and have no scopes; the
sub-account's permission fragment (servers, storages, networks, tags) is the only
narrowing, and it is by resource type, not by label. No workload identity or OIDC
exists. The chart gets `vendors.upcloud.credentials.mode: secret` (and `none`
for credentials supplied some other way), like AWS's modes.

## Scope

### Client layer

- **SDK: `github.com/UpCloudLtd/upcloud-go-api/v8`** (MIT, v8.42.0, 2026-09,
  maintained by UpCloud, active). It covers servers, storages (clone,
  templatize, import), networks and routers, server groups, the per-server
  Public & Utility firewall, account limits, zones, plans, labels and filters.
  It lacks the standalone **firewall rulesets** and the private-ruleset attach
  call (not in v8.42.0). A **v9** module (`.../v9`, OpenAPI generated, alpha:
  `v9.0.0-alpha.0.0.108.0`) has them. Decision: v8 for everything, and a thin
  hand-written client (about three endpoints: list and get rulesets, attach and
  detach the private ruleset, set `firewall_private` through the existing v8
  `ModifyServer` fields) behind the `upcloudAPI` interface; revisit when v9 is
  stable (open question 3).
- A narrow `upcloudAPI` interface holding only the calls used: `GetZones`,
  `GetPlans`, `GetAccount`, `GetServers` (with label filters), `GetServerDetails`,
  `CreateServer`, `StopServer`, `DeleteServerAndStorages`, `WaitForServerState`,
  `GetStorages` (private templates, by zone), `GetStorageDetails`,
  `GetNetworks` (label filter), `CreateNetwork`, `DeleteNetwork`, the firewall
  calls (`CreateFirewallRules` for the Public & Utility firewall, plus the
  ruleset calls above), `GetServerGroups`, `CreateServerGroup`,
  `DeleteServerGroup`, `GetIPAddresses`, `GetDevicesAvailability`.
- Credentials from the environment (`UPCLOUD_TOKEN`); `Init` builds the client and
  makes no call. One client for all zones (the API is global; the zone is a
  parameter). User agent `scarpenter/<version>`. Per-call deadlines (45 s; 90 s
  for `CreateServer`), and no SDK retry of 5xx (only the SDK's pollers retry; the
  driver classifies 5xx as `Retryable` and backs off, also on throttling). Rate limits: none documented (**unverified**, S7).

### Driver

- **Regions and zones.** `Regions`: `GET /zone` (public zones; private zones only
  when the account has them), cached for 1 h. `Zones(region)`: one zone, ID and
  name equal to the region (Keeper's dummy zone). `RegionUnavailable` for a zone
  the account cannot use (a trial account, **unverified**).
- **ResolveImage / ReportImages.** Per zone: list private templates, filter by
  the label convention (or the `images.use` form), require exactly one match
  that is `online`, report its UUID and tier; the template's size is the image's
  root disk size. No match gives `ImagesReady=False` with the zone named and the
  hint "import the Flatcar template for <version>". Arch is amd64 only: an arm64
  request is `Unsupported`.
- **CreateInstance**, from a `CreateRequest`:
  - Name: `hostname` and `title` = the NodeClaim name (lower case, at most 128
    characters), providerID `scarpenter://upcloud/<zone>/<nodeclaim>`, instance
    ref `<zone>/<server uuid>`.
  - `plan` = the API reference; `zone`; `metadata: yes` (required for Ignition);
    `firewall: on`; `user_data` = the Ignition config as the raw JSON body
    (not a URL); `login_user` and `password_delivery` as above; `timezone` UTC;
    `simple_backup: no`; `video_model`, `nic_model` default; `remote_access`
    off.
  - Storage: the root as `action: clone` of the template with the resolved
    `size`, `tier` (same as the template's) and `encrypted`; data volumes as
    `action: create` with `title`, `size`, `tier`, `encrypted` and labels
    (`scarpenter-cluster`, `scarpenter-nodeclaim`). The create call accepts up
    to 16 devices in the table and "more than four" in an error text: the
    driver caps at the lower number until S3 says otherwise.
  - Networking: `public` IPv4 (and IPv6 when `publicIPv6`), the `utility`
    interface (always), and the `private` interface on the configured network
    when `networks` resolves (the first and cheapest case has none).
  - Labels on the server, mapped by `TagKey`: **`scarpenter-<key>`**
    (`scarpenter-cluster`, `scarpenter-nodeclaim`, `scarpenter-nodepool`,
    `scarpenter-nodeclass`, `scarpenter-managed-by`), because label keys are 2
    to 32 printable ASCII characters and `scarpenter.sparecores.com/cluster` has
    33 (the same encoding as for the `scarpenter-use` label). `ValidateNodeClass`
    rejects `spec.tags` keys longer than 32 or starting with `_`, and more than
    255 labels in total.
  - After the call returns (the server is in `maintenance` while its storage is
    cloned): write the Public & Utility firewall, attach the private ruleset and
    set `firewall_private: on` when a private interface exists, join the server
    group when the placement profile asks (the group is a field of the create
    call, so it is set there, not afterwards).
  - **Idempotency**: no token exists. The server carries the label
    `scarpenter-idempotency-key=<NodeClaim UID>` (the `nodeclaim` label holds the
    NodeClaim name, as on every vendor). Before a create, the driver looks the key up
    (`GET /server?label=...`, a single call across zones); after a timeout or a lost response, the same lookup. `IdempotencyKey`
    replays return that server whatever zone or plan the replay asks for. The
    label is written in the create body, so a server without it cannot exist
    (checked by S5). A failed create leaves nothing: partial servers found by the
    lookup with no storage are deleted by the next call.
  - **State mapping**: `maintenance` (while cloning) and `started` without a
    node yet are `pending`/`running`; `error` is `terminating`; a server we have
    stopped, or found `stopped`, is `terminating` (the stopped-node rule of plan 14, A4: a
    stopped server is not coming back and may be billed for the next hours, **unverified**; the
    hour already started is paid either way).
  - **Addresses** (`Instance.Addresses`): the server's public IPv4 and IPv6, the
    utility address and any SDN address, for the serving-certificate approver
    (`direct` template: IP SANs against these).
- **DeleteInstance**: UpCloud only deletes a **stopped** server. Hard stop, wait
  for `stopped`, then `DELETE /server/<uuid>?storages=1&backups=delete` (every
  storage on a node server belongs to it, as we create them in the call). A gone
  server (404) and one already `stopped` are success; a server in `maintenance`
  (cloning, or a stop in progress) is `Retryable`. Storage deletion is part of the
  call, so no storage outlives a node unless the call failed between the two steps;
  the sweeper and the garbage collector's orphan pass (labels `scarpenter-cluster`
  on storages) cover that.
- **GetInstance / ListInstances**: `GET /server` with label filters, filtered
  client-side by zone; no terminated state exists, so `GetInstance` returns
  `NotFound` for a deleted server (allowed by the contract: "NotFound when the
  vendor does not know it").
- **Limits**: 1 to 10 interfaces and 16 storages per server; combined limit 24;
  `SERVER_CREATING_LIMIT_REACHED` bounds concurrent creates (`Retryable`).
- **Spot**: only GPU plans have spot variants (`GPU-SPOT-...`) and Keeper's spot
  rows are for those; v1 launches on-demand only and the UpCloud catalog drops
  spot offerings (as the AWS driver drops `mac*`). No interruption handling.
- **GPU plans**: dropped from the v1 catalog (no GPU resources or drivers in v1,
  ADR-011); the driver keeps a table of plan families (`GPU-*`) it declares
  unsupported. Reconsider with plan 12's outcome.

### Billing period (ADR-024, plan 23)

- **Rule**: hourly; the Simple plan capped at 672 hours a month (**unverified** on a primary page);
  Keeper 3600/3600 with `stopped_server_charged: true` (Cloud Native: not billed when stopped; irrelevant, a
  retained node runs). Whether the period counts from creation or from first start, and that it counts from the start and not the clock hour (the docs are
  silent on partial hours), is **unverified** and is checked live before retention is enabled (plan 23 has no `anchor` field).
- **Start field**: no creation stamp on the server detail could be confirmed. The driver writes its own
  creation time into a server label (`scarpenter-created`, RFC 3339, set in the create call, so before the
  server exists, hence early: the safe side) and `BillingStart` reads it back; no label, no `BillingStart`,
  no retention for that node (an Event, never a guess). Whether a label value may hold a timestamp (the
  character set) is **unverified**; the fallback is epoch seconds.
- **Delete latency**: the longest of the hourly vendors: the server is stopped first (hard stop, wait for
  `stopped`), then deleted with its storages. The margin must cover it; the driver measures it in the lane
  and exposes it as `DeleteLatency`; the core uses the larger of the NodeClass margin and that latency plus
  the 90 s floor (ADR-024).
- **Verify live**: the start of the period (create time versus first start); the billed hours for a server
  deleted at 59 and 61 minutes; the stop-then-delete duration; the label value characters.

### Catalog and offerings

Keeper already has UpCloud: 15 regions, a dummy zone per region (`zone_id` =
region ID), servers keyed by plan name (`vendor` `upcloud`, `cpu_architecture`
x86_64, `cpu_allocation` shared), on-demand prices per zone in EUR (hourly,
capped at 672 hours a month; the hourly rate is the monthly price divided by 672
and a node that lives a month pays the monthly price), IPv4 prices, storage
rows for `hdd`, `standard` and `maxiops` with per-region GB-month prices, and
status `RETIRED` for plans that are no longer offered. Things the offerings
need that Keeper lacks: nothing for v1. Availability: no stock API for regular
plans (reactive `SERVER_RESOURCES_UNAVAILABLE`); GPU stock exists
(`/device/availability`) and is not used in v1. A plan can be missing in a zone:
Keeper's price list is per zone, so an offering exists only where UpCloud prices
the plan; a launch that disagrees is `Unsupported` (24 h).

Currency: EUR. Check at implementation how the core compares prices across currencies (open question 10).

### Node settings and profiles (ADR-020)

Built-in defaults: root `tier` = the plan's included tier (Standard for Starter
and Developer plans, MaxIOPS for the rest and for plans without included
storage), `size` = the plan's included storage, at least 25 GB, `encrypted:
true` (free), public IPv4 on. This is the one type-dependent default, like
GCP's Hyperdisk series; documented. **Open question 4** (the size default).

| Block | Fields | Notes |
|---|---|---|
| `storage.root` | `tier` (`maxiops`, `standard`), `size`, `encrypted` | `hdd` (Archive) is not bootable from a template here (**unverified**). Size from the template's size up to 1024 GB; the scheduler's ephemeral storage is the size. `iops` and `throughput` do not exist (not tunable). |
| `storage.data[]` | `name`, `filesystem`, `mountPath` (generic), `tier` (`maxiops`, `standard`, `hdd`), `size`, `encrypted` | In the create call; deleted with the server. The device is `/dev/disk/by-id/virtio-<first 20 hex digits of the storage UUID>` (the metadata service's disk `serial`; **unverified** as the by-id name), unknown before create, so the first-boot unit matches the serial pattern `virtio-*` and the size, as for AWS; equal-size volumes rely on the unit's lock. |
| `localStorage` | not supported | No local disks: every plan disk is network storage (Keeper has no local disks for UpCloud either). |
| network profile | none in v1 | `nicModel` exists (`virtio` default, others rate-limited) and has no use; a profile with no UpCloud block does not exist on UpCloud (ADR-017). |
| placement profile | `serverGroup: {antiAffinity: strict | yes}` | One server group per (zone, profile, run key), `perUse`; `strict` is enforced at start and fails with `SERVER_RESOURCES_UNAVAILABLE_FOR_STRICT_AA` (`InsufficientCapacity` for the zone; the group size limit is unknown, **unverified**). No cluster/co-location construct exists: a `cluster` profile has no UpCloud block, so those offerings are infeasible. `tenancy: dedicated` has none either. |

`Supported`: root tier per plan (a Standard root only where a Standard template
exists in the zone, by the image report; a plan whose included tier differs from
the root tier is allowed but priced extra); server groups for every plan.
`PriceDelta`: a disk larger than the plan's included storage is billed in full per
GB-hour at its tier (the billing page says the largest disk that fits in the quota
is the part of the plan; a bigger one does not fit, **unverified**, S4), data
volumes per GB-month from Keeper's storage rows, IPv4 for Cloud Native plans (no
included address) from Keeper's IPv4 price; everything is complete (no
provisioned IOPS). `EpochReporter` when Keeper's storage or IPv4 prices load.

Networking (generic): `publicIPv4: true` default. `publicIPv4: false` needs a
`networks` resource with a gateway the user provides (a NAT gateway or router):
the utility network has no internet access (**unverified**), so on the
default route it is `Unsupported`, with the AWS driver's rule. `publicIPv6` adds
an IPv6 address on the public interface (UpCloud supports IPv6 on public
interfaces only). `isolation: none` is the default; `nodeclass` uses the SDN
network and ruleset; `isolation: run` needs one SDN network per run key: **not
supported in v1** (open question 6), its offerings are infeasible per ADR-017.

Public & Utility firewall written for every node (stateless, so replies must be
allowed explicitly; first match wins; the last rule is the default):

1. `accept in` ICMP and ICMPv6 (neighbour discovery needs it), DHCP replies.
2. `accept in` from `spec.networking.inbound` (CIDRs and ports).
3. `accept in` UDP 41641 (Tailscale direct paths; the `tailscale` template only).
4. `accept in` TCP and UDP to the ephemeral range `32768-60999` (replies to the
   node's own connections; Flatcar's default range), IPv4 and IPv6.
5. `drop in` (default). Outbound: accept all.

Rollout takes "1-2 minutes", and the account's default rule set applies until it
does (**unverified** what that is). S3 measures the window; the alternative is a
firewall off plus a guest-side rule from the base template, which the core does not
have. The rules are an iteration target of L5, not a final list.

### Resources (`ManagedResources`)

Kinds `networks`, `firewalls`, `serverGroups` per the table above; `images` is
`use`-only and not a managed kind (no `create`, no GC). Names and marker:
`scarpenter-<cluster>-<kind>-<region>[-hash]` (UpCloud titles/names are free text,
at most 255) plus labels `scarpenter-cluster`, `scarpenter-nodeclass`,
`scarpenter-managed-by`, `scarpenter-resource-kind` (all under 32 characters);
networks and server groups accept labels, rulesets accept labels (**unverified**
for rulesets in the list response). Dependency order for deletes: servers, server
groups, firewall attachments (a ruleset still attached to a server refuses
deletion: `Retryable`), rulesets, networks (a network with attached interfaces is
`Retryable`). `Resolve` with an empty reference returns nothing (no vendor
default network; the public interface needs none). `InUse`: a network is in use
while a server of the cluster has an interface on it; the engine counts from its
first sighting when the answer is unknown. The managed network is IPv4 /24, DHCP
on, DHCP default route off, no router; managed networks are limited by the
account's `networks` limit.

### Quotas (`QuotaReporter`, overview §5.9)

Source: `GET /account` `resource_limits` (cores, memory in MB, per-tier storage
in GB, public IPv4 and IPv6, networks, floating IPs, GPUs) against usage computed
by the driver from the lists of servers (cores and memory per server; states
that count: **unverified**, assume all but deleted), private storages (GB per
tier), IP addresses and networks. **All limits are account-wide**
(`GlobalQuotas`; `Quotas(region)` returns none). Buckets: `cores`, `memory`,
`storage-<tier>`, `public-ipv4`, `networks`; `Consumption(plan)`: the plan's
cores, memory MB and one IPv4 (none for Cloud Native), plus the root disk GB on
its tier. A limit of 0 or an absent field means "unknown", not "none" (**unverified**);
the driver reports a bucket only when the limit is positive. Quota errors:
`SERVER_CORE_LIMIT_REACHED`, `SERVER_MEMORY_LIMIT_REACHED`,
`*_STORAGE_LIMIT_REACHED`, `SERVER_IP_LIMIT_REACHED`, `NETWORK_LIMIT_REACHED`,
`DETACHED_FLOATING_IP_LIMIT_REACHED`. Trial accounts have small limits and a
locked firewall: documented, not supported as a target.

### Error mapping

By the API's error codes (`upcloud.Problem.ErrorCode()`), never message text;
unlisted codes are `Fatal`.

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `SERVER_RESOURCES_UNAVAILABLE`, `STORAGE_RESOURCES_UNAVAILABLE`, `SERVER_RESOURCES_UNAVAILABLE_FOR_STRICT_AA`, `IP_ADDRESS_RESOURCES_UNAVAILABLE` (zone out of addresses; also gates the zone, S5) |
| `Unsupported` | `CORE_MEMORY_UNSUPPORTED`, `PLAN_CORE_NUMBER_ILLEGAL`, `PLAN_MEMORY_AMOUNT_ILLEGAL`, `STORAGE_TIER_ILLEGAL` (tier not allowed on the plan) |
| `QuotaExceeded` | the six limit codes above, marked **account-wide** (plan 14, A5: UpCloud's limits are account-wide, so every zone is gated); `LABEL_LIMIT_REACHED` is `Fatal` (our bug) |
| `ZoneUnusable` | `ZONE_NOT_FOUND`, `ZONE_INVALID` (a zone that vanished is `RegionUnavailable` from `Zones`); `ZONE_MISMATCH` (template or network in another zone) is our bug: `Fatal` |
| `Retryable` | `SERVER_CREATING_LIMIT_REACHED`, `SERVER_STATE_ILLEGAL`, `STORAGE_STATE_ILLEGAL` (a stop or clone in progress), HTTP 5xx and 429 (**unverified** that 429 is used), network errors, the call timeout, `STORAGE_IN_USE` and dependency errors on network or ruleset deletes |
| `NotFound` | `SERVER_NOT_FOUND`, `STORAGE_NOT_FOUND` (for a template: also `ResourceMissing`), `NETWORK_NOT_FOUND`, `FIREWALL_RULESET_NOT_FOUND` (exact names **unverified**) |
| `Fatal` | `AUTHENTICATION_FAILED`, `ACTION_FORBIDDEN`, `*_FORBIDDEN`, `INSUFFICIENT_CREDITS` (402; billing, not a capacity: Event on the NodeClass, `Fatal`, so that nothing retries into a drained account), `USER_DATA_INVALID`, validation errors |

Every code of the table has a test.

### In-memory fake and conformance

`pkg/vendors/upcloud/fake`: zones, plans with included storage and tiers, the
account limits, servers with the `maintenance`/`started`/`stopped` transitions
and the stop-before-delete rule, label filters, storages (templates per zone and
tier, clone with size rules, import, per-zone isolation), networks, rulesets with
attach and the private-firewall switch, the per-server firewall rule list,
server groups with strict anti-affinity, account-wide usage and limits, other
owners' servers, and error injection per call and per code. The driver's table
tests assert every field of the `CreateServer` request (golden). The conformance
suite runs once per resource kind over the UpCloud types of the committed
Keeper snapshot (all plans; GPU plans on the unsupported list; a test fails for
a new plan family until it is classified). The fixture hooks (`Profiles`,
`ProfileTypes`, `ResourceKind`, `ResourceParams`, `PreCreate`, `Restart`) follow
the AWS ones.

### Docs

`docs/reference/upcloud-driver.md` (zones, images and the template convention,
credentials, the two firewalls and the rules written, labels, defaults, profiles,
resources, quotas, errors, deferred, not verified); the API reference gets
`vendors.upcloud` and the profile blocks (`pkg/apis/v1alpha1/upcloud`, one line
each in `VendorBlocks`, `StorageProfile`, `PlacementProfile`, `Names`, `Get` and the
`For` methods, and `NetworkProfile` stays without an UpCloud field); a how-to,
"Import Flatcar templates into UpCloud"; the node-settings and
vendor-resources pages get the UpCloud rows; chart values for the credentials
mode; the permission list (below) replaces an IAM policy.

**Permissions** (UpCloud has no policy language): a dedicated sub-account with API
access, server, storage and network access (as in a typical sub-account permission file
fragment) and no GUI access. List, in the doc, the operations the driver uses so
that an operator can judge it.

## L5 lane and sweeper

Tier and cost policy as in `docs/how-to-guides/testing-and-releasing.md`: a full
lane on pull requests touching the driver or its workflow, smoke on core changes,
a release gate and a nightly canary slot; a line in `test/e2e-vendors.txt`; the
vendor's section in the testing how-to.

- **Identity**: UpCloud has no OIDC federation: a **static API token** of a
  dedicated sub-account (`scarpenter-ci`), stored as the secret `UPCLOUD_TOKEN` of
  the `e2e` GitHub environment, with `UPCLOUD_ZONE` (one zone, for example
  `fi-hel1`) as a variable. The token expires (up to a year): the canary notices
  (a read-only `GET /account`), and the rotation is `bootstrap/upcloud.sh`'s.
  an IaC tenant may not be able to mint it; document the manual step. A leaked token
  is as powerful as the sub-account: its limits (small `resource_limits` cores and
  memory, set by the main account) are the cost guard, together with the sweeper.
- **Prerequisite**: the Flatcar templates of the pinned test version in the lane's
  zone (both tiers if S1 allows), created by the operator's IaC from the pipeline's artifact;
  persistent, labelled `scarpenter-use: "true"`, never touched by the sweeper.
- **`.github/workflows/e2e-upcloud.yml`**, a copy of the AWS lane with the
  UpCloud specifics: `environment: e2e`, concurrency group, 90-minute job, 55-minute
  scenarios; Tailscale join with the federated identity as on AWS; k3s control plane
  on the runner; nodes join over Tailscale.
- **Scenarios** (`test/e2e-upcloud/`):
  1. amd64 on-demand exact plan (`upcloud.1xCPU-1GB`): node Ready, Pod Running,
     `logs` and `exec`, labels as §5.5, server labels as above, `metadata` on,
     user-data size recorded (measure against the unknown limit, S3).
  2. Scale to zero: servers gone (stopped and deleted, storages gone) within
     2 minutes of the Pod's deletion.
  3. Drift: change `storage.root.size` → replacement node, old server and storage
     gone.
  4. Quota: `cores` and `memory` limits in the metrics equal `GET /account`; usage
     moves with a launch and a deletion within one refresh.
  4a. Public firewall: from the runner (outside the tailnet) the node's public
     TCP ports (10250, 22) do not answer, while the node stays Ready over
     Tailscale; the rules on the server equal the driver's list; measure the
     rollout window.
  4b. Two nodes with the `colocated`-style anti-affinity profile and one run key:
     one server group named and labelled by SCarpenter, both servers members on
     different hosts (`anti_affinity_status` met); after deletion the group is gone.
  4c. Described, not named: a `vcpus` range and a metric threshold within the
     allowed test plans.
  4d. Bring your own: the workflow creates a labelled SDN network and ruleset; a
     NodeClass `use`s them by `tags`; the node gets a private address, the
     ruleset is attached and the private firewall on; both survive the run
     (teardown deletes them).
  4e. Data volume: a `storage.data` profile with a small MaxIOPS volume mounted at
     the profile's path; the volume is deleted with the node.
  5. Final check: no server, storage, network, ruleset or server group labelled
     with the run's cluster remains.
- **Teardown** (`if: always()`): delete NodePools, wait for NodeClaims, force-delete
  everything labelled with the cluster (servers: stop, then delete with storages),
  remove the runner from the tailnet, dump diagnostics (SCarpenter logs, events,
  server details, the VNC console is not available over the API: record the
  serial output only if S2 finds a way).
- **`.github/workflows/e2e-upcloud-sweeper.yml`**: every 3 hours and manual; in
  the environment `e2e-sweeper`; one account-wide `GET /server?label=scarpenter-cluster`
  (a single call for all zones) deleting servers whose cluster label starts with
  `scarpenter-ci-` and that are older than 2 hours, then orphaned storages,
  server groups, networks and rulesets with that label prefix. Never touches
  resources without the prefix.
- **Cost**: a `1xCPU-1GB` server is billed per hour (price unverified; euros per
  month for the plan, hourly rate its 1/672); a full run is some hours of a few
  servers: cents. Templates are the persistent cost, not the run.

## Spikes (first, before the driver: manual, by the coordinator)

Each needs an UpCloud account and costs cents; none belongs in the repository's
tests. Their results go into the "not verified" section of the reference doc.

- **S1 (image)**: import the patched generic image (gzip) with `http_import` and
  `direct_upload`; record the raw size, the import and templatize times, the
  Content-Type behaviour of a GitHub Release URL versus object storage, whether
  a Standard-tier storage can be templatized, whether a Standard-tier clone of a
  MaxIOPS template is refused, and the cost line of a template.
- **S2 (boot)**: create a server from the template with `metadata: yes` and an
  Ignition config as `user_data`: BIOS or UEFI boot works (the image has both);
  `flatcar.oem.id=upcloud` on the command line and Ignition's platform `upcloud`; the
  disk grows on first boot (Flatcar's root resize after a clone to 25 GB);
  `login_user`/`user_data` being "ignored with other than Linux templates" does
  not stop the metadata from carrying `user_data`; the interfaces get DHCP
  addresses and the metadata address is reachable in the initrd.
- **S3 (limits)**: user-data size (try 16, 32 and 64 KiB; the budget is 16 KiB as on
  AWS, the default templates are about 5 KiB); the per-create storage limit (4 or
  16); the firewall rollout window and the default rule set; whether the rules of
  section "Node settings" keep a node working (DNS, DHCP, sysext downloads).
- **S4 (billing)**: a 64 GB root on a 25 GB plan; Standard versus MaxIOPS
  included storage; Cloud Native IPv4; stopped-server billing; the monthly cap.
- **S5 (idempotency and capacity)**: a create that times out and its label lookup;
  `SERVER_RESOURCES_UNAVAILABLE` text and status for a full zone; which code a
  zone out of addresses gives.
- **S6 (accounts)**: whether a sub-account's token sees the private templates,
  networks and rulesets another sub-account created.
- **S7 (API behaviour)**: rate limits and the status of throttling; list cost for a
  few hundred servers; whether `GET /account` has usage and what a missing limit
  means.

## Dependencies

- Core, resource engine, conformance, AWS driver and the L5 lane (06, 08, 10).
- **The images repository** (plan 22) and the operator's template creation (plan 14, B1 contract): the driver can be written and tested against the fake
  without them; **L5 cannot run without them**. No `SCImage` plan is needed for
  this vendor (rung 3).
- Plan 14 (the groundwork), which comes first.
- The operator's IaC: the points above; the e2e
  sub-account and its token (manual).
- ADR text: ADR-021 is the image policy; it records that UpCloud uses a Spare Cores built
  artifact until Flatcar ships one.

## Open questions for the maintainers

1. **Who owns the UpCloud image artifact?** **Decided (ADR-021, plan 22):** the Spare Cores
   images repository (proposed `SpareCores/flatcar-images`, release assets, signed
   digests), with the upstream Flatcar proposal started alongside; the source switches to
   the official file when it ships.
2. **Hosting for `http_import`**: GitHub Releases (octet-stream, may defeat
   decompression) versus object storage with the right type versus
   `direct_upload` from the IaC run. Recommendation: `direct_upload` first (no
   hosting, the provider sets the type), object storage if runs get slow.
   Still open; the images repository (plan 22, open question 2) publishes the artifact either way.
3. **SDK**: v8 plus a thin ruleset client, or v9 (alpha) for everything.
   Recommendation: v8 plus the thin client; move when v9 is stable.
4. **Root size default**: the plan's included storage, at least 25 GB (type
   dependent, documented), or the 64 GB of the 00a proposal for every vendor.
   Recommendation: the plan's included size (25 GB minimum), because UpCloud bills
   a disk larger than the quota in full. The type-dependent default uses
   `RootDiskForType`, **decided (plan 14, A2)**, shared with plans 15 and 20.
5. **Public interface firewall**: the driver writes stateless rules (section
   "Node settings") and tolerates the 1-2 minute rollout, or the base template
   gains a guest-side firewall for vendors like this. Recommendation: the driver's
   rules now; measure in S3; a guest-side option as a later, vendor-neutral plan.
6. **`isolation: run` on UpCloud**: unsupported in v1 (infeasible offerings), or one
   SDN network per run key (account network limit, create latency).
   Recommendation: unsupported in v1.
7. **Managed network CIDR**: SCarpenter-created networks need a range that does not
   overlap a user's networks. Recommendation: a /24 hash-derived
   inside a documented `/16`, configurable.
8. **GPU plans**: drop from the catalog (recommended) or keep them selectable
   without drivers. Their spot variants stay out either way.
9. **Credentials**: a dedicated `scarpenter` sub-account per cluster (the token can
   only see its own resources, which means the IaC's templates and networks must be
   visible to it: S6) or the IaC's sub-account. Recommendation: after S6; default
   to a separate sub-account with the IaC resources shared through the main account.
10. **EUR prices**: keep Keeper's EUR rows and convert in the core, or use USD
    rows if Keeper has them for UpCloud. Recommendation: check Keeper's currencies
    at implementation; no per-vendor conversion in the driver.

## Verified and unverified

Verified from documentation or source on 2026-11-10 (no UpCloud call):

- Ignition `upcloud` platform and its URL (source file); the release containing it
  (v2.23.0); Flatcar stable's Ignition 2.24.0 and Afterburn 5.10.0, LTS's Ignition
  2.19.0 (package lists of the releases); Afterburn's UpCloud provider in 5.10.0.
- The `oem_id` to Ignition platform chain (Flatcar `build_library/grub.cfg`, bootengine
  `dracut/53ignition/ignition-setup-pre.sh` and `ignition-fetch.service`).
- The Flatcar release server's artifact list for stable (amd64 and arm64): no
  `upcloud`; `vm_image_util.sh` image types; the OEM partition procedure.
- UpCloud API pages (servers, storages, firewall, networks via SDK): create fields,
  label limits, error codes, delete-needs-stopped, import formats and modes,
  templatize rules, per-zone custom images, firewall types and their properties,
  metadata service (fields, `serial`, `metadata: yes`), server groups,
  account `resource_limits` fields (SDK).
- `upcloud-go-api` v8.42.0 contents and v9 alpha; the Terraform provider's
  `upcloud_storage` import block, `upcloud_storage_template`.
- sc-crawler's UpCloud data: plan names, zones, no ARM, spot only for GPU.

Unverified (also marked inline): the Flatcar docs having no UpCloud page; the
live boot of the patched image; GitHub Release `Content-Type`;
raw image size, import and clone times, template cost; Standard-tier templates; the
user-data size limit; rate limits and the throttling status; the 4 versus 16 storage
cap; the by-id name of a data disk; that stopped servers are billed and how the
quota counts them; how a limit of 0 is reported; labels on rulesets in the list;
sub-account visibility; the default firewall rules and the rollout window; whether
the utility network reaches the internet; exact names of the not-found error codes;
the number of servers a group may hold; BIOS or UEFI behaviour of UpCloud for this
image.

## Sources

- Ignition supported platforms and UpCloud provider:
  https://github.com/coreos/ignition/blob/main/docs/supported-platforms.md,
  https://github.com/coreos/ignition/blob/main/internal/providers/upcloud/upcloud.go,
  https://github.com/coreos/ignition/pull/2110
- Afterburn UpCloud provider: https://github.com/coreos/afterburn/pull/1227
- Flatcar release artifacts: https://stable.release.flatcar-linux.net/amd64-usr/current/ and
  https://stable.release.flatcar-linux.net/arm64-usr/current/;
  image types: https://github.com/flatcar/scripts/blob/main/build_library/vm_image_util.sh;
  the OEM `grub.cfg` hook: https://github.com/flatcar/scripts/blob/main/build_library/grub.cfg;
  `flatcar.oem.id` to the Ignition platform:
  https://github.com/flatcar/bootengine/blob/flatcar-master/dracut/53ignition/ignition-setup-pre.sh;
  customizing the image (OEM partition, `grub.cfg`):
  https://github.com/flatcar/flatcar-docs/blob/main/docs/installing/customizing-the-image/customize-the-image.md;
  notes for distributors:
  https://github.com/flatcar/flatcar-docs/blob/main/docs/installing/community-platforms/notes-for-distributors.md
- UpCloud: storage import guide https://upcloud.com/docs/guides/storage-import-api/;
  API 1.3 servers https://developers.upcloud.com/1.3/8-servers/,
  storages https://developers.upcloud.com/1.3/9-storages/,
  firewall https://developers.upcloud.com/1.3/11-firewall/;
  custom images https://upcloud.com/docs/products/block-storage/custom-images/;
  block storage billing https://upcloud.com/docs/products/block-storage/billing/;
  metadata service https://upcloud.com/docs/products/cloud-servers/metadata-service/;
  firewall https://upcloud.com/docs/products/networking/firewall/
- SDK: https://github.com/UpCloudLtd/upcloud-go-api (v8.42.0, v9 alpha);
  Terraform provider (import, templates):
  https://github.com/UpCloudLtd/terraform-provider-upcloud
- Spare Cores catalog code:
  https://github.com/SpareCores/sc-crawler/blob/main/src/sc_crawler/vendors/_upcloud.py
- Talos' UpCloud platform (a precedent for raw images and the metadata service):
  https://github.com/siderolabs/talos/tree/main/internal/app/machined/pkg/runtime/v1alpha1/platform/upcloud

## Gates

CONVENTIONS gates 1 to 4 (L3 still green: the driver must not change core
behaviour); `go test ./pkg/vendors/upcloud/...` coverage of at least 80 % of
statements (report the number); `actionlint` and `shellcheck` clean on the lane's
workflows and scripts; the lane's non-UpCloud parts run in `make
test-e2e-upcloud-dryrun`. The coordinator runs the spikes before the driver starts
and the first live L5 run after it; report the expected cost per run.
