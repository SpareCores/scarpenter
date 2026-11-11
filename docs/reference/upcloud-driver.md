# UpCloud driver

The UpCloud driver is being built (plan 19). This page describes the foundation
that exists so far, in `pkg/vendors/upcloud`: the API client behind a small
interface, an in-memory UpCloud, error classification, the mapping of catalog
names to UpCloud's, the launch request builder, the firewall rules, the lifecycle
helpers and the quota reader. It is not yet a registered `vendors.Driver`: no
NodeClass block, no conformance run, no L5 lane. Those wait for the shared
groundwork of plan 14 (the `images` kind, the unified `use` selector,
`RootDiskForType`); the seams are marked `TODO(plan 14)` in the code. Nothing in
the repository's tests calls UpCloud.

The contract the driver will implement is in the [driver contract](driver-contract.md).

## Table of Contents

- [What exists](#what-exists)
- [Regions and zones](#regions-and-zones)
- [Names, labels and references](#names-labels-and-references)
- [Plans and defaults](#plans-and-defaults)
- [Launch request](#launch-request)
- [Firewalls](#firewalls)
- [Delete](#delete)
- [Idempotency](#idempotency)
- [Quotas](#quotas)
- [Error mapping](#error-mapping)
- [Credentials](#credentials)
- [Not yet](#not-yet)
- [What is not verified against UpCloud yet](#what-is-not-verified-against-upcloud-yet)

## What exists

| Package | Content |
|---|---|
| `pkg/vendors/upcloud/api` | `API`, the calls the driver makes, in the shape of `upcloud-go-api/v8`'s service methods; the standalone firewall ruleset calls v8 lacks, written over the SDK's HTTP client; `Client`, the real implementation. |
| `pkg/vendors/upcloud/fake` | The in-memory UpCloud implementing `API`. |
| `pkg/vendors/upcloud` | Classification, mapping, request builder, firewall rules, lifecycle helpers, quota reader. |

The fake is faithful to what the plan relies on: a created server is in the
`maintenance` state until its storage is cloned, and its storages too (a storage
that is not `online` cannot be modified); a stop passes through
`maintenance` too; a server must be `stopped` before it is deleted; there is no
idempotency token (a replayed create makes a second server); list answers carry
no labels (the details do); templates belong to one zone and one tier; account
limits are account-wide and count other owners' servers; the per-server firewall
is replaced as a whole; strict anti-affinity runs out of hosts; networks must not
overlap. Errors can be injected per call and per code. What the plan marks
unverified is configurable (`LooseTemplateTier`, the limits, the user-data size).

## Regions and zones

UpCloud's regions are its zones (`fi-hel1`, `de-fra1`, ...): there are no
availability zones inside a data centre. The region and its dummy zone carry the
same ID and name (`upcloud.Zone`).

## Names, labels and references

- Provider ID: `scarpenter://upcloud/<zone>/<nodeclaim>`; instance reference
  `<zone>/<server uuid>`. A reference that is not `<zone>/<uuid>` cannot exist and
  is `NotFound`.
- Server and storage labels: the logical tags as `scarpenter-<key>`
  (`scarpenter-cluster`, `scarpenter-nodeclaim`, `scarpenter-nodepool`,
  `scarpenter-nodeclass`, `scarpenter-managed-by`, `scarpenter-idempotency-key`),
  because label keys are 2 to 32 printable ASCII characters and
  `scarpenter.sparecores.com/idempotency-key` has 33 and more. The NodeClass's own
  tags are written as they are; the keys must be valid label keys, must not start
  with `_` or `scarpenter-`, and at most 255 labels fit.
- `scarpenter-created` holds the creation time (RFC 3339, UTC), written in the
  create call: it is what `BillingStart` reads. A server without it has no billing
  start, so no retention.
- State: `started` is `running`; `maintenance` (cloning, or a stop in progress) is
  `pending`; `stopped` and `error` are `terminating` (a stopped node is not coming
  back and is collected, plan 14 A4).
- Addresses: public IPv4 and IPv6 first, then the utility address and any SDN address.

## Plans and defaults

The server is a plan name (`1xCPU-2GB`, `DEV-1xCPU-1GB-10GB`, `HICPU-8xCPU-16GB`,
`HIMEM-...`, `CLOUDNATIVE-...`, `GPU-...`). Plans are classified by family; GPU
plans and any plan of an unknown family are not launched.

| Default | Value |
|---|---|
| Root tier | the plan's included tier (Standard for Developer, MaxIOPS for the others and for plans without included storage) |
| Root size | the plan's included storage, at least 25 GB |
| Encrypted | true |
| Public IPv4 | on |
| Storage devices per server | 4 (root plus three volumes) until the limit is measured |

The root disk is a clone of the zone's template in the template's tier. Storage
size is 10 to 1024 GB.

## Launch request

`BuildCreateServer` sets `hostname` and `title` to the NodeClaim name, the `plan`,
the `zone`, `metadata: yes` (Ignition reads its configuration from the metadata
service), `firewall: on`, `user_data` as the raw Ignition JSON, no one-time password
(`password_delivery: none`, `login_user.create_password: no`), `timezone: UTC`,
`simple_backup: no`, remote access off, the labels, the server group if any, the
root as a clone and data volumes as `create`. Interfaces: public (IPv4, and IPv6
when asked), utility (always) and, when a network is configured, private.
`publicIPv4: false` needs a private network with a gateway of the user's own and is
`Unsupported` otherwise. The request is golden-tested field by field.

The API takes labels per storage device in the create call (for `create` and
`clone`), but the SDK's device type has no field for them: the driver's client adds
the cluster, NodeClaim and managed-by labels to every device in the request body,
so no storage of a node exists unlabelled (labelling afterwards would not work
either: a storage can only be modified when `online`, and the clone runs in
`maintenance`). The sweeper and the orphan pass find storages by these labels.

## Firewalls

UpCloud has two. The Public & Utility firewall is per server, stateless, limited
to 1000 rules and cannot be brought or shared, so the driver writes it for every
node (`PublicFirewallRules`, replacing the whole list; rollout takes 1 to 2
minutes). The first match wins:

1. accept ICMP and ICMPv6, DHCP and DHCPv6 replies;
2. accept the NodeClass's inbound rules;
3. accept UDP 41641 (Tailscale direct paths, the `tailscale` template only);
4. accept TCP and UDP to the ephemeral range 32768-60999, both families (replies
   to the node's own connections);
5. drop everything else inbound; outbound is accepted.

The SDN firewall (standalone rulesets) only filters private interfaces: a ruleset
the user brings never covers the public interface.

## Delete

UpCloud deletes only a stopped server. `Terminate` hard stops the server, waits for
`stopped`, then deletes it with its storages and backups. A gone server is success;
a server in `maintenance` is `Retryable`; a stopped one is deleted without a stop.

## Idempotency

There is no create token. `FindByIdempotencyKey` looks up the label
`scarpenter-idempotency-key=<NodeClaim UID>` in one list call across zones and reads
the details of the match. Servers found with no storage (a create that failed half
way) are deleted by the lookup.

## Quotas

All limits are account-wide (`GET /account` `resource_limits`) and are reported as
global buckets: `cores`, `memory` (MB), `storage-maxiops`, `storage-standard`,
`storage-hdd` (GB), `public-ipv4`, `networks`. The account call has no usage, so the
driver computes it from the lists of servers, private storages, addresses and
networks. A bucket is reported only when its limit is positive. `Consumption` gives
a plan's cores, memory, root disk and one public IPv4 (none for Cloud Native plans,
whose address is billed separately).

## Error mapping

By the API's error code (`upcloud.Problem.ErrorCode()`, from the problem type or the
legacy code), never by message text. Unlisted codes are `Fatal`; a 5xx or 429 with
an unlisted code is `Retryable`, also when the body is not JSON (a proxy's page). On
deletes an unlisted 409 is `Retryable` (a dependency under an unknown name) and any
404 is "gone". The v8 SDK retries nothing but its state pollers (5xx, 3 tries).

| Class | Codes |
|---|---|
| `InsufficientCapacity` | `SERVER_RESOURCES_UNAVAILABLE`, `STORAGE_RESOURCES_UNAVAILABLE`, `SERVER_RESOURCES_UNAVAILABLE_FOR_STRICT_AA`, `IP_ADDRESS_RESOURCES_UNAVAILABLE` |
| `Unsupported` | `CORE_MEMORY_UNSUPPORTED`, `PLAN_CORE_NUMBER_ILLEGAL`, `PLAN_MEMORY_AMOUNT_ILLEGAL`, `STORAGE_TIER_ILLEGAL` |
| `QuotaExceeded` (account-wide) | `SERVER_CORE_LIMIT_REACHED`, `SERVER_MEMORY_LIMIT_REACHED`, `MAXIOPS_STORAGE_LIMIT_REACHED`, `STANDARD_STORAGE_LIMIT_REACHED`, `HDD_STORAGE_LIMIT_REACHED`, `SERVER_IP_LIMIT_REACHED`, `NETWORK_LIMIT_REACHED`, `DETACHED_FLOATING_IP_LIMIT_REACHED` |
| `ZoneUnusable` | `ZONE_NOT_FOUND`, `ZONE_INVALID` |
| `Retryable` | `SERVER_CREATING_LIMIT_REACHED`, `SERVER_STATE_ILLEGAL`, `STORAGE_STATE_ILLEGAL`, `STORAGE_IN_USE`, `NETWORK_NOT_EMPTY`, `FIREWALL_RULESET_IN_USE`, HTTP 5xx and 429, network errors, call timeouts |
| `NotFound` | `SERVER_NOT_FOUND`, `STORAGE_NOT_FOUND`, `NETWORK_NOT_FOUND`, `FIREWALL_RULESET_NOT_FOUND` |
| `Fatal` | everything else, among it `AUTHENTICATION_FAILED`, `ACTION_FORBIDDEN`, `*_FORBIDDEN`, `INSUFFICIENT_CREDITS` (billing, not capacity), `USER_DATA_INVALID`, `LABEL_LIMIT_REACHED` and `ZONE_MISMATCH` (our bugs) |

Deletes treat any 404 as "gone", whatever the code is called (except
`LABEL_LIMIT_REACHED`, which the reference documents with status 404).

## Credentials

An API token in `UPCLOUD_TOKEN` (bearer). The client is built without a call; the
user agent is `scarpenter/<version>`. Per-call deadlines: 45 seconds, 90 for
`CreateServer`.

## Not yet

The `vendors.Driver` implementation and its registration; the typed
`spec.vendors.upcloud` block and the profile blocks; `ResolveImage` and
`ReportImages` over the `images` kind; `ManagedResources` for networks, rulesets
and server groups; the conformance run; the chart's credentials mode; the L5 lane
and the sweeper; the how-to for importing the Flatcar templates.

## What is not verified against UpCloud yet

Everything here was written from documentation and the SDK source; no UpCloud call
was made.

- The ruleset codes (`FIREWALL_RULESET_NOT_FOUND`, `FIREWALL_RULESET_IN_USE`; the
  fallbacks above cover other names), and whether 429 is used. The storage limit
  codes and `NETWORK_NOT_EMPTY` are from the API reference.
- The storage labels in the create body (documented, but not in the SDK).
- The ruleset endpoints (`/firewall-ruleset`, `/server/<uuid>/{attach,detach}_firewall_ruleset/private`)
  and their JSON, taken from the OpenAPI-generated v9 module (alpha), and whether a
  list answer carries labels.
- Whether several label filters on a list are ANDed; whether a label value may hold a
  timestamp (the fallback is epoch seconds, which `Created` reads).
- The storage device limit (4 or 16), the user-data size limit, the firewall rollout
  window and the default rules until it ends.
- Whether Cloud Native servers' public IPv4 counts against the account limit, which
  server states count towards the limits, and how a limit of 0 is reported.
- Whether `GB` is GiB, and whether a clone may have a different tier than its template.
