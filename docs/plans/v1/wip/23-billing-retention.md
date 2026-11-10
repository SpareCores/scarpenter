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
(hour on hcloud, UpCloud, Vultr) until the end of its paid period, so Karpenter reuses it for the next job, and delete
it before the next period opens. A job that runs a minute every ten minutes then pays one hour per hour, not six.
Off by default; no effect on per-second vendors, spot, static pools or unverified vendors.

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
  Rule{Period, Minimum, StoppedCharged, DeleteLatency, Source, Verified}`. The driver's override wins field by field
  and is never lower than what the vendor row says without a comment in the code that links the source. A rule is
  **qualifying** when `Verified`, `Period >= spec.billing.minPeriod` and the capacity is on-demand.

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
    minPeriod: 10m                # default 10m
    maxRetainedNodes: 0           # default 0 = no cap
```

- CEL: `margin >= 90s` (`duration(self.margin) >= duration('90s')`), `minPeriod >= 1m`, `maxRetainedNodes >= 0`; the block
  is optional and every field defaulted. Cross-field and vendor rules (`minPeriod` below twice the margin, a period shorter than twice the
  margin on a vendor) are conditions, not CEL: `BillingRetention` on the NodeClass.
- Status: `status.billing` lists, per vendor in the NodeClass, `{vendor, active, reason, period, source}` with reason one of
  `Disabled`, `PerSecondBilling`, `BelowMinPeriod`, `Unverified`, `NoKeeperData`, and `Active`. `NodeClassValidator`
  can add reasons of its own (a region where the rule differs).
- Changing `spec.billing` is **not** drift (the nodes are the same); turning it off releases every hold at the next reconcile.
- Regenerate the CRD and `docs/reference/api.md` (`make generate manifests`).

### 4. The retention controller (`pkg/controllers/retention`)

Per Node owned by a NodeClaim (watch Nodes, NodeClaims and, through a field index on `spec.nodeName`, Pods; the NodeClass
for its `billing` block and its vendor). Idempotent reconcile with no in-memory state; time comes from `clock.Clock`.

Qualification (anything else: remove an annotation SCarpenter set, and do nothing): `retainUntilPeriodEnd`; the vendor's rule
qualifies (section 1); on-demand; the NodePool is not static and not `consolidateAfter: Never`; the Node is registered (Initialized is not needed:
Karpenter never disrupts a node that is not, so the hold is on before it can be a candidate); the NodeClaim is
not `Drifted` and not being deleted; the node has no `do-not-disrupt` that SCarpenter did not set; `BillingStart` is known
(else an Event `BillingStartUnknown` once, never a guess) and not in the future beyond a small skew allowance;
the node is not over `maxRetainedNodes` (oldest first).

The arithmetic is one pure function:
`Window(start, rule, margin, now) (boundary, deadline time.Time, inMargin bool)`. The first boundary is
`start + max(minimum, period)`, later ones add the period; `deadline = boundary - margin`, where `margin` is the effective
one: the larger of `spec.billing.margin` and the rule's `DeleteLatency` plus 90 s; `inMargin` is
`now >= deadline`.

Per reconcile of a qualifying node:

1. Before the deadline: put `karpenter.sh/do-not-disrupt: "true"` and `scarpenter.sparecores.com/retain-until=<deadline>`
   on the Node (at registration, not at emptiness: Karpenter at `consolidateAfter: 0s` would otherwise pick a node the moment it
   empties). Requeue at the deadline.
2. At the deadline: if the node is **empty** (no pod except DaemonSet, static and terminal pods: the same notion as Karpenter's
   reschedulable pods), put the `NoSchedule` taint `scarpenter.sparecores.com/releasing` on the Node, re-read the pods, and
   delete the NodeClaim (Event `ReleasedAtPeriodEnd`, metric); a pod found on the re-read makes it busy (taint removed). If it is
   **busy**, remove the hold for the window `[deadline, boundary)` (Karpenter may then replace it) and requeue at the boundary;
   a pod event that leaves it empty inside the window takes the empty path at once.
3. After the boundary: back to step 1 for the next period.

Rules the controller keeps: only annotations it set are removed (marker annotation present); no hold and no delete when the
NodePool says `consolidateAfter: Never`; a Drifted NodeClaim loses its hold; a `terminating` or `stopped` instance is
never held (plan 14, A4, applies); its deletes ignore the NodePool's disruption budgets (ADR-024) and respect a foreign
`do-not-disrupt`. RBAC: `nodes` get, list, watch, patch; `nodeclaims` get, list, watch, delete; `pods` get, list, watch;
Helm chart and the permissions section of the controller reference change with it. A flag `--billing-retention` (default
on; the NodeClass field is the real switch) turns the controller off as a whole.

On start the controller logs, once per vendor, the rule it resolved (period, source, verified) so that an operator can see it.

### 5. Observability

- Events on the Node: `RetainedUntil`, `ReleasedAtPeriodEnd`, `BillingStartUnknown` (Warning).
- Metrics (all in the controller reference): `scarpenter_billing_retained_nodes{vendor,nodeclass}` (gauge),
  `scarpenter_billing_retained_seconds_total{vendor}`, `scarpenter_billing_reuse_total{vendor}` (a node that was empty
  under the hold receives a pod), `scarpenter_billing_periods_saved_estimate_total{vendor}` (equals the reuses: an estimate,
  not an invoice), `scarpenter_billing_deletes_at_deadline_total{vendor}`,
  `scarpenter_billing_boundary_missed_total{vendor}` (an empty node alive 30 s past its boundary: the margin is too small).
- NodeClass status as in section 3. Log: vendor, `BillingStart`, period, deadline, decision.

### 6. Tests

- **Pure function**: table tests of `Window` and `Resolve`: exactly on a boundary, inside the margin, many periods in, a first
  boundary from a minimum above the period, zero start, null Keeper fields, override over Keeper, per-second vendors, unverified.
- **Controller (L2, envtest)** with `k8s.io/utils/clock/testing.FakeClock` and the fake driver with an impersonated hourly vendor:
  advance the clock across the deadline: annotation on, off, on again after the boundary; a restart in the middle changes
  nothing; no hold for a per-second vendor, spot, a static pool, `retainUntilPeriodEnd: false`, a period below `minPeriod`, a
  Drifted claim, `consolidateAfter: Never`, a foreign `do-not-disrupt` (and that one is never removed); `maxRetainedNodes`; an
  empty node is deleted at the deadline with the budget set to 0; a busy node rolls over; a pod that lands in the race is evicted
  with the claim and the metrics move.
- **Conformance** (fake and AWS): `BillingStart` rules of section 2. Catalog tests for the vendor table, nulls and a missing table.
- **L3 (kind and KWOK, core's clock is real)**: the fake driver's vendor rule is shortened (period 5 minutes, margin 90 s, a hook
  on the fake): (a) a job every minute-or-two reuses **one** node and no second NodeClaim appears; (b) a node kept busy across
  the deadline is not deleted and holds again after the boundary; (c) an empty node is gone before the boundary; (d) a per-second
  vendor ignores the setting; (e) `expireAfter` shorter than the period still removes the node, and an `expireAfter` of a multiple of the period minus
  the margin ends it before a boundary; (f) a budget of 0 for `Empty` does not
  keep an empty node past its deadline.
- **Per-vendor L5 lanes** (plans 15, 18, 19, 20): assert delete latency and instance state only; billing cannot be asserted
  from the API (the invoice is the check, the plans list what to read).

### 7. Docs

[Reuse nodes within the billing period](../../../how-to-guides/reuse-nodes-within-billing-period.md) (written with
this plan's design; update it with what is verified), the `SCNodeClass` API reference (generated), the controller reference
(flag, RBAC, metrics, Events, annotation, status), the driver contract reference (`BillingStart`, `BillingDescriber`), the
node-settings reference link, the labels reference (the annotations `scarpenter.sparecores.com/retain-until` and `billing-start`, the taint
`scarpenter.sparecores.com/releasing`), each vendor's reference gets its
verified rule when its plan's live check passes.

### Not here

- **Consolidation awareness** (a hook so that Karpenter treats a held node's remaining period as free): a follow-up.
- **Monthly caps** (no deadline once a cap is reached) and Keeper's `billing_rule` table: when Keeper has them (plan 13).
- Stopping nodes to pause billing: no hourly vendor pauses on stop.

## Per-vendor status

| Vendor | Retention | Why | Where |
|---|---|---|---|
| hcloud | after live check | hourly, started hour; start `created` | plan 15 |
| UpCloud | after live check | hourly; no stamp, own label; slowest delete | plan 19 |
| Vultr | after live check | hourly; S7 open; `date_created` may be late | plan 20 |
| OVH | after live check, per region | Keeper says hourly, the billing page says per second | plan 18 |
| AWS | never | per second (1/60) | plan 08 |
| GCP | never | per second (1/60) | plan 16 |
| Azure | never | per minute (1/300 in Keeper, a data issue) | plan 17 |
| Alibaba | never | per second; the minimum is for the release only | plan 21 |

## Dependencies

- Plan 14 (A4 and the contract, the API work), plan 03 (catalog), plan 04 (the fake driver and clock). The Keeper
  client step is inside this plan. Plan 13 is **not** a dependency.
- The vendor plans turn it on for their vendor; none is needed for the fake-driver gates.

## Open questions for the maintainers

1. **`maxRetainedNodes`.** Added to the first sketch because held nodes count against NodePool limits and the
   quota tracker; default no cap. Drop it if a NodePool `limits` entry is felt to be enough.
2. **Ignoring budgets.** SCarpenter's deletes bypass NodePool disruption budgets (ADR-024). If an operator's
   budget is a hard compliance window, the alternative is to respect it and accept a lost period; a flag can
   choose it. Recommended: bypass, they only remove empty nodes.
3. **Busy-node window.** A busy node is released for `[deadline, boundary)`, not held to the next boundary
   (which would block consolidation for ever). Underutilised replacement then happens near a boundary only.
4. **Default `margin`.** 3 minutes covers drain, the termination finalizer, a hcloud delete action and skew
   (measured in each vendor's lane); UpCloud's stop-then-delete may need more, hence the driver's `DeleteLatency`.
5. **Retention and `consolidateAfter`.** With `Never` the controller does nothing, which respects the pool.
   Alternative: still delete at the deadline. Recommended: respect the pool.
6. **Where the vendor facts are unverified**, retention is off by driver: a nuisance for the first release (nothing works until
   a live check), the price of ADR-017. Which vendor is first (hcloud, the best documented) is the maintainers' call.
7. **Hold at registration** (settled from core v1.15.1, to confirm in L3): annotations returned by `Create` reach the
   NodeClaim and, at registration, the Node, and Karpenter never disrupts a node before it is Initialized. So the controller's
   hold at registration leaves no unprotected window, and `Create` need not set `do-not-disrupt` itself (it would then lack
   SCarpenter's marker and look like the user's).

## Gates

CONVENTIONS gates 1 to 4 (the L3 scenarios of section 6 pass), `hack/check-docs.py` clean, the CRD and API reference
regenerated, the reference docs of section 7 updated in the same change, `docs/plans/v1/README.md` status `review`. Report: the
resolved rule per vendor, the margin components measured on the fake, and any Karpenter behaviour that differed from ADR-024
(the hold at registration, the reuse of a held empty node).
