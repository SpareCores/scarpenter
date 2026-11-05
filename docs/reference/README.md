# Reference

Technical descriptions of how specific parts of SCarpenter work.

Planned for v1 (each written by the plan that implements it):

- **`SCNodeClass` API** — every field, generated from the CRD (plans 02, 05).
- **Labels and offerings** — instance-type naming, attribute, metric,
  selection, location and profile labels, how Keeper rows become Karpenter
  instance types and offerings (plan 03).
- **Node settings** — built-in defaults, vendor overrides, profiles, how a
  node's settings are resolved, drift (plan 06).
- **Vendor resources** — resource kinds, `use` / `create` / `forbid`,
  lifecycles, timing, status, garbage collection (plan 06).
- **Driver contract** — the `Driver` interface, optional capability
  interfaces (quotas, launchable types), error classes, the conformance
  suite (plan 04).
- **Quota-aware placement** — buckets, headroom, the in-flight ledger,
  `QuotaAvailable` conditions and what a pinned region does (plan 04).
- **Node bootstrap** — the rendered Ignition, default templates, template
  variables, size budget, RBAC for the join (plan 05).
- **AWS driver** — regions, zones, AMIs, subnets and security groups, tags,
  quota classes and codes, IAM policy, error mapping (plan 08).
- **Controller** — flags and environment variables, Helm values, status
  conditions, Events, metrics, permissions (plan 09).
