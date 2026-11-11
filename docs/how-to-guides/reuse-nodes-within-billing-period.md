# Reuse nodes within the billing period

Some vendors bill a server by the started period, usually the hour (hcloud, UpCloud, Vultr;
perhaps OVH's US regions), and one bills a short minimum and then per second (Alibaba, probably, a few minutes).
A job that runs one minute every ten minutes and gets a new node each
time pays for six node-hours per hour. With this setting SCarpenter keeps such a node until the
period (or the minimum) it has already paid for is about to end, lets Karpenter put the next job on it, and
deletes it before the next charge starts.

Status: designed in [ADR-024](../explanation/DECISIONS.md#adr-024-billing-period-aware-node-retention-accepted-2026-11-10),
built by [plan 23](../plans/v1/done/23-billing-retention.md) (not released yet), tested with the fake driver impersonating a vendor with a short
period. **No real vendor has it switched on yet:** each vendor is enabled only after its billing rule has been checked on a live account (its
driver then reports the rule as verified); Hetzner is the first. Until then the NodeClass status says `Unverified` for the vendor and nothing is held.
Periods always count from the server's start (no vendor documents clock-hour billing); each hourly vendor's live check confirms it
before the feature is enabled for it.

## Turn it on

On the `SCNodeClass`, off by default:

```yaml
spec:
  billing:
    retainUntilPeriodEnd: true
    margin: 3m          # stop this long before the period ends (default 3m, at least 90s)
    minPeriod: 10m      # billing increments at least this long are held to their end (default 10m)
    # maxRetainedNodes: 20  # optional safety valve, unset by default
```

The setting is on the NodeClass because the vendor and the region, which decide whether a billing
period exists, are known there. A NodeClass for several vendors needs no per-vendor setting: each
vendor's own rule decides whether it has any effect.

## What happens

- The length of the period and when it started come from data, not from a built-in hour: Keeper's vendor
  table (and a driver's finer rule where a vendor differs by region or family) and the vendor's own creation time
  of the server. Any period works, not only hours.
- While more than `margin` of the current period is left, the node is protected from Karpenter's voluntary
  disruption (consolidation) by `karpenter.sh/do-not-disrupt`, set by SCarpenter. Karpenter puts pending pods on
  existing nodes first, so the next job lands on the idle node.
- `margin` before the period ends: an empty node is deleted by SCarpenter, if the NodePool's disruption budget allows
  it. If a budget blocks the delete (a budget of 0 for `Empty` during a maintenance window, say), the node is held into
  the next period, so **one more period is paid**; SCarpenter tries again at the next deadline. A node that is busy then is never
  deleted by SCarpenter: it is released from the protection until the period turns over, so that Karpenter may replace
  it at a moment that costs nothing (or SCarpenter deletes it if it empties before then), and is protected again for the
  next period.
- On a vendor that bills a **minimum and then per second**, there is one deadline, `margin` before the end of the minimum. An
  empty node is held and reused until then and then deleted at once, because after the minimum every second costs. A busy node
  is simply released to Karpenter. Holding is worth it only if the minimum minus the margin is at least 2 minutes: with the default
  3-minute margin a 10-minute minimum holds a node for 7 minutes, a 5-minute minimum for 2, and a 2-minute minimum is never held.
  For example, a job every 3 minutes on a 10-minute-minimum vendor uses one node for the jobs at 0, 3 and 6 minutes, and the node is
  gone at about minute 7.
- Before the delete SCarpenter checks that Karpenter has not nominated a pod to the node, marks the node for deletion in
  Karpenter's cluster state (so the scheduler stops using it), puts the taint `scarpenter.sparecores.com/releasing`, waits 5 seconds, reads
  the pods again from the API server and checks the nomination again: a pod, or a nomination, undoes the mark and the taint and the node is busy
  after all. About 25 seconds of the margin go on this at the deadline. A pod that tolerates the taint, or that binds in a scheduling
  cycle older than 5 seconds, is evicted and rescheduled by the drain: delayed, never lost.
- `margin` has to cover the drain and the vendor's delete; missing the boundary costs a whole extra period, so the
  default errs early.

`kubectl describe scnodeclass` shows, per vendor, whether retention is active and why not (`status.billing`: `Disabled`, `PerSecondBilling`, `MinimumTooShort`,
`Unverified`, `UnsupportedShape`, `NoKeeperData`, `NoBillingStart`, and `PeriodTooShort` or `InvalidSettings` when the margin and `minPeriod` leave no room), and the condition
`BillingRetention` counts the nodes it leaves alone (`kubectl get scnodeclass -o yaml` has the details). On a node,
`kubectl describe node` shows the Events: `RetainedUntil`, `ReleasedAtPeriodEnd`, `ReleaseBlockedByBudget`, `ReleasedUnderPressure`, and
`RetentionSkipped` with the reason when a node is left alone. A node with `scarpenter.sparecores.com/retain-until` is held, and the value is its deadline.
The metrics (`scarpenter_billing_*`) are in the [controller reference](../reference/controller.md#billing-retention): `reuse_total` is how often a held node took a job,
`boundary_missed_total` should stay at 0 (if it does not, raise `margin`).

## When it helps

A job (or a burst) of the same shape that returns within the period: a CI runner, a cron job every few
minutes. It saves nothing on per-second or per-minute vendors with no real minimum (AWS, GCP, Azure), so it does nothing there.

## Limits

- A pod that does not fit the held node (a different size, taints, requirements) gets a new node: the
  saving is for repeated work of the same shape.
- Held nodes count against NodePool `limits` and vendor quotas, but retention never makes a launch wait longer than it would without
  it: when a pending pod that Karpenter has found no capacity for is blocked by a NodePool limit or by a vendor quota, SCarpenter releases held empty nodes
  (the signals are read every 10 seconds), the one nearest its deadline first (it has the least paid time left), as many as cover the request. If a disruption budget blocks that release, the node loses its protection and Karpenter deletes it
  when the budget allows, as it would have without retention. There is
  no cap by default; `maxRetainedNodes` is an optional safety valve.
- Stopping a node does not pause billing on any of these vendors, so a stopped node is still deleted (it
  cannot take pods); the deletion saves the following periods, not the one already started.
- SCarpenter's deletes respect NodePool disruption budgets (see above: a blocked delete costs one more period).
  A NodePool with `consolidateAfter: Never`, or a node with a `karpenter.sh/do-not-disrupt` annotation of your own, is left
  completely alone: no hold and no delete. The NodeClass status and a Node event say why.
- Only two rule shapes are supported: a minimum equal to the increment (hourly) or a minimum with per-second billing after it. A vendor
  whose minimum differs from a held increment shows the reason `UnsupportedShape` and its nodes are not held.
- The budget check can be raced by one node: if Karpenter starts a disruption at the same moment, the NodePool budget may be exceeded
  by one node once (the `scarpenter_billing_budget_overshoot_total` metric counts it).
- A drifted node is not held. `expireAfter`, `terminationGracePeriod`, interruption and node repair ignore the
  protection. `expireAfter` counts from the node's creation, so set it to a multiple of the period minus the margin
  (`23h57m` for hourly billing and the default 3m), or the last period is partly wasted or a new one is started.
- The first release does not tell Karpenter that a held node's remaining period is free, so Karpenter may still
  replace a busy node with a smaller one inside a period (a follow-up).
- Monthly price caps (Hetzner, Vultr, UpCloud) are not used yet: once a cap is reached more hours are free,
  and the node is deleted before the boundary anyway.

See also [node settings](../reference/node-settings.md) and the vendor plans' billing sections
([hcloud](../plans/v1/wip/15-hcloud.md), [OVH](../plans/v1/wip/18-ovh.md), [UpCloud](../plans/v1/wip/19-upcloud.md),
[Vultr](../plans/v1/wip/20-vultr.md)).
