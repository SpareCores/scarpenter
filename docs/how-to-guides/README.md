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
- [**Handle spot interruptions**](handle-spot-interruptions.md) (plan 26, ADR-027) —
  what a workload sees when a vendor reclaims a spot or preemptible node, how to count
  interruptions per instance type, and `terminationGracePeriodSeconds` against each vendor's notice.
- [**Which types does a class offer**](which-types-does-a-class-offer.md) (plan 28) — read the
  offered instance types and why the others are not from the status, an opt-in ConfigMap listing
  and the logs; `jq` recipes and the ready-made pin.
- [**Run GPU workloads**](run-gpu-workloads.md) (plan 12) — the RuntimeClass and device
  plugin the chart ships, how to turn them off, taints and the Pod spec of a GPU workload.
- [**Install SCarpenter**](install.md) (plan 09) — prerequisites (bootstrap-token
  auth, CNI and kube-proxy that work on remote nodes), the Helm install, the values
  that matter, the k3s kubelet client CA, upgrade, uninstall and what it leaves
  behind.

Providers: each guide takes an account to instances joining your cluster (the
Flatcar image, credentials, the chart values, a first NodeClass and NodePool, checks
and limits). Their status is in the [README](../../README.md#cloud-providers).

- [**Set up AWS**](set-up-aws.md)
- [**Set up Oracle Cloud Infrastructure (OCI)**](set-up-oci.md)
- [**Set up OVHcloud Public Cloud**](set-up-ovh.md)
- [**Set up Google Cloud**](set-up-gcp.md)
- [**Set up Microsoft Azure**](set-up-azure.md)
- [**Set up Alibaba Cloud**](set-up-alicloud.md)
- [**Set up Hetzner Cloud**](set-up-hcloud.md)
- [**Set up UpCloud**](set-up-upcloud.md)
- [**Set up Vultr**](set-up-vultr.md)

Planned for v1:

- **Select instances** (plan 11) — exact choices, attribute ranges, benchmark
  thresholds and percentiles, selection profiles and objectives.
- **Add a vendor driver** (plan 11) — the driver contract, the conformance
  test suite, what to put in the catalog mapping.
