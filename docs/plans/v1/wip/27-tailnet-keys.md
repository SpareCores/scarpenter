# 27 — Per-node tailnet auth keys

Depends on: 05 (bootstrap tokens, per-attempt rendering, the `bootstraptoken` controller as the model), 07 (the L4
qemu lane and its Headscale), 09 (the chart and the controller reference), 14 (contract groundwork), and
[ADR-025](../../../explanation/DECISIONS.md#adr-025-instance-metadata-reaches-only-the-nodes-own-components-accepted-2026-11-19)
(on main). Read `docs/plans/v1/CONVENTIONS.md` first. Spec: this plan; overview §8.3 (network templates, the
"per-node keys are post-v1" note), §8.6 (secrets in user-data), §15 Q8; `docs/reference/node-bootstrap.md`
([network templates](../../../reference/node-bootstrap.md#network-templates),
[bootstrap tokens](../../../reference/node-bootstrap.md#bootstrap-tokens),
[instance metadata access](../../../reference/node-bootstrap.md#instance-metadata-access),
[size budget](../../../reference/node-bootstrap.md#size-budget),
[the NodeClass controller](../../../reference/node-bootstrap.md#the-nodeclass-controller)); the
[controller reference](../../../reference/controller.md) (RBAC, garbage collection).

Written 2026-11-20 from the repository at `origin/main`, the Headscale source at v0.29.7 (the version the L4 lane
pins) and v0.23.0, v0.26.1, v0.27.1 for the differences, the Tailscale API client `tailscale-client-go-v2` (main)
and Tailscale's knowledge base. No tailnet API was called. Everything marked **unverified** is settled by the step
that names it.

## Goal

A NodeClass can ask for **one auth key per node**, minted by the controller when the node is launched: single use,
pre-authorised, tagged with the NodeClass's ACL tags, ephemeral, expiring an hour after it is minted, and present
only in that node's user-data. After the node has registered, the key is revoked; when its NodeClaim is deleted,
its tailnet device is deleted, so stale devices do not pile up. A key read from a node, from its user-data or from
the vendor's API after the node joined is worthless; a key read before it joined admits one device with the
NodeClass's tags, for at most an hour. The first user is the benchmark workload, whose untrusted code runs on
`bench` nodes, before more vendors are added.

Two backends behind one small interface: Tailscale's API (SaaS) and Headscale's API (self-hosted; the live
cluster's). The shared static key stays as it is and stays the default: a NodeClass that does not set the new field
renders and hashes exactly as before.

## 1. Today (as found)

- `spec.network.tailscale` (`pkg/apis/v1alpha1/scnodeclass.go`, `TailscaleSpec`): `version`, `loginServer` (empty:
  Tailscale SaaS; set: Headscale), `authKeySecretRef` (**required**), `tags` (`tag:` prefix, at most 16),
  `acceptRoutes`.
- `bootstrap.LoadInputs` (`pkg/bootstrap/sources.go`) reads the key from the Secret for every rendering; the
  NodeClass controller's drift hash (`pkg/controllers/nodeclass/hash.go`) covers the Secret's `resourceVersion`
  (`AuthKeyVersion`), never the key, and marshals `spec.network` whole.
- `pkg/bootstrap/templates/tailscale.bu` writes the key to `/etc/scarpenter/tailscale-authkey` (mode 0600) and runs,
  on **every boot**, `tailscale up --auth-key=file:/etc/scarpenter/tailscale-authkey --hostname=<nodeclaim>
  --accept-dns=false [--login-server] [--accept-routes] [--advertise-tags]`, then waits for the tailscale IPv4 that
  becomes the kubelet's `--node-ip`.
- The chart's `rbac.authKeySecrets` grants `get` on a Secret of another namespace; the release namespace's
  `scarpenter-auth-key` Role covers the default.
- So every node of a NodeClass carries the same reusable key, on its disk and in its user-data. Since ADR-025 Pods
  and other host processes cannot read the metadata service (unless the NodeClass says `Open`), but the key stays
  readable on the disk by root, in the vendor's API by anyone allowed to read the instance's user-data, and in the
  Secret. A leaked reusable key lets anyone join the tailnet with the NodeClass's tags until it expires.
- The L5 lanes mint one reusable, ephemeral, pre-authorised key per run through the Tailscale API
  (`test/e2e-*/tailscale-api.sh`, `expirySeconds: 3600`); the L4 lane runs Headscale 0.29.7 and mints a reusable key
  with the Headscale CLI (`test/qemu/run.sh`).

## 2. Design

### 2.1 API (decided)

```yaml
spec:
  network:
    template: tailscale
    tailscale:
      version: v1.98.10
      loginServer: https://headscale.example.com   # must match the controller's backend (2.2)
      tags: [tag:scarpenter-bench]                  # required with PerNode
      keySource: PerNode                            # Static (the default when unset) | PerNode
      # authKeySecretRef: only with Static
```

- `keySource` (`Static` | `PerNode`, optional, no CRD default): unset means `Static`.
- `authKeySecretRef` becomes optional (a pointer). CEL: with `keySource: PerNode` it must be absent; otherwise it is
  required (the existing message stays for the static case). `tags` must be non-empty with `PerNode`: Tailscale
  requires tags on keys an OAuth client mints, and tagged devices have no node key expiry on either backend
  (Tailscale: "key expiry for tagged devices is disabled by default"; Headscale 0.28+: tagged nodes are exempt from
  `node.expiry`), so a node never needs to log in again with its used key.
- **Drift hash**: unchanged for every stored NodeClass. `keySource` is `omitempty`, and an explicit `Static` is
  normalised to unset before hashing (the way an unset `maxPods` is normalised), so writing `Static` drifts nothing.
  A pointer to `SecretKeyRef` marshals to the same JSON as the value, so the hash of a NodeClass with a Secret is the
  same. Switching to `PerNode` changes `network` and drifts the nodes once, which is wanted (they hold the shared
  key). With `PerNode` there is no `AuthKeyVersion`. A golden test pins the hash of the stored example NodeClasses.
- No new field for ephemeral, expiry or the backend on the NodeClass (simplicity; 2.4 and decision 2).

### 2.2 Controller configuration (decided)

The backend, its URL and its credentials are **controller-wide**, not per NodeClass. A NodeClass editor must never be
able to send the controller's tailnet credentials to a URL of their choosing (a `loginServer` pointing elsewhere
would otherwise receive a Headscale API key). The controller therefore refuses `PerNode` for a NodeClass whose
`loginServer` does not match its backend: empty for `tailscale`, equal (scheme, host, port; trailing slash ignored)
to `--tailnet-api-url` for `headscale`.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--tailnet-backend` | `SCARPENTER_TAILNET_BACKEND` | `none` | `none`, `tailscale` or `headscale`. `none`: `PerNode` NodeClasses are not valid. |
| `--tailnet-api-url` | `SCARPENTER_TAILNET_API_URL` | `https://api.tailscale.com` with `tailscale`; required with `headscale` | The API's base URL (Headscale: the server URL, the API is below `/api/v1`). |
| `--tailnet-credentials-dir` | `SCARPENTER_TAILNET_CREDENTIALS_DIR` | none | A directory with one file per Secret key: `client-id` and `client-secret` (Tailscale OAuth client), or `api-key` (Headscale). Read at every token fetch or call, so a rotated Secret takes effect without a restart (like `--keeper-api-key-file`). |
| `--tailnet-key-ttl` | `SCARPENTER_TAILNET_KEY_TTL` | `1h` | Expiry of a minted key; at least 30 m (core gives 15 m to register, retries come within it; the same reasoning as `--bootstrap-token-ttl`). |

Chart values: `tailnet.backend`, `tailnet.apiURL`, `tailnet.credentials.existingSecret` and the key names
(`clientIdKey`, `clientSecretKey`, `apiKeyKey`), mounted read-only as the credentials directory. The values schema
requires `existingSecret` when `backend` is not `none`. The Secret is mounted by the Pod spec: **no new RBAC on
Secrets**.

Credentials needed (documented in a how-to section of the reference):

- **Tailscale**: an OAuth client with the scopes `auth_keys` (write) and `devices:core` (write; device lookup and
  delete) and the NodeClasses' tags (or a tag that owns them in `tagOwners`). Token: `POST /api/v2/oauth/token`
  (client credentials; tokens last an hour), with `golang.org/x/oauth2/clientcredentials` (already a direct
  dependency). Tailnet path `-`.
- **Headscale**: an API key (`Authorization: Bearer`). Headscale API keys are not scoped: the key can administer the
  whole server. It lives in one Secret mounted into the controller only (decision 4). Headscale **0.28.0 or
  newer** (2026-02-04): pre-auth keys are expired and deleted by ID and need no user, and tags are the node's identity.
  Older servers are refused with a clear message (`GET /api/v1/preauthkey` answers in the 0.28 shape, or the create
  call's error is classified `Fatal` with the version hint).

### 2.3 The interface (decided)

`pkg/tailnet`, no Kubernetes client, plain `net/http` (no Tailscale client library: four calls do not justify the
dependency):

```go
type Backend interface {
    MintKey(ctx context.Context, r KeyRequest) (Key, error)   // KeyRequest{Tags []string; Expiry time.Duration; Description string}
    RevokeKey(ctx context.Context, id string) error           // idempotent: gone or already used is success
    FindDevice(ctx context.Context, q DeviceQuery) (Device, error) // DeviceQuery{IP netip.Addr; Hostname string; KeyIDs []string}
    DeleteDevice(ctx context.Context, id string) error        // idempotent: gone is success
}

type Key struct { ID string; Secret Secret; Expires time.Time }
type Secret string // String, GoString, Format and LogValue print "[redacted]"
```

Every key is minted single use (`reusable: false`), pre-authorised and **ephemeral** (2.5). Errors carry a class
(`Retryable` for 429, 5xx and transport errors, with the `Retry-After` time; `Fatal` for 400, 401, 403, 404 on create;
`NotFound`), and their messages hold the status and the API's error text only, never a request or response body
that contains a key.

| Operation | Tailscale API v2 | Headscale 0.28+ (REST through grpc-gateway, `/api/v1`) |
|---|---|---|
| mint | `POST /api/v2/tailnet/-/keys` `{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":true,"preauthorized":true,"tags":[...]}}},"expirySeconds":3600,"description":"scarpenter <nodeclaim>"}` → `id`, `key`, `expires` | `POST /api/v1/preauthkey` `{"reusable":false,"ephemeral":true,"expiration":"<RFC 3339>","aclTags":[...]}` (no `user`) → `preAuthKey.id` (uint64), `preAuthKey.key`, `preAuthKey.expiration` |
| revoke | `DELETE /api/v2/tailnet/-/keys/{id}` (a one-off key is revoked by Tailscale on use; the call cleans the list) | `POST /api/v1/preauthkey/expire` `{"id":<id>}` |
| find device | `GET /api/v2/tailnet/-/devices` → `nodeId`, `hostname`, `addresses`, `tags` | `GET /api/v1/node` → `id`, `ipAddresses`, `name`, `preAuthKey.id`, `tags` |
| delete device | `DELETE /api/v2/device/{nodeId}` | `DELETE /api/v1/node/{nodeId}` |

Headscale's device match is exact: the node's `preAuthKey.id` is one of the NodeClaim's key IDs. Tailscale's devices
do not name the key that admitted them, so the match is the Node's tailscale IPv4 in `addresses`, cross-checked by
`hostname` (the NodeClaim's name) and tags; without the Node, `hostname` plus tags plus `created` after the NodeClaim
(an ambiguous match is not deleted: Warning Event, metric).

A fixed client-side limiter (10 requests per second, burst 20, not a flag) per backend, and after a 429 every call
fails fast with `Retryable` until `Retry-After` (default 10 s) has passed, so a scale-up of a hundred nodes does not
hammer the API.

### 2.4 Key lifecycle (decided)

Modelled on the bootstrap token ([bootstrap tokens](../../../reference/node-bootstrap.md#bootstrap-tokens)), except
that the key itself is **never stored in a Kubernetes object** (the token is, as a `bootstrap.kubernetes.io/token`
Secret, because the API server reads it there).

1. **Mint** in `CloudProvider.Create`, after the bootstrap token and before the first vendor call, only for a
   `PerNode` NodeClass. The key goes into every attempt's rendering of this Create (the renderer takes it from
   `RenderRequest.TailnetKey`, which wins over `Inputs.AuthKey`).
2. **Record** before the vendor call: a JSON merge patch of the NodeClaim's annotation
   `scarpenter.sparecores.com/tailnet-keys` = `<id>@<expiry unix>[,<id>@<expiry unix>...]` (IDs only). Core's
   NodeClaim patch is a plain merge patch from its own copy, so it does not remove the annotation (verified in core
   v1.15.1, `nodeclaim/lifecycle/controller.go`). A failed patch revokes the key and fails the launch `Retryable`:
   a key that is not recorded is never put on an instance.
3. **Reuse**: an in-memory map NodeClaim UID → `Key` (leader only; Create runs on the leader). A retried Create of the
   same NodeClaim reuses the key while it has at least 20 minutes left: an instance that a replayed idempotency key
   returns booted with it. A key with less left is not revoked (an instance may still be booting with it) and a new
   one is minted and appended. After a controller restart the map is empty: a new key is minted and appended; the
   earlier IDs stay valid until registration or expiry for the same reason. Entries whose expiry has passed are
   dropped from the annotation when it is next written. The map entry is removed when the NodeClaim is Registered or
   gone. So a NodeClaim holds one key per controller restart during its launch, at most.
4. **Failure without an instance** (the same path that deletes the bootstrap token: not `Retryable`): revoke every key
   of the annotation, best effort.
5. **Registered**: the new controller `pkg/controllers/tailnetkey` (the shape of `bootstraptoken`: watch NodeClaims,
   act on `Registered` and on deletion) revokes every recorded key, finds the device (2.3) and writes
   `scarpenter.sparecores.com/tailnet-device` = the device ID. Event `TailnetKeyRevoked` (Normal) on the NodeClaim.
   Failures requeue with backoff; nothing blocks the node.

Expiry 1 h, single use: the window in which a stolen key is useful is from the vendor's API accepting the user-data
to the node's own `tailscale up`, typically under two minutes, and it admits one device with the NodeClass's tags.

### 2.5 Devices: ephemeral, deleted on NodeClaim deletion (decided)

- **Delete**: in `CloudProvider.Delete`, after the driver reports the instance gone (before Delete returns
  `NodeClaimNotFoundError`), delete the device of the annotation (or found by 2.3 when the annotation is missing).
  One try per Delete call; a failure publishes `TailnetDeviceDeleteFailed` (Warning) and counts the metric but does
  not hold the NodeClaim's termination (a tailnet outage must not block Karpenter); the ephemeral flag is the
  backstop. Success: `TailnetDeviceDeleted` (Normal).
- **Ephemeral, yes.** The coordination server removes an ephemeral device that has been offline for a while
  (Tailscale: "30 to 60 minutes after the last activity"; Headscale: `node.ephemeral.inactivity_timeout`, 30 m in the
  example config). That covers what the Delete path misses: a controller that was down, a NodeClaim removed by force,
  an orphan instance the garbage collector deleted. Reboots are safe: tailscaled keeps its node key in its state
  directory on `/var` (persistent on Flatcar; **unverified** for the sysext's `tailscaled.service`, step 5), so a node
  back within the timeout is the same device with the same IP, and `tailscale up --auth-key=...` on a node that is
  already logged in does not use the key (**unverified**, step 5). The cost: a node cut off for longer than the
  timeout (a network partition, a suspended VM) loses its tailnet identity for good, cannot log in again with its
  used key, and stays `NotReady` until its NodeClaim is deleted (manually, or by core's node repair where enabled).
  For benchmark nodes that live for one job this is the right trade. Stopped instances are already treated as gone
  by the driver contract (the stopped-node rule), so retention (ADR-024) does not stop a node with a tailnet identity.
- **No device sweeper** in this plan: nothing on a device says "this cluster" (tags may be shared with other
  clusters or hosts), so a sweep could delete what is not SCarpenter's. Ephemeral removal does that job (open
  question 3).

### 2.6 Failure handling (decided)

- **Mint fails** (API down, 429, 5xx, bad credentials): Create returns
  `cloudprovider.NewCreateError(err, "TailnetKeyUnavailable", <message>)`; core sets `Launched=Unknown` with that
  reason and retries (core v1.15.1 `launch.go`: a `CreateError` is not deleted, unlike `InsufficientCapacity`).
  Warning Event `TailnetKeyFailed` on the NodeClass (deduplicated 10 m, like the other NodeClass Events). If the
  NodeClaim does not register within core's 15 minutes, core replaces it: no special case. **The condition message
  ends up in the NodeClaim's status**, so it is built only from the classified error (2.3), never from a body.
- **Configuration**: the NodeClass controller sets `ValidationSucceeded=False` with the new reason
  `TailnetKeysNotConfigured` (backend `none`, no credentials directory, `loginServer` that does not match, no tags) for a
  `PerNode` NodeClass; Create then fails `NodeClassNotReady` as for any invalid NodeClass. It does not call the API
  (the first launch reports bad credentials through the Event; decision 5).
- **Rate limits**: 2.3 (limiter, fail fast until `Retry-After`).
- **Idempotency**: 2.4 (reuse, at most one key per controller restart per NodeClaim, every recorded key revoked at
  registration or on a failed launch, expiry as the backstop).

### 2.7 Security (decided)

- The key is never logged, never put in an Event, a condition, an error string, a metric label or a Kubernetes object;
  only its ID is. `tailnet.Secret` redacts itself in every formatter; a test renders every log line, Event and error
  of the L2 suite through a sink and fails if a minted key's secret appears. The user-data is not logged (unchanged;
  the fake driver keeps it in memory for tests only).
- On the node the key stays in `/etc/scarpenter/tailscale-authkey` (0600): after the join it is used and revoked, so
  removing it would gain nothing and would make the every-boot `tailscale up` depend on a missing file.
- **ADR-025**: the metadata restriction stays the defence for the bootstrap token (deleted at registration) and for
  vendor credentials. With per-node keys the tailnet no longer depends on it: a `bench` NodeClass that needs
  `instanceMetadata.access: Open` (a benchmark that reads the instance's identity) leaks only a used key. Untrusted
  Pods run only after registration, by which time the key is revoked.
- **What it does not stop**: a compromised node keeps its own tailnet identity until it is deleted (the tailnet ACL
  on the tags limits what it reaches, as today); the controller's credentials are stronger than the static key they
  replace (Tailscale: scoped to keys, devices and the client's tags; Headscale: full admin) and are only in the
  controller's Pod.
- **RBAC**: none new. The credentials are a mounted Secret; SCarpenter's role already patches NodeClaims (the
  annotations) and creates Events. `rbac.authKeySecrets` stays for `Static`.
- **User-data size**: the per-node key replaces the static one in the same file; a Tailscale key is about 60
  characters and a Headscale 0.28 key (`hskey-auth-<prefix>-<secret>`) about 90, the same order as a static key.
  The trial rendering uses a 128-character placeholder; no change to the 12 KiB budget.

## 3. Facts and their status

| Fact | Status |
|---|---|
| Tailscale create key body (`capabilities.devices.create.{reusable,ephemeral,preauthorized,tags}`, `expirySeconds`, `description`), response `id`, `key`, `expires`; delete `DELETE tailnet/-/keys/{id}`; devices list and `DELETE /device/{id}`; device fields `nodeId`, `hostname`, `addresses`, `tags`, `created` | verified in `tailscale-client-go-v2` (`keys.go`, `devices.go`); the L5 lanes already mint with `expirySeconds: 3600` |
| OAuth scopes `auth_keys`, `devices:core`; keys from an OAuth client must be tagged; token URL, 1 h tokens; tailnet `-` | verified, Tailscale KB "OAuth clients" |
| One-off keys are revoked after use; revoking a key does not remove its devices; tagged devices have key expiry disabled | verified, Tailscale KB "Auth keys" |
| Ephemeral removal 30 to 60 min offline | verified, Tailscale KB "Ephemeral nodes" |
| Minimum `expirySeconds` through the API, description length and charset, rate limits and `Retry-After` | **unverified** (step 2: from the API docs; step 6 if run live) |
| Headscale 0.29.7: `CreatePreAuthKeyRequest{user uint64, reusable, ephemeral, expiration, acl_tags}`, `POST /api/v1/preauthkey`, `POST /api/v1/preauthkey/expire {id}`, `DELETE /api/v1/preauthkey`, `GET /api/v1/node`, `DELETE /api/v1/node/{node_id}`; `Node.pre_auth_key`, `ip_addresses`, `tags`; Bearer API key | verified in the proto files at v0.29.7 and `docs/ref/api.md` |
| Headscale differences: 0.23 `user` is a name and expire takes user and key; 0.26 and 0.27 `user` is an ID and expire still takes user and key; 0.28 expire and delete by ID, `user` optional, tags as identity (tagged keys make tagged nodes, `--advertise-tags` ignored for them), tagged nodes exempt from expiry | verified in the protos and the CHANGELOG |
| Headscale accepts `user: 0` (absent) with tags over the REST API; JSON field names of grpc-gateway (`aclTags`, `preAuthKey`) | **unverified** (step 2 against the L4 lane's Headscale) |
| tailscaled state on `/var` with the sysext's unit; `tailscale up --auth-key` with a used key on an already logged-in node succeeds after a reboot | **unverified** (step 5, L4) |
| The live cluster's Headscale is 0.28 or newer | **unverified** (decision 1) |

## 4. Steps (one agent each, each ending green)

1. **Done: API, configuration, ADR.** (Done: `keySource` and the optional `authKeySecretRef` with their CEL rules, the hash
   normalisation and golden test of the example NodeClasses, the four flags and the chart values and schema, the
   `TailnetKeysNotConfigured` check and the placeholder trial rendering, `Create` refusing `PerNode` with `NodeClassNotReady`,
   ADR-028 and the docs. The step needed no backend interface, so none was defined.) `keySource`, optional `authKeySecretRef`, the CEL rules and CRD tests; the hash
   normalisation and a golden test that every stored example NodeClass keeps its hash; flags, chart values and schema,
   the mount; the NodeClass controller's `TailnetKeysNotConfigured` checks and the trial rendering with the
   placeholder (a `PerNode` NodeClass validates, nothing mints yet; Create refuses `PerNode` with `NodeClassNotReady`
   until step 3). Write the ADR (next free number, ADR-028 if plan 26 took no further one): per-node keys, controller-wide
   backend, ephemeral, no sweeper. Update overview §8.3, §8.6, the deferred list and Q8. Docs: SCNodeClass reference,
   node bootstrap (network templates), controller reference (flags, chart values).
2. **`pkg/tailnet` backends.** The interface, `Secret`, error classes, limiter and `Retry-After`; the Tailscale backend
   (client credentials, keys, devices) and the Headscale backend (0.28+); in-memory fakes of both APIs as
   `httptest` servers in `pkg/tailnet/tailnettest` (they record calls, enforce single use, expiry, tags and Bearer
   auth, and can inject 429, 5xx and slow answers). L1 tests per backend: mint, revoke idempotent, find by IP / key
   ID / hostname, ambiguous match, delete idempotent, 429 with and without `Retry-After`, 401, the redaction test. One
   extra L1 test runs against a real Headscale binary when `HEADSCALE_BIN` is set (the L4 lane's cached 0.29.7;
   skipped otherwise) to settle the unverified Headscale rows of section 3.
3. **Launch.** The key manager (map, annotation, reuse, revoke on a failed launch) in `pkg/tailnet` (pure, with a small
   `Annotator` seam) wired into `CloudProvider.Create` and the renderer; Event `TailnetKeyFailed`, metrics. L2 envtest
   with the fake API: one key per NodeClaim, reuse across a `Retryable` retry, a new key after a simulated restart with
   both IDs recorded, revoke on a `Fatal` launch, no key in any object, `Static` NodeClasses render byte-for-byte as
   before (golden). L3 KWOK: a `PerNode` NodePool scales to 20 with the fake API answering 429 for a while; every
   NodeClaim launches, at most one live key each.
4. **Registration and deletion.** `pkg/controllers/tailnetkey` (revoke on `Registered`, device annotation), the device
   delete in `CloudProvider.Delete`, Events `TailnetKeyRevoked`, `TailnetDeviceDeleted`,
   `TailnetDeviceDeleteFailed`; metrics. L2: registration revokes all IDs and records the device; deletion deletes it;
   a failing API does not hold the termination; a missing annotation falls back to the lookup. L3: scale up and down,
   the fake API ends with no keys valid and no devices.
5. **L4 with the lane's Headscale.** `run.sh` also creates a Headscale API key (CLI, git-ignored run directory) and
   starts the controller with `--tailnet-backend headscale`; a new scenario with a `PerNode` NodeClass: the VM joins
   with a single-use, tagged, ephemeral key; the key is expired after registration and a second join with it fails;
   the node survives a reboot as the same device (state, `tailscale up` with a used key); deleting the NodeClaim
   removes the node from Headscale. The existing Q09 (static key) stays. No credentials, no external network beyond the
   lane's.
6. **Live (optional, each run needs the maintainer's approval).** (a) Against the operator's Headscale: one `bench`
   NodeClass switched to `PerNode` on a test NodePool, one node up and down, checked in Headscale's node and key
   lists; credentials from the coordinator, never in the repository. (b) Tailscale SaaS: the same with an OAuth client
   in a test tailnet, or not at all (decision 6). Record the unverified Tailscale rows of section 3.

Each step updates in the same change: the SCNodeClass reference, node bootstrap, the controller reference (flags,
chart values, Events, metrics, conditions, RBAC), the labels reference (the two annotations) and, from step 4, a
how-to section "Per-node tailnet keys" (credentials for each backend, the ACL for the tags, what an operator sees).

**Metrics**: `scarpenter_tailnet_api_calls_total{backend,operation,result}` (`operation`: `mint`, `revoke`, `find`,
`delete`; `result`: `ok`, `error`, `rate-limited`) and `scarpenter_tailnet_api_duration_seconds{backend,operation}`.

## 5. Tests

- **L1**: section 4 step 2; the CEL rules; the hash golden test; the redaction test.
- **L2** (envtest, fake clock, fake API): steps 3 and 4.
- **L3** (KWOK, fake driver, fake API): steps 3 and 4.
- **L4** (qemu, real Flatcar, real Headscale 0.29.7): step 5.
- **L5**: none in CI. The L5 lanes keep the static key they mint (decision 6). Step 6 is a manual live check.

## Not here

A sweeper for orphan devices; per-NodeClass credentials or several tailnets per controller; Tailscale workload
identity federation for the controller (a token file instead of an OAuth client); a scoped Headscale credential;
moving the bootstrap token out of user-data; tailnet ACL management.

## Decisions of the maintainer

Decided by the maintainer 2026-11-20: every recommendation of the former open questions is accepted.

1. **Headscale version**: 0.28 or newer only. Decided by the maintainer 2026-11-20. Older servers are refused with a
   clear message; the operator's Headscale is confirmed to be on 0.28+ before step 6.
2. **Ephemeral**: every per-node key is ephemeral, fixed, no field. Decided by the maintainer 2026-11-20. If
   long-lived nodes behind flaky links appear, `ephemeral: false` can be added later (it changes the hash only when set).
3. **Device sweeper**: none. Decided by the maintainer 2026-11-20. Ephemeral removal covers what Delete misses.
4. **Headscale credential**: the API key with full admin rights is kept only in the controller's Secret (release
   namespace, mounted only into the controller, documented rotation, file re-read at every call). Decided by the
   maintainer 2026-11-20.
5. **Credential check**: none at validation; bad credentials show at the first launch as `TailnetKeyFailed` and
   `Launched=Unknown`. Decided by the maintainer 2026-11-20.
6. **CI lanes**: the L5 lanes keep their static keys; at most one Tailscale SaaS live check (step 6b, in a test
   tailnet, only if an OAuth client with `auth_keys` and `devices:core` can be made for it; otherwise the backend ships
   verified against the fake and the client library only, said so in the reference). Decided by the maintainer
   2026-11-20.
7. **`--tailnet-key-ttl`**: default 1 h. Decided by the maintainer 2026-11-20.

## Gates

CONVENTIONS gates 1 to 4 for every step, gate 5 (L4) for step 5; `hack/check-docs.py` clean; the reference docs of
section 4 in the same change; the drift hash of every stored example NodeClass unchanged (golden); no key secret in
any log line, Event, condition, error or object in the L2 and L3 suites; the `docs/plans/v1/README.md` status
`review`. Report: the unverified rows of section 3 with their results, the Headscale version tested, and, for step 6,
what the live run showed.
