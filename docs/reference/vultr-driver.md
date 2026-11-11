# Vultr driver (in progress)

The Vultr driver (plan 20) is being built from the bottom up. This page describes
what exists in `pkg/vendors/vultr` so far; it is not yet a `vendors.Driver`, it is
not registered, and there is no `vultr` block in the NodeClass API. Everything
marked **unverified** is a hypothesis from the public API documentation and the
`govultr` source that a live spike (plan 20, section 11) has to confirm.

## What exists

| Part | Where | Notes |
|---|---|---|
| API interface | `api.go` | The calls the plan needs, in the shape of `govultr` v3.34.0 minus the `*http.Response`. |
| Real client | `client.go` | One global endpoint, `VULTR_API_KEY` as a bearer token, user agent `scarpenter/<version>`, a client-side limit of 10 requests per second (Vultr's is 30 per second per source IP), per-call timeouts (45 s, 90 s for a create), `govultr`'s retries of 429 and 5xx. |
| In-memory fake | `fake/` | Regions and stock, snapshots, instances (pending to active, no IP while pending, one-tag list filter, cursor pagination, an optional list lag), firewall groups (50 rules, refused delete while used), VPCs (attachments, refused delete while attached), the account's instance limit, injected errors with a status and a text, a create that commits but times out for the caller. |
| Error classification | `errors.go` | By HTTP status first, then by message text, see below. |
| Mapping | `mapping.go`, `launch.go`, `firewall.go` | Provider ID, the one dummy zone per region, plan families, tags, resource descriptions, the launch request, the state map, firewall rules. |
| Operations | `ops.go` | Idempotent launch, get, find, list, delete, snapshot resolution and the list of launchable plans, over the API interface. The Driver will wrap these. |

There is no quota reader: Vultr has no quota API (the account limits are shown in the
console only). Stock is read per region from `GET /v2/regions/{id}/availability`.

## Provider ID, zones, state

- Provider ID `scarpenter://vultr/<region>/<nodeclaim>`; instance reference
  `<region>/<instance id>`. A reference whose ID is not a UUID is `NotFound` without a
  call.
- Vultr has no zones. The zone of a node is its region ID (Keeper's zone ID equals
  the region ID).
- State: `status: pending` is `pending`; otherwise `suspended` or `power_status: stopped` is `terminating` (a stopped instance
  still bills, so the garbage collector deletes it); `active` with `running` and
  `server_status: ok` is `running`; everything else is `pending`. `main_ip` is
  `0.0.0.0` while pending and is then not an address.

## Tags and idempotency

Instance tags are plain strings. A logical tag becomes `scarpenter.sparecores.com/<key>=<value>`
(fallback style `scarpenter-<key>=<value>` if the charset check of spike S6 refuses the
slash or the dot); the NodeClass's own tags are added as `key=value`, logical tags win.
There is no create token, so the NodeClaim UID is written as the tag `idempotency-key`
and every create is preceded by an account-wide lookup of it (the list filter takes one
tag; the rest is compared client-side). A hit is returned whatever region and plan the
replay asks for. A create that the server accepted but whose response was lost is found
by the same lookup once the list shows it (unverified consistency); a cache of the last
10 minutes of creates covers the gap inside one process, and the garbage collector's
orphan sweep is the backstop.

Firewall groups and VPCs have no tags: a managed one carries its ownership marker in the
description, `<name> | cluster=<c> nodeclass=<n> managed-by=scarpenter resource-kind=<k>`.
A description that does not parse, or whose `managed-by` is not `scarpenter`, is never
touched. The driver refuses to delete an instance without its cluster's tag.

## Plans

The plan is Keeper's `api_reference` (`vc2-2c-4gb`). Families `vc2`, `vhf`, `vhp`,
`voc` are launched; `vx1` only with a local disk (a diskless plan boots from a volume);
`vcg`, `vdm`, `vbm` and the free plan `vc2-1c-0.5gb-free` are not; a family nobody
classified is unsupported. Vultr has no Arm instances, so `arm64` is `Unsupported`.
The root disk is the plan's disk (`RootDiskForType`), read as GB of 10^9 bytes.
Launchable plans of a region are the availability list filtered by that rule, cached for
5 minutes; a plan missing from it fails a launch before the call as `InsufficientCapacity`.

## Images

The Flatcar snapshot of a release is found by its description
`flatcar-<channel>-<version>-<arch>`: exactly one snapshot with that exact description (the
API filter may match a substring, so the comparison is client-side) and the status
`complete`, otherwise `ImageNotFound` or `ImageAmbiguous`. Snapshots are global in the
account and are never created or deleted by SCarpenter.

## Error classes

Vultr errors have no code: the body is `{"error": "<text>", "status": <n>}`, and `govultr`
drops the status from its errors. The client's transport records the status of each call
(the last attempt, retries included) and the error carries it (`APIError.HTTPStatus`).
Classification is the documented exception to "never by message text": status first, then
the texts below, only with a known 4xx status; an unmatched text is `Fatal` and a text with
no status decides nothing.

| Class (code) | Rule |
|---|---|
| `Retryable` (RateLimited, ServerError, CallTimeout, CallCanceled, TransportError, RetriesExhausted) | 429, 5xx, a canceled or timed out call, a network error, `govultr`'s "gave up after" text |
| `NotFound` | 404 on get or delete; 404 on a create is `Fatal` (a referenced snapshot, group or VPC is missing) |
| `RegionUnavailable` | 404 or 400 on a region's availability |
| `InsufficientCapacity` (OutOfStock, PlanUnavailable) | the plan is not in the region's availability (before the call), or a create text such as "currently unavailable", "out of stock" |
| `Unsupported` (Incompatible) | create texts such as "invalid plan", "not compatible" |
| `QuotaExceeded` (AccountLimit, ResourceLimit), account-wide | create texts such as "account limit", "maximum instance cost", "spending limit"; group and VPC limit texts |
| `Retryable` (Dependency, Locked) | a delete of a group, VPC or instance that is still used, attached or installing; 409 or 423 on a delete |
| `Fatal` | anything else, an invalid `user_data` included |

Every text is a hypothesis pinned by `errors_test.go` until spike S4 records the real ones.

## Deviations from the plan found so far

- `govultr` v3.34.0's `Plan` has no `storage_type` and no per-location cost, and
  `PlanAvailability` has only `available_plans` (no `available_vpc_only_plans`). The vx1
  diskless check therefore uses the plan's disk (Keeper's `storage_size`, 1 for diskless).
- `govultr`'s `OnRequestCompleted` hook is client-wide and sees `nil` after retries run
  out, so the status is recorded by a transport keyed by a per-call context value instead.
- `govultr` flattens a canceled or timed-out call into the text "gave up after N
  attempts, last error : ..."; the client restores the context error from the call's
  context so `errors.Is` works.
- `govultr`'s `SetRateLimit` is the retry backoff, not a limiter; the rate limit is the
  client's own.
- `InstanceCreateReq` always serializes `"block_devices": null` and `"tags"`; whether the
  API accepts a null `block_devices` is unverified (add to spike S3).

## Not yet built

The `vendors.Driver` and its registration, the `vultr` NodeClass types and CEL rules,
`ValidateNodeClass`, the resources (`firewallGroups`, `networks`, the `images` kind) on the
resource engine, node options (`RootDiskForType`), conformance wiring, the billing period,
the e2e lane and the sweeper. The seams are marked `TODO(plan 14)` in the code.
