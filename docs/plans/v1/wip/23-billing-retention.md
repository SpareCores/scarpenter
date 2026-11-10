# 23 — Billing-period-aware node retention

Depends on: 14 (the stopped-node rule A4, the contract changes it makes, the `SCNodeClass` API
work), 03 (the catalog) and the Keeper vendor data: the **minimum** is the small Keeper client
step below (`Vendors()` over the live `/table/vendor` route and the catalog reading it). The vendor
table is already live, so this plan does **not** wait for plan 13's snapshots; plan 13 part B later only swaps
the transport. It is useful with the fake driver alone (an impersonated vendor with an hourly rule), and is
turned on per real vendor by that vendor's plan once its live check passes (15, 18, 19, 20). Read
`docs/plans/v1/CONVENTIONS.md` first. Spec:
[ADR-024](../../../explanation/DECISIONS.md#adr-024-billing-period-aware-node-retention-accepted-2026-11-10)
(read it fully: it is the design), overview §5.1 and §7, plan 13 "Vendor billing data".

Written 2026-11-10. The decisions are in ADR-024; this plan is the work order.

## Goal

An opt-in `SCNodeClass.spec.billing` makes SCarpenter keep a node that a vendor bills by the started period
(hour on hcloud, UpCloud, Vultr) until the end of its paid period, or, on a vendor that bills a minimum and then
per second (Alibaba), until the end of the minimum, so Karpenter reuses it for the next job, and delete
it before the next charge starts. A job that runs a minute every ten minutes then pays one hour per hour, not six.
Off by default; no effect on per-second vendors with no worthwhile minimum, spot, static pools, nodes the operator
protects (`consolidateAfter: Never`, a user `do-not-disrupt`) or unverified vendors. The first vendor is Hetzner (plan 15).

## Scope

### 1. Keeper client and catalog: billing facts

- **`sc-keeper-go`** (its own working tree; same rules as plan 01): `Vendors(ctx)` over `GET /table/vendor` with the
  version pinning of the other table methods; a `Vendor` type with `VendorID`, `Status`, `StoppedServerCharged *bool`,
  `BillingIncrementSeconds *int`, `MinimumBillingSeconds *int`, `BillingComment *string` (null is legal). A recorded
  fixture and a test with null fields.
- **`pkg/catalog`**: read the vendor table with the rest of the refresh (one small call), keep it in the snapshot as
  `model.Vendor`, expose `catalog.BillingFacts(vendor) (BillingFacts, ok)`. A failed or missing vendor table never fails
  the refresh: retention reports `NoKeeperData` for the vendor. `catalogtest` gains the vendor table (with null
  fields) so every test can set it.
- **Rule resolution** (pure function, `pkg/billing`): `Resolve(keeper BillingFacts, override *vendors.BillingRule)
  Rule{Increment, Minimum, StoppedCharged, DeleteLatency, Source, Verified}`. The driver's override wins field by field
  and is never lower than what the vendor row says without a comment in the code that links the source. A rule is
  **qualifying** when `Verified`, the capacity is on-demand and either `Increment >= spec.billing.minPeriod` (increments are
  held) or `Minimum - margin >= 2m` with the effective margin of section 4 (the hold floor, a constant: a shorter window is not worth a hold; with the default
  margin a 5-minute minimum holds 2 minutes, a 10-minute one 7, 120 seconds never).

### 2. Driver contract

- `Instance.BillingStart time.Time`: the vendor's own creation or billing-start stamp; zero when the vendor has
  none and the driver could not record one. `CreatedAt` is unchanged (it is not always the billing start). The
  driver sets the earliest plausible stamp: an early stamp is the safe side (ADR-024). AWS sets `LaunchTime`
  (per second, so unused), hcloud `created`, OVH Nova `created`, Vultr `date_created`, UpCloud its own
  `scarpenter-created` label (plan 19).
- `Create` returns `BillingStart` as the NodeClaim annotation `scarpenter.sparecores.com/billing-start` (RFC 3339).
  Karpenter core v1.15.1 merges the annotations the cloud provider returns into the NodeClaim
  (`PopulateNodeClaimDetails`) and copies the NodeClaim's annotations to the Node at registration, so the controller
  reads it without a vendor call and a restart loses nothing. A NodeClaim without it (launched before this plan)
  gets it once from `Get`.
- Optional capability `BillingDescriber` (next to `QuotaReporter`):
  `BillingRule(region, family string) (BillingRule, bool)` where `BillingRule` has optional `Increment`, `Minimum`,
  `StoppedCharged`, `DeleteLatency` and `Verified bool` plus a `Source` string for the status. A driver that does not
  implement it uses Keeper's row and is **unverified**: retention is off for it. The core never branches on a
  vendor name.
- Conformance: `BillingStart` is never after the instance's first `GetInstance`, is stable across calls and survives
  `FindInstance`/`ListInstances`; zero is allowed and documented. The fake driver gets a fixture hook to set the vendor's
  rule and the start stamp (from the fake clock).

### 3. API and validation

```yaml
spec:
  billing:
    retainUntilPeriodEnd: false   # bool, default false
    margin: 3m                    # metav1.Duration, default 3m
    minPeriod: 10m                # default 10m: gates holding per increment, not the minimum hold
    # maxRetainedNodes: 20        # *int32, unset by default: an optional safety valve
```

- CEL: `margin >= 90s` (`duration(self.margin) >= duration('90s')`), `minPeriod >= 1m`, `maxRetainedNodes >= 1` when set (unset means no cap); the block
  is optional and every field defaulted. Cross-field and vendor rules (`minPeriod` below twice the margin, a period shorter than twice the
  margin on a vendor, a minimum under the hold floor) are conditions, not CEL: `BillingRetention` on the NodeClass.
- Status: `status.billing` lists, per vendor in the NodeClass, `{vendor, active, reason, period, source}` with reason one of
  `Disabled`, `PerSecondBilling` (no minimum worth holding), `MinimumTooShort` (`minimum - margin` under the 2-minute hold floor
  and no held increment), `Unverified`, `NoKeeperData`, `NoBillingStart` (the driver records no stamp for the vendor), and `Active` (its message says `increments` or `minimum only`). The same
  condition counts the nodes left alone, per reason (`ConsolidationNever`, `UserDoNotDisrupt`). `NodeClassValidator`
  can add reasons of its own (a region where the rule differs).
- Changing `spec.billing` is **not** drift (the nodes are the same); turning it off releases every hold at the next reconcile.
- Regenerate the CRD and `docs/reference/api.md` (`make generate manifests`).

### 4. The retention controller (`pkg/controllers/retention`)

Per Node owned by a NodeClaim (watch Nodes, NodeClaims and, through a field index on `spec.nodeName`, Pods; the NodeClass
for its `billing` block and its vendor). Idempotent reconcile with no in-memory state; time comes from `clock.Clock`.

Qualification (anything else: remove an annotation SCarpenter set, and do nothing, a node left alone gets a once-only Event
`RetentionSkipped` with the reason): `retainUntilPeriodEnd`; the vendor's rule
qualifies (section 1); on-demand; the NodePool is not static and not `consolidateAfter: Never` (left completely alone: no hold, no
delete, no release under pressure); the Node is registered (Initialized is not needed:
Karpenter never disrupts a node that is not, so the hold is on before it can be a candidate); the NodeClaim is
not `Drifted` and not being deleted; the node has no `do-not-disrupt` that SCarpenter did not set; `BillingStart` is known
(else an Event `BillingStartUnknown` once, never a guess) and not in the future beyond a small skew allowance.
(`maxRetainedNodes` is not a qualification: it trims held *empty* nodes, step 4.)

The arithmetic is one pure function covering both shapes:
`Window(start, rule, minPeriod, margin, now) (boundary, deadline time.Time, inMargin, last bool)`. With increments held
(`Increment >= minPeriod`) the first boundary is `start + max(Minimum, Increment)` and later ones add the increment; without,
the only boundary is `start + Minimum` and `last` is true (a per-second vendor after its minimum). `deadline = boundary - margin`, where `margin` is the effective
one: the larger of `spec.billing.margin` and the rule's `DeleteLatency` plus 90 s; `inMargin` is
`now >= deadline`. The anchor is always `start`: there is no `anchor` field (decided 2026-11-10: no vendor documents clock-hour billing).

Per reconcile of a qualifying node:

1. Before the deadline: put `karpenter.sh/do-not-disrupt: "true"` and `scarpenter.sparecores.com/retain-until=<deadline>`
   on the Node (at registration, not at emptiness: Karpenter at `consolidateAfter: 0s` would otherwise pick a node the moment it
   empties). Requeue at the deadline.
2. At the deadline: if the node is **empty** (no pod except DaemonSet, static and terminal pods: the same notion as Karpenter's
   reschedulable pods) and Initialized (a node not yet Initialized may have been launched for pods that have not bound yet: it is
   treated as busy, and is never deleted by SCarpenter), ask the budget: the NodePool's allowed disruptions for reason `Empty`, computed with Karpenter core's
   own budget helper (nodes already deleting count). Allowed: put the `NoSchedule` taint `scarpenter.sparecores.com/releasing` on the Node, re-read the pods from the API server (not the
   informer cache, which can lag a fresh binding), and
   delete the NodeClaim (Event `ReleasedAtPeriodEnd`, metric); a pod found on the re-read makes it busy (taint removed). **Blocked by
   the budget**: do nothing, Event `ReleaseBlockedByBudget` and the metric, and let the node pass the boundary held (one more period is paid; the
   how-to says so); step 3 gives the next deadline. If it is
   **busy**, remove the hold for the window `[deadline, boundary)` (Karpenter may then replace it) and requeue at the boundary;
   a pod event that leaves it empty inside the window takes the empty path at once.
3. After the boundary: back to step 1 for the next period. At the **last** boundary (`last`, a minimum-then-per-second vendor) there is no
   next period: an empty node is deleted at the deadline as above, a busy one, or one blocked by a budget, gets its hold removed for good and
   becomes an ordinary node (Karpenter's consolidation, under the same budget, decides).
4. **Release under pressure.** The controller keeps a small signal per scope: a launch blocked by a NodePool limit (pods pending with
   the pool's `status.resources` at `spec.limits`) or by a vendor quota (the quota tracker refused `Create`, reported to the controller
   by the cloud provider). On a signal it releases held **empty** nodes of that scope, only as many as cover the request, in the order
   **nearest deadline first, ties by oldest start** (the least paid time left is lost; for equal periods it is the oldest-paid-up node), with the same
   budget check, taint-and-re-read and Event (`ReleasedUnderPressure`). **A candidate blocked by its budget** is skipped (Event
   `ReleaseBlockedByBudget`) and loses its hold, so it is an ordinary empty node that Karpenter's own emptiness deletes when the
   budget opens; the next candidate in order is tried. The blocked launch then waits for the budget exactly as it would without
   retention (Karpenter could not have deleted that node either): SCarpenter adds no wait and no lock of its own, and the signal is
   re-evaluated on every reconcile while it lasts. With `maxRetainedNodes` set, the same order trims the held empty nodes
   of the NodeClass down to it. Nodes left alone are never released.

Rules the controller keeps: only annotations it set are removed (marker annotation present); no hold and no delete when the
NodePool says `consolidateAfter: Never`; a Drifted NodeClaim loses its hold; a `terminating` or `stopped` instance is
never held (plan 14, A4, applies); its deletes respect the NodePool's disruption budgets (ADR-024, decided 2026-11-10) and a foreign
`do-not-disrupt`. RBAC: `nodes` get, list, watch, patch; `nodeclaims` get, list, watch, delete; `pods` get, list, watch;
Helm chart and the permissions section of the controller reference change with it. A flag `--billing-retention` (default
on; the NodeClass field is the real switch) turns the controller off as a whole.

On start the controller logs, once per vendor, the rule it resolved (period, source, verified) so that an operator can see it.

### 5. Observability

- Events on the Node: `RetainedUntil`, `ReleasedAtPeriodEnd`, `ReleasedUnderPressure`, `ReleaseBlockedByBudget`, `RetentionSkipped`, `BillingStartUnknown` (Warning).
- Metrics (all in the controller reference): `scarpenter_billing_retained_nodes{vendor,nodeclass}` (gauge),
  `scarpenter_billing_retained_seconds_total{vendor}`, `scarpenter_billing_reuse_total{vendor}` (a node that was empty
  under the hold receives a pod), `scarpenter_billing_periods_saved_estimate_total{vendor}` (equals the reuses: an estimate,
  not an invoice), `scarpenter_billing_deletes_at_deadline_total{vendor}`, `scarpenter_billing_blocked_by_budget_total{vendor}`,
  `scarpenter_billing_released_under_pressure_total{vendor,scope}`, `scarpenter_billing_skipped_nodes{vendor,nodeclass,reason}` (gauge),
  `scarpenter_billing_boundary_missed_total{vendor}` (an empty node alive 30 s past its boundary: the margin is too small).
- NodeClass status as in section 3. Log: vendor, `BillingStart`, period, deadline, decision.

### 6. Tests

- **Pure function**: table tests of `Window` and `Resolve`: exactly on a boundary, inside the margin, many periods in, a first
  boundary from a minimum above the period, zero start, null Keeper fields, override over Keeper, per-second vendors with and
  without a minimum, minimum-only rules (600 s with the default margin holds 7 minutes; 300 s holds 2; 120 s is `MinimumTooShort`;
  `last` is true), the hold floor at exactly `minimum - margin = 2m`, unverified.
- **Controller (L2, envtest)** with `k8s.io/utils/clock/testing.FakeClock` and the fake driver with an impersonated hourly vendor:
  advance the clock across the deadline: annotation on, off, on again after the boundary; a restart in the middle changes
  nothing; no hold for a per-second vendor with no minimum, spot, a static pool, `retainUntilPeriodEnd: false`, a minimum under the hold floor, a
  Drifted claim, `consolidateAfter: Never`, a foreign `do-not-disrupt` (and that one is never removed, no Event spam: once);
  `maxRetainedNodes` unset (no cap) and set (trims in deadline order); an
  empty node is deleted at the deadline with a budget that allows it, and **held into the next period** with a budget of 0 and deleted at
  the next deadline once the budget opens; a limit-blocked and a quota-blocked launch each release the nearest-deadline empty node first,
  and not a node of another pool or quota; a pressure release blocked by a budget drops that node's hold and takes the next candidate;
  an empty node not yet Initialized at the deadline is not deleted; a minimum-only rule: the node is deleted at `start + minimum - margin` and a busy one loses its hold for good; a busy node rolls over; a pod that lands in the race is evicted
  with the claim and the metrics move.
- **Conformance** (fake and AWS): `BillingStart` rules of section 2. Catalog tests for the vendor table, nulls and a missing table.
- **L3 (kind and KWOK, core's clock is real)**: the fake driver's vendor rule is shortened (period 5 minutes, margin 90 s, a hook
  on the fake): (a) a job every minute-or-two reuses **one** node and no second NodeClaim appears; (a2) a vendor with a 10-minute minimum and per-second
  increments (fake rule shortened to 5 minutes for the lane): a job every 3 minutes reuses one node and the node is gone right after the
  minimum, so the next job gets a fresh one;  (b) a node kept busy across
  the deadline is not deleted and holds again after the boundary; (c) an empty node is gone before the boundary; (d) a per-second
  vendor ignores the setting; (e) `expireAfter` shorter than the period still removes the node, and an `expireAfter` of a multiple of the period minus
  the margin ends it before a boundary; (f) a budget of 0 for `Empty` holds an empty node into the next period and it goes at the next deadline once the budget opens;
  (g) a NodePool at its `limits` with a held empty node: the pending pod gets the capacity after SCarpenter releases it.
- **Per-vendor L5 lanes** (plans 15, 18, 19, 20): assert delete latency and instance state only; billing cannot be asserted
  from the API (the invoice is the check, the plans list what to read).

### 7. Docs

[Reuse nodes within the billing period](../../../how-to-guides/reuse-nodes-within-billing-period.md) (written with
this plan's design; update it with what is verified), the `SCNodeClass` API reference (generated), the controller reference
(flag, RBAC, metrics, Events, annotation, status), the driver contract reference (`BillingStart`, `BillingDescriber`), the
node-settings reference link, the labels reference (the annotations `scarpenter.sparecores.com/retain-until` and `billing-start`, the taint
`scarpenter.sparecores.com/releasing`), each vendor's reference gets its
verified rule when its plan's live check passes (Hetzner first).

### Not here

- **Consolidation awareness** (a hook so that Karpenter treats a held node's remaining period as free): a follow-up.
- **Monthly caps** (no deadline once a cap is reached) and Keeper's `billing_rule` table: when Keeper has them (plan 13).
- Stopping nodes to pause billing: no hourly vendor pauses on stop.

## Per-vendor status

| Vendor | Retention | Why | Where |
|---|---|---|---|
| hcloud | **first vendor**, after its live check (billing start, from-start periods) | hourly, started hour; start `created` | plan 15 |
| UpCloud | after live check | hourly; no stamp, own label; slowest delete; from-start to be verified live | plan 19 |
| Vultr | after live check | hourly; S7 open; `date_created` may be late; from-start to be verified live | plan 20 |
| OVH | off until verified | EU, CA, APAC per second with a 1-minute minimum (never); US hourly per the US terms but per second per the US support guide: conflicting | plan 18 |
| Azure | never | per full minute, no documented minimum (Keeper's 1/300 is not supported by the page) | plan 17 |
| Alibaba | minimum-only candidate, after live check | per second; a short-lived instance pays a minimum of 10, 5 or 2 minutes by vCPU, per the pay-as-you-go page; which cycle it applies to (the instance's life or the last hourly settlement) is unverified: 7, 2 and 0 minutes held at the default margin | plan 21 |
| AWS | never | per second, minimum 60 s: under the hold floor | plan 08 |
| GCP | never | per second, minimum 60 s: under the hold floor | plan 16 |

## Dependencies

- Plan 14 (A4 and the contract, the API work), plan 03 (catalog), plan 04 (the fake driver and clock). The Keeper
  client step is inside this plan. Plan 13 is **not** a dependency.
- The vendor plans turn it on for their vendor; none is needed for the fake-driver gates.

## Decided (maintainer, 2026-11-10)

These were open questions; ADR-024 records them.

1. **Disruption budgets are respected.** A budget that blocks a deadline delete holds the node into the next period (one more period paid).
2. **No fixed `maxRetainedNodes` by default.** A launch blocked by a NodePool limit or a vendor quota releases held empty nodes first, nearest
   deadline first (ties: oldest start); `maxRetainedNodes` stays as an optional safety valve, unset by default.
3. **`consolidateAfter: Never` and a user `do-not-disrupt`**: the node is left completely alone; the NodeClass condition and an Event say why.
4. **Minimum-then-per-second billing** (Alibaba; Azure has no minimum) is optimised too: one rule, the boundaries `start + minimum` and, for increments worth
   holding, `+ n*increment`; `minPeriod` gates per-increment holding, a 2-minute hold floor gates the minimum hold, the 3-minute margin default stays.
5. **Hetzner is the first vendor**, verified in its live lane (billing start, from-start periods).
6. **The period anchor is closed**: no vendor documents clock-hour billing, so there is no `anchor` field and periods count from the server's billing start; from-start is verified live per hourly vendor (Hetzner, Vultr, UpCloud) before retention is enabled for it.
7. **Hold at registration** (settled from core v1.15.1, to confirm in L3): annotations returned by `Create` reach the
   NodeClaim and, at registration, the Node, and Karpenter never disrupts a node before it is Initialized. So the controller's
   hold at registration leaves no unprotected window, and `Create` need not set `do-not-disrupt` itself (it would then lack
   SCarpenter's marker and look like the user's).
8. **Busy-node window.** A busy node is released for `[deadline, boundary)`, not held to the next boundary
   (which would block consolidation for ever); at the last boundary the hold is released for good.
9. **Default `margin`**: 3 minutes covers drain, the termination finalizer, a hcloud delete action and skew (measured in each vendor's lane);
   UpCloud's stop-then-delete may need more, hence the driver's `DeleteLatency`.

## Open questions for the maintainers

1. **Hold floor and margin for short minimums.** The 2-minute floor is a constant and the margin default is 3 minutes for both shapes; the
   first live measurement of a minimum-only vendor (Alibaba) may argue for a smaller margin there (missing the deadline costs
   seconds, not a period) or a configurable floor.
2. **Pending pods SCarpenter cannot see.** Karpenter's nomination of a pending pod to an existing node is in memory only; the
   taint-and-re-read catches bound pods, not a nominated one, which then gets a new node (one launch wasted, never a busy node
   deleted). If that shows up in L3, a short quiet time since the last pod event before a deadline delete would close it.
3. **Margin and the first window.** The hold floor counts from the billing start, but a node is useful only after it registers (a
   minute or more): a 5-minute minimum leaves well under 2 minutes of real reuse. Count the floor from registration instead?
4. **Boundaries after a minimum longer than the increment.** `first + n*I` assumes increments are counted from the end of the minimum;
   if a vendor counts them from the start (`ceil(t/I)*I`), a minimum that is not a multiple of a held increment puts the real boundaries
   elsewhere. No known vendor has such a rule (hourly vendors have `M = I`); reject it as `Unverified` until one does? Alibaba's minimum
   has the same question (per instance life or per hourly settlement, plan 21).
5. **Budget race.** SCarpenter and Karpenter's disruption loop read the same budget independently and can each take the last allowed
   disruption in the same second; accept an overshoot of one, or serialise through a core hook?
6. **Pressure signals.** The exact detection of "blocked by a NodePool limit" (pending pods against the pool's status) is confirmed in L2/L3;
   if Karpenter exposes a better signal, use it.

## Gates

CONVENTIONS gates 1 to 4 (the L3 scenarios of section 6 pass), `hack/check-docs.py` clean, the CRD and API reference
regenerated, the reference docs of section 7 updated in the same change, `docs/plans/v1/README.md` status `review`. Report: the
resolved rule per vendor, the margin components measured on the fake, and any Karpenter behaviour that differed from ADR-024
(the hold at registration, the reuse of a held empty node).
