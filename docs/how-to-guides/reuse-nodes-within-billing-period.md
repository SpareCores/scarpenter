# Reuse nodes within the billing period

Some vendors bill a server by the started period, usually the hour (hcloud, UpCloud, Vultr;
OVH in some regions). A job that runs one minute every ten minutes and gets a new node each
time pays for six node-hours per hour. With this setting SCarpenter keeps such a node until the
period it has already paid for is about to end, lets Karpenter put the next job on it, and
deletes it before the next period starts.

Status: designed in [ADR-024](../explanation/DECISIONS.md#adr-024-billing-period-aware-node-retention-accepted-2026-11-10),
built by [plan 23](../plans/v1/wip/23-billing-retention.md); not released yet. Each vendor is switched on only after
its billing rule has been checked on a live account.

## Turn it on

On the `SCNodeClass`, off by default:

```yaml
spec:
  billing:
    retainUntilPeriodEnd: true
    margin: 3m          # stop this long before the period ends (default 3m, at least 90s)
    minPeriod: 10m      # only periods at least this long qualify (default 10m)
    maxRetainedNodes: 0 # 0 = no cap
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
- `margin` before the period ends: an empty node is deleted by SCarpenter. A node that is busy then is never
  deleted by SCarpenter: it is released from the protection until the period turns over, so that Karpenter may replace
  it at a moment that costs nothing (or SCarpenter deletes it if it empties before then), and is protected again for the
  next period.
- `margin` has to cover the drain and the vendor's delete; missing the boundary costs a whole extra period, so the
  default errs early.

`kubectl describe scnodeclass` shows, per vendor, whether retention is active and why not (per-second billing,
a period below `minPeriod`, a vendor rule not yet verified, no billing data).

## When it helps

A job (or a burst) of the same shape that returns within the period: a CI runner, a cron job every few
minutes. It saves nothing on per-second vendors (AWS, GCP, Azure, Alibaba), so it does nothing there.

## Limits

- A pod that does not fit the held node (a different size, taints, requirements) gets a new node: the
  saving is for repeated work of the same shape. Held nodes count against NodePool `limits`; `maxRetainedNodes` caps them.
- Stopping a node does not pause billing on any of these vendors, so a stopped node is still deleted (it
  cannot take pods); the deletion saves the following periods, not the one already started.
- SCarpenter's deletes at the end of a period ignore NodePool disruption budgets: they only remove empty nodes.
  `consolidateAfter: Never` on the NodePool switches retention off for its nodes, as does a `do-not-disrupt` annotation of
  your own.
- A drifted node is not held. `expireAfter`, `terminationGracePeriod`, interruption and node repair ignore the
  protection. `expireAfter` counts from the node's creation, so set it to a multiple of the period minus the margin
  (`23h57m` for hourly billing and the default 3m), or the last period is partly wasted or a new one is started.
- The first release does not tell Karpenter that a held node's remaining period is free, so Karpenter may still
  replace a busy node with a smaller one inside a period (a follow-up).
- Monthly price caps (Hetzner, Vultr, UpCloud) are not used yet: once a cap is reached more hours are free,
  and the node is deleted at the boundary anyway.

See also [node settings](../reference/node-settings.md) and the vendor plans' billing sections
([hcloud](../plans/v1/wip/15-hcloud.md), [OVH](../plans/v1/wip/18-ovh.md), [UpCloud](../plans/v1/wip/19-upcloud.md),
[Vultr](../plans/v1/wip/20-vultr.md)).
