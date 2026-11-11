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

- [**Reuse nodes within the billing period**](reuse-nodes-within-billing-period.md)
  (plan 23, ADR-024) — keep hourly-billed nodes until the period they paid for is
  about to end, and when that helps and where it stops.
- [**Install SCarpenter**](install.md) (plan 09) — prerequisites (bootstrap-token
  auth, CNI and kube-proxy that work on remote nodes), the Helm install, the values
  that matter, the k3s kubelet client CA, upgrade, uninstall and what it leaves
  behind.
- [**Set up AWS credentials**](aws-credentials.md) (plan 09) — the IAM policy, web
  identity federation from the cluster's ServiceAccount issuer, EKS Pod Identity,
  static keys, the account's Spot service-linked role.
- [**Set up Alibaba Cloud credentials**](alicloud-credentials.md) (plan 21) — the RAM policy, an OIDC role or an access
  key, wired into the chart.
- [**Flatcar snapshots on Hetzner Cloud**](hcloud-images.md) (plan 15) — make the
  snapshots the `hcloud` driver boots, the label convention, and how the driver finds
  and checks them.

- [**Set up Vultr credentials**](vultr-credentials.md) (plan 20) — the API key and
  its IP allowlist, the Secret and the chart values, and the snapshot of the pinned
  Flatcar version each account creates itself.

- [**Set up OVHcloud credentials**](ovh-credentials.md) (plan 18) — a dedicated
  OpenStack user of the Public Cloud project with the five operator roles, the
  Secret and the chart values, and the IAM policy of the later service-account mode.
- [**Upload the Flatcar image to OVH**](ovh-images.md) (plan 18) — one Glance image per
  region from Flatcar's OpenStack release, the naming convention, the properties and the
  tags the driver reads.

Planned for v1:

- **Select instances** (plan 11) — exact choices, attribute ranges, benchmark
  thresholds and percentiles, selection profiles and objectives.
- **Add a vendor driver** (plan 11) — the driver contract, the conformance
  test suite, what to put in the catalog mapping.
