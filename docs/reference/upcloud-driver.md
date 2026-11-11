# UpCloud driver

The UpCloud driver (`pkg/vendors/upcloud`, vendor ID and registration name `upcloud`): cloud
servers created by plan name from a Flatcar template the operator imported, regions that are
zones, the Public & Utility firewall written for every server, SDN networks, rulesets and server
groups as managed resources, and quotas read from the account. Design: plan 19, ADR-002,
ADR-017, ADR-018, ADR-020, ADR-021. The contract it implements is in the
[driver contract](driver-contract.md); the settings it reads are in [node settings](node-settings.md);
the resource engine around it is in [vendor resources](vendor-resources.md) and the `use` selector in
[resource selectors](resource-selectors.md#upcloud). Nodes run Flatcar, configured by Ignition from
UpCloud's metadata service and joined over Tailscale like on AWS.

It is enabled with `--vendors=upcloud` (the chart's `vendors.upcloud.enabled`). Tests run against the
in-memory UpCloud of `pkg/vendors/upcloud/fake` and the shared conformance suite over the UpCloud
plans of the committed Keeper snapshot; nothing in the repository's tests calls UpCloud (the first
live run is the L5 lane, [the UpCloud lane](../how-to-guides/testing.md#the-upcloud-lane)).
**Nothing in this driver has run against UpCloud yet**; the [last section](#what-is-not-verified-against-upcloud-yet)
lists every fact that is unverified.

## Table of Contents

- [Capabilities](#capabilities)
- [Credentials](#credentials)
- [Regions and zones](#regions-and-zones)
- [Images](#images)
- [Launch](#launch)
- [Firewalls](#firewalls)
- [Node settings](#node-settings)
- [Resources and network](#resources-and-network)
- [Names, labels and references](#names-labels-and-references)
- [Billing](#billing)
- [Quotas](#quotas)
- [Error mapping](#error-mapping)
- [Permissions](#permissions)
- [Deferred](#deferred)
- [What is not verified against UpCloud yet](#what-is-not-verified-against-upcloud-yet)

## Capabilities

| Interface | UpCloud |
|---|---|
| `Driver` | zones as regions, `ResolveImage`, `CreateInstance`, `GetInstance`, `FindInstance`, `ListInstances`, `DeleteInstance`, `TagKey` |
| `NodeClassValidator` | syntax of the `upcloud` block and of the UpCloud blocks of the profiles; the `lts` channel is refused |
| `ImageReporter` | `status.vendors.upcloud.images`, condition `ImagesReady` (amd64 only) |
| `NodeOptions`, `EpochReporter` | defaults, `RootDiskForType`, `Supported`, `PriceDelta`, `Prepare` with device hints; the epoch changes when the plan table, the templates or the prices load |
| `ManagedResources` | kinds `networks`, `firewalls`, `serverGroups`; `images` is `use`-only |
| `QuotaReporter`, `OfferingLister` | account-wide buckets; every supported plan in every zone |
| `BillingDescriber` | hourly, unverified: [billing retention](controller.md#billing-retention) stays off |
| `ArchNamer`, `NodeHardener` | `{vendorArch}` (amd64 only); the hardening `metadata-from-pods` |

## Credentials

An API token (a bearer token) in `UPCLOUD_TOKEN`; the chart's `vendors.upcloud.credentials.mode: secret`
reads it from an existing Secret (`secret.existingSecret`, key `tokenKey`, default `UPCLOUD_TOKEN`), and `none`
wires nothing (supply the variable with `extraEnv`). UpCloud has no workload identity or OIDC federation, a
token has no scopes, and tokens expire (a sub-account's last up to a year): rotate the Secret before that and
restart the controller. The driver does not start without a token. Everything lives in the account of the token
(there is no project): region limits are account limits, and the token must see the templates, networks and
rulesets your IaC created (**unverified** across sub-accounts).

- `Init` builds the client and makes no call. One client serves every zone (the API is global; the zone is a
  parameter of the calls). The user agent is `scarpenter/<version>`.
- **Per-call deadlines**: 45 seconds, 90 for the create call. The v8 SDK retries nothing except its state
  pollers (5xx, three tries), so every other failure of a call is classified and the callers back off.
- The SDK is `upcloud-go-api/v8`. It lacks the standalone firewall rulesets, so three endpoints
  (`/firewall-ruleset`, and `attach` and `detach_firewall_ruleset/private` of a server) are written over the
  SDK's HTTP client, from the OpenAPI-generated v9 module (alpha). They have never been called.
- The driver reads storage and IPv4 prices from Keeper's `/storage_prices` and `/ipv4_prices` in USD (`--keeper-url`),
  in the background from the first read of the regions and every six hours. Until they are loaded, and for what
  Keeper lacks, the price of an offering is incomplete. Currency is Keeper's: the catalog and the driver both ask for
  USD, nothing is converted here.
- `SCARPENTER_UPCLOUD_NETWORK_RANGE` (default `10.241.0.0/16`): the /16 of the managed networks, see
  [resources](#resources-and-network).

## Regions and zones

UpCloud's regions are its zones (`fi-hel1`, `de-fra1`, ...): there are no availability zones inside a data
centre, and Keeper has a region and a dummy zone with the same ID. `Regions` is `GET /zone` (the public zones and
the account's private ones), cached for an hour; `Zones(region)` is the one zone whose ID and name are the region;
a region the account cannot use is `RegionUnavailable`. The `regions` of the block are zones.

## Images

Rung 3 of ADR-021: UpCloud has no public Flatcar template and nothing can be shared across accounts, so the
operator imports the Spare Cores images repository's `flatcar-upcloud-<channel>-<version>-amd64.img.gz` into a
storage and makes a template of it in every zone and storage tier
([how to](../how-to-guides/import-flatcar-templates-into-upcloud.md)). SCarpenter only resolves and reports them;
it never imports. The LTS channel is unsupported until an LTS release ships Ignition 2.23 or later (the `upcloud`
platform); `NodeClassValidator` refuses it.

- **Convention.** Without `resources.images`, a template is the image when its labels say `scarpenter-image=flatcar`,
  the pinned `flatcar-channel` and `flatcar-version`, and `flatcar-arch=amd64`, and it is `online`. The title
  convention is `flatcar-<channel>-<version>-amd64-<tier>`. With a `use`, the selector chooses among the account's
  private templates of the zone ([selectors](resource-selectors.md#upcloud)); whatever chooses, the chosen template's
  labels must say the pinned release (ADR-017): another release is `ImageVersionMismatch`, none `ImageVersionUnknown`.
- **Tiers.** A clone keeps the tier of its template. A selector that matches one template per tier is fine: the
  MaxIOPS one is the resolved image, and the launch takes the sibling of the same release in the root tier it needs
  (`storage.root.tier`, else the plan's included tier where such a template exists, else the resolved template's tier).
  A tier with no template is `Unsupported` with the label to import. Two templates of one tier under the default
  select are ambiguous.
- `ReportImages` lists the account's templates once and applies the selector per zone: a zone without a template is
  not ready, with the zone named and "import the Flatcar template for <version>".
- The template's size is the image's root disk size. amd64 only: without a `use`, an `arm64` request is `Unsupported`.

## Launch

`CreateInstance` is one create call (`plan`, `zone`, the template as `action: clone`), with:

- `hostname` and `title` the NodeClaim name; `metadata: yes` (Ignition reads the user-data from the metadata service,
  which is off by default); `user_data` the Ignition config as the raw body (at most 16 KiB, the renderer's budget;
  UpCloud's own limit is unverified); `firewall: on`; no one-time password (`password_delivery: none`,
  `login_user.create_password: no`); `timezone: UTC`; no simple backup; remote access off.
- **Storage**: the root a clone of the template at the resolved size, tier and `encrypted`; data volumes `action: create`
  in the same call. Every storage carries the labels `scarpenter-cluster`, `scarpenter-nodeclaim` and
  `scarpenter-managed-by`, set in the create body (the SDK has no field for them; the driver's client adds them), so no
  storage of a node exists unlabelled. The driver takes at most four storage devices per server (the API's table says
  16, an error text says "more than four"; S3 decides).
- **Interfaces**: `public` with IPv4 (and IPv6 with `publicIPv6`), `utility` always, and `private` on the resolved SDN
  network when the node has one.
- **Server labels**: the logical tags as `scarpenter-<key>`, `scarpenter-idempotency-key` (the NodeClaim UID),
  `scarpenter-created` (RFC 3339, the creation time) and the NodeClass's own tags.
- **Group**: the server group of a placement profile is a field of the create call.

After the call returns (the server is in `maintenance` while its storage is cloned) the driver writes the Public &
Utility firewall and, with a private interface and a ruleset, attaches the ruleset and switches the private firewall
on. A server whose steps fail is deleted, so the key stays unused.

- **Idempotency.** UpCloud has no create token. The server carries `scarpenter-idempotency-key=<NodeClaim UID>`; before
  a create, and after a failure that may have lost the response, the driver looks the label up (one list call across zones)
  and returns the server whatever zone or plan the replay asks for. A replay also rewrites the firewall when the server
  has none: the first attempt may have stopped between the create call and the firewall write, and a node must not run
  on the account's default rules. Servers the lookup finds with no storage (a create that failed half way) are deleted.
- **State**: `started` is `running`; `maintenance` (cloning, or a stop in progress) is `pending`; `stopped` and `error`
  are `terminating` (the stopped-node rule: a stopped server is not coming back and may be billed, the garbage collector
  deletes it).
- **Delete**: UpCloud deletes only a stopped server. The driver hard stops it, waits for `stopped`, then deletes it with its
  storages and backups. A gone server is success; a server in `maintenance` is `Retryable`. A server that does not carry
  the cluster's `scarpenter-cluster` label is never deleted (the token is the account's).
- **Addresses**: public IPv4 and IPv6 first, then the utility and any SDN address, for the serving-certificate approver.
- **Get, list**: `GetInstance` is `NotFound` for a deleted server (there is no terminated state) and for a reference that is
  not `<zone>/<server uuid>`. A list answer has no labels, so `ListInstances` and `FindInstance` read the details of each
  match (SCarpenter's servers only: the `scarpenter-managed-by` label).
- **Plans**: the server is the plan name (Keeper's `api_reference`). GPU plans and families the driver has not classified are
  not launched (`LaunchableTypes` leaves them out, and a launch is `Unsupported`); spot exists only for GPU plans, so UpCloud
  is on-demand only.

## Firewalls

UpCloud has two. The **Public & Utility** firewall is per server, stateless, limited to 1000 rules and cannot be shared, so the
driver writes it for every node, replacing the whole list (rollout takes one to two minutes; the account's default rules
apply until it does). The first match wins:

1. accept ICMP and ICMPv6, DHCP and DHCPv6 replies;
2. accept the NodeClass's `networking.inbound` rules (one rule per CIDR);
3. accept UDP 41641 (Tailscale's direct paths; opened for every node, whatever the network template);
4. accept TCP and UDP to the ephemeral range 32768-60999, both families (replies to the node's own connections);
5. drop everything else inbound; outbound is accepted.

The **SDN** firewall (standalone rulesets) only filters private interfaces, so a ruleset you bring never covers the public
interface. Without one, an attached private interface is unfiltered (the SDN default). The rules are an iteration target
of the L5 lane, not a final list.

## Node settings

`spec.vendors.upcloud` and the UpCloud blocks of the storage and placement profiles ([API](api.md)). There is no network
profile: `nicModel` exists and has no use (ADR-017).

| Setting | Value |
|---|---|
| `storage.root.tier` | `maxiops` or `standard`. Default: the plan's included tier where a template of it exists, else the template's tier. The one type-dependent default of the driver, like GCP's Hyperdisk series. |
| `storage.root.size` | 10 to 1024 (UpCloud's GB are read as GiB). Default: the plan's included storage, at least 25 GB. A disk larger than the plan's is billed in full, so the default is the plan's. The scheduler's ephemeral storage is this size (`RootDiskForType`). |
| `storage.root.encrypted` | default `true` (free) |
| `storage.data[]` | `name`, `filesystem`, `mountPath`, `tier` (`maxiops`, `standard`, `hdd`), `size`, `encrypted`; at most three; created in the create call and deleted with the server. The first-boot unit finds the device by the virtio serial pattern `*` and the size (the UpCloud serial is the first 20 hex digits of the storage UUID, unknown before the create), so equal-size disks rely on the unit's lock. |
| `localStorage` | not supported: every plan disk is network storage |
| placement profile | `serverGroup: {antiAffinity: strict \| yes}` or `{use: {name}}`; one group per (zone, profile, run key); `strict` fails at start with no capacity when no other host is free. There is no cluster or co-location construct and no dedicated tenancy: profiles that ask for them have no UpCloud block. A profile that creates groups needs a run key. |
| `networking.publicIPv4` | default `true`; `false` needs a brought private network with a gateway (the utility network has no internet access, **unverified**), else the NodeClass is refused |
| `networking.publicIPv6` | an IPv6 address on the public interface (UpCloud has IPv6 on public interfaces only) |
| `networking.isolation` | `none`: no private network. `nodeclass`: the managed (or brought) SDN network and ruleset. `run` is the API default: it is fine without a run key (the nodes of the NodeClass are one run, with no private network), and `Unsupported` with one, because it would need one SDN network per run key: a NodeClass that uses run keys (placement profiles) sets `none` or `nodeclass`. |

`Supported` (per plan and zone): a storage profile whose root tier has no template in the zone is left out once the zone's
templates are known (until then it is offered, and a launch that finds none is `Unsupported`); sizes within 10 to 1024 GB and at most
four devices; every placement profile with a server group. `PriceDelta`: a root disk larger than the plan's included storage, of
another tier, or on a plan with none (Cloud Native) at Keeper's GB-month price of the tier divided by 672; data disks the same;
the public IPv4 address of Cloud Native plans (the others include one). It is incomplete when a price is missing, and while the
plan's included tier is not known and an explicit tier is set.

## Resources and network

Kinds a node needs, per [vendor resources](vendor-resources.md): UpCloud has **no vendor default** for any, and a node has a
private interface only where `resources.networks` is configured (any form but `forbid`) or `networking.isolation` is
`nodeclass`. The driver reads the NodeClass named by the launch's tags to know (the settings of a launch do not carry the
resource configuration).

| Kind | Scope | Notes |
|---|---|---|
| `images` | zone and tier | `use` only; see [images](#images) |
| `networks` | zone | an SDN private network. Managed: IPv4 /24 inside the `SCARPENTER_UPCLOUD_NETWORK_RANGE` /16, derived from the name and moved on while it overlaps a network of the account, DHCP on, no default route, no router. `idleTTL`. The account's network limit applies (`NETWORK_LIMIT_REACHED`). |
| `firewalls` | zone (the ruleset is account-wide; one managed ruleset per zone by name) | a stateful SDN ruleset: the NodeClass's inbound rules, everything from the network's range with `isolation: nodeclass`, drop inbound, accept outbound. Attached to the private interface only. `idleTTL`. |
| `serverGroups` | run | anti-affinity, `perUse` per (zone, profile, run key), policy `strict` by default |

Names are `scarpenter-<cluster>-<kind>-<zone>[-hash]`; the marker is the labels `scarpenter-cluster`, `scarpenter-nodeclass`,
`scarpenter-managed-by` and `scarpenter-resource-kind`, plus `scarpenter-region` for the account-wide kinds. Delete order:
server groups, rulesets, networks (a ruleset still attached or a network with an interface is `Retryable`; a server group with
members is refused). A resource without the cluster's marker is never deleted. `InUse`: a network while a server has an interface
on it, a group while it has members, a ruleset while a server of the cluster in the zone has a private interface (the API does not
say which servers a ruleset is attached to).

## Names, labels and references

- Provider ID `scarpenter://upcloud/<zone>/<nodeclaim>`; instance reference `<zone>/<server uuid>`.
- Label keys are 2 to 32 printable ASCII characters (`scarpenter.sparecores.com/idempotency-key` has 33), so the logical tags
  are `scarpenter-<key>`; `scarpenter-use: "true"` is the opt-in label of the operator's resources. `spec.tags` keys must be valid
  label keys, must not start with `_` or `scarpenter-`, and at most 255 labels fit.
- `scarpenter-created` holds the creation time and is what `BillingStart` reads; a server without it has no billing start.

## Billing

UpCloud bills cloud servers per hour, a month capped at 672 hours. Keeper has the same (3600 s increment, stopped servers
charged). The driver reports the rule **unverified**: whether the hour counts from creation or from the first start, whether it is the
clock hour, and whether a server in `maintenance` is billed are not known, so [billing retention](controller.md#billing-retention)
stays off for UpCloud and the NodeClass status says so, until the live check of plan 23 (the billed hours of a server deleted at 59
and 61 minutes) is recorded. `Instance.BillingStart` is the creation label, written in the create call, so before the server exists: the
safe side. The delete latency (stop, wait for `stopped`, delete) is the longest of the hourly vendors: the rule reports the stop deadline,
two minutes, until the lane measures it.

## Quotas

Every limit is account-wide (`GET /account` `resource_limits`) and reported as a global bucket: `cores`, `memory` (MB),
`storage-maxiops`, `storage-standard`, `storage-hdd` (GB), `public-ipv4`, `networks`; `Quotas(zone)` has none. The account call has no usage,
so the driver computes it from the lists of servers, private storages, addresses and networks. A bucket is reported only when its
limit is positive (0 or absent means unknown, **unverified**). `Consumption` is the plan's cores, memory, one public IPv4 (none for
Cloud Native) and the plan's default root disk on its tier; a NodeClass's own root size is not known there. Limits are read every 15
minutes. A quota error (`SERVER_CORE_LIMIT_REACHED` and the others below) gates every zone.

## Error mapping

By the API's error code (`upcloud.Problem.ErrorCode()`, from the problem type or the legacy code), never by message text; unlisted
codes are `Fatal`, except a 5xx or 429 (`Retryable`). On deletes an unlisted 409 is `Retryable` (a dependency under an unknown name)
and any 404 is "gone".

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `SERVER_RESOURCES_UNAVAILABLE`, `STORAGE_RESOURCES_UNAVAILABLE`, `SERVER_RESOURCES_UNAVAILABLE_FOR_STRICT_AA`, `IP_ADDRESS_RESOURCES_UNAVAILABLE` |
| `Unsupported` | `CORE_MEMORY_UNSUPPORTED`, `PLAN_CORE_NUMBER_ILLEGAL`, `PLAN_MEMORY_AMOUNT_ILLEGAL`, `STORAGE_TIER_ILLEGAL`; and the driver's own: `PlanNotOffered`, `TierMismatch`, `NoRoute`, `IsolationRunUnsupported`, `UnsupportedArch` |
| `QuotaExceeded` (account-wide) | `SERVER_CORE_LIMIT_REACHED`, `SERVER_MEMORY_LIMIT_REACHED`, `MAXIOPS_STORAGE_LIMIT_REACHED`, `STANDARD_STORAGE_LIMIT_REACHED`, `HDD_STORAGE_LIMIT_REACHED`, `SERVER_IP_LIMIT_REACHED`, `NETWORK_LIMIT_REACHED`, `DETACHED_FLOATING_IP_LIMIT_REACHED` |
| `ZoneUnusable` | `ZONE_NOT_FOUND`, `ZONE_INVALID` |
| `Retryable` | `SERVER_CREATING_LIMIT_REACHED`, `SERVER_STATE_ILLEGAL`, `STORAGE_STATE_ILLEGAL`, `STORAGE_IN_USE`, `NETWORK_NOT_EMPTY`, `FIREWALL_RULESET_IN_USE`, HTTP 5xx and 429, network errors, call timeouts |
| `NotFound` | `SERVER_NOT_FOUND`, `STORAGE_NOT_FOUND`, `NETWORK_NOT_FOUND`, `FIREWALL_RULESET_NOT_FOUND` |
| `Fatal` | everything else, among it `AUTHENTICATION_FAILED`, `ACTION_FORBIDDEN`, `*_FORBIDDEN`, `INSUFFICIENT_CREDITS` (billing, not capacity: nothing should retry into a drained account), `USER_DATA_INVALID`, `LABEL_LIMIT_REACHED` and `ZONE_MISMATCH` (our bugs), `UserDataTooLarge`, `InvalidSettings` |

## Permissions

UpCloud has no policy language. Use a dedicated sub-account with API access, server, storage and network access (and
firewall) and no GUI access; its `resource_limits` (cores and memory, set by the main account) are the cost guard. The operations
the driver uses, so that you can judge it:

- servers: list (with label filters), get, create, stop, modify (the private firewall switch), delete with storages and backups,
  the Public & Utility firewall rules (create, get);
- storages: list private templates, get;
- networks: list, get, create, delete; server groups: list, get, create, delete;
- firewall rulesets: list, get, create, delete, attach and detach to a server's private interfaces (v9 endpoints);
- account (the limits), zones, plans, IP addresses.

The driver deletes only servers, networks, rulesets and groups that carry the cluster's label, and never a template.

## Deferred

`isolation: run` (one SDN network per run key), GPU plans and spot, a guest-side firewall for the interval of the Public & Utility
firewall's rollout, `SCImage` (rung 4: SCarpenter importing the template itself), discovery of labelled networks without a
`networks` block, upstream Flatcar shipping an official `upcloud` image (the source then switches), the v9 SDK once it is stable.

## What is not verified against UpCloud yet

Everything here was written from documentation and the SDK source; no UpCloud call was made. The spikes of plan 19 settle
these before the first live run:

- **Images (S1, S2)**: that the patched generic image boots with OEM id `upcloud`, imports as raw gzip, and that Ignition
  finds its user-data; the Content-Type behaviour of the importer; whether a Standard-tier clone of a MaxIOPS template is refused;
  whether a template carries `created`; the by-id name of a data disk.
- **Limits (S3)**: the user-data size limit, the 4 versus 16 storage devices, the firewall rollout window and the default rules,
  whether the written rules keep a node working (DNS, DHCP, sysext downloads), whether the utility network reaches the internet.
- **Billing (S4)**: from creation or from first start, the clock hour, a disk larger than the plan's included storage, Standard
  versus MaxIOPS included storage per family (the driver reads the tier from the plan list), stopped servers, the cap; whether `GB` is GiB.
- **Idempotency and capacity (S5)**: the labels in the create body, that several label filters on a list are ANDed, that a label
  value may hold a timestamp (the fallback is epoch seconds, which `Created` reads), the code of a full zone.
- **Accounts (S6)**: that a sub-account's token sees the templates, networks and rulesets another created.
- **API behaviour (S7)**: rate limits and the status of throttling, how a limit of 0 is reported, which server states count towards
  the limits, whether `GET /account` has usage.
- **Rulesets**: the endpoints (`/firewall-ruleset`, `attach` and `detach_firewall_ruleset/private`), their JSON, the error codes
  (`FIREWALL_RULESET_NOT_FOUND`, `FIREWALL_RULESET_IN_USE`), whether the list carries labels (without them a managed ruleset is
  adopted by its name), the number of servers a group may hold.
- **Keeper**: the `/ipv4_prices` endpoint and the unit of the storage prices for UpCloud.
- **Plans**: Keeper's active plans today are Starter, Premium, Cloud Native and GPU; the driver classifies them by name and takes
  the included tier from the account's plan list.
