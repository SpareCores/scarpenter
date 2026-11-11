# Hetzner Cloud driver

The `hcloud` driver (`pkg/vendors/hcloud`, vendor ID and registration name `hcloud`):
servers through `POST /servers` from Flatcar snapshots of the user's own project, the
project's locations as regions and zones, vendor-native node settings (data volumes,
spread placement groups), managed and brought resources (firewalls, placement groups,
volumes, private networks) and an optional server limit for headroom-aware placement.
Design: plan 15, overview section 11, ADR-020 to ADR-024. The contract it implements is
the [driver contract](driver-contract.md); the settings it reads are in
[node settings](node-settings.md); the resource engine around it is in
[vendor resources](vendor-resources.md); the selector is in
[resource selectors](resource-selectors.md#hcloud). How to make the images:
[Flatcar snapshots on Hetzner Cloud](../how-to-guides/hcloud-images.md).

It is enabled with `--vendors=hcloud` (the chart: `vendors.hcloud.enabled`). Tests run
against the in-memory API of `pkg/vendors/hcloud/fake` and the shared conformance suite;
nothing in the repository's tests calls Hetzner (the first live run is the L5 lane).

## Table of Contents

- [Capabilities](#capabilities)
- [What is different on Hetzner](#what-is-different-on-hetzner)
- [Credentials and the client](#credentials-and-the-client)
- [Regions and zones](#regions-and-zones)
- [Images](#images)
- [Launch](#launch)
- [Node settings](#node-settings)
- [Resources and network](#resources-and-network)
- [What your IaC provides](#what-your-iac-provides)
- [Labels and names](#labels-and-names)
- [States, references and provider IDs](#states-references-and-provider-ids)
- [Quotas and availability](#quotas-and-availability)
- [Billing](#billing)
- [Error mapping](#error-mapping)
- [In-memory fake](#in-memory-fake)
- [Deferred](#deferred)
- [What is not verified against Hetzner yet](#what-is-not-verified-against-hetzner-yet)

## Capabilities

| Interface | Hetzner Cloud |
|---|---|
| `Driver` | regions (locations), zones (the location itself), `ResolveImage`, `CreateInstance`, `GetInstance`, `FindInstance`, `ListInstances`, `DeleteInstance`, `TagKey` |
| `NodeClassValidator` | syntax of the `hcloud` block and of the hcloud blocks of the profiles, the isolation rule for a brought network |
| `ImageReporter` | `status.vendors.hcloud.images`, condition `ImagesReady`: one snapshot per architecture, reported for every location of the NodeClass |
| `NodeOptions`, `EpochReporter` | defaults, `RootDiskForType`, `Supported`, `PriceDelta`, `Prepare` with device hints; the epoch changes when the server type table or the volume prices load |
| `ManagedResources` | kinds `volumes`, `placementGroups`, `firewalls`, `networks` (use only) and the `images` selector |
| `QuotaReporter`, `OfferingLister` | one global bucket `servers` when `--hcloud-server-limit` is set; launchable types by location from `GET /server_types` |
| `BillingDescriber` | hourly, per started hour, a stopped server still bills: **unverified**, so [billing retention](controller.md#billing-retention) stays off until the live check passed. `Instance.BillingStart` is the server's `created`. |
| `ArchNamer` | `{vendorArch}` is `x86` or `arm` |

## What is different on Hetzner

- **No public Flatcar image.** The image rung is 3: snapshots in your own project, made
  with `hcloud-upload-image`, found by labels. They are global in the project, per
  architecture, not per location.
- **No spot, no instance profiles, no IAM.** Every server is on-demand; the token is the
  whole credential and it is bound to one project.
- **The root disk belongs to the server type** (20 to 960 GB): the scheduler's ephemeral
  storage is per type and a profile cannot change it.
- **Billing is hourly**, rounded up per started hour, and a Primary IPv4 and volumes bill
  separately.
- **A server is open without a firewall**, and firewalls filter only the public interface.
- **The project's limits cannot be read** through the API.

## Credentials and the client

One credential, one project: `HCLOUD_TOKEN` (the chart: `vendors.hcloud.tokenSecretRef`)
is a read-write token of the project, created in the Console, separate from your IaC's
own so that it can be rotated apart. It gives full control of the project: use a project
that holds only SCarpenter's workload, because label filtering (the driver deletes only
what carries its ownership marker) is the only scoping SCarpenter does. `HCLOUD_ENDPOINT`
overrides the API URL (tests). Several projects in one controller are not supported.

`Init` builds the client and makes no call. One client serves the whole project (no
per-region clients). Every call runs under its own deadline: 30 seconds (90 for the wait of
the create action); a call that runs into it fails `Retryable` with code `CallTimeout`.
The SDK retries `rate_limit_exceeded`, `conflict`, `bad_gateway` and `timeout` itself. The
user agent is `scarpenter/<version>` and the SDK's request metrics go to the controller's
metrics registry.

**Rate limit.** Hetzner allows 3600 requests an hour per project, refilled one per second.
The driver never makes a per-node `GET` in a loop: the server list is one project-wide
paginated call shared for 5 seconds, the server types are read every 3 minutes, and
resources are listed once per kind. The headers of every response feed the gauges
`scarpenter_hcloud_api_remaining` and `scarpenter_hcloud_api_limit`; below 20 % remaining the
driver logs a warning (at most every 5 minutes).

## Regions and zones

`Regions` is `GET /locations` (cached for an hour): `fsn1`, `nbg1`, `hel1`, `ash`, `hil`,
`sin`. A location is its own zone (`topology.kubernetes.io/zone` and
`scarpenter.sparecores.com/zone-id` both carry it). `spec.vendors.hcloud.regions` narrows
them; empty means every location of the project. A location belongs to a network zone
(`eu-central`: fsn1, nbg1, hel1; `us-east`: ash; `us-west`: hil; `ap-southeast`: sin), which
matters for private networks.

## Images

Rung 3 of ADR-021: the Flatcar snapshot of the pinned release in your own project, one per
architecture, found by its labels:

| Label | Value |
|---|---|
| `os` | `flatcar` |
| `flatcar-channel` | the channel (`stable`) |
| `flatcar-version` | the pinned version (`4757.2.3`) |

plus the snapshot's architecture (`x86`, `arm`). `GET /images?type=snapshot&status=available&
architecture=...&label_selector=...` must return exactly one image; none is `ImageNotFound`
(NotFound: the region's images are not ready), several are `ImageAmbiguous` (Fatal: the driver
never guesses). `resources.images.use` replaces the lookup with the shared selector
(`ids` under the key `*` since snapshots are global, `tags` and `expression` as labels,
`match.architecture`, `state`, `includeDeprecated`, `createdAfter`/`createdBefore`, `description`;
`filters` `type`, `status`, `bound_to`; `name` matches system images only, snapshots have none).
Whatever the lookup, the image's `flatcar-channel` and `flatcar-version` labels must say the
pinned release or it is never booted (`ImageVersionMismatch`, `ImageVersionUnknown`). Answers are
cached for an hour, a missing image for five minutes. How to make the snapshots:
[the how-to](../how-to-guides/hcloud-images.md). The driver does not import images.

## Launch

`CreateInstance` turns a resolved `LaunchSpec` into the one `POST /servers`:

| Field | Value |
|---|---|
| `name` | the NodeClaim name; longer than 63 characters or not an RFC 1123 label: the first 50 characters, `-`, an 8-character hash |
| `server_type`, `location` | the API reference (`cpx22`) and the location name |
| `image` | the resolved snapshot ID |
| `user_data` | the rendered Ignition, at most 32 KiB |
| `labels` | the logical tags, the NodeClass's own tags, `scarpenter.sparecores.com/idempotency-key` (the NodeClaim UID), `scarpenter.sparecores.com/capacity-type` (`on-demand`) |
| `public_net` | `enable_ipv4`, `enable_ipv6` (at least one is required) |
| `networks`, `firewalls`, `placement_group`, `ssh_keys` | the resolved IDs, only when set |
| `volumes`, `automount` | the pre-created volumes, `automount: false` |
| `start_after_create` | true |

Volumes are created with `BuildVolumeOpts` (10 to 10240 GB, no format: the first-boot
unit formats them); their device is `/dev/disk/by-id/scsi-0HC_Volume_<id>`.
`FirewallRules` makes inbound-only rules, one per rule and protocol (`any` becomes
tcp, udp and icmp, ports only on tcp and udp; a tcp or udp rule without ports
gets `1-65535`, since the API requires a port for both); no rules mean a
deny-all-inbound firewall.

`Project.Create` waits for the create action (a failed or timed-out action deletes
the half-created server, waits for that delete so the name and the key are free, and
returns the classified error). **Idempotency**: Hetzner
has no create token; the server name is unique per project. A replay fails with
`uniqueness_error`, the server is read by name and returned when its
`idempotency-key` label is the key, whatever zone and type the replay asked for. A
server of that name with another key, or one in status `deleting`, is `Retryable`
code `NameInUse`. The instance
returned by a create is `pending` (read before the start action); `Get` reports it
running afterwards.


## Node settings

Built-in defaults (layer 1): **nothing to set**. The root disk is the server type's, public
IPv4 is on and IPv6 off, there is no network, a managed deny-inbound firewall is created and
there is no placement group. The `hcloud` block and the storage profiles override them.

| Block | Fields |
|---|---|
| `regions` | location allow-list |
| `networking` | the generic override: `publicIPv4` (default true), `publicIPv6` (default false), `inbound.rules`, `isolation` |
| `sshKey` | name or ID of a key of the project; default none (nodes are configured by Ignition) |
| `storage.data[]` | `name`, `size`, `filesystem`, `mountPath`; **no `root`, no `localStorage`** |
| placement profile | `placementGroup: {type: spread}` or `{use: {name}}`; there is no `cluster` placement and no tenancy |
| network profile | none: Hetzner has no NIC options |

**Data volumes.** Each `storage.data` entry is a volume created in the location before the server
(`POST /volumes` with `location`, no format), passed in `volumes` with `automount: false`; the
first-boot unit formats and mounts it by its stable `/dev/disk/by-id/scsi-0HC_Volume_<id>` link.
`size` is rounded up to whole GB (10 GB to 10 TB, at most 16 per server). Volumes outlive servers,
so the `volumes` kind is `perUse`: the engine deletes a volume no server uses after the grace period.

**Networking.** A server needs a public IPv4 or IPv6 address: private-only servers are refused
(they need a NAT route in your network to reach the sysext hosts, and the metadata service that
Ignition reads is not verified on them). IPv6-only is accepted, but `github.com` and its release
hosts have no IPv6 address, so the default sysext base does not work without a mirror.
`inbound.rules` become inbound rules of the firewall SCarpenter creates (`any` becomes one rule
per protocol of tcp, udp, icmp; at most 500); outbound is open.

**Placement.** A spread group holds at most 10 servers and a server is in one group. The driver
counts the members before a create and answers `InsufficientCapacity` with code `PlacementFull`
when the group is full (a second group per run key is not made).

**Validation** (`ValidateNodeClass`, shown in the NodeClass status): location names, the `sshKey`
form, volume sizes (10 to 10240 GB), duplicate names and mount paths, at most 16 volumes, inbound
CIDRs and protocols, the selector of every resource kind against the capability declaration
(`match` attributes, `filters` names, placeholders, `orderBy`), numeric IDs per location, label keys and
values of `tags`, `networks.create` (rejected), no public address (rejected), tag keys under
`scarpenter.sparecores.com/` (reserved). With a brought network (`resources.networks.use`) the
resolved `networking.isolation` (generic or the `hcloud` override) must be `nodeclass` or `none`: Hetzner
firewalls filter only the public interface, so nodes on a shared private network reach each other at every
level; `run` (the default) is not possible until SCarpenter creates per-run networks. Bring a network
dedicated to the NodeClass. A network found by the opt-in label is held to the same rule at the launch
(`Fatal`, `InvalidSettings`).

**Price delta** (`PriceDelta`): volumes at Keeper's `block` price of the location per GB-month over 730 hours
plus the Primary IPv4 as a constant, 0.0007 USD an hour (0.50 EUR a month; override with
`SCARPENTER_HCLOUD_IPV4_PRICE`, chart `vendors.hcloud.ipv4Price`), because Keeper has no IPv4 price.
Traffic over the included amount is not priced. The price is incomplete when a price is unknown.
A storage profile is supported in a location only when Keeper has a `block` row for it (after the first load).

## Resources and network

| Kind | Scope, default policy | `use` | Without `use` |
|---|---|---|---|
| `images` | global in the project | `ids` under `*`, `tags`, `expression`, `match`, `filters` | the label lookup above (not managed) |
| `networks` | region, `retain` | `ids` per location, `name`, `tags`, `expression`, `match.cidr`/`within`/`zone` (network zone) | a network with the opt-in label, else none: **not created in v1** (an optional kind), the server has public addresses only |
| `firewalls` | region, `idleTTL` 24 h | `ids`, `name`, `tags`, `expression` | an opt-in-tagged firewall, else one deny-inbound firewall per location with the `networking.inbound` rules, applied at server create by ID |
| `placementGroups` | run, `perUse` | `use: {name}` in a placement profile | one group per (location, profile, run key) |
| `volumes` | node, `perUse` | none (internal) | one per node and `storage.data` entry |

- A network is attached when `resources.networks.use` or the opt-in label `scarpenter.sparecores.com/use: "true"`
  provides one; nothing found is no network (the kind is optional to the core). `Resolve` keeps the networks
  that have a cloud subnet in the network zone of the location; a location where the selector finds none is
  gated with the reason. Your IaC needs no subnet ID: a server created with `networks: [<id>]` gets an address
  from the subnet of its location's network zone. A network holds at most 100 servers.
- The firewall is applied by ID in the create call, not by label selector: a label-selector firewall attaches
  with a delay, during which a new server would be open. `firewalls.use` brings the firewall as it is:
  `networking.inbound` does not apply to it.
- Managed resources are named `scarpenter-<cluster>-<kind>-<location>[-hash]` (at most 63 characters) and
  carry the ownership labels (`cluster`, `nodeclass`, `managed-by`, `resource-kind`) and the label
  `scarpenter.sparecores.com/location`, because firewalls and placement groups are project-wide on Hetzner and
  SCarpenter counts them per location. Delete only touches what carries this cluster's marker; a resource
  in use is `Retryable`. The delete order is servers, volumes, placement groups, firewalls.
- Nothing is a vendor default: there is no `Default` for any kind.

## What your IaC provides

1. The Flatcar snapshots (both architectures, the three labels) or a CI job that makes them.
2. Optionally a private network and a firewall per location (`networks.use`, `firewalls.use`).
3. The controller's own read-write token, in a Secret.

SCarpenter creates without `use`: a deny-inbound firewall per location (`idleTTL`), placement groups and
volumes.

## Labels and names

Label keys are `scarpenter.sparecores.com/<key>`. A label value is at most 63
characters of `[A-Za-z0-9._-]`, starting and ending alphanumeric: a value that does
not fit becomes its first 50 valid characters, `-` and an 8-character hash of the
whole value. The same function runs on write and on lookup, so lookups by the long
name find the label; the original cannot be read back from a shortened label.


`scarpenter.sparecores.com/capacity-type` is `on-demand`. The ownership labels of managed resources add
`scarpenter.sparecores.com/location`.

## States, references and provider IDs

| Server status | Instance state |
|---|---|
| `initializing`, `starting` | pending |
| `running`, `migrating`, `rebuilding`, `unknown` | running |
| `stopping`, `off`, `deleting` | terminating |

`off` is terminating on purpose (the stopped-node rule): a powered-off server still
bills and is no node, so it stays listed for the garbage collector and `Delete`
removes it. The instance reference is `<location>/<server ID>`; the provider ID
`scarpenter://hcloud/<location>/<nodeclaim>`. A location is its own (dummy) zone.
Addresses are the public IPv4, `<prefix>::1` of the IPv6 /64 and the private
network IPs.

`Project.Delete` refuses a server that lacks this cluster's `managed-by` and
`cluster` labels, treats `not_found` and an unparseable reference as gone, waits for
the delete action and deletes the server's Primary IPs that stayed unassigned
without `auto_delete`.


## Quotas and availability

Hetzner's project limits (servers, Primary IPs, volumes, networks, firewalls, placement groups) are not
readable through the API. The path is reactive: `resource_limit_exceeded` is `QuotaExceeded` and marked
**account-wide**, so the other locations are not tried for the same limit. With `--hcloud-server-limit`
(`SCARPENTER_HCLOUD_SERVER_LIMIT`, chart `vendors.hcloud.serverLimit`; default 0, unknown) the driver reports one
global bucket `servers` (unit `instances`; usage is every server of the project, also those of other owners;
consumption 1 for every type, none for a capacity type other than on-demand), so headroom is known before a launch.

**Availability.** `LaunchableTypes(location)` is one `GET /server_types` (cached 3 minutes, which also serves the
root disk of a type): a type is launchable in a location when its entry there is `available` and not past
`deprecation.unavailable_after`. This replaces the removed `datacenters.server_types.available`.

## Billing

Hourly, rounded up per started hour; deleting is the only thing that ends the charge (a powered-off server
bills). `Instance.BillingStart` is the server's `created`: Hetzner bills once the creation has completed, so a
boundary computed from `created` falls early, the safe side. `DeleteInstance` returns when the delete action is
done. The rule is reported **unverified** (`Verified: false`): the live check of the L5 lane must show that periods
count from the creation of the server and not from the clock hour (create, delete at 59 and 61 minutes, read the
hours billed) before retention is enabled for Hetzner ([plan 23](../plans/v1/done/23-billing-retention.md)).

## Error mapping

By the API's `error.code`, never by message. A response without a JSON error body
is classified by its HTTP status alone: 429 is `Retryable` `RateLimited` (the SDK
retries only the JSON `rate_limit_exceeded`, and the contract has no delay hook, so
`Retry-After` is not read; the core's backoff applies), 5xx is `Retryable`
`ServerError`, any other status `Fatal`. Every code and status is pinned by a table test.

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `resource_unavailable`, `placement_error`, `no_space_left_in_location`, `PlacementFull` (driver) |
| `Unsupported` | `invalid_input` whose fields are all `server_type`, `location` or `image`; `invalid_server_type` |
| `QuotaExceeded`, account-wide | `resource_limit_exceeded` |
| `ZoneUnusable` | `no_subnet_available`, `ip_not_available` |
| `Retryable` | `rate_limit_exceeded`, `server_error`, `bad_gateway`, `timeout`, `unavailable`, `conflict`, `locked`, `maintenance`, `robot_unavailable`, `service_error`, `uniqueness_error`, `resource_in_use`, deadlines, transport errors |
| `NotFound` | `not_found` |
| `Fatal` | everything else (`unauthorized`, `forbidden`, `json_error`, `protected`, other `invalid_input`, ...) |


`resource_in_use` on a resource delete is `Retryable` (the dependency is still on its way out).

## In-memory fake

`fake.Fake` implements `API`: six locations with network zones, server types with per-location `available` and
`deprecation`, labelled snapshot images of both architectures, servers with the status machine (initializing,
starting, running, stopping, off, deleting; `Tick` steps it, waiting for an action completes it), unique names
(`uniqueness_error`), label selectors (equality, set and presence forms), 50-per-page pagination counted as requests,
Primary IPs with `auto_delete` and the unassigned state, networks with subnets by network zone, firewalls with
`applied_to`, spread placement groups with the 10-server limit, volumes (location-bound, attached at create, outliving
servers), project limits raising `resource_limit_exceeded` (foreign servers count), a `RateLimit` counter with a 429
mode, per-call error injection and servers of other owners. The `POST /servers` body is pinned by a golden test through
the real SDK. The conformance suite runs once per managed kind (`firewalls`, `placementGroups`, `volumes`); the
resolution-order cases of the suite are not run because Hetzner has no default resource to arrange, and are covered
through the real engine in the driver's own tests.

## Deferred

Spot (does not exist), private-only servers, `networks.create`, per-run private networks and the isolation levels that
need them, several projects per controller, Floating IPs, load balancers, snapshots as node roots, an `ImageImporter`
(the label convention is its contract), a `direct`-template `--node-ip` from the private network.

## What is not verified against Hetzner yet

Nothing here has run against Hetzner: every behaviour above is from the API documentation and the in-memory fake. The
L5 lane settles each of these (plan 15, "Unverified facts"):

- the responses of `resource_unavailable`, `placement_error` and the `invalid_input` field names for an unsupported
  location or architecture; the `details.limits[].name` values; `no_subnet_available`;
- that a firewall without rules drops all inbound traffic (the deny-inbound default is the plan's reading);
- whether Primary IPs created with a server are deleted with it (`auto_delete`); the cleanup after the delete is the safety net;
- the address Flatcar uses on the IPv6 /64 (`::1`), and IPv6 provisioning by the Flatcar image;
- root partition expansion on the first boot of the snapshot;
- whether Ignition's metadata fetch works on a server without a public interface (the reason private-only is deferred);
- whether a placement group may have members in two locations;
- whether Keeper's server prices exclude the Primary IPv4, and the unit of `storage_size`;
- the billing rule (above);
- whether a create without `ssh_keys` has account side effects (a root password in the response).
