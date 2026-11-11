# Vultr driver

The `vultr` driver (`pkg/vendors/vultr`, vendor ID and registration name `vultr`): Vultr Cloud Compute
(`/v2/instances`) booting a pinned Flatcar from a snapshot in your own account, with a firewall group and,
for the isolations that need one, a VPC. Design: [plan 20](../plans/v1/wip/20-vultr.md), ADR-021 (the image
ladder), ADR-022 to ADR-024. The contract it implements is in the [driver contract](driver-contract.md);
the settings are in [node settings](node-settings.md); the resource engine around it is in
[vendor resources](vendor-resources.md); the selector is in [resource selectors](resource-selectors.md#vultr).

It is enabled with `--vendors=vultr` (chart: `vendors.vultr.enabled`) and needs the API key of the account
([credentials](../how-to-guides/vultr-credentials.md)). Tests run against the in-memory API of
`pkg/vendors/vultr/fake` and the shared conformance suite; nothing in the repository's tests calls Vultr
(the first live run is the opt-in L5 lane, [testing](../how-to-guides/testing.md#the-l5-lane)). Everything
marked **unverified** is a hypothesis from the public API documentation and the `govultr` source that a
live spike (plan 20, section 11) has to confirm.

## Table of Contents

- [Capabilities](#capabilities)
- [Credentials and the client](#credentials-and-the-client)
- [Regions and zones](#regions-and-zones)
- [Plans](#plans)
- [Images](#images)
- [Launch](#launch)
- [Node settings and profiles](#node-settings-and-profiles)
- [Resources and network](#resources-and-network)
- [Tags and idempotency](#tags-and-idempotency)
- [Quotas and stock](#quotas-and-stock)
- [Billing](#billing)
- [Error mapping](#error-mapping)
- [Deferred](#deferred)
- [What is not verified against Vultr yet](#what-is-not-verified-against-vultr-yet)
- [Deviations from the plan](#deviations-from-the-plan)

## Capabilities

| Interface | Vultr |
|---|---|
| `Driver` | regions, the one zone per region, `ResolveImage`, `CreateInstance`, `GetInstance`, `FindInstance`, `ListInstances`, `DeleteInstance`, `TagKey` |
| `NodeClassValidator` | syntax of the `vultr` block, what Vultr cannot do, the NodeClass's tags, the API key |
| `ImageReporter` | `status.vendors.vultr.images` (amd64 only), condition `ImagesReady` |
| `NodeOptions` | defaults, `RootDiskForType` (the plan's disk), `Supported` (nothing), `PriceDelta` (zero), `Prepare` (the firewall group and the VPC) |
| `ManagedResources` | kinds `firewallGroups` and `networks` (VPC); the `images` kind accepts `use` only |
| `OfferingLister` | the plans in stock per region |
| `NodeHardener` | the hardening `metadata-from-pods` (drops Pod traffic to `169.254.169.254`, which serves the node's user-data) |
| `BillingDescriber` | hourly, one hour minimum, stopped servers charged: **reported unverified**, so [billing retention](controller.md#billing-retention) stays off (below) |

There is no `QuotaReporter` (Vultr has no quota API: the account limits are shown in the console only) and no
`ResourceDefaults` (Vultr has no default firewall group or VPC to find).

## Credentials and the client

- `VULTR_API_KEY` from a Kubernetes Secret (chart `vendors.vultr.credentials.secret`). One key is one account:
  there is no project, subscription or tenant to name, so the block has no scope. There is no other
  credential mode (Vultr has no workload identity for callers). The key is bound to an **IP allowlist**: the
  controller's egress address must be in it.
- `Init` builds the client and makes no call, so an unreachable Vultr does not keep the controller from
  starting. The key is checked by the NodeClass controller: `ValidateNodeClass` calls
  `GET /v2/instances?per_page=1` (cached 5 minutes, a refusal for 1 minute) and a refused key (401, 403) is
  `VendorReady=False` with the reason. Throttling or an outage says nothing about the key and gates nothing.
- One global client (one endpoint, no regional clients): user agent `scarpenter/<version>`, a client-side limit of
  10 requests per second (Vultr's is 30 per second per source IP, HTTP 429 above it), per-call timeouts (45 s,
  90 s for a create) and `govultr`'s three retries of 429 and 5xx.
- `SCARPENTER_VULTR_AVAILABILITY_TTL` (chart `vendors.vultr.availabilityTTL`, default `5m`) is how long the stock
  of a region is cached. It is the driver's cache under the offering refresh of the quota tracker.

## Regions and zones

- `Regions` is `GET /v2/regions` (public, the same for every account), cached for an hour. The NodeClass's
  `vendors.vultr.regions` is the allow-list; empty means every region the API lists.
- Vultr has no zones. The zone of a node is its region (Keeper's zone ID equals the region ID, `ewr`), and so is
  the value of `topology.kubernetes.io/zone`.
- Provider ID `scarpenter://vultr/<region>/<nodeclaim>`; instance reference `<region>/<instance UUID>`. A
  reference whose ID is not a UUID is `NotFound` without a call.

## Plans

The plan is Keeper's `api_reference` (`vc2-2c-4gb`). The driver launches the families `vc2`, `vhf`, `vhp` and
`voc`, and `vx1` only with a local disk (a diskless plan boots from a block storage volume: v1 does not). Cloud GPU
(`vcg`), GPU bare metal (`vdm`), bare metal (`vbm`) and the free plan `vc2-1c-0.5gb-free` are not launched; a
family nobody classified is unsupported, and a test fails when a family of the committed Keeper snapshot has no
classification. Vultr has no Arm instances: an arm64 type or image is `Unsupported`.

The root disk is the plan's disk (`RootDiskForType`, Keeper's `storage_size` in GB, read as 10^9 bytes). It is not
configurable; swapping to a bigger sibling plan is forbidden by
[ADR-017](../explanation/DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05).

## Images

Rung 3 of the [image ladder](../explanation/DECISIONS.md#adr-021-flatcar-images-the-image-ladder-accepted-2026-11-10):
Flatcar publishes no Vultr image and Vultr snapshots cannot be shared between accounts, so **you create the
snapshot in your account once per pinned Flatcar version** (or have your tooling do it), from the public URL
of the image the images repository builds for Vultr (Flatcar with the OEM id `vultr`: Ignition reads
`/user-data/user-data` from the metadata service). SCarpenter never creates or deletes a snapshot. Create it with
the description `flatcar-<channel>-<version>-amd64` (for example with `vultr-cli snapshot create-url`, or a
snapshot-from-URL resource of your IaC tool); the import is global in the account, billed at $0.05 per
GB-month of the compressed size.

`ResolveImage` is the shared `images` selector, finished by the shared matcher:

| Selector | Maps to | Where |
|---|---|---|
| none (the default) | the description `flatcar-<channel>-<version>-amd64`, state `complete`, exactly one | description hint on the server, exact compare on the client |
| `ids` (`"*"` or a region key) | the snapshot ID | client (one list call) |
| `name` | the description, a glob with `{channel}`, `{version}`, `{arch}`, `{region}` | server hint if it has no wildcard, client |
| `match.state` | `status`: `complete` is `available`, `pending` is `pending`, anything else `failed`; default `[available]` | client |
| `match.createdAfter`, `createdBefore`, `orderBy: created` | `date_created` | client |
| `match.architecture` | only `amd64` is accepted | client |
| `filters` | `os_id`, `app_id`, `size`, `compressed_size` (exact, values ORed) | client |
| `tags`, `match.owner`, `expression` | not supported (snapshots have no tags, there is no owner) | rejected |

The chosen snapshot must **say** it is the pinned release, by its description: `flatcar-<channel>-<version>-amd64`,
optionally followed by a space and a note. Another release is `ImageVersionMismatch`, a description that says
none `ImageVersionUnknown`, and the image is never booted
([ADR-017](../explanation/DECISIONS.md#adr-017-never-degrade-an-option-silently-accepted-2026-11-05)).
Several complete snapshots with the description are `ImageAmbiguous`; none is `ImageNotFound`. The answer is
cached for an hour (a miss for five minutes), and a failing list serves the last good answer. Snapshots are
global, so one lookup serves every region.

Vultr's own `Flatcar Container Linux ... x64` OS entries (rung 1) are **not used**: they cannot be pinned or
read back, which ADR-017 rules out.

## Launch

`POST /v2/instances` with `region`, `plan`, `snapshot_id`, `label` and `hostname` (the NodeClaim name),
`user_data` (the rendered Ignition, base64), `firewall_group_id` and `attach_vpc` from `Prepare`, `enable_ipv6`
from `networking.publicIPv6`, `backups: disabled`, no activation e-mail, no DDoS protection, no SSH keys (Ignition
installs them). The response's `default_password` is never read, logged or stored. The user-data budget is 16 KiB
(Vultr documents no limit: unverified); a larger one is `Fatal` before any call.

- **Idempotency.** Vultr has no create token, so the NodeClaim UID is written as the tag `idempotency-key`
  and every create is preceded by an account-wide lookup of it. A hit is returned whatever region and plan the
  replay asks for. A create the server accepted but whose response was lost is found by the same lookup once
  the list shows it (eventual consistency: unverified), a cache of the last 10 minutes of creates covers the
  gap inside one process (confirmed with a get before it answers), and the garbage collector's orphan sweep is
  the backstop.
- **Stock.** The plan must be in the region's `available_plans` (cached, see above) before the call; otherwise
  `InsufficientCapacity` without a call.
- **State.** `status: pending` is `pending`; `suspended` or `power_status: stopped` is `terminating` (a stopped
  instance still bills, so the garbage collector deletes it); `active` with `running` and `server_status: ok`
  is `running`; anything else is `pending`. `main_ip` is `0.0.0.0` while pending and is then no address.
- **Delete** is `DELETE /v2/instances/{id}`; 404 is success; an instance without the cluster's tag is refused.
  Whether an installing instance can be deleted is unverified; a refusal is `Retryable`.
- **List and find** are one paginated, account-wide call (`per_page=500`) with the server-side `tag` filter of
  one tag, the rest client-side: never one call per region.

## Node settings and profiles

The vendor block has no node settings of its own in v1: the defaults are the plan's disk, a public IPv4 address,
no IPv6, a firewall group (deny all inbound unless `networking.inbound` says otherwise) and, with an isolation
that needs one, a VPC. `PriceDelta` is zero and complete (the public IPv4 is in the plan price; reserved IPs and
block storage are not used).

`Supported` returns nothing: a **storage, network or placement profile with a `vultr` block makes the offerings
that select it unavailable** with the reason (ADR-017); the blocks are empty structs in v1.

## Resources and network

| Kind | Scope | `use` | `create` | Default |
|---|---|---|---|---|
| `images` | account (global) | see [Images](#images) | never | the description pattern |
| `firewallGroups` | one managed group per NodeClass and region (the object is account-wide) | `ids` (`"*"` is every region), `name` (description glob), `match.createdAfter` / `createdBefore`, `filters` (`instance_count`, `rule_count`), `orderBy: created`; default select `exactlyOne` | yes: a group with the rules of `networking.inbound` (none: deny all); `idleTTL` 24 h | created |
| `networks` (VPC) | region | `ids` per region, `name` (description glob), `match` (`cidr`, `within`, `createdAfter`, `createdBefore`); default select `exactlyOne` | yes: a VPC per NodeClass and region; `idleTTL` 24 h | created when the isolation needs one |

- **No tags anywhere but on instances.** Snapshots, firewall groups and VPCs have a `description` and no tags: `use.tags`
  is rejected on every kind, and there is no opt-in-tag discovery. An IaC can mark its objects in the description
  (`<env>-<thing>`), which `use.name` matches.
- **The ownership marker of a managed resource** (ADR-018) is the name prefix `scarpenter-<cluster>-<kind>-<region>[-<hash>]` plus a
  marker in the description:
  `<name> | cluster=<c> nodeclass=<n> managed-by=scarpenter resource-kind=<k> region=<r>`. Both name and marker are checked before the
  driver finds, adopts or deletes anything; a group or VPC of that name without the cluster's marker is never adopted.
  The region is in the marker because a firewall group has no region. The NodeClass's `spec.tags` cannot be put on these
  objects (no tags). The description's length and charset limits are unverified.
- **Firewall groups.** A rule of the generic `inbound` becomes one rule per CIDR and family; `any` becomes `tcp`, `udp` (all
  ports) and `icmp`, because Vultr has no all-protocols rule. A group takes at most 50 rules (a NodeClass with more is invalid).
  `Ensure` syncs the rules of an existing managed group (creates the missing ones, deletes the others): a group whose creation
  stopped halfway is completed. Inbound is open without a group, so every instance gets one. `InUse` is `instance_count > 0`
  (the last use is unknown: the garbage collector counts idle time from its first sighting); `Delete` is `Retryable` while an
  instance holds the group. Whether a group filters the public interface only, and whether an empty group drops all inbound, is
  unverified (S6).
- **VPCs.** `InUse` is `ListVPCAttachments`; `Delete` is `Retryable` while attached. Vultr picks a free range when none is given
  (unverified). A firewall group filters the public interface (unverified), so a VPC is the private boundary, not the firewall.
- **`networking.isolation`.** `none` attaches no VPC. `nodeclass` and `run` put every node of the NodeClass in the region's VPC
  (brought with `use`, or created and owned): Vultr has no placement profiles, so there is no run key and a run is the whole
  NodeClass. The CRD's default isolation is `run`, so **a NodeClass that says nothing gets a managed VPC per region**; set
  `networking.isolation: none` to attach none. `isolation: none` with a `networks` that is brought or created, and `forbid` with
  another isolation, are invalid (the VPC would be ignored or required).
- **IPv6.** `publicIPv6: true` sets `enable_ipv6` at create (a /64 on the public interface). **`publicIPv4: false` is refused** (it needs a
  VPC-only instance behind a NAT gateway, which v1 does not manage): the NodeClass is not ready, with the reason.

## Tags and idempotency

Instance tags are plain strings. A logical tag is the string `scarpenter.sparecores.com/<key>=<value>`, the NodeClass's own tags are
added as `key=value` (logical tags win; a key with `=` or the reserved prefix is invalid). The charset and length limits of tags are
unverified (S6); if `/` or `=` is refused, the fallback style is `scarpenter-<key>=<value>`. The logical keys are `cluster`, `nodeclaim`,
`nodepool`, `nodeclass`, `managed-by` and `idempotency-key`.

## Quotas and stock

No quota reader: the account's limits (maximum instances, maximum instance cost) are visible in the console only, so every
type has an unknown quota and the reactive path applies. A limit hit on a create is `QuotaExceeded` marked **account-wide**
(the core then gates the capacity type in every region, as the limits are the account's). Stock comes from
`GET /v2/regions/{id}/availability` (`available_plans`), which is both the pre-call check and the `OfferingLister`. Vultr has no spot
instances for cloud plans, so there is no capacity type but `on-demand`.

## Billing

Vultr bills a started hour (the minimum billing unit is one hour; charges begin at deploy and a powered-off server is charged until it is
destroyed), up to a monthly cap of 672 hours. `Instance.BillingStart` is `date_created`. The driver reports the rule as **unverified**:
whether the started hour is billed in full on destroy, whether periods count from the start rather than the clock hour, and how far
`date_created` lies from the real billing start (a later stamp is the unsafe side) have to be measured live (S7) before retention is
enabled, so `billing.retainUntilPeriodEnd` has no effect on Vultr and the NodeClass status says `Unverified`.

## Error mapping

Vultr errors have no code: the body is `{"error": "<text>", "status": <n>}`, and `govultr` drops the status from its errors. The client's
transport records the status of each call and the error carries it. Classification is the documented exception to "never by message text"
([driver contract](driver-contract.md)): status first, then the texts below, only with a known 4xx status; an unmatched text is `Fatal` and a
text with no status decides nothing.

| Class (code) | Rule |
|---|---|
| `Retryable` (RateLimited, ServerError, CallTimeout, CallCanceled, TransportError, RetriesExhausted) | 429, 5xx, a canceled or timed out call, a network error, `govultr`'s "gave up after" text |
| `NotFound` | 404 on get or delete; 404 on a create is `Fatal` (a referenced snapshot, group or VPC is missing) |
| `RegionUnavailable` | 404 or 400 on a region's availability |
| `InsufficientCapacity` (OutOfStock, PlanUnavailable) | the plan is not in the region's availability (before the call), or a create text such as "currently unavailable", "out of stock" |
| `Unsupported` (Incompatible, NoSuchInstances, PublicIPv4Required) | create texts such as "invalid plan", "not compatible"; arm64; `publicIPv4: false` |
| `QuotaExceeded` (AccountLimit, ResourceLimit), account-wide | create texts such as "account limit", "maximum instance cost", "spending limit"; group and VPC limit texts |
| `ZoneUnusable` (RegionUnusable) | a create text such as "invalid region": the region is the zone, and it cannot be used at all |
| `Retryable` (Dependency, Locked) | a delete of a group, VPC or instance that is still used, attached or installing; 409 or 423 on a delete |
| `Fatal` | anything else, an invalid `user_data` included |

Every text is a hypothesis pinned by `errors_test.go` until spike S4 records the real ones.

## Deferred

Bare metal and GPU plans; block storage volumes and `data` profiles; VPC-only nodes behind a NAT gateway (`publicIPv4: false`); a VPC per
run key (`isolation: run` with a run key); reserved IPs; backups; DDoS protection; spot (`deploy_preemptible` exists only on GPU bare
metal); VX1 diskless plans; a `QuotaReporter` if Vultr ever exposes limits; `ImageImporter` (the driver creating the snapshot from the
images repository's URL).

## What is not verified against Vultr yet

Plan 20 section 11 lists the spikes; the L5 run needs them first. In short: the import of the patched image (accepted formats, size cap,
time, BIOS or UEFI) and that Ignition runs with the OEM id `vultr`; that the snapshot deploys on every plan family; the user-data size
limit; the real error messages and statuses; tag list consistency right after a create; the ACLs the key needs; the charset and length of
tags and descriptions; what a firewall group filters; the VPC range chosen without a subnet; and the billing facts above.

## Deviations from the plan

- `govultr` v3.34.0's `Plan` has no `storage_type` and no per-location cost, and `PlanAvailability` has only `available_plans`. The vx1
  diskless check therefore uses the plan's disk (Keeper's `storage_size`, 1 for diskless).
- `govultr`'s `OnRequestCompleted` hook is client-wide and sees `nil` after retries run out, so the status is recorded by a transport keyed by
  a per-call context value instead.
- `govultr` flattens a canceled or timed-out call into the text "gave up after N attempts, last error : ..."; the client restores the
  context error from the call's context so `errors.Is` works.
- `govultr`'s `SetRateLimit` is the retry backoff, not a limiter; the rate limit is the client's own.
- `InstanceCreateReq` always serializes `"block_devices": null` and `"tags"`; whether the API accepts a null `block_devices` is unverified (add to S3).
- The region is a fifth field of the ownership marker (a firewall group has no region field).
- An invalid region on a create is `ZoneUnusable` (the plan has none: the shared conformance suite exercises the class, and for a
  zoneless vendor the region is the zone).
- `isolation: run` and `nodeclass` both put the nodes in a VPC, and a NodeClass that leaves isolation at its default gets a managed VPC: the
  plan's section 3.2 says "unavailable" and section 3.4 says "a managed VPC", and the second is taken.
