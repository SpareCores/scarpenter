# Hetzner Cloud driver (in progress)

The foundation of the `hcloud` driver (`pkg/vendors/hcloud`, vendor ID and
registration name `hcloud`): the layers below `vendors.Driver`. The driver itself
(registration, node options, resources, the `images` kind) follows plan 15 once the
shared groundwork of plan 14 is in the core. Nothing in this package is wired into
the controller yet, and no test calls Hetzner. The contract the driver will
implement is in the [driver contract](driver-contract.md).

## Table of Contents

- [What exists](#what-exists)
- [API client](#api-client)
- [In-memory fake](#in-memory-fake)
- [Launch request](#launch-request)
- [Labels and names](#labels-and-names)
- [States, references and provider IDs](#states-references-and-provider-ids)
- [Error mapping](#error-mapping)
- [Quota and availability](#quota-and-availability)
- [Not yet](#not-yet)

## What exists

| Part | Where |
|---|---|
| `API`: the narrow interface of the calls used, and `Client`, its adapter over `hcloud-go` v2.53 | `api.go`, `client.go` |
| `fake`: the in-memory API | `pkg/vendors/hcloud/fake` |
| `Project`: caches, create with idempotency, delete with Primary IP cleanup, shared server list, optional server limit | `project.go` |
| Launch and volume request builders, firewall rules | `launch.go` |
| Labels, names, provider ID, references, states, addresses, availability | `labels.go`, `mapping.go` |
| Error classification | `errors.go` |

## API client

One client for the whole project (no per-region clients); the token is the account
scope (`HCLOUD_TOKEN`, `HCLOUD_ENDPOINT` for tests). Every call runs under its own
deadline: 30 seconds (90 for the wait of the create action); a call that runs into it
fails `Retryable` with code `CallTimeout`. The SDK retries `rate_limit_exceeded`,
`conflict`, `bad_gateway` and `timeout` itself. The user agent is
`scarpenter/<version>`. The `RateLimit-*` headers of every response are recorded
(`API.RateLimit`), for the planned `scarpenter_hcloud_api_remaining` metric; below
20 % remaining `RateLimit.LowRemaining` is true.

## In-memory fake

`fake.Fake` implements `API`: six locations with network zones, server types with
per-location `available` and `deprecation`, labelled snapshot images of both
architectures, servers with the status machine (initializing, starting, running,
stopping, off, deleting; `Tick` steps it, waiting for an action completes it), unique
names (`uniqueness_error`), label selectors (equality, set and presence forms),
50-per-page pagination counted as requests, Primary IPs with `auto_delete` and the
unassigned state, networks with subnets by network zone, firewalls with `applied_to`,
spread placement groups with the 10-server limit, volumes (location-bound, attached at
create, outliving servers), project limits raising `resource_limit_exceeded`
(foreign servers count), a `RateLimit` counter with a 429 mode, per-call error
injection and servers of other owners.

## Launch request

`BuildCreateOpts` turns a resolved `LaunchSpec` into the one `POST /servers`:

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

## Labels and names

Label keys are `scarpenter.sparecores.com/<key>`. A label value is at most 63
characters of `[A-Za-z0-9._-]`, starting and ending alphanumeric: a value that does
not fit becomes its first 50 valid characters, `-` and an 8-character hash of the
whole value. The same function runs on write and on lookup, so lookups by the long
name find the label; the original cannot be read back from a shortened label.

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

## Quota and availability

Hetzner's project limits are not readable through the API. `Project` implements
`vendors.QuotaReporter` with one global bucket `servers` (unit `instances`; usage is
every server of the project) only when a server limit is configured
(`WithServerLimit`, later `--hcloud-server-limit`); without it the path is reactive
(`QuotaExceeded` on `resource_limit_exceeded`). `LaunchableTypes` implements
`vendors.OfferingLister` from one `GET /server_types` (cached 3 minutes): a type is
launchable in a location when its entry there is `available` and not past
`unavailable_after`.

## Not yet

The `vendors.Driver` implementation and its registration, the node options and
profiles, the managed resources (`firewalls`, `placementGroups`, `volumes`, `networks`
use), the NodeClass API types, image resolution through the `images` kind (`FindImage`
is the label lookup of the plan until then), the conformance wiring and the L5 lane.
Nothing here has run against Hetzner: every behaviour above is from the API
documentation and the in-memory fake.
