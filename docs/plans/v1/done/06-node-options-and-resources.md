# 06 — Node options and vendor resources

Depends on: 05. Read `docs/plans/v1/CONVENTIONS.md` first.
Spec: 00a §3, §5, §6, §8–§11 (all of them); overview §4.1, §6, §7.1, §10;
ADR-015, ADR-017, ADR-018, ADR-020.

## Goal

The vendor-neutral machinery behind node settings, profiles and vendor
resources, complete and proven with the fake driver in the L3 lane, so that
every real driver only fills in its own blocks: settings resolution, profile
feasibility, `Prepare`, the `use`/`create` resource framework with its
lifecycles and GC, pricing deltas, and the first-boot volume unit.

## Scope

- **`pkg/options`**:
  - Option registry (00a §5.1): profile-name keys (`storage-profile`,
    `network-profile`, `placement-profile`) as enums from the NodeClass, the
    `placement-group` run key as a free slug; registration as well-known
    labels in the restricted domain.
  - Settings resolution per vendor: driver `Defaults()` ← NodeClass vendor
    block ← selected profiles (00a §3), deterministic; the applied profile
    names (or `default`) as labels.
  - The `Supported` hook of plan 03 now asks the driver's `NodeOptions`
    which profiles each instance type (and zone) can host; a profile without
    a block for a vendor is absent on that vendor (ADR-017).
  - Pricing deltas (00a §10) from the driver's `PriceDelta`;
    `price-incomplete` annotation; `pricing.allowIncomplete: false` excludes
    such offerings.
  - Per-profile drift hashes (overview §6.3): changing one profile drifts
    only the nodes that use it.
- **`pkg/resources`** (00a §9):
  - The `ManagedResources` capability contract and a generic engine:
    `use` resolution (IDs per region, `{region}`/`{zone}` name patterns, tag
    selectors), the driver's vendor-default resources (00a §9.2), `create`
    with find-or-create by deterministic name and ownership marker,
    adoption after restart, serialisation per (vendor, kind, region),
    `forbid`.
  - Lifecycles: `perUse` (2-minute grace), `idleTTL` (default from
    `--resource-default-ttl`, 24 h), `retain`; `onDemand` and
    `preCreate: [regions]` timing; deletion in the driver's dependency order
    with retries; never touching `use` resources.
  - Status per vendor and region (`status.vendors.<v>.resources`,
    `ResourcesReady`); offerings in a region with a missing or failed
    required resource are unavailable with a reason.
  - Metrics and Events for every create, adoption and deletion.
- **`Create`** (overview §4.1): resolve settings, run `Prepare` (placement
  constructs per run key, separate volumes, per-run networks) before the
  vendor call, pass resolved settings and prepared resources in
  `CreateRequest`, stamp profile labels.
- **GC** (overview §10): managed resources by lifecycle, added to the plan 04
  collector.
- **First-boot volume unit** (00a §11): an Ignition-rendered systemd unit
  that, from device hints (by-id path, or vendor serial pattern + size),
  creates the filesystem label once (never wiping a filesystem it did not
  create) and lets Ignition-generated mount units mount by label; also
  `localStorage: mount | raid0` for instance storage.
- **Fake driver**: `NodeOptions` and `ManagedResources` for three resource
  kinds (a regional "network" with `idleTTL`, a per-run "group" with
  `perUse`, a per-node "volume"), storage and placement profile blocks, a
  configurable "not supported on these types" rule, and fake device hints.
  Passes the extended conformance suite (overview §7.4).
- **L3 scenarios** (`test/kwok/`), each with an assertion on the fake
  driver's state:
  1. Exact placement: a Job with vendor, type, region and capacity type gets
     exactly that offering; with hostname anti-affinity and a taint, one pod
     per node.
  2. Described, not named: `vcpus` range + `metric-…-pct` + `continent`
     selects only matching types; `selection In [<profile>]` only members.
  3. A storage profile with a block for the vendor is applied (resolved
     settings visible in the fake instance, labels on the node); a profile
     without a block for the vendor leaves the pod Pending; an unsupported
     type for a profile is never chosen.
  4. The 00a §8.2 pair: a StatefulSet and a Deployment with one placement
     profile and run key share one fake group, land in one zone, on two
     nodes; the group is deleted (perUse) after both are gone.
  5. Resources: `use` by tag selector in two regions (never deleted); a
     missing `use` resource makes the region's offerings unavailable with a
     reason; default `create` on the first launch in a region, adoption after
     a controller restart, `idleTTL` deletion after the TTL (clock shortened
     for the test), `preCreate` keeps resources while listed, `forbid` makes
     offerings unavailable.
  6. Incomplete price: offerings flagged and still used; excluded with
     `allowIncomplete: false`.
- **Docs**: `docs/reference/node-settings.md` (layers, profiles, labels,
  resolution order, drift), `docs/reference/vendor-resources.md` (kinds,
  `use`/`create`/`forbid`, lifecycles, timing, status, GC rules), how-tos
  `write-profiles.md` and `bring-your-own-resources.md`.

## Carried over from the plan 04 review

- The fake driver's debounced state write is not retried on failure (review
  N17), and L3 helpers wait a fixed 1.2 s for it; make the write reliable and
  replace the fixed wait with a revision-acknowledged wait.
- A replay of an idempotency key that returns a different instance than the
  attempted candidate commits the attempted candidate's quota reservation;
  reconcile the reservation with the returned instance.
- The catalog fallback path in `Create` sets no price annotation; set it (with
  `price-incomplete` where applicable).

## Carried over from the plan 05 review

- kube-reserved: the kubelet reserves only what `kubelet.kubeReserved` sets, while
  the catalog assumes the AWS provider's per-type formula; decide between
  per-type rendering and a boot-time unit that computes it, so that the node's
  allocatable matches the scheduler's.
- The template variables `.Zone` and `.InstanceType` are empty because user-data
  is rendered once per (vendor, region, arch). If node settings or device hints
  need per-type data, revisit that rule and the user-data size budget
  (`docs/reference/node-bootstrap.md`).

## Gates

CONVENTIONS gates 1–4. Report §15 Q11 (confirmed in L3 with real core) and the
L3 runtime added by these scenarios.
