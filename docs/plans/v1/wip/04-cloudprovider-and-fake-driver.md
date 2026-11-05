# 04 — Cloud provider, driver contract, fake driver, GC, L3 lane

Depends on: 03. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: overview §4, §5.9, §7, §10, §13.1–§13.2, §5.7; ADR-013.

## Goal

The vendor-neutral heart of SCarpenter: a complete `CloudProvider` on top of
the driver contract, proven against real Karpenter core with a fake driver
whose "instances" are KWOK nodes. Bootstrap is stubbed (empty user-data and a
no-op token minter behind an interface) until plan 05.

## Scope

- **`pkg/vendors`**: `Driver` and the capability interfaces exactly as
  overview §7.1, request/response types, `InstanceRef` (driver-opaque string
  with region), error classes (§7.2) as typed errors with helpers
  (`vendors.IsClass(err, vendors.InsufficientCapacity)`), logical tag keys,
  the registry (§7.3) and enablement from `--vendors`.
- **`pkg/vendors/conformance`**: the suite of §7.4 as an exported function
  taking a driver factory and a fault-injection hook, run by every driver's
  tests.
- **`pkg/cloudprovider`**: every method of overview §4 for a multi-vendor
  NodeClass (per-vendor readiness and drift hash), including §4.1
  (candidate computation, price ordering with deterministic ties, attempt
  loop, error-to-core mapping, unavailable cache updates, annotations,
  idempotency key) and §4.2 (provider ID format, parsing, lookup through a
  30 s List cache with a driver fallback). Interfaces for the bootstrap token
  minter and the user-data renderer, with stub implementations wired for now.
  Metrics: launches by vendor/region/capacity type/result, launch attempts per
  Create, `Create` latency.
- **`pkg/quota`** (overview §5.9): the tracker over `QuotaReporter` and
  `OfferingLister` drivers — usage and limit refresh loops per (vendor,
  region in scope) and global, debounced refresh triggers (quota error, own
  launch or termination), the in-flight ledger (reserve, release on failure,
  release when counted, 10-minute expiry), headroom per bucket, staleness
  fallback to "unknown", the sequence number shared with the
  unavailable-offerings cache, metrics; and the NodeClass `QuotaAvailable`
  condition and `status.quota` for scopes where every allowed offering is
  blocked (`QuotaExhausted` / `QuotaZero`), with rate-limited Events. Wire it
  into `GetInstanceTypes` (offering availability, launchable-type filtering)
  and `Create` (skip-without-attempt, reserve before the vendor call).
  Clock-injectable; table tests for every rule, including concurrent
  `Create`s competing for the last headroom.
- **`pkg/controllers/garbagecollection`**: overview §10 for instances and
  bootstrap tokens (managed vendor resources come with plan 06), with clock
  injection and dry-run logging.
- **`pkg/vendors/fake`** (build tag `scarpenter_dev`): in-memory instances;
  zones and regions from the impersonated vendor's snapshot
  (`--fake-catalog-vendor`); on create, a Node object for kwok-controller
  (provider ID, labels, `karpenter.sh/unregistered` taint, kwok annotation;
  approach of core's KWOK provider at
  `8fb3abde2be3111cd26e0c0f45ac0babdeb8e9c0`); error-injection rules from a
  ConfigMap (match on type/zone/capacity type/region → error class, optional
  count); simulated launch delay; `QuotaReporter` and `OfferingLister` driven
  by a ConfigMap (buckets, limits, "foreign" usage, launchable types per
  zone, consumption per type derived from vCPUs and a family → bucket map).
  Passes the conformance suite.
- **L3 lane** (`test/kwok/`, `make test-kwok`, CI job): kind cluster,
  kwok-controller, Keeper replay server in-cluster serving the committed
  snapshot, SCarpenter (dev build) via kustomize, scenarios:
  1. Pod with an exact `aws.*` instance type → NodeClaim → Node Ready (KWOK)
     with every expected label; deleting the Pod removes the node
     (consolidation `WhenEmpty`).
  2. Cheapest offering wins among several allowed types and zones.
  3. ICE injected for the cheapest offering → second cheapest launched in the
     same Create; ICE on all attempts → NodeClaim deleted, offerings
     unavailable, next round picks another; TTL expiry makes them available.
  4. Quota, flexible region: the cheapest region's bucket is exhausted
     (foreign usage) → nodes land in the next-cheapest region with headroom,
     with **no** vendor call in the exhausted region; 5 pods needing more
     than the remaining headroom of one region in total spread over regions
     without a single quota error (ledger).
  5. Quota, pinned region: NodePool restricted to one exhausted region → pods
     Pending, no NodeClaim launched, `QuotaAvailable=False` with
     `QuotaExhausted`; lower the foreign usage → nodes provisioned within one
     refresh interval plus a scheduling round, without touching the pods
     (this answers overview §15 Q9; if core needs a nudge, implement and
     document it). Limit 0 → reason `QuotaZero`.
  6. A type not launchable in a zone (`OfferingLister`) is never attempted
     there.
  7. Reactive path for a driver without quota knowledge: injected
     `QuotaExceeded` → region/capacity type skipped for the TTL.
  8. NodeClass change → `Drifted` → replacement.
  9. Orphan: an instance in the fake driver with the cluster tag and no
     NodeClaim is deleted by GC after the grace period.
  10. Scale (overview §5.7, §15 Q7): full AWS catalog, 50 pending Pods with
     distinct exact-type selectors in one NodePool, all regions vs. three:
     time to all NodeClaims created, controller peak memory. Measured and
     reported; asserted only loosely (no timeouts tighter than 10 min).
- **Docs**: `docs/reference/driver-contract.md`; testing how-to updated.

## Gates

CONVENTIONS gates 1–4. Report the scale numbers and a recommendation
(pre-filter enough, or a NodeClass instance-type filter needed), and the
§15 Q9 finding (time from quota freed to node created in scenario 5).
