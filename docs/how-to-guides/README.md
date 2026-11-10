# How-to guides

Goal-oriented procedures.

Available:

- [**Test and develop**](testing.md) (plan 02, extended by every plan) — dev
  container, gates, the L1–L5 test lanes, generated files, CI.
- [**Testing and releasing**](testing-and-releasing.md) — when the real-cloud
  tests run and what they cost, the PR tiers and path rules, approval, the
  nightly canary, the release process and its gate, adding a vendor lane.
- [**Choose a node network template**](choose-a-network-template.md) (plan 05) —
  `tailscale`, `direct`, or a custom Butane template.

- [**Write node profiles**](write-profiles.md) (plan 06) — storage, network and
  placement profiles in each vendor's own terms; what a workload gets without
  one.
- [**Bring your own vendor resources**](bring-your-own-resources.md) (plan 06) —
  reference pre-created networks, subnets, security groups or placement groups by
  ID, name pattern or tags per region; or let SCarpenter create them, and choose
  their lifecycle.

- [**Install SCarpenter**](install.md) (plan 09) — prerequisites (bootstrap-token
  auth, CNI and kube-proxy that work on remote nodes), the Helm install, the values
  that matter, the k3s kubelet client CA, upgrade, uninstall and what it leaves
  behind.
- [**Set up AWS credentials**](aws-credentials.md) (plan 09) — the IAM policy, web
  identity federation from the cluster's ServiceAccount issuer, EKS Pod Identity,
  static keys, the account's Spot service-linked role.

Planned for v1:

- **Select instances** (plan 11) — exact choices, attribute ranges, benchmark
  thresholds and percentiles, selection profiles and objectives.
- **Add a vendor driver** (plan 11) — the driver contract, the conformance
  test suite, what to put in the catalog mapping.
