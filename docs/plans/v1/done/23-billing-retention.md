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
  `Resolve` supports only two shapes: **increments held with `Minimum == Increment`** (or the minimum null or 0), and **minimum-only**
  (`Increment < minPeriod`: one boundary, `start + Minimum`). Any other combination (a minimum different from a held increment,
  such as 90m/60m or 30m/60m) gives `Verified=false`, status reason `UnsupportedShape` and no hold; no new field until a vendor needs one.

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
  and no held increment), `Unverified`, `UnsupportedShape` (a minimum different from a held increment), `NoKeeperData`, `NoBillingStart` (the driver records no stamp for the vendor), and `Active` (its message says `increments` or `minimum only`). The same
  condition counts the nodes left alone, per reason (`ConsolidationNever`, `UserDoNotDisrupt`). `NodeClassValidator`
  can add reasons of its own (a region where the rule differs).
- Changing `spec.billing` is **not** drift (the nodes are the same); turning it off releases every hold at the next reconcile.
- Regenerate the CRD and `docs/reference/api.md` (`make generate manifests`).

### 4. The retention controller (`pkg/controllers/retention`)

Per Node owned by a NodeClaim (watch Nodes, NodeClaims and, through a field index on `spec.nodeName`, Pods; the NodeClass
for its `billing` block and its vendor). Idempotent reconcile with no in-memory state of its own; time comes from `clock.Clock`. The controller is built with the operator's
`state.Cluster` (SCarpenter constructs it, `pkg/operator/operator.go`, and hands it to core), so Karpenter's in-memory mechanisms
(nomination, mark-for-deletion, the budget helpers) are available to it. Karpenter paths below are in `sigs.k8s.io/karpenter@v1.15.1`.

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
(`Increment >= minPeriod`, and `Minimum == Increment` or no minimum: the shape `Resolve` accepts) the first boundary is `start + Increment` and later ones add the increment; without,
the only boundary is `start + Minimum` and `last` is true (a per-second vendor after its minimum). `deadline = boundary - margin`, where `margin` is the effective
one: the larger of `spec.billing.margin` and the rule's `DeleteLatency` plus 90 s; `inMargin` is
`now >= deadline`. The anchor is always `start`: there is no `anchor` field (decided 2026-11-10: no vendor documents clock-hour billing).

Per reconcile of a qualifying node:

1. Before the deadline: put `karpenter.sh/do-not-disrupt: "true"` and `scarpenter.sparecores.com/retain-until=<deadline>`
   on the Node (at registration, not at emptiness: Karpenter at `consolidateAfter: 0s` would otherwise pick a node the moment it
   empties). Requeue at the deadline.
2. At the deadline: if the node is **empty** (no pod except DaemonSet, static and terminal pods: the same notion as Karpenter's
   reschedulable pods) and Initialized (a node not yet Initialized may have been launched for pods that have not bound yet: it is
   treated as busy, and is never deleted by SCarpenter), ask the budget (below). Allowed: run the **release sequence**:
   (a) `cluster.IsNodeNominated(providerID)`: nominated means busy, requeue in 20 s (nomination is a per-node timestamp in cluster state,
   set by every scheduling pass, window `max(2*BatchMaxDuration, 10s)` = 20 s by default; `pkg/controllers/state/statenode.go`,
   `pkg/controllers/provisioning/scheduling/scheduler.go`; Karpenter's own emptiness validator rejects nominated candidates the same way,
   `pkg/controllers/disruption/validation.go`);
   (b) `cluster.MarkForDeletion(providerID)`: the scheduler then drops the node from existing capacity, so no new nomination can occur,
   and Karpenter's candidate filter excludes it; (c) put the `NoSchedule` taint `scarpenter.sparecores.com/releasing`;
   (d) requeue 5 s (Karpenter's `MinDrainTime`, for the same reason: a kube-scheduler cycle that started before the taint may still bind;
   `pkg/controllers/node/termination/controller.go`); (e) re-read the pods from the API server (not the informer cache, which can lag a
   fresh binding) and re-check `IsNodeNominated`: any hit means `UnmarkForDeletion`, remove the taint and take the busy path; otherwise
   delete the NodeClaim (Event `ReleasedAtPeriodEnd`, metric). Residual: a pod that tolerates the taint, or binds in a cycle older than
   5 s; Karpenter's drain evicts and reschedules it (delayed, never lost). The sequence costs about 25 s of the 3-minute margin.
   **Budget**: the NodePool's allowed disruptions for reason `Empty`, computed from the same cluster-state snapshot with Karpenter's exported
   `NodePoolStatsFromNodes` and `NodePoolBudgetMap` (`pkg/controllers/disruption/helpers.go`); nodes marked for deletion count as
   disrupting, and so do nodes carrying `karpenter.sh/disrupted` or the NodeClaim `DisruptionReason` condition (Karpenter patches those in
   `markDisrupted`, `pkg/controllers/disruption/queue.go`, before its own `MarkForDeletion`). `cluster.MarkForDeletion` comes before the
   NodeClaim delete, so Karpenter's next budget build sees the slot taken under the mutex, and `Deleted()` keeps it counted after the delete.
   Residual: Karpenter re-validates a command's budget 15 s after computing it and marks only in `StartCommand`, so a SCarpenter mark inside
   those milliseconds overshoots the budget by one node. Accepted (core has no hook; its queue is private to `NewControllers`), documented,
   and counted by `scarpenter_billing_budget_overshoot_total` when SCarpenter's post-mark recount is negative. **Blocked by
   the budget**: do nothing, Event `ReleaseBlockedByBudget` and the metric, and let the node pass the boundary held (one more period is paid; the
   how-to says so); step 3 gives the next deadline. If it is
   **busy**, remove the hold for the window `[deadline, boundary)` (Karpenter may then replace it) and requeue at the boundary;
   a pod event that leaves it empty inside the window takes the empty path at once.
3. After the boundary: back to step 1 for the next period. At the **last** boundary (`last`, a minimum-then-per-second vendor) there is no
   next period: an empty node is deleted at the deadline as above, a busy one, or one blocked by a budget, gets its hold removed for good and
   becomes an ordinary node (Karpenter's consolidation, under the same budget, decides).
4. **Release under pressure.** The controller keeps a small signal per scope, read from cluster state: a launch blocked by a NodePool limit
   (the `NodePoolState` resources against `spec.limits`, with pending pods and Karpenter's `Nominated` and failed-to-schedule events) or by a
   vendor quota (a refusal from SCarpenter's own quota tracker, reported to the controller by the cloud provider). Confirmed in L3 (g). On a signal it releases held **empty** nodes of that scope, only as many as cover the request, in the order
   **nearest deadline first, ties by oldest start** (the least paid time left is lost; for equal periods it is the oldest-paid-up node), with the same
   budget check, release sequence and Event (`ReleasedUnderPressure`). **A candidate blocked by its budget** is skipped (Event
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
  `scarpenter_billing_boundary_missed_total{vendor}` (an empty node alive 30 s past its boundary: the margin is too small),
  `scarpenter_billing_budget_overshoot_total{vendor}` (SCarpenter's recount after its mark is negative: the budget race of section 4),
  `scarpenter_billing_boot_seconds{vendor}` (histogram of `initialized - billingStart`: whether a short minimum leaves a window).
- NodeClass status as in section 3. Log: vendor, `BillingStart`, period, deadline, decision.

### 6. Tests

- **Pure function**: table tests of `Window` and `Resolve`: exactly on a boundary, inside the margin, many periods in, zero start, null Keeper fields, override over Keeper, per-second vendors with and
  without a minimum, minimum-only rules (600 s with the default margin holds 7 minutes; 300 s holds 2; 120 s is `MinimumTooShort`;
  `last` is true), the hold floor at exactly `minimum - margin = 2m`, unverified; `Resolve` rows: 90m/60m gives `UnsupportedShape`, 60m/60m
  is hourly, 10m/1s is minimum-only, 30m/60m gives `UnsupportedShape`.
- **Controller (L2, envtest)** with `k8s.io/utils/clock/testing.FakeClock` and the fake driver with an impersonated hourly vendor:
  advance the clock across the deadline: annotation on, off, on again after the boundary; a restart in the middle changes
  nothing; no hold for a per-second vendor with no minimum, spot, a static pool, `retainUntilPeriodEnd: false`, a minimum under the hold floor, a
  Drifted claim, `consolidateAfter: Never`, a foreign `do-not-disrupt` (and that one is never removed, no Event spam: once);
  `maxRetainedNodes` unset (no cap) and set (trims in deadline order); an
  empty node is deleted at the deadline with a budget that allows it, and **held into the next period** with a budget of 0 and deleted at
  the next deadline once the budget opens; a limit-blocked and a quota-blocked launch each release the nearest-deadline empty node first,
  and not a node of another pool or quota; a pressure release blocked by a budget drops that node's hold and takes the next candidate;
  an empty node not yet Initialized at the deadline is not deleted; a nominated node (`cluster.NominateNodeForPod`) at the deadline is not
  deleted until the window lapses (fake clock); a budget of 1 with two empty nodes, SCarpenter's deadline and a fake Karpenter mark in the same
  tick: exactly one delete; a minimum-only rule: the node is deleted at `start + minimum - margin` and a busy one loses its hold for good; a busy node rolls over; a pod that lands in the race is evicted
  with the claim and the metrics move.
- **Conformance** (fake and AWS): `BillingStart` rules of section 2. Catalog tests for the vendor table, nulls and a missing table.
- **L3 (kind and KWOK, core's clock is real)**: the fake driver's vendor rule is shortened (period 5 minutes, margin 90 s, a hook
  on the fake): (a) a job every minute-or-two reuses **one** node and no second NodeClaim appears; (a2) (also covers a boot inside the window) a vendor with a 10-minute minimum and per-second
  increments (fake rule shortened to 5 minutes for the lane): a job every 3 minutes reuses one node and the node is gone right after the
  minimum, so the next job gets a fresh one;  (b) a node kept busy across
  the deadline is not deleted and holds again after the boundary; (c) an empty node is gone before the boundary; (d) a per-second
  vendor ignores the setting; (e) `expireAfter` shorter than the period still removes the node, and an `expireAfter` of a multiple of the period minus
  the margin ends it before a boundary; (f) a budget of 0 for `Empty` (the budget race test of L2 runs here too) holds an empty node into the next period and it goes at the next deadline once the budget opens;
  (g) a NodePool at its `limits` with a held empty node: the pending pod gets the capacity after SCarpenter releases it.
  (h) a pending pod created 1 s before the deadline never ends on a deleting node, and `scarpenter_billing_boundary_missed_total` stays 0.
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
   A candidate whose delete a disruption budget forbids loses its hold and the next candidate is tried, so the blocked launch waits
   only as long as it would without retention (confirmed 2026-11-10).
3. **`consolidateAfter: Never` and a user `do-not-disrupt`**: the node is left completely alone; the NodeClass condition and an Event say why.
4. **Minimum-then-per-second billing** (Alibaba; Azure has no minimum) is optimised too: one rule with two shapes, `start + minimum` (minimum only) or
   `start + n*increment` (increments held, a minimum equal to the increment or none; decision 12); `minPeriod` gates per-increment holding, a 2-minute hold floor gates the minimum hold, the 3-minute margin default stays.
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

10. **Nominated pods** (answered 2026-11-10): the retention controller shares the operator's `state.Cluster`, so it sees Karpenter's nomination.
    The deadline delete is the release sequence of section 4 (nominated check, `MarkForDeletion`, taint, 5 s, re-read and re-check, delete or
    undo). Residual: a pod that tolerates the taint or binds in a cycle older than 5 s is evicted and rescheduled, never lost. About 25 s of the margin.
11. **No hold floor from registration**: the minimum is charged whether the node dies at minute 2 or 7, so a useless hold costs only an idle node,
    which release under pressure handles. The floor stays a vendor-level fact counted from `start`; `scarpenter_billing_boot_seconds{vendor}`
    shows whether a 5-minute minimum leaves any window, and the constant is revisited with data.
12. **Minimum different from the increment** is not supported: `Resolve` accepts `Minimum == Increment` (or no minimum) with increments
    held, and minimum-only; anything else is `Verified=false` with reason `UnsupportedShape` and no hold. Alibaba: both readings (per instance
    life, per hourly consumption record) agree inside the first cycle, and SCarpenter deletes only before `start + minimum`, so no new paid
    period is ever started either way; under the per-cycle reading the exposure is at most one minimum extra when the start falls within
    one minimum of a clock hour. Live check in plan 21.
13. **Budget race**: Karpenter's accounting is reused (section 4); the one-node overshoot against Karpenter's 15 s re-validation is accepted,
    documented and counted by `scarpenter_billing_budget_overshoot_total`.
14. **Hold floor and margin for short minimums**: keep both constants; the boot-time histogram decides.
15. **Pressure signals** come from cluster state (`NodePoolState` against `spec.limits`, pending pods, Karpenter's events) and the quota
    tracker; confirmed in L3 (g).

## Open questions for the maintainers

None: all were answered on 2026-11-10 (decisions 10 to 15 above).

## Gates

CONVENTIONS gates 1 to 4 (the L3 scenarios of section 6 pass), `hack/check-docs.py` clean, the CRD and API reference
regenerated, the reference docs of section 7 updated in the same change, `docs/plans/v1/README.md` status `review`. Report: the
resolved rule per vendor, the margin components measured on the fake, and any Karpenter behaviour that differed from ADR-024
(the hold at registration, the reuse of a held empty node).

## Outcome

Landed on main in the commits d9bf98a..0d3cc88: the Keeper vendor billing facts,
`BillingStart` and `BillingDescriber` in the driver contract, the rule resolution
and period window arithmetic, `spec.billing` and `status.billing`, the retention
controller with its chart flag and RBAC, and the L3 scenarios.

- A release under pressure that the NodePool's disruption budget blocks sets the
  `scarpenter.sparecores.com/hold-released-until` annotation; the node is an ordinary
  empty node until the end of its period.
- The shape decision (a minimum over a shorter increment is minimum-only) moved to
  Assess, because it depends on `minPeriod`.
- Known residual: approximate pressure signals can release held nodes early. That
  costs savings; it does not break any invariant.
