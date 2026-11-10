# 15 — Hetzner Cloud driver (`hcloud`)

Depends on: 14 (the shared groundwork: the `images` kind, `RootDiskForType`, the
idempotency label, the stopped-node rule, account-wide quota errors), 06 (node options and
resources), 09 and 10 (packaging and the L5 lane machinery: `e2e-cloud.yml`, canary,
sweeper pattern). **Not** on an image-import plan: the image rung of this vendor is 3
(below, ADR-021). Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §11 as the model, §5.8, §5.9, §7, §8, §9, §13.4; 00a §3, §6, §9, §11;
ADR-002, ADR-013, ADR-017, ADR-018, ADR-020, ADR-021; `docs/reference/driver-contract.md`,
`docs/reference/vendor-resources.md`, `docs/explanation/vendor-capabilities.md`.

Research date 2026-11-10. Sources are the Hetzner Cloud API specification
(<https://docs.hetzner.cloud/cloud.spec.json>) and changelog
(<https://docs.hetzner.cloud/changelog>), Hetzner's docs (<https://docs.hetzner.com/cloud/>),
Flatcar's Hetzner page (<https://www.flatcar.org/docs/latest/deploy/cloud/hetzner/>), the
Flatcar release server, Ignition's supported platforms, `hcloud-go` v2.53.0 and Keeper.
Every fact I could not verify is marked **unverified** and listed again in "Unverified
facts" at the end; the L5 lane or the first implementation step settles each.

## Goal

A production driver `pkg/vendors/hcloud` (vendor ID and registration name `hcloud`):
servers through `POST /servers` with Flatcar snapshots, the project's locations as regions,
vendor-native node settings (data volumes, spread placement groups), managed and brought
resources (networks, firewalls, placement groups, volumes), an in-memory fake and the
conformance suite, the reference docs, and the opt-in L5 lane with sweeper and canary.
No Hetzner call in this plan's tests (CONVENTIONS rule 8): the lane is written and
validated statically; the coordinator runs it.

## What is different on Hetzner (read this first)

1. **No official image, no way to share one.** Flatcar publishes a Hetzner disk image, but
   Hetzner only boots its own images and snapshots of the project (rung 3, below).
2. **No zones, no quotas API, no spot.** A location is both region and (dummy) zone; limits
   are per project and visible only in the Console; every server is on-demand.
3. **No idempotency token.** `POST /servers` has none; the server name is unique per
   project, which gives the same effect (idempotency section).
4. **The root disk belongs to the server type** (20 to 960 GB, from Keeper's
   `storage_size`), so the scheduler's ephemeral storage is per type, not per NodeClass.
   The core's `Facts.RootDiskBytes(vendor, class)` has no type argument: plan 14 (A2) adds
   `RootDiskForType`.
5. **Hourly billing rounded up per started hour**, a Primary IPv4 billed separately and
   per month, volumes billed per GB-month (<https://docs.hetzner.com/cloud/billing/faq/>).
6. **Inbound is open without a firewall**, and firewalls only filter the public interface.
7. **The token is the account scope**: one project, read or read-write, no IAM, no workload
   identity.

## Image rung and why

**Rung 3** (images created in the user's own account), per Flatcar release and
architecture, **not per region**. Confidence: high. Decided by ADR-021: rung 3 now,
`ImageImporter` reserved (open question 1).

| Question | Answer | Source, confidence |
|---|---|---|
| Rung 1: a public Flatcar image offered by Hetzner? | No. Flatcar's own page: "not installable as one of the default operating system options". Hetzner's system images are its own list (Debian, Ubuntu, Fedora, ...) | Flatcar Hetzner page; high |
| Rung 2: can anyone publish a public image? | No. The API has images of type `system`, `snapshot`, `backup`, `app`; the only image action is `change_protection`; no public flag and no sharing with other accounts. Hetzner's FAQ says snapshots can be moved to another project (of the same account; how, and whether through the API, is not documented: **unverified**), which does not make them public | API spec, paths `/images`, `/images/{id}/actions`; snapshots FAQ; high |
| Rung 3: images in the user's account? | Yes, as **snapshots** of a server's disk; the API has no upload or import-from-URL | API spec; `hcloud-upload-image` README; high |
| Does Flatcar publish `flatcar_production_<platform>_image...` for Hetzner? | Yes: `flatcar_production_hetzner_image.bin.bz2` (raw disk, bzip2) with `.DIGESTS`, `.DIGESTS.asc`, `.sig` | release server, checked 2026-11-10 with HTTP requests, stable 4757.2.3 and `current` of all four channels; high |
| Arches and channels | `amd64-usr` (about 518 to 603 MB) and `arm64-usr` (about 455 to 482 MB); channels stable, lts, beta, alpha all answer 200 | same; high |
| Official Flatcar docs for the vendor? | Yes, "Hetzner" under Cloud Providers (not flagged "community support", unlike OVHcloud, Scaleway, Vultr). Methods: `hcloud-upload-image`, Packer with `flatcar-install -o hetzner`. Known limitations: no volume automount, no root password reset | Flatcar Hetzner page; high |
| Ignition platform? | `hetzner`: Ignition reads the instance user-data (`http://169.254.169.254/hetzner/v1/userdata`); cloud SSH keys are handled separately. The Flatcar image already boots with that platform ID, so no kernel command line work | Ignition `docs/supported-platforms.md` and `internal/providers/hetzner/hetzner.go`; high |
| User-data limit | 32 KiB (API field description). The 16 KiB budget of overview §8.2 holds | API spec; high |
| Import formats and size | No import API. Through `hcloud-upload-image`: raw (limited by the temporary server's root disk) or qcow2 (limited to the rescue system's 960 MB RAM file system), compression bz2, xz, zstd. The Flatcar image is raw bz2 | `hcloud-upload-image upload` help; high |
| How long, what it costs | A few minutes (Flatcar docs). A temporary server in rescue mode is created, written, powered off, snapshotted and deleted, so it bills one started hour of a small type (a cent or so, unverified). The snapshot bills per GB-month of its compressed size (the Flatcar docs show 0.47 GB x86 and 0.41 GB arm); about 0.0143 EUR per GB-month, **unverified** (sources disagree) | Flatcar page; billing FAQ; medium |
| Regional or global | **Global in the project**: "not location bound ... create a new server in any location". Bound to an architecture: x86 snapshots only for x86 types, arm for arm | <https://docs.hetzner.com/cloud/servers/backups-snapshots/faq/>; high |
| Shareable with other accounts or public? | No (see rung 2). A snapshot count limit per account exists (the FAQ names 30; **unverified** for current accounts) | API spec; snapshots FAQ; high |
| Pinned version to image ID | Labels on the snapshot, set at upload: `os=flatcar`, `flatcar-channel=<channel>`, `flatcar-version=<version>`, plus the image's `architecture`. `GET /images?type=snapshot&architecture=x86&label_selector=os=flatcar,flatcar-channel=stable,flatcar-version=4757.2.3` must return exactly one image. (`os` and `flatcar-channel` are the labels Flatcar's docs already use; `flatcar-version` is ours.) Snapshots have no `name`, so the `name` filter does not apply | API spec (`label_selector`, `architecture`, `type` filters); high |

Why not rung 4 first: the one mechanism exists as a maintained MIT tool and Go library
(`github.com/apricote/hcloud-upload-image/hcloudimages/v2`, v1.5.0 / v2.0.1 released June 2026,
repository pushed 2026-11-09; "not an official Hetzner product"), the result is one pair of
snapshots per project (not per region), and the cost is cents. An `ImageImporter` for this
vendor stays possible later with the same library (the temporary server needs outbound
access to the Flatcar release server and SSH reachability from the importer).

**How the images get created** (the plan does not build a pipeline, it documents and
checks):

- The operator's IaC, or a CI job next to it, runs `hcloud-upload-image` for the pinned
  `(channel, version)` of the clusters, both architectures, with the three labels above,
  and keeps them. Some IaC tools (Pulumi among them) have no resource that uploads an
  image; a command step or CI job is needed. Looking them up in an IaC tool is possible
  with the provider's image data source (from memory, **unverified**).
- Anyone else: the how-to `docs/how-to-guides/hcloud-images.md` (two commands, the
  Packer variant of the Flatcar docs, the label convention).
- The L5 lane's `bootstrap.sh` creates them idempotently in the CI project when the pinned
  version has none (about 5 minutes, in parallel for both architectures).

## Hand-off: `spec.vendors.hcloud`

Unit of location: Hetzner calls them locations; the API and Keeper (`region`/`zone`
tables: `fsn1`, `nbg1`, `hel1`, `ash`, `hil`, `sin`, the zone ID equal to the region)
agree. A location belongs to a **network zone** (`eu-central`: fsn1, nbg1, hel1;
`us-east`: ash; `us-west`: hil; `ap-southeast`: sin); private networks live in one zone.
Keeper also has inactive rows named after the former data centers (`nbg1-dc3`, numeric
zone IDs); the catalog already drops inactive rows, and the driver never sees them.

```yaml
spec:
  vendors:
    hcloud:
      regions: [fsn1, nbg1, hel1]        # locations; empty = every location of GET /locations
      networking: {publicIPv4: true, publicIPv6: false, inbound: {rules: []}}   # generic override
      sshKey: my-ssh-key                 # optional name or ID; default none
      storage:                           # no root settings: the root disk is the server type's
        data:
          - {name: pgdata, size: 100Gi, filesystem: xfs, mountPath: /var/lib/pgdata}
      resources:
        images:          {use: {ids: {"*": ["123456789", "987654321"]}}}   # optional (plan 14, A1, A10): default is the label lookup above (a built-in selector: tags os=flatcar and the pin, match.architecture); global snapshots, so the IDs under the key "*" (every location; both architectures)
        networks:        {use: {ids: {fsn1: ["12345"], nbg1: ["12346"]}}}
        firewalls:       {use: {tags: {scarpenter.sparecores.com/use: "true"}}}
        placementGroups: {create: {lifecycle: {policy: perUse}}}
  profiles:
    storage:   {db: {hcloud: {data: [{name: pgdata, size: 500Gi, filesystem: xfs, mountPath: /var/lib/pgdata}]}}}
    placement: {spread: {hcloud: {placementGroup: {type: spread}}}}
```

API types go in `pkg/apis/v1alpha1/hcloud/` (`Settings`, `Storage`, `Placement`; **no
`Network` profile**: Hetzner has no NIC options) plus the one line each in `vendors.go`
(`VendorBlocks`, `StorageProfile`, `PlacementProfile`, `Names`, `Get`, the `For` methods);
CRD regenerated. `Settings` implements `GetRegions`; there is no `imageOwner`.

### Resource kinds

| Kind | Scope, default policy | `use` forms | `create` | Without `use` (ADR-022) | What your IaC must output |
|---|---|---|---|---|---|
| `images` (snapshots; not managed) | region key, global in the project; per architecture | `ids` per location or under `*` (the same IDs), `tags` (labels, `key=value`; `expression` for a full label selector with `!=`, `in`, `notin`), `match.architecture` (`x86`/`arm`), `state`, `includeDeprecated`, `filters` `type`/`status`/`bound_to`; `name` matches system images only (snapshots have none); `newest` by `sort=created:desc` on the server; the label convention is the default | never (rung 3; ADR-021) | the label lookup of the image section | snapshot IDs per location (the same IDs repeated), or nothing when the labels follow the convention |
| `networks` (private network) | region (network zone of the location), `retain` | `ids` per region (location); `name` (exact: the API filter is exact; a glob is finished client-side); `tags` and `expression` (the API label selector); `match.cidr` and `within` (client-side: `ip_range`), `match.zone` (the `network_zone` of a subnet) | **not in v1** (open question 4); `create` is rejected by validation | tagged networks (discovery, planned), else no network: nodes have a public address only (no vendor default, no `create`) | the network ID per location (`use.ids`), or the label `scarpenter.sparecores.com/use: "true"` on it |
| `firewalls` | region (one per location although Hetzner firewalls are project-wide: the engine's names are per region, 6 at most; the limit is 50 per project), `idleTTL` 24 h | `ids`, `name`, `tags`, `expression` as above | yes: rules from `networking.inbound` (inbound only; outbound open), applied **at server create by ID**; deny all inbound when there are no rules | labelled firewalls (discovery, planned), else create (Hetzner has no default firewall and an open server is refused by ADR-017 and `inbound`) | the firewall ID (the same in every location; hand over only the one nodes may use), or the opt-in label on it |
| `placementGroups` (`spread`) | run, `perUse` | `use: {name}` inside a placement profile; by IDs or name in the kind block | yes, per (location, profile, run key) | n/a (per-run kinds have no default) | nothing (SCarpenter creates them) |
| `volumes` | node, `perUse`, **internal** (no block key, no `use`): one per node and `storage.data` entry | n/a | yes, from the resolved storage settings | n/a | nothing |

Notes:

- There is no subnet kind. A server created with `networks: [<id>]` gets an address from a
  subnet of that network in its location's network zone. `Resolve` of `networks` checks that
  the network has a `cloud` subnet in the zone of every configured location; a location
  where it has none is gated with the reason (resource missing). Your IaC needs no subnet
  ID for hcloud; the network's range can be matched with `use.match.cidr` or `within` (plan 14, A10).
- The firewall is applied by ID in the create call, not by label selector: a label-selector
  firewall attaches with a delay of up to a few seconds (changelog 2025-09), during which a
  new server would be open. Hetzner refuses to attach a firewall to a server without a public
  interface (`private_net_only_server`), so it is skipped for such servers (not supported in
  v1, below).
- `firewalls.use` brings the firewall as it is: `networking.inbound` rules do not apply to
  it (the same rule as AWS security groups). `networking.isolation` is in open question 3.
- Resource names: `scarpenter-<cluster>-<kind>-<region>[-hash]`, at most 63 characters
  (volume names up to 64, placement group and firewall names have no stricter limit in the
  spec). Ownership marker as labels through `TagKey`: the default `scarpenter.sparecores.com/<key>` is a
  valid Hetzner key (DNS prefix plus name). **Label values are at most 63 characters and
  `[A-Za-z0-9._-]`**: the driver maps a value that does not fit to its first 50 characters plus
  `-` plus an 8-character hash, the same function on write and on lookup (cluster,
  nodeclass and nodepool names, NodeClaim names).

### What the operator's IaC must provide, and what SCarpenter self-manages

The operator's IaC (any tool, or the Console) provides:

1. **Flatcar snapshots** (x86 and arm, per pinned `channel`/`version`, with the three
   labels), or a CI job that makes them. They are project-wide: the same IDs listed under
   every location (`resources.images.use.ids`); if SCarpenter finds them by label, nothing
   has to be passed.
2. **Optionally a private network and a firewall per location** (`networks.use`,
   `firewalls.use` by ID, name, label or, planned, CIDR). No `create` for networks in v1.
3. **The controller's own token.** A Kubernetes Secret with a read-write token of the
   project, separate from the IaC's own token, created in the Console, so it can be rotated
   apart.
4. **The e2e project and its token** (for the L5 lane): a dedicated Hetzner project
   (projects cannot be created through the API), with a low server limit requested in the
   Console, a usage alert, and two tokens (read-write for the lane, read-only for the
   canary), stored as the secrets of the `e2e`, `e2e-sweeper` and `e2e-canary`
   environments. The tailnet tags are unchanged.
5. Optional: an SSH key, the value of `sshKey`; nodes do not need it (Ignition), see the open
   question on the root password.

SCarpenter self-manages, without `use`: a deny-inbound firewall per location (`idleTTL`),
placement groups and volumes. Nothing else is needed for a node with a public address.

### Account scope and credentials

- One credential, one Hetzner project: the API token is bound to one project and is read or
  read-write (no finer permissions). `HCLOUD_TOKEN` from a Secret (the chart's value
  `hcloud.tokenSecretRef`), `HCLOUD_ENDPOINT` for tests. Nothing identifies the project in
  the API (no project ID), so `scope` stays `{}`. Hetzner has no workload identity; the
  token is long lived. Use a project that holds only SCarpenter's workload; label-based
  filtering is the only scoping SCarpenter does (it deletes only what carries its marker).
- Several projects (several tokens) in one controller are out of v1.

## Driver

- **SDK**: `github.com/hetznercloud/hcloud-go/v2/hcloud`, MIT, maintained by Hetzner, monthly
  releases (v2.53.0 of 2026-10-22), pin **at least v2.50** (the removal of the data center
  properties of servers on 2026-07-01 is in; `Server.Datacenter` is gone in v2.53.0). Built-in
  retry with exponential backoff for `rate_limit_exceeded`, `conflict`, `bad_gateway`,
  `timeout`; `WithApplication("scarpenter", version)` sets the user agent;
  `WithInstrumentation` (Prometheus) is wired to the controller registry; `exp/mockutil`
  for request-level golden tests.
- **Client layer**: a narrow `hcloudAPI` interface of the calls used, one client for the
  whole project (no per-region clients): `Location.All`, `ServerType.All`, `Image.AllWithOpts`,
  `Server.Create/GetByID/AllWithOpts/Delete`, `Action.WaitFor` (create action only),
  `Network.GetByID/AllWithOpts`, `Firewall.GetByID/AllWithOpts/Create/Delete`,
  `PlacementGroup.GetByID/AllWithOpts/Create/Delete`, `Volume.Create/GetByID/AllWithOpts/Delete`,
  `PrimaryIP.GetByID/Delete`, `SSHKey.Get`. Every call runs under its own deadline (30 s;
  90 s for the create action wait), a timeout is `Retryable` code `CallTimeout`.
- **Regions and zones**: `Regions` = `GET /locations` (cached 1 h), the zone of a region is
  the location itself: `Zone{ID: "fsn1", Name: "fsn1"}`; `topology.kubernetes.io/zone` and
  `scarpenter.sparecores.com/zone-id` both carry it. No `RegionUnavailable` (the token covers
  every location).
- **`CreateInstance`** (one `POST /servers`):

  | Field | Value |
  |---|---|
  | `name` | the NodeClaim name (also the node name), at most 63 characters, RFC 1123 (a longer name: first 50 characters, `-`, 8-character hash of the full name) |
  | `server_type`, `location` | the type's API reference (`cpx22`), the zone name |
  | `image` | the resolved snapshot ID (as a string) |
  | `user_data` | the rendered Ignition (JSON), at most 32 KiB |
  | `labels` | the logical tags (`TagKey`), the NodeClass's `spec.tags`, `scarpenter.sparecores.com/idempotency-key` = the NodeClaim UID, `scarpenter.sparecores.com/capacity-type` |
  | `public_net` | `enable_ipv4` from `networking.publicIPv4` (default true), `enable_ipv6` from `publicIPv6` (default false) |
  | `networks` | the resolved network ID, if the NodeClass has one |
  | `firewalls` | the resolved or created firewall ID |
  | `placement_group` | the group's ID, from a placement profile |
  | `volumes` | the pre-created volume IDs; `automount: false` |
  | `ssh_keys` | `sshKey`, only if set |
  | `start_after_create` | true (default) |

  After the call the driver waits for the create action to be `success` (deadline 90 s;
  a few polling requests): a failed action deletes the half-created server and returns the
  action's error code, classified as below, so the attempt loop can fall back to another
  offering. The `start` action is not awaited.
- **Idempotency** (the label of plan 14, A3): no token exists, so the name does it. A replayed key: the create fails
  with `uniqueness_error`; the driver reads the server by name, and returns it when its
  `idempotency-key` label equals the key (any zone, type, capacity type the replay asked
  for, as the contract wants). A server with that name but another key is a leftover of an
  earlier NodeClaim of the same name: `Retryable` code `NameInUse` (it is being deleted by
  the GC, which owns it). A create that fails leaves nothing, so the key is unused.
- **`GetInstance`** (`GET /servers/{id}`; the reference is `<location>/<server ID>`), **`FindInstance`**
  and **`ListInstances`** by label selector (`cluster=<c>[,nodeclaim=<n>]`): the list is one
  project-wide, paginated call (50 per page) filtered by location client-side, shared by
  all callers for 5 seconds (rate budget, below). A server the API does not know is
  `NotFound` (Hetzner has no "terminated" instances: a deleted server is gone).
- **State mapping**: `initializing`, `starting` -> pending; `running`, `migrating`,
  `rebuilding`, `unknown` -> running; `stopping`, `off`, `deleting` -> terminating. **`off`
  is terminating on purpose** (the stopped-node rule of plan 14, A4): a stopped server still
  bills (it is no node, and its charges continue into the next hours, which the deletion ends;
  the started hour is paid either way), so a node that powered itself off (the base template's optional poweroff timer) must
  be collected, and `DeleteInstance` deletes it. `CreatedAt` is the server's `created`.
- **`DeleteInstance`**: `DELETE /servers/{id}`; `not_found` is success; refused (a different
  `cluster` label, or no marker) with a clear error under broad credentials; `locked` is
  `Retryable`; `protected` is `Fatal` (someone set delete protection). Before the delete the
  driver records the IDs of the server's Primary IPs; after it, an IP that is still
  unassigned and has `auto_delete: false` is deleted (it bills monthly). Whether Hetzner
  auto-deletes the IPs it creates with a server is **unverified**: L5 asserts it, and the
  cleanup stays as the safety net (open question 6).
- **Provider ID and addresses**: `scarpenter://hcloud/<location>/<nodeclaim>`. `Instance.Addresses`:
  the public IPv4, the IPv6 address `<prefix>::1` of the server's /64 (Hetzner assigns `::1`;
  **unverified** that it is always the address the guest uses), and the private network
  IPs (`PrivateNet[].IP`). The CSR approver's IP SAN check uses these.
- **Metadata on the node**: `coreos-metadata` exposes `COREOS_HETZNER_*` attributes
  (hostname, IPs) to units; the `tailscale` template needs none. For the `direct` template,
  the kubelet's `--node-ip` is the private network IP when a network is attached, else the
  public IPv4 (a new template variable, same pattern as AWS's primary address).
- **Rate limit**: 3600 requests per hour per project, refilling one per second, burst
  allowed; headers `RateLimit-*` (<https://docs.hetzner.cloud/#rate-limiting>). Budget at
  100 nodes: one shared server list every 5 s while work is pending (at most 720/h, idle
  every 30 s: 120/h), `GET /server_types` every 5 minutes (about 24/h, 2 pages), one
  `GET /networks` and one `GET /firewalls` per resource check cycle shared by all regions
  (`Resolve` lists and filters instead of one call per region and ID: about 240/h), per
  launch about 6 calls (create, action polls, resolve), per delete 2. Well below the
  limit, but the plan's rule is: **never a per-node `GET` in a loop**; the metric
  `scarpenter_hcloud_api_remaining` (from the headers) and a warning below 20 % are part
  of the plan.
- **Boot time**: the first server created from a snapshot takes "a minute or two to load
  the snapshot" (Flatcar docs); `--launch-timeout` of the core (5 minutes) still holds.
  The first-boot expansion of the root partition to the type's disk is standard Flatcar
  behaviour, **unverified** on Hetzner: L5 asserts `df` of the root.

### Billing period (ADR-024, plan 23)

- **Rule**: hourly, rounded up per started hour; deletion is the only thing that ends the charge
  (a powered-off server bills); a monthly price cap exists (hours not published on the page read).
  Keeper has 3600/3600 and `stopped_server_charged: true`, which agrees. Retention is allowed once the
  live check below passes; until then the driver reports the rule as unverified and the feature stays off.
- **Start field**: `BillingStart` is the server's `created`. Hetzner bills once creation has
  completed, so `created` is earlier than the real start by the provisioning time (tens of seconds,
  **unverified**): boundaries computed from it fall early, the safe side.
- **Delete latency**: `DELETE /servers/{id}` returns an action; the server is gone when it completes. Whether
  billing stops at the call or at completion is not documented: the margin assumes completion plus the node drain.
- **Verify live (L5, one-time spike)**: that periods count from creation, not from the clock hour
  (create, delete at 59 and 61 minutes, read the hours billed per server on the Console's cost view or the
  invoice line); the `created` versus completion offset; delete action duration; skew of the API's clock
  against the controller's.

## Catalog (Keeper)

Keeper already has the vendor (checked 2026-11-10 through its public API): 26 active
server types (22 x86: `cx23`..`cx53`, `cpx02`..`cpx62`, `ccx13`..`ccx63`; 4 arm: `cax11`..`cax41`),
9 retired ones, six active regions equal to the locations, a `block` volume row per region
(10 to 10000 GB, about 0.049 USD per GB-month) and prices per location. Notes:

- Names: the API reference is the Keeper `api_reference` (`cpx22`), the catalog name
  `hcloud.cpx22`.
- Keeper has **no IPv4 price** and no snapshot price (same gap as AWS). `PriceDelta` uses a
  constant, 0.0007 USD per hour (0.50 EUR per month for a Primary IPv4 over 730 hours,
  at about the current rate), overridable by `SCARPENTER_HCLOUD_IPV4_PRICE`; the volume
  price from `storage_prices` (`hcloud`, `block`). Whether Keeper's server price excludes
  the IPv4 (Hetzner's server prices exclude it) is **unverified**. Traffic over the
  included amount (20 TB per server in the EU, less elsewhere) is not priced; this stays out
  of the delta (documented, no `price-incomplete`).
- **Availability** (the "stock" input): `ServerType.locations[]` of `GET /server_types` lists
  where a type is supported, with `available` (false: temporarily out of stock) and
  `deprecation.unavailable_after`. `OfferingLister.LaunchableTypes(region)` returns the types
  whose entry for the location has `available: true` and is not past `unavailable_after`,
  keyed by zone ID (= location). The quota tracker accepts a driver with only an
  `OfferingLister`. This replaces `datacenters.server_types.available`, removed on
  2026-10-01 (HTTP 410). Refresh every 3 minutes (the same call serves the type table).
- No benchmark or hardware data is needed from the driver.
- ARM (`cax`) exists only in some locations; the catalog shows that through the price rows
  and `LaunchableTypes`; the driver adds no table.
- The root disk per type is Keeper's `storage_size` (open question 2).

## Quotas, availability, errors

- **Quotas**: Hetzner has per-project limits (servers, Primary IPs, volumes, networks,
  firewalls, placement groups, ...) that are **not readable through the API**; the Console
  shows them and support raises them. No `QuotaReporter` knowledge by default: the path is
  reactive (`QuotaExceeded` on `resource_limit_exceeded`). Optionally
  `--hcloud-server-limit` (env `SCARPENTER_HCLOUD_SERVER_LIMIT`, default 0 = unknown) enables a
  `QuotaReporter` with one **global** bucket `servers` (unit `instances`, usage = every
  server of the project from the shared list, `Consumption` 1 for every type), so
  headroom is known before a launch and `QuotaAvailable` is reported (open question 8).
  A limit hit anyway is project-wide, not per location: `QuotaExceeded` makes the region
  unavailable per the tracker's rule; the other five locations get the same error once each
  (the account-wide quota error flag of plan 14, A5, fixes this: the driver sets it).
- **Capacity**: only through `GET /server_types` flags and the reactive errors.
- **Spot**: none; `capacity-type` is `on-demand` only; no spot interruption work.
- **Placement group size**: 10 servers per spread group, 50 groups, one group per server.
  The driver counts members itself (list by label) before a create and returns
  `InsufficientCapacity` code `PlacementFull` when full, like AWS's spread limit; a group
  that is full is not reused: the next run key member waits (a second group per key is
  post-v1).

Error mapping (by API `error.code`, never by message; `hcloud.IsError`). The `details`
object is read only for the field name:

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `resource_unavailable` and `placement_error` on server create; `no_space_left_in_location` on volume create; `PlacementFull` (driver) |
| `Unsupported` | `invalid_input` whose `details.fields[].name` is `server_type`, `location` or `image` (type not offered in the location, arm/x86 mismatch with the snapshot); `invalid_server_type` |
| `QuotaExceeded` | `resource_limit_exceeded` (all resource kinds; `details.limits[].name` is logged) |
| `ZoneUnusable` | `no_subnet_available`, `ip_not_available` (the network of the location is full: 100 attached resources per network, 50 subnets) |
| `Retryable` | `rate_limit_exceeded` (after the SDK's retries), `server_error`, `bad_gateway`, `timeout`, `unavailable`, `conflict`, `locked`, `maintenance`, `robot_unavailable`, `service_error`, `uniqueness_error` with a foreign idempotency key, a context deadline, `resource_in_use` on a resource delete (the dependency is still there) |
| `NotFound` | `not_found` on get and delete |
| `Fatal` | `unauthorized`, `forbidden`, `json_error`, `invalid_input` with any other field, `protected` on server delete, `deprecated_api_endpoint`, `unsupported_error`, everything else |

The mapping of `resource_unavailable`, `placement_error`, and the field names of
`invalid_input` for an unsupported location are from the API docs and from how other
controllers read them (the Cluster API provider treats `invalid_input` and
`resource_unavailable` as terminal for a server type and location); the exact responses
are **unverified** and a table-driven test pins every code; L5 provokes the cheap ones
(an arm type in a location without it, a snapshot of the wrong architecture).

## Node options and profiles (ADR-020)

- **Built-in defaults**: nothing to set for the root (the disk is the type's); public IPv4
  on, IPv6 off, no network, a managed deny-inbound firewall, no placement group. The
  fixed-root-disk fact is documented, and a profile cannot change it (ADR-017: no
  silent swap to a bigger type).
- **Storage** (`hcloud.Storage`): `data[]` with `name`, `size` (Gi, 10 GB to 10 TB; Hetzner
  counts GB, the API field is `size` in GB), `filesystem`, `mountPath` (the generic guest
  fields). **No** `root`, **no** `localStorage` (no local disks: `Supported` returns no
  profile with local storage). Each entry is a volume created in the location before the
  server (`POST /volumes` with `location`, **no** `format`: the first-boot unit formats and
  labels), passed in `volumes`, `automount: false`. Name: the engine's deterministic name
  (unique per project, 64 characters). Delete-with-server does not exist: volumes outlive
  servers, so the `volumes` kind is `perUse` with `InUse` from the volume's `server`/
  `assignee` field and the instance list. Up to 16 volumes per server.
- **Device identity**: the volume exists before the server, so `DeviceHint.ByID` is known:
  `/dev/disk/by-id/scsi-0HC_Volume_<id>` (the ordering of overview §11 and 00a §11, row
  hcloud). No serial patterns needed.
- **Network profiles**: none in v1 (no NIC options). A network profile without an
  `hcloud` block does not exist on the vendor (ADR-017).
- **Placement** (`hcloud.Placement`): `placementGroup: {type: spread}` or `{use: {name}}`.
  `cluster` (co-locate) and tenancy do not exist on Hetzner: no block, so those profile
  names are invalid on this vendor. `Supported` is the same for every type and location;
  a spread group needs a run key (like AWS). A group is created per (location, profile,
  run key) with the ownership labels, deleted when no member is left (`perUse`).
- **`PriceDelta`**: volumes from `storage_prices` per GB-month / 730; IPv4 constant;
  complete in every case (placement groups, firewalls and networks are free; Hetzner has
  no provisioned-IOPS or dedicated-tenancy dimension).
- **`Supported`**: volume profiles are valid in every location that has a Keeper `block`
  price row; the first-boot volume limit is 16 per server; the same deterministic answer
  for every type.
- **Networking generic**: `publicIPv4` and `publicIPv6` map to `public_net`; a server with
  neither is **rejected by validation in v1** (private-only servers need a NAT route in the
  user's network to reach the Flatcar sysext hosts, and whether the metadata service that
  Ignition reads answers on a private-only server is **unverified**; open question 11).
  IPv6-only is accepted but warned: `github.com` and its release hosts have no AAAA
  records (checked 2026-11-10), so the default sysext base does not work without a mirror.
  `inbound.rules` become inbound rules of the created firewall (tcp, udp, icmp; the generic
  protocol `any` becomes one rule per protocol of those three, up to 500 rules per firewall).
  `isolation`: open question 3.
- **First-hour minimum**: a server is billed for the whole started hour; the docs say so
  in the consolidation guidance (`consolidateAfter` of a few minutes does not save money
  for nodes younger than one hour). Keeping and reusing such nodes until the hour ends is
  the opt-in `spec.billing` of [plan 23](23-billing-retention.md) (ADR-024); this plan
  adds nothing for it beyond the billing period section below.
- **`NodeClassValidator`**: syntax of the block and the profile blocks (volume sizes 10 to
  10240 GB, filesystem, `sshKey` format, ID forms, regions are known locations, `networks.create`
  rejected with a pointer to open question 4, at least one public address).
- **`EpochReporter`**: the epoch changes when the server type table or the storage prices load.

## Resources (`ManagedResources`)

Kinds `networks` (use only), `firewalls`, `placementGroups`, `volumes` (internal) as in the
table above. Specifics:

- `Resolve` lists once per kind (`GET /networks`, `GET /firewalls`, labels via
  `label_selector`) and filters by ID, exact name or labels; an empty reference means no
  default: `networks` is optional, `firewalls` has no vendor default (so it is created),
  placement groups and volumes are per-run and per-node.
- `Ensure` creates with the ownership labels and the deterministic name in one call and
  finds by name plus marker first (names are unique per project for servers and volumes;
  for firewalls and placement groups the driver filters by label); `uniqueness_error` falls
  back to the lookup.
- `Delete`: `resource_in_use` (a firewall applied to a server, a placement group with
  members, a volume attached) is `Retryable`; delete order: servers, volumes, placement
  groups, firewalls (networks are never deleted: `use` only).
- `InUse`: from the instance list: a firewall's `applied_to`, a placement group's `servers`,
  a volume's `server`; `lastUsed` zero when unknown (the engine counts from its first sighting).
- Resource reads count against the rate limit: one list per kind per sweep, not per region.

## In-memory fake, conformance

`pkg/vendors/hcloud/fake` implements `hcloudAPI`: locations with network zones,
server types with per-location `available` and `deprecation`, images (labelled snapshots, both
architectures, a wrong-architecture case), servers with the status machine
(initializing, starting, running, stopping, off, deleting), unique names (`uniqueness_error`),
label selectors (equality, set and presence forms), pagination (50 per page, with short
pages), Primary IPs with `auto_delete` and the unassigned state, networks with subnets
by zone, firewalls with `applied_to`, placement groups with the 10-server limit, volumes
(location-bound, attach at create, outlive servers), project limits that raise
`resource_limit_exceeded`, `RateLimit-*` counters with a 429 mode, per-call error injection,
and servers of other owners (account-wide counting). Used by the conformance suite (the
fixture kinds `firewalls`, `placementGroups`, `volumes`; `UnknownQuotaTypes` is not needed
without a `QuotaReporter`; the unknown-quota path runs with the flag set) and by the
driver's own table tests, including a golden of the `POST /servers` body asserting every
field (through `exp/mockutil` against the real SDK, so a changed SDK schema fails too).
Every code of the error table has a test; the label-value shortening is tested for
collisions (two long names with the same prefix).

## Testing: L5 lane, sweeper, canary

Tier: the same as AWS (`docs/how-to-guides/testing-and-releasing.md`): full L5 on pull
requests that touch `pkg/vendors/hcloud/**` or `test/e2e-hcloud/**`, smoke on shared
code, a release gate, nightly canary. Cost per full run: about 8 servers for one started
hour each of the cheapest types (cents; Keeper lists the cheapest active type near 0.011
USD per hour in `fsn1`), two small volumes, snapshots kept (about 1 GB, under a cent a month).

**Files**: `test/e2e-hcloud/` with the standard scripts (`bootstrap.sh`, `mint-authkey.sh`,
`install.sh`, `scenarios.sh`, `diagnostics.sh`, `teardown.sh`, `lib.sh` with `redact`,
`defaults.sh`, `selftest.sh`, manifests, the sweeper `sweeper/main.go`), one line `hcloud` in
`test/e2e-vendors.txt`, the credentials step and region variable in `e2e-cloud.yml` and
`e2e-canary.yml`, `e2e-hcloud-sweeper.yml`, `test/canary/hcloud.go` and
`hcloud_guard.go`, added to `make test-e2e-hcloud-dryrun`.

**Identity and permissions**: unlike AWS there is no federation: the lane uses the secret
`HCLOUD_TOKEN` (read-write, dedicated CI project) of the `e2e` environment (required
reviewers, as the AWS role), the same secret in `e2e-sweeper` (deployments from `main` only),
and a **read-only** token in `e2e-canary`. Hetzner cannot restrict a token by type, location or
tag, so the cost guards are different from AWS's table:

| Guard | hcloud lane |
|---|---|
| Budget alert to a person | the project's usage alert (Console) |
| Instance-type deny | **none possible** in the token; the lane's NodePools list the types, and the project's server limit (requested low, for example 6) bounds a bug |
| Region restriction | `E2E_HCLOUD_LOCATIONS` (default `fsn1,nbg1,hel1`: arm and cheap types are there); not enforceable by the token |
| Run tag | label `scarpenter.sparecores.com/cluster=scarpenter-ci-<run id>` and `ci-created-at` on every resource |
| Sweeper | `e2e-hcloud-sweeper.yml` every 3 hours: servers, volumes, placement groups, firewalls and unassigned Primary IPs with the cluster label `scarpenter-ci-*` older than 2 hours (the label selector has no prefix match: list by key presence, filter client-side); never the Flatcar snapshots |
| Run time limit, teardown | as AWS (90 min job, 55 min scenarios, `if: always()`) |
| Read-only canary | read-only token, a transport guard that refuses every method but GET (with a test), like `aws_guard.go` |
| Approval | the `e2e` environment's required reviewers |

**Scenarios** (names as AWS where the meaning holds):

1. `amd64`: `hcloud.cx23` on-demand: node Ready, Pod Running, `kubectl logs` and `exec`,
   node labels (zone = zone ID = location), server labels and the `idempotency-key`,
   root filesystem expanded, the real user-data size, time from the create call to Ready.
2. `arm64`: `hcloud.cax11` (stands in for AWS's spot scenario).
3. `scalezero`: servers gone within 2 minutes of the Pods' deletion, no Primary IP left
   (asserts the auto-delete fact), bootstrap tokens gone.
4. `drift`: change `networking.publicIPv6` (a profile no node uses drifts nothing, one
   a node uses replaces it); the replacement has an IPv6 address.
5. `db`: a StatefulSet with a `db` storage profile (a 10 GB volume mounted at the path by
   its by-id link) and a client Deployment, both with the `spread` placement profile and
   one run key: one placement group named and labelled by SCarpenter, both servers in it,
   the volume attached, mounted and formatted by the first-boot unit; after deletion the
   group and the volume are gone.
6. `described`: a selection by `vcpus` range and a `metric-...-pct` threshold within the
   lane's types.
7. `byo`: the workflow creates a labelled network (with a cloud subnet) and firewall with the
   `hcloud` CLI; a second NodeClass `use`s them by label selector; the node has a private IP
   in that network, the firewall is applied, and both survive the run (the workflow deletes them).
8. `offerings`: the launched type's location entry in `GET /server_types` has
   `available: true`, and an offering the API lists as unavailable is not scheduled
   (the L5 analogue of AWS's quota discovery).
9. `negative`: an `arm64` type pinned to a location without it stays Pending with the
   documented reason (no launch attempt).
10. Final: nothing labelled with the run remains (servers, volumes, placement groups,
    firewalls, Primary IPs).

**Canary** (`test/canary/hcloud.go`, read-only): `GET /locations`; the lane's types are
present and not past their deprecation in a lane location; exactly one labelled Flatcar
snapshot per architecture for the pinned version (the `ResolveImage` path); the Flatcar
release server answers for `flatcar_production_hetzner_image.bin.bz2` and its `.DIGESTS` of
the pin, both architectures (added to the shared artifact checks); the API's rate-limit
headers are above a floor.

**Live verification list** (everything in this plan ran only against the fake): the items of
"Unverified facts" below, each as a scenario assertion or a one-off note in the first
report.

## Scope (what the executing model builds)

1. `pkg/apis/v1alpha1/hcloud` types, `vendors.go` lines, CRD and CEL (one of use, create,
   forbid; `networks.create` rejected; sizes), regenerate.
2. `pkg/vendors/hcloud`: client layer, driver, image resolution and `ReportImages`
   (`ImageReporter`: one snapshot per architecture, reported for every location of the
   NodeClass, `ImagesReady` False when a named architecture has none), node options,
   resources, `OfferingLister`, optional `QuotaReporter`, error mapping, label mapping,
   rate-limit metric.
3. `pkg/vendors/hcloud/fake` and the conformance fixtures.
4. Docs: `docs/reference/hcloud-driver.md` (regions, images and the label convention,
   launch table, node settings, resources, network and firewall behaviour, labels, limits,
   error table, deferred features, what is not verified), `docs/how-to-guides/hcloud-images.md`,
   the L5 section of the testing how-to, the chart values for the token, the controller
   reference (flags `--hcloud-server-limit`, env `HCLOUD_TOKEN`, `SCARPENTER_HCLOUD_*`).
5. The L5 lane, sweeper and canary as above, statically validated (selftests, `actionlint`,
   `shellcheck`, the manifests test); no run.

Gates: CONVENTIONS gates 1 to 4 (L3 unchanged), `go test ./pkg/vendors/hcloud/...`
coverage at least 80 % (report the number), `make test-e2e-hcloud-dryrun`.

Deferred: spot (does not exist), private-only servers, `networks.create`, per-run private
networks and the isolation levels that need them, several projects per controller, floating
IPs, load balancers, snapshots as node roots, an `ImageImporter`.

## Dependencies

- Plans 06, 09, 10 (done) and 14. The image-import plan is **not** a dependency (rung 3).
- Core, from plan 14: `RootDiskForType` (A2), the `images` kind (A1), the idempotency label
  (A3), the stopped-node rule (A4) and the account-wide quota flag (A5).
- The operator's IaC: the points above; none blocks the driver's tests; the lane needs the
  CI project and token and the images before its first run.

## Open questions for the maintainers

1. **Rung 3 now, or wait for the image importer?** **Decided (ADR-021): rung 3 now.** The
   images are two snapshots per project, created by a documented tool; the lane's
   `bootstrap.sh` proves the process; an `ImageImporter` with `hcloudimages` can follow
   without API change (the label convention is its contract).
2. **Per-type root disk.** **Decided (plan 14, A2):** `RootDiskForType(settings, type)` in
   `NodeOptions`, with the catalog falling back to Keeper's `storage_size` (GB, decimal) for
   `0` or unknown.
3. **`networking.isolation` on Hetzner** (the default is `run`). Hetzner firewalls filter
   only the public interface, so a shared private network (one your IaC provides) lets
   every attached node reach every other, whatever the level. Recommendation: with no
   network attached (the default), every level is satisfied (a deny-inbound firewall and no
   private path); with a `networks` block, the validator requires an explicit
   `hcloud.networking.isolation: nodeclass` and the docs say the brought network must be
   dedicated to the NodeClass; `run` with a network is rejected until per-run networks exist
   (ADR-017: never degrade silently).
4. **`networks.create`.** Recommendation: not in v1. Creating a network needs an IP range
   that does not collide with the user's own, and network and subnet are a pair per network
   zone; `use` covers per-location networks the IaC made (for example a /21 each, up to 100 attached
   servers per network, which is also the node ceiling per location: document it).
5. **Firewall default.** Recommendation: create a deny-inbound firewall per location by
   default (`idleTTL` 24 h), since Hetzner's default is open and ADR-022 says a kind
   without settings is created when nothing else resolves it.
6. **Primary IP lifecycle.** Recommendation: let Hetzner create them with the server
   (`public_net.enable_ipv4`), clean up unassigned ones after delete, and verify the
   auto-delete behaviour in L5. If auto-delete is false, make `primaryIPs` a managed
   per-node kind (labelled, created before the server) instead of the cleanup.
7. **SSH key.** Recommendation: optional `sshKey`, default none (nodes are Ignition
   configured; an injected key gives shell access). Check in L5 whether a create without
   keys has side effects for the account (Hetzner returns a root password in the response
   when no key is given and the account may get a mail; **unverified**); if it does, a key
   becomes the default.
8. **Optional server limit flag** for a synthetic global quota bucket (project limits are
   not readable). Recommendation: include, default off, so a Spare Cores deployment that
   knows its limit gets headroom-aware placement and `QuotaAvailable`.
9. **Credentials model.** Recommendation: one read-write token per project in a Secret,
   documented as full control of the project, a dedicated project per cluster or
   environment, and nothing else in v1 (no multi-project).
10. **CI project hygiene.** Recommendation: a dedicated Hetzner project for L5 with a server
    limit of about 6, a usage alert, and the sweeper as the primary leak guard; accept
    that the token cannot be narrowed (state it in the testing how-to).
11. **Private-only nodes.** Recommendation: defer; v1 requires a public IPv4 or IPv6 until
    the metadata service on private-only servers and a NAT route are verified.
12. **Label value shortening** (63-character limit; cluster, nodepool, nodeclass and NodeClaim
    names can be longer). Recommendation: the hash scheme above, with the full NodeClaim name
    also in the server name when it fits; a collision test in the plan.

## Unverified facts (settle in the first implementation steps or in L5)

- Snapshot price per GB-month (sources disagree: 0.0110 to 0.0143 EUR) and the temporary
  server's cost for an upload.
- Whether Primary IPs created together with a server have `auto_delete: true`.
- The `placement_error` and `resource_unavailable` responses for out-of-stock and for a
  full group; the `invalid_input` field names for an unsupported location or architecture;
  the `details.limits[].name` values of `resource_limit_exceeded`; the error for a network
  that is full (`no_subnet_available`).
- Whether a placement group may have members in two locations (the create body has no
  location, so the group itself is project-wide).
- The address Flatcar uses on the IPv6 /64 (`::1`), and IPv6 provisioning by the Flatcar image.
- Root partition expansion on first boot of the Hetzner image.
- Ignition's metadata fetch on a server without a public interface.
- Whether Keeper's server prices exclude the Primary IPv4 and the unit of `storage_size`
  (GB or GiB).
- Whether a pre-existing label-selector firewall attaches fast enough to matter (the
  plan avoids it).
- The `hcloud` image data source of IaC providers (for example Pulumi's `with_selector`).
- Creating a server without `ssh_keys`: account notification side effects.
- Server and project limits of a new project, and whether dedicated-vCPU types (`ccx`) have
  a separate limit.
