# How-to guides

Goal-oriented procedures.

Available:

- [**Test and develop**](testing.md) (plan 02, extended by every plan) — dev
  container, gates, the L1–L5 test lanes, generated files, CI.
- [**Choose a node network template**](choose-a-network-template.md) (plan 05) —
  `tailscale`, `direct`, or a custom Butane template.

- [**Write node profiles**](write-profiles.md) (plan 06) — storage, network and
  placement profiles in each vendor's own terms; what a workload gets without
  one.
- [**Bring your own vendor resources**](bring-your-own-resources.md) (plan 06) —
  reference pre-created networks, subnets, security groups or placement groups by
  ID, name pattern or tags per region; or let SCarpenter create them, and choose
  their lifecycle.

Planned for v1:

- **Install SCarpenter** (plan 09) — prerequisites (bootstrap-token auth, CNI
  and kube-proxy that work on remote nodes), Helm values, API server endpoint
  and CA, catalog URL and API key, upgrade, uninstall.
- **Set up AWS credentials** (plan 09) — web identity federation from the
  cluster's ServiceAccount issuer, or static keys; the IAM policy.
- **Select instances** (plan 11) — exact choices, attribute ranges, benchmark
  thresholds and percentiles, selection profiles and objectives.
- **Add a vendor driver** (plan 11) — the driver contract, the conformance
  test suite, what to put in the catalog mapping.
